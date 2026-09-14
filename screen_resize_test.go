package twin

import (
	"os"
	"testing"

	"gotest.tools/v3/assert"
)

// newSizeTestScreen returns a terminalScreen suitable for exercising Size() /
// Clear() / SetCell() / GetCell() resize handling without a real terminal.
// getSize starts out reporting initialWidth x initialHeight; call the returned
// queueResize to change what it reports next and mark a resize as pending, the
// same way a real SIGWINCH would.
func newSizeTestScreen(initialWidth int, initialHeight int) (screen *terminalScreen, queueResize func(width int, height int)) {
	nextWidth, nextHeight := initialWidth, initialHeight

	screen = &terminalScreen{
		ttyOut:   os.Stdout, // Fd() must not panic; the value is never actually used
		sigwinch: make(chan int, 1),
		getSize: func(int) (int, int, error) {
			return nextWidth, nextHeight, nil
		},
	}
	screen.sigwinch <- 0 // Trigger the initial screen size query, like NewScreen() does

	queueResize = func(width int, height int) {
		nextWidth, nextHeight = width, height
		select {
		case screen.sigwinch <- 0:
		default:
			// Already pending, nothing more to do
		}
	}

	return screen, queueResize
}

// A resize landing in the middle of a frame, between two SetCell() calls, must
// not be picked up until the frame is done: otherwise the cell buffer gets
// swapped out mid-draw and whatever was already written is lost.
func TestFrameSurvivesResizeBetweenSetCells(t *testing.T) {
	screen, queueResize := newSizeTestScreen(10, 5)

	screen.Clear()
	screen.SetCell(0, 0, StyledRune{Rune: 'A', Style: StyleDefault})

	// A resize arrives after the first cell was drawn but before the second.
	queueResize(20, 8)

	screen.SetCell(1, 0, StyledRune{Rune: 'B', Style: StyleDefault})

	assert.Equal(t, screen.GetCell(0, 0).Rune, rune('A'))
	assert.Equal(t, screen.GetCell(1, 0).Rune, rune('B'))
}

// Consumers like ftop call Size() to plan a frame's layout before calling
// Clear() to start drawing it. A resize landing between those two calls must
// not let Clear() pick up a different size than the one the layout was planned
// for: whichever of Size()/Clear() runs first after the previous frame is what
// decides this frame's size, and it must stick for the rest of the frame.
func TestClearKeepsSizeEstablishedByEarlierSizeCall(t *testing.T) {
	screen, queueResize := newSizeTestScreen(20, 8)

	width, height := screen.Size()
	assert.Equal(t, width, 20)
	assert.Equal(t, height, 8)

	// A resize lands after the layout was planned for a 20 wide screen, but
	// before Clear() / drawing happens.
	queueResize(10, 5)

	screen.Clear()

	// Still 20 wide: matches what Size() returned above, not the 10 that
	// arrived afterwards.
	width, height = screen.Size()
	assert.Equal(t, width, 20)
	assert.Equal(t, height, 8)

	// A cell written at the edge of the originally-planned 20 wide layout
	// must land there, not get silently dropped by bounds-checking against a
	// narrower buffer.
	screen.SetCell(19, 0, StyledRune{Rune: 'X', Style: StyleDefault})
	assert.Equal(t, screen.GetCell(19, 0).Rune, rune('X'))
}

// Clear() is documented to reset cursor visibility to hidden, the same as
// HideCursor(). This treats the cursor as just another piece of on-screen
// decoration that gets wiped along with cell content.
func TestClearHidesCursor(t *testing.T) {
	screen, _ := newSizeTestScreen(20, 8)

	screen.ShowCursor(3, 4)
	assert.Assert(t, screen.cursor.visible)

	screen.Clear()
	assert.Assert(t, !screen.cursor.visible)
}
