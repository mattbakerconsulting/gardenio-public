package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPublishHookEvent_Success(t *testing.T) {
	var gotPath string
	var gotBody PublishHookEventRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	payload := json.RawMessage(`{"agent":"worker-1","event":"PreToolUse","ts":"2026-03-02T07:30:00Z"}`)
	err = client.PublishHookEvent(context.Background(), PublishHookEventRequest{
		Subject: "hooks.worker-1.PreToolUse",
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotPath != "/v1/hooks/publish" {
		t.Errorf("expected path /v1/hooks/publish, got %s", gotPath)
	}
	if gotBody.Subject != "hooks.worker-1.PreToolUse" {
		t.Errorf("expected subject hooks.worker-1.PreToolUse, got %s", gotBody.Subject)
	}
}

func TestPublishHookEvent_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer srv.Close()

	client, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	err = client.PublishHookEvent(context.Background(), PublishHookEventRequest{
		Subject: "hooks.worker-1.Stop",
		Payload: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Error("expected error for server error response, got nil")
	}
}

func TestPublishHookEvent_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	client, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	err = client.PublishHookEvent(context.Background(), PublishHookEventRequest{
		Subject: "hooks.worker-1.Stop",
		Payload: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Error("expected error for 404 response, got nil")
	}
}
