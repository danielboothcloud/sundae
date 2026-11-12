package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/matryer/is"
)

func TestParseMultipleKeys(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	is.NoErr(os.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", "testdata/k1.pub\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8 a@b"))
	is.NoErr(os.Setenv("SOFT_SERVE_DATA_PATH", td))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_INITIAL_ADMIN_KEYS"))
		is.NoErr(os.Unsetenv("SOFT_SERVE_DATA_PATH"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.InitialAdminKeys, []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8",
	})
}

func TestMergeInitAdminKeys(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", "testdata/k1.pub"))
	t.Cleanup(func() { is.NoErr(os.Unsetenv("SOFT_SERVE_INITIAL_ADMIN_KEYS")) })
	cfg := &Config{
		DataPath:         t.TempDir(),
		InitialAdminKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8 a@b"},
	}
	is.NoErr(cfg.WriteConfig())
	is.NoErr(cfg.Parse())
	is.Equal(cfg.InitialAdminKeys, []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFxIobhwtfdwN7m1TFt9wx3PsfvcAkISGPxmbmbauST8",
	})
}

func TestValidateInitAdminKeys(t *testing.T) {
	is := is.New(t)
	cfg := &Config{
		DataPath: t.TempDir(),
		InitialAdminKeys: []string{
			"testdata/k1.pub",
			"abc",
			"",
		},
	}
	is.NoErr(cfg.WriteConfig())
	is.NoErr(cfg.Parse())
	is.Equal(cfg.InitialAdminKeys, []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINMwLvyV3ouVrTysUYGoJdl5Vgn5BACKov+n9PlzfPwH",
	})
}

func TestCustomConfigLocation(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_CONFIG_LOCATION"))
	})

	// Test that we get data from the custom file location, and not from the data dir.
	is.NoErr(os.Setenv("SOFT_SERVE_CONFIG_LOCATION", "testdata/config.yaml"))
	is.NoErr(os.Setenv("SOFT_SERVE_DATA_PATH", td))
	cfg := DefaultConfig()
	is.NoErr(cfg.Parse())
	is.Equal(cfg.Name, "Test server name")
	// If we unset the custom location, then use the default location.
	is.NoErr(os.Unsetenv("SOFT_SERVE_CONFIG_LOCATION"))
	cfg = DefaultConfig()
	is.Equal(cfg.Name, "Soft Serve")
	// Test that if the custom config location doesn't exist, default to datapath config.
	is.NoErr(os.Setenv("SOFT_SERVE_CONFIG_LOCATION", "testdata/config_nonexistent.yaml"))
	cfg = DefaultConfig()
	is.Equal(cfg.Name, "Soft Serve")
}

func TestDefaultRepoDisabledByDefault(t *testing.T) {
	is := is.New(t)
	is.Equal(DefaultConfig().DefaultRepo, "")
}

func TestParseDefaultRepo(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_DEFAULT_REPO", "gitops"))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_DEFAULT_REPO"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.DefaultRepo, "gitops")
}

func TestParseDefaultRepoFromFile(t *testing.T) {
	is := is.New(t)
	cfg := &Config{DataPath: t.TempDir()}
	is.NoErr(parseFile(cfg, "testdata/config_default_repo.yaml"))
	is.Equal(cfg.DefaultRepo, "gitops")
}

func TestParseMultipleHeaders(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_HTTP_CORS_ALLOWED_HEADERS", "Accept,Accept-Language,User-Agent"))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_HTTP_CORS_ALLOWED_HEADERS"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.HTTP.CORS.AllowedHeaders, []string{
		"Accept",
		"Accept-Language",
		"User-Agent",
	})
}

func TestParseMultipleOrigins(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS", "http://example.com,https://example.com"))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.HTTP.CORS.AllowedOrigins, []string{
		"http://localhost:23232",
		"http://example.com",
		"https://example.com",
	})
}

func TestParseMultipleMethods(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_HTTP_CORS_ALLOWED_METHODS", "GET,POST,PUT"))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_HTTP_CORS_ALLOWED_METHODS"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.Equal(cfg.HTTP.CORS.AllowedMethods, []string{
		"GET",
		"POST",
		"PUT",
	})
}

func TestAnonAccessEnvUnsetByDefault(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	// nil is the "no override" sentinel.
	is.True(cfg.AnonAccess == nil)
}

func TestParseAnonAccessEnv(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_ANON_ACCESS", access.AdminAccess.String()))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_ANON_ACCESS"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.True(cfg.AnonAccess != nil)
	is.Equal(*cfg.AnonAccess, access.AdminAccess)
}

// An invalid anon-access level is rejected at parse time by AccessLevel's
// TextUnmarshaler, so it never reaches Validate as a bad value.
func TestParseRejectsInvalidAnonAccess(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_ANON_ACCESS", "not-a-real-access-level"))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_ANON_ACCESS"))
	})
	cfg := DefaultConfig()
	err := cfg.ParseEnv()
	is.True(err != nil)
}

func TestAllowKeylessEnvUnsetByDefault(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	// nil is the "no override" sentinel — distinct from an explicit false.
	is.True(cfg.AllowKeyless == nil)
}

func TestParseAllowKeylessEnvTrue(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_ALLOW_KEYLESS", "true"))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_ALLOW_KEYLESS"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	is.True(cfg.AllowKeyless != nil)
	is.Equal(*cfg.AllowKeyless, true)
}

func TestParseAllowKeylessEnvFalse(t *testing.T) {
	is := is.New(t)
	is.NoErr(os.Setenv("SOFT_SERVE_ALLOW_KEYLESS", "false"))
	t.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_ALLOW_KEYLESS"))
	})
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	// Explicit false must survive as a real override, not collapse back to
	// "unset" — that's the whole reason this field is a *bool.
	is.True(cfg.AllowKeyless != nil)
	is.Equal(*cfg.AllowKeyless, false)
}

func TestOpenShellDisabledByDefault(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	is.Equal(cfg.OpenShell.Enabled, false)
	is.Equal(cfg.OpenShell.Workspace, "default")
	is.Equal(cfg.OpenShell.TokenTTL, 24*time.Hour)
	is.Equal(cfg.OpenShell.Command, "bash -l")
	// Disabled needs no address or image.
	cfg.DataPath = t.TempDir()
	is.NoErr(cfg.Validate())
}

func TestParseOpenShellEnv(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	envs := map[string]string{
		"SOFT_SERVE_DATA_PATH":                 td,
		"SOFT_SERVE_OPENSHELL_ENABLED":         "true",
		"SOFT_SERVE_OPENSHELL_GATEWAY_ADDRESS": "https://openshell.example:8443",
		"SOFT_SERVE_OPENSHELL_WORKSPACE":       "agents",
		"SOFT_SERVE_OPENSHELL_IMAGE":           "ghcr.io/example/sandbox:1",
		"SOFT_SERVE_OPENSHELL_TOKEN":           "gw-secret",
		"SOFT_SERVE_OPENSHELL_TLS_CA":          "/tls/ca.crt",
		"SOFT_SERVE_OPENSHELL_TLS_CERT":        "tls/tls.crt",
		"SOFT_SERVE_OPENSHELL_TLS_KEY":         "tls/tls.key",
		"SOFT_SERVE_OPENSHELL_INSECURE":        "true",
		"SOFT_SERVE_OPENSHELL_CLONE_URL":       "http://soft-serve.soft-serve.svc:23232/",
		"SOFT_SERVE_OPENSHELL_TOKEN_TTL":       "2h",
		"SOFT_SERVE_OPENSHELL_COMMAND":         "claude",
	}
	for k, v := range envs {
		t.Setenv(k, v)
	}
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	oc := cfg.OpenShell
	is.True(oc.Enabled)
	is.Equal(oc.GatewayAddress, "https://openshell.example:8443")
	is.Equal(oc.Workspace, "agents")
	is.Equal(oc.Image, "ghcr.io/example/sandbox:1")
	is.Equal(oc.Token, "gw-secret")
	is.Equal(oc.TLSCA, "/tls/ca.crt")
	is.Equal(oc.TLSCert, filepath.Join(td, "tls/tls.crt"))
	is.Equal(oc.TLSKey, filepath.Join(td, "tls/tls.key"))
	is.True(oc.Insecure)
	is.Equal(oc.CloneURL, "http://soft-serve.soft-serve.svc:23232")
	is.Equal(cfg.OpenShellCloneURL(), "http://soft-serve.soft-serve.svc:23232")
	is.Equal(oc.TokenTTL, 2*time.Hour)
	is.Equal(oc.Command, "claude")

	// The gateway token never leaks into subprocess environments.
	for _, e := range cfg.Environ() {
		is.True(!strings.HasPrefix(e, "SOFT_SERVE_OPENSHELL_TOKEN="))
	}
}

func TestOpenShellCloneURLDefaultsToPublicURL(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.HTTP.PublicURL = "https://git.example.com/"
	is.Equal(cfg.OpenShellCloneURL(), "https://git.example.com")
}

func TestValidateOpenShell(t *testing.T) {
	valid := func() *Config {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		cfg.OpenShell.Enabled = true
		cfg.OpenShell.GatewayAddress = "openshell:8443"
		cfg.OpenShell.Image = "sandbox:latest"
		return cfg
	}
	cases := map[string]struct {
		mutate func(*Config)
		err    string
	}{
		"valid":            {mutate: func(*Config) {}},
		"missing address":  {mutate: func(c *Config) { c.OpenShell.GatewayAddress = "" }, err: "gateway_address"},
		"missing image":    {mutate: func(c *Config) { c.OpenShell.Image = "" }, err: "image"},
		"cert without key": {mutate: func(c *Config) { c.OpenShell.TLSCert = "c.crt" }, err: "tls_cert and tls_key"},
		"zero ttl":         {mutate: func(c *Config) { c.OpenShell.TokenTTL = 0 }, err: "token_ttl"},
		"empty command":    {mutate: func(c *Config) { c.OpenShell.Command = " " }, err: "command"},
		"blank workspace":  {mutate: func(c *Config) { c.OpenShell.Workspace = " " }, err: "workspace"},
		"disabled ignores missing fields": {mutate: func(c *Config) {
			c.OpenShell.Enabled = false
			c.OpenShell.GatewayAddress = ""
			c.OpenShell.Image = ""
		}},
		"token file": {mutate: func(c *Config) {
			c.OpenShell.TokenFile = writeTokenFile(t, c.DataPath, "gw-token", "gw-secret\n")
		}},
		"relative token file": {mutate: func(c *Config) {
			writeTokenFile(t, c.DataPath, "gw-token", "gw-secret\n")
			c.OpenShell.TokenFile = "gw-token"
		}},
		"token and token file": {mutate: func(c *Config) {
			c.OpenShell.Token = "gw-secret"
			c.OpenShell.TokenFile = writeTokenFile(t, c.DataPath, "gw-token", "gw-secret\n")
		}, err: "token and token_file are mutually exclusive"},
		"token and token file while disabled": {mutate: func(c *Config) {
			c.OpenShell.Enabled = false
			c.OpenShell.Token = "gw-secret"
			c.OpenShell.TokenFile = "gw-token"
		}, err: "token and token_file are mutually exclusive"},
		"missing token file": {mutate: func(c *Config) {
			c.OpenShell.TokenFile = "does-not-exist"
		}, err: "read token_file"},
		"unreadable token file": {mutate: func(c *Config) {
			// A directory exists but cannot be read as a file.
			c.OpenShell.TokenFile = c.DataPath
		}, err: "read token_file"},
		"empty token file": {mutate: func(c *Config) {
			c.OpenShell.TokenFile = writeTokenFile(t, c.DataPath, "gw-token", " \n")
		}, err: "is empty"},
		"disabled ignores missing token file": {mutate: func(c *Config) {
			c.OpenShell.Enabled = false
			c.OpenShell.TokenFile = "does-not-exist"
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			cfg := valid()
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.err == "" {
				is.NoErr(err)
				return
			}
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), tc.err))
		})
	}
}

func writeTokenFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseOpenShellTokenFileEnv(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	writeTokenFile(t, td, "gw-token", "gw-secret\r\n")
	envs := map[string]string{
		"SOFT_SERVE_DATA_PATH":                 td,
		"SOFT_SERVE_OPENSHELL_ENABLED":         "true",
		"SOFT_SERVE_OPENSHELL_GATEWAY_ADDRESS": "https://openshell.example:8080",
		"SOFT_SERVE_OPENSHELL_IMAGE":           "ghcr.io/example/sandbox:1",
		"SOFT_SERVE_OPENSHELL_TOKEN_FILE":      "gw-token",
	}
	for k, v := range envs {
		t.Setenv(k, v)
	}
	cfg := DefaultConfig()
	is.NoErr(cfg.ParseEnv())
	// Relative paths resolve against the data path, like the TLS files.
	is.Equal(cfg.OpenShell.TokenFile, filepath.Join(td, "gw-token"))
	// The token is not copied into the config.
	is.Equal(cfg.OpenShell.Token, "")
	token, err := cfg.OpenShell.GatewayToken()
	is.NoErr(err)
	is.Equal(token, "gw-secret")

	// The path is passed on; the token never is.
	environ := cfg.Environ()
	is.True(slices.Contains(environ, "SOFT_SERVE_OPENSHELL_TOKEN_FILE="+filepath.Join(td, "gw-token")))
	for _, e := range environ {
		is.True(!strings.HasPrefix(e, "SOFT_SERVE_OPENSHELL_TOKEN="))
	}
}

func TestOpenShellGatewayToken(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()

	// No token configured.
	token, err := OpenShellConfig{}.GatewayToken()
	is.NoErr(err)
	is.Equal(token, "")

	// Inline token.
	token, err = OpenShellConfig{Token: "inline"}.GatewayToken()
	is.NoErr(err)
	is.Equal(token, "inline")

	// Only trailing whitespace is trimmed.
	p := writeTokenFile(t, td, "gw-token", "  first \n\n")
	oc := OpenShellConfig{TokenFile: p}
	token, err = oc.GatewayToken()
	is.NoErr(err)
	is.Equal(token, "  first")

	// The file is read on every call, so a rotated token is picked up.
	writeTokenFile(t, td, "gw-token", "second\n")
	token, err = oc.GatewayToken()
	is.NoErr(err)
	is.Equal(token, "second")

	// Both set is refused here too, not only by Validate.
	_, err = OpenShellConfig{Token: "inline", TokenFile: p}.GatewayToken()
	is.True(err != nil)
}

func TestOpenShellConfigFileRoundTrip(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.OpenShell.Enabled = true
	cfg.OpenShell.GatewayAddress = "openshell:8443"
	cfg.OpenShell.Image = "sandbox:latest"
	cfg.OpenShell.TokenFile = writeTokenFile(t, cfg.DataPath, "gw-token", "gw-secret\n")
	cfg.OpenShell.TokenTTL = 90 * time.Minute
	cfg.OpenShell.Command = `bash -lc "echo hi"`
	is.NoErr(cfg.WriteConfig())

	got := &Config{DataPath: cfg.DataPath}
	is.NoErr(parseFile(got, cfg.ConfigPath()))
	is.True(got.OpenShell.Enabled)
	is.Equal(got.OpenShell.GatewayAddress, "openshell:8443")
	is.Equal(got.OpenShell.Image, "sandbox:latest")
	is.Equal(got.OpenShell.Workspace, "default")
	is.Equal(got.OpenShell.TokenTTL, 90*time.Minute)
	is.Equal(got.OpenShell.Command, `bash -lc "echo hi"`)
	is.Equal(got.OpenShell.TokenFile, filepath.Join(cfg.DataPath, "gw-token"))
	is.Equal(got.OpenShell.Token, "")
}
