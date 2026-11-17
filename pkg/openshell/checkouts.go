package openshell

import (
	"context"
	"fmt"
	"strings"
)

// Checkout update states, the State of a CheckoutUpdate.
const (
	// CheckoutUpdated: new commits were fetched and the checkout moved to
	// them.
	CheckoutUpdated = "updated"
	// CheckoutCurrent: the checkout already had the latest commits.
	CheckoutCurrent = "current"
	// CheckoutFetched: new commits were fetched, but the checkout was left
	// where it was, because it has local work (or, for git, no upstream or
	// a diverged branch). Detail says what to do.
	CheckoutFetched = "fetched"
	// CheckoutMissing: the repository has no checkout in the sandbox.
	CheckoutMissing = "missing"
	// CheckoutFailed: the fetch failed; Detail holds the end of its error.
	CheckoutFailed = "failed"
)

// CheckoutUpdate is what UpdateCheckouts did to one repository's checkout.
type CheckoutUpdate struct {
	Repo   string
	State  string
	Detail string
}

// updateCheckoutsName is the script's name in the sandbox's process list.
const updateCheckoutsName = "sundae-update-checkouts"

// updateCheckoutsScript fetches each checkout from Sundae and moves it to
// the new commits only where that cannot lose work. Its arguments are
// pairs of repository and checkout; it prints one line per pair: the
// repository, the state and a detail, tab-separated.
//
//   - jj: `jj git fetch`. When the working-copy change is empty and has no
//     description, and its parent is behind trunk(), a new change is made
//     on trunk() (jj abandons the empty one). Otherwise the change stays.
//   - git: `git fetch`, then `git merge --ff-only` with the upstream. Git
//     refuses it when local changes would be overwritten or the branch
//     diverged; the checkout then stays.
//
// The fetch uses the credentials of /tmp/sundae/gitconfig, which
// ~/.gitconfig includes.
const updateCheckoutsScript = `
emit() { printf '%s\t%s\t%s\n' "$1" "$2" "$(printf '%s' "$3" | tr '\t\n' '  ' | cut -c1-200)"; }
while [ $# -ge 2 ]; do
	repo=$1 dir=$2
	shift 2
	if [ ! -d "$dir" ]; then emit "$repo" missing ""; continue; fi
	if [ -d "$dir/.jj" ]; then
		if ! out=$(cd "$dir" && jj git fetch 2>&1); then emit "$repo" failed "$out"; continue; fi
		at() { (cd "$dir" && jj log --no-graph --ignore-working-copy -r "$1" -T 'change_id ++ "\n"' 2>/dev/null); }
		if [ -n "$(at '@- & trunk()')" ]; then emit "$repo" current ""; continue; fi
		if [ -n "$(at '@ & empty() & description(exact:"") & ~merges()')" ] && [ -n "$(at '@- & ::trunk()')" ]; then
			if out=$(cd "$dir" && jj new 'trunk()' 2>&1); then emit "$repo" updated ""; else emit "$repo" failed "$out"; fi
			continue
		fi
		emit "$repo" fetched "the working-copy change has work; run jj rebase or jj new trunk()"
		continue
	fi
	if ! out=$(git -C "$dir" fetch --prune 2>&1); then emit "$repo" failed "$out"; continue; fi
	if ! git -C "$dir" rev-parse --abbrev-ref '@{u}' >/dev/null 2>&1; then
		emit "$repo" fetched "the branch has no upstream"; continue
	fi
	before=$(git -C "$dir" rev-parse HEAD)
	if ! out=$(git -C "$dir" merge --ff-only '@{u}' 2>&1); then
		emit "$repo" fetched "not a fast-forward, or local changes conflict; run git pull"; continue
	fi
	if [ "$before" = "$(git -C "$dir" rev-parse HEAD)" ]; then emit "$repo" current ""; else emit "$repo" updated ""; fi
done`

// UpdateCheckouts fetches the checkouts of repos in the sandbox from Sundae
// and moves each to the new commits where that cannot lose work (see
// updateCheckoutsScript). The sandbox must be running and its git
// credentials current: prepare the session first, as attach does.
func (m *Manager) UpdateCheckouts(ctx context.Context, sandbox string, repos []string) ([]CheckoutUpdate, error) {
	return updateCheckouts(ctx, m.client, sandbox, repos, repoDir)
}

// updateCheckouts is UpdateCheckouts with the checkout of each repository
// given by dir.
func updateCheckouts(ctx context.Context, client Client, sandbox string, repos []string, dir func(string) string) ([]CheckoutUpdate, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	args := make([]string, 0, 2*len(repos))
	for _, r := range repos {
		args = append(args, r, dir(r))
	}
	out, err := runScript(ctx, client, sandbox, updateCheckoutsName, updateCheckoutsScript, nil, args...)
	if err != nil {
		return nil, err
	}
	got := map[string]CheckoutUpdate{}
	for line := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 {
			continue
		}
		u := CheckoutUpdate{Repo: parts[0], State: parts[1]}
		if len(parts) == 3 {
			u.Detail = strings.TrimSpace(parts[2])
		}
		got[u.Repo] = u
	}
	res := make([]CheckoutUpdate, 0, len(repos))
	for _, r := range repos {
		u, ok := got[r]
		if !ok {
			return nil, fmt.Errorf("openshell: %s: no result for %s", updateCheckoutsName, r)
		}
		res = append(res, u)
	}
	return res, nil
}
