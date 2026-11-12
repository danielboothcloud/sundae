package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/events"
)

var t0 = time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)

func at(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

// obs is a status observation of one session for the debouncer tests:
// "blocked:permission:msg" style, triggering for blocked, done and error.
func obs(spec string) note {
	parts := strings.SplitN(spec, ":", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	state := parts[0]
	n := note{key: "s1", sig: state + sep + parts[1] + sep + parts[2]}
	switch state {
	case "blocked", "done", "error":
		n.trigger = true
		n.msg = Message{Title: spec}
	}
	return n
}

func titles(ns []note) string {
	var s []string
	for _, n := range ns {
		s = append(s, n.msg.Title)
	}
	return strings.Join(s, ",")
}

func TestDebouncer(t *testing.T) {
	type step struct {
		at   int    // seconds after t0
		obs  string // observation ("none" is a cleared record), or "" to only run due
		want string // titles sent at this step
	}
	cases := map[string][]step{
		"first trigger is sent at once": {
			{0, "blocked:permission:Apply?", "blocked:permission:Apply?"},
		},
		"non-trigger without a window is ignored": {
			{0, "working", ""},
			{1, "idle", ""},
			{100, "", ""},
		},
		"identical repeat inside the window is suppressed": {
			{0, "blocked:permission:Apply?", "blocked:permission:Apply?"},
			{5, "working", ""},
			{10, "blocked:permission:Apply?", ""},
			{30, "", ""},
		},
		"a burst collapses into its latest state": {
			{0, "blocked:question:Which?", "blocked:question:Which?"},
			{3, "working", ""},
			{6, "blocked:permission:Apply?", ""},
			{9, "working", ""},
			{12, "done::Finished", ""},
			{29, "", ""},
			{30, "", "done::Finished"},
		},
		"the final state is sent when its window closes and opens a new window": {
			{0, "blocked::A", "blocked::A"},
			{10, "error::boom", ""},
			{30, "", "error::boom"},
			{35, "blocked::A", ""},
			{60, "", "blocked::A"},
		},
		"blocked then lost status does not re-notify": {
			{0, "blocked:permission:Apply?", "blocked:permission:Apply?"},
			{4, "none", ""},
			{30, "", ""},
		},
		"blocked held, then cleared, is never sent": {
			{0, "done::one", "done::one"},
			{5, "blocked:permission:Apply?", ""},
			{8, "none", ""},
			{30, "", ""},
		},
		"same message with another kind is not a repeat": {
			{0, "blocked:permission:x", "blocked:permission:x"},
			{5, "blocked:question:x", ""},
			{30, "", "blocked:question:x"},
		},
		"after the window an identical notification is sent again": {
			{0, "blocked::A", "blocked::A"},
			{40, "blocked::A", "blocked::A"},
		},
		"an observation after the window closed, before due ran, closes it first": {
			{0, "blocked::A", "blocked::A"},
			{10, "done::B", ""},
			{45, "error::C", "done::B"}, // done closes the old window; error is held in the new one
			{75, "", "error::C"},
		},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			d := newDebouncer(30 * time.Second)
			for i, s := range steps {
				var got []note
				if s.obs == "" {
					got = d.due(at(s.at))
				} else {
					got = d.observe(obs(s.obs), at(s.at))
				}
				if titles(got) != s.want {
					t.Fatalf("step %d (t=%ds %q): sent %q, want %q", i, s.at, s.obs, titles(got), s.want)
				}
			}
		})
	}
}

func TestDebouncerNextAndFlush(t *testing.T) {
	d := newDebouncer(30 * time.Second)
	if _, ok := d.next(); ok {
		t.Fatal("next with no windows")
	}
	d.observe(obs("blocked::A"), at(0))
	b := obs("done::B")
	b.key = "s2"
	d.observe(b, at(10))
	if next, ok := d.next(); !ok || !next.Equal(at(30)) {
		t.Fatalf("next = %v %v", next, ok)
	}

	// Held states are flushed at shutdown; repeats and non-triggers are not.
	d.observe(obs("error::C"), at(12))
	repeat := obs("done::B")
	repeat.key = "s2"
	d.observe(repeat, at(13))
	if got := titles(d.flush()); got != "error::C" {
		t.Fatalf("flush = %q", got)
	}
	if _, ok := d.next(); ok {
		t.Fatal("windows left after flush")
	}
}

func TestDebouncerZeroWindow(t *testing.T) {
	d := newDebouncer(0)
	for i, spec := range []string{"blocked::A", "working", "blocked::A", "done::B"} {
		got := titles(d.observe(obs(spec), at(i)))
		want := spec
		if spec == "working" {
			want = ""
		}
		if got != want {
			t.Fatalf("%s: sent %q", spec, got)
		}
	}
	if _, ok := d.next(); ok {
		t.Fatal("zero window kept state")
	}
}

func statusEv(user int64, sandbox, id string, state events.State, kind events.BlockKind, msg string) events.Event {
	return events.SessionStatusEvent(user, events.SessionStatus{
		Session: events.Session{User: "alice", Repo: "infra", Sandbox: sandbox},
		Record:  events.Record{ID: id, State: state, Kind: kind, Msg: msg},
	})
}

func TestClassifier(t *testing.T) {
	nc := config.NotificationsConfig{On: []string{"blocked", "done", "error", "forge_failed"}}
	c := newClassifier(nc)

	n, ok := c.classify(statusEv(1, "sb", "", events.StateBlocked, events.BlockPermission, "Apply?"))
	if !ok || !n.trigger || n.msg.Priority != priorityHigh {
		t.Fatalf("blocked: %+v %v", n, ok)
	}
	// The same status again is not a change.
	if _, ok := c.classify(statusEv(1, "sb", "", events.StateBlocked, events.BlockPermission, "Apply?")); ok {
		t.Fatal("unchanged status counted")
	}
	// Another record, user or session is another key.
	for _, ev := range []events.Event{
		statusEv(1, "sb", "build", events.StateBlocked, events.BlockPermission, "Apply?"),
		statusEv(2, "sb", "", events.StateBlocked, events.BlockPermission, "Apply?"),
		statusEv(1, "sb2", "", events.StateBlocked, events.BlockPermission, "Apply?"),
	} {
		if n2, ok := c.classify(ev); !ok || n2.key == n.key {
			t.Fatalf("not a separate key: %+v", ev)
		}
	}
	// Losing the status is an observation that does not trigger.
	if n, ok := c.classify(statusEv(1, "sb", "", events.StateNone, "", "")); !ok || n.trigger {
		t.Fatalf("none: %+v %v", n, ok)
	}
	// working is not in on.
	if n, ok := c.classify(statusEv(1, "sb", "", events.StateWorking, "", "x")); !ok || n.trigger {
		t.Fatalf("working: %+v", n)
	}

	// Forge: only a change of outcome counts, and only failures trigger.
	sync := func(ok bool, msg string) events.Event {
		return events.ForgeEvent(0, events.ForgeOutcome{Kind: "sync", Forge: "github", Repo: "github/me/x", OK: ok, Error: msg})
	}
	if n, ok := c.classify(sync(false, "401 Bad credentials")); !ok || !n.trigger {
		t.Fatalf("first failure: %+v %v", n, ok)
	}
	if _, ok := c.classify(sync(false, "401 Bad credentials")); ok {
		t.Fatal("repeated identical failure counted")
	}
	if n, ok := c.classify(sync(true, "")); !ok || n.trigger {
		t.Fatalf("recovery: %+v %v", n, ok)
	}
	if n, ok := c.classify(sync(false, "401 Bad credentials")); !ok || !n.trigger {
		t.Fatalf("failure after recovery: %+v %v", n, ok)
	}

	// session_failed is opt-in.
	failed := events.SessionLifecycleEvent(1, events.SessionLifecycle{
		Session: events.Session{User: "alice", Repo: "infra", Sandbox: "sb"},
		Step:    events.LifecycleFailed, Op: "start", Reason: "gateway unreachable",
	})
	if n, ok := c.classify(failed); !ok || n.trigger {
		t.Fatalf("session_failed not opted in: %+v", n)
	}
	c2 := newClassifier(config.NotificationsConfig{On: []string{"session_failed"}})
	if n, ok := c2.classify(failed); !ok || !n.trigger {
		t.Fatalf("session_failed opted in: %+v", n)
	}

	// Ending a session forgets its statuses, so a new session of the same
	// name notifies again.
	ended := events.SessionLifecycleEvent(1, events.SessionLifecycle{
		Session: events.Session{User: "alice", Repo: "infra", Sandbox: "sb"},
		Step:    events.LifecycleEnded,
	})
	c.classify(statusEv(1, "sb", "", events.StateDone, "", "ok"))
	if _, ok := c.classify(ended); !ok {
		t.Fatal("ended not observed")
	}
	if n, ok := c.classify(statusEv(1, "sb", "", events.StateDone, "", "ok")); !ok || !n.trigger {
		t.Fatalf("after end: %+v %v", n, ok)
	}
	// Other sessions are kept.
	if _, ok := c.classify(statusEv(1, "sb2", "", events.StateBlocked, events.BlockPermission, "Apply?")); ok {
		t.Fatal("ending sb forgot sb2")
	}
}
