package ssh

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/db"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/sessionctl"
	"github.com/danielboothcloud/sundae/pkg/store"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/sessions"
)

var updateGolden = flag.Bool("update", false, "update golden files")

// frameRows are a few sessions: one blocked, one done, two working.
func frameRows() []sessionctl.Row {
	st := func(s events.State, msg string) *openshell.SessionStatus {
		return &openshell.SessionStatus{Record: events.Record{State: s, Msg: msg}, Changed: time.Now().Add(-5 * time.Minute)}
	}
	info := func(repo, name string) openshell.Info {
		return openshell.Info{Repo: repo, Name: name, Sandbox: "sb-" + repo + "-" + name, Phase: "Ready"}
	}
	return []sessionctl.Row{
		{Info: info("infra", "claude-1"), Status: st(events.StateBlocked, "Apply 3 to add, 1 to change, 0 to destroy?")},
		{Info: info("sundae", "forge-sync"), Status: st(events.StateDone, "Implemented GitHub discovery")},
		{Info: info("sundae", "tui"), Status: st(events.StateWorking, "Running go test ./pkg/ui/...")},
		{Info: info("gateway", "main"), Status: st(events.StateWorking, "")},
		{Info: info("dotfiles", "main"), Status: st(events.StateIdle, "")},
	}
}

// newTestUI returns a started home page TUI of width x height, with src
// as the Sessions tab's data (nil: no Sessions tab).
func newTestUI(t *testing.T, width, height int, src sessions.Source) *UI {
	t.Helper()
	return newHomeUI(t, width, height, func(ctx context.Context, _ *backend.Backend) context.Context {
		if src != nil {
			ctx = context.WithValue(ctx, sessions.SourceKey, src)
		}
		return ctx
	})
}

// newHomeUI returns a started home page TUI of width x height; setup
// adds to its context (the tabs' sources) and to the backend.
func newHomeUI(t *testing.T, width, height int, setup func(ctx context.Context, be *backend.Backend) context.Context) *UI {
	t.Helper()
	cfg, dbx, datastore, be := newTestBackend(t)
	cfg.Name = "Sundae"
	// As the SSH server's ContextMiddleware: backend, config, db and store.
	ctx := backend.WithContext(config.WithContext(context.Background(), cfg), be)
	ctx = context.WithValue(ctx, db.ContextKey, dbx)
	ctx = context.WithValue(ctx, store.ContextKey, datastore)
	ctx = setup(ctx, be)
	c := common.NewCommon(ctx, width, height)
	c.SetValue(common.ConfigKey, cfg)
	ui := NewUI(c, "")
	t.Cleanup(ui.close)
	runUI(ui, ui.Init(), 4)
	_, cmd := ui.Update(tea.WindowSizeMsg{Width: width, Height: height})
	runUI(ui, cmd, 4)
	return ui
}

// runUI runs cmd and feeds its messages to the UI, following batches, up
// to depth rounds; commands that block (the signals feed) are left.
func runUI(ui *UI, cmd tea.Cmd, depth int) {
	if cmd == nil || depth == 0 {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(100 * time.Millisecond):
		// Keep what it returns later (a live update), for drainLate.
		late := lateFor(ui)
		go func() {
			if m := <-done; m != nil {
				late <- m
			}
		}()
		return
	}
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			runUI(ui, c, depth)
		}
	default:
		_, next := ui.Update(msg)
		runUI(ui, next, depth-1)
	}
}

var (
	lateMu   sync.Mutex
	lateMsgs = map[*UI]chan tea.Msg{}
)

// lateFor returns where the messages of the UI's commands that took
// longer than runUI waits go.
func lateFor(ui *UI) chan tea.Msg {
	lateMu.Lock()
	defer lateMu.Unlock()
	ch, ok := lateMsgs[ui]
	if !ok {
		ch = make(chan tea.Msg, 64)
		lateMsgs[ui] = ch
	}
	return ch
}

// drainLate feeds the UI the late messages that arrived.
func drainLate(ui *UI) {
	ch := lateFor(ui)
	for {
		select {
		case msg := <-ch:
			_, next := ui.Update(msg)
			runUI(ui, next, 3)
		default:
			return
		}
	}
}

func uiKey(ui *UI, keys ...string) { uiKeyDepth(ui, 2, keys...) }

// uiKeyDepth presses keys, following what each starts depth rounds.
func uiKeyDepth(ui *UI, depth int, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		default:
			r := []rune(k)
			msg = tea.KeyPressMsg{Code: r[0], Text: k}
		}
		_, cmd := ui.Update(msg)
		runUI(ui, cmd, depth)
	}
}

func screenText(ui *UI) string {
	lines := strings.Split(ansi.Strip(ui.View().Content), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

func goldenFile(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *updateGolden {
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

func TestHomeFrame(t *testing.T) {
	for _, w := range []int{80, 50} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			ui := newTestUI(t, w, 24, &fakeSource{rows: frameRows()})
			home := screenText(ui)
			summary := "▲ 1 blocked  ✓ 1 done  ● 2 working"
			if w < 60 {
				summary = "▲1 ✓1 ●2"
			}
			if !strings.Contains(home, summary) {
				t.Fatalf("no summary %q:\n%s", summary, home)
			}
			uiKey(ui, "2")
			goldenFile(t, fmt.Sprintf("home-sessions-%d", w), screenText(ui))
			uiKey(ui, "3")
			if s := screenText(ui); !strings.Contains(s, "No readme found") {
				t.Fatalf("3 did not open About:\n%s", s)
			}
			uiKey(ui, "1")
			if s := screenText(ui); !strings.Contains(s, "No items") && !strings.Contains(s, "• Repositories") && !strings.Contains(s, "‹ Repositories ›") {
				t.Fatalf("1 did not open Repositories:\n%s", s)
			}
		})
	}
}

func TestHomeWithoutSessions(t *testing.T) {
	ui := newTestUI(t, 80, 24, nil)
	s := screenText(ui)
	if strings.Contains(s, "Sessions") || !strings.Contains(s, "About") {
		t.Fatalf("tabs:\n%s", s)
	}
	if strings.Contains(s, "blocked") {
		t.Fatalf("summary without sessions:\n%s", s)
	}
	uiKey(ui, "2")
	if s := screenText(ui); !strings.Contains(s, "No readme found") {
		t.Fatalf("2 did not open About:\n%s", s)
	}
	if !strings.Contains(s, "1-2 tab") {
		t.Fatalf("footer:\n%s", s)
	}
}

func TestHomeSessionsKeys(t *testing.T) {
	ui := newTestUI(t, 80, 24, &fakeSource{rows: frameRows()})
	uiKey(ui, "2")
	if s := screenText(ui); !strings.Contains(s, "1-3 tab") || !strings.Contains(s, "enter attach") {
		t.Fatalf("footer:\n%s", s)
	}
	// While the filter is typed, q, ? and the digits are text.
	uiKey(ui, "/", "q", "?", "1")
	if s := screenText(ui); !strings.Contains(s, "/ q?1") || !strings.Contains(s, "No sessions match") {
		t.Fatalf("filter:\n%s", s)
	}
	uiKey(ui, "esc")
	// The end confirm takes the footer's place.
	uiKey(ui, "x")
	s := screenText(ui)
	last := s[strings.LastIndex(s, "\n")+1:]
	if !strings.Contains(last, "End infra / claude-1?") || !strings.HasSuffix(last, "y / N") {
		t.Fatalf("confirm line %q:\n%s", last, s)
	}
	uiKey(ui, "esc")
	if s := screenText(ui); strings.Contains(s, "End infra") {
		t.Fatalf("confirm still shown:\n%s", s)
	}
	// A gateway outage stays in the tab: the other tabs work.
	ui2 := newTestUI(t, 80, 24, errSource{&fakeSource{}})
	uiKey(ui2, "2")
	s = screenText(ui2)
	if !strings.Contains(s, "OpenShell gateway unreachable (r to retry)") || strings.Contains(s, "Bummer") {
		t.Fatalf("gateway down:\n%s", s)
	}
	goldenFile(t, "home-gateway-down-80", s)
	uiKey(ui2, "1")
	if s := screenText(ui2); strings.Contains(s, "unreachable") {
		t.Fatalf("outage leaked out of the tab:\n%s", s)
	}
}

// errSource is a Source whose gateway is down.
type errSource struct{ *fakeSource }

func (errSource) List(context.Context) ([]sessionctl.Row, error) {
	return nil, fmt.Errorf("dial tcp 127.0.0.1:1: connect: connection refused")
}
