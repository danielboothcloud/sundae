package selection

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/danielboothcloud/sundae/pkg/access"
	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/code"
	"github.com/danielboothcloud/sundae/pkg/ui/components/prompt"
	"github.com/danielboothcloud/sundae/pkg/ui/components/selector"
	"github.com/danielboothcloud/sundae/pkg/ui/components/tabs"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/forges"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/sessions"
)

const (
	defaultNoContent = "No readme found.\n\nCreate a `.sundae` repository and add a `README.md` file to display readme."
)

// pane is a tab of the home page.
type pane int

const (
	selectorPane pane = iota
	sessionsPane
	forgesPane
	readmePane
)

func (p pane) String() string {
	return map[pane]string{
		selectorPane: "Repositories",
		sessionsPane: "Sessions",
		forgesPane:   "Forges",
		readmePane:   "About",
	}[p]
}

// Selection is the model for the selection screen/page: the home page,
// with its tabs.
type Selection struct {
	common     common.Common
	readme     *code.Code
	selector   *selector.Selector
	sessions   *sessions.Sessions
	forges     *forges.Forges
	activePane pane
	// panes are the tabs shown, in order.
	panes []pane
	tabs  *tabs.Tabs
	// tabKeys jumps to a tab by its number, or cycles through them.
	tabKeys key.Binding
	// newSession is the Repositories tab's s key.
	newSession key.Binding
	// newRepo is the Repositories tab's n key: the new-repository form.
	newRepo key.Binding
	// deleteRepo is the Repositories tab's D key: delete the selected
	// repository, after a confirm.
	deleteRepo key.Binding
	// deleting is the delete confirm while it is open, and deleteTarget
	// the repository it asks about.
	deleting     *prompt.Confirm
	deleteTarget string

	// repoForm is the new-repository form while it is open; creating
	// names the repository it is creating.
	repoForm *prompt.Form
	creating string
	// repoNotice is a one-line message about the last repository
	// created, shown in the footer until the next key.
	repoNotice string

	// badges are the Repositories tab's session and origin badges, which
	// delegate renders.
	badges     *badges
	delegate   *ItemDelegate
	origins    OriginSource
	originFeed forges.Feed

	ctx    context.Context
	cancel context.CancelFunc
}

// New creates a new selection model.
func New(c common.Common) *Selection {
	ctx, cancel := context.WithCancel(c.Context())
	sel := &Selection{
		common:     c,
		activePane: selectorPane, // start with the selector focused
		badges:     &badges{},
		ctx:        ctx,
		cancel:     cancel,
		newSession: key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "new session")),
		newRepo:    key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new repository")),
		deleteRepo: key.NewBinding(key.WithKeys("D"), key.WithHelp("D", "delete repository")),
	}

	// The home tabs, in order. A feature's tab is added here and left out
	// when the feature is off in the config, or when the user is not
	// authenticated (tui-screens.md, section 3). Forges is for server
	// admins only, as the `forge` command's listing, import and sync are.
	sel.panes = []pane{selectorPane}
	if src := sessions.SourceFor(c.Context()); src != nil {
		sel.sessions = sessions.New(c, src)
		sel.panes = append(sel.panes, sessionsPane)
		sel.badges.sessionsOn = true
	}
	if src := forges.SourceFor(c.Context()); src != nil {
		sel.forges = forges.New(c, src)
		sel.panes = append(sel.panes, forgesPane)
	}
	if src := OriginSourceFor(c.Context()); src != nil {
		sel.origins = src
		sel.badges.originsOn = true
	}
	sel.panes = append(sel.panes, readmePane)

	ts := make([]string, len(sel.panes))
	for i, p := range sel.panes {
		ts[i] = p.String()
	}
	t := tabs.New(c, ts)
	t.TabSeparator = lipgloss.NewStyle()
	t.TabInactive = c.Styles.TopLevelNormalTab
	t.TabActive = c.Styles.TopLevelActiveTab
	t.TabDot = c.Styles.TopLevelActiveTabDot
	t.UseDot = true
	t.CollapseBelow = sessions.NarrowWidth
	sel.tabs = t

	keys := []string{"tab", "shift+tab"}
	for i := range sel.panes {
		keys = append(keys, strconv.Itoa(i+1))
	}
	sel.tabKeys = key.NewBinding(key.WithKeys(keys...),
		key.WithHelp(fmt.Sprintf("1-%d", len(sel.panes)), "tab"))

	readme := code.New(c, "", "")
	readme.UseGlamour = true
	readme.NoContentStyle = c.Styles.NoContent.
		SetString(defaultNoContent)
	sel.delegate = NewItemDelegate(&c, &sel.activePane, sel.badges)
	selector := selector.New(c,
		[]selector.IdentifiableItem{},
		sel.delegate)
	selector.SetShowTitle(false)
	selector.SetShowHelp(false)
	selector.SetShowStatusBar(false)
	selector.DisableQuitKeybindings()
	sel.selector = selector
	sel.readme = readme
	return sel
}

func (s *Selection) getMargins() (wm, hm int) {
	wm = 0
	hm = s.common.Styles.Tabs.GetVerticalFrameSize() +
		s.common.Styles.Tabs.GetHeight()
	if s.activePane == selectorPane && s.FilterState() == list.Filtering {
		// hide tabs when filtering
		hm = 0
	}
	return
}

// FilterState returns the current filter state.
func (s *Selection) FilterState() list.FilterState {
	return s.selector.FilterState()
}

// SetSize implements common.Component.
func (s *Selection) SetSize(width, height int) {
	s.common.SetSize(width, height)
	wm, hm := s.getMargins()
	s.tabs.SetSize(width, height-hm)
	s.selector.SetSize(width-wm, height-hm)
	s.readme.SetSize(width-wm, height-hm-1) // -1 for readme status line
	if s.sessions != nil {
		s.sessions.SetSize(width-wm, height-hm)
	}
	if s.forges != nil {
		s.forges.SetSize(width-wm, height-hm)
	}
}

// bindSessions makes the Sessions model, which the repository page's
// Sessions tab shares, the home tab's: all sessions, at the home size.
func (s *Selection) bindSessions() {
	wm, hm := s.getMargins()
	s.sessions.SetSize(s.common.Width-wm, s.common.Height-hm)
	s.sessions.SetScope("")
}

// SessionsModel returns the Sessions tab's model, for the repository
// page's Sessions tab; nil when sessions are off.
func (s *Selection) SessionsModel() *sessions.Sessions { return s.sessions }

// StatusSummary is the attention summary for the repository page's
// status bar (R7); "" when there is none.
func (s *Selection) StatusSummary() string {
	if s.sessions == nil {
		return ""
	}
	return s.sessions.StatusSummary()
}

// IsFiltering returns true when keys go to text input: the repository
// filter is typed, or the active tab has a form, confirm or filter open.
func (s *Selection) IsFiltering() bool {
	return (s.activePane == selectorPane && s.FilterState() == list.Filtering) || s.Capturing()
}

// Capturing reports whether the active tab takes every key, so the frame
// leaves q, ?, tab and the digits to it.
func (s *Selection) Capturing() bool {
	switch s.activePane {
	case selectorPane:
		return s.repoForm != nil || s.deleting != nil
	case sessionsPane:
		return s.sessions != nil && s.sessions.Capturing()
	case forgesPane:
		return s.forges != nil && s.forges.Capturing()
	}
	return false
}

// capturingHelp is the help of the tab that takes every key.
func (s *Selection) capturingHelp() []key.Binding {
	switch s.activePane {
	case selectorPane:
		if s.creating != "" {
			return nil
		}
		if s.deleting != nil {
			return []key.Binding{s.deleting.Yes, s.deleting.No}
		}
		return s.repoForm.ShortHelp()
	case forgesPane:
		return s.forges.ShortHelp()
	}
	return s.sessions.ShortHelp()
}

// Summary is the attention summary for the header (section 3): "" when the
// user has no sessions, or sessions are off.
func (s *Selection) Summary(narrow bool) string {
	if s.sessions == nil {
		return ""
	}
	return s.sessions.Summary(narrow)
}

// FooterLine is a line that replaces the help footer, e.g. a confirm; ""
// for none.
func (s *Selection) FooterLine(width int) string {
	switch {
	case s.activePane == selectorPane && s.creating != "":
		return common.TruncateString("Creating "+s.creating+" …", max(width, 1))
	case s.activePane == selectorPane && s.deleting != nil:
		return s.deleting.View(max(width, 1))
	case s.activePane == selectorPane && s.repoNotice != "":
		line := common.TruncateString(s.repoNotice, max(width, 1))
		if strings.HasPrefix(s.repoNotice, "✗") {
			return s.common.Styles.State.Error.Render(line)
		}
		return line
	case s.activePane == sessionsPane && s.sessions != nil:
		return s.sessions.FooterLine(width)
	case s.activePane == forgesPane && s.forges != nil:
		return s.forges.FooterLine(width)
	}
	return ""
}

// Close stops the tabs' background work. The UI calls it when it quits.
func (s *Selection) Close() {
	s.cancel()
	if s.sessions != nil {
		s.sessions.Close()
	}
	if s.forges != nil {
		s.forges.Close()
	}
	if s.originFeed != nil {
		s.originFeed.Close()
	}
}

// OpenNewSession opens the Sessions tab's new-session form with repo
// picked; more repositories can be added (the Repositories tab's s key).
func (s *Selection) OpenNewSession(repo string) tea.Cmd {
	if s.sessions == nil {
		return nil
	}
	cmd := s.selectPane(sessionsPane)
	s.bindSessions()
	return tea.Batch(cmd, s.sessions.OpenNewSession(repo, false))
}

// selectPane makes p the active tab.
func (s *Selection) selectPane(p pane) tea.Cmd {
	for i, q := range s.panes {
		if q == p {
			s.activePane = p
			s.tabs.SetActiveTab(i)
			s.SetSize(s.common.Width, s.common.Height)
		}
	}
	return s.shown()
}

// shown starts the active tab when it is first shown: the Forges tab
// reads, and maybe lists, the forges only then.
func (s *Selection) shown() tea.Cmd {
	if s.activePane == forgesPane && s.forges != nil {
		return s.forges.Start()
	}
	return nil
}

// ShortHelp implements help.KeyMap.
func (s *Selection) ShortHelp() []key.Binding {
	if s.Capturing() {
		return s.capturingHelp()
	}
	k := s.selector.KeyMap
	kb := make([]key.Binding, 0)
	kb = append(kb,
		s.common.KeyMap.UpDown,
		s.tabKeys,
	)
	switch s.activePane {
	case selectorPane:
		copyKey := s.common.KeyMap.Copy
		copyKey.SetHelp("c", "copy command")
		kb = append(kb,
			s.common.KeyMap.Select,
			k.Filter,
			k.ClearFilter,
			copyKey,
		)
		if s.sessions != nil {
			kb = append(kb, s.newSession)
		}
		if s.canCreate() {
			kb = append(kb, s.newRepo)
		}
		if s.canDelete(s.SelectedRepo()) {
			kb = append(kb, s.deleteRepo)
		}
	case forgesPane:
		kb = kb[1:]
		sh := s.forges.ShortHelp()
		if len(sh) > 0 && sh[0].Help().Desc == s.common.KeyMap.UpDown.Help().Desc {
			kb = append([]key.Binding{sh[0]}, kb...)
			sh = sh[1:]
		}
		kb = append(kb, sh...)
	case sessionsPane:
		s.bindSessions()
		kb = kb[1:]
		sh := s.sessions.ShortHelp()
		if len(sh) > 0 && sh[0].Help().Desc == s.common.KeyMap.UpDown.Help().Desc {
			kb = append([]key.Binding{sh[0]}, kb...)
			sh = sh[1:]
		}
		kb = append(kb, sh...)
	}
	return kb
}

// FullHelp implements help.KeyMap.
func (s *Selection) FullHelp() [][]key.Binding {
	if s.Capturing() {
		return [][]key.Binding{s.capturingHelp()}
	}
	b := [][]key.Binding{
		{
			s.tabKeys,
		},
	}
	switch s.activePane {
	case readmePane:
		k := s.readme.KeyMap
		b = append(b, []key.Binding{
			k.PageDown,
			k.PageUp,
		})
		b = append(b, []key.Binding{
			k.HalfPageDown,
			k.HalfPageUp,
		})
		b = append(b, []key.Binding{
			k.Down,
			k.Up,
		})
	case sessionsPane:
		s.bindSessions()
		b = append(b, s.sessions.FullHelp()...)
	case forgesPane:
		b = append(b, s.forges.FullHelp()...)
	case selectorPane:
		copyKey := s.common.KeyMap.Copy
		copyKey.SetHelp("c", "copy command")
		k := s.selector.KeyMap
		if s.FilterState() != list.Filtering {
			b[0] = append(b[0],
				s.common.KeyMap.Select,
				copyKey,
			)
			if s.sessions != nil {
				b[0] = append(b[0], s.newSession)
			}
			if s.canCreate() {
				b[0] = append(b[0], s.newRepo)
			}
			if s.canDelete(s.SelectedRepo()) {
				b[0] = append(b[0], s.deleteRepo)
			}
		}
		b = append(b, []key.Binding{
			k.CursorUp,
			k.CursorDown,
		})
		b = append(b, []key.Binding{
			k.NextPage,
			k.PrevPage,
			k.GoToStart,
			k.GoToEnd,
		})
		b = append(b, []key.Binding{
			k.Filter,
			k.ClearFilter,
			k.CancelWhileFiltering,
			k.AcceptWhileFiltering,
		})
	}
	return b
}

// Init implements tea.Model.
func (s *Selection) Init() tea.Cmd {
	var readmeCmd, sessionsCmd tea.Cmd
	cfg := s.common.Config()
	if cfg == nil {
		return nil
	}
	if s.sessions != nil {
		sessionsCmd = s.sessions.Init()
	}
	if s.origins != nil {
		s.originFeed = s.origins.Follow()
		sessionsCmd = tea.Batch(sessionsCmd, s.originsCmd(), s.originNextCmd())
	}

	ctx := s.common.Context()
	be := s.common.Backend()
	pk := s.common.PublicKey()
	if pk == nil && !be.AllowKeyless(ctx) {
		return sessionsCmd
	}

	items, about, err := s.listRepos()
	if err != nil {
		return tea.Batch(sessionsCmd, common.ErrorCmd(err))
	}
	if about != nil {
		readmeCmd = s.readme.SetContent(about.readme, about.path)
	}
	return tea.Batch(
		s.selector.Init(),
		s.selector.SetItems(items),
		readmeCmd,
		sessionsCmd,
	)
}

// aboutReadme is the README of the .sundae repository, which the
// About tab shows.
type aboutReadme struct{ readme, path string }

// listRepos lists the repositories the user may read, for the Repositories
// tab, most recently updated first, and the .sundae README; nil when
// there is none. A .sundae repository without a README is not listed.
func (s *Selection) listRepos() ([]selector.IdentifiableItem, *aboutReadme, error) {
	ctx := s.common.Context()
	be := s.common.Backend()
	pk := s.common.PublicKey()
	repos, err := be.Repositories(ctx)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck
	}
	var about *aboutReadme
	sortedItems := make(Items, 0)
	for _, r := range repos {
		if r.Name() == ".sundae" {
			readme, path, err := backend.Readme(r, nil)
			if err != nil {
				continue
			}

			about = &aboutReadme{readme: readme, path: path}
		}

		if r.IsHidden() {
			continue
		}
		al := be.AccessLevelByPublicKey(ctx, r.Name(), pk)
		if al >= access.ReadOnlyAccess {
			item, err := NewItem(s.common, r)
			if err != nil {
				s.common.Logger.Debugf("ui: failed to create item for %s: %v", r.Name(), err)
				continue
			}
			sortedItems = append(sortedItems, item)
		}
	}
	sort.Sort(sortedItems)
	s.delegate.setUpdatedWidth(sortedItems)
	items := make([]selector.IdentifiableItem, len(sortedItems))
	for i, it := range sortedItems {
		items[i] = it
	}
	return items, about, nil
}

// repoItems lists the repositories for the Repositories tab.
func (s *Selection) repoItems() ([]selector.IdentifiableItem, error) {
	items, _, err := s.listRepos()
	return items, err
}

// Update implements tea.Model.
func (s *Selection) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	cmds := make([]tea.Cmd, 0)
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		r, cmd := s.readme.Update(msg)
		s.readme = r.(*code.Code)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		m, cmd := s.selector.Update(msg)
		s.selector = m.(*selector.Selector)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		return s, tea.Batch(cmds...)
	case repoCreatedMsg:
		return s, s.onRepoCreated(msg)
	case repoDeletedMsg:
		return s, s.onRepoDeleted(msg)
	case tea.KeyPressMsg:
		if s.activePane == selectorPane {
			s.repoNotice = ""
		}
		if s.Capturing() {
			// A form, confirm or filter of the tab has the keys.
			if s.activePane == selectorPane {
				if s.deleting != nil {
					return s, s.onDeleteKey(msg)
				}
				return s, s.onRepoFormKey(msg)
			}
			if s.activePane == forgesPane {
				_, cmd := s.forges.Update(msg)
				return s, cmd
			}
			s.bindSessions()
			_, cmd := s.sessions.Update(msg)
			return s, cmd
		}
		repoFiltering := s.activePane == selectorPane && s.FilterState() == list.Filtering
		if s.activePane == selectorPane && !repoFiltering && key.Matches(msg, s.newRepo) && s.canCreate() {
			s.openNewRepo()
			return s, nil
		}
		if s.activePane == selectorPane && !repoFiltering && key.Matches(msg, s.deleteRepo) {
			s.openDelete(s.SelectedRepo())
			return s, nil
		}
		if s.activePane == selectorPane && !repoFiltering && s.sessions != nil && key.Matches(msg, s.newSession) {
			if it, ok := s.selector.SelectedItem().(Item); ok {
				return s, s.OpenNewSession(it.ID())
			}
			return s, nil
		}
		if i, err := strconv.Atoi(msg.String()); err == nil && !repoFiltering {
			if i >= 1 && i <= len(s.panes) {
				return s, s.selectPane(s.panes[i-1])
			}
			return s, nil
		}
		if key.Matches(msg, s.common.KeyMap.Back) && s.activePane == selectorPane {
			cmds = append(cmds, s.selector.Init())
		}
		t, cmd := s.tabs.Update(msg)
		s.tabs = t.(*tabs.Tabs)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	case tea.MouseMsg:
		t, cmd := s.tabs.Update(msg)
		s.tabs = t.(*tabs.Tabs)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	case tabs.ActiveTabMsg:
		if i := int(msg); i >= 0 && i < len(s.panes) {
			s.activePane = s.panes[i]
		}
		s.SetSize(s.common.Width, s.common.Height)
		if cmd := s.shown(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	default:
		// The tabs' live updates and results reach them whether they are
		// shown or not: the header summary and the badges follow them.
		switch {
		case forges.IsMsg(msg):
			if s.forges != nil {
				_, cmd := s.forges.Update(msg)
				cmds = append(cmds, cmd)
			}
		case IsMsg(msg) && !sessions.IsMsg(msg):
			cmds = append(cmds, s.onBadgeMsg(msg))
		case s.sessions != nil:
			_, cmd := s.sessions.Update(msg)
			cmds = append(cmds, cmd)
			if sessions.IsMsg(msg) {
				s.refreshSessionBadges()
			}
		}
	}
	switch s.activePane {
	case readmePane:
		r, cmd := s.readme.Update(msg)
		s.readme = r.(*code.Code)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	case selectorPane:
		m, cmd := s.selector.Update(msg)
		s.selector = m.(*selector.Selector)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	case sessionsPane:
		switch msg.(type) {
		case tea.KeyPressMsg, tea.MouseMsg:
			s.bindSessions()
			_, cmd := s.sessions.Update(msg)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case forgesPane:
		switch msg.(type) {
		case tea.KeyPressMsg, tea.MouseMsg:
			_, cmd := s.forges.Update(msg)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	return s, tea.Batch(cmds...)
}

// View implements tea.Model.
func (s *Selection) View() string {
	var view string
	wm, hm := s.getMargins()
	switch s.activePane {
	case selectorPane:
		ss := lipgloss.NewStyle().
			Width(s.common.Width - wm).
			Height(s.common.Height - hm)
		if s.repoForm != nil {
			// Placed as the Sessions tab places its forms.
			lines := strings.Split(s.repoForm.View(s.common.Width-wm-1), "\n")
			for i, l := range lines {
				lines[i] = " " + l
			}
			view = ss.Render("\n" + strings.Join(lines, "\n"))
			break
		}
		// The session badges are what the Monitor reads now.
		s.refreshSessionBadges()
		view = ss.Render(s.selector.View())
	case sessionsPane:
		s.bindSessions()
		view = s.sessions.View()
	case forgesPane:
		view = s.forges.View()
	case readmePane:
		rs := lipgloss.NewStyle().
			Height(s.common.Height - hm)
		status := fmt.Sprintf("☰ %.f%%", s.readme.ScrollPercent()*100)
		readmeStatus := lipgloss.NewStyle().
			Align(lipgloss.Right).
			Width(s.common.Width - wm).
			Foreground(s.common.Styles.InactiveBorderColor).
			Render(status)
		view = rs.Render(lipgloss.JoinVertical(lipgloss.Left,
			s.readme.View(),
			readmeStatus,
		))
	}
	if s.activePane != selectorPane || s.FilterState() != list.Filtering {
		tabs := s.common.Styles.Tabs.Render(s.tabs.View())
		view = lipgloss.JoinVertical(lipgloss.Left,
			tabs,
			view,
		)
	}
	return lipgloss.JoinVertical(
		lipgloss.Left,
		view,
	)
}
