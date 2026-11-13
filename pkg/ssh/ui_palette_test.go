package ssh

import (
	"regexp"
	"strings"
	"testing"
)

// paletteType types text into the open palette.
func paletteType(ui *UI, text string) {
	for _, r := range text {
		if r == ' ' {
			uiKey(ui, "space")
			continue
		}
		uiKey(ui, string(r))
	}
}

func paletteEntries(ui *UI) string {
	return strings.Join(ui.selection().PaletteEntries(), "\n")
}

func TestPaletteRunsCrossTab(t *testing.T) {
	ui := newTestUI(t, 80, 24, &fakeSource{rows: frameRows()})
	uiKey(ui, "?")
	if s := screenText(ui); !regexp.MustCompile(`:\s+commands`).MatchString(s) {
		t.Fatalf("full help:\n%s", s)
	}
	uiKey(ui, "?", ":")
	if !ui.capturing() {
		t.Fatal("palette does not capture keys")
	}
	goldenFile(t, "home-palette-80", screenText(ui))
	if s := screenText(ui); !strings.Contains(s, "type to search • ↑↓ move • enter run • esc close") {
		t.Fatalf("footer:\n%s", s)
	}
	for _, want := range []string{"Go to Sessions  2", "Sessions › End  d"} {
		if !strings.Contains(paletteEntries(ui), want) {
			t.Fatalf("no %q in\n%s", want, paletteEntries(ui))
		}
	}
	// q types, it does not quit; esc closes.
	paletteType(ui, "q")
	uiKey(ui, "esc")
	if ui.capturing() {
		t.Fatal("esc did not close")
	}
	// From Repositories, End runs on the Sessions tab, after its confirm.
	uiKey(ui, ":")
	paletteType(ui, "sessions end")
	if got := paletteEntries(ui); got != "Sessions › End  d" {
		t.Fatalf("filtered:\n%s", got)
	}
	uiKey(ui, "enter")
	if s := screenText(ui); !strings.Contains(s, "End infra / claude-1?") {
		t.Fatalf("End did not ask:\n%s", s)
	}
	uiKey(ui, "esc")
	// A tab switch.
	uiKey(ui, ":")
	paletteType(ui, "go to about")
	uiKey(ui, "enter")
	if ui.capturing() || ui.selection().PaletteEntries() != nil {
		t.Fatal("palette still open")
	}
}

func TestPaletteSyncForge(t *testing.T) {
	ui, _, _ := richHomeUI(t, 80, 20)
	uiKey(ui, "3")
	waitScreen(t, ui, "me/sundae")
	uiKey(ui, "1", ":")
	paletteType(ui, "sync github")
	if got := paletteEntries(ui); !strings.Contains(got, "Forges › Sync forge github  r") {
		t.Fatalf("entries:\n%s", got)
	}
}

func TestPaletteNotInFilter(t *testing.T) {
	ui, _, _ := richHomeUI(t, 80, 20)
	uiKey(ui, "/", ":")
	if ui.selection().PaletteEntries() != nil {
		t.Fatalf("palette opened in the filter:\n%s", screenText(ui))
	}
	uiKey(ui, "esc", "2", "/", ":")
	if ui.selection().PaletteEntries() != nil {
		t.Fatalf("palette opened in the sessions filter:\n%s", screenText(ui))
	}
}
