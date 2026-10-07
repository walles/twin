// Package twin provides Terminal Window Interaction
package twin

import (
	"fmt"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// MouseMode controls how mouse events are captured. See MouseModeAuto,
// MouseModeSelect and MouseModeScroll for the available behaviors.
type MouseMode int

const (
	// MouseModeAuto auto-detects whether to capture mouse events, based on
	// the terminal.
	MouseModeAuto MouseMode = iota

	// MouseModeSelect doesn't capture mouse events. This makes selecting with
	// the mouse work. On some terminals mouse scrolling will work using arrow
	// keys emulation, and on some not.
	MouseModeSelect

	// MouseModeScroll captures mouse events. This makes mouse scrolling work.
	// Special gymnastics will be required for marking with the mouse to copy
	// text.
	MouseModeScroll
)

// Options configures a new Screen, created with NewScreen.
//
// The zero value auto-detects mouse mode and terminal color count from the
// environment, and disables twin's own logging.
type Options struct {
	// MouseMode controls how mouse events are captured. Leave as
	// MouseModeAuto to auto-detect based on the terminal.
	MouseMode MouseMode

	// TerminalColorCount overrides how many colors twin assumes the terminal
	// supports. Leave as ColorCountDefault to auto-detect from the
	// environment.
	TerminalColorCount ColorCount

	// Logger receives twin's own log messages. Leave nil to disable logging.
	Logger Logger
}

// Screen is the main interface for interacting with the terminal, created
// with NewScreen.
type Screen interface {
	// Close restores the terminal to normal state, must be called after you are
	// done with the screen returned by NewScreen().
	Close()

	// Erases all screen cells, replacing them with spaces in the default style.
	// Also resets cursor visibility to hidden; call ShowCursor() again after
	// Clear() if you want the cursor to keep showing.
	//
	// Like Size(), may apply a pending resize; see Size() for how that affects
	// the rest of the frame.
	Clear()

	// Returns the width of the rune just added, in number of columns.
	//
	// Note that if you set a wide rune (like '午') in one column, then whatever
	// you put in the next column will be hidden by the wide rune. A wide rune
	// in the last screen column will be replaced by a space, to prevent it from
	// overflowing onto the next line.
	SetCell(column int, row int, styledRune StyledRune) int

	// Returns the StyledRune at the given screen position.
	//
	// Note that this does not read cells from the physical screen, but rather
	// from what was previously set using SetCell().
	//
	// For out-of-bounds requests, a space with default style is returned.
	GetCell(column int, row int) StyledRune

	// ShowCursor places the terminal's real cursor at the given screen
	// coordinate and makes it visible. Takes effect on the next Show() call.
	ShowCursor(column int, row int)

	// Ask the terminal to show a progress bar
	//
	// Ref: https://rockorager.dev/misc/osc-9-4-progress-bars/
	SetProgress(state ProgressState, percent int)

	// Render our contents into the terminal window.
	//
	// The first call takes over the terminal: alternate screen, cursor hidden,
	// mouse tracked. Until then the user's own screen is left alone.
	//
	// Renders nothing while somebody else owns the terminal, meaning after
	// Close() or during PauseAndCall(); Ctrl-Z handling makes that possible
	// without your main loop asking for it.
	Show()

	// Can be called after Close()ing the screen to fake retaining its output.
	// Plain Show() is what you'd call during normal operation.
	//
	// Unlike Show(), this one never takes over the terminal; it prints where
	// the cursor already is.
	PrintLines(lineCountToShow int)

	// Returns screen width and height.
	//
	// NOTE: Never cache this response! On window resizes you'll get an
	// EventResize on the Screen.Events channel. The new size takes effect the
	// next time you call Size() or Clear(), whichever comes first after your
	// last Show()/PrintLines() call, and stays consistent for the rest of that
	// frame.
	Size() (width int, height int)

	// The terminal's background color, as reported by the terminal during
	// NewScreen().
	//
	// nil if the terminal didn't report it.
	TerminalBackground() *Color

	// This channel is what your main loop should be checking.
	Events() chan Event

	// Pause the screen, run the given function, then resume the screen. Blocks
	// until the function has completed and the screen has been resumed again.
	//
	// Error returns mean that either pausing failed or the run function failed.
	// If resuming fails, this method will panic.
	PauseAndCall(run func() error) error
}

type lastRendered struct {
	width  int
	height int
	cells  [][]StyledRune
}

// terminalScreen is the real, terminal-backed implementation of Screen,
// created with NewScreen.
type terminalScreen struct {
	// Access only through freezeSizeForFrame()/applyPendingResize()
	widthAccessFromSizeOnly  int
	heightAccessFromSizeOnly int

	// Set by freezeSizeForFrame() once it's applied this frame's pending resize
	// (if any), cleared once Show()/PrintLines() is done rendering. Not guarded
	// by renderLock, see freezeSizeForFrame().
	inFrame bool

	// Queries the current terminal size. Defaults to term.GetSize, overridden
	// in tests so resize handling can be exercised without a real terminal.
	getSize func(fd int) (width int, height int, err error)

	// Protects both screen writes (through the ttyOut field) and lastRendered
	// updates
	renderLock sync.Mutex

	// Terminal query results and bookkeeping, guarded by terminalQueryLock
	terminalBackground      *Color
	terminalAlternateScroll alternateScrollSupport
	terminalQueryTime       time.Time // When we sent the terminal queries
	terminalQueriesDone     bool      // Set when done waiting for query responses
	terminalQueryLock       sync.Mutex

	cells        [][]StyledRune
	lastRendered lastRendered // Kept up to date by snapshotLastRendered()

	// Progress bar state, sent to the terminal on every render. Guarded by
	// renderLock.
	progress Progress

	// Cursor state, sent to the terminal on every render. Guarded by
	// renderLock.
	cursor cursorState

	// True while we are on the alternate screen. Guarded by renderLock.
	alternateScreenActive bool

	// Set by Close(), never cleared. Guarded by renderLock.
	closed bool

	// True while PauseAndCall() has handed the terminal to somebody else.
	// Guarded by renderLock.
	paused bool

	// Note that the type here doesn't matter, we only want to know whether or
	// not this channel has been signalled
	sigwinch chan int

	events chan Event

	ttyInReader interruptableReader

	ttyIn            *os.File
	oldTerminalState *term.State //nolint Not used on Windows
	oldTtyInMode     uint32      //nolint Windows only

	ttyOut        *os.File
	oldTtyOutMode uint32 //nolint Windows only

	terminalColorCount ColorCount
	mouseMode          MouseMode
}

// Example event: "\x1b[<65;127;41M"
//
// Where:
//   - "\x1b[<" says this is a mouse event
//   - "65" says this is Wheel Up. "64" would be Wheel Down.
//   - "127" is the column number on screen, "1" is the first column.
//   - "41" is the row number on screen, "1" is the first row.
//   - "M" marks the end of the mouse event.
var mouseEventRegex = regexp.MustCompile("^\x1b\\[<([0-9]+);([0-9]+);([0-9]+)M")

// Example response: "\x1b[12;40R", meaning the cursor is at row 12, column 40
var cursorPositionResponseRegex = regexp.MustCompile("^\x1b\\[[0-9]+;[0-9]+R$")

// Internal event, handled by processInput() and never posted: The terminal's
// response to the background color query
type eventTerminalBackground struct {
	color Color
}

// Internal event, handled by processInput() and never posted: The terminal's
// response to the cursor position query
type eventCursorPosition struct{}

// Example response: "\x1b[?1007;2$y", meaning Alternate Scroll Mode is
// supported but currently off.
//
// Ref: https://vt100.net/docs/vt510-rm/DECRPM.html
var alternateScrollResponseRegex = regexp.MustCompile("^\x1b\\[\\?1007;([0-9]+)\\$y$")

// Internal event, handled by processInput() and never posted: The terminal's
// response to the Alternate Scroll Mode query
type eventAlternateScroll struct {
	status string // As sent by the terminal, like "2"
}

// Whether the terminal supports Alternate Scroll Mode (1007), which makes the
// terminal send the mouse wheel as arrow keys while on the alternate screen
//
// Ref: https://github.com/walles/moor/issues/53#issuecomment-3392572761
type alternateScrollSupport int

const (
	// We don't know: no response before the cursor position response, or a
	// status we don't understand
	alternateScrollUnknown alternateScrollSupport = iota

	alternateScrollSupported
	alternateScrollUnsupported
)

// Ref: https://vt100.net/docs/vt510-rm/DECRPM.html
func alternateScrollSupportFromStatus(status string) alternateScrollSupport {
	switch status {
	case "1", "2", "3": // Set, reset, permanently set
		// Reset means supported but currently off. That's fine, we enable it
		// when entering the alternate screen.
		return alternateScrollSupported
	case "0": // Not recognized
		return alternateScrollUnsupported
	case "4": // Permanently reset, can't be enabled
		return alternateScrollUnsupported
	default:
		return alternateScrollUnknown
	}
}

func (support alternateScrollSupport) String() string {
	switch support {
	case alternateScrollUnknown:
		return "unknown"
	case alternateScrollSupported:
		return "supported"
	case alternateScrollUnsupported:
		return "unsupported"
	default:
		return fmt.Sprint("alternateScrollSupport(", int(support), ")")
	}
}

// NewScreen creates a new Screen according to options. Passing the zero value
// Options{} auto-detects mouse mode and terminal color count, and disables
// twin's own logging.
//
// The returned Screen requires Close() to be called after you are done with
// it, most likely somewhere in your shutdown code.
func NewScreen(options Options) (Screen, error) {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return nil, fmt.Errorf("stdout (fd=%d) must be a terminal for paging to work", os.Stdout.Fd())
	}

	terminalColorCount := options.TerminalColorCount
	if terminalColorCount == ColorCountDefault {
		terminalColorCount = ColorCount24bit
		if os.Getenv("COLORTERM") != "truecolor" && strings.Contains(os.Getenv("TERM"), "256") {
			// Covers "xterm-256color" as used by the macOS Terminal
			terminalColorCount = ColorCount256
		}
	}

	if options.Logger != nil {
		log = options.Logger
	} else {
		log = &noopLogger{}
	}

	screen := terminalScreen{
		terminalColorCount: terminalColorCount,
		mouseMode:          options.MouseMode,
		getSize:            term.GetSize,

		// Sized from manual testing on my MacBook: start
		// "./moor.sh sample-files/large-git-log-patch.txt", then do a two
		// finger flick initiating a momentum based scroll-up. If you get
		// "Events buffer full" warnings, the buffer is too small.
		//
		// Doubled from the smallest size that held up in that test, for
		// headroom: https://github.com/walles/moor/issues/164
		events: make(chan Event, 160),
	}

	screen.setupSigwinchNotification()
	err := screen.setupTtyInTtyOut()
	if err != nil {
		return nil, fmt.Errorf("problem setting up TTY: %w", err)
	}
	screen.ttyInReader = newInterruptableReader(screen.ttyIn)

	go func() {
		defer func() {
			panicHandler("NewScreen()/mainLoop()", recover(), debug.Stack())
		}()

		screen.mainLoop()
	}()

	// The responses will be handled in screen.mainLoop() that we just started ^.
	screen.queryTerminal()

	// NOTE: We deliberately do *not* enter the alternate screen here. That
	// happens on the first Show(), so that a moor run that never paints
	// anything leaves the terminal alone.

	return &screen, nil
}

// Ask the terminal for its background color and whether it supports Alternate
// Scroll Mode, and wait for the responses (or give up on them). mainLoop()
// must be running, it's what handles the responses.
func (screen *terminalScreen) queryTerminal() {
	// Note the query timestamp before asking, so that mainLoop() can never
	// observe a response that arrived before we recorded asking for it.
	start := time.Now()
	screen.terminalQueryLock.Lock()
	screen.terminalQueryTime = start
	screen.terminalQueryLock.Unlock()

	// Ref: https://stackoverflow.com/questions/2507337/how-to-determine-a-terminals-background-color
	const backgroundColorQuery = "\x1b]11;?\x07"

	// DECRQM query. This only asks, we enable the mode when entering the
	// alternate screen.
	//
	// Ref: https://vt100.net/docs/vt510-rm/DECRQM.html
	const alternateScrollQuery = "\x1b[?1007$p"

	// Terminals answer queries in order, and practically all terminals answer
	// the cursor position query. So we ask for the cursor position last, as an
	// "end-of-message" marker.
	//
	// Ref: https://github.com/walles/moor/issues/53#issuecomment-3392572761
	const cursorPositionQuery = "\x1b[6n"

	screen.renderLock.Lock()
	screen.writeLocked(backgroundColorQuery + alternateScrollQuery + cursorPositionQuery)
	screen.renderLock.Unlock()

	// Normally the wait ends as soon as the cursor position query is answered,
	// with or without other responses before it. This timeout is a backstop
	// for terminals that answer no queries at all, which we don't expect to
	// happen. Make it long enough to accommodate slow links.
	const maxWait = 500 * time.Millisecond

	// Wait for the responses (or give up on them) before returning. Callers
	// want the color for styling their first frame, and waiting for it here
	// means the wait happens while the user's terminal is still untouched.
	//
	// Waiting here also means the responses have been consumed before anybody
	// can Close() the screen. Otherwise they could be printed as text in the
	// user's terminal after exit.
	//
	// Refs:
	// * https://github.com/walles/moor/issues/380
	// * https://github.com/walles/moor/issues/425
	for {
		screen.terminalQueryLock.Lock()
		if screen.terminalQueriesDone {
			screen.terminalQueryLock.Unlock()
			return
		}

		if time.Since(start) > maxWait {
			log.Info(fmt.Sprint("No terminal query responses after ", maxWait, ", giving up"))
			screen.terminalQueriesDone = true
			screen.terminalQueryLock.Unlock()
			return
		}

		// Unlock so mainLoop() can handle the responses
		screen.terminalQueryLock.Unlock()

		// It's not more urgent than this
		time.Sleep(5 * time.Millisecond)
	}
}

func (screen *terminalScreen) Close() {
	// Tell the pager to exit unless it hasn't already
	screen.events <- EventExit{}

	// Tell our main loop to exit
	screen.ttyInReader.Interrupt()

	// Prevent ttyInReader from re-asserting our terminal mode after we restore
	// it below. The screen is going away, so we never unpause it.
	//
	// Pausing it blocks until any ongoing PauseAndCall() has unpaused it again,
	// which is what keeps us from closing the screen out from under a running
	// editor. It is also why the closed and paused flags can never both be set.
	screen.ttyInReader.SetPaused(true)

	screen.markClosedAndLeaveAlternateScreen()

	err := screen.restoreTtyInTtyOut()
	if err != nil {
		// Debug logging because this is expected to fail in some cases:
		// * https://github.com/walles/moor/issues/145
		// * https://github.com/walles/moor/issues/149
		// * https://github.com/walles/moor/issues/150
		log.Info(fmt.Sprint("Problem restoring TTY state: ", err))
	}
}

func (screen *terminalScreen) Events() chan Event {
	return screen.events
}

// Write string to ttyOut, panic on failure, return number of bytes written.
//
// You must hold renderLock when calling this method.
func (screen *terminalScreen) writeLocked(s string) int {
	reassertTtyOutMode(screen.ttyOut)

	bytesWritten, err := screen.ttyOut.Write([]byte(s))
	if err != nil {
		panic(err)
	}
	return bytesWritten
}

// You must hold renderLock when calling this method.
func (screen *terminalScreen) setAlternateScreenModeLocked(enable bool) {
	// Ref: https://stackoverflow.com/a/11024208/473672
	if enable {
		screen.writeLocked("\x1b[?1049h")

		// Enable alternateScroll mode. This makes the mouse wheel work without
		// blocking selection.
		//
		// Ref: https://github.com/walles/moor/issues/53#issuecomment-3392572761
		screen.writeLocked("\x1b[?1007h")
	} else {
		screen.writeLocked("\x1b[?1007l")
		screen.writeLocked("\x1b[?1049l")
	}
}

// Leave the alternate screen for good. Doing both under the same lock keeps a
// concurrent Show() (the signal handler closes us while the pager goroutine is
// still running) from putting us back on the alternate screen just as we exit.
func (screen *terminalScreen) markClosedAndLeaveAlternateScreen() {
	screen.renderLock.Lock()
	defer screen.renderLock.Unlock()

	screen.closed = true
	screen.leaveAlternateScreenSessionLocked()

	// Reset progress state so that calling PrintLines() after Close() won't
	// leave a progress bar on the screen.
	screen.progress = Progress{}

	// Restore terminal progress bar. See renderProgress() for details.
	screen.writeLocked(progressRemoveSequence)
}

// Does nothing if we're already on the alternate screen, or if the screen is
// closed or paused.
//
// You must hold renderLock when calling this method.
func (screen *terminalScreen) enterAlternateScreenSessionLocked() {
	if screen.alternateScreenActive || screen.closed || screen.paused {
		return
	}

	screen.setAlternateScreenModeLocked(true)
	screen.enableMouseTrackingLocked(screen.shouldEnableMouseTracking())

	screen.alternateScreenActive = true

	// Clear the render cache to force a full redraw. This is needed after
	// suspend/resume because the terminal's alternate screen buffer is blank
	// but our cache still has the old content.
	screen.lastRendered = lastRendered{}
}

// Does nothing if we aren't on the alternate screen. Sending ESC[?1049l
// unpaired is not harmless: it also restores a saved cursor position, so it can
// teleport the cursor.
//
// You must hold renderLock when calling this method.
func (screen *terminalScreen) leaveAlternateScreenSessionLocked() {
	if !screen.alternateScreenActive {
		return
	}

	screen.writeLocked("\x1b[m")
	screen.writeLocked(cursorShowSeq)
	screen.enableMouseTrackingLocked(false)
	screen.setAlternateScreenModeLocked(false)
	screen.alternateScreenActive = false
}

func (screen *terminalScreen) shouldEnableMouseTracking() bool {
	switch screen.mouseMode {
	case MouseModeAuto:
		return !terminalHasArrowKeysEmulation()
	case MouseModeSelect:
		return false
	case MouseModeScroll:
		return true
	default:
		panic(fmt.Errorf("unknown mouse mode: %d", screen.mouseMode))
	}
}

// Tell both screen.Size() and the client app that the window was resized
func (screen *terminalScreen) onWindowResized() {
	select {
	case screen.sigwinch <- 0:
		// Screen.Size() method notified about resize
	default:
		// Notification already pending, never mind
	}

	// Notify client app.
	select {
	case screen.events <- EventResize{}:
		// Event delivered
	default:
		// This likely means that the user isn't processing events
		// quickly enough. Maybe the user's queue will get flooded if
		// the window is resized too quickly?
		log.Info("Unable to deliver EventResize, event queue full")
	}
}

// Some terminals convert mouse events to key events making scrolling better
// without our built-in mouse support, and some do not.
//
// For those that do, we're better off without mouse tracking.
//
// To test your terminal, run with `moor --mousemode=mark` and see if mouse
// scrolling still works (both down and then back up to the top). If it does,
// add another check to this function!
//
// See also: https://github.com/walles/moor/issues/53
func terminalHasArrowKeysEmulation() bool {
	// Better off with mouse tracking:
	// * Terminal.app (macOS)
	// * Contour, thanks to @postsolar (GitHub username) for testing, 2023-12-18
	// * Foot, thanks to @postsolar (GitHub username) for testing, 2023-12-19

	// Hyper, tested on macOS, December 14th 2023
	if os.Getenv("TERM_PROGRAM") == "Hyper" {
		log.Info("Hyper terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Kitty, tested on macOS, December 14th 2023
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		log.Info("Kitty terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Alacritty, tested on macOS, December 14th 2023
	if os.Getenv("ALACRITTY_WINDOW_ID") != "" {
		log.Info("Alacritty terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Warp, tested on macOS, December 14th 2023
	if os.Getenv("TERM_PROGRAM") == "WarpTerminal" {
		log.Info("Warp terminal detected, assuming arrow keys emulation active")
		return true
	}

	// GNOME Terminal, tested on Ubuntu 22.04, December 16th 2023
	if os.Getenv("GNOME_TERMINAL_SCREEN") != "" {
		log.Info("GNOME Terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Tilix, tested on Ubuntu 22.04, December 16th 2023
	if os.Getenv("TILIX_ID") != "" {
		log.Info("Tilix terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Konsole, tested on Ubuntu 22.04, December 16th 2023
	if os.Getenv("KONSOLE_VERSION") != "" {
		log.Info("Konsole terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Terminator, tested on Ubuntu 22.04, December 16th 2023
	if os.Getenv("TERMINATOR_UUID") != "" {
		log.Info("Terminator terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Foot, tested on Ubuntu 22.04, December 16th 2023
	if os.Getenv("TERM") == "foot" || strings.HasPrefix(os.Getenv("TERM"), "foot-") {
		// Note that this test isn't very good, somebody could be running Foot
		// with some other TERM setting. Other suggestions welcome.
		log.Info("Foot terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Wezterm, tested on MacOS 12.6, January 3rd, 2024
	if os.Getenv("TERM_PROGRAM") == "WezTerm" {
		log.Info("Wezterm terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Rio, tested on macOS 14.3, January 27th, 2024
	if os.Getenv("TERM_PROGRAM") == "rio" {
		log.Info("Rio terminal detected, assuming arrow keys emulation active")
		return true
	}

	// VSCode 1.89.0, tested on macOS 14.4, May 6th, 2024
	if os.Getenv("TERM_PROGRAM") == "vscode" {
		log.Info("VSCode terminal detected, assuming arrow keys emulation active")
		return true
	}

	// IntelliJ IDEA CE 2023.2.2, tested on macOS 14.4, May 6th, 2024
	if os.Getenv("TERM_PROGRAM") == "JetBrains-JediTerm" {
		log.Info("IntelliJ IDEA terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Ghostty 1.0.1, tested on macOS 15.1.1, Jan 12th, 2025
	if os.Getenv("TERM_PROGRAM") == "ghostty" {
		log.Info("Ghostty terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Windows Terminal, tested here:
	// https://github.com/walles/moor/issues/53#issuecomment-3276404279
	if os.Getenv("WT_SESSION") != "" {
		log.Info("Windows Terminal detected, assuming arrow keys emulation active")
		return true
	}

	// iTerm2, supports alternateScroll mode, and therefore works with "select"
	if os.Getenv("TERM_PROGRAM") == "iTerm.app" {
		log.Info("iTerm2 terminal detected, gets arrow keys emulation through alternateScroll mode")
		return true
	}

	// In ssh sessions we can't detect the terminal, but most terminals support
	// "select" mode, especially now that we activate alternateScroll as well.
	// Go for select.
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_CLIENT") != "" || os.Getenv("SSH_TTY") != "" {
		log.Info("SSH session detected, assuming arrow keys emulation active since most terminals support it, especially with alternateScroll mode active")
		return true
	}

	log.Info("No known terminal with arrow keys emulation detected, assuming mouse tracking is needed")
	return false
}

// You must hold renderLock when calling this method.
func (screen *terminalScreen) enableMouseTrackingLocked(enable bool) {
	if enable {
		screen.writeLocked("\x1b[?1006;1000h")
	} else {
		screen.writeLocked("\x1b[?1006;1000l")
	}
}

func (screen *terminalScreen) mainLoop() {
	// "1400" comes from me trying fling scroll operations on my MacBook
	// trackpad and looking at the high watermark (logged below).
	//
	// The highest I saw when I tried this was 700 something. 1400 is twice
	// that, so 1400 should be good.
	buffer := make([]byte, 1400)

	log.Info("Entering Twin main loop...")

	maxBytesRead := 0
	incomplete := "" // Escape sequence split across reads
	for {
		count, err := screen.ttyInReader.Read(buffer)
		if err != nil {
			// Ref:
			// * https://github.com/walles/moor/issues/145
			// * https://github.com/walles/moor/issues/149
			// * https://github.com/walles/moor/issues/150
			log.Info(fmt.Sprint("ttyin read error, twin giving up: ", err))

			screen.events <- EventExit{}
			return
		}

		if count > maxBytesRead {
			maxBytesRead = count
			log.Debug(fmt.Sprint("ttyin high watermark bumped to ", maxBytesRead, " bytes"))
		}

		incomplete = screen.processInput(incomplete + string(buffer[0:count]))
	}
}

// Parse input into events and post them.
//
// Returns any incomplete escape sequence at the end of input. It should be
// passed back in again together with the next input.
func (screen *terminalScreen) processInput(input string) (incomplete string) {
	if !utf8.ValidString(input) {
		log.Info(fmt.Sprint("Got invalid UTF-8 sequence on ttyin: ", input))
		return ""
	}

	for len(input) > 0 {
		var event *Event
		var isIncomplete bool
		event, input, isIncomplete = consumeEncodedEvent(input)
		if isIncomplete {
			// Should be completed by the next input
			return input
		}

		if event == nil {
			// Nothing to report, but there may be more after whatever was
			// just consumed
			continue
		}

		if background, ok := (*event).(eventTerminalBackground); ok {
			screen.terminalQueryLock.Lock()
			if screen.terminalQueriesDone {
				log.Info(fmt.Sprint("Got the terminal background color ", background.color, " after we stopped waiting for it, ignoring"))
			} else {
				screen.terminalBackground = &background.color
				log.Debug(fmt.Sprint("Terminal background color detected as ", background.color, " after ", time.Since(screen.terminalQueryTime)))
			}
			screen.terminalQueryLock.Unlock()
			continue
		}

		if alternateScroll, ok := (*event).(eventAlternateScroll); ok {
			support := alternateScrollSupportFromStatus(alternateScroll.status)
			screen.terminalQueryLock.Lock()
			if screen.terminalQueriesDone {
				log.Info(fmt.Sprint("Got Alternate Scroll Mode status ", alternateScroll.status, " (", support, ") after we stopped waiting for it, ignoring"))
			} else {
				screen.terminalAlternateScroll = support
				log.Debug(fmt.Sprint("Alternate Scroll Mode status ", alternateScroll.status, " (", support, ") after ", time.Since(screen.terminalQueryTime)))
			}
			screen.terminalQueryLock.Unlock()
			continue
		}

		if _, ok := (*event).(eventCursorPosition); ok {
			screen.terminalQueryLock.Lock()
			if !screen.terminalQueriesDone {
				log.Debug(fmt.Sprint("Got the cursor position response after ", time.Since(screen.terminalQueryTime), ", done waiting for terminal query responses"))
				screen.terminalQueriesDone = true
			}
			screen.terminalQueryLock.Unlock()
			continue
		}

		// Intercept Ctrl-Z and handle suspend/resume automatically
		if runeEvent, ok := (*event).(EventRune); ok {
			if runeEvent.Rune == '\x1a' {
				log.Info("Twin: Ctrl-Z detected, suspending...")

				err := screen.suspend()
				if err != nil {
					log.Info(fmt.Sprint("Twin: Suspend failed: ", err))
					continue
				}

				log.Info("Twin: Resumed from suspend")

				continue
			}
		}

		// Post the event
		select {
		case screen.events <- *event:
			// Yay
		default:
			// If this happens, consider increasing the channel size in
			// NewScreen()
			log.Info(fmt.Sprintf("Events buffer (size %d) full, events are being dropped", cap(screen.events)))
		}
	}

	return ""
}

// Turn ESC into <0x1b> and other low ASCII characters into <0xXX> for logging
// purposes.
func humanizeLowASCII(withLowAsciis string) string {
	humanized := ""
	for _, char := range withLowAsciis {
		if char < ' ' {
			humanized += fmt.Sprintf("<0x%2x>", char)
			continue
		}
		humanized += string(char)
	}
	return humanized
}

// How long an incomplete escape sequence can get before we give up on it.
//
// The longest sequence we expect is a background color response terminated by
// ST, like "\x1b]11;rgb:2828/2828/2828\x1b\\", at 25 bytes. This leaves room
// for longer variants.
const maxIncompleteSequenceLength = 64

// Consume initial key code from the sequence of encoded keycodes.
//
// Returns a (possibly nil) event that should be posted, the remainder of the
// encoded events sequence, and whether the sequence starts with an incomplete
// escape sequence. If it does, nothing is consumed.
func consumeEncodedEvent(encodedEventSequences string) (*Event, string, bool) {
	for singleKeyCodeSequence, keyCode := range escapeSequenceToKeyCode {
		if !strings.HasPrefix(encodedEventSequences, singleKeyCodeSequence) {
			continue
		}

		// Encoded key code sequence found, report it!
		var event Event = EventKeyCode{keyCode}
		return &event, strings.TrimPrefix(encodedEventSequences, singleKeyCodeSequence), false
	}

	mouseMatch := mouseEventRegex.FindStringSubmatch(encodedEventSequences)
	if mouseMatch != nil {
		if mouseMatch[1] == "64" {
			var event Event = EventMouse{Buttons: MouseWheelUp}
			return &event, strings.TrimPrefix(encodedEventSequences, mouseMatch[0]), false
		}
		if mouseMatch[1] == "65" {
			var event Event = EventMouse{Buttons: MouseWheelDown}
			return &event, strings.TrimPrefix(encodedEventSequences, mouseMatch[0]), false
		}

		log.Debug(fmt.Sprint(
			"Unhandled mouse escape sequence: {",
			humanizeLowASCII(mouseMatch[0]),
			"}"))
		return nil, strings.TrimPrefix(encodedEventSequences, mouseMatch[0]), false
	}

	//
	// No escape sequence prefix matched
	//

	runes := []rune(encodedEventSequences)
	if len(runes) == 0 {
		return nil, "", false
	}

	if runes[0] == '\r' {
		var event Event = EventKeyCode{KeyEnter}
		return &event, string(runes[1:]), false
	}

	if runes[0] != '\x1b' {
		// Report the single rune
		var event Event = EventRune{Rune: runes[0]}
		return &event, string(runes[1:]), false
	}

	//
	// The first rune is ESC, but no known escape sequence matched. It could be
	// the start of a longer escape sequence, or it could be a lone ESC keypress.
	//

	byteLength, incomplete := escapeSequenceByteLength(encodedEventSequences)
	if incomplete {
		if len(encodedEventSequences) > maxIncompleteSequenceLength {
			// Probably not a sequence at all, but something like Alt-]
			// followed by typing. Don't hold on to user input forever.
			log.Info(fmt.Sprint(
				"Giving up on incomplete escape sequence after ",
				len(encodedEventSequences),
				" bytes, dropping it: {",
				humanizeLowASCII(encodedEventSequences),
				"}"))
			return nil, "", false
		}

		// Should be completed by more input
		return nil, encodedEventSequences, true
	}

	if byteLength > 0 {
		sequence := encodedEventSequences[:byteLength]

		if background := parseTerminalBgColorResponse(sequence); background != nil {
			var event Event = eventTerminalBackground{color: *background}
			return &event, encodedEventSequences[byteLength:], false
		}

		if match := alternateScrollResponseRegex.FindStringSubmatch(sequence); match != nil {
			var event Event = eventAlternateScroll{status: match[1]}
			return &event, encodedEventSequences[byteLength:], false
		}

		if cursorPositionResponseRegex.MatchString(sequence) {
			var event Event = eventCursorPosition{}
			return &event, encodedEventSequences[byteLength:], false
		}

		// Drop only this sequence, there could be more events after it.
		//
		// If this is a keypress, it should be added to escapeSequenceToKeyCode
		// in keys.go.
		log.Debug(fmt.Sprint(
			"Unhandled terminal escape sequence: {",
			humanizeLowASCII(sequence),
			"}"))
		return nil, encodedEventSequences[byteLength:], false
	}

	if encodedEventSequences == "\x1b" {
		var event Event = EventKeyCode{KeyEscape}
		return &event, "", false
	}

	// This means one or more sequences should be added to
	// escapeSequenceToKeyCode in keys.go.
	log.Debug(fmt.Sprint(
		"Unhandled multi character terminal escape sequence(s): {",
		humanizeLowASCII(encodedEventSequences),
		"}"))

	// Mark everything as consumed since we don't know how to proceed otherwise.
	return nil, "", false
}

// Returns the length in bytes of the CSI or OSC escape sequence at the start of
// s, or 0 if s doesn't start with one.
//
// If s starts with an incomplete CSI or OSC sequence, incomplete will be true.
//
// Ref: https://en.wikipedia.org/wiki/ANSI_escape_code#Fe_Escape_sequences
func escapeSequenceByteLength(s string) (byteLength int, incomplete bool) {
	if strings.HasPrefix(s, "\x1b[") {
		// CSI: Parameter and intermediate bytes, then a final byte
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				// Final byte, '@' to '~'
				return i + 1, false
			}
			if s[i] < 0x20 || s[i] > 0x3f {
				// Not a valid parameter or intermediate byte, ' ' (space) to '?'
				return 0, false
			}
		}
		return 0, true
	}

	if strings.HasPrefix(s, "\x1b]") {
		// OSC: A string terminated by either BEL or ST (ESC + backslash)
		for i := 2; i < len(s); i++ {
			if s[i] == '\x07' {
				return i + 1, false
			}
			if s[i] == '\x1b' {
				if i+1 == len(s) {
					// Possibly the start of an ST
					return 0, true
				}
				if s[i+1] == '\\' {
					return i + 2, false
				}
				return 0, false
			}
			if s[i] < 0x20 {
				// Control characters can't be part of an OSC sequence, so
				// the sequence ends right before this one. This way a Ctrl-C
				// after Alt-] ("\x1b]") still comes through.
				return i, false
			}
		}
		return 0, true
	}

	return 0, false
}

func (screen *terminalScreen) Size() (width int, height int) {
	screen.freezeSizeForFrame()

	if screen.widthAccessFromSizeOnly == 0 || screen.heightAccessFromSizeOnly == 0 {
		panic(fmt.Sprintf("No screen size available, this is a bug: %d x %d",
			screen.widthAccessFromSizeOnly,
			screen.heightAccessFromSizeOnly))
	}
	return screen.widthAccessFromSizeOnly, screen.heightAccessFromSizeOnly
}

// freezeSizeForFrame is the single gate a pending resize goes through: the
// first call after the previous frame's Show()/PrintLines() completed applies
// the resize and locks in screen.inFrame; every call after that within the same
// frame sees the same dimensions instead. This is what keeps a frame internally
// consistent instead of tearing between two different sizes mid rendering.
//
// screen.inFrame is intentionally not guarded by renderLock: it's only ever
// touched by whichever single goroutine drives the screen (calls Size(),
// Clear(), Show()...), same as screen.cells itself. The SIGWINCH-handling
// goroutine (see onWindowResized()) never touches either.
func (screen *terminalScreen) freezeSizeForFrame() {
	if screen.inFrame {
		return
	}

	screen.applyPendingResize()
	screen.inFrame = true
}

// applyPendingResize applies a terminal resize detected since the last call (if
// any), reallocating screen.cells to the new dimensions.
//
// Only call this through freezeSizeForFrame(): calling it directly while
// screen.inFrame is true would swap out the cell buffer mid-frame, while some
// of the frame's content has already been drawn and some hasn't, tearing that
// frame's next Show().
func (screen *terminalScreen) applyPendingResize() {
	select {
	case <-screen.sigwinch:
		// Resize logic needed, see below
	default:
		return // No resize pending
	}

	// Window was resized
	width, height, err := screen.getSize(int(screen.ttyOut.Fd()))
	if err != nil {
		panic(err)
	}

	if width == 0 || height == 0 {
		panic(fmt.Sprintf("Got zero screen size: %d x %d", width, height))
	}

	if screen.widthAccessFromSizeOnly == width && screen.heightAccessFromSizeOnly == height {
		// Not sure when this would happen, but if it does this wasn't really a
		// resize, and we don't need to treat it as such.
		return
	}

	oldHeight := screen.heightAccessFromSizeOnly
	if (height != oldHeight) && (oldHeight <= 1 || height <= 1) {
		// For help debugging this: https://github.com/walles/moor/issues/378
		//
		// A one-high screen may or may not have been part of that issue.
		log.Info(fmt.Sprintf("Screen height changed from %d to %d", oldHeight, height))
	}

	newCells := make([][]StyledRune, height)
	for rowNumber := range height {
		newCells[rowNumber] = make([]StyledRune, width)
	}
	clearCells(newCells)

	screen.widthAccessFromSizeOnly = width
	screen.heightAccessFromSizeOnly = height
	screen.cells = newCells
}

// Nobody can call this until they got their Screen from NewScreen(). And
// NewScreen() waits for background color to be populated. So this method only
// has to take the lock and return the value.
func (screen *terminalScreen) TerminalBackground() *Color {
	screen.terminalQueryLock.Lock()
	defer screen.terminalQueryLock.Unlock()
	return screen.terminalBackground
}

// Parses a complete terminal background color response, like
// "\x1b]11;rgb:1212/3434/5656\x07". Returns nil if sequence isn't one.
func parseTerminalBgColorResponse(sequence string) *Color {
	prefix := "\x1b]11;rgb:"
	if !strings.HasPrefix(sequence, prefix) {
		return nil
	}
	rgb := strings.TrimPrefix(sequence, prefix)
	rgb = strings.TrimSuffix(rgb, "\x07")
	rgb = strings.TrimSuffix(rgb, "\x1b\\")

	if len(rgb) != 14 {
		log.Info(fmt.Sprint("Got unexpected length bg color response from terminal: <", humanizeLowASCII(sequence), ">"))
		return nil
	}

	// rgb is now "RRRR/GGGG/BBBB"
	red, err := strconv.ParseUint(rgb[0:4], 16, 16)
	if err != nil {
		log.Info(fmt.Sprint("Failed parsing red in bg color response from terminal: <", humanizeLowASCII(sequence), ">: ", err))
		return nil
	}

	green, err := strconv.ParseUint(rgb[5:9], 16, 16)
	if err != nil {
		log.Info(fmt.Sprint("Failed parsing green in bg color response from terminal: <", humanizeLowASCII(sequence), ">: ", err))
		return nil
	}

	blue, err := strconv.ParseUint(rgb[10:14], 16, 16)
	if err != nil {
		log.Info(fmt.Sprint("Failed parsing blue in bg color response from terminal: <", humanizeLowASCII(sequence), ">: ", err))
		return nil
	}

	color := NewColor24Bit(uint8(red/256), uint8(green/256), uint8(blue/256))

	return &color
}

func (screen *terminalScreen) SetCell(column int, row int, styledRune StyledRune) int {
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

	runeWidth := styledRune.Width()
	if runeWidth == 0 {
		// This happens for unprintable runes. But we were asked to set
		// something in this screen cell, so unprintable or not this will count
		// as one cell wide.
		return 1
	}
	return runeWidth
}

func (screen *terminalScreen) GetCell(column int, row int) StyledRune {
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

func (screen *terminalScreen) Clear() {
	// See freezeSizeForFrame(): this only actually applies a pending resize if
	// nothing else already did earlier in this frame.
	screen.freezeSizeForFrame()

	clearCells(screen.cells)

	// ShowCursor() can be called from other goroutines (same as SetProgress()
	// and screen.progress), so this specific write needs renderLock.
	screen.renderLock.Lock()
	screen.cursor = cursorState{}
	screen.renderLock.Unlock()
}

// clearCells fills cells with spaces in the default style.
func clearCells(cells [][]StyledRune) {
	empty := StyledRune{Rune: ' ', Style: StyleDefault}

	for _, row := range cells {
		for column := range row {
			row[column] = empty
		}
	}
}

// A cell is considered hidden if it's preceded by a wide character that spans
// multiple columns.
func withoutHiddenRunes(runes []StyledRune) []StyledRune {
	result := make([]StyledRune, 0, len(runes))

	for i := range runes {
		if i > 0 && runes[i-1].Width() == 2 {
			// This is a hidden rune
			continue
		}

		result = append(result, runes[i])
	}

	return result
}

// Returns the rendered line, plus how many information carrying cells went into
// it. The width is used to decide whether or not to clear to EOL at the end of
// the line.
func renderLine(row []StyledRune, width int, terminalColorCount ColorCount) (string, int) {
	row = withoutHiddenRunes(row)

	// Strip trailing whitespace
	trailerBg := ColorDefault
	trailerBgSet := false
	lastSignificantCellIndex := len(row) - 1
	for ; lastSignificantCellIndex >= 0; lastSignificantCellIndex-- {
		lastCell := row[lastSignificantCellIndex]
		if lastCell.Rune != ' ' {
			break
		}

		whiteSpaceBg := lastCell.Style.bg
		if lastCell.Style.attrs.has(AttrReverse) {
			// Style is inverted, take the foreground color instead
			if lastCell.Style.fg == ColorDefault {
				// We don't know what the default color is in this case, so we
				// can't use it.
				break
			}

			whiteSpaceBg = lastCell.Style.fg
		}

		if !trailerBgSet {
			trailerBg = whiteSpaceBg
			trailerBgSet = true
		}

		if whiteSpaceBg != trailerBg {
			break
		}
	}
	row = row[0 : lastSignificantCellIndex+1]

	var builder strings.Builder

	// Set initial line style to normal
	builder.WriteString("\x1b[m")
	lastStyle := StyleDefault

	for _, cell := range row {
		style := cell.Style
		runeToWrite := cell.Rune
		if !Printable(runeToWrite) {
			// Highlight unprintable runes
			style = Style{
				fg:    NewColor16(7), // White
				bg:    NewColor16(1), // Red
				attrs: AttrBold,
			}
			runeToWrite = '?'
		}

		if style != lastStyle {
			builder.WriteString(style.RenderUpdateFrom(lastStyle, terminalColorCount))
			lastStyle = style
		}

		builder.WriteRune(runeToWrite)
	}

	lastStyleMinusHyperlink := lastStyle.WithHyperlink(nil)
	if lastStyleMinusHyperlink != lastStyle {
		// Remove the hyperlink attribute
		builder.WriteString(lastStyleMinusHyperlink.RenderUpdateFrom(lastStyle, terminalColorCount))
		lastStyle = lastStyleMinusHyperlink
	}

	if len(row) < width {
		// Clear to end of line
		// https://en.wikipedia.org/wiki/ANSI_escape_code#CSI_(Control_Sequence_Introducer)_sequences
		//
		// Note that we can't do this if we're one the last screen column:
		// https://github.com/microsoft/terminal/issues/18115#issuecomment-2448054645
		builder.WriteString(StyleDefault.WithBackground(trailerBg).RenderUpdateFrom(lastStyle, terminalColorCount))
		builder.WriteString("\x1b[K")
	}

	return builder.String(), len(row)
}

// Render our contents onto the alternate screen. We switch there first if we
// aren't already, and render nothing at all if we can't, because overwriting
// the user's normal screen would destroy contents we have no way of restoring.
// Owning the screen this way is also what makes the render cache trustworthy
// enough to update only the lines that changed.
func (screen *terminalScreen) Show() {
	width, height := screen.Size()

	screen.renderLock.Lock()
	defer screen.renderLock.Unlock()

	// The frame ends here, see freezeSizeForFrame().
	defer func() { screen.inFrame = false }()

	// Note that entering drops the render cache, so the delta check below
	// correctly falls through to a full render the first time around.
	screen.enterAlternateScreenSessionLocked()

	if !screen.alternateScreenActive {
		// Somebody else owns the terminal right now: we're either closed,
		// or paused for an editor or for the shell after Ctrl-Z
		return
	}

	screen.writeLocked(screen.renderProgressLocked())

	if screen.showNLinesDeltaLocked(width, height) {
		return
	}

	var builder strings.Builder

	// Start in the top left corner:
	// https://en.wikipedia.org/wiki/ANSI_escape_code#CSI_(Control_Sequence_Introducer)_sequences
	builder.WriteString("\x1b[1;1H")

	for row := range height {
		renderWithNewline(&builder, screen.cells[row], width, screen.terminalColorCount, row == (height-1))
	}

	// This must come last, after all cell content: otherwise the
	// content writes above would reposition the terminal's real cursor
	// after we placed it here.
	builder.WriteString(screen.renderCursorLocked(width, height))

	// Write out what we have
	screen.writeLocked(builder.String())
	screen.snapshotLastRenderedLocked()
}

// Print the topmost height lines of our cells wherever the cursor happens to
// be, like any other command line tool would. This is how ReprintAfterExit()
// leaves moor's output behind on the user's own screen.
func (screen *terminalScreen) PrintLines(height int) {
	width, _ := screen.Size()

	screen.renderLock.Lock()
	defer screen.renderLock.Unlock()

	// The frame ends here, see freezeSizeForFrame().
	defer func() { screen.inFrame = false }()

	screen.writeLocked(screen.renderProgressLocked())

	var builder strings.Builder

	for row := range height {
		renderWithNewline(&builder, screen.cells[row], width, screen.terminalColorCount, row == (height-1))
	}

	// The last line can end with styling still set. Reset styling here to
	// not mess up whatever will come after it, like a shell prompt.
	builder.WriteString("\x1b[m")

	// Write out what we have
	screen.writeLocked(builder.String())
}

// Take a snapshot of the current screen. Will be used on the next render to
// decide whether to do a full render or a delta render.
//
// You must hold renderLock when calling this method.
func (screen *terminalScreen) snapshotLastRenderedLocked() {
	height := len(screen.cells)
	width := 0
	if height > 0 {
		width = len(screen.cells[0])
	}

	if screen.lastRendered.width != width || screen.lastRendered.height != height {
		// Create a new snapshot storage
		screen.lastRendered = lastRendered{
			width:  width,
			height: height,
		}

		if width > 0 && height > 0 {
			screen.lastRendered.cells = make([][]StyledRune, height)
			for row := range height {
				screen.lastRendered.cells[row] = make([]StyledRune, width)
			}
		}
	}

	if screen.lastRendered.width == 0 || screen.lastRendered.height == 0 {
		screen.lastRendered.cells = nil
	}

	if screen.lastRendered.cells == nil {
		// Nowhere to copy to
		return
	}

	// Copy current cells into snapshot
	for row := range height {
		copy(screen.lastRendered.cells[row], screen.cells[row][0:width])
	}
}

// Map updated lines
//
// You must hold renderLock when calling this method.
func (screen *terminalScreen) findUpdatedLinesLocked() map[int][]StyledRune {
	height := len(screen.cells)
	updatedLines := make(map[int][]StyledRune, height)
	for row := range height {
		newLine := screen.cells[row]
		cachedLine := screen.lastRendered.cells[row]

		if len(newLine) != len(cachedLine) {
			updatedLines[row] = newLine
			continue
		}

		for col := range newLine {
			if !newLine[col].Equal(cachedLine[col]) {
				updatedLines[row] = newLine
				break
			}
		}
	}

	return updatedLines
}

// Renders a single line and appends a newline if needed (except for last line)
func renderWithNewline(builder *strings.Builder, line []StyledRune, width int, terminalColorCount ColorCount, isLastLine bool) {
	rendered, lineLength := renderLine(line, width, terminalColorCount)
	builder.WriteString(rendered)

	// NOTE: This <= should *really* be <= and nothing else. Otherwise, if
	// one line precisely as long as the terminal window goes before one
	// empty line, the empty line will never be rendered.
	//
	// Can be demonstrated using "moor m/pager.go", scroll right once to
	// make the line numbers go away, then make the window narrower until
	// some line before an empty line is just as wide as the window.
	//
	// With the wrong comparison here, then the empty line just disappears.
	if lineLength <= len(line) && !isLastLine {
		builder.WriteString("\r\n")
	}
}

// If only a few lines changed, update just those lines.
//
// You must hold renderLock when calling this method. Only called from
// Show(): it unconditionally renders cursor state, which must never happen
// when printing plain lines onto the user's own screen.
//
// Returns true if delta rendering was done, false if a full render is needed.
func (screen *terminalScreen) showNLinesDeltaLocked(width int, height int) bool {
	if screen.lastRendered.width != width || screen.lastRendered.height != height {
		return false
	}

	// Map from line number to line contents
	updatedLines := screen.findUpdatedLinesLocked()

	// We have two spinners, those two should be able to spin without updating
	// the whole screen.
	if len(updatedLines) > 2 {
		// Nah, do the full render
		return false
	}

	var builder strings.Builder
	for row, line := range updatedLines {
		// Move cursor to the start of the line
		fmt.Fprintf(&builder, "\x1b[%d;1H", row+1)

		renderWithNewline(&builder, line, width, screen.terminalColorCount, row == (height-1))
	}

	// This must come last, after all cell content: otherwise the content
	// writes above would reposition the terminal's real cursor after we
	// placed it here.
	builder.WriteString(screen.renderCursorLocked(width, height))

	// Write out what we have
	screen.writeLocked(builder.String())
	screen.snapshotLastRenderedLocked()

	return true
}

func (screen *terminalScreen) PauseAndCall(run func() error) error {
	screen.ttyInReader.SetPaused(true)
	defer screen.ttyInReader.SetPaused(false)

	// The paused flag keeps a concurrent Show() from grabbing the terminal back
	// from whoever we're handing it to. Reading wasActive under the same lock
	// as the leave is what makes "restore what we had" trustworthy.
	screen.renderLock.Lock()
	wasActive := screen.alternateScreenActive
	screen.leaveAlternateScreenSessionLocked()

	// Drop any progress bar while paused. Will be restored as a side effect of
	// screen.onWindowResized() below.
	screen.writeLocked(progressRemoveSequence)

	screen.paused = true
	screen.renderLock.Unlock()

	// Covers the error and panic paths below. The happy path can't rely on this
	// one: it runs after the re-enter, which bails out while we're paused.
	defer func() {
		screen.renderLock.Lock()
		screen.paused = false
		screen.renderLock.Unlock()
	}()

	err := screen.restoreTtyInTtyOut()
	if err != nil {
		return fmt.Errorf("failed to restore terminal state before pause: %w", err)
	}

	runErr := run()

	restoreRawErr := screen.restoreRawModeAfterResume()
	if restoreRawErr != nil {
		panic(fmt.Errorf("failed to resume screen after paused operation (%v): %w", runErr, restoreRawErr))
	}

	screen.renderLock.Lock()
	screen.paused = false
	if wasActive {
		screen.enterAlternateScreenSessionLocked()
	}
	screen.renderLock.Unlock()

	screen.onWindowResized()

	if runErr != nil {
		return runErr
	}

	return nil
}

func (screen *terminalScreen) restoreRawModeAfterResume() error {
	terminalState, err := term.MakeRaw(int(screen.ttyIn.Fd()))
	if err != nil {
		return fmt.Errorf("failed to re-enter raw mode after suspend: %w", err)
	}

	screen.oldTerminalState = terminalState
	return nil
}
