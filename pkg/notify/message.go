package notify

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/events"
)

// Length caps, in bytes. ntfy limits titles to 1 KB, all tags to 512 bytes
// and messages to 4 KB (longer ones become attachments). OSC 7501 allows a
// 192-byte title and a 2048-byte msg; a phone shows far less.
const (
	maxTitle = 192
	maxBody  = 1024
	maxLine  = 512
	maxTag   = 32
	maxTags  = 4
	maxClick = 2048
)

// ntfy priorities.
const (
	priorityLow     = 2
	priorityDefault = 3
	priorityHigh    = 4
)

// Message is one notification, ready to publish. Every field is plain,
// sanitised text.
type Message struct {
	Title    string
	Body     string
	Priority int
	Tags     []string
	// Click is the URL opened when the notification is tapped; may be
	// empty.
	Click string
}

// sanitize makes untrusted text safe to show on a phone: invalid UTF-8 is
// replaced, control characters (C0, DEL, C1) and invisible formatting
// characters (bidi overrides, zero-width characters) are removed or turned
// into spaces, runs of whitespace collapse to one space, and the result is
// capped at max bytes on a rune boundary, marked with an ellipsis.
func sanitize(s string, max int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	return truncate(b.String(), max)
}

// truncate caps s at max bytes on a rune boundary, ending in "…" when cut.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "…"
	cut := max - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRightFunc(s[:cut], unicode.IsSpace) + ellipsis
}

// tagRe matches a tag that is safe to pass to ntfy: the OSC 7501 app
// character set.
var tagRe = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,32}$`)

func tags(candidates ...string) []string {
	var out []string
	for _, t := range candidates {
		if len(out) == maxTags {
			break
		}
		if t != "" && len(t) <= maxTag && tagRe.MatchString(t) {
			out = append(out, t)
		}
	}
	return out
}

// sessionLabel is "repo / session" as shown in titles, with a child record's
// id after the session name.
func sessionLabel(s events.Session, recordID string) string {
	label := sanitize(s.Repo, maxLine) + " / " + sanitize(s.DisplayName(), maxLine)
	if recordID != "" {
		label += "/" + sanitize(recordID, maxLine)
	}
	return label
}

// blockedPhrase says what a blocked agent waits for.
func blockedPhrase(k events.BlockKind) string {
	switch k {
	case events.BlockPermission:
		return "Needs permission"
	case events.BlockQuestion:
		return "Has a question"
	case events.BlockAuth:
		return "Needs a login"
	default:
		return ""
	}
}

// statusMessage renders a session status notification.
//
//	title: ▲ blocked · infra / claude-1
//	body:  Needs permission: Apply 3 to add, 1 to change?
//	       claude-code · 40%
func statusMessage(st events.SessionStatus, nc config.NtfyConfig) Message {
	rec := st.Record
	title := fmt.Sprintf("%s %s · %s", rec.State.Glyph(), rec.State, sessionLabel(st.Session, rec.ID))

	main := sanitize(rec.Msg, maxLine)
	if main == "" {
		main = sanitize(rec.Title, maxLine)
	}
	if rec.State == events.StateBlocked {
		if phrase := blockedPhrase(rec.Kind); phrase != "" {
			if main == "" {
				main = phrase
			} else {
				main = phrase + ": " + main
			}
		}
	}
	var detail []string
	if app := sanitize(rec.App, maxTag); app != "" {
		detail = append(detail, app)
	}
	if rec.Progress != nil && (rec.State == events.StateWorking || rec.State == events.StateBlocked) {
		if p := *rec.Progress; p >= 0 && p <= 100 {
			detail = append(detail, strconv.Itoa(p)+"%")
		}
	}
	if main == "" {
		if len(detail) > 0 {
			main, detail = detail[0], detail[1:]
		} else {
			main = "Agent is " + string(rec.State)
		}
	}
	body := main
	if len(detail) > 0 {
		body += "\n" + strings.Join(detail, " · ")
	}

	priority := priorityDefault
	switch rec.State {
	case events.StateBlocked:
		priority = priorityHigh
	case events.StateWorking, events.StateIdle:
		priority = priorityLow
	}

	return Message{
		Title:    truncate(title, maxTitle),
		Body:     truncate(body, maxBody),
		Priority: priority,
		Tags:     tags(string(rec.State), string(rec.Kind), rec.App),
		Click:    clickURL(nc, st.Session.Repo, st.Session.DisplayName(), st.Session.Sandbox, st.Session.User),
	}
}

// lifecycleFailedMessage renders a failed session start or end.
func lifecycleFailedMessage(l events.SessionLifecycle, nc config.NtfyConfig) Message {
	title := fmt.Sprintf("%s session failed · %s", events.StateError.Glyph(), sessionLabel(l.Session, ""))
	body := sanitize(l.Reason, maxBody)
	if op := sanitize(l.Op, maxTag); op != "" {
		if body == "" {
			body = "Could not " + op + " the session"
		} else {
			body = "Could not " + op + " the session: " + body
		}
	}
	if body == "" {
		body = "Session failed"
	}
	return Message{
		Title:    truncate(title, maxTitle),
		Body:     truncate(body, maxBody),
		Priority: priorityDefault,
		Tags:     tags("session", "failed", l.Op),
		Click:    clickURL(nc, l.Session.Repo, l.Session.DisplayName(), l.Session.Sandbox, l.Session.User),
	}
}

// forgeFailedMessage renders a failed forge operation.
func forgeFailedMessage(f events.ForgeOutcome, nc config.NtfyConfig) Message {
	title := fmt.Sprintf("%s forge %s failed · %s", events.StateError.Glyph(), sanitize(f.Kind, maxTag), sanitize(f.Forge, maxLine))
	if f.Repo != "" {
		title += " · " + sanitize(f.Repo, maxLine)
	}
	body := sanitize(f.Error, maxBody)
	if body == "" {
		body = "Forge " + sanitize(f.Kind, maxTag) + " failed"
	}
	return Message{
		Title:    truncate(title, maxTitle),
		Body:     truncate(body, maxBody),
		Priority: priorityDefault,
		Tags:     tags("forge", f.Kind, f.Forge),
		Click:    clickURL(nc, f.Repo, "", "", ""),
	}
}

// clickURL expands the configured click URL, dropping it when it is too
// long.
func clickURL(nc config.NtfyConfig, repo, session, sandbox, user string) string {
	u := nc.ExpandClickURL(repo, session, sandbox, user)
	if len(u) > maxClick {
		return ""
	}
	return u
}

// classifier turns bus events into debouncer observations. It remembers the
// last observation of every key so that only changes count: a status that
// is published again unchanged, or a forge sync that fails the same way
// every interval, notifies once. Like the debouncer, it belongs to the
// notifier's loop.
type classifier struct {
	nc   config.NotificationsConfig
	last map[string]string
}

func newClassifier(nc config.NotificationsConfig) *classifier {
	return &classifier{nc: nc, last: map[string]string{}}
}

const sep = "\x00"

func sessionKey(prefix string, userID int64, s events.Session) string {
	return prefix + sep + strconv.FormatInt(userID, 10) + sep + s.Sandbox
}

// changed records sig for key and reports whether it differs from the last.
func (c *classifier) changed(key, sig string) bool {
	if old, ok := c.last[key]; ok && old == sig {
		return false
	}
	c.last[key] = sig
	return true
}

// classify returns the observation an event makes, or false when the event
// changes nothing a notification is about.
func (c *classifier) classify(ev events.Event) (note, bool) {
	switch {
	case ev.Status != nil:
		st := *ev.Status
		rec := st.Record
		key := sessionKey("status", ev.UserID, st.Session) + sep + rec.ID
		sig := string(rec.State) + sep + string(rec.Kind) + sep + rec.Msg
		if !c.changed(key, sig) {
			return note{}, false
		}
		n := note{key: key, sig: sig}
		if rec.State != events.StateNone && c.nc.Notifies(string(rec.State)) {
			n.trigger = true
			n.msg = statusMessage(st, c.nc.Ntfy)
		}
		return n, true

	case ev.Lifecycle != nil:
		l := *ev.Lifecycle
		key := sessionKey("lifecycle", ev.UserID, l.Session)
		if l.Step == events.LifecycleEnded {
			// The session is gone: forget it, so a new session of the
			// same name starts afresh.
			status := sessionKey("status", ev.UserID, l.Session) + sep
			for k := range c.last {
				if k == key || strings.HasPrefix(k, status) {
					delete(c.last, k)
				}
			}
			return note{key: key, sig: string(l.Step)}, true
		}
		sig := string(l.Step) + sep + l.Op + sep + l.Reason
		if !c.changed(key, sig) {
			return note{}, false
		}
		n := note{key: key, sig: sig}
		if l.Step == events.LifecycleFailed && c.nc.Notifies(config.TriggerSessionFailed) {
			n.trigger = true
			n.msg = lifecycleFailedMessage(l, c.nc.Ntfy)
		}
		return n, true

	case ev.Forge != nil:
		f := *ev.Forge
		key := "forge" + sep + f.Forge + sep + f.Kind + sep + f.Repo
		sig := "ok"
		if !f.OK {
			sig = "error" + sep + f.Error
		}
		if !c.changed(key, sig) {
			return note{}, false
		}
		n := note{key: key, sig: sig}
		if !f.OK && c.nc.Notifies(config.TriggerForgeFailed) {
			n.trigger = true
			n.msg = forgeFailedMessage(f, c.nc.Ntfy)
		}
		return n, true
	}
	return note{}, false
}
