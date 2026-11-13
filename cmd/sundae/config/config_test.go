package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sconfig "github.com/danielboothcloud/sundae/pkg/config"
	"github.com/matryer/is"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheck(t *testing.T) {
	td := t.TempDir()
	t.Setenv("SUNDAE_DATA_PATH", td)
	t.Setenv("SUNDAE_CONFIG_LOCATION", "")

	t.Run("default template", func(t *testing.T) {
		is := is.New(t)
		cfg := sconfig.DefaultConfig()
		is.NoErr(cfg.WriteConfig())
		is.NoErr(check(cfg.ConfigPath()))
	})

	t.Run("every problem", func(t *testing.T) {
		is := is.New(t)
		p := filepath.Join(td, "bad.yaml")
		writeFile(t, p, "title: typo\nopenshell:\n  enabled: true\n")
		err := check(p)
		is.True(err != nil)
		msg := err.Error()
		is.True(strings.Contains(msg, "3 problems:"))
		is.True(strings.Contains(msg, "\n  line 1: unknown key title"))
		is.True(strings.Contains(msg, "\n  openshell: gateway_address is required"))
		is.True(strings.Contains(msg, "\n  openshell: image is required"))
	})

	t.Run("forges", func(t *testing.T) {
		is := is.New(t)
		writeFile(t, filepath.Join(td, "gh-token"), "ghp_x\n")
		p := filepath.Join(td, "forges.yaml")
		writeFile(t, p, `forges:
  - name: github
    kind: github
    token_file: gh-token
    include: ["me/*"]
  - name: gitlab
    kind: gitlab
    url: http://10.0.0.5/gitlab
`)
		is.NoErr(check(p))

		writeFile(t, p, `forges:
  - name: github
    kind: github
    token: inline
    token_file: missing-token
  - name: github
    kind: gitea
    include: ["[x"]
    sync: 5s
`)
		err := check(p)
		is.True(err != nil)
		msg := err.Error()
		for _, want := range []string{
			"line 4: unknown key forges[].token",
			"forges[0] (github): read token_file",
			`forges[1] (github): name "github" is already used by forges[0]`,
			`forges[1] (github): kind "gitea" is not supported`,
			`forges[1] (github): include[0] "[x": invalid glob`,
			"forges[1] (github): sync 5s is too short",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("%q missing from:\n%s", want, msg)
			}
		}
	})

	t.Run("notifications", func(t *testing.T) {
		is := is.New(t)
		writeFile(t, filepath.Join(td, "ntfy-token"), "tk_x\n")
		p := filepath.Join(td, "notifications.yaml")
		writeFile(t, p, `notifications:
  "on": [blocked, done, session_failed]
  debounce: 1m
  ntfy:
    enabled: true
    url: https://ntfy.example.com
    topic: agents
    token_file: ntfy-token
    click_url: "https://git.example.com/{repo}?s={session}"
`)
		is.NoErr(check(p))

		// Disabled: nothing beyond types is checked.
		writeFile(t, p, `notifications:
  "on": [nonsense]
  ntfy:
    url: ftp://x
    token_file: missing
`)
		is.NoErr(check(p))

		writeFile(t, p, `notifications:
  "on": [blokced]
  ntfy:
    enabled: true
    token: tk_inline
    token_file: missing
    click_url: "https://x/{nope}"
`)
		err := check(p)
		is.True(err != nil)
		msg := err.Error()
		for _, want := range []string{
			"line 5: unknown key notifications.ntfy.token",
			`notifications.on[0] "blokced" is not a trigger`,
			"notifications.ntfy.url is required",
			"notifications.ntfy.topic is required",
			"notifications.ntfy.token_file: read token_file",
			`notifications.ntfy.click_url "https://x/{nope}": unknown placeholder {nope}`,
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("%q missing from:\n%s", want, msg)
			}
		}
	})

	t.Run("sessions signals", func(t *testing.T) {
		is := is.New(t)
		p := filepath.Join(td, "signals.yaml")
		writeFile(t, p, "sessions:\n  signals:\n    keep: 5000\n")
		is.NoErr(check(p))

		writeFile(t, p, "sessions:\n  signals:\n    keep: 0\n    max: 3\n")
		err := check(p)
		is.True(err != nil)
		is.True(strings.Contains(err.Error(), "line 4: unknown key sessions.signals.max"))

		writeFile(t, p, "sessions:\n  signals:\n    keep: 1000000\n")
		err = check(p)
		is.True(err != nil)
		is.True(strings.Contains(err.Error(), "sessions.signals.keep 1000000 must be between 1 and 100000"))
	})

	t.Run("projects", func(t *testing.T) {
		is := is.New(t)
		p := filepath.Join(td, "projects.yaml")
		writeFile(t, p, `sessions:
  profiles:
    claude:
      command: claude
projects:
  templates:
    go-service:
      repo: templates/go
      ref: main
      subdir: service
      description: Go service
      profile: claude
`)
		is.NoErr(check(p))

		writeFile(t, p, `projects:
  templates:
    go-service:
      repo: ""
      ref: "main~1"
      subdir: ../x
      profile: claude
      branch: main
`)
		err := check(p)
		is.True(err != nil)
		msg := err.Error()
		for _, want := range []string{
			"line 8: unknown key projects.templates.<name>.branch",
			"projects.templates.go-service.repo is required",
			`projects.templates.go-service.ref "main~1" must be a branch, tag or commit hash`,
			`projects.templates.go-service.subdir "../x" must stay inside the template repository`,
			`projects.templates.go-service.profile "claude" is not defined in sessions.profiles`,
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("%q missing from:\n%s", want, msg)
			}
		}
	})

	t.Run("missing file is not written", func(t *testing.T) {
		is := is.New(t)
		p := filepath.Join(td, "missing", "config.yaml")
		err := check(p)
		is.True(err != nil)
		is.True(strings.Contains(err.Error(), "does not exist"))
		_, err = os.Stat(filepath.Dir(p))
		is.True(errors.Is(err, fs.ErrNotExist))
	})
}

func TestCheckPath(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	t.Setenv("SUNDAE_DATA_PATH", td)

	t.Setenv("SUNDAE_CONFIG_LOCATION", "")
	is.Equal(checkPath(""), filepath.Join(td, "config.yaml"))

	// A configured location is checked even when it does not exist, rather
	// than falling back to the data path as the server does.
	loc := filepath.Join(td, "nope.yaml")
	t.Setenv("SUNDAE_CONFIG_LOCATION", loc)
	is.Equal(checkPath(""), loc)

	is.Equal(checkPath("other.yaml"), "other.yaml")
}
