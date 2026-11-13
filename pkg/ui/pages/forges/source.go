package forges

import (
	"context"
	"errors"

	"charm.land/ssh"
	"github.com/danielboothcloud/sundae/pkg/access"
	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/forge"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/sshutils"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

// Source is where the Forges tab gets its data and does its work. The one
// NewSource returns drives the server's forge.Service, as the `forge`
// commands do; tests use fakes.
type Source interface {
	// Status returns every configured forge's status, in configuration
	// order. It never calls a forge.
	Status(ctx context.Context) ([]forge.ForgeStatus, error)
	// Repos returns a forge's repositories as last listed, with the
	// imported ones. It never calls the forge.
	Repos(ctx context.Context, forge string) ([]forge.RepoStatus, error)
	// Discover lists a forge's repositories now.
	Discover(ctx context.Context, forge string) error
	// Import imports forge repositories by path.
	Import(ctx context.Context, forge string, paths []string) ([]forge.ImportResult, error)
	// SyncForge syncs every repository imported from a forge.
	SyncForge(ctx context.Context, forge string) ([]forge.RepoSyncResult, error)
	// SyncRepo syncs one imported repository.
	SyncRepo(ctx context.Context, forge, repo string) (forge.RepoSyncResult, error)
	// PushBranch returns the branch a push of the repository pushes.
	PushBranch(ctx context.Context, repo string) (string, error)
	// Push pushes a branch of an imported repository to its forge, as a
	// fast-forward only.
	Push(ctx context.Context, repo, branch string) (*forge.PushResult, error)
	// Follow returns the forge events published from now on; nil when
	// there is no event bus.
	Follow() Feed
	// CloneCommand is the command that clones a Sundae repository.
	CloneCommand(repo string) string
}

// Feed delivers events as they are published.
type Feed interface {
	// Next waits for events and returns them, oldest first. It returns
	// ctx's error when ctx is done, and ErrFeedClosed once the feed ended.
	Next(ctx context.Context) ([]events.Event, error)
	// Close ends the feed.
	Close()
}

// ErrFeedClosed is returned by Feed.Next once the feed ended.
var ErrFeedClosed = errors.New("feed closed")

// SourceKey is the key of a Source in the context of common.Common. When
// one is there, SourceFor returns it instead of building one.
var SourceKey = &struct{ string }{"forges-source"}

// SourceFor returns the Forges tab's Source for the context of
// common.Common: the one under SourceKey, else NewSource's. nil means the
// tab is hidden.
func SourceFor(ctx context.Context) Source {
	if src, ok := ctx.Value(SourceKey).(Source); ok {
		return src
	}
	return NewSource(ctx)
}

// NewSource returns the Source of the SSH connection whose context is ctx,
// or nil when the tab is not for this user: no forge is configured, or the
// user is not a server admin. Listing, importing and syncing use the
// server's forge tokens and see everything they can, so like the `forge`
// commands they are for server admins only.
func NewSource(ctx context.Context) Source {
	svc := forge.FromContext(ctx)
	cfg := config.FromContext(ctx)
	if svc == nil || cfg == nil || !IsServerAdmin(ctx) {
		return nil
	}
	return &source{ctx: ctx, svc: svc, cfg: cfg}
}

// IsServerAdmin reports whether the user of the SSH connection whose
// context is ctx is a server admin: their key is an admin key from the
// config, or their account has the admin flag. It is the rule the `forge`
// command's admin-only subcommands use.
func IsServerAdmin(ctx context.Context) bool {
	if cfg := config.FromContext(ctx); cfg != nil {
		if pk, ok := ctx.Value(ssh.ContextKeyPublicKey).(ssh.PublicKey); ok && pk != nil {
			for _, k := range cfg.AdminKeys() {
				if sshutils.KeysEqual(pk, k) {
					return true
				}
			}
		}
	}
	user := proto.UserFromContext(ctx)
	return user != nil && user.IsAdmin()
}

// source is the Source backed by the forge service of the SSH
// connection's context.
type source struct {
	// ctx is the SSH connection's context: it carries the backend,
	// config, user and event bus. Operations run under the context they
	// are given, which derives from it.
	ctx context.Context
	svc *forge.Service
	cfg *config.Config
}

func (s *source) Status(ctx context.Context) ([]forge.ForgeStatus, error) {
	return s.svc.Status(ctx) //nolint:wrapcheck
}

func (s *source) Repos(ctx context.Context, name string) ([]forge.RepoStatus, error) {
	return s.svc.KnownRepos(ctx, name, false) //nolint:wrapcheck
}

func (s *source) Discover(ctx context.Context, name string) error {
	_, err := s.svc.Discover(ctx, name)
	return err //nolint:wrapcheck
}

func (s *source) Import(ctx context.Context, name string, paths []string) ([]forge.ImportResult, error) {
	return s.svc.Import(ctx, proto.UserFromContext(s.ctx), name, forge.ImportOptions{Paths: paths}) //nolint:wrapcheck
}

func (s *source) SyncForge(ctx context.Context, name string) ([]forge.RepoSyncResult, error) {
	return s.svc.SyncForge(ctx, name) //nolint:wrapcheck
}

func (s *source) SyncRepo(ctx context.Context, name, repo string) (forge.RepoSyncResult, error) {
	return s.svc.SyncRepo(ctx, name, repo) //nolint:wrapcheck
}

func (s *source) PushBranch(ctx context.Context, repo string) (string, error) {
	return s.svc.PushBranch(ctx, repo) //nolint:wrapcheck
}

// Push needs write access to the repository, as `forge push` does; a
// server admin always has it.
func (s *source) Push(ctx context.Context, repo, branch string) (*forge.PushResult, error) {
	be := backend.FromContext(s.ctx)
	if be == nil || be.AccessLevelForUser(ctx, repo, proto.UserFromContext(s.ctx)) < access.ReadWriteAccess {
		return nil, proto.ErrUnauthorized
	}
	return s.svc.Push(ctx, repo, branch, false) //nolint:wrapcheck
}

func (s *source) Follow() Feed {
	bus := events.FromContext(s.ctx)
	if bus == nil {
		return nil
	}
	sub := bus.Subscribe(events.Named("tui-forges"), events.OfKind(events.KindForge), events.Buffer(64))
	// The subscription ends with the SSH connection, also when the TUI
	// does not get to close it.
	context.AfterFunc(s.ctx, sub.Cancel)
	return &busFeed{sub: sub}
}

func (s *source) CloneCommand(repo string) string {
	vcs := proto.DefaultVCS
	if be := backend.FromContext(s.ctx); be != nil {
		if r, err := be.Repository(s.ctx, repo); err == nil {
			vcs = r.Vcs()
		}
	}
	if vcs == proto.VCSJJ {
		return "jj git clone " + common.RepoURL(s.cfg.SSH.PublicURL, repo)
	}
	return "git clone " + common.RepoURL(s.cfg.SSH.PublicURL, repo)
}

// BusFeed returns a Feed of a bus subscription.
func BusFeed(sub *events.Subscription) Feed { return &busFeed{sub: sub} }

// busFeed is a Feed of a bus subscription.
type busFeed struct{ sub *events.Subscription }

func (f *busFeed) Next(ctx context.Context) ([]events.Event, error) {
	var out []events.Event
	select {
	case ev, ok := <-f.sub.C:
		if !ok {
			return nil, ErrFeedClosed
		}
		out = append(out, ev)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// Take what else is buffered, so a burst (a forge-wide sync) is one
	// update.
	for {
		select {
		case ev, ok := <-f.sub.C:
			if !ok {
				return out, nil
			}
			out = append(out, ev)
		default:
			return out, nil
		}
	}
}

func (f *busFeed) Close() { f.sub.Cancel() }
