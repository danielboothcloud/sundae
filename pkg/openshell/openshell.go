// Package openshell launches and resumes OpenShell agent sessions for Soft
// Serve repositories.
//
// Soft Serve keeps no session state: sandbox names are derived from the user
// and repository (see SandboxName) and the OpenShell gateway is the source of
// truth for which sessions exist.
package openshell

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/charmbracelet/soft-serve/pkg/config"
)

// Re-exported SDK types so callers don't need to import the SDK directly.
type (
	// Sandbox is an OpenShell sandbox.
	Sandbox = v1.Sandbox
	// ExecOptions configures a command execution in a sandbox.
	ExecOptions = v1.ExecOptions
	// ExecResult is the collected output of a completed command.
	ExecResult = v1.ExecResult
	// InteractiveSession is a bidirectional, resizable command stream.
	InteractiveSession = v1.InteractiveSession
)

const (
	// LabelUserID is the sandbox label holding the owning Soft Serve user ID.
	LabelUserID = "soft-serve.user-id"
	// LabelManaged marks sandboxes created by Soft Serve.
	LabelManaged = "soft-serve.managed"
	// AnnotationUser holds the owning Soft Serve username.
	AnnotationUser = "soft-serve.user"
	// AnnotationRepo holds the repository name the sandbox was created for.
	AnnotationRepo = "soft-serve.repo"

	// readyTimeout bounds how long Ensure waits for a sandbox to become ready.
	readyTimeout = 5 * time.Minute
	// cleanupTimeout bounds best-effort cleanup calls.
	cleanupTimeout = 30 * time.Second
)

// ErrNotOwned is returned when a sandbox with the expected name exists but
// belongs to someone else.
var ErrNotOwned = errors.New("openshell: sandbox name is in use by another owner")

// SandboxRequest describes the sandbox to create when none exists.
type SandboxRequest struct {
	// Image is the sandbox base image.
	Image string
	// Labels are set on creation. The LabelUserID label is also used to verify
	// ownership of an existing sandbox.
	Labels map[string]string
	// Annotations are set on creation.
	Annotations map[string]string
}

// Client is the narrow OpenShell surface Soft Serve needs. It is satisfied by
// the SDK-backed client returned from New, and in tests by one built on the
// SDK's fake sandbox client.
type Client interface {
	// EnsureSandbox returns a ready sandbox named name, creating it from req
	// when absent and starting it when stopped. created reports whether the
	// sandbox was created by this call. A half-made sandbox is deleted when
	// creation or the ready-wait fails.
	EnsureSandbox(ctx context.Context, name string, req SandboxRequest) (sb *Sandbox, created bool, err error)
	// GetSandbox returns the named sandbox. Absence is reported with an error
	// for which IsNotFound returns true.
	GetSandbox(ctx context.Context, name string) (*Sandbox, error)
	// ListSandboxes returns sandboxes carrying all the given labels.
	ListSandboxes(ctx context.Context, labels map[string]string) ([]*Sandbox, error)
	// DeleteSandbox deletes the named sandbox. An absent sandbox is not an
	// error.
	DeleteSandbox(ctx context.Context, name string) error
	// Run runs a command to completion in the sandbox.
	Run(ctx context.Context, name string, command []string, opts ExecOptions) (*ExecResult, error)
	// Interactive starts an interactive command with a PTY of cols x rows.
	Interactive(ctx context.Context, name string, command []string, cols, rows uint32, opts ExecOptions) (InteractiveSession, error)
	// Close releases the client's resources.
	Close() error
}

// IsNotFound reports whether err means the resource does not exist.
func IsNotFound(err error) bool { return v1.IsNotFound(err) }

// New returns an SDK-backed Client for the given configuration. The gRPC
// connection is established lazily on first use.
func New(cfg config.OpenShellConfig) (Client, error) {
	sdkCfg := v1.Config{Address: cfg.GatewayAddress}
	if cfg.Token != "" {
		sdkCfg.Auth = v1.StaticToken(cfg.Token)
	}
	if cfg.TLSCA != "" || cfg.TLSCert != "" || cfg.TLSKey != "" || cfg.Insecure {
		sdkCfg.TLS = &v1.TLSConfig{
			CAFile:   cfg.TLSCA,
			CertFile: cfg.TLSCert,
			KeyFile:  cfg.TLSKey,
			Insecure: cfg.Insecure,
		}
	}

	c, err := v1.NewClient(sdkCfg)
	if err != nil {
		return nil, fmt.Errorf("openshell: create client: %w", err)
	}

	return newSDKClient(cfg.Workspace, c.Sandboxes(), c.Exec(), c), nil
}

// sdkClient implements Client on top of the SDK sub-clients.
type sdkClient struct {
	workspace string
	sandboxes v1.SandboxInterface
	exec      v1.ExecInterface
	closer    io.Closer
}

var _ Client = (*sdkClient)(nil)

func newSDKClient(workspace string, sandboxes v1.SandboxInterface, exec v1.ExecInterface, closer io.Closer) *sdkClient {
	if workspace == "" {
		workspace = "default"
	}
	return &sdkClient{workspace: workspace, sandboxes: sandboxes, exec: exec, closer: closer}
}

func (c *sdkClient) EnsureSandbox(ctx context.Context, name string, req SandboxRequest) (*Sandbox, bool, error) {
	sb, err := c.sandboxes.Get(ctx, c.workspace, name)
	switch {
	case IsNotFound(err):
		return c.create(ctx, name, req)
	case err != nil:
		return nil, false, fmt.Errorf("openshell: get sandbox %q: %w", name, err)
	}

	return c.existing(ctx, sb, req)
}

// existing resumes a sandbox that already exists, after checking ownership.
func (c *sdkClient) existing(ctx context.Context, sb *Sandbox, req SandboxRequest) (*Sandbox, bool, error) {
	if !owns(sb, req.Labels) {
		return nil, false, ErrNotOwned
	}

	sb, err := c.resume(ctx, sb)
	return sb, false, err
}

// create creates the sandbox and waits for it, deleting it on failure.
func (c *sdkClient) create(ctx context.Context, name string, req SandboxRequest) (*Sandbox, bool, error) {
	spec := &v1.SandboxSpec{}
	if req.Image != "" {
		spec.Template = &v1.SandboxTemplate{Image: req.Image}
	}

	if _, err := c.sandboxes.Create(ctx, c.workspace, name, spec,
		v1.WithLabels(req.Labels),
		v1.WithAnnotations(req.Annotations),
	); err != nil {
		if v1.IsAlreadyExists(err) {
			// Lost a race with a concurrent start; resume the winner's
			// sandbox. Look it up once rather than retrying the whole ensure,
			// so an inconsistent gateway cannot make this loop.
			sb, gerr := c.sandboxes.Get(ctx, c.workspace, name)
			if gerr != nil {
				return nil, false, fmt.Errorf("openshell: create sandbox %q: already exists, but get failed: %w", name, gerr)
			}
			return c.existing(ctx, sb, req)
		}
		c.cleanup(name)
		return nil, false, fmt.Errorf("openshell: create sandbox %q: %w", name, err)
	}

	sb, err := c.waitReady(ctx, name)
	if err != nil {
		c.cleanup(name)
		return nil, false, err
	}

	return sb, true, nil
}

// resume brings an existing sandbox to the Ready phase.
func (c *sdkClient) resume(ctx context.Context, sb *Sandbox) (*Sandbox, error) {
	switch sb.Status.Phase {
	case v1.SandboxReady:
		return sb, nil
	case v1.SandboxStopped:
		if _, err := c.sandboxes.Start(ctx, c.workspace, sb.Name); err != nil {
			return nil, fmt.Errorf("openshell: start sandbox %q: %w", sb.Name, err)
		}
		return c.waitReady(ctx, sb.Name)
	case v1.SandboxProvisioning, v1.SandboxStarting, v1.SandboxUnknown, "":
		return c.waitReady(ctx, sb.Name)
	default:
		return nil, fmt.Errorf("openshell: sandbox %q is %s; end the session and start a new one", sb.Name, sb.Status.Phase)
	}
}

func (c *sdkClient) waitReady(ctx context.Context, name string) (*Sandbox, error) {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	sb, err := c.sandboxes.WaitReady(ctx, c.workspace, name)
	if err != nil {
		return nil, fmt.Errorf("openshell: wait for sandbox %q: %w", name, err)
	}
	return sb, nil
}

// cleanup deletes a half-made sandbox. It uses a fresh context so it still
// runs when the caller's context was cancelled.
func (c *sdkClient) cleanup(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_ = c.DeleteSandbox(ctx, name)
}

func (c *sdkClient) GetSandbox(ctx context.Context, name string) (*Sandbox, error) {
	return c.sandboxes.Get(ctx, c.workspace, name) //nolint:wrapcheck
}

func (c *sdkClient) ListSandboxes(ctx context.Context, labels map[string]string) ([]*Sandbox, error) {
	sbs, err := c.sandboxes.ListAll(ctx, c.workspace, v1.ListOptions{LabelSelector: labelSelector(labels)})
	if err != nil {
		return nil, fmt.Errorf("openshell: list sandboxes: %w", err)
	}

	// Filter client-side too: not every implementation honors the selector.
	out := make([]*Sandbox, 0, len(sbs))
	for _, sb := range sbs {
		if hasLabels(sb, labels) {
			out = append(out, sb)
		}
	}
	return out, nil
}

func (c *sdkClient) DeleteSandbox(ctx context.Context, name string) error {
	if _, err := c.sandboxes.Delete(ctx, c.workspace, name, v1.DeleteOptions{AllowMissing: true}); err != nil && !IsNotFound(err) {
		return fmt.Errorf("openshell: delete sandbox %q: %w", name, err)
	}
	return nil
}

func (c *sdkClient) Run(ctx context.Context, name string, command []string, opts ExecOptions) (*ExecResult, error) {
	return c.exec.Run(ctx, c.workspace, name, command, opts) //nolint:wrapcheck
}

func (c *sdkClient) Interactive(ctx context.Context, name string, command []string, cols, rows uint32, opts ExecOptions) (InteractiveSession, error) {
	return c.exec.Interactive(ctx, c.workspace, name, command, cols, rows, opts) //nolint:wrapcheck
}

func (c *sdkClient) Close() error {
	if c.closer == nil {
		return nil
	}
	return c.closer.Close() //nolint:wrapcheck
}

// owns reports whether sb carries the ownership label from want.
func owns(sb *Sandbox, want map[string]string) bool {
	id, ok := want[LabelUserID]
	if !ok {
		return true
	}
	return sb.Labels[LabelUserID] == id
}

func hasLabels(sb *Sandbox, labels map[string]string) bool {
	for k, v := range labels {
		if sb.Labels[k] != v {
			return false
		}
	}
	return true
}

func labelSelector(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	// Deterministic order for logging and tests.
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// maxNameLen is the maximum length of a DNS label.
const maxNameLen = 63

// hashSep joins a sandbox name and its hash suffix.
const hashSep = "--"

// SandboxName returns the deterministic sandbox name for a user's session on
// a repository: "soft-<user>-<repo>" sanitized to a DNS label of at most 63
// characters.
//
// A short hash of the raw (user, repo) pair is appended whenever the plain
// form could be ambiguous or lossy: when sanitizing changed either part
// (e.g. "a/b" and "a-b" would otherwise collide), when the username contains
// a hyphen (so "foo-bar"+"baz" and "foo"+"bar-baz" stay distinct), or when the
// name has to be truncated. Plain names are therefore unique per pair.
//
// The hash is joined with "--". Plain names never contain "--" (sanitizing
// collapses hyphen runs, and such inputs are hashed), so a crafted plain
// name can never equal another pair's hashed name.
func SandboxName(user, repo string) string {
	u, r := dnsClean(user), dnsClean(repo)
	parts := []string{"soft"}
	for _, p := range []string{u, r} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	name := strings.Join(parts, "-")

	if u == user && r == repo && u != "" && r != "" &&
		!strings.Contains(user, "-") && len(name) <= maxNameLen {
		return name
	}

	sum := sha256.Sum256([]byte(user + "\x00" + repo))
	suffix := hex.EncodeToString(sum[:])[:8]
	if max := maxNameLen - len(suffix) - len(hashSep); len(name) > max {
		name = strings.TrimRight(name[:max], "-")
	}
	return name + hashSep + suffix
}

// dnsClean lowercases s, maps characters outside [a-z0-9] to hyphens,
// collapses hyphen runs and trims leading and trailing hyphens.
func dnsClean(s string) string {
	var b strings.Builder
	lastHyphen := true // suppress leading hyphens
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastHyphen = false
			continue
		}
		if !lastHyphen {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}
