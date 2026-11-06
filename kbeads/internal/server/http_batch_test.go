package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

func TestHandleBatchGetBeads(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, nil)

	ms.beads["gd-AAA"] = &model.Bead{
		ID:     "gd-AAA",
		Title:  "First bead",
		Type:   model.TypeTask,
		Status: model.StatusOpen,
	}
	ms.beads["gd-BBB"] = &model.Bead{
		ID:     "gd-BBB",
		Title:  "Second bead",
		Type:   model.TypeBug,
		Status: model.StatusClosed,
	}

	// Cap boundary (ka-0000005142). The /v1/batch/beads cap used to be a bare
	// literal 50 with no test behind it; these pin the constant so a future
	// change to batchGetMaxIDs cannot silently widen or narrow the contract.
	t.Run("AtCapAccepted", func(t *testing.T) {
		ids := make([]string, batchGetMaxIDs)
		for i := range ids {
			ids[i] = "gd-AAA"
		}
		req := httptest.NewRequest(http.MethodGet,
			"/v1/batch/beads?ids="+strings.Join(ids, ","), nil)
		rec := httptest.NewRecorder()
		srv.handleBatchGetBeads(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("exactly %d ids should be accepted, got %d; body: %s",
				batchGetMaxIDs, rec.Code, rec.Body.String())
		}
	})

	t.Run("OverCapRejected", func(t *testing.T) {
		ids := make([]string, batchGetMaxIDs+1)
		for i := range ids {
			ids[i] = "gd-AAA"
		}
		req := httptest.NewRequest(http.MethodGet,
			"/v1/batch/beads?ids="+strings.Join(ids, ","), nil)
		rec := httptest.NewRecorder()
		srv.handleBatchGetBeads(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %d ids, got %d", batchGetMaxIDs+1, rec.Code)
		}
	})

	t.Run("MultipleBeads", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/batch/beads?ids=gd-AAA,gd-BBB", nil)
		rec := httptest.NewRecorder()

		srv.handleBatchGetBeads(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			Results []struct {
				ID    string      `json:"id"`
				Bead  *model.Bead `json:"bead,omitempty"`
				Error string      `json:"error,omitempty"`
			} `json:"results"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(resp.Results) != 2 {
			t.Fatalf("expected 2 results, got %d", len(resp.Results))
		}
		if resp.Results[0].Bead == nil || resp.Results[0].Bead.Title != "First bead" {
			t.Fatalf("unexpected first result: %+v", resp.Results[0])
		}
		if resp.Results[1].Bead == nil || resp.Results[1].Bead.Title != "Second bead" {
			t.Fatalf("unexpected second result: %+v", resp.Results[1])
		}
	})

	t.Run("NotFound", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/batch/beads?ids=gd-AAA,gd-MISSING", nil)
		rec := httptest.NewRecorder()

		srv.handleBatchGetBeads(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}

		var resp struct {
			Results []struct {
				ID    string      `json:"id"`
				Bead  *model.Bead `json:"bead,omitempty"`
				Error string      `json:"error,omitempty"`
			} `json:"results"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&resp)

		if len(resp.Results) != 2 {
			t.Fatalf("expected 2 results, got %d", len(resp.Results))
		}
		if resp.Results[0].Bead == nil {
			t.Fatal("first result should have a bead")
		}
		if resp.Results[1].Error != "not found" {
			t.Fatalf("second result should have error 'not found', got %q", resp.Results[1].Error)
		}
	})

	t.Run("MissingIDs", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/batch/beads", nil)
		rec := httptest.NewRecorder()

		srv.handleBatchGetBeads(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("SingleBead", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/batch/beads?ids=gd-AAA", nil)
		rec := httptest.NewRecorder()

		srv.handleBatchGetBeads(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}

		var resp struct {
			Results []struct {
				ID   string      `json:"id"`
				Bead *model.Bead `json:"bead,omitempty"`
			} `json:"results"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&resp)
		if len(resp.Results) != 1 {
			t.Fatalf("expected 1 result, got %d", len(resp.Results))
		}
	})
}

func TestHandleBatchChildrenByStatus(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, nil)

	// Seed two epics with children of mixed status, plus a third epic with no
	// children. Also seed a non-parent-child dep that must be ignored.
	ms.beads["gd-EPIC1"] = &model.Bead{ID: "gd-EPIC1", Type: model.TypeEpic, Status: model.StatusOpen}
	ms.beads["gd-EPIC2"] = &model.Bead{ID: "gd-EPIC2", Type: model.TypeEpic, Status: model.StatusOpen}
	ms.beads["gd-EPIC3"] = &model.Bead{ID: "gd-EPIC3", Type: model.TypeEpic, Status: model.StatusOpen}

	ms.beads["gd-CH1"] = &model.Bead{ID: "gd-CH1", Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-CH2"] = &model.Bead{ID: "gd-CH2", Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-CH3"] = &model.Bead{ID: "gd-CH3", Type: model.TypeTask, Status: model.StatusInProgress}
	ms.beads["gd-CH4"] = &model.Bead{ID: "gd-CH4", Type: model.TypeTask, Status: model.StatusClosed}
	ms.beads["gd-CH5"] = &model.Bead{ID: "gd-CH5", Type: model.TypeTask, Status: model.StatusOpen}

	ms.deps["gd-CH1"] = []*model.Dependency{{BeadID: "gd-CH1", DependsOnID: "gd-EPIC1", Type: model.DepParentChild}}
	ms.deps["gd-CH2"] = []*model.Dependency{{BeadID: "gd-CH2", DependsOnID: "gd-EPIC1", Type: model.DepParentChild}}
	ms.deps["gd-CH3"] = []*model.Dependency{{BeadID: "gd-CH3", DependsOnID: "gd-EPIC1", Type: model.DepParentChild}}
	ms.deps["gd-CH4"] = []*model.Dependency{{BeadID: "gd-CH4", DependsOnID: "gd-EPIC1", Type: model.DepParentChild}}
	ms.deps["gd-CH5"] = []*model.Dependency{{BeadID: "gd-CH5", DependsOnID: "gd-EPIC2", Type: model.DepParentChild}}
	// blocks-edge to EPIC1 must NOT show up in the histogram.
	ms.deps["gd-CH5"] = append(ms.deps["gd-CH5"],
		&model.Dependency{BeadID: "gd-CH5", DependsOnID: "gd-EPIC1", Type: model.DepBlocks})

	t.Run("HappyPath", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/batch/children-by-status?parents=gd-EPIC1,gd-EPIC2,gd-EPIC3", nil)
		rec := httptest.NewRecorder()

		srv.handleBatchChildrenByStatus(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			Histograms map[string]map[string]int `json:"histograms"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}

		// Stable shape: every requested parent is present.
		for _, id := range []string{"gd-EPIC1", "gd-EPIC2", "gd-EPIC3"} {
			if _, ok := resp.Histograms[id]; !ok {
				t.Errorf("expected key %q in histograms", id)
			}
		}

		e1 := resp.Histograms["gd-EPIC1"]
		if e1["open"] != 2 || e1["in_progress"] != 1 || e1["closed"] != 1 {
			t.Errorf("EPIC1 = %v, want {open:2, in_progress:1, closed:1}", e1)
		}
		e2 := resp.Histograms["gd-EPIC2"]
		if e2["open"] != 1 {
			t.Errorf("EPIC2 = %v, want {open:1}", e2)
		}
		if len(resp.Histograms["gd-EPIC3"]) != 0 {
			t.Errorf("EPIC3 should have empty map, got %v", resp.Histograms["gd-EPIC3"])
		}
	})

	t.Run("MissingParents", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/batch/children-by-status", nil)
		rec := httptest.NewRecorder()
		srv.handleBatchChildrenByStatus(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("EmptyAfterTrim", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/batch/children-by-status?parents=,,", nil)
		rec := httptest.NewRecorder()
		srv.handleBatchChildrenByStatus(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("OverCap", func(t *testing.T) {
		ids := make([]string, childrenStatusHistogramMaxParents+1)
		for i := range ids {
			ids[i] = "gd-X"
		}
		req := httptest.NewRequest(http.MethodGet,
			"/v1/batch/children-by-status?parents="+strings.Join(ids, ","), nil)
		rec := httptest.NewRecorder()
		srv.handleBatchChildrenByStatus(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("UnknownParentsReturnEmpty", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/batch/children-by-status?parents=gd-NOPE", nil)
		rec := httptest.NewRecorder()
		srv.handleBatchChildrenByStatus(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var resp struct {
			Histograms map[string]map[string]int `json:"histograms"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&resp)
		if got, ok := resp.Histograms["gd-NOPE"]; !ok || len(got) != 0 {
			t.Errorf("gd-NOPE should map to empty {}, got %v", got)
		}
	})
}

// TestHandleBatchGetDependencies covers the forward + reverse direction modes,
// partial results (one ID with edges, one without), and request-validation
// errors. Mirrors the shape of the /v1/batch/beads coverage above so future
// callers can lean on a stable response envelope.
func TestHandleBatchGetDependencies(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, nil)

	// Seed two parents, each with two parent-child edges to leaf beads. PARENT3
	// has no edges so it exercises the "present in request, empty in response"
	// path. A leaf->PARENT1 edge is what the reverse direction surfaces.
	ms.beads["gd-PARENT1"] = &model.Bead{ID: "gd-PARENT1", Type: model.TypeEpic, Status: model.StatusOpen}
	ms.beads["gd-PARENT2"] = &model.Bead{ID: "gd-PARENT2", Type: model.TypeEpic, Status: model.StatusOpen}
	ms.beads["gd-PARENT3"] = &model.Bead{ID: "gd-PARENT3", Type: model.TypeEpic, Status: model.StatusOpen}
	ms.beads["gd-LEAF1"] = &model.Bead{ID: "gd-LEAF1", Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-LEAF2"] = &model.Bead{ID: "gd-LEAF2", Type: model.TypeTask, Status: model.StatusOpen}

	// Forward: PARENT1 depends on LEAF1 and LEAF2; PARENT2 depends on LEAF1.
	ms.deps["gd-PARENT1"] = []*model.Dependency{
		{BeadID: "gd-PARENT1", DependsOnID: "gd-LEAF1", Type: model.DepBlocks},
		{BeadID: "gd-PARENT1", DependsOnID: "gd-LEAF2", Type: model.DepParentChild},
	}
	ms.deps["gd-PARENT2"] = []*model.Dependency{
		{BeadID: "gd-PARENT2", DependsOnID: "gd-LEAF1", Type: model.DepBlocks},
	}

	t.Run("ForwardDefault", func(t *testing.T) {
		// Omitting direction must default to forward.
		req := httptest.NewRequest(http.MethodGet,
			"/v1/batch/dependencies?ids=gd-PARENT1,gd-PARENT2,gd-PARENT3", nil)
		rec := httptest.NewRecorder()
		srv.handleBatchGetDependencies(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Results []struct {
				ID   string              `json:"id"`
				Deps []*model.Dependency `json:"deps"`
			} `json:"results"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Results) != 3 {
			t.Fatalf("expected 3 results, got %d", len(resp.Results))
		}
		// Order must follow the request.
		if resp.Results[0].ID != "gd-PARENT1" || len(resp.Results[0].Deps) != 2 {
			t.Errorf("PARENT1: %+v", resp.Results[0])
		}
		if resp.Results[1].ID != "gd-PARENT2" || len(resp.Results[1].Deps) != 1 {
			t.Errorf("PARENT2: %+v", resp.Results[1])
		}
		// Stable shape: empty (non-nil) deps for parents with no edges.
		if resp.Results[2].ID != "gd-PARENT3" || resp.Results[2].Deps == nil || len(resp.Results[2].Deps) != 0 {
			t.Errorf("PARENT3 should map to empty deps array, got %+v", resp.Results[2])
		}
	})

	t.Run("Reverse", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/v1/batch/dependencies?ids=gd-LEAF1,gd-LEAF2&direction=reverse", nil)
		rec := httptest.NewRecorder()
		srv.handleBatchGetDependencies(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Results []struct {
				ID   string              `json:"id"`
				Deps []*model.Dependency `json:"deps"`
			} `json:"results"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&resp)
		if len(resp.Results) != 2 {
			t.Fatalf("expected 2 results, got %d", len(resp.Results))
		}
		// LEAF1 is depended-on by both PARENT1 and PARENT2.
		if resp.Results[0].ID != "gd-LEAF1" || len(resp.Results[0].Deps) != 2 {
			t.Errorf("LEAF1 expected 2 reverse deps, got: %+v", resp.Results[0])
		}
		// LEAF2 is depended-on only by PARENT1.
		if resp.Results[1].ID != "gd-LEAF2" || len(resp.Results[1].Deps) != 1 {
			t.Errorf("LEAF2 expected 1 reverse dep, got: %+v", resp.Results[1])
		}
		// Each reverse-direction row reads "BeadID depends on DependsOnID".
		if got := resp.Results[1].Deps[0]; got.BeadID != "gd-PARENT1" || got.DependsOnID != "gd-LEAF2" {
			t.Errorf("unexpected reverse-dep row: %+v", got)
		}
	})

	t.Run("MissingIDsParam", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/batch/dependencies", nil)
		rec := httptest.NewRecorder()
		srv.handleBatchGetDependencies(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("EmptyAfterTrim", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/batch/dependencies?ids=,,", nil)
		rec := httptest.NewRecorder()
		srv.handleBatchGetDependencies(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("BadDirection", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/v1/batch/dependencies?ids=gd-PARENT1&direction=sideways", nil)
		rec := httptest.NewRecorder()
		srv.handleBatchGetDependencies(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("OverCap", func(t *testing.T) {
		ids := make([]string, batchGetDependenciesMaxIDs+1)
		for i := range ids {
			ids[i] = "gd-X"
		}
		req := httptest.NewRequest(http.MethodGet,
			"/v1/batch/dependencies?ids="+strings.Join(ids, ","), nil)
		rec := httptest.NewRecorder()
		srv.handleBatchGetDependencies(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("UnknownIDsReturnEmpty", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/v1/batch/dependencies?ids=gd-NOPE", nil)
		rec := httptest.NewRecorder()
		srv.handleBatchGetDependencies(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var resp struct {
			Results []struct {
				ID   string              `json:"id"`
				Deps []*model.Dependency `json:"deps"`
			} `json:"results"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&resp)
		if len(resp.Results) != 1 || resp.Results[0].ID != "gd-NOPE" {
			t.Fatalf("expected single empty entry for gd-NOPE, got %+v", resp.Results)
		}
		if resp.Results[0].Deps == nil || len(resp.Results[0].Deps) != 0 {
			t.Errorf("gd-NOPE should map to empty (non-nil) deps array, got %+v", resp.Results[0].Deps)
		}
	})
}
