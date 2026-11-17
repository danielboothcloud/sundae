package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/sshutils"
	"github.com/matryer/is"
)

func TestSessionSnapshotCommands(t *testing.T) {
	is := is.New(t)
	ctx, _, srv, _ := newSnapshotTestContext(t, nil)
	noPty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true})

	out, _, err := runSessionCmd(t, ctx, "snapshot", "ls")
	is.NoErr(err)
	is.True(strings.Contains(out, "No snapshots found"))

	// Ending the session stores its snapshot; it is listed as ended.
	_, _, err = runSessionCmd(t, noPty, "start", "r", "-d")
	is.NoErr(err)
	_, _, err = runSessionCmd(t, ctx, "end", "r")
	is.NoErr(err)
	out, _, err = runSessionCmd(t, ctx, "snapshot", "ls")
	is.NoErr(err)
	rows := tableRows(out)
	is.Equal(len(rows), 1)
	is.True(strings.Contains(rows[0], "r / r | ended")) // got: rows[0]
	out, _, err = runSessionCmd(t, ctx, "snapshot", "ls", "--json")
	is.NoErr(err)
	var js []openshell.StoredSnapshot
	is.NoErr(json.Unmarshal([]byte(out), &js))
	is.Equal(len(js), 1)
	is.Equal(js[0].State, openshell.SnapshotEnded)
	is.Equal(js[0].URL, "s3://snapshots/"+cmdSnapKey)

	// prune --dry-run lists it and keeps it; --older-than a day keeps it.
	out, stderr, err := runSessionCmd(t, ctx, "snapshot", "prune", "--dry-run")
	is.NoErr(err)
	is.True(strings.Contains(out, "Would delete the snapshot of r / r")) // got: out
	is.True(strings.Contains(stderr, "Would delete 1 snapshot(s)"))
	out, _, err = runSessionCmd(t, ctx, "snapshot", "prune", "--older-than", "24h")
	is.NoErr(err)
	is.True(strings.Contains(out, "No snapshots to prune"))
	_, ok := srv.Object(cmdSnapKey)
	is.True(ok)

	// rm deletes it; again, there is none.
	_, stderr, err = runSessionCmd(t, ctx, "snapshot", "rm", "r")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Deleted the snapshot of r")) // got: stderr
	_, ok = srv.Object(cmdSnapKey)
	is.True(!ok)
	_, _, err = runSessionCmd(t, ctx, "snapshot", "rm", "r")
	is.True(err != nil && strings.Contains(err.Error(), "no snapshot"))

	// end --delete-snapshot takes none and deletes the earlier one.
	_, _, err = runSessionCmd(t, noPty, "start", "r", "-d")
	is.NoErr(err)
	_, _, err = runSessionCmd(t, ctx, "end", "r")
	is.NoErr(err)
	_, _, err = runSessionCmd(t, noPty, "start", "r", "-d", "--fresh")
	is.NoErr(err)
	_, stderr, err = runSessionCmd(t, ctx, "end", "r", "--delete-snapshot")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Session ended; its S3 snapshot")) // got: stderr
	is.True(!strings.Contains(stderr, "Saving /sandbox"))
	_, ok = srv.Object(cmdSnapKey)
	is.True(!ok)

	// prune deletes ended sessions' snapshots.
	_, _, err = runSessionCmd(t, noPty, "start", "r", "-d")
	is.NoErr(err)
	_, _, err = runSessionCmd(t, ctx, "end", "r")
	is.NoErr(err)
	out, _, err = runSessionCmd(t, ctx, "snapshot", "prune")
	is.NoErr(err)
	is.True(strings.Contains(out, "Deleted the snapshot of r / r"))
	_, ok = srv.Object(cmdSnapKey)
	is.True(!ok)
}

func TestSessionSnapshotOff(t *testing.T) {
	is := is.New(t)
	ctx, _, _, _ := newSessionTestContext(t)
	_, _, err := runSessionCmd(t, ctx, "snapshot", "ls")
	is.True(err != nil && strings.Contains(err.Error(), "S3 snapshots are off")) // got: err
}
