package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mkClosedIterationWithWrapup builds a closed iteration agent bead carrying a
// wrapup JSON with the given tldr + accomplishments, so the invocations drawer
// can split the two for the detail toggle.
func mkClosedIterationWithWrapup(id, gardenerID, tldr, accomplishments string, created time.Time) *beadsapi.BeadDetail {
	b := mkIteration(id, gardenerID, "", 1, "closed", created)
	wrap := map[string]any{"tldr": tldr, "accomplishments": accomplishments}
	raw, _ := json.Marshal(wrap)
	b.Fields["wrapup"] = string(raw)
	return b
}

func TestHandleGardenerInvocations_TLDROnlyByDefault(t *testing.T) {
	d := newFakeDaemon()
	// Two closed runs with distinct tldr + accomplishments.
	r1 := mkClosedIterationWithWrapup("gd-it1", "gd-cap", "shipped MR 42", "Merged MR 42; closed 3 beads", time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC))
	r2 := mkClosedIterationWithWrapup("gd-it2", "gd-cap", "drained the queue", "Spawned 4 crew; queue 12->0", time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC))
	r2.Fields["schedule_prior_agent_id"] = "gd-it1" // chain order: it1 -> it2
	d.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{r1, r2}, Total: 2}
	// BatchGetBeads must serve the raw wrapups.
	d.beads = map[string]*beadsapi.BeadDetail{"gd-it1": r1, "gd-it2": r2}

	mux := mkServerWithNow(t, d, gardenersNow)
	req := httptest.NewRequest("GET", "/gardener/gd-cap/invocations", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// TLDRs render as headlines.
	for _, want := range []string{"shipped MR 42", "drained the queue"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing tldr %q", want)
		}
	}
	// Accomplishments must NOT render in the default (tldr-only) view.
	if strings.Contains(body, "Merged MR 42; closed 3 beads") {
		t.Errorf("accomplishments leaked into TLDR-only view")
	}
	// Both detail tabs present.
	if !strings.Contains(body, "detail=full") || !strings.Contains(body, "detail=tldr") {
		t.Errorf("detail toggle tabs missing")
	}
}

// TestHandleGardenerInvocations_RunRowsLinkToRunBead guards the "runs dropdown
// doesn't work" bug (gd-9t0i0PjcPF): each invocation row must link to its own
// run/agent bead (/{runID}), NOT back to the gardener page (/gardener/{id}).
// Previously every row pointed at /gardener/gd-cap, so clicking a run in the
// drawer merely reloaded the same gardener page.
func TestHandleGardenerInvocations_RunRowsLinkToRunBead(t *testing.T) {
	d := newFakeDaemon()
	r1 := mkClosedIterationWithWrapup("gd-it1", "gd-cap", "shipped MR 42", "Merged MR 42; closed 3 beads", time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC))
	d.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{r1}, Total: 1}
	d.beads = map[string]*beadsapi.BeadDetail{"gd-it1": r1}

	mux := mkServerWithNow(t, d, gardenersNow)
	req := httptest.NewRequest("GET", "/gardener/gd-cap/invocations", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The run headline must link to the run bead.
	if !strings.Contains(body, `href="/gd-it1"`) {
		t.Errorf("invocation row should link to the run bead /gd-it1, body=%s", body)
	}
	// The "open full page" link still points at the gardener; that's the ONLY
	// remaining /gardener/gd-cap link (no run row should reuse it).
	if got := strings.Count(body, `href="/gardener/gd-cap"`); got != 1 {
		t.Errorf("expected exactly one /gardener/gd-cap link (the 'open full page' link), got %d; run rows must not link back to the gardener page", got)
	}
}

func TestHandleGardenerInvocations_FullShowsAccomplishments(t *testing.T) {
	d := newFakeDaemon()
	r1 := mkClosedIterationWithWrapup("gd-it1", "gd-cap", "shipped MR 42", "Merged MR 42; closed 3 beads", time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC))
	d.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{r1}, Total: 1}
	d.beads = map[string]*beadsapi.BeadDetail{"gd-it1": r1}

	mux := mkServerWithNow(t, d, gardenersNow)
	req := httptest.NewRequest("GET", "/gardener/gd-cap/invocations?detail=full", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "shipped MR 42") {
		t.Errorf("missing tldr headline")
	}
	if !strings.Contains(body, "Merged MR 42; closed 3 beads") {
		t.Errorf("full view must show accomplishments prose")
	}
}

// TestHandleGardenerInvocations_PerRunCost guards C2 (bug ka-0000001009): each
// invocation row surfaces that run's OWN cost so an operator can attribute a
// spend spike to a specific run, not just the gardener's cumulative roll-up.
func TestHandleGardenerInvocations_PerRunCost(t *testing.T) {
	d := newFakeDaemon()
	r1 := mkClosedIterationWithWrapup("gd-it1", "gd-cap", "shipped MR 42", "Merged MR 42", time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC))
	r1.Fields["total_cost_usd"] = "12.34" // this run cost $12.34
	d.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{r1}, Total: 1}
	d.beads = map[string]*beadsapi.BeadDetail{"gd-it1": r1}

	mux := mkServerWithNow(t, d, gardenersNow)
	req := httptest.NewRequest("GET", "/gardener/gd-cap/invocations", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "$12.34") {
		t.Errorf("invocation drawer must show the run's own cost $12.34; body=%s", body)
	}
}

func TestHandleGardenerInvocations_EmptyWhenNoRuns(t *testing.T) {
	d := newFakeDaemon()
	d.liveAgentsResp = &beadsapi.ListBeadsResult{}
	mux := mkServerWithNow(t, d, gardenersNow)
	req := httptest.NewRequest("GET", "/gardener/gd-cap/invocations", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No invocations yet") {
		t.Errorf("expected empty-state message, got %s", w.Body.String())
	}
}

func TestParseWrapUpTLDRAndAccomplishments(t *testing.T) {
	tldr, acc := parseWrapUpTLDRAndAccomplishments(`{"tldr":"  did a thing  ","accomplishments":"  X then Y  "}`)
	if tldr != "did a thing" {
		t.Errorf("tldr = %q, want trimmed 'did a thing'", tldr)
	}
	if acc != "X then Y" {
		t.Errorf("accomplishments = %q, want trimmed 'X then Y'", acc)
	}
	// Garbage / empty inputs yield empties, never a panic.
	if tl, a := parseWrapUpTLDRAndAccomplishments("not json"); tl != "" || a != "" {
		t.Errorf("garbage input should yield empties, got %q/%q", tl, a)
	}
	if tl, a := parseWrapUpTLDRAndAccomplishments(""); tl != "" || a != "" {
		t.Errorf("empty input should yield empties, got %q/%q", tl, a)
	}
}

func TestInvocationWrapupsByID_SkipsLiveRuns(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d)
	closed := mkClosedIterationWithWrapup("gd-c", "gd-cap", "done", "did stuff", gardenersNow())
	live := mkIteration("gd-l", "gd-cap", "", 1, "in_progress", gardenersNow())
	d.beads = map[string]*beadsapi.BeadDetail{"gd-c": closed, "gd-l": live}

	iters := []gardenerIterationView{
		{agentRowView: agentRowView{ID: "gd-c"}, IsLive: false},
		{agentRowView: agentRowView{ID: "gd-l"}, IsLive: true},
	}
	got := s.invocationWrapupsByID(context.Background(), iters)
	if _, ok := got["gd-l"]; ok {
		t.Errorf("live run must not be fetched for wrapup")
	}
	if _, ok := got["gd-c"]; !ok {
		t.Errorf("closed run wrapup should be present")
	}
}
