package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

func TestHandleFindSimilar_HappyPath(t *testing.T) {
	_, ms, h := newTestServer()

	ms.beads["gd-SRC"] = &model.Bead{ID: "gd-SRC", Title: "Source", Status: model.StatusOpen}
	ms.beads["gd-N1"] = &model.Bead{ID: "gd-N1", Title: "N1", Status: model.StatusOpen}
	ms.beads["gd-N2"] = &model.Bead{ID: "gd-N2", Title: "N2", Status: model.StatusOpen}

	ms.embeddings = map[string]mockEmbedding{
		"gd-SRC": {vec: []float32{1, 0, 0}, model: "test-model"},
	}
	// SemanticSearch returns source first (score=1), then neighbors.
	ms.semanticResults = []*model.SemanticResult{
		{Bead: ms.beads["gd-SRC"], Score: 1.0},
		{Bead: ms.beads["gd-N1"], Score: 0.92},
		{Bead: ms.beads["gd-N2"], Score: 0.81},
	}

	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-SRC/similar?limit=10", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp FindSimilarResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SourceID != "gd-SRC" {
		t.Errorf("SourceID: got %q", resp.SourceID)
	}
	if resp.Model != "test-model" {
		t.Errorf("Model: got %q", resp.Model)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results len: got %d, want 2 (source excluded)", len(resp.Results))
	}
	if resp.Results[0].Bead.ID != "gd-N1" || resp.Results[1].Bead.ID != "gd-N2" {
		t.Errorf("unexpected result order: %+v", resp.Results)
	}
}

func TestHandleFindSimilar_MinScoreFilter(t *testing.T) {
	_, ms, h := newTestServer()

	ms.beads["gd-SRC"] = &model.Bead{ID: "gd-SRC", Status: model.StatusOpen}
	ms.beads["gd-N1"] = &model.Bead{ID: "gd-N1", Status: model.StatusOpen}
	ms.beads["gd-N2"] = &model.Bead{ID: "gd-N2", Status: model.StatusOpen}
	ms.embeddings = map[string]mockEmbedding{"gd-SRC": {vec: []float32{1}, model: "m"}}
	ms.semanticResults = []*model.SemanticResult{
		{Bead: ms.beads["gd-N1"], Score: 0.90},
		{Bead: ms.beads["gd-N2"], Score: 0.40},
	}

	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-SRC/similar?min_score=0.75", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp FindSimilarResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Results) != 1 {
		t.Fatalf("filtered results: got %d, want 1", len(resp.Results))
	}
	if resp.Results[0].Bead.ID != "gd-N1" {
		t.Errorf("kept wrong result: %q", resp.Results[0].Bead.ID)
	}
}

func TestHandleFindSimilar_NoEmbedding_409(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-SRC"] = &model.Bead{ID: "gd-SRC", Status: model.StatusOpen}
	// no embeddings -> GetEmbedding returns sql.ErrNoRows

	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-SRC/similar", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleFindSimilar_BeadNotFound_404(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-MISSING/similar", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestHandleFindSimilar_InvalidLimit_400(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-SRC"] = &model.Bead{ID: "gd-SRC", Status: model.StatusOpen}

	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-SRC/similar?limit=abc", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}

	rec2 := doJSON(t, h, http.MethodGet, "/v1/beads/gd-SRC/similar?limit=0", nil)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 status: got %d, want 400", rec2.Code)
	}
}

func TestHandleFindSimilar_LimitClamp(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-SRC"] = &model.Bead{ID: "gd-SRC", Status: model.StatusOpen}
	ms.embeddings = map[string]mockEmbedding{"gd-SRC": {vec: []float32{1}, model: "m"}}
	ms.semanticResults = []*model.SemanticResult{}

	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-SRC/similar?limit=9999", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleFindSimilar_SemanticSearchError_500(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-SRC"] = &model.Bead{ID: "gd-SRC", Status: model.StatusOpen}
	ms.embeddings = map[string]mockEmbedding{"gd-SRC": {vec: []float32{1}, model: "m"}}
	ms.semanticErr = errors.New("search blew up")

	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-SRC/similar", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

// ensure sql.ErrNoRows from GetEmbedding is treated as 409, not 500.
func TestHandleFindSimilar_ErrNoRowsEmbedding(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-SRC"] = &model.Bead{ID: "gd-SRC", Status: model.StatusOpen}
	ms.embeddingErr = sql.ErrNoRows

	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-SRC/similar", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}
