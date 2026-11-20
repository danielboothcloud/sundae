package ssh

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// helpLine is the screen's last line, the help.
func helpLine(screen string) string {
	return strings.TrimSpace(screen[strings.LastIndex(screen, "\n")+1:])
}

// TestKeySurfaces renders the home screens whose keys the help line ranks
// by width: whole keys only, the context's answer always kept.
func TestKeySurfaces(t *testing.T) {
	for _, w := range []int{80, 50} {
		for _, sc := range []struct {
			name string
			keys []string
			wait string
			want []string
		}{
			{"repos", nil, "", []string{"enter open", "space actions", "? more"}},
			{"sessions", []string{"2"}, "", []string{"enter attach", "space actions", "n new", "? more"}},
			{"forges", []string{"3"}, "me/sundae", []string{"enter open", "space actions", "? more"}},
			{"new-session", []string{"2", "n"}, "", []string{"enter create", "esc cancel"}},
			{"palette", []string{"2", ":"}, "", []string{"enter run", "esc close"}},
			{"menu", []string{"2", "space"}, "", []string{"enter run", "esc close"}},
		} {
			t.Run(fmt.Sprintf("%s-%d", sc.name, w), func(t *testing.T) {
				ui, _, _ := richHomeUI(t, w, 24)
				uiKey(ui, sc.keys...)
				if sc.wait != "" {
					waitScreen(t, ui, sc.wait)
				}
				s := screenText(ui)
				goldenFile(t, fmt.Sprintf("keys-%s-%d", sc.name, w), s)
				help := helpLine(s)
				for _, want := range sc.want {
					if !strings.Contains(help, want) {
						t.Errorf("help %q lacks %q", help, want)
					}
				}
				if strings.Contains(help, "…") {
					t.Errorf("help %q cuts a key", help)
				}
				if strings.Contains(s, "▸ infra") || strings.Contains(s, "space more") {
					t.Errorf("action bar still shown:\n%s", s)
				}
			})
		}
	}
}

// TestKeySurfacesQuitLast checks q quit, the lowest key, shows only where
// every other key fits, and ? lists it.
func TestKeySurfacesQuitLast(t *testing.T) {
	ui, _, _ := richHomeUI(t, 120, 24)
	uiKey(ui, "2")
	if help := helpLine(screenText(ui)); !strings.HasSuffix(help, "q quit • ? more") {
		t.Fatalf("120 columns: %q", help)
	}
	ui, _, _ = richHomeUI(t, 80, 24)
	uiKey(ui, "2")
	if help := helpLine(screenText(ui)); strings.Contains(help, "q quit") || !strings.Contains(help, "H/L tabs") {
		t.Fatalf("80 columns: %q", help)
	}
	uiKey(ui, "?")
	if s := screenText(ui); !regexp.MustCompile(`q\s+quit`).MatchString(s) {
		t.Fatalf("full help:\n%s", s)
	}
}

// TestPaletteNamesTargets checks palette entries name the row they act on.
func TestPaletteNamesTargets(t *testing.T) {
	ui, _, _ := richHomeUI(t, 80, 24)
	uiKey(ui, "3")
	waitScreen(t, ui, "me/sundae")
	uiKey(ui, "j", "j", ":")
	got := paletteEntries(ui)
	for _, want := range []string{
		"Go to Sessions  2",
		"Repositories › Open sundae  enter",
		"Sessions › Delete infra / claude-1  d",
		"Forges › Push me/sundae  p",
		"Forges › Sync forge github  u",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	if n := strings.Count(got, "Sync forge github"); n != 1 {
		t.Errorf("%d Sync forge github entries:\n%s", n, got)
	}
}
