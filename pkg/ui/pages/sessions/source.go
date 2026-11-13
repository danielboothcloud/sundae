package sessions

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/danielboothcloud/sundae/pkg/access"
	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/sessionctl"
	"github.com/danielboothcloud/sundae/pkg/signals"
)

// listTimeout bounds one listing of the user's sessions, so an unreachable
// gateway shows as such instead of a list that never loads.
const listTimeout = 15 * time.Second

// Source is where the Sessions tab gets its data and does its work. The
// one NewSource returns drives OpenShell through sessionctl, as the
// `session` commands do; tests use fakes.
type Source interface {
	// List returns the user's sessions as the gateway lists them, with
	// the status the Monitor last read. It fails when the gateway cannot
	// be reached.
	List(ctx context.Context) ([]sessionctl.Row, error)
	// Statuses returns the Monitor's last read of the user's sessions, by
	// sandbox name. It never calls the gateway.
	Statuses() map[string]openshell.SessionStatus
	// Follow returns the user's kept signals, newest first, and a feed of
	// those recorded after.
	Follow() ([]signals.Signal, Feed)
	// Keep is how many signals the server keeps.
	Keep() int
	// Repos returns the repositories the user may start sessions for,
	// sorted.
	Repos(ctx context.Context) ([]string, error)
	// Profiles returns the configured session profiles, sorted by name.
	Profiles() []Profile
	// RepoProfile returns the profile new sessions of the repository are
	// created from without one: the repository's own (`repo profile`),
	// else the sessions.repos match; "" for none.
	RepoProfile(repo string) string
	// Command returns the program a session created from the profile runs
	// ("" is no profile).
	Command(profile string) string
	// Prepare starts or resumes a session, as `session start` and
	// `session attach` do before they attach. The Attacher counts a bridge
	// on the session until it is closed.
	Prepare(ctx context.Context, req sessionctl.Request) (Attacher, error)
	// End ends a session, deleting its sandbox. It reports whether there
	// was one. When the session's S3 snapshot fails it is not ended
	// (openshell.ErrSnapshotFailed), unless force is set.
	End(ctx context.Context, repo, name string, force bool) (bool, error)
	// SnapshotsOnEnd reports whether ending a session saves /sandbox to
	// S3 first.
	SnapshotsOnEnd() bool
	// Park parks a running session. It reports whether it parked it.
	Park(ctx context.Context, repo, name string) (bool, error)
	// AttachCommand is the command that attaches to the session from a
	// terminal.
	AttachCommand(repo, name string) string
}

// Feed delivers signals as they are recorded; *signals.Feed satisfies it.
type Feed interface {
	// Next waits for signals and returns them, oldest first. It returns
	// ctx's error when ctx is done and signals.ErrClosed when the feed
	// ended.
	Next(ctx context.Context) ([]signals.Signal, error)
}

// Attacher is a prepared session, ready to attach to.
type Attacher interface {
	// Session is the prepared session.
	Session() *openshell.Session
	// Attach bridges term to the session until the user detaches, the
	// program exits or ctx is done; see sessionctl.Prepared.Attach.
	Attach(ctx context.Context, term openshell.Terminal) (int, error)
	// Close releases the session's bridge and client. It is safe to call
	// more than once.
	Close()
}

// Profile is a configured session profile.
type Profile struct {
	Name    string
	Image   string
	Command string
}

// NewClient creates the OpenShell client of an operation. Tests may
// replace it.
var NewClient = func(cfg *config.Config) (openshell.Client, error) {
	return openshell.New(cfg.OpenShell)
}

// SourceKey is the key of a Source in the context of common.Common. When
// one is there, SourceFor returns it instead of building one: an embedder
// (or a test) supplies the tab's data that way.
var SourceKey = &struct{ string }{"sessions-source"}

// SourceFor returns the Sessions tab's Source for the context of
// common.Common: the one under SourceKey, else NewSource's. nil means the
// tab is hidden.
func SourceFor(ctx context.Context) Source {
	if src, ok := ctx.Value(SourceKey).(Source); ok {
		return src
	}
	return NewSource(ctx)
}

// NewSource returns the Source of the SSH connection whose context is ctx,
// or nil when it has no sessions: OpenShell is disabled, or the user is
// anonymous.
func NewSource(ctx context.Context) Source {
	cfg := config.FromContext(ctx)
	user := proto.UserFromContext(ctx)
	if cfg == nil || !cfg.OpenShell.Enabled || user == nil {
		return nil
	}
	return &source{ctx: ctx, cfg: cfg, user: user}
}

// source is the Source backed by OpenShell, the Monitor and the signals
// History of the SSH connection's context.
type source struct {
	// ctx is the SSH connection's context: it carries the backend,
	// config, user, event bus, Monitor and signals History. Operations
	// run under the context they are given, which derives from it.
	ctx  context.Context
	cfg  *config.Config
	user proto.User
}

// withClient runs fn with a new OpenShell client and its Manager, closing
// the client after.
func (s *source) withClient(fn func(*openshell.Manager) error) error {
	client, err := NewClient(s.cfg)
	if err != nil {
		return err
	}
	defer client.Close() //nolint:errcheck
	return fn(sessionctl.NewManager(s.ctx, client))
}

func (s *source) List(ctx context.Context) (rows []sessionctl.Row, err error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	err = s.withClient(func(mgr *openshell.Manager) error {
		rows, err = sessionctl.List(ctx, mgr, openshell.MonitorFromContext(s.ctx), s.user)
		return err //nolint:wrapcheck
	})
	return rows, err
}

func (s *source) Statuses() map[string]openshell.SessionStatus {
	out := map[string]openshell.SessionStatus{}
	for _, st := range openshell.MonitorFromContext(s.ctx).ForUser(s.user.ID()) {
		out[st.Session.Sandbox] = st
	}
	return out
}

func (s *source) Follow() ([]signals.Signal, Feed) {
	return signals.FromContext(s.ctx).Follow(signals.Query{UserID: s.user.ID()})
}

func (s *source) Keep() int {
	if k := signals.FromContext(s.ctx).Keep(); k > 0 {
		return k
	}
	return config.DefaultSignalsKeep
}

func (s *source) Repos(ctx context.Context) ([]string, error) {
	be := backend.FromContext(s.ctx)
	repos, err := be.Repositories(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	var names []string
	for _, r := range repos {
		if be.AccessLevelForUser(ctx, r.Name(), s.user) >= access.ReadOnlyAccess {
			names = append(names, r.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

func (s *source) Profiles() []Profile {
	var out []Profile
	for _, name := range slices.Sorted(maps.Keys(s.cfg.Sessions.Profiles)) {
		p := s.cfg.Sessions.Profiles[name]
		out = append(out, Profile{Name: name, Image: p.Image, Command: p.Command})
	}
	return out
}

func (s *source) RepoProfile(repo string) string {
	p, err := backend.FromContext(s.ctx).RepoProfile(s.ctx, repo)
	if _, ok := s.cfg.Sessions.Profiles[p]; err == nil && ok {
		return p
	}
	return s.cfg.Sessions.RepoProfile(repo)
}

func (s *source) Command(profile string) string {
	if p, ok := s.cfg.Sessions.Profiles[profile]; ok && p.Command != "" {
		return p.Command
	}
	return s.cfg.OpenShell.Command
}

func (s *source) Prepare(ctx context.Context, req sessionctl.Request) (Attacher, error) {
	client, err := NewClient(s.cfg)
	if err != nil {
		return nil, err
	}
	p, err := sessionctl.Prepare(ctx, sessionctl.NewManager(s.ctx, client), req)
	if err != nil {
		client.Close()  //nolint:errcheck,gosec
		return nil, err //nolint:wrapcheck
	}
	return &attacher{Prepared: p, client: client}, nil
}

func (s *source) End(ctx context.Context, repo, name string, force bool) (existed bool, err error) {
	err = s.withClient(func(mgr *openshell.Manager) error {
		res, err := mgr.EndSession(ctx, s.user, repo, name, openshell.EndOptions{Force: force})
		existed = res.Existed
		return err //nolint:wrapcheck
	})
	return existed, err
}

func (s *source) SnapshotsOnEnd() bool { return s.cfg.Snapshots.S3.Snapshots(config.SnapshotOnEnd) }

func (s *source) Park(ctx context.Context, repo, name string) (parked bool, err error) {
	err = s.withClient(func(mgr *openshell.Manager) error {
		parked, err = mgr.Park(ctx, s.user, repo, name)
		return err //nolint:wrapcheck
	})
	return parked, err
}

func (s *source) AttachCommand(repo, name string) string {
	return sessionctl.AttachCommand(s.cfg.SSH.PublicURL, repo, name)
}

// attacher is a sessionctl.Prepared with the client it was prepared with.
type attacher struct {
	*sessionctl.Prepared
	client openshell.Client
	once   sync.Once
}

func (a *attacher) Session() *openshell.Session { return a.Prepared.Session }

func (a *attacher) Close() {
	a.once.Do(func() {
		a.Prepared.Close()
		a.client.Close() //nolint:errcheck,gosec
	})
}
