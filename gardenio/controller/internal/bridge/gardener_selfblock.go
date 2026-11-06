// Package bridge — gardener SELF-BLOCK classifier (ka-0000000849).
//
// Two shipped tools historically answered "is this gardener bounded?" and
// disagreed, because they were silently measuring TWO DIFFERENT safety
// properties and both called the answer "bounded":
//
//   - gardener_bounds_guard.go (ValidateGardenerBounds, behind `gb gardener
//     status` and the engine warn) checks the BACKSTOP: run_budget > 0 AND a
//     hard_stop wall_clock parsing into (0, gardenerMaxWallClock]. This is the
//     engine/reaper EMERGENCY BRAKE — the ceiling that fires unconditionally
//     even if every progress signal is broken.
//   - scripts/gardener-autoblock-audit.py checks the INTELLIGENT SELF-BLOCK:
//     per advice ka-0000000974, a progress-aware (no_progress_for + a progress
//     spec) or goal-completion (bead_query / specific_bead_closed / max_runs)
//     loop_end so the gardener terminates when it stops making forward progress —
//     NOT merely on a pod crash or a multi-week wall_clock.
//
// Neither property implies the other. ka-0000003772 (gardener-tree-hug) carries
// run_budget=50 + loop_end [max_runs, no_progress_for] + a progress spec, so it
// is `self-block: ok` in the audit, yet its only hard_stop is
// consecutive_failures — no wall_clock — so it is `backstop: NO` per the guard.
// Same bead, opposite one-word verdicts, and an operator spot-checking with `gb
// gardener status` saw a different offender set than the sentinel's audit. That
// is how a reviver produced "3 unbounded gardeners" the audit scored 0
// (decision ka-0000006804).
//
// The reconciliation (bug FIX OPTION b, the honest one): keep BOTH verdicts —
// they measure genuinely different things — but name them distinctly and
// surface BOTH everywhere. This file is the shared Go classifier that computes
// both axes off one decoded GardenerSpec, so `gb gardener status` reports the same
// two axes the audit does. The audit's Python classifier gains the mirror-image
// backstop axis (has_backstop), and a cross-tool parity test pins the
// intelligent-kind vocabulary so the two can never silently drift again.
//
// This is PURELY ADDITIVE: no enforcement threshold, authoring-guard rule, or
// audit bucket changes — the backstop stays the engine-enforced brake, the
// self-block stays advisory. Nothing here decides "which definition wins"; it
// makes both visible so no one mistakes one for the other.
package bridge

import (
	"strings"

	"gardenio/controller/internal/scheduler"
)

// IntelligentLoopEndKinds are the stop.loop_end.any kinds that prove a gardener
// terminates on PROGRESS or GOAL completion rather than only on crash / old
// age. This is the Go mirror of INTELLIGENT_LOOP_END_KINDS in
// gardenio/scripts/gardener-autoblock-audit.py; TestIntelligentLoopEndKinds_Doc
// pins the exact set so the two tools can never drift on what "intelligent"
// means (the drift that produced bug ka-0000000849).
var IntelligentLoopEndKinds = map[string]bool{
	scheduler.StopKindNoProgressFor:      true,
	scheduler.StopKindBeadQuery:          true,
	scheduler.StopKindSpecificBeadClosed: true,
	scheduler.StopKindMaxRuns:            true,
}

// SelfBlockLevel is the progress/goal-awareness verdict for a gardener — the
// axis the auto-block audit classifies. The three levels map 1:1 onto the
// audit's offender / weak / ok buckets so a surveyor reading either tool sees
// the same word for the same shape.
type SelfBlockLevel string

const (
	// SelfBlockNone is the audit's "offender": no run_budget AND no intelligent
	// loop_end kind — can only stop on a pod crash or a multi-week wall_clock.
	SelfBlockNone SelfBlockLevel = "none"
	// SelfBlockWeak is the audit's "weak": terminates eventually (a run_budget
	// or a goal-completion stop) but has no progress-aware wedge detector, so it
	// cannot notice "crew keeps getting blocked, nothing is closing".
	SelfBlockWeak SelfBlockLevel = "weak"
	// SelfBlockOK is the audit's "ok": a no_progress_for wedge detector backed
	// by a progress spec, so the loop self-terminates on a stall.
	SelfBlockOK SelfBlockLevel = "ok"
)

// GardenerSafetyVerdict carries BOTH safety axes for one gardener so callers
// (notably `gb gardener status`) can render them side by side. HasBackstop /
// BackstopReason are the engine-enforced emergency brake (ValidateGardenerBounds);
// SelfBlock / SelfBlockReason are the advisory progress-aware self-block. The
// remaining fields are the raw signals the self-block verdict was derived from,
// surfaced for the --json consumer (mirroring the audit's per-line detail).
type GardenerSafetyVerdict struct {
	HasBackstop     bool           `json:"has_backstop"`
	BackstopReason  string         `json:"backstop_reason,omitempty"`
	SelfBlock       SelfBlockLevel `json:"self_block"`
	SelfBlockReason string         `json:"self_block_reason,omitempty"`
	RunBudget       int            `json:"run_budget"`
	LoopEndKinds    []string       `json:"loop_end_kinds"`
	HasProgressSpec bool           `json:"has_progress_spec"`
	ProgressAware   bool           `json:"progress_aware_wedge_detector"`
}

// ClassifyGardenerBlob computes both safety axes for a raw `gardener` JSON blob —
// the artifact `gb gardener start` / `gb trigger create` persist and the engine
// decodes at fire time. It is the single Go entry point any surveyor should use
// to get the full picture instead of the one-axis ValidateGardenerBlob.
//
// The backstop axis delegates to ValidateGardenerBlob so it stays byte-identical
// to the fire-time guard (empty / unparseable blobs are fail-closed there). The
// self-block axis decodes the same blob and mirrors the audit classifier.
func ClassifyGardenerBlob(raw string) GardenerSafetyVerdict {
	raw = strings.TrimSpace(raw)

	v := GardenerSafetyVerdict{SelfBlock: SelfBlockNone}
	if viol := ValidateGardenerBlob(raw); viol != nil {
		v.BackstopReason = viol.Error()
	} else {
		v.HasBackstop = true
	}

	spec, ok := decodeGardenerSpec(raw)
	if !ok {
		// Empty or unparseable blob — no gardener loop spec to score. The
		// backstop verdict above already fail-closes this case; mirror it on the
		// self-block axis so both axes agree "nothing here is bounded".
		v.SelfBlockReason = "no gardener loop spec"
		return v
	}
	classifyGardenerSelfBlock(spec, &v)
	return v
}

// ClassifyGardenerSafety computes both axes for an already-decoded GardenerSpec
// (the shape the engine holds at fire time). A nil spec is not a gardener —
// nothing to bound — reported as no-backstop / self-block none.
func ClassifyGardenerSafety(spec *GardenerSpec) GardenerSafetyVerdict {
	v := GardenerSafetyVerdict{SelfBlock: SelfBlockNone}
	if spec == nil {
		v.SelfBlockReason = "no gardener loop spec"
		v.BackstopReason = "no gardener loop spec"
		return v
	}
	if viol := ValidateGardenerSpec(spec); viol != nil {
		v.BackstopReason = viol.Error()
	} else {
		v.HasBackstop = true
	}
	classifyGardenerSelfBlock(spec, &v)
	return v
}

// classifyGardenerSelfBlock fills the self-block axis of v from spec, mirroring
// classify() in gardener-autoblock-audit.py exactly:
//
//   - run_budget = first positive of run_budget / max_runs;
//   - intelligent = any loop_end.any kind in IntelligentLoopEndKinds;
//   - progress_aware = a no_progress_for loop_end kind AND a non-empty progress
//     spec;
//   - none (offender)  when run_budget==0 AND not intelligent;
//   - weak             when it terminates but is not progress_aware;
//   - ok               when progress_aware.
func classifyGardenerSelfBlock(spec *GardenerSpec, v *GardenerSafetyVerdict) {
	v.RunBudget = firstPositiveGardenerInt(spec.RunBudget, spec.MaxRuns)

	var kinds []string
	if stop, err := scheduler.ParseStopSpec(string(spec.Stop)); err == nil {
		for _, e := range stop.LoopEnd.Any {
			kinds = append(kinds, e.Kind)
		}
	}
	v.LoopEndKinds = kinds
	v.HasProgressSpec = gardenerHasProgressSpec(spec)

	intelligent := false
	hasNoProgressFor := false
	for _, k := range kinds {
		if IntelligentLoopEndKinds[k] {
			intelligent = true
		}
		if k == scheduler.StopKindNoProgressFor {
			hasNoProgressFor = true
		}
	}
	v.ProgressAware = hasNoProgressFor && v.HasProgressSpec

	switch {
	case v.RunBudget == 0 && !intelligent:
		v.SelfBlock = SelfBlockNone
		v.SelfBlockReason = "no run_budget and no intelligent loop_end kind " +
			"(no_progress_for/bead_query/specific_bead_closed/max_runs) — this " +
			"gardener can only stop on a pod crash or a multi-week wall_clock"
	case !v.ProgressAware:
		v.SelfBlock = SelfBlockWeak
		v.SelfBlockReason = "terminates eventually (budget or goal-completion " +
			"stop) but has no progress-aware wedge detector (no_progress_for + " +
			"progress spec) — cannot detect 'crew keeps getting blocked, nothing " +
			"is closing'"
	default:
		v.SelfBlock = SelfBlockOK
		v.SelfBlockReason = "bounded + progress-aware"
	}
}

// gardenerHasProgressSpec reports whether the gardener declares a non-empty
// progress spec (needed for a no_progress_for wedge detector to score runs).
// Mirrors has_progress_spec() in the audit: an empty object / null counts as
// absent.
func gardenerHasProgressSpec(spec *GardenerSpec) bool {
	p := strings.TrimSpace(string(spec.Progress))
	return p != "" && p != "{}" && p != "null"
}

// firstPositiveGardenerInt returns the first argument greater than zero, or zero.
// Mirrors run_budget()'s (run_budget, max_runs) precedence in the audit.
func firstPositiveGardenerInt(vals ...int) int {
	for _, n := range vals {
		if n > 0 {
			return n
		}
	}
	return 0
}
