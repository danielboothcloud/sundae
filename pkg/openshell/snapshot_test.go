package openshell

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5" //nolint:gosec
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/fake"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/events"
	"github.com/charmbracelet/soft-serve/pkg/s3store"
	"github.com/charmbracelet/soft-serve/pkg/s3store/s3test"
)

// fakeSnapshotKnobs configures the fake's snapshot scripts.
type fakeSnapshotKnobs struct {
	// packExit makes the pack script fail with this status.
	packExit int
	// packWarning is the pack script's warning= value.
	packWarning string
	// curlExit makes the upload or download fail as curl would.
	curlExit int
}

// excluded reports whether a path relative to /sandbox ("./x/y") is left
// out by one of the tar --exclude patterns: a pattern matches the path or
// a leading directory of it, and a trailing "*" matches any suffix.
func excluded(rel string, patterns []string) bool {
	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(rel, prefix) {
				return true
			}
			continue
		}
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// snapshotScript simulates the snapshot scripts against the fake sandbox
// box: pack archives its files and directories under /sandbox with tar,
// upload and restore talk HTTP to the URL they are given, as curl does.
// Call with f.mu held.
func (f *fakeExec) snapshotScript(b *fakeSandbox, call execCall) (*types.ExecResult, error) {
	a := call.args()
	switch call.script() {
	case snapshotProbeName:
		var out []string
		for _, p := range a {
			if b.dirs[p] || b.files[p] != "" {
				out = append(out, p)
				continue
			}
			for k := range b.files {
				if strings.HasPrefix(k, p+"/") {
					out = append(out, p)
					break
				}
			}
		}
		return ok(strings.Join(out, "\n"))

	case snapshotPackName:
		if f.snap.packExit != 0 {
			return fail(f.snap.packExit, "tar: ./r: Cannot open: Permission denied")
		}
		root, dir, patterns := a[0], a[1], a[3:]
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		var dirs, files []string
		for d := range b.dirs {
			if strings.HasPrefix(d, root+"/") {
				dirs = append(dirs, d)
			}
		}
		for p := range b.files {
			if strings.HasPrefix(p, root+"/") {
				files = append(files, p)
			}
		}
		slices.Sort(dirs)
		slices.Sort(files)
		for _, d := range dirs {
			rel := "." + strings.TrimPrefix(d, root)
			if !excluded(rel, patterns) {
				_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: rel + "/", Mode: 0o755})
			}
		}
		for _, p := range files {
			rel := "." + strings.TrimPrefix(p, root)
			if excluded(rel, patterns) {
				continue
			}
			_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: rel, Mode: 0o644, Size: int64(len(b.files[p]))})
			_, _ = tw.Write([]byte(b.files[p]))
		}
		_ = tw.Close()
		_ = zw.Close()
		spool := dir + "/snapshot.abc123"
		b.files[spool] = buf.String()
		sum := md5.Sum(buf.Bytes()) //nolint:gosec
		return ok(fmt.Sprintf("spool=%s\nsize=%d\nmd5=%s\nwarning=%s\n", spool, buf.Len(), hex.EncodeToString(sum[:]), f.snap.packWarning))

	case snapshotUploadName:
		spool, url := a[0], a[1]
		body := b.files[spool]
		delete(b.files, spool)
		if f.snap.curlExit != 0 {
			return fail(1, fmt.Sprintf("the upload failed (curl exit status %d)", f.snap.curlExit))
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, url, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		for _, h := range a[2:] {
			k, v, _ := strings.Cut(h, ": ")
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fail(1, "curl: (7) Failed to connect")
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close() //nolint:errcheck
		if resp.StatusCode/100 != 2 {
			return fail(1, fmt.Sprintf("S3 refused the upload: HTTP %d: %s", resp.StatusCode, rb))
		}
		return ok(fmt.Sprintf("status=%d\n", resp.StatusCode))

	case snapshotRestoreName:
		url, root := a[0], a[1]
		if f.snap.curlExit != 0 {
			return fail(1, fmt.Sprintf("the restore failed: download (curl exit status %d);", f.snap.curlExit))
		}
		resp, err := http.Get(url) //nolint:noctx
		if err != nil {
			return fail(1, "the restore failed: download (curl exit status 7);")
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != http.StatusOK {
			return fail(1, "the restore failed: download (curl exit status 22);")
		}
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return fail(1, "the restore failed: decompress (gzip exit status 1);")
		}
		tr := tar.NewReader(zr)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fail(1, "the restore failed: extract (tar exit status 2);")
			}
			p := path.Join(root, hdr.Name)
			if hdr.Typeflag == tar.TypeDir {
				b.dirs[p] = true
				continue
			}
			data, _ := io.ReadAll(tr)
			b.files[p] = string(data)
		}
		return ok("restored\n")
	}
	return nil, fmt.Errorf("fakeExec: unexpected snapshot script %q", call.script())
}

// snapHarness is a session harness with S3 snapshots on, a fake S3 server,
// a Monitor, and an event subscription.
type snapHarness struct {
	*harness
	s3  *s3test.Server
	mon *Monitor
	sub *events.Subscription
	cfg *config.Config
}

func newSnapHarness(t *testing.T, configure func(*config.Config), suspend bool) *snapHarness {
	t.Helper()
	srv := s3test.New(t)
	cfg := newHarnessConfig()
	cfg.DataPath = t.TempDir()
	cfg.Snapshots.S3 = srv.Config(t, t.TempDir())
	if configure != nil {
		configure(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	fc := fake.NewClient()
	t.Cleanup(func() { _ = fc.Close() })
	var sandboxes v1.SandboxInterface = fc.Sandboxes()
	if !suspend {
		sandboxes = noSuspend{fc.Sandboxes()}
	}
	ex := newFakeExec()
	client := newSDKClient("default", sandboxes, ex, fc.Files(), nil)
	bus := events.New(events.WithLogger(log.New(io.Discard)))
	t.Cleanup(bus.Close)
	mon := NewMonitor(func() (Client, error) { return client, nil }, cfg,
		WithMonitorEvents(bus), WithMonitorLogger(log.New(io.Discard)))
	tokens := newFakeTokens()
	mgr := NewManager(client, tokens, cfg, WithEvents(bus), WithMonitor(mon))
	mgr.logger = log.New(io.Discard)
	return &snapHarness{
		harness: &harness{fc: fc, exec: ex, tokens: tokens, mgr: mgr},
		s3:      srv,
		mon:     mon,
		sub:     bus.Subscribe(events.Buffer(128)),
		cfg:     cfg,
	}
}

// lifecycles returns the lifecycle events published so far, as
// "step[ op][ snapshot]".
func (h *snapHarness) lifecycles() []string {
	var out []string
	for {
		select {
		case ev := <-h.sub.C:
			if l := ev.Lifecycle; l != nil {
				s := string(l.Step)
				if l.Op != "" {
					s += " " + l.Op
				}
				if l.Snapshot {
					s += " snapshot"
				}
				out = append(out, s)
			}
		default:
			return out
		}
	}
}

func (h *snapHarness) exists(t *testing.T, sandbox string) bool {
	t.Helper()
	_, err := h.fc.Sandboxes().Get(context.Background(), "default", sandbox)
	if err != nil && !IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// archive returns the paths in the stored snapshot of sandbox.
func (h *snapHarness) archive(t *testing.T, key string) map[string]string {
	t.Helper()
	o, ok := h.s3.Object(key)
	if !ok {
		t.Fatalf("no object %s; have %v", key, h.s3.Keys())
	}
	zr, err := gzip.NewReader(bytes.NewReader(o.Body))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		out[hdr.Name] = string(data)
	}
}

const snapKey = "soft-serve/sessions/1/soft-alice-r.tar.gz"

// work puts files into the session's sandbox: the checkout, osewa state and
// credential-like files.
func work(h *snapHarness, sandbox string) {
	h.exec.mu.Lock()
	defer h.exec.mu.Unlock()
	b := h.exec.box(sandbox)
	b.files["/sandbox/r/main.go"] = "package main // unpushed\n"
	b.files["/sandbox/.bashrc"] = "# mine\n"
	b.files["/sandbox/.config/osewa/server.key"] = "server key"
	b.files["/sandbox/.local/state/osewa/holder.sock"] = "socket"
	b.files["/sandbox/.config/soft-serve/osewa-session"] = "osewa1\n"
	b.files["/sandbox/.config/soft-serve/osewa-session.lock"] = ""
	b.files["/sandbox/.config/soft-serve/osewa-client/client.key"] = "client key"
	b.files["/sandbox/.local/lib/soft-serve/osewa-0.1.4"] = "binary"
	b.files["/sandbox/.ssh/id_ed25519"] = "PRIVATE KEY"
	b.files["/sandbox/.cache/go-build/x"] = "cache"
	b.files["/sandbox/.claude/.credentials.json"] = "{}"
	b.files["/sandbox/.claude/settings.json"] = "{}"
	b.files["/tmp/soft-serve/gitconfig"] = "token"
}

// freshVolume forgets what the fake kept of a deleted sandbox: the next
// sandbox of that name starts from the image.
func freshVolume(h *snapHarness, sandbox string) {
	h.exec.mu.Lock()
	defer h.exec.mu.Unlock()
	delete(h.exec.boxes, sandbox)
	h.exec.box(sandbox).files["/sandbox/.bashrc"] = "# image skeleton\n"
}

func TestSnapshotKeys(t *testing.T) {
	h := newSnapHarness(t, func(c *config.Config) { c.Snapshots.S3.Prefix = "team/snaps" }, false)
	s := h.mgr.snaps
	if got := s.key(42, "soft-bob-x--1a2b3c4d"); got != "team/snaps/42/soft-bob-x--1a2b3c4d.tar.gz" {
		t.Fatalf("key = %s", got)
	}
	if got := s.markerKey(42, "soft-bob-x"); got != "team/snaps/42/soft-bob-x.parked.json" {
		t.Fatalf("marker = %s", got)
	}
	if got := s.objectURL(s.key(1, "sb")); got != "s3://snapshots/team/snaps/1/sb.tar.gz" {
		t.Fatalf("url = %s", got)
	}
	// Off: no store, nothing taken.
	if newSnapshotStore(config.DefaultConfig()) != nil {
		t.Fatal("store without snapshots.s3.enabled")
	}
}

func TestEndSnapshotsThenDeletes(t *testing.T) {
	ctx := context.Background()
	h := newSnapHarness(t, func(c *config.Config) { c.Snapshots.S3.SSE = config.SSES3 }, false)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	work(h, s.Sandbox)
	h.lifecycles()

	var progress bytes.Buffer
	res, err := h.mgr.EndSession(ctx, alice, "r", "", EndOptions{Progress: &progress})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Existed || res.Snapshot == nil || res.Snapshot.URL != "s3://snapshots/"+snapKey || res.Snapshot.Size == 0 {
		t.Fatalf("result = %+v", res)
	}
	if h.exists(t, s.Sandbox) {
		t.Fatal("sandbox not deleted")
	}
	if got := strings.Join(h.lifecycles(), ","); got != "snapshotted end,ended" {
		t.Fatalf("events = %s", got)
	}
	if !strings.Contains(progress.String(), "Saving /sandbox to s3://snapshots/"+snapKey) {
		t.Fatalf("progress = %q", progress.String())
	}

	// What is captured: the checkout and the home, without osewa's state,
	// the uploaded osewa, caches and credentials.
	files := h.archive(t, snapKey)
	for _, want := range []string{"./r/main.go", "./.bashrc", "./.claude/settings.json"} {
		if _, ok := files[want]; !ok {
			t.Errorf("snapshot lacks %s: %v", want, files)
		}
	}
	for name := range files {
		if strings.Contains(name, "osewa") || strings.Contains(name, ".ssh") || strings.Contains(name, ".cache") ||
			strings.Contains(name, "credentials") || strings.Contains(name, "gitconfig") {
			t.Errorf("snapshot has %s", name)
		}
	}
	o, _ := h.s3.Object(snapKey)
	if o.SSE != "AES256" || o.Metadata["soft-serve-repo"] != "r" || o.Metadata["soft-serve-session"] != "main" || o.ContentType != "application/gzip" {
		t.Fatalf("object = %+v", o)
	}
	// The upload was presigned, the archive went straight from the
	// sandbox, and no exec carried the S3 keys.
	for _, r := range h.s3.Requests() {
		if r.Method == http.MethodPut && r.Key == snapKey && !r.Presigned {
			t.Fatal("snapshot uploaded with a header-signed request")
		}
	}
	for _, r := range h.exec.runs {
		all := strings.Join(r.command, " ")
		for _, v := range r.opts.Env {
			all += v
		}
		if strings.Contains(all, s3test.SecretAccessKey) {
			t.Fatalf("exec %s carries the S3 secret", r.script())
		}
	}
	if scripts := h.exec.scripts(s.Sandbox); !slices.Contains(scripts, snapshotPackName) || !slices.Contains(scripts, snapshotUploadName) {
		t.Fatalf("scripts = %v", scripts)
	}
	// The spooled archive is gone.
	if _, ok := h.exec.box(s.Sandbox).files[snapshotSpoolDir+"/snapshot.abc123"]; ok {
		t.Fatal("spool left behind")
	}
}

func TestEndSnapshotFailureKeepsSession(t *testing.T) {
	ctx := context.Background()
	h := newSnapHarness(t, nil, false)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	h.s3.PutObject(snapKey, []byte("previous snapshot"), nil)
	h.lifecycles()

	// S3 refuses the upload: the session is not ended, and the previous
	// snapshot is untouched.
	h.s3.SetFail(func(r *http.Request, key string) int {
		if r.Method == http.MethodPut && key == snapKey {
			return http.StatusServiceUnavailable
		}
		return 0
	})
	existed, err := h.mgr.End(ctx, alice, "r", "")
	if !errors.Is(err, ErrSnapshotFailed) || existed {
		t.Fatalf("end = %v, %v", existed, err)
	}
	if !strings.Contains(err.Error(), "HTTP 503") || !strings.Contains(err.Error(), "session end r --force") {
		t.Fatalf("err = %v", err)
	}
	if !h.exists(t, s.Sandbox) {
		t.Fatal("sandbox deleted after a failed snapshot")
	}
	if len(h.tokens.named(TokenName(s.Sandbox))) == 0 {
		t.Fatal("tokens revoked after a failed snapshot")
	}
	if o, _ := h.s3.Object(snapKey); string(o.Body) != "previous snapshot" {
		t.Fatalf("previous snapshot replaced: %q", o.Body)
	}
	if got := strings.Join(h.lifecycles(), ","); got != "failed snapshot" {
		t.Fatalf("events = %s", got)
	}

	// The pack fails (an unreadable file): same.
	h.s3.SetFail(nil)
	h.exec.snap.packExit = 1
	if _, err := h.mgr.End(ctx, alice, "r", ""); !errors.Is(err, ErrSnapshotFailed) || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("end = %v", err)
	}
	h.lifecycles()

	// --force ends it without the snapshot.
	var progress bytes.Buffer
	res, err := h.mgr.EndSession(ctx, alice, "r", "", EndOptions{Force: true, Progress: &progress})
	if err != nil || !res.Existed || res.Snapshot != nil || res.SnapshotErr == nil {
		t.Fatalf("forced end = %+v, %v", res, err)
	}
	if h.exists(t, s.Sandbox) || len(h.tokens.named(TokenName(s.Sandbox))) != 0 {
		t.Fatal("forced end left the sandbox or its tokens")
	}
	if !strings.Contains(progress.String(), "ending the session without it (--force)") {
		t.Fatalf("progress = %q", progress.String())
	}
	if got := strings.Join(h.lifecycles(), ","); got != "failed snapshot,ended" {
		t.Fatalf("events = %s", got)
	}
}

func TestEndResumesParkedSandboxToSnapshot(t *testing.T) {
	ctx := context.Background()
	h := newSnapHarness(t, nil, true)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.Park(ctx, alice, "r", ""); err != nil {
		t.Fatal(err)
	}
	var progress bytes.Buffer
	res, err := h.mgr.EndSession(ctx, alice, "r", "", EndOptions{Progress: &progress})
	if err != nil || res.Snapshot == nil {
		t.Fatalf("end = %+v, %v", res, err)
	}
	if !strings.Contains(progress.String(), "Resuming sandbox "+s.Sandbox+" to snapshot it") || h.exists(t, s.Sandbox) {
		t.Fatalf("progress = %q", progress.String())
	}
}

func TestSnapshotTooLarge(t *testing.T) {
	ctx := context.Background()
	h := newSnapHarness(t, nil, false)
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	// The pack script refuses an archive over the single-PUT limit.
	h.exec.snap.packExit = 3
	_, err := h.mgr.End(ctx, alice, "r", "")
	if !errors.Is(err, ErrSnapshotFailed) {
		t.Fatalf("end = %v", err)
	}
	pack := h.exec.last("soft-alice-r", snapshotPackName)
	if pack.args()[2] != "5368709120" {
		t.Fatalf("pack limit = %v", pack.args()[:3])
	}
}

func TestStartRestoresSnapshot(t *testing.T) {
	ctx := context.Background()
	h := newSnapHarness(t, nil, false)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	work(h, s.Sandbox)
	if _, err := h.mgr.End(ctx, alice, "r", ""); err != nil {
		t.Fatal(err)
	}
	freshVolume(h, s.Sandbox)
	h.exec.runs = nil
	h.lifecycles()

	var progress bytes.Buffer
	s, err = h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Progress: &progress})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Created || s.Cloned || s.Restored == nil || s.Restored.URL != "s3://snapshots/"+snapKey || s.Unparked {
		t.Fatalf("session = %+v", s)
	}
	b := h.exec.box(s.Sandbox)
	if b.files["/sandbox/r/main.go"] != "package main // unpushed\n" || b.files["/sandbox/.bashrc"] != "# mine\n" {
		t.Fatalf("not restored: %v", b.files)
	}
	if _, ok := b.files["/sandbox/.ssh/id_ed25519"]; ok {
		t.Fatal("excluded file restored")
	}
	// Restored before the credentials and the clone check; osewa is
	// enrolled afresh.
	want := []string{snapshotProbeName, snapshotRestoreName, gitCredentialsName, osewaProbeName, osewaServerName, osewaEnrollName, osewaHostsName, osewaListName, osewaNewName}
	if got := h.exec.scripts(s.Sandbox); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("scripts = %v, want %v", got, want)
	}
	if got := strings.Join(h.lifecycles(), ","); got != "created,restored start,ready" {
		t.Fatalf("events = %s", got)
	}
	if !strings.Contains(progress.String(), "Restoring /sandbox from the snapshot of ") {
		t.Fatalf("progress = %q", progress.String())
	}
	for _, r := range h.exec.runs {
		if r.script() == snapshotRestoreName && !strings.Contains(r.args()[0], "X-Amz-Signature=") {
			t.Fatalf("restore URL is not presigned: %s", r.args()[0])
		}
	}

	// A sandbox that exists is never restored into.
	h.exec.runs = nil
	if s, err = h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil || s.Restored != nil {
		t.Fatalf("second start = %+v, %v", s, err)
	}
	if slices.Contains(h.exec.scripts(s.Sandbox), snapshotRestoreName) {
		t.Fatal("restored into an existing sandbox")
	}
}

func TestStartFreshSkipsSnapshot(t *testing.T) {
	ctx := context.Background()
	h := newSnapHarness(t, nil, false)
	h.s3.PutObject(snapKey, []byte("not even gzip"), nil)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Restore: RestoreSkip})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Cloned || s.Restored != nil || slices.Contains(h.exec.scripts(s.Sandbox), snapshotRestoreName) {
		t.Fatalf("session = %+v scripts = %v", s, h.exec.scripts(s.Sandbox))
	}
	// Kept until the next snapshot replaces it.
	if _, ok := h.s3.Object(snapKey); !ok {
		t.Fatal("snapshot deleted")
	}
}

func TestStartRestoreConflict(t *testing.T) {
	ctx := context.Background()
	h := newSnapHarness(t, nil, false)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	work(h, s.Sandbox)
	if _, err := h.mgr.End(ctx, alice, "r", ""); err != nil {
		t.Fatal(err)
	}
	// The fake keeps the deleted sandbox's files, like a volume that
	// outlived its sandbox: the new sandbox holds earlier work.
	h.exec.box(s.Sandbox).files["/sandbox/r/main.go"] = "package main // newer\n"
	h.lifecycles()

	_, err = h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	var conflict *RestoreConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, ErrRestoreConflict) {
		t.Fatalf("start = %v", err)
	}
	if strings.Join(conflict.Paths, ",") != "/sandbox/r,/sandbox/.config/soft-serve" || conflict.Snapshot.Size == 0 {
		t.Fatalf("conflict = %+v", conflict)
	}
	for _, want := range []string{"--restore", "--fresh", "already holds files"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err %q lacks %q", err, want)
		}
	}
	if !strings.Contains(conflict.Question(), "Restore the snapshot over them?") {
		t.Fatalf("question = %q", conflict.Question())
	}
	// Nothing was started: the new sandbox is gone, and a question is not
	// a failure.
	if h.exists(t, s.Sandbox) {
		t.Fatal("sandbox left after a conflict")
	}
	if got := strings.Join(h.lifecycles(), ","); got != "created" {
		t.Fatalf("events = %s", got)
	}

	// --restore: the snapshot wins over the files it also has.
	s, err = h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Restore: RestoreOver})
	if err != nil || s.Restored == nil {
		t.Fatalf("start --restore = %+v, %v", s, err)
	}
	if got := h.exec.box(s.Sandbox).files["/sandbox/r/main.go"]; got != "package main // unpushed\n" {
		t.Fatalf("main.go = %q", got)
	}
}

func TestRestoreFailureDeletesSandbox(t *testing.T) {
	ctx := context.Background()
	h := newSnapHarness(t, nil, false)
	h.s3.PutObject(snapKey, []byte("not gzip"), nil)

	var progress bytes.Buffer
	_, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Progress: &progress})
	if err == nil || !strings.Contains(err.Error(), "restore the snapshot of session main (s3://snapshots/"+snapKey+")") || !strings.Contains(err.Error(), "decompress") {
		t.Fatalf("start = %v", err)
	}
	// Not half-started: no sandbox, no token, no clone, no osewa.
	if h.exists(t, "soft-alice-r") {
		t.Fatal("sandbox left after a failed restore")
	}
	if len(h.tokens.named(TokenName("soft-alice-r"))) != 0 || len(h.exec.clones()) != 0 {
		t.Fatal("session half-started")
	}
	if got := strings.Join(h.lifecycles(), ","); got != "created,failed restore" {
		t.Fatalf("events = %s", got)
	}

	// The store is unreachable while looking the snapshot up: same.
	h.s3.SetFail(func(*http.Request, string) int { return http.StatusInternalServerError })
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err == nil || h.exists(t, "soft-alice-r") {
		t.Fatalf("start with S3 down = %v", err)
	}
}

func TestFallbackParkAndResume(t *testing.T) {
	ctx := context.Background()
	h := newSnapHarness(t, func(c *config.Config) { c.Sessions.Suspend.Fallback = config.SuspendFallbackSnapshot }, false)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Profile: ""})
	if err != nil {
		t.Fatal(err)
	}
	work(h, s.Sandbox)
	h.lifecycles()

	// The gateway cannot suspend: the session is parked to its snapshot.
	parked, snapshot, err := h.mgr.ParkSession(ctx, alice, "r", "")
	if err != nil || !parked || !snapshot {
		t.Fatalf("park = %v %v %v", parked, snapshot, err)
	}
	if h.exists(t, s.Sandbox) {
		t.Fatal("sandbox kept")
	}
	if got := strings.Join(h.lifecycles(), ","); got != "snapshotted park,parked park snapshot" {
		t.Fatalf("events = %s", got)
	}
	var marker ParkedSnapshot
	o, ok := h.s3.Object("soft-serve/sessions/1/soft-alice-r.parked.json")
	if !ok || json.Unmarshal(o.Body, &marker) != nil {
		t.Fatalf("marker = %v %s", ok, o.Body)
	}
	if marker.Repo != "r" || marker.Session != "main" || marker.Image != "default-image" || marker.Op != ParkManual || marker.Size == 0 || marker.UserID != 1 {
		t.Fatalf("marker = %+v", marker)
	}
	// Parking again is a no-op, and says it is parked to its snapshot.
	if parked, snapshot, err := h.mgr.ParkSession(ctx, alice, "r", ""); err != nil || parked || !snapshot {
		t.Fatalf("park again = %v %v %v", parked, snapshot, err)
	}

	// Listed as parked to its snapshot, with no sandbox.
	infos, err := h.mgr.List(ctx, alice)
	if err != nil || len(infos) != 1 || infos[0].Phase != PhaseSnapshot || infos[0].Snapshot == nil || infos[0].Name != "main" {
		t.Fatalf("list = %+v, %v", infos, err)
	}
	if PhaseWord(infos[0].Phase) != WordParkedSnapshot {
		t.Fatalf("word = %q", PhaseWord(infos[0].Phase))
	}

	// Attach recreates the sandbox from the same image, restores and
	// starts the program again, saying so.
	freshVolume(h, s.Sandbox)
	h.lifecycles()
	var progress bytes.Buffer
	s, err = h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r", Progress: &progress})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Created || !s.Unparked || !s.Started || s.Restored == nil || s.Cloned {
		t.Fatalf("session = %+v", s)
	}
	for _, want := range []string{
		"was parked to a snapshot of /sandbox",
		"Started bash -l again: it was not running when the session resumed (the session was parked to a snapshot of /sandbox, which keeps files but not running programs)",
	} {
		if !strings.Contains(progress.String(), want) {
			t.Fatalf("progress %q lacks %q", progress.String(), want)
		}
	}
	if got := h.exec.box(s.Sandbox).files["/sandbox/r/main.go"]; got != "package main // unpushed\n" {
		t.Fatalf("not restored: %q", got)
	}
	sb, _ := h.fc.Sandboxes().Get(ctx, "default", s.Sandbox)
	if sb.Spec.Template.Image != "default-image" {
		t.Fatalf("image = %v", sb.Spec.Template)
	}
	if _, ok := h.s3.Object("soft-serve/sessions/1/soft-alice-r.parked.json"); ok {
		t.Fatal("marker kept after resume")
	}
	if got := strings.Join(h.lifecycles(), ","); got != "created,restored attach,ready" {
		t.Fatalf("events = %s", got)
	}
	if infos, _ := h.mgr.List(ctx, alice); len(infos) != 1 || infos[0].Phase != string(types.SandboxReady) {
		t.Fatalf("list after resume = %+v", infos)
	}
}

func TestEndParkedSnapshotSession(t *testing.T) {
	ctx := context.Background()
	h := newSnapHarness(t, func(c *config.Config) { c.Sessions.Suspend.Fallback = config.SuspendFallbackSnapshot }, false)
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.mgr.ParkSession(ctx, alice, "r", ""); err != nil {
		t.Fatal(err)
	}
	h.lifecycles()
	res, err := h.mgr.EndSession(ctx, alice, "r", "", EndOptions{})
	if err != nil || !res.Existed || !res.Parked {
		t.Fatalf("end = %+v, %v", res, err)
	}
	if _, ok := h.s3.Object("soft-serve/sessions/1/soft-alice-r.parked.json"); ok {
		t.Fatal("marker kept")
	}
	if _, ok := h.s3.Object(snapKey); !ok {
		t.Fatal("snapshot deleted")
	}
	if got := strings.Join(h.lifecycles(), ","); got != "ended" {
		t.Fatalf("events = %s", got)
	}
	if infos, _ := h.mgr.List(ctx, alice); len(infos) != 0 {
		t.Fatalf("list = %+v", infos)
	}
	// Nothing left to end.
	if existed, err := h.mgr.End(ctx, alice, "r", ""); err != nil || existed {
		t.Fatalf("end again = %v %v", existed, err)
	}
}

func TestFallbackParkFailureKeepsSandbox(t *testing.T) {
	ctx := context.Background()
	h := newSnapHarness(t, func(c *config.Config) { c.Sessions.Suspend.Fallback = config.SuspendFallbackSnapshot }, false)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	h.lifecycles()
	h.exec.snap.curlExit = 7
	if _, _, err := h.mgr.ParkSession(ctx, alice, "r", ""); err == nil || !strings.Contains(err.Error(), "curl exit status 7") {
		t.Fatalf("park = %v", err)
	}
	if !h.exists(t, s.Sandbox) {
		t.Fatal("sandbox deleted without a snapshot")
	}
	if _, ok := h.s3.Object("soft-serve/sessions/1/soft-alice-r.parked.json"); ok {
		t.Fatal("marker written without a snapshot")
	}
	if got := strings.Join(h.lifecycles(), ","); got != "failed snapshot" {
		t.Fatalf("events = %s", got)
	}

	// Without the fallback, an unsupported suspend is still an error.
	h2 := newSnapHarness(t, nil, false)
	if _, err := h2.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h2.mgr.Park(ctx, alice, "r", ""); !IsSuspendUnsupported(err) || !strings.Contains(err.Error(), "sessions.suspend.fallback: snapshot") {
		t.Fatalf("park without fallback = %v", err)
	}
	if !h2.exists(t, "soft-alice-r") {
		t.Fatal("sandbox deleted without the fallback")
	}
}

func TestAutoParkFallback(t *testing.T) {
	ctx := context.Background()
	h := newSnapHarness(t, func(c *config.Config) { c.Sessions.Suspend.Fallback = config.SuspendFallbackSnapshot }, false)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	h.lifecycles()
	sb, _ := h.fc.Sandboxes().Get(ctx, "default", s.Sandbox)
	h.mon.reconcile([]*Sandbox{sb})
	h.mon.mu.Lock()
	e := h.mon.entries[s.Sandbox]
	e.known = true
	e.lastActive = time.Now().Add(-2 * time.Hour)
	e.st.Record = events.Record{State: events.StateIdle, Msg: "waiting"}
	h.mon.unsuspendable[s.Sandbox] = "unsupported"
	h.mon.mu.Unlock()

	h.mon.autoPark(ctx, s.Sandbox)
	if h.exists(t, s.Sandbox) {
		t.Fatal("not parked")
	}
	if got := strings.Join(h.lifecycles(), ","); got != "snapshotted park,parked auto snapshot" {
		t.Fatalf("events = %s", got)
	}
	var marker ParkedSnapshot
	o, _ := h.s3.Object("soft-serve/sessions/1/soft-alice-r.parked.json")
	if err := json.Unmarshal(o.Body, &marker); err != nil || marker.Op != ParkAuto || marker.Record.Msg != "waiting" {
		t.Fatalf("marker = %+v, %v", marker, err)
	}
	if st, _ := h.mon.Status(s.Sandbox); st.Phase != PhaseSnapshot || !st.Parked {
		t.Fatalf("status = %+v", st)
	}
}

func TestParkBackupSnapshot(t *testing.T) {
	ctx := context.Background()
	h := newSnapHarness(t, func(c *config.Config) { c.Snapshots.S3.On = []string{"end", "park"} }, true)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	h.lifecycles()
	parked, snapshot, err := h.mgr.ParkSession(ctx, alice, "r", "")
	if err != nil || !parked || snapshot {
		t.Fatalf("park = %v %v %v", parked, snapshot, err)
	}
	if got := strings.Join(h.lifecycles(), ","); got != "snapshotted park,parked park" {
		t.Fatalf("events = %s", got)
	}
	if _, ok := h.s3.Object(snapKey); !ok {
		t.Fatal("no backup")
	}
	sb, _ := h.fc.Sandboxes().Get(ctx, "default", s.Sandbox)
	if sb.Status.Phase != types.SandboxSuspended {
		t.Fatalf("phase = %s", sb.Status.Phase)
	}

	// A failed backup does not stop the park.
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	h.lifecycles()
	h.exec.snap.packExit = 2
	if parked, _, err := h.mgr.ParkSession(ctx, alice, "r", ""); err != nil || !parked {
		t.Fatalf("park after a failed backup = %v %v", parked, err)
	}
	if got := strings.Join(h.lifecycles(), ","); got != "failed snapshot,parked park" {
		t.Fatalf("events = %s", got)
	}
}

func TestSnapshotsOffTouchNothing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.End(ctx, alice, "r", ""); err != nil {
		t.Fatal(err)
	}
	for _, sc := range h.exec.scripts("soft-alice-r") {
		if strings.HasPrefix(sc, "soft-serve-snapshot") {
			t.Fatalf("ran %s with snapshots off", sc)
		}
	}
}

func TestSnapshotExecsFitGatewayLimits(t *testing.T) {
	// The fake's gatewayLimits checked every exec of the tests above; this
	// checks the worst case: an STS session token in the presigned URL.
	ctx := context.Background()
	h := newSnapHarness(t, nil, false)
	creds := "[default]\naws_access_key_id = " + s3test.AccessKeyID + "\naws_secret_access_key = " + s3test.SecretAccessKey +
		"\naws_session_token = " + strings.Repeat("T", 2000) + "\n"
	writeCreds(t, h.cfg.Snapshots.S3.CredentialsFile, creds)
	h.mgr.snaps = newSnapshotStore(h.cfg)
	p, err := h.mgr.snaps.store.PresignPut(ctx, snapKey, presignTTL, s3PutOpts())
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"sh", "-c", snapshotUploadScript, snapshotUploadName, snapshotSpoolDir + "/snapshot.abc123", p.URL}, p.HeaderLines()...)
	if err := gatewayLimits(args, ExecOptions{Env: osewaEnv()}); err != nil {
		t.Fatal(err)
	}
	if len(p.URL) > 8<<10 {
		t.Fatalf("URL is %d bytes", len(p.URL))
	}
}

func writeCreds(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func s3PutOpts() s3store.PutOptions {
	return s3store.PutOptions{
		ContentMD5:  "1B2M2Y8AsgTpgAmY7PhCfg==",
		ContentType: "application/gzip",
		Metadata:    snapshotMeta(events.Session{User: "alice", Repo: "r", Name: "main", Sandbox: "soft-alice-r"}),
	}
}
