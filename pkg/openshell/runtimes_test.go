package openshell

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/fake"
	"github.com/danielboothcloud/sundae/pkg/config"
	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
	"github.com/danielboothcloud/sundae/pkg/sandbox/docker"
	"github.com/danielboothcloud/sundae/pkg/sandbox/docker/dockertest"
	"github.com/danielboothcloud/sundae/pkg/sandbox/pod"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	utilexec "k8s.io/client-go/util/exec"
)

// unwrapPodExec undoes the pod runtime's command wrapping, so the
// harness's fake exec sees what the Manager asked for.
func unwrapPodExec(command []string) ([]string, v1.ExecOptions) {
	o := v1.ExecOptions{NoLoginShell: true}
	if len(command) > 0 && command[0] == "env" {
		o.Env = map[string]string{}
		command = command[1:]
		for len(command) > 0 {
			k, v, ok := strings.Cut(command[0], "=")
			if !ok {
				break
			}
			o.Env[k] = v
			command = command[1:]
		}
	}
	if len(command) > 4 && command[0] == "/bin/sh" && command[2] == pod.ExecWrapper {
		o.NoLoginShell = command[1] == "-c"
		o.WorkDir = command[4]
		command = command[5:]
	}
	return command, o
}

// podHarness is a harness whose sessions run in the pod runtime (a fake
// clientset) next to OpenShell (the SDK's fake).
type podHarness struct {
	*harness
	cs *kfake.Clientset
}

func newPodHarness(t *testing.T, mutate func(*config.Config)) *podHarness {
	t.Helper()
	cfg := newHarnessConfig()
	cfg.DataPath = t.TempDir()
	cfg.OpenShell.Enabled, cfg.OpenShell.GatewayAddress = true, "gw:1"
	cfg.Pod.Image = "pod-image"
	cfg.Sessions.Profiles = map[string]config.SessionProfile{
		"podman": {Runtime: config.RuntimePod, Pod: &config.PodSettings{RuntimeClass: "kata"}},
		"claude": {OpenShell: &config.ProfileOpenShell{RuntimeClass: "gvisor"}},
	}
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	fc := fake.NewClient()
	t.Cleanup(func() { _ = fc.Close() })
	ex := newFakeExec()
	osh := newSDKClient("default", fc.Sandboxes(), ex, fc.Files(), nil)

	cs := kfake.NewClientset()
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		p.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: pod.ContainerName, Ready: true}}}
		// A new pod: whatever ran in the old one is gone; /sandbox stays.
		ex.mu.Lock()
		b := ex.box(p.Name)
		b.server, b.sessions = false, map[string]bool{}
		ex.mu.Unlock()
		return false, nil, nil
	})
	pr := pod.NewWithClient(cfg, cs, "sundae")
	pr.SetStream(func(ctx context.Context, name string, o pod.StreamOptions) error {
		command, opts := unwrapPodExec(o.Command)
		res, err := ex.Run(ctx, "", name, command, opts)
		if err != nil {
			return err
		}
		_, _ = o.Stdout.Write(res.Stdout)
		_, _ = o.Stderr.Write(res.Stderr)
		if res.ExitCode != 0 {
			return utilexec.CodeExitError{Err: fmt.Errorf("exit %d", res.ExitCode), Code: res.ExitCode}
		}
		return nil
	})
	client := NewRouter(cfg.RuntimesInUse(), map[string]sbx.Runtime{config.RuntimeOpenShell: osh, config.RuntimePod: pr})
	tokens := newFakeTokens()
	return &podHarness{harness: &harness{fc: fc, exec: ex, tokens: tokens, mgr: NewManager(client, tokens, cfg)}, cs: cs}
}

func TestPodRuntimeSession(t *testing.T) {
	ctx := context.Background()
	h := newPodHarness(t, nil)
	var progress strings.Builder

	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Profile: "podman", Progress: &progress})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Created || !s.Started || s.Runtime != config.RuntimePod || len(s.Providers) != 0 {
		t.Fatalf("session = %+v", s)
	}
	p, err := h.cs.CoreV1().Pods("sundae").Get(ctx, s.Sandbox, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Spec.Containers[0].Image != "pod-image" || *p.Spec.RuntimeClassName != "kata" {
		t.Fatalf("pod spec = %+v", p.Spec)
	}
	if p.Annotations[AnnotationRuntime] != config.RuntimePod || p.Annotations[AnnotationRuntimeClass] != "kata" || p.Annotations[AnnotationProfile] != "podman" {
		t.Fatalf("pod annotations = %v", p.Annotations)
	}
	// Not on the OpenShell gateway.
	if _, err := h.fc.Sandboxes().Get(ctx, "default", s.Sandbox); err == nil {
		t.Fatal("pod session created on the OpenShell gateway too")
	}
	// The repository was cloned and the program started in the pod.
	if b := h.exec.boxes[s.Sandbox]; !b.dirs["/sandbox/r"] || len(b.sessions) != 1 {
		t.Fatalf("box = %+v", b)
	}

	// An OpenShell session next to it.
	s2, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Name: "two", Profile: "claude", Progress: &progress})
	if err != nil {
		t.Fatal(err)
	}
	if s2.Runtime != config.RuntimeOpenShell {
		t.Fatalf("openshell session runtime = %q", s2.Runtime)
	}

	infos, err := h.mgr.List(ctx, alice)
	if err != nil || len(infos) != 2 {
		t.Fatalf("list: %+v %v", infos, err)
	}
	if infos[0].Name != "main" || infos[0].Runtime != config.RuntimePod || infos[1].Runtime != config.RuntimeOpenShell {
		t.Fatalf("list = %+v", infos)
	}

	// Park: the pod goes, the volume stays.
	if parked, err := h.mgr.Park(ctx, alice, "main"); err != nil || !parked {
		t.Fatalf("park: %v %v", parked, err)
	}
	if _, err := h.cs.CoreV1().Pods("sundae").Get(ctx, s.Sandbox, metav1.GetOptions{}); err == nil {
		t.Fatal("pod still there")
	}
	infos, _ = h.mgr.List(ctx, alice)
	if PhaseWord(infos[0].Phase) != WordParked {
		t.Fatalf("parked phase = %q", infos[0].Phase)
	}

	// Attach resumes it: a new pod, the program started again.
	progress.Reset()
	s, err = h.mgr.Resume(ctx, StartOptions{User: alice, Name: "main", Progress: &progress})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Unparked || !s.Started || s.Created {
		t.Fatalf("resumed session = %+v", s)
	}
	if !strings.Contains(progress.String(), "parking stopped it") {
		t.Fatalf("progress = %q", progress.String())
	}

	// End: pod and volume go.
	if existed, err := h.mgr.End(ctx, alice, "main"); err != nil || !existed {
		t.Fatalf("end: %v %v", existed, err)
	}
	if _, err := h.cs.CoreV1().PersistentVolumeClaims("sundae").Get(ctx, s.Sandbox, metav1.GetOptions{}); err == nil {
		t.Fatal("claim still there")
	}
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Name: "main"}); !errors.Is(err, ErrNoSession) {
		t.Fatalf("resume ended: %v", err)
	}
}

// With sessions.runtime: pod, a session without a profile runs in a pod
// from pod.image.
func TestPodRuntimeDefault(t *testing.T) {
	ctx := context.Background()
	h := newPodHarness(t, func(c *config.Config) {
		c.Sessions.Runtime = config.RuntimePod
		c.OpenShell.Enabled = false
		c.Sessions.Profiles = nil
	})
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Image: "custom"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := h.cs.CoreV1().Pods("sundae").Get(ctx, s.Sandbox, metav1.GetOptions{})
	if err != nil || p.Spec.Containers[0].Image != "custom" || p.Spec.RuntimeClassName != nil {
		t.Fatalf("pod = %+v, %v", p, err)
	}
}

func TestDockerRuntimeSession(t *testing.T) {
	ctx := context.Background()
	cfg := newHarnessConfig()
	cfg.DataPath = t.TempDir()
	cfg.Sessions.Runtime = config.RuntimeDocker
	cfg.Docker.Image = "docker-image"
	cfg.Sessions.Profiles = map[string]config.SessionProfile{
		"gvisor": {Docker: &config.DockerSettings{Runtime: "runsc"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ex := newFakeExec()
	engine := dockertest.New()
	engine.OnStart = func(name string) {
		// A started container: whatever ran in it before is gone.
		ex.mu.Lock()
		b := ex.box(name)
		b.server, b.sessions = false, map[string]bool{}
		ex.mu.Unlock()
	}
	dr := docker.NewWithAPI(cfg, engine)
	dr.SetStream(func(ctx context.Context, name string, o docker.StreamOptions) (int, error) {
		command, opts := o.Command, v1.ExecOptions{NoLoginShell: true, Env: o.Env, WorkDir: o.WorkDir}
		if len(command) > 3 && command[0] == "/bin/sh" && command[2] == docker.LoginWrapper {
			command, opts.NoLoginShell = command[4:], false
		}
		res, err := ex.Run(ctx, "", name, command, opts)
		if err != nil {
			return 0, err
		}
		_, _ = o.Stdout.Write(res.Stdout)
		_, _ = o.Stderr.Write(res.Stderr)
		return res.ExitCode, nil
	})
	tokens := newFakeTokens()
	mgr := NewManager(NewRouter(cfg.RuntimesInUse(), map[string]sbx.Runtime{config.RuntimeDocker: dr}), tokens, cfg)
	var progress strings.Builder

	s, err := mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Profile: "gvisor", Progress: &progress})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Created || !s.Started || s.Runtime != config.RuntimeDocker {
		t.Fatalf("session = %+v", s)
	}
	c := engine.Containers[s.Sandbox]
	if c.Config.Image != "docker-image" || c.HostConfig.Runtime != "runsc" || c.Config.Labels[AnnotationProfile] != "gvisor" || c.Config.Labels[AnnotationRuntime] != config.RuntimeDocker {
		t.Fatalf("container = %+v", c)
	}
	if b := ex.boxes[s.Sandbox]; !b.dirs["/sandbox/r"] || len(b.sessions) != 1 {
		t.Fatalf("box = %+v", b)
	}
	infos, err := mgr.List(ctx, alice)
	if err != nil || len(infos) != 1 || infos[0].Runtime != config.RuntimeDocker || infos[0].Profile != "gvisor" {
		t.Fatalf("list: %+v %v", infos, err)
	}

	if parked, err := mgr.Park(ctx, alice, "main"); err != nil || !parked {
		t.Fatalf("park: %v %v", parked, err)
	}
	progress.Reset()
	s, err = mgr.Resume(ctx, StartOptions{User: alice, Name: "main", Progress: &progress})
	if err != nil || !s.Unparked || !s.Started {
		t.Fatalf("resume: %+v %v", s, err)
	}
	if !strings.Contains(progress.String(), "a parked docker session keeps /sandbox") {
		t.Fatalf("progress = %q", progress.String())
	}
	if existed, err := mgr.End(ctx, alice, "main"); err != nil || !existed || len(engine.Containers)+len(engine.Volumes) != 0 {
		t.Fatalf("end: %v %v", existed, err)
	}
}
