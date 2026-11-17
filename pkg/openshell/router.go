package openshell

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/danielboothcloud/sundae/pkg/config"
	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
)

// NewClient returns the Client for the session runtimes cfg uses
// (config.RuntimesInUse): the OpenShell runtime alone when it is the only
// one, else a Router over all of them. The OpenShell runtime creates
// sandboxes with openshell.sandbox_policy's policy.
func NewClient(cfg *config.Config) (Client, error) {
	runtimes := cfg.RuntimesInUse()
	backends := make(map[string]sbx.Runtime, len(runtimes))
	closeAll := func() {
		for _, b := range backends {
			_ = b.Close()
		}
	}
	for _, r := range runtimes {
		switch r {
		case config.RuntimeOpenShell:
			c, err := newOpenShell(cfg.OpenShell)
			if err != nil {
				closeAll()
				return nil, err
			}
			c.policy, c.policyErr = BuildSandboxPolicy(cfg)
			backends[r] = c
		default:
			closeAll()
			return nil, fmt.Errorf("session runtime %q is not implemented", r)
		}
	}
	if c, ok := backends[config.RuntimeOpenShell].(Client); ok && len(backends) == 1 {
		return c, nil
	}
	return NewRouter(runtimes, backends), nil
}

// Router is a Client over several runtimes. A sandbox lives in exactly
// one: EnsureSandbox creates a new one in the runtime its request names,
// and every other call goes to the runtime that holds the named sandbox,
// found by asking each in turn and then remembered.
type Router struct {
	// order is the order runtimes are asked in.
	order    []string
	backends map[string]sbx.Runtime

	mu sync.Mutex
	// where maps a sandbox name to its runtime.
	where map[string]string
}

var _ Client = (*Router)(nil)

// NewRouter returns a Router over backends, keyed by runtime name, asking
// them in the given order; runtimes not in order are asked after, in name
// order.
func NewRouter(order []string, backends map[string]sbx.Runtime) *Router {
	r := &Router{backends: backends, where: map[string]string{}}
	for _, name := range order {
		if _, ok := backends[name]; ok && !slices.Contains(r.order, name) {
			r.order = append(r.order, name)
		}
	}
	var rest []string
	for name := range backends {
		if !slices.Contains(r.order, name) {
			rest = append(rest, name)
		}
	}
	slices.Sort(rest)
	r.order = append(r.order, rest...)
	return r
}

func (r *Router) remember(name, runtime string) {
	r.mu.Lock()
	r.where[name] = runtime
	r.mu.Unlock()
}

func (r *Router) forget(name string) {
	r.mu.Lock()
	delete(r.where, name)
	r.mu.Unlock()
}

// locate returns the sandbox named name and its runtime's name. A
// sandbox no runtime holds is reported with an error for which IsNotFound
// is true, unless a runtime failed to answer.
func (r *Router) locate(ctx context.Context, name string) (*Sandbox, string, error) {
	r.mu.Lock()
	known, ok := r.where[name]
	r.mu.Unlock()
	if ok {
		sb, err := r.backends[known].GetSandbox(ctx, name)
		if err == nil {
			return sb, known, nil
		}
		if !IsNotFound(err) {
			return nil, known, err
		}
		r.forget(name)
	}

	var errs []error
	for _, rt := range r.order {
		if rt == known {
			continue
		}
		sb, err := r.backends[rt].GetSandbox(ctx, name)
		switch {
		case err == nil:
			r.remember(name, rt)
			return sb, rt, nil
		case !IsNotFound(err):
			errs = append(errs, fmt.Errorf("%s runtime: %w", rt, err))
		}
	}
	if len(errs) > 0 {
		return nil, "", errors.Join(errs...)
	}
	return nil, "", fmt.Errorf("sandbox %q: %w", name, sbx.ErrNotFound)
}

// backend returns the runtime holding the sandbox named name: the one
// remembered for it, else the one locate finds.
func (r *Router) backend(ctx context.Context, name string) (sbx.Runtime, error) {
	r.mu.Lock()
	rt, ok := r.where[name]
	r.mu.Unlock()
	if ok {
		return r.backends[rt], nil
	}
	_, rt, err := r.locate(ctx, name)
	if err != nil {
		return nil, err
	}
	return r.backends[rt], nil
}

// EnsureSandbox creates the sandbox in req.Runtime (the first runtime
// when empty), unless another runtime holds it already: a session keeps
// the runtime it was created in.
func (r *Router) EnsureSandbox(ctx context.Context, name string, req SandboxRequest) (*Sandbox, bool, error) {
	_, rt, err := r.locate(ctx, name)
	switch {
	case err == nil:
	case IsNotFound(err):
		rt = req.Runtime
		if rt == "" {
			rt = r.order[0]
		}
	default:
		return nil, false, err
	}
	b, ok := r.backends[rt]
	if !ok {
		return nil, false, fmt.Errorf("session runtime %q is not configured", rt)
	}
	sb, created, err := b.EnsureSandbox(ctx, name, req)
	if err == nil {
		r.remember(name, rt)
	}
	return sb, created, err //nolint:wrapcheck
}

func (r *Router) GetSandbox(ctx context.Context, name string) (*Sandbox, error) {
	sb, _, err := r.locate(ctx, name)
	return sb, err
}

// ListSandboxes lists the sandboxes of every runtime. A runtime that
// fails to list fails the whole listing: a partial one would look like
// sessions had ended.
func (r *Router) ListSandboxes(ctx context.Context, labels map[string]string) ([]*Sandbox, error) {
	var out []*Sandbox
	for _, rt := range r.order {
		sbs, err := r.backends[rt].ListSandboxes(ctx, labels)
		if err != nil {
			return nil, fmt.Errorf("%s runtime: %w", rt, err)
		}
		for _, sb := range sbs {
			r.remember(sb.Name, rt)
		}
		out = append(out, sbs...)
	}
	return out, nil
}

func (r *Router) ResumeSandbox(ctx context.Context, name string, labels map[string]string) (*Sandbox, error) {
	b, err := r.backend(ctx, name)
	if err != nil {
		return nil, err
	}
	return b.ResumeSandbox(ctx, name, labels) //nolint:wrapcheck
}

func (r *Router) DeleteSandbox(ctx context.Context, name string) error {
	b, err := r.backend(ctx, name)
	switch {
	case IsNotFound(err):
		return nil
	case err != nil:
		return err
	}
	if err := b.DeleteSandbox(ctx, name); err != nil {
		return err //nolint:wrapcheck
	}
	r.forget(name)
	return nil
}

func (r *Router) SuspendSandbox(ctx context.Context, name string) (*Sandbox, error) {
	b, err := r.backend(ctx, name)
	if err != nil {
		return nil, err
	}
	return b.SuspendSandbox(ctx, name) //nolint:wrapcheck
}

// SyncProviders syncs the providers of a sandbox in a runtime with
// providers. In any other runtime there is nothing to sync, and wanting
// providers there is an error.
func (r *Router) SyncProviders(ctx context.Context, sb *Sandbox, want []string) (ProviderChanges, error) {
	b, err := r.backend(ctx, sb.Name)
	if err != nil {
		return ProviderChanges{}, err
	}
	if ps, ok := b.(sbx.ProviderSyncer); ok {
		return ps.SyncProviders(ctx, sb, want) //nolint:wrapcheck
	}
	if len(want) > 0 {
		return ProviderChanges{}, fmt.Errorf("sandbox %q runs in the %s runtime, which has no OpenShell providers", sb.Name, SandboxRuntime(sb))
	}
	return ProviderChanges{}, nil
}

func (r *Router) Upload(ctx context.Context, name, localPath, remotePath string) error {
	b, err := r.backend(ctx, name)
	if err != nil {
		return err
	}
	return b.Upload(ctx, name, localPath, remotePath) //nolint:wrapcheck
}

func (r *Router) Run(ctx context.Context, name string, command []string, opts ExecOptions) (*ExecResult, error) {
	b, err := r.backend(ctx, name)
	if err != nil {
		return nil, err
	}
	return b.Run(ctx, name, command, opts) //nolint:wrapcheck
}

func (r *Router) Interactive(ctx context.Context, name string, command []string, cols, rows uint32, opts ExecOptions) (InteractiveSession, error) {
	b, err := r.backend(ctx, name)
	if err != nil {
		return nil, err
	}
	return b.Interactive(ctx, name, command, cols, rows, opts) //nolint:wrapcheck
}

// Close closes every runtime.
func (r *Router) Close() error {
	var errs []error
	for _, rt := range r.order {
		if err := r.backends[rt].Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
