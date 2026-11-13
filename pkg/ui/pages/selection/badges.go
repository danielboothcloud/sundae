package selection

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/events"
	"github.com/charmbracelet/soft-serve/pkg/forge"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/forges"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/sessions"
)

// OriginSource is where the Repositories tab gets the origin badges (where
// each repository comes from, and how far it is from its forge). The one
// NewOriginSource returns reads the server's forge service; tests use
// fakes.
type OriginSource interface {
	// Origins returns the origin of every imported repository, by name;
	// the others are local. It never calls a forge.
	Origins(ctx context.Context) (map[string]forge.Origin, error)
	// Origin returns one repository's origin; false when it is local.
	Origin(ctx context.Context, repo string) (forge.Origin, bool, error)
	// Follow returns the forge events published from now on; nil when
	// there is no event bus.
	Follow() forges.Feed
}

// OriginSourceKey is the key of an OriginSource in the context of
// common.Common; OriginSourceFor returns it when there is one.
var OriginSourceKey = &struct{ string }{"selection-origins"}

// OriginSourceFor returns the origin badges' source: the one under
// OriginSourceKey, else NewOriginSource's. nil means no origin badges.
func OriginSourceFor(ctx context.Context) OriginSource {
	if src, ok := ctx.Value(OriginSourceKey).(OriginSource); ok {
		return src
	}
	return NewOriginSource(ctx)
}

// NewOriginSource returns the origin source of the SSH connection whose
// context is ctx; nil when no forge is configured: every repository is
// local then, and the badge says nothing.
func NewOriginSource(ctx context.Context) OriginSource {
	svc := forge.FromContext(ctx)
	if svc == nil {
		return nil
	}
	return &originSource{ctx: ctx, svc: svc}
}

type originSource struct {
	ctx context.Context
	svc *forge.Service
}

func (s *originSource) Origins(ctx context.Context) (map[string]forge.Origin, error) {
	return s.svc.Origins(ctx) //nolint:wrapcheck
}

func (s *originSource) Origin(ctx context.Context, repo string) (forge.Origin, bool, error) {
	return s.svc.Origin(ctx, repo) //nolint:wrapcheck
}

func (s *originSource) Follow() forges.Feed {
	bus := events.FromContext(s.ctx)
	if bus == nil {
		return nil
	}
	sub := bus.Subscribe(events.Named("tui-origins"), events.OfKind(events.KindForge), events.Buffer(64))
	// The subscription ends with the SSH connection, also when the TUI
	// does not get to close it.
	context.AfterFunc(s.ctx, sub.Cancel)
	return forges.BusFeed(sub)
}

// badges are the Repositories tab's badges (section 4), by repository
// name. The item delegate reads them when it renders, so they update
// without reloading the repository list.
type badges struct {
	// sessionsOn and originsOn report whether each badge is shown at all.
	sessionsOn bool
	originsOn  bool
	sessions   map[string]sessions.RepoBadge
	origins    map[string]forge.Origin
}

// sessionText is the session badge of a repository: "● 2 sessions";
// compact, "●2". "" when the user has none.
func (b *badges) sessionText(repo string, compact bool) (string, sessions.RepoBadge) {
	sb, ok := b.sessions[repo]
	if !ok || sb.Count == 0 {
		return "", sb
	}
	if compact {
		return fmt.Sprintf("%s%d", sb.State.Glyph(), sb.Count), sb
	}
	word := "sessions"
	if sb.Count == 1 {
		word = "session"
	}
	return fmt.Sprintf("%s %d %s", sb.State.Glyph(), sb.Count, word), sb
}

// originText is the origin badge of a repository: "local", or the forge's
// name with ↑n ↓n when the repository and the forge differ.
func (b *badges) originText(repo string) string {
	o, ok := b.origins[repo]
	if !ok {
		return "local"
	}
	parts := []string{o.Forge}
	if o.Ahead != nil && *o.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("↑%d", *o.Ahead))
	}
	if o.Behind != nil && *o.Behind > 0 {
		parts = append(parts, fmt.Sprintf("↓%d", *o.Behind))
	}
	return strings.Join(parts, " ")
}

// Messages of the origin badges.
type (
	originsMsg struct {
		origins map[string]forge.Origin
		err     error
	}
	originMsg struct {
		repo   string
		origin forge.Origin
		ok     bool
		err    error
	}
	originFeedMsg    struct{ evs []events.Event }
	originFeedEndMsg struct{}
)

// IsMsg reports whether msg belongs to the home page: its tabs' live
// updates and the results of what they started. The frame delivers them
// to the home page while a repository is shown.
func IsMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case originsMsg, originMsg, originFeedMsg, originFeedEndMsg, repoCreatedMsg:
		return true
	}
	return sessions.IsMsg(msg) || forges.IsMsg(msg)
}

func (s *Selection) originsCmd() tea.Cmd {
	src, ctx := s.origins, s.ctx
	return func() tea.Msg {
		o, err := src.Origins(ctx)
		if ctx.Err() != nil {
			return nil
		}
		return originsMsg{origins: o, err: err}
	}
}

func (s *Selection) originCmd(repo string) tea.Cmd {
	src, ctx := s.origins, s.ctx
	return func() tea.Msg {
		o, ok, err := src.Origin(ctx, repo)
		if ctx.Err() != nil {
			return nil
		}
		return originMsg{repo: repo, origin: o, ok: ok, err: err}
	}
}

func (s *Selection) originNextCmd() tea.Cmd {
	feed, ctx := s.originFeed, s.ctx
	if feed == nil {
		return nil
	}
	return func() tea.Msg {
		evs, err := feed.Next(ctx)
		if err != nil {
			return originFeedEndMsg{}
		}
		return originFeedMsg{evs: evs}
	}
}

// onBadgeMsg handles the origin badges' messages.
func (s *Selection) onBadgeMsg(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case originsMsg:
		if msg.err != nil {
			s.common.Logger.Debug("ui: cannot read repository origins", "err", msg.err)
			return nil
		}
		s.badges.origins = msg.origins
	case originMsg:
		switch {
		case msg.err != nil:
			s.common.Logger.Debug("ui: cannot read repository origin", "repo", msg.repo, "err", msg.err)
		case msg.ok:
			if s.badges.origins == nil {
				s.badges.origins = map[string]forge.Origin{}
			}
			s.badges.origins[msg.repo] = msg.origin
		default:
			delete(s.badges.origins, msg.repo)
		}
	case originFeedMsg:
		// An import, sync or push changes one repository: read just
		// its origin again.
		seen := map[string]bool{}
		var cmds []tea.Cmd
		for _, ev := range msg.evs {
			if ev.Forge == nil || ev.Forge.Repo == "" || seen[ev.Forge.Repo] {
				continue
			}
			seen[ev.Forge.Repo] = true
			cmds = append(cmds, s.originCmd(ev.Forge.Repo))
		}
		return tea.Batch(append(cmds, s.originNextCmd())...)
	case originFeedEndMsg:
		s.originFeed = nil
	}
	return nil
}

// refreshSessionBadges reads the session badges again.
func (s *Selection) refreshSessionBadges() {
	if s.sessions != nil {
		s.badges.sessions = s.sessions.RepoBadges()
	}
}
