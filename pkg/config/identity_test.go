package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
)

func TestValidateGitIdentities(t *testing.T) {
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
		"none": {mutate: func(*Config) {}},
		"both": {mutate: func(c *Config) {
			c.Sessions.GitIdentity = GitIdentity{Name: "Jane Doe", Email: "jane@example.com"}
			c.Forges[0].GitIdentity = ForgeGitIdentity{Name: "Jane O'Doe", Email: "12345+jane@users.noreply.github.com"}
		}},
		"session name missing": {
			mutate: func(c *Config) { c.Sessions.GitIdentity = GitIdentity{Email: "jane@example.com"} },
			errs:   []string{"sessions.git_identity.name is required when sessions.git_identity is set"},
		},
		"session email missing": {
			mutate: func(c *Config) { c.Sessions.GitIdentity = GitIdentity{Name: "Jane"} },
			errs:   []string{"sessions.git_identity.email is required when sessions.git_identity is set"},
		},
		"blank is unset": {
			mutate: func(c *Config) { c.Sessions.GitIdentity = GitIdentity{Name: " ", Email: "\t"} },
		},
		"session email not an email": {
			mutate: func(c *Config) { c.Sessions.GitIdentity = GitIdentity{Name: "Jane", Email: "jane at example"} },
			errs:   []string{`sessions.git_identity.email "jane at example" does not look like an email address`},
		},
		"two @": {
			mutate: func(c *Config) { c.Sessions.GitIdentity = GitIdentity{Name: "Jane", Email: "a@b@c"} },
			errs:   []string{`sessions.git_identity.email "a@b@c" does not look like an email address`},
		},
		"control character": {
			mutate: func(c *Config) { c.Sessions.GitIdentity = GitIdentity{Name: "Jane\nDoe", Email: "jane@example.com"} },
			errs:   []string{`sessions.git_identity.name "Jane\nDoe" must not contain control characters`},
		},
		"angle brackets": {
			mutate: func(c *Config) { c.Sessions.GitIdentity = GitIdentity{Name: "Jane <x>", Email: "<jane@example.com>"} },
			errs: []string{
				`sessions.git_identity.name "Jane <x>" must not contain '<' or '>'`,
				`sessions.git_identity.email "<jane@example.com>" must not contain '<' or '>'`,
			},
		},
		"too long": {
			mutate: func(c *Config) {
				c.Sessions.GitIdentity = GitIdentity{Name: strings.Repeat("a", MaxGitIdentityLen+1), Email: "jane@example.com"}
			},
			errs: []string{"sessions.git_identity.name is longer than 256 bytes"},
		},
		"forge email missing": {
			mutate: func(c *Config) { c.Forges[0].GitIdentity = ForgeGitIdentity{Name: "Jane"} },
			errs:   []string{"forges[0] (github): git_identity.email is required when git_identity is set"},
		},
		"forge bad email": {
			mutate: func(c *Config) { c.Forges[0].GitIdentity = ForgeGitIdentity{Name: "Jane", Email: "jane"} },
			errs:   []string{`forges[0] (github): git_identity.email "jane" does not look like an email address`},
		},
		"forge without name": {
			mutate: func(c *Config) {
				c.Forges[0].Name = ""
				c.Forges[0].GitIdentity = ForgeGitIdentity{Email: "jane@example.com"}
			},
			errs: []string{"forges[0]: git_identity.name is required"},
		},
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

func TestValidateGitIdentityTrims(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Sessions.GitIdentity = GitIdentity{Name: "  Jane Doe ", Email: " jane@example.com\t"}
	is.NoErr(cfg.Validate())
	is.Equal(cfg.Sessions.GitIdentity, GitIdentity{Name: "Jane Doe", Email: "jane@example.com"})
	is.Equal(cfg.Sessions.GitIdentity.String(), "Jane Doe <jane@example.com>")
}

func TestSessionGitIdentity(t *testing.T) {
	gh := GitIdentity{Name: "Jane GH", Email: "1+jane@users.noreply.github.com"}
	def := GitIdentity{Name: "Jane", Email: "jane@example.com"}
	cfg := DefaultConfig()
	cfg.Forges = []ForgeConfig{
		{Name: "github", Kind: "github", GitIdentity: ForgeGitIdentity(gh)},
		{Name: "gitlab", Kind: "gitlab"},
	}
	cfg.Sessions.GitIdentity = def

	for forge, want := range map[string]GitIdentity{
		"github":  gh,  // the forge's own
		"gitlab":  def, // a forge without one
		"":        def, // not imported from a forge
		"removed": def, // a link to a forge no longer configured
	} {
		if got := cfg.SessionGitIdentity(forge); got != want {
			t.Errorf("SessionGitIdentity(%q) = %v, want %v", forge, got, want)
		}
	}

	// Without a default, only forge repositories get an identity.
	cfg.Sessions.GitIdentity = GitIdentity{}
	for forge, want := range map[string]GitIdentity{"github": gh, "gitlab": {}, "": {}} {
		if got := cfg.SessionGitIdentity(forge); got != want {
			t.Errorf("no default: SessionGitIdentity(%q) = %v, want %v", forge, got, want)
		}
	}
	if (GitIdentity{}).IsSet() || (GitIdentity{Name: "x"}).IsSet() || !def.IsSet() {
		t.Error("IsSet")
	}
}

// TestGitIdentityEnvironRoundTrip checks that hook subprocesses, which
// rebuild the config from Environ, see the same identities.
func TestGitIdentityEnvironRoundTrip(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Sessions.GitIdentity = GitIdentity{Name: "Jane Doe", Email: "jane@example.com"}
	cfg.Forges = []ForgeConfig{
		{Name: "github", Kind: "github", GitIdentity: ForgeGitIdentity{Name: "Jane, \"GH\" O'Doe", Email: "1+jane@users.noreply.github.com"}},
		{Name: "gitlab", Kind: "gitlab"},
	}
	is.NoErr(cfg.Validate())

	envs := strings.Join(cfg.Environ(), "\n")
	is.True(strings.Contains(envs, "SUNDAE_SESSIONS_GIT_IDENTITY_NAME=Jane Doe\n"))
	is.True(strings.Contains(envs, "SUNDAE_SESSIONS_GIT_IDENTITY_EMAIL=jane@example.com\n"))
	is.True(strings.Contains(envs, "SUNDAE_FORGES_0_GIT_IDENTITY_EMAIL=1+jane@users.noreply.github.com\n"))

	for _, e := range cfg.Environ() {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	got := DefaultConfig()
	is.NoErr(got.ParseEnv())
	is.Equal(got.Sessions.GitIdentity, cfg.Sessions.GitIdentity)
	is.Equal(got.Forges, cfg.Forges)
}

func TestGitIdentityConfigFileRoundTrip(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	is.NoErr(cfg.WriteConfig())
	// The template documents both, commented out.
	bts, err := os.ReadFile(cfg.ConfigPath())
	is.NoErr(err)
	is.True(strings.Contains(string(bts), "  #git_identity:\n  #  name: Jane Doe\n"))
	is.True(strings.Contains(string(bts), "#    git_identity:\n#      name: Jane Doe\n"))

	cfg.Sessions.GitIdentity = GitIdentity{Name: `Jane "JD" Doe`, Email: "jane@example.com"}
	cfg.Forges = []ForgeConfig{
		{Name: "github", Kind: "github", GitIdentity: ForgeGitIdentity{Name: "Jané O'Doe", Email: "1+jane@users.noreply.github.com"}},
		{Name: "gitlab", Kind: "gitlab"},
	}
	is.NoErr(cfg.Validate())
	is.NoErr(cfg.WriteConfig())

	got := &Config{DataPath: cfg.DataPath}
	is.NoErr(parseFile(got, cfg.ConfigPath()))
	is.Equal(got.Sessions.GitIdentity, cfg.Sessions.GitIdentity)
	is.Equal(got.Forges, cfg.Forges)
}

func TestGitIdentityUnknownKeys(t *testing.T) {
	is := is.New(t)
	p := filepath.Join(t.TempDir(), "config.yaml")
	is.NoErr(os.WriteFile(p, []byte(`sessions:
  git_identity:
    name: Jane
    mail: jane@example.com
forges:
  - name: github
    kind: github
    git_identity:
      user: jane
      email: jane@example.com
`), 0o600))
	err := DefaultConfig().Load(p)
	is.True(err != nil)
	for _, want := range []string{
		"line 4: unknown key sessions.git_identity.mail",
		"line 9: unknown key forges[].git_identity.user",
		"sessions.git_identity.email is required",
		"forges[0] (github): git_identity.name is required",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from %v", want, err)
		}
	}
}
