// Package bridge — gardener standing-marker guard (ka-0000004311).
//
// The engine recognizes a STANDING (perpetual) gardener by exactly two
// MACHINE-READABLE signals (gardenerHasStandingMarker): the gardener blob's
// `standing:true`, or the trigger bead's `perpetual:true` label. But gardener
// AUTHORS also express standing-ness in a THIRD place the engine never reads —
// the `terminal_goal` PROSE ("STANDING groomer gardener: … does NOT
// self-complete on a momentarily-empty lane"). A gardener that self-describes as
// perpetual in prose while carrying NEITHER marker is silently treated as an
// ordinary epic-bounded gardener, and BOTH standing protections invert:
//
//   - gardener_stop.go — the IsStanding no-op that suppresses the max_runs
//     loop_end never applies, so the run-count ceiling trips (the exact bug
//     ka-0000004754 was written to prevent), and
//   - gardener_evaluator.go — on resume-budget exhaustion the gardener takes the
//     escalate+CLOSE branch instead of gardenerRearmStandingInPlace (the exact
//     ka-0000006241 reaping that "systematically reaped exactly the gardeners
//     that must never die").
//
// The result is the closed-but-enabled dark shape (enabled=true, status=closed)
// that is invisible to the engine liveness sweep — a cohort of 11 standing
// gardeners stayed dark for ~11 days with no alert, because the two mechanisms
// that should have caught it (the fleet-liveness guard + the resumer watchdog)
// were themselves in the reaped cohort.
//
// This guard closes the prose/marker gap the same way the bounded-stop guard
// (gardener_bounds_guard.go) closes the unbounded-loop gap, with the SAME
// asymmetric rollout the operator settled on in decision ka-0000003439:
//
//   - AUTHORING time — `gb gardener start` / `gb trigger create` REJECT (exit
//     non-zero, write nothing) a gardener whose terminal_goal declares standing
//     while carrying no marker. A silent 11-day outage becomes a loud authoring
//     error the author fixes in one line (add `standing:true` to the blob, or
//     the `perpetual:true` label, or rephrase the goal if it is NOT perpetual).
//   - FIRING time — the engine WARNS (warn-once) so a live gardener that predates
//     this guard is NOT disabled mid-flight; auto-modifying a live gardener bead
//     is exactly the blast-radius the operator declined. The fleet-wide stop
//     stays the explicit `gardeners-pause` kill-switch.
//
// Detection is deliberately HIGH-PRECISION, not the reporter's broad
// /standing|perpetual/i sweep: that broad regex false-positives on the
// incidental adjective in an epic-bounded goal ("… WS1 (standing warm runsc
// node …)" on gardener ka-0000003543, whose goal is genuinely "All children of
// the epic are closed AND …"). Auto-stamping perpetual:true on such a gardener
// would invert the protection the OTHER way — pin a gardener that MUST terminate
// into a perpetual loop. So the detector matches a DECLARATION (the
// standing/perpetual word bound to a gardener-nature noun, or an explicit
// "does-not-self-terminate" phrase), never a bare adjective.
package bridge

import (
	"fmt"
	"regexp"
	"strings"
)

// standingDeclRe matches a terminal_goal that DECLARES the gardener a standing /
// perpetual loop: the word "standing" or "perpetual" bound (within a few words)
// to a gardener-nature noun — "STANDING groomer gardener", "perpetual loop",
// "STANDING auto-block hygiene sweep", "STANDING liveness sentinel". The bounded
// {0,5}-word gap is what separates a real declaration from the incidental
// adjective in "standing warm runsc node" (no nature noun follows within the
// window), so the epic-bounded false-positive ka-0000003543 is NOT matched.
var standingDeclRe = regexp.MustCompile(
	`(?i)\b(?:standing|perpetual)\b(?:\W+\w+){0,5}\W+(?:gardener|loop|sweep|sweeper|sentinel|watchdog|janitor|groomer|daemon|monitor|guard)\b`)

// standingNeverTerminateRe matches an explicit "this loop does not self-
// terminate" declaration — the OTHER way authors express perpetual intent,
// independent of the standing/perpetual word. These phrases NEGATE self-
// completion, which is precisely what distinguishes a standing gardener from an
// epic-bounded one (an epic-bounded gardener DOES self-complete when its epic
// drains). Kept tight to unambiguous negations so an ordinary "does not stop on
// a momentarily-empty queue"-style epic-bounded goal is not swept in. Catches
// the marker-less dailbert gardener ka-0000007197 ("Loop persists …; does NOT
// self-close on an empty queue"), which carries no standing/perpetual word.
var standingNeverTerminateRe = regexp.MustCompile(
	`(?i)(does\s+not\s+self-complete|does\s+not\s+self-close|never\s+self-declares|no\s+wall_clock\s+self-completion|loop\s+persists)`)

// TerminalGoalDeclaresStanding reports whether a gardener's terminal_goal PROSE
// declares the gardener to be a standing (perpetual) loop, and returns the
// matched declaration text so callers can name it in the guard message. An
// empty goal, or a goal that only uses "standing"/"perpetual" as an incidental
// adjective (no bound nature noun, no self-termination negation), returns
// (false, "").
func TerminalGoalDeclaresStanding(goal string) (declared bool, matched string) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return false, ""
	}
	if m := standingDeclRe.FindString(goal); m != "" {
		return true, strings.Join(strings.Fields(m), " ")
	}
	if m := standingNeverTerminateRe.FindString(goal); m != "" {
		return true, strings.Join(strings.Fields(m), " ")
	}
	return false, ""
}

// StandingMarkerViolation describes a gardener whose terminal_goal declares
// standing/perpetual intent but carries no machine-readable standing marker
// (neither the blob's standing:true nor the perpetual:true label). A nil
// *StandingMarkerViolation means there is no mismatch — the gardener either does
// not declare standing in prose, or declares it AND carries a marker.
type StandingMarkerViolation struct {
	// MatchedProse is the offending declaration text found in the terminal_goal
	// (e.g. "STANDING groomer gardener", "does NOT self-close"), for the message.
	MatchedProse string
}

// Error renders an actionable, operator-facing rejection naming the offending
// prose and the three ways to resolve it. Implements error so authoring-time
// callers can return it directly.
func (v *StandingMarkerViolation) Error() string {
	if v == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("gardener terminal_goal DECLARES standing/perpetual but carries NO standing marker — refusing (bug ka-0000004311).\n")
	fmt.Fprintf(&b, "  The goal says %q, but the engine reads standing-ness ONLY from machine markers,\n", v.MatchedProse)
	b.WriteString("  NEVER from terminal_goal prose. Without a marker the engine treats this as an\n")
	b.WriteString("  ordinary epic-bounded gardener: the max_runs ceiling trips (ka-0000004754) and the\n")
	b.WriteString("  resume give-up path REAPS it (ka-0000006241) — a closed-but-enabled dark gardener.\n")
	b.WriteString("  Resolve ONE of:\n")
	b.WriteString("    • add \"standing\":true to the gardener blob, OR\n")
	b.WriteString("    • attach the `perpetual:true` label (gb gardener start --label perpetual:true), OR\n")
	b.WriteString("    • rephrase terminal_goal if this gardener is NOT perpetual (it self-completes on an\n")
	b.WriteString("      epic drain / condition) — an incidental adjective like \"standing warm node\" should\n")
	b.WriteString("      not read as a standing DECLARATION.\n")
	return strings.TrimRight(b.String(), "\n")
}

// ValidateGardenerStandingMarker is the fail-closed check. Given a decoded
// gardener spec and the trigger bead's label set, it returns a non-nil
// *StandingMarkerViolation exactly when the spec's terminal_goal declares
// standing/perpetual intent AND no machine-readable standing marker is present.
// A nil spec is not a gardener (nothing to check). This is the single source of
// truth shared by the authoring-time CLI rejects and the firing-time engine
// warn, so the two can never disagree on what "declares standing" means.
func ValidateGardenerStandingMarker(spec *GardenerSpec, labels []string) *StandingMarkerViolation {
	if spec == nil {
		return nil
	}
	declared, matched := TerminalGoalDeclaresStanding(spec.TerminalGoal)
	if !declared {
		return nil
	}
	if gardenerHasStandingMarker(spec, labels) {
		return nil
	}
	return &StandingMarkerViolation{MatchedProse: matched}
}

// ValidateGardenerStandingBlob applies the standing-marker guard to a raw
// `gardener` JSON blob plus the trigger bead's labels — the authoring-time entry
// point that mirrors ValidateGardenerBlob. It decodes the blob so the CLI guard
// validates the EXACT bytes that will be persisted. An empty blob is not a
// gardener (returns nil — the caller gates on non-empty). A non-empty but
// unparseable blob returns nil here (the bounded-stop guard already fails closed
// on an unparseable blob; this guard only speaks to the prose/marker mismatch).
func ValidateGardenerStandingBlob(raw string, labels []string) *StandingMarkerViolation {
	spec, ok := decodeGardenerSpec(raw)
	if !ok {
		return nil
	}
	return ValidateGardenerStandingMarker(spec, labels)
}
