package ssh

import (
	"errors"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/danielboothcloud/sundae/git"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/footer"
	"github.com/danielboothcloud/sundae/pkg/ui/components/header"
	"github.com/danielboothcloud/sundae/pkg/ui/components/selector"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/forges"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/repo"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/selection"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/sessions"
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
	showFooter  bool
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
		showFooter:  true,
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
	if ui.showFooter {
		// NOTE: we don't use the footer's style to determine the margins
		// because footer.Height() is the height of the footer after applying
		// the styles.
		hm += lipgloss.Height(ui.footerView())
	}
	return
}

// selection returns the home page.
func (ui *UI) selection() *selection.Selection {
	if s, ok := ui.pages[selectionPage].(*selection.Selection); ok {
		return s
	}
	return nil
}

// footerView renders the footer: the help, or a line the home page puts in
// its place (the end-session confirm), laid out as the footer lays out the
// help.
func (ui *UI) footerView() string {
	if s := ui.selection(); s != nil && ui.activePage == selectionPage && ui.state == readyState {
		st := ui.common.Styles
		w := ui.common.Width - st.App.GetHorizontalFrameSize()
		if line := s.FooterLine(w - st.Footer.GetHorizontalFrameSize()); line != "" {
			return st.Footer.Width(w).Render(line)
		}
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
	b = append(b, h)
	return b
}

// SetSize implements common.Component.
func (ui *UI) SetSize(width, height int) {
	ui.common.SetSize(width, height)
	wm, hm := ui.getMargins()
	ui.header.SetSize(width-wm, height-hm)
	ui.footer.SetSize(width-wm, height-hm)
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
		repo.NewLog(ui.common),
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
			// Always show the footer on error.
			ui.showFooter = ui.footer.ShowAll()
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
			// Always show the footer on selection page.
			ui.showFooter = true
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
		// Show the footer when on repo page and shot all help.
		if ui.error == nil && ui.activePage == repoPage {
			ui.showFooter = !ui.showFooter
		}
	case repo.RepoMsg:
		ui.common.SetValue(common.RepoKey, msg)
		ui.activePage = repoPage
		// Show the footer on repo page if show all is set.
		ui.showFooter = ui.footer.ShowAll()
		cmds = append(cmds, repo.UpdateRefCmd(msg))
	case common.ErrorMsg:
		ui.error = msg
		ui.state = errorState
		ui.showFooter = true
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
		// The attention summary, in a repository, is at the status
		// bar's right end (R7).
		r.SetSummary(s.StatusSummary())
	}
	switch ui.state {
	case loadingState:
		view = "Loading..."
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
			ui.header.SetRight(s.Summary(ui.common.Width-wm < sessions.NarrowWidth))
		}
		view = lipgloss.JoinVertical(lipgloss.Left, ui.header.View(), view)
	}
	if ui.showFooter {
		view = lipgloss.JoinVertical(lipgloss.Left, view, ui.footerView())
	}
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
