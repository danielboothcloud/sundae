package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/caarlos0/env/v11"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

var binPath = "soft"

// SSHConfig is the configuration for the SSH server.
type SSHConfig struct {
	// Enabled toggles the SSH server on/off
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// ListenAddr is the address on which the SSH server will listen.
	ListenAddr string `env:"LISTEN_ADDR" yaml:"listen_addr"`

	// PublicURL is the public URL of the SSH server.
	PublicURL string `env:"PUBLIC_URL" yaml:"public_url"`

	// KeyPath is the path to the SSH server's private key.
	KeyPath string `env:"KEY_PATH" yaml:"key_path"`

	// ClientKeyPath is the path to the server's client private key.
	ClientKeyPath string `env:"CLIENT_KEY_PATH" yaml:"client_key_path"`

	// MaxTimeout is the maximum number of seconds a connection can take.
	MaxTimeout int `env:"MAX_TIMEOUT" yaml:"max_timeout"`

	// IdleTimeout is the number of seconds a connection can be idle before it is closed.
	IdleTimeout int `env:"IDLE_TIMEOUT" yaml:"idle_timeout"`
}

// GitConfig is the Git daemon configuration for the server.
type GitConfig struct {
	// Enabled toggles the Git daemon on/off
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// ListenAddr is the address on which the Git daemon will listen.
	ListenAddr string `env:"LISTEN_ADDR" yaml:"listen_addr"`

	// PublicURL is the public URL of the Git daemon server.
	PublicURL string `env:"PUBLIC_URL" yaml:"public_url"`

	// MaxTimeout is the maximum number of seconds a connection can take.
	MaxTimeout int `env:"MAX_TIMEOUT" yaml:"max_timeout"`

	// IdleTimeout is the number of seconds a connection can be idle before it is closed.
	IdleTimeout int `env:"IDLE_TIMEOUT" yaml:"idle_timeout"`

	// MaxConnections is the maximum number of concurrent connections.
	MaxConnections int `env:"MAX_CONNECTIONS" yaml:"max_connections"`
}

// CORSConfig is the CORS configuration for the server.
type CORSConfig struct {
	AllowedHeaders []string `env:"ALLOWED_HEADERS" yaml:"allowed_headers"`

	AllowedOrigins []string `env:"ALLOWED_ORIGINS" yaml:"allowed_origins"`

	AllowedMethods []string `env:"ALLOWED_METHODS" yaml:"allowed_methods"`
}

// HTTPConfig is the HTTP configuration for the server.
type HTTPConfig struct {
	// Enabled toggles the HTTP server on/off
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// ListenAddr is the address on which the HTTP server will listen.
	ListenAddr string `env:"LISTEN_ADDR" yaml:"listen_addr"`

	// TLSKeyPath is the path to the TLS private key.
	TLSKeyPath string `env:"TLS_KEY_PATH" yaml:"tls_key_path"`

	// TLSCertPath is the path to the TLS certificate.
	TLSCertPath string `env:"TLS_CERT_PATH" yaml:"tls_cert_path"`

	// PublicURL is the public URL of the HTTP server.
	PublicURL string `env:"PUBLIC_URL" yaml:"public_url"`

	// CORS is the cross-origin configuration for the HTTP server.
	CORS CORSConfig `envPrefix:"CORS_" yaml:"cors"`
}

// StatsConfig is the configuration for the stats server.
type StatsConfig struct {
	// Enabled toggles the Stats server on/off
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// ListenAddr is the address on which the stats server will listen.
	ListenAddr string `env:"LISTEN_ADDR" yaml:"listen_addr"`
}

// LogConfig is the logger configuration.
type LogConfig struct {
	// Format is the format of the logs.
	// Valid values are "json", "logfmt", and "text".
	Format string `env:"FORMAT" yaml:"format"`

	// Time format for the log `ts` field.
	// Format must be described in Golang's time format.
	TimeFormat string `env:"TIME_FORMAT" yaml:"time_format"`

	// Path to a file to write logs to.
	// If not set, logs will be written to stderr.
	Path string `env:"PATH" yaml:"path"`
}

// DBConfig is the database connection configuration.
type DBConfig struct {
	// Driver is the driver for the database.
	Driver string `env:"DRIVER" yaml:"driver"`

	// DataSource is the database data source name.
	DataSource string `env:"DATA_SOURCE" yaml:"data_source"`
}

// LFSConfig is the configuration for Git LFS.
type LFSConfig struct {
	// Enabled is whether or not Git LFS is enabled.
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// SSHEnabled is whether or not Git LFS over SSH is enabled.
	// This is only used if LFS is enabled.
	SSHEnabled bool `env:"SSH_ENABLED" yaml:"ssh_enabled"`
}

// JobsConfig is the configuration for cron jobs.
type JobsConfig struct {
	MirrorPull string `env:"MIRROR_PULL" yaml:"mirror_pull"`
}

// OpenShellConfig is the configuration for OpenShell agent sessions.
//
// When enabled, the SSH `session` command starts and resumes one OpenShell
// sandbox per (user, repository) through an OpenShell gateway.
type OpenShellConfig struct {
	// Enabled toggles OpenShell sessions on/off.
	Enabled bool `env:"ENABLED" yaml:"enabled"`

	// GatewayAddress is the OpenShell gateway gRPC address. An http:// scheme
	// selects plaintext; https:// or no scheme selects TLS.
	GatewayAddress string `env:"GATEWAY_ADDRESS" yaml:"gateway_address"`

	// Workspace is the OpenShell workspace sandboxes are created in.
	Workspace string `env:"WORKSPACE" yaml:"workspace"`

	// Image is the default sandbox base image.
	Image string `env:"IMAGE" yaml:"image"`

	// Token is an optional bearer token used to authenticate to the gateway.
	// Mutually exclusive with TokenFile.
	Token string `env:"TOKEN" yaml:"token"`

	// TokenFile is the path to a file holding the gateway bearer token, so
	// the token itself stays out of the config file. Trailing whitespace is
	// trimmed. The file is read again whenever a gateway client is created,
	// so a token rotated in place is picked up without a restart. Mutually
	// exclusive with Token.
	TokenFile string `env:"TOKEN_FILE" yaml:"token_file"`

	// TLSCA is the path to the CA certificate used to verify the gateway.
	TLSCA string `env:"TLS_CA" yaml:"tls_ca"`

	// TLSCert is the path to the client certificate (mTLS).
	TLSCert string `env:"TLS_CERT" yaml:"tls_cert"`

	// TLSKey is the path to the client private key (mTLS).
	TLSKey string `env:"TLS_KEY" yaml:"tls_key"`

	// Insecure skips verification of the gateway's TLS certificate.
	Insecure bool `env:"INSECURE" yaml:"insecure"`

	// CloneURL is the internal HTTP base URL sandboxes use to clone and push.
	// Defaults to http.public_url when empty.
	CloneURL string `env:"CLONE_URL" yaml:"clone_url"`

	// TokenTTL is the lifetime of the access token issued to a sandbox.
	TokenTTL time.Duration `env:"TOKEN_TTL" yaml:"token_ttl"`

	// Command is what runs in the sandbox when a user attaches.
	Command string `env:"COMMAND" yaml:"command"`
}

// Config is the configuration for Soft Serve.
type Config struct {
	// Name is the name of the server.
	Name string `env:"NAME" yaml:"name"`

	// SSH is the configuration for the SSH server.
	SSH SSHConfig `envPrefix:"SSH_" yaml:"ssh"`

	// Git is the configuration for the Git daemon.
	Git GitConfig `envPrefix:"GIT_" yaml:"git"`

	// HTTP is the configuration for the HTTP server.
	HTTP HTTPConfig `envPrefix:"HTTP_" yaml:"http"`

	// Stats is the configuration for the stats server.
	Stats StatsConfig `envPrefix:"STATS_" yaml:"stats"`

	// Log is the logger configuration.
	Log LogConfig `envPrefix:"LOG_" yaml:"log"`

	// DB is the database configuration.
	DB DBConfig `envPrefix:"DB_" yaml:"db"`

	// LFS is the configuration for Git LFS.
	LFS LFSConfig `envPrefix:"LFS_" yaml:"lfs"`

	// Jobs is the configuration for cron jobs
	Jobs JobsConfig `envPrefix:"JOBS_" yaml:"jobs"`

	// OpenShell is the configuration for OpenShell agent sessions.
	OpenShell OpenShellConfig `envPrefix:"OPENSHELL_" yaml:"openshell"`

	// InitialAdminKeys is a list of public keys that will be added to the list of admins.
	InitialAdminKeys []string `env:"INITIAL_ADMIN_KEYS" envSeparator:"\n" yaml:"initial_admin_keys"`

	// AnonAccess overrides the access level for anonymous users.
	//
	// If set (non-nil), this takes precedence over the "anon-access" value
	// stored in the database (see the `settings` command) on every read, for
	// as long as it remains set. It is not a one-time default: it behaves
	// like InitialAdminKeys, not like a seed value. Invalid values are
	// rejected at parse time by AccessLevel's TextUnmarshaler.
	//
	// This is intended for scripted, non-production bootstrapping only.
	AnonAccess *access.AccessLevel `env:"ANON_ACCESS" yaml:"anon_access"`

	// AllowKeyless overrides whether keyless (no public key) connections
	// are allowed.
	//
	// If set (non-nil), this takes precedence over the "allow-keyless"
	// value stored in the database on every read, same as AnonAccess above.
	// A nil value means "no override": fall back to the database.
	AllowKeyless *bool `env:"ALLOW_KEYLESS" yaml:"allow_keyless"`

	// DefaultRepo is a repository name to create on boot if it does not
	// already exist. Leave empty to disable.
	DefaultRepo string `env:"DEFAULT_REPO" yaml:"default_repo"`

	// DataPath is the path to the directory where Soft Serve will store its data.
	DataPath string `env:"DATA_PATH" yaml:"-"`
}

// Environ returns the config as a list of environment variables.
func (c *Config) Environ() []string {
	envs := []string{
		fmt.Sprintf("SOFT_SERVE_BIN_PATH=%s", binPath),
	}
	if c == nil {
		return envs
	}

	// TODO: do this dynamically
	envs = append(envs, []string{
		fmt.Sprintf("SOFT_SERVE_CONFIG_LOCATION=%s", c.ConfigPath()),
		fmt.Sprintf("SOFT_SERVE_DATA_PATH=%s", c.DataPath),
		fmt.Sprintf("SOFT_SERVE_NAME=%s", c.Name),
		fmt.Sprintf("SOFT_SERVE_INITIAL_ADMIN_KEYS=%s", strings.Join(c.InitialAdminKeys, "\n")),
		fmt.Sprintf("SOFT_SERVE_DEFAULT_REPO=%s", c.DefaultRepo),
		fmt.Sprintf("SOFT_SERVE_SSH_ENABLED=%t", c.SSH.Enabled),
		fmt.Sprintf("SOFT_SERVE_SSH_LISTEN_ADDR=%s", c.SSH.ListenAddr),
		fmt.Sprintf("SOFT_SERVE_SSH_PUBLIC_URL=%s", c.SSH.PublicURL),
		fmt.Sprintf("SOFT_SERVE_SSH_KEY_PATH=%s", c.SSH.KeyPath),
		fmt.Sprintf("SOFT_SERVE_SSH_CLIENT_KEY_PATH=%s", c.SSH.ClientKeyPath),
		fmt.Sprintf("SOFT_SERVE_SSH_MAX_TIMEOUT=%d", c.SSH.MaxTimeout),
		fmt.Sprintf("SOFT_SERVE_SSH_IDLE_TIMEOUT=%d", c.SSH.IdleTimeout),
		fmt.Sprintf("SOFT_SERVE_GIT_ENABLED=%t", c.Git.Enabled),
		fmt.Sprintf("SOFT_SERVE_GIT_LISTEN_ADDR=%s", c.Git.ListenAddr),
		fmt.Sprintf("SOFT_SERVE_GIT_PUBLIC_URL=%s", c.Git.PublicURL),
		fmt.Sprintf("SOFT_SERVE_GIT_MAX_TIMEOUT=%d", c.Git.MaxTimeout),
		fmt.Sprintf("SOFT_SERVE_GIT_IDLE_TIMEOUT=%d", c.Git.IdleTimeout),
		fmt.Sprintf("SOFT_SERVE_GIT_MAX_CONNECTIONS=%d", c.Git.MaxConnections),
		fmt.Sprintf("SOFT_SERVE_HTTP_ENABLED=%t", c.HTTP.Enabled),
		fmt.Sprintf("SOFT_SERVE_HTTP_LISTEN_ADDR=%s", c.HTTP.ListenAddr),
		fmt.Sprintf("SOFT_SERVE_HTTP_TLS_KEY_PATH=%s", c.HTTP.TLSKeyPath),
		fmt.Sprintf("SOFT_SERVE_HTTP_TLS_CERT_PATH=%s", c.HTTP.TLSCertPath),
		fmt.Sprintf("SOFT_SERVE_HTTP_PUBLIC_URL=%s", c.HTTP.PublicURL),
		fmt.Sprintf("SOFT_SERVE_HTTP_CORS_ALLOWED_HEADERS=%s", strings.Join(c.HTTP.CORS.AllowedHeaders, ",")),
		fmt.Sprintf("SOFT_SERVE_HTTP_CORS_ALLOWED_ORIGINS=%s", strings.Join(c.HTTP.CORS.AllowedOrigins, ",")),
		fmt.Sprintf("SOFT_SERVE_HTTP_CORS_ALLOWED_METHODS=%s", strings.Join(c.HTTP.CORS.AllowedMethods, ",")),
		fmt.Sprintf("SOFT_SERVE_STATS_ENABLED=%t", c.Stats.Enabled),
		fmt.Sprintf("SOFT_SERVE_STATS_LISTEN_ADDR=%s", c.Stats.ListenAddr),
		fmt.Sprintf("SOFT_SERVE_LOG_FORMAT=%s", c.Log.Format),
		fmt.Sprintf("SOFT_SERVE_LOG_TIME_FORMAT=%s", c.Log.TimeFormat),
		fmt.Sprintf("SOFT_SERVE_DB_DRIVER=%s", c.DB.Driver),
		fmt.Sprintf("SOFT_SERVE_DB_DATA_SOURCE=%s", c.DB.DataSource),
		fmt.Sprintf("SOFT_SERVE_LFS_ENABLED=%t", c.LFS.Enabled),
		fmt.Sprintf("SOFT_SERVE_LFS_SSH_ENABLED=%t", c.LFS.SSHEnabled),
		fmt.Sprintf("SOFT_SERVE_JOBS_MIRROR_PULL=%s", c.Jobs.MirrorPull),
		fmt.Sprintf("SOFT_SERVE_OPENSHELL_ENABLED=%t", c.OpenShell.Enabled),
		fmt.Sprintf("SOFT_SERVE_OPENSHELL_GATEWAY_ADDRESS=%s", c.OpenShell.GatewayAddress),
		fmt.Sprintf("SOFT_SERVE_OPENSHELL_WORKSPACE=%s", c.OpenShell.Workspace),
		fmt.Sprintf("SOFT_SERVE_OPENSHELL_IMAGE=%s", c.OpenShell.Image),
		fmt.Sprintf("SOFT_SERVE_OPENSHELL_TOKEN_FILE=%s", c.OpenShell.TokenFile),
		fmt.Sprintf("SOFT_SERVE_OPENSHELL_TLS_CA=%s", c.OpenShell.TLSCA),
		fmt.Sprintf("SOFT_SERVE_OPENSHELL_TLS_CERT=%s", c.OpenShell.TLSCert),
		fmt.Sprintf("SOFT_SERVE_OPENSHELL_TLS_KEY=%s", c.OpenShell.TLSKey),
		fmt.Sprintf("SOFT_SERVE_OPENSHELL_INSECURE=%t", c.OpenShell.Insecure),
		fmt.Sprintf("SOFT_SERVE_OPENSHELL_CLONE_URL=%s", c.OpenShell.CloneURL),
		fmt.Sprintf("SOFT_SERVE_OPENSHELL_TOKEN_TTL=%s", c.OpenShell.TokenTTL),
		fmt.Sprintf("SOFT_SERVE_OPENSHELL_COMMAND=%s", c.OpenShell.Command),
		// SOFT_SERVE_OPENSHELL_TOKEN is deliberately omitted: these envs are
		// handed to hook and git subprocesses, which never need the gateway
		// credential.
	}...)

	// AnonAccess and AllowKeyless are tri-state overrides: only emit them
	// when explicitly set, so a subprocess parsing these envs sees the same
	// "unset" state (rather than an empty string coercing to a zero value).
	if c.AnonAccess != nil {
		envs = append(envs, fmt.Sprintf("SOFT_SERVE_ANON_ACCESS=%s", c.AnonAccess.String()))
	}

	if c.AllowKeyless != nil {
		envs = append(envs, fmt.Sprintf("SOFT_SERVE_ALLOW_KEYLESS=%t", *c.AllowKeyless))
	}

	return envs
}

// IsDebug returns true if the server is running in debug mode.
func IsDebug() bool {
	debug, _ := strconv.ParseBool(os.Getenv("SOFT_SERVE_DEBUG"))
	return debug
}

// IsVerbose returns true if the server is running in verbose mode.
// Verbose mode is only enabled if debug mode is enabled.
func IsVerbose() bool {
	verbose, _ := strconv.ParseBool(os.Getenv("SOFT_SERVE_VERBOSE"))
	return IsDebug() && verbose
}

// parseFile parses the given file as a configuration file.
// The file must be in YAML format. Unknown keys are rejected.
func parseFile(cfg *Config, path string) error {
	if err := decodeFile(cfg, path); err != nil {
		return err
	}

	return cfg.Validate()
}

// decodeFile decodes the YAML file at path into cfg. Keys that do not match a
// config field are rejected, so a misspelt key fails instead of being
// silently ignored. Problems with individual keys (unknown keys, values of
// the wrong type) are returned as a *keyErrors, after the rest of the file has
// been decoded; any other error means the file could not be read or parsed.
func decodeFile(cfg *Config, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}

	defer f.Close() //nolint: errcheck
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			ke := &keyErrors{}
			for _, msg := range te.Errors {
				ke.errs = append(ke.errs, errors.New(describeKeyError(msg)))
			}
			return ke
		}
		return fmt.Errorf("decode config: %w", err)
	}

	return nil
}

// keyErrors holds the problems with individual keys found while decoding a
// config file.
type keyErrors struct {
	errs []error
}

func (e *keyErrors) Error() string {
	return "decode config: " + errors.Join(e.errs...).Error()
}

func (e *keyErrors) Unwrap() []error {
	return e.errs
}

// unknownKeyRe matches yaml.v3's error for a key with no matching field.
var unknownKeyRe = regexp.MustCompile(`^line (\d+): field (\S+) not found in type (\S+)$`)

// describeKeyError rewrites yaml.v3's unknown-field error, which names the Go
// type ("field listen_adr not found in type config.SSHConfig"), to name the
// full config key instead ("line 3: unknown key ssh.listen_adr"). Other
// messages are returned unchanged.
func describeKeyError(msg string) string {
	m := unknownKeyRe.FindStringSubmatch(msg)
	if m == nil {
		return msg
	}
	section, ok := configSections[m[3]]
	if !ok {
		return msg
	}
	key := m[2]
	if section != "" {
		key = section + "." + key
	}
	return fmt.Sprintf("line %s: unknown key %s", m[1], key)
}

// configSections maps the Go type of each config section, as yaml.v3 prints
// it ("config.SSHConfig"), to the section's key path ("ssh"). The top-level
// Config maps to "".
var configSections = func() map[string]string {
	m := map[string]string{}
	var walk func(t reflect.Type, path string)
	walk = func(t reflect.Type, path string) {
		m[t.String()] = path
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if f.Type.Kind() != reflect.Struct || name == "" || name == "-" {
				continue
			}
			if path != "" {
				name = path + "." + name
			}
			walk(f.Type, name)
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	return m
}()

// ParseFile parses the config from the default file path.
// This also calls Validate() on the config.
func (c *Config) ParseFile() error {
	return parseFile(c, c.ConfigPath())
}

// parseEnv parses the environment variables as a configuration file.
func parseEnv(cfg *Config) error {
	if err := applyEnv(cfg); err != nil {
		return err
	}

	return cfg.Validate()
}

// applyEnv overrides cfg with the SOFT_SERVE_* environment variables, without
// validating the result.
func applyEnv(cfg *Config) error {
	// Merge initial admin keys from both config file and environment variables.
	initialAdminKeys := append([]string{}, cfg.InitialAdminKeys...)

	// Override with environment variables
	if err := env.ParseWithOptions(cfg, env.Options{
		Prefix: "SOFT_SERVE_",
	}); err != nil {
		return fmt.Errorf("parse environment variables: %w", err)
	}

	// Merge initial admin keys from environment variables.
	if initialAdminKeysEnv := os.Getenv("SOFT_SERVE_INITIAL_ADMIN_KEYS"); initialAdminKeysEnv != "" {
		cfg.InitialAdminKeys = append(cfg.InitialAdminKeys, initialAdminKeys...)
	}

	return nil
}

// ParseEnv parses the config from the environment variables.
// This also calls Validate() on the config.
func (c *Config) ParseEnv() error {
	return parseEnv(c)
}

// Parse parses the config from the default file path and environment variables.
// This also calls Validate() on the config. See Load.
func (c *Config) Parse() error {
	return c.Load(c.ConfigPath())
}

// Load reads the config the way the server does: the YAML file at path, then
// the SOFT_SERVE_* environment variables as overrides, then Validate on the
// result. The file must exist; Load never writes it.
//
// It reports every problem it finds, joined: unknown keys and bad values in
// the file, environment variables that do not parse, and validation errors.
// It stops early only when the file cannot be read or is not valid YAML.
func (c *Config) Load(path string) error {
	var errs []error
	if err := decodeFile(c, path); err != nil {
		var ke *keyErrors
		if !errors.As(err, &ke) {
			return err
		}
		errs = append(errs, ke.errs...)
	}

	if err := applyEnv(c); err != nil {
		errs = append(errs, err)
	}

	if err := c.Validate(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// writeConfig writes the configuration to the given file.
func writeConfig(cfg *Config, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), os.ModePerm); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(newConfigFile(cfg)), 0o644) //nolint: errcheck, gosec
}

// WriteConfig writes the configuration to the default file.
func (c *Config) WriteConfig() error {
	return writeConfig(c, c.ConfigPath())
}

// DefaultDataPath returns the path to the data directory.
// It uses the SOFT_SERVE_DATA_PATH environment variable if set, otherwise it
// uses "data".
func DefaultDataPath() string {
	dp := os.Getenv("SOFT_SERVE_DATA_PATH")
	if dp == "" {
		dp = "data"
	}

	return dp
}

// ConfigPath returns the path to the config file.
func (c *Config) ConfigPath() string { //nolint:revive
	// If we have a custom config location set, then use that.
	if path := os.Getenv("SOFT_SERVE_CONFIG_LOCATION"); exist(path) {
		return path
	}

	// Otherwise, look in the data path.
	return filepath.Join(c.DataPath, "config.yaml")
}

func exist(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Exist returns true if the config file exists.
func (c *Config) Exist() bool {
	return exist(c.ConfigPath())
}

// DefaultConfig returns the default Config. All the path values are relative
// to the data directory.
// Use Validate() to validate the config and ensure absolute paths.
func DefaultConfig() *Config {
	return &Config{
		Name: "Soft Serve",
		// DefaultRepo: "",
		DataPath: DefaultDataPath(),
		SSH: SSHConfig{
			Enabled:       true,
			ListenAddr:    ":23231",
			PublicURL:     "ssh://localhost:23231",
			KeyPath:       filepath.Join("ssh", "soft_serve_host_ed25519"),
			ClientKeyPath: filepath.Join("ssh", "soft_serve_client_ed25519"),
			MaxTimeout:    0,
			IdleTimeout:   10 * 60, // 10 minutes
		},
		Git: GitConfig{
			Enabled:        true,
			ListenAddr:     ":9418",
			PublicURL:      "git://localhost",
			MaxTimeout:     0,
			IdleTimeout:    3,
			MaxConnections: 32,
		},
		HTTP: HTTPConfig{
			Enabled:    true,
			ListenAddr: ":23232",
			PublicURL:  "http://localhost:23232",
			CORS: CORSConfig{
				AllowedHeaders: []string{"Accept", "Accept-Language", "Content-Language", "Content-Type", "Origin", "X-Requested-With", "User-Agent", "Authorization", "Access-Control-Request-Method", "Access-Control-Allow-Origin"},
				AllowedMethods: []string{"GET", "HEAD", "POST", "PUT", "OPTIONS"},
				AllowedOrigins: []string{"http://localhost:23232"},
			},
		},
		Stats: StatsConfig{
			Enabled:    true,
			ListenAddr: "localhost:23233",
		},
		Log: LogConfig{
			Format:     "text",
			TimeFormat: time.DateTime,
		},
		DB: DBConfig{
			Driver: "sqlite",
			DataSource: "soft-serve.db" +
				"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)",
		},
		LFS: LFSConfig{
			Enabled:    true,
			SSHEnabled: false,
		},
		Jobs: JobsConfig{
			MirrorPull: "@every 10m",
		},
		OpenShell: OpenShellConfig{
			Enabled:   false,
			Workspace: "default",
			TokenTTL:  24 * time.Hour,
			Command:   "bash -l",
		},
	}
}

// Validate validates the configuration.
// It updates the configuration with absolute paths.
func (c *Config) Validate() error {
	// Use absolute paths
	if !filepath.IsAbs(c.DataPath) {
		dp, err := filepath.Abs(c.DataPath)
		if err != nil {
			return fmt.Errorf("data path (SOFT_SERVE_DATA_PATH): %w", err)
		}
		c.DataPath = dp
	}

	c.SSH.PublicURL = strings.TrimSuffix(c.SSH.PublicURL, "/")
	c.HTTP.PublicURL = strings.TrimSuffix(c.HTTP.PublicURL, "/")

	if c.SSH.KeyPath != "" && !filepath.IsAbs(c.SSH.KeyPath) {
		c.SSH.KeyPath = filepath.Join(c.DataPath, c.SSH.KeyPath)
	}

	if c.SSH.ClientKeyPath != "" && !filepath.IsAbs(c.SSH.ClientKeyPath) {
		c.SSH.ClientKeyPath = filepath.Join(c.DataPath, c.SSH.ClientKeyPath)
	}

	if c.HTTP.TLSKeyPath != "" && !filepath.IsAbs(c.HTTP.TLSKeyPath) {
		c.HTTP.TLSKeyPath = filepath.Join(c.DataPath, c.HTTP.TLSKeyPath)
	}

	if c.HTTP.TLSCertPath != "" && !filepath.IsAbs(c.HTTP.TLSCertPath) {
		c.HTTP.TLSCertPath = filepath.Join(c.DataPath, c.HTTP.TLSCertPath)
	}

	if strings.HasPrefix(c.DB.Driver, "sqlite") && !filepath.IsAbs(c.DB.DataSource) {
		c.DB.DataSource = filepath.Join(c.DataPath, c.DB.DataSource)
	}

	// Validate keys
	pks := make([]string, 0)
	for _, key := range parseAuthKeys(c.InitialAdminKeys) {
		ak := sshutils.MarshalAuthorizedKey(key)
		pks = append(pks, ak)
	}

	c.InitialAdminKeys = pks

	c.HTTP.CORS.AllowedOrigins = append([]string{c.HTTP.PublicURL}, c.HTTP.CORS.AllowedOrigins...)

	return c.validateOpenShell()
}

// validateOpenShell validates the OpenShell configuration and resolves its
// relative paths. It is a no-op beyond path cleanup when OpenShell is disabled.
func (c *Config) validateOpenShell() error {
	oc := &c.OpenShell
	oc.CloneURL = strings.TrimSuffix(oc.CloneURL, "/")
	for _, p := range []*string{&oc.TLSCA, &oc.TLSCert, &oc.TLSKey, &oc.TokenFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(c.DataPath, *p)
		}
	}

	// Collect every problem rather than stopping at the first, so one run
	// reports everything that needs fixing.
	var errs []error
	conflict := oc.Token != "" && oc.TokenFile != ""
	if conflict {
		errs = append(errs, errOpenShellTokenConflict)
	}

	if !oc.Enabled {
		return errors.Join(errs...)
	}

	if oc.GatewayAddress == "" {
		errs = append(errs, errors.New("openshell: gateway_address is required when openshell is enabled"))
	}
	if oc.Image == "" {
		errs = append(errs, errors.New("openshell: image is required when openshell is enabled"))
	}
	if (oc.TLSCert == "") != (oc.TLSKey == "") {
		errs = append(errs, errors.New("openshell: tls_cert and tls_key must be set together"))
	}
	if oc.TokenTTL <= 0 {
		errs = append(errs, errors.New("openshell: token_ttl must be positive"))
	}
	if strings.TrimSpace(oc.Command) == "" {
		errs = append(errs, errors.New("openshell: command must not be empty"))
	}
	if strings.TrimSpace(oc.Workspace) == "" {
		errs = append(errs, errors.New("openshell: workspace must not be empty"))
	}
	// Read the token file once now so a missing or unreadable file fails at
	// startup. The value is not kept: GatewayToken reads the file again for
	// every client, which picks up a rotated token. Skipped when both token
	// and token_file are set, which is already reported above.
	if !conflict {
		if _, err := oc.GatewayToken(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// errOpenShellTokenConflict is returned when both token and token_file are set.
var errOpenShellTokenConflict = errors.New("openshell: token and token_file are mutually exclusive; set only one")

// GatewayToken returns the bearer token for the OpenShell gateway: Token, or
// the contents of TokenFile with trailing whitespace trimmed. It returns ""
// when neither is set. TokenFile is read on every call.
func (oc OpenShellConfig) GatewayToken() (string, error) {
	if oc.TokenFile == "" {
		return oc.Token, nil
	}
	if oc.Token != "" {
		return "", errOpenShellTokenConflict
	}

	bts, err := os.ReadFile(oc.TokenFile)
	if err != nil {
		return "", fmt.Errorf("openshell: read token_file: %w", err)
	}
	token := strings.TrimRightFunc(string(bts), unicode.IsSpace)
	if token == "" {
		return "", fmt.Errorf("openshell: token_file %q is empty", oc.TokenFile)
	}

	return token, nil
}

// OpenShellCloneURL returns the base HTTP URL sandboxes use to reach this
// server's git endpoint: openshell.clone_url, or http.public_url when unset.
func (c *Config) OpenShellCloneURL() string {
	if c.OpenShell.CloneURL != "" {
		return strings.TrimSuffix(c.OpenShell.CloneURL, "/")
	}
	return strings.TrimSuffix(c.HTTP.PublicURL, "/")
}

// parseAuthKeys parses authorized keys from either file paths or string authorized_keys.
func parseAuthKeys(aks []string) []ssh.PublicKey {
	exist := make(map[string]struct{}, 0)
	pks := make([]ssh.PublicKey, 0)
	for _, key := range aks {
		if bts, err := os.ReadFile(key); err == nil {
			// key is a file
			key = strings.TrimSpace(string(bts))
		}

		if pk, _, err := sshutils.ParseAuthorizedKey(key); err == nil {
			if _, ok := exist[key]; !ok {
				pks = append(pks, pk)
				exist[key] = struct{}{}
			}
		}
	}
	return pks
}

// AdminKeys returns the server admin keys.
func (c *Config) AdminKeys() []ssh.PublicKey {
	return parseAuthKeys(c.InitialAdminKeys)
}

func init() {
	if ex, err := os.Executable(); err == nil {
		binPath = filepath.ToSlash(ex)
	}
}
