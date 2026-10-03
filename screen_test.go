package twin

import (
	"io"
	"os"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

func assertEncode(t *testing.T, incomingString string, expectedEvent Event, expectedRemainder string) {
	t.Helper()
	actualEvent, actualRemainder := consumeEncodedEvent(incomingString)

	message := strings.ReplaceAll(incomingString, "\x1b", "ESC")
	message = strings.ReplaceAll(message, "\r", "RET")

	assert.Assert(t, actualEvent != nil,
		"Input: %s Result: %#v Expected: %#v", message, "nil", expectedEvent)
	assert.Equal(t, *actualEvent, expectedEvent,
		"Input: %s Result: %#v Expected: %#v", message, *actualEvent, expectedEvent)
	assert.Equal(t, actualRemainder, expectedRemainder, message)
}

func TestConsumeEncodedEvent(t *testing.T) {
	assertEncode(t, "a", EventRune{Rune: 'a'}, "")
	assertEncode(t, "\r", EventKeyCode{KeyCode: KeyEnter}, "")
	assertEncode(t, "\x1b", EventKeyCode{KeyCode: KeyEscape}, "")

	// Implicitly test having a remaining rune at the end
	assertEncode(t, "\x1b[Ax", EventKeyCode{KeyCode: KeyUp}, "x")

	assertEncode(t, "\x1b[<64;127;41M", EventMouse{Buttons: MouseWheelUp}, "")
	assertEncode(t, "\x1b[<65;127;41M", EventMouse{Buttons: MouseWheelDown}, "")

	// This happens when users paste.
	//
	// Ref: https://github.com/walles/moor/issues/73
	assertEncode(t, "1234", EventRune{Rune: '1'}, "234")
}

// Unsupported escape sequences should be dropped, but only those. Whatever comes
// after them must survive.
func assertDropsUnsupported(t *testing.T, unsupported string) {
	t.Helper()
	event, remainder := consumeEncodedEvent(unsupported + "q")

	message := strings.ReplaceAll(unsupported, "\x1b", "ESC")
	message = strings.ReplaceAll(message, "\x07", "BEL")

	assert.Assert(t, event == nil, "Input: %s Result: %#v", message, event)
	assert.Equal(t, remainder, "q", message)
}

func TestConsumeEncodedEventWithUnsupportedCSI(t *testing.T) {
	// A cursor position report, as sent by the terminal in response to "\x1b[6n"
	assertDropsUnsupported(t, "\x1b[12;40R")
}

func TestConsumeEncodedEventWithUnsupportedOSCBel(t *testing.T) {
	// A terminal background color report, terminated by BEL
	assertDropsUnsupported(t, "\x1b]11;rgb:1234/5678/9abc\x07")
}

func TestConsumeEncodedEventWithUnsupportedOSCST(t *testing.T) {
	// A terminal background color report, terminated by ST
	assertDropsUnsupported(t, "\x1b]11;rgb:1234/5678/9abc\x1b\\")
}

func TestConsumeEncodedEventWithUnsupportedMouseEvent(t *testing.T) {
	// A left mouse button press
	assertDropsUnsupported(t, "\x1b[<0;10;20M")
}

func TestConsumeEncodedEventWithNoInput(t *testing.T) {
	event, remainder := consumeEncodedEvent("")
	assert.Assert(t, event == nil)
	assert.Equal(t, remainder, "")
}

func TestRenderLine(t *testing.T) {
	row := []StyledRune{
		{
			Rune:  '<',
			Style: StyleDefault.WithAttr(AttrReverse),
		},
		{
			Rune:  'f',
			Style: StyleDefault.WithAttr(AttrDim),
		},
	}

	rendered, count := renderLine(row, 33, ColorCount16)
	assert.Equal(t, count, 2)
	reset := "\x1b[m"
	reversed := "\x1b[7m"
	notReversed := "\x1b[27m"
	dim := "\x1b[2m"
	clearToEol := "\x1b[K"
	assert.Equal(t,
		strings.ReplaceAll(rendered, "\x1b", "ESC"),
		strings.ReplaceAll(reset+reversed+"<"+dim+notReversed+"f"+reset+clearToEol, "\x1b", "ESC"))
}

func TestRenderLineEmpty(t *testing.T) {
	row := []StyledRune{}

	rendered, count := renderLine(row, 33, ColorCount16)
	assert.Equal(t, count, 0)

	// All lines are expected to stand on their own, so we always need to clear
	// to EOL whether or not the line is empty.
	assert.Equal(t, rendered, "\x1b[m\x1b[K")
}

func TestRenderLineLastReversed(t *testing.T) {
	row := []StyledRune{
		{
			Rune:  '<',
			Style: StyleDefault.WithAttr(AttrReverse),
		},
	}

	rendered, count := renderLine(row, 33, ColorCount16)
	assert.Equal(t, count, 1)
	reset := "\x1b[m"
	reversed := "\x1b[7m"
	clearToEol := "\x1b[K"
	assert.Equal(t,
		strings.ReplaceAll(rendered, "\x1b", "ESC"),
		strings.ReplaceAll(reset+reversed+"<"+reset+clearToEol, "\x1b", "ESC"))
}

func TestRenderLineLastNonSpace(t *testing.T) {
	row := []StyledRune{
		{
			Rune:  'X',
			Style: StyleDefault,
		},
	}

	rendered, count := renderLine(row, 33, ColorCount16)
	assert.Equal(t, count, 1)
	reset := "\x1b[m"
	clearToEol := "\x1b[K"
	assert.Equal(t,
		strings.ReplaceAll(rendered, "\x1b", "ESC"),
		strings.ReplaceAll(reset+"X"+clearToEol, "\x1b", "ESC"))
}

func TestRenderLineLastReversedPlusTrailingSpace(t *testing.T) {
	row := []StyledRune{
		{
			Rune:  '<',
			Style: StyleDefault.WithAttr(AttrReverse),
		},
		{
			Rune:  ' ',
			Style: StyleDefault,
		},
	}

	rendered, count := renderLine(row, 33, ColorCount16)
	assert.Equal(t, count, 1)
	reset := "\x1b[m"
	reversed := "\x1b[7m"
	clearToEol := "\x1b[K"
	assert.Equal(t,
		strings.ReplaceAll(rendered, "\x1b", "ESC"),
		strings.ReplaceAll(reset+reversed+"<"+reset+clearToEol, "\x1b", "ESC"))
}

func TestRenderLineOnlyTrailingSpaces(t *testing.T) {
	row := []StyledRune{
		{
			Rune:  ' ',
			Style: StyleDefault,
		},
		{
			Rune:  ' ',
			Style: StyleDefault,
		},
	}

	rendered, count := renderLine(row, 33, ColorCount16)
	assert.Equal(t, count, 0)

	// All lines are expected to stand on their own, so we always need to clear
	// to EOL whether or not the line is empty.
	assert.Equal(t, rendered, "\x1b[m\x1b[K")
}

func TestRenderLineLastReversedSpaces(t *testing.T) {
	row := []StyledRune{
		{
			Rune:  ' ',
			Style: StyleDefault.WithAttr(AttrReverse),
		},
	}

	rendered, count := renderLine(row, 33, ColorCount16)
	assert.Equal(t, count, 1)
	reset := "\x1b[m"
	reversed := "\x1b[7m"
	clearToEol := "\x1b[K"
	assert.Equal(t,
		strings.ReplaceAll(rendered, "\x1b", "ESC"),
		strings.ReplaceAll(reset+reversed+" "+reset+clearToEol, "\x1b", "ESC"))
}

func TestRenderLineNonPrintable(t *testing.T) {
	row := []StyledRune{
		{
			Rune: '\x1b',
		},
	}

	rendered, count := renderLine(row, 33, ColorCount16)
	assert.Equal(t, count, 1)
	reset := "\x1b[m"
	white := "\x1b[37m"
	redBg := "\x1b[41m"
	bold := "\x1b[1m"
	clearToEol := "\x1b[K"
	assert.Equal(t,
		strings.ReplaceAll(rendered, "\x1b", "ESC"),
		strings.ReplaceAll(reset+white+redBg+bold+"?"+reset+clearToEol, "\x1b", "ESC"))
}

func TestRenderHyperlinkAtEndOfLine(t *testing.T) {
	url := "https://example.com/"
	row := []StyledRune{
		{
			Rune:  '*',
			Style: StyleDefault.WithHyperlink(&url),
		},
	}

	rendered, count := renderLine(row, 33, ColorCount16)
	assert.Equal(t, count, 1)

	assert.Equal(t,
		strings.ReplaceAll(rendered, "\x1b", "ESC"),
		`ESC[mESC]8;;`+url+`ESC\*ESC]8;;ESC\ESC[K`)
}

func TestMultiCharHyperlink(t *testing.T) {
	url := "https://example.com/"
	row := []StyledRune{
		{
			Rune:  '-',
			Style: StyleDefault.WithHyperlink(&url),
		},
		{
			Rune:  'X',
			Style: StyleDefault.WithHyperlink(&url),
		},
		{
			Rune:  '-',
			Style: StyleDefault.WithHyperlink(&url),
		},
	}

	rendered, count := renderLine(row, 33, ColorCount16)
	assert.Equal(t, count, 3)

	assert.Equal(t,
		strings.ReplaceAll(rendered, "\x1b", "ESC"),
		`ESC[mESC]8;;`+url+`ESC\-X-ESC]8;;ESC\ESC[K`)
}

func TestRenderLineFullWidth(t *testing.T) {
	row := []StyledRune{
		{
			Rune: 'x',
		},
		{
			Rune: 'y',
		},
	}

	rendered, count := renderLine(row, 2, ColorCount16)
	assert.Equal(t, count, 2)

	assert.Equal(t,
		strings.ReplaceAll(rendered, "\x1b", "ESC"),
		"ESC[mxy", "Expected no clear-to-EOL at the end of a full-width line")

	rendered, count = renderLine(row, 3, ColorCount16)
	assert.Equal(t, count, 2)

	assert.Equal(t,
		strings.ReplaceAll(rendered, "\x1b", "ESC"),
		"ESC[mxyESC[K", "Expected clear-to-EOL at the end of a full-width line")
}

// The default, zero-value cursor state must render as hidden. This is what
// Clear() resets the cursor to, and what a freshly created screen starts out
// as.
func TestRenderCursorLockedDefaultHidden(t *testing.T) {
	screen := &terminalScreen{}

	rendered := screen.renderCursorLocked(80, 24)
	assert.Equal(t, rendered, "\x1b[?25l")
}

// A cursor shown within the screen's bounds must be positioned and made
// visible.
func TestRenderCursorLockedVisible(t *testing.T) {
	screen := &terminalScreen{}
	screen.cursor = cursorState{visible: true, column: 3, row: 5}

	rendered := screen.renderCursorLocked(80, 24)

	// CUP is 1-based, our column/row are 0-based.
	assert.Equal(t, rendered, "\x1b[6;4H\x1b[?25h")
}

// A resize can shrink the screen between a ShowCursor() call and the next
// render, leaving the requested coordinate outside the new bounds. Rather
// than emit a CUP that could land outside the terminal, this must fall back
// to hiding the cursor.
func TestRenderCursorLockedOutOfBounds(t *testing.T) {
	screen := &terminalScreen{}

	screen.cursor = cursorState{visible: true, column: 80, row: 5}
	assert.Equal(t, screen.renderCursorLocked(80, 24), "\x1b[?25l", "column out of bounds")

	screen.cursor = cursorState{visible: true, column: 3, row: 24}
	assert.Equal(t, screen.renderCursorLocked(80, 24), "\x1b[?25l", "row out of bounds")

	screen.cursor = cursorState{visible: true, column: -1, row: 5}
	assert.Equal(t, screen.renderCursorLocked(80, 24), "\x1b[?25l", "negative column")
}

// PrintLines() writes onto the user's own shell screen, not the alternate
// screen, so repositioning or showing the real cursor there would leave it in
// the wrong place once the shell prompt comes back. It must never touch the
// cursor, no matter what ShowCursor() last requested.
func TestPrintLinesNeverTouchesCursor(t *testing.T) {
	screen, _ := newSizeTestScreen(20, 8)

	screen.Clear()
	screen.SetCell(0, 0, StyledRune{Rune: 'X', Style: StyleDefault})
	screen.ShowCursor(3, 4)

	read, write, err := os.Pipe()
	assert.NilError(t, err)
	screen.ttyOut = write

	screen.PrintLines(8)

	assert.NilError(t, write.Close())
	output, err := io.ReadAll(read)
	assert.NilError(t, err)

	assert.Assert(t, !strings.Contains(string(output), cursorHideSeq), "PrintLines() must never hide the cursor")
	assert.Assert(t, !strings.Contains(string(output), cursorShowSeq), "PrintLines() must never show or reposition the cursor")
}

// Test the most basic form of interruptability. Interrupting and sending a byte
// should make the reader return EOF.
//
// What we really want is for the reader to return EOF immediately when
// interrupted, with no write needed.
//
// This test should be replaced by
// TestInterruptableReader_blockedOnReadImmediate if or when the Windows
// implementation catches up.
func TestInterruptableReader_blockedOnRead(t *testing.T) {
	// Make a pipe to read from and write to
	pipeReader, pipeWriter, err := os.Pipe()
	assert.NilError(t, err)

	// Make an interruptable reader
	testMe := newInterruptableReader(pipeReader)

	// Start a thread that reads from the pipe
	type readResult struct {
		n   int
		err error
	}
	readResultChan := make(chan readResult)
	go func() {
		defer func() {
			panicHandler("TestInterruptableReader_blockedOnRead()", recover(), debug.Stack())
		}()

		buffer := make([]byte, 1)
		n, err := testMe.Read(buffer)
		readResultChan <- readResult{n, err}
	}()

	// Give the reader thread some time to start waiting
	time.Sleep(100 * time.Millisecond)

	// Ensure the reader is not done, it should be blocked on the read()
	select {
	case <-readResultChan:
		t.Fatal("Reader should not be done yet")
	default:
	}

	// Interrupt the reader
	testMe.Interrupt()

	// Write a byte to the pipe
	n, err := pipeWriter.Write([]byte{42})
	assert.NilError(t, err)
	assert.Equal(t, n, 1)

	// Wait for the reader thread to finish
	result := <-readResultChan

	// Check the result
	assert.Equal(t, result.n, 0)
	assert.Equal(t, result.err, io.EOF)

	// Another read should return EOF immediately
	buffer := make([]byte, 1)
	n, err = testMe.Read(buffer)
	assert.Equal(t, err, io.EOF)
	assert.Equal(t, n, 0)
}

// An idle reader must keep re-asserting the terminal mode.
//
// A reset that happens while nothing is readable would otherwise never be
// noticed: in cooked mode the kernel line buffers keystrokes, so waiting for
// something to read before re-asserting would be waiting for input that cannot
// arrive.
func TestInterruptableReader_reassertsModeWhileIdle(t *testing.T) {
	// Make a pipe to read from, and never write anything to it
	pipeReader, _, err := os.Pipe()
	assert.NilError(t, err)

	// Make an interruptable reader that counts its terminal mode re-asserts
	testMe := newInterruptableReader(pipeReader)
	var reasserts atomic.Int32
	testMe.reassert = func(*os.File) {
		reasserts.Add(1)
	}

	// Start a thread that reads from the pipe. With nothing to read it will
	// just keep polling.
	readDone := make(chan struct{})
	go func() {
		defer func() {
			panicHandler("TestInterruptableReader_reassertsModeWhileIdle()", recover(), debug.Stack())
		}()

		buffer := make([]byte, 1)
		_, _ = testMe.Read(buffer)
		close(readDone)
	}()

	// Long enough for the reader to poll a few times
	time.Sleep(3 * interruptableReaderMaxWait)

	assert.Assert(t, reasserts.Load() > 1,
		"Idle reader should have re-asserted the terminal mode repeatedly, did it %d times",
		reasserts.Load())

	testMe.Interrupt()
	<-readDone
}

// Resuming from a pause must re-assert the terminal mode before the reader
// blocks on its next read.
//
// Whatever ran during the pause may have left the terminal in a mode of its
// own choosing. A reader that blocks first and re-asserts afterwards can end
// up waiting for input that the current mode will never deliver, and then the
// re-assert never happens because the blocked read is holding the semaphore.
func TestInterruptableReader_reassertsModeWhenResuming(t *testing.T) {
	// Make a pipe to read from and write to
	pipeReader, pipeWriter, err := os.Pipe()
	assert.NilError(t, err)

	// Make an interruptable reader that counts its terminal mode re-asserts
	testMe := newInterruptableReader(pipeReader)
	var reasserts atomic.Int32
	testMe.reassert = func(*os.File) {
		reasserts.Add(1)
	}

	// Give the reader something to read, so that it gets all the way to its
	// blocking read rather than looping on "nothing available yet"
	n, err := pipeWriter.Write([]byte{42})
	assert.NilError(t, err)
	assert.Equal(t, n, 1)

	testMe.SetPaused(true)

	// Start a thread that reads from the pipe
	readDone := make(chan struct{})
	go func() {
		defer func() {
			panicHandler("TestInterruptableReader_reassertsModeWhenResuming()", recover(), debug.Stack())
		}()

		buffer := make([]byte, 1)
		_, _ = testMe.Read(buffer)
		close(readDone)
	}()

	// Wait for the reader thread to reach the blocking acquire it does just
	// before reading. That takes only as long as starting a goroutine: the pipe
	// already has data, so the readiness check returns immediately rather than
	// polling for interruptableReaderMaxWait.
	//
	// Sleeping too little would make this test pass without proving anything,
	// since a reader still at the top of its loop re-asserts there instead.
	// Measured: one millisecond is enough, so this is a hundredfold margin.
	time.Sleep(100 * time.Millisecond)

	// Only re-asserts made after resuming count
	reasserts.Store(0)

	testMe.SetPaused(false)

	<-readDone

	assert.Assert(t, reasserts.Load() > 0,
		"Terminal mode should have been re-asserted when resuming from the pause")
}

// Pausing must always be possible, also right after a pause during which
// somebody else consumed our input.
//
// A reader that got told there was something to read, and then waited out a
// pause before acting on it, can find the input gone by the time it gets to
// read. Committing to a blocking read at that point would park the reader with
// the pause semaphore held, and Close() would then have to wait for a keypress
// that nobody is going to make.
func TestInterruptableReader_pausableWhenPauseConsumedTheInput(t *testing.T) {
	// Make a pipe to read from and write to
	pipeReader, pipeWriter, err := os.Pipe()
	assert.NilError(t, err)

	// Make an interruptable reader
	testMe := newInterruptableReader(pipeReader)
	testMe.reassert = func(*os.File) {}

	// Give the reader something to read, so that it gets all the way to its
	// blocking acquire rather than looping on "nothing available yet"
	n, err := pipeWriter.Write([]byte{42})
	assert.NilError(t, err)
	assert.Equal(t, n, 1)

	testMe.SetPaused(true)

	// Start a thread that reads from the pipe
	go func() {
		defer func() {
			panicHandler("TestInterruptableReader_pausableWhenPauseConsumedTheInput()", recover(), debug.Stack())
		}()

		buffer := make([]byte, 1)
		_, _ = testMe.Read(buffer)
	}()

	// Wait for the reader thread to reach the blocking acquire it does just
	// before reading. Same reasoning and margin as in
	// TestInterruptableReader_reassertsModeWhenResuming().
	time.Sleep(100 * time.Millisecond)

	// This is the paused-in code eating the keystroke the reader was told about
	buffer := make([]byte, 1)
	n, err = pipeReader.Read(buffer)
	assert.NilError(t, err)
	assert.Equal(t, n, 1)

	testMe.SetPaused(false)

	// This is what Close() does
	testMe.Interrupt()
	pausedAgain := make(chan struct{})
	go func() {
		defer func() {
			panicHandler("TestInterruptableReader_pausableWhenPauseConsumedTheInput()", recover(), debug.Stack())
		}()

		testMe.SetPaused(true)
		close(pausedAgain)
	}()

	select {
	case <-pausedAgain:
	case <-time.After(2 * time.Second):
		t.Fatal("Pausing should not have to wait for input that nobody is going to type")
	}
}

func TestInterruptableReader_interruptFirstReadLater(t *testing.T) {
	// Make a pipe to read from and write to
	pipeReader, pipeWriter, err := os.Pipe()
	assert.NilError(t, err)

	// Make an interruptable reader
	testMe := newInterruptableReader(pipeReader)

	// Interrupt the reader
	testMe.Interrupt()

	// Write something so that we have something to read
	n, err := pipeWriter.Write([]byte{42})
	assert.NilError(t, err)
	assert.Equal(t, n, 1)

	// Try reading from the interrupted reader
	buffer := make([]byte, 1)
	n, err = testMe.Read(buffer)
	assert.Equal(t, n, 0)
	assert.Equal(t, err, io.EOF)
}

func TestInterruptableReader_justRead(t *testing.T) {
	// Make a pipe to read from and write to
	pipeReader, pipeWriter, err := os.Pipe()
	assert.NilError(t, err)

	// Make an interruptable reader
	testMe := newInterruptableReader(pipeReader)

	// Write something so that we have something to read
	n, err := pipeWriter.Write([]byte{42})
	assert.NilError(t, err)
	assert.Equal(t, n, 1)

	// Try reading from the reader
	buffer := make([]byte, 7)
	n, err = testMe.Read(buffer)
	assert.Equal(t, n, 1)
	assert.NilError(t, err)
	assert.Equal(t, buffer[0], byte(42))
	assert.Equal(t, len(buffer), 7)
}

func TestInterruptableReader_waitForReadReadyPipe(t *testing.T) {
	// Make a pipe to read from and write to
	pipeReader, pipeWriter, err := os.Pipe()
	assert.NilError(t, err)

	t.Cleanup(func() {
		_ = pipeReader.Close()
		_ = pipeWriter.Close()
	})

	// Make an interruptable reader
	testMe := newInterruptableReader(pipeReader)

	// With no data available we should wait a bit, then report not ready.
	t0 := time.Now()
	ready, err := testMe.waitForReadReady(time.Millisecond * 100)
	duration := time.Since(t0)
	assert.NilError(t, err)
	assert.Equal(t, ready, false)
	assert.Assert(t, duration > time.Millisecond*100)

	// After writing, the pipe should become ready.
	n, err := pipeWriter.Write([]byte{42})
	assert.NilError(t, err)
	assert.Equal(t, n, 1)

	// With data available we should report ready immediately
	ready, err = testMe.waitForReadReady(time.Hour)
	assert.NilError(t, err)
	assert.Equal(t, ready, true)
}

// On Unix, files are always ready (to return EOF if nothing else), but on
// Windows they are non-ready if they have no data. So we just verify the
// have-data case here, and let the no-data case be whatever.
func TestInterruptableReader_waitForReadReadyFile(t *testing.T) {
	tempFile, err := os.CreateTemp("", "moor-wait-for-read-ready-*.txt")
	assert.NilError(t, err)

	t.Cleanup(func() {
		_ = tempFile.Close()
		_ = os.Remove(tempFile.Name())
	})

	// Put something in the file
	n, err := tempFile.Write([]byte("x"))
	assert.NilError(t, err)
	assert.Equal(t, n, 1)

	// Rewind so we can see the "x"
	seek, err := tempFile.Seek(0, 0)
	assert.NilError(t, err)
	assert.Equal(t, seek, int64(0))

	// Expect read-ready immediately
	testMe := newInterruptableReader(tempFile)
	ready, err := testMe.waitForReadReady(time.Hour)
	assert.NilError(t, err)
	assert.Equal(t, ready, true)
}
