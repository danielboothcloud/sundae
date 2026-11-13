package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anmitsu/go-shlex"
	"github.com/gobwas/glob"
)

// Session defaults.
const (
	// DefaultOsewaBinaryDir is where the Sundae image keeps the pinned
	// osewa release builds it uploads to sandboxes (see the root
	// Containerfile).
	DefaultOsewaBinaryDir = "/usr/local/share/sundae/osewa"
	// DefaultOsewaPort is the loopback port of the osewa server in each
	// sandbox. It is osewa's own default.
	DefaultOsewaPort = 7322
	// DefaultStatusPollInterval is how often Sundae reads each running
	// session's status by default.
	DefaultStatusPollInterval = 10 * time.Second
	// MinStatusPollInterval is the shortest sessions.status.poll_interval:
	// each poll is one exec per running sandbox.
	MinStatusPollInterval = time.Second
	// DefaultSuspendAfter is how long a session must sit unattached and not
	// working before it is parked, by default.
	DefaultSuspendAfter = 30 * time.Minute
	// DefaultSignalsKeep is how many signals (session status reports and
	// lifecycle events) Sundae keeps in memory by default.
	DefaultSignalsKeep = 1000
	// MaxSignalsKeep is the largest sessions.signals.keep. A signal can
	// carry a few KB of program output.
	MaxSignalsKeep = 100000
)

// SessionsConfig configures agent sessions beyond the OpenShell connection:
// the osewa session layer in each sandbox, named profiles and per-repository
// defaults.
//
// Profiles and repos are maps and can only be set in the config file; there
// is no environment form for them.
type SessionsConfig struct {
	// Osewa configures osewa, which keeps each session's program running
	// after the SSH client disconnects.
	Osewa OsewaConfig `envPrefix:"OSEWA_" yaml:"osewa"`

	// Status configures how Sundae collects each session's OSC 7501
	// status from its sandbox.
	Status SessionStatusConfig `envPrefix:"STATUS_" yaml:"status"`

	// Suspend configures parking: suspending the sandbox of a session that
	// nobody is attached to and that is not working.
	Suspend SessionSuspendConfig `envPrefix:"SUSPEND_" yaml:"suspend"`

	// Signals configures the signals feed: the session status reports and
	// lifecycle events Sundae keeps in memory for `session signals`
	// and the TUI.
	Signals SessionSignalsConfig `envPrefix:"SIGNALS_" yaml:"signals"`

	// Profiles are named presets a session starts from (session start
	// --profile NAME). A profile's empty fields fall back to openshell.image
	// and openshell.command.
	Profiles map[string]SessionProfile `yaml:"profiles"`

	// Repos maps repository name globs to defaults for sessions started
	// without --profile. "*" does not cross "/", "**" does; matching is
	// case-insensitive. When several patterns match, the most specific one
	// wins: the one with the most characters that are not wildcards.
	Repos map[string]SessionRepoDefaults `yaml:"repos"`

	// GitIdentity is the name and email commits are made as in agent
	// sessions of repositories not imported from a forge, and of those
	// whose forge has no git_identity. Optional: without one, Sundae
	// configures no identity in the sandbox.
	GitIdentity GitIdentity `envPrefix:"GIT_IDENTITY_" yaml:"git_identity"`
}

// OsewaConfig configures osewa in the sandboxes.
type OsewaConfig struct {
	// BinaryDir holds the pinned osewa Linux release builds as published,
	// osewa-linux-amd64.gz and osewa-linux-arm64.gz. Sundae verifies one
	// against its pinned checksum and uploads it to a sandbox whose image
	// does not provide the pinned osewa. Relative paths are resolved
	// against the data path.
	BinaryDir string `env:"BINARY_DIR" yaml:"binary_dir"`

	// Port is the loopback TCP port the osewa server in each sandbox listens
	// on. Nothing outside the sandbox connects to it.
	Port int `env:"PORT" yaml:"port"`
}

// SessionStatusConfig configures status collection.
type SessionStatusConfig struct {
	// PollInterval is how often Sundae runs `osewa local ls` in every
	// running session's sandbox to read its status. 0 means
	// DefaultStatusPollInterval; at least MinStatusPollInterval otherwise.
	PollInterval time.Duration `env:"POLL_INTERVAL" yaml:"poll_interval"`
}

// SessionSuspendConfig configures parking idle sessions.
type SessionSuspendConfig struct {
	// Enabled turns automatic parking on. Default true. Parking needs an
	// OpenShell compute driver that supports suspend; where the gateway
	// reports it unsupported, the sandbox is left running.
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// After is how long a session must have had nobody attached and no
	// working status before it is parked. 0 means DefaultSuspendAfter.
	After time.Duration `env:"AFTER" yaml:"after"`

	// Fallback is what parking does where the gateway cannot suspend:
	// SuspendFallbackNone leaves the sandbox running;
	// SuspendFallbackSnapshot snapshots /sandbox to S3 (snapshots.s3 must
	// be enabled) and deletes the sandbox, which stops the session's
	// program; start and attach then recreate the sandbox and restore it.
	Fallback string `env:"FALLBACK" yaml:"fallback"`
}

// Values of sessions.suspend.fallback.
const (
	// SuspendFallbackNone leaves a sandbox the gateway cannot suspend
	// running. The default.
	SuspendFallbackNone = "none"
	// SuspendFallbackSnapshot parks a sandbox the gateway cannot suspend
	// by snapshotting /sandbox to S3 and deleting the sandbox.
	SuspendFallbackSnapshot = "snapshot"
)

// SnapshotFallback reports whether parking falls back to an S3 snapshot.
func (c *Config) SnapshotFallback() bool {
	return c.Sessions.Suspend.Fallback == SuspendFallbackSnapshot && c.Snapshots.S3.Enabled
}

// SessionSignalsConfig configures the signals feed.
type SessionSignalsConfig struct {
	// Keep is how many signals are kept, across all users; the oldest go
	// first. They are kept in memory only. 0 means DefaultSignalsKeep; at
	// most MaxSignalsKeep.
	Keep int `env:"KEEP" yaml:"keep"`
}

// SessionProfile is a named session preset.
type SessionProfile struct {
	// Image is the sandbox image. Empty uses openshell.image.
	Image string `yaml:"image"`

	// Command is the program the session runs, split like a shell would.
	// Empty uses openshell.command.
	Command string `yaml:"command"`
}

// SessionRepoDefaults are the session defaults for matching repositories.
type SessionRepoDefaults struct {
	// Profile names an entry of sessions.profiles.
	Profile string `yaml:"profile"`
}

// RepoProfile returns the profile sessions.repos assigns to repo, or "" when
// no pattern matches. When several patterns match, the one with the most
// non-wildcard characters wins, then the longer pattern, then the pattern
// that sorts first.
func (s SessionsConfig) RepoProfile(repo string) string {
	repo = strings.ToLower(repo)
	best, bestScore := "", -1
	for p := range s.Repos {
		g, err := glob.Compile(strings.ToLower(p), '/')
		if err != nil || !g.Match(repo) {
			continue
		}
		score := globSpecificity(p)
		if score > bestScore ||
			(score == bestScore && (len(p) > len(best) || (len(p) == len(best) && p < best))) {
			best, bestScore = p, score
		}
	}
	if best == "" {
		return ""
	}
	return s.Repos[best].Profile
}

// ErrUnknownProfile is returned for a session profile that is not defined in
// sessions.profiles.
var ErrUnknownProfile = errors.New("unknown profile")

// CheckProfile returns an error wrapping ErrUnknownProfile, naming the
// configured profiles, when name is not one of them.
func (s SessionsConfig) CheckProfile(name string) error {
	if _, ok := s.Profiles[name]; ok {
		return nil
	}
	known := sortedKeys(s.Profiles)
	if len(known) == 0 {
		return fmt.Errorf("%w %q: no profiles are configured", ErrUnknownProfile, name)
	}
	return fmt.Errorf("%w %q: use one of %s", ErrUnknownProfile, name, strings.Join(known, ", "))
}

// globSpecificity counts the characters of a glob pattern that are not
// wildcard syntax.
func globSpecificity(p string) int {
	n := 0
	for _, r := range p {
		switch r {
		case '*', '?', '[', ']', '{', '}', ',', '!', '\\':
		default:
			n++
		}
	}
	return n
}

// validateSessions validates the sessions section, fills in defaults and
// resolves a relative binary_dir against the data path. Every problem is
// reported, each naming its key.
func (c *Config) validateSessions() error {
	sc := &c.Sessions
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("sessions."+format, args...))
	}

	sc.Osewa.BinaryDir = strings.TrimSpace(sc.Osewa.BinaryDir)
	if sc.Osewa.BinaryDir == "" {
		sc.Osewa.BinaryDir = DefaultOsewaBinaryDir
	}
	if !filepath.IsAbs(sc.Osewa.BinaryDir) {
		sc.Osewa.BinaryDir = filepath.Join(c.DataPath, sc.Osewa.BinaryDir)
	}
	switch {
	case sc.Osewa.Port == 0:
		sc.Osewa.Port = DefaultOsewaPort
	case sc.Osewa.Port < 1 || sc.Osewa.Port > 65535:
		fail("osewa.port %d must be between 1 and 65535", sc.Osewa.Port)
	}

	switch {
	case sc.Status.PollInterval == 0:
		sc.Status.PollInterval = DefaultStatusPollInterval
	case sc.Status.PollInterval < MinStatusPollInterval:
		fail("status.poll_interval %s must be at least %s", sc.Status.PollInterval, MinStatusPollInterval)
	}
	switch {
	case sc.Suspend.After == 0:
		sc.Suspend.After = DefaultSuspendAfter
	case sc.Suspend.After < 0:
		fail("suspend.after %s must not be negative", sc.Suspend.After)
	case sc.Suspend.After < sc.Status.PollInterval:
		fail("suspend.after %s must not be shorter than status.poll_interval %s", sc.Suspend.After, sc.Status.PollInterval)
	}

	sc.Suspend.Fallback = strings.ToLower(strings.TrimSpace(sc.Suspend.Fallback))
	switch sc.Suspend.Fallback {
	case "":
		sc.Suspend.Fallback = SuspendFallbackNone
	case SuspendFallbackNone:
	case SuspendFallbackSnapshot:
		if !c.Snapshots.S3.Enabled {
			fail("suspend.fallback %q needs snapshots.s3.enabled: true", SuspendFallbackSnapshot)
		}
	default:
		fail("suspend.fallback %q must be %s or %s", sc.Suspend.Fallback, SuspendFallbackNone, SuspendFallbackSnapshot)
	}

	switch {
	case sc.Signals.Keep == 0:
		sc.Signals.Keep = DefaultSignalsKeep
	case sc.Signals.Keep < 0 || sc.Signals.Keep > MaxSignalsKeep:
		fail("signals.keep %d must be between 1 and %d", sc.Signals.Keep, MaxSignalsKeep)
	}

	for _, name := range sortedKeys(sc.Profiles) {
		p := sc.Profiles[name]
		key := fmt.Sprintf("profiles.%s", name)
		if !forgeNameRe.MatchString(name) {
			fail("profiles: name %q must be a DNS label: lowercase letters, digits and hyphens, at most 63 characters", name)
		}
		p.Image = strings.TrimSpace(p.Image)
		p.Command = strings.TrimSpace(p.Command)
		if p.Command != "" {
			if args, err := shlex.Split(p.Command, true); err != nil || len(args) == 0 {
				fail("%s.command %q is not a valid command line", key, p.Command)
			}
		}
		sc.Profiles[name] = p
	}

	for _, pattern := range sortedKeys(sc.Repos) {
		d := sc.Repos[pattern]
		key := fmt.Sprintf("repos[%s]", strconv.Quote(pattern))
		if strings.TrimSpace(pattern) == "" {
			fail("repos: pattern is empty")
		} else if _, err := glob.Compile(strings.ToLower(pattern), '/'); err != nil {
			fail("%s: invalid glob: %v", key, err)
		}
		d.Profile = strings.TrimSpace(d.Profile)
		switch _, ok := sc.Profiles[d.Profile]; {
		case d.Profile == "":
			fail("%s.profile is required", key)
		case !ok:
			fail("%s.profile %q is not defined in sessions.profiles", key, d.Profile)
		}
		sc.Repos[pattern] = d
	}

	return errors.Join(errs...)
}

// sortedKeys returns the keys of m in order, so problems are reported in a
// stable order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// sessionsEnviron returns the sessions section as SUNDAE_SESSIONS_*
// environment variables, git_identity included. Profiles and repos have no
// environment form; hook subprocesses read them from the config file.
func (c *Config) sessionsEnviron() []string {
	return []string{
		"SUNDAE_SESSIONS_OSEWA_BINARY_DIR=" + c.Sessions.Osewa.BinaryDir,
		fmt.Sprintf("SUNDAE_SESSIONS_OSEWA_PORT=%d", c.Sessions.Osewa.Port),
		"SUNDAE_SESSIONS_STATUS_POLL_INTERVAL=" + c.Sessions.Status.PollInterval.String(),
		fmt.Sprintf("SUNDAE_SESSIONS_SUSPEND_ENABLED=%t", c.Sessions.Suspend.Enabled),
		"SUNDAE_SESSIONS_SUSPEND_AFTER=" + c.Sessions.Suspend.After.String(),
		"SUNDAE_SESSIONS_SUSPEND_FALLBACK=" + c.Sessions.Suspend.Fallback,
		fmt.Sprintf("SUNDAE_SESSIONS_SIGNALS_KEEP=%d", c.Sessions.Signals.Keep),
		"SUNDAE_SESSIONS_GIT_IDENTITY_NAME=" + c.Sessions.GitIdentity.Name,
		"SUNDAE_SESSIONS_GIT_IDENTITY_EMAIL=" + c.Sessions.GitIdentity.Email,
	}
}
