package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// highlight.js scrolls a ?highlight= mention into view. An explicit
// behavior:'smooth' ignores the CSS scroll-behavior net in homestead.css, so
// the script must pick the behavior from prefers-reduced-motion itself
// (gd-D4WqjO2PNF). There is no JS test runner in CI, so — like
// deck_swipe_asset_test.go — this guards the shipped file's markers.
func TestHighlightAsset_HonoursReducedMotion(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/highlight.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("highlight.js: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "matchMedia('(prefers-reduced-motion: reduce)')") {
		t.Error("highlight.js no longer reads prefers-reduced-motion")
	}
	if strings.Contains(body, "behavior: 'smooth' }") {
		t.Error("highlight.js hard-codes behavior: 'smooth' in a scrollIntoView call; use scrollBehavior()")
	}
	if n := strings.Count(body, "behavior: scrollBehavior()"); n != 2 {
		t.Errorf("highlight.js: %d scrollIntoView calls use scrollBehavior(), want 2", n)
	}
}
