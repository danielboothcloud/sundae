package forge

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

// newLocal creates an empty Soft Serve side bare repository.
func newLocal(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "local.git")
	git(t, filepath.Dir(dir), "init", "--quiet", "--bare", "--initial-branch=main", dir)
	return dir
}

func mustSync(t *testing.T, local string, up *upstream) *SyncResult {
	t.Helper()
	res, err := syncRepo(t.Context(), local, "gh", up.bare, nil)
	if err != nil {
		t.Fatalf("syncRepo: %v", err)
	}
	return res
}

func wantBranch(t *testing.T, res *SyncResult, name string, action BranchAction, ahead, behind int) {
	t.Helper()
	b, ok := res.Branch(name)
	if !ok {
		t.Fatalf("branch %s missing from result %+v", name, res.Branches)
	}
	if b.Action != action || b.Ahead != ahead || b.Behind != behind {
		t.Errorf("branch %s: got %s ahead %d behind %d (%s), want %s ahead %d behind %d",
			name, b.Action, b.Ahead, b.Behind, b.Error, action, ahead, behind)
	}
}

func TestSyncRepo(t *testing.T) {
	hermeticGit(t)
	up := newUpstream(t)
	git(t, up.work, "tag", "v1")
	git(t, up.work, "push", "--quiet", "origin", "v1")
	local := newLocal(t)

	// First sync: everything is created, in the namespace and locally.
	res := mustSync(t, local, up)
	wantBranch(t, res, "main", BranchCreated, 0, 0)
	if !slices.Equal(res.TagsCreated, []string{"v1"}) {
		t.Errorf("tags created = %v, want [v1]", res.TagsCreated)
	}
	upMain := rev(t, up.bare, "refs/heads/main")
	for _, ref := range []string{"refs/heads/main", "refs/remotes/gh/heads/main"} {
		if got := rev(t, local, ref); got != upMain {
			t.Errorf("%s = %q, want %q", ref, got, upMain)
		}
	}
	if rev(t, local, "refs/remotes/gh/tags/v1") == "" || rev(t, local, "refs/tags/v1") == "" {
		t.Error("tag v1 not fetched into the namespace and created locally")
	}

	// Nothing changed: up to date, nothing created.
	res = mustSync(t, local, up)
	wantBranch(t, res, "main", BranchUpToDate, 0, 0)
	if res.Changed() {
		t.Errorf("unchanged upstream reported changes: %+v", res)
	}

	// Upstream moves on and grows a branch: fast-forward and create.
	upMain = up.commit(t, "main", "second")
	feature := up.commit(t, "feature", "feature work")
	res = mustSync(t, local, up)
	wantBranch(t, res, "main", BranchFastForwarded, 0, 0)
	wantBranch(t, res, "feature", BranchCreated, 0, 0)
	if rev(t, local, "refs/heads/main") != upMain || rev(t, local, "refs/heads/feature") != feature {
		t.Error("main not fast-forwarded or feature not created")
	}

	// Local work on main: ahead, left alone.
	work := localClone(t, local)
	localMain := commitAndPush(t, work, "main", "local work")
	res = mustSync(t, local, up)
	wantBranch(t, res, "main", BranchAhead, 1, 0)
	if rev(t, local, "refs/heads/main") != localMain {
		t.Error("sync moved a branch that is ahead")
	}

	// Upstream moves too: diverged, still left alone, namespace follows.
	upMain = up.commit(t, "main", "upstream third")
	res = mustSync(t, local, up)
	wantBranch(t, res, "main", BranchDiverged, 1, 1)
	if rev(t, local, "refs/heads/main") != localMain {
		t.Error("sync rewrote a diverged branch")
	}
	if rev(t, local, "refs/remotes/gh/heads/main") != upMain {
		t.Error("namespace did not follow upstream")
	}

	// A branch only on Soft Serve is never touched.
	localOnly := commitAndPush(t, work, "mine", "only here")
	// Upstream deletes feature: pruned from the namespace only.
	git(t, up.work, "push", "--quiet", "origin", "--delete", "feature")
	res = mustSync(t, local, up)
	wantBranch(t, res, "mine", BranchLocalOnly, 0, 0)
	wantBranch(t, res, "feature", BranchLocalOnly, 0, 0)
	if rev(t, local, "refs/remotes/gh/heads/feature") != "" {
		t.Error("deleted upstream branch not pruned from the namespace")
	}
	if rev(t, local, "refs/heads/feature") != feature || rev(t, local, "refs/heads/mine") != localOnly {
		t.Error("sync deleted or moved a local branch")
	}

	// Upstream moves a tag and deletes another: the local tag stays, the
	// namespace follows.
	git(t, up.work, "checkout", "--quiet", "main")
	git(t, up.work, "tag", "--force", "v1")
	git(t, up.work, "push", "--quiet", "--force", "origin", "v1")
	oldTag := rev(t, local, "refs/tags/v1")
	res = mustSync(t, local, up)
	if !slices.Equal(res.TagsConflicting, []string{"v1"}) {
		t.Errorf("tags conflicting = %v, want [v1]", res.TagsConflicting)
	}
	if rev(t, local, "refs/tags/v1") != oldTag {
		t.Error("sync moved a local tag")
	}
	git(t, up.work, "push", "--quiet", "origin", "--delete", "v1")
	mustSync(t, local, up)
	if rev(t, local, "refs/remotes/gh/tags/v1") != "" || rev(t, local, "refs/tags/v1") != oldTag {
		t.Error("tag prune escaped the namespace")
	}

	// An upstream force push that drops history moves the namespace, never
	// the local branch.
	git(t, up.work, "checkout", "--quiet", "-b", "rewritten", "main~2")
	forced := up.commit(t, "rewritten", "rewrite")
	git(t, up.work, "push", "--quiet", "--force", "origin", "rewritten:main")
	res = mustSync(t, local, up)
	if rev(t, local, "refs/remotes/gh/heads/main") != forced {
		t.Error("namespace did not follow an upstream force push")
	}
	wantBranch(t, res, "main", BranchDiverged, 2, 1)
	if rev(t, local, "refs/heads/main") != localMain {
		t.Error("an upstream force push rewrote the local branch")
	}
}

func TestSyncRepoFetchError(t *testing.T) {
	hermeticGit(t)
	local := newLocal(t)
	_, err := syncRepo(t.Context(), local, "gh", filepath.Join(t.TempDir(), "missing.git"), nil)
	if err == nil {
		t.Fatal("sync from a missing upstream succeeded")
	}
}

func TestFixHead(t *testing.T) {
	hermeticGit(t)
	up := newUpstream(t)
	up.commit(t, "trunk", "trunk")
	local := newLocal(t)
	git(t, local, "symbolic-ref", "HEAD", "refs/heads/nothing")
	mustSync(t, local, up)

	if err := fixHead(t.Context(), local, "trunk"); err != nil {
		t.Fatal(err)
	}
	if got := headBranch(t.Context(), local); got != "trunk" {
		t.Errorf("HEAD = %q, want trunk", got)
	}
	// A HEAD on an existing branch is left alone.
	if err := fixHead(t.Context(), local, "main"); err != nil {
		t.Fatal(err)
	}
	if got := headBranch(t.Context(), local); got != "trunk" {
		t.Errorf("HEAD = %q, want trunk", got)
	}
}

func TestPushBranch(t *testing.T) {
	hermeticGit(t)
	ctx := t.Context()
	up := newUpstream(t)
	local := newLocal(t)
	mustSync(t, local, up)
	work := localClone(t, local)

	// Local is ahead: a plain push fast-forwards the forge.
	ahead := commitAndPush(t, work, "main", "local work")
	res, err := pushBranch(ctx, local, "gh", up.bare, "main", false, nil)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.Forced || res.UpToDate || res.New != ahead {
		t.Errorf("unexpected result %+v", res)
	}
	if rev(t, up.bare, "refs/heads/main") != ahead || rev(t, local, "refs/remotes/gh/heads/main") != ahead {
		t.Error("forge or namespace not at the pushed commit")
	}

	// Nothing new: up to date, nothing sent.
	res, err = pushBranch(ctx, local, "gh", up.bare, "main", false, nil)
	if err != nil || !res.UpToDate {
		t.Fatalf("push = %+v, %v; want up to date", res, err)
	}

	// Diverged as of the last sync: refused before sending, with the count.
	git(t, up.work, "pull", "--quiet", "--ff-only", "origin", "main")
	upNew := up.commit(t, "main", "upstream work")
	mustSync(t, local, up)
	localNew := commitAndPush(t, work, "main", "more local work")
	_, err = pushBranch(ctx, local, "gh", up.bare, "main", false, nil)
	var ua *UpstreamAheadError
	if !errors.As(err, &ua) || ua.Missing != 1 || ua.Stale {
		t.Fatalf("push of a diverged branch: err = %v, want UpstreamAheadError with 1 missing", err)
	}
	if rev(t, up.bare, "refs/heads/main") != upNew {
		t.Error("a refused push changed the forge")
	}

	// The lease is the last fetched upstream ref: if the forge moved since,
	// the forced push is refused too.
	upNewer := up.commit(t, "main", "upstream moved after the sync")
	_, err = pushBranch(ctx, local, "gh", up.bare, "main", true, nil)
	if !errors.As(err, &ua) || !ua.Stale {
		t.Fatalf("lease push after upstream moved: err = %v, want stale UpstreamAheadError", err)
	}
	if rev(t, up.bare, "refs/heads/main") != upNewer {
		t.Error("a failed lease push changed the forge")
	}

	// After a sync the lease matches and the push replaces upstream.
	mustSync(t, local, up)
	res, err = pushBranch(ctx, local, "gh", up.bare, "main", true, nil)
	if err != nil {
		t.Fatalf("lease push: %v", err)
	}
	if !res.Forced || res.Old != upNewer || res.New != localNew {
		t.Errorf("unexpected result %+v", res)
	}
	if rev(t, up.bare, "refs/heads/main") != localNew || rev(t, local, "refs/remotes/gh/heads/main") != localNew {
		t.Error("forge or namespace not at the force-pushed commit")
	}

	// The namespace says fast-forward, but the forge moved since the last
	// sync: the forge itself refuses, reported as stale.
	stale := up.commit(t, "main", "unfetched upstream work")
	_ = stale
	commitAndPush(t, work, "main", "yet more local work")
	_, err = pushBranch(ctx, local, "gh", up.bare, "main", false, nil)
	if !errors.As(err, &ua) || !ua.Stale {
		t.Fatalf("push over unfetched upstream work: err = %v, want stale UpstreamAheadError", err)
	}

	// A new branch the forge does not have is created there.
	topic := commitAndPush(t, work, "topic", "new branch")
	if _, err := pushBranch(ctx, local, "gh", up.bare, "topic", false, nil); err != nil {
		t.Fatalf("push new branch: %v", err)
	}
	if rev(t, up.bare, "refs/heads/topic") != topic {
		t.Error("new branch not created on the forge")
	}

	if _, err := pushBranch(ctx, local, "gh", up.bare, "nope", false, nil); !errors.Is(err, ErrBranchNotFound) {
		t.Errorf("push of a missing branch: err = %v, want ErrBranchNotFound", err)
	}
}

func TestCheckBranchName(t *testing.T) {
	hermeticGit(t)
	local := newLocal(t)
	for _, bad := range []string{"", "-f", "a..b", "x:refs/heads/main", "a b", "refs/heads/../x"} {
		if err := checkBranchName(t.Context(), local, bad); err == nil {
			t.Errorf("checkBranchName(%q) accepted", bad)
		}
	}
	for _, good := range []string{"main", "feature/x", "v1.2"} {
		if err := checkBranchName(t.Context(), local, good); err != nil {
			t.Errorf("checkBranchName(%q): %v", good, err)
		}
	}
}
