package twin

import "fmt"

// Ref: https://en.wikipedia.org/wiki/ANSI_escape_code#Control_Sequence_Introducer_commands
const (
	cursorHideSeq = "\x1b[?25l"
	cursorShowSeq = "\x1b[?25h"
)

// cursorState is the terminal's real cursor position and visibility, set by
// ShowCursor()/HideCursor() and sent to the terminal on every render.
type cursorState struct {
	visible bool
	column  int
	row     int
}

// ShowCursor places the terminal's real cursor at the given screen coordinate
// and makes it visible. Takes effect on the next Show() call, not immediately.
func (screen *terminalScreen) ShowCursor(column int, row int) {
	screen.renderLock.Lock()
	defer screen.renderLock.Unlock()

	screen.cursor = cursorState{
		visible: true,
		column:  column,
		row:     row,
	}
}

// HideCursor hides the terminal's real cursor again. Takes effect on the next
// Show() call, not immediately. This is the default state, and Clear() also
// resets to it.
func (screen *terminalScreen) HideCursor() {
	screen.renderLock.Lock()
	defer screen.renderLock.Unlock()

	screen.cursor = cursorState{}
}

// renderCursorLocked returns the escape sequence needed to reposition and show
// the cursor, or just hide it. width and height must be the ones already
// established for this frame, since the screen size can change between a
// ShowCursor() call and the next render.
//
// You must hold renderLock when calling this method.
func (screen *terminalScreen) renderCursorLocked(width int, height int) string {
	if !screen.cursor.visible {
		return cursorHideSeq
	}

	if screen.cursor.column < 0 || screen.cursor.column >= width {
		return cursorHideSeq
	}
	if screen.cursor.row < 0 || screen.cursor.row >= height {
		return cursorHideSeq
	}

	// ANSI Cursor Position is 1-based, our column/row are 0-based.
	return fmt.Sprintf(
		"\x1b[%d;%dH"+cursorShowSeq,
		screen.cursor.row+1,
		screen.cursor.column+1,
	)
}
