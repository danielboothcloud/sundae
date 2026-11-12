package sessionctl

import (
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/events"
	"github.com/charmbracelet/soft-serve/pkg/openshell"
)

func TestAttachCommand(t *testing.T) {
	for _, tc := range []struct {
		url, repo, name, want string
	}{
		{"ssh://git.example.com", "infra", "claude-1", "ssh -t git.example.com session attach infra -n claude-1"},
		{"ssh://git.example.com:22", "infra", "", "ssh -t git.example.com session attach infra -n main"},
		{"ssh://localhost:23231", "soft-serve", "tui", "ssh -t -p 23231 localhost session attach soft-serve -n tui"},
		{"ssh://me@host:2222", "r", "x", "ssh -t -p 2222 me@host session attach r -n x"},
		{"git.example.com", "r", "x", "ssh -t git.example.com session attach r -n x"},
	} {
		if got := AttachCommand(tc.url, tc.repo, tc.name); got != tc.want {
			t.Errorf("AttachCommand(%q, %q, %q) = %q, want %q", tc.url, tc.repo, tc.name, got, tc.want)
		}
	}
}

func TestSince(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		10 * time.Second: "now",
		4 * time.Minute:  "4m",
		3 * time.Hour:    "3h",
		72 * time.Hour:   "3d",
	} {
		if got := Since(now.Add(-d), now); got != want {
			t.Errorf("Since(-%s) = %q, want %q", d, got, want)
		}
	}
	if got := Since(time.Time{}, now); got != "-" {
		t.Errorf("Since(zero) = %q", got)
	}
}

func TestRowWords(t *testing.T) {
	forty := 40
	for _, tc := range []struct {
		row  Row
		want string
	}{
		{Row{}, "none"},
		{Row{
			Info:   openshell.Info{Phase: "Suspended"},
			Status: &openshell.SessionStatus{Record: events.Record{State: events.StateBlocked, Kind: events.BlockPermission}},
		}, "blocked · permission · parked"},
		{Row{Status: &openshell.SessionStatus{Record: events.Record{State: events.StateWorking, Progress: &forty}}}, "working 40%"},
		{Row{Status: &openshell.SessionStatus{Exited: true, Stale: true}}, "none · exited · stale"},
	} {
		if got := tc.row.Words(); got != tc.want {
			t.Errorf("Words() = %q, want %q", got, tc.want)
		}
	}
	r := Row{Status: &openshell.SessionStatus{Record: events.Record{State: events.StateDone}}}
	if got := r.Label(); got != "✓ done" {
		t.Errorf("Label() = %q", got)
	}
}
