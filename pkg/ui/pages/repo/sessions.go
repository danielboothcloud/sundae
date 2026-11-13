package repo

import (
	"fmt"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/sessions"
)

// Sessions is the repository page's Sessions tab (tui-screens.md section
// 8): the home page's Sessions tab narrowed to the repository, with the
// same rows (without the repository prefix) and keys. It shares the home
// tab's model, which follows the live updates whichever page is shown;
// each side sets the model's size and scope before using it.
type Sessions struct {
	common common.Common
	model  *sessions.Sessions
	repo   proto.Repository
}

var _ common.TabComponent = (*Sessions)(nil)

// NewSessions returns the Sessions tab showing model.
func NewSessions(c common.Common, model *sessions.Sessions) *Sessions {
	return &Sessions{common: c, model: model}
}

// bind narrows the shared model to the repository, at this tab's size.
func (s *Sessions) bind() {
	name := ""
	if s.repo != nil {
		name = s.repo.Name()
	}
	s.model.SetSize(s.common.Width, s.common.Height)
	s.model.SetScope(name)
}

// Capturing reports whether the tab takes every key: a form, a confirm
// or the filter is open. The repository page then leaves tab and esc, and
// the frame q and ?, to it.
func (s *Sessions) Capturing() bool { return s.model.Capturing() }

// SetSize implements common.Component.
func (s *Sessions) SetSize(width, height int) { s.common.SetSize(width, height) }

// Init implements tea.Model. The home page starts the model.
func (s *Sessions) Init() tea.Cmd { return nil }

// Update implements tea.Model. The model's own messages reach it through
// the home page; this tab passes it the keys.
func (s *Sessions) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case RepoMsg:
		s.repo = msg
	case tea.KeyPressMsg, tea.PasteMsg:
		s.bind()
		_, cmd := s.model.Update(msg)
		return s, cmd
	}
	return s, nil
}

// View implements tea.Model.
func (s *Sessions) View() string {
	s.bind()
	return s.model.View()
}

// ShortHelp implements help.KeyMap.
func (s *Sessions) ShortHelp() []key.Binding {
	s.bind()
	return s.model.ShortHelp()
}

// FullHelp implements help.KeyMap.
func (s *Sessions) FullHelp() [][]key.Binding {
	s.bind()
	return s.model.FullHelp()
}

// StatusBarValue implements common.TabComponent: the selected session.
func (s *Sessions) StatusBarValue() string {
	s.bind()
	name, _, _ := s.model.SelectedName()
	if name == "" {
		return "no sessions"
	}
	return name
}

// StatusBarInfo implements common.TabComponent: "1 of 2".
func (s *Sessions) StatusBarInfo() string {
	s.bind()
	_, i, n := s.model.SelectedName()
	return fmt.Sprintf("%d of %d", i, n)
}

// SpinnerID implements common.TabComponent; the tab has no spinner.
func (s *Sessions) SpinnerID() int { return -1 }

// TabName implements common.TabComponent.
func (s *Sessions) TabName() string { return "Sessions" }

// Path implements common.TabComponent. It is not empty while esc has
// something to do in the tab (close a form or confirm, clear a filter,
// cancel a start, leave the signal history), so esc does not leave the repository.
func (s *Sessions) Path() string {
	s.bind()
	if s.model.Capturing() || s.model.Busy() || s.model.Filtered() || s.model.History() {
		return "sessions"
	}
	return ""
}
