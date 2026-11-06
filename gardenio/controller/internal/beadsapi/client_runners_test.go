package beadsapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRevokeRunner_PostsAndDecodesCounts(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"revoked_tokens":2,"agents_killed":3}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	tokens, agents, err := c.RevokeRunner(context.Background(), "gd-r/1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/runners/gd-r%2F1/revoke" {
		t.Fatalf("got %s %s, want POST /v1/runners/gd-r%%2F1/revoke", gotMethod, gotPath)
	}
	if tokens != 2 || agents != 3 {
		t.Fatalf("got tokens=%d agents=%d, want 2 and 3", tokens, agents)
	}
}

func TestRevokeRunner_ForbiddenIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"only the runner's owner or an admin may revoke it"}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	tokens, agents, err := c.RevokeRunner(context.Background(), "gd-r1")
	if err == nil {
		t.Fatal("a 403 must surface as an error")
	}
	if tokens != 0 || agents != 0 {
		t.Fatalf("a refused revoke must report no counts, got tokens=%d agents=%d", tokens, agents)
	}
}
