package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/events"
	"github.com/charmbracelet/soft-serve/pkg/signals"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/x/ansi"
	"github.com/matryer/is"
)

// signalsTestContext returns alice's session context with a running
// signals History fed by bus.
func signalsTestContext(t *testing.T) (context.Context, *events.Bus, *signals.History, int64) {
	t.Helper()
	ctx, _, user, _ := newSessionTestContext(t)
	bus := events.New(events.WithLogger(log.New(io.Discard)))
	h := signals.New(100, signals.WithLogger(log.New(io.Discard)))
	h.Start(bus)
	t.Cleanup(func() {
		h.Close()
		bus.Close()
	})
	return signals.WithContext(ctx, h), bus, h, user.ID()
}

func sigSession(repo, name string) events.Session {
	return events.Session{User: "alice", Repo: repo, Name: name, Sandbox: "soft-alice-" + repo + "-" + name}
}

// publishAll publishes evs and waits for h to have recorded them.
func publishAll(t *testing.T, bus *events.Bus, h *signals.History, evs ...events.Event) {
	t.Helper()
	want := h.Len()
	for _, ev := range evs {
		if ev.Forge == nil { // not a signal
			want++
		}
		bus.Publish(ev)
	}
	waitUntil(t, func() bool { return h.Len() >= want })
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// signalLines returns the non-empty lines of a `session signals` table.
func signalLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// fields splits a table line into its columns.
func fields(l string) []string {
	var out []string
	for _, f := range strings.Split(l, signalColumnGap) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func sampleSignals(user int64) []events.Event {
	p := 40
	review := sigSession("infra", "review")
	return []events.Event{
		events.SessionLifecycleEvent(user, events.SessionLifecycle{Session: review, Step: events.LifecycleCreated}),
		events.SessionStatusEvent(user, events.SessionStatus{Session: sigSession("infra", "main"), Record: events.Record{State: events.StateWorking, Progress: &p, Msg: "Running go test"}}),
		events.SessionStatusEvent(user, events.SessionStatus{Session: review, Record: events.Record{ID: "build/lint", State: events.StateWorking, App: "golangci-lint"}}),
		events.SessionStatusEvent(user+1, events.SessionStatus{Session: sigSession("infra", "main"), Record: events.Record{State: events.StateBlocked, Msg: "someone else's"}}),
		events.ForgeEvent(user, events.ForgeOutcome{Kind: "sync", Forge: "github", OK: false, Error: "nope"}),
		events.SessionStatusEvent(user, events.SessionStatus{Session: review, Record: events.Record{
			State: events.StateBlocked, Kind: events.BlockPermission,
			Msg: "Apply \x1b[31m3\u202e to add?\n" + strings.Repeat("x", 200),
		}}),
	}
}

func TestSessionSignalsTable(t *testing.T) {
	is := is.New(t)
	ctx, bus, h, uid := signalsTestContext(t)

	stdout, _, err := runSessionCmd(t, ctx, "signals")
	is.NoErr(err)
	is.Equal(strings.TrimSpace(stdout), "No signals yet")

	publishAll(t, bus, h, sampleSignals(uid)...)

	stdout, _, err = runSessionCmd(t, ctx, "signals")
	is.NoErr(err)
	is.True(!strings.ContainsAny(stdout, "\x1b\u202e"))
	lines := signalLines(stdout)
	is.Equal(len(lines), 5) // header and alice's four signals, not bob's or the forge's
	is.Equal(fields(lines[0]), []string{"TIME", "STATE", "SESSION", "MESSAGE"})
	type row struct{ label, where, msg string }
	want := []row{
		{"▲ blocked · permission", "infra / review", "Apply [31m3 to add? " + strings.Repeat("x", 200)},
		{"● working", "infra / review/build/lint", "golangci-lint"},
		{"● working 40%", "infra / main", "Running go test"},
		{"+ created", "infra / review", "sandbox created"},
	}
	for i, w := range want {
		f := fields(lines[i+1])
		is.Equal(len(f), 4) // time, state, session, message
		is.Equal(len(f[0]), 8)
		is.Equal(row{f[1], f[2], f[3]}, w)
	}

	// Filters.
	for args, n := range map[string]int{
		"--repo infra":             4,
		"--repo docs":              0,
		"--name review":            3,
		"--repo infra --name main": 1,
		"--attention":              1,
		"--limit 2":                2,
		"-l 0":                     4,
	} {
		stdout, _, err := runSessionCmd(t, ctx, append([]string{"signals"}, strings.Fields(args)...)...)
		is.NoErr(err)
		got := len(signalLines(stdout)) - 1
		if n == 0 {
			got = 0
			is.Equal(strings.TrimSpace(stdout), "No signals yet")
		}
		if got != n {
			t.Errorf("%s: %d rows, want %d", args, got, n)
		}
	}
	_, _, err = runSessionCmd(t, ctx, "signals", "--limit", "-1")
	is.True(err != nil)

	// With a PTY, rows are cut to the terminal's width.
	pty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{})
	stdout, _, err = runSessionCmd(t, pty, "signals")
	is.NoErr(err)
	lines = signalLines(stdout)
	is.Equal(len(lines), 5)
	for _, l := range lines {
		is.True(ansi.StringWidth(l) <= 80) // cut to the PTY width
	}
	is.True(strings.HasSuffix(lines[1], "…"))
}

func TestSessionSignalsJSON(t *testing.T) {
	is := is.New(t)
	ctx, bus, h, uid := signalsTestContext(t)
	publishAll(t, bus, h, sampleSignals(uid)...)

	stdout, _, err := runSessionCmd(t, ctx, "signals", "--json")
	is.NoErr(err)
	lines := signalLines(stdout)
	is.Equal(len(lines), 4)
	var got []map[string]any
	for _, l := range lines {
		var m map[string]any
		is.NoErr(json.Unmarshal([]byte(l), &m)) // one object per line
		got = append(got, m)
	}
	blocked := got[0]
	is.Equal(blocked["type"], "status")
	is.Equal(blocked["repo"], "infra")
	is.Equal(blocked["session"], "review")
	is.Equal(blocked["state"], "blocked")
	is.Equal(blocked["kind"], "permission")
	is.Equal(blocked["attention"], true)
	is.True(!strings.ContainsAny(blocked["msg"].(string), "\x1b\u202e\n"))
	is.True(strings.HasSuffix(blocked["msg"].(string), strings.Repeat("x", 200))) // not cut
	is.Equal(got[1]["record"], "build/lint")
	is.Equal(got[2]["progress"], float64(40))
	is.Equal(got[3]["type"], "lifecycle")
	is.Equal(got[3]["step"], "created")
	is.Equal(got[3]["state"], "none")
	is.True(got[0]["id"].(float64) > got[1]["id"].(float64)) // newest first

	stdout, _, err = runSessionCmd(t, ctx, "signals", "--json", "--repo", "nope")
	is.NoErr(err)
	is.Equal(stdout, "")
}

// syncBuffer is a bytes.Buffer safe to read while a command writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startFollow runs `session signals --follow args...` in the background.
func startFollow(t *testing.T, ctx context.Context, args ...string) (stdout, stderr *syncBuffer, done <-chan error) {
	t.Helper()
	c := SessionCommand()
	stdout, stderr = &syncBuffer{}, &syncBuffer{}
	c.SetOut(stdout)
	c.SetErr(stderr)
	c.SetArgs(append([]string{"signals", "--follow"}, args...))
	ch := make(chan error, 1)
	go func() { ch <- c.ExecuteContext(ctx) }()
	return stdout, stderr, ch
}

func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("session signals --follow did not end")
		return nil
	}
}

func TestSessionSignalsFollow(t *testing.T) {
	is := is.New(t)
	ctx, bus, h, uid := signalsTestContext(t)
	review := sigSession("infra", "review")
	publishAll(t, bus, h,
		events.SessionStatusEvent(uid, events.SessionStatus{Session: review, Record: events.Record{State: events.StateWorking, Msg: "first"}}),
		events.SessionStatusEvent(uid, events.SessionStatus{Session: review, Record: events.Record{State: events.StateIdle, Msg: "second"}}),
	)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stdout, _, done := startFollow(t, ctx)
	waitUntil(t, func() bool { return strings.Contains(stdout.String(), "second") })

	bus.Publish(events.SessionStatusEvent(uid+1, events.SessionStatus{Session: review, Record: events.Record{State: events.StateDone, Msg: "not mine"}}))
	bus.Publish(events.SessionLifecycleEvent(uid, events.SessionLifecycle{Session: review, Step: events.LifecycleParked, Op: "auto"}))
	waitUntil(t, func() bool { return strings.Contains(stdout.String(), "parked while idle") })

	// The client goes away.
	cancel()
	is.NoErr(waitDone(t, done))

	lines := signalLines(stdout.String())
	is.Equal(len(lines), 4)
	// Oldest first, like tail -f; nothing twice, nobody else's.
	is.True(strings.Contains(lines[1], "first"))
	is.True(strings.Contains(lines[2], "second"))
	is.Equal(fields(lines[3])[1:], []string{"○ idle · parked", "infra / review", "parked while idle"})
	is.True(!strings.Contains(stdout.String(), "not mine"))
}

func TestSessionSignalsFollowEndsAtShutdown(t *testing.T) {
	is := is.New(t)
	ctx, bus, h, uid := signalsTestContext(t)
	stdout, stderr, done := startFollow(t, ctx, "--json", "--attention")
	bus.Publish(events.SessionStatusEvent(uid, events.SessionStatus{Session: sigSession("r", "main"), Record: events.Record{State: events.StateWorking}}))
	bus.Publish(events.SessionStatusEvent(uid, events.SessionStatus{Session: sigSession("r", "main"), Record: events.Record{State: events.StateDone, Msg: "finished"}}))
	waitUntil(t, func() bool { return strings.Contains(stdout.String(), "finished") })

	h.Close()
	is.NoErr(waitDone(t, done))
	is.True(strings.Contains(stderr.String(), "shutting down"))
	lines := signalLines(stdout.String())
	is.Equal(len(lines), 1) // the working report is filtered out
	var m map[string]any
	is.NoErr(json.Unmarshal([]byte(lines[0]), &m))
	is.Equal(m["state"], "done")
}

// interruptSession is a PTY session whose client types what is sent on in.
type interruptSession struct {
	*fakeSSHSession
	in chan []byte
}

func (s *interruptSession) Read(p []byte) (int, error) {
	b, ok := <-s.in
	if !ok {
		return 0, io.EOF
	}
	return copy(p, b), nil
}

func TestSessionSignalsFollowCtrlC(t *testing.T) {
	is := is.New(t)
	ctx, _, _, _ := signalsTestContext(t)
	sess := &interruptSession{fakeSSHSession: &fakeSSHSession{}, in: make(chan []byte, 1)}
	ctx = context.WithValue(ctx, sshutils.ContextKeySession, sess)
	stdout, _, done := startFollow(t, ctx)
	waitUntil(t, func() bool { return strings.Contains(stdout.String(), "MESSAGE") })
	sess.in <- []byte{'x', 0x03}
	is.NoErr(waitDone(t, done))
	close(sess.in)
}

func TestSessionSignalsWithoutFeed(t *testing.T) {
	is := is.New(t)
	ctx, _, _, _ := newSessionTestContext(t)
	_, _, err := runSessionCmd(t, ctx, "signals")
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "not available"))
}
