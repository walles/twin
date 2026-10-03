package twin

import (
	"os"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

// What a terminal with a 0x123456 background sends in response to "\x1b]11;?"
const backgroundReply = "\x1b]11;rgb:1212/3434/5656\x07"

// What a terminal sends in response to "\x1b[6n"
const cursorPositionReply = "\x1b[12;40R"

// A screen with mainLoop() running, reading from a pipe instead of from a
// terminal.
//
// Write to the returned terminal to fake terminal input. Read what the screen
// wrote to the terminal using the returned output function.
func newPipeTestScreen(t *testing.T) (screen *terminalScreen, terminal *os.File, output func() string) {
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
		bytes, err := os.ReadFile(ttyOut.Name())
		assert.NilError(t, err)
		return string(bytes)
	}

	return screen, terminal, output
}

func writeTerminal(t *testing.T, terminal *os.File, s string) {
	_, err := terminal.WriteString(s)
	assert.NilError(t, err)
}

// Assert that the next event is a 'q' keypress, with nothing before it
func assertNextEventIsQ(t *testing.T, screen *terminalScreen) {
	select {
	case event := <-screen.events:
		assert.Equal(t, event, Event(EventRune{Rune: 'q'}))
	case <-time.After(time.Second):
		t.Fatal("Timed out waiting for the 'q' keypress")
	}
}

// The cursor position request is what tells us we can stop waiting for an
// answer to the background color query.
func TestTerminalBackgroundQueryAsksForCursorPosition(t *testing.T) {
	screen, terminal, output := newPipeTestScreen(t)
	writeTerminal(t, terminal, cursorPositionReply)

	screen.queryTerminalBackground()

	assert.Assert(t, strings.Contains(output(), "\x1b]11;?\x07\x1b[6n"), humanizeLowASCII(output()))
}

// Slow links can take a while to answer. That's fine, as long as they answer.
func TestTerminalBackgroundSlowAnswer(t *testing.T) {
	screen, terminal, _ := newPipeTestScreen(t)
	written := make(chan struct{})
	go func() {
		defer close(written)
		time.Sleep(100 * time.Millisecond)
		_, err := terminal.WriteString(backgroundReply + cursorPositionReply)
		assert.Check(t, err)
	}()

	// Cleanups run last-registered-first, so this runs before the pipe closes
	t.Cleanup(func() { <-written })

	screen.queryTerminalBackground()

	background := screen.TerminalBackground()
	assert.Assert(t, background != nil)
	assert.Equal(t, *background, NewColorHex(0x123456))
}

// Both replies should be consumed, without showing up as events
func TestTerminalBackgroundAnswerThenKey(t *testing.T) {
	screen, terminal, _ := newPipeTestScreen(t)
	writeTerminal(t, terminal, backgroundReply+cursorPositionReply)

	screen.queryTerminalBackground()

	background := screen.TerminalBackground()
	assert.Assert(t, background != nil)
	assert.Equal(t, *background, NewColorHex(0x123456))

	writeTerminal(t, terminal, "q")
	assertNextEventIsQ(t, screen)
}

// A terminal that doesn't support background color queries. The cursor
// position reply should make us give up right away, rather than waiting for a
// background color that's never coming.
func TestTerminalBackgroundNoAnswer(t *testing.T) {
	screen, terminal, _ := newPipeTestScreen(t)
	writeTerminal(t, terminal, cursorPositionReply)

	start := time.Now()
	screen.queryTerminalBackground()
	assert.Assert(t, time.Since(start) < 250*time.Millisecond, "Waited for %s", time.Since(start))

	assert.Assert(t, screen.TerminalBackground() == nil)

	writeTerminal(t, terminal, "q")
	assertNextEventIsQ(t, screen)
}

// After an unsupported escape sequence, the main loop must go on parsing
// whatever comes after it.
func TestMainLoopKeyAfterUnsupportedSequence(t *testing.T) {
	screen, terminal, _ := newPipeTestScreen(t)

	// Get the main loop past expecting answers to the background color query
	writeTerminal(t, terminal, backgroundReply+cursorPositionReply)
	time.Sleep(50 * time.Millisecond)

	writeTerminal(t, terminal, cursorPositionReply+"q")
	assertNextEventIsQ(t, screen)
}
