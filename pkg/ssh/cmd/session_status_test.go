package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/sshutils"
	"github.com/matryer/is"
)

// withMonitor attaches a Monitor polling fc to the context, as the SSH
// server does.
func withMonitor(ctx context.Context, fc *fakeOpenShell) (context.Context, *openshell.Monitor) {
	mon := openshell.NewMonitor(func() (openshell.Client, error) { return fc, nil }, config.FromContext(ctx),
		openshell.WithMonitorLogger(log.New(io.Discard)))
	return openshell.ContextWithMonitor(ctx, mon), mon
}

// tableRows returns the rows of a `session ls` table that mention a
// sandbox, with the cell borders turned into " | ".
func tableRows(out string) []string {
	var rows []string
	for _, l := range strings.Split(out, "\n") {
		if !strings.Contains(l, "sd-") {
			continue
		}
		cells := strings.FieldsFunc(l, func(r rune) bool { return r == '│' || r == '|' })
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		rows = append(rows, strings.Join(cells, " | "))
	}
	return rows
}

func TestSessionListStatus(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	for _, r := range []string{"infra", "docs", "web"} {
		_, err := be.CreateRepository(ctx, r, user, proto.RepositoryOptions{})
		is.NoErr(err)
	}
	ctx, mon := withMonitor(ctx, fc)
	noPty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true})
	for _, args := range [][]string{
		{"start", "infra", "-d"},
		{"start", "infra", "-d", "-n", "review"},
		{"start", "docs", "-d"},
		{"start", "web", "-d"},
		{"start", "web", "-d", "-n", "quiet"},
	} {
		_, _, err := runSessionCmd(t, noPty, args...)
		is.NoErr(err)
	}
	sb := func(repo, name string) string { return openshell.SessionSandboxName("alice", name) }
	fc.setStatus(sb("infra", "infra"), 1, map[string]any{"state": "working", "app": "claude-code", "msg": "Running go test ./pkg/ui/...", "progress": 40})
	fc.setStatus(sb("infra", "review"), 0, map[string]any{
		"state": "blocked", "kind": "permission", "app": "claude-code",
		"msg": "Apply \x1b[31m3 to add,\u202e 1 to change?\n" + strings.Repeat("x", 100),
	})
	fc.setStatus(sb("docs", "docs"), 0, map[string]any{"state": "done", "app": "claude-code", "title": "Implemented GitHub discovery"})
	fc.setStatus(sb("web", "web"), 0, map[string]any{"state": "error", "msg": "build failed"})
	fc.setStatus(sb("web", "quiet"), 0, nil)
	mon.Poll(ctx)

	// A parked blocked session still reads blocked.
	_, stderr, err := runSessionCmd(t, ctx, "park", "review")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Session review parked. Resume it with: session resume review"))
	mon.Poll(ctx)

	stdout, _, err := runSessionCmd(t, ctx, "ls")
	is.NoErr(err)
	for _, h := range []string{"State", "Message", "Attached", "Changed"} {
		is.True(strings.Contains(stdout, h)) // missing header
	}
	rows := tableRows(stdout)
	is.Equal(len(rows), 5)
	want := []string{
		"review | infra | - | " + sb("infra", "review") + " | Suspended | ▲ blocked · permission · parked | Apply [31m3 to add, 1 to change? x",
		"web | web | - | " + sb("web", "web") + " | Ready | ✗ error | build failed | 0 | now",
		"docs | docs | - | " + sb("docs", "docs") + " | Ready | ✓ done | Implemented GitHub discovery | 0 | now",
		"infra | infra | - | " + sb("infra", "infra") + " | Ready | ● working 40% | Running go test ./pkg/ui/... | 1 | now",
		"quiet | web | - | " + sb("web", "quiet") + " | Ready | · none | - | 0 | now",
	}
	for i, w := range want {
		if !strings.HasPrefix(rows[i], w) {
			t.Fatalf("row %d:\n got %q\nwant %q...", i, rows[i], w)
		}
	}
	// The message is sanitised and cut.
	is.True(!strings.ContainsAny(stdout, "\x1b\u202e"))
	is.True(strings.Contains(rows[0], "Apply [31m3 to add, 1 to change? xxxx"))
	is.True(strings.Contains(rows[0], "…"))

	// Listing never resumes.
	is.Equal(fc.phase(sb("infra", "review")), types.SandboxSuspended)
	is.Equal(fc.resumes, 0)

	stdout, _, err = runSessionCmd(t, ctx, "ls", "--json")
	is.NoErr(err)
	var got []map[string]any
	is.NoErr(json.Unmarshal([]byte(stdout), &got))
	is.Equal(len(got), 5)
	order := []string{}
	for _, s := range got {
		order = append(order, s["repo"].(string)+"/"+s["name"].(string))
	}
	is.Equal(strings.Join(order, ","), "infra/review,web/web,docs/docs,infra/infra,web/quiet")
	review := got[0]
	is.Equal(review["state"], "blocked")
	is.Equal(review["kind"], "permission")
	is.Equal(review["parked"], true)
	is.Equal(review["phase"], "Suspended")
	is.True(!strings.ContainsAny(review["msg"].(string), "\x1b\u202e\n"))
	is.True(strings.HasSuffix(review["msg"].(string), strings.Repeat("x", 100))) // not cut
	working := got[3]
	is.Equal(working["progress"], float64(40))
	is.Equal(working["attached"], float64(1))
	is.Equal(working["running"], true)
	is.True(working["changed"] != nil && working["polled"] != nil)
	is.Equal(got[4]["state"], "none")

	// Without a Monitor (or before its first poll) the status is unknown.
	plain := context.WithValue(ctx, openshell.MonitorContextKey, (*openshell.Monitor)(nil))
	stdout, _, err = runSessionCmd(t, plain, "ls")
	is.NoErr(err)
	rows = tableRows(stdout)
	is.True(strings.HasPrefix(rows[0], "docs | docs | - | "+sb("docs", "docs")+" | Ready | · none | - | - | -"))
	is.True(strings.Contains(stdout, "· none · parked"))
}

func TestSessionParkAndResumeCommands(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	_, err := be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)
	ctx, _ = withMonitor(ctx, fc)
	bus := events.New(events.WithLogger(log.New(io.Discard)))
	defer bus.Close()
	sub := bus.Subscribe(events.ForUser(user.ID()))
	ctx = events.WithContext(ctx, bus)
	noPty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true})

	_, _, err = runSessionCmd(t, noPty, "start", "r", "-d", "-n", "two")
	is.NoErr(err)
	sandbox := openshell.SessionSandboxName("alice", "two")

	_, stderr, err := runSessionCmd(t, ctx, "park", "two")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Session two parked"))
	is.Equal(fc.phase(sandbox), types.SandboxSuspended)
	_, stderr, err = runSessionCmd(t, ctx, "park", "two")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Session two is already parked"))

	_, stderr, err = runSessionCmd(t, noPty, "resume", "two")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Session two was parked; resuming it"))
	is.True(strings.Contains(stderr, "Session two (r) is running in sandbox "+sandbox))
	is.Equal(fc.phase(sandbox), types.SandboxReady)
	is.Equal(fc.resumes, 1)
	is.Equal(len(fc.interactive), 0) // resume never attaches

	// resume never creates; park needs a session.
	_, _, err = runSessionCmd(t, noPty, "resume", "nope")
	is.True(err != nil && strings.Contains(err.Error(), "no such session"))
	_, _, err = runSessionCmd(t, ctx, "park", "nope")
	is.True(err != nil && strings.Contains(err.Error(), "no such session"))
	_, ok := fc.sandboxes[openshell.SessionSandboxName("alice", "nope")]
	is.True(!ok)

	// A gateway that cannot suspend says so.
	fc.suspendErr = &types.StatusError{Code: types.ErrorConflict, GRPCCode: 9, Message: "compute driver does not support sandbox suspend"}
	_, _, err = runSessionCmd(t, ctx, "park", "two")
	is.True(err != nil && strings.Contains(err.Error(), "cannot suspend"))
	fc.suspendErr = nil
	// ...and the Monitor remembers: it is not asked again.
	_, _, err = runSessionCmd(t, ctx, "park", "two")
	is.True(err != nil && strings.Contains(err.Error(), "cannot suspend"))
	is.Equal(fc.suspends, 1)

	// Publishing is synchronous: everything is buffered by now.
	var steps []string
	for len(sub.C) > 0 {
		l := (<-sub.C).Lifecycle
		steps = append(steps, string(l.Step)+":"+l.Op)
	}
	is.Equal(strings.Join(steps, ","), "created:,ready:,parked:park,resumed:,ready:")
}

// Attaching to a parked session resumes it; after a clean restart the
// program is started again and the user is told.
func TestSessionAttachParkedCleanRestart(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	_, err := be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)
	ctx, _ = withMonitor(ctx, fc)
	_, _, err = runSessionCmd(t, context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true}), "start", "r", "-d")
	is.NoErr(err)
	_, _, err = runSessionCmd(t, ctx, "park", "r")
	is.NoErr(err)

	fc.cleanRestart = true
	sess := &fakeSSHSession{}
	_, stderr, err := runSessionCmd(t, context.WithValue(ctx, sshutils.ContextKeySession, sess), "attach", "r")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Session r was parked; resuming it"))
	is.True(strings.Contains(stderr, "Started bash -l again: it was not running when the session resumed"))
	is.Equal(fc.resumes, 1)
	is.Equal(strings.Join(fc.interactive[0], " "), "/usr/local/bin/osewa attach --take --predict never sandbox id2")
}

// Every way a start or attach ends closes its bridge.
func TestSessionBridgeCounting(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	_, err := be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)
	ctx, mon := withMonitor(ctx, fc)
	sandbox := openshell.SessionSandboxName("alice", "r")
	withSess := func() context.Context { return context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{}) }

	var during []int
	fc.onInteractive = func(name string) { during = append(during, mon.Bridges(name)) }

	// Attached, then the program exits (or the user detaches).
	_, _, err = runSessionCmd(t, withSess(), "start", "r")
	is.NoErr(err)
	is.Equal(during, []int{1})
	is.Equal(mon.Bridges(sandbox), 0)

	// Two bridges at once count two.
	fc.onInteractive = func(name string) {
		during = append(during, mon.Bridges(name))
		if len(during) == 2 {
			_, _, err := runSessionCmd(t, withSess(), "attach", "r")
			is.NoErr(err)
		}
	}
	_, _, err = runSessionCmd(t, withSess(), "attach", "r")
	is.NoErr(err)
	is.Equal(during, []int{1, 1, 2})
	is.Equal(mon.Bridges(sandbox), 0)
	fc.onInteractive = nil

	// Detached start.
	_, _, err = runSessionCmd(t, context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true}), "start", "r", "-d")
	is.NoErr(err)
	is.Equal(mon.Bridges(sandbox), 0)

	// The attach client fails to start.
	fc.interactiveErr = errors.New("gateway hung up")
	_, _, err = runSessionCmd(t, withSess(), "attach", "r")
	is.True(err != nil)
	is.Equal(mon.Bridges(sandbox), 0)
	fc.interactiveErr = nil

	// Preparing the session fails.
	fc.ensureErr = errors.New("quota exceeded")
	_, _, err = runSessionCmd(t, withSess(), "start", "r", "-n", "other")
	is.True(err != nil)
	is.Equal(mon.Bridges(openshell.SessionSandboxName("alice", "other")), 0)
	fc.ensureErr = nil

	// No such session, and a bad name.
	_, _, err = runSessionCmd(t, withSess(), "attach", "r", "-n", "nope")
	is.True(err != nil)
	is.Equal(mon.Bridges(openshell.SessionSandboxName("alice", "nope")), 0)
	_, _, err = runSessionCmd(t, withSess(), "attach", "r", "-n", "Bad")
	is.True(err != nil)
	is.Equal(mon.Bridges(openshell.SessionSandboxName("alice", "Bad")), 0)

	// The SSH client goes away mid-bridge.
	cctx, cancel := context.WithCancel(withSess())
	fc.onInteractive = func(string) { cancel() }
	_, _, _ = runSessionCmd(t, cctx, "attach", "r")
	is.Equal(mon.Bridges(sandbox), 0)
}

// `session ls --json` lists the OpenShell providers attached to a
// session's sandbox.
func TestSessionListProviders(t *testing.T) {
	is := is.New(t)
	ctx, be, user, _ := newSessionTestContext(t)
	config.FromContext(ctx).Sessions.Profiles["claude"] = config.SessionProfile{Command: "claude", Providers: []string{"my-claude", "my-github"}}
	_, err := be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)
	noPty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true})
	_, _, err = runSessionCmd(t, noPty, "start", "r", "-d", "--profile", "claude")
	is.NoErr(err)
	_, _, err = runSessionCmd(t, noPty, "start", "r", "-d", "-n", "plain")
	is.NoErr(err)

	stdout, _, err := runSessionCmd(t, ctx, "ls", "--json")
	is.NoErr(err)
	var got []sessionJSON
	is.NoErr(json.Unmarshal([]byte(stdout), &got))
	is.Equal(len(got), 2)
	providers := map[string][]string{}
	for _, s := range got {
		providers[s.Name] = s.Providers
	}
	is.Equal(providers["r"], []string{"my-claude", "my-github"})
	is.Equal(providers["plain"], nil)
	is.True(!strings.Contains(stdout, `"providers":null`))
}
