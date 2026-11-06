package main

// claude_md_directive_density_budget_test.go closes a real coverage gap the
// Opus/Fable-5 context-audit rollup silently assumed shut: the four repo
// CLAUDE.md files are DIRECTIVE-DENSITY guarded in CI.
//
// The four CLAUDE.md files (repo-root + gardenio/coop/kbeads component files) are
// injected into every agent session by the Claude Code harness — root into every
// session, a component file into any session whose CWD is under that component.
// They are NOT part of the `gb prime` body, so the sibling
// TestPrimeDirectiveDensityBudget (which renders config beads + advice) never
// touches them. The 2026-07-25 audit (bundle gd-7mDScJ4NY5, findings A1-A4)
// deliberately trimmed these files' over-constraint down to documentation shape
// — "1 directive across 14k tokens (0.07/1k)" per the audit insight — and the
// 2026-07-31 differential re-audit rollup
// (gardenio/docs/audits/2026-07-31-differential/README.md) tabled all four as
// "Held (CI-gated)". They were NOT gated: nothing measured them, so that win was
// one careless edit away from silently eroding. This test makes the "CI-gated"
// claim true.
//
// METHODOLOGY: identical to prime_directive_density_budget_test.go and the
// committed scripts/directive-density.py — same emphatic-directive marker set
// (MUST/NEVER/DO NOT/CRITICAL/Prohibited + ALWAYS/IMPORTANT/REQUIRED/WARNING),
// same case-insensitive "Do NOT" match, same bytes/4 proxy tokens
// (advice.TokenBytesRatio). We reuse that test's countDirectives / countMentions
// / proxyTokens / densityPer1k / compactPerMarker helpers verbatim so the two
// guards can never drift apart in what they count.
//
// ENFORCEMENT MODEL — why this differs from the sibling's percentage band:
// these files sit at 0/0/0/1 directives, where a percentage spike band
// degenerates (0 * 1.2 == 0, and count >= 0 is vacuously true). A percentage
// staleness floor is equally meaningless at zero. So the guard is ABSOLUTE:
//   1. per-file directive count must stay <= baseline + directiveHeadroom — a
//      couple of load-bearing gotcha imperatives (e.g. the kbeads --label-any
//      MUST) may be added without churn, but a wall of shouts trips it;
//   2. per-file directive DENSITY must stay <= claudeMDDensityCeiling — catches
//      a SMALL file gaining a few directives (high density at low absolute
//      count) that the count headroom alone would miss;
//   3. per-file @-mention count must stay <= baseline + mentionHeadroom (reusing
//      the sibling's mentionHeadroom) — over-specific handle/email references are
//      a documentation smell;
//   4. the aggregate directive count across all four must stay <= its baseline +
//      aggregateHeadroom — a diffuse spread of one new directive per file that
//      each per-file headroom would tolerate still trips the whole.
// When a directive change is deliberate (a load-bearing hard rule added to a
// gotcha), re-measure from the BASELINE log lines this test emits and bump the
// row in the same MR with a note on why the imperative is load-bearing.

import (
	"os"
	"path/filepath"
	"testing"
)

// directiveHeadroom is the absolute number of extra emphatic directives a single
// CLAUDE.md file may accrue over its frozen baseline before the guard trips. Set
// small (2) because these are DOCUMENTATION files: a load-bearing gotcha or two
// (a MUST on an irreversible action, an escalation-label footgun) is legitimate,
// but the audit's whole point is they must not drift back into imperative-stacked
// prompt text.
const directiveHeadroom = 2

// claudeMDDensityCeiling is the absolute per-1k-token directive-density ceiling
// for any single CLAUDE.md file. The audit measured these files at 0.07/1k
// corpus-wide and the current per-file max is kbeads at 0.39/1k; the injected
// workflow prompt (init_prompts.go) runs ~1.1/1k and the pre-remediation hot
// spot ran 4.25/1k. A 2.0/1k ceiling sits comfortably above documentation-shaped
// text (so a single load-bearing MUST in a short component file does not false-
// trip) while staying far below the imperative-stacked prompt density the audit
// retired — a small file that gains several directives crosses it well before it
// would read as documentation again.
const claudeMDDensityCeiling = 2.0

// aggregateHeadroom is the absolute number of extra directives permitted across
// ALL four files combined, over their summed baseline. It catches the diffuse
// failure mode the per-file headroom misses: one new directive added to each of
// the four files is within every per-file headroom yet quadruples the corpus's
// directive load. Kept modest (3) so a broad re-bloat surfaces.
const aggregateHeadroom = 3

// claudeMDBaseline is the frozen per-file baseline. Measured 2026-08-08 against
// the post-A1-A4-remediation working tree with the same marker set + bytes/4
// proxy tokens the sibling density guard uses. directives/mentions are exact
// counts; density is the recorded value for the BASELINE log line (the guard
// checks against claudeMDDensityCeiling, not this density, so a byte-only edit
// that shifts density slightly does not require a re-baseline).
//
//	root:    0 directives, 0 mentions, 0.00/1k — pure documentation.
//	gardenio: 0 directives, 0 mentions, 0.00/1k.
//	coop:    0 directives, 1 mention (@gmail.com, an email fragment), 0.00/1k.
//	kbeads:  1 directive  (the load-bearing `--label-any` escalation MUST),
//	         0 mentions, 0.39/1k.
var claudeMDBaseline = []struct {
	name       string // display label
	relPath    string // path relative to repo root
	directives int    // frozen emphatic-directive count
	mentions   int    // frozen @-mention count
	density    float64
}{
	{"CLAUDE.md (root)", "CLAUDE.md", 0, 0, 0.00},
	{"CLAUDE.md (gardenio)", "gardenio/CLAUDE.md", 0, 0, 0.00},
	{"CLAUDE.md (coop)", "coop/CLAUDE.md", 0, 1, 0.00},
	{"CLAUDE.md (kbeads)", "kbeads/CLAUDE.md", 1, 0, 0.39},
}

// TestClaudeMDDirectiveDensityBudget reads each injected CLAUDE.md file from the
// working tree and fails if its emphatic-directive count, directive density, or
// @-mention count regresses past its frozen baseline + tolerance — the analog of
// TestPrimeDirectiveDensityBudget for the harness-injected files gb prime does
// not render.
func TestClaudeMDDirectiveDensityBudget(t *testing.T) {
	repoRoot := findRepoRootForSizing()

	var aggregateDirectives, aggregateBaseline int

	for _, c := range claudeMDBaseline {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(repoRoot, c.relPath)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: cannot read %s: %v — the guard measures nothing if the file is missing", c.name, path, err)
			}

			// Sanity: a truncated/empty file would let the budget pass vacuously.
			if len(raw) < 500 {
				t.Fatalf("%s: %s suspiciously small (%d bytes) — file likely truncated or moved", c.name, path, len(raw))
			}

			directives, per := countDirectives(raw)
			mentions := countMentions(raw)
			tokens := proxyTokens(len(raw))
			density := densityPer1k(directives, tokens)

			t.Logf("BASELINE %s: directives=%d mentions=%d bytes=%d tokens=%d density=%.2f/1k per=%v",
				c.name, directives, mentions, len(raw), tokens, density, compactPerMarker(per))

			// 1. Absolute directive-count cap: baseline + headroom.
			countCap := c.directives + directiveHeadroom
			if directives > countCap {
				t.Errorf("%s: directive count %d > cap %d (baseline %d + %d headroom) — CLAUDE.md is documentation, not injected prompt text; rewrite negatives to positive intent, or if the imperative is load-bearing re-baseline this row in-MR with a note (see gardenio/docs/audits/2026-07-25-directive-density/)",
					c.name, directives, countCap, c.directives, directiveHeadroom)
			}

			// 2. Absolute density ceiling — catches a small file gaining a few
			//    directives (high density at low absolute count).
			if density > claudeMDDensityCeiling {
				t.Errorf("%s: directive density %.2f/1k > ceiling %.2f/1k — the file is drifting toward imperative-stacked prompt shape; audit with scripts/directive-density.py",
					c.name, density, claudeMDDensityCeiling)
			}

			// 3. @-mention over-specificity budget (reuses the sibling's headroom).
			mentionCap := c.mentions + mentionHeadroom
			if mentions > mentionCap {
				t.Errorf("%s: @-mention count %d > cap %d (baseline %d + %d headroom) — prefer role/principle framing over naming specific agents/handles, or re-baseline in-MR",
					c.name, mentions, mentionCap, c.mentions, mentionHeadroom)
			}

			aggregateDirectives += directives
			aggregateBaseline += c.directives
		})
	}

	// 4. Aggregate guard — the diffuse-spread failure mode the per-file headroom
	//    misses (one new directive in each file is within every per-file headroom
	//    yet multiplies the corpus's directive load).
	aggCap := aggregateBaseline + aggregateHeadroom
	if aggregateDirectives > aggCap {
		t.Errorf("aggregate CLAUDE.md directive count %d > cap %d (summed baseline %d + %d headroom) — over-constraint is re-leaking across the injected doc files even if no single file crossed its own headroom",
			aggregateDirectives, aggCap, aggregateBaseline, aggregateHeadroom)
	}
}
