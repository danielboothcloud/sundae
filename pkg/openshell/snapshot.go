package openshell

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/events"
	"github.com/charmbracelet/soft-serve/pkg/s3store"
	"github.com/dustin/go-humanize"
)

// S3 snapshots of /sandbox (snapshots.s3).
//
// The archive never passes through Soft Serve and the S3 keys never enter
// the sandbox: Soft Serve presigns a PUT or GET of the session's object and
// the sandbox moves the archive itself with tar, gzip and curl.
//
//   - Snapshot: one exec packs /sandbox into a gzipped tar in
//     snapshotSpoolDir (outside /sandbox) and prints its size and MD5; Soft
//     Serve checks the size against s3store.MaxPutSize, presigns a PUT that
//     signs the MD5 (so S3 refuses a corrupted upload) and the server-side
//     encryption header; a second exec uploads the file with curl and
//     deletes it. The archive is spooled rather than streamed because AWS
//     S3 refuses a PUT without a Content-Length (chunked transfer
//     encoding), and so that a tar that fails half-way never uploads a
//     truncated archive.
//   - Replacing: the upload goes straight to the session's key. S3 makes a
//     PUT visible only once it has stored all of it, so a failed or
//     interrupted upload leaves the previous snapshot as it was; no
//     temporary key or copy is needed (and a copy would have the same
//     5 GiB limit as the PUT).
//   - Restore: one exec streams curl | gzip -dc | tar -x into /sandbox and
//     fails when any of the three does. It only ever runs in a sandbox
//     created by the same call, which is deleted again when the restore
//     fails, so a half-extracted /sandbox is never used.

// Object layout: <prefix>/<user-id>/<sandbox>.tar.gz holds a session's
// snapshot; <prefix>/<user-id>/<sandbox>.parked.json marks a session parked
// to its snapshot (see ParkedSnapshot).
const (
	snapshotSuffix = ".tar.gz"
	markerSuffix   = ".parked.json"
)

// snapshotSpoolDir holds the archive between packing and uploading. It is
// outside /sandbox (so never part of a snapshot) and the directory of the
// git credentials, which is checked to be the sandbox user's.
const snapshotSpoolDir = gitCredentialDir

// softServeStateDir is Soft Serve's state in /sandbox. A newly created
// sandbox has none, unless it holds earlier work.
var softServeStateDir = path.Join(SandboxRoot, ".config/soft-serve")

// presignTTL is how long a presigned URL is valid. Each is signed just
// before the exec that uses it, and S3 checks it when the request starts.
const presignTTL = time.Hour

// maxMarkerSize bounds a parked marker.
const maxMarkerSize = 64 << 10

// SnapshotExcludes are the paths, relative to /sandbox, a snapshot leaves
// out, as tar --exclude patterns:
//
//   - osewa's runtime: the server's identity and enrolled devices
//     (~/.config/osewa), its log and holder sockets (~/.local/state/osewa),
//     the bridge's client key and the id of the osewa session. Restored
//     into another sandbox they would pin a server key that no longer
//     exists and name a session that does not run; without them Soft
//     Serve enrols the bridge again.
//   - The osewa build Soft Serve uploads (~/.local/lib/soft-serve): it is
//     uploaded again, checked against its pinned checksum.
//   - ~/.cache: the XDG cache directory, which by definition holds nothing
//     a program cannot rebuild.
//   - Credentials: SSH and GnuPG keys, netrc and git-credential stores,
//     cloud and cluster credentials, and the login tokens of gh, glab,
//     Claude Code and Codex. Soft Serve's own git token is never in
//     /sandbox (see gitCredentialFile).
var SnapshotExcludes = []string{
	"./.config/osewa",
	"./.local/state/osewa",
	"./.config/soft-serve/osewa-client",
	"./.config/soft-serve/osewa-session*",
	"./.local/lib/soft-serve",
	"./.cache",
	"./.ssh",
	"./.gnupg",
	"./.netrc",
	"./.git-credentials",
	"./.aws",
	"./.kube",
	"./.docker/config.json",
	"./.config/gh/hosts.yml",
	"./.config/glab-cli/config.yml",
	"./.claude/.credentials.json",
	"./.codex/auth.json",
}

// Snapshot scripts, run as `sh -c <script> <name> <args...>` like the
// bootstrap scripts. They need sh, tar, gzip, md5sum, mktemp, wc and curl.
const (
	snapshotPackName = "soft-serve-snapshot-pack"
	// $1: the directory to archive, $2: the spool directory, $3: the
	// largest archive allowed, then tar --exclude patterns, relative to
	// $1. Writes the gzipped tar to a new file in the spool directory and
	// prints spool=, size=, md5= (hex) and warning= lines. With GNU tar
	// (the sandbox image's) the patterns are --anchored, so ./.cache is
	// the home's cache and not a checkout's .cache; other tars match them
	// at any depth. GNU tar's exit status 1 (a file changed or vanished
	// while it was read, which a running program causes) is a warning;
	// anything else fails. Exit 3: the archive is too large.
	snapshotPackScript = `set -e
root=$1 dir=$2 max=$3
shift 3
umask 077
mkdir -p "$dir"
[ -O "$dir" ] || { echo "$dir is not owned by uid $(id -u)" >&2; exit 1; }
chmod 0700 "$dir"
rm -f "$dir"/snapshot.*
spool=$(mktemp "$dir/snapshot.XXXXXX")
trap 'rm -f "$spool" "$spool.rc" "$spool.err"' EXIT
gnu=
case $(tar --version 2>/dev/null) in *GNU*) gnu=1 ;; esac
n=$#
while [ "$n" -gt 0 ]; do set -- "$@" "--exclude=$1"; shift; n=$((n-1)); done
if [ -n "$gnu" ]; then set -- --anchored "$@"; fi
{ rc=0; tar -c -f - -C "$root" "$@" . 2>"$spool.err" || rc=$?; echo "$rc" >"$spool.rc"; } | gzip -c >"$spool"
rc=$(cat "$spool.rc")
warning=
case $rc in
0) ;;
1) if [ -n "$gnu" ]; then warning=files-changed; else tail -c 512 "$spool.err" >&2; exit 1; fi ;;
*) tail -c 512 "$spool.err" >&2; exit 1 ;;
esac
size=$(wc -c <"$spool" | tr -d ' ')
if [ "$size" -gt "$max" ]; then
  echo "the snapshot is $size bytes compressed, more than the $max bytes one upload may carry" >&2
  exit 3
fi
sum=$(md5sum <"$spool" | cut -d' ' -f1)
rm -f "$spool.rc" "$spool.err"
trap - EXIT
printf 'spool=%s\nsize=%s\nmd5=%s\nwarning=%s\n' "$spool" "$size" "$sum" "$warning"`

	snapshotUploadName = "soft-serve-snapshot-upload"
	// $1: the spooled archive, $2: the presigned PUT URL, then the signed
	// headers as "Name: value". Uploads the archive and deletes it,
	// whatever the outcome.
	snapshotUploadScript = `spool=$1 url=$2
shift 2
out=$spool.resp
trap 'rm -f "$spool" "$out"' EXIT
n=$#
while [ "$n" -gt 0 ]; do set -- "$@" -H "$1"; shift; n=$((n-1)); done
code=$(curl -sS --retry 3 --retry-delay 2 -o "$out" -w '%{http_code}' -T "$spool" "$@" "$url") || { echo "the upload failed (curl exit status $?)" >&2; exit 1; }
case $code in
2??) echo "status=$code" ;;
*) echo "S3 refused the upload: HTTP $code: $(head -c 512 "$out" 2>/dev/null)" >&2; exit 1 ;;
esac`

	snapshotRestoreName = "soft-serve-snapshot-restore"
	// $1: the presigned GET URL, $2: the directory to extract into, $3: a
	// directory for the status file. Streams the archive into tar; fails
	// when the download, gzip or tar does.
	snapshotRestoreScript = `url=$1 root=$2 dir=$3
umask 077
mkdir -p "$dir" || exit 1
st=$(mktemp "$dir/restore.XXXXXX") || exit 1
trap 'rm -f "$st"' EXIT
umask 022
{ curl -fsS --retry 3 --retry-delay 2 "$url" || echo "download (curl exit status $?)" >>"$st"; } |
  { gzip -dc || echo "decompress (gzip exit status $?)" >>"$st"; } |
  { tar -x -p -f - -C "$root" || echo "extract (tar exit status $?)" >>"$st"; }
if [ -s "$st" ]; then echo "the restore failed: $(tr '\n' ';' <"$st")" >&2; exit 1; fi
echo restored`

	snapshotProbeName = "soft-serve-snapshot-probe"
	// The paths: prints those that exist.
	snapshotProbeScript = `for p in "$@"; do if [ -e "$p" ]; then echo "$p"; fi; done`
)

// ErrSnapshotFailed is returned by End when /sandbox could not be
// snapshotted; the session was not ended.
var ErrSnapshotFailed = errors.New("the snapshot of /sandbox failed")

// SnapshotError is a failed snapshot.
type SnapshotError struct {
	Repo, Name string
	Err        error
}

func (e *SnapshotError) Error() string {
	return fmt.Sprintf("the snapshot of /sandbox failed: %v; session %s of %s was not ended. Fix the problem and end it again, or end it without a snapshot with: session end %s%s --force",
		e.Err, e.Name, e.Repo, e.Repo, nameFlag(e.Name))
}

func (e *SnapshotError) Unwrap() []error { return []error{ErrSnapshotFailed, e.Err} }

// ErrRestoreConflict is returned by Start when the session has a snapshot
// and its newly created sandbox already holds earlier work.
var ErrRestoreConflict = errors.New("the new sandbox already holds files")

// RestoreConflictError says what the new sandbox holds and which snapshot
// would replace it. Start deleted the new sandbox again; start the session
// with RestoreOver to restore over the files, or RestoreSkip to keep them.
type RestoreConflictError struct {
	Repo, Name string
	// Paths are what the new sandbox holds of earlier work.
	Paths []string
	// Snapshot is the session's snapshot.
	Snapshot SnapshotInfo
}

func (e *RestoreConflictError) Error() string {
	return fmt.Sprintf("session %s of %s has a snapshot of /sandbox (%s), but its new sandbox already holds files from earlier work (%s); restoring replaces those the snapshot also has. Start it again with --restore to restore the snapshot over them, or --fresh to keep them",
		e.Name, e.Repo, e.Snapshot.Describe(), strings.Join(e.Paths, ", "))
}

// Question is the yes/no question to ask the user.
func (e *RestoreConflictError) Question() string {
	return fmt.Sprintf("Session %s of %s has a snapshot of /sandbox (%s), but its new sandbox already holds files from earlier work: %s. Restoring replaces the files the snapshot also has and keeps the others. Restore the snapshot over them?",
		e.Name, e.Repo, e.Snapshot.Describe(), strings.Join(e.Paths, ", "))
}

func (e *RestoreConflictError) Is(target error) bool { return target == ErrRestoreConflict }

// RestoreMode says whether Start restores a session's snapshot into its
// newly created sandbox.
type RestoreMode int

const (
	// RestoreAuto restores the snapshot, unless the new sandbox already
	// holds earlier work (Soft Serve's state or the checkout): then Start
	// fails with a RestoreConflictError, for the user to choose.
	RestoreAuto RestoreMode = iota
	// RestoreOver restores the snapshot whatever the new sandbox holds
	// (session start --restore).
	RestoreOver
	// RestoreSkip never restores (session start --fresh). A session parked
	// to its snapshot starts afresh and is no longer parked.
	RestoreSkip
)

// SnapshotInfo describes a stored snapshot.
type SnapshotInfo struct {
	// URL is s3://bucket/key.
	URL  string
	Size int64
	// Time is when it was stored.
	Time time.Time
}

// Describe renders the snapshot's time and size.
func (i SnapshotInfo) Describe() string {
	if i.Time.IsZero() {
		return humanize.IBytes(uint64(max(i.Size, 0))) //nolint:gosec
	}
	return i.Time.UTC().Format("2006-01-02 15:04 MST") + ", " + humanize.IBytes(uint64(max(i.Size, 0))) //nolint:gosec
}

// ParkedSnapshot marks a session parked to its snapshot: its sandbox was
// deleted after /sandbox was saved. It is the only record of the session,
// stored as JSON next to the snapshot: Soft Serve keeps no session state,
// and the sandbox that would carry it is gone.
type ParkedSnapshot struct {
	UserID  int64  `json:"user_id"`
	User    string `json:"user"`
	Repo    string `json:"repo"`
	Session string `json:"session"`
	Sandbox string `json:"sandbox"`
	Profile string `json:"profile,omitempty"`
	// Image is the image the sandbox was created from, so it is created
	// from it again.
	Image string `json:"image,omitempty"`
	// Parked is when the session was parked.
	Parked time.Time `json:"parked"`
	// Op is who parked it: ParkAuto or ParkManual.
	Op string `json:"op"`
	// Record is the program's last status before it was stopped.
	Record events.Record `json:"record"`
	// Size is the snapshot's size.
	Size int64 `json:"size"`
}

// snapshotStore is the S3 snapshot store of snapshots.s3.
type snapshotStore struct {
	cfg   config.S3SnapshotConfig
	store *s3store.Store
	// err is why the store could not be set up; every operation returns
	// it.
	err error
	now func() time.Time
}

// newSnapshotStore returns the snapshot store, or nil when snapshots are
// off.
func newSnapshotStore(cfg *config.Config) *snapshotStore {
	if cfg == nil || !cfg.Snapshots.S3.Enabled {
		return nil
	}
	st, err := s3store.New(cfg.Snapshots.S3)
	return &snapshotStore{cfg: cfg.Snapshots.S3, store: st, err: err, now: time.Now}
}

// onEnd reports whether session end snapshots.
func (s *snapshotStore) onEnd() bool { return s != nil && s.cfg.Snapshots(config.SnapshotOnEnd) }

// onPark reports whether a memory-retaining park snapshots first.
func (s *snapshotStore) onPark() bool { return s != nil && s.cfg.Snapshots(config.SnapshotOnPark) }

func (s *snapshotStore) userPrefix(userID int64) string {
	return s.cfg.Prefix + "/" + strconv.FormatInt(userID, 10) + "/"
}

// key is the object key of a session's snapshot.
func (s *snapshotStore) key(userID int64, sandbox string) string {
	return s.userPrefix(userID) + sandbox + snapshotSuffix
}

// markerKey is the object key of a session's parked marker.
func (s *snapshotStore) markerKey(userID int64, sandbox string) string {
	return s.userPrefix(userID) + sandbox + markerSuffix
}

func (s *snapshotStore) objectURL(key string) string {
	return "s3://" + s.cfg.Bucket + "/" + key
}

// stat returns the session's snapshot; nil when it has none.
func (s *snapshotStore) stat(ctx context.Context, userID int64, sandbox string) (*SnapshotInfo, error) {
	if s.err != nil {
		return nil, s.err
	}
	key := s.key(userID, sandbox)
	o, err := s.store.Head(ctx, key)
	switch {
	case errors.Is(err, s3store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err //nolint:wrapcheck
	}
	return &SnapshotInfo{URL: s.objectURL(key), Size: o.Size, Time: o.LastModified}, nil
}

// snapshotMeta is the metadata stored with a snapshot, for whoever browses
// the bucket.
func snapshotMeta(sess events.Session) map[string]string {
	return map[string]string{
		"soft-serve-user":    url.QueryEscape(sess.User),
		"soft-serve-repo":    url.QueryEscape(sess.Repo),
		"soft-serve-session": url.QueryEscape(sess.Name),
	}
}

// take snapshots the sandbox's /sandbox to the session's object, replacing
// the previous snapshot once the new one is stored.
func (s *snapshotStore) take(ctx context.Context, client Client, userID int64, sess events.Session, progress io.Writer) (*SnapshotInfo, error) {
	if s.err != nil {
		return nil, s.err
	}
	fmt.Fprintf(progress, "Saving /sandbox to %s...\n", s.objectURL(s.key(userID, sess.Sandbox))) //nolint:errcheck
	args := append([]string{SandboxRoot, snapshotSpoolDir, strconv.FormatInt(s3store.MaxPutSize, 10)}, SnapshotExcludes...)
	out, err := runScript(ctx, client, sess.Sandbox, snapshotPackName, snapshotPackScript, nil, args...)
	if err != nil {
		return nil, err
	}
	kv := parseKeyValues(out)
	spool := kv["spool"]
	size, serr := strconv.ParseInt(kv["size"], 10, 64)
	sum, herr := hex.DecodeString(kv["md5"])
	if spool == "" || serr != nil || herr != nil || len(sum) != 16 {
		return nil, fmt.Errorf("openshell: %s: unexpected output %q", snapshotPackName, trimOutput([]byte(out)))
	}
	if kv["warning"] == "files-changed" {
		fmt.Fprintln(progress, "Warning: some files changed while they were archived; the snapshot has the version tar read") //nolint:errcheck
	}

	key := s.key(userID, sess.Sandbox)
	p, err := s.store.PresignPut(ctx, key, presignTTL, s3store.PutOptions{
		ContentMD5:  base64.StdEncoding.EncodeToString(sum),
		ContentType: "application/gzip",
		Metadata:    snapshotMeta(sess),
	})
	if err != nil {
		// The spooled archive is deleted by the next pack, or with the
		// sandbox.
		return nil, err //nolint:wrapcheck
	}
	if _, err := runScript(ctx, client, sess.Sandbox, snapshotUploadName, snapshotUploadScript, nil, append([]string{spool, p.URL}, p.HeaderLines()...)...); err != nil {
		return nil, err
	}
	info := &SnapshotInfo{URL: s.objectURL(key), Size: size, Time: s.now()}
	fmt.Fprintf(progress, "Saved /sandbox (%s).\n", humanize.IBytes(uint64(size))) //nolint:errcheck,gosec
	return info, nil
}

// restore extracts the session's snapshot into the sandbox's /sandbox.
func (s *snapshotStore) restore(ctx context.Context, client Client, userID int64, sandbox string) error {
	if s.err != nil {
		return s.err
	}
	p, err := s.store.PresignGet(ctx, s.key(userID, sandbox), presignTTL)
	if err != nil {
		return err //nolint:wrapcheck
	}
	_, err = runScript(ctx, client, sandbox, snapshotRestoreName, snapshotRestoreScript, nil, p.URL, SandboxRoot, snapshotSpoolDir)
	return err
}

// probe returns which of paths exist in the sandbox.
func probePaths(ctx context.Context, client Client, sandbox string, paths ...string) ([]string, error) {
	out, err := runScript(ctx, client, sandbox, snapshotProbeName, snapshotProbeScript, nil, paths...)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			found = append(found, l)
		}
	}
	return found, nil
}

// readMarker returns the session's parked marker; nil when it is not
// parked to its snapshot.
func (s *snapshotStore) readMarker(ctx context.Context, userID int64, sandbox string) (*ParkedSnapshot, error) {
	if s.err != nil {
		return nil, s.err
	}
	b, err := s.store.Get(ctx, s.markerKey(userID, sandbox), maxMarkerSize)
	switch {
	case errors.Is(err, s3store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err //nolint:wrapcheck
	}
	var p ParkedSnapshot
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("openshell: parked marker %s: %w", s.markerKey(userID, sandbox), err)
	}
	if p.UserID != userID || p.Sandbox != sandbox {
		return nil, fmt.Errorf("openshell: parked marker %s is for another session", s.markerKey(userID, sandbox))
	}
	return &p, nil
}

// writeMarker records the session as parked to its snapshot.
func (s *snapshotStore) writeMarker(ctx context.Context, p ParkedSnapshot) error {
	if s.err != nil {
		return s.err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("openshell: parked marker: %w", err)
	}
	return s.store.Put(ctx, s.markerKey(p.UserID, p.Sandbox), b, "application/json") //nolint:wrapcheck
}

// deleteMarker records the session as no longer parked.
func (s *snapshotStore) deleteMarker(ctx context.Context, userID int64, sandbox string) error {
	if s.err != nil {
		return s.err
	}
	return s.store.Delete(ctx, s.markerKey(userID, sandbox)) //nolint:wrapcheck
}

// listMarkers returns the user's sessions parked to their snapshots.
func (s *snapshotStore) listMarkers(ctx context.Context, userID int64) ([]ParkedSnapshot, error) {
	if s.err != nil {
		return nil, s.err
	}
	objs, err := s.store.List(ctx, s.userPrefix(userID))
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	var out []ParkedSnapshot
	for _, o := range objs {
		sandbox, ok := strings.CutSuffix(strings.TrimPrefix(o.Key, s.userPrefix(userID)), markerSuffix)
		if !ok || strings.Contains(sandbox, "/") {
			continue
		}
		p, err := s.readMarker(ctx, userID, sandbox)
		if err != nil {
			return nil, err
		}
		if p != nil {
			out = append(out, *p)
		}
	}
	return out, nil
}

// sandboxLocks serializes what snapshots and restores do to one sandbox
// within this process: a start that creates and restores it, an end that
// snapshots and deletes it, a park that snapshots and deletes it.
var sandboxLocks = &keyedLock{held: map[string]chan struct{}{}}

// keyedLock is a set of mutexes by key whose Lock gives up when its
// context is done.
type keyedLock struct {
	mu   sync.Mutex
	held map[string]chan struct{}
}

// Lock locks key and returns the function that unlocks it.
func (k *keyedLock) Lock(ctx context.Context, key string) (func(), error) {
	for {
		k.mu.Lock()
		ch, busy := k.held[key]
		if !busy {
			ch = make(chan struct{})
			k.held[key] = ch
			k.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					k.mu.Lock()
					delete(k.held, key)
					k.mu.Unlock()
					close(ch)
				})
			}, nil
		}
		k.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, fmt.Errorf("openshell: wait for another operation on sandbox %q: %w", key, ctx.Err())
		}
	}
}
