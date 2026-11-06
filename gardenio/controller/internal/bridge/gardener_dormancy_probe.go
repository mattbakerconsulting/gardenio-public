// Package bridge — read-only gardener-dormancy probe for `gb trigger reconcile`
// (bug ka-0000007872).
//
// THE BUG. `gb trigger reconcile` diffs a gardener's UNCONDITIONAL cron schedule
// against its observed fires and folds four overlays (pause → lease →
// time_window → gate) onto the residual. Gardener DORMANCY was the one
// suppression path left OUT of that chain, so a gardener CORRECTLY PARKED behind
// an open operator escalation/decision — its entire frontier single-threaded
// behind a human, firing nothing BY DESIGN — reported its every by-design
// non-fire as a candidate miss (measured: 336 phantom missed fires, 100%
// "drift"). Worse, the two persisted fields a reader reaches for both LIE: the
// DRIFT-CANDIDATE math never subtracted dormancy, and `dormant_blocker` is
// write-once so it goes stale the moment the wedge moves to a different blocker
// bead (advice ka-0000002229 / ka-0000000175: "evaluate the frontier predicate,
// not the two fields").
//
// THE FIX. This probe recomputes the gardener's dormancy verdict LIVE, from the
// same canonical engine predicates the pre-fire dormant gate consults
// (gardenerFrontierFullyWedged / gardenerMRMergeBlockActive in
// gardener_block_on_mr.go) — never trusting the stamped DormantOnEscalation /
// DormantBlocker / DormantBlockerKind fields for the verdict. It is the single
// source of truth by construction: it CALLS the engine predicates rather than
// re-deriving them, so it cannot drift from the fire path (the re-implementation
// gap that produced this bug). cmd/gb folds a positive verdict into the
// reconcile drift chain (dormancy takes precedence over the point-in-time gate)
// so a parked gardener reads PARKED, not MISSED N.
//
// FAIL-SAFE toward NOT parked, exactly as the engine predicates are: the verdict
// is Parked=true only on a positively-proven live wedge (an open operator
// blocker on the frontier AND a proven-zero actionable-child count, or a
// declared+active block-until-MR-merged). Any read error, any uncertainty, and
// any non-gardener trigger yields Parked=false — so this can NEVER mask a genuine
// lease-wedge outage (a dead spawn holding max_concurrent=1 is orthogonal to
// dormancy: DormantOnEscalation is not the mechanism and the frontier is not
// operator-wedged) or a genuinely drifting trigger. Being read-only, it stamps
// nothing and closes nothing.

package bridge

import (
	"context"
	"log/slog"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler/conditions"
)

// ReconcileDormancy is the live gardener-dormancy verdict `gb trigger reconcile`
// folds into its drift-candidate chain. Field names are stable JSON contract the
// reconciler agent and the human printer both read.
type ReconcileDormancy struct {
	// Parked is the load-bearing verdict: the gardener is PROVABLY parked right
	// now — its every by-design non-fire in the window is a park, not a miss. It
	// is computed LIVE from the engine frontier predicates, never from the
	// stamped dormancy fields. Fail-safe: only a positively-proven wedge sets it.
	Parked bool `json:"parked"`

	// Kind names WHICH live predicate proved the park:
	//   "operator_wedge" — the entire open frontier is single-threaded behind an
	//                       open operator escalation/decision (zero actionable
	//                       children). gardenerFrontierFullyWedged.
	//   "mr_merged"       — the gardener DECLARED a block-until-MR-merged loop_end
	//                       and a tracked MR under its parent is still unmerged.
	//                       gardenerMRMergeBlockActive.
	//   "resume_budget_spent" — the gardener is parked on its plan after spending
	//                       its resume budget, and the plan's open work still
	//                       matches the stamped digest (gardenerPlanQuiet,
	//                       gd-dNxyjFLGRJ). A read error reads not parked.
	// Empty when Parked is false.
	Kind string `json:"kind,omitempty"`

	// LiveBlocker is the blocker bead id computed THIS cycle (the open
	// escalation/decision on the frontier, or the block-until-MR parent). This
	// is the authoritative value — unlike StampedBlocker it is never stale.
	LiveBlocker string `json:"live_blocker,omitempty"`

	// FrontierEpic is the epic the gardener drives (AssignBead / gardener-epic:
	// label), the root the actionable-child count is scoped to. Empty when the
	// gardener is not pinned to an epic (no frontier to inspect → never parked
	// via operator_wedge).
	FrontierEpic string `json:"frontier_epic,omitempty"`

	// ActionableChildren is the live count of open subtree children MINUS those
	// blocked_by an open escalation/decision — the exact quantity the
	// operator_wedge predicate floors to 0. 0 with an open blocker present is the
	// proven wedge; >0 means the frontier has self-servable work and the gardener
	// is NOT operator-wedged (a genuine drift signal survives). -1 when not
	// computed (no frontier epic, or a count error — treated as not-wedged).
	ActionableChildren int `json:"actionable_children"`

	// --- Stamped fields, surfaced for the stale-annotation the bug asks for. ---

	// DormantDeclared echoes the persisted GardenerSpec.DormantOnEscalation flag.
	// It is NOT consulted for the Parked verdict (advice ka-0000002229: the field
	// lies — a gardener can read enabled+dormant while genuinely broken, or park
	// correctly with the flag never flipped). Surfaced only so a reader can see
	// the declared-vs-live divergence.
	DormantDeclared bool `json:"dormant_declared"`

	// StampedBlocker echoes the persisted GardenerSpec.DormantBlocker. It is
	// WRITE-ONCE at park time, so it goes stale the moment the wedge is
	// superseded by a different blocker bead. Compare against LiveBlocker.
	StampedBlocker string `json:"stamped_blocker,omitempty"`

	// StampedBlockerStale is true when a live wedge exists on a blocker bead
	// DIFFERENT from the persisted StampedBlocker — i.e. the stamped field is
	// pointing at a superseded bead and must not be trusted. This is the
	// deterministic answer to ASK 2: the stamped label is a non-authoritative
	// first-park marker; the LiveBlocker is the current truth.
	StampedBlockerStale bool `json:"stamped_blocker_stale,omitempty"`

	// Evaluated is false only when the probe could not run (nil daemon). A
	// non-gardener trigger returns a nil *ReconcileDormancy, not Evaluated=false.
	Evaluated bool `json:"evaluated"`
}

// ReconcileDormancyProbe computes the live dormancy verdict for a trigger bead,
// or nil when the bead is not a gardener (no gardener block) — in which case
// dormancy cannot apply and the caller leaves its drift chain untouched.
//
// It parses the trigger, resolves the frontier, and asks the SAME engine
// predicates the pre-fire dormant gate uses whether the gardener is wedged RIGHT
// NOW — deliberately ignoring the stamped dormancy fields for the verdict (they
// are surfaced only for the stale annotation). The verdict is fail-safe toward
// NOT parked; see the package doc.
func ReconcileDormancyProbe(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, bead *beadsapi.BeadDetail) *ReconcileDormancy {
	if logger == nil {
		logger = slog.Default()
	}
	if bead == nil {
		return nil
	}
	trig := ParseTriggerDef(bead, strings.TrimSpace(bead.Fields["source"]))
	if trig.Gardener == nil {
		// Not a gardener — dormancy is inapplicable. Signal "no overlay" with nil.
		return nil
	}
	d := &ReconcileDormancy{
		ActionableChildren: -1,
		DormantDeclared:    trig.Gardener.DormantOnEscalation,
		StampedBlocker:     strings.TrimSpace(trig.Gardener.DormantBlocker),
	}
	if daemon == nil {
		// No client — cannot probe. Leave Parked=false so the drift residual
		// stands (fail-safe toward reporting drift, never masking it).
		d.Evaluated = false
		return d
	}
	d.Evaluated = true
	d.FrontierEpic = gardenerFrontierEpic(ctx, daemon, logger, trig)

	// Live actionable-child count for the printed row — the exact quantity the
	// operator_wedge predicate floors to 0. Independent of the verdict below so
	// the row is populated even when the gardener is NOT wedged (a >0 count is the
	// diagnostic that says WHY a supposedly-dormant gardener still shows drift).
	if d.FrontierEpic != "" {
		if n, err := conditions.EvaluateBeadQueryCount(ctx, gardenerDaemonStopAdapter{d: daemon}, conditions.Leaf{
			Type:                      "bead_query",
			Op:                        "==",
			Value:                     "0",
			Types:                     []string{"task", "bug", "feature", "epic"},
			Statuses:                  []string{"open", "in_progress", "blocked"},
			Parent:                    d.FrontierEpic,
			ExcludeBlockedByOpenTypes: []string{"escalation", "decision"},
		}, trig.Project); err == nil {
			d.ActionableChildren = n
		}
	}

	// LIVE verdict — recomputed from the canonical engine predicates, never the
	// stamped fields. Prong 1 (proactive operator-wedge) is the common park; it
	// requires an open operator blocker on the frontier AND a proven-zero
	// actionable count, so it can never fire on a gardener with self-servable
	// work. Prong 2 (declared block-until-MR-merged) is opt-in config, not a
	// stale field, so it is safe to consult.
	if blocker, wedged := gardenerFrontierFullyWedged(ctx, daemon, logger, trig); wedged {
		d.Parked = true
		d.Kind = gardenerDormantKindOperatorWedge
		d.LiveBlocker = strings.TrimSpace(blocker)
	} else if parent, active := gardenerMRMergeBlockActive(ctx, daemon, logger, trig); active {
		d.Parked = true
		d.Kind = gardenerDormantKindMRMerged
		d.LiveBlocker = strings.TrimSpace(parent)
	} else if trig.Gardener.DormantOnEscalation && trig.Gardener.DormantBlockerKind == gardenerDormantKindResumeBudgetSpent {
		// Parked by its own run history, not a live blocker, so the stamped
		// kind selects the predicate; the predicate itself is still live.
		if quiet, err := gardenerPlanQuiet(ctx, daemon, trig); err == nil && quiet {
			d.Parked = true
			d.Kind = gardenerDormantKindResumeBudgetSpent
			d.LiveBlocker = strings.TrimSpace(trig.Gardener.DormantBlocker)
		}
	}

	// Stale-stamp annotation (ASK 2): a live wedge on a blocker DIFFERENT from
	// the persisted first-park marker means the stamped field is pointing at a
	// superseded bead. Only meaningful when both a live and a stamped blocker
	// exist — an absent stamp is "never parked before", not stale.
	if d.LiveBlocker != "" && d.StampedBlocker != "" && d.LiveBlocker != d.StampedBlocker {
		d.StampedBlockerStale = true
	}
	return d
}
