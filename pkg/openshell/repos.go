package openshell

import (
	"context"
	"path"
	"slices"
	"strings"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
)

// A session holds one or more repositories, each cloned into
// /sandbox/<repo>. Its sandbox records the repositories it was created
// with (AnnotationRepos); OpenShell cannot change a sandbox's annotations,
// so the current list, after `session repos add|remove`, is kept in the
// sandbox in reposFile, which snapshots keep too. The server's Monitor
// remembers the lists Sundae has read or written, so listing sessions
// needs no exec; after a restart a session shows its create-time list
// until it is next started or attached.

// AnnotationRepos holds the repositories the session was created with,
// comma-separated, in order. Repository names cannot hold commas.
const AnnotationRepos = "sundae.repos"

// reposFile is the session's current repository list, one per line.
var reposFile = path.Join(sundaeStateDir, "repos")

// RepoSpec is a repository of a session as Start clones and configures
// it.
type RepoSpec struct {
	// Name is the canonical repository name.
	Name string
	// VCS selects the clone tool.
	VCS proto.VCS
	// GitIdentity is set in the checkout's git and jj config on every
	// start and attach, and given to the first clone. The zero value
	// leaves the sandbox's identity alone.
	GitIdentity config.GitIdentity
}

// annotatedRepos returns the repositories a sandbox was created with.
func annotatedRepos(a map[string]string) []string {
	if v := a[AnnotationRepos]; v != "" {
		return strings.Split(v, ",")
	}
	if v := a[AnnotationRepo]; v != "" {
		return []string{v}
	}
	return nil
}

// rememberRepos records sandbox's current repository list. A nil
// Monitor remembers nothing.
func (m *Monitor) rememberRepos(sandbox string, repos []string) {
	if m == nil {
		return
	}
	m.reposMu.Lock()
	defer m.reposMu.Unlock()
	m.repoLists[sandbox] = slices.Clone(repos)
}

// forgetRepos drops what is known of sandbox's list.
func (m *Monitor) forgetRepos(sandbox string) {
	if m == nil {
		return
	}
	m.reposMu.Lock()
	defer m.reposMu.Unlock()
	delete(m.repoLists, sandbox)
}

// knownRepos returns sandbox's list as last read or written, if known.
func (m *Monitor) knownRepos(sandbox string) ([]string, bool) {
	if m == nil {
		return nil, false
	}
	m.reposMu.Lock()
	defer m.reposMu.Unlock()
	r, ok := m.repoLists[sandbox]
	return slices.Clone(r), ok
}

// SessionRepos returns the repositories of the session sb holds: its
// current list when known, else the list it was created with. A nil
// Monitor knows no list.
func (m *Monitor) SessionRepos(sb *Sandbox) []string {
	if sb == nil {
		return nil
	}
	if r, ok := m.knownRepos(sb.Name); ok {
		return r
	}
	return annotatedRepos(sb.Annotations)
}

// repoDir is the checkout of repo in the sandbox.
func repoDir(repo string) string { return path.Join(SandboxRoot, repo) }

// sessionWorkDir is where a session's program runs: the checkout of its
// only repository, else /sandbox.
func sessionWorkDir(repos []string) string {
	if len(repos) == 1 {
		return repoDir(repos[0])
	}
	return SandboxRoot
}

// mergeRepos returns cur with add appended (those not in it already, in
// order) and remove taken out.
func mergeRepos(cur, add, remove []string) []string {
	out := make([]string, 0, len(cur)+len(add))
	for _, r := range append(slices.Clone(cur), add...) {
		if r != "" && !slices.Contains(out, r) && !slices.Contains(remove, r) {
			out = append(out, r)
		}
	}
	return out
}

// readReposScript prints the session's repository list file, if any.
const (
	readReposName   = "sundae-repos-read"
	readReposScript = `[ -f "$1" ] && cat "$1" || true`
	// writeReposScript writes $SUNDAE_REPOS (base64, one repository per
	// line) to the list file atomically.
	writeReposName   = "sundae-repos-write"
	writeReposScript = `set -e
mkdir -p "$(dirname "$1")"
printf '%s' "$SUNDAE_REPOS" | base64 -d > "$1.part"
mv -f "$1.part" "$1"`
)

// readRepos returns the sandbox's current repository list; nil when the
// sandbox has no list file (its create-time list applies).
func (m *Manager) readRepos(ctx context.Context, sandbox string) ([]string, error) {
	out, err := m.script(ctx, sandbox, readReposName, readReposScript, nil, reposFile)
	if err != nil {
		return nil, err
	}
	var repos []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" && !slices.Contains(repos, l) {
			repos = append(repos, l)
		}
	}
	return repos, nil
}

// writeRepos writes the sandbox's current repository list.
func (m *Manager) writeRepos(ctx context.Context, sandbox string, repos []string) error {
	_, err := m.script(ctx, sandbox, writeReposName, writeReposScript,
		map[string]string{"SUNDAE_REPOS": execEnvValue(strings.Join(repos, "\n") + "\n")}, reposFile)
	return err
}

// DefaultSessionNameFor is the session name `session start <repo>` uses
// for repo when none is given: the last path segment of the repository,
// as a valid session name; DefaultSessionName when nothing is left.
func DefaultSessionNameFor(repo string) string {
	seg := repo
	if i := strings.LastIndex(seg, "/"); i >= 0 {
		seg = seg[i+1:]
	}
	seg = dnsClean(strings.TrimSuffix(seg, ".git"))
	if len(seg) > 32 {
		seg = strings.TrimRight(seg[:32], "-")
	}
	if !ValidSessionName(seg) {
		return DefaultSessionName
	}
	return seg
}
