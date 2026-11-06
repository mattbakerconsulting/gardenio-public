package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

func TestResponseBytesByCallerAndRoute(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenFull("agent-tok", "agent-gd-a1", "gardenio", nil, nil, time.Time{}, "crew-a", false)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/beads", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 1234)))
	})
	h := AuthMiddleware(fc, nil, loggingMiddleware(nil, requestTimeoutMiddleware(captureRoute(mux))))
	responseBytesTotal.Reset()

	for _, path := range []string{"/v1/beads", "/v1/nope"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer agent-tok")
		req.Header.Set("X-Operated-By", "slack:U0OPERATOR1")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if got := testutil.ToFloat64(responseBytesTotal.WithLabelValues("fleet", "GET /v1/beads")); got != 1234 {
		t.Errorf("fleet GET /v1/beads bytes = %v, want 1234", got)
	}
	if got := testutil.ToFloat64(responseBytesTotal.WithLabelValues("fleet", "unmatched")); got == 0 {
		t.Errorf("unmatched route recorded no bytes (the 404 body)")
	}
}

func TestRequestDurationByCallerAndRoute(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenFull("agent-tok", "agent-gd-a1", "gardenio", nil, nil, time.Time{}, "crew-a", false)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/beads", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("GET /v1/events/stream", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("data: {}\n\n"))
	})
	h := AuthMiddleware(fc, nil, loggingMiddleware(nil, requestTimeoutMiddleware(captureRoute(mux))))
	requestDuration.Reset()

	for _, path := range []string{"/v1/beads", "/v1/beads", "/v1/events/stream"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer agent-tok")
		req.Header.Set("X-Operated-By", "slack:U0OPERATOR1")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if got := testutil.CollectAndCount(requestDuration); got != 1 {
		t.Errorf("duration series = %d, want 1 (the SSE stream must not be observed)", got)
	}
	m := &dto.Metric{}
	if err := requestDuration.WithLabelValues("fleet", "GET /v1/beads").(prometheus.Histogram).Write(m); err != nil {
		t.Fatal(err)
	}
	if got := m.GetHistogram().GetSampleCount(); got != 2 {
		t.Errorf("fleet GET /v1/beads observations = %d, want 2", got)
	}
}

func TestResponseCaller(t *testing.T) {
	for _, c := range []struct {
		id   AuthIdentity
		want string
	}{
		{AuthIdentity{Type: "machine", Subject: "machine:gardenio-trigger-engine"}, "machine:gardenio-trigger-engine"},
		{AuthIdentity{Type: "machine", Subject: "weird"}, "machine"},
		{AuthIdentity{Type: "oidc", Subject: "someone@example.com"}, "oidc"},
		{AuthIdentity{Type: "fleet", Subject: "agent-x"}, "fleet"},
		{AuthIdentity{}, "other"},
	} {
		if got := responseCaller(c.id); got != c.want {
			t.Errorf("responseCaller(%+v) = %q, want %q", c.id, got, c.want)
		}
	}
}
