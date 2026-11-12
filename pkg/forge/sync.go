package forge

import (
	"context"
	"sort"
)

// BranchAction is what a sync did to, or found about, one local branch.
type BranchAction string

const (
	// BranchCreated: the branch existed upstream only and was created.
	BranchCreated BranchAction = "created"
	// BranchFastForwarded: the branch was strictly behind upstream and was
	// moved to it.
	BranchFastForwarded BranchAction = "fast-forwarded"
	// BranchUpToDate: the branch matches upstream.
	BranchUpToDate BranchAction = "up-to-date"
	// BranchAhead: the branch has commits upstream lacks and none of
	// upstream's are missing. Left alone; forge push publishes it.
	BranchAhead BranchAction = "ahead"
	// BranchDiverged: both sides have commits the other lacks. Left alone.
	BranchDiverged BranchAction = "diverged"
	// BranchLocalOnly: the branch does not exist upstream (it was never
	// pushed, or was deleted there). Left alone: sync never deletes.
	BranchLocalOnly BranchAction = "local-only"
	// BranchFailed: the branch could not be updated; see Error.
	BranchFailed BranchAction = "failed"
)

// BranchStatus is one branch after a sync.
type BranchStatus struct {
	Name   string       `json:"name"`
	Action BranchAction `json:"action"`
	// Local and Upstream are the object ids of refs/heads/<name> and
	// refs/remotes/<forge>/heads/<name> after the sync; empty when missing.
	Local    string `json:"local,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	// Ahead and Behind count the commits local has that upstream lacks and
	// the reverse. Both are zero when either side is missing.
	Ahead  int    `json:"ahead"`
	Behind int    `json:"behind"`
	Error  string `json:"error,omitempty"`
}

// SyncResult is the outcome of syncing one repository.
type SyncResult struct {
	Branches []BranchStatus `json:"branches"`
	// TagsCreated are upstream tags created locally.
	TagsCreated []string `json:"tags_created,omitempty"`
	// TagsConflicting are tags that exist locally with a different target
	// than upstream. They are left alone.
	TagsConflicting []string `json:"tags_conflicting,omitempty"`
}

// Changed reports whether the sync created or moved any local ref.
func (r *SyncResult) Changed() bool {
	if len(r.TagsCreated) > 0 {
		return true
	}
	for _, b := range r.Branches {
		if b.Action == BranchCreated || b.Action == BranchFastForwarded {
			return true
		}
	}
	return false
}

// Branch returns the status of the named branch.
func (r *SyncResult) Branch(name string) (BranchStatus, bool) {
	for _, b := range r.Branches {
		if b.Name == name {
			return b, true
		}
	}
	return BranchStatus{}, false
}

// namespace returns the ref prefix the forge's upstream refs are fetched
// into: refs/remotes/<forge>/. Branches go under heads/ and tags under tags/
// so both fit in one namespace and the TUI and web views, which list
// refs/heads and refs/tags, never show them.
func namespace(forge string) string {
	return "refs/remotes/" + forge + "/"
}

// fetchRefspecs are the refspecs that fetch a forge into its namespace. They
// force, because the namespace mirrors upstream exactly (an upstream force
// push moves it), and --prune applies only to their destinations.
func fetchRefspecs(forge string) []string {
	ns := namespace(forge)
	return []string{
		"+refs/heads/*:" + ns + "heads/*",
		"+refs/tags/*:" + ns + "tags/*",
	}
}

// fetchUpstream fetches remote into the forge's namespace, pruning only
// there. --no-tags stops git from following tags into refs/tags.
func fetchUpstream(ctx context.Context, dir, forge, remote string, env []string) error {
	args := append([]string{"fetch", "--prune", "--no-tags", "--no-write-fetch-head", "--quiet", remote}, fetchRefspecs(forge)...)
	_, err := runGit(ctx, dir, env, networkTimeout, args...)
	return err
}

// syncRepo fetches upstream into the forge's namespace and then brings the
// local branches and tags along without ever losing local work:
//
//   - a branch that exists only upstream is created;
//   - a branch strictly behind upstream (no local-only commits) is
//     fast-forwarded;
//   - a branch that is ahead, has diverged, or exists only locally is left
//     alone;
//   - a tag that exists only upstream is created; an existing tag is never
//     moved.
//
// Every local update is a compare-and-swap against the value read, so a
// push that lands meanwhile wins.
func syncRepo(ctx context.Context, dir, forge, remote string, env []string) (*SyncResult, error) {
	if err := fetchUpstream(ctx, dir, forge, remote, env); err != nil {
		return nil, err
	}
	return reconcile(ctx, dir, forge)
}

// reconcile applies the local side of a sync from the forge's namespace.
func reconcile(ctx context.Context, dir, forge string) (*SyncResult, error) {
	ns := namespace(forge)
	upstream, err := refMap(ctx, dir, ns+"heads/")
	if err != nil {
		return nil, err
	}
	local, err := refMap(ctx, dir, "refs/heads/")
	if err != nil {
		return nil, err
	}

	res := &SyncResult{}
	for name, u := range upstream {
		l, exists := local[name]
		st := BranchStatus{Name: name, Upstream: u, Local: l}
		switch {
		case !exists:
			if err := updateRef(ctx, dir, "refs/heads/"+name, u, ""); err != nil {
				st.Action, st.Error = BranchFailed, err.Error()
			} else {
				st.Action, st.Local = BranchCreated, u
			}
		case l == u:
			st.Action = BranchUpToDate
		default:
			behind, err := isAncestor(ctx, dir, l, u)
			if err != nil {
				st.Action, st.Error = BranchFailed, err.Error()
				break
			}
			if behind {
				if err := updateRef(ctx, dir, "refs/heads/"+name, u, l); err != nil {
					st.Action, st.Error = BranchFailed, err.Error()
				} else {
					st.Action, st.Local = BranchFastForwarded, u
				}
				break
			}
			ahead, err := isAncestor(ctx, dir, u, l)
			switch {
			case err != nil:
				st.Action, st.Error = BranchFailed, err.Error()
			case ahead:
				st.Action = BranchAhead
			default:
				st.Action = BranchDiverged
			}
		}
		if st.Local != "" && st.Local != st.Upstream {
			if a, b, err := aheadBehind(ctx, dir, st.Local, st.Upstream); err == nil {
				st.Ahead, st.Behind = a, b
			}
		}
		res.Branches = append(res.Branches, st)
	}
	for name, l := range local {
		if _, ok := upstream[name]; !ok {
			res.Branches = append(res.Branches, BranchStatus{Name: name, Action: BranchLocalOnly, Local: l})
		}
	}
	sort.Slice(res.Branches, func(i, j int) bool { return res.Branches[i].Name < res.Branches[j].Name })

	upTags, err := refMap(ctx, dir, ns+"tags/")
	if err != nil {
		return nil, err
	}
	localTags, err := refMap(ctx, dir, "refs/tags/")
	if err != nil {
		return nil, err
	}
	for name, u := range upTags {
		l, exists := localTags[name]
		switch {
		case !exists:
			if err := updateRef(ctx, dir, "refs/tags/"+name, u, ""); err != nil {
				// Created meanwhile, or blocked by a ref of the same
				// path: either way not ours to change.
				res.TagsConflicting = append(res.TagsConflicting, name)
				continue
			}
			res.TagsCreated = append(res.TagsCreated, name)
		case l != u:
			res.TagsConflicting = append(res.TagsConflicting, name)
		}
	}
	sort.Strings(res.TagsCreated)
	sort.Strings(res.TagsConflicting)

	return res, nil
}

// fixHead points a HEAD whose branch does not exist at the default branch,
// e.g. after importing an empty repository that has since got commits. A
// HEAD on an existing branch is never changed.
func fixHead(ctx context.Context, dir, defaultBranch string) error {
	if defaultBranch == "" {
		return nil
	}
	if cur := headBranch(ctx, dir); cur != "" {
		if oid, err := resolveRef(ctx, dir, "refs/heads/"+cur); err != nil || oid != "" {
			return err
		}
	}
	oid, err := resolveRef(ctx, dir, "refs/heads/"+defaultBranch)
	if err != nil || oid == "" {
		return err
	}
	_, err = runGit(ctx, dir, nil, localTimeout, "symbolic-ref", "HEAD", "refs/heads/"+defaultBranch)
	return err
}
