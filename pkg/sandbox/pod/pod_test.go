package pod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielboothcloud/sundae/pkg/config"
	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	utilexec "k8s.io/client-go/util/exec"
)

const ns = "sundae"

var owner = map[string]string{sbx.LabelManaged: "true", sbx.LabelUserID: "1"}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Sessions.Runtime = config.RuntimePod
	cfg.Pod.Image = "sandbox:1"
	cfg.Pod.Labels = map[string]string{"team": "a", sbx.LabelUserID: "spoofed"}
	cfg.Pod.Annotations = map[string]string{"note": "x"}
	cfg.Pod.StorageClass = "fast"
	cfg.Sessions.Profiles = map[string]config.SessionProfile{
		"podman": {Pod: &config.PodSettings{
			RuntimeClass:    "kata",
			SecurityContext: map[string]any{"seccompProfile": map[string]any{"type": "Unconfined"}},
			Volumes:         []map[string]any{{"name": "fuse", "hostPath": map[string]any{"path": "/dev/fuse", "type": "CharDevice"}}},
			VolumeMounts:    []map[string]any{{"name": "fuse", "mountPath": "/dev/fuse"}},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// newTestRuntime returns a runtime on a fake clientset whose pods run at
// once, unless ready is false.
func newTestRuntime(t *testing.T, cfg *config.Config, ready *bool) (*Runtime, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if ready == nil || *ready {
			p.Status = corev1.PodStatus{
				Phase:             corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{Name: ContainerName, Ready: true}},
			}
		}
		return false, nil, nil
	})
	r := NewWithClient(cfg, cs, ns)
	r.poll = time.Millisecond
	return r, cs
}

func request(profile string) sbx.Request {
	return sbx.Request{
		Runtime:      config.RuntimePod,
		Profile:      profile,
		Image:        "img:2",
		RuntimeClass: "kata",
		Labels:       owner,
		Annotations:  map[string]string{"sundae.repo": "r", "sundae.session": "main"},
	}
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	r, cs := newTestRuntime(t, cfg, nil)

	sb, created, err := r.EnsureSandbox(ctx, "sd-a", request("podman"))
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if sb.Phase != sbx.Ready || sb.Runtime != config.RuntimePod || sb.Image != "img:2" || sb.RuntimeClass != "kata" {
		t.Fatalf("sandbox = %+v", sb)
	}
	if sb.Annotations["sundae.repo"] != "r" || sb.Annotations[AnnotationTemplate] != "" {
		t.Fatalf("annotations = %v", sb.Annotations)
	}

	pvc, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "sd-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *pvc.Spec.StorageClassName != "fast" || pvc.Spec.Resources.Requests.Storage().String() != config.DefaultPodStorageSize {
		t.Fatalf("claim spec = %+v", pvc.Spec)
	}
	pod, err := cs.CoreV1().Pods(ns).Get(ctx, "sd-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Sundae's labels win over pod.labels; the runtime label is set.
	if pod.Labels[sbx.LabelUserID] != "1" || pod.Labels["team"] != "a" || pod.Labels[LabelRuntime] != config.RuntimePod {
		t.Fatalf("pod labels = %v", pod.Labels)
	}
	if pod.Annotations["note"] != "x" || pod.Annotations["sundae.session"] != "main" {
		t.Fatalf("pod annotations = %v", pod.Annotations)
	}
	spec := pod.Spec
	if *spec.RuntimeClassName != "kata" || *spec.AutomountServiceAccountToken || *spec.EnableServiceLinks {
		t.Fatalf("pod spec = %+v", spec)
	}
	c := spec.Containers[0]
	if c.Image != "img:2" || c.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined || c.WorkingDir != SandboxRoot {
		t.Fatalf("container = %+v", c)
	}
	if len(c.VolumeMounts) != 2 || c.VolumeMounts[0].MountPath != SandboxRoot || c.VolumeMounts[1].MountPath != "/dev/fuse" {
		t.Fatalf("mounts = %+v", c.VolumeMounts)
	}
	if len(spec.Volumes) != 2 || spec.Volumes[0].PersistentVolumeClaim.ClaimName != "sd-a" || spec.Volumes[1].HostPath.Path != "/dev/fuse" {
		t.Fatalf("volumes = %+v", spec.Volumes)
	}
	if *spec.SecurityContext.RunAsUser != config.SandboxUID || *spec.SecurityContext.FSGroup != config.SandboxGID {
		t.Fatalf("pod security context = %+v", spec.SecurityContext)
	}
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Image != "img:2" {
		t.Fatalf("init containers = %+v", spec.InitContainers)
	}

	// Existing: resumed as is, ownership checked.
	if _, created, err := r.EnsureSandbox(ctx, "sd-a", request("podman")); err != nil || created {
		t.Fatalf("again: created=%v err=%v", created, err)
	}
	other := request("podman")
	other.Labels = map[string]string{sbx.LabelUserID: "2"}
	if _, _, err := r.EnsureSandbox(ctx, "sd-a", other); !errors.Is(err, sbx.ErrNotOwned) {
		t.Fatalf("foreign ensure: %v", err)
	}
	if _, err := r.ResumeSandbox(ctx, "sd-a", other.Labels); !errors.Is(err, sbx.ErrNotOwned) {
		t.Fatalf("foreign resume: %v", err)
	}

	// Park: the pod goes, the claim stays.
	sb, err = r.SuspendSandbox(ctx, "sd-a")
	if err != nil || sb.Phase != sbx.Suspended {
		t.Fatalf("suspend: %+v %v", sb, err)
	}
	if _, err := cs.CoreV1().Pods(ns).Get(ctx, "sd-a", metav1.GetOptions{}); err == nil {
		t.Fatal("pod still there after park")
	}
	if sb, err := r.GetSandbox(ctx, "sd-a"); err != nil || sb.Phase != sbx.Suspended || sb.Image != "img:2" {
		t.Fatalf("parked: %+v %v", sb, err)
	}
	list, err := r.ListSandboxes(ctx, map[string]string{sbx.LabelManaged: "true"})
	if err != nil || len(list) != 1 || list[0].Phase != sbx.Suspended {
		t.Fatalf("list parked: %+v %v", list, err)
	}

	// Resume: the pod is created again from the recorded template, not
	// from the current settings.
	cfg.Sessions.Profiles["podman"].Pod.RuntimeClass = "gvisor"
	sb, err = r.ResumeSandbox(ctx, "sd-a", owner)
	if err != nil || sb.Phase != sbx.Ready {
		t.Fatalf("resume: %+v %v", sb, err)
	}
	pod, _ = cs.CoreV1().Pods(ns).Get(ctx, "sd-a", metav1.GetOptions{})
	if *pod.Spec.RuntimeClassName != "kata" || len(pod.Spec.Volumes) != 2 {
		t.Fatalf("resumed pod spec = %+v", pod.Spec)
	}

	// End: both go.
	if err := r.DeleteSandbox(ctx, "sd-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetSandbox(ctx, "sd-a"); !errors.Is(err, sbx.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := r.DeleteSandbox(ctx, "sd-a"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	if _, err := r.ResumeSandbox(ctx, "sd-a", owner); !errors.Is(err, sbx.ErrNotFound) {
		t.Fatalf("resume absent: %v", err)
	}
	if _, err := r.SuspendSandbox(ctx, "sd-a"); !errors.Is(err, sbx.ErrNotFound) {
		t.Fatalf("suspend absent: %v", err)
	}
}

func TestEvictedPodIsReplaced(t *testing.T) {
	ctx := context.Background()
	r, cs := newTestRuntime(t, testConfig(t), nil)
	if _, _, err := r.EnsureSandbox(ctx, "sd-e", request("")); err != nil {
		t.Fatal(err)
	}
	pod, _ := cs.CoreV1().Pods(ns).Get(ctx, "sd-e", metav1.GetOptions{})
	pod.Status = corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}
	if _, err := cs.CoreV1().Pods(ns).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if sb, _ := r.GetSandbox(ctx, "sd-e"); sb.Phase != sbx.Stopped {
		t.Fatalf("phase = %s", sb.Phase)
	}
	if sb, err := r.ResumeSandbox(ctx, "sd-e", owner); err != nil || sb.Phase != sbx.Ready {
		t.Fatalf("resume: %+v %v", sb, err)
	}
}

func TestStartFailures(t *testing.T) {
	ctx := context.Background()
	notReady := false
	r, cs := newTestRuntime(t, testConfig(t), &notReady)
	r.readyTimeout = 50 * time.Millisecond

	// Not ready in time: ErrReadyTimeout naming the setting, and nothing
	// is left behind.
	_, _, err := r.EnsureSandbox(ctx, "sd-t", request(""))
	if !errors.Is(err, sbx.ErrReadyTimeout) || !strings.Contains(err.Error(), "pod.ready_timeout") {
		t.Fatalf("timeout: %v", err)
	}
	if _, err := r.GetSandbox(ctx, "sd-t"); !errors.Is(err, sbx.ErrNotFound) {
		t.Fatalf("left behind: %v", err)
	}

	// A problem waiting cannot fix fails at once.
	r.readyTimeout = time.Minute
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		p.Status = corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: ContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "InvalidImageName", Message: "bad"}},
		}}}
		return false, nil, nil
	})
	start := time.Now()
	_, _, err = r.EnsureSandbox(ctx, "sd-f", request(""))
	if err == nil || !strings.Contains(err.Error(), "InvalidImageName") || time.Since(start) > 10*time.Second {
		t.Fatalf("fatal: %v", err)
	}

	// No image.
	req := request("")
	req.Image = ""
	if _, _, err := r.EnsureSandbox(ctx, "sd-n", req); err == nil || !strings.Contains(err.Error(), "no image") {
		t.Fatalf("no image: %v", err)
	}
}

func TestPhase(t *testing.T) {
	now := metav1.Now()
	pvc := &corev1.PersistentVolumeClaim{}
	cases := []struct {
		pvc  *corev1.PersistentVolumeClaim
		pod  *corev1.Pod
		want sbx.Phase
	}{
		{pvc, nil, sbx.Suspended},
		{&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}}, nil, sbx.Deleting},
		{pvc, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}}, sbx.Suspending},
		{pvc, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}, sbx.Provisioning},
		{pvc, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}}}, sbx.Starting},
		{pvc, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, sbx.Starting},
		{pvc, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: ContainerName, Ready: true}}}}, sbx.Ready},
		{pvc, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}, sbx.Stopped},
	}
	for i, tc := range cases {
		if got := phase(tc.pvc, tc.pod); got != tc.want {
			t.Errorf("%d: phase = %s, want %s", i, got, tc.want)
		}
	}
}

func TestWrapCommand(t *testing.T) {
	cases := []struct {
		opts sbx.ExecOptions
		want []string
	}{
		{sbx.ExecOptions{NoLoginShell: true}, []string{"git", "status"}},
		{sbx.ExecOptions{NoLoginShell: true, Env: map[string]string{"B": "2", "A": "1\n2"}}, []string{"env", "A=1\n2", "B=2", "git", "status"}},
		{sbx.ExecOptions{NoLoginShell: true, WorkDir: "/sandbox/r"}, []string{"/bin/sh", "-c", ExecWrapper, "sundae-exec", "/sandbox/r", "git", "status"}},
		{sbx.ExecOptions{}, []string{"/bin/sh", "-lc", ExecWrapper, "sundae-exec", "", "git", "status"}},
	}
	for i, tc := range cases {
		if got := wrapCommand([]string{"git", "status"}, tc.opts); !slices.Equal(got, tc.want) {
			t.Errorf("%d: %q, want %q", i, got, tc.want)
		}
	}
}

func TestRunUploadInteractive(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRuntime(t, testConfig(t), nil)
	var got []StreamOptions
	r.stream = func(_ context.Context, pod string, o StreamOptions) error {
		got = append(got, o)
		switch o.Command[len(o.Command)-1] {
		case "fail":
			_, _ = io.WriteString(o.Stderr, "nope")
			return utilexec.CodeExitError{Err: errors.New("exit 3"), Code: 3}
		case "broken":
			return errors.New("connection reset")
		}
		if o.Stdin != nil {
			b, _ := io.ReadAll(o.Stdin)
			_, _ = o.Stdout.Write(bytes.ToUpper(b))
		} else {
			_, _ = io.WriteString(o.Stdout, "out:"+pod)
		}
		return nil
	}

	res, err := r.Run(ctx, "sd-a", []string{"echo"}, sbx.ExecOptions{NoLoginShell: true})
	if err != nil || res.ExitCode != 0 || string(res.Stdout) != "out:sd-a" {
		t.Fatalf("run: %+v %v", res, err)
	}
	res, err = r.Run(ctx, "sd-a", []string{"fail"}, sbx.ExecOptions{NoLoginShell: true})
	if err != nil || res.ExitCode != 3 || string(res.Stderr) != "nope" {
		t.Fatalf("exit code: %+v %v", res, err)
	}
	if _, err := r.Run(ctx, "sd-a", []string{"broken"}, sbx.ExecOptions{NoLoginShell: true}); err == nil {
		t.Fatal("transport error: none")
	}

	// Upload streams the file to the upload script.
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Upload(ctx, "sd-a", f, "/sandbox/x/y"); err != nil {
		t.Fatal(err)
	}
	if last := got[len(got)-1]; last.Command[2] != uploadScript || last.Command[4] != "/sandbox/x/y" {
		t.Fatalf("upload command = %q", last.Command)
	}

	// Interactive: a TTY stream with resizes; input and output piped.
	is, err := r.Interactive(ctx, "sd-a", []string{"cat"}, 80, 24, sbx.ExecOptions{NoLoginShell: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := is.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := sbx.CloseInput(is); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(is)
	if code, err := is.ExitCode(); string(out) != "HI" || code != 0 || err != nil {
		t.Fatalf("interactive: %q %d %v", out, code, err)
	}
	last := got[len(got)-1]
	if !last.TTY || last.Sizes == nil {
		t.Fatalf("interactive options = %+v", last)
	}
	if sz := last.Sizes.Next(); sz == nil || sz.Width != 80 || sz.Height != 24 {
		t.Fatalf("size = %+v", sz)
	}
	_ = is.Close()
}

func TestTemplateRoundTrip(t *testing.T) {
	r, _ := newTestRuntime(t, testConfig(t), nil)
	tmpl, pvc, err := r.build("sd-x", request("podman"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := claimTemplate(pvc)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(tmpl)
	b, _ := json.Marshal(got)
	if !bytes.Equal(a, b) {
		t.Fatalf("template changed:\n%s\n%s", a, b)
	}
}
