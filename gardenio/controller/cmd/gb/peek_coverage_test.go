package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// ── peek HTTP handlers ────────────────────────────────────────────────────

func newPeekServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestPeekListSessions_Empty(t *testing.T) {
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	if err := peekListSessions(muxHTTPClient(), srv.URL, ""); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPeekListSessions_Populated(t *testing.T) {
	sessions := []peekSessionInfo{
		{ID: "00000000-0000-0000-0000-000000000001", HealthFailures: 2},
		{ID: "00000000-0000-0000-0000-000000000002"},
	}
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(sessions)
	})
	if err := peekListSessions(muxHTTPClient(), srv.URL, "tok"); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPeekListSessions_BadJSON(t *testing.T) {
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	})
	// Falls back to printing raw body, not an error.
	if err := peekListSessions(muxHTTPClient(), srv.URL, ""); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPeekListSessions_HTTPError(t *testing.T) {
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	})
	if err := peekListSessions(muxHTTPClient(), srv.URL, ""); err == nil {
		t.Error("expected error")
	}
}

func TestPeekListSessions_ConnRefused(t *testing.T) {
	if err := peekListSessions(muxHTTPClient(), "http://127.0.0.1:1", ""); err == nil {
		t.Error("expected connection error")
	}
}

func TestResolveSessionTarget_FullUUID(t *testing.T) {
	// 36-char UUID with 4 dashes short-circuits without an HTTP call.
	uuid := "12345678-1234-1234-1234-123456789abc"
	got, err := resolveSessionTarget(nil, "", "", uuid)
	if err != nil || got != uuid {
		t.Errorf("got %q err=%v", got, err)
	}
}

func TestResolveSessionTarget_NoMatch(t *testing.T) {
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]peekSessionInfo{{ID: "abc"}})
	})
	if _, err := resolveSessionTarget(muxHTTPClient(), srv.URL, "", "zzz"); err == nil {
		t.Error("expected no match error")
	}
}

func TestResolveSessionTarget_PrefixMatch(t *testing.T) {
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]peekSessionInfo{
			{ID: "abc-123"},
			{ID: "xyz-456"},
		})
	})
	got, err := resolveSessionTarget(muxHTTPClient(), srv.URL, "", "abc")
	if err != nil || got != "abc-123" {
		t.Errorf("got %q err=%v", got, err)
	}
}

func TestResolveSessionTarget_Multi(t *testing.T) {
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]peekSessionInfo{
			{ID: "abc-111"}, {ID: "abc-222"},
		})
	})
	if _, err := resolveSessionTarget(muxHTTPClient(), srv.URL, "", "abc"); err == nil {
		t.Error("expected multi-match error")
	}
}

func TestPeekShowScreen_OK(t *testing.T) {
	sessionID := "12345678-1234-1234-1234-123456789abc"
	screen := peekScreenSnapshot{Lines: []string{"line1", "line2"}, Cols: 80, Rows: 24}
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/screen") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(screen)
	})
	if err := peekShowScreen(muxHTTPClient(), srv.URL, "", sessionID); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPeekShowScreen_HTTPError(t *testing.T) {
	sessionID := "12345678-1234-1234-1234-123456789abc"
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	if err := peekShowScreen(muxHTTPClient(), srv.URL, "", sessionID); err == nil {
		t.Error("expected error")
	}
}

func TestPeekShowStatus_OK(t *testing.T) {
	sessionID := "12345678-1234-1234-1234-123456789abc"
	pid := int32(123)
	exit := int32(0)
	status := peekSessionStatus{
		SessionID: sessionID, State: "running", PID: &pid, ExitCode: &exit,
		UptimeSecs: 90, BytesRead: 2000, BytesWritten: 3000, WSClients: 2,
	}
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(status)
	})
	if err := peekShowStatus(muxHTTPClient(), srv.URL, "", sessionID); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPeekShowStatus_HTTPError(t *testing.T) {
	sessionID := "12345678-1234-1234-1234-123456789abc"
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if err := peekShowStatus(muxHTTPClient(), srv.URL, "", sessionID); err == nil {
		t.Error("expected error")
	}
}

func TestPeekShowOutput_OK(t *testing.T) {
	sessionID := "12345678-1234-1234-1234-123456789abc"
	data := base64.StdEncoding.EncodeToString([]byte("hello world"))
	out := peekOutputResponse{Data: data, Offset: 0, NextOffset: 11, TotalWritten: 11}
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(out)
	})
	// Reset flag state so we exercise the plain path.
	oldTail, oldOffset, oldLimit := peekTail, peekOffset, peekLimit
	peekTail, peekOffset, peekLimit = 0, -1, 0
	t.Cleanup(func() { peekTail, peekOffset, peekLimit = oldTail, oldOffset, oldLimit })

	if err := peekShowOutput(muxHTTPClient(), srv.URL, "", sessionID); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPeekShowOutput_HTTPError(t *testing.T) {
	sessionID := "12345678-1234-1234-1234-123456789abc"
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	oldTail, oldOffset, oldLimit := peekTail, peekOffset, peekLimit
	peekTail, peekOffset, peekLimit = 0, -1, 0
	t.Cleanup(func() { peekTail, peekOffset, peekLimit = oldTail, oldOffset, oldLimit })

	if err := peekShowOutput(muxHTTPClient(), srv.URL, "", sessionID); err == nil {
		t.Error("expected error")
	}
}

func TestPeekShowTranscripts_List(t *testing.T) {
	sessionID := "12345678-1234-1234-1234-123456789abc"
	transcripts := []peekTranscriptMeta{
		{Number: 1, Timestamp: "2026-01-01T00:00:00Z", LineCount: 10, ByteSize: 256},
	}
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(transcripts)
	})
	oldT := peekTranscript
	peekTranscript = ""
	t.Cleanup(func() { peekTranscript = oldT })
	if err := peekShowTranscripts(muxHTTPClient(), srv.URL, "", sessionID); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPeekShowTranscripts_ListEmpty(t *testing.T) {
	sessionID := "12345678-1234-1234-1234-123456789abc"
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	oldT := peekTranscript
	peekTranscript = "list"
	t.Cleanup(func() { peekTranscript = oldT })
	if err := peekShowTranscripts(muxHTTPClient(), srv.URL, "", sessionID); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPeekShowTranscripts_Fetch(t *testing.T) {
	sessionID := "12345678-1234-1234-1234-123456789abc"
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"line":1}`))
	})
	oldT := peekTranscript
	peekTranscript = "5"
	t.Cleanup(func() { peekTranscript = oldT })
	if err := peekShowTranscripts(muxHTTPClient(), srv.URL, "", sessionID); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPeekShowTranscripts_HTTPError(t *testing.T) {
	sessionID := "12345678-1234-1234-1234-123456789abc"
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	oldT := peekTranscript
	peekTranscript = ""
	t.Cleanup(func() { peekTranscript = oldT })
	if err := peekShowTranscripts(muxHTTPClient(), srv.URL, "", sessionID); err == nil {
		t.Error("expected error")
	}
}

func TestPeekShowRecording_OK(t *testing.T) {
	sessionID := "12345678-1234-1234-1234-123456789abc"
	path := "/tmp/rec.log"
	rec := peekRecordingStatus{Enabled: true, Path: &path, Entries: 100}
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(rec)
	})
	if err := peekShowRecording(muxHTTPClient(), srv.URL, "", sessionID); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPeekShowRecording_HTTPError(t *testing.T) {
	sessionID := "12345678-1234-1234-1234-123456789abc"
	srv := newPeekServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if err := peekShowRecording(muxHTTPClient(), srv.URL, "", sessionID); err == nil {
		t.Error("expected error")
	}
}

func TestPeekSessionInfo_PodAndState(t *testing.T) {
	cached := "running"
	s := &peekSessionInfo{
		ID:          "abc",
		Metadata:    json.RawMessage(`{"k8s":{"pod":"my-pod"}}`),
		CachedState: &cached,
	}
	if got := s.Pod(); got != "my-pod" {
		t.Errorf("Pod: %q", got)
	}
	if got := s.State(); got != "running" {
		t.Errorf("State: %q", got)
	}
	// Empty metadata → "-".
	s2 := &peekSessionInfo{Metadata: json.RawMessage(`{}`)}
	if got := s2.Pod(); got != "-" {
		t.Errorf("empty pod: %q", got)
	}
	if got := s2.State(); got != "unknown" {
		t.Errorf("unknown state: %q", got)
	}
}

// ── decision print helpers ────────────────────────────────────────────────

func TestPrintDecisionSummary(t *testing.T) {
	printDecisionSummary(&beadsapi.BeadDetail{
		ID:     "gd-D1",
		Title:  "pick",
		Status: "open",
		Fields: map[string]string{"prompt": "choose option", "options": `[{"id":"a","label":"A","artifact_type":"plan"},{"id":"b","short":"B"}]`},
	})
	// With chosen field.
	printDecisionSummary(&beadsapi.BeadDetail{
		ID:     "gd-D2",
		Title:  "pick2",
		Status: "open",
		Fields: map[string]string{"chosen": "a", "options": "[]"},
	})
	// No prompt or options.
	printDecisionSummary(&beadsapi.BeadDetail{
		ID: "gd-D3", Title: "title-only", Status: "open",
		Fields: map[string]string{},
	})
	// Malformed options JSON.
	printDecisionSummary(&beadsapi.BeadDetail{
		ID: "gd-D4", Fields: map[string]string{"options": "not json"},
	})
}

func TestPrintDecisionDetail(t *testing.T) {
	printDecisionDetail(&beadsapi.BeadDetail{
		ID:     "gd-DD1",
		Title:  "title",
		Status: "open",
		Fields: map[string]string{
			"prompt":            "p",
			"context":           "ctx",
			"options":           `[{"id":"a","short":"S","label":"L","artifact_type":"plan"},{"id":"b","short":"B"}]`,
			"chosen":            "a",
			"response_text":     "go for it",
			"responded_by":      "alice",
			"required_artifact": "plan",
			"artifact_status":   "pending",
		},
	})
	// Without prompt, bad options.
	printDecisionDetail(&beadsapi.BeadDetail{
		ID: "gd-DD2", Title: "t", Status: "open",
		Fields: map[string]string{"options": "not json"},
	})
}

// ── parseSimpleOption extra branches ──────────────────────────────────────

func TestParseSimpleOption_Variants(t *testing.T) {
	// id only
	m, err := parseSimpleOption("onlyid")
	if err != nil || m["id"] != "onlyid" || m["artifact_type"] != "report" {
		t.Errorf("one-part: %+v err=%v", m, err)
	}
	// id:label
	m, err = parseSimpleOption("id:Label")
	if err != nil || m["id"] != "id" || m["short"] != "Label" {
		t.Errorf("two-part: %+v err=%v", m, err)
	}
	// id:label:plan
	m, err = parseSimpleOption("id:Label:plan")
	if err != nil || m["artifact_type"] != "plan" {
		t.Errorf("three-part: %+v err=%v", m, err)
	}
	// id:label: (empty artifact defaults to report)
	m, err = parseSimpleOption("id:Label:")
	if err != nil || m["artifact_type"] != "report" {
		t.Errorf("empty artifact: %+v err=%v", m, err)
	}
	// invalid artifact type
	if _, err := parseSimpleOption("id:Label:bogus"); err == nil {
		t.Error("expected error for bogus artifact_type")
	}
}
