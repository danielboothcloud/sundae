package cmd

import (
	"bytes"
	"context"
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

// fakeOpenShell is an in-memory openshell.Client.
type fakeOpenShell struct {
	mu        sync.Mutex
	sandboxes map[string]*openshell.Sandbox
	cloned    map[string]bool
	exitCode  int
	output    string
}

var _ openshell.Client = (*fakeOpenShell)(nil)

func newFakeOpenShell() *fakeOpenShell {
	return &fakeOpenShell{sandboxes: map[string]*openshell.Sandbox{}, cloned: map[string]bool{}}
}

func notFound() error { return &types.StatusError{Code: types.ErrorNotFound, Message: "not found"} }

func (f *fakeOpenShell) EnsureSandbox(_ context.Context, name string, req openshell.SandboxRequest) (*openshell.Sandbox, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sb, ok := f.sandboxes[name]; ok {
		return sb, false, nil
	}
	sb := &openshell.Sandbox{Name: name, Labels: req.Labels, Annotations: req.Annotations}
	sb.Status.Phase = types.SandboxReady
	f.sandboxes[name] = sb
	return sb, true, nil
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
	return nil
}

func (f *fakeOpenShell) Run(_ context.Context, name string, command []string, _ openshell.ExecOptions) (*openshell.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch command[0] {
	case "test":
		if !f.cloned[name] {
			return &openshell.ExecResult{ExitCode: 1}, nil
		}
	case "git", "jj":
		f.cloned[name] = true
	}
	return &openshell.ExecResult{}, nil
}

func (f *fakeOpenShell) Interactive(context.Context, string, []string, uint32, uint32, openshell.ExecOptions) (openshell.InteractiveSession, error) {
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

	fc := newFakeOpenShell()
	prev := newOpenShellClient
	newOpenShellClient = func(*config.Config) (openshell.Client, error) { return fc, nil }
	t.Cleanup(func() { newOpenShellClient = prev })

	ctx = withUser(t, ctx, be, "alice", false)
	user := proto.UserFromContext(ctx)
	is.True(user != nil)
	return ctx, be, user, fc
}

func runSession(t *testing.T, ctx context.Context, args ...string) (stdout, stderr string, err error) {
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

	_, stderr, err := runSession(t, ctx, "start", "r")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Starting sandbox soft-alice-r"))
	is.True(strings.Contains(sess.out.String(), "hello from the sandbox"))
	is.True(sess.exited)
	is.Equal(sess.exitCode, 3)

	// A clean exit does not force an exit status.
	fc.exitCode = 0
	sess = &fakeSSHSession{}
	ctx = context.WithValue(ctx, sshutils.ContextKeySession, sess)
	_, _, err = runSession(t, ctx, "start", "r")
	is.NoErr(err)
	is.True(!sess.exited)
}

func TestSessionListAndEnd(t *testing.T) {
	is := is.New(t)
	ctx, _, user, fc := newSessionTestContext(t)

	stdout, _, err := runSession(t, ctx, "ls")
	is.NoErr(err)
	is.True(strings.Contains(stdout, "No sessions found"))

	// A session whose repository has since been deleted.
	name := openshell.SandboxName(user.Username(), "gone")
	sb := &openshell.Sandbox{
		Name:        name,
		Labels:      map[string]string{openshell.LabelUserID: strconv.FormatInt(user.ID(), 10), openshell.LabelManaged: "true"},
		Annotations: map[string]string{openshell.AnnotationRepo: "gone"},
	}
	sb.Status.Phase = types.SandboxReady
	fc.sandboxes[name] = sb

	stdout, _, err = runSession(t, ctx, "ls")
	is.NoErr(err)
	for _, want := range []string{"Repository", "gone", name, "Ready"} {
		is.True(strings.Contains(stdout, want))
	}

	// It can still be ended, and ending again is a no-op.
	_, stderr, err := runSession(t, ctx, "end", "gone")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Session ended"))
	_, ok := fc.sandboxes[name]
	is.True(!ok)

	_, stderr, err = runSession(t, ctx, "end", "gone")
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

	_, _, err = runSession(t, ctx, "start", "r")
	is.NoErr(err)
	_, _, err = runSession(t, ctx, "end", "r")
	is.NoErr(err)
	_, ok := fc.sandboxes["soft-alice-r"]
	is.True(!ok)

	var steps []string
	for range 3 {
		select {
		case ev := <-sub.C:
			is.Equal(ev.Lifecycle.Session.Sandbox, "soft-alice-r")
			steps = append(steps, string(ev.Lifecycle.Step))
		case <-time.After(5 * time.Second):
			t.Fatalf("lifecycle events so far: %v", steps)
		}
	}
	is.Equal(strings.Join(steps, ","), "created,ready,ended")
}
