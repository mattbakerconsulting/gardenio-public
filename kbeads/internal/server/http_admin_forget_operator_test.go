package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/store"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

// postForget builds a request for POST /v1/admin/operators/{id}/forget with the
// path value set (the handler reads r.PathValue("id"), which httptest does not
// populate on its own).
func postForget(t *testing.T, idPath string, body any) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/operators/"+idPath+"/forget", &buf)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", idPath)
	return req
}

func TestHandleForgetOperator_PassesOptionsAndReturnsResult(t *testing.T) {
	ms := newMockStore()
	ms.forgetOperatorFn = func(id int64, opts store.ForgetOperatorOptions) (*store.ForgetOperatorResult, error) {
		return &store.ForgetOperatorResult{
			ID:                 id,
			ForgottenAt:        mustTime(t, "2026-06-16T16:24:46Z"),
			SlugCleared:        true,
			SlugAliasesRetired: 2,
			AuditRows:          7,
		}, nil
	}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	rec := httptest.NewRecorder()
	srv.handleForgetOperator(rec, postForget(t, "42", ForgetOperatorRequest{Reason: "GDPR-123", Confirm: true, Threshold: 5}))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := len(ms.forgetOperatorCalls); got != 1 {
		t.Fatalf("expected 1 store call, got %d", got)
	}
	call := ms.forgetOperatorCalls[0]
	if call.id != 42 {
		t.Errorf("id = %d, want 42", call.id)
	}
	if call.opts.Reason != "GDPR-123" || !call.opts.Confirm || call.opts.AuditGuardThreshold != 5 {
		t.Errorf("opts mismatch: %+v", call.opts)
	}

	var resp ForgetOperatorResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OperatorID != 42 || !resp.SlugCleared || resp.SlugAliasesRetired != 2 || resp.AuditRows != 7 {
		t.Errorf("response mismatch: %+v", resp)
	}
}

// TestHandleForgetOperator_GuardReturns409WithCode guards ka-0000005150: a
// forget refused for audit history is a 409 carrying the row count and threshold.
func TestHandleForgetOperator_GuardReturns409WithCode(t *testing.T) {
	ms := newMockStore()
	ms.forgetOperatorFn = func(id int64, _ store.ForgetOperatorOptions) (*store.ForgetOperatorResult, error) {
		return nil, &store.OperatorForgetGuardError{OperatorID: id, AuditRows: 5000, Threshold: 1000}
	}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	rec := httptest.NewRecorder()
	srv.handleForgetOperator(rec, postForget(t, "42", ForgetOperatorRequest{}))

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error     string `json:"error"`
		Code      string `json:"code"`
		AuditRows int64  `json:"audit_rows"`
		Threshold int    `json:"threshold"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != OperatorForgetGuardErrorCode {
		t.Errorf("code = %q, want %q", body.Code, OperatorForgetGuardErrorCode)
	}
	if body.AuditRows != 5000 || body.Threshold != 1000 {
		t.Errorf("audit_rows/threshold mismatch: %+v", body)
	}
}

func TestHandleForgetOperator_NotFoundReturns404(t *testing.T) {
	ms := newMockStore()
	ms.forgetOperatorFn = func(id int64, _ store.ForgetOperatorOptions) (*store.ForgetOperatorResult, error) {
		return nil, &store.OperatorNotFoundError{OperatorID: id}
	}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	rec := httptest.NewRecorder()
	srv.handleForgetOperator(rec, postForget(t, "99", ForgetOperatorRequest{}))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleForgetOperator_InvalidIDReturns400(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	rec := httptest.NewRecorder()
	srv.handleForgetOperator(rec, postForget(t, "not-a-number", ForgetOperatorRequest{}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := len(ms.forgetOperatorCalls); got != 0 {
		t.Errorf("store should not be called on bad id, got %d calls", got)
	}
}

func TestHandleForgetOperator_EmptyBodyOK(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	// nil body → no JSON payload at all; the handler must still forget with
	// defaults (decodeJSONBodyOptional tolerates an empty body).
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/operators/7/forget", http.NoBody)
	req.SetPathValue("id", "7")
	rec := httptest.NewRecorder()
	srv.handleForgetOperator(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := len(ms.forgetOperatorCalls); got != 1 {
		t.Fatalf("expected 1 store call, got %d", got)
	}
}
