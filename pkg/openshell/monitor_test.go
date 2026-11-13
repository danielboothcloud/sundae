package openshell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/fake"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/events"
)

// monitorClient is a Client for Monitor tests: sandboxes live in the SDK's
// fake, and the status poll is answered from a table per sandbox.
type monitorClient struct {
	*sdkClient
	fc *fake.Client

	mu sync.Mutex
	// out is the status script's output per sandbox: the recorded id, a
	// newline, `osewa local ls` JSON.
	out    map[string]string
	runErr map[string]error
	// gate, when set, holds every poll until it is closed or the call's
	// context ends.
	gate        chan struct{}
	inflight    int
	maxInflight int
	runs        map[string]int
	// suspendErr is returned by SuspendSandbox.
	suspendErr error
	suspends   map[string]int
	closed     int
}

func newMonitorClient(t *testing.T) *monitorClient {
	t.Helper()
	fc := fake.NewClient()
	t.Cleanup(func() { _ = fc.Close() })
	return &monitorClient{
		sdkClient: newSDKClient("default", fc.Sandboxes(), nil, nil, nil),
		fc:        fc,
		out:       map[string]string{},
		runErr:    map[string]error{},
		runs:      map[string]int{},
		suspends:  map[string]int{},
	}
}

func (c *monitorClient) Run(ctx context.Context, sandbox string, command []string, opts ExecOptions) (*ExecResult, error) {
	if err := gatewayLimits(command, opts); err != nil {
		return nil, err
	}
	call := execCall{sandbox, command, opts}
	if call.script() != osewaStatusName || opts.Env["HOME"] != SandboxRoot || opts.Env["OSEWA_HOME"] != SandboxRoot {
		return nil, fmt.Errorf("unexpected exec %q %v", command, opts.Env)
	}
	if a := call.args(); len(a) != 2 || a[0] != osewaPath() || a[1] != osewaSessionFile {
		return nil, fmt.Errorf("unexpected status args %q", a)
	}
	c.mu.Lock()
	c.runs[sandbox]++
	c.inflight++
	c.maxInflight = max(c.maxInflight, c.inflight)
	gate := c.gate
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.inflight--
		c.mu.Unlock()
	}()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, &types.StatusError{Code: types.ErrorDeadlineExceeded, Message: ctx.Err().Error()}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.runErr[sandbox]; err != nil {
		return nil, err
	}
	return &ExecResult{Stdout: []byte(c.out[sandbox])}, nil
}

func (c *monitorClient) SuspendSandbox(ctx context.Context, name string) (*Sandbox, error) {
	c.mu.Lock()
	c.suspends[name]++
	err := c.suspendErr
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return c.sdkClient.SuspendSandbox(ctx, name)
}

func (c *monitorClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	return nil
}

func (c *monitorClient) set(sandbox, id string, sessions ...osewaSessionInfo) {
	b, err := json.Marshal(map[string]any{"id": 0, "sessions": sessions})
	if err != nil {
		panic(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.out[sandbox] = id + "\n" + string(b) + "\n"
	delete(c.runErr, sandbox)
}

func (c *monitorClient) fail(sandbox string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runErr[sandbox] = err
}

func (c *monitorClient) runCount(sandbox string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs[sandbox]
}

func (c *monitorClient) suspendCount(sandbox string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.suspends[sandbox]
}

// addSession adds a sandbox Sundae created for the user's session.
func (c *monitorClient) addSession(user testUser, repo, name string, phase types.SandboxPhase) string {
	sandbox := SessionSandboxName(user.name, repo, name)
	labels := ownerLabels(user)
	c.fc.AddSandbox("default", &types.Sandbox{
		Name:        sandbox,
		Labels:      labels,
		Annotations: sessionAnnotations(user.name, repo, name, "", nil),
		Status:      types.SandboxStatus{Phase: phase},
	})
	return sandbox
}

func (c *monitorClient) setPhase(t *testing.T, sandbox string, phase types.SandboxPhase) {
	t.Helper()
	sb, err := c.fc.Sandboxes().Get(context.Background(), "default", sandbox)
	if err != nil {
		t.Fatal(err)
	}
	sb.Status.Phase = phase
	c.fc.AddSandbox("default", sb)
}

func (c *monitorClient) phase(t *testing.T, sandbox string) types.SandboxPhase {
	t.Helper()
	sb, err := c.fc.Sandboxes().Get(context.Background(), "default", sandbox)
	if err != nil {
		t.Fatal(err)
	}
	return sb.Status.Phase
}

// testClock is an injectable clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type monitorHarness struct {
	mc   *monitorClient
	mon  *Monitor
	clk  *testClock
	sub  *events.Subscription
	logs *syncBuffer
}

// syncBuffer is a bytes.Buffer safe for concurrent writes.
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

func newMonitorHarness(t *testing.T, configure func(*config.Config)) *monitorHarness {
	t.Helper()
	cfg := config.DefaultConfig()
	if configure != nil {
		configure(cfg)
	}
	mc := newMonitorClient(t)
	bus := events.New(events.WithLogger(log.New(io.Discard)))
	t.Cleanup(bus.Close)
	logs := &syncBuffer{}
	clk := newTestClock()
	mon := NewMonitor(func() (Client, error) { return mc, nil }, cfg,
		WithMonitorEvents(bus), WithMonitorLogger(log.New(logs)))
	mon.now = clk.now
	return &monitorHarness{mc: mc, mon: mon, clk: clk, sub: bus.Subscribe(events.Buffer(128)), logs: logs}
}

// poll runs one round and waits for the parks it started.
func (h *monitorHarness) poll(t *testing.T) {
	t.Helper()
	h.mon.Poll(t.Context())
	h.mon.parks.Wait()
}

// events returns the events published so far.
func (h *monitorHarness) events() []events.Event {
	var out []events.Event
	for {
		select {
		case ev := <-h.sub.C:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func (h *monitorHarness) status(t *testing.T, sandbox string) SessionStatus {
	t.Helper()
	st, ok := h.mon.Status(sandbox)
	if !ok {
		t.Fatalf("no status for %s", sandbox)
	}
	return st
}

func describe(evs []events.Event) string {
	var s []string
	for _, ev := range evs {
		switch {
		case ev.Status != nil:
			s = append(s, fmt.Sprintf("status %s %s->%s", ev.Status.Session.Name, orNone(ev.Status.Previous), orNone(ev.Status.Record.State)))
		case ev.Lifecycle != nil:
			l := ev.Lifecycle
			x := fmt.Sprintf("lifecycle %s %s", l.Session.Name, l.Step)
			if l.Op != "" {
				x += " " + l.Op
			}
			if l.ExitCode != nil {
				x += fmt.Sprintf(" %d", *l.ExitCode)
			}
			s = append(s, x)
		}
	}
	return strings.Join(s, "; ")
}

func orNone(s events.State) string {
	if s == events.StateNone {
		return "none"
	}
	return string(s)
}

func st(state, kind, msg string, progress ...int64) *osewaProgramStatus {
	s := &osewaProgramStatus{State: state, Kind: kind, Msg: msg, App: "claude-code"}
	if len(progress) > 0 {
		s.Progress = &progress[0]
	}
	return s
}

var bob = testUser{id: 2, name: "bob"}

// The status poll parses what osewa 0.1.4 prints: the live darwin build's
// output, and a fixture built from its wire types.
func TestParseOsewaListFixtures(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	ls, err := parseOsewaList(read("osewa-local-ls-empty.json"))
	if err != nil || len(ls.Sessions) != 0 {
		t.Fatalf("empty: %+v %v", ls, err)
	}

	ls, err = parseOsewaList(read("osewa-local-ls-working.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, ok := ls.find("0a37cad0111161f38ab404a00dd6c51e")
	if !ok || s.Exited || s.Attached != 0 || s.Status == nil {
		t.Fatalf("working: %+v", s)
	}
	rec := recordOf(s.Status)
	if rec.State != events.StateWorking || rec.App != "sundae-test" || rec.Msg != "Building" || rec.Progress == nil || *rec.Progress != 40 || rec.Kind != "" {
		t.Fatalf("working record: %+v", rec)
	}

	ls, _ = parseOsewaList(read("osewa-local-ls-blocked.json"))
	s, _ = ls.find("0a37cad0111161f38ab404a00dd6c51e")
	if rec := recordOf(s.Status); rec.State != events.StateBlocked || rec.Kind != events.BlockPermission || rec.Msg != "Apply 3 changes?" || rec.Progress != nil {
		t.Fatalf("blocked record: %+v", rec)
	}

	ls, _ = parseOsewaList(read("osewa-local-ls-exited.json"))
	s, _ = ls.find("0a37cad0111161f38ab404a00dd6c51e")
	if !s.Exited || s.ExitCode != 7 {
		t.Fatalf("exited: %+v", s)
	}
	if rec := recordOf(s.Status); rec.State != events.StateDone || rec.Title != "Tests" || rec.Msg != "All tests pass" {
		t.Fatalf("exited record: %+v", rec)
	}

	ls, err = parseOsewaList(read("osewa-local-ls-mixed.json"))
	if err != nil || len(ls.Sessions) != 3 {
		t.Fatalf("mixed: %+v %v", ls, err)
	}
	if s, _ := ls.find("a"); s.Status != nil || recordOf(s.Status).State != events.StateNone {
		t.Fatalf("no status: %+v", s)
	}
	s, _ = ls.find("5f0c2b9e7d")
	rec = recordOf(s.Status)
	if s.Attached != 2 || rec.State != events.StateBlocked || rec.Kind != events.BlockQuestion || rec.Title != "Plan" ||
		rec.Msg != "Which database? \x1b]52;c;aGk=\x07" || *rec.Progress != 10 {
		t.Fatalf("attached session: %+v %+v", s, rec)
	}
	if s, _ := ls.find("old"); s.Incompatible == "" || s.IdleSeconds != 7200 {
		t.Fatalf("incompatible: %+v", s)
	}
	if _, ok := ls.find(""); ok {
		t.Fatal("found the empty id")
	}

	if _, err := parseOsewaList("osewa local: ls: No such file or directory"); err == nil {
		t.Fatal("garbage parsed")
	}
}

func TestRecordOf(t *testing.T) {
	for _, tc := range []struct {
		in   *osewaProgramStatus
		want events.State
		prog *int
	}{
		{nil, events.StateNone, nil},
		{&osewaProgramStatus{State: "clear"}, events.StateNone, nil},
		{st("idle", "", "", 10), events.StateIdle, intp(10)},
		{st("working", "", "", 101), events.StateWorking, nil}, // out of range
		{st("working", "", "", -1), events.StateWorking, nil},
	} {
		rec := recordOf(tc.in)
		if rec.State != tc.want || (rec.Progress == nil) != (tc.prog == nil) || (rec.Progress != nil && *rec.Progress != *tc.prog) {
			t.Errorf("recordOf(%+v) = %+v", tc.in, rec)
		}
	}
}

func intp(i int) *int { return &i }

func TestMonitorChangeDetectionAndEvents(t *testing.T) {
	h := newMonitorHarness(t, nil)
	main := h.mc.addSession(alice, "infra", "main", types.SandboxReady)
	h.mc.set(main, "o1", osewaSessionInfo{ID: "o1", Status: st("blocked", "permission", "Apply?")})

	// The first listing is a baseline: a restart must not replay every
	// session's status to the notifier.
	h.poll(t)
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("baseline published %s", describe(evs))
	}
	got := h.status(t, main)
	if got.Record.State != events.StateBlocked || got.Record.Kind != events.BlockPermission || got.Record.Msg != "Apply?" ||
		!got.Running || got.Stale || got.Error != "" || got.OsewaID != "o1" || got.Session.Name != "main" ||
		got.Session.User != "alice" || got.Session.Repo != "infra" || got.UserID != alice.id {
		t.Fatalf("baseline: %+v", got)
	}
	if !got.Changed.Equal(h.clk.now()) || !got.Polled.Equal(h.clk.now()) {
		t.Fatalf("times: %+v", got)
	}
	changed := got.Changed

	// Unchanged: nothing published, Changed kept, Polled moves.
	h.clk.advance(10 * time.Second)
	h.mc.set(main, "o1", osewaSessionInfo{ID: "o1", Attached: 1, IdleSeconds: 3, Status: st("blocked", "permission", "Apply?")})
	h.poll(t)
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("unchanged published %s", describe(evs))
	}
	got = h.status(t, main)
	if !got.Changed.Equal(changed) || !got.Polled.Equal(h.clk.now()) || got.Attached != 1 || got.IdleSeconds != 3 {
		t.Fatalf("unchanged: %+v", got)
	}

	// Each change is published with the previous state.
	for _, step := range []struct {
		status *osewaProgramStatus
		want   string
	}{
		{st("working", "", "Planning", 10), "status main blocked->working"},
		{st("working", "", "Planning", 40), "status main working->working"}, // progress
		{st("working", "", "Running tests", 40), "status main working->working"},
		{nil, "status main working->none"}, // cleared
		{st("done", "", "All tests pass"), "status main none->done"},
	} {
		h.clk.advance(10 * time.Second)
		h.mc.set(main, "o1", osewaSessionInfo{ID: "o1", Status: step.status})
		h.poll(t)
		evs := h.events()
		if describe(evs) != step.want {
			t.Fatalf("%+v: events %q, want %q", step.status, describe(evs), step.want)
		}
		if ev := evs[0]; ev.UserID != alice.id || ev.Status.Session.Sandbox != main || ev.Status.Session.Repo != "infra" {
			t.Fatalf("event: %+v", ev)
		}
		if got := h.status(t, main); !got.Changed.Equal(h.clk.now()) {
			t.Fatalf("Changed not updated: %+v", got)
		}
	}

	// The program exits: exited with its code, once.
	h.clk.advance(10 * time.Second)
	h.mc.set(main, "o1", osewaSessionInfo{ID: "o1", Exited: true, ExitCode: 3, Status: st("done", "", "All tests pass")})
	h.poll(t)
	if d := describe(h.events()); d != "lifecycle main exited 3" {
		t.Fatalf("exit: %q", d)
	}
	got = h.status(t, main)
	if got.Running || !got.Exited || got.ExitCode == nil || *got.ExitCode != 3 || got.Record.State != events.StateDone {
		t.Fatalf("exited: %+v", got)
	}
	h.poll(t)
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("exit published twice: %s", describe(evs))
	}

	// Attach starts the program again in a new osewa session; when that
	// session disappears (osewa reaped it, the sandbox restarted), the
	// program is gone too.
	h.mc.set(main, "o2", osewaSessionInfo{ID: "o2", Status: st("working", "", "")})
	h.poll(t)
	if d := describe(h.events()); d != "status main done->working" {
		t.Fatalf("restart: %q", d)
	}
	h.mc.set(main, "o2")
	h.poll(t)
	if d := describe(h.events()); d != "status main working->none; lifecycle main exited" {
		t.Fatalf("gone: %q", d)
	}
	if got := h.status(t, main); got.Running || got.Exited || got.ExitCode != nil {
		t.Fatalf("gone: %+v", got)
	}
}

// A session that appears after the first listing is new: its first status
// is published.
func TestMonitorNewSessionPublishes(t *testing.T) {
	h := newMonitorHarness(t, nil)
	h.poll(t) // nothing yet

	sb := h.mc.addSession(alice, "r", "two", types.SandboxReady)
	// Not set up yet: no id recorded, no osewa needed.
	h.mc.mu.Lock()
	h.mc.out[sb] = "\n"
	h.mc.mu.Unlock()
	h.poll(t)
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("no program: %s", describe(evs))
	}
	if got := h.status(t, sb); got.Running || got.Stale || got.OsewaID != "" {
		t.Fatalf("no program: %+v", got)
	}

	h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1", Status: st("working", "", "")})
	h.poll(t)
	if d := describe(h.events()); d != "status two none->working" {
		t.Fatalf("first status: %q", d)
	}

	// A new session whose program has already exited.
	other := h.mc.addSession(alice, "r", "three", types.SandboxReady)
	h.mc.set(other, "o9", osewaSessionInfo{ID: "o9", Exited: true, ExitCode: 1})
	h.poll(t)
	if d := describe(h.events()); d != "lifecycle three exited 1" {
		t.Fatalf("exited at first sight: %q", d)
	}

	// An ended session is forgotten.
	if err := h.mc.DeleteSandbox(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	h.poll(t)
	if _, ok := h.mon.Status(other); ok {
		t.Fatal("ended session still listed")
	}
}

func TestMonitorStale(t *testing.T) {
	h := newMonitorHarness(t, nil)
	sb := h.mc.addSession(alice, "r", "main", types.SandboxReady)
	h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1", Status: st("blocked", "question", "Which?")})
	h.poll(t)

	// The exec fails: the last known status stays, marked stale, and the
	// failure is logged once.
	h.clk.advance(10 * time.Second)
	h.mc.fail(sb, &types.StatusError{Code: types.ErrorUnavailable, Message: "gateway down"})
	h.poll(t)
	h.poll(t)
	got := h.status(t, sb)
	if !got.Stale || got.Record.State != events.StateBlocked || !strings.Contains(got.Error, "gateway down") || got.Polled.Equal(h.clk.now()) {
		t.Fatalf("stale: %+v", got)
	}
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("stale published %s", describe(evs))
	}
	if n := strings.Count(h.logs.String(), "cannot read session status"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, h.logs)
	}

	// Garbage output is a failure too, and so is a sandbox without osewa.
	h.mc.mu.Lock()
	h.mc.out[sb] = "o1\nosewa: command not found\n"
	delete(h.mc.runErr, sb)
	h.mc.mu.Unlock()
	h.poll(t)
	if got := h.status(t, sb); !got.Stale || !strings.Contains(got.Error, "unexpected output") {
		t.Fatalf("garbage: %+v", got)
	}

	// Recovery.
	h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1", Status: st("blocked", "question", "Which?")})
	h.poll(t)
	if got := h.status(t, sb); got.Stale || got.Error != "" || !got.Polled.Equal(h.clk.now()) {
		t.Fatalf("recovered: %+v", got)
	}

	// The listing fails: every session is stale, nothing is exec'd.
	h.mc.fc.Close()
	runs := h.mc.runCount(sb)
	h.poll(t)
	if got := h.status(t, sb); !got.Stale || got.Error == "" || got.Record.State != events.StateBlocked {
		t.Fatalf("list failed: %+v", got)
	}
	if h.mc.runCount(sb) != runs {
		t.Fatal("exec'd after a failed listing")
	}
	if !strings.Contains(h.logs.String(), "cannot list session sandboxes") {
		t.Fatalf("not logged:\n%s", h.logs)
	}
}

// Suspended, stopped and starting sandboxes are not exec'd; unmanaged ones
// are ignored.
func TestMonitorPollsOnlyReadyManagedSandboxes(t *testing.T) {
	h := newMonitorHarness(t, nil)
	ready := h.mc.addSession(alice, "r", "main", types.SandboxReady)
	parked := h.mc.addSession(alice, "r", "parked", types.SandboxReady)
	stopped := h.mc.addSession(alice, "r", "stopped", types.SandboxReady)
	for _, sb := range []string{ready, parked, stopped} {
		h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1", Status: st("blocked", "permission", "Apply?")})
	}
	h.mc.fc.AddSandbox("default", &types.Sandbox{
		Name:        "someone-elses",
		Labels:      map[string]string{LabelUserID: "1"}, // not managed
		Annotations: map[string]string{AnnotationRepo: "r"},
		Status:      types.SandboxStatus{Phase: types.SandboxReady},
	})
	h.mc.fc.AddSandbox("default", &types.Sandbox{
		Name:   "managed-but-ownerless",
		Labels: map[string]string{LabelManaged: "true"},
		Status: types.SandboxStatus{Phase: types.SandboxReady},
	})
	h.poll(t)

	h.mc.setPhase(t, parked, types.SandboxSuspended)
	h.mc.setPhase(t, stopped, types.SandboxStopped)
	starting := h.mc.addSession(alice, "r", "starting", types.SandboxProvisioning)
	h.poll(t)

	for sb, want := range map[string]int{ready: 2, parked: 1, stopped: 1, starting: 0, "someone-elses": 0, "managed-but-ownerless": 0} {
		if got := h.mc.runCount(sb); got != want {
			t.Errorf("%s exec'd %d times, want %d", sb, got, want)
		}
	}
	for _, sb := range []string{"someone-elses", "managed-but-ownerless"} {
		if _, ok := h.mon.Status(sb); ok {
			t.Errorf("%s is tracked", sb)
		}
	}
	// A parked session keeps the status it had: still blocked, not stale.
	if got := h.status(t, parked); !got.Parked || got.Stale || got.Record.State != events.StateBlocked || got.Phase != string(types.SandboxSuspended) {
		t.Fatalf("parked: %+v", got)
	}
	if got := h.status(t, stopped); got.Parked || !got.Stale || got.Record.State != events.StateBlocked {
		t.Fatalf("stopped: %+v", got)
	}
	if got := h.status(t, starting); got.Stale != true || got.Record.State != events.StateNone {
		t.Fatalf("starting: %+v", got)
	}
}

func TestMonitorConcurrencyBound(t *testing.T) {
	h := newMonitorHarness(t, nil)
	h.mon.concurrency = 3
	var sandboxes []string
	for i := range 10 {
		sb := h.mc.addSession(alice, fmt.Sprintf("r%d", i), "main", types.SandboxReady)
		h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1"})
		sandboxes = append(sandboxes, sb)
	}
	gate := make(chan struct{})
	h.mc.gate = gate

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.mon.Poll(t.Context())
	}()
	// Wait until the poll is held at the bound, then let it go.
	for {
		h.mc.mu.Lock()
		n := h.mc.inflight
		h.mc.mu.Unlock()
		if n == 3 {
			break
		}
		select {
		case <-done:
			t.Fatal("poll finished while gated")
		default:
		}
		runtime.Gosched()
	}
	close(gate)
	<-done
	if h.mc.maxInflight != 3 {
		t.Fatalf("max in flight = %d, want 3", h.mc.maxInflight)
	}
	for _, sb := range sandboxes {
		if h.mc.runCount(sb) != 1 {
			t.Fatalf("%s polled %d times", sb, h.mc.runCount(sb))
		}
	}
	h.mc.mu.Lock()
	closed := h.mc.closed
	h.mc.mu.Unlock()
	if closed != 1 {
		t.Fatalf("client closed %d times", closed)
	}
}

// A poll that hangs is cut off by the per-call timeout and leaves the
// status stale.
func TestMonitorPollTimeout(t *testing.T) {
	h := newMonitorHarness(t, nil)
	h.mon.timeout = 50 * time.Millisecond
	sb := h.mc.addSession(alice, "r", "main", types.SandboxReady)
	h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1"})
	h.mc.gate = make(chan struct{}) // never opened
	h.poll(t)
	if got := h.status(t, sb); !got.Stale || !strings.Contains(got.Error, "deadline") {
		t.Fatalf("timeout: %+v", got)
	}
}

// Run polls at once, then on every tick, until its context ends.
func TestMonitorRun(t *testing.T) {
	h := newMonitorHarness(t, func(c *config.Config) { c.Sessions.Status.PollInterval = 7 * time.Second })
	sb := h.mc.addSession(alice, "r", "main", types.SandboxReady)
	h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1"})
	ticks := make(chan time.Time)
	asked := make(chan time.Duration, 4)
	h.mon.tick = func(d time.Duration) <-chan time.Time {
		asked <- d
		return ticks
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.mon.Run(ctx)
	}()
	if d := <-asked; d != 7*time.Second {
		t.Fatalf("interval %s", d)
	}
	if n := h.mc.runCount(sb); n != 1 {
		t.Fatalf("first poll: %d", n)
	}
	ticks <- time.Time{}
	<-asked
	if n := h.mc.runCount(sb); n != 2 {
		t.Fatalf("after a tick: %d", n)
	}
	cancel()
	<-done
	var nilMon *Monitor
	nilMon.Run(ctx) // a nil Monitor returns at once
}

func TestParkable(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	after := 30 * time.Minute
	idle := func(mutate func(*entry)) *entry {
		e := &entry{known: true, lastActive: now.Add(-after)}
		e.st.Phase = string(types.SandboxReady)
		e.st.Running = true
		e.st.Record = events.Record{State: events.StateIdle}
		e.st.IdleSeconds = int64(after / time.Second)
		if mutate != nil {
			mutate(e)
		}
		return e
	}
	for name, tc := range map[string]struct {
		e       *entry
		bridges int
		want    bool
	}{
		"idle for after":                 {idle(nil), 0, true},
		"idle, one second short":         {idle(func(e *entry) { e.lastActive = now.Add(-after + time.Second) }), 0, false},
		"done":                           {idle(func(e *entry) { e.st.Record.State = events.StateDone }), 0, true},
		"blocked":                        {idle(func(e *entry) { e.st.Record = events.Record{State: events.StateBlocked, Kind: events.BlockPermission} }), 0, true},
		"error":                          {idle(func(e *entry) { e.st.Record.State = events.StateError }), 0, true},
		"working":                        {idle(func(e *entry) { e.st.Record.State = events.StateWorking }), 0, false},
		"none, quiet":                    {idle(func(e *entry) { e.st.Record = events.Record{} }), 0, true},
		"none, output a second ago":      {idle(func(e *entry) { e.st.Record = events.Record{}; e.st.IdleSeconds = 1 }), 0, false},
		"blocked, output a second ago":   {idle(func(e *entry) { e.st.Record.State = events.StateBlocked; e.st.IdleSeconds = 1 }), 0, true},
		"attached":                       {idle(func(e *entry) { e.st.Attached = 1 }), 0, false},
		"bridge open":                    {idle(nil), 1, false},
		"exited":                         {idle(func(e *entry) { e.st.Running, e.st.Exited, e.st.Record = false, true, events.Record{} }), 0, true},
		"exited, recently":               {idle(func(e *entry) { e.st.Running, e.st.Exited = false, true; e.lastActive = now.Add(-time.Minute) }), 0, false},
		"exited, output-free regardless": {idle(func(e *entry) { e.st.Running, e.st.Record, e.st.IdleSeconds = false, events.Record{}, 0 }), 0, true},
		"no program yet":                 {idle(func(e *entry) { e.st.Running, e.st.OsewaID, e.st.Record = false, "", events.Record{} }), 0, true},
		"status unknown":                 {idle(func(e *entry) { e.known = false }), 0, false},
		"stale":                          {idle(func(e *entry) { e.st.Stale = true }), 0, false},
		"parked already":                 {idle(func(e *entry) { e.st.Phase = string(types.SandboxSuspended) }), 0, false},
		"stopped":                        {idle(func(e *entry) { e.st.Phase = string(types.SandboxStopped) }), 0, false},
		"unsuspendable":                  {idle(func(e *entry) { e.st.Unsuspendable = "no" }), 0, false},
	} {
		if got, why := parkable(tc.e, tc.bridges, now, after, false); got != tc.want {
			t.Errorf("%s: parkable = %v (%s), want %v", name, got, why, tc.want)
		}
	}
	// With the snapshot fallback, a sandbox the gateway cannot suspend is
	// parked all the same.
	if got, why := parkable(idle(func(e *entry) { e.st.Unsuspendable = "no" }), 0, now, after, true); !got {
		t.Errorf("unsuspendable with fallback: parkable = false (%s)", why)
	}
}

func TestMonitorAutoPark(t *testing.T) {
	h := newMonitorHarness(t, nil)
	sb := h.mc.addSession(alice, "infra", "main", types.SandboxReady)
	busy := h.mc.addSession(alice, "infra", "busy", types.SandboxReady)
	h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1", IdleSeconds: 0, Status: st("blocked", "permission", "Apply?")})
	h.mc.set(busy, "o1", osewaSessionInfo{ID: "o1", Status: st("working", "", "")})
	h.poll(t)
	h.events()

	// Not yet: the idle window starts when Sundae first sees it.
	h.clk.advance(30*time.Minute - time.Second)
	h.poll(t)
	if h.mc.suspendCount(sb) != 0 {
		t.Fatal("parked early")
	}
	h.clk.advance(time.Second)
	h.poll(t)
	if h.mc.suspendCount(sb) != 1 || h.mc.phase(t, sb) != types.SandboxSuspended {
		t.Fatalf("not parked: %d %s", h.mc.suspendCount(sb), h.mc.phase(t, sb))
	}
	if h.mc.suspendCount(busy) != 0 {
		t.Fatal("parked a working session")
	}
	if d := describe(h.events()); d != "lifecycle main parked auto" {
		t.Fatalf("events: %q", d)
	}
	// Parked, it keeps reading blocked and is not exec'd again.
	got := h.status(t, sb)
	if !got.Parked || got.Record.State != events.StateBlocked || got.Stale {
		t.Fatalf("parked: %+v", got)
	}
	runs := h.mc.runCount(sb)
	h.clk.advance(time.Hour)
	h.poll(t)
	if h.mc.runCount(sb) != runs || h.mc.suspendCount(sb) != 1 {
		t.Fatal("parked sandbox exec'd or suspended again")
	}

	// Resumed (by a session command): a new idle window.
	if _, err := h.mc.ResumeSandbox(t.Context(), sb, nil); err != nil {
		t.Fatal(err)
	}
	h.poll(t)
	if h.mc.suspendCount(sb) != 1 {
		t.Fatal("parked again right after resuming")
	}
	if got := h.status(t, sb); got.Parked {
		t.Fatalf("still parked: %+v", got)
	}
	h.clk.advance(30 * time.Minute)
	h.poll(t)
	if h.mc.suspendCount(sb) != 2 {
		t.Fatal("not parked after a new window")
	}
}

// Attached clients, open bridges and working status hold a session off.
func TestMonitorAutoParkActivity(t *testing.T) {
	h := newMonitorHarness(t, nil)
	sb := h.mc.addSession(alice, "r", "main", types.SandboxReady)
	h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1", Status: st("idle", "", "")})
	h.poll(t)

	// Attached (e.g. a bridge before Sundae restarted, or another
	// client) at 20m: the window restarts.
	h.clk.advance(20 * time.Minute)
	h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1", Attached: 1, Status: st("idle", "", "")})
	h.poll(t)
	h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1", IdleSeconds: 1800, Status: st("idle", "", "")})
	h.clk.advance(29 * time.Minute)
	h.poll(t)
	if h.mc.suspendCount(sb) != 0 {
		t.Fatal("parked within the window after an attach")
	}

	// A bridge is open: never parked, however long.
	closeBridge := h.mon.OpenBridge(t.Context(), sb)
	h.clk.advance(2 * time.Hour)
	h.poll(t)
	if h.mc.suspendCount(sb) != 0 {
		t.Fatal("parked under an open bridge")
	}
	closeBridge()
	h.clk.advance(29 * time.Minute)
	h.poll(t)
	if h.mc.suspendCount(sb) != 0 {
		t.Fatal("parked within the window after the bridge closed")
	}

	// Working holds it off until the window after it has passed.
	h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1", IdleSeconds: 9999, Status: st("working", "", "")})
	h.clk.advance(time.Hour)
	h.poll(t)
	h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1", IdleSeconds: 9999, Status: st("done", "", "")})
	h.clk.advance(29 * time.Minute)
	h.poll(t)
	if h.mc.suspendCount(sb) != 0 {
		t.Fatal("parked within the window after working")
	}
	h.clk.advance(time.Minute)
	h.poll(t)
	if h.mc.suspendCount(sb) != 1 {
		t.Fatal("not parked")
	}
}

func TestMonitorSuspendDisabled(t *testing.T) {
	h := newMonitorHarness(t, func(c *config.Config) { c.Sessions.Suspend.Enabled = false })
	sb := h.mc.addSession(alice, "r", "main", types.SandboxReady)
	h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1", IdleSeconds: 99999})
	h.poll(t)
	h.clk.advance(48 * time.Hour)
	h.poll(t)
	if h.mc.suspendCount(sb) != 0 {
		t.Fatal("parked with suspend disabled")
	}
}

// A gateway that cannot suspend is asked once per sandbox, and logged once.
func TestMonitorSuspendUnsupported(t *testing.T) {
	h := newMonitorHarness(t, nil)
	// What the OpenShell 0.1.8 gateway answers without the
	// suspend-resume capability: FailedPrecondition, which the SDK
	// classifies as Conflict.
	h.mc.suspendErr = &types.StatusError{Code: types.ErrorConflict, GRPCCode: 9, Message: "compute driver does not support sandbox suspend"}
	a := h.mc.addSession(alice, "r", "a", types.SandboxReady)
	b := h.mc.addSession(alice, "r", "b", types.SandboxReady)
	for _, sb := range []string{a, b} {
		h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1", IdleSeconds: 99999})
	}
	h.poll(t)
	h.clk.advance(time.Hour)
	h.poll(t)
	h.clk.advance(time.Hour)
	h.poll(t)
	for _, sb := range []string{a, b} {
		if n := h.mc.suspendCount(sb); n != 1 {
			t.Fatalf("%s: suspend tried %d times", sb, n)
		}
		if got := h.status(t, sb); got.Unsuspendable == "" || got.Phase != string(types.SandboxReady) {
			t.Fatalf("%s: %+v", sb, got)
		}
	}
	if n := strings.Count(h.logs.String(), "cannot suspend sandboxes"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, h.logs)
	}
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("published %s", describe(evs))
	}
	// Manual parks are refused at once.
	if err := h.mon.Park(t.Context(), h.mc, a); !IsSuspendUnsupported(err) || h.mc.suspendCount(a) != 1 {
		t.Fatalf("manual park: %v", err)
	}

	// Another failure is not taken for unsupported: tried again next time.
	h2 := newMonitorHarness(t, nil)
	h2.mc.suspendErr = &types.StatusError{Code: types.ErrorUnavailable, Message: "try later"}
	sb := h2.mc.addSession(alice, "r", "a", types.SandboxReady)
	h2.mc.set(sb, "o1", osewaSessionInfo{ID: "o1", IdleSeconds: 99999})
	h2.poll(t)
	h2.clk.advance(time.Hour)
	h2.poll(t)
	h2.poll(t)
	if n := h2.mc.suspendCount(sb); n != 2 {
		t.Fatalf("transient failure: tried %d times", n)
	}
}

func TestIsSuspendUnsupported(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{ErrSuspendUnsupported, true},
		{fmt.Errorf("wrapped: %w", ErrSuspendUnsupported), true},
		{&types.StatusError{Code: types.ErrorUnimplemented, Message: "the Kubernetes compute driver does not support sandbox suspend"}, true},
		{fmt.Errorf("openshell: suspend: %w", &types.StatusError{Code: types.ErrorConflict, Message: "compute driver does not support sandbox suspend"}), true},
		{&types.StatusError{Code: types.ErrorConflict, Message: `sandbox "x" cannot move from Stopped to Suspended`}, false},
		{&types.StatusError{Code: types.ErrorUnavailable, Message: "not supported right now"}, false},
		{errors.New("does not support"), false},
		{nil, false},
	} {
		if got := IsSuspendUnsupported(tc.err); got != tc.want {
			t.Errorf("IsSuspendUnsupported(%v) = %v", tc.err, got)
		}
	}
}

func TestMonitorBridges(t *testing.T) {
	h := newMonitorHarness(t, nil)
	sb := h.mc.addSession(alice, "r", "main", types.SandboxReady)
	h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1"})
	h.poll(t)

	c1 := h.mon.OpenBridge(t.Context(), sb)
	c2 := h.mon.OpenBridge(t.Context(), sb)
	if n := h.mon.Bridges(sb); n != 2 {
		t.Fatalf("bridges = %d", n)
	}
	if got := h.status(t, sb); got.Bridges != 2 {
		t.Fatalf("status bridges = %d", got.Bridges)
	}
	if err := h.mon.Park(t.Context(), h.mc, sb); !errors.Is(err, ErrBridgeOpen) {
		t.Fatalf("park with a bridge: %v", err)
	}
	c1()
	c1() // idempotent
	if n := h.mon.Bridges(sb); n != 1 {
		t.Fatalf("bridges after close = %d", n)
	}
	c2()
	if n := h.mon.Bridges(sb); n != 0 {
		t.Fatalf("bridges after both = %d", n)
	}
	if err := h.mon.Park(t.Context(), h.mc, sb); err != nil || h.mc.phase(t, sb) != types.SandboxSuspended {
		t.Fatalf("park: %v", err)
	}

	var nilMon *Monitor
	nilMon.OpenBridge(t.Context(), sb)()
	if nilMon.Bridges(sb) != 0 || nilMon.ForUser(1) != nil {
		t.Fatal("nil monitor")
	}
}

// blockingSuspend holds SuspendSandbox until released.
type blockingSuspend struct {
	*monitorClient
	entered, release chan struct{}
}

func (b *blockingSuspend) SuspendSandbox(ctx context.Context, name string) (*Sandbox, error) {
	close(b.entered)
	<-b.release
	return b.monitorClient.SuspendSandbox(ctx, name)
}

// A command that opens a bridge while the session is being parked waits for
// the park to finish, then finds it parked (and resumes it).
func TestOpenBridgeWaitsForPark(t *testing.T) {
	h := newMonitorHarness(t, nil)
	sb := h.mc.addSession(alice, "r", "main", types.SandboxReady)
	h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1"})
	h.poll(t)

	bs := &blockingSuspend{monitorClient: h.mc, entered: make(chan struct{}), release: make(chan struct{})}
	parked := make(chan error, 1)
	go func() { parked <- h.mon.Park(t.Context(), bs, sb) }()
	<-bs.entered

	opened := make(chan func())
	go func() { opened <- h.mon.OpenBridge(t.Context(), sb) }()
	select {
	case <-opened:
		t.Fatal("OpenBridge did not wait for the park")
	default:
	}
	// A second park meanwhile is refused.
	if err := h.mon.Park(t.Context(), h.mc, sb); !errors.Is(err, ErrParkInProgress) && !errors.Is(err, ErrBridgeOpen) {
		t.Fatalf("concurrent park: %v", err)
	}
	close(bs.release)
	if err := <-parked; err != nil {
		t.Fatal(err)
	}
	closeBridge := <-opened
	defer closeBridge()
	if h.mc.phase(t, sb) != types.SandboxSuspended {
		t.Fatal("not parked")
	}
	if n := h.mon.Bridges(sb); n != 1 {
		t.Fatalf("bridges = %d", n)
	}
}

func TestMonitorReadAPI(t *testing.T) {
	h := newMonitorHarness(t, nil)
	a1 := h.mc.addSession(alice, "infra", "main", types.SandboxReady)
	a2 := h.mc.addSession(alice, "infra", "review", types.SandboxReady)
	a3 := h.mc.addSession(alice, "docs", "main", types.SandboxReady)
	b1 := h.mc.addSession(bob, "infra", "main", types.SandboxReady)
	for _, sb := range []string{a1, a2, a3, b1} {
		h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1", Status: st("idle", "", sb)})
	}
	h.poll(t)

	names := func(sts []SessionStatus) string {
		var s []string
		for _, st := range sts {
			s = append(s, st.Session.User+"/"+st.Session.Repo+"/"+st.Session.Name)
		}
		return strings.Join(s, ",")
	}
	if got := names(h.mon.ForUser(alice.id)); got != "alice/docs/main,alice/infra/main,alice/infra/review" {
		t.Fatalf("ForUser = %s", got)
	}
	if got := names(h.mon.ForRepo(alice.id, "infra")); got != "alice/infra/main,alice/infra/review" {
		t.Fatalf("ForRepo = %s", got)
	}
	if got := names(h.mon.All()); got != "alice/docs/main,alice/infra/main,alice/infra/review,bob/infra/main" {
		t.Fatalf("All = %s", got)
	}
	if st, ok := h.mon.Session(alice.id, "infra", ""); !ok || st.Session.Sandbox != a1 || st.Record.Msg != a1 {
		t.Fatalf("Session main = %+v %v", st, ok)
	}
	if st, ok := h.mon.Session(bob.id, "infra", "main"); !ok || st.Session.Sandbox != b1 {
		t.Fatalf("Session bob = %+v", st)
	}
	if _, ok := h.mon.Session(bob.id, "docs", "main"); ok {
		t.Fatal("found bob's docs session")
	}

	// Snapshots are copies.
	st, _ := h.mon.Status(a1)
	st.Record.Msg = "changed"
	if again, _ := h.mon.Status(a1); again.Record.Msg != a1 {
		t.Fatal("snapshot shares state")
	}

	ctx := ContextWithMonitor(context.Background(), h.mon)
	if MonitorFromContext(ctx) != h.mon || MonitorFromContext(context.Background()) != nil {
		t.Fatal("context")
	}
}
