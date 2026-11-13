// Package openshell launches and resumes OpenShell agent sessions for Sundae
// repositories.
//
// Sundae stores no session state: sandbox names are derived from the
// user, repository and session name (see SessionSandboxName), the OpenShell
// gateway is the source of truth for which sessions exist, and each sandbox
// carries its user, repository, session name and profile as annotations.
// Inside the sandbox, an osewa session holds the session's program, so it
// outlives the SSH connection that started it. The server's Monitor keeps
// what it reads of each session's status, and the open bridges, in memory
// only. With S3 snapshots on (snapshots.s3), a session parked to its
// snapshot has no sandbox; a small marker object next to the snapshot is
// then its only record (see ParkedSnapshot).
package openshell

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/danielboothcloud/sundae/pkg/config"
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
	// LabelUserID is the sandbox label holding the owning Sundae user ID.
	LabelUserID = "sundae.user-id"
	// LabelManaged marks sandboxes created by Sundae.
	LabelManaged = "sundae.managed"
	// AnnotationUser holds the owning Sundae username.
	AnnotationUser = "sundae.user"
	// AnnotationRepo holds the first repository the session was created
	// with; AnnotationRepos holds them all.
	AnnotationRepo = "sundae.repo"
	// AnnotationSession holds the session name.
	AnnotationSession = "sundae.session"
	// AnnotationProfile holds the profile the session was created from;
	// empty when none applied.
	AnnotationProfile = "sundae.profile"
	// AnnotationProviders holds the OpenShell providers the profile listed
	// when the sandbox was created, comma-separated; absent for none. The
	// sandbox's Spec.Providers is what is attached now: start and attach
	// reconcile it with the profile's current list.
	AnnotationProviders = "sundae.providers"

	// DefaultSessionName is the session name used when none is given and
	// none can be derived from a repository.
	DefaultSessionName = "main"

	// suspendTimeout bounds how long a park waits for a sandbox to be
	// Suspended.
	suspendTimeout = 5 * time.Minute
	// cleanupTimeout bounds best-effort cleanup calls.
	cleanupTimeout = 30 * time.Second
)

// ErrReadyTimeout is returned when a sandbox does not become ready within
// openshell.ready_timeout. A first start pulls the image and seeds the
// workspace volume from the image's /sandbox; a large image or /sandbox
// can need a longer timeout.
var ErrReadyTimeout = errors.New("ready timeout")

// ErrNotOwned is returned when a sandbox with the expected name exists but
// belongs to someone else.
var ErrNotOwned = errors.New("openshell: sandbox name is in use by another owner")

// ErrSuspendUnsupported is returned by SuspendSandbox when the client has no
// way to suspend sandboxes. IsSuspendUnsupported also recognises the
// gateway's own refusals.
var ErrSuspendUnsupported = errors.New("openshell: sandbox suspend is not supported")

// resumeRestartIfRestoreFails is the RestartIfRestoreFails option every
// resume of a suspended sandbox uses. When the isolation backend cannot
// restore the sandbox's memory, true boots it cleanly instead of failing:
// the session stays usable with whatever the backend kept of /sandbox (the
// checkout, unpushed work), and Start finds the osewa session gone and starts
// the program again, telling the user. With false the session could only be
// ended, losing more than the clean restart does.
const resumeRestartIfRestoreFails = true

// IsSuspendUnsupported reports whether err means the gateway cannot suspend
// sandboxes: ErrSuspendUnsupported, an Unimplemented status, or the
// FailedPrecondition the OpenShell gateway (0.1.8, compute/mod.rs
// require_suspend_resume) returns when its compute driver lacks the
// suspend-resume capability, which the SDK reports as a Conflict.
func IsSuspendUnsupported(err error) bool {
	if errors.Is(err, ErrSuspendUnsupported) || v1.IsUnimplemented(err) {
		return true
	}
	var se *v1.StatusError
	if !errors.As(err, &se) || se.Code != v1.ErrorConflict {
		return false
	}
	msg := strings.ToLower(se.Message)
	return strings.Contains(msg, "not support") || strings.Contains(msg, "unsupported")
}

// SandboxRequest describes the sandbox to create when none exists.
type SandboxRequest struct {
	// Image is the sandbox base image.
	Image string
	// Labels are set on creation. The LabelUserID label is also used to verify
	// ownership of an existing sandbox.
	Labels map[string]string
	// Annotations are set on creation.
	Annotations map[string]string
	// Providers are the OpenShell providers attached to the sandbox on
	// creation. An existing sandbox's are left alone; see SyncProviders.
	Providers []string
	// Policy is the sandbox's policy on creation (see BuildSandboxPolicy);
	// nil sends none, so the image's /etc/openshell/policy.yaml or
	// OpenShell's default applies. An existing sandbox keeps the policy it
	// has. Read only.
	Policy *SandboxPolicy
}

// ProviderChanges is what SyncProviders changed.
type ProviderChanges struct {
	// Attached and Detached are the providers attached and detached.
	Attached, Detached []string
}

// Changed reports whether any provider was attached or detached.
func (c ProviderChanges) Changed() bool { return len(c.Attached)+len(c.Detached) > 0 }

// MissingProviderError is returned when the gateway refuses to attach a
// provider because it does not exist.
type MissingProviderError struct {
	// Provider is the provider's name.
	Provider string
	// Err is the gateway's error.
	Err error
}

func (e *MissingProviderError) Error() string {
	return fmt.Sprintf("openshell: provider %q does not exist on the OpenShell gateway: %v", e.Provider, e.Err)
}

func (e *MissingProviderError) Unwrap() error { return e.Err }

// missingProviderRe matches the gateway's refusal of a provider that does
// not exist, a FailedPrecondition the SDK reports as a Conflict
// (openshell-server 0.1.8 grpc/sandbox.rs, handle_create_sandbox and
// handle_attach_sandbox_provider).
var missingProviderRe = regexp.MustCompile(`^provider '(.+)' not found$`)

// asMissingProvider returns err as a *MissingProviderError when it is the
// gateway's refusal of a provider that does not exist, else err.
func asMissingProvider(err error) error {
	var se *v1.StatusError
	if !errors.As(err, &se) || se.Code != v1.ErrorConflict {
		return err
	}
	if m := missingProviderRe.FindStringSubmatch(se.Message); m != nil {
		return &MissingProviderError{Provider: m[1], Err: err}
	}
	return err
}

// grpcAborted is the gRPC code Aborted, which the gateway returns for a
// resource version conflict.
const grpcAborted = 10

// isVersionConflict reports whether err is the gateway's resource version
// conflict: a concurrent change it could not merge.
func isVersionConflict(err error) bool {
	var se *v1.StatusError
	if !errors.As(err, &se) || se.Code != v1.ErrorConflict {
		return false
	}
	return se.GRPCCode == grpcAborted || (se.ErrorInfo != nil && se.ErrorInfo.Reason == "RESOURCE_VERSION_CONFLICT")
}

// Client is the narrow OpenShell surface Sundae needs. It is satisfied by
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
	// ResumeSandbox returns the existing sandbox named name, ready, starting
	// it when stopped. It never creates one: absence is reported with an
	// error for which IsNotFound returns true. labels[LabelUserID], when set,
	// must match the sandbox's, or ErrNotOwned is returned.
	ResumeSandbox(ctx context.Context, name string, labels map[string]string) (*Sandbox, error)
	// DeleteSandbox deletes the named sandbox. An absent sandbox is not an
	// error.
	DeleteSandbox(ctx context.Context, name string) error
	// SyncProviders makes the OpenShell providers attached to sb exactly
	// want: it attaches the missing ones, in order, and detaches the
	// others. sb is the sandbox as last read; it is read again once when a
	// concurrent change conflicts. A provider the gateway says does not
	// exist stops it with a *MissingProviderError. What it changed before
	// an error is returned with it.
	SyncProviders(ctx context.Context, sb *Sandbox, want []string) (ProviderChanges, error)
	// SuspendSandbox parks the named sandbox: it suspends it, which keeps
	// its memory where the compute driver supports that, and waits until it
	// is Suspended. A gateway that cannot suspend returns an error for which
	// IsSuspendUnsupported returns true. EnsureSandbox and ResumeSandbox
	// resume a suspended sandbox.
	SuspendSandbox(ctx context.Context, name string) (*Sandbox, error)
	// Upload copies the local file at localPath to remotePath in the
	// sandbox, replacing it. Missing parent directories are created.
	Upload(ctx context.Context, name, localPath, remotePath string) error
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
// connection is established lazily on first use. A token_file is read here,
// on every call, so a rotated token takes effect for the next client.
func New(cfg config.OpenShellConfig) (Client, error) {
	c, err := newSDK(cfg)
	if err != nil {
		return nil, err
	}

	sc := newSDKClient(cfg.Workspace, c.Sandboxes(), c.Exec(), c.Files(), c)
	sc.suspender = c.SandboxSuspendResume()
	if cfg.ReadyTimeout > 0 {
		sc.readyTimeout = cfg.ReadyTimeout
	}
	return sc, nil
}

// NewProviderAPI returns the gateway's provider API for the given
// configuration and the closer that releases it. Like New, it reads a
// token_file on every call.
func NewProviderAPI(cfg config.OpenShellConfig) (ProviderAPI, io.Closer, error) {
	c, err := newSDK(cfg)
	if err != nil {
		return nil, nil, err
	}
	return c.Providers(), c, nil
}

// newSDK returns an SDK client for the given configuration. The gRPC
// connection is established lazily on first use.
func newSDK(cfg config.OpenShellConfig) (*v1.Client, error) {
	sdkCfg := v1.Config{Address: cfg.GatewayAddress}
	token, err := cfg.GatewayToken()
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	if token != "" {
		sdkCfg.Auth = v1.StaticToken(token)
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
	return c, nil
}

// sdkClient implements Client on top of the SDK sub-clients.
type sdkClient struct {
	workspace string
	sandboxes v1.SandboxInterface
	// suspender suspends and resumes sandboxes; nil when the sandbox
	// sub-client cannot.
	suspender v1.SandboxSuspendResumeInterface
	exec      v1.ExecInterface
	files     v1.FileInterface
	closer    io.Closer
	// readyTimeout bounds how long a start waits for a sandbox to become
	// ready (openshell.ready_timeout).
	readyTimeout time.Duration
}

var _ Client = (*sdkClient)(nil)

// newSDKClient builds the client. The sandbox sub-client also suspends and
// resumes when it implements v1.SandboxSuspendResumeInterface, as the SDK's
// and its fake's do.
func newSDKClient(workspace string, sandboxes v1.SandboxInterface, exec v1.ExecInterface, files v1.FileInterface, closer io.Closer) *sdkClient {
	if workspace == "" {
		workspace = "default"
	}
	sr, _ := sandboxes.(v1.SandboxSuspendResumeInterface)
	return &sdkClient{
		workspace: workspace, sandboxes: sandboxes, suspender: sr, exec: exec, files: files, closer: closer,
		readyTimeout: config.DefaultReadyTimeout,
	}
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
	spec := &v1.SandboxSpec{Providers: slices.Clone(req.Providers), Policy: req.Policy}
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
		return nil, false, fmt.Errorf("openshell: create sandbox %q: %w", name, asMissingProvider(err))
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
	case v1.SandboxSuspending:
		// Parked a moment ago: let the suspend finish, then resume.
		if _, err := c.waitSuspended(ctx, sb.Name); err != nil {
			return nil, err
		}
		return c.unsuspend(ctx, sb.Name)
	case v1.SandboxSuspended:
		return c.unsuspend(ctx, sb.Name)
	default:
		return nil, fmt.Errorf("openshell: sandbox %q is %s; end the session and start a new one", sb.Name, sb.Status.Phase)
	}
}

// unsuspend resumes a Suspended sandbox and waits until it is Ready.
func (c *sdkClient) unsuspend(ctx context.Context, name string) (*Sandbox, error) {
	if c.suspender == nil {
		return nil, fmt.Errorf("openshell: sandbox %q is suspended and cannot be resumed: %w", name, ErrSuspendUnsupported)
	}
	if _, err := c.suspender.Resume(ctx, c.workspace, name, v1.ResumeOptions{RestartIfRestoreFails: resumeRestartIfRestoreFails}); err != nil {
		return nil, fmt.Errorf("openshell: resume sandbox %q: %w", name, err)
	}
	return c.waitReady(ctx, name)
}

// waitSuspended waits until the sandbox is Suspended.
func (c *sdkClient) waitSuspended(ctx context.Context, name string) (*Sandbox, error) {
	if c.suspender == nil {
		return nil, fmt.Errorf("openshell: wait for sandbox %q to suspend: %w", name, ErrSuspendUnsupported)
	}
	ctx, cancel := context.WithTimeout(ctx, suspendTimeout)
	defer cancel()
	sb, err := c.suspender.WaitSuspended(ctx, c.workspace, name)
	if err != nil {
		return nil, fmt.Errorf("openshell: wait for sandbox %q to suspend: %w", name, err)
	}
	return sb, nil
}

func (c *sdkClient) SuspendSandbox(ctx context.Context, name string) (*Sandbox, error) {
	if c.suspender == nil {
		return nil, fmt.Errorf("openshell: suspend sandbox %q: %w", name, ErrSuspendUnsupported)
	}
	if _, err := c.suspender.Suspend(ctx, c.workspace, name); err != nil {
		return nil, fmt.Errorf("openshell: suspend sandbox %q: %w", name, err)
	}
	return c.waitSuspended(ctx, name)
}

func (c *sdkClient) waitReady(ctx context.Context, name string) (*Sandbox, error) {
	wctx, cancel := context.WithTimeout(ctx, c.readyTimeout)
	defer cancel()
	sb, err := c.sandboxes.WaitReady(wctx, c.workspace, name)
	if err != nil {
		if ctx.Err() == nil && errors.Is(wctx.Err(), context.DeadlineExceeded) {
			// Our bound, not the caller's: say which setting it is.
			return nil, fmt.Errorf("openshell: sandbox %q not ready after %s (a large image can need a longer openshell.ready_timeout): %w", name, c.readyTimeout, ErrReadyTimeout)
		}
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

func (c *sdkClient) ResumeSandbox(ctx context.Context, name string, labels map[string]string) (*Sandbox, error) {
	sb, err := c.sandboxes.Get(ctx, c.workspace, name)
	switch {
	case IsNotFound(err):
		return nil, err //nolint:wrapcheck
	case err != nil:
		return nil, fmt.Errorf("openshell: get sandbox %q: %w", name, err)
	case !owns(sb, labels):
		return nil, ErrNotOwned
	}
	return c.resume(ctx, sb)
}

func (c *sdkClient) DeleteSandbox(ctx context.Context, name string) error {
	if _, err := c.sandboxes.Delete(ctx, c.workspace, name, v1.DeleteOptions{AllowMissing: true}); err != nil && !IsNotFound(err) {
		return fmt.Errorf("openshell: delete sandbox %q: %w", name, err)
	}
	return nil
}

// SyncProviders passes expected resource version 0 to AttachProvider and
// DetachProvider: the gateway then applies the change to the sandbox as it
// stands, retrying its own compare-and-swap (openshell-server 0.1.8
// grpc/sandbox.rs, update_sandbox_cas), and both are idempotent, so a
// concurrent change to the providers is merged rather than refused. Should
// the gateway still report a resource version conflict, the sandbox is read
// again and the difference recomputed, once.
func (c *sdkClient) SyncProviders(ctx context.Context, sb *Sandbox, want []string) (ProviderChanges, error) {
	var changes ProviderChanges
	name := sb.Name
	for attempt := 0; ; attempt++ {
		err := c.syncProviders(ctx, sb, want, &changes)
		if err == nil || attempt > 0 || !isVersionConflict(err) {
			return changes, err
		}
		if sb, err = c.sandboxes.Get(ctx, c.workspace, name); err != nil {
			return changes, fmt.Errorf("openshell: get sandbox %q: %w", name, err)
		}
	}
}

func (c *sdkClient) syncProviders(ctx context.Context, sb *Sandbox, want []string, changes *ProviderChanges) error {
	have := sb.Spec.Providers
	for _, p := range want {
		if slices.Contains(have, p) {
			continue
		}
		r, err := c.sandboxes.AttachProvider(ctx, c.workspace, sb.Name, p, 0)
		if err != nil {
			return fmt.Errorf("openshell: attach provider %q to sandbox %q: %w", p, sb.Name, asMissingProvider(err))
		}
		if r.Attached {
			changes.Attached = append(changes.Attached, p)
		}
	}
	for _, p := range have {
		if slices.Contains(want, p) {
			continue
		}
		r, err := c.sandboxes.DetachProvider(ctx, c.workspace, sb.Name, p, 0)
		if err != nil {
			return fmt.Errorf("openshell: detach provider %q from sandbox %q: %w", p, sb.Name, err)
		}
		if r.Detached {
			changes.Detached = append(changes.Detached, p)
		}
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

// sandboxNamePrefix starts every sandbox name Sundae gives.
const sandboxNamePrefix = "sd-"

// SessionSandboxName returns the deterministic sandbox name of a user's
// named session: "sd-" and the first 16 hex digits of SHA-256 of the
// username and session name joined by a NUL byte. 19 characters, the most
// OpenShell 0.1.8 accepts for a sandbox name (MAX_ROUTABLE_NAME_LEN in
// crates/openshell-server/src/grpc/mod.rs, checked on create), and a valid
// DNS label. The session name and repositories are in the sandbox's
// annotations; an empty session name is DefaultSessionName.
func SessionSandboxName(user, session string) string {
	if session == "" {
		session = DefaultSessionName
	}
	sum := sha256.Sum256([]byte(user + "\x00" + session))
	return sandboxNamePrefix + hex.EncodeToString(sum[:])[:16]
}

// sessionNameRe matches a valid session name.
var sessionNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// ErrInvalidSessionName is returned for a session name ValidSessionName
// rejects.
var ErrInvalidSessionName = errors.New("session name must be 1 to 32 lowercase letters, digits and hyphens, starting and ending with a letter or digit")

// ValidSessionName reports whether name is a valid session name: 1 to 32
// lowercase letters, digits and hyphens, starting and ending with a letter or
// digit.
func ValidSessionName(name string) bool {
	return sessionNameRe.MatchString(name)
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
