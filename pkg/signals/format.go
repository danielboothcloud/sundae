package signals

import (
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/soft-serve/pkg/events"
)

// The status vocabulary shared by `session ls`, `session signals` and the
// TUI: a glyph never appears without its word (events.State.Glyph and
// Word), and the sandbox lifecycle is a second word after the state.

// Row markers of lifecycle signals that are not about the program's state.
// They mark feed rows only; they are not session states.
const (
	GlyphCreated = "+"
	GlyphEnded   = "-"
	GlyphGap     = "!"
)

// Sanitize makes untrusted program output safe to print: invalid UTF-8 is
// replaced, control characters (C0, DEL, C1) and invisible formatting
// characters (bidi overrides, zero-width characters) are removed, runs of
// whitespace become one space, and the result is cut to max characters,
// ending in "…" when cut. max <= 0 does not cut.
func Sanitize(s string, max int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	var b strings.Builder
	space := false
	n := 0
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			continue
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
		}
		b.WriteRune(r)
		n++
	}
	out := b.String()
	if max > 0 && n > max {
		runes := []rune(out)
		out = strings.TrimRightFunc(string(runes[:max-1]), unicode.IsSpace) + "…"
	}
	return out
}

// RecordLabel renders an OSC 7501 record's state: glyph and word, the
// progress of a working or blocked program, and what a blocked one waits
// for, e.g. "▲ blocked · permission" or "● working 40%".
func RecordLabel(rec events.Record) string {
	return rec.State.Glyph() + " " + RecordWords(rec)
}

// RecordWords is RecordLabel without the glyph, for where the glyph is
// shown apart from the words: "blocked · permission", "working 40%".
func RecordWords(rec events.Record) string {
	label := rec.State.Word()
	if rec.Progress != nil && (rec.State == events.StateWorking || rec.State == events.StateBlocked) {
		label += " " + strconv.Itoa(*rec.Progress) + "%"
	}
	if rec.State == events.StateBlocked && rec.Kind != "" {
		label += " · " + Sanitize(string(rec.Kind), 16)
	}
	return label
}

// RecordMessage is the line a record shows: its message, else its title,
// else the program's name; sanitised and cut to max characters (max <= 0
// does not cut).
func RecordMessage(rec events.Record, max int) string {
	for _, s := range []string{rec.Msg, rec.Title, rec.App} {
		if t := Sanitize(s, max); t != "" {
			return t
		}
	}
	return ""
}

// State is the state a signal's row shows: a status signal's record state;
// error for a failed lifecycle step; none for a gap marker and for created
// and ended; for any other lifecycle step, the session's last known state
// (Last).
func (s Signal) State() events.State {
	switch {
	case s.Status != nil:
		return s.Status.Record.State
	case s.Lifecycle != nil:
		switch s.Lifecycle.Step {
		case events.LifecycleFailed:
			return events.StateError
		case events.LifecycleCreated, events.LifecycleEnded:
			return events.StateNone
		}
		return s.Last
	}
	return events.StateNone
}

// NeedsAttention reports whether the signal asks for the user: a status
// report of blocked, error or done, a failed lifecycle step, or a gap
// marker (what was lost may have asked).
func (s Signal) NeedsAttention() bool {
	switch {
	case s.IsGap():
		return true
	case s.Status != nil:
		return s.Status.Record.State.NeedsAttention()
	case s.Lifecycle != nil:
		return s.Lifecycle.Step == events.LifecycleFailed
	}
	return false
}

// Label renders the signal's state column: glyph and word, then kind and
// progress for a status report, the lifecycle step for a lifecycle event,
// e.g. "▲ blocked · permission", "● working 40%", "○ idle · parked",
// "+ created", "✗ failed · start", "! missed".
func (s Signal) Label() string {
	switch {
	case s.IsGap():
		return GlyphGap + " missed"
	case s.Status != nil:
		return RecordLabel(s.Status.Record)
	case s.Lifecycle != nil:
		l := s.Lifecycle
		switch l.Step {
		case events.LifecycleCreated:
			return GlyphCreated + " created"
		case events.LifecycleEnded:
			return GlyphEnded + " ended"
		case events.LifecycleFailed:
			label := events.StateError.Glyph() + " failed"
			if op := Sanitize(l.Op, 16); op != "" {
				label += " · " + op
			}
			return label
		}
		return s.Last.Glyph() + " " + s.Last.Word() + " · " + Sanitize(string(l.Step), 16)
	}
	return events.StateNone.Glyph() + " " + events.StateNone.Word()
}

// Where renders the session a signal is about, "repo / session", with a
// child record's id after the session name ("repo / session/lint");
// sanitised, not cut. "" for a gap marker.
func (s Signal) Where() string {
	sess, ok := s.session()
	if !ok {
		return ""
	}
	w := Sanitize(sess.Repo, 0) + " / " + Sanitize(sess.DisplayName(), 0)
	if s.Status != nil && s.Status.Record.ID != "" {
		w += "/" + Sanitize(s.Status.Record.ID, 0)
	}
	return w
}

// Message renders the signal's message, sanitised and cut to max
// characters (max <= 0 does not cut): the record's message, title or app
// for a status report, and what happened for a lifecycle step or a gap.
func (s Signal) Message(max int) string {
	var m string
	switch {
	case s.IsGap():
		m = gapMessage(*s.Gap)
	case s.Status != nil:
		m = RecordMessage(s.Status.Record, 0)
		if m == "" && s.Status.Record.State == events.StateNone {
			m = "status cleared"
		}
	case s.Lifecycle != nil:
		m = lifecycleMessage(*s.Lifecycle)
	}
	return Sanitize(m, max)
}

func gapMessage(g Gap) string {
	n := ""
	if g.Lost > 0 {
		n = strconv.FormatUint(g.Lost, 10) + " "
	}
	switch g.Reason {
	case GapDropped:
		return "signals were lost: " + n + "events arrived faster than they could be kept"
	case GapEvicted:
		return "signals were missed: " + n + "older signals were dropped from memory before they were shown"
	}
	return "signals were lost"
}

func lifecycleMessage(l events.SessionLifecycle) string {
	switch l.Step {
	case events.LifecycleCreated:
		return "sandbox created"
	case events.LifecycleResumed:
		return "sandbox reused"
	case events.LifecycleReady:
		return "ready to attach"
	case events.LifecycleParked:
		// openshell.ParkAuto and openshell.ParkManual.
		switch l.Op {
		case "auto":
			return "parked while idle"
		case "park":
			return "parked by session park"
		}
		return "parked"
	case events.LifecycleExited:
		if l.ExitCode != nil {
			return "program exited with status " + strconv.Itoa(*l.ExitCode)
		}
		return "program exited"
	case events.LifecycleEnded:
		return "sandbox deleted"
	case events.LifecycleFailed:
		m := "could not " + orWord(l.Op, "run") + " the session"
		if l.Reason != "" {
			m += ": " + l.Reason
		}
		return m
	}
	return string(l.Step)
}

func orWord(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// FormatTime renders when a signal happened: the time of day for today
// (server time), the date before that; "-" for the zero time.
func FormatTime(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	t, now = t.Local(), now.Local()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04:05")
	}
	return t.Format("Jan 02")
}
