package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The HTTP client carries arms on create, update and the list filter (heraldry
// spec §3.1, step 1.2 gd-ljbF6ftzdY); the gRPC half is pinned against the real
// server in internal/server/heraldry_grpc_test.go.
func TestHTTPClient_Heraldry(t *testing.T) {
	var query string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			query = r.URL.Query().Get("heraldry")
			_, _ = w.Write([]byte(`{"beads":[],"total":0}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"gd-1","heraldry":"blue 🔥 . 🧭 . 📦"}`))
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, "")
	ctx := context.Background()

	if _, err := c.ListBeads(ctx, &ListBeadsRequest{Heraldry: "🔥🧭📦 on blue"}); err != nil {
		t.Fatalf("ListBeads: %v", err)
	}
	if query != "🔥🧭📦 on blue" {
		t.Errorf("heraldry query = %q, want the arms as given", query)
	}

	b, err := c.CreateBead(ctx, &CreateBeadRequest{Title: "Plan", Type: "epic", Heraldry: "🔥🧭📦"})
	if err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if b.Heraldry != "blue 🔥 . 🧭 . 📦" {
		t.Errorf("created heraldry = %q, want the server's canonical form", b.Heraldry)
	}
	none := "none"
	if _, err := c.UpdateBead(ctx, "gd-1", &UpdateBeadRequest{Heraldry: &none}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}
	if _, err := c.UpdateBead(ctx, "gd-1", &UpdateBeadRequest{}); err != nil {
		t.Fatalf("UpdateBead (no arms): %v", err)
	}
	if len(bodies) != 3 || bodies[0]["heraldry"] != "🔥🧭📦" || bodies[1]["heraldry"] != "none" {
		t.Fatalf("bodies = %v, want heraldry on the create and the first update", bodies)
	}
	if _, set := bodies[2]["heraldry"]; set {
		t.Errorf("an update without arms sent heraldry: %v", bodies[2])
	}
}
