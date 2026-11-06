package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/store"
)

func TestHandleBackfillSearchTSV_PassesCursor(t *testing.T) {
	ms := newMockStore()
	ms.backfillSearchTSVFn = func(afterID string, batchSize int, reindex bool) (*store.BackfillSearchTSVStats, error) {
		return &store.BackfillSearchTSVStats{
			BeadsScanned: 7,
			LastID:       "gd-end",
			Done:         true,
			Remaining:    0,
		}, nil
	}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(BackfillSearchTSVRequest{AfterID: "gd-start", BatchSize: 100, Reindex: true})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/backfill-search-tsv", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleBackfillSearchTSV(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := len(ms.backfillSearchTSVCalls); got != 1 {
		t.Fatalf("expected 1 store call, got %d", got)
	}
	if ms.backfillSearchTSVCalls[0].AfterID != "gd-start" {
		t.Errorf("AfterID = %q, want gd-start", ms.backfillSearchTSVCalls[0].AfterID)
	}
	if ms.backfillSearchTSVCalls[0].BatchSize != 100 {
		t.Errorf("BatchSize = %d, want 100", ms.backfillSearchTSVCalls[0].BatchSize)
	}
	if !ms.backfillSearchTSVCalls[0].Reindex {
		t.Errorf("Reindex = false, want true (forwarded from request body)")
	}

	var resp BackfillSearchTSVResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Done || resp.BeadsScanned != 7 || resp.LastID != "gd-end" || resp.Remaining != 0 {
		t.Errorf("response mismatch: %+v", resp)
	}
}

func TestHandleBackfillSearchTSV_StoreErrorReturns500(t *testing.T) {
	ms := newMockStore()
	ms.backfillSearchTSVFn = func(_ string, _ int, _ bool) (*store.BackfillSearchTSVStats, error) {
		return nil, errors.New("boom")
	}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(BackfillSearchTSVRequest{})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/backfill-search-tsv", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleBackfillSearchTSV(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}
