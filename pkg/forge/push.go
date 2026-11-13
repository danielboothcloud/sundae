package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrBranchNotFound is returned when pushing a branch that does not exist.
var ErrBranchNotFound = errors.New("branch not found")

// UpstreamAheadError is returned when a push is refused because the forge
// has commits the local branch lacks.
type UpstreamAheadError struct {
	Branch string
	// Missing is the number of upstream commits the branch lacks, as of the
	// last sync. Zero when the forge itself rejected the push because it
	// moved after the last sync.
	Missing int
	// Stale is true when the forge rejected the push: it has commits the
	// last sync did not fetch, or (with a lease) moved since the last sync.
	Stale bool
}

func (e *UpstreamAheadError) Error() string {
	if e.Stale {
		return fmt.Sprintf("forge branch %s has moved since the last sync; run forge sync and try again", e.Branch)
	}
	return fmt.Sprintf("forge branch %s has %d commit(s) that %s lacks; sync and merge them first, or push with --force-with-lease to overwrite them", e.Branch, e.Missing, e.Branch)
}

// PushResult is the outcome of a push.
type PushResult struct {
	Branch string `json:"branch"`
	// Old is the upstream branch as of the last sync, empty when it did not
	// exist. New is what was pushed.
	Old string `json:"old,omitempty"`
	New string `json:"new"`
	// Forced is true for a --force-with-lease push that replaced commits.
	Forced bool `json:"forced"`
	// UpToDate is true when the forge already had the branch; nothing was
	// pushed.
	UpToDate bool `json:"up_to_date"`
}

// pushBranch pushes refs/heads/<branch> to remote.
//
// Without lease, a branch the forge has commits for that the local branch
// lacks (as of the last fetch) is refused before anything is sent, and the
// forge refuses a non-fast-forward on its own if it moved since. With lease,
// the push uses --force-with-lease against the last fetched upstream ref
// (refs/remotes/<forge>/heads/<branch>), so it overwrites exactly what the
// last sync saw and nothing newer.
//
// After a push, the namespace ref is moved to what was pushed.
func pushBranch(ctx context.Context, dir, forge, remote, branch string, lease bool, env []string) (*PushResult, error) {
	localRef := "refs/heads/" + branch
	trackRef := namespace(forge) + "heads/" + branch

	l, err := resolveRef(ctx, dir, localRef)
	if err != nil {
		return nil, err
	}
	if l == "" {
		return nil, fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
	}
	u, err := resolveRef(ctx, dir, trackRef)
	if err != nil {
		return nil, err
	}

	res := &PushResult{Branch: branch, Old: u, New: l}
	if u == l {
		res.UpToDate = true
		return res, nil
	}

	ff := true
	if u != "" {
		ff, err = isAncestor(ctx, dir, u, l)
		if err != nil {
			return nil, err
		}
	}
	if !ff && !lease {
		_, missing, err := aheadBehind(ctx, dir, l, u)
		if err != nil {
			return nil, err
		}
		return nil, &UpstreamAheadError{Branch: branch, Missing: missing}
	}

	args := []string{"push", "--porcelain"}
	if lease {
		args = append(args, "--force-with-lease="+localRef+":"+u)
	}
	args = append(args, remote, localRef+":"+localRef)
	out, err := runGit(ctx, dir, env, networkTimeout, args...)
	if err != nil {
		if rejected(out) {
			return nil, &UpstreamAheadError{Branch: branch, Stale: true}
		}
		return nil, err
	}

	res.Forced = !ff
	if err := updateRef(ctx, dir, trackRef, l, u); err != nil {
		// git push itself moves the remote-tracking ref when the remote's
		// fetch refspec maps the pushed branch, so it may already be there.
		if cur, rerr := resolveRef(ctx, dir, trackRef); rerr == nil && cur == l {
			return res, nil
		}
		return res, fmt.Errorf("pushed, but could not record it: %w", err)
	}
	return res, nil
}

// rejected reports whether git push --porcelain output shows a ref rejected
// because the remote has other commits ("fetch first", "non-fast-forward")
// or the lease failed ("stale info").
func rejected(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "!\t") {
			continue
		}
		for _, reason := range []string{"fetch first", "non-fast-forward", "stale info"} {
			if strings.Contains(line, reason) {
				return true
			}
		}
	}
	return false
}
