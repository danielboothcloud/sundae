package ssh

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/db"
	"github.com/danielboothcloud/sundae/pkg/db/migrate"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/sessionctl"
	"github.com/danielboothcloud/sundae/pkg/signals"
	"github.com/danielboothcloud/sundae/pkg/store"
	"github.com/danielboothcloud/sundae/pkg/store/database"
	"github.com/danielboothcloud/sundae/pkg/test"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/sessions"
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

// fakeAttacher bridges to an echoShell with the real openshell.Bridge, as
// sessionctl.Prepared does: with the home key and, with bar, the status bar.
type fakeAttacher struct {
	shell  *echoShell
	home   openshell.HomeKey
	bar    bool
	name   string
	size   string
	closed chan struct{}
	once   sync.Once
}

func newFakeAttacher(name string, home openshell.HomeKey, bar bool) *fakeAttacher {
	return &fakeAttacher{shell: newEchoShell(), home: home, bar: bar, name: name, closed: make(chan struct{})}
}

func (a *fakeAttacher) Session() *openshell.Session {
	name := a.name
	if name == "" {
		name = "main"
	}
	return &openshell.Session{Repo: "r", Repos: []string{"r"}, Name: name, Sandbox: "sb-r-" + name}
}

func (a *fakeAttacher) Attach(ctx context.Context, term openshell.Terminal) (int, error) {
	pty, _, _ := term.Pty()
	cols, rows := openshell.RemoteSize(pty.Window, a.bar)
	a.size = fmt.Sprintf("%dx%d", cols, rows)
	opts := []openshell.BridgeOption{openshell.WithHomeKey(a.home)}
	if a.bar {
		s := a.Session()
		opts = append(opts, openshell.WithStatusBar(signals.SessionLabel(s.Name, s.Repos)))
	}
	return openshell.Bridge(ctx, term, a.shell, opts...) //nolint:wrapcheck
}

func (a *fakeAttacher) Close() { a.once.Do(func() { close(a.closed) }) }

// fakeSource is a Sessions tab Source: rows, or one session, r / main.
// Prepare hands out att, or the attacher of the session named.
type fakeSource struct {
	att      *fakeAttacher
	atts     map[string]*fakeAttacher
	rows     []sessionctl.Row
	statuses map[string]openshell.SessionStatus
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
func (f *fakeSource) Statuses() map[string]openshell.SessionStatus { return f.statuses }
func (f *fakeSource) Follow() ([]signals.Signal, sessions.Feed) {
	_, feed := (*signals.History)(nil).Follow(signals.Query{})
	return nil, feed
}
func (f *fakeSource) Keep() int                               { return 100 }
func (f *fakeSource) Repos(context.Context) ([]string, error) { return []string{"r"}, nil }
func (f *fakeSource) Profiles() []sessions.Profile            { return nil }
func (f *fakeSource) RepoProfile(string) string               { return "" }
func (f *fakeSource) Command(string) string                   { return "bash" }
func (f *fakeSource) AttachCommand(name string) string {
	return "ssh -t host session attach " + name
}
func (f *fakeSource) Delete(context.Context, string) (bool, error)  { return true, nil }
func (f *fakeSource) Suspend(context.Context, string) (bool, error) { return true, nil }
func (f *fakeSource) SyncRepos(context.Context, string, []string) ([]sessions.RepoSync, error) {
	return nil, nil
}

func (f *fakeSource) Prepare(_ context.Context, req sessionctl.Request) (sessions.Attacher, error) {
	if a, ok := f.atts[req.Name]; ok && !req.Create {
		return a, nil
	}
	if req.Name != "main" || req.Create {
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
	t.Setenv("SUNDAE_DATA_PATH", t.TempDir())
	t.Setenv("SUNDAE_SSH_LISTEN_ADDR", fmt.Sprintf(":%d", test.RandomPort()))
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
// with an emulated PTY, as `sundae serve` runs it: it attaches to a session
// from the Sessions tab, types into the session, resizes the window,
// detaches with ~., and checks the TUI has its input back.
func TestTUIAttachRoundTrip(t *testing.T) {
	cfg, dbx, dbstore, be := newTestBackend(t)

	att := newFakeAttacher("main", 0, false)
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
	out.waitFor(t, mark, "Left r / main. enter goes back.")
	select {
	case <-att.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the attacher (and its bridge count) was not closed")
	}
	if in, _ := att.shell.seen(); in != "qx-hello\r~." {
		t.Errorf("the session read %q, want exactly what was typed while attached", in)
	}

	// The TUI reads the keyboard again, at the size the window has now:
	// the delete confirm's keys end the selected row of the 100-column tab.
	mark = out.len()
	send("d")
	out.waitFor(t, mark, "delete?")
	out.waitFor(t, mark, "esc keep")
	// 100 - 2 (margin) - 19 ("y delete • esc keep") + 1 = 80; an 80-column
	// render puts it at 60.
	if col := answerColumn(out.raw(mark)); col != 80 {
		t.Errorf("the confirm's y delete • esc keep is at column %d, want 80, at the end of the 100-column row", col)
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

// TestTUIAttachHomeKey leaves a session attached from the Sessions tab with
// Ctrl-]: the status bar showed while attached, the TUI comes back on the
// row with the notice and reads the keyboard again.
func TestTUIAttachHomeKey(t *testing.T) {
	cfg, dbx, dbstore, be := newTestBackend(t)

	att := newFakeAttacher("main", openshell.DefaultHomeKey, true)
	src := &fakeSource{att: att}
	handlerDone := make(chan struct{})
	srv := &ssh.Server{
		Handler: ContextMiddleware(cfg, dbx, dbstore, be, log.Default())(func(s ssh.Session) {
			s.Context().SetValue(sessions.SourceKey, sessions.Source(src))
			TUIMiddleware(func(ssh.Session) { close(handlerDone) })(s)
		}),
	}
	out, send := startPtySession(t, srv)

	out.waitFor(t, 0, "Repositories")
	send("2")
	out.waitFor(t, 0, "idle")
	mark := out.len()
	send("\r")
	out.waitFor(t, mark, "Attaching to r / main …  Ctrl-] comes back here.")
	out.waitFor(t, mark, "REMOTE READY")
	out.waitFor(t, mark, " r / main ")
	out.waitFor(t, mark, " Ctrl-] sessions ")
	if att.size != "80x23" {
		t.Errorf("attached with a %s PTY, want 80x23 (a row for the status bar)", att.size)
	}
	if !strings.Contains(out.raw(mark), "\x1b[1;23r") {
		t.Error("the scroll region was not set above the status bar")
	}

	mark = out.len()
	send("abc\x1dd")
	out.waitFor(t, mark, "Left r / main; it keeps running. enter goes back.")
	select {
	case <-att.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the attacher was not closed")
	}
	if raw := out.raw(mark); !strings.Contains(raw, "\x1b[?1049l") || !strings.Contains(raw, "\x1b7\x1b[r\x1b[24;1H\x1b[2K\x1b8") {
		t.Error("the terminal modes and the status bar were not reset")
	}

	mark = out.len()
	send("d")
	out.waitFor(t, mark, "delete?")
	send("n")
	if in, _ := att.shell.seen(); in != "abc" {
		t.Errorf("the session read %q, want abc", in)
	}
	send("q")
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the TUI did not quit")
	}
}

// TestCLIHomeKeyOpensSessionsScreen attaches as `session attach` does, on
// the SSH session itself, and presses Ctrl-]: the Sundae TUI opens on the
// same connection, on the Sessions tab with the session selected (enter
// attaches to it again), and quitting it ends the command.
func TestCLIHomeKeyOpensSessionsScreen(t *testing.T) {
	cfg, dbx, dbstore, be := newTestBackend(t)

	cli := newFakeAttacher("tool", openshell.DefaultHomeKey, true)
	again := newFakeAttacher("tool", openshell.DefaultHomeKey, true)
	idle := &openshell.SessionStatus{Record: events.Record{State: events.StateIdle}}
	src := &fakeSource{
		// "main" sorts first: the cursor is on "tool" only if selected.
		rows: []sessionctl.Row{
			{Info: openshell.Info{Repo: "r", Repos: []string{"r"}, Name: "main", Sandbox: "sb-r-main", Phase: "Ready"}, Status: &openshell.SessionStatus{Record: events.Record{State: events.StateBlocked}}},
			{Info: openshell.Info{Repo: "r", Repos: []string{"r"}, Name: "tool", Sandbox: "sb-r-tool", Phase: "Ready"}, Status: idle},
		},
		atts: map[string]*fakeAttacher{"tool": again},
	}
	handlerDone := make(chan bool, 1)
	srv := &ssh.Server{
		Handler: ContextMiddleware(cfg, dbx, dbstore, be, log.Default())(func(s ssh.Session) {
			s.Context().SetValue(sessions.SourceKey, sessions.Source(src))
			_, err := cli.Attach(s.Context(), s)
			if !errors.Is(err, openshell.ErrHome) {
				t.Errorf("attach: %v", err)
			}
			cli.Close()
			handlerDone <- sessionsScreen(s)("sb-r-tool", "r / tool")
		}),
	}
	out, send := startPtySession(t, srv)

	out.waitFor(t, 0, "REMOTE READY")
	out.waitFor(t, 0, " r / tool ")
	mark := out.len()
	send("x\x1b[93;5u")
	out.waitFor(t, mark, "Left r / tool; it keeps running. enter goes back.")
	out.waitFor(t, mark, "enter attach")
	// q quit ranks lowest; at 80 columns ? lists it.
	out.waitFor(t, mark, "? more")
	out.waitFor(t, mark, "idle") // listed: enter has a row to attach to
	if in, _ := cli.shell.seen(); in != "x" {
		t.Errorf("the session read %q, want x", in)
	}

	// Enter attaches to the selected session through the TUI's handoff.
	mark = out.len()
	send("\r")
	out.waitFor(t, mark, "REMOTE READY")
	send("hi")
	out.waitFor(t, mark, "echo:hi")
	mark = out.len()
	send("\x1d")
	out.waitFor(t, mark, "Left r / tool; it keeps running.")

	send("q")
	select {
	case ok := <-handlerDone:
		if !ok {
			t.Fatal("the Sessions screen did not run")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the TUI did not quit")
	}
}

// startPtySession opens an 80x24 PTY shell session on srv; send types.
func startPtySession(t *testing.T, srv *ssh.Server) (*screen, func(string)) {
	t.Helper()
	if err := ssh.EmulatePty()(srv); err != nil {
		t.Fatal(err)
	}
	sess := testsession.New(t, srv, nil)
	out := &screen{}
	sess.Stdout = out
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
	t.Cleanup(func() { _ = sess.Close() })
	return out, func(s string) {
		t.Helper()
		if _, err := io.WriteString(stdin, s); err != nil {
			t.Fatal(err)
		}
	}
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

// answerColumn returns the column the renderer moved to (CHA or CUP) to draw the
// confirm's "y delete" in the row: it skips blanks with cursor moves, so the
// column is where the keys start, at the row's right end.
func answerColumn(raw string) int {
	m := regexp.MustCompile(`\x1b\[(?:\d+;)?(\d+)[GH](?:\x1b\[[0-9;]*m)*y(?:\x1b\[[0-9;]*m)* (?:\x1b\[[0-9;]*m)*delete`).FindStringSubmatch(raw)
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
