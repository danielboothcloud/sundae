package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/hooks"
)

// ErrPushRefused is wrapped by every error that refuses a session push.
var ErrPushRefused = errors.New("push refused")

// refusal is a refused push; its message is what the pusher sees.
type refusal struct{ msg string }

func (e *refusal) Error() string        { return e.msg }
func (e *refusal) Is(target error) bool { return target == ErrPushRefused }

func refuse(format string, args ...any) error {
	return &refusal{msg: "sundae: " + fmt.Sprintf(format, args...)}
}

// forwardUpdate is one branch update of a push that goes on to the forge.
type forwardUpdate struct {
	branch   string
	old, new string
	// forge is the branch on the forge as ls-remote reads it, "" if missing.
	forge string
}

func (u forwardUpdate) deletes() bool { return isZeroOID(u.new) }

func isZeroOID(oid string) bool {
	return oid != "" && strings.Trim(oid, "0") == ""
}

// Hooks wraps next so that pushes to forge-linked repositories are
// forwarded to the forge: PreReceive forwards (or refuses the push) and
// PostReceive records what was forwarded in the forge's namespace.
func (s *Service) Hooks(next hooks.Hooks) hooks.Hooks {
	return &forwardHooks{Hooks: next, s: s}
}

type forwardHooks struct {
	hooks.Hooks
	s *Service
}

func (h *forwardHooks) PreReceive(ctx context.Context, stdout, stderr io.Writer, repo string, args []hooks.HookArg) error {
	if err := h.Hooks.PreReceive(ctx, stdout, stderr, repo, args); err != nil {
		return err
	}
	return h.s.Forward(ctx, stderr, repo, args)
}

func (h *forwardHooks) PostReceive(ctx context.Context, stdout, stderr io.Writer, repo string, args []hooks.HookArg) {
	h.Hooks.PostReceive(ctx, stdout, stderr, repo, args)
	h.s.RecordForward(ctx, stderr, repo, args)
}

// forwardTarget returns the link and forge of a repository whose pushes are
// forwarded: no link for an unlinked repository, and a link without a forge
// when its forge is no longer configured.
func (s *Service) forwardTarget(ctx context.Context, repo string) (*Link, *Forge, error) {
	r, err := s.be.Repository(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	l, err := readLink(r)
	if err != nil || l == nil {
		return nil, nil, err
	}
	return l, s.byName[l.Forge], nil
}

// Forward forwards the branch updates of a push to a forge-linked
// repository to its forge, in one atomic push, and returns an error that
// refuses the whole push when any branch may not move or the forge refuses
// it. It runs in pre-receive, so the pushed objects are still quarantined
// and nothing has moved in Sundae yet. Progress is written to out.
func (s *Service) Forward(ctx context.Context, out io.Writer, repo string, updates []hooks.HookArg) error {
	if !slices.ContainsFunc(updates, isBranchUpdate) {
		return nil
	}
	l, f, err := s.forwardTarget(ctx, repo)
	if err != nil {
		return fmt.Errorf("sundae: cannot read the forge link of %s: %w", repo, err)
	}
	if l == nil {
		return nil
	}
	if f == nil {
		fmt.Fprintf(out, "sundae: %s is linked to forge %s, which is not configured; nothing was forwarded\n", l.Repo, l.Forge) //nolint:errcheck
		s.logger.Warn("repository is linked to a forge that is not configured; nothing was forwarded", "repo", l.Repo, "forge", l.Forge)
		return nil
	}
	fwd, err := planForward(ctx, out, f, l, updates)
	if err != nil || len(fwd) == 0 {
		return err
	}

	unlock := s.lock(l.Repo)
	defer unlock()

	rr, _ := s.knownListing(f).Find(l.Path)
	remote, err := s.linkRemote(ctx, f, l, rr.CloneURL)
	if err != nil {
		return refuse("cannot forward to %s: %v", f.Name(), err)
	}
	env, cleanup, err := f.gitEnv()
	if err != nil {
		return refuse("cannot forward to %s: %v", f.Name(), err)
	}
	defer cleanup()
	pushed, err := pushForward(ctx, out, l.Dir, f.Name(), remote, env, fwd)
	if err != nil || len(pushed) == 0 {
		return err
	}
	rec := pendingForward{Time: time.Now(), Updates: updates, Forwarded: pushed}
	if err := addPending(l.Dir, rec); err != nil {
		s.logger.Warn("could not record a pending forward", "repo", l.Repo, "err", err)
	}
	return nil
}

func isBranchUpdate(u hooks.HookArg) bool { return strings.HasPrefix(u.RefName, "refs/heads/") }

// planForward applies the forge's branch rules and the private-path guard
// to a push and returns the updates to forward. Nothing here talks to the
// forge.
func planForward(ctx context.Context, out io.Writer, f *Forge, l *Link, updates []hooks.HookArg) ([]forwardUpdate, error) {
	fc := f.Config()
	var fwd []forwardUpdate
	for _, u := range updates {
		branch, ok := strings.CutPrefix(u.RefName, "refs/heads/")
		if !ok {
			// Tags and Sundae's own refs (refs/sundae/*) stay here.
			continue
		}
		rule := fc.BranchRule(branch, l.DefaultBranch)
		if rule.Deny {
			return nil, refuse("%s is protected; pushes to it are refused", branch)
		}
		if rule.FastForwardOnly {
			if err := checkFastForward(ctx, l.Dir, branch, u); err != nil {
				return nil, err
			}
		}
		if !rule.Forward {
			fmt.Fprintf(out, "sundae: %s kept in sundae only\n", branch) //nolint:errcheck
			continue
		}
		fwd = append(fwd, forwardUpdate{branch: branch, old: u.OldSha, new: u.NewSha})
	}
	if len(fwd) == 0 {
		return nil, nil
	}
	paths, err := effectivePrivatePaths(ctx, l.Dir, fc.PrivatePaths)
	if err != nil {
		return nil, fmt.Errorf("sundae: reading private paths: %w", err)
	}
	if err := guardPrivatePaths(ctx, out, l, fwd, paths.Paths); err != nil {
		return nil, err
	}
	return fwd, nil
}

// checkFastForward refuses a delete or rewrite of a fast_forward_only
// branch, kept or forwarded alike.
func checkFastForward(ctx context.Context, dir, branch string, u hooks.HookArg) error {
	if isZeroOID(u.NewSha) {
		return refuse("%s allows fast-forward only; it cannot be deleted", branch)
	}
	if isZeroOID(u.OldSha) {
		return nil
	}
	ff, err := isAncestor(ctx, dir, u.OldSha, u.NewSha)
	if err != nil {
		return fmt.Errorf("sundae: %s: %w", branch, err)
	}
	if !ff {
		return refuse("%s allows fast-forward only; rewrites are refused", branch)
	}
	return nil
}

// guardPrivatePaths refuses forwarded commits that add a private path. Only
// commits the forge does not have yet (as of the namespace) are checked; a
// private path the forge's default branch already tracks is not guarded.
func guardPrivatePaths(ctx context.Context, out io.Writer, l *Link, fwd []forwardUpdate, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	ns := namespace(l.Forge)
	// NOTE: --not --all would skip commits already on Sundae-only branches
	// (e.g. a kept push-* bookmark), letting their private files through.
	args := []string{"rev-list"}
	for _, u := range fwd {
		if !u.deletes() {
			args = append(args, u.new)
		}
	}
	if len(args) == 1 {
		return nil
	}
	args = append(args, "--not", "--glob="+ns+"*")
	commits, err := runGit(ctx, l.Dir, nil, localTimeout, args...)
	if err != nil {
		return fmt.Errorf("sundae: listing pushed commits: %w", err)
	}

	tracked := map[string]bool{}
	warned := map[string]bool{}
	if l.DefaultBranch != "" {
		tip := ns + "heads/" + l.DefaultBranch
		for _, p := range paths {
			if _, err := runGit(ctx, l.Dir, nil, localTimeout, "cat-file", "-e", tip+":"+p); err == nil {
				tracked[p] = true
			}
		}
	}
	for _, c := range strings.Fields(commits) {
		// -c: without it diff-tree prints nothing for a merge, so a file the
		// merge itself adds would slip through.
		added, err := runGit(ctx, l.Dir, nil, localTimeout,
			"diff-tree", "-c", "--no-commit-id", "--name-only", "--diff-filter=A", "-r", "--root", "-z", c)
		if err != nil {
			return fmt.Errorf("sundae: reading commit %s: %w", c, err)
		}
		for _, file := range strings.Split(added, "\x00") {
			for _, p := range paths {
				if file == "" || !config.MatchPrivatePath([]string{p}, file) {
					continue
				}
				if !tracked[p] {
					return refuse("commit %.8s adds private path %s; Sundae keeps it, the forge must never receive it", c, file)
				}
				if !warned[p] {
					warned[p] = true
					fmt.Fprintf(out, "sundae: warning: private path %s is tracked by the forge, so it is not guarded\n", p) //nolint:errcheck
				}
			}
		}
	}
	return nil
}

// pushForward checks each update against the forge's current branch,
// pushes them all in one atomic, leased push and returns the pushed ones.
func pushForward(ctx context.Context, out io.Writer, dir, forge, remote string, env []string, fwd []forwardUpdate) ([]hooks.HookArg, error) {
	current, err := lsRemoteHeads(ctx, dir, remote, env, fwd)
	if err != nil {
		return nil, refuse("cannot read the forge branches; nothing was pushed: %v", err)
	}
	args := []string{"push", "--atomic", "--porcelain"}
	var specs, names []string
	var pushed []hooks.HookArg
	for i := range fwd {
		u := &fwd[i]
		u.forge = current[u.branch]
		if u.forge == u.new || (u.deletes() && u.forge == "") {
			continue
		}
		if u.forge != "" && u.forge != u.old && !forgeIsAncestor(ctx, dir, u) {
			return nil, refuse("forge %s has commits you lack; run jj git fetch, rebase onto %s@origin, and push again", u.branch, u.branch)
		}
		ref := "refs/heads/" + u.branch
		// NOTE: a "+" refspec silently disables the lease, so the refspecs
		// have none; the lease alone allows a rewrite.
		args = append(args, "--force-with-lease="+ref+":"+u.forge)
		src := u.new
		if u.deletes() {
			src = ""
		}
		specs = append(specs, src+":"+ref)
		names = append(names, u.branch)
		pushed = append(pushed, hooks.HookArg{OldSha: u.old, NewSha: u.new, RefName: ref})
	}
	if len(specs) == 0 {
		return nil, nil
	}
	args = append(append(args, remote), specs...)
	stdout, err := runForgeGit(ctx, dir, env, args...)
	if err != nil {
		return nil, refuse("the forge refused the push; nothing was pushed: %s", pushFailure(stdout, err))
	}
	fmt.Fprintf(out, "sundae: forwarded %s to %s\n", strings.Join(names, ", "), forge) //nolint:errcheck
	return pushed, nil
}

// forgeIsAncestor reports whether the forge's branch is in the pushed
// commit; a forge commit Sundae has never fetched is not.
func forgeIsAncestor(ctx context.Context, dir string, u *forwardUpdate) bool {
	if u.deletes() {
		return false
	}
	ok, err := isAncestor(ctx, dir, u.forge, u.new)
	return err == nil && ok
}

// lsRemoteHeads returns the forge's object id of each forwarded branch;
// missing branches are absent.
func lsRemoteHeads(ctx context.Context, dir, remote string, env []string, fwd []forwardUpdate) (map[string]string, error) {
	args := []string{"ls-remote", "--heads", remote}
	for _, u := range fwd {
		args = append(args, "refs/heads/"+u.branch)
	}
	stdout, err := runForgeGit(ctx, dir, env, args...)
	if err != nil {
		return nil, errors.New(gitFailure(err))
	}
	want := map[string]bool{}
	for _, u := range fwd {
		want[u.branch] = true
	}
	heads := map[string]string{}
	for _, line := range strings.Split(stdout, "\n") {
		oid, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		// ls-remote patterns match ref name tails, so keep exact names only.
		if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok && want[b] {
			heads[b] = oid
		}
	}
	return heads, nil
}

// runForgeGit is runGit for commands that reach the forge from a hook.
// GIT_QUARANTINE_PATH is dropped because it forbids ref updates in every
// git process, including a receive-pack the transport spawns locally; the
// quarantined objects stay readable through GIT_OBJECT_DIRECTORY.
func runForgeGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, networkTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_QUARANTINE_PATH=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &gitError{args: redactArgs(args), stderr: trimStderr(stderr.String()), err: err}
	}
	return stdout.String(), nil
}

// pushFailure is why a forge push failed: the porcelain lines of refused
// refs and what the forge said, or the git error.
func pushFailure(stdout string, err error) string {
	var reasons []string
	for _, line := range strings.Split(stdout, "\n") {
		if fields := strings.Split(line, "\t"); len(fields) >= 3 && fields[0] == "!" {
			ref := fields[1]
			if _, dst, ok := strings.Cut(ref, ":"); ok {
				ref = dst
			}
			reasons = append(reasons, strings.TrimPrefix(ref, "refs/heads/")+" "+fields[2])
		}
	}
	var ge *gitError
	if errors.As(err, &ge) {
		for _, line := range strings.Split(ge.stderr, "\n") {
			if strings.HasPrefix(line, "remote:") && strings.TrimSpace(strings.TrimPrefix(line, "remote:")) != "" {
				reasons = append(reasons, strings.TrimSpace(line))
			}
		}
	}
	if len(reasons) == 0 {
		return gitFailure(err)
	}
	return strings.Join(reasons, "; ")
}

// gitFailure is the git error output of err, without the command line.
func gitFailure(err error) string {
	var ge *gitError
	if errors.As(err, &ge) && ge.stderr != "" {
		return strings.Join(strings.Fields(ge.stderr), " ")
	}
	return err.Error()
}

// RecordForward moves refs/remotes/<forge>/heads/<branch> to what a push
// forwarded. It runs in post-receive, after the quarantine, because
// pre-receive cannot update refs; a failure only leaves the namespace stale
// until the next sync. Forwarded branches Sundae then refused are recorded
// too and reported on out.
func (s *Service) RecordForward(ctx context.Context, out io.Writer, repo string, updates []hooks.HookArg) {
	if len(s.forges) == 0 {
		return
	}
	l, f, err := s.forwardTarget(ctx, repo)
	if err != nil || l == nil || f == nil {
		return
	}
	fc := f.Config()
	unlock := s.lock(l.Repo)
	defer unlock()
	ns := namespace(l.Forge)
	record := func(branch, sha string) {
		args := []string{"update-ref", "-m", "forge forward", ns + "heads/" + branch, sha}
		if isZeroOID(sha) {
			args = []string{"update-ref", "-d", ns + "heads/" + branch}
		}
		if _, err := runGit(ctx, l.Dir, nil, localTimeout, args...); err != nil {
			s.logger.Warn("could not record forwarded branch", "repo", l.Repo, "branch", branch, "err", err)
		}
	}
	for _, u := range updates {
		branch, ok := strings.CutPrefix(u.RefName, "refs/heads/")
		if ok && fc.BranchRule(branch, l.DefaultBranch).Forward {
			record(branch, u.NewSha)
		}
	}
	for _, u := range s.takePending(l, updates) {
		branch := strings.TrimPrefix(u.RefName, "refs/heads/")
		record(branch, u.NewSha)
		fmt.Fprintf(out, "sundae: warning: %s was forwarded to %s but refused here; the forge is ahead of Sundae\n", branch, l.Forge) //nolint:errcheck
		s.logger.Warn("branch forwarded to the forge but refused in Sundae", "repo", l.Repo, "forge", l.Forge, "branch", branch)
	}
}

// pendingFile, in the bare repository, holds the forwards whose
// post-receive has not run yet.
const pendingFile = "sundae-forwarded"

// pendingTTL is when a pending forward is given up on: its push failed
// after the forward, or every ref was refused, so post-receive never ran.
const pendingTTL = time.Hour

// pendingForward is one forwarding pre-receive: its whole input, to match
// it to its post-receive, and the updates it pushed to the forge.
type pendingForward struct {
	Time      time.Time       `json:"time"`
	Updates   []hooks.HookArg `json:"updates"`
	Forwarded []hooks.HookArg `json:"forwarded"`
}

// takePending removes the pending forward matching any of updates (no two
// pushes share a ref update) and returns what it forwarded but Sundae refused.
func (s *Service) takePending(l *Link, updates []hooks.HookArg) []hooks.HookArg {
	recs, err := readPending(l.Dir)
	if err != nil {
		s.logger.Warn("could not read pending forwards", "repo", l.Repo, "err", err)
		return nil
	}
	in := map[hooks.HookArg]bool{}
	for _, u := range updates {
		in[u] = true
	}
	var keep []pendingForward
	var refused []hooks.HookArg
	for _, r := range recs {
		switch {
		case slices.ContainsFunc(r.Updates, func(u hooks.HookArg) bool { return in[u] }):
			for _, u := range r.Forwarded {
				if !in[u] {
					refused = append(refused, u)
				}
			}
		case time.Since(r.Time) > pendingTTL:
			s.logger.Warn("a forwarded push was not accepted by Sundae; the forge may be ahead of it",
				"repo", l.Repo, "forge", l.Forge, "forwarded", r.Forwarded)
		default:
			keep = append(keep, r)
		}
	}
	if len(keep) != len(recs) {
		if err := writePending(l.Dir, keep); err != nil {
			s.logger.Warn("could not write pending forwards", "repo", l.Repo, "err", err)
		}
	}
	return refused
}

func readPending(dir string) ([]pendingForward, error) {
	data, err := os.ReadFile(filepath.Join(dir, pendingFile)) //nolint:gosec
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []pendingForward
	for _, line := range bytes.Split(data, []byte("\n")) {
		var r pendingForward
		if len(bytes.TrimSpace(line)) > 0 && json.Unmarshal(line, &r) == nil {
			recs = append(recs, r)
		}
	}
	return recs, nil
}

// addPending appends rec; the caller holds the repository lock.
func addPending(dir string, rec pendingForward) error {
	recs, err := readPending(dir)
	if err != nil {
		return err
	}
	return writePending(dir, append(recs, rec))
}

func writePending(dir string, recs []pendingForward) error {
	path := filepath.Join(dir, pendingFile)
	if len(recs) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	var buf bytes.Buffer
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
