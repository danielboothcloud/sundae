// Package docker is the docker session runtime: each session's sandbox is
// a container on a Docker-compatible engine, with a named volume mounted
// at /sandbox. The container carries the session's labels and
// annotations, as labels. Suspending stops the container; resuming starts it
// again. Commands run through the engine's exec API.
package docker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/danielboothcloud/sundae/pkg/config"
	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

const (
	// LabelRuntime marks this runtime's containers and volumes.
	LabelRuntime = "sundae.runtime"
	// SandboxRoot is the workspace and the sandbox user's home.
	SandboxRoot = "/sandbox"
	// stopTimeout is how long a suspend lets the container stop.
	stopTimeout = 10
)

// keepAlive is the container's main process: it does nothing until told
// to stop. The session's program runs in osewa, started over exec.
var keepAlive = []string{"/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done"}

// API is the part of the engine's client the runtime uses.
type API interface {
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, id string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerStop(ctx context.Context, id string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(ctx context.Context, id string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerInspect(ctx context.Context, id string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	VolumeCreate(ctx context.Context, options client.VolumeCreateOptions) (client.VolumeCreateResult, error)
	VolumeRemove(ctx context.Context, id string, options client.VolumeRemoveOptions) (client.VolumeRemoveResult, error)
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	ExecCreate(ctx context.Context, id string, options client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(ctx context.Context, id string, options client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecInspect(ctx context.Context, id string, options client.ExecInspectOptions) (client.ExecInspectResult, error)
	ExecResize(ctx context.Context, id string, options client.ExecResizeOptions) (client.ExecResizeResult, error)
	Close() error
}

// Runtime is the docker runtime.
type Runtime struct {
	cfg *config.Config
	api API
	// stream runs a command in a container; replaced in tests.
	stream StreamFunc
	// readyTimeout bounds a start (docker.ready_timeout).
	readyTimeout time.Duration
	// poll is how often waits look again.
	poll time.Duration
}

var _ sbx.Runtime = (*Runtime)(nil)

// New returns the docker runtime for cfg, connecting to docker.host, else
// $DOCKER_HOST, else the default socket.
func New(cfg *config.Config) (*Runtime, error) {
	opts := []client.Opt{client.FromEnv}
	if cfg.Docker.Host != "" {
		opts = append(opts, client.WithHost(cfg.Docker.Host))
	}
	if cfg.Docker.TLSCA != "" || cfg.Docker.TLSCert != "" {
		opts = append(opts, client.WithTLSClientConfig(cfg.Docker.TLSCA, cfg.Docker.TLSCert, cfg.Docker.TLSKey))
	}
	c, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker runtime: client: %w", err)
	}
	return NewWithAPI(cfg, c), nil
}

// NewWithAPI returns the docker runtime using api.
func NewWithAPI(cfg *config.Config, api API) *Runtime {
	r := &Runtime{cfg: cfg, api: api, readyTimeout: cfg.Docker.ReadyTimeout, poll: 200 * time.Millisecond}
	if r.readyTimeout <= 0 {
		r.readyTimeout = config.DefaultReadyTimeout
	}
	r.stream = r.apiStream
	return r
}

func notFound(name string) error {
	return fmt.Errorf("docker runtime: sandbox %q: %w", name, sbx.ErrNotFound)
}

// inspect returns the named container; nil when there is none.
func (r *Runtime) inspect(ctx context.Context, name string) (*container.InspectResponse, error) {
	res, err := r.api.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	switch {
	case cerrdefs.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("docker runtime: inspect container %q: %w", name, err)
	}
	c := res.Container
	if c.Config == nil || c.Config.Labels[LabelRuntime] != config.RuntimeDocker {
		// Not one of Sundae's.
		return nil, nil
	}
	return &c, nil
}

func (r *Runtime) EnsureSandbox(ctx context.Context, name string, req sbx.Request) (*sbx.Sandbox, bool, error) {
	c, err := r.inspect(ctx, name)
	if err != nil {
		return nil, false, err
	}
	if c == nil {
		return r.create(ctx, name, req)
	}
	if !sbx.Owns(c.Config.Labels, req.Labels) {
		return nil, false, sbx.ErrNotOwned
	}
	sb, err := r.resume(ctx, c)
	return sb, false, err
}

// create creates the volume and the container, starts it and waits for
// it, removing both on failure.
func (r *Runtime) create(ctx context.Context, name string, req sbx.Request) (*sbx.Sandbox, bool, error) {
	if req.Image == "" {
		return nil, false, errors.New("docker runtime: no image: set docker.image or the profile's image")
	}
	wctx, cancel := context.WithTimeout(ctx, r.readyTimeout)
	defer cancel()
	s := r.cfg.DockerSettingsFor(req.Profile)

	own := map[string]string{LabelRuntime: config.RuntimeDocker}
	maps.Copy(own, req.Labels)
	labels := maps.Clone(s.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range req.Annotations {
		if strings.HasPrefix(k, "sundae.") {
			labels[k] = v
		}
	}
	maps.Copy(labels, own)

	if _, err := r.api.VolumeCreate(wctx, client.VolumeCreateOptions{
		Name: name, Driver: s.VolumeDriver, DriverOpts: s.VolumeOptions, Labels: own,
	}); err != nil {
		return nil, false, fmt.Errorf("docker runtime: create volume %q: %w", name, err)
	}

	opts := client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:      req.Image,
			Hostname:   name,
			User:       s.User,
			Entrypoint: keepAlive,
			WorkingDir: SandboxRoot,
			Env:        []string{"HOME=" + SandboxRoot},
			Labels:     labels,
		},
		HostConfig: &container.HostConfig{
			Mounts:        []mount.Mount{{Type: mount.TypeVolume, Source: name, Target: SandboxRoot}},
			NetworkMode:   container.NetworkMode(s.Network),
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			Runtime:       s.Runtime,
			CapAdd:        s.CapAdd,
			CapDrop:       s.CapDrop,
			SecurityOpt:   s.SecurityOpt,
			Resources: container.Resources{
				Memory:   s.MemoryBytes(),
				NanoCPUs: s.NanoCPUs(),
				Devices:  devices(s.Devices),
			},
		},
	}
	_, err := r.api.ContainerCreate(wctx, opts)
	if cerrdefs.IsNotFound(err) {
		// The image is not on the engine: pull it, then try again.
		if err = r.pull(wctx, req.Image); err == nil {
			_, err = r.api.ContainerCreate(wctx, opts)
		}
	}
	if cerrdefs.IsConflict(err) {
		// Lost a race with a concurrent start; resume the winner's.
		c, ierr := r.inspect(ctx, name)
		if ierr != nil || c == nil {
			return nil, false, fmt.Errorf("docker runtime: create container %q: %w", name, err)
		}
		if !sbx.Owns(c.Config.Labels, req.Labels) {
			return nil, false, sbx.ErrNotOwned
		}
		sb, err := r.resume(ctx, c)
		return sb, false, err
	}
	if err != nil {
		r.cleanup(name)
		return nil, false, fmt.Errorf("docker runtime: create container %q: %w", name, err)
	}
	sb, err := r.start(ctx, name)
	if err != nil {
		r.cleanup(name)
		return nil, false, err
	}
	return sb, true, nil
}

// devices parses host-path[:container-path[:permissions]] entries.
func devices(specs []string) []container.DeviceMapping {
	var out []container.DeviceMapping
	for _, d := range specs {
		parts := strings.SplitN(d, ":", 3)
		m := container.DeviceMapping{PathOnHost: parts[0], PathInContainer: parts[0], CgroupPermissions: "rwm"}
		if len(parts) > 1 && parts[1] != "" {
			m.PathInContainer = parts[1]
		}
		if len(parts) > 2 && parts[2] != "" {
			m.CgroupPermissions = parts[2]
		}
		out = append(out, m)
	}
	return out
}

// pull pulls image.
func (r *Runtime) pull(ctx context.Context, image string) error {
	resp, err := r.api.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("docker runtime: pull %s: %w", image, err)
	}
	defer resp.Close() //nolint:errcheck
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("docker runtime: pull %s: %w", image, err)
	}
	return nil
}

// resume starts a stopped container and waits for it.
func (r *Runtime) resume(ctx context.Context, c *container.InspectResponse) (*sbx.Sandbox, error) {
	name := strings.TrimPrefix(c.Name, "/")
	if c.State != nil && c.State.Running && !c.State.Paused {
		return toSandbox(c), nil
	}
	return r.start(ctx, name)
}

// start starts the container and waits until it runs.
func (r *Runtime) start(ctx context.Context, name string) (*sbx.Sandbox, error) {
	wctx, cancel := context.WithTimeout(ctx, r.readyTimeout)
	defer cancel()
	if _, err := r.api.ContainerStart(wctx, name, client.ContainerStartOptions{}); err != nil {
		return nil, fmt.Errorf("docker runtime: start container %q: %w", name, err)
	}
	for {
		c, err := r.inspect(wctx, name)
		switch {
		case err != nil && wctx.Err() == nil:
			return nil, err
		case err == nil && c == nil:
			return nil, fmt.Errorf("docker runtime: container %q disappeared while starting", name)
		case err == nil && c.State != nil:
			if c.State.Running && !c.State.Restarting {
				return toSandbox(c), nil
			}
			if c.State.Status == container.StateExited || c.State.Status == container.StateDead {
				return nil, fmt.Errorf("docker runtime: container %q exited at start (code %d): %s", name, c.State.ExitCode, c.State.Error)
			}
		}
		select {
		case <-wctx.Done():
			if ctx.Err() != nil {
				return nil, fmt.Errorf("docker runtime: wait for container %q: %w", name, ctx.Err())
			}
			return nil, fmt.Errorf("docker runtime: sandbox %q not running after %s (a large image can need a longer docker.ready_timeout): %w", name, r.readyTimeout, sbx.ErrReadyTimeout)
		case <-time.After(r.poll):
		}
	}
}

func (r *Runtime) GetSandbox(ctx context.Context, name string) (*sbx.Sandbox, error) {
	c, err := r.inspect(ctx, name)
	switch {
	case err != nil:
		return nil, err
	case c == nil:
		return nil, notFound(name)
	}
	return toSandbox(c), nil
}

func (r *Runtime) ListSandboxes(ctx context.Context, want map[string]string) ([]*sbx.Sandbox, error) {
	f := client.Filters{}.Add("label", LabelRuntime+"="+config.RuntimeDocker)
	for k, v := range want {
		f.Add("label", k+"="+v)
	}
	res, err := r.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("docker runtime: list containers: %w", err)
	}
	out := make([]*sbx.Sandbox, 0, len(res.Items))
	for _, s := range res.Items {
		if s.Labels[LabelRuntime] != config.RuntimeDocker || !hasLabels(s.Labels, want) || len(s.Names) == 0 {
			continue
		}
		out = append(out, &sbx.Sandbox{
			Name:        strings.TrimPrefix(s.Names[0], "/"),
			Runtime:     config.RuntimeDocker,
			Labels:      ownLabels(s.Labels),
			Annotations: annotations(s.Labels),
			Phase:       phase(s.State, false),
			Image:       s.Image,
			CreatedAt:   time.Unix(s.Created, 0),
		})
	}
	return out, nil
}

func hasLabels(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func (r *Runtime) ResumeSandbox(ctx context.Context, name string, want map[string]string) (*sbx.Sandbox, error) {
	c, err := r.inspect(ctx, name)
	switch {
	case err != nil:
		return nil, err
	case c == nil:
		return nil, notFound(name)
	case !sbx.Owns(c.Config.Labels, want):
		return nil, sbx.ErrNotOwned
	}
	return r.resume(ctx, c)
}

// DeleteSandbox removes the container and its volume.
func (r *Runtime) DeleteSandbox(ctx context.Context, name string) error {
	c, err := r.inspect(ctx, name)
	if err != nil {
		return err
	}
	if c != nil {
		if _, err := r.api.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("docker runtime: remove container %q: %w", name, err)
		}
	}
	if _, err := r.api.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("docker runtime: remove volume %q: %w", name, err)
	}
	return nil
}

// SuspendSandbox suspends the sandbox: it stops the container, which stops
// the session's program, and keeps it and /sandbox.
func (r *Runtime) SuspendSandbox(ctx context.Context, name string) (*sbx.Sandbox, error) {
	c, err := r.inspect(ctx, name)
	switch {
	case err != nil:
		return nil, err
	case c == nil:
		return nil, notFound(name)
	}
	timeout := stopTimeout
	if _, err := r.api.ContainerStop(ctx, name, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !cerrdefs.IsNotFound(err) {
		return nil, fmt.Errorf("docker runtime: stop container %q: %w", name, err)
	}
	return r.GetSandbox(ctx, name)
}

func (r *Runtime) Close() error { return r.api.Close() } //nolint:wrapcheck

// cleanup removes a half-made sandbox with a fresh context, so it still
// runs when the caller's was cancelled.
func (r *Runtime) cleanup(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = r.api.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true})
	_, _ = r.api.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true})
}

// toSandbox returns the sandbox of a container.
func toSandbox(c *container.InspectResponse) *sbx.Sandbox {
	sb := &sbx.Sandbox{
		Name:    strings.TrimPrefix(c.Name, "/"),
		Runtime: config.RuntimeDocker,
		Phase:   sbx.Unknown,
	}
	if c.Config != nil {
		sb.Labels = ownLabels(c.Config.Labels)
		sb.Annotations = annotations(c.Config.Labels)
		sb.Image = c.Config.Image
	}
	if c.State != nil {
		sb.Phase = phase(c.State.Status, c.State.Restarting)
	}
	if t, err := time.Parse(time.RFC3339Nano, c.Created); err == nil {
		sb.CreatedAt = t
	}
	return sb
}

// ownLabels returns Sundae's ownership labels among a container's.
func ownLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for _, k := range []string{sbx.LabelManaged, sbx.LabelUserID, LabelRuntime} {
		if v, ok := labels[k]; ok {
			out[k] = v
		}
	}
	return out
}

// annotations returns the session's annotations, which the container
// keeps as sundae.* labels.
func annotations(labels map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		if strings.HasPrefix(k, "sundae.") && k != sbx.LabelManaged && k != sbx.LabelUserID {
			out[k] = v
		}
	}
	return out
}

// phase maps a container state to a sandbox phase. A stopped container is
// suspended: starting it resumes the session.
func phase(state container.ContainerState, restarting bool) sbx.Phase {
	if restarting {
		return sbx.Starting
	}
	switch state {
	case container.StateRunning:
		return sbx.Ready
	case container.StateRestarting:
		return sbx.Starting
	case container.StateCreated, container.StateExited, container.StatePaused:
		return sbx.Suspended
	case container.StateRemoving:
		return sbx.Deleting
	case container.StateDead:
		return sbx.Error
	}
	return sbx.Unknown
}
