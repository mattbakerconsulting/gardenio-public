package thawepoch

import (
	"context"
	"sort"
	"time"
)

// GateResult is the outcome of waiting on rehydration acks for one epoch.
type GateResult struct {
	Epoch int64
	// Released is true iff every required consumer acked OK within the
	// deadline. When false the thaw failed closed (C5) and the controller
	// must not mark the agent live.
	Released bool
	// Acks holds the final ack observed per consumer (may be missing entries
	// for consumers that never acked).
	Acks map[string]Ack
	// FailedConsumers lists consumers that either never acked or acked with
	// OK=false, in deterministic order. Empty iff Released.
	FailedConsumers []string
	// Elapsed is how long the gate waited.
	Elapsed time.Duration
}

// GateOptions configures WaitForAcks.
type GateOptions struct {
	// Required is the set of consumers whose OK ack is mandatory for release.
	// Defaults to {ConsumerGB, ConsumerCoop} when empty.
	Required []string
	// Deadline overrides the marker's deadline when > 0.
	Deadline time.Duration
	// PollInterval defaults to DefaultPollInterval when zero.
	PollInterval time.Duration
}

func (o GateOptions) required() []string {
	if len(o.Required) > 0 {
		return o.Required
	}
	return []string{ConsumerGB, ConsumerCoop}
}

func (o GateOptions) interval() time.Duration {
	if o.PollInterval > 0 {
		return o.PollInterval
	}
	return DefaultPollInterval
}

// WaitForAcks is the controller-side deadline gate (rehydration-design §4
// Stage 5). After writing the marker for epoch, the controller calls this to
// block until every required consumer has written an OK ack for *that epoch*,
// or the deadline expires. It is the fail-closed barrier that prevents a
// half-rehydrated agent from resuming authenticated work.
//
// The deadline is, in priority order: opts.Deadline, the marker's Deadline(),
// DefaultDeadline. ctx cancellation aborts early with Released=false.
func WaitForAcks(ctx context.Context, stateDir string, epoch int64, opts GateOptions) GateResult {
	required := opts.required()
	deadline := opts.Deadline
	if deadline <= 0 {
		if m, ok, err := Read(stateDir); err == nil && ok && m.Epoch == epoch {
			deadline = m.Deadline()
		} else {
			deadline = DefaultDeadline
		}
	}

	start := time.Now()
	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	tick := time.NewTicker(opts.interval())
	defer tick.Stop()

	eval := func() (GateResult, bool) { // OK: Go closure identifier named "eval", not a shell eval invocation
		acks := make(map[string]Ack, len(required))
		var failed []string
		allOK := true
		for _, c := range required {
			a, ok, err := ReadAck(stateDir, c)
			if ok {
				acks[c] = a
			}
			if err != nil || !ok || a.Epoch != epoch || !a.OK {
				allOK = false
				failed = append(failed, c)
			}
		}
		sort.Strings(failed)
		return GateResult{
			Epoch:           epoch,
			Released:        allOK,
			Acks:            acks,
			FailedConsumers: failed,
			Elapsed:         time.Since(start),
		}, allOK
	}

	// Evaluate immediately — consumers may already have acked (the watcher
	// fires on an eager nudge, and acks can land before the gate starts).
	if res, done := eval(); done {
		return res
	}

	for {
		select {
		case <-dctx.Done():
			res, _ := eval()
			res.Elapsed = time.Since(start)
			return res // Released=false unless a final ack raced in
		case <-tick.C:
			if res, done := eval(); done {
				return res
			}
		}
	}
}
