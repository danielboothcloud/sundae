package openshell

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/config"
	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ghToken = "ghp_forge-secret-1"
	glToken = "glpat-forge-secret-2"
)

// withForges declares a github.com and a self-hosted GitLab forge with
// token files, returning the github token file's path.
func withForges(t *testing.T, cfg *config.Config) string {
	t.Helper()
	dir := t.TempDir()
	gh, gl := filepath.Join(dir, "gh"), filepath.Join(dir, "gl")
	for f, v := range map[string]string{gh: ghToken + "\n", gl: glToken} {
		if err := os.WriteFile(f, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg.Forges = []config.ForgeConfig{
		{Name: "github", Kind: config.ForgeKindGitHub, URL: "https://github.com", TokenFile: gh},
		{Name: "gitlab", Kind: config.ForgeKindGitLab, URL: "https://gitlab.example.com", TokenFile: gl},
	}
	return gh
}

var (
	ghRemote = &ForgeRemote{Forge: "github", Kind: config.ForgeKindGitHub, URL: "https://github.com", CloneURL: "https://github.com/me/tool.git"}
	glRemote = &ForgeRemote{Forge: "gitlab", Kind: config.ForgeKindGitLab, URL: "https://gitlab.example.com", CloneURL: "https://gitlab.example.com/g/p.git"}
)

// assertForgeTokensConfined fails when a forge token is in an exec
// argument, in an exec environment other than allowed's, or in the attach
// environment.
func assertForgeTokensConfined(t *testing.T, ex *fakeExec, allowed func(execCall, string) bool) {
	t.Helper()
	ex.mu.Lock()
	defer ex.mu.Unlock()
	for _, secret := range []string{ghToken, glToken} {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + secret))
		for _, r := range ex.runs {
			for _, arg := range r.command {
				if strings.Contains(arg, secret) || strings.Contains(arg, basic) {
					t.Fatalf("forge token in argument of %q", r.command)
				}
			}
			for k, v := range r.opts.Env {
				decoded, _ := base64.StdEncoding.DecodeString(v)
				if (strings.Contains(v, secret) || strings.Contains(string(decoded), secret)) && !allowed(r, k) {
					t.Fatalf("forge token in env %s of %q", k, r.command)
				}
			}
		}
		for _, r := range ex.interactive {
			for k, v := range r.opts.Env {
				if strings.Contains(v, secret) {
					t.Fatalf("forge token in attach env %s", k)
				}
			}
		}
	}
}

// A forge-linked repository is cloned with jj, remote sundae, and gets its
// origin remote on every start and attach; only the session's forges get a
// token, in /tmp/sundae, never in an argument.
func TestForgeSessionOpenShell(t *testing.T) {
	ctx := context.Background()
	cfg := newHarnessConfig()
	ghFile := withForges(t, cfg)
	h := newHarnessWith(t, cfg)

	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repos: []RepoSpec{
		{Name: "github/me/tool", VCS: "git", Forge: ghRemote},
		{Name: "local", VCS: "git"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var clones []string
	for _, c := range h.exec.clones() {
		clones = append(clones, strings.Join(c.command, " "))
	}
	want := []string{
		"jj git clone --colocate --remote sundae http://sundae.svc:23232/github/me/tool.git /sandbox/github/me/tool",
		"git clone http://sundae.svc:23232/local.git /sandbox/local",
	}
	if !slices.Equal(clones, want) {
		t.Fatalf("clones = %q, want %q", clones, want)
	}
	b := h.exec.boxes[s.Sandbox]
	wantRemotes := []string{"/sandbox/github/me/tool", "http://sundae.svc:23232/github/me/tool.git", "https://github.com/me/tool.git", ForgeOriginPushURL, "1"}
	if len(b.remotes) != 1 || !slices.Equal(b.remotes["/sandbox/github/me/tool"], wantRemotes) {
		t.Fatalf("remotes = %v", b.remotes)
	}

	// The github token, scoped to github.com; no gitlab: not in the session.
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + ghToken))
	if !strings.Contains(b.files[gitCredentialFile], "[http \"https://github.com/\"]\n\textraHeader = Authorization: Basic "+cred+"\n") ||
		strings.Contains(b.files[gitCredentialFile], "gitlab") {
		t.Fatalf("credential file = %q", b.files[gitCredentialFile])
	}
	if got := b.files[forgeEnvFile]; !strings.Contains(got, "export GH_TOKEN='"+ghToken+"'\n") || strings.Contains(got, "GITLAB") || strings.Contains(got, "GH_HOST") {
		t.Fatalf("forge env = %q", got)
	}
	if !strings.HasPrefix(forgeEnvFile, gitCredentialDir+"/") {
		t.Fatalf("forge env file %s is not in %s", forgeEnvFile, gitCredentialDir)
	}
	// The session's shell sources it.
	if !strings.Contains(osewaNewScript, ". "+forgeEnvFile+";") {
		t.Fatal("osewa sessions do not source the forge env file")
	}
	if _, err := h.mgr.Attach(ctx, s, 80, 24); err != nil {
		t.Fatal(err)
	}
	assertForgeTokensConfined(t, h.exec, func(r execCall, k string) bool {
		return r.script() == gitCredentialsName && (k == "SUNDAE_GITCONFIG" || k == "SUNDAE_FORGE_ENV")
	})

	// An attach sets the remotes up again and re-reads the token file.
	if err := os.WriteFile(ghFile, []byte("ghp_rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Name: "main", ResolveRepo: func(name string) (RepoSpec, error) {
		if name == "github/me/tool" {
			return RepoSpec{Name: name, Forge: ghRemote}, nil
		}
		return RepoSpec{Name: name}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if n := h.exec.count(s.Sandbox, forgeRemotesName); n != 2 {
		t.Fatalf("remote setups = %d", n)
	}
	if !strings.Contains(b.files[forgeEnvFile], "GH_TOKEN='ghp_rotated'") {
		t.Fatalf("forge env not refreshed: %q", b.files[forgeEnvFile])
	}
	if len(h.exec.clones()) != 2 {
		t.Fatal("attach cloned again")
	}

	// Without the token, jj git fetch leaves origin out, and gets it back once
	// the token is readable again.
	if err := os.Remove(ghFile); err != nil {
		t.Fatal(err)
	}
	var progress strings.Builder
	resume := func() {
		t.Helper()
		if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Name: "main", Progress: &progress, ResolveRepo: func(name string) (RepoSpec, error) {
			if name == "github/me/tool" {
				return RepoSpec{Name: name, Forge: ghRemote}, nil
			}
			return RepoSpec{Name: name}, nil
		}}); err != nil {
			t.Fatal(err)
		}
	}
	resume()
	if got := b.remotes["/sandbox/github/me/tool"]; got[len(got)-1] != "0" {
		t.Fatalf("remotes without a token = %v", got)
	}
	if !strings.Contains(progress.String(), "Warning: no github token in the session: jj git fetch in /sandbox/github/me/tool fetches sundae only, not origin") {
		t.Fatalf("progress = %q", progress.String())
	}
	if err := os.WriteFile(ghFile, []byte(ghToken), 0o600); err != nil {
		t.Fatal(err)
	}
	progress.Reset()
	resume()
	if got := b.remotes["/sandbox/github/me/tool"]; got[len(got)-1] != "1" || strings.Contains(progress.String(), "not origin") {
		t.Fatalf("remotes with the token again = %v, progress %q", got, progress.String())
	}
}

// A forge-linked repository with a git identity clones through the
// identity script, naming the sundae remote.
func TestForgeSessionIdentityClone(t *testing.T) {
	ctx := context.Background()
	cfg := newHarnessConfig()
	withForges(t, cfg)
	h := newHarnessWith(t, cfg)
	id := config.GitIdentity{Name: "Jane", Email: "jane@example.com"}
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repos: []RepoSpec{{Name: "gitlab/g/p", VCS: "git", Forge: glRemote, GitIdentity: id}}})
	if err != nil {
		t.Fatal(err)
	}
	b := h.exec.boxes[s.Sandbox]
	if b.cloneRemote["/sandbox/gitlab/g/p"] != ForgeRemoteSundae || b.jjIdentity["/sandbox/gitlab/g/p"] == "" {
		t.Fatalf("clone remote = %v, jj identity = %v", b.cloneRemote, b.jjIdentity)
	}
	if got := b.files[forgeEnvFile]; !strings.Contains(got, "export GITLAB_TOKEN='"+glToken+"'\nexport GITLAB_HOST='gitlab.example.com'\n") {
		t.Fatalf("forge env = %q", got)
	}
}

// A failing remote setup is a warning, not a failed start.
func TestForgeRemotesFailureWarns(t *testing.T) {
	ctx := context.Background()
	cfg := newHarnessConfig()
	withForges(t, cfg)
	h := newHarnessWith(t, cfg)
	h.exec.remotesExit = 3
	var progress strings.Builder
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repos: []RepoSpec{{Name: "github/me/tool", Forge: ghRemote}}, Progress: &progress}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(progress.String(), "Warning: could not set up the sundae and origin remotes in /sandbox/github/me/tool") {
		t.Fatalf("progress = %q", progress.String())
	}
}

// The pod runtime gets the credentials on standard input: no forge token
// in the exec command (the pod exec URL), its environment, or the pod.
func TestForgeSessionPod(t *testing.T) {
	ctx := context.Background()
	h := newPodHarness(t, func(c *config.Config) { withForges(t, c) })
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Profile: "podman", Repos: []RepoSpec{
		{Name: "github/me/tool", Forge: ghRemote},
		{Name: "gitlab/g/p", Forge: glRemote},
	}})
	if err != nil {
		t.Fatal(err)
	}
	b := h.exec.boxes[s.Sandbox]
	for _, want := range []string{"GH_TOKEN='" + ghToken + "'", "GITLAB_TOKEN='" + glToken + "'", "GITLAB_HOST='gitlab.example.com'"} {
		if !strings.Contains(b.files[forgeEnvFile], want) {
			t.Fatalf("forge env = %q, want %s", b.files[forgeEnvFile], want)
		}
	}
	if !strings.Contains(b.files[gitCredentialFile], `[http "https://gitlab.example.com/"]`) {
		t.Fatalf("credential file = %q", b.files[gitCredentialFile])
	}
	var stdinCalls int
	for _, r := range h.exec.runs {
		if r.script() == gitCredentialsName {
			if r.stdin == "" || r.opts.Env["SUNDAE_GITCONFIG"] != "" {
				t.Fatalf("pod credentials not on stdin: env %v", r.opts.Env)
			}
			stdinCalls++
		}
	}
	if stdinCalls != 1 {
		t.Fatalf("credential writes = %d", stdinCalls)
	}
	assertForgeTokensConfined(t, h.exec, func(execCall, string) bool { return false })
	p, err := h.cs.CoreV1().Pods("sundae").Get(ctx, s.Sandbox, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec := p.String()
	for _, secret := range []string{ghToken, glToken} {
		if strings.Contains(spec, secret) {
			t.Fatal("forge token in the pod")
		}
	}
}

func TestForgeEnv(t *testing.T) {
	for _, tc := range []struct {
		name  string
		creds []forgeCredential
		want  string
		warns int
	}{
		{"github.com", []forgeCredential{{forge: "gh", kind: "github", url: "https://github.com", token: "t"}}, "export GH_TOKEN='t'\n", 0},
		{"ghe.com", []forgeCredential{{forge: "gh", kind: "github", url: "https://acme.ghe.com", token: "t"}}, "export GH_TOKEN='t'\nexport GH_HOST='acme.ghe.com'\n", 0},
		{"enterprise server", []forgeCredential{{forge: "ghes", kind: "github", url: "https://git.corp:8443", token: "t"}}, "export GH_ENTERPRISE_TOKEN='t'\nexport GH_HOST='git.corp:8443'\n", 0},
		{"gitlab.com", []forgeCredential{{forge: "gl", kind: "gitlab", url: "https://gitlab.com", token: "it's"}}, "export GITLAB_TOKEN='it'\\''s'\nexport GITLAB_HOST='gitlab.com'\n", 0},
		{"first wins", []forgeCredential{
			{forge: "a", kind: "github", url: "https://github.com", token: "1"},
			{forge: "b", kind: "github", url: "https://github.com", token: "2"},
			{forge: "c", kind: "gitlab", url: "https://gitlab.com", token: "3"},
		}, "export GH_TOKEN='1'\nexport GITLAB_TOKEN='3'\nexport GITLAB_HOST='gitlab.com'\n", 1},
		{"none", nil, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, warns := forgeEnv(tc.creds)
			if got != "# Written by Sundae on every session start and attach.\n"+tc.want || len(warns) != tc.warns {
				t.Fatalf("forgeEnv = %q, %q", got, warns)
			}
		})
	}
	// Two forges on one URL: git would send both headers, so one is used.
	cfg, warns := forgeGitConfig([]forgeCredential{
		{forge: "a", kind: "github", url: "https://github.com", token: "1"},
		{forge: "b", kind: "github", url: "https://github.com", token: "2"},
	})
	if strings.Count(cfg, "extraHeader") != 1 || len(warns) != 1 {
		t.Fatalf("forgeGitConfig = %q, %q", cfg, warns)
	}
}

// The remote setup, run for real with jj and git: a new clone and a
// checkout cloned before (origin = Sundae) both end with sundae for fetch
// and push, origin fetching the forge, pushes to origin failing.
func TestForgeRemotesScript(t *testing.T) {
	for _, bin := range []string{"jj", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip("no " + bin)
		}
	}
	// jj resolves symlinks in file URLs (macOS's /var is /private/var).
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(tmp, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1", "JJ_USER=t", "JJ_EMAIL=t@example.com",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	}
	run := func(dir string, args ...string) (string, error) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), args[0], args[1:]...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(dir string, args ...string) string {
		t.Helper()
		out, err := run(dir, args...)
		if err != nil {
			t.Fatalf("%q: %v: %s", args, err, out)
		}
		return out
	}
	sundae, forge := filepath.Join(tmp, "sundae.git"), filepath.Join(tmp, "forge.git")
	must(tmp, "git", "init", "-q", "--bare", sundae)
	must(tmp, "git", "init", "-q", "--bare", forge)
	tree := must(sundae, "git", "hash-object", "-t", "tree", "-w", "--stdin")
	commit := must(sundae, "git", "commit-tree", "-m", "init", tree)
	must(sundae, "git", "update-ref", "refs/heads/main", commit)
	must(sundae, "git", "symbolic-ref", "HEAD", "refs/heads/main")
	must(forge, "git", "fetch", "-q", sundae, "main:main")
	sundaeURL, forgeURL := "file://"+sundae, "file://"+forge

	setupFetch := func(ws, fetchOrigin string) {
		t.Helper()
		for range 2 { // idempotent
			must(tmp, "sh", "-c", forgeRemotesScript, forgeRemotesName, ws, sundaeURL, forgeURL, ForgeOriginPushURL, fetchOrigin)
		}
	}
	setup := func(ws string) { t.Helper(); setupFetch(ws, "1") }
	check := func(ws string) {
		t.Helper()
		remotes := must(ws, "git", "remote", "-v")
		for _, want := range []string{
			"origin\t" + forgeURL + " (fetch)", "origin\t" + ForgeOriginPushURL + " (push)",
			"sundae\t" + sundaeURL + " (fetch)", "sundae\t" + sundaeURL + " (push)",
		} {
			if !strings.Contains(remotes, want) {
				t.Fatalf("git remote -v = %q, want %q", remotes, want)
			}
		}
		cfg := must(ws, "jj", "config", "list", "--repo")
		for _, want := range []string{`git.push = "sundae"`, `git.fetch = ["sundae", "origin"]`, `remotes.sundae.auto-track-bookmarks = "*"`} {
			if !strings.Contains(cfg, want) {
				t.Fatalf("jj repo config = %q, want %q", cfg, want)
			}
		}
		must(ws, "jj", "git", "fetch")
		must(ws, "jj", "new", "-m", "work", "main@sundae")
		must(ws, "jj", "bookmark", "create", "-r", "@", "feature")
		if out, err := run(ws, "jj", "git", "push", "--remote", "origin", "-b", "feature"); err == nil {
			t.Fatalf("push to origin succeeded: %s", out)
		}
		must(ws, "jj", "bookmark", "track", "feature", "--remote", "origin")
		if out, err := run(ws, "jj", "git", "push", "--remote", "origin", "-b", "feature"); err == nil || !strings.Contains(out, "sundae-forwards-pushes") {
			t.Fatalf("tracked push to origin: %v: %s", err, out)
		}
		must(ws, "jj", "git", "push", "-b", "feature")
		if must(sundae, "git", "rev-parse", "--verify", "refs/heads/feature") == "" {
			t.Fatal("feature not on sundae")
		}
		if _, err := run(forge, "git", "rev-parse", "--verify", "refs/heads/feature"); err == nil {
			t.Fatal("feature reached the forge directly")
		}
		must(sundae, "git", "update-ref", "-d", "refs/heads/feature")
	}

	fresh := filepath.Join(tmp, "fresh")
	cmd := ForgeCloneCommand(sundaeURL, fresh)
	must(tmp, cmd...)
	setup(fresh)
	check(fresh)

	// Without the forge token origin is left out of jj git fetch, which then
	// works with the forge unreachable; with it, origin is back.
	setupFetch(fresh, "0")
	if cfg := must(fresh, "jj", "config", "list", "--repo"); !strings.Contains(cfg, `git.fetch = ["sundae"]`) {
		t.Fatalf("jj repo config = %q", cfg)
	}
	if err := os.Rename(forge, forge+".away"); err != nil {
		t.Fatal(err)
	}
	must(fresh, "jj", "git", "fetch")
	if err := os.Rename(forge+".away", forge); err != nil {
		t.Fatal(err)
	}
	setup(fresh)
	check(fresh)

	// Cloned before: origin is Sundae.
	old := filepath.Join(tmp, "old")
	must(tmp, CloneCommand("jj", sundaeURL, old)...)
	setup(old)
	if out := must(old, "jj", "config", "get", `revset-aliases."trunk()"`); out != "main@sundae" {
		t.Fatalf("trunk() = %q", out)
	}
	check(old)

	// A git checkout gets the remotes, not the jj config.
	plain := filepath.Join(tmp, "plain")
	must(tmp, CloneCommand("git", sundaeURL, plain)...)
	setup(plain)
	if remotes := must(plain, "git", "remote", "-v"); !strings.Contains(remotes, "origin\t"+forgeURL+" (fetch)") || !strings.Contains(remotes, "sundae\t"+sundaeURL+" (push)") {
		t.Fatalf("git checkout remotes = %q", remotes)
	}
}

// OpenShell's exec has no input: Run refuses Stdin instead of dropping it.
func TestSDKRunRefusesStdin(t *testing.T) {
	c := newSDKClient("default", nil, newFakeExec(), nil, nil)
	_, err := c.Run(context.Background(), "sd", []string{"cat"}, ExecOptions{Stdin: strings.NewReader("x")})
	if !errors.Is(err, sbx.ErrStdinUnsupported) {
		t.Fatalf("err = %v", err)
	}
}
