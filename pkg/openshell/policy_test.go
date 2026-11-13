package openshell

import (
	"context"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/fake"
	"github.com/danielboothcloud/sundae/pkg/config"
)

// policyConfig returns a config whose sandbox policy BuildSandboxPolicy
// can build, with the clone URL given.
func policyConfig(cloneURL string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.OpenShell.CloneURL = cloneURL
	return cfg
}

func anyBinaryRule(name string, ep v1.PolicyNetworkEndpoint) v1.NetworkPolicyRule {
	return v1.NetworkPolicyRule{
		Name:      name,
		Endpoints: []v1.PolicyNetworkEndpoint{ep},
		Binaries:  []v1.PolicyNetworkBinary{{Path: "/**"}},
	}
}

func TestBuildSandboxPolicyDefault(t *testing.T) {
	p, err := BuildSandboxPolicy(policyConfig("http://sundae-http.sundae.svc.cluster.local:23232"))
	if err != nil {
		t.Fatal(err)
	}
	// OpenShell's restrictive default, as restrictive_default_policy()
	// builds it in 0.1.8, with only Sundae's git rule added.
	want := &v1.SandboxPolicy{
		Version: 1,
		Filesystem: &v1.FilesystemPolicy{
			IncludeWorkdir: true,
			ReadOnly:       []string{"/bin", "/usr", "/lib", "/proc", "/dev/urandom", "/etc", "/var/log"},
			ReadWrite:      []string{"/tmp", "/dev/null"},
		},
		Landlock: &v1.LandlockPolicy{Compatibility: "best_effort"},
		NetworkPolicies: map[string]v1.NetworkPolicyRule{
			"sundae_git": anyBinaryRule("sundae_git", v1.PolicyNetworkEndpoint{Host: "sundae-http.sundae.svc.cluster.local", Port: 23232}),
		},
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("policy =\n%+v\nwant\n%+v", p, want)
	}
}

func TestBuildSandboxPolicyGitRule(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want v1.PolicyNetworkEndpoint
	}{
		{"http://git.example.com", v1.PolicyNetworkEndpoint{Host: "git.example.com", Port: 80}},
		{"https://Git.Example.com/", v1.PolicyNetworkEndpoint{Host: "git.example.com", Port: 443}},
		{"https://git.example.com:8443", v1.PolicyNetworkEndpoint{Host: "git.example.com", Port: 8443}},
		{"http://10.0.0.5:23232", v1.PolicyNetworkEndpoint{Host: "10.0.0.5", Port: 23232, AllowedIPs: []string{"10.0.0.5"}}},
		{"http://[fd00::5]:23232", v1.PolicyNetworkEndpoint{Host: "fd00::5", Port: 23232, AllowedIPs: []string{"fd00::5"}}},
	} {
		p, err := BuildSandboxPolicy(policyConfig(tc.url))
		if err != nil {
			t.Fatalf("%s: %v", tc.url, err)
		}
		if got, want := p.NetworkPolicies["sundae_git"], anyBinaryRule("sundae_git", tc.want); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: sundae_git = %+v, want %+v", tc.url, got, want)
		}
	}

	// Without clone_url, http.public_url.
	cfg := config.DefaultConfig()
	cfg.HTTP.PublicURL = "https://code.example.org"
	p, err := BuildSandboxPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ep := p.NetworkPolicies["sundae_git"].Endpoints[0]; ep.Host != "code.example.org" || ep.Port != 443 {
		t.Fatalf("public_url: %+v", ep)
	}

	// A clone URL Sundae cannot allow is an error, not a policy without it.
	if _, err := BuildSandboxPolicy(policyConfig("ssh://git.example.com")); err == nil || !strings.Contains(err.Error(), "clone URL") {
		t.Fatalf("ssh clone URL: err = %v", err)
	}
}

func TestBuildSandboxPolicySnapshots(t *testing.T) {
	for _, tc := range []struct {
		name string
		s3   config.S3SnapshotConfig
		want v1.PolicyNetworkEndpoint
	}{
		{
			"aws virtual-hosted",
			config.S3SnapshotConfig{Region: "eu-west-2", Bucket: "snaps"},
			v1.PolicyNetworkEndpoint{Host: "snaps.s3.eu-west-2.amazonaws.com", Port: 443},
		},
		{
			"aws path-style",
			config.S3SnapshotConfig{Region: "eu-west-2", Bucket: "snaps", PathStyle: true},
			v1.PolicyNetworkEndpoint{Host: "s3.eu-west-2.amazonaws.com", Port: 443},
		},
		{
			"rgw path-style",
			config.S3SnapshotConfig{Endpoint: "http://rook-ceph-rgw-store.rook-ceph.svc", Bucket: "snaps", PathStyle: true},
			v1.PolicyNetworkEndpoint{Host: "rook-ceph-rgw-store.rook-ceph.svc", Port: 80},
		},
		{
			"minio virtual-hosted",
			config.S3SnapshotConfig{Endpoint: "https://minio.example.com:9000", Bucket: "snaps"},
			v1.PolicyNetworkEndpoint{Host: "snaps.minio.example.com", Port: 9000},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := policyConfig("http://sundae:23232")
			cfg.Snapshots.S3 = tc.s3
			cfg.Snapshots.S3.Enabled = true
			p, err := BuildSandboxPolicy(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := p.NetworkPolicies["sundae_snapshots"], anyBinaryRule("sundae_snapshots", tc.want); !reflect.DeepEqual(got, want) {
				t.Fatalf("sundae_snapshots = %+v, want %+v", got, want)
			}
			if len(p.NetworkPolicies) != 2 {
				t.Fatalf("rules = %v", p.NetworkPolicies)
			}
		})
	}

	// Off: no rule, even when the section is filled in.
	cfg := policyConfig("http://sundae:23232")
	cfg.Snapshots.S3 = config.S3SnapshotConfig{Region: "eu-west-2", Bucket: "snaps"}
	p, err := BuildSandboxPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.NetworkPolicies["sundae_snapshots"]; ok || len(p.NetworkPolicies) != 1 {
		t.Fatalf("snapshots off: rules = %v", p.NetworkPolicies)
	}
}

func TestBuildSandboxPolicyExtraRules(t *testing.T) {
	cfg := policyConfig("http://sundae:23232")
	cfg.OpenShell.NetworkPolicies = []config.OpenShellNetworkPolicy{
		{Name: "anthropic", Endpoints: []config.OpenShellNetworkEndpoint{{Host: "api.anthropic.com", Port: 443}}, Binaries: []string{"/usr/local/bin/claude", "/usr/bin/node"}},
		{Name: "internal", Endpoints: []config.OpenShellNetworkEndpoint{
			{Host: "*.corp.example.com", Ports: []int{80, 443}, AllowedIPs: []string{"10.0.0.0/8"}},
			{Host: "pypi.org", Port: 443, Protocol: "rest", Access: "read-only"},
			{Host: "redis.corp.example.com", Port: 6379, Protocol: "tcp"},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := BuildSandboxPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]v1.NetworkPolicyRule{
		"sundae_git": anyBinaryRule("sundae_git", v1.PolicyNetworkEndpoint{Host: "sundae", Port: 23232}),
		"anthropic": {
			Name:      "anthropic",
			Endpoints: []v1.PolicyNetworkEndpoint{{Host: "api.anthropic.com", Port: 443}},
			Binaries:  []v1.PolicyNetworkBinary{{Path: "/usr/local/bin/claude"}, {Path: "/usr/bin/node"}},
		},
		"internal": {
			Name: "internal",
			Endpoints: []v1.PolicyNetworkEndpoint{
				{Host: "*.corp.example.com", Ports: []uint32{80, 443}, AllowedIPs: []string{"10.0.0.0/8"}},
				{Host: "pypi.org", Port: 443, Protocol: "rest", Access: v1.NetworkAccessPresetReadOnly},
				{Host: "redis.corp.example.com", Port: 6379, Protocol: "tcp"},
			},
			// No binaries: any program.
			Binaries: []v1.PolicyNetworkBinary{{Path: "/**"}},
		},
	}
	if !reflect.DeepEqual(p.NetworkPolicies, want) {
		t.Fatalf("rules =\n%+v\nwant\n%+v", p.NetworkPolicies, want)
	}
}

func TestBuildSandboxPolicyImageMode(t *testing.T) {
	cfg := policyConfig("http://sundae:23232")
	cfg.OpenShell.SandboxPolicy = config.SandboxPolicyImage
	cfg.Snapshots.S3 = config.S3SnapshotConfig{Enabled: true, Region: "eu-west-2", Bucket: "snaps"}
	p, err := BuildSandboxPolicy(cfg)
	if err != nil || p != nil {
		t.Fatalf("image mode: policy = %+v, err = %v; want none", p, err)
	}
	// Even with a clone URL no policy could allow.
	cfg.OpenShell.CloneURL = "ssh://x"
	if p, err := BuildSandboxPolicy(cfg); err != nil || p != nil {
		t.Fatalf("image mode, bad clone URL: policy = %+v, err = %v", p, err)
	}
}

func TestEnsureSandboxSendsPolicy(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClient()
	c := newSDKClient("default", fc.Sandboxes(), nil, nil, nil)
	policy, err := BuildSandboxPolicy(policyConfig("http://sundae:23232"))
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{LabelUserID: "1"}

	sb, created, err := c.EnsureSandbox(ctx, "sd-a", SandboxRequest{Image: "img", Labels: labels, Policy: policy})
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if !reflect.DeepEqual(sb.Spec.Policy, policy) {
		t.Fatalf("spec policy = %+v, want %+v", sb.Spec.Policy, policy)
	}

	// No policy: the spec has none, so the image's or OpenShell's applies.
	sb, _, err = c.EnsureSandbox(ctx, "sd-b", SandboxRequest{Image: "img", Labels: labels})
	if err != nil || sb.Spec.Policy != nil {
		t.Fatalf("no policy: spec policy = %+v, err = %v", sb.Spec.Policy, err)
	}
}

func TestStartCreatesSandboxWithPolicy(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Progress: &strings.Builder{}})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := h.fc.Sandboxes().Get(ctx, "default", s.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	// The harness's clone URL is http://sundae.svc:23232.
	p := sb.Spec.Policy
	if p == nil || len(p.NetworkPolicies) != 1 {
		t.Fatalf("spec policy = %+v", p)
	}
	if ep := p.NetworkPolicies["sundae_git"].Endpoints; len(ep) != 1 || ep[0].Host != "sundae.svc" || ep[0].Port != 23232 {
		t.Fatalf("sundae_git = %+v", p.NetworkPolicies["sundae_git"])
	}

	// With sandbox_policy: image, none.
	cfg := newHarnessConfig()
	cfg.OpenShell.SandboxPolicy = config.SandboxPolicyImage
	h = newHarnessWith(t, cfg)
	s, err = h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Progress: &strings.Builder{}})
	if err != nil {
		t.Fatal(err)
	}
	if sb, err = h.fc.Sandboxes().Get(ctx, "default", s.Sandbox); err != nil || sb.Spec.Policy != nil {
		t.Fatalf("image mode: spec policy = %+v, err = %v", sb.Spec.Policy, err)
	}

	// A policy that cannot be built stops the create; no sandbox is made.
	cfg = newHarnessConfig()
	cfg.OpenShell.CloneURL = "ssh://sundae.svc"
	h = newHarnessWith(t, cfg)
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Progress: &strings.Builder{}}); err == nil || !strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("bad clone URL: err = %v", err)
	}
	if sbs, err := h.fc.Sandboxes().ListAll(ctx, "default", v1.ListOptions{}); err != nil || len(sbs) != 0 {
		t.Fatalf("bad clone URL: sandboxes = %v, err = %v", sbs, err)
	}
}
