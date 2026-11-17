package openshell

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/danielboothcloud/sundae/pkg/proto"
)

// What became of the session of a stored snapshot.
const (
	// SnapshotLive: the session has a sandbox. Its snapshot is the last
	// one taken (on end, on park, or by `session snapshot`); the session's
	// work is in its sandbox.
	SnapshotLive = "live"
	// SnapshotParked: the session is parked to the snapshot. It is the
	// session's only copy of /sandbox, so it is never deleted on its own.
	SnapshotParked = "parked"
	// SnapshotEnded: the session has ended. Starting a session of the same
	// name restores the snapshot, unless --fresh.
	SnapshotEnded = "ended"
)

// ErrSnapshotsOff is returned by the snapshot commands when S3 snapshots
// are not configured (snapshots.s3).
var ErrSnapshotsOff = errors.New("S3 snapshots are off (snapshots.s3 is not enabled)")

// ErrNoSnapshot is returned when a session has no stored snapshot.
var ErrNoSnapshot = errors.New("no snapshot")

// ErrSnapshotParked is returned when deleting the snapshot a session is
// parked to: it is the session's only copy of /sandbox.
var ErrSnapshotParked = errors.New("the session is parked to this snapshot, its only copy of /sandbox")

// StoredSnapshot is one of a user's stored snapshots.
type StoredSnapshot struct {
	// Sandbox is the sandbox name the snapshot is stored under.
	Sandbox string `json:"sandbox"`
	// Session and Repo are the session's name and first repository, from
	// the snapshot's metadata; empty for a snapshot without it.
	Session string `json:"session,omitempty"`
	Repo    string `json:"repo,omitempty"`
	// State is SnapshotLive, SnapshotParked or SnapshotEnded.
	State string `json:"state"`
	// URL is s3://bucket/key.
	URL  string    `json:"url"`
	Size int64     `json:"size"`
	Time time.Time `json:"time"`
}

// Info returns the snapshot's location, size and time.
func (s StoredSnapshot) Info() SnapshotInfo { return SnapshotInfo{URL: s.URL, Size: s.Size, Time: s.Time} }

// Label names the snapshot's session: "repo / session", the session alone,
// or the sandbox when the snapshot has no metadata.
func (s StoredSnapshot) Label() string {
	switch {
	case s.Session == "":
		return s.Sandbox
	case s.Repo == "":
		return s.Session
	}
	return s.Repo + " / " + s.Session
}

// Snapshots returns the user's stored snapshots, newest first, with what
// became of each one's session.
func (m *Manager) Snapshots(ctx context.Context, user proto.User) ([]StoredSnapshot, error) {
	if user == nil {
		return nil, proto.ErrUnauthorized
	}
	if m.snaps == nil {
		return nil, ErrSnapshotsOff
	}
	if m.snaps.err != nil {
		return nil, m.snaps.err
	}
	prefix := m.snaps.userPrefix(user.ID())
	objs, err := m.snaps.store.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("openshell: list snapshots: %w", err)
	}
	parked := map[string]bool{}
	for _, o := range objs {
		if sb, ok := strings.CutSuffix(strings.TrimPrefix(o.Key, prefix), markerSuffix); ok {
			parked[sb] = true
		}
	}
	var out []StoredSnapshot
	for _, o := range objs {
		sandbox, ok := strings.CutSuffix(strings.TrimPrefix(o.Key, prefix), snapshotSuffix)
		if !ok || sandbox == "" || strings.Contains(sandbox, "/") {
			continue
		}
		s := StoredSnapshot{Sandbox: sandbox, URL: m.snaps.objectURL(o.Key), Size: o.Size, Time: o.LastModified}
		if h, err := m.snaps.store.Head(ctx, o.Key); err == nil {
			s.Session = unescapeMeta(h.Metadata["sundae-session"])
			s.Repo = unescapeMeta(h.Metadata["sundae-repo"])
		}
		if s.State, err = m.snapshotState(ctx, user, sandbox, parked[sandbox]); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	slices.SortStableFunc(out, func(a, b StoredSnapshot) int { return b.Time.Compare(a.Time) })
	return out, nil
}

// snapshotState returns what became of the session of sandbox's snapshot.
func (m *Manager) snapshotState(ctx context.Context, user proto.User, sandbox string, parked bool) (string, error) {
	if parked {
		return SnapshotParked, nil
	}
	sb, err := m.client.GetSandbox(ctx, sandbox)
	switch {
	case IsNotFound(err):
		return SnapshotEnded, nil
	case err != nil:
		return "", fmt.Errorf("openshell: get sandbox %q: %w", sandbox, err)
	case !owns(sb, ownerLabels(user)):
		// Not the user's sandbox: their session of that name has ended.
		return SnapshotEnded, nil
	}
	return SnapshotLive, nil
}

// unescapeMeta decodes a snapshot metadata value (see snapshotMeta).
func unescapeMeta(v string) string {
	if s, err := url.QueryUnescape(v); err == nil {
		return s
	}
	return v
}

// DeleteSnapshot deletes the stored snapshot of the user's session name.
// It refuses, with ErrSnapshotParked, the snapshot a session is parked to;
// ErrNoSnapshot when there is none.
func (m *Manager) DeleteSnapshot(ctx context.Context, user proto.User, name string) (StoredSnapshot, error) {
	if user == nil {
		return StoredSnapshot{}, proto.ErrUnauthorized
	}
	if m.snaps == nil {
		return StoredSnapshot{}, ErrSnapshotsOff
	}
	name, err := sessionName(name)
	if err != nil {
		return StoredSnapshot{}, err
	}
	sandbox := SessionSandboxName(user.Username(), name)
	unlock, err := sandboxLocks.Lock(ctx, sandbox)
	if err != nil {
		return StoredSnapshot{}, err
	}
	defer unlock()
	return m.deleteSnapshotLocked(ctx, user, sandbox, name)
}

// deleteSnapshotLocked deletes sandbox's snapshot; the caller holds its
// lock.
func (m *Manager) deleteSnapshotLocked(ctx context.Context, user proto.User, sandbox, name string) (StoredSnapshot, error) {
	if p, err := m.snaps.readMarker(ctx, user.ID(), sandbox); err != nil {
		return StoredSnapshot{}, err
	} else if p != nil {
		return StoredSnapshot{}, fmt.Errorf("session %s: %w; end it with \"session end %s --delete-snapshot\"", name, ErrSnapshotParked, name)
	}
	info, err := m.snaps.stat(ctx, user.ID(), sandbox)
	if err != nil {
		return StoredSnapshot{}, err
	}
	if info == nil {
		return StoredSnapshot{}, fmt.Errorf("session %s: %w", name, ErrNoSnapshot)
	}
	state, err := m.snapshotState(ctx, user, sandbox, false)
	if err != nil {
		return StoredSnapshot{}, err
	}
	if err := m.snaps.store.Delete(ctx, m.snaps.key(user.ID(), sandbox)); err != nil {
		return StoredSnapshot{}, fmt.Errorf("openshell: delete snapshot: %w", err)
	}
	return StoredSnapshot{Sandbox: sandbox, Session: name, State: state, URL: info.URL, Size: info.Size, Time: info.Time}, nil
}

// PruneOptions selects the snapshots PruneSnapshots deletes.
type PruneOptions struct {
	// OlderThan keeps snapshots stored less than this long ago; 0 prunes
	// them whatever their age.
	OlderThan time.Duration
	// DryRun reports what would be deleted without deleting it.
	DryRun bool
}

// PruneSnapshots deletes the user's snapshots of ended sessions (those
// with neither a sandbox nor a parked marker), older than o.OlderThan, and
// returns them. Snapshots of live and parked sessions are kept.
func (m *Manager) PruneSnapshots(ctx context.Context, user proto.User, o PruneOptions) ([]StoredSnapshot, error) {
	all, err := m.Snapshots(ctx, user)
	if err != nil {
		return nil, err
	}
	now := m.now()
	var pruned []StoredSnapshot
	for _, s := range all {
		if s.State != SnapshotEnded || (o.OlderThan > 0 && now.Sub(s.Time) < o.OlderThan) {
			continue
		}
		if o.DryRun {
			pruned = append(pruned, s)
			continue
		}
		del, err := m.pruneOne(ctx, user, s)
		if err != nil {
			return pruned, err
		}
		if del {
			pruned = append(pruned, s)
		}
	}
	return pruned, nil
}

// pruneOne deletes the snapshot of an ended session, checking again under
// the sandbox's lock that the session is still ended: one started since
// the listing keeps its snapshot. It reports whether it deleted it.
func (m *Manager) pruneOne(ctx context.Context, user proto.User, s StoredSnapshot) (bool, error) {
	unlock, err := sandboxLocks.Lock(ctx, s.Sandbox)
	if err != nil {
		return false, err
	}
	defer unlock()
	p, err := m.snaps.readMarker(ctx, user.ID(), s.Sandbox)
	if err != nil {
		return false, err
	}
	state, err := m.snapshotState(ctx, user, s.Sandbox, p != nil)
	if err != nil || state != SnapshotEnded {
		return false, err
	}
	if err := m.snaps.store.Delete(ctx, m.snaps.key(user.ID(), s.Sandbox)); err != nil {
		return false, fmt.Errorf("openshell: delete snapshot of %s: %w", s.Label(), err)
	}
	return true, nil
}
