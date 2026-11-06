package main

// CED Efficiency token-cost hard-cap (Token-eff 3, ka-0000007291).
//
// The CED session-inspector (formula ka-0000004287) grades a real agent session
// on Correctness / Efficiency / Discoverability. This file folds token spend in
// as a DISTINCT SIGNAL ON the existing Efficiency dimension — not a separate 4th
// dimension — so the 7-bead molecule keeps its shape.
//
// Split of responsibility mirrors the fleet ethos:
//   - MEASURE step (fetch-and-measure): records task-1's objective TokenSignals
//     block with NO scoring. Purely objective counters.
//   - JUDGE step (judge): invokes evaluateEfficiencyCap here to decide, from
//     those objective counters, whether token bloat CAPS the Efficiency score.
//
// The cap is a PURE FUNCTION of the objective counters, so it is reproducible:
// two graders running `gb session analyze <jsonl> --efficiency-cap` on the same
// session get byte-identical verdicts. That determinism is what the fixture unit
// tests pin (a known-bloated JSONL fires; a healthy one does not).
//
// Cost-source note: every threshold below compares a token COUNT or a
// token/token RATIO against a documented constant. There is deliberately NO
// token×rate product and NO USD figure anywhere in this file — authoritative
// cost stays with LiteLLM, and the cost-source AST guard
// (internal/metrics/cost_source_guard_test.go) stays green.

// Documented thresholds for the Efficiency token-cost hard-cap. These are the
// single source of truth: gardenio/docs/ced-token-cost-rubric.md and the unit
// tests cite the same numbers so the rubric is reproducible and auditable.
const (
	// efficiencyCapScore is the value the Efficiency dimension (0-10) is capped
	// at when ANY token-cost condition fires. 6 matches the existing
	// redundant_repeat_queries / brute_force_scans efficiency caps in the rubric
	// and keeps the dimension at-or-below the file-bugs threshold (a dimension
	// scoring <= 6 gets one bug filed), so token bloat reliably surfaces a bug.
	efficiencyCapScore = 6

	// cacheHitFloor: a LONG session whose cache-hit ratio falls below this floor
	// is thrashing context — re-priming/rebuilding instead of riding cache. This
	// is a context-composition defect. Tag: data-plane (crew-fixable).
	cacheHitFloor = 0.60

	// longSessionMinTurns gates the cache-hit floor. Short sessions are cold by
	// nature — their first turns are all fresh input / cache creation — so the
	// ratio is only meaningful once a session has run long enough to benefit from
	// cache. This is the "documented floor for a long session" the rubric calls
	// for; a short session is never capped on cache-hit alone.
	longSessionMinTurns = 20

	// redundantSpikeCap: two or more re-prime balloons (turns whose cache
	// creation exceeds 2x the running median write, per task-1's
	// redundant_context_spikes counter) is a REPEATED bloated-context / redundant
	// payload defect — one spike can be legitimate, a habit of them is not.
	// Tag: data-plane (crew-fixable).
	redundantSpikeCap = 2

	// verbosityCeiling: mean output tokens per request above this is a verbose
	// output habit. Output is billed at the top rate and is one of the two
	// dominant cost levers (with cache_read), but trimming it is agent education,
	// not a code fix. Tag: agent-behavior (advice-only, never crew-fixed).
	verbosityCeiling = 8000.0
)

// Finding tags route remediation (task spec + formula step 6 / step "judge"):
// data-plane defects become crew fix-tasks; agent-behavior findings become
// advice-only observations and are NEVER crew-fixed.
const (
	tagDataPlane     = "data-plane"
	tagAgentBehavior = "agent-behavior"
)

// capReason is one objective condition that fired the Efficiency hard-cap. It
// carries the observed value, the threshold it crossed, and the remediation tag,
// so the file-bugs step can emit a precise, reproducible finding.
type capReason struct {
	Signal    string  `json:"signal"`
	Observed  float64 `json:"observed"`
	Threshold float64 `json:"threshold"`
	Tag       string  `json:"tag"`
	Detail    string  `json:"detail"`
}

// EfficiencyCap is the JUDGE-step verdict for the token-cost sub-dimension of
// Efficiency. Capped is true when >= 1 reason fired; the Efficiency dimension is
// then capped at CapScore (the judge applies min(existing_score, CapScore)).
// Reasons are appended in a FIXED order (cache-hit, spikes, verbosity) so the
// serialized verdict is deterministic — two graders agree byte-for-byte.
type EfficiencyCap struct {
	Capped   bool        `json:"capped"`
	CapScore int         `json:"cap_score"`
	Reasons  []capReason `json:"reasons"`
}

// evaluateEfficiencyCap applies the ONE token-cost hard-cap to a session's
// objective TokenSignals (produced by task-1's --token-signals pass). It is a
// pure function: no I/O, no clock, no randomness, no map iteration — the same
// signals always yield the same verdict, which is exactly what makes the cap
// reproducible across graders.
func evaluateEfficiencyCap(s *TokenSignals) EfficiencyCap {
	verdict := EfficiencyCap{CapScore: efficiencyCapScore}
	if s == nil {
		return verdict
	}

	// (1) Cache-hit floor — data-plane. Only meaningful on a long session, so a
	// cold short session is never capped on this signal alone.
	if s.TurnCount >= longSessionMinTurns && s.CacheHitRatio < cacheHitFloor {
		verdict.Reasons = append(verdict.Reasons, capReason{
			Signal:    "cache_hit_ratio",
			Observed:  s.CacheHitRatio,
			Threshold: cacheHitFloor,
			Tag:       tagDataPlane,
			Detail:    "long session rode cache below the documented floor — re-priming/rebuilding context instead of reusing it (context-composition defect)",
		})
	}

	// (2) Redundant context spikes — data-plane.
	if s.RedundantContextSpikes >= redundantSpikeCap {
		verdict.Reasons = append(verdict.Reasons, capReason{
			Signal:    "redundant_context_spikes",
			Observed:  float64(s.RedundantContextSpikes),
			Threshold: float64(redundantSpikeCap),
			Tag:       tagDataPlane,
			Detail:    "repeated re-prime balloons (cache creation > 2x running median write) — redundant context payloads",
		})
	}

	// (3) Output verbosity — agent-behavior (advice-only).
	if s.OutputPerRequest > verbosityCeiling {
		verdict.Reasons = append(verdict.Reasons, capReason{
			Signal:    "output_per_request",
			Observed:  s.OutputPerRequest,
			Threshold: verbosityCeiling,
			Tag:       tagAgentBehavior,
			Detail:    "mean output per request exceeds the verbosity ceiling — verbose-output habit (agent education, not a code fix)",
		})
	}

	verdict.Capped = len(verdict.Reasons) > 0
	return verdict
}
