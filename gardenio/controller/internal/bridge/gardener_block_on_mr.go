// Package bridge — gardener block-on-MR-merge + proactive operator-wedge
// dormancy (ka-0000007387).
//
// Two pre-fire dormancy paths, both parking a gardener into the existing
// DormantOnEscalation machinery (bug ka-0000003082) rather than closing it —
// the gardener stays OPEN + enabled so the cron preflight wakes it automatically
// the moment its blocker clears, with no pod spawns while parked:
//
//   PRONG 1 (declared, opt-in) — gardenerBlockOnMR: a gardener whose stop.loop_end
//     carries an {"kind":"mr_merged","parent":"<epic>"} entry parks while ANY
//     tracked MR under that parent is unmerged. This is the first-class "block
//     until MR merged" facility an operator can DECLARE — the missing intent
//     that made a merge-lane gardener burn ~110 pod spawns re-deriving "nothing
//     to do, waiting on a human merge."
//
//   PRONG 2 (proactive, all gardeners) — gardenerFrontierFullyWedged: any gardener
//     whose ENTIRE open frontier is single-threaded behind open operator
//     escalation/decision beads (zero actionable children) parks immediately.
//     This is the DIRECT path into dormancy that the resume-budget deadlock made
//     unreachable: a gardener that self-reports progress_score=1 every cycle (by
//     "reflagging the escalation") pins ResumesWithoutProgress at 0, so the
//     legacy dormancy check — gated behind resume-budget exhaustion — was never
//     consulted. This gate does not depend on that counter.
//
// Both are FAIL-SAFE toward NOT parking: any daemon read error, any uncertainty,
// leaves the gardener live (it does one more normal cycle and re-evaluates). A
// wrongful non-park is a self-correcting no-op; a wrongful park is avoided by
// construction. The wake side is symmetric (gardenerDormantStillBlocked): a
// dormant gardener fail-OPENs to waking, so a flaky read can never trap it.

package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
)

// DormantBlockerKind discriminators (persisted on GardenerSpec.DormantBlockerKind)
// select the wake predicate for a dormant gardener. Empty string is the legacy
// operator-escalation dormancy (every pre-existing dormant gardener); it now wakes
// on the operator_wedge predicate (bug gd-FUC635HJ2E).
const (
	gardenerDormantKindOperatorWedge     = "operator_wedge"
	gardenerDormantKindMRMerged          = "mr_merged"
	gardenerDormantKindResumeBudgetSpent = "resume_budget_spent"
)

// gardenerMRSubtreeScanCap bounds how many subtree beads gardenerHasUnmergedTrackedMR
// walks for produces→MR edges. Realistic epics have far fewer children; the cap is
// a safety valve. If the subtree exceeds it we fail toward NOT parking (see the
// truncation note there), which is the safe direction for a park decision.
const gardenerMRSubtreeScanCap = 200

// gardenerMRMergeBlockParent returns the parent epic a declared mr_merged loop_end
// entry scopes its tracked MRs to, and whether such an entry is declared at all.
// The entry's Parent field wins; an empty Parent falls back to the gardener's
// frontier epic (AssignBead / gardener-epic:<id> label). Returns declared=false
// (and skips all MR work) for the overwhelming majority of gardeners that do not
// opt into the facility.
func gardenerMRMergeBlockParent(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) (parent string, declared bool) {
	if trig.Gardener == nil || len(trig.Gardener.Stop) == 0 {
		return "", false
	}
	spec, err := scheduler.ParseStopSpec(string(trig.Gardener.Stop))
	if err != nil {
		return "", false
	}
	for _, e := range spec.LoopEnd.Any {
		if e.Kind != scheduler.StopKindMRMerged {
			continue
		}
		if p := strings.TrimSpace(e.Parent); p != "" {
			return p, true
		}
		// No explicit parent — scope to the gardener's frontier epic.
		return gardenerFrontierEpic(ctx, daemon, logger, trig), true
	}
	return "", false
}

// gardenerMRMergeBlockActive reports whether a gardener that DECLARED an mr_merged
// loop_end entry should park right now: the declared parent is still open AND at
// least one tracked MR under it is unmerged. Returns the parent (for
// DormantBlocker) and active=true only when parking is warranted.
//
// Fail-safe: a non-declared gardener, a closed/unresolved parent, or any read
// error yields active=false — the gardener stays live.
func gardenerMRMergeBlockActive(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) (parent string, active bool) {
	parent, declared := gardenerMRMergeBlockParent(ctx, daemon, logger, trig)
	if !declared || strings.TrimSpace(parent) == "" {
		return "", false
	}
	return parent, gardenerHasUnmergedTrackedMR(ctx, daemon, logger, parent)
}

// gardenerHasUnmergedTrackedMR reports whether the subtree rooted at parent
// contains at least one bead that PRODUCES an MR bead which is not yet merged —
// i.e. the gardener is provably "blocked on an MR being merged." Zero tracked MRs
// (none produced yet) and all-MRs-merged both yield false: in neither case is
// there an MR to sleep on.
//
// Fail-safe toward NOT parking: a closed/unfetchable parent, a subtree read
// error, or a truncated subtree scan all return false so the gardener stays live
// (worst case it runs one more normal cycle and re-checks). "Merged" mirrors the
// progress evaluator's taskHasMergedMR exactly (MR bead mr_outcome/mr_state ==
// "merged"), reading the type=merge-request bead reached via the `produces` edge
// (epic ka-0000001613) — the canonical source of truth.
func gardenerHasUnmergedTrackedMR(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, parent string) bool {
	parent = strings.TrimSpace(parent)
	if parent == "" || daemon == nil {
		return false
	}
	// A closed parent epic means the plan is done — nothing to wait on.
	if epic, err := daemon.GetBead(ctx, parent); err != nil || epic == nil || epic.Status == "closed" {
		return false
	}
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Parent: parent,
		Types:  []string{"task", "bug", "feature", "epic"},
		// Only OPEN producing beads: an unmerged MR "blocks" a gardener only
		// while the work that produced it is still open. A CLOSED child whose
		// MR happens to be unmerged (abandoned/superseded) must not trap the
		// gardener dormant forever — restricting statuses here means an all-
		// children-closed gardener reports zero unmerged tracked MRs and falls
		// through to its bead_query goal-completion close instead of parking.
		Statuses: []string{"open", "in_progress", "blocked"},
		Limit:    gardenerMRSubtreeScanCap,
	})
	if err != nil {
		logger.Warn("gardener block-on-mr: subtree list failed — not parking (fail-safe)",
			"trigger", parent, "error", err)
		return false
	}
	if res == nil {
		return false
	}
	if res.Total > len(res.Beads) {
		// Truncated scan: we cannot see every producing bead, so we cannot
		// prove an unmerged MR exists across the whole subtree. Bias toward NOT
		// parking (keep the gardener live) rather than parking on partial data.
		logger.Warn("gardener block-on-mr: subtree exceeds scan cap — not parking (fail-safe)",
			"parent", parent, "total", res.Total, "scanned", len(res.Beads), "cap", gardenerMRSubtreeScanCap)
		return false
	}
	for _, b := range res.Beads {
		if b == nil || b.ID == "" {
			continue
		}
		mr, err := daemon.MRBeadForTask(ctx, b.ID)
		if err != nil || mr == nil {
			continue
		}
		if mr.Fields["mr_outcome"] == "merged" || mr.Fields["mr_state"] == "merged" {
			continue
		}
		// A tracked MR bead exists and is NOT merged — the gardener is blocked
		// on this merge.
		return true
	}
	return false
}

// gardenerFrontierFullyWedged reports whether a gardener's ENTIRE open frontier is
// single-threaded behind open operator escalation/decision beads — i.e. every
// open child is blocked_by an open escalation/decision, so the gardener provably
// cannot self-serve any forward work this cycle. Returns the blocker bead ID
// (for DormantBlocker) and wedged=true only when ALL of:
//
//   - the gardener resolves to an open frontier epic, AND
//   - at least one OPEN operator escalation/decision sits on that frontier, AND
//   - the actionable-child count (open subtree children MINUS those blocked_by an
//     open escalation/decision) is exactly zero.
//
// This is the reachability fix (ka-0000007387): it parks a fully-wedged gardener
// DIRECTLY, without waiting for the resume budget to exhaust — the path that
// reflag-churn (self-reported progress_score=1 pinning ResumesWithoutProgress at
// 0) rendered unreachable.
//
// Fail-safe toward NOT wedged: the actionable-count query fail-safes to the full
// count on any uncertainty (conditions.subtractExternallyBlocked only ever
// REDUCES for beads it can positively prove blocked), and any evaluator error is
// treated as not-wedged here. So the only way to return wedged=true is a
// positively-proven empty actionable frontier with an open blocker present.
func gardenerFrontierFullyWedged(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) (blocker string, wedged bool) {
	epic := gardenerFrontierEpic(ctx, daemon, logger, trig)
	if epic == "" {
		return "", false
	}
	if epicDetail, err := daemon.GetBead(ctx, epic); err != nil || epicDetail == nil || epicDetail.Status == "closed" {
		return "", false
	}
	// Need an actual open operator blocker on the frontier — a gardener with no
	// escalation/decision is "idle," not "operator-wedged," and must not park
	// here (it routes through the normal stop/resume gates instead).
	blocker = gardenerFrontierBlocker(ctx, daemon, logger, trig)
	if blocker == "" {
		return "", false
	}
	// actionable == 0 : count open subtree children, subtracting any blocked_by
	// an OPEN escalation/decision. met=true ⇒ the frontier has no self-servable
	// work left.
	met, err := conditions.EvaluateCondition(ctx, gardenerDaemonStopAdapter{d: daemon}, conditions.Condition{
		Type:                      "bead_query",
		Op:                        "==",
		Value:                     "0",
		Types:                     []string{"task", "bug", "feature", "epic"},
		Statuses:                  []string{"open", "in_progress", "blocked"},
		Parent:                    epic,
		ExcludeBlockedByOpenTypes: []string{"escalation", "decision"},
	}, trig.Project)
	if err != nil {
		logger.Warn("gardener wedge check: actionable-count query failed — treating as not wedged (fail-safe)",
			"trigger", trig.ID, "epic", epic, "error", err)
		return "", false
	}
	if !met {
		return "", false
	}
	return blocker, true
}

// gardenerDormantStillBlocked reports whether an already-dormant gardener's blocker
// is still active, selecting the wake predicate by DormantBlockerKind. This is
// the single decision the pre-fire dormant gate consults each cycle: true ⇒ stay
// parked (skip fire silently); false ⇒ wake and fall through to the normal gates.
//
// The empty-kind legacy operator-escalation dormancy (ka-0000003082) wakes on the
// same predicate as operator_wedge (bug gd-FUC635HJ2E). It used to stay parked
// while ANY open escalation/decision sat anywhere in the subtree, so one
// owner-at-device escalation or a stale in_progress executor escalation froze 9
// of 13 plan gardeners for days with most of their frontier actionable. A
// dormant gardener now stays parked only while the frontier has zero actionable
// children; the moment one exists it wakes, with no data migration.
func gardenerDormantStillBlocked(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) bool {
	if trig.Gardener == nil {
		return false
	}
	switch trig.Gardener.DormantBlockerKind {
	case gardenerDormantKindMRMerged:
		return gardenerHasUnmergedTrackedMR(ctx, daemon, logger, trig.Gardener.DormantBlocker)
	case gardenerDormantKindResumeBudgetSpent:
		return gardenerPlanStillQuiet(ctx, daemon, logger, trig)
	default:
		_, wedged := gardenerFrontierFullyWedged(ctx, daemon, logger, trig)
		return wedged
	}
}

// gardenerPlanActivityDigest fingerprints the open work under epic: the sorted
// id:status of every open, in_progress or blocked subtree bead, plus the
// count. A step closing, reopening, starting, returning to ready or being
// added changes it; comments and edits that keep the status do not
// (gd-dNxyjFLGRJ).
func gardenerPlanActivityDigest(ctx context.Context, daemon gardenerDaemon, epic string) (string, error) {
	return gardenerPlanDigest(ctx, daemon, epic, false)
}

// gardenerPlanProgressDigest is gardenerPlanActivityDigest plus a mark on each
// open step whose PR has merged, so a merge on a step that stays open (it
// awaits a release) changes it too. It backs the resume-progress check
// (gardenerPlanProgressSinceResume, gd-WE8g4FFEHm). A failed PR read is an
// error: a guessed mark would read as progress.
func gardenerPlanProgressDigest(ctx context.Context, daemon gardenerDaemon, epic string) (string, error) {
	return gardenerPlanDigest(ctx, daemon, epic, true)
}

func gardenerPlanDigest(ctx context.Context, daemon gardenerDaemon, epic string, markMerged bool) (string, error) {
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Parent:   epic,
		Types:    []string{"task", "bug", "feature", "epic"},
		Statuses: []string{"open", "in_progress", "blocked"},
		Limit:    gardenerMRSubtreeScanCap,
	})
	if err != nil {
		return "", err
	}
	var rows []string
	total := 0
	if res != nil {
		total = res.Total
		for _, b := range res.Beads {
			if b == nil || b.ID == "" {
				continue
			}
			row := b.ID + ":" + b.Status
			if markMerged {
				mr, err := daemon.MRBeadForTask(ctx, b.ID)
				if err != nil {
					return "", err
				}
				if mr != nil && (mr.Fields["mr_outcome"] == "merged" || mr.Fields["mr_state"] == "merged") {
					row += ":merged"
				}
			}
			rows = append(rows, row)
		}
	}
	sort.Strings(rows)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\n%s", total, strings.Join(rows, "\n"))))
	return hex.EncodeToString(sum[:8]), nil
}

// gardenerPlanStillQuiet is the wake predicate of a resume_budget_spent
// gardener: true while its plan's open work matches the digest stamped at
// park time. A closed plan wakes it so the normal gates can finish it. A read
// error keeps it parked: waking refills the run budget, so a flaky read must
// not buy spawns; the next cycle reads again.
func gardenerPlanStillQuiet(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) bool {
	quiet, err := gardenerPlanQuiet(ctx, daemon, trig)
	if err != nil {
		logger.Warn("gardener plan-activity wake: plan read failed, staying dormant",
			"trigger", trig.ID, "epic", trig.Gardener.DormantBlocker, "error", err)
		return true
	}
	return quiet
}

// gardenerPlanQuiet reports whether a resume_budget_spent gardener's plan
// (DormantBlocker) is open and its open work still matches the stamped digest.
// It returns the read error rather than choosing a side, so the wake gate can
// stay parked and the read-only dormancy probe can report not parked.
func gardenerPlanQuiet(ctx context.Context, daemon gardenerDaemon, trig TriggerDef) (bool, error) {
	if trig.Gardener == nil {
		return false, nil
	}
	epic := strings.TrimSpace(trig.Gardener.DormantBlocker)
	if epic == "" {
		return false, nil
	}
	detail, err := daemon.GetBead(ctx, epic)
	if err != nil {
		return false, err
	}
	if detail == nil || detail.Status == "closed" {
		return false, nil
	}
	digest, err := gardenerPlanActivityDigest(ctx, daemon, epic)
	if err != nil {
		return false, err
	}
	return digest == trig.Gardener.DormantActivityDigest, nil
}
