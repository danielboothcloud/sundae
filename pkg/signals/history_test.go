package signals

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/events"
)

func newTestHistory(keep int, opts ...Option) *History {
	return New(keep, append([]Option{WithLogger(log.New(io.Discard))}, opts...)...)
}

func sess(repo, name string) events.Session {
	return events.Session{User: "alice", Repo: repo, Name: name, Sandbox: "soft-alice-" + repo + "-" + name}
}

func status(user int64, s events.Session, state events.State, msg string) events.Event {
	return events.SessionStatusEvent(user, events.SessionStatus{Session: s, Record: events.Record{State: state, Msg: msg}})
}

func lifecycle(user int64, s events.Session, step events.Lifecycle) events.Event {
	return events.SessionLifecycleEvent(user, events.SessionLifecycle{Session: s, Step: step})
}

// feed records events on h as the bus would deliver them, numbering them
// from the last ID h received.
func feed(h *History, evs ...events.Event) {
	for _, ev := range evs {
		h.mu.Lock()
		ev.ID = h.lastID + 1
		h.mu.Unlock()
		if ev.Time.IsZero() {
			ev.Time = time.Unix(int64(ev.ID), 0)
		}
		h.observe(ev)
	}
}

// waitFor waits until cond holds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func ids(sigs []Signal) []uint64 {
	out := make([]uint64, len(sigs))
	for i, s := range sigs {
		out[i] = s.ID
	}
	return out
}

func TestCapacityAndEviction(t *testing.T) {
	h := newTestHistory(3)
	a := sess("r", "main")
	for range 5 {
		feed(h, status(1, a, events.StateWorking, ""))
	}
	if h.Len() != 3 {
		t.Fatalf("len = %d, want 3", h.Len())
	}
	if got := ids(h.Snapshot(Query{UserID: 1})); !slices.Equal(got, []uint64{5, 4, 3}) {
		t.Fatalf("snapshot = %v, want newest first 5 4 3", got)
	}
	// Forge events are not signals but their IDs count.
	feed(h, events.ForgeEvent(1, events.ForgeOutcome{Kind: "sync", Forge: "gh", OK: true}))
	feed(h, status(1, a, events.StateDone, ""))
	if got := ids(h.Snapshot(Query{UserID: 1})); !slices.Equal(got, []uint64{7, 5, 4}) {
		t.Fatalf("snapshot = %v, want 7 5 4", got)
	}
	if New(0).Keep() != 1 {
		t.Fatal("keep is at least 1")
	}
}

func TestPerUserIsolation(t *testing.T) {
	h := newTestHistory(10)
	a := sess("r", "main")
	feed(h,
		status(1, a, events.StateWorking, "alice"),
		status(2, a, events.StateBlocked, "bob"),
		lifecycle(2, a, events.LifecycleCreated),
		status(0, a, events.StateDone, "nobody"),
	)
	if got := ids(h.Snapshot(Query{UserID: 1})); !slices.Equal(got, []uint64{1}) {
		t.Fatalf("user 1 sees %v", got)
	}
	if got := ids(h.Snapshot(Query{UserID: 2})); !slices.Equal(got, []uint64{3, 2}) {
		t.Fatalf("user 2 sees %v", got)
	}
	if got := ids(h.Snapshot(Query{UserID: 3})); len(got) != 0 {
		t.Fatalf("user 3 sees %v", got)
	}
	_, f := h.Follow(Query{UserID: 1})
	feed(h, status(2, a, events.StateError, "bob again"), status(1, a, events.StateDone, "alice again"))
	got, err := f.Next(context.Background())
	if err != nil || !slices.Equal(ids(got), []uint64{6}) {
		t.Fatalf("user 1 feed = %v, %v", ids(got), err)
	}
}

func TestFilters(t *testing.T) {
	h := newTestHistory(20)
	infra, review, docs := sess("infra", "main"), sess("infra", "review"), sess("docs", "main")
	child := events.SessionStatusEvent(1, events.SessionStatus{Session: review, Record: events.Record{ID: "build/lint", State: events.StateError, App: "golangci-lint"}})
	failed := events.SessionLifecycleEvent(1, events.SessionLifecycle{Session: docs, Step: events.LifecycleFailed, Op: "start", Reason: "image pull failed"})
	feed(h,
		status(1, infra, events.StateWorking, "Planning"),          // 1
		status(1, review, events.StateBlocked, "Apply 3 to add?"),  // 2
		status(1, docs, events.StateDone, "Implemented discovery"), // 3
		child, // 4
		lifecycle(1, infra, events.LifecycleParked), // 5
		failed,                                 // 6
		status(1, infra, events.StateIdle, ""), // 7
	)
	cases := map[string]struct {
		q    Query
		want []uint64
	}{
		"all":            {Query{}, []uint64{7, 6, 5, 4, 3, 2, 1}},
		"repo":           {Query{Repo: "infra"}, []uint64{7, 5, 4, 2, 1}},
		"session":        {Query{Session: "review"}, []uint64{4, 2}},
		"repo+session":   {Query{Repo: "docs", Session: "main"}, []uint64{6, 3}},
		"attention":      {Query{Attention: true}, []uint64{6, 4, 3, 2}},
		"text msg":       {Query{Text: "APPLY"}, []uint64{2}},
		"text app":       {Query{Text: "lint"}, []uint64{4}},
		"text reason":    {Query{Text: "image pull"}, []uint64{6}},
		"text state":     {Query{Text: "parked"}, []uint64{5}},
		"text repo":      {Query{Text: "doc"}, []uint64{6, 3}},
		"limit":          {Query{Limit: 2}, []uint64{7, 6}},
		"limit filtered": {Query{Attention: true, Limit: 3}, []uint64{6, 4, 3}},
		"after":          {Query{After: 4}, []uint64{7, 6, 5}},
		"after newest":   {Query{After: 7}, nil},
		"after+filter":   {Query{After: 2, Repo: "infra"}, []uint64{7, 5, 4}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tc.q.UserID = 1
			if got := ids(h.Snapshot(tc.q)); !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCursorOlderThanKept(t *testing.T) {
	h := newTestHistory(3)
	a := sess("r", "main")
	for range 6 {
		feed(h, status(1, a, events.StateWorking, ""))
	}
	// Signals 2 and 3 are gone: the reader learns it missed some.
	got := h.Snapshot(Query{UserID: 1, After: 1})
	if !slices.Equal(ids(got), []uint64{6, 5, 4, 3}) || !got[3].IsGap() || got[3].Gap.Reason != GapEvicted {
		t.Fatalf("snapshot = %+v", got)
	}
	// A cursor at or past the oldest kept has nothing missing.
	if got := h.Snapshot(Query{UserID: 1, After: 3}); !slices.Equal(ids(got), []uint64{6, 5, 4}) {
		t.Fatalf("snapshot = %v", ids(got))
	}
}

// When the bus drops events for the History, a gap marker takes their
// place, shown to every user and through every filter.
func TestDroppedEventsLeaveAGapMarker(t *testing.T) {
	bus := events.New(events.WithLogger(log.New(io.Discard)))
	defer bus.Close()
	h := newTestHistory(2 * subscriptionBuffer)
	h.subscribe(bus)
	// Fill the subscription before the History reads it.
	a := sess("r", "main")
	total := subscriptionBuffer + 5
	for range total {
		bus.Publish(status(1, a, events.StateWorking, ""))
	}
	if h.sub.Dropped() != 5 {
		t.Fatalf("dropped = %d", h.sub.Dropped())
	}
	h.startOnce.Do(func() { go h.run() })
	defer h.Close()

	// Once the History has caught up, the loss shows without waiting for
	// another event, to every user and through every filter.
	snap, f := h.Follow(Query{UserID: 2, Repo: "other", Attention: true})
	if len(snap) != 0 && !(len(snap) == 1 && snap[0].IsGap()) {
		t.Fatalf("nothing published for user 2: %v", ids(snap))
	}
	if len(snap) == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		got, err := f.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		snap = got
	}
	if len(snap) != 1 || !snap[0].IsGap() {
		t.Fatalf("user 2 got %+v", snap)
	}
	g := snap[0]
	if g.Gap.Reason != GapDropped || g.Gap.Lost != 5 || g.ID != uint64(total) {
		t.Fatalf("gap = %+v %+v, want 5 lost through %d", g.Event, *g.Gap, total)
	}
	last := bus.Publish(status(1, a, events.StateDone, "after the gap"))
	waitFor(t, func() bool { return h.Len() == total-5+2 })
	if g.Label() != "! missed" || g.Message(0) == "" || !g.NeedsAttention() {
		t.Fatalf("gap renders as %q %q", g.Label(), g.Message(0))
	}

	// The owner sees the gap in its place.
	all := h.Snapshot(Query{UserID: 1, Limit: 3})
	if !slices.Equal(ids(all), []uint64{last.ID, last.ID - 1, uint64(subscriptionBuffer)}) || !all[1].IsGap() {
		t.Fatalf("snapshot = %v", ids(all))
	}
}

// A hole in the IDs received is a gap, in its place.
func TestHoleInIDsIsAGap(t *testing.T) {
	h := newTestHistory(10)
	a := sess("r", "main")
	feed(h, status(1, a, events.StateWorking, ""))
	ev := status(1, a, events.StateDone, "")
	ev.ID = 5 // the bus dropped 2, 3 and 4
	h.observe(ev)
	got := h.Snapshot(Query{UserID: 1})
	if !slices.Equal(ids(got), []uint64{5, 4, 1}) || !got[1].IsGap() || got[1].Gap.Lost != 3 || got[1].Gap.Reason != GapDropped {
		t.Fatalf("snapshot = %v %+v", ids(got), got)
	}
}

// A reader that falls further behind than the History keeps gets a gap
// marker, then what is still kept.
func TestFeedFallsBehind(t *testing.T) {
	h := newTestHistory(3)
	a := sess("r", "main")
	_, f := h.Follow(Query{UserID: 1})
	for range 10 {
		feed(h, status(1, a, events.StateWorking, ""))
	}
	got, err := f.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got), []uint64{7, 8, 9, 10}) || !got[0].IsGap() || got[0].Gap.Lost != 7 || got[0].Gap.Reason != GapEvicted {
		t.Fatalf("got %v %+v", ids(got), got[0])
	}
}

func TestFeedEnds(t *testing.T) {
	h := newTestHistory(3)
	_, f := h.Follow(Query{UserID: 1})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.Next(ctx)
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}

	go func() {
		_, err := f.Next(context.Background())
		done <- err
	}()
	h.Close()
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("closed: %v", err)
	}
	h.Close() // twice is fine

	var nilH *History
	if nilH.Snapshot(Query{}) != nil || nilH.Len() != 0 {
		t.Fatal("nil history holds nothing")
	}
	if _, f := nilH.Follow(Query{}); f != nil {
		if _, err := f.Next(context.Background()); !errors.Is(err, ErrClosed) {
			t.Fatalf("nil feed: %v", err)
		}
	}
	nilH.Start(nil)
	nilH.Close()
}

// The bus closing (server shutdown) ends feeds after what was kept.
func TestBusCloseEndsFeeds(t *testing.T) {
	bus := events.New(events.WithLogger(log.New(io.Discard)))
	h := newTestHistory(10)
	h.Start(bus)
	_, f := h.Follow(Query{UserID: 1})
	bus.Publish(status(1, sess("r", "main"), events.StateDone, ""))
	bus.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := f.Next(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", ids(got), err)
	}
	if _, err := f.Next(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("after close: %v", err)
	}
}

// Followers that start while events are published concurrently see every
// event of their user exactly once, in order: the snapshot and the live
// part neither overlap nor leave a hole.
func TestFollowSnapshotThenLiveUnderConcurrentPublishes(t *testing.T) {
	bus := events.New(events.WithLogger(log.New(io.Discard)))
	defer bus.Close()
	const users, publishers, each = 2, 4, 300
	h := newTestHistory(users * publishers * each)
	h.Start(bus)
	defer h.Close()

	var mu sync.Mutex
	published := map[int64][]uint64{}

	var pubs sync.WaitGroup
	start := make(chan struct{})
	for p := range publishers {
		pubs.Add(1)
		go func() {
			defer pubs.Done()
			<-start
			for i := range each {
				user := int64(1 + (p+i)%users)
				ev := bus.Publish(status(user, sess("r", "main"), events.StateWorking, ""))
				mu.Lock()
				published[user] = append(published[user], ev.ID)
				mu.Unlock()
				if i%50 == 0 {
					// Interleave with the followers.
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}

	type result struct {
		user int64
		seen []uint64
		gaps int
		err  error
	}
	results := make(chan result, 8)
	var followers sync.WaitGroup
	for i := range 8 {
		followers.Add(1)
		go func() {
			defer followers.Done()
			user := int64(1 + i%users)
			<-start
			time.Sleep(time.Duration(i) * 2 * time.Millisecond)
			snap, f := h.Follow(Query{UserID: user})
			r := result{user: user}
			for j := len(snap) - 1; j >= 0; j-- {
				r.seen = append(r.seen, snap[j].ID)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for len(r.seen)+r.gaps < publishers*each/users {
				got, err := f.Next(ctx)
				if err != nil {
					r.err = err
					break
				}
				for _, s := range got {
					if s.IsGap() {
						r.gaps++
						continue
					}
					r.seen = append(r.seen, s.ID)
				}
			}
			results <- r
		}()
	}
	close(start)
	pubs.Wait()
	followers.Wait()
	close(results)

	for r := range results {
		if r.err != nil {
			t.Fatalf("user %d follower: %v after %d", r.user, r.err, len(r.seen))
		}
		if r.gaps != 0 {
			t.Fatalf("user %d follower saw %d gaps", r.user, r.gaps)
		}
		want := slices.Clone(published[r.user])
		slices.Sort(want)
		if !slices.Equal(r.seen, want) {
			t.Fatalf("user %d follower saw %d signals, want %d (in order, once each)", r.user, len(r.seen), len(want))
		}
	}
}

func TestLastStateOfLifecycleSignals(t *testing.T) {
	a, b := sess("r", "main"), sess("r", "other")
	lookup := map[string]events.State{b.Sandbox: events.StateBlocked}
	h := newTestHistory(10, WithStateLookup(func(sb string) (events.State, bool) {
		st, ok := lookup[sb]
		return st, ok
	}))
	feed(h,
		lifecycle(1, a, events.LifecycleCreated),
		status(1, a, events.StateIdle, ""),
		lifecycle(1, a, events.LifecycleParked), // 3: idle, from the feed
		lifecycle(1, b, events.LifecycleParked), // 4: blocked, from the lookup
		events.SessionStatusEvent(1, events.SessionStatus{Session: a, Record: events.Record{ID: "child", State: events.StateError}}),
		lifecycle(1, a, events.LifecycleResumed), // 6: a child record does not change the root state
		lifecycle(1, a, events.LifecycleEnded),
		lifecycle(1, a, events.LifecycleCreated),
		lifecycle(1, a, events.LifecycleReady), // 9: forgotten on end
	)
	snap := h.Snapshot(Query{UserID: 1})
	byID := map[uint64]Signal{}
	for _, s := range snap {
		byID[s.ID] = s
	}
	for id, want := range map[uint64]string{
		1: "+ created",
		3: "○ idle · parked",
		4: "▲ blocked · parked",
		6: "○ idle · resumed",
		7: "- ended",
		9: "· none · ready",
	} {
		if got := byID[id].Label(); got != want {
			t.Errorf("signal %d label = %q, want %q", id, got, want)
		}
	}
	// A parked row of a blocked session is not itself a report asking for
	// attention.
	if byID[4].NeedsAttention() {
		t.Error("lifecycle rows other than failed do not ask for attention")
	}
}

func TestRendering(t *testing.T) {
	p := 40
	code := 2
	a := sess("infra", "claude-1")
	for name, tc := range map[string]struct {
		ev                events.Event
		label, where, msg string
		attention         bool
		state             events.State
	}{
		"blocked": {
			events.SessionStatusEvent(1, events.SessionStatus{Session: a, Record: events.Record{State: events.StateBlocked, Kind: events.BlockPermission, Progress: &p, Msg: "Apply \x1b[31m3\u202e to add?\n"}}),
			"▲ blocked 40% · permission", "infra / claude-1", "Apply [31m3 to add?", true, events.StateBlocked,
		},
		"working child": {
			events.SessionStatusEvent(1, events.SessionStatus{Session: a, Record: events.Record{ID: "lint", State: events.StateWorking, App: "golangci-lint"}}),
			"● working", "infra / claude-1/lint", "golangci-lint", false, events.StateWorking,
		},
		"cleared": {
			events.SessionStatusEvent(1, events.SessionStatus{Session: a, Previous: events.StateDone}),
			"· none", "infra / claude-1", "status cleared", false, events.StateNone,
		},
		"exited": {
			events.SessionLifecycleEvent(1, events.SessionLifecycle{Session: a, Step: events.LifecycleExited, ExitCode: &code}),
			"· none · exited", "infra / claude-1", "program exited with status 2", false, events.StateNone,
		},
		"failed": {
			events.SessionLifecycleEvent(1, events.SessionLifecycle{Session: a, Step: events.LifecycleFailed, Op: "start", Reason: "no\x07 image"}),
			"✗ failed · start", "infra / claude-1", "could not start the session: no image", true, events.StateError,
		},
		"parked auto": {
			events.SessionLifecycleEvent(1, events.SessionLifecycle{Session: a, Step: events.LifecycleParked, Op: "auto"}),
			"· none · parked", "infra / claude-1", "parked while idle", false, events.StateNone,
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := Signal{Event: tc.ev}
			if got := s.Label(); got != tc.label {
				t.Errorf("label = %q, want %q", got, tc.label)
			}
			if got := s.Where(); got != tc.where {
				t.Errorf("where = %q, want %q", got, tc.where)
			}
			if got := s.Message(0); got != tc.msg {
				t.Errorf("message = %q, want %q", got, tc.msg)
			}
			if got := s.NeedsAttention(); got != tc.attention {
				t.Errorf("attention = %t", got)
			}
			if got := s.State(); got != tc.state {
				t.Errorf("state = %q", got)
			}
		})
	}
	if got := Sanitize("a  b\tc\x00"+"d long tail", 8); got != "a b cd…" {
		t.Errorf("sanitize = %q", got)
	}
}
