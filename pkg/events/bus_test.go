package events

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func newTestBus(opts ...Option) *Bus {
	return New(append([]Option{WithLogger(log.New(io.Discard))}, opts...)...)
}

func statusEvent(user int64, state State) Event {
	return SessionStatusEvent(user, SessionStatus{
		Session: Session{User: "alice", Repo: "r", Sandbox: "soft-alice-r"},
		Record:  Record{State: state},
	})
}

// recv returns the next event on s, failing the test when none arrives.
func recv(t *testing.T, s *Subscription) Event {
	t.Helper()
	select {
	case ev, ok := <-s.C:
		if !ok {
			t.Fatal("subscription closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event delivered")
	}
	return Event{}
}

func TestPublishStampsAndFansOut(t *testing.T) {
	at := time.Date(2026, 11, 12, 14, 2, 11, 0, time.UTC)
	b := newTestBus(WithClock(func() time.Time { return at }))
	defer b.Close()

	a := b.Subscribe(Named("a"))
	c := b.Subscribe(Named("c"))

	got := b.Publish(statusEvent(1, StateBlocked))
	if got.ID != 1 || !got.Time.Equal(at) || got.Kind != KindSessionStatus {
		t.Fatalf("stamped = %+v", got)
	}
	for _, s := range []*Subscription{a, c} {
		ev := recv(t, s)
		if ev.ID != 1 || ev.Status.Record.State != StateBlocked || ev.UserID != 1 {
			t.Fatalf("%s got %+v", s.Name(), ev)
		}
	}

	// A caller-supplied time is kept; IDs keep increasing.
	when := at.Add(-time.Hour)
	ev := ForgeEvent(0, ForgeOutcome{Kind: "sync", Forge: "github", OK: true})
	ev.Time = when
	got = b.Publish(ev)
	if got.ID != 2 || !got.Time.Equal(when) || got.Kind != KindForge {
		t.Fatalf("second = %+v", got)
	}
	if ev := recv(t, a); ev.ID != 2 {
		t.Fatalf("a got %+v", ev)
	}
}

func TestPublishDerivesKindAndRejectsEmpty(t *testing.T) {
	b := newTestBus()
	defer b.Close()
	s := b.Subscribe()

	// The kind always follows the payload.
	got := b.Publish(Event{Kind: KindForge, Lifecycle: &SessionLifecycle{Step: LifecycleReady}})
	if got.Kind != KindSessionLifecycle {
		t.Fatalf("kind = %q", got.Kind)
	}
	if ev := recv(t, s); ev.Kind != KindSessionLifecycle {
		t.Fatalf("delivered %+v", ev)
	}

	// No payload: nothing delivered, no ID used.
	if got := b.Publish(Event{Kind: KindForge}); got.ID != 0 {
		t.Fatalf("empty event got ID %d", got.ID)
	}
	if got := b.Publish(statusEvent(1, StateDone)); got.ID != 2 {
		t.Fatalf("next ID = %d, want 2", got.ID)
	}
}

func TestFilters(t *testing.T) {
	b := newTestBus()
	defer b.Close()
	alice := b.Subscribe(ForUser(1))
	statusOnly := b.Subscribe(OfKind(KindSessionStatus))
	both := b.Subscribe(ForUser(1), OfKind(KindForge))

	b.Publish(statusEvent(2, StateDone))                           // 1: bob's status
	b.Publish(ForgeEvent(0, ForgeOutcome{Kind: "sync", OK: true})) // 2: no owner
	b.Publish(ForgeEvent(1, ForgeOutcome{Kind: "push", OK: true})) // 3: alice's push
	b.Publish(statusEvent(1, StateBlocked))                        // 4: alice's status
	b.Close()

	ids := func(s *Subscription) []uint64 {
		var out []uint64
		for ev := range s.C {
			out = append(out, ev.ID)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		s    *Subscription
		want []uint64
	}{
		{"ForUser", alice, []uint64{3, 4}},
		{"OfKind", statusOnly, []uint64{1, 4}},
		{"ForUser+OfKind", both, []uint64{3}},
	} {
		got := ids(tc.s)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		}
	}
}

// A subscriber that never reads must not hold up the publisher or the other
// subscribers; what it loses is counted.
func TestStuckSubscriberDoesNotBlockPublish(t *testing.T) {
	b := newTestBus()
	defer b.Close()
	stuck := b.Subscribe(Named("test-stuck"), Buffer(2))
	live := b.Subscribe(Named("test-live"), Buffer(100))
	before := scrape(t, `soft_serve_events_dropped_total{subscriber="test-stuck"}`)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 10 {
			b.Publish(statusEvent(1, StateWorking))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a stuck subscriber")
	}

	if got := stuck.Dropped(); got != 8 {
		t.Fatalf("stuck dropped = %d, want 8", got)
	}
	if got := scrape(t, `soft_serve_events_dropped_total{subscriber="test-stuck"}`) - before; got != 8 {
		t.Fatalf("dropped metric delta = %v, want 8", got)
	}
	if live.Dropped() != 0 || len(live.C) != 10 {
		t.Fatalf("live: dropped=%d buffered=%d", live.Dropped(), len(live.C))
	}
	// The stuck subscriber kept the oldest events it had room for.
	if ev := recv(t, stuck); ev.ID != 1 {
		t.Fatalf("stuck first = %d", ev.ID)
	}

	var stats SubscriberStats
	for _, st := range b.Stats() {
		if st.Name == "test-stuck" {
			stats = st
		}
	}
	if stats.Dropped != 8 || stats.Capacity != 2 || stats.Buffered != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	// Once it has room again it receives new events.
	recv(t, stuck)
	b.Publish(statusEvent(1, StateDone))
	if ev := recv(t, stuck); ev.Status.Record.State != StateDone {
		t.Fatalf("after catching up got %+v", ev)
	}
}

func TestCancel(t *testing.T) {
	b := newTestBus()
	defer b.Close()
	s := b.Subscribe()
	other := b.Subscribe()

	b.Publish(statusEvent(1, StateWorking))
	s.Cancel()
	s.Cancel() // idempotent
	b.Publish(statusEvent(1, StateDone))

	// What was buffered before Cancel can still be read, then C is closed.
	if ev, ok := <-s.C; !ok || ev.ID != 1 {
		t.Fatalf("buffered event: %+v ok=%v", ev, ok)
	}
	if _, ok := <-s.C; ok {
		t.Fatal("C not closed after Cancel")
	}
	if len(b.Stats()) != 1 {
		t.Fatalf("subscribers after cancel = %d", len(b.Stats()))
	}
	if ev := recv(t, other); ev.ID != 1 {
		t.Fatal("other subscriber affected")
	}
	if ev := recv(t, other); ev.ID != 2 {
		t.Fatal("other subscriber affected")
	}
}

func TestCloseEndsSubscriptions(t *testing.T) {
	b := newTestBus()
	s := b.Subscribe()
	b.Close()
	b.Close() // idempotent
	if _, ok := <-s.C; ok {
		t.Fatal("C not closed by Close")
	}
	s.Cancel() // after Close: no panic

	if got := b.Publish(statusEvent(1, StateDone)); got.ID != 0 {
		t.Fatal("publish after close delivered")
	}
	late := b.Subscribe()
	if _, ok := <-late.C; ok {
		t.Fatal("subscription to a closed bus is open")
	}
}

func TestNilBus(t *testing.T) {
	var b *Bus
	ev := b.Publish(statusEvent(1, StateDone))
	if ev.ID != 0 {
		t.Fatal("nil bus stamped an event")
	}
	s := b.Subscribe()
	if _, ok := <-s.C; ok {
		t.Fatal("nil bus subscription is open")
	}
	s.Cancel()
	b.Close()
	if b.Stats() != nil {
		t.Fatal("nil bus has stats")
	}
	if FromContext(context.Background()) != nil {
		t.Fatal("empty context has a bus")
	}
}

func TestContext(t *testing.T) {
	b := newTestBus()
	defer b.Close()
	if FromContext(WithContext(context.Background(), b)) != b {
		t.Fatal("bus not found in context")
	}
}

// Publish, Subscribe and Cancel race freely; run with -race.
func TestConcurrentUse(t *testing.T) {
	b := newTestBus()
	reader := b.Subscribe(Buffer(1000))
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				b.Publish(statusEvent(1, StateWorking))
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				s := b.Subscribe(Buffer(4))
				select {
				case <-s.C:
				default:
				}
				s.Cancel()
			}
		}()
	}
	wg.Wait()
	b.Close()
	var last uint64
	n := 0
	for ev := range reader.C {
		if ev.ID == 0 || ev.ID == last {
			t.Fatalf("bad id %d after %d", ev.ID, last)
		}
		last = ev.ID
		n++
	}
	if n+int(reader.Dropped()) != 800 {
		t.Fatalf("delivered %d + dropped %d != 800", n, reader.Dropped())
	}
}

// scrape returns the value of a metric series from the default registry, as
// the stats server exposes it.
func scrape(t *testing.T, series string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + ` (\S+)$`).FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("series %s not exposed", series)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatal(fmt.Errorf("parse %s: %w", series, err))
	}
	return v
}
