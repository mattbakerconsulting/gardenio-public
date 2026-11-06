package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The /up type-conditional footer + global keymap (task ka-0000004641) is driven
// by up.js. CI has no JS test runner, so — like deck_swipe_asset_test.go — this
// Go test guards the two things that would silently regress the feature: (1) the
// asset is embedded + served, and (2) the shipped file still carries the
// behavioural + safety contract markers.
func TestUpAsset_ServedStatic(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/up.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("up.js: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if len(body) < 1000 {
		t.Fatalf("up.js suspiciously small: %d bytes", len(body))
	}
	// Contract markers:
	//   data-surface="up"  — the self-gate; up.js no-ops everywhere else.
	//   UP_KEYMAP          — the JSON island that is the single source of truth.
	//   chatboxFocused     — the §8.4 chatbox-suspend gate stub.
	//   data-up-deferred   — the honest phase-toast path for sibling endpoints.
	//   actionForKey       — the §4.2 key→action resolver (space/j/arrow aliases).
	for _, want := range []string{
		`data-surface="up"`,
		"UP_KEYMAP",
		"chatboxFocused",
		"data-up-deferred",
		"actionForKey",
		"/b/{id}/comments",
		"/b/{id}/decision/resolve",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("up.js missing contract marker %q", want)
		}
	}

	// Safety: up.js must only ever POST to the /b/{id}/* endpoints the footer
	// model declares. It must NOT hard-code a raw daemon path (/v1/...) or a
	// destructive verb the footer doesn't route — dispatch always flows through
	// the keymap/data-up-* attributes, never a baked-in URL.
	for _, forbidden := range []string{
		"/v1/",           // never talk to the daemon directly
		"DELETE",         // no bead deletion from the card
		"/b/{id}/labels", // labels editor is cmd-K only (PRD §3.2)
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("up.js references %q — /up dispatches only the footer's declared "+
				"/b/{id}/* actions (PRD §3.2/§10)", forbidden)
		}
	}
}

// The driver is only useful if it is actually loaded on the page. It is a
// self-gating global include in layout.html (like deck_swipe.js), so guard the
// <script> tag against accidental removal.
func TestUpAsset_ScriptTaggedInLayout(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /: got %d want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "/assets/static/up.js") {
		t.Error("layout.html no longer script-tags up.js — the /up keymap driver " +
			"would never load")
	}
}
