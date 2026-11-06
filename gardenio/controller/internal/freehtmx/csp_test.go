package freehtmx

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCSPMiddleware_SetsStrictHeader(t *testing.T) {
	t.Parallel()
	h := CSPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/widgets/foo", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	got := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'none'",
		"script-src 'self'",
		"connect-src 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"object-src 'none'",
		"base-uri 'none'",
		"report-uri /htmx/widget/csp-report",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected CSP header to contain %q, got %q", want, got)
		}
	}
	if strings.Contains(got, "'unsafe-inline'") && !strings.Contains(got, "style-src 'self' 'unsafe-inline'") {
		t.Fatalf("unsafe-inline only permitted on style-src; got %q", got)
	}
	// Critical: script-src must not have 'unsafe-inline' (audit F6).
	scriptIdx := strings.Index(got, "script-src")
	semiIdx := strings.Index(got[scriptIdx:], ";")
	if semiIdx == -1 {
		t.Fatalf("malformed CSP, no terminator after script-src: %q", got)
	}
	scriptDirective := got[scriptIdx : scriptIdx+semiIdx]
	if strings.Contains(scriptDirective, "'unsafe-inline'") || strings.Contains(scriptDirective, "'unsafe-eval'") {
		t.Fatalf("script-src must not have unsafe-inline / unsafe-eval, got %q", scriptDirective)
	}
}

func TestCSPReportHandler_AcceptsValidReport(t *testing.T) {
	t.Parallel()
	captured := &captureSink{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := CSPReportHandler(logger, captured)

	body := `{"csp-report":{"document-uri":"https://example/widget","violated-directive":"script-src","blocked-uri":"inline","source-file":"/foo.js","line-number":42}}`
	req := httptest.NewRequest(http.MethodPost, "/htmx/widget/csp-report", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/csp-report")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	captured.mu.Lock()
	defer captured.mu.Unlock()
	if len(captured.reports) != 1 {
		t.Fatalf("expected 1 captured report, got %d", len(captured.reports))
	}
	if captured.reports[0].Report.ViolatedDirective != "script-src" {
		t.Fatalf("expected violated-directive=script-src, got %q", captured.reports[0].Report.ViolatedDirective)
	}
	if captured.reports[0].Report.LineNumber != 42 {
		t.Fatalf("expected line-number=42, got %d", captured.reports[0].Report.LineNumber)
	}
}

func TestCSPReportHandler_RejectsNonPOST(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := CSPReportHandler(logger, nil)
	req := httptest.NewRequest(http.MethodGet, "/htmx/widget/csp-report", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "POST" {
		t.Fatalf("expected Allow: POST, got %q", got)
	}
}

func TestCSPReportHandler_TolerantOfMalformedJSON(t *testing.T) {
	t.Parallel()
	captured := &captureSink{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := CSPReportHandler(logger, captured)
	req := httptest.NewRequest(http.MethodPost, "/htmx/widget/csp-report", strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/csp-report")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	// Browsers must not back off on retry — return 204 even on malformed.
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 even on malformed body, got %d", w.Code)
	}
	captured.mu.Lock()
	defer captured.mu.Unlock()
	if len(captured.reports) != 0 {
		t.Fatalf("expected no captured reports for malformed body, got %d", len(captured.reports))
	}
}

func TestCSPReportHandler_CapsBodySize(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := CSPReportHandler(logger, nil)
	// 1 MiB of garbage — handler must read at most maxCSPReportBytes and
	// not allocate the whole body.
	huge := bytes.Repeat([]byte("a"), 1<<20)
	req := httptest.NewRequest(http.MethodPost, "/htmx/widget/csp-report", bytes.NewReader(huge))
	req.Header.Set("Content-Type", "application/csp-report")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
}

func TestCSPReportHandler_DefaultSinkLogs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := CSPReportHandler(logger, nil)
	body := `{"csp-report":{"violated-directive":"script-src","blocked-uri":"https://evil.tld/x"}}`
	req := httptest.NewRequest(http.MethodPost, "/htmx/widget/csp-report", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/csp-report")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "csp violation") {
		t.Fatalf("expected default sink to log csp violation, got %q", out)
	}
	if !strings.Contains(out, "evil.tld") {
		t.Fatalf("expected default sink to log blocked_uri, got %q", out)
	}
}

type captureSink struct {
	mu      sync.Mutex
	reports []CSPReport
}

func (c *captureSink) Record(_ *http.Request, r CSPReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reports = append(c.reports, r)
}
