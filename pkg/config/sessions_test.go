package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
)

func TestSessionsDefaults(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	is.NoErr(cfg.Validate())
	is.Equal(cfg.Sessions.Osewa.BinaryDir, DefaultOsewaBinaryDir)
	is.Equal(cfg.Sessions.Osewa.Port, DefaultOsewaPort)

	// A zeroed section gets the defaults too.
	cfg.Sessions = SessionsConfig{}
	is.NoErr(cfg.Validate())
	is.Equal(cfg.Sessions.Osewa.BinaryDir, DefaultOsewaBinaryDir)
	is.Equal(cfg.Sessions.Osewa.Port, DefaultOsewaPort)

	// A relative binary_dir is resolved against the data path.
	cfg.Sessions.Osewa.BinaryDir = "osewa"
	is.NoErr(cfg.Validate())
	is.Equal(cfg.Sessions.Osewa.BinaryDir, filepath.Join(cfg.DataPath, "osewa"))
}

func TestValidateSessions(t *testing.T) {
	valid := func() *Config {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		cfg.Sessions.Profiles = map[string]SessionProfile{
			"claude": {Image: "agent:1", Command: "claude --resume"},
			"shell":  {},
		}
		cfg.Sessions.Repos = map[string]SessionRepoDefaults{
			"infra/*": {Profile: "claude"},
		}
		return cfg
	}
	cases := map[string]struct {
		mutate func(*Config)
		err    string
	}{
		"valid":    {func(*Config) {}, ""},
		"bad port": {func(c *Config) { c.Sessions.Osewa.Port = 70000 }, "sessions.osewa.port 70000 must be between 1 and 65535"},
		"negative port": {
			func(c *Config) { c.Sessions.Osewa.Port = -1 }, "sessions.osewa.port -1",
		},
		"profile name": {
			func(c *Config) { c.Sessions.Profiles["Bad_Name"] = SessionProfile{} },
			`sessions.profiles: name "Bad_Name" must be a DNS label`,
		},
		"profile command": {
			func(c *Config) { c.Sessions.Profiles["claude"] = SessionProfile{Command: `claude "unterminated`} },
			`sessions.profiles.claude.command "claude \"unterminated" is not a valid command line`,
		},
		"repo without profile": {
			func(c *Config) { c.Sessions.Repos["x/*"] = SessionRepoDefaults{} },
			`sessions.repos["x/*"].profile is required`,
		},
		"repo unknown profile": {
			func(c *Config) { c.Sessions.Repos["x/*"] = SessionRepoDefaults{Profile: "nope"} },
			`sessions.repos["x/*"].profile "nope" is not defined in sessions.profiles`,
		},
		"repo bad glob": {
			func(c *Config) { c.Sessions.Repos["x/[a"] = SessionRepoDefaults{Profile: "claude"} },
			`sessions.repos["x/[a"]: invalid glob`,
		},
		"openshell command": {
			func(c *Config) {
				c.OpenShell.Enabled, c.OpenShell.GatewayAddress, c.OpenShell.Image, c.OpenShell.Command = true, "gw:1", "img", `bash "-l`
			},
			`openshell: command "bash \"-l" is not a valid command line`,
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

func TestRepoProfile(t *testing.T) {
	is := is.New(t)
	s := SessionsConfig{Repos: map[string]SessionRepoDefaults{
		"**":            {Profile: "any"},
		"infra/*":       {Profile: "infra"},
		"infra/cluster": {Profile: "cluster"},
		"Docs/**":       {Profile: "docs"},
		"x/*/y":         {Profile: "a"},
		"x/z/*":         {Profile: "b"},
	}}
	for repo, want := range map[string]string{
		"infra/dns":       "infra",
		"infra/cluster":   "cluster", // exact beats glob
		"infra/a/b":       "any",     // "*" does not cross "/"
		"docs/site/guide": "docs",    // case-insensitive
		"plain":           "any",
		"x/z/y":           "a", // equal specificity and length: "x/*/y" sorts first
	} {
		is.Equal(s.RepoProfile(repo), want) // repo
	}
	is.Equal(SessionsConfig{}.RepoProfile("anything"), "")
}

func TestSessionsConfigFileRoundTrip(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Sessions.Osewa.Port = 7400
	cfg.Sessions.Profiles = map[string]SessionProfile{
		"claude": {Image: "ghcr.io/me/agent-claude:latest", Command: `claude --model "opus"`},
	}
	cfg.Sessions.Repos = map[string]SessionRepoDefaults{"infra/*": {Profile: "claude"}}
	is.NoErr(cfg.WriteConfig())

	got := &Config{DataPath: cfg.DataPath}
	is.NoErr(parseFile(got, cfg.ConfigPath()))
	is.Equal(got.Sessions.Osewa.Port, 7400)
	is.Equal(got.Sessions.Osewa.BinaryDir, DefaultOsewaBinaryDir)
	is.Equal(got.Sessions.Profiles["claude"], cfg.Sessions.Profiles["claude"])
	is.Equal(got.Sessions.Repos["infra/*"].Profile, "claude")
}

func TestSessionsUnknownKeys(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	p := filepath.Join(td, "config.yaml")
	is.NoErr(os.WriteFile(p, []byte(`sessions:
  osewa:
    version: v0.1.4
  profiles:
    claude:
      agent: claude
  repos:
    "infra/*":
      profil: claude
`), 0o600))

	err := DefaultConfig().Load(p)
	is.True(err != nil)
	for _, want := range []string{
		"line 3: unknown key sessions.osewa.version",
		"line 6: unknown key sessions.profiles.<name>.agent",
		"line 9: unknown key sessions.repos.<name>.profil",
	} {
		is.True(strings.Contains(err.Error(), want)) // missing: want
	}
}

func TestSessionsEnv(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	t.Setenv("SOFT_SERVE_DATA_PATH", td)
	t.Setenv("SOFT_SERVE_SESSIONS_OSEWA_BINARY_DIR", "/opt/osewa")
	t.Setenv("SOFT_SERVE_SESSIONS_OSEWA_PORT", "7401")
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.Sessions.Osewa.BinaryDir, "/opt/osewa")
	is.Equal(cfg.Sessions.Osewa.Port, 7401)

	envs := strings.Join(cfg.Environ(), "\n")
	is.True(strings.Contains(envs, "SOFT_SERVE_SESSIONS_OSEWA_BINARY_DIR=/opt/osewa\n"))
	is.True(strings.Contains(envs, "SOFT_SERVE_SESSIONS_OSEWA_PORT=7401"))
}
