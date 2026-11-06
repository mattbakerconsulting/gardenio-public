// Package bridge — engine-owned SLA-by-priority nudge sweep for open
// escalation and decision beads (Phase 2 of the event-gardeners design,
// gd-RxGnZ8ZUav / gd-Jc3NHOcgDj).
//
// SLANudgeSweep periodically walks open escalations and open/in-progress
// decisions, computes each bead's age against its priority's SLA
// (sla_nudge_policy.go), and drives the 3-rung ladder from the spec:
//
//   - Rung 1 (reflag): bumps priority one step toward P0 and stamps the same
//     reflag_count/reflagged_at/reflagged_by fields `gb decision|escalation
//     reflag` stamps (runReflag, cmd/gb/decision.go). For escalations this
//     also stamps pingback_requested_at/pingback_count, which the EXISTING
//     Escalations SSE watcher (escalations.go maybeNotifyPingback) already
//     reacts to by reposting to the original Slack thread — no new Slack
//     code needed. For decisions, adding the "escalated" label at P0/P1
//     similarly triggers the EXISTING Decisions watcher's repost
//     (decisions.go handleUpdated).
//   - Rung 2 (repeat on the operator queue channel): for escalations, stamps
//     queue_alert_requested_at, reusing the EXISTING queue-alert delivery
//     path (escalations.go maybeNotifyQueueAlert -> NotifyEscalationQueueAlert)
//     but gated on SLA-elapsed instead of the fixed 48h
//     pingbackQueueThreshold. Decisions have no equivalent existing path, so
//     this sweep posts directly via SLANudgeNotifier.
//   - Rung 3 (overdue): stamps sla_breached=true, adds the "overdue" label
//     (queryable per spec §3/§8), and posts directly via SLANudgeNotifier for
//     both bead types.
//
// Dedupe (spec §5) is per-bead-per-rung, keyed on the SAME kind of
// stamp-and-compare-age fields the existing reflag/pingback mechanisms
// already use — no new "which rung did we last fire" tracking field. Snooze
// (spec §6) needs zero new code: the sweep's Statuses filter is
// ["open","in_progress"], which structurally excludes status=deferred
// (`kd defer`/`kd undefer`), and the age anchor (pingbackAnchor, shared with
// the pingback loop) is unaffected by a defer/undefer cycle. Quiet hours
// (spec §4) defer a due nudge to the next sweep after the window ends: a
// nudge found due during quiet hours is simply skipped this sweep, with no
// dedupe field stamped, so it remains due and fires on the first sweep once
// quiet hours end.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// defaultSLASweepInterval is how often the sweep re-evaluates open
	// escalations and decisions. P0's SLA is 1h with a rung-1 fire at ~15m,
	// so this needs to be materially tighter than the hours-scale pingback/
	// digest sweeps.
	defaultSLASweepInterval = 5 * time.Minute

	// slaSweepPageSize / slaSweepMaxPages bound the per-type exhaustion walk,
	// mirroring the pingback loop and staleness digest (ka-0000005723):
	// 10 x 200 = 2000 open beads of a given type, an order of magnitude above
	// observed populations.
	slaSweepPageSize = 200
	slaSweepMaxPages = 10

	// slaNudgeActor is the audit-trail actor recorded on every field this
	// sweep stamps, distinguishing an automated SLA nudge from a human
	// `gb reflag` or the exponential-backoff pingback loop.
	slaNudgeActor = "system:sla-nudge"

	// slaBasePriorityField anchors the SLA lookup to the bead's priority as
	// first observed by the sweep, stamped once when rung 1 fires (before the
	// same fire bumps the live Priority). Without this, a sweep after rung 1
	// has bumped e.g. P1->P0 would re-read the live (now tighter) priority and
	// measure age against the wrong SLA window (merge-desk finding on PR #173:
	// a P1 escalation bumped to P0 at ~1h was falsely marked sla_breached at
	// ~1h05m against P0's 1h SLA, instead of its real P1 4h SLA).
	slaBasePriorityField = "sla_base_priority"
)

// slaSweptTypes are the bead types this sweep covers (spec §1: "for
// escalation and decision beads, symmetrically").
var slaSweptTypes = []string{"escalation", "decision"}

// SLANudgeSweepConfig configures an SLANudgeSweep.
type SLANudgeSweepConfig struct {
	Daemon BeadClient
	// Notifier delivers rung-2 (decision only) and rung-3 (both types) Slack
	// alerts. Optional: a nil Notifier still drives every field/label/
	// priority side effect and the metrics, it just skips the direct Slack
	// post (escalation rung 2 is unaffected either way — it reuses the
	// existing queue-alert SSE-watcher path, not this notifier).
	Notifier SLANudgeNotifier
	Logger   *slog.Logger
	Interval time.Duration // ≤ 0 → defaultSLASweepInterval
	Policy   SLAPolicy     // zero value → DefaultSLAPolicy()
}

// SLANudgeSweep drives the SLA-by-priority nudge ladder.
type SLANudgeSweep struct {
	daemon   BeadClient
	notifier SLANudgeNotifier
	logger   *slog.Logger
	interval time.Duration
	policy   SLAPolicy

	// now is injectable for tests; defaults to time.Now.
	now func() time.Time
}

// NewSLANudgeSweep constructs the sweep. A nil Daemon disables it (returns
// nil), mirroring the digest loop's nil-safety contract — callers `go
// sweep.Run(ctx)` unconditionally.
func NewSLANudgeSweep(cfg SLANudgeSweepConfig) *SLANudgeSweep {
	if cfg.Daemon == nil {
		return nil
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultSLASweepInterval
	}
	policy := cfg.Policy
	if policy.SLAByPriority == nil {
		policy = DefaultSLAPolicy()
	}
	return &SLANudgeSweep{
		daemon:   cfg.Daemon,
		notifier: cfg.Notifier,
		logger:   logger,
		interval: interval,
		policy:   policy,
		now:      time.Now,
	}
}

// Run sweeps immediately, then on every tick until ctx is canceled. Nil-safe.
func (s *SLANudgeSweep) Run(ctx context.Context) {
	if s == nil {
		return
	}
	s.logger.Info("sla nudge sweep starting", "interval", s.interval)
	s.Sweep(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("sla nudge sweep stopping")
			return
		case <-ticker.C:
			s.Sweep(ctx)
		}
	}
}

// SLASweepResult summarizes one Sweep call, for tests and startup logging.
type SLASweepResult struct {
	NudgesSent   int
	OverdueCount map[string]int // bead_type -> current overdue count
}

// Sweep walks every open escalation and open/in-progress decision once,
// firing due nudges and refreshing the sla_overdue_count gauge. Exported (not
// sweepOnce) so tests can assert on the returned summary without scraping
// Prometheus.
func (s *SLANudgeSweep) Sweep(ctx context.Context) SLASweepResult {
	now := s.now()
	result := SLASweepResult{OverdueCount: map[string]int{}}
	for _, beadType := range slaSweptTypes {
		result.OverdueCount[beadType] = 0
		s.sweepType(ctx, beadType, now, &result)
	}
	for beadType, count := range result.OverdueCount {
		slaOverdueCount.WithLabelValues(beadType).Set(float64(count))
	}
	return result
}

func (s *SLANudgeSweep) sweepType(ctx context.Context, beadType string, now time.Time, result *SLASweepResult) {
	for page := 0; page < slaSweepMaxPages; page++ {
		res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{beadType},
			Statuses: []string{"open", "in_progress"},
			Sort:     "created_at",
			Limit:    slaSweepPageSize,
			Offset:   page * slaSweepPageSize,
		})
		if err != nil {
			s.logger.Warn("sla nudge sweep: list failed", "type", beadType, "error", err, "page", page)
			return
		}
		for _, b := range res.Beads {
			s.processBead(ctx, b, now, result)
		}
		if len(res.Beads) < slaSweepPageSize {
			return
		}
		if page == slaSweepMaxPages-1 {
			s.logger.Warn("sla nudge sweep: page cap reached, tail not evaluated this sweep",
				"type", beadType, "max_pages", slaSweepMaxPages, "page_size", slaSweepPageSize)
		}
	}
}

// processBead evaluates one bead against the ladder and, if a rung is due
// and not deduped/quiet-houred away, fires it.
func (s *SLANudgeSweep) processBead(ctx context.Context, b *beadsapi.BeadDetail, now time.Time, result *SLASweepResult) {
	fields := b.Fields
	if fields == nil {
		fields = map[string]string{}
	}

	if b.Type == "escalation" {
		// Approved escalations are moving through execution; review-flagged
		// ones already carry their one actionable pingback (mirrors
		// EscalationStalenessDigestLoop.evaluateBead). E2E fixtures must
		// never post to real channels.
		if fields["approved_by"] != "" {
			return
		}
		if fields[mrLandedField] == "true" {
			return
		}
		if isE2EEscalation(b.CreatedBy, b.Labels) {
			return
		}
	}

	anchor := pingbackAnchor(fields, b.CreatedAt)
	if anchor.IsZero() {
		return
	}
	age := now.Sub(anchor)

	// slaPriority is the priority the SLA lookup is anchored to: the bead's
	// priority as first seen, NOT the live b.Priority, which rung 1 may have
	// already bumped on a prior sweep (see slaBasePriorityField).
	slaPriority := b.Priority
	if raw := fields[slaBasePriorityField]; raw != "" {
		if p, err := strconv.Atoi(raw); err == nil {
			slaPriority = p
		}
	}

	rung := s.policy.RungForAge(slaPriority, age)
	if rung == SLARungNone {
		return
	}
	if rung == SLARung3 {
		result.OverdueCount[b.Type]++
	}

	if !s.rungDue(b.Type, fields, rung, slaPriority, now) {
		return
	}

	if quiet, err := s.policy.IsQuietHours(now); err != nil {
		s.logger.Warn("sla nudge sweep: quiet-hours check failed, proceeding", "id", b.ID, "error", err)
	} else if quiet {
		// Deferred to the window's end, not dropped (spec §4): no dedupe
		// field is stamped, so this bead is still "due" and the first sweep
		// after quiet hours end will fire it.
		return
	}

	if s.fireRung(ctx, b, fields, rung, slaPriority, age, now) {
		result.NudgesSent++
	}
}

// rungDue applies the per-bead-per-rung dedupe (spec §5: "one nudge per bead
// per rung"). Rung 1 is a one-shot, like rung 3: it fires only if this bead
// has never been reflagged, and never re-fires once reflagged_at is stamped.
// Rung 2 keeps the recurrence-window shape (dedupe stamp empty or older than
// the rung's own SLA fraction), since unlike rung 1 (a single priority bump)
// it is a repeat-post, not a one-time state transition.
func (s *SLANudgeSweep) rungDue(beadType string, fields map[string]string, rung SLARung, priority int, now time.Time) bool {
	sla := s.policy.SLAByPriority[priority]
	switch rung {
	case SLARung1:
		// Previously this compared reflagged_at's age against the rung-1 SLA
		// fraction, which made rung 1 re-fire every Rung1Fraction*SLA
		// interval after its first fire (merge-desk finding on PR #173: a P2
		// bead fired at 6h AND again at 12h, double-bumping priority) instead
		// of firing exactly once per bead.
		return fields["reflagged_at"] == ""
	case SLARung2:
		var field string
		if beadType == "decision" {
			field = "decision_pingback_requested_at"
		} else {
			// Escalation rung 2 dedupes on its OWN field (queue_alert_requested_at),
			// distinct from rung 1's pingback_requested_at, so a recent rung-1
			// fire never masks a rung-2 fire (they'd otherwise share a field and
			// rung 1's fresher timestamp would make rung 2 look "not due yet").
			field = "queue_alert_requested_at"
		}
		return dueByStamp(fields[field], now, scaleDuration(sla, s.policy.Rung2Fraction))
	case SLARung3:
		return fields["sla_breached"] != "true"
	default:
		return false
	}
}

// dueByStamp reports whether an RFC3339 dedupe stamp is stale enough (or
// absent) for the rung to fire again.
func dueByStamp(raw string, now time.Time, minAge time.Duration) bool {
	if raw == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return true
	}
	return now.Sub(t) >= minAge
}

// fireRung applies the side effects for rung and reports whether a nudge was
// actually sent (vs. a best-effort daemon/Slack failure that should retry
// next sweep without being counted).
func (s *SLANudgeSweep) fireRung(ctx context.Context, b *beadsapi.BeadDetail, fields map[string]string, rung SLARung, slaPriority int, age time.Duration, now time.Time) bool {
	nowStr := now.UTC().Format(time.RFC3339)
	switch rung {
	case SLARung1:
		return s.fireRung1(ctx, b, fields, slaPriority, nowStr)
	case SLARung2:
		return s.fireRung2(ctx, b, fields, age, nowStr)
	case SLARung3:
		return s.fireRung3(ctx, b, age, nowStr)
	default:
		return false
	}
}

// fireRung1 bumps priority one step toward P0 and stamps reflag metadata,
// exactly mirroring runReflag (cmd/gb/decision.go) with actor=slaNudgeActor.
// slaPriority is the SLA-anchored priority (see slaBasePriorityField) — since
// rung 1 is now one-shot (rungDue), this is always still the live b.Priority
// at the moment of this call, but it is stamped explicitly so later sweeps,
// which read the anchor field rather than the live (post-bump) Priority,
// keep measuring age against the SLA this bead actually started on.
func (s *SLANudgeSweep) fireRung1(ctx context.Context, b *beadsapi.BeadDetail, fields map[string]string, slaPriority int, nowStr string) bool {
	oldPriority := b.Priority
	newPriority := oldPriority - 1
	if newPriority < 0 {
		newPriority = 0
	}

	reason := fmt.Sprintf("sla nudge: rung 1 (%.0f%% of P%d SLA elapsed)", s.policy.Rung1Fraction*100, oldPriority)
	patch := map[string]string{
		"reflag_count":  incrementCountField(fields["reflag_count"]),
		"reflagged_at":  nowStr,
		"reflagged_by":  slaNudgeActor,
		"reflag_reason": reason,
	}
	if fields[slaBasePriorityField] == "" {
		patch[slaBasePriorityField] = strconv.Itoa(slaPriority)
	}
	if b.Type == "escalation" {
		patch["pingback_requested_at"] = nowStr
		patch["pingback_count"] = incrementCountField(fields["pingback_count"])
		patch["pingback_by"] = slaNudgeActor
		patch["pingback_source"] = "reflag"
		patch["pingback_reason"] = reason
	}

	if newPriority != oldPriority {
		if err := s.daemon.UpdateBead(ctx, b.ID, beadsapi.UpdateBeadRequest{Priority: &newPriority}); err != nil {
			s.logger.Warn("sla nudge: failed to bump priority", "id", b.ID, "error", err)
			return false
		}
	}
	if err := s.daemon.UpdateBeadFields(ctx, b.ID, patch); err != nil {
		s.logger.Warn("sla nudge: failed to stamp rung1 fields", "id", b.ID, "error", err)
		return false
	}
	if newPriority <= 1 {
		if err := s.daemon.AddLabel(ctx, b.ID, "escalated"); err != nil {
			s.logger.Warn("sla nudge: failed to add escalated label", "id", b.ID, "error", err)
		}
	}
	auditMsg := fmt.Sprintf("SLA nudge rung 1: P%d -> P%d (%s)", oldPriority, newPriority, reason)
	if err := s.daemon.AddComment(ctx, b.ID, slaNudgeActor, auditMsg); err != nil {
		s.logger.Warn("sla nudge: failed to record audit comment", "id", b.ID, "error", err)
	}

	slaNudgesSentTotal.WithLabelValues(b.Type, SLARung1.String()).Inc()
	return true
}

// fireRung2 delivers "repeat on the operator queue channel". Escalations
// reuse the existing queue-alert stamp-then-SSE-react path; decisions have no
// equivalent so this posts directly via the notifier.
func (s *SLANudgeSweep) fireRung2(ctx context.Context, b *beadsapi.BeadDetail, fields map[string]string, age time.Duration, nowStr string) bool {
	if b.Type == "escalation" {
		if err := s.daemon.UpdateBeadFields(ctx, b.ID, map[string]string{"queue_alert_requested_at": nowStr}); err != nil {
			s.logger.Warn("sla nudge: failed to stamp rung2 queue alert", "id", b.ID, "error", err)
			return false
		}
		slaNudgesSentTotal.WithLabelValues(b.Type, SLARung2.String()).Inc()
		return true
	}

	if s.notifier != nil {
		if err := s.notifier.NotifySLANudge(ctx, toSLABeadEvent(b), SLARung2, age); err != nil {
			s.logger.Warn("sla nudge: rung2 notify failed, will retry next sweep", "id", b.ID, "error", err)
			return false
		}
	}
	patch := map[string]string{
		"decision_pingback_requested_at": nowStr,
		"decision_pingback_count":        incrementCountField(fields["decision_pingback_count"]),
		"decision_pingback_source":       "sla-nudge",
	}
	if err := s.daemon.UpdateBeadFields(ctx, b.ID, patch); err != nil {
		s.logger.Warn("sla nudge: failed to stamp rung2 fields", "id", b.ID, "error", err)
		return false
	}
	slaNudgesSentTotal.WithLabelValues(b.Type, SLARung2.String()).Inc()
	return true
}

// fireRung3 marks a bead overdue: sla_breached=true, "overdue" label, and a
// one-shot direct notify for both bead types (the digest-loop generalization
// the spec sketches for rung 3 is explicitly out of scope for this change —
// see the design doc's documented gaps).
func (s *SLANudgeSweep) fireRung3(ctx context.Context, b *beadsapi.BeadDetail, age time.Duration, nowStr string) bool {
	_ = nowStr // no timestamp field needed beyond the boolean marker
	if s.notifier != nil {
		if err := s.notifier.NotifySLANudge(ctx, toSLABeadEvent(b), SLARung3, age); err != nil {
			s.logger.Warn("sla nudge: rung3 notify failed, will retry next sweep", "id", b.ID, "error", err)
			return false
		}
	}
	if err := s.daemon.UpdateBeadFieldsTyped(ctx, b.ID, map[string]any{"sla_breached": true}); err != nil {
		s.logger.Warn("sla nudge: failed to stamp sla_breached", "id", b.ID, "error", err)
		return false
	}
	if err := s.daemon.AddLabel(ctx, b.ID, "overdue"); err != nil {
		s.logger.Warn("sla nudge: failed to add overdue label", "id", b.ID, "error", err)
	}
	slaNudgesSentTotal.WithLabelValues(b.Type, SLARung3.String()).Inc()
	return true
}

// incrementCountField parses a stamped counter field (empty-safe) and
// returns the next value as a string, matching runReflag's fmt.Sscanf/++
// pattern.
func incrementCountField(raw string) string {
	n, _ := strconv.Atoi(raw)
	return strconv.Itoa(n + 1)
}

// toSLABeadEvent builds the minimal BeadEvent NotifySLANudge needs from a
// BeadDetail, mirroring cross_operator_notify.go's synth-event pattern.
func toSLABeadEvent(b *beadsapi.BeadDetail) BeadEvent {
	return BeadEvent{
		ID:        b.ID,
		Type:      b.Type,
		Title:     b.Title,
		Status:    b.Status,
		CreatedBy: b.CreatedBy,
		Labels:    b.Labels,
		Fields:    b.Fields,
		Priority:  b.Priority,
	}
}
