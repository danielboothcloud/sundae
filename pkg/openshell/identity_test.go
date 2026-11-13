package openshell

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
)

func TestTOMLString(t *testing.T) {
	for in, want := range map[string]string{
		"Jane Doe":       `"Jane Doe"`,
		"1":              `"1"`,
		`Jane "JD" O'Do`: `"Jane \"JD\" O'Do"`,
		`a\b`:            `"a\\b"`,
		"tab\there\x7f":  `"tab\u0009here\u007F"`,
		"Jané":           `"Jané"`,
	} {
		if got := tomlString(in); got != want {
			t.Errorf("tomlString(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestStartGitIdentity(t *testing.T) {
	ctx := context.Background()
	id := config.GitIdentity{Name: `Jane "JD" O'Doe`, Email: "1+jane@users.noreply.github.com"}

	for _, tc := range []struct {
		vcs  proto.VCS
		tool string
	}{{proto.VCSJJ, "jj"}, {proto.VCSGit, "git"}} {
		t.Run(tc.tool, func(t *testing.T) {
			h := newHarness(t)
			s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "team/r", VCS: tc.vcs, GitIdentity: id})
			if err != nil {
				t.Fatal(err)
			}
			b := h.exec.boxes[s.Sandbox]

			// The clone carries the identity, in the environment only.
			clones := h.exec.clones()
			if len(clones) != 1 || clones[0].script() != gitIdentityCloneName {
				t.Fatalf("clones = %v", clones)
			}
			c := clones[0]
			if got := strings.Join(c.args(), " "); got != tc.tool+" http://sundae.svc:23232/team/r.git /sandbox/team/r" {
				t.Fatalf("clone args = %q", got)
			}
			if env(c.opts, "SUNDAE_GIT_USER_NAME") != id.Name || env(c.opts, "SUNDAE_GIT_USER_EMAIL") != id.Email ||
				env(c.opts, "SUNDAE_JJ_USER_NAME") != `"Jane \"JD\" O'Doe"` || env(c.opts, "SUNDAE_JJ_USER_EMAIL") != `"1+jane@users.noreply.github.com"` {
				t.Fatalf("clone env = %v", c.opts.Env)
			}
			if c.opts.Env["HOME"] != SandboxRoot {
				t.Fatalf("clone HOME = %q", c.opts.Env["HOME"])
			}
			for _, flag := range map[string][]string{
				"jj":  {`jj git clone --colocate --config "user.name=$name" --config "user.email=$email" "$2" "$3"`},
				"git": {`git clone -c "user.name=$name" -c "user.email=$email" "$2" "$3"`},
			}[tc.tool] {
				if !strings.Contains(c.command[2], flag) {
					t.Fatalf("clone script lacks %q", flag)
				}
			}

			// Then the identity is set in the checkout, before osewa.
			want := []string{gitCredentialsName, gitIdentityCloneName, gitIdentityName, osewaProbeName}
			if got := h.exec.scripts(s.Sandbox); strings.Join(got[:4], ",") != strings.Join(want, ",") {
				t.Fatalf("scripts = %v, want %v first", got, want)
			}
			if got := h.exec.last(s.Sandbox, gitIdentityName).args(); strings.Join(got, " ") != "/sandbox/team/r" {
				t.Fatalf("identity args = %q", got)
			}
			if b.identity["/sandbox/team/r"] != id.String() {
				t.Fatalf("identity = %q", b.identity["/sandbox/team/r"])
			}

			// No argument of any exec holds the identity.
			for _, r := range h.exec.runs {
				for _, arg := range r.command {
					if strings.Contains(arg, "Jane") || strings.Contains(arg, "noreply") {
						t.Fatalf("identity in argument of %q", r.command)
					}
				}
			}

			// Every attach sets it again, with whatever is configured now.
			id2 := config.GitIdentity{Name: "Jane", Email: "jane@example.com"}
			if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "team/r", VCS: tc.vcs, GitIdentity: id2}); err != nil {
				t.Fatal(err)
			}
			if n := h.exec.count(s.Sandbox, gitIdentityName); n != 2 {
				t.Fatalf("identity writes = %d", n)
			}
			if len(h.exec.clones()) != 1 {
				t.Fatal("attach cloned again")
			}
			if b.identity["/sandbox/team/r"] != id2.String() || b.jjIdentity["/sandbox/team/r"] != `"Jane" "jane@example.com"` {
				t.Fatalf("identity after attach = %q / %q", b.identity["/sandbox/team/r"], b.jjIdentity["/sandbox/team/r"])
			}
		})
	}
}

func TestStartWithoutGitIdentity(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	// Only a name is not an identity either.
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", VCS: proto.VCSJJ, GitIdentity: config.GitIdentity{Name: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	// The plain clone, and nothing set.
	clones := h.exec.clones()
	if len(clones) != 1 || strings.Join(clones[0].command, " ") != "jj git clone --colocate http://sundae.svc:23232/r.git /sandbox/r" {
		t.Fatalf("clones = %v", clones)
	}
	if n := h.exec.count(s.Sandbox, gitIdentityName); n != 0 {
		t.Fatalf("identity writes = %d", n)
	}
}

func TestGitIdentityFailureIsAWarning(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.exec.identityExit = 1
	var progress bytes.Buffer
	id := config.GitIdentity{Name: "Jane", Email: "jane@example.com"}
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", GitIdentity: id, Progress: &progress})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Started || !strings.Contains(progress.String(), "Warning: could not set the git identity Jane <jane@example.com> in /sandbox/r") ||
		!strings.Contains(progress.String(), "could not lock config file") {
		t.Fatalf("started = %v, progress = %q", s.Started, progress.String())
	}
}

func TestGitIdentityCloneFailure(t *testing.T) {
	h := newHarness(t)
	h.exec.cloneExit = 128
	_, err := h.mgr.Start(context.Background(), StartOptions{User: alice, Repo: "r", GitIdentity: config.GitIdentity{Name: "Jane", Email: "jane@example.com"}})
	if err == nil || !strings.Contains(err.Error(), gitIdentityCloneName) || !strings.Contains(err.Error(), "fatal: boom") {
		t.Fatalf("err = %v", err)
	}
}

// The identity's environment stays within the gateway's limits for the
// longest identity the config accepts, and has no control characters.
func TestGitIdentityEnvLimits(t *testing.T) {
	id := config.GitIdentity{
		Name:  strings.Repeat(`"\'é`, config.MaxGitIdentityLen/5),
		Email: strings.Repeat("a", config.MaxGitIdentityLen-12) + "@example.com",
	}
	e := osewaEnv()
	for k, v := range gitIdentityEnv(id) {
		e[k] = v
	}
	command := []string{"sh", "-c", gitIdentityCloneScript, gitIdentityCloneName, "jj", "http://sundae.svc:23232/" + strings.Repeat("r/", 100) + "r.git", "/sandbox/x"}
	if err := gatewayLimits(command, ExecOptions{Env: e, WorkDir: SandboxRoot}); err != nil {
		t.Fatal(err)
	}
	command = []string{"sh", "-c", gitIdentityScript, gitIdentityName, "/sandbox/x"}
	if err := gatewayLimits(command, ExecOptions{Env: e, WorkDir: SandboxRoot}); err != nil {
		t.Fatal(err)
	}
}

// localIdentityEnv is the environment the scripts run with on this machine:
// a temporary home, no system git config, and the identity.
func localIdentityEnv(home string, id config.GitIdentity) []string {
	env := []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(home),
	}
	for k, v := range gitIdentityEnv(id) {
		env = append(env, k+"="+v)
	}
	return env
}

// The identity scripts, run for real against a local bare repository: the
// first jj clone's working-copy commit is authored by the identity, later
// commits by the one set on attach, in git and in jj, and neither the
// user's ~/.gitconfig nor their jj user config is touched.
func TestGitIdentityScriptsLocal(t *testing.T) {
	for _, tool := range []string{"sh", "git", "jj", "base64"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	root := t.TempDir()
	home := filepath.Join(root, "sandbox")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	upstream := filepath.Join(root, "up.git")

	run := func(dir string, env []string, name string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), name, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %q: %v\n%s", name, args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	plain := localIdentityEnv(home, config.GitIdentity{})
	seed := append(plain[:len(plain):len(plain)],
		"GIT_AUTHOR_NAME=Seed", "GIT_AUTHOR_EMAIL=seed@example.com",
		"GIT_COMMITTER_NAME=Seed", "GIT_COMMITTER_EMAIL=seed@example.com")
	run(root, plain, "git", "init", "--quiet", "--bare", "--initial-branch=main", upstream)
	work := filepath.Join(root, "seed")
	run(root, plain, "git", "clone", "--quiet", upstream, work)
	run(work, seed, "git", "commit", "--quiet", "--allow-empty", "-m", "initial")
	run(work, seed, "git", "push", "--quiet", "origin", "HEAD:main")

	// A name jj would read as a TOML integer if unquoted.
	first := config.GitIdentity{Name: "1", Email: "1+jane@users.noreply.github.com"}
	second := config.GitIdentity{Name: `Jane "JD" O'Doe`, Email: "jane@example.com"}
	script := func(id config.GitIdentity, name, body string, args ...string) {
		t.Helper()
		run(home, localIdentityEnv(home, id), "sh", append([]string{"-c", body, name}, args...)...)
	}

	t.Run("jj", func(t *testing.T) {
		checkout := filepath.Join(home, "team", "j")
		if err := os.MkdirAll(filepath.Dir(checkout), 0o755); err != nil {
			t.Fatal(err)
		}
		script(first, gitIdentityCloneName, gitIdentityCloneScript, "jj", upstream, checkout)
		script(first, gitIdentityName, gitIdentityScript, checkout)
		author := func(rev string) string {
			return run(checkout, plain, "jj", "log", "--no-graph", "-r", rev, "-T", `author.name() ++ " <" ++ author.email() ++ ">"`)
		}
		// The working-copy commit jj made while cloning.
		if got := author("@"); got != first.String() {
			t.Fatalf("clone's working-copy author = %q, want %q", got, first.String())
		}
		run(checkout, plain, "jj", "describe", "-m", "first")
		if got := author("@"); got != first.String() {
			t.Fatalf("described author = %q", got)
		}

		// Attach with another identity: new commits use it, in jj and in
		// the colocated git repository.
		script(second, gitIdentityName, gitIdentityScript, checkout)
		run(checkout, plain, "jj", "new", "-m", "second")
		if got := author("@"); got != second.String() {
			t.Fatalf("new commit's author = %q, want %q", got, second.String())
		}
		if got := run(checkout, plain, "git", "config", "--local", "user.name"); got != second.Name {
			t.Fatalf("colocated git user.name = %q", got)
		}
		if got := run(checkout, plain, "jj", "config", "list", "--user"); strings.Contains(got, "user.") {
			t.Fatalf("jj user config changed: %q", got)
		}
	})

	t.Run("git", func(t *testing.T) {
		checkout := filepath.Join(home, "g")
		script(first, gitIdentityCloneName, gitIdentityCloneScript, "git", upstream, checkout)
		if got := run(checkout, plain, "git", "config", "--local", "user.name"); got != first.Name {
			t.Fatalf("cloned user.name = %q", got)
		}
		script(second, gitIdentityName, gitIdentityScript, checkout)
		run(checkout, plain, "git", "commit", "--quiet", "--allow-empty", "-m", "work")
		got := run(checkout, plain, "git", "log", "-1", "--format=%an <%ae>|%cn <%ce>")
		if got != second.String()+"|"+second.String() {
			t.Fatalf("commit identity = %q", got)
		}
		if _, err := os.Stat(filepath.Join(checkout, ".jj")); err == nil {
			t.Fatal("the identity script made a jj repository")
		}
	})

	t.Run("no checkout yet", func(t *testing.T) {
		missing := filepath.Join(home, "missing")
		script(second, gitIdentityName, gitIdentityScript, missing)
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Fatalf("created %s: %v", missing, err)
		}
		notRepo := filepath.Join(home, "plain")
		if err := os.Mkdir(notRepo, 0o755); err != nil {
			t.Fatal(err)
		}
		script(second, gitIdentityName, gitIdentityScript, notRepo)
		if entries, _ := os.ReadDir(notRepo); len(entries) != 0 {
			t.Fatalf("wrote into a directory that is no repository: %v", entries)
		}
	})

	// The user's ~/.gitconfig was never written.
	if _, err := os.Stat(filepath.Join(home, ".gitconfig")); !os.IsNotExist(err) {
		t.Fatalf("~/.gitconfig: %v", err)
	}
}
