package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/store"
)

// TestHandleAcceptanceMetricsHappyPath exercises GET
// /v1/metrics/acceptance through the auth stack and verifies the JSON
// shape — the gardener bead-query preflight gate parses this payload
// to decide whether the operated_by epic is done.
func TestHandleAcceptanceMetricsHappyPath(t *testing.T) {
	s, ms, h := newTestServer()
	_ = s
	ms.acceptanceMetrics = &store.AcceptanceMetrics{
		OwnerShareMatt:        store.AcceptanceOwnerShare{Total: 100, Matt: 8, Fraction: 0.08},
		AgentOperatorLeaks:    store.AcceptanceOperatorLeaks{Count: 0},
		OperatorAliasCoverage: store.AcceptanceAliasCoverage{Total: 5, Matched: 5, Fraction: 1.0},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/metrics/acceptance", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	var got store.AcceptanceMetrics
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, rec.Body.String())
	}
	if got.OwnerShareMatt.Matt != 8 || got.OwnerShareMatt.Total != 100 {
		t.Errorf("owner-share: want 8/100, got %+v", got.OwnerShareMatt)
	}
	if got.AgentOperatorLeaks.Count != 0 {
		t.Errorf("agent leaks: want 0, got %d", got.AgentOperatorLeaks.Count)
	}
	if got.OperatorAliasCoverage.Matched != 5 || got.OperatorAliasCoverage.Total != 5 {
		t.Errorf("alias coverage: want 5/5, got %+v", got.OperatorAliasCoverage)
	}
}
