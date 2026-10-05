package twin

import (
	"os"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

// What a terminal with a 0x123456 background sends in response to "\x1b]11;?"
const backgroundResponse = "\x1b]11;rgb:1212/3434/5656\x07"

// What a terminal with the cursor at row 12, column 40 sends in response to the
// "\x1b[6n" cursor position query
const cursorPositionResponse = "\x1b[12;40R"

// What a terminal with Alternate Scroll Mode reset (supported but currently
// off) sends in response to the "\x1b[?1007$p" DECRQM query
const alternateScrollOffResponse = "\x1b[?1007;2$y"

// A screen with mainLoop() running, reading from a pipe instead of from a
// terminal.
//
// Write to the returned terminal to fake terminal input. Read what the screen
// wrote to the terminal using the returned output function.
func newPipeTestScreen(t *testing.T) (screen *terminalScreen, terminal *os.File, output func() string) {
	t.Helper()
	ttyIn, terminal, err := os.Pipe()
	assert.NilError(t, err)

	ttyOut, err := os.CreateTemp(t.TempDir(), "ttyout")
	assert.NilError(t, err)

	screen = &terminalScreen{
		ttyIn:  ttyIn,
		ttyOut: ttyOut,
		events: make(chan Event, 160),
	}
	screen.ttyInReader = newInterruptableReader(ttyIn)

	go screen.mainLoop()

	t.Cleanup(func() {
		screen.ttyInReader.Interrupt()
		_ = terminal.Close()
		_ = ttyOut.Close()
	})

	output = func() string {
		t.Helper()
		bytes, err := os.ReadFile(ttyOut.Name())
		assert.NilError(t, err)
		return string(bytes)
	}

	return screen, terminal, output
}

func writeTerminal(t *testing.T, terminal *os.File, s string) {
	t.Helper()
	_, err := terminal.WriteString(s)
	assert.NilError(t, err)
}

// Assert that the next event is a 'q' keypress, with nothing before it
func assertNextEventIsQ(t *testing.T, screen *terminalScreen) {
	t.Helper()
	select {
	case event := <-screen.events:
		assert.Equal(t, event, Event(EventRune{Rune: 'q'}))
	case <-time.After(time.Second):
		t.Fatal("Timed out waiting for the 'q' keypress")
	}
}

// The cursor position query goes last. Its response is what tells us we can
// stop waiting for responses to the other queries.
func TestTerminalBackgroundQueryAsksForCursorPosition(t *testing.T) {
	screen, terminal, output := newPipeTestScreen(t)
	writeTerminal(t, terminal, cursorPositionResponse)

	screen.queryTerminalBackground()

	assert.Assert(t, strings.Contains(output(), "\x1b]11;?\x07\x1b[?1007$p\x1b[6n"), humanizeLowASCII(output()))
}

// Slow links can take a while to respond. That's fine, as long as they respond.
func TestTerminalBackgroundSlowResponse(t *testing.T) {
	screen, terminal, _ := newPipeTestScreen(t)
	written := make(chan struct{})
	go func() {
		defer close(written)
		time.Sleep(100 * time.Millisecond)
		_, err := terminal.WriteString(backgroundResponse + cursorPositionResponse)
		assert.Check(t, err)
	}()

	// Cleanups run last-registered-first, so this runs before the pipe closes
	t.Cleanup(func() { <-written })

	start := time.Now()
	screen.queryTerminalBackground()

	// Well below the 500ms backstop, so we know it was the cursor position
	// response that ended the wait
	assert.Assert(t, time.Since(start) < 400*time.Millisecond, "Waited for %s", time.Since(start))

	background := screen.TerminalBackground()
	assert.Assert(t, background != nil)
	assert.Equal(t, *background, NewColorHex(0x123456))
}

// Both responses should be consumed, without showing up as events.
//
// To verify that, we send a 'q' after the responses, and check that it's the
// first event we get. Any events caused by the responses would have shown up
// before it.
func TestTerminalBackgroundResponsesThenKey(t *testing.T) {
	screen, terminal, _ := newPipeTestScreen(t)
	writeTerminal(t, terminal, backgroundResponse+cursorPositionResponse)

	screen.queryTerminalBackground()

	background := screen.TerminalBackground()
	assert.Assert(t, background != nil)
	assert.Equal(t, *background, NewColorHex(0x123456))

	writeTerminal(t, terminal, "q")
	assertNextEventIsQ(t, screen)
}

// A terminal that doesn't support background color queries. The cursor
// position response should make us give up right away, rather than waiting for
// a background color that's never coming.
func TestTerminalBackgroundNoBackgroundResponse(t *testing.T) {
	screen, terminal, _ := newPipeTestScreen(t)
	writeTerminal(t, terminal, cursorPositionResponse)

	start := time.Now()
	screen.queryTerminalBackground()
	assert.Assert(t, time.Since(start) < 50*time.Millisecond, "Waited for %s", time.Since(start))

	assert.Assert(t, screen.TerminalBackground() == nil)

	writeTerminal(t, terminal, "q")
	assertNextEventIsQ(t, screen)
}

// All three responses should be consumed, without showing up as events
func TestTerminalBackgroundAlternateScrollResponse(t *testing.T) {
	screen, terminal, _ := newPipeTestScreen(t)
	writeTerminal(t, terminal, backgroundResponse+alternateScrollOffResponse+cursorPositionResponse)

	screen.queryTerminalBackground()

	screen.terminalBackgroundLock.Lock()
	alternateScroll := screen.alternateScroll
	screen.terminalBackgroundLock.Unlock()
	assert.Equal(t, alternateScroll, alternateScrollSupported)

	writeTerminal(t, terminal, "q")
	assertNextEventIsQ(t, screen)
}

// Keys typed before the responses arrive should be reported as usual, without
// making us miss the responses.
func TestTerminalBackgroundKeyFirst(t *testing.T) {
	screen, terminal, _ := newPipeTestScreen(t)
	writeTerminal(t, terminal, "q"+backgroundResponse+cursorPositionResponse)

	screen.queryTerminalBackground()

	background := screen.TerminalBackground()
	assert.Assert(t, background != nil)
	assert.Equal(t, *background, NewColorHex(0x123456))

	assertNextEventIsQ(t, screen)
}

// After an unsupported escape sequence, the main loop must go on parsing
// whatever comes after it.
func TestMainLoopKeyAfterUnsupportedSequence(t *testing.T) {
	screen, terminal, _ := newPipeTestScreen(t)

	// Get the main loop past expecting responses to the background color query
	writeTerminal(t, terminal, backgroundResponse+cursorPositionResponse)
	screen.queryTerminalBackground()

	// A "terminal OK" report, which we never asked for
	writeTerminal(t, terminal, "\x1b[0n"+"q")
	assertNextEventIsQ(t, screen)
}

// A screen that isn't connected to any terminal, for testing processInput()
func newInputTestScreen() *terminalScreen {
	return &terminalScreen{events: make(chan Event, 10)}
}

// Assert that the screen has posted exactly these events, no more, no less
func assertEvents(t *testing.T, screen *terminalScreen, expected ...Event) {
	t.Helper()
	var actual []Event
	for len(screen.events) > 0 {
		actual = append(actual, <-screen.events)
	}
	assert.DeepEqual(t, actual, expected)
}

// Responses can be split across reads
func TestProcessInputSplitResponses(t *testing.T) {
	screen := newInputTestScreen()

	// Split in the middle of the background color response
	incomplete := screen.processInput(backgroundResponse[:10])
	assert.Equal(t, incomplete, backgroundResponse[:10])

	// Split in the middle of the cursor position response
	incomplete = screen.processInput(incomplete + backgroundResponse[10:] + cursorPositionResponse[:4])
	assert.Equal(t, incomplete, cursorPositionResponse[:4])
	assert.Assert(t, !screen.terminalBackgroundDone)

	incomplete = screen.processInput(incomplete + cursorPositionResponse[4:] + "q")
	assert.Equal(t, incomplete, "")
	assert.Assert(t, screen.terminalBackgroundDone)

	assert.Equal(t, *screen.terminalBackground, NewColorHex(0x123456))
	assertEvents(t, screen, EventRune{Rune: 'q'})
}

// A background color response terminated by ST, split between the ESC and the
// backslash of the ST
func TestProcessInputSplitST(t *testing.T) {
	screen := newInputTestScreen()
	response := strings.TrimSuffix(backgroundResponse, "\x07") + "\x1b\\"

	incomplete := screen.processInput(response[:len(response)-1])
	assert.Equal(t, incomplete, response[:len(response)-1])

	incomplete = screen.processInput(incomplete + response[len(response)-1:] + cursorPositionResponse)
	assert.Equal(t, incomplete, "")
	assert.Assert(t, screen.terminalBackgroundDone)

	assert.Equal(t, *screen.terminalBackground, NewColorHex(0x123456))
	assertEvents(t, screen)
}

// If the user types something before the responses arrive, we should get both
// the keypress and the responses
func TestProcessInputKeyFirst(t *testing.T) {
	screen := newInputTestScreen()

	incomplete := screen.processInput("q" + backgroundResponse + cursorPositionResponse)
	assert.Equal(t, incomplete, "")
	assert.Assert(t, screen.terminalBackgroundDone)

	assert.Assert(t, screen.terminalBackground != nil)
	assert.Equal(t, *screen.terminalBackground, NewColorHex(0x123456))
	assertEvents(t, screen, EventRune{Rune: 'q'})
}

// Once queryTerminalBackground() has given up waiting, late responses should be
// ignored
func TestProcessInputLateBackgroundResponse(t *testing.T) {
	screen := newInputTestScreen()
	screen.terminalBackgroundDone = true

	incomplete := screen.processInput(backgroundResponse + cursorPositionResponse + "q")
	assert.Equal(t, incomplete, "")

	assert.Assert(t, screen.terminalBackground == nil)
	assertEvents(t, screen, EventRune{Rune: 'q'})
}

// Mouse events can be split across reads as well, also after we're done with
// the query responses
func TestProcessInputSplitMouseEvent(t *testing.T) {
	screen := newInputTestScreen()
	screen.terminalBackgroundDone = true

	incomplete := screen.processInput("\x1b[<64;10;2")
	assert.Equal(t, incomplete, "\x1b[<64;10;2")
	assertEvents(t, screen)

	incomplete = screen.processInput(incomplete + "0Mq")
	assert.Equal(t, incomplete, "")
	assertEvents(t, screen, EventMouse{Buttons: MouseWheelUp}, EventRune{Rune: 'q'})
}

// A lone ESC is the Escape key, not the start of a split sequence
func TestProcessInputLoneEscape(t *testing.T) {
	screen := newInputTestScreen()

	incomplete := screen.processInput("\x1b")
	assert.Equal(t, incomplete, "")
	assertEvents(t, screen, EventKeyCode{KeyCode: KeyEscape})
}

// Windows Terminal with WSL has been seen delivering the background color
// response in three pieces:
// "\x1b]11;rgb:2828/28" + "28/2828" + "\x1b\\"
//
// We split it in four, for a margin of error.
//
// Ref: https://github.com/walles/moor/issues/271
func TestProcessInputBackgroundResponseInFourPieces(t *testing.T) {
	screen := newInputTestScreen()
	response := "\x1b]11;rgb:2828/2828/2828\x1b\\"

	incomplete := screen.processInput(response[:16])
	incomplete = screen.processInput(incomplete + response[16:21])
	incomplete = screen.processInput(incomplete + response[21:24])
	incomplete = screen.processInput(incomplete + response[24:] + cursorPositionResponse)
	assert.Equal(t, incomplete, "")
	assert.Assert(t, screen.terminalBackgroundDone)

	assert.Assert(t, screen.terminalBackground != nil)
	assert.Equal(t, *screen.terminalBackground, NewColorHex(0x282828))
	assertEvents(t, screen)
}

// Alt-] sends "\x1b]", which looks like the start of an OSC sequence. Control
// characters can't be part of an OSC sequence, so a Ctrl-C after it must come
// through.
func TestProcessInputUnterminatedOscThenCtrlC(t *testing.T) {
	screen := newInputTestScreen()

	incomplete := screen.processInput("\x1b]")
	assert.Equal(t, incomplete, "\x1b]")

	incomplete = screen.processInput(incomplete + "\x03")
	assert.Equal(t, incomplete, "")
	assertEvents(t, screen, EventRune{Rune: '\x03'})
}

// Text typed after Alt-] ("\x1b]") is valid OSC sequence content. We must give
// up on it at some point, rather than hold all input forever.
func TestProcessInputUnterminatedOscThenText(t *testing.T) {
	screen := newInputTestScreen()

	incomplete := screen.processInput("\x1b]")
	for i := 0; i < 10 && incomplete != ""; i++ {
		incomplete = screen.processInput(incomplete + "xxxxxxxxxxxxxxxx")
	}

	// The held sequence got too long, so we should have given up on it and
	// dropped it
	assert.Equal(t, len(incomplete), 0, "Still holding: %q", incomplete)

	screen.processInput("q")
	assertEvents(t, screen, EventRune{Rune: 'q'})
}

// The DECRQM response says how the terminal handles Alternate Scroll Mode:
// "\x1b[?1007;N$y". Everything except "not recognized" (0) and "permanently
// reset" (4) means we can use it, since we enable it ourselves when entering
// the alternate screen. Statuses not in the spec tell us nothing.
//
// Ref: https://vt100.net/docs/vt510-rm/DECRPM.html
func TestProcessInputAlternateScrollStatus(t *testing.T) {
	for _, testCase := range []struct {
		status   string
		expected alternateScrollSupport
	}{
		{"0", alternateScrollUnsupported}, // Not recognized
		{"1", alternateScrollSupported},   // Set
		{"2", alternateScrollSupported},   // Reset
		{"3", alternateScrollSupported},   // Permanently set
		{"4", alternateScrollUnsupported}, // Permanently reset
		{"5", alternateScrollUnknown},     // Not in the spec
	} {
		t.Run(testCase.status, func(t *testing.T) {
			screen := newInputTestScreen()

			incomplete := screen.processInput("\x1b[?1007;" + testCase.status + "$y" + cursorPositionResponse + "q")
			assert.Equal(t, incomplete, "")
			assert.Assert(t, screen.terminalBackgroundDone)

			assert.Equal(t, screen.alternateScroll, testCase.expected)
			assertEvents(t, screen, EventRune{Rune: 'q'})
		})
	}
}

// Terminals that don't know DECRQM send only the other responses
func TestProcessInputNoAlternateScrollResponse(t *testing.T) {
	screen := newInputTestScreen()

	incomplete := screen.processInput(backgroundResponse + cursorPositionResponse)
	assert.Equal(t, incomplete, "")
	assert.Assert(t, screen.terminalBackgroundDone)

	assert.Equal(t, screen.alternateScroll, alternateScrollUnknown)
}

// A DECRQM response for some other mode says nothing about Alternate Scroll
// Mode. This one is for bracketed paste.
func TestProcessInputOtherModeResponse(t *testing.T) {
	screen := newInputTestScreen()

	incomplete := screen.processInput("\x1b[?2004;1$y" + cursorPositionResponse + "q")
	assert.Equal(t, incomplete, "")

	assert.Equal(t, screen.alternateScroll, alternateScrollUnknown)
	assertEvents(t, screen, EventRune{Rune: 'q'})
}

func TestProcessInputSplitAlternateScrollResponse(t *testing.T) {
	screen := newInputTestScreen()

	incomplete := screen.processInput(alternateScrollOffResponse[:6])
	assert.Equal(t, incomplete, alternateScrollOffResponse[:6])

	incomplete = screen.processInput(incomplete + alternateScrollOffResponse[6:] + cursorPositionResponse)
	assert.Equal(t, incomplete, "")
	assert.Assert(t, screen.terminalBackgroundDone)

	assert.Equal(t, screen.alternateScroll, alternateScrollSupported)
	assertEvents(t, screen)
}

// Once queryTerminalBackground() has given up waiting, late responses should be
// ignored
func TestProcessInputLateAlternateScrollResponse(t *testing.T) {
	screen := newInputTestScreen()
	screen.terminalBackgroundDone = true

	incomplete := screen.processInput(alternateScrollOffResponse + cursorPositionResponse + "q")
	assert.Equal(t, incomplete, "")

	assert.Equal(t, screen.alternateScroll, alternateScrollUnknown)
	assertEvents(t, screen, EventRune{Rune: 'q'})
}
