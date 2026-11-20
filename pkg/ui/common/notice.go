package common

import (
	"regexp"
	"strings"
)

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

// NoticeWidth is the most cells a message takes with a typical label, so
// it fits a 50-column terminal after the frame's margins.
const NoticeWidth = 44

// Messenger is a page or tab with a message for the frame's message slot,
// the line above the help; "" for none.
type Messenger interface {
	Message() (string, MessageKind)
}

// NoticeView renders a message on one line of at most width cells, cut with
// "…", so its mark (✗, ✓, trailing " …") and colour match on every page.
func (c *Common) NoticeView(text string, kind MessageKind, width int) string {
	ps := c.Styles.Prompt
	text = strings.Join(strings.Fields(text), " ")
	style := ps.Notice
	switch kind {
	case MessageError:
		style, text = ps.Error, "✗ "+text
	case MessageSuccess:
		style, text = ps.Success, "✓ "+text
	case MessageBusy:
		text = Busy(strings.TrimSuffix(text, "…"))
	case MessageInfo:
	}
	return style.Render(TruncateString(text, max(width, 1)))
}

// Busy marks text as still running, as every busy word and message is
// marked: "importing …".
func Busy(text string) string { return strings.TrimSpace(text) + " …" }

// requestPrefix is the `Get "https://…": ` or `GET /path: ` an HTTP error
// starts with.
var requestPrefix = regexp.MustCompile(`(?:GET|POST|HEAD|Get|Post|Head) (?:"[^"]*"|\S+): `)

// transportErrors map the end of a network error to a few words; the
// address and call chain before it only matter in the log.
var transportErrors = []struct{ match, short string }{
	{"connection refused", "connection refused"},
	{"connection reset", "connection reset"},
	{"no such host", "host not found"},
	{"i/o timeout", "timed out"},
	{"deadline exceeded", "timed out"},
	{"Client.Timeout", "timed out"},
	{"no route to host", "unreachable"},
	{"network is unreachable", "unreachable"},
}

// ShortError shortens an error for a message: one line, without the request
// it made, and a transport failure as a few words. Log the full error.
func ShortError(err error) string {
	if err == nil {
		return ""
	}
	return ShortErrorText(err.Error())
}

// ShortErrorText is ShortError of an error's text.
func ShortErrorText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for _, t := range transportErrors {
		if strings.Contains(s, t.match) {
			return t.short
		}
	}
	return requestPrefix.ReplaceAllString(s, "")
}
