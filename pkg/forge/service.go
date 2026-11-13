package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/log/v2"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/utils"
)

var (
	// ErrSyncRunning is returned when a forge is already being synced.
	ErrSyncRunning = errors.New("a sync of this forge is already running")
	// ErrNotLinked is returned for a repository that was not imported from
	// a forge.
	ErrNotLinked = errors.New("repository is not linked to a forge")
	// ErrRepoNotImported is returned when syncing a repository the forge
	// has no link to.
	ErrRepoNotImported = errors.New("repository is not imported from this forge")
)

// Backend is the part of the Sundae backend the forge service uses.
// *backend.Backend implements it.
type Backend interface {
	Repositories(ctx context.Context) ([]proto.Repository, error)
	Repository(ctx context.Context, name string) (proto.Repository, error)
	CreateRepository(ctx context.Context, name string, user proto.User, opts proto.RepositoryOptions) (proto.Repository, error)
	DeleteRepository(ctx context.Context, name string) error
}

// Option configures a Service.
type Option func(*Service)

// WithNotifier sets the Notifier that receives forge events.
func WithNotifier(n Notifier) Option {
	return func(s *Service) { s.SetNotifier(n) }
}

// WithHTTPClient sets the HTTP client used for forge APIs.
func WithHTTPClient(c *http.Client) Option {
	return func(s *Service) { s.client = c }
}

// Service discovers, imports, syncs and pushes forge repositories.
type Service struct {
	forges []*Forge
	byName map[string]*Forge
	be     Backend
	logger *log.Logger
	client *http.Client

	// checkRemote guards every URL git is pointed at, and the clone URLs
	// discovery returns; it is Forge.CheckRemote. Tests in this package
	// replace it to reach local upstream repositories. It cannot be changed
	// from outside the package, so the production check is never weakened.
	checkRemote func(f *Forge, remote string) error

	mu       sync.Mutex
	notifier Notifier
	states   map[string]*forgeState
	locks    map[string]*sync.Mutex
}

// forgeState is what the service remembers about a forge while it runs.
type forgeState struct {
	running  bool
	lastSync time.Time
	lastErr  string
	listing  *Listing
	// listErr is why the last discovery failed; empty when it succeeded.
	listErr string
}

// NewService returns the forge service for the forges in cfg, which must
// have been validated.
func NewService(cfg *config.Config, be Backend, logger *log.Logger, opts ...Option) (*Service, error) {
	if logger == nil {
		logger = log.Default()
	}
	s := &Service{
		byName:      map[string]*Forge{},
		be:          be,
		logger:      logger.WithPrefix("forge"),
		client:      newAPIClient(),
		checkRemote: (*Forge).CheckRemote,
		notifier:    nopNotifier{},
		states:      map[string]*forgeState{},
		locks:       map[string]*sync.Mutex{},
	}
	for _, fc := range cfg.Forges {
		f, err := New(fc)
		if err != nil {
			return nil, err
		}
		s.forges = append(s.forges, f)
		s.byName[f.Name()] = f
		s.states[f.Name()] = &forgeState{}
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// SetNotifier replaces the Notifier. A nil Notifier drops events.
func (s *Service) SetNotifier(n Notifier) {
	if n == nil {
		n = nopNotifier{}
	}
	s.mu.Lock()
	s.notifier = n
	s.mu.Unlock()
}

func (s *Service) notify(ctx context.Context, ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	s.mu.Lock()
	n := s.notifier
	s.mu.Unlock()
	n.ForgeEvent(ctx, ev)
}

// Forges returns the configured forges in configuration order.
func (s *Service) Forges() []*Forge {
	return append([]*Forge(nil), s.forges...)
}

// Forge returns the named forge.
func (s *Service) Forge(name string) (*Forge, error) {
	f, ok := s.byName[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrForgeNotFound, name)
	}
	return f, nil
}

// lock serialises operations on one repository and returns the unlock.
func (s *Service) lock(repo string) func() {
	s.mu.Lock()
	m, ok := s.locks[repo]
	if !ok {
		m = &sync.Mutex{}
		s.locks[repo] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// Link is a Sundae repository's link to its forge repository, read from
// the repository's git config.
type Link struct {
	Forge string
	Path  string
	Repo  string
	// Dir is the bare repository's directory.
	Dir string
	// Remote is remote.<forge>.url.
	Remote        string
	DefaultBranch string
	LastSync      time.Time
	LastError     string
}

// readLink returns the repository's forge link, or nil when it has none.
func readLink(r proto.Repository) (*Link, error) {
	gr, err := r.Open()
	if err != nil {
		return nil, err
	}
	cfg, err := gr.Config()
	if err != nil {
		return nil, err
	}
	sec := cfg.Section("forge")
	name, path := sec.Option("name"), sec.Option("path")
	if name == "" || path == "" {
		return nil, nil //nolint:nilnil
	}
	l := &Link{
		Forge:         name,
		Path:          path,
		Repo:          r.Name(),
		Dir:           gr.Path,
		Remote:        cfg.Section("remote").Subsection(name).Option("url"),
		DefaultBranch: sec.Option("defaultbranch"),
		LastError:     sec.Option("lasterror"),
	}
	if t, err := time.Parse(time.RFC3339, sec.Option("lastsync")); err == nil {
		l.LastSync = t
	}
	return l, nil
}

// LinkedForge returns the name of the forge the repository was imported
// from, as its git config records it ([forge] name), or "" when it is not
// linked to a forge. The forge may no longer be configured.
func LinkedForge(r proto.Repository) (string, error) {
	l, err := readLink(r)
	if err != nil || l == nil {
		return "", err
	}
	return l.Forge, nil
}

// links returns the links of every repository imported from forge, or from
// any forge when forge is "".
func (s *Service) links(ctx context.Context, forge string) ([]*Link, error) {
	repos, err := s.be.Repositories(ctx)
	if err != nil {
		return nil, err
	}
	var links []*Link
	for _, r := range repos {
		l, err := readLink(r)
		if err != nil {
			s.logger.Debug("skipping repository with unreadable config", "repo", r.Name(), "err", err)
			continue
		}
		if l == nil || (forge != "" && l.Forge != forge) {
			continue
		}
		links = append(links, l)
	}
	sort.Slice(links, func(i, j int) bool { return links[i].Repo < links[j].Repo })
	return links, nil
}

// writeLink records the link in the repository's git config: the remote
// (url, fetch refspecs into the forge's namespace, no tag following), a
// receive.hideRefs entry so pushes to Sundae cannot write the namespace,
// and the [forge] section.
func writeLink(ctx context.Context, dir string, f *Forge, rr RemoteRepo, remoteURL string) error {
	remote := "remote." + f.Name()
	if err := gitConfigSet(ctx, dir, remote+".url", remoteURL); err != nil {
		return err
	}
	if err := gitConfigReplaceAll(ctx, dir, remote+".fetch", fetchRefspecs(f.Name())...); err != nil {
		return err
	}
	if err := gitConfigSet(ctx, dir, remote+".tagOpt", "--no-tags"); err != nil {
		return err
	}
	hide := namespace(f.Name())
	if _, err := runGit(ctx, dir, nil, localTimeout, "config", "--file", "config", "--replace-all",
		"receive.hideRefs", hide, "^"+regexp.QuoteMeta(hide)+"$"); err != nil {
		return err
	}
	for k, v := range map[string]string{
		"forge.name":          f.Name(),
		"forge.path":          rr.Path,
		"forge.defaultbranch": rr.DefaultBranch,
	} {
		if v == "" {
			continue
		}
		if err := gitConfigSet(ctx, dir, k, v); err != nil {
			return err
		}
	}
	return nil
}

// recordSync stores the time and error of the last sync in the repository's
// git config.
func recordSync(ctx context.Context, dir string, t time.Time, syncErr error) error {
	if err := gitConfigSet(ctx, dir, "forge.lastsync", t.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if syncErr == nil {
		return gitConfigUnset(ctx, dir, "forge.lasterror")
	}
	msg := strings.Join(strings.Fields(syncErr.Error()), " ")
	return gitConfigSet(ctx, dir, "forge.lasterror", msg)
}

// ForgeStatus is a forge as `forge ls` shows it.
type ForgeStatus struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	URL      string `json:"url"`
	Prefix   string `json:"prefix"`
	Interval string `json:"sync_interval"`
	// Transport is how git reaches the forge: "https" or "ssh".
	Transport string `json:"transport"`
	// SSHURL is the SSH endpoint when Transport is "ssh".
	SSHURL string `json:"ssh_url,omitempty"`
	// Imported counts repositories linked to the forge.
	Imported int `json:"imported"`
	// Listed counts the repositories the last discovery returned, and
	// Matching those passing include and exclude. Nil before any discovery.
	Listed   *int `json:"listed"`
	Matching *int `json:"matching"`
	// LastSync is the last scheduled or forge-wide sync in this process, or
	// failing that the newest sync recorded in an imported repository.
	LastSync  *time.Time `json:"last_sync"`
	LastError string     `json:"last_error,omitempty"`
	// ListError is why the last discovery failed; empty when it succeeded
	// or none ran.
	ListError string `json:"list_error,omitempty"`
	// Syncing reports that a forge-wide sync is running.
	Syncing bool `json:"syncing"`
}

// Status returns every forge's status. It never calls a forge.
func (s *Service) Status(ctx context.Context) ([]ForgeStatus, error) {
	links, err := s.links(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []ForgeStatus
	for _, f := range s.forges {
		st := ForgeStatus{
			Name:      f.Name(),
			Kind:      f.Kind(),
			URL:       f.URL(),
			Prefix:    f.Prefix(),
			Interval:  f.Interval().String(),
			Transport: f.Transport(),
			SSHURL:    f.SSHURL(),
		}
		var newest time.Time
		for _, l := range links {
			if l.Forge != f.Name() {
				continue
			}
			st.Imported++
			if l.LastSync.After(newest) {
				newest = l.LastSync
			}
		}
		s.mu.Lock()
		fs := s.states[f.Name()]
		if fs.listing != nil {
			listed, matching := len(fs.listing.Repos), 0
			for _, r := range fs.listing.Repos {
				if f.Matches(r.Path) {
					matching++
				}
			}
			st.Listed, st.Matching = &listed, &matching
		}
		if !fs.lastSync.IsZero() {
			newest = fs.lastSync
		}
		st.LastError = fs.lastErr
		st.ListError = fs.listErr
		st.Syncing = fs.running
		s.mu.Unlock()
		if !newest.IsZero() {
			st.LastSync = &newest
		}
		out = append(out, st)
	}
	return out, nil
}

// Discover lists the forge's repositories now and remembers the listing.
func (s *Service) Discover(ctx context.Context, name string) (*Listing, error) {
	f, err := s.Forge(name)
	if err != nil {
		return nil, err
	}
	listing, err := f.discover(ctx, s.client, s.checkRemote)
	s.mu.Lock()
	if err == nil {
		s.states[name].listing = listing
		s.states[name].listErr = ""
	} else {
		s.states[name].listErr = strings.Join(strings.Fields(err.Error()), " ")
	}
	s.mu.Unlock()
	s.notify(ctx, Event{Kind: EventDiscover, Forge: name, Err: err})
	if err == nil && len(listing.Rejected) > 0 {
		for _, r := range listing.Rejected {
			s.logger.Warn("rejected forge repository", "forge", name, "path", r.Path, "reason", r.Reason)
		}
	}
	return listing, err
}

// RepoStatus is a forge repository as `forge repos` shows it.
type RepoStatus struct {
	Path string `json:"path"`
	// Repo is the Sundae repository name: the imported repository, or
	// the name an import would use.
	Repo     string `json:"repo"`
	Imported bool   `json:"imported"`
	// Listed is false for an imported repository the forge no longer lists
	// (deleted, renamed, or the token lost access).
	Listed bool `json:"listed"`
	// Excluded is true for a listed repository include/exclude filter out.
	Excluded      bool   `json:"excluded"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch,omitempty"`
	// Ahead and Behind compare the local default branch with the upstream
	// one as of the last sync. Nil when not imported or either is missing.
	Ahead     *int       `json:"ahead"`
	Behind    *int       `json:"behind"`
	LastSync  *time.Time `json:"last_sync"`
	LastError string     `json:"last_error,omitempty"`
}

// Repos lists the forge's repositories, imported or not, and imported
// repositories the forge no longer lists. Repositories the include/exclude
// filter removes are only listed with all.
func (s *Service) Repos(ctx context.Context, name string, all bool) ([]RepoStatus, error) {
	f, err := s.Forge(name)
	if err != nil {
		return nil, err
	}
	listing, err := s.Discover(ctx, name)
	if err != nil {
		return nil, err
	}
	return s.reposFrom(ctx, f, listing, all)
}

// KnownRepos is Repos from the last discovery instead of a new one: it
// never calls the forge. Before any discovery succeeded it lists the
// imported repositories only, all with Listed false.
func (s *Service) KnownRepos(ctx context.Context, name string, all bool) ([]RepoStatus, error) {
	f, err := s.Forge(name)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	listing := s.states[name].listing
	s.mu.Unlock()
	return s.reposFrom(ctx, f, listing, all)
}

// reposFrom lists the repositories of a listing (nil for none) and the
// imported ones it lacks.
func (s *Service) reposFrom(ctx context.Context, f *Forge, listing *Listing, all bool) ([]RepoStatus, error) {
	links, err := s.links(ctx, f.Name())
	if err != nil {
		return nil, err
	}
	byPath := map[string]*Link{}
	for _, l := range links {
		byPath[strings.ToLower(l.Path)] = l
	}

	var out []RepoStatus
	var listed []RemoteRepo
	if listing != nil {
		listed = listing.Repos
	}
	for _, rr := range listed {
		excluded := !f.Matches(rr.Path)
		l := byPath[strings.ToLower(rr.Path)]
		delete(byPath, strings.ToLower(rr.Path))
		if excluded && !all && l == nil {
			continue
		}
		st := RepoStatus{
			Path:          rr.Path,
			Listed:        true,
			Excluded:      excluded,
			Private:       rr.Private,
			Archived:      rr.Archived,
			DefaultBranch: rr.DefaultBranch,
		}
		st.Repo, _ = f.RepoName(rr.Path)
		if l != nil {
			s.fillLinkStatus(ctx, &st, l)
		}
		out = append(out, st)
	}
	for _, l := range byPath {
		st := RepoStatus{Path: l.Path, DefaultBranch: l.DefaultBranch}
		s.fillLinkStatus(ctx, &st, l)
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Path) < strings.ToLower(out[j].Path) })
	return out, nil
}

// Origin is where an imported repository comes from, and how its default
// branch compares with the forge's as of the last sync.
type Origin struct {
	Forge string `json:"forge"`
	Path  string `json:"path"`
	// DefaultBranch is the forge's default branch as of the last sync.
	DefaultBranch string `json:"default_branch,omitempty"`
	// Ahead and Behind are as in RepoStatus: nil when either branch is
	// missing.
	Ahead     *int       `json:"ahead"`
	Behind    *int       `json:"behind"`
	LastSync  *time.Time `json:"last_sync"`
	LastError string     `json:"last_error,omitempty"`
}

func (s *Service) originOf(ctx context.Context, l *Link) Origin {
	st := RepoStatus{Path: l.Path, DefaultBranch: l.DefaultBranch}
	s.fillLinkStatus(ctx, &st, l)
	return Origin{
		Forge:         l.Forge,
		Path:          l.Path,
		DefaultBranch: l.DefaultBranch,
		Ahead:         st.Ahead,
		Behind:        st.Behind,
		LastSync:      st.LastSync,
		LastError:     st.LastError,
	}
}

// Origins returns the origin of every repository imported from a forge,
// by Sundae repository name; repositories not in it are local. It
// never calls a forge.
func (s *Service) Origins(ctx context.Context) (map[string]Origin, error) {
	links, err := s.links(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make(map[string]Origin, len(links))
	for _, l := range links {
		if _, ok := s.byName[l.Forge]; !ok {
			// Imported from a forge no longer configured: local now.
			continue
		}
		out[l.Repo] = s.originOf(ctx, l)
	}
	return out, nil
}

// Origin returns the origin of one repository; false when it is local. It
// never calls a forge.
func (s *Service) Origin(ctx context.Context, repo string) (Origin, bool, error) {
	r, err := s.be.Repository(ctx, repo)
	if err != nil {
		return Origin{}, false, err
	}
	l, err := readLink(r)
	if err != nil || l == nil {
		return Origin{}, false, err
	}
	if _, ok := s.byName[l.Forge]; !ok {
		return Origin{}, false, nil
	}
	return s.originOf(ctx, l), true, nil
}

// fillLinkStatus fills in the status of an imported repository.
func (s *Service) fillLinkStatus(ctx context.Context, st *RepoStatus, l *Link) {
	st.Repo = l.Repo
	st.Imported = true
	st.LastError = l.LastError
	if !l.LastSync.IsZero() {
		t := l.LastSync
		st.LastSync = &t
	}
	branch := st.DefaultBranch
	if branch == "" {
		branch = l.DefaultBranch
	}
	if branch == "" {
		return
	}
	local, err := resolveRef(ctx, l.Dir, "refs/heads/"+branch)
	if err != nil || local == "" {
		return
	}
	upstream, err := resolveRef(ctx, l.Dir, namespace(l.Forge)+"heads/"+branch)
	if err != nil || upstream == "" {
		return
	}
	ahead, behind, err := aheadBehind(ctx, l.Dir, local, upstream)
	if err != nil {
		return
	}
	st.Ahead, st.Behind = &ahead, &behind
}

// ImportOptions select what Import imports.
type ImportOptions struct {
	// Paths are forge paths ("owner/repo") to import.
	Paths []string
	// All imports every listed repository include/exclude let through that
	// is not imported yet.
	All bool
	// Vcs is the VCS kind of the new repositories; empty is
	// proto.DefaultVCS.
	Vcs proto.VCS
}

// ImportStatus is the outcome of importing one repository.
type ImportStatus string

const (
	// ImportImported: the repository was created and synced.
	ImportImported ImportStatus = "imported"
	// ImportExists: the repository was already imported from this path.
	ImportExists ImportStatus = "exists"
	// ImportFailed: see Error. Nothing was left behind.
	ImportFailed ImportStatus = "failed"
)

// ImportResult is the outcome of importing one repository.
type ImportResult struct {
	Path   string       `json:"path"`
	Repo   string       `json:"repo,omitempty"`
	Status ImportStatus `json:"status"`
	Error  string       `json:"error,omitempty"`
	Sync   *SyncResult  `json:"sync,omitempty"`
}

// Import imports forge repositories as new Sundae repositories owned by
// user. It fails as a whole only when the forge cannot be listed; every
// repository gets its own result.
func (s *Service) Import(ctx context.Context, user proto.User, name string, opts ImportOptions) ([]ImportResult, error) {
	f, err := s.Forge(name)
	if err != nil {
		return nil, err
	}
	if _, err := proto.ParseVCS(string(opts.Vcs)); err != nil {
		return nil, err
	}
	if user == nil {
		return nil, errors.New("importing needs a user to own the repositories")
	}
	// The backend reads the acting user from the context, e.g. for the
	// webhook sent when a failed import is cleaned up.
	ctx = proto.WithUserContext(ctx, user)
	listing, err := s.Discover(ctx, name)
	if err != nil {
		return nil, err
	}

	var targets []RemoteRepo
	var results []ImportResult
	if opts.All {
		links, err := s.links(ctx, name)
		if err != nil {
			return nil, err
		}
		linked := map[string]bool{}
		for _, l := range links {
			linked[strings.ToLower(l.Path)] = true
		}
		for _, rr := range listing.Repos {
			if f.Matches(rr.Path) && !linked[strings.ToLower(rr.Path)] {
				targets = append(targets, rr)
			}
		}
	}
	for _, p := range opts.Paths {
		p = strings.Trim(strings.TrimSpace(p), "/")
		rr, ok := listing.Find(p)
		switch {
		case !ok:
			reason := "not listed by the forge; check the path and the token's access"
			for _, rj := range listing.Rejected {
				if strings.EqualFold(rj.Path, p) {
					reason = rj.Reason
				}
			}
			results = append(results, ImportResult{Path: p, Status: ImportFailed, Error: reason})
		case !f.Matches(rr.Path):
			results = append(results, ImportResult{Path: rr.Path, Status: ImportFailed, Error: "excluded by the forge's include/exclude patterns"})
		default:
			targets = append(targets, rr)
		}
	}

	seen := map[string]bool{}
	for _, rr := range targets {
		if seen[strings.ToLower(rr.Path)] {
			continue
		}
		seen[strings.ToLower(rr.Path)] = true
		results = append(results, s.importOne(ctx, user, f, rr, opts.Vcs))
	}
	return results, nil
}

// importOne imports one listed repository.
func (s *Service) importOne(ctx context.Context, user proto.User, f *Forge, rr RemoteRepo, vcs proto.VCS) (res ImportResult) {
	res = ImportResult{Path: rr.Path, Status: ImportFailed}
	defer func() {
		ev := Event{Kind: EventImport, Forge: f.Name(), Repo: res.Repo, Path: rr.Path, Sync: res.Sync}
		if res.Status == ImportFailed {
			ev.Err = errors.New(res.Error)
		}
		if res.Status != ImportExists {
			s.notify(ctx, ev)
		}
	}()

	name, err := f.RepoName(rr.Path)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Repo = name
	unlock := s.lock(name)
	defer unlock()

	existing, err := s.be.Repository(ctx, name)
	switch {
	case err == nil:
		l, lerr := readLink(existing)
		if lerr == nil && l != nil && l.Forge == f.Name() && strings.EqualFold(l.Path, rr.Path) {
			res.Status = ImportExists
			return res
		}
		res.Error = fmt.Sprintf("repository %s already exists and is not imported from %s:%s; it was left alone", name, f.Name(), rr.Path)
		return res
	case !errors.Is(err, proto.ErrRepoNotFound):
		res.Error = err.Error()
		return res
	}

	remote := f.remoteFor(rr.Path, "", rr.CloneURL)
	if err := s.checkRemote(f, remote); err != nil {
		res.Error = err.Error()
		return res
	}
	env, cleanup, err := f.gitEnv()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer cleanup()

	repo, err := s.be.CreateRepository(ctx, name, user, proto.RepositoryOptions{
		Private:     rr.Private,
		Description: rr.Description,
		Vcs:         vcs,
	})
	if err != nil {
		if errors.Is(err, proto.ErrRepoExist) {
			err = fmt.Errorf("repository %s already exists; it was left alone", name)
		}
		res.Error = err.Error()
		return res
	}
	fail := func(err error) ImportResult {
		if derr := s.be.DeleteRepository(ctx, name); derr != nil {
			s.logger.Error("failed to remove partially imported repository", "repo", name, "err", derr)
		}
		res.Error = err.Error()
		return res
	}

	gr, err := repo.Open()
	if err != nil {
		return fail(err)
	}
	if err := writeLink(ctx, gr.Path, f, rr, remote); err != nil {
		return fail(err)
	}
	sr, err := syncRepo(ctx, gr.Path, f.Name(), remote, env)
	if err != nil {
		return fail(err)
	}
	if err := fixHead(ctx, gr.Path, rr.DefaultBranch); err != nil {
		s.logger.Warn("could not point HEAD at the default branch", "repo", name, "branch", rr.DefaultBranch, "err", err)
	}
	if err := recordSync(ctx, gr.Path, time.Now(), nil); err != nil {
		s.logger.Warn("could not record sync", "repo", name, "err", err)
	}
	s.logger.Info("imported forge repository", "forge", f.Name(), "path", rr.Path, "repo", name)
	res.Status, res.Sync = ImportImported, sr
	return res
}

// RepoSyncResult is the outcome of syncing one imported repository.
type RepoSyncResult struct {
	Repo  string      `json:"repo"`
	Path  string      `json:"path"`
	Sync  *SyncResult `json:"sync,omitempty"`
	Error string      `json:"error,omitempty"`
}

// SyncForge refreshes the forge's listing and syncs every repository
// imported from it. It is what the scheduled job runs. A sync of the same
// forge already running is refused with ErrSyncRunning.
func (s *Service) SyncForge(ctx context.Context, name string) ([]RepoSyncResult, error) {
	f, err := s.Forge(name)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	st := s.states[name]
	if st.running {
		s.mu.Unlock()
		return nil, fmt.Errorf("forge %s: %w", name, ErrSyncRunning)
	}
	st.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		st.running = false
		s.mu.Unlock()
	}()

	var problems []string
	// Listing keeps default branches and `forge ls` counts fresh. Without a
	// token there is nothing to list, but imported public repositories still
	// sync.
	var listing *Listing
	if f.cfg.TokenFile != "" {
		listing, err = s.Discover(ctx, name)
		if err != nil {
			problems = append(problems, "discover: "+err.Error())
		}
	}

	links, err := s.links(ctx, name)
	if err != nil {
		return nil, err
	}
	var results []RepoSyncResult
	failed := 0
	for _, l := range links {
		r := s.syncLink(ctx, f, l, listing)
		if r.Error != "" {
			failed++
		}
		results = append(results, r)
	}
	if failed > 0 {
		problems = append(problems, fmt.Sprintf("%d of %d repositories failed to sync", failed, len(links)))
	}

	s.mu.Lock()
	st.lastSync = time.Now()
	st.lastErr = strings.Join(problems, "; ")
	s.mu.Unlock()
	return results, nil
}

// SyncRepo syncs one repository imported from the forge, named by its Sundae
// name or its forge path.
func (s *Service) SyncRepo(ctx context.Context, name, repo string) (RepoSyncResult, error) {
	f, err := s.Forge(name)
	if err != nil {
		return RepoSyncResult{}, err
	}
	links, err := s.links(ctx, name)
	if err != nil {
		return RepoSyncResult{}, err
	}
	want := utils.SanitizeRepo(repo)
	for _, l := range links {
		if l.Repo == want || strings.EqualFold(l.Path, strings.Trim(repo, "/")) {
			s.mu.Lock()
			listing := s.states[name].listing
			s.mu.Unlock()
			return s.syncLink(ctx, f, l, listing), nil
		}
	}
	return RepoSyncResult{}, fmt.Errorf("%w: %s (forge %s)", ErrRepoNotImported, repo, name)
}

// syncLink syncs one imported repository and records the outcome.
func (s *Service) syncLink(ctx context.Context, f *Forge, l *Link, listing *Listing) RepoSyncResult {
	res := RepoSyncResult{Repo: l.Repo, Path: l.Path}
	unlock := s.lock(l.Repo)
	defer unlock()

	sr, err := s.syncLinkLocked(ctx, f, l, listing)
	now := time.Now()
	if rerr := recordSync(ctx, l.Dir, now, err); rerr != nil {
		s.logger.Warn("could not record sync", "repo", l.Repo, "err", rerr)
	}
	if err != nil {
		res.Error = err.Error()
		s.logger.Warn("forge sync failed", "forge", f.Name(), "repo", l.Repo, "err", err)
	} else {
		res.Sync = sr
	}
	s.notify(ctx, Event{Kind: EventSync, Forge: f.Name(), Repo: l.Repo, Path: l.Path, Time: now, Err: err, Sync: sr})
	return res
}

func (s *Service) syncLinkLocked(ctx context.Context, f *Forge, l *Link, listing *Listing) (*SyncResult, error) {
	rr, listed := listing.Find(l.Path)
	remote, err := s.linkRemote(ctx, f, l, rr.CloneURL)
	if err != nil {
		return nil, err
	}
	env, cleanup, err := f.gitEnv()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	branch := l.DefaultBranch
	if listed && rr.DefaultBranch != "" && rr.DefaultBranch != branch {
		branch = rr.DefaultBranch
		if err := gitConfigSet(ctx, l.Dir, "forge.defaultbranch", branch); err != nil {
			s.logger.Warn("could not record default branch", "repo", l.Repo, "err", err)
		}
	}
	sr, err := syncRepo(ctx, l.Dir, f.Name(), remote, env)
	if err != nil {
		return nil, err
	}
	if err := fixHead(ctx, l.Dir, branch); err != nil {
		s.logger.Warn("could not point HEAD at the default branch", "repo", l.Repo, "err", err)
	}
	return sr, nil
}

// linkRemote returns the URL git uses for an imported repository, after
// checking it. When the forge's transport or SSH endpoint changed since the
// remote was written (ssh_key_file or ssh_url added, changed or removed),
// remote.<forge>.url is updated first. listed is the clone URL of the
// latest listing, "" when unknown. The ref namespace is not touched.
func (s *Service) linkRemote(ctx context.Context, f *Forge, l *Link, listed string) (string, error) {
	if l.Remote == "" {
		return "", fmt.Errorf("remote.%s.url is not set", f.Name())
	}
	want := f.remoteFor(l.Path, l.Remote, listed)
	if err := s.checkRemote(f, want); err != nil {
		return "", err
	}
	if want != l.Remote {
		if err := gitConfigSet(ctx, l.Dir, "remote."+f.Name()+".url", want); err != nil {
			return "", err
		}
		s.logger.Info("updated forge remote", "forge", f.Name(), "repo", l.Repo, "transport", f.Transport(),
			"from", redactArgs([]string{l.Remote})[0], "to", redactArgs([]string{want})[0])
		l.Remote = want
	}
	return want, nil
}

// PushBranch returns the branch Push pushes when given none: the branch
// HEAD points at, else the forge's default branch.
func (s *Service) PushBranch(ctx context.Context, repo string) (string, error) {
	r, err := s.be.Repository(ctx, repo)
	if err != nil {
		return "", err
	}
	l, err := readLink(r)
	if err != nil {
		return "", err
	}
	if l == nil {
		return "", fmt.Errorf("%w: %s", ErrNotLinked, r.Name())
	}
	if b := headBranch(ctx, l.Dir); b != "" {
		return b, nil
	}
	return l.DefaultBranch, nil
}

// Push pushes a branch of an imported repository to its forge. An empty
// branch pushes the branch HEAD points at. See pushBranch for when a push
// is refused.
func (s *Service) Push(ctx context.Context, repo, branch string, lease bool) (*PushResult, error) {
	r, err := s.be.Repository(ctx, repo)
	if err != nil {
		return nil, err
	}
	l, err := readLink(r)
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotLinked, r.Name())
	}
	f, err := s.Forge(l.Forge)
	if err != nil {
		return nil, err
	}

	unlock := s.lock(l.Repo)
	defer unlock()

	if branch == "" {
		branch = headBranch(ctx, l.Dir)
		if branch == "" {
			branch = l.DefaultBranch
		}
	}
	res, err := s.pushLocked(ctx, f, l, branch, lease)
	s.notify(ctx, Event{Kind: EventPush, Forge: f.Name(), Repo: l.Repo, Path: l.Path, Err: err, Push: res})
	return res, err
}

func (s *Service) pushLocked(ctx context.Context, f *Forge, l *Link, branch string, lease bool) (*PushResult, error) {
	if err := checkBranchName(ctx, l.Dir, branch); err != nil {
		return nil, err
	}
	s.mu.Lock()
	rr, _ := s.states[f.Name()].listing.Find(l.Path)
	s.mu.Unlock()
	remote, err := s.linkRemote(ctx, f, l, rr.CloneURL)
	if err != nil {
		return nil, err
	}
	env, cleanup, err := f.gitEnv()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return pushBranch(ctx, l.Dir, f.Name(), remote, branch, lease, env)
}

// checkBranchName refuses anything that is not a valid branch name, so a
// branch argument can never be read as an option or a refspec.
func checkBranchName(ctx context.Context, dir, branch string) error {
	if branch == "" || strings.HasPrefix(branch, "-") {
		return fmt.Errorf("invalid branch name %q", branch)
	}
	if _, err := runGit(ctx, dir, nil, localTimeout, "check-ref-format", "refs/heads/"+branch); err != nil {
		return fmt.Errorf("invalid branch name %q", branch)
	}
	return nil
}
