// Package signals keeps the signals feed: every session status report and
// lifecycle event published on the event bus, in memory and capped, for
// `session signals` and the TUI's Signals pane. It also holds the status
// vocabulary those share with `session ls` (see format.go).
//
// The History lives as long as the server. It is created in server setup
// and reaches SSH sessions through the context (FromContext), like the
// session Monitor. Nothing is stored on disk: after a restart the feed is
// empty.
package signals

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/events"
)

// KindGap is the Kind of a gap marker: a Signal that stands for signals
// that were lost. Gap is set.
const KindGap events.Kind = "gap"

// Why signals were lost.
const (
	// GapDropped: the History's own bus subscription fell behind and the
	// bus dropped events. Gap.Lost counts every event lost, of any user
	// and kind (forge events included), so it is an upper bound.
	GapDropped = "dropped"
	// GapEvicted: signals newer than a reader's cursor were evicted to
	// make room before the reader saw them. Gap.Lost counts the evicted
	// signals of every user; 0 when not known.
	GapEvicted = "evicted"
)

// subscriptionBuffer is the History's bus subscription buffer. Its reader
// only appends to memory, so it is rarely more than a few events behind.
const subscriptionBuffer = 4 * events.DefaultBuffer

// ErrClosed is returned by Feed.Next once the History is closed: the
// server is shutting down.
var ErrClosed = errors.New("signals feed closed")

// Gap describes lost signals.
type Gap struct {
	// Lost is how many events were lost; 0 when not known.
	Lost uint64 `json:"lost,omitempty"`
	// Reason is GapDropped or GapEvicted.
	Reason string `json:"reason"`
}

// Signal is one entry of the feed: a session status or lifecycle event as
// published, or a gap marker. Payloads are shared with other subscribers:
// treat them as read-only.
type Signal struct {
	events.Event
	// Gap is set for gap markers (Kind KindGap). They belong to no user and
	// are shown to every reader whatever the query.
	Gap *Gap `json:"gap,omitempty"`
	// Last is, for a lifecycle event, the session's root state as last
	// known when the event arrived: from the session's own status events,
	// else from the Monitor. StateNone when unknown.
	Last events.State `json:"last,omitempty"`
}

// IsGap reports whether s is a gap marker.
func (s Signal) IsGap() bool { return s.Gap != nil }

// session returns the session the signal is about.
func (s Signal) session() (events.Session, bool) {
	switch {
	case s.Status != nil:
		return s.Status.Session, true
	case s.Lifecycle != nil:
		return s.Lifecycle.Session, true
	}
	return events.Session{}, false
}

// Query selects signals. The zero Query (but for UserID) selects all of a
// user's signals.
type Query struct {
	// UserID is the owner of the sessions. Signals of other users are
	// never returned; gap markers always are.
	UserID int64
	// Repo, when set, selects the sessions of that repository.
	Repo string
	// Session, when set, selects sessions of that name.
	Session string
	// Attention selects the signals that ask for the user (see
	// Signal.NeedsAttention).
	Attention bool
	// Text, when set, selects signals whose repository, session, record
	// id, state, kind, step, app, title, message or reason contains it,
	// ignoring case.
	Text string
	// After is a cursor: only signals with an ID greater than After are
	// selected. 0 selects from the oldest kept.
	After uint64
	// Limit caps a snapshot to the newest Limit signals; 0 means no cap.
	// The live part of a Follow is not capped.
	Limit int
}

// Match reports whether the query selects s, cursor and limit aside.
func (q Query) Match(s Signal) bool {
	if s.IsGap() {
		return true
	}
	if s.UserID != q.UserID {
		return false
	}
	sess, ok := s.session()
	if !ok {
		return false
	}
	if q.Repo != "" && sess.Repo != q.Repo {
		return false
	}
	if q.Session != "" && sess.DisplayName() != q.Session {
		return false
	}
	if q.Attention && !s.NeedsAttention() {
		return false
	}
	if q.Text != "" && !s.contains(strings.ToLower(q.Text)) {
		return false
	}
	return true
}

// contains reports whether any text field of s contains lower, a
// lower-case string.
func (s Signal) contains(lower string) bool {
	sess, _ := s.session()
	fields := []string{sess.Repo, sess.DisplayName(), s.State().Word()}
	if st := s.Status; st != nil {
		r := st.Record
		fields = append(fields, r.ID, string(r.Kind), r.App, r.Title, r.Msg)
	}
	if l := s.Lifecycle; l != nil {
		fields = append(fields, string(l.Step), l.Op, l.Reason)
	}
	for _, f := range fields {
		if f != "" && strings.Contains(strings.ToLower(f), lower) {
			return true
		}
	}
	return false
}

// Option configures a History.
type Option func(*History)

// WithLogger sets the logger.
func WithLogger(l *log.Logger) Option {
	return func(h *History) { h.logger = l }
}

// WithStateLookup sets where a lifecycle event's Last state comes from
// when the History has not seen a status event of the session: the
// Monitor's last read, which covers sessions whose status predates the
// server's start. ok is false when the state is not known.
func WithStateLookup(lookup func(sandbox string) (state events.State, ok bool)) Option {
	return func(h *History) { h.lookup = lookup }
}

// History is the signals feed: the last signals published on the bus, in
// memory. It is safe for concurrent use. A nil *History is valid: it holds
// nothing and its feeds are closed.
type History struct {
	keep   int
	logger *log.Logger
	lookup func(string) (events.State, bool)

	// mu guards everything below.
	mu sync.Mutex
	// buf is a ring of up to keep signals in ID order; head is the index
	// of the oldest and n the number held.
	buf  []Signal
	head int
	n    int
	// seq counts the signals ever appended; the signal at ring position i
	// (0 is the oldest) has sequence number seq-n+i+1. Feeds read by
	// sequence number.
	seq uint64
	// lastID is the ID of the last bus event received, of any kind, or
	// accounted for by a gap marker.
	lastID uint64
	// dropped is how many events the gap markers account for.
	dropped uint64
	// lastEvicted is the ID of the newest signal evicted.
	lastEvicted uint64
	// state is the last root state seen per session.
	state map[sessionKey]events.State
	// wake is closed, and replaced, whenever a signal is appended and when
	// the History closes.
	wake   chan struct{}
	closed bool

	sub       *events.Subscription
	done      chan struct{}
	startOnce sync.Once
}

// New returns a History keeping the last keep signals (at least 1). It
// records nothing until Start.
func New(keep int, opts ...Option) *History {
	h := &History{
		keep:  max(keep, 1),
		state: map[sessionKey]events.State{},
		wake:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	for _, o := range opts {
		o(h)
	}
	if h.logger == nil {
		h.logger = log.Default()
	}
	h.logger = h.logger.WithPrefix("signals")
	h.buf = make([]Signal, 0, min(h.keep, 64))
	return h
}

// Keep returns how many signals the History keeps.
func (h *History) Keep() int {
	if h == nil {
		return 0
	}
	return h.keep
}

// Start subscribes to bus and records its session status and lifecycle
// events until Close, or until the bus is closed. Calling it again does
// nothing.
func (h *History) Start(bus *events.Bus) {
	if h == nil {
		return
	}
	h.startOnce.Do(func() {
		h.subscribe(bus)
		go h.run()
	})
}

// subscribe subscribes to every event: forge events are skipped by observe
// rather than filtered by the bus, so that a hole in the IDs received
// means the bus dropped events for this subscription.
func (h *History) subscribe(bus *events.Bus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sub = bus.Subscribe(events.Named("signals"), events.Buffer(subscriptionBuffer))
}

// run records events until the subscription ends, then closes the
// History. Whenever it has caught up, it checks for events dropped since
// the last one it received: a hole in the IDs only shows once the next
// event arrives.
func (h *History) run() {
	defer close(h.done)
	for {
		var (
			ev events.Event
			ok bool
		)
		select {
		case ev, ok = <-h.sub.C:
		default:
			h.checkDropped()
			ev, ok = <-h.sub.C
		}
		if !ok {
			break
		}
		h.observe(ev)
	}
	h.markClosed()
}

// Close stops recording and ends every Feed with ErrClosed; snapshots
// still read what was kept. It is safe to call more than once.
func (h *History) Close() {
	if h == nil {
		return
	}
	h.startOnce.Do(func() { close(h.done) }) // never started
	h.mu.Lock()
	sub := h.sub
	h.mu.Unlock()
	if sub != nil {
		sub.Cancel()
	}
	<-h.done
	h.markClosed()
}

func (h *History) markClosed() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		close(h.wake)
	}
}

// observe records one bus event. It runs on the History's goroutine only.
func (h *History) observe(ev events.Event) {
	// Other kinds (forge) are not signals, but their IDs still count.
	var sig Signal
	switch ev.Kind {
	case events.KindSessionStatus, events.KindSessionLifecycle:
		sig = Signal{Event: ev}
	}
	if l := ev.Lifecycle; l != nil && sig.Kind == events.KindSessionLifecycle {
		sig.Last = h.lastState(sessionKey{ev.UserID, l.Session.Sandbox})
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	if h.lastID != 0 && ev.ID > h.lastID+1 {
		h.addDropped(ev.ID-h.lastID-1, ev.ID-1, ev.Time)
	}
	if ev.ID > h.lastID {
		h.lastID = ev.ID
	}
	if sig.Kind == "" {
		h.broadcast() // a gap marker may have been added
		return
	}

	switch {
	case ev.Status != nil && ev.Status.Record.ID == "":
		h.state[sessionKey{ev.UserID, ev.Status.Session.Sandbox}] = ev.Status.Record.State
	case ev.Lifecycle != nil && ev.Lifecycle.Step == events.LifecycleEnded:
		delete(h.state, sessionKey{ev.UserID, ev.Lifecycle.Session.Sandbox})
	}
	h.append(sig)
	h.broadcast()
}

// checkDropped adds a gap marker for events the bus dropped after the
// last one received. Called when the subscription's buffer is empty: every
// event delivered has been read, so the dropped events not accounted for
// yet are the ones right after lastID.
func (h *History) checkDropped() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	if d := h.sub.Dropped(); d > h.dropped {
		lost := d - h.dropped
		h.addDropped(lost, h.lastID+lost, time.Now())
		h.lastID += lost
		h.broadcast()
	}
}

// addDropped appends a GapDropped marker for lost events, the newest of
// which had ID newest. Call with mu held.
func (h *History) addDropped(lost, newest uint64, at time.Time) {
	h.dropped += lost
	h.logger.Warn("signals feed fell behind; events were lost", "lost", lost, "through", newest)
	h.append(Signal{
		Event: events.Event{ID: newest, Time: at, Kind: KindGap},
		Gap:   &Gap{Lost: lost, Reason: GapDropped},
	})
}

// sessionKey identifies a session: its owner and sandbox.
type sessionKey struct {
	userID  int64
	sandbox string
}

// lastState returns the last root state known for a session.
func (h *History) lastState(k sessionKey) events.State {
	h.mu.Lock()
	st, ok := h.state[k]
	h.mu.Unlock()
	if ok {
		return st
	}
	if h.lookup != nil {
		if st, ok := h.lookup(k.sandbox); ok {
			return st
		}
	}
	return events.StateNone
}

// append adds a signal, evicting the oldest when full. Call with mu held.
func (h *History) append(s Signal) {
	if h.n < h.keep {
		// Not full yet: the ring grows, and nothing was evicted, so
		// head is 0 and n is len(buf).
		h.buf = append(h.buf, s)
		h.n++
	} else {
		h.lastEvicted = h.buf[h.head].ID
		h.buf[h.head] = s
		h.head = (h.head + 1) % len(h.buf)
	}
	h.seq++
}

// broadcast wakes the feeds. Call with mu held.
func (h *History) broadcast() {
	close(h.wake)
	h.wake = make(chan struct{})
}

// at returns the signal at ring position i, 0 being the oldest. Call with
// mu held.
func (h *History) at(i int) Signal {
	return h.buf[(h.head+i)%len(h.buf)]
}

// Len returns how many signals are kept, of every user, gap markers
// included.
func (h *History) Len() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// Snapshot returns the signals q selects, newest first. When q.After is
// set and signals newer than it were evicted, a GapEvicted marker is the
// last (oldest) entry, unless the limit was reached first.
func (h *History) Snapshot(q Query) []Signal {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshot(q)
}

// snapshot is Snapshot with mu held.
func (h *History) snapshot(q Query) []Signal {
	var out []Signal
	for i := h.n - 1; i >= 0; i-- {
		if q.Limit > 0 && len(out) == q.Limit {
			return out
		}
		s := h.at(i)
		if s.ID <= q.After {
			return out
		}
		if q.Match(s) {
			out = append(out, s)
		}
	}
	if q.After > 0 && h.lastEvicted > q.After && (q.Limit == 0 || len(out) < q.Limit) {
		out = append(out, Signal{
			Event: events.Event{ID: h.lastEvicted, Kind: KindGap},
			Gap:   &Gap{Reason: GapEvicted},
		})
	}
	return out
}

// Follow returns a snapshot of the signals q selects, newest first, and a
// Feed of the signals q selects that are recorded after it. The snapshot
// and the feed's start are taken together, so the feed neither repeats a
// signal of the snapshot nor misses one recorded meanwhile.
func (h *History) Follow(q Query) ([]Signal, *Feed) {
	f := &Feed{h: h, q: q}
	if h == nil {
		return nil, f
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	f.pos = h.seq
	return h.snapshot(q), f
}

// Feed delivers the signals recorded after a Follow. It holds no resources
// on the History: dropping it is enough. It is not safe for concurrent use.
type Feed struct {
	h   *History
	q   Query
	pos uint64
}

// Next waits for signals the Feed's query selects and returns them, oldest
// first. When the reader fell so far behind that signals were evicted
// before it read them, the first entry is a GapEvicted marker. Next
// returns ctx's error when ctx is done, and ErrClosed once the History is
// closed and every signal recorded before has been returned.
func (f *Feed) Next(ctx context.Context) ([]Signal, error) {
	h := f.h
	if h == nil {
		return nil, ErrClosed
	}
	for {
		h.mu.Lock()
		out := f.read()
		closed, wake := h.closed, h.wake
		h.mu.Unlock()
		if len(out) > 0 {
			return out, nil
		}
		if closed {
			return nil, ErrClosed
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return nil, ctx.Err() //nolint:wrapcheck
		}
	}
}

// read returns the signals recorded since the last read that the query
// selects, oldest first, and moves past them. Call with mu held.
func (f *Feed) read() []Signal {
	h := f.h
	if f.pos >= h.seq {
		return nil
	}
	var out []Signal
	oldest := h.seq - uint64(h.n) // sequence number before the oldest kept
	if f.pos < oldest {
		var id uint64
		if h.n > 0 {
			id = h.at(0).ID - 1
		}
		out = append(out, Signal{
			Event: events.Event{ID: id, Kind: KindGap},
			Gap:   &Gap{Lost: oldest - f.pos, Reason: GapEvicted},
		})
		f.pos = oldest
	}
	for i := int(f.pos - oldest); i < h.n; i++ {
		if s := h.at(i); f.q.Match(s) {
			out = append(out, s)
		}
	}
	f.pos = h.seq
	return out
}

// ContextKey is the key for the History in a context.
var ContextKey = &struct{ string }{"signals"}

// FromContext returns the History from a context, or nil when there is
// none. A nil *History is safe to use.
func FromContext(ctx context.Context) *History {
	if h, ok := ctx.Value(ContextKey).(*History); ok {
		return h
	}
	return nil
}

// WithContext returns a new context with the History attached.
func WithContext(ctx context.Context, h *History) context.Context {
	return context.WithValue(ctx, ContextKey, h)
}
