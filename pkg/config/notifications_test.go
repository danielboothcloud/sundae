package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
)

func TestNotificationsDefaults(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	is.Equal(cfg.Notifications.On, []string{"blocked", "done", "error"})
	is.Equal(cfg.Notifications.Debounce, 30*time.Second)
	is.True(!cfg.Notifications.Ntfy.Enabled)
	cfg.DataPath = t.TempDir()
	is.NoErr(cfg.Validate())
}

func TestValidateNotifications(t *testing.T) {
	valid := func(t *testing.T) *Config {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		cfg.Notifications.Ntfy = NtfyConfig{Enabled: true, URL: "https://ntfy.sh", Topic: "agents_x1"}
		return cfg
	}
	cases := map[string]struct {
		mutate func(*Config)
		errs   []string
	}{
		"valid": {mutate: func(*Config) {}},
		"disabled checks nothing": {mutate: func(c *Config) {
			c.Notifications = NotificationsConfig{
				On:       []string{"nope"},
				Debounce: -time.Second,
				Ntfy:     NtfyConfig{URL: "ftp://x", Topic: "bad topic", TokenFile: "missing", ClickURL: "{nope}"},
			}
		}},
		"self-hosted url with path": {mutate: func(c *Config) { c.Notifications.Ntfy.URL = "http://10.0.0.7:8080/ntfy/" }},
		"opt-in triggers":           {mutate: func(c *Config) { c.Notifications.On = []string{"Blocked", "session_failed", "forge_failed", "blocked"} }},
		"zero debounce":             {mutate: func(c *Config) { c.Notifications.Debounce = 0 }},
		"token file": {mutate: func(c *Config) {
			c.Notifications.Ntfy.TokenFile = writeTokenFile(t, c.DataPath, "ntfy-token", "tk_x\n")
		}},
		"click url": {mutate: func(c *Config) {
			c.Notifications.Ntfy.ClickURL = "https://git.example.com/{repo}?session={session}&u={user}&sb={sandbox}"
		}},
		"click url other scheme": {mutate: func(c *Config) { c.Notifications.Ntfy.ClickURL = "moshi://{sandbox}" }},
		"unknown trigger": {
			mutate: func(c *Config) { c.Notifications.On = []string{"blocked", "blokced"} },
			errs:   []string{`notifications.on[1] "blokced" is not a trigger: use idle, working, blocked, done, error, session_failed, forge_failed`},
		},
		"empty triggers": {
			mutate: func(c *Config) { c.Notifications.On = []string{} },
			errs:   []string{"notifications.on is empty"},
		},
		"negative debounce": {
			mutate: func(c *Config) { c.Notifications.Debounce = -time.Second },
			errs:   []string{"notifications.debounce -1s must not be negative"},
		},
		"missing url": {
			mutate: func(c *Config) { c.Notifications.Ntfy.URL = "" },
			errs:   []string{"notifications.ntfy.url is required"},
		},
		"url not http": {
			mutate: func(c *Config) { c.Notifications.Ntfy.URL = "ntfy.sh" },
			errs:   []string{`notifications.ntfy.url "ntfy.sh": must be an http or https URL`},
		},
		"url with credentials": {
			mutate: func(c *Config) { c.Notifications.Ntfy.URL = "https://me:pw@ntfy.sh" },
			errs:   []string{"notifications.ntfy.url", "must not include credentials"},
		},
		"url is the topic url": {
			mutate: func(c *Config) { c.Notifications.Ntfy.URL = "https://ntfy.sh/agents_x1" },
			errs:   []string{`notifications.ntfy.url "https://ntfy.sh/agents_x1" must be the server's base URL`},
		},
		"missing topic": {
			mutate: func(c *Config) { c.Notifications.Ntfy.Topic = " " },
			errs:   []string{"notifications.ntfy.topic is required"},
		},
		"bad topic": {
			mutate: func(c *Config) { c.Notifications.Ntfy.Topic = "a/b" },
			errs:   []string{`notifications.ntfy.topic "a/b" must be 1 to 64`},
		},
		"missing token file": {
			mutate: func(c *Config) { c.Notifications.Ntfy.TokenFile = "nope" },
			errs:   []string{"notifications.ntfy.token_file: read token_file:"},
		},
		"empty token file": {mutate: func(c *Config) {
			c.Notifications.Ntfy.TokenFile = writeTokenFile(t, c.DataPath, "ntfy-token", "\n")
		}, errs: []string{"notifications.ntfy.token_file: token_file", "is empty"}},
		"unknown placeholder": {
			mutate: func(c *Config) { c.Notifications.Ntfy.ClickURL = "https://x/{repository}" },
			errs:   []string{`notifications.ntfy.click_url "https://x/{repository}": unknown placeholder {repository}`},
		},
		"relative click url": {
			mutate: func(c *Config) { c.Notifications.Ntfy.ClickURL = "/{repo}" },
			errs:   []string{"must be an absolute URL"},
		},
		"unbalanced braces": {
			mutate: func(c *Config) { c.Notifications.Ntfy.ClickURL = "https://x/{repo" },
			errs:   []string{"unbalanced braces"},
		},
		"every problem reported": {mutate: func(c *Config) {
			c.Notifications.On = []string{"x"}
			c.Notifications.Ntfy.URL = ""
			c.Notifications.Ntfy.Topic = ""
		}, errs: []string{`on[0] "x"`, "ntfy.url is required", "ntfy.topic is required"}},
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

func TestValidateNotificationsNormalises(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	writeTokenFile(t, cfg.DataPath, "ntfy-token", "tk_first\n")
	cfg.Notifications.On = []string{" Blocked", "done", "blocked", "FORGE_FAILED"}
	cfg.Notifications.Ntfy = NtfyConfig{Enabled: true, URL: " https://ntfy.example.com/ ", Topic: " agents ", TokenFile: "ntfy-token"}
	is.NoErr(cfg.Validate())

	nc := cfg.Notifications
	is.Equal(nc.On, []string{"blocked", "done", "forge_failed"})
	is.True(nc.Notifies("forge_failed"))
	is.True(!nc.Notifies("error"))
	is.Equal(nc.Ntfy.URL, "https://ntfy.example.com")
	is.Equal(nc.Ntfy.Topic, "agents")
	is.Equal(nc.Ntfy.TokenFile, filepath.Join(cfg.DataPath, "ntfy-token"))

	// The token file is read on every call.
	tok, err := nc.Ntfy.Token()
	is.NoErr(err)
	is.Equal(tok, "tk_first")
	writeTokenFile(t, cfg.DataPath, "ntfy-token", "tk_second\n")
	tok, err = nc.Ntfy.Token()
	is.NoErr(err)
	is.Equal(tok, "tk_second")
}

func TestExpandClickURL(t *testing.T) {
	is := is.New(t)
	n := NtfyConfig{ClickURL: "https://git.example.com/{repo}?s={session}&u={user}"}
	is.Equal(n.ExpandClickURL("infra/net", "claude 1", "sundae-alice-infra", "alice"),
		"https://git.example.com/infra/net?s=claude%201&u=alice")
	// Query metacharacters in a value cannot add parameters.
	is.Equal(n.ExpandClickURL("r", "a&b=c#d", "", "alice"), "https://git.example.com/r?s=a%26b%3Dc%23d&u=alice")
	// A placeholder without a value drops the click URL.
	is.Equal(n.ExpandClickURL("r", "", "sb", "alice"), "")
	is.Equal(NtfyConfig{}.ExpandClickURL("r", "s", "sb", "u"), "")
	is.Equal(NtfyConfig{ClickURL: "https://x"}.ExpandClickURL("", "", "", ""), "https://x")
}

func TestParseNotificationsFile(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	writeTokenFile(t, td, "ntfy-token", "tk_x\n")
	p := filepath.Join(td, "config.yaml")

	// An unquoted `on` key decodes (yaml.v3 reads it as a string key).
	is.NoErr(os.WriteFile(p, []byte(`notifications:
  on: [blocked, session_failed]
  debounce: 1m
  ntfy:
    enabled: true
    url: https://ntfy.sh
    topic: agents
    token_file: ntfy-token
    click_url: "https://git.example.com/{repo}"
`), 0o600))
	cfg := DefaultConfig()
	cfg.DataPath = td
	is.NoErr(parseFile(cfg, p))
	nc := cfg.Notifications
	is.Equal(nc.On, []string{"blocked", "session_failed"})
	is.Equal(nc.Debounce, time.Minute)
	is.True(nc.Ntfy.Enabled)
	is.Equal(nc.Ntfy.TokenFile, filepath.Join(td, "ntfy-token"))

	// Inline secrets are unknown keys; a YAML 1.1 tool's `true` key gets a
	// hint.
	is.NoErr(os.WriteFile(p, []byte(`notifications:
  "true": [blocked]
  ntfy:
    token: tk_inline
`), 0o600))
	err := parseFile(&Config{DataPath: td}, p)
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), `line 2: unknown key notifications.true (an unquoted`)) // got: err
	is.True(strings.Contains(err.Error(), "line 4: unknown key notifications.ntfy.token"))
}

func TestNotificationsEnvironRoundTrip(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataPath = td
	cfg.Notifications = NotificationsConfig{
		On:       []string{"blocked", "forge_failed"},
		Debounce: 45 * time.Second,
		Ntfy: NtfyConfig{
			Enabled:   true,
			URL:       "https://ntfy.example.com",
			Topic:     "agents",
			TokenFile: writeTokenFile(t, td, "ntfy-token", "tk_secret\n"),
			ClickURL:  "https://git.example.com/{repo}?s={session}",
		},
	}
	is.NoErr(cfg.Validate())

	environ := cfg.Environ()
	is.True(slices.Contains(environ, "SUNDAE_NOTIFICATIONS_ON=blocked,forge_failed"))
	for _, e := range environ {
		is.True(!strings.Contains(e, "tk_secret"))
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	got := DefaultConfig()
	is.NoErr(got.ParseEnv())
	is.Equal(got.Notifications, cfg.Notifications)
}

func TestNotificationsConfigFileRoundTrip(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Notifications = NotificationsConfig{
		On:       []string{"blocked", "error"},
		Debounce: 0,
		Ntfy: NtfyConfig{
			Enabled:   true,
			URL:       "https://ntfy.sh",
			Topic:     "agents",
			TokenFile: writeTokenFile(t, cfg.DataPath, "ntfy-token", "tk_x\n"),
			ClickURL:  `https://git.example.com/{repo}?q="x"`,
		},
	}
	is.NoErr(cfg.Validate())
	is.NoErr(cfg.WriteConfig())

	got := &Config{DataPath: cfg.DataPath}
	is.NoErr(parseFile(got, cfg.ConfigPath()))
	is.Equal(got.Notifications, cfg.Notifications)

	// The default template parses and keeps the defaults.
	def := DefaultConfig()
	def.DataPath = t.TempDir()
	is.NoErr(def.WriteConfig())
	got = &Config{DataPath: def.DataPath}
	is.NoErr(parseFile(got, def.ConfigPath()))
	is.Equal(got.Notifications.On, []string{"blocked", "done", "error"})
	is.Equal(got.Notifications.Debounce, 30*time.Second)
	is.True(!got.Notifications.Ntfy.Enabled)
}
