package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAppendBeadArrayField_PostsElement verifies the client POSTs the element
// to the /fields/{field}/append endpoint with the field path-escaped.
func TestAppendBeadArrayField_PostsElement(t *testing.T) {
	var gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_ = json.NewEncoder(w).Encode(beadJSON{ID: "gd-plan"})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.AppendBeadArrayField(context.Background(), "gd-plan", "phase_order", "gd-new"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if gotPath != "/v1/beads/gd-plan/fields/phase_order/append" {
		t.Errorf("path = %q, want /v1/beads/gd-plan/fields/phase_order/append", gotPath)
	}
	if gotBody["element"] != "gd-new" {
		t.Errorf("body element = %q, want gd-new", gotBody["element"])
	}
}

// TestReorderBeadArrayField_PostsExpectedAndNext verifies the CAS body carries
// both the expected snapshot and the desired next array.
func TestReorderBeadArrayField_PostsExpectedAndNext(t *testing.T) {
	var gotPath string
	var gotBody struct {
		Expected []string `json:"expected"`
		Next     []string `json:"next"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_ = json.NewEncoder(w).Encode(beadJSON{ID: "gd-plan"})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	err := c.ReorderBeadArrayField(context.Background(), "gd-plan", "phase_order",
		[]string{"gd-a", "gd-b"}, []string{"gd-b", "gd-a"})
	if err != nil {
		t.Fatalf("reorder: %v", err)
	}
	if gotPath != "/v1/beads/gd-plan/fields/phase_order/reorder" {
		t.Errorf("path = %q", gotPath)
	}
	if len(gotBody.Expected) != 2 || gotBody.Expected[0] != "gd-a" {
		t.Errorf("expected = %v", gotBody.Expected)
	}
	if len(gotBody.Next) != 2 || gotBody.Next[0] != "gd-b" {
		t.Errorf("next = %v", gotBody.Next)
	}
}

// TestReorderBeadArrayField_NilArraysBecomeEmpty verifies nil expected/next are
// normalised to [] on the wire (never JSON null), so an absent field CAS is
// expressible and the daemon's COALESCE([]) default compares cleanly.
func TestReorderBeadArrayField_NilArraysBecomeEmpty(t *testing.T) {
	var raw map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &raw)
		_ = json.NewEncoder(w).Encode(beadJSON{ID: "gd-plan"})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.ReorderBeadArrayField(context.Background(), "gd-plan", "step_order", nil, nil); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	if string(raw["expected"]) != "[]" {
		t.Errorf("expected wire value = %s, want []", raw["expected"])
	}
	if string(raw["next"]) != "[]" {
		t.Errorf("next wire value = %s, want []", raw["next"])
	}
}

// TestReorderBeadArrayField_ConflictIsDetectable verifies a 409 surfaces as an
// *APIError that IsCASConflict recognises, so a caller can re-read and retry.
func TestReorderBeadArrayField_ConflictIsDetectable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "compare-and-swap conflict"})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	err := c.ReorderBeadArrayField(context.Background(), "gd-plan", "phase_order",
		[]string{"gd-a"}, []string{"gd-a"})
	if err == nil {
		t.Fatal("expected error on 409, got nil")
	}
	if !IsCASConflict(err) {
		t.Errorf("IsCASConflict(%v) = false, want true", err)
	}
}

// TestAppendBeadArrayField_NotFoundIsError verifies a 404 propagates as an error
// (append to a missing bead is a hard failure, not a silent no-op).
func TestAppendBeadArrayField_NotFoundIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "bead not found"})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	err := c.AppendBeadArrayField(context.Background(), "gd-missing", "phase_order", "gd-x")
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if IsCASConflict(err) {
		t.Errorf("404 must not be reported as a CAS conflict")
	}
}
