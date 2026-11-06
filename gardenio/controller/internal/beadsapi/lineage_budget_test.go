package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestLineageContinuationCount_QueryShape pins the exact daemon query the budget
// counter issues: a count-only listing of type=agent beads filtered on the
// lineage_root_id field, across ALL statuses (no status param — closed
// predecessors must still count against the ceiling).
func TestLineageContinuationCount_QueryShape(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 3})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	count, err := c.LineageContinuationCount(context.Background(), "gd-root-1")
	if err != nil {
		t.Fatalf("LineageContinuationCount: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
	if got := gotQuery.Get("type"); got != "agent" {
		t.Errorf("type param = %q, want agent", got)
	}
	if got := gotQuery.Get("count_only"); got != "true" {
		t.Errorf("count_only param = %q, want true", got)
	}
	// The lineage predicate must arrive as a field= entry.
	fields := gotQuery["field"]
	if len(fields) != 1 || fields[0] != "lineage_root_id=gd-root-1" {
		t.Errorf("field params = %v, want [lineage_root_id=gd-root-1]", fields)
	}
	// No status filter: the count spans open + closed so a closed predecessor
	// still counts. If a status param leaked in, closed predecessors would be
	// undercounted and the budget could be bypassed.
	if got := gotQuery.Get("status"); got != "" {
		t.Errorf("status param = %q, want empty (all statuses)", got)
	}
}

// TestLineageContinuationCount_EmptyRoot rejects an empty root rather than
// issuing a query that would match every agent bead lacking the field.
func TestLineageContinuationCount_EmptyRoot(t *testing.T) {
	c := &Client{baseURL: "http://unused", httpClient: http.DefaultClient}
	if _, err := c.LineageContinuationCount(context.Background(), ""); err == nil {
		t.Error("expected an error for an empty lineage_root_id, got nil")
	}
}

// TestLineageContinuationCount_ServerError surfaces a daemon error so the caller
// can fail-open on it (rather than silently reading 0 and skipping the gate).
func TestLineageContinuationCount_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if _, err := c.LineageContinuationCount(context.Background(), "gd-root-1"); err == nil {
		t.Error("expected an error on daemon 500, got nil")
	}
}
