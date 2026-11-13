package openshell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/anmitsu/go-shlex"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/proto"
)

// SandboxRoot is the directory repositories are cloned into. It is also the
// sandbox user's home.
const SandboxRoot = "/sandbox"

// tokenPrefix prefixes the name of access tokens issued to sandboxes.
const tokenPrefix = "openshell:"

// ErrNoSession is returned when attaching to a session that does not exist.
var ErrNoSession = errors.New("no such session")

// TokenStore issues and revokes Sundae access tokens. It is satisfied by
// *backend.Backend.
type TokenStore interface {
	CreateAccessToken(ctx context.Context, user proto.User, name string, expiresAt time.Time) (string, error)
	ListAccessTokens(ctx context.Context, user proto.User) ([]proto.AccessToken, error)
	DeleteAccessToken(ctx context.Context, user proto.User, id int64) error
}

// Manager drives the session lifecycle. It holds no session state; the
// gateway and the sandboxes are the source of truth.
type Manager struct {
	client   Client
	tokens   TokenStore
	image    string
	command  string
	sessions config.SessionsConfig
	cloneURL string
	tokenTTL time.Duration
	now      func() time.Time
	// osewaBinaryDir holds the osewa release builds to upload.
	osewaBinaryDir string
	// osewaPort is the loopback port of the sandbox's osewa server.
	osewaPort int
	// osewaSHA256 maps an architecture to the checksum of its osewa build.
	osewaSHA256 map[string]string
	// bus receives lifecycle events. nil drops them.
	bus *events.Bus
	// monitor tracks bridges and parks sessions; nil when there is none.
	monitor *Monitor
	// snaps is the S3 snapshot store; nil when snapshots are off.
	snaps *snapshotStore
	// logger reports what does not fail a command.
	logger *log.Logger
}

// ManagerOption configures a Manager.
type ManagerOption func(*Manager)

// WithEvents publishes the session lifecycle (created, resumed, ready,
// ended, failed) on bus. Without it, or with a nil bus, nothing is
// published.
func WithEvents(bus *events.Bus) ManagerOption {
	return func(m *Manager) { m.bus = bus }
}

// WithMonitor makes the Manager coordinate with the server's Monitor:
// parking goes through it (so a session with an open bridge is not parked),
// and starting a session tells it. nil is allowed.
func WithMonitor(mon *Monitor) ManagerOption {
	return func(m *Manager) { m.monitor = mon }
}

// NewManager returns a Manager using client for sandboxes and tokens for git
// credentials, configured from cfg.
func NewManager(client Client, tokens TokenStore, cfg *config.Config, opts ...ManagerOption) *Manager {
	m := &Manager{
		client:         client,
		tokens:         tokens,
		image:          cfg.OpenShell.Image,
		command:        cfg.OpenShell.Command,
		sessions:       cfg.Sessions,
		cloneURL:       cfg.OpenShellCloneURL(),
		tokenTTL:       cfg.OpenShell.TokenTTL,
		now:            time.Now,
		osewaBinaryDir: cfg.Sessions.Osewa.BinaryDir,
		osewaPort:      cfg.Sessions.Osewa.Port,
		osewaSHA256:    osewaSHA256,
		snaps:          newSnapshotStore(cfg),
		logger:         log.Default().WithPrefix("sessions"),
	}
	if m.osewaBinaryDir == "" {
		m.osewaBinaryDir = config.DefaultOsewaBinaryDir
	}
	if m.osewaPort == 0 {
		m.osewaPort = config.DefaultOsewaPort
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// lifecycle publishes a lifecycle step of one of the user's sessions.
func (m *Manager) lifecycle(user proto.User, repo, name, sandbox string, step events.Lifecycle, op string, err error) {
	m.publish(user, events.SessionLifecycle{
		Session: events.Session{User: user.Username(), Repo: repo, Name: name, Sandbox: sandbox},
		Step:    step,
		Op:      op,
	}, err)
}

// publish publishes a lifecycle step, with err as its reason.
func (m *Manager) publish(user proto.User, l events.SessionLifecycle, err error) {
	if m.bus == nil {
		return
	}
	if err != nil {
		l.Reason = err.Error()
	}
	m.bus.Publish(events.SessionLifecycleEvent(user.ID(), l))
}

// StartOptions configures Start and Resume.
type StartOptions struct {
	// User is the authenticated caller. Required.
	User proto.User
	// Repo is the canonical repository name.
	Repo string
	// VCS selects the clone tool.
	VCS proto.VCS
	// Name is the session name. Empty is DefaultSessionName.
	Name string
	// Profile names the sessions.profiles entry to create the session from.
	// Empty uses RepoProfile, else the sessions.repos match for Repo, if
	// any. Start only; it is ignored, with a warning, for a session that
	// already exists.
	Profile string
	// RepoProfile is the repository's own default profile (`repo
	// profile`), used when Profile is empty. One that is no longer in
	// sessions.profiles is skipped with a warning.
	RepoProfile string
	// Image overrides the image when the sandbox is created. Start only;
	// ignored, with a warning, for a session that already exists.
	Image string
	// Term is the client's TERM, given to the attach client.
	Term string
	// Restore says whether a snapshot of the session is restored into a
	// newly created sandbox (snapshots.s3). The zero value restores it
	// unless the new sandbox already holds earlier work.
	Restore RestoreMode
	// GitIdentity is the name and email commits in the checkout are made
	// as: set in the checkout's git and jj repository config on every
	// start and attach, and given to the first clone. The zero value
	// leaves the sandbox's identity alone.
	GitIdentity config.GitIdentity
	// Progress receives human-readable progress messages. May be nil.
	Progress io.Writer
}

// Session is a started session, ready to attach to.
type Session struct {
	// Name is the session name.
	Name string
	// Repo is the repository name.
	Repo string
	// Sandbox is the sandbox name.
	Sandbox string
	// Profile is the profile the session was created from; empty for none.
	Profile string
	// Created reports whether the sandbox was created by this call.
	Created bool
	// Cloned reports whether this call cloned the repository.
	Cloned bool
	// WorkDir is the repository checkout inside the sandbox.
	WorkDir string
	// Command is the session's program, running in an osewa session.
	Command []string
	// Started reports whether this call started the program, rather than
	// finding it running.
	Started bool
	// Unparked reports whether this call resumed the session from parked.
	Unparked bool
	// Restored is the snapshot this call restored /sandbox from; nil when
	// it restored none.
	Restored *SnapshotInfo
	// Osewa is the path of the osewa binary in the sandbox.
	Osewa string
	// OsewaID is the id of the osewa session holding the program.
	OsewaID string
	// Env is the environment of the attach client. It holds no
	// credentials.
	Env map[string]string
}

// AttachCommand is the command Attach runs in the sandbox.
func (s *Session) AttachCommand() []string {
	return OsewaAttachCommand(s.Osewa, s.OsewaID)
}

// Info describes one of a user's sessions.
type Info struct {
	// Repo is the repository name.
	Repo string
	// Name is the session name.
	Name string
	// Profile is the profile the session was created from; empty for none.
	Profile string
	// Sandbox is the sandbox name.
	Sandbox string
	// Phase is the sandbox phase; PhaseSnapshot for a session parked to
	// its snapshot, which has no sandbox.
	Phase string
	// Snapshot is set when the session is parked to its S3 snapshot.
	Snapshot *ParkedSnapshot
}

// TokenName returns the access token name used for a sandbox.
func TokenName(sandbox string) string { return tokenPrefix + sandbox }

// sessionName returns name, or DefaultSessionName for "", after checking
// it.
func sessionName(name string) (string, error) {
	if name == "" {
		return DefaultSessionName, nil
	}
	if !ValidSessionName(name) {
		return "", fmt.Errorf("invalid session name %q: %w", name, ErrInvalidSessionName)
	}
	return name, nil
}

// Start ensures the user's named session for the repository is running:
// it creates the sandbox (from the session's profile) or resumes it, clones
// the repository on first use, renews the sandbox's git credentials, sets
// the session's git identity in the checkout (opts.GitIdentity), makes
// sure osewa is installed, serving and enrolled, and starts the session's
// program in an osewa session unless it is running already. The returned
// Session is ready to Attach to.
//
// It publishes created or resumed once the sandbox is running, ready when
// the session can be attached to, and failed when it returns an error.
func (m *Manager) Start(ctx context.Context, opts StartOptions) (*Session, error) {
	return m.prepare(ctx, opts, true)
}

// Resume is Start for a session that must already exist: it never creates a
// sandbox, and returns an error wrapping ErrNoSession when there is none.
// Profile and Image are ignored.
func (m *Manager) Resume(ctx context.Context, opts StartOptions) (*Session, error) {
	opts.Profile, opts.Image = "", ""
	return m.prepare(ctx, opts, false)
}

func (m *Manager) prepare(ctx context.Context, opts StartOptions, create bool) (_ *Session, err error) {
	if opts.User == nil {
		return nil, proto.ErrUnauthorized
	}
	progress := opts.Progress
	if progress == nil {
		progress = io.Discard
	}
	name, err := sessionName(opts.Name)
	if err != nil {
		return nil, err
	}

	// Input errors are returned before anything is published.
	var (
		profile string
		p       config.SessionProfile
	)
	if create {
		profile, p, err = m.resolveProfile(opts.Repo, opts.Profile, opts.RepoProfile, progress)
		if err != nil {
			return nil, err
		}
	}

	sandbox := SessionSandboxName(opts.User.Username(), opts.Repo, name)
	op := "start"
	if !create {
		op = "attach"
	}
	defer func() {
		// Attaching to a session that does not exist is not a failure of
		// the session, nor is a restore that needs the user's answer.
		if err != nil && !errors.Is(err, ErrNoSession) && !errors.Is(err, ErrRestoreConflict) {
			m.lifecycle(opts.User, opts.Repo, name, sandbox, events.LifecycleFailed, op, err)
		}
	}()

	if m.snaps != nil {
		// A snapshot park or end of the session waits for this start,
		// and the other way round.
		unlock, err := sandboxLocks.Lock(ctx, sandbox)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}

	// A parked session is resumed by EnsureSandbox and ResumeSandbox; look
	// first, to say so. A session parked to its snapshot has no sandbox,
	// only its marker.
	existing, getErr := m.client.GetSandbox(ctx, sandbox)
	unparked := getErr == nil && existing != nil && isParkedPhase(existing.Status.Phase)
	var parked *ParkedSnapshot
	if m.snaps != nil && IsNotFound(getErr) {
		parked, err = m.snaps.readMarker(ctx, opts.User.ID(), sandbox)
		if err != nil {
			return nil, fmt.Errorf("openshell: look up whether session %s is parked to a snapshot: %w", name, err)
		}
	}
	switch {
	case unparked:
		fmt.Fprintf(progress, "Session %s was parked; resuming it...\n", name) //nolint:errcheck
	case parked != nil:
		fmt.Fprintf(progress, "Session %s was parked to a snapshot of /sandbox at %s, which stopped its program; recreating its sandbox...\n", name, parked.Parked.UTC().Format("2006-01-02 15:04 MST")) //nolint:errcheck
		if opts.Image != "" {
			fmt.Fprintf(progress, "Warning: --image ignored, session %s already exists\n", name) //nolint:errcheck
		}
		if opts.Profile != "" && opts.Profile != parked.Profile {
			fmt.Fprintf(progress, "Warning: --profile ignored, session %s already exists\n", name) //nolint:errcheck
		}
		// The session is created again as it was.
		create, unparked = true, true
		profile = parked.Profile
		p = m.sessions.Profiles[profile]
	}

	var (
		sb      *Sandbox
		created bool
	)
	if create {
		image := opts.Image
		if parked != nil {
			image = parked.Image
		}
		if image == "" {
			image = p.Image
		}
		if image == "" {
			image = m.image
		}

		fmt.Fprintf(progress, "Starting sandbox %s...\n", sandbox) //nolint:errcheck
		sb, created, err = m.client.EnsureSandbox(ctx, sandbox, SandboxRequest{
			Image:       image,
			Labels:      ownerLabels(opts.User),
			Annotations: sessionAnnotations(opts.User.Username(), opts.Repo, name, profile),
		})
		if err != nil {
			return nil, err
		}
	} else {
		fmt.Fprintf(progress, "Resuming sandbox %s...\n", sandbox) //nolint:errcheck
		sb, err = m.client.ResumeSandbox(ctx, sandbox, ownerLabels(opts.User))
		if IsNotFound(err) {
			return nil, fmt.Errorf("%w: %q for %s; start it with \"session start %s%s\"", ErrNoSession, name, opts.Repo, opts.Repo, nameFlag(name))
		}
		if err != nil {
			return nil, err
		}
	}
	m.monitor.noteReady(sandbox)
	if created {
		m.lifecycle(opts.User, opts.Repo, name, sandbox, events.LifecycleCreated, "", nil)
	} else {
		m.lifecycle(opts.User, opts.Repo, name, sandbox, events.LifecycleResumed, "", nil)
		// The sandbox records the profile it was created from.
		profile = sb.Annotations[AnnotationProfile]
		if opts.Image != "" {
			fmt.Fprintf(progress, "Warning: --image ignored, sandbox %s already exists\n", sandbox) //nolint:errcheck
		}
		if opts.Profile != "" && opts.Profile != profile {
			fmt.Fprintf(progress, "Warning: --profile ignored, session %s already exists\n", name) //nolint:errcheck
		}
	}

	command, err := m.profileCommand(profile, progress)
	if err != nil {
		return nil, err
	}

	s := &Session{
		Name:     name,
		Repo:     opts.Repo,
		Sandbox:  sandbox,
		Profile:  profile,
		Created:  created,
		Unparked: unparked && (!created || parked != nil),
		WorkDir:  path.Join(SandboxRoot, opts.Repo),
		Command:  command,
	}

	// Restore the session's snapshot into a sandbox this call created,
	// before the clone check, so a restored checkout is resumed rather
	// than cloned again. A failed restore deletes the sandbox again: the
	// next start then restores into a new one.
	if created && m.snaps != nil {
		s.Restored, err = m.restoreSnapshot(ctx, opts, op, s, parked, progress)
		if err != nil {
			if !errors.Is(err, ErrRestoreConflict) {
				op = "restore"
			}
			m.discardSandbox(sandbox)
			return nil, err
		}
	}

	// Renew the git credentials first: the clone uses them too.
	token, err := m.issueToken(ctx, opts.User, sandbox)
	if err != nil {
		return nil, err
	}
	if err := m.writeGitCredentials(ctx, sandbox, opts.User.Username(), token); err != nil {
		return nil, err
	}

	exists, err := m.dirExists(ctx, sandbox, s.WorkDir)
	if err != nil {
		return nil, err
	}
	if !exists {
		fmt.Fprintf(progress, "Cloning %s into %s...\n", opts.Repo, s.WorkDir) //nolint:errcheck
		if err := m.clone(ctx, sandbox, opts.VCS, opts.Repo, s.WorkDir, opts.GitIdentity); err != nil {
			return nil, err
		}
		s.Cloned = true
	}

	// Set the session's git identity in the checkout on every start and
	// attach, so a changed identity reaches existing checkouts. A failure
	// leaves the session usable: it is reported, not fatal.
	if opts.GitIdentity.IsSet() {
		if err := m.writeGitIdentity(ctx, sandbox, s.WorkDir, opts.GitIdentity); err != nil {
			m.logger.Warn("cannot set the git identity", "sandbox", sandbox, "err", err)
			fmt.Fprintf(progress, "Warning: could not set the git identity %s in %s; commits may use another identity: %v\n", opts.GitIdentity, s.WorkDir, err) //nolint:errcheck
		}
	}

	bin, enrolled, err := m.osewaBinary(ctx, sandbox, progress)
	if err != nil {
		return nil, err
	}
	s.Osewa = bin
	if err := m.startOsewaServer(ctx, sandbox, bin); err != nil {
		return nil, err
	}
	if !enrolled {
		if err := m.enrollBridge(ctx, sandbox, bin); err != nil {
			return nil, err
		}
	}
	s.OsewaID, s.Started, err = m.ensureOsewaSession(ctx, sandbox, bin, s.WorkDir, command)
	if err != nil {
		return nil, err
	}
	switch {
	case s.Started && s.Unparked && parked != nil:
		// A snapshot keeps /sandbox, not running programs.
		fmt.Fprintf(progress, "Started %s again: it was not running when the session resumed (the session was parked to a snapshot of /sandbox, which keeps files but not running programs)\n", strings.Join(command, " ")) //nolint:errcheck
	case s.Started && s.Unparked:
		// The sandbox's memory was not restored (it restarted cleanly,
		// see resumeRestartIfRestoreFails), or the program had exited
		// before the session was parked.
		fmt.Fprintf(progress, "Started %s again: it was not running when the session resumed (it had exited, or the sandbox could not restore its memory and restarted cleanly)\n", strings.Join(command, " ")) //nolint:errcheck
	case s.Started && !created:
		fmt.Fprintf(progress, "Started %s again: it was not running\n", strings.Join(command, " ")) //nolint:errcheck
	}

	s.Env = m.osewaClientEnv()
	if opts.Term != "" {
		s.Env["TERM"] = opts.Term
	}

	m.lifecycle(opts.User, opts.Repo, name, sandbox, events.LifecycleReady, "", nil)
	return s, nil
}

// restoreSnapshot restores the session's snapshot, if it has one, into the
// sandbox this start created, as opts.Restore says, and returns it; nil
// when it restored none. A session parked to its snapshot is no longer
// parked once restored (or started afresh).
func (m *Manager) restoreSnapshot(ctx context.Context, opts StartOptions, op string, s *Session, parked *ParkedSnapshot, progress io.Writer) (*SnapshotInfo, error) {
	uid := opts.User.ID()
	unpark := func() {
		if parked == nil {
			return
		}
		if err := m.snaps.deleteMarker(ctx, uid, s.Sandbox); err != nil {
			// Harmless: a marker is only shown while the sandbox is
			// absent, and the next park or end replaces it.
			m.logger.Warn("cannot clear a parked marker", "sandbox", s.Sandbox, "err", err)
		}
	}
	if opts.Restore == RestoreSkip {
		if parked != nil {
			fmt.Fprintln(progress, "Not restoring the session's snapshot (--fresh): it starts afresh. The snapshot stays in S3 until the next one replaces it.") //nolint:errcheck
		}
		unpark()
		return nil, nil
	}

	info, err := m.snaps.stat(ctx, uid, s.Sandbox)
	if err != nil {
		return nil, fmt.Errorf("openshell: look up the snapshot of session %s: %w", s.Name, err)
	}
	if info == nil {
		if parked != nil {
			return nil, fmt.Errorf("openshell: session %s was parked to a snapshot, but %s is missing; start it with --fresh to start afresh", s.Name, m.snaps.objectURL(m.snaps.key(uid, s.Sandbox)))
		}
		return nil, nil
	}

	if opts.Restore == RestoreAuto {
		// A new sandbox's /sandbox holds what the image put there (the
		// shell start-up files): the snapshot's versions win over those
		// without asking. Sundae's own state or the checkout can only
		// be there from earlier work, e.g. a volume that outlived its
		// sandbox; replacing that is the user's call.
		found, err := probePaths(ctx, m.client, s.Sandbox, s.WorkDir, sundaeStateDir)
		if err != nil {
			return nil, err
		}
		if len(found) > 0 {
			return nil, &RestoreConflictError{Repo: s.Repo, Name: s.Name, Paths: found, Snapshot: *info}
		}
	}

	fmt.Fprintf(progress, "Restoring /sandbox from the snapshot of %s...\n", info.Describe()) //nolint:errcheck
	if err := m.snaps.restore(ctx, m.client, uid, s.Sandbox); err != nil {
		return nil, fmt.Errorf("openshell: restore the snapshot of session %s (%s): %w", s.Name, info.URL, err)
	}
	m.publish(opts.User, events.SessionLifecycle{
		Session: events.Session{User: opts.User.Username(), Repo: s.Repo, Name: s.Name, Sandbox: s.Sandbox},
		Step:    events.LifecycleRestored,
		Op:      op,
		Bytes:   info.Size,
	}, nil)
	unpark()
	return info, nil
}

// discardSandbox deletes a sandbox this call created and could not
// prepare. It uses a fresh context so it still runs when ctx was
// cancelled.
func (m *Manager) discardSandbox(sandbox string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := m.client.DeleteSandbox(ctx, sandbox); err != nil {
		m.logger.Warn("cannot delete a sandbox that failed to start", "sandbox", sandbox, "err", err)
	}
}

// Park suspends the sandbox of the user's named session for the repository
// (`session park`). It reports whether it parked it: a session that is
// parked already is a successful no-op. It refuses a session with an open
// bridge, one that is not running, and sandboxes Sundae did not create,
// and publishes parked (Op ParkManual) on success. start and attach resume
// the session.
func (m *Manager) Park(ctx context.Context, user proto.User, repo, name string) (bool, error) {
	parked, _, err := m.ParkSession(ctx, user, repo, name)
	return parked, err
}

// ParkSession is Park, also reporting whether the session was parked to
// its S3 snapshot (sessions.suspend.fallback: snapshot) rather than
// suspended: its sandbox deleted and its program stopped.
func (m *Manager) ParkSession(ctx context.Context, user proto.User, repo, name string) (parked, snapshot bool, err error) {
	if user == nil {
		return false, false, proto.ErrUnauthorized
	}
	name, err = sessionName(name)
	if err != nil {
		return false, false, err
	}
	sandbox := SessionSandboxName(user.Username(), repo, name)
	sb, err := m.client.GetSandbox(ctx, sandbox)
	switch {
	case IsNotFound(err) && m.snaps != nil:
		// Parked to its snapshot already?
		if p, merr := m.snaps.readMarker(ctx, user.ID(), sandbox); merr == nil && p != nil {
			return false, true, nil
		}
		return false, false, fmt.Errorf("%w: %q for %s", ErrNoSession, name, repo)
	case IsNotFound(err):
		return false, false, fmt.Errorf("%w: %q for %s", ErrNoSession, name, repo)
	case err != nil:
		return false, false, fmt.Errorf("openshell: get sandbox %q: %w", sandbox, err)
	case !owns(sb, ownerLabels(user)):
		return false, false, ErrNotOwned
	}
	if _, ok := managedSandbox(sb); !ok {
		return false, false, fmt.Errorf("openshell: sandbox %q was not created by Sundae; not parking it", sandbox)
	}
	switch sb.Status.Phase {
	case v1.SandboxReady:
	case v1.SandboxSuspended, v1.SandboxSuspending:
		return false, false, nil
	default:
		return false, false, fmt.Errorf("session %s of %s is %s; only a running session can be parked", name, repo, sb.Status.Phase)
	}

	snapshot, err = m.monitor.ParkSession(ctx, m.client, sandbox)
	if err != nil {
		if IsSuspendUnsupported(err) {
			return false, false, fmt.Errorf("the OpenShell gateway cannot suspend this sandbox (set sessions.suspend.fallback: snapshot to park it to an S3 snapshot instead): %w", err)
		}
		return false, false, err
	}
	m.publish(user, events.SessionLifecycle{
		Session:  events.Session{User: user.Username(), Repo: repo, Name: name, Sandbox: sandbox},
		Step:     events.LifecycleParked,
		Op:       ParkManual,
		Snapshot: snapshot,
	}, nil)
	return true, snapshot, nil
}

// nameFlag returns the --name flag selecting session name, or "" for the
// default session.
func nameFlag(name string) string {
	if name == DefaultSessionName {
		return ""
	}
	return " --name " + name
}

// resolveProfile returns the profile a new session for repo is created from:
// explicit when set, else the repository's own profile (repoSetting), else
// the sessions.repos match, else none (""). An explicit profile that is not
// configured is an error; a repository profile that no longer is falls
// through to the next, with a warning on progress.
func (m *Manager) resolveProfile(repo, explicit, repoSetting string, progress io.Writer) (string, config.SessionProfile, error) {
	name := explicit
	if name == "" && repoSetting != "" {
		if _, ok := m.sessions.Profiles[repoSetting]; ok {
			name = repoSetting
		} else {
			fmt.Fprintf(progress, "Warning: the repository's profile %q is no longer configured; ignoring it\n", repoSetting) //nolint:errcheck
		}
	}
	if name == "" {
		name = m.sessions.RepoProfile(repo)
	}
	if name == "" {
		return "", config.SessionProfile{}, nil
	}
	if err := m.sessions.CheckProfile(name); err != nil {
		return "", config.SessionProfile{}, err //nolint:wrapcheck
	}
	return name, m.sessions.Profiles[name], nil
}

// profileCommand returns the program of a session created from profile: the
// profile's command, else openshell.command. A profile that is no longer
// configured falls back to openshell.command with a warning.
func (m *Manager) profileCommand(profile string, progress io.Writer) ([]string, error) {
	line := m.command
	if profile != "" {
		p, ok := m.sessions.Profiles[profile]
		if !ok {
			fmt.Fprintf(progress, "Warning: profile %q is no longer configured; running %q\n", profile, m.command) //nolint:errcheck
		} else if p.Command != "" {
			line = p.Command
		}
	}
	command, err := shlex.Split(line, true)
	if err != nil || len(command) == 0 {
		return nil, fmt.Errorf("openshell: invalid command %q", line)
	}
	return command, nil
}

// Attach runs the osewa attach client for s with a cols x rows PTY. Ending
// it, by detaching (~.), closing the SSH connection or cancelling ctx, ends
// only the client: the session's program keeps running.
func (m *Manager) Attach(ctx context.Context, s *Session, cols, rows uint32) (InteractiveSession, error) {
	is, err := m.client.Interactive(ctx, s.Sandbox, s.AttachCommand(), cols, rows, ExecOptions{
		Env:          maps.Clone(s.Env),
		WorkDir:      s.WorkDir,
		NoLoginShell: true,
	})
	if err != nil {
		return nil, fmt.Errorf("openshell: attach to sandbox %q: %w", s.Sandbox, err)
	}
	return is, nil
}

// EndOptions configures EndSession.
type EndOptions struct {
	// Force ends the session even when its snapshot fails.
	Force bool
	// Progress receives human-readable progress messages. May be nil.
	Progress io.Writer
}

// EndResult is what EndSession did.
type EndResult struct {
	// Existed reports whether there was a session to end: a sandbox, or a
	// session parked to its snapshot.
	Existed bool
	// Snapshot is the snapshot taken before the sandbox was deleted; nil
	// when none was.
	Snapshot *SnapshotInfo
	// SnapshotErr is why the snapshot failed when Force ended the session
	// anyway.
	SnapshotErr error
	// Parked reports that the session was parked to its snapshot, which is
	// kept.
	Parked bool
}

// End is EndSession without options: a failed snapshot stops it.
func (m *Manager) End(ctx context.Context, user proto.User, repo, name string) (bool, error) {
	r, err := m.EndSession(ctx, user, repo, name, EndOptions{})
	return r.Existed, err
}

// EndSession deletes the sandbox of the user's named session for the
// repository and revokes its access tokens. Other sessions of the
// repository are left alone. Ending an absent session is a successful
// no-op.
//
// With snapshots.s3.on including end, /sandbox is snapshotted first; when
// that fails the session is not ended and a *SnapshotError is returned,
// unless Force is set. A session parked to its snapshot is ended by
// forgetting that it is parked; its snapshot is kept.
//
// It publishes snapshotted when the snapshot is stored, ended once the
// sandbox is deleted, and failed when the sandbox could not be looked up,
// snapshotted or deleted. Ending an absent session publishes nothing.
func (m *Manager) EndSession(ctx context.Context, user proto.User, repo, name string, o EndOptions) (EndResult, error) {
	var res EndResult
	if user == nil {
		return res, proto.ErrUnauthorized
	}
	name, err := sessionName(name)
	if err != nil {
		return res, err
	}
	progress := o.Progress
	if progress == nil {
		progress = io.Discard
	}

	sandbox := SessionSandboxName(user.Username(), repo, name)
	sess := events.Session{User: user.Username(), Repo: repo, Name: name, Sandbox: sandbox}
	failed := func(op string, err error) (EndResult, error) {
		m.publish(user, events.SessionLifecycle{Session: sess, Step: events.LifecycleFailed, Op: op}, err)
		return res, err
	}

	if m.snaps != nil {
		unlock, err := sandboxLocks.Lock(ctx, sandbox)
		if err != nil {
			return res, err
		}
		defer unlock()
	}

	existed := true
	sb, err := m.client.GetSandbox(ctx, sandbox)
	switch {
	case IsNotFound(err):
		existed = false
	case err != nil:
		return failed("end", fmt.Errorf("openshell: get sandbox %q: %w", sandbox, err))
	case !owns(sb, ownerLabels(user)):
		return failed("end", ErrNotOwned)
	}

	if !existed && m.snaps != nil {
		parked, err := m.snaps.readMarker(ctx, user.ID(), sandbox)
		switch {
		case err != nil && !o.Force:
			return failed("end", fmt.Errorf("openshell: look up whether session %s is parked to a snapshot: %w (session end --force ends it anyway)", name, err))
		case parked != nil:
			if err := m.snaps.deleteMarker(ctx, user.ID(), sandbox); err != nil {
				return failed("end", err)
			}
			res.Existed, res.Parked = true, true
			m.publish(user, events.SessionLifecycle{Session: sess, Step: events.LifecycleEnded}, nil)
		}
	}

	if existed {
		if m.snaps.onEnd() {
			info, serr := m.snapshotForEnd(ctx, user, sess, sb, progress)
			switch {
			case serr == nil:
				res.Snapshot = info
				m.publish(user, events.SessionLifecycle{Session: sess, Step: events.LifecycleSnapshotted, Op: "end", Bytes: info.Size}, nil)
			case !o.Force:
				return failed("snapshot", &SnapshotError{Repo: repo, Name: name, Err: serr})
			default:
				res.SnapshotErr = serr
				m.publish(user, events.SessionLifecycle{Session: sess, Step: events.LifecycleFailed, Op: "snapshot"}, serr)
				fmt.Fprintf(progress, "Warning: the snapshot of /sandbox failed: %v; ending the session without it (--force)\n", serr) //nolint:errcheck
			}
		}
		if err := m.client.DeleteSandbox(ctx, sandbox); err != nil {
			return failed("end", err)
		}
		res.Existed = true
		if m.snaps != nil {
			// A marker left by a park whose delete failed would show the
			// ended session as parked.
			if err := m.snaps.deleteMarker(ctx, user.ID(), sandbox); err != nil {
				m.logger.Warn("cannot clear a parked marker", "sandbox", sandbox, "err", err)
			}
		}
		// The session is gone even if revoking its tokens below fails.
		m.publish(user, events.SessionLifecycle{Session: sess, Step: events.LifecycleEnded}, nil)
	}

	if err := m.revokeTokens(ctx, user, TokenName(sandbox), true); err != nil {
		return res, err
	}

	return res, nil
}

// snapshotForEnd snapshots the sandbox of a session being ended, resuming
// it first when it is parked or stopped: only a running sandbox can be
// archived.
func (m *Manager) snapshotForEnd(ctx context.Context, user proto.User, sess events.Session, sb *Sandbox, progress io.Writer) (*SnapshotInfo, error) {
	switch sb.Status.Phase {
	case v1.SandboxReady:
	case v1.SandboxSuspended, v1.SandboxSuspending, v1.SandboxStopped, v1.SandboxProvisioning, v1.SandboxStarting:
		fmt.Fprintf(progress, "Resuming sandbox %s to snapshot it...\n", sess.Sandbox) //nolint:errcheck
		if _, err := m.client.ResumeSandbox(ctx, sess.Sandbox, ownerLabels(user)); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("the sandbox is %s", sb.Status.Phase)
	}
	return m.snaps.take(ctx, m.client, user.ID(), sess, progress)
}

// List returns the user's sessions sorted by repository and session name.
func (m *Manager) List(ctx context.Context, user proto.User) ([]Info, error) {
	if user == nil {
		return nil, proto.ErrUnauthorized
	}

	sbs, err := m.client.ListSandboxes(ctx, ownerLabels(user))
	if err != nil {
		return nil, err
	}

	infos := make([]Info, 0, len(sbs))
	for _, sb := range sbs {
		name := sb.Annotations[AnnotationSession]
		if name == "" {
			// Created before named sessions.
			name = DefaultSessionName
		}
		infos = append(infos, Info{
			Repo:    sb.Annotations[AnnotationRepo],
			Name:    name,
			Profile: sb.Annotations[AnnotationProfile],
			Sandbox: sb.Name,
			Phase:   string(sb.Status.Phase),
		})
	}
	if m.snaps != nil {
		infos = m.withParkedSnapshots(ctx, user, sbs, infos)
	}
	slices.SortFunc(infos, func(a, b Info) int {
		if c := strings.Compare(a.Repo, b.Repo); c != 0 {
			return c
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Sandbox, b.Sandbox)
	})
	return infos, nil
}

// withParkedSnapshots adds the user's sessions parked to their snapshots,
// which have no sandbox, to infos. When the store cannot be read, they are
// left out and that is logged: the running sessions are still listed.
func (m *Manager) withParkedSnapshots(ctx context.Context, user proto.User, sbs []*Sandbox, infos []Info) []Info {
	markers, err := m.snaps.listMarkers(ctx, user.ID())
	if err != nil {
		m.logger.Warn("cannot list sessions parked to snapshots", "user", user.Username(), "err", err)
		return infos
	}
	for _, p := range markers {
		if slices.ContainsFunc(sbs, func(sb *Sandbox) bool { return sb.Name == p.Sandbox }) {
			// Recreated, and the marker not cleared yet.
			continue
		}
		infos = append(infos, Info{
			Repo:     p.Repo,
			Name:     p.Session,
			Profile:  p.Profile,
			Sandbox:  p.Sandbox,
			Phase:    PhaseSnapshot,
			Snapshot: &p,
		})
	}
	return infos
}

// issueToken creates a fresh short-lived access token for the sandbox after
// pruning its expired predecessors. Unexpired tokens are kept until they
// expire or the session ends: a git command that read the credential file
// just before it was replaced may still be using one.
func (m *Manager) issueToken(ctx context.Context, user proto.User, sandbox string) (string, error) {
	name := TokenName(sandbox)
	if err := m.revokeTokens(ctx, user, name, false); err != nil {
		return "", err
	}

	token, err := m.tokens.CreateAccessToken(ctx, user, name, m.now().Add(m.tokenTTL))
	if err != nil {
		return "", fmt.Errorf("openshell: issue access token: %w", err)
	}
	return token, nil
}

// revokeTokens deletes the user's tokens with the given name: all of them,
// or only the expired ones.
func (m *Manager) revokeTokens(ctx context.Context, user proto.User, name string, all bool) error {
	tokens, err := m.tokens.ListAccessTokens(ctx, user)
	if err != nil {
		return fmt.Errorf("openshell: list access tokens: %w", err)
	}

	now := m.now()
	var errs []error
	for _, t := range tokens {
		if t.Name != name {
			continue
		}
		if !all && (t.ExpiresAt.IsZero() || now.Before(t.ExpiresAt)) {
			continue
		}
		if err := m.tokens.DeleteAccessToken(ctx, user, t.ID); err != nil && !errors.Is(err, proto.ErrTokenNotFound) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("openshell: revoke access tokens: %w", err)
	}
	return nil
}

// dirExists reports whether dir exists in the sandbox.
func (m *Manager) dirExists(ctx context.Context, sandbox, dir string) (bool, error) {
	res, err := m.client.Run(ctx, sandbox, []string{"test", "-d", dir}, ExecOptions{NoLoginShell: true})
	if err != nil {
		return false, fmt.Errorf("openshell: check %s: %w", dir, err)
	}
	switch res.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("openshell: check %s: exit status %d: %s", dir, res.ExitCode, trimOutput(res.Stderr))
	}
}

// clone clones the repository into workDir, with id as the clone's git
// identity when it is set. The credentials come from the credential file
// writeGitCredentials wrote; neither the URL nor the environment carries
// any.
func (m *Manager) clone(ctx context.Context, sandbox string, vcs proto.VCS, repo, workDir string, id config.GitIdentity) error {
	if parent := path.Dir(workDir); parent != SandboxRoot {
		if err := m.run(ctx, sandbox, []string{"mkdir", "-p", parent}); err != nil {
			return err
		}
	}

	if id.IsSet() {
		return m.cloneWithIdentity(ctx, sandbox, vcs, m.repoURL(repo), workDir, id)
	}
	return m.run(ctx, sandbox, CloneCommand(vcs, m.repoURL(repo), workDir))
}

func (m *Manager) run(ctx context.Context, sandbox string, command []string) error {
	res, err := m.client.Run(ctx, sandbox, command, ExecOptions{
		Env:          map[string]string{"HOME": SandboxRoot},
		WorkDir:      SandboxRoot,
		NoLoginShell: true,
	})
	if err != nil {
		return fmt.Errorf("openshell: %s: %w", command[0], err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("openshell: %s: exit status %d: %s", strings.Join(command[:min(len(command), 3)], " "), res.ExitCode, trimOutput(res.Stderr))
	}
	return nil
}

// repoURL is the HTTP clone URL of the repository, without credentials.
func (m *Manager) repoURL(repo string) string {
	return m.cloneURL + "/" + repo + ".git"
}

// CloneCommand returns the command that clones url into dir: a colocated jj
// clone for jj repositories and a plain git clone otherwise.
func CloneCommand(vcs proto.VCS, url, dir string) []string {
	if vcs == proto.VCSJJ {
		return []string{"jj", "git", "clone", "--colocate", url, dir}
	}
	return []string{"git", "clone", url, dir}
}

func ownerLabels(user proto.User) map[string]string {
	return map[string]string{
		LabelManaged: "true",
		LabelUserID:  strconv.FormatInt(user.ID(), 10),
	}
}

func sessionAnnotations(user, repo, name, profile string) map[string]string {
	a := map[string]string{
		AnnotationUser:    user,
		AnnotationRepo:    repo,
		AnnotationSession: name,
	}
	if profile != "" {
		a[AnnotationProfile] = profile
	}
	return a
}

func trimOutput(b []byte) string {
	const max = 512
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}

// Terminal is the client side of a bridged session. *ssh.Session satisfies
// it.
type Terminal interface {
	io.ReadWriter
	Pty() (ssh.Pty, <-chan ssh.Window, bool)
}

// drainTimeout bounds how long Bridge waits for trailing output after the
// remote command has exited.
const drainTimeout = 2 * time.Second

// Bridge connects term to the interactive session: it copies input and
// output in both directions, resizes the remote PTY when the terminal's
// window changes, and returns the remote command's exit code.
//
// Cancelling ctx (e.g. the SSH client disconnecting) aborts the stream. The
// sandbox itself keeps running.
func Bridge(ctx context.Context, term Terminal, is InteractiveSession) (int, error) {
	defer is.Close() //nolint:errcheck

	done := make(chan struct{})
	defer close(done)

	_, winch, _ := term.Pty()
	go func() {
		for {
			select {
			case <-done:
				return
			case w, ok := <-winch:
				if !ok {
					return
				}
				if w.Width > 0 && w.Height > 0 {
					_ = is.Resize(uint32(w.Width), uint32(w.Height)) //nolint:gosec
				}
			}
		}
	}()

	go func() {
		select {
		case <-ctx.Done():
			_ = v1.CancelInteractive(is)
		case <-done:
		}
	}()

	go func() {
		_, _ = io.Copy(is, term)
		// The client closed its input; let the remote side see EOF.
		_ = v1.CloseInteractiveInput(is)
	}()

	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		_, _ = io.Copy(term, is)
	}()

	code, err := is.ExitCode()

	select {
	case <-outDone:
	case <-time.After(drainTimeout):
	}

	return code, err //nolint:wrapcheck
}
