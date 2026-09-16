package twin

// FakeScreen is an in-memory Screen implementation, needing no real terminal.
// Typically used for testing code that renders to a Screen, but also suitable
// for headless rendering in general.
//
// Create one with NewFakeScreen(width, height) and hand it to the code under
// test. Let that code call SetCell(), Clear(), Show() etc. as it normally
// would, then call GetRow() or GetCell() afterwards to inspect what ended up on
// screen.
//
// Events() always returns nil.
type FakeScreen struct {
	width  int
	height int
	cells  [][]StyledRune
}

// NewFakeScreen creates a FakeScreen of the given size, with all cells set to
// a space in the default style.
func NewFakeScreen(width int, height int) *FakeScreen {
	rows := make([][]StyledRune, height)
	for i := range height {
		rows[i] = make([]StyledRune, width)
	}
	clearCells(rows)

	return &FakeScreen{
		width:  width,
		height: height,
		cells:  rows,
	}
}

// Close does nothing, since a FakeScreen owns no real terminal to restore.
func (screen *FakeScreen) Close() {
}

// Clear erases all screen cells, replacing them with spaces in the default
// style.
func (screen *FakeScreen) Clear() {
	clearCells(screen.cells)
}

// SetCell returns the width of the rune just added, in number of columns.
//
// Note that if you set a wide rune (like '午') in one column, then whatever
// you put in the next column will be hidden by the wide rune. A wide rune in
// the last screen column will be replaced by a space, to prevent it from
// overflowing onto the next line.
func (screen *FakeScreen) SetCell(column int, row int, styledRune StyledRune) int {
	// This method's contents has been copied from terminalScreen.SetCell()

	if column < 0 {
		return styledRune.Width()
	}
	if row < 0 {
		return styledRune.Width()
	}

	width, height := screen.Size()
	if column >= width {
		return styledRune.Width()
	}
	if row >= height {
		return styledRune.Width()
	}

	if column+styledRune.Width() > width {
		// This cell is too wide for the screen, write a space instead
		screen.cells[row][column] = StyledRune{Rune: ' ', Style: styledRune.Style}
		return styledRune.Width()
	}

	screen.cells[row][column] = styledRune

	return styledRune.Width()
}

// GetCell returns the StyledRune at the given screen position.
//
// Note that this does not read cells from a physical screen, since there is
// none, but rather from what was previously set using SetCell().
//
// For out-of-bounds requests, a space with default style is returned.
func (screen *FakeScreen) GetCell(column int, row int) StyledRune {
	// This method's contents has been copied from terminalScreen.GetCell()

	if column < 0 {
		return StyledRune{Rune: ' ', Style: StyleDefault}
	}
	if row < 0 {
		return StyledRune{Rune: ' ', Style: StyleDefault}
	}

	width, height := screen.Size()
	if column >= width {
		return StyledRune{Rune: ' ', Style: StyleDefault}
	}
	if row >= height {
		return StyledRune{Rune: ' ', Style: StyleDefault}
	}

	return screen.cells[row][column]
}

// SetProgress does nothing, since a FakeScreen has no terminal to show a
// progress bar in.
func (screen *FakeScreen) SetProgress(state ProgressState, percent int) {
}

// ShowCursor does nothing, since a FakeScreen has no real terminal to show a
// cursor on.
func (screen *FakeScreen) ShowCursor(column int, row int) {
}

// Show does nothing, since a FakeScreen has no real terminal to render into.
func (screen *FakeScreen) Show() {
}

// PrintLines does nothing, since a FakeScreen has no real terminal to render
// into.
func (screen *FakeScreen) PrintLines(int) {
}

// Size returns the width and height given to NewFakeScreen().
func (screen *FakeScreen) Size() (width int, height int) {
	return screen.width, screen.height
}

// TerminalBackground always returns nil, since a FakeScreen has no real
// terminal to query.
func (screen *FakeScreen) TerminalBackground() *Color {
	return nil
}

// Events always returns nil, so anything reading from it will block forever.
func (screen *FakeScreen) Events() chan Event {
	return nil
}

// GetRow returns the row's cells, skipping any cell hidden behind a
// preceding wide rune.
func (screen *FakeScreen) GetRow(row int) []StyledRune {
	return withoutHiddenRunes(screen.cells[row])
}

// PauseAndCall runs the given function and returns its result. A FakeScreen
// has no terminal state to pause and resume, so there is nothing else to do.
func (screen *FakeScreen) PauseAndCall(run func() error) error {
	return run()
}
