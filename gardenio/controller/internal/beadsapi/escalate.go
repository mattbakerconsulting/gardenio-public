package beadsapi

import (
	"context"

	"gardenio/clicreds/beadswire"
)

// WithEscalate returns a context that signals the client to set
// X-Escalate: true on the next HTTP request made with it; the daemon answers
// an escalatable denial with 202 and an *EscalationCreated. Shared with kd's
// --escalate flag via beadswire; used by `gb spawn --escalate`
// (gd-lsgF8rL4Zm).
func WithEscalate(ctx context.Context) context.Context {
	return beadswire.WithEscalate(ctx)
}

// EscalateFromContext reports whether ctx was marked via WithEscalate.
func EscalateFromContext(ctx context.Context) bool {
	return beadswire.EscalateFromContext(ctx)
}

// EscalationCreated is returned (as an error, via errors.As) when the daemon
// responds 202 Accepted after auto-creating an escalation bead in response
// to an X-Escalate: true request that hit an escalatable denial.
type EscalationCreated = beadswire.EscalationCreated
