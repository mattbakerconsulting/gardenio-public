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

func TestHandleRepairReversedParentChildDeps_PassesOptionsAndReturnsStats(t *testing.T) {
	ms := newMockStore()
	ms.repairReversedParentChildDepsFn = func(opts store.RepairReversedParentChildDepsOptions) (*store.RepairReversedParentChildDepsStats, error) {
		return &store.RepairReversedParentChildDepsStats{
			DryRun:                 opts.DryRun,
			ReversedRows:           2,
			EdgesFlipped:           1,
			SkippedCanonicalExists: 1,
			Edges: []store.ReversedParentChildEdge{
				{EpicID: "gd-epic1", ChildID: "gd-childA", ChildType: "task"},
			},
			Batches: 1,
		}, nil
	}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(RepairReversedParentChildDepsRequest{BatchSize: 250, MaxBatches: 3, DryRun: false})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/repair-reversed-parent-child-deps", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleRepairReversedParentChildDeps(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := len(ms.repairReversedParentChildDepsCalls); got != 1 {
		t.Fatalf("expected 1 store call, got %d", got)
	}
	call := ms.repairReversedParentChildDepsCalls[0]
	if call.BatchSize != 250 || call.MaxBatches != 3 || call.DryRun {
		t.Errorf("options passed = %+v, want BatchSize=250 MaxBatches=3 DryRun=false", call)
	}

	var resp RepairReversedParentChildDepsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ReversedRows != 2 || resp.EdgesFlipped != 1 || resp.SkippedCanonicalExists != 1 {
		t.Errorf("response counts mismatch: %+v", resp)
	}
	if len(resp.Edges) != 1 || resp.Edges[0].EpicID != "gd-epic1" {
		t.Errorf("Edges mismatch: %+v", resp.Edges)
	}
}

func TestHandleRepairReversedParentChildDeps_StoreErrorReturns500(t *testing.T) {
	ms := newMockStore()
	ms.repairReversedParentChildDepsFn = func(_ store.RepairReversedParentChildDepsOptions) (*store.RepairReversedParentChildDepsStats, error) {
		return nil, errors.New("boom")
	}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(RepairReversedParentChildDepsRequest{})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/repair-reversed-parent-child-deps", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleRepairReversedParentChildDeps(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}
