package hooks

import (
	"context"
	"io"
)

// HookArg is an argument to a git hook.
type HookArg struct {
	OldSha  string
	NewSha  string
	RefName string
}

// Hooks provides an interface for git server-side hooks.
type Hooks interface {
	// PreReceive refuses the whole push by returning an error; its message
	// is shown to the pusher.
	PreReceive(ctx context.Context, stdout io.Writer, stderr io.Writer, repo string, args []HookArg) error
	Update(ctx context.Context, stdout io.Writer, stderr io.Writer, repo string, arg HookArg)
	PostReceive(ctx context.Context, stdout io.Writer, stderr io.Writer, repo string, args []HookArg)
	PostUpdate(ctx context.Context, stdout io.Writer, stderr io.Writer, repo string, args ...string)
}
