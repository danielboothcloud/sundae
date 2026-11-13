package events

import (
	"sync"
	"sync/atomic"
	"time"

	"charm.land/log/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// DefaultBuffer is the number of events a subscription holds before it
// starts dropping. It covers a burst of status changes across many sessions
// while a subscriber is busy, e.g. an HTTP call to a notification service.
const DefaultBuffer = 256

var (
	publishedCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sundae",
		Subsystem: "events",
		Name:      "published_total",
		Help:      "Events published on the event bus, by kind.",
	}, []string{"kind"})

	droppedCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sundae",
		Subsystem: "events",
		Name:      "dropped_total",
		Help:      "Events a subscriber lost because its buffer was full, by subscriber name.",
	}, []string{"subscriber"})
)

// Bus fans events out to subscribers. It is safe for concurrent use. A nil
// *Bus is valid: it drops everything published and its subscriptions never
// deliver, so code that publishes does not need to check whether a bus was
// configured.
type Bus struct {
	logger *log.Logger
	now    func() time.Time
	nextID atomic.Uint64

	// mu guards subs and closed. Publish holds it for reading while it
	// delivers; Cancel and Close hold it for writing before closing a
	// channel, so a channel is never sent on after it is closed.
	mu sync.RWMutex
	// pubMu serialises publishing: an event's ID is assigned and the event
	// delivered to every subscriber before the next publish starts, so
	// each subscriber receives events in ID order, and an unfiltered
	// subscriber that sees a hole in the IDs knows it dropped events.
	// Delivery never blocks, so this costs publishers little.
	pubMu  sync.Mutex
	subs   map[*Subscription]struct{}
	closed bool
}

// Option configures a Bus.
type Option func(*Bus)

// WithLogger sets the logger the bus reports dropping subscribers to.
func WithLogger(l *log.Logger) Option {
	return func(b *Bus) { b.logger = l }
}

// WithClock sets the clock used to stamp events published without a time.
func WithClock(now func() time.Time) Option {
	return func(b *Bus) { b.now = now }
}

// New returns a running bus.
func New(opts ...Option) *Bus {
	b := &Bus{
		now:  time.Now,
		subs: map[*Subscription]struct{}{},
	}
	for _, o := range opts {
		o(b)
	}
	if b.logger == nil {
		b.logger = log.Default()
	}
	b.logger = b.logger.WithPrefix("events")
	return b
}

// Publish stamps ev with an ID, and with the current time when ev.Time is
// zero, and delivers it to every subscriber whose filter accepts it. It
// never blocks: a subscriber whose buffer is full loses the event, which is
// counted on the subscription and in sundae_events_dropped_total.
// Publishes are serialised, so every subscriber receives events in ID
// order.
//
// It returns the stamped event. Publishing on a nil or closed bus, or an
// event without a payload, delivers nothing and returns ev with ID 0.
func (b *Bus) Publish(ev Event) Event {
	if b == nil {
		return ev
	}
	pk := ev.payloadKind()
	if pk == "" {
		b.logger.Warn("dropping event without a payload", "kind", ev.Kind)
		return ev
	}
	ev.Kind = pk
	if ev.Time.IsZero() {
		ev.Time = b.now()
	}

	b.pubMu.Lock()
	defer b.pubMu.Unlock()
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return ev
	}
	ev.ID = b.nextID.Add(1)
	publishedCounter.WithLabelValues(string(ev.Kind)).Inc()
	for s := range b.subs {
		s.deliver(ev)
	}
	return ev
}

// Subscribe registers a subscriber and returns its subscription. Events
// published from now on that pass every filter are delivered on
// Subscription.C until Cancel is called or the bus is closed, which closes
// C. Subscribing to a nil or closed bus returns a subscription whose C is
// already closed.
func (b *Bus) Subscribe(opts ...SubscribeOption) *Subscription {
	so := subscribeOptions{name: "anonymous", buffer: DefaultBuffer}
	for _, o := range opts {
		o(&so)
	}
	if so.buffer < 1 {
		so.buffer = 1
	}
	ch := make(chan Event, so.buffer)
	s := &Subscription{
		C:       ch,
		ch:      ch,
		bus:     b,
		name:    so.name,
		filters: so.filters,
		dropped: droppedCounter.WithLabelValues(so.name),
	}
	if b == nil {
		close(ch)
		return s
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return s
	}
	b.subs[s] = struct{}{}
	return s
}

// Close ends every subscription and makes later publishes no-ops. It is safe
// to call more than once.
func (b *Bus) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for s := range b.subs {
		delete(b.subs, s)
		close(s.ch)
	}
}

// SubscriberStats describes one subscription.
type SubscriberStats struct {
	Name     string
	Buffered int
	Capacity int
	Dropped  uint64
}

// Stats returns the current subscriptions, for diagnostics.
func (b *Bus) Stats() []SubscriberStats {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]SubscriberStats, 0, len(b.subs))
	for s := range b.subs {
		out = append(out, SubscriberStats{
			Name:     s.name,
			Buffered: len(s.ch),
			Capacity: cap(s.ch),
			Dropped:  s.Dropped(),
		})
	}
	return out
}

// Filter decides whether a subscription receives an event. It runs on the
// publisher's goroutine while the bus is locked for reading, so it must be
// fast, must not block, and must not call back into the bus.
type Filter func(Event) bool

// SubscribeOption configures a subscription.
type SubscribeOption func(*subscribeOptions)

type subscribeOptions struct {
	name    string
	buffer  int
	filters []Filter
}

// Named names the subscription in logs and in the dropped-events metric.
// Use a fixed name per kind of subscriber ("ntfy", "tui"), not one per user,
// to keep the metric's label set small.
func Named(name string) SubscribeOption {
	return func(o *subscribeOptions) { o.name = name }
}

// Buffer sets how many events the subscription holds before it drops
// (default DefaultBuffer, at least 1).
func Buffer(n int) SubscribeOption {
	return func(o *subscribeOptions) { o.buffer = n }
}

// Where adds a filter. An event is delivered only if every filter accepts
// it.
func Where(f Filter) SubscribeOption {
	return func(o *subscribeOptions) { o.filters = append(o.filters, f) }
}

// ForUser delivers only events owned by the user with the given ID. Events
// without an owner (UserID 0) are not delivered.
func ForUser(userID int64) SubscribeOption {
	return Where(func(ev Event) bool { return ev.UserID == userID })
}

// OfKind delivers only events of the given kinds.
func OfKind(kinds ...Kind) SubscribeOption {
	return Where(func(ev Event) bool {
		for _, k := range kinds {
			if ev.Kind == k {
				return true
			}
		}
		return false
	})
}

// Subscription is one subscriber's view of the bus.
type Subscription struct {
	// C delivers the events. It is closed by Cancel and when the bus is
	// closed.
	C <-chan Event

	ch      chan Event
	bus     *Bus
	name    string
	filters []Filter

	droppedN atomic.Uint64
	dropped  prometheus.Counter
	// dropping is set while the subscriber is losing events, so a stuck
	// subscriber is logged once rather than once per event.
	dropping atomic.Bool
	cancel   sync.Once
}

// Name returns the subscription's name.
func (s *Subscription) Name() string { return s.name }

// Dropped returns how many events this subscription has lost because its
// buffer was full.
func (s *Subscription) Dropped() uint64 { return s.droppedN.Load() }

// Cancel unsubscribes and closes C. Events still buffered can be read until
// C reports closed. It is safe to call more than once and from any
// goroutine.
func (s *Subscription) Cancel() {
	s.cancel.Do(func() {
		b := s.bus
		if b == nil {
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[s]; ok {
			delete(b.subs, s)
			close(s.ch)
		}
	})
}

// deliver hands ev to the subscriber without blocking. The caller holds the
// bus's read lock.
func (s *Subscription) deliver(ev Event) {
	for _, f := range s.filters {
		if !f(ev) {
			return
		}
	}
	select {
	case s.ch <- ev:
		if s.dropping.Load() && s.dropping.CompareAndSwap(true, false) {
			s.bus.logger.Info("subscriber caught up", "subscriber", s.name, "dropped_total", s.Dropped())
		}
	default:
		s.droppedN.Add(1)
		s.dropped.Inc()
		if s.dropping.CompareAndSwap(false, true) {
			s.bus.logger.Warn("subscriber is not keeping up; dropping events", "subscriber", s.name, "buffer", cap(s.ch), "event", ev.ID, "kind", ev.Kind)
		}
	}
}
