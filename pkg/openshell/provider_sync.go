package openshell

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/log/v2"
	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Provider is an OpenShell provider.
type Provider = v1.Provider

// ProviderAPI is the gateway's provider surface the ProviderReconciler
// uses. The SDK's v1.ProviderInterface, and its fake's, satisfy it.
type ProviderAPI interface {
	Get(ctx context.Context, workspace, name string) (*v1.Provider, error)
	Create(ctx context.Context, workspace string, provider *v1.Provider) (*v1.Provider, error)
	Update(ctx context.Context, workspace string, provider *v1.Provider) (*v1.Provider, error)
	Delete(ctx context.Context, workspace, name string, opts ...v1.DeleteOptions) (*v1.DeletionResult, error)
	ListAll(ctx context.Context, workspace string, opts ...v1.ListOptions) ([]*v1.Provider, error)
}

// Sundae's sync state on a provider. OpenShell (0.1.8) sets a provider's
// labels only when it is created and returns no credential values or keys
// through the SDK, and a provider update merges credentials and config
// into what is stored (an empty value deletes a key). So Sundae keeps what
// it needs to compare and to delete keys in the provider's config, which
// update changes and the API returns.
const (
	// ProviderConfigHash holds an HMAC-SHA256 of the provider's type,
	// credentials and declared config, keyed by a secret in Sundae's data
	// directory, so an unchanged provider is not rewritten and the hash
	// says nothing about the credentials to whoever can read it.
	ProviderConfigHash = config.ProviderConfigReserved + "sync-hash"
	// ProviderConfigCredentialKeys holds the credential keys Sundae set,
	// comma-separated, so a key dropped from credentials_dir is deleted.
	ProviderConfigCredentialKeys = config.ProviderConfigReserved + "credential-keys"
	// ProviderConfigKeys holds the declared config keys Sundae set.
	ProviderConfigKeys = config.ProviderConfigReserved + "config-keys"
)

// Provider sync states.
const (
	// ProviderPending: not synced yet.
	ProviderPending = "pending"
	// ProviderSynced: the gateway has the declared provider.
	ProviderSynced = "synced"
	// ProviderFailed: the last sync failed.
	ProviderFailed = "failed"
	// ProviderConflict: a provider of the name exists that Sundae did not
	// create, and the declaration does not adopt it.
	ProviderConflict = "conflict"
	// ProviderUndeclared: a provider Sundae created that is no longer
	// declared. It is kept unless openshell.prune_providers is set.
	ProviderUndeclared = "undeclared"
	// ProviderManual: a provider a profile lists that is not declared;
	// Sundae attaches it by name and does not manage it.
	ProviderManual = "manual"
)

// Provider sync defaults.
const (
	// DefaultProviderSyncTimeout bounds the gateway calls of one provider's
	// sync.
	DefaultProviderSyncTimeout = 30 * time.Second
	// providerSyncKeyLen is the length of the hash key, in bytes.
	providerSyncKeyLen = 32
)

var (
	providerSyncCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sundae",
		Subsystem: "openshell",
		Name:      "provider_syncs_total",
		Help:      "Syncs of OpenShell providers declared in openshell.providers, by provider and result: created, updated, unchanged, deleted (pruned) or failed.",
	}, []string{"provider", "result"})

	providerSyncOK = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "sundae",
		Subsystem: "openshell",
		Name:      "provider_sync_ok",
		Help:      "1 when the last sync of the declared OpenShell provider succeeded, else 0.",
	}, []string{"provider"})
)

// ProviderStatus is what the ProviderReconciler knows of one provider. It
// never holds a credential value.
type ProviderStatus struct {
	// Name is the provider's name.
	Name string `json:"name"`
	// Type is the declared type; for an undeclared provider, the
	// gateway's.
	Type string `json:"type,omitempty"`
	// Declared reports whether openshell.providers declares it.
	Declared bool `json:"declared"`
	// Adopt is the declaration's adopt.
	Adopt bool `json:"adopt,omitempty"`
	// State is ProviderPending, ProviderSynced, ProviderFailed,
	// ProviderConflict, ProviderUndeclared or ProviderManual.
	State string `json:"state"`
	// Managed reports whether the provider carries Sundae's managed label:
	// Sundae created it.
	Managed bool `json:"managed"`
	// Adopted reports whether Sundae keeps a provider it did not create up
	// to date, because the declaration adopts it.
	Adopted bool `json:"adopted"`
	// CredentialKeys are the names of the credentials Sundae read, never
	// their values.
	CredentialKeys []string `json:"credential_keys,omitempty"`
	// ConfigKeys are the names of the declared config entries.
	ConfigKeys []string `json:"config_keys,omitempty"`
	// Profiles are the profiles that list the provider.
	Profiles []string `json:"profiles,omitempty"`
	// LastSync is when the provider was last found or made up to date.
	LastSync *time.Time `json:"last_sync,omitempty"`
	// LastChange is when Sundae last created or updated it.
	LastChange *time.Time `json:"last_change,omitempty"`
	// LastAction is what the last successful sync did: created, updated
	// or unchanged.
	LastAction string `json:"last_action,omitempty"`
	// LastError is why the last sync failed; empty when it succeeded.
	LastError string `json:"last_error,omitempty"`
}

// ProviderSyncError is returned for a session whose profile lists a
// declared provider that failed to sync.
type ProviderSyncError struct {
	// Provider is the provider's name.
	Provider string
	// State is ProviderFailed or ProviderConflict.
	State string
	// Err is why the sync failed.
	Err string
}

func (e *ProviderSyncError) Error() string {
	return fmt.Sprintf("OpenShell provider %q (openshell.providers) failed to sync to the gateway: %s", e.Provider, e.Err)
}

// ProviderReconciler creates and updates the OpenShell providers declared
// in openshell.providers from their credential files, on an interval, for
// the server's lifetime. Each provider is synced on its own: one failing
// does not hold up the others. It keeps what it knows in memory only. A
// nil *ProviderReconciler is valid: it declares nothing.
type ProviderReconciler struct {
	newAPI    func() (ProviderAPI, io.Closer, error)
	workspace string
	declared  []config.OpenShellProvider
	profiles  map[string][]string
	prune     bool
	interval  time.Duration
	timeout   time.Duration
	key       []byte
	bus       *events.Bus
	logger    *log.Logger
	now       func() time.Time
	tick      func(time.Duration) <-chan time.Time

	// syncMu serialises sync rounds.
	syncMu sync.Mutex

	mu         sync.Mutex
	status     map[string]*ProviderStatus
	undeclared map[string]*ProviderStatus
}

// ProviderReconcilerOption configures a ProviderReconciler.
type ProviderReconcilerOption func(*ProviderReconciler)

// WithProviderEvents publishes provider creations, updates, deletions and
// failures on bus.
func WithProviderEvents(bus *events.Bus) ProviderReconcilerOption {
	return func(r *ProviderReconciler) { r.bus = bus }
}

// WithProviderLogger sets the logger.
func WithProviderLogger(l *log.Logger) ProviderReconcilerOption {
	return func(r *ProviderReconciler) { r.logger = l }
}

// WithProviderSyncKey sets the key the sync hash is computed with,
// instead of the one kept in the data directory.
func WithProviderSyncKey(key []byte) ProviderReconcilerOption {
	return func(r *ProviderReconciler) { r.key = slices.Clone(key) }
}

// NewProviderReconciler returns a ProviderReconciler for cfg's
// openshell.providers. newAPI is called for every sync round and its
// closer closed after, so a rotated gateway token is picked up as it is by
// the session commands. The hash key is read from, or created in,
// ProviderSyncKeyPath(cfg.DataPath).
func NewProviderReconciler(newAPI func() (ProviderAPI, io.Closer, error), cfg *config.Config, opts ...ProviderReconcilerOption) *ProviderReconciler {
	r := &ProviderReconciler{
		newAPI:     newAPI,
		workspace:  cfg.OpenShell.Workspace,
		declared:   slices.Clone(cfg.OpenShell.Providers),
		profiles:   ProfilesByProvider(cfg.Sessions),
		prune:      cfg.OpenShell.PruneProviders,
		interval:   cfg.OpenShell.ProvidersSync,
		timeout:    DefaultProviderSyncTimeout,
		now:        time.Now,
		tick:       time.After,
		status:     map[string]*ProviderStatus{},
		undeclared: map[string]*ProviderStatus{},
	}
	if r.workspace == "" {
		r.workspace = "default"
	}
	if r.interval <= 0 {
		r.interval = config.DefaultProvidersSync
	}
	for _, o := range opts {
		o(r)
	}
	if r.logger == nil {
		r.logger = log.Default()
	}
	r.logger = r.logger.WithPrefix("providers")
	if r.key == nil {
		key, err := loadProviderSyncKey(ProviderSyncKeyPath(cfg.DataPath))
		if err != nil {
			// A key that does not survive the restart only costs one
			// rewrite of every provider after it.
			r.logger.Warn("cannot keep the provider sync key; using one for this run only", "err", err)
		}
		r.key = key
	}
	for _, p := range r.declared {
		r.status[p.Name] = &ProviderStatus{
			Name:     p.Name,
			Type:     p.Type,
			Declared: true,
			Adopt:    p.Adopt,
			State:    ProviderPending,
			Profiles: r.profiles[p.Name],
		}
	}
	return r
}

// ProviderSyncKeyPath is where the key of the provider sync hash is kept.
func ProviderSyncKeyPath(dataPath string) string {
	return filepath.Join(dataPath, "openshell", "provider-sync.key")
}

// loadProviderSyncKey reads the key at path, creating it when absent. On
// error it returns a random key that lasts for this run.
func loadProviderSyncKey(path string) ([]byte, error) {
	key := make([]byte, providerSyncKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate provider sync key: %w", err)
	}
	b, err := os.ReadFile(path)
	if err == nil && len(b) >= providerSyncKeyLen {
		return b[:providerSyncKeyLen], nil
	}
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			err = errors.New("too short")
		}
		return key, fmt.Errorf("read %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return key, err //nolint:wrapcheck
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		// Created concurrently.
		return loadProviderSyncKey(path)
	}
	if err != nil {
		return key, err //nolint:wrapcheck
	}
	_, werr := f.Write(key)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
		return key, fmt.Errorf("write %s: %w", path, werr)
	}
	return key, nil
}

// ProfilesByProvider maps each provider the profiles list to the sorted
// names of the profiles listing it.
func ProfilesByProvider(sc config.SessionsConfig) map[string][]string {
	m := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(sc.Profiles)) {
		for _, p := range sc.Profiles[name].Providers {
			m[p] = append(m[p], name)
		}
	}
	return m
}

// Run syncs the declared providers now and then on every interval, until
// ctx is done.
func (r *ProviderReconciler) Run(ctx context.Context) {
	if r == nil {
		return
	}
	for {
		r.Sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-r.tick(r.interval):
		}
	}
}

// Sync runs one round now: every declared provider is read from its files
// and created or updated on the gateway as needed, then the providers
// Sundae created that are no longer declared are reported, or deleted
// with openshell.prune_providers.
func (r *ProviderReconciler) Sync(ctx context.Context) {
	if r == nil {
		return
	}
	r.syncMu.Lock()
	defer r.syncMu.Unlock()

	api, closer, err := r.newAPI()
	if err != nil {
		for _, p := range r.declared {
			r.failed(p, fmt.Errorf("connect to the gateway: %w", err), false)
		}
		return
	}
	defer closer.Close() //nolint:errcheck

	for _, p := range r.declared {
		if ctx.Err() != nil {
			return
		}
		pctx, cancel := context.WithTimeout(ctx, r.timeout)
		r.syncOne(pctx, api, p)
		cancel()
	}
	if ctx.Err() == nil {
		lctx, cancel := context.WithTimeout(ctx, r.timeout)
		r.reviewUndeclared(lctx, api)
		cancel()
	}
}

// desired is a declared provider as the gateway should have it.
type desired struct {
	creds  map[string]string
	config map[string]string
	hash   string
}

// desiredProvider reads p's credentials and computes what the gateway
// should hold.
func (r *ProviderReconciler) desiredProvider(p config.OpenShellProvider) (desired, error) {
	creds, err := p.ReadCredentials()
	if err != nil {
		return desired{}, err //nolint:wrapcheck
	}
	return desired{creds: creds, config: maps.Clone(p.Config), hash: r.hash(p.Type, creds, p.Config)}, nil
}

// hash returns the keyed hash of a provider's type, credentials and
// declared config.
func (r *ProviderReconciler) hash(typ string, creds, cfg map[string]string) string {
	mac := hmac.New(sha256.New, r.key)
	write := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		mac.Write(n[:])
		mac.Write([]byte(s))
	}
	write("sundae-provider-v1")
	write(typ)
	for _, m := range []map[string]string{creds, cfg} {
		write(fmt.Sprint(len(m)))
		for _, k := range slices.Sorted(maps.Keys(m)) {
			write(k)
			write(m[k])
		}
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// syncOne syncs one declared provider.
func (r *ProviderReconciler) syncOne(ctx context.Context, api ProviderAPI, p config.OpenShellProvider) {
	want, err := r.desiredProvider(p)
	if err != nil {
		r.failed(p, err, false)
		return
	}
	credKeys := slices.Sorted(maps.Keys(want.creds))
	cfgKeys := slices.Sorted(maps.Keys(want.config))
	stateConfig := func() map[string]string {
		c := maps.Clone(want.config)
		if c == nil {
			c = map[string]string{}
		}
		c[ProviderConfigHash] = want.hash
		c[ProviderConfigCredentialKeys] = strings.Join(credKeys, ",")
		c[ProviderConfigKeys] = strings.Join(cfgKeys, ",")
		return c
	}

	cur, err := api.Get(ctx, r.workspace, p.Name)
	switch {
	case IsNotFound(err):
		_, err := api.Create(ctx, r.workspace, &v1.Provider{
			Name:   p.Name,
			Type:   p.Type,
			Labels: map[string]string{LabelManaged: "true"},
			Spec:   v1.ProviderSpec{Credentials: want.creds, Config: stateConfig()},
		})
		if err != nil {
			r.failed(p, fmt.Errorf("create provider: %w", err), false)
			return
		}
		r.succeeded(p, events.ProviderCreated, true, false, credKeys, cfgKeys)
		return
	case err != nil:
		r.failed(p, fmt.Errorf("get provider: %w", err), false)
		return
	}

	managed := cur.Labels[LabelManaged] == "true"
	if !managed && !p.Adopt {
		r.failed(p, fmt.Errorf("a provider named %q already exists on the gateway and was not created by Sundae (it lacks the label %s=true); Sundae leaves it alone: delete it (openshell provider delete %s) to let Sundae create it, or set adopt: true to let Sundae take it over", p.Name, LabelManaged, p.Name), true)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(cur.Type), p.Type) {
		r.failed(p, fmt.Errorf("the provider on the gateway has type %q, not %q, and OpenShell cannot change a provider's type: delete it (openshell provider delete %s) to let Sundae create it again", cur.Type, p.Type, p.Name), false)
		return
	}
	if cur.Spec.Config[ProviderConfigHash] == want.hash {
		r.succeeded(p, "unchanged", managed, !managed, credKeys, cfgKeys)
		return
	}

	// OpenShell merges an update into the stored provider and deletes the
	// keys updated to "": delete those Sundae set before and no longer
	// declares. An adopted provider's other keys are left alone.
	creds := maps.Clone(want.creds)
	if creds == nil {
		creds = map[string]string{}
	}
	for _, k := range splitKeys(cur.Spec.Config[ProviderConfigCredentialKeys]) {
		if _, ok := creds[k]; !ok {
			creds[k] = ""
		}
	}
	cfg := stateConfig()
	for _, k := range splitKeys(cur.Spec.Config[ProviderConfigKeys]) {
		if _, ok := cfg[k]; !ok && !strings.HasPrefix(k, config.ProviderConfigReserved) {
			cfg[k] = ""
		}
	}
	_, err = api.Update(ctx, r.workspace, &v1.Provider{
		ID:              cur.ID,
		Name:            p.Name,
		Type:            p.Type,
		ResourceVersion: cur.ResourceVersion,
		Spec:            v1.ProviderSpec{Credentials: creds, Config: cfg},
	})
	if err != nil {
		r.failed(p, fmt.Errorf("update provider: %w", err), false)
		return
	}
	r.succeeded(p, events.ProviderUpdated, managed, !managed, credKeys, cfgKeys)
}

// splitKeys splits a comma-separated key list.
func splitKeys(s string) []string {
	var keys []string
	for _, k := range strings.Split(s, ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// succeeded records a sync that did action.
func (r *ProviderReconciler) succeeded(p config.OpenShellProvider, action string, managed, adopted bool, credKeys, cfgKeys []string) {
	now := r.now()
	r.mu.Lock()
	st := r.status[p.Name]
	recovered := st.LastError != ""
	st.State = ProviderSynced
	st.Managed, st.Adopted = managed, adopted
	st.CredentialKeys, st.ConfigKeys = credKeys, cfgKeys
	st.LastSync = &now
	st.LastAction = action
	st.LastError = ""
	if action != "unchanged" {
		st.LastChange = &now
	}
	r.mu.Unlock()

	providerSyncCounter.WithLabelValues(p.Name, action).Inc()
	providerSyncOK.WithLabelValues(p.Name).Set(1)
	switch {
	case action != "unchanged":
		r.logger.Info("synced OpenShell provider", "provider", p.Name, "type", p.Type, "action", action, "credential_keys", credKeys, "config_keys", cfgKeys, "adopted", adopted)
		r.bus.Publish(events.ProviderEvent(events.ProviderOutcome{Provider: p.Name, Action: action, OK: true}))
	case recovered:
		r.logger.Info("OpenShell provider is in sync again", "provider", p.Name)
	}
}

// failed records a failed sync. The error is logged and published when it
// differs from the last one, so a lasting failure is not repeated every
// interval.
func (r *ProviderReconciler) failed(p config.OpenShellProvider, err error, conflict bool) {
	msg := err.Error()
	r.mu.Lock()
	st := r.status[p.Name]
	repeated := st.LastError == msg
	st.LastError = msg
	st.State = ProviderFailed
	if conflict {
		st.State = ProviderConflict
		st.Managed, st.Adopted = false, false
	}
	r.mu.Unlock()

	providerSyncCounter.WithLabelValues(p.Name, events.ProviderFailed).Inc()
	providerSyncOK.WithLabelValues(p.Name).Set(0)
	if repeated {
		r.logger.Debug("OpenShell provider still failing to sync", "provider", p.Name, "err", msg)
		return
	}
	r.logger.Error("cannot sync OpenShell provider", "provider", p.Name, "type", p.Type, "err", msg)
	r.bus.Publish(events.ProviderEvent(events.ProviderOutcome{Provider: p.Name, Action: events.ProviderFailed, Error: msg}))
}

// reviewUndeclared finds the providers Sundae created that are no longer
// declared, and deletes them with openshell.prune_providers. A provider
// still attached to a sandbox cannot be deleted: that is retried every
// round.
func (r *ProviderReconciler) reviewUndeclared(ctx context.Context, api ProviderAPI) {
	all, err := api.ListAll(ctx, r.workspace)
	if err != nil {
		r.logger.Warn("cannot list OpenShell providers to find undeclared ones", "err", err)
		return
	}
	seen := map[string]bool{}
	for _, gp := range all {
		if gp == nil || gp.Labels[LabelManaged] != "true" {
			continue
		}
		if _, ok := r.status[gp.Name]; ok {
			continue
		}
		seen[gp.Name] = true
		r.mu.Lock()
		st, ok := r.undeclared[gp.Name]
		if !ok {
			st = &ProviderStatus{Name: gp.Name, Type: gp.Type, State: ProviderUndeclared, Managed: true, Profiles: r.profiles[gp.Name]}
			r.undeclared[gp.Name] = st
		}
		r.mu.Unlock()
		if !ok && !r.prune {
			r.logger.Warn("OpenShell provider created by Sundae is no longer declared; keeping it (openshell.prune_providers is off)", "provider", gp.Name)
		}
		if !r.prune {
			continue
		}
		_, err := api.Delete(ctx, r.workspace, gp.Name, v1.DeleteOptions{AllowMissing: true})
		if err != nil {
			msg := fmt.Sprintf("delete provider: %v", err)
			r.mu.Lock()
			repeated := st.LastError == msg
			st.LastError = msg
			r.mu.Unlock()
			providerSyncCounter.WithLabelValues(gp.Name, events.ProviderFailed).Inc()
			if !repeated {
				r.logger.Error("cannot prune OpenShell provider", "provider", gp.Name, "err", msg)
				r.bus.Publish(events.ProviderEvent(events.ProviderOutcome{Provider: gp.Name, Action: events.ProviderFailed, Error: msg}))
			}
			continue
		}
		r.mu.Lock()
		delete(r.undeclared, gp.Name)
		r.mu.Unlock()
		delete(seen, gp.Name)
		providerSyncCounter.WithLabelValues(gp.Name, events.ProviderDeleted).Inc()
		r.logger.Info("pruned OpenShell provider that is no longer declared", "provider", gp.Name)
		r.bus.Publish(events.ProviderEvent(events.ProviderOutcome{Provider: gp.Name, Action: events.ProviderDeleted, OK: true}))
	}
	r.mu.Lock()
	for name := range r.undeclared {
		if !seen[name] {
			delete(r.undeclared, name)
		}
	}
	r.mu.Unlock()
}

// Status returns what is known of the declared providers, in declaration
// order, then of the undeclared ones Sundae created, by name.
func (r *ProviderReconciler) Status() []ProviderStatus {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ProviderStatus, 0, len(r.declared)+len(r.undeclared))
	for _, p := range r.declared {
		out = append(out, cloneStatus(r.status[p.Name]))
	}
	for _, name := range slices.Sorted(maps.Keys(r.undeclared)) {
		out = append(out, cloneStatus(r.undeclared[name]))
	}
	return out
}

func cloneStatus(st *ProviderStatus) ProviderStatus {
	c := *st
	c.CredentialKeys = slices.Clone(st.CredentialKeys)
	c.ConfigKeys = slices.Clone(st.ConfigKeys)
	c.Profiles = slices.Clone(st.Profiles)
	return c
}

// Declared reports whether name is declared in openshell.providers.
func (r *ProviderReconciler) Declared(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.status[name]
	return ok
}

// Check returns a *ProviderSyncError for the first of providers that is
// declared and whose last sync failed, or nil. Providers not declared, or
// not synced yet, pass.
func (r *ProviderReconciler) Check(providers []string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range providers {
		st, ok := r.status[name]
		if ok && (st.State == ProviderFailed || st.State == ProviderConflict) {
			return &ProviderSyncError{Provider: name, State: st.State, Err: st.LastError}
		}
	}
	return nil
}

// ProviderReconcilerContextKey is the key for the ProviderReconciler in a
// context.
var ProviderReconcilerContextKey = &struct{ string }{"openshell-provider-reconciler"}

// ProviderReconcilerFromContext returns the ProviderReconciler from a
// context, or nil when there is none. A nil *ProviderReconciler is safe to
// use.
func ProviderReconcilerFromContext(ctx context.Context) *ProviderReconciler {
	if r, ok := ctx.Value(ProviderReconcilerContextKey).(*ProviderReconciler); ok {
		return r
	}
	return nil
}

// ContextWithProviderReconciler returns a new context with the
// ProviderReconciler attached.
func ContextWithProviderReconciler(ctx context.Context, r *ProviderReconciler) context.Context {
	return context.WithValue(ctx, ProviderReconcilerContextKey, r)
}

// ProviderReport lists every provider Sundae knows of: those declared in
// openshell.providers, with what r knows of them (pending when r is nil),
// then the undeclared ones Sundae created, then those the profiles list
// that are not declared, which Sundae attaches by name but does not manage.
func ProviderReport(cfg *config.Config, r *ProviderReconciler) []ProviderStatus {
	byProvider := ProfilesByProvider(cfg.Sessions)
	out := r.Status()
	if r == nil {
		for _, p := range cfg.OpenShell.Providers {
			out = append(out, ProviderStatus{Name: p.Name, Type: p.Type, Declared: true, Adopt: p.Adopt, State: ProviderPending, Profiles: byProvider[p.Name]})
		}
	}
	known := map[string]bool{}
	for _, st := range out {
		known[st.Name] = true
	}
	for _, name := range slices.Sorted(maps.Keys(byProvider)) {
		if !known[name] {
			out = append(out, ProviderStatus{Name: name, State: ProviderManual, Profiles: byProvider[name]})
		}
	}
	return out
}
