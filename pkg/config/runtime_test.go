package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
)

func TestSessionRuntimeDefaults(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Sessions.Profiles = map[string]SessionProfile{"shell": {}}
	is.NoErr(cfg.Validate())
	// No runtime key anywhere: everything runs on OpenShell, as before.
	is.Equal(cfg.Sessions.Runtime, RuntimeOpenShell)
	is.Equal(cfg.ProfileRuntime("shell"), RuntimeOpenShell)
	is.Equal(cfg.ProfileRuntime(""), RuntimeOpenShell)
	is.Equal(cfg.ProfileRuntime("unknown"), RuntimeOpenShell)
	is.Equal(cfg.RuntimesInUse(), []string{RuntimeOpenShell})
}

func TestSessionRuntimeValidation(t *testing.T) {
	cases := map[string]struct {
		set  func(*Config)
		want []string
	}{
		"explicit openshell": {
			func(c *Config) {
				c.Sessions.Runtime = " OpenShell "
				c.Sessions.Profiles["claude"] = SessionProfile{Runtime: RuntimeOpenShell, OpenShell: &ProfileOpenShell{RuntimeClass: "kata"}}
			}, nil,
		},
		"unknown runtime": {
			func(c *Config) { c.Sessions.Runtime = "lxc" },
			[]string{`sessions.runtime "lxc" must be one of openshell, pod, docker`},
		},
		"unknown profile runtime": {
			func(c *Config) { c.Sessions.Profiles["claude"] = SessionProfile{Runtime: "lxc"} },
			[]string{`sessions.profiles.claude.runtime "lxc" must be one of openshell, pod, docker`},
		},
		"runtime not implemented": {
			func(c *Config) { c.Sessions.Profiles["claude"] = SessionProfile{Runtime: RuntimeDocker} },
			[]string{`sessions.profiles.claude.runtime "docker" is not implemented yet`},
		},
		"openshell settings on another runtime": {
			func(c *Config) {
				c.Sessions.Runtime = RuntimePod
				c.Sessions.Profiles["claude"] = SessionProfile{OpenShell: &ProfileOpenShell{Providers: []string{"p"}}}
			},
			[]string{"sessions.profiles.claude.openshell is set, but the profile's runtime is pod", "sessions.profiles.claude.runtime: openshell"},
		},
		"openshell settings on an openshell profile": {
			func(c *Config) {
				c.Sessions.Runtime = RuntimePod
				c.Sessions.Profiles["claude"] = SessionProfile{Runtime: RuntimeOpenShell, OpenShell: &ProfileOpenShell{Providers: []string{"p"}}}
			},
			// Only sessions.runtime: pod is a problem in this build.
			[]string{`sessions.runtime "pod" is not implemented yet`},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.DataPath = t.TempDir()
			cfg.Sessions.Profiles = map[string]SessionProfile{}
			tc.set(cfg)
			err := cfg.Validate()
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("no error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error = %v, want %q", err, w)
				}
			}
			if name == "openshell settings on an openshell profile" && strings.Contains(err.Error(), "profiles.claude.openshell") {
				t.Errorf("error = %v; the openshell profile's settings are fine", err)
			}
		})
	}
}

func TestProfileRuntimeAndRuntimesInUse(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.Sessions.Runtime = RuntimeDocker
	cfg.Sessions.Profiles = map[string]SessionProfile{
		"a": {Runtime: RuntimeOpenShell, OpenShell: &ProfileOpenShell{RuntimeClass: "kata"}},
		"b": {Runtime: RuntimePod},
		"c": {},
	}
	is.Equal(cfg.ProfileRuntime("a"), RuntimeOpenShell)
	is.Equal(cfg.ProfileRuntime("b"), RuntimePod)
	is.Equal(cfg.ProfileRuntime("c"), RuntimeDocker)
	// sessions.runtime first, then the others in Runtimes order.
	is.Equal(cfg.RuntimesInUse(), []string{RuntimeDocker, RuntimeOpenShell, RuntimePod})
	// A RuntimeClass is an OpenShell setting here.
	is.Equal(cfg.ProfileRuntimeClass("a"), "kata")
	is.Equal(cfg.ProfileRuntimeClass("c"), "")
}

func TestMovedProfileKeys(t *testing.T) {
	is := is.New(t)
	td := t.TempDir()
	p := filepath.Join(td, "config.yaml")
	is.NoErr(os.WriteFile(p, []byte("sessions:\n  profiles:\n    claude:\n      providers: [a]\n      runtime_class: kata\n"), 0o600))
	cfg := DefaultConfig()
	cfg.DataPath = td
	err := parseFile(cfg, p)
	is.True(err != nil)
	for _, want := range []string{
		"line 4: sessions.profiles.<name>.providers has moved to sessions.profiles.<name>.openshell.providers",
		"line 5: sessions.profiles.<name>.runtime_class has moved to sessions.profiles.<name>.openshell.runtime_class",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from %v", want, err)
		}
	}
}
