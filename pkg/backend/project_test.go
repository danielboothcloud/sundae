package backend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/access"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/db"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/matryer/is"
)

// git runs git in dir and returns its trimmed stdout, failing the test on
// error.
func testGit(t *testing.T, dir string, stdin string, env []string, args ...string) string {
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

// writeTree writes files (path → contents) into the bare repository at dir
// and returns the root tree.
func writeTree(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "index")}
	for p, content := range files {
		id := testGit(t, dir, content, nil, "hash-object", "-w", "--stdin")
		testGit(t, dir, "", env, "update-index", "--add", "--cacheinfo", "100644,"+id+","+p)
	}
	return testGit(t, dir, "", env, "write-tree")
}

var templateAuthor = []string{
	"GIT_AUTHOR_NAME=Template Author", "GIT_AUTHOR_EMAIL=tpl@example.com",
	"GIT_COMMITTER_NAME=Template Author", "GIT_COMMITTER_EMAIL=tpl@example.com",
}

// projectFixture is a backend with a template repository "tpl" holding
// two commits on main (tagged v1 and v2), configured as templates, and the
// users alice and bob.
type projectFixture struct {
	be         *Backend
	cfg        *config.Config
	alice, bob proto.User
	// v1 and v2 are the template's commits; v1Only is a blob only v1 has.
	v1, v2, v1Only string
}

func newProjectFixture(t *testing.T) *projectFixture {
	t.Helper()
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	f := &projectFixture{be: be, cfg: cfg}

	var err error
	f.alice, err = be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	f.bob, err = be.CreateUser(ctx, "bob", proto.UserOptions{})
	is.NoErr(err)

	tpl, err := be.CreateRepository(ctx, "tpl", f.alice, proto.RepositoryOptions{Vcs: proto.VCSGit})
	is.NoErr(err)
	dir := be.repoPath(tpl.Name())
	tree1 := writeTree(t, dir, map[string]string{"README.md": "old\n", "old.txt": "only in v1\n"})
	f.v1Only = testGit(t, dir, "only in v1\n", nil, "hash-object", "--stdin")
	f.v1 = testGit(t, dir, "", templateAuthor, "commit-tree", "-m", "v1", tree1)
	tree2 := writeTree(t, dir, map[string]string{
		"README.md":           "# Template\n",
		"go.mod":              "module example.com/x\n",
		"service/main.go":     "package main\n",
		"service/cmd/run.txt": "run\n",
	})
	f.v2 = testGit(t, dir, "", templateAuthor, "commit-tree", "-p", f.v1, "-m", "v2", tree2)
	testGit(t, dir, "", nil, "update-ref", "refs/heads/main", f.v2)
	testGit(t, dir, "", nil, "symbolic-ref", "HEAD", "refs/heads/main")
	testGit(t, dir, "", nil, "update-ref", "refs/tags/v1", f.v1)

	cfg.Sessions.Profiles = map[string]config.SessionProfile{"claude": {Command: "claude"}, "shell": {}}
	cfg.Projects.Templates = map[string]config.ProjectTemplate{
		"full":    {Repo: "tpl"},
		"service": {Repo: "tpl", Subdir: "service", Profile: "claude"},
		"old":     {Repo: "tpl", Ref: "v1"},
		"no-ref":  {Repo: "tpl", Ref: "nope"},
		"no-dir":  {Repo: "tpl", Subdir: "nope"},
		"file":    {Repo: "tpl", Subdir: "go.mod"},
		"missing": {Repo: "gone"},
	}
	return f
}

// files lists the files at HEAD of the repository.
func (f *projectFixture) files(t *testing.T, name string) []string {
	t.Helper()
	out := testGit(t, f.be.repoPath(name), "", nil, "ls-tree", "-r", "--name-only", "HEAD")
	return strings.Split(out, "\n")
}

func TestCreateProjectFromTemplate(t *testing.T) {
	for _, vcs := range []proto.VCS{proto.VCSGit, proto.VCSJJ} {
		t.Run(string(vcs), func(t *testing.T) {
			is := is.New(t)
			f := newProjectFixture(t)
			ctx := proto.WithUserContext(context.Background(), f.alice)

			r, err := f.be.CreateProject(ctx, "app", f.alice, ProjectOptions{
				RepositoryOptions: proto.RepositoryOptions{Vcs: vcs, Description: "my app"},
				Template:          "full",
			})
			is.NoErr(err)
			is.Equal(r.Vcs(), vcs)
			is.Equal(r.Description(), "my app")
			dir := f.be.repoPath("app")

			// The tree is copied as it is at the template's HEAD.
			is.Equal(f.files(t, "app"), []string{"README.md", "go.mod", "service/cmd/run.txt", "service/main.go"})
			is.Equal(testGit(t, dir, "", nil, "show", "HEAD:README.md"), "# Template")
			// One commit, with no parent, on the branch HEAD names.
			is.Equal(testGit(t, dir, "", nil, "rev-list", "--count", "HEAD"), "1")
			is.Equal(testGit(t, dir, "", nil, "log", "-1", "--format=%P"), "")
			is.True(strings.HasPrefix(testGit(t, dir, "", nil, "symbolic-ref", "HEAD"), "refs/heads/"))
			is.Equal(testGit(t, dir, "", nil, "for-each-ref", "--format=%(refname)"), testGit(t, dir, "", nil, "symbolic-ref", "HEAD"))
			// Authored and committed by the creator.
			is.Equal(testGit(t, dir, "", nil, "log", "-1", "--format=%an <%ae>|%cn <%ce>"), "alice <alice@localhost>|alice <alice@localhost>")
			is.True(strings.HasPrefix(testGit(t, dir, "", nil, "log", "-1", "--format=%B"), "Initial commit from template full\n\nFrom tpl at "+f.v2[:12]+"."))
			// None of the template's history came along.
			for _, id := range []string{f.v1, f.v2, f.v1Only} {
				cmd := exec.CommandContext(t.Context(), "git", "cat-file", "-e", id)
				cmd.Dir = dir
				is.True(cmd.Run() != nil) // object copied: id
			}
			// The repository lists the commit's time as its update time.
			info, err := os.ReadFile(dir + "/info/last-modified")
			is.NoErr(err)
			is.True(len(info) > 0)
		})
	}
}

func TestCreateProjectRefAndSubdir(t *testing.T) {
	is := is.New(t)
	f := newProjectFixture(t)
	ctx := context.Background()

	_, err := f.be.CreateProject(ctx, "svc", f.alice, ProjectOptions{Template: "service"})
	is.NoErr(err)
	is.Equal(f.files(t, "svc"), []string{"cmd/run.txt", "main.go"})
	is.True(strings.Contains(testGit(t, f.be.repoPath("svc"), "", nil, "log", "-1", "--format=%B"), "directory service."))
	// The template's profile became the repository's.
	p, err := f.be.RepoProfile(ctx, "svc")
	is.NoErr(err)
	is.Equal(p, "claude")

	_, err = f.be.CreateProject(ctx, "old", f.alice, ProjectOptions{Template: "old", Profile: "shell"})
	is.NoErr(err)
	is.Equal(f.files(t, "old"), []string{"README.md", "old.txt"})
	// An explicit profile wins over the template's.
	p, err = f.be.RepoProfile(ctx, "old")
	is.NoErr(err)
	is.Equal(p, "shell")
}

func TestCreateProjectErrors(t *testing.T) {
	f := newProjectFixture(t)
	ctx := context.Background()
	is.New(t).NoErr(f.be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return f.be.store.SetRepoIsPrivateByName(ctx, tx, "tpl", true)
	}))

	cases := []struct {
		name string
		user proto.User
		opts ProjectOptions
		want string
		is   error
	}{
		{"unknown template", f.alice, ProjectOptions{Template: "nope"}, `unknown template "nope": use one of file, full, missing, no-dir, no-ref, old, service`, ErrUnknownTemplate},
		{"template repository missing", f.alice, ProjectOptions{Template: "missing"}, `template "missing": repository "gone": repository not found`, proto.ErrRepoNotFound},
		{"permission denied", f.bob, ProjectOptions{Template: "full"}, `template "full": unauthorized`, proto.ErrUnauthorized},
		{"anonymous denied", nil, ProjectOptions{Template: "full"}, `template "full": unauthorized`, proto.ErrUnauthorized},
		{"ref missing", f.alice, ProjectOptions{Template: "no-ref"}, `template "no-ref": ref "nope" not found in "tpl"`, nil},
		{"subdir missing", f.alice, ProjectOptions{Template: "no-dir"}, `template "no-dir": "tpl" has no directory "nope"`, nil},
		{"subdir is a file", f.alice, ProjectOptions{Template: "file"}, `template "file": "go.mod" is not a directory in "tpl"`, nil},
		{"unknown profile", f.alice, ProjectOptions{Profile: "nope"}, `unknown profile "nope": use one of claude, shell`, config.ErrUnknownProfile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			is := is.New(t)
			_, err := f.be.CreateProject(ctx, "x", tc.user, tc.opts)
			is.True(err != nil)
			is.Equal(err.Error(), tc.want)
			if tc.is != nil {
				is.True(errors.Is(err, tc.is))
			}
			// Nothing was created.
			_, err = f.be.Repository(ctx, "x")
			is.True(errors.Is(err, proto.ErrRepoNotFound))
		})
	}

	// A collaborator may read the private template.
	is := is.New(t)
	is.NoErr(f.be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return f.be.store.AddCollabByUsernameAndRepo(ctx, tx, "bob", "tpl", access.ReadOnlyAccess)
	}))
	_, err := f.be.CreateProject(ctx, "x", f.bob, ProjectOptions{Template: "full"})
	is.NoErr(err)
	is.Equal(testGit(t, f.be.repoPath("x"), "", nil, "log", "-1", "--format=%an"), "bob")

	// An existing repository is not touched.
	_, err = f.be.CreateProject(ctx, "x", f.alice, ProjectOptions{Template: "old"})
	is.True(errors.Is(err, proto.ErrRepoExist))
	is.Equal(f.files(t, "x"), []string{"README.md", "go.mod", "service/cmd/run.txt", "service/main.go"})
}

func TestCreateProjectEmptyTemplate(t *testing.T) {
	is := is.New(t)
	f := newProjectFixture(t)
	ctx := context.Background()
	_, err := f.be.CreateRepository(ctx, "empty", f.alice, proto.RepositoryOptions{})
	is.NoErr(err)
	f.cfg.Projects.Templates["empty"] = config.ProjectTemplate{Repo: "empty"}
	_, err = f.be.CreateProject(ctx, "x", f.alice, ProjectOptions{Template: "empty"})
	is.True(err != nil)
	is.Equal(err.Error(), `template "empty": repository "empty" has no commits`)
}

func TestRepoProfileStorage(t *testing.T) {
	is := is.New(t)
	f := newProjectFixture(t)
	ctx := context.Background()

	r, err := f.be.CreateProject(ctx, "plain", f.alice, ProjectOptions{})
	is.NoErr(err)
	// No template: an empty repository.
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "--verify", "HEAD")
	cmd.Dir = f.be.repoPath(r.Name())
	is.True(cmd.Run() != nil)

	p, err := f.be.RepoProfile(ctx, "plain")
	is.NoErr(err)
	is.Equal(p, "")

	is.NoErr(f.be.SetRepoProfile(ctx, "plain", "claude"))
	p, err = f.be.RepoProfile(ctx, "plain.git")
	is.NoErr(err)
	is.Equal(p, "claude")
	is.Equal(testGit(t, f.be.repoPath("plain"), "", nil, "config", "--file", "config", "session.profile"), "claude")

	err = f.be.SetRepoProfile(ctx, "plain", "nope")
	is.True(errors.Is(err, config.ErrUnknownProfile))
	p, _ = f.be.RepoProfile(ctx, "plain")
	is.Equal(p, "claude")

	is.NoErr(f.be.SetRepoProfile(ctx, "plain", ""))
	is.NoErr(f.be.SetRepoProfile(ctx, "plain", "")) // unsetting twice is fine
	p, err = f.be.RepoProfile(ctx, "plain")
	is.NoErr(err)
	is.Equal(p, "")

	_, err = f.be.RepoProfile(ctx, "gone")
	is.True(errors.Is(err, proto.ErrRepoNotFound))
	is.True(errors.Is(f.be.SetRepoProfile(ctx, "gone", "claude"), proto.ErrRepoNotFound))
}

func TestCommitIdentity(t *testing.T) {
	is := is.New(t)
	f := newProjectFixture(t)
	f.cfg.SSH.PublicURL = "ssh://git.example.com:2222"
	name, email := f.be.CommitIdentity(f.alice)
	is.Equal(name+" <"+email+">", "alice <alice@git.example.com>")
	f.cfg.Name = "My <Server>"
	name, email = f.be.CommitIdentity(nil)
	is.Equal(name+" <"+email+">", "My Server <sundae@git.example.com>")
}

// A failure after the repository was created removes it again.
func TestCreateProjectUndoesOnFailure(t *testing.T) {
	is := is.New(t)
	f := newProjectFixture(t)
	ctx := context.Background()
	dir := f.be.repoPath("tpl")
	// A tree whose blob is missing resolves, but cannot be copied.
	missing := strings.Repeat("1", 40)
	tree := testGit(t, dir, "100644 blob "+missing+"\tghost\n", nil, "mktree", "--missing")
	commit := testGit(t, dir, "", templateAuthor, "commit-tree", "-m", "broken", tree)
	testGit(t, dir, "", nil, "update-ref", "refs/heads/broken", commit)
	f.cfg.Projects.Templates["broken"] = config.ProjectTemplate{Repo: "tpl", Ref: "broken"}

	_, err := f.be.CreateProject(ctx, "x", f.alice, ProjectOptions{Template: "broken"})
	is.True(err != nil)
	is.True(strings.HasPrefix(err.Error(), `template "broken": git rev-list`))
	_, err = f.be.Repository(ctx, "x")
	is.True(errors.Is(err, proto.ErrRepoNotFound))
	_, err = os.Stat(f.be.repoPath("x"))
	is.True(errors.Is(err, os.ErrNotExist))
	// The name can be used again.
	_, err = f.be.CreateProject(ctx, "x", f.alice, ProjectOptions{Template: "full"})
	is.NoErr(err)
}
