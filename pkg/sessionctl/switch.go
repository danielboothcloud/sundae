package sessionctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/signals"
)

// Attacher is a prepared session to attach to; *Prepared is one. Close
// releases its bridge count and is safe to call more than once.
type Attacher interface {
	Session() *openshell.Session
	Attach(ctx context.Context, term openshell.Terminal, opts ...openshell.BridgeOption) (int, error)
	Close()
}

// Switcher attaches one session at a time, switching on prefix n, p and s, for
// the CLI and the TUI alike; the zero Switcher attaches once.
// NOTE: with detach_key none there is no prefix, so no switching; ~. only detaches.
type Switcher struct {
	// Names lists the user's session names; nil disables switching.
	Names func(ctx context.Context) ([]string, error)
	// Prepare starts or resumes the named session for attaching, writing its
	// progress lines to progress.
	Prepare func(ctx context.Context, name string, progress io.Writer) (Attacher, error)
	// Pick asks the user which session prefix s switches to; returning
	// current goes back to it. nil ends Run with the *openshell.SwitchRequest,
	// for a caller with its own picker (the TUI's Sessions tab).
	Pick func(ctx context.Context, term openshell.Terminal, current string) (string, error)
	// Prefix is the prefix key, for the banner's detach hint.
	Prefix openshell.PrefixKey
}

// Run attaches term to cur and each session switched to until a detach, exit
// or error; it closes every Attacher and returns the last session's outcome.
// NOTE: cur's bridge is released only once the target is prepared, so a failed
// switch can attach cur again and it is not suspended meanwhile.
func (s *Switcher) Run(ctx context.Context, term openshell.Terminal, cur Attacher) (*openshell.Session, int, error) {
	defer func() { cur.Close() }()
	for {
		code, err := cur.Attach(ctx, term, s.bridgeOptions(ctx, cur)...)
		var sw *openshell.SwitchRequest
		if !errors.As(err, &sw) || !s.switching() || (sw.Dir == openshell.SwitchPick && s.Pick == nil) || ctx.Err() != nil {
			return cur.Session(), code, err
		}
		if next := s.switchTo(ctx, term, cur.Session().Name, sw.Dir); next != nil {
			cur.Close()
			cur = next
		}
	}
}

// switching reports whether s can switch sessions.
func (s *Switcher) switching() bool { return s.Names != nil && s.Prepare != nil }

// bridgeOptions enables switching for an attach to cur.
func (s *Switcher) bridgeOptions(ctx context.Context, cur Attacher) []openshell.BridgeOption {
	if !s.switching() {
		return nil
	}
	name := cur.Session().Name
	return []openshell.BridgeOption{openshell.WithSwitching(func() bool {
		names, err := s.Names(ctx)
		if err != nil {
			// Switch anyway: Run reports why it cannot.
			return true
		}
		_, ok := SwitchTarget(names, name, openshell.SwitchNext)
		return ok
	})}
}

// switchTo prepares the session dir names from current, saying so on term;
// nil means attach current again.
func (s *Switcher) switchTo(ctx context.Context, term openshell.Terminal, current string, dir openshell.SwitchDir) Attacher {
	target, err := s.target(ctx, term, current, dir)
	switch {
	case err != nil:
		say(term, "cannot switch: "+err.Error())
		return nil
	case target == "":
		say(term, "no other session")
		return nil
	case target == current:
		return nil
	}
	next, err := s.Prepare(ctx, target, &crlfWriter{w: term})
	if err != nil {
		say(term, fmt.Sprintf("cannot switch to %s: %v", target, err))
		return nil
	}
	sess := next.Session()
	say(term, "switched to "+signals.SessionLabel(sess.Name, sess.Repos)+". "+DetachHint(s.Prefix))
	return next
}

// target returns the session dir names from current; "" when there is none.
func (s *Switcher) target(ctx context.Context, term openshell.Terminal, current string, dir openshell.SwitchDir) (string, error) {
	if dir == openshell.SwitchPick {
		return s.Pick(ctx, term, current)
	}
	names, err := s.Names(ctx)
	if err != nil {
		return "", err
	}
	target, _ := SwitchTarget(names, current, dir)
	return target, nil
}

// SwitchTarget returns the session after (SwitchNext) or before (SwitchPrev)
// current among names sorted by name, wrapping around; current need not be
// listed. ok is false when there is no other session.
func SwitchTarget(names []string, current string, dir openshell.SwitchDir) (target string, ok bool) {
	others := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == current })
	slices.Sort(others)
	others = slices.Compact(others)
	if len(others) == 0 {
		return "", false
	}
	i, _ := slices.BinarySearch(others, current)
	if dir == openshell.SwitchPrev {
		i--
	}
	return others[(i+len(others))%len(others)], true
}

// say writes a Sundae line on the terminal, between attaches.
func say(w io.Writer, line string) {
	_, _ = io.WriteString(w, "[sundae] "+line+"\r\n")
}

// crlfWriter ends lines with CRLF, as the raw SSH terminal needs.
type crlfWriter struct{ w io.Writer }

func (c *crlfWriter) Write(p []byte) (int, error) {
	if _, err := c.w.Write(bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))); err != nil {
		return 0, err //nolint:wrapcheck
	}
	return len(p), nil
}
