package ssh

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
	"charm.land/wish/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/sessions"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var tuiSessionCounter = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "soft_serve",
	Subsystem: "ssh",
	Name:      "tui_session_total",
	Help:      "The total number of TUI sessions",
}, []string{"repo", "term"})

var tuiSessionDuration = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "soft_serve",
	Subsystem: "ssh",
	Name:      "tui_session_seconds_total",
	Help:      "The total time spent in TUI sessions",
}, []string{"repo", "term"})

// SessionHandler is the soft-serve bubbletea ssh session handler.
// This middleware must be run after the ContextMiddleware. Its TUI cannot
// attach to agent sessions; TUIMiddleware's can.
func SessionHandler(s ssh.Session) *tea.Program {
	return sessionProgram(s, nil)
}

// sessionProgram returns the TUI program of a PTY session; nil when there
// is none. sw, when set, is the terminal the TUI hands to an attached
// agent session.
func sessionProgram(s ssh.Session, sw *terminalSwitch) *tea.Program {
	pty, _, active := s.Pty()
	if !active {
		return nil
	}

	ctx := s.Context()
	be := backend.FromContext(ctx)
	cfg := config.FromContext(ctx)
	cmd := s.Command()
	initialRepo := ""
	if len(cmd) == 1 {
		initialRepo = cmd[0]
	}

	auth := be.AccessLevelByPublicKey(ctx, initialRepo, s.PublicKey())
	if auth < access.ReadOnlyAccess {
		wish.Fatalln(s, proto.ErrUnauthorized)
		return nil
	}

	opts := programOptions(s, sw)
	opts = append(opts,
		tea.WithoutCatchPanics(),
		tea.WithContext(ctx),
		tea.WithColorProfile(common.DefaultColorProfile),
	)

	c := common.NewCommon(ctx, pty.Window.Width, pty.Window.Height)
	c.SetValue(common.ConfigKey, cfg)
	if sw != nil {
		c.SetValue(sessions.HandoffKey, sw)
		c.SetValue(sessions.TermKey, pty.Term)
	}
	m := NewUI(c, initialRepo)
	p := tea.NewProgram(m, opts...)

	tuiSessionCounter.WithLabelValues(initialRepo, pty.Term).Inc()

	start := time.Now()
	go func() {
		<-ctx.Done()
		tuiSessionDuration.WithLabelValues(initialRepo, pty.Term).Add(time.Since(start).Seconds())
	}()

	return p
}
