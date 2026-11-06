package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/embedding"
	"gardenio/kbeads/internal/model"
)

func TestHandleMigrateEmbeddings_Success(t *testing.T) {
	srv, ms := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))
	srv.SetEmbedder(&fakeEmbedder{name: "mxbai-embed-large", dims: 1024})

	body, _ := json.Marshal(MigrateEmbeddingsRequest{
		Model: "mxbai-embed-large",
		Dims:  1024,
		Actor: "alice",
		Note:  "model swap",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings/migrate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-tok")
	req = asOIDCAdmin(req)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleMigrateEmbeddings(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if ms.lastMigrate == nil {
		t.Fatal("expected MigrateEmbeddingsSchema to be called")
	}
	if ms.lastMigrate.Model != "mxbai-embed-large" || ms.lastMigrate.Dims != 1024 {
		t.Errorf("unexpected migrate args: %+v", ms.lastMigrate)
	}
	if ms.lastMigrate.Actor != "alice" || ms.lastMigrate.Note != "model swap" {
		t.Errorf("actor/note not propagated: %+v", ms.lastMigrate)
	}
	if !ms.indexDropped {
		t.Error("expected indexDropped=true after migrate")
	}
}

func TestHandleMigrateEmbeddings_RequiresAdmin(t *testing.T) {
	srv, ms := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))

	body, _ := json.Marshal(MigrateEmbeddingsRequest{Model: "m", Dims: 1024})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings/migrate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer fleet-tok")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleMigrateEmbeddings(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if ms.lastMigrate != nil {
		t.Error("store should not have been called when unauthorized")
	}
}

func TestHandleMigrateEmbeddings_MissingModel(t *testing.T) {
	srv, _ := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))

	body, _ := json.Marshal(MigrateEmbeddingsRequest{Dims: 1024})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings/migrate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-tok")
	req = asOIDCAdmin(req)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleMigrateEmbeddings(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model") {
		t.Errorf("expected model error, got: %s", rec.Body.String())
	}
}

func TestHandleMigrateEmbeddings_BadDims(t *testing.T) {
	srv, _ := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))

	for _, dims := range []int{0, -1} {
		body, _ := json.Marshal(MigrateEmbeddingsRequest{Model: "m", Dims: dims})
		req := httptest.NewRequest(http.MethodPost, "/v1/embeddings/migrate", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-tok")
		req = asOIDCAdmin(req)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		srv.handleMigrateEmbeddings(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("dims=%d: expected 400, got %d", dims, rec.Code)
		}
	}
}

func TestHandleMigrateEmbeddings_StoreError(t *testing.T) {
	srv, ms := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))
	srv.SetEmbedder(&fakeEmbedder{name: "m", dims: 1024})
	ms.migrateErr = errors.New("ddl failed")

	body, _ := json.Marshal(MigrateEmbeddingsRequest{Model: "m", Dims: 1024})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings/migrate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-tok")
	req = asOIDCAdmin(req)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleMigrateEmbeddings(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleReindexEmbeddings_Success(t *testing.T) {
	srv, ms := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))
	ms.indexDropped = true

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings/reindex", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	req = asOIDCAdmin(req)
	rec := httptest.NewRecorder()

	srv.handleReindexEmbeddings(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if !ms.indexRecreated {
		t.Error("expected indexRecreated=true")
	}
	if ms.indexDropped {
		t.Error("expected indexDropped=false after reindex")
	}
}

func TestHandleReindexEmbeddings_RequiresAdmin(t *testing.T) {
	srv, ms := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings/reindex", nil)
	req.Header.Set("Authorization", "Bearer fleet-tok")
	rec := httptest.NewRecorder()

	srv.handleReindexEmbeddings(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if ms.indexRecreated {
		t.Error("store should not have been called when unauthorized")
	}
}

func TestHandleReindexEmbeddings_StoreError(t *testing.T) {
	srv, ms := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))
	ms.reindexErr = errors.New("disk full")

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings/reindex", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	req = asOIDCAdmin(req)
	rec := httptest.NewRecorder()

	srv.handleReindexEmbeddings(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func doSyncBatch(t *testing.T, srv *BeadsServer, token string, req SyncBatchEmbeddingsRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/v1/embeddings/sync-batch", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	if token == "admin-tok" {
		r = asOIDCAdmin(r)
	}
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleSyncBatchEmbeddings(rec, r)
	return rec
}

func TestHandleSyncBatch_HappyPath(t *testing.T) {
	srv, ms := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))
	srv.SetEmbedder(&fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}, name: "test-model", dims: 3})
	b1 := &model.Bead{ID: "gd-a", Title: "Task A", Description: "alpha", Status: model.StatusOpen}
	b2 := &model.Bead{ID: "gd-b", Title: "Task B", Description: "beta", Status: model.StatusOpen}
	ms.beads[b1.ID] = b1
	ms.beads[b2.ID] = b2

	rec := doSyncBatch(t, srv, "admin-tok", SyncBatchEmbeddingsRequest{Limit: 50})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp SyncBatchEmbeddingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Embedded != 2 || resp.Skipped != 0 {
		t.Errorf("counts: got embedded=%d skipped=%d, want 2/0", resp.Embedded, resp.Skipped)
	}
	if resp.Model != "test-model" {
		t.Errorf("model: got %q", resp.Model)
	}
	if len(ms.embeddings) != 2 {
		t.Errorf("expected 2 upserted embeddings, got %d", len(ms.embeddings))
	}
}

func TestHandleSyncBatch_SkipsUnchanged(t *testing.T) {
	srv, ms := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))
	srv.SetEmbedder(&fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}, name: "test-model", dims: 3})
	b := &model.Bead{ID: "gd-a", Title: "Task A", Description: "alpha", Status: model.StatusOpen}
	ms.beads[b.ID] = b
	// Pre-populate a matching hash.
	ms.embeddings = map[string]mockEmbedding{
		b.ID: {vec: []float32{0, 0, 0}, model: "old", textHash: embedding.TextHash(embedding.BeadText(b))},
	}

	rec := doSyncBatch(t, srv, "admin-tok", SyncBatchEmbeddingsRequest{Limit: 50})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp SyncBatchEmbeddingsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Skipped != 1 || resp.Embedded != 0 {
		t.Errorf("counts: got embedded=%d skipped=%d, want 0/1", resp.Embedded, resp.Skipped)
	}
}

func TestHandleSyncBatch_ForceIgnoresHash(t *testing.T) {
	srv, ms := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))
	srv.SetEmbedder(&fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}, name: "test-model", dims: 3})
	b := &model.Bead{ID: "gd-a", Title: "Task A", Status: model.StatusOpen}
	ms.beads[b.ID] = b
	ms.embeddings = map[string]mockEmbedding{
		b.ID: {vec: []float32{0, 0, 0}, model: "old", textHash: embedding.TextHash(embedding.BeadText(b))},
	}

	rec := doSyncBatch(t, srv, "admin-tok", SyncBatchEmbeddingsRequest{Limit: 50, Force: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp SyncBatchEmbeddingsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Embedded != 1 || resp.Skipped != 0 {
		t.Errorf("counts: got embedded=%d skipped=%d, want 1/0", resp.Embedded, resp.Skipped)
	}
}

func TestHandleSyncBatch_OnlyMissingSkipsAlreadyEmbedded(t *testing.T) {
	srv, ms := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))
	srv.SetEmbedder(&fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}, name: "test-model", dims: 3})
	// Two beads embedded already, one fresh — only the fresh one should be
	// returned by ListBeadsNeedingEmbedding and ultimately embedded.
	embedded := &model.Bead{ID: "gd-a", Title: "Already", Status: model.StatusOpen}
	embedded2 := &model.Bead{ID: "gd-b", Title: "Already 2", Status: model.StatusOpen}
	fresh := &model.Bead{ID: "gd-c", Title: "Fresh", Status: model.StatusOpen}
	ms.beads[embedded.ID] = embedded
	ms.beads[embedded2.ID] = embedded2
	ms.beads[fresh.ID] = fresh
	ms.embeddings = map[string]mockEmbedding{
		embedded.ID:  {vec: []float32{0, 0, 0}, model: "old", textHash: "stale"},
		embedded2.ID: {vec: []float32{0, 0, 0}, model: "old", textHash: "stale"},
	}

	rec := doSyncBatch(t, srv, "admin-tok", SyncBatchEmbeddingsRequest{Limit: 50, OnlyMissing: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp SyncBatchEmbeddingsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.PageCount != 1 {
		t.Errorf("page_count: got %d, want 1 (only the fresh bead)", resp.PageCount)
	}
	if resp.Embedded != 1 || resp.Skipped != 0 {
		t.Errorf("counts: got embedded=%d skipped=%d, want 1/0", resp.Embedded, resp.Skipped)
	}
	if resp.Total != 1 {
		t.Errorf("total: got %d, want 1 (workload, not global bead count)", resp.Total)
	}
	if _, ok := ms.embeddings[fresh.ID]; !ok {
		t.Error("fresh bead should have been upserted")
	}
}

// Regression for ka-0000000728: when a bead's stored text_hash matches the
// canonical text but the SQL filter returned it (b.updated_at > e.updated_at),
// the handler must refresh the embedding's updated_at so the same bead does
// not reappear on the next page. Without the bump, `kd embed sync
// --only-missing` is a non-progressing tight loop on a populated pod and
// the e2e shard times out.
func TestHandleSyncBatch_OnlyMissingRefreshesSkippedTimestamps(t *testing.T) {
	srv, ms := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))
	srv.SetEmbedder(&fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}, name: "test-model", dims: 3})

	// Two beads: both have an existing embedding with matching text_hash, but
	// the SQL filter flagged them as stale-by-updated_at (staleEmbeddingIDs).
	// Without the timestamp bump, each sync call would pull them again forever.
	stale1 := &model.Bead{ID: "gd-stale1", Title: "Stale 1", Status: model.StatusOpen}
	stale2 := &model.Bead{ID: "gd-stale2", Title: "Stale 2", Status: model.StatusOpen}
	ms.beads[stale1.ID] = stale1
	ms.beads[stale2.ID] = stale2
	ms.embeddings = map[string]mockEmbedding{
		stale1.ID: {vec: []float32{0, 0, 0}, model: "test-model", textHash: embedding.TextHash(embedding.BeadText(stale1))},
		stale2.ID: {vec: []float32{0, 0, 0}, model: "test-model", textHash: embedding.TextHash(embedding.BeadText(stale2))},
	}
	ms.staleEmbeddingIDs = map[string]bool{stale1.ID: true, stale2.ID: true}

	rec := doSyncBatch(t, srv, "admin-tok", SyncBatchEmbeddingsRequest{Limit: 50, OnlyMissing: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp SyncBatchEmbeddingsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Embedded != 0 {
		t.Errorf("embedded: got %d, want 0 (text_hash matches)", resp.Embedded)
	}
	if resp.Skipped != 2 {
		t.Errorf("skipped: got %d, want 2", resp.Skipped)
	}
	if len(ms.refreshedEmbeddingCalls) != 1 {
		t.Fatalf("RefreshEmbeddingTimestamps calls: got %d, want 1", len(ms.refreshedEmbeddingCalls))
	}
	got := ms.refreshedEmbeddingCalls[0]
	if len(got) != 2 {
		t.Fatalf("refreshed IDs: got %v, want 2 entries", got)
	}
	saw := map[string]bool{}
	for _, id := range got {
		saw[id] = true
	}
	if !saw[stale1.ID] || !saw[stale2.ID] {
		t.Errorf("refreshed IDs missing entries: got %v", got)
	}

	// Second call must now find no work (the mock's RefreshEmbeddingTimestamps
	// cleared the staleness flag).
	rec = doSyncBatch(t, srv, "admin-tok", SyncBatchEmbeddingsRequest{Limit: 50, OnlyMissing: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.PageCount != 0 {
		t.Errorf("loop did not terminate: page_count=%d on second call (got=%v)", resp.PageCount, resp)
	}
}

// Force=true must NOT refresh-and-skip — it embeds everything regardless of
// hash match, so the refresh path should never trigger.
func TestHandleSyncBatch_ForceDoesNotRefresh(t *testing.T) {
	srv, ms := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))
	srv.SetEmbedder(&fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}, name: "test-model", dims: 3})
	b := &model.Bead{ID: "gd-x", Title: "Hi", Status: model.StatusOpen}
	ms.beads[b.ID] = b
	ms.embeddings = map[string]mockEmbedding{
		b.ID: {vec: []float32{0, 0, 0}, model: "test-model", textHash: embedding.TextHash(embedding.BeadText(b))},
	}
	rec := doSyncBatch(t, srv, "admin-tok", SyncBatchEmbeddingsRequest{Limit: 50, Force: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if len(ms.refreshedEmbeddingCalls) != 0 {
		t.Errorf("Force path should not refresh timestamps, got %d calls", len(ms.refreshedEmbeddingCalls))
	}
}

func TestHandleSyncBatch_OnlyMissingAndForceMutuallyExclusive(t *testing.T) {
	srv, _ := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))
	srv.SetEmbedder(&fakeEmbedder{name: "m", dims: 3})
	rec := doSyncBatch(t, srv, "admin-tok", SyncBatchEmbeddingsRequest{Limit: 50, OnlyMissing: true, Force: true})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSyncBatch_RequiresAdmin(t *testing.T) {
	srv, _ := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))
	srv.SetEmbedder(&fakeEmbedder{name: "m", dims: 3})
	rec := doSyncBatch(t, srv, "fleet-tok", SyncBatchEmbeddingsRequest{Limit: 50})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestHandleSyncBatch_RejectsNoopBackend(t *testing.T) {
	srv, _ := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))
	srv.SetEmbedder(embedding.NewNoopEmbedder())
	rec := doSyncBatch(t, srv, "admin-tok", SyncBatchEmbeddingsRequest{Limit: 50})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "noop") {
		t.Errorf("expected noop mention, got: %s", rec.Body.String())
	}
}

func TestHandleSyncBatch_ValidatesLimit(t *testing.T) {
	srv, _ := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))
	srv.SetEmbedder(&fakeEmbedder{name: "m", dims: 3})
	for _, tc := range []struct {
		name  string
		limit int
	}{{"zero", 0}, {"negative", -1}, {"over-max", 501}} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doSyncBatch(t, srv, "admin-tok", SyncBatchEmbeddingsRequest{Limit: tc.limit})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleMigrateEmbeddings_RejectsMismatchedEmbedder(t *testing.T) {
	srv, _ := newTestServerWithFleetAuth(t, "admin-tok", NewFleetConfig("default", nil))
	srv.SetEmbedder(&fakeEmbedder{name: "actual-model", dims: 768})

	body, _ := json.Marshal(MigrateEmbeddingsRequest{Model: "wanted-model", Dims: 1024})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings/migrate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-tok")
	req = asOIDCAdmin(req)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleMigrateEmbeddings(rec, req)

	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 4xx/5xx validation reject, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reports model") && !strings.Contains(rec.Body.String(), "dims") {
		t.Errorf("expected model/dims mismatch error, got: %s", rec.Body.String())
	}
}
