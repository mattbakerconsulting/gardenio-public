package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPlanCardSearchText asserts the client-side search haystack folds the
// fields an operator types to find a plan (title / id / project / status /
// operator attribution / current step title), lowercased and space-joined,
// skipping empty parts — mirroring mapsCardSearchText.
func TestPlanCardSearchText(t *testing.T) {
	row := planRowView{
		ID:                "gd-plan9",
		Title:             "Ship the Search Bar",
		Project:           "gardenio",
		Status:            "in_progress",
		OperatedByDisplay: "Matt Baker",
		OperatedByEmail:   "matt@acme.example",
		OperatedBySlug:    "matt.baker",
		OperatedBy:        "oidc:matt@acme.example",
		CreatedBy:         "agent-x",
		CurrentOrNextStep: &planCardStep{Title: "Wire the Route"},
	}
	got := planCardSearchText(row)
	want := "ship the search bar gd-plan9 gardenio in_progress matt baker matt@acme.example matt.baker oidc:matt@acme.example agent-x wire the route"
	if got != want {
		t.Errorf("planCardSearchText = %q, want %q", got, want)
	}
	for _, tok := range []string{"search bar", "gd-plan9", "gardenio", "matt baker", "wire the route"} {
		if !strings.Contains(got, tok) {
			t.Errorf("planCardSearchText %q missing token %q", got, tok)
		}
	}
}

// TestPlanCardSearchText_SkipsEmpty ensures absent fields (no attribution, no
// step) leave no stray separators in the haystack.
func TestPlanCardSearchText_SkipsEmpty(t *testing.T) {
	got := planCardSearchText(planRowView{ID: "gd-p1", Title: "Bare Plan", Status: "open"})
	if want := "bare plan gd-p1 open"; got != want {
		t.Errorf("planCardSearchText = %q, want %q", got, want)
	}
}

// TestPlansIndex_RendersSearchBox proves the populated index ships the
// (hidden, JS-revealed) progressive search box and stamps every card with its
// data-search haystack.
func TestPlansIndex_RendersSearchBox(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "data-plans-search") {
		t.Error("populated index should render the progressive search box")
	}
	// The seed plan's haystack: title + id + status fold in, lowercased.
	if !strings.Contains(body, `data-search="plan: ship the /plans page gd-plan1`) {
		t.Errorf("plan card should carry its lowercased data-search haystack; body lacks it")
	}
}

// TestPlansIndex_EmptyIndexOmitsSearchBox proves the cold-start (no plans)
// page renders no search box — nothing to filter.
func TestPlansIndex_EmptyIndexOmitsSearchBox(t *testing.T) {
	mux := mkServer(t, newFake())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "data-plans-search") {
		t.Error("empty index should not render the search box")
	}
}
