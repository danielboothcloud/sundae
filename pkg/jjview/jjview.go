// Package jjview shows jj repositories through jj itself: the repository
// page's Changes tab is `jj log` and `jj show` as jj renders them.
//
// jj never runs on a served repository. Run on a bare git repository, jj
// writes its own objects and refs/jj/keep/* refs into it, which clients
// would then see. Instead each repository gets a cache copy under the data
// path: a bare git repository that borrows the served repository's objects
// (objects/info/alternates) and holds a copy of its branches and tags,
// fetched again before every view, with a jj workspace on top whose
// working copy is never checked out. Only the cache is written. A cache
// that cannot be brought up to date (the served history was rewritten and
// its old objects pruned, say) is deleted and built again.
package jjview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// ErrNoJJ is returned when the jj binary is not installed.
var ErrNoJJ = errors.New("jj is not installed on the server")

// ErrBadChange is returned for a change ID that is not one.
var ErrBadChange = errors.New("not a change ID")

// DefaultRevset is what Log shows: every change reachable from a
// bookmark or tag, which is everything the server holds.
const DefaultRevset = "::(bookmarks() | tags()) ~ root()"

// runTimeout bounds each git and jj command.
const runTimeout = 60 * time.Second

// config is jj's configuration in the cache, so the server's environment
// (or a developer's own jj config) does not change what is shown.
const config = `user.name = "Sundae"
user.email = "sundae@localhost"

[ui]
paginate = "never"
color = "never"

[snapshot]
auto-track = "none()"
`

// Viewer runs jj on cache copies of served repositories.
type Viewer struct {
	jj, git string
	root    string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

var (
	viewersMu sync.Mutex
	viewers   = map[string]*Viewer{}
)

// EnvJJPath names the environment variable that sets the jj binary: a
// path, or "off" to show jj repositories with the git log. Unset, jj is
// looked up in PATH.
const EnvJJPath = "SUNDAE_JJ_PATH"

// ForDataPath returns the process's Viewer for the data path, with its
// cache in <dataPath>/cache/jj, jj from EnvJJPath or PATH and git from
// PATH.
func ForDataPath(dataPath string) *Viewer {
	viewersMu.Lock()
	defer viewersMu.Unlock()
	if v, ok := viewers[dataPath]; ok {
		return v
	}
	var jj string
	switch p, set := os.LookupEnv(EnvJJPath); {
	case !set || p == "":
		jj, _ = exec.LookPath("jj")
	case p != "off":
		jj = p
	}
	git, _ := exec.LookPath("git")
	v := New(jj, git, filepath.Join(dataPath, "cache", "jj"))
	viewers[dataPath] = v
	return v
}

// New returns a Viewer running the jj and git binaries, with its caches in
// root. An empty jj means jj is not installed.
func New(jj, git, root string) *Viewer {
	if git == "" {
		git = "git"
	}
	return &Viewer{jj: jj, git: git, root: root, locks: map[string]*sync.Mutex{}}
}

// Available reports whether jj is installed.
func (v *Viewer) Available() bool { return v != nil && v.jj != "" }

// Line is one line of `jj log`.
type Line struct {
	// Text is the line as jj renders it: the graph, then the change.
	// With color, it holds ANSI escapes.
	Text string
	// Change is the change ID of the change this line starts; "" for the
	// lines that continue a change or only draw the graph.
	Change string
}

// LogOptions are what Log shows.
type LogOptions struct {
	// Revset selects the changes; empty is DefaultRevset.
	Revset string
	// Limit is the most changes shown; 0 is no limit.
	Limit int
	// Color renders jj's colours as ANSI escapes.
	Color bool
}

// Log returns `jj log` of the repository whose git directory is gitDir.
func (v *Viewer) Log(ctx context.Context, gitDir string, o LogOptions) ([]Line, error) {
	revset := o.Revset
	if revset == "" {
		revset = DefaultRevset
	}
	args := []string{"log", "-r", revset, "-T", `"\x1e" ++ change_id.short(12) ++ "\x1f" ++ builtin_log_compact`}
	if o.Limit > 0 {
		args = append(args, "-n", strconv.Itoa(o.Limit))
	}
	out, err := v.withWorkspace(ctx, gitDir, o.Color, args...)
	if err != nil {
		return nil, err
	}
	return parseLog(out), nil
}

// parseLog splits jj's output into lines, taking each change's ID out of
// the marker the template puts before it.
func parseLog(out string) []Line {
	var lines []Line
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if l == "" {
			continue
		}
		var change string
		if i := strings.IndexByte(l, '\x1e'); i >= 0 {
			if j := strings.IndexByte(l[i:], '\x1f'); j >= 0 {
				change = ansi.Strip(l[i+1 : i+j])
				l = l[:i] + l[i+j+1:]
			}
		}
		lines = append(lines, Line{Text: l, Change: change})
	}
	return lines
}

// changeRe matches a change ID (jj's k-z alphabet) or a commit ID prefix.
var changeRe = regexp.MustCompile(`^([k-z]{1,64}|[0-9a-f]{4,64})$`)

// Show returns `jj show` of the change: its description and diff.
func (v *Viewer) Show(ctx context.Context, gitDir, change string, color bool) (string, error) {
	if !changeRe.MatchString(change) {
		return "", fmt.Errorf("%w: %q", ErrBadChange, change)
	}
	return v.withWorkspace(ctx, gitDir, color, "show", "-r", change)
}

// withWorkspace brings gitDir's cache up to date and runs jj in it,
// building the cache again once if it is broken.
func (v *Viewer) withWorkspace(ctx context.Context, gitDir string, color bool, args ...string) (string, error) {
	if !v.Available() {
		return "", ErrNoJJ
	}
	gitDir, err := filepath.Abs(gitDir)
	if err != nil {
		return "", err
	}
	dir := v.cacheDir(gitDir)
	lock := v.lock(dir)
	lock.Lock()
	defer lock.Unlock()

	if err := v.writeConfig(); err != nil {
		return "", err
	}
	if err := v.sync(ctx, gitDir, dir); err != nil {
		if rerr := os.RemoveAll(dir); rerr != nil {
			return "", errors.Join(err, rerr)
		}
		if err := v.sync(ctx, gitDir, dir); err != nil {
			return "", err
		}
	}
	colorArg := "--color=never"
	if color {
		colorArg = "--color=always"
	}
	base := []string{"--no-pager", "--ignore-working-copy", colorArg, "-R", filepath.Join(dir, "ws")}
	return v.run(ctx, "", v.jj, append(base, args...)...)
}

// sync creates gitDir's cache if needed and brings it up to date: its
// branches and tags fetched again from the served repository, then
// imported into the jj workspace.
func (v *Viewer) sync(ctx context.Context, gitDir, dir string) error {
	cache, ws := filepath.Join(dir, "git"), filepath.Join(dir, "ws")
	if _, err := os.Stat(cache); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if _, err := v.run(ctx, "", v.git, "init", "-q", "--bare", cache); err != nil {
			return err
		}
		alt := filepath.Join(gitDir, "objects") + "\n"
		if err := os.WriteFile(filepath.Join(cache, "objects", "info", "alternates"), []byte(alt), 0o600); err != nil {
			return err
		}
		// A HEAD to no branch: jj then checks nothing out.
		if _, err := v.run(ctx, "", v.git, "-C", cache, "symbolic-ref", "HEAD", "refs/heads/sundae-none"); err != nil {
			return err
		}
	}
	if _, err := v.run(ctx, "", v.git, "-C", cache, "fetch", "-q", "--prune", "--no-tags", gitDir,
		"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return err
	}
	if _, err := os.Stat(ws); errors.Is(err, os.ErrNotExist) {
		_, err := v.run(ctx, dir, v.jj, "git", "init", "--git-repo", cache, ws)
		return err
	}
	_, err := v.run(ctx, "", v.jj, "--no-pager", "--ignore-working-copy", "-R", ws, "git", "import")
	return err
}

// cacheDir is gitDir's cache directory: named for a hash of the path, so
// a repository name cannot reach outside the cache.
func (v *Viewer) cacheDir(gitDir string) string {
	sum := sha256.Sum256([]byte(gitDir))
	return filepath.Join(v.root, hex.EncodeToString(sum[:12]))
}

func (v *Viewer) lock(dir string) *sync.Mutex {
	v.mu.Lock()
	defer v.mu.Unlock()
	l, ok := v.locks[dir]
	if !ok {
		l = &sync.Mutex{}
		v.locks[dir] = l
	}
	return l
}

func (v *Viewer) configPath() string { return filepath.Join(v.root, "config.toml") }

func (v *Viewer) writeConfig() error {
	if b, err := os.ReadFile(v.configPath()); err == nil && string(b) == config {
		return nil
	}
	if err := os.MkdirAll(v.root, 0o700); err != nil {
		return err
	}
	return os.WriteFile(v.configPath(), []byte(config), 0o600)
}

// run runs a command with the cache's jj config, returning its output.
func (v *Viewer) run(ctx context.Context, dir, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "JJ_CONFIG="+v.configPath(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		return "", fmt.Errorf("%s %s: %w: %s", filepath.Base(bin), args[0], err, msg)
	}
	return stdout.String(), nil
}
