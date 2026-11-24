package forge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/hooks"
	"github.com/danielboothcloud/sundae/pkg/proto"
)

const zero = "0000000000000000000000000000000000000000"

// forwardFixture is an imported repository ("gh/me/proj", default branch
// main, also develop) and an agent's work tree cloned from it. The forge
// is a local bare repository.
type forwardFixture struct {
	*serviceFixture
	up   *upstream
	repo string
	// dir is the Sundae bare repository, work the agent's clone of it.
	dir  string
	work string
}

func newForwardFixture(t *testing.T, mutate func(*config.ForgeConfig)) *forwardFixture {
	t.Helper()
	fx := newServiceFixtureWith(t, t.TempDir(), func(cfg *config.Config, _ *fakeForge) {
		if mutate != nil {
			mutate(&cfg.Forges[0])
		}
	})
	return fx.importProj(t, nil)
}

// importProj imports me/proj; seed, if not nil, commits on the forge first.
func (fx *serviceFixture) importProj(t *testing.T, seed func(*upstream)) *forwardFixture {
	t.Helper()
	up := fx.upstream(t, "me/proj", false)
	up.commit(t, "develop", "develop")
	if seed != nil {
		seed(up)
	}
	results, err := fx.svc.Import(fx.ctx, fx.user, "gh", ImportOptions{Paths: []string{"me/proj"}})
	if err != nil || resultFor(t, results, "me/proj").Status != ImportImported {
		t.Fatalf("import: %+v, %v", results, err)
	}
	r, err := fx.be.Repository(fx.ctx, "gh/me/proj")
	if err != nil {
		t.Fatal(err)
	}
	gr, err := r.Open()
	if err != nil {
		t.Fatal(err)
	}
	return &forwardFixture{serviceFixture: fx, up: up, repo: "gh/me/proj", dir: gr.Path, work: localClone(t, gr.Path)}
}

// commit commits files on branch in the agent's work tree, starting the
// branch at base when base is not "", and returns the commit.
func (fx *forwardFixture) commit(t *testing.T, base, branch string, files map[string]string, msg string) string {
	t.Helper()
	if base != "" {
		git(t, fx.work, "checkout", "--quiet", "-B", branch, base)
	} else {
		git(t, fx.work, "checkout", "--quiet", branch)
	}
	for name, body := range files {
		p := filepath.Join(fx.work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, fx.work, "add", "-A")
	git(t, fx.work, "commit", "--quiet", "--allow-empty", "-m", msg)
	return git(t, fx.work, "rev-parse", "HEAD")
}

// forgeCommit is another writer committing on the forge's branch.
func (fx *forwardFixture) forgeCommit(t *testing.T, branch, msg string) string {
	t.Helper()
	git(t, fx.up.work, "fetch", "--quiet", "origin")
	git(t, fx.up.work, "checkout", "--quiet", "-B", branch, "origin/"+branch)
	return commitAndPush(t, fx.up.work, branch, msg)
}

// rejectOnForge makes the forge refuse pushes of branch.
func (fx *forwardFixture) rejectOnForge(t *testing.T, branch string) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\nwhile read o n r; do [ \"$r\" = refs/heads/%s ] && { echo \"forge: rejected $r\"; exit 1; }; done; exit 0\n", branch)
	if err := os.WriteFile(filepath.Join(fx.up.bare, "hooks", "pre-receive"), []byte(script), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
}

type refUpdate struct {
	ref string
	// sha is the new value; "" deletes.
	sha string
}

func branch(name, sha string) refUpdate { return refUpdate{ref: "refs/heads/" + name, sha: sha} }

// push does what receive-pack does around the hooks: the objects arrive
// without moving any ref, pre-receive (Forward) runs, and only when it
// accepts are the refs updated and post-receive (RecordForward) run.
func (fx *forwardFixture) push(t *testing.T, updates ...refUpdate) (string, error) {
	t.Helper()
	var args []hooks.HookArg
	for i, u := range updates {
		old := rev(t, fx.dir, u.ref)
		if old == "" {
			old = zero
		}
		newSha := zero
		if u.sha != "" {
			newSha = u.sha
			tmp := fmt.Sprintf("refs/tmp/%d", i)
			git(t, fx.work, "push", "--quiet", fx.dir, u.sha+":"+tmp)
			git(t, fx.dir, "update-ref", "-d", tmp)
		}
		args = append(args, hooks.HookArg{OldSha: old, NewSha: newSha, RefName: u.ref})
	}
	var out bytes.Buffer
	err := fx.svc.Forward(fx.ctx, &out, fx.repo, args)
	if err != nil {
		return out.String(), err
	}
	for _, a := range args {
		if a.NewSha == zero {
			git(t, fx.dir, "update-ref", "-d", a.RefName, a.OldSha)
		} else {
			git(t, fx.dir, "update-ref", a.RefName, a.NewSha)
		}
	}
	fx.svc.RecordForward(fx.ctx, &out, fx.repo, args)
	return out.String(), nil
}

func mustPush(t *testing.T, fx *forwardFixture, updates ...refUpdate) string {
	t.Helper()
	out, err := fx.push(t, updates...)
	if err != nil {
		t.Fatalf("push refused: %v\n%s", err, out)
	}
	return out
}

func mustRefuse(t *testing.T, fx *forwardFixture, want string, updates ...refUpdate) {
	t.Helper()
	out, err := fx.push(t, updates...)
	if err == nil || !errors.Is(err, ErrPushRefused) || !strings.Contains(err.Error(), want) {
		t.Fatalf("push: err = %v, want a refusal containing %q\n%s", err, want, out)
	}
}

// wantRefs checks branch on the forge, in Sundae and in the namespace.
func (fx *forwardFixture) wantRefs(t *testing.T, name, forge, sundae, ns string) {
	t.Helper()
	got := [3]string{
		rev(t, fx.up.bare, "refs/heads/"+name),
		rev(t, fx.dir, "refs/heads/"+name),
		rev(t, fx.dir, "refs/remotes/gh/heads/"+name),
	}
	if want := [3]string{forge, sundae, ns}; got != want {
		t.Errorf("%s: forge/sundae/namespace = %.8v, want %.8v", name, got, want)
	}
}

// TestForwardMain ports t5-cases.sh CASE=main and t5-c4.sh.
func TestForwardMain(t *testing.T) {
	fx := newForwardFixture(t, func(fc *config.ForgeConfig) {
		fc.Keep = []string{"push-*"}
	})

	// 1: develop is forwarded.
	c1 := fx.commit(t, "origin/develop", "develop", map[string]string{"c1": "1"}, "c1")
	if out := mustPush(t, fx, branch("develop", c1)); !strings.Contains(out, "forwarded develop to gh") {
		t.Errorf("output = %q", out)
	}
	fx.wantRefs(t, "develop", c1, c1, c1)

	// 2: push-* stays in Sundae.
	wip := fx.commit(t, "develop", "push-abc", map[string]string{"w": "w"}, "wip")
	if out := mustPush(t, fx, branch("push-abc", wip)); !strings.Contains(out, "push-abc kept in sundae only") {
		t.Errorf("output = %q", out)
	}
	fx.wantRefs(t, "push-abc", "", wip, "")

	// 3: a rewrite is forwarded with the lease.
	git(t, fx.work, "checkout", "--quiet", "develop")
	git(t, fx.work, "commit", "--quiet", "--amend", "--allow-empty", "-m", "c1 reworded")
	reworded := git(t, fx.work, "rev-parse", "HEAD")
	mustPush(t, fx, branch("develop", reworded))
	fx.wantRefs(t, "develop", reworded, reworded, reworded)

	// 4 (t5-c4): another writer moved the forge; the stale push is refused
	// with the fetch/rebase hint and nothing moves.
	other := fx.forgeCommit(t, "develop", "other writer")
	c2 := fx.commit(t, "", "develop", map[string]string{"c2": "2"}, "c2")
	mustRefuse(t, fx, "forge develop has commits you lack; run jj git fetch, rebase onto develop@origin, and push again", branch("develop", c2))
	fx.wantRefs(t, "develop", other, reworded, reworded)

	// 5 (t5-c4): fetch, rebase onto the forge branch, push again.
	git(t, fx.work, "fetch", "--quiet", fx.up.bare, "develop")
	git(t, fx.work, "rebase", "--quiet", "FETCH_HEAD")
	rebased := git(t, fx.work, "rev-parse", "HEAD")
	mustPush(t, fx, branch("develop", rebased))
	fx.wantRefs(t, "develop", rebased, rebased, rebased)

	// 6: the forge refuses one of two refs: nothing moves on either side.
	fx.rejectOnForge(t, "develop")
	c3 := fx.commit(t, "", "develop", map[string]string{"c3": "3"}, "c3")
	side := fx.commit(t, "develop", "feature/side", map[string]string{"s": "s"}, "side")
	mustRefuse(t, fx, "forge: rejected refs/heads/develop", branch("develop", c3), branch("feature/side", side))
	fx.wantRefs(t, "develop", rebased, rebased, rebased)
	fx.wantRefs(t, "feature/side", "", "", "")
	if err := os.Remove(filepath.Join(fx.up.bare, "hooks", "pre-receive")); err != nil {
		t.Fatal(err)
	}

	// Without the forge's objection both go, in one push.
	if out := mustPush(t, fx, branch("develop", c3), branch("feature/side", side)); !strings.Contains(out, "forwarded develop, feature/side to gh") {
		t.Errorf("output = %q", out)
	}
	fx.wantRefs(t, "develop", c3, c3, c3)
	fx.wantRefs(t, "feature/side", side, side, side)

	// A forwarded delete removes the forge branch and the namespace ref.
	mustPush(t, fx, branch("feature/side", ""))
	fx.wantRefs(t, "feature/side", "", "", "")
}

func TestForwardPrivatePaths(t *testing.T) {
	fx := newServiceFixtureWith(t, t.TempDir(), func(cfg *config.Config, _ *fakeForge) {
		cfg.Forges[0].Keep = []string{"push-*"}
		cfg.Forges[0].PrivatePaths = []string{"AGENTS.md", "_bmad/", "notes"}
	}).importProj(t, func(up *upstream) {
		// The forge already tracks notes/.
		git(t, up.work, "checkout", "--quiet", "main")
		if err := os.MkdirAll(filepath.Join(up.work, "notes"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(up.work, "notes", "a.md"), []byte("a"), 0o600); err != nil {
			t.Fatal(err)
		}
		git(t, up.work, "add", "-A")
		up.commit(t, "main", "notes")
	})
	start := rev(t, fx.dir, "refs/heads/develop")

	// 7: a commit adding a private file or a file under a private directory
	// is refused, even below a clean commit.
	agents := fx.commit(t, "origin/develop", "develop", map[string]string{"AGENTS.md": "secret"}, "agents")
	mustRefuse(t, fx, "adds private path AGENTS.md", branch("develop", agents))
	fx.commit(t, "origin/develop", "develop", map[string]string{"_bmad/x/y.md": "y"}, "bmad")
	clean := fx.commit(t, "", "develop", map[string]string{"ok": "ok"}, "clean")
	mustRefuse(t, fx, "adds private path _bmad/x/y.md", branch("develop", clean))
	fx.wantRefs(t, "develop", start, start, start)

	// Kept branches may carry private files, but forwarding the same
	// commits later is refused.
	mustPush(t, fx, branch("push-agents", agents))
	mustRefuse(t, fx, "adds private path AGENTS.md", branch("develop", agents))

	// A private path the forge already tracks is not guarded.
	notes := fx.commit(t, "origin/develop", "develop", map[string]string{"notes/b.md": "b", "_bmadx": "not private"}, "notes")
	if out := mustPush(t, fx, branch("develop", notes)); !strings.Contains(out, "private path notes is tracked by the forge") {
		t.Errorf("output = %q", out)
	}
	fx.wantRefs(t, "develop", notes, notes, notes)

	// The repository's sundae.privatepath replaces the forge's list.
	git(t, fx.dir, "config", "--file", "config", "--add", "sundae.privatepath", "secret/")
	ok := fx.commit(t, "", "develop", map[string]string{"AGENTS.md": "fine here"}, "agents allowed")
	mustPush(t, fx, branch("develop", ok))
	secret := fx.commit(t, "", "develop", map[string]string{"secret/k": "k"}, "secret")
	mustRefuse(t, fx, "adds private path secret/k", branch("develop", secret))
	fx.wantRefs(t, "develop", ok, ok, ok)

	// Sessions read the same list the guard does, and SetPrivatePaths changes both.
	r, err := fx.be.Repository(fx.ctx, fx.repo)
	if err != nil {
		t.Fatal(err)
	}
	if pp, err := PrivatePathsOf(fx.ctx, fx.cfg, r); err != nil || pp.Source != PrivatePathsFromRepo || !slices.Equal(pp.Paths, []string{"secret"}) {
		t.Fatalf("PrivatePathsOf = %+v, %v", pp, err)
	}
	if err := SetPrivatePaths(fx.ctx, fx.cfg, r, []string{"keys/", "../x"}); err == nil {
		t.Fatal("invalid path accepted")
	}
	if err := SetPrivatePaths(fx.ctx, fx.cfg, r, []string{"keys/", "AGENTS.md", "keys"}); err != nil {
		t.Fatal(err)
	}
	if pp, _ := PrivatePathsOf(fx.ctx, fx.cfg, r); !slices.Equal(pp.Paths, []string{"keys", "AGENTS.md"}) {
		t.Fatalf("after set = %+v", pp)
	}
	keys := fx.commit(t, "", "develop", map[string]string{"keys/id": "k"}, "keys")
	mustRefuse(t, fx, "adds private path keys/id", branch("develop", keys))
	if err := SetPrivatePaths(fx.ctx, fx.cfg, r, nil); err != nil {
		t.Fatal(err)
	}
	if pp, _ := PrivatePathsOf(fx.ctx, fx.cfg, r); pp.Source != PrivatePathsFromForge || !slices.Equal(pp.Paths, []string{"AGENTS.md", "_bmad", "notes"}) {
		t.Fatalf("after clear = %+v", pp)
	}
	if err := SetPrivatePaths(fx.ctx, fx.cfg, r, nil); err != nil {
		t.Fatalf("clearing twice: %v", err)
	}
}

// TestForwardPrivatePathInMerge checks a merge commit that adds a private
// path itself, in neither parent, is refused.
func TestForwardPrivatePathInMerge(t *testing.T) {
	fx := newForwardFixture(t, func(fc *config.ForgeConfig) {
		fc.PrivatePaths = []string{"AGENTS.md"}
	})
	start := rev(t, fx.dir, "refs/heads/develop")
	a := fx.commit(t, "origin/develop", "develop", map[string]string{"a": "a"}, "a")
	b := fx.commit(t, "origin/develop", "side", map[string]string{"b": "b"}, "b")
	fx.commit(t, a, "merge-tmp", map[string]string{"b": "b", "AGENTS.md": "secret"}, "tree")
	tree := git(t, fx.work, "rev-parse", "HEAD^{tree}")
	merge := git(t, fx.work, "commit-tree", tree, "-p", a, "-p", b, "-m", "merge")
	mustRefuse(t, fx, "adds private path AGENTS.md", branch("develop", merge))
	fx.wantRefs(t, "develop", start, start, start)
}

// TestPrivatePathControlCharacter checks a private path with a newline is
// refused by SetPrivatePaths and skipped when set in the repo config.
func TestPrivatePathControlCharacter(t *testing.T) {
	fx := newForwardFixture(t, func(fc *config.ForgeConfig) {
		fc.PrivatePaths = []string{"AGENTS.md"}
	})
	r, err := fx.be.Repository(fx.ctx, fx.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetPrivatePaths(fx.ctx, fx.cfg, r, []string{"x\nsrc"}); err == nil || !strings.Contains(err.Error(), "control character") {
		t.Fatalf("SetPrivatePaths = %v, want a control character error", err)
	}
	git(t, fx.dir, "config", "--file", "config", "--add", "sundae.privatepath", "x\nsrc")
	if pp, err := PrivatePathsOf(fx.ctx, fx.cfg, r); err != nil || pp.Source != PrivatePathsFromForge || !slices.Equal(pp.Paths, []string{"AGENTS.md"}) {
		t.Fatalf("PrivatePathsOf = %+v, %v; want the forge's list", pp, err)
	}
	git(t, fx.dir, "config", "--file", "config", "--add", "sundae.privatepath", "keys")
	if pp, err := PrivatePathsOf(fx.ctx, fx.cfg, r); err != nil || pp.Source != PrivatePathsFromRepo || !slices.Equal(pp.Paths, []string{"keys"}) {
		t.Fatalf("PrivatePathsOf = %+v, %v; want only keys", pp, err)
	}
}

// TestForwardProtect ports t5-cases.sh CASE=protect and the default
// branch rule.
func TestForwardProtect(t *testing.T) {
	fx := newForwardFixture(t, func(fc *config.ForgeConfig) {
		fc.Protect.Deny = []string{"release/*"}
		fc.Protect.FastForwardOnly = []string{"develop"}
	})
	mainTip := rev(t, fx.dir, "refs/heads/main")

	// 9: a fast-forward of a fast_forward_only branch is forwarded.
	c1 := fx.commit(t, "origin/develop", "develop", nil, "c1")
	mustPush(t, fx, branch("develop", c1))
	fx.wantRefs(t, "develop", c1, c1, c1)

	// 10: its rewrite and delete are refused.
	git(t, fx.work, "commit", "--quiet", "--amend", "--allow-empty", "-m", "c1 reworded")
	mustRefuse(t, fx, "develop allows fast-forward only; rewrites are refused", branch("develop", git(t, fx.work, "rev-parse", "HEAD")))
	mustRefuse(t, fx, "develop allows fast-forward only; it cannot be deleted", branch("develop", ""))
	fx.wantRefs(t, "develop", c1, c1, c1)

	// 11: a denied branch is refused, and refuses the whole push.
	rel := fx.commit(t, "origin/main", "release/1", nil, "release")
	c2 := fx.commit(t, c1, "develop", nil, "c2")
	mustRefuse(t, fx, "release/1 is protected; pushes to it are refused", branch("develop", c2), branch("release/1", rel))
	fx.wantRefs(t, "develop", c1, c1, c1)
	fx.wantRefs(t, "release/1", "", "", "")

	// The default branch is protected by default.
	m := fx.commit(t, "origin/main", "main", nil, "main work")
	mustRefuse(t, fx, "main is protected; pushes to it are refused", branch("main", m))
	fx.wantRefs(t, "main", mainTip, mainTip, mainTip)
}

// TestForwardFastForwardOnlyKept checks protect.fast_forward_only also
// applies to a branch kept in Sundae.
func TestForwardFastForwardOnlyKept(t *testing.T) {
	fx := newForwardFixture(t, func(fc *config.ForgeConfig) {
		fc.Keep = []string{"stable"}
		fc.Protect.FastForwardOnly = []string{"stable"}
	})
	c1 := fx.commit(t, "origin/develop", "stable", nil, "c1")
	mustPush(t, fx, branch("stable", c1))
	c2 := fx.commit(t, "", "stable", nil, "c2")
	if out := mustPush(t, fx, branch("stable", c2)); !strings.Contains(out, "stable kept in sundae only") {
		t.Errorf("output = %q", out)
	}
	fx.wantRefs(t, "stable", "", c2, "")

	git(t, fx.work, "commit", "--quiet", "--amend", "--allow-empty", "-m", "c2 reworded")
	mustRefuse(t, fx, "stable allows fast-forward only; rewrites are refused", branch("stable", git(t, fx.work, "rev-parse", "HEAD")))
	mustRefuse(t, fx, "stable allows fast-forward only; it cannot be deleted", branch("stable", ""))
	fx.wantRefs(t, "stable", "", c2, "")
}

func TestForwardDefaultBranchUnprotected(t *testing.T) {
	fx := newForwardFixture(t, func(fc *config.ForgeConfig) {
		off := false
		fc.ProtectDefaultBranch = &off
	})
	m := fx.commit(t, "origin/main", "main", nil, "main work")
	mustPush(t, fx, branch("main", m))
	fx.wantRefs(t, "main", m, m, m)
}

// TestForwardOtherRefs checks tags and refs/sundae/* stay in Sundae: they
// are accepted and never reach the forge.
func TestForwardOtherRefs(t *testing.T) {
	fx := newForwardFixture(t, nil)
	c := fx.commit(t, "origin/develop", "develop", map[string]string{"AGENTS.md": "x"}, "c")
	out := mustPush(t, fx, refUpdate{ref: "refs/tags/v2", sha: c}, refUpdate{ref: "refs/sundae/private", sha: c})
	if out != "" {
		t.Errorf("output = %q, want none", out)
	}
	if rev(t, fx.up.bare, "refs/tags/v2") != "" || rev(t, fx.up.bare, "refs/sundae/private") != "" {
		t.Error("a tag or refs/sundae ref reached the forge")
	}
}

func TestForwardUnlinkedRepo(t *testing.T) {
	fx := newServiceFixture(t)
	if _, err := fx.be.CreateRepository(fx.ctx, "plain", fx.user, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	args := []hooks.HookArg{{OldSha: zero, NewSha: strings.Repeat("1", 40), RefName: "refs/heads/main"}}
	if err := fx.svc.Forward(fx.ctx, &out, "plain", args); err != nil || out.Len() != 0 {
		t.Errorf("Forward = %v, output %q; want a silent no-op", err, out.String())
	}
	fx.svc.RecordForward(fx.ctx, &out, "plain", args)
	if out.Len() != 0 {
		t.Errorf("RecordForward output %q; want none", out.String())
	}
}

// TestForwardRefusedAfterForward checks a forwarded branch Sundae refused is
// recorded and reported, leaving a concurrent push's pending forward alone.
func TestForwardRefusedAfterForward(t *testing.T) {
	fx := newForwardFixture(t, func(fc *config.ForgeConfig) {
		fc.Keep = []string{"push-*"}
	})
	start := rev(t, fx.dir, "refs/heads/develop")
	c1 := fx.commit(t, "origin/develop", "develop", map[string]string{"c1": "1"}, "c1")
	git(t, fx.work, "push", "--quiet", fx.dir, c1+":refs/heads/push-x")
	git(t, fx.dir, "update-ref", "-d", "refs/heads/push-x")
	first := []hooks.HookArg{
		{OldSha: start, NewSha: c1, RefName: "refs/heads/develop"},
		{OldSha: zero, NewSha: c1, RefName: "refs/heads/push-x"},
	}
	var out bytes.Buffer
	if err := fx.svc.Forward(fx.ctx, &out, fx.repo, first); err != nil {
		t.Fatalf("Forward: %v\n%s", err, out.String())
	}

	// Another push forwards feature/b before the first one's post-receive.
	b := fx.commit(t, "origin/develop", "feature/b", map[string]string{"b": "b"}, "b")
	git(t, fx.work, "push", "--quiet", fx.dir, b+":refs/tmp/b")
	git(t, fx.dir, "update-ref", "-d", "refs/tmp/b")
	second := []hooks.HookArg{{OldSha: zero, NewSha: b, RefName: "refs/heads/feature/b"}}
	if err := fx.svc.Forward(fx.ctx, &out, fx.repo, second); err != nil {
		t.Fatalf("Forward: %v\n%s", err, out.String())
	}

	// An update hook refused develop: only push-x moves.
	git(t, fx.dir, "update-ref", "refs/heads/push-x", c1)
	out.Reset()
	fx.svc.RecordForward(fx.ctx, &out, fx.repo, first[1:])
	if got := out.String(); !strings.Contains(got, "develop was forwarded to gh but refused here") || strings.Contains(got, "feature/b") {
		t.Errorf("output = %q", got)
	}
	fx.wantRefs(t, "develop", c1, start, c1)

	git(t, fx.dir, "update-ref", "refs/heads/feature/b", b)
	out.Reset()
	fx.svc.RecordForward(fx.ctx, &out, fx.repo, second)
	if out.Len() != 0 {
		t.Errorf("output = %q, want none", out.String())
	}
	fx.wantRefs(t, "feature/b", b, b, b)
	if _, err := os.Stat(filepath.Join(fx.dir, pendingFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pending forwards left behind: %v", err)
	}
}

// TestForwardUnconfiguredForge checks a push linked to an unconfigured forge
// is accepted with a warning, and an unlinked repository stays silent.
func TestForwardUnconfiguredForge(t *testing.T) {
	fx := newForwardFixture(t, nil)
	cfg := *fx.cfg
	cfg.Forges = nil
	var logs bytes.Buffer
	svc, err := NewService(&cfg, fx.be, log.New(&logs))
	if err != nil {
		t.Fatal(err)
	}
	c := fx.commit(t, "origin/develop", "develop", nil, "c")
	var out bytes.Buffer
	args := []hooks.HookArg{{OldSha: rev(t, fx.dir, "refs/heads/develop"), NewSha: c, RefName: "refs/heads/develop"}}
	if err := svc.Forward(fx.ctx, &out, fx.repo, args); err != nil {
		t.Fatal(err)
	}
	want := "sundae: gh/me/proj is linked to forge gh, which is not configured; nothing was forwarded"
	if !strings.Contains(out.String(), want) || !strings.Contains(logs.String(), "WARN") {
		t.Errorf("output = %q, logs = %q", out.String(), logs.String())
	}
	svc.RecordForward(fx.ctx, &out, fx.repo, args)

	out.Reset()
	tag := []hooks.HookArg{{OldSha: zero, NewSha: c, RefName: "refs/tags/v1"}}
	if err := svc.Forward(fx.ctx, &out, fx.repo, tag); err != nil || out.Len() != 0 {
		t.Errorf("tag push: %v, output %q", err, out.String())
	}

	if _, err := fx.be.CreateRepository(fx.ctx, "plain", fx.user, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	args[0].OldSha = zero
	if err := svc.Forward(fx.ctx, &out, "plain", args); err != nil || out.Len() != 0 || logs.Len() != 0 {
		t.Errorf("unlinked: %v, output %q, logs %q", err, out.String(), logs.String())
	}
}

// TestForwardForgeUnreachable checks a forward that cannot reach the forge
// refuses the push.
func TestForwardForgeUnreachable(t *testing.T) {
	fx := newForwardFixture(t, nil)
	if err := os.RemoveAll(fx.up.bare); err != nil {
		t.Fatal(err)
	}
	c := fx.commit(t, "origin/develop", "develop", nil, "c")
	mustRefuse(t, fx, "cannot read the forge branches; nothing was pushed", branch("develop", c))
}

// TestLockAcrossServices checks the repository lock holds across Service
// instances sharing a data path, as the server and a hook process do.
func TestLockAcrossServices(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	a, err := NewService(cfg, nil, log.New(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewService(cfg, nil, log.New(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	unlock := a.lock("gh/me/proj")
	got := make(chan struct{})
	go func() {
		b.lock("gh/me/proj")()
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("second service took a held lock")
	case <-time.After(200 * time.Millisecond):
	}
	b.lock("gh/me/other")()
	unlock()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("lock not released")
	}
}

// hookSpec is what the hook subprocess of TestForwardThroughReceivePack
// needs to rebuild the service.
type hookSpec struct {
	DataPath string
	Root     string
	Forges   []config.ForgeConfig
}

const hookSpecEnv = "SUNDAE_FORWARD_TEST_HOOK"

// TestForwardThroughReceivePack pushes through a real git receive-pack,
// whose generated hooks run this test binary as the Sundae hook command:
// the forward runs in git's quarantine, and a refused push moves nothing.
func TestForwardThroughReceivePack(t *testing.T) {
	fx := newForwardFixture(t, func(fc *config.ForgeConfig) {
		fc.Keep = []string{"push-*"}
		fc.PrivatePaths = []string{"AGENTS.md"}
	})
	spec, err := json.Marshal(hookSpec{DataPath: fx.cfg.DataPath, Root: fx.root, Forges: fx.cfg.Forges})
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "sundae")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run='^TestForwardHookProcess$' -- \"$@\"\n", bin)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	push := func(refspecs ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"push", "origin"}, refspecs...)...)
		cmd.Dir = fx.work
		cmd.Env = append(os.Environ(), "SUNDAE_REPO_NAME="+fx.repo, "SUNDAE_BIN_PATH="+wrapper, hookSpecEnv+"="+string(spec))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	c1 := fx.commit(t, "origin/develop", "develop", map[string]string{"c1": "1"}, "c1")
	if out, err := push("develop", "push-x"); err == nil {
		t.Fatalf("push of a missing ref succeeded:\n%s", out)
	}
	git(t, fx.work, "branch", "push-x", c1)
	out, err := push("develop", "push-x")
	if err != nil || !strings.Contains(out, "remote: sundae: forwarded develop to gh") || !strings.Contains(out, "push-x kept in sundae only") {
		t.Fatalf("push: %v\n%s", err, out)
	}
	fx.wantRefs(t, "develop", c1, c1, c1)
	fx.wantRefs(t, "push-x", "", c1, "")

	other := fx.forgeCommit(t, "develop", "other writer")
	fx.commit(t, "", "develop", nil, "c2")
	out, err = push("develop")
	if err == nil || !strings.Contains(out, "remote: sundae: forge develop has commits you lack") || !strings.Contains(out, "pre-receive hook declined") {
		t.Fatalf("stale push: %v\n%s", err, out)
	}
	fx.wantRefs(t, "develop", other, c1, c1)

	git(t, fx.work, "fetch", "--quiet", fx.up.bare, "develop")
	git(t, fx.work, "rebase", "--quiet", "FETCH_HEAD")
	fx.commit(t, "", "develop", map[string]string{"AGENTS.md": "x"}, "agents")
	out, err = push("develop")
	if err == nil || !strings.Contains(out, "adds private path AGENTS.md") {
		t.Fatalf("private push: %v\n%s", err, out)
	}
	fx.wantRefs(t, "develop", other, c1, c1)

	git(t, fx.work, "reset", "--quiet", "--hard", "HEAD~1")
	rebased := git(t, fx.work, "rev-parse", "HEAD")
	if out, err := push("develop"); err != nil {
		t.Fatalf("rebased push: %v\n%s", err, out)
	}
	fx.wantRefs(t, "develop", rebased, rebased, rebased)
}

// TestForwardHookProcess is the Sundae hook command in the subprocesses of
// TestForwardThroughReceivePack.
func TestForwardHookProcess(t *testing.T) {
	raw := os.Getenv(hookSpecEnv)
	if raw == "" {
		t.Skip("run as a git hook by TestForwardThroughReceivePack")
	}
	os.Exit(runTestHook(t, raw)) //nolint:revive
}

func runTestHook(t *testing.T, raw string) int {
	var spec hookSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		fmt.Fprintln(os.Stderr, err) //nolint:errcheck
		return 2
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) < 2 || args[0] != "hook" {
		fmt.Fprintln(os.Stderr, "unexpected hook arguments", args) //nolint:errcheck
		return 2
	}
	if args[1] != hooks.PreReceiveHook && args[1] != hooks.PostReceiveHook {
		return 0
	}
	cfg := config.DefaultConfig()
	cfg.DataPath = spec.DataPath
	cfg.Forges = spec.Forges
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err) //nolint:errcheck
		return 2
	}
	ctx, be := newTestBackend(t, cfg)
	svc, err := NewService(cfg, be, log.New(io.Discard))
	if err != nil {
		fmt.Fprintln(os.Stderr, err) //nolint:errcheck
		return 2
	}
	svc.checkRemote = func(f *Forge, remote string) error {
		if strings.HasPrefix(remote, spec.Root+string(filepath.Separator)) {
			return nil
		}
		return f.CheckRemote(remote)
	}
	var updates []hooks.HookArg
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 3 {
			updates = append(updates, hooks.HookArg{OldSha: f[0], NewSha: f[1], RefName: f[2]})
		}
	}
	h := svc.Hooks(be)
	repo := os.Getenv("SUNDAE_REPO_NAME")
	if args[1] == hooks.PostReceiveHook {
		h.PostReceive(ctx, os.Stdout, os.Stderr, repo, updates)
		return 0
	}
	if err := h.PreReceive(ctx, os.Stdout, os.Stderr, repo, updates); err != nil {
		fmt.Fprintln(os.Stderr, err) //nolint:errcheck
		return 1
	}
	return 0
}
