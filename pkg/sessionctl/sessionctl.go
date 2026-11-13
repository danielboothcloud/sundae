// Package sessionctl drives a user's OpenShell agent sessions on behalf of
// an SSH connection: what the `session` commands and the TUI share. It
// builds the Manager from the connection's context, lists the user's
// sessions with the status the Monitor last read, prepares a session for
// attaching (counting its bridge with the Monitor), and bridges a terminal
// to it.
package sessionctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielboothcloud/sundae/pkg/access"
	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/forge"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/signals"
	"github.com/danielboothcloud/sundae/pkg/utils"
)

// ErrSessionLost is returned when the sandbox stream ends without reporting
// an exit status.
var ErrSessionLost = errors.New("session ended without an exit status")

// NewManager returns a Manager for an SSH connection, publishing on the
// context's event bus and coordinating with the context's Monitor.
func NewManager(ctx context.Context, client openshell.Client) *openshell.Manager {
	return openshell.NewManager(client, backend.FromContext(ctx), config.FromContext(ctx),
		openshell.WithEvents(events.FromContext(ctx)),
		openshell.WithMonitor(openshell.MonitorFromContext(ctx)),
		openshell.WithProviderReconciler(openshell.ProviderReconcilerFromContext(ctx)))
}

// Row is one of a user's sessions: what the gateway lists, and what the
// Monitor last read from the sandbox (nil when it has nothing yet).
type Row struct {
	Info   openshell.Info
	Status *openshell.SessionStatus
}

// Record returns the session's OSC 7501 record; none when unknown.
func (r Row) Record() events.Record {
	if r.Status == nil {
		return events.Record{}
	}
	return r.Status.Record
}

// State returns the session's agent state; StateNone when unknown.
func (r Row) State() events.State { return r.Record().State }

// Polled reports whether the Monitor has read the session's status.
func (r Row) Polled() bool { return r.Status != nil && !r.Status.Polled.IsZero() }

// Parked reports whether the session is parked: its sandbox suspended, or
// the session parked to its S3 snapshot.
func (r Row) Parked() bool { return openshell.IsParkedWord(openshell.PhaseWord(r.Info.Phase)) }

// ParkedToSnapshot reports whether the session is parked to its S3
// snapshot: it has no sandbox, and its program was stopped.
func (r Row) ParkedToSnapshot() bool { return r.Info.Snapshot != nil }

// Note says what a session's state does not: for a session parked to its
// snapshot, that its program was stopped. "" for none.
func (r Row) Note() string {
	if r.ParkedToSnapshot() {
		return "program stopped by the snapshot park; attach restarts it"
	}
	return ""
}

// Changed returns when the session's status last changed; zero when not
// known.
func (r Row) Changed() time.Time {
	if r.Status == nil {
		return time.Time{}
	}
	return r.Status.Changed
}

// Lifecycle returns the words shown after the state: the sandbox lifecycle
// (parked, starting), else whether the program exited and whether the
// status is stale.
func (r Row) Lifecycle() []string {
	if word := openshell.PhaseWord(r.Info.Phase); word != "" {
		return []string{word}
	}
	var words []string
	if r.Status != nil && r.Status.Exited {
		words = append(words, "exited")
	}
	if r.Status != nil && r.Status.Stale && !r.Status.Parked {
		words = append(words, "stale")
	}
	return words
}

// Words renders the session's state without its glyph: the word, the
// progress, what a blocked program waits for and the lifecycle words, e.g.
// "blocked · permission · parked" or "working 40%".
func (r Row) Words() string {
	return strings.Join(append([]string{signals.RecordWords(r.Record())}, r.Lifecycle()...), " · ")
}

// Label renders the session's state with its glyph, as `session ls` shows
// it, e.g. "▲ blocked · permission · parked".
func (r Row) Label() string {
	return r.State().Glyph() + " " + r.Words()
}

// List returns the user's sessions as the gateway lists them, each with the
// status the Monitor last read, sorted by state (blocked, error, done,
// working, idle, none) and within a state by repository and name. It never
// resumes a parked session.
func List(ctx context.Context, mgr *openshell.Manager, mon *openshell.Monitor, user proto.User) ([]Row, error) {
	infos, err := mgr.List(ctx, user)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	statuses := map[string]*openshell.SessionStatus{}
	for _, st := range mon.ForUser(user.ID()) {
		statuses[st.Session.Sandbox] = &st
	}
	rows := make([]Row, len(infos))
	for i, info := range infos {
		rows[i] = Row{Info: info, Status: statuses[info.Sandbox]}
		if p := info.Snapshot; p != nil && rows[i].Status == nil {
			// No sandbox to read: the status it had when it was parked.
			rows[i].Status = &openshell.SessionStatus{
				Session: events.Session{User: p.User, Repo: p.Repo, Name: p.Session, Sandbox: p.Sandbox},
				UserID:  p.UserID,
				Profile: p.Profile,
				Phase:   openshell.PhaseSnapshot,
				Parked:  true,
				Record:  p.Record,
				Changed: p.Parked,
			}
		}
	}
	// List sorts by repository and name; keep that within a state.
	slices.SortStableFunc(rows, func(a, b Row) int {
		return a.State().Rank() - b.State().Rank()
	})
	return rows, nil
}

// Since renders how long ago t was: "now" under a minute, then minutes,
// hours and days; "-" for the zero time.
func Since(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
}

// Request describes a session to start or resume for attaching.
type Request struct {
	// Repo is the repository as the user named it.
	Repo string
	// Name is the session name; "" is openshell.DefaultSessionName.
	Name string
	// Profile and Image apply when Create makes a new session.
	Profile string
	Image   string
	// Create starts the session, creating it when absent; otherwise the
	// session must exist and is resumed.
	Create bool
	// Term is the client's TERM.
	Term string
	// Restore says whether a snapshot is restored into a newly created
	// sandbox; see openshell.RestoreMode.
	Restore openshell.RestoreMode
	// Progress receives human-readable progress lines. May be nil.
	Progress io.Writer
}

// Prepared is a session started or resumed and ready to attach to. Its
// bridge is counted with the Monitor, so it is not parked, until Close.
type Prepared struct {
	Session     *openshell.Session
	mgr         *openshell.Manager
	closeBridge func()
}

// Close stops counting the bridge. It is safe to call more than once.
func (p *Prepared) Close() {
	if p != nil && p.closeBridge != nil {
		p.closeBridge()
	}
}

// Prepare checks that the caller may read the repository, counts a bridge
// on the session with the context's Monitor, and starts (Create) or resumes
// the session. On success the bridge stays counted until the Prepared is
// closed; on error it is closed already.
func Prepare(ctx context.Context, mgr *openshell.Manager, req Request) (_ *Prepared, err error) {
	be := backend.FromContext(ctx)
	user := proto.UserFromContext(ctx)
	if user == nil {
		return nil, proto.ErrUnauthorized
	}
	if be.AccessLevelForUser(ctx, utils.SanitizeRepo(req.Repo), user) < access.ReadOnlyAccess {
		return nil, proto.ErrRepoNotFound
	}
	rr, err := be.Repository(ctx, req.Repo)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	// Count the bridge from here until it closes, whichever way this
	// returns, so the session is not parked while it is prepared or
	// attached. An invalid name counts against a sandbox that cannot exist,
	// briefly, and Start rejects it.
	closeBridge := openshell.MonitorFromContext(ctx).OpenBridge(ctx,
		openshell.SessionSandboxName(user.Username(), rr.Name(), req.Name))
	defer func() {
		if err != nil {
			closeBridge()
		}
	}()

	// A new session's profile: --profile, else the repository's own
	// (`repo profile`), else the sessions.repos match, else none.
	var repoProfile string
	if req.Create && req.Profile == "" {
		if repoProfile, err = be.RepoProfile(ctx, rr.Name()); err != nil {
			return nil, err //nolint:wrapcheck
		}
	}

	// Commits in the session are made as the repository's forge identity,
	// else the default one, else as whatever the sandbox says.
	identity, err := GitIdentity(config.FromContext(ctx), rr)
	if err != nil {
		return nil, err
	}

	opts := openshell.StartOptions{
		User:        user,
		Repo:        rr.Name(),
		VCS:         rr.Vcs(),
		Name:        req.Name,
		Profile:     req.Profile,
		RepoProfile: repoProfile,
		Image:       req.Image,
		Term:        req.Term,
		Restore:     req.Restore,
		GitIdentity: identity,
		Progress:    req.Progress,
	}
	var s *openshell.Session
	if req.Create {
		s, err = mgr.Start(ctx, opts)
	} else {
		s, err = mgr.Resume(ctx, opts)
	}
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	return &Prepared{Session: s, mgr: mgr, closeBridge: closeBridge}, nil
}

// GitIdentity returns the git identity for sessions of the repository: the
// git_identity of the forge it was imported from, else
// sessions.git_identity, else none (the zero GitIdentity). A repository
// whose git config cannot be read is an error rather than a session that
// commits as the wrong identity.
func GitIdentity(cfg *config.Config, rr proto.Repository) (config.GitIdentity, error) {
	if cfg == nil {
		return config.GitIdentity{}, nil
	}
	name, err := forge.LinkedForge(rr)
	if err != nil {
		return config.GitIdentity{}, fmt.Errorf("read the forge link of %s: %w", rr.Name(), err)
	}
	return cfg.SessionGitIdentity(name), nil
}

// Attach bridges term to the session until the user detaches (~.), the
// program exits, or ctx is done. It returns the program's exit status when
// it exited with one other than 0, and an error when the stream failed or
// ended without an exit status (ErrSessionLost). Cancelling ctx is not an
// error.
func (p *Prepared) Attach(ctx context.Context, term openshell.Terminal) (int, error) {
	pty, _, _ := term.Pty()
	is, err := p.mgr.Attach(ctx, p.Session, uint32(pty.Window.Width), uint32(pty.Window.Height)) //nolint:gosec
	if err != nil {
		return 0, err //nolint:wrapcheck
	}

	code, err := openshell.Bridge(ctx, term, is)
	if code < 0 && ctx.Err() == nil {
		// The stream ended without an exit status: surface why.
		if err == nil {
			err = ErrSessionLost
		}
		return 0, err
	}
	if err != nil && ctx.Err() == nil && code == 0 {
		return 0, err
	}
	return max(code, 0), nil
}

// AttachCommand is the command that attaches to the user's named session
// of a repository from a terminal, for a server whose public SSH URL is
// publicURL: "ssh -t [-p port] host session attach repo -n name".
func AttachCommand(publicURL, repo, name string) string {
	if name == "" {
		name = openshell.DefaultSessionName
	}
	host, port := publicURL, ""
	if u, err := url.Parse(publicURL); err == nil && u.Host != "" {
		host, port = u.Hostname(), u.Port()
		if u.User != nil {
			host = u.User.Username() + "@" + host
		}
	}
	cmd := "ssh -t "
	if port != "" && port != "22" {
		cmd += "-p " + port + " "
	}
	return fmt.Sprintf("%s%s session attach %s -n %s", cmd, host, repo, name)
}
