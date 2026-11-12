package ssh

import (
	"context"
	"io"
	"runtime/debug"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/log/v2"
	"charm.land/ssh"
	bm "charm.land/wish/v2/bubbletea"
	"github.com/charmbracelet/soft-serve/pkg/openshell"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/sessions"
)

// pausedPoll is how often a read by the TUI returns while an attach holds
// the input. Bubble Tea cancels its input reader before it runs an
// ExecCommand, but on an SSH channel (not a file) the cancel cannot stop a
// read in progress: the reader only notices once Read returns. Returning
// (0, nil) now and then lets it notice and stop, so the attach is then the
// only reader. It only happens between handing over and Bubble Tea's
// cancel, a few milliseconds.
const pausedPoll = 10 * time.Millisecond

// terminalSwitch shares the SSH channel of a TUI session between the TUI
// and an attached session (tui-screens.md R8). One goroutine reads the
// channel and hands what it read to whoever holds the input: the TUI, or
// the attach that took it. Window changes go the same way: to the TUI as
// tea.WindowSizeMsg, or to the attach's bridge. So the TUI's input reader
// and the bridge never both read the channel, and neither loses the
// other's keystrokes or resizes.
type terminalSwitch struct {
	src io.Reader
	out io.Writer
	pty ssh.Pty

	mu sync.Mutex
	// changed is closed, and replaced, whenever the state below changes.
	changed chan struct{}
	// pending is what was read from src and not consumed yet; the pump
	// reads again once it is.
	pending []byte
	// err is src's read error; reads return it once pending is consumed.
	err     error
	pumping bool
	closed  bool
	// owner is 0 when the TUI holds the input, else the attach's id.
	owner, nextID uint64
	// win is the current window size.
	win ssh.Window
	// attachWin receives window changes while an attach holds the
	// terminal.
	attachWin chan ssh.Window
	// tuiWin receives the TUI's window changes, the latest only, for
	// sendWindows to deliver.
	tuiWin chan ssh.Window
	send   func(tea.Msg)
}

var _ sessions.Handoff = (*terminalSwitch)(nil)

func newTerminalSwitch(s ssh.Session) *terminalSwitch {
	pty, _, _ := s.Pty()
	return &terminalSwitch{
		src:     s,
		out:     s,
		pty:     pty,
		win:     pty.Window,
		changed: make(chan struct{}),
		tuiWin:  make(chan ssh.Window, 1),
	}
}

// broadcast wakes the waiters. Call with mu held.
func (t *terminalSwitch) broadcast() {
	close(t.changed)
	t.changed = make(chan struct{})
}

// wait releases mu until the state changes or d passes (d <= 0: no
// limit), then takes it again.
func (t *terminalSwitch) wait(d time.Duration) {
	ch := t.changed
	t.mu.Unlock()
	if d > 0 {
		timer := time.NewTimer(d)
		select {
		case <-ch:
		case <-timer.C:
		}
		timer.Stop()
	} else {
		<-ch
	}
	t.mu.Lock()
}

// pump reads the SSH channel until it fails or the switch closes,
// handing over one read at a time.
func (t *terminalSwitch) pump() {
	buf := make([]byte, 4096)
	for {
		n, err := t.src.Read(buf)
		t.mu.Lock()
		for len(t.pending) > 0 && !t.closed {
			t.wait(0)
		}
		if t.closed {
			t.mu.Unlock()
			return
		}
		t.pending = append([]byte(nil), buf[:n]...)
		t.err = err
		t.broadcast()
		t.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// read reads for the holder id: 0 is the TUI. A TUI read while an attach
// holds the input returns (0, nil) within pausedPoll; an attach's read
// once it no longer holds the input returns io.EOF.
func (t *terminalSwitch) read(id uint64, p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.pumping {
		t.pumping = true
		go t.pump()
	}
	for {
		switch {
		case t.owner != id && id != 0:
			return 0, io.EOF
		case t.owner != id:
			t.wait(pausedPoll)
			return 0, nil
		case len(t.pending) > 0:
			n := copy(p, t.pending)
			t.pending = t.pending[n:]
			if len(t.pending) == 0 {
				t.broadcast()
			}
			return n, nil
		case t.err != nil:
			return 0, t.err
		case t.closed:
			return 0, io.EOF
		}
		t.wait(0)
	}
}

// tuiReader is the TUI's input.
type tuiReader struct{ t *terminalSwitch }

func (r tuiReader) Read(p []byte) (int, error) { return r.t.read(0, p) }

// Take implements sessions.Handoff.
func (t *terminalSwitch) Take() (openshell.Terminal, func()) {
	t.mu.Lock()
	t.nextID++
	id := t.nextID
	t.owner = id
	win := make(chan ssh.Window, 1)
	t.attachWin = win
	t.broadcast()
	t.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			t.mu.Lock()
			if t.owner == id {
				t.owner = 0
				t.attachWin = nil
			}
			w := t.win
			t.broadcast()
			t.mu.Unlock()
			// The window may have changed while the attach had it.
			t.toTUI(w)
		})
	}
	return &attachTerminal{t: t, id: id, win: win}, release
}

// Close ends the switch: reads return io.EOF and the pump stops. It is
// called once the TUI has exited.
func (t *terminalSwitch) Close() {
	t.mu.Lock()
	t.closed = true
	t.broadcast()
	t.mu.Unlock()
}

// setSend sets how window changes reach the TUI.
func (t *terminalSwitch) setSend(send func(tea.Msg)) {
	t.mu.Lock()
	t.send = send
	t.mu.Unlock()
}

// toTUI queues a window size for the TUI, replacing one not delivered.
func (t *terminalSwitch) toTUI(w ssh.Window) {
	for {
		select {
		case t.tuiWin <- w:
			return
		default:
		}
		select {
		case <-t.tuiWin:
		default:
		}
	}
}

// routeWindows delivers the session's window changes until ctx is done:
// to the attach holding the terminal, else to the TUI.
func (t *terminalSwitch) routeWindows(ctx context.Context, winch <-chan ssh.Window) {
	for {
		select {
		case <-ctx.Done():
			return
		case w, ok := <-winch:
			if !ok {
				return
			}
			t.mu.Lock()
			t.win = w
			aw := t.attachWin
			t.mu.Unlock()
			if aw == nil {
				t.toTUI(w)
				continue
			}
			select {
			case aw <- w:
			default:
				// Replace the size the bridge has not read yet.
				select {
				case <-aw:
				default:
				}
				select {
				case aw <- w:
				default:
				}
			}
		}
	}
}

// sendWindows sends the TUI's window changes to it until ctx is done. It
// blocks while the TUI runs an attach, without holding up routeWindows.
func (t *terminalSwitch) sendWindows(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case w := <-t.tuiWin:
			t.mu.Lock()
			send := t.send
			t.mu.Unlock()
			if send != nil {
				send(tea.WindowSizeMsg{Width: w.Width, Height: w.Height})
			}
		}
	}
}

// attachTerminal is the terminal an attach gets from Take.
type attachTerminal struct {
	t   *terminalSwitch
	id  uint64
	win chan ssh.Window
}

func (a *attachTerminal) Read(p []byte) (int, error)  { return a.t.read(a.id, p) }
func (a *attachTerminal) Write(p []byte) (int, error) { return a.t.out.Write(p) } //nolint:wrapcheck

// Pty returns the session's PTY with the current window size, and the
// window changes while the attach holds the terminal.
func (a *attachTerminal) Pty() (ssh.Pty, <-chan ssh.Window, bool) {
	a.t.mu.Lock()
	pty := a.t.pty
	pty.Window = a.t.win
	a.t.mu.Unlock()
	return pty, a.win, true
}

// TUIMiddleware runs the TUI on PTY sessions, like wish's bubbletea
// middleware, but the SSH channel's input and window changes go through a
// terminalSwitch, so the TUI can hand the terminal to an attached session
// and take it back. Sessions SessionHandler does not run a TUI for go on
// to next.
func TUIMiddleware(next ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		_, winch, ok := s.Pty()
		if !ok {
			next(s)
			return
		}
		sw := newTerminalSwitch(s)
		p := sessionProgram(s, sw)
		if p == nil {
			next(s)
			return
		}
		sw.setSend(p.Send)
		ctx, cancel := context.WithCancel(s.Context())
		go func() {
			defer recoverWindows(p)
			sw.routeWindows(ctx, winch)
		}()
		go func() {
			defer recoverWindows(p)
			sw.sendWindows(ctx)
		}()
		go func() {
			<-ctx.Done()
			p.Quit()
		}()
		if _, err := p.Run(); err != nil {
			log.Error("app exit with error", "error", err)
		}
		// Kill restores the terminal in case the TUI crashed.
		p.Kill()
		cancel()
		sw.Close()
		next(s)
	}
}

// recoverWindows keeps a panic on a window goroutine (which the recover
// middleware cannot see) from taking the server down; it quits the TUI.
func recoverWindows(p *tea.Program) {
	if r := recover(); r != nil {
		log.Error("panic in window change handler", "panic", r, "stack", string(debug.Stack()))
		p.Quit()
	}
}

// programOptions are the options of a TUI program on s: wish's, with the
// input taken from sw when there is one.
func programOptions(s ssh.Session, sw *terminalSwitch) []tea.ProgramOption {
	opts := bm.MakeOptions(s)
	if sw != nil {
		opts = append(opts, tea.WithInput(tuiReader{sw}))
	}
	return opts
}
