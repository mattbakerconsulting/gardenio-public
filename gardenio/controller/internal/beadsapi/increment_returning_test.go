package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIncrementBeadFieldsReturning verifies the fetch-and-add variant POSTs the
// deltas to /increment and returns the daemon's post-increment bead so callers
// can read back the value their own increment landed on.
func TestIncrementBeadFieldsReturning(t *testing.T) {
	var gotDeltas map[string]float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/beads/gd-counter/increment" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotDeltas)
		// Daemon echoes the post-increment bead; counter field comes back as a
		// JSON number, exercising the numeric re-marshal in ParseFieldsJSON.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "gd-counter",
			"fields": map[string]any{"jdark_counter": 7},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	bead, err := c.IncrementBeadFieldsReturning(context.Background(), "gd-counter", map[string]float64{"jdark_counter": 1})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if gotDeltas["jdark_counter"] != 1 {
		t.Fatalf("delta not sent: %+v", gotDeltas)
	}
	if bead == nil || bead.Fields["jdark_counter"] != "7" {
		t.Fatalf("post-increment value not returned: %+v", bead)
	}
}

func TestIncrementBeadFieldsReturning_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if _, err := c.IncrementBeadFieldsReturning(context.Background(), "gd-missing", map[string]float64{"n": 1}); err == nil {
		t.Fatal("expected error on 404")
	}
}
