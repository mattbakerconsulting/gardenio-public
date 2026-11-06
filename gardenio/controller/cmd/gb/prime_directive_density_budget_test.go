package main

// prime_directive_density_budget_test.go pins the DIRECTIVE DENSITY of the
// rendered `gb prime` body — the over-constraint proxy metric — so it cannot
// silently regress as new prompts/advice land (task gd-YhQIU5ckK2, under the
// P4 over-constraint-lint epic ka-0000006604; sibling of the shipped baseline
// task ka-0000001681 / MR !6018).
//
// Where prime_size_budget_test.go pins the *byte* budget (quantitative bloat)
// and real_catalog_budget_test.go pins the *matched advice pool* token budget,
// this test pins a *qualitative* dimension the byte budget is blind to: how
// many emphatic imperative directives (MUST / NEVER / ALWAYS / DO NOT /
// CRITICAL / Prohibited / IMPORTANT / REQUIRED / WARNING) and @-mention
// references the rendered prime context stacks per 1k tokens. The "new rules of
// context engineering for Claude-5-generation models" favour trusting the
// model's judgment over stacking shout-directives, so a rising directive
// density is a regression even when the byte budget is fine — text can fit the
// byte budget and still be over-constrained (see the baseline audit at
// gardenio/docs/audits/2026-07-25-directive-density/README.md).
//
// Methodology mirrors scripts/directive-density.py (the committed reproducible
// baseline tool): the primary marker set is MUST / NEVER / DO NOT / CRITICAL /
// Prohibited, and the emphatic negative is matched case-insensitively on the
// NOT ("Do NOT" / "do NOT" / "DO NOT") because these corpora write it that way.
// We extend the set with the other emphatic uppercase markers the task calls
// out (ALWAYS / IMPORTANT / REQUIRED / WARNING) to cover the fuller
// over-constraint surface. Tokens are the fleet's bytes/4 proxy
// (advice.TokenBytesRatio), the same proxy gb prime --size and gb advice audit
// use.
//
// FIDELITY: unlike prime_size_budget_test.go (synthetic corpus), this test
// renders against the REAL injected content — the canonical claude-instructions
// config beads (seeded by bridge.EnsureConfigs, the exact production seed) plus
// the committed real advice-catalog snapshot (testdata/advice_catalog_snapshot.json,
// shared with real_catalog_budget_test.go). So a directive re-leak into either
// the workflow-context config OR the advice corpus turns this test red.
//
// Enforcement is a per-archetype frozen baseline with a TWO-SIDED 20% band: the
// test fails if any archetype's directive density (or raw directive count)
// spikes >= 20% ABOVE baseline, if its @-mention count exceeds the baseline plus
// a small headroom, OR — the staleness floor — if its directive count/density
// falls >= 20% BELOW baseline. The lower bound exists because a spike ceiling
// alone silently rots: it guards a fixed +20% band above the RECORDED floor, so
// a remediation that cuts the real floor without re-baselining detaches that band
// from the live surface and the guard tolerates an ever-larger true spike
// (gd-qnzvcBBsiK). When a directive change is deliberate — a rise past the +20%
// ceiling or a cut past the -20% floor — re-measure from the BASELINE log lines
// this test emits and update the baselines below in the same MR (with a note on
// why the imperative pressure moved).

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// directiveSpikeTolerance is the fraction above baseline a rendered archetype's
// directive count / density may grow before the guard trips. 0.20 == the "fail
// on a >= 20% spike" contract from the task (gd-YhQIU5ckK2).
const directiveSpikeTolerance = 0.20

// stalenessTolerance is the mirror of directiveSpikeTolerance: how far BELOW its
// frozen baseline a rendered archetype may fall before the guard forces a
// re-baseline. The spike ceiling only ever guards a fixed distance (+20%) above
// the recorded floor, so if a remediation drops the real floor but the recorded
// baseline is never re-measured down, the +20% band silently detaches from the
// live surface and the guard tolerates an ever-larger true spike (gd-qnzvcBBsiK:
// the 2026-07-25 baselines survived the F3/F4/F5 cut of 2026-07-26 unchanged and
// drifted ~2x above the post-remediation floor, so a guard built to fire at +20%
// silently tolerated +74..127%). Failing when current <= baseline*(1-tolerance)
// keeps the sensitivity band pinned to the live floor — a floor drop of >= 20%
// must be ratcheted into the baseline in the same MR, exactly as a spike must.
const stalenessTolerance = 0.20

// mentionHeadroom is the absolute number of extra @-mention references a
// rendered archetype may accrue over its baseline before the guard trips. Set
// small (the task's "max 3 @-mentions per context" framing) because @-mentions
// are specific-agent/handle references — an over-specificity smell that should
// grow only deliberately.
const mentionHeadroom = 3

// directiveMarkers is the emphatic-directive marker set, aligned with
// scripts/directive-density.py's PRIMARY set (MUST/NEVER/DO NOT/CRITICAL/
// Prohibited) extended with the other emphatic markers the task enumerates
// (ALWAYS/IMPORTANT/REQUIRED/WARNING). "DO NOT" is matched case-insensitively
// on NOT because these corpora overwhelmingly write the emphatic negative as
// "Do NOT"/"do NOT".
var directiveMarkers = map[string]*regexp.Regexp{
	"MUST":       regexp.MustCompile(`\bMUST\b`),
	"NEVER":      regexp.MustCompile(`\bNEVER\b`),
	"DO NOT":     regexp.MustCompile(`\b[Dd][Oo] NOT\b`),
	"ALWAYS":     regexp.MustCompile(`\bALWAYS\b`),
	"CRITICAL":   regexp.MustCompile(`\bCRITICAL\b`),
	"Prohibited": regexp.MustCompile(`\bProhibited\b`),
	"IMPORTANT":  regexp.MustCompile(`\bIMPORTANT\b`),
	"REQUIRED":   regexp.MustCompile(`\bREQUIRED\b`),
	"WARNING":    regexp.MustCompile(`\bWARNING\b`),
}

// mentionRE matches an @-mention CANDIDATE: an @ (not preceded by an email
// local-part character, so `gardenio@acme.example` never matches on the `@p`) and
// a following handle of an initial letter plus 2+ handle characters. Handle
// characters deliberately EXCLUDE `.` so a dotted email domain or a filename
// extension is not swallowed into the handle — capture group 1 is the bare
// handle, and countMentions post-filters the remaining false positives.
//
// The final class was tightened (gd-82eN3qgk7m) from the original broad
// `@[A-Za-z][A-Za-z0-9_.-]{2,}` because that form counted three CLI/email idioms
// as @-mentions: the `@file.md`/`@report.md` CLI file-argument idiom (a handful
// of advice beads document `gb escalation --context @file.md`) and the
// `gardenio@acme.example` bot email. Those are not specific-agent references, so
// they inflated the mention budget this guard claims to measure. countMentions
// drops the residual @file / @<name>.<ext> file-argument shapes; the leading
// negative-lookbehind class here drops the email case.
var mentionRE = regexp.MustCompile(`(^|[^A-Za-z0-9._%+@-])@([A-Za-z][A-Za-z0-9_-]{2,})`)

// fileArgMentionHandles are bare CLI file-argument idioms (`@file`, as in `kd
// create -d @file`) that read as @-mentions but reference a file, not an agent.
// The dotted forms (`@file.md`, `@report.md`, `@issue.json`) are dropped
// structurally by countMentions' extension check; this set covers the
// extension-less `@file` idiom that check cannot see.
var fileArgMentionHandles = map[string]bool{"file": true}

// countDirectives returns the total emphatic-directive count and the per-marker
// breakdown for the given rendered text.
func countDirectives(text []byte) (total int, per map[string]int) {
	per = make(map[string]int, len(directiveMarkers))
	for name, re := range directiveMarkers {
		n := len(re.FindAll(text, -1))
		per[name] = n
		total += n
	}
	return total, per
}

// countMentions returns the number of @-mention references in the rendered
// text, excluding CLI file-argument idioms (`@file`, `@report.md`) and email
// addresses (`gardenio@acme.example`). mentionRE already excludes the email case
// (its leading negative class rejects an `@` glued to an email local part) and
// captures the bare handle in group 1; here we additionally drop the file-arg
// idioms: the bare `@file` names in fileArgMentionHandles, and any `@<handle>`
// immediately followed by a `.` + alnum filename extension (`@report.md`,
// `@issue.json`).
func countMentions(text []byte) int {
	n := 0
	for _, m := range mentionRE.FindAllSubmatchIndex(text, -1) {
		// m[2]:m[3] is capture group 1 (leading char, may be empty at BOL);
		// m[4]:m[5] is capture group 2 (the bare handle).
		handleStart, handleEnd := m[4], m[5]
		if fileArgMentionHandles[string(text[handleStart:handleEnd])] {
			continue
		}
		// `@handle.ext` file-argument shape: handle followed by `.` + an
		// alphanumeric (extension). Real handles are not followed by a dotted
		// extension here because mentionRE's handle class excludes `.`.
		if handleEnd+1 < len(text) && text[handleEnd] == '.' {
			if c := text[handleEnd+1]; (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
				continue
			}
		}
		n++
	}
	return n
}

// proxyTokens converts a byte length to the fleet's bytes/4 proxy-token count,
// the same estimate gb prime --size and gb advice audit use.
func proxyTokens(nBytes int) int {
	return nBytes / advice.TokenBytesRatio
}

// densityPer1k returns emphatic directives per 1k proxy tokens.
func densityPer1k(directives, tokens int) float64 {
	if tokens == 0 {
		return 0
	}
	return float64(directives) / float64(tokens) * 1000
}

// realConfigBudgetBeads captures the canonical config beads the production
// controller seeds (bridge.EnsureConfigs) and projects them into the
// budgetBead shape startBudgetDaemon serves. This is what makes the test
// measure the REAL workflow-context directives (the init_prompts.go hot spot,
// 56% of corpus directives per the baseline audit) rather than a synthetic
// stand-in — with zero duplication of the prompt text.
func realConfigBudgetBeads(t *testing.T) []budgetBead {
	t.Helper()
	setter := &densityConfigSetter{}
	if err := bridge.EnsureConfigs(context.Background(), setter, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}
	out := make([]budgetBead, 0, len(setter.beads))
	for _, b := range setter.beads {
		out = append(out, budgetBead{
			ID: b.ID, Title: b.Title, Description: b.Description,
			Type: "config", Kind: "config", Status: "open",
			Labels: b.Labels,
		})
	}
	return out
}

// realAdviceBudgetBeads projects the committed real advice-catalog snapshot
// (shared with real_catalog_budget_test.go) into the budgetBead shape. Reuses
// loadAdviceSnapshot so both tests stay pinned to the same frozen corpus.
func realAdviceBudgetBeads(t *testing.T) []budgetBead {
	t.Helper()
	beads := loadAdviceSnapshot(t)
	out := make([]budgetBead, 0, len(beads))
	for _, b := range beads {
		out = append(out, budgetBead{
			ID: b.ID, Title: b.Title, Description: b.Description,
			Type: "advice", Kind: b.Kind, Status: b.Status,
			Labels: b.Labels, Fields: b.Fields,
		})
	}
	return out
}

// TestPrimeDirectiveDensityBudget renders the full gb prime body for each
// (role, project) archetype we ship — against the REAL config + advice content
// — and fails if the emphatic-directive count, directive density (per 1k
// tokens), or @-mention count regresses past its frozen baseline + tolerance.
//
// The archetype set includes the generic crew/gardener rows AND every gardenio
// gardener SUB-role (gardener-delivery, plan-gardener, gardener-release-owner, …),
// because a differentiated gardener subscribes — via advice.AppendRoleFamilySubs
// — to the shared role:gardener pool PLUS its own role:<sub-role> pool, and that
// sub-role-scoped advice is INVISIBLE to the generic-gardener baseline. Without
// a per-sub-role row a directive re-leak into e.g. role:gardener-release-owner
// advice would sail through CI green (gd-q1ASEY6UPM). A completeness guard makes
// the coverage self-maintaining: it fails if the frozen fleet grid ever surfaces
// a gardenio gardener-family archetype with no baseline row here.
func TestPrimeDirectiveDensityBudget(t *testing.T) {
	cleanup := startBudgetDaemon(t, realAdviceBudgetBeads(t), realConfigBudgetBeads(t))
	defer cleanup()

	// Force a clean flag state — the render must not depend on cobra parsing.
	oldFor, oldNoAdvice, oldSize := primeForAgent, primeNoAdvice, primeSizeOnly
	primeNoAdvice = false
	primeSizeOnly = false
	defer func() {
		primeForAgent, primeNoAdvice, primeSizeOnly = oldFor, oldNoAdvice, oldSize
	}()

	// Per-archetype frozen baselines. Re-measure and update in-MR when directives
	// change on purpose (a deliberate rise past the +20% spike ceiling OR a
	// deliberate cut past the -20% staleness floor), reading the new numbers off
	// the BASELINE log lines this test emits.
	//
	// Re-baselined 2026-08-07 (gd-qnzvcBBsiK) to the post-remediation floor. The
	// original values were measured 2026-07-25 (commit 1b9322a55), ONE DAY before
	// the F3/F4/F5 progressive-disclosure cut (MR !6070, commit 5778fc93d,
	// 2026-07-26) roughly halved the rendered directive load — and were never
	// re-measured down. The stale-high baselines (crew/gardenio 21, gardener/gardenio
	// 30, crew/monorepo 34) sat ~2x above the live floor (11/17/23), so the +20%
	// spike ceiling had detached from the surface and silently tolerated a
	// +74..127% directive spike. The staleness floor added in this MR now trips if
	// a future remediation drops the floor >= 20% without ratcheting these numbers,
	// so the guard cannot go toothless the same way again.
	cases := []struct {
		name       string
		role       string
		project    string
		agent      string
		directives int     // frozen emphatic-directive count
		mentions   int     // frozen @-mention count
		density    float64 // frozen directives per 1k proxy tokens
	}{
		// Re-baselined 2026-11-01 (gd-FQqvaIw8SH) at the advice-snapshot refresh
		// that replaced the 2026-09-01 corpus (365 beads) with the live one (26).
		// Every row fell to 4 directives, all four the "DO NOT" lines in the
		// global workflow-context config: the advice beads that carried the
		// rest (RWX, monorepo and cs doctrine among them) are closed. Earlier
		// pins (2026-08-14, gd-RrifqICPIn) sat at 18-30.
		//
		// Rows follow the fleet grid (testdata/fleet_archetypes_snapshot.json),
		// plus local, a shipped laptop role that has no pod. crew/monorepo and the
		// gardener sub-roles (gardener-bugfix, -c3-bake, -delivery,
		// -release-owner, -remediate-gardener, -rescope) were dropped with this
		// refresh: none of them is in the grid, and the completeness guard below
		// asks for a row again if one respawns.
		{"crew/gardenio", "crew", "gardenio", "gardenio/crews/budget-density-crew", 4, 0, 0.65},
		{"gardener/gardenio", "gardener", "gardenio", "gardenio/gardeners/budget-density-gardener", 4, 0, 0.64},
		// local (gd-rIHQbpZR0Y): role:local overrides workflows + lifecycle with
		// zero directives of their own; all are inherited from the global
		// sections.
		{"local/gardenio", "local", "gardenio", "gardenio/locals/budget-density-local", 4, 0, 0.72},

		// gardenio gardener-family roles in the fleet grid. A differentiated
		// gardener subscribes, via AppendRoleFamilySubs, to role:gardener PLUS its
		// own role:<sub-role> pool, and that sub-role advice is invisible to the
		// generic-gardener row, so each one in the grid gets its own row
		// (gd-q1ASEY6UPM). "gardener,plan-gardener" is a live agent bead whose role
		// field holds two roles; it is in the grid, so it is pinned here as it
		// renders.
		{"plan-gardener/gardenio", "plan-gardener", "gardenio", "gardenio/plan-gardeners/budget-density-plan-gardener", 4, 0, 0.67},
		{"gardener,plan-gardener/gardenio", "gardener,plan-gardener", "gardenio", "gardenio/gardener-plan-gardeners/budget-density-gardener-plan-gardener", 4, 0, 0.63},
	}

	// Completeness guard: every gardenio gardener-family archetype in the frozen
	// fleet grid must have a baseline row above, so a fleet refresh that
	// surfaces a new differentiated gardener sub-role (e.g. gardener-e2e /
	// gardener-intake respawning into the window) cannot silently ship an
	// unguarded injected surface. Mirrors the nudge-prompt guard's "every
	// shipped template must have a baseline row" completeness check.
	baselinedGardenioGardeners := make(map[string]bool, len(cases))
	for _, c := range cases {
		if c.project == "gardenio" && isGardenerFamilyRole(c.role) {
			baselinedGardenioGardeners[c.role] = true
		}
	}
	for _, a := range loadFleetArchetypeSnapshot(t) {
		if a.Project != "gardenio" || !isGardenerFamilyRole(a.Role) {
			continue
		}
		if !baselinedGardenioGardeners[a.Role] {
			t.Errorf("fleet archetype gardenio/%s is a gardener-family role with no baseline row in TestPrimeDirectiveDensityBudget — its distinct sub-role advice pool is unguarded; add a row (render it and read the BASELINE log line) so a directive re-leak into role:%s advice turns CI red",
				a.Role, a.Role)
		}
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("BOAT_ROLE", c.role)
			t.Setenv("BOAT_PROJECT", c.project)
			t.Setenv("KD_ACTOR", "")
			t.Setenv("KD_AGENT_ID", "")
			t.Setenv("BOAT_AGENT_STATE", "")
			t.Setenv("BOAT_TASK_ID", "skip-auto-assign")
			primeForAgent = c.agent

			body, _, _ := buildPrimeBody(c.agent, c.role)
			raw := body.Bytes()

			directives, per := countDirectives(raw)
			mentions := countMentions(raw)
			tokens := proxyTokens(len(raw))
			density := densityPer1k(directives, tokens)

			t.Logf("BASELINE %s: directives=%d mentions=%d bytes=%d tokens=%d density=%.2f/1k per=%v",
				c.name, directives, mentions, len(raw), tokens, density, compactPerMarker(per))

			// Sanity: the rendered body must be non-trivial, else a broken
			// render would pass every budget vacuously.
			if len(raw) < 2000 {
				t.Fatalf("%s: rendered prime body suspiciously small (%d bytes) — render likely broke", c.name, len(raw))
			}

			pct := int(directiveSpikeTolerance * 100)

			// 1. Raw directive-count spike guard (bloat in absolute imperatives).
			//    Fail when the count spikes >= 20% above baseline.
			countThreshold := float64(c.directives) * (1 + directiveSpikeTolerance)
			if float64(directives) >= countThreshold {
				t.Errorf("%s: directive count %d spiked >= %d%% above baseline %d (threshold %.1f) — rewrite negatives to positive intent or re-baseline in-MR (see gardenio/docs/audits/2026-07-25-directive-density/)",
					c.name, directives, pct, c.directives, countThreshold)
			}

			// 2. Directive-DENSITY spike guard — the headline over-constraint
			//    metric: fail on a >= 20% density spike even if bytes are fine.
			densityThreshold := c.density * (1 + directiveSpikeTolerance)
			if density >= densityThreshold {
				t.Errorf("%s: directive density %.2f/1k spiked >= %d%% above baseline %.2f/1k (threshold %.2f) — prompt over-constraint regression the byte budget is blind to; audit with scripts/directive-density.py or re-baseline in-MR",
					c.name, density, pct, c.density, densityThreshold)
			}

			// 3. @-mention budget — specific-reference over-specificity.
			mentionCap := c.mentions + mentionHeadroom
			if mentions > mentionCap {
				t.Errorf("%s: @-mention count %d > cap %d (baseline %d + %d headroom) — prefer role/principle framing over naming specific agents/handles, or re-baseline in-MR",
					c.name, mentions, mentionCap, c.mentions, mentionHeadroom)
			}

			stalePct := int(stalenessTolerance * 100)

			// 4. Directive-count STALENESS floor — the mirror of guard 1. A spike
			//    ceiling alone only guards a fixed +20% band ABOVE the recorded
			//    baseline; if a remediation cuts the real floor but nobody
			//    re-baselines, that band detaches and the guard silently tolerates
			//    an ever-larger true spike (gd-qnzvcBBsiK). Fail when the count
			//    falls >= 20% BELOW baseline so a floor drop must be ratcheted into
			//    the baseline in the same MR, keeping the +20% band pinned to the
			//    live surface.
			countFloor := float64(c.directives) * (1 - stalenessTolerance)
			if float64(directives) <= countFloor {
				t.Errorf("%s: directive count %d fell >= %d%% below baseline %d (floor %.1f) — a remediation cut the directive load; re-baseline this row DOWN in the same MR so the +%d%% spike ceiling stays pinned to the live floor (see gardenio/docs/audits/2026-07-25-directive-density/)",
					c.name, directives, stalePct, c.directives, countFloor, int(directiveSpikeTolerance*100))
			}

			// 5. Directive-DENSITY staleness floor — same mirror for the headline
			//    density metric.
			densityFloor := c.density * (1 - stalenessTolerance)
			if density <= densityFloor {
				t.Errorf("%s: directive density %.2f/1k fell >= %d%% below baseline %.2f/1k (floor %.2f) — re-baseline this row DOWN in the same MR so the density spike ceiling stays pinned to the live floor",
					c.name, density, stalePct, c.density, densityFloor)
			}
		})
	}
}

// isGardenerFamilyRole reports whether a role is in the gardener family — it
// carries "gardener" (or "gardeners") as a hyphen-delimited token. This is the
// SAME token-boundary rule advice.AppendRoleFamilySubs uses to decide that a
// differentiated role (gardener-delivery, plan-gardener, …) inherits the shared
// gardener advice pool, so the completeness guard's notion of "gardener-family"
// can never drift from what the production matcher actually family-expands. A
// prefix glob would wrongly miss the suffix forms (plan-gardener, cost-gardener);
// a substring test would wrongly match "recaptcha" — only a token match is
// correct (advice.AppendRoleFamilySubs / ka-0000003366).
func isGardenerFamilyRole(role string) bool {
	for _, tok := range strings.Split(role, "-") {
		if tok == "gardener" || tok == "gardeners" {
			return true
		}
	}
	return false
}

// compactPerMarker drops zero-count markers so the BASELINE log stays legible.
func compactPerMarker(per map[string]int) map[string]int {
	out := make(map[string]int, len(per))
	for k, v := range per {
		if v > 0 {
			out[k] = v
		}
	}
	return out
}

// densityConfigSetter is a minimal in-memory bridge.ConfigSetter used to
// capture the canonical config beads bridge.EnsureConfigs seeds, without a live
// daemon. Mirrors internal/bridge/init_test.go's mockConfigSetter.
type densityConfigSetter struct {
	beads []*beadsapi.BeadDetail
}

func (m *densityConfigSetter) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	id := "gd-cfg-" + req.Title + "-" + strings.Join(req.Labels, ",")
	m.beads = append(m.beads, &beadsapi.BeadDetail{
		ID: id, Title: req.Title, Type: req.Type,
		Description: req.Description, Labels: req.Labels, Status: "open",
	})
	return id, nil
}

func (m *densityConfigSetter) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{Beads: m.beads, Total: len(m.beads)}, nil
}

func (m *densityConfigSetter) UpdateBeadDescription(_ context.Context, beadID, description string) error {
	for _, b := range m.beads {
		if b.ID == beadID {
			b.Description = description
			break
		}
	}
	return nil
}
