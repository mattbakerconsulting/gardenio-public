package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// plans_dark_coverage_test.go — iter-85 (ka-0000005352): a STRUCTURAL dark-skin
// coverage guard for the /plans index + peek drawer and the /deck mobile card.
//
// WHY THIS EXISTS. The 84 density iterations built an exhaustively dense card
// across four surfaces (index face · /deck face · peek header · cockpit roster),
// and each new signal hard-codes a LIGHT-mode text color. The iter-16 defect
// (ka-0000007937) and its fix established the rule "density illegible in dark
// mode is wasted density" and the palette every override lifts to, but its
// guard, TestPlansIndex_DarkSkinChipLegibility, keys on a CURATED ALLOWLIST of
// ~16 named classes. That allowlist is exactly the gap the standing audit lesson
// warns about verbatim: "a NEW light-only chip added later would still slip
// through until asserted here" — because the plans-ux-audit (PACES) harness the
// mission re-runs each iteration only ever renders the LIGHT skin, so a
// dark-skin regression is STRUCTURALLY INVISIBLE to the measurement.
//
// This guard closes that gap exhaustively. It parses the served <style> block,
// finds EVERY card-family rule that hard-codes a non-white text color, and
// requires each such class to be named by some html[data-skin^="dark"] override
// in the same sheet. A future light-only card chip reds CI at author time
// instead of shipping a white-on-white / dark-on-dark illegible signal.
//
// What this iter FOUND when first run (the fail-without proof): the guard was
// red on three real, never-fixed white-island bugs the curated allowlist missed
// — the whole /deck card (`.deck-card` inks background: var(--card-bg,#fff) with
// no dark --card-bg → a pure-#fff rectangle on the near-black dark page), the
// /deck P1-P4 prio pills + error/all-clear banners riding light ink on it, and
// the top-level /plans peek DRAWER (same white-island, plus its steps-notice /
// blocked / error chips). All three are fixed in the same MR; this test then
// stays green and fails-without by construction if any override is deleted.
//
// A class that sets only opacity/border/background (inheriting the themed chip
// color) is EXEMPT — that is the established opacity-only convention (e.g.
// .plan-card-crew-working, .plan-peek-crew-spend), and those inherit their color
// from a parent that IS themed. Only a HARD-CODED non-white `color:` needs its
// own dark override.

// cssColorRe matches a standalone `color: #hex` (not border-color/background-color:
// the negative class before `color` rejects a preceding `-` or word char).
var cssColorRe = regexp.MustCompile(`(?:^|[^-\w])color:\s*#([0-9a-fA-F]{3,6})`)

// cssRuleRe matches a single flat `selectors { body }` rule. Because the body
// class `[^{}]*` cannot span a nested brace, an `@media (...) { .a {…} }` wrapper
// is skipped and its INNER rules are matched flat (with the @media prefix
// dropped) — which is what we want: @media rules are responsive, not skin, and
// we key on the class token either way.
var cssRuleRe = regexp.MustCompile(`(?s)([^{}]+)\{([^{}]*)\}`)

// cssClassRe pulls every `.class-name` token out of a selector.
var cssClassRe = regexp.MustCompile(`\.([A-Za-z][\w-]*)`)

// darkSkinCoverageCase describes one surface to audit and which class prefixes
// are "card-family" signals on it.
type darkSkinCoverageCase struct {
	name     string
	target   string
	email    string
	prefixes []string
}

func extractStyle(body string) string {
	var b strings.Builder
	rest := body
	for {
		i := strings.Index(rest, "<style")
		if i < 0 {
			break
		}
		open := strings.Index(rest[i:], ">")
		if open < 0 {
			break
		}
		start := i + open + 1
		end := strings.Index(rest[start:], "</style>")
		if end < 0 {
			break
		}
		b.WriteString(rest[start : start+end])
		b.WriteString("\n")
		rest = rest[start+end:]
	}
	return b.String()
}

// hasHardcodedNonWhiteColor reports whether a rule body sets a `color:` to a
// hard-coded hex that is not white (#fff / #ffffff).
func hasHardcodedNonWhiteColor(body string) bool {
	m := cssColorRe.FindStringSubmatch(body)
	if m == nil {
		return false
	}
	hex := strings.ToLower(m[1])
	return hex != "fff" && hex != "ffffff"
}

func TestPlansSurfaces_DarkSkinCoverageIsExhaustive(t *testing.T) {
	cases := []darkSkinCoverageCase{
		{
			name:   "plans-index+peek",
			target: "/plans",
			email:  "matt@acme.example",
			// Every card-family class the index card + peek drawer render dense
			// signals with. chip-status-blocked* and chip-attention* are the plan
			// status/decision chips; the rest are the card/step/crew/mr/prio/
			// lifecycle/progress/escalation/stall/closeout families.
			prefixes: []string{
				"plan-card", "plan-step", "plan-prio", "plan-progress",
				"plan-esc", "plan-stall", "plan-closeout", "plan-lifecycle-chip",
				"plan-peek", "plan-steps-", "chip-attention", "chip-status-blocked",
			},
		},
		{
			name:   "deck",
			target: "/deck",
			email:  "matt@acme.example",
			prefixes: []string{
				"deck-card", "deck-prio", "plan-lifecycle-chip",
				// The card's reason banner and its blocked link (Homestead 2.3).
				"deck-reason", "deck-blocked",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := seedPlansFake()
			mux := mkServer(t, f)
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", tc.target, nil)
			if tc.email != "" {
				req.Header.Set("X-Auth-Request-Email", tc.email)
			}
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s: got %d, want 200", tc.target, w.Code)
			}
			style := extractStyle(w.Body.String())
			if style == "" {
				t.Fatalf("GET %s: no <style> block found in body", tc.target)
			}

			lightLeaves, missing := darkSkinUncovered(style, tc.prefixes)

			// Sanity floor: the walk must actually find card-family light rules,
			// or a future refactor that renames every class would make this test
			// vacuously pass. The index alone carries dozens.
			if len(lightLeaves) < 5 {
				t.Fatalf("GET %s: only found %d card-family light color rules — the parser or prefixes drifted (expected many)", tc.target, len(lightLeaves))
			}

			if len(missing) > 0 {
				for _, leaf := range missing {
					t.Errorf("GET %s: card chip .%s hard-codes a non-white light text color (from selector %q) but has NO html[data-skin^=\"dark\"] override — it will read illegible on the dark skin (\"density illegible in dark mode is wasted density\", ka-0000007937). Add a dark override, or make the chip inherit its parent's themed color (opacity-only) instead of hard-coding a color.", tc.target, leaf, lightLeaves[leaf])
				}
			}
		})
	}
}

// darkSkinUncovered walks every flat rule in style. It collects (a) the
// classes matching one of prefixes that hard-code a non-white color under a
// NON-dark selector, keyed on the leaf class with an example selector, and (b)
// every dark-skin selector, then returns the leaves no dark override names.
//
// A dark override counts only when Homestead night matches it too:
// html[data-skin^="dark"] or a selector naming dark-homestead. One keyed on
// html[data-skin="dark-terminal"] alone leaves the night skin on the light ink
// (Homestead 2.3, gd-3nQwIneabf), so it is neither a cover nor a light rule.
func darkSkinUncovered(style string, prefixes []string) (lightLeaves map[string]string, missing []string) {
	lightLeaves = map[string]string{} // leaf class -> example selector
	var darkSelectors strings.Builder

	inScope := func(cls string) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(cls, p) {
				return true
			}
		}
		return false
	}

	for _, m := range cssRuleRe.FindAllStringSubmatch(style, -1) {
		sel := strings.TrimSpace(m[1])
		body := m[2]
		if sel == "" || strings.HasPrefix(sel, "@") {
			continue
		}
		if strings.Contains(sel, "data-skin") && strings.Contains(sel, "dark") {
			if strings.Contains(sel, `data-skin^="dark"`) || strings.Contains(sel, "dark-homestead") {
				darkSelectors.WriteString(sel)
				darkSelectors.WriteString(" | ")
			}
			continue
		}
		if !hasHardcodedNonWhiteColor(body) {
			continue
		}
		classes := cssClassRe.FindAllStringSubmatch(sel, -1)
		if len(classes) == 0 {
			continue
		}
		// The leaf (rightmost) class is the element actually colored; an
		// ancestor like `.plan-peek` in `.plan-peek .plan-steps-notice`
		// only scopes it. Key coverage on the leaf.
		leaf := classes[len(classes)-1][1]
		if !inScope(leaf) {
			continue
		}
		if _, seen := lightLeaves[leaf]; !seen {
			lightLeaves[leaf] = sel
		}
	}

	darkBlob := darkSelectors.String()
	for leaf := range lightLeaves {
		// A dark override "covers" the leaf when some dark selector names
		// that exact class (word-boundary so `.plan-card` does not match
		// `.plan-card-crew`). The override may target the leaf directly
		// or via an ancestor selector that includes it.
		re := regexp.MustCompile(`\.` + regexp.QuoteMeta(leaf) + `(?:[^\w-]|$)`)
		if !re.MatchString(darkBlob) {
			missing = append(missing, leaf)
		}
	}
	sort.Strings(missing)
	return lightLeaves, missing
}
