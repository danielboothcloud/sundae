package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// writeSSHKey writes a new ed25519 private key in OpenSSH format, with a
// passphrase when one is given, and returns its path and public key.
func writeSSHKey(t *testing.T, dir, name, passphrase string) (string, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase != "" {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	} else {
		block, err = ssh.MarshalPrivateKey(priv, "")
	}
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return p, sp
}

// writeKnownHosts writes a known_hosts file with one key for each pattern.
func writeKnownHosts(t *testing.T, dir, name string, key ssh.PublicKey, patterns ...string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range patterns {
		b.WriteString(p + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + "\n")
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateForgesSSH(t *testing.T) {
	type env struct {
		dir, key, hostKH, wildKH, hashedKH, otherKH string
	}
	setup := func(t *testing.T) (*Config, env) {
		cfg := DefaultConfig()
		cfg.DataPath = t.TempDir()
		cfg.Forges = []ForgeConfig{{Name: "gh", Kind: "github"}}
		e := env{dir: cfg.DataPath}
		e.key, _ = writeSSHKey(t, e.dir, "id", "")
		_, hostKey := writeSSHKey(t, t.TempDir(), "host", "")
		e.hostKH = writeKnownHosts(t, e.dir, "kh", hostKey, "[git.example.com]:2222")
		e.wildKH = writeKnownHosts(t, e.dir, "kh-wild", hostKey, "*.example.com")
		e.hashedKH = writeKnownHosts(t, e.dir, "kh-hashed", hostKey, knownhosts.HashHostname("git.example.com"))
		e.otherKH = writeKnownHosts(t, e.dir, "kh-other", hostKey, "other.example.com")
		return cfg, e
	}
	cases := map[string]struct {
		mutate func(*ForgeConfig, env)
		errs   []string
	}{
		"https only":                    {mutate: func(*ForgeConfig, env) {}},
		"github.com with built-in keys": {mutate: func(f *ForgeConfig, e env) { f.SSHKeyFile = e.key }},
		"ssh.github.com:443 with built-in keys": {mutate: func(f *ForgeConfig, e env) {
			f.SSHKeyFile, f.SSHURL = e.key, "ssh://git@ssh.github.com:443"
		}},
		"gitlab.com with built-in keys": {mutate: func(f *ForgeConfig, e env) {
			f.Kind, f.SSHKeyFile = "gitlab", e.key
		}},
		"relative key path": {mutate: func(f *ForgeConfig, e env) { f.SSHKeyFile = "id" }},
		"self-hosted needs known_hosts": {
			mutate: func(f *ForgeConfig, e env) {
				f.Kind, f.URL, f.SSHKeyFile = "gitlab", "https://git.example.com", e.key
			},
			errs: []string{"forges[0] (gh): ssh_known_hosts_file is required for SSH host git.example.com"},
		},
		"ssh_url to another host needs known_hosts": {
			mutate: func(f *ForgeConfig, e env) { f.SSHKeyFile, f.SSHURL = e.key, "ssh://git@github.example.com" },
			errs:   []string{"ssh_known_hosts_file is required for SSH host github.example.com"},
		},
		"self-hosted with known_hosts": {mutate: func(f *ForgeConfig, e env) {
			f.Kind, f.URL, f.SSHKeyFile, f.SSHKnownHostsFile, f.SSHURL = "gitlab", "https://git.example.com", e.key, e.hostKH, "ssh://git@git.example.com:2222"
		}},
		"wildcard known_hosts": {mutate: func(f *ForgeConfig, e env) {
			f.URL, f.SSHKeyFile, f.SSHKnownHostsFile = "https://git.example.com", e.key, e.wildKH
		}},
		"hashed known_hosts": {mutate: func(f *ForgeConfig, e env) {
			f.URL, f.SSHKeyFile, f.SSHKnownHostsFile = "https://git.example.com", e.key, e.hashedKH
		}},
		"known_hosts without the host": {
			mutate: func(f *ForgeConfig, e env) {
				f.URL, f.SSHKeyFile, f.SSHKnownHostsFile = "https://git.example.com", e.key, e.otherKH
			},
			errs: []string{"forges[0] (gh): ssh_known_hosts_file:", "has no host key for git.example.com"},
		},
		"known_hosts port mismatch": {
			mutate: func(f *ForgeConfig, e env) {
				f.URL, f.SSHKeyFile, f.SSHKnownHostsFile = "https://git.example.com", e.key, e.hostKH
			},
			errs: []string{"has no host key for git.example.com"},
		},
		"missing known_hosts file": {
			mutate: func(f *ForgeConfig, e env) { f.SSHKeyFile, f.SSHKnownHostsFile = e.key, "nope" },
			errs:   []string{"forges[0] (gh): ssh_known_hosts_file: read"},
		},
		"missing key file": {
			mutate: func(f *ForgeConfig, e env) { f.SSHKeyFile = "nope" },
			errs:   []string{"forges[0] (gh): read ssh_key_file:"},
		},
		"empty key file": {
			mutate: func(f *ForgeConfig, e env) { f.SSHKeyFile = writeTokenFile(t, e.dir, "empty", "\n") },
			errs:   []string{"forges[0] (gh): ssh_key_file", "is empty"},
		},
		"not a key": {
			mutate: func(f *ForgeConfig, e env) { f.SSHKeyFile = writeTokenFile(t, e.dir, "junk", "ghp_x\n") },
			errs:   []string{"forges[0] (gh): ssh_key_file", "is not a usable private key"},
		},
		"passphrase key": {
			mutate: func(f *ForgeConfig, e env) { f.SSHKeyFile, _ = writeSSHKey(t, e.dir, "locked", "pw") },
			errs:   []string{"forges[0] (gh): ssh_key_file", "is passphrase-protected"},
		},
		"ssh_url without key": {
			mutate: func(f *ForgeConfig, e env) { f.SSHURL = "ssh://git@ssh.github.com:443" },
			errs:   []string{"forges[0] (gh): ssh_url is set but ssh_key_file is not"},
		},
		"known_hosts without key": {
			mutate: func(f *ForgeConfig, e env) { f.SSHKnownHostsFile = e.hostKH },
			errs:   []string{"forges[0] (gh): ssh_known_hosts_file is set but ssh_key_file is not"},
		},
		"bad ssh_url": {
			mutate: func(f *ForgeConfig, e env) { f.SSHKeyFile, f.SSHURL = e.key, "git@github.com:me" },
			errs:   []string{`forges[0] (gh): ssh_url "git@github.com:me": `},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, e := setup(t)
			tc.mutate(&cfg.Forges[0], e)
			err := cfg.Validate()
			if len(tc.errs) == 0 {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate: no error, want %q", tc.errs)
			}
			for _, want := range tc.errs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestParseForgeSSHURL(t *testing.T) {
	for _, tc := range []struct {
		in, want, err string
	}{
		{in: "ssh://git@ssh.github.com:443", want: "ssh://git@ssh.github.com:443"},
		{in: "ssh://ssh.github.com:443/", want: "ssh://git@ssh.github.com:443"},
		{in: "SSH://Git@GitLab.Example.com", want: "ssh://Git@gitlab.example.com"},
		{in: "ssh://git@gitlab.example.com:22", want: "ssh://git@gitlab.example.com"},
		{in: "ssh://deploy@[2001:db8::1]:2222", want: "ssh://deploy@[2001:db8::1]:2222"},
		{in: "ssh://git@[2001:db8::1]", want: "ssh://git@[2001:db8::1]"},
		{in: "git@github.com:me/x.git", err: "must be an ssh:// URL"},
		{in: "https://github.com", err: "must be an ssh:// URL"},
		{in: "ssh://", err: "must include a host"},
		{in: "ssh://git:pw@github.com", err: "must not include a password"},
		{in: "ssh://git@github.com/me", err: "must not include a path"},
		{in: "ssh://git@github.com?x=1", err: "must not include a path, query"},
		{in: "ssh://git@github.com:0", err: "invalid port"},
		{in: "ssh://git@github.com:99999", err: "invalid port"},
		{in: "ssh://-oProxyCommand=x@github.com", err: "invalid user"},
		{in: "ssh://git@-oProxyCommand=x", err: "invalid host"},
	} {
		u, err := ParseForgeSSHURL(tc.in)
		switch {
		case tc.err != "":
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("ParseForgeSSHURL(%q) = %v, %v; want error containing %q", tc.in, u, err, tc.err)
			}
		case err != nil:
			t.Errorf("ParseForgeSSHURL(%q): %v", tc.in, err)
		case u.String() != tc.want:
			t.Errorf("ParseForgeSSHURL(%q) = %q, want %q", tc.in, u, tc.want)
		}
	}
}

func TestForgeSSHEndpointAndKnownHosts(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	key, _ := writeSSHKey(t, cfg.DataPath, "id", "")
	cfg.Forges = []ForgeConfig{
		{Name: "gh", Kind: "github", SSHKeyFile: key},
		{Name: "gh443", Kind: "github", Prefix: "x", SSHKeyFile: key, SSHURL: "ssh://git@ssh.github.com:443"},
	}
	is.NoErr(cfg.Validate())

	ep, err := cfg.Forges[0].SSHEndpoint()
	is.NoErr(err)
	is.Equal(ep.String(), "ssh://git@github.com")
	kh, err := cfg.Forges[0].SSHKnownHosts()
	is.NoErr(err)
	is.Equal(strings.Count(string(kh), "\ngithub.com ")+1, 3) // three keys for github.com
	is.True(strings.HasPrefix(string(kh), "github.com ssh-ed25519 "))

	kh, err = cfg.Forges[1].SSHKnownHosts()
	is.NoErr(err)
	is.True(strings.HasPrefix(string(kh), "[ssh.github.com]:443 ssh-ed25519 "))

	// Every built-in key parses.
	for host := range builtinForgeHostKeys {
		kh, ok := BuiltinForgeKnownHosts(host, "22")
		is.True(ok)
		rest := kh
		n := 0
		for len(rest) > 0 {
			_, _, _, _, r, err := ssh.ParseKnownHosts(rest)
			if err != nil {
				break
			}
			rest = r
			n++
		}
		is.Equal(n, 3)
	}
	_, ok := BuiltinForgeKnownHosts("github.example.com", "22")
	is.True(!ok)
}

func TestParseForgesFileSSH(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	key, _ := writeSSHKey(t, td, "id", "")
	p := filepath.Join(td, "config.yaml")
	is.NoErr(os.WriteFile(p, []byte(`forges:
  - name: github
    kind: github
    ssh_key: `+key+`
`), 0o600))
	err := parseFile(&Config{DataPath: td}, p)
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "unknown key forges[].ssh_key")) // got: err

	is.NoErr(os.WriteFile(p, []byte(`forges:
  - name: github
    kind: github
    ssh_key_file: id
    ssh_url: ssh://git@ssh.github.com:443
`), 0o600))
	cfg := &Config{DataPath: td}
	is.NoErr(parseFile(cfg, p))
	is.Equal(cfg.Forges[0].SSHKeyFile, key)
	is.Equal(cfg.Forges[0].SSHURL, "ssh://git@ssh.github.com:443")
}

// TestForgesSSHEnvironRoundTrip checks that the SSH settings reach hook
// subprocesses through Environ, as paths only, and survive the config
// file round trip.
func TestForgesSSHEnvironRoundTrip(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	key, hostKey := writeSSHKey(t, td, "id", "")
	kh := writeKnownHosts(t, td, "kh", hostKey, "[git.example.com]:2222")
	cfg := DefaultConfig()
	cfg.DataPath = td
	cfg.Forges = []ForgeConfig{
		{Name: "github", Kind: "github", SSHKeyFile: key},
		{Name: "gitlab", Kind: "gitlab", URL: "https://git.example.com", Prefix: "gl", SSHKeyFile: key, SSHKnownHostsFile: kh, SSHURL: "ssh://git@git.example.com:2222"},
	}
	is.NoErr(cfg.Validate())

	keyBytes, err := os.ReadFile(key)
	is.NoErr(err)
	environ := cfg.Environ()
	for _, e := range environ {
		is.True(!strings.Contains(e, "PRIVATE KEY"))
		is.True(!strings.Contains(e, strings.TrimSpace(string(keyBytes))))
	}
	is.True(contains(environ, "SUNDAE_FORGES_1_SSH_URL=ssh://git@git.example.com:2222"))
	for _, e := range environ {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	got := DefaultConfig()
	is.NoErr(got.ParseEnv())
	is.Equal(got.Forges, cfg.Forges)

	is.NoErr(cfg.WriteConfig())
	fromFile := &Config{DataPath: cfg.DataPath}
	is.NoErr(parseFile(fromFile, cfg.ConfigPath()))
	is.Equal(fromFile.Forges, cfg.Forges)
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}
