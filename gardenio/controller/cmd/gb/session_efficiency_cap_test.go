package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// ── CED Efficiency token-cost hard-cap (Token-eff 3, ka-0000007291) ──────────
//
// These tests pin the ONE reproducible hard-cap so two graders agree: each
// threshold is exercised in isolation and at its boundary, and the three captured
// JSONL fixtures lock end-to-end behavior — bloated fires; healthy does not; and
// efficient_build (a real 0.99-cache-hit build) does not, which is the
// false-positive regression from bug ka-0000003202.

// TestEvaluateEfficiencyCap_NilAndEmpty: a nil or empty signal set never caps.
func TestEvaluateEfficiencyCap_NilAndEmpty(t *testing.T) {
	if v := evaluateEfficiencyCap(nil); v.Capped {
		t.Errorf("nil signals must not cap, got %+v", v)
	}
	if v := evaluateEfficiencyCap(&TokenSignals{}); v.Capped {
		t.Errorf("empty signals must not cap, got %+v", v)
	}
	// CapScore is always the documented value, even when nothing fires.
	if v := evaluateEfficiencyCap(nil); v.CapScore != efficiencyCapScore {
		t.Errorf("cap_score: expected %d, got %d", efficiencyCapScore, v.CapScore)
	}
}

// TestEvaluateEfficiencyCap_CacheHitFloor exercises the cache-hit condition and
// its long-session gate.
func TestEvaluateEfficiencyCap_CacheHitFloor(t *testing.T) {
	// Long session (>= floor turns) below the cache-hit floor => caps, data-plane.
	low := evaluateEfficiencyCap(&TokenSignals{
		TurnCount:     longSessionMinTurns,
		CacheHitRatio: cacheHitFloor - 0.01,
	})
	if !low.Capped {
		t.Fatal("long session below cache-hit floor must cap")
	}
	if got := reasonsTagged(low, tagDataPlane); len(got) != 1 || got[0].Signal != "cache_hit_ratio" {
		t.Errorf("expected one data-plane cache_hit_ratio reason, got %+v", low.Reasons)
	}

	// Same low ratio but a SHORT session => not capped (cold-session carve-out).
	short := evaluateEfficiencyCap(&TokenSignals{
		TurnCount:     longSessionMinTurns - 1,
		CacheHitRatio: cacheHitFloor - 0.01,
	})
	if short.Capped {
		t.Errorf("short session must not cap on cache-hit alone, got %+v", short.Reasons)
	}

	// Long session AT the floor => not capped (strict <, documented boundary).
	atFloor := evaluateEfficiencyCap(&TokenSignals{
		TurnCount:     longSessionMinTurns,
		CacheHitRatio: cacheHitFloor,
	})
	if atFloor.Capped {
		t.Errorf("ratio exactly at floor must not cap (strict <), got %+v", atFloor.Reasons)
	}
}

// TestEvaluateEfficiencyCap_RedundantSpikes exercises the redundant-spike
// condition at its boundary.
func TestEvaluateEfficiencyCap_RedundantSpikes(t *testing.T) {
	// One spike below the cap threshold => not capped (a single re-prime is OK).
	one := evaluateEfficiencyCap(&TokenSignals{RedundantContextSpikes: redundantSpikeCap - 1})
	if one.Capped {
		t.Errorf("one spike must not cap, got %+v", one.Reasons)
	}
	// At the threshold => caps, data-plane.
	at := evaluateEfficiencyCap(&TokenSignals{RedundantContextSpikes: redundantSpikeCap})
	if !at.Capped {
		t.Fatal("spikes at threshold must cap")
	}
	if got := reasonsTagged(at, tagDataPlane); len(got) != 1 || got[0].Signal != "redundant_context_spikes" {
		t.Errorf("expected one data-plane redundant_context_spikes reason, got %+v", at.Reasons)
	}
}

// TestEvaluateEfficiencyCap_Verbosity exercises the output-verbosity condition
// and confirms it is tagged agent-behavior (advice-only), not data-plane.
func TestEvaluateEfficiencyCap_Verbosity(t *testing.T) {
	// At the ceiling => not capped (strict >).
	at := evaluateEfficiencyCap(&TokenSignals{OutputPerRequest: verbosityCeiling})
	if at.Capped {
		t.Errorf("output exactly at ceiling must not cap (strict >), got %+v", at.Reasons)
	}
	// Above the ceiling => caps, agent-behavior.
	over := evaluateEfficiencyCap(&TokenSignals{OutputPerRequest: verbosityCeiling + 1})
	if !over.Capped {
		t.Fatal("output over ceiling must cap")
	}
	if got := reasonsTagged(over, tagAgentBehavior); len(got) != 1 || got[0].Signal != "output_per_request" {
		t.Errorf("expected one agent-behavior output_per_request reason, got %+v", over.Reasons)
	}
	// Verbosity is NOT a data-plane finding — it must never appear as crew-fixable.
	if len(reasonsTagged(over, tagDataPlane)) != 0 {
		t.Errorf("verbosity must not be data-plane tagged, got %+v", reasonsTagged(over, tagDataPlane))
	}
}

// TestEvaluateEfficiencyCap_ReasonOrderDeterministic locks the fixed evaluation
// order (cache-hit, spikes, verbosity) so the serialized verdict is byte-stable
// across graders.
func TestEvaluateEfficiencyCap_ReasonOrderDeterministic(t *testing.T) {
	all := &TokenSignals{
		TurnCount:              longSessionMinTurns,
		CacheHitRatio:          cacheHitFloor - 0.1,
		RedundantContextSpikes: redundantSpikeCap,
		OutputPerRequest:       verbosityCeiling + 1000,
	}
	want := []string{"cache_hit_ratio", "redundant_context_spikes", "output_per_request"}

	// Evaluate twice; both the fired signals and their order must be identical.
	first := evaluateEfficiencyCap(all)
	second := evaluateEfficiencyCap(all)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("evaluation is not deterministic:\n first=%+v\nsecond=%+v", first, second)
	}
	var gotOrder []string
	for _, r := range first.Reasons {
		gotOrder = append(gotOrder, r.Signal)
	}
	if !reflect.DeepEqual(gotOrder, want) {
		t.Errorf("reason order: expected %v, got %v", want, gotOrder)
	}
}

// loadCEDFixtureSignals parses a testdata/ced fixture through the SAME production
// pass the measure step uses, so the fixture test tracks real behavior.
func loadCEDFixtureSignals(t *testing.T, name string) *TokenSignals {
	t.Helper()
	path := filepath.Join("testdata", "ced", name)
	ts, err := parseSessionTokenSignals(path)
	if err != nil {
		t.Fatalf("parseSessionTokenSignals(%s): %v", path, err)
	}
	return ts
}

// TestEfficiencyCap_BloatedFixtureFires is the core acceptance pin: the captured
// bloated session fires the hard-cap deterministically, on all three conditions,
// with the correct data-plane vs agent-behavior split.
func TestEfficiencyCap_BloatedFixtureFires(t *testing.T) {
	ts := loadCEDFixtureSignals(t, "token_cost_bloated.jsonl")
	verdict := evaluateEfficiencyCap(ts)

	if !verdict.Capped {
		t.Fatalf("bloated fixture must cap; signals=%+v verdict=%+v", ts, verdict)
	}
	if verdict.CapScore != efficiencyCapScore {
		t.Errorf("cap_score: expected %d, got %d", efficiencyCapScore, verdict.CapScore)
	}

	// Two data-plane (crew-fixable) reasons: cache-hit + redundant spikes.
	dp := reasonsTagged(verdict, tagDataPlane)
	if len(dp) != 2 {
		t.Fatalf("expected 2 data-plane reasons, got %d: %+v", len(dp), dp)
	}
	if dp[0].Signal != "cache_hit_ratio" || dp[1].Signal != "redundant_context_spikes" {
		t.Errorf("data-plane signals wrong: %+v", dp)
	}

	// One agent-behavior (advice-only) reason: verbosity.
	ab := reasonsTagged(verdict, tagAgentBehavior)
	if len(ab) != 1 || ab[0].Signal != "output_per_request" {
		t.Errorf("expected one agent-behavior output_per_request reason, got %+v", ab)
	}
}

// TestEfficiencyCap_HealthyFixtureDoesNotFire is the negative pin: a healthy
// captured session fires NOTHING, so the cap does not false-positive.
func TestEfficiencyCap_HealthyFixtureDoesNotFire(t *testing.T) {
	ts := loadCEDFixtureSignals(t, "token_cost_healthy.jsonl")
	verdict := evaluateEfficiencyCap(ts)

	if verdict.Capped {
		t.Fatalf("healthy fixture must NOT cap; signals=%+v reasons=%+v", ts, verdict.Reasons)
	}
	if len(verdict.Reasons) != 0 {
		t.Errorf("healthy fixture must have zero reasons, got %+v", verdict.Reasons)
	}
}

// TestEfficiencyCap_EfficientBuildFixtureDoesNotFire is the regression pin for
// bug ka-0000003202. The fixture carries the REAL per-turn token series of
// session ka-0000005126/7622b506 — a 152-turn feature build with a 0.9909
// cache_hit_ratio and 784 mean output tokens, i.e. an exemplary-efficient
// session by every other signal.
//
// Before the minSpikeTokens floor, its four ordinary source-file reads
// (3687/5153/5278/5434 tokens against a ~1.5k median write) each cleared the
// scale-free ">2× running median" test and capped the Efficiency dimension from
// 9 to 6 on a single spurious reason. A session riding cache at 0.99 is
// definitionally not re-priming, so nothing here may fire.
func TestEfficiencyCap_EfficientBuildFixtureDoesNotFire(t *testing.T) {
	ts := loadCEDFixtureSignals(t, "token_cost_efficient_build.jsonl")

	// Guard the fixture itself: if these drift, the pin below stops testing the
	// bug it was written for.
	if ts.CacheHitRatio < 0.99 {
		t.Fatalf("fixture must be a high-cache-hit session, got %f", ts.CacheHitRatio)
	}
	if ts.TurnCount < longSessionMinTurns {
		t.Fatalf("fixture must be a long session, got %d turns", ts.TurnCount)
	}

	if got := ts.RedundantContextSpikes; got != 0 {
		t.Errorf("small source reads must not count as spikes, got %d", got)
	}
	verdict := evaluateEfficiencyCap(ts)
	if verdict.Capped {
		t.Fatalf("efficient build must NOT cap; signals=%+v reasons=%+v", ts, verdict.Reasons)
	}
}

// TestEfficiencyCap_ResumeReprimeFixtureDoesNotFire is the acceptance pin for bug
// gd-bpJOr1dotX. The fixture carries the REAL per-turn token series of session
// ka-0000005428/98c9b8f5 — a resume-in-place cron gardener woken 6× across ~6h
// gaps. By every non-resume signal it is exemplary (cache_hit_ratio 0.916,
// output_per_request 672, zero errors), yet its 6 post-resume cold-cache
// re-primes (42k→122k cache_creation) each cleared the scale-blind spike test and
// capped Efficiency 9→6.
//
// With the detector made resume-aware, the 6 re-primes are exempt (each is the
// first write after a "<synthetic>"/"Continue from where you left off" boundary),
// so redundant_context_spikes is 0 and nothing caps. This mirrors the
// minSpikeTokens regression (ka-0000003202): a false positive fixed IN the
// detector so every consumer stops seeing the noise.
func TestEfficiencyCap_ResumeReprimeFixtureDoesNotFire(t *testing.T) {
	ts := loadCEDFixtureSignals(t, "token_cost_resume_reprime.jsonl")

	// Guard the fixture itself: if these drift, the pin stops testing the bug.
	if ts.CacheHitRatio < 0.90 {
		t.Fatalf("fixture must be a high-cache-hit session, got %f", ts.CacheHitRatio)
	}
	if ts.TurnCount < longSessionMinTurns {
		t.Fatalf("fixture must be a long session, got %d turns", ts.TurnCount)
	}

	if got := ts.RedundantContextSpikes; got != 0 {
		t.Errorf("resume re-primes must be exempt, got %d spikes", got)
	}
	verdict := evaluateEfficiencyCap(ts)
	if verdict.Capped {
		t.Fatalf("resumed session must NOT cap; signals=%+v reasons=%+v", ts, verdict.Reasons)
	}
}

// TestEfficiencyCap_FixturesAgreeAcrossGraders is the "two graders agree" pin:
// running the whole measure+judge pipeline twice on each fixture yields
// byte-identical JSON verdicts.
func TestEfficiencyCap_FixturesAgreeAcrossGraders(t *testing.T) {
	for _, name := range []string{"token_cost_bloated.jsonl", "token_cost_healthy.jsonl", "token_cost_efficient_build.jsonl", "token_cost_resume_reprime.jsonl"} {
		graderA := evaluateEfficiencyCap(loadCEDFixtureSignals(t, name))
		graderB := evaluateEfficiencyCap(loadCEDFixtureSignals(t, name))

		jsonA, err := json.Marshal(graderA)
		if err != nil {
			t.Fatalf("marshal graderA(%s): %v", name, err)
		}
		jsonB, err := json.Marshal(graderB)
		if err != nil {
			t.Fatalf("marshal graderB(%s): %v", name, err)
		}
		if string(jsonA) != string(jsonB) {
			t.Errorf("graders disagree on %s:\n A=%s\n B=%s", name, jsonA, jsonB)
		}
	}
}

// TestEfficiencyCap_FixtureFilesExist guards against the fixtures being deleted
// or renamed out from under the acceptance tests.
func TestEfficiencyCap_FixtureFilesExist(t *testing.T) {
	for _, name := range []string{"token_cost_bloated.jsonl", "token_cost_healthy.jsonl", "token_cost_efficient_build.jsonl", "token_cost_resume_reprime.jsonl"} {
		path := filepath.Join("testdata", "ced", name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing fixture %s: %v", path, err)
		}
	}
}

// reasonsTagged returns the fired reasons carrying tag, in evaluation order.
func reasonsTagged(c EfficiencyCap, tag string) []capReason {
	var out []capReason
	for _, r := range c.Reasons {
		if r.Tag == tag {
			out = append(out, r)
		}
	}
	return out
}
