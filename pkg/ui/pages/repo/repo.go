package repo

import (
	"fmt"
	"math"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/git"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/tabs"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/sessions"
)

type state int

const (
	loadingState state = iota
	readyState
)

// EmptyRepoMsg is a message to indicate that the repository is empty.
type EmptyRepoMsg struct{}

// CopyURLMsg is a message to copy the URL of the current repository.
type CopyURLMsg struct{}

// RepoMsg is a message that contains a git.Repository.
type RepoMsg proto.Repository //nolint:revive

// GoBackMsg is a message to go back to the previous view.
type GoBackMsg struct{}

// CopyMsg is a message to indicate copied text.
type CopyMsg struct {
	Text    string
	Message string
}

// SwitchTabMsg is a message to switch tabs.
type SwitchTabMsg common.TabComponent

// Repo is a view for a git repository.
type Repo struct {
	common       common.Common
	selectedRepo proto.Repository
	activeTab    int
	tabs         *tabs.Tabs
	panes        []common.TabComponent
	ref          *git.Reference
	state        state
	spinner      spinner.Model
	panesReady   []bool
	// notice is the page's own message (what was copied), until the next
	// key or click.
	notice string
	// summary is the attention summary with words; summaryNarrow, with
	// glyphs only, for a narrow terminal where the words do not fit.
	summary, summaryNarrow string
}

// New returns a new Repo.
func New(c common.Common, comps ...common.TabComponent) *Repo {
	ts := make([]string, 0)
	for _, c := range comps {
		ts = append(ts, c.TabName())
	}
	c.Logger = c.Logger.WithPrefix("ui.repo")
	// The home page's tabs: on a narrow terminal they collapse to the active
	// one (tui-screens.md section 9), so the Sessions tab stays reachable.
	tb := tabs.NewTopLevel(c, ts, sessions.NarrowWidth)
	// Make sure the order matches the order of tab constants above.
	s := spinner.New(spinner.WithSpinner(spinner.Dot),
		spinner.WithStyle(c.Styles.Spinner))
	r := &Repo{
		common:     c,
		tabs:       tb,
		panes:      comps,
		state:      loadingState,
		spinner:    s,
		panesReady: make([]bool, len(comps)),
	}
	return r
}

// tabNames returns the tab names for a repository of the given VCS.
func (r *Repo) tabNames(vcs proto.VCS) []string {
	names := make([]string, 0, len(r.panes))
	for _, p := range r.panes {
		if n, ok := p.(interface{ TabNameFor(proto.VCS) string }); ok {
			names = append(names, n.TabNameFor(vcs))
			continue
		}
		names = append(names, p.TabName())
	}
	return names
}

// headerLines is the header's height: the name and clone command, then
// the description and the context.
const headerLines = 2

// getMargins returns the height above the active tab: the header, the
// tabs and the body's top margin. The frame draws the footer.
func (r *Repo) getMargins() (int, int) {
	st := r.common.Styles
	hm := st.Tabs.GetHeight() + st.Tabs.GetVerticalFrameSize() + st.Repo.Body.GetMarginTop()
	if r.selectedRepo != nil {
		hm += headerLines
	}
	return 0, hm
}

// SetSize implements common.Component.
func (r *Repo) SetSize(width, height int) {
	r.common.SetSize(width, height)
	_, hm := r.getMargins()
	r.tabs.SetSize(width, height-hm)
	for _, p := range r.panes {
		p.SetSize(width, height-hm)
	}
}

// Path returns the current component path.
func (r *Repo) Path() string {
	return r.panes[r.activeTab].Path()
}

// commonHelp is the page's own keys, after the tab's, as home ends its
// help line with switching tabs.
func (r *Repo) commonHelp() []key.Binding {
	back := r.common.KeyMap.Back
	back.SetHelp("esc", "back")
	tab := r.common.KeyMap.Section
	tab.SetHelp("H/L", "tabs")
	return []key.Binding{back, tab}
}

// Capturing reports whether the active tab takes every key (the Sessions
// tab's form, confirm or filter): the page then leaves tab and esc to it,
// and the frame q and ?.
func (r *Repo) Capturing() bool {
	if c, ok := r.panes[r.activeTab].(interface{ Capturing() bool }); ok {
		return c.Capturing()
	}
	return false
}

// SetSummary sets the attention summary at the header's right end
// (tui-screens.md R7), with words and with glyphs only; "" for none.
func (r *Repo) SetSummary(words, glyphs string) {
	r.summary, r.summaryNarrow = words, glyphs
}

// Message implements common.Messenger: the page's notice, else the
// active tab's message.
func (r *Repo) Message() (string, common.MessageKind) {
	if r.notice != "" {
		return r.notice, common.MessageInfo
	}
	if m, ok := r.panes[r.activeTab].(common.Messenger); ok {
		return m.Message()
	}
	return "", common.MessageInfo
}

// ShortHelp implements help.KeyMap.
func (r *Repo) ShortHelp() []key.Binding {
	if r.Capturing() {
		return r.panes[r.activeTab].(help.KeyMap).ShortHelp()
	}
	return append(r.panes[r.activeTab].(help.KeyMap).ShortHelp(), r.commonHelp()...)
}

// FullHelp implements help.KeyMap.
func (r *Repo) FullHelp() [][]key.Binding {
	b := make([][]key.Binding, 0)
	b = append(b, r.commonHelp())
	b = append(b, r.panes[r.activeTab].(help.KeyMap).FullHelp()...)
	return b
}

// Init implements tea.View.
func (r *Repo) Init() tea.Cmd {
	r.state = loadingState
	r.activeTab = 0
	return tea.Batch(
		r.tabs.Init(),
		r.spinner.Tick,
	)
}

// Update implements tea.Model.
func (r *Repo) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	cmds := make([]tea.Cmd, 0)
	switch msg := msg.(type) {
	case RepoMsg:
		// Set the state to loading when we get a new repository.
		r.selectedRepo = msg
		r.tabs.SetTabs(r.tabNames(msg.Vcs()))
		cmds = append(cmds,
			r.Init(),
			// This will set the selected repo in each pane's model.
			r.updateModels(msg),
		)
	case RefMsg:
		r.ref = msg
		cmds = append(cmds, r.updateModels(msg))
		r.state = readyState
	case tabs.SelectTabMsg:
		r.activeTab = int(msg)
		t, cmd := r.tabs.Update(msg)
		r.tabs = t.(*tabs.Tabs)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	case tabs.ActiveTabMsg:
		r.activeTab = int(msg)
	case tea.KeyPressMsg, tea.MouseClickMsg:
		r.notice = ""
		if _, ok := msg.(tea.KeyPressMsg); ok && r.Capturing() {
			// A form, confirm or filter of the tab has the keys.
			break
		}
		t, cmd := r.tabs.Update(msg)
		r.tabs = t.(*tabs.Tabs)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		if r.selectedRepo != nil {
			urlID := fmt.Sprintf("%s-url", r.selectedRepo.Name())
			cmd := r.common.CloneCmd(r.common.Config().SSH.PublicURL, r.selectedRepo.Name(), r.selectedRepo.Vcs())
			if msg, ok := msg.(tea.MouseMsg); ok && r.common.Zone.Get(urlID).InBounds(msg) {
				cmds = append(cmds, copyCmd(cmd, "Command copied to clipboard"))
			}
		}
		switch msg := msg.(type) {
		case tea.MouseClickMsg:
			switch msg.Button {
			case tea.MouseRight:
				switch {
				case r.common.Zone.Get("repo-main").InBounds(msg):
					cmds = append(cmds, goBackCmd)
				}
			}
		}
		switch msg := msg.(type) {
		case tea.KeyPressMsg:
			switch {
			case key.Matches(msg, r.common.KeyMap.Back):
				cmds = append(cmds, goBackCmd)
			}
		}
	case CopyMsg:
		txt := msg.Text
		if cfg := r.common.Config(); cfg != nil {
			cmds = append(cmds, tea.SetClipboard(txt))
		}
		r.notice = msg.Message
	case ReadmeMsg:
		cmds = append(cmds, r.updateTabComponent(&Readme{}, msg))
	case FileItemsMsg, FileContentMsg:
		cmds = append(cmds, r.updateTabComponent(&Files{}, msg))
	case LogItemsMsg, LogDiffMsg, LogCountMsg:
		cmds = append(cmds, r.updateTabComponent(&Log{}, msg))
	case RefItemsMsg:
		// By prefix, not tab name: a jj repository's Branches tab is named Bookmarks.
		for i, p := range r.panes {
			if refs, ok := p.(*Refs); ok && refs.refPrefix == msg.prefix {
				m, cmd := refs.Update(msg)
				r.panes[i] = m.(common.TabComponent)
				cmds = append(cmds, cmd)
				break
			}
		}
	case StashListMsg, StashPatchMsg:
		cmds = append(cmds, r.updateTabComponent(&Stash{}, msg))
	// We have two spinners, one is used to when loading the repository and the
	// other is used when loading the log.
	// Check if the spinner ID matches the spinner model.
	case spinner.TickMsg:
		if r.state == loadingState && r.spinner.ID() == msg.ID {
			s, cmd := r.spinner.Update(msg)
			r.spinner = s
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		} else {
			for i, c := range r.panes {
				if c.SpinnerID() == msg.ID {
					m, cmd := c.Update(msg)
					r.panes[i] = m.(common.TabComponent)
					if cmd != nil {
						cmds = append(cmds, cmd)
					}
					break
				}
			}
		}
	case tea.WindowSizeMsg:
		r.SetSize(msg.Width, msg.Height)
		cmds = append(cmds, r.updateModels(msg))
	case EmptyRepoMsg:
		r.ref = nil
		r.state = readyState
		cmds = append(cmds, r.updateModels(msg))
	case common.ErrorMsg:
		r.state = readyState
	case SwitchTabMsg:
		for i, c := range r.panes {
			if c.TabName() == msg.TabName() {
				cmds = append(cmds, tabs.SelectTabCmd(i))
				break
			}
		}
	}
	active := r.panes[r.activeTab]
	m, cmd := active.Update(msg)
	r.panes[r.activeTab] = m.(common.TabComponent)
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	return r, tea.Batch(cmds...)
}

// View implements tea.Model.
func (r *Repo) View() string {
	_, hm := r.getMargins()
	var main string
	switch r.state {
	case loadingState:
		main = fmt.Sprintf("%s loading…", r.spinner.View())
	case readyState:
		main = r.panes[r.activeTab].View()
	}
	// The frame's footer starts with a blank line, so the body needs no
	// bottom margin.
	main = r.common.Zone.Mark(
		"repo-main",
		r.common.Styles.Repo.Body.MarginBottom(0).Height(r.common.Height-hm).Render(main),
	)
	view := lipgloss.JoinVertical(lipgloss.Left,
		r.headerView(),
		r.common.Styles.Tabs.Render(r.tabs.View()),
		main,
	)
	return r.common.Styles.Repo.Base.
		Width(r.common.Width).
		MaxHeight(r.common.Height).
		Render(view)
}

// headerView is the page's header: the name and the clone command, then
// the description and the context.
func (r *Repo) headerView() string {
	if r.selectedRepo == nil {
		return ""
	}
	w := r.common.Width
	name := r.selectedRepo.ProjectName()
	if name == "" {
		name = r.selectedRepo.Name()
	}
	name = r.common.Styles.Repo.HeaderName.Render(common.TruncateString(name, w))
	urlWidth := w - lipgloss.Width(name) - 1
	urlStyle := r.common.Styles.URLStyle.
		Width(max(urlWidth, 0)).
		Align(lipgloss.Right)
	var url string
	if cfg := r.common.Config(); cfg != nil {
		url = r.common.CloneCmd(cfg.SSH.PublicURL, r.selectedRepo.Name(), r.selectedRepo.Vcs())
	}
	url = r.common.Zone.Mark(
		fmt.Sprintf("%s-url", r.selectedRepo.Name()),
		urlStyle.Render(common.TruncateString(url, urlWidth)),
	)
	return lipgloss.NewStyle().MaxWidth(w).Render(lipgloss.JoinVertical(lipgloss.Left,
		lipgloss.JoinHorizontal(lipgloss.Top, name, url),
		r.contextLine(w),
	))
}

// contextLine is the header's second line: the description, and at the
// right end the context, which takes the room the description needs.
func (r *Repo) contextLine(width int) string {
	right := r.contextView(width)
	room := width
	if right != "" {
		room -= lipgloss.Width(right) + 2
	}
	left := ""
	if desc := strings.Join(strings.Fields(r.selectedRepo.Description()), " "); desc != "" && room >= minCut {
		left = r.common.Styles.Repo.HeaderDesc.Render(common.TruncateString(desc, room))
	}
	gap := max(width-lipgloss.Width(left)-lipgloss.Width(right), 0)
	return left + strings.Repeat(" ", gap) + right
}

// minCut is the fewest cells a cut path or description keeps; shorter
// reads as noise, so it is left out instead.
const minCut = 8

// contextView is the ref, the tab's item and position, then the summary. The item
// gives way first, then the ref; the words go only when narrow and nothing else can.
func (r *Repo) contextView(width int) string {
	var ref, item, at string
	if r.ref != nil {
		ref = r.ref.Name().Short()
	}
	if r.state == readyState {
		item, at = r.panes[r.activeTab].Position()
	}
	sums := []string{r.summary}
	if width < sessions.NarrowWidth && r.summaryNarrow != r.summary {
		sums = append(sums, r.summaryNarrow)
	}
	for _, sum := range sums {
		for _, ref := range []string{ref, ""} {
			if item == "" {
				break
			}
			// Paths keep their end, the file's name.
			without := lipgloss.Width(r.renderContext(sum, ref, "x", at)) - 1
			if room := width - without; room >= min(lipgloss.Width(item), minCut) {
				return r.renderContext(sum, ref, cutLeft(item, room), at)
			}
		}
		for _, parts := range [][]string{{ref, at}, {at}} {
			if v := r.renderContext(sum, parts...); lipgloss.Width(v) <= width {
				return v
			}
		}
	}
	return ansi.Truncate(r.renderContext(sums[len(sums)-1], at), width, "…")
}

// renderContext joins the parts that are not empty with " · ", then the
// summary after a wider gap, as it is about every session, not this tab.
func (r *Repo) renderContext(summary string, parts ...string) string {
	st := r.common.Styles
	sep := lipgloss.NewStyle().Foreground(st.HelpDivider.GetForeground()).Render(" · ")
	on := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			on = append(on, st.Repo.HeaderContext.Render(p))
		}
	}
	ctx := strings.Join(on, sep)
	switch {
	case summary == "":
		return ctx
	case ctx == "":
		return summary
	}
	return ctx + "   " + summary
}

// cutLeft cuts s to n cells from the left, marking the cut with "…".
func cutLeft(s string, n int) string {
	w := ansi.StringWidth(s)
	if w <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return ansi.TruncateLeft(s, w-n+1, "…")
}

// scrollPosition is how far a view is scrolled, from its 0-1 fraction.
func scrollPosition(fraction float64) string {
	p := fraction * 100
	if p < 0 || math.IsNaN(p) {
		p = 0
	}
	return common.ScrollPercent(int(math.Round(p)))
}

func (r *Repo) updateTabComponent(c common.TabComponent, msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, 0)
	for i, b := range r.panes {
		if b.TabName() == c.TabName() {
			m, cmd := b.Update(msg)
			r.panes[i] = m.(common.TabComponent)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			break
		}
	}
	return tea.Batch(cmds...)
}

func (r *Repo) updateModels(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, 0)
	for i, b := range r.panes {
		m, cmd := b.Update(msg)
		r.panes[i] = m.(common.TabComponent)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

func copyCmd(text, msg string) tea.Cmd {
	return func() tea.Msg {
		return CopyMsg{
			Text:    text,
			Message: msg,
		}
	}
}

// openKey is enter on a list row, the tab's main key.
func openKey(c common.Common) key.Binding {
	k := c.KeyMap.Select
	k.SetHelp("enter", "open")
	return k
}

func goBackCmd() tea.Msg {
	return GoBackMsg{}
}

func switchTabCmd(m common.TabComponent) tea.Cmd {
	return func() tea.Msg {
		return SwitchTabMsg(m)
	}
}

func renderLoading(c common.Common, s spinner.Model) string {
	msg := fmt.Sprintf("%s loading…", s.View())
	return c.Styles.SpinnerContainer.
		Height(c.Height).
		Render(msg)
}
