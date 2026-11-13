package sessions

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/events"
	"github.com/charmbracelet/soft-serve/pkg/openshell"
	"github.com/charmbracelet/soft-serve/pkg/sessionctl"
	"github.com/charmbracelet/x/ansi"
)

func TestEndWithSnapshot(t *testing.T) {
	src := newFakeSource()
	src.snapshots = true
	m := loaded(t, 120, 30, src)
	keys(m, "x")
	if line := ansi.Strip(m.FooterLine(120)); !strings.HasPrefix(line, "End infra / claude-1?  /sandbox is saved to S3, then the sandbox is deleted.") {
		t.Fatalf("confirm line %q", line)
	}

	// The snapshot fails: not ended; offered to end without one.
	src.endErr = &openshell.SnapshotError{Repo: "infra", Name: "claude-1", Err: errors.New("S3 refused the upload: HTTP 403")}
	run(m, keys(m, "y"), 2)
	if len(src.ended) != 0 {
		t.Fatalf("ended %v", src.ended)
	}
	if line := ansi.Strip(m.FooterLine(120)); !strings.HasPrefix(line, "The snapshot failed. End infra / claude-1 without one?  Unpushed work is lost.") {
		t.Fatalf("force confirm %q", line)
	}
	if !strings.Contains(view(m), "HTTP 403") {
		t.Fatalf("notice:\n%s", view(m))
	}
	run(m, keys(m, "y"), 2)
	if fmt.Sprint(src.ended) != "[infra/claude-1 (force)]" {
		t.Fatalf("ended %v", src.ended)
	}

	// n leaves it.
	src.ended = nil
	run(m, keys(m, "x", "y"), 2)
	keys(m, "n")
	if len(src.ended) != 0 || m.confirm != nil {
		t.Fatalf("ended %v", src.ended)
	}
}

func parkedToSnapshot(r *sessionctl.Row) {
	r.Info.Phase = openshell.PhaseSnapshot
	r.Info.Snapshot = &openshell.ParkedSnapshot{Repo: r.Info.Repo, Session: r.Info.Name, Size: 1 << 20}
}

func TestParkedToSnapshotRow(t *testing.T) {
	src := newFakeSource()
	src.rows = []sessionctl.Row{row("dotfiles", "main", events.StateIdle, 3*time.Hour, "waiting for input", parkedToSnapshot)}
	m := loaded(t, 100, 20, src)
	v := view(m)
	if !strings.Contains(v, "idle · parked (snapshot)") || !strings.Contains(v, "program stopped by the snapshot park; attach restarts it · waiting for input") {
		t.Fatalf("view:\n%s", v)
	}
	r, _ := m.selected()
	if !r.Parked() || !r.ParkedToSnapshot() {
		t.Fatal("not parked")
	}
	keys(m, "x")
	if line := ansi.Strip(m.FooterLine(100)); !strings.HasPrefix(line, "End dotfiles / main?  It is parked to an S3 snapshot, which is kept.") {
		t.Fatalf("confirm %q", line)
	}
}

func TestRestoreConflictConfirm(t *testing.T) {
	conflict := &openshell.RestoreConflictError{
		Repo: "dotfiles", Name: "main", Paths: []string{"/sandbox/dotfiles"},
		Snapshot: openshell.SnapshotInfo{Size: 2 << 20, Time: time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)},
	}

	// Resuming a parked session.
	src := newFakeSource()
	src.prepErrs = []error{conflict}
	m := loaded(t, 120, 30, src)
	keys(m, "G", "k")
	run(m, keys(m, "z"), 1)
	line := ansi.Strip(m.FooterLine(120))
	if !strings.HasPrefix(line, "Restore its snapshot (2026-11-12 09:30 UTC, 2.0 MiB) over files already in the new sandbox?") {
		t.Fatalf("confirm %q", line)
	}
	if !strings.Contains(view(m), "the new sandbox already holds /sandbox/dotfiles") {
		t.Fatalf("notice:\n%s", view(m))
	}
	run(m, keys(m, "y"), 1)
	if len(src.prepared) != 2 || src.prepared[0].Restore != openshell.RestoreAuto || src.prepared[1].Restore != openshell.RestoreOver {
		t.Fatalf("prepared %+v", src.prepared)
	}
	if !strings.Contains(view(m), "Resumed dotfiles / main.") {
		t.Fatalf("notice:\n%s", view(m))
	}

	// Starting a new one; no starts nothing.
	src = newFakeSource()
	src.prepErrs = []error{conflict}
	m = loaded(t, 120, 30, src)
	run(m, keys(m, "n"), 1)
	keys(m, "down", "down", "down", "left") // do not attach
	run(m, keys(m, "enter"), 3)
	if m.confirm == nil || m.op != nil {
		t.Fatalf("no confirm:\n%s", view(m))
	}
	keys(m, "n")
	if len(src.prepared) != 1 || m.op != nil {
		t.Fatalf("prepared %+v", src.prepared)
	}
	// Yes starts it again, restoring over the files.
	src.prepErrs = []error{conflict}
	run(m, keys(m, "n"), 1)
	keys(m, "down", "down", "down", "left")
	run(m, keys(m, "enter"), 3)
	run(m, keys(m, "y"), 3)
	if len(src.prepared) != 3 || src.prepared[2].Restore != openshell.RestoreOver || !src.prepared[2].Create {
		t.Fatalf("prepared %+v", src.prepared)
	}
	if !strings.Contains(view(m), "Started infra / claude-2.") {
		t.Fatalf("notice:\n%s", view(m))
	}
}
