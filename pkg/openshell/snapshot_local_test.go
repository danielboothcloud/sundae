package openshell

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/danielboothcloud/sundae/pkg/s3store/s3test"
)

// localSandbox runs the snapshot scripts on this machine, with /sandbox and
// /tmp/sundae mapped to temporary directories: the real tar, gzip,
// md5sum and curl against the fake S3 server, which checks the presigned
// requests' signatures.
type localSandbox struct {
	Client
	root, spool string
}

func (c *localSandbox) mapArg(a string) string {
	switch {
	case a == SandboxRoot || strings.HasPrefix(a, SandboxRoot+"/"):
		return c.root + strings.TrimPrefix(a, SandboxRoot)
	case a == snapshotSpoolDir || strings.HasPrefix(a, snapshotSpoolDir+"/"):
		return c.spool + strings.TrimPrefix(a, snapshotSpoolDir)
	}
	return a
}

func (c *localSandbox) Run(ctx context.Context, _ string, command []string, opts ExecOptions) (*ExecResult, error) {
	if err := gatewayLimits(command, opts); err != nil {
		return nil, err
	}
	args := make([]string, len(command))
	for i, a := range command {
		args[i] = c.mapArg(a)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = c.root
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + c.root}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return &ExecResult{ExitCode: exit.ExitCode(), Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
	case err != nil:
		return nil, err
	}
	return &ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"sh", "tar", "gzip", "md5sum", "curl", "mktemp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out[rel] = "-> " + target
		case info.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func archiveNames(t *testing.T, body []byte) []string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, strings.TrimSuffix(hdr.Name, "/"))
	}
}

func TestSnapshotPipelineLocal(t *testing.T) {
	requireTools(t)
	ctx := context.Background()
	srv := s3test.New(t)
	cfg := config.DefaultConfig()
	cfg.Snapshots.S3 = srv.Config(t, t.TempDir())
	cfg.Snapshots.S3.SSE = config.SSES3
	snaps := newSnapshotStore(cfg)

	root := filepath.Join(t.TempDir(), "sandbox")
	spool := filepath.Join(t.TempDir(), "sundae")
	writeTree(t, root, map[string]string{
		"r/main.go":                         "package main // unpushed\n",
		"r/.git/HEAD":                       "ref: refs/heads/main\n",
		"r/.cache/kept":                     "a repo's own .cache is not the XDG cache\n",
		"r/.ssh/fixture":                    "a repo's test fixture\n",
		".bashrc":                           "# mine\n",
		".claude/settings.json":             "{}\n",
		".claude/.credentials.json":         "{\"token\":\"x\"}\n",
		".config/osewa/server.key":          "server key\n",
		".local/state/osewa/osewa.log":      "log\n",
		".config/sundae/osewa-session":      "osewa1\n",
		".config/sundae/osewa-session.lock": "",
		".config/sundae/osewa-client/hosts": "[]\n",
		".config/sundae/notes":              "kept\n",
		".local/lib/sundae/osewa-0.1.4":     "binary\n",
		".cache/go-build/00/x":              "cache\n",
		".ssh/id_ed25519":                   "PRIVATE KEY\n",
		".aws/credentials":                  "[default]\n",
		".docker/config.json":               "{}\n",
		".docker/cli-plugins/kept":          "kept\n",
		".config/gh/hosts.yml":              "token\n",
		".config/gh/config.yml":             "kept\n",
		".git-credentials":                  "https://u:p@host\n",
		".netrc":                            "machine x\n",
		".kube/config":                      "kubeconfig\n",
		".gnupg/private-keys-v1.d/x.key":    "key\n",
		".codex/auth.json":                  "{}\n",
		".config/glab-cli/config.yml":       "token\n",
		".local/share/kept":                 "kept\n",
	})
	if err := os.Chmod(filepath.Join(root, "r/main.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("r/main.go", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	box := &localSandbox{root: root, spool: spool}
	sess := events.Session{User: "alice", Repo: "r", Name: "main", Sandbox: "sundae-alice-r"}

	var progress bytes.Buffer
	info, err := snaps.take(ctx, box, 1, sess, &progress)
	if err != nil {
		t.Fatalf("take: %v (progress %q)", err, progress.String())
	}
	o, ok := srv.Object(snapKey)
	if !ok || int64(len(o.Body)) != info.Size || o.SSE != "AES256" {
		t.Fatalf("object = %v %d/%d %q", ok, len(o.Body), info.Size, o.SSE)
	}
	names := archiveNames(t, o.Body)
	want := []string{
		"./r/main.go", "./r/.git/HEAD", "./.bashrc", "./link",
		"./.claude/settings.json", "./.config/sundae/notes", "./.docker/cli-plugins/kept", "./.config/gh/config.yml", "./.local/share/kept",
	}
	if gnuTar() {
		// Anchored: only the home's .cache and .ssh are left out.
		want = append(want, "./r/.cache/kept", "./r/.ssh/fixture")
	} else {
		t.Log("tar is not GNU tar: the exclusions also match at depth")
	}
	for _, w := range want {
		if !slices.Contains(names, w) {
			t.Errorf("archive lacks %s", w)
		}
	}
	for _, name := range names {
		if excluded(name, SnapshotExcludes) {
			t.Errorf("archive has excluded %s", name)
		}
	}
	for _, gone := range []string{"./.ssh/id_ed25519", "./.config/osewa/server.key", "./.cache/go-build/00/x", "./.config/sundae/osewa-session.lock"} {
		if slices.Contains(names, gone) {
			t.Errorf("archive has %s", gone)
		}
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Fatalf("spool not cleaned up: %v", entries)
	}

	// Restore into a new sandbox holding the image's skeleton: the
	// snapshot's files win, the image's other files stay.
	newRoot := filepath.Join(t.TempDir(), "sandbox")
	writeTree(t, newRoot, map[string]string{".bashrc": "# image\n", ".profile": "# image profile\n"})
	box2 := &localSandbox{root: newRoot, spool: filepath.Join(t.TempDir(), "sundae")}
	if found, err := probePaths(ctx, box2, "sundae-alice-r", "/sandbox/r", sundaeStateDir); err != nil || len(found) != 0 {
		t.Fatalf("probe new = %v %v", found, err)
	}
	if err := snaps.restore(ctx, box2, 1, "sundae-alice-r"); err != nil {
		t.Fatal(err)
	}
	got := readTree(t, newRoot)
	if got["r/main.go"] != "package main // unpushed\n" || got[".bashrc"] != "# mine\n" || got[".profile"] != "# image profile\n" || got["link"] != "-> r/main.go" {
		t.Fatalf("restored = %v", got)
	}
	if _, ok := got[".ssh/id_ed25519"]; ok {
		t.Fatal("restored a credential")
	}
	if fi, err := os.Stat(filepath.Join(newRoot, "r/main.go")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v %v", fi.Mode(), err)
	}
	if found, err := probePaths(ctx, box2, "sundae-alice-r", "/sandbox/r", sundaeStateDir); err != nil || strings.ReplaceAll(strings.Join(found, ","), newRoot, "/sandbox") != "/sandbox/r,/sandbox/.config/sundae" {
		t.Fatalf("probe restored = %v %v", found, err)
	}

	// A refused upload fails, and leaves no spool and the previous
	// snapshot.
	srv.SetFail(func(r *http.Request, _ string) int {
		if r.Method == http.MethodPut {
			return http.StatusForbidden
		}
		return 0
	})
	_, err = snaps.take(ctx, box, 1, sess, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "S3 refused the upload: HTTP 403") || !strings.Contains(err.Error(), "InjectedFailure") {
		t.Fatalf("refused take = %v", err)
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Fatalf("spool not cleaned up: %v", entries)
	}
	if o2, _ := srv.Object(snapKey); !bytes.Equal(o2.Body, o.Body) {
		t.Fatal("previous snapshot replaced")
	}
	srv.SetFail(nil)

	// A corrupt object fails the restore.
	srv.PutObject(snapKey, []byte("not gzip"), nil)
	err = snaps.restore(ctx, &localSandbox{root: t.TempDir(), spool: t.TempDir()}, 1, "sundae-alice-r")
	if err == nil || !strings.Contains(err.Error(), "decompress") {
		t.Fatalf("corrupt restore = %v", err)
	}
	// A missing object fails the download.
	srv.DeleteObject(snapKey)
	err = snaps.restore(ctx, &localSandbox{root: t.TempDir(), spool: t.TempDir()}, 1, "sundae-alice-r")
	if err == nil || !strings.Contains(err.Error(), "download") {
		t.Fatalf("missing restore = %v", err)
	}

	// An archive over the limit is refused before any upload.
	out, err := runScript(ctx, box, "sundae-alice-r", snapshotPackName, snapshotPackScript, nil, SandboxRoot, snapshotSpoolDir, "10")
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "more than the 10 bytes") {
		t.Fatalf("pack over limit = %q %v", out, err)
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Fatalf("spool not cleaned up: %v", entries)
	}
}

// gnuTar reports whether the tar on PATH is GNU tar.
func gnuTar() bool {
	out, err := exec.CommandContext(context.Background(), "tar", "--version").Output()
	return err == nil && strings.Contains(string(out), "GNU")
}

// TestSnapshotPipelineS3 runs the snapshot pipeline against a real S3 store
// when SUNDAE_S3_E2E_ENDPOINT is set (with _BUCKET, _CREDENTIALS_FILE,
// and optionally _REGION, _SSE and _BIG_MB, the size of an incompressible
// file to add). It was run against a local MinIO; see the change's report.
func TestSnapshotPipelineS3(t *testing.T) {
	endpoint := os.Getenv("SUNDAE_S3_E2E_ENDPOINT")
	if endpoint == "" {
		t.Skip("SUNDAE_S3_E2E_ENDPOINT is not set")
	}
	requireTools(t)
	ctx := context.Background()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Snapshots.S3 = config.S3SnapshotConfig{
		Enabled:         true,
		Endpoint:        endpoint,
		Region:          os.Getenv("SUNDAE_S3_E2E_REGION"),
		Bucket:          os.Getenv("SUNDAE_S3_E2E_BUCKET"),
		CredentialsFile: os.Getenv("SUNDAE_S3_E2E_CREDENTIALS_FILE"),
		SSE:             os.Getenv("SUNDAE_S3_E2E_SSE"),
		PathStyle:       true,
		Prefix:          "e2e/" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	snaps := newSnapshotStore(cfg)
	if snaps.err != nil {
		t.Fatal(snaps.err)
	}

	root := filepath.Join(t.TempDir(), "sandbox")
	spool := filepath.Join(t.TempDir(), "sundae")
	writeTree(t, root, map[string]string{
		"r/main.go":                "package main\n",
		".bashrc":                  "# mine\n",
		".ssh/id_ed25519":          "PRIVATE KEY\n",
		".config/osewa/server.key": "key\n",
	})
	var bigSize int64
	if mb := os.Getenv("SUNDAE_S3_E2E_BIG_MB"); mb != "" {
		// Random bytes: gzip cannot shrink them, so the upload is as big.
		p := filepath.Join(root, "r", "big.bin")
		cmd := exec.CommandContext(ctx, "dd", "if=/dev/urandom", "of="+p, "bs=1048576", "count="+mb)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("dd: %v %s", err, out)
		}
		fi, _ := os.Stat(p)
		bigSize = fi.Size()
	}
	box := &localSandbox{root: root, spool: spool}
	sess := events.Session{User: "alice", Repo: "r", Name: "main", Sandbox: "sundae-alice-r"}

	info, err := snaps.take(ctx, box, 1, sess, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("snapshot %s: %d bytes", info.URL, info.Size)
	if info.Size < bigSize {
		t.Fatalf("snapshot of %d bytes holds a %d byte file", info.Size, bigSize)
	}
	head, err := snaps.store.Head(ctx, snaps.key(1, "sundae-alice-r"))
	if err != nil || head.Size != info.Size || head.Metadata["sundae-repo"] != "r" {
		t.Fatalf("head = %+v, %v", head, err)
	}
	if sse := cfg.Snapshots.S3.SSE; sse != "" && head.SSE != sse {
		t.Fatalf("stored with SSE %q, want %q", head.SSE, sse)
	}
	t.Logf("stored with SSE %q", head.SSE)
	if bigSize > 0 {
		// Free the space before restoring.
		if err := os.Remove(filepath.Join(root, "r", "big.bin")); err != nil {
			t.Fatal(err)
		}
	}

	newRoot := filepath.Join(t.TempDir(), "sandbox")
	writeTree(t, newRoot, map[string]string{".bashrc": "# image\n"})
	box2 := &localSandbox{root: newRoot, spool: filepath.Join(t.TempDir(), "sundae")}
	if err := snaps.restore(ctx, box2, 1, "sundae-alice-r"); err != nil {
		t.Fatal(err)
	}
	got := readTree(t, newRoot)
	if got["r/main.go"] != "package main\n" || got[".bashrc"] != "# mine\n" {
		t.Fatalf("restored %v", got)
	}
	for _, gone := range []string{".ssh/id_ed25519", ".config/osewa/server.key"} {
		if _, ok := got[gone]; ok {
			t.Fatalf("restored %s", gone)
		}
	}
	if bigSize > 0 {
		fi, err := os.Stat(filepath.Join(newRoot, "r", "big.bin"))
		if err != nil || fi.Size() != bigSize {
			t.Fatalf("big file restored as %v, %v", fi, err)
		}
		_ = os.Remove(filepath.Join(newRoot, "r", "big.bin"))
	}

	// The next snapshot replaces the object.
	writeTree(t, root, map[string]string{"r/main.go": "package main // v2\n"})
	if _, err := snaps.take(ctx, box, 1, sess, io.Discard); err != nil {
		t.Fatal(err)
	}
	newer := filepath.Join(t.TempDir(), "sandbox")
	if err := os.MkdirAll(newer, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := snaps.restore(ctx, &localSandbox{root: newer, spool: filepath.Join(t.TempDir(), "s")}, 1, "sundae-alice-r"); err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, newer)["r/main.go"]; got != "package main // v2\n" {
		t.Fatalf("second snapshot: %q", got)
	}

	// The parked marker round-trips and lists.
	m := ParkedSnapshot{UserID: 1, User: "alice", Repo: "r", Session: "main", Sandbox: "sundae-alice-r", Parked: time.Now().UTC().Truncate(time.Second), Op: ParkAuto}
	if err := snaps.writeMarker(ctx, m); err != nil {
		t.Fatal(err)
	}
	ms, err := snaps.listMarkers(ctx, 1)
	if err != nil || len(ms) != 1 || ms[0].Sandbox != "sundae-alice-r" {
		t.Fatalf("markers = %+v, %v", ms, err)
	}
	if err := snaps.deleteMarker(ctx, 1, "sundae-alice-r"); err != nil {
		t.Fatal(err)
	}

	// Clean up the snapshot.
	if err := snaps.store.Delete(ctx, snaps.key(1, "sundae-alice-r")); err != nil {
		t.Fatal(err)
	}
}
