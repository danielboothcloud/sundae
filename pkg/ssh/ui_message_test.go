package ssh

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/sessions"
)

// screenLines is the screen with trailing spaces trimmed, keeping the
// frame's bottom margin line.
func screenLines(ui *UI) []string {
	lines := strings.Split(ansi.Strip(ui.View().Content), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return lines
}

// checkSlot checks msg is in the frame's rendering on the line above the help,
// on every page, and that only that line differs from before.
func checkSlot(t *testing.T, ui *UI, before []string, msg string) {
	t.Helper()
	lines := screenLines(ui)
	after := strings.Join(lines, "\n")
	help := len(lines) - 2
	slot := help - 1
	if got := lines[slot]; got != "   "+msg {
		t.Fatalf("slot %q, want %q:\n%s", got, "   "+msg, after)
	}
	wantHelp := strings.TrimRight("  "+ansi.Strip(ui.footer.ViewNoMargin()), " ")
	if lines[help] != wantHelp {
		t.Fatalf("help line %q, want %q:\n%s", lines[help], wantHelp, after)
	}
	text, kind := ui.pages[ui.activePage].(common.Messenger).Message()
	w := ui.common.Width - ui.common.Styles.App.GetHorizontalFrameSize() - ui.common.Styles.Footer.GetHorizontalFrameSize()
	raw := strings.Split(ui.View().Content, "\n")
	if want := ui.common.NoticeView(text, kind, w); !strings.Contains(raw[slot], want) {
		t.Fatalf("slot %q is not the frame's rendering %q", raw[slot], want)
	}
	if before == nil {
		return
	}
	if len(before) != len(lines) {
		t.Fatalf("%d lines with the message, %d without:\n%s\n---\n%s", len(lines), len(before), after, strings.Join(before, "\n"))
	}
	for i := range lines {
		if i != slot && lines[i] != before[i] {
			t.Fatalf("line %d changed with the message: %q, was %q", i, lines[i], before[i])
		}
	}
	if before[slot] != "" {
		t.Fatalf("slot not empty without a message: %q", before[slot])
	}
}

// focusUI is the home page started on the Sessions tab, on claude-1, as
// after leaving it; left "" leaves no notice.
func focusUI(t *testing.T, w int, left string) *UI {
	t.Helper()
	return newHomeUI(t, w, 24, func(ctx context.Context, _ *backend.Backend) context.Context {
		ctx = context.WithValue(ctx, sessions.SourceKey, sessions.Source(&fakeSource{rows: frameRows()}))
		return context.WithValue(ctx, sessions.FocusKey, sessions.Focus{Sandbox: "sb-infra-claude-1", Left: left})
	})
}

// TestMessageSlot checks every page's message lands directly above the
// help line, rendered the same way, without moving a row.
func TestMessageSlot(t *testing.T) {
	for _, w := range []int{80, 50} {
		cut := func(s string) string { return common.TruncateString(s, w-6) }
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			// Sessions: back from an attached session.
			before := screenLines(focusUI(t, w, ""))
			ui := focusUI(t, w, "infra / claude-1")
			checkSlot(t, ui, before, cut("Left infra / claude-1; it keeps running. enter goes back."))
			goldenFile(t, fmt.Sprintf("message-sessions-left-%d", w), screenText(ui))

			// Forges: an error, and a selected row's last error.
			ui, _, ff := richHomeUI(t, w, 24)
			ff.branchErr = errors.New("HEAD is detached")
			uiKey(ui, "3", "j", "j")
			before = screenLines(ui)
			uiKey(ui, "p")
			checkSlot(t, ui, before, cut("✗ Cannot push sundae: HEAD is detached"))
			goldenFile(t, fmt.Sprintf("message-forges-error-%d", w), screenText(ui))

			// Repositories: kept, deleted, creating.
			ui = deleteRepoUI(t, w)
			before = screenLines(ui)
			uiKey(ui, "d", "n")
			checkSlot(t, ui, before, "Kept mine.")
			uiKey(ui, "d", "y")
			waitScreen(t, ui, "Deleted mine.")
			checkSlot(t, ui, nil, "✓ Deleted mine.")
			goldenFile(t, fmt.Sprintf("message-repos-deleted-%d", w), screenText(ui))

			ui, _, _, _ = newRepoUI(t, w, true)
			uiKey(ui, "n", "/")
			typeText(ui, "apps/web")
			press(ui, tea.KeyDown, 1) // ends typing
			// Not run: the creating message stays.
			_, _ = ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			checkSlot(t, ui, nil, "Creating apps/web …")
			goldenFile(t, fmt.Sprintf("message-repos-creating-%d", w), screenText(ui))

			// The repository page's Sessions tab.
			ui, _, _ = richHomeUI(t, w, 24)
			uiKeyDepth(ui, 6, "enter") // sundae
			settle(t, ui)
			for range 5 {
				uiKey(ui, "tab")
			}
			before = screenLines(ui)
			uiKey(ui, "d", "esc")
			checkSlot(t, ui, before, "Kept sundae / forge-sync.")
			goldenFile(t, fmt.Sprintf("message-repo-sessions-%d", w), screenText(ui))
		})
	}
}
