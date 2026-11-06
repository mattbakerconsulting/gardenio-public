package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// captureLogger returns a logger that writes structured JSON into buf so the
// tests can assert on emitted events without depending on stderr. Source key
// is omitted to keep the JSON shape stable across Go versions.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h)
}

// findEvents pulls out every log line whose `msg` matches eventName. Each
// result is the parsed JSON object. Empty slice when none match.
func findEvents(t *testing.T, buf *bytes.Buffer, eventName string) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if msg, _ := rec["msg"].(string); msg == eventName {
			out = append(out, rec)
		}
	}
	return out
}

func mkServerWithLogger(t *testing.T, daemon beadAPI, logger *slog.Logger) http.Handler {
	t.Helper()
	return mkServerWithLoggerOpts(t, daemon, logger, false)
}

// mkServerWithLoggerOpts is the legacy-flag-aware variant. The /inbox route
// 308s to /u/me?view=dashboard in production after M3 (ka-0000003587); inbox
// telemetry tests that need the renderer itself to run must opt in to the
// legacy /inbox mapping.
func mkServerWithLoggerOpts(t *testing.T, daemon beadAPI, logger *slog.Logger, inboxLegacy bool) http.Handler {
	t.Helper()
	srv := NewServer(daemon, logger, "", "", "", nil)
	srv.inboxLegacyRoute = inboxLegacy
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return edgeAuth(mux)
}

// TestRefererPath covers the bounded-path extraction used to keep the
// Referer field PII-light. Empty input, missing host, query stripping.
func TestRefererPath(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want string
	}{
		{"empty", "", ""},
		{"path-only", "/gd-abc", "/gd-abc"},
		{"with-host", "https://viewer.example.com/u/alice", "/u/alice"},
		{"strips-query", "https://viewer.example.com/search?q=secret", "/search"},
		{"malformed", "://nope", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if tc.ref != "" {
				r.Header.Set("Referer", tc.ref)
			}
			got := refererPath(r)
			if got != tc.want {
				t.Errorf("refererPath(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

// TestTrimEmail just guards the normalization contract — log consumers can
// rely on lower-case, trimmed values without redoing the work themselves.
func TestTrimEmail(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"  Alice@X.COM  ":  "alice@x.com",
		"bob@example.com":  "bob@example.com",
		"\tCarol@Y.com\n":  "carol@y.com",
	}
	for in, want := range cases {
		if got := trimEmail(in); got != want {
			t.Errorf("trimEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHandleUserHome_EmitsTelemetry exercises GET /u/{slug} with a captured
// logger and asserts the emitted record carries the §9 fields: slug,
// viewer_self, latency_ms, referer_path, pinned_count.
func TestHandleUserHome_EmitsTelemetry(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	var buf bytes.Buffer
	mux := mkServerWithLogger(t, f, captureLogger(&buf))

	req := httptest.NewRequest("GET", "/u/alice", nil)
	req.Header.Set("Referer", "https://viewer.example.com/gd-source-bead")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	events := findEvents(t, &buf, telemetryEventUserHomeView)
	if len(events) != 1 {
		t.Fatalf("want exactly 1 %s event, got %d (buf=%s)", telemetryEventUserHomeView, len(events), buf.String())
	}
	ev := events[0]
	if got, _ := ev["slug"].(string); got != "alice" {
		t.Errorf("slug = %v, want alice", ev["slug"])
	}
	// operator_id arrives as float64 via JSON
	if got, _ := ev["operator_id"].(float64); got != 42 {
		t.Errorf("operator_id = %v, want 42", ev["operator_id"])
	}
	if got, _ := ev["viewer_self"].(bool); got != false {
		t.Errorf("viewer_self = %v, want false", ev["viewer_self"])
	}
	if got, _ := ev["referer_path"].(string); got != "/gd-source-bead" {
		t.Errorf("referer_path = %v, want /gd-source-bead", ev["referer_path"])
	}
	if _, ok := ev["latency_ms"]; !ok {
		t.Errorf("latency_ms missing: %v", ev)
	}
	if got, _ := ev["pinned_count"].(float64); got != 0 {
		t.Errorf("pinned_count = %v, want 0", ev["pinned_count"])
	}
}

// TestHandleInbox_EmitsTelemetry exercises GET /inbox and asserts the emitted
// event carries the fields needed for the §9 bounce-rate baseline: scope,
// kind, project, stream_total, mine_empty, viewer_email.
func TestHandleInbox_EmitsTelemetry(t *testing.T) {
	f := newFake()
	var buf bytes.Buffer
	// M3 (ka-0000003587) 308s bare /inbox to /u/me?view=dashboard. The
	// telemetry emit lives in handleInbox itself (also reachable via
	// /u/me?view=constellation), so opt into the legacy /inbox route to
	// exercise the renderer directly.
	mux := mkServerWithLoggerOpts(t, f, captureLogger(&buf), true)

	req := httptest.NewRequest("GET", "/inbox?scope=mine&kind=&project=", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	events := findEvents(t, &buf, telemetryEventInboxView)
	if len(events) != 1 {
		t.Fatalf("want exactly 1 %s event, got %d (buf=%s)", telemetryEventInboxView, len(events), buf.String())
	}
	ev := events[0]
	if got, _ := ev["scope"].(string); got != "mine" {
		t.Errorf("scope = %v, want mine", ev["scope"])
	}
	if _, ok := ev["stream_total"]; !ok {
		t.Errorf("stream_total missing: %v", ev)
	}
	if _, ok := ev["mine_empty"]; !ok {
		t.Errorf("mine_empty missing: %v", ev)
	}
}

// TestHandleSearch_EmitsTelemetry covers the §9.5 chip-fallback signal —
// /search with an operated_by_slug param flags has_operated_by_slug=true so
// the offline aggregator can divide chip-fallback hits by /u/:slug hits.
func TestHandleSearch_EmitsTelemetry(t *testing.T) {
	t.Run("with-operated_by_slug", func(t *testing.T) {
		f := newFake()
		var buf bytes.Buffer
		mux := mkServerWithLogger(t, f, captureLogger(&buf))

		req := httptest.NewRequest("GET", "/search?operated_by_slug=alice", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		events := findEvents(t, &buf, telemetryEventSearchView)
		if len(events) != 1 {
			t.Fatalf("want exactly 1 %s event, got %d", telemetryEventSearchView, len(events))
		}
		ev := events[0]
		if got, _ := ev["has_operated_by_slug"].(bool); got != true {
			t.Errorf("has_operated_by_slug = %v, want true", ev["has_operated_by_slug"])
		}
		if got, _ := ev["operated_by_slug"].(string); got != "alice" {
			t.Errorf("operated_by_slug = %v, want alice", ev["operated_by_slug"])
		}
		if got, _ := ev["has_query"].(bool); got != false {
			t.Errorf("has_query = %v, want false", ev["has_query"])
		}
	})

	t.Run("plain-search", func(t *testing.T) {
		f := newFake()
		var buf bytes.Buffer
		mux := mkServerWithLogger(t, f, captureLogger(&buf))

		req := httptest.NewRequest("GET", "/search", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		events := findEvents(t, &buf, telemetryEventSearchView)
		if len(events) != 1 {
			t.Fatalf("want exactly 1 %s event, got %d", telemetryEventSearchView, len(events))
		}
		ev := events[0]
		if got, _ := ev["has_operated_by_slug"].(bool); got != false {
			t.Errorf("has_operated_by_slug = %v, want false", ev["has_operated_by_slug"])
		}
	})
}
