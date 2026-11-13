package openshell

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	"github.com/danielboothcloud/sundae/pkg/events"
)

// withBus attaches a bus to the harness's manager and returns a subscription
// that sees everything published on it.
func withBus(t *testing.T, h *harness) *events.Subscription {
	t.Helper()
	bus := events.New(events.WithLogger(log.New(io.Discard)))
	t.Cleanup(bus.Close)
	h.mgr.bus = bus
	return bus.Subscribe(events.Buffer(64))
}

// drain returns the lifecycle events buffered on s.
func drain(t *testing.T, s *events.Subscription) []events.SessionLifecycle {
	t.Helper()
	var out []events.SessionLifecycle
	for {
		select {
		case ev := <-s.C:
			if ev.Kind != events.KindSessionLifecycle || ev.UserID != alice.ID() || ev.ID == 0 || ev.Time.IsZero() {
				t.Fatalf("unexpected event %+v", ev)
			}
			out = append(out, *ev.Lifecycle)
		default:
			return out
		}
	}
}

func steps(ls []events.SessionLifecycle) string {
	var s []string
	for _, l := range ls {
		s = append(s, string(l.Step))
	}
	return strings.Join(s, ",")
}

func TestStartPublishesLifecycle(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	sub := withBus(t, h)

	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "infra"}); err != nil {
		t.Fatal(err)
	}
	got := drain(t, sub)
	if steps(got) != "created,ready" {
		t.Fatalf("first start = %s", steps(got))
	}
	want := events.Session{User: "alice", Repo: "infra", Name: "main", Sandbox: "sundae-alice-infra"}
	for _, l := range got {
		if l.Session != want || l.Op != "" || l.Reason != "" {
			t.Fatalf("lifecycle = %+v", l)
		}
	}

	// Starting again reuses the sandbox.
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "infra"}); err != nil {
		t.Fatal(err)
	}
	if s := steps(drain(t, sub)); s != "resumed,ready" {
		t.Fatalf("second start = %s", s)
	}
}

func TestStartPublishesResumeOfStoppedSandbox(t *testing.T) {
	h := newHarness(t)
	sub := withBus(t, h)
	h.fc.AddSandbox("default", &types.Sandbox{
		Name:   "sundae-alice-r",
		Labels: ownerLabels(alice),
		Status: types.SandboxStatus{Phase: types.SandboxStopped},
	})
	h.exec.box("sundae-alice-r").dirs["/sandbox/r"] = true
	if _, err := h.mgr.Start(context.Background(), StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	if s := steps(drain(t, sub)); s != "resumed,ready" {
		t.Fatalf("steps = %s", s)
	}
}

func TestStartPublishesFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("clone", func(t *testing.T) {
		h := newHarness(t)
		sub := withBus(t, h)
		h.exec.cloneExit = 128
		if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err == nil {
			t.Fatal("expected error")
		}
		got := drain(t, sub)
		if steps(got) != "created,failed" {
			t.Fatalf("steps = %s", steps(got))
		}
		if f := got[1]; f.Op != "start" || !strings.Contains(f.Reason, "boom") || f.Session.Sandbox != "sundae-alice-r" {
			t.Fatalf("failed = %+v", f)
		}
	})

	t.Run("sandbox never ready", func(t *testing.T) {
		h := newHarness(t)
		sub := withBus(t, h)
		h.mgr.client = newSDKClient("default", failingWait{h.fc.Sandboxes()}, h.exec, h.fc.Files(), nil)
		if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err == nil {
			t.Fatal("expected error")
		}
		if got := drain(t, sub); steps(got) != "failed" || got[0].Reason == "" {
			t.Fatalf("lifecycle = %+v", got)
		}
	})

	t.Run("foreign sandbox", func(t *testing.T) {
		h := newHarness(t)
		sub := withBus(t, h)
		h.fc.AddSandbox("default", &types.Sandbox{
			Name:   "sundae-alice-r",
			Labels: map[string]string{LabelUserID: "99"},
			Status: types.SandboxStatus{Phase: types.SandboxReady},
		})
		if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); !errors.Is(err, ErrNotOwned) {
			t.Fatalf("err = %v", err)
		}
		if got := drain(t, sub); steps(got) != "failed" || got[0].Reason != ErrNotOwned.Error() {
			t.Fatalf("lifecycle = %+v", got)
		}
	})

	t.Run("anonymous publishes nothing", func(t *testing.T) {
		h := newHarness(t)
		sub := withBus(t, h)
		_, _ = h.mgr.Start(ctx, StartOptions{Repo: "r"})
		if got := drain(t, sub); len(got) != 0 {
			t.Fatalf("lifecycle = %+v", got)
		}
	})
}

func TestEndPublishesLifecycle(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	sub := withBus(t, h)
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	drain(t, sub)

	if _, err := h.mgr.End(ctx, alice, "r", ""); err != nil {
		t.Fatal(err)
	}
	got := drain(t, sub)
	if steps(got) != "ended" || got[0].Session.Sandbox != "sundae-alice-r" || got[0].Session.Repo != "r" {
		t.Fatalf("lifecycle = %+v", got)
	}

	// Ending an absent session publishes nothing.
	if _, err := h.mgr.End(ctx, alice, "r", ""); err != nil {
		t.Fatal(err)
	}
	if got := drain(t, sub); len(got) != 0 {
		t.Fatalf("lifecycle = %+v", got)
	}

	// A sandbox that is not alice's fails.
	h.fc.AddSandbox("default", &types.Sandbox{Name: "sundae-alice-x", Labels: map[string]string{LabelUserID: "99"}})
	if _, err := h.mgr.End(ctx, alice, "x", ""); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("err = %v", err)
	}
	if got := drain(t, sub); steps(got) != "failed" || got[0].Op != "end" {
		t.Fatalf("lifecycle = %+v", got)
	}
}

// Managers built without a bus, as every other test here does, publish
// nothing and work as before.
func TestManagerWithoutBus(t *testing.T) {
	h := newHarness(t)
	if h.mgr.bus != nil {
		t.Fatal("harness has a bus")
	}
	if _, err := h.mgr.Start(context.Background(), StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.End(context.Background(), alice, "r", ""); err != nil {
		t.Fatal(err)
	}
	// WithEvents(nil) is the same.
	m := NewManager(h.mgr.client, h.tokens, newHarnessConfig(), WithEvents(nil))
	if _, err := m.Start(context.Background(), StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
}

func TestResumeAndNamedSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	sub := withBus(t, h)

	// Input errors publish nothing: an unknown profile, a bad name, and
	// attaching to a session that does not exist.
	_, _ = h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Profile: "nope"})
	_, _ = h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Name: "Bad"})
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r", Name: "two"}); !errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v", err)
	}
	if got := drain(t, sub); len(got) != 0 {
		t.Fatalf("lifecycle = %+v", got)
	}

	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Name: "two"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r", Name: "two"}); err != nil {
		t.Fatal(err)
	}
	got := drain(t, sub)
	if steps(got) != "created,ready,resumed,ready" {
		t.Fatalf("steps = %s", steps(got))
	}
	want := events.Session{User: "alice", Repo: "r", Name: "two", Sandbox: SessionSandboxName("alice", "r", "two")}
	for _, l := range got {
		if l.Session != want {
			t.Fatalf("session = %+v, want %+v", l.Session, want)
		}
	}

	// A failed attach says so.
	h.exec.probeExit = 1
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r", Name: "two"}); err == nil {
		t.Fatal("expected error")
	}
	got = drain(t, sub)
	if steps(got) != "resumed,failed" || got[1].Op != "attach" || got[1].Session.Name != "two" {
		t.Fatalf("lifecycle = %+v", got)
	}
}
