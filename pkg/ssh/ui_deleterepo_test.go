package ssh

import (
	"context"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/footer"
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

	// Any key but y cancels.
	uiKey(ui, "d")
	waitScreen(t, ui, "Delete mine?")
	checkFooterLine(t, ui, "confirm", confirmLine(ui,
		"Delete mine? Its history, collaborators and webhooks are removed for good."))
	uiKey(ui, "n")
	if _, err := be.Repository(ctx, "mine"); err != nil {
		t.Fatalf("deleted after no: %v", err)
	}

	uiKey(ui, "d")
	uiKey(ui, "y")
	waitScreen(t, ui, "Deleted mine.")
	checkFooterLine(t, ui, "notice", ui.common.Styles.Prompt.Notice.Render("Deleted mine."))
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

// confirmLine is a confirm as it should read in the footer: the question
// in the help's key colour, cut to fit, and the answers at the right end
// in the help's key, description and separator colours.
func confirmLine(ui *UI, question string) string {
	st := ui.common.Styles
	w := ui.common.Width - st.App.GetHorizontalFrameSize() - st.Footer.GetHorizontalFrameSize()
	sep := footer.NewHelp(ui.common).Styles.ShortSeparator.Render(" • ")
	answer := st.HelpKey.Render("y") + " " + st.HelpValue.Render("yes") + sep +
		st.HelpKey.Render("N") + " " + st.HelpValue.Render("no")
	q := common.TruncateString(question, w-lipgloss.Width(answer)-2)
	return st.HelpKey.Render(q) + strings.Repeat(" ", w-lipgloss.Width(q)-lipgloss.Width(answer)) + answer
}

// checkFooterLine checks the footer shows line, laid out as the help
// footer: the footer's style, padding and width.
func checkFooterLine(t *testing.T, ui *UI, what, line string) {
	t.Helper()
	st := ui.common.Styles
	want := st.Footer.Width(ui.common.Width - st.App.GetHorizontalFrameSize()).Render(line)
	if got := ui.footerView(); got != want {
		t.Fatalf("%s footer\n%q\nwant\n%q", what, got, want)
	}
	if got, help := lipgloss.Width(ui.footerView()), lipgloss.Width(ui.footer.View()); got != help {
		t.Fatalf("%s footer is %d columns wide, the help footer %d", what, got, help)
	}
}
