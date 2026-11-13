package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Sandbox policy modes: the values openshell.sandbox_policy accepts.
const (
	// SandboxPolicySundae makes Sundae send each sandbox it creates a
	// policy: OpenShell's restrictive default with network rules for
	// Sundae's git endpoint, the S3 snapshot endpoint when snapshots are
	// on, and openshell.network_policies. The default.
	SandboxPolicySundae = "sundae"
	// SandboxPolicyImage sends no policy: the image's
	// /etc/openshell/policy.yaml applies, or OpenShell's default, which
	// denies all network access.
	SandboxPolicyImage = "image"
)

// SandboxPolicyModes lists every valid openshell.sandbox_policy value.
var SandboxPolicyModes = []string{SandboxPolicySundae, SandboxPolicyImage}

// Network rule names. The rules Sundae adds itself use the reserved
// "sundae_" prefix; OpenShell reserves "_provider_" for the rules it
// derives from attached providers, which the name pattern already excludes.
const (
	// NetworkPolicyReservedPrefix starts the names of the network rules
	// Sundae adds; openshell.network_policies may not use it.
	NetworkPolicyReservedPrefix = "sundae_"
	// NetworkPolicyGit is the rule that lets the sandbox reach Sundae's
	// HTTP git endpoint (OpenShellCloneURL).
	NetworkPolicyGit = NetworkPolicyReservedPrefix + "git"
	// NetworkPolicySnapshots is the rule that lets the sandbox reach the
	// S3 snapshot endpoint, when snapshots.s3 is enabled.
	NetworkPolicySnapshots = NetworkPolicyReservedPrefix + "snapshots"
)

// Network endpoint protocols and access presets accepted in
// openshell.network_policies. They are a subset of what OpenShell 0.1.8
// accepts (crates/openshell-policy/src/l7_validate.rs): the protocols that
// need explicit L7 rules are left to image policies.
const (
	// NetworkProtocolL4 (no protocol) passes connections through the
	// sandbox proxy without inspecting requests. The usual choice.
	NetworkProtocolL4 = ""
	// NetworkProtocolREST inspects HTTP requests; it needs an access
	// preset.
	NetworkProtocolREST = "rest"
	// NetworkProtocolTCP is native transparent TCP; the host must be a DNS
	// name.
	NetworkProtocolTCP = "tcp"
)

// NetworkAccessPresets lists the access presets accepted with protocol
// rest.
var NetworkAccessPresets = []string{"read-only", "read-write", "full"}

// OpenShellNetworkPolicy is an extra network rule added to the policy
// Sundae gives the sandboxes it creates (openshell.network_policies, with
// openshell.sandbox_policy: sundae). The environment form is
// SUNDAE_OPENSHELL_NETWORK_POLICIES_<i>_<KEY>, and for its endpoints
// SUNDAE_OPENSHELL_NETWORK_POLICIES_<i>_ENDPOINTS_<j>_<KEY>; list values
// are comma-separated.
type OpenShellNetworkPolicy struct {
	// Name is the rule's name in the policy: 1 to 63 letters, digits, '_',
	// '-' and '.', starting with a letter or digit, and not starting with
	// "sundae_".
	Name string `env:"NAME" yaml:"name"`

	// Endpoints are the destinations the rule allows. At least one.
	Endpoints []OpenShellNetworkEndpoint `envPrefix:"ENDPOINTS_" yaml:"endpoints"`

	// Binaries are the absolute paths, or globs, of the programs the rule
	// allows; a rule also covers the processes they start. Empty means
	// any program ("/**").
	Binaries []string `env:"BINARIES" envSeparator:"," yaml:"binaries"`
}

// OpenShellNetworkEndpoint is a destination an OpenShellNetworkPolicy
// allows.
type OpenShellNetworkEndpoint struct {
	// Host is a DNS name, an IP address, or a wildcard name whose first
	// label is "*" or "**" (e.g. *.example.com). An exact name may
	// resolve to a private address; a wildcard needs allowed_ips for
	// that. Empty is allowed with allowed_ips: any host resolving into
	// them.
	Host string `env:"HOST" yaml:"host"`

	// Port is the destination port. Set it or Ports.
	Port int `env:"PORT" yaml:"port"`

	// Ports are several destination ports. Set them or Port.
	Ports []int `env:"PORTS" envSeparator:"," yaml:"ports"`

	// Protocol is empty (the default: connections pass through the
	// sandbox proxy uninspected, which suits git, curl and most
	// clients), "rest" (HTTP requests are inspected and Access applies)
	// or "tcp" (native transparent TCP, for non-HTTP clients).
	Protocol string `env:"PROTOCOL" yaml:"protocol"`

	// Access is the access preset with protocol rest: read-only,
	// read-write or full. Required with rest, not allowed otherwise.
	Access string `env:"ACCESS" yaml:"access"`

	// AllowedIPs are IP addresses or CIDR ranges every address the host
	// resolves to must be in.
	AllowedIPs []string `env:"ALLOWED_IPS" envSeparator:"," yaml:"allowed_ips"`
}

// networkPolicyNameRe matches a valid openshell.network_policies name.
var networkPolicyNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// validateSandboxPolicy validates openshell.sandbox_policy and
// openshell.network_policies, normalising them. Every problem is
// reported, each naming its key.
func (c *Config) validateSandboxPolicy() error {
	oc := &c.OpenShell
	var errs []error
	oc.SandboxPolicy = strings.ToLower(strings.TrimSpace(oc.SandboxPolicy))
	if oc.SandboxPolicy == "" {
		oc.SandboxPolicy = SandboxPolicySundae
	}
	if !slices.Contains(SandboxPolicyModes, oc.SandboxPolicy) {
		errs = append(errs, fmt.Errorf("openshell.sandbox_policy %q must be %s", oc.SandboxPolicy, strings.Join(SandboxPolicyModes, " or ")))
	}
	if oc.SandboxPolicy == SandboxPolicyImage && len(oc.NetworkPolicies) > 0 {
		errs = append(errs, fmt.Errorf("openshell.network_policies needs openshell.sandbox_policy: %s; with %s, put the rules in the image's /etc/openshell/policy.yaml", SandboxPolicySundae, SandboxPolicyImage))
	}
	if oc.Enabled && oc.SandboxPolicy == SandboxPolicySundae {
		if _, _, err := URLHostPort(c.OpenShellCloneURL()); err != nil {
			errs = append(errs, fmt.Errorf("openshell: the clone URL %q (openshell.clone_url, or http.public_url) %v; the sandbox policy allows it", c.OpenShellCloneURL(), err))
		}
	}

	names := map[string]int{}
	for i := range oc.NetworkPolicies {
		p := &oc.NetworkPolicies[i]
		p.Name = strings.TrimSpace(p.Name)
		key := fmt.Sprintf("openshell.network_policies[%d]", i)
		if p.Name != "" {
			key = fmt.Sprintf("openshell.network_policies[%d] (%s)", i, p.Name)
		}
		fail := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("%s: "+format, append([]any{key}, args...)...))
		}

		switch {
		case p.Name == "":
			fail("name is required")
		case !networkPolicyNameRe.MatchString(p.Name):
			fail("name %q must be 1 to 63 letters, digits, '_', '-' and '.', starting with a letter or digit", p.Name)
		case strings.HasPrefix(p.Name, NetworkPolicyReservedPrefix):
			fail("name %q uses the %q prefix Sundae reserves for its own rules (%s, %s)", p.Name, NetworkPolicyReservedPrefix, NetworkPolicyGit, NetworkPolicySnapshots)
		default:
			if j, ok := names[p.Name]; ok {
				fail("name %q is already used by openshell.network_policies[%d]", p.Name, j)
			} else {
				names[p.Name] = i
			}
		}

		if len(p.Endpoints) == 0 {
			fail("endpoints: at least one is required")
		}
		for j := range p.Endpoints {
			for _, err := range validateNetworkEndpoint(&p.Endpoints[j]) {
				fail("endpoints[%d]: %v", j, err)
			}
		}

		for j, b := range p.Binaries {
			b = strings.TrimSpace(b)
			p.Binaries[j] = b
			if !strings.HasPrefix(b, "/") || strings.Contains(b, "/../") || strings.HasSuffix(b, "/..") {
				fail("binaries[%d] %q must be an absolute path or glob, e.g. /usr/bin/curl or /**", j, b)
			}
		}
	}
	return errors.Join(errs...)
}

// validateNetworkEndpoint normalises an endpoint and returns its problems.
func validateNetworkEndpoint(ep *OpenShellNetworkEndpoint) []error {
	var errs []error
	ep.Host = strings.ToLower(strings.TrimSpace(ep.Host))
	ep.Protocol = strings.ToLower(strings.TrimSpace(ep.Protocol))
	ep.Access = strings.ToLower(strings.TrimSpace(ep.Access))

	_, ipErr := netip.ParseAddr(ep.Host)
	isIP := ipErr == nil
	switch {
	case ep.Host == "" && len(ep.AllowedIPs) == 0:
		errs = append(errs, errors.New("host or allowed_ips is required"))
	case ep.Host == "":
	case strings.ContainsAny(ep.Host, "/ \t") || (strings.Contains(ep.Host, ":") && !isIP):
		errs = append(errs, fmt.Errorf("host %q must be a host name or IP address, without a scheme, port or path", ep.Host))
	case strings.Contains(ep.Host, "*"):
		if err := validWildcardHost(ep.Host); err != nil {
			errs = append(errs, fmt.Errorf("host %q %v", ep.Host, err))
		}
	}

	switch {
	case ep.Port == 0 && len(ep.Ports) == 0:
		errs = append(errs, errors.New("port or ports is required"))
	case ep.Port != 0 && len(ep.Ports) > 0:
		errs = append(errs, errors.New("set port or ports, not both"))
	}
	if ep.Port < 0 || ep.Port > 65535 {
		errs = append(errs, fmt.Errorf("port %d is out of range 1-65535", ep.Port))
	}
	for k, p := range ep.Ports {
		if p < 1 || p > 65535 {
			errs = append(errs, fmt.Errorf("ports[%d] %d is out of range 1-65535", k, p))
		}
	}

	switch ep.Protocol {
	case NetworkProtocolL4:
	case NetworkProtocolREST:
		if ep.Access == "" {
			errs = append(errs, fmt.Errorf("protocol rest needs access: %s", strings.Join(NetworkAccessPresets, ", ")))
		}
	case NetworkProtocolTCP:
		if ep.Host == "" || isIP {
			errs = append(errs, errors.New("protocol tcp needs a DNS host name"))
		}
	default:
		errs = append(errs, fmt.Errorf("protocol %q must be empty (the default), rest or tcp; other OpenShell protocols need an image policy", ep.Protocol))
	}
	switch {
	case ep.Access == "":
	case !slices.Contains(NetworkAccessPresets, ep.Access):
		errs = append(errs, fmt.Errorf("access %q must be %s", ep.Access, strings.Join(NetworkAccessPresets, ", ")))
	case ep.Protocol != NetworkProtocolREST:
		errs = append(errs, errors.New("access needs protocol: rest"))
	}

	for k, ip := range ep.AllowedIPs {
		ip = strings.TrimSpace(ip)
		ep.AllowedIPs[k] = ip
		if _, err := netip.ParseAddr(ip); err == nil {
			continue
		}
		if _, err := netip.ParsePrefix(ip); err != nil {
			errs = append(errs, fmt.Errorf("allowed_ips[%d] %q is not an IP address or CIDR range", k, ip))
		}
	}
	return errs
}

// validWildcardHost checks a wildcard host the way OpenShell 0.1.8 does
// (crates/openshell-policy/src/lib.rs, host_wildcard_shape_invalid and the
// TLD wildcard check): only the first label may hold a wildcard, "**" only
// as the whole label, and a leading wildcard needs at least two more
// labels.
func validWildcardHost(host string) error {
	labels := strings.Split(host, ".")
	first := labels[0]
	switch {
	case host == "*" || host == "**":
		return errors.New("matches every host; name the hosts")
	case strings.Contains(first, "**") && first != "**":
		return errors.New("may use ** only as a whole label")
	}
	for _, l := range labels[1:] {
		if strings.Contains(l, "*") {
			return errors.New("may only have a wildcard in its first label")
		}
	}
	if (first == "*" || first == "**") && len(labels) <= 2 {
		return errors.New("is a top-level domain wildcard; name a domain, e.g. *.example.com")
	}
	return nil
}

// URLHostPort returns the host name and port an http or https URL
// connects to: its explicit port, else 80 or 443 by scheme.
func URLHostPort(raw string) (string, int, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", 0, fmt.Errorf("is not a URL: %w", err)
	case u.Scheme != "http" && u.Scheme != "https":
		return "", 0, errors.New("must be an http or https URL")
	case u.Hostname() == "":
		return "", 0, errors.New("has no host")
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("has an invalid port %q", p)
		}
		port = n
	}
	return strings.ToLower(u.Hostname()), port, nil
}

// sandboxPolicyEnviron returns openshell.sandbox_policy and
// openshell.network_policies as SUNDAE_OPENSHELL_* environment variables.
func (c *Config) sandboxPolicyEnviron() []string {
	envs := []string{"SUNDAE_OPENSHELL_SANDBOX_POLICY=" + c.OpenShell.SandboxPolicy}
	ints := func(ns []int) string {
		s := make([]string, len(ns))
		for i, n := range ns {
			s[i] = strconv.Itoa(n)
		}
		return strings.Join(s, ",")
	}
	for i, p := range c.OpenShell.NetworkPolicies {
		pre := fmt.Sprintf("SUNDAE_OPENSHELL_NETWORK_POLICIES_%d_", i)
		envs = append(envs, pre+"NAME="+p.Name)
		if len(p.Binaries) > 0 {
			envs = append(envs, pre+"BINARIES="+strings.Join(p.Binaries, ","))
		}
		for j, ep := range p.Endpoints {
			epre := fmt.Sprintf("%sENDPOINTS_%d_", pre, j)
			envs = append(envs,
				epre+"HOST="+ep.Host,
				epre+"PORT="+strconv.Itoa(ep.Port),
				epre+"PROTOCOL="+ep.Protocol,
				epre+"ACCESS="+ep.Access,
			)
			if len(ep.Ports) > 0 {
				envs = append(envs, epre+"PORTS="+ints(ep.Ports))
			}
			if len(ep.AllowedIPs) > 0 {
				envs = append(envs, epre+"ALLOWED_IPS="+strings.Join(ep.AllowedIPs, ","))
			}
		}
	}
	return envs
}
