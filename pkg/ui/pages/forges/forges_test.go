package forges

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/forge"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

var update = flag.Bool("update", false, "update golden files")

// now is the tests' clock.
var now = time.Date(2026, 11, 12, 14, 2, 30, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

// fakeSource is an in-memory Source: two forges, one of them failing.
type fakeSource struct {
	mu        sync.Mutex
	status    []forge.ForgeStatus
	statusErr error
	repos     map[string][]forge.RepoStatus
	discovers []string
	imports   [][]string
	syncs     []string
	pushes    []string
	feed      *fakeFeed
	// gate, when set, holds SyncForge until closed.
	gate    chan struct{}
	pushErr error
}

func (f *fakeSource) Status(context.Context) ([]forge.ForgeStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forge.ForgeStatus(nil), f.status...), f.statusErr
}

func (f *fakeSource) Repos(_ context.Context, name string) ([]forge.RepoStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forge.RepoStatus(nil), f.repos[name]...), nil
}

func (f *fakeSource) Discover(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.discovers = append(f.discovers, name)
	if name == "gitlab" {
		return errors.New("GET https://gitlab.com/api/v4/projects: 401 Unauthorized")
	}
	return nil
}

func (f *fakeSource) Import(_ context.Context, name string, paths []string) ([]forge.ImportResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imports = append(f.imports, append([]string{name}, paths...))
	var out []forge.ImportResult
	for _, p := range paths {
		if p == "me/broken" {
			out = append(out, forge.ImportResult{Path: p, Status: forge.ImportFailed, Error: "git fetch: repository not found"})
			continue
		}
		out = append(out, forge.ImportResult{Path: p, Repo: name + "/" + p, Status: forge.ImportImported, Sync: &forge.SyncResult{}})
	}
	return out, nil
}

func (f *fakeSource) SyncForge(_ context.Context, name string) ([]forge.RepoSyncResult, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, name)
	var out []forge.RepoSyncResult
	for _, r := range f.repos[name] {
		if r.Imported {
			out = append(out, forge.RepoSyncResult{Repo: r.Repo, Path: r.Path, Sync: &forge.SyncResult{}})
		}
	}
	return out, nil
}

func (f *fakeSource) SyncRepo(_ context.Context, name, repo string) (forge.RepoSyncResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, name+":"+repo)
	return forge.RepoSyncResult{Repo: repo, Sync: &forge.SyncResult{Branches: []forge.BranchStatus{{Name: "main", Action: forge.BranchFastForwarded}}}}, nil
}

func (f *fakeSource) PushBranch(context.Context, string) (string, error) { return "main", nil }

func (f *fakeSource) Push(_ context.Context, repo, branch string) (*forge.PushResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes = append(f.pushes, repo+"@"+branch)
	if f.pushErr != nil {
		return nil, f.pushErr
	}
	return &forge.PushResult{Branch: branch, Old: "1111111aaaa", New: "2222222bbbb"}, nil
}

func (f *fakeSource) Follow() Feed {
	if f.feed == nil {
		f.feed = &fakeFeed{ch: make(chan []events.Event, 4), done: make(chan struct{})}
	}
	return f.feed
}

func (f *fakeSource) CloneCommand(repo string) string {
	return "git clone ssh://git.example.com/" + repo
}

type fakeFeed struct {
	ch   chan []events.Event
	done chan struct{}
	once sync.Once
}

func (f *fakeFeed) Next(ctx context.Context) ([]events.Event, error) {
	select {
	case evs := <-f.ch:
		return evs, nil
	case <-f.done:
		return nil, ErrFeedClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeFeed) Close() { f.once.Do(func() { close(f.done) }) }

// newFakeSource is section 6's mockup: a GitHub forge with two imported
// repositories and two that are not, and a GitLab forge whose last sync
// failed.
func newFakeSource() *fakeSource {
	synced := now.Add(-3 * time.Minute)
	glSynced := now.Add(-2 * time.Hour)
	return &fakeSource{
		status: []forge.ForgeStatus{
			{Name: "github", Kind: "github", URL: "https://github.com", Imported: 2, Listed: ptr(42), Matching: ptr(4), LastSync: &synced},
			{Name: "gitlab", Kind: "gitlab", URL: "https://gitlab.com", Imported: 1, LastSync: &glSynced, LastError: "discover: 401 token expired"},
		},
		repos: map[string][]forge.RepoStatus{
			"github": {
				{Path: "danielboothcloud/dotfiles", Repo: "github/danielboothcloud/dotfiles", Listed: true},
				{Path: "danielboothcloud/pi-aio", Repo: "github/danielboothcloud/pi-aio", Listed: true, Imported: true, Ahead: ptr(0), Behind: ptr(2), LastSync: &synced},
				{Path: "danielboothcloud/quartermaster", Repo: "github/danielboothcloud/quartermaster", Listed: true},
				{Path: "danielboothcloud/sundae", Repo: "github/danielboothcloud/sundae", Listed: true, Imported: true, Ahead: ptr(3), Behind: ptr(0), LastSync: &synced},
			},
			"gitlab": {
				{Path: "ops/infra", Repo: "gitlab/ops/infra", Imported: true, Ahead: ptr(0), Behind: ptr(0), LastSync: &glSynced, LastError: "git fetch: HTTP 401"},
			},
		},
	}
}

func newModel(t *testing.T, width, height int, src *fakeSource) *Forges {
	t.Helper()
	m := New(common.NewCommon(context.Background(), width, height), src)
	m.now = func() time.Time { return now }
	m.SetSize(width, height)
	t.Cleanup(m.Close)
	return m
}

// started returns a model that started and read src.
func started(t *testing.T, width, height int, src *fakeSource) *Forges {
	t.Helper()
	m := newModel(t, width, height, src)
	run(m, m.Start(), 3)
	return m
}

func press(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	}
	r := []rune(k)
	return tea.KeyPressMsg{Code: r[0], Text: k}
}

func keys(m *Forges, ks ...string) tea.Cmd {
	var cmd tea.Cmd
	for _, k := range ks {
		_, cmd = m.Update(press(k))
	}
	return cmd
}

// run runs cmd and feeds what it returns to the model, following batches,
// for up to depth rounds. Commands that block (the event feed) are left
// after a short wait.
func run(m *Forges, cmd tea.Cmd, depth int) {
	if cmd == nil || depth == 0 {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(200 * time.Millisecond):
		return
	}
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			run(m, c, depth)
		}
	default:
		_, next := m.Update(msg)
		run(m, next, depth-1)
	}
}

func view(m *Forges) string {
	lines := strings.Split(ansi.Strip(m.View()), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got+"\n"), 0o644); err != nil { //nolint:gosec
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got+"\n" != string(want) {
		t.Fatalf("%s differs from %s:\n%s\n--- want ---\n%s", name, path, got, want)
	}
}

// selectRow moves the cursor to the row whose path (or forge, for a
// header) is name.
func selectRow(t *testing.T, m *Forges, name string) {
	t.Helper()
	for i, it := range m.items() {
		g := m.groups[it.g]
		if (it.kind == headerRow && g.status.Name == name) || (it.kind == repoRow && g.repos[it.r].Path == name) {
			m.cursor = i
			m.syncSel()
			return
		}
	}
	t.Fatalf("no row %s", name)
}

func TestForgesGolden(t *testing.T) {
	for _, w := range []int{80, 50} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			// The tab gets the terminal width less the frame's margins.
			m := started(t, w-4, 20, newFakeSource())
			keys(m, "j")
			golden(t, fmt.Sprintf("forges-%d", w), view(m))
		})
	}
}

func TestForgesStartListsOnlyUnlistedForges(t *testing.T) {
	src := newFakeSource()
	m := started(t, 76, 20, src)
	// github was listed before (Listed set); gitlab never was.
	if fmt.Sprint(src.discovers) != "[gitlab]" {
		t.Fatalf("discovered %v", src.discovers)
	}
	// Start does nothing the second time.
	if m.Start() != nil {
		t.Fatal("started twice")
	}
	// Opening the TUI does not touch the forges: Init does nothing.
	n := newModel(t, 76, 20, newFakeSource())
	if n.Init() != nil {
		t.Fatal("Init reads the forges")
	}
}

func TestForgesInProgressGolden(t *testing.T) {
	src := newFakeSource()
	src.gate = make(chan struct{})
	m := started(t, 76, 20, src)
	// r on the github header syncs the forge in the background.
	selectRow(t, m, "github")
	syncCmd := keys(m, "r")
	if syncCmd == nil || m.busy[headerKey("github")] == "" {
		t.Fatal("r did not start a forge sync")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- syncCmd() }()
	// i on a repository imports it alongside.
	selectRow(t, m, "danielboothcloud/dotfiles")
	importCmd := keys(m, "i")
	// A sync event of the running sync counts as progress, live.
	_, cmd := m.Update(feedMsg{evs: []events.Event{events.ForgeEvent(0, events.ForgeOutcome{Kind: "sync", Forge: "github", Repo: "github/danielboothcloud/pi-aio", Path: "danielboothcloud/pi-aio", OK: true})}})
	if cmd == nil {
		t.Fatal("an event did not read the forge again")
	}
	if !strings.Contains(view(m), "syncing… 1 of 2") {
		t.Fatalf("progress:\n%s", view(m))
	}
	golden(t, "forges-busy-80", view(m))

	// The UI was never blocked: keys still work while both run.
	keys(m, "j")
	run(m, importCmd, 2)
	if fmt.Sprint(src.imports) != "[[github danielboothcloud/dotfiles]]" {
		t.Fatalf("imports %v", src.imports)
	}
	if !strings.Contains(view(m), "Imported danielboothcloud/dotfiles as github/danielboothcloud/dotfiles.") {
		t.Fatalf("import result:\n%s", view(m))
	}
	close(src.gate)
	_, next := m.Update(<-done)
	run(m, next, 2)
	if m.busy[headerKey("github")] != "" || !strings.Contains(view(m), "Synced github: 2 repositories.") {
		t.Fatalf("sync result:\n%s", view(m))
	}
}

func TestForgesStatusError(t *testing.T) {
	src := newFakeSource()
	src.statusErr = errors.New("database is locked")
	m := started(t, 76, 10, src)
	if got := view(m); !strings.Contains(got, "✗ Cannot read the forges (r to retry): database is locked") {
		t.Fatalf("error:\n%s", got)
	}
	if h := m.ShortHelp(); len(h) != 1 || h[0].Help().Desc != "retry" {
		t.Fatalf("help %v", h)
	}
	src.statusErr = nil
	run(m, keys(m, "r"), 2)
	if got := view(m); !strings.Contains(got, "github") {
		t.Fatalf("after retry:\n%s", got)
	}
}

func TestForgesImportAllRespectsFilter(t *testing.T) {
	src := newFakeSource()
	src.repos["gitlab"] = append(src.repos["gitlab"], forge.RepoStatus{Path: "ops/dot-tools", Repo: "gitlab/ops/dot-tools", Listed: true})
	m := started(t, 76, 20, src)
	keys(m, "I")
	if m.confirm == nil || !m.Capturing() {
		t.Fatal("I did not ask")
	}
	if got := ansi.Strip(m.FooterLine(76)); !strings.HasPrefix(got, "Import 3 repositories (github 2, gitlab 1)?") || !strings.HasSuffix(got, "y / N") {
		t.Fatalf("confirm %q", got)
	}
	keys(m, "n")
	if m.confirm != nil || len(src.imports) != 0 {
		t.Fatal("n did not cancel")
	}
	// The filter narrows what I imports.
	keys(m, "/")
	for _, r := range "dot" {
		keys(m, string(r))
	}
	keys(m, "enter")
	keys(m, "I")
	if got := ansi.Strip(m.FooterLine(76)); !strings.HasPrefix(got, "Import 2 repositories matching “dot” (github 1, gitlab 1)?") {
		t.Fatalf("confirm %q", got)
	}
	cmd := keys(m, "y")
	if m.busy[rowKey("github", "danielboothcloud/dotfiles")] != "importing…" {
		t.Fatalf("busy %v", m.busy)
	}
	run(m, cmd, 2)
	if fmt.Sprint(src.imports) != "[[github danielboothcloud/dotfiles] [gitlab ops/dot-tools]]" &&
		fmt.Sprint(src.imports) != "[[gitlab ops/dot-tools] [github danielboothcloud/dotfiles]]" {
		t.Fatalf("imports %v", src.imports)
	}
	// Nothing left to import among the rows shown once they are imported.
	src.repos["github"][0].Imported = true
	src.repos["gitlab"][1].Imported = true
	run(m, m.loadCmd(""), 1)
	keys(m, "I")
	if m.confirm != nil || !strings.Contains(view(m), "Nothing to import") {
		t.Fatalf("view:\n%s", view(m))
	}
}

func TestForgesImportFailureStaysInTheRow(t *testing.T) {
	src := newFakeSource()
	src.repos["github"] = append(src.repos["github"], forge.RepoStatus{Path: "me/broken", Repo: "github/me/broken", Listed: true})
	m := started(t, 76, 20, src)
	selectRow(t, m, "me/broken")
	run(m, keys(m, "i"), 2)
	got := view(m)
	if !strings.Contains(got, "✗ import failed") || !strings.Contains(got, "✗ Cannot import me/broken: git fetch: repository not found") {
		t.Fatalf("view:\n%s", got)
	}
	// The reason stays shown when the row is selected again.
	keys(m, "k", "j")
	if !strings.Contains(view(m), "✗ me/broken: git fetch: repository not found") {
		t.Fatalf("view:\n%s", view(m))
	}
}

func TestForgesPushConfirmAndRefusal(t *testing.T) {
	src := newFakeSource()
	m := started(t, 116, 20, src)
	selectRow(t, m, "danielboothcloud/sundae")
	run(m, keys(m, "p"), 1)
	if m.confirm == nil {
		t.Fatal("p did not ask")
	}
	if got := ansi.Strip(m.FooterLine(116)); !strings.HasPrefix(got, "Push main of github/danielboothcloud/sundae to github?  Fast-forward only, no force.") {
		t.Fatalf("confirm %q", got)
	}
	run(m, keys(m, "y"), 2)
	if fmt.Sprint(src.pushes) != "[github/danielboothcloud/sundae@main]" {
		t.Fatalf("pushes %v", src.pushes)
	}
	if !strings.Contains(view(m), "Pushed main of github/danielboothcloud/sundae to github: 1111111..2222222.") {
		t.Fatalf("view:\n%s", view(m))
	}

	// A refused push says why, in the tab.
	src.pushErr = &forge.UpstreamAheadError{Branch: "main", Missing: 2}
	selectRow(t, m, "danielboothcloud/pi-aio")
	run(m, keys(m, "p"), 1)
	run(m, keys(m, "y"), 2)
	got := view(m)
	if !strings.Contains(got, "✗ push refused") || !strings.Contains(got, "✗ Push refused: forge branch main has 2 commit(s) that main lacks") {
		t.Fatalf("view:\n%s", got)
	}

	// Pushing a repository that is not imported, or from a header, is
	// explained instead.
	selectRow(t, m, "danielboothcloud/dotfiles")
	if keys(m, "p") != nil || !strings.Contains(view(m), "not imported; there is nothing to push") {
		t.Fatalf("view:\n%s", view(m))
	}
}

func TestForgesSyncAndOpen(t *testing.T) {
	src := newFakeSource()
	m := started(t, 116, 20, src)
	selectRow(t, m, "danielboothcloud/sundae")
	run(m, keys(m, "r"), 2)
	if fmt.Sprint(src.syncs) != "[github:github/danielboothcloud/sundae]" {
		t.Fatalf("syncs %v", src.syncs)
	}
	if !strings.Contains(view(m), "Synced github/danielboothcloud/sundae: main fast-forwarded.") {
		t.Fatalf("view:\n%s", view(m))
	}
	cmd := keys(m, "enter")
	if msg, ok := cmd().(OpenRepoMsg); !ok || msg.Repo != "github/danielboothcloud/sundae" {
		t.Fatalf("enter: %v", cmd())
	}
	if c := keys(m, "c"); c == nil || !strings.Contains(view(m), "git clone ssh://git.example.com/github/danielboothcloud/sundae") {
		t.Fatalf("copy:\n%s", view(m))
	}
	selectRow(t, m, "danielboothcloud/dotfiles")
	if keys(m, "enter") != nil || !strings.Contains(view(m), "not imported; press i to import it") {
		t.Fatalf("enter on a repository not imported:\n%s", view(m))
	}
}

func TestForgesLiveUpdates(t *testing.T) {
	src := newFakeSource()
	m := started(t, 76, 20, src)
	// Someone imports quartermaster from the command line: the event
	// reads github again and the row shows it imported.
	src.mu.Lock()
	src.repos["github"][2].Imported = true
	src.repos["github"][2].Ahead, src.repos["github"][2].Behind = ptr(0), ptr(0)
	src.status[0].Imported = 3
	src.mu.Unlock()
	_, cmd := m.Update(feedMsg{evs: []events.Event{events.ForgeEvent(1, events.ForgeOutcome{Kind: "import", Forge: "github", Repo: "github/danielboothcloud/quartermaster", Path: "danielboothcloud/quartermaster", OK: true})}})
	run(m, cmd, 2)
	got := view(m)
	if !strings.Contains(got, "3 imported") || !strings.Contains(got, "✓ danielboothcloud/quartermaster") {
		t.Fatalf("view:\n%s", got)
	}
	// Reads in a burst are coalesced: one under way, the rest after it.
	m.loading = true
	if m.loadCmd("github") != nil || m.loadCmd("gitlab") != nil || len(m.dirty) != 2 {
		t.Fatalf("dirty %v", m.dirty)
	}
}

func TestForgesFeedEndsOnClose(t *testing.T) {
	src := newFakeSource()
	m := newModel(t, 76, 20, src)
	m.Start()
	cmd := m.nextCmd()
	ended := make(chan tea.Msg, 1)
	go func() { ended <- cmd() }()
	time.Sleep(20 * time.Millisecond)
	m.Close()
	select {
	case msg := <-ended:
		if _, ok := msg.(feedEndMsg); !ok {
			t.Fatalf("got %T", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the feed wait did not end on Close")
	}
}

func TestForgesFilter(t *testing.T) {
	m := started(t, 76, 20, newFakeSource())
	keys(m, "/")
	if !m.Capturing() {
		t.Fatal("not capturing while typing the filter")
	}
	for _, r := range "infra" {
		keys(m, string(r))
	}
	keys(m, "enter")
	got := view(m)
	if strings.Contains(got, "github") || !strings.Contains(got, "ops/infra") || !strings.Contains(got, "/infra · 1 of 5 repositories") {
		t.Fatalf("filtered:\n%s", got)
	}
	keys(m, "esc")
	if !strings.Contains(view(m), "github") {
		t.Fatal("esc did not clear the filter")
	}
}

func TestBusFeed(t *testing.T) {
	bus := events.New()
	feed := BusFeed(bus.Subscribe(events.OfKind(events.KindForge)))
	bus.Publish(events.ForgeEvent(0, events.ForgeOutcome{Kind: "sync", Forge: "github", OK: true}))
	bus.Publish(events.ForgeEvent(0, events.ForgeOutcome{Kind: "sync", Forge: "gitlab", OK: false, Error: "boom"}))
	evs, err := feed.Next(t.Context())
	if err != nil || len(evs) != 2 || evs[1].Forge.Forge != "gitlab" {
		t.Fatalf("Next = %+v, %v", evs, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := feed.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Next without events: %v", err)
	}
	feed.Close()
	if _, err := feed.Next(t.Context()); !errors.Is(err, ErrFeedClosed) {
		t.Fatalf("Next after Close: %v", err)
	}
}

// TestForgesListErrorStaysInTheHeader is the error state of an
// unreachable forge: the header says what went wrong, shortened, and the
// whole of it shows when the header is selected.
func TestForgesListErrorStaysInTheHeader(t *testing.T) {
	src := newFakeSource()
	src.status[1].LastError = ""
	src.status[1].ListError = `forge gitlab: Get "http://127.0.0.1:1/api/v4/projects?membership=true": dial tcp 127.0.0.1:1: connect: connection refused`
	src.repos["gitlab"] = nil
	m := started(t, 96, 20, src)
	got := view(m)
	if !strings.Contains(got, "✗ cannot list: dial tcp 127.0.0.1:1: connect: connection refused") ||
		!strings.Contains(got, "Cannot list its repositories (r to sync and retry).") {
		t.Fatalf("view:\n%s", got)
	}
	selectRow(t, m, "gitlab")
	if !strings.Contains(view(m), "✗ gitlab: cannot list: dial tcp") {
		t.Fatalf("selected header:\n%s", view(m))
	}
	golden(t, "forges-list-error-100", view(m))
}
