package serve

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/danielboothcloud/sundae/pkg/access"
	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/db"
	"github.com/danielboothcloud/sundae/pkg/db/migrate"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/signals"
	"github.com/danielboothcloud/sundae/pkg/store"
	"github.com/danielboothcloud/sundae/pkg/store/database"
	"github.com/matryer/is"
	_ "modernc.org/sqlite" // sqlite driver
)

func setupTestBackend(tb testing.TB) (context.Context, *config.Config, *backend.Backend) {
	tb.Helper()
	ctx, cfg, be, _ := setupTestBackendWithDB(tb)
	return ctx, cfg, be
}

func setupTestBackendWithDB(tb testing.TB) (context.Context, *config.Config, *backend.Backend, *db.DB) {
	tb.Helper()
	is := is.New(tb)

	cfg := config.DefaultConfig()
	cfg.DataPath = tb.TempDir()
	is.NoErr(cfg.Validate())

	ctx := context.Background()
	ctx = config.WithContext(ctx, cfg)

	is.NoErr(os.MkdirAll(cfg.DataPath, os.ModePerm))
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	is.NoErr(err)
	tb.Cleanup(func() { dbx.Close() }) //nolint: errcheck

	is.NoErr(migrate.Migrate(ctx, dbx))

	dbstore := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, dbstore)
	be := backend.New(ctx, cfg, dbx, dbstore)

	return ctx, cfg, be, dbx
}

func TestWarnIfAnonAdminAccess(t *testing.T) {
	cases := []struct {
		name         string
		allowKeyless bool
		anonAccess   access.AccessLevel
		wantWarning  bool
	}{
		{"defaults: keyless disabled, read-only anon", false, access.ReadOnlyAccess, false},
		{"keyless allowed but anon access below admin", true, access.ReadWriteAccess, false},
		{"admin anon access but keyless disallowed", false, access.AdminAccess, false},
		{"keyless allowed with admin anon access: the dangerous combo", true, access.AdminAccess, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			is := is.New(t)
			ctx, cfg, be := setupTestBackend(t)

			allow := c.allowKeyless
			cfg.AllowKeyless = &allow
			cfg.AnonAccess = &c.anonAccess

			var buf bytes.Buffer
			logger := log.New(&buf)

			warnIfAnonAdminAccess(ctx, be, logger)

			gotWarning := strings.Contains(buf.String(), "WARNING")
			is.Equal(gotWarning, c.wantWarning)
		})
	}
}

func TestEnsureDefaultRepoDisabled(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	cfg.DefaultRepo = ""
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	is.Equal(len(repos), 0)
}

func TestEnsureDefaultRepoCreatesWhenMissing(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	r, err := be.Repository(ctx, "gitops")
	is.NoErr(err)
	is.Equal(r.Name(), "gitops")
	is.Equal(r.IsPrivate(), false)
}

func TestEnsureDefaultRepoIsIdempotent(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	cfg.DefaultRepo = "gitops"
	logger := log.New(os.Stderr)

	ensureDefaultRepo(ctx, cfg, be, logger)
	r, err := be.Repository(ctx, "gitops")
	is.NoErr(err)
	createdAt := r.CreatedAt()

	// Restart-safety: a second boot against the same data must not error or
	// touch the existing repository.
	ensureDefaultRepo(ctx, cfg, be, logger)

	r, err = be.Repository(ctx, "gitops")
	is.NoErr(err)
	is.Equal(r.CreatedAt(), createdAt)

	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	is.Equal(len(repos), 1)
}

func TestEnsureDefaultRepoInvalidNameSkipped(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	cfg.DefaultRepo = "../not valid!"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	is.Equal(len(repos), 0)
}

func TestEnsureDefaultRepoLooksUpExisting(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	owner, err := be.UserByID(ctx, 1)
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "gitops", owner, proto.RepositoryOptions{
		Private: true,
	})
	is.NoErr(err)

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	r, err := be.Repository(ctx, "gitops")
	is.NoErr(err)
	is.Equal(r.IsPrivate(), true)
}

func TestEnsureDefaultRepoOwnerLookupFailsSkipped(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	// Delete the built-in admin account (ID 1) so UserByID cannot find it.
	// ensureDefaultRepo must log and return, not panic.
	is.NoErr(be.DeleteUser(ctx, "admin"))

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	is.Equal(len(repos), 0)
}

func TestEnsureDefaultRepoLookupErrorSkipped(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be, dbx := setupTestBackendWithDB(t)

	// Create the repo directory on disk so Repository() passes os.Stat,
	// then close the DB so the lookup behind it fails with a non-not-found error.
	repoDir := filepath.Join(cfg.DataPath, "repos", "gitops.git")
	is.NoErr(os.MkdirAll(repoDir, os.ModePerm))
	is.NoErr(dbx.Close())

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	_, err := be.UserByID(ctx, 1)
	is.True(err != nil) // db is closed, sanity check we broke it correctly
}

func TestEnsureDefaultRepoCreateErrorSkipped(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires enforceable directory permissions as a non-root user")
	}

	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	// Make the "repos" directory read-only so CreateRepository's git.Init
	// fails for a reason other than the repo already existing.
	reposDir := filepath.Join(cfg.DataPath, "repos")
	is.NoErr(os.MkdirAll(reposDir, os.ModePerm))
	is.NoErr(os.Chmod(reposDir, 0o555))
	t.Cleanup(func() { os.Chmod(reposDir, 0o755) }) //nolint: errcheck

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	is.Equal(len(repos), 0)
}

// With OpenShell enabled the server keeps a signals feed from the bus, and
// shutting down ends its followers.
func TestServerSignalsFeed(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be, dbx := setupTestBackendWithDB(t)
	cfg.OpenShell.Enabled = true
	cfg.OpenShell.GatewayAddress = "127.0.0.1:1"
	cfg.OpenShell.Image = "sandbox:latest"
	cfg.Sessions.Signals.Keep = 7
	cfg.SSH.ListenAddr, cfg.HTTP.ListenAddr, cfg.Git.ListenAddr, cfg.Stats.ListenAddr = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
	is.NoErr(cfg.Validate())
	ctx = backend.WithContext(ctx, be)
	ctx = db.WithContext(ctx, dbx)
	ctx = log.WithContext(ctx, log.New(io.Discard))

	srv, err := NewServer(ctx)
	is.NoErr(err)
	is.True(srv.Signals != nil)
	is.Equal(srv.Signals.Keep(), 7)

	srv.Events.Publish(events.SessionLifecycleEvent(1, events.SessionLifecycle{
		Session: events.Session{User: "admin", Repo: "r", Name: "main", Sandbox: "sundae-admin-r"},
		Step:    events.LifecycleCreated,
	}))
	deadline := time.Now().Add(5 * time.Second)
	for srv.Signals.Len() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the signal was not recorded")
		}
		time.Sleep(time.Millisecond)
	}
	snap, feed := srv.Signals.Follow(signals.Query{UserID: 1})
	is.Equal(len(snap), 1)

	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	is.NoErr(srv.Shutdown(sctx))
	_, err = feed.Next(sctx)
	is.True(errors.Is(err, signals.ErrClosed))
}
