package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
)

// writeSecretVolume lays dir out the way the kubelet mounts a Secret: the
// files live in a timestamped "..<time>" directory, "..data" links to it
// and each key is a link through "..data". Calling it again rotates the
// Secret the way the kubelet does, by swapping the ..data link.
func writeSecretVolume(t *testing.T, dir string, data map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ts := "..2026_11_13_12_00_00." + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	if err := os.Mkdir(filepath.Join(dir, ts), 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range data {
		if err := os.WriteFile(filepath.Join(dir, ts, k), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tmp := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(ts, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
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

func TestReadCredentialsDirSecretVolume(t *testing.T) {
	is := is.New(t)
	dir := filepath.Join(t.TempDir(), "my-github")
	writeSecretVolume(t, dir, map[string]string{"GITHUB_TOKEN": "ghp_one\n", "GH_HOST": "github.example.com"})

	creds, err := ReadCredentialsDir(dir)
	is.NoErr(err)
	// One trailing newline is trimmed; the ..data internals are skipped.
	is.Equal(creds, map[string]string{"GITHUB_TOKEN": "ghp_one", "GH_HOST": "github.example.com"})

	// The kubelet rotates the Secret by swapping ..data.
	writeSecretVolume(t, dir, map[string]string{"GITHUB_TOKEN": "ghp_two\n\n", "GH_HOST": "github.example.com"})
	creds, err = ReadCredentialsDir(dir)
	is.NoErr(err)
	is.Equal(creds["GITHUB_TOKEN"], "ghp_two\n") // only one newline is trimmed
}

func TestReadCredentialsDirErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"empty dir":     {map[string]string{}, "holds no credential files"},
		"empty value":   {map[string]string{"API_KEY": "\n"}, "API_KEY is empty"},
		"bad key":       {map[string]string{"api-key": "x"}, `file name "api-key" is the credential key and is not an environment variable name`},
		"digit key":     {map[string]string{"1KEY": "x"}, `file name "1KEY"`},
		"reserved key":  {map[string]string{"v10_TOKEN": "x"}, "v<digits>_ prefix"},
		"too large":     {map[string]string{"API_KEY": strings.Repeat("x", MaxProviderMapValueLen+1)}, "8192-byte limit"},
		"largest value": {map[string]string{"API_KEY": strings.Repeat("x", MaxProviderMapValueLen) + "\n"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			dir := t.TempDir()
			for k, v := range tc.files {
				is.NoErr(os.WriteFile(filepath.Join(dir, k), []byte(v), 0o600))
			}
			_, err := ReadCredentialsDir(dir)
			if tc.want == "" {
				is.NoErr(err)
				return
			}
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), tc.want)) // got: err
			// Never the value.
			is.True(!strings.Contains(err.Error(), "xxxx"))
		})
	}

	is := is.New(t)
	dir := t.TempDir()
	is.NoErr(os.Mkdir(filepath.Join(dir, "SUBDIR"), 0o755))
	_, err := ReadCredentialsDir(dir)
	is.True(err != nil && strings.Contains(err.Error(), "SUBDIR is not a regular file"))

	_, err = ReadCredentialsDir(filepath.Join(dir, "missing"))
	is.True(err != nil && strings.Contains(err.Error(), "read credentials_dir"))

	many := map[string]string{}
	for i := range MaxProviderCredentials + 1 {
		many["K"+strings.Repeat("X", i)] = "v"
	}
	dir = t.TempDir()
	for k, v := range many {
		is.NoErr(os.WriteFile(filepath.Join(dir, k), []byte(v), 0o600))
	}
	_, err = ReadCredentialsDir(dir)
	is.True(err != nil && strings.Contains(err.Error(), "holds 33 credentials; OpenShell keeps at most 32"))
}

// enabledOpenShellConfig returns a valid config with OpenShell enabled.
func enabledOpenShellConfig(t *testing.T) *Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.OpenShell.Enabled = true
	cfg.OpenShell.GatewayAddress = "http://gateway:8080"
	cfg.OpenShell.Image = "sandbox:latest"
	return cfg
}

func TestValidateProviders(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(c *Config, credDir string)
		want   []string
	}{
		"valid": {func(c *Config, d string) {
			c.OpenShell.Providers = []OpenShellProvider{
				{Name: "my-github", Type: "github", CredentialsDir: d},
				{Name: "pypi", Type: "pypi"},
				{Name: "my-claude", Type: "anthropic", CredentialsDir: d, Config: map[string]string{"ANTHROPIC_BASE_URL": "https://api.anthropic.com"}, Adopt: true},
			}
		}, nil},
		"no name": {func(c *Config, d string) {
			c.OpenShell.Providers = []OpenShellProvider{{Type: "github", CredentialsDir: d}}
		}, []string{"openshell.providers[0]: name is required"}},
		"bad name": {func(c *Config, d string) {
			c.OpenShell.Providers = []OpenShellProvider{{Name: "a,b", Type: "github", CredentialsDir: d}}
		}, []string{`openshell.providers[0] (a,b): name "a,b" is not a valid OpenShell provider name`}},
		"duplicate": {func(c *Config, d string) {
			c.OpenShell.Providers = []OpenShellProvider{{Name: "gh", Type: "github", CredentialsDir: d}, {Name: " gh ", Type: "github", CredentialsDir: d}}
		}, []string{`openshell.providers[1] (gh): name "gh" is already used by openshell.providers[0]`}},
		"no type": {func(c *Config, d string) {
			c.OpenShell.Providers = []OpenShellProvider{{Name: "gh", CredentialsDir: d}}
		}, []string{"openshell.providers[0] (gh): type is required"}},
		"bad type": {func(c *Config, d string) {
			c.OpenShell.Providers = []OpenShellProvider{{Name: "gh", Type: "git hub", CredentialsDir: d}, {Name: "x", Type: strings.Repeat("t", MaxProviderTypeLen+1)}}
		}, []string{`(gh): type "git hub" is not a valid`, `(x): type "ttt`}},
		"missing dir": {func(c *Config, d string) {
			c.OpenShell.Providers = []OpenShellProvider{{Name: "gh", Type: "github", CredentialsDir: "nope"}}
		}, []string{"openshell.providers[0] (gh): read credentials_dir", "mount the Secret"}},
		"empty dir": {func(c *Config, d string) {
			empty := filepath.Join(c.DataPath, "empty")
			_ = os.Mkdir(empty, 0o755)
			c.OpenShell.Providers = []OpenShellProvider{{Name: "gh", Type: "github", CredentialsDir: empty}}
		}, []string{"(gh): credentials_dir " + "", "holds no credential files"}},
		"config keys": {func(c *Config, d string) {
			c.OpenShell.Providers = []OpenShellProvider{{Name: "gh", Type: "github", CredentialsDir: d, Config: map[string]string{
				"sundae.hash": "x", "A=B": "x", "EMPTY": "", "MULTI": "a\nb", "OK": "fine",
			}}}
		}, []string{
			`(gh): config key "sundae.hash" uses the "sundae." prefix`,
			`(gh): config key "A=B" must not contain '='`,
			"(gh): config.EMPTY is empty",
			"(gh): config.MULTI must be one line",
		}},
		"sync too short": {
			func(c *Config, d string) { c.OpenShell.ProvidersSync = 5 * time.Second },
			[]string{"openshell.providers_sync 5s is too short: use at least 10s"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			cfg := enabledOpenShellConfig(t)
			d := filepath.Join(cfg.DataPath, "creds")
			writeSecretVolume(t, d, map[string]string{"TOKEN": "secret-value\n"})
			tc.mutate(cfg, d)
			err := cfg.Validate()
			if len(tc.want) == 0 {
				is.NoErr(err)
				return
			}
			is.True(err != nil)
			for _, w := range tc.want {
				is.True(strings.Contains(err.Error(), w)) // missing: w
			}
			is.True(!strings.Contains(err.Error(), "secret-value"))
		})
	}
}

func TestValidateProvidersDefaults(t *testing.T) {
	is := is.New(t)
	// With OpenShell off, the files are not read, but the rest is checked
	// and relative paths are resolved.
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.OpenShell.ProvidersSync = 0
	cfg.OpenShell.Providers = []OpenShellProvider{{Name: " gh ", Type: " github ", CredentialsDir: "providers/gh"}}
	is.NoErr(cfg.Validate())
	is.Equal(cfg.OpenShell.ProvidersSync, DefaultProvidersSync)
	is.Equal(cfg.OpenShell.Providers[0], OpenShellProvider{Name: "gh", Type: "github", CredentialsDir: filepath.Join(cfg.DataPath, "providers/gh")})
	p, ok := cfg.OpenShell.DeclaredProvider("gh")
	is.True(ok)
	is.Equal(p.Type, "github")
	_, ok = cfg.OpenShell.DeclaredProvider("other")
	is.True(!ok)
}

// TestProvidersEnvironRoundTrip checks that hook subprocesses, which
// rebuild the config from Environ, see the same providers, and never a
// credential.
func TestProvidersEnvironRoundTrip(t *testing.T) {
	is := is.New(t)
	cfg := enabledOpenShellConfig(t)
	d := filepath.Join(cfg.DataPath, "creds")
	writeSecretVolume(t, d, map[string]string{"TOKEN": "secret-value\n"})
	cfg.OpenShell.ProvidersSync = 90 * time.Second
	cfg.OpenShell.PruneProviders = true
	cfg.OpenShell.Providers = []OpenShellProvider{
		{Name: "my-claude", Type: "anthropic", CredentialsDir: d, Config: map[string]string{"ANTHROPIC_BASE_URL": "https://api.example.com:8443/v1?a=b", "region": "eu"}, Adopt: true},
		{Name: "pypi", Type: "pypi"},
	}
	is.NoErr(cfg.Validate())

	environ := cfg.Environ()
	for _, e := range environ {
		is.True(!strings.Contains(e, "secret-value"))
	}
	for _, e := range environ {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	got := DefaultConfig()
	is.NoErr(got.ParseEnv())
	is.Equal(got.OpenShell.Providers, cfg.OpenShell.Providers)
	is.Equal(got.OpenShell.ProvidersSync, 90*time.Second)
	is.True(got.OpenShell.PruneProviders)
}

func TestProvidersConfigFileRoundTrip(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.OpenShell.ProvidersSync = 2 * time.Minute
	cfg.OpenShell.Providers = []OpenShellProvider{
		{Name: "my-claude", Type: "anthropic", CredentialsDir: "/etc/sundae-providers/my-claude", Config: map[string]string{"ANTHROPIC_BASE_URL": "https://api.anthropic.com", "x": `quote "me"`}, Adopt: true},
		{Name: "pypi", Type: "pypi"},
	}
	is.NoErr(cfg.Validate())
	is.NoErr(cfg.WriteConfig())

	got := &Config{DataPath: cfg.DataPath}
	is.NoErr(parseFile(got, cfg.ConfigPath()))
	is.Equal(got.OpenShell.Providers, cfg.OpenShell.Providers)
	is.Equal(got.OpenShell.ProvidersSync, 2*time.Minute)
	is.True(!got.OpenShell.PruneProviders)

	// The default file documents the section.
	cfg = DefaultConfig()
	cfg.DataPath = t.TempDir()
	is.NoErr(cfg.WriteConfig())
	b, err := os.ReadFile(cfg.ConfigPath())
	is.NoErr(err)
	is.True(strings.Contains(string(b), "#providers:\n  #  - name: my-github"))
	is.True(strings.Contains(string(b), `providers_sync: "1m0s"`))
}

func TestProvidersUnknownKeys(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	p := filepath.Join(td, "config.yaml")
	is.NoErr(os.WriteFile(p, []byte(`openshell:
  providers:
    - name: gh
      type: github
      credentials: {GITHUB_TOKEN: inline}
      credential_dir: /x
  provider_sync: 1m
`), 0o600))
	cfg := &Config{DataPath: td}
	err := parseFile(cfg, p)
	is.True(err != nil)
	for _, want := range []string{
		"line 5: unknown key openshell.providers[].credentials",
		"line 6: unknown key openshell.providers[].credential_dir",
		"line 7: unknown key openshell.provider_sync",
	} {
		is.True(strings.Contains(err.Error(), want)) // missing: want
	}
	is.True(!strings.Contains(err.Error(), "inline"))
}
