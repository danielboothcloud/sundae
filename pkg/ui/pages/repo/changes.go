package repo

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/jjview"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/tabs"
	"github.com/danielboothcloud/sundae/pkg/ui/components/viewport"
)

// changesLimit is how many changes the Changes tab lists.
const changesLimit = 500

// Messages of the Changes tab.
type (
	changesLogMsg struct {
		repo  string
		lines []jjview.Line
		err   error
	}
	changesShowMsg struct {
		change string
		out    string
		err    error
	}
)

// Changes is the Commits tab of a jj repository when the server has jj:
// `jj log` of the repository as jj draws it, graph and colours included,
// with its changes selectable, and `jj show` of the selected one. jj runs
// on a cache copy of the repository (pkg/jjview), never on it.
type Changes struct {
	common  common.Common
	viewer  *jjview.Viewer
	repo    proto.Repository
	lines   []jjview.Line
	nodes   []int
	cursor  int
	offset  int
	showing string
	vp      *viewport.Viewport
	loading bool
	// stale is set for a repository not listed yet: the log is run when
	// the tab is first shown, not when the repository is opened.
	stale   bool
	err     error
	spinner spinner.Model
	keys    changesKeys
}

type changesKeys struct {
	Show, Back, Refresh key.Binding
}

// NewChanges returns the Changes tab.
func NewChanges(c common.Common, viewer *jjview.Viewer) *Changes {
	return &Changes{
		common:  c,
		viewer:  viewer,
		vp:      viewport.New(c),
		spinner: spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(c.Styles.Spinner)),
		keys: changesKeys{
			Show:    key.NewBinding(key.WithKeys("enter", "l", "right"), key.WithHelp("enter", "show change")),
			Back:    key.NewBinding(key.WithKeys("h", "left", "backspace"), key.WithHelp("h", "back")),
			Refresh: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		},
	}
}

// TabName implements common.TabComponent.
func (c *Changes) TabName() string { return "Changes" }

// Path implements common.TabComponent.
func (c *Changes) Path() string {
	if c.showing != "" {
		return "change"
	}
	return ""
}

// SetSize implements common.Component.
func (c *Changes) SetSize(width, height int) {
	c.common.SetSize(width, height)
	c.vp.SetSize(width, height)
}

// SpinnerID implements common.TabComponent.
func (c *Changes) SpinnerID() int { return c.spinner.ID() }

// Init implements tea.Model.
func (c *Changes) Init() tea.Cmd { return nil }

// load runs jj log for the repository.
func (c *Changes) load() tea.Cmd {
	if c.repo == nil {
		return nil
	}
	c.loading, c.err = true, nil
	r, viewer, ctx := c.repo, c.viewer, c.common.Context()
	return tea.Batch(c.spinner.Tick, func() tea.Msg {
		gr, err := r.Open()
		if err != nil {
			return changesLogMsg{repo: r.Name(), err: err}
		}
		lines, err := viewer.Log(ctx, gr.Path, jjview.LogOptions{Limit: changesLimit, Color: true})
		return changesLogMsg{repo: r.Name(), lines: lines, err: err}
	})
}

func (c *Changes) show(change string) tea.Cmd {
	r, viewer, ctx := c.repo, c.viewer, c.common.Context()
	c.loading = true
	return func() tea.Msg {
		gr, err := r.Open()
		if err != nil {
			return changesShowMsg{change: change, err: err}
		}
		out, err := viewer.Show(ctx, gr.Path, change, true)
		return changesShowMsg{change: change, out: out, err: err}
	}
}

// selected returns the change under the cursor.
func (c *Changes) selected() string {
	if c.cursor < 0 || c.cursor >= len(c.nodes) {
		return ""
	}
	return c.lines[c.nodes[c.cursor]].Change
}

// Update implements tea.Model.
func (c *Changes) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case RepoMsg:
		c.repo = msg
		c.lines, c.nodes, c.cursor, c.offset, c.showing = nil, nil, 0, 0, ""
		c.stale = true
		return c, nil
	case tabs.ActiveTabMsg, tabs.SelectTabMsg:
		// Shown: list the changes once per repository.
		if c.stale {
			c.stale = false
			return c, c.load()
		}
		return c, nil
	case changesLogMsg:
		if c.repo == nil || msg.repo != c.repo.Name() {
			return c, nil
		}
		c.loading, c.err = false, msg.err
		c.lines, c.nodes = msg.lines, nil
		for i, l := range c.lines {
			if l.Change != "" {
				c.nodes = append(c.nodes, i)
			}
		}
		c.cursor = max(min(c.cursor, len(c.nodes)-1), 0)
		return c, nil
	case changesShowMsg:
		c.loading = false
		if msg.err != nil {
			c.err = msg.err
			return c, nil
		}
		c.showing = msg.change
		c.vp.SetContent(msg.out)
		c.vp.GotoTop()
		return c, nil
	case spinner.TickMsg:
		if c.loading && msg.ID == c.spinner.ID() {
			var cmd tea.Cmd
			c.spinner, cmd = c.spinner.Update(msg)
			return c, cmd
		}
	case tea.KeyPressMsg:
		return c, c.onKey(msg)
	}
	if c.showing != "" {
		_, cmd := c.vp.Update(msg)
		return c, cmd
	}
	return c, nil
}

func (c *Changes) onKey(msg tea.KeyPressMsg) tea.Cmd {
	km := c.common.KeyMap
	if c.showing != "" {
		if key.Matches(msg, c.keys.Back) {
			c.showing = ""
			return nil
		}
		_, cmd := c.vp.Update(msg)
		return cmd
	}
	switch {
	case key.Matches(msg, c.keys.Refresh):
		return c.load()
	case key.Matches(msg, km.Up):
		c.cursor = max(c.cursor-1, 0)
	case key.Matches(msg, km.Down):
		c.cursor = min(c.cursor+1, max(len(c.nodes)-1, 0))
	case key.Matches(msg, km.GotoTop):
		c.cursor = 0
	case key.Matches(msg, km.GotoBottom):
		c.cursor = max(len(c.nodes)-1, 0)
	case key.Matches(msg, c.keys.Show):
		if ch := c.selected(); ch != "" {
			return c.show(ch)
		}
	}
	return nil
}

// View implements tea.Model.
func (c *Changes) View() string {
	st := c.common.Styles
	h := max(c.common.Height, 1)
	if c.showing != "" {
		return c.vp.View()
	}
	var body string
	switch {
	case c.err != nil:
		body = " " + c.common.NoticeView(c.err.Error(), true, c.common.Width-1)
	case (c.loading || c.stale) && len(c.lines) == 0:
		body = st.SpinnerContainer.Render(c.spinner.View() + " running jj log…")
	case len(c.nodes) == 0:
		body = " " + st.Prompt.Detail.Render("No changes: nothing has been pushed to a bookmark or tag yet.")
	default:
		body = c.logView(h)
	}
	return lipgloss.NewStyle().Width(c.common.Width).Height(h).MaxHeight(h).Render(body)
}

// logView draws the log's lines that fit in h, keeping the selected
// change in view, with a pointer at it.
func (c *Changes) logView(h int) string {
	at := c.nodes[c.cursor]
	end := at + 1
	for end < len(c.lines) && c.lines[end].Change == "" {
		end++ // the selected change's other lines
	}
	if at < c.offset {
		c.offset = at
	}
	if end > c.offset+h {
		c.offset = end - h
	}
	c.offset = max(min(c.offset, len(c.lines)-h), 0)
	pointer := c.common.Styles.Prompt.Pointer.Render("▸") + " "
	var b strings.Builder
	for i := c.offset; i < len(c.lines) && i < c.offset+h; i++ {
		prefix := "  "
		if i == at {
			prefix = pointer
		}
		if i > c.offset {
			b.WriteByte('\n')
		}
		b.WriteString(ansi.Truncate(prefix+c.lines[i].Text, max(c.common.Width, 1), "…"))
	}
	return b.String()
}

// StatusBarValue implements common.TabComponent.
func (c *Changes) StatusBarValue() string {
	if c.showing != "" {
		return "change " + c.showing
	}
	if ch := c.selected(); ch != "" {
		return "change " + ch
	}
	return ""
}

// StatusBarInfo implements common.TabComponent.
func (c *Changes) StatusBarInfo() string {
	if c.showing != "" {
		return fmt.Sprintf("☰ %.f%%", c.vp.ScrollPercent()*100)
	}
	if len(c.nodes) == 0 {
		return ""
	}
	return fmt.Sprintf("%d/%d", c.cursor+1, len(c.nodes))
}

// ShortHelp implements help.KeyMap.
func (c *Changes) ShortHelp() []key.Binding {
	if c.showing != "" {
		return []key.Binding{c.common.KeyMap.UpDown, c.keys.Back}
	}
	return []key.Binding{c.common.KeyMap.UpDown, c.keys.Show, c.keys.Refresh}
}

// FullHelp implements help.KeyMap.
func (c *Changes) FullHelp() [][]key.Binding {
	km := c.common.KeyMap
	if c.showing != "" {
		return [][]key.Binding{{km.UpDown, km.NextPage, km.PrevPage}, {km.GotoTop, km.GotoBottom, c.keys.Back}}
	}
	return [][]key.Binding{{km.UpDown, km.GotoTop, km.GotoBottom}, {c.keys.Show, c.keys.Refresh}}
}

// History is the repository page's Commits tab: the git log (Log), or,
// for a jj repository when the server has jj, jj's own (Changes). Its
// TabName stays "Commits", which the page routes messages by; the tab
// shows TabNameFor.
type History struct {
	log     *Log
	changes *Changes
	viewer  *jjview.Viewer
	jj      bool
}

var _ common.TabComponent = (*History)(nil)

// NewHistory returns the Commits tab. viewer may be nil (no jj view).
func NewHistory(c common.Common, viewer *jjview.Viewer) *History {
	return &History{log: NewLog(c), changes: NewChanges(c, viewer), viewer: viewer}
}

func (h *History) active() common.TabComponent {
	if h.jj {
		return h.changes
	}
	return h.log
}

// UsesJJ reports whether a repository of vcs is shown with jj.
func (h *History) UsesJJ(vcs proto.VCS) bool {
	return vcs == proto.VCSJJ && h.viewer.Available()
}

// TabNameFor names the tab for a repository of vcs: "Changes" when jj
// shows it.
func (h *History) TabNameFor(vcs proto.VCS) string {
	if h.UsesJJ(vcs) {
		return h.changes.TabName()
	}
	return h.log.TabName()
}

// TabName implements common.TabComponent.
func (h *History) TabName() string { return h.log.TabName() }

// Update implements tea.Model.
func (h *History) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case RepoMsg:
		h.jj = h.UsesJJ(msg.Vcs())
	case tea.WindowSizeMsg:
		_, c1 := h.log.Update(msg)
		_, c2 := h.changes.Update(msg)
		return h, tea.Batch(c1, c2)
	}
	_, cmd := h.active().Update(msg)
	return h, cmd
}

// Init implements tea.Model.
func (h *History) Init() tea.Cmd { return h.active().Init() }

// View implements tea.Model.
func (h *History) View() string { return h.active().View() }

// SetSize implements common.Component.
func (h *History) SetSize(width, height int) {
	h.log.SetSize(width, height)
	h.changes.SetSize(width, height)
}

// ShortHelp implements help.KeyMap.
func (h *History) ShortHelp() []key.Binding { return h.active().ShortHelp() }

// FullHelp implements help.KeyMap.
func (h *History) FullHelp() [][]key.Binding { return h.active().FullHelp() }

// StatusBarValue implements common.TabComponent.
func (h *History) StatusBarValue() string { return h.active().StatusBarValue() }

// StatusBarInfo implements common.TabComponent.
func (h *History) StatusBarInfo() string { return h.active().StatusBarInfo() }

// SpinnerID implements common.TabComponent.
func (h *History) SpinnerID() int { return h.active().SpinnerID() }

// Path implements common.TabComponent.
func (h *History) Path() string { return h.active().Path() }
