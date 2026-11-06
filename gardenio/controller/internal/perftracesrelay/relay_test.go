package perftracesrelay

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestRelay_RunOnce_EndToEnd(t *testing.T) {
	const tracesJSON = `{"data":[{
		"traceID":"abc123",
		"spans":[{"traceID":"abc123","spanID":"def456","operationName":"GET /x",
			"startTime":1700000000000000,"duration":1000,"processID":"p1",
			"tags":[{"key":"http.status_code","type":"int64","value":"200"}]}],
		"processes":{"p1":{"serviceName":"site-api"}}
	}]}`

	var gotServices []string
	gotAuth := make(chan string, 1)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		select {
		case gotAuth <- u + ":" + p:
		default:
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/services"):
			_, _ = w.Write([]byte(`{"data":["site-api"]}`))
		case strings.HasSuffix(r.URL.Path, "/api/traces"):
			gotServices = append(gotServices, r.URL.Query().Get("service"))
			_, _ = w.Write([]byte(tracesJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer source.Close()

	var mu sync.Mutex
	var receivedSpans int
	var destPath string
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		destPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		var req collectortracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			t.Errorf("dest: bad protobuf: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				receivedSpans += len(ss.Spans)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer dest.Close()

	relay := New(Config{
		SourceURL:      source.URL,
		SourceUsername: "u",
		SourcePassword: "p",
		DestURL:        dest.URL,
	}, testLogger())

	n, err := relay.runOnce(context.Background(), time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if n != 1 {
		t.Errorf("forwarded %d spans, want 1", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if receivedSpans != 1 {
		t.Errorf("dest received %d spans, want 1", receivedSpans)
	}
	if destPath != otlpInsertPath {
		t.Errorf("dest path = %q, want %q", destPath, otlpInsertPath)
	}
	if len(gotServices) != 1 || gotServices[0] != "site-api" {
		t.Errorf("queried services = %v", gotServices)
	}
	select {
	case auth := <-gotAuth:
		if auth != "u:p" {
			t.Errorf("source auth = %q, want u:p", auth)
		}
	default:
		t.Error("source never received a basic-auth request")
	}
}

func TestRelay_RunOnce_ExplicitServices(t *testing.T) {
	var queried []string
	var mu sync.Mutex
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/services") {
			t.Error("services API should not be hit when Services is set")
		}
		mu.Lock()
		queried = append(queried, r.URL.Query().Get("service"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer source.Close()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer dest.Close()

	relay := New(Config{
		SourceURL: source.URL,
		DestURL:   dest.URL,
		Services:  []string{"keycloak", "sponsor-api"},
	}, testLogger())

	if _, err := relay.runOnce(context.Background(), time.Now().Add(-time.Hour), time.Now()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(queried) != 2 || queried[0] != "keycloak" || queried[1] != "sponsor-api" {
		t.Errorf("queried services = %v, want [keycloak sponsor-api]", queried)
	}
}

func TestConfig_WithDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	if c.Interval != 30*time.Second {
		t.Errorf("Interval = %v", c.Interval)
	}
	if c.MaxTracesPerService != 200 {
		t.Errorf("MaxTracesPerService = %d", c.MaxTracesPerService)
	}
	if c.InitialLookback != 15*time.Minute {
		t.Errorf("InitialLookback = %v", c.InitialLookback)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
