package config

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/utils"
)

// ProjectsConfig configures how new repositories are initialised: the
// templates `repo create --template NAME` and the TUI's new-repository form
// offer.
//
// Templates is a map and can only be set in the config file; there is no
// environment form for it.
type ProjectsConfig struct {
	// Templates are named repository templates. A template is an existing
	// Soft Serve repository whose tree, at a ref and optionally under a
	// subdirectory, becomes the single initial commit of a new repository.
	// The template's history is not copied.
	Templates map[string]ProjectTemplate `yaml:"templates"`
}

// ProjectTemplate is a named repository template.
type ProjectTemplate struct {
	// Repo is the Soft Serve repository the template is read from.
	// Required. Whoever creates a repository from the template needs read
	// access to it.
	Repo string `yaml:"repo"`

	// Ref is the branch, tag or commit whose tree is copied. Empty is the
	// template repository's HEAD.
	Ref string `yaml:"ref"`

	// Subdir is a directory of the template repository to copy instead of
	// its whole tree: its contents become the new repository's root.
	Subdir string `yaml:"subdir"`

	// Description describes the template where it is offered (the TUI).
	// It is not copied to the new repository.
	Description string `yaml:"description"`

	// Profile names an entry of sessions.profiles that becomes the new
	// repository's default session profile, unless one is given when the
	// repository is created.
	Profile string `yaml:"profile"`
}

// templateRefRe matches what projects.templates.<name>.ref accepts: a
// branch, tag or full ref name, or a commit hash. Revision syntax (~, ^, :,
// @{...}) is not accepted.
var templateRefRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/+-]*$`)

// validateProjects validates the projects section and normalises its
// values. Every problem is reported, each naming its key. The template
// repositories are not looked up: the config is checked without a database.
func (c *Config) validateProjects() error {
	pc := &c.Projects
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("projects."+format, args...))
	}

	for _, name := range sortedKeys(pc.Templates) {
		t := pc.Templates[name]
		key := "templates." + name
		if !forgeNameRe.MatchString(name) {
			fail("templates: name %q must be a DNS label: lowercase letters, digits and hyphens, at most 63 characters", name)
		}

		t.Repo = strings.TrimSpace(t.Repo)
		if t.Repo == "" {
			fail("%s.repo is required", key)
		} else {
			repo := utils.SanitizeRepo(t.Repo)
			if err := utils.ValidateRepo(repo); err != nil || repo != strings.TrimSuffix(strings.Trim(t.Repo, "/"), ".git") {
				fail("%s.repo %q is not a repository name", key, t.Repo)
			} else {
				t.Repo = repo
			}
		}

		t.Ref = strings.TrimSpace(t.Ref)
		if t.Ref != "" && (!templateRefRe.MatchString(t.Ref) || strings.Contains(t.Ref, "..") ||
			strings.Contains(t.Ref, "//") || strings.HasSuffix(t.Ref, "/") || strings.HasSuffix(t.Ref, ".lock")) {
			fail("%s.ref %q must be a branch, tag or commit hash", key, t.Ref)
		}

		if sub, err := cleanSubdir(t.Subdir); err != nil {
			fail("%s.subdir %q %v", key, t.Subdir, err)
		} else {
			t.Subdir = sub
		}

		t.Description = strings.TrimSpace(t.Description)

		t.Profile = strings.TrimSpace(t.Profile)
		if _, ok := c.Sessions.Profiles[t.Profile]; t.Profile != "" && !ok {
			fail("%s.profile %q is not defined in sessions.profiles", key, t.Profile)
		}
		pc.Templates[name] = t
	}

	return errors.Join(errs...)
}

// cleanSubdir returns a template subdirectory as a clean slash-separated
// path relative to the repository root; "" for the root itself.
func cleanSubdir(s string) (string, error) {
	s = strings.TrimSpace(s)
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", errors.New("must be a slash-separated path without control characters or backslashes")
		}
	}
	s = strings.Trim(s, "/")
	if s == "" {
		return "", nil
	}
	clean := path.Clean(s)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("must stay inside the template repository")
	}
	if clean == "." {
		return "", nil
	}
	return clean, nil
}

// TemplateNames returns the names of the configured templates, sorted.
func (pc ProjectsConfig) TemplateNames() []string {
	return sortedKeys(pc.Templates)
}
