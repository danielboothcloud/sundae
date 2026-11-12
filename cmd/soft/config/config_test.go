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
