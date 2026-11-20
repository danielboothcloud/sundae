package selection

import (
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/actionmenu"
	"github.com/danielboothcloud/sundae/pkg/ui/components/footer"
)

// rowActions are the keys that act on the active tab's selected row, for
// its action menu: the tab's own bindings, only those enabled now. None
// while a form, confirm or filter has the keys.
func (s *Selection) rowActions() []key.Binding {
	if s.menu == nil && s.palette == nil && s.Capturing() {
		return nil
	}
	return s.paneActions(s.activePane)
}

// paneActions are the keys that act on tab p's selected row, whether or
// not it is the active tab.
func (s *Selection) paneActions(p pane) []key.Binding {
	switch p {
	case selectorPane:
		if s.FilterState() == list.Filtering || s.repoForm != nil || s.deleting != nil || s.creating != "" {
			return nil
		}
		repo := s.SelectedRepo()
		if repo == "" {
			return nil
		}
		open := s.common.KeyMap.Select
		open.SetHelp("enter", "open")
		copyKey := s.common.KeyMap.Copy
		copyKey.SetHelp("c", "copy command")
		as := []key.Binding{open}
		if s.sessions != nil {
			as = append(as, s.newSession)
		}
		as = append(as, copyKey)
		if s.canDelete(repo) {
			as = append(as, s.deleteRepo)
		}
		return as
	case sessionsPane:
		if s.sessions != nil {
			s.bindSessions()
			return s.sessions.Actions()
		}
	case forgesPane:
		if s.forges != nil {
			return s.forges.Actions()
		}
	}
	return nil
}

// rowTitle names the selected row for the action menu.
func (s *Selection) rowTitle() string {
	switch s.activePane {
	case selectorPane:
		return s.SelectedRepo()
	case sessionsPane:
		return s.sessions.ActionsTitle()
	case forgesPane:
		return s.forges.ActionsTitle()
	}
	return ""
}

// openMenu opens the selected row's action menu, when it has actions.
func (s *Selection) openMenu() {
	as := s.rowActions()
	if len(as) == 0 {
		return
	}
	s.repoNotice = notice{}
	s.menu = actionmenu.New(s.common, s.rowTitle(), as)
}

// onMenuKey handles a key while the action menu is open. A chosen action
// closes the menu and re-feeds its key to the tab, so it runs exactly as
// pressing the key does (a confirm still asks first).
func (s *Selection) onMenuKey(msg tea.KeyPressMsg) tea.Cmd {
	res, b := s.menu.Update(msg)
	switch res {
	case actionmenu.Closed:
		s.menu = nil
	case actionmenu.Run:
		s.menu = nil
		_, cmd := s.Update(actionmenu.KeyPress(b))
		return cmd
	}
	return nil
}

// MenuActions is the open action menu's actions; nil when it is closed.
func (s *Selection) MenuActions() []key.Binding {
	if s.menu == nil {
		return nil
	}
	return s.menu.Actions()
}

// moreActions is the action bar's last entry: the action menu with every
// action of the row.
var moreActions = key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "more"))

// ActionBar is the line above the help on the home page: the selected
// row's name, then what can be done with it, each key coloured by its kind
// (common.KindOf), ending with the key of the action menu. The row's main
// action (enter) is in the help, so it is left out here. "" when nothing
// can be acted on: no row, or a menu, palette, form, confirm or filter is
// open.
func (s *Selection) ActionBar(width int) string {
	if s.menu != nil || s.palette != nil || s.Capturing() {
		return ""
	}
	var as []key.Binding
	for _, a := range s.rowActions() {
		if ks := a.Keys(); len(ks) > 0 && ks[0] != "enter" {
			as = append(as, a)
		}
	}
	title := s.rowTitle()
	if len(as) == 0 || title == "" {
		return ""
	}
	ps := s.common.Styles.Prompt
	head := ps.Pointer.Render("▸") + " " + ps.ActiveLabel.Render(common.TruncateString(title, max(width/3, 8))) + "  "
	more := footer.ShortView(s.common, []key.Binding{moreActions}, 0)
	room := width - lipgloss.Width(head) - lipgloss.Width(more) - 3
	if room < 8 {
		return common.TruncateString(head+more, max(width, 1))
	}
	body := footer.ShortView(s.common, as, room)
	return head + body + s.common.Styles.HelpDivider.Render() + more
}
