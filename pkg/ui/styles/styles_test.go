package styles

import (
	"testing"

	"charm.land/lipgloss/v2"
)

// TestPromptStylesComeFromTheSheet checks the prompt styles: the title and
// the focused field reuse the repository list's colours, errors take the
// error title's colour, and none of them borrows a session state's colour,
// which only agent states use.
func TestPromptStylesComeFromTheSheet(t *testing.T) {
	s := DefaultStyles()
	same := func(name string, a, b lipgloss.Style) {
		t.Helper()
		if got, want := a.Render("x"), b.Render("x"); got != want {
			t.Errorf("%s renders %q, want %q", name, got, want)
		}
	}
	same("Prompt.Title", s.Prompt.Title, s.RepoSelector.Normal.Title)
	same("Prompt.ActiveLabel", s.Prompt.ActiveLabel, s.RepoSelector.Active.Title)
	same("Prompt.Pointer", s.Prompt.Pointer, s.RepoSelector.Active.Title)
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

// TestPromptTextIsBright checks the text of forms and confirms is bright
// enough to read: greys from the light end of the 256-colour grey ramp
// (245 to 255), never the help's dim greys.
func TestPromptTextIsBright(t *testing.T) {
	s := DefaultStyles()
	for name, st := range map[string]lipgloss.Style{
		"Label": s.Prompt.Label, "Value": s.Prompt.Value, "Detail": s.Prompt.Detail,
		"Question": s.Prompt.Question, "Notice": s.Prompt.Notice,
	} {
		c, ok := st.GetForeground().(lipgloss.ANSIColor)
		if !ok || c < 245 || c > 255 {
			t.Errorf("Prompt.%s foreground %v is not a bright grey", name, st.GetForeground())
		}
	}
}
