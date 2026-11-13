package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
)

func TestSessionsDefaults(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	is.NoErr(cfg.Validate())
	is.Equal(cfg.Sessions.Osewa.BinaryDir, DefaultOsewaBinaryDir)
	is.Equal(cfg.Sessions.Osewa.Port, DefaultOsewaPort)
	is.Equal(cfg.Sessions.Status.PollInterval, DefaultStatusPollInterval)
	is.True(cfg.Sessions.Suspend.Enabled)
	is.Equal(cfg.Sessions.Suspend.After, DefaultSuspendAfter)
	is.Equal(cfg.Sessions.Signals.Keep, DefaultSignalsKeep)

	// A zeroed section gets the defaults too, except that parking stays
	// off: false is a value, not an absence.
	cfg.Sessions = SessionsConfig{}
	is.NoErr(cfg.Validate())
	is.Equal(cfg.Sessions.Osewa.BinaryDir, DefaultOsewaBinaryDir)
	is.Equal(cfg.Sessions.Osewa.Port, DefaultOsewaPort)
	is.Equal(cfg.Sessions.Status.PollInterval, DefaultStatusPollInterval)
	is.Equal(cfg.Sessions.Suspend.After, DefaultSuspendAfter)
	is.Equal(cfg.Sessions.Signals.Keep, DefaultSignalsKeep)
	is.True(!cfg.Sessions.Suspend.Enabled)

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
		"poll interval too short": {
			func(c *Config) { c.Sessions.Status.PollInterval = 500 * time.Millisecond },
			"sessions.status.poll_interval 500ms must be at least 1s",
		},
		"negative poll interval": {
			func(c *Config) { c.Sessions.Status.PollInterval = -time.Second },
			"sessions.status.poll_interval -1s must be at least 1s",
		},
		"negative suspend after": {
			func(c *Config) { c.Sessions.Suspend.After = -time.Minute },
			"sessions.suspend.after -1m0s must not be negative",
		},
		"suspend after shorter than poll": {
			func(c *Config) { c.Sessions.Suspend.After = 5 * time.Second },
			"sessions.suspend.after 5s must not be shorter than status.poll_interval 10s",
		},
		"signals keep negative": {
			func(c *Config) { c.Sessions.Signals.Keep = -1 },
			"sessions.signals.keep -1 must be between 1 and 100000",
		},
		"signals keep too large": {
			func(c *Config) { c.Sessions.Signals.Keep = MaxSignalsKeep + 1 },
			"sessions.signals.keep 100001 must be between 1 and 100000",
		},
		"signals keep max": {
			func(c *Config) { c.Sessions.Signals.Keep = MaxSignalsKeep }, "",
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
	cfg.Sessions.Status.PollInterval = 15 * time.Second
	cfg.Sessions.Suspend = SessionSuspendConfig{Enabled: false, After: 2 * time.Hour}
	cfg.Sessions.Signals.Keep = 250
	cfg.Sessions.Profiles = map[string]SessionProfile{
		"claude": {Image: "ghcr.io/me/agent-claude:latest", Command: `claude --model "opus"`},
	}
	cfg.Sessions.Repos = map[string]SessionRepoDefaults{"infra/*": {Profile: "claude"}}
	is.NoErr(cfg.WriteConfig())

	got := &Config{DataPath: cfg.DataPath}
	is.NoErr(parseFile(got, cfg.ConfigPath()))
	is.Equal(got.Sessions.Osewa.Port, 7400)
	is.Equal(got.Sessions.Osewa.BinaryDir, DefaultOsewaBinaryDir)
	is.Equal(got.Sessions.Status.PollInterval, 15*time.Second)
	is.Equal(got.Sessions.Suspend, SessionSuspendConfig{Enabled: false, After: 2 * time.Hour, Fallback: SuspendFallbackNone})
	is.Equal(got.Sessions.Signals.Keep, 250)
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
  status:
    interval: 5s
  suspend:
    idle: 1h
  signals:
    max: 10
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
		"line 5: unknown key sessions.status.interval",
		"line 7: unknown key sessions.suspend.idle",
		"line 9: unknown key sessions.signals.max",
		"line 12: unknown key sessions.profiles.<name>.agent",
		"line 15: unknown key sessions.repos.<name>.profil",
	} {
		is.True(strings.Contains(err.Error(), want)) // missing: want
	}
}

func TestSessionsEnv(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	t.Setenv("SUNDAE_DATA_PATH", td)
	t.Setenv("SUNDAE_SESSIONS_OSEWA_BINARY_DIR", "/opt/osewa")
	t.Setenv("SUNDAE_SESSIONS_OSEWA_PORT", "7401")
	t.Setenv("SUNDAE_SESSIONS_STATUS_POLL_INTERVAL", "20s")
	t.Setenv("SUNDAE_SESSIONS_SUSPEND_ENABLED", "false")
	t.Setenv("SUNDAE_SESSIONS_SUSPEND_AFTER", "45m")
	t.Setenv("SUNDAE_SESSIONS_SIGNALS_KEEP", "500")
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.Sessions.Osewa.BinaryDir, "/opt/osewa")
	is.Equal(cfg.Sessions.Osewa.Port, 7401)
	is.Equal(cfg.Sessions.Status.PollInterval, 20*time.Second)
	is.Equal(cfg.Sessions.Suspend, SessionSuspendConfig{Enabled: false, After: 45 * time.Minute, Fallback: SuspendFallbackNone})
	is.Equal(cfg.Sessions.Signals.Keep, 500)

	envs := strings.Join(cfg.Environ(), "\n")
	is.True(strings.Contains(envs, "SUNDAE_SESSIONS_OSEWA_BINARY_DIR=/opt/osewa\n"))
	is.True(strings.Contains(envs, "SUNDAE_SESSIONS_OSEWA_PORT=7401"))
	is.True(strings.Contains(envs, "SUNDAE_SESSIONS_STATUS_POLL_INTERVAL=20s"))
	is.True(strings.Contains(envs, "SUNDAE_SESSIONS_SUSPEND_ENABLED=false"))
	is.True(strings.Contains(envs, "SUNDAE_SESSIONS_SUSPEND_AFTER=45m0s"))
	is.True(strings.Contains(envs, "SUNDAE_SESSIONS_SIGNALS_KEEP=500"))

	// Hook subprocesses rebuild the config from Environ.
	for _, kv := range cfg.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "SUNDAE_SESSIONS_") {
			t.Setenv(k, v)
		}
	}
	again := DefaultConfig()
	is.NoErr(again.ParseEnv())
	is.Equal(again.Sessions.Status, cfg.Sessions.Status)
	is.Equal(again.Sessions.Suspend, cfg.Sessions.Suspend)
	is.Equal(again.Sessions.Signals, cfg.Sessions.Signals)
}
