package openshell

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/ssh"
	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/anmitsu/go-shlex"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// SandboxRoot is the directory repositories are cloned into.
const SandboxRoot = "/sandbox"

// tokenPrefix prefixes the name of access tokens issued to sandboxes.
const tokenPrefix = "openshell:"

// TokenStore issues and revokes Soft Serve access tokens. It is satisfied by
// *backend.Backend.
type TokenStore interface {
	CreateAccessToken(ctx context.Context, user proto.User, name string, expiresAt time.Time) (string, error)
	ListAccessTokens(ctx context.Context, user proto.User) ([]proto.AccessToken, error)
	DeleteAccessToken(ctx context.Context, user proto.User, id int64) error
}

// Manager drives the session lifecycle. It holds no session state; the
// gateway is the source of truth.
type Manager struct {
	client   Client
	tokens   TokenStore
	image    string
	cloneURL string
	command  string
	tokenTTL time.Duration
	now      func() time.Time
}

// NewManager returns a Manager using client for sandboxes and tokens for git
// credentials, configured from cfg.
func NewManager(client Client, tokens TokenStore, cfg *config.Config) *Manager {
	return &Manager{
		client:   client,
		tokens:   tokens,
		image:    cfg.OpenShell.Image,
		cloneURL: cfg.OpenShellCloneURL(),
		command:  cfg.OpenShell.Command,
		tokenTTL: cfg.OpenShell.TokenTTL,
		now:      time.Now,
	}
}

// StartOptions configures Start.
type StartOptions struct {
	// User is the authenticated caller. Required.
	User proto.User
	// Repo is the canonical repository name.
	Repo string
	// VCS selects the clone tool.
	VCS proto.VCS
	// Image overrides the configured image when the sandbox is created.
	Image string
	// Term is the client's TERM, forwarded to the attached command.
	Term string
	// Progress receives human-readable progress messages. May be nil.
	Progress io.Writer
}

// Session is a started session, ready to attach to.
type Session struct {
	// Name is the sandbox name.
	Name string
	// Created reports whether the sandbox was created by Start.
	Created bool
	// Cloned reports whether Start cloned the repository.
	Cloned bool
	// WorkDir is the repository checkout inside the sandbox.
	WorkDir string
	// Command is what runs on attach.
	Command []string
	// Env is passed to the attached command. It carries the git credentials,
	// which are never written to disk in the sandbox.
	Env map[string]string
}

// Info describes one of a user's sessions.
type Info struct {
	Repo  string
	Name  string
	Phase string
}

// TokenName returns the access token name used for a sandbox.
func TokenName(sandbox string) string { return tokenPrefix + sandbox }

// Start ensures the user's sandbox for the repository is running, clones the
// repository on first use, and returns the attach parameters.
func (m *Manager) Start(ctx context.Context, opts StartOptions) (*Session, error) {
	if opts.User == nil {
		return nil, proto.ErrUnauthorized
	}
	progress := opts.Progress
	if progress == nil {
		progress = io.Discard
	}

	command, err := shlex.Split(m.command, true)
	if err != nil || len(command) == 0 {
		return nil, fmt.Errorf("openshell: invalid command %q", m.command)
	}

	name := SandboxName(opts.User.Username(), opts.Repo)
	image := opts.Image
	if image == "" {
		image = m.image
	}

	fmt.Fprintf(progress, "Starting sandbox %s...\n", name) //nolint:errcheck
	sb, created, err := m.client.EnsureSandbox(ctx, name, SandboxRequest{
		Image:  image,
		Labels: ownerLabels(opts.User),
		Annotations: map[string]string{
			AnnotationUser: opts.User.Username(),
			AnnotationRepo: opts.Repo,
		},
	})
	if err != nil {
		return nil, err
	}
	if !created && opts.Image != "" {
		fmt.Fprintf(progress, "Warning: --image ignored, sandbox %s already exists\n", sb.Name) //nolint:errcheck
	}

	// Seam: restoring a /sandbox snapshot belongs here, before the clone
	// check, so a restored checkout is resumed rather than re-cloned.

	token, err := m.issueToken(ctx, opts.User, name)
	if err != nil {
		return nil, err
	}

	s := &Session{
		Name:    name,
		Created: created,
		WorkDir: path.Join(SandboxRoot, opts.Repo),
		Command: command,
		Env:     gitAuthEnv(m.cloneURL, opts.User.Username(), token),
	}

	exists, err := m.dirExists(ctx, name, s.WorkDir)
	if err != nil {
		return nil, err
	}
	if !exists {
		fmt.Fprintf(progress, "Cloning %s into %s...\n", opts.Repo, s.WorkDir) //nolint:errcheck
		if err := m.clone(ctx, name, opts, s); err != nil {
			return nil, err
		}
		s.Cloned = true
	}

	if opts.Term != "" {
		s.Env["TERM"] = opts.Term
	}

	return s, nil
}

// Attach starts the session command with a cols x rows PTY.
func (m *Manager) Attach(ctx context.Context, s *Session, cols, rows uint32) (InteractiveSession, error) {
	is, err := m.client.Interactive(ctx, s.Name, s.Command, cols, rows, ExecOptions{
		Env:     maps.Clone(s.Env),
		WorkDir: s.WorkDir,
	})
	if err != nil {
		return nil, fmt.Errorf("openshell: attach to sandbox %q: %w", s.Name, err)
	}
	return is, nil
}

// End deletes the user's sandbox for the repository and revokes its access
// tokens. It reports whether a sandbox existed; ending an absent session is a
// successful no-op.
func (m *Manager) End(ctx context.Context, user proto.User, repo string) (bool, error) {
	if user == nil {
		return false, proto.ErrUnauthorized
	}

	name := SandboxName(user.Username(), repo)
	existed := true
	sb, err := m.client.GetSandbox(ctx, name)
	switch {
	case IsNotFound(err):
		existed = false
	case err != nil:
		return false, fmt.Errorf("openshell: get sandbox %q: %w", name, err)
	case !owns(sb, ownerLabels(user)):
		return false, ErrNotOwned
	}

	if existed {
		// Seam: snapshotting /sandbox belongs here, before the delete.
		if err := m.client.DeleteSandbox(ctx, name); err != nil {
			return false, err
		}
	}

	if err := m.revokeTokens(ctx, user, TokenName(name), true); err != nil {
		return existed, err
	}

	return existed, nil
}

// List returns the user's sessions sorted by repository.
func (m *Manager) List(ctx context.Context, user proto.User) ([]Info, error) {
	if user == nil {
		return nil, proto.ErrUnauthorized
	}

	sbs, err := m.client.ListSandboxes(ctx, ownerLabels(user))
	if err != nil {
		return nil, err
	}

	infos := make([]Info, 0, len(sbs))
	for _, sb := range sbs {
		infos = append(infos, Info{
			Repo:  sb.Annotations[AnnotationRepo],
			Name:  sb.Name,
			Phase: string(sb.Status.Phase),
		})
	}
	slices.SortFunc(infos, func(a, b Info) int {
		if c := strings.Compare(a.Repo, b.Repo); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return infos, nil
}

// issueToken creates a fresh short-lived access token for the sandbox after
// pruning its expired predecessors. Unexpired tokens are kept so concurrent
// attachments to the same sandbox keep working until they expire or the
// session ends.
func (m *Manager) issueToken(ctx context.Context, user proto.User, sandbox string) (string, error) {
	name := TokenName(sandbox)
	if err := m.revokeTokens(ctx, user, name, false); err != nil {
		return "", err
	}

	token, err := m.tokens.CreateAccessToken(ctx, user, name, m.now().Add(m.tokenTTL))
	if err != nil {
		return "", fmt.Errorf("openshell: issue access token: %w", err)
	}
	return token, nil
}

// revokeTokens deletes the user's tokens with the given name: all of them,
// or only the expired ones.
func (m *Manager) revokeTokens(ctx context.Context, user proto.User, name string, all bool) error {
	tokens, err := m.tokens.ListAccessTokens(ctx, user)
	if err != nil {
		return fmt.Errorf("openshell: list access tokens: %w", err)
	}

	now := m.now()
	var errs []error
	for _, t := range tokens {
		if t.Name != name {
			continue
		}
		if !all && (t.ExpiresAt.IsZero() || now.Before(t.ExpiresAt)) {
			continue
		}
		if err := m.tokens.DeleteAccessToken(ctx, user, t.ID); err != nil && !errors.Is(err, proto.ErrTokenNotFound) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("openshell: revoke access tokens: %w", err)
	}
	return nil
}

// dirExists reports whether dir exists in the sandbox.
func (m *Manager) dirExists(ctx context.Context, sandbox, dir string) (bool, error) {
	res, err := m.client.Run(ctx, sandbox, []string{"test", "-d", dir}, ExecOptions{NoLoginShell: true})
	if err != nil {
		return false, fmt.Errorf("openshell: check %s: %w", dir, err)
	}
	switch res.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("openshell: check %s: exit status %d: %s", dir, res.ExitCode, trimOutput(res.Stderr))
	}
}

// clone clones the repository into s.WorkDir. The credentials travel only in
// the exec environment; the clone URL carries none.
func (m *Manager) clone(ctx context.Context, sandbox string, opts StartOptions, s *Session) error {
	if parent := path.Dir(s.WorkDir); parent != SandboxRoot {
		if err := m.run(ctx, sandbox, []string{"mkdir", "-p", parent}, nil); err != nil {
			return err
		}
	}

	return m.run(ctx, sandbox, CloneCommand(opts.VCS, m.repoURL(opts.Repo), s.WorkDir), s.Env)
}

func (m *Manager) run(ctx context.Context, sandbox string, command []string, env map[string]string) error {
	res, err := m.client.Run(ctx, sandbox, command, ExecOptions{
		Env:          maps.Clone(env),
		WorkDir:      SandboxRoot,
		NoLoginShell: true,
	})
	if err != nil {
		return fmt.Errorf("openshell: %s: %w", command[0], err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("openshell: %s: exit status %d: %s", strings.Join(command[:min(len(command), 3)], " "), res.ExitCode, trimOutput(res.Stderr))
	}
	return nil
}

// repoURL is the HTTP clone URL of the repository, without credentials.
func (m *Manager) repoURL(repo string) string {
	return m.cloneURL + "/" + repo + ".git"
}

// CloneCommand returns the command that clones url into dir: a colocated jj
// clone for jj repositories and a plain git clone otherwise.
func CloneCommand(vcs proto.VCS, url, dir string) []string {
	if vcs == proto.VCSJJ {
		return []string{"jj", "git", "clone", "--colocate", url, dir}
	}
	return []string{"git", "clone", url, dir}
}

// gitAuthEnv returns environment variables that make git send the token as
// basic-auth credentials to cloneURL only. Using GIT_CONFIG_* keeps the token
// out of .git/config and every other file in the sandbox.
func gitAuthEnv(cloneURL, username, token string) map[string]string {
	cred := base64.StdEncoding.EncodeToString([]byte(username + ":" + token))
	return map[string]string{
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "http." + cloneURL + "/.extraHeader",
		"GIT_CONFIG_VALUE_0": "Authorization: Basic " + cred,
	}
}

func ownerLabels(user proto.User) map[string]string {
	return map[string]string{
		LabelManaged: "true",
		LabelUserID:  strconv.FormatInt(user.ID(), 10),
	}
}

func trimOutput(b []byte) string {
	const max = 512
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}

// Terminal is the client side of a bridged session. *ssh.Session satisfies
// it.
type Terminal interface {
	io.ReadWriter
	Pty() (ssh.Pty, <-chan ssh.Window, bool)
}

// drainTimeout bounds how long Bridge waits for trailing output after the
// remote command has exited.
const drainTimeout = 2 * time.Second

// Bridge connects term to the interactive session: it copies input and
// output in both directions, resizes the remote PTY when the terminal's
// window changes, and returns the remote command's exit code.
//
// Cancelling ctx (e.g. the SSH client disconnecting) aborts the stream. The
// sandbox itself keeps running.
func Bridge(ctx context.Context, term Terminal, is InteractiveSession) (int, error) {
	defer is.Close() //nolint:errcheck

	done := make(chan struct{})
	defer close(done)

	_, winch, _ := term.Pty()
	go func() {
		for {
			select {
			case <-done:
				return
			case w, ok := <-winch:
				if !ok {
					return
				}
				if w.Width > 0 && w.Height > 0 {
					_ = is.Resize(uint32(w.Width), uint32(w.Height)) //nolint:gosec
				}
			}
		}
	}()

	go func() {
		select {
		case <-ctx.Done():
			_ = v1.CancelInteractive(is)
		case <-done:
		}
	}()

	go func() {
		_, _ = io.Copy(is, term)
		// The client closed its input; let the remote side see EOF.
		_ = v1.CloseInteractiveInput(is)
	}()

	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		_, _ = io.Copy(term, is)
	}()

	code, err := is.ExitCode()

	select {
	case <-outDone:
	case <-time.After(drainTimeout):
	}

	return code, err //nolint:wrapcheck
}
