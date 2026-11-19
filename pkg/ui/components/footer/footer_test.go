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
	// Cut at width with an ellipsis.
	if s := ansi.Strip(ShortView(c, kb, 18)); s != "enter attach …" {
		t.Fatalf("narrow %q", s)
	}
}
