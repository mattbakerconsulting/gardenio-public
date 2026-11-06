package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAddLabel_SendsCorrectRequest(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.AddLabel(context.Background(), "gd-bead1", "project:gardenio")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/beads/gd-bead1/labels" {
		t.Errorf("expected path /v1/beads/gd-bead1/labels, got %s", gotPath)
	}
	if gotBody["label"] != "project:gardenio" {
		t.Errorf("expected label=project:gardenio, got %s", gotBody["label"])
	}
}

func TestAddLabel_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid label format"}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.AddLabel(context.Background(), "gd-bead1", "bad label")
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

func TestRemoveLabel_SendsCorrectRequest(t *testing.T) {
	var gotMethod, gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.RemoveLabel(context.Background(), "gd-bead1", "obsolete")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotMethod != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
	if gotPath != "/v1/beads/gd-bead1/labels/obsolete" {
		t.Errorf("expected path /v1/beads/gd-bead1/labels/obsolete, got %s", gotPath)
	}
}

func TestRemoveLabel_HandlesLabelWithColon(t *testing.T) {
	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.RemoveLabel(context.Background(), "gd-bead1", "project:gardenio")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The colon may or may not be percent-encoded depending on Go's URL handling;
	// either way the decoded path should contain the full label value.
	if gotPath != "/v1/beads/gd-bead1/labels/project:gardenio" {
		t.Errorf("expected path with label, got %s", gotPath)
	}
}
