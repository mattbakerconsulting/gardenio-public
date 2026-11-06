//go:build e2e

// trigger_changed_to_seam_livedaemon_e2e_test.go — LIVE-DAEMON e2e for the
// declarative bead-trigger `changed_to` / `field_changed` operators across the
// FOUR-PROCESS daemon→SSE→bridge seam (bead ka-0000002238, parent
// ka-0000000958).
//
// ── The gap this closes ───────────────────────────────────────────────────────
//
// The diff-aware trigger operators `field_changed` and `changed_to`
// (trigger_source_bead.go:634/636) had ZERO end-to-end coverage: a HEAD grep for
// `changed_to` / `field_changed` returned 0 hits across BOTH e2e suites
// (tests/e2e/, tests/beads-viewer-e2e/) and 0 across every `*_e2e_test.go`. They
// consume the per-field `changes` overlay that the daemon produces in
// `perFieldChanges` (kbeads beads.go:1055, published at :1241) and serializes
// over SSE (events.go:80 `changes` tag) — a seam that spans four processes and
// that nothing exercised as a whole.
//
// ── Why a LIVE-DAEMON go:build e2e test, not the nearest existing tests ───────
//
// Every test that touches this operator today starts DOWNSTREAM of the wire, so
// none of them would red if the seam broke:
//
//	(a) kbeads/internal/server/beads_field_changes_test.go:72 calls
//	    `perFieldChanges` as a PURE FUNCTION — never HTTP, never publishes, never
//	    serializes the map onto an SSE frame.
//	(b) trigger_liveness_invariants_t1_test.go (the B3 test) hand-stuffs
//	    `src.pending` with `Changes: map[string]any{"enabled": false}` directly
//	    into the in-memory source. It STARTS downstream of the entire wire: if
//	    kbeads stopped emitting `enabled` in `changes`, the JSON tag drifted, or
//	    sse.go:398 dropped the field, that test STILL PASSES.
//	(c) kbeads sse_integration_test.go:262 reads `changes` off a real SSE stream
//	    but asserts `changes["title"]` — a first-class COLUMN populated by a
//	    different code branch (beads.go:983), NOT by `perFieldChanges`. Classic
//	    different-guard-same-name.
//	(d) The trigger-probe (`--source=bead`) injects a synthetic BeadEvent via
//	    InjectBeadEvent — with NO Changes map at all — so the shell
//	    test-trigger-action-fidelity.sh path structurally cannot reach these ops,
//	    and bolting a `--bead-changes` flag onto it would only recreate trap (b):
//	    a hand-authored map injected past the wire.
//
// So this test drives the REAL bridge.SSEStream (the exact
// NewSSEStream+RegisterSSE wiring prod's trigger-engine uses, main.go:141-142)
// connected to the LIVE daemon, then mutates a watched bead's custom field via
// the REAL beadsapi.Client. That forces the whole seam for real:
//
//	kd/beadsapi UpdateBeadFieldsTyped(enabled=false)
//	  → daemon perFieldChanges (beads.go:1055) → published overlay (beads.go:1241)
//	  → SSE frame `changes` (events.go:80)      → bridge sse.go:398 decode
//	  → BeadStateTriggerSource.pending          → Evaluate → matchOneCondition
//	                                               (changed_to / field_changed)
//
// Evaluate (not the fleet-wide EvaluateSource) is driven against a TriggerDef we
// build in-process, so the test never authors a live `type:trigger` bead that
// the shared shard engine could fire, and keys every assertion on the ONE
// watched bead id it seeded — hermetic on a shared daemon.
//
// ── The production failure mode this pins ─────────────────────────────────────
//
// A janitor gardener is disabled (`kd update <cap> -f enabled=false`). An
// operator's watchdog trigger uses `--condition enabled:changed_to:false`. If a
// kbeads refactor drops the fieldChanges overlay, renames the JSON tag, or
// sse.go:398 stops forwarding `Changes`, `changeMapLookup` returns `!ok` and the
// watchdog SILENTLY never fires — the gardener goes dark and nobody is paged,
// while every pure-function / hand-stuffed test above stays green. This test is
// the only thing that reds on that regression.
//
// Assertions (each on the seeded bead only):
//	1. changed_to     enabled:false on the real enabled=true→false transition FIRES exactly once.
//	2. anti-vacuity   a note-only update to the SAME bead stays SILENT (the `changes`
//	                  map lacks `enabled`, so changed_to must not match) — proving
//	                  assertion 1 tested a transition, not "fires on any update".
//	3. field_changed  enabled (unary) FIRES on the same transition and stays SILENT
//	                  on the note-only update — the sibling operator over the same map.
//	4. wrong-value    enabled:changed_to:true does NOT fire on a →false transition —
//	                  the value comparand is load-bearing, not decorative.
//
// Mutation-verified while authoring (see the shell wrapper header): reverting the
// kbeads fieldChanges overlay (beads.go:1241-1249) so the daemon emits only the
// opaque "fields" key reds assertions 1 and 3 (the `changes` map no longer
// carries `enabled`, so changeMapLookup/changeMapHas return !ok).
//
// It is `//go:build e2e`-tagged (invisible to `go build ./...`, `go test ./...`,
// and golangci-lint — none pass `-tags e2e`) so it never inflates the default
// build/lint/test surface, mirroring thread_task_autoclose_livedaemon_e2e_test.go.
// It runs ONLY when the trigger-changed-to-seam module invokes it via
// `go test -tags e2e -run TestTriggerChangedToFieldChangedSeamLiveDaemonE2E`, and
// SKIPs LOUD (t.Skip) when BEADS_HTTP_URL/token are unset or the daemon is
// unreachable — a missing daemon must never be read as a pass.

package bridge

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// changedToProbeThread namespaces this test's synthetic beads. The e2eOperator
// const + liveE2EClient / firstNonEmpty helpers are shared with the sibling
// thread_task_autoclose_livedaemon_e2e_test.go in this same package.
const changedToWatchLabel = "e2e:trigger-changed-to-seam"

// seedLiveWatchedTrigger creates a REAL open type=trigger bead on the live
// daemon carrying `enabled=true` so a subsequent enabled=false PATCH produces a
// genuine per-field transition in the daemon's `changes` overlay. The bead
// carries the fixture:e2e: label so the shared shard trigger-engine's
// LoadTriggers skips it (isE2EFixture) and it can never fire in prod. Returns
// its id; registers cleanup that closes it.
//
// type=trigger is deliberate: `enabled` is a first-class boolean field on the
// type:trigger schema (config.go), so both the create and the enabled=false
// PATCH validate cleanly, and it is the EXACT shape the production failure mode
// names — a gardener/trigger bead being disabled.
func seedLiveWatchedTrigger(t *testing.T, client *beadsapi.Client, tag string) string {
	t.Helper()
	ctx := beadsapi.WithOperator(context.Background(), e2eOperator)
	// A minimal-but-valid trigger bead: action is required by the type:trigger
	// schema. enabled=true is the pre-state the transition diffs against.
	fields := []byte(`{"enabled":true,"action":"notify","source":"bead-state"}`)
	id, err := client.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Type:      "trigger",
		Title:     "e2e changed_to watched trigger " + tag,
		CreatedBy: "e2e-trigger-changed-to-seam",
		// fixture:e2e: makes isE2EFixture(bead)=true → the shared shard engine
		// never fires it. project:e2e-test keeps it out of the gardenio lane.
		Labels: []string{"fixture:e2e:changed-to-seam", changedToWatchLabel, "e2e:true", "project:e2e-test"},
		Fields: fields,
	})
	if err != nil {
		t.Fatalf("seed live watched trigger: %v", err)
	}
	if id == "" {
		t.Fatal("seed live watched trigger: empty id returned")
	}
	t.Cleanup(func() {
		_ = client.CloseBead(beadsapi.WithOperator(context.Background(), e2eOperator), id, map[string]string{})
	})
	return id
}

// liveBeadSourceOverSSE wires a REAL BeadStateTriggerSource to a REAL SSEStream
// connected to the live daemon — the exact NewSSEStream+RegisterSSE pairing
// prod's trigger-engine builds (cmd/trigger-engine/main.go:141-142), subscribing
// to the same bead + edge topics. It then BLOCKS until the stream is confirmed
// live by round-tripping a FRESHLY-minted sentinel (see awaitStreamLive), so a
// mutation issued after this returns cannot race ahead of the subscription.
// Returns the source and a stop func.
func liveBeadSourceOverSSE(t *testing.T, client *beadsapi.Client) (*BeadStateTriggerSource, func()) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	addr := firstNonEmpty(os.Getenv("BEADS_HTTP_URL"), os.Getenv("BEADS_HTTP_ADDR"))
	token := firstNonEmpty(os.Getenv("BEADS_AUTH_TOKEN"), os.Getenv("BEADS_TOKEN"), os.Getenv("BEADS_DAEMON_TOKEN"))

	src := NewBeadStateTriggerSource(logger)
	stream := NewSSEStream(SSEStreamConfig{
		BeadsHTTPAddr: addr,
		Topics:        append([]string{"beads.bead.created", "beads.bead.updated", "beads.bead.closed"}, EdgeEventTopics()...),
		Token:         token,
		Logger:        logger,
	})
	src.RegisterSSE(stream)

	streamCtx, cancel := context.WithCancel(context.Background())
	go func() { _ = stream.Start(streamCtx) }()

	// Readiness gate. The SSE endpoint delivers ONLY events broadcast AFTER the
	// server-side subscribe — a fresh connection with no Last-Event-ID gets no
	// replay (kbeads sse.go handleEventStream). So we cannot assert on a mutation
	// until the stream is provably live.
	if !awaitStreamLive(t, client, src, 60*time.Second) {
		cancel()
		t.Fatal("SSE stream never went live: no freshly-minted readiness sentinel round-tripped within 60s — stream not live, cannot assert the seam")
	}
	// Clear the sentinel(s) (and any ambient events) so each leg starts from a
	// clean buffer and keys only on its own seeded bead.
	src.DrainPending()

	return src, cancel
}

// awaitStreamLive blocks until the SSE-backed source is provably live, returning
// true on success or false if budget expires first.
//
// It proves liveness by round-tripping a sentinel bead — create it, then poll the
// source's pending buffer until its `created` event lands. The load-bearing detail
// is that it mints a FRESH sentinel each round rather than betting on a single one:
//
// A single sentinel created immediately after `go stream.Start()` RACES the
// subscription. The beadsapi client already holds a WARM keep-alive connection
// (liveE2EClient's reachability GetBead probe primed it), so its CreateBead POST
// can broadcast the sentinel's `created` event BEFORE the COLD SSE GET (a fresh
// http.Client doing DNS+TCP+GET+auth) has subscribed server-side. With no
// Last-Event-ID replay that event is then lost forever, and polling the SAME
// sentinel longer never recovers it — it is gone. That is exactly the
// deterministic, retry-proof 20s timeout that reddened the 2026.218.17 release
// gate on a loaded e2e shard, where the warm POST reliably wins the race
// (gd-onNyOnedn4). The original genproto cold-download was a red herring: the
// cached retry still timed out because the single sentinel was still lost to the
// same race.
//
// Re-minting fixes it structurally: the moment the subscription is established, the
// NEXT sentinel created is delivered and caught, so this converges as soon as the
// stream comes up — independent of how long the cold connect (or a cold module
// download) took. Registers cleanup for every sentinel it creates.
func awaitStreamLive(t *testing.T, client *beadsapi.Client, src *BeadStateTriggerSource, budget time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(budget)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		sentinelCtx := beadsapi.WithOperator(context.Background(), e2eOperator)
		sentinelID, err := client.CreateBead(sentinelCtx, beadsapi.CreateBeadRequest{
			Type:      "task",
			Title:     "e2e changed_to SSE readiness sentinel",
			CreatedBy: "e2e-trigger-changed-to-seam",
			Labels:    []string{"fixture:e2e:changed-to-seam-sentinel", "e2e:true", "project:e2e-test"},
		})
		if err != nil {
			// A transient create error must not abort readiness — retry within budget.
			t.Logf("readiness sentinel create failed (attempt %d): %v — retrying", attempt, err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		id := sentinelID
		t.Cleanup(func() {
			_ = client.CloseBead(beadsapi.WithOperator(context.Background(), e2eOperator), id, map[string]string{})
		})
		// Short per-attempt wait: once the stream is live this returns near-instantly;
		// if this sentinel raced the pre-subscription window, the next round mints a
		// fresh one that will land after the subscription is up. Cap the wait to the
		// remaining budget so a straggler cannot overshoot the deadline.
		perAttempt := 3 * time.Second
		if rem := time.Until(deadline); perAttempt > rem {
			perAttempt = rem
		}
		if perAttempt > 0 && waitForPendingBead(src, sentinelID, perAttempt) {
			return true
		}
	}
	return false
}

// waitForPendingBead polls the source's pending buffer until an event for beadID
// appears (or timeout). It never drains — callers decide when to clear.
func waitForPendingBead(src *BeadStateTriggerSource, beadID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pendingHasBead(src, beadID) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return pendingHasBead(src, beadID)
}

// pendingHasBead reports whether the source's pending buffer currently holds any
// event for beadID. Reads src.pending under the source's own mutex.
func pendingHasBead(src *BeadStateTriggerSource, beadID string) bool {
	src.mu.Lock()
	defer src.mu.Unlock()
	for _, e := range src.pending {
		if e.Bead != nil && e.Bead.ID == beadID {
			return true
		}
	}
	return false
}

// evaluateForBead runs the source's REAL Evaluate over the given trigger def and
// returns only the matches whose dedup key belongs to beadID — so ambient prod
// events buffered on the shared stream can never be miscounted as our fire.
func evaluateForBead(t *testing.T, src *BeadStateTriggerSource, def TriggerDef, beadID string) []TriggerMatch {
	t.Helper()
	all, err := src.Evaluate(context.Background(), def)
	if err != nil {
		t.Fatalf("Evaluate(%s): %v", def.ID, err)
	}
	var mine []TriggerMatch
	for _, m := range all {
		// beadEventToMatch keys as "<bead-id>:<event>[:<changes-fp>]"; a prefix
		// match on "<bead-id>:" isolates our bead's matches.
		if len(m.Key) >= len(beadID)+1 && m.Key[:len(beadID)+1] == beadID+":" {
			mine = append(mine, m)
		}
	}
	return mine
}

// beadTriggerDef builds a TriggerDef whose SourceData is a beadTriggerData with
// the given event types + conditions — the exact struct LoadTriggers produces
// from a `gb trigger create bead` authored bead, so Evaluate sees byte-identical
// input to production without persisting a fireable trigger bead.
func beadTriggerDef(id string, eventTypes []string, conds []beadCondition) TriggerDef {
	return TriggerDef{
		ID:     id,
		Source: "bead",
		Action: TriggerActionNotify,
		SourceData: beadTriggerData{
			EventTypes: eventTypes,
			Conditions: conds,
		},
	}
}

// TestTriggerChangedToFieldChangedSeamLiveDaemonE2E drives the changed_to /
// field_changed operators across the REAL daemon→SSE→bridge seam. It shares one
// live SSE-backed source across the legs (each leg mutates its OWN seeded bead
// and keys only on that id), so the readiness handshake runs once.
func TestTriggerChangedToFieldChangedSeamLiveDaemonE2E(t *testing.T) {
	client := liveE2EClient(t)
	src, stop := liveBeadSourceOverSSE(t, client)
	defer stop()

	updCtx := beadsapi.WithOperator(context.Background(), e2eOperator)

	// ── 1. changed_to fires on the REAL enabled=true→false transition ─────────
	// This is the headline: the daemon perFieldChanges overlay + SSE `changes`
	// frame + sse.go:398 decode + changeMapLookup all have to work for this to
	// fire. Any break in that chain reds here (and ONLY here among all the
	// downstream-of-wire tests above).
	t.Run("changed-to-fires-on-transition", func(t *testing.T) {
		id := seedLiveWatchedTrigger(t, client, "L1")
		src.DrainPending()

		if err := client.UpdateBeadFieldsTyped(updCtx, id, map[string]any{"enabled": false}); err != nil {
			t.Fatalf("PATCH enabled=false on %s: %v", id, err)
		}
		if !waitForPendingBead(src, id, 20*time.Second) {
			t.Fatalf("no beads.bead.updated event for %s reached the SSE-backed source within 20s — the daemon→SSE→bridge seam did not deliver the transition", id)
		}

		def := beadTriggerDef("gd-e2e-changed-to-false", []string{"updated"}, []beadCondition{
			{Field: "id", Op: "==", Value: id},
			{Field: "enabled", Op: "changed_to", Value: "false"},
		})
		matches := evaluateForBead(t, src, def, id)
		if len(matches) != 1 {
			t.Fatalf("enabled:changed_to:false must fire EXACTLY once on the true→false transition, got %d matches (key set proves the `changes` map carried enabled=false end-to-end)", len(matches))
		}
	})

	// ── 2. anti-vacuity: a note-only update stays SILENT ──────────────────────
	// Proves leg 1 asserted a TRANSITION, not "fires on any update". A
	// notes/title update produces a beads.bead.updated whose `changes` map does
	// NOT carry `enabled`, so changed_to must not match. Without this, a
	// regression that made changed_to fire on every update (e.g. treating a
	// missing key as a match) would pass leg 1 and go undetected.
	t.Run("changed-to-silent-on-unrelated-update", func(t *testing.T) {
		id := seedLiveWatchedTrigger(t, client, "L2")
		src.DrainPending()

		note := "e2e unrelated update " + id
		if err := client.UpdateBead(updCtx, id, beadsapi.UpdateBeadRequest{Notes: &note}); err != nil {
			t.Fatalf("PATCH notes on %s: %v", id, err)
		}
		if !waitForPendingBead(src, id, 20*time.Second) {
			t.Fatalf("no beads.bead.updated event for the note-only update of %s within 20s — cannot prove SILENCE without first proving the update was delivered", id)
		}

		def := beadTriggerDef("gd-e2e-changed-to-false-neg", []string{"updated"}, []beadCondition{
			{Field: "id", Op: "==", Value: id},
			{Field: "enabled", Op: "changed_to", Value: "false"},
		})
		matches := evaluateForBead(t, src, def, id)
		if len(matches) != 0 {
			t.Fatalf("enabled:changed_to:false must stay SILENT on a note-only update (enabled did NOT transition), got %d matches — changed_to is matching on something other than a real field transition", len(matches))
		}
	})

	// ── 3. field_changed (unary) — the sibling operator over the same map ─────
	// Fires on the enabled transition, silent on a note-only update. Guards the
	// changeMapHas branch (trigger_source_bead.go:635) the same way legs 1-2
	// guard changeMapLookup.
	t.Run("field-changed-fires-on-transition-silent-otherwise", func(t *testing.T) {
		id := seedLiveWatchedTrigger(t, client, "L3")
		src.DrainPending()

		// Positive: enabled transition.
		if err := client.UpdateBeadFieldsTyped(updCtx, id, map[string]any{"enabled": false}); err != nil {
			t.Fatalf("PATCH enabled=false on %s: %v", id, err)
		}
		if !waitForPendingBead(src, id, 20*time.Second) {
			t.Fatalf("no updated event for the enabled transition of %s within 20s", id)
		}
		def := beadTriggerDef("gd-e2e-field-changed", []string{"updated"}, []beadCondition{
			{Field: "id", Op: "==", Value: id},
			{Field: "enabled", Op: "field_changed"},
		})
		if got := evaluateForBead(t, src, def, id); len(got) != 1 {
			t.Fatalf("enabled:field_changed must FIRE once on the transition, got %d", len(got))
		}

		// Negative: note-only update, fresh buffer. The bead now has enabled=false
		// already, so a notes PATCH leaves enabled untouched → changes lacks it.
		src.DrainPending()
		note := "e2e field_changed unrelated update " + id
		if err := client.UpdateBead(updCtx, id, beadsapi.UpdateBeadRequest{Notes: &note}); err != nil {
			t.Fatalf("PATCH notes on %s: %v", id, err)
		}
		if !waitForPendingBead(src, id, 20*time.Second) {
			t.Fatalf("no updated event for the note-only update of %s within 20s", id)
		}
		if got := evaluateForBead(t, src, def, id); len(got) != 0 {
			t.Fatalf("enabled:field_changed must stay SILENT on a note-only update, got %d matches — field_changed is not keyed on the `changes` map", len(got))
		}
	})

	// ── 4. wrong-value: the comparand is load-bearing ─────────────────────────
	// enabled:changed_to:true must NOT fire on a →false transition. Without this,
	// changed_to could ignore Value entirely (degenerate to field_changed) and
	// every leg above would still pass.
	t.Run("changed-to-wrong-value-does-not-fire", func(t *testing.T) {
		id := seedLiveWatchedTrigger(t, client, "L4")
		src.DrainPending()

		if err := client.UpdateBeadFieldsTyped(updCtx, id, map[string]any{"enabled": false}); err != nil {
			t.Fatalf("PATCH enabled=false on %s: %v", id, err)
		}
		if !waitForPendingBead(src, id, 20*time.Second) {
			t.Fatalf("no updated event for the enabled transition of %s within 20s", id)
		}

		// Same transition, but the trigger watches for changed_to:TRUE.
		def := beadTriggerDef("gd-e2e-changed-to-true", []string{"updated"}, []beadCondition{
			{Field: "id", Op: "==", Value: id},
			{Field: "enabled", Op: "changed_to", Value: "true"},
		})
		if got := evaluateForBead(t, src, def, id); len(got) != 0 {
			t.Fatalf("enabled:changed_to:true must NOT fire on a true→false transition, got %d matches — changed_to is ignoring the value comparand", len(got))
		}

		// Control: the correct value DOES fire on the same buffered event, so the
		// zero above is a real value-mismatch, not the event having been missed.
		defOK := beadTriggerDef("gd-e2e-changed-to-false-ctrl", []string{"updated"}, []beadCondition{
			{Field: "id", Op: "==", Value: id},
			{Field: "enabled", Op: "changed_to", Value: "false"},
		})
		if got := evaluateForBead(t, src, defOK, id); len(got) != 1 {
			t.Fatalf("control: enabled:changed_to:false MUST fire on this same event (got %d) — otherwise the wrong-value zero is a false negative from a missed event, not a value mismatch", len(got))
		}
	})
}
