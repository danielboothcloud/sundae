package sessions

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/openshell"
)

func snapSource() *fakeSource {
	src := newFakeSource()
	src.snapshots = true
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	src.stored = []openshell.StoredSnapshot{
		{Sandbox: "sd-1", Session: "claude-1", Repo: "infra", State: openshell.SnapshotLive, Size: 2 << 20, Time: now.Add(-time.Hour), URL: "s3://b/1"},
		{Sandbox: "sd-2", Session: "main", Repo: "dotfiles", State: openshell.SnapshotParked, Size: 1 << 20, Time: now.Add(-2 * time.Hour), URL: "s3://b/2"},
		{Sandbox: "sd-3", Session: "old", Repo: "sundae", State: openshell.SnapshotEnded, Size: 5 << 20, Time: now.Add(-72 * time.Hour), URL: "s3://b/3"},
	}
	return src
}

func TestSnapshotsView(t *testing.T) {
	src := snapSource()
	m := loaded(t, 76, 24, src)
	m.now = func() time.Time { return time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC) }

	// S and D are offered with snapshots on, in the menu and full help.
	var acts []string
	for _, a := range m.Actions() {
		acts = append(acts, a.Help().Key)
	}
	if got := strings.Join(acts, ","); !strings.Contains(got, "D,S") {
		t.Fatalf("actions %s", got)
	}

	run(m, keys(m, "S"), 1)
	if m.snaps == nil || !m.Capturing() {
		t.Fatal("S did not open the snapshots")
	}
	golden(t, "snapshots-76", view(m))

	// The parked session's snapshot is refused, with why.
	keys(m, "j")
	keys(m, "x")
	if m.confirm != nil || !strings.Contains(view(m), "parked to this snapshot") {
		t.Fatalf("parked delete:\n%s", view(m))
	}
	// A live session's: asks, then deletes.
	keys(m, "k", "x")
	if m.confirm == nil || !strings.Contains(ansi.Strip(m.FooterLine(120)), "The session keeps running") {
		t.Fatalf("confirm %q", ansi.Strip(m.FooterLine(120)))
	}
	run(m, keys(m, "y"), 2)
	if strings.Join(src.deletedSnaps, ",") != "claude-1" || !strings.Contains(view(m), "✓ Deleted the snapshot of infra / claude-1") {
		t.Fatalf("deleted %v:\n%s", src.deletedSnaps, view(m))
	}
	// P prunes ended sessions' snapshots, after asking.
	keys(m, "P")
	if m.confirm == nil || !strings.Contains(ansi.Strip(m.FooterLine(120)), "Delete the 1 snapshot(s) of ended sessions?") {
		t.Fatalf("prune confirm %q", ansi.Strip(m.FooterLine(120)))
	}
	run(m, keys(m, "y"), 2)
	if src.pruned != 1 || !strings.Contains(view(m), "✓ Pruned 1 snapshot(s), 5.0 MiB: sundae / old.") {
		t.Fatalf("pruned %d:\n%s", src.pruned, view(m))
	}
	if len(m.snaps.rows) != 1 {
		t.Fatalf("rows after prune %v", m.snaps.rows)
	}
	// esc goes back to the sessions.
	keys(m, "esc")
	if m.snaps != nil || m.Capturing() {
		t.Fatal("esc did not close")
	}
}

func TestEndAndDeleteSnapshot(t *testing.T) {
	src := snapSource()
	m := loaded(t, 76, 24, src)
	keys(m, "D")
	if m.confirm == nil || !strings.Contains(ansi.Strip(m.FooterLine(120)), "and delete its S3 snapshot?") {
		t.Fatalf("confirm %q", ansi.Strip(m.FooterLine(120)))
	}
	run(m, keys(m, "y"), 2)
	if strings.Join(src.endDeleted, ",") != "claude-1" || !strings.Contains(view(m), "Ended infra / claude-1 and deleted its snapshot.") {
		t.Fatalf("end-deleted %v:\n%s", src.endDeleted, view(m))
	}

	// Without snapshots, S and D do nothing and are not offered.
	plain := loaded(t, 76, 24, newFakeSource())
	keys(plain, "S")
	keys(plain, "D")
	if plain.snaps != nil || plain.confirm != nil {
		t.Fatal("S or D acted with snapshots off")
	}
	for _, a := range plain.Actions() {
		if k := a.Help().Key; k == "S" || k == "D" {
			t.Fatalf("%s offered with snapshots off", k)
		}
	}
}
