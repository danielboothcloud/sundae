package notify

import (
	"sort"
	"time"
)

// note is one observation of a notification key: a session record's state,
// a session's lifecycle, or a forge operation's outcome.
type note struct {
	// key groups observations that rate-limit and replace each other, e.g.
	// one record of one session.
	key string
	// sig is what makes two notifications the same (state, kind and
	// message for a status).
	sig string
	// trigger is whether this observation notifies. Observations that do
	// not (working, a cleared record, a successful sync) still replace a
	// held notification.
	trigger bool
	msg     Message
}

// slot is the debounce state of one key while its window is open.
type slot struct {
	// sent is the last notification sent for the key.
	sent note
	// until is when the window opened by that notification closes.
	until time.Time
	// pending is the latest observation since then, if any.
	pending *note
}

// debouncer rate-limits notifications per key. It is not safe for
// concurrent use; the notifier's loop owns it. Times are passed in, so it
// runs on any clock.
//
// The rules, per key, for a window w:
//   - When no window is open, a triggering observation is sent at once and
//     opens a window of w. A non-triggering one is ignored.
//   - While the window is open, observations are held; each replaces the
//     last, so a burst collapses into its latest state.
//   - When the window closes, the held observation is sent if it triggers
//     and differs from what was last sent (state, kind and message), which
//     opens a new window. Otherwise nothing is sent: a repeat is
//     suppressed, and a state that does not notify (working, or a record
//     that disappeared, such as blocked dropped at process exit) cancels
//     what was held, so blocked is not sent again.
//   - flush sends every held triggering observation at once, so the final
//     state is not lost on shutdown.
type debouncer struct {
	window time.Duration
	slots  map[string]*slot
}

func newDebouncer(window time.Duration) *debouncer {
	return &debouncer{window: window, slots: map[string]*slot{}}
}

// observe records n at now and returns what to send now.
func (d *debouncer) observe(n note, now time.Time) []note {
	out := d.closeExpired(n.key, now)
	s := d.slots[n.key]
	if s == nil {
		if !n.trigger {
			return out
		}
		return append(out, d.send(n, now))
	}
	held := n
	s.pending = &held
	return out
}

// due returns what is to be sent at now, from windows that have closed.
func (d *debouncer) due(now time.Time) []note {
	keys := make([]string, 0, len(d.slots))
	for k, s := range d.slots {
		if !now.Before(s.until) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []note
	for _, k := range keys {
		out = append(out, d.closeExpired(k, now)...)
	}
	return out
}

// next returns when the earliest open window closes.
func (d *debouncer) next() (time.Time, bool) {
	var first time.Time
	for _, s := range d.slots {
		if first.IsZero() || s.until.Before(first) {
			first = s.until
		}
	}
	return first, !first.IsZero()
}

// flush closes every window now and returns the held notifications that
// would have been sent when their window closed.
func (d *debouncer) flush() []note {
	keys := make([]string, 0, len(d.slots))
	for k := range d.slots {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []note
	for _, k := range keys {
		if p := d.slots[k].pending; p != nil && p.trigger && p.sig != d.slots[k].sent.sig {
			out = append(out, *p)
		}
		delete(d.slots, k)
	}
	return out
}

// closeExpired closes key's window if it has elapsed at now, sending what it
// held when that is due.
func (d *debouncer) closeExpired(key string, now time.Time) []note {
	s := d.slots[key]
	if s == nil || now.Before(s.until) {
		return nil
	}
	p := s.pending
	delete(d.slots, key)
	if p != nil && p.trigger && p.sig != s.sent.sig {
		return []note{d.send(*p, now)}
	}
	return nil
}

// send records n as sent at now and opens its window.
func (d *debouncer) send(n note, now time.Time) note {
	if d.window > 0 {
		d.slots[n.key] = &slot{sent: n, until: now.Add(d.window)}
	}
	return n
}
