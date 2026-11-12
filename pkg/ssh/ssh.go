package ssh

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	"charm.land/wish/v2"
	rm "charm.land/wish/v2/recover"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/events"
	"github.com/charmbracelet/soft-serve/pkg/forge"
	"github.com/charmbracelet/soft-serve/pkg/openshell"
	"github.com/charmbracelet/soft-serve/pkg/signals"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	gossh "golang.org/x/crypto/ssh"
)

var (
	publicKeyCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "ssh",
		Name:      "public_key_auth_total",
		Help:      "The total number of public key auth requests",
	}, []string{"allowed"})

	keyboardInteractiveCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "ssh",
		Name:      "keyboard_interactive_auth_total",
		Help:      "The total number of keyboard interactive auth requests",
	}, []string{"allowed"})
)

// SSHServer is a SSH server that implements the git protocol.
type SSHServer struct { //nolint: revive
	srv    *ssh.Server
	cfg    *config.Config
	be     *backend.Backend
	ctx    context.Context
	logger *log.Logger
}

// NewSSHServer returns a new SSHServer.
func NewSSHServer(ctx context.Context) (*SSHServer, error) {
	cfg := config.FromContext(ctx)
	logger := log.FromContext(ctx).WithPrefix("ssh")
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	be := backend.FromContext(ctx)

	var err error
	s := &SSHServer{
		cfg:    cfg,
		ctx:    ctx,
		be:     be,
		logger: logger,
	}

	mw := []wish.Middleware{
		rm.MiddlewareWithLogger(
			logger,
			// BubbleTea middleware: the TUI, which can hand the
			// terminal to an agent session and take it back.
			TUIMiddleware,
			// CLI middleware.
			CommandMiddleware,
			// Logging middleware.
			LoggingMiddleware,
			// Authentication middleware.
			// gossh.PublicKeyHandler doesn't guarantee that the public key
			// is in fact the one used for authentication, so we need to
			// check it again here.
			AuthenticationMiddleware,
			// Forge middleware: attaches the forge service, when forges
			// are configured, for the `forge` command.
			ForgeContextMiddleware(forge.FromContext(ctx)),
			// Events middleware: attaches the event bus, for session
			// lifecycle events and subscribers.
			EventsContextMiddleware(events.FromContext(ctx)),
			// Monitor middleware: attaches the session monitor, when
			// OpenShell is enabled, for session status, bridge
			// tracking and parking.
			MonitorContextMiddleware(openshell.MonitorFromContext(ctx)),
			// Signals middleware: attaches the signals feed, when
			// OpenShell is enabled, for `session signals`.
			SignalsContextMiddleware(signals.FromContext(ctx)),
			// Context middleware.
			// This must come first to set up the context.
			ContextMiddleware(cfg, dbx, datastore, be, logger),
		),
	}

	opts := []ssh.Option{
		ssh.PublicKeyAuth(s.PublicKeyHandler),
		ssh.KeyboardInteractiveAuth(s.KeyboardInteractiveHandler),
		wish.WithAddress(cfg.SSH.ListenAddr),
		wish.WithHostKeyPath(cfg.SSH.KeyPath),
		wish.WithMiddleware(mw...),
	}

	// TODO: Support a real PTY in future version.
	opts = append(opts, ssh.EmulatePty())

	s.srv, err = wish.NewServer(opts...)
	if err != nil {
		return nil, err
	}

	if config.IsDebug() {
		s.srv.ServerConfigCallback = func(_ ssh.Context) *gossh.ServerConfig {
			return &gossh.ServerConfig{
				AuthLogCallback: func(conn gossh.ConnMetadata, method string, err error) {
					logger.Debug("authentication", "user", conn.User(), "method", method, "err", err)
				},
			}
		}
	}

	if cfg.SSH.MaxTimeout > 0 {
		s.srv.MaxTimeout = time.Duration(cfg.SSH.MaxTimeout) * time.Second
	}

	if cfg.SSH.IdleTimeout > 0 {
		s.srv.IdleTimeout = time.Duration(cfg.SSH.IdleTimeout) * time.Second
	}

	// Create client ssh key
	if _, err := os.Stat(cfg.SSH.ClientKeyPath); err != nil && os.IsNotExist(err) {
		_, err := keygen.New(cfg.SSH.ClientKeyPath, keygen.WithKeyType(keygen.Ed25519), keygen.WithWrite())
		if err != nil {
			return nil, fmt.Errorf("client ssh key: %w", err)
		}
	}

	return s, nil
}

// ListenAndServe starts the SSH server.
func (s *SSHServer) ListenAndServe() error {
	return s.srv.ListenAndServe()
}

// Serve starts the SSH server on the given net.Listener.
func (s *SSHServer) Serve(l net.Listener) error {
	return s.srv.Serve(l)
}

// Close closes the SSH server.
func (s *SSHServer) Close() error {
	return s.srv.Close()
}

// Shutdown gracefully shuts down the SSH server.
func (s *SSHServer) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

func initializePermissions(ctx ssh.Context) {
	perms := ctx.Permissions()
	if perms == nil || perms.Permissions == nil {
		perms = &ssh.Permissions{Permissions: &gossh.Permissions{}}
	}
	if perms.Extensions == nil {
		perms.Extensions = make(map[string]string)
	}
}

// PublicKeyHandler handles public key authentication.
func (s *SSHServer) PublicKeyHandler(ctx ssh.Context, pk ssh.PublicKey) (allowed bool) {
	if pk == nil {
		return false
	}

	allowed = true

	// XXX: store the first "approved" public-key fingerprint in the
	// permissions block to use for authentication later.
	initializePermissions(ctx)
	perms := ctx.Permissions()

	// Set the public key fingerprint to be used for authentication.
	perms.Extensions["pubkey-fp"] = gossh.FingerprintSHA256(pk)
	ctx.SetValue(ssh.ContextKeyPermissions, perms)

	return
}

// KeyboardInteractiveHandler handles keyboard interactive authentication.
// This is used after all public key authentication has failed.
func (s *SSHServer) KeyboardInteractiveHandler(ctx ssh.Context, _ gossh.KeyboardInteractiveChallenge) bool {
	ac := s.be.AllowKeyless(ctx)
	keyboardInteractiveCounter.WithLabelValues(strconv.FormatBool(ac)).Inc()

	// If we're allowing keyless access, reset the public key fingerprint
	initializePermissions(ctx)
	perms := ctx.Permissions()

	if ac {
		// XXX: reset the public-key fingerprint. This is used to validate the
		// public key being used to authenticate.
		perms.Extensions["pubkey-fp"] = ""
		ctx.SetValue(ssh.ContextKeyPermissions, perms)
	}
	return ac
}
