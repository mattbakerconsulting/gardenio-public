package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Homestead surface pass, step 2.3 (gd-3nQwIneabf): Plans, the cockpit, the
// deck, Gardeners, the gardener page and Maps. The night skin gets every dark
// fix through html[data-skin^="dark"], so these pages must never key a dark
// override on html[data-skin="dark-terminal"] alone (gardeners.html and
// maps.html did, 89 rules, and left Homestead night on #595959 ink), and the
// dark-terminal coverage guard (plans_dark_coverage_test.go) holds for the
// gardener pages too, not only /plans and /deck.

type homesteadSurface struct {
	name, target string
	seed         func(t *testing.T) *fakeDaemon
	// prefixes are the page's own class families for the coverage walk; nil
	// skips it (/plans and /deck have theirs in plans_dark_coverage_test.go,
	// and /plans keeps its curated list: its new-plan fields are a fixed
	// light surface on every skin, #1a1a1a on #fff, by design).
	prefixes []string
}

var homesteadPlansMapsSurfaces = []homesteadSurface{
	{name: "plans", target: "/plans", seed: func(*testing.T) *fakeDaemon { return seedPlansFake() }},
	{name: "cockpit", target: "/plans/gd-plan1", seed: func(*testing.T) *fakeDaemon { return seedPlansFake() }, prefixes: []string{"plan-"}},
	{name: "deck", target: "/deck", seed: func(*testing.T) *fakeDaemon { return seedPlansFake() }},
	{name: "gardeners", target: "/gardeners", seed: seedGardenersIndexFake, prefixes: []string{"gardeners-", "cap-"}},
	{name: "gardener", target: "/gardener/gd-cap", seed: seedGardenerPageFake, prefixes: []string{"gardener-"}},
	{name: "maps", target: "/maps", seed: seedMapsFake, prefixes: []string{"maps-"}},
}

func renderHomesteadSurface(t *testing.T, s homesteadSurface) string {
	t.Helper()
	mux := mkServer(t, s.seed(t))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", s.target, nil)
	req.Header.Set("X-Auth-Request-Email", "matt@acme.example")
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: got %d, want 200", s.target, w.Code)
	}
	style := extractStyle(w.Body.String())
	if style == "" {
		t.Fatalf("GET %s: no <style> block found in body", s.target)
	}
	return style
}

func TestHomesteadNight_SurfaceDarkCoverage(t *testing.T) {
	for _, s := range homesteadPlansMapsSurfaces {
		t.Run(s.name, func(t *testing.T) {
			style := renderHomesteadSurface(t, s)
			for _, m := range cssRuleRe.FindAllStringSubmatch(style, -1) {
				if sel := strings.TrimSpace(m[1]); strings.Contains(sel, `data-skin="dark-terminal"`) {
					t.Errorf("GET %s: %q keys a dark override on Dark terminal alone; use html[data-skin^=\"dark\"] so Homestead night gets it too", s.target, sel)
				}
			}
			if s.prefixes == nil {
				return
			}
			lightLeaves, missing := darkSkinUncovered(style, s.prefixes)
			if len(lightLeaves) < 5 {
				t.Fatalf("GET %s: only found %d light color rules for %v — the parser or prefixes drifted", s.target, len(lightLeaves), s.prefixes)
			}
			for _, leaf := range missing {
				t.Errorf("GET %s: .%s hard-codes a light text color (from %q) with no html[data-skin^=\"dark\"] override; it reads illegible on both dark skins", s.target, leaf, lightLeaves[leaf])
			}
		})
	}
}

// homestead.css paints these pages on the skin's paper: the cards read
// --card-bg, which the skin points at --gd-surface (so their buttons and
// fields follow), and the rules double the gate, because the pages' own
// <style> blocks come after the stylesheet and Dark terminal's overrides tie
// a single gate. The gardener page's breadcrumb is a second <nav> under body:
// it must not take the shelf's window anchor or its fence.
func TestHomesteadCSS_PlansGardenersMapsSurfaces(t *testing.T) {
	css := readHomesteadCSS(t)
	i := strings.Index(css, "/* ---- Surface pass (2.3)")
	if i < 0 {
		t.Fatal("homestead.css has no Surface pass (2.3) section")
	}
	sec := css[i:]
	sec = sec[:strings.Index(sec, "\n/* ---- ")+1]
	const gate = `html:is([data-skin="homestead"], [data-skin="dark-homestead"]):is([data-skin="homestead"], [data-skin="dark-homestead"])`
	for _, want := range []string{
		gate + ` :is(.plan-detail, .plan-card, .plan-empty-hero, .plan-peek-drawer, .plan-new-panel, .deck-card, .deck-scope select) {
  --card-bg: var(--gd-surface);`,
		`.maps-card, .gardener-header, .gardener-trajectory) {`,
		gate + ` .plans-lane {`,
		gate + ` :is(.gardeners-summary-tile, .maps-summary-tile) {`,
		gate + ` .cap-table thead th {`,
		gate + ` :is(.plan-new-btn, .plan-new-submit) {`,
		gate + ` body:not(.gd-login) > nav ~ nav { anchor-name: none;`,
		`body > nav ~ nav::after { content: none; }`,
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("the 2.3 section lacks %q", want)
		}
	}
	// The cockpit's night fields sit on the skin's paper, not Dark terminal's #11161d.
	if !strings.Contains(sec, ".plan-assistant-input, .plan-step-add-input,") || !strings.Contains(sec, "background-color: var(--bg); color: var(--text);") {
		t.Error("the 2.3 section must put the plan fields on var(--bg) with var(--text)")
	}
}
