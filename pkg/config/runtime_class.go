package config

import (
	"regexp"
	"strings"
)

// Sandbox runtimes. A runtime is chosen by the name of a Kubernetes
// RuntimeClass, which Sundae sends to OpenShell as the sandbox template's
// runtime_class_name when it creates a sandbox. OpenShell's Kubernetes
// driver (0.1.8) puts it on the sandbox pod as runtimeClassName. Without
// one, the driver's default_runtime_class_name applies, else the cluster's
// default runtime (runc).
const (
	// RuntimeClassDefault is the runtime_class value that selects no
	// RuntimeClass: the OpenShell driver's or the cluster's default
	// runtime. A profile uses it to opt out of openshell.runtime_class.
	RuntimeClassDefault = "default"
	// RuntimeClassKata is the RuntimeClass name the Kata Containers
	// deployment (kata-deploy) registers for its default hypervisor; a
	// cluster can name its Kata RuntimeClasses differently.
	RuntimeClassKata = "kata-containers"
	// MaxRuntimeClassLen is the longest RuntimeClass name: a DNS-1123
	// subdomain.
	MaxRuntimeClassLen = 253
)

// runtimeClassRe matches a DNS-1123 subdomain, the form of a Kubernetes
// RuntimeClass name.
var runtimeClassRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// validRuntimeClass reports whether name is a valid runtime_class value:
// empty, RuntimeClassDefault or a RuntimeClass name.
func validRuntimeClass(name string) bool {
	return name == "" || (len(name) <= MaxRuntimeClassLen && runtimeClassRe.MatchString(name))
}

// runtimeClassProblem is the message for an invalid runtime_class value.
const runtimeClassProblem = "is not a Kubernetes RuntimeClass name: lowercase letters, digits, '-' and '.', at most 253 characters, starting and ending with a letter or digit"

// ResolveRuntimeClass returns the RuntimeClass name a sandbox is created
// with: the first of names that is not empty, or "" when the first such
// name is RuntimeClassDefault or all are empty. "" means no RuntimeClass:
// the OpenShell driver's or the cluster's default runtime.
func ResolveRuntimeClass(names ...string) string {
	for _, n := range names {
		switch n = strings.TrimSpace(n); n {
		case "":
			continue
		case RuntimeClassDefault:
			return ""
		default:
			return n
		}
	}
	return ""
}

// ProfileRuntimeClass returns the RuntimeClass name a new sandbox of the
// named profile is created with: the profile's runtime_class, else
// openshell.runtime_class. "" means the default runtime. An unknown or
// empty profile name uses openshell.runtime_class.
func (c *Config) ProfileRuntimeClass(profile string) string {
	return ResolveRuntimeClass(c.Sessions.Profiles[profile].RuntimeClass, c.OpenShell.RuntimeClass)
}
