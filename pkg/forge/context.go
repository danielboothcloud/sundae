package forge

import "context"

// ContextKey is the key for the forge service in a context.
var ContextKey = &struct{ string }{"forge"}

// FromContext returns the forge service from a context, or nil when forges
// are not configured.
func FromContext(ctx context.Context) *Service {
	if s, ok := ctx.Value(ContextKey).(*Service); ok {
		return s
	}
	return nil
}

// WithContext returns a new context with the forge service attached.
func WithContext(ctx context.Context, s *Service) context.Context {
	return context.WithValue(ctx, ContextKey, s)
}
