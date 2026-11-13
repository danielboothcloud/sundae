package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
)

func TestValidateProjects(t *testing.T) {
	valid := func() *Config {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		cfg.Sessions.Profiles = map[string]SessionProfile{"claude": {Command: "claude"}}
		cfg.Projects.Templates = map[string]ProjectTemplate{
			"go":   {Repo: "templates/go", Ref: "main", Subdir: "service", Description: "Go service", Profile: "claude"},
			"bare": {Repo: "tpl"},
		}
		return cfg
	}
	cases := map[string]struct {
		mutate func(*Config)
		err    string
	}{
		"valid": {func(*Config) {}, ""},
		"name": {
			func(c *Config) { c.Projects.Templates["Go_Service"] = ProjectTemplate{Repo: "tpl"} },
			`projects.templates: name "Go_Service" must be a DNS label`,
		},
		"repo required": {
			func(c *Config) { c.Projects.Templates["x"] = ProjectTemplate{} },
			"projects.templates.x.repo is required",
		},
		"repo invalid": {
			func(c *Config) { c.Projects.Templates["x"] = ProjectTemplate{Repo: "a b"} },
			`projects.templates.x.repo "a b" is not a repository name`,
		},
		"repo escapes": {
			func(c *Config) { c.Projects.Templates["x"] = ProjectTemplate{Repo: "a/../b"} },
			`projects.templates.x.repo "a/../b" is not a repository name`,
		},
		"ref revision syntax": {
			func(c *Config) { c.Projects.Templates["x"] = ProjectTemplate{Repo: "tpl", Ref: "main~1"} },
			`projects.templates.x.ref "main~1" must be a branch, tag or commit hash`,
		},
		"ref option": {
			func(c *Config) { c.Projects.Templates["x"] = ProjectTemplate{Repo: "tpl", Ref: "--upload-pack=x"} },
			`projects.templates.x.ref "--upload-pack=x"`,
		},
		"ref dots": {
			func(c *Config) { c.Projects.Templates["x"] = ProjectTemplate{Repo: "tpl", Ref: "a..b"} },
			`projects.templates.x.ref "a..b"`,
		},
		"subdir escapes": {
			func(c *Config) { c.Projects.Templates["x"] = ProjectTemplate{Repo: "tpl", Subdir: "a/../../etc"} },
			`projects.templates.x.subdir "a/../../etc" must stay inside the template repository`,
		},
		"subdir backslash": {
			func(c *Config) { c.Projects.Templates["x"] = ProjectTemplate{Repo: "tpl", Subdir: `a\b`} },
			`projects.templates.x.subdir "a\\b" must be a slash-separated path`,
		},
		"unknown profile": {
			func(c *Config) { c.Projects.Templates["x"] = ProjectTemplate{Repo: "tpl", Profile: "nope"} },
			`projects.templates.x.profile "nope" is not defined in sessions.profiles`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.err == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("error = %v, want %q", err, tc.err)
			}
		})
	}
}

func TestValidateProjectsNormalises(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Projects.Templates = map[string]ProjectTemplate{
		"x": {Repo: " /templates/go.git/ ", Ref: " v1.2.0 ", Subdir: "/a/./b/", Description: "  Go  "},
		"y": {Repo: "tpl", Subdir: "."},
	}
	is.NoErr(cfg.Validate())
	is.Equal(cfg.Projects.Templates["x"], ProjectTemplate{Repo: "templates/go", Ref: "v1.2.0", Subdir: "a/b", Description: "Go"})
	is.Equal(cfg.Projects.Templates["y"].Subdir, "")
	is.Equal(cfg.Projects.TemplateNames(), []string{"x", "y"})
}

func TestProjectsConfigFileRoundTrip(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Sessions.Profiles = map[string]SessionProfile{"claude": {Command: "claude"}}
	cfg.Projects.Templates = map[string]ProjectTemplate{
		"go-service": {Repo: "templates/go", Ref: "main", Subdir: "svc", Description: `Go "service"`, Profile: "claude"},
	}
	is.NoErr(cfg.WriteConfig())

	got := &Config{DataPath: cfg.DataPath}
	is.NoErr(parseFile(got, cfg.ConfigPath()))
	is.Equal(got.Projects.Templates["go-service"], cfg.Projects.Templates["go-service"])

	// The default template has an empty, commented example.
	def := DefaultConfig()
	def.DataPath = t.TempDir()
	is.NoErr(def.WriteConfig())
	bts, err := os.ReadFile(def.ConfigPath())
	is.NoErr(err)
	is.True(strings.Contains(string(bts), "projects:\n  templates: {}\n  #  go-service:"))
	again := &Config{DataPath: def.DataPath}
	is.NoErr(parseFile(again, def.ConfigPath()))
	is.Equal(len(again.Projects.Templates), 0)
}

func TestProjectsUnknownKeys(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	p := filepath.Join(td, "config.yaml")
	is.NoErr(os.WriteFile(p, []byte(`projects:
  template: {}
  templates:
    go:
      repository: templates/go
`), 0o600))

	err := DefaultConfig().Load(p)
	is.True(err != nil)
	for _, want := range []string{
		"line 2: unknown key projects.template",
		"line 5: unknown key projects.templates.<name>.repository",
		"projects.templates.go.repo is required",
	} {
		is.True(strings.Contains(err.Error(), want)) // missing: want
	}
}
