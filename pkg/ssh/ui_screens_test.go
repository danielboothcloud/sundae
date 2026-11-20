package ssh

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// checkFits checks the screen fits w x h, and that the full help, when
// shown, cuts no key: it reflows its columns instead.
func checkFits(t *testing.T, ui *UI, w, h int) {
	t.Helper()
	lines := screenLines(ui)
	if len(lines) > h {
		t.Fatalf("%d lines on a %d line screen:\n%s", len(lines), h, strings.Join(lines, "\n"))
	}
	for i, l := range lines {
		if ansi.StringWidth(l) > w {
			t.Fatalf("line %d is %d cells wide: %q", i, ansi.StringWidth(l), l)
		}
	}
	if ui.footer.ShowAll() {
		help := ansi.Strip(ui.footer.ViewNoMargin())
		if strings.Contains(help, "…") || !regexp.MustCompile(`\?\s+less`).MatchString(help) {
			t.Fatalf("full help cut:\n%s", help)
		}
	}
}

// TestScreens renders the screens other tests leave out, whole (header to
// message slot and help line), at 80 and 50 columns.
func TestScreens(t *testing.T) {
	for _, w := range []int{80, 50} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			shot := func(ui *UI, name string) {
				t.Helper()
				checkFits(t, ui, w, 24)
				goldenFile(t, fmt.Sprintf("%s-%d", name, w), screenText(ui))
			}
			ui, _, _ := richHomeUI(t, w, 24)
			uiKey(ui, "?")
			shot(ui, "home-repos-help")
			uiKey(ui, "?", "2", "?")
			shot(ui, "home-sessions-help")
			uiKey(ui, "?", "l")
			shot(ui, "home-signals")
			uiKey(ui, "?")
			shot(ui, "home-signals-help")
			uiKey(ui, "?", "esc", "3")
			waitScreen(t, ui, "me/sundae")
			uiKey(ui, "?")
			shot(ui, "home-forges-help")
			uiKey(ui, "?", "4")
			if s := screenText(ui); !strings.Contains(strings.Split(s, "\n")[1], "100%   ") || strings.Contains(s, "☰") {
				t.Fatalf("About's position is not in the header:\n%s", s)
			}
			shot(ui, "home-about")

			ui = contentUI(t, w)
			uiKey(ui, "tab", "tab", "tab")
			waitScreen(t, ui, "main updated")
			shot(ui, "repo-bookmarks")
			uiKey(ui, "tab")
			waitScreen(t, ui, "No tags.")
			shot(ui, "repo-tags")
			uiKey(ui, "tab", "?")
			shot(ui, "repo-sessions-help")
			uiKey(ui, "?", "l")
			shot(ui, "repo-signals")
			uiKey(ui, "?")
			shot(ui, "repo-signals-help")
		})
	}
}

// TestScreensNarrow is the stress check: at 40 x 20 the screens still fit
// and the full help still lists every key it keeps whole.
func TestScreensNarrow(t *testing.T) {
	ui, _, _ := richHomeUI(t, 40, 20)
	uiKey(ui, "2")
	checkFits(t, ui, 40, 20)
	goldenFile(t, "home-sessions-40", screenText(ui))
	uiKey(ui, "?")
	checkFits(t, ui, 40, 20)
	goldenFile(t, "home-sessions-help-40", screenText(ui))
	uiKey(ui, "?", "1")
	uiKeyDepth(ui, 6, "enter")
	settle(t, ui)
	for range 5 {
		uiKey(ui, "tab")
	}
	checkFits(t, ui, 40, 20)
	goldenFile(t, "repo-sessions-40", screenText(ui))
}
