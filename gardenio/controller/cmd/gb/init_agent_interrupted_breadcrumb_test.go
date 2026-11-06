package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPostThreadInterruptedBreadcrumb_ThreadBound verifies that a thread-bound
// agent posts a single interrupted breadcrumb to /api/slack/threads/reply with the
// thread coordinates — the fix for the silent mid-turn death (ka-0000004614).
func TestPostThreadInterruptedBreadcrumb_ThreadBound(t *testing.T) {
	var calls int
	var body map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/slack/threads/reply" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(slackReplyResult{OK: true, Timestamp: "1.2"})
	}))
	defer server.Close()

	if posted := postThreadInterruptedBreadcrumbTo(server.URL, "C42", "111.222"); !posted {
		t.Errorf("expected postThreadInterruptedBreadcrumbTo to report a successful post")
	}

	if calls != 1 {
		t.Fatalf("expected exactly 1 POST to the bridge, got %d", calls)
	}
	if body["channel"] != "C42" {
		t.Errorf("channel = %q, want %q", body["channel"], "C42")
	}
	if body["thread_ts"] != "111.222" {
		t.Errorf("thread_ts = %q, want %q", body["thread_ts"], "111.222")
	}
	if !strings.Contains(strings.ToLower(body["text"]), "interrupted") {
		t.Errorf("breadcrumb text should mention it was interrupted, got: %q", body["text"])
	}
	if !strings.Contains(strings.ToLower(body["text"]), "resume") {
		t.Errorf("breadcrumb text should tell the user how to resume, got: %q", body["text"])
	}
}

// TestPostThreadInterruptedBreadcrumb_BridgeError verifies the breadcrumb is
// best-effort: a bridge returning 500 must not panic or otherwise disturb the
// caller (the pod is already being torn down; teardown must proceed).
func TestPostThreadInterruptedBreadcrumb_BridgeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	// Must not panic. A non-200 is swallowed and logged, and reported as NOT
	// posted so the caller never stamps the dedup marker on a failed post.
	if posted := postThreadInterruptedBreadcrumbTo(server.URL, "C42", "111.222"); posted {
		t.Errorf("a non-200 bridge response must report no post (marker must not be stamped)")
	}
}
