package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/anmitsu/go-shlex"
	"github.com/gobwas/glob"
)

// Session defaults.
const (
	// DefaultOsewaBinaryDir is where the Soft Serve image keeps the pinned
	// osewa release builds it uploads to sandboxes (see the root
	// Containerfile).
	DefaultOsewaBinaryDir = "/usr/local/share/soft-serve/osewa"
	// DefaultOsewaPort is the loopback port of the osewa server in each
	// sandbox. It is osewa's own default.
	DefaultOsewaPort = 7322
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

	// Profiles are named presets a session starts from (session start
	// --profile NAME). A profile's empty fields fall back to openshell.image
	// and openshell.command.
	Profiles map[string]SessionProfile `yaml:"profiles"`

	// Repos maps repository name globs to defaults for sessions started
	// without --profile. "*" does not cross "/", "**" does; matching is
	// case-insensitive. When several patterns match, the most specific one
	// wins: the one with the most characters that are not wildcards.
	Repos map[string]SessionRepoDefaults `yaml:"repos"`
}

// OsewaConfig configures osewa in the sandboxes.
type OsewaConfig struct {
	// BinaryDir holds the pinned osewa Linux release builds as published,
	// osewa-linux-amd64.gz and osewa-linux-arm64.gz. Soft Serve verifies one
	// against its pinned checksum and uploads it to a sandbox whose image
	// does not provide the pinned osewa. Relative paths are resolved
	// against the data path.
	BinaryDir string `env:"BINARY_DIR" yaml:"binary_dir"`

	// Port is the loopback TCP port the osewa server in each sandbox listens
	// on. Nothing outside the sandbox connects to it.
	Port int `env:"PORT" yaml:"port"`
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

// sessionsEnviron returns the sessions section as SOFT_SERVE_SESSIONS_*
// environment variables. Profiles and repos have no environment form; hook
// subprocesses read them from the config file.
func (c *Config) sessionsEnviron() []string {
	return []string{
		"SOFT_SERVE_SESSIONS_OSEWA_BINARY_DIR=" + c.Sessions.Osewa.BinaryDir,
		fmt.Sprintf("SOFT_SERVE_SESSIONS_OSEWA_PORT=%d", c.Sessions.Osewa.Port),
	}
}
