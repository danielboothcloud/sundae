package cmd

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/sessionctl"
	"github.com/danielboothcloud/sundae/pkg/signals"
	"github.com/danielboothcloud/sundae/pkg/sshutils"
	"github.com/spf13/cobra"
)

// errPickCancelled is returned when the user leaves the session picker.
var errPickCancelled = errors.New("cancelled")

// pickSession returns the session `session attach` without SESSION attaches
// to: the user's only one, else the one picked from a list on the terminal.
func pickSession(cmd *cobra.Command) (string, error) {
	ctx := cmd.Context()
	client, err := newOpenShellClient(config.FromContext(ctx))
	if err != nil {
		return "", err
	}
	defer client.Close() //nolint:errcheck
	rows, err := sessionctl.List(ctx, newManager(ctx, client), openshell.MonitorFromContext(ctx), proto.UserFromContext(ctx))
	if err != nil {
		return "", err //nolint:wrapcheck
	}
	switch len(rows) {
	case 0:
		return "", errors.New("you have no sessions; start one with: session start REPO")
	case 1:
		return rows[0].Info.Name, nil
	}
	sess := sshutils.SessionFromContext(ctx)
	if sess == nil {
		return "", severalSessionsError(rows)
	}
	if _, _, ok := sess.Pty(); !ok {
		return "", severalSessionsError(rows)
	}
	return promptSession(sess, rows)
}

// severalSessionsError asks for a session name when there is no terminal to
// pick one on.
func severalSessionsError(rows []sessionctl.Row) error {
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.Info.Name
	}
	return fmt.Errorf("you have %d sessions (%s); name one: session attach SESSION", len(rows), strings.Join(names, ", "))
}

// promptSession lists rows on the terminal, numbered, and returns the name
// of the one the user picks. q, Ctrl-C, Ctrl-D or the end of the input
// cancel (errPickCancelled).
func promptSession(term io.ReadWriter, rows []sessionctl.Row) (string, error) {
	var b strings.Builder
	b.WriteString("Your sessions:\r\n")
	for i, r := range rows {
		fmt.Fprintf(&b, "%3d) %s  %s\r\n", i+1, signals.SessionLabel(r.Info.Name, r.Info.Repos), r.Label())
	}
	if _, err := io.WriteString(term, b.String()); err != nil {
		return "", err //nolint:wrapcheck
	}
	prompt := fmt.Sprintf("Attach to [1-%d, q cancels]: ", len(rows))
	for {
		if _, err := io.WriteString(term, prompt); err != nil {
			return "", err //nolint:wrapcheck
		}
		line, err := readChoice(term)
		if err != nil {
			_, _ = io.WriteString(term, "\r\n")
			return "", err
		}
		_, _ = io.WriteString(term, "\r\n")
		i, err := parseChoice(line, len(rows))
		if err == nil {
			return rows[i].Info.Name, nil
		}
		_, _ = io.WriteString(term, err.Error()+"\r\n")
	}
}

// readChoice reads a number up to Enter from a terminal in raw mode,
// echoing it, as the SSH PTY has no line discipline.
func readChoice(term io.ReadWriter) (string, error) {
	var line []byte
	// NOTE: one byte at a time, so input after Enter stays for the attach.
	buf := make([]byte, 1)
	for {
		if _, err := io.ReadFull(term, buf); err != nil {
			return "", errPickCancelled
		}
		switch c := buf[0]; {
		case c == '\r' || c == '\n':
			return string(line), nil
		case c == 'q' || c == 'Q' || c == 0x03 || c == 0x04:
			return "", errPickCancelled
		case (c == 0x7f || c == '\b') && len(line) > 0:
			line = line[:len(line)-1]
			_, _ = io.WriteString(term, "\b \b")
		case c >= '0' && c <= '9' && len(line) < 6:
			line = append(line, c)
			_, _ = term.Write([]byte{c})
		}
	}
}

// parseChoice returns the index of the row a 1-based choice of n names.
func parseChoice(line string, n int) (int, error) {
	i, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || i < 1 || i > n {
		return 0, fmt.Errorf("enter a number from 1 to %d", n)
	}
	return i - 1, nil
}
