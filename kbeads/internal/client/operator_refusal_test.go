package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDecodeResponseOperatorRefusal asserts that a 400 carrying the structured
// {"error","code":"operator_refusal","resolver_source":"..."} body is decoded
// into a typed *OperatorRefusalError. This is the contract the kd CLI's
// classifyAndExit handler keys on to emit the EX_USAGE-family sentinel exit
// code 64. See ka-0000006731.
func TestDecodeResponseOperatorRefusal(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantSource string
	}{
		{
			name:       "header_missing",
			body:       `{"error":"operator could not be resolved","code":"operator_refusal","resolver_source":"header_missing"}`,
			wantSource: "header_missing",
		},
		{
			name:       "header_non_human",
			body:       `{"error":"operator could not be resolved","code":"operator_refusal","resolver_source":"header_non_human"}`,
			wantSource: "header_non_human",
		},
		{
			name:       "fallback_agent_only",
			body:       `{"error":"operator could not be resolved","code":"operator_refusal","resolver_source":"fallback_agent_only"}`,
			wantSource: "fallback_agent_only",
		},
		{
			name:       "chain_root_emailless reserved label round-trips",
			body:       `{"error":"operator could not be resolved","code":"operator_refusal","resolver_source":"chain_root_emailless"}`,
			wantSource: "chain_root_emailless",
		},
		{
			name:       "empty resolver source preserved verbatim",
			body:       `{"error":"operator could not be resolved","code":"operator_refusal"}`,
			wantSource: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := decodeResponse(http.StatusBadRequest, []byte(tc.body), nil)
			var refusal *OperatorRefusalError
			if !errors.As(err, &refusal) {
				t.Fatalf("expected *OperatorRefusalError, got %T: %v", err, err)
			}
			if refusal.ResolverSource != tc.wantSource {
				t.Errorf("ResolverSource: got %q want %q", refusal.ResolverSource, tc.wantSource)
			}
			if refusal.StatusCode != http.StatusBadRequest {
				t.Errorf("StatusCode: got %d want 400", refusal.StatusCode)
			}
		})
	}
}

// TestDecodeResponseGenericAPIError ensures that a 400 with a regular {"error"}
// body (no `code` field) is still surfaced as a plain APIError — the refusal
// path must not poison ordinary input-validation errors.
func TestDecodeResponseGenericAPIError(t *testing.T) {
	body := `{"error":"bead_id is required"}`
	err := decodeResponse(http.StatusBadRequest, []byte(body), nil)
	var refusal *OperatorRefusalError
	if errors.As(err, &refusal) {
		t.Fatalf("did not expect OperatorRefusalError, got %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.Message != "bead_id is required" {
		t.Errorf("Message: got %q", apiErr.Message)
	}
}

// TestHTTPClientSurfacesOperatorRefusal exercises the full HTTP client path:
// a daemon-shaped 400 response should bubble back to callers as a typed
// *OperatorRefusalError, not a generic *APIError or wrapped error.
func TestHTTPClientSurfacesOperatorRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"operator could not be resolved","code":"operator_refusal","resolver_source":"header_missing"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "")
	// Drive a write path — CreateBead is the canonical write surface.
	_, err := c.CreateBead(context.Background(), &CreateBeadRequest{
		Title: "title",
		Type:  "task",
	})
	if err == nil {
		t.Fatalf("expected refusal error, got nil")
	}
	var refusal *OperatorRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("expected *OperatorRefusalError, got %T: %v", err, err)
	}
	if refusal.ResolverSource != "header_missing" {
		t.Errorf("ResolverSource: got %q want %q", refusal.ResolverSource, "header_missing")
	}
}
