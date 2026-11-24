package openshell

import (
	"context"
	"encoding/base64"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	"github.com/danielboothcloud/sundae/pkg/config"
)

// privateResolver resolves github/me/tool as forge-linked with private
// paths and anything else as a plain repository.
func privateResolver(id config.GitIdentity) func(string) (RepoSpec, error) {
	return func(name string) (RepoSpec, error) {
		if name == "github/me/tool" {
			return RepoSpec{Name: name, Forge: ghRemote, PrivatePaths: []string{"AGENTS.md", "_bmad"}, GitIdentity: id}, nil
		}
		return RepoSpec{Name: name}, nil
	}
}

// Start and attach restore the private files of forge-linked repositories
// that have private paths, after their remotes; others get none.
func TestPrivateRestore(t *testing.T) {
	ctx := context.Background()
	cfg := newHarnessConfig()
	withForges(t, cfg)
	h := newHarnessWith(t, cfg)
	var progress strings.Builder
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Progress: &progress, Repos: []RepoSpec{
		{Name: "github/me/tool", Forge: ghRemote, PrivatePaths: []string{"AGENTS.md", "_bmad"}},
		{Name: "gitlab/g/p", Forge: glRemote},
		{Name: "local", PrivatePaths: []string{"ignored"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	b := h.exec.boxes[s.Sandbox]
	if len(b.restores) != 1 || !slices.Equal(b.restores["/sandbox/github/me/tool"], []string{"AGENTS.md", "_bmad"}) {
		t.Fatalf("restores = %v", b.restores)
	}
	scripts := h.exec.scripts(s.Sandbox)
	if i, j := slices.Index(scripts, forgeRemotesName), slices.Index(scripts, privateRestoreName); i < 0 || j < i {
		t.Fatalf("restore before remotes: %v", scripts)
	}
	if !strings.Contains(progress.String(), "Warning: github/me/tool: AGENTS.md is tracked by the repository; not restored") {
		t.Fatalf("progress = %q", progress.String())
	}
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Name: "main", ResolveRepo: privateResolver(config.GitIdentity{})}); err != nil {
		t.Fatal(err)
	}
	if n := h.exec.count(s.Sandbox, privateRestoreName); n != 2 {
		t.Fatalf("restores = %d", n)
	}

	// No restore when the remotes could not be set up.
	h.exec.remotesExit = 3
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Name: "main", ResolveRepo: privateResolver(config.GitIdentity{})}); err != nil {
		t.Fatal(err)
	}
	if n := h.exec.count(s.Sandbox, privateRestoreName); n != 2 {
		t.Fatalf("restored without remotes: %d", n)
	}
}

// Suspend saves the private files first, with fresh credentials, as the
// session's identity or Sundae's; a failed save only warns.
func TestPrivateSaveOnSuspend(t *testing.T) {
	ctx := context.Background()
	cfg := newHarnessConfig()
	withForges(t, cfg)
	h := newHarnessWith(t, cfg)
	h.mgr.resolveRepo = privateResolver(config.GitIdentity{})
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repos: []RepoSpec{{Name: "github/me/tool", Forge: ghRemote}, {Name: "local"}}})
	if err != nil {
		t.Fatal(err)
	}
	creds := h.exec.count(s.Sandbox, gitCredentialsName)
	if ok, err := h.mgr.Suspend(ctx, alice, "main", nil); err != nil || !ok {
		t.Fatalf("suspend: %v %v", ok, err)
	}
	b := h.exec.boxes[s.Sandbox]
	if len(b.saves) != 1 || !slices.Equal(b.saves[0].args, []string{"/sandbox/github/me/tool", "AGENTS.md", "_bmad"}) || b.saves[0].identity != "Sundae <sundae@localhost>" {
		t.Fatalf("saves = %+v", b.saves)
	}
	scripts := h.exec.scripts(s.Sandbox)
	if h.exec.count(s.Sandbox, gitCredentialsName) != creds+1 || scripts[len(scripts)-1] != privateSaveName || scripts[len(scripts)-2] != gitCredentialsName {
		t.Fatalf("credentials not renewed before the save: %v", scripts)
	}
	// The renewed credentials still hold the session's forge token.
	if !strings.Contains(b.files[forgeEnvFile], "GH_TOKEN") {
		t.Fatalf("forge env = %q", b.files[forgeEnvFile])
	}
	assertForgeTokensConfined(t, h.exec, func(r execCall, k string) bool {
		return r.script() == gitCredentialsName && (k == "SUNDAE_GITCONFIG" || k == "SUNDAE_FORGE_ENV")
	})
	// Nor is the Sundae token in the private scripts: they use the credential file.
	token := h.tokens.newest(TokenName(s.Sandbox))
	for _, r := range h.exec.runs {
		if r.script() != privateSaveName && r.script() != privateRestoreName {
			continue
		}
		for _, v := range append(slices.Clone(r.command), slices.Collect(maps.Values(r.opts.Env))...) {
			if decoded, _ := base64.StdEncoding.DecodeString(v); token == "" || strings.Contains(v, token) || strings.Contains(string(decoded), token) {
				t.Fatalf("Sundae token in %s", r.script())
			}
		}
	}

	// The session's identity, and a failed save that only warns.
	h.mgr.resolveRepo = privateResolver(config.GitIdentity{Name: "Jane", Email: "jane@example.com"})
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Name: "main"}); err != nil {
		t.Fatal(err)
	}
	h.exec.saveExit = 1
	var progress strings.Builder
	if ok, err := h.mgr.Suspend(ctx, alice, "main", &progress); err != nil || !ok {
		t.Fatalf("suspend: %v %v", ok, err)
	}
	if !strings.Contains(progress.String(), "Warning: could not save the private files; the session's storage keeps them: github/me/tool (AGENTS.md, _bmad)") {
		t.Fatalf("progress = %q", progress.String())
	}
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Name: "main"}); err != nil {
		t.Fatal(err)
	}
	h.exec.saveExit = 0
	saves, err := h.mgr.SavePrivate(ctx, alice, "main", nil)
	if err != nil || len(saves) != 1 || saves[0].Result != PrivateSaved || saves[0].Commit != "0123456789abcdef" {
		t.Fatalf("SavePrivate = %+v, %v", saves, err)
	}
	if got := b.saves[len(b.saves)-1].identity; got != "Jane <jane@example.com>" {
		t.Fatalf("identity = %q", got)
	}
}

// A failed save refuses the delete unless DiscardPrivate.
func TestPrivateSaveOnDelete(t *testing.T) {
	ctx := context.Background()
	cfg := newHarnessConfig()
	withForges(t, cfg)
	h := newHarnessWith(t, cfg)
	h.mgr.resolveRepo = privateResolver(config.GitIdentity{})
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repos: []RepoSpec{{Name: "github/me/tool", Forge: ghRemote}}})
	if err != nil {
		t.Fatal(err)
	}
	h.exec.saveExit = 1
	if existed, err := h.mgr.Delete(ctx, alice, "main", DeleteOptions{}); existed || !errors.Is(err, ErrPrivateNotSaved) {
		t.Fatalf("delete = %v, %v", existed, err)
	}
	if _, err := h.fc.Sandboxes().Get(ctx, "default", s.Sandbox); err != nil {
		t.Fatalf("sandbox gone after a refused delete: %v", err)
	}
	var progress strings.Builder
	if existed, err := h.mgr.Delete(ctx, alice, "main", DeleteOptions{DiscardPrivate: true, Progress: &progress}); !existed || err != nil {
		t.Fatalf("discarding delete = %v, %v", existed, err)
	}
	if !strings.Contains(progress.String(), "loses its unsaved private files") {
		t.Fatalf("progress = %q", progress.String())
	}

	// A suspended session cannot save, so its delete is refused too.
	s, err = h.mgr.Start(ctx, StartOptions{User: alice, Repos: []RepoSpec{{Name: "github/me/tool", Forge: ghRemote}}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := h.mgr.Suspend(ctx, alice, "main", nil); err != nil || !ok {
		t.Fatalf("suspend: %v %v", ok, err)
	}
	saves := len(h.exec.boxes[s.Sandbox].saves)
	if existed, err := h.mgr.Delete(ctx, alice, "main", DeleteOptions{}); existed || !errors.Is(err, ErrPrivateNotSaved) ||
		!strings.Contains(err.Error(), `resume it and run "session save-private main"`) {
		t.Fatalf("delete of a suspended session = %v, %v", existed, err)
	}
	if _, err := h.fc.Sandboxes().Get(ctx, "default", s.Sandbox); err != nil {
		t.Fatalf("sandbox gone after a refused delete: %v", err)
	}
	progress.Reset()
	if existed, err := h.mgr.Delete(ctx, alice, "main", DeleteOptions{DiscardPrivate: true, Progress: &progress}); !existed || err != nil {
		t.Fatalf("discarding delete = %v, %v", existed, err)
	}
	if len(h.exec.boxes[s.Sandbox].saves) != saves || !strings.Contains(progress.String(), "not saved before") {
		t.Fatalf("progress = %q", progress.String())
	}
}

// An idle suspend runs the Monitor's save first, as the session's owner.
func TestMonitorSavesPrivateBeforeAutoSuspend(t *testing.T) {
	h := newMonitorHarness(t, nil)
	var (
		mu    sync.Mutex
		saved []string
	)
	h.mon.savePrivate = func(_ context.Context, c Client, userID int64, sandbox string) {
		mu.Lock()
		defer mu.Unlock()
		sb, _ := c.GetSandbox(context.Background(), sandbox)
		saved = append(saved, sandbox+" "+string(sb.Phase)+" "+strings.Repeat("u", int(userID)))
	}
	sb := h.mc.addSession(alice, "infra", "main", types.SandboxReady)
	h.mc.set(sb, "o1", osewaSessionInfo{ID: "o1", Status: st("idle", "", "")})
	h.poll(t)
	h.clk.advance(30 * time.Minute)
	h.poll(t)
	if h.mc.suspendCount(sb) != 1 || !slices.Equal(saved, []string{sb + " Ready " + strings.Repeat("u", int(alice.id))}) {
		t.Fatalf("suspends = %d, saved = %v", h.mc.suspendCount(sb), saved)
	}
}

// privateGit runs the restore and save scripts for real, with git and jj.
type privateGit struct {
	t    *testing.T
	tmp  string
	env  []string
	repo string // the Sundae repository's URL
}

func newPrivateGit(t *testing.T) *privateGit {
	t.Helper()
	for _, bin := range []string{"jj", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip("no " + bin)
		}
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(tmp, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	g := &privateGit{t: t, tmp: tmp, env: []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "PATH=" + os.Getenv("PATH"), "TMPDIR=" + tmp,
		"GIT_CONFIG_NOSYSTEM=1", "JJ_USER=t", "JJ_EMAIL=t@example.com",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	}}
	return g
}

// repos creates the Sundae and forge repositories, with main tracking
// main.go and notes, and returns their paths and the forge's URL.
func (g *privateGit) repos() (sundae, forge, forgeURL string) {
	g.t.Helper()
	sundae, forge = filepath.Join(g.tmp, "sundae.git"), filepath.Join(g.tmp, "forge.git")
	g.must(g.tmp, "git", "init", "-q", "--bare", sundae)
	g.must(g.tmp, "git", "init", "-q", "--bare", forge)
	seed := filepath.Join(g.tmp, "seed")
	g.must(g.tmp, "git", "init", "-q", seed)
	g.write(filepath.Join(seed, "main.go"), "package main\n")
	g.write(filepath.Join(seed, "notes"), "tracked upstream\n")
	g.must(seed, "git", "add", "-A")
	commit := g.must(seed, "git", "commit-tree", "-m", "init", g.must(seed, "git", "write-tree"))
	for _, bare := range []string{sundae, forge} {
		g.must(seed, "git", "push", "-q", bare, commit+":refs/heads/main")
		g.must(bare, "git", "symbolic-ref", "HEAD", "refs/heads/main")
	}
	g.repo = "file://" + sundae
	return sundae, forge, "file://" + forge
}

func (g *privateGit) run(dir string, env []string, args ...string) (string, error) {
	g.t.Helper()
	cmd := exec.CommandContext(g.t.Context(), args[0], args[1:]...)
	cmd.Dir, cmd.Env = dir, append(slices.Clone(g.env), env...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (g *privateGit) must(dir string, args ...string) string {
	g.t.Helper()
	out, err := g.run(dir, nil, args...)
	if err != nil {
		g.t.Fatalf("%q: %v: %s", args, err, out)
	}
	return out
}

// clone makes a session checkout of the Sundae repository, as Start does.
func (g *privateGit) clone(name, forgeURL string) string {
	g.t.Helper()
	ws := filepath.Join(g.tmp, name)
	g.must(g.tmp, ForgeCloneCommand(g.repo, ws)...)
	g.must(g.tmp, "sh", "-c", forgeRemotesScript, forgeRemotesName, ws, g.repo, forgeURL, ForgeOriginPushURL)
	return ws
}

func (g *privateGit) restore(ws string, paths ...string) string {
	g.t.Helper()
	return g.must(g.tmp, append([]string{"sh", "-c", privateRestoreScript, privateRestoreName, ws}, paths...)...)
}

func (g *privateGit) save(ws string, paths ...string) string {
	g.t.Helper()
	env := []string{
		"SUNDAE_GIT_USER_NAME=" + base64.StdEncoding.EncodeToString([]byte("Jane Doe")),
		"SUNDAE_GIT_USER_EMAIL=" + base64.StdEncoding.EncodeToString([]byte("jane@example.com")),
	}
	out, err := g.run(g.tmp, env, append([]string{"sh", "-c", privateSaveScript, privateSaveName, ws}, paths...)...)
	if err != nil {
		g.t.Fatalf("save %s: %v: %s", ws, err, out)
	}
	return out
}

func (g *privateGit) write(path, content string) {
	g.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec
		g.t.Fatal(err)
	}
}

func (g *privateGit) read(path string) string {
	g.t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// The save in one session and the restore in a fresh one, with real git
// and jj: the files stay out of jj, branches and the forge; local edits
// win over the restore; tracked paths are skipped; the later save of a file
// wins.
func TestPrivateScripts(t *testing.T) {
	g := newPrivateGit(t)
	sundae, forge, forgeURL := g.repos()
	paths := []string{"AGENTS.md", "_bmad", "notes", "we[ir]d*"}

	// Session A: nothing saved yet.
	a := g.clone("a", forgeURL)
	if out := g.restore(a, paths...); !strings.HasSuffix(out, "none") || !strings.Contains(out, "warning: notes is tracked by the repository; not restored") {
		t.Fatalf("first restore = %q", out)
	}
	if out := g.save(a, paths...); !strings.HasSuffix(out, "empty") {
		t.Fatalf("save of nothing = %q", out)
	}
	g.write(filepath.Join(a, "AGENTS.md"), "agent rules\n")
	g.write(filepath.Join(a, "_bmad", "plan.md"), "plan\n")
	g.write(filepath.Join(a, "we[ir]d*"), "odd name\n")
	g.write(filepath.Join(a, "notes"), "local notes\n")
	out := g.save(a, paths...)
	if !strings.Contains(out, "warning: notes is tracked by the repository; not saved") || !strings.Contains(out, "\nsaved ") {
		t.Fatalf("save = %q", out)
	}
	first := strings.TrimPrefix(out[strings.LastIndex(out, "\n")+1:], "saved ")
	if got := g.must(sundae, "git", "rev-parse", PrivateRef); got != first {
		t.Fatalf("sundae ref = %s, want %s", got, first)
	}
	if got := g.must(sundae, "git", "ls-tree", "-r", "--name-only", PrivateRef); got != "AGENTS.md\n_bmad/plan.md\nwe[ir]d*" {
		t.Fatalf("saved tree = %q", got)
	}
	if got := g.must(sundae, "git", "log", "-1", "--format=%an <%ae>|%cn <%ce>|%s", PrivateRef); got != "Jane Doe <jane@example.com>|Jane Doe <jane@example.com>|sundae: save private paths" {
		t.Fatalf("commit = %q", got)
	}
	if out := g.save(a, paths...); !strings.HasSuffix(out, "unchanged "+first) {
		t.Fatalf("second save = %q", out)
	}
	// jj sees only the tracked edit; branches and the forge never get the files.
	if st := g.must(a, "jj", "status"); strings.Contains(st, "AGENTS.md") || strings.Contains(st, "_bmad") || strings.Contains(st, "weird") || !strings.Contains(st, "notes") {
		t.Fatalf("jj status = %q", st)
	}
	g.must(a, "jj", "restore", "notes")
	g.must(a, "jj", "new", "-m", "work", "main@sundae")
	g.write(filepath.Join(a, "x.go"), "package main\n")
	g.must(a, "jj", "bookmark", "create", "-r", "@", "feature")
	g.must(a, "jj", "git", "push", "-b", "feature")
	if files := g.must(sundae, "git", "ls-tree", "-r", "--name-only", "feature"); files != "main.go\nnotes\nx.go" {
		t.Fatalf("feature = %q", files)
	}
	if refs := g.must(forge, "git", "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
		t.Fatalf("forge refs = %q", refs)
	}

	// Session B: a fresh clone restores them, out of jj's sight.
	b := g.clone("b", forgeURL)
	if out := g.restore(b, paths...); !strings.HasSuffix(out, "restored") {
		t.Fatalf("restore = %q", out)
	}
	for f, want := range map[string]string{"AGENTS.md": "agent rules\n", "_bmad/plan.md": "plan\n", "we[ir]d*": "odd name\n", "notes": "tracked upstream\n"} {
		if got := g.read(filepath.Join(b, f)); got != want {
			t.Fatalf("B %s = %q, want %q", f, got, want)
		}
	}
	if st := g.must(b, "jj", "status"); !strings.Contains(st, "The working copy has no changes") {
		t.Fatalf("jj status in B = %q", st)
	}
	if st := g.must(b, "git", "status", "--porcelain"); st != "" {
		t.Fatalf("git status in B = %q", st)
	}
	g.restore(b, paths...)
	if ex := g.read(filepath.Join(b, ".git", "info", "exclude")); strings.Count(ex, "/AGENTS.md\n") != 1 || !strings.Contains(ex, "/we\\[ir\\]d\\*\n") || strings.Contains(ex, "/notes") {
		t.Fatalf("exclude = %q", ex)
	}

	// A local edit survives a restore; a file missing locally comes back.
	g.write(filepath.Join(a, "_bmad", "new.md"), "from A\n")
	g.write(filepath.Join(a, "AGENTS.md"), "A v2\n")
	g.save(a, paths...)
	g.write(filepath.Join(b, "AGENTS.md"), "B local\n")
	g.restore(b, paths...)
	if got := g.read(filepath.Join(b, "AGENTS.md")); got != "B local\n" {
		t.Fatalf("local edit overwritten: %q", got)
	}
	if got := g.read(filepath.Join(b, "_bmad", "new.md")); got != "from A\n" {
		t.Fatalf("missing file not restored: %q", got)
	}

	// Both edit AGENTS.md: the later save wins it, on top of A's tip.
	g.write(filepath.Join(a, "AGENTS.md"), "A v3\n")
	out = g.save(a, paths...)
	aTip := strings.TrimPrefix(out[strings.LastIndex(out, "\n")+1:], "saved ")
	g.write(filepath.Join(b, "AGENTS.md"), "B wins\n")
	out = g.save(b, paths...)
	if !strings.Contains(out, "saved ") {
		t.Fatalf("B save = %q", out)
	}
	if got := g.must(sundae, "git", "show", PrivateRef+":AGENTS.md"); got != "B wins" {
		t.Fatalf("AGENTS.md = %q", got)
	}
	if got := g.must(sundae, "git", "rev-parse", PrivateRef+"^"); got != aTip {
		t.Fatalf("B's save parent = %s, want A's %s", got, aTip)
	}
	if got := g.must(b, "git", "rev-parse", privateRemoteRef); got != g.must(sundae, "git", "rev-parse", PrivateRef) {
		t.Fatal("B's remote ref not updated")
	}
	// No temporary index is left behind.
	if left, _ := filepath.Glob(filepath.Join(g.tmp, "sundae-private.*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

// show returns path in the Sundae repository's private ref, or "<none>".
func (g *privateGit) show(sundae, path string) string {
	g.t.Helper()
	out, err := g.run(sundae, nil, "git", "show", PrivateRef+":"+path)
	if err != nil {
		return "<none>"
	}
	return out
}

// result returns the last line of a script's output.
func result(out string) string {
	return out[strings.LastIndex(out, "\n")+1:]
}

// A save sends only what its session changed since it last synced, on top
// of the current tip: a stale unedited file never reverts a newer save.
func TestPrivateScriptsConcurrent(t *testing.T) {
	g := newPrivateGit(t)
	sundae, _, forgeURL := g.repos()
	paths := []string{"AGENTS.md", "_bmad", "notes"}
	a, b := g.clone("a", forgeURL), g.clone("b", forgeURL)
	g.restore(a, paths...)
	g.write(filepath.Join(a, "AGENTS.md"), "v1\n")
	g.write(filepath.Join(a, "_bmad", "plan.md"), "plan\n")
	g.save(a, paths...)
	if out := g.restore(b, paths...); result(out) != "restored" || g.read(filepath.Join(b, "AGENTS.md")) != "v1\n" {
		t.Fatalf("B restore = %q", out)
	}
	g.write(filepath.Join(a, "AGENTS.md"), "v2\n")
	g.save(a, paths...)

	// B suspends without an attach in between: nothing to send.
	if out := g.save(b, paths...); !strings.HasPrefix(result(out), "unchanged ") {
		t.Fatalf("B save = %q", out)
	}
	// B attaches (its AGENTS.md exists, so it stays v1) and suspends.
	g.restore(b, paths...)
	if out := g.save(b, paths...); !strings.HasPrefix(result(out), "unchanged ") || g.show(sundae, "AGENTS.md") != "v2" {
		t.Fatalf("B save after attach = %q, AGENTS.md = %q", out, g.show(sundae, "AGENTS.md"))
	}
	// B edits another path: only that one is sent, twice (the base keeps the stale file).
	for _, v := range []string{"B plan\n", "B plan 2\n"} {
		g.write(filepath.Join(b, "_bmad", "plan.md"), v)
		if out := g.save(b, paths...); !strings.HasPrefix(result(out), "saved ") {
			t.Fatalf("B save = %q", out)
		}
		if got, plan := g.show(sundae, "AGENTS.md"), g.show(sundae, "_bmad/plan.md"); got != "v2" || plan+"\n" != v {
			t.Fatalf("AGENTS.md = %q, plan = %q", got, plan)
		}
	}

	// B's push is rejected (A saves v3 meanwhile): B redoes it on A's tip.
	saveScript := filepath.Join(g.tmp, "save.sh")
	g.write(saveScript, privateSaveScript)
	g.write(filepath.Join(a, "AGENTS.md"), "v3\n")
	marker := filepath.Join(g.tmp, "raced")
	hook := filepath.Join(b, ".git", "hooks", "pre-push")
	g.write(hook, "#!/bin/sh\n[ -e "+marker+" ] && exit 0\ntouch "+marker+
		"\nunset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE\nsh "+saveScript+" "+a+" AGENTS.md _bmad notes >/dev/null\n")
	if err := os.Chmod(hook, 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	g.write(filepath.Join(b, "_bmad", "new.md"), "B new\n")
	out := g.save(b, paths...)
	if _, err := os.Stat(marker); err != nil || !strings.HasPrefix(result(out), "saved ") {
		t.Fatalf("B save = %q (raced: %v)", out, err)
	}
	if got, nw := g.show(sundae, "AGENTS.md"), g.show(sundae, "_bmad/new.md"); got != "v3" || nw != "B new" {
		t.Fatalf("after the retry AGENTS.md = %q, new.md = %q", got, nw)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}

	// Edits to different paths both stay; the same path goes to the later save.
	g.restore(a, paths...)
	g.restore(b, paths...)
	g.write(filepath.Join(a, "_bmad", "a.md"), "from A\n")
	g.write(filepath.Join(a, "AGENTS.md"), "A v4\n")
	g.write(filepath.Join(b, "_bmad", "b.md"), "from B\n")
	g.write(filepath.Join(b, "AGENTS.md"), "B v4\n")
	g.save(a, paths...)
	g.save(b, paths...)
	for f, want := range map[string]string{"_bmad/a.md": "from A", "_bmad/b.md": "from B", "AGENTS.md": "B v4", "_bmad/new.md": "B new"} {
		if got := g.show(sundae, f); got != want {
			t.Fatalf("%s = %q, want %q", f, got, want)
		}
	}
}

// A deleted private file leaves the ref, and a session holding an unedited
// copy does not bring it back; nor does one that restored it before.
func TestPrivateScriptsDelete(t *testing.T) {
	g := newPrivateGit(t)
	sundae, _, forgeURL := g.repos()
	paths := []string{"AGENTS.md", "_bmad"}
	a, b := g.clone("a", forgeURL), g.clone("b", forgeURL)
	g.restore(a, paths...)
	g.write(filepath.Join(a, "AGENTS.md"), "rules\n")
	g.write(filepath.Join(a, "_bmad", "plan.md"), "plan\n")
	g.save(a, paths...)
	g.restore(b, paths...)
	g.write(filepath.Join(a, "_bmad", "extra.md"), "extra\n")
	g.save(a, paths...)
	g.restore(b, paths...) // extracts extra.md

	if err := os.Remove(filepath.Join(a, "_bmad", "plan.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a, "_bmad", "extra.md")); err != nil {
		t.Fatal(err)
	}
	if out := g.save(a, paths...); !strings.HasPrefix(result(out), "saved ") {
		t.Fatalf("A save = %q", out)
	}
	if g.show(sundae, "_bmad/plan.md") != "<none>" || g.show(sundae, "_bmad/extra.md") != "<none>" || g.show(sundae, "AGENTS.md") != "rules" {
		t.Fatalf("ref = %q", g.must(sundae, "git", "ls-tree", "-r", "--name-only", PrivateRef))
	}
	g.restore(b, paths...)
	g.write(filepath.Join(b, "AGENTS.md"), "B rules\n")
	g.save(b, paths...)
	if files := g.must(sundae, "git", "ls-tree", "-r", "--name-only", PrivateRef); files != "AGENTS.md" || g.show(sundae, "AGENTS.md") != "B rules" {
		t.Fatalf("deleted files came back: %q", files)
	}
	c := g.clone("c", forgeURL)
	g.restore(c, paths...)
	if _, err := os.Stat(filepath.Join(c, "_bmad", "plan.md")); !os.IsNotExist(err) {
		t.Fatalf("plan.md restored: %v", err)
	}
	if st := g.must(c, "jj", "status"); !strings.Contains(st, "The working copy has no changes") {
		t.Fatalf("jj status = %q", st)
	}
}

// A checkout restored before the base ref existed syncs from its local
// refs/sundae/private: its stale unedited copy is not saved.
func TestPrivateScriptsLegacyCheckout(t *testing.T) {
	g := newPrivateGit(t)
	sundae, _, forgeURL := g.repos()
	a, b := g.clone("a", forgeURL), g.clone("b", forgeURL)
	g.restore(a, "AGENTS.md")
	g.write(filepath.Join(a, "AGENTS.md"), "v1\n")
	v1 := strings.TrimPrefix(result(g.save(a, "AGENTS.md")), "saved ")
	g.write(filepath.Join(a, "AGENTS.md"), "v2\n")
	g.save(a, "AGENTS.md")

	// What the old restore left: the file and the local ref, no base.
	g.must(b, "git", "fetch", "-q", "sundae", v1)
	g.must(b, "git", "update-ref", PrivateRef, v1)
	g.write(filepath.Join(b, "AGENTS.md"), "v1\n")
	if out := g.save(b, "AGENTS.md"); !strings.HasPrefix(result(out), "unchanged") || g.show(sundae, "AGENTS.md") != "v2" {
		t.Fatalf("legacy save = %q, AGENTS.md = %q", out, g.show(sundae, "AGENTS.md"))
	}
	g.restore(b, "AGENTS.md")
	if out := g.save(b, "AGENTS.md"); !strings.HasPrefix(result(out), "unchanged") || g.show(sundae, "AGENTS.md") != "v2" {
		t.Fatalf("legacy save after restore = %q, AGENTS.md = %q", out, g.show(sundae, "AGENTS.md"))
	}
}
