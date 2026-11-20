package footer

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

func TestShortViewColoursByKind(t *testing.T) {
	c := common.NewCommon(context.Background(), 80, 24)
	b := func(k, desc string) key.Binding { return key.NewBinding(key.WithKeys(k), key.WithHelp(k, desc)) }
	off := b("x", "hidden")
	off.SetEnabled(false)
	kb := []key.Binding{b("enter", "attach"), b("n", "new"), off, b("d", "delete")}
	got := ShortView(c, kb, 0)
	if s := ansi.Strip(got); s != "enter attach • n new • d delete" {
		t.Fatalf("text %q", s)
	}
	st := c.Styles
	for _, want := range []string{
		st.Action.PrimaryKey.Render("enter"),
		st.HelpKey.Render("n"),
		st.Action.DangerKey.Render("d") + " " + st.Action.DangerDesc.Render("delete"),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	// Whole keys only: what does not fit is left out, never cut.
	if s := ansi.Strip(ShortView(c, kb, 18)); s != "enter attach" {
		t.Fatalf("narrow %q", s)
	}
}

// sessionsHelp is the Sessions tab's short help as the frame builds it.
func sessionsHelp() []key.Binding {
	b := func(k, desc string) key.Binding { return key.NewBinding(key.WithKeys(k), key.WithHelp(k, desc)) }
	enter := key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "attach"))
	return []key.Binding{
		b("j/k", "move"), enter, b("space", "actions"), b("n", "new"), b("/", "filter"),
		b("l", "history"), b("H/L", "tabs"), b("q", "quit"), b("?", "more"),
	}
}

func TestFitRanksByWidth(t *testing.T) {
	c := common.NewCommon(context.Background(), 80, 24)
	for _, tc := range []struct {
		width int
		want  string
	}{
		{0, "j/k move • enter attach • space actions • n new • / filter • l history • H/L tabs • q quit • ? more"},
		{80, "j/k move • enter attach • space actions • n new • / filter • H/L tabs • ? more"},
		{75, "j/k move • enter attach • space actions • n new • H/L tabs • ? more"},
		{50, "enter attach • space actions • n new • ? more"},
		{45, "enter attach • space actions • n new • ? more"},
		{44, "enter attach • space actions • ? more"},
		{40, "enter attach • space actions • ? more"},
		{21, "enter attach • ? more"},
		{20, "? more"},
	} {
		got := ansi.Strip(ShortView(c, sessionsHelp(), tc.width))
		if got != tc.want {
			t.Errorf("width %d:\n got %q\nwant %q", tc.width, got, tc.want)
		}
		if tc.width > 0 && ansi.StringWidth(got) > tc.width {
			t.Errorf("width %d: %q is %d wide", tc.width, got, ansi.StringWidth(got))
		}
	}
}

// TestFitFindBeforeRowKeys checks / filter outranks the row keys that the
// action menu lists too (Forges at 80 columns: 75 cells of help).
func TestFitFindBeforeRowKeys(t *testing.T) {
	c := common.NewCommon(context.Background(), 80, 24)
	b := func(k, desc string) key.Binding { return key.NewBinding(key.WithKeys(k), key.WithHelp(k, desc)) }
	enter := key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open"))
	kb := []key.Binding{b("j/k", "move"), enter, b("space", "actions"), b("i", "import"), b("r", "sync"), b("/", "filter"), b("H/L", "tabs"), b("?", "more")}
	want := "j/k move • enter open • space actions • / filter • H/L tabs • ? more"
	if got := ansi.Strip(ShortView(c, kb, 75)); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestFitKeepsFormAndPaletteAnswers(t *testing.T) {
	c := common.NewCommon(context.Background(), 50, 24)
	b := func(k, desc string) key.Binding { return key.NewBinding(key.WithKeys(k), key.WithHelp(k, desc)) }
	enter := func(desc string) key.Binding {
		return key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", desc))
	}
	form := []key.Binding{b("j/k", "field"), b("h/l", "change"), b("/", "type"), enter("create"), b("esc", "cancel")}
	palette := []key.Binding{b("type", "to search"), b("↑↓", "move"), enter("run"), b("esc", "close")}
	confirm := []key.Binding{b("y", "delete"), b("esc", "keep")}
	for _, tc := range []struct {
		kb    []key.Binding
		width int
		want  string
	}{
		{form, 46, "j/k field • / type • enter create • esc cancel"},
		{form, 30, "enter create • esc cancel"},
		{palette, 46, "↑↓ move • enter run • esc close"},
		{palette, 24, "enter run • esc close"},
		{confirm, 20, "y delete • esc keep"},
	} {
		if got := ansi.Strip(ShortView(c, tc.kb, tc.width)); got != tc.want {
			t.Errorf("width %d: got %q want %q", tc.width, got, tc.want)
		}
	}
}

func TestRankOf(t *testing.T) {
	b := func(k, desc string) key.Binding { return key.NewBinding(key.WithKeys(k), key.WithHelp(k, desc)) }
	for _, tc := range []struct {
		b    key.Binding
		want Rank
	}{
		{b("?", "more"), RankMore},
		{key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")), RankPrimary},
		{b("esc", "cancel"), RankPrimary},
		{b("y", "push"), RankPrimary},
		{b("space", "actions"), RankRow},
		{b("n", "new repository"), RankRow},
		{b("j/k", "move"), RankNav},
		{b("H/L", "tabs"), RankNav},
		{b("/", "filter"), RankFind},
		{b("i", "import"), RankOther},
		{b("q", "quit"), RankQuit},
	} {
		if got := RankOf(tc.b); got != tc.want {
			t.Errorf("%s %s: rank %d, want %d", tc.b.Help().Key, tc.b.Help().Desc, got, tc.want)
		}
	}
}
