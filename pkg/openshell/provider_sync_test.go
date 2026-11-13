package openshell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/fake"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/events"
)

// providerStore adds to the SDK's fake provider client what the OpenShell
// 0.1.8 gateway does and the fake does not (grpc/provider.rs):
//   - get and list return no credentials (the gateway redacts the values
//     and the SDK drops the map);
//   - create requires a type, and credentials unless the type needs none;
//   - update merges credentials and config into what is stored, deleting
//     the keys updated to "", keeps the labels and refuses a type change;
//   - delete refuses a provider attached to a sandbox.
//
// It records the mutating calls, and can fail them by provider name.
type providerStore struct {
	inner v1.ProviderInterface
	mu    sync.Mutex
	calls []string
	// fail fails every call for the provider with the error.
	fail map[string]error
	// attached are providers attached to a sandbox.
	attached map[string]bool
}

func newProviderStore(t *testing.T) *providerStore {
	t.Helper()
	fc := fake.NewClient()
	t.Cleanup(func() { _ = fc.Close() })
	return &providerStore{inner: fc.Providers(), fail: map[string]error{}, attached: map[string]bool{}}
}

// typesWithoutCredentials are provider types that need no credentials.
var typesWithoutCredentials = map[string]bool{"pypi": true}

func (s *providerStore) record(verb, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, verb+" "+name)
	return s.fail[name]
}

func redacted(p *v1.Provider) *v1.Provider {
	if p != nil {
		p.Spec.Credentials = nil
	}
	return p
}

func (s *providerStore) Get(ctx context.Context, ws, name string) (*v1.Provider, error) {
	s.mu.Lock()
	err := s.fail[name]
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	p, err := s.inner.Get(ctx, ws, name)
	return redacted(p), err
}

func (s *providerStore) ListAll(ctx context.Context, ws string, opts ...v1.ListOptions) ([]*v1.Provider, error) {
	ps, err := s.inner.ListAll(ctx, ws, opts...)
	for _, p := range ps {
		redacted(p)
	}
	return ps, err
}

func (s *providerStore) Create(ctx context.Context, ws string, p *v1.Provider) (*v1.Provider, error) {
	if err := s.record("create", p.Name); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Type) == "" {
		return nil, &types.StatusError{Code: types.ErrorInvalidArgument, Message: "provider.type is required"}
	}
	if len(p.Spec.Credentials) == 0 && !typesWithoutCredentials[p.Type] {
		return nil, &types.StatusError{Code: types.ErrorInvalidArgument, Message: "provider.credentials must not be empty"}
	}
	got, err := s.inner.Create(ctx, ws, p)
	return redacted(got), err
}

func merge(stored, incoming map[string]string) map[string]string {
	out := maps.Clone(stored)
	if out == nil {
		out = map[string]string{}
	}
	for k, v := range incoming {
		if v == "" {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

func (s *providerStore) Update(ctx context.Context, ws string, p *v1.Provider) (*v1.Provider, error) {
	if err := s.record("update", p.Name); err != nil {
		return nil, err
	}
	cur, err := s.inner.Get(ctx, ws, p.Name)
	if err != nil {
		return nil, err
	}
	if p.Type != "" && !strings.EqualFold(p.Type, cur.Type) {
		return nil, &types.StatusError{Code: types.ErrorInvalidArgument, Message: "provider type cannot be changed; delete and recreate the provider"}
	}
	next := *cur
	next.Spec.Credentials = merge(cur.Spec.Credentials, p.Spec.Credentials)
	next.Spec.Config = merge(cur.Spec.Config, p.Spec.Config)
	got, err := s.inner.Update(ctx, ws, &next)
	return redacted(got), err
}

func (s *providerStore) Delete(ctx context.Context, ws, name string, opts ...v1.DeleteOptions) (*v1.DeletionResult, error) {
	if err := s.record("delete", name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	attached := s.attached[name]
	s.mu.Unlock()
	if attached {
		return nil, &types.StatusError{Code: types.ErrorConflict, Message: "provider '" + name + "' is attached to sandbox(es): x"}
	}
	return s.inner.Delete(ctx, ws, name, opts...)
}

// stored returns the provider as the gateway holds it, credentials too.
func (s *providerStore) stored(t *testing.T, name string) *v1.Provider {
	t.Helper()
	p, err := s.inner.Get(context.Background(), "default", name)
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return p
}

func (s *providerStore) takeCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.calls
	s.calls = nil
	return c
}

// writeSecretVolume lays dir out the way the kubelet mounts a Secret, or
// rotates it by swapping the ..data link: the files live in a
// "..<timestamp>" directory, "..data" links to it, and each key is a link
// through "..data". A key not in data is removed.
func writeSecretVolume(t *testing.T, dir string, data map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ts, err := os.MkdirTemp(dir, "..2026_11_13_12_00_00.")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range data {
		if err := os.WriteFile(filepath.Join(ts, k), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tmp := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(filepath.Base(ts), tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := data[e.Name()]; !ok && !strings.HasPrefix(e.Name(), "..") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	for k := range data {
		link := filepath.Join(dir, k)
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.Symlink(filepath.Join("..data", k), link); err != nil {
			t.Fatal(err)
		}
	}
}

type syncHarness struct {
	store *providerStore
	r     *ProviderReconciler
	sub   *events.Subscription
	logs  *bytes.Buffer
	cfg   *config.Config
}

// newSyncHarness returns a reconciler for the declared providers, over a
// fake gateway.
func newSyncHarness(t *testing.T, prune bool, declared ...config.OpenShellProvider) *syncHarness {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.OpenShell.Providers = declared
	cfg.OpenShell.PruneProviders = prune
	cfg.Sessions.Profiles = map[string]config.SessionProfile{
		"agent": {Providers: []string{"my-github", "manual"}},
		"other": {Providers: []string{"my-github"}},
	}
	store := newProviderStore(t)
	bus := events.New()
	t.Cleanup(bus.Close)
	sub := bus.Subscribe(events.OfKind(events.KindProvider))
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)
	r := NewProviderReconciler(func() (ProviderAPI, io.Closer, error) {
		return store, io.NopCloser(nil), nil
	}, cfg, WithProviderEvents(bus), WithProviderLogger(logger), WithProviderSyncKey([]byte("test-key")))
	return &syncHarness{store: store, r: r, sub: sub, logs: &logs, cfg: cfg}
}

func (h *syncHarness) status(t *testing.T, name string) ProviderStatus {
	t.Helper()
	for _, st := range h.r.Status() {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("no status for %s", name)
	return ProviderStatus{}
}

// events returns the provider events published so far.
func (h *syncHarness) events() []events.ProviderOutcome {
	var out []events.ProviderOutcome
	for {
		select {
		case ev := <-h.sub.C:
			out = append(out, *ev.Provider)
		default:
			return out
		}
	}
}

// checkNoSecrets fails when a credential value shows in the status (as
// JSON), the logs or the events.
func (h *syncHarness) checkNoSecrets(t *testing.T, secrets ...string) {
	t.Helper()
	b, _ := json.Marshal(h.r.Status())
	for _, s := range secrets {
		if strings.Contains(string(b), s) || strings.Contains(h.logs.String(), s) {
			t.Fatalf("secret %q leaked: status %s, logs %s", s, b, h.logs)
		}
	}
}

func TestProviderSyncCreateUnchangedUpdate(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "my-github")
	writeSecretVolume(t, dir, map[string]string{"GITHUB_TOKEN": "ghp_one\n", "GH_ENTERPRISE_TOKEN": "ghe_one"})
	h := newSyncHarness(t, false, config.OpenShellProvider{
		Name: "my-github", Type: "github", CredentialsDir: dir,
		Config: map[string]string{"GITHUB_API_URL": "https://api.github.com", "extra": "x"},
	})
	if st := h.status(t, "my-github"); st.State != ProviderPending || !slices.Equal(st.Profiles, []string{"agent", "other"}) {
		t.Fatalf("before sync: %+v", st)
	}

	// Created, with the managed label, the credentials and the sync state.
	h.r.Sync(ctx)
	if calls := h.store.takeCalls(); !slices.Equal(calls, []string{"create my-github"}) {
		t.Fatalf("calls = %v", calls)
	}
	p := h.store.stored(t, "my-github")
	if p.Type != "github" || p.Labels[LabelManaged] != "true" ||
		!maps.Equal(p.Spec.Credentials, map[string]string{"GITHUB_TOKEN": "ghp_one", "GH_ENTERPRISE_TOKEN": "ghe_one"}) ||
		p.Spec.Config["GITHUB_API_URL"] != "https://api.github.com" ||
		p.Spec.Config[ProviderConfigCredentialKeys] != "GH_ENTERPRISE_TOKEN,GITHUB_TOKEN" ||
		p.Spec.Config[ProviderConfigKeys] != "GITHUB_API_URL,extra" ||
		len(p.Spec.Config[ProviderConfigHash]) != 64 {
		t.Fatalf("created = %+v", p)
	}
	st := h.status(t, "my-github")
	if st.State != ProviderSynced || !st.Managed || st.Adopted || st.LastAction != "created" || st.LastSync == nil || st.LastChange == nil ||
		!slices.Equal(st.CredentialKeys, []string{"GH_ENTERPRISE_TOKEN", "GITHUB_TOKEN"}) || !slices.Equal(st.ConfigKeys, []string{"GITHUB_API_URL", "extra"}) {
		t.Fatalf("status = %+v", st)
	}
	if evs := h.events(); len(evs) != 1 || evs[0] != (events.ProviderOutcome{Provider: "my-github", Action: events.ProviderCreated, OK: true}) {
		t.Fatalf("events = %+v", evs)
	}

	// Unchanged: nothing is written, nothing published.
	hash := p.Spec.Config[ProviderConfigHash]
	version := p.ResourceVersion
	h.r.Sync(ctx)
	if calls := h.store.takeCalls(); len(calls) != 0 {
		t.Fatalf("unchanged provider rewritten: %v", calls)
	}
	if st := h.status(t, "my-github"); st.LastAction != "unchanged" || st.State != ProviderSynced {
		t.Fatalf("status = %+v", st)
	}
	if h.store.stored(t, "my-github").ResourceVersion != version || len(h.events()) != 0 {
		t.Fatal("unchanged provider changed or published")
	}

	// The Secret is rotated, one key dropped and the config changed: the
	// provider is updated, the dropped keys deleted.
	writeSecretVolume(t, dir, map[string]string{"GITHUB_TOKEN": "ghp_two\n"})
	h.r.declared[0].Config = map[string]string{"GITHUB_API_URL": "https://ghe.example.com/api/v3"}
	h.r.Sync(ctx)
	if calls := h.store.takeCalls(); !slices.Equal(calls, []string{"update my-github"}) {
		t.Fatalf("calls = %v", calls)
	}
	p = h.store.stored(t, "my-github")
	if !maps.Equal(p.Spec.Credentials, map[string]string{"GITHUB_TOKEN": "ghp_two"}) ||
		p.Spec.Config["GITHUB_API_URL"] != "https://ghe.example.com/api/v3" || p.Spec.Config["extra"] != "" ||
		p.Spec.Config[ProviderConfigCredentialKeys] != "GITHUB_TOKEN" || p.Spec.Config[ProviderConfigKeys] != "GITHUB_API_URL" ||
		p.Spec.Config[ProviderConfigHash] == hash || p.Labels[LabelManaged] != "true" {
		t.Fatalf("updated = %+v", p)
	}
	if st := h.status(t, "my-github"); st.LastAction != "updated" || !slices.Equal(st.CredentialKeys, []string{"GITHUB_TOKEN"}) {
		t.Fatalf("status = %+v", st)
	}
	if evs := h.events(); len(evs) != 1 || evs[0].Action != events.ProviderUpdated {
		t.Fatalf("events = %+v", evs)
	}
	h.r.Sync(ctx)
	if calls := h.store.takeCalls(); len(calls) != 0 {
		t.Fatalf("updated provider rewritten: %v", calls)
	}
	h.checkNoSecrets(t, "ghp_one", "ghp_two", "ghe_one")

	// A type change is refused, as the gateway would.
	h.r.declared[0].Type = "gitlab"
	h.r.Sync(ctx)
	st = h.status(t, "my-github")
	if st.State != ProviderFailed || !strings.Contains(st.LastError, `has type "github", not "gitlab"`) {
		t.Fatalf("status = %+v", st)
	}
}

// A provider of the name that Sundae did not create is left alone and
// reported, unless the declaration adopts it.
func TestProviderSyncConflictAndAdopt(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "creds")
	writeSecretVolume(t, dir, map[string]string{"GITHUB_TOKEN": "ghp_sundae"})
	decl := config.OpenShellProvider{Name: "my-github", Type: "github", CredentialsDir: dir}
	h := newSyncHarness(t, false, decl)
	if _, err := h.store.inner.Create(ctx, "default", &v1.Provider{
		Name: "my-github", Type: "github", Labels: map[string]string{"team": "x"},
		Spec: v1.ProviderSpec{Credentials: map[string]string{"GITHUB_TOKEN": "ghp_manual", "OTHER": "o"}},
	}); err != nil {
		t.Fatal(err)
	}

	h.r.Sync(ctx)
	h.r.Sync(ctx)
	if calls := h.store.takeCalls(); len(calls) != 0 {
		t.Fatalf("unmanaged provider touched: %v", calls)
	}
	if got := h.store.stored(t, "my-github").Spec.Credentials["GITHUB_TOKEN"]; got != "ghp_manual" {
		t.Fatalf("unmanaged provider overwritten: %q", got)
	}
	st := h.status(t, "my-github")
	if st.State != ProviderConflict || st.Managed || !strings.Contains(st.LastError, "was not created by Sundae") || !strings.Contains(st.LastError, "adopt: true") {
		t.Fatalf("status = %+v", st)
	}
	// Logged and published once, not every round.
	if evs := h.events(); len(evs) != 1 || evs[0].Action != events.ProviderFailed || evs[0].OK {
		t.Fatalf("events = %+v", evs)
	}
	if n := strings.Count(h.logs.String(), "cannot sync OpenShell provider"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, h.logs)
	}
	var serr *ProviderSyncError
	if err := h.r.Check([]string{"manual", "my-github"}); !errors.As(err, &serr) || serr.Provider != "my-github" || serr.State != ProviderConflict {
		t.Fatalf("check = %v", err)
	}

	// Adopted: updated in place; its labels stay (OpenShell keeps them),
	// and keys Sundae never set are left alone.
	h.r.declared[0].Adopt = true
	h.r.Sync(ctx)
	if calls := h.store.takeCalls(); !slices.Equal(calls, []string{"update my-github"}) {
		t.Fatalf("calls = %v", calls)
	}
	p := h.store.stored(t, "my-github")
	if !maps.Equal(p.Spec.Credentials, map[string]string{"GITHUB_TOKEN": "ghp_sundae", "OTHER": "o"}) || p.Labels[LabelManaged] != "" || p.Labels["team"] != "x" {
		t.Fatalf("adopted = %+v", p)
	}
	st = h.status(t, "my-github")
	if st.State != ProviderSynced || st.Managed || !st.Adopted || st.LastError != "" {
		t.Fatalf("status = %+v", st)
	}
	if err := h.r.Check([]string{"my-github"}); err != nil {
		t.Fatal(err)
	}
	h.r.Sync(ctx)
	if calls := h.store.takeCalls(); len(calls) != 0 {
		t.Fatalf("adopted provider rewritten: %v", calls)
	}
	h.checkNoSecrets(t, "ghp_sundae", "ghp_manual")
}

// Each provider is synced on its own: one failing does not hold up the
// others.
func TestProviderSyncErrorsIsolated(t *testing.T) {
	ctx := context.Background()
	td := t.TempDir()
	good := filepath.Join(td, "good")
	writeSecretVolume(t, good, map[string]string{"ANTHROPIC_API_KEY": "sk-ant"})
	h := newSyncHarness(t, false,
		config.OpenShellProvider{Name: "missing-dir", Type: "github", CredentialsDir: filepath.Join(td, "nope")},
		config.OpenShellProvider{Name: "gateway-fails", Type: "github", CredentialsDir: good},
		config.OpenShellProvider{Name: "my-claude", Type: "anthropic", CredentialsDir: good},
		config.OpenShellProvider{Name: "no-creds", Type: "github"},
		config.OpenShellProvider{Name: "pypi", Type: "pypi"},
	)
	h.store.fail["gateway-fails"] = &types.StatusError{Code: types.ErrorUnavailable, Message: "gateway down"}

	h.r.Sync(ctx)
	want := map[string]string{
		"missing-dir":   ProviderFailed,
		"gateway-fails": ProviderFailed,
		"my-claude":     ProviderSynced,
		"no-creds":      ProviderFailed,
		"pypi":          ProviderSynced,
	}
	for name, state := range want {
		if st := h.status(t, name); st.State != state {
			t.Errorf("%s: %+v, want %s", name, st, state)
		}
	}
	if st := h.status(t, "missing-dir"); !strings.Contains(st.LastError, "read credentials_dir") {
		t.Errorf("missing-dir: %q", st.LastError)
	}
	if st := h.status(t, "no-creds"); !strings.Contains(st.LastError, "provider.credentials must not be empty") {
		t.Errorf("no-creds: %q", st.LastError)
	}
	if err := h.r.Check([]string{"my-claude", "pypi", "manual"}); err != nil {
		t.Fatal(err)
	}
	if err := h.r.Check([]string{"my-claude", "gateway-fails"}); err == nil || !strings.Contains(err.Error(), `OpenShell provider "gateway-fails" (openshell.providers) failed to sync to the gateway: get provider:`) {
		t.Fatalf("check = %v", err)
	}

	// Recovered: the failure clears.
	delete(h.store.fail, "gateway-fails")
	h.r.Sync(ctx)
	if st := h.status(t, "gateway-fails"); st.State != ProviderSynced || st.LastError != "" {
		t.Fatalf("status = %+v", st)
	}

	// A gateway that cannot be reached fails every provider.
	h.r.newAPI = func() (ProviderAPI, io.Closer, error) { return nil, nil, errors.New("no token") }
	h.r.Sync(ctx)
	for name := range want {
		if st := h.status(t, name); st.State != ProviderFailed || !strings.Contains(st.LastError, "connect to the gateway: no token") {
			t.Errorf("%s: %+v", name, st)
		}
	}
}

// Providers Sundae created and no longer declares are kept and reported,
// unless prune_providers; providers it did not create are never deleted.
func TestProviderSyncPrune(t *testing.T) {
	ctx := context.Background()
	for _, prune := range []bool{false, true} {
		h := newSyncHarness(t, prune, config.OpenShellProvider{Name: "pypi", Type: "pypi"})
		for _, p := range []*v1.Provider{
			{Name: "old", Type: "github", Labels: map[string]string{LabelManaged: "true"}, Spec: v1.ProviderSpec{Credentials: map[string]string{"T": "x"}}},
			{Name: "busy", Type: "github", Labels: map[string]string{LabelManaged: "true"}, Spec: v1.ProviderSpec{Credentials: map[string]string{"T": "x"}}},
			{Name: "manual", Type: "github", Spec: v1.ProviderSpec{Credentials: map[string]string{"T": "x"}}},
		} {
			if _, err := h.store.inner.Create(ctx, "default", p); err != nil {
				t.Fatal(err)
			}
		}
		h.store.attached["busy"] = true

		h.r.Sync(ctx)
		names := func() []string {
			ps, _ := h.store.inner.ListAll(ctx, "default")
			var n []string
			for _, p := range ps {
				n = append(n, p.Name)
			}
			slices.Sort(n)
			return n
		}
		var got []string
		for _, st := range h.r.Status() {
			got = append(got, st.Name+":"+st.State)
		}
		if !prune {
			if n := names(); !slices.Equal(n, []string{"busy", "manual", "old", "pypi"}) {
				t.Fatalf("prune off: providers = %v", n)
			}
			if !slices.Equal(got, []string{"pypi:synced", "busy:undeclared", "old:undeclared"}) {
				t.Fatalf("prune off: status = %v", got)
			}
			if calls := h.store.takeCalls(); slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "delete") }) {
				t.Fatalf("prune off deleted: %v", calls)
			}
			continue
		}
		if n := names(); !slices.Equal(n, []string{"busy", "manual", "pypi"}) {
			t.Fatalf("prune on: providers = %v", n)
		}
		if !slices.Equal(got, []string{"pypi:synced", "busy:undeclared"}) {
			t.Fatalf("prune on: status = %v", got)
		}
		if st := h.status(t, "busy"); !strings.Contains(st.LastError, "is attached to sandbox") || !st.Managed || st.Declared {
			t.Fatalf("busy = %+v", st)
		}
		evs := h.events()
		if !slices.Contains(evs, events.ProviderOutcome{Provider: "old", Action: events.ProviderDeleted, OK: true}) {
			t.Fatalf("events = %+v", evs)
		}
		// Detached: deleted on the next round, and no longer reported.
		h.store.attached["busy"] = false
		h.r.Sync(ctx)
		if n := names(); !slices.Equal(n, []string{"manual", "pypi"}) || len(h.r.Status()) != 1 {
			t.Fatalf("after detach: providers = %v, status = %+v", n, h.r.Status())
		}
	}
}

// A session start fails, naming the provider, when its profile lists a
// declared provider that failed to sync; an attach only warns.
func TestStartFailsOnFailingDeclaredProvider(t *testing.T) {
	ctx := context.Background()
	h, _ := newProviderHarness(t, "my-claude", "my-github")
	sh := newSyncHarness(t, false,
		config.OpenShellProvider{Name: "my-github", Type: "github", CredentialsDir: filepath.Join(t.TempDir(), "missing")},
	)

	// Not synced yet: nothing is known, the start goes ahead.
	h.mgr.providerSync = sh.r
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Profile: "agent"}); err != nil {
		t.Fatal(err)
	}

	sh.r.Sync(ctx)
	_, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Name: "two", Profile: "agent"})
	var serr *ProviderSyncError
	if !errors.As(err, &serr) || serr.Provider != "my-github" ||
		!strings.Contains(err.Error(), `session two: profile "agent" lists a provider Sundae cannot sync: OpenShell provider "my-github" (openshell.providers) failed to sync to the gateway: read credentials_dir`) {
		t.Fatalf("err = %v", err)
	}
	if _, err := h.fc.Sandboxes().Get(ctx, "default", SessionSandboxName("alice", "r", "two")); !IsNotFound(err) {
		t.Fatalf("sandbox created: %v", err)
	}
	// Starting the existing session fails too; attaching warns.
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); !errors.As(err, &serr) {
		t.Fatalf("start existing: %v", err)
	}
	var progress bytes.Buffer
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r", Progress: &progress}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(progress.String(), `Warning: OpenShell provider "my-github" (openshell.providers) failed to sync`) {
		t.Fatalf("progress = %q", progress.String())
	}
	// Profiles without it are not affected.
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Name: "plain", Profile: "shell"}); err != nil {
		t.Fatal(err)
	}
}

// A declared provider the gateway does not have yet says so.
func TestStartDeclaredProviderNotCreatedYet(t *testing.T) {
	ctx := context.Background()
	h, _ := newProviderHarness(t, "my-claude", "declared")
	sh := newSyncHarness(t, false, config.OpenShellProvider{Name: "declared", Type: "pypi"})
	h.mgr.providerSync = sh.r
	_, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Profile: "agent"})
	if err == nil || !strings.Contains(err.Error(), `which openshell.providers declares but Sundae has not created on the gateway yet`) {
		t.Fatalf("err = %v", err)
	}
}

func TestProviderSyncKey(t *testing.T) {
	td := t.TempDir()
	path := ProviderSyncKeyPath(td)
	k1, err := loadProviderSyncKey(path)
	if err != nil || len(k1) != providerSyncKeyLen {
		t.Fatalf("key = %x, %v", k1, err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v, %v", fi, err)
	}
	k2, err := loadProviderSyncKey(path)
	if err != nil || !bytes.Equal(k1, k2) {
		t.Fatal("key not kept")
	}

	// The hash depends on the key, the type and every key and value.
	cfg := config.DefaultConfig()
	cfg.DataPath = td
	r := NewProviderReconciler(nil, cfg)
	if !bytes.Equal(r.key, k1) {
		t.Fatal("reconciler does not use the kept key")
	}
	base := r.hash("github", map[string]string{"A": "1"}, nil)
	for _, h := range []string{
		r.hash("gitlab", map[string]string{"A": "1"}, nil),
		r.hash("github", map[string]string{"A": "2"}, nil),
		r.hash("github", map[string]string{"B": "1"}, nil),
		r.hash("github", nil, map[string]string{"A": "1"}),
		NewProviderReconciler(nil, cfg, WithProviderSyncKey([]byte("other"))).hash("github", map[string]string{"A": "1"}, nil),
	} {
		if h == base {
			t.Fatal("hash collision")
		}
	}
	if r.hash("github", map[string]string{"A": "1"}, nil) != base {
		t.Fatal("hash not stable")
	}
}

func TestProviderReconcilerRunAndNil(t *testing.T) {
	var r *ProviderReconciler
	r.Sync(context.Background())
	r.Run(context.Background())
	if r.Status() != nil || r.Check([]string{"x"}) != nil || r.Declared("x") {
		t.Fatal("nil reconciler")
	}
	if ProviderReconcilerFromContext(context.Background()) != nil {
		t.Fatal("context")
	}

	h := newSyncHarness(t, false, config.OpenShellProvider{Name: "pypi", Type: "pypi"})
	ticks := make(chan time.Time)
	var waits []time.Duration
	h.r.tick = func(d time.Duration) <-chan time.Time { waits = append(waits, d); return ticks }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.r.Run(ctx); close(done) }()
	ticks <- time.Now() // the first round ran; let the second run
	cancel()
	<-done
	if len(waits) < 1 || waits[0] != config.DefaultProvidersSync {
		t.Fatalf("waits = %v", waits)
	}
	if ProviderReconcilerFromContext(ContextWithProviderReconciler(context.Background(), h.r)) != h.r {
		t.Fatal("context")
	}
}
