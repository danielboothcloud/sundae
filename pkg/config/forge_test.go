package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
)

func TestValidateForges(t *testing.T) {
	valid := func(t *testing.T) *Config {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		cfg.Forges = []ForgeConfig{{Name: "github", Kind: "github"}}
		return cfg
	}
	cases := map[string]struct {
		mutate func(*Config)
		errs   []string
	}{
		"valid": {mutate: func(*Config) {}},
		"missing name": {
			mutate: func(c *Config) { c.Forges[0].Name = "" },
			errs:   []string{"forges[0]: name is required"},
		},
		"name not a DNS label": {
			mutate: func(c *Config) { c.Forges[0].Name = "Git_Hub" },
			errs:   []string{`forges[0] (Git_Hub): name "Git_Hub" must be a DNS label`},
		},
		"duplicate name": {mutate: func(c *Config) {
			c.Forges = append(c.Forges, ForgeConfig{Name: "github", Kind: "gitlab", Prefix: "gl"})
		}, errs: []string{`forges[1] (github): name "github" is already used by forges[0]`}},
		"missing kind": {
			mutate: func(c *Config) { c.Forges[0].Kind = "" },
			errs:   []string{"forges[0] (github): kind is required"},
		},
		"unknown kind": {
			mutate: func(c *Config) { c.Forges[0].Kind = "gitea" },
			errs:   []string{`forges[0] (github): kind "gitea" is not supported`},
		},
		"kind is case-insensitive": {mutate: func(c *Config) { c.Forges[0].Kind = "GitHub" }},
		"url not http": {
			mutate: func(c *Config) { c.Forges[0].URL = "ssh://github.com" },
			errs:   []string{`forges[0] (github): url "ssh://github.com": must be an http or https URL`},
		},
		"url with credentials": {
			mutate: func(c *Config) { c.Forges[0].URL = "https://me:secret@github.com" },
			errs:   []string{"must not include credentials"},
		},
		"url without host": {
			mutate: func(c *Config) { c.Forges[0].URL = "https://" },
			errs:   []string{"must include a host"},
		},
		"private url is allowed": {mutate: func(c *Config) {
			c.Forges[0].Kind = "gitlab"
			c.Forges[0].URL = "http://10.0.0.5:8080/gitlab/"
		}},
		"token file": {mutate: func(c *Config) {
			c.Forges[0].TokenFile = writeTokenFile(t, c.DataPath, "gh-token", "ghp_x\n")
		}},
		"missing token file": {
			mutate: func(c *Config) { c.Forges[0].TokenFile = "nope" },
			errs:   []string{"forges[0] (github): read token_file:"},
		},
		"empty token file": {mutate: func(c *Config) {
			c.Forges[0].TokenFile = writeTokenFile(t, c.DataPath, "gh-token", "\n")
		}, errs: []string{"forges[0] (github): token_file", "is empty"}},
		"bad include glob": {
			mutate: func(c *Config) { c.Forges[0].Include = []string{"me/[a"} },
			errs:   []string{`forges[0] (github): include[0] "me/[a": invalid glob`},
		},
		"empty exclude glob": {
			mutate: func(c *Config) { c.Forges[0].Exclude = []string{"me/*", " "} },
			errs:   []string{`forges[0] (github): exclude[1] " ": pattern is empty`},
		},
		"comma in glob": {
			mutate: func(c *Config) { c.Forges[0].Include = []string{"me/{a,b}"} },
			errs:   []string{"must not contain a comma"},
		},
		"sync too short": {
			mutate: func(c *Config) { c.Forges[0].Sync = 30 * time.Second },
			errs:   []string{"forges[0] (github): sync 30s is too short"},
		},
		"negative sync": {
			mutate: func(c *Config) { c.Forges[0].Sync = -time.Minute },
			errs:   []string{"sync -1m0s is too short"},
		},
		"bad prefix": {
			mutate: func(c *Config) { c.Forges[0].Prefix = "a/../b" },
			errs:   []string{`forges[0] (github): prefix "a/../b"`},
		},
		"overlapping prefixes": {mutate: func(c *Config) {
			c.Forges[0].Prefix = "code"
			c.Forges = append(c.Forges, ForgeConfig{Name: "gitlab", Kind: "gitlab", Prefix: "code/gitlab"})
		}, errs: []string{`forges[1] (gitlab): prefix "code/gitlab" overlaps the prefix "code" of forges[0] (github)`}},
		"same prefix as another name": {mutate: func(c *Config) {
			c.Forges = append(c.Forges, ForgeConfig{Name: "work", Kind: "github", Prefix: "github"})
		}, errs: []string{`prefix "github" overlaps`}},
		"every problem reported": {mutate: func(c *Config) {
			c.Forges = append(c.Forges, ForgeConfig{Name: "x", Kind: "svn", Sync: time.Second})
		}, errs: []string{`kind "svn"`, "sync 1s is too short"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			cfg := valid(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if len(tc.errs) == 0 {
				is.NoErr(err)
				return
			}
			is.True(err != nil)
			for _, want := range tc.errs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestValidateForgesDefaults(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	writeTokenFile(t, cfg.DataPath, "gl-token", "glpat-x\n")
	cfg.Forges = []ForgeConfig{
		{Name: "github", Kind: "github"},
		{Name: "gitlab", Kind: "GitLab", URL: "https://gitlab.example.com/", TokenFile: "gl-token", Prefix: "/work/gl/"},
	}
	is.NoErr(cfg.Validate())

	gh := cfg.Forges[0]
	is.Equal(gh.URL, "https://github.com")
	is.Equal(gh.Sync, 10*time.Minute)
	is.Equal(gh.Prefix, "github")

	gl := cfg.Forges[1]
	is.Equal(gl.Kind, "gitlab")
	is.Equal(gl.URL, "https://gitlab.example.com")
	is.Equal(gl.TokenFile, filepath.Join(cfg.DataPath, "gl-token"))
	is.Equal(gl.Prefix, "work/gl")
	token, err := gl.Token()
	is.NoErr(err)
	is.Equal(token, "glpat-x")
}

func TestParseForgesFile(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	writeTokenFile(t, td, "gh-token", "ghp_x\n")
	p := filepath.Join(td, "config.yaml")
	is.NoErr(os.WriteFile(p, []byte(`forges:
  - name: github
    kind: github
    token_file: gh-token
    include: ["me/*", "org/**"]
    exclude: ["me/old-*"]
    sync: 15m
  - name: gitlab
    kind: gitlab
    token: "inline-is-not-a-key"
`), 0o600))

	cfg := &Config{DataPath: td}
	err := parseFile(cfg, p)
	is.True(err != nil)
	// An inline token is refused as an unknown key, naming the list.
	is.True(strings.Contains(err.Error(), "line 10: unknown key forges[].token")) // got: err

	is.NoErr(os.WriteFile(p, []byte(`forges:
  - name: github
    kind: github
    token_file: gh-token
    include: ["me/*", "org/**"]
    exclude: ["me/old-*"]
    sync: 15m
`), 0o600))
	cfg = &Config{DataPath: td}
	is.NoErr(parseFile(cfg, p))
	is.Equal(len(cfg.Forges), 1)
	is.Equal(cfg.Forges[0].Include, []string{"me/*", "org/**"})
	is.Equal(cfg.Forges[0].Exclude, []string{"me/old-*"})
	is.Equal(cfg.Forges[0].Sync, 15*time.Minute)
	is.Equal(cfg.Forges[0].TokenFile, filepath.Join(td, "gh-token"))
}

// TestForgesEnvironRoundTrip checks that hook subprocesses, which rebuild
// the config from Environ, see the same forges, and never the token.
func TestForgesEnvironRoundTrip(t *testing.T) {
	is := is.New(t)
	no := false
	td := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataPath = td
	cfg.Forges = []ForgeConfig{
		{Name: "github", Kind: "github", TokenFile: writeTokenFile(t, td, "gh-token", "ghp_secret\n"), Include: []string{"me/*", "org/**"}},
		{Name: "gitlab", Kind: "gitlab", URL: "https://gitlab.example.com", Exclude: []string{"x/*"}, Sync: time.Hour, Prefix: "gl", Keep: []string{"push-*"}, ProtectDefaultBranch: &no, Protect: ForgeProtect{Deny: []string{"release/*"}, FastForwardOnly: []string{"develop", "main"}}, PrivatePaths: []string{"AGENTS.md", "_bmad/"}},
	}
	is.NoErr(cfg.Validate())

	environ := cfg.Environ()
	for _, e := range environ {
		is.True(!strings.Contains(e, "ghp_secret"))
	}
	for _, e := range environ {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}

	got := DefaultConfig()
	is.NoErr(got.ParseEnv())
	is.Equal(got.Forges, cfg.Forges)
}

func TestForgesConfigFileRoundTrip(t *testing.T) {
	is := is.New(t)
	no := false
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Forges = []ForgeConfig{
		{Name: "github", Kind: "github", TokenFile: writeTokenFile(t, cfg.DataPath, "gh-token", "ghp_x\n"), Include: []string{"me/*"}},
		{Name: "gitlab", Kind: "gitlab", URL: "https://gitlab.example.com", Sync: 2 * time.Hour, Prefix: "gl", Forward: []string{"**"}, ProtectDefaultBranch: &no, Protect: ForgeProtect{FastForwardOnly: []string{"develop"}}, PrivatePaths: []string{".claude/"}},
	}
	is.NoErr(cfg.Validate())
	is.NoErr(cfg.WriteConfig())

	got := &Config{DataPath: cfg.DataPath}
	is.NoErr(parseFile(got, cfg.ConfigPath()))
	is.Equal(got.Forges, cfg.Forges)
}

func TestForgeBranchRule(t *testing.T) {
	no := false
	cases := map[string]struct {
		forge         ForgeConfig
		branch        string
		defaultBranch string
		want          BranchRule
	}{
		"default forwards all":              {branch: "develop", defaultBranch: "main", want: BranchRule{Forward: true}},
		"default forwards slashed branches": {branch: "feature/x", want: BranchRule{Forward: true}},
		"keep push-*": {
			forge: ForgeConfig{Keep: []string{"push-*"}}, branch: "push-abc", want: BranchRule{},
		},
		"keep does not match other": {
			forge: ForgeConfig{Keep: []string{"push-*"}}, branch: "develop", want: BranchRule{Forward: true},
		},
		"keep wins over forward": {
			forge: ForgeConfig{Forward: []string{"**"}, Keep: []string{"wip/**"}}, branch: "wip/a/b", want: BranchRule{},
		},
		"deny": {
			forge: ForgeConfig{Protect: ForgeProtect{Deny: []string{"release-*"}}}, branch: "release-1", want: BranchRule{Deny: true},
		},
		"default branch protected": {branch: "main", defaultBranch: "main", want: BranchRule{Deny: true}},
		"unknown default branch":   {branch: "main", want: BranchRule{Forward: true}},
		"default branch protection off": {
			forge: ForgeConfig{ProtectDefaultBranch: &no}, branch: "main", defaultBranch: "main", want: BranchRule{Forward: true},
		},
		"fast-forward only": {
			forge:  ForgeConfig{Protect: ForgeProtect{FastForwardOnly: []string{"develop"}}},
			branch: "develop", want: BranchRule{Forward: true, FastForwardOnly: true},
		},
		"feature/* matches one level": {
			forge: ForgeConfig{Forward: []string{"feature/*"}}, branch: "feature/a", want: BranchRule{Forward: true},
		},
		"feature/* does not match two levels": {
			forge: ForgeConfig{Forward: []string{"feature/*"}}, branch: "feature/a/b", want: BranchRule{},
		},
		"feature/** matches two levels": {
			forge: ForgeConfig{Forward: []string{"feature/**"}}, branch: "feature/a/b", want: BranchRule{Forward: true},
		},
		"** matches everything": {
			forge: ForgeConfig{Forward: []string{"**"}}, branch: "a/b/c", want: BranchRule{Forward: true},
		},
		"case-sensitive": {
			forge: ForgeConfig{Forward: []string{"Feature-*"}}, branch: "feature-a", want: BranchRule{},
		},
		"full match": {
			forge: ForgeConfig{Forward: []string{"dev"}}, branch: "develop", want: BranchRule{},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.forge.BranchRule(tc.branch, tc.defaultBranch); got != tc.want {
				t.Errorf("BranchRule(%q, %q) = %+v, want %+v", tc.branch, tc.defaultBranch, got, tc.want)
			}
		})
	}
}

func TestMatchPrivatePath(t *testing.T) {
	paths := []string{"AGENTS.md", "_bmad", ".claude"}
	for file, want := range map[string]bool{
		"AGENTS.md":          true,
		"_bmad/x/y.md":       true,
		".claude/settings":   true,
		"AGENTS.md.bak":      false,
		"_bmadx/y":           false,
		"docs/AGENTS.md":     false,
		"_bmad":              true,
		"README.md":          false,
		".claude-extra/file": false,
	} {
		if got := MatchPrivatePath(paths, file); got != want {
			t.Errorf("MatchPrivatePath(%q) = %v, want %v", file, got, want)
		}
	}
}

func TestValidateForgeRules(t *testing.T) {
	cases := map[string]struct {
		mutate func(*ForgeConfig)
		errs   []string
	}{
		"valid": {mutate: func(f *ForgeConfig) {
			f.Forward = []string{"*", "feature/**"}
			f.Keep = []string{"push-*"}
			f.Protect = ForgeProtect{Deny: []string{"release/*"}, FastForwardOnly: []string{"develop"}}
			f.PrivatePaths = []string{"AGENTS.md", "_bmad/", ".github/agents.md"}
		}},
		"bad forward glob": {
			mutate: func(f *ForgeConfig) { f.Forward = []string{"[a"} },
			errs:   []string{`forges[0] (github): forward[0] "[a": invalid glob`},
		},
		"empty keep glob": {
			mutate: func(f *ForgeConfig) { f.Keep = []string{"push-*", ""} },
			errs:   []string{`forges[0] (github): keep[1] "": pattern is empty`},
		},
		"bad deny glob": {
			mutate: func(f *ForgeConfig) { f.Protect.Deny = []string{"{a,b}"} },
			errs:   []string{`forges[0] (github): protect.deny[0] "{a,b}": pattern must not contain a comma`},
		},
		"bad fast_forward_only glob": {
			mutate: func(f *ForgeConfig) { f.Protect.FastForwardOnly = []string{"[x"} },
			errs:   []string{`forges[0] (github): protect.fast_forward_only[0] "[x": invalid glob`},
		},
		"empty private path": {
			mutate: func(f *ForgeConfig) { f.PrivatePaths = []string{"/"} },
			errs:   []string{`forges[0] (github): private_paths[0] "/": path is empty`},
		},
		"absolute private path": {
			mutate: func(f *ForgeConfig) { f.PrivatePaths = []string{"/etc/x"} },
			errs:   []string{`private_paths[0] "/etc/x": path must be relative`},
		},
		"dotdot private path": {
			mutate: func(f *ForgeConfig) { f.PrivatePaths = []string{"a/../b"} },
			errs:   []string{`private_paths[0] "a/../b": path must not contain ".."`},
		},
		"git private path": {
			mutate: func(f *ForgeConfig) { f.PrivatePaths = []string{".git/config"} },
			errs:   []string{`private_paths[0] ".git/config": path must not be inside .git or .jj`},
		},
		"jj private path": {
			mutate: func(f *ForgeConfig) { f.PrivatePaths = []string{".jj/"} },
			errs:   []string{`private_paths[0] ".jj/": path must not be inside .git or .jj`},
		},
		"unclean private path": {
			mutate: func(f *ForgeConfig) { f.PrivatePaths = []string{"./a", "a//b", "a//"} },
			errs:   []string{`private_paths[0] "./a": path must be clean`, `private_paths[1] "a//b": path must be clean`, `private_paths[2] "a//": path must be clean`},
		},
		"control character private path": {
			mutate: func(f *ForgeConfig) { f.PrivatePaths = []string{"x\nsrc", "a\tb"} },
			errs:   []string{`private_paths[0] "x\nsrc": path must not contain a control character`, `private_paths[1] "a\tb": path must not contain a control character`},
		},
		"every problem reported": {
			mutate: func(f *ForgeConfig) { f.Keep = []string{"[a"}; f.PrivatePaths = []string{"../x"} },
			errs:   []string{`keep[0] "[a"`, `private_paths[0] "../x"`},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			cfg := DefaultConfig()
			cfg.DataPath = t.TempDir()
			cfg.Forges = []ForgeConfig{{Name: "github", Kind: "github"}}
			tc.mutate(&cfg.Forges[0])
			err := cfg.Validate()
			if len(tc.errs) == 0 {
				is.NoErr(err)
				return
			}
			is.True(err != nil)
			for _, want := range tc.errs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestValidateForgeRulesDefaults(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Forges = []ForgeConfig{{Name: "github", Kind: "github", PrivatePaths: []string{"_bmad/", "AGENTS.md"}}}
	is.NoErr(cfg.Validate())
	f := cfg.Forges[0]
	is.Equal(f.Forward, []string{"**"})
	is.Equal(f.Keep, nil)
	is.True(f.ProtectsDefaultBranch())
	is.Equal(f.PrivatePaths, []string{"_bmad", "AGENTS.md"})
}

func TestParseForgeRulesFile(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	p := filepath.Join(td, "config.yaml")
	is.NoErr(os.WriteFile(p, []byte(`forges:
  - name: github
    kind: github
    forward: ["*", "feature/**"]
    keep: ["push-*"]
    protect_default_branch: false
    protect:
      deny: ["release/*"]
      fast_forward_only: ["develop"]
    private_paths: ["AGENTS.md", "_bmad/"]
`), 0o600))
	cfg := &Config{DataPath: td}
	is.NoErr(parseFile(cfg, p))
	f := cfg.Forges[0]
	is.Equal(f.Forward, []string{"*", "feature/**"})
	is.Equal(f.Keep, []string{"push-*"})
	is.True(!f.ProtectsDefaultBranch())
	is.Equal(f.Protect.Deny, []string{"release/*"})
	is.Equal(f.Protect.FastForwardOnly, []string{"develop"})
	is.Equal(f.PrivatePaths, []string{"AGENTS.md", "_bmad"})
}
