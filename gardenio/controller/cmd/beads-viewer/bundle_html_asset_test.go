package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// bundle_html.js drives two bundle-page enhancements that the bundle html-doc
// renderer's sandboxed iframe depends on: auto-resize to content (kill the
// inner scrollbar, ka-0000004212) and binding bead-link hover previews inside
// the same-origin frame (ka-0000006557). It must be embedded and served, or
// both regress silently.
func TestBundleHTMLAsset_ServedStatic(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/bundle_html.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("bundle_html.js: got %d want 200", w.Code)
	}
	if got := w.Body.Len(); got < 1000 {
		t.Errorf("bundle_html.js suspiciously small: %d bytes", got)
	}
}
