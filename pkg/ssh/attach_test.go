package ssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	"charm.land/wish/v2/testsession"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/events"
	"github.com/charmbracelet/soft-serve/pkg/openshell"
	"github.com/charmbracelet/soft-serve/pkg/sessionctl"
	"github.com/charmbracelet/soft-serve/pkg/signals"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/test"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/sessions"
	"github.com/charmbracelet/x/ansi"
	gossh "golang.org/x/crypto/ssh"
)

// echoShell is a remote program for openshell.Bridge: it greets, echoes
// its input as "echo:<input>", records resizes, and exits 0 when it reads
// "~." (as the osewa attach client does on detach).
type echoShell struct {
	out     *io.PipeReader
	w       *io.PipeWriter
	exited  chan struct{}
	once    sync.Once
	mu      sync.Mutex
	input   bytes.Buffer
	resizes []string
}

func newEchoShell() *echoShell {
	r, w := io.Pipe()
	s := &echoShell{out: r, w: w, exited: make(chan struct{})}
	go func() { _, _ = io.WriteString(w, "REMOTE READY\r\n") }()
	return s
}

func (s *echoShell) Read(p []byte) (int, error) { return s.out.Read(p) }

func (s *echoShell) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.input.Write(p)
	detach := strings.Contains(s.input.String(), "~.")
	s.mu.Unlock()
	if detach {
		s.exit()
		return len(p), nil
	}
	go func() { _, _ = fmt.Fprintf(s.w, "echo:%s\r\n", p) }()
	return len(p), nil
}

func (s *echoShell) exit() {
	s.once.Do(func() {
		close(s.exited)
		_ = s.w.Close()
	})
}

func (s *echoShell) Resize(cols, rows uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resizes = append(s.resizes, fmt.Sprintf("%dx%d", cols, rows))
	return nil
}

func (s *echoShell) ExitCode() (int, error) {
	<-s.exited
	return 0, nil
}

func (s *echoShell) Close() error { s.exit(); return nil }

func (s *echoShell) seen() (string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.input.String(), append([]string(nil), s.resizes...)
}

// fakeAttacher bridges to an echoShell with the real openshell.Bridge.
type fakeAttacher struct {
	shell  *echoShell
	size   string
	closed chan struct{}
	once   sync.Once
}

func (a *fakeAttacher) Session() *openshell.Session {
	return &openshell.Session{Repo: "r", Name: "main", Sandbox: "sb-r"}
}

func (a *fakeAttacher) Attach(ctx context.Context, term openshell.Terminal) (int, error) {
	pty, _, _ := term.Pty()
	a.size = fmt.Sprintf("%dx%d", pty.Window.Width, pty.Window.Height)
	return openshell.Bridge(ctx, term, a.shell) //nolint:wrapcheck
}

func (a *fakeAttacher) Close() { a.once.Do(func() { close(a.closed) }) }

// fakeSource is a Sessions tab Source: rows, or one session, r / main.
type fakeSource struct {
	att  *fakeAttacher
	rows []sessionctl.Row
}

func (f *fakeSource) List(context.Context) ([]sessionctl.Row, error) {
	if f.rows != nil {
		return f.rows, nil
	}
	return []sessionctl.Row{{
		Info:   openshell.Info{Repo: "r", Name: "main", Sandbox: "sb-r", Phase: "Ready"},
		Status: &openshell.SessionStatus{Record: events.Record{State: events.StateIdle, Msg: "waiting"}},
	}}, nil
}
func (f *fakeSource) Statuses() map[string]openshell.SessionStatus { return nil }
func (f *fakeSource) Follow() ([]signals.Signal, sessions.Feed) {
	_, feed := (*signals.History)(nil).Follow(signals.Query{})
	return nil, feed
}
func (f *fakeSource) Keep() int                               { return 100 }
func (f *fakeSource) Repos(context.Context) ([]string, error) { return []string{"r"}, nil }
func (f *fakeSource) Profiles() []sessions.Profile            { return nil }
func (f *fakeSource) RepoProfile(string) string               { return "" }
func (f *fakeSource) Command(string) string                   { return "bash" }
func (f *fakeSource) AttachCommand(repo, name string) string {
	return "ssh -t host session attach " + repo + " -n " + name
}
func (f *fakeSource) End(context.Context, string, string) (bool, error)  { return true, nil }
func (f *fakeSource) Park(context.Context, string, string) (bool, error) { return true, nil }
func (f *fakeSource) Prepare(_ context.Context, req sessionctl.Request) (sessions.Attacher, error) {
	if req.Repo != "r" || req.Name != "main" || req.Create {
		return nil, fmt.Errorf("unexpected request %+v", req)
	}
	return f.att, nil
}

// screen collects what the SSH client receives.
type screen struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// text returns what was received from offset on, escape sequences removed.
func (s *screen) text(from int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ansi.Strip(s.buf.String()[min(from, s.buf.Len()):])
}

// raw returns what was received from offset on.
func (s *screen) raw(from int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()[min(from, s.buf.Len()):]
}

func (s *screen) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

// waitFor waits until want shows up after offset from.
func (s *screen) waitFor(t *testing.T, from int, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.text(from), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%q never showed up; got:\n%s", want, s.text(from))
}

// newTestBackend returns a backend on a fresh database.
func newTestBackend(t *testing.T) (*config.Config, *db.DB, store.Store, *backend.Backend) {
	t.Helper()
	t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
	t.Setenv("SOFT_SERVE_SSH_LISTEN_ADDR", fmt.Sprintf(":%d", test.RandomPort()))
	ctx := context.Background()
	cfg := config.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbx.Close() })
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	dbstore := database.New(ctx, dbx)
	be := backend.New(store.WithContext(ctx, dbstore), cfg, dbx, dbstore)
	return cfg, dbx, dbstore, be
}

// TestTUIAttachRoundTrip drives the real TUI over a charm.land/ssh server
// with an emulated PTY, as `soft serve` runs it: it attaches to a session
// from the Sessions tab, types into the session, resizes the window,
// detaches with ~., and checks the TUI has its input back.
func TestTUIAttachRoundTrip(t *testing.T) {
	cfg, dbx, dbstore, be := newTestBackend(t)

	att := &fakeAttacher{shell: newEchoShell(), closed: make(chan struct{})}
	src := &fakeSource{att: att}
	handlerDone := make(chan struct{})
	srv := &ssh.Server{
		Handler: ContextMiddleware(cfg, dbx, dbstore, be, log.Default())(func(s ssh.Session) {
			s.Context().SetValue(sessions.SourceKey, sessions.Source(src))
			TUIMiddleware(func(s ssh.Session) {
				close(handlerDone)
			})(s)
		}),
	}
	if err := ssh.EmulatePty()(srv); err != nil {
		t.Fatal(err)
	}

	before := goroutines("pkg/ssh.(*terminalSwitch)", "pages/sessions.")

	sess := testsession.New(t, srv, nil)
	var out screen
	sess.Stdout = &out
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm-256color", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	send := func(s string) {
		t.Helper()
		if _, err := io.WriteString(stdin, s); err != nil {
			t.Fatal(err)
		}
	}

	out.waitFor(t, 0, "Repositories")
	send("2")
	out.waitFor(t, 0, "r / main")
	out.waitFor(t, 0, "idle")

	// Attach.
	mark := out.len()
	send("\r")
	out.waitFor(t, mark, "Attaching to r / main")
	out.waitFor(t, mark, "REMOTE READY")
	if att.size != "80x24" {
		t.Errorf("attached with a %s PTY, want 80x24", att.size)
	}

	// What the user types goes to the session, not the TUI ("x" would
	// open the end confirm, "q" would quit).
	send("qx-hello")
	out.waitFor(t, mark, "echo:qx-hello")
	if err := sess.WindowChange(30, 100); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		_, rs := att.shell.seen()
		return len(rs) > 0 && rs[len(rs)-1] == "100x30"
	}, "the session was not resized to 100x30")

	// Detach; the TUI comes back on the same row.
	mark = out.len()
	send("\r~.")
	out.waitFor(t, mark, "Detached from r / main")
	select {
	case <-att.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the attacher (and its bridge count) was not closed")
	}
	if in, _ := att.shell.seen(); in != "qx-hello\r~." {
		t.Errorf("the session read %q, want exactly what was typed while attached", in)
	}

	// The TUI reads the keyboard again, at the size the window has now:
	// the end confirm fills the 100-column line (96 inside the margins).
	mark = out.len()
	send("x")
	out.waitFor(t, mark, "End r / main?")
	out.waitFor(t, mark, "y / N")
	if col := answerColumn(out.raw(mark)); col < 90 {
		t.Errorf("the confirm's y / N is at column %d, want near the end of the 100-column window", col)
	}
	send("n")
	if in, _ := att.shell.seen(); in != "qx-hello\r~." {
		t.Errorf("keys typed after detaching reached the session: %q", in)
	}

	// Quit; the TUI's goroutines end with the session.
	send("q")
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the TUI did not quit")
	}
	_ = sess.Close()
	waitUntil(t, func() bool {
		return goroutines("pkg/ssh.(*terminalSwitch)", "pages/sessions.") <= before
	}, "TUI goroutines are left running:\n"+goroutineDump())
}

func waitUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}

// answerColumn returns the column the renderer moved to (CHA) to draw the
// confirm's "y / N": it skips blanks with cursor moves, so the column is
// where the line's right end is.
func answerColumn(raw string) int {
	m := regexp.MustCompile(`\x1b\[(\d+)G(?:\x1b\[[0-9;]*m)*y(?:\x1b\[[0-9;]*m)* / `).FindStringSubmatch(raw)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func goroutineDump() string {
	var b bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&b, 1)
	return b.String()
}

// goroutines counts the goroutines whose stack mentions any of subs.
func goroutines(subs ...string) int {
	var b bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&b, 2)
	n := 0
	for _, g := range strings.Split(b.String(), "\n\n") {
		for _, s := range subs {
			if strings.Contains(g, s) {
				n++
				break
			}
		}
	}
	return n
}
