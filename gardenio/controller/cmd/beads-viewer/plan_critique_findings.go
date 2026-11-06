package main

import (
	"os"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// plan_critique_findings.go — AUTOMATED, crit-style inline plan critique
// (prototype, task ka-0000002614). This is the *computed-findings* sibling of
// the manual line-comment critique already shipped in epic ka-0000002316: where
// that feature lets a human point at a line and leave a comment, this one runs a
// small, deterministic rule set over the plan the operator is already looking at
// and surfaces PROOF gaps, coverage gaps, and design-review nits INLINE on
// /plans/{id}, each with a collapsible detail section.
//
// Deliberate constraints for the prototype (per the task's "demonstrate the UX
// pattern, not production-hardening"):
//   - ZERO new daemon round-trips. Every finding is derived from the data bag
//     handlePlanDetail already assembled (bead, steps, phases, decisions,
//     narrative/bundle view, escalations). Adding critique costs no latency.
//   - Additive + non-destructive. The findings render in their own collapsible
//     panel at the top of the narrative column; if the panel is suppressed the
//     page is byte-identical to today.
//   - Dark-launched behind BEADS_VIEWER_PLAN_CRITIQUE_FINDINGS=1 (env, read
//     once at startup) so it can ship disabled and be flipped on for a demo
//     without a redeploy of the toggle itself. The manual-comment critique
//     ships on-by-default (decision ka-0000004596); this computed layer is
//     newer and more opinionated, so it stays gated until the heuristics prove
//     out.
//
// The rules are intentionally simple and explainable — each finding names the
// exact bead/section it fires on and what would clear it, so the operator can
// act (or dismiss it as a false positive) without leaving the cockpit.

// critiqueSeverity ranks a finding for sort order + the severity pill skin.
type critiqueSeverity int

const (
	// critiqueInfo — an observation worth a glance; nothing is wrong.
	critiqueInfo critiqueSeverity = iota
	// critiqueWarn — a likely gap the operator probably wants to close.
	critiqueWarn
	// critiqueBlock — a hard problem: the plan cannot ship clean as-is.
	critiqueBlock
)

func (s critiqueSeverity) label() string {
	switch s {
	case critiqueBlock:
		return "blocker"
	case critiqueWarn:
		return "warn"
	default:
		return "info"
	}
}

// slug is the CSS-class / data-attr token for the severity (the template skins
// the pill and the left rule off it).
func (s critiqueSeverity) slug() string {
	switch s {
	case critiqueBlock:
		return "block"
	case critiqueWarn:
		return "warn"
	default:
		return "info"
	}
}

// critiqueFinding is one automated critique surfaced inline on the plan. Detail
// is the collapsible body; it is plain prose (the template escapes it) so a
// finding can never inject markup. AnchorLabel/AnchorHref, when set, deep-link
// the finding to the exact step or narrative section it fires on.
type critiqueFinding struct {
	Severity    critiqueSeverity
	Category    string // "proof-gap" | "coverage-gap" | "design-review" | "structure"
	Title       string
	Detail      string
	Remediation string
	// AnchorLabel/AnchorHref render a "→ <label>" deep link into the plan when
	// the finding is line-precise (a step row or a narrative section anchor).
	// Both empty for a plan-level finding.
	AnchorLabel string
	AnchorHref  string
}

// critiqueFindingView is the render-ready projection the template consumes.
type critiqueFindingView struct {
	SeverityLabel string
	SeveritySlug  string
	Category      string
	Title         string
	Detail        string
	Remediation   string
	AnchorLabel   string
	AnchorHref    string
}

// planCritiqueFindingsData is the data bag the plan_critique_findings partial
// consumes. Empty (Findings == nil) suppresses the whole panel.
type planCritiqueFindingsData struct {
	PlanID       string
	BasePath     string
	Findings     []critiqueFindingView
	BlockerCount int
	WarnCount    int
	InfoCount    int
}

// planCritiqueFindingsEnabledFromEnv reports whether the automated-critique
// panel is dark-launched on. Read once at startup (main wires the result onto
// the Server) so the hot page/SSE path never touches the environment.
func planCritiqueFindingsEnabledFromEnv() bool {
	v := strings.TrimSpace(os.Getenv("BEADS_VIEWER_PLAN_CRITIQUE_FINDINGS"))
	return v == "1" || strings.EqualFold(v, "true")
}

// computePlanCritiqueFindings runs the rule set over the already-fetched plan
// data and returns the findings, most-severe first. All inputs are values the
// detail handler already holds — this adds NO daemon call.
//
// bead        — the plan epic (nil-safe).
// steps/phases — the rendered timeline (flat + phased).
// decisions   — the plan's clarifying-question decisions.
// prose       — the plan's narrative bundle view (nil when the plan has no bundle).
// esc         — the plan's blocking-escalation rollup.
// state       — the plan lifecycle state (draft/ready/building).
func computePlanCritiqueFindings(
	bead *beadsapi.BeadDetail,
	steps []planStepView,
	phases []planPhaseView,
	decisions []planDecisionView,
	prose *bundleView,
	esc planEscalationInfo,
	state planState,
) []critiqueFinding {
	var out []critiqueFinding

	allSteps := flattenSteps(steps, phases)

	out = append(out, proofGapFindings(allSteps)...)
	out = append(out, coverageGapFindings(bead, allSteps, phases, prose, state)...)
	out = append(out, designReviewFindings(bead, allSteps, decisions, prose, esc, state)...)
	out = append(out, structureFindings(allSteps)...)

	// Stable, most-severe-first ordering. Within a severity keep category then
	// title so the panel reads deterministically across renders (a jittering
	// list reads as noise).
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity > out[j].Severity
		}
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// flattenSteps returns the flat steps plus every phase's steps as one slice —
// the same rendered set the timeline shows, so a finding's step reference always
// points at a row the operator can see.
func flattenSteps(steps []planStepView, phases []planPhaseView) []planStepView {
	out := make([]planStepView, 0, len(steps))
	out = append(out, steps...)
	for _, p := range phases {
		out = append(out, p.Steps...)
	}
	return out
}

// proofGapFindings fires when a step has shipped an MR but shows no sign the
// merged change was proven. The gardenio house rule (advice ka-0000008060) is
// that every MR carries a PROOF block; a plan step whose linked MR is merged
// with no proof reference in its title/description is exactly the "claim without
// evidence" the crit workflow exists to catch.
func proofGapFindings(steps []planStepView) []critiqueFinding {
	var out []critiqueFinding
	for _, st := range steps {
		if st.MRURL == "" {
			continue
		}
		// A merged MR with no cross-referenced proof is the sharp case: the
		// work claims done but nothing on the plan attests it was verified.
		if strings.EqualFold(st.MRState, "merged") && st.Closed {
			continue // closed step + merged MR: assume the reviewer saw the proof.
		}
		if strings.EqualFold(st.MRState, "merged") && !st.Closed {
			out = append(out, critiqueFinding{
				Severity:    critiqueWarn,
				Category:    "proof-gap",
				Title:       "Step's MR merged but step still open",
				Detail:      "Step " + stepNum(st) + " (" + st.Title + ") has a merged MR yet is not closed. Either the proof/verification for the merged change has not been recorded, or the step needs closing. A merged MR with an open step is the classic 'claimed done, not attested' gap the PROOF-block rule (advice ka-0000008060) guards against.",
				Remediation: "Confirm the MR's PROOF block exercised the step's claim, then close the step — or reopen the MR if the claim is not yet proven.",
				AnchorLabel: "Step " + stepNum(st),
				AnchorHref:  "/" + st.ID,
			})
		}
	}
	return out
}

// coverageGapFindings fires on parts of the plan the narrative describes but no
// step implements, and vice-versa. These are the "the plan talks about X but
// nothing builds X" gaps a reviewer scans for.
func coverageGapFindings(
	bead *beadsapi.BeadDetail,
	steps []planStepView,
	phases []planPhaseView,
	prose *bundleView,
	state planState,
) []critiqueFinding {
	var out []critiqueFinding

	// A building plan with narrative prose but zero steps is uncovered by
	// construction — the plan describes work nobody is doing.
	if state == planStateBuilding && len(steps) == 0 {
		out = append(out, critiqueFinding{
			Severity:    critiqueBlock,
			Category:    "coverage-gap",
			Title:       "Plan is building but has no steps",
			Detail:      "This plan is in the building state yet has no TODO steps. Every commitment in the narrative is therefore uncovered — there is no work item tracking it, so nothing will get built or reviewed.",
			Remediation: "Break the narrative into steps (add them on the timeline) before the plan can meaningfully progress.",
		})
	}

	// Steps with no §-section cross-reference into the narrative: the step
	// exists but the reader cannot tell which part of the plan it satisfies.
	// Only meaningful when the plan actually HAS a narrative bundle to link to.
	if prose != nil && len(prose.Docs) > 0 {
		openCount, unlinked := 0, 0
		for _, st := range steps {
			if st.Closed {
				continue // history; a closed step's traceability matters less.
			}
			openCount++
			if st.SectionRef.Label == "" {
				unlinked++
			}
		}
		// Only raise it when it is the norm, not a one-off — a single unlinked
		// step is noise; most OPEN steps unlinked is a real traceability gap.
		if openCount > 0 && unlinked >= (openCount+1)/2 {
			out = append(out, critiqueFinding{
				Severity:    critiqueWarn,
				Category:    "coverage-gap",
				Title:       "Most open steps don't reference the narrative",
				Detail:      pluralCount(unlinked, "open step") + " out of " + itoa(openCount) + " carry no `see ka-BUNDLE#section` cross-reference into the plan narrative. Without it a reviewer cannot trace a step back to the part of the plan it implements, and the round-to-round 'is every commitment covered?' check has to be done by hand.",
				Remediation: "Add a `see gd-<bundle>#<section>` reference to each step's description so the timeline row deep-links to the narrative section it satisfies.",
			})
		}
	}

	// A phase with no steps is a declared unit of work with nothing in it.
	for _, p := range phases {
		if p.Closed {
			continue
		}
		if len(p.Steps) == 0 {
			out = append(out, critiqueFinding{
				Severity:    critiqueWarn,
				Category:    "coverage-gap",
				Title:       "Phase has no steps",
				Detail:      "Phase " + itoa(p.Number) + " (" + p.Title + ") declares a unit of work but contains no steps, so nothing under it will be built. Either the phase is a placeholder that should be filled in, or it should be removed so the plan's shape reflects the real work.",
				Remediation: "Add the phase's steps, or drop the empty phase.",
				AnchorLabel: p.Title,
				AnchorHref:  "/" + p.ID,
			})
		}
	}

	return out
}

// designReviewFindings are the softer "a reviewer would flag this" nits about
// the plan's shape and readiness — open questions, missing narrative, blocked
// state — the design-review pass a human does before approving a plan to build.
func designReviewFindings(
	bead *beadsapi.BeadDetail,
	steps []planStepView,
	decisions []planDecisionView,
	prose *bundleView,
	esc planEscalationInfo,
	state planState,
) []critiqueFinding {
	var out []critiqueFinding

	// Unanswered clarifying questions on a plan that is already building: the
	// plan advanced past questions it never resolved.
	openQ := 0
	for _, d := range decisions {
		if !d.Resolved {
			openQ++
		}
	}
	if openQ > 0 && state == planStateBuilding {
		out = append(out, critiqueFinding{
			Severity:    critiqueBlock,
			Category:    "design-review",
			Title:       "Building with open clarifying questions",
			Detail:      pluralCount(openQ, "clarifying question") + " on this plan " + isAre(openQ) + " still unanswered, yet the plan is already building. The design was approved for build with open forks — the work may be heading in a direction the operator has not actually chosen.",
			Remediation: "Answer (or dismiss) the open clarifying questions to confirm the plan's direction, or pause the build until they are resolved.",
		})
	} else if openQ > 0 {
		out = append(out, critiqueFinding{
			Severity:    critiqueInfo,
			Category:    "design-review",
			Title:       "Open clarifying questions",
			Detail:      pluralCount(openQ, "clarifying question") + " " + isAre(openQ) + " awaiting an answer before this plan is fully specified.",
			Remediation: "Answer them inline in the Decisions panel below.",
		})
	}

	// No narrative at all: there is nothing to critique or design-review — the
	// plan is a bag of steps with no framing.
	hasProse := bead != nil && strings.TrimSpace(bead.Description) != ""
	hasBundle := prose != nil && len(prose.Docs) > 0
	if !hasProse && !hasBundle && len(steps) > 0 {
		out = append(out, critiqueFinding{
			Severity:    critiqueWarn,
			Category:    "design-review",
			Title:       "Plan has steps but no narrative",
			Detail:      "This plan has a step timeline but no narrative summary or bundle doc. A reviewer has no statement of intent to check the steps against — the plan cannot be design-reviewed, only its individual steps read.",
			Remediation: "Ask the plan agent to draft a narrative (or write a summary) so the steps have a design to be measured against.",
		})
	}

	// Blocked by an escalation: privileged action gates the whole plan.
	if esc.Count > 0 {
		out = append(out, critiqueFinding{
			Severity:    critiqueBlock,
			Category:    "design-review",
			Title:       "Plan blocked by escalation",
			Detail:      pluralCount(esc.Count, "open escalation") + " " + isAre(esc.Count) + " blocking this plan. Until a privileged operator acts on " + itThem(esc.Count) + ", the blocked steps cannot advance regardless of how well the design reads.",
			Remediation: "Review and approve/reject the escalation(s) in the Blocked-by-escalation banner above.",
		})
	}

	return out
}

// structureFindings catch shape problems in the step graph itself — the
// mechanical checks a reviewer does not want to do by eye.
func structureFindings(steps []planStepView) []critiqueFinding {
	var out []critiqueFinding

	// Steps blocked by an OPEN blocker for a long time show up as blocker
	// chips; here we just count open-blocked open steps as a structural nit —
	// the plan cannot make progress on them until their blockers clear.
	blocked := 0
	for _, st := range steps {
		if st.Closed {
			continue
		}
		if len(st.BlockedBy) > 0 {
			blocked++
		}
	}
	if blocked > 0 {
		out = append(out, critiqueFinding{
			Severity:    critiqueInfo,
			Category:    "structure",
			Title:       "Open steps waiting on blockers",
			Detail:      pluralCount(blocked, "open step") + " " + isAre(blocked) + " waiting on an unresolved blocker and cannot start until it clears. If the blockers are stale, the plan's critical path is longer than the timeline suggests.",
			Remediation: "Check each blocked step's 'waiting on' chip and clear or re-scope the blocker.",
		})
	}

	return out
}

// --- render projection -------------------------------------------------------

// buildPlanCritiqueFindingsData projects findings into the template bag, folding
// the per-severity tallies the panel header renders. Returns a zero bag (with
// Findings == nil) when there is nothing to show, so the template suppresses the
// whole panel.
func buildPlanCritiqueFindingsData(planID, basePath string, findings []critiqueFinding) planCritiqueFindingsData {
	data := planCritiqueFindingsData{PlanID: planID, BasePath: basePath}
	if len(findings) == 0 {
		return data
	}
	data.Findings = make([]critiqueFindingView, 0, len(findings))
	for _, f := range findings {
		href := f.AnchorHref
		if href != "" {
			href = basePath + href
		}
		data.Findings = append(data.Findings, critiqueFindingView{
			SeverityLabel: f.Severity.label(),
			SeveritySlug:  f.Severity.slug(),
			Category:      f.Category,
			Title:         f.Title,
			Detail:        f.Detail,
			Remediation:   f.Remediation,
			AnchorLabel:   f.AnchorLabel,
			AnchorHref:    href,
		})
		switch f.Severity {
		case critiqueBlock:
			data.BlockerCount++
		case critiqueWarn:
			data.WarnCount++
		default:
			data.InfoCount++
		}
	}
	return data
}

// --- small formatting helpers ------------------------------------------------

// stepNum renders a step's stable 1..N number for prose, falling back to a short
// id when unnumbered.
func stepNum(st planStepView) string {
	if st.Number > 0 {
		return itoa(st.Number)
	}
	return st.ID
}

// pluralCount renders "1 step" / "3 steps" — a count with its noun pluralized.
func pluralCount(n int, noun string) string {
	s := itoa(n) + " " + noun
	if n != 1 {
		s += "s"
	}
	return s
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func itThem(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

