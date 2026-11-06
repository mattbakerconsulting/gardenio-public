package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The 3-tier anchor resolver (T6, ka-0000000456, epic ka-0000002316) is pure
// client-side JS in plan_gutter.js. There is no JS test runner in CI, so this
// Go test guards the two things that would silently regress the feature: (1)
// the asset is embedded + served, and (2) the resolver contract markers the
// overlay-render integration depends on are still present in the shipped file.
// The resolver's tier logic is exercised by scratchpad/verify_resolver.js
// (mirrored DOM harness) at author time.
func TestPlanGutterAnchorResolver_ServedStatic(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/plan_gutter.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("plan_gutter.js: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if len(body) < 1000 {
		t.Fatalf("plan_gutter.js suspiciously small: %d bytes", len(body))
	}
	// Each marker maps to an acceptance criterion / contract point of T6:
	//   resolveAnchor            — the 3-tier resolver entry point.
	//   fuzzySubstringDistance   — tier-3 bounded-fuzzy matcher.
	//   plan-narrative           — the prose container the resolver scopes to
	//                              (excludes the overlay's own data-block-id LIs).
	//   critique-comment-orphaned — tier-0 orphan class (never silent mis-anchor).
	//   Document changed — anchor lost — the visible orphan indicator text.
	//   console.debug            — tier-resolution logging.
	for _, want := range []string{
		"function resolveAnchor(",
		"function fuzzySubstringDistance(",
		"data-block-id",
		"plan-narrative",
		"critique-comment-orphaned",
		"Document changed — anchor lost",
		"console.debug",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plan_gutter.js missing T6 resolver marker %q", want)
		}
	}
	// Highlight-to-critique (ka-0000003210): a prose selection must OPEN the tray
	// bound to the mapped step. These markers guard the contract points that
	// integration depends on — the mouseup entry point, the section-slug map read
	// from the step rows, and the selected-step affordance — so a refactor that
	// silently drops the "highlight opens the tray" gesture reds this test.
	for _, want := range []string{
		"function openTrayForSelection(", // mouseup entry point for highlight→open
		"function sectionSlugForBlock(",  // prose block → narrative section slug
		"data-section-slug",              // the step-row attribute the map reads
		"function stepRowForSlug(",       // slug → step row (prefers open steps)
		"function fallbackStep(",         // always-open guarantee when no section maps
		"critique-step-selected",         // the bound-step tint
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plan_gutter.js missing highlight-to-critique marker %q", want)
		}
	}
}
