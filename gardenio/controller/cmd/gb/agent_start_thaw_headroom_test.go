package main

// agent_start_thaw_headroom_test.go — RED→GREEN proofs for the gb-side
// ack-write headroom (ka-0000003746 RANK5).
//
// The watcher runs the handler under the marker's FULL deadline and only
// writes the ack after the handler returns, while the controller gate's window
// is anchored at marker-write time — up to one watcher poll tick EARLIER than
// the handler's detection-anchored window. Without headroom, retryUntilDeadline
// keeps attempting to the very end of the handler window, so a last attempt
// succeeding at deadline-epsilon acks after the gate already fail-closed. The
// handler now caps its stage budget at thawAckHeadroomFraction of the remaining
// deadline (mirroring coop's RECONNECT_RETRY_FRACTION), reserving the tail for
// the ack write.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/thawepoch"
)

func TestWithAckHeadroom_CapsBelowParentDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sctx, scancel := withAckHeadroom(parent)
	defer scancel()

	pdl, _ := parent.Deadline()
	sdl, ok := sctx.Deadline()
	if !ok {
		t.Fatal("headroom context must carry a deadline when parent has one")
	}
	if !sdl.Before(pdl) {
		t.Fatalf("headroom deadline %v must be earlier than parent %v (the tail is the ack-write budget)", sdl, pdl)
	}
	// ~15% of 10s reserved: the gap must be a real slice, not clock noise.
	if gap := pdl.Sub(sdl); gap < time.Second {
		t.Fatalf("headroom gap = %v, want >= 1s of a 10s deadline reserved for the ack write", gap)
	}

	// No parent deadline → passthrough (single-shot semantics downstream).
	sctx2, scancel2 := withAckHeadroom(context.Background())
	defer scancel2()
	if _, ok := sctx2.Deadline(); ok {
		t.Fatal("headroom context must not invent a deadline when parent has none")
	}
}

// TestGBThawHandler_HeartbeatRunsUnderAckHeadroom proves the handler's required
// stage retries under the HEADROOM-capped budget, not the full marker window:
// the deadline the heartbeat observes must sit strictly inside the handler
// ctx's deadline. RED on unmodified source (heartbeat retried under ctx
// directly, so the observed deadline equaled the parent's — the last attempt
// could run to deadline-epsilon and the ack write landed after gate expiry).
// Deadline observation keeps this deterministic — no sleeps, no elapsed-time
// assertions.
func TestGBThawHandler_HeartbeatRunsUnderAckHeadroom(t *testing.T) {
	var mu sync.Mutex
	var observed time.Time
	h := newGBThawHandler(thawDeps{
		heartbeat: func(hctx context.Context) error {
			// The per-attempt clamp is 2s; with only 1s remaining the attempt
			// ctx inherits the earlier (headroom) deadline, so this observes
			// the stage budget directly.
			if dl, ok := hctx.Deadline(); ok {
				mu.Lock()
				if observed.IsZero() || dl.After(observed) {
					observed = dl
				}
				mu.Unlock()
			}
			return nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pdl, _ := ctx.Deadline()

	ack, err := h(ctx, thawepoch.Marker{Epoch: 1})
	if err != nil || !ack.OK {
		t.Fatalf("handler: err=%v ack=%+v", err, ack)
	}
	mu.Lock()
	defer mu.Unlock()
	if observed.IsZero() {
		t.Fatal("heartbeat never observed a deadline; expected the headroom-capped stage ctx")
	}
	// 0.85 × 1s leaves a ~150ms tail; assert a solid slice of it so scheduler
	// jitter cannot flake the check.
	if !observed.Before(pdl.Add(-50 * time.Millisecond)) {
		t.Fatalf("heartbeat deadline %v is not meaningfully inside the handler deadline %v — "+
			"no ack-write headroom reserved", observed, pdl)
	}
}

// TestGBThawHandler_HeadroomExhaustionStillFailsClosed pins the fail-closed
// contract: a heartbeat that never lands exhausts the headroom budget and the
// handler acks OK=false — the headroom shrinks the retry window; it never turns
// a hard failure into a release.
func TestGBThawHandler_HeadroomExhaustionStillFailsClosed(t *testing.T) {
	h := newGBThawHandler(thawDeps{
		heartbeat: func(context.Context) error {
			return errors.New("Temporary failure in name resolution")
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ack, err := h(ctx, thawepoch.Marker{Epoch: 1})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if ack.OK {
		t.Fatalf("expected fail-closed ack when the heartbeat never lands, got %+v", ack)
	}
	if ack.Stages["heartbeat"] != "failed" {
		t.Fatalf("expected heartbeat failed, got %q", ack.Stages["heartbeat"])
	}
}
