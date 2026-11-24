// Package sandbox defines what Sundae needs from a session runtime: the
// place a session's sandbox runs (an OpenShell gateway, a Kubernetes pod, a
// Docker container). The session manager (pkg/openshell) drives sessions
// through the Runtime interface only; each runtime maps its own objects onto
// the types here.
package sandbox

import (
	"context"
	"errors"
	"io"
	"time"
)

// Phase is the lifecycle phase of a sandbox. The values are OpenShell's
// sandbox phases; other runtimes map their state onto them.
type Phase string

// Sandbox phases.
const (
	// Provisioning: the sandbox is being created.
	Provisioning Phase = "Provisioning"
	// Ready: the sandbox runs and accepts commands.
	Ready Phase = "Ready"
	// Error: the sandbox failed.
	Error Phase = "Error"
	// Deleting: the sandbox is being deleted.
	Deleting Phase = "Deleting"
	// Unknown: the runtime cannot tell.
	Unknown Phase = "Unknown"
	// Stopping: the sandbox is being stopped.
	Stopping Phase = "Stopping"
	// Stopped: the sandbox is stopped; starting it again keeps /sandbox.
	Stopped Phase = "Stopped"
	// Starting: the sandbox is being started.
	Starting Phase = "Starting"
	// Completed: the sandbox's main process exited.
	Completed Phase = "Completed"
	// Suspending: the sandbox is being suspended.
	Suspending Phase = "Suspending"
	// Suspended: the sandbox is stopped, its storage kept. Resuming it
	// keeps /sandbox, and its memory where the runtime can.
	Suspended Phase = "Suspended"
)

// Labels and annotations Sundae sets on every sandbox, in every runtime.
const (
	// LabelUserID is the sandbox label holding the owning Sundae user ID.
	LabelUserID = "sundae.user-id"
	// LabelManaged marks sandboxes created by Sundae.
	LabelManaged = "sundae.managed"
	// AnnotationRuntime holds the session runtime the sandbox was created
	// in; absent means OpenShell.
	AnnotationRuntime = "sundae.runtime"
	// AnnotationRuntimeClass holds the Kubernetes RuntimeClass the sandbox
	// was created with; absent for the default runtime.
	AnnotationRuntimeClass = "sundae.runtime-class"
)

// Owns reports whether a sandbox's labels carry the owner label of want;
// true when want has none.
func Owns(labels, want map[string]string) bool {
	id, ok := want[LabelUserID]
	if !ok {
		return true
	}
	return labels[LabelUserID] == id
}

// Sandbox is a session's sandbox as a runtime reports it.
type Sandbox struct {
	// Name is the sandbox name, unique across runtimes.
	Name string
	// Runtime is the runtime the sandbox runs in (config.Runtime*).
	Runtime string
	// Labels and Annotations are those the sandbox was created with.
	Labels      map[string]string
	Annotations map[string]string
	// Phase is the sandbox's lifecycle phase.
	Phase Phase
	// Image is the image the sandbox was created from; empty when the
	// runtime does not say.
	Image string
	// RuntimeClass is the Kubernetes RuntimeClass the sandbox runs with;
	// empty for the default runtime, or when it does not apply.
	RuntimeClass string
	// Providers are the OpenShell providers attached to the sandbox; nil
	// in runtimes without providers.
	Providers []string
	// CreatedAt is when the sandbox was created; zero when unknown.
	CreatedAt time.Time
}

// Request describes the sandbox to create when none exists.
type Request struct {
	// Runtime is the runtime to create the sandbox in (config.Runtime*).
	// An existing sandbox stays in the runtime it has.
	Runtime string
	// Profile is the sessions.profiles entry the session is created from;
	// empty for none. Runtimes read their per-profile settings from it.
	Profile string
	// Image is the sandbox image.
	Image string
	// RuntimeClass is the Kubernetes RuntimeClass of the sandbox; empty
	// for the default runtime. An existing sandbox keeps its own.
	RuntimeClass string
	// Labels are set on creation. The owner label is also used to verify
	// ownership of an existing sandbox.
	Labels map[string]string
	// Annotations are set on creation.
	Annotations map[string]string
	// Providers are the OpenShell providers attached on creation. Only the
	// OpenShell runtime has providers.
	Providers []string
}

// ExecOptions configures a command run in a sandbox.
type ExecOptions struct {
	// Env adds environment variables.
	Env map[string]string
	// WorkDir is the working directory; empty for the runtime's default.
	WorkDir string
	// NoLoginShell runs the command without sourcing the shell's login
	// files first.
	NoLoginShell bool
	// Stdin feeds Run's input, keeping secrets out of exec args and env;
	// runtimes without exec input refuse it.
	Stdin io.Reader
}

// ErrStdinUnsupported is returned by Run when ExecOptions.Stdin is set and
// the runtime cannot feed an exec's standard input.
var ErrStdinUnsupported = errors.New("sandbox exec standard input is not supported")

// ExecResult is the collected output of a completed command.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// InteractiveSession is a bidirectional, resizable command stream.
type InteractiveSession interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows uint32) error
	// ExitCode waits for the command to end and returns its exit code.
	ExitCode() (int, error)
	Close() error
}

// CloseInput ends the input of s while keeping its output, where s
// supports that (a CloseWrite method); else it does nothing.
func CloseInput(s InteractiveSession) error {
	if c, ok := s.(interface{ CloseWrite() error }); ok {
		return c.CloseWrite() //nolint:wrapcheck
	}
	return nil
}

// Cancel aborts s: its Cancel method where it has one, else Close.
func Cancel(s InteractiveSession) error {
	if c, ok := s.(interface{ Cancel() error }); ok {
		return c.Cancel() //nolint:wrapcheck
	}
	return s.Close() //nolint:wrapcheck
}

// Runtime is a session runtime: where sandboxes are created, run, suspended
// and deleted, and where commands run in them.
type Runtime interface {
	// EnsureSandbox returns a ready sandbox named name, creating it from
	// req when absent and starting or resuming it when stopped or suspended.
	// created reports whether this call created it. A half-made sandbox is
	// deleted when creation or the ready-wait fails.
	EnsureSandbox(ctx context.Context, name string, req Request) (sb *Sandbox, created bool, err error)
	// GetSandbox returns the named sandbox. Absence is reported with an
	// error wrapping ErrNotFound (or the runtime's own not-found error).
	GetSandbox(ctx context.Context, name string) (*Sandbox, error)
	// ListSandboxes returns the sandboxes carrying all the given labels.
	ListSandboxes(ctx context.Context, labels map[string]string) ([]*Sandbox, error)
	// ResumeSandbox returns the existing sandbox named name, ready,
	// starting or resuming it when stopped or suspended. It never creates
	// one. labels[owner label], when set, must match the sandbox's, or
	// ErrNotOwned is returned.
	ResumeSandbox(ctx context.Context, name string, labels map[string]string) (*Sandbox, error)
	// DeleteSandbox deletes the named sandbox and its storage. An absent
	// sandbox is not an error.
	DeleteSandbox(ctx context.Context, name string) error
	// SuspendSandbox stops the named sandbox and waits until it is
	// Suspended: /sandbox is kept, and its memory where the runtime can.
	// A runtime that cannot suspend returns an error wrapping
	// ErrSuspendUnsupported.
	SuspendSandbox(ctx context.Context, name string) (*Sandbox, error)
	// Upload copies the local file at localPath to remotePath in the
	// sandbox, replacing it. Missing parent directories are created.
	Upload(ctx context.Context, name, localPath, remotePath string) error
	// Run runs a command to completion in the sandbox.
	Run(ctx context.Context, name string, command []string, opts ExecOptions) (*ExecResult, error)
	// Interactive starts an interactive command with a PTY of cols x rows.
	Interactive(ctx context.Context, name string, command []string, cols, rows uint32, opts ExecOptions) (InteractiveSession, error)
	// Close releases the runtime's resources.
	Close() error
}

// ProviderChanges is what SyncProviders changed.
type ProviderChanges struct {
	// Attached and Detached are the providers attached and detached.
	Attached, Detached []string
}

// Changed reports whether any provider was attached or detached.
func (c ProviderChanges) Changed() bool { return len(c.Attached)+len(c.Detached) > 0 }

// ProviderSyncer is implemented by runtimes with OpenShell providers.
type ProviderSyncer interface {
	// SyncProviders makes the providers attached to sb exactly want. What
	// it changed before an error is returned with it.
	SyncProviders(ctx context.Context, sb *Sandbox, want []string) (ProviderChanges, error)
}

// Errors shared by runtimes.
var (
	// ErrNotFound is wrapped by errors for a sandbox that does not exist.
	ErrNotFound = errors.New("sandbox not found")
	// ErrNotOwned is returned when a sandbox with the expected name
	// exists but belongs to someone else.
	ErrNotOwned = errors.New("sandbox name is in use by another owner")
	// ErrReadyTimeout is returned when a sandbox does not become ready in
	// time.
	ErrReadyTimeout = errors.New("ready timeout")
	// ErrSuspendUnsupported is returned by SuspendSandbox when the
	// runtime cannot suspend sandboxes.
	ErrSuspendUnsupported = errors.New("sandbox suspend is not supported")
)
