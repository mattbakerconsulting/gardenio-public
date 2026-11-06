package beadsapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSetSystemOperator_SetsHeader pins that SetSystemOperator stamps the
// X-System-Operator header on every outbound request without forwarding the
// value as X-Operated-By. Regression for ka-0000004577: the controller used
// to call SetOperatedBy("system:gardenio-controller"), which the daemon's
// sanitizeOperatedByHeader rejects as non-human-shaped and which silently
// drops audit RecordEvent/RecordMutation for the parent write.
func TestSetSystemOperator_SetsHeader(t *testing.T) {
	var gotSystem, gotOperator string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSystem = r.Header.Get("X-System-Operator")
		gotOperator = r.Header.Get("X-Operated-By")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	c.SetSystemOperator("gardenio-controller")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/v1/beads", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if so := SystemOperatorFromContext(req.Context()); so != "" {
		t.Fatalf("ctx must not carry a per-call override; got %q", so)
	}

	// Exercise sendOnce-equivalent path through the public API. Use a small
	// GET to a real endpoint registered by the test server above.
	_, _, _, _, _, err = c.sendOnce(context.Background(), http.MethodGet, "/v1/beads", nil, "")
	if err != nil {
		t.Fatalf("sendOnce: %v", err)
	}

	if gotSystem != "gardenio-controller" {
		t.Errorf("X-System-Operator = %q; want %q", gotSystem, "gardenio-controller")
	}
	if gotOperator != "" {
		t.Errorf("X-Operated-By must NOT be set when only the system-operator default is configured; got %q", gotOperator)
	}
}

// TestSetSystemOperator_ContextOverride pins that a per-call WithSystemOperator
// overrides the client-level default.
func TestSetSystemOperator_ContextOverride(t *testing.T) {
	var gotSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSystem = r.Header.Get("X-System-Operator")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	c.SetSystemOperator("gardenio-controller")

	ctx := WithSystemOperator(context.Background(), "scheduler")
	_, _, _, _, _, err := c.sendOnce(ctx, http.MethodGet, "/v1/beads", nil, "")
	if err != nil {
		t.Fatalf("sendOnce: %v", err)
	}
	if gotSystem != "scheduler" {
		t.Errorf("X-System-Operator = %q; want %q (ctx override wins)", gotSystem, "scheduler")
	}
}

// TestSetSystemOperator_OperatorWins pins that a per-call WithOperator
// (X-Operated-By) coexists with the client-level system-operator default —
// both headers are sent so the daemon's resolver prefers the human operator
// but still has the system-operator escape hatch available.
func TestSetSystemOperator_OperatorWins(t *testing.T) {
	var gotSystem, gotOperator string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSystem = r.Header.Get("X-System-Operator")
		gotOperator = r.Header.Get("X-Operated-By")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	c.SetSystemOperator("gardenio-controller")

	ctx := WithOperator(context.Background(), "slack:U0OPERATOR2")
	_, _, _, _, _, err := c.sendOnce(ctx, http.MethodGet, "/v1/beads", nil, "")
	if err != nil {
		t.Fatalf("sendOnce: %v", err)
	}
	if gotOperator != "slack:U0OPERATOR2" {
		t.Errorf("X-Operated-By = %q; want slack:U0OPERATOR2", gotOperator)
	}
	if gotSystem != "gardenio-controller" {
		t.Errorf("X-System-Operator = %q; want gardenio-controller (default still emitted)", gotSystem)
	}
}
