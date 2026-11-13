package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/openshell"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/matryer/is"
)

// gitIn runs git in dir and returns its trimmed stdout.
func gitIn(t *testing.T, dir, stdin string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("git %v: %v: %s", args, err, ee.Stderr)
		}
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// repoDir returns the path of a repository.
func repoDir(ctx context.Context, name string) string {
	return filepath.Join(config.FromContext(ctx).DataPath, "repos", name+".git")
}

// makeTemplate creates the repository name owned by owner with one commit
// holding files on main, and declares it as the template "base".
func makeTemplate(t *testing.T, ctx context.Context, be *backend.Backend, owner proto.User, name string, private bool, files map[string]string) {
	t.Helper()
	is := is.New(t)
	_, err := be.CreateRepository(ctx, name, owner, proto.RepositoryOptions{Private: private, Vcs: proto.VCSGit})
	is.NoErr(err)
	dir := repoDir(ctx, name)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "index")}
	for p, content := range files {
		id := gitIn(t, dir, content, nil, "hash-object", "-w", "--stdin")
		gitIn(t, dir, "", env, "update-index", "--add", "--cacheinfo", "100644,"+id+","+p)
	}
	tree := gitIn(t, dir, "", env, "write-tree")
	commit := gitIn(t, dir, "", []string{
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	}, "commit-tree", "-m", "template", tree)
	gitIn(t, dir, "", nil, "update-ref", "refs/heads/main", commit)
	gitIn(t, dir, "", nil, "symbolic-ref", "HEAD", "refs/heads/main")

	cfg := config.FromContext(ctx)
	if cfg.Projects.Templates == nil {
		cfg.Projects.Templates = map[string]config.ProjectTemplate{}
	}
	cfg.Projects.Templates["base"] = config.ProjectTemplate{Repo: name, Description: "the base"}
}

func TestRepoCreateTemplateProfileStart(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	makeTemplate(t, ctx, be, user, "tpl", false, map[string]string{"README.md": "hi\n", "src/main.go": "package main\n"})
	ctx = context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true})

	stdout, stderr, err := runRepoOutput(t, ctx, "create", "app", "--template", "base", "--profile", "claude", "--start", "--vcs", "jj")
	is.NoErr(err)
	is.True(strings.Contains(stdout, "jj git clone"))
	for _, want := range []string{
		"Created repository app", "Initialised from template base", "Session profile: claude",
		"Starting sandbox soft-alice-app", "Session main of app is running", "Attach with: session attach app",
	} {
		is.True(strings.Contains(stderr, want)) // missing: want
	}

	dir := repoDir(ctx, "app")
	is.Equal(gitIn(t, dir, "", nil, "ls-tree", "-r", "--name-only", "HEAD"), "README.md\nsrc/main.go")
	is.Equal(gitIn(t, dir, "", nil, "log", "--format=%an", "HEAD"), "alice")

	p, err := be.RepoProfile(ctx, "app")
	is.NoErr(err)
	is.Equal(p, "claude")

	// The session was started detached, from the profile.
	sb := fc.sandboxes["soft-alice-app"]
	is.True(sb != nil)
	is.Equal(sb.Annotations[openshell.AnnotationProfile], "claude")
	is.Equal(len(fc.interactive), 0)
}

func TestRepoCreateStartChecks(t *testing.T) {
	is := is.New(t)
	ctx, be, _, fc := newSessionTestContext(t)

	// Without sessions, --start fails before anything is created.
	config.FromContext(ctx).OpenShell.Enabled = false
	_, _, err := runRepoOutput(t, ctx, "create", "r1", "--start")
	is.True(errors.Is(err, errStartNeedsSessions))
	_, err = be.Repository(ctx, "r1")
	is.True(errors.Is(err, proto.ErrRepoNotFound))
	config.FromContext(ctx).OpenShell.Enabled = true

	// A failed start leaves the repository, and says so.
	fc.ensureErr = errors.New("gateway down")
	_, _, err = runRepoOutput(t, ctx, "create", "r2", "--start")
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "repository r2 was created, but its session did not start: gateway down"))
	_, err = be.Repository(ctx, "r2")
	is.NoErr(err)
}

func TestRepoCreateTemplateErrors(t *testing.T) {
	is := is.New(t)
	ctx, be, _, _ := newSessionTestContext(t)
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)
	makeTemplate(t, ctx, be, admin, "private-tpl", true, map[string]string{"a": "b"})

	_, _, err = runRepoOutput(t, ctx, "create", "x", "--template", "nope")
	is.True(errors.Is(err, backend.ErrUnknownTemplate))

	// alice cannot read the private template.
	_, _, err = runRepoOutput(t, ctx, "create", "x", "--template", "base")
	is.True(errors.Is(err, proto.ErrUnauthorized))

	_, _, err = runRepoOutput(t, ctx, "create", "x", "--profile", "nope")
	is.True(errors.Is(err, config.ErrUnknownProfile))

	_, err = be.Repository(ctx, "x")
	is.True(errors.Is(err, proto.ErrRepoNotFound))

	// Plain creation is unchanged.
	stdout, stderr, err := runRepoOutput(t, ctx, "create", "x", "--vcs", "git")
	is.NoErr(err)
	is.True(strings.HasSuffix(stdout, "/x.git\n") && !strings.Contains(stdout, "jj"))
	is.True(!strings.Contains(stderr, "template") && !strings.Contains(stderr, "profile"))
}

func TestRepoProfileCommand(t *testing.T) {
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	cfg := config.FromContext(ctx)
	cfg.Sessions.Profiles["shell"] = config.SessionProfile{Command: "zsh"}
	cfg.Sessions.Repos = map[string]config.SessionRepoDefaults{"r": {Profile: "claude"}}
	_, err := be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)

	stdout, _, err := runRepoOutput(t, ctx, "profile", "r")
	is.NoErr(err)
	is.Equal(stdout, "\n")

	is.NoErr(runRepo(t, ctx, "profile", "r", "shell"))
	stdout, _, err = runRepoOutput(t, ctx, "profile", "r.git")
	is.NoErr(err)
	is.Equal(stdout, "shell\n")

	err = runRepo(t, ctx, "profile", "r", "nope")
	is.True(err != nil && strings.Contains(err.Error(), `unknown profile "nope": use one of claude, shell`))
	is.True(runRepo(t, ctx, "profile", "r", "shell", "--unset") != nil)

	// The repository's profile beats the sessions.repos glob; --profile
	// beats both.
	noPty := context.WithValue(ctx, sshutils.ContextKeySession, &fakeSSHSession{noPty: true})
	_, _, err = runSessionCmd(t, noPty, "start", "r", "-d")
	is.NoErr(err)
	is.Equal(fc.sandboxes["soft-alice-r"].Annotations[openshell.AnnotationProfile], "shell")
	_, _, err = runSessionCmd(t, noPty, "start", "r", "-d", "--name", "two", "--profile", "claude")
	is.NoErr(err)
	is.Equal(fc.sandboxes[openshell.SessionSandboxName("alice", "r", "two")].Annotations[openshell.AnnotationProfile], "claude")

	// Unset: the glob applies again.
	is.NoErr(runRepo(t, ctx, "profile", "r", "--unset"))
	stdout, _, err = runRepoOutput(t, ctx, "profile", "r")
	is.NoErr(err)
	is.Equal(stdout, "\n")
	_, _, err = runSessionCmd(t, noPty, "start", "r", "-d", "--name", "three")
	is.NoErr(err)
	is.Equal(fc.sandboxes[openshell.SessionSandboxName("alice", "r", "three")].Annotations[openshell.AnnotationProfile], "claude")

	// A reader who is not a collaborator may read but not set it; a
	// private repository is not found.
	bob := withUser(t, ctx, be, "bob", false)
	_, _, err = runRepoOutput(t, bob, "profile", "r")
	is.NoErr(err)
	is.True(errors.Is(runRepo(t, bob, "profile", "r", "claude"), proto.ErrUnauthorized))
	is.True(errors.Is(runRepo(t, bob, "profile", "r", "--unset"), proto.ErrUnauthorized))
	_, err = be.CreateRepository(ctx, "secret", user, proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	is.True(errors.Is(runRepo(t, bob, "profile", "secret"), proto.ErrRepoNotFound))
}
