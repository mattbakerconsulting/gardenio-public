package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
)

// The search panel's reader and the R3/R4 retirement (Lands workbench-glass spec WB-5, §2.6; step 5.7, gd-Oqh2suV8ap).

func searchIDs(d *gardenhub.SearchDoc) string {
	var ids []string
	for _, h := range d.Hits {
		ids = append(ids, h.ID)
	}
	return strings.Join(ids, " ")
}

// A query that is a bead id finds that bead first, then what semantic search finds; a plan is said to be one and a hit
// carries its land from its project label.
func TestGardenSearchIDThenSemantic(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	f := newFake()
	f.beads["gd-ExactBead1"] = &beadsapi.BeadDetail{ID: "gd-ExactBead1", Title: "The one typed", Type: "task", Status: "open", Priority: 1,
		Labels: []string{"project:gardenio"}, Description: "Line one.\nLine two.", UpdatedAt: at}
	f.semanticResp = &beadsapi.SemanticSearchResponse{Results: []*beadsapi.SemanticSearchResult{
		{Bead: &beadsapi.BeadDetail{ID: "gd-PlanBead01", Title: "A plan", Type: "epic", Status: "open", Labels: []string{"plan", "project:music"}}, Score: 0.9},
		nil,
		{Bead: nil, Score: 0.5},
	}}
	d, err := gardenSearch{NewServer(f, slog.Default(), "", "", "", nil)}.Search(context.Background(), "gd-ExactBead1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Mode != gardenhub.SearchSemantic || d.Note != "" || searchIDs(d) != "gd-ExactBead1 gd-PlanBead01" {
		t.Fatalf("mode %q note %q hits %q", d.Mode, d.Note, searchIDs(d))
	}
	e, p := d.Hits[0], d.Hits[1]
	if !e.Exact || e.Project != "gardenio" || e.Priority != 1 || e.Text != "Line one. Line two." || e.Updated != at.Unix() || e.Plan {
		t.Errorf("exact hit %+v", e)
	}
	if p.Exact || !p.Plan || p.Project != "music" {
		t.Errorf("plan hit %+v", p)
	}
	if f.semanticLastReq == nil || f.semanticLastReq.Query != "gd-ExactBead1" || f.semanticLastReq.Limit != gardenSearchLimit {
		t.Errorf("semantic request %+v", f.semanticLastReq)
	}

	// words are not an id: no probe
	f.getBeadCalls = 0
	if _, err := (gardenSearch{NewServer(f, slog.Default(), "", "", "", nil)}).Search(context.Background(), "edge idp"); err != nil || f.getBeadCalls != 0 {
		t.Errorf("words: err %v, %d id probes", err, f.getBeadCalls)
	}
}

// Semantic search failing degrades to the word search, as /search does, and the note says why: off (503) or not answering.
// With the word search failing too, an id hit still answers with a note; with nothing to show it is an error.
func TestGardenSearchFallsBackToLexical(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{{ID: "gd-lex1", Title: "Lexical hit", Type: "task", Status: "open"}}, Total: 1}
	srv := NewServer(f, slog.Default(), "", "", "", nil)

	f.semanticErr = &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable, Message: "semantic search disabled"}
	d, err := gardenSearch{srv}.Search(context.Background(), "anything")
	if err != nil || d.Mode != gardenhub.SearchLexical || searchIDs(d) != "gd-lex1" || !strings.Contains(d.Note, "is off") {
		t.Fatalf("503: err %v mode %q hits %q note %q", err, d.Mode, searchIDs(d), d.Note)
	}
	if f.listLastQ == nil || f.listLastQ.Search != "anything" || f.listLastQ.Limit != gardenSearchLimit {
		t.Errorf("lexical query %+v", f.listLastQ)
	}

	f.semanticErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "failed to embed query"}
	if d, err = (gardenSearch{srv}).Search(context.Background(), "anything"); err != nil || d.Mode != gardenhub.SearchLexical || !strings.Contains(d.Note, "did not answer") {
		t.Errorf("500: err %v mode %q note %q", err, d.Mode, d.Note)
	}

	f.listErr = errors.New("daemon down")
	if _, err = (gardenSearch{srv}).Search(context.Background(), "anything"); err == nil {
		t.Error("both searches failing with nothing to show: want an error")
	}
	d, err = gardenSearch{srv}.Search(context.Background(), "gd-aaa0000000")
	if err == nil {
		t.Errorf("an id that is not there and no search: want an error, got %+v", d)
	}
	f.beads["gd-aaa0000000"] = &beadsapi.BeadDetail{ID: "gd-aaa0000000", Title: "Found by id", Type: "bug", Status: "open"}
	if d, err = (gardenSearch{srv}).Search(context.Background(), "gd-aaa0000000"); err != nil || searchIDs(d) != "gd-aaa0000000" || !strings.Contains(d.Note, "Only the bead with this id") {
		t.Errorf("id hit with no search: err %v hits %q note %q", err, searchIDs(d), d.Note)
	}
}

// The bead page (R3) and /search (R4) say "Open it in the garden" to a viewer who reaches /garden (default on), and with
// their redirect flags on answer 302 to the bead panel and the search panel; an agent's page (its terminal) retires to
// the Glass instead, on its own flags (G-R1); an unknown id keeps its answer, and each retirement has its own flags.
func TestGardenBeadAndSearchRetirement(t *testing.T) {
	newSrv := func(t *testing.T, env map[string]string) http.Handler {
		f := newFake()
		f.beads["gd-agent00001"] = &beadsapi.BeadDetail{ID: "gd-agent00001", Title: "crew-1", Type: "agent", Status: "open"}
		f.semanticErr = &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable, Message: "off"}
		f.listResp = &beadsapi.ListBeadsResult{}
		srv := NewServer(f, slog.Default(), "/v", "", "", nil)
		if env != nil {
			srv.tunables = newWatcherWithEnv(t, env)
		}
		srv.SetGardenHandler(&gardenhub.Handler{Hub: gardenhub.New(nil, "/v", nil), BasePath: "/v"})
		return edgeAuthMux(srv)
	}
	get := func(h http.Handler, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Auth-Request-Email", "alice@example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	banner := func(w *httptest.ResponseRecorder) bool {
		return strings.Contains(w.Body.String(), `id="garden-banner"`)
	}

	on := newSrv(t, nil)
	for path, want := range map[string]string{
		"/gd-aaa":          `href="/v/garden?bead=gd-aaa"`,
		"/search?q=edge+x": `href="/v/garden?q=edge&#43;x"`,
		"/search":          `href="/v/garden"`,
	} {
		if w := get(on, path); w.Code != http.StatusOK || !banner(w) || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d, banner with %s missing", path, w.Code, want)
		}
	}
	if w := get(on, "/gd-agent00001"); w.Code != http.StatusOK || !banner(w) || !strings.Contains(w.Body.String(), `href="/v/garden?glass=gd-agent00001"`) {
		t.Errorf("an agent's page: %d, banner %v; want the page with the banner to its Glass", w.Code, banner(w))
	}
	if w := get(newSrv(t, map[string]string{gardenBeadBannerEnv: "false"}), "/gd-agent00001"); !banner(w) {
		t.Error("the bead banner's flag does not turn off the Glass one")
	}
	if w := get(newSrv(t, map[string]string{gardenGlassBannerEnv: "false"}), "/gd-agent00001"); banner(w) {
		t.Error("Glass banner flag off: no banner on an agent's page")
	}
	if w := get(newSrv(t, map[string]string{gardenBeadBannerEnv: "false"}), "/gd-aaa"); banner(w) {
		t.Error("bead banner flag off: no banner")
	}
	if w := get(newSrv(t, map[string]string{gardenBeadBannerEnv: "false"}), "/search?q=x"); !banner(w) {
		t.Error("the bead banner's flag does not turn off the search one")
	}

	redirect := newSrv(t, map[string]string{gardenBeadRedirectEnv: "true", gardenSearchRedirectEnv: "true"})
	for path, want := range map[string]string{
		"/gd-aaa":                    "/v/garden?bead=gd-aaa",
		"/gd-aaa?bead=1&at=gardenio": "/v/garden?at=gardenio&bead=gd-aaa",
		"/search?q=edge+idp":         "/v/garden?q=edge+idp",
		"/search?q=x&status=open":    "/v/garden?status=open&q=x",
		"/search":                    "/v/garden",
	} {
		if w := get(redirect, path); w.Code != http.StatusFound || w.Header().Get("Location") != want {
			t.Errorf("%s: %d to %q, want 302 to %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
	hx := httptest.NewRequest(http.MethodGet, "/gd-aaa", nil)
	hx.Header.Set("X-Auth-Request-Email", "alice@example.com")
	hx.Header.Set("HX-Request", "true")
	hw := httptest.NewRecorder()
	redirect.ServeHTTP(hw, hx)
	if hw.Code != http.StatusOK {
		t.Errorf("an htmx swap of the bead page with the redirect on: %d, want the page", hw.Code)
	}
	if w := get(redirect, "/gd-agent00001"); w.Code != http.StatusOK {
		t.Errorf("an agent's page with the bead redirect on: %d, want the page", w.Code)
	}
	glassOn := newSrv(t, map[string]string{gardenGlassRedirectEnv: "true"})
	if w := get(glassOn, "/gd-agent00001?x=1"); w.Code != http.StatusFound || w.Header().Get("Location") != "/v/garden?x=1&glass=gd-agent00001" {
		t.Errorf("an agent's page with the Glass redirect on: %d to %q, want 302 to its Glass", w.Code, w.Header().Get("Location"))
	}
	if w := get(glassOn, "/gd-agent00001?old=1"); w.Code != http.StatusOK {
		t.Errorf("?old=1 keeps an agent's page: %d", w.Code)
	}
	if w := get(glassOn, "/gd-aaa"); w.Code != http.StatusOK {
		t.Errorf("only the Glass redirect on: a plain bead's page %d, want the page", w.Code)
	}
	if w := get(redirect, "/gd-nothere99"); w.Code != http.StatusNotFound {
		t.Errorf("an unknown id with the redirect on: %d, want 404", w.Code)
	}
	if w := get(newSrv(t, map[string]string{gardenBeadRedirectEnv: "true"}), "/search?q=x"); w.Code != http.StatusOK {
		t.Errorf("only the bead redirect on: /search %d, want the page", w.Code)
	}
}
