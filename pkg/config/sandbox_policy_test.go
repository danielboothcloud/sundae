package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
)

func TestSandboxPolicyDefaults(t *testing.T) {
	is := is.New(t)
	cfg := enabledOpenShellConfig(t)
	is.NoErr(cfg.Validate())
	is.Equal(cfg.OpenShell.SandboxPolicy, SandboxPolicySundae)

	// Unset (a config from before the key existed) means sundae too.
	cfg = enabledOpenShellConfig(t)
	cfg.OpenShell.SandboxPolicy = " Image "
	is.NoErr(cfg.Validate())
	is.Equal(cfg.OpenShell.SandboxPolicy, SandboxPolicyImage)
	cfg = &Config{DataPath: t.TempDir()}
	is.NoErr(cfg.Validate())
	is.Equal(cfg.OpenShell.SandboxPolicy, SandboxPolicySundae)
}

func TestValidateSandboxPolicy(t *testing.T) {
	ep := func(mutate func(*OpenShellNetworkEndpoint)) []OpenShellNetworkPolicy {
		e := OpenShellNetworkEndpoint{Host: "api.example.com", Port: 443}
		mutate(&e)
		return []OpenShellNetworkPolicy{{Name: "api", Endpoints: []OpenShellNetworkEndpoint{e}}}
	}
	for name, tc := range map[string]struct {
		mutate func(*Config)
		err    string
	}{
		"valid": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = []OpenShellNetworkPolicy{
				{Name: "anthropic", Endpoints: []OpenShellNetworkEndpoint{{Host: "api.anthropic.com", Port: 443}}, Binaries: []string{"/usr/local/bin/claude"}},
				{Name: "pypi", Endpoints: []OpenShellNetworkEndpoint{
					{Host: "*.pythonhosted.org", Ports: []int{80, 443}},
					{Host: "pypi.org", Port: 443, Protocol: "REST", Access: "read-only"},
					{AllowedIPs: []string{"10.0.0.0/8"}, Port: 8080},
					{Host: "10.1.2.3", Port: 80, AllowedIPs: []string{"10.1.2.3"}},
					{Host: "redis.internal", Port: 6379, Protocol: "tcp"},
				}},
			}
		}},
		"unknown mode": {mutate: func(c *Config) { c.OpenShell.SandboxPolicy = "none" }, err: `openshell.sandbox_policy "none" must be sundae or image`},
		"rules with image mode": {mutate: func(c *Config) {
			c.OpenShell.SandboxPolicy = SandboxPolicyImage
			c.OpenShell.NetworkPolicies = ep(func(*OpenShellNetworkEndpoint) {})
		}, err: "openshell.network_policies needs openshell.sandbox_policy: sundae"},
		"bad clone url": {mutate: func(c *Config) { c.OpenShell.CloneURL = "ssh://sundae:23231" }, err: "must be an http or https URL"},
		"missing name": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(*OpenShellNetworkEndpoint) {})
			c.OpenShell.NetworkPolicies[0].Name = " "
		}, err: "openshell.network_policies[0]: name is required"},
		"bad name": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(*OpenShellNetworkEndpoint) {})
			c.OpenShell.NetworkPolicies[0].Name = "_provider_x"
		}, err: `name "_provider_x" must be 1 to 63 letters`},
		"reserved name": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(*OpenShellNetworkEndpoint) {})
			c.OpenShell.NetworkPolicies[0].Name = NetworkPolicyGit
		}, err: `name "sundae_git" uses the "sundae_" prefix`},
		"duplicate name": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = append(ep(func(*OpenShellNetworkEndpoint) {}), ep(func(*OpenShellNetworkEndpoint) {})...)
		}, err: `openshell.network_policies[1] (api): name "api" is already used by openshell.network_policies[0]`},
		"no endpoints": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = []OpenShellNetworkPolicy{{Name: "x"}}
		}, err: "endpoints: at least one is required"},
		"no host": {mutate: func(c *Config) { c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.Host = "" }) }, err: "endpoints[0]: host or allowed_ips is required"},
		"url host": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.Host = "https://api.example.com" })
		}, err: "without a scheme, port or path"},
		"host with port": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.Host = "api.example.com:443" })
		}, err: "without a scheme, port or path"},
		"tld wildcard": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.Host = "*.com" })
		}, err: "top-level domain wildcard"},
		"inner wildcard": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.Host = "api.*.example.com" })
		}, err: "only have a wildcard in its first label"},
		"star":    {mutate: func(c *Config) { c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.Host = "*" }) }, err: "matches every host"},
		"no port": {mutate: func(c *Config) { c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.Port = 0 }) }, err: "port or ports is required"},
		"port and ports": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.Ports = []int{80} })
		}, err: "set port or ports, not both"},
		"port out of range": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.Port = 70000 })
		}, err: "port 70000 is out of range"},
		"ports out of range": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.Port, e.Ports = 0, []int{443, 0} })
		}, err: "ports[1] 0 is out of range"},
		"unknown protocol": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.Protocol = "graphql" })
		}, err: `protocol "graphql" must be empty`},
		"rest without access": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.Protocol = "rest" })
		}, err: "protocol rest needs access"},
		"access without rest": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.Access = "full" })
		}, err: "access needs protocol: rest"},
		"unknown access": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.Protocol, e.Access = "rest", "write" })
		}, err: `access "write" must be read-only, read-write, full`},
		"tcp ip host": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.Host, e.Protocol = "10.0.0.1", "tcp" })
		}, err: "protocol tcp needs a DNS host name"},
		"bad allowed ip": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(e *OpenShellNetworkEndpoint) { e.AllowedIPs = []string{"10.0.0.0/33"} })
		}, err: `allowed_ips[0] "10.0.0.0/33" is not an IP address or CIDR range`},
		"relative binary": {mutate: func(c *Config) {
			c.OpenShell.NetworkPolicies = ep(func(*OpenShellNetworkEndpoint) {})
			c.OpenShell.NetworkPolicies[0].Binaries = []string{"curl"}
		}, err: `binaries[0] "curl" must be an absolute path`},
	} {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			cfg := enabledOpenShellConfig(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.err == "" {
				is.NoErr(err)
				return
			}
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), tc.err)) // got: err
		})
	}
}

func TestValidateSandboxPolicyNormalises(t *testing.T) {
	is := is.New(t)
	cfg := enabledOpenShellConfig(t)
	cfg.OpenShell.NetworkPolicies = []OpenShellNetworkPolicy{{
		Name:      " api ",
		Endpoints: []OpenShellNetworkEndpoint{{Host: " API.Example.com ", Port: 443, Protocol: " Rest", Access: "Read-Write ", AllowedIPs: []string{" 10.0.0.0/8"}}},
		Binaries:  []string{" /usr/bin/curl "},
	}}
	is.NoErr(cfg.Validate())
	is.Equal(cfg.OpenShell.NetworkPolicies[0], OpenShellNetworkPolicy{
		Name:      "api",
		Endpoints: []OpenShellNetworkEndpoint{{Host: "api.example.com", Port: 443, Protocol: "rest", Access: "read-write", AllowedIPs: []string{"10.0.0.0/8"}}},
		Binaries:  []string{"/usr/bin/curl"},
	})
}

func TestURLHostPort(t *testing.T) {
	for raw, want := range map[string]struct {
		host string
		port int
		err  bool
	}{
		"http://sundae-http.sundae.svc.cluster.local:23232": {"sundae-http.sundae.svc.cluster.local", 23232, false},
		"http://Git.Example.com":                            {"git.example.com", 80, false},
		"https://git.example.com/base":                      {"git.example.com", 443, false},
		"http://[fd00::1]:8080":                             {"fd00::1", 8080, false},
		"ssh://git.example.com":                             {"", 0, true},
		"http://":                                           {"", 0, true},
		"http://h:0":                                        {"", 0, true},
	} {
		host, port, err := URLHostPort(raw)
		if (err != nil) != want.err || host != want.host || port != want.port {
			t.Errorf("URLHostPort(%q) = %q, %d, %v; want %q, %d, err %t", raw, host, port, err, want.host, want.port, want.err)
		}
	}
}

func testNetworkPolicies() []OpenShellNetworkPolicy {
	return []OpenShellNetworkPolicy{
		{Name: "anthropic", Endpoints: []OpenShellNetworkEndpoint{{Host: "api.anthropic.com", Port: 443}}, Binaries: []string{"/usr/local/bin/claude", "/usr/bin/node"}},
		{Name: "internal", Endpoints: []OpenShellNetworkEndpoint{
			{Host: "*.corp.example.com", Ports: []int{80, 443}, AllowedIPs: []string{"10.0.0.0/8", "10.1.2.3"}},
			{Host: "pypi.org", Port: 443, Protocol: "rest", Access: "read-only"},
		}},
	}
}

// TestSandboxPolicyEnvironRoundTrip checks that hook subprocesses, which
// rebuild the config from Environ, see the same sandbox policy settings.
func TestSandboxPolicyEnvironRoundTrip(t *testing.T) {
	is := is.New(t)
	cfg := enabledOpenShellConfig(t)
	cfg.OpenShell.NetworkPolicies = testNetworkPolicies()
	is.NoErr(cfg.Validate())

	for _, e := range cfg.Environ() {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	got := DefaultConfig()
	got.OpenShell.SandboxPolicy = ""
	is.NoErr(got.ParseEnv())
	is.Equal(got.OpenShell.SandboxPolicy, SandboxPolicySundae)
	is.Equal(got.OpenShell.NetworkPolicies, cfg.OpenShell.NetworkPolicies)
}

func TestSandboxPolicyEnv(t *testing.T) {
	is := is.New(t)
	for k, v := range map[string]string{
		"SUNDAE_OPENSHELL_SANDBOX_POLICY":                             "image",
		"SUNDAE_OPENSHELL_NETWORK_POLICIES_0_NAME":                    "api",
		"SUNDAE_OPENSHELL_NETWORK_POLICIES_0_BINARIES":                "/usr/bin/curl,/usr/bin/git",
		"SUNDAE_OPENSHELL_NETWORK_POLICIES_0_ENDPOINTS_0_HOST":        "api.example.com",
		"SUNDAE_OPENSHELL_NETWORK_POLICIES_0_ENDPOINTS_0_PORTS":       "80,443",
		"SUNDAE_OPENSHELL_NETWORK_POLICIES_0_ENDPOINTS_1_HOST":        "",
		"SUNDAE_OPENSHELL_NETWORK_POLICIES_0_ENDPOINTS_1_PORT":        "8080",
		"SUNDAE_OPENSHELL_NETWORK_POLICIES_0_ENDPOINTS_1_ALLOWED_IPS": "10.0.0.0/8",
	} {
		t.Setenv(k, v)
	}
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	is.NoErr(applyEnv(cfg))
	is.Equal(cfg.OpenShell.SandboxPolicy, "image")
	is.Equal(cfg.OpenShell.NetworkPolicies, []OpenShellNetworkPolicy{{
		Name: "api",
		Endpoints: []OpenShellNetworkEndpoint{
			{Host: "api.example.com", Ports: []int{80, 443}},
			{Port: 8080, AllowedIPs: []string{"10.0.0.0/8"}},
		},
		Binaries: []string{"/usr/bin/curl", "/usr/bin/git"},
	}})
	// Rules with sandbox_policy: image are refused.
	err := cfg.Validate()
	is.True(err != nil && strings.Contains(err.Error(), "needs openshell.sandbox_policy: sundae"))
}

func TestSandboxPolicyConfigFileRoundTrip(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.OpenShell.NetworkPolicies = append(testNetworkPolicies(), OpenShellNetworkPolicy{
		Name: "hostless", Endpoints: []OpenShellNetworkEndpoint{{Port: 8080, AllowedIPs: []string{"192.168.0.0/16"}}},
	})
	is.NoErr(cfg.Validate())
	is.NoErr(cfg.WriteConfig())

	got := &Config{DataPath: cfg.DataPath}
	is.NoErr(parseFile(got, cfg.ConfigPath()))
	is.Equal(got.OpenShell.SandboxPolicy, SandboxPolicySundae)
	is.Equal(got.OpenShell.NetworkPolicies, cfg.OpenShell.NetworkPolicies)

	// The default file documents the keys.
	cfg = DefaultConfig()
	cfg.DataPath = t.TempDir()
	is.NoErr(cfg.WriteConfig())
	b, err := os.ReadFile(cfg.ConfigPath())
	is.NoErr(err)
	is.True(strings.Contains(string(b), `sandbox_policy: "sundae"`))
	is.True(strings.Contains(string(b), "#network_policies:\n  #  - name: anthropic"))
	got = &Config{DataPath: cfg.DataPath}
	is.NoErr(parseFile(got, cfg.ConfigPath()))
	is.Equal(got.OpenShell.SandboxPolicy, SandboxPolicySundae)
	is.Equal(len(got.OpenShell.NetworkPolicies), 0)
}

func TestSandboxPolicyUnknownKeys(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	p := filepath.Join(td, "config.yaml")
	is.NoErr(os.WriteFile(p, []byte(`openshell:
  network_policies:
    - name: api
      endpoints:
        - host: api.example.com
          port: 443
          tls: skip
      binaries:
        - path: /usr/bin/curl
`), 0o600))
	cfg := &Config{DataPath: td}
	err := parseFile(cfg, p)
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "line 7: unknown key openshell.network_policies[].endpoints[].tls")) // got: err
	is.True(strings.Contains(err.Error(), "line 9: cannot unmarshal !!map into string"))                       // got: err
}
