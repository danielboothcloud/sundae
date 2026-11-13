package selection

import (
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/actionmenu"
)

// PaletteKey opens the command palette: every action of every home tab.
var PaletteKey = key.NewBinding(key.WithKeys(":"), key.WithHelp(":", "commands"))

// paletteRows is how many entries the palette shows at once.
const paletteRows = 10

// command is a palette entry: a key of a tab, re-fed to it after
// switching to it, or run, when set, instead.
type command struct {
	pane    pane
	binding key.Binding // its help is the palette's label and key
	run     func() tea.Cmd
}

// palette is the command palette's state.
type palette struct {
	all    []command
	shown  []command
	query  string
	cursor int
}

// commands are every home tab's entries: the tab switches, each tab's
// actions for its selected row and a sync of each forge listed.
func (s *Selection) commands() []command {
	var cs []command
	for i, p := range s.panes {
		k := string(rune('1' + i))
		cs = append(cs, command{pane: p, binding: key.NewBinding(key.WithKeys(k), key.WithHelp(k, "go to "+p.String()))})
	}
	for _, p := range s.panes {
		if p == sessionsPane {
			s.bindSessions()
		}
		for _, b := range s.paneActions(p) {
			if !b.Enabled() || len(b.Keys()) == 0 {
				continue
			}
			nb := b
			nb.SetHelp(actionmenu.KeyLabel(b), p.String()+" › "+actionmenu.Label(b))
			cs = append(cs, command{pane: p, binding: nb})
		}
	}
	if s.forges != nil {
		for _, name := range s.forges.ForgeNames() {
			name := name
			b := key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "Forges › Sync forge "+name))
			cs = append(cs, command{pane: forgesPane, binding: b, run: func() tea.Cmd { return s.forges.SyncForge(name) }})
		}
	}
	return cs
}

// openPalette opens the command palette.
func (s *Selection) openPalette() {
	s.repoNotice = ""
	p := &palette{all: s.commands()}
	p.filter()
	s.palette = p
}

// filter shows the entries whose label has every word of the query.
func (p *palette) filter() {
	words := strings.Fields(strings.ToLower(p.query))
	p.shown = p.shown[:0]
	for _, c := range p.all {
		label := strings.ToLower(c.binding.Help().Desc)
		ok := true
		for _, w := range words {
			if !strings.Contains(label, w) {
				ok = false
				break
			}
		}
		if ok {
			p.shown = append(p.shown, c)
		}
	}
	p.cursor = min(p.cursor, max(len(p.shown)-1, 0))
}

// onPaletteKey handles a key while the palette is open. A chosen entry
// closes it, switches to its tab and re-feeds its key there, as the
// action menu does (a confirm still asks first).
func (s *Selection) onPaletteKey(msg tea.KeyPressMsg) tea.Cmd {
	p := s.palette
	switch msg.String() {
	case "esc", "ctrl+c":
		s.palette = nil
	case "up", "ctrl+p":
		p.cursor = max(p.cursor-1, 0)
	case "down", "ctrl+n":
		p.cursor = min(p.cursor+1, max(len(p.shown)-1, 0))
	case "backspace":
		if p.query != "" {
			_, n := utf8.DecodeLastRuneInString(p.query)
			p.query = p.query[:len(p.query)-n]
			p.filter()
		}
	case "enter":
		s.palette = nil
		if p.cursor >= len(p.shown) {
			return nil
		}
		return s.runCommand(p.shown[p.cursor])
	default:
		if msg.Text != "" {
			p.query += msg.Text
			p.cursor = 0
			p.filter()
		}
	}
	return nil
}

// runCommand switches to the entry's tab and runs it.
func (s *Selection) runCommand(c command) tea.Cmd {
	var cmd tea.Cmd
	if s.activePane != c.pane {
		cmd = s.selectPane(c.pane)
	}
	if strings.HasPrefix(c.binding.Help().Desc, "go to ") {
		return cmd
	}
	if c.run != nil {
		return tea.Batch(cmd, c.run())
	}
	_, run := s.Update(actionmenu.KeyPress(c.binding))
	return tea.Batch(cmd, run)
}

// paletteHelp is the footer help while the palette is open.
func paletteHelp() []key.Binding {
	return []key.Binding{
		key.NewBinding(key.WithKeys("type"), key.WithHelp("type", "to search")),
		key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑↓", "move")),
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "run")),
		key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close")),
	}
}

// view renders the palette as a box at most width cells wide.
func (p *palette) view(c common.Common, width int) string {
	st := c.Styles.ActionMenu
	inner := max(width-st.Box.GetHorizontalFrameSize(), 1)
	input := st.Title.Render(": ") + p.query + st.Cursor.Render("█")
	bs := make([]key.Binding, len(p.shown))
	for i, cmd := range p.shown {
		bs[i] = cmd.binding
	}
	start := max(min(p.cursor-paletteRows/2, len(bs)-paletteRows), 0)
	end := min(start+paletteRows, len(bs))
	body := c.Styles.HelpValue.Render("No matching commands")
	if len(bs) > 0 {
		body = actionmenu.ListView(c, bs[start:end], p.cursor-start, inner)
	}
	return st.Box.Width(width).Render(st.Title.Render("Commands") + "\n" + input + "\n" + body)
}

// PaletteEntries is the open palette's shown labels; nil when closed.
func (s *Selection) PaletteEntries() []string {
	if s.palette == nil {
		return nil
	}
	out := make([]string, len(s.palette.shown))
	for i, c := range s.palette.shown {
		out[i] = actionmenu.Label(c.binding) + "  " + actionmenu.KeyLabel(c.binding)
	}
	return out
}
