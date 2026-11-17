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
		"pod runtime without an image": {
			func(c *Config) { c.Sessions.Runtime = RuntimePod },
			[]string{"pod.image (or openshell.image) is required: sessions.runtime is pod"},
		},
		"pod runtime": {
			func(c *Config) {
				c.Sessions.Runtime = RuntimePod
				c.Pod.Image = "img"
				c.Sessions.Profiles["podman"] = SessionProfile{Pod: &PodSettings{RuntimeClass: "kata"}}
			}, nil,
		},
		"pod settings on another runtime": {
			func(c *Config) { c.Sessions.Profiles["claude"] = SessionProfile{Pod: &PodSettings{}} },
			[]string{"sessions.profiles.claude.pod is set, but the profile's runtime is openshell"},
		},
		"openshell profile without openshell": {
			func(c *Config) {
				c.Sessions.Runtime = RuntimePod
				c.Pod.Image = "img"
				c.Sessions.Profiles["claude"] = SessionProfile{Runtime: RuntimeOpenShell}
			},
			[]string{"sessions.profiles.claude.runtime is openshell, but openshell.enabled is false"},
		},
		"image and pod image": {
			func(c *Config) {
				c.Sessions.Profiles["podman"] = SessionProfile{Runtime: RuntimePod, Image: "a", Pod: &PodSettings{Image: "b"}}
			},
			[]string{"sessions.profiles.podman: set image or pod.image, not both"},
		},
		"runtime not implemented": {
			func(c *Config) { c.Sessions.Profiles["claude"] = SessionProfile{Runtime: RuntimeDocker} },
			[]string{`sessions.profiles.claude.runtime "docker" is not implemented yet`},
		},
		"openshell settings on another runtime": {
			func(c *Config) {
				c.Sessions.Runtime = RuntimePod
				c.Pod.Image = "img"
				c.Sessions.Profiles["claude"] = SessionProfile{OpenShell: &ProfileOpenShell{Providers: []string{"p"}}}
			},
			[]string{"sessions.profiles.claude.openshell is set, but the profile's runtime is pod", "sessions.profiles.claude.runtime: openshell"},
		},
		"openshell settings on an openshell profile": {
			func(c *Config) {
				c.Sessions.Runtime = RuntimePod
				c.Pod.Image = "img"
				c.OpenShell.Enabled, c.OpenShell.GatewayAddress, c.OpenShell.Image = true, "gw:1", "img"
				c.Sessions.Profiles["claude"] = SessionProfile{Runtime: RuntimeOpenShell, OpenShell: &ProfileOpenShell{Providers: []string{"p"}}}
			}, nil,
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

func TestPodSettingsFor(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Sessions.Runtime = RuntimePod
	cfg.OpenShell.Image = "os-img"
	cfg.Pod.RuntimeClass = "kata"
	cfg.Pod.Labels = map[string]string{"team": "a", "tier": "x"}
	cfg.Pod.Resources = map[string]any{"limits": map[string]any{"memory": "1Gi"}}
	cfg.Sessions.Profiles = map[string]SessionProfile{
		"podman": {Pod: &PodSettings{
			Image: "podman-img", RuntimeClass: RuntimeClassDefault, StorageSize: "50Gi",
			SecurityContext: map[string]any{"seccompProfile": map[string]any{"type": "Unconfined"}},
			Labels:          map[string]string{"tier": "y"},
		}},
		"plain": {},
	}
	is.NoErr(cfg.Validate())

	// Defaults filled in.
	is.Equal(cfg.Pod.SecurityContext, DefaultContainerSecurityContext())
	is.Equal(cfg.Pod.PodSecurityContext, DefaultPodSecurityContext())

	s := cfg.PodSettingsFor("podman")
	is.Equal(s.Image, "podman-img")
	is.Equal(s.StorageSize, "50Gi")
	is.Equal(s.Labels, map[string]string{"team": "a", "tier": "y"})                                     // merged, the profile's winning
	is.Equal(s.SecurityContext, map[string]any{"seccompProfile": map[string]any{"type": "Unconfined"}}) // replaced
	is.Equal(s.Resources, cfg.Pod.Resources)                                                            // inherited
	is.Equal(cfg.Pod.Labels, map[string]string{"team": "a", "tier": "x"})                               // untouched
	is.Equal(cfg.ProfileRuntimeClass("podman"), "")                                                     // "default"
	is.Equal(cfg.ProfileRuntimeClass("plain"), "kata")
	is.Equal(cfg.ProfileImage("podman"), "podman-img")
	is.Equal(cfg.ProfileImage("plain"), "os-img")

	d, err := s.Decode()
	is.NoErr(err)
	is.Equal(string(d.SecurityContext.SeccompProfile.Type), "Unconfined")
	is.Equal(d.StorageSize.String(), "50Gi")
	is.True(d.PodSecurityContext != nil && *d.PodSecurityContext.RunAsUser == SandboxUID)
}

func TestSessionsEnabled(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	is.True(!cfg.SessionsEnabled())
	cfg.OpenShell.Enabled = true
	is.True(cfg.SessionsEnabled())
	cfg.OpenShell.Enabled = false
	cfg.Sessions.Runtime = RuntimePod
	is.True(cfg.SessionsEnabled())
}

func TestPodConfigFileRoundTrip(t *testing.T) {
	is := is.New(t)
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Sessions.Runtime = RuntimePod
	cfg.Pod.Image = "img"
	cfg.Pod.Tolerations = []map[string]any{{"key": "k", "operator": "Exists"}}
	cfg.Sessions.Profiles = map[string]SessionProfile{"podman": {Pod: &PodSettings{RuntimeClass: "kata", SecurityContext: map[string]any{}}}}
	is.NoErr(cfg.Validate())
	is.NoErr(cfg.WriteConfig())

	got := &Config{DataPath: cfg.DataPath}
	is.NoErr(parseFile(got, cfg.ConfigPath()))
	is.Equal(got.Sessions.Runtime, RuntimePod)
	is.Equal(got.Pod.Image, "img")
	is.Equal(got.Pod.SecurityContext, DefaultContainerSecurityContext())
	is.Equal(got.Pod.StorageSize, DefaultPodStorageSize)
	is.Equal(len(got.Pod.Tolerations), 1)
	is.Equal(got.Sessions.Profiles["podman"].Pod.RuntimeClass, "kata")
	is.Equal(got.Sessions.Profiles["podman"].Pod.SecurityContext, map[string]any{}) // {} stays none
	is.Equal(got.Sessions.Profiles["podman"].Pod.Resources, nil)                    // null inherits
}
