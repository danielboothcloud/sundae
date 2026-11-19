package common

import "strings"

// doneVerbs are the first words of notices that report a finished action.
var doneVerbs = map[string]bool{
	"started": true, "suspended": true, "resumed": true,
	"added": true, "removed": true, "imported": true, "created": true,
	"deleted": true, "copied": true, "synced": true, "pushed": true,
	"detached": true, "pruned": true,
}

// NoticeView renders a one-line result notice, at most width cells: an
// error with ✗ in the error colour; a finished action ("Started …",
// "Deleted …") with ✓ in the success colour; anything else as plain
// notice text. A notice that already starts with ✗ or ✓ keeps it.
func (c *Common) NoticeView(text string, isErr bool, width int) string {
	ps := c.Styles.Prompt
	style, glyph := ps.Notice, ""
	first, _, _ := strings.Cut(strings.ToLower(text), " ")
	switch {
	case isErr || strings.HasPrefix(text, "✗"):
		style, glyph = ps.Error, "✗ "
	case strings.HasPrefix(text, "✓") || doneVerbs[strings.TrimRight(first, ".,:")]:
		style, glyph = ps.Success, "✓ "
	}
	if strings.HasPrefix(text, "✗") || strings.HasPrefix(text, "✓") {
		glyph = ""
	}
	return style.Render(TruncateString(glyph+text, max(width, 1)))
}
