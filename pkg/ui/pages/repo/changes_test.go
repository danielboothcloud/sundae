package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/git"
	"github.com/danielboothcloud/sundae/pkg/jjview"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/tabs"
)

// fakeRepo is a proto.Repository at a git directory.
type fakeRepo struct {
	name, path string
	vcs        proto.VCS
}

func (r fakeRepo) ID() int64                      { return 1 }
func (r fakeRepo) Name() string                   { return r.name }
func (r fakeRepo) ProjectName() string            { return r.name }
func (r fakeRepo) Description() string            { return "" }
func (r fakeRepo) IsPrivate() bool                { return false }
func (r fakeRepo) IsMirror() bool                 { return false }
func (r fakeRepo) IsHidden() bool                 { return false }
func (r fakeRepo) Vcs() proto.VCS                 { return r.vcs }
func (r fakeRepo) UserID() int64                  { return 0 }
func (r fakeRepo) CreatedAt() time.Time           { return time.Time{} }
func (r fakeRepo) UpdatedAt() time.Time           { return time.Time{} }
func (r fakeRepo) Open() (*git.Repository, error) { return git.Open(r.path) }

// jjRepo is a bare repository pushed from jj: two changes on main.
func jjRepo(t *testing.T) (string, string) {
	t.Helper()
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Skip("jj is not installed")
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "jj.toml")
	if err := os.WriteFile(cfg, []byte("user.name = \"T\"\nuser.email = \"t@example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, bare := filepath.Join(dir, "src"), filepath.Join(dir, "bare.git")
	run := func(d, bin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), bin, args...)
		cmd.Dir = d
		cmd.Env = append(os.Environ(), "JJ_CONFIG="+cfg)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(dir, jj, "git", "init", "--colocate", "src")
	if err := os.WriteFile(filepath.Join(src, "a"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(src, jj, "describe", "-m", "add a")
	run(src, jj, "new", "-m", "second change")
	run(src, "git", "branch", "main", run(src, jj, "log", "-r", "@", "--no-graph", "-T", "commit_id"))
	run(dir, "git", "init", "-q", "--bare", bare)
	run(bare, "git", "fetch", "-q", src, "+main:main")
	return jj, bare
}

// drive runs cmd and feeds its messages back to the tab, a few rounds.
func drive(h *History, cmd tea.Cmd) {
	for range 4 {
		if cmd == nil {
			return
		}
		msg := cmd()
		if b, ok := msg.(tea.BatchMsg); ok {
			var next []tea.Cmd
			for _, c := range b {
				if c == nil {
					continue
				}
				m := c()
				if _, tick := m.(interface{ Tag() int }); tick {
					continue
				}
				_, nc := h.Update(m)
				next = append(next, nc)
			}
			cmd = tea.Batch(next...)
			continue
		}
		_, cmd = h.Update(msg)
	}
}

func TestHistoryShowsJJ(t *testing.T) {
	jj, bare := jjRepo(t)
	c := common.NewCommon(context.Background(), 80, 20)
	h := NewHistory(c, jjview.New(jj, "", t.TempDir()))
	h.SetSize(80, 20)

	if got := h.TabNameFor(proto.VCSJJ); got != "Changes" {
		t.Fatalf("jj tab %q", got)
	}
	if got := h.TabNameFor(proto.VCSGit); got != "Commits" || h.TabName() != "Commits" {
		t.Fatalf("git tab %q", got)
	}

	h.Update(RepoMsg(fakeRepo{name: "r", path: bare, vcs: proto.VCSJJ}))
	if !h.jj {
		t.Fatal("a jj repository is not shown with jj")
	}
	// Listed once the tab is shown.
	_, cmd := h.Update(tabs.ActiveTabMsg(2))
	drive(h, cmd)
	v := ansi.Strip(h.View())
	if !strings.Contains(v, "▸ ") || !strings.Contains(v, "second change") || !strings.Contains(v, "add a") || !strings.Contains(v, "main") {
		t.Fatalf("log:\n%s", v)
	}
	if !strings.HasPrefix(h.StatusBarValue(), "change ") || h.StatusBarInfo() != "1/2" {
		t.Fatalf("status %q %q", h.StatusBarValue(), h.StatusBarInfo())
	}

	// j moves to the next change; enter shows it; h goes back.
	h.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if h.StatusBarInfo() != "2/2" {
		t.Fatalf("after j %q", h.StatusBarInfo())
	}
	_, cmd = h.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	drive(h, cmd)
	v = ansi.Strip(h.View())
	if h.Path() != "change" || !strings.Contains(v, "add a") || !strings.Contains(v, "Change ID:") {
		t.Fatalf("show:\n%s", v)
	}
	h.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
	if h.Path() != "" {
		t.Fatal("h did not go back")
	}

	// A git repository, or a server without jj, gets the git log.
	h.Update(RepoMsg(fakeRepo{name: "g", path: bare, vcs: proto.VCSGit}))
	if h.jj {
		t.Fatal("a git repository is shown with jj")
	}
	off := NewHistory(c, jjview.New("", "", t.TempDir()))
	if off.TabNameFor(proto.VCSJJ) != "Commits" {
		t.Fatal("Changes without jj")
	}
}
