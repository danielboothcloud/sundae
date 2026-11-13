package sessions

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/sessionctl"
	"github.com/danielboothcloud/sundae/pkg/signals"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/prompt"
)

var update = flag.Bool("update", false, "update golden files")

// now is the tests' clock.
var now = time.Date(2026, 11, 12, 14, 2, 30, 0, time.Local)

// fakeSource is an in-memory Source.
type fakeSource struct {
	mu       sync.Mutex
	rows     []sessionctl.Row
	listErr  error
	lists    int
	statuses map[string]openshell.SessionStatus
	snap     []signals.Signal
	feed     *fakeFeed
	repos    []string
	profiles []Profile
	repoProf map[string]string
	prepared []sessionctl.Request
	ended    []string
	parked   []string
	closed   int
	prepErr  error
	// prepErrs, when set, are returned by successive Prepares first.
	prepErrs []error
	// endErr is returned by End without force.
	endErr    error
	snapshots bool
	// gate, when set, holds Prepare after its progress lines.
	gate chan struct{}
}

func (f *fakeSource) List(context.Context) ([]sessionctl.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	return f.rows, f.listErr
}

func (f *fakeSource) Statuses() map[string]openshell.SessionStatus { return f.statuses }

func (f *fakeSource) Follow() ([]signals.Signal, Feed) {
	if f.feed == nil {
		f.feed = &fakeFeed{ch: make(chan []signals.Signal, 4)}
	}
	return f.snap, f.feed
}

func (f *fakeSource) Keep() int                               { return 1000 }
func (f *fakeSource) Repos(context.Context) ([]string, error) { return f.repos, nil }
func (f *fakeSource) Profiles() []Profile                     { return f.profiles }
func (f *fakeSource) RepoProfile(repo string) string          { return f.repoProf[repo] }
func (f *fakeSource) Command(profile string) string {
	for _, p := range f.profiles {
		if p.Name == profile {
			return p.Command
		}
	}
	return "bash"
}

func (f *fakeSource) Prepare(_ context.Context, req sessionctl.Request) (Attacher, error) {
	if req.Progress != nil {
		fmt.Fprintf(req.Progress, "Starting sandbox for %s...\nCloning %s...\n", req.Name, req.Repo) //nolint:errcheck
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared = append(f.prepared, req)
	if len(f.prepErrs) > 0 {
		err := f.prepErrs[0]
		f.prepErrs = f.prepErrs[1:]
		return nil, err
	}
	if f.prepErr != nil {
		return nil, f.prepErr
	}
	return &fakeAttacher{src: f, s: &openshell.Session{Repo: req.Repo, Name: req.Name, Sandbox: "sb-" + req.Repo + "-" + req.Name}}, nil
}

func (f *fakeSource) End(_ context.Context, repo, name string, force bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.endErr != nil && !force {
		return false, f.endErr
	}
	if force {
		f.ended = append(f.ended, repo+"/"+name+" (force)")
	} else {
		f.ended = append(f.ended, repo+"/"+name)
	}
	return true, nil
}

func (f *fakeSource) SnapshotsOnEnd() bool { return f.snapshots }

func (f *fakeSource) Park(_ context.Context, repo, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parked = append(f.parked, repo+"/"+name)
	return true, nil
}

func (f *fakeSource) AttachCommand(repo, name string) string {
	return sessionctl.AttachCommand("ssh://git.example.com", repo, name)
}

type fakeAttacher struct {
	src  *fakeSource
	s    *openshell.Session
	once sync.Once
	code int
	err  error
	term openshell.Terminal
}

func (a *fakeAttacher) Session() *openshell.Session { return a.s }
func (a *fakeAttacher) Attach(_ context.Context, term openshell.Terminal) (int, error) {
	a.term = term
	return a.code, a.err
}

func (a *fakeAttacher) Close() {
	a.once.Do(func() {
		a.src.mu.Lock()
		a.src.closed++
		a.src.mu.Unlock()
	})
}

type fakeFeed struct{ ch chan []signals.Signal }

func (f *fakeFeed) Next(ctx context.Context) ([]signals.Signal, error) {
	select {
	case s, ok := <-f.ch:
		if !ok {
			return nil, signals.ErrClosed
		}
		return s, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// row builds a session row.
func row(repo, name string, state events.State, changed time.Duration, msg string, opts ...func(*sessionctl.Row)) sessionctl.Row {
	r := sessionctl.Row{
		Info: openshell.Info{Repo: repo, Name: name, Sandbox: "sb-" + repo + "-" + name, Phase: "Ready"},
		Status: &openshell.SessionStatus{
			Record:  events.Record{State: state, Msg: msg},
			Changed: now.Add(-changed),
			Polled:  now,
		},
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func parked(r *sessionctl.Row)   { r.Info.Phase = "Suspended" }
func noStatus(r *sessionctl.Row) { r.Status = nil }
func kind(k events.BlockKind) func(*sessionctl.Row) {
	return func(r *sessionctl.Row) { r.Status.Record.Kind = k }
}

func progress(p int) func(*sessionctl.Row) {
	return func(r *sessionctl.Row) { r.Status.Record.Progress = &p }
}

func profile(p string) func(*sessionctl.Row) {
	return func(r *sessionctl.Row) { r.Info.Profile = p }
}

// mockRows are the sessions of tui-screens.md section 5, plus one with no
// status.
func mockRows() []sessionctl.Row {
	return []sessionctl.Row{
		row("dotfiles", "main", events.StateIdle, 3*time.Hour, "", parked),
		row("gateway", "main", events.StateWorking, 2*time.Minute, "", profile("claude")),
		row("infra", "claude-1", events.StateBlocked, 4*time.Minute, "Apply 3 to add, 1 to change, 0 to destroy?", kind(events.BlockPermission)),
		row("scratch", "shell", "", 0, "", noStatus),
		row("sundae", "forge-sync", events.StateDone, 12*time.Minute, "Implemented GitHub discovery; 14 tests pass"),
		row("sundae", "tui", events.StateWorking, 10*time.Second, "Running go test ./pkg/ui/...", progress(40)),
	}
}

type fakeHandoff struct {
	taken, released int
}

func (h *fakeHandoff) Take() (openshell.Terminal, func()) {
	h.taken++
	return nil, func() { h.released++ }
}

func newModel(t *testing.T, width, height int, src *fakeSource) *Sessions {
	t.Helper()
	c := common.NewCommon(context.Background(), width, height)
	m := New(c, src)
	m.now = func() time.Time { return now }
	m.SetSize(width, height)
	t.Cleanup(m.Close)
	return m
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		rows:     mockRows(),
		repos:    []string{"dotfiles", "gateway", "infra", "sundae"},
		profiles: []Profile{{Name: "claude", Image: "ghcr.io/me/agent-claude:latest", Command: "claude"}, {Name: "shell", Image: "ubuntu", Command: "bash"}},
		repoProf: map[string]string{"sundae": "claude"},
	}
}

// loaded returns a model that listed src's sessions.
func loaded(t *testing.T, width, height int, src *fakeSource) *Sessions {
	t.Helper()
	m := newModel(t, width, height, src)
	m.Update(listMsg{rows: src.rows})
	return m
}

func press(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	}
	r := []rune(k)
	return tea.KeyPressMsg{Code: r[0], Text: k}
}

// keys sends key presses and returns the last command.
func keys(m *Sessions, ks ...string) tea.Cmd {
	var cmd tea.Cmd
	for _, k := range ks {
		_, cmd = m.Update(press(k))
	}
	return cmd
}

// typeText types s into the model.
func typeText(m *Sessions, s string) {
	for _, r := range s {
		m.Update(press(string(r)))
	}
}

// run runs cmd and feeds what it returns to the model, following batches,
// for up to depth rounds. Commands that block (the signals feed) are left
// after a short wait.
func run(m *Sessions, cmd tea.Cmd, depth int) {
	if cmd == nil || depth == 0 {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(200 * time.Millisecond):
		return
	}
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			run(m, c, depth)
		}
	default:
		_, next := m.Update(msg)
		run(m, next, depth-1)
	}
}

func view(m *Sessions) string {
	lines := strings.Split(ansi.Strip(m.View()), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got+"\n"), 0o644); err != nil { //nolint:gosec
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got+"\n" != string(want) {
		t.Fatalf("%s differs from %s:\n%s\n--- want ---\n%s", name, path, got, want)
	}
}

func visibleKeys(m *Sessions) []string {
	var out []string
	for _, r := range m.visibleRows() {
		out = append(out, r.Info.Repo+"/"+r.Info.Name)
	}
	return out
}

func TestSortOrder(t *testing.T) {
	m := loaded(t, 76, 30, newFakeSource())
	got := strings.Join(visibleKeys(m), " ")
	want := "infra/claude-1 sundae/forge-sync sundae/tui gateway/main dotfiles/main scratch/shell"
	if got != want {
		t.Fatalf("order:\n%s\nwant:\n%s", got, want)
	}
}

func TestSelectionFollowsTheSessionAcrossRefresh(t *testing.T) {
	src := newFakeSource()
	m := loaded(t, 76, 30, src)
	keys(m, "j", "j")
	if r, _ := m.selected(); r.Info.Name != "tui" {
		t.Fatalf("selected %s", r.Info.Name)
	}
	// tui becomes blocked and moves to the top: the cursor goes with it.
	rows := mockRows()
	rows[5].Status.Record.State = events.StateBlocked
	rows[5].Status.Changed = now
	m.Update(listMsg{rows: rows})
	if r, _ := m.selected(); r.Info.Name != "tui" || m.cursor != 0 {
		t.Fatalf("selected %s at %d", r.Info.Name, m.cursor)
	}
}

func TestSessionsGolden(t *testing.T) {
	for _, w := range []int{80, 50} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			// The tab gets the terminal width less the frame's margins.
			m := loaded(t, w-4, 22, newFakeSource())
			keys(m, "j")
			golden(t, fmt.Sprintf("sessions-%d", w), view(m))
		})
	}
}

func TestSignalsGolden(t *testing.T) {
	for _, w := range []int{80, 50} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			src := newFakeSource()
			src.snap = mockSignals()
			m := loaded(t, w-4, 14, src)
			snap, feed := src.Follow()
			m.Update(followMsg{snap: snap, feed: feed})
			keys(m, "e")
			golden(t, fmt.Sprintf("signals-%d", w), view(m))
		})
	}
}

func TestStatesGolden(t *testing.T) {
	src := newFakeSource()
	m := newModel(t, 76, 8, src)
	if got := view(m); !strings.Contains(got, "Loading sessions…") {
		t.Fatalf("loading:\n%s", got)
	}
	m.Update(listMsg{err: errors.New("dial tcp 127.0.0.1:1: connect: connection refused")})
	golden(t, "gateway-down-80", view(m))
	if m.Summary(false) != "" {
		t.Fatal("summary shown while the gateway is down")
	}
	if h := m.ShortHelp(); len(h) == 0 || h[0].Help().Key != "r" {
		t.Fatalf("help while down: %v", h)
	}
	// r retries.
	src.listErr = nil
	src.rows = nil
	run(m, keys(m, "r"), 2)
	golden(t, "empty-80", view(m))

	n := newModel(t, 46, 8, src)
	n.Update(listMsg{err: errors.New("connection refused")})
	golden(t, "gateway-down-50", view(n))
}

func TestFilterSessions(t *testing.T) {
	m := loaded(t, 76, 30, newFakeSource())
	keys(m, "/")
	if !m.Capturing() {
		t.Fatal("not capturing while typing the filter")
	}
	typeText(m, "sundae")
	if got := strings.Join(visibleKeys(m), " "); got != "sundae/forge-sync sundae/tui" {
		t.Fatalf("filtered: %s", got)
	}
	keys(m, "enter")
	if m.Capturing() {
		t.Fatal("still capturing after enter")
	}
	if !strings.Contains(view(m), "/sundae · 2 of 6") {
		t.Fatalf("filter not shown:\n%s", view(m))
	}
	// The filter also matches the message.
	keys(m, "/")
	for range 6 {
		keys(m, "backspace")
	}
	typeText(m, "destroy")
	if got := strings.Join(visibleKeys(m), " "); got != "infra/claude-1" {
		t.Fatalf("filtered by message: %s", got)
	}
	keys(m, "esc")
	if len(m.visibleRows()) != 6 || m.Capturing() {
		t.Fatal("esc did not clear the filter")
	}
	keys(m, "/")
	typeText(m, "nothing")
	keys(m, "enter")
	if !strings.Contains(view(m), "No sessions match") {
		t.Fatalf("no match:\n%s", view(m))
	}
	keys(m, "esc")
	if len(m.visibleRows()) != 6 {
		t.Fatal("esc did not clear the applied filter")
	}
}

func mockSignals() []signals.Signal {
	at := func(d time.Duration) time.Time { return now.Add(-d) }
	sess := func(repo, name string) events.Session {
		return events.Session{Repo: repo, Name: name, Sandbox: "sb-" + repo + "-" + name}
	}
	status := func(id uint64, d time.Duration, repo, name string, st events.State, msg string) signals.Signal {
		ev := events.SessionStatusEvent(7, events.SessionStatus{Session: sess(repo, name), Record: events.Record{State: st, Msg: msg}})
		ev.ID, ev.Time = id, at(d)
		return signals.Signal{Event: ev}
	}
	life := func(id uint64, d time.Duration, repo, name string, step events.Lifecycle) signals.Signal {
		ev := events.SessionLifecycleEvent(7, events.SessionLifecycle{Session: sess(repo, name), Step: step})
		ev.ID, ev.Time = id, at(d)
		return signals.Signal{Event: ev, Last: events.StateIdle}
	}
	blocked := status(9, 19*time.Second, "infra", "claude-1", events.StateBlocked, "Apply 3 to add, 1 to change, 0 to destroy?")
	blocked.Status.Record.Kind = events.BlockPermission
	gap := signals.Signal{Event: events.Event{ID: 5, Kind: signals.KindGap, Time: at(30 * time.Minute)}, Gap: &signals.Gap{Lost: 2, Reason: signals.GapDropped}}
	// Newest first, as Follow returns them.
	return []signals.Signal{
		blocked,
		status(8, 27*time.Second, "infra", "claude-1", events.StateWorking, "Planning"),
		status(7, 4*time.Minute, "sundae", "forge-sync", events.StateDone, "Implemented GitHub discovery"),
		status(6, 21*time.Minute, "sundae", "tui", events.StateWorking, "Running go test ./pkg/ui/..."),
		gap,
		life(4, 50*time.Minute, "dotfiles", "main", events.LifecycleParked),
		life(3, 72*time.Minute, "sundae", "tui", events.LifecycleCreated),
		life(2, 80*time.Minute, "old", "gone", events.LifecycleEnded),
	}
}

func sigIDs(m *Sessions) []uint64 {
	var ids []uint64
	for _, s := range m.visibleSignals() {
		ids = append(ids, s.ID)
	}
	return ids
}

func newSignal(id uint64, repo, name string, st events.State) signals.Signal {
	ev := events.SessionStatusEvent(7, events.SessionStatus{
		Session: events.Session{Repo: repo, Name: name, Sandbox: "sb-" + repo + "-" + name},
		Record:  events.Record{State: st},
	})
	ev.ID, ev.Time = id, now
	return signals.Signal{Event: ev}
}

func TestSignalsFollowAndPause(t *testing.T) {
	src := newFakeSource()
	src.snap = mockSignals()
	m := loaded(t, 76, 30, src)
	snap, feed := src.Follow()
	m.Update(followMsg{snap: snap, feed: feed})
	keys(m, "e")
	if ids := sigIDs(m); ids[0] != 9 || len(ids) != 8 {
		t.Fatalf("snapshot order %v", ids)
	}
	if !strings.Contains(view(m), "following · 8 kept") {
		t.Fatalf("not following:\n%s", view(m))
	}

	// Following: new signals show at the top, the cursor stays on it.
	m.Update(signalsMsg{sigs: []signals.Signal{newSignal(10, "infra", "claude-1", events.StateWorking), newSignal(11, "infra", "claude-1", events.StateDone)}})
	if m.sigCursor != 0 || m.visibleSignals()[0].ID != 11 {
		t.Fatalf("cursor %d, top %d", m.sigCursor, m.visibleSignals()[0].ID)
	}

	// Moving down pauses: the cursor stays on its signal.
	keys(m, "j", "j")
	on := m.visibleSignals()[m.sigCursor].ID
	m.Update(signalsMsg{sigs: []signals.Signal{newSignal(12, "infra", "claude-1", events.StateIdle)}})
	if got := m.visibleSignals()[m.sigCursor].ID; got != on {
		t.Fatalf("cursor moved from %d to %d while paused", on, got)
	}
	if !strings.Contains(view(m), "paused · 1 new") {
		t.Fatalf("not paused:\n%s", view(m))
	}

	// g goes back to the top and follows again.
	keys(m, "g")
	if m.sigCursor != 0 || !strings.Contains(view(m), "following") {
		t.Fatalf("g did not resume following: cursor %d", m.sigCursor)
	}
}

func TestSignalsAttentionAndFilter(t *testing.T) {
	src := newFakeSource()
	src.snap = mockSignals()
	m := loaded(t, 76, 30, src)
	m.Update(followMsg{snap: src.snap, feed: &fakeFeed{ch: make(chan []signals.Signal)}})
	keys(m, "e", "a")
	// blocked, done, and the gap marker.
	if got := fmt.Sprint(sigIDs(m)); got != "[9 7 5]" {
		t.Fatalf("attention only: %s", got)
	}
	if !strings.Contains(view(m), "attention only · 3 of 8") {
		t.Fatalf("header:\n%s", view(m))
	}
	keys(m, "a", "/")
	typeText(m, "sundae")
	keys(m, "enter")
	if got := fmt.Sprint(sigIDs(m)); got != "[7 6 5 3]" {
		t.Fatalf("filtered: %s (gap markers always show)", got)
	}
}

func TestReactToSignals(t *testing.T) {
	src := newFakeSource()
	m := loaded(t, 76, 30, src)
	// A status change of a listed session re-reads the Monitor only.
	st := *src.rows[0].Status
	st.Record.State = events.StateError
	src.statuses = map[string]openshell.SessionStatus{"sb-dotfiles-main": st}
	lists := src.lists
	_, cmd := m.Update(signalsMsg{sigs: []signals.Signal{newSignal(1, "dotfiles", "main", events.StateError)}})
	run(m, cmd, 1)
	if src.lists != lists {
		t.Fatal("a status change listed the sessions from the gateway")
	}
	if got := visibleKeys(m)[1]; got != "dotfiles/main" {
		t.Fatalf("dotfiles did not move up as error: %v", visibleKeys(m))
	}
	// A lifecycle step lists them again.
	ev := events.SessionLifecycleEvent(7, events.SessionLifecycle{Session: events.Session{Repo: "r", Name: "new", Sandbox: "sb-r-new"}, Step: events.LifecycleCreated})
	_, cmd = m.Update(signalsMsg{sigs: []signals.Signal{{Event: ev}}})
	run(m, cmd, 1)
	if src.lists != lists+1 {
		t.Fatalf("lists %d, want %d", src.lists, lists+1)
	}
}

func TestLiveFeedEndsOnClose(t *testing.T) {
	src := newFakeSource()
	m := newModel(t, 76, 30, src)
	snap, feed := src.Follow()
	_, cmd := m.Update(followMsg{snap: snap, feed: feed})
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	src.feed.ch <- []signals.Signal{newSignal(1, "infra", "claude-1", events.StateDone)}
	msg := <-done
	if _, ok := msg.(signalsMsg); !ok {
		t.Fatalf("got %T", msg)
	}
	_, cmd = m.Update(msg)
	// The next wait ends when the tab closes (quit, or the SSH connection
	// ending): no goroutine is left waiting on the feed.
	ended := make(chan feedEndMsg, 1)
	for _, c := range cmd().(tea.BatchMsg) {
		if c != nil {
			go func() {
				if fe, ok := c().(feedEndMsg); ok {
					ended <- fe
				}
			}()
		}
	}
	time.Sleep(20 * time.Millisecond)
	m.Close()
	select {
	case fe := <-ended:
		if !errors.Is(fe.err, context.Canceled) {
			t.Fatalf("feed ended with %v", fe.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the feed wait did not end on Close")
	}
}

func TestEndConfirm(t *testing.T) {
	src := newFakeSource()
	m := loaded(t, 76, 30, src)
	keys(m, "x")
	if !m.Capturing() {
		t.Fatal("the confirm does not capture keys")
	}
	line := ansi.Strip(m.FooterLine(76))
	if !strings.HasPrefix(line, "End infra / claude-1?  The sandbox and any unpushed work are") || !strings.HasSuffix(line, "y / N") {
		t.Fatalf("confirm line %q", line)
	}
	keys(m, "n")
	if m.confirm != nil || len(src.ended) != 0 {
		t.Fatal("n did not cancel")
	}
	cmd := keys(m, "x", "y")
	if m.words(m.rows[0]) != "blocked · permission · ending" {
		t.Fatalf("busy words %q", m.words(m.rows[0]))
	}
	run(m, cmd, 2)
	if fmt.Sprint(src.ended) != "[infra/claude-1]" {
		t.Fatalf("ended %v", src.ended)
	}
	if !strings.Contains(view(m), "Ended infra / claude-1.") {
		t.Fatalf("notice:\n%s", view(m))
	}
}

func TestParkAndResume(t *testing.T) {
	src := newFakeSource()
	m := loaded(t, 76, 30, src)
	run(m, keys(m, "z"), 1) // infra / claude-1 is running: park it
	if fmt.Sprint(src.parked) != "[infra/claude-1]" {
		t.Fatalf("parked %v", src.parked)
	}
	keys(m, "G") // dotfiles / main is parked: resume it, without attaching
	keys(m, "k")
	if r, _ := m.selected(); !r.Parked() {
		t.Fatalf("selected %s", r.Info.Repo)
	}
	run(m, keys(m, "z"), 1)
	if len(src.prepared) != 1 || src.prepared[0].Create || src.prepared[0].Repo != "dotfiles" || src.closed != 1 {
		t.Fatalf("prepared %+v, closed %d", src.prepared, src.closed)
	}
	if !strings.Contains(view(m), "Resumed dotfiles / main.") {
		t.Fatalf("notice:\n%s", view(m))
	}
}

func TestCopyAttachCommand(t *testing.T) {
	m := loaded(t, 76, 30, newFakeSource())
	cmd := keys(m, "c")
	if cmd == nil {
		t.Fatal("no clipboard command")
	}
	if got := fmt.Sprint(cmd()); !strings.Contains(got, "ssh -t git.example.com session attach infra -n claude-1") {
		t.Fatalf("clipboard %q", got)
	}
	if !strings.Contains(view(m), "(copied to clipboard)") {
		t.Fatalf("view:\n%s", view(m))
	}
	keys(m, "j")
	if strings.Contains(view(m), "(copied to clipboard)") {
		t.Fatal("copied stays after the next key")
	}
}

func TestPaneSwitch(t *testing.T) {
	m := loaded(t, 76, 30, newFakeSource())
	keys(m, "e")
	if m.pane != signalsPane || !strings.Contains(view(m), "‹ Signals ›") {
		t.Fatal("e did not switch to Signals")
	}
	if h := m.ShortHelp(); h[2].Help().Desc != "sessions" {
		t.Fatalf("help %v", h[2].Help())
	}
	keys(m, "e")
	if m.pane != sessionsPane {
		t.Fatal("e did not switch back")
	}
}

func TestSummary(t *testing.T) {
	m := loaded(t, 76, 30, newFakeSource())
	if got := ansi.Strip(m.Summary(false)); got != "▲ 1 blocked  ✓ 1 done  ● 2 working" {
		t.Fatalf("summary %q", got)
	}
	if got := ansi.Strip(m.Summary(true)); got != "▲1 ✓1 ●2" {
		t.Fatalf("narrow summary %q", got)
	}
	m.Update(listMsg{rows: nil})
	if got := m.Summary(false); got != "" {
		t.Fatalf("summary without sessions %q", got)
	}
}

func TestNewSessionForm(t *testing.T) {
	src := newFakeSource()
	m := loaded(t, 76, 30, src)
	keys(m, "j") // sundae / forge-sync
	run(m, keys(m, "n"), 1)
	if m.form == nil || !m.Capturing() {
		t.Fatal("n did not open the form")
	}
	f := m.form
	if f.Field("repo").Value() != "sundae" || f.Field("profile").Value() != "claude" || f.Field("name").Value() != "claude-1" {
		t.Fatalf("defaults: %s %s %s", f.Field("repo").Value(), f.Field("profile").Value(), f.Field("name").Value())
	}
	golden(t, "new-session-80", view(m))

	// Changing the profile renames an untouched name.
	keys(m, "down", "down", "right")
	if f.Field("profile").Value() != "shell" || f.Field("name").Value() != "shell-1" {
		t.Fatalf("after profile change: %s %s", f.Field("profile").Value(), f.Field("name").Value())
	}
	// An invalid name is refused.
	keys(m, "up")
	for range len("shell-1") {
		keys(m, "backspace")
	}
	typeText(m, "Bad")
	keys(m, "enter")
	if m.form == nil || !strings.Contains(view(m), "session name must be") {
		t.Fatalf("invalid name accepted:\n%s", view(m))
	}
	// An existing one too.
	for range 3 {
		keys(m, "backspace")
	}
	typeText(m, "tui")
	keys(m, "enter")
	if m.form == nil || !strings.Contains(view(m), "sundae / tui already exists") {
		t.Fatalf("existing name accepted:\n%s", view(m))
	}
	keys(m, "backspace")
	typeText(m, "2")
	// Do not attach: start in the background.
	keys(m, "down", "down", "left")
	cmd := keys(m, "enter")
	if m.form != nil || m.op == nil {
		t.Fatal("submit did not start the session")
	}
	run(m, cmd, 3)
	if len(src.prepared) != 1 {
		t.Fatalf("prepared %+v", src.prepared)
	}
	req := src.prepared[0]
	if !req.Create || req.Repo != "sundae" || req.Name != "tu2" || req.Profile != "shell" {
		t.Fatalf("request %+v", req)
	}
	if m.op != nil || src.closed != 1 || !strings.Contains(view(m), "Started sundae / tu2.") {
		t.Fatalf("op %v closed %d:\n%s", m.op, src.closed, view(m))
	}

	// esc cancels the form.
	run(m, keys(m, "n"), 1)
	keys(m, "esc")
	if m.form != nil {
		t.Fatal("esc did not cancel")
	}
}

func TestNewSessionFormRepoPicker(t *testing.T) {
	src := newFakeSource()
	m := loaded(t, 76, 30, src)
	run(m, keys(m, "n"), 1)
	f := m.form
	if f.Focused().Key != "repo" {
		t.Fatalf("focused %s", f.Focused().Key)
	}
	typeText(m, "sundae")
	if f.Field("repo").Value() != "sundae" || f.Field("profile").Value() != "claude" || f.Field("name").Value() != "claude-1" {
		t.Fatalf("picked %s %s %s", f.Field("repo").Value(), f.Field("profile").Value(), f.Field("name").Value())
	}
}

func TestStartAndAttach(t *testing.T) {
	src := newFakeSource()
	m := loaded(t, 76, 30, src)
	h := &fakeHandoff{}
	m.handoff = h
	src.gate = make(chan struct{})
	run(m, keys(m, "n"), 1)
	cmd := keys(m, "enter")
	if !strings.Contains(view(m), "Starting infra / claude-2 …") {
		t.Fatalf("progress:\n%s", view(m))
	}
	// The progress lines show in place.
	_, cmd = m.Update(cmd())
	waitFor(t, func() bool {
		lines, _, _, _ := m.op.state()
		return len(lines) == 2
	})
	if len(m.opLines) < 2 {
		_, cmd = m.Update(cmd())
	}
	if !strings.Contains(view(m), "Cloning infra...") {
		t.Fatalf("progress lines:\n%s", view(m))
	}
	// Prepare is done: the terminal is handed over through tea.Exec.
	close(src.gate)
	for m.op != nil {
		_, cmd = m.Update(cmd())
	}
	if h.taken != 1 || cmd == nil || src.prepared[0].Name != "claude-2" {
		t.Fatalf("taken %d, prepared %+v", h.taken, src.prepared)
	}
}

func TestEnterAttachesFromBothPanes(t *testing.T) {
	src := newFakeSource()
	src.snap = mockSignals()
	m := loaded(t, 76, 30, src)
	m.handoff = &fakeHandoff{}
	keys(m, "G")
	cmd := keys(m, "k", "enter")
	if m.op == nil || !strings.Contains(view(m), "Attaching to dotfiles / main …  detach with ~.") || !strings.Contains(view(m), "Resuming…") {
		t.Fatalf("attach of a parked session:\n%s", view(m))
	}
	run(m, cmd, 1)
	if len(src.prepared) != 1 || src.prepared[0].Create || src.prepared[0].Repo != "dotfiles" {
		t.Fatalf("prepared %+v", src.prepared)
	}
	if m.op != nil || m.handoff.(*fakeHandoff).taken != 1 {
		t.Fatal("the prepared session was not attached")
	}

	// From the Signals pane: a listed session attaches...
	m.Update(followMsg{snap: src.snap, feed: &fakeFeed{ch: make(chan []signals.Signal)}})
	keys(m, "e", "g")
	keys(m, "enter")
	if m.op == nil || m.op.repo != "infra" || m.op.name != "claude-1" {
		t.Fatalf("op %+v", m.op)
	}
	m.op.abandon()
	m.op = nil
	// ...an ended one opens the form for it.
	keys(m, "G")
	run(m, keys(m, "enter"), 1)
	if m.form == nil {
		t.Fatal("no form for the ended session")
	}
}

func TestAttachWithoutTerminal(t *testing.T) {
	m := loaded(t, 76, 30, newFakeSource())
	keys(m, "enter")
	if m.op != nil || !strings.Contains(view(m), "attaching needs the SSH terminal") {
		t.Fatalf("view:\n%s", view(m))
	}
}

func TestAttachExec(t *testing.T) {
	src := newFakeSource()
	att := &fakeAttacher{src: src, code: 3}
	released := 0
	ex := &attachExec{ctx: context.Background(), att: att, term: nopTerm{}, release: func() { released++ }, banner: "Attaching"}
	if err := ex.Run(); err != nil {
		t.Fatal(err)
	}
	if ex.code != 3 || released != 1 || src.closed != 1 {
		t.Fatalf("code %d released %d closed %d", ex.code, released, src.closed)
	}
	m := loaded(t, 76, 30, src)
	m.Update(attachDoneMsg{repo: "infra", name: "claude-1", code: 3})
	if !strings.Contains(view(m), "infra / claude-1: the program exited with status 3.") {
		t.Fatalf("view:\n%s", view(m))
	}
	m.Update(attachDoneMsg{repo: "infra", name: "claude-1"})
	if !strings.Contains(view(m), "Detached from infra / claude-1.") {
		t.Fatalf("view:\n%s", view(m))
	}
}

func TestOperationAbandonClosesAttacher(t *testing.T) {
	src := newFakeSource()
	ctx, cancel := context.WithCancel(context.Background())
	op, opCtx := newOperation(ctx, "t", "r", "n", true)
	cmd := op.run(opCtx, src, sessionctl.Request{Repo: "r", Name: "n"})
	cmd() // first progress or done
	// The TUI goes away before it took the Attacher.
	cancel()
	waitFor(t, func() bool {
		src.mu.Lock()
		defer src.mu.Unlock()
		return src.closed == 1
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type nopTerm struct{}

func (nopTerm) Read([]byte) (int, error)    { return 0, nil }
func (nopTerm) Write(p []byte) (int, error) { return len(p), nil }
func (nopTerm) Pty() (ssh.Pty, <-chan ssh.Window, bool) {
	return ssh.Pty{}, nil, true
}

// TestScopeGolden is the repository page's Sessions tab (section 8): the
// sessions of one repository, without the repository prefix.
func TestScopeGolden(t *testing.T) {
	for _, w := range []int{80, 50} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			src := newFakeSource()
			src.snap = mockSignals()
			m := loaded(t, w, 12, src)
			m.Update(followMsg{snap: src.snap, feed: &fakeFeed{ch: make(chan []signals.Signal)}})
			m.SetScope("sundae")
			golden(t, fmt.Sprintf("repo-sessions-%d", w), view(m))
			keys(m, "e")
			golden(t, fmt.Sprintf("repo-signals-%d", w), view(m))
		})
	}
}

func TestScopeKeys(t *testing.T) {
	src := newFakeSource()
	m := loaded(t, 80, 20, src)
	// The home tab's filter is kept while a repository is shown.
	keys(m, "/")
	typeText(m, "infra")
	keys(m, "enter")
	m.SetScope("sundae")
	if got := strings.Join(visibleKeys(m), " "); got != "sundae/forge-sync sundae/tui" {
		t.Fatalf("scoped rows %s", got)
	}
	if name, i, n := m.SelectedName(); name != "forge-sync" || i != 1 || n != 2 {
		t.Fatalf("SelectedName = %s %d %d", name, i, n)
	}
	// n opens the form for this repository, fixed.
	run(m, keys(m, "n"), 1)
	if m.form == nil || m.form.Field("repo").Kind() != prompt.Fixed || m.form.Field("repo").Value() != "sundae" {
		t.Fatal("n did not open the form for the repository")
	}
	keys(m, "esc")
	// x asks in the tab: the repository page's footer is the help.
	keys(m, "j", "x")
	if m.FooterLine(80) != "" || !strings.Contains(view(m), "End sundae / tui?") || !strings.Contains(view(m), "y / N") {
		t.Fatalf("confirm:\n%s", view(m))
	}
	run(m, keys(m, "y"), 2)
	if fmt.Sprint(src.ended) != "[sundae/tui]" {
		t.Fatalf("ended %v", src.ended)
	}
	// A filter in the repository is its own; esc clears it.
	keys(m, "/")
	typeText(m, "forge")
	keys(m, "enter")
	if !m.Filtered() {
		t.Fatal("not filtered")
	}
	m.SetScope("")
	if m.rowQuery != "infra" || len(visibleKeys(m)) != 1 {
		t.Fatalf("home filter lost: %q %v", m.rowQuery, visibleKeys(m))
	}
	m.SetScope("sundae")
	if m.rowQuery != "forge" {
		t.Fatalf("repository filter lost: %q", m.rowQuery)
	}
	keys(m, "esc")
	if m.Filtered() {
		t.Fatal("esc did not clear the filter")
	}
	// A repository without sessions says so.
	m.SetScope("gateway-2")
	if !strings.Contains(view(m), "No sessions of this repository. Press n to start one.") {
		t.Fatalf("empty:\n%s", view(m))
	}
	// The header summary still counts every session.
	if got := ansi.Strip(m.Summary(true)); got != "▲1 ✓1 ●2" {
		t.Fatalf("summary %q", got)
	}
}
