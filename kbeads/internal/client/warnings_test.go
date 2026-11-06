package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestWithWarnings_CollectsHeaderOnSuccessOnly(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add(WarningHeader, "short_title missing: one")
		w.Header().Add(WarningHeader, "two")
		w.WriteHeader(status)
		if status >= 400 {
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"gd-1"}`))
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, "")

	ctx, w := WithWarnings(context.Background())
	if _, err := c.CreateBead(ctx, &CreateBeadRequest{Title: "t", Type: "task"}); err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if got := w.List(); !slices.Equal(got, []string{"short_title missing: one", "two"}) {
		t.Errorf("warnings = %q", got)
	}

	status = http.StatusBadRequest
	ctx, w = WithWarnings(context.Background())
	if _, err := c.UpdateBead(ctx, "gd-1", &UpdateBeadRequest{}); err == nil {
		t.Fatal("UpdateBead: want an error on 400")
	}
	if got := w.List(); len(got) != 0 {
		t.Errorf("warnings on a 400 = %q, want none", got)
	}

	// A context without a collector is fine.
	status = http.StatusOK
	if _, err := c.GetBead(context.Background(), "gd-1"); err != nil {
		t.Fatalf("GetBead: %v", err)
	}
}

func TestListBeads_ShortTitleMissingQuery(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("short_title_missing")
		_, _ = w.Write([]byte(`{"beads":[],"total":0}`))
	}))
	defer srv.Close()

	if _, err := NewHTTPClient(srv.URL, "").ListBeads(context.Background(), &ListBeadsRequest{ShortTitleMissing: true}); err != nil {
		t.Fatalf("ListBeads: %v", err)
	}
	if got != "true" {
		t.Errorf("short_title_missing = %q, want true", got)
	}
}
