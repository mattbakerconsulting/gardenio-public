package main

// nudge_prompt_directive_density_budget_test.go closes a live coverage gap in
// the Opus/Fable-5 over-constraint guard family: the per-agent SPAWN-NUDGE
// templates are directive-density guarded in CI.
//
// The nudge-prompt templates live in the `config:nudge-prompts:global` config
// bead (`adhoc`/`default`/`thread`/`prewarmed`/`gardener`, authored in
// gardenio/controller/internal/bridge/init_prompts.go). One of them is the very
// first thing every agent reads at boot — the spawn prompt that frames the whole
// session (`adhoc` for a task-carrying pod, `default` for a work-seeker, etc.).
// They are injected into every session, yet they are NOT part of the `gb prime`
// body: prime's outputConfigSections renders only the claude-instructions
// sections (prime_header/session_close/core_rules/…), never the nudge map. So
// the sibling TestPrimeDirectiveDensityBudget (which renders config beads +
// advice) never touches them, and TestClaudeMDDirectiveDensityBudget covers only
// the four repo CLAUDE.md files. Nothing measured this surface.
//
// PROOF THE GAP IS LIVE: commit 68e06012a (2026-08-06, ka-0000002081) added a
// "do NOT `kd close` … file a gitlab_mr_merge escalation" directive to BOTH the
// `adhoc` and `default` nudges — a legitimate behavioral fix, but it landed
// CI-green with zero directive-density signal, taking each of those two
// templates from 0 → 1 emphatic directive. That is exactly the differential
// audit's D1 failure mode (an ungated injected surface drifting up unnoticed —
// gardenio/docs/audits/2026-07-31-differential/README.md) and the same
// coverage-gap class g49 closed for CLAUDE.md (MR !7017). This test makes the
// nudge surface a guarded one.
//
// METHODOLOGY: identical to prime_directive_density_budget_test.go and
// claude_md_directive_density_budget_test.go and the committed
// scripts/directive-density.py — same emphatic-directive marker set
// (MUST/NEVER/DO NOT/CRITICAL/Prohibited + ALWAYS/IMPORTANT/REQUIRED/WARNING),
// same case-insensitive "Do NOT" match, same bytes/4 proxy tokens
// (advice.TokenBytesRatio). We reuse this package's countDirectives /
// countMentions / proxyTokens / densityPer1k / compactPerMarker helpers verbatim
// so the three guards can never drift apart in what they count. We measure the
// RAW template text (Go template vars like {{.TaskHint}} left intact) — the
// directive markers are authored text, not injected values, so template
// expansion cannot add or remove a marker.
//
// FIDELITY: rather than reading the source strings, we seed the canonical config
// beads via bridge.EnsureConfigs into the same in-memory densityConfigSetter the
// sibling prime guard uses, then unmarshal the `nudge-prompts` bead's
// Description — the exact production seed path (init_ensure.go json.Marshal of
// the map[string]string). A directive re-leak into any nudge template turns this
// test red.
//
// ENFORCEMENT MODEL — ABSOLUTE COUNT, not density, and here is why it differs
// from the CLAUDE.md sibling: those four files are LARGE (1k–2.9k tokens), so a
// per-1k density ceiling is a meaningful backstop (a small file gaining a few
// directives spikes density while its absolute count stays low). The nudge
// templates are the opposite regime — all SHORT (≈80–220 proxy tokens), so a
// SINGLE legitimate load-bearing directive already reads as 5–13/1k purely
// because the denominator is tiny. A density ceiling there is degenerate: it
// would false-trip on the very first lifecycle imperative and fight the count
// headroom, exactly the zero-degeneracy trap that pushed the CLAUDE.md guard to
// absolute counts in the first place. Across templates of near-identical length,
// directive COUNT and density track each other, so count is the discriminating
// signal and density adds none. We therefore guard on absolute counts and log
// density only as a diagnostic:
//   1. per-template directive count must stay <= baseline + nudgeDirectiveHeadroom
//      — a load-bearing lifecycle imperative or two may be added without churn,
//      but a wall of shouts trips it;
//   2. per-template @-mention count must stay <= baseline + mentionHeadroom
//      (reusing the sibling's mentionHeadroom) — naming specific handles in a
//      universally-injected prompt is an over-specificity smell;
//   3. the aggregate directive count across ALL templates must stay <= its summed
//      baseline + nudgeAggregateHeadroom — the diffuse failure mode (one new
//      directive per template) each per-template headroom would tolerate;
//   4. a completeness guard: every shipped template must have a baseline row and
//      every baseline row must still ship, so a NEW template cannot slip in
//      unguarded and a silently-emptied one cannot pass vacuously.
// When a directive change is deliberate, re-measure from the BASELINE log lines
// this test emits and bump the row(s) in the same MR with a note on why the
// imperative is load-bearing (see gardenio/docs/audits/2026-07-25-directive-density/).

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"testing"

	"gardenio/controller/internal/bridge"
)

// nudgeDirectiveHeadroom is the absolute number of extra emphatic directives a
// single nudge template may accrue over its frozen baseline before the guard
// trips. Set small (2) — matching the CLAUDE.md guard's directiveHeadroom: the
// nudge templates are terse spawn framing, so a genuinely load-bearing lifecycle
// imperative (an irreversible-action gate, a stop-gate footgun) is legitimate,
// but the audit's whole point is that they must not drift back into
// imperative-stacked prompt text.
const nudgeDirectiveHeadroom = 2

// nudgeAggregateHeadroom is the absolute number of extra directives permitted
// across ALL nudge templates combined, over their summed baseline. It catches
// the diffuse failure mode the per-template headroom misses: one new directive
// added to each template is within every per-template headroom yet multiplies
// the corpus's directive load. Kept modest (3) so a broad re-bloat surfaces.
const nudgeAggregateHeadroom = 3

// nudgeBaseline is the frozen per-template baseline. Measured 2026-08-08 against
// the post-68e06012a working tree with the same marker set + bytes/4 proxy
// tokens the sibling guards use. directives/mentions are exact counts; density is
// recorded only for the BASELINE log line (the guard checks absolute counts, not
// density — see the ENFORCEMENT MODEL note above — so a byte-only edit that
// shifts density does not require a re-baseline).
//
//	thread:    0 directives, 0 mentions, 0.00/1k — pure onboarding framing.
//	adhoc:     1 directive  (the load-bearing unmerged-MR "do NOT kd close"
//	           lifecycle gate, ka-0000002081), 0 mentions, ~4.8/1k.
//	default:   1 directive  (same unmerged-MR lifecycle gate), 0 mentions, ~6.0/1k.
//	prewarmed: 0 directives, 0 mentions, 0.00/1k — idle-pool wait framing.
//	gardener:   0 directives, 0 mentions, 0.00/1k — orchestrator framing.
var nudgeBaseline = []struct {
	key        string // nudge-prompts map key
	directives int    // frozen emphatic-directive count
	mentions   int    // frozen @-mention count
	density    float64
}{
	{"thread", 0, 0, 0.00},
	{"adhoc", 1, 0, 4.81},
	{"default", 1, 0, 6.02},
	{"prewarmed", 0, 0, 0.00},
	{"gardener", 0, 0, 0.00},
}

// nudgeLocalBaseline is the frozen baseline for the role:local nudge layer
// (config:nudge-prompts:role:local, gd-rIHQbpZR0Y), which overrides only the two
// templates a `gb agent start --local --role local` session boots on. Both were
// written with no emphatic directives: the escalation limit is stated as the
// route to take ("goes through `gb escalation create`"), not as a prohibition.
var nudgeLocalBaseline = []struct {
	key        string
	directives int
	mentions   int
	density    float64
}{
	{"adhoc", 0, 0, 0.00},
	{"default", 0, 0, 0.00},
}

// loadNudgePrompts seeds the canonical config beads via bridge.EnsureConfigs into
// an in-memory setter (the exact production seed path) and returns the resolved
// nudge-prompts template map for one layer (label "global" or "role:<r>").
// Using the seed path rather than reading source strings keeps this guard pinned
// to what actually ships.
func loadNudgePrompts(t *testing.T, label string) map[string]string {
	t.Helper()
	setter := &densityConfigSetter{}
	if err := bridge.EnsureConfigs(context.Background(), setter, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}
	// Each nudge-prompts entry seeds as a config bead titled "nudge-prompts"
	// with its layer as the single label and the JSON-marshalled
	// map[string]string as its Description (init_ensure.go
	// parseEntryKey/ensureConfigBead).
	for _, b := range setter.beads {
		if b.Title != "nudge-prompts" || len(b.Labels) != 1 || b.Labels[0] != label {
			continue
		}
		var prompts map[string]string
		if err := json.Unmarshal([]byte(b.Description), &prompts); err != nil {
			t.Fatalf("unmarshal nudge-prompts bead %s: %v", b.ID, err)
		}
		return prompts
	}
	t.Fatalf("EnsureConfigs did not seed a nudge-prompts config bead labelled %q — the guard measures nothing", label)
	return nil
}

// TestNudgePromptDirectiveDensityBudget resolves each spawn-nudge template from
// the canonical config seed and fails if its emphatic-directive count or
// @-mention count regresses past its frozen baseline + tolerance — the analog of
// TestPrimeDirectiveDensityBudget / TestClaudeMDDirectiveDensityBudget for the
// universally-injected spawn prompts gb prime does not render.
func TestNudgePromptDirectiveDensityBudget(t *testing.T) {
	prompts := loadNudgePrompts(t, "global")

	// (4) Completeness guard: every template we froze must still exist, and every
	// shipped template must be covered by a baseline row (a NEW template must be
	// measured and added here, not slip in unguarded).
	baselined := make(map[string]bool, len(nudgeBaseline))
	for _, c := range nudgeBaseline {
		baselined[c.key] = true
		if _, ok := prompts[c.key]; !ok {
			t.Errorf("baselined nudge template %q is missing from the shipped nudge-prompts map — remove its baseline row or restore the template", c.key)
		}
	}
	shipped := make([]string, 0, len(prompts))
	for k := range prompts {
		shipped = append(shipped, k)
	}
	sort.Strings(shipped)
	for _, k := range shipped {
		if !baselined[k] {
			raw := []byte(prompts[k])
			directives, per := countDirectives(raw)
			t.Errorf("shipped nudge template %q has no baseline row — measure it (directives=%d density=%.2f/1k per=%v) and add a nudgeBaseline entry so it is guarded",
				k, directives, densityPer1k(directives, proxyTokens(len(raw))), compactPerMarker(per))
		}
	}

	var aggregateDirectives, aggregateBaseline int

	for _, c := range nudgeBaseline {
		t.Run(c.key, func(t *testing.T) {
			raw := []byte(prompts[c.key])

			// Sanity: an empty/truncated template would let the budget pass
			// vacuously. The shortest real template (prewarmed) is >300 bytes.
			if len(raw) < 100 {
				t.Fatalf("%s: nudge template suspiciously small (%d bytes) — likely truncated or emptied", c.key, len(raw))
			}

			directives, per := countDirectives(raw)
			mentions := countMentions(raw)
			tokens := proxyTokens(len(raw))
			density := densityPer1k(directives, tokens)

			t.Logf("BASELINE nudge:%s: directives=%d mentions=%d bytes=%d tokens=%d density=%.2f/1k per=%v",
				c.key, directives, mentions, len(raw), tokens, density, compactPerMarker(per))

			// 1. Absolute directive-count cap: baseline + headroom. (Density is
			//    logged above as a diagnostic but NOT asserted — for templates
			//    this short a per-1k ceiling is degenerate; see the file header.)
			countCap := c.directives + nudgeDirectiveHeadroom
			if directives > countCap {
				t.Errorf("%s: directive count %d > cap %d (baseline %d + %d headroom) — the nudge is the spawn prompt every agent boots on, not imperative-stacked prompt text; rewrite negatives to positive intent, or if the imperative is load-bearing re-baseline this row in-MR with a note (see gardenio/docs/audits/2026-07-25-directive-density/)",
					c.key, directives, countCap, c.directives, nudgeDirectiveHeadroom)
			}

			// 2. @-mention over-specificity budget (reuses the sibling's headroom).
			mentionCap := c.mentions + mentionHeadroom
			if mentions > mentionCap {
				t.Errorf("%s: @-mention count %d > cap %d (baseline %d + %d headroom) — prefer role/principle framing over naming specific agents/handles in a universally-injected prompt, or re-baseline in-MR",
					c.key, mentions, mentionCap, c.mentions, mentionHeadroom)
			}

			aggregateDirectives += directives
			aggregateBaseline += c.directives
		})
	}

	// 3. Aggregate guard — the diffuse-spread failure mode the per-template
	//    headroom misses (one new directive in each template is within every
	//    per-template headroom yet multiplies the corpus's directive load).
	aggCap := aggregateBaseline + nudgeAggregateHeadroom
	if aggregateDirectives > aggCap {
		t.Errorf("aggregate nudge-prompt directive count %d > cap %d (summed baseline %d + %d headroom) — over-constraint is re-leaking across the injected spawn prompts even if no single template crossed its own headroom",
			aggregateDirectives, aggCap, aggregateBaseline, nudgeAggregateHeadroom)
	}
}

// TestNudgePromptLocalLayerDirectiveBudget applies the same per-template count
// and completeness guards to the role:local nudge layer, so a local agent's
// spawn prompt cannot drift into imperative-stacked text unmeasured either.
func TestNudgePromptLocalLayerDirectiveBudget(t *testing.T) {
	prompts := loadNudgePrompts(t, "role:local")

	baselined := make(map[string]bool, len(nudgeLocalBaseline))
	for _, c := range nudgeLocalBaseline {
		baselined[c.key] = true
	}
	for k := range prompts {
		if !baselined[k] {
			t.Errorf("shipped role:local nudge template %q has no nudgeLocalBaseline row — measure it and add one so it is guarded", k)
		}
	}

	for _, c := range nudgeLocalBaseline {
		t.Run(c.key, func(t *testing.T) {
			raw := []byte(prompts[c.key])
			if len(raw) < 100 {
				t.Fatalf("role:local %s: nudge template missing or suspiciously small (%d bytes)", c.key, len(raw))
			}
			directives, per := countDirectives(raw)
			mentions := countMentions(raw)
			tokens := proxyTokens(len(raw))
			t.Logf("BASELINE nudge:role:local:%s: directives=%d mentions=%d bytes=%d tokens=%d density=%.2f/1k per=%v",
				c.key, directives, mentions, len(raw), tokens, densityPer1k(directives, tokens), compactPerMarker(per))

			if countCap := c.directives + nudgeDirectiveHeadroom; directives > countCap {
				t.Errorf("role:local %s: directive count %d > cap %d (baseline %d + %d headroom) — rewrite negatives to positive intent, or re-baseline this row in-MR with a note",
					c.key, directives, countCap, c.directives, nudgeDirectiveHeadroom)
			}
			if mentionCap := c.mentions + mentionHeadroom; mentions > mentionCap {
				t.Errorf("role:local %s: @-mention count %d > cap %d (baseline %d + %d headroom)",
					c.key, mentions, mentionCap, c.mentions, mentionHeadroom)
			}
		})
	}
}
