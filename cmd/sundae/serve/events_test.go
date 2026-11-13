package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/db"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/forge"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/matryer/is"
)

// newTestServer builds a Server (without starting its listeners) from a
// fresh backend; configure may change the config first.
func newTestServer(t *testing.T, configure func(ctx context.Context)) *Server {
	t.Helper()
	is := is.New(t)
	ctx, cfg, be, dbx := setupTestBackendWithDB(t)
	cfg.SSH.ListenAddr = "127.0.0.1:0"
	cfg.HTTP.ListenAddr = "127.0.0.1:0"
	cfg.Stats.ListenAddr = "127.0.0.1:0"
	cfg.Git.ListenAddr = "127.0.0.1:0"
	ctx = backend.WithContext(ctx, be)
	ctx = db.WithContext(ctx, dbx)
	ctx = log.WithContext(ctx, log.New(io.Discard))
	if configure != nil {
		configure(ctx)
	}
	srv, err := NewServer(ctx)
	is.NoErr(err)
	return srv
}

func shutdown(t *testing.T, srv *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestServerEventBus(t *testing.T) {
	is := is.New(t)
	srv := newTestServer(t, nil)
	is.True(srv.Events != nil)
	is.True(srv.Notifier == nil) // notifications are off by default
	// Jobs and the SSH server are built from this context.
	is.True(events.FromContext(srv.ctx) == srv.Events)
	is.True(forge.FromContext(srv.ctx) == nil)

	sub := srv.Events.Subscribe()
	shutdown(t, srv)
	_, open := <-sub.C
	is.True(!open) // the bus is closed with the server
}

func TestServerNotifications(t *testing.T) {
	is := is.New(t)
	got := make(chan map[string]any, 10)
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- body
	}))
	defer ntfy.Close()

	srv := newTestServer(t, func(ctx context.Context) {
		cfg := config.FromContext(ctx)
		cfg.Notifications.Ntfy.Enabled = true
		cfg.Notifications.Ntfy.URL = ntfy.URL
		cfg.Notifications.Ntfy.Topic = "agents"
		cfg.Notifications.On = []string{"blocked", "forge_failed"}
		is.NoErr(cfg.Validate())
	})
	is.True(srv.Notifier != nil)

	srv.Events.Publish(events.SessionStatusEvent(1, events.SessionStatus{
		Session: events.Session{User: "admin", Repo: "infra", Sandbox: "sundae-admin-infra"},
		Record:  events.Record{State: events.StateBlocked, Kind: events.BlockQuestion, Msg: "Which region?"},
	}))
	select {
	case body := <-got:
		is.Equal(body["title"], "▲ blocked · infra / sundae-admin-infra")
		is.Equal(body["message"], "Has a question: Which region?")
	case <-time.After(5 * time.Second):
		t.Fatal("no notification")
	}
	shutdown(t, srv)
}

func TestServerSessionMonitor(t *testing.T) {
	is := is.New(t)

	// Off with OpenShell.
	srv := newTestServer(t, nil)
	is.True(srv.Monitor == nil)
	is.True(openshell.MonitorFromContext(srv.ctx) == nil)
	_, ok := srv.startMonitor()
	is.True(!ok)
	shutdown(t, srv)

	srv = newTestServer(t, func(ctx context.Context) {
		cfg := config.FromContext(ctx)
		cfg.OpenShell.Enabled = true
		cfg.OpenShell.GatewayAddress = "http://127.0.0.1:1" // nothing listens
		cfg.OpenShell.Image = "sandbox:latest"
		cfg.Sessions.Status.PollInterval = time.Hour
	})
	is.True(srv.Monitor != nil)
	// The SSH server is built from this context.
	is.True(openshell.MonitorFromContext(srv.ctx) == srv.Monitor)

	run, ok := srv.startMonitor()
	is.True(ok)
	_, again := srv.startMonitor()
	is.True(!again) // once
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		run()
	}()
	shutdown(t, srv) // stops the monitor before closing the bus
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("monitor still running after shutdown")
	}
	_, ok = srv.startMonitor()
	is.True(!ok) // not after shutdown
}
