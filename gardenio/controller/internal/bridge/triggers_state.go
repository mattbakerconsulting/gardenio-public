package bridge

import (
	"context"
	"encoding/json"
	"maps"
	"sort"
	"strings"
	"time"
)

// pruneStaleTriggers removes seen entries for trigger IDs that are no longer
// active (the trigger bead was closed or deleted). This prevents unbounded
// memory growth from accumulated dedup state for defunct triggers.
func (e *TriggerEngine) pruneStaleTriggers(activeTriggers map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	pruned := 0
	prunedKeys := 0
	for id, keys := range e.seen {
		if !activeTriggers[id] {
			prunedKeys += len(keys)
			delete(e.seen, id)
			delete(e.seenAt, id)
			pruned++
		}
	}
	// seenAt can hold a trigger seen never did (defensive) — drop those too.
	for id := range e.seenAt {
		if !activeTriggers[id] {
			delete(e.seenAt, id)
		}
	}
	for id := range e.orphanRefireAt {
		if !activeTriggers[id] {
			delete(e.orphanRefireAt, id)
		}
	}
	// Keep the out-of-band last_checked detector state (ka-0000006237) from
	// growing unbounded alongside seen.
	for id := range e.lastObserved {
		if !activeTriggers[id] {
			delete(e.lastObserved, id)
		}
	}
	for id := range e.selfStamped {
		if !activeTriggers[id] {
			delete(e.selfStamped, id)
		}
	}
	for id := range e.checkStamps {
		if !activeTriggers[id] {
			delete(e.checkStamps, id)
		}
	}
	// Same for the starvation detector — and drop its published gauge series
	// so a closed trigger cannot page forever off a stale starved=1 sample.
	for id, st := range e.starve {
		if !activeTriggers[id] {
			triggerCooldownSkipStreak.DeleteLabelValues(st.source, id)
			triggerStarvedGauge.DeleteLabelValues(st.source, id)
			delete(e.starve, id)
		}
	}
	if pruned > 0 {
		e.logger.Info("triggers: pruned stale seen entries",
			"triggers_removed", pruned, "keys_removed", prunedKeys)
	}
}

// gcSeenKeys ages out INNER dedup keys from the still-active triggers' seen
// sets (ka-0000003049). pruneStaleTriggers only reclaims WHOLE triggers that no
// longer load; a long-lived trigger's inner map[dedupKey]bool would otherwise
// grow forever — one permanent entry per distinct event a broad bead-state
// trigger ever observed (for `updated` events the key folds in a changes
// fingerprint, so effectively one entry per fleet-wide bead field-change).
//
// The sweep is reconcile-based: seenAt is a parallel trigger→key→first-observed
// map that the sweep itself maintains, so the ~10 sites that write seen need no
// instrumentation. Each sweep:
//   - stamps seenAt[trig][key] = now for any key present in seen but not yet in
//     seenAt (its first observation by the GC),
//   - drops any key whose first-observed time is older than triggerSeenKeyTTL
//     from BOTH seen and seenAt,
//   - drops seenAt entries whose key is gone from seen (a wider dedup refresh or
//     external replacement of the inner map),
//   - as a hard safety cap, evicts the oldest-observed keys of any trigger whose
//     inner set still exceeds triggerSeenKeyMaxPerTrigger.
//
// Eviction is safe because the daemon-side `state` persists dedup durably: a key
// evicted here and re-observed later is re-checked by evalOne's pre-fire
// CatchUpKeys refresh before any action fires, so the worst case is a redundant
// daemon round-trip, never a duplicate fire. Throttled to triggerSeenGCInterval
// so the O(total keys) walk does not run on every ~30s Evaluate() cycle.
func (e *TriggerEngine) gcSeenKeys() {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.lastSeenGC.IsZero() && now.Sub(e.lastSeenGC) < triggerSeenGCInterval {
		return
	}
	e.lastSeenGC = now

	evicted := 0
	for id, keys := range e.seen {
		at := e.seenAt[id]
		if at == nil {
			at = make(map[string]time.Time, len(keys))
			e.seenAt[id] = at
		}
		// Stamp first-observation for any newly-present key.
		for k := range keys {
			if _, ok := at[k]; !ok {
				at[k] = now
			}
		}
		// TTL eviction: drop keys observed longer ago than the horizon, and any
		// seenAt entry whose key no longer lives in seen.
		for k, t := range at {
			if _, live := keys[k]; !live {
				delete(at, k)
				continue
			}
			if now.Sub(t) > triggerSeenKeyTTL {
				delete(keys, k)
				delete(at, k)
				evicted++
			}
		}
		// Hard per-trigger cap: if still oversized, evict oldest-observed first.
		if len(keys) > triggerSeenKeyMaxPerTrigger {
			evicted += e.evictOldestSeenLocked(keys, at, len(keys)-triggerSeenKeyMaxPerTrigger)
		}
		// Reclaim empty inner maps so a burst-then-idle trigger frees memory.
		if len(keys) == 0 {
			delete(e.seen, id)
			delete(e.seenAt, id)
		}
	}
	if evicted > 0 {
		triggers, keysTotal := len(e.seen), 0
		for _, m := range e.seen {
			keysTotal += len(m)
		}
		e.logger.Info("triggers: gc'd stale seen keys",
			"keys_evicted", evicted, "triggers", triggers, "keys_remaining", keysTotal)
	}
}

// evictOldestSeenLocked drops the n oldest-observed keys from a single
// trigger's inner seen set (and its seenAt shadow), returning how many it
// removed. Callers must hold e.mu. Used only as the hard per-trigger safety cap
// in gcSeenKeys when a firehose fills the set faster than the TTL evicts it;
// re-firing an evicted key later is caught by the pre-fire CatchUpKeys refresh.
func (e *TriggerEngine) evictOldestSeenLocked(keys map[string]bool, at map[string]time.Time, n int) int {
	if n <= 0 {
		return 0
	}
	type kt struct {
		key string
		t   time.Time
	}
	entries := make([]kt, 0, len(at))
	for k, t := range at {
		entries = append(entries, kt{k, t})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].t.Before(entries[j].t) })
	if n > len(entries) {
		n = len(entries)
	}
	for i := 0; i < n; i++ {
		delete(keys, entries[i].key)
		delete(at, entries[i].key)
	}
	return n
}

// SeenMapSize returns the total number of keys across all triggers in the
// seen map. Useful for observability and memory diagnostics.
func (e *TriggerEngine) SeenMapSize() (triggers, keys int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	triggers = len(e.seen)
	for _, m := range e.seen {
		keys += len(m)
	}
	return triggers, keys
}

// warnUnbackedGardenerProse logs a loud, warn-once message when a trigger's
// prompt claims a run_budget/terminal_goal but nothing machine-readable backs
// it (no gardener block, no conditions pre-flight). Such a trigger LOOKS gated
// yet loops unbounded because the engine never reads budget/goal prose. See
// bug ka-0000006735 and advice ka-0000000974.
func (e *TriggerEngine) warnUnbackedGardenerProse(trig TriggerDef) {
	markers, unbacked := UnbackedGardenerProse(trig.Prompt, trig.Gardener != nil, trig.HasConditions)
	if !unbacked {
		return
	}
	e.mu.Lock()
	already := e.warnedGate[trig.ID]
	if !already {
		e.warnedGate[trig.ID] = true
	}
	e.mu.Unlock()
	if already {
		return
	}
	e.logger.Warn("triggers: unbacked gardener prose — budget/goal claimed but no machine gate",
		"trigger", trig.ID, "title", trig.Title, "markers", strings.Join(markers, ","),
		"detail", "no gardener block and no conditions pre-flight; engine never enforces the claimed budget/goal — loop is effectively unbounded")
}

// warnRepolessRepoPaths logs a loud, warn-once message when a project-less
// (repo-less) spawn_agent trigger's prompt names a git path the pod cannot
// resolve at runtime — the repo-less pod clones no working tree, so the path
// 404s (bug ka-0000004796, task ka-0000004167). Warn-only, and suppressed when
// the prompt already acknowledges the repo-less runtime (the corrected shape
// names a path only to tell the agent NOT to open it). New triggers are also
// warned at authoring time by the CLI guard (cmd/gb/{trigger,gardener}.go). The
// warn is deduped under a distinct key namespace so it composes with the other
// per-trigger warns.
func (e *TriggerEngine) warnRepolessRepoPaths(trig TriggerDef) {
	paths, unreachable := UnreachableRepoPaths(trig.Prompt, trig.Project, string(trig.Action))
	if !unreachable {
		return
	}
	key := "repoless-gitpath:" + trig.ID
	e.mu.Lock()
	already := e.warnedGate[key]
	if !already {
		e.warnedGate[key] = true
	}
	e.mu.Unlock()
	if already {
		return
	}
	e.logger.Warn("triggers: repo-less trigger names git path(s) that 404 at runtime",
		"trigger", trig.ID, "title", trig.Title, "paths", strings.Join(paths, ","),
		"detail", "project-less trigger spawns a repo-less pod (no working tree); inline the content into an advice body / bundle read via gb, or give the trigger a project (bug ka-0000004796)")
}

// warnUnboundedGardener implements the firing-time warn-with-grace half of the
// global bounded-stop guard (ka-0000006744). A gardener with a `gardener` block
// MUST carry a positive run_budget AND a bounded hard_stop wall_clock (advice
// ka-0000000974). New gardeners are rejected at AUTHORING time (the CLI guard in
// cmd/gb/{gardener,trigger}.go fails closed), but a live gardener bead that
// predates this guard (or was hand-edited / mirrored) must NOT be silently
// disabled mid-flight — fleet-wide auto-disable keyed on a check is exactly the
// blast-radius the operator declined in decision ka-0000003439. So at firing
// time we WARN (once per trigger) and let the gardener fire; an operator can
// re-author it with bounds, or engage the `gardeners-pause` kill-switch to stop
// the fleet. The warn is deduped under a distinct key namespace from the
// unbacked-prose warn so both can fire for the same trigger.
func (e *TriggerEngine) warnUnboundedGardener(trig TriggerDef) {
	if trig.Gardener == nil {
		return
	}
	v := ValidateGardenerSpec(trig.Gardener)
	if v == nil {
		return
	}
	key := "unbounded:" + trig.ID
	e.mu.Lock()
	already := e.warnedGate[key]
	if !already {
		e.warnedGate[key] = true
	}
	e.mu.Unlock()
	if already {
		return
	}
	e.logger.Warn("triggers: gardener is UNBOUNDED — firing anyway (warn-with-grace)",
		"trigger", trig.ID, "title", trig.Title, "violation", v.Error(),
		"detail", "gardener lacks a positive run_budget and/or a bounded hard_stop wall_clock (advice ka-0000000974); "+
			"re-author with bounds — new gardeners are rejected at authoring time, and `gardeners-pause` halts the fleet")
}

// warnStandingProseWithoutMarker implements the firing-time warn-with-grace
// half of the standing-marker guard (bug ka-0000004311). A gardener whose
// terminal_goal DECLARES it perpetual ("STANDING groomer gardener: … does NOT
// self-complete") while carrying NO machine-readable standing marker
// (standing:true / perpetual:true) is silently treated as epic-bounded: the
// max_runs ceiling trips and the resume give-up path REAPS it, producing a
// closed-but-enabled dark gardener. New gardeners are rejected at AUTHORING time
// (the CLI guard in cmd/gb/{gardener,trigger}.go fails closed), but a live
// gardener bead that predates this guard must NOT be silently disabled or
// auto-modified mid-flight (the blast-radius the operator declined in decision
// ka-0000003439). So at firing time we WARN (once per trigger) and let it fire;
// an operator adds the marker (or rephrases the goal if it is NOT perpetual).
// The warn is deduped under a distinct key namespace so it composes with the
// unbacked-prose and unbounded warns.
func (e *TriggerEngine) warnStandingProseWithoutMarker(trig TriggerDef) {
	v := ValidateGardenerStandingMarker(trig.Gardener, trig.Labels)
	if v == nil {
		return
	}
	key := "standing-marker:" + trig.ID
	e.mu.Lock()
	already := e.warnedGate[key]
	if !already {
		e.warnedGate[key] = true
	}
	e.mu.Unlock()
	if already {
		return
	}
	e.logger.Warn("triggers: gardener declares STANDING in terminal_goal but carries NO standing marker — firing anyway (warn-with-grace)",
		"trigger", trig.ID, "title", trig.Title, "matched_prose", v.MatchedProse,
		"detail", "engine reads standing-ness from machine markers (standing:true / perpetual:true label), NEVER from terminal_goal prose (bug ka-0000004311); "+
			"without a marker the max_runs ceiling trips (ka-0000004754) and the resume give-up path reaps it (ka-0000006241) — add the marker or rephrase the goal if it is NOT perpetual")
}

// stampLastChecked writes last_checked=now on the trigger bead and records
// the stamp time so the out-of-band advance detector can tell this engine's
// own writes apart from a foreign evaluator's. All engine-side last_checked
// writes go through here (ka-0000006237 Layer 1b).
func (e *TriggerEngine) stampLastChecked(ctx context.Context, triggerID string) error {
	return e.stampCheckedOutcome(ctx, triggerID, "")
}

// stampCheckedOutcome writes last_checked=now AND, when outcome is non-empty,
// last_outcome/last_outcome_at in the SAME field patch — the diagnosability
// seam of bug ka-0000001513 folded onto the once-per-cycle liveness stamp.
//
// Folding matters: last_checked already changes every evaluation, so this patch
// already fires every cycle regardless of the outcome. Carrying last_outcome in
// the same write adds ZERO extra daemon round-trips and ZERO extra SSE events
// over the pre-existing last_checked churn, while giving `kd show <gardener>` a
// standing answer to "why didn't it fire this cycle?" — a question that used to
// require grepping controller logs the operator cannot reach. `fired` is
// recorded as a positive outcome too, so a gardener that DID run reads a fresh
// `last_outcome=fired: spawned N` rather than a stale decline from an earlier
// dark window. Every evalOne exit that stamps last_checked routes through here.
func (e *TriggerEngine) stampCheckedOutcome(ctx context.Context, triggerID, outcome string) error {
	now := time.Now().UTC()
	e.mu.Lock()
	prev := e.checkStamps[triggerID]
	skip := stampThrottled(prev, outcome, now)
	if skip {
		prev.throttledAt = now
		e.checkStamps[triggerID] = prev
	}
	if !skip {
		e.selfStamped[triggerID] = now
	}
	e.mu.Unlock()
	if skip {
		return nil
	}
	fields := map[string]string{
		"last_checked": now.Format(time.RFC3339),
	}
	if outcome != "" {
		fields["last_outcome"] = outcome
		fields["last_outcome_at"] = now.Format(time.RFC3339)
	}
	if err := e.daemon.UpdateBeadFields(ctx, triggerID, fields); err != nil {
		return err
	}
	e.mu.Lock()
	e.checkStamps[triggerID] = checkStamp{wroteAt: now, outcome: outcome}
	e.mu.Unlock()
	return nil
}

// checkStampMinInterval bounds how often a steady-state evaluation re-writes
// last_checked on a trigger bead. It must stay far below every liveness
// threshold that reads last_checked (gardener liveness floor 15m, check
// liveness floor 30m, stalled-open heal floor 1h) so a throttled stamp can
// never read as a dark trigger. Triggers whose cooldown is >= this interval
// are never throttled: each evaluation already lands past the interval.
const checkStampMinInterval = 5 * time.Minute

// checkStamp is the per-trigger last_checked throttle state.
type checkStamp struct {
	throttledAt time.Time // latest stamp skipped since wroteAt; zero if none
	wroteAt     time.Time // last successful bead write
	outcome     string    // outcome carried by that write
}

// stampThrottled reports whether this stamp may skip the bead write: the
// outcome is the one already on the bead and the bead was written less than
// checkStampMinInterval ago. Fires, outcome changes and outcome-less stamps
// (the bootstrap seed, which must advance last_checked off zero) always write.
func stampThrottled(prev checkStamp, outcome string, now time.Time) bool {
	if outcome == "" || strings.HasPrefix(outcome, "fired") {
		return false
	}
	if prev.wroteAt.IsZero() || prev.outcome != outcome {
		return false
	}
	return now.Sub(prev.wroteAt) < checkStampMinInterval
}

// lastEvaluated returns the later of the bead's last_checked and the time of
// the latest stamp this engine throttled. The cooldown gate uses it so a
// skipped (unwritten) stamp still starts the cooldown window.
func (e *TriggerEngine) lastEvaluated(trig TriggerDef) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	if t := e.checkStamps[trig.ID].throttledAt; t.After(trig.LastChecked) {
		return t
	}
	return trig.LastChecked
}

// stampLastRun writes last_run (and, when a spawn produced one, last_agent_id)
// on a NON-gardener spawn-producing trigger after a successful fire. Gardener
// triggers get the richer gardenerPostFire write instead (blob counters +
// mirrored top-level fields); before this helper existed gardenerPostFire was
// the ONLY writer of last_run, so plain spawn_agent / apply_formula triggers
// read as permanently frozen even while firing on cadence (the FLAT stall,
// ka-0000001622). For schedule-shaped sources last_run also feeds the source's
// already-fired suppression (trigger_source_schedule.go Evaluate), giving
// pours restart continuity that the in-memory seen set alone cannot.
// Best-effort: a failed write logs and moves on — last_checked (stamped every
// cycle) remains the canonical liveness signal.
// matchKey, when non-empty, is persisted as last_match in the same write so a
// schedule-sourced trigger's catch-up can skip any match <= last_match across
// a restart (gd-pLf5LSHDEh) without depending on the in-memory seen set.
func (e *TriggerEngine) stampLastRun(ctx context.Context, trig TriggerDef, agentID, matchKey string) {
	fields := map[string]string{
		"last_run": time.Now().UTC().Format(time.RFC3339),
	}
	if agentID != "" {
		fields["last_agent_id"] = agentID
	}
	if matchKey != "" {
		fields["last_match"] = matchKey
	}
	if err := e.daemon.UpdateBeadFields(ctx, trig.ID, fields); err != nil {
		e.logger.Warn("triggers: failed to stamp last_run",
			"trigger", trig.ID, "error", err)
	}
}

// maxFireErrorLen bounds the persisted last_fire_error string. A fireAction
// error can carry a large HTTP body or a wrapped chain; cap it so the trigger
// bead field (and the SSE event it fires) stays small while still preserving
// the operator-diagnostic head of the message.
const maxFireErrorLen = 500

// stampFireError persists a fireAction failure onto the trigger bead so a
// gardener (or any trigger) that MATCHES EVERY CYCLE AND FAILS is visible in the
// data plane instead of byte-indistinguishable from an idle/gated one (bug
// ka-0000002991). It records two things:
//
//   - last_fire_error / last_fire_error_at — the truncated error string and
//     when it happened. `gb gardener status` surfaces these prominently and the
//     next SUCCESSFUL fire clears them (gardenerPostFire / stampLastRun), so a
//     stale error can never masquerade as a live failure.
//   - consecutive_failures — INCREMENTED for gardeners. This is the sharper half
//     of the bug: gardenerAgentTracker only advances the failure streak when it
//     observes a CLOSED agent, but a fire that errors spawns NO agent, so the
//     streak stayed pinned at 0 and a hard_stop.consecutive_failures self-block
//     could never engage — a gardener could fail forever without ever tripping
//     its own brake. The counter is the GardenerSpec blob's authoritative value
//     (gardener_stop reads it via GardenerStopContext), so we mutate the blob and
//     mirror it to the integer-typed top-level field in one typed patch, the
//     same shape gardenerPostFire / gardenerAgentTracker use (ka-0000007976). For
//     a non-gardener trigger there is no stop-block consuming the counter, so we
//     stamp only the error fields.
//
// trig is taken by pointer so the blob mutation is visible to later gates in
// the same evalOne cycle (mirrors gardenerPostFire's in-place update).
func (e *TriggerEngine) stampFireError(ctx context.Context, trig *TriggerDef, fireErr error) {
	now := time.Now().UTC().Format(time.RFC3339)
	errStr := truncateText(fireErr.Error(), maxFireErrorLen)

	// Gardener: bump the authoritative blob counter and mirror it (typed) so
	// hard_stop.consecutive_failures actually engages.
	if trig.Gardener != nil {
		updated := *trig.Gardener
		updated.ConsecutiveFailures++
		raw, mErr := json.Marshal(updated)
		if mErr != nil {
			e.logger.Warn("triggers: fire-error gardener marshal failed",
				"trigger", trig.ID, "error", mErr)
			// Fall through to the plain error-field stamp below so the
			// diagnostic is not lost even if the blob write is skipped.
		} else {
			if err := e.daemon.UpdateBeadFieldsTyped(ctx, trig.ID, map[string]any{
				"gardener":             string(raw),
				"consecutive_failures": updated.ConsecutiveFailures,
				"last_fire_error":      errStr,
				"last_fire_error_at":   now,
			}); err != nil {
				e.logger.Warn("triggers: failed to stamp fire error (gardener)",
					"trigger", trig.ID, "error", err)
				return
			}
			// Mutate in place so later gates this cycle see the bumped streak.
			trig.Gardener.ConsecutiveFailures = updated.ConsecutiveFailures
			trig.LastFireError = errStr
			return
		}
	}

	// Non-gardener trigger (or gardener blob marshal failure): stamp only the
	// error fields — no stop-block consumes consecutive_failures here.
	if err := e.daemon.UpdateBeadFields(ctx, trig.ID, map[string]string{
		"last_fire_error":    errStr,
		"last_fire_error_at": now,
	}); err != nil {
		e.logger.Warn("triggers: failed to stamp fire error",
			"trigger", trig.ID, "error", err)
		return
	}
	trig.LastFireError = errStr
}

// clearFireError blanks last_fire_error / last_fire_error_at on the trigger bead
// after a successful fire so a resolved failure stops showing on `gb gardener
// status` (bug ka-0000002991). Best-effort: a failed clear logs and moves on —
// the stale error is a diagnostic annoyance, not a correctness hazard, and the
// next successful cycle retries the clear.
func (e *TriggerEngine) clearFireError(ctx context.Context, triggerID string) {
	if err := e.daemon.UpdateBeadFields(ctx, triggerID, map[string]string{
		"last_fire_error":    "",
		"last_fire_error_at": "",
	}); err != nil {
		e.logger.Warn("triggers: failed to clear fire error",
			"trigger", triggerID, "error", err)
	}
}

// noteForeignLastChecked records the trigger's currently-loaded last_checked
// and reports whether it advanced since the previous evaluation cycle WITHOUT
// this engine writing it — the signature of a foreign evaluator (e.g. a
// sandbox bridge misconfigured onto this daemon) racing this engine for the
// fire window and starving its cooldown gate (ka-0000006237, epic
// ka-0000005107). Returns the previously observed value for logging.
func (e *TriggerEngine) noteForeignLastChecked(trig TriggerDef) (foreign bool, prev time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	prev, observed := e.lastObserved[trig.ID]
	self := e.selfStamped[trig.ID]
	e.lastObserved[trig.ID] = trig.LastChecked
	if !observed || trig.LastChecked.IsZero() || !trig.LastChecked.After(prev) {
		return false, prev
	}
	// last_checked advanced since the previous cycle. Our own stamps are
	// written at second precision, so anything within a small tolerance of
	// the recorded self-stamp is our own write echoing back.
	if !self.IsZero() && trig.LastChecked.Sub(self) <= 2*time.Second && self.Sub(trig.LastChecked) <= 2*time.Second {
		return false, prev
	}
	return true, prev
}

// SeenKeys returns a copy of the seen set for a trigger (for testing).
func (e *TriggerEngine) SeenKeys(triggerID string) map[string]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	orig := e.seen[triggerID]
	if orig == nil {
		return nil
	}
	cp := make(map[string]bool, len(orig))
	maps.Copy(cp, orig)
	return cp
}

// SeedSeen adds a key to the seen set for a trigger (for testing).
func (e *TriggerEngine) SeedSeen(triggerID, key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.seen[triggerID] == nil {
		e.seen[triggerID] = make(map[string]bool)
	}
	e.seen[triggerID][key] = true
}

// IgnoreCooldown marks a trigger to bypass the per-trigger cooldown gate in
// evalOne (TEST-ONLY; mirrors SeedSeen). It exists for cmd/trigger-probe, a
// test-only binary that fires ONE injected event through a real EvaluateSource
// pass. On a SHARED daemon another live evaluator (the E2E namespace runs a
// trigger-engine polling every 30s) stamps last_checked on the freshly-created
// trigger:bead beads on every non-firing poll; the 30s cooldown floor then
// suppresses the probe's deliberate fire depending on race timing — the
// cross-env cooldown-starvation hazard the engine WARN-detects at ka-0000006237
// and the root cause of the test-trigger-action-fidelity flake (ka-0000007982).
// The production trigger-engine/bridge never calls this, so live cooldown
// throttling is unchanged.
func (e *TriggerEngine) IgnoreCooldown(triggerID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ignoreCooldown[triggerID] = true
}

// cooldownIgnored reports whether IgnoreCooldown was called for this trigger.
func (e *TriggerEngine) cooldownIgnored(triggerID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ignoreCooldown[triggerID]
}
