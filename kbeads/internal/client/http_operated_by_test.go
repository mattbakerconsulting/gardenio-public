package client

import (
	"context"
	"net/http"
	"testing"
)

// TestHTTPClient_OperatedByHeader covers the X-Operated-By header injection
// in sendOnce: the SetOperatedBy(v) client-wide default is sent; when it is
// unset the header is omitted entirely.
//
// Regression coverage for gd-9VOMY2b6TJ — header arrival metric was ~0% on
// e2e/prod because static-token callers (gb CLI, agent pods) were not seeding
// the operator id. The transport itself was already wired correctly; this test
// pins the wiring so a future refactor cannot silently regress it.
func TestHTTPClient_OperatedByHeader(t *testing.T) {
	tests := []struct {
		name           string
		setDefault     string
		wantHeader     string
		wantHeaderSent bool
	}{
		{
			name:           "no operator set",
			wantHeaderSent: false,
		},
		{
			name:           "client-wide default",
			setDefault:     "slack:U123",
			wantHeader:     "slack:U123",
			wantHeaderSent: true,
		},
		{
			name:           "OIDC-shaped operator id",
			setDefault:     "oidc:user@example.com",
			wantHeader:     "oidc:user@example.com",
			wantHeaderSent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &testHandler{responseBody: `{"status":"ok"}`}
			c, srv := newTestClient(h)
			defer srv.Close()

			if tt.setDefault != "" {
				c.SetOperatedBy(tt.setDefault)
			}

			if _, err := c.Health(context.Background()); err != nil {
				t.Fatalf("Health() error = %v", err)
			}

			got := h.headers.Get("X-Operated-By")
			_, present := h.headers[http.CanonicalHeaderKey("X-Operated-By")]

			if present != tt.wantHeaderSent {
				t.Errorf("X-Operated-By present = %v, want %v (value=%q)", present, tt.wantHeaderSent, got)
			}
			if got != tt.wantHeader {
				t.Errorf("X-Operated-By = %q, want %q", got, tt.wantHeader)
			}
		})
	}
}

// TestHTTPClient_OperatedByHeader_EmptyValuesNotSent verifies that
// SetOperatedBy("") does not produce an X-Operated-By header. Server-side the resolver counts the header as
// header_present=false when the value is empty; suppressing it client-side
// keeps wire payloads clean and makes "absent" unambiguous in metrics.
func TestHTTPClient_OperatedByHeader_EmptyValuesNotSent(t *testing.T) {
	h := &testHandler{responseBody: `{"status":"ok"}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	c.SetOperatedBy("") // explicit empty

	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}

	if got := h.headers.Get("X-Operated-By"); got != "" {
		t.Errorf("X-Operated-By = %q, want absent", got)
	}
}
