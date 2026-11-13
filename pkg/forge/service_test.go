package forge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	_ "modernc.org/sqlite"
)

// newTestBackend returns a real, migrated SQLite backend in a temp dir.
func newTestBackend(t *testing.T, cfg *config.Config) (context.Context, *backend.Backend) {
	t.Helper()
	ctx := config.WithContext(t.Context(), cfg)
	dbx, err := db.Open(ctx, "sqlite", filepath.Join(cfg.DataPath, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	ctx = db.WithContext(ctx, dbx)
	st := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, st)
	be := backend.New(ctx, cfg, dbx, st)
	return backend.WithContext(ctx, be), be
}

// recorder collects forge events.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) ForgeEvent(_ context.Context, ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) kinds() map[EventKind]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := map[EventKind]int{}
	for _, e := range r.events {
		m[e.Kind]++
	}
	return m
}

// fakeForge is a GitHub API whose repositories are local bare repositories.
type fakeForge struct {
	srv   *httptest.Server
	mu    sync.Mutex
	repos []map[string]any
}

func newFakeForge(t *testing.T) *fakeForge {
	t.Helper()
	ff := &fakeForge{}
	ff.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/user/repos" || r.Header.Get("Authorization") != "Bearer ghp_test" {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		ff.mu.Lock()
		defer ff.mu.Unlock()
		json.NewEncoder(w).Encode(ff.repos) //nolint:errcheck
	}))
	t.Cleanup(ff.srv.Close)
	return ff
}

func (ff *fakeForge) add(path, cloneURL string, private bool) {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	ff.repos = append(ff.repos, map[string]any{
		"full_name": path, "clone_url": cloneURL, "default_branch": "main", "private": private,
		"description": "about " + path,
	})
}

type serviceFixture struct {
	ctx   context.Context
	be    *backend.Backend
	svc   *Service
	ff    *fakeForge
	user  proto.User
	ups   map[string]*upstream
	root  string
	event *recorder
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	hermeticGit(t)
	ff := newFakeForge(t)
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Forges = []config.ForgeConfig{{
		Name:      "gh",
		Kind:      "github",
		URL:       ff.srv.URL,
		TokenFile: writeToken(t, "ghp_test"),
		Include:   []string{"me/*", "org/*"},
		Exclude:   []string{"org/*"},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, be := newTestBackend(t, cfg)
	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{Admin: true})
	if err != nil {
		t.Fatal(err)
	}

	rec := &recorder{}
	svc, err := NewService(cfg, be, log.New(io.Discard), WithNotifier(rec))
	if err != nil {
		t.Fatal(err)
	}
	fx := &serviceFixture{ctx: ctx, be: be, svc: svc, ff: ff, user: user, ups: map[string]*upstream{}, root: t.TempDir(), event: rec}
	// The test seam: let git reach the local upstreams this test made, and
	// nothing else that the production check would refuse.
	svc.checkRemote = func(f *Forge, remote string) error {
		if strings.HasPrefix(remote, fx.root+string(filepath.Separator)) {
			return nil
		}
		return f.CheckCloneURL(remote)
	}
	return fx
}

// upstream creates a local upstream repository listed at path.
func (fx *serviceFixture) upstream(t *testing.T, path string, private bool) *upstream {
	t.Helper()
	up := newUpstream(t)
	// Move it under root so the test seam lets git reach it.
	bare := filepath.Join(fx.root, strings.ReplaceAll(path, "/", "_")+".git")
	git(t, fx.root, "clone", "--quiet", "--bare", up.bare, bare)
	git(t, up.work, "remote", "set-url", "origin", bare)
	up.bare = bare
	fx.ff.add(path, bare, private)
	fx.ups[path] = up
	return up
}

func resultFor(t *testing.T, results []ImportResult, path string) ImportResult {
	t.Helper()
	for _, r := range results {
		if r.Path == path {
			return r
		}
	}
	t.Fatalf("no result for %s in %+v", path, results)
	return ImportResult{}
}

func TestServiceImportSyncPush(t *testing.T) {
	fx := newServiceFixture(t)
	ctx := fx.ctx
	proj := fx.upstream(t, "me/proj", true)
	git(t, proj.work, "tag", "v1")
	git(t, proj.work, "push", "--quiet", "origin", "v1")
	fx.upstream(t, "me/other", false)
	fx.upstream(t, "org/skip", false)
	fx.ff.add("me/evil", "https://evil.example/me/evil.git", false)
	fx.ff.add("me/broken", filepath.Join(fx.root, "missing.git"), false)

	// Import by path.
	results, err := fx.svc.Import(ctx, fx.user, "gh", ImportOptions{Paths: []string{"me/proj", "org/skip", "me/evil", "me/nope", "me/broken"}})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	r := resultFor(t, results, "me/proj")
	if r.Status != ImportImported || r.Repo != "gh/me/proj" {
		t.Fatalf("me/proj: %+v", r)
	}
	for path, want := range map[string]string{
		"org/skip":  "excluded",
		"me/evil":   "not on the forge host",
		"me/nope":   "not listed",
		"me/broken": "git fetch",
	} {
		if r := resultFor(t, results, path); r.Status != ImportFailed || !strings.Contains(r.Error, want) {
			t.Errorf("%s: %+v, want failure containing %q", path, r, want)
		}
	}
	// A failed import leaves nothing behind.
	if _, err := fx.be.Repository(ctx, "gh/me/broken"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Errorf("failed import left a repository: %v", err)
	}

	repo, err := fx.be.Repository(ctx, "gh/me/proj")
	if err != nil {
		t.Fatal(err)
	}
	if !repo.IsPrivate() || repo.IsMirror() || repo.Vcs() != proto.DefaultVCS || repo.Description() != "about me/proj" {
		t.Errorf("repo = private %v mirror %v vcs %s desc %q", repo.IsPrivate(), repo.IsMirror(), repo.Vcs(), repo.Description())
	}
	gr, _ := repo.Open()
	dir := gr.Path
	upMain := rev(t, proj.bare, "refs/heads/main")
	if rev(t, dir, "refs/heads/main") != upMain || rev(t, dir, "refs/remotes/gh/heads/main") != upMain || rev(t, dir, "refs/tags/v1") == "" {
		t.Error("import did not fetch into the namespace and create local refs")
	}
	if headBranch(ctx, dir) != "main" {
		t.Error("HEAD not on the default branch")
	}
	// The link lives in the repository's git config; the token does not.
	for key, want := range map[string]string{
		"forge.name":          "gh",
		"forge.path":          "me/proj",
		"forge.defaultbranch": "main",
		"remote.gh.url":       proj.bare,
		"remote.gh.tagopt":    "--no-tags",
		"receive.hiderefs":    "refs/remotes/gh/",
	} {
		if got := git(t, dir, "config", "--file", "config", "--get", key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if cfgText := git(t, dir, "config", "--file", "config", "--list"); strings.Contains(cfgText, "ghp_test") {
		t.Error("token written to the repository config")
	}

	// Importing again is a no-op.
	results, err = fx.svc.Import(ctx, fx.user, "gh", ImportOptions{Paths: []string{"me/proj"}})
	if err != nil || resultFor(t, results, "me/proj").Status != ImportExists {
		t.Errorf("re-import: %+v, %v", results, err)
	}

	// A local repository with the target name is never adopted.
	if _, err := fx.be.CreateRepository(ctx, "gh/me/other", fx.user, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	results, err = fx.svc.Import(ctx, fx.user, "gh", ImportOptions{Paths: []string{"me/other"}})
	if err != nil {
		t.Fatal(err)
	}
	if r := resultFor(t, results, "me/other"); r.Status != ImportFailed || !strings.Contains(r.Error, "already exists") {
		t.Errorf("collision: %+v", r)
	}
	other, _ := fx.be.Repository(ctx, "gh/me/other")
	if l, _ := readLink(other); l != nil {
		t.Error("collision linked the existing repository")
	}
	if err := fx.be.DeleteRepository(proto.WithUserContext(ctx, fx.user), "gh/me/other"); err != nil {
		t.Fatal(err)
	}

	// --all imports what the filter lets through and is not imported yet.
	results, err = fx.svc.Import(ctx, fx.user, "gh", ImportOptions{All: true, Vcs: proto.VCSGit})
	if err != nil {
		t.Fatal(err)
	}
	if r := resultFor(t, results, "me/other"); r.Status != ImportImported {
		t.Errorf("--all me/other: %+v", r)
	}
	for _, r := range results {
		if r.Path == "me/proj" || r.Path == "org/skip" {
			t.Errorf("--all touched %s: %+v", r.Path, r)
		}
	}
	if o, _ := fx.be.Repository(ctx, "gh/me/other"); o.Vcs() != proto.VCSGit || o.IsPrivate() {
		t.Errorf("gh/me/other vcs %s private %v", o.Vcs(), o.IsPrivate())
	}

	// Upstream moves; the forge-wide sync fast-forwards.
	upMain = proj.commit(t, "main", "upstream work")
	syncs, err := fx.svc.SyncForge(ctx, "gh")
	if err != nil {
		t.Fatal(err)
	}
	if len(syncs) != 2 {
		t.Fatalf("synced %d repositories, want 2: %+v", len(syncs), syncs)
	}
	for _, s := range syncs {
		if s.Error != "" {
			t.Errorf("sync %s: %s", s.Repo, s.Error)
		}
	}
	if rev(t, dir, "refs/heads/main") != upMain {
		t.Error("forge sync did not fast-forward main")
	}

	// Local work, then the repository view shows it ahead.
	work := localClone(t, dir)
	localMain := commitAndPush(t, work, "main", "local work")
	repos, err := fx.svc.Repos(ctx, "gh", false)
	if err != nil {
		t.Fatal(err)
	}
	var projStatus *RepoStatus
	for i := range repos {
		if repos[i].Path == "org/skip" {
			t.Error("excluded repository listed without --all")
		}
		if repos[i].Path == "me/proj" {
			projStatus = &repos[i]
		}
	}
	if projStatus == nil || !projStatus.Imported || projStatus.Ahead == nil || *projStatus.Ahead != 1 || *projStatus.Behind != 0 || projStatus.LastSync == nil {
		t.Errorf("me/proj status = %+v", projStatus)
	}
	repos, err = fx.svc.Repos(ctx, "gh", true)
	if err != nil {
		t.Fatal(err)
	}
	foundExcluded := false
	for _, r := range repos {
		if r.Path == "org/skip" && r.Excluded && !r.Imported && r.Repo == "gh/org/skip" {
			foundExcluded = true
		}
	}
	if !foundExcluded {
		t.Errorf("--all did not list the excluded repository: %+v", repos)
	}

	// Sync one repository by forge path: main is ahead and left alone.
	one, err := fx.svc.SyncRepo(ctx, "gh", "me/proj")
	if err != nil || one.Error != "" {
		t.Fatalf("SyncRepo: %+v, %v", one, err)
	}
	if b, _ := one.Sync.Branch("main"); b.Action != BranchAhead {
		t.Errorf("main after local work: %+v", b)
	}
	if _, err := fx.svc.SyncRepo(ctx, "gh", "me/unknown"); !errors.Is(err, ErrRepoNotImported) {
		t.Errorf("SyncRepo unknown: %v", err)
	}

	// Push publishes it.
	pr, err := fx.svc.Push(ctx, "gh/me/proj", "", false)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if pr.Branch != "main" || pr.New != localMain || rev(t, proj.bare, "refs/heads/main") != localMain {
		t.Errorf("push result %+v", pr)
	}

	// forge ls.
	st, err := fx.svc.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 || st[0].Imported != 2 || st[0].Listed == nil || *st[0].Listed != 4 || *st[0].Matching != 3 || st[0].LastSync == nil || st[0].LastError != "" {
		t.Errorf("status = %+v (listed %v matching %v)", st, deref(st[0].Listed), deref(st[0].Matching))
	}

	// The events seam saw everything.
	kinds := fx.event.kinds()
	if kinds[EventImport] < 3 || kinds[EventSync] < 3 || kinds[EventPush] != 1 || kinds[EventDiscover] < 1 {
		t.Errorf("events = %v", kinds)
	}
}

func deref(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

func TestServicePushNotLinked(t *testing.T) {
	fx := newServiceFixture(t)
	if _, err := fx.be.CreateRepository(fx.ctx, "plain", fx.user, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.Push(fx.ctx, "plain", "main", false); !errors.Is(err, ErrNotLinked) {
		t.Errorf("err = %v, want ErrNotLinked", err)
	}
}

// TestServiceProductionCheck checks the production guard, without the test
// seam, refuses the local upstreams the other tests reach through it.
func TestServiceProductionCheck(t *testing.T) {
	fx := newServiceFixture(t)
	fx.upstream(t, "me/proj", false)
	fx.svc.checkRemote = (*Forge).CheckCloneURL

	listing, err := fx.svc.Discover(fx.ctx, "gh")
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Repos) != 0 || len(listing.Rejected) != 1 {
		t.Fatalf("listing = %+v, want the local path rejected", listing)
	}
	results, err := fx.svc.Import(fx.ctx, fx.user, "gh", ImportOptions{Paths: []string{"me/proj"}})
	if err != nil {
		t.Fatal(err)
	}
	if r := resultFor(t, results, "me/proj"); r.Status != ImportFailed || !strings.Contains(r.Error, ErrForeignCloneURL.Error()) {
		t.Errorf("import = %+v", r)
	}
}

func TestServiceSyncRunningRefused(t *testing.T) {
	fx := newServiceFixture(t)
	fx.svc.states["gh"].running = true
	if _, err := fx.svc.SyncForge(fx.ctx, "gh"); !errors.Is(err, ErrSyncRunning) {
		t.Errorf("err = %v, want ErrSyncRunning", err)
	}
	if _, err := fx.svc.SyncForge(fx.ctx, "nope"); !errors.Is(err, ErrForgeNotFound) {
		t.Errorf("err = %v, want ErrForgeNotFound", err)
	}
}

// TestServiceKnownReposAndOrigins checks the views the TUI reads without
// calling the forge: the last listing with the imported repositories, each
// repository's origin, the branch a push would push, and the status of a
// failed discovery.
func TestServiceKnownReposAndOrigins(t *testing.T) {
	fx := newServiceFixture(t)
	ctx := fx.ctx
	fx.upstream(t, "me/proj", false)
	fx.upstream(t, "me/other", false)

	// Before any discovery nothing is known.
	known, err := fx.svc.KnownRepos(ctx, "gh", false)
	if err != nil || len(known) != 0 {
		t.Fatalf("KnownRepos before discovery: %+v, %v", known, err)
	}
	if _, err := fx.svc.Import(ctx, fx.user, "gh", ImportOptions{Paths: []string{"me/proj"}}); err != nil {
		t.Fatal(err)
	}
	known, err = fx.svc.KnownRepos(ctx, "gh", false)
	if err != nil || len(known) != 2 {
		t.Fatalf("KnownRepos: %+v, %v", known, err)
	}
	if !known[1].Imported || known[1].Path != "me/proj" || known[1].Ahead == nil || *known[1].Ahead != 0 || known[0].Imported {
		t.Errorf("KnownRepos = %+v", known)
	}

	// Origins: the imported repository and its counts; local ones are
	// left out.
	if _, err := fx.be.CreateRepository(ctx, "plain", fx.user, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	origins, err := fx.svc.Origins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	o, ok := origins["gh/me/proj"]
	if len(origins) != 1 || !ok || o.Forge != "gh" || o.Path != "me/proj" || deref(o.Ahead) != 0 || deref(o.Behind) != 0 || o.LastSync == nil {
		t.Fatalf("Origins = %+v", origins)
	}
	repo, _ := fx.be.Repository(ctx, "gh/me/proj")
	gr, _ := repo.Open()
	commitAndPush(t, localClone(t, gr.Path), "main", "local work")
	fx.ups["me/proj"].commit(t, "main", "upstream work")
	if _, err := fx.svc.SyncRepo(ctx, "gh", "me/proj"); err != nil {
		t.Fatal(err)
	}
	o, ok, err = fx.svc.Origin(ctx, "gh/me/proj")
	if err != nil || !ok || deref(o.Ahead) != 1 || deref(o.Behind) != 1 {
		t.Fatalf("Origin after diverging = %+v, %v, %v", o, ok, err)
	}
	if _, ok, err := fx.svc.Origin(ctx, "plain"); ok || err != nil {
		t.Errorf("Origin of a local repository: %v, %v", ok, err)
	}

	// The branch a push without one pushes.
	if b, err := fx.svc.PushBranch(ctx, "gh/me/proj"); err != nil || b != "main" {
		t.Errorf("PushBranch = %q, %v", b, err)
	}
	if _, err := fx.svc.PushBranch(ctx, "plain"); !errors.Is(err, ErrNotLinked) {
		t.Errorf("PushBranch of a local repository: %v", err)
	}

	// A failed discovery is in the status until one succeeds; the last
	// listing stays known.
	tokenFile := fx.svc.forges[0].cfg.TokenFile
	if err := os.WriteFile(tokenFile, []byte("bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.Discover(ctx, "gh"); err == nil {
		t.Fatal("discovery with a bad token succeeded")
	}
	st, _ := fx.svc.Status(ctx)
	if !strings.Contains(st[0].ListError, "401") || st[0].Syncing {
		t.Errorf("status after a failed discovery = %+v", st[0])
	}
	if known, _ := fx.svc.KnownRepos(ctx, "gh", false); len(known) != 2 {
		t.Errorf("listing lost after a failed discovery: %+v", known)
	}
	if err := os.WriteFile(tokenFile, []byte("ghp_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.Discover(ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	if st, _ := fx.svc.Status(ctx); st[0].ListError != "" {
		t.Errorf("list error kept after a good discovery: %q", st[0].ListError)
	}
}
