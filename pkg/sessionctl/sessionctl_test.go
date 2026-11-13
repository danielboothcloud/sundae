package sessionctl

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielboothcloud/sundae/git"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/proto"
)

func TestAttachCommand(t *testing.T) {
	for _, tc := range []struct {
		url, name, want string
	}{
		{"ssh://git.example.com", "claude-1", "ssh -t git.example.com session attach claude-1"},
		{"ssh://git.example.com:22", "", "ssh -t git.example.com session attach main"},
		{"ssh://localhost:23231", "tui", "ssh -t -p 23231 localhost session attach tui"},
		{"ssh://me@host:2222", "x", "ssh -t -p 2222 me@host session attach x"},
		{"git.example.com", "x", "ssh -t git.example.com session attach x"},
	} {
		if got := AttachCommand(tc.url, tc.name); got != tc.want {
			t.Errorf("AttachCommand(%q, %q) = %q, want %q", tc.url, tc.name, got, tc.want)
		}
	}
}

func TestResolveAndDefaultName(t *testing.T) {
	infos := []openshell.Info{
		{Name: "tool", Repos: []string{"github/me/tool"}},
		{Name: "app", Repos: []string{"github/me/app", "github/me/lib"}},
		{Name: "lib", Repos: []string{"x/other"}},
		{Name: "both", Repos: []string{"x/shared"}},
		{Name: "both-2", Repos: []string{"x/shared", "x/more"}},
	}
	for arg, want := range map[string]string{
		"tool":          "tool",
		"github/me/lib": "app",
		"app":           "app",
	} {
		if got, err := resolveIn(infos, arg); err != nil || got != want {
			t.Errorf("resolveIn(%q) = %q, %v; want %q", arg, got, err, want)
		}
	}
	if _, err := resolveIn(infos, "x/shared"); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := resolveIn(infos, "nope"); !errors.Is(err, openshell.ErrNoSession) {
		t.Errorf("absent: %v", err)
	}

	for repo, want := range map[string]string{
		"github/me/tool": "tool",  // the session of that name holds it
		"other/lib":      "lib-2", // "lib" holds something else
		"new/thing":      "thing",
	} {
		if got := DefaultName(infos, repo); got != want {
			t.Errorf("DefaultName(%q) = %q, want %q", repo, got, want)
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

func TestRowParkedToSnapshot(t *testing.T) {
	memory := Row{Info: openshell.Info{Phase: "Suspended"}}
	if !memory.Parked() || memory.ParkedToSnapshot() || memory.Note() != "" || memory.Words() != "none · parked" {
		t.Fatalf("memory park: %v %v %q %q", memory.Parked(), memory.ParkedToSnapshot(), memory.Note(), memory.Words())
	}
	snap := Row{
		Info:   openshell.Info{Phase: openshell.PhaseSnapshot, Snapshot: &openshell.ParkedSnapshot{}},
		Status: &openshell.SessionStatus{Record: events.Record{State: events.StateIdle}},
	}
	if !snap.Parked() || !snap.ParkedToSnapshot() || snap.Words() != "idle · parked (snapshot)" ||
		snap.Note() != "program stopped by the snapshot park; attach restarts it" || snap.Label() != "○ idle · parked (snapshot)" {
		t.Fatalf("snapshot park: %v %v %q %q", snap.Parked(), snap.ParkedToSnapshot(), snap.Note(), snap.Words())
	}
	running := Row{Info: openshell.Info{Phase: "Ready"}}
	if running.Parked() || running.Note() != "" {
		t.Fatal("running row parked")
	}
}

// linkedRepo is a repository whose bare git directory is dir.
type linkedRepo struct {
	proto.Repository
	dir string
}

func (r linkedRepo) Name() string                   { return "r" }
func (r linkedRepo) Open() (*git.Repository, error) { return git.Open(r.dir) }

func TestGitIdentity(t *testing.T) {
	gh := config.GitIdentity{Name: "Jane GH", Email: "1+jane@users.noreply.github.com"}
	def := config.GitIdentity{Name: "Jane", Email: "jane@example.com"}
	cfg := config.DefaultConfig()
	cfg.Forges = []config.ForgeConfig{
		{Name: "github", Kind: "github", GitIdentity: config.ForgeGitIdentity(gh)},
		{Name: "gitlab", Kind: "gitlab"},
	}
	cfg.Sessions.GitIdentity = def

	// repo returns a bare repository linked to forge; "" for none.
	repo := func(t *testing.T, forge string) proto.Repository {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "r.git")
		if _, err := git.Init(dir, true); err != nil {
			t.Fatal(err)
		}
		if forge != "" {
			cfgFile := filepath.Join(dir, "config")
			bts, err := os.ReadFile(cfgFile)
			if err != nil {
				t.Fatal(err)
			}
			link := "[forge]\n\tname = " + forge + "\n\tpath = me/r\n"
			if err := os.WriteFile(cfgFile, append(bts, link...), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return linkedRepo{dir: dir}
	}

	for _, tc := range []struct {
		name  string
		forge string
		cfg   func(*config.Config)
		want  config.GitIdentity
	}{
		{"forge identity", "github", nil, gh},
		{"forge without identity", "gitlab", nil, def},
		{"forge no longer configured", "gone", nil, def},
		{"not imported", "", nil, def},
		{"no default", "", func(c *config.Config) { c.Sessions.GitIdentity = config.GitIdentity{} }, config.GitIdentity{}},
		{"forge identity, no default", "github", func(c *config.Config) { c.Sessions.GitIdentity = config.GitIdentity{} }, gh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := *cfg
			if tc.cfg != nil {
				tc.cfg(&c)
			}
			got, err := GitIdentity(&c, repo(t, tc.forge))
			if err != nil || got != tc.want {
				t.Fatalf("GitIdentity = %v, %v; want %v", got, err, tc.want)
			}
		})
	}

	// An unreadable repository is an error, not the default identity.
	if _, err := GitIdentity(cfg, linkedRepo{dir: filepath.Join(t.TempDir(), "missing.git")}); err == nil {
		t.Fatal("no error for a repository that cannot be opened")
	}
}
