package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/thawepoch"
)

func TestGBThawHandlerHeartbeatOnly(t *testing.T) {
	var beats int
	h := newGBThawHandler(thawDeps{
		heartbeat: func(context.Context) error { beats++; return nil },
	})
	ack, err := h(context.Background(), thawepoch.Marker{Epoch: 1})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !ack.OK {
		t.Fatalf("expected OK ack, got %+v", ack)
	}
	if beats != 1 {
		t.Fatalf("expected exactly one heartbeat, got %d", beats)
	}
	if ack.Stages["heartbeat"] != "ok" {
		t.Fatalf("expected heartbeat ok, got %q", ack.Stages["heartbeat"])
	}
}

func TestGBThawHandlerHeartbeatFailureFailsClosed(t *testing.T) {
	h := newGBThawHandler(thawDeps{
		heartbeat: func(context.Context) error { return errors.New("beads down") },
	})
	ack, err := h(context.Background(), thawepoch.Marker{Epoch: 1})
	if err != nil {
		t.Fatalf("handler returned err (should encode in ack): %v", err)
	}
	if ack.OK {
		t.Fatal("required heartbeat failure must fail-close (OK=false)")
	}
	if !strings.Contains(ack.Error, "heartbeat") {
		t.Fatalf("error should mention heartbeat, got %q", ack.Error)
	}
}

// startK8sGBThawWatcher is the k8s-entrypoint wiring (gb init / gb agent start
// --k8s) for the gb-side ConsumerGB watcher. The prod bug (ka-0000004330) was
// that NO k8s path armed the gate, so thaw-epoch.gb.armed never existed and the
// {gb,coop} rehydrate gate could never release. This asserts the boot-time arm
// happens when COOP_THAW_STATE_DIR is set.
func TestStartK8sGBThawWatcherArmsWhenThawDirSet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COOP_THAW_STATE_DIR", dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startK8sGBThawWatcher(ctx, "gd-agent", "agent-name", t.TempDir())

	// WriteArmed runs synchronously inside startGBThawWatcher before the watcher
	// goroutine launches, so the sentinel is present on return.
	armed, err := thawepoch.IsArmed(dir, thawepoch.ConsumerGB)
	if err != nil {
		t.Fatalf("IsArmed: %v", err)
	}
	if !armed {
		t.Fatalf("expected thaw-epoch.gb.armed sentinel to exist at %s after wiring", dir)
	}
}

// With no thaw-state dir configured (non-gVisor pod), the wiring is a no-op: it
// must NOT arm a watcher (nothing to consume) and must not panic.
func TestStartK8sGBThawWatcherNoopWhenThawDirUnset(t *testing.T) {
	t.Setenv("COOP_THAW_STATE_DIR", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Should return cleanly without arming anything.
	startK8sGBThawWatcher(ctx, "gd-agent", "agent-name", t.TempDir())
}

// retryUntilDeadline absorbs a transient failure (cold-netns DNS EAI_AGAIN on a
// just-restored pod, bug ka-0000001492) and succeeds once the closure recovers,
// as long as ctx carries a deadline.
func TestRetryUntilDeadlineRecoversTransient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	attempts := 0
	err := retryUntilDeadline(ctx, func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("Temporary failure in name resolution")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected recovery, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts (2 transient + 1 ok), got %d", attempts)
	}
}

// A connect that never recovers returns the last error once the deadline passes
// — the caller still fail-closes; we never hang.
func TestRetryUntilDeadlineTimesOutWithLastError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	attempts := 0
	want := errors.New("never routable")
	err := retryUntilDeadline(ctx, func(context.Context) error {
		attempts++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected last error surfaced, got %v", err)
	}
	if attempts < 2 {
		t.Fatalf("expected at least one retry before deadline, got %d attempts", attempts)
	}
}

// With NO deadline on ctx, retryUntilDeadline runs single-shot — a permanent
// failure returns immediately instead of looping forever (the direct unit-call
// / boot-equivalent path).
func TestRetryUntilDeadlineSingleShotWithoutDeadline(t *testing.T) {
	attempts := 0
	err := retryUntilDeadline(context.Background(), func(context.Context) error {
		attempts++
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if attempts != 1 {
		t.Fatalf("expected single-shot (1 attempt) without a deadline, got %d", attempts)
	}
}

// The heartbeat stage retries under the marker deadline: a heartbeat that fails
// the first attempt then succeeds yields an OK ack (the bug ka-0000001492 fix on
// the gb side). The watcher passes a deadline-bounded ctx; we emulate that here.
func TestGBThawHandlerHeartbeatRetriesUnderDeadline(t *testing.T) {
	beats := 0
	h := newGBThawHandler(thawDeps{
		heartbeat: func(context.Context) error {
			beats++
			if beats < 2 {
				return errors.New("Temporary failure in name resolution")
			}
			return nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ack, err := h(ctx, thawepoch.Marker{Epoch: 1})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !ack.OK {
		t.Fatalf("expected OK ack after transient heartbeat recovered, got %+v", ack)
	}
	if ack.Stages["heartbeat"] != "ok" {
		t.Fatalf("expected heartbeat ok, got %q", ack.Stages["heartbeat"])
	}
	if beats != 2 {
		t.Fatalf("expected 2 heartbeat attempts (1 transient + 1 ok), got %d", beats)
	}
}

// TestGBThawHandlerWarmsVIPRouteBeforeHeartbeat is the handler-half RED→GREEN
// proof for the cold-VIP-route fix (bug ka-0000002195). It models the thaw the
// canary hit: the cross-namespace beads ClusterIP VIP route is COLD until
// something drives a connect through it (kube-proxy seeds the DNAT/conntrack on
// the first successful connect). The heartbeat POST alone keeps black-holing on
// the cold route; only the warm-route probe — which we retry under the deadline —
// programs/seeds the route, after which the heartbeat lands.
//
// RED-BAR: revert the N4.6 warm_vip_route stage (so deps.warmRoute is never
// invoked) and `routeWarm` stays false, so the heartbeat black-holes to the
// deadline and the ack fails closed — this test goes RED. It is NOT tautological:
// the heartbeat's success is causally gated on the warm probe having run first.
func TestGBThawHandlerWarmsVIPRouteBeforeHeartbeat(t *testing.T) {
	// Shared cold-route state. The handler runs warm-then-heartbeat sequentially
	// in one goroutine (retryUntilDeadline is synchronous), so a plain bool +
	// slice are race-free here.
	var routeWarm bool
	var order []string
	warmAttempts := 0

	h := newGBThawHandler(thawDeps{
		warmRoute: func(context.Context) error {
			warmAttempts++
			order = append(order, "warm")
			// First probe hits the cold VIP (black-hole); the second lands and
			// seeds conntrack — modeling kube-proxy finishing DNAT programming.
			if warmAttempts < 2 {
				return errors.New("connect: no route to host (cold cross-ns VIP)")
			}
			routeWarm = true
			return nil
		},
		heartbeat: func(context.Context) error {
			order = append(order, "beat")
			if !routeWarm {
				// Cold VIP route: the POST to gardenio-beads.gardenio.svc:8080
				// black-holes ("context deadline exceeded" in prod).
				return errors.New("Post http://gardenio-beads.gardenio.svc.cluster.local:8080/v1/hooks/emit: context deadline exceeded")
			}
			return nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ack, err := h(ctx, thawepoch.Marker{Epoch: 1})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !ack.OK {
		t.Fatalf("expected OK ack once the VIP route was warmed before the heartbeat, got %+v", ack)
	}
	if ack.Stages["warm_vip_route"] != "ok" {
		t.Fatalf("expected warm_vip_route ok, got %q", ack.Stages["warm_vip_route"])
	}
	if ack.Stages["heartbeat"] != "ok" {
		t.Fatalf("expected heartbeat ok, got %q", ack.Stages["heartbeat"])
	}
	if warmAttempts != 2 {
		t.Fatalf("expected the warm probe to retry through the cold VIP (2 attempts), got %d", warmAttempts)
	}
	// Ordering is load-bearing: every warm probe must precede the first heartbeat.
	if len(order) == 0 || order[len(order)-1] != "beat" {
		t.Fatalf("heartbeat must be the LAST step (warm precedes it), got order %v", order)
	}
	for i, ev := range order {
		if ev == "beat" {
			if i != len(order)-1 {
				t.Fatalf("warm must fully precede the heartbeat, got order %v", order)
			}
			break
		}
	}
}

// TestGBThawHandlerWarmRouteFailureIsNonFatal proves the warm-up is BEST-EFFORT,
// not a required ack-gate: a warm probe that never lands records
// warm_vip_route=failed but must NOT fail-close the thaw — the heartbeat remains
// the only required gate (so we never regress a thaw the heartbeat could carry on
// its own retry). Here the route happens to be warm for the heartbeat regardless.
func TestGBThawHandlerWarmRouteFailureIsNonFatal(t *testing.T) {
	h := newGBThawHandler(thawDeps{
		warmRoute: func(context.Context) error {
			return errors.New("connect: no route to host (route never programmed)")
		},
		heartbeat: func(context.Context) error { return nil },
	})
	// Short deadline so the doomed warm probe exhausts its (halved) budget fast.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	ack, err := h(ctx, thawepoch.Marker{Epoch: 1})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !ack.OK {
		t.Fatalf("a failed BEST-EFFORT warm-up must NOT fail-close the thaw, got %+v", ack)
	}
	if ack.Stages["warm_vip_route"] != "failed" {
		t.Fatalf("expected warm_vip_route failed (best-effort), got %q", ack.Stages["warm_vip_route"])
	}
	if ack.Stages["heartbeat"] != "ok" {
		t.Fatalf("expected heartbeat ok, got %q", ack.Stages["heartbeat"])
	}
}

// TestGBThawHandlerNoWarmRouteHookSkipsStage confirms a nil warmRoute (test wiring
// without a live beadsapi client, or a future non-gVisor caller) cleanly skips the
// stage: no warm_vip_route key is recorded and the required heartbeat still gates.
func TestGBThawHandlerNoWarmRouteHookSkipsStage(t *testing.T) {
	h := newGBThawHandler(thawDeps{
		heartbeat: func(context.Context) error { return nil },
		// warmRoute intentionally nil
	})
	ack, err := h(context.Background(), thawepoch.Marker{Epoch: 1})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !ack.OK {
		t.Fatalf("expected OK ack, got %+v", ack)
	}
	if _, present := ack.Stages["warm_vip_route"]; present {
		t.Fatalf("nil warmRoute must skip the stage entirely, got %q", ack.Stages["warm_vip_route"])
	}
	if ack.Stages["heartbeat"] != "ok" {
		t.Fatalf("expected heartbeat ok, got %q", ack.Stages["heartbeat"])
	}
}

// TestRouteWarmContextHalvesDeadline pins the budget split: the warm-up gets a
// fraction of the remaining rehydrate budget so the heartbeat (the required gate)
// keeps the majority. A deadline-less parent passes through unchanged.
func TestRouteWarmContextHalvesDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wctx, wcancel := routeWarmContext(parent)
	defer wcancel()
	pdl, _ := parent.Deadline()
	wdl, ok := wctx.Deadline()
	if !ok {
		t.Fatal("warm context must carry a deadline when parent has one")
	}
	if !wdl.Before(pdl) {
		t.Fatalf("warm deadline %v must be earlier than parent %v (heartbeat keeps the rest)", wdl, pdl)
	}

	// No parent deadline → passthrough (single-shot semantics downstream).
	wctx2, wcancel2 := routeWarmContext(context.Background())
	defer wcancel2()
	if _, ok := wctx2.Deadline(); ok {
		t.Fatal("warm context must not invent a deadline when parent has none")
	}
}
