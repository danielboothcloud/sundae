package hooks

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/danielboothcloud/sundae/git"
	"github.com/danielboothcloud/sundae/pkg/config"
)

func TestGenerateHooks(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = tmp
	repoPath := filepath.Join(tmp, "repos", "test.git")
	_, err := git.Init(repoPath, true)
	if err != nil {
		t.Fatal(err)
	}

	if err := GenerateHooks(context.TODO(), cfg, "test.git"); err != nil {
		t.Fatal(err)
	}

	for _, hn := range []string{
		PreReceiveHook,
		UpdateHook,
		PostReceiveHook,
		PostUpdateHook,
	} {
		if _, err := os.Stat(filepath.Join(repoPath, "hooks", hn)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(repoPath, "hooks", hn+".d", "sundae")); err != nil {
			t.Fatal(err)
		}
	}
}
