package tabs

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// SelectTabMsg is a message that contains the index of the tab to select.
type SelectTabMsg int

// ActiveTabMsg is a message that contains the index of the current active tab.
type ActiveTabMsg int

// Tabs is bubbletea component that displays a list of tabs.
type Tabs struct {
	common       common.Common
	tabs         []string
	activeTab    int
	TabSeparator lipgloss.Style
	TabInactive  lipgloss.Style
	TabActive    lipgloss.Style
	TabDot       lipgloss.Style
	UseDot       bool
	// CollapseBelow, when set, shows only the active tab, as "‹ name ›",
	// when narrower than this many columns.
	CollapseBelow int
}

// New creates a new Tabs component.
func New(c common.Common, tabs []string) *Tabs {
	r := &Tabs{
		common:       c,
		tabs:         tabs,
		activeTab:    0,
		TabSeparator: c.Styles.TabSeparator,
		TabInactive:  c.Styles.TabInactive,
		TabActive:    c.Styles.TabActive,
	}
	return r
}

// SetSize implements common.Component.
func (t *Tabs) SetSize(width, height int) {
	t.common.SetSize(width, height)
}

// SetTabs replaces the tab names, keeping the active tab when still valid.
func (t *Tabs) SetTabs(tabs []string) {
	t.tabs = tabs
	if t.activeTab >= len(tabs) {
		t.activeTab = 0
	}
}

// SetActiveTab makes the tab at index i active.
func (t *Tabs) SetActiveTab(i int) {
	if i >= 0 && i < len(t.tabs) {
		t.activeTab = i
	}
}

// ActiveTab returns the index of the active tab.
func (t *Tabs) ActiveTab() int { return t.activeTab }

// Init implements tea.Model.
func (t *Tabs) Init() tea.Cmd {
	t.activeTab = 0
	return nil
}

// Update implements tea.Model.
func (t *Tabs) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	cmds := make([]tea.Cmd, 0)
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "tab":
			t.activeTab = (t.activeTab + 1) % len(t.tabs)
			cmds = append(cmds, t.activeTabCmd)
		case "shift+tab":
			t.activeTab = (t.activeTab - 1 + len(t.tabs)) % len(t.tabs)
			cmds = append(cmds, t.activeTabCmd)
		}
	case tea.MouseClickMsg:
		switch msg.Button {
		case tea.MouseLeft:
			for i, tab := range t.tabs {
				if t.common.Zone.Get(tab).InBounds(msg) {
					t.activeTab = i
					cmds = append(cmds, t.activeTabCmd)
				}
			}
		}
	case SelectTabMsg:
		tab := int(msg)
		if tab >= 0 && tab < len(t.tabs) {
			t.activeTab = int(msg)
		}
	}
	return t, tea.Batch(cmds...)
}

// View implements tea.Model.
func (t *Tabs) View() string {
	if t.CollapseBelow > 0 && t.common.Width < t.CollapseBelow && t.activeTab < len(t.tabs) {
		tab := t.tabs[t.activeTab]
		return t.common.Zone.Mark(tab, t.TabActive.Render("‹ "+tab+" ›"))
	}
	s := strings.Builder{}
	sep := t.TabSeparator
	for i, tab := range t.tabs {
		style := t.TabInactive
		prefix := "  "
		if i == t.activeTab {
			style = t.TabActive
			prefix = t.TabDot.Render("• ")
		}
		if t.UseDot {
			s.WriteString(prefix)
		}
		s.WriteString(
			t.common.Zone.Mark(
				tab,
				style.Render(tab),
			),
		)
		if i != len(t.tabs)-1 {
			s.WriteString(sep.String())
		}
	}
	return lipgloss.NewStyle().
		MaxWidth(t.common.Width).
		Render(s.String())
}

func (t *Tabs) activeTabCmd() tea.Msg {
	return ActiveTabMsg(t.activeTab)
}

// SelectTabCmd is a bubbletea command that selects the tab at the given index.
func SelectTabCmd(tab int) tea.Cmd {
	return func() tea.Msg {
		return SelectTabMsg(tab)
	}
}
