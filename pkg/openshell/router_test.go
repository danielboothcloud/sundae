package openshell

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/config"
	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
)

// mapRuntime is an in-memory sbx.Runtime without providers.
type mapRuntime struct {
	name string
	mu   sync.Mutex
	sbs  map[string]*Sandbox
	runs []string
	// getErr fails every GetSandbox.
	getErr error
}

var _ sbx.Runtime = (*mapRuntime)(nil)

func newMapRuntime(name string) *mapRuntime {
	return &mapRuntime{name: name, sbs: map[string]*Sandbox{}}
}

func (r *mapRuntime) EnsureSandbox(_ context.Context, name string, req SandboxRequest) (*Sandbox, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sb, ok := r.sbs[name]; ok {
		sb.Phase = sbx.Ready
		return sb, false, nil
	}
	sb := &Sandbox{Name: name, Runtime: r.name, Labels: req.Labels, Annotations: req.Annotations, Phase: sbx.Ready, Image: req.Image}
	r.sbs[name] = sb
	return sb, true, nil
}

func (r *mapRuntime) GetSandbox(_ context.Context, name string) (*Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	sb, ok := r.sbs[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, sbx.ErrNotFound)
	}
	return sb, nil
}

func (r *mapRuntime) ListSandboxes(_ context.Context, labels map[string]string) ([]*Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Sandbox
	for _, sb := range r.sbs {
		if hasLabels(sb.Labels, labels) {
			out = append(out, sb)
		}
	}
	return out, nil
}

func (r *mapRuntime) ResumeSandbox(ctx context.Context, name string, _ map[string]string) (*Sandbox, error) {
	return r.GetSandbox(ctx, name)
}

func (r *mapRuntime) DeleteSandbox(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sbs, name)
	return nil
}

func (r *mapRuntime) SuspendSandbox(_ context.Context, name string) (*Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb, ok := r.sbs[name]
	if !ok {
		return nil, sbx.ErrNotFound
	}
	sb.Phase = sbx.Suspended
	return sb, nil
}

func (r *mapRuntime) Upload(context.Context, string, string, string) error { return nil }

func (r *mapRuntime) Run(_ context.Context, name string, command []string, _ ExecOptions) (*ExecResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, name+": "+command[0])
	return &ExecResult{}, nil
}

func (r *mapRuntime) Interactive(context.Context, string, []string, uint32, uint32, ExecOptions) (InteractiveSession, error) {
	return nil, errors.New("not supported")
}

func (r *mapRuntime) Close() error { return nil }

// syncRuntime is a mapRuntime with providers.
type syncRuntime struct {
	*mapRuntime
	synced []string
}

func (r *syncRuntime) SyncProviders(_ context.Context, sb *Sandbox, want []string) (ProviderChanges, error) {
	r.synced = append(r.synced, sb.Name)
	return ProviderChanges{Attached: want}, nil
}

func TestRouter(t *testing.T) {
	ctx := context.Background()
	osh := &syncRuntime{mapRuntime: newMapRuntime(config.RuntimeOpenShell)}
	pod := newMapRuntime(config.RuntimePod)
	r := NewRouter([]string{config.RuntimeOpenShell}, map[string]sbx.Runtime{
		config.RuntimeOpenShell: osh, config.RuntimePod: pod,
	})
	if !slices.Equal(r.order, []string{config.RuntimeOpenShell, config.RuntimePod}) {
		t.Fatalf("order = %v", r.order)
	}
	labels := map[string]string{LabelManaged: "true"}

	// Created where the request says; an empty runtime is the first.
	if _, created, err := r.EnsureSandbox(ctx, "sd-a", SandboxRequest{Runtime: config.RuntimePod, Labels: labels}); err != nil || !created {
		t.Fatalf("pod: created=%v err=%v", created, err)
	}
	if _, created, err := r.EnsureSandbox(ctx, "sd-b", SandboxRequest{Labels: labels}); err != nil || !created {
		t.Fatalf("default: created=%v err=%v", created, err)
	}
	if _, ok := pod.sbs["sd-a"]; !ok {
		t.Fatal("sd-a not in the pod runtime")
	}
	if _, ok := osh.sbs["sd-b"]; !ok {
		t.Fatal("sd-b not in the openshell runtime")
	}

	// An existing sandbox stays in its runtime, whatever the request says.
	if _, created, err := r.EnsureSandbox(ctx, "sd-a", SandboxRequest{Runtime: config.RuntimeOpenShell}); err != nil || created {
		t.Fatalf("existing: created=%v err=%v", created, err)
	}
	if _, ok := osh.sbs["sd-a"]; ok {
		t.Fatal("sd-a created again in the openshell runtime")
	}

	// A fresh router finds sandboxes by asking each runtime.
	r = NewRouter([]string{config.RuntimeOpenShell, config.RuntimePod}, map[string]sbx.Runtime{
		config.RuntimeOpenShell: osh, config.RuntimePod: pod,
	})
	if sb, err := r.GetSandbox(ctx, "sd-a"); err != nil || sb.Runtime != config.RuntimePod {
		t.Fatalf("get: sb=%+v err=%v", sb, err)
	}
	if _, err := r.Run(ctx, "sd-a", []string{"true"}, ExecOptions{}); err != nil || !slices.Equal(pod.runs, []string{"sd-a: true"}) {
		t.Fatalf("run: err=%v runs=%v", err, pod.runs)
	}
	if _, err := r.GetSandbox(ctx, "sd-none"); !IsNotFound(err) {
		t.Fatalf("absent: err=%v", err)
	}
	if err := r.DeleteSandbox(ctx, "sd-none"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}

	// Listing covers every runtime.
	sbs, err := r.ListSandboxes(ctx, labels)
	if err != nil || len(sbs) != 2 {
		t.Fatalf("list: %d sandboxes, err=%v", len(sbs), err)
	}

	// Providers: synced where the runtime has them; elsewhere none can
	// be wanted.
	if c, err := r.SyncProviders(ctx, osh.sbs["sd-b"], []string{"p"}); err != nil || !slices.Equal(c.Attached, []string{"p"}) {
		t.Fatalf("sync openshell: %+v %v", c, err)
	}
	if c, err := r.SyncProviders(ctx, pod.sbs["sd-a"], nil); err != nil || c.Changed() {
		t.Fatalf("sync pod, none: %+v %v", c, err)
	}
	if _, err := r.SyncProviders(ctx, pod.sbs["sd-a"], []string{"p"}); err == nil {
		t.Fatal("sync pod with providers: no error")
	}

	// Suspending goes to the sandbox's runtime.
	if sb, err := r.SuspendSandbox(ctx, "sd-a"); err != nil || sb.Phase != sbx.Suspended {
		t.Fatalf("suspend: %+v %v", sb, err)
	}

	// A runtime that cannot answer is reported, not taken for absence.
	broken := errors.New("gateway down")
	osh.getErr = broken
	r = NewRouter([]string{config.RuntimeOpenShell, config.RuntimePod}, map[string]sbx.Runtime{
		config.RuntimeOpenShell: osh, config.RuntimePod: pod,
	})
	if _, err := r.GetSandbox(ctx, "sd-new"); !errors.Is(err, broken) || IsNotFound(err) {
		t.Fatalf("broken runtime: err=%v", err)
	}
	if _, _, err := r.EnsureSandbox(ctx, "sd-new", SandboxRequest{Runtime: config.RuntimePod}); !errors.Is(err, broken) {
		t.Fatalf("ensure with a broken runtime: err=%v", err)
	}
	// A sandbox found elsewhere still works.
	if sb, err := r.GetSandbox(ctx, "sd-a"); err != nil || sb.Runtime != config.RuntimePod {
		t.Fatalf("get with a broken runtime: %+v %v", sb, err)
	}
}

func TestNewClientOpenShellOnly(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.OpenShell.GatewayAddress = "localhost:1"
	cfg.OpenShell.CloneURL = "http://sundae:23232"
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck
	sc, ok := c.(*sdkClient)
	if !ok {
		t.Fatalf("client = %T, want the OpenShell runtime itself", c)
	}
	if sc.policy == nil || sc.policyErr != nil {
		t.Fatalf("policy = %+v, err = %v", sc.policy, sc.policyErr)
	}
}
