package common

import "strings"

// MessageKind is what a message in the frame's message slot reports, for
// its mark and colour.
type MessageKind int

const (
	// MessageInfo is any other message.
	MessageInfo MessageKind = iota
	// MessageSuccess reports a finished action.
	MessageSuccess
	// MessageError reports a failure.
	MessageError
	// MessageBusy reports an action still running.
	MessageBusy
)

// Messenger is a page or tab with a message for the frame's message slot,
// the line above the help; "" for none.
type Messenger interface {
	Message() (string, MessageKind)
}

// doneVerbs are the first words of notices that report a finished action.
var doneVerbs = map[string]bool{
	"started": true, "suspended": true, "resumed": true,
	"added": true, "removed": true, "imported": true, "created": true,
	"deleted": true, "copied": true, "synced": true, "pushed": true,
	"detached": true, "pruned": true,
}

// NoticeKind returns the kind of a notice text: an error when isErr or it
// starts with ✗, a success when it starts with ✓ or a done verb, else info.
func NoticeKind(text string, isErr bool) MessageKind {
	first, _, _ := strings.Cut(strings.ToLower(text), " ")
	switch {
	case isErr || strings.HasPrefix(text, "✗"):
		return MessageError
	case strings.HasPrefix(text, "✓") || doneVerbs[strings.TrimRight(first, ".,:")]:
		return MessageSuccess
	}
	return MessageInfo
}

// NoticeView renders a message on one line of at most width cells, cut with
// "…", so its mark (✗, ✓, trailing " …") and colour match on every page.
func (c *Common) NoticeView(text string, kind MessageKind, width int) string {
	ps := c.Styles.Prompt
	text = strings.Join(strings.Fields(text), " ")
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "✗"), "✓"))
	style := ps.Notice
	switch kind {
	case MessageError:
		style, text = ps.Error, "✗ "+text
	case MessageSuccess:
		style, text = ps.Success, "✓ "+text
	case MessageBusy:
		text = strings.TrimSpace(strings.TrimSuffix(text, "…")) + " …"
	}
	return style.Render(TruncateString(text, max(width, 1)))
}
