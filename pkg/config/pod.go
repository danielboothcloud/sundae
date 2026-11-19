package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	kjson "sigs.k8s.io/json"
)

// Pod runtime defaults.
const (
	// DefaultPodStorageSize is the size of a pod session's /sandbox
	// volume by default.
	DefaultPodStorageSize = "10Gi"
	// SandboxPath is where a pod session's own claim is mounted.
	SandboxPath = "/sandbox"
	// SandboxVolumeName is the pod volume of that claim.
	SandboxVolumeName = "sandbox"
	// SandboxUID and SandboxGID are the sandbox user of the Sundae
	// sandbox image (deploy/sandbox/Containerfile), which the default
	// pod_security_context runs as.
	SandboxUID = 10001
	SandboxGID = 10001
)

// DefaultPodStorageAccessModes are the access modes of a pod session's
// /sandbox claim by default.
var DefaultPodStorageAccessModes = []string{string(corev1.ReadWriteOnce)}

// PodConfig configures the pod session runtime (RuntimePod): each session
// is a Kubernetes Pod, with a PersistentVolumeClaim mounted at /sandbox,
// that Sundae creates with its own Kubernetes credentials.
type PodConfig struct {
	// Namespace is where the pods and their volumes are created. Empty is
	// Sundae's own namespace (its service account's, or the kubeconfig
	// context's).
	Namespace string `env:"NAMESPACE" yaml:"namespace"`

	// Kubeconfig is the kubeconfig file to use. Empty uses the in-cluster
	// service account, else $KUBECONFIG or ~/.kube/config.
	Kubeconfig string `env:"KUBECONFIG" yaml:"kubeconfig"`

	// ReadyTimeout bounds how long a start waits for a pod to run. A
	// first start pulls the image. 0 means DefaultReadyTimeout.
	ReadyTimeout time.Duration `env:"READY_TIMEOUT" yaml:"ready_timeout"`

	// PodSettings are the settings of every session's pod, which a
	// profile's pod: section overrides.
	PodSettings `yaml:",inline"`
}

// PodSettings are the settings of a session's pod. Profiles override them
// (sessions.profiles.<name>.pod): a string or map that is set replaces the
// default, except node_selector, annotations and labels, whose keys are
// merged, and shared_volumes, merged by name; the profile's win.
//
// The Kubernetes objects (security_context, pod_security_context,
// resources, tolerations, volumes, volume_mounts) use Kubernetes field
// names, e.g. seccompProfile, and are checked against the Kubernetes API
// types: an unknown field is an error.
type PodSettings struct {
	// Image is the sandbox image. Empty uses openshell.image. A profile's
	// image, if set, wins.
	Image string `env:"IMAGE" yaml:"image"`
	// RuntimeClass is the pod's Kubernetes RuntimeClass, e.g. kata.
	// Empty, or RuntimeClassDefault, is the cluster's default runtime.
	RuntimeClass string `env:"RUNTIME_CLASS" yaml:"runtime_class"`
	// SecurityContext is the sandbox container's securityContext. Unset
	// is DefaultPodSecurityContext's container part; {} is none.
	SecurityContext map[string]any `yaml:"security_context"`
	// PodSecurityContext is the pod's securityContext. Unset is the
	// sandbox user and its fsGroup; {} is none.
	PodSecurityContext map[string]any `yaml:"pod_security_context"`
	// Resources are the sandbox container's requests and limits.
	Resources map[string]any `yaml:"resources"`
	// StorageClass is the StorageClass of /sandbox's volume. Empty is the
	// cluster's default.
	StorageClass string `env:"STORAGE_CLASS" yaml:"storage_class"`
	// StorageSize is the size of /sandbox's volume. Empty is
	// DefaultPodStorageSize.
	StorageSize string `env:"STORAGE_SIZE" yaml:"storage_size"`
	// StorageAccessModes are the access modes of /sandbox's claim. Empty
	// is DefaultPodStorageAccessModes.
	StorageAccessModes []string `env:"STORAGE_ACCESS_MODES" envSeparator:"," yaml:"storage_access_modes"`
	// NodeSelector, Tolerations, Annotations and Labels are set on the
	// pod. Labels and annotations do not replace Sundae's own.
	NodeSelector map[string]string `yaml:"node_selector"`
	Tolerations  []map[string]any  `yaml:"tolerations"`
	Annotations  map[string]string `yaml:"annotations"`
	Labels       map[string]string `yaml:"labels"`
	// Volumes are extra pod volumes, and VolumeMounts their mounts in the
	// sandbox container.
	Volumes      []map[string]any `yaml:"volumes"`
	VolumeMounts []map[string]any `yaml:"volume_mounts"`
	// SharedVolumes are existing claims mounted into every session's pod,
	// e.g. a ReadWriteMany cache. A profile's are merged with these by
	// name.
	SharedVolumes []SharedVolume `yaml:"shared_volumes"`
}

// SharedVolume is an existing PersistentVolumeClaim mounted into every
// session's sandbox container.
type SharedVolume struct {
	// Name is the pod volume's name, and the key profiles merge by.
	Name string `json:"name" yaml:"name"`
	// ClaimName is the PersistentVolumeClaim, in pod.namespace.
	ClaimName string `json:"claim_name" yaml:"claim_name"`
	// MountPath is where it is mounted: absolute, outside /sandbox.
	MountPath string `json:"mount_path" yaml:"mount_path"`
	// ReadOnly mounts it read-only.
	ReadOnly bool `json:"read_only,omitempty" yaml:"read_only"`
	// SubPath mounts a directory of the volume instead of its root.
	SubPath string `json:"sub_path,omitempty" yaml:"sub_path"`
}

// DefaultContainerSecurityContext is pod.security_context when unset: no
// privilege escalation, no capabilities, the runtime's default seccomp
// profile.
func DefaultContainerSecurityContext() map[string]any {
	return map[string]any{
		"allowPrivilegeEscalation": false,
		"capabilities":             map[string]any{"drop": []any{"ALL"}},
		"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
	}
}

// DefaultPodSecurityContext is pod.pod_security_context when unset: the
// Sundae sandbox image's user, which owns /sandbox through fsGroup.
func DefaultPodSecurityContext() map[string]any {
	return map[string]any{
		"runAsUser":           SandboxUID,
		"runAsGroup":          SandboxGID,
		"runAsNonRoot":        true,
		"fsGroup":             SandboxGID,
		"fsGroupChangePolicy": "OnRootMismatch",
	}
}

// PodSpecSettings are PodSettings with the Kubernetes objects decoded.
type PodSpecSettings struct {
	Image, RuntimeClass, StorageClass string
	StorageSize                       resource.Quantity
	SecurityContext                   *corev1.SecurityContext
	PodSecurityContext                *corev1.PodSecurityContext
	Resources                         corev1.ResourceRequirements
	NodeSelector                      map[string]string
	Tolerations                       []corev1.Toleration
	Annotations, Labels               map[string]string
	StorageAccessModes                []corev1.PersistentVolumeAccessMode
	Volumes                           []corev1.Volume
	VolumeMounts                      []corev1.VolumeMount
}

// Decode decodes the settings' Kubernetes objects. Validate has checked
// that they decode.
func (s PodSettings) Decode() (PodSpecSettings, error) {
	out := PodSpecSettings{
		Image:        s.Image,
		RuntimeClass: ResolveRuntimeClass(s.RuntimeClass),
		StorageClass: s.StorageClass,
		NodeSelector: maps.Clone(s.NodeSelector),
		Annotations:  maps.Clone(s.Annotations),
		Labels:       maps.Clone(s.Labels),
	}
	var errs []error
	size := s.StorageSize
	if size == "" {
		size = DefaultPodStorageSize
	}
	q, err := resource.ParseQuantity(size)
	if err != nil {
		errs = append(errs, fmt.Errorf("storage_size %q: %w", size, err))
	}
	out.StorageSize = q
	modes := s.StorageAccessModes
	if len(modes) == 0 {
		modes = DefaultPodStorageAccessModes
	}
	for _, m := range modes {
		out.StorageAccessModes = append(out.StorageAccessModes, corev1.PersistentVolumeAccessMode(m))
	}
	if s.SecurityContext != nil {
		out.SecurityContext = &corev1.SecurityContext{}
		errs = append(errs, decodeK8s("security_context", s.SecurityContext, out.SecurityContext))
	}
	if s.PodSecurityContext != nil {
		out.PodSecurityContext = &corev1.PodSecurityContext{}
		errs = append(errs, decodeK8s("pod_security_context", s.PodSecurityContext, out.PodSecurityContext))
	}
	if s.Resources != nil {
		errs = append(errs, decodeK8s("resources", s.Resources, &out.Resources))
	}
	if s.Tolerations != nil {
		errs = append(errs, decodeK8s("tolerations", s.Tolerations, &out.Tolerations))
	}
	if s.Volumes != nil {
		errs = append(errs, decodeK8s("volumes", s.Volumes, &out.Volumes))
	}
	if s.VolumeMounts != nil {
		errs = append(errs, decodeK8s("volume_mounts", s.VolumeMounts, &out.VolumeMounts))
	}
	for _, v := range s.SharedVolumes {
		out.Volumes = append(out.Volumes, corev1.Volume{
			Name: v.Name,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: v.ClaimName, ReadOnly: v.ReadOnly,
			}},
		})
		out.VolumeMounts = append(out.VolumeMounts, corev1.VolumeMount{
			Name: v.Name, MountPath: v.MountPath, ReadOnly: v.ReadOnly, SubPath: v.SubPath,
		})
	}
	return out, errors.Join(errs...)
}

// decodeK8s decodes v, a value read from YAML, into the Kubernetes API
// object out, rejecting unknown fields.
func decodeK8s(key string, v, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	// As the API server decodes: field names are case-sensitive.
	strict, err := kjson.UnmarshalStrict(b, out, kjson.DisallowUnknownFields)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	var errs []error
	for _, e := range strict {
		errs = append(errs, fmt.Errorf("%s: %w", key, e))
	}
	return errors.Join(errs...)
}

// PodSettingsFor returns the pod settings of the named profile's
// sessions: pod: with the profile's pod: section applied.
func (c *Config) PodSettingsFor(profile string) PodSettings {
	s := c.Pod.PodSettings
	s.NodeSelector = maps.Clone(s.NodeSelector)
	s.Annotations = maps.Clone(s.Annotations)
	s.Labels = maps.Clone(s.Labels)
	s.StorageAccessModes = slices.Clone(s.StorageAccessModes)
	s.SharedVolumes = slices.Clone(s.SharedVolumes)
	p := c.Sessions.Profiles[profile].Pod
	if p == nil {
		return s
	}
	for _, f := range []struct{ dst, src *string }{
		{&s.Image, &p.Image},
		{&s.RuntimeClass, &p.RuntimeClass},
		{&s.StorageClass, &p.StorageClass},
		{&s.StorageSize, &p.StorageSize},
	} {
		if *f.src != "" {
			*f.dst = *f.src
		}
	}
	if p.SecurityContext != nil {
		s.SecurityContext = p.SecurityContext
	}
	if p.PodSecurityContext != nil {
		s.PodSecurityContext = p.PodSecurityContext
	}
	if p.Resources != nil {
		s.Resources = p.Resources
	}
	if p.Tolerations != nil {
		s.Tolerations = p.Tolerations
	}
	if p.Volumes != nil {
		s.Volumes = p.Volumes
	}
	if p.VolumeMounts != nil {
		s.VolumeMounts = p.VolumeMounts
	}
	if len(p.StorageAccessModes) > 0 {
		s.StorageAccessModes = slices.Clone(p.StorageAccessModes)
	}
	s.SharedVolumes = mergeSharedVolumes(s.SharedVolumes, p.SharedVolumes)
	s.NodeSelector = mergeStrings(s.NodeSelector, p.NodeSelector)
	s.Annotations = mergeStrings(s.Annotations, p.Annotations)
	s.Labels = mergeStrings(s.Labels, p.Labels)
	return s
}

// mergeSharedVolumes returns base with over's volumes applied by name:
// one with a name in base replaces it in place, the others are appended.
func mergeSharedVolumes(base, over []SharedVolume) []SharedVolume {
	for _, v := range over {
		if i := slices.IndexFunc(base, func(b SharedVolume) bool { return b.Name == v.Name }); i >= 0 {
			base[i] = v
		} else {
			base = append(base, v)
		}
	}
	return base
}

func mergeStrings(base, over map[string]string) map[string]string {
	if len(over) == 0 {
		return base
	}
	if base == nil {
		base = map[string]string{}
	}
	maps.Copy(base, over)
	return base
}

// ProfileImage returns the image of a new sandbox of the named profile:
// the profile's image, else its runtime's (pod or docker) image, else
// openshell.image.
func (c *Config) ProfileImage(profile string) string {
	if img := c.Sessions.Profiles[profile].Image; img != "" {
		return img
	}
	switch c.ProfileRuntime(profile) {
	case RuntimePod:
		if img := c.PodSettingsFor(profile).Image; img != "" {
			return img
		}
	case RuntimeDocker:
		if img := c.DockerSettingsFor(profile).Image; img != "" {
			return img
		}
	}
	return c.OpenShell.Image
}

// validatePod validates pod: and the profiles' pod: sections, and fills
// in defaults. The settings are checked whether or not a profile uses the
// pod runtime; an image is required only where one does.
func (c *Config) validatePod() error {
	pc := &c.Pod
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	pc.Namespace = strings.TrimSpace(pc.Namespace)
	if pc.Namespace != "" {
		for _, msg := range validation.IsDNS1123Label(pc.Namespace) {
			fail("pod.namespace %q: %s", pc.Namespace, msg)
		}
	}
	pc.Kubeconfig = strings.TrimSpace(pc.Kubeconfig)
	if pc.Kubeconfig != "" && !filepath.IsAbs(pc.Kubeconfig) {
		pc.Kubeconfig = filepath.Join(c.DataPath, pc.Kubeconfig)
	}
	switch {
	case pc.ReadyTimeout == 0:
		pc.ReadyTimeout = DefaultReadyTimeout
	case pc.ReadyTimeout < MinReadyTimeout:
		fail("pod.ready_timeout %s is too short: use at least %s", pc.ReadyTimeout, MinReadyTimeout)
	}
	if pc.SecurityContext == nil {
		pc.SecurityContext = DefaultContainerSecurityContext()
	}
	if pc.PodSecurityContext == nil {
		pc.PodSecurityContext = DefaultPodSecurityContext()
	}
	if strings.TrimSpace(pc.StorageSize) == "" {
		pc.StorageSize = DefaultPodStorageSize
	}
	if len(pc.StorageAccessModes) == 0 {
		pc.StorageAccessModes = slices.Clone(DefaultPodStorageAccessModes)
	}
	errs = append(errs, validatePodSettings(&pc.PodSettings, "pod"))
	errs = append(errs, validatePodMounts(c.PodSettingsFor(""), "pod"))

	for _, name := range sortedKeys(c.Sessions.Profiles) {
		p := c.Sessions.Profiles[name]
		key := "sessions.profiles." + name
		if p.Pod != nil {
			errs = append(errs, validatePodSettings(p.Pod, key+".pod"))
			c.Sessions.Profiles[name] = p
			errs = append(errs, validatePodMounts(c.PodSettingsFor(name), key+".pod"))
			if p.Image != "" && p.Pod.Image != "" {
				fail("%s: set image or pod.image, not both", key)
			}
		}
		c.Sessions.Profiles[name] = p
	}

	if c.SessionsEnabled() && slices.Contains(c.RuntimesInUse(), RuntimePod) {
		for _, name := range append([]string{""}, sortedKeys(c.Sessions.Profiles)...) {
			if c.ProfileRuntime(name) == RuntimePod && c.ProfileImage(name) == "" {
				if name == "" {
					fail("pod.image (or openshell.image) is required: sessions.runtime is %s", RuntimePod)
				} else {
					fail("sessions.profiles.%s runs in the %s runtime and has no image: set its image, pod.image or openshell.image", name, RuntimePod)
				}
				break
			}
		}
	}
	return errors.Join(errs...)
}

// validatePodSettings checks s, reporting problems under key, and trims
// its strings.
func validatePodSettings(s *PodSettings, key string) error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(key+"."+format, args...))
	}
	s.Image = strings.TrimSpace(s.Image)
	s.RuntimeClass = strings.TrimSpace(s.RuntimeClass)
	s.StorageClass = strings.TrimSpace(s.StorageClass)
	s.StorageSize = strings.TrimSpace(s.StorageSize)
	if !validRuntimeClass(s.RuntimeClass) {
		fail("runtime_class %q %s", s.RuntimeClass, runtimeClassProblem)
	}
	if s.StorageClass != "" {
		for _, msg := range validation.IsDNS1123Subdomain(s.StorageClass) {
			fail("storage_class %q: %s", s.StorageClass, msg)
		}
	}
	if s.StorageSize != "" {
		if q, err := resource.ParseQuantity(s.StorageSize); err != nil || q.Sign() <= 0 {
			fail("storage_size %q is not a positive Kubernetes quantity, e.g. 10Gi", s.StorageSize)
		}
	}
	if s.StorageAccessModes != nil {
		modes, err := validateAccessModes(s.StorageAccessModes, key+".storage_access_modes")
		s.StorageAccessModes = modes
		errs = append(errs, err)
	}
	for i := range s.SharedVolumes {
		errs = append(errs, validateSharedVolume(&s.SharedVolumes[i], fmt.Sprintf("%s.shared_volumes[%d]", key, i)))
	}
	objects := *s
	objects.StorageSize = "" // checked above
	objects.StorageAccessModes = nil
	objects.SharedVolumes = nil
	if _, err := objects.Decode(); err != nil {
		for _, e := range unjoin(err) {
			fail("%v", e)
		}
	}
	for _, k := range sortedKeys(s.Labels) {
		for _, msg := range validation.IsQualifiedName(k) {
			fail("labels: key %q: %s", k, msg)
		}
		for _, msg := range validation.IsValidLabelValue(s.Labels[k]) {
			fail("labels.%s: value %q: %s", k, s.Labels[k], msg)
		}
	}
	for _, k := range sortedKeys(s.Annotations) {
		for _, msg := range validation.IsQualifiedName(k) {
			fail("annotations: key %q: %s", k, msg)
		}
	}
	for _, k := range sortedKeys(s.NodeSelector) {
		for _, msg := range validation.IsQualifiedName(k) {
			fail("node_selector: key %q: %s", k, msg)
		}
	}
	return errors.Join(errs...)
}

// writableAccessModes are the access modes a /sandbox claim may have:
// the sandbox writes to it.
var writableAccessModes = []string{
	string(corev1.ReadWriteOnce), string(corev1.ReadWriteMany), string(corev1.ReadWriteOncePod),
}

// validateAccessModes checks a /sandbox claim's access modes and returns
// them trimmed, without duplicates. Empty is the default.
func validateAccessModes(modes []string, key string) ([]string, error) {
	var errs []error
	var seen []string
	for _, m := range modes {
		m = strings.TrimSpace(m)
		switch {
		case !slices.Contains(writableAccessModes, m):
			errs = append(errs, fmt.Errorf("%s: %q must be one of %s (/sandbox must be writable)", key, m, strings.Join(writableAccessModes, ", ")))
		case !slices.Contains(seen, m):
			seen = append(seen, m)
		}
	}
	// NOTE: the API server rejects ReadWriteOncePod combined with any other mode.
	if slices.Contains(seen, string(corev1.ReadWriteOncePod)) && len(seen) > 1 {
		errs = append(errs, fmt.Errorf("%s: %s cannot be combined with another mode", key, corev1.ReadWriteOncePod))
	}
	return seen, errors.Join(errs...)
}

// validateSharedVolume checks one shared_volumes entry and cleans it up.
func validateSharedVolume(v *SharedVolume, key string) error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(key+"."+format, args...)) }
	v.Name = strings.TrimSpace(v.Name)
	v.ClaimName = strings.TrimSpace(v.ClaimName)
	v.MountPath = strings.TrimSpace(v.MountPath)
	v.SubPath = strings.TrimSpace(v.SubPath)
	switch v.Name {
	case "":
		fail("name is required")
	case SandboxVolumeName:
		fail("name %q is the /sandbox volume's", v.Name)
	default:
		for _, msg := range validation.IsDNS1123Label(v.Name) {
			fail("name %q: %s", v.Name, msg)
		}
	}
	if v.ClaimName == "" {
		fail("claim_name is required")
	} else {
		for _, msg := range validation.IsDNS1123Subdomain(v.ClaimName) {
			fail("claim_name %q: %s", v.ClaimName, msg)
		}
	}
	switch mp := filepath.Clean(v.MountPath); {
	case v.MountPath == "":
		fail("mount_path is required")
	case !filepath.IsAbs(v.MountPath):
		fail("mount_path %q must be absolute", v.MountPath)
	case mp == SandboxPath || strings.HasPrefix(mp+"/", SandboxPath+"/") || strings.HasPrefix(SandboxPath, strings.TrimSuffix(mp, "/")+"/"):
		fail("mount_path %q clashes with %s, the session's own volume: mount it outside %s", v.MountPath, SandboxPath, SandboxPath)
	default:
		v.MountPath = mp
	}
	if v.SubPath != "" && (filepath.IsAbs(v.SubPath) || slices.Contains(strings.Split(v.SubPath, "/"), "..")) {
		fail("sub_path %q must be a relative path without ..", v.SubPath)
	}
	return errors.Join(errs...)
}

// validatePodMounts checks that the effective settings' shared volumes
// clash neither with each other nor with volumes and volume_mounts.
func validatePodMounts(s PodSettings, key string) error {
	var errs []error
	names := map[string]string{SandboxVolumeName: "the /sandbox volume"}
	paths := map[string]string{SandboxPath: "the /sandbox volume"}
	for i, v := range s.Volumes {
		if n, ok := v["name"].(string); ok {
			names[n] = fmt.Sprintf("volumes[%d]", i)
		}
	}
	for i, m := range s.VolumeMounts {
		if p, ok := m["mountPath"].(string); ok {
			paths[filepath.Clean(p)] = fmt.Sprintf("volume_mounts[%d]", i)
		}
	}
	for i, v := range s.SharedVolumes {
		at := fmt.Sprintf("%s.shared_volumes[%d]", key, i)
		if v.Name != "" {
			if other, ok := names[v.Name]; ok {
				errs = append(errs, fmt.Errorf("%s.name %q is also the name of %s", at, v.Name, other))
			}
			names[v.Name] = fmt.Sprintf("shared_volumes[%d]", i)
		}
		if v.MountPath != "" {
			if other, ok := paths[v.MountPath]; ok {
				errs = append(errs, fmt.Errorf("%s.mount_path %q is also the mount path of %s", at, v.MountPath, other))
			}
			paths[v.MountPath] = fmt.Sprintf("shared_volumes[%d]", i)
		}
	}
	return errors.Join(errs...)
}

// unjoin returns the errors joined in err.
func unjoin(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok { //nolint:errorlint
		var out []error
		for _, e := range j.Unwrap() {
			if e != nil {
				out = append(out, e)
			}
		}
		return out
	}
	return []error{err}
}

// podEnviron returns pod:'s scalar settings as SUNDAE_POD_* environment
// variables. The maps and lists have no environment form.
func (c *Config) podEnviron() []string {
	return []string{
		"SUNDAE_POD_NAMESPACE=" + c.Pod.Namespace,
		"SUNDAE_POD_KUBECONFIG=" + c.Pod.Kubeconfig,
		"SUNDAE_POD_READY_TIMEOUT=" + c.Pod.ReadyTimeout.String(),
		"SUNDAE_POD_IMAGE=" + c.Pod.Image,
		"SUNDAE_POD_RUNTIME_CLASS=" + c.Pod.RuntimeClass,
		"SUNDAE_POD_STORAGE_CLASS=" + c.Pod.StorageClass,
		"SUNDAE_POD_STORAGE_SIZE=" + c.Pod.StorageSize,
		"SUNDAE_POD_STORAGE_ACCESS_MODES=" + strings.Join(c.Pod.StorageAccessModes, ","),
	}
}
