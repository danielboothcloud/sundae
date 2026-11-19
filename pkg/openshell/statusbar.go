package openshell

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"charm.land/ssh"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
)

// minBarRows is the smallest terminal that gets the status bar: smaller
// ones keep every row for the program.
const minBarRows = 5

// barEvery bounds how often program output redraws the status bar.
const barEvery = 100 * time.Millisecond

// modesReset undoes modes a program left set when the user leaves it; kitty
// keyboard is popped first as its stack is per screen, before leaving the alt screen.
const modesReset = "\x1b[<u\x1b[>4m\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1004l\x1b[?2004l\x1b[0m\x1b[?25h\x1b[?1049l"

// StatusBarFits reports whether a terminal rows high gets the status bar.
func StatusBarFits(rows int) bool { return rows >= minBarRows }

// RemoteSize is the program's PTY size on a terminal of window w: a row
// less when bar is on and the terminal fits it.
func RemoteSize(w ssh.Window, bar bool) (cols, rows uint32) {
	h := w.Height
	if bar && StatusBarFits(h) {
		h--
	}
	return uint32(max(w.Width, 0)), uint32(max(h, 0)) //nolint:gosec
}

// statusBar is the attached terminal's output: it passes the program's output
// through and keeps a one-line status bar on the terminal's bottom row, which
// the program's PTY leaves out (RemoteSize). Without the bar it only
// serialises writes and the reset on leaving.
// NOTE: DECSC has one slot, so a program relying on its own saved cursor
// across a redraw can see it moved.
type statusBar struct {
	mu sync.Mutex
	w  io.Writer
	// enabled: the bar is wanted; shown: the terminal fits it now.
	enabled, shown bool
	// drawn reports whether the bar was ever drawn, so leaving clears it.
	drawn       bool
	left, right string
	cols, rows  int
	// top is the scroll region's top row the program asked for.
	top    int
	parser *ansi.Parser
	// dirty: output since the last draw; urgent: draw at the next safe
	// point; region: set the scroll region with it.
	dirty, urgent, region bool
	last                  time.Time
	stop                  func() bool
	muted                 bool

	every time.Duration
	now   func() time.Time
	after func(time.Duration, func()) (stop func() bool)
}

// newStatusBar returns the output to w, a terminal of window win; enabled
// shows the bar with label, and the home key on its right.
func newStatusBar(w io.Writer, win ssh.Window, enabled bool, label string, home HomeKey) *statusBar {
	b := &statusBar{
		w:       w,
		enabled: enabled,
		left:    " " + label + " ",
		top:     1,
		every:   barEvery,
		now:     time.Now,
		after: func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, f).Stop
		},
	}
	if home.Enabled() {
		b.right = " " + home.String() + " sessions "
	}
	b.setSize(win)
	if enabled {
		b.parser = ansi.NewParser()
		b.parser.SetDataSize(1)
		b.parser.SetHandler(ansi.Handler{HandleCsi: b.onCsi, HandleEsc: b.onEsc})
	}
	return b
}

func (b *statusBar) setSize(win ssh.Window) {
	b.cols, b.rows = win.Width, win.Height
	b.shown = b.enabled && StatusBarFits(b.rows)
}

// start draws the bar and sets the scroll region above it.
func (b *statusBar) start() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.shown {
		return
	}
	// NOTE: LF then CUU frees the bottom row, scrolling once if the cursor is on it.
	b.writeFrame("\n\x1b[A", true)
}

// Write passes the program's output on, then redraws the bar if the output
// may have erased it.
func (b *statusBar) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.muted {
		return len(p), nil
	}
	n, err := b.w.Write(p)
	if !b.enabled {
		return n, err //nolint:wrapcheck
	}
	for _, c := range p[:n] {
		b.parser.Advance(c)
	}
	b.dirty = true
	b.flush()
	return n, err //nolint:wrapcheck
}

// resize records the terminal's new window and redraws the bar there; it
// returns the program's new PTY size.
func (b *statusBar) resize(win ssh.Window) (cols, rows uint32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	was := b.shown
	b.setSize(win)
	switch {
	case b.muted:
	case b.shown:
		// Terminals reset the scroll region on resize.
		b.urgent, b.region = true, true
		b.flush()
	case was && b.safe():
		_, _ = io.WriteString(b.w, "\x1b7\x1b[r\x1b8")
	}
	return RemoteSize(win, b.enabled)
}

// leave resets the terminal for whatever follows the attach, and drops the
// program's output from then on.
func (b *statusBar) leave() { b.end(modesReset) }

// close takes the bar down once the program's output has ended.
func (b *statusBar) close() { b.end("") }

func (b *statusBar) end(reset string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.muted {
		return
	}
	b.muted = true
	if b.stop != nil {
		b.stop()
		b.stop = nil
	}
	if b.drawn || reset != "" {
		_, _ = io.WriteString(b.w, reset+b.teardown())
	}
}

// teardown resets the scroll region and clears the bar's row, keeping the
// cursor where it is, then starts a new line.
func (b *statusBar) teardown() string {
	clear := ""
	if b.drawn {
		clear = fmt.Sprintf("\x1b[%d;1H\x1b[2K", b.rows)
	}
	return "\x1b7\x1b[r" + clear + "\x1b8\r\n"
}

// safe reports whether the output so far ends between complete escape
// sequences and characters, where the bar can be drawn.
func (b *statusBar) safe() bool {
	return b.parser == nil || b.parser.State() == parser.GroundState
}

// flush draws the bar if it is due and safe; otherwise a timer draws it
// once output pauses. Call with mu held.
func (b *statusBar) flush() {
	if !b.shown || !(b.dirty || b.urgent) || !b.safe() {
		// Unsafe: the output that completes the sequence flushes again.
		return
	}
	if wait := b.every - b.now().Sub(b.last); !b.urgent && wait > 0 {
		if b.stop == nil {
			b.stop = b.after(wait, b.tick)
		}
		return
	}
	b.writeFrame("", b.region)
}

func (b *statusBar) tick() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stop = nil
	if !b.muted {
		b.flush()
	}
}

// writeFrame writes before, then the bar, and the scroll region with
// region, all between DECSC and DECRC. Call with mu held.
func (b *statusBar) writeFrame(before string, region bool) {
	var s strings.Builder
	s.WriteString(before)
	// Origin mode off and ASCII G0, so the bar lands on the last row legibly; DECRC restores both.
	s.WriteString("\x1b7\x1b[?6l\x1b(B")
	if region {
		top := b.top
		if top >= b.rows-1 {
			top = 1
		}
		fmt.Fprintf(&s, "\x1b[%d;%dr", top, b.rows-1)
	}
	fmt.Fprintf(&s, "\x1b[%d;1H\x1b[2K\x1b[0;7m%s\x1b[0m\x1b8", b.rows, barText(b.left, b.right, b.cols))
	_, _ = io.WriteString(b.w, s.String())
	b.drawn = true
	b.dirty, b.urgent, b.region = false, false, false
	b.last = b.now()
	if b.stop != nil {
		b.stop()
		b.stop = nil
	}
}

// onCsi notes the sequences that clear the screen, switch screens or reset
// the scroll region over the bar's row.
func (b *statusBar) onCsi(cmd ansi.Cmd, params ansi.Params) {
	if cmd.Intermediate() != 0 {
		return
	}
	switch prefix, final := cmd.Prefix(), cmd.Final(); {
	case prefix == 0 && final == 'J':
		if n, _, _ := params.Param(0, 0); n == 2 || n == 3 {
			b.urgent = true
		}
	case prefix == '?' && (final == 'h' || final == 'l'):
		params.ForEach(0, func(_, mode int, _ bool) {
			if mode == 47 || mode == 1047 || mode == 1049 {
				b.urgent = true
			}
		})
	case prefix == 0 && final == 'r':
		top, _, _ := params.Param(0, 1)
		if bottom, _, _ := params.Param(1, 0); bottom == 0 || bottom >= b.rows {
			b.top = max(top, 1)
			b.urgent, b.region = true, true
		}
	}
}

// onEsc notes a full reset (RIS), which clears the screen and the scroll region.
func (b *statusBar) onEsc(cmd ansi.Cmd) {
	if cmd.Intermediate() == 0 && cmd.Final() == 'c' {
		b.top = 1
		b.urgent, b.region = true, true
	}
}

// barText lays out the bar, width cells: left, then right at the end;
// left is cut first when they do not fit.
func barText(left, right string, width int) string {
	lw, rw := ansi.StringWidth(left), ansi.StringWidth(right)
	switch {
	case width <= 0:
		return ""
	case lw+rw <= width:
		return left + strings.Repeat(" ", width-lw-rw) + right
	case rw < width:
		return ansi.Truncate(left, width-rw, "…") + right
	}
	return ansi.Truncate(right, width, "")
}
