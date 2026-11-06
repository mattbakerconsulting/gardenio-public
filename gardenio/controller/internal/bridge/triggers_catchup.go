package bridge

import (
	"context"
	"errors"
	"fmt"
)

// bootstrapTrigger performs one-time catch-up for a trigger on its very first
// evaluation at runtime. It returns true when bootstrap ran (caller should
// return immediately without firing); false when no bootstrap was needed and
// normal evaluation should proceed.
//
// Behavior (mirrors catchUpSource):
//   - If prior beads exist for this trigger (CatchUpKeys non-empty), seed the
//     seen set from those keys and return false so the caller proceeds with
//     normal evaluation. This handles engine restarts for triggers that were
//     created before the previous shutdown.
//   - If no prior beads exist, evaluate the source once and seed ALL current
//     matches into the seen set WITHOUT firing actions, then return true so
//     the caller skips firing. This prevents a thundering herd for pre-existing
//     events when a brand-new trigger is created at runtime.
func (e *TriggerEngine) bootstrapTrigger(ctx context.Context, source TriggerSource, trig TriggerDef) bool {
	priorKeys, err := source.CatchUpKeys(ctx, e.daemon, trig.ID)
	if err != nil {
		e.logger.Warn("triggers: bootstrap catch-up-keys failed",
			"source", source.Name(), "trigger", trig.ID, "error", err)
		// Fall through: treat as "no prior beads" and bootstrap seeding below.
	}

	e.mu.Lock()
	if e.seen[trig.ID] == nil {
		e.seen[trig.ID] = make(map[string]bool)
	}
	e.mu.Unlock()

	if len(priorKeys) > 0 {
		e.mu.Lock()
		for _, k := range priorKeys {
			e.seen[trig.ID][k] = true
		}
		e.mu.Unlock()
		e.logger.Info("triggers: runtime catch-up from prior beads",
			"source", source.Name(), "trigger", trig.ID, "title", trig.Title,
			"seen", len(priorKeys))
		return false
	}

	matches, evalErr := source.Evaluate(ctx, trig)
	if evalErr != nil {
		e.logger.Error("triggers: bootstrap evaluation failed",
			"source", source.Name(), "trigger", trig.ID, "error", evalErr)
		// Stamp last_checked so we don't loop into bootstrap again on every tick.
		_ = e.stampLastChecked(ctx, trig.ID)
		return true
	}

	e.mu.Lock()
	for _, m := range matches {
		e.seen[trig.ID][m.Key] = true
	}
	e.mu.Unlock()

	e.logger.Info("triggers: runtime bootstrap — seeded existing matches without firing",
		"source", source.Name(), "trigger", trig.ID, "title", trig.Title,
		"seeded", len(matches))

	_ = e.stampLastChecked(ctx, trig.ID)
	return true
}

// CatchUp populates the seen sets for all triggers across all sources.
//
// Best-effort: a source whose load fails is logged and skipped. Callers that
// must know whether the catch-up actually landed — the startup gate in
// cmd/trigger-engine, which may not start evaluating with empty seen sets —
// use CatchUpAll or CatchUpWithRetry instead.
func (e *TriggerEngine) CatchUp(ctx context.Context) {
	_ = e.CatchUpAll(ctx)
}

// CatchUpAll populates the seen sets for all sources and reports whether every
// source loaded, joining the failures (gd-niQPoeR7ja).
//
// Only a LOAD failure — the daemon being unreachable or erroring — is reported.
// Per-trigger CatchUpKeys failures stay warn-and-continue exactly as before: a
// single malformed trigger must not be able to hold the whole engine
// permanently un-ready, whereas an unreachable daemon must.
//
// Re-running after a partial failure is safe and idempotent: seeding a seen set
// from the same keys twice is a no-op, and a trigger bootstrapped on the first
// pass has its last_checked stamped, so the second pass takes the normal path.
func (e *TriggerEngine) CatchUpAll(ctx context.Context) error {
	var errs []error
	for _, source := range e.sources {
		if err := e.catchUpSource(ctx, source); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", source.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// CatchUpWithRetry runs the startup catch-up, retrying with capped exponential
// backoff until every source loads or ctx is done (gd-niQPoeR7ja).
//
// This exists because the engine used to treat a failed catch-up as a shrug: it
// logged "catch-up load failed" per source and carried on with EMPTY seen sets.
// During the 2026-09-20 cluster-DNS outage every source failed, and the pod that
// came up behind it was one beads blip away from re-firing every trigger it had
// already handled. Worse, the load ran BEFORE the health listener was up, so the
// kubelet's probes failed against a process that was merely waiting on the
// daemon, SIGTERMed it, and the engine exited 0 into CrashLoopBackOff.
//
// The contract is now: keep trying, stay up, stay NOT ready, never exit. A
// non-nil return means ctx ended first (shutdown) — never "give up".
func (e *TriggerEngine) CatchUpWithRetry(ctx context.Context, cfg BackoffConfig) error {
	return RetryWithBackoff(ctx, e.logger, "triggers: startup catch-up", cfg, e.CatchUpAll)
}

// CatchUpSource populates the seen sets for a specific source.
func (e *TriggerEngine) CatchUpSource(ctx context.Context, sourceName string) {
	source, ok := e.sources[sourceName]
	if !ok {
		return
	}
	_ = e.catchUpSource(ctx, source)
}

// catchUpSource seeds one source's seen sets. It returns the source's load
// error (nil on success) so CatchUpAll can tell a reachable daemon from an
// unreachable one; per-trigger failures below remain logged and skipped.
func (e *TriggerEngine) catchUpSource(ctx context.Context, source TriggerSource) error {
	triggers, err := source.LoadTriggers(ctx, e.daemon)
	if err != nil {
		e.logger.Warn("triggers: catch-up load failed",
			"source", source.Name(), "error", err)
		return err
	}

	for _, trig := range triggers {
		keys, err := source.CatchUpKeys(ctx, e.daemon, trig.ID)
		if err != nil {
			e.logger.Warn("triggers: catch-up keys failed",
				"source", source.Name(), "trigger", trig.ID, "error", err)
			continue
		}

		// Bootstrap guard: when a trigger has never been evaluated (no
		// last_checked) and CatchUpKeys found no prior beads, this is a
		// brand-new trigger. Evaluate the source now and seed all current
		// matches into the seen set WITHOUT firing actions. This prevents a
		// thundering herd of agents/notifications for pre-existing events
		// (e.g. all currently-failed pipelines) when a trigger is first
		// created. Triggers that have been evaluated before will have
		// last_checked set from the previous evalOne call.
		if len(keys) == 0 && trig.LastChecked.IsZero() {
			matches, evalErr := source.Evaluate(ctx, trig)
			if evalErr == nil && len(matches) > 0 {
				e.mu.Lock()
				if e.seen[trig.ID] == nil {
					e.seen[trig.ID] = make(map[string]bool)
				}
				for _, m := range matches {
					e.seen[trig.ID][m.Key] = true
				}
				e.mu.Unlock()

				e.logger.Info("triggers: bootstrap — seeded existing matches without firing",
					"source", source.Name(), "trigger", trig.ID, "title", trig.Title,
					"seeded", len(matches))

				_ = e.stampLastChecked(ctx, trig.ID)
				continue
			}
		}

		e.mu.Lock()
		if e.seen[trig.ID] == nil {
			e.seen[trig.ID] = make(map[string]bool)
		}
		for _, key := range keys {
			e.seen[trig.ID][key] = true
		}
		e.mu.Unlock()

		e.logger.Info("triggers: catch-up complete",
			"source", source.Name(), "trigger", trig.ID, "seen", len(keys))
	}
	return nil
}

// CatchUpTrigger populates the seen set for a single trigger by ID.
// It looks up which source owns the trigger and delegates to that source.
func (e *TriggerEngine) CatchUpTrigger(ctx context.Context, triggerID string) {
	for _, source := range e.sources {
		keys, err := source.CatchUpKeys(ctx, e.daemon, triggerID)
		if err != nil {
			continue
		}
		if len(keys) == 0 {
			continue
		}

		e.mu.Lock()
		if e.seen[triggerID] == nil {
			e.seen[triggerID] = make(map[string]bool)
		}
		for _, key := range keys {
			e.seen[triggerID][key] = true
		}
		e.mu.Unlock()

		e.logger.Info("triggers: catch-up complete",
			"trigger", triggerID, "seen", len(keys))
		return
	}
}
