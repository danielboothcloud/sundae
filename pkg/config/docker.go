package config

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// DockerConfig configures the docker session runtime (RuntimeDocker): each
// session is a container, with a named volume mounted at /sandbox, on a
// Docker (or Docker-compatible) engine.
type DockerConfig struct {
	// Host is the engine's address: unix:///path/to/socket or
	// tcp://host:port. Empty uses $DOCKER_HOST, else the default socket.
	Host string `env:"HOST" yaml:"host"`

	// TLSCA, TLSCert and TLSKey are PEM files for a tcp:// host with TLS:
	// the CA to verify the engine with, and the client certificate and
	// key. Relative paths are resolved against the data path.
	TLSCA   string `env:"TLS_CA" yaml:"tls_ca"`
	TLSCert string `env:"TLS_CERT" yaml:"tls_cert"`
	TLSKey  string `env:"TLS_KEY" yaml:"tls_key"`

	// ReadyTimeout bounds how long a start waits for its container,
	// pulling the image included. 0 means DefaultReadyTimeout.
	ReadyTimeout time.Duration `env:"READY_TIMEOUT" yaml:"ready_timeout"`

	// DockerSettings are the settings of every session's container, which
	// a profile's docker: section overrides.
	DockerSettings `yaml:",inline"`
}

// DockerSettings are the settings of a session's container. Profiles
// override them (sessions.profiles.<name>.docker): a value that is set
// replaces the default, except labels and volume_options, whose keys are
// merged, the profile's winning. For the lists, null (unset) inherits and
// [] sets none.
type DockerSettings struct {
	// Image is the sandbox image. Empty uses openshell.image. A profile's
	// image, if set, wins.
	Image string `env:"IMAGE" yaml:"image"`
	// User is the user the container runs as, user[:group]. Empty is the
	// image's.
	User string `env:"USER" yaml:"user"`
	// Network is the network the container joins. Empty is the engine's
	// default (bridge).
	Network string `env:"NETWORK" yaml:"network"`
	// Runtime is the engine's OCI runtime for the container, e.g. runsc.
	// Empty is the engine's default.
	Runtime string `env:"RUNTIME" yaml:"runtime"`
	// VolumeDriver and VolumeOptions create /sandbox's named volume.
	// Empty driver is the engine's default (local).
	VolumeDriver  string            `env:"VOLUME_DRIVER" yaml:"volume_driver"`
	VolumeOptions map[string]string `yaml:"volume_options"`
	// Memory and CPUs limit the container, as Kubernetes quantities
	// (4Gi, 1.5 or 1500m). Empty is no limit.
	Memory string `env:"MEMORY" yaml:"memory"`
	CPUs   string `env:"CPUS" yaml:"cpus"`
	// CapAdd and CapDrop change the container's capabilities. Unset
	// CapDrop is DefaultDockerCapDrop.
	CapAdd  []string `yaml:"cap_add"`
	CapDrop []string `yaml:"cap_drop"`
	// SecurityOpt are the engine's security options, e.g.
	// seccomp=unconfined. Unset is DefaultDockerSecurityOpt.
	SecurityOpt []string `yaml:"security_opt"`
	// Devices are host devices for the container, as
	// host-path[:container-path[:permissions]].
	Devices []string `yaml:"devices"`
	// Labels are added to the container, next to Sundae's own.
	Labels map[string]string `yaml:"labels"`
}

// DefaultDockerCapDrop is docker.cap_drop when unset: no capabilities.
func DefaultDockerCapDrop() []string { return []string{"ALL"} }

// DefaultDockerSecurityOpt is docker.security_opt when unset: no
// privilege escalation.
func DefaultDockerSecurityOpt() []string { return []string{"no-new-privileges:true"} }

// DockerSettingsFor returns the docker settings of the named profile's
// sessions: docker: with the profile's docker: section applied.
func (c *Config) DockerSettingsFor(profile string) DockerSettings {
	s := c.Docker.DockerSettings
	s.Labels = maps.Clone(s.Labels)
	s.VolumeOptions = maps.Clone(s.VolumeOptions)
	p := c.Sessions.Profiles[profile].Docker
	if p == nil {
		return s
	}
	for _, f := range []struct{ dst, src *string }{
		{&s.Image, &p.Image},
		{&s.User, &p.User},
		{&s.Network, &p.Network},
		{&s.Runtime, &p.Runtime},
		{&s.VolumeDriver, &p.VolumeDriver},
		{&s.Memory, &p.Memory},
		{&s.CPUs, &p.CPUs},
	} {
		if *f.src != "" {
			*f.dst = *f.src
		}
	}
	for _, f := range []struct{ dst, src *[]string }{
		{&s.CapAdd, &p.CapAdd},
		{&s.CapDrop, &p.CapDrop},
		{&s.SecurityOpt, &p.SecurityOpt},
		{&s.Devices, &p.Devices},
	} {
		if *f.src != nil {
			*f.dst = *f.src
		}
	}
	s.Labels = mergeStrings(s.Labels, p.Labels)
	s.VolumeOptions = mergeStrings(s.VolumeOptions, p.VolumeOptions)
	return s
}

// NanoCPUs returns CPUs in units of 10^-9 CPUs; 0 for no limit.
func (s DockerSettings) NanoCPUs() int64 {
	if s.CPUs == "" {
		return 0
	}
	q, err := resource.ParseQuantity(s.CPUs)
	if err != nil {
		return 0
	}
	return q.MilliValue() * 1_000_000
}

// MemoryBytes returns Memory in bytes; 0 for no limit.
func (s DockerSettings) MemoryBytes() int64 {
	if s.Memory == "" {
		return 0
	}
	q, err := resource.ParseQuantity(s.Memory)
	if err != nil {
		return 0
	}
	return q.Value()
}

// dockerNameRe matches a Docker object name.
var dockerNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// validateDocker validates docker: and the profiles' docker: sections,
// and fills in defaults.
func (c *Config) validateDocker() error {
	dc := &c.Docker
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	dc.Host = strings.TrimSpace(dc.Host)
	if dc.Host != "" {
		u, err := url.Parse(dc.Host)
		switch {
		case err != nil:
			fail("docker.host %q: %v", dc.Host, err)
		case u.Scheme != "unix" && u.Scheme != "tcp":
			fail("docker.host %q must be unix:///path or tcp://host:port", dc.Host)
		case u.Scheme == "unix" && u.Path == "":
			fail("docker.host %q has no socket path", dc.Host)
		case u.Scheme == "tcp" && u.Host == "":
			fail("docker.host %q has no host", dc.Host)
		}
	}
	for _, p := range []*string{&dc.TLSCA, &dc.TLSCert, &dc.TLSKey} {
		*p = strings.TrimSpace(*p)
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(c.DataPath, *p)
		}
	}
	if (dc.TLSCert == "") != (dc.TLSKey == "") {
		fail("docker.tls_cert and docker.tls_key must be set together")
	}
	if (dc.TLSCA != "" || dc.TLSCert != "") && !strings.HasPrefix(dc.Host, "tcp://") {
		fail("docker.tls_* need a tcp:// docker.host")
	}
	switch {
	case dc.ReadyTimeout == 0:
		dc.ReadyTimeout = DefaultReadyTimeout
	case dc.ReadyTimeout < MinReadyTimeout:
		fail("docker.ready_timeout %s is too short: use at least %s", dc.ReadyTimeout, MinReadyTimeout)
	}
	if dc.CapDrop == nil {
		dc.CapDrop = DefaultDockerCapDrop()
	}
	if dc.SecurityOpt == nil {
		dc.SecurityOpt = DefaultDockerSecurityOpt()
	}
	errs = append(errs, validateDockerSettings(&dc.DockerSettings, "docker"))

	for _, name := range sortedKeys(c.Sessions.Profiles) {
		p := c.Sessions.Profiles[name]
		key := "sessions.profiles." + name
		if p.Docker != nil {
			errs = append(errs, validateDockerSettings(p.Docker, key+".docker"))
			if p.Image != "" && p.Docker.Image != "" {
				fail("%s: set image or docker.image, not both", key)
			}
		}
		c.Sessions.Profiles[name] = p
	}

	if c.SessionsEnabled() && slices.Contains(c.RuntimesInUse(), RuntimeDocker) {
		for _, name := range append([]string{""}, sortedKeys(c.Sessions.Profiles)...) {
			if c.ProfileRuntime(name) == RuntimeDocker && c.ProfileImage(name) == "" {
				if name == "" {
					fail("docker.image (or openshell.image) is required: sessions.runtime is %s", RuntimeDocker)
				} else {
					fail("sessions.profiles.%s runs in the %s runtime and has no image: set its image, docker.image or openshell.image", name, RuntimeDocker)
				}
				break
			}
		}
	}
	return errors.Join(errs...)
}

// validateDockerSettings checks s, reporting problems under key, and
// trims its strings.
func validateDockerSettings(s *DockerSettings, key string) error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(key+"."+format, args...))
	}
	for _, p := range []*string{&s.Image, &s.User, &s.Network, &s.Runtime, &s.VolumeDriver, &s.Memory, &s.CPUs} {
		*p = strings.TrimSpace(*p)
	}
	if s.Memory != "" {
		if q, err := resource.ParseQuantity(s.Memory); err != nil || q.Sign() <= 0 {
			fail("memory %q is not a positive quantity, e.g. 4Gi", s.Memory)
		}
	}
	if s.CPUs != "" {
		if q, err := resource.ParseQuantity(s.CPUs); err != nil || q.MilliValue() <= 0 {
			fail("cpus %q is not a positive quantity, e.g. 2 or 500m", s.CPUs)
		}
	}
	for field, list := range map[string][]string{"cap_add": s.CapAdd, "cap_drop": s.CapDrop, "security_opt": s.SecurityOpt, "devices": s.Devices} {
		for i, v := range list {
			if strings.TrimSpace(v) == "" {
				fail("%s[%d] is empty", field, i)
			}
		}
	}
	for _, d := range s.Devices {
		if d != "" && !strings.HasPrefix(d, "/") {
			fail("devices: %q must start with a host path", d)
		}
	}
	for _, k := range sortedKeys(s.Labels) {
		if strings.HasPrefix(k, "sundae.") {
			fail("labels: key %q: the sundae. prefix is Sundae's own", k)
		}
	}
	if s.Network != "" && !dockerNameRe.MatchString(s.Network) {
		fail("network %q is not a Docker network name: letters, digits, '_', '.' and '-', starting with a letter or digit", s.Network)
	}
	return errors.Join(errs...)
}

// dockerEnviron returns docker:'s scalar settings as SUNDAE_DOCKER_*
// environment variables. The maps and lists have no environment form.
func (c *Config) dockerEnviron() []string {
	return []string{
		"SUNDAE_DOCKER_HOST=" + c.Docker.Host,
		"SUNDAE_DOCKER_TLS_CA=" + c.Docker.TLSCA,
		"SUNDAE_DOCKER_TLS_CERT=" + c.Docker.TLSCert,
		"SUNDAE_DOCKER_TLS_KEY=" + c.Docker.TLSKey,
		"SUNDAE_DOCKER_READY_TIMEOUT=" + c.Docker.ReadyTimeout.String(),
		"SUNDAE_DOCKER_IMAGE=" + c.Docker.Image,
		"SUNDAE_DOCKER_USER=" + c.Docker.User,
		"SUNDAE_DOCKER_NETWORK=" + c.Docker.Network,
		"SUNDAE_DOCKER_RUNTIME=" + c.Docker.Runtime,
		"SUNDAE_DOCKER_VOLUME_DRIVER=" + c.Docker.VolumeDriver,
		"SUNDAE_DOCKER_MEMORY=" + c.Docker.Memory,
		"SUNDAE_DOCKER_CPUS=" + c.Docker.CPUs,
	}
}
