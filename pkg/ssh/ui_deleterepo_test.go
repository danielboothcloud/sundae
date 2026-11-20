package ssh

import (
	"context"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/proto"
)

// selectRepo moves the Repositories tab's cursor to name.
func selectRepo(t *testing.T, ui *UI, name string) {
	t.Helper()
	for range 10 {
		if selected(ui) == name {
			return
		}
		uiKey(ui, "j")
	}
	t.Fatalf("%s not in the list (at %q)", name, selected(ui))
}

func TestDeleteRepoConfirmsAndDeletes(t *testing.T) {
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
	selectRepo(t, ui, "mine")

	// The row is the confirm; the help line shows only its keys.
	uiKey(ui, "d")
	waitScreen(t, ui, "loses history, collaborators, webhooks")
	s := screenText(ui)
	if !strings.Contains(s, "┃ mine") || !strings.Contains(s, "delete?") || !strings.Contains(s, "┃ loses history, collaborators, webhooks") {
		t.Fatalf("confirm not in the row:\n%s", s)
	}
	help := ui.footer.View()
	if got := ansi.Strip(help); strings.TrimSpace(got) != "y delete • esc keep" || !strings.Contains(help, ui.common.Styles.Action.DangerKey.Render("y")) {
		t.Fatalf("help %q", help)
	}
	// Other keys are ignored; n keeps the repository and says so.
	uiKey(ui, "j", "q", "x")
	if !strings.Contains(screenText(ui), "delete?") {
		t.Fatalf("a stray key answered:\n%s", screenText(ui))
	}
	uiKey(ui, "n")
	checkFooterLine(t, ui, "kept", ui.common.Styles.Prompt.Notice.Render("Kept mine."))
	if _, err := be.Repository(ctx, "mine"); err != nil {
		t.Fatalf("deleted after no: %v", err)
	}

	uiKey(ui, "d")
	uiKey(ui, "y")
	waitScreen(t, ui, "Deleted mine.")
	checkFooterLine(t, ui, "notice", ui.common.Styles.Prompt.Success.Render("✓ Deleted mine."))
	if _, err := be.Repository(ctx, "mine"); err == nil {
		t.Fatal("repository still exists")
	}
	if s := screenText(ui); strings.Contains(s, "┃ mine") {
		t.Fatalf("still listed:\n%s", s)
	}
}

func TestDeleteRepoNeedsWriteAccess(t *testing.T) {
	ui, _, be, _ := newRepoUI(t, 80, true)
	ctx := context.Background()
	runUI(ui, ui.pages[selectionPage].Init(), 2)
	selectRepo(t, ui, "scratch") // the admin's; alice may only read it
	for _, b := range ui.ShortHelp() {
		if b.Help().Key == "d" {
			t.Fatal("D offered without write access")
		}
	}
	uiKey(ui, "d")
	waitScreen(t, ui, "✗ scratch: unauthorized")
	checkFooterLine(t, ui, "error", ui.common.Styles.Prompt.Error.Render("✗ scratch: unauthorized"))
	if _, err := be.Repository(ctx, "scratch"); err != nil {
		t.Fatal(err)
	}
}

// checkFooterLine checks the footer's help line shows line, laid out as
// the help footer: the footer's style, padding and width. The action bar
// line above it is not checked.
func checkFooterLine(t *testing.T, ui *UI, what, line string) {
	t.Helper()
	st := ui.common.Styles
	want := st.Footer.Width(ui.common.Width - st.App.GetHorizontalFrameSize()).Render(line)
	if got := ui.footerView(); !strings.HasSuffix(got, want) {
		t.Fatalf("%s footer\n%q\nwant it to end with\n%q", what, got, want)
	}
	if got, help := lipgloss.Width(ui.footerView()), lipgloss.Width(ui.footer.View()); got != help {
		t.Fatalf("%s footer is %d columns wide, the help footer %d", what, got, help)
	}
}
