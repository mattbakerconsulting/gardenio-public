package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
)

func TestHandleAuditBlocksCycles(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	// Concrete 2-cycle from the bug report plus an unrelated acyclic edge.
	addBlocks(ms, "ka-0000001355", "ka-0000000696")
	addBlocks(ms, "ka-0000000696", "ka-0000001355")
	addBlocks(ms, "gd-solo", "gd-other")

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit-blocks-cycles", nil)
	rec := httptest.NewRecorder()
	srv.handleAuditBlocksCycles(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp BlocksCycleAudit
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.CycleCount != 1 {
		t.Fatalf("CycleCount = %d; want 1 (resp=%+v)", resp.CycleCount, resp)
	}
	if len(resp.Cycles) != 1 || resp.Cycles[0].Size != 2 {
		t.Fatalf("expected one 2-member cycle, got %+v", resp.Cycles)
	}
	if resp.EdgeCount != 3 {
		t.Errorf("EdgeCount = %d; want 3", resp.EdgeCount)
	}
}

func TestHandleAuditBlocksCycles_Clean(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	addBlocks(ms, "gd-a", "gd-b")

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit-blocks-cycles", nil)
	rec := httptest.NewRecorder()
	srv.handleAuditBlocksCycles(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp BlocksCycleAudit
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.CycleCount != 0 {
		t.Fatalf("CycleCount = %d; want 0", resp.CycleCount)
	}
}
