package selection

import (
	"fmt"
	"sort"
	"strconv"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/tabs"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/sessions"
)

const (
	defaultNoContent = "No readme found.\n\nCreate a `.soft-serve` repository and add a `README.md` file to display readme."
)

// pane is a tab of the home page.
type pane int

const (
	selectorPane pane = iota
	sessionsPane
	readmePane
)

func (p pane) String() string {
	return map[pane]string{
		selectorPane: "Repositories",
		sessionsPane: "Sessions",
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
	activePane pane
	// panes are the tabs shown, in order.
	panes []pane
	tabs  *tabs.Tabs
	// tabKeys jumps to a tab by its number, or cycles through them.
	tabKeys key.Binding
}

// New creates a new selection model.
func New(c common.Common) *Selection {
	sel := &Selection{
		common:     c,
		activePane: selectorPane, // start with the selector focused
	}

	// The home tabs, in order. A feature's tab is added here and left out
	// when the feature is off in the config, or when the user is not
	// authenticated (tui-screens.md, section 3).
	sel.panes = []pane{selectorPane}
	if src := sessions.SourceFor(c.Context()); src != nil {
		sel.sessions = sessions.New(c, src)
		sel.panes = append(sel.panes, sessionsPane)
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
	selector := selector.New(c,
		[]selector.IdentifiableItem{},
		NewItemDelegate(&c, &sel.activePane))
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
}

// IsFiltering returns true when keys go to text input: the repository
// filter is typed, or the active tab has a form, confirm or filter open.
func (s *Selection) IsFiltering() bool {
	return (s.activePane == selectorPane && s.FilterState() == list.Filtering) || s.Capturing()
}

// Capturing reports whether the active tab takes every key, so the frame
// leaves q, ?, tab and the digits to it.
func (s *Selection) Capturing() bool {
	return s.activePane == sessionsPane && s.sessions != nil && s.sessions.Capturing()
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
	if s.activePane == sessionsPane && s.sessions != nil {
		return s.sessions.FooterLine(width)
	}
	return ""
}

// Close stops the tabs' background work. The UI calls it when it quits.
func (s *Selection) Close() {
	if s.sessions != nil {
		s.sessions.Close()
	}
}

// OpenNewSession opens the Sessions tab's new-session form for repo (the
// Repositories tab's s key, next change).
func (s *Selection) OpenNewSession(repo string) tea.Cmd {
	if s.sessions == nil {
		return nil
	}
	return tea.Batch(s.selectPane(sessionsPane), s.sessions.OpenNewSession(repo, true))
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
	return nil
}

// ShortHelp implements help.KeyMap.
func (s *Selection) ShortHelp() []key.Binding {
	if s.Capturing() {
		return s.sessions.ShortHelp()
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
	case sessionsPane:
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
		return s.sessions.FullHelp()
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
		b = append(b, s.sessions.FullHelp()...)
	case selectorPane:
		copyKey := s.common.KeyMap.Copy
		copyKey.SetHelp("c", "copy command")
		k := s.selector.KeyMap
		if s.FilterState() != list.Filtering {
			b[0] = append(b[0],
				s.common.KeyMap.Select,
				copyKey,
			)
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

	ctx := s.common.Context()
	be := s.common.Backend()
	pk := s.common.PublicKey()
	if pk == nil && !be.AllowKeyless(ctx) {
		return sessionsCmd
	}

	repos, err := be.Repositories(ctx)
	if err != nil {
		return tea.Batch(sessionsCmd, common.ErrorCmd(err))
	}
	sortedItems := make(Items, 0)
	for _, r := range repos {
		if r.Name() == ".soft-serve" {
			readme, path, err := backend.Readme(r, nil)
			if err != nil {
				continue
			}

			readmeCmd = s.readme.SetContent(readme, path)
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
	items := make([]selector.IdentifiableItem, len(sortedItems))
	for i, it := range sortedItems {
		items[i] = it
	}
	return tea.Batch(
		s.selector.Init(),
		s.selector.SetItems(items),
		readmeCmd,
		sessionsCmd,
	)
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
	case tea.KeyPressMsg:
		if s.Capturing() {
			// A form, confirm or filter of the tab has the keys.
			_, cmd := s.sessions.Update(msg)
			return s, cmd
		}
		repoFiltering := s.activePane == selectorPane && s.FilterState() == list.Filtering
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
	default:
		// The Sessions tab's live updates and results reach it whether
		// it is shown or not: the header summary follows them.
		if s.sessions != nil {
			_, cmd := s.sessions.Update(msg)
			if cmd != nil {
				cmds = append(cmds, cmd)
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
			_, cmd := s.sessions.Update(msg)
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
		view = ss.Render(s.selector.View())
	case sessionsPane:
		view = s.sessions.View()
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
