package sessions

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/footer"
	"github.com/dustin/go-humanize"
)

// snapList is the Snapshots view of the tab (S): the user's stored S3
// snapshots, newest first, with what became of each one's session.
type snapList struct {
	rows    []openshell.StoredSnapshot
	cursor  int
	loading bool
	err     error
}

// snapKeys are the Snapshots view's keys.
type snapKeys struct {
	Delete, Prune, Refresh, Close key.Binding
}

func defaultSnapKeys() snapKeys {
	return snapKeys{
		Delete:  key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "delete snapshot")),
		Prune:   key.NewBinding(key.WithKeys("P"), key.WithHelp("P", "prune ended")),
		Refresh: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Close:   key.NewBinding(key.WithKeys("esc", "h", "S"), key.WithHelp("esc", "back")),
	}
}

// Messages of the Snapshots view.
type (
	snapListMsg struct {
		rows []openshell.StoredSnapshot
		err  error
	}
	snapDoneMsg struct {
		verb    string
		deleted []openshell.StoredSnapshot
		err     error
	}
)

// openSnapshots opens the Snapshots view and lists the snapshots.
func (m *Sessions) openSnapshots() tea.Cmd {
	m.snaps = &snapList{loading: true}
	m.notice = notice{}
	return m.snapListCmd()
}

func (m *Sessions) snapListCmd() tea.Cmd {
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		rows, err := src.Snapshots(ctx)
		return snapListMsg{rows: rows, err: err}
	}
}

func (m *Sessions) onSnapList(msg snapListMsg) {
	if m.snaps == nil {
		return
	}
	m.snaps.loading = false
	m.snaps.rows, m.snaps.err = msg.rows, msg.err
	m.snaps.cursor = max(min(m.snaps.cursor, len(m.snaps.rows)-1), 0)
}

func (m *Sessions) onSnapDone(msg snapDoneMsg) tea.Cmd {
	var total int64
	labels := make([]string, 0, len(msg.deleted))
	for _, s := range msg.deleted {
		total += s.Size
		labels = append(labels, s.Label())
	}
	switch {
	case msg.err != nil:
		m.notice = notice{text: "✗ " + msg.err.Error(), err: true}
	case msg.verb == "prune" && len(msg.deleted) == 0:
		m.notice = notice{text: "No snapshots of ended sessions to prune."}
	case msg.verb == "prune":
		m.notice = notice{text: fmt.Sprintf("Pruned %d snapshot(s), %s: %s.", len(msg.deleted), humanize.IBytes(uint64(max(total, 0))), strings.Join(labels, ", "))}
	default:
		m.notice = notice{text: "Deleted the snapshot of " + strings.Join(labels, ", ") + " (" + humanize.IBytes(uint64(max(total, 0))) + ")."}
	}
	if m.snaps == nil {
		return nil
	}
	m.snaps.loading = true
	return m.snapListCmd()
}

// onSnapKey handles a key in the Snapshots view.
func (m *Sessions) onSnapKey(msg tea.KeyPressMsg) tea.Cmd {
	sl, km := m.snaps, m.common.KeyMap
	m.notice = notice{}
	switch {
	case key.Matches(msg, m.snapKeys.Close):
		m.snaps = nil
		return nil
	case key.Matches(msg, km.Up):
		sl.cursor = max(sl.cursor-1, 0)
	case key.Matches(msg, km.Down):
		sl.cursor = min(sl.cursor+1, max(len(sl.rows)-1, 0))
	case key.Matches(msg, m.snapKeys.Refresh):
		sl.loading = true
		return m.snapListCmd()
	case key.Matches(msg, m.snapKeys.Delete):
		if sl.cursor >= len(sl.rows) {
			return nil
		}
		s := sl.rows[sl.cursor]
		switch {
		case s.State == openshell.SnapshotParked:
			m.notice = notice{text: s.Label() + " is parked to this snapshot, its only copy of /sandbox; end the session (D) to delete both.", err: true}
			return nil
		case s.Session == "":
			m.notice = notice{text: "This snapshot has no session name; prune it (P) or delete it from the bucket.", err: true}
			return nil
		}
		q := "Delete the snapshot of " + s.Label() + " (" + humanize.IBytes(uint64(max(s.Size, 0))) + ")?"
		if s.State == openshell.SnapshotLive {
			q += "  The session keeps running."
		} else {
			q += "  It cannot be restored."
		}
		ctx, src, name := m.ctx, m.src, s.Session
		m.ask(q, func() tea.Cmd {
			return func() tea.Msg {
				d, err := src.DeleteSnapshot(ctx, name)
				return snapDoneMsg{verb: "delete", deleted: []openshell.StoredSnapshot{d}, err: err}
			}
		})
	case key.Matches(msg, m.snapKeys.Prune):
		n := 0
		for _, s := range sl.rows {
			if s.State == openshell.SnapshotEnded {
				n++
			}
		}
		if n == 0 {
			m.notice = notice{text: "No snapshots of ended sessions to prune."}
			return nil
		}
		ctx, src := m.ctx, m.src
		m.ask(fmt.Sprintf("Delete the %d snapshot(s) of ended sessions?  Their work cannot be restored.", n), func() tea.Cmd {
			return func() tea.Msg {
				d, err := src.PruneSnapshots(ctx, 0)
				return snapDoneMsg{verb: "prune", deleted: d, err: err}
			}
		})
	}
	return nil
}

// snapHelp is the Snapshots view's help.
func (m *Sessions) snapHelp() []key.Binding {
	return []key.Binding{m.common.KeyMap.UpDown, m.snapKeys.Delete, m.snapKeys.Prune, m.snapKeys.Refresh, m.snapKeys.Close}
}

// snapView renders the Snapshots view in w columns, at most h lines.
func (m *Sessions) snapView(w, h int) []string {
	st := m.common.Styles
	sl := m.snaps
	title := st.Prompt.Title.Render("Snapshots")
	count := ""
	if !sl.loading && sl.err == nil {
		var total int64
		for _, s := range sl.rows {
			total += s.Size
		}
		count = st.Prompt.Detail.Render(fmt.Sprintf("%d · %s", len(sl.rows), humanize.IBytes(uint64(max(total, 0)))))
	}
	lines := []string{"", " " + title + strings.Repeat(" ", max(w-2-len("Snapshots")-lipgloss.Width(count), 2)) + count, ""}
	switch {
	case sl.loading && len(sl.rows) == 0:
		return append(lines, "   "+st.Prompt.Detail.Render("Listing snapshots…"))
	case sl.err != nil:
		return append(lines, " "+m.common.NoticeView(sl.err.Error(), true, w-1))
	case len(sl.rows) == 0:
		return append(lines, "   "+st.Prompt.Detail.Render("No snapshots. A session's /sandbox is saved when it ends or is parked (snapshots.s3.on)."))
	}
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	labelW := 0
	for _, s := range sl.rows {
		labelW = max(labelW, lipgloss.Width(s.Label()))
	}
	labelW = min(labelW, max(w/3, 12))
	room := h - len(lines) - 3
	start := max(0, min(sl.cursor-room+1, len(sl.rows)-room))
	for i := start; i < len(sl.rows) && i < start+max(room, 1); i++ {
		s := sl.rows[i]
		pointer, name := "  ", st.Prompt.Value
		if i == sl.cursor {
			pointer, name = st.Prompt.Pointer.Render("▸")+" ", st.Prompt.ActiveValue
		}
		label := s.Label()
		if lipgloss.Width(label) > labelW {
			label = common.TruncateString(label, labelW)
		}
		label += strings.Repeat(" ", max(labelW-lipgloss.Width(label), 0))
		state := st.Prompt.Detail
		switch s.State {
		case openshell.SnapshotLive:
			state = st.State.Working
		case openshell.SnapshotParked:
			state = st.State.Blocked
		}
		row := " " + pointer + name.Render(label) + "  " + state.Render(fmt.Sprintf("%-6s", s.State)) + "  " +
			st.Prompt.Detail.Render(fmt.Sprintf("%9s  %s", humanize.IBytes(uint64(max(s.Size, 0))), humanize.RelTime(s.Time, now, "ago", "from now")))
		lines = append(lines, common.TruncateString(row, w))
	}
	lines = append(lines, "", " "+footer.ShortView(m.common, []key.Binding{m.snapKeys.Delete, m.snapKeys.Prune, m.snapKeys.Close}, w-1))
	return lines
}
