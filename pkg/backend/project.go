package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/danielboothcloud/sundae/pkg/access"
	"github.com/danielboothcloud/sundae/pkg/db"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/utils"
	gcfg "github.com/go-git/go-git/v5/plumbing/format/config"
)

// ErrUnknownTemplate is returned for a template that is not defined in
// projects.templates.
var ErrUnknownTemplate = errors.New("unknown template")

// Where a repository keeps its default session profile: in its git config,
// as forges keep their link (see pkg/forge), so there is no database table.
const (
	repoProfileSection = "session"
	repoProfileOption  = "profile"
	repoProfileKey     = repoProfileSection + "." + repoProfileOption
)

// gitTimeout bounds one local git command run while initialising a
// repository.
const gitTimeout = 2 * time.Minute

// ProjectOptions configures CreateProject.
type ProjectOptions struct {
	proto.RepositoryOptions

	// Template names the projects.templates entry whose tree becomes the
	// repository's initial commit. Empty creates an empty repository.
	Template string

	// Profile names the sessions.profiles entry stored as the
	// repository's default session profile. Empty uses the template's
	// profile, if any.
	Profile string
}

// CreateProject creates a repository the way `repo create` and the TUI's
// new-repository form do: CreateRepository, then, from a template, a single
// initial commit holding the template's tree authored by user, and the
// default session profile in the repository's git config.
//
// The template and profile are checked before anything is created: the
// profile must be configured, and user must be able to read the template
// repository. A failure after the repository was created deletes it again.
// The caller checks that user may create the repository.
func (d *Backend) CreateProject(ctx context.Context, name string, user proto.User, opts ProjectOptions) (proto.Repository, error) {
	profile := opts.Profile
	var src *templateSource
	if opts.Template != "" {
		var err error
		src, err = d.resolveTemplate(ctx, opts.Template, user)
		if err != nil {
			return nil, err
		}
		if profile == "" {
			profile = src.profile
		}
	}
	if profile != "" {
		if err := d.cfg.Sessions.CheckProfile(profile); err != nil {
			return nil, err //nolint:wrapcheck
		}
	}

	r, err := d.CreateRepository(ctx, name, user, opts.RepositoryOptions)
	if err != nil {
		return nil, err
	}
	undo := func(cause error) (proto.Repository, error) {
		if err := d.discardRepository(ctx, r.Name()); err != nil {
			d.logger.Error("failed to delete repository after a failed initialisation", "repo", r.Name(), "err", err)
		}
		return nil, cause
	}

	if src != nil {
		if err := d.seedRepository(ctx, r.Name(), src, user); err != nil {
			return undo(fmt.Errorf("template %q: %w", src.name, err))
		}
	}
	if profile != "" {
		if err := d.SetRepoProfile(ctx, r.Name(), profile); err != nil {
			return undo(err)
		}
	}
	return d.Repository(ctx, r.Name())
}

// discardRepository deletes a repository CreateProject just created. Unlike
// DeleteRepository it sends no webhook: nothing announced the repository.
func (d *Backend) discardRepository(ctx context.Context, name string) error {
	return db.WrapError(d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		if err := d.store.DeleteRepoByName(ctx, tx, name); err != nil {
			return err //nolint:wrapcheck
		}
		return os.RemoveAll(d.repoPath(name))
	}))
}

// templateSource is a template resolved to the tree it copies.
type templateSource struct {
	name    string
	repo    string
	subdir  string
	profile string
	// dir is the template repository's path.
	dir string
	// commit and tree are the resolved object ids.
	commit, tree string
}

// resolveTemplate looks the template up in the config, checks that user may
// read its repository, and resolves its ref and subdirectory to a tree.
func (d *Backend) resolveTemplate(ctx context.Context, name string, user proto.User) (*templateSource, error) {
	t, ok := d.cfg.Projects.Templates[name]
	if !ok {
		known := d.cfg.Projects.TemplateNames()
		if len(known) == 0 {
			return nil, fmt.Errorf("%w %q: no templates are configured", ErrUnknownTemplate, name)
		}
		return nil, fmt.Errorf("%w %q: use one of %s", ErrUnknownTemplate, name, strings.Join(known, ", "))
	}

	// The template repository is named by the server's config, so saying
	// that it is missing reveals nothing a user could not already learn.
	tr, err := d.Repository(ctx, t.Repo)
	if err != nil {
		return nil, fmt.Errorf("template %q: repository %q: %w", name, t.Repo, err)
	}
	if d.AccessLevelForUser(ctx, tr.Name(), user) < access.ReadOnlyAccess {
		return nil, fmt.Errorf("template %q: %w", name, proto.ErrUnauthorized)
	}

	src := &templateSource{name: name, repo: tr.Name(), subdir: t.Subdir, profile: t.Profile, dir: d.repoPath(tr.Name())}
	rev := t.Ref
	if rev == "" {
		rev = "HEAD"
	}
	// The ref is checked by the config: a ref or hash, never an option or
	// revision syntax.
	commit, err := runGit(ctx, src.dir, nil, nil, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		if t.Ref == "" {
			return nil, fmt.Errorf("template %q: repository %q has no commits", name, t.Repo)
		}
		return nil, fmt.Errorf("template %q: ref %q not found in %q", name, t.Ref, t.Repo)
	}
	src.commit = strings.TrimSpace(commit)

	spec := src.commit + "^{tree}"
	if t.Subdir != "" {
		spec = src.commit + ":" + t.Subdir
	}
	tree, err := runGit(ctx, src.dir, nil, nil, "rev-parse", "--verify", "--quiet", spec)
	if err != nil {
		return nil, fmt.Errorf("template %q: %q has no directory %q", name, t.Repo, t.Subdir)
	}
	src.tree = strings.TrimSpace(tree)
	if typ, err := runGit(ctx, src.dir, nil, nil, "cat-file", "-t", src.tree); err != nil || strings.TrimSpace(typ) != "tree" {
		return nil, fmt.Errorf("template %q: %q is not a directory in %q", name, t.Subdir, t.Repo)
	}
	return src, nil
}

// seedRepository makes src's tree the single initial commit of the
// repository: it copies only the objects of that tree (none of the
// template's history), commits it authored by user, and points the branch
// HEAD names at it. jj repositories are bare git repositories too; jj
// assigns the commit a change id when it imports it.
func (d *Backend) seedRepository(ctx context.Context, name string, src *templateSource, user proto.User) error {
	dir := d.repoPath(name)

	objects, err := runGit(ctx, src.dir, nil, nil, "rev-list", "--objects", src.tree)
	if err != nil {
		return err
	}
	if err := copyObjects(ctx, src.dir, dir, objects); err != nil {
		return err
	}

	authorName, authorEmail := d.CommitIdentity(user)
	env := []string{
		"GIT_AUTHOR_NAME=" + authorName,
		"GIT_AUTHOR_EMAIL=" + authorEmail,
		"GIT_COMMITTER_NAME=" + authorName,
		"GIT_COMMITTER_EMAIL=" + authorEmail,
	}
	from := fmt.Sprintf("From %s at %s", src.repo, shortHash(src.commit))
	if src.subdir != "" {
		from += ", directory " + src.subdir
	}
	msg := fmt.Sprintf("Initial commit from template %s\n\n%s.\n", src.name, from)
	commit, err := runGit(ctx, dir, env, strings.NewReader(msg), "commit-tree", "--no-gpg-sign", "-F", "-", src.tree)
	if err != nil {
		return err
	}
	commit = strings.TrimSpace(commit)

	head, err := runGit(ctx, dir, nil, nil, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return err
	}
	branch := strings.TrimSpace(head)
	if _, err := runGit(ctx, dir, nil, nil, "update-ref", "-m", "sundae: created from template "+src.name, branch, commit, ""); err != nil {
		return err
	}

	if err := populateLastModified(ctx, d, name); err != nil {
		d.logger.Warn("failed to populate last-modified", "repo", name, "err", err)
	}
	return nil
}

// copyObjects copies the objects listed (`git rev-list --objects` output)
// from the repository at from into the one at to, as one pack.
func copyObjects(ctx context.Context, from, to, objects string) error {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	pack := exec.CommandContext(ctx, "git", "pack-objects", "--stdout", "-q") //nolint:gosec
	pack.Dir = from
	pack.Stdin = strings.NewReader(objects)
	var packErr bytes.Buffer
	pack.Stderr = &packErr
	out, err := pack.StdoutPipe()
	if err != nil {
		return err //nolint:wrapcheck
	}

	index := exec.CommandContext(ctx, "git", "index-pack", "--stdin") //nolint:gosec
	index.Dir = to
	index.Stdin = out
	var indexErr bytes.Buffer
	index.Stdout = io.Discard
	index.Stderr = &indexErr

	if err := pack.Start(); err != nil {
		return fmt.Errorf("git pack-objects: %w", err)
	}
	ierr := index.Run()
	perr := pack.Wait()
	if perr != nil {
		return fmt.Errorf("git pack-objects: %w: %s", perr, strings.TrimSpace(packErr.String()))
	}
	if ierr != nil {
		return fmt.Errorf("git index-pack: %w: %s", ierr, strings.TrimSpace(indexErr.String()))
	}
	return nil
}

// runGit runs git in dir with env added to the server's environment and
// returns its stdout.
func runGit(ctx context.Context, dir string, env []string, stdin io.Reader, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := fmt.Sprintf("git %s: %v", args[0], err)
		if s := strings.TrimSpace(stderr.String()); s != "" {
			msg += ": " + s
		}
		return stdout.String(), &gitError{msg: msg, err: err}
	}
	return stdout.String(), nil
}

// gitError is a failed git command.
type gitError struct {
	msg string
	err error
}

func (e *gitError) Error() string { return e.msg }
func (e *gitError) Unwrap() error { return e.err }

// exitCode returns the exit code of a failed git command, or -1.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// CommitIdentity returns the name and email of the commits Sundae makes
// on user's behalf. Sundae does not know its users' email addresses, so
// the address is the username at the host of the server's SSH URL. Without
// a user (anonymous access), the server itself is the author.
func (d *Backend) CommitIdentity(user proto.User) (name, email string) {
	host := "localhost"
	if u, err := url.Parse(d.cfg.SSH.PublicURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	if user == nil {
		name = strings.Map(func(r rune) rune {
			if r == '<' || r == '>' || r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, d.cfg.Name)
		if strings.TrimSpace(name) == "" {
			name = "Sundae"
		}
		return strings.TrimSpace(name), "sundae@" + host
	}
	return user.Username(), user.Username() + "@" + host
}

// RepoProfile returns the repository's default session profile, from its
// git config; "" when it has none. The profile may since have been removed
// from sessions.profiles.
func (d *Backend) RepoProfile(ctx context.Context, name string) (string, error) {
	name = utils.SanitizeRepo(name)
	if _, err := d.Repository(ctx, name); err != nil {
		return "", err
	}
	f, err := os.Open(filepath.Join(d.repoPath(name), "config"))
	if err != nil {
		return "", err //nolint:wrapcheck
	}
	defer f.Close() //nolint:errcheck
	cfg := gcfg.New()
	if err := gcfg.NewDecoder(f).Decode(cfg); err != nil {
		return "", fmt.Errorf("read %s git config: %w", name, err)
	}
	return strings.TrimSpace(cfg.Section(repoProfileSection).Option(repoProfileOption)), nil
}

// SetRepoProfile stores profile as the repository's default session
// profile, in its git config; "" removes it. The profile must be defined in
// sessions.profiles.
func (d *Backend) SetRepoProfile(ctx context.Context, name, profile string) error {
	name = utils.SanitizeRepo(name)
	if _, err := d.Repository(ctx, name); err != nil {
		return err
	}
	dir := d.repoPath(name)
	if profile == "" {
		if _, err := runGit(ctx, dir, nil, nil, "config", "--file", "config", "--unset-all", repoProfileKey); err != nil && exitCode(err) != 5 {
			return err
		}
		return nil
	}
	if err := d.cfg.Sessions.CheckProfile(profile); err != nil {
		return err //nolint:wrapcheck
	}
	_, err := runGit(ctx, dir, nil, nil, "config", "--file", "config", repoProfileKey, profile)
	return err
}
