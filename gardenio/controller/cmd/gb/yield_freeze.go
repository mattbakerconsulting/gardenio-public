package main

// yield_freeze.go — freeze-on-yield for freeze-eligible agents (Δ4/A2,
// ka-0000000241).
//
// `gb yield` holds a LIVE pod parked on an open decision by design (yield.go
// long-doc): the pod blocks in-process on SSE until the operator resolves the
// decision, then unblocks WARM in the same turn. That is optimal for a SHORT
// wait — a few-minute decision resolves faster than any thaw could restore.
//
// For a freeze-eligible agent (runsc + FREEZE_THAW_ENABLED + role allowlist,
// signalled by the controller-injected BOAT_FREEZE_ELIGIBLE the same way gb
// stop's shouldSuspend reads it) a LONG wait is pure waste: a whole node slot
// held idle for what may be hours or days until a human answers. Δ4/A2 converts
// that long hold into "checkpoint + free the slot + thaw on resolution":
//
//   1. gb yield blocks live for up to a config'd freeze-after threshold — SHORT
//      yields resolve inside that window and stay warm, exactly as today.
//   2. If the decision is STILL open past the threshold, gb yield stops holding
//      the pod: it SUSPENDS the agent (the same agent_state=stopping freeze
//      trigger gb stop and the A1 idle path use — suspendAgent, stop.go), which
//      the controller observes to `runsc checkpoint --leave-running` the live
//      sandbox and tear down Pod A. The turn ends cleanly (decision gate
//      satisfied); the pod's node slot is freed.
//   3. The durable re-entry handle is UNCHANGED: the open decision bead filed
//      with --requesting-task. When the operator resolves it, the bridge's
//      decision-resume respawn (decisions.go) re-mints the same-name agent — and
//      because A1 left the bead OPEN at agent_state=stopping with a persisted
//      snapshot, that spawn THAWS the frozen process instead of cold-starting.
//
// Default-OFF and conservative: freeze-on-yield is inert unless (a) the agent is
// freeze-eligible AND (b) a "yield-freeze" config bead enables it for this
// scope with a positive freeze_after_minutes. With neither, yield holds live
// forever exactly as before — no behavior change for any existing agent.

import (
	"context"
	"fmt"
	"os"
	"time"
)

// yieldFreezeAfterDefault is the threshold applied when a "yield-freeze" config
// bead enables the feature (enabled=true) but omits freeze_after_minutes. 15m is
// deliberately well above any plausible thaw-latency budget: a decision that
// resolves inside 15m stays warm (no freeze), and only a genuinely long park —
// where holding the node idle is the real waste — trips the checkpoint. The
// value is overridable per scope via the config bead's freeze_after_minutes.
const yieldFreezeAfterDefault = 15 * time.Minute

// yieldFreezeConfig is the resolved per-agent freeze-on-yield policy.
type yieldFreezeConfig struct {
	// enabled gates the whole feature. Default false (config-absent) so yield
	// holds live forever unless an operator opts a project/role/agent in.
	enabled bool

	// freezeAfter is how long yield blocks LIVE before, if the decision is still
	// open, freezing the pod. Zero (config omits freeze_after_minutes) falls back
	// to yieldFreezeAfterDefault at arm time.
	freezeAfter time.Duration
}

// resolveYieldFreeze determines whether this yield should freeze the pod after a
// long wait, and after how long. It is a no-op (disabled) unless the agent is
// freeze-eligible AND a "yield-freeze" config bead enables it for this scope —
// the same default-off, opt-in shape resolveIdleTTL uses for A1. Resolution
// order (global < project < role < agent) is handled by ResolveConfigBeads.
func resolveYieldFreeze(ctx context.Context) yieldFreezeConfig {
	cfg := yieldFreezeConfig{}

	// Gate 1: the controller-injected freeze-eligibility signal. A non-runsc /
	// freeze-disabled agent can never be checkpointed, so freeze-on-yield stays
	// off regardless of config — mirrors gb stop's shouldSuspend and the A1 idle
	// path, so the in-pod yield decision and the controller's freeze decision
	// never diverge.
	if !freezeEligible() || daemon == nil {
		return cfg
	}

	// Gate 2: an opt-in "yield-freeze" config bead for this scope.
	subs := buildSubscriptions(os.Getenv("BOAT_ROLE"))
	merged, count, err := ResolveConfigBeads(ctx, daemon, "yield-freeze", subs)
	if err != nil || count == 0 {
		return cfg
	}
	if v, ok := merged["enabled"].(bool); ok {
		cfg.enabled = v
	}
	if m, ok := idleTTLMinutes(merged["freeze_after_minutes"]); ok && m > 0 {
		cfg.freezeAfter = time.Duration(m * float64(time.Minute))
	}
	return cfg
}

// yieldFreezeDeadline returns the absolute time at which a live yield should
// freeze, or the zero Time when freeze-on-yield is disabled for this agent. An
// absolute deadline (rather than a re-armed duration) keeps the threshold stable
// across the SSE→poll fallback inside the wait loop: elapsed live-hold time is
// never reset by a transient reconnect.
func yieldFreezeDeadline(cfg yieldFreezeConfig) time.Time {
	if !cfg.enabled {
		return time.Time{}
	}
	after := cfg.freezeAfter
	if after <= 0 {
		after = yieldFreezeAfterDefault
	}
	return time.Now().Add(after)
}

// freezeChan returns a channel that fires at the freeze deadline, or nil (a
// forever-blocking receive) when freeze-on-yield is disabled. Recomputing it
// from the absolute deadline on each wait-loop entry (including the SSE→poll
// fallback) means the threshold reflects real elapsed time, not time-since-this-
// call.
func freezeChan(freezeAt time.Time) <-chan time.Time {
	if freezeAt.IsZero() {
		return nil
	}
	d := time.Until(freezeAt)
	if d < 0 {
		d = 0
	}
	return time.After(d)
}

// freezeOnYield suspends a freeze-eligible agent whose yield has waited past the
// freeze-after threshold with the decision still open. It ENDS the turn: the
// pod is checkpointed (the controller observes agent_state=stopping and runs
// `runsc checkpoint --leave-running`) and its node slot is freed, and the agent
// re-enters via the standard decision-resume respawn — which thaws the frozen
// process — when the operator resolves the decision.
//
// The freeze mechanics are DELEGATED to suspendAgent (stop.go), the single
// canonical freeze path gb stop uses: satisfy the decision gate + record
// gate_satisfied_by=operator (so the Stop hook lets the turn end quietly
// without re-blocking), fsync transcripts, transition agent_state=stopping
// WITHOUT closing the bead and WITHOUT requesting coop shutdown (the checkpoint
// must capture the LIVE Claude process). Reusing it keeps the yield-freeze and
// stop-freeze paths identical — any future fix to the suspend contract applies
// to both, and the two can never diverge.
//
// A fresh timeout context is used for the teardown writes rather than the yield
// command context, mirroring suspendIdleAgent / the stop path: the session ctx
// may already be cancelling as the turn ends, which would chop the writes.
func freezeOnYield(agentID, decisionID string) error {
	fmt.Printf("Yield exceeded the freeze-after threshold with decision %s still open.\n", decisionID)
	fmt.Printf("Freezing this agent to free its node slot — your turn ends now. You will be THAWED (state restored, same identity) when the operator resolves the decision; there is nothing more to do this turn.\n")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return suspendAgent(ctx, agentID)
}
