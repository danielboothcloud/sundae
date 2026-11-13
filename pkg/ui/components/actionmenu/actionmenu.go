// Package actionmenu is a small popup that lists actions, each with its
// key: the home tabs' menu for the selected row (space or .). Its actions
// are the tab's own key bindings, and running one re-feeds that key, so
// the menu runs exactly what the key does and cannot drift from it.
package actionmenu

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

// OpenKey opens the menu for the selected row.
var OpenKey = key.NewBinding(key.WithKeys("space", "."), key.WithHelp("space/.", "actions"))

// KeyMap is the menu's keys.
type KeyMap struct {
	Up    key.Binding
	Down  key.Binding
	Run   key.Binding
	Close key.Binding
}

// DefaultKeyMap returns the menu's keys.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Up:    key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:  key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Run:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "run")),
		Close: key.NewBinding(key.WithKeys("esc", "space", "."), key.WithHelp("esc", "close")),
	}
}

// Result is what a key did to the menu.
type Result int

const (
	// Pending means the menu is still open.
	Pending Result = iota
	// Run means an action was chosen; the menu is done.
	Run
	// Closed means the menu was closed without an action.
	Closed
)

// Menu is a list of actions, one highlighted.
type Menu struct {
	common  common.Common
	title   string
	actions []key.Binding
	cursor  int
	Keys    KeyMap
}

// New makes a menu of the enabled actions; disabled ones are left out.
func New(c common.Common, title string, actions []key.Binding) *Menu {
	var as []key.Binding
	for _, a := range actions {
		if a.Enabled() && len(a.Keys()) > 0 {
			as = append(as, a)
		}
	}
	return &Menu{common: c, title: title, actions: as, Keys: DefaultKeyMap()}
}

// Actions returns the menu's actions.
func (m *Menu) Actions() []key.Binding { return m.actions }

// Cursor returns the index of the highlighted action.
func (m *Menu) Cursor() int { return m.cursor }

// Update handles a key. On Run it returns the chosen action.
func (m *Menu) Update(msg tea.KeyPressMsg) (Result, key.Binding) {
	switch {
	case key.Matches(msg, m.Keys.Close):
		return Closed, key.Binding{}
	case key.Matches(msg, m.Keys.Up):
		m.cursor = max(m.cursor-1, 0)
		return Pending, key.Binding{}
	case key.Matches(msg, m.Keys.Down):
		m.cursor = min(m.cursor+1, max(len(m.actions)-1, 0))
		return Pending, key.Binding{}
	case key.Matches(msg, m.Keys.Run):
		if m.cursor < len(m.actions) {
			return Run, m.actions[m.cursor]
		}
		return Closed, key.Binding{}
	}
	for _, a := range m.actions {
		if key.Matches(msg, a) {
			return Run, a
		}
	}
	return Pending, key.Binding{}
}

// ShortHelp is the menu's footer help.
func (m *Menu) ShortHelp() []key.Binding {
	move := key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑↓", "move"))
	return []key.Binding{move, m.Keys.Run, m.Keys.Close}
}

// Label is an action's name for the menu: its help, capitalised.
func Label(b key.Binding) string {
	d := b.Help().Desc
	r, n := utf8.DecodeRuneInString(d)
	if n == 0 {
		return d
	}
	return string(unicode.ToUpper(r)) + d[n:]
}

// KeyLabel is an action's key for the menu.
func KeyLabel(b key.Binding) string {
	if k := b.Help().Key; k != "" {
		return k
	}
	return b.Keys()[0]
}

// ListView renders actions as rows of key and label, the cursor's
// highlighted, at most width cells wide. The command palette reuses it.
func ListView(c common.Common, actions []key.Binding, cursor, width int) string {
	st := c.Styles.ActionMenu
	kw := 0
	for _, a := range actions {
		kw = max(kw, ansi.StringWidth(KeyLabel(a)))
	}
	rows := make([]string, len(actions))
	for i, a := range actions {
		ks, as, cur := st.Key, st.Action, "  "
		if i == cursor {
			ks, as, cur = st.ActiveKey, st.ActiveAction, st.Cursor.Render("> ")
		}
		k := KeyLabel(a)
		k += strings.Repeat(" ", kw-ansi.StringWidth(k))
		label := common.TruncateString(Label(a), max(width-kw-4, 1))
		rows[i] = cur + ks.Render(k) + "  " + as.Render(label)
	}
	return strings.Join(rows, "\n")
}

// View renders the menu as a box at most width cells wide.
func (m *Menu) View(width int) string {
	st := m.common.Styles.ActionMenu
	inner := max(width-st.Box.GetHorizontalFrameSize(), 1)
	body := ListView(m.common, m.actions, m.cursor, inner)
	if m.title != "" {
		body = st.Title.Render(common.TruncateString(m.title, inner)) + "\n" + body
	}
	return st.Box.Render(body)
}

// Overlay draws fg over bg, centred in a width × height area.
func Overlay(bg, fg string, width, height int) string {
	bl := strings.Split(bg, "\n")
	for len(bl) < height {
		bl = append(bl, "")
	}
	fl := strings.Split(fg, "\n")
	fw := lipgloss.Width(fg)
	x := max((width-fw)/2, 0)
	y := max((height-len(fl))/2, 0)
	for i, f := range fl {
		row := y + i
		if row >= len(bl) {
			break
		}
		b := bl[row]
		left := ansi.Truncate(b, x, "")
		if w := ansi.StringWidth(left); w < x {
			left += strings.Repeat(" ", x-w)
		}
		right := ansi.TruncateLeft(b, x+ansi.StringWidth(f), "")
		bl[row] = left + "\x1b[0m" + f + "\x1b[0m" + right
	}
	return strings.Join(bl, "\n")
}

// KeyPress is the key press of an action's first key, to re-feed it to
// the tab that owns the binding.
func KeyPress(b key.Binding) tea.KeyPressMsg {
	k := b.Keys()[0]
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	}
	r, _ := utf8.DecodeRuneInString(k)
	return tea.KeyPressMsg{Code: r, Text: k}
}
