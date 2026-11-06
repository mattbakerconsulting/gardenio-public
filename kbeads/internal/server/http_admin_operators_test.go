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

func TestHandleBackfillOperators_PassesOptions(t *testing.T) {
	ms := newMockStore()
	ms.backfillOperatorsFn = func(opts store.BackfillOperatorsOptions) (*store.BackfillOperatorsStats, error) {
		return &store.BackfillOperatorsStats{
			DistinctOperatedBy:    9,
			OperatorsUpserted:     7,
			AliasesUpserted:       7,
			BeadsUpdated:          10,
			CommentsUpdated:       20,
			DepsUpdated:           3,
			EventsUpdated:         100,
			MutationsUpdated:      7,
			BeadsEmptySentinel:    2,
			CommentsEmptySentinel: 1,
			DepsEmptySentinel:     0,
			QuarantinedOperatedBy: []string{"weird"},
			VerifierBeads:         0,
			DryRun:                opts.DryRun,
			DurationMs:            42,
		}, nil
	}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(BackfillOperatorsRequest{DryRun: true, BatchSize: 25000})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/backfill-operators", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleBackfillOperators(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := len(ms.backfillOperatorsCalls); got != 1 {
		t.Fatalf("expected 1 store call, got %d", got)
	}
	got := ms.backfillOperatorsCalls[0]
	if !got.DryRun {
		t.Errorf("DryRun = false, want true")
	}
	if got.BatchSize != 25000 {
		t.Errorf("BatchSize = %d, want 25000", got.BatchSize)
	}

	var resp BackfillOperatorsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DistinctOperatedBy != 9 || resp.OperatorsUpserted != 7 || resp.AliasesUpserted != 7 ||
		resp.BeadsUpdated != 10 || resp.CommentsUpdated != 20 || resp.DepsUpdated != 3 ||
		resp.EventsUpdated != 100 || resp.MutationsUpdated != 7 ||
		resp.BeadsEmptySentinel != 2 || resp.CommentsEmptySentinel != 1 || resp.DepsEmptySentinel != 0 ||
		!resp.DryRun || resp.DurationMs != 42 {
		t.Errorf("response mismatch: %+v", resp)
	}
	if len(resp.QuarantinedOperatedBy) != 1 || resp.QuarantinedOperatedBy[0] != "weird" {
		t.Errorf("quarantined mismatch: %+v", resp.QuarantinedOperatedBy)
	}
}

func TestHandleBackfillOperators_StoreErrorReturns500(t *testing.T) {
	ms := newMockStore()
	ms.backfillOperatorsFn = func(_ store.BackfillOperatorsOptions) (*store.BackfillOperatorsStats, error) {
		return nil, errors.New("boom")
	}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(BackfillOperatorsRequest{})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/backfill-operators", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleBackfillOperators(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}
