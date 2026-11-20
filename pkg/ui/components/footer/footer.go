package footer

import (
	"slices"
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
	// fullHeight is the most lines the full help takes; 0: any.
	fullHeight int
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
	// RankPrimary is the context's answer: enter, y, esc, and r retry when
	// the tab failed.
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
	case common.KindOf(b) == common.ActionPrimary || h.Key == "esc" || h.Key == "y" || h.Desc == "retry":
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

// fullGap is the space between the full help's columns.
const fullGap = "    "

// FullView renders groups as columns, wrapping to a band below rather than cutting at width; over
// height lines the lowest-ranked keys (mostly on the action menu too) give way, never ? or q. <= 0: no limit.
func FullView(h help.Model, groups [][]key.Binding, width, height int) string {
	on := make([][]key.Binding, len(groups))
	for i, g := range groups {
		for _, b := range g {
			if shown(b) {
				on[i] = append(on[i], b)
			}
		}
	}
	for {
		v := fullBands(h, on, width)
		if height <= 0 || lipgloss.Height(v) <= height || !dropLowest(on) {
			return v
		}
	}
}

// dropLowest removes the last of the lowest ranked keys that may give
// way, reporting whether there was one.
func dropLowest(groups [][]key.Binding) bool {
	gi, bi, worst := -1, -1, RankMore
	for i, g := range groups {
		for j, b := range g {
			if r := RankOf(b); r != RankQuit && r != RankMore && r >= worst {
				gi, bi, worst = i, j, r
			}
		}
	}
	if gi < 0 {
		return false
	}
	groups[gi] = slices.Delete(groups[gi], bi, bi+1)
	return true
}

// fullBands lays out groups as columns, in bands at most width wide.
func fullBands(h help.Model, groups [][]key.Binding, width int) string {
	var cols []string
	for _, g := range groups {
		if len(g) == 0 {
			continue
		}
		keys, descs := make([]string, len(g)), make([]string, len(g))
		for i, b := range g {
			keys[i], descs[i] = b.Help().Key, b.Help().Desc
		}
		cols = append(cols, lipgloss.JoinHorizontal(lipgloss.Top,
			h.Styles.FullKey.Render(strings.Join(keys, "\n")), " ",
			h.Styles.FullDesc.Render(strings.Join(descs, "\n"))))
	}
	gap := h.Styles.FullSeparator.Inline(true).Render(fullGap)
	var bands, band []string
	used := 0
	for _, c := range cols {
		w := lipgloss.Width(c)
		if len(band) > 0 && width > 0 && used+len(fullGap)+w > width {
			bands = append(bands, lipgloss.JoinHorizontal(lipgloss.Top, band...))
			band, used = nil, 0
		}
		if len(band) > 0 {
			band = append(band, gap)
			used += len(fullGap)
		}
		band = append(band, c)
		used += w
	}
	if len(band) > 0 {
		bands = append(bands, lipgloss.JoinHorizontal(lipgloss.Top, band...))
	}
	lines := strings.Split(strings.Join(bands, "\n\n"), "\n")
	for i, l := range lines {
		// Only a column wider than the screen on its own is cut.
		if width > 0 {
			l = ansi.Truncate(l, width, "…")
		}
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
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

// SetFullHeight sets the most lines the full help takes; 0: any.
func (f *Footer) SetFullHeight(n int) { f.fullHeight = n }

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
	helpView := FullView(f.help, f.keymap.FullHelp(), f.help.Width(), f.fullHeight)
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
