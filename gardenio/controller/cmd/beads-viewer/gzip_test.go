package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/gzipmw"
)

// TestGzipThroughStatusRecorder: inside the viewer, viewTelemetry's
// statusRecorder wraps the writer the middleware hands down; SSE routes
// type-assert http.Flusher on that and must still reach ours.
func TestGzipThroughStatusRecorder(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	gzipmw.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sr := &statusRecorder{ResponseWriter: w}
		sr.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(sr, "data: x\n\n")
		sr.Flush()
	})).ServeHTTP(rec, req)
	if !rec.Flushed {
		t.Error("Flush through statusRecorder did not reach the connection")
	}
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != "data: x\n\n" {
		t.Errorf("SSE through statusRecorder: Content-Encoding %q body %q", rec.Header().Get("Content-Encoding"), rec.Body.String())
	}
}
