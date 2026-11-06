package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func sampleViewerMoleculeUsage() *beadsapi.MoleculeUsage {
	return &beadsapi.MoleculeUsage{
		MoleculeID: "gd-Abc123",
		SessionUsage: beadsapi.SessionUsage{
			InputTokens:  1_000_000,
			OutputTokens: 200_000,
			TotalCostUSD: 21.70,
		},
		WorkflowSpend: beadsapi.WorkflowSpend{
			AgentsSpawned: 47,
			InputTokens:   3_100_000,
			TotalCostUSD:  62.40,
		},
		TotalCostUSD:     84.10,
		BeadCount:        6,
		SessionCount:     11,
		WorkflowRunCount: 1,
		Steps: []beadsapi.MoleculeStepUsage{
			{
				BeadID:       "gd-s1",
				Title:        "scan",
				SessionUsage: beadsapi.SessionUsage{TotalCostUSD: 3.20, InputTokens: 100_000},
				TotalCostUSD: 3.20,
			},
			{
				BeadID:       "gd-s2",
				Title:        "fan-out",
				SessionUsage: beadsapi.SessionUsage{TotalCostUSD: 0.80},
				WorkflowSpend: &beadsapi.WorkflowSpend{
					AgentsSpawned: 47,
					TotalCostUSD:  62.40,
					Final:         true,
				},
				TotalCostUSD: 63.20,
			},
		},
	}
}

func TestHandleMoleculeCost_HTML(t *testing.T) {
	f := newFake()
	f.moleculeUsageResp = sampleViewerMoleculeUsage()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/molecule/gd-Abc123/cost", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if f.moleculeUsageLastID != "gd-Abc123" {
		t.Errorf("rollup id = %q, want gd-Abc123", f.moleculeUsageLastID)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Molecule cost",
		"$84.10", // summed total
		"$21.70", // host-session split
		"$62.40", // workflow_spend split
		"scan",   // step title
		"gd-s2",  // workflow step id
		"47 agents",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("html body missing %q", want)
		}
	}
	// A final molecule must NOT surface the partial banner.
	if strings.Contains(body, "Partial —") {
		t.Errorf("final molecule should not show partial banner")
	}
}

func TestHandleMoleculeCost_PartialBanner(t *testing.T) {
	f := newFake()
	u := sampleViewerMoleculeUsage()
	u.Partial = true
	u.Steps[1].WorkflowSpend.Final = false
	f.moleculeUsageResp = u
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/molecule/gd-Abc123/cost", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Partial —") {
		t.Errorf("expected partial banner in body")
	}
	if !strings.Contains(body, "non-final") {
		t.Errorf("expected per-step non-final note in body")
	}
}

func TestHandleMoleculeCost_JSON(t *testing.T) {
	f := newFake()
	f.moleculeUsageResp = sampleViewerMoleculeUsage()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/molecule/gd-Abc123/cost?format=json", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var got beadsapi.MoleculeUsage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal json: %v\n%s", err, w.Body.String())
	}
	if got.TotalCostUSD != 84.10 {
		t.Errorf("total_cost_usd = %v, want 84.10", got.TotalCostUSD)
	}
	// The endpoint returns the SAME rolled-up data as the CLI: total == sum.
	if got.TotalCostUSD != got.SessionUsage.TotalCostUSD+got.WorkflowSpend.TotalCostUSD {
		t.Errorf("total is not session+workflow sum")
	}
	if got.WorkflowSpend.AgentsSpawned != 47 {
		t.Errorf("workflow agents = %d, want 47", got.WorkflowSpend.AgentsSpawned)
	}
}

func TestHandleMoleculeCost_JSONViaAccept(t *testing.T) {
	f := newFake()
	f.moleculeUsageResp = sampleViewerMoleculeUsage()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/molecule/gd-Abc123/cost", nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json (Accept negotiation)", ct)
	}
}

func TestHandleMoleculeCost_RollupError(t *testing.T) {
	f := newFake()
	f.moleculeUsageErr = errors.New("daemon down")
	mux := mkServer(t, f)

	// HTML path degrades to an error banner, still 200.
	req := httptest.NewRequest("GET", "/molecule/gd-Abc123/cost", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("html error path: expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to roll up") {
		t.Errorf("expected error banner in html body")
	}

	// JSON path returns a non-2xx so programmatic callers see the failure.
	reqJSON := httptest.NewRequest("GET", "/molecule/gd-Abc123/cost?format=json", nil)
	wJSON := httptest.NewRecorder()
	mux.ServeHTTP(wJSON, reqJSON)
	if wJSON.Code == http.StatusOK {
		t.Errorf("json error path: expected non-200, got %d", wJSON.Code)
	}
}
