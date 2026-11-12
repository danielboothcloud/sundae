package forge

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/events"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	gossh "golang.org/x/crypto/ssh"
)

// busTestUser is a minimal proto.User.
type busTestUser struct{ id int64 }

func (u busTestUser) ID() int64                     { return u.id }
func (u busTestUser) Username() string              { return "u" }
func (u busTestUser) IsAdmin() bool                 { return false }
func (u busTestUser) PublicKeys() []gossh.PublicKey { return nil }
func (u busTestUser) Password() string              { return "" }

func newTestBus(t *testing.T) (*events.Bus, *events.Subscription) {
	t.Helper()
	bus := events.New(events.WithLogger(log.New(io.Discard)))
	t.Cleanup(bus.Close)
	return bus, bus.Subscribe(events.Buffer(64))
}

func nextForgeEvent(t *testing.T, s *events.Subscription) events.Event {
	t.Helper()
	select {
	case ev := <-s.C:
		if ev.Kind != events.KindForge || ev.Forge == nil || ev.ID == 0 {
			t.Fatalf("not a forge event: %+v", ev)
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no forge event on the bus")
	}
	return events.Event{}
}

func TestBusNotifier(t *testing.T) {
	bus, sub := newTestBus(t)
	n := BusNotifier(bus)
	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

	// A scheduled sync: no user.
	n.ForgeEvent(context.Background(), Event{Kind: EventSync, Forge: "github", Repo: "github/me/x", Path: "me/x", Time: at})
	ev := nextForgeEvent(t, sub)
	want := events.ForgeOutcome{Kind: "sync", Forge: "github", Repo: "github/me/x", Path: "me/x", OK: true}
	if *ev.Forge != want || ev.UserID != 0 || !ev.Time.Equal(at) {
		t.Fatalf("event = %+v / %+v", ev, *ev.Forge)
	}

	// A push run by a user, failing.
	ctx := proto.WithUserContext(context.Background(), busTestUser{id: 7})
	n.ForgeEvent(ctx, Event{Kind: EventPush, Forge: "github", Repo: "github/me/x", Err: errors.New("rejected: non-fast-forward")})
	ev = nextForgeEvent(t, sub)
	if ev.UserID != 7 || ev.Forge.OK || ev.Forge.Error != "rejected: non-fast-forward" || ev.Forge.Kind != "push" || ev.Time.IsZero() {
		t.Fatalf("event = %+v / %+v", ev, *ev.Forge)
	}

	// A nil bus is a no-op.
	BusNotifier(nil).ForgeEvent(ctx, Event{Kind: EventSync})
}

// Forge operations reach the bus through the service's notifier.
func TestServiceEventsReachBus(t *testing.T) {
	fx := newServiceFixture(t)
	bus, sub := newTestBus(t)
	fx.svc.SetNotifier(BusNotifier(bus))
	fx.upstream(t, "me/proj", false)
	fx.ff.add("me/broken", filepath.Join(fx.root, "missing.git"), false)

	results, err := fx.svc.Import(fx.ctx, fx.user, "gh", ImportOptions{Paths: []string{"me/proj", "me/broken"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v", results)
	}

	got := map[string]events.Event{}
	for range 3 { // discover, then one import each
		ev := nextForgeEvent(t, sub)
		got[ev.Forge.Kind+" "+ev.Forge.Path] = ev
	}
	if ev, ok := got["discover "]; !ok || !ev.Forge.OK || ev.Forge.Forge != "gh" {
		t.Fatalf("discover = %+v", got)
	}
	ok := got["import me/proj"]
	if ok.Forge == nil || !ok.Forge.OK || ok.Forge.Repo != "gh/me/proj" || ok.UserID != fx.user.ID() {
		t.Fatalf("import me/proj = %+v", ok)
	}
	failed := got["import me/broken"]
	if failed.Forge == nil || failed.Forge.OK || failed.Forge.Error == "" || failed.UserID != fx.user.ID() {
		t.Fatalf("import me/broken = %+v", failed)
	}
}
