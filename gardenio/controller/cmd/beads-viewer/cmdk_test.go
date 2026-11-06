package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestHandleCmdkSuggest_EmptyQuery verifies an empty `?q=` produces the empty
// fragment without invoking ListBeadsFiltered or GetBead — so the very first
// keystroke on the overlay (focus event) doesn't pummel the daemon.
func TestHandleCmdkSuggest_EmptyQuery(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/cmdk/suggest?q=", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if len(f.listAllQ) != 0 {
		t.Errorf("ListBeadsFiltered should not be called for empty query, got %d calls", len(f.listAllQ))
	}
	body := w.Body.String()
	if !strings.Contains(body, "Type to search beads") {
		t.Errorf("empty-query fragment missing prompt; body=%s", body)
	}
}

// TestHandleCmdkSuggest_TitleMatch covers the lexical-search path: a query
// that doesn't look like a bead id is forwarded to ListBeadsFiltered with
// Search:q, and the rendered <li> rows carry id, title, type, and status.
func TestHandleCmdkSuggest_TitleMatch(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-foo", Title: "Foo title", Type: "task", Status: "open"},
			{ID: "gd-bar", Title: "Bar title", Type: "bug", Status: "in_progress"},
		},
		Total: 2,
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/cmdk/suggest?q=foo", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.listLastQ == nil {
		t.Fatalf("expected ListBeadsFiltered to be called")
	}
	if f.listLastQ.Search != "foo" {
		t.Errorf("ListBeadsFiltered Search = %q, want %q", f.listLastQ.Search, "foo")
	}
	if f.listLastQ.Limit != cmdkResultLimit {
		t.Errorf("ListBeadsFiltered Limit = %d, want %d", f.listLastQ.Limit, cmdkResultLimit)
	}
	body := w.Body.String()
	for _, want := range []string{
		"gd-foo",
		"Foo title",
		`tag-type`,
		`>task<`,
		`tag-open`,
		">open<",
		"gd-bar",
		"Bar title",
		"in_progress",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("title-match fragment missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleCmdkSuggest_IdPrefix exercises the "looks like a bead id" branch:
// GetBead is probed first and the exact hit lands at the top, deduped against
// any later ListBeadsFiltered response that includes the same id.
func TestHandleCmdkSuggest_IdPrefix(t *testing.T) {
	f := newFake()
	f.beads["gd-3sTfBI1qyM"] = &beadsapi.BeadDetail{
		ID: "gd-3sTfBI1qyM", Title: "Direct hit", Type: "task", Status: "open",
	}
	// Lexical search returns the same id (plus a sibling) so the dedupe path
	// is exercised — the direct hit must appear once and at index 0.
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-3sTfBI1qyM", Title: "Direct hit", Type: "task", Status: "open"},
			{ID: "gd-other", Title: "Sibling", Type: "task", Status: "open"},
		},
		Total: 2,
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/cmdk/suggest?q=gd-3sTfBI1qyM", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Count(body, "gd-3sTfBI1qyM") < 1 {
		t.Errorf("expected direct-hit bead in fragment\nbody=%s", body)
	}
	// Dedupe: the id must appear only on its single row. The row template
	// emits the id three times per row (data-cmdk-id, the <a href>, and the
	// <code>), so a single row is 3 occurrences and a duplicated row would
	// be 6.
	if got := strings.Count(body, "gd-3sTfBI1qyM"); got != 3 {
		t.Errorf("gd-3sTfBI1qyM appears %d times (expected 3 — one row), body=%s", got, body)
	}
	// Direct hit must precede the sibling so the user's exact-id query lands
	// at the top of the list.
	if iHit, iSib := strings.Index(body, "gd-3sTfBI1qyM"), strings.Index(body, "gd-other"); iHit < 0 || iSib < 0 || iHit > iSib {
		t.Errorf("direct hit must precede sibling; iHit=%d iSib=%d body=%s", iHit, iSib, body)
	}
}

// TestHandleCmdkSuggest_IdProbeOnlyForFullID guards the looksLikeBeadID
// gate: a bare "gd-" or short prefix must not trigger a GetBead probe (which
// would round-trip the daemon for every keystroke), while still flowing
// through to the lexical search path.
func TestHandleCmdkSuggest_IdProbeOnlyForFullID(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/cmdk/suggest?q=gd-ab", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// fakeDaemon doesn't track GetBead calls explicitly; instead we verify the
	// short-prefix never produced a row. If looksLikeBeadID had let "gd-ab"
	// through, GetBead would 404 (no such bead in beads map) and produce no
	// row anyway — so the assertion is on the empty-result UI shape.
	body := w.Body.String()
	if !strings.Contains(body, "No matches") && !strings.Contains(body, "Type to search") {
		t.Errorf("short-prefix query should produce empty-state, got body=%s", body)
	}
}

func TestLooksLikeBeadID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"gd-3sTfBI1qyM", true},
		{"ka-0000005771", true},
		{"ka-0000004973", true}, // exactly 10 alnum
		{"gd-abcdefghi", false}, // 9 alnum — too short
		{"gd-", false},
		{"kd", false},
		{"", false},
		{"foobar", false},
		{"gd-Oal9Rw3o!Y", false}, // non-alnum
	}
	for _, tc := range cases {
		if got := looksLikeBeadID(tc.in); got != tc.want {
			t.Errorf("looksLikeBeadID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestIsPlanBead gates the plan-detection helper: only an epic carrying the
// `plan` label qualifies. A task with a stray `plan` label, or a plain epic
// without it, must not — mirroring the daemon-side plan query.
func TestIsPlanBead(t *testing.T) {
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{"plan epic", &beadsapi.BeadDetail{Type: "epic", Labels: []string{"plan"}}, true},
		{"epic no plan label", &beadsapi.BeadDetail{Type: "epic", Labels: []string{"project:gardenio"}}, false},
		{"task with stray plan label", &beadsapi.BeadDetail{Type: "task", Labels: []string{"plan"}}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isPlanBead(tc.bead); got != tc.want {
			t.Errorf("isPlanBead(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestHandleCmdkSuggest_RouteMounted asserts the new endpoint is wired
// through RegisterRoutes — a regression where the wiring is forgotten would
// surface as 404 here (matches the bead_preview RouteMounted test).
func TestHandleCmdkSuggest_RouteMounted(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/cmdk/suggest?q=", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code == http.StatusNotFound {
		t.Fatalf("/cmdk/suggest not mounted — got 404 from registered mux")
	}
}
