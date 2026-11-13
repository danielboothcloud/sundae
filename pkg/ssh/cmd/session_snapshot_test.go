package cmd

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/s3store/s3test"
	"github.com/danielboothcloud/sundae/pkg/sshutils"
	"github.com/matryer/is"
)

// snapshotScripts answers the snapshot scripts for fakeOpenShell: the
// archive is the sandbox name, uploaded and downloaded over HTTP with the
// presigned URL, as curl would. conflict makes the probe report earlier
// work.
type snapshotScripts struct {
	conflict bool
	restored []string
}

func (s *snapshotScripts) run(name string, command []string) (*openshell.ExecResult, bool) {
	if len(command) < 4 || command[0] != "sh" {
		return nil, false
	}
	a := command[4:]
	fail := func(msg string) (*openshell.ExecResult, bool) {
		return &openshell.ExecResult{ExitCode: 1, Stderr: []byte(msg)}, true
	}
	switch command[3] {
	case "sundae-snapshot-probe":
		if s.conflict {
			return &openshell.ExecResult{Stdout: []byte(a[0] + "\n")}, true
		}
		return &openshell.ExecResult{}, true
	case "sundae-snapshot-pack":
		sum := md5.Sum([]byte("archive of " + name)) //nolint:gosec
		return &openshell.ExecResult{Stdout: fmt.Appendf(nil, "spool=/tmp/sundae/snapshot.x\nsize=%d\nmd5=%s\nwarning=\n", len("archive of "+name), hex.EncodeToString(sum[:]))}, true
	case "sundae-snapshot-upload":
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, a[1], strings.NewReader("archive of "+name))
		for _, h := range a[2:] {
			k, v, _ := strings.Cut(h, ": ")
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fail(err.Error())
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != http.StatusOK {
			return fail(fmt.Sprintf("S3 refused the upload: HTTP %d: %s", resp.StatusCode, body))
		}
		return &openshell.ExecResult{Stdout: []byte("status=200\n")}, true
	case "sundae-snapshot-restore":
		resp, err := http.Get(a[0]) //nolint:noctx
		if err != nil {
			return fail(err.Error())
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != http.StatusOK {
			return fail("the restore failed: download (curl exit status 22);")
		}
		s.restored = append(s.restored, string(body))
		return &openshell.ExecResult{Stdout: []byte("restored\n")}, true
	}
	return nil, false
}

// newSnapshotTestContext is newSessionTestContext with S3 snapshots on,
// against a fake S3 server.
func newSnapshotTestContext(t *testing.T, configure func(*config.Config)) (context.Context, *fakeOpenShell, *s3test.Server, *snapshotScripts) {
	t.Helper()
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	_, err := be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)
	srv := s3test.New(t)
	cfg := config.FromContext(ctx)
	cfg.Snapshots.S3 = srv.Config(t, t.TempDir())
	if configure != nil {
		configure(cfg)
	}
	scripts := &snapshotScripts{}
	fc.scripts = scripts.run
	ctx, _ = withMonitor(ctx, fc)
	return ctx, fc, srv, scripts
}

// alice is user 2: the admin is 1.
var (
	cmdSandbox   = openshell.SessionSandboxName("alice", "r")
	cmdSnapKey   = "sundae/sessions/2/" + cmdSandbox + ".tar.gz"
	cmdMarkerKey = "sundae/sessions/2/" + cmdSandbox + ".parked.json"
)

func TestSessionEndSnapshotAndForce(t *testing.T) {
	is := is.New(t)
	ctx, fc, srv, _ := newSnapshotTestContext(t, nil)
	noPty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true})
	_, _, err := runSessionCmd(t, noPty, "start", "r", "-d")
	is.NoErr(err)

	// The snapshot fails: the session is not ended.
	srv.SetFail(func(r *http.Request, _ string) int {
		if r.Method == http.MethodPut {
			return http.StatusInsufficientStorage
		}
		return 0
	})
	_, _, err = runSessionCmd(t, ctx, "end", "r")
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "the snapshot of /sandbox failed")) // got: err
	is.True(strings.Contains(err.Error(), "session end r --force"))
	is.Equal(fc.phase(openshell.SessionSandboxName("alice", "r")), types.SandboxReady)

	// --force ends it without one.
	_, stderr, err := runSessionCmd(t, ctx, "end", "r", "--force")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "ending the session without it (--force)"))
	is.True(strings.Contains(stderr, "Session ended"))
	is.Equal(fc.phase(openshell.SessionSandboxName("alice", "r")), types.SandboxPhase(""))

	// With S3 back, end saves /sandbox and start restores it.
	srv.SetFail(nil)
	_, _, err = runSessionCmd(t, noPty, "start", "r", "-d")
	is.NoErr(err)
	_, stderr, err = runSessionCmd(t, ctx, "end", "r")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Session ended; /sandbox is saved in s3://snapshots/"+cmdSnapKey)) // got: stderr
	o, ok := srv.Object(cmdSnapKey)
	is.True(ok)
	is.Equal(string(o.Body), "archive of "+cmdSandbox)

	_, stderr, err = runSessionCmd(t, noPty, "start", "r", "-d")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Restored /sandbox from s3://snapshots/"+cmdSnapKey)) // got: stderr

	// --fresh and --restore are exclusive.
	_, _, err = runSessionCmd(t, noPty, "start", "r", "-d", "--fresh", "--restore")
	is.True(err != nil && strings.Contains(err.Error(), "mutually exclusive"))
}

func TestSessionStartRestorePrompt(t *testing.T) {
	is := is.New(t)
	ctx, fc, srv, scripts := newSnapshotTestContext(t, nil)
	srv.PutObject(cmdSnapKey, []byte("archive of "+cmdSandbox), nil)
	scripts.conflict = true
	tty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{})
	noPty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true})

	run := func(ctx context.Context, input string, args ...string) (string, error) {
		c := SessionCommand()
		var out, errb bytes.Buffer
		c.SetOut(&out)
		c.SetErr(&errb)
		c.SetIn(strings.NewReader(input))
		c.SetArgs(args)
		err := c.ExecuteContext(ctx)
		return errb.String(), err
	}

	// Without a terminal, the error says what to do.
	_, err := run(noPty, "", "start", "r", "-d")
	is.True(err != nil && strings.Contains(err.Error(), "--restore") && strings.Contains(err.Error(), "--fresh")) // got: err
	is.Equal(fc.phase(openshell.SessionSandboxName("alice", "r")), types.SandboxPhase(""))

	// No: nothing is started.
	stderr, err := run(tty, "n\r", "start", "r", "-d")
	is.True(err != nil)
	is.True(strings.Contains(stderr, "Restore the snapshot over them? [y/N] n\r\n")) // got: stderr
	is.Equal(len(scripts.restored), 0)
	is.Equal(fc.phase(openshell.SessionSandboxName("alice", "r")), types.SandboxPhase(""))

	// Ctrl-C cancels.
	_, err = run(tty, "\x03", "start", "r", "-d")
	is.True(err != nil && strings.Contains(err.Error(), "cancelled"))

	// Yes (with a typo fixed): restored over the files.
	stderr, err = run(tty, "yx\x7fes\r", "start", "r", "-d")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Restored /sandbox from s3://snapshots/"+cmdSnapKey)) // got: stderr
	is.Equal(scripts.restored, []string{"archive of " + cmdSandbox})
	is.Equal(fc.phase(openshell.SessionSandboxName("alice", "r")), types.SandboxReady)
}

func TestSessionParkToSnapshotAndList(t *testing.T) {
	is := is.New(t)
	ctx, fc, srv, scripts := newSnapshotTestContext(t, func(c *config.Config) {
		c.Sessions.Suspend.Fallback = config.SuspendFallbackSnapshot
	})
	noPty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true})
	_, _, err := runSessionCmd(t, noPty, "start", "r", "-d")
	is.NoErr(err)

	fc.suspendErr = &types.StatusError{Code: types.ErrorConflict, GRPCCode: 9, Message: "compute driver does not support sandbox suspend"}
	_, stderr, err := runSessionCmd(t, ctx, "park", "r")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "parked to an S3 snapshot: its sandbox was deleted and its program stopped")) // got: stderr
	is.Equal(fc.phase(openshell.SessionSandboxName("alice", "r")), types.SandboxPhase(""))
	_, ok := srv.Object(cmdMarkerKey)
	is.True(ok)
	_, stderr, err = runSessionCmd(t, ctx, "park", "r")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "is already parked"))

	out, _, err := runSessionCmd(t, ctx, "ls")
	is.NoErr(err)
	rows := tableRows(out)
	is.Equal(len(rows), 1)
	is.True(strings.Contains(rows[0], "Snapshot | · none · parked (snapshot) | program stopped by the snapshot park; attach restarts it")) // got: rows[0]

	out, _, err = runSessionCmd(t, ctx, "ls", "--json")
	is.NoErr(err)
	var js []sessionJSON
	is.NoErr(json.Unmarshal([]byte(out), &js))
	is.Equal(len(js), 1)
	is.Equal(js[0].Phase, "Snapshot")
	is.True(js[0].Parked)
	is.Equal(js[0].Park, "snapshot")
	is.True(js[0].SnapshotSize > 0)

	// Resume recreates, restores and restarts the program.
	_, stderr, err = runSessionCmd(t, noPty, "resume", "r")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "was parked to a snapshot of /sandbox"))
	is.True(strings.Contains(stderr, "Started bash -l again: it was not running when the session resumed (the session was parked to a snapshot")) // got: stderr
	is.Equal(scripts.restored, []string{"archive of " + cmdSandbox})
	is.Equal(fc.phase(openshell.SessionSandboxName("alice", "r")), types.SandboxReady)
	_, ok = srv.Object(cmdMarkerKey)
	is.True(!ok)

	out, _, err = runSessionCmd(t, ctx, "ls", "--json")
	is.NoErr(err)
	js = nil
	is.NoErr(json.Unmarshal([]byte(out), &js))
	is.Equal(js[0].Park, "")
}
