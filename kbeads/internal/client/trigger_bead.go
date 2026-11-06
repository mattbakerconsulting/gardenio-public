package client

import "context"

type triggerBeadKeyType struct{}

// WithTriggerBead returns a context that signals the client to set
// X-Trigger-Bead: <id> on HTTP requests. Used by `kd claim` when the
// BOAT_TRIGGER_BEAD_ID env var is set (auto-link M3, ka-0000003164).
// The server uses the header on a claim transition (status→in_progress)
// to write a triggered-by dep from the claimed task to the trigger bead,
// but only if no parent-child or triggered-by dep already exists.
func WithTriggerBead(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, triggerBeadKeyType{}, id)
}

// TriggerBeadFromContext returns the trigger bead ID stored on the context,
// or "" if none was set.
func TriggerBeadFromContext(ctx context.Context) string {
	v, _ := ctx.Value(triggerBeadKeyType{}).(string)
	return v
}
