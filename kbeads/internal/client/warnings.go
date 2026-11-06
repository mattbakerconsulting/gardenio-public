package client

import (
	"context"
	"sync"
)

// WarningHeader is the response header the daemon sets for an advisory,
// non-fatal note about a successful write — e.g. "short_title missing: …"
// while BEADS_SHORT_TITLE_GATE is in warn mode. It may repeat.
const WarningHeader = "X-Beads-Warning"

// Warnings collects the WarningHeader values seen on successful responses
// made with a context from WithWarnings. Safe for concurrent use.
type Warnings struct {
	mu   sync.Mutex
	msgs []string
}

// List returns the collected warnings in the order they arrived.
func (w *Warnings) List() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.msgs...)
}

type warningsKeyType struct{}

// WithWarnings returns a context whose HTTP requests record any
// X-Beads-Warning response headers into the returned collector. The CLI uses
// it to echo server warnings on stderr without changing the Client interface
// or the bead it prints. The gRPC transport does not record warnings.
func WithWarnings(ctx context.Context) (context.Context, *Warnings) {
	w := &Warnings{}
	return context.WithValue(ctx, warningsKeyType{}, w), w
}

// noteWarnings appends msgs to the context's collector, if it has one.
func noteWarnings(ctx context.Context, msgs []string) {
	if len(msgs) == 0 {
		return
	}
	w, _ := ctx.Value(warningsKeyType{}).(*Warnings)
	if w == nil {
		return
	}
	w.mu.Lock()
	w.msgs = append(w.msgs, msgs...)
	w.mu.Unlock()
}
