package ssh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/forge"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/forges"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/selection"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/sessions"
)

func intp(n int) *int { return &n }

// fakeForges is a Forges tab Source and an origin badge source: one
// forge with an imported repository and one that is not.
type fakeForges struct {
	mu      sync.Mutex
	origins map[string]forge.Origin
	pushes  []string
	feed    *chanFeed
}

func newFakeForges() *fakeForges {
	return &fakeForges{
		origins: map[string]forge.Origin{"sundae": {Forge: "github", Path: "me/sundae", Ahead: intp(3), Behind: intp(0)}},
		feed:    &chanFeed{ch: make(chan []events.Event, 4)},
	}
}

func (f *fakeForges) Status(context.Context) ([]forge.ForgeStatus, error) {
	synced := time.Now().Add(-3 * time.Minute)
	return []forge.ForgeStatus{{Name: "github", Kind: "github", URL: "https://github.com", Imported: 1, Listed: intp(2), Matching: intp(2), LastSync: &synced}}, nil
}

func (f *fakeForges) Repos(context.Context, string) ([]forge.RepoStatus, error) {
	synced := time.Now().Add(-3 * time.Minute)
	return []forge.RepoStatus{
		{Path: "me/dotfiles", Repo: "github/me/dotfiles", Listed: true},
		{Path: "me/sundae", Repo: "sundae", Listed: true, Imported: true, Ahead: intp(3), Behind: intp(0), LastSync: &synced},
	}, nil
}
func (f *fakeForges) Discover(context.Context, string) error { return nil }
func (f *fakeForges) Import(context.Context, string, []string, proto.VCS) ([]forge.ImportResult, error) {
	return nil, nil
}

func (f *fakeForges) SyncForge(context.Context, string) ([]forge.RepoSyncResult, error) {
	return nil, nil
}

func (f *fakeForges) SyncRepo(context.Context, string, string) (forge.RepoSyncResult, error) {
	return forge.RepoSyncResult{}, nil
}
func (f *fakeForges) PushBranch(context.Context, string) (string, error) { return "main", nil }
func (f *fakeForges) Push(_ context.Context, repo, branch string) (*forge.PushResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes = append(f.pushes, repo+"@"+branch)
	return &forge.PushResult{Branch: branch, New: "abcdef0123"}, nil
}
func (f *fakeForges) Follow() forges.Feed             { return nil }
func (f *fakeForges) CloneCommand(repo string) string { return "git clone ssh://localhost/" + repo }

// The origin badge source.
func (f *fakeForges) Origins(context.Context) (map[string]forge.Origin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]forge.Origin{}
	for k, v := range f.origins {
		out[k] = v
	}
	return out, nil
}

func (f *fakeForges) Origin(_ context.Context, repo string) (forge.Origin, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.origins[repo]
	return o, ok, nil
}

// originSource adapts fakeForges to selection.OriginSource, whose Follow
// is the event feed.
type originSource struct{ *fakeForges }

func (o originSource) Follow() forges.Feed { return o.feed }

type chanFeed struct {
	ch chan []events.Event
}

func (c *chanFeed) Next(ctx context.Context) ([]events.Event, error) {
	select {
	case evs := <-c.ch:
		return evs, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (c *chanFeed) Close() {}

// richHomeUI is a home page with sessions, forges and three
// repositories: one imported with sessions, one with a blocked session,
// one local.
func richHomeUI(t *testing.T, width, height int) (*UI, *fakeSource, *fakeForges) {
	t.Helper()
	ff := newFakeForges()
	src := &fakeSource{rows: frameRows(), statuses: map[string]openshell.SessionStatus{}}
	for _, r := range src.rows {
		st := *r.Status
		st.Session = events.Session{Repo: r.Info.Repo, Name: r.Info.Name, Sandbox: r.Info.Sandbox}
		src.statuses[r.Info.Sandbox] = st
	}
	ui := newHomeUI(t, width, height, func(ctx context.Context, be *backend.Backend) context.Context {
		for i, name := range []string{"sundae", "infra", "scratch"} {
			r, err := be.CreateRepository(ctx, name, nil, proto.RepositoryOptions{Description: "about " + name})
			if err != nil {
				t.Fatal(err)
			}
			// Fixed update times, newest first, "a long while ago".
			gr, err := r.Open()
			if err != nil {
				t.Fatal(err)
			}
			at := time.Date(1970, 1, 3-i, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
			if err := os.WriteFile(filepath.Join(gr.Path, "info", "last-modified"), []byte(at), 0o644); err != nil { //nolint:gosec
				t.Fatal(err)
			}
		}
		ctx = context.WithValue(ctx, sessions.SourceKey, src)
		ctx = context.WithValue(ctx, forges.SourceKey, forges.Source(ff))
		return context.WithValue(ctx, selection.OriginSourceKey, selection.OriginSource(originSource{ff}))
	})
	return ui, src, ff
}

// titleLines returns the title lines of the repository rows.
func titleLines(screen string) string {
	var out []string
	for _, l := range strings.Split(screen, "\n") {
		for _, name := range []string{"sundae", "infra", "scratch"} {
			if strings.Contains(l, " "+name+" ") || strings.HasSuffix(l, " "+name) {
				out = append(out, strings.TrimRight(l, " "))
			}
		}
	}
	return strings.Join(out, "\n")
}

func TestRepositoryBadges(t *testing.T) {
	for _, w := range []int{80, 50} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			ui, _, _ := richHomeUI(t, w, 24)
			goldenFile(t, fmt.Sprintf("home-repos-%d", w), titleLines(screenText(ui)))
		})
	}
	// Narrower still: the origin, then the session badge's word, then
	// the badge go.
	ui, _, _ := richHomeUI(t, 34, 24)
	got := titleLines(screenText(ui))
	if strings.Contains(got, "github") || !strings.Contains(got, "▲ 1 session") {
		t.Fatalf("narrow drop at 34:\n%s", got)
	}
	ui, _, _ = richHomeUI(t, 26, 24)
	if got := titleLines(screenText(ui)); !strings.Contains(got, "▲1") || strings.Contains(got, "session") {
		t.Fatalf("narrow drop at 26:\n%s", got)
	}
	// A title too long for the badges drops them on its row only.
	ui, _, _ = richHomeUI(t, 17, 24)
	if got := titleLines(screenText(ui)); !strings.Contains(got, "infra  ▲1") || strings.Contains(got, "✓") {
		t.Fatalf("badges at 17 columns:\n%s", got)
	}
}

func TestRepositoryBadgesLive(t *testing.T) {
	ui, src, ff := richHomeUI(t, 80, 24)
	before := screenText(ui)
	if !strings.Contains(before, "github ↑3") || !strings.Contains(before, "▲ 1 session") {
		t.Fatalf("badges:\n%s", before)
	}
	// A sync event re-reads that repository's origin only.
	ff.mu.Lock()
	ff.origins["sundae"] = forge.Origin{Forge: "github", Ahead: intp(0), Behind: intp(2)}
	ff.mu.Unlock()
	ff.feed.ch <- []events.Event{events.ForgeEvent(1, events.ForgeOutcome{Kind: "sync", Forge: "github", Repo: "sundae", OK: true})}
	waitScreen(t, ui, "github ↓2")
	// A session's status change, as the Monitor reads it, moves the
	// session badge: sundae's done session becomes blocked.
	st := src.statuses["sb-sundae-forge-sync"]
	st.Record.State = events.StateBlocked
	src.statuses["sb-sundae-forge-sync"] = st
	if got := screenText(ui); !strings.Contains(got, "▲ 2 sessions") {
		t.Fatalf("session badge not updated:\n%s", titleLines(got))
	}
}

// waitScreen feeds the UI the feed's messages until want shows.
func waitScreen(t *testing.T, ui *UI, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(screenText(ui), want) {
			return
		}
		drainLate(ui)
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%q never showed:\n%s", want, screenText(ui))
}

func TestRepositoriesNewSessionKey(t *testing.T) {
	ui, _, _ := richHomeUI(t, 80, 24)
	uiKey(ui, "s")
	s := screenText(ui)
	if !strings.Contains(s, "New session") || !strings.Contains(s, "• Sessions") {
		t.Fatalf("s did not open the form:\n%s", s)
	}
	// The repository is the selected one, fixed.
	if !strings.Contains(s, "Repository   sundae") || strings.Contains(s, "‹ sundae ›") {
		t.Fatalf("repository not fixed:\n%s", s)
	}
	uiKey(ui, "esc", "1")
	found := false
	for _, b := range ui.ShortHelp() {
		found = found || b.Help().Key == "s" && b.Help().Desc == "new session"
	}
	if !found {
		t.Fatal("no s in the Repositories help")
	}
}

func TestHomeForgesTab(t *testing.T) {
	for _, w := range []int{80, 50} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			ui, _, ff := richHomeUI(t, w, 20)
			uiKey(ui, "3")
			goldenFile(t, fmt.Sprintf("home-forges-%d", w), screenText(ui))
			// p pushes, after a confirm naming the branch, in the footer.
			uiKey(ui, "j", "j", "p")
			s := screenText(ui)
			last := s[strings.LastIndex(s, "\n")+1:]
			if !strings.Contains(last, "Push main of sundae to github?") || !strings.HasSuffix(last, "y / N") {
				t.Fatalf("confirm %q", last)
			}
			// q is an answer, not quit.
			uiKey(ui, "q")
			if len(ff.pushes) != 0 || strings.Contains(screenText(ui), "Push main of") {
				t.Fatal("q did not cancel the push")
			}
			uiKey(ui, "p", "y")
			waitPushes(t, ff, 1)
			// enter opens the imported repository.
			uiKeyDepth(ui, 6, "enter")
			if ui.activePage != repoPage {
				t.Fatalf("enter did not open the repository:\n%s", screenText(ui))
			}
		})
	}
}

// settle waits for the repository page to be ready.
func settle(t *testing.T, ui *UI) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		drainLate(ui)
		// The status bar shows once the repository is read.
		if s := screenText(ui); ui.activePage == repoPage && strings.Contains(s, "? Help") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the repository did not load:\n%s", screenText(ui))
}

func waitPushes(t *testing.T, ff *fakeForges, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ff.mu.Lock()
		got := len(ff.pushes)
		ff.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pushes %v", ff.pushes)
}

func TestHomeForgesHiddenWithoutSource(t *testing.T) {
	ui := newTestUI(t, 80, 24, &fakeSource{rows: frameRows()})
	if s := screenText(ui); strings.Contains(s, "Forges") || !strings.Contains(s, "1-3 tab") {
		t.Fatalf("tabs:\n%s", s)
	}
}

// TestRepoSessionsTab is the repository page's Sessions tab (section 8).
func TestRepoSessionsTab(t *testing.T) {
	for _, w := range []int{80, 50} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			ui, _, _ := richHomeUI(t, w, 20)
			uiKeyDepth(ui, 6, "enter") // sundae
			settle(t, ui)
			for range 5 {
				uiKey(ui, "tab")
			}
			goldenFile(t, fmt.Sprintf("repo-sessions-%d", w), screenText(ui))
		})
	}
}

func TestRepoSessionsKeys(t *testing.T) {
	ui, _, _ := richHomeUI(t, 80, 20)
	uiKeyDepth(ui, 6, "enter")
	settle(t, ui)
	for range 5 {
		uiKey(ui, "tab")
	}
	s := screenText(ui)
	if !strings.Contains(s, "forge-sync") || strings.Contains(s, "infra / claude-1") || !strings.Contains(s, "▲1 ✓1 ●2") {
		t.Fatalf("repo sessions:\n%s", s)
	}
	// While the filter is typed, q, ? and tab are text or the tab's.
	uiKey(ui, "/", "q", "?")
	if ui.activePage != repoPage || !strings.Contains(screenText(ui), "/ q?") {
		t.Fatalf("filter:\n%s", screenText(ui))
	}
	// esc cancels the filter, and only then leaves the repository.
	uiKey(ui, "esc")
	if ui.activePage != repoPage {
		t.Fatal("esc left the repository while the filter was open")
	}
	// The end confirm shows in the tab.
	uiKey(ui, "x")
	if s := screenText(ui); !strings.Contains(s, "End sundae / forge-sync?") {
		t.Fatalf("confirm:\n%s", s)
	}
	uiKey(ui, "esc")
	// e shows the repository's signals.
	uiKey(ui, "e")
	if s := screenText(ui); !strings.Contains(s, "‹ Signals ›") {
		t.Fatalf("signals:\n%s", s)
	}
	uiKey(ui, "e", "esc")
	if ui.activePage != selectionPage {
		t.Fatal("esc did not go back home")
	}
	// Home shows every session again.
	uiKey(ui, "2")
	if s := screenText(ui); !strings.Contains(s, "infra / claude-1") {
		t.Fatalf("home sessions after the repository:\n%s", s)
	}
}

// TestKeysDoNotClash checks the new keys only act where they belong: s
// on Repositories, p and i on Forges, and the Files tab's own p.
func TestKeysDoNotClash(t *testing.T) {
	ui, _, ff := richHomeUI(t, 80, 20)
	// p and i on Repositories do nothing.
	uiKey(ui, "p", "i")
	if ui.activePage != selectionPage || len(ff.pushes) != 0 || strings.Contains(screenText(ui), "Push") {
		t.Fatalf("p on Repositories:\n%s", screenText(ui))
	}
	// s on Forges does nothing.
	uiKey(ui, "3", "s")
	if strings.Contains(screenText(ui), "New session") {
		t.Fatal("s on Forges opened the form")
	}
	// s and p in a repository's Files tab do not open a form or push.
	uiKey(ui, "1")
	uiKeyDepth(ui, 6, "enter")
	settle(t, ui)
	uiKey(ui, "tab", "s", "p")
	if s := screenText(ui); strings.Contains(s, "New session") || strings.Contains(s, "Push main") || len(ff.pushes) != 0 {
		t.Fatalf("s/p in Files:\n%s", s)
	}
}
