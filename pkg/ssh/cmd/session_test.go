package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/events"
	"github.com/charmbracelet/soft-serve/pkg/openshell"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/matryer/is"
)

// fakeOpenShell is an in-memory openshell.Client. It answers the bootstrap
// scripts as a sandbox whose image has the pinned osewa.
type fakeOpenShell struct {
	mu        sync.Mutex
	sandboxes map[string]*openshell.Sandbox
	cloned    map[string]bool
	// osewa maps a sandbox to its running osewa session id.
	osewa       map[string]string
	nextID      int
	interactive [][]string
	exitCode    int
	output      string
	// status maps a sandbox to the `osewa local ls` session its status
	// poll reports for the running osewa session.
	status map[string]map[string]any
	// suspends and resumes count parks and resumes of parked sandboxes.
	suspends, resumes int
	suspendErr        error
	// cleanRestart makes a resume lose the osewa session, as a sandbox
	// whose memory could not be restored.
	cleanRestart bool
	// onInteractive runs when the attach client starts; interactiveErr
	// makes it fail.
	onInteractive  func(sandbox string)
	interactiveErr error
	// ensureErr makes EnsureSandbox fail.
	ensureErr error
}

var _ openshell.Client = (*fakeOpenShell)(nil)

func newFakeOpenShell() *fakeOpenShell {
	return &fakeOpenShell{
		sandboxes: map[string]*openshell.Sandbox{},
		cloned:    map[string]bool{},
		osewa:     map[string]string{},
		status:    map[string]map[string]any{},
	}
}

func notFound() error { return &types.StatusError{Code: types.ErrorNotFound, Message: "not found"} }

// unpark resumes a parked sandbox, as the SDK client does. Call with mu
// held.
func (f *fakeOpenShell) unpark(sb *openshell.Sandbox) {
	if sb.Status.Phase != types.SandboxSuspended {
		return
	}
	sb.Status.Phase = types.SandboxReady
	f.resumes++
	if f.cleanRestart {
		delete(f.osewa, sb.Name)
	}
}

func (f *fakeOpenShell) EnsureSandbox(_ context.Context, name string, req openshell.SandboxRequest) (*openshell.Sandbox, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ensureErr != nil {
		return nil, false, f.ensureErr
	}
	if sb, ok := f.sandboxes[name]; ok {
		f.unpark(sb)
		return sb, false, nil
	}
	sb := &openshell.Sandbox{Name: name, Labels: req.Labels, Annotations: req.Annotations}
	sb.Status.Phase = types.SandboxReady
	f.sandboxes[name] = sb
	return sb, true, nil
}

func (f *fakeOpenShell) ResumeSandbox(_ context.Context, name string, labels map[string]string) (*openshell.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.sandboxes[name]
	if !ok {
		return nil, notFound()
	}
	if sb.Labels[openshell.LabelUserID] != labels[openshell.LabelUserID] {
		return nil, openshell.ErrNotOwned
	}
	f.unpark(sb)
	return sb, nil
}

func (f *fakeOpenShell) SuspendSandbox(_ context.Context, name string) (*openshell.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.suspendErr != nil {
		return nil, f.suspendErr
	}
	sb, ok := f.sandboxes[name]
	if !ok {
		return nil, notFound()
	}
	f.suspends++
	sb.Status.Phase = types.SandboxSuspended
	return sb, nil
}

// phase returns a sandbox's phase.
func (f *fakeOpenShell) phase(name string) types.SandboxPhase {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sb, ok := f.sandboxes[name]; ok {
		return sb.Status.Phase
	}
	return ""
}

// setStatus sets the OSC 7501 record the sandbox's status poll reports; nil
// for none.
func (f *fakeOpenShell) setStatus(sandbox string, attached int, record map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := map[string]any{"attached": attached}
	if record != nil {
		s["status"] = record
	}
	f.status[sandbox] = s
}

func (f *fakeOpenShell) GetSandbox(_ context.Context, name string) (*openshell.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sb, ok := f.sandboxes[name]; ok {
		return sb, nil
	}
	return nil, notFound()
}

func (f *fakeOpenShell) ListSandboxes(_ context.Context, labels map[string]string) ([]*openshell.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*openshell.Sandbox
	for _, sb := range f.sandboxes {
		match := true
		for k, v := range labels {
			match = match && sb.Labels[k] == v
		}
		if match {
			out = append(out, sb)
		}
	}
	return out, nil
}

func (f *fakeOpenShell) DeleteSandbox(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sandboxes, name)
	delete(f.osewa, name)
	return nil
}

func (f *fakeOpenShell) Upload(context.Context, string, string, string) error {
	return fmt.Errorf("unexpected upload")
}

func (f *fakeOpenShell) Run(_ context.Context, name string, command []string, _ openshell.ExecOptions) (*openshell.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := ""
	switch command[0] {
	case "test":
		if !f.cloned[name] {
			return &openshell.ExecResult{ExitCode: 1}, nil
		}
	case "git", "jj":
		f.cloned[name] = true
	case "sh":
		switch command[3] {
		case "soft-serve-osewa-probe":
			out = "osewa=/usr/local/bin/osewa\narch=x86_64\nenrolled=yes\n"
		case "soft-serve-osewa-ls":
			id := f.osewa[name]
			ls, _ := json.Marshal(map[string]any{"sessions": []map[string]any{{"id": id, "exited": false}}})
			out = id + "\n" + string(ls) + "\n"
		case "soft-serve-osewa-status":
			id := f.osewa[name]
			sessions := []map[string]any{}
			if id != "" {
				s := map[string]any{"id": id, "exited": false}
				for k, v := range f.status[name] {
					s[k] = v
				}
				sessions = append(sessions, s)
			}
			ls, _ := json.Marshal(map[string]any{"id": 0, "sessions": sessions})
			out = id + "\n" + string(ls) + "\n"
		case "soft-serve-osewa-new":
			f.nextID++
			f.osewa[name] = "id" + strconv.Itoa(f.nextID)
			out = "created " + f.osewa[name] + "\n"
		}
	}
	return &openshell.ExecResult{Stdout: []byte(out)}, nil
}

func (f *fakeOpenShell) Interactive(_ context.Context, name string, command []string, _, _ uint32, _ openshell.ExecOptions) (openshell.InteractiveSession, error) {
	f.mu.Lock()
	onInteractive, err := f.onInteractive, f.interactiveErr
	f.mu.Unlock()
	if onInteractive != nil {
		onInteractive(name)
	}
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interactive = append(f.interactive, command)
	return &scriptedShell{out: strings.NewReader(f.output), code: f.exitCode}, nil
}

func (f *fakeOpenShell) Close() error { return nil }

// scriptedShell prints fixed output and exits with a fixed code.
type scriptedShell struct {
	out  io.Reader
	code int
}

func (s *scriptedShell) Read(p []byte) (int, error)  { return s.out.Read(p) }
func (s *scriptedShell) Write(p []byte) (int, error) { return len(p), nil }
func (s *scriptedShell) Resize(uint32, uint32) error { return nil }
func (s *scriptedShell) ExitCode() (int, error)      { return s.code, nil }
func (s *scriptedShell) CloseWrite() error           { return nil }
func (s *scriptedShell) Close() error                { return nil }

// fakeSSHSession is the parts of ssh.Session the session commands use.
type fakeSSHSession struct {
	ssh.Session
	noPty    bool
	mu       sync.Mutex
	out      bytes.Buffer
	exitCode int
	exited   bool
}

func (s *fakeSSHSession) Read([]byte) (int, error) { return 0, io.EOF }
func (s *fakeSSHSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.Write(p)
}

func (s *fakeSSHSession) Pty() (ssh.Pty, <-chan ssh.Window, bool) {
	if s.noPty {
		return ssh.Pty{}, nil, false
	}
	return ssh.Pty{Term: "xterm", Window: ssh.Window{Width: 80, Height: 24}}, make(chan ssh.Window), true
}

func (s *fakeSSHSession) Exit(code int) error {
	s.exitCode, s.exited = code, true
	return nil
}

// newSessionTestContext returns an authenticated context with OpenShell
// enabled and newOpenShellClient swapped for an in-memory fake.
func newSessionTestContext(t *testing.T) (context.Context, *backend.Backend, proto.User, *fakeOpenShell) {
	t.Helper()
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	cfg := config.FromContext(ctx)
	cfg.OpenShell.Enabled = true
	cfg.OpenShell.Image = "sandbox:latest"
	cfg.Sessions.Profiles = map[string]config.SessionProfile{"claude": {Command: "claude"}}

	fc := newFakeOpenShell()
	prev := newOpenShellClient
	newOpenShellClient = func(*config.Config) (openshell.Client, error) { return fc, nil }
	t.Cleanup(func() { newOpenShellClient = prev })

	ctx = withUser(t, ctx, be, "alice", false)
	user := proto.UserFromContext(ctx)
	is.True(user != nil)
	return ctx, be, user, fc
}

func runSessionCmd(t *testing.T, ctx context.Context, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	c := SessionCommand()
	var outBuf, errBuf bytes.Buffer
	c.SetOut(&outBuf)
	c.SetErr(&errBuf)
	c.SetArgs(args)
	err = c.ExecuteContext(ctx)
	return outBuf.String(), errBuf.String(), err
}

func TestSessionStartPropagatesExitCode(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	_, err := be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)

	fc.output = "hello from the sandbox\r\n"
	fc.exitCode = 3
	sess := &fakeSSHSession{}
	ctx = context.WithValue(ctx, sshutils.ContextKeySession, sess)

	_, stderr, err := runSessionCmd(t, ctx, "start", "r")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Starting sandbox soft-alice-r"))
	is.True(strings.Contains(sess.out.String(), "hello from the sandbox"))
	is.True(sess.exited)
	is.Equal(sess.exitCode, 3)
	// The terminal is bridged to the osewa attach client.
	is.Equal(strings.Join(fc.interactive[0], " "), "/usr/local/bin/osewa attach --take --predict never sandbox id1")

	// A clean exit (or a detach) does not force an exit status.
	fc.exitCode = 0
	sess = &fakeSSHSession{}
	ctx = context.WithValue(ctx, sshutils.ContextKeySession, sess)
	_, _, err = runSessionCmd(t, ctx, "start", "r")
	is.NoErr(err)
	is.True(!sess.exited)
	// The running program was reused.
	is.Equal(strings.Join(fc.interactive[1], " "), "/usr/local/bin/osewa attach --take --predict never sandbox id1")
}

func TestSessionStartDetachAndAttach(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	_, err := be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)

	// Without a terminal, only --detach works.
	noPty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true})
	_, _, err = runSessionCmd(t, noPty, "start", "r")
	is.True(err != nil && strings.Contains(err.Error(), "session start requires a terminal (ssh -t), or --detach"))

	_, stderr, err := runSessionCmd(t, noPty, "start", "r", "--name", "review", "--profile", "claude", "--detach")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Session review of r is running"))
	is.True(strings.Contains(stderr, "Attach with: session attach r --name review"))
	is.Equal(len(fc.interactive), 0) // not attached
	sb := fc.sandboxes[openshell.SessionSandboxName("alice", "r", "review")]
	is.True(sb != nil)
	is.Equal(sb.Annotations[openshell.AnnotationProfile], "claude")
	is.Equal(sb.Annotations[openshell.AnnotationSession], "review")

	// attach needs a terminal, and an existing session.
	_, _, err = runSessionCmd(t, noPty, "attach", "r", "--name", "review")
	is.True(err != nil && strings.Contains(err.Error(), "session attach requires a terminal (ssh -t)"))
	withPty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{})
	_, _, err = runSessionCmd(t, withPty, "attach", "r")
	is.True(err != nil && strings.Contains(err.Error(), "no such session"))
	is.True(strings.Contains(err.Error(), `"session start r"`))
	_, ok := fc.sandboxes["soft-alice-r"]
	is.True(!ok) // attach never creates

	fc.exitCode = 5
	sess := &fakeSSHSession{}
	_, _, err = runSessionCmd(t, context.WithValue(ctx, sshutils.ContextKeySession, sess), "attach", "r", "-n", "review")
	is.NoErr(err)
	is.Equal(len(fc.interactive), 1)
	is.True(sess.exited)
	is.Equal(sess.exitCode, 5)

	// Bad names are refused before the gateway is contacted.
	_, _, err = runSessionCmd(t, withPty, "attach", "r", "--name", "Bad")
	is.True(err != nil && strings.Contains(err.Error(), "invalid session name"))
}

func TestSessionListAndEnd(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	_, err := be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)

	stdout, _, err := runSessionCmd(t, ctx, "ls")
	is.NoErr(err)
	is.True(strings.Contains(stdout, "No sessions found"))

	// A session whose repository has since been deleted, from before
	// named sessions.
	name := openshell.SandboxName(user.Username(), "gone")
	sb := &openshell.Sandbox{
		Name:        name,
		Labels:      map[string]string{openshell.LabelUserID: strconv.FormatInt(user.ID(), 10), openshell.LabelManaged: "true"},
		Annotations: map[string]string{openshell.AnnotationRepo: "gone"},
	}
	sb.Status.Phase = types.SandboxReady
	fc.sandboxes[name] = sb

	// Two sessions of r.
	noPty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true})
	for _, args := range [][]string{{"start", "r", "-d"}, {"start", "r", "-d", "--name", "review", "--profile", "claude"}} {
		_, _, err = runSessionCmd(t, noPty, args...)
		is.NoErr(err)
	}

	stdout, _, err = runSessionCmd(t, ctx, "ls")
	is.NoErr(err)
	for _, want := range []string{"Repository", "Name", "Profile", "Sandbox", "Phase", "gone", name, "Ready", "soft-alice-r", "review", "claude"} {
		is.True(strings.Contains(stdout, want)) // missing: want
	}
	lines := strings.Split(stdout, "\n")
	var rows []string
	for _, l := range lines {
		if strings.Contains(l, "soft-alice") {
			rows = append(rows, strings.Join(strings.Fields(strings.NewReplacer("│", " ", "|", " ").Replace(l)), " "))
		}
	}
	is.Equal(len(rows), 3)
	is.True(strings.HasPrefix(rows[0], "gone main - "+name))
	is.True(strings.HasPrefix(rows[1], "r main - soft-alice-r Ready"))
	is.True(strings.HasPrefix(rows[2], "r review claude "+openshell.SessionSandboxName("alice", "r", "review")))

	// Ending the named session leaves main.
	_, stderr, err := runSessionCmd(t, ctx, "end", "r", "--name", "review")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Session ended"))
	_, ok := fc.sandboxes[openshell.SessionSandboxName("alice", "r", "review")]
	is.True(!ok)
	_, ok = fc.sandboxes["soft-alice-r"]
	is.True(ok)

	// The deleted repository's session can still be ended, and ending
	// again is a no-op.
	_, stderr, err = runSessionCmd(t, ctx, "end", "gone")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Session ended"))
	_, ok = fc.sandboxes[name]
	is.True(!ok)

	_, stderr, err = runSessionCmd(t, ctx, "end", "gone")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "No session to end"))
}

// The session commands publish the lifecycle on the bus in the context.
func TestSessionCommandsPublishLifecycle(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	_, err := be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)

	bus := events.New(events.WithLogger(log.New(io.Discard)))
	defer bus.Close()
	sub := bus.Subscribe(events.ForUser(user.ID()))
	ctx = events.WithContext(ctx, bus)
	ctx = context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{})

	_, _, err = runSessionCmd(t, ctx, "start", "r", "--name", "two")
	is.NoErr(err)
	_, _, err = runSessionCmd(t, ctx, "attach", "r", "--name", "two")
	is.NoErr(err)
	_, _, err = runSessionCmd(t, ctx, "end", "r", "--name", "two")
	is.NoErr(err)
	sandbox := openshell.SessionSandboxName("alice", "r", "two")
	_, ok := fc.sandboxes[sandbox]
	is.True(!ok)

	var steps []string
	for range 5 {
		select {
		case ev := <-sub.C:
			is.Equal(ev.Lifecycle.Session.Sandbox, sandbox)
			is.Equal(ev.Lifecycle.Session.Name, "two")
			is.Equal(ev.Lifecycle.Session.Repo, "r")
			steps = append(steps, string(ev.Lifecycle.Step))
		case <-time.After(5 * time.Second):
			t.Fatalf("lifecycle events so far: %v", steps)
		}
	}
	is.Equal(strings.Join(steps, ","), "created,ready,resumed,ready,ended")
}
