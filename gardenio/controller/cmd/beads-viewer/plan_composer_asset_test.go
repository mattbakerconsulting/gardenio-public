package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Enter-to-send keybinding (task ka-0000002000, Phase B of plan epic
// ka-0000004110; operator ask ka-0000000394: "enter should send the message,
// shift+enter can be newline") is pure client-side JS in plan_composer.js.
// There is no JS test runner in CI, so — like plan_gutter_anchor_asset_test.go
// — this Go test guards the two things that would silently regress the feature:
// (1) the asset is embedded + served, and (2) the shipped file still carries
// the behavioural contract markers the composer depends on.
func TestPlanComposerAsset_ServedStatic(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/plan_composer.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("plan_composer.js: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if len(body) < 500 {
		t.Fatalf("plan_composer.js suspiciously small: %d bytes", len(body))
	}
	// Each marker maps to an acceptance point of the operator ask:
	//   keydown / Enter       — the send gesture entry point.
	//   evt.shiftKey          — Shift+Enter falls through to a native newline.
	//   isComposing / 229     — IME composition is never accidentally sent.
	//   requestSubmit         — send routes through the form's real submit path
	//                           (htmx hx-post + clear/restore lifecycle), not a
	//                           bespoke POST.
	//   preventDefault        — the newline the textarea would insert is swallowed.
	//   plan-assistant-input  — the handler is scoped to the plan composer only.
	//   plan-assistant-form   — the form it submits.
	for _, want := range []string{
		"'keydown'",
		"'Enter'",
		"evt.shiftKey",
		"isComposing",
		"229",
		"requestSubmit",
		"preventDefault",
		"plan-assistant-input",
		"plan-assistant-form",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plan_composer.js missing contract marker %q", want)
		}
	}
}

// TestPlanDetail_IncludesComposerScript: the /plans/{id} cockpit must actually
// pull in plan_composer.js — the asset being served is inert unless the page
// references it. Guards the <script> include the feature depends on.
func TestPlanDetail_IncludesComposerScript(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "/assets/static/plan_composer.js") {
		t.Errorf("plan detail page missing plan_composer.js script include")
	}
	// The discoverability hint next to Send documents the gesture for operators.
	if !strings.Contains(body, "plan-assistant-kbd") {
		t.Errorf("plan detail composer missing the Enter-to-send keybinding hint")
	}
}
