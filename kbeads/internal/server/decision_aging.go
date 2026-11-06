// Server-side decision-aging / auto-default policy.
//
// Operator decisions accrue without bound: a decision nobody answers stays
// open forever, gating whatever work it blocks. Grooming task ka-0000003434
// measured 32 open decisions aged >=14d (oldest ~24d), and that backlog is
// the sole gate on the entire gardenio bug queue (every open bug is blocked,
// overwhelmingly by these stale decisions). The queue cannot trend to zero
// by dispatching harder — the constraint is operator decision throughput.
//
// Two remedies were on the table: (a) auto-DEFAULT stale decisions to a
// pre-declared safe option, or (b) PING the operator about each stale one.
// A prior measurement (ka-0000003001, re-confirmed by the news+ready critic
// on ka-0000003434) showed that bulk-pinging N stale decisions posts N Slack
// notifications in one burst — manufacturing exactly the queue noise this is
// meant to remove. So this policy implements the auto-default half only, and
// makes it strictly OPT-IN per decision: the sweep touches a decision ONLY
// when its author pre-declared a default option (a `default_option` field, or
// an option carrying `"default": true`). A decision with no declared default
// is left completely untouched — no resolution, no ping. That keeps the
// operator in the loop for every genuinely-forking decision while draining
// the ones whose author already said "if nobody answers, this is the safe
// choice".
//
// When the sweep auto-defaults a decision it runs the SAME merge+close+cascade
// path as an operator resolve (handleResolveDecision): it records the chosen
// option, stamps `auto_defaulted: true` and `responded_by: system:decision-aging`
// for audit, publishes the BeadClosed event with a DecisionAnswer, and runs
// afterBeadClosed so the work the decision gated is parked, not silently freed.
//
// Ships DARK behind BEADS_DECISION_AGING_ENABLED (default OFF), mirroring the
// dedup-ack and authoring-human gates. With the flag off, the goroutine is
// never started and createBead/resolve behave exactly as before.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// decisionAgingActor is the responder recorded on an auto-defaulted decision.
// A distinct, non-human sentinel so an auto-default is never mistaken for an
// operator answer in the audit log or in responded_by-based attribution.
const decisionAgingActor = "system:decision-aging"

// decisionAgingDefaultDays is the threshold below which an open decision is
// left alone. 14 days per the grooming task's acceptance criteria
// (ka-0000003434): "auto-defaults ... decisions left >14d so the backlog stops
// accruing unbounded".
const decisionAgingDefaultDays = 14

// decisionAgingAdvisoryLockKey is the Postgres advisory-lock key serializing
// the decision-aging sweep across replicas (Phase 2 2-replica spike,
// gd-XxrObXvc8r). Both StartDecisionAging goroutines fire "once at startup"
// near-simultaneously on a rollout, and every 24h tick after carries the same
// overlap risk; without serialization two replicas can each list and
// auto-default the SAME stale decision, running merge→close→publish→cascade
// twice (duplicate TopicBeadClosed events, duplicate cascade side effects).
// An arbitrary but stable value distinct from auditRetentionAdvisoryLockKey —
// Postgres advisory locks share one 64-bit keyspace per database, so any two
// sweeps that must never contend for the same lock need different constants.
const decisionAgingAdvisoryLockKey int64 = 0x6265_6164_0001

// decisionAgingSweepCap bounds the open-decision working set fetched per sweep
// so a runaway backlog cannot materialize an unbounded slice in daemon heap
// (ka-0000003555). When the true open count exceeds the cap the sweep logs the
// truncation rather than silently covering only a prefix.
const decisionAgingSweepCap = 1000

// decisionAgingEnabled reports whether the aging sweep is active. Default OFF:
// the policy ships dark and is flipped per rollout once operators expect
// auto-defaulting. Mirrors dedupAckGateEnabled / authoringHumanGateEnabled.
func decisionAgingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BEADS_DECISION_AGING_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// decisionAgingDays returns the global staleness threshold in days. Reads
// BEADS_DECISION_AGING_DAYS, defaulting to 14 and flooring at 1 (a 0/negative
// value would auto-default freshly-created decisions the instant the sweep
// ran, which is never intended).
func decisionAgingDays() int {
	raw := strings.TrimSpace(os.Getenv("BEADS_DECISION_AGING_DAYS"))
	if raw == "" {
		return decisionAgingDefaultDays
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		slog.Warn("decision aging: invalid BEADS_DECISION_AGING_DAYS, using default",
			"value", raw, "default", decisionAgingDefaultDays)
		return decisionAgingDefaultDays
	}
	return n
}

// DecisionAgingEnabled is the exported gate check for the serve entrypoint,
// which lives in package main and cannot see the unexported helper.
func DecisionAgingEnabled() bool { return decisionAgingEnabled() }

// DecisionAgingDays is the exported threshold accessor for startup logging.
func DecisionAgingDays() int { return decisionAgingDays() }

// StartDecisionAging launches a background goroutine that periodically
// auto-defaults stale, opted-in decisions. The goroutine runs once per day and
// stops when ctx is cancelled. No-op when the policy is disabled — the caller
// gates on decisionAgingEnabled() before starting, but the goroutine also
// re-checks so a mid-run disable is honored on the next tick.
func (s *BeadsServer) StartDecisionAging(ctx context.Context) {
	go s.runDecisionAging(ctx, 24*time.Hour)
}

func (s *BeadsServer) runDecisionAging(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run once shortly after startup.
	s.sweepStaleDecisions(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepStaleDecisions(ctx)
		}
	}
}

// sweepStaleDecisions finds open decisions older than the global threshold and
// auto-defaults each one that declared a default option. Decisions without a
// declared default are skipped (no ping — see the file comment).
//
// Serialized across replicas by decisionAgingAdvisoryLockKey: only the
// replica that wins the advisory lock for this tick runs the sweep body
// (sweepStaleDecisionsLocked); a replica that loses the race skips the tick
// entirely rather than blocking — the next daily tick tries again.
func (s *BeadsServer) sweepStaleDecisions(ctx context.Context) {
	if !decisionAgingEnabled() {
		return
	}

	acquired, err := s.store.TryAdvisoryLock(ctx, decisionAgingAdvisoryLockKey, s.sweepStaleDecisionsLocked)
	if err != nil {
		slog.Warn("decision aging: advisory lock error", "error", err)
		return
	}
	if !acquired {
		slog.Info("decision aging: skipping tick, another replica holds the sweep lock")
	}
}

// sweepStaleDecisionsLocked is the sweep body, run only while
// decisionAgingAdvisoryLockKey is held by this replica. Errors are logged
// internally (best-effort sweep, retried next tick) rather than returned, so
// this always returns nil and the wrapping TryAdvisoryLock transaction always
// commits.
func (s *BeadsServer) sweepStaleDecisionsLocked(ctx context.Context) error {
	days := decisionAgingDays()
	now := time.Now().UTC()
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)

	filter := model.BeadFilter{
		Type:          []model.BeadType{model.TypeDecision},
		Status:        []model.Status{model.StatusOpen},
		UpdatedBefore: &cutoff,
		Limit:         decisionAgingSweepCap,
		Sort:          "updated_at",
	}

	beads, _, err := s.store.ListBeads(ctx, filter)
	if err != nil {
		slog.Warn("decision aging: failed to list stale decisions", "error", err)
		return nil
	}
	if len(beads) == 0 {
		return nil
	}
	if len(beads) == decisionAgingSweepCap {
		// Truncation-awareness (ka-0000004800): more stale decisions than the
		// cap exist; the remainder are handled on the next daily sweep.
		slog.Warn("decision aging: sweep hit the working-set cap; remainder deferred to next sweep",
			"cap", decisionAgingSweepCap)
	}

	defaulted, skipped := 0, 0
	for _, b := range beads {
		optID, ok := defaultOptionFor(b)
		if !ok {
			// No author-declared default → leave the decision entirely alone.
			// This is the deliberate no-ping design: only opted-in decisions
			// are drained; genuinely-forking ones keep waiting for a human.
			skipped++
			continue
		}
		// Honor a per-decision extension: a decision may ask to wait LONGER
		// than the global floor via `auto_default_after_days`. The global
		// cutoff above is the minimum; a larger per-decision value pushes the
		// effective threshold out. (A shorter value cannot pull it in below the
		// global floor — the query already filtered to >= globalDays old.)
		if override := agingOverrideDays(b); override > days {
			overrideCutoff := now.Add(-time.Duration(override) * 24 * time.Hour)
			if b.UpdatedAt.After(overrideCutoff) {
				skipped++
				continue
			}
		}
		if err := s.autoDefaultDecision(ctx, b, optID); err != nil {
			slog.Warn("decision aging: failed to auto-default decision",
				"id", b.ID, "option", optID, "error", err)
			continue
		}
		defaulted++
	}

	slog.Info("decision aging: sweep complete",
		"threshold_days", days,
		"considered", len(beads),
		"auto_defaulted", defaulted,
		"skipped_no_default", skipped,
		"cutoff", cutoff.Format(time.RFC3339),
	)
	return nil
}

// autoDefaultDecision resolves one stale decision by selecting its declared
// default option, mirroring handleResolveDecision's merge → close → publish →
// cascade so an auto-default is indistinguishable downstream from an operator
// answer (except for the auditable auto_defaulted / responded_by markers).
func (s *BeadsServer) autoDefaultDecision(ctx context.Context, bead *model.Bead, optionID string) error {
	respondedAt := time.Now().UTC().Format(time.RFC3339)
	extra := map[string]any{
		"chosen":         optionID,
		"responded_at":   respondedAt,
		"responded_by":   decisionAgingActor,
		"auto_defaulted": true,
	}
	if err := s.mergeBeadFields(ctx, bead.ID, extra); err != nil {
		// Non-fatal for the merge itself, but if we cannot record the answer we
		// must NOT close — a closed decision with no chosen option loses the
		// audit trail. Abort this one; the next sweep retries.
		return err
	}

	closed, err := s.store.CloseBead(ctx, bead.ID, decisionAgingActor)
	if err != nil {
		return err
	}

	s.recordAndPublish(ctx, events.TopicBeadClosed, closed.ID, decisionAgingActor, events.BeadClosed{
		Bead:     closed,
		ClosedBy: decisionAgingActor,
		DecisionAnswer: &events.DecisionAnswer{
			Chosen:      optionID,
			RespondedBy: decisionAgingActor,
			RespondedAt: respondedAt,
		},
	})

	// Run the same close cascade the resolve/cancel handlers run so the work
	// this decision gated is PARKED at blocked, not silently freed back into
	// the ready lane (ka-0000005620).
	s.afterBeadClosed(ctx, closed)

	slog.Info("decision aging: auto-defaulted stale decision",
		"id", bead.ID, "option", optionID, "responded_by", decisionAgingActor)
	return nil
}

// agingDecisionOption is the subset of a decision option this policy reads:
// the id and an optional `default` flag marking it as the auto-default choice.
type agingDecisionOption struct {
	ID      string `json:"id"`
	Default bool   `json:"default"`
}

// defaultOptionFor returns the id of the option a stale decision should
// auto-default to, and whether one is declared. Resolution order:
//
//  1. A top-level `default_option` field whose value matches one of the
//     decision's option ids (explicit, wins).
//  2. Otherwise, the first option carrying `"default": true`.
//
// Returns ("", false) when neither is present — the decision is NOT opted in
// and the sweep leaves it untouched. A `default_option` naming an id that is
// NOT among the options is treated as un-declared (false) and logged, so a
// typo never resolves the decision to a non-existent choice.
func defaultOptionFor(bead *model.Bead) (string, bool) {
	if bead == nil || len(bead.Fields) == 0 {
		return "", false
	}
	var f struct {
		Options       json.RawMessage `json:"options"`
		DefaultOption string          `json:"default_option"`
	}
	if err := json.Unmarshal(bead.Fields, &f); err != nil {
		return "", false
	}

	opts := parseAgingOptions(f.Options)
	if len(opts) == 0 {
		return "", false
	}
	valid := make(map[string]bool, len(opts))
	for _, o := range opts {
		if id := strings.TrimSpace(o.ID); id != "" {
			valid[id] = true
		}
	}

	// 1. explicit default_option field.
	if dv := strings.TrimSpace(f.DefaultOption); dv != "" {
		if valid[dv] {
			return dv, true
		}
		slog.Warn("decision aging: default_option names an unknown option id; ignoring",
			"id", bead.ID, "default_option", dv)
		return "", false
	}

	// 2. option flagged default:true.
	for _, o := range opts {
		if o.Default {
			if id := strings.TrimSpace(o.ID); id != "" {
				return id, true
			}
		}
	}
	return "", false
}

// parseAgingOptions decodes a decision's `options` field into id/default pairs,
// tolerating the two shapes the store may hold: a raw JSON array of objects, or
// a JSON STRING that itself contains that array (double-encoded — the shape
// extractDecisionFields passes through verbatim). A plain string-array of ids
// (["a","b"]) yields options with empty ids and no default, which correctly
// produces no auto-default.
func parseAgingOptions(raw json.RawMessage) []agingDecisionOption {
	if len(raw) == 0 {
		return nil
	}
	var opts []agingDecisionOption
	if err := json.Unmarshal(raw, &opts); err == nil {
		return opts
	}
	// Double-encoded: the field is a JSON string whose content is the array.
	var inner string
	if err := json.Unmarshal(raw, &inner); err != nil {
		return nil
	}
	if strings.TrimSpace(inner) == "" {
		return nil
	}
	_ = json.Unmarshal([]byte(inner), &opts)
	return opts
}

// agingOverrideDays returns a per-decision staleness threshold in days when the
// decision carries a positive integer `auto_default_after_days` field, else 0.
// Only used to EXTEND the global floor (see sweepStaleDecisions).
func agingOverrideDays(bead *model.Bead) int {
	if bead == nil || len(bead.Fields) == 0 {
		return 0
	}
	var f struct {
		AutoDefaultAfterDays *json.Number `json:"auto_default_after_days"`
	}
	if err := json.Unmarshal(bead.Fields, &f); err != nil || f.AutoDefaultAfterDays == nil {
		return 0
	}
	n, err := f.AutoDefaultAfterDays.Int64()
	if err != nil || n < 1 {
		return 0
	}
	return int(n)
}
