// Package sessions is the home page's Sessions tab: the user's OpenShell
// agent sessions, with what their agents last reported, and the signals
// feed, in two panes. It starts, attaches to, parks, resumes and ends
// sessions, as the `session` commands do, and follows the signals History
// for live updates. See _bmad-output/planning-artifacts/tui-screens.md,
// sections 5 and 7.
package sessions

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"image/color"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/sessionctl"
	"github.com/danielboothcloud/sundae/pkg/signals"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/prompt"
	"github.com/danielboothcloud/sundae/pkg/ui/styles"
)

// NarrowWidth is the content width under which the tab, and the home
// frame, use their narrow layout (section 9): a terminal under 60 columns,
// less the frame's margins.
const NarrowWidth = 56

// refreshEvery is how often the tab redraws, so the "4m" ages stay right,
// and retries listing while the gateway is unreachable. Status changes do
// not wait for it: they arrive through the signals feed.
const refreshEvery = 30 * time.Second

type pane int

const (
	sessionsPane pane = iota
	signalsPane
)

// KeyMap is the Sessions tab's keys.
type KeyMap struct {
	Attach    key.Binding
	New       key.Binding
	End       key.Binding
	Park      key.Binding
	Copy      key.Binding
	Filter    key.Binding
	Clear     key.Binding
	Pane      key.Binding
	Retry     key.Binding
	Attention key.Binding
	Follow    key.Binding
	Bottom    key.Binding
	Apply     key.Binding
	Cancel    key.Binding
}

// DefaultKeyMap returns the tab's keys. None clashes with the frame's:
// q, ?, tab, 1-9 and esc (tui-screens.md R9).
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Attach:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "attach")),
		New:       key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new")),
		End:       key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "end")),
		Park:      key.NewBinding(key.WithKeys("z"), key.WithHelp("z", "park/resume")),
		Copy:      key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy attach command")),
		Filter:    key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Clear:     key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear filter")),
		Pane:      key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "signals")),
		Retry:     key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Attention: key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "attention only")),
		Follow:    key.NewBinding(key.WithKeys("g", "home"), key.WithHelp("g", "top / follow")),
		Bottom:    key.NewBinding(key.WithKeys("G", "end"), key.WithHelp("G", "bottom")),
		Apply:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "apply filter")),
		Cancel:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
	}
}

// notice is a one-line message shown at the bottom of the tab.
type notice struct {
	text string
	err  bool
}

// Messages.
type (
	listMsg struct {
		rows []sessionctl.Row
		err  error
	}
	followMsg struct {
		snap []signals.Signal
		feed Feed
	}
	signalsMsg struct{ sigs []signals.Signal }
	feedEndMsg struct{ err error }
	tickMsg    struct{}
	reposMsg   struct {
		repos []string
		err   error
		pre   prefill
	}
	actionMsg struct {
		key, verb  string
		repo, name string
		done       bool
		err        error
		// row is the session acted on.
		row sessionctl.Row
	}
)

// IsMsg reports whether msg is one of the tab's own messages (live
// updates, results of what it started). The frame delivers them to the tab
// even while another page is shown, so its live updates do not stop.
func IsMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case listMsg, followMsg, signalsMsg, feedEndMsg, tickMsg, reposMsg, actionMsg, opMsg, attachDoneMsg:
		return true
	}
	return false
}

// prefill is what a new-session form opens with.
type prefill struct {
	repo    string
	fixed   bool
	profile string
}

// Sessions is the Sessions tab.
type Sessions struct {
	common  common.Common
	src     Source
	handoff Handoff
	keys    KeyMap
	now     func() time.Time

	ctx     context.Context
	cancel  context.CancelFunc
	started bool

	pane pane

	// The Sessions pane.
	rows    []sessionctl.Row
	loaded  bool
	listErr error
	listing bool
	relist  bool
	cursor  int
	selKey  string
	filter  textinput.Model
	// typing reports that the filter of the active pane takes keys.
	typing   bool
	rowQuery string
	// busy maps a sandbox to the lifecycle word of what the tab is doing
	// to it: resuming, parking, ending.
	busy   map[string]string
	copied string
	// profiles remembers each session's profile, for re-creating an
	// ended session from the Signals pane.
	profiles map[string]string

	// The Signals pane: the user's signals, newest first.
	sigs      []signals.Signal
	keep      int
	feed      Feed
	sigCursor int
	unseen    int
	attention bool
	sigQuery  string

	// Overlays.
	form    *prompt.Form
	confirm *prompt.Confirm
	// onYes is what a yes to the confirm does.
	onYes   func() tea.Cmd
	target  sessionctl.Row
	op      *operation
	opLines []string

	notice notice

	// scope, when set, narrows the tab to one repository's sessions and
	// signals: the repository page's Sessions tab (section 8) shows this
	// model so scoped. scopes keeps each scope's cursor and filters.
	scope  string
	scopes map[string]viewState
}

// viewState is what the tab shows of one scope.
type viewState struct {
	pane      pane
	cursor    int
	selKey    string
	rowQuery  string
	attention bool
	sigQuery  string
}

// New returns the Sessions tab showing src. src may be nil: the tab then
// shows nothing (the frame hides it).
func New(c common.Common, src Source) *Sessions {
	ctx, cancel := context.WithCancel(c.Context())
	in := textinput.New()
	in.Prompt = "/ "
	in.CharLimit = 64
	st := in.Styles()
	st.Cursor.Blink = false
	in.SetStyles(st)
	m := &Sessions{
		common:   c,
		src:      src,
		keys:     DefaultKeyMap(),
		now:      time.Now,
		ctx:      ctx,
		cancel:   cancel,
		filter:   in,
		busy:     map[string]string{},
		profiles: map[string]string{},
		keep:     1000,
		scopes:   map[string]viewState{},
	}
	if h, ok := c.Context().Value(HandoffKey).(Handoff); ok {
		m.handoff = h
	}
	if src != nil {
		m.keep = max(src.Keep(), 1)
	}
	return m
}

// Close stops the tab's live updates and abandons what it was doing. The
// frame calls it when the TUI quits; the SSH connection ending does the
// same through the context.
func (m *Sessions) Close() {
	if m.op != nil {
		m.op.abandon()
	}
	m.cancel()
}

// Capturing reports whether the tab takes every key: a form, a confirm or
// a filter is open. The frame then leaves q, tab and the digits to it.
func (m *Sessions) Capturing() bool {
	return m.form != nil || m.confirm != nil || m.typing
}

// SetSize implements common.Component.
func (m *Sessions) SetSize(width, height int) {
	m.common.SetSize(width, height)
}

// Init implements tea.Model: it lists the sessions and starts following
// the signals feed.
func (m *Sessions) Init() tea.Cmd {
	if m.src == nil || m.started {
		return nil
	}
	m.started = true
	return tea.Batch(m.listCmd(), m.followCmd(), m.tickCmd())
}

// SetScope narrows the tab to the sessions and signals of repo; "" shows
// all of them. Each scope keeps its own cursor, pane and filters; the
// Signals pane follows again on a switch.
func (m *Sessions) SetScope(repo string) {
	if repo == m.scope {
		return
	}
	m.scopes[m.scope] = viewState{
		pane: m.pane, cursor: m.cursor, selKey: m.selKey,
		rowQuery: m.rowQuery, attention: m.attention, sigQuery: m.sigQuery,
	}
	v := m.scopes[repo]
	m.scope = repo
	m.pane, m.cursor, m.selKey = v.pane, v.cursor, v.selKey
	m.rowQuery, m.attention, m.sigQuery = v.rowQuery, v.attention, v.sigQuery
	m.sigCursor, m.unseen = 0, 0
	m.copied = ""
	m.notice = notice{}
	m.sortRows()
}

// Scope returns the repository the tab is narrowed to; "" for none.
func (m *Sessions) Scope() string { return m.scope }

// Busy reports whether the tab is starting or attaching to a session:
// esc cancels that.
func (m *Sessions) Busy() bool { return m.op != nil }

// Filtered reports whether a text filter narrows the active pane: esc
// clears it.
func (m *Sessions) Filtered() bool { return m.query() != "" }

// SelectedName returns the selected session's name and how many sessions
// the tab shows, for a status bar; "" when none is selected.
func (m *Sessions) SelectedName() (name string, index, total int) {
	vis := m.visibleRows()
	r, ok := m.selected()
	if !ok {
		return "", 0, len(vis)
	}
	return signals.Sanitize(r.Info.Name, 0), m.cursor + 1, len(vis)
}

// OpenNewSession opens the new-session form for repo; fixed keeps the
// repository from being changed (the Repositories tab's s key).
func (m *Sessions) OpenNewSession(repo string, fixed bool) tea.Cmd {
	return m.reposCmd(prefill{repo: repo, fixed: fixed})
}

// listCmd lists the sessions, unless a listing is under way: then it
// lists again when that one is done.
func (m *Sessions) listCmd() tea.Cmd {
	if m.src == nil {
		return nil
	}
	if m.listing {
		m.relist = true
		return nil
	}
	m.listing = true
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		rows, err := src.List(ctx)
		if ctx.Err() != nil {
			return nil
		}
		return listMsg{rows: rows, err: err}
	}
}

func (m *Sessions) followCmd() tea.Cmd {
	src := m.src
	return func() tea.Msg {
		snap, feed := src.Follow()
		return followMsg{snap: snap, feed: feed}
	}
}

// nextCmd waits for the next signals; it ends when the tab closes or the
// feed does.
func (m *Sessions) nextCmd() tea.Cmd {
	feed, ctx := m.feed, m.ctx
	if feed == nil {
		return nil
	}
	return func() tea.Msg {
		sigs, err := feed.Next(ctx)
		if err != nil {
			return feedEndMsg{err: err}
		}
		return signalsMsg{sigs: sigs}
	}
}

func (m *Sessions) tickCmd() tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		t := time.NewTimer(refreshEvery)
		defer t.Stop()
		select {
		case <-t.C:
			return tickMsg{}
		case <-ctx.Done():
			return nil
		}
	}
}

func (m *Sessions) reposCmd(pre prefill) tea.Cmd {
	if m.src == nil {
		return nil
	}
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		repos, err := src.Repos(ctx)
		return reposMsg{repos: repos, err: err, pre: pre}
	}
}

// Update implements tea.Model.
func (m *Sessions) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case listMsg:
		return m, m.onList(msg)
	case followMsg:
		m.feed = msg.feed
		snap := slices.Clone(msg.snap)
		slices.Reverse(snap)
		m.addSignals(snap)
		return m, m.nextCmd()
	case signalsMsg:
		m.addSignals(msg.sigs)
		return m, tea.Batch(m.react(msg.sigs), m.nextCmd())
	case feedEndMsg:
		m.feed = nil
		return m, nil
	case tickMsg:
		var cmd tea.Cmd
		if m.listErr != nil {
			cmd = m.listCmd()
		}
		return m, tea.Batch(cmd, m.tickCmd())
	case reposMsg:
		if msg.err != nil {
			m.notice = notice{text: "Cannot list repositories: " + msg.err.Error(), err: true}
			return m, nil
		}
		if len(msg.repos) == 0 && !msg.pre.fixed {
			m.notice = notice{text: "No repository to start a session for.", err: true}
			return m, nil
		}
		m.form = m.newSessionForm(msg.repos, msg.pre)
		return m, nil
	case opMsg:
		return m, m.onOp(msg.op)
	case attachDoneMsg:
		return m, m.onAttachDone(msg)
	case actionMsg:
		return m, m.onAction(msg)
	case tea.KeyPressMsg:
		return m, m.onKey(msg)
	}
	if m.form != nil {
		_, cmd := m.form.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *Sessions) onList(msg listMsg) tea.Cmd {
	m.listing = false
	if msg.err != nil {
		m.listErr = msg.err
		m.common.Logger.Debug("sessions: cannot list sessions", "err", msg.err)
	} else {
		m.listErr = nil
		m.loaded = true
		m.setRows(msg.rows)
	}
	if m.relist {
		m.relist = false
		return m.listCmd()
	}
	return nil
}

// setRows replaces the sessions, keeping the selection on the same one.
func (m *Sessions) setRows(rows []sessionctl.Row) {
	m.rows = slices.Clone(rows)
	for _, r := range rows {
		if r.Info.Profile != "" {
			m.profiles[r.Info.Sandbox] = r.Info.Profile
		}
	}
	for k := range m.busy {
		if !slices.ContainsFunc(rows, func(r sessionctl.Row) bool { return r.Info.Sandbox == k }) {
			delete(m.busy, k)
		}
	}
	m.sortRows()
}

// sortRows sorts the sessions as everywhere (blocked, error, done,
// working, idle, none), the most recently changed first within a state,
// and moves the cursor to the selected session.
func (m *Sessions) sortRows() {
	slices.SortStableFunc(m.rows, func(a, b sessionctl.Row) int {
		return cmp.Or(
			a.State().Rank()-b.State().Rank(),
			b.Changed().Compare(a.Changed()),
			strings.Compare(a.Info.Repo, b.Info.Repo),
			strings.Compare(a.Info.Name, b.Info.Name),
		)
	})
	vis := m.visibleRows()
	m.cursor = min(m.cursor, max(len(vis)-1, 0))
	for i, r := range vis {
		if r.Info.Sandbox == m.selKey {
			m.cursor = i
		}
	}
	m.syncSel()
}

func (m *Sessions) syncSel() {
	if r, ok := m.selected(); ok {
		m.selKey = r.Info.Sandbox
	}
}

// mergeStatuses refreshes the sessions' status from the Monitor.
func (m *Sessions) mergeStatuses() {
	st := m.src.Statuses()
	for i, r := range m.rows {
		if s, ok := st[r.Info.Sandbox]; ok {
			m.rows[i].Status = &s
		}
	}
	m.sortRows()
}

// react refreshes the sessions for new signals: a status change re-reads
// the Monitor, a lifecycle step (or lost signals, or a session not listed
// yet) lists the sessions again.
func (m *Sessions) react(sigs []signals.Signal) tea.Cmd {
	relist, merge := false, false
	for _, s := range sigs {
		switch {
		case s.IsGap(), s.Lifecycle != nil:
			relist = true
		case s.Status != nil:
			if m.rowIndex(s.Status.Session.Sandbox) < 0 {
				relist = true
			} else {
				merge = true
			}
		}
	}
	if merge && m.src != nil {
		m.mergeStatuses()
	}
	if relist {
		return m.listCmd()
	}
	return nil
}

func (m *Sessions) rowIndex(sandbox string) int {
	return slices.IndexFunc(m.rows, func(r sessionctl.Row) bool { return r.Info.Sandbox == sandbox })
}

// addSignals adds signals, oldest first, keeping the cursor on the same
// signal unless it is following (on the first row).
func (m *Sessions) addSignals(sigs []signals.Signal) {
	if len(sigs) == 0 {
		return
	}
	following := m.sigCursor == 0
	added := 0
	for _, s := range sigs {
		if m.sigMatch(s) {
			added++
		}
	}
	newest := slices.Clone(sigs)
	slices.Reverse(newest)
	m.sigs = append(newest, m.sigs...)
	if len(m.sigs) > m.keep {
		m.sigs = m.sigs[:m.keep]
	}
	if !following {
		m.sigCursor = min(m.sigCursor+added, max(len(m.visibleSignals())-1, 0))
		m.unseen += added
	}
}

// sigMatch reports whether the Signals pane's filter selects s.
func (m *Sessions) sigMatch(s signals.Signal) bool {
	q := signals.Query{UserID: s.UserID, Repo: m.scope, Attention: m.attention, Text: m.sigQuery}
	return q.Match(s)
}

// visibleSignals returns the signals the filter selects, newest first.
func (m *Sessions) visibleSignals() []signals.Signal {
	if !m.attention && m.sigQuery == "" && m.scope == "" {
		return m.sigs
	}
	var out []signals.Signal
	for _, s := range m.sigs {
		if m.sigMatch(s) {
			out = append(out, s)
		}
	}
	return out
}

// scopedRows returns the sessions of the scope.
func (m *Sessions) scopedRows() []sessionctl.Row {
	if m.scope == "" {
		return m.rows
	}
	var out []sessionctl.Row
	for _, r := range m.rows {
		if r.Info.Repo == m.scope {
			out = append(out, r)
		}
	}
	return out
}

// visibleRows returns the sessions of the scope the filter selects.
func (m *Sessions) visibleRows() []sessionctl.Row {
	rows := m.scopedRows()
	if m.rowQuery == "" {
		return rows
	}
	q := strings.ToLower(m.rowQuery)
	var out []sessionctl.Row
	for _, r := range rows {
		text := strings.ToLower(strings.Join([]string{r.Info.Repo, r.Info.Name, r.Words(), m.message(r)}, " "))
		if strings.Contains(text, q) {
			out = append(out, r)
		}
	}
	return out
}

func (m *Sessions) selected() (sessionctl.Row, bool) {
	vis := m.visibleRows()
	if m.cursor < 0 || m.cursor >= len(vis) {
		return sessionctl.Row{}, false
	}
	return vis[m.cursor], true
}

// message is a session's second line: what the agent last said, else the
// program's name, else the command it runs.
func (m *Sessions) message(r sessionctl.Row) string {
	msg := signals.RecordMessage(r.Record(), 0)
	if note := r.Note(); note != "" {
		if msg != "" {
			return note + " · " + msg
		}
		return note
	}
	if msg != "" {
		return msg
	}
	if m.src == nil {
		return ""
	}
	return signals.Sanitize(m.src.Command(r.Info.Profile), 0)
}

func (m *Sessions) onKey(msg tea.KeyPressMsg) tea.Cmd {
	m.copied = ""
	switch {
	case m.confirm != nil:
		res := m.confirm.Update(msg)
		if res == prompt.Pending {
			return nil
		}
		m.confirm = nil
		yes := m.onYes
		m.onYes = nil
		if res == prompt.Submitted && yes != nil {
			return yes()
		}
		return nil
	case m.form != nil:
		res, cmd := m.form.Update(msg)
		switch res {
		case prompt.Submitted:
			f := m.form
			m.form = nil
			return m.startCmd(f)
		case prompt.Cancelled:
			m.form = nil
		}
		return cmd
	case m.op != nil:
		if key.Matches(msg, m.keys.Cancel) {
			m.op.abandon()
			m.op = nil
			m.notice = notice{text: "Cancelled."}
		}
		return nil
	case m.typing:
		return m.onFilterKey(msg)
	}
	m.notice = notice{}
	if m.src == nil {
		return nil
	}
	km := m.common.KeyMap
	switch {
	case key.Matches(msg, m.keys.Pane):
		m.pane = 1 - m.pane
		return nil
	case key.Matches(msg, m.keys.Filter):
		m.typing = true
		m.filter.SetValue(m.query())
		m.filter.CursorEnd()
		return m.filter.Focus()
	case key.Matches(msg, m.keys.Clear):
		m.setQuery("")
		return nil
	case key.Matches(msg, m.keys.Retry):
		return m.listCmd()
	case key.Matches(msg, m.keys.New):
		if m.scope != "" {
			return m.reposCmd(prefill{repo: m.scope, fixed: true})
		}
		return m.reposCmd(prefill{})
	case key.Matches(msg, km.Up):
		m.move(-1)
		return nil
	case key.Matches(msg, km.Down):
		m.move(1)
		return nil
	case key.Matches(msg, km.PrevPage):
		m.move(-m.perPage())
		return nil
	case key.Matches(msg, km.NextPage):
		m.move(m.perPage())
		return nil
	case key.Matches(msg, m.keys.Follow):
		m.move(-1 << 30)
		return nil
	case key.Matches(msg, m.keys.Bottom):
		m.move(1 << 30)
		return nil
	}
	if m.pane == signalsPane {
		return m.onSignalsKey(msg)
	}
	return m.onSessionsKey(msg)
}

func (m *Sessions) query() string {
	if m.pane == signalsPane {
		return m.sigQuery
	}
	return m.rowQuery
}

func (m *Sessions) setQuery(q string) {
	if m.pane == signalsPane {
		m.sigQuery = q
		m.sigCursor, m.unseen = 0, 0
		return
	}
	m.rowQuery = q
	m.cursor = 0
	m.sortRows()
}

func (m *Sessions) onFilterKey(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, m.keys.Apply):
		m.typing = false
		m.filter.Blur()
		return nil
	case key.Matches(msg, m.keys.Cancel):
		m.typing = false
		m.filter.Blur()
		m.setQuery("")
		return nil
	}
	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(msg)
	m.setQuery(strings.TrimSpace(m.filter.Value()))
	return cmd
}

// move moves the cursor of the active pane by d rows.
func (m *Sessions) move(d int) {
	if m.pane == signalsPane {
		n := len(m.visibleSignals())
		m.sigCursor = clamp(m.sigCursor+d, 0, n-1)
		if m.sigCursor == 0 {
			m.unseen = 0
		}
		return
	}
	m.cursor = clamp(m.cursor+d, 0, len(m.visibleRows())-1)
	m.syncSel()
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return min(max(v, lo), hi)
}

func (m *Sessions) onSessionsKey(msg tea.KeyPressMsg) tea.Cmd {
	r, ok := m.selected()
	if !ok || m.listErr != nil {
		return nil
	}
	label := sessionLabel(r.Info.Repo, r.Info.Name)
	switch {
	case key.Matches(msg, m.keys.Attach):
		return m.attachCmd(r.Info.Repo, r.Info.Name, r.Info.Sandbox, r.Parked())
	case key.Matches(msg, m.keys.End):
		m.target = r
		question := "End " + label + "?  The sandbox and any unpushed work are deleted."
		switch {
		case r.ParkedToSnapshot():
			question = "End " + label + "?  It is parked to an S3 snapshot, which is kept."
		case m.src.SnapshotsOnEnd():
			question = "End " + label + "?  /sandbox is saved to S3, then the sandbox is deleted."
		}
		m.ask(question, func() tea.Cmd { return m.endCmd(r, false) })
		return nil
	case key.Matches(msg, m.keys.Park):
		if m.busy[r.Info.Sandbox] != "" {
			return nil
		}
		if r.Parked() {
			return m.resumeCmd(r, openshell.RestoreAuto)
		}
		return m.parkCmd(r)
	case key.Matches(msg, m.keys.Copy):
		m.copied = r.Info.Sandbox
		return tea.SetClipboard(m.src.AttachCommand(r.Info.Repo, r.Info.Name))
	}
	return nil
}

func (m *Sessions) onSignalsKey(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, m.keys.Attention):
		m.attention = !m.attention
		m.sigCursor, m.unseen = 0, 0
		return nil
	case key.Matches(msg, m.keys.Attach):
		vis := m.visibleSignals()
		if m.sigCursor >= len(vis) {
			return nil
		}
		return m.launch(vis[m.sigCursor])
	}
	return nil
}

// launch opens a signal's session: attach (resuming it if parked), or,
// when it has ended, the new-session form for it.
func (m *Sessions) launch(s signals.Signal) tea.Cmd {
	var sess events.Session
	switch {
	case s.Status != nil:
		sess = s.Status.Session
	case s.Lifecycle != nil:
		sess = s.Lifecycle.Session
	default:
		return nil
	}
	if i := m.rowIndex(sess.Sandbox); i >= 0 {
		r := m.rows[i]
		return m.attachCmd(r.Info.Repo, r.Info.Name, r.Info.Sandbox, r.Parked())
	}
	if m.listErr != nil || !m.loaded {
		m.notice = notice{text: "Cannot tell whether " + sessionLabel(sess.Repo, sess.Name) + " is running: the session list is not loaded.", err: true}
		return nil
	}
	return m.reposCmd(prefill{repo: sess.Repo, profile: m.profiles[sess.Sandbox]})
}

// attachCmd prepares the session (resuming it when parked) and then
// attaches to it.
func (m *Sessions) attachCmd(repo, name, key string, parked bool) tea.Cmd {
	if m.handoff == nil {
		m.notice = notice{text: errNoTerminal.Error(), err: true}
		return nil
	}
	title := "Attaching to " + sessionLabel(repo, name) + " …  detach with ~."
	op, ctx := newOperation(m.ctx, title, repo, name, true)
	op.key = key
	m.op, m.opLines = op, nil
	if parked {
		m.opLines = []string{"Resuming…"}
	}
	return op.run(ctx, m.src, sessionctl.Request{Repo: repo, Name: name, Term: m.term()})
}

// StartNew starts a new session of repo, as the new-session form does with
// its defaults: from profile, else the repository's default profile, named
// after it; then attaches to it when attach is set and the TUI can hand the
// terminal over. The Repositories tab's new-repository form uses it.
func (m *Sessions) StartNew(repo, profile string, attach bool) tea.Cmd {
	if m.src == nil {
		return nil
	}
	m.notice = notice{}
	if profile == "" {
		profile = m.src.RepoProfile(repo)
	}
	return m.start(repo, m.defaultName(repo, profile), profile, attach)
}

// startCmd starts the session the new-session form describes.
func (m *Sessions) startCmd(f *prompt.Form) tea.Cmd {
	repo, name, profile := f.Field("repo").Value(), f.Field("name").Value(), f.Field("profile").Value()
	return m.start(repo, name, profile, f.Field("attach").On())
}

// start creates the named session and attaches to it when attach is set.
func (m *Sessions) start(repo, name, profile string, attach bool) tea.Cmd {
	if attach && m.handoff == nil {
		attach = false
		m.notice = notice{text: errNoTerminal.Error(), err: true}
	}
	title := "Starting " + sessionLabel(repo, name) + " …"
	op, ctx := newOperation(m.ctx, title, repo, name, attach)
	m.op, m.opLines = op, nil
	return op.run(ctx, m.src, sessionctl.Request{Repo: repo, Name: name, Profile: profile, Create: true, Term: m.term()})
}

// term is the TERM given to the attach client.
func (m *Sessions) term() string {
	if m.handoff == nil {
		return ""
	}
	if s, ok := m.common.Context().Value(TermKey).(string); ok {
		return s
	}
	return ""
}

// TermKey is the key of the client's TERM in the context of common.Common.
var TermKey = &struct{ string }{"sessions-term"}

func (m *Sessions) onOp(op *operation) tea.Cmd {
	if op != m.op {
		return nil
	}
	lines, done, att, err := op.state()
	if len(m.opLines) > 0 && m.opLines[0] == "Resuming…" {
		lines = append([]string{"Resuming…"}, lines...)
	}
	m.opLines = lines
	if !done {
		return op.waitCmd(m.ctx)
	}
	m.op = nil
	label := sessionLabel(op.repo, op.name)
	if err != nil {
		op.cancel()
		var conflict *openshell.RestoreConflictError
		if errors.As(err, &conflict) {
			// Nothing was started: ask, and start it again with the
			// answer.
			m.notice = conflictNotice(label, conflict)
			m.ask(restoreQuestion(conflict), func() tea.Cmd { return m.rerun(op, openshell.RestoreOver) })
			return nil
		}
		m.notice = notice{text: "✗ " + label + ": " + err.Error(), err: true}
		return m.listCmd()
	}
	if !op.attach || m.handoff == nil {
		op.cancel()
		att.Close()
		if m.notice.text == "" {
			m.notice = notice{text: "Started " + label + "."}
		}
		return m.listCmd()
	}
	op.cancel()
	if s := att.Session(); s != nil {
		m.selKey = s.Sandbox
	}
	return attachCmd(m.ctx, m.handoff, att, op.repo, op.name, op.title)
}

func (m *Sessions) onAttachDone(msg attachDoneMsg) tea.Cmd {
	label := sessionLabel(msg.repo, msg.name)
	switch {
	case msg.err != nil:
		m.notice = notice{text: "✗ " + label + ": " + msg.err.Error(), err: true}
	case msg.code > 0:
		m.notice = notice{text: fmt.Sprintf("%s: the program exited with status %d.", label, msg.code)}
	default:
		m.notice = notice{text: "Detached from " + label + "."}
	}
	return m.listCmd()
}

// ask opens the confirm with question; yes runs onYes.
func (m *Sessions) ask(question string, onYes func() tea.Cmd) {
	m.confirm = prompt.NewConfirm(m.common, question)
	m.onYes = onYes
}

// restoreQuestion is the one-line question for a restore conflict; the
// error has the long form.
func restoreQuestion(e *openshell.RestoreConflictError) string {
	return fmt.Sprintf("Restore its snapshot (%s) over files already in the new sandbox?", e.Snapshot.Describe())
}

// conflictNotice says what the new sandbox holds and what no means.
func conflictNotice(label string, e *openshell.RestoreConflictError) notice {
	return notice{text: label + ": the new sandbox already holds " + strings.Join(e.Paths, ", ") +
		". y restores the snapshot over them (files only in the sandbox are kept); n starts nothing (session start --fresh keeps them)."}
}

// rerun starts op's preparation again, restoring as restore says.
func (m *Sessions) rerun(op *operation, restore openshell.RestoreMode) tea.Cmd {
	again, ctx := newOperation(m.ctx, op.title, op.repo, op.name, op.attach)
	again.key = op.key
	m.op, m.opLines = again, nil
	m.notice = notice{}
	req := op.req
	req.Restore = restore
	return again.run(ctx, m.src, req)
}

func (m *Sessions) endCmd(r sessionctl.Row, force bool) tea.Cmd {
	m.busy[r.Info.Sandbox] = "ending"
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		existed, err := src.End(ctx, r.Info.Repo, r.Info.Name, force)
		return actionMsg{key: r.Info.Sandbox, verb: "end", repo: r.Info.Repo, name: r.Info.Name, done: existed, err: err, row: r}
	}
}

func (m *Sessions) parkCmd(r sessionctl.Row) tea.Cmd {
	m.busy[r.Info.Sandbox] = "parking"
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		parked, err := src.Park(ctx, r.Info.Repo, r.Info.Name)
		return actionMsg{key: r.Info.Sandbox, verb: "park", repo: r.Info.Repo, name: r.Info.Name, done: parked, err: err}
	}
}

func (m *Sessions) resumeCmd(r sessionctl.Row, restore openshell.RestoreMode) tea.Cmd {
	m.busy[r.Info.Sandbox] = "resuming"
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		att, err := src.Prepare(ctx, sessionctl.Request{Repo: r.Info.Repo, Name: r.Info.Name, Restore: restore})
		if att != nil {
			att.Close()
		}
		return actionMsg{key: r.Info.Sandbox, verb: "resume", repo: r.Info.Repo, name: r.Info.Name, done: err == nil, err: err, row: r}
	}
}

func (m *Sessions) onAction(msg actionMsg) tea.Cmd {
	delete(m.busy, msg.key)
	label := sessionLabel(msg.repo, msg.name)
	var conflict *openshell.RestoreConflictError
	switch {
	case msg.verb == "end" && errors.Is(msg.err, openshell.ErrSnapshotFailed):
		// Not ended: say why, and offer to end it without a snapshot.
		m.notice = notice{text: "✗ " + label + ": " + msg.err.Error(), err: true}
		r := msg.row
		m.ask("The snapshot failed. End "+label+" without one?  Unpushed work is lost.", func() tea.Cmd { return m.endCmd(r, true) })
		return m.listCmd()
	case msg.verb == "resume" && errors.As(msg.err, &conflict):
		m.notice = conflictNotice(label, conflict)
		r := msg.row
		m.ask(restoreQuestion(conflict), func() tea.Cmd { return m.resumeCmd(r, openshell.RestoreOver) })
		return m.listCmd()
	case msg.err != nil:
		m.notice = notice{text: "✗ " + label + ": " + msg.err.Error(), err: true}
	case msg.verb == "end" && msg.done:
		m.notice = notice{text: "Ended " + label + "."}
	case msg.verb == "end":
		m.notice = notice{text: label + " had already ended."}
	case msg.verb == "park" && msg.done:
		m.notice = notice{text: "Parked " + label + "."}
	case msg.verb == "park":
		m.notice = notice{text: label + " is already parked."}
	case msg.verb == "resume":
		m.notice = notice{text: "Resumed " + label + "."}
	}
	return m.listCmd()
}

// newSessionForm returns the new-session form (section 5a).
func (m *Sessions) newSessionForm(repos []string, pre prefill) *prompt.Form {
	repo := pre.repo
	if repo == "" && len(repos) > 0 {
		repo = repos[0]
		if r, ok := m.selected(); ok && slices.Contains(repos, r.Info.Repo) {
			repo = r.Info.Repo
		}
	}
	var repoField *prompt.Field
	if pre.fixed {
		repoField = prompt.FixedField("repo", "Repository", repo)
	} else {
		opts := make([]prompt.Option, len(repos))
		for i, r := range repos {
			opts[i] = prompt.Option{Value: r}
		}
		repoField = prompt.ChoiceField("repo", "Repository", opts, repo, true)
	}

	profile := pre.profile
	if profile == "" {
		profile = m.src.RepoProfile(repo)
	}
	var popts []prompt.Option
	for _, p := range m.src.Profiles() {
		popts = append(popts, prompt.Option{Value: p.Name, Detail: profileDetail(p.Image, p.Command)})
	}
	if len(popts) == 0 {
		popts = []prompt.Option{{Value: "", Label: "default", Detail: m.src.Command("")}}
	}
	profileField := prompt.ChoiceField("profile", "Profile", popts, profile, false)
	nameField := prompt.TextField("name", "Name", m.defaultName(repo, profileField.Value()), func(name string) error {
		if !openshell.ValidSessionName(name) {
			return openshell.ErrInvalidSessionName
		}
		return nil
	})
	f := prompt.NewForm(m.common, "New session", "create",
		repoField, nameField, profileField, prompt.ToggleField("attach", "Attach", true))
	profileTouched := pre.profile != ""
	f.OnChange = func(f *prompt.Form, key string) {
		repo := f.Field("repo").Value()
		switch key {
		case "repo":
			if !profileTouched {
				f.Field("profile").SetValue(m.src.RepoProfile(repo))
			}
		case "profile":
			profileTouched = true
		}
		if name := f.Field("name"); !name.Edited() {
			name.SetValue(m.defaultName(repo, f.Field("profile").Value()))
		}
	}
	f.Validate = func(f *prompt.Form) error {
		repo, name := f.Field("repo").Value(), f.Field("name").Value()
		if m.exists(repo, name) {
			return fmt.Errorf("%s already exists; pick another name, or attach to it from the list", sessionLabel(repo, name))
		}
		return nil
	}
	return f
}

func profileDetail(image, command string) string {
	var parts []string
	for _, s := range []string{image, command} {
		if s = signals.Sanitize(s, 40); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " · ")
}

// exists reports whether the user has the named session of repo.
func (m *Sessions) exists(repo, name string) bool {
	return slices.ContainsFunc(m.rows, func(r sessionctl.Row) bool {
		return r.Info.Repo == repo && r.Info.Name == name
	})
}

// defaultName is a new session's name: the profile's name and the first
// number not taken in the repository ("main" when there is no profile and
// the repository has no main session).
func (m *Sessions) defaultName(repo, profile string) string {
	base := sessionNameBase(profile)
	if profile == "" && !m.exists(repo, openshell.DefaultSessionName) {
		return openshell.DefaultSessionName
	}
	for n := 1; ; n++ {
		name := fmt.Sprintf("%s-%d", base, n)
		if !m.exists(repo, name) {
			return name
		}
	}
}

// sessionNameBase makes a profile name usable in a session name.
func sessionNameBase(profile string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(profile) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 24 {
		s = strings.TrimRight(s[:24], "-")
	}
	if s == "" {
		return "session"
	}
	return s
}

// Summary renders the attention summary of the header: how many of the
// user's sessions are blocked, errored, done and working, zero counts left
// out; "" when there are none. Narrow drops the words.
func (m *Sessions) Summary(narrow bool) string {
	return m.summary(narrow, nil)
}

// StatusSummary renders the attention summary for the repository page's
// status bar (R7): narrow, on the bar's background.
func (m *Sessions) StatusSummary() string {
	bg := m.common.Styles.StatusBarSummary.GetBackground()
	return m.summary(true, bg)
}

func (m *Sessions) summary(narrow bool, bg color.Color) string {
	if m.src == nil || m.listErr != nil {
		return ""
	}
	counts := map[events.State]int{}
	for _, r := range m.rows {
		counts[r.State()]++
	}
	st := m.common.Styles
	sep := "  "
	if narrow {
		sep = " "
	}
	var parts []string
	for _, s := range []events.State{events.StateBlocked, events.StateError, events.StateDone, events.StateWorking} {
		n := counts[s]
		if n == 0 {
			continue
		}
		text := fmt.Sprintf("%s %d %s", s.Glyph(), n, s.Word())
		if narrow {
			text = fmt.Sprintf("%s%d", s.Glyph(), n)
		}
		style := stateStyle(st, s)
		if bg != nil {
			style = style.Background(bg)
		}
		parts = append(parts, style.Render(text))
	}
	if bg != nil {
		return strings.Join(parts, lipgloss.NewStyle().Background(bg).Render(sep))
	}
	return strings.Join(parts, sep)
}

// RepoBadge is the session badge of a repository (section 4): the most
// urgent state among the user's sessions of it, and how many there are.
type RepoBadge struct {
	State events.State
	Count int
}

// RepoBadges returns the session badge of each repository the user has
// sessions of, from what the Monitor last read. It never calls the
// gateway.
func (m *Sessions) RepoBadges() map[string]RepoBadge {
	if m.src == nil {
		return nil
	}
	out := map[string]RepoBadge{}
	for _, st := range m.src.Statuses() {
		repo := st.Session.Repo
		b, ok := out[repo]
		s := st.Record.State
		if !ok || s.Rank() < b.State.Rank() {
			b.State = s
		}
		b.Count++
		out[repo] = b
	}
	return out
}

// StateStyle returns the colour of a state (section 2).
func StateStyle(st *styles.Styles, s events.State) lipgloss.Style { return stateStyle(st, s) }
