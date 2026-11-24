package openshell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
)

// PrivateRef holds a linked repository's Sundae-only files, outside any
// branch so the forward never sends them; see "Private paths" in README.md.
const PrivateRef = "refs/sundae/private"

// privateSaveTimeout bounds one repository's save, so a hung push cannot
// hold up a suspend or delete for long.
const privateSaveTimeout = 2 * time.Minute

// DefaultPrivateIdentity commits the private files when the session has
// no git identity.
var DefaultPrivateIdentity = config.GitIdentity{Name: "Sundae", Email: "sundae@localhost"}

// A checkout's local refs: the remote ref as last fetched, and the tree its
// private files were last synced from; a save sends only what changed since.
const (
	privateRemoteRef = "refs/sundae/private-remote"
	privateBaseRef   = "refs/sundae/private-base"
)

// privateScriptLib runs after "cd checkout; shift" in both scripts.
const privateScriptLib = `export GIT_LITERAL_PATHSPECS=1
ref=` + PrivateRef + `
remote=` + privateRemoteRef + `
base=` + privateBaseRef + `
empty=$(git hash-object -t tree /dev/null)
idx=$(mktemp "${TMPDIR:-/tmp}/sundae-private.XXXXXX")
trap 'rm -f "$idx" "$idx.lock" "$idx.paths"' EXIT
rm -f "$idx"
export GIT_INDEX_FILE="$idx"
tracked() { git cat-file -e "HEAD:$1" 2>/dev/null; }
# NOTE: a checkout from before the base ref falls back to its local $ref, last fetched or saved.
oldbase() { git rev-parse -q --verify "$base^{tree}" || git rev-parse -q --verify "$ref^{tree}"; }
# worktree PATHS: tree= the private files in the work tree.
worktree() {
	git read-tree --empty
	for p do
		if [ -e "$p" ] || [ -L "$p" ]; then git add -f -- "$p"; fi
	done
	tree=$(git write-tree)
}
# mix START A B SRC PATHS: tree= START, with SRC's version of the files under
# PATHS that differ between A and B.
mix() {
	git read-tree "$1"
	mix_a=$2 mix_b=$3 mix_src=$4
	shift 4
	if [ $# -gt 0 ]; then
		git diff-tree -r -z --no-renames --name-only "$mix_a" "$mix_b" -- "$@" > "$idx.paths"
		[ ! -s "$idx.paths" ] || git reset -q "$mix_src" --pathspec-from-file="$idx.paths" --pathspec-file-nul
	fi
	tree=$(git write-tree)
}
# syncbase SYNCED OLD PATHS: the base becomes SYNCED where the work tree ($w)
# matches it. NOTE: elsewhere it keeps OLD, so a stale unedited file is never saved.
syncbase() {
	sb_synced=$1 sb_old=$2
	shift 2
	mix "$sb_synced" "$sb_synced" "$w" "$sb_old" "$@"
	git update-ref "$base" "$tree"
}`

const (
	privateRestoreName = "sundae-private-restore"
	// $1: checkout, then the paths. Never overwrites a work-tree file; prints
	// "warning: ..." lines, then "restored" or "none".
	privateRestoreScript = `set -e
cd "$1"
shift
` + privateScriptLib + `
ex=$(git rev-parse --git-path info/exclude)
mkdir -p "$(dirname "$ex")"
touch "$ex"
[ ! -s "$ex" ] || [ -z "$(tail -c1 "$ex")" ] || echo >> "$ex"
for p do
	shift
	if tracked "$p"; then echo "warning: $p is tracked by the repository; not restored"; continue; fi
	set -- "$@" "$p"
	pat=/$(printf '%s' "$p" | sed -e 's/[][\\*?]/\\&/g' -e 's/ $/\\ /')
	found=
	while IFS= read -r l; do [ "$l" != "$pat" ] || { found=1; break; }; done < "$ex"
	[ -n "$found" ] || printf '%s\n' "$pat" >> "$ex"
done
if ! out=$(git fetch -q sundae "+$ref:$remote" 2>&1); then
	case $out in
	*"couldn't find remote ref"*) echo none; exit 0 ;;
	esac
	echo "warning: could not fetch $ref: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-300)"
fi
t=$(git rev-parse -q --verify "$remote^{tree}") || { echo none; exit 0; }
git read-tree "$t"
for p do
	# NOTE: checkout-index never overwrites an existing file; it reports it and exits 1.
	err=$(git ls-files -z -- "$p" | git checkout-index -z --stdin 2>&1 >/dev/null) || true
	printf '%s\n' "$err" | while IFS= read -r l; do
		case $l in ''|*' already exists, no checkout') ;; *) echo "warning: $l" ;; esac
	done
done
worktree "$@"
w=$tree
syncbase "$t" "$(oldbase || echo "$t")" "$@"
echo restored`

	privateSaveName = "sundae-private-save"
	// $1: checkout, then the paths; identity in SUNDAE_GIT_USER_*. Commits the files
	// changed since the base onto the remote tip; a rejected push is redone once.
	privateSaveScript = `set -e
cd "$1"
shift
dec() { printf '%s' "$1" | base64 -d; }
GIT_AUTHOR_NAME=$(dec "$SUNDAE_GIT_USER_NAME")
GIT_AUTHOR_EMAIL=$(dec "$SUNDAE_GIT_USER_EMAIL")
GIT_COMMITTER_NAME=$GIT_AUTHOR_NAME
GIT_COMMITTER_EMAIL=$GIT_AUTHOR_EMAIL
export GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL
msg="sundae: save private paths"
` + privateScriptLib + `
for p do
	shift
	if tracked "$p"; then echo "warning: $p is tracked by the repository; not saved"; continue; fi
	set -- "$@" "$p"
done
worktree "$@"
w=$tree
old=$(oldbase || true)
from=${old:-$empty}
changed=
[ $# -eq 0 ] || changed=$(git diff-tree -r --no-renames --name-only "$from" "$w" -- "$@")
if [ -z "$changed" ]; then
	# NOTE: never synced and nothing to save: never replace another session's files with none.
	[ -n "$old" ] || { echo empty; exit 0; }
	echo "unchanged $(git rev-parse -q --verify "$remote^{commit}" || true)"
	exit 0
fi
# build PATHS: c= the remote tip plus this checkout's changes since the base.
build() {
	if out=$(git fetch -q sundae "+$ref:$remote" 2>&1); then
		tip=$(git rev-parse "$remote^{commit}")
	else
		case $out in
		*"couldn't find remote ref"*) tip= ; git update-ref -d "$remote" ;;
		*) printf '%s\n' "$out" >&2; exit 1 ;;
		esac
	fi
	mix "${tip:-$empty}" "$from" "$w" "$w" "$@"
	c=$tip
	if [ -z "$tip" ]; then
		[ "$tree" = "$empty" ] || c=$(git commit-tree "$tree" -m "$msg")
	elif [ "$(git rev-parse "$tip^{tree}")" != "$tree" ]; then
		c=$(git commit-tree "$tree" -p "$tip" -m "$msg")
	fi
}
build "$@"
if [ -n "$c" ] && [ "$c" != "$tip" ] && ! out=$(git push --porcelain sundae "$c:$ref" 2>&1); then
	case $out in
	*"[rejected]"*|*"[remote rejected]"*) ;;
	*) printf '%s\n' "$out" >&2; exit 1 ;;
	esac
	build "$@"
	[ -z "$c" ] || [ "$c" = "$tip" ] || git push -q sundae "$c:$ref"
fi
[ -n "$c" ] || { echo empty; exit 0; }
git update-ref "$remote" "$c"
syncbase "$c" "$from" "$@"
if [ "$c" = "$tip" ]; then echo "unchanged $c"; else echo "saved $c"; fi`
)

// Results of saving a repository's private files, PrivateSave.Result.
const (
	// PrivateSaved: a new commit holds the files.
	PrivateSaved = "saved"
	// PrivateUnchanged: the ref already held them.
	PrivateUnchanged = "unchanged"
	// PrivateEmpty: none exist and none were saved before.
	PrivateEmpty = "empty"
)

// PrivateSave is what saving one repository's private files did.
type PrivateSave struct {
	Repo   string
	Paths  []string
	Result string
	// Commit is the ref's commit after the save, as last fetched for
	// PrivateUnchanged; "" for PrivateEmpty.
	Commit string
	// Warnings are the script's warnings, e.g. a path tracked upstream.
	Warnings []string
}

// ErrPrivateNotSaved is wrapped by the error of a delete refused because
// the session's private files could not be saved.
var ErrPrivateNotSaved = errors.New("the private files could not be saved")

// WithRepoResolver gives the Manager the spec of a session's repository,
// which suspend and delete need to save its private files. Without it they
// save nothing.
func WithRepoResolver(resolve func(repo string) (RepoSpec, error)) ManagerOption {
	return func(m *Manager) { m.resolveRepo = resolve }
}

// hasPrivatePaths reports whether sp's private files are kept by Sundae.
func (sp RepoSpec) hasPrivatePaths() bool {
	return sp.Forge != nil && len(sp.PrivatePaths) > 0
}

// scriptLines splits a script's output into its "warning: " lines and the
// last other line.
func scriptLines(out string) (warnings []string, result string) {
	for l := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if w, ok := strings.CutPrefix(l, "warning: "); ok {
			warnings = append(warnings, w)
		} else if l = strings.TrimSpace(l); l != "" {
			result = l
		}
	}
	return warnings, result
}

// restorePrivate restores sp's private files into the checkout in dir;
// problems are warnings on progress.
func (m *Manager) restorePrivate(ctx context.Context, sandbox, dir string, sp RepoSpec, progress io.Writer) {
	out, err := m.script(ctx, sandbox, privateRestoreName, privateRestoreScript, nil, append([]string{dir}, sp.PrivatePaths...)...)
	if err != nil {
		m.logger.Warn("cannot restore private files", "sandbox", sandbox, "repo", sp.Name, "err", err)
		fmt.Fprintf(progress, "Warning: could not restore the private files of %s: %v\n", sp.Name, err) //nolint:errcheck
		return
	}
	warnings, _ := scriptLines(out)
	for _, w := range warnings {
		fmt.Fprintf(progress, "Warning: %s: %s\n", sp.Name, w) //nolint:errcheck
	}
}

// savePrivateRepo saves sp's private files from its checkout.
func (m *Manager) savePrivateRepo(ctx context.Context, sandbox string, sp RepoSpec) (PrivateSave, error) {
	id := sp.GitIdentity
	if !id.IsSet() {
		id = DefaultPrivateIdentity
	}
	ctx, cancel := context.WithTimeout(ctx, privateSaveTimeout)
	defer cancel()
	out, err := m.script(ctx, sandbox, privateSaveName, privateSaveScript, gitIdentityEnv(id),
		append([]string{repoDir(sp.Name)}, sp.PrivatePaths...)...)
	res := PrivateSave{Repo: sp.Name, Paths: sp.PrivatePaths}
	if err != nil {
		return res, err
	}
	var result string
	res.Warnings, result = scriptLines(out)
	res.Result, res.Commit, _ = strings.Cut(result, " ")
	switch res.Result {
	case PrivateSaved, PrivateUnchanged, PrivateEmpty:
		return res, nil
	}
	return res, fmt.Errorf("openshell: %s: unexpected output %q", privateSaveName, result)
}

// privateSpecs resolves the session's repositories and returns them all
// and those with private files; unresolvable ones are skipped with a
// warning.
func (m *Manager) privateSpecs(repos []string, progress io.Writer) (all, private []RepoSpec) {
	if m.resolveRepo == nil || len(m.cfg.Forges) == 0 {
		return nil, nil
	}
	for _, r := range repos {
		sp, err := m.resolveRepo(r)
		if err != nil {
			fmt.Fprintf(progress, "Warning: skipping the private files of %s: %v\n", r, err) //nolint:errcheck
			continue
		}
		sp.Name = r
		all = append(all, sp)
		if sp.hasPrivatePaths() {
			private = append(private, sp)
		}
	}
	return all, private
}

// currentRepos returns the repositories of the running sandbox sb: as the
// Monitor knows them, else its list file, else its create-time list.
func (m *Manager) currentRepos(ctx context.Context, sb *Sandbox) []string {
	if r, ok := m.monitor.knownRepos(sb.Name); ok {
		return r
	}
	if r, err := m.readRepos(ctx, sb.Name); err == nil && r != nil {
		return r
	}
	return annotatedRepos(sb.Annotations)
}

// savePrivate saves the private files of every repository of the running
// sandbox sb that has some, after renewing its git credentials (the push
// needs them, and they may have expired). Warnings go to progress.
func (m *Manager) savePrivate(ctx context.Context, user proto.User, sb *Sandbox, progress io.Writer) ([]PrivateSave, error) {
	// NOTE: no forges, no private files: spares every suspend and delete an exec.
	if m.resolveRepo == nil || len(m.cfg.Forges) == 0 {
		return nil, nil
	}
	all, private := m.privateSpecs(m.currentRepos(ctx, sb), progress)
	if len(private) == 0 {
		return nil, nil
	}
	if _, err := m.renewCredentials(ctx, user, sb.Name, SandboxRuntime(sb), all, progress); err != nil {
		return nil, fmt.Errorf("renew the git credentials: %w", err)
	}
	var (
		saves []PrivateSave
		errs  []error
	)
	for _, sp := range private {
		res, err := m.savePrivateRepo(ctx, sb.Name, sp)
		for _, w := range res.Warnings {
			fmt.Fprintf(progress, "Warning: %s: %s\n", sp.Name, w) //nolint:errcheck
		}
		if err != nil {
			m.logger.Warn("cannot save private files", "sandbox", sb.Name, "repo", sp.Name, "err", err)
			errs = append(errs, fmt.Errorf("%s (%s): %w", sp.Name, strings.Join(sp.PrivatePaths, ", "), err))
			continue
		}
		saves = append(saves, res)
	}
	return saves, errors.Join(errs...)
}

// unsavedPrivate returns the repositories of sb, which cannot run a save,
// that have private files.
func (m *Manager) unsavedPrivate(sb *Sandbox) []string {
	_, private := m.privateSpecs(m.monitor.SessionRepos(sb), io.Discard)
	names := make([]string, 0, len(private))
	for _, sp := range private {
		names = append(names, sp.Name)
	}
	return names
}

// SavePrivate saves the private files of the user's named session's
// forge-linked repositories to refs/sundae/private (`session
// save-private`). The session must be running. Warnings go to progress.
func (m *Manager) SavePrivate(ctx context.Context, user proto.User, name string, progress io.Writer) ([]PrivateSave, error) {
	if user == nil {
		return nil, proto.ErrUnauthorized
	}
	if progress == nil {
		progress = io.Discard
	}
	name, err := sessionName(name)
	if err != nil {
		return nil, err
	}
	sandbox := SessionSandboxName(user.Username(), name)
	sb, err := m.client.GetSandbox(ctx, sandbox)
	switch {
	case IsNotFound(err):
		return nil, fmt.Errorf("%w: %q", ErrNoSession, name)
	case err != nil:
		return nil, fmt.Errorf("openshell: get sandbox %q: %w", sandbox, err)
	case !owns(sb.Labels, ownerLabels(user)):
		return nil, ErrNotOwned
	case sb.Phase != sbx.Ready:
		return nil, fmt.Errorf("session %s is %s; resume it first: session resume %s", name, sb.Phase, name)
	}
	return m.savePrivate(ctx, user, sb, progress)
}

// SavePrivateSandbox is SavePrivate for the user's sandbox by name, for
// an idle suspend; a sandbox that is not running is skipped.
func (m *Manager) SavePrivateSandbox(ctx context.Context, user proto.User, sandbox string, progress io.Writer) ([]PrivateSave, error) {
	sb, err := m.client.GetSandbox(ctx, sandbox)
	switch {
	case err != nil:
		return nil, fmt.Errorf("openshell: get sandbox %q: %w", sandbox, err)
	case !owns(sb.Labels, ownerLabels(user)):
		return nil, ErrNotOwned
	case sb.Phase != sbx.Ready:
		return nil, nil
	}
	if progress == nil {
		progress = io.Discard
	}
	return m.savePrivate(ctx, user, sb, progress)
}
