package selection

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/sessions"
	"github.com/dustin/go-humanize"
)

var _ sort.Interface = Items{}

// Items is a list of Item.
type Items []Item

// Len implements sort.Interface.
func (it Items) Len() int {
	return len(it)
}

// Less implements sort.Interface.
func (it Items) Less(i int, j int) bool {
	if it[i].lastUpdate == nil && it[j].lastUpdate != nil {
		return false
	}
	if it[i].lastUpdate != nil && it[j].lastUpdate == nil {
		return true
	}
	if it[i].lastUpdate == nil && it[j].lastUpdate == nil {
		return it[i].repo.Name() < it[j].repo.Name()
	}
	return it[i].lastUpdate.After(*it[j].lastUpdate)
}

// Swap implements sort.Interface.
func (it Items) Swap(i int, j int) {
	it[i], it[j] = it[j], it[i]
}

// Item represents a single item in the selector.
type Item struct {
	repo       proto.Repository
	lastUpdate *time.Time
	cmd        string
}

// New creates a new Item.
func NewItem(c common.Common, repo proto.Repository) (Item, error) {
	var lastUpdate *time.Time
	lu := repo.UpdatedAt()
	if !lu.IsZero() {
		lastUpdate = &lu
	}
	var cmd string
	if cfg := c.Config(); cfg != nil {
		cmd = c.CloneCmd(cfg.SSH.PublicURL, repo.Name(), repo.Vcs())
	}
	return Item{
		repo:       repo,
		lastUpdate: lastUpdate,
		cmd:        cmd,
	}, nil
}

// ID implements selector.IdentifiableItem.
func (i Item) ID() string {
	return i.repo.Name()
}

// Title returns the item title. Implements list.DefaultItem.
func (i Item) Title() string {
	name := i.repo.ProjectName()
	if name == "" {
		name = i.repo.Name()
	}

	return name
}

// Description returns the item description. Implements list.DefaultItem.
func (i Item) Description() string { return strings.TrimSpace(i.repo.Description()) }

// FilterValue implements list.Item.
func (i Item) FilterValue() string { return i.Title() }

// Command returns the item Command view.
func (i Item) Command() string {
	return i.cmd
}

// ItemDelegate is the delegate for the item.
type ItemDelegate struct {
	common     *common.Common
	activePane *pane
	copiedID   string
	// badges are the session and origin badges; nil for none.
	badges *badges
	// updatedW is the width of the longest "Updated …", and titleW of
	// the longest title, so the badges line up from row to row.
	updatedW int
	titleW   int
}

// NewItemDelegate creates a new ItemDelegate. b holds the badges it
// shows on the title line; nil for none.
func NewItemDelegate(common *common.Common, activePane *pane, b *badges) *ItemDelegate {
	return &ItemDelegate{
		common:     common,
		activePane: activePane,
		badges:     b,
	}
}

// updated renders when a repository was last updated.
func updated(i Item) string {
	if i.lastUpdate == nil {
		return ""
	}
	return fmt.Sprintf(" Updated %s", humanize.Time(*i.lastUpdate))
}

// setUpdatedWidth fits the "Updated" column, and the room left for
// titles, to the items.
func (d *ItemDelegate) setUpdatedWidth(items Items) {
	d.updatedW, d.titleW = 0, 0
	for _, it := range items {
		d.updatedW = max(d.updatedW, lipgloss.Width(updated(it)))
		title := it.Title() + " " // the selected row's trailing space
		if it.repo.IsPrivate() {
			title += " 🔒"
		}
		d.titleW = max(d.titleW, lipgloss.Width(title))
	}
}

// Width returns the item width.
func (d ItemDelegate) Width() int {
	width := d.common.Styles.MenuItem.GetHorizontalFrameSize() + d.common.Styles.MenuItem.GetWidth()
	return width
}

// Height returns the item height. Implements list.ItemDelegate.
func (d *ItemDelegate) Height() int {
	height := d.common.Styles.MenuItem.GetVerticalFrameSize() + d.common.Styles.MenuItem.GetHeight()
	return height
}

// Spacing returns the spacing between items. Implements list.ItemDelegate.
func (d *ItemDelegate) Spacing() int { return 1 }

// Update implements list.ItemDelegate.
func (d *ItemDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd {
	item, ok := m.SelectedItem().(Item)
	if !ok {
		return nil
	}
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.common.KeyMap.Copy):
			d.copiedID = item.ID()
			return tea.Batch(
				tea.SetClipboard(item.Command()),
				m.SetItem(m.GlobalIndex(), item),
			)
		}
	}
	return nil
}

// Render implements list.ItemDelegate.
func (d *ItemDelegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) {
	i := listItem.(Item)
	s := strings.Builder{}
	var matchedRunes []int

	// Conditions
	var (
		isSelected = index == m.Index()
		isFiltered = m.FilterState() == list.Filtering || m.FilterState() == list.FilterApplied
	)

	styles := d.common.Styles.RepoSelector.Normal
	if isSelected {
		styles = d.common.Styles.RepoSelector.Active
	}

	title := i.Title()
	title = common.TruncateString(title, m.Width()-styles.Base.GetHorizontalFrameSize())
	if i.repo.IsPrivate() {
		title += " 🔒"
	}
	if isSelected {
		title += " "
	}
	updatedStr := updated(i)
	var right string
	if d.badges != nil && (d.badges.sessionsOn || d.badges.originsOn) {
		right = d.badgeColumns(i, styles.Updated, m.Width()-styles.Base.GetHorizontalFrameSize(), lipgloss.Width(title))
	} else {
		if m.Width()-styles.Base.GetHorizontalFrameSize()-lipgloss.Width(updatedStr)-lipgloss.Width(title) <= 0 {
			updatedStr = ""
		}
		updatedStyle := styles.Updated.
			Align(lipgloss.Right).
			Width(m.Width() - styles.Base.GetHorizontalFrameSize() - lipgloss.Width(title))
		right = updatedStyle.Render(updatedStr)
	}

	if isFiltered && index < len(m.VisibleItems()) {
		// Get indices of matched characters
		matchedRunes = m.MatchesForItem(index)
	}

	if isFiltered {
		unmatched := styles.Title.Inline(true)
		matched := unmatched.Underline(true)
		title = lipgloss.StyleRunes(title, matchedRunes, matched, unmatched)
	}
	title = styles.Title.Render(title)
	desc := i.Description()
	desc = common.TruncateString(desc, m.Width()-styles.Base.GetHorizontalFrameSize())
	desc = styles.Desc.Render(desc)

	s.WriteString(lipgloss.JoinHorizontal(lipgloss.Bottom, title, right))
	s.WriteRune('\n')
	s.WriteString(desc)
	s.WriteRune('\n')

	cmd := i.Command()
	cmdStyler := styles.Command.Render
	if d.copiedID == i.ID() {
		cmd = "(copied to clipboard)"
		cmdStyler = styles.Desc.Render
		d.copiedID = ""
	}
	cmd = common.TruncateString(cmd, m.Width()-styles.Base.GetHorizontalFrameSize())
	s.WriteString(cmdStyler(cmd))
	fmt.Fprint(w, //nolint:errcheck
		d.common.Zone.Mark(i.ID(),
			styles.Base.Render(s.String()),
		),
	)
}

// Badge column widths: "● 12 sessions", and its compact "●12".
const (
	sessionBadgeWidth = 13
	compactBadgeWidth = 4
	badgeGap          = 2
)

// badgeColumns renders the right part of a title line of cw columns
// whose title takes titleW: the session badge, the origin badge and when
// the repository was last updated, each in its column so they line up
// from row to row (section 4). What does not fit is dropped: first the
// update time, then the origin, then the session badge's word, then the
// session badge. Every row drops the same, fitting the longest title (up
// to half the width); a longer title drops more on its own row.
func (d *ItemDelegate) badgeColumns(i Item, dim lipgloss.Style, cw, titleW int) string {
	if line, ok := d.badgeLine(i, dim, cw-min(max(d.titleW, titleW), cw/2), cw-titleW); ok {
		return line
	}
	line, _ := d.badgeLine(i, dim, cw-titleW, cw-titleW)
	return line
}

// badgeLine renders the badges that fit in fit columns, padded to room;
// false when they do not fit in room.
func (d *ItemDelegate) badgeLine(i Item, dim lipgloss.Style, fit, room int) (string, bool) {
	b := d.badges
	st := d.common.Styles
	name := i.ID()

	originW := 0
	origin := ""
	if b.originsOn {
		origin = b.originText(name)
		originW = lipgloss.Width("local")
		for repo := range b.origins {
			originW = max(originW, lipgloss.Width(b.originText(repo)))
		}
	}
	sessText, sb := "", sessions.RepoBadge{}
	if b.sessionsOn {
		sessText, sb = b.sessionText(name, false)
	}
	upd := d.updatedW

	width := func(sessW, originW, upd int) int {
		n := 0
		for _, w := range []int{sessW, originW, upd} {
			if w > 0 {
				if n > 0 {
					n += badgeGap
				}
				n += w
			}
		}
		return n
	}
	sessW := 0
	if b.sessionsOn {
		sessW = sessionBadgeWidth
	}
	compact := false
	// Leave at least a space after the title.
	fit--
	if width(sessW, originW, upd) > fit {
		upd = 0
	}
	if width(sessW, originW, upd) > fit {
		originW = 0
	}
	if sessW > 0 && width(sessW, originW, upd) > fit {
		sessW, compact = compactBadgeWidth, true
	}
	if width(sessW, originW, upd) > fit {
		sessW = 0
	}
	total := width(sessW, originW, upd)
	if total <= 0 {
		return "", true
	}
	if total > room-1 {
		return "", false
	}

	var cols []string
	if sessW > 0 {
		if compact {
			sessText, _ = b.sessionText(name, true)
		}
		text := ""
		if sessText != "" {
			text = sessions.StateStyle(st, sb.State).Render(sessText)
		}
		cols = append(cols, padRight(text, sessW))
	}
	if originW > 0 {
		cols = append(cols, dim.Render(padRight(origin, originW)))
	}
	if upd > 0 {
		cols = append(cols, dim.Render(fmt.Sprintf("%*s", upd, strings.TrimSpace(updated(i)))))
	}
	line := strings.Join(cols, strings.Repeat(" ", badgeGap))
	return strings.Repeat(" ", max(room-total, 1)) + line, true
}

// padRight pads s with spaces to n cells.
func padRight(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}
