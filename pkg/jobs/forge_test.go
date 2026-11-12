package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

func TestRegisterForgeSync(t *testing.T) {
	cfg := &config.Config{Forges: []config.ForgeConfig{
		{Name: "github", Sync: 15 * time.Minute},
		{Name: "gitlab", Sync: time.Hour},
	}}
	RegisterForgeSync(cfg)
	t.Cleanup(func() { RegisterForgeSync(&config.Config{}) })

	list := List()
	if _, ok := list["mirror-pull"]; !ok {
		t.Error("mirror-pull job dropped")
	}
	for name, spec := range map[string]string{
		"forge-sync:github": "@every 15m0s",
		"forge-sync:gitlab": "@every 1h0m0s",
	} {
		j, ok := list[name]
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if got := j.Runner.Spec(context.Background()); got != spec {
			t.Errorf("%s spec = %q, want %q", name, got, spec)
		}
	}

	// Registering again replaces the forge jobs.
	RegisterForgeSync(&config.Config{Forges: []config.ForgeConfig{{Name: "gitlab"}}})
	list = List()
	if _, ok := list["forge-sync:github"]; ok {
		t.Error("stale forge job kept")
	}
	if got := list["forge-sync:gitlab"].Runner.Spec(context.Background()); got != "@every 10m0s" {
		t.Errorf("default spec = %q", got)
	}

	// Without a forge service in the context the job is a logged no-op.
	list["forge-sync:gitlab"].Runner.Func(context.Background())()
}
