package jjview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// tools returns jj and git, skipping the test without them.
func tools(t *testing.T) (string, string) {
	t.Helper()
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Skip("jj is not installed")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	return jj, git
}

// served makes a bare repository as a jj client pushes it: two changes on
// main, with change-id headers. It returns its path and a function that
// adds a third change.
func served(t *testing.T, jj, git string) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "jj.toml")
	if err := os.WriteFile(cfg, []byte("user.name = \"T\"\nuser.email = \"t@example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, bare := filepath.Join(dir, "src"), filepath.Join(dir, "bare.git")
	run := func(d, bin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), bin, args...)
		cmd.Dir = d
		cmd.Env = append(os.Environ(), "JJ_CONFIG="+cfg)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(src, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run(dir, jj, "git", "init", "--colocate", "src")
	write("a")
	run(src, jj, "describe", "-m", "first change")
	run(src, jj, "new", "-m", "second change")
	write("b")
	run(src, jj, "status")
	run(src, git, "branch", "main", run(src, jj, "log", "-r", "@", "--no-graph", "-T", "commit_id"))
	run(dir, git, "init", "-q", "--bare", bare)
	run(bare, git, "fetch", "-q", src, "+main:main")
	return bare, func() {
		run(src, jj, "new", "-m", "third change")
		write("c")
		run(src, jj, "status")
		run(src, git, "branch", "-f", "main", run(src, jj, "log", "-r", "@", "--no-graph", "-T", "commit_id"))
		run(bare, git, "fetch", "-q", src, "+main:main")
	}
}

// fingerprint is every file of dir with a hash of its content.
func fingerprint(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		rel, _ := filepath.Rel(dir, p)
		lines = append(lines, rel+" "+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func changes(lines []Line) []string {
	var out []string
	for _, l := range lines {
		if l.Change != "" {
			out = append(out, l.Change)
		}
	}
	return out
}

func TestLogShowAndServedRepoUntouched(t *testing.T) {
	jj, git := tools(t)
	bare, addChange := served(t, jj, git)
	before := fingerprint(t, bare)
	v := New(jj, git, t.TempDir())
	ctx := context.Background()

	lines, err := v.Log(ctx, bare, LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ids := changes(lines)
	text := ""
	for _, l := range lines {
		text += l.Text + "\n"
	}
	if len(ids) != 2 || !strings.Contains(text, "second change") || !strings.Contains(text, "first change") || !strings.Contains(text, "main") {
		t.Fatalf("log %v:\n%s", ids, text)
	}
	if strings.ContainsAny(text, "\x1e\x1f") {
		t.Fatalf("markers left in:\n%q", text)
	}
	for _, id := range ids {
		if !regexp.MustCompile(`^[k-z]{12}$`).MatchString(id) {
			t.Fatalf("change ID %q", id)
		}
	}

	out, err := v.Show(ctx, bare, ids[0], false)
	if err != nil || !strings.Contains(out, "second change") || !strings.Contains(out, "Change ID: "+ids[0]) {
		t.Fatalf("show = %v:\n%s", err, out)
	}
	if _, err := v.Show(ctx, bare, "main; rm -rf /", false); !errors.Is(err, ErrBadChange) {
		t.Fatalf("bad change: %v", err)
	}

	// Colour keeps jj's escapes; the change IDs are still plain.
	colored, err := v.Log(ctx, bare, LogOptions{Color: true, Limit: 1})
	if err != nil || len(changes(colored)) != 1 || changes(colored)[0] != ids[0] || !strings.Contains(colored[0].Text, "\x1b[") {
		t.Fatalf("colour log = %v %+v", err, colored)
	}

	// A push shows up on the next view.
	addChange()
	before = fingerprint(t, bare)
	lines, err = v.Log(ctx, bare, LogOptions{})
	if err != nil || len(changes(lines)) != 3 {
		t.Fatalf("after a push: %v %v", err, changes(lines))
	}

	// The served repository is exactly as it was: no jj objects or refs.
	if after := fingerprint(t, bare); after != before {
		t.Fatalf("the served repository changed:\nbefore\n%s\nafter\n%s", before, after)
	}
}

func TestBrokenCacheIsRebuilt(t *testing.T) {
	jj, git := tools(t)
	bare, _ := served(t, jj, git)
	root := t.TempDir()
	v := New(jj, git, root)
	ctx := context.Background()
	if _, err := v.Log(ctx, bare, LogOptions{}); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(bare)
	// Break the jj workspace.
	if err := os.RemoveAll(filepath.Join(v.cacheDir(abs), "ws", ".jj", "repo")); err != nil {
		t.Fatal(err)
	}
	lines, err := v.Log(ctx, bare, LogOptions{})
	if err != nil || len(changes(lines)) != 2 {
		t.Fatalf("after breaking the cache: %v %v", err, changes(lines))
	}
}

func TestEmptyRepoAndNoJJ(t *testing.T) {
	jj, git := tools(t)
	bare := filepath.Join(t.TempDir(), "empty.git")
	if out, err := exec.CommandContext(t.Context(), git, "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	v := New(jj, git, t.TempDir())
	lines, err := v.Log(context.Background(), bare, LogOptions{})
	if err != nil || len(changes(lines)) != 0 {
		t.Fatalf("empty: %v %+v", err, lines)
	}
	none := New("", git, t.TempDir())
	if none.Available() {
		t.Fatal("available without jj")
	}
	if _, err := none.Log(context.Background(), bare, LogOptions{}); !errors.Is(err, ErrNoJJ) {
		t.Fatalf("no jj: %v", err)
	}
}

// TestJJPinMatchesContainerfiles checks the server and sandbox images pin
// the same jj release and checksums.
func TestJJPinMatchesContainerfiles(t *testing.T) {
	read := func(p string) map[string]string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, m := range regexp.MustCompile(`(?m)^ARG (JJ_[A-Z0-9_]+)=(\S+)$`).FindAllSubmatch(b, -1) {
			out[string(m[1])] = string(m[2])
		}
		return out
	}
	server, sandbox := read("../../Containerfile"), read("../../deploy/sandbox/Containerfile")
	for _, arg := range []string{"JJ_VERSION", "JJ_SHA256_AMD64", "JJ_SHA256_ARM64"} {
		if server[arg] == "" || server[arg] != sandbox[arg] {
			t.Errorf("ARG %s: server %q, sandbox %q", arg, server[arg], sandbox[arg])
		}
	}
}

func TestForDataPathEnv(t *testing.T) {
	t.Setenv(EnvJJPath, "off")
	if v := ForDataPath(t.TempDir()); v.Available() {
		t.Fatal("off: available")
	}
	t.Setenv(EnvJJPath, "/opt/jj")
	if v := ForDataPath(t.TempDir()); v.jj != "/opt/jj" {
		t.Fatalf("path: %q", v.jj)
	}
}
