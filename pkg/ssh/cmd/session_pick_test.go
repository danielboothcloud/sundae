package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/proto"
	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
	"github.com/danielboothcloud/sundae/pkg/sessionctl"
	"github.com/danielboothcloud/sundae/pkg/sshutils"
	"github.com/matryer/is"
)

func TestParseChoice(t *testing.T) {
	for line, want := range map[string]int{"1": 0, " 3 ": 2, "0": -1, "4": -1, "": -1, "x": -1} {
		i, err := parseChoice(line, 3)
		if want < 0 {
			if err == nil || err.Error() != "enter a number from 1 to 3" {
				t.Errorf("%q: %d %v", line, i, err)
			}
			continue
		}
		if err != nil || i != want {
			t.Errorf("%q: %d %v, want %d", line, i, err, want)
		}
	}
}

// pickTerm is a raw terminal: input typed, output shown.
type pickTerm struct {
	io.Reader
	out bytes.Buffer
}

func (p *pickTerm) Write(b []byte) (int, error) { return p.out.Write(b) }

func TestPromptSession(t *testing.T) {
	rows := []sessionctl.Row{
		{Info: openshell.Info{Name: "a", Repos: []string{"r1"}}},
		{Info: openshell.Info{Name: "b", Repos: []string{"r2"}}},
	}
	// An invalid number asks again; backspace edits; input after Enter stays.
	term := &pickTerm{Reader: strings.NewReader("7\r\x7f1\x7f2\rrest")}
	name, err := promptSession(term, rows, "")
	if err != nil || name != "b" {
		t.Fatalf("name %q err %v", name, err)
	}
	out := term.out.String()
	for _, want := range []string{"  1) r1 / a", "  2) r2 / b", "Attach to [1-2, q cancels]: 7\r\nenter a number from 1 to 2\r\n", "1\b \b2\r\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
	if rest, _ := io.ReadAll(term.Reader); string(rest) != "rest" {
		t.Fatalf("rest %q", rest)
	}

	for _, in := range []string{"q", "\x1b", "\x03", "1\x04", ""} {
		if _, err := promptSession(&pickTerm{Reader: strings.NewReader(in)}, rows, ""); !errors.Is(err, errPickCancelled) {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func TestSessionAttachWithoutArgument(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	for _, r := range []string{"app", "lib"} {
		_, err := be.CreateRepository(ctx, r, user, proto.RepositoryOptions{})
		is.NoErr(err)
	}
	withSess := func(s *fakeSSHSession) context.Context {
		return context.WithValue(ctx, sshutils.ContextKeySession, s)
	}

	// No sessions.
	_, _, err := runSessionCmd(t, withSess(&fakeSSHSession{}), "attach")
	is.True(err != nil && strings.Contains(err.Error(), "you have no sessions; start one with: session start REPO"))
	_, _, err = runSessionCmd(t, withSess(&fakeSSHSession{}), "attach", "a", "b")
	is.True(err != nil && strings.Contains(err.Error(), "accepts at most 1 arg"))

	// One session: attached to without asking; still needs a terminal.
	noPty := withSess(&fakeSSHSession{noPty: true})
	_, _, err = runSessionCmd(t, noPty, "start", "app", "--detach")
	is.NoErr(err)
	_, _, err = runSessionCmd(t, noPty, "attach")
	is.True(err != nil && strings.Contains(err.Error(), "requires a terminal"))
	sess := &fakeSSHSession{}
	_, stderr, err := runSessionCmd(t, withSess(sess), "attach")
	is.NoErr(err)
	is.Equal(len(fc.interactive), 1)
	is.True(strings.Contains(stderr, "Attached to app / app. Detach: Ctrl-] d (or ~. after a newline)."))

	// Several, without a terminal: name one.
	_, _, err = runSessionCmd(t, noPty, "start", "lib", "--detach")
	is.NoErr(err)
	_, _, err = runSessionCmd(t, noPty, "attach")
	is.True(err != nil && strings.Contains(err.Error(), "you have 2 sessions (app, lib); name one: session attach SESSION"))

	// Several, with a terminal: pick one.
	sess = &fakeSSHSession{in: strings.NewReader("2\r")}
	_, _, err = runSessionCmd(t, withSess(sess), "attach")
	is.NoErr(err)
	is.True(strings.Contains(sess.out.String(), "Your sessions:"))
	is.Equal(len(fc.interactive), 2)
	is.Equal(fc.interactive[1][len(fc.interactive[1])-1], fc.osewa[openshell.SessionSandboxName("alice", "lib")])

	// ...or cancel.
	sess = &fakeSSHSession{in: strings.NewReader("q")}
	_, stderr, err = runSessionCmd(t, withSess(sess), "attach")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Cancelled."))
	is.Equal(len(fc.interactive), 2)
}

func TestSessionAttachDetachMessage(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	_, err := be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)
	_, _, err = runSessionCmd(t, context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true}), "start", "r", "--detach")
	is.NoErr(err)

	fc.blockUntilCancel = true
	sess := &fakeSSHSession{in: strings.NewReader("\x1dd")}
	_, stderr, err := runSessionCmd(t, context.WithValue(ctx, sshutils.ContextKeySession, sess), "attach", "r")
	is.NoErr(err)
	is.True(!sess.exited)
	is.True(strings.Contains(stderr, "Detached from r / r; it keeps running. Re-attach: session attach r"))

	// With no prefix key, only ~. is offered and Ctrl-] reaches the program.
	fc.blockUntilCancel = false
	config.FromContext(ctx).Sessions.Attach.DetachKey = "none"
	_, stderr, err = runSessionCmd(t, context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{in: strings.NewReader("\x1dd")}), "attach", "r")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Attached to r / r. Detach: ~. after a newline."))
	is.True(!strings.Contains(stderr, "Detached from"))
}

// switchSetup starts sessions a and b (repositories of the same names),
// detached, and makes the attach client run until it is cancelled.
func switchSetup(t *testing.T) (context.Context, *fakeOpenShell, *openshell.Monitor) {
	t.Helper()
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	ctx, mon := withMonitor(ctx, fc)
	noPty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true})
	for _, r := range []string{"a", "b"} {
		_, err := be.CreateRepository(ctx, r, user, proto.RepositoryOptions{})
		is.NoErr(err)
		_, _, err = runSessionCmd(t, noPty, "start", r, "--detach")
		is.NoErr(err)
	}
	fc.blockUntilCancel = true
	return ctx, fc, mon
}

// typed returns input arriving in separate reads, as keys typed apart.
func typed(keys ...string) io.Reader {
	rs := make([]io.Reader, len(keys))
	for i, k := range keys {
		rs[i] = strings.NewReader(k)
	}
	return io.MultiReader(rs...)
}

func TestSessionAttachSwitchNextThenDetach(t *testing.T) {
	is := is.New(t)
	ctx, fc, mon := switchSetup(t)
	// The target is suspended: switching resumes it, its progress on the terminal.
	fc.sandboxes[openshell.SessionSandboxName("alice", "b")].Phase = sbx.Suspended
	sess := &fakeSSHSession{in: typed("\x1dn", "\x1dd")}
	_, stderr, err := runSessionCmd(t, context.WithValue(ctx, sshutils.ContextKeySession, sess), "attach", "a")
	is.NoErr(err)
	is.True(!sess.exited)
	is.Equal(len(fc.interactive), 2)
	is.Equal(fc.interactive[1][len(fc.interactive[1])-1], fc.osewa[openshell.SessionSandboxName("alice", "b")])
	is.True(strings.Contains(sess.out.String(), "Session b was suspended; resuming it...\r\n"))
	is.Equal(fc.phase(openshell.SessionSandboxName("alice", "b")), sbx.Ready)
	is.True(strings.Contains(sess.out.String(), "[sundae] switched to b / b. Detach: Ctrl-] d (or ~. after a newline).\r\n"))
	is.True(strings.Contains(stderr, "Detached from b / b; it keeps running. Re-attach: session attach b"))
	// Every bridge is released.
	is.Equal(mon.Bridges(openshell.SessionSandboxName("alice", "a")), 0)
	is.Equal(mon.Bridges(openshell.SessionSandboxName("alice", "b")), 0)
}

func TestSessionAttachSwitchFailureFallsBack(t *testing.T) {
	is := is.New(t)
	ctx, fc, mon := switchSetup(t)
	sbB := openshell.SessionSandboxName("alice", "b")
	fc.scripts = func(sandbox string, command []string) (*openshell.ExecResult, bool) {
		if sandbox == sbB && command[0] == "test" {
			return &openshell.ExecResult{ExitCode: 2, Stderr: []byte("disk on fire")}, true
		}
		return nil, false
	}
	sess := &fakeSSHSession{in: typed("\x1dp", "\x1dd")}
	_, stderr, err := runSessionCmd(t, context.WithValue(ctx, sshutils.ContextKeySession, sess), "attach", "a")
	is.NoErr(err)
	is.True(strings.Contains(sess.out.String(), "[sundae] cannot switch to b: "))
	is.True(strings.Contains(sess.out.String(), "disk on fire"))
	// a is attached again, then detached.
	is.Equal(len(fc.interactive), 2)
	is.Equal(fc.interactive[1][len(fc.interactive[1])-1], fc.osewa[openshell.SessionSandboxName("alice", "a")])
	is.True(strings.Contains(stderr, "Detached from a / a"))
	is.Equal(mon.Bridges(sbB), 0)
	is.Equal(mon.Bridges(openshell.SessionSandboxName("alice", "a")), 0)
}

func TestSessionAttachSwitchPick(t *testing.T) {
	is := is.New(t)
	ctx, fc, _ := switchSetup(t)

	// q goes back to the current session.
	sess := &fakeSSHSession{in: typed("\x1ds", "q", "\x1dd")}
	_, stderr, err := runSessionCmd(t, context.WithValue(ctx, sshutils.ContextKeySession, sess), "attach", "a")
	is.NoErr(err)
	out := sess.out.String()
	is.True(strings.Contains(out, "*  1) a / a"))
	is.True(strings.Contains(out, "(current)"))
	is.True(strings.Contains(out, "   2) b / b"))
	is.True(strings.Contains(out, "Switch to [1-2, q goes back]: "))
	is.True(!strings.Contains(out, "switched to"))
	is.Equal(len(fc.interactive), 2)
	is.Equal(fc.interactive[1][len(fc.interactive[1])-1], fc.osewa[openshell.SessionSandboxName("alice", "a")])
	is.True(strings.Contains(stderr, "Detached from a / a"))

	// A number switches.
	sess = &fakeSSHSession{in: typed("\x1ds", "2", "\r", "\x1dd")}
	_, stderr, err = runSessionCmd(t, context.WithValue(ctx, sshutils.ContextKeySession, sess), "attach", "a")
	is.NoErr(err)
	is.True(strings.Contains(sess.out.String(), "[sundae] switched to b / b."))
	is.True(strings.Contains(stderr, "Detached from b / b"))
}

func TestSessionAttachSwitchOnlySession(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	_, err := be.CreateRepository(ctx, "a", user, proto.RepositoryOptions{})
	is.NoErr(err)
	_, _, err = runSessionCmd(t, context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true}), "start", "a", "--detach")
	is.NoErr(err)
	fc.blockUntilCancel = true
	sess := &fakeSSHSession{in: typed("\x1dn", "\x1dd")}
	_, _, err = runSessionCmd(t, context.WithValue(ctx, sshutils.ContextKeySession, sess), "attach", "a")
	is.NoErr(err)
	is.True(strings.Contains(sess.out.String(), "[sundae] no other session"))
	// Still the first attach: it stayed attached.
	is.Equal(len(fc.interactive), 1)
}
