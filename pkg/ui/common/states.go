package common

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// StateLines renders a tab's empty or error state at the top of its body:
// text wrapped to width with a one-cell indent, so wrapped lines align.
func StateLines(style lipgloss.Style, text string, width int) []string {
	wrapped := ansi.Wrap(strings.Join(strings.Fields(text), " "), max(width-1, 1), "")
	lines := strings.Split(wrapped, "\n")
	for i, l := range lines {
		lines[i] = " " + style.Render(l)
	}
	return lines
}

// ErrorState renders what failed, with the key that retries ("r retry"),
// then the short cause under it; the caller logs the whole error.
func ErrorState(c Common, what string, err error, hint string, width int) []string {
	st := c.Styles
	head := "✗ " + what
	if hint != "" {
		head += " · " + hint
	}
	lines := StateLines(st.Prompt.Error, head, width)
	if cause := ShortError(err); cause != "" {
		for _, l := range StateLines(st.RepoSelector.Normal.Desc, cause, width-2) {
			lines = append(lines, "  "+l)
		}
	}
	return lines
}
