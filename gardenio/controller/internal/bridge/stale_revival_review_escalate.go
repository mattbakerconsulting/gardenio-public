// Engine-side age-based ESCALATION of stale revival-review decisions
// (bug ka-0000002082 fix 3, task ka-0000005280).
//
// THE GAP. trigger-reviver files `trigger-reviver-revival-review for <trigger>`
// decisions at whatever priority it chose (P2 in practice), and nothing bumps
// them afterward. Measured 2026-07-28: decision ka-0000001729 sat 10 days
// unanswered at P2, buried in the operator queue behind newer work, because
// priority is set once at filing and never revisited. A revival-review that goes
// unanswered is not getting LESS important with age — it is getting MORE
// important (the dark lane it is about stays dark the whole time), yet its queue
// position only decays. Fixes 1+2 (SweepClosedEnabledDrain) drain the low-value
// rows out of the pool so fewer of these are filed, and fix 4
// (SweepUncoveredOpenLaneRollup) decouples the uncovered-open lanes from the
// per-decision queue — but the decisions that ARE filed still need to surface
// above newer work once they go stale. That is this sweep.
//
// WHAT IT DOES. Once per throttle interval it scans OPEN revival-review decisions
// and, for any that has been open longer than an age threshold, RAISES its
// priority up an escalation ladder so it climbs the operator queue:
//
//   - open > staleRevivalReviewP1AgeThreshold (7d)  → escalate to P1
//   - open > staleRevivalReviewP0AgeThreshold (14d) → escalate to P0
//
// (Priority is numeric with 0 = most urgent, so "escalate" means LOWER the
// number.) Each actual bump comments an audit line on the decision and, when
// notify is on, routes a one-line summary to Slack. The operator then sees the
// stale decision at P1/P0 in `gb news` / the queue instead of parked at P2.
//
// WHY THE ENGINE. This is the same shape as the sibling sweeps
// (SweepClosedEnabledDrain, SweepUncoveredOpenLaneRollup): the reviver processes
// ONE candidate per cycle while the engine already iterates the whole roster
// every cycle for free, and priority escalation is a deterministic function of
// age that needs no agent tokens. It lives beside the drain/roll-up sweeps that
// address the other legs of bug ka-0000002082.
//
// IDEMPOTENT + OPERATOR-RESPECTING. The bump is naturally self-deduping: once a
// decision is at (or above) the urgency its age warrants, the sweep leaves it
// alone. It also stamps the most-urgent priority it has escalated a decision to
// (staleRevivalReviewEscalatedField); if an operator DELIBERATELY lowers a
// decision back down, the sweep will NOT re-escalate it to a level it already
// reached — it never fights an operator's explicit re-prioritization. Every bump
// is reversible (an operator can re-set priority) and audited (a comment per
// bump). No decision is ever created, closed, or resolved here — only its
// priority moves.

package bridge

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Stale revival-review escalation tunables.
const (
	// staleRevivalReviewRosterLimit bounds the open-decision roster pulled per
	// run. Open decisions fleet-wide are dozens today; this cap only guards a
	// runaway query and a breach is logged loudly rather than silently truncated
	// (advice ka-0000003555).
	staleRevivalReviewRosterLimit = 1000

	// staleRevivalReviewP1AgeThreshold is how long a revival-review decision may
	// sit OPEN before the sweep escalates it to P1. A week unanswered is the point
	// at which "buried behind newer work" (the measured failure: 10 days at P2) is
	// the likely cause rather than deliberate deferral.
	staleRevivalReviewP1AgeThreshold = 7 * 24 * time.Hour

	// staleRevivalReviewP0AgeThreshold is how long a revival-review decision may
	// sit OPEN before the sweep escalates it to P0 (top of the operator queue).
	// Two weeks unanswered is unambiguously stuck.
	staleRevivalReviewP0AgeThreshold = 14 * 24 * time.Hour

	// staleRevivalReviewThrottle is the minimum wall-clock between full sweep
	// runs. The sweep mutates a bead attribute (priority) and comments, so it must
	// not run on the engine's 30s Evaluate cadence; hourly is ample for an
	// age-based escalation measured in days.
	staleRevivalReviewThrottle = time.Hour

	// staleRevivalReviewTitlePrefix identifies a trigger-reviver revival-review
	// decision. The reviver files these as `type=decision` with a title/prompt
	// beginning "trigger-reviver-revival-review for <trigger>" (reviver.md §5) and
	// carries NO marker label, so the prefix is the stable machine handle. Matched
	// case-insensitively against BOTH the title and the `prompt` field so a future
	// reviver that only sets one still classifies.
	staleRevivalReviewTitlePrefix = "trigger-reviver-revival-review"

	// staleRevivalReviewActor is the synthetic author stamped on the sweep's
	// audit comments, mirroring the sibling sweeps' CreatedBy.
	staleRevivalReviewActor = "stale-revival-review-escalate-sweep"

	// staleRevivalReviewSystemOperator is the X-System-Operator source label the
	// sweep stamps on its writes so the daemon attributes this TRUE machine-origin
	// cron write instead of fail-closing on "operator could not be resolved"
	// (ka-0000007257), exactly as SweepClosedEnabledDrain does. Must satisfy the
	// daemon's sanitizeSystemOperatorHeader rules: ASCII lowercase / digits / '-'.
	staleRevivalReviewSystemOperator = "stale-revival-review-escalate"

	// staleRevivalReviewEscalatedField records (as the numeric priority string)
	// the MOST URGENT priority this sweep has already escalated a decision to.
	// Absent means never escalated. It exists solely so the sweep does not fight
	// an operator who deliberately LOWERS a decision the sweep already raised: the
	// sweep will not re-escalate to a level it has already reached.
	staleRevivalReviewEscalatedField = "revival_review_escalated_priority"

	// staleRevivalReviewNeverEscalated is the sentinel for "no prior escalation"
	// (larger than any real priority, so any target is strictly more urgent).
	staleRevivalReviewNeverEscalated = 99
)

// staleRevivalReviewEscalateState throttles the whole sweep to once per
// staleRevivalReviewThrottle. The sweep's idempotency lives on the beads it
// writes (priority + the escalated-priority stamp), so the only in-memory state
// needed is the last-run clock — same shape as the drain sweep's state.
type staleRevivalReviewEscalateState struct {
	mu        sync.Mutex
	lastRunAt time.Time
}

func newStaleRevivalReviewEscalateState() *staleRevivalReviewEscalateState {
	return &staleRevivalReviewEscalateState{}
}

// dueForRun reports whether the throttle interval has elapsed since the last run,
// and if so records now as the new last-run time (so concurrent callers cannot
// both pass). A zero lastRunAt (first ever call) is always due.
func (s *staleRevivalReviewEscalateState) dueForRun(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lastRunAt.IsZero() && now.Sub(s.lastRunAt) < staleRevivalReviewThrottle {
		return false
	}
	s.lastRunAt = now
	return true
}

// isRevivalReviewDecision reports whether an open decision bead is a
// trigger-reviver revival-review, matched case-insensitively on the title or the
// `prompt` field prefix.
func isRevivalReviewDecision(bead *beadsapi.BeadDetail) bool {
	if bead == nil {
		return false
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(bead.Title)), staleRevivalReviewTitlePrefix) {
		return true
	}
	if p := bead.Fields["prompt"]; strings.HasPrefix(strings.ToLower(strings.TrimSpace(p)), staleRevivalReviewTitlePrefix) {
		return true
	}
	return false
}

// staleRevivalReviewTargetPriority maps an open-decision age to the priority the
// sweep should escalate it to, or -1 when it is not stale enough to escalate.
func staleRevivalReviewTargetPriority(age time.Duration) int {
	switch {
	case age >= staleRevivalReviewP0AgeThreshold:
		return 0
	case age >= staleRevivalReviewP1AgeThreshold:
		return 1
	default:
		return -1
	}
}

// SweepStaleRevivalReviewEscalate escalates the priority of stale open
// revival-review decisions (bug ka-0000002082 fix 3). Once per throttle interval
// it raises any revival-review decision that has been open past an age threshold
// up the P2→P1→P0 ladder so it surfaces above newer work in the operator queue.
//
// No-op unless staleRevivalReviewEscalateEnabled. Skips entirely under a global
// triggers-pause / gardeners-pause (an operator holding the fleet should not have
// the engine reshuffling their queue underneath them). Errors are logged and
// downgraded so a daemon hiccup cannot wedge the engine, and every bump is
// reversible (an operator can re-set priority) and audited.
func (e *TriggerEngine) SweepStaleRevivalReviewEscalate(ctx context.Context) {
	if !e.staleRevivalReviewEscalateEnabled {
		return
	}
	now := e.now()
	if !e.staleRevivalReviewEscalateState.dueForRun(now) {
		return
	}

	// A deliberate global pause means the operator is holding the fleet; the
	// engine must not be mutating bead priorities under it. Mirrors the drain
	// sweep's pause guards.
	if gardenersPauseEngaged(ctx, e.daemon, e.logger) {
		e.logger.Info("stale-revival-review-escalate: gardeners-pause engaged, skipping")
		return
	}
	if e.loadGateState(ctx).globallyPaused {
		e.logger.Info("stale-revival-review-escalate: triggers-pause engaged, skipping")
		return
	}

	// Attribute every write to the machine operator (see SweepClosedEnabledDrain
	// for why: the daemon's fail-closed audit-operator resolver would otherwise
	// 400 the priority PATCH and the audit AddComment). Any per-call human
	// operator still wins, so this never mis-attributes a human write.
	ctx = beadsapi.WithSystemOperator(ctx, staleRevivalReviewSystemOperator)

	res, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Limit:    staleRevivalReviewRosterLimit,
	})
	if err != nil {
		e.logger.Warn("stale-revival-review-escalate: failed to load open decisions", "error", err)
		return
	}
	if res.Total > len(res.Beads) {
		// A silent truncation here would under-count the pool and let a stale
		// decision escape escalation (advice ka-0000003555). Make it loud.
		e.logger.Warn("stale-revival-review-escalate: roster truncated by limit — more open decisions than one page",
			"returned", len(res.Beads), "total", res.Total, "limit", staleRevivalReviewRosterLimit)
	}

	// Collect the revival-review candidates so escalation is applied oldest-first
	// (deterministic, and the oldest decisions climb the queue first).
	var candidates []*beadsapi.BeadDetail
	for _, bead := range res.Beads {
		if isRevivalReviewDecision(bead) {
			candidates = append(candidates, bead)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
	})

	escalated := 0
	for _, bead := range candidates {
		if e.escalateStaleRevivalReview(ctx, bead, now) {
			escalated++
		}
	}

	e.logger.Info("stale-revival-review-escalate: complete",
		"open_decisions", len(res.Beads),
		"revival_review_candidates", len(candidates),
		"escalated", escalated)

	if e.staleRevivalReviewEscalateNotify && e.notifier != nil && escalated > 0 {
		e.notifier(ctx, "stale-revival-review-escalate",
			fmt.Sprintf("⏫ stale-revival-review-escalate: raised priority on %d stale revival-review decision(s) so they surface above newer work in the operator queue (bug ka-0000002082 fix 3).", escalated),
			"")
	}
}

// escalateStaleRevivalReview raises one revival-review decision's priority to the
// level its age warrants, if that is strictly more urgent than both its current
// priority and the most-urgent level the sweep already stamped. Returns true when
// it changed the priority. Priority + the escalated-priority stamp are written in
// ONE atomic PATCH so a restart between them cannot leave the two out of sync.
func (e *TriggerEngine) escalateStaleRevivalReview(ctx context.Context, bead *beadsapi.BeadDetail, now time.Time) bool {
	age := now.Sub(bead.CreatedAt)
	target := staleRevivalReviewTargetPriority(age)
	if target < 0 {
		return false // not stale enough yet
	}
	// Already at (or above) the urgency the age warrants — nothing to do. This is
	// what makes repeated runs idempotent once a decision has been escalated.
	if target >= bead.Priority {
		return false
	}
	// Respect an operator who deliberately lowered a decision we already raised:
	// never re-escalate to a level we have already reached.
	if prior := staleRevivalReviewPriorEscalation(bead); prior <= target {
		return false
	}

	if err := e.daemon.UpdateBead(ctx, bead.ID, beadsapi.UpdateBeadRequest{
		Priority: &target,
		Fields:   map[string]any{staleRevivalReviewEscalatedField: strconv.Itoa(target)},
	}); err != nil {
		e.logger.Warn("stale-revival-review-escalate: failed to bump decision priority",
			"decision", bead.ID, "from", bead.Priority, "to", target, "error", err)
		return false
	}
	e.logger.Info("stale-revival-review-escalate: escalated stale revival-review decision",
		"decision", bead.ID, "from_priority", bead.Priority, "to_priority", target,
		"age_days", int(age.Hours()/24))
	audit := fmt.Sprintf(
		"%s: escalated priority P%d→P%d — this revival-review decision has been open %d day(s) unanswered, "+
			"so it is buried behind newer work in the operator queue. Age-based escalation surfaces it (bug "+
			"ka-0000002082 fix 3). Reversible: an operator can re-set the priority; the sweep will not re-raise "+
			"a level it has already reached.",
		staleRevivalReviewActor, bead.Priority, target, int(age.Hours()/24))
	if err := e.daemon.AddComment(ctx, bead.ID, staleRevivalReviewActor, audit); err != nil {
		e.logger.Warn("stale-revival-review-escalate: failed to comment escalation audit line",
			"decision", bead.ID, "error", err)
	}
	return true
}

// staleRevivalReviewPriorEscalation returns the most-urgent priority the sweep
// has already escalated this decision to, or staleRevivalReviewNeverEscalated
// when it carries no such stamp (or an unparseable one — treated as never, so a
// corrupt stamp can never wedge escalation).
func staleRevivalReviewPriorEscalation(bead *beadsapi.BeadDetail) int {
	raw := strings.TrimSpace(bead.Fields[staleRevivalReviewEscalatedField])
	if raw == "" {
		return staleRevivalReviewNeverEscalated
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return staleRevivalReviewNeverEscalated
	}
	return v
}
