package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/sshutils"
	"github.com/matryer/is"
)

// screenCall is one run of the Sessions screen.
type screenCall struct{ sandbox, label string }

// withScreen gives ctx a Sessions screen that records its runs; ok is what
// it reports.
func withScreen(ctx context.Context, ok bool) (context.Context, *[]screenCall) {
	var calls []screenCall
	return context.WithValue(ctx, SessionsScreenKey, SessionsScreen(func(sandbox, label string) bool {
		calls = append(calls, screenCall{sandbox, label})
		return ok
	})), &calls
}

func TestSessionAttachHomeKeyOpensSessionsScreen(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	_, err := be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)
	fc.blockUntilCancel = true

	sess := &fakeSSHSession{in: strings.NewReader("ls\x1dd")}
	ctx = context.WithValue(ctx, sshutils.ContextKeySession, sess)
	ctx, calls := withScreen(ctx, true)
	_, stderr, err := runSessionCmd(t, ctx, "start", "r")
	is.NoErr(err)
	is.True(!sess.exited) // exit 0
	is.Equal(*calls, []screenCall{{openshell.SessionSandboxName("alice", "r"), "r / r"}})
	// The status bar replaces the hint line; the program gets the other rows.
	is.True(!strings.Contains(stderr, "Attached to"))
	is.True(strings.Contains(sess.output(), " r / r "))
	is.True(strings.Contains(sess.output(), " Ctrl-] sessions "))
	is.Equal(fc.sizes[0], [2]uint32{80, 23})
	is.True(!strings.Contains(stderr, "Left"))
}

func TestSessionAttachHomeKeyWithoutSessionsScreen(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	_, err := be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)
	fc.blockUntilCancel = true
	config.FromContext(ctx).Sessions.Attach.StatusBar = false

	sess := &fakeSSHSession{in: strings.NewReader("\x1d")}
	ctx = context.WithValue(ctx, sshutils.ContextKeySession, sess)
	ctx, calls := withScreen(ctx, false)
	_, stderr, err := runSessionCmd(t, ctx, "start", "r")
	is.NoErr(err)
	is.True(!sess.exited)
	is.Equal(len(*calls), 1)
	is.True(strings.Contains(stderr, "Attached to r / r. Ctrl-] opens the Sessions screen.\n"))
	is.True(strings.Contains(stderr, "Left r / r; it keeps running. Re-attach: session attach r\n"))
	is.Equal(fc.sizes[0], [2]uint32{80, 24})
}

func TestSessionAttachSmallTerminalHasNoStatusBar(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	_, err := be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)

	sess := &fakeSSHSession{rows: 4}
	_, stderr, err := runSessionCmd(t, context.WithValue(ctx, sshutils.ContextKeySession, sess), "start", "r")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Attached to r / r. Ctrl-] opens the Sessions screen.\n"))
	is.Equal(fc.sizes[0], [2]uint32{80, 4})
	is.True(!strings.Contains(sess.output(), "Ctrl-] sessions"))
}

func TestSessionAttachWithoutName(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	withPty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{})
	noPty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true})

	// None.
	_, _, err := runSessionCmd(t, withPty, "attach")
	is.True(err != nil && strings.Contains(err.Error(), "you have no sessions; start one with: session start REPO"))

	// One: attached.
	_, err = be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)
	_, _, err = runSessionCmd(t, noPty, "start", "r", "--detach")
	is.NoErr(err)
	_, _, err = runSessionCmd(t, noPty, "attach")
	is.True(err != nil && strings.Contains(err.Error(), "session attach requires a terminal (ssh -t)"))
	screenCtx, calls := withScreen(withPty, true)
	_, _, err = runSessionCmd(t, screenCtx, "attach")
	is.NoErr(err)
	is.Equal(len(fc.interactive), 1)
	is.Equal(len(*calls), 0)

	// Several: the Sessions screen, or without a terminal an error naming them.
	_, _, err = runSessionCmd(t, noPty, "start", "r", "--name", "review", "--detach")
	is.NoErr(err)
	_, _, err = runSessionCmd(t, screenCtx, "attach")
	is.NoErr(err)
	is.Equal(*calls, []screenCall{{"", ""}})
	is.Equal(len(fc.interactive), 1)
	_, _, err = runSessionCmd(t, noPty, "attach")
	is.True(err != nil && strings.Contains(err.Error(), "you have 2 sessions (r, review); name one: session attach SESSION"))
	// No TUI to show: the same error.
	noScreen, _ := withScreen(withPty, false)
	_, _, err = runSessionCmd(t, noScreen, "attach")
	is.True(err != nil && strings.Contains(err.Error(), "you have 2 sessions"))
}
