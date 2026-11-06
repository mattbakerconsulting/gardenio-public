package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestHandleDecisions_Empty(t *testing.T) {
	f := newFake()
	f.decisionResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/decisions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "No open decisions.") {
		t.Errorf("expected empty-state copy, body=%s", body)
	}
	if len(f.listAllQ) == 0 {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	q := f.listAllQ[0]
	if got := q.Types; len(got) != 1 || got[0] != "decision" {
		t.Errorf("types = %v, want [decision]", got)
	}
	if got := q.Statuses; len(got) != 1 || got[0] != "open" {
		t.Errorf("statuses = %v, want [open]", got)
	}
	if q.Sort != "-created_at" {
		t.Errorf("sort = %q, want -created_at", q.Sort)
	}
	if len(q.Labels) != 0 {
		t.Errorf("labels = %v, want none (no project filter)", q.Labels)
	}
}

func TestHandleDecisions_NonEmpty_RendersCards(t *testing.T) {
	f := newFake()
	f.decisionResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-dec1",
				Title:     "Pick a state backend",
				Type:      "decision",
				Status:    "open",
				Labels:    []string{"project:gardenio"},
				CreatedAt: time.Now().Add(-3 * time.Hour),
				CreatedBy: "gardener-bot",
				Fields: map[string]string{
					"prompt":  "Choose a TF state backend before module skeleton lands.",
					"options": `[{"id":"s3","short":"S3","label":"S3 + DynamoDB lock","artifact_type":"plan"},{"id":"tfc","short":"TFC","label":"Terraform Cloud","artifact_type":"plan"}]`,
				},
			},
			{
				ID:        "gd-dec2",
				Title:     "Defer the cutover",
				Type:      "decision",
				Status:    "open",
				CreatedAt: time.Now().Add(-2 * 24 * time.Hour),
				Fields: map[string]string{
					"requested_by": "alice",
				},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/decisions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		"gd-dec1", "Pick a state backend",
		"gd-dec2", "Defer the cutover",
		"Choose a TF state backend",
		`href="/gd-dec1"`,
		`href="/gd-dec2"`,
		">S3<", ">TFC<",
		"gardenio",
		"requested by gardener-bot",
		"requested by alice",
		"resolve",
		"3h",
		"2d",
		// Each option chip must carry the parent decision's bead ID, not an
		// empty {{$.ID}}, so the resolve POST hits /b/<id>/decision/resolve
		// instead of /b//decision/resolve (404). Regression for gd-ZhoF2Fvbqr.
		`data-bead-id="gd-dec1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("decisions page missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, `data-bead-id=""`) {
		t.Errorf("decisions page rendered empty data-bead-id on a resolve chip — would 404 on POST /b//decision/resolve\nbody=%s", body)
	}
	if strings.Contains(body, "No open decisions.") {
		t.Errorf("unexpected empty-state when decisions present")
	}
}

func TestHandleDecisions_RendersOperatorChip(t *testing.T) {
	f := newFake()
	f.decisionResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:                "gd-dec-op",
				Title:             "Pick a backend",
				Type:              "decision",
				Status:            "open",
				CreatedAt:         time.Now().Add(-1 * time.Hour),
				CreatedBy:         "gardener-bot",
				OperatedBy:        "slack:U0MATT",
				OperatedByDisplay: "Matt Baker",
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/decisions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The operator chip must render the resolved display name for the
	// decision's operated_by, so the list discloses who each decision
	// belongs to (gd-RbOeud56dN).
	for _, want := range []string{
		"bead-row-operator",
		"operator-chip",
		"Matt Baker",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("decisions page missing operator chip token %q\nbody=%s", want, body)
		}
	}
}

// decisionProjectFixture seeds three open decisions across two projects plus
// one unlabeled, so the project facet row has something real to count.
func decisionProjectFixture() *beadsapi.ListBeadsResult {
	return &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-gb1", Title: "Gardenio one", Type: "decision", Status: "open",
				Labels: []string{"project:gardenio"}, CreatedAt: time.Now().Add(-1 * time.Hour),
			},
			{
				ID: "gd-gb2", Title: "Gardenio two", Type: "decision", Status: "open",
				Labels: []string{"project:gardenio"}, CreatedAt: time.Now().Add(-2 * time.Hour),
			},
			{
				ID: "gd-mono1", Title: "Monorepo one", Type: "decision", Status: "open",
				Labels: []string{"project:monorepo"}, CreatedAt: time.Now().Add(-3 * time.Hour),
			},
			{
				ID: "gd-bare", Title: "No project", Type: "decision", Status: "open",
				CreatedAt: time.Now().Add(-4 * time.Hour),
			},
		},
		Total: 4,
	}
}

// The project predicate is applied in-handler, NOT as a project:<x> label on
// the daemon query — the facet row needs the unfiltered set to count every
// project's queue depth. Regression for ka-0000004140.
func TestHandleDecisions_ProjectFilter_FiltersInHandler(t *testing.T) {
	f := newFake()
	f.decisionResp = decisionProjectFixture()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/decisions?project=gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(f.listAllQ) == 0 {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	if got := f.listAllQ[0].Labels; len(got) != 0 {
		t.Errorf("labels = %v, want none — the project filter must NOT narrow the query, or the facet counts go blind", got)
	}
	body := w.Body.String()
	for _, want := range []string{"gd-gb1", "gd-gb2"} {
		if !strings.Contains(body, want) {
			t.Errorf("in-project decision %s missing from filtered page\nbody=%s", want, body)
		}
	}
	for _, unwanted := range []string{"gd-mono1", "gd-bare"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("out-of-project decision %s leaked into ?project=gardenio page", unwanted)
		}
	}
}

// The project selector must render as a real chip row (the shared filter_row
// partial) with one clickable chip per project that has an open decision,
// each carrying its count — the page previously exposed NO way to pick a
// project short of hand-editing the URL (ka-0000004140).
func TestHandleDecisions_RendersProjectSelector(t *testing.T) {
	f := newFake()
	f.decisionResp = decisionProjectFixture()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/decisions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`aria-label="Project"`,
		`href="/decisions?project=gardenio"`,
		`href="/decisions?project=monorepo"`,
		// "All" is the active chip on an unfiltered render.
		`chip chip-active`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("decisions project selector missing %q\nbody=%s", want, body)
		}
	}
}

// Selecting a project must not silently reset the mine/all scope toggle: every
// chip URL carries ?all=1 through when the viewer opted into the full queue.
func TestHandleDecisions_ProjectSelectorPreservesShowAll(t *testing.T) {
	f := newFake()
	f.decisionResp = decisionProjectFixture()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/decisions?all=1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/decisions?all=1&amp;project=gardenio"`) {
		t.Errorf("project chip dropped the ?all=1 scope\nbody=%s", body)
	}
}

// A ?project=<x> that matches nothing must still light a chip so the operator
// can see (and click off) the pinned scope rather than have it vanish.
func TestHandleDecisions_ProjectFilter_EmptyKeepsActiveChip(t *testing.T) {
	f := newFake()
	f.decisionResp = decisionProjectFixture()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/decisions?project=nosuch", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/decisions?project=nosuch"`) {
		t.Errorf("active-but-empty project lost its chip\nbody=%s", body)
	}
	if !strings.Contains(body, "all projects") {
		t.Errorf("expected an escape hatch back to all projects, body=%s", body)
	}
}

func TestHandleDecisions_DoesNotRenderSaveViewForm(t *testing.T) {
	f := newFake()
	f.decisionResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/decisions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "Save this view") {
		t.Errorf("save-view form should not appear on /decisions (moved to /search)")
	}
}

func TestHandleDecisions_DaemonError_RendersBanner(t *testing.T) {
	f := newFake()
	f.listErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/decisions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to load decisions") {
		t.Errorf("expected error banner, body=%s", w.Body.String())
	}
}
