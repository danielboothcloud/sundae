package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/gobwas/glob"
)

// Forge kinds.
const (
	// ForgeKindGitHub is GitHub.com or GitHub Enterprise Server.
	ForgeKindGitHub = "github"
	// ForgeKindGitLab is GitLab.com or a self-managed GitLab.
	ForgeKindGitLab = "gitlab"
)

// Forge defaults.
const (
	// DefaultGitHubURL is the base URL used for a github forge without url.
	DefaultGitHubURL = "https://github.com"
	// DefaultGitLabURL is the base URL used for a gitlab forge without url.
	DefaultGitLabURL = "https://gitlab.com"
	// DefaultForgeSync is the sync interval used for a forge without sync.
	DefaultForgeSync = 10 * time.Minute
	// MinForgeSync is the shortest sync interval accepted.
	MinForgeSync = time.Minute
)

// ForgeConfig declares a GitHub or GitLab account whose repositories Soft
// Serve discovers, imports and keeps in sync.
//
// Forges are only declared here: there is no runtime "add forge". The token
// is only ever read from TokenFile, so the config file can live in a
// ConfigMap or in git.
type ForgeConfig struct {
	// Name identifies the forge in commands, ref namespaces
	// (refs/remotes/<name>/...) and repository git config. A DNS label:
	// lowercase letters, digits and hyphens, at most 63 characters.
	Name string `env:"NAME" yaml:"name"`

	// Kind is "github" or "gitlab".
	Kind string `env:"KIND" yaml:"kind"`

	// URL is the web base URL of the forge, e.g. https://github.com or
	// https://gitlab.example.com. The API URL is derived from it. Defaults to
	// https://github.com or https://gitlab.com by kind.
	URL string `env:"URL" yaml:"url"`

	// TokenFile is the path of a file holding the access token. It is read
	// again for every API call and git operation, so a rotated token is
	// picked up without a restart.
	TokenFile string `env:"TOKEN_FILE" yaml:"token_file"`

	// Include lists glob patterns matched against the forge path
	// ("owner/repo", or "group/subgroup/project" on GitLab). Empty includes
	// everything. "*" does not cross "/"; "**" does.
	Include []string `env:"INCLUDE" yaml:"include"`

	// Exclude lists glob patterns, like Include, that remove repositories
	// Include let through.
	Exclude []string `env:"EXCLUDE" yaml:"exclude"`

	// Sync is the interval of the scheduled sync of this forge's imported
	// repositories. Defaults to 10m; at least 1m.
	Sync time.Duration `env:"SYNC" yaml:"sync"`

	// Prefix is the Soft Serve repository namespace imported repositories go
	// under: forge path "owner/repo" becomes "<prefix>/owner/repo". Defaults
	// to Name.
	Prefix string `env:"PREFIX" yaml:"prefix"`
}

// Token returns the forge access token: the contents of TokenFile with
// trailing whitespace trimmed, or "" when no token file is configured.
// TokenFile is read on every call.
func (f ForgeConfig) Token() (string, error) {
	if f.TokenFile == "" {
		return "", nil
	}
	token, err := readTokenFile(f.TokenFile)
	if err != nil {
		return "", fmt.Errorf("forge %s: %w", f.Name, err)
	}
	return token, nil
}

// readTokenFile reads a token file, trimming trailing whitespace. An empty
// token is an error.
func readTokenFile(path string) (string, error) {
	bts, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token_file: %w", err)
	}
	token := strings.TrimRightFunc(string(bts), unicode.IsSpace)
	if token == "" {
		return "", fmt.Errorf("token_file %q is empty", path)
	}
	return token, nil
}

// forgeNameRe matches a DNS label.
var forgeNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// repoSegmentRe matches one segment of a repository name prefix.
var repoSegmentRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// validateForges validates the forges section, fills in defaults and resolves
// relative token_file paths against the data path. Every problem is reported,
// each naming the forge and the key.
func (c *Config) validateForges() error {
	var errs []error
	names := map[string]int{}
	prefixes := map[string]string{}
	for i := range c.Forges {
		f := &c.Forges[i]
		key := fmt.Sprintf("forges[%d]", i)
		if f.Name != "" {
			key = fmt.Sprintf("forges[%d] (%s)", i, f.Name)
		}
		fail := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("%s: "+format, append([]any{key}, args...)...))
		}

		f.Name = strings.TrimSpace(f.Name)
		switch {
		case f.Name == "":
			fail("name is required")
		case !forgeNameRe.MatchString(f.Name):
			fail("name %q must be a DNS label: lowercase letters, digits and hyphens, at most 63 characters", f.Name)
		default:
			if j, ok := names[f.Name]; ok {
				fail("name %q is already used by forges[%d]", f.Name, j)
			} else {
				names[f.Name] = i
			}
		}

		f.Kind = strings.ToLower(strings.TrimSpace(f.Kind))
		defaultURL := ""
		switch f.Kind {
		case ForgeKindGitHub:
			defaultURL = DefaultGitHubURL
		case ForgeKindGitLab:
			defaultURL = DefaultGitLabURL
		case "":
			fail("kind is required: github or gitlab")
		default:
			fail("kind %q is not supported: use github or gitlab", f.Kind)
		}

		f.URL = strings.TrimSuffix(strings.TrimSpace(f.URL), "/")
		if f.URL == "" {
			f.URL = defaultURL
		}
		if f.URL != "" {
			if err := validateForgeURL(f.URL); err != nil {
				fail("url %q: %v", f.URL, err)
			}
		}

		if f.TokenFile != "" {
			if !filepath.IsAbs(f.TokenFile) {
				f.TokenFile = filepath.Join(c.DataPath, f.TokenFile)
			}
			// Read the token once now so a missing or empty file fails at
			// startup. The value is not kept: Token reads the file again.
			if _, err := readTokenFile(f.TokenFile); err != nil {
				fail("%v", err)
			}
		}

		for _, list := range []struct {
			key      string
			patterns []string
		}{{"include", f.Include}, {"exclude", f.Exclude}} {
			for j, p := range list.patterns {
				if err := validateForgeGlob(p); err != nil {
					fail("%s[%d] %q: %v", list.key, j, p, err)
				}
			}
		}

		switch {
		case f.Sync == 0:
			f.Sync = DefaultForgeSync
		case f.Sync < MinForgeSync:
			fail("sync %s is too short: use at least %s", f.Sync, MinForgeSync)
		}

		f.Prefix = strings.Trim(strings.TrimSpace(f.Prefix), "/")
		if f.Prefix == "" {
			f.Prefix = f.Name
		}
		if err := validateRepoPrefix(f.Prefix); err != nil {
			fail("prefix %q: %v", f.Prefix, err)
			continue
		}
		for other, otherKey := range prefixes {
			if f.Prefix == other || strings.HasPrefix(f.Prefix+"/", other+"/") || strings.HasPrefix(other+"/", f.Prefix+"/") {
				fail("prefix %q overlaps the prefix %q of %s; imported repository names could collide", f.Prefix, other, otherKey)
			}
		}
		prefixes[f.Prefix] = key
	}

	return errors.Join(errs...)
}

// validateForgeURL checks a forge base URL.
func validateForgeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("must be an http or https URL")
	}
	if u.Hostname() == "" {
		return errors.New("must include a host")
	}
	if u.User != nil {
		return errors.New("must not include credentials; use token_file")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must not include a query or fragment")
	}
	return nil
}

// validateForgeGlob checks one include or exclude pattern.
func validateForgeGlob(p string) error {
	if strings.TrimSpace(p) == "" {
		return errors.New("pattern is empty")
	}
	// Lists travel to hook subprocesses as comma-separated environment
	// variables (see Environ), so a comma cannot be part of a pattern.
	if strings.Contains(p, ",") {
		return errors.New("pattern must not contain a comma; list alternatives as separate entries")
	}
	if _, err := glob.Compile(p, '/'); err != nil {
		return fmt.Errorf("invalid glob: %w", err)
	}
	return nil
}

// validateRepoPrefix checks a repository name prefix: one or more segments
// of letters, digits, '-', '_' and '.', none of them "." or "..".
func validateRepoPrefix(p string) error {
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." || !repoSegmentRe.MatchString(seg) || strings.HasSuffix(seg, ".git") {
			return errors.New("must be slash-separated segments of letters, digits, '-', '_' and '.' (not \".\", \"..\" or ending in .git)")
		}
	}
	return nil
}

// forgeEnviron returns the forges as SOFT_SERVE_FORGES_<i>_* environment
// variables. Token values are never included, only token_file paths.
func (c *Config) forgeEnviron() []string {
	var envs []string
	for i, f := range c.Forges {
		p := fmt.Sprintf("SOFT_SERVE_FORGES_%d_", i)
		envs = append(envs,
			p+"NAME="+f.Name,
			p+"KIND="+f.Kind,
			p+"URL="+f.URL,
			p+"TOKEN_FILE="+f.TokenFile,
			p+"INCLUDE="+strings.Join(f.Include, ","),
			p+"EXCLUDE="+strings.Join(f.Exclude, ","),
			p+"SYNC="+f.Sync.String(),
			p+"PREFIX="+f.Prefix,
		)
	}
	return envs
}
