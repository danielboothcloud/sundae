package config

import (
	"fmt"
	"slices"
	"strings"
)

// Session runtimes: where a session's sandbox runs. sessions.runtime is the
// default; sessions.profiles.<name>.runtime overrides it for one profile.
const (
	// RuntimeOpenShell runs sandboxes on the OpenShell gateway
	// (openshell:). The default. Only it has providers, the L7 egress
	// policy and suspend.
	RuntimeOpenShell = "openshell"
	// RuntimePod runs each sandbox as a plain Kubernetes pod (pod:).
	RuntimePod = "pod"
	// RuntimeDocker runs each sandbox as a Docker container (docker:).
	RuntimeDocker = "docker"
)

// Runtimes lists the session runtimes Sundae knows.
var Runtimes = []string{RuntimeOpenShell, RuntimePod, RuntimeDocker}

// availableRuntimes are the runtimes this build implements.
var availableRuntimes = []string{RuntimeOpenShell, RuntimePod}

// SessionsEnabled reports whether agent sessions are on: with
// sessions.runtime openshell, when openshell.enabled is; with any other
// runtime, always.
func (c *Config) SessionsEnabled() bool {
	if c.ProfileRuntime("") == RuntimeOpenShell {
		return c.OpenShell.Enabled
	}
	return true
}

// ProfileRuntime returns the runtime of the named profile's sessions: the
// profile's runtime, else sessions.runtime, else RuntimeOpenShell. An
// unknown or empty profile name uses sessions.runtime.
func (c *Config) ProfileRuntime(profile string) string {
	if r := c.Sessions.Profiles[profile].Runtime; r != "" {
		return r
	}
	if c.Sessions.Runtime != "" {
		return c.Sessions.Runtime
	}
	return RuntimeOpenShell
}

// RuntimesInUse returns the runtimes sessions can run in: sessions.runtime
// and every profile's, sessions.runtime first, then in order.
func (c *Config) RuntimesInUse() []string {
	out := []string{c.ProfileRuntime("")}
	for _, name := range sortedKeys(c.Sessions.Profiles) {
		if r := c.ProfileRuntime(name); !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out[1:], func(a, b string) int {
		return slices.Index(Runtimes, a) - slices.Index(Runtimes, b)
	})
	return out
}

// runtimeProblem returns why r is not a valid runtime value, or "".
func runtimeProblem(r string) string {
	switch {
	case !slices.Contains(Runtimes, r):
		return fmt.Sprintf("must be one of %s", strings.Join(Runtimes, ", "))
	case !slices.Contains(availableRuntimes, r):
		return "is not implemented yet"
	}
	return ""
}

// movedKeys maps config keys that were removed to where their setting now
// belongs, for the unknown-key error. The keys are in the form
// describeKeyError reports them.
var movedKeys = map[string]string{
	"sessions.profiles.<name>.providers":     "sessions.profiles.<name>.openshell.providers",
	"sessions.profiles.<name>.runtime_class": "sessions.profiles.<name>.openshell.runtime_class",
}
