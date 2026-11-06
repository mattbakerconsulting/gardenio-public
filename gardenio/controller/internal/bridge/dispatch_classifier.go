// Gardener dispatch classifier (ka-0000003173, design ka-0000007305 §5).
//
// When a gardener finds a `ready` step (deps satisfied, unclaimed) it normally
// spawns one pod per step. This classifier inserts a pre-dispatch pass that
// recommends HOW to execute the step's fan-out:
//
//   - dynamic-workflow — one pod runs an in-session JS orchestration
//     (parallel()/pipeline()) over the sub-items, converging on a single
//     merge-gate. Cheap overhead: one pod cold-start, one credential context,
//     one bead.
//   - pod-fanout — N step beads, each claimed by its own pod (separate Claude
//     session, checkout, branch/MR, per-bead cost row). The right shape when
//     outputs must diverge.
//
// The recommendation is ADVISORY. The gardener (or the crew it spawns) reads it;
// nothing here forces an execution mode. The guardrail advice bead linked from
// the gardener formula via `extends` carries the human-readable rationale.
//
// # The signals
//
// An explicit `exec_hint` step field WINS outright when present and valid
// (values: dynamic-workflow | pod-fanout | separate-mrs). Absent that, the
// classifier scores four signals readable off the bead without spawning
// anything, and recommends dynamic-workflow when TWO OR MORE are positive:
//
//  1. uniform requires_skills — every projected sub-item needs the same skill
//     set (one image, one credential context → in-session fan-out is safe).
//  2. >=3 INBOUND fan-out items — see the timing correction below.
//  3. audit/sweep keyword — title/description matches the sweep vocabulary.
//  4. fan-out annotation — the step is explicitly marked as a fan-out step
//     (carries an on_complete / expand / loop spec, or a dwf-candidate label).
//
// # CRITICAL timing correction (ka-0000003173)
//
// Signal (2) counts the items threaded INTO this step — the array an UPSTREAM
// step produced and that this step will fan out over. It must NOT be read from
// "this step's own output": a `ready` step has not run yet, so its output /
// dwf_result is empty. Reading the output would make every ready fan-out step
// score 0 on the width signal and silently disable the heuristic. inboundFanoutItems
// therefore consults only inbound-threaded fields and never the result/output
// fields.

package bridge

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// ExecHint is the explicit per-step execution-shape annotation. When present
// and valid it overrides the four-signal score.
type ExecHint string

const (
	// ExecHintDynamicWorkflow → run the step as a single pod hosting an
	// in-session dynamic workflow.
	ExecHintDynamicWorkflow ExecHint = "dynamic-workflow"
	// ExecHintPodFanout → expand the step into N step beads, one pod each.
	ExecHintPodFanout ExecHint = "pod-fanout"
	// ExecHintSeparateMRs → like pod-fanout, emphasising that each sub-item
	// produces its own independently-reviewable MR. Treated as pod-fanout for
	// the binary dispatch recommendation, but preserved as a distinct hint so
	// downstream consumers can surface the MR-per-item intent.
	ExecHintSeparateMRs ExecHint = "separate-mrs"
)

// validExecHint reports whether s is a recognised exec_hint value.
func validExecHint(s string) (ExecHint, bool) {
	switch ExecHint(strings.TrimSpace(s)) {
	case ExecHintDynamicWorkflow:
		return ExecHintDynamicWorkflow, true
	case ExecHintPodFanout:
		return ExecHintPodFanout, true
	case ExecHintSeparateMRs:
		return ExecHintSeparateMRs, true
	default:
		return "", false
	}
}

// DispatchRecommendation is the classifier's binary verdict: run the step as a
// single in-session dynamic workflow, or fan out into separate pods.
type DispatchRecommendation string

const (
	DispatchDynamicWorkflow DispatchRecommendation = "dynamic-workflow"
	DispatchPodFanout       DispatchRecommendation = "pod-fanout"
)

// dispatchScoreThreshold is the number of positive signals (out of four)
// required to recommend a dynamic workflow when no explicit exec_hint is set.
const dispatchScoreThreshold = 2

// DispatchSignals records the per-signal evaluation behind a recommendation.
// It is attached to logs and (optionally) stamped onto the spawned agent so an
// operator can see WHY a step was classified the way it was.
type DispatchSignals struct {
	// ExecHint is the explicit annotation when present; "" otherwise.
	ExecHint ExecHint
	// FromExecHint is true when the recommendation came from an explicit
	// exec_hint rather than the scored signals.
	FromExecHint bool

	// The four scored signals (only meaningful when FromExecHint is false).
	UniformSkills bool // (1) uniform requires_skills across projected sub-items
	InboundFanout bool // (2) >=3 inbound fan-out items
	InboundCount  int  // resolved inbound item count (for diagnostics)
	Keyword       bool // (3) audit/sweep/classify/... keyword in title/desc
	Annotation    bool // (4) step carries a fan-out annotation

	// Score is the number of positive scored signals.
	Score int
}

// inboundFanoutThreshold is the minimum inbound item count for signal (2).
const inboundFanoutThreshold = 3

// keywordPattern matches the sweep / fan-out vocabulary in a step's title or
// description (signal 3). Mirrors the design doc §5.1 list. Case-insensitive.
var keywordPattern = regexp.MustCompile(
	`(?i)\b(audit|sweep|sanitize|classify|per-file|per-component|embarrassingly[ -]parallel|fan-?out)\b`,
)

// ClassifyStepDispatch evaluates a ready step bead and returns a dispatch
// recommendation plus the per-signal breakdown.
//
// Decision order:
//  1. A valid `exec_hint` field wins outright (FromExecHint=true).
//  2. Otherwise score the four signals; >=dispatchScoreThreshold positive ⇒
//     dynamic-workflow, else pod-fanout.
//
// The function is pure (no daemon I/O) so it is fully table-testable; the
// caller is responsible for fetching the bead. A nil step yields a pod-fanout
// recommendation with an empty signal set (the conservative default — never
// recommend collapsing work we cannot inspect into one pod).
func ClassifyStepDispatch(step *beadsapi.BeadDetail) (DispatchRecommendation, DispatchSignals) {
	if step == nil {
		return DispatchPodFanout, DispatchSignals{}
	}

	// (0) Explicit exec_hint wins.
	if hint, ok := validExecHint(step.Fields["exec_hint"]); ok {
		sig := DispatchSignals{ExecHint: hint, FromExecHint: true}
		if hint == ExecHintDynamicWorkflow {
			return DispatchDynamicWorkflow, sig
		}
		// pod-fanout and separate-mrs both fan out into pods.
		return DispatchPodFanout, sig
	}

	var sig DispatchSignals

	// (1) Uniform requires_skills. A step that fans out via a single
	// on_complete/expand formula yields sub-items that all inherit the same
	// requires_skills, so a non-empty, single skill set is the readable proxy
	// for "uniform across projected sub-items".
	skills := requiresSkills(step)
	sig.UniformSkills = len(skills) > 0

	// (2) Inbound fan-out width — items threaded INTO this step (NOT its own
	// empty output; see the timing correction in the package doc).
	sig.InboundCount = len(inboundFanoutItems(step))
	sig.InboundFanout = sig.InboundCount >= inboundFanoutThreshold

	// (3) Keyword scan of title + description.
	sig.Keyword = keywordPattern.MatchString(step.Title + "\n" + step.Description)

	// (4) Fan-out annotation: the step is explicitly marked as a fan-out step.
	sig.Annotation = hasFanoutAnnotation(step)

	for _, ok := range []bool{sig.UniformSkills, sig.InboundFanout, sig.Keyword, sig.Annotation} {
		if ok {
			sig.Score++
		}
	}

	if sig.Score >= dispatchScoreThreshold {
		return DispatchDynamicWorkflow, sig
	}
	return DispatchPodFanout, sig
}

// requiresSkills extracts the step's required skills, preferring the
// `requires_skills` field (JSON array) and falling back to `skill:<name>`
// labels (how kd formula apply materialises step skills).
func requiresSkills(step *beadsapi.BeadDetail) []string {
	if raw := strings.TrimSpace(step.Fields["requires_skills"]); raw != "" {
		if vals, ok := parseStringArray(raw); ok && len(vals) > 0 {
			return vals
		}
	}
	var skills []string
	for _, l := range step.Labels {
		if s := strings.TrimPrefix(l, "skill:"); s != l && s != "" {
			skills = append(skills, s)
		}
	}
	return skills
}

// inboundFanoutItems returns the items threaded INTO this step by an upstream
// step — the array this step will fan out over.
//
// It reads, in priority order:
//   - fields["inbound_items"]  — explicit inbound array (JSON or CSV)
//   - fields["fanout_items"]   — alias
//   - the step's own on_complete.expand_vars values that reference a resolved
//     inbound list (rare; included for completeness)
//
// It deliberately NEVER reads `dwf_result`, `output`, or any *.output field:
// those are the step's OWN output, which is empty for a ready (not-yet-run)
// step. Reading them is the timing bug this classifier exists to avoid
// (ka-0000003173).
func inboundFanoutItems(step *beadsapi.BeadDetail) []string {
	for _, key := range []string{"inbound_items", "fanout_items"} {
		if raw := strings.TrimSpace(step.Fields[key]); raw != "" {
			if vals, ok := parseStringArray(raw); ok {
				return vals
			}
		}
	}
	return nil
}

// hasFanoutAnnotation reports whether the step is explicitly annotated as a
// fan-out step: it carries an on_complete / expand / loop spec, or a
// dwf-candidate / fan-out label. This is signal (4) — distinct from exec_hint,
// which short-circuits before scoring.
func hasFanoutAnnotation(step *beadsapi.BeadDetail) bool {
	for _, key := range []string{"on_complete", "expand", "loop"} {
		if strings.TrimSpace(step.Fields[key]) != "" {
			return true
		}
	}
	for _, l := range step.Labels {
		switch l {
		case "dwf-candidate", "fan-out", "fanout":
			return true
		}
	}
	return false
}

// parseStringArray parses either a JSON string array (`["a","b"]`) or a
// comma-separated list (`a, b, c`) into a trimmed, non-empty slice. The bool
// reports whether any value was parsed.
func parseStringArray(raw string) ([]string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	if strings.HasPrefix(raw, "[") {
		var arr []string
		if err := json.Unmarshal([]byte(raw), &arr); err == nil {
			out := trimNonEmpty(arr)
			return out, len(out) > 0
		}
		// Fall through to CSV on malformed JSON.
	}
	out := trimNonEmpty(strings.Split(raw, ","))
	return out, len(out) > 0
}

// stampDispatchRecommendation writes the classifier verdict and the signal
// breakdown onto the extraFields map handed to SpawnAgent. These advisory
// fields ride on the spawned agent bead so gb prime / the agent card can
// surface WHY a step was classified the way it was. All values are strings (the
// SpawnAgent extra-field contract).
func stampDispatchRecommendation(fields map[string]string, rec DispatchRecommendation, sig DispatchSignals) {
	if fields == nil {
		return
	}
	fields["dispatch_recommendation"] = string(rec)
	if sig.FromExecHint {
		fields["dispatch_signal_source"] = "exec_hint"
		fields["dispatch_exec_hint"] = string(sig.ExecHint)
		return
	}
	fields["dispatch_signal_source"] = "score"
	fields["dispatch_score"] = strconv.Itoa(sig.Score)
	fields["dispatch_signals"] = fmt.Sprintf(
		"uniform_skills=%t,inbound_fanout=%t(%d),keyword=%t,annotation=%t",
		sig.UniformSkills, sig.InboundFanout, sig.InboundCount, sig.Keyword, sig.Annotation)
}

// dispatchNudge returns a one-line, prompt-prependable nudge describing the
// recommendation, or "" when there is nothing actionable to say. The trailing
// blank line separates it from the gardener's own prompt body.
func dispatchNudge(rec DispatchRecommendation, sig DispatchSignals) string {
	var why string
	if sig.FromExecHint {
		why = fmt.Sprintf("exec_hint=%s", sig.ExecHint)
	} else {
		why = fmt.Sprintf("%d/4 signals", sig.Score)
	}
	switch rec {
	case DispatchDynamicWorkflow:
		return fmt.Sprintf(
			"[dispatch hint: DYNAMIC-WORKFLOW — %s] This step's sub-items look "+
				"embarrassingly parallel and convergent. Prefer running them as ONE pod "+
				"via a dynamic workflow (parallel()/pipeline() + a single merge-gate) "+
				"rather than fanning out N step beads. See the dynamic-workflow guardrail "+
				"advice for when to override.\n\n", why)
	case DispatchPodFanout:
		return fmt.Sprintf(
			"[dispatch hint: POD-FANOUT — %s] This step's sub-items look divergent "+
				"(separate MRs / worktrees / per-item beads) or too few to batch. Prefer "+
				"fanning out separate step beads/pods so each gets its own bead, branch, "+
				"and cost row. See the dynamic-workflow guardrail advice.\n\n", why)
	default:
		return ""
	}
}

// trimNonEmpty trims each element and drops empties.
func trimNonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
