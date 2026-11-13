package selection

import (
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/danielboothcloud/sundae/pkg/ui/components/actionmenu"
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
	s.repoNotice = ""
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
