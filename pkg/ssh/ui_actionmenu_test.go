package ssh

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/proto"
)

// menuActions is the open action menu's actions, as key: label.
func menuActions(ui *UI) []string {
	s := ui.selection()
	if s == nil {
		return nil
	}
	var out []string
	for _, b := range s.MenuActions() {
		out = append(out, b.Help().Key+": "+b.Help().Desc)
	}
	return out
}

func TestActionMenuSessions(t *testing.T) {
	for _, w := range []int{80, 50} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			ui := newTestUI(t, w, 24, &fakeSource{rows: frameRows()})
			uiKey(ui, "2", "?")
			if s := screenText(ui); !strings.Contains(s, "space/. actions") {
				t.Fatalf("full help:\n%s", s)
			}
			uiKey(ui, "?", "space")
			if !ui.capturing() {
				t.Fatal("menu does not capture keys")
			}
			goldenFile(t, fmt.Sprintf("home-sessions-menu-%d", w), screenText(ui))
			got := strings.Join(menuActions(ui), ", ")
			for _, want := range []string{"enter: attach", "z: park", "A: add repos", "c: copy attach command", "d: end"} {
				if !strings.Contains(got, want) {
					t.Fatalf("no %q in %s", want, got)
				}
			}
			if s := screenText(ui); !strings.Contains(s, "enter run") || !strings.Contains(s, "esc close") {
				t.Fatalf("footer:\n%s", s)
			}
			// esc closes; q in the menu does not quit.
			uiKey(ui, "q", "esc")
			if ui.capturing() || strings.Contains(screenText(ui), "Copy attach command") {
				t.Fatalf("esc did not close:\n%s", screenText(ui))
			}
			// The action's key runs it, as pressing d does: the confirm.
			uiKey(ui, ".", "d")
			if s := screenText(ui); !strings.Contains(s, "End infra / claude-1?") {
				t.Fatalf("d in the menu did not ask:\n%s", s)
			}
			uiKey(ui, "esc")
			// enter runs the highlighted one: move to End, the last.
			uiKey(ui, ".")
			for range menuActions(ui) {
				uiKey(ui, "j")
			}
			uiKey(ui, "enter")
			if s := screenText(ui); !strings.Contains(s, "End infra / claude-1?") {
				t.Fatalf("enter on End did not ask:\n%s", s)
			}
			uiKey(ui, "esc")
			// . and space close it too.
			uiKey(ui, ".", ".")
			uiKey(ui, "space", "space")
			if ui.capturing() {
				t.Fatal("menu still open")
			}
		})
	}
}

func TestActionMenuNotInFilter(t *testing.T) {
	ui := newTestUI(t, 80, 24, &fakeSource{rows: frameRows()})
	uiKey(ui, "2", "/", ".")
	if menuActions(ui) != nil || !strings.Contains(screenText(ui), "/ .") {
		t.Fatalf("menu opened in the filter:\n%s", screenText(ui))
	}
}

func TestActionMenuRepositories(t *testing.T) {
	ui, _, be, _ := newRepoUI(t, 80, true)
	ctx := context.Background()
	alice, err := be.User(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "mine", alice, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	ui.Init()
	uiKey(ui, "1")
	runUI(ui, ui.pages[selectionPage].Init(), 2)

	// Without write access there is no Delete.
	selectRepo(t, ui, "scratch")
	uiKey(ui, ".")
	got := strings.Join(menuActions(ui), ", ")
	if got != "enter: open, s: new session, c: copy command" {
		t.Fatalf("scratch actions %s", got)
	}
	if s := screenText(ui); !strings.Contains(s, "> enter  Open") || !strings.Contains(s, "c      Copy command") {
		t.Fatalf("menu:\n%s", s)
	}
	uiKey(ui, "esc")

	uiKey(ui, "k", "k", "k")
	selectRepo(t, ui, "mine")
	uiKey(ui, ".")
	if got := strings.Join(menuActions(ui), ", "); !strings.HasSuffix(got, "d: delete repository") {
		t.Fatalf("mine actions %s", got)
	}
	uiKey(ui, "d")
	waitScreen(t, ui, "Delete mine?")
	uiKey(ui, "n")
	// enter runs Open, the first.
	uiKey(ui, "space")
	uiKeyDepth(ui, 6, "enter")
	if ui.activePage != repoPage {
		t.Fatalf("enter did not open the repository:\n%s", screenText(ui))
	}
}

func TestActionMenuForges(t *testing.T) {
	ui, _, ff := richHomeUI(t, 80, 20)
	// A row not imported has nothing to open, push or copy.
	uiKey(ui, "3", "j", ".")
	if got := strings.Join(menuActions(ui), ", "); got != "i: import, r: sync forge" {
		t.Fatalf("not imported row actions %s", got)
	}
	uiKey(ui, "esc", "j", ".")
	goldenFile(t, "home-forges-menu-80", screenText(ui))
	got := strings.Join(menuActions(ui), ", ")
	if got != "enter: open, r: sync, p: push, c: copy clone command" {
		t.Fatalf("forge row actions %s", got)
	}
	// p runs the push, after its confirm.
	uiKey(ui, "p")
	if s := screenText(ui); !strings.Contains(s, "Push main of sundae") {
		t.Fatalf("p did not ask:\n%s", s)
	}
	uiKey(ui, "y")
	waitPushes(t, ff, 1)
}
