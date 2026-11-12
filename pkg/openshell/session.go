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

	"charm.land/ssh"
	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/anmitsu/go-shlex"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/events"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// SandboxRoot is the directory repositories are cloned into. It is also the
// sandbox user's home.
const SandboxRoot = "/sandbox"

// tokenPrefix prefixes the name of access tokens issued to sandboxes.
const tokenPrefix = "openshell:"

// ErrNoSession is returned when attaching to a session that does not exist.
var ErrNoSession = errors.New("no such session")

// TokenStore issues and revokes Soft Serve access tokens. It is satisfied by
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
	if m.bus == nil {
		return
	}
	l := events.SessionLifecycle{
		Session: events.Session{User: user.Username(), Repo: repo, Name: name, Sandbox: sandbox},
		Step:    step,
		Op:      op,
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
	// Empty uses the sessions.repos match for Repo, if any. Start only; it
	// is ignored, with a warning, for a session that already exists.
	Profile string
	// Image overrides the image when the sandbox is created. Start only;
	// ignored, with a warning, for a session that already exists.
	Image string
	// Term is the client's TERM, given to the attach client.
	Term string
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
	// Phase is the sandbox phase.
	Phase string
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
// the repository on first use, renews the sandbox's git credentials, makes
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
		profile, p, err = m.resolveProfile(opts.Repo, opts.Profile)
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
		// the session.
		if err != nil && !errors.Is(err, ErrNoSession) {
			m.lifecycle(opts.User, opts.Repo, name, sandbox, events.LifecycleFailed, op, err)
		}
	}()

	// A parked session is resumed by EnsureSandbox and ResumeSandbox; look
	// first, to say so.
	unparked := m.isParked(ctx, sandbox)
	if unparked {
		fmt.Fprintf(progress, "Session %s was parked; resuming it...\n", name) //nolint:errcheck
	}

	var (
		sb      *Sandbox
		created bool
	)
	if create {
		image := opts.Image
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
		Unparked: unparked && !created,
		WorkDir:  path.Join(SandboxRoot, opts.Repo),
		Command:  command,
	}

	// Seam: restoring a /sandbox snapshot belongs here, before the clone
	// check, so a restored checkout is resumed rather than re-cloned.

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
		if err := m.clone(ctx, sandbox, opts.VCS, opts.Repo, s.WorkDir); err != nil {
			return nil, err
		}
		s.Cloned = true
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

// isParked reports whether the sandbox exists and is suspended. Lookup
// errors count as not parked: the start that follows reports them.
func (m *Manager) isParked(ctx context.Context, sandbox string) bool {
	sb, err := m.client.GetSandbox(ctx, sandbox)
	return err == nil && sb != nil && isParkedPhase(sb.Status.Phase)
}

// Park suspends the sandbox of the user's named session for the repository
// (`session park`). It reports whether it parked it: a session that is
// parked already is a successful no-op. It refuses a session with an open
// bridge, one that is not running, and sandboxes Soft Serve did not create,
// and publishes parked (Op ParkManual) on success. start and attach resume
// the session.
func (m *Manager) Park(ctx context.Context, user proto.User, repo, name string) (bool, error) {
	if user == nil {
		return false, proto.ErrUnauthorized
	}
	name, err := sessionName(name)
	if err != nil {
		return false, err
	}
	sandbox := SessionSandboxName(user.Username(), repo, name)
	sb, err := m.client.GetSandbox(ctx, sandbox)
	switch {
	case IsNotFound(err):
		return false, fmt.Errorf("%w: %q for %s", ErrNoSession, name, repo)
	case err != nil:
		return false, fmt.Errorf("openshell: get sandbox %q: %w", sandbox, err)
	case !owns(sb, ownerLabels(user)):
		return false, ErrNotOwned
	}
	if _, ok := managedSandbox(sb); !ok {
		return false, fmt.Errorf("openshell: sandbox %q was not created by Soft Serve; not parking it", sandbox)
	}
	switch sb.Status.Phase {
	case v1.SandboxReady:
	case v1.SandboxSuspended, v1.SandboxSuspending:
		return false, nil
	default:
		return false, fmt.Errorf("session %s of %s is %s; only a running session can be parked", name, repo, sb.Status.Phase)
	}

	if err := m.monitor.Park(ctx, m.client, sandbox); err != nil {
		if IsSuspendUnsupported(err) {
			return false, fmt.Errorf("the OpenShell gateway cannot suspend this sandbox: %w", err)
		}
		return false, err
	}
	m.lifecycle(user, repo, name, sandbox, events.LifecycleParked, ParkManual, nil)
	return true, nil
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
// explicit when set, else the sessions.repos match, else none ("").
func (m *Manager) resolveProfile(repo, explicit string) (string, config.SessionProfile, error) {
	name := explicit
	if name == "" {
		name = m.sessions.RepoProfile(repo)
	}
	if name == "" {
		return "", config.SessionProfile{}, nil
	}
	p, ok := m.sessions.Profiles[name]
	if !ok {
		known := slices.Sorted(maps.Keys(m.sessions.Profiles))
		if len(known) == 0 {
			return "", config.SessionProfile{}, fmt.Errorf("unknown profile %q: no profiles are configured", name)
		}
		return "", config.SessionProfile{}, fmt.Errorf("unknown profile %q: use one of %s", name, strings.Join(known, ", "))
	}
	return name, p, nil
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

// End deletes the sandbox of the user's named session for the repository
// and revokes its access tokens. Other sessions of the repository are left
// alone. It reports whether a sandbox existed; ending an absent session is a
// successful no-op.
//
// It publishes ended once the sandbox is deleted, and failed when the
// sandbox could not be looked up or deleted. Ending an absent session
// publishes nothing.
func (m *Manager) End(ctx context.Context, user proto.User, repo, name string) (bool, error) {
	if user == nil {
		return false, proto.ErrUnauthorized
	}
	name, err := sessionName(name)
	if err != nil {
		return false, err
	}

	sandbox := SessionSandboxName(user.Username(), repo, name)
	failed := func(err error) (bool, error) {
		m.lifecycle(user, repo, name, sandbox, events.LifecycleFailed, "end", err)
		return false, err
	}
	existed := true
	sb, err := m.client.GetSandbox(ctx, sandbox)
	switch {
	case IsNotFound(err):
		existed = false
	case err != nil:
		return failed(fmt.Errorf("openshell: get sandbox %q: %w", sandbox, err))
	case !owns(sb, ownerLabels(user)):
		return failed(ErrNotOwned)
	}

	if existed {
		// Seam: snapshotting /sandbox belongs here, before the delete.
		if err := m.client.DeleteSandbox(ctx, sandbox); err != nil {
			return failed(err)
		}
		// The session is gone even if revoking its tokens below fails.
		m.lifecycle(user, repo, name, sandbox, events.LifecycleEnded, "", nil)
	}

	if err := m.revokeTokens(ctx, user, TokenName(sandbox), true); err != nil {
		return existed, err
	}

	return existed, nil
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

// clone clones the repository into workDir. The credentials come from the
// credential file writeGitCredentials wrote; neither the URL nor the
// environment carries any.
func (m *Manager) clone(ctx context.Context, sandbox string, vcs proto.VCS, repo, workDir string) error {
	if parent := path.Dir(workDir); parent != SandboxRoot {
		if err := m.run(ctx, sandbox, []string{"mkdir", "-p", parent}); err != nil {
			return err
		}
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
