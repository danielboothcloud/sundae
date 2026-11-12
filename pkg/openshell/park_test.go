package openshell

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/fake"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
)

// suspendRecorder is the fake's sandbox client, recording resume options.
// Its WaitSuspended completes a suspend in progress, as the gateway would.
type suspendRecorder struct {
	v1.SandboxInterface
	sr v1.SandboxSuspendResumeInterface
	fc *fake.Client

	mu      sync.Mutex
	resumes []v1.ResumeOptions
	waits   int
}

func newSuspendRecorder(fc *fake.Client) *suspendRecorder {
	return &suspendRecorder{SandboxInterface: fc.Sandboxes(), sr: fc.SandboxSuspendResume(), fc: fc}
}

func (s *suspendRecorder) Suspend(ctx context.Context, ws, name string) (*Sandbox, error) {
	return s.sr.Suspend(ctx, ws, name) //nolint:wrapcheck
}

func (s *suspendRecorder) Resume(ctx context.Context, ws, name string, opts ...v1.ResumeOptions) (*Sandbox, error) {
	s.mu.Lock()
	s.resumes = append(s.resumes, opts...)
	s.mu.Unlock()
	return s.sr.Resume(ctx, ws, name, opts...) //nolint:wrapcheck
}

func (s *suspendRecorder) WaitSuspended(ctx context.Context, ws, name string, opts ...v1.WaitOptions) (*Sandbox, error) {
	s.mu.Lock()
	s.waits++
	s.mu.Unlock()
	if sb, err := s.Get(ctx, ws, name); err == nil && sb.Status.Phase == types.SandboxSuspending {
		sb.Status.Phase = types.SandboxSuspended
		s.fc.AddSandbox(ws, sb)
	}
	return s.sr.WaitSuspended(ctx, ws, name, opts...) //nolint:wrapcheck
}

// noSuspend hides the fake's suspend support.
type noSuspend struct{ v1.SandboxInterface }

func TestSuspendAndResumeSandbox(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClient()
	rec := newSuspendRecorder(fc)
	c := newSDKClient("default", rec, nil, nil, nil)
	labels := map[string]string{LabelUserID: "1"}
	req := SandboxRequest{Labels: labels}
	add := func(name string, phase types.SandboxPhase) {
		fc.AddSandbox("default", &types.Sandbox{Name: name, Labels: labels, Status: types.SandboxStatus{Phase: phase}})
	}

	// Suspend waits until the sandbox is Suspended.
	add("soft-a-r", types.SandboxReady)
	sb, err := c.SuspendSandbox(ctx, "soft-a-r")
	if err != nil || sb.Status.Phase != types.SandboxSuspended || rec.waits != 1 {
		t.Fatalf("suspend: sb=%v err=%v waits=%d", sb, err, rec.waits)
	}

	// EnsureSandbox and ResumeSandbox resume a Suspended sandbox, always
	// with RestartIfRestoreFails.
	sb, created, err := c.EnsureSandbox(ctx, "soft-a-r", req)
	if err != nil || created || sb.Status.Phase != types.SandboxReady {
		t.Fatalf("ensure suspended: sb=%v created=%v err=%v", sb, created, err)
	}
	add("soft-a-s", types.SandboxSuspended)
	if sb, err := c.ResumeSandbox(ctx, "soft-a-s", labels); err != nil || sb.Status.Phase != types.SandboxReady {
		t.Fatalf("resume suspended: sb=%v err=%v", sb, err)
	}
	if len(rec.resumes) != 2 || !rec.resumes[0].RestartIfRestoreFails || !rec.resumes[1].RestartIfRestoreFails {
		t.Fatalf("resume options = %+v", rec.resumes)
	}

	// Suspending: wait for the suspend to finish, then resume.
	add("soft-a-t", types.SandboxSuspending)
	waits := rec.waits
	if sb, err := c.ResumeSandbox(ctx, "soft-a-t", labels); err != nil || sb.Status.Phase != types.SandboxReady {
		t.Fatalf("resume suspending: sb=%v err=%v", sb, err)
	}
	if rec.waits != waits+1 || len(rec.resumes) != 3 {
		t.Fatalf("suspending: waits=%d resumes=%d", rec.waits, len(rec.resumes))
	}

	// Suspending a stopped sandbox is the gateway's error, not
	// "unsupported".
	add("soft-a-u", types.SandboxStopped)
	if _, err := c.SuspendSandbox(ctx, "soft-a-u"); err == nil || IsSuspendUnsupported(err) {
		t.Fatalf("suspend stopped: %v", err)
	}

	// A client without suspend support says so.
	plain := newSDKClient("default", noSuspend{fc.Sandboxes()}, nil, nil, nil)
	if _, err := plain.SuspendSandbox(ctx, "soft-a-r"); !errors.Is(err, ErrSuspendUnsupported) {
		t.Fatalf("plain suspend: %v", err)
	}
	add("soft-a-v", types.SandboxSuspended)
	if _, err := plain.ResumeSandbox(ctx, "soft-a-v", labels); !errors.Is(err, ErrSuspendUnsupported) {
		t.Fatalf("plain resume: %v", err)
	}
}

// parkSession suspends the sandbox of a started session.
func parkSession(t *testing.T, h *harness, sandbox string) {
	t.Helper()
	if _, err := h.mgr.client.SuspendSandbox(context.Background(), sandbox); err != nil {
		t.Fatal(err)
	}
}

// start and attach resume a parked session; with its memory restored the
// program is still running.
func TestStartAndAttachResumeParkedSession(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	sub := withBus(t, h)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, sub)

	for _, op := range []string{"start", "attach"} {
		parkSession(t, h, s.Sandbox)
		var progress bytes.Buffer
		opts := StartOptions{User: alice, Repo: "r", Progress: &progress}
		var again *Session
		if op == "start" {
			again, err = h.mgr.Start(ctx, opts)
		} else {
			again, err = h.mgr.Resume(ctx, opts)
		}
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if !again.Unparked || again.Started || again.OsewaID != s.OsewaID || again.Created {
			t.Fatalf("%s: %+v", op, again)
		}
		if !strings.Contains(progress.String(), "Session main was parked; resuming it") || strings.Contains(progress.String(), "Started") {
			t.Fatalf("%s: progress %q", op, progress.String())
		}
		sb, _ := h.fc.Sandboxes().Get(ctx, "default", s.Sandbox)
		if sb.Status.Phase != types.SandboxReady {
			t.Fatalf("%s: phase %s", op, sb.Status.Phase)
		}
		if got := steps(drain(t, sub)); got != "resumed,ready" {
			t.Fatalf("%s: lifecycle %s", op, got)
		}
	}

	// Not parked: no word about it.
	var progress bytes.Buffer
	again, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r", Progress: &progress})
	if err != nil || again.Unparked || strings.Contains(progress.String(), "parked") {
		t.Fatalf("not parked: %+v %v %q", again, err, progress.String())
	}
}

// When the backend cannot restore the sandbox's memory it restarts it
// cleanly (RestartIfRestoreFails): osewa's server and session are gone, so
// attach starts them again, and says why.
func TestAttachAfterCleanRestoreRestartsProgram(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	parkSession(t, h, s.Sandbox)
	// The restart lost every process; /sandbox (the checkout, osewa's
	// files, the recorded session id) is still there.
	h.exec.mu.Lock()
	b := h.exec.box(s.Sandbox)
	b.server = false
	b.sessions = map[string]bool{}
	h.exec.mu.Unlock()

	var progress bytes.Buffer
	again, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r", Progress: &progress})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Unparked || !again.Started || again.OsewaID == s.OsewaID {
		t.Fatalf("session = %+v", again)
	}
	if !strings.Contains(progress.String(), "Started bash -l again: it was not running when the session resumed") ||
		!strings.Contains(progress.String(), "could not restore its memory and restarted cleanly") {
		t.Fatalf("progress %q", progress.String())
	}
	h.exec.mu.Lock()
	servers := h.exec.box(s.Sandbox).servers
	h.exec.mu.Unlock()
	if servers != 2 {
		t.Fatalf("osewa servers started = %d", servers)
	}
	if len(h.exec.clones()) != 1 {
		t.Fatal("cloned again")
	}
}

func TestManagerPark(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	sub := withBus(t, h)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Name: "two"})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, sub)

	parked, err := h.mgr.Park(ctx, alice, "r", "two")
	if err != nil || !parked {
		t.Fatalf("park: %v %v", parked, err)
	}
	sb, _ := h.fc.Sandboxes().Get(ctx, "default", s.Sandbox)
	if sb.Status.Phase != types.SandboxSuspended {
		t.Fatalf("phase %s", sb.Status.Phase)
	}
	got := drain(t, sub)
	if steps(got) != "parked" || got[0].Op != ParkManual || got[0].Session.Name != "two" || got[0].Session.Sandbox != s.Sandbox {
		t.Fatalf("lifecycle %+v", got)
	}
	// Parking again is a no-op.
	if parked, err := h.mgr.Park(ctx, alice, "r", "two"); err != nil || parked {
		t.Fatalf("park again: %v %v", parked, err)
	}
	if got := drain(t, sub); len(got) != 0 {
		t.Fatalf("lifecycle %+v", got)
	}

	// Absent, someone else's, not running, not Soft Serve's, bad name.
	if _, err := h.mgr.Park(ctx, alice, "r", "nope"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("absent: %v", err)
	}
	if _, err := h.mgr.Park(ctx, testUser{id: 9, name: "alice"}, "r", "two"); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("foreign: %v", err)
	}
	h.fc.AddSandbox("default", &types.Sandbox{
		Name:   SessionSandboxName("alice", "r", "stopped"),
		Labels: ownerLabels(alice), Annotations: sessionAnnotations("alice", "r", "stopped", ""),
		Status: types.SandboxStatus{Phase: types.SandboxStopped},
	})
	if _, err := h.mgr.Park(ctx, alice, "r", "stopped"); err == nil || !strings.Contains(err.Error(), "only a running session can be parked") {
		t.Fatalf("stopped: %v", err)
	}
	h.fc.AddSandbox("default", &types.Sandbox{
		Name:        SessionSandboxName("alice", "r", "foreign"),
		Labels:      map[string]string{LabelUserID: "1"}, // not managed
		Annotations: map[string]string{AnnotationRepo: "r"},
		Status:      types.SandboxStatus{Phase: types.SandboxReady},
	})
	if _, err := h.mgr.Park(ctx, alice, "r", "foreign"); err == nil || !strings.Contains(err.Error(), "not created by Soft Serve") {
		t.Fatalf("unmanaged: %v", err)
	}
	if _, err := h.mgr.Park(ctx, alice, "r", "Bad"); !errors.Is(err, ErrInvalidSessionName) {
		t.Fatalf("bad name: %v", err)
	}
	if _, err := h.mgr.Park(ctx, nil, "r", ""); err == nil {
		t.Fatal("anonymous park")
	}

	// With the server's Monitor, an open bridge refuses the park, and a
	// gateway that cannot suspend is reported.
	mon := NewMonitor(func() (Client, error) { return h.mgr.client, nil }, newHarnessConfig())
	h.mgr.monitor = mon
	main, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	closeBridge := mon.OpenBridge(ctx, main.Sandbox)
	if _, err := h.mgr.Park(ctx, alice, "r", ""); !errors.Is(err, ErrBridgeOpen) {
		t.Fatalf("bridge open: %v", err)
	}
	closeBridge()
	mon.unsuspendable[main.Sandbox] = "compute driver does not support sandbox suspend"
	if _, err := h.mgr.Park(ctx, alice, "r", ""); !IsSuspendUnsupported(err) || !strings.Contains(err.Error(), "cannot suspend") {
		t.Fatalf("unsupported: %v", err)
	}
	delete(mon.unsuspendable, main.Sandbox)
	if parked, err := h.mgr.Park(ctx, alice, "r", ""); err != nil || !parked {
		t.Fatalf("park main: %v %v", parked, err)
	}
	if got := drain(t, sub); steps(got) != "created,ready,parked" {
		t.Fatalf("lifecycle %s", steps(got))
	}
}
