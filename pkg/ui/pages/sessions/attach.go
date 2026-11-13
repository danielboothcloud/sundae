package sessions

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/sessionctl"
	"github.com/danielboothcloud/sundae/pkg/signals"
)

// Handoff hands the SSH terminal the TUI runs on to an attach, and back.
// The SSH layer provides it (see pkg/ssh): one goroutine owns the SSH
// channel's input and gives it either to the TUI or to the attach, so the
// two never read it at once.
type Handoff interface {
	// Take stops the TUI's input and returns the terminal for the attach:
	// it reads what the user types, writes to the SSH channel, and
	// reports window changes. release gives the input back to the TUI and
	// tells it the current window size; reads of term then return EOF.
	// release is safe to call more than once.
	Take() (term openshell.Terminal, release func())
}

// HandoffKey is the key of the Handoff in the context of common.Common.
var HandoffKey = &struct{ string }{"sessions-handoff"}

// errNoTerminal is shown when there is no terminal to hand over.
var errNoTerminal = errors.New("attaching needs the SSH terminal; use the attach command (c copies it)")

// operation is a session being started or resumed for attaching: Prepare
// runs in the background, reporting progress lines, and its Attacher is
// handed to the TUI when it is done. When the TUI goes away first (quit,
// disconnect, esc) the operation is abandoned and the Attacher closed, so
// its bridge is never left counted.
type operation struct {
	// title is shown above the progress lines.
	title string
	// repos and name are the session's.
	repos []string
	name  string
	// attach reports whether to attach once prepared.
	attach bool
	// key is the session's sandbox, when it is listed.
	key string
	// req is what run prepares, to run it again with an answer.
	req sessionctl.Request

	cancel context.CancelFunc
	stop   func() bool
	wake   chan struct{}

	mu        sync.Mutex
	lines     []string
	partial   []byte
	done      bool
	att       Attacher
	err       error
	abandoned bool
}

// opMsg reports progress or the end of an operation.
type opMsg struct{ op *operation }

// newOperation returns an operation running under a child of ctx; it is
// abandoned when ctx is done.
func newOperation(ctx context.Context, title string, repos []string, name string, attach bool) (*operation, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	op := &operation{title: title, repos: repos, name: name, attach: attach, cancel: cancel, wake: make(chan struct{}, 1)}
	op.stop = context.AfterFunc(ctx, op.abandon)
	return op, ctx
}

// run prepares the session in the background and returns the command
// waiting for the operation's first message.
func (op *operation) run(ctx context.Context, src Source, req sessionctl.Request) tea.Cmd {
	op.req = req
	req.Progress = op
	return func() tea.Msg {
		go func() {
			att, err := src.Prepare(ctx, req)
			op.finish(att, err)
		}()
		return op.waitMsg(ctx)
	}
}

// Write records progress lines.
func (op *operation) Write(p []byte) (int, error) {
	op.mu.Lock()
	op.partial = append(op.partial, p...)
	for {
		i := bytes.IndexByte(op.partial, '\n')
		if i < 0 {
			break
		}
		if line := signals.Sanitize(string(op.partial[:i]), 0); line != "" {
			op.lines = append(op.lines, line)
		}
		op.partial = op.partial[i+1:]
	}
	op.mu.Unlock()
	op.notify()
	return len(p), nil
}

func (op *operation) notify() {
	select {
	case op.wake <- struct{}{}:
	default:
	}
}

// finish records the outcome of Prepare. An abandoned operation closes the
// Attacher at once.
func (op *operation) finish(att Attacher, err error) {
	op.mu.Lock()
	if op.abandoned {
		op.mu.Unlock()
		if att != nil {
			att.Close()
		}
		return
	}
	op.done, op.att, op.err = true, att, err
	op.mu.Unlock()
	op.notify()
}

// abandon cancels the operation and closes its Attacher if nobody took it.
func (op *operation) abandon() {
	op.mu.Lock()
	op.abandoned = true
	att := op.att
	op.att = nil
	op.mu.Unlock()
	op.cancel()
	if att != nil {
		att.Close()
	}
}

// waitMsg waits for the operation's next message; nil once ctx is done.
func (op *operation) waitMsg(ctx context.Context) tea.Msg {
	select {
	case <-op.wake:
		return opMsg{op}
	case <-ctx.Done():
		return nil
	}
}

func (op *operation) waitCmd(ctx context.Context) tea.Cmd {
	return func() tea.Msg { return op.waitMsg(ctx) }
}

// state returns the progress lines and, once done, the outcome. The
// Attacher is handed over once: the caller owns it.
func (op *operation) state() (lines []string, done bool, att Attacher, err error) {
	op.mu.Lock()
	defer op.mu.Unlock()
	lines = append([]string(nil), op.lines...)
	if op.done && !op.abandoned {
		att, op.att = op.att, nil
		op.stop()
	}
	return lines, op.done, att, op.err
}

// attachExec is the tea.ExecCommand of an attach: Bubble Tea releases the
// terminal (stops its renderer, leaves the alt screen, cancels its input
// reader), runs it, then restores the terminal and repaints. Run bridges
// the handed-over terminal to the session.
type attachExec struct {
	ctx     context.Context
	att     Attacher
	term    openshell.Terminal
	release func()
	banner  string

	// code and err are the outcome; read them after Run.
	code int
	err  error
}

// SetStdin implements tea.ExecCommand. The attach reads the handed-over
// terminal instead: Bubble Tea's input on an SSH channel cannot stop a
// read in progress, so it is not given to the bridge.
func (a *attachExec) SetStdin(io.Reader) {}

// SetStdout implements tea.ExecCommand.
func (a *attachExec) SetStdout(io.Writer) {}

// SetStderr implements tea.ExecCommand.
func (a *attachExec) SetStderr(io.Writer) {}

// Run implements tea.ExecCommand. It always closes the Attacher and gives
// the input back, also when the TUI is gone by the time it returns.
func (a *attachExec) Run() error {
	defer a.att.Close()
	defer a.release()
	if a.banner != "" {
		_, _ = io.WriteString(a.term, "\r\n"+a.banner+"\r\n")
	}
	a.code, a.err = a.att.Attach(a.ctx, a.term)
	return nil
}

// attachDoneMsg reports the end of an attach.
type attachDoneMsg struct {
	repos []string
	name  string
	code  int
	err   error
}

// attachCmd hands the terminal to the prepared session.
func attachCmd(ctx context.Context, h Handoff, att Attacher, repos []string, name, banner string) tea.Cmd {
	term, release := h.Take()
	ex := &attachExec{ctx: ctx, att: att, term: term, release: release, banner: banner}
	return tea.Exec(ex, func(err error) tea.Msg {
		// Exec failed to release the terminal: Run never ran.
		if err != nil {
			release()
			att.Close()
		}
		return attachDoneMsg{repos: repos, name: name, code: ex.code, err: errors.Join(err, ex.err)}
	})
}

// sessionLabel names a session: "repo / name" for one repository, else
// "name (repo1, repo2, +N)".
func sessionLabel(name string, repos []string) string {
	if name == "" {
		name = openshell.DefaultSessionName
	}
	return signals.SessionLabel(name, repos)
}
