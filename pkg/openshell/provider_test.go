package openshell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/fake"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	"github.com/danielboothcloud/sundae/pkg/config"
)

// providerGateway adds to the SDK's fake sandbox client what the OpenShell
// 0.1.8 gateway does with providers and the fake does not: it refuses a
// provider that does not exist, on create and attach, with the
// FailedPrecondition the SDK reports as a Conflict (grpc/sandbox.rs). It
// records attach and detach calls, and can fail them.
type providerGateway struct {
	v1.SandboxInterface
	mu sync.Mutex
	// known are the providers that exist on the gateway.
	known map[string]bool
	// calls are the attach and detach calls, as "attach NAME@VERSION".
	calls []string
	// failNext fails the next attach or detach with it.
	failNext error
	// conflicts is how many attaches or detaches fail with a resource
	// version conflict, after a concurrent attach of concurrent (when
	// set).
	conflicts  int
	concurrent string
}

func missingProvider(name string) error {
	return &types.StatusError{Code: types.ErrorConflict, GRPCCode: 9, Message: fmt.Sprintf("provider '%s' not found", name)}
}

func (g *providerGateway) Create(ctx context.Context, workspace, name string, spec *types.SandboxSpec, opts ...types.CreateOption) (*types.Sandbox, error) {
	g.mu.Lock()
	for _, p := range spec.Providers {
		if !g.known[p] {
			g.mu.Unlock()
			return nil, missingProvider(p)
		}
	}
	g.mu.Unlock()
	return g.SandboxInterface.Create(ctx, workspace, name, spec, opts...)
}

// mutate records a call and returns the error it fails with, if any.
func (g *providerGateway) mutate(ctx context.Context, workspace, sandbox, verb, provider string, version uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, fmt.Sprintf("%s %s@%d", verb, provider, version))
	if err := g.failNext; err != nil {
		g.failNext = nil
		return err
	}
	if g.conflicts > 0 {
		g.conflicts--
		if g.concurrent != "" {
			if _, err := g.SandboxInterface.AttachProvider(ctx, workspace, sandbox, g.concurrent, 0); err != nil {
				return err
			}
		}
		return &types.StatusError{
			Code: types.ErrorConflict, GRPCCode: grpcAborted,
			Message:   "attach sandbox provider failed due to concurrent modification",
			ErrorInfo: &types.ErrorInfo{Reason: "RESOURCE_VERSION_CONFLICT"},
		}
	}
	if verb == "attach" && !g.known[provider] {
		return missingProvider(provider)
	}
	return nil
}

func (g *providerGateway) AttachProvider(ctx context.Context, workspace, sandbox, provider string, version uint64) (*types.AttachProviderResult, error) {
	if err := g.mutate(ctx, workspace, sandbox, "attach", provider, version); err != nil {
		return nil, err
	}
	return g.SandboxInterface.AttachProvider(ctx, workspace, sandbox, provider, version)
}

func (g *providerGateway) DetachProvider(ctx context.Context, workspace, sandbox, provider string, version uint64) (*types.DetachProviderResult, error) {
	if err := g.mutate(ctx, workspace, sandbox, "detach", provider, version); err != nil {
		return nil, err
	}
	return g.SandboxInterface.DetachProvider(ctx, workspace, sandbox, provider, version)
}

func (g *providerGateway) takeCalls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.calls
	g.calls = nil
	return c
}

func newProviderHarness(t *testing.T, providers ...string) (*harness, *providerGateway) {
	t.Helper()
	cfg := newHarnessConfig()
	cfg.Sessions.Profiles = map[string]config.SessionProfile{
		"agent": {Command: "claude", Providers: providers},
		"shell": {},
	}
	fc := fake.NewClient()
	t.Cleanup(func() { _ = fc.Close() })
	gw := &providerGateway{SandboxInterface: fc.Sandboxes(), known: map[string]bool{}}
	for _, p := range []string{"my-claude", "my-github", "my-pypi"} {
		gw.known[p] = true
	}
	ex := newFakeExec()
	tokens := newFakeTokens()
	mgr := NewManager(newSDKClient("default", gw, ex, fc.Files(), nil), tokens, cfg)
	return &harness{fc: fc, exec: ex, tokens: tokens, mgr: mgr}, gw
}

func (h *harness) sandbox(t *testing.T, name string) *Sandbox {
	t.Helper()
	sb, err := h.fc.Sandboxes().Get(context.Background(), "default", name)
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

// A profile's providers are attached when its session's sandbox is
// created, recorded in an annotation, and the program started with the
// OpenShell exec environment. Sundae's own exec environment carries no
// credential: the supervisor adds the placeholders.
func TestStartAttachesProfileProviders(t *testing.T) {
	ctx := context.Background()
	h, gw := newProviderHarness(t, "my-claude", "my-github")

	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Profile: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Providers, []string{"my-claude", "my-github"}) || s.ProviderChanges.Changed() {
		t.Fatalf("session = %+v", s)
	}
	sb := h.sandbox(t, s.Sandbox)
	if !slices.Equal(sb.Spec.Providers, []string{"my-claude", "my-github"}) || sb.Annotations[AnnotationProviders] != "my-claude,my-github" {
		t.Fatalf("providers = %v, annotations = %v", sb.Spec.Providers, sb.Annotations)
	}
	if calls := gw.takeCalls(); len(calls) != 0 {
		t.Fatalf("a new sandbox needs no attach: %v", calls)
	}

	call := h.exec.last(s.Sandbox, osewaNewName)
	b := h.exec.boxes[s.Sandbox]
	if b.newEnvFile != osewaSessionEnvFile || strings.Join(b.newArgv, " ") != "/sandbox/r claude" {
		t.Fatalf("osewa new: env file %q, argv %q", b.newEnvFile, b.newArgv)
	}
	want := []string{"/usr/local/bin/osewa", osewaSessionFile, "", "/sandbox/r", osewaSessionEnvFile, "claude"}
	if !slices.Equal(call.args(), want) {
		t.Fatalf("osewa new args = %q, want %q", call.args(), want)
	}
	if !maps.Equal(call.opts.Env, h.mgr.osewaClientEnv()) {
		t.Fatalf("osewa new env = %v", call.opts.Env)
	}

	// Without providers nothing is staged, and no annotation is written.
	s, err = h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Name: "plain", Profile: "shell"})
	if err != nil {
		t.Fatal(err)
	}
	sb = h.sandbox(t, s.Sandbox)
	if len(sb.Spec.Providers) != 0 || sb.Annotations[AnnotationProviders] != "" || len(s.Providers) != 0 {
		t.Fatalf("plain: providers = %v, annotations = %v", sb.Spec.Providers, sb.Annotations)
	}
	if args := h.exec.last(s.Sandbox, osewaNewName).args(); args[4] != "" {
		t.Fatalf("plain: osewa new args = %q", args)
	}

	// `session ls` lists what is attached.
	infos, err := h.mgr.List(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, i := range infos {
		got[i.Name] = i.Providers
	}
	if !slices.Equal(got["main"], []string{"my-claude", "my-github"}) || got["plain"] != nil {
		t.Fatalf("list providers = %v", got)
	}
}

// A provider that does not exist fails the start, naming it and the
// profile key; no sandbox is left behind.
func TestStartMissingProvider(t *testing.T) {
	ctx := context.Background()
	h, _ := newProviderHarness(t, "my-claude", "nope")

	_, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Profile: "agent"})
	var missing *MissingProviderError
	if !errors.As(err, &missing) || missing.Provider != "nope" ||
		!strings.Contains(err.Error(), `sessions.profiles.agent.providers lists OpenShell provider "nope", which does not exist on the gateway`) {
		t.Fatalf("err = %v", err)
	}
	if _, err := h.fc.Sandboxes().Get(ctx, "default", SessionSandboxName("alice", "main")); !IsNotFound(err) {
		t.Fatalf("sandbox left behind: %v", err)
	}
}

// On start and attach, an existing sandbox's providers are made to match
// its profile's: missing ones attached, others detached.
func TestReconcileProviders(t *testing.T) {
	ctx := context.Background()
	h, gw := newProviderHarness(t, "my-claude", "my-github")
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Profile: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	setProviders := func(ps ...string) {
		p := h.mgr.sessions.Profiles["agent"]
		p.Providers = ps
		h.mgr.sessions.Profiles["agent"] = p
	}
	resume := func(op string) (*Session, string, error) {
		var progress bytes.Buffer
		opts := StartOptions{User: alice, Repo: "r", Progress: &progress}
		var s *Session
		var err error
		if op == "start" {
			s, err = h.mgr.Start(ctx, opts)
		} else {
			s, err = h.mgr.Resume(ctx, opts)
		}
		return s, progress.String(), err
	}
	attached := func() []string { return h.sandbox(t, s.Sandbox).Spec.Providers }

	// Unchanged: nothing to do, not even a read.
	if _, progress, err := resume("attach"); err != nil || strings.Contains(progress, "provider") {
		t.Fatalf("unchanged: err=%v progress=%q", err, progress)
	}
	if calls := gw.takeCalls(); len(calls) != 0 {
		t.Fatalf("unchanged: calls = %v", calls)
	}

	// Added and removed. Expected resource version 0: the gateway applies
	// each change to the sandbox as it stands.
	setProviders("my-github", "my-pypi")
	s2, progress, err := resume("attach")
	if err != nil {
		t.Fatal(err)
	}
	if calls := gw.takeCalls(); !slices.Equal(calls, []string{"attach my-pypi@0", "detach my-claude@0"}) {
		t.Fatalf("calls = %v", calls)
	}
	if !slices.Equal(attached(), []string{"my-github", "my-pypi"}) ||
		!slices.Equal(s2.ProviderChanges.Attached, []string{"my-pypi"}) || !slices.Equal(s2.ProviderChanges.Detached, []string{"my-claude"}) {
		t.Fatalf("attached = %v, changes = %+v", attached(), s2.ProviderChanges)
	}
	for _, want := range []string{
		`Attached OpenShell provider(s) my-pypi to session main (profile "agent")`,
		`Detached OpenShell provider(s) my-claude from session main: profile "agent" does not list them`,
		// The program was running: it keeps its environment.
		"The running claude keeps the provider credentials it started with",
	} {
		if !strings.Contains(progress, want) {
			t.Fatalf("progress = %q, want %q", progress, want)
		}
	}
	// The running program was not restarted, nor given a new environment.
	if s2.Started || h.exec.count(s.Sandbox, osewaNewName) != 1 {
		t.Fatal("the program was restarted")
	}

	// A provider that does not exist fails attach and start: a config
	// error. What could be done is done.
	setProviders("my-github", "my-pypi", "nope")
	for _, op := range []string{"attach", "start"} {
		_, _, err = resume(op)
		var missing *MissingProviderError
		if !errors.As(err, &missing) || missing.Provider != "nope" ||
			!strings.Contains(err.Error(), `sessions.profiles.agent.providers lists OpenShell provider "nope"`) {
			t.Fatalf("%s: err = %v", op, err)
		}
	}
	gw.takeCalls()

	// Any other failure is a warning: the session is attached with the
	// providers it has.
	setProviders("my-github")
	gw.failNext = &types.StatusError{Code: types.ErrorUnavailable, Message: "gateway restarting"}
	s3, progress, err := resume("attach")
	if err != nil || s3 == nil {
		t.Fatalf("transient: err = %v", err)
	}
	if !strings.Contains(progress, `Warning: could not make the OpenShell providers of session main match profile "agent"`) ||
		!strings.Contains(progress, "gateway restarting") || !strings.Contains(progress, "it keeps the providers it has") {
		t.Fatalf("transient: progress = %q", progress)
	}
	if !slices.Equal(attached(), []string{"my-github", "my-pypi"}) {
		t.Fatalf("transient: attached = %v", attached())
	}
	gw.takeCalls()

	// The next attach catches up.
	if _, _, err := resume("attach"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(attached(), []string{"my-github"}) {
		t.Fatalf("after: attached = %v", attached())
	}
	gw.takeCalls()

	// A resource version conflict reads the sandbox again, once, and
	// recomputes: a provider attached concurrently is detached too.
	setProviders("my-github", "my-claude")
	gw.conflicts, gw.concurrent = 1, "my-pypi"
	if _, _, err := resume("attach"); err != nil {
		t.Fatal(err)
	}
	if calls := gw.takeCalls(); !slices.Equal(calls, []string{"attach my-claude@0", "attach my-claude@0", "detach my-pypi@0"}) {
		t.Fatalf("conflict: calls = %v", calls)
	}
	if !slices.Equal(attached(), []string{"my-github", "my-claude"}) {
		t.Fatalf("conflict: attached = %v", attached())
	}
	// Twice is a warning.
	setProviders("my-github")
	gw.conflicts, gw.concurrent = 2, ""
	if _, progress, err := resume("attach"); err != nil || !strings.Contains(progress, "concurrent modification") {
		t.Fatalf("conflicts: err=%v progress=%q", err, progress)
	}
	if calls := gw.takeCalls(); !slices.Equal(calls, []string{"detach my-claude@0", "detach my-claude@0"}) {
		t.Fatalf("conflicts: calls = %v", calls)
	}
}

// A session without a profile has no providers: any attached by other
// means are detached. One whose profile is no longer configured keeps
// what it has, and its program still gets their placeholders.
func TestReconcileProvidersWithoutProfile(t *testing.T) {
	ctx := context.Background()
	h, gw := newProviderHarness(t, "my-claude")

	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.fc.Sandboxes().AttachProvider(ctx, "default", s.Sandbox, "my-github", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	if calls := gw.takeCalls(); !slices.Equal(calls, []string{"detach my-github@0"}) {
		t.Fatalf("calls = %v", calls)
	}

	s, err = h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Name: "gone", Profile: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	delete(h.mgr.sessions.Profiles, "agent")
	// The program exits, so attach starts it again.
	h.exec.boxes[s.Sandbox].sessions[s.OsewaID] = false
	var progress bytes.Buffer
	s, err = h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r", Name: "gone", Progress: &progress})
	if err != nil || !s.Started {
		t.Fatalf("gone: s=%+v err=%v", s, err)
	}
	if calls := gw.takeCalls(); len(calls) != 0 {
		t.Fatalf("gone: calls = %v", calls)
	}
	if !slices.Equal(s.Providers, []string{"my-claude"}) || h.exec.boxes[s.Sandbox].newEnvFile != osewaSessionEnvFile {
		t.Fatalf("gone: providers = %v, env file = %q", s.Providers, h.exec.boxes[s.Sandbox].newEnvFile)
	}
	if !strings.Contains(progress.String(), `profile "agent" is no longer configured`) {
		t.Fatalf("gone: progress = %q", progress.String())
	}
}

// A program started right after a provider was attached may start before
// the sandbox applies the change; the user is told what to do about it.
func TestReconcileProvidersRestartedProgram(t *testing.T) {
	ctx := context.Background()
	h, _ := newProviderHarness(t, "my-claude")
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Profile: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	p := h.mgr.sessions.Profiles["agent"]
	p.Providers = []string{"my-claude", "my-github"}
	h.mgr.sessions.Profiles["agent"] = p
	h.exec.boxes[s.Sandbox].sessions[s.OsewaID] = false

	var progress bytes.Buffer
	s, err = h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r", Progress: &progress})
	if err != nil || !s.Started {
		t.Fatalf("s=%+v err=%v", s, err)
	}
	if !strings.Contains(progress.String(), "If claude lacks the credentials of a provider attached just now, exit it and attach again") {
		t.Fatalf("progress = %q", progress.String())
	}
	if h.exec.boxes[s.Sandbox].newEnvFile != osewaSessionEnvFile {
		t.Fatal("no environment staged")
	}
}
