package ssh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/forges"
)

// checkConfirmRow checks the confirm is drawn in the selected row: the
// question and the keys whole, on lines marked ┃, and the help line
// showing only the keys.
func checkConfirmRow(t *testing.T, screen, question, keys string) {
	t.Helper()
	var asked, answered bool
	for _, l := range strings.Split(screen, "\n") {
		if !strings.Contains(l, "┃") {
			continue
		}
		asked = asked || strings.HasSuffix(l, " "+question)
		answered = answered || strings.HasSuffix(l, keys)
	}
	if !asked || !answered {
		t.Fatalf("%q and %q not whole in the selected row:\n%s", question, keys, screen)
	}
	if last := screen[strings.LastIndex(screen, "\n")+1:]; strings.TrimSpace(last) != keys {
		t.Fatalf("help line %q, want only the keys", last)
	}
}

// TestConfirmInRow renders each confirm in its row at 80 and 50 columns.
func TestConfirmInRow(t *testing.T) {
	for _, w := range []int{80, 50} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			ui := newTestUI(t, w, 24, &fakeSource{rows: frameRows()})
			uiKey(ui, "2", "d")
			s := screenText(ui)
			checkConfirmRow(t, s, "delete?", "y delete • esc keep")
			goldenFile(t, fmt.Sprintf("home-sessions-confirm-%d", w), s)

			ui, _, _ = richHomeUI(t, w, 24)
			uiKeyDepth(ui, 6, "enter") // sundae
			settle(t, ui)
			for range 5 {
				uiKey(ui, "tab")
			}
			uiKey(ui, "d")
			s = screenText(ui)
			checkConfirmRow(t, s, "delete?", "y delete • esc keep")
			goldenFile(t, fmt.Sprintf("repo-sessions-confirm-%d", w), s)

			ui, _, _ = richHomeUI(t, w, 24)
			uiKey(ui, "3", "j", "j", "p")
			s = screenText(ui)
			checkConfirmRow(t, s, "push main → github?", "y push • esc cancel")
			goldenFile(t, fmt.Sprintf("home-forges-confirm-%d", w), s)

			ui = deleteRepoUI(t, w)
			uiKey(ui, "d")
			waitScreen(t, ui, "delete?")
			s = screenText(ui)
			checkConfirmRow(t, s, "delete?", "y delete • esc keep")
			goldenFile(t, fmt.Sprintf("home-repos-confirm-%d", w), s)
		})
	}
}

// deleteRepoUI is the home page of alice, who owns mine, the selected
// repository, with fixed update times.
func deleteRepoUI(t *testing.T, w int) *UI {
	t.Helper()
	ui, _, be, _ := newRepoUI(t, w, true)
	ctx := context.Background()
	alice, err := be.User(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "mine", alice, proto.RepositoryOptions{Description: "about mine"}); err != nil {
		t.Fatal(err)
	}
	repos, err := be.Repositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range repos {
		gr, err := r.Open()
		if err != nil {
			t.Fatal(err)
		}
		// mine is the newest, first in the list.
		day := 1
		if r.Name() == "mine" {
			day = 3
		}
		at := time.Date(1970, 1, day, i, 0, 0, 0, time.UTC).Format(time.RFC3339)
		if err := os.WriteFile(filepath.Join(gr.Path, "info", "last-modified"), []byte(at), 0o644); err != nil { //nolint:gosec
			t.Fatal(err)
		}
	}
	ui.Init()
	uiKey(ui, "1")
	runUI(ui, ui.pages[selectionPage].Init(), 2)
	selectRepo(t, ui, "mine")
	return ui
}

// TestConfirmIgnoresOtherKeys checks no key but y, n and esc answers a
// confirm, on each tab.
func TestConfirmIgnoresOtherKeys(t *testing.T) {
	ui, _, ff := richHomeUI(t, 50, 24)
	uiKey(ui, "2", "d", "j", "q", "enter", "1", "tab", "?")
	if s := screenText(ui); !strings.Contains(s, "delete?") || strings.Contains(s, "deleting") || ui.activePage != selectionPage {
		t.Fatalf("a stray key answered the delete:\n%s", s)
	}
	uiKey(ui, "N")
	if s := screenText(ui); !strings.Contains(s, "Kept infra / claude-1.") {
		t.Fatalf("N:\n%s", s)
	}
	uiKey(ui, "3", "j", "j", "p", "k", "q", "enter")
	if s := screenText(ui); !strings.Contains(s, "push main → github?") || len(ff.pushes) != 0 {
		t.Fatalf("a stray key answered the push:\n%s", s)
	}
	uiKey(ui, "esc")
	if s := screenText(ui); !strings.Contains(s, "Not pushed.") {
		t.Fatalf("esc:\n%s", s)
	}
}

// TestConfirmRowGone checks a delete confirm closes when its repository is
// deleted elsewhere, as the Sessions and Forges tabs' confirms do.
func TestConfirmRowGone(t *testing.T) {
	ui := deleteRepoUI(t, 80)
	uiKey(ui, "d")
	waitScreen(t, ui, "delete?")
	if err := ui.common.Backend().DeleteRepository(ui.common.Context(), "mine"); err != nil {
		t.Fatal(err)
	}
	_, cmd := ui.Update(forges.ReposChangedMsg{})
	runUI(ui, cmd, 2)
	if s := screenText(ui); strings.Contains(s, "delete?") || ui.capturing() || !strings.Contains(helpLine(s), "? more") {
		t.Fatalf("the confirm outlived its row:\n%s", s)
	}
}
