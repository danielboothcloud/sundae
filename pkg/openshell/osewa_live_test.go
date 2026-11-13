package openshell

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/events"
)

// localClient runs a sandbox's commands on this machine, with HOME (and
// OSEWA_HOME) set to home instead of /sandbox. It lists one Ready sandbox.
type localClient struct {
	home, bin, sandbox string
	// supervisorEnv is what the OpenShell supervisor adds to every exec's
	// environment, such as provider placeholders; the exec request's own
	// environment wins over it, as in openshell-sandbox 0.1.8
	// (boundary_exec.rs, LocalBoundaryExec::command).
	supervisorEnv map[string]string
}

func (c *localClient) Run(ctx context.Context, _ string, command []string, opts ExecOptions) (*ExecResult, error) {
	if err := gatewayLimits(command, opts); err != nil {
		return nil, err
	}
	env := maps.Clone(c.supervisorEnv)
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, opts.Env)
	env["HOME"], env["OSEWA_HOME"] = c.home, c.home
	env["PATH"] = c.bin + ":/usr/bin:/bin"
	prog := command[0]
	if p := filepath.Join(c.bin, prog); !strings.Contains(prog, "/") {
		if _, err := os.Stat(p); err == nil {
			// As the PATH below would find it.
			prog = p
		}
	}
	cmd := exec.CommandContext(ctx, prog, command[1:]...)
	cmd.Dir = c.home
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return &ExecResult{ExitCode: exit.ExitCode(), Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
	case err != nil:
		return nil, err
	}
	return &ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
}

func (c *localClient) ListSandboxes(context.Context, map[string]string) ([]*Sandbox, error) {
	sb := &Sandbox{
		Name:        c.sandbox,
		Labels:      map[string]string{LabelManaged: "true", LabelUserID: "1"},
		Annotations: map[string]string{AnnotationUser: "alice", AnnotationRepo: "r", AnnotationSession: "main"},
	}
	sb.Status.Phase = types.SandboxReady
	return []*Sandbox{sb}, nil
}

func (c *localClient) EnsureSandbox(context.Context, string, SandboxRequest) (*Sandbox, bool, error) {
	return nil, false, errors.ErrUnsupported
}

func (c *localClient) GetSandbox(context.Context, string) (*Sandbox, error) {
	return nil, errors.ErrUnsupported
}

func (c *localClient) ResumeSandbox(context.Context, string, map[string]string) (*Sandbox, error) {
	return nil, errors.ErrUnsupported
}
func (c *localClient) DeleteSandbox(context.Context, string) error { return errors.ErrUnsupported }
func (c *localClient) SuspendSandbox(context.Context, string) (*Sandbox, error) {
	return nil, ErrSuspendUnsupported
}

func (c *localClient) SyncProviders(context.Context, *Sandbox, []string) (ProviderChanges, error) {
	return ProviderChanges{}, errors.ErrUnsupported
}

func (c *localClient) Upload(context.Context, string, string, string) error {
	return errors.ErrUnsupported
}

func (c *localClient) Interactive(context.Context, string, []string, uint32, uint32, ExecOptions) (InteractiveSession, error) {
	return nil, errors.ErrUnsupported
}
func (c *localClient) Close() error { return nil }

// TestOsewaLive runs Sundae's osewa bootstrap and status poll against a
// real osewa build for this machine, given in SUNDAE_TEST_OSEWA (it must
// be the pinned version). The session's program reports OSC 7501 states
// one after another and exits 7; the Monitor must see each of them.
func TestOsewaLive(t *testing.T) {
	bin := os.Getenv("SUNDAE_TEST_OSEWA")
	if bin == "" {
		t.Skip("set SUNDAE_TEST_OSEWA to an osewa " + OsewaVersion + " binary for this machine")
	}
	bin, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	// Short paths: osewa's holder sockets live under HOME, and a Unix
	// socket path is limited to about 100 bytes.
	home, err := os.MkdirTemp("/tmp", "ssw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, filepath.Join(binDir, "osewa")); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(home, "r")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}

	// The scripts take Sundae's paths in the sandbox as arguments.
	for _, v := range []struct {
		p   *string
		val string
	}{
		{&osewaInstallDir, filepath.Join(home, "not-installed")}, // poll falls back to PATH
		{&osewaClientDir, filepath.Join(home, ".config/sundae/osewa-client")},
		{&osewaSessionFile, filepath.Join(home, ".config/sundae/osewa-session")},
	} {
		old := *v.p
		*v.p = v.val
		t.Cleanup(func() { *v.p = old })
	}

	var lc0 net.ListenConfig
	l, err := lc0.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	lc := &localClient{home: home, bin: binDir, sandbox: "sundae-alice-r"}
	m := &Manager{client: lc, osewaPort: port}
	t.Cleanup(func() {
		_ = exec.CommandContext(context.Background(), "pkill", "-f", "--", "--listen 127.0.0.1:"+strconv.Itoa(port)).Run()
	})
	if err := m.startOsewaServer(ctx, lc.sandbox, filepath.Join(binDir, "osewa")); err != nil {
		t.Fatal(err)
	}
	if err := m.enrollBridge(ctx, lc.sandbox, filepath.Join(binDir, "osewa")); err != nil {
		t.Fatal(err)
	}

	mon := NewMonitor(func() (Client, error) { return lc, nil }, config.DefaultConfig(),
		WithMonitorLogger(log.New(io.Discard)))
	bus := events.New(events.WithLogger(log.New(io.Discard)))
	defer bus.Close()
	sub := bus.Subscribe(events.Buffer(64))
	mon.bus = bus
	mon.Poll(ctx) // baseline: no session yet
	if st, ok := mon.Status(lc.sandbox); !ok || st.Error != "" || st.OsewaID != "" {
		t.Fatalf("before the session: %+v", st)
	}

	program := `emit() { printf '\033]7501;%s\033\\' "$1"; }
wait_for() { while [ ! -e "$1" ]; do sleep 0.05; done; }
emit 'state=working:app=sundae-test:progress=40:msg=QnVpbGRpbmc='
wait_for step1
emit 'state=blocked:kind=permission:app=sundae-test:msg=QXBwbHkgMyBjaGFuZ2VzPw=='
wait_for step2
emit 'state=done:app=sundae-test:title=VGVzdHM=:msg=QWxsIHRlc3RzIHBhc3M='
wait_for step3
exit 7`
	id, started, err := m.ensureOsewaSession(ctx, lc.sandbox, filepath.Join(binDir, "osewa"), work, []string{"/bin/sh", "-c", program}, false)
	if err != nil || !started {
		t.Fatalf("session: id=%q started=%v err=%v", id, started, err)
	}
	t.Cleanup(func() {
		_, _ = lc.Run(context.Background(), lc.sandbox, []string{filepath.Join(binDir, "osewa"), "local", "kill", id}, ExecOptions{Env: osewaEnv()})
	})

	// await polls until the session's status satisfies ok.
	await := func(what string, ok func(SessionStatus) bool) SessionStatus {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			mon.Poll(ctx)
			st, _ := mon.Status(lc.sandbox)
			if ok(st) {
				return st
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: last status %+v", what, st)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	raw := func() string {
		res, err := lc.Run(ctx, lc.sandbox, []string{"sh", "-c", osewaStatusScript, osewaStatusName, osewaPath(), osewaSessionFile}, ExecOptions{Env: osewaEnv()})
		if err != nil {
			t.Fatal(err)
		}
		return string(res.Stdout)
	}
	next := func(want events.State) events.SessionStatus {
		t.Helper()
		for {
			select {
			case ev := <-sub.C:
				if ev.Status != nil && ev.Status.Record.State == want {
					return *ev.Status
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no %s status event", want)
			}
		}
	}

	st := await("working", func(st SessionStatus) bool { return st.Record.State == events.StateWorking })
	t.Logf("osewa local ls, working: %s", raw())
	if st.OsewaID != id || !st.Running || st.Record.App != "sundae-test" || st.Record.Msg != "Building" ||
		st.Record.Progress == nil || *st.Record.Progress != 40 || st.Attached != 0 {
		t.Fatalf("working: %+v", st)
	}
	if ev := next(events.StateWorking); ev.Previous != events.StateNone || ev.Session.Repo != "r" {
		t.Fatalf("working event: %+v", ev)
	}

	touch := func(name string) {
		if err := os.WriteFile(filepath.Join(work, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	touch("step1")
	st = await("blocked", func(st SessionStatus) bool { return st.Record.State == events.StateBlocked })
	t.Logf("osewa local ls, blocked: %s", raw())
	if st.Record.Kind != events.BlockPermission || st.Record.Msg != "Apply 3 changes?" || st.Record.Progress != nil {
		t.Fatalf("blocked: %+v", st)
	}
	if ev := next(events.StateBlocked); ev.Previous != events.StateWorking {
		t.Fatalf("blocked event: %+v", ev)
	}

	touch("step2")
	st = await("done", func(st SessionStatus) bool { return st.Record.State == events.StateDone })
	if st.Record.Title != "Tests" || st.Record.Msg != "All tests pass" {
		t.Fatalf("done: %+v", st)
	}
	if ev := next(events.StateDone); ev.Previous != events.StateBlocked {
		t.Fatalf("done event: %+v", ev)
	}

	touch("step3")
	st = await("exited", func(st SessionStatus) bool { return st.Exited || !st.Running })
	t.Logf("osewa local ls, exited: %s", raw())
	t.Logf("status after exit: %+v", st)
	if st.Exited && (st.ExitCode == nil || *st.ExitCode != 7) {
		t.Fatalf("exit code: %+v", st)
	}
	for {
		select {
		case ev := <-sub.C:
			if ev.Lifecycle != nil && ev.Lifecycle.Step == events.LifecycleExited {
				if st.Exited && (ev.Lifecycle.ExitCode == nil || *ev.Lifecycle.ExitCode != 7) {
					t.Fatalf("exited event: %+v", ev.Lifecycle)
				}
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no exited event")
		}
	}
}

var _ Client = (*localClient)(nil)
