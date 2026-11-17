package config

import (
	"testing"

	"github.com/matryer/is"
)

func TestResolveRuntimeClass(t *testing.T) {
	cases := map[string]struct {
		names []string
		want  string
	}{
		"none":                    {nil, ""},
		"all empty":               {[]string{"", " "}, ""},
		"first set wins":          {[]string{"kata-containers", "gvisor"}, "kata-containers"},
		"empty falls through":     {[]string{"", "kata-containers"}, "kata-containers"},
		"default stops":           {[]string{RuntimeClassDefault, "kata-containers"}, ""},
		"default after a class":   {[]string{"kata-qemu", RuntimeClassDefault}, "kata-qemu"},
		"spaces are trimmed":      {[]string{" kata-containers "}, "kata-containers"},
		"only default is default": {[]string{RuntimeClassDefault}, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			is.New(t).Equal(ResolveRuntimeClass(tc.names...), tc.want)
		})
	}
}

func TestProfileRuntimeClass(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.Sessions.Profiles = map[string]SessionProfile{
		"podman": {OpenShell: &ProfileOpenShell{RuntimeClass: RuntimeClassKata}},
		"runc":   {OpenShell: &ProfileOpenShell{RuntimeClass: RuntimeClassDefault}},
		"shell":  {},
	}

	// Without openshell.runtime_class, only a profile's own class applies.
	is.Equal(cfg.ProfileRuntimeClass("podman"), RuntimeClassKata)
	is.Equal(cfg.ProfileRuntimeClass("runc"), "")
	is.Equal(cfg.ProfileRuntimeClass("shell"), "")
	is.Equal(cfg.ProfileRuntimeClass(""), "")

	// openshell.runtime_class is the default for profiles that set none,
	// and for sessions without a profile; "default" opts out of it.
	cfg.OpenShell.RuntimeClass = "gvisor"
	is.Equal(cfg.ProfileRuntimeClass("podman"), RuntimeClassKata)
	is.Equal(cfg.ProfileRuntimeClass("runc"), "")
	is.Equal(cfg.ProfileRuntimeClass("shell"), "gvisor")
	is.Equal(cfg.ProfileRuntimeClass(""), "gvisor")
	is.Equal(cfg.ProfileRuntimeClass("unknown"), "gvisor")
}
