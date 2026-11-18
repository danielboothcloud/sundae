// Package dockertest is an in-memory Docker engine for the docker
// runtime's tests. It keeps containers and volumes; exec is not
// implemented (tests replace the runtime's stream).
package dockertest

import (
	"context"
	"errors"
	"io"
	"iter"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// Container is a fake container.
type Container struct {
	Name       string
	Config     container.Config
	HostConfig container.HostConfig
	Status     container.ContainerState
	Created    time.Time
}

// Engine is a fake engine.
type Engine struct {
	mu         sync.Mutex
	Containers map[string]*Container
	Volumes    map[string]client.VolumeCreateOptions
	// Images are the images present; others must be pulled.
	Images map[string]bool
	Pulls  []string
	// StartFails makes ContainerStart fail.
	StartFails error
	// OnStart is called with each container started.
	OnStart func(name string)
}

// New returns an empty engine.
func New() *Engine {
	return &Engine{Containers: map[string]*Container{}, Volumes: map[string]client.VolumeCreateOptions{}, Images: map[string]bool{}}
}

func errNotFound(what string) error { return cerrdefs.ErrNotFound.WithMessage(what + " not found") }

func (e *Engine) ContainerCreate(_ context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.Containers[o.Name]; ok {
		return client.ContainerCreateResult{}, cerrdefs.ErrConflict.WithMessage("name in use")
	}
	if !e.Images[o.Config.Image] {
		return client.ContainerCreateResult{}, errNotFound("image " + o.Config.Image)
	}
	for _, m := range o.HostConfig.Mounts {
		if _, ok := e.Volumes[m.Source]; !ok {
			return client.ContainerCreateResult{}, errNotFound("volume " + m.Source)
		}
	}
	e.Containers[o.Name] = &Container{Name: o.Name, Config: *o.Config, HostConfig: *o.HostConfig, Status: container.StateCreated, Created: time.Now()}
	return client.ContainerCreateResult{ID: o.Name}, nil
}

func (e *Engine) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	e.mu.Lock()
	c, ok := e.Containers[id]
	if !ok {
		e.mu.Unlock()
		return client.ContainerStartResult{}, errNotFound("container")
	}
	if e.StartFails != nil {
		e.mu.Unlock()
		return client.ContainerStartResult{}, e.StartFails
	}
	c.Status = container.StateRunning
	on := e.OnStart
	e.mu.Unlock()
	if on != nil {
		on(id)
	}
	return client.ContainerStartResult{}, nil
}

func (e *Engine) ContainerStop(_ context.Context, id string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.Containers[id]
	if !ok {
		return client.ContainerStopResult{}, errNotFound("container")
	}
	c.Status = container.StateExited
	return client.ContainerStopResult{}, nil
}

func (e *Engine) ContainerRemove(_ context.Context, id string, o client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.Containers[id]
	if !ok {
		return client.ContainerRemoveResult{}, errNotFound("container")
	}
	if c.Status == container.StateRunning && !o.Force {
		return client.ContainerRemoveResult{}, cerrdefs.ErrConflict.WithMessage("running")
	}
	delete(e.Containers, id)
	return client.ContainerRemoveResult{}, nil
}

func (e *Engine) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.Containers[id]
	if !ok {
		return client.ContainerInspectResult{}, errNotFound("container")
	}
	cfg, hc := c.Config, c.HostConfig
	return client.ContainerInspectResult{Container: container.InspectResponse{
		ID:         c.Name,
		Name:       "/" + c.Name,
		Created:    c.Created.Format(time.RFC3339Nano),
		State:      &container.State{Status: c.Status, Running: c.Status == container.StateRunning},
		Config:     &cfg,
		HostConfig: &hc,
	}}, nil
}

func (e *Engine) ContainerList(_ context.Context, o client.ContainerListOptions) (client.ContainerListResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out client.ContainerListResult
next:
	for _, c := range e.Containers {
		for kv := range o.Filters["label"] {
			k, v, _ := strings.Cut(kv, "=")
			if c.Config.Labels[k] != v {
				continue next
			}
		}
		out.Items = append(out.Items, container.Summary{
			ID: c.Name, Names: []string{"/" + c.Name}, Image: c.Config.Image,
			Labels: c.Config.Labels, State: c.Status, Created: c.Created.Unix(),
		})
	}
	return out, nil
}

func (e *Engine) VolumeCreate(_ context.Context, o client.VolumeCreateOptions) (client.VolumeCreateResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.Volumes[o.Name]; !ok {
		e.Volumes[o.Name] = o
	}
	return client.VolumeCreateResult{Volume: volume.Volume{Name: o.Name}}, nil
}

func (e *Engine) VolumeRemove(_ context.Context, id string, _ client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.Volumes[id]; !ok {
		return client.VolumeRemoveResult{}, errNotFound("volume")
	}
	delete(e.Volumes, id)
	return client.VolumeRemoveResult{}, nil
}

func (e *Engine) ImagePull(_ context.Context, ref string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Pulls = append(e.Pulls, ref)
	if strings.Contains(ref, "missing") {
		return nil, errNotFound("manifest")
	}
	e.Images[ref] = true
	return pullResponse{}, nil
}

type pullResponse struct{}

func (pullResponse) Read([]byte) (int, error) { return 0, io.EOF }
func (pullResponse) Close() error             { return nil }
func (pullResponse) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return func(func(jsonstream.Message, error) bool) {}
}
func (pullResponse) Wait(context.Context) error { return nil }

var errNoExec = errors.New("dockertest: exec is not implemented; replace the runtime's stream")

func (e *Engine) ExecCreate(context.Context, string, client.ExecCreateOptions) (client.ExecCreateResult, error) {
	return client.ExecCreateResult{}, errNoExec
}

func (e *Engine) ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error) {
	return client.ExecAttachResult{}, errNoExec
}

func (e *Engine) ExecInspect(context.Context, string, client.ExecInspectOptions) (client.ExecInspectResult, error) {
	return client.ExecInspectResult{}, errNoExec
}

func (e *Engine) ExecResize(context.Context, string, client.ExecResizeOptions) (client.ExecResizeResult, error) {
	return client.ExecResizeResult{}, errNoExec
}

func (e *Engine) Close() error { return nil }
