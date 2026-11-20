package ssh

import (
	"errors"
	"slices"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/danielboothcloud/sundae/git"
	"github.com/danielboothcloud/sundae/pkg/jjview"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/footer"
	"github.com/danielboothcloud/sundae/pkg/ui/components/header"
	"github.com/danielboothcloud/sundae/pkg/ui/components/selector"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/forges"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/repo"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/selection"
)

type page int

const (
	selectionPage page = iota
	repoPage
)

type sessionState int

const (
	loadingState sessionState = iota
	errorState
	readyState
)

// UI is the main UI model.
type UI struct {
	serverName  string
	initialRepo string
	common      common.Common
	pages       []common.Component
	activePage  page
	state       sessionState
	header      *header.Header
	footer      *footer.Footer
	error       error
}

// NewUI returns a new UI model.
func NewUI(c common.Common, initialRepo string) *UI {
	serverName := c.Config().Name
	h := header.New(c, serverName)
	ui := &UI{
		serverName:  serverName,
		common:      c,
		pages:       make([]common.Component, 2), // selection & repo
		activePage:  selectionPage,
		state:       loadingState,
		header:      h,
		initialRepo: initialRepo,
	}
	ui.footer = footer.New(c, ui)
	return ui
}

func (ui *UI) getMargins() (wm, hm int) {
	style := ui.common.Styles.App
	switch ui.activePage {
	case selectionPage:
		hm += ui.common.Styles.ServerName.GetHeight() +
			ui.common.Styles.ServerName.GetVerticalFrameSize()
	case repoPage:
	}
	wm += style.GetHorizontalFrameSize()
	hm += style.GetVerticalFrameSize()
	hm += lipgloss.Height(ui.footerView())
	return
}

// jjViewer is the server's jj viewer, for the Changes tab of jj
// repositories; nil without a config.
func (ui *UI) jjViewer() *jjview.Viewer {
	cfg := ui.common.Config()
	if cfg == nil {
		return nil
	}
	return jjview.ForDataPath(cfg.DataPath)
}

// selection returns the home page.
func (ui *UI) selection() *selection.Selection {
	if s, ok := ui.pages[selectionPage].(*selection.Selection); ok {
		return s
	}
	return nil
}

// footerView renders the footer, the same on home and the repository page:
// the active page's message slot, then the help.
func (ui *UI) footerView() string {
	if m, ok := ui.pages[ui.activePage].(common.Messenger); ok && ui.state == readyState {
		return ui.footer.ViewWithMessage(m.Message())
	}
	return ui.footer.View()
}

// repo returns the repository page.
func (ui *UI) repo() *repo.Repo {
	if r, ok := ui.pages[repoPage].(*repo.Repo); ok {
		return r
	}
	return nil
}

// capturing reports whether the active page's tab takes every key (a
// form, confirm or filter is open).
func (ui *UI) capturing() bool {
	switch ui.activePage {
	case selectionPage:
		s := ui.selection()
		return s != nil && s.Capturing()
	case repoPage:
		r := ui.repo()
		return r != nil && r.Capturing()
	}
	return false
}

// close stops the pages' background work before quitting.
func (ui *UI) close() {
	if s := ui.selection(); s != nil {
		s.Close()
	}
	// Stop bubblezone background workers.
	ui.common.Zone.Close()
}

// ShortHelp implements help.KeyMap.
func (ui *UI) ShortHelp() []key.Binding {
	b := make([]key.Binding, 0)
	switch ui.state {
	case errorState:
		b = append(b, ui.common.KeyMap.Back)
	case readyState:
		b = append(b, ui.pages[ui.activePage].ShortHelp()...)
	}
	// q ranks lowest, so it shows only where every other key fits.
	if !ui.IsFiltering() {
		b = append(b, ui.common.KeyMap.Quit)
	}
	if !ui.capturing() {
		more := ui.common.KeyMap.Help
		more.SetHelp("?", "more")
		b = append(b, more)
	}
	return b
}

// FullHelp implements help.KeyMap.
func (ui *UI) FullHelp() [][]key.Binding {
	b := make([][]key.Binding, 0)
	switch ui.state {
	case errorState:
		b = append(b, []key.Binding{ui.common.KeyMap.Back})
	case readyState:
		b = append(b, ui.pages[ui.activePage].FullHelp()...)
	}
	less := ui.common.KeyMap.Help
	less.SetHelp("?", "less")
	h := []key.Binding{less}
	if !ui.IsFiltering() {
		h = append(h, ui.common.KeyMap.Quit)
	}
	// In the first column, which narrow widths never drop: the short help drops q first.
	if len(b) > 0 {
		b[0] = append(slices.Clone(b[0]), h...)
		return b
	}
	return append(b, h)
}

// SetSize implements common.Component.
func (ui *UI) SetSize(width, height int) {
	ui.common.SetSize(width, height)
	wm, hm := ui.getMargins()
	ui.header.SetSize(width-wm, height-hm)
	ui.footer.SetSize(width-wm, height-hm)
	// The page keeps half the screen while ? shows every key.
	ui.footer.SetFullHeight((height - ui.common.Styles.App.GetVerticalFrameSize()) / 2)
	for _, p := range ui.pages {
		if p != nil {
			p.SetSize(width-wm, height-hm)
		}
	}
}

// Init implements tea.Model.
func (ui *UI) Init() tea.Cmd {
	sel := selection.New(ui.common)
	ui.pages[selectionPage] = sel
	tabs := []common.TabComponent{
		repo.NewReadme(ui.common),
		repo.NewFiles(ui.common),
		repo.NewHistory(ui.common, ui.jjViewer()),
		repo.NewRefs(ui.common, git.RefsHeads),
		repo.NewRefs(ui.common, git.RefsTags),
	}
	// The repository's sessions, after Tags (section 8), when the user
	// has sessions: the home Sessions tab's model, narrowed to it.
	if m := sel.SessionsModel(); m != nil {
		tabs = append(tabs, repo.NewSessions(ui.common, m))
	}
	ui.pages[repoPage] = repo.New(ui.common, tabs...)
	ui.SetSize(ui.common.Width, ui.common.Height)
	cmds := make([]tea.Cmd, 0)
	cmds = append(cmds,
		ui.pages[selectionPage].Init(),
		ui.pages[repoPage].Init(),
	)
	if ui.initialRepo != "" {
		cmds = append(cmds, ui.initialRepoCmd(ui.initialRepo))
	}
	ui.state = readyState
	ui.SetSize(ui.common.Width, ui.common.Height)
	return tea.Batch(cmds...)
}

// IsFiltering returns true if the selection page is filtering, or the
// active page's tab takes every key.
func (ui *UI) IsFiltering() bool {
	if ui.activePage == selectionPage {
		if s := ui.selection(); s != nil && s.IsFiltering() {
			return true
		}
	}
	return ui.capturing()
}

// Update implements tea.Model.
func (ui *UI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	ui.common.Logger.Debugf("msg received: %T", msg)
	cmds := make([]tea.Cmd, 0)
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		ui.SetSize(msg.Width, msg.Height)
		for i, p := range ui.pages {
			m, cmd := p.Update(msg)
			ui.pages[i] = m.(common.Component)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, ui.common.KeyMap.Back) && ui.error != nil:
			ui.error = nil
			ui.state = readyState
		case key.Matches(msg, ui.common.KeyMap.Help) && !ui.capturing():
			cmds = append(cmds, footer.ToggleFooterCmd)
		case key.Matches(msg, ui.common.KeyMap.Quit):
			if !ui.IsFiltering() {
				ui.close()
				return ui, tea.Quit
			}
		case ui.activePage == repoPage &&
			ui.pages[ui.activePage].(*repo.Repo).Path() == "" &&
			key.Matches(msg, ui.common.KeyMap.Back):
			ui.activePage = selectionPage
		}
	case tea.MouseClickMsg:
		switch msg.Button {
		case tea.MouseLeft:
			switch {
			case ui.common.Zone.Get("footer").InBounds(msg):
				cmds = append(cmds, footer.ToggleFooterCmd)
			}
		}
	case footer.ToggleFooterMsg:
		ui.footer.SetShowAll(!ui.footer.ShowAll())
	case repo.RepoMsg:
		ui.common.SetValue(common.RepoKey, msg)
		ui.activePage = repoPage
		cmds = append(cmds, repo.UpdateRefCmd(msg))
	case common.ErrorMsg:
		ui.error = msg
		ui.state = errorState
	case forges.OpenRepoMsg:
		cmds = append(cmds, ui.setRepoCmd(msg.Repo))
	case selector.SelectMsg:
		switch msg.IdentifiableItem.(type) {
		case selection.Item:
			if ui.activePage == selectionPage {
				cmds = append(cmds, ui.setRepoCmd(msg.ID()))
			}
		}
	}
	h, cmd := ui.header.Update(msg)
	ui.header = h.(*header.Header)
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	f, cmd := ui.footer.Update(msg)
	ui.footer = f.(*footer.Footer)
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	if ui.activePage != selectionPage && selection.IsMsg(msg) {
		// The home tabs keep following their live updates while a
		// repository is open.
		m, cmd := ui.pages[selectionPage].Update(msg)
		ui.pages[selectionPage] = m.(common.Component)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	} else if ui.state != loadingState {
		m, cmd := ui.pages[ui.activePage].Update(msg)
		ui.pages[ui.activePage] = m.(common.Component)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	// This fixes determining the height margin of the footer.
	ui.SetSize(ui.common.Width, ui.common.Height)
	return ui, tea.Batch(cmds...)
}

// View implements tea.Model.
func (ui *UI) View() tea.View {
	var v tea.View
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion

	var view string
	wm, hm := ui.getMargins()
	if r, s := ui.repo(), ui.selection(); ui.activePage == repoPage && r != nil && s != nil {
		// The attention summary stays in view in a repository (R7), in its header.
		r.SetSummary(s.Summary)
	}
	switch ui.state {
	case loadingState:
		view = common.Busy("Loading")
	case errorState:
		err := ui.common.Styles.ErrorTitle.Render("Bummer")
		err += ui.common.Styles.ErrorBody.Render(ui.error.Error())
		view = ui.common.Styles.Error.
			Width(ui.common.Width -
				wm -
				ui.common.Styles.ErrorBody.GetHorizontalFrameSize()).
			Height(ui.common.Height -
				hm -
				ui.common.Styles.Error.GetVerticalFrameSize()).
			Render(err)
	case readyState:
		view = ui.pages[ui.activePage].View()
	default:
		view = "Unknown state :/ this is a bug!"
	}
	if ui.activePage == selectionPage {
		if s := ui.selection(); s != nil {
			ui.header.SetRight(s.HeaderRight)
		}
		view = lipgloss.JoinVertical(lipgloss.Left, ui.header.View(), view)
	}
	view = lipgloss.JoinVertical(lipgloss.Left, view, ui.footerView())
	v.Content = ui.common.Zone.Scan(
		ui.common.Styles.App.Render(view),
	)
	return v
}

func (ui *UI) openRepo(rn string) (proto.Repository, error) {
	cfg := ui.common.Config()
	if cfg == nil {
		return nil, errors.New("config is nil")
	}

	ctx := ui.common.Context()
	be := ui.common.Backend()
	repos, err := be.Repositories(ctx)
	if err != nil {
		ui.common.Logger.Debugf("ui: failed to list repos: %v", err)
		return nil, err
	}
	for _, r := range repos {
		if r.Name() == rn {
			return r, nil
		}
	}
	return nil, common.ErrMissingRepo
}

func (ui *UI) setRepoCmd(rn string) tea.Cmd {
	return func() tea.Msg {
		r, err := ui.openRepo(rn)
		if err != nil {
			return common.ErrorMsg(err)
		}
		return repo.RepoMsg(r)
	}
}

func (ui *UI) initialRepoCmd(rn string) tea.Cmd {
	return func() tea.Msg {
		r, err := ui.openRepo(rn)
		if err != nil {
			return nil
		}
		return repo.RepoMsg(r)
	}
}
