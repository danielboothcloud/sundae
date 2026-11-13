package footer

import (
	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

// ToggleFooterMsg is a message sent to show/hide the footer.
type ToggleFooterMsg struct{}

// Footer is a Bubble Tea model that displays help and other info.
type Footer struct {
	common common.Common
	help   help.Model
	keymap help.KeyMap
}

// NewHelp returns the help the footer renders keys with. Other lines that
// show keys (a confirm's answers) use it to look the same.
func NewHelp(c common.Common) help.Model {
	h := help.New()
	h.Styles.ShortKey = c.Styles.HelpKey
	h.Styles.ShortDesc = c.Styles.HelpValue
	h.Styles.FullKey = c.Styles.HelpKey
	h.Styles.FullDesc = c.Styles.HelpValue
	return h
}

// New creates a new Footer.
func New(c common.Common, keymap help.KeyMap) *Footer {
	f := &Footer{
		common: c,
		help:   NewHelp(c),
		keymap: keymap,
	}
	f.SetSize(c.Width, c.Height)
	return f
}

// SetSize implements common.Component.
func (f *Footer) SetSize(width, height int) {
	f.common.SetSize(width, height)
	f.help.SetWidth(width -
		f.common.Styles.Footer.GetHorizontalFrameSize())
}

// Init implements tea.Model.
func (f *Footer) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model.
func (f *Footer) Update(_ tea.Msg) (common.Model, tea.Cmd) {
	return f, nil
}

// View implements tea.Model.
func (f *Footer) View() string {
	if f.keymap == nil {
		return ""
	}
	s := f.common.Styles.Footer.
		Width(f.common.Width)
	helpView := f.help.View(f.keymap)
	if !f.help.ShowAll {
		helpView = f.shortView()
	}
	if !f.help.ShowAll {
		// The help model adds items past its width when no "…" fits
		// after the last one that does; keep the short help on its line.
		helpView = ansi.Truncate(helpView, max(f.common.Width-f.common.Styles.Footer.GetHorizontalFrameSize(), 0), "…")
	}
	return f.common.Zone.Mark(
		"footer",
		s.Render(helpView),
	)
}

// shortView renders the short help, keeping a final "?" entry visible when
// the actions before it do not all fit.
func (f *Footer) shortView() string {
	kb := f.keymap.ShortHelp()
	n := len(kb)
	if n < 2 || kb[n-1].Help().Key != "?" {
		return f.help.ShortHelpView(kb)
	}
	h := f.help
	last := h.ShortHelpView(kb[n-1:])
	sep := h.Styles.ShortSeparator.Render(h.ShortSeparator)
	rest := h.ShortHelpView(kb[:n-1])
	room := h.Width() - lipgloss.Width(sep) - lipgloss.Width(last)
	if lipgloss.Width(rest) > room {
		h.SetWidth(max(room, 1))
		rest = h.ShortHelpView(kb[:n-1])
	}
	if rest == "" {
		return last
	}
	return rest + sep + last
}

// ShortHelp returns the short help key bindings.
func (f *Footer) ShortHelp() []key.Binding {
	return f.keymap.ShortHelp()
}

// FullHelp returns the full help key bindings.
func (f *Footer) FullHelp() [][]key.Binding {
	return f.keymap.FullHelp()
}

// ShowAll returns whether the full help is shown.
func (f *Footer) ShowAll() bool {
	return f.help.ShowAll
}

// SetShowAll sets whether the full help is shown.
func (f *Footer) SetShowAll(show bool) {
	f.help.ShowAll = show
}

// Height returns the height of the footer.
func (f *Footer) Height() int {
	return lipgloss.Height(f.View())
}

// ToggleFooterCmd sends a ToggleFooterMsg to show/hide the help footer.
func ToggleFooterCmd() tea.Msg {
	return ToggleFooterMsg{}
}
