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

func TestHandleBackfillOperatorSlugs_PassesOptions(t *testing.T) {
	ms := newMockStore()
	ms.backfillOperatorSlugsFn = func(opts store.BackfillOperatorSlugsOptions) (*store.BackfillOperatorSlugsStats, error) {
		return &store.BackfillOperatorSlugsStats{
			DryRun:           opts.DryRun,
			BeforeOpaque:     52,
			AfterOpaque:      12,
			Renamed:          40,
			SkippedCollision: 2,
			Machine:          10,
			DurationMs:       77,
		}, nil
	}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(BackfillOperatorSlugsRequest{DryRun: true, BatchSize: 500})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/backfill-operator-slugs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleBackfillOperatorSlugs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := len(ms.backfillOperatorSlugsCalls); got != 1 {
		t.Fatalf("expected 1 store call, got %d", got)
	}
	got := ms.backfillOperatorSlugsCalls[0]
	if !got.DryRun {
		t.Errorf("DryRun = false, want true")
	}
	if got.BatchSize != 500 {
		t.Errorf("BatchSize = %d, want 500", got.BatchSize)
	}

	var resp BackfillOperatorSlugsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.BeforeOpaque != 52 || resp.AfterOpaque != 12 || resp.Renamed != 40 ||
		resp.SkippedCollision != 2 || resp.Machine != 10 || !resp.DryRun || resp.DurationMs != 77 {
		t.Errorf("response mismatch: %+v", resp)
	}
}

func TestHandleBackfillOperatorSlugs_StoreErrorReturns500(t *testing.T) {
	ms := newMockStore()
	ms.backfillOperatorSlugsFn = func(_ store.BackfillOperatorSlugsOptions) (*store.BackfillOperatorSlugsStats, error) {
		return nil, errors.New("boom")
	}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(BackfillOperatorSlugsRequest{})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/backfill-operator-slugs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleBackfillOperatorSlugs(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}
