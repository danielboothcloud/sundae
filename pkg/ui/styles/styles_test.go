package styles

import (
	"testing"

	"charm.land/lipgloss/v2"
)

// TestPromptStylesComeFromTheSheet checks the prompt styles reuse the
// sheet's colours: questions and notices read like the help, errors take
// the error title's colour, and none of them borrows a session state's
// colour, which only agent states use.
func TestPromptStylesComeFromTheSheet(t *testing.T) {
	s := DefaultStyles()
	same := func(name string, a, b lipgloss.Style) {
		t.Helper()
		if got, want := a.Render("x"), b.Render("x"); got != want {
			t.Errorf("%s renders %q, want %q", name, got, want)
		}
	}
	same("Prompt.Question", s.Prompt.Question, s.HelpKey)
	same("Prompt.Notice", s.Prompt.Notice, s.HelpKey)
	same("Prompt.Title", s.Prompt.Title, s.RepoSelector.Normal.Title)
	same("Prompt.Label", s.Prompt.Label, s.RepoSelector.Normal.Desc)
	same("Prompt.ActiveLabel", s.Prompt.ActiveLabel, s.RepoSelector.Active.Title)
	same("Prompt.Detail", s.Prompt.Detail, s.RepoSelector.Normal.Desc)
	same("Prompt.Error", s.Prompt.Error, lipgloss.NewStyle().Foreground(s.ErrorTitle.GetBackground()))

	prompt := map[string]lipgloss.Style{
		"Question": s.Prompt.Question, "Notice": s.Prompt.Notice, "Error": s.Prompt.Error,
		"Label": s.Prompt.Label, "ActiveLabel": s.Prompt.ActiveLabel, "Detail": s.Prompt.Detail,
	}
	state := map[string]lipgloss.Style{
		"Blocked": s.State.Blocked, "Error": s.State.Error, "Done": s.State.Done, "Working": s.State.Working,
	}
	for pn, p := range prompt {
		for sn, st := range state {
			if p.GetForeground() == st.GetForeground() {
				t.Errorf("Prompt.%s has State.%s's colour", pn, sn)
			}
		}
	}
}
