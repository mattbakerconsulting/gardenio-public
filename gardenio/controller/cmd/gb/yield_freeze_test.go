package main

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestYieldFreezeDeadline_DisabledIsZero verifies the default-off contract: a
// disabled config (the config-absent case) yields the zero Time, which the wait
// loop treats as "never freeze — hold live forever", i.e. today's behavior.
func TestYieldFreezeDeadline_DisabledIsZero(t *testing.T) {
	if got := yieldFreezeDeadline(yieldFreezeConfig{enabled: false, freezeAfter: time.Hour}); !got.IsZero() {
		t.Errorf("disabled freeze config must produce the zero deadline, got %v", got)
	}
}

// TestYieldFreezeDeadline_EnabledDefaultsWhenNoMinutes verifies an enabled
// config that omits freeze_after_minutes falls back to yieldFreezeAfterDefault
// rather than freezing immediately (a zero freezeAfter must NOT mean "freeze
// now").
func TestYieldFreezeDeadline_EnabledDefaultsWhenNoMinutes(t *testing.T) {
	before := time.Now().Add(yieldFreezeAfterDefault - time.Second)
	got := yieldFreezeDeadline(yieldFreezeConfig{enabled: true, freezeAfter: 0})
	if got.IsZero() {
		t.Fatal("enabled freeze config must produce a non-zero deadline")
	}
	if got.Before(before) {
		t.Errorf("enabled config with no minutes must default to ~%s out, got deadline %v (too soon)", yieldFreezeAfterDefault, got)
	}
}

// TestYieldFreezeDeadline_HonorsConfiguredMinutes verifies a positive
// freeze_after_minutes is used verbatim.
func TestYieldFreezeDeadline_HonorsConfiguredMinutes(t *testing.T) {
	got := yieldFreezeDeadline(yieldFreezeConfig{enabled: true, freezeAfter: 5 * time.Minute})
	lo, hi := time.Now().Add(4*time.Minute), time.Now().Add(6*time.Minute)
	if got.Before(lo) || got.After(hi) {
		t.Errorf("deadline %v not within [~5m] window [%v,%v]", got, lo, hi)
	}
}

// TestFreezeChan_ZeroNeverFires verifies a zero deadline produces a nil channel
// (a forever-blocking receive) so a disabled agent never freezes.
func TestFreezeChan_ZeroNeverFires(t *testing.T) {
	if ch := freezeChan(time.Time{}); ch != nil {
		t.Error("zero deadline must produce a nil (never-firing) channel")
	}
}

// TestFreezeChan_PastDeadlineFiresImmediately verifies an already-elapsed
// deadline (e.g. re-armed after the SSE→poll fallback consumed the whole window)
// fires promptly rather than waiting a negative duration.
func TestFreezeChan_PastDeadlineFiresImmediately(t *testing.T) {
	ch := freezeChan(time.Now().Add(-time.Hour))
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Error("past deadline should fire immediately, not block")
	}
}

// TestResolveYieldFreeze_DisabledWhenNotFreezeEligible verifies the first gate:
// a non-freeze-eligible agent (BOAT_FREEZE_ELIGIBLE unset) never freezes on
// yield, regardless of any config — so a runc agent's yield is unchanged.
func TestResolveYieldFreeze_DisabledWhenNotFreezeEligible(t *testing.T) {
	// BOAT_FREEZE_ELIGIBLE deliberately unset.
	cfg := resolveYieldFreeze(context.Background())
	if cfg.enabled {
		t.Error("a non-freeze-eligible agent must not enable freeze-on-yield")
	}
	if !yieldFreezeDeadline(cfg).IsZero() {
		t.Error("a non-freeze-eligible agent must produce the zero (never-freeze) deadline")
	}
}

// TestYieldSSE_FreezesAfterThreshold verifies the wait loop returns
// outcome.freeze once the freeze deadline elapses with the decision still open —
// the SSE path. The stream stays open with no resolving frame, so only the
// freeze deadline can end the wait.
func TestYieldSSE_FreezesAfterThreshold(t *testing.T) {
	stub := &yieldDaemonStub{
		beads: map[string]map[string]any{
			"gd-dec": beadMap("gd-dec", "decision", "data", "open", nil),
		},
		// A peer's mail (addressed to a DIFFERENT agent) — ignored by the wait
		// loop. Its only jobs here are to flush the SSE response headers (so
		// EventStream returns a live channel rather than blocking on an
		// unflushed stream) and to prove the freeze deadline still fires with
		// unrelated traffic on the wire.
		sseEvents: []sseFrame{
			{event: "beads.bead.created", data: map[string]any{
				"bead": map[string]any{
					"id": "gd-peer-mail", "type": "mail", "kind": "data",
					"assignee": "some-other-agent",
				},
			}},
		},
	}
	setupYieldDaemon(t, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pending := []*beadsapi.BeadDetail{{ID: "gd-dec", Type: "decision"}}
	freezeAt := time.Now().Add(50 * time.Millisecond)
	outcome := yieldSSE(ctx, pending, "agent-x", "agent-x", freezeAt)

	if !outcome.freeze {
		t.Errorf("expected freeze=true after the threshold with an open decision, got %+v", outcome)
	}
	if outcome.resolved {
		t.Error("a freeze is not a resolution")
	}
}

// TestYieldSSE_ResolutionBeatsFreeze verifies a decision that resolves before
// the deadline takes the WARM path — the freeze deadline must not fire a
// needless checkpoint when the answer already arrived.
func TestYieldSSE_ResolutionBeatsFreeze(t *testing.T) {
	stub := &yieldDaemonStub{
		beads: map[string]map[string]any{
			// Already closed: yieldPollOnce (5s ticker) and the SSE close frame
			// both resolve it well before the far-out freeze deadline.
			"gd-dec": beadMap("gd-dec", "decision", "data", "closed",
				map[string]any{"chosen": "yes"}),
		},
		sseEvents: []sseFrame{
			{event: "beads.bead.closed", data: map[string]any{
				"bead": map[string]any{"id": "gd-dec", "type": "decision", "kind": "data"},
			}},
		},
	}
	setupYieldDaemon(t, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pending := []*beadsapi.BeadDetail{{ID: "gd-dec", Type: "decision"}}
	freezeAt := time.Now().Add(3 * time.Second)
	outcome := yieldSSE(ctx, pending, "agent-x", "agent-x", freezeAt)

	if outcome.freeze {
		t.Error("a resolved decision must take the warm path, not freeze")
	}
	if !outcome.resolved {
		t.Errorf("expected resolved=true when the decision closes before the deadline, got %+v", outcome)
	}
}

// TestYieldPoll_FreezesAfterThreshold verifies the poll fallback path (used when
// SSE is unavailable) also honors the freeze deadline.
func TestYieldPoll_FreezesAfterThreshold(t *testing.T) {
	stub := &yieldDaemonStub{
		beads: map[string]map[string]any{
			"gd-dec": beadMap("gd-dec", "decision", "data", "open", nil),
		},
	}
	setupYieldDaemon(t, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	freezeAt := time.Now().Add(50 * time.Millisecond)
	outcome := yieldPoll(ctx, map[string]bool{"gd-dec": true}, "agent-x", freezeAt)

	if !outcome.freeze {
		t.Errorf("expected freeze=true from the poll path after the threshold, got %+v", outcome)
	}
}

// TestYieldPoll_ZeroDeadlineNeverFreezes verifies the disabled case: with the
// zero deadline the poll loop blocks until ctx expiry (never freezes), exactly
// as today.
func TestYieldPoll_ZeroDeadlineNeverFreezes(t *testing.T) {
	stub := &yieldDaemonStub{
		beads: map[string]map[string]any{
			"gd-dec": beadMap("gd-dec", "decision", "data", "open", nil),
		},
	}
	setupYieldDaemon(t, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	outcome := yieldPoll(ctx, map[string]bool{"gd-dec": true}, "agent-x", time.Time{})
	if outcome.freeze {
		t.Error("zero deadline must never freeze")
	}
	if outcome.resolved {
		t.Error("open decision must not resolve")
	}
}
