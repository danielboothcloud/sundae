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
	"github.com/anmitsu/go-shlex"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/proto"
	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
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
	client Client
	tokens TokenStore
	// cfg is the configuration: the runtime of each profile, and each
	// runtime's defaults.
	cfg     *config.Config
	image   string
	command string
	// runtimeClass is openshell.runtime_class: the RuntimeClass of new
	// OpenShell sandboxes whose profile sets none.
	runtimeClass string
	sessions     config.SessionsConfig
	cloneURL     string
	tokenTTL     time.Duration
	now          func() time.Time
	// osewaBinaryDir holds the osewa release builds to upload.
	osewaBinaryDir string
	// osewaPort is the loopback port of the sandbox's osewa server.
	osewaPort int
	// osewaSHA256 maps an architecture to the checksum of its osewa build.
	osewaSHA256 map[string]string
	// bus receives lifecycle events. nil drops them.
	bus *events.Bus
	// monitor tracks bridges and suspends sessions; nil when there is none.
	monitor *Monitor
	// providerSync syncs the providers declared in openshell.providers;
	// nil when there is none.
	providerSync *ProviderReconciler
	// logger reports what does not fail a command.
	logger *log.Logger
}

// ManagerOption configures a Manager.
type ManagerOption func(*Manager)

// WithEvents publishes the session lifecycle (created, resumed, ready,
// suspended, deleted, failed) on bus. Without it, or with a nil bus, nothing is
// published.
func WithEvents(bus *events.Bus) ManagerOption {
	return func(m *Manager) { m.bus = bus }
}

// WithMonitor makes the Manager coordinate with the server's Monitor:
// suspending goes through it (so a session with an open bridge is not
// suspended),
// and starting a session tells it. nil is allowed.
func WithMonitor(mon *Monitor) ManagerOption {
	return func(m *Manager) { m.monitor = mon }
}

// WithProviderReconciler makes a start fail when the session's profile
// lists a declared provider that failed to sync. nil is allowed.
func WithProviderReconciler(r *ProviderReconciler) ManagerOption {
	return func(m *Manager) { m.providerSync = r }
}

// NewManager returns a Manager using client for sandboxes and tokens for git
// credentials, configured from cfg.
func NewManager(client Client, tokens TokenStore, cfg *config.Config, opts ...ManagerOption) *Manager {
	m := &Manager{
		client:         client,
		tokens:         tokens,
		cfg:            cfg,
		image:          cfg.OpenShell.Image,
		runtimeClass:   cfg.OpenShell.RuntimeClass,
		command:        cfg.OpenShell.Command,
		sessions:       cfg.Sessions,
		cloneURL:       cfg.OpenShellCloneURL(),
		tokenTTL:       cfg.OpenShell.TokenTTL,
		now:            time.Now,
		osewaBinaryDir: cfg.Sessions.Osewa.BinaryDir,
		osewaPort:      cfg.Sessions.Osewa.Port,
		osewaSHA256:    osewaSHA256,
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
func (m *Manager) lifecycle(user proto.User, sess events.Session, step events.Lifecycle, op string, err error) {
	m.publish(user, events.SessionLifecycle{Session: sess, Step: step, Op: op}, err)
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
	// Repos are the repositories the session must hold: a new session is
	// created with them, in order; an existing one gets those it lacks
	// added. Resume may leave it empty.
	Repos []RepoSpec
	// Repo, VCS and GitIdentity are a single repository, used when Repos
	// is empty.
	Repo string
	VCS  proto.VCS
	// RemoveRepos are taken out of an existing session's list. Their
	// checkouts stay in /sandbox.
	RemoveRepos []string
	// ResolveRepo returns the spec of a repository the session holds but
	// Repos does not name (to clone it, or set its git identity). nil, or
	// an error, leaves that repository as it is.
	ResolveRepo func(name string) (RepoSpec, error)
	// Name is the session name. Empty is DefaultSessionName.
	Name string
	// Profile names the sessions.profiles entry to create the session from.
	// Empty uses RepoProfile, else the first sessions.repos match among the
	// repositories, in order, if any. Start only; it is ignored, with a
	// warning, for a session that already exists.
	Profile string
	// RepoProfile is the default profile of the first repository, in
	// order, that has one (`repo profile`), used when Profile is empty.
	// One that is no longer in sessions.profiles is skipped with a
	// warning.
	RepoProfile string
	// Image overrides the image when the sandbox is created. Start only;
	// ignored, with a warning, for a session that already exists.
	Image string
	// Term is the client's TERM, given to the attach client.
	Term string
	// GitIdentity is Repo's; see RepoSpec.
	GitIdentity config.GitIdentity
	// Progress receives human-readable progress messages. May be nil.
	Progress io.Writer
}

// repoSpecs returns the repositories opts names.
func (opts StartOptions) repoSpecs() []RepoSpec {
	if len(opts.Repos) > 0 {
		return opts.Repos
	}
	if opts.Repo != "" {
		return []RepoSpec{{Name: opts.Repo, VCS: opts.VCS, GitIdentity: opts.GitIdentity}}
	}
	return nil
}

// Session is a started session, ready to attach to.
type Session struct {
	// Name is the session name.
	Name string
	// Repo is the session's first repository; Repos are all of them.
	Repo  string
	Repos []string
	// Sandbox is the sandbox name.
	Sandbox string
	// Profile is the profile the session was created from; empty for none.
	Profile string
	// Runtime is the runtime the session's sandbox runs in.
	Runtime string
	// Providers are the OpenShell providers the session has: those its
	// profile lists, or, for a profile no longer configured, those
	// attached to its sandbox. A program this call starts gets their
	// credentials' placeholders (see osewaNewScript).
	Providers []string
	// ProviderChanges is what this call attached and detached to make an
	// existing sandbox's providers match its profile.
	ProviderChanges ProviderChanges
	// Created reports whether the sandbox was created by this call.
	Created bool
	// Cloned are the repositories this call cloned.
	Cloned []string
	// WorkDir is where the program runs: the checkout of the session's
	// only repository, else /sandbox.
	WorkDir string
	// Command is the session's program, running in an osewa session.
	Command []string
	// Started reports whether this call started the program, rather than
	// finding it running.
	Started bool
	// WasSuspended reports whether this call resumed a suspended session.
	WasSuspended bool
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
	// Repo is the session's first repository; Repos are all of them, in
	// order (see SessionRepos).
	Repo  string
	Repos []string
	// Name is the session name.
	Name string
	// Profile is the profile the session was created from; empty for none.
	Profile string
	// Runtime is the runtime the session's sandbox runs in.
	Runtime string
	// Providers are the OpenShell providers attached to the sandbox, as
	// the gateway lists them.
	Providers []string
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

// Start ensures the user's named session is running: it creates the
// sandbox (from the session's profile) or resumes it, updates its
// repository list (opts.Repos, opts.RemoveRepos), clones each repository
// on first use, renews the sandbox's git credentials, sets each
// repository's git identity in its checkout, makes
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
	specs := opts.repoSpecs()
	wanted := make([]string, 0, len(specs))
	specOf := map[string]RepoSpec{}
	for _, sp := range specs {
		if sp.Name == "" {
			return nil, errors.New("openshell: a repository with no name")
		}
		if _, ok := specOf[sp.Name]; !ok {
			wanted = append(wanted, sp.Name)
		}
		specOf[sp.Name] = sp
	}

	// Input errors are returned before anything is published.
	var (
		profile string
		p       config.SessionProfile
	)
	if create {
		profile, p, err = m.resolveProfile(wanted, opts.Profile, opts.RepoProfile, progress)
		if err != nil {
			return nil, err
		}
	}

	sandbox := SessionSandboxName(opts.User.Username(), name)
	// The session's repositories, as far as known before the sandbox is
	// read: what the events say until then.
	repos := wanted
	op := "start"
	if !create {
		op = "attach"
	}
	defer func() {
		// Attaching to a session that does not exist is not a failure of
		// the session.
		if err != nil && !errors.Is(err, ErrNoSession) {
			m.lifecycle(opts.User, events.NewSession(opts.User.Username(), name, sandbox, repos), events.LifecycleFailed, op, err)
		}
	}()

	// A suspended session is resumed by EnsureSandbox and ResumeSandbox;
	// look first, to say so.
	existing, getErr := m.client.GetSandbox(ctx, sandbox)
	if getErr != nil {
		existing = nil
	}
	if existing != nil {
		repos = mergeRepos(m.monitor.SessionRepos(existing), wanted, opts.RemoveRepos)
	}
	wasSuspended := existing != nil && isSuspendedPhase(existing.Phase)
	if wasSuspended {
		fmt.Fprintf(progress, "Session %s was suspended; resuming it...\n", name) //nolint:errcheck
	}
	if len(repos) == 0 {
		if create {
			return nil, errors.New("openshell: a session needs at least one repository")
		}
	}

	// A start fails when the session's profile lists a provider declared
	// in openshell.providers whose last sync failed: its sandbox would get
	// the provider's credentials as they were, or none. An attach only
	// warns: the session keeps what the gateway has.
	checkProfile, checkProviders := profile, p.OpenShellProviders()
	if existing != nil {
		checkProfile = existing.Annotations[AnnotationProfile]
		checkProviders, _ = m.profileProviders(checkProfile)
	}
	if serr := m.providerSync.Check(checkProviders); serr != nil {
		if op == "start" {
			return nil, fmt.Errorf("session %s: %s lists a provider Sundae cannot sync: %w; an admin can see why with \"openshell providers\"", name, describeProfile(checkProfile), serr)
		}
		fmt.Fprintf(progress, "Warning: %v; session %s keeps the credentials the gateway already has for it\n", serr, name) //nolint:errcheck
	}

	// An existing sandbox's providers are made to match its profile
	// before it is resumed, so a program that starts with the sandbox
	// (after a suspend whose memory could not be restored) gets them. A
	// sandbox of someone else's is left to EnsureSandbox and
	// ResumeSandbox to refuse.
	var (
		changes ProviderChanges
		synced  bool
	)
	if existing != nil && owns(existing.Labels, ownerLabels(opts.User)) {
		if _, ok := managedSandbox(existing); ok {
			if changes, err = m.syncProviders(ctx, existing, name, progress); err != nil {
				return nil, err
			}
			synced = true
		}
	}

	var (
		sb      *Sandbox
		created bool
	)
	if create {
		runtime := m.cfg.ProfileRuntime(profile)
		image := opts.Image
		var (
			runtimeClass string
			providers    []string
		)
		switch runtime {
		case config.RuntimeOpenShell:
			if image == "" {
				image = p.Image
			}
			if image == "" {
				image = m.image
			}
			runtimeClass = config.ResolveRuntimeClass(p.OpenShellRuntimeClass(), m.runtimeClass)
			providers = p.OpenShellProviders()
		default:
			if image == "" {
				image = m.cfg.ProfileImage(profile)
			}
			runtimeClass = config.ResolveRuntimeClass(m.cfg.ProfileRuntimeClass(profile))
		}
		annotations := sessionAnnotations(opts.User.Username(), repos, name, profile, providers)
		annotations[AnnotationRuntime] = runtime
		if runtimeClass != "" {
			annotations[AnnotationRuntimeClass] = runtimeClass
		}

		fmt.Fprintf(progress, "Starting sandbox %s...\n", sandbox) //nolint:errcheck
		sb, created, err = m.client.EnsureSandbox(ctx, sandbox, SandboxRequest{
			Runtime:      runtime,
			Profile:      profile,
			Image:        image,
			RuntimeClass: runtimeClass,
			Labels:       ownerLabels(opts.User),
			Annotations:  annotations,
			Providers:    providers,
		})
		var missing *MissingProviderError
		if errors.As(err, &missing) {
			return nil, m.missingProviderError(profile, missing)
		}
		if err != nil {
			m.logger.Warn("cannot start sandbox", "sandbox", sandbox, "user", opts.User.Username(), "runtime", runtime, "image", image, "runtime_class", runtimeClass, "err", err)
			return nil, err
		}
	} else {
		fmt.Fprintf(progress, "Resuming sandbox %s...\n", sandbox) //nolint:errcheck
		sb, err = m.client.ResumeSandbox(ctx, sandbox, ownerLabels(opts.User))
		if IsNotFound(err) {
			return nil, fmt.Errorf("%w: %q; create it with \"session new %s --repo <repository>\"", ErrNoSession, name, name)
		}
		if err != nil {
			m.logger.Warn("cannot resume sandbox", "sandbox", sandbox, "user", opts.User.Username(), "err", err)
			return nil, err
		}
	}
	m.monitor.noteReady(sandbox)
	if !created && existing == nil {
		// Created by a concurrent start after the look above.
		repos = mergeRepos(m.monitor.SessionRepos(sb), wanted, opts.RemoveRepos)
	}
	if created {
		m.lifecycle(opts.User, events.NewSession(opts.User.Username(), name, sandbox, repos), events.LifecycleCreated, "", nil)
	} else {
		m.lifecycle(opts.User, events.NewSession(opts.User.Username(), name, sandbox, repos), events.LifecycleResumed, "", nil)
		// The sandbox records the profile it was created from.
		profile = sb.Annotations[AnnotationProfile]
		if opts.Image != "" {
			fmt.Fprintf(progress, "Warning: --image ignored, sandbox %s already exists\n", sandbox) //nolint:errcheck
		}
		if opts.Profile != "" && opts.Profile != profile {
			fmt.Fprintf(progress, "Warning: --profile ignored, session %s already exists\n", name) //nolint:errcheck
		}
		if !synced {
			// Created by a concurrent start after the look above.
			if changes, err = m.syncProviders(ctx, sb, name, progress); err != nil {
				return nil, err
			}
		}
	}

	command, err := m.profileCommand(profile, progress)
	if err != nil {
		return nil, err
	}

	s := &Session{
		Name:            name,
		Sandbox:         sandbox,
		Profile:         profile,
		Runtime:         SandboxRuntime(sb),
		Providers:       m.sessionProviders(profile, sb),
		ProviderChanges: changes,
		Created:         created,
		WasSuspended:    wasSuspended && !created,
		Command:         command,
	}
	s.setRepos(repos)

	// Renew the git credentials first: the clone uses them too.
	token, err := m.issueToken(ctx, opts.User, sandbox)
	if err != nil {
		return nil, err
	}
	if err := m.writeGitCredentials(ctx, sandbox, opts.User.Username(), token); err != nil {
		return nil, err
	}

	// The session's current list: what the Monitor knows, else the
	// sandbox's list file (`session repos` may have changed it since the
	// sandbox was created), else the list the sandbox was created with. The file is only
	// written when the list differs from that.
	base, known := m.monitor.knownRepos(sandbox)
	if !known && !created {
		onDisk, err := m.readRepos(ctx, sandbox)
		if err != nil {
			return nil, err
		}
		base, known = onDisk, onDisk != nil
	}
	if !known {
		base = annotatedRepos(sb.Annotations)
		if len(base) == 0 {
			base = repos
		}
	}
	s.setRepos(mergeRepos(base, wanted, opts.RemoveRepos))
	if len(s.Repos) == 0 {
		return nil, fmt.Errorf("session %s has no repositories left; add one with \"session repos %s add <repository>\"", name, name)
	}
	if !slices.Equal(s.Repos, base) {
		if err := m.writeRepos(ctx, sandbox, s.Repos); err != nil {
			return nil, err
		}
	}
	m.monitor.rememberRepos(sandbox, s.Repos)
	repos = s.Repos

	for _, repo := range s.Repos {
		sp, ok := specOf[repo]
		if !ok {
			if opts.ResolveRepo == nil {
				sp = RepoSpec{Name: repo}
			} else if sp, err = opts.ResolveRepo(repo); err != nil {
				// Deleted, renamed, or no longer readable: its checkout,
				// if any, stays as it is.
				fmt.Fprintf(progress, "Warning: skipping %s: %v\n", repo, err) //nolint:errcheck
				continue
			}
		}
		dir := repoDir(repo)
		exists, err := m.dirExists(ctx, sandbox, dir)
		if err != nil {
			return nil, err
		}
		if !exists {
			fmt.Fprintf(progress, "Cloning %s into %s...\n", repo, dir) //nolint:errcheck
			if err := m.clone(ctx, sandbox, sp.VCS, repo, dir, sp.GitIdentity); err != nil {
				return nil, err
			}
			s.Cloned = append(s.Cloned, repo)
		}

		// Set the repository's git identity in its checkout on every
		// start and attach, so a changed identity reaches existing
		// checkouts. A failure leaves the session usable: it is
		// reported, not fatal.
		if sp.GitIdentity.IsSet() {
			if err := m.writeGitIdentity(ctx, sandbox, dir, sp.GitIdentity); err != nil {
				m.logger.Warn("cannot set the git identity", "sandbox", sandbox, "repo", repo, "err", err)
				fmt.Fprintf(progress, "Warning: could not set the git identity %s in %s; commits may use another identity: %v\n", sp.GitIdentity, dir, err) //nolint:errcheck
			}
		}
	}
	for _, r := range opts.RemoveRepos {
		if !slices.Contains(s.Repos, r) {
			fmt.Fprintf(progress, "Removed %s from session %s; its checkout %s stays in the sandbox\n", r, name, repoDir(r)) //nolint:errcheck
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
	// Every session's program gets the OpenShell exec environment: the
	// supervisor terminates TLS for every sandbox, so the program needs its
	// CA variables (SSL_CERT_FILE, NODE_EXTRA_CA_CERTS, ...) whether or not
	// the profile attaches providers.
	s.OsewaID, s.Started, err = m.ensureOsewaSession(ctx, sandbox, bin, s.WorkDir, command, true)
	if err != nil {
		return nil, err
	}
	switch {
	case !changes.Changed():
	case !s.Started:
		// OpenShell gives a process the credential placeholders of the
		// providers attached when it starts; it never changes a running
		// process's environment. A detached provider's placeholders stop
		// resolving at once.
		fmt.Fprintf(progress, "The running %s keeps the provider credentials it started with; exit it and attach again to start it with the current ones\n", strings.Join(command, " ")) //nolint:errcheck
	case len(changes.Attached) > 0:
		// The sandbox picks a provider change up within its poll interval
		// (10s by default), so a program started at once may miss it.
		fmt.Fprintf(progress, "If %s lacks the credentials of a provider attached just now, exit it and attach again: the sandbox may not have applied the change yet\n", strings.Join(command, " ")) //nolint:errcheck
	}
	switch {
	case s.Started && s.WasSuspended && s.Runtime != config.RuntimeOpenShell:
		// Suspending a pod session deletes its pod; a docker session's
		// container is stopped.
		fmt.Fprintf(progress, "Started %s again: suspending stopped it (a suspended %s session keeps /sandbox, not running programs)\n", strings.Join(command, " "), s.Runtime) //nolint:errcheck
	case s.Started && s.WasSuspended:
		// The sandbox's memory was not restored (it restarted cleanly,
		// see resumeRestartIfRestoreFails), or the program had exited
		// before the session was suspended.
		fmt.Fprintf(progress, "Started %s again: it was not running when the session resumed (it had exited, or the sandbox could not restore its memory and restarted cleanly)\n", strings.Join(command, " ")) //nolint:errcheck
	case s.Started && !created:
		fmt.Fprintf(progress, "Started %s again: it was not running\n", strings.Join(command, " ")) //nolint:errcheck
	}

	s.Env = m.osewaClientEnv()
	if opts.Term != "" {
		s.Env["TERM"] = opts.Term
	}

	m.lifecycle(opts.User, events.NewSession(opts.User.Username(), name, sandbox, s.Repos), events.LifecycleReady, "", nil)
	return s, nil
}

// setRepos sets the session's repositories and the directory its program
// runs in.
func (s *Session) setRepos(repos []string) {
	s.Repos = slices.Clone(repos)
	s.Repo = ""
	if len(repos) > 0 {
		s.Repo = repos[0]
	}
	s.WorkDir = sessionWorkDir(repos)
}

// event returns the session's events.Session.
func (s *Session) event(user string) events.Session {
	return events.NewSession(user, s.Name, s.Sandbox, s.Repos)
}

// profileProviders returns the OpenShell providers the sessions of profile
// have attached, and whether that is known: none for no profile, and not
// known for a profile that is no longer configured.
func (m *Manager) profileProviders(profile string) ([]string, bool) {
	if profile == "" {
		return nil, true
	}
	p, ok := m.sessions.Profiles[profile]
	return p.OpenShellProviders(), ok
}

// sessionProviders returns the providers of a session of profile in sb:
// the profile's, or, for a profile no longer configured, those attached.
func (m *Manager) sessionProviders(profile string, sb *Sandbox) []string {
	if want, ok := m.profileProviders(profile); ok {
		return want
	}
	return sb.Providers
}

// syncProviders makes the OpenShell providers attached to an existing
// sandbox those its profile lists now, so a provider added to or removed
// from a profile reaches its existing sessions on their next start or
// attach. Providers attached by other means are detached: the profile
// decides which credentials its sessions get. A sandbox whose profile is
// no longer configured keeps its providers.
//
// A provider that does not exist on the gateway is a configuration error
// and fails the start. Any other failure is a warning on progress: the
// session keeps the providers it has, and attaching to it is not blocked.
func (m *Manager) syncProviders(ctx context.Context, sb *Sandbox, name string, progress io.Writer) (ProviderChanges, error) {
	profile := sb.Annotations[AnnotationProfile]
	want, ok := m.profileProviders(profile)
	if !ok {
		return ProviderChanges{}, nil
	}
	changes, err := m.client.SyncProviders(ctx, sb, want)
	var missing *MissingProviderError
	switch {
	case errors.As(err, &missing):
		return changes, m.missingProviderError(profile, missing)
	case err != nil:
		m.logger.Warn("cannot update the providers of a sandbox", "sandbox", sb.Name, "profile", profile, "want", want, "err", err)
		fmt.Fprintf(progress, "Warning: could not make the OpenShell providers of session %s match %s: %v; it keeps the providers it has\n", name, describeProfile(profile), err) //nolint:errcheck
	}
	if len(changes.Attached) > 0 {
		fmt.Fprintf(progress, "Attached OpenShell provider(s) %s to session %s (%s)\n", strings.Join(changes.Attached, ", "), name, describeProfile(profile)) //nolint:errcheck
	}
	if len(changes.Detached) > 0 {
		fmt.Fprintf(progress, "Detached OpenShell provider(s) %s from session %s: %s does not list them\n", strings.Join(changes.Detached, ", "), name, describeProfile(profile)) //nolint:errcheck
	}
	return changes, nil
}

// describeProfile names a profile for messages.
func describeProfile(profile string) string {
	if profile == "" {
		return "no profile"
	}
	return "profile " + strconv.Quote(profile)
}

// missingProviderError explains a provider a profile lists that does not
// exist on the gateway.
func (m *Manager) missingProviderError(profile string, err *MissingProviderError) error {
	if m.providerSync.Declared(err.Provider) {
		return fmt.Errorf("sessions.profiles.%s.providers lists OpenShell provider %q, which openshell.providers declares but Sundae has not created on the gateway yet; it is created on the next sync (an admin can see its state with \"openshell providers\"): %w",
			profile, err.Provider, err)
	}
	return fmt.Errorf("sessions.profiles.%s.providers lists OpenShell provider %q, which does not exist on the gateway: declare it in openshell.providers, create it (openshell provider create --name %s ...) or remove it from the profile: %w",
		profile, err.Provider, err.Provider, err)
}

// Suspend suspends the sandbox of the user's named session (`session
// suspend`): it stops the sandbox and keeps its storage. It reports whether
// it suspended it: a session that is suspended already is a successful
// no-op. It refuses a session with an open bridge, one that is not running,
// sandboxes Sundae did not create, and a runtime that cannot suspend, and
// publishes suspended (Op SuspendManual) on success. start, attach and
// resume start the session again.
func (m *Manager) Suspend(ctx context.Context, user proto.User, name string) (bool, error) {
	if user == nil {
		return false, proto.ErrUnauthorized
	}
	name, err := sessionName(name)
	if err != nil {
		return false, err
	}
	sandbox := SessionSandboxName(user.Username(), name)
	sb, err := m.client.GetSandbox(ctx, sandbox)
	switch {
	case IsNotFound(err):
		return false, fmt.Errorf("%w: %q", ErrNoSession, name)
	case err != nil:
		return false, fmt.Errorf("openshell: get sandbox %q: %w", sandbox, err)
	case !owns(sb.Labels, ownerLabels(user)):
		return false, ErrNotOwned
	}
	if _, ok := managedSandbox(sb); !ok {
		return false, fmt.Errorf("openshell: sandbox %q was not created by Sundae; not suspending it", sandbox)
	}
	switch sb.Phase {
	case sbx.Ready:
	case sbx.Suspended, sbx.Suspending:
		return false, nil
	default:
		return false, fmt.Errorf("session %s is %s; only a running session can be suspended", name, sb.Phase)
	}

	if err := m.monitor.Suspend(ctx, m.client, sandbox); err != nil {
		if IsSuspendUnsupported(err) {
			return false, fmt.Errorf("session %s cannot be suspended: its %s runtime cannot suspend this sandbox, so it keeps running (\"session delete %s\" deletes it and its storage): %w", name, SandboxRuntime(sb), name, err)
		}
		return false, err
	}
	m.publish(user, events.SessionLifecycle{
		Session: events.NewSession(user.Username(), name, sandbox, m.monitor.SessionRepos(sb)),
		Step:    events.LifecycleSuspended,
		Op:      SuspendManual,
	}, nil)
	return true, nil
}

// resolveProfile returns the profile a new session of repos is created
// from: explicit when set, else the first repository's own profile that
// has one (repoSetting), else the first sessions.repos match among the
// repositories, in order, else none (""). An explicit profile that is not
// configured is an error; a repository profile that no longer is falls
// through to the next, with a warning on progress.
func (m *Manager) resolveProfile(repos []string, explicit, repoSetting string, progress io.Writer) (string, config.SessionProfile, error) {
	name := explicit
	if name == "" && repoSetting != "" {
		if _, ok := m.sessions.Profiles[repoSetting]; ok {
			name = repoSetting
		} else {
			fmt.Fprintf(progress, "Warning: the repository's profile %q is no longer configured; ignoring it\n", repoSetting) //nolint:errcheck
		}
	}
	for _, repo := range repos {
		if name != "" {
			break
		}
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

// Delete deletes the sandbox of the user's named session and its storage,
// and revokes its git credentials. Deleting an absent session is a
// successful no-op; it reports whether there was one.
//
// It publishes deleted once the sandbox is deleted, and failed when the
// sandbox could not be looked up or deleted.
func (m *Manager) Delete(ctx context.Context, user proto.User, name string) (bool, error) {
	if user == nil {
		return false, proto.ErrUnauthorized
	}
	name, err := sessionName(name)
	if err != nil {
		return false, err
	}
	sandbox := SessionSandboxName(user.Username(), name)
	sess := events.NewSession(user.Username(), name, sandbox, nil)
	failed := func(err error) (bool, error) {
		m.publish(user, events.SessionLifecycle{Session: sess, Step: events.LifecycleFailed, Op: "delete"}, err)
		return false, err
	}

	existed := true
	sb, err := m.client.GetSandbox(ctx, sandbox)
	switch {
	case IsNotFound(err):
		existed = false
	case err != nil:
		return failed(fmt.Errorf("openshell: get sandbox %q: %w", sandbox, err))
	case !owns(sb.Labels, ownerLabels(user)):
		return failed(ErrNotOwned)
	default:
		sess = events.NewSession(user.Username(), name, sandbox, m.monitor.SessionRepos(sb))
	}

	if existed {
		if err := m.client.DeleteSandbox(ctx, sandbox); err != nil {
			return failed(err)
		}
		m.monitor.forgetRepos(sandbox)
		// The session is gone even if revoking its tokens below fails.
		m.publish(user, events.SessionLifecycle{Session: sess, Step: events.LifecycleDeleted}, nil)
	}

	if err := m.revokeTokens(ctx, user, TokenName(sandbox), true); err != nil {
		return existed, err
	}
	return existed, nil
}

// List returns the user's sessions sorted by session name.
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
		repos := m.monitor.SessionRepos(sb)
		infos = append(infos, Info{
			Repo:      first(repos),
			Repos:     repos,
			Name:      name,
			Profile:   sb.Annotations[AnnotationProfile],
			Runtime:   SandboxRuntime(sb),
			Providers: slices.Clone(sb.Providers),
			Sandbox:   sb.Name,
			Phase:     string(sb.Phase),
		})
	}
	slices.SortFunc(infos, func(a, b Info) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Sandbox, b.Sandbox)
	})
	return infos, nil
}

// issueToken creates a fresh short-lived access token for the sandbox after
// pruning its expired predecessors. Unexpired tokens are kept until they
// expire or the session is deleted: a git command that read the credential file
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

func sessionAnnotations(user string, repos []string, name, profile string, providers []string) map[string]string {
	a := map[string]string{
		AnnotationUser:    user,
		AnnotationRepos:   strings.Join(repos, ","),
		AnnotationSession: name,
	}
	if len(repos) > 0 {
		a[AnnotationRepo] = repos[0]
	}
	if profile != "" {
		a[AnnotationProfile] = profile
	}
	if len(providers) > 0 {
		a[AnnotationProviders] = strings.Join(providers, ",")
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
			_ = sbx.Cancel(is)
		case <-done:
		}
	}()

	go func() {
		_, _ = io.Copy(is, term)
		// The client closed its input; let the remote side see EOF.
		_ = sbx.CloseInput(is)
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

// first returns the first of ss; "" for none.
func first(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

// HasRepo reports whether repo is one of the session's repositories.
func (i Info) HasRepo(repo string) bool { return slices.Contains(i.Repos, repo) }
