package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/ssh"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/signals"
	"github.com/danielboothcloud/sundae/pkg/sshutils"
	"github.com/spf13/cobra"
)

// errNoSignals is returned when the server keeps no signals feed.
var errNoSignals = errors.New("the signals feed is not available")

// defaultSignalsLimit is how many signals `session signals` shows by
// default.
const defaultSignalsLimit = 50

// Column widths of the `session signals` table. Rows are printed as they
// come with --follow, so the widths are fixed rather than fitted.
const (
	signalTimeWidth  = 8
	signalLabelWidth = 22
	signalWhereWidth = 28
	signalColumnGap  = "  "
)

func sessionSignalsCommand() *cobra.Command {
	var (
		repo, name                string
		attention, follow, asJSON bool
		limit                     int
	)
	cmd := &cobra.Command{
		Use:   "signals",
		Short: "Show what your agent sessions reported, newest first",
		Long: `Show the signals of your agent sessions, newest first: every status
change (OSC 7501) and lifecycle step (created, ready, resumed, parked,
exited, ended, failed) Sundae has seen, with when it happened, the state,
the session (and the record id of a child record) and the message.

--attention shows only what asks for you: blocked, error and done reports,
and failures. --follow prints the signals kept so far oldest first, like
tail -f, then each new one as it arrives, until you disconnect (Ctrl-C) or
the server shuts down; it needs no terminal. --json prints one JSON object
per line.

Signals are kept in memory only, the last sessions.signals.keep across all
users, so the feed is empty after a restart. Status is collected by polling
(sessions.status.poll_interval): changes between two polls, and child
records, are not seen. A "! missed" row marks signals that were lost.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			user := proto.UserFromContext(ctx)
			if user == nil {
				return proto.ErrUnauthorized
			}
			if limit < 0 {
				return fmt.Errorf("--limit %d must not be negative", limit)
			}
			h := signals.FromContext(ctx)
			if h == nil {
				return errNoSignals
			}
			q := signals.Query{
				UserID:    user.ID(),
				Repo:      repo,
				Session:   name,
				Attention: attention,
				Limit:     limit,
			}

			w := &signalWriter{out: cmd.OutOrStdout(), json: asJSON, now: time.Now}
			sess := sshutils.SessionFromContext(ctx)
			var (
				winCh  <-chan ssh.Window
				hasPty bool
			)
			if sess != nil {
				if pty, ch, ok := sess.Pty(); ok {
					w.width.Store(int64(pty.Window.Width))
					winCh, hasPty = ch, true
				}
			}

			if !follow {
				sigs := h.Snapshot(q)
				if len(sigs) == 0 && !asJSON {
					cmd.Println("No signals yet")
					return nil
				}
				return w.write(sigs)
			}

			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if hasPty {
				// With a PTY the client is in raw mode: Ctrl-C arrives as
				// a byte rather than closing the connection.
				go stopOnInterrupt(sess, cancel)
				go func() {
					for {
						select {
						case win, ok := <-winCh:
							if !ok {
								return
							}
							w.width.Store(int64(win.Width))
						case <-ctx.Done():
							return
						}
					}
				}()
			}
			return followSignals(ctx, h, q, w, cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVarP(&repo, "repo", "r", "", "Only signals of this repository's sessions")
	cmd.Flags().StringVarP(&name, "name", "n", "", "Only signals of sessions with this name")
	cmd.Flags().BoolVarP(&attention, "attention", "a", false, "Only signals that ask for you: blocked, error, done, failures")
	cmd.Flags().IntVarP(&limit, "limit", "l", defaultSignalsLimit, "Show at most this many signals (0 for all kept); with --follow, of those kept so far")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep printing new signals as they arrive")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print one JSON object per line")
	return cmd
}

// followSignals prints the signals kept so far, oldest first, then new ones
// as they arrive, until ctx is done (the client went away) or the History
// closes (the server is shutting down).
func followSignals(ctx context.Context, h *signals.History, q signals.Query, w *signalWriter, stderr io.Writer) error {
	snap, feed := h.Follow(q)
	slices.Reverse(snap)
	if err := w.write(snap); err != nil {
		return err
	}
	for {
		sigs, err := feed.Next(ctx)
		switch {
		case errors.Is(err, signals.ErrClosed):
			fmt.Fprintln(stderr, "The signals feed closed: the server is shutting down.") //nolint:errcheck
			return nil
		case err != nil:
			// The client went away.
			return nil
		}
		if err := w.write(sigs); err != nil {
			return err
		}
	}
}

// stopOnInterrupt calls stop when the client types Ctrl-C, Ctrl-D or q.
// It returns when reading fails, which it does once the session ends.
func stopOnInterrupt(r io.Reader, stop func()) {
	buf := make([]byte, 64)
	for {
		n, err := r.Read(buf)
		for _, b := range buf[:n] {
			if b == 0x03 || b == 0x04 || b == 'q' {
				stop()
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// signalWriter prints signals as table rows or JSON lines.
type signalWriter struct {
	out  io.Writer
	json bool
	now  func() time.Time
	// width is the terminal width, 0 without a PTY. Rows are cut to it.
	width atomic.Int64
	// headed reports whether the table header was printed.
	headed bool
}

// write prints sigs in the order given.
func (w *signalWriter) write(sigs []signals.Signal) error {
	if w.json {
		enc := json.NewEncoder(w.out)
		for _, s := range sigs {
			if err := enc.Encode(signalToJSON(s)); err != nil {
				return err //nolint:wrapcheck
			}
		}
		return nil
	}
	var b strings.Builder
	if !w.headed {
		w.headed = true
		b.WriteString(w.line("TIME", "STATE", "SESSION", "MESSAGE"))
	}
	now := w.now()
	for _, s := range sigs {
		b.WriteString(w.line(signals.FormatTime(s.Time, now), s.Label(), s.Where(), s.Message(0)))
	}
	_, err := io.WriteString(w.out, b.String())
	return err //nolint:wrapcheck
}

// line renders one table row, cut to the terminal width when there is one.
func (w *signalWriter) line(when, label, where, msg string) string {
	l := pad(when, signalTimeWidth) + signalColumnGap +
		pad(label, signalLabelWidth) + signalColumnGap +
		pad(ansi.Truncate(orDash(where), signalWhereWidth, "…"), signalWhereWidth) + signalColumnGap +
		orDash(msg)
	if width := int(w.width.Load()); width > 0 {
		l = ansi.Truncate(l, width, "…")
	}
	return strings.TrimRight(l, " ") + "\n"
}

// pad pads s with spaces to n cells.
func pad(s string, n int) string {
	if w := ansi.StringWidth(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// signalJSON is one signal of `session signals --json`. Text from the
// program and from errors is sanitised but not cut.
type signalJSON struct {
	ID   uint64    `json:"id"`
	Time time.Time `json:"time"`
	// Type is status, lifecycle or gap.
	Type    string `json:"type"`
	Repo    string `json:"repo,omitempty"`
	Session string `json:"session,omitempty"`
	Sandbox string `json:"sandbox,omitempty"`
	// Record is a child record's id; empty for the root record.
	Record string `json:"record,omitempty"`
	// State is the state the row shows (see signals.Signal.State).
	State     string `json:"state"`
	Previous  string `json:"previous,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Progress  *int   `json:"progress,omitempty"`
	App       string `json:"app,omitempty"`
	Title     string `json:"title,omitempty"`
	Msg       string `json:"msg,omitempty"`
	Step      string `json:"step,omitempty"`
	Op        string `json:"op,omitempty"`
	Reason    string `json:"reason,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Lost      uint64 `json:"lost,omitempty"`
	GapReason string `json:"gap_reason,omitempty"`
	Attention bool   `json:"attention"`
	// Message is the row's message.
	Message string `json:"message"`
}

func signalToJSON(s signals.Signal) signalJSON {
	j := signalJSON{
		ID:        s.ID,
		Time:      s.Time,
		State:     s.State().Word(),
		Attention: s.NeedsAttention(),
		Message:   s.Message(0),
	}
	session := func(sess events.Session) {
		j.Repo = signals.Sanitize(sess.Repo, 0)
		j.Session = signals.Sanitize(sess.DisplayName(), 0)
		j.Sandbox = signals.Sanitize(sess.Sandbox, 0)
	}
	switch {
	case s.IsGap():
		j.Type = "gap"
		j.Lost, j.GapReason = s.Gap.Lost, s.Gap.Reason
	case s.Status != nil:
		st := s.Status
		j.Type = "status"
		session(st.Session)
		r := st.Record
		j.Record = signals.Sanitize(r.ID, 0)
		j.Previous = st.Previous.Word()
		j.Kind = signals.Sanitize(string(r.Kind), 0)
		j.Progress = r.Progress
		j.App, j.Title, j.Msg = signals.Sanitize(r.App, 0), signals.Sanitize(r.Title, 0), signals.Sanitize(r.Msg, 0)
	case s.Lifecycle != nil:
		l := s.Lifecycle
		j.Type = "lifecycle"
		session(l.Session)
		j.Step = string(l.Step)
		j.Op = signals.Sanitize(l.Op, 0)
		j.Reason = signals.Sanitize(l.Reason, 0)
		j.ExitCode = l.ExitCode
	}
	return j
}
