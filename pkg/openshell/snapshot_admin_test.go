package openshell

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danielboothcloud/sundae/pkg/config"
)

// endWithSnapshot starts alice's session name, gives it work and ends it,
// which stores its snapshot.
func endWithSnapshot(t *testing.T, h *snapHarness, name string) {
	t.Helper()
	ctx := context.Background()
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	work(h, s.Sandbox)
	if _, err := h.mgr.EndSession(ctx, alice, name, EndOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotsListDeletePrune(t *testing.T) {
	ctx := context.Background()
	h := newSnapHarness(t, func(c *config.Config) { c.Sessions.Suspend.Fallback = config.SuspendFallbackSnapshot }, false)

	// old: ended; gone: ended; live: has a sandbox and a snapshot taken by
	// an earlier end; parked: parked to its snapshot.
	endWithSnapshot(t, h, "old")
	endWithSnapshot(t, h, "gone")
	endWithSnapshot(t, h, "live")
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Name: "live", Restore: RestoreSkip}); err != nil {
		t.Fatal(err)
	}
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Name: "parked"})
	if err != nil {
		t.Fatal(err)
	}
	work(h, s.Sandbox)
	if _, snap, err := h.mgr.ParkSession(ctx, alice, "parked"); err != nil || !snap {
		t.Fatalf("park = %v %v", snap, err)
	}

	all, err := h.mgr.Snapshots(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, s := range all {
		states[s.Session] = s.State
		if s.Repo != "r" || s.Size == 0 || s.URL == "" || s.Label() != "r / "+s.Session {
			t.Fatalf("snapshot %+v", s)
		}
	}
	want := map[string]string{"old": SnapshotEnded, "gone": SnapshotEnded, "live": SnapshotLive, "parked": SnapshotParked}
	if len(states) != len(want) {
		t.Fatalf("states %v", states)
	}
	for k, v := range want {
		if states[k] != v {
			t.Fatalf("states %v, want %v", states, want)
		}
	}
	// Someone else sees none of them.
	if other, err := h.mgr.Snapshots(ctx, bob); err != nil || len(other) != 0 {
		t.Fatalf("bob sees %v %v", other, err)
	}

	// The snapshot a session is parked to is its only copy: refused.
	if _, err := h.mgr.DeleteSnapshot(ctx, alice, "parked"); !errors.Is(err, ErrSnapshotParked) {
		t.Fatalf("delete parked: %v", err)
	}
	// A live session's snapshot can go; the session stays.
	if got, err := h.mgr.DeleteSnapshot(ctx, alice, "live"); err != nil || got.State != SnapshotLive || got.Size == 0 {
		t.Fatalf("delete live = %+v %v", got, err)
	}
	if !h.exists(t, SessionSandboxName("alice", "live")) {
		t.Fatal("the live session's sandbox went with its snapshot")
	}
	if _, err := h.mgr.DeleteSnapshot(ctx, alice, "live"); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("delete again: %v", err)
	}

	// Prune: only ended sessions' snapshots; --older-than keeps newer ones.
	h.mgr.now = func() time.Time { return time.Now().Add(time.Hour) }
	if got, err := h.mgr.PruneSnapshots(ctx, alice, PruneOptions{OlderThan: 48 * time.Hour}); err != nil || len(got) != 0 {
		t.Fatalf("prune older than 48h = %v %v", got, err)
	}
	got, err := h.mgr.PruneSnapshots(ctx, alice, PruneOptions{DryRun: true})
	if err != nil || len(got) != 2 {
		t.Fatalf("dry run = %v %v", got, err)
	}
	if left, _ := h.mgr.Snapshots(ctx, alice); len(left) != 3 {
		t.Fatalf("dry run deleted: %v", left)
	}
	if got, err = h.mgr.PruneSnapshots(ctx, alice, PruneOptions{OlderThan: time.Minute}); err != nil || len(got) != 2 {
		t.Fatalf("prune = %v %v", got, err)
	}
	left, _ := h.mgr.Snapshots(ctx, alice)
	if len(left) != 1 || left[0].Session != "parked" {
		t.Fatalf("left %v", left)
	}
}

func TestEndDeletesSnapshot(t *testing.T) {
	ctx := context.Background()
	h := newSnapHarness(t, func(c *config.Config) { c.Sessions.Suspend.Fallback = config.SuspendFallbackSnapshot }, false)

	// A running session: no snapshot is taken, the earlier one goes.
	endWithSnapshot(t, h, "main")
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Restore: RestoreSkip})
	if err != nil {
		t.Fatal(err)
	}
	h.lifecycles()
	res, err := h.mgr.EndSession(ctx, alice, "", EndOptions{DeleteSnapshot: true})
	if err != nil || !res.Existed || res.Snapshot != nil || res.SnapshotDeleted == nil {
		t.Fatalf("end = %+v %v", res, err)
	}
	if _, ok := h.s3.Object(snapKey); ok || h.exists(t, s.Sandbox) {
		t.Fatal("snapshot or sandbox kept")
	}

	// A session parked to its snapshot: both the marker and the snapshot go.
	s, err = h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Restore: RestoreSkip})
	if err != nil {
		t.Fatal(err)
	}
	work(h, s.Sandbox)
	if _, _, err := h.mgr.ParkSession(ctx, alice, ""); err != nil {
		t.Fatal(err)
	}
	res, err = h.mgr.EndSession(ctx, alice, "", EndOptions{DeleteSnapshot: true})
	if err != nil || !res.Parked || res.SnapshotDeleted == nil {
		t.Fatalf("end parked = %+v %v", res, err)
	}
	if left, _ := h.mgr.Snapshots(ctx, alice); len(left) != 0 {
		t.Fatalf("left %v", left)
	}

	// Without S3 snapshots it is refused.
	plain := newHarness(t)
	if _, err := plain.mgr.EndSession(ctx, alice, "", EndOptions{DeleteSnapshot: true}); !errors.Is(err, ErrSnapshotsOff) {
		t.Fatalf("snapshots off: %v", err)
	}
	if _, err := plain.mgr.Snapshots(ctx, alice); !errors.Is(err, ErrSnapshotsOff) {
		t.Fatalf("list with snapshots off: %v", err)
	}
}
