package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestProjectRow_RendersCompactOperatorChip pins that the project_row
// partial renders the `.operator-chip-compact` element when the row has
// an operated_by source (human or agent). Bead-shape covered: row
// constructed by projectViewFromBead.
func TestProjectRow_RendersCompactOperatorChip(t *testing.T) {
	now := time.Now()
	b := &beadsapi.BeadDetail{
		ID:                "gd-x",
		Title:             "Has operator",
		Type:              "task",
		Status:            "open",
		OperatedBy:        "slack:U0OPERATOR1",
		OperatedByDisplay: "Matt Baker",
		OperatedByEmail:   "matt@example.com",
		UpdatedAt:         now,
	}
	v := projectViewFromBead(b, "viewer@example.com", "gardenio", "/b")
	if v.OperatorChipCompactHTML == "" {
		t.Fatalf("expected OperatorChipCompactHTML to be populated for human op bead")
	}
	out := renderProjectRow(t, v)
	if !strings.Contains(out, "operator-chip-compact") {
		t.Errorf("project row missing .operator-chip-compact element: %s", out)
	}
	if !strings.Contains(out, ">MB<") {
		t.Errorf("project row missing MB initials: %s", out)
	}
	// Pin: raw slack:U… must not leak into the rendered initials.
	if strings.Contains(out, ">slack:") {
		t.Errorf("raw slack: leaked into row body: %s", out)
	}
}

// TestProjectRow_NoCompactChipWhenNoOperator pins that rows with no
// operator and no createdBy don't carry a compact chip — the row falls
// back to its existing meta layout without an empty wrapper span.
func TestProjectRow_NoCompactChipWhenNoOperator(t *testing.T) {
	b := &beadsapi.BeadDetail{
		ID:     "gd-y",
		Title:  "No operator",
		Type:   "task",
		Status: "open",
	}
	v := projectViewFromBead(b, "viewer@example.com", "gardenio", "/b")
	if v.OperatorChipCompactHTML != "" {
		t.Errorf("expected empty OperatorChipCompactHTML for op-less bead, got %q", v.OperatorChipCompactHTML)
	}
	out := renderProjectRow(t, v)
	if strings.Contains(out, "operator-chip-compact") {
		t.Errorf("row should not emit compact chip when operator empty: %s", out)
	}
}

// TestProjectRow_AgentFallbackRendersCompactChip pins that the agent
// fallback path (operated_by empty, CreatedBy set) still surfaces a
// compact chip — the operator column is always populated when there is
// real attribution signal (ka-0000003067).
func TestProjectRow_AgentFallbackRendersCompactChip(t *testing.T) {
	b := &beadsapi.BeadDetail{
		ID:        "gd-z",
		Title:     "Agent-created",
		Type:      "task",
		Status:    "open",
		CreatedBy: "gardenio-gardener-delivery",
	}
	v := projectViewFromBead(b, "viewer@example.com", "gardenio", "/b")
	if v.OperatorChipCompactHTML == "" {
		t.Fatal("expected compact chip to be populated for agent-created bead")
	}
	out := renderProjectRow(t, v)
	if !strings.Contains(out, "operator-chip-compact is-agent") {
		t.Errorf("expected is-agent class for agent-created row: %s", out)
	}
	if !strings.Contains(out, ">GGD<") {
		t.Errorf("expected GGD initials for gardenio-gardener-delivery: %s", out)
	}
}

// TestProjectViewFromBead_IsViewerSelfRequiresEmailMatch pins that
// is-viewer-self only fires when the bead's operator resolves to the
// current viewer's email. Mirrors the resolver-level test but runs
// through the project row pipeline so callers can't accidentally drop
// the viewer-email plumb.
func TestProjectViewFromBead_IsViewerSelfRequiresEmailMatch(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:                "gd-self",
		Title:             "Mine",
		Type:              "task",
		Status:            "open",
		OperatedBy:        "oidc:matt@example.com",
		OperatedByDisplay: "Matt Baker",
		OperatedByEmail:   "matt@example.com",
	}

	mine := projectViewFromBead(bead, "matt@example.com", "gardenio", "/b")
	if !strings.Contains(string(mine.OperatorChipCompactHTML), "is-viewer-self") {
		t.Errorf("viewer email matches op email: expected is-viewer-self, got %q", mine.OperatorChipCompactHTML)
	}

	other := projectViewFromBead(bead, "alice@example.com", "gardenio", "/b")
	if strings.Contains(string(other.OperatorChipCompactHTML), "is-viewer-self") {
		t.Errorf("viewer email differs: should NOT carry is-viewer-self, got %q", other.OperatorChipCompactHTML)
	}

	anon := projectViewFromBead(bead, "", "gardenio", "/b")
	if strings.Contains(string(anon.OperatorChipCompactHTML), "is-viewer-self") {
		t.Errorf("anonymous viewer: should NOT carry is-viewer-self, got %q", anon.OperatorChipCompactHTML)
	}
}

// TestHandleActive_RowsCarryCompactOperatorChip pins that the /active
// page renders the compact chip in the project_row partial when a bead
// has operated_by metadata.
func TestHandleActive_RowsCarryCompactOperatorChip(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:                "gd-a1",
				Title:             "Active alpha",
				Type:              "task",
				Status:            "open",
				Labels:            []string{"project:gardenio"},
				OperatedBy:        "slack:U0OPERATOR1",
				OperatedByDisplay: "Matt Baker",
				OperatedByEmail:   "matt@example.com",
				UpdatedAt:         now,
			},
		},
		Total: 1,
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/active", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, body)
	}
	if !strings.Contains(body, "operator-chip-compact") {
		t.Errorf("/active body missing .operator-chip-compact: %s", body)
	}
	if !strings.Contains(body, ">MB<") {
		t.Errorf("/active body missing MB initials: %s", body)
	}
	// Pin: raw slack:U… must never appear in the rendered row body.
	// (The data-operated-by attribute carries the raw id; we look for
	// the visible-text leak shape ">slack:".)
	if strings.Contains(body, ">slack:") {
		t.Errorf("raw slack: leaked into /active row body: %s", body)
	}
}

// TestHandleProject_RowsCarryCompactOperatorChip pins the same for
// /p/<project> — the per-project list view shares the project_row
// partial but is rendered through handleProject.
func TestHandleProject_RowsCarryCompactOperatorChip(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-p1",
				Title:     "Project alpha",
				Type:      "task",
				Status:    "open",
				Labels:    []string{"project:gardenio"},
				CreatedBy: "gardenio-gardener-delivery",
				UpdatedAt: now,
			},
		},
		Total: 1,
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/p/gardenio?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, body)
	}
	if !strings.Contains(body, "operator-chip-compact is-agent") {
		t.Errorf("/p/gardenio body missing agent compact chip: %s", body)
	}
	if !strings.Contains(body, ">GGD<") {
		t.Errorf("/p/gardenio body missing GGD initials: %s", body)
	}
}

// TestHandleSearch_RendersCompactOperatorChip pins that search.html now
// renders the compact chip variant in its results table (replacing the
// full operator chip column).
func TestHandleSearch_RendersCompactOperatorChip(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.semanticResp = &beadsapi.SemanticSearchResponse{
		Model: "nomic-embed-text",
		Results: []*beadsapi.SemanticSearchResult{
			{Bead: &beadsapi.BeadDetail{
				ID:                "gd-s1",
				Title:             "Result one",
				Type:              "task",
				Status:            "open",
				OperatedBy:        "oidc:matt@example.com",
				OperatedByDisplay: "Matt Baker",
				OperatedByEmail:   "matt@example.com",
				UpdatedAt:         now,
			}, Score: 0.9},
			{Bead: &beadsapi.BeadDetail{
				ID:        "gd-s2",
				Title:     "Result two",
				Type:      "task",
				Status:    "open",
				CreatedBy: "gardenio-gardener-delivery",
				UpdatedAt: now.Add(-1 * time.Hour),
			}, Score: 0.8},
		},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=anything", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, body)
	}
	if !strings.Contains(body, `class="search-results-operator"`) {
		t.Errorf("expected search-results-operator td class, got body=%s", body)
	}
	if !strings.Contains(body, "operator-chip-compact") {
		t.Errorf("search body missing .operator-chip-compact: %s", body)
	}
	if !strings.Contains(body, ">MB<") {
		t.Errorf("search body missing MB initials for matt row: %s", body)
	}
	if !strings.Contains(body, "operator-chip-compact is-agent") {
		t.Errorf("search body missing agent compact chip for createdBy row: %s", body)
	}
	if !strings.Contains(body, ">GGD<") {
		t.Errorf("search body missing GGD initials for createdBy row: %s", body)
	}
	// Pin: raw slack:U… never leaks into a row body in search.
	if strings.Contains(body, ">slack:") {
		t.Errorf("raw slack: leaked into search body: %s", body)
	}
}

// --- ka-0000000013: sub-task 5 — compact chip on the remaining dense list
// surfaces (active_row, view_show, decisions, escalations). Each test below
// pins that the surface renders .operator-chip-compact with the right
// initials + skin and that raw slack:U… never leaks into a visible row body.

// TestActiveRowPartial_RendersCompactOperatorChip pins that the active_row
// partial (the htmx OOB row-swap rendered by bead action endpoints with
// ?return=row, and the per-row table on saved views) carries the compact
// chip. The bead's operator resolves to the viewer's own email → MB initials
// + is-viewer-self royal-purple treatment.
func TestActiveRowPartial_RendersCompactOperatorChip(t *testing.T) {
	f := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-act1": {
				ID:                "gd-act1",
				Title:             "Action target",
				Type:              "task",
				Status:            "open",
				Priority:          2,
				Labels:            []string{"project:gardenio"},
				OperatedBy:        "oidc:matt@example.com",
				OperatedByDisplay: "Matt Baker",
				OperatedByEmail:   "matt@example.com",
			},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/b/gd-act1/claim?return=row", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="bead-row-gd-act1"`) {
		t.Fatalf("expected active_row partial output, body: %s", body)
	}
	if !strings.Contains(body, "operator-chip-compact") {
		t.Errorf("active_row missing .operator-chip-compact: %s", body)
	}
	// Preserve the is-viewer-self royal-purple treatment: a row whose operator
	// resolves to the viewer's own email carries the marker class.
	if !strings.Contains(body, "is-viewer-self") {
		t.Errorf("active_row: viewer-owned row should carry is-viewer-self: %s", body)
	}
	if !strings.Contains(body, ">MB<") {
		t.Errorf("active_row missing MB initials: %s", body)
	}
	// Pin: raw oidc:/slack: identifiers must never leak into the visible body.
	if strings.Contains(body, ">oidc:") || strings.Contains(body, ">slack:") {
		t.Errorf("raw operator id leaked into active_row body: %s", body)
	}
}

// TestHandleViewShow_RendersCompactOperatorChip pins that the saved-view show
// page (view_show.html) renders the compact chip in its results table. The
// row bead is agent-created (operated_by empty, CreatedBy set) → GGD initials
// + is-agent.
func TestHandleViewShow_RendersCompactOperatorChip(t *testing.T) {
	f := newFake()
	f.beads["gd-v1"] = &beadsapi.BeadDetail{
		ID:        "gd-v1",
		Title:     "My active gardenio",
		Type:      "view",
		Status:    "open",
		CreatedBy: "alice@example.com",
		Labels:    []string{"view:owner:alice@example.com"},
		Fields: map[string]string{
			"source":   "active",
			"project":  "gardenio",
			"statuses": "in_progress",
		},
	}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-task1",
				Title:     "Agent-created work",
				Type:      "task",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				CreatedBy: "gardenio-gardener-delivery",
				UpdatedAt: time.Now().Add(-1 * time.Hour),
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/views/gd-v1", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "operator-chip-compact is-agent") {
		t.Errorf("view_show missing agent compact chip: %s", body)
	}
	if !strings.Contains(body, ">GGD<") {
		t.Errorf("view_show missing GGD initials: %s", body)
	}
}

// TestHandleDecisions_RendersCompactOperatorChip pins that the /decisions list
// renders the compact chip in its dense rows. The decision bead carries a
// human operator → MB initials, with raw slack:U… suppressed.
func TestHandleDecisions_RendersCompactOperatorChip(t *testing.T) {
	f := newFake()
	f.decisionResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:                "gd-dec1",
				Title:             "Pick a state backend",
				Type:              "decision",
				Status:            "open",
				Labels:            []string{"project:gardenio"},
				CreatedAt:         time.Now().Add(-3 * time.Hour),
				OperatedBy:        "slack:U0OPERATOR1",
				OperatedByDisplay: "Matt Baker",
				OperatedByEmail:   "matt@example.com",
				Fields: map[string]string{
					"prompt":  "Choose a TF state backend.",
					"options": `[{"id":"s3","short":"S3","label":"S3 + DynamoDB lock","artifact_type":"plan"}]`,
				},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/decisions?all=1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "operator-chip-compact") {
		t.Errorf("/decisions body missing .operator-chip-compact: %s", body)
	}
	if !strings.Contains(body, ">MB<") {
		t.Errorf("/decisions body missing MB initials: %s", body)
	}
	// Pin: raw slack:U… never leaks into a decision row body.
	if strings.Contains(body, ">slack:") {
		t.Errorf("raw slack: leaked into /decisions body: %s", body)
	}
}

// TestHandleEscalations_RendersCompactOperatorChip pins that the /escalations
// list renders the compact chip in its dense rows. The escalation's issue
// bead is agent-created → GGD initials + is-agent.
func TestHandleEscalations_RendersCompactOperatorChip(t *testing.T) {
	now := time.Now()
	f := &fakeDaemon{
		escalationsResp: []beadsapi.EscalationDetail{
			{Issue: &beadsapi.BeadDetail{
				ID:        "gd-esc1",
				Title:     "Grant ec2:DeleteVolume",
				Type:      "escalation",
				Status:    "open",
				Labels:    []string{"project:gardenio"},
				CreatedBy: "gardenio-gardener-delivery",
				CreatedAt: now.Add(-2 * time.Hour),
				UpdatedAt: now.Add(-2 * time.Hour),
				Fields: map[string]string{
					"operation_type":   "aws_iam",
					"sensitivity":      "internal",
					"requesting_agent": "agent-7",
				},
			}},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/escalations", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "operator-chip-compact is-agent") {
		t.Errorf("/escalations body missing agent compact chip: %s", body)
	}
	if !strings.Contains(body, ">GGD<") {
		t.Errorf("/escalations body missing GGD initials: %s", body)
	}
}
