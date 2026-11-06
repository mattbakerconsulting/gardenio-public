package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Step 5 (task ka-0000005925, plan ka-0000000492) adds horizontal swipe gestures
// to the mobile card-deck. The behaviour is pure client-side JS in deck_swipe.js.
// There is no JS test runner in CI, so — like plans_board_test.go and
// plan_composer_asset_test.go — this Go test guards the two things that would
// silently regress the feature: (1) the asset is embedded + served, and (2) the
// shipped file still carries the behavioural contract markers the deck depends
// on, including the ones that enforce the non-negotiable safety rule.
func TestDeckSwipeAsset_ServedStatic(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/deck_swipe.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("deck_swipe.js: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if len(body) < 1000 {
		t.Fatalf("deck_swipe.js suspiciously small: %d bytes", len(body))
	}
	// Each marker maps to a contract point of the DOM/gesture contract:
	//   data-deck / data-deck-card   — the gate + the dragged element.
	//   data-deck-skip / -back       — the horizontal nav controls a swipe clicks.
	//   data-deck-up                 — the up-flick target (open the cockpit);
	//                                  Step 5b (ka-0000005224), the "swipe up to
	//                                  action" criterion.
	//   deck:navigate                — the cancelable nav event (non-destructive).
	//   pointerdown / pointermove    — Pointer Events (mouse/trackpad/touch alike).
	//   pointercancel                — a real scroll cancels the pointer, so the
	//                                  up-flick (which acts only on a clean
	//                                  pointerup) can never fire while scrolling.
	//   setPointerCapture            — the drag owns the pointer once axis-locked.
	//   touch-action:pan-y           — vertical scroll stays native; we own x.
	//   maybeUpFlick                 — the up-flick decision, gated on no-scroll.
	//   prefers-reduced-motion       — snap/fly-off transitions honour the pref.
	//   blocksDrag                   — answer buttons/links never start a drag.
	for _, want := range []string{
		"data-deck",
		"data-deck-card",
		"data-deck-skip",
		"data-deck-back",
		"data-deck-up",
		"deck:navigate",
		"'pointerdown'",
		"'pointermove'",
		"'pointercancel'",
		"setPointerCapture",
		"touch-action:pan-y",
		"maybeUpFlick",
		"prefers-reduced-motion",
		"blocksDrag",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("deck_swipe.js missing contract marker %q", want)
		}
	}

	// The safety rule is that gestures NAVIGATE, never COMMIT — and that now
	// includes the up-flick, whose ONLY action is to open the cockpit (a read
	// surface). The module must carry no path to the resolve/dispatch/build verbs
	// — assert the destructive route substrings are entirely absent so a future
	// edit can't quietly wire a swipe (up OR sideways) to a commit without this
	// test going red.
	for _, forbidden := range []string{
		"/resolve",
		"/dispatch",
		"/build",
		"decision/resolve",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("deck_swipe.js must never reference a commit verb, found %q — "+
				"gestures navigate, they never commit (ka-0000005925 / ka-0000005224)", forbidden)
		}
	}
}

// The driver is only useful if it is actually loaded on the page. It is a
// self-gating global include in layout.html (like mermaid_render.js), so guard
// the <script> tag against an accidental removal.
func TestDeckSwipeAsset_ScriptTaggedInLayout(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /: got %d want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "/assets/static/deck_swipe.js") {
		t.Error("layout.html no longer script-tags deck_swipe.js — the swipe " +
			"driver would never load on /deck")
	}
}
