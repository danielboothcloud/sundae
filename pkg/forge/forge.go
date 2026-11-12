// Package forge discovers repositories on GitHub and GitLab, imports them
// into Soft Serve and keeps them in sync without ever rewriting local work.
//
// # Naming
//
// A forge repository "owner/repo" is imported as "<prefix>/owner/repo", where
// prefix defaults to the forge name. Prefixes are unique and never nest (see
// config validation), so two forges can never produce the same name. A name
// already taken by a repository that is not linked to the same forge path is
// a collision: the import of that repository is refused and nothing is
// adopted or overwritten.
//
// # Refs
//
// Upstream branches and tags are fetched into the forge's own namespace,
// refs/remotes/<forge>/heads/* and refs/remotes/<forge>/tags/*, which is the
// only place sync prunes. Soft Serve's own refs/heads and refs/tags, which the
// TUI and web views list, are only ever created or fast-forwarded (see
// SyncResult).
//
// # Link
//
// The link between a Soft Serve repository and its forge repository lives in
// the repository's git config, the way mirrors keep their remote: a remote
// named after the forge (url and fetch refspecs) and a [forge] section with
// name, path, defaultbranch, lastsync and lasterror. There is no database
// table.
package forge

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/gobwas/glob"
)

var (
	// ErrForgeNotFound is returned for a forge name that is not configured.
	ErrForgeNotFound = errors.New("forge not found")
	// ErrForeignCloneURL is returned when a clone URL is not on the forge's
	// configured host.
	ErrForeignCloneURL = errors.New("clone URL is not on the forge host")
	// ErrInvalidPath is returned for a forge path that cannot be a Soft Serve
	// repository name.
	ErrInvalidPath = errors.New("invalid forge repository path")
)

// Forge is a configured forge.
type Forge struct {
	cfg     config.ForgeConfig
	base    *url.URL
	include []glob.Glob
	exclude []glob.Glob
}

// New returns the forge for a validated forge configuration.
func New(fc config.ForgeConfig) (*Forge, error) {
	base, err := url.Parse(strings.TrimSuffix(fc.URL, "/"))
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("forge %s: invalid url %q", fc.Name, fc.URL)
	}
	f := &Forge{cfg: fc, base: base}
	for _, p := range fc.Include {
		g, err := glob.Compile(strings.ToLower(p), '/')
		if err != nil {
			return nil, fmt.Errorf("forge %s: include %q: %w", fc.Name, p, err)
		}
		f.include = append(f.include, g)
	}
	for _, p := range fc.Exclude {
		g, err := glob.Compile(strings.ToLower(p), '/')
		if err != nil {
			return nil, fmt.Errorf("forge %s: exclude %q: %w", fc.Name, p, err)
		}
		f.exclude = append(f.exclude, g)
	}
	return f, nil
}

// Name returns the forge name.
func (f *Forge) Name() string { return f.cfg.Name }

// Kind returns the forge kind, "github" or "gitlab".
func (f *Forge) Kind() string { return f.cfg.Kind }

// URL returns the forge's web base URL.
func (f *Forge) URL() string { return f.base.String() }

// Prefix returns the repository name prefix.
func (f *Forge) Prefix() string { return f.cfg.Prefix }

// Interval returns the scheduled sync interval.
func (f *Forge) Interval() time.Duration { return f.cfg.Sync }

// Config returns the forge configuration.
func (f *Forge) Config() config.ForgeConfig { return f.cfg }

// Matches reports whether the forge path passes the include and exclude
// patterns. Matching is case-insensitive, as forge paths are.
func (f *Forge) Matches(path string) bool {
	path = strings.ToLower(path)
	if len(f.include) > 0 {
		included := false
		for _, g := range f.include {
			if g.Match(path) {
				included = true
				break
			}
		}
		if !included {
			return false
		}
	}
	for _, g := range f.exclude {
		if g.Match(path) {
			return false
		}
	}
	return true
}

// pathSegmentRe matches one segment of a forge path.
var pathSegmentRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// RepoName returns the Soft Serve repository name for a forge path:
// "<prefix>/<path>". The path must be at least "owner/repo", made of plain
// segments; anything that would be rewritten by repository name
// sanitisation (".." segments, a trailing ".git") is refused, so a forge
// can never place a repository outside its prefix.
func (f *Forge) RepoName(path string) (string, error) {
	segs := strings.Split(path, "/")
	if len(segs) < 2 {
		return "", fmt.Errorf("%w %q: want owner/repo", ErrInvalidPath, path)
	}
	for _, seg := range segs {
		if seg == "." || seg == ".." || !pathSegmentRe.MatchString(seg) {
			return "", fmt.Errorf("%w %q", ErrInvalidPath, path)
		}
	}
	name := f.cfg.Prefix + "/" + path
	if utils.SanitizeRepo(name) != name || utils.ValidateRepo(name) != nil {
		return "", fmt.Errorf("%w %q", ErrInvalidPath, path)
	}
	return name, nil
}

// CheckCloneURL checks that a clone URL is an http(s) URL on the forge's
// configured host, under its base path. It is the guard for every URL that
// git is pointed at: clone URLs come from API responses, and a forge host is
// trusted because the admin declared it, not because of what the API says.
func (f *Forge) CheckCloneURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %q: %v", ErrForeignCloneURL, raw, err)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("%w: %q", ErrForeignCloneURL, raw)
	}
	if !strings.EqualFold(u.Scheme, f.base.Scheme) || hostPort(u) != hostPort(f.base) {
		return fmt.Errorf("%w: %q is not on %s", ErrForeignCloneURL, raw, f.URL())
	}
	basePath := strings.TrimSuffix(f.base.Path, "/")
	if !strings.HasPrefix(u.Path, basePath+"/") {
		return fmt.Errorf("%w: %q is not under %s", ErrForeignCloneURL, raw, f.URL())
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == ".." || seg == "." {
			return fmt.Errorf("%w: %q", ErrForeignCloneURL, raw)
		}
	}
	return nil
}

// hostPort returns the lowercased host and port of u, with the scheme's
// default port filled in.
func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// apiBase returns the REST API base URL.
//
// GitHub.com's API lives on api.github.com; GitHub Enterprise Server's under
// /api/v3 of the web URL. GitLab's is /api/v4 of the web URL.
func (f *Forge) apiBase() string {
	base := strings.TrimSuffix(f.base.String(), "/")
	switch f.cfg.Kind {
	case config.ForgeKindGitHub:
		if strings.EqualFold(f.base.Hostname(), "github.com") {
			return "https://api.github.com"
		}
		return base + "/api/v3"
	default:
		return base + "/api/v4"
	}
}

// gitUser is the HTTP basic auth user name sent with the token over git
// HTTPS. GitHub takes the token as the password with any user name and
// documents x-access-token (which also works for GitHub App installation
// tokens); GitLab takes any non-blank user name with a personal, project or
// group access token, and oauth2 also covers OAuth tokens.
func (f *Forge) gitUser() string {
	if f.cfg.Kind == config.ForgeKindGitHub {
		return "x-access-token"
	}
	return "oauth2"
}

// gitEnv returns the environment for a git command that talks to the forge.
//
// The token travels as an Authorization header scoped to the forge's base
// URL through GIT_CONFIG_* variables, like the OpenShell session clone: it is
// never written into a URL or the repository's config. Redirects are off, as
// for every other remote Soft Serve fetches (ssrf.GitEnv), credential
// helpers are reset so nothing stores or substitutes a credential, and git
// never prompts.
func (f *Forge) gitEnv() ([]string, error) {
	token, err := f.cfg.Token()
	if err != nil {
		return nil, err
	}
	entries := []ssrf.GitConfigEntry{{Key: "credential.helper", Value: ""}}
	if token != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(f.gitUser() + ":" + token))
		entries = append(entries, ssrf.GitConfigEntry{
			Key:   "http." + strings.TrimSuffix(f.base.String(), "/") + "/.extraHeader",
			Value: "Authorization: Basic " + cred,
		})
	}
	env := ssrf.GitEnv(ssrf.ValidatedGitRemote{Config: entries})
	return append(env, "GIT_TERMINAL_PROMPT=0"), nil
}
