package notify

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/events"
)

// fakeClock is a Clock whose timers fire only when the test advances it.
// Every timer the notifier arms is reported on armed, so the test can wait
// for the notifier to have handled an event without sleeping.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	armed  chan time.Time
}

type fakeTimer struct {
	c       chan time.Time
	at      time.Time
	stopped bool
	clock   *fakeClock
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: t0, armed: make(chan time.Time, 100)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: make(chan time.Time, 1), at: c.now.Add(d), clock: c}
	c.timers = append(c.timers, t)
	c.armed <- t.at
	return t
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

// advance moves the clock and fires the timers that are due.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.timers {
		if !t.stopped && !t.at.After(c.now) {
			t.stopped = true
			t.c <- c.now
		}
	}
}

// waitArmed waits until the notifier arms a timer for at.
func (c *fakeClock) waitArmed(t *testing.T, at time.Time) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-c.armed:
			if got.Equal(at) {
				return
			}
		case <-deadline:
			t.Fatalf("no timer armed for %v", at)
		}
	}
}

func nextRequest(t *testing.T, f *fakeNtfy) request {
	t.Helper()
	select {
	case r := <-f.got:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no notification published")
	}
	return request{}
}

func noRequest(t *testing.T, f *fakeNtfy) {
	t.Helper()
	select {
	case r := <-f.got:
		t.Fatalf("unexpected notification %q", r.body["title"])
	default:
	}
}

func status(state events.State, kind events.BlockKind, msg string) events.Event {
	return events.SessionStatusEvent(1, events.SessionStatus{
		Session: events.Session{User: "alice", Repo: "infra", Name: "claude-1", Sandbox: "sundae-alice-infra"},
		Record:  events.Record{State: state, Kind: kind, Msg: msg, App: "claude-code"},
	})
}

func TestNotifierEndToEnd(t *testing.T) {
	f := newFakeNtfy(t)
	clock := newFakeClock()
	bus := events.New(events.WithLogger(log.New(io.Discard)))
	defer bus.Close()

	n := New(config.NotificationsConfig{
		On:       config.DefaultTriggers,
		Debounce: 30 * time.Second,
		Ntfy: config.NtfyConfig{
			Enabled:  true,
			URL:      f.srv.URL,
			Topic:    "agents",
			ClickURL: "https://git.example.com/{repo}?s={session}",
		},
	}, WithClock(clock), WithLogger(log.New(io.Discard)), WithHTTPClient(f.srv.Client()))
	n.Start(bus)
	window := clock.Now().Add(30 * time.Second)

	// blocked goes out at once, at high priority.
	bus.Publish(status(events.StateBlocked, events.BlockPermission, "Apply 3 to add?"))
	r := nextRequest(t, f)
	if r.body["title"] != "▲ blocked · infra / claude-1" || r.body["priority"] != float64(4) ||
		r.body["message"] != "Needs permission: Apply 3 to add?\nclaude-code" ||
		r.body["click"] != "https://git.example.com/infra?s=claude-1" || r.body["topic"] != "agents" {
		t.Fatalf("blocked = %s", r.raw)
	}
	clock.waitArmed(t, window)

	// A burst inside the window is held...
	bus.Publish(status(events.StateWorking, "", "Planning"))
	clock.waitArmed(t, window)
	bus.Publish(status(events.StateBlocked, events.BlockPermission, "Apply 3 to add?"))
	clock.waitArmed(t, window)
	bus.Publish(status(events.StateDone, "", "Applied"))
	clock.waitArmed(t, window)
	noRequest(t, f)

	// ...and collapses into its latest state when the window closes.
	clock.advance(30 * time.Second)
	r = nextRequest(t, f)
	if r.body["title"] != "✓ done · infra / claude-1" || r.body["priority"] != float64(3) || r.body["message"] != "Applied\nclaude-code" {
		t.Fatalf("done = %s", r.raw)
	}
	window = clock.Now().Add(30 * time.Second)
	clock.waitArmed(t, window)

	// A state held at shutdown is still sent.
	bus.Publish(status(events.StateError, "", "Tests failed"))
	clock.waitArmed(t, window)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.Close(ctx); err != nil {
		t.Fatal(err)
	}
	r = nextRequest(t, f)
	if r.body["title"] != "✗ error · infra / claude-1" {
		t.Fatalf("error = %s", r.raw)
	}
	noRequest(t, f)
	if len(f.requests()) != 3 {
		t.Fatalf("requests = %d", len(f.requests()))
	}

	// Close is idempotent; the notifier no longer subscribes.
	if err := n.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(bus.Stats()) != 0 {
		t.Fatalf("subscribers left: %+v", bus.Stats())
	}
}

// Publishing on the bus never waits for ntfy, even when ntfy hangs.
func TestNotifierDoesNotBlockBus(t *testing.T) {
	f := newFakeNtfy(t)
	f.block = make(chan struct{})
	defer close(f.block)
	bus := events.New(events.WithLogger(log.New(io.Discard)))
	defer bus.Close()
	n := New(config.NotificationsConfig{
		On:   []string{"blocked", "done", "error", "working"},
		Ntfy: config.NtfyConfig{Enabled: true, URL: f.srv.URL, Topic: "agents"},
	}, WithLogger(log.New(io.Discard)), WithHTTPClient(f.srv.Client()), WithRetry(time.Hour, time.Hour))
	n.Start(bus)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 2000 {
			state := events.StateWorking
			if i%2 == 0 {
				state = events.StateBlocked
			}
			bus.Publish(events.SessionStatusEvent(1, events.SessionStatus{
				Session: events.Session{User: "alice", Repo: "r", Sandbox: "sb"},
				Record:  events.Record{State: state, Msg: string(rune('a' + i%26))},
			}))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bus blocked behind a hanging ntfy")
	}

	// Close gives up on the hanging request when its context ends.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := n.Close(ctx); err == nil {
		t.Fatal("Close returned nil with a request still hanging")
	}
}

// When the bus closes first, the notifier stops by itself.
func TestNotifierStopsWithBus(t *testing.T) {
	f := newFakeNtfy(t)
	bus := events.New(events.WithLogger(log.New(io.Discard)))
	n := New(config.NotificationsConfig{
		On:   config.DefaultTriggers,
		Ntfy: config.NtfyConfig{Enabled: true, URL: f.srv.URL, Topic: "agents"},
	}, WithLogger(log.New(io.Discard)), WithHTTPClient(f.srv.Client()))
	n.Start(bus)
	bus.Publish(status(events.StateDone, "", "ok"))
	nextRequest(t, f)
	bus.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNotifierNeverStarted(t *testing.T) {
	n := New(config.NotificationsConfig{})
	if err := n.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var nilN *Notifier
	if err := nilN.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Events published before Close are not lost, even when Close comes before
// the notifier has read them.
func TestNotifierCloseTakesPublishedEvents(t *testing.T) {
	for range 20 {
		f := newFakeNtfy(t)
		bus := events.New(events.WithLogger(log.New(io.Discard)))
		n := New(config.NotificationsConfig{
			On:       config.DefaultTriggers,
			Debounce: 30 * time.Second,
			Ntfy:     config.NtfyConfig{Enabled: true, URL: f.srv.URL, Topic: "agents"},
		}, WithLogger(log.New(io.Discard)), WithHTTPClient(f.srv.Client()))
		n.Start(bus)
		bus.Publish(status(events.StateBlocked, events.BlockAuth, "Log in"))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := n.Close(ctx); err != nil {
			t.Fatal(err)
		}
		cancel()
		bus.Close()
		if len(f.requests()) != 1 {
			t.Fatalf("requests = %d", len(f.requests()))
		}
	}
}
