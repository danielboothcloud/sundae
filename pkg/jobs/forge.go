package jobs

import (
	"context"
	"errors"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/forge"
)

// forgeJobPrefix prefixes the name of every forge sync job.
const forgeJobPrefix = "forge-sync:"

// RegisterForgeSync registers one scheduled sync job per configured forge,
// named "forge-sync:<forge>" and run every forge's sync interval. Forge sync
// jobs registered before are dropped first, so the registered jobs always
// match cfg. The jobs find the forge service in their context
// (forge.FromContext).
func RegisterForgeSync(cfg *config.Config) {
	mtx.Lock()
	defer mtx.Unlock()
	for name := range jobs {
		if strings.HasPrefix(name, forgeJobPrefix) {
			delete(jobs, name)
		}
	}
	for _, f := range cfg.Forges {
		jobs[forgeJobPrefix+f.Name] = &Job{Runner: forgeSync{name: f.Name, interval: f.Sync}}
	}
}

// forgeSync is the scheduled sync of one forge: refresh its listing, then
// sync every repository imported from it (forge.Service.SyncForge).
type forgeSync struct {
	name     string
	interval time.Duration
}

// Spec implements Runner.
func (f forgeSync) Spec(context.Context) string {
	interval := f.interval
	if interval <= 0 {
		interval = config.DefaultForgeSync
	}
	return "@every " + interval.String()
}

// Func implements Runner.
func (f forgeSync) Func(ctx context.Context) func() {
	logger := log.FromContext(ctx).WithPrefix("jobs.forge")
	return func() {
		svc := forge.FromContext(ctx)
		if svc == nil {
			logger.Warn("forge service not running, skipping sync", "forge", f.name)
			return
		}
		logger.Debug("syncing forge", "forge", f.name)
		results, err := svc.SyncForge(ctx, f.name)
		if errors.Is(err, forge.ErrSyncRunning) {
			logger.Info("previous sync still running, skipping", "forge", f.name)
			return
		}
		if err != nil {
			logger.Error("forge sync failed", "forge", f.name, "err", err)
			return
		}
		failed := 0
		for _, r := range results {
			if r.Error != "" {
				failed++
			}
		}
		logger.Debug("forge synced", "forge", f.name, "repos", len(results), "failed", failed)
	}
}
