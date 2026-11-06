package beadsapi

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rollupFixture stands up an httptest server that answers the two endpoints
// RollupMoleculeUsage hits:
//
//   - GET /v1/batch/dependencies?ids=...&direction=reverse → for each id, the
//     reverse-dep rows (children → parent). A row {BeadID: child, DependsOnID:
//     parent, Type: "parent-child"} means `child` is a child of `parent`.
//   - GET /v1/batch/beads?ids=... → each bead's fields (usage + workflow_spend).
//
// children maps a parent id to its reverse-dep rows; fields maps a bead id to
// its stringified field map (as the daemon would return).
func rollupFixture(t *testing.T, children map[string][]Dependency, fields map[string]map[string]any) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/batch/dependencies"):
			ids := strings.Split(r.URL.Query().Get("ids"), ",")
			var results []batchDepResult
			for _, id := range ids {
				results = append(results, batchDepResult{ID: id, Deps: children[id]})
			}
			_ = json.NewEncoder(w).Encode(batchDepsResponse{Results: results})
		case strings.HasPrefix(r.URL.Path, "/v1/batch/beads"):
			ids := strings.Split(r.URL.Query().Get("ids"), ",")
			var results []batchBeadResult
			for _, id := range ids {
				f, ok := fields[id]
				if !ok {
					results = append(results, batchBeadResult{ID: id, Error: "not found"})
					continue
				}
				fjson, _ := json.Marshal(f)
				results = append(results, batchBeadResult{
					ID:   id,
					Bead: &beadJSON{ID: id, Title: id, Fields: fjson},
				})
			}
			_ = json.NewEncoder(w).Encode(batchBeadsResponse{Results: results})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, httpClient: srv.Client()}
}

func pc(child, parent string) Dependency {
	return Dependency{BeadID: child, DependsOnID: parent, Type: "parent-child"}
}

// TestRollupMoleculeUsage_BFS verifies the parent-child descendant walk visits
// the whole subtree (multiple levels), sums per-session usage, and ignores
// non-parent-child edges and beads outside the subtree.
func TestRollupMoleculeUsage_BFS(t *testing.T) {
	// Tree:
	//   gd-mol
	//     ├─ gd-s1               (session usage: $3.20)
	//     └─ gd-s2               (session usage: $0.80)
	//          └─ gd-agent       (spawned agent session usage: $1.10)
	//   gd-other  (NOT under the molecule — must be excluded)
	children := map[string][]Dependency{
		"gd-mol": {
			pc("gd-s1", "gd-mol"),
			pc("gd-s2", "gd-mol"),
			// a "blocks" edge must NOT be followed as a child
			{BeadID: "gd-blocker", DependsOnID: "gd-mol", Type: "blocks"},
		},
		"gd-s1":    nil,
		"gd-s2":    {pc("gd-agent", "gd-s2")},
		"gd-agent": nil,
	}
	fields := map[string]map[string]any{
		"gd-mol":   {}, // molecule bead itself carries no usage
		"gd-s1":    {"input_tokens": 1000, "output_tokens": 200, "total_cost_usd": 3.20, "request_count": 5},
		"gd-s2":    {"input_tokens": 500, "output_tokens": 50, "total_cost_usd": 0.80, "request_count": 2},
		"gd-agent": {"input_tokens": 300, "output_tokens": 40, "total_cost_usd": 1.10, "request_count": 1},
		// gd-blocker would contribute $99 if (wrongly) followed.
		"gd-blocker": {"total_cost_usd": 99.0},
	}
	c := rollupFixture(t, children, fields)

	got, err := c.RollupMoleculeUsage(context.Background(), "gd-mol")
	if err != nil {
		t.Fatalf("RollupMoleculeUsage: %v", err)
	}

	if got.BeadCount != 4 { // mol + s1 + s2 + agent
		t.Errorf("BeadCount = %d, want 4", got.BeadCount)
	}
	if got.SessionCount != 3 { // s1, s2, agent carry usage; mol does not
		t.Errorf("SessionCount = %d, want 3", got.SessionCount)
	}
	if got.WorkflowRunCount != 0 {
		t.Errorf("WorkflowRunCount = %d, want 0", got.WorkflowRunCount)
	}
	wantCost := 3.20 + 0.80 + 1.10
	if !approx(got.TotalCostUSD, wantCost) {
		t.Errorf("TotalCostUSD = %v, want %v (blocker/other must be excluded)", got.TotalCostUSD, wantCost)
	}
	if got.SessionUsage.InputTokens != 1800 {
		t.Errorf("SessionUsage.InputTokens = %d, want 1800", got.SessionUsage.InputTokens)
	}
	if got.SessionUsage.RequestCount != 8 {
		t.Errorf("SessionUsage.RequestCount = %d, want 8", got.SessionUsage.RequestCount)
	}
	if got.Partial {
		t.Errorf("Partial = true, want false (no workflow_spend present)")
	}
}

// TestRollupMoleculeUsage_SumSemantics is the heart of §3.2: a dynamic-workflow
// host step carries BOTH host-session usage (driver pool) and a workflow_spend
// field (subagent pool). They are non-overlapping, so the molecule total must
// be their SUM — not prefer-one.
func TestRollupMoleculeUsage_SumSemantics(t *testing.T) {
	children := map[string][]Dependency{
		"gd-mol": {pc("gd-wf", "gd-mol")},
		"gd-wf":  nil,
	}
	// Host driver session: $0.80. Subagent pool (workflow_spend): $62.40.
	// Correct molecule cost = 0.80 + 62.40 = 63.20.
	fields := map[string]map[string]any{
		"gd-mol": {},
		"gd-wf": {
			"input_tokens":   10000,
			"output_tokens":  2000,
			"total_cost_usd": 0.80,
			"workflow_spend": map[string]any{
				"agents_spawned":    47,
				"input_tokens":      3120000,
				"output_tokens":     410000,
				"cache_read_tokens": 8800000,
				"total_cost_usd":    62.40,
				"run_id":            "wf_bbba80e9",
				"reported_at":       "2026-05-29T18:40:00Z",
				"final":             true,
			},
		},
	}
	c := rollupFixture(t, children, fields)

	got, err := c.RollupMoleculeUsage(context.Background(), "gd-mol")
	if err != nil {
		t.Fatalf("RollupMoleculeUsage: %v", err)
	}

	if got.WorkflowRunCount != 1 {
		t.Errorf("WorkflowRunCount = %d, want 1", got.WorkflowRunCount)
	}
	// SUM, do not prefer-one.
	if !approx(got.TotalCostUSD, 63.20) {
		t.Errorf("TotalCostUSD = %v, want 63.20 (host $0.80 + workflow_spend $62.40)", got.TotalCostUSD)
	}
	if !approx(got.SessionUsage.TotalCostUSD, 0.80) {
		t.Errorf("SessionUsage.TotalCostUSD = %v, want 0.80", got.SessionUsage.TotalCostUSD)
	}
	if !approx(got.WorkflowSpend.TotalCostUSD, 62.40) {
		t.Errorf("WorkflowSpend.TotalCostUSD = %v, want 62.40", got.WorkflowSpend.TotalCostUSD)
	}
	if got.WorkflowSpend.AgentsSpawned != 47 {
		t.Errorf("WorkflowSpend.AgentsSpawned = %d, want 47", got.WorkflowSpend.AgentsSpawned)
	}
	// Token pools are non-overlapping → summed independently per dimension.
	if got.SessionUsage.InputTokens != 10000 {
		t.Errorf("SessionUsage.InputTokens = %d, want 10000", got.SessionUsage.InputTokens)
	}
	if got.WorkflowSpend.InputTokens != 3120000 {
		t.Errorf("WorkflowSpend.InputTokens = %d, want 3120000", got.WorkflowSpend.InputTokens)
	}
	if got.Partial {
		t.Errorf("Partial = true, want false (workflow_spend.final = true)")
	}

	// Per-step breakdown: the single step's TotalCostUSD is the local sum.
	if len(got.Steps) != 2 { // mol + wf
		t.Fatalf("len(Steps) = %d, want 2", len(got.Steps))
	}
	var wfStep *MoleculeStepUsage
	for i := range got.Steps {
		if got.Steps[i].BeadID == "gd-wf" {
			wfStep = &got.Steps[i]
		}
	}
	if wfStep == nil {
		t.Fatal("gd-wf step missing from breakdown")
	}
	if wfStep.WorkflowSpend == nil {
		t.Fatal("gd-wf step missing workflow_spend")
	}
	if !approx(wfStep.TotalCostUSD, 63.20) {
		t.Errorf("step gd-wf TotalCostUSD = %v, want 63.20", wfStep.TotalCostUSD)
	}
}

// TestRollupMoleculeUsage_MultiWorkflowAndPartial covers (a) summing across
// multiple workflow_spend-bearing steps and (b) the Partial flag surfacing when
// any descendant's workflow_spend is non-final (mid-run rollup, §3.4).
func TestRollupMoleculeUsage_MultiWorkflowAndPartial(t *testing.T) {
	children := map[string][]Dependency{
		"gd-mol": {pc("gd-wf1", "gd-mol"), pc("gd-wf2", "gd-mol")},
		"gd-wf1": nil,
		"gd-wf2": nil,
	}
	fields := map[string]map[string]any{
		"gd-mol": {},
		"gd-wf1": {
			"total_cost_usd": 1.00,
			"workflow_spend": map[string]any{
				"agents_spawned": 10, "total_cost_usd": 20.00,
				"run_id": "wf_a", "final": true,
			},
		},
		"gd-wf2": {
			"total_cost_usd": 2.00,
			"workflow_spend": map[string]any{
				"agents_spawned": 5, "total_cost_usd": 8.00,
				"run_id": "wf_b", "final": false, // in-flight
			},
		},
	}
	c := rollupFixture(t, children, fields)

	got, err := c.RollupMoleculeUsage(context.Background(), "gd-mol")
	if err != nil {
		t.Fatalf("RollupMoleculeUsage: %v", err)
	}

	if got.WorkflowRunCount != 2 {
		t.Errorf("WorkflowRunCount = %d, want 2", got.WorkflowRunCount)
	}
	if got.WorkflowSpend.AgentsSpawned != 15 {
		t.Errorf("WorkflowSpend.AgentsSpawned = %d, want 15", got.WorkflowSpend.AgentsSpawned)
	}
	// (1.00 + 2.00) host + (20.00 + 8.00) workflow = 31.00.
	if !approx(got.TotalCostUSD, 31.00) {
		t.Errorf("TotalCostUSD = %v, want 31.00", got.TotalCostUSD)
	}
	if !got.Partial {
		t.Errorf("Partial = false, want true (wf2.final = false)")
	}
}

// TestRollupMoleculeUsage_CycleAndMissing verifies the visited-set cycle guard
// and that beads the daemon omits (deleted/cross-fleet) contribute nothing
// without erroring.
func TestRollupMoleculeUsage_CycleAndMissing(t *testing.T) {
	// gd-mol → gd-a → gd-b → gd-a (cycle); gd-ghost is referenced but absent.
	children := map[string][]Dependency{
		"gd-mol":   {pc("gd-a", "gd-mol"), pc("gd-ghost", "gd-mol")},
		"gd-a":     {pc("gd-b", "gd-a")},
		"gd-b":     {pc("gd-a", "gd-b")}, // back-edge → cycle
		"gd-ghost": nil,
	}
	fields := map[string]map[string]any{
		"gd-mol": {},
		"gd-a":   {"total_cost_usd": 1.50},
		"gd-b":   {"total_cost_usd": 2.50},
		// gd-ghost intentionally absent from fields → daemon "not found".
	}
	c := rollupFixture(t, children, fields)

	got, err := c.RollupMoleculeUsage(context.Background(), "gd-mol")
	if err != nil {
		t.Fatalf("RollupMoleculeUsage: %v", err)
	}
	if !approx(got.TotalCostUSD, 4.00) {
		t.Errorf("TotalCostUSD = %v, want 4.00 (cycle visited once, ghost ignored)", got.TotalCostUSD)
	}
	// mol, a, b, ghost are all distinct ids in the walk order (each visited once).
	if got.BeadCount != 4 {
		t.Errorf("BeadCount = %d, want 4", got.BeadCount)
	}
}

func TestRollupMoleculeUsage_EmptyID(t *testing.T) {
	c := &Client{}
	if _, err := c.RollupMoleculeUsage(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty molecule id")
	}
}

// TestReportWorkflowSpend round-trips a WorkflowSpend write through the PATCH
// endpoint and confirms the field is persisted as a JSON object.
func TestReportWorkflowSpend(t *testing.T) {
	var patched json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// read-modify-write read leg: return an empty bead.
			_ = json.NewEncoder(w).Encode(beadJSON{ID: "gd-wf"})
		case http.MethodPatch:
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			patched = body["fields"]
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	ws := WorkflowSpend{AgentsSpawned: 47, TotalCostUSD: 62.40, RunID: "wf_x", Final: true}
	if err := c.ReportWorkflowSpend(context.Background(), "gd-wf", ws); err != nil {
		t.Fatalf("ReportWorkflowSpend: %v", err)
	}

	var stored map[string]any
	if err := json.Unmarshal(patched, &stored); err != nil {
		t.Fatalf("patched fields not valid JSON: %v", err)
	}
	wsField, ok := stored["workflow_spend"].(map[string]any)
	if !ok {
		t.Fatalf("workflow_spend not stored as object: %T", stored["workflow_spend"])
	}
	if !approx(wsField["total_cost_usd"].(float64), 62.40) {
		t.Errorf("stored total_cost_usd = %v, want 62.40", wsField["total_cost_usd"])
	}
	if wsField["run_id"] != "wf_x" {
		t.Errorf("stored run_id = %v, want wf_x", wsField["run_id"])
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
