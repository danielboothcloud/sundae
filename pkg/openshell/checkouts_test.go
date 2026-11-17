package openshell

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs git in dir with a fixed identity and fails the test on error.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitFile commits a file with the given content in the git repository
// dir.
func commitFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", name)
	gitIn(t, dir, "commit", "-q", "-m", name)
}

// TestUpdateCheckoutsGit: a clean clone moves to the new commit; one with a
// conflicting local change, or diverged, is fetched and left; a missing
// checkout is reported.
func TestUpdateCheckoutsGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	home, binDir, _ := localHome(t)
	origin := filepath.Join(home, "origin")
	if err := os.MkdirAll(origin, 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, origin, "init", "-q", "-b", "main")
	commitFile(t, origin, "a.txt", "1\n")

	clone := func(name string) string {
		dir := filepath.Join(home, name)
		gitIn(t, home, "clone", "-q", origin, dir)
		return dir
	}
	clean, dirty, diverged, current := clone("clean"), clone("dirty"), clone("diverged"), clone("current")
	commitFile(t, origin, "a.txt", "2\n")
	gitIn(t, current, "pull", "-q", "--ff-only")
	if err := os.WriteFile(filepath.Join(dirty, "a.txt"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commitFile(t, diverged, "b.txt", "mine\n")

	dirs := map[string]string{"clean": clean, "dirty": dirty, "diverged": diverged, "current": current, "gone": filepath.Join(home, "gone")}
	lc := &localClient{home: home, bin: binDir, sandbox: "sd-test", supervisorEnv: map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null"}}
	res, err := updateCheckouts(t.Context(), lc, lc.sandbox, []string{"clean", "dirty", "diverged", "current", "gone"}, func(r string) string { return dirs[r] })
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"clean": CheckoutUpdated, "dirty": CheckoutFetched, "diverged": CheckoutFetched, "current": CheckoutCurrent, "gone": CheckoutMissing}
	for _, u := range res {
		if u.State != want[u.Repo] {
			t.Errorf("%s: state %q (%s), want %q", u.Repo, u.State, u.Detail, want[u.Repo])
		}
	}
	if b, _ := os.ReadFile(filepath.Join(clean, "a.txt")); string(b) != "2\n" {
		t.Errorf("clean checkout not updated: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dirty, "a.txt")); string(b) != "local\n" {
		t.Errorf("local change lost: %q", b)
	}
}

// TestUpdateCheckoutsJJ: an empty working-copy change moves onto the new
// trunk(); one with work stays where it is.
func TestUpdateCheckoutsJJ(t *testing.T) {
	for _, tool := range []string{"git", "jj"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	home, binDir, _ := localHome(t)
	if err := os.Symlink(mustLookPath(t, "jj"), filepath.Join(binDir, "jj")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mustLookPath(t, "git"), filepath.Join(binDir, "git")); err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(home, "origin")
	if err := os.MkdirAll(origin, 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, origin, "init", "-q", "-b", "main")
	commitFile(t, origin, "a.txt", "1\n")

	jjEnv := []string{"JJ_USER=t", "JJ_EMAIL=t@example.com", "JJ_CONFIG=/dev/null", "HOME=" + home, "GIT_CONFIG_GLOBAL=/dev/null"}
	jj := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "jj", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), jjEnv...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("jj %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	clone := func(name string) string {
		dir := filepath.Join(home, name)
		jj(home, "--quiet", "git", "clone", "--colocate", origin, dir)
		return dir
	}
	empty, busy := clone("empty"), clone("busy")
	if err := os.WriteFile(filepath.Join(busy, "wip.txt"), []byte("wip\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jj(busy, "status") // snapshot the work into @
	commitFile(t, origin, "a.txt", "2\n")

	dirs := map[string]string{"empty": empty, "busy": busy}
	env := map[string]string{"JJ_USER": "t", "JJ_EMAIL": "t@example.com", "JJ_CONFIG": "/dev/null", "GIT_CONFIG_GLOBAL": "/dev/null"}
	lc := &localClient{home: home, bin: binDir, sandbox: "sd-test", supervisorEnv: env}
	res, err := updateCheckouts(t.Context(), lc, lc.sandbox, []string{"empty", "busy"}, func(r string) string { return dirs[r] })
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"empty": CheckoutUpdated, "busy": CheckoutFetched}
	for _, u := range res {
		if u.State != want[u.Repo] {
			t.Errorf("%s: state %q (%s), want %q", u.Repo, u.State, u.Detail, want[u.Repo])
		}
	}
	if b, _ := os.ReadFile(filepath.Join(empty, "a.txt")); string(b) != "2\n" {
		t.Errorf("empty change not moved to trunk(): a.txt = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(busy, "wip.txt")); string(b) != "wip\n" {
		t.Errorf("work lost: %q", b)
	}

	// A second run finds both up to date or left alone.
	res, err = updateCheckouts(t.Context(), lc, lc.sandbox, []string{"empty"}, func(r string) string { return dirs[r] })
	if err != nil || res[0].State != CheckoutCurrent {
		t.Fatalf("second run: %+v, %v", res, err)
	}
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
