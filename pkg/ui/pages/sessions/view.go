package sessions

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/sessionctl"
	"github.com/danielboothcloud/sundae/pkg/signals"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/footer"
	"github.com/danielboothcloud/sundae/pkg/ui/styles"
)

// Signals pane column widths (wide layout): the session and state
// columns fit the rows shown, within these bounds.
const (
	sigTimeWidth     = 8
	sigWhereMaxWidth = 24
	sigWordsMaxWidth = 22
	sigGap           = "  "
	ageWidth         = 5
)

// gatewayDown is the line that replaces the session list when the
// gateway cannot be reached (section 5).
const gatewayDown = "OpenShell gateway unreachable (r to retry)"

// stateStyle returns the colour of a state (section 2).
func stateStyle(st *styles.Styles, s events.State) lipgloss.Style {
	switch s {
	case events.StateBlocked:
		return st.State.Blocked
	case events.StateError:
		return st.State.Error
	case events.StateDone:
		return st.State.Done
	case events.StateWorking:
		return st.State.Working
	case events.StateIdle:
		return st.State.Idle
	}
	return st.State.None
}

// narrow reports whether the tab uses its narrow layout.
func (m *Sessions) narrow() bool { return m.common.Width < NarrowWidth }

// collapsed reports whether the frame's tabs are collapsed to the active
// one, "‹ Sessions ›", naming the Sessions pane: the home page's and the
// repository page's both are when narrow.
func (m *Sessions) collapsed() bool { return m.narrow() }

// where names a session; just the name when the tab is scoped to its only
// repository.
func (m *Sessions) where(name string, repos []string) string {
	if m.scope != "" && len(repos) == 1 && repos[0] == m.scope {
		if name == "" {
			name = openshell.DefaultSessionName
		}
		return signals.Sanitize(name, 0)
	}
	return sessionLabel(name, repos)
}

// View implements tea.Model.
func (m *Sessions) View() string {
	if m.src == nil {
		return ""
	}
	w, h := m.common.Width, m.common.Height
	var lines []string
	switch {
	case m.form != nil:
		lines = append([]string{""}, indent(strings.Split(m.form.View(w-1), "\n"), " ")...)
	case m.op != nil:
		lines = append([]string{""}, m.opView(w)...)
	default:
		if m.collapsed() && m.pane == sessionsPane && !m.typing {
			// The collapsed tabs name the pane; keep the lines for rows.
			lines = append(lines, "")
		} else {
			lines = append(lines, "", m.paneHeader(w), "")
		}
		body := h - len(lines)
		if m.notice.text != "" || m.inlineConfirm() {
			body -= 2
		}
		if m.pane == signalsPane {
			lines = append(lines, m.signalsView(w, body)...)
		} else {
			lines = append(lines, m.stackedView(w, body)...)
		}
	}
	if m.inlineConfirm() {
		for len(lines) < h-2 {
			lines = append(lines, "")
		}
		lines = append(lines, "", " "+m.confirm.View(w-2))
	} else if m.notice.text != "" {
		style := m.common.Styles.Prompt.Notice
		if m.notice.err {
			style = m.common.Styles.Prompt.Error
		}
		for len(lines) < h-2 {
			lines = append(lines, "")
		}
		lines = append(lines, "", " "+style.Render(common.TruncateString(m.notice.text, w-1)))
	}
	if len(lines) > h {
		lines = lines[:max(h, 0)]
	}
	return lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(strings.Join(lines, "\n"))
}

func indent(lines []string, pad string) []string {
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return lines
}

// opView shows a session being started or resumed: its title, then its
// progress lines, the latest at the bottom.
func (m *Sessions) opView(w int) []string {
	st := m.common.Styles
	lines := []string{" " + st.RepoSelector.Normal.Title.Render(common.TruncateString(m.op.title, w-1)), ""}
	progress := m.opLines
	if room := m.common.Height - 4; room > 0 && len(progress) > room {
		progress = progress[len(progress)-room:]
	}
	for _, l := range progress {
		lines = append(lines, "   "+st.RepoSelector.Normal.Desc.Render(common.TruncateString(l, w-3)))
	}
	if len(m.opLines) == 0 {
		lines = append(lines, "   "+st.RepoSelector.Normal.Desc.Render("…"))
	}
	lines = append(lines, "", " "+footer.NewHelp(m.common).ShortHelpView([]key.Binding{m.keys.Cancel}))
	return lines
}

// paneHeader is the line naming the two panes, the active one in ‹ ›,
// with what the active pane shows on the right; the filter input while
// the filter is typed.
func (m *Sessions) paneHeader(w int) string {
	st := m.common.Styles
	if m.typing {
		m.filter.SetWidth(max(w-4, 8))
		return " " + m.filter.View()
	}
	left := " "
	switch {
	case m.pane == signalsPane:
		left += st.TopLevelActiveTab.UnsetMarginRight().Render("‹ Signal history ›")
	case !m.collapsed():
		// The collapsed tabs already read ‹ Sessions ›.
		left += st.TopLevelActiveTab.UnsetMarginRight().Render("Sessions")
	}
	right := m.paneInfo()
	gap := w - lipgloss.Width(left) - lipgloss.Width(right) - 1
	if gap < 2 {
		right = common.TruncateString(right, max(w-lipgloss.Width(left)-3, 0))
		gap = max(w-lipgloss.Width(left)-lipgloss.Width(right)-1, 1)
	}
	return left + strings.Repeat(" ", gap) + st.RepoSelector.Normal.Desc.Render(right)
}

// paneInfo says what the active pane shows: the number of sessions, or
// whether the feed follows and how many signals are kept; with the filter.
func (m *Sessions) paneInfo() string {
	var parts []string
	if m.pane == signalsPane {
		if m.sigCursor == 0 {
			parts = append(parts, "following")
		} else {
			p := "paused"
			if m.unseen > 0 {
				p += fmt.Sprintf(" · %d new", m.unseen)
			}
			parts = append(parts, p)
		}
		if m.attention {
			parts = append(parts, "attention only")
		}
		if m.sigQuery != "" {
			parts = append(parts, "/"+m.sigQuery)
		}
		if m.attention || m.sigQuery != "" || m.scope != "" {
			parts = append(parts, fmt.Sprintf("%d of %d", len(m.visibleSignals()), len(m.sigs)))
		} else {
			parts = append(parts, fmt.Sprintf("%d kept", len(m.sigs)))
		}
		return strings.Join(parts, " · ")
	}
	if !m.loaded || m.listErr != nil {
		return ""
	}
	n := len(m.scopedRows())
	if m.rowQuery != "" {
		return fmt.Sprintf("/%s · %d of %d", m.rowQuery, len(m.visibleRows()), n)
	}
	if n == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", n)
}

// sessionRowHeight is the lines of a session row, its spacing included.
func (m *Sessions) sessionRowHeight() int {
	if m.narrow() {
		return 4
	}
	return 3
}

func (m *Sessions) signalRowHeight() int {
	if m.narrow() {
		return 2
	}
	return 1
}

// perPage is how many rows of the active pane fit.
func (m *Sessions) perPage() int {
	body := m.common.Height - 3
	if m.pane == signalsPane {
		return max(body/m.signalRowHeight(), 1)
	}
	sessH, _, _ := m.stackLayout(body)
	return max((sessH+1)/m.sessionRowHeight(), 1)
}

// Signals section sizes of the stacked view: about a third of the
// height, within these bounds; under minStackSessions lines left for the
// sessions it collapses to one line. It never takes more rows than the
// session has signals.
const (
	minSignalRows    = 3
	maxSignalRows    = 8
	minStackSessions = 5
)

// stackLayout splits h lines between the sessions and the selected
// session's signals: the sessions' lines, the signal rows, and whether
// the signals collapse to one line. With no session selected the
// sessions get all of it.
func (m *Sessions) stackLayout(h int) (sessH, sigRows int, compact bool) {
	if _, ok := m.selected(); !ok || m.listErr != nil {
		return h, 0, false
	}
	sigRows = min(max(h/3, minSignalRows), maxSignalRows)
	// Rows the session has no signals for go to the sessions.
	sigRows = min(sigRows, max(len(m.sessionSignals()), 1))
	// A blank line and the title come before the rows.
	if !m.narrow() && h-2-sigRows >= minStackSessions {
		return h - 2 - sigRows, sigRows, false
	}
	// One blank line and the signal line.
	return max(h-2, 1), 0, true
}

// stackedView is the default view: the sessions, and below them the
// selected session's latest signals, newest first.
func (m *Sessions) stackedView(w, h int) []string {
	sessH, sigRows, compact := m.stackLayout(h)
	lines := m.sessionsView(w, sessH)
	r, ok := m.selected()
	if !ok || m.listErr != nil {
		return lines
	}
	for len(lines) < sessH {
		lines = append(lines, "")
	}
	st := m.common.Styles
	desc := st.RepoSelector.Normal.Desc
	name := m.where(r.Info.Name, rowRepos(r))
	sigs := m.sessionSignals()
	if compact {
		title := "Signals · "
		if len(sigs) == 0 {
			return append(lines, "", " "+desc.Render(common.TruncateString(title+name+": none yet", w-1)))
		}
		s := sigs[0]
		glyph, words, _ := strings.Cut(s.Label(), " ")
		ss := m.sigStyle(s)
		head := " " + desc.Render(signals.FormatTime(s.Time, m.now())) + " " + ss.Render(glyph+" "+words) + "  "
		return append(lines, "", head+desc.Render(common.TruncateString(s.Message(0), max(w-lipgloss.Width(head)-1, 0))))
	}
	title := " " + st.TopLevelActiveTab.UnsetMarginRight().Render(common.TruncateString("Signals · "+name, w-4))
	if n := len(sigs); n > 0 {
		info := fmt.Sprintf("e full history · %d", n)
		if gap := w - lipgloss.Width(title) - len(info) - 1; gap >= 2 {
			title += strings.Repeat(" ", gap) + desc.Render(info)
		}
	}
	lines = append(lines, "", title)
	if len(sigs) == 0 {
		return append(lines, "   "+desc.Render("No signals from this session yet."))
	}
	wordsW := 6
	for _, s := range sigs[:min(sigRows, len(sigs))] {
		_, words, _ := strings.Cut(s.Label(), " ")
		wordsW = max(wordsW, ansi.StringWidth(words))
	}
	wordsW = min(wordsW, sigWordsMaxWidth)
	for _, s := range sigs[:min(sigRows, len(sigs))] {
		glyph, words, _ := strings.Cut(s.Label(), " ")
		ss := m.sigStyle(s)
		row := "   " + desc.Render(pad(signals.FormatTime(s.Time, m.now()), sigTimeWidth)) + sigGap +
			ss.Render(glyph) + " " + ss.Render(pad(truncate(words, wordsW), wordsW)) + sigGap
		lines = append(lines, row+desc.Render(common.TruncateString(s.Message(0), max(w-lipgloss.Width(row)-1, 0))))
	}
	return lines
}

// sigStyle is the colour of a signal's state; missed signals are not one.
func (m *Sessions) sigStyle(s signals.Signal) lipgloss.Style {
	if s.IsGap() {
		return m.common.Styles.RepoSelector.Normal.Desc
	}
	return stateStyle(m.common.Styles, s.State())
}

func (m *Sessions) sessionsView(w, h int) []string {
	st := m.common.Styles
	switch {
	case m.listErr != nil:
		line := gatewayDown
		cause := signals.Sanitize(m.listErr.Error(), 0)
		out := " " + st.Prompt.Error.Render("✗ "+common.TruncateString(line, w-3))
		if room := w - 3 - lipgloss.Width(line) - 4; room > 8 {
			out += st.RepoSelector.Normal.Desc.Render("  · " + common.TruncateString(cause, room))
		}
		return []string{out}
	case !m.loaded:
		return []string{" " + st.RepoSelector.Normal.Desc.Render("Loading sessions…")}
	case len(m.scopedRows()) == 0:
		text := "No sessions. Press n to start one."
		if m.scope != "" {
			text = "No sessions of this repository. Press n to start one."
		}
		return []string{" " + st.NoContent.UnsetMargins().UnsetPadding().Render(common.TruncateString(text, w-1))}
	}
	vis := m.visibleRows()
	if len(vis) == 0 {
		return []string{" " + st.RepoSelector.Normal.Desc.Render("No sessions match “"+m.rowQuery+"”.")}
	}
	per := max((h+1)/m.sessionRowHeight(), 1)
	start := (m.cursor / per) * per
	var lines []string
	for i := start; i < len(vis) && i < start+per; i++ {
		if i > start {
			lines = append(lines, "")
		}
		lines = append(lines, m.sessionRow(vis[i], i == m.cursor, w)...)
	}
	return lines
}

// words is a session's state words, with what the tab is doing to it in
// place of the lifecycle.
func (m *Sessions) words(r sessionctl.Row) string {
	if b := m.busy[r.Info.Sandbox]; b != "" {
		return signals.RecordWords(r.Record()) + " · " + b
	}
	return r.Words()
}

// sessionRow renders a session as two lines (three when narrow), as
// section 5 draws: glyph, repo / name, state and age; then the message.
func (m *Sessions) sessionRow(r sessionctl.Row, selected bool, w int) []string {
	st := m.common.Styles
	sel := st.RepoSelector.Normal
	if selected {
		sel = st.RepoSelector.Active
	}
	base := sel.Base.UnsetHeight()
	cw := max(w-base.GetHorizontalFrameSize(), 10)
	ss := stateStyle(st, r.State())
	glyph := ss.Render(r.State().Glyph())
	where := m.where(r.Info.Name, rowRepos(r))
	words := m.words(r)
	age := sessionctl.Since(r.Changed(), m.now())
	if age == "-" {
		age = ""
	}
	msg := m.message(r)
	msgStyle := sel.Desc
	if m.copied == r.Info.Sandbox {
		msg = "(copied to clipboard)"
	}

	var lines []string
	if m.narrow() {
		lines = []string{
			glyph + " " + sel.Title.Render(common.TruncateString(where, cw-2)),
			"  " + ss.Render(common.TruncateString(words, cw-2-ageWidth)) + sel.Updated.Render(strings.TrimRight("  "+age, " ")),
			"  " + msgStyle.Render(common.TruncateString(msg, cw-2)),
		}
	} else {
		right := words + "  " + fmt.Sprintf("%*s", ageWidth-2, age)
		if lipgloss.Width(right) > cw/2 {
			words = common.TruncateString(words, cw/2-ageWidth)
			right = words + "  " + fmt.Sprintf("%*s", ageWidth-2, age)
		}
		where = common.TruncateString(where, cw-2-lipgloss.Width(right)-2)
		gap := max(cw-2-lipgloss.Width(where)-lipgloss.Width(right), 2)
		lines = []string{
			glyph + " " + sel.Title.Render(where) + strings.Repeat(" ", gap) +
				ss.Render(words) + sel.Updated.Render("  "+fmt.Sprintf("%*s", ageWidth-2, age)),
			"  " + msgStyle.Render(common.TruncateString(msg, cw-2)),
		}
	}
	return strings.Split(base.Render(strings.Join(lines, "\n")), "\n")
}

func (m *Sessions) signalsView(w, h int) []string {
	st := m.common.Styles
	vis := m.visibleSignals()
	if len(vis) == 0 {
		text := "No signals yet. Sessions report here as they work."
		if len(m.sigs) > 0 {
			text = "No signals match."
		}
		return []string{" " + st.RepoSelector.Normal.Desc.Render(text)}
	}
	per := max(h/m.signalRowHeight(), 1)
	start := (m.sigCursor / per) * per
	end := min(start+per, len(vis))
	// The session and state columns fit the rows shown.
	whereW, wordsW := 10, 6
	for _, s := range vis[start:end] {
		_, words, _ := strings.Cut(s.Label(), " ")
		whereW = max(whereW, ansi.StringWidth(m.signalWhere(s)))
		wordsW = max(wordsW, ansi.StringWidth(words))
	}
	whereW, wordsW = min(whereW, sigWhereMaxWidth), min(wordsW, sigWordsMaxWidth)
	var lines []string
	for i := start; i < end; i++ {
		lines = append(lines, m.signalRow(vis[i], i == m.sigCursor, w, whereW, wordsW)...)
	}
	return lines
}

// signalRow renders a signal (section 7): time, glyph, repo / session,
// state and message; two lines when narrow.
func (m *Sessions) signalRow(s signals.Signal, selected bool, w, whereW, wordsW int) []string {
	st := m.common.Styles
	sel := st.RepoSelector.Normal
	if selected {
		sel = st.RepoSelector.Active
	}
	base := sel.Base.UnsetHeight()
	cw := max(w-base.GetHorizontalFrameSize(), 10)
	ss := stateStyle(st, s.State())
	if s.IsGap() {
		// Missed signals are not an agent state.
		ss = sel.Desc
	}
	glyph, words, _ := strings.Cut(s.Label(), " ")
	when := signals.FormatTime(s.Time, m.now())
	where := m.signalWhere(s)
	var lines []string
	if m.narrow() {
		head := pad(when, sigTimeWidth) + " "
		lines = []string{
			sel.Updated.Render(head) + ss.Render(glyph) + " " + sel.Title.Render(common.TruncateString(where, cw-lipgloss.Width(head)-2)),
			"  " + ss.Render(common.TruncateString(words, cw-2)) + "  " +
				sel.Desc.Render(common.TruncateString(s.Message(0), max(cw-4-lipgloss.Width(words), 0))),
		}
	} else {
		fixed := sigTimeWidth + len(sigGap) + 2 + whereW + len(sigGap) + wordsW + len(sigGap)
		lines = []string{
			sel.Updated.Render(pad(when, sigTimeWidth)) + sigGap +
				ss.Render(glyph) + " " +
				sel.Title.Render(pad(truncate(where, whereW), whereW)) + sigGap +
				ss.Render(pad(truncate(words, wordsW), wordsW)) + sigGap +
				sel.Desc.Render(common.TruncateString(s.Message(0), max(cw-fixed, 0))),
		}
	}
	return strings.Split(base.Render(strings.Join(lines, "\n")), "\n")
}

// signalWhere is a signal's "repo / session"; without the repository
// when the tab is scoped to it.
func (m *Sessions) signalWhere(s signals.Signal) string {
	w := s.Where()
	if m.scope != "" {
		w = strings.TrimPrefix(w, signals.Sanitize(m.scope, 0)+" / ")
	}
	return w
}

// inlineConfirm reports whether the confirm shows in the tab rather than
// in the frame's footer: on the repository page, whose footer is the
// help.
func (m *Sessions) inlineConfirm() bool { return m.confirm != nil && m.scope != "" }

// truncate cuts s to n cells, ending in "…" when cut.
func truncate(s string, n int) string { return ansi.Truncate(s, n, "…") }

// pad pads s with spaces to n cells.
func pad(s string, n int) string {
	if w := ansi.StringWidth(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// FooterLine is what replaces the help line: the end-session confirm
// (section 5b). "" when there is none.
func (m *Sessions) FooterLine(width int) string {
	if m.confirm == nil || m.inlineConfirm() {
		return ""
	}
	return m.confirm.View(width)
}

// ShortHelp implements help.KeyMap.
func (m *Sessions) ShortHelp() []key.Binding {
	switch {
	case m.form != nil:
		return m.form.ShortHelp()
	case m.confirm != nil:
		return []key.Binding{m.confirm.Yes, m.confirm.No}
	case m.op != nil:
		return []key.Binding{m.keys.Cancel}
	case m.typing:
		return []key.Binding{m.keys.Apply, m.keys.Cancel}
	}
	pane := m.keys.Pane
	pane.SetHelp("e", "history")
	if m.pane == signalsPane {
		return []key.Binding{m.keys.Attach, m.keys.Filter, m.keys.Attention, m.backKey()}
	}
	if m.listErr != nil {
		retry := m.keys.Retry
		retry.SetHelp("r", "retry")
		return []key.Binding{retry, pane}
	}
	return []key.Binding{m.keys.Attach, m.keys.New, m.keys.Filter, m.keys.End, pane}
}

// backKey is esc in the signal history: it clears the filter first.
func (m *Sessions) backKey() key.Binding {
	if m.sigQuery != "" {
		return m.keys.Clear
	}
	return m.keys.Back
}

// FullHelp implements help.KeyMap.
func (m *Sessions) FullHelp() [][]key.Binding {
	if m.form != nil || m.confirm != nil || m.op != nil || m.typing {
		return [][]key.Binding{m.ShortHelp()}
	}
	km := m.common.KeyMap
	nav := []key.Binding{km.UpDown, km.NextPage, km.PrevPage, m.keys.Follow, m.keys.Bottom}
	pane := m.keys.Pane
	if m.pane == signalsPane {
		pane.SetHelp("e", "sessions")
		return [][]key.Binding{
			nav,
			{m.keys.Attach, m.backKey(), pane, m.keys.Attention},
			{m.keys.Filter, m.keys.Clear, m.keys.Retry},
		}
	}
	return [][]key.Binding{
		nav,
		{m.keys.Attach, m.keys.New, m.keys.End, m.keys.Park},
		{m.keys.AddRepo, m.keys.DropRepo},
		{m.keys.Copy, m.keys.Filter, m.keys.Clear, pane, m.keys.Retry},
	}
}

// Actions are the keys that act on the selected row, for the home page's
// action menu: only those enabled for it now. None while a form, confirm,
// filter or operation has the keys, or there is no row.
func (m *Sessions) Actions() []key.Binding {
	if m.src == nil || m.Capturing() || m.op != nil {
		return nil
	}
	if m.pane == signalsPane {
		if m.sigCursor >= len(m.visibleSignals()) {
			return nil
		}
		open := m.keys.Attach
		open.SetHelp("enter", "open session")
		return []key.Binding{open}
	}
	r, ok := m.selected()
	if !ok || m.listErr != nil {
		return nil
	}
	busy := m.busy[r.Info.Sandbox] != ""
	attach := m.keys.Attach
	park := m.keys.Park
	park.SetHelp("z", "park")
	if r.Parked() {
		attach.SetHelp("enter", "resume & attach")
		park.SetHelp("z", "resume")
	}
	as := []key.Binding{attach}
	if !busy {
		as = append(as, park, m.keys.AddRepo)
		if len(rowRepos(r)) >= 2 {
			as = append(as, m.keys.DropRepo)
		}
	}
	return append(as, m.keys.Copy, m.keys.End)
}

// ActionsTitle names the selected row for the action menu.
func (m *Sessions) ActionsTitle() string {
	if m.pane == signalsPane {
		return "Signal"
	}
	if r, ok := m.selected(); ok {
		return sessionLabel(r.Info.Name, rowRepos(r))
	}
	return ""
}
