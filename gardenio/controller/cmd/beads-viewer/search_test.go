package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestComputeAgeSummary(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	mk := func(off time.Duration) searchResultView {
		return searchResultView{UpdatedAt: now.Add(-off)}
	}

	t.Run("empty_slice", func(t *testing.T) {
		oldest, newest, median := computeAgeSummary(nil, now)
		if oldest != "" || newest != "" || median != "" {
			t.Errorf("got (%q,%q,%q), want all empty", oldest, newest, median)
		}
	})

	t.Run("ignores_zero_time", func(t *testing.T) {
		views := []searchResultView{{}, {}, {}}
		oldest, _, _ := computeAgeSummary(views, now)
		if oldest != "" {
			t.Errorf("zero-time rows should be skipped, got oldest=%q", oldest)
		}
	})

	t.Run("range_and_median", func(t *testing.T) {
		views := []searchResultView{
			mk(10 * time.Minute),
			mk(1 * time.Hour),
			mk(2 * time.Hour),
			mk(24 * time.Hour),
			mk(72 * time.Hour),
		}
		oldest, newest, median := computeAgeSummary(views, now)
		if oldest != "3d" {
			t.Errorf("oldest = %q, want 3d", oldest)
		}
		if newest != "10m" {
			t.Errorf("newest = %q, want 10m", newest)
		}
		if median != "2h" {
			t.Errorf("median = %q, want 2h", median)
		}
	})
}

// ka-0000003067: the Operator column is always rendered. Rows without a
// human operator but with a CreatedBy agent identity fall back to an
// "(agent)" chip so the column is populated with real attribution signal
// instead of being hidden when sparse. The age summary still surfaces
// oldest/median/newest in the heading meta.
func TestHandleSearch_PopulatesOperatorColumnWithAgentFallback(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.semanticResp = &beadsapi.SemanticSearchResponse{
		Model: "nomic-embed-text",
		Results: []*beadsapi.SemanticSearchResult{
			{Bead: &beadsapi.BeadDetail{ID: "gd-a", Title: "A", Type: "task", Status: "open", CreatedBy: "agent-alpha", UpdatedAt: now.Add(-10 * time.Minute)}, Score: 0.9},
			{Bead: &beadsapi.BeadDetail{ID: "gd-b", Title: "B", Type: "task", Status: "open", CreatedBy: "agent-beta", UpdatedAt: now.Add(-2 * time.Hour)}, Score: 0.8},
			{Bead: &beadsapi.BeadDetail{ID: "gd-c", Title: "C", Type: "task", Status: "open", CreatedBy: "sched-cycle", UpdatedAt: now.Add(-72 * time.Hour)}, Score: 0.7},
			{Bead: &beadsapi.BeadDetail{ID: "gd-d", Title: "D", Type: "task", Status: "open", CreatedBy: "agent-delta", UpdatedAt: now.Add(-96 * time.Hour)}, Score: 0.6},
			{Bead: &beadsapi.BeadDetail{ID: "gd-e", Title: "E", Type: "task", Status: "open", UpdatedAt: now.Add(-120 * time.Hour), OperatedBy: "oidc:matt@example.com", OperatedByDisplay: "Matt"}, Score: 0.5},
		},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=anything", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()

	// Operator column header is always present.
	if !strings.Contains(body, "<th>Operator</th>") {
		t.Errorf("expected Operator <th> to always render\nbody=%s", body)
	}
	// Each row contributes signal: agent compact chips for agent-* CreatedBy
	// rows (ka-0000004740: search.html now renders the initials-only variant),
	// scheduler chip for sched-* row (compact chip drops to empty for kind=
	// scheduler so the template falls back to the full-chip rendering),
	// human chip for the Matt row.
	for _, want := range []string{
		// Raw agent names appear in the compact chip's title/data attrs.
		`title="agent-alpha"`,
		`title="agent-beta"`,
		`title="agent-delta"`,
		// Compact agent chip class.
		`operator-chip-compact is-agent`,
		// Scheduler still rendered via full-chip fallback in the search td.
		`(scheduler)`,
		// Human row carries the compact chip plus Matt's name on title.
		`title="Matt"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body\nbody=%s", want, body)
		}
	}
	// Pin: raw slack:U… never leaks into a visible monogram.
	if strings.Contains(body, ">slack:") {
		t.Errorf("raw slack: leaked into search row body\nbody=%s", body)
	}
	// No em-dash sentinel in the table when every row carries CreatedBy.
	if strings.Contains(body, `<span class="empty">&mdash;</span>`) {
		t.Errorf("expected no em-dash sentinel rows when CreatedBy is populated\nbody=%s", body)
	}
	// Age summary still appears.
	if !strings.Contains(body, "search-age-summary") {
		t.Errorf("expected age summary chip in heading, body=%s", body)
	}
}

// ka-0000007172: each result row carries a Priority chip and the heading
// renders type/status facet counts so /search has signal/pixel beyond the
// pre-existing bead-id, title, type, status, operator, age columns.
func TestHandleSearch_RendersPriorityChipAndFacets(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.semanticResp = &beadsapi.SemanticSearchResponse{
		Model: "nomic-embed-text",
		Results: []*beadsapi.SemanticSearchResult{
			{Bead: &beadsapi.BeadDetail{ID: "gd-a", Title: "A", Type: "task", Status: "open", Priority: 0, UpdatedAt: now.Add(-10 * time.Minute), CreatedBy: "agent-x"}, Score: 0.9},
			{Bead: &beadsapi.BeadDetail{ID: "gd-b", Title: "B", Type: "task", Status: "open", Priority: 2, UpdatedAt: now.Add(-1 * time.Hour), CreatedBy: "agent-x"}, Score: 0.8},
			{Bead: &beadsapi.BeadDetail{ID: "gd-c", Title: "C", Type: "bug", Status: "in_progress", Priority: 1, UpdatedAt: now.Add(-2 * time.Hour), CreatedBy: "agent-x"}, Score: 0.7},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=anything", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()

	for _, want := range []string{
		`<th>Pri</th>`,
		`chip-priority chip-priority-0`,
		`chip-priority chip-priority-1`,
		`chip-priority chip-priority-2`,
		`>P0<`, `>P1<`, `>P2<`,
		`search-facets`,
		// Type facet ordering: task (count 2) before bug (count 1).
		`2 task`, `1 bug`,
		// Status facet ordering: open (count 2) before in_progress (count 1).
		`2 open`, `1 in_progress`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body", want)
		}
	}
}

// /search heading renders project and age-bucket facets alongside the
// existing type/status facets so users can see the full breakdown of
// visible rows at a glance (ka-0000005947).
func TestHandleSearch_RendersProjectAndAgeBucketFacets(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.semanticResp = &beadsapi.SemanticSearchResponse{
		Model: "nomic-embed-text",
		Results: []*beadsapi.SemanticSearchResult{
			{Bead: &beadsapi.BeadDetail{ID: "gd-a", Title: "A", Type: "task", Status: "open", Labels: []string{"project:gardenio"}, UpdatedAt: now.Add(-10 * time.Minute)}, Score: 0.9},
			{Bead: &beadsapi.BeadDetail{ID: "gd-b", Title: "B", Type: "task", Status: "open", Labels: []string{"project:gardenio"}, UpdatedAt: now.Add(-5 * time.Hour)}, Score: 0.8},
			{Bead: &beadsapi.BeadDetail{ID: "gd-c", Title: "C", Type: "bug", Status: "open", Labels: []string{"project:monorepo"}, UpdatedAt: now.Add(-72 * time.Hour)}, Score: 0.7},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=anything", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{
		`search-facet-project`,
		`2 gardenio`,
		`1 monorepo`,
		`search-facet-age`,
		`1 &lt;1h`, // template auto-escapes the literal "<1h"
		`1 today`,
		`1 this week`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body\nbody=%s", want, body)
		}
	}
}

func TestComputeFacets_SortedByCountThenKey(t *testing.T) {
	views := []searchResultView{
		{Type: "task", Status: "open"},
		{Type: "task", Status: "open"},
		{Type: "bug", Status: "in_progress"},
		{Type: "feature", Status: "open"},
		{Type: "bug", Status: "open"},
		{Type: "", Status: ""}, // skipped — empty values produce no facet entries
	}
	types, statuses := computeFacets(views)

	wantTypes := []facetCount{
		{Key: "bug", Count: 2},
		{Key: "task", Count: 2},
		{Key: "feature", Count: 1},
	}
	if len(types) != len(wantTypes) {
		t.Fatalf("types len = %d, want %d (%+v)", len(types), len(wantTypes), types)
	}
	for i, w := range wantTypes {
		if types[i] != w {
			t.Errorf("types[%d] = %+v, want %+v", i, types[i], w)
		}
	}
	wantStatuses := []facetCount{
		{Key: "open", Count: 4},
		{Key: "in_progress", Count: 1},
	}
	if len(statuses) != len(wantStatuses) {
		t.Fatalf("statuses len = %d, want %d (%+v)", len(statuses), len(wantStatuses), statuses)
	}
	for i, w := range wantStatuses {
		if statuses[i] != w {
			t.Errorf("statuses[%d] = %+v, want %+v", i, statuses[i], w)
		}
	}
}

func TestComputeProjectFacets(t *testing.T) {
	views := []searchResultView{
		{Labels: []string{"project:gardenio", "kind:info-density"}},
		{Labels: []string{"project:gardenio"}},
		{Labels: []string{"project:monorepo", "project:gardenio"}}, // both projects counted once each
		{Labels: []string{"project:monorepo"}},
		{Labels: []string{"area:beads-viewer"}}, // no project label — skipped
		{Labels: nil},                           // no labels — skipped
		{Labels: []string{"project:", "project:gardenio"}}, // empty project name skipped, duplicate within bead deduped
	}
	got := computeProjectFacets(views)
	want := []facetCount{
		{Key: "gardenio", Count: 4},
		{Key: "monorepo", Count: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d (%+v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d] = %+v want %+v", i, got[i], w)
		}
	}
}

func TestComputeAgeBucketFacets(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	mk := func(off time.Duration) searchResultView {
		return searchResultView{UpdatedAt: now.Add(-off)}
	}
	views := []searchResultView{
		mk(10 * time.Minute),    // <1h
		mk(30 * time.Minute),    // <1h
		mk(5 * time.Hour),       // today
		mk(48 * time.Hour),      // this week
		mk(10 * 24 * time.Hour), // this month
		mk(60 * 24 * time.Hour), // older
		{},                      // zero time skipped
	}
	got := computeAgeBucketFacets(views, now)
	want := []facetCount{
		{Key: "<1h", Count: 2},
		{Key: "today", Count: 1},
		{Key: "this week", Count: 1},
		{Key: "this month", Count: 1},
		{Key: "older", Count: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d (%+v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d] = %+v want %+v", i, got[i], w)
		}
	}
}

func TestComputeAgeBucketFacets_OmitsEmptyBuckets(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	views := []searchResultView{
		{UpdatedAt: now.Add(-10 * time.Minute)},
		{UpdatedAt: now.Add(-15 * time.Minute)},
	}
	got := computeAgeBucketFacets(views, now)
	if len(got) != 1 || got[0].Key != "<1h" || got[0].Count != 2 {
		t.Errorf("got %+v want one bucket [<1h:2]", got)
	}
}

// Rows with a resolved human operator render the human chip — the agent
// fallback only kicks in when operated_by is empty.
func TestHandleSearch_HumanOperatorChipUnchanged(t *testing.T) {
	f := newFake()
	f.semanticResp = &beadsapi.SemanticSearchResponse{
		Model: "nomic-embed-text",
		Results: []*beadsapi.SemanticSearchResult{
			{Bead: &beadsapi.BeadDetail{ID: "gd-a", Title: "A", Type: "task", Status: "open", OperatedBy: "oidc:a@x", OperatedByDisplay: "A", CreatedBy: "agent-foo"}, Score: 0.9},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=anything", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "<th>Operator</th>") {
		t.Errorf("expected Operator <th>\nbody=%s", body)
	}
	// The agent fallback chip class only appears in row markup, not in the
	// inline stylesheet — guard against it leaking into a row where a human
	// operator is set by checking the table body cell.
	if strings.Contains(body, `class="operator-chip operator-chip-agent"`) {
		t.Errorf("agent fallback must not override a human operator chip\nbody=%s", body)
	}
}

func TestHighlightQuery(t *testing.T) {
	cases := []struct {
		name, text, query, want string
	}{
		{"empty_query", "Hello world", "", "Hello world"},
		{"empty_text", "", "hello", ""},
		{"no_match", "Hello world", "xyz", "Hello world"},
		{"single_match", "Hello world", "world", `Hello <strong class="search-hl">world</strong>`},
		{"case_insensitive", "Hello WORLD", "world", `Hello <strong class="search-hl">WORLD</strong>`},
		{"multi_token", "Auth login bug", "auth bug", `<strong class="search-hl">Auth</strong> login <strong class="search-hl">bug</strong>`},
		{"overlapping_tokens_merged", "abcabc", "abc bca", `<strong class="search-hl">abcabc</strong>`},
		{"html_escaped", "<script>foo</script>", "foo", `&lt;script&gt;<strong class="search-hl">foo</strong>&lt;/script&gt;`},
		{"whitespace_query", "Hello world", "   ", "Hello world"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(highlightQuery(tc.text, tc.query))
			if got != tc.want {
				t.Errorf("highlightQuery(%q,%q) = %q, want %q", tc.text, tc.query, got, tc.want)
			}
		})
	}
}

// /search defaults to compact density: snippet column is omitted, the table
// carries the search-results-compact class, and the toggle UI exposes a "list"
// opt-out chip (ka-0000005947).
func TestHandleSearch_DefaultsToCompactDensity(t *testing.T) {
	f := newFake()
	f.semanticResp = &beadsapi.SemanticSearchResponse{
		Model: "nomic-embed-text",
		Results: []*beadsapi.SemanticSearchResult{
			{Bead: &beadsapi.BeadDetail{ID: "gd-a", Title: "A", Type: "task", Status: "open", Description: "long-snippet-text-payload"}, Score: 0.9},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=anything", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{
		`search-results-compact`,
		`search-density-toggle`,
		`<span class="chip chip-active">compact</span>`,
		`?density=list`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in default-density body", want)
		}
	}
	if strings.Contains(body, `<th>Snippet</th>`) {
		t.Errorf("compact density should omit the Snippet column header")
	}
	if strings.Contains(body, `long-snippet-text-payload`) {
		t.Errorf("compact density should not render snippet text in row")
	}
}

// Explicit ?density=list opts out of compact: snippet column returns and the
// toggle chip flips so the user can switch back.
func TestHandleSearch_DensityListOptOut(t *testing.T) {
	f := newFake()
	f.semanticResp = &beadsapi.SemanticSearchResponse{
		Model: "nomic-embed-text",
		Results: []*beadsapi.SemanticSearchResult{
			{Bead: &beadsapi.BeadDetail{ID: "gd-a", Title: "A", Type: "task", Status: "open", Description: "long-snippet-text-payload"}, Score: 0.9},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=anything&density=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{
		`search-results-list`,
		`<th>Snippet</th>`,
		`long-snippet-text-payload`,
		`<span class="chip chip-active">list</span>`,
		`?density=compact`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in list-density body", want)
		}
	}
	if strings.Contains(body, `class="search-results search-results-compact"`) {
		t.Errorf("list density should not carry the compact table class")
	}
}

// /search renders matched query tokens inside the title cell as bold spans
// so users can pinpoint why a row hit (ka-0000005947).
func TestHandleSearch_HighlightsTitleQueryMatches(t *testing.T) {
	f := newFake()
	f.semanticResp = &beadsapi.SemanticSearchResponse{
		Model: "nomic-embed-text",
		Results: []*beadsapi.SemanticSearchResult{
			{Bead: &beadsapi.BeadDetail{ID: "gd-a", Title: "Auth refactor for login bug", Type: "bug", Status: "open"}, Score: 0.9},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=auth+bug", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{
		`<strong class="search-hl">Auth</strong>`,
		`<strong class="search-hl">bug</strong>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body", want)
		}
	}
}

// TestHandleSearch_DegradesToLexicalOn500 pins the ka-0000000389 fix: when the
// daemon's semantic endpoint returns HTTP 500 (embedder configured but its call
// failed — e.g. the new-prod empty-BEADS_EMBED_API_KEY bug where the in-cluster
// LiteLLM proxy 401s), /search must quietly fall back to the healthy lexical
// path and NOT render the hard red "Search failed" error banner. Before the fix
// only 503 (embedder disabled) degraded; 500 skipped the fallback.
func TestHandleSearch_DegradesToLexicalOn500(t *testing.T) {
	f := newFake()
	f.semanticErr = &beadsapi.APIError{StatusCode: 500, Message: "failed to embed query"}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-lex1", Title: "Lexical hit", Type: "task", Status: "open"},
		},
		Total: 1,
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=embedding+broken", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()

	// Degraded banner is shown (graceful), with the generic non-noop reason.
	if !strings.Contains(body, "Semantic search unavailable.") {
		t.Errorf("expected degraded banner on 500, body=%s", body)
	}
	if !strings.Contains(body, "temporarily unavailable") {
		t.Errorf("expected generic (non-noop) degraded reason on 500, body=%s", body)
	}
	// The lexical fallback ran and its results rendered.
	if !strings.Contains(body, "gd-lex1") || !strings.Contains(body, "lexical") {
		t.Errorf("expected lexical fallback results on 500, body=%s", body)
	}
	// The hard error banner must NOT render. ("banner-error" also appears in the
	// base-layout CSS, so assert on the error-mode-only rendered <div> + its
	// "Search failed:" prefix instead of the bare class token.)
	if strings.Contains(body, `<div class="banner banner-error">`) || strings.Contains(body, "Search failed:") {
		t.Errorf("500 must degrade, not hard-error; body=%s", body)
	}
}

// TestHandleSearch_DegradesToLexicalOn503 confirms the pre-existing 503
// (BEADS_EMBED_BACKEND=noop) path still degrades and still carries the
// noop-specific reason string after the widening.
func TestHandleSearch_DegradesToLexicalOn503(t *testing.T) {
	f := newFake()
	f.semanticErr = &beadsapi.APIError{StatusCode: 503, Message: "semantic search disabled"}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-lex2", Title: "Lexical hit 2", Type: "task", Status: "open"},
		},
		Total: 1,
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=anything", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()

	if !strings.Contains(body, "Semantic search unavailable.") {
		t.Errorf("expected degraded banner on 503, body=%s", body)
	}
	if !strings.Contains(body, "BEADS_EMBED_BACKEND=noop") {
		t.Errorf("expected noop-specific degraded reason on 503, body=%s", body)
	}
	if !strings.Contains(body, "gd-lex2") {
		t.Errorf("expected lexical fallback results on 503, body=%s", body)
	}
	if strings.Contains(body, `<div class="banner banner-error">`) {
		t.Errorf("503 must degrade, not hard-error; body=%s", body)
	}
}
