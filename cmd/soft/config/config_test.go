package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sconfig "github.com/charmbracelet/soft-serve/pkg/config"
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
	t.Setenv("SOFT_SERVE_DATA_PATH", td)
	t.Setenv("SOFT_SERVE_CONFIG_LOCATION", "")

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
	t.Setenv("SOFT_SERVE_DATA_PATH", td)

	t.Setenv("SOFT_SERVE_CONFIG_LOCATION", "")
	is.Equal(checkPath(""), filepath.Join(td, "config.yaml"))

	// A configured location is checked even when it does not exist, rather
	// than falling back to the data path as the server does.
	loc := filepath.Join(td, "nope.yaml")
	t.Setenv("SOFT_SERVE_CONFIG_LOCATION", loc)
	is.Equal(checkPath(""), loc)

	is.Equal(checkPath("other.yaml"), "other.yaml")
}
