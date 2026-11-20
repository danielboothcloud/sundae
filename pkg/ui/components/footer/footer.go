package footer

import (
	"sort"
	"strings"

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
	h.Styles.ShortSeparator = separatorStyle(c)
	h.Styles.FullSeparator = separatorStyle(c)
	h.Styles.Ellipsis = separatorStyle(c)
	return h
}

// separatorStyle is the style of the " • " between help items.
func separatorStyle(c common.Common) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(c.Styles.HelpDivider.GetForeground())
}

// Rank is how much a key earns its place on a narrow help line: lower
// ranks are kept first, so the context's answer survives a phone width.
type Rank int

const (
	// RankMore is "? more", the way to every key that was dropped.
	RankMore Rank = iota
	// RankPrimary is the context's answer: enter, y, esc.
	RankPrimary
	// RankRow is what to do with the selected row next: space actions, n new.
	RankRow
	// RankNav is moving the cursor and switching tabs.
	RankNav
	// RankFind is filtering and the signal history.
	RankFind
	// RankOther is any other key: mostly row keys the action menu lists too.
	RankOther
	// RankQuit is q quit, which the full help always lists.
	RankQuit
)

// RankOf returns b's Rank, read from its help as common.KindOf reads its kind.
func RankOf(b key.Binding) Rank {
	h := b.Help()
	switch {
	case h.Key == "?":
		return RankMore
	case h.Desc == "quit":
		return RankQuit
	case common.KindOf(b) == common.ActionPrimary || h.Key == "esc" || h.Key == "y":
		return RankPrimary
	case h.Desc == "actions" || h.Desc == "new" || strings.HasPrefix(h.Desc, "new "):
		return RankRow
	case h.Desc == "move" || h.Desc == "tabs":
		return RankNav
	case h.Desc == "filter" || h.Desc == "history":
		return RankFind
	}
	return RankOther
}

// sepWidth is the width of the " • " between help items.
const sepWidth = 3

// shown reports whether b has a place on a help line.
func shown(b key.Binding) bool {
	return b.Enabled() && (b.Help().Key != "" || b.Help().Desc != "")
}

// itemWidth is the width of b's "key desc".
func itemWidth(b key.Binding) int {
	return ansi.StringWidth(b.Help().Key) + 1 + ansi.StringWidth(b.Help().Desc)
}

// Fit returns the enabled bindings of kb that fit a help line width cells
// wide, in kb's order: whole keys, taken by Rank, then by order. width <= 0
// keeps them all.
func Fit(kb []key.Binding, width int) []key.Binding {
	var on []key.Binding
	total := -sepWidth
	for _, b := range kb {
		if shown(b) {
			on = append(on, b)
			total += sepWidth + itemWidth(b)
		}
	}
	if width <= 0 || total <= width {
		return on
	}
	order := make([]int, len(on))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return RankOf(on[order[i]]) < RankOf(on[order[j]]) })
	// Once a key does not fit, no lower rank may take its place; keys of
	// its own rank still may.
	keep := make([]bool, len(on))
	used, full := -sepWidth, Rank(-1)
	for _, i := range order {
		r := RankOf(on[i])
		if full >= 0 && r > full {
			break
		}
		if w := sepWidth + itemWidth(on[i]); used+w <= width {
			keep[i] = true
			used += w
		} else {
			full = r
		}
	}
	var out []key.Binding
	for i, b := range on {
		if keep[i] {
			out = append(out, b)
		}
	}
	return out
}

// ShortView renders bindings as a short help line: "key desc" items
// separated by " • ", each key (and the words of one that deletes or ends
// something) coloured by its action kind (common.KindOf). Disabled
// bindings are left out. With width > 0 only the keys Fit keeps are shown.
func ShortView(c common.Common, kb []key.Binding, width int) string {
	items := make([]string, 0, len(kb))
	for _, k := range Fit(kb, width) {
		items = append(items, c.KeyStyle(k).Render(k.Help().Key)+" "+c.DescStyle(k).Render(k.Help().Desc))
	}
	return strings.Join(items, separatorStyle(c).Render(" • "))
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
	return f.view(f.common.Styles.Footer)
}

// ViewNoMargin renders the help line alone, without the footer's top margin.
func (f *Footer) ViewNoMargin() string {
	return f.view(f.common.Styles.Footer.UnsetMarginTop())
}

// ViewWithMessage renders the footer every page shares: the message slot, then
// the help. The slot stays when empty so the page above never shifts.
func (f *Footer) ViewWithMessage(text string, kind common.MessageKind) string {
	st := f.common.Styles.Footer
	slot := st.UnsetMarginTop().Width(f.common.Width)
	msg := ""
	if text != "" {
		msg = f.common.NoticeView(text, kind, f.common.Width-slot.GetHorizontalFrameSize())
	}
	return lipgloss.NewStyle().MarginTop(st.GetMarginTop()).Render(
		lipgloss.JoinVertical(lipgloss.Left, slot.Render(msg), f.ViewNoMargin()))
}

func (f *Footer) view(style lipgloss.Style) string {
	if f.keymap == nil {
		return ""
	}
	s := style.Width(f.common.Width)
	helpView := f.help.View(f.keymap)
	if !f.help.ShowAll {
		// The left-aligned keys take the right padding too: at 50 columns
		// that cell is what lets n new stay next to the row's actions.
		s = s.PaddingRight(0)
		helpView = ShortView(f.common, f.keymap.ShortHelp(), f.help.Width()+style.GetPaddingRight())
	}
	return f.common.Zone.Mark(
		"footer",
		s.Render(helpView),
	)
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
