package coopapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetScreen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/screen" || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Write([]byte("raw screen"))
	}))
	defer srv.Close()

	c := New(srv.URL)
	text, err := c.GetScreen(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "raw screen" {
		t.Errorf("unexpected text: %q", text)
	}
}

func TestGetScreen_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.GetScreen(context.Background())
	if err == nil {
		t.Fatal("expected error for 502 response")
	}
}

func TestGetScreenText_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.GetScreenText(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestGetScreenText_ConnectionRefused(t *testing.T) {
	c := New("http://localhost:1")
	_, err := c.GetScreenText(context.Background())
	if err == nil {
		t.Fatal("expected connection error")
	}
}

func TestGetSessionStatus(t *testing.T) {
	pid := 4242
	exit := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/status" || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(SessionStatus{
			SessionID:    "sess-1",
			State:        "running",
			PID:          &pid,
			UptimeSecs:   120,
			ExitCode:     &exit,
			ScreenSeq:    7,
			BytesRead:    100,
			BytesWritten: 200,
			WSClients:    1,
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	status, err := c.GetSessionStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.SessionID != "sess-1" {
		t.Errorf("expected session_id=sess-1, got %q", status.SessionID)
	}
	if status.PID == nil || *status.PID != 4242 {
		t.Errorf("expected pid=4242, got %v", status.PID)
	}
	if status.ScreenSeq != 7 {
		t.Errorf("expected screen_seq=7, got %d", status.ScreenSeq)
	}
}

func TestGetSessionStatus_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.GetSessionStatus(context.Background())
	if err == nil {
		t.Fatal("expected error for 503 response")
	}
}

func TestListTranscripts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/transcripts" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(TranscriptList{
			Transcripts: []TranscriptInfo{
				{Number: 1, Timestamp: "2026-01-01T00:00:00Z", LineCount: 10, ByteSize: 256},
				{Number: 2, Timestamp: "2026-01-02T00:00:00Z", LineCount: 20, ByteSize: 512},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	list, err := c.ListTranscripts(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(list.Transcripts) != 2 {
		t.Fatalf("expected 2 transcripts, got %d", len(list.Transcripts))
	}
	if list.Transcripts[0].Number != 1 || list.Transcripts[1].Number != 2 {
		t.Errorf("unexpected numbers: %+v", list.Transcripts)
	}
}

func TestListTranscripts_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.ListTranscripts(context.Background())
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestGetTranscript(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/transcripts/3" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"line":1}` + "\n" + `{"line":2}` + "\n"))
	}))
	defer srv.Close()

	c := New(srv.URL)
	data, err := c.GetTranscript(context.Background(), 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(data), `"line":1`) || !strings.Contains(string(data), `"line":2`) {
		t.Errorf("unexpected body: %q", data)
	}
}

func TestGetTranscript_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.GetTranscript(context.Background(), 99)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestGetTranscript_ConnectionRefused(t *testing.T) {
	c := New("http://localhost:1")
	_, err := c.GetTranscript(context.Background(), 1)
	if err == nil {
		t.Fatal("expected connection error")
	}
}

func TestGetEventsCatchup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/events/catchup" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"state_events":[{"kind":"state"}],"hook_events":[{"kind":"hook"},{"kind":"hook2"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	events, err := c.GetEventsCatchup(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events.StateEvents) != 1 {
		t.Errorf("expected 1 state event, got %d", len(events.StateEvents))
	}
	if len(events.HookEvents) != 2 {
		t.Errorf("expected 2 hook events, got %d", len(events.HookEvents))
	}
}

func TestGetEventsCatchupSince(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/events/catchup" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{"state_events":[{"seq":11}],"hook_events":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	events, err := c.GetEventsCatchupSince(context.Background(), 10, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotQuery != "since_seq=10&since_hook_seq=5" {
		t.Errorf("query = %q, want since_seq=10&since_hook_seq=5", gotQuery)
	}
	if len(events.StateEvents) != 1 {
		t.Errorf("expected 1 state event, got %d", len(events.StateEvents))
	}
}

func TestGetEventsCatchup_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.GetEventsCatchup(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestGetRecording(t *testing.T) {
	body := `{"seq":1}` + "\n" + `{"seq":2}` + "\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/recording/download" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New(srv.URL)
	data, err := c.GetRecording(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != body {
		t.Errorf("unexpected body: %q", data)
	}
}

func TestGetRecording_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.GetRecording(context.Background())
	if err == nil {
		t.Fatal("expected error for 502 response")
	}
}

func TestRespondFull_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(srv.URL)
	accept := true
	_, err := c.RespondFull(context.Background(), RespondRequest{Accept: &accept})
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

func TestNudge_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.Nudge(context.Background(), "hey")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestShutdown_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL)
	err := c.Shutdown(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestSendKeys_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL)
	err := c.SendKeys(context.Background(), []string{"Return"})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestRespond_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(srv.URL)
	err := c.Respond(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

// TestGetJSON_InvalidJSON covers the decode-error path in getJSON.
func TestGetJSON_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this is not json"))
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.GetAgentState(context.Background())
	if err == nil {
		t.Fatal("expected decode error for invalid JSON")
	}
}

// TestPostJSON_ConnectionRefused covers request-failed path in postJSON.
func TestPostJSON_ConnectionRefused(t *testing.T) {
	c := New("http://localhost:1")
	err := c.Shutdown(context.Background())
	if err == nil {
		t.Fatal("expected connection error")
	}
}

// TestPostJSON_ResponseDecodeError covers the decode path for response bodies.
func TestPostJSON_ResponseDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<<garbage>>"))
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.Nudge(context.Background(), "x")
	if err == nil {
		t.Fatal("expected decode error for garbage response body")
	}
}

// TestPostJSON_UnmarshalableBody covers the body-marshal error path.
// json.Marshal returns an error for channels.
func TestPostJSON_UnmarshalableBody(t *testing.T) {
	c := New("http://localhost:0")
	err := c.postJSON(context.Background(), "/anything", make(chan int), nil)
	if err == nil {
		t.Fatal("expected marshal error for channel body")
	}
	if !strings.Contains(err.Error(), "marshal body") {
		t.Errorf("expected marshal-body error, got %v", err)
	}
}

// TestNewRequest_BadURL exercises the http.NewRequestWithContext error branch
// by passing a baseURL that produces an invalid URL (control character).
func TestNewRequest_BadURL(t *testing.T) {
	c := New("http://localhost:8080\x7f")
	ctx := context.Background()

	if _, err := c.GetAgentState(ctx); err == nil {
		t.Error("expected request-creation error in getJSON")
	}
	if _, err := c.GetScreenText(ctx); err == nil {
		t.Error("expected request-creation error in getText")
	}
	if _, err := c.GetTranscript(ctx, 1); err == nil {
		t.Error("expected request-creation error in getBytes")
	}
	if err := c.Shutdown(ctx); err == nil {
		t.Error("expected request-creation error in postJSON")
	}
}

// Sanity check the status-code error messages include path + code for quick ops debugging.
func TestErrorMessages_IncludeStatusAndPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	c := New(srv.URL)

	for _, tc := range []struct {
		name    string
		call    func() error
		wantSub string
	}{
		{"getJSON", func() error { _, e := c.GetAgentState(context.Background()); return e }, "/api/v1/agent"},
		{"getText", func() error { _, e := c.GetScreenText(context.Background()); return e }, "/api/v1/screen/text"},
		{"getBytes", func() error { _, e := c.GetRecording(context.Background()); return e }, "/api/v1/recording/download"},
		{"postJSON", func() error { return c.Shutdown(context.Background()) }, "/api/v1/shutdown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error()
			if !strings.Contains(msg, fmt.Sprintf("%d", http.StatusTeapot)) || !strings.Contains(msg, tc.wantSub) {
				t.Errorf("error message should include status and path, got %q", msg)
			}
		})
	}
}
