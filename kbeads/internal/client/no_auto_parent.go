package client

import "context"

type noAutoParentKeyType struct{}

// WithNoAutoParent returns a context that signals the client to set
// X-No-Auto-Parent: 1 on HTTP requests. Used by `kd create --no-parent` and by
// `kd create --parent-task <id>` (which adds the parent dep on the CLI side and
// must therefore suppress the server's auto-parent step to avoid a duplicate
// resolver attempt). The server-side dep insert is idempotent regardless.
func WithNoAutoParent(ctx context.Context) context.Context {
	return context.WithValue(ctx, noAutoParentKeyType{}, true)
}

// NoAutoParentFromContext returns true if the context was marked with
// WithNoAutoParent.
func NoAutoParentFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(noAutoParentKeyType{}).(bool)
	return v
}

type forceParentEdgeKeyType struct{}

// WithForceParentEdge returns a context that signals the client to set
// X-Force-Parent-Edge: 1, overriding the server's closed-parent guard on an
// explicit parent-child dep-add (ka-0000005756). Used by `kd dep add --force`
// to deliberately re-parent new work onto a recently-closed epic.
func WithForceParentEdge(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceParentEdgeKeyType{}, true)
}

// ForceParentEdgeFromContext returns true if the context was marked with
// WithForceParentEdge.
func ForceParentEdgeFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(forceParentEdgeKeyType{}).(bool)
	return v
}
