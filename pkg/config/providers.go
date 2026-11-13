package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// OpenShell provider declarations (openshell.providers).
const (
	// DefaultProvidersSync is how often declared providers are synced to
	// the gateway when openshell.providers_sync is not set.
	DefaultProvidersSync = time.Minute
	// DefaultReadyTimeout is how long a session start waits for its
	// sandbox to become ready when openshell.ready_timeout is unset. The
	// first start of a large image pulls it and seeds the workspace
	// volume from its /sandbox, which can take several minutes.
	DefaultReadyTimeout = 15 * time.Minute
	// MinReadyTimeout is the shortest openshell.ready_timeout accepted.
	MinReadyTimeout = 30 * time.Second
	// MinProvidersSync is the shortest openshell.providers_sync accepted.
	MinProvidersSync = 10 * time.Second

	// MaxProviderTypeLen is the longest provider type, in bytes
	// (openshell-server 0.1.8, grpc/mod.rs MAX_PROVIDER_TYPE_LEN).
	MaxProviderTypeLen = 64
	// MaxProviderCredentials is how many credentials a provider holds
	// (MAX_PROVIDER_CREDENTIALS_ENTRIES).
	MaxProviderCredentials = 32
	// MaxProviderConfigEntries is how many config entries a provider holds
	// (MAX_PROVIDER_CONFIG_ENTRIES), Sundae's own included.
	MaxProviderConfigEntries = 64
	// MaxProviderMapKeyLen and MaxProviderMapValueLen bound a credential
	// or config key and value, in bytes (MAX_MAP_KEY_LEN, MAX_MAP_VALUE_LEN).
	MaxProviderMapKeyLen   = 256
	MaxProviderMapValueLen = 8192

	// ProviderConfigReserved prefixes the provider config keys Sundae
	// keeps its sync state in; a declared config key may not use it.
	ProviderConfigReserved = "sundae."
	// ProviderConfigReservedEntries is how many config entries Sundae
	// keeps on each provider it syncs.
	ProviderConfigReservedEntries = 3
)

// OpenShellProvider declares an OpenShell provider that Sundae creates on
// the gateway and keeps up to date (see openshell.providers). Its
// credentials are only ever read from files, so the config file can live in
// a ConfigMap or in git.
type OpenShellProvider struct {
	// Name is the provider's name on the gateway, which profiles list in
	// sessions.profiles.<name>.providers.
	Name string `env:"NAME" yaml:"name"`

	// Type is the provider type: the ID of a provider profile the gateway
	// knows, such as anthropic, github or an imported one. It cannot be
	// changed once the provider exists.
	Type string `env:"TYPE" yaml:"type"`

	// CredentialsDir is a directory with one file per credential: the
	// file's name is the credential key (an environment variable name,
	// e.g. GITHUB_TOKEN), its content the value, less one trailing
	// newline. A Kubernetes Secret mounted as a volume is such a
	// directory; its "..data" entries are skipped. The files are read
	// again on every sync, so a rotated Secret reaches the gateway. Empty
	// for a provider type that needs no credentials.
	CredentialsDir string `env:"CREDENTIALS_DIR" yaml:"credentials_dir"`

	// Config is the provider's non-secret configuration, e.g.
	// ANTHROPIC_BASE_URL. Keys starting with "sundae." are reserved.
	Config map[string]string `env:"CONFIG" envKeyValSeparator:"=" envSeparator:"\n" yaml:"config"`

	// Adopt lets Sundae take over a provider of this name that already
	// exists on the gateway without Sundae's managed label, e.g. one made
	// with `openshell provider create`. Without it such a provider is left
	// alone and reported as a conflict.
	Adopt bool `env:"ADOPT" yaml:"adopt"`
}

// providerTypeOK reports whether t is a provider type the gateway can
// accept: not empty, at most MaxProviderTypeLen bytes, printable.
func providerTypeOK(t string) bool {
	if t == "" || len(t) > MaxProviderTypeLen || !utf8.ValidString(t) {
		return false
	}
	for _, r := range t {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// envKeyRe matches what OpenShell accepts as a credential key it can
// inject: an environment variable name (openshell-server 0.1.8,
// grpc/provider.rs is_valid_env_key).
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// revisionKeyRe matches the v<digits>_ prefix OpenShell reserves for
// revision-scoped placeholders.
var revisionKeyRe = regexp.MustCompile(`^v[0-9]+_`)

// ValidCredentialKey reports why key cannot be an OpenShell credential
// key, or nil when it can.
func ValidCredentialKey(key string) error {
	switch {
	case !envKeyRe.MatchString(key):
		return errors.New("is not an environment variable name (letters, digits and _, not starting with a digit)")
	case len(key) > MaxProviderMapKeyLen:
		return fmt.Errorf("is longer than %d bytes", MaxProviderMapKeyLen)
	case revisionKeyRe.MatchString(key):
		return errors.New("uses the v<digits>_ prefix OpenShell reserves for revision-scoped placeholders")
	}
	return nil
}

// validConfigKey reports why key cannot be a declared provider config key,
// or nil when it can. Keys travel to hook subprocesses as KEY=VALUE lines
// (see Environ).
func validConfigKey(key string) error {
	switch {
	case key == "":
		return errors.New("is empty")
	case len(key) > MaxProviderMapKeyLen:
		return fmt.Errorf("is longer than %d bytes", MaxProviderMapKeyLen)
	case strings.HasPrefix(key, ProviderConfigReserved):
		return fmt.Errorf("uses the %q prefix Sundae reserves for its sync state", ProviderConfigReserved)
	case !utf8.ValidString(key) || strings.ContainsFunc(key, func(r rune) bool {
		return r == '=' || unicode.IsSpace(r) || unicode.IsControl(r)
	}):
		return errors.New("must not contain '=', spaces or control characters")
	}
	return nil
}

// validConfigValue reports why v cannot be a provider config value, or nil.
// OpenShell deletes a key updated to "", so a value cannot be empty.
func validConfigValue(v string) error {
	switch {
	case v == "":
		return errors.New("is empty (OpenShell treats an empty value as a deletion)")
	case len(v) > MaxProviderMapValueLen:
		return fmt.Errorf("is longer than %d bytes", MaxProviderMapValueLen)
	case strings.ContainsAny(v, "\r\n"):
		return errors.New("must be one line")
	}
	return nil
}

// ReadCredentials reads the provider's credentials from CredentialsDir:
// one per file, keyed by file name. It returns none when CredentialsDir is
// empty. Entries whose names start with ".." (a Kubernetes Secret volume's
// ..data link and ..<timestamp> directory) are skipped, and symbolic links
// are followed. An unreadable directory, an empty one, a file that is not a
// regular file, an invalid key or an empty or oversized value is an error
// naming the file, never its content.
func (p OpenShellProvider) ReadCredentials() (map[string]string, error) {
	if p.CredentialsDir == "" {
		return nil, nil
	}
	return ReadCredentialsDir(p.CredentialsDir)
}

// ReadCredentialsDir reads a directory of credential files; see
// OpenShellProvider.ReadCredentials.
func ReadCredentialsDir(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read credentials_dir: %w", err)
	}
	creds := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "..") {
			continue
		}
		path := filepath.Join(dir, name)
		fi, err := os.Stat(path) // follows the Secret volume's links
		if err != nil {
			return nil, fmt.Errorf("credentials_dir %s: %w", dir, err)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("credentials_dir %s: %s is not a regular file", dir, name)
		}
		if err := ValidCredentialKey(name); err != nil {
			return nil, fmt.Errorf("credentials_dir %s: file name %q is the credential key and %v", dir, name, err)
		}
		if fi.Size() > MaxProviderMapValueLen+2 {
			return nil, fmt.Errorf("credentials_dir %s: %s is larger than OpenShell's %d-byte limit", dir, name, MaxProviderMapValueLen)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("credentials_dir %s: %w", dir, err)
		}
		v := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
		switch {
		case v == "":
			return nil, fmt.Errorf("credentials_dir %s: %s is empty", dir, name)
		case len(v) > MaxProviderMapValueLen:
			return nil, fmt.Errorf("credentials_dir %s: %s is larger than OpenShell's %d-byte limit", dir, name, MaxProviderMapValueLen)
		}
		creds[name] = v
	}
	switch {
	case len(creds) == 0:
		return nil, fmt.Errorf("credentials_dir %s holds no credential files", dir)
	case len(creds) > MaxProviderCredentials:
		return nil, fmt.Errorf("credentials_dir %s holds %d credentials; OpenShell keeps at most %d per provider", dir, len(creds), MaxProviderCredentials)
	}
	return creds, nil
}

// DeclaredProvider returns the declared provider named name.
func (oc OpenShellConfig) DeclaredProvider(name string) (OpenShellProvider, bool) {
	for _, p := range oc.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return OpenShellProvider{}, false
}

// validateProviders validates openshell.providers and
// openshell.providers_sync, fills in defaults and resolves relative
// credentials_dir paths against the data path. The credential files are
// read only when OpenShell is enabled. Every problem is reported, each
// naming the provider and the key.
func (c *Config) validateProviders() error {
	oc := &c.OpenShell
	var errs []error
	switch {
	case oc.ProvidersSync == 0:
		oc.ProvidersSync = DefaultProvidersSync
	case oc.ProvidersSync < MinProvidersSync:
		errs = append(errs, fmt.Errorf("openshell.providers_sync %s is too short: use at least %s", oc.ProvidersSync, MinProvidersSync))
	}

	names := map[string]int{}
	for i := range oc.Providers {
		p := &oc.Providers[i]
		p.Name = strings.TrimSpace(p.Name)
		key := fmt.Sprintf("openshell.providers[%d]", i)
		if p.Name != "" {
			key = fmt.Sprintf("openshell.providers[%d] (%s)", i, p.Name)
		}
		fail := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("%s: "+format, append([]any{key}, args...)...))
		}

		switch {
		case p.Name == "":
			fail("name is required")
		case !validProviderName(p.Name):
			fail("name %q is not a valid OpenShell provider name: 1 to %d bytes without spaces, commas or control characters", p.Name, MaxProviderNameLen)
		default:
			if j, ok := names[p.Name]; ok {
				fail("name %q is already used by openshell.providers[%d]", p.Name, j)
			} else {
				names[p.Name] = i
			}
		}

		p.Type = strings.TrimSpace(p.Type)
		switch {
		case p.Type == "":
			fail("type is required: the provider profile ID, e.g. anthropic or github (openshell profile list)")
		case !providerTypeOK(p.Type):
			fail("type %q is not a valid OpenShell provider type: 1 to %d bytes without spaces or control characters", p.Type, MaxProviderTypeLen)
		}

		if limit := MaxProviderConfigEntries - ProviderConfigReservedEntries; len(p.Config) > limit {
			fail("config has %d entries; OpenShell keeps %d per provider, %d of them Sundae's", len(p.Config), MaxProviderConfigEntries, ProviderConfigReservedEntries)
		}
		keys := make([]string, 0, len(p.Config))
		for k := range p.Config {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if err := validConfigKey(k); err != nil {
				fail("config key %q %v", k, err)
			} else if err := validConfigValue(p.Config[k]); err != nil {
				fail("config.%s %v", k, err)
			}
		}

		p.CredentialsDir = strings.TrimSpace(p.CredentialsDir)
		if p.CredentialsDir != "" && !filepath.IsAbs(p.CredentialsDir) {
			p.CredentialsDir = filepath.Join(c.DataPath, p.CredentialsDir)
		}
		// Read the credentials once now so a missing, empty or malformed
		// directory fails at startup. The values are not kept: the
		// reconciler reads the files again on every sync.
		if oc.Enabled && p.CredentialsDir != "" {
			if _, err := p.ReadCredentials(); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					fail("%v (mount the Secret holding the credentials there)", err)
				} else {
					fail("%v", err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// providersEnviron returns the declared providers as
// SUNDAE_OPENSHELL_PROVIDERS_<i>_* environment variables. Credential values
// are never included, only the directory.
func (c *Config) providersEnviron() []string {
	envs := []string{
		"SUNDAE_OPENSHELL_PROVIDERS_SYNC=" + c.OpenShell.ProvidersSync.String(),
		fmt.Sprintf("SUNDAE_OPENSHELL_PRUNE_PROVIDERS=%t", c.OpenShell.PruneProviders),
	}
	for i, p := range c.OpenShell.Providers {
		pre := fmt.Sprintf("SUNDAE_OPENSHELL_PROVIDERS_%d_", i)
		keys := make([]string, 0, len(p.Config))
		for k := range p.Config {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		lines := make([]string, 0, len(keys))
		for _, k := range keys {
			lines = append(lines, k+"="+p.Config[k])
		}
		envs = append(envs,
			pre+"NAME="+p.Name,
			pre+"TYPE="+p.Type,
			pre+"CREDENTIALS_DIR="+p.CredentialsDir,
			pre+"CONFIG="+strings.Join(lines, "\n"),
			fmt.Sprintf("%sADOPT=%t", pre, p.Adopt),
		)
	}
	return envs
}
