package client

import (
	"context"

	"gardenio/clicreds/beadswire"
)

// WithEscalate returns a context that signals the client to set X-Escalate: true
// on HTTP requests. Used by the CLI --escalate flag to trigger the server-side
// escalation-on-retry flow.
func WithEscalate(ctx context.Context) context.Context {
	return beadswire.WithEscalate(ctx)
}

// EscalateFromContext returns true if the context was marked with WithEscalate.
func EscalateFromContext(ctx context.Context) bool {
	return beadswire.EscalateFromContext(ctx)
}

type forceCloseKeyType struct{}

// WithForceClose returns a context that signals the client to set
// X-Force-Close: true on HTTP requests. Used by `kd close --force` to ask the
// server to close a task or bug whose linked PR is still open; the server
// honors it only from an operator's sign-in (gd-lIKFejVISw).
func WithForceClose(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceCloseKeyType{}, true)
}

// ForceCloseFromContext returns true if the context was marked with
// WithForceClose.
func ForceCloseFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(forceCloseKeyType{}).(bool)
	return v
}
