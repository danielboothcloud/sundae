package actionmenu

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

func actions() []key.Binding {
	off := key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "off"))
	off.SetEnabled(false)
	return []key.Binding{
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
		off,
		key.NewBinding(key.WithKeys("A"), key.WithHelp("A", "add repos")),
		key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "end")),
	}
}

func newMenu() *Menu {
	return New(common.NewCommon(context.Background(), 80, 24), "row", actions())
}

func TestDisabledHidden(t *testing.T) {
	m := newMenu()
	if len(m.Actions()) != 3 || strings.Contains(m.View(40), "Off") {
		t.Fatalf("actions %v\n%s", m.Actions(), m.View(40))
	}
}

func TestMoveAndRun(t *testing.T) {
	m := newMenu()
	m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // stops at the last
	m.Update(tea.KeyPressMsg{Code: 'k', Text: "k"})
	res, b := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if res != Run || b.Help().Key != "A" {
		t.Fatalf("enter ran %v %q", res, b.Help().Key)
	}
	if got := KeyPress(b).String(); got != "A" {
		t.Fatalf("re-fed key %q", got)
	}
}

func TestDirectKeyAndClose(t *testing.T) {
	m := newMenu()
	if res, b := m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"}); res != Run || b.Help().Key != "d" {
		t.Fatalf("d: %v", res)
	}
	if res, _ := m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"}); res != Pending {
		t.Fatal("disabled key ran")
	}
	for _, k := range []tea.KeyPressMsg{{Code: tea.KeyEscape}, {Code: tea.KeySpace, Text: " "}, {Code: '.', Text: "."}} {
		if res, _ := m.Update(k); res != Closed {
			t.Fatalf("%s did not close", k)
		}
	}
	if !key.Matches(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}, OpenKey) {
		t.Fatal("space does not open")
	}
}

func TestOverlay(t *testing.T) {
	bg := strings.Repeat("..........\n", 4) + ".........."
	got := Overlay(bg, "ab\ncd", 10, 5)
	want := "..........\n....ab....\n....cd....\n..........\n.........."
	if ansi.Strip(got) != want {
		t.Fatalf("got\n%s", ansi.Strip(got))
	}
}

func TestListView(t *testing.T) {
	m := newMenu()
	got := ansi.Strip(ListView(m.common, m.Actions(), 1, 30))
	want := "  enter  Open\n> A      Add repos\n  d      End"
	if got != want {
		t.Fatalf("got\n%q", got)
	}
}
