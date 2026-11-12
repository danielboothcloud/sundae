package events

import "context"

// ContextKey is the key for the event bus in a context.
var ContextKey = &struct{ string }{"events"}

// FromContext returns the event bus from a context, or nil when there is
// none. A nil *Bus is safe to publish to.
func FromContext(ctx context.Context) *Bus {
	if b, ok := ctx.Value(ContextKey).(*Bus); ok {
		return b
	}
	return nil
}

// WithContext returns a new context with the event bus attached.
func WithContext(ctx context.Context, b *Bus) context.Context {
	return context.WithValue(ctx, ContextKey, b)
}
