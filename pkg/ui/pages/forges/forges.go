// Package forges is the home page's Forges tab: the configured forges
// (GitHub, GitLab) as one flat list grouped by forge, with each forge's
// repositories, imported or not. It imports, syncs and pushes them as the
// `forge` commands do, in the background, and follows the forge events on
// the bus for live updates. See
// _bmad-output/planning-artifacts/tui-screens.md, section 6.
package forges

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/forge"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/prompt"
)

// NarrowWidth is the content width under which the tab uses its narrow
// layout (section 9); the same as the Sessions tab's.
const NarrowWidth = 56

// KeyMap is the Forges tab's keys.
type KeyMap struct {
	Open      key.Binding
	Import    key.Binding
	ImportAll key.Binding
	Sync      key.Binding
	Push      key.Binding
	Copy      key.Binding
	Filter    key.Binding
	Clear     key.Binding
	Top       key.Binding
	Bottom    key.Binding
	Apply     key.Binding
	Cancel    key.Binding
}

// DefaultKeyMap returns the tab's keys. None clashes with the frame's:
// q, ?, tab, 1-9 and esc (tui-screens.md section 10).
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Open:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
		Import:    key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "import")),
		ImportAll: key.NewBinding(key.WithKeys("I"), key.WithHelp("I", "import all shown")),
		Sync:      key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "sync")),
		Push:      key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "push")),
		Copy:      key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy clone command")),
		Filter:    key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Clear:     key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear filter")),
		Top:       key.NewBinding(key.WithKeys("g", "home"), key.WithHelp("g", "top")),
		Bottom:    key.NewBinding(key.WithKeys("G", "end"), key.WithHelp("G", "bottom")),
		Apply:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "apply filter")),
		Cancel:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
	}
}

// OpenRepoMsg asks the frame to open an imported repository.
type OpenRepoMsg struct{ Repo string }

// ReposChangedMsg tells the home page that an operation of the tab added
// repositories, so that it lists them again.
type ReposChangedMsg struct{}

// Messages.
type (
	// loadMsg is a read of the forges' status and of some forges'
	// repositories.
	loadMsg struct {
		status    []forge.ForgeStatus
		statusErr error
		repos     map[string][]forge.RepoStatus
		errs      map[string]error
	}
	discoverMsg struct {
		forge string
		err   error
	}
	feedMsg    struct{ evs []events.Event }
	feedEndMsg struct{}
	// opMsg is the end of an operation the tab started.
	opMsg struct {
		// keys are the rows and headers the operation marked busy.
		keys []string
		// forge is the forge whose data to read again.
		forge string
		// rows are the outcomes per row.
		rows map[string]result
		// notice is the line shown at the bottom.
		notice notice
		// reposChanged reports whether the operation added repositories.
		reposChanged bool
	}
	branchMsg struct {
		forge, path, repo, branch string
		err                       error
	}
)

// IsMsg reports whether msg is one of the tab's own messages (live
// updates, results of what it started). The frame delivers them to the
// tab while another page is shown.
func IsMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case loadMsg, discoverMsg, feedMsg, feedEndMsg, opMsg, branchMsg:
		return true
	}
	return false
}

// notice is a one-line message shown at the bottom of the tab.
type notice struct {
	text string
	err  bool
}

// result is the outcome of an operation on a row: a few words shown in
// the row, and the full text shown when the row is selected.
type result struct {
	short  string
	detail string
	err    bool
}

// group is a forge and its repositories.
type group struct {
	status forge.ForgeStatus
	repos  []forge.RepoStatus
	loaded bool
	err    error
}

type rowKind int

const (
	headerRow rowKind = iota
	repoRow
)

// item is a row of the list: a forge's header, or one of its
// repositories.
type item struct {
	kind rowKind
	g, r int
}

// progress counts the repositories an operation on a whole forge has done.
type progress struct {
	verb        string
	done, total int
}

// Forges is the Forges tab.
type Forges struct {
	common common.Common
	src    Source
	keys   KeyMap
	now    func() time.Time

	ctx     context.Context
	cancel  context.CancelFunc
	started bool
	feed    Feed

	groups    []group
	loaded    bool
	statusErr error
	// loading reports that a read is under way; dirty, the forges to
	// read again after it ("" for every forge).
	loading bool
	dirty   map[string]bool

	// listing marks forges being listed (discovered).
	listing map[string]bool
	// busy maps a row or header key to what the tab is doing to it.
	busy map[string]string
	// progress is the progress of forge-wide operations, by forge.
	progress map[string]*progress
	results  map[string]result

	cursor int
	selKey string
	offset int
	filter textinput.Model
	typing bool
	query  string

	confirm *prompt.Confirm
	onYes   func() tea.Cmd

	// importForm asks how to import (jj or git) while it is open;
	// onImport imports with the VCS chosen.
	importForm *prompt.Form
	onImport   func(vcs proto.VCS) tea.Cmd

	notice notice
}

// New returns the Forges tab showing src. src may be nil: the tab then
// shows nothing (the frame hides it).
func New(c common.Common, src Source) *Forges {
	ctx, cancel := context.WithCancel(c.Context())
	in := textinput.New()
	in.Prompt = "/ "
	in.CharLimit = 64
	st := in.Styles()
	st.Cursor.Blink = false
	in.SetStyles(st)
	return &Forges{
		common:   c,
		src:      src,
		keys:     DefaultKeyMap(),
		now:      time.Now,
		ctx:      ctx,
		cancel:   cancel,
		filter:   in,
		dirty:    map[string]bool{},
		listing:  map[string]bool{},
		busy:     map[string]string{},
		progress: map[string]*progress{},
		results:  map[string]result{},
	}
}

// Close stops the tab's live updates and cancels what it was doing.
func (m *Forges) Close() {
	m.cancel()
	if m.feed != nil {
		m.feed.Close()
	}
}

// Capturing reports whether the tab takes every key: a confirm or the
// filter is open.
func (m *Forges) Capturing() bool { return m.confirm != nil || m.importForm != nil || m.typing }

// SetSize implements common.Component.
func (m *Forges) SetSize(width, height int) { m.common.SetSize(width, height) }

// Init implements tea.Model. The tab starts when it is first shown
// (Start), so opening the TUI does not list the forges.
func (m *Forges) Init() tea.Cmd { return nil }

// Start reads the forges, lists those never listed, and follows the
// forge events. Only the first call does anything.
func (m *Forges) Start() tea.Cmd {
	if m.src == nil || m.started {
		return nil
	}
	m.started = true
	m.feed = m.src.Follow()
	return tea.Batch(m.loadCmd(""), m.nextCmd())
}

func rowKey(forgeName, path string) string {
	return "r\x00" + forgeName + "\x00" + strings.ToLower(path)
}
func headerKey(forgeName string) string { return "f\x00" + forgeName }

// loadCmd reads the forges' status and the repositories of forgeName (""
// for every forge), unless a read is under way: then it reads them when
// that one is done.
func (m *Forges) loadCmd(forgeName string) tea.Cmd {
	if m.loading {
		m.dirty[forgeName] = true
		return nil
	}
	m.loading = true
	var names []string
	if forgeName != "" {
		names = []string{forgeName}
	}
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		msg := loadMsg{repos: map[string][]forge.RepoStatus{}, errs: map[string]error{}}
		msg.status, msg.statusErr = src.Status(ctx)
		if msg.statusErr == nil && names == nil {
			for _, st := range msg.status {
				names = append(names, st.Name)
			}
		}
		for _, n := range names {
			msg.repos[n], msg.errs[n] = src.Repos(ctx, n)
		}
		if ctx.Err() != nil {
			return nil
		}
		return msg
	}
}

func (m *Forges) discoverCmd(forgeName string) tea.Cmd {
	m.listing[forgeName] = true
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		err := src.Discover(ctx, forgeName)
		if ctx.Err() != nil {
			return nil
		}
		return discoverMsg{forge: forgeName, err: err}
	}
}

// nextCmd waits for the next forge events.
func (m *Forges) nextCmd() tea.Cmd {
	feed, ctx := m.feed, m.ctx
	if feed == nil {
		return nil
	}
	return func() tea.Msg {
		evs, err := feed.Next(ctx)
		if err != nil {
			return feedEndMsg{}
		}
		return feedMsg{evs: evs}
	}
}

// Update implements tea.Model.
func (m *Forges) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case loadMsg:
		return m, m.onLoad(msg)
	case discoverMsg:
		delete(m.listing, msg.forge)
		return m, m.loadCmd(msg.forge)
	case feedMsg:
		return m, tea.Batch(m.onEvents(msg.evs), m.nextCmd())
	case feedEndMsg:
		m.feed = nil
		return m, nil
	case opMsg:
		return m, m.onOp(msg)
	case branchMsg:
		return m, m.onBranch(msg)
	case tea.KeyPressMsg:
		return m, m.onKey(msg)
	}
	return m, nil
}

func (m *Forges) onLoad(msg loadMsg) tea.Cmd {
	m.loading = false
	var cmds []tea.Cmd
	if msg.statusErr != nil {
		m.statusErr = msg.statusErr
		m.common.Logger.Debug("forges: cannot read the forges", "err", msg.statusErr)
	} else {
		first := !m.loaded
		m.statusErr = nil
		m.loaded = true
		m.setStatus(msg.status)
		for i := range m.groups {
			name := m.groups[i].status.Name
			if repos, ok := msg.repos[name]; ok {
				m.groups[i].repos, m.groups[i].err, m.groups[i].loaded = repos, msg.errs[name], true
			}
			// A forge never listed is listed when the tab opens.
			if first && m.groups[i].status.Listed == nil && !m.listing[name] {
				cmds = append(cmds, m.discoverCmd(name))
			}
		}
		m.restoreCursor()
	}
	if len(m.dirty) > 0 {
		name := ""
		if !m.dirty[""] && len(m.dirty) == 1 {
			for n := range m.dirty {
				name = n
			}
		}
		clear(m.dirty)
		cmds = append(cmds, m.loadCmd(name))
	}
	return tea.Batch(cmds...)
}

// setStatus replaces the forges' status, keeping their repositories.
func (m *Forges) setStatus(status []forge.ForgeStatus) {
	old := map[string]group{}
	for _, g := range m.groups {
		old[g.status.Name] = g
	}
	m.groups = m.groups[:0]
	for _, st := range status {
		g := old[st.Name]
		g.status = st
		m.groups = append(m.groups, g)
	}
}

// onEvents follows forge events: what changed is read again, and the
// progress of the tab's forge-wide operations counted.
func (m *Forges) onEvents(evs []events.Event) tea.Cmd {
	touched := map[string]bool{}
	for _, ev := range evs {
		o := ev.Forge
		if o == nil {
			continue
		}
		touched[o.Forge] = true
		rk := rowKey(o.Forge, o.Path)
		p := m.progress[o.Forge]
		switch forge.EventKind(o.Kind) {
		case forge.EventSync:
			if p != nil && p.verb == "syncing" {
				p.done++
				m.results[rk] = outcome("synced", "sync failed", o)
			}
		case forge.EventImport:
			if p != nil && p.verb == "importing" && m.busy[rk] != "" {
				p.done++
				delete(m.busy, rk)
				m.results[rk] = outcome("imported", "import failed", o)
			}
		case forge.EventPush, forge.EventDiscover:
		}
	}
	var cmds []tea.Cmd
	for name := range touched {
		cmds = append(cmds, m.loadCmd(name))
	}
	return tea.Batch(cmds...)
}

func outcome(ok, failed string, o *events.ForgeOutcome) result {
	if o.OK {
		return result{short: "✓ " + ok}
	}
	return result{short: "✗ " + failed, detail: o.Path + ": " + failed + ": " + o.Error, err: true}
}

// items returns the rows the filter selects: each forge's header, then
// its repositories.
func (m *Forges) items() []item {
	q := strings.ToLower(m.query)
	var out []item
	for gi, g := range m.groups {
		forgeHit := q == "" || strings.Contains(strings.ToLower(g.status.Name), q)
		var rows []item
		for ri, r := range g.repos {
			if forgeHit || strings.Contains(strings.ToLower(r.Path), q) || strings.Contains(strings.ToLower(r.Repo), q) {
				rows = append(rows, item{kind: repoRow, g: gi, r: ri})
			}
		}
		if !forgeHit && len(rows) == 0 {
			continue
		}
		out = append(out, item{kind: headerRow, g: gi, r: -1})
		out = append(out, rows...)
	}
	return out
}

func (m *Forges) key(it item) string {
	g := m.groups[it.g]
	if it.kind == headerRow {
		return headerKey(g.status.Name)
	}
	return rowKey(g.status.Name, g.repos[it.r].Path)
}

func (m *Forges) selected() (item, bool) {
	its := m.items()
	if m.cursor < 0 || m.cursor >= len(its) {
		return item{}, false
	}
	return its[m.cursor], true
}

// restoreCursor keeps the cursor on the selected row after the rows
// changed.
func (m *Forges) restoreCursor() {
	its := m.items()
	m.cursor = clamp(m.cursor, 0, len(its)-1)
	for i, it := range its {
		if m.key(it) == m.selKey {
			m.cursor = i
		}
	}
	m.syncSel()
}

func (m *Forges) syncSel() {
	if it, ok := m.selected(); ok {
		m.selKey = m.key(it)
	}
}

func (m *Forges) move(d int) {
	m.cursor = clamp(m.cursor+d, 0, len(m.items())-1)
	m.syncSel()
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return min(max(v, lo), hi)
}

func (m *Forges) onKey(msg tea.KeyPressMsg) tea.Cmd {
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
	case m.importForm != nil:
		res, cmd := m.importForm.Update(msg)
		switch res {
		case prompt.Submitted:
			vcs, _ := proto.ParseVCS(m.importForm.Field("vcs").Value())
			do := m.onImport
			m.importForm, m.onImport = nil, nil
			return do(vcs)
		case prompt.Cancelled:
			m.importForm, m.onImport = nil, nil
		}
		return cmd
	case m.typing:
		return m.onFilterKey(msg)
	}
	m.notice = notice{}
	if m.src == nil {
		return nil
	}
	km := m.common.KeyMap
	switch {
	case key.Matches(msg, m.keys.Filter):
		m.typing = true
		m.filter.SetValue(m.query)
		m.filter.CursorEnd()
		return m.filter.Focus()
	case key.Matches(msg, m.keys.Clear):
		m.setQuery("")
		return nil
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
	case key.Matches(msg, m.keys.Top):
		m.move(-1 << 30)
		return nil
	case key.Matches(msg, m.keys.Bottom):
		m.move(1 << 30)
		return nil
	case key.Matches(msg, m.keys.ImportAll):
		return m.importAll()
	}
	if m.statusErr != nil || !m.loaded {
		if key.Matches(msg, m.keys.Sync) {
			return m.loadCmd("")
		}
		return nil
	}
	it, ok := m.selected()
	if !ok {
		return nil
	}
	g := m.groups[it.g]
	name := g.status.Name
	if it.kind == headerRow {
		switch {
		case key.Matches(msg, m.keys.Sync):
			return m.syncForge(name)
		case key.Matches(msg, m.keys.Import, m.keys.Push, m.keys.Open, m.keys.Copy):
			m.notice = notice{text: "Select a repository of " + name + " (I imports all shown, r syncs the forge)."}
		}
		return nil
	}
	r := g.repos[it.r]
	rk := rowKey(name, r.Path)
	switch {
	case key.Matches(msg, m.keys.Open):
		if !r.Imported {
			m.notice = notice{text: r.Path + " is not imported; press i to import it."}
			return nil
		}
		repo := r.Repo
		return func() tea.Msg { return OpenRepoMsg{Repo: repo} }
	case key.Matches(msg, m.keys.Copy):
		if !r.Imported {
			m.notice = notice{text: r.Path + " is not imported; press i to import it."}
			return nil
		}
		cmd := m.src.CloneCommand(r.Repo)
		m.notice = notice{text: "Copied to the clipboard: " + cmd}
		return tea.SetClipboard(cmd)
	case key.Matches(msg, m.keys.Import):
		if r.Imported {
			m.notice = notice{text: r.Path + " is already imported as " + r.Repo + "."}
			return nil
		}
		if m.busy[rk] != "" {
			return nil
		}
		m.askImport("Import "+r.Path+" from "+name, func(vcs proto.VCS) tea.Cmd {
			return m.importCmd(name, []forge.RepoStatus{r}, vcs)
		})
		return nil
	case key.Matches(msg, m.keys.Sync):
		if !r.Imported {
			return m.syncForge(name)
		}
		return m.syncRepo(name, r)
	case key.Matches(msg, m.keys.Push):
		if !r.Imported {
			m.notice = notice{text: r.Path + " is not imported; there is nothing to push."}
			return nil
		}
		if m.busy[rk] != "" {
			return nil
		}
		return m.branchCmd(name, r)
	}
	return nil
}

func (m *Forges) setQuery(q string) {
	m.query = q
	m.cursor, m.offset = 0, 0
	m.restoreCursor()
}

func (m *Forges) onFilterKey(msg tea.KeyPressMsg) tea.Cmd {
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

// importAll asks to import every repository shown that is not imported:
// all forges, narrowed by the filter.
func (m *Forges) importAll() tea.Cmd {
	byForge := map[string][]forge.RepoStatus{}
	var order []string
	n := 0
	for _, it := range m.items() {
		if it.kind != repoRow {
			continue
		}
		g := m.groups[it.g]
		r := g.repos[it.r]
		if r.Imported || r.Excluded || m.busy[rowKey(g.status.Name, r.Path)] != "" {
			continue
		}
		if _, ok := byForge[g.status.Name]; !ok {
			order = append(order, g.status.Name)
		}
		byForge[g.status.Name] = append(byForge[g.status.Name], r)
		n++
	}
	if n == 0 {
		m.notice = notice{text: "Nothing to import: every repository shown is imported."}
		return nil
	}
	q := fmt.Sprintf("Import %s", plural(n, "repository", "repositories"))
	if m.query != "" {
		q += " matching “" + m.query + "”"
	}
	if len(order) > 1 {
		var parts []string
		for _, f := range order {
			parts = append(parts, fmt.Sprintf("%s %d", f, len(byForge[f])))
		}
		q += " (" + strings.Join(parts, ", ") + ")"
	} else {
		q += " from " + order[0]
	}
	m.askImport(q, func(vcs proto.VCS) tea.Cmd {
		var cmds []tea.Cmd
		for _, f := range order {
			cmds = append(cmds, m.importCmd(f, byForge[f], vcs))
		}
		return tea.Batch(cmds...)
	})
	return nil
}

// askImport opens the import form: what is imported, and as jj or git.
func (m *Forges) askImport(what string, do func(vcs proto.VCS) tea.Cmd) {
	vcs := prompt.ChoiceField("vcs", "VCS", []prompt.Option{
		{Value: string(proto.VCSJJ), Detail: "jj git clone; bookmarks and change IDs"},
		{Value: string(proto.VCSGit), Detail: "git clone; branches and commits"},
	}, string(proto.DefaultVCS), false)
	m.importForm = prompt.NewForm(m.common, what, "import", vcs)
	m.onImport = do
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// importCmd imports repositories of a forge in the background, as vcs
// repositories.
func (m *Forges) importCmd(forgeName string, repos []forge.RepoStatus, vcs proto.VCS) tea.Cmd {
	var keys, paths []string
	for _, r := range repos {
		rk := rowKey(forgeName, r.Path)
		m.busy[rk] = "importing…"
		delete(m.results, rk)
		keys = append(keys, rk)
		paths = append(paths, r.Path)
	}
	if len(repos) > 1 {
		hk := headerKey(forgeName)
		m.busy[hk] = "importing"
		m.progress[forgeName] = &progress{verb: "importing", total: len(repos)}
		keys = append(keys, hk)
	}
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		results, err := src.Import(ctx, forgeName, paths, vcs)
		if ctx.Err() != nil {
			return nil
		}
		out := opMsg{keys: keys, forge: forgeName, rows: map[string]result{}}
		if err != nil {
			for _, k := range keys {
				out.rows[k] = result{short: "✗ import failed", detail: err.Error(), err: true}
			}
			out.notice = notice{text: "✗ Import from " + forgeName + " failed: " + err.Error(), err: true}
			return out
		}
		failed := 0
		for _, r := range results {
			rk := rowKey(forgeName, r.Path)
			switch r.Status {
			case forge.ImportImported:
				out.reposChanged = true
				out.rows[rk] = result{short: "✓ imported", detail: r.Path + " imported as " + r.Repo + " (" + r.Sync.Summary() + ")"}
			case forge.ImportExists:
				out.rows[rk] = result{short: "✓ imported", detail: r.Path + " is already imported as " + r.Repo}
			default:
				failed++
				out.rows[rk] = result{short: "✗ import failed", detail: r.Path + ": " + r.Error, err: true}
			}
		}
		switch {
		case len(results) == 1 && failed == 1:
			out.notice = notice{text: "✗ Cannot import " + results[0].Path + ": " + results[0].Error, err: true}
		case len(results) == 1:
			out.notice = notice{text: "Imported " + results[0].Path + " as " + results[0].Repo + "."}
		case failed > 0:
			out.notice = notice{text: fmt.Sprintf("✗ Imported %d of %d from %s; %d failed (select a row for why).", len(results)-failed, len(results), forgeName, failed), err: true}
		default:
			out.notice = notice{text: fmt.Sprintf("Imported %s from %s.", plural(len(results), "repository", "repositories"), forgeName)}
		}
		return out
	}
}

// syncForge syncs every repository imported from a forge in the
// background.
func (m *Forges) syncForge(forgeName string) tea.Cmd {
	hk := headerKey(forgeName)
	if m.busy[hk] != "" {
		return nil
	}
	total := 0
	for _, g := range m.groups {
		if g.status.Name == forgeName {
			total = g.status.Imported
		}
	}
	m.busy[hk] = "syncing"
	m.progress[forgeName] = &progress{verb: "syncing", total: total}
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		results, err := src.SyncForge(ctx, forgeName)
		if ctx.Err() != nil {
			return nil
		}
		out := opMsg{keys: []string{hk}, forge: forgeName, rows: map[string]result{}}
		if err != nil {
			text := err.Error()
			if errors.Is(err, forge.ErrSyncRunning) {
				text = "a sync of " + forgeName + " is already running"
			}
			out.notice = notice{text: "✗ " + text, err: true}
			return out
		}
		failed := 0
		for _, r := range results {
			rk := rowKey(forgeName, r.Path)
			if r.Error != "" {
				failed++
				out.rows[rk] = result{short: "✗ sync failed", detail: r.Path + ": sync failed: " + r.Error, err: true}
				continue
			}
			out.rows[rk] = result{short: "✓ synced", detail: r.Path + ": " + r.Sync.Summary()}
		}
		switch {
		case len(results) == 0:
			out.notice = notice{text: "Listed " + forgeName + "; no imported repositories to sync."}
		case failed > 0:
			out.notice = notice{text: fmt.Sprintf("✗ Synced %s: %d of %d failed (select a row for why).", forgeName, failed, len(results)), err: true}
		default:
			out.notice = notice{text: fmt.Sprintf("Synced %s: %s.", forgeName, plural(len(results), "repository", "repositories"))}
		}
		return out
	}
}

// syncRepo syncs one imported repository in the background.
func (m *Forges) syncRepo(forgeName string, r forge.RepoStatus) tea.Cmd {
	rk := rowKey(forgeName, r.Path)
	if m.busy[rk] != "" {
		return nil
	}
	m.busy[rk] = "syncing…"
	delete(m.results, rk)
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		res, err := src.SyncRepo(ctx, forgeName, r.Repo)
		if ctx.Err() != nil {
			return nil
		}
		out := opMsg{keys: []string{rk}, forge: forgeName, rows: map[string]result{}}
		if err == nil && res.Error != "" {
			err = errors.New(res.Error)
		}
		if err != nil {
			out.rows[rk] = result{short: "✗ sync failed", detail: r.Path + ": sync failed: " + err.Error(), err: true}
			out.notice = notice{text: "✗ " + out.rows[rk].detail, err: true}
			return out
		}
		out.rows[rk] = result{short: "✓ synced", detail: r.Path + ": " + res.Sync.Summary()}
		out.notice = notice{text: "Synced " + r.Repo + ": " + res.Sync.Summary() + "."}
		return out
	}
}

// branchCmd finds the branch a push would push, for the confirm.
func (m *Forges) branchCmd(forgeName string, r forge.RepoStatus) tea.Cmd {
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		b, err := src.PushBranch(ctx, r.Repo)
		return branchMsg{forge: forgeName, path: r.Path, repo: r.Repo, branch: b, err: err}
	}
}

func (m *Forges) onBranch(msg branchMsg) tea.Cmd {
	if msg.err != nil || msg.branch == "" {
		err := msg.err
		if err == nil {
			err = errors.New("no branch to push: HEAD is detached and the forge's default branch is unknown")
		}
		m.notice = notice{text: "✗ Cannot push " + msg.repo + ": " + err.Error(), err: true}
		return nil
	}
	q := fmt.Sprintf("Push %s of %s to %s?  Fast-forward only, no force.", msg.branch, msg.repo, msg.forge)
	m.confirm = prompt.NewConfirm(m.common, q)
	m.onYes = func() tea.Cmd { return m.pushCmd(msg.forge, msg.path, msg.repo, msg.branch) }
	return nil
}

// pushCmd pushes a branch in the background. A refused push says why.
func (m *Forges) pushCmd(forgeName, path, repo, branch string) tea.Cmd {
	rk := rowKey(forgeName, path)
	m.busy[rk] = "pushing…"
	delete(m.results, rk)
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		res, err := src.Push(ctx, repo, branch)
		if ctx.Err() != nil {
			return nil
		}
		out := opMsg{keys: []string{rk}, forge: forgeName, rows: map[string]result{}}
		var ahead *forge.UpstreamAheadError
		switch {
		case errors.As(err, &ahead):
			out.rows[rk] = result{short: "✗ push refused", detail: "Push refused: " + err.Error(), err: true}
		case err != nil:
			out.rows[rk] = result{short: "✗ push failed", detail: "Push failed: " + err.Error(), err: true}
		case res.UpToDate:
			out.rows[rk] = result{short: "✓ up to date", detail: fmt.Sprintf("%s is already up to date on %s.", branch, forgeName)}
		default:
			out.rows[rk] = result{short: "✓ pushed", detail: fmt.Sprintf("Pushed %s of %s to %s: %s..%s.", branch, repo, forgeName, short(res.Old), short(res.New))}
		}
		r := out.rows[rk]
		out.notice = notice{text: r.detail, err: r.err}
		if r.err {
			out.notice.text = "✗ " + r.detail
		}
		return out
	}
}

// short abbreviates an object id; "" (a branch the forge did not have) is
// "(new)".
func short(oid string) string {
	switch {
	case oid == "":
		return "(new)"
	case len(oid) > 7:
		return oid[:7]
	default:
		return oid
	}
}

func (m *Forges) onOp(msg opMsg) tea.Cmd {
	for _, k := range msg.keys {
		delete(m.busy, k)
	}
	delete(m.progress, msg.forge)
	for k, r := range msg.rows {
		m.results[k] = r
	}
	m.notice = msg.notice
	if msg.reposChanged {
		return tea.Batch(m.loadCmd(msg.forge), func() tea.Msg { return ReposChangedMsg{} })
	}
	return m.loadCmd(msg.forge)
}

// perPage is about how many rows fit.
func (m *Forges) perPage() int {
	per := m.common.Height - 3
	if m.narrow() {
		per /= 2
	}
	return max(per, 1)
}

// narrow reports whether the tab uses its narrow layout.
func (m *Forges) narrow() bool { return m.common.Width < NarrowWidth }

// FooterLine is what replaces the help line: a confirm. "" when there is
// none.
func (m *Forges) FooterLine(width int) string {
	if m.confirm == nil {
		return ""
	}
	return m.confirm.View(width)
}

// ShortHelp implements help.KeyMap.
func (m *Forges) ShortHelp() []key.Binding {
	switch {
	case m.confirm != nil:
		return []key.Binding{m.confirm.Yes, m.confirm.No}
	case m.importForm != nil:
		return m.importForm.ShortHelp()
	case m.typing:
		return []key.Binding{m.keys.Apply, m.keys.Cancel}
	case m.statusErr != nil:
		retry := m.keys.Sync
		retry.SetHelp("r", "retry")
		return []key.Binding{retry}
	}
	return []key.Binding{m.common.KeyMap.UpDown, m.keys.Open, m.keys.Import, m.keys.ImportAll, m.keys.Sync, m.keys.Push, m.keys.Filter}
}

// FullHelp implements help.KeyMap.
func (m *Forges) FullHelp() [][]key.Binding {
	if m.confirm != nil || m.importForm != nil || m.typing || m.statusErr != nil {
		return [][]key.Binding{m.ShortHelp()}
	}
	km := m.common.KeyMap
	return [][]key.Binding{
		{km.UpDown, km.NextPage, km.PrevPage, m.keys.Top, m.keys.Bottom},
		{m.keys.Open, m.keys.Import, m.keys.ImportAll},
		{m.keys.Sync, m.keys.Push, m.keys.Copy},
		{m.keys.Filter, m.keys.Clear},
	}
}
