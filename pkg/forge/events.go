package forge

import (
	"context"
	"time"
)

// EventKind is the kind of a forge event.
type EventKind string

const (
	// EventImport is sent once per repository import attempt.
	EventImport EventKind = "import"
	// EventSync is sent once per repository sync, scheduled or on demand.
	EventSync EventKind = "sync"
	// EventPush is sent once per push attempt.
	EventPush EventKind = "push"
	// EventDiscover is sent when listing a forge's repositories fails or
	// succeeds. Repo and Path are empty.
	EventDiscover EventKind = "discover"
)

// Event is the outcome of one forge operation.
type Event struct {
	Kind  EventKind `json:"kind"`
	Forge string    `json:"forge"`
	// Repo is the Soft Serve repository name; Path the forge path.
	Repo string    `json:"repo,omitempty"`
	Path string    `json:"path,omitempty"`
	Time time.Time `json:"time"`
	// Err is nil on success.
	Err error `json:"-"`
	// Sync is set for import and sync events that got as far as syncing.
	Sync *SyncResult `json:"sync,omitempty"`
	// Push is set for successful push events.
	Push *PushResult `json:"push,omitempty"`
}

// Notifier receives forge events. It is the seam the in-process event bus
// subscribes through (sync results feed a TUI tab and notifications).
// ForgeEvent is called synchronously from the operation, so it must not
// block; hand the event off instead.
type Notifier interface {
	ForgeEvent(ctx context.Context, ev Event)
}

// NotifierFunc adapts a function to a Notifier.
type NotifierFunc func(ctx context.Context, ev Event)

// ForgeEvent implements Notifier.
func (f NotifierFunc) ForgeEvent(ctx context.Context, ev Event) { f(ctx, ev) }

// nopNotifier is the default Notifier: it drops every event.
type nopNotifier struct{}

func (nopNotifier) ForgeEvent(context.Context, Event) {}
