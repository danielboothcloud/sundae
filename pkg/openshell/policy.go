package openshell

import (
	"fmt"
	"net/netip"
	"slices"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/s3store"
)

// SandboxPolicy is an OpenShell sandbox policy.
type SandboxPolicy = v1.SandboxPolicy

// anyBinary is the binary glob that matches every program in the sandbox.
const anyBinary = "/**"

// restrictiveDefaultPolicy returns OpenShell's restrictive default sandbox
// policy, the one a sandbox gets when neither the gateway nor its image
// has one: version 1, the working directory (/sandbox) and /tmp writable,
// the system directories read-only, Landlock where the kernel has it, and
// no network access. It mirrors restrictive_default_policy() in OpenShell
// 0.1.8, crates/openshell-policy/src/lib.rs, plus read-write /dev/pts,
// /dev/ptmx and /dev/tty: osewa's holder opens a pseudo-terminal for each
// session, which Landlock otherwise denies (EACCES). The process identity is left
// unset, as there: the compute driver fills it in (sandbox:sandbox on
// Kubernetes).
func restrictiveDefaultPolicy() *SandboxPolicy {
	return &SandboxPolicy{
		Version: 1,
		Filesystem: &v1.FilesystemPolicy{
			IncludeWorkdir: true,
			ReadOnly:       []string{"/bin", "/usr", "/lib", "/proc", "/dev/urandom", "/etc", "/var/log"},
			ReadWrite:      []string{"/tmp", "/dev/null", "/dev/pts", "/dev/ptmx", "/dev/tty"},
		},
		Landlock:        &v1.LandlockPolicy{Compatibility: "best_effort"},
		NetworkPolicies: map[string]v1.NetworkPolicyRule{},
	}
}

// BuildSandboxPolicy returns the policy Sundae gives the sandboxes it
// creates, or nil with openshell.sandbox_policy: image, when the image's
// policy or OpenShell's default applies instead.
//
// The policy is OpenShell's restrictive default (restrictiveDefaultPolicy)
// with these network rules:
//
//   - sundae_git: Sundae's HTTP git endpoint (cfg.OpenShellCloneURL), the
//     host and port git and jj clone, fetch and push over;
//   - sundae_snapshots, with snapshots.s3 enabled: the host and port the
//     presigned snapshot URLs point at, which curl in the sandbox uploads
//     to and downloads from. With virtual-hosted addressing that is
//     <bucket>.<endpoint host>, e.g. <bucket>.s3.<region>.amazonaws.com for
//     AWS; with path_style, the endpoint's host. The bucket is fixed, so
//     the host is exact and no wildcard is needed;
//   - each of openshell.network_policies.
//
// Sundae's own rules allow any program ("/**") and inspect nothing (no
// protocol): connections go through the sandbox proxy as they are, so
// HTTPS needs no proxy CA. A host that is an IP address is also pinned
// with allowed_ips. Providers attached to the sandbox add their own rules
// on the gateway.
func BuildSandboxPolicy(cfg *config.Config) (*SandboxPolicy, error) {
	if cfg.OpenShell.SandboxPolicy == config.SandboxPolicyImage {
		return nil, nil
	}
	p := restrictiveDefaultPolicy()

	host, port, err := config.URLHostPort(cfg.OpenShellCloneURL())
	if err != nil {
		return nil, fmt.Errorf("openshell: sandbox policy: clone URL %q %w", cfg.OpenShellCloneURL(), err)
	}
	p.NetworkPolicies[config.NetworkPolicyGit] = builtinRule(config.NetworkPolicyGit, host, port)

	if cfg.Snapshots.S3.Enabled {
		st, err := s3store.New(cfg.Snapshots.S3)
		if err != nil {
			return nil, fmt.Errorf("openshell: sandbox policy: snapshots: %w", err)
		}
		host, port, err := config.URLHostPort(st.BucketURL().String())
		if err != nil {
			return nil, fmt.Errorf("openshell: sandbox policy: snapshots endpoint %w", err)
		}
		p.NetworkPolicies[config.NetworkPolicySnapshots] = builtinRule(config.NetworkPolicySnapshots, host, port)
	}

	for _, r := range cfg.OpenShell.NetworkPolicies {
		if _, ok := p.NetworkPolicies[r.Name]; ok {
			return nil, fmt.Errorf("openshell: sandbox policy: network rule %q is defined twice", r.Name)
		}
		p.NetworkPolicies[r.Name] = configuredRule(r)
	}
	return p, nil
}

// builtinRule returns a rule allowing any program to reach host:port
// through the sandbox proxy, uninspected.
func builtinRule(name, host string, port int) v1.NetworkPolicyRule {
	ep := v1.PolicyNetworkEndpoint{Host: host, Port: uint32(port)} //nolint:gosec // 1-65535, checked by URLHostPort
	if addr, err := netip.ParseAddr(host); err == nil {
		ep.AllowedIPs = []string{addr.String()}
	}
	return v1.NetworkPolicyRule{
		Name:      name,
		Endpoints: []v1.PolicyNetworkEndpoint{ep},
		Binaries:  []v1.PolicyNetworkBinary{{Path: anyBinary}},
	}
}

// accessPresets maps an openshell.network_policies access value to the
// OpenShell preset.
var accessPresets = map[string]v1.NetworkAccessPreset{
	"":           v1.NetworkAccessPresetUnspecified,
	"read-only":  v1.NetworkAccessPresetReadOnly,
	"read-write": v1.NetworkAccessPresetReadWrite,
	"full":       v1.NetworkAccessPresetFull,
}

// configuredRule converts a validated openshell.network_policies entry.
// No binaries means any program.
func configuredRule(r config.OpenShellNetworkPolicy) v1.NetworkPolicyRule {
	rule := v1.NetworkPolicyRule{Name: r.Name}
	for _, e := range r.Endpoints {
		ep := v1.PolicyNetworkEndpoint{
			Host:       e.Host,
			Port:       uint32(e.Port), //nolint:gosec // 0-65535, checked by config validation
			Protocol:   e.Protocol,
			Access:     accessPresets[e.Access],
			AllowedIPs: slices.Clone(e.AllowedIPs),
		}
		for _, p := range e.Ports {
			ep.Ports = append(ep.Ports, uint32(p)) //nolint:gosec // 1-65535, checked by config validation
		}
		rule.Endpoints = append(rule.Endpoints, ep)
	}
	bins := r.Binaries
	if len(bins) == 0 {
		bins = []string{anyBinary}
	}
	for _, b := range bins {
		rule.Binaries = append(rule.Binaries, v1.PolicyNetworkBinary{Path: b})
	}
	return rule
}
