package serve

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"charm.land/log/v2"

	"charm.land/ssh"
	"github.com/danielboothcloud/sundae/pkg/access"
	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/cron"
	"github.com/danielboothcloud/sundae/pkg/daemon"
	"github.com/danielboothcloud/sundae/pkg/db"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/forge"
	"github.com/danielboothcloud/sundae/pkg/jobs"
	"github.com/danielboothcloud/sundae/pkg/notify"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/signals"
	sshsrv "github.com/danielboothcloud/sundae/pkg/ssh"
	"github.com/danielboothcloud/sundae/pkg/stats"
	"github.com/danielboothcloud/sundae/pkg/utils"
	"github.com/danielboothcloud/sundae/pkg/web"
	"golang.org/x/sync/errgroup"
)

// Server is the Sundae server.
type Server struct {
	SSHServer   *sshsrv.SSHServer
	GitDaemon   *daemon.GitDaemon
	HTTPServer  *web.HTTPServer
	StatsServer *stats.StatsServer
	CertLoader  *CertReloader
	Cron        *cron.Scheduler
	Config      *config.Config
	Backend     *backend.Backend
	DB          *db.DB
	// Events is the event bus, which lives as long as the server.
	Events *events.Bus
	// Notifier publishes notifications; nil when notifications are off.
	Notifier *notify.Notifier
	// Monitor collects session status and parks idle sessions; nil when
	// OpenShell is disabled.
	Monitor *openshell.Monitor
	// Signals keeps the signals feed, recording from the bus from setup
	// until Shutdown; nil when OpenShell is disabled.
	Signals *signals.History

	logger *log.Logger
	ctx    context.Context

	// monitorMu guards the running Monitor's cancel and done channel.
	monitorMu      sync.Mutex
	monitorCancel  context.CancelFunc
	monitorDone    chan struct{}
	monitorStopped bool
}

// NewServer returns a new *Server configured to serve Sundae. The SSH
// server key-pair will be created if none exists.
// It expects a context with *backend.Backend, *db.DB, *log.Logger, and
// *config.Config attached.
func NewServer(ctx context.Context) (*Server, error) {
	var err error
	cfg := config.FromContext(ctx)
	be := backend.FromContext(ctx)
	db := db.FromContext(ctx)
	logger := log.FromContext(ctx).WithPrefix("server")

	// The event bus lives as long as the server. It goes into the context
	// the cron jobs and the SSH server are built from, so sessions and jobs
	// publish to the same bus.
	bus := events.New(events.WithLogger(log.FromContext(ctx)))
	ctx = events.WithContext(ctx, bus)

	// The session monitor, like the bus, reaches the SSH server through
	// the context. It runs from Start until Shutdown.
	var monitor *openshell.Monitor
	if cfg.OpenShell.Enabled {
		monitor = openshell.NewMonitor(func() (openshell.Client, error) {
			return openshell.New(cfg.OpenShell)
		}, cfg, openshell.WithMonitorEvents(bus), openshell.WithMonitorLogger(log.FromContext(ctx)))
		ctx = openshell.ContextWithMonitor(ctx, monitor)
	}

	// The signals feed records session events from now on, so it has
	// what is published while the server starts. It reaches the SSH
	// server through the context.
	var sigs *signals.History
	if cfg.OpenShell.Enabled {
		sigs = signals.New(cfg.Sessions.Signals.Keep,
			signals.WithLogger(log.FromContext(ctx)),
			signals.WithStateLookup(func(sandbox string) (events.State, bool) {
				st, ok := monitor.Status(sandbox)
				return st.Record.State, ok && !st.Polled.IsZero()
			}))
		sigs.Start(bus)
		ctx = signals.WithContext(ctx, sigs)
	}

	// Forges are declared in the config file only. The service goes into
	// the context the cron jobs and the SSH server are built from.
	if len(cfg.Forges) > 0 {
		svc, err := forge.NewService(cfg, be, log.FromContext(ctx), forge.WithNotifier(forge.BusNotifier(bus)))
		if err != nil {
			return nil, fmt.Errorf("forges: %w", err)
		}
		ctx = forge.WithContext(ctx, svc)
	}
	jobs.RegisterForgeSync(cfg)

	srv := &Server{
		Config:  cfg,
		Backend: be,
		DB:      db,
		Events:  bus,
		Monitor: monitor,
		Signals: sigs,
		logger:  log.FromContext(ctx).WithPrefix("server"),
		ctx:     ctx,
	}

	// Notifications are off unless ntfy is enabled in the config file.
	if cfg.Notifications.Ntfy.Enabled {
		srv.Notifier = notify.New(cfg.Notifications, notify.WithLogger(log.FromContext(ctx)))
		srv.Notifier.Start(bus)
		logger.Info("ntfy notifications enabled", "url", cfg.Notifications.Ntfy.URL, "on", cfg.Notifications.On)
	}

	// Add cron jobs.
	sched := cron.NewScheduler(ctx)
	for n, j := range jobs.List() {
		id, err := sched.AddFunc(j.Runner.Spec(ctx), j.Runner.Func(ctx))
		if err != nil {
			logger.Warn("error adding cron job", "job", n, "err", err)
		}

		j.ID = id
	}

	srv.Cron = sched

	ensureDefaultRepo(ctx, cfg, be, logger)

	srv.SSHServer, err = sshsrv.NewSSHServer(ctx)
	if err != nil {
		return nil, fmt.Errorf("create ssh server: %w", err)
	}

	srv.GitDaemon, err = daemon.NewGitDaemon(ctx)
	if err != nil {
		return nil, fmt.Errorf("create git daemon: %w", err)
	}

	srv.HTTPServer, err = web.NewHTTPServer(ctx)
	if err != nil {
		return nil, fmt.Errorf("create http server: %w", err)
	}

	srv.StatsServer, err = stats.NewStatsServer(ctx)
	if err != nil {
		return nil, fmt.Errorf("create stats server: %w", err)
	}

	if cfg.HTTP.TLSKeyPath != "" && cfg.HTTP.TLSCertPath != "" {
		srv.CertLoader, err = NewCertReloader(cfg.HTTP.TLSCertPath, cfg.HTTP.TLSKeyPath, logger)
		if err != nil {
			return nil, fmt.Errorf("create cert reloader: %w", err)
		}

		srv.HTTPServer.SetTLSConfig(&tls.Config{
			GetCertificate: srv.CertLoader.GetCertificateFunc(),
		})
	}

	warnIfAnonAdminAccess(ctx, be, logger)

	return srv, nil
}

// warnIfAnonAdminAccess logs a loud warning if the server's effective,
// post-override settings grant unauthenticated (keyless) connections admin
// access. This checks effective runtime state via the backend, not just the
// new config-override fields, since the same risk exists whether the
// dangerous combination came from config or was set via `ssh sundae settings`
// on a previous run.
func warnIfAnonAdminAccess(ctx context.Context, be *backend.Backend, logger *log.Logger) {
	if !be.AllowKeyless(ctx) || be.AnonAccess(ctx) < access.AdminAccess {
		return
	}

	logger.Warn("################################################################")
	logger.Warn("# WARNING: anonymous keyless connections have ADMIN access.    #")
	logger.Warn("# Anyone who can reach this server has full control, no auth.  #")
	logger.Warn("# This is intended for local/dev use only. Do not expose this  #")
	logger.Warn("# server to an untrusted network.                              #")
	logger.Warn("################################################################")
}

// ensureDefaultRepo creates the repo named by cfg.DefaultRepo if it does not
// already exist. It never fails startup: it logs invalid names and creation
// errors, then returns.
func ensureDefaultRepo(ctx context.Context, cfg *config.Config, be *backend.Backend, logger *log.Logger) {
	if cfg.DefaultRepo == "" {
		return
	}

	name := utils.SanitizeRepo(cfg.DefaultRepo)
	if err := utils.ValidateRepo(name); err != nil {
		logger.Warn("invalid default_repo, skipping", "name", cfg.DefaultRepo, "err", err)
		return
	}

	if _, err := be.Repository(ctx, name); err == nil {
		return
	} else if !errors.Is(err, proto.ErrRepoNotFound) {
		logger.Warn("failed to look up default repo", "name", name, "err", err)
		return
	}

	// The migration always inserts a user at ID 1. The repos table requires
	// a non-null owner, so we attribute the repo to that account.
	owner, err := be.UserByID(ctx, 1)
	if err != nil {
		logger.Warn("failed to look up default repo owner, skipping", "name", name, "err", err)
		return
	}

	if _, err := be.CreateRepository(ctx, name, owner, proto.RepositoryOptions{}); err != nil && !errors.Is(err, proto.ErrRepoExist) {
		logger.Warn("failed to create default repo", "name", name, "err", err)
		return
	}

	logger.Info("created default repo", "name", name)
}

// ReloadCertificates reloads the TLS certificates for the HTTP server.
func (s *Server) ReloadCertificates() error {
	if s.CertLoader == nil {
		return nil
	}
	return s.CertLoader.Reload()
}

// Start starts the SSH server.
func (s *Server) Start() error {
	errg, _ := errgroup.WithContext(s.ctx)

	// optionally start the SSH server
	if s.Config.SSH.Enabled {
		errg.Go(func() error {
			s.logger.Print("Starting SSH server", "addr", s.Config.SSH.ListenAddr)
			if err := s.SSHServer.ListenAndServe(); !errors.Is(err, ssh.ErrServerClosed) {
				return err
			}
			return nil
		})
	}

	// optionally start the git daemon
	if s.Config.Git.Enabled {
		errg.Go(func() error {
			s.logger.Print("Starting Git daemon", "addr", s.Config.Git.ListenAddr)
			if err := s.GitDaemon.ListenAndServe(); !errors.Is(err, daemon.ErrServerClosed) {
				return err
			}
			return nil
		})
	}

	// optionally start the HTTP server
	if s.Config.HTTP.Enabled {
		errg.Go(func() error {
			s.logger.Print("Starting HTTP server", "addr", s.Config.HTTP.ListenAddr)
			if err := s.HTTPServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
	}

	// optionally start the Stats server
	if s.Config.Stats.Enabled {
		errg.Go(func() error {
			s.logger.Print("Starting Stats server", "addr", s.Config.Stats.ListenAddr)
			if err := s.StatsServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
	}

	errg.Go(func() error {
		s.Cron.Start()
		return nil
	})

	if run, ok := s.startMonitor(); ok {
		errg.Go(func() error {
			s.logger.Print("Starting session monitor",
				"poll_interval", s.Config.Sessions.Status.PollInterval,
				"suspend", s.Config.Sessions.Suspend.Enabled,
				"suspend_after", s.Config.Sessions.Suspend.After)
			run()
			return nil
		})
	}
	return errg.Wait()
}

// startMonitor prepares the session monitor to run, once, unless there is
// none or the server is shutting down. run runs it until stopMonitor.
func (s *Server) startMonitor() (run func(), ok bool) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()
	if s.Monitor == nil || s.monitorStopped || s.monitorCancel != nil {
		return nil, false
	}
	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan struct{})
	s.monitorCancel, s.monitorDone = cancel, done
	return func() {
		defer close(done)
		s.Monitor.Run(ctx)
	}, true
}

// stopMonitor stops the session monitor and waits, within ctx, for it to
// finish: polls in flight are abandoned, parks in flight are cancelled.
func (s *Server) stopMonitor(ctx context.Context) {
	s.monitorMu.Lock()
	s.monitorStopped = true
	cancel, done := s.monitorCancel, s.monitorDone
	s.monitorMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Shutdown lets the server gracefully shutdown.
func (s *Server) Shutdown(ctx context.Context) error {
	defer s.closeEvents(ctx)
	errg, ctx := errgroup.WithContext(ctx)
	errg.Go(func() error {
		return s.GitDaemon.Shutdown(ctx)
	})
	errg.Go(func() error {
		return s.HTTPServer.Shutdown(ctx)
	})
	errg.Go(func() error {
		return s.SSHServer.Shutdown(ctx)
	})
	errg.Go(func() error {
		return s.StatsServer.Shutdown(ctx)
	})
	errg.Go(func() error {
		for _, j := range jobs.List() {
			s.Cron.Remove(j.ID)
		}
		s.Cron.Stop()
		return nil
	})
	errg.Go(func() error {
		s.stopMonitor(ctx)
		return nil
	})
	errg.Go(func() error {
		// Ends `session signals --follow`, which the SSH server would
		// otherwise wait for.
		s.Signals.Close()
		return nil
	})
	// defer s.DB.Close() // nolint: errcheck
	return errg.Wait()
}

// closeEventsTimeout bounds how long Close waits for held notifications.
const closeEventsTimeout = 5 * time.Second

// closeEvents sends the notifications still held, within ctx, and closes the
// event bus.
func (s *Server) closeEvents(ctx context.Context) {
	if err := s.Notifier.Close(ctx); err != nil {
		s.logger.Warn("notifications not all sent at shutdown", "err", err)
	}
	s.Events.Close()
}

// Close closes the SSH server.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), closeEventsTimeout)
	defer cancel()
	defer s.closeEvents(ctx)
	var errg errgroup.Group
	errg.Go(s.GitDaemon.Close)
	errg.Go(s.HTTPServer.Close)
	errg.Go(s.SSHServer.Close)
	errg.Go(s.StatsServer.Close)
	errg.Go(func() error {
		s.Cron.Stop()
		return nil
	})
	errg.Go(func() error {
		s.stopMonitor(ctx)
		return nil
	})
	errg.Go(func() error {
		s.Signals.Close()
		return nil
	})
	// defer s.DB.Close() // nolint: errcheck
	return errg.Wait()
}
