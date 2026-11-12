package forge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hermeticGit isolates git from the developer's global and system config
// (credential helpers, url rewrites, default branch names) and fixes the
// identity, for this test and every git it runs, including the code under
// test.
func hermeticGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
}

// git runs git in dir and returns its trimmed output, failing the test on
// error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// upstream is a bare "forge" repository with a work tree to commit from.
type upstream struct {
	bare string
	work string
}

// newUpstream creates a bare repository with one commit on main.
func newUpstream(t *testing.T) *upstream {
	t.Helper()
	dir := t.TempDir()
	u := &upstream{bare: filepath.Join(dir, "up.git"), work: filepath.Join(dir, "work")}
	git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", u.bare)
	git(t, dir, "init", "--quiet", "--initial-branch=main", u.work)
	git(t, u.work, "remote", "add", "origin", u.bare)
	u.commit(t, "main", "initial")
	return u
}

// commit adds a commit on branch in the work tree (creating the branch from
// the current HEAD when missing) and pushes it.
func (u *upstream) commit(t *testing.T, branch, msg string) string {
	t.Helper()
	return commitAndPush(t, u.work, branch, msg)
}

// commitAndPush commits on branch in a work tree and pushes it to origin.
func commitAndPush(t *testing.T, work, branch, msg string) string {
	t.Helper()
	if cur := git(t, work, "symbolic-ref", "--short", "HEAD"); cur != branch {
		if git(t, work, "branch", "--list", branch) == "" {
			git(t, work, "checkout", "--quiet", "-b", branch)
		} else {
			git(t, work, "checkout", "--quiet", branch)
		}
	}
	git(t, work, "commit", "--quiet", "--allow-empty", "-m", msg)
	git(t, work, "push", "--quiet", "--force", "origin", branch)
	return git(t, work, "rev-parse", "HEAD")
}

// rev returns the object id of ref in a bare repository, or "".
func rev(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = dir
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// localClone clones a Soft Serve bare repository into a work tree, for
// committing "local" work to it.
func localClone(t *testing.T, bare string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "local")
	git(t, filepath.Dir(work), "clone", "--quiet", bare, work)
	return work
}
