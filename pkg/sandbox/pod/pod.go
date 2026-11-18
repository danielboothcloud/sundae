// Package pod is the pod session runtime: each session's sandbox is a
// Kubernetes Pod running the sandbox image, with a PersistentVolumeClaim
// mounted at /sandbox. The claim is the sandbox: it carries the session's
// labels and annotations and the pod template, and outlives the pod.
// Parking deletes the pod; resuming creates it again from the template.
// Commands run through the pods/exec subresource.
package pod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/danielboothcloud/sundae/pkg/config"
	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// LabelRuntime marks the pods and claims of this runtime, e.g. for a
	// NetworkPolicy.
	LabelRuntime = "sundae.runtime"
	// AnnotationTemplate on the claim holds the pod template (JSON), so a
	// parked session's pod is created again as it was.
	AnnotationTemplate = "sundae.pod-template"
	// ContainerName is the sandbox container.
	ContainerName = "sandbox"
	// seedContainerName is the init container that seeds a new volume.
	seedContainerName = "seed"
	// volumeName is the /sandbox volume.
	volumeName = "sandbox"
	// SandboxRoot is the workspace and the sandbox user's home.
	SandboxRoot = "/sandbox"
	// seedMount is where the seed container mounts the volume.
	seedMount = "/seed"
	// seededMarker, in /sandbox, records that the volume was seeded.
	seededMarker = ".sundae-seeded"

	// goneTimeout bounds waits for a pod or claim to go.
	goneTimeout = 5 * time.Minute
)

// keepAlive is the sandbox container's main process: it does nothing until
// told to stop. The session's program runs in osewa, started over exec.
var keepAlive = []string{"/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done"}

// seedScript copies the image's /sandbox (the shell start-up files) into a
// new volume once, as OpenShell's Kubernetes driver does. It is best effort:
// what cannot be copied is reported in the container's log and the session
// starts anyway. Directories a failed attempt left read-only are made
// writable first, so a retry is not stuck on them; tar, as a non-root user,
// copies modes but not ownership.
const seedScript = `set -e
[ -e ` + seedMount + `/` + seededMarker + ` ] && exit 0
if [ -d ` + SandboxRoot + ` ]; then
	chmod -R u+w ` + seedMount + ` 2>/dev/null || true
	if command -v tar >/dev/null 2>&1; then
		(cd ` + SandboxRoot + ` && tar -cf - .) | (cd ` + seedMount + ` && tar -xf -) || echo "sundae: warning: the image's ` + SandboxRoot + ` was not copied completely" >&2
	else
		cp -R ` + SandboxRoot + `/. ` + seedMount + `/ || echo "sundae: warning: the image's ` + SandboxRoot + ` was not copied completely" >&2
	fi
fi
: > ` + seedMount + `/` + seededMarker

// Runtime is the pod runtime.
type Runtime struct {
	cfg       *config.Config
	namespace string
	cs        kubernetes.Interface
	rest      *rest.Config
	// stream runs a command in a pod; replaced in tests.
	stream StreamFunc
	// readyTimeout bounds a start (pod.ready_timeout).
	readyTimeout time.Duration
	// poll is how often waits look again.
	poll time.Duration
}

var _ sbx.Runtime = (*Runtime)(nil)

// New returns the pod runtime for cfg: Kubernetes credentials from
// pod.kubeconfig, else in-cluster, else the default kubeconfig.
func New(cfg *config.Config) (*Runtime, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if cfg.Pod.Kubeconfig != "" {
		rules.ExplicitPath = cfg.Pod.Kubeconfig
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	rc, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("pod runtime: kubernetes client config: %w", err)
	}
	rc.UserAgent = "sundae"
	rc.QPS, rc.Burst = 20, 40
	ns := cfg.Pod.Namespace
	if ns == "" {
		if ns, _, err = cc.Namespace(); err != nil {
			return nil, fmt.Errorf("pod runtime: namespace: %w", err)
		}
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("pod runtime: kubernetes client: %w", err)
	}
	r := NewWithClient(cfg, cs, ns)
	r.rest = rc
	r.stream = r.remoteStream
	return r, nil
}

// NewWithClient returns the pod runtime using cs in namespace. Commands
// cannot run until stream is set (New sets it).
func NewWithClient(cfg *config.Config, cs kubernetes.Interface, namespace string) *Runtime {
	r := &Runtime{
		cfg: cfg, namespace: namespace, cs: cs,
		readyTimeout: cfg.Pod.ReadyTimeout,
		poll:         time.Second,
	}
	if r.readyTimeout <= 0 {
		r.readyTimeout = config.DefaultReadyTimeout
	}
	r.stream = func(context.Context, string, StreamOptions) error {
		return errors.New("pod runtime: exec is not configured")
	}
	return r
}

// Namespace returns the namespace the runtime works in.
func (r *Runtime) Namespace() string { return r.namespace }

func (r *Runtime) pods() podClient { return r.cs.CoreV1().Pods(r.namespace) }

type podClient = interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Pod, error)
	Create(ctx context.Context, pod *corev1.Pod, opts metav1.CreateOptions) (*corev1.Pod, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.PodList, error)
}

func (r *Runtime) claims() claimClient { return r.cs.CoreV1().PersistentVolumeClaims(r.namespace) }

type claimClient = interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.PersistentVolumeClaim, error)
	Create(ctx context.Context, pvc *corev1.PersistentVolumeClaim, opts metav1.CreateOptions) (*corev1.PersistentVolumeClaim, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.PersistentVolumeClaimList, error)
}

// template is what the claim records of the pod.
type template struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Spec        corev1.PodSpec    `json:"spec"`
}

func notFound(name string) error {
	return fmt.Errorf("pod runtime: sandbox %q: %w", name, sbx.ErrNotFound)
}

func (r *Runtime) EnsureSandbox(ctx context.Context, name string, req sbx.Request) (*sbx.Sandbox, bool, error) {
	pvc, err := r.claims().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return r.create(ctx, name, req)
	case err != nil:
		return nil, false, fmt.Errorf("pod runtime: get claim %q: %w", name, err)
	}
	if !sbx.Owns(pvc.Labels, req.Labels) {
		return nil, false, sbx.ErrNotOwned
	}
	if pvc.DeletionTimestamp != nil {
		// Ended a moment ago: let it go, then start afresh.
		if err := r.waitClaimGone(ctx, name); err != nil {
			return nil, false, err
		}
		return r.create(ctx, name, req)
	}
	sb, err := r.resume(ctx, pvc)
	return sb, false, err
}

// create creates the claim and the pod and waits for the pod, deleting
// both on failure.
func (r *Runtime) create(ctx context.Context, name string, req sbx.Request) (*sbx.Sandbox, bool, error) {
	tmpl, pvc, err := r.build(name, req)
	if err != nil {
		return nil, false, err
	}
	if _, err := r.claims().Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Lost a race with a concurrent start; resume the winner's.
			cur, gerr := r.claims().Get(ctx, name, metav1.GetOptions{})
			if gerr != nil {
				return nil, false, fmt.Errorf("pod runtime: create claim %q: already exists, but get failed: %w", name, gerr)
			}
			if !sbx.Owns(cur.Labels, req.Labels) {
				return nil, false, sbx.ErrNotOwned
			}
			sb, err := r.resume(ctx, cur)
			return sb, false, err
		}
		return nil, false, fmt.Errorf("pod runtime: create claim %q: %w", name, err)
	}
	if err := r.createPod(ctx, name, tmpl); err != nil {
		r.cleanup(name)
		return nil, false, err
	}
	pod, err := r.waitReady(ctx, name)
	if err != nil {
		r.cleanup(name)
		return nil, false, err
	}
	return toSandbox(pvc, pod), true, nil
}

// build returns the pod template and the claim of a new sandbox.
func (r *Runtime) build(name string, req sbx.Request) (*template, *corev1.PersistentVolumeClaim, error) {
	s, err := r.cfg.PodSettingsFor(req.Profile).Decode()
	if err != nil {
		return nil, nil, fmt.Errorf("pod runtime: pod settings: %w", err)
	}
	if req.Image == "" {
		return nil, nil, errors.New("pod runtime: no image: set pod.image or the profile's image")
	}
	own := map[string]string{LabelRuntime: config.RuntimePod}
	maps.Copy(own, req.Labels)

	podLabels := maps.Clone(s.Labels)
	if podLabels == nil {
		podLabels = map[string]string{}
	}
	maps.Copy(podLabels, own)
	podAnnotations := maps.Clone(s.Annotations)
	if podAnnotations == nil {
		podAnnotations = map[string]string{}
	}
	maps.Copy(podAnnotations, req.Annotations)

	mounts := append([]corev1.VolumeMount{{Name: volumeName, MountPath: SandboxRoot}}, s.VolumeMounts...)
	volumes := append([]corev1.Volume{{
		Name:         volumeName,
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}},
	}}, s.Volumes...)
	env := []corev1.EnvVar{{Name: "HOME", Value: SandboxRoot}}
	spec := corev1.PodSpec{
		RestartPolicy:                 corev1.RestartPolicyAlways,
		AutomountServiceAccountToken:  ptr(false),
		EnableServiceLinks:            ptr(false),
		TerminationGracePeriodSeconds: ptr(int64(10)),
		Hostname:                      name,
		SecurityContext:               s.PodSecurityContext,
		NodeSelector:                  s.NodeSelector,
		Tolerations:                   s.Tolerations,
		Volumes:                       volumes,
		InitContainers: []corev1.Container{{
			Name:            seedContainerName,
			Image:           req.Image,
			Command:         []string{"/bin/sh", "-c", seedScript},
			SecurityContext: s.SecurityContext,
			VolumeMounts:    []corev1.VolumeMount{{Name: volumeName, MountPath: seedMount}},
		}},
		Containers: []corev1.Container{{
			Name:            ContainerName,
			Image:           req.Image,
			Command:         keepAlive,
			WorkingDir:      SandboxRoot,
			Env:             env,
			SecurityContext: s.SecurityContext,
			Resources:       s.Resources,
			VolumeMounts:    mounts,
		}},
	}
	if req.RuntimeClass != "" {
		spec.RuntimeClassName = ptr(req.RuntimeClass)
	}
	tmpl := &template{Labels: podLabels, Annotations: podAnnotations, Spec: spec}
	raw, err := json.Marshal(tmpl)
	if err != nil {
		return nil, nil, fmt.Errorf("pod runtime: encode pod template: %w", err)
	}

	claimAnnotations := maps.Clone(req.Annotations)
	if claimAnnotations == nil {
		claimAnnotations = map[string]string{}
	}
	claimAnnotations[AnnotationTemplate] = string(raw)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: own, Annotations: claimAnnotations},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: s.StorageSize},
			},
		},
	}
	if s.StorageClass != "" {
		pvc.Spec.StorageClassName = ptr(s.StorageClass)
	}
	return tmpl, pvc, nil
}

func ptr[T any](v T) *T { return &v }

// createPod creates the sandbox's pod from tmpl.
func (r *Runtime) createPod(ctx context.Context, name string, tmpl *template) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: tmpl.Labels, Annotations: tmpl.Annotations},
		Spec:       tmpl.Spec,
	}
	if _, err := r.pods().Create(ctx, pod, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("pod runtime: create pod %q: %w", name, err)
	}
	return nil
}

// claimTemplate returns the pod template a claim records.
func claimTemplate(pvc *corev1.PersistentVolumeClaim) (*template, error) {
	raw, ok := pvc.Annotations[AnnotationTemplate]
	if !ok {
		return nil, fmt.Errorf("pod runtime: claim %q has no %s annotation; end the session", pvc.Name, AnnotationTemplate)
	}
	var t template
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil, fmt.Errorf("pod runtime: claim %q: decode pod template: %w", pvc.Name, err)
	}
	return &t, nil
}

// resume makes the pod of an existing claim run and waits for it.
func (r *Runtime) resume(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (*sbx.Sandbox, error) {
	name := pvc.Name
	pod, err := r.pods().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		pod = nil
	case err != nil:
		return nil, fmt.Errorf("pod runtime: get pod %q: %w", name, err)
	}
	if pod != nil && (pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded) {
		// Being parked, or stopped (evicted): replace it.
		if pod.DeletionTimestamp == nil {
			if err := r.pods().Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("pod runtime: delete stopped pod %q: %w", name, err)
			}
		}
		if err := r.waitPodGone(ctx, name); err != nil {
			return nil, err
		}
		pod = nil
	}
	if pod == nil {
		tmpl, err := claimTemplate(pvc)
		if err != nil {
			return nil, err
		}
		if err := r.createPod(ctx, name, tmpl); err != nil {
			return nil, err
		}
	}
	pod, err = r.waitReady(ctx, name)
	if err != nil {
		return nil, err
	}
	return toSandbox(pvc, pod), nil
}

func (r *Runtime) GetSandbox(ctx context.Context, name string) (*sbx.Sandbox, error) {
	pvc, err := r.claims().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, notFound(name)
	case err != nil:
		return nil, fmt.Errorf("pod runtime: get claim %q: %w", name, err)
	}
	pod, err := r.pods().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		pod = nil
	case err != nil:
		return nil, fmt.Errorf("pod runtime: get pod %q: %w", name, err)
	}
	return toSandbox(pvc, pod), nil
}

func (r *Runtime) ListSandboxes(ctx context.Context, want map[string]string) ([]*sbx.Sandbox, error) {
	set := maps.Clone(want)
	if set == nil {
		set = map[string]string{}
	}
	set[LabelRuntime] = config.RuntimePod
	sel := labels.SelectorFromSet(set).String()
	pvcs, err := r.claims().List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("pod runtime: list claims: %w", err)
	}
	pods, err := r.pods().List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("pod runtime: list pods: %w", err)
	}
	byName := make(map[string]*corev1.Pod, len(pods.Items))
	for i := range pods.Items {
		byName[pods.Items[i].Name] = &pods.Items[i]
	}
	out := make([]*sbx.Sandbox, 0, len(pvcs.Items))
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if _, ok := pvc.Annotations[AnnotationTemplate]; !ok {
			continue
		}
		out = append(out, toSandbox(pvc, byName[pvc.Name]))
	}
	return out, nil
}

func (r *Runtime) ResumeSandbox(ctx context.Context, name string, want map[string]string) (*sbx.Sandbox, error) {
	pvc, err := r.claims().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, notFound(name)
	case err != nil:
		return nil, fmt.Errorf("pod runtime: get claim %q: %w", name, err)
	case !sbx.Owns(pvc.Labels, want):
		return nil, sbx.ErrNotOwned
	case pvc.DeletionTimestamp != nil:
		return nil, notFound(name)
	}
	return r.resume(ctx, pvc)
}

// DeleteSandbox deletes the pod and the claim, and waits until both are
// gone, so the session can be started again at once.
func (r *Runtime) DeleteSandbox(ctx context.Context, name string) error {
	if err := r.pods().Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("pod runtime: delete pod %q: %w", name, err)
	}
	if err := r.claims().Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("pod runtime: delete claim %q: %w", name, err)
	}
	if err := r.waitPodGone(ctx, name); err != nil {
		return err
	}
	return r.waitClaimGone(ctx, name)
}

// SuspendSandbox parks the sandbox: it deletes the pod, which stops the
// session's program, and keeps the claim and so /sandbox.
func (r *Runtime) SuspendSandbox(ctx context.Context, name string) (*sbx.Sandbox, error) {
	pvc, err := r.claims().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, notFound(name)
	case err != nil:
		return nil, fmt.Errorf("pod runtime: get claim %q: %w", name, err)
	}
	if err := r.pods().Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("pod runtime: delete pod %q: %w", name, err)
	}
	if err := r.waitPodGone(ctx, name); err != nil {
		return nil, err
	}
	return toSandbox(pvc, nil), nil
}

func (r *Runtime) Close() error { return nil }

// cleanup deletes a half-made sandbox with a fresh context, so it still
// runs when the caller's was cancelled.
func (r *Runtime) cleanup(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = r.pods().Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
	_ = r.claims().Delete(ctx, name, metav1.DeleteOptions{})
}

// waitReady waits until the pod runs with its sandbox container ready, or
// fails at once on a problem that waiting cannot fix.
func (r *Runtime) waitReady(ctx context.Context, name string) (*corev1.Pod, error) {
	wctx, cancel := context.WithTimeout(ctx, r.readyTimeout)
	defer cancel()
	var last *corev1.Pod
	for {
		pod, err := r.pods().Get(wctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			last = pod
			if podReady(pod) {
				return pod, nil
			}
			if msg := podFatal(pod); msg != "" {
				return nil, fmt.Errorf("pod runtime: sandbox %q cannot start: %s", name, msg)
			}
		case apierrors.IsNotFound(err):
			return nil, fmt.Errorf("pod runtime: pod %q disappeared while starting", name)
		case wctx.Err() == nil:
			return nil, fmt.Errorf("pod runtime: get pod %q: %w", name, err)
		}
		select {
		case <-wctx.Done():
			if ctx.Err() != nil {
				return nil, fmt.Errorf("pod runtime: wait for pod %q: %w", name, ctx.Err())
			}
			return nil, fmt.Errorf("pod runtime: sandbox %q not ready after %s (%s; a large image can need a longer pod.ready_timeout): %w",
				name, r.readyTimeout, podWaiting(last), sbx.ErrReadyTimeout)
		case <-time.After(r.poll):
		}
	}
}

func (r *Runtime) waitPodGone(ctx context.Context, name string) error {
	return r.waitGone(ctx, "pod", name, func(ctx context.Context) error {
		_, err := r.pods().Get(ctx, name, metav1.GetOptions{})
		return err
	})
}

func (r *Runtime) waitClaimGone(ctx context.Context, name string) error {
	return r.waitGone(ctx, "claim", name, func(ctx context.Context) error {
		_, err := r.claims().Get(ctx, name, metav1.GetOptions{})
		return err
	})
}

func (r *Runtime) waitGone(ctx context.Context, kind, name string, get func(context.Context) error) error {
	wctx, cancel := context.WithTimeout(ctx, goneTimeout)
	defer cancel()
	for {
		err := get(wctx)
		switch {
		case apierrors.IsNotFound(err):
			return nil
		case err != nil && wctx.Err() == nil:
			return fmt.Errorf("pod runtime: get %s %q: %w", kind, name, err)
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("pod runtime: %s %q still exists after %s: %w", kind, name, goneTimeout, wctx.Err())
		case <-time.After(r.poll):
		}
	}
}

// podReady reports whether the pod runs with its sandbox container ready.
func podReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == ContainerName {
			return cs.Ready
		}
	}
	return false
}

// fatalReasons are container waiting reasons a wait cannot fix.
var fatalReasons = map[string]bool{
	"InvalidImageName":           true,
	"ErrImageNeverPull":          true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
}

// crashLimit is how many restarts of a crash-looping container a start
// waits through before it gives up.
const crashLimit = 2

// podFatal returns why the pod cannot start, or "".
func podFatal(pod *corev1.Pod) string {
	if pod.Status.Phase == corev1.PodFailed {
		return fmt.Sprintf("pod failed: %s %s", pod.Status.Reason, pod.Status.Message)
	}
	for _, cs := range append(slicesClone(pod.Status.InitContainerStatuses), pod.Status.ContainerStatuses...) {
		w := cs.State.Waiting
		switch {
		case w != nil && fatalReasons[w.Reason]:
			return fmt.Sprintf("container %s: %s: %s", cs.Name, w.Reason, w.Message)
		case w != nil && w.Reason == "CrashLoopBackOff" && cs.RestartCount >= crashLimit:
			msg := fmt.Sprintf("container %s keeps failing", cs.Name)
			if t := cs.LastTerminationState.Terminated; t != nil {
				msg += fmt.Sprintf(" (exit code %d)", t.ExitCode)
			}
			return msg + fmt.Sprintf("; see kubectl -n %s logs %s -c %s", pod.Namespace, pod.Name, cs.Name)
		}
	}
	return ""
}

func slicesClone[T any](s []T) []T { return append([]T(nil), s...) }

// podWaiting says what a pod that is not ready waits for.
func podWaiting(pod *corev1.Pod) string {
	if pod == nil {
		return "no pod"
	}
	var parts []string
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status != corev1.ConditionTrue {
			parts = append(parts, fmt.Sprintf("not scheduled: %s", strings.TrimSpace(c.Reason+" "+c.Message)))
		}
	}
	for _, cs := range append(slicesClone(pod.Status.InitContainerStatuses), pod.Status.ContainerStatuses...) {
		switch {
		case cs.State.Waiting != nil:
			parts = append(parts, fmt.Sprintf("container %s waiting: %s", cs.Name, strings.TrimSpace(cs.State.Waiting.Reason+" "+cs.State.Waiting.Message)))
		case cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0:
			parts = append(parts, fmt.Sprintf("container %s exited %d: %s", cs.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Reason))
		}
	}
	if len(parts) == 0 {
		return "pod " + string(pod.Status.Phase)
	}
	return strings.Join(parts, "; ")
}

// toSandbox returns the sandbox of a claim and its pod (nil when parked).
func toSandbox(pvc *corev1.PersistentVolumeClaim, pod *corev1.Pod) *sbx.Sandbox {
	sb := &sbx.Sandbox{
		Name:        pvc.Name,
		Runtime:     config.RuntimePod,
		Labels:      maps.Clone(pvc.Labels),
		Annotations: map[string]string{},
		Phase:       phase(pvc, pod),
		CreatedAt:   pvc.CreationTimestamp.Time,
	}
	for k, v := range pvc.Annotations {
		if strings.HasPrefix(k, "sundae.") && k != AnnotationTemplate {
			sb.Annotations[k] = v
		}
	}
	if t, err := claimTemplate(pvc); err == nil {
		for _, c := range t.Spec.Containers {
			if c.Name == ContainerName {
				sb.Image = c.Image
			}
		}
		if t.Spec.RuntimeClassName != nil {
			sb.RuntimeClass = *t.Spec.RuntimeClassName
		}
	}
	return sb
}

// phase maps a claim and its pod to a sandbox phase.
func phase(pvc *corev1.PersistentVolumeClaim, pod *corev1.Pod) sbx.Phase {
	switch {
	case pvc.DeletionTimestamp != nil:
		return sbx.Deleting
	case pod == nil:
		return sbx.Suspended
	case pod.DeletionTimestamp != nil:
		return sbx.Suspending
	case podReady(pod):
		return sbx.Ready
	}
	switch pod.Status.Phase {
	case corev1.PodPending:
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionTrue {
				return sbx.Starting
			}
		}
		return sbx.Provisioning
	case corev1.PodRunning:
		return sbx.Starting
	case corev1.PodFailed, corev1.PodSucceeded:
		return sbx.Stopped
	}
	return sbx.Unknown
}
