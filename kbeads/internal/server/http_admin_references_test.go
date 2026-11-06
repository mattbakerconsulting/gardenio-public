package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/store"
)

func TestHandleReextractReferences_PassesCursor(t *testing.T) {
	ms := newMockStore()
	ms.reextractFn = func(afterID string, batchSize int) (*store.ReextractReferencesStats, error) {
		return &store.ReextractReferencesStats{
			BeadsScanned:      3,
			CommentsScanned:   1,
			ReferencesWritten: 4,
			LastID:            "gd-end",
			Done:              true,
			TotalReferences:   42,
		}, nil
	}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(ReextractReferencesRequest{AfterID: "gd-start", BatchSize: 100})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/reextract-references", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleReextractReferences(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := len(ms.reextractCalls); got != 1 {
		t.Fatalf("expected 1 store call, got %d", got)
	}
	if ms.reextractCalls[0].AfterID != "gd-start" {
		t.Errorf("AfterID = %q, want gd-start", ms.reextractCalls[0].AfterID)
	}
	if ms.reextractCalls[0].BatchSize != 100 {
		t.Errorf("BatchSize = %d, want 100", ms.reextractCalls[0].BatchSize)
	}

	var resp ReextractReferencesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Done || resp.BeadsScanned != 3 || resp.CommentsScanned != 1 ||
		resp.ReferencesWritten != 4 || resp.LastID != "gd-end" || resp.TotalReferences != 42 {
		t.Errorf("response mismatch: %+v", resp)
	}
}

// TestHandleReextractReferences_PassesType asserts the handler forwards
// req.Type to the store so a `--type decision` targeted backfill reaches the
// page walk (ka-0000005898). Empty Type (the common full-backfill case) is
// covered by TestHandleReextractReferences_PassesCursor above.
func TestHandleReextractReferences_PassesType(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(ReextractReferencesRequest{BatchSize: 100, Type: "decision"})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/reextract-references", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleReextractReferences(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := len(ms.reextractCalls); got != 1 {
		t.Fatalf("expected 1 store call, got %d", got)
	}
	if ms.reextractCalls[0].Type != "decision" {
		t.Errorf("Type = %q, want decision", ms.reextractCalls[0].Type)
	}
}

func TestHandleReextractReferences_StoreErrorReturns500(t *testing.T) {
	ms := newMockStore()
	ms.reextractFn = func(_ string, _ int) (*store.ReextractReferencesStats, error) {
		return nil, errors.New("boom")
	}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(ReextractReferencesRequest{})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/reextract-references", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleReextractReferences(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
	// A non-deadline error keeps the generic message — the distinct timeout
	// body is reserved for context.DeadlineExceeded.
	if got := decodeErrorBody(t, rec); got == reextractTimeoutMessage {
		t.Errorf("non-deadline error should NOT use the timeout message; got %q", got)
	}
}

// TestHandleReextractReferences_DeadlineSurfacesActionableMessage asserts that
// a page overrunning the server's per-request budget surfaces a distinct,
// actionable body — still a 500 so the CLI auto-halving retry engages — rather
// than the opaque generic message that discarded the deadline cause
// (ka-0000001700).
func TestHandleReextractReferences_DeadlineSurfacesActionableMessage(t *testing.T) {
	ms := newMockStore()
	ms.reextractFn = func(_ string, _ int) (*store.ReextractReferencesStats, error) {
		// Mirror the real store's wrapping of a per-bead deadline hit.
		return nil, fmt.Errorf("reextract gd-abc: re-read bead text: %w", context.DeadlineExceeded)
	}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(ReextractReferencesRequest{BatchSize: 500})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/reextract-references", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleReextractReferences(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 (so the CLI retry engages), got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := decodeErrorBody(t, rec); got != reextractTimeoutMessage {
		t.Errorf("deadline error body = %q, want %q", got, reextractTimeoutMessage)
	}
}

func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return strings.TrimSpace(body.Error)
}
