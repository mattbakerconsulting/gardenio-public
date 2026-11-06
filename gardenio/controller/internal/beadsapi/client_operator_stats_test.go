package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestOperatorStats_SendsParamsAndParses confirms the client encodes the cap
// params into the query string and parses the daemon's aggregate response
// (ka-0000001390).
func TestOperatorStats_SendsParamsAndParses(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(OperatorStatsResponse{
			Stats: []OperatorStatEntry{
				{
					OperatorID: 42, OpenBeads: 7,
					InProgress: 3, Created7d: 9, Closed7d: 4,
					MRsOpen: 2, MRsMerged7d: 1, DecisionsPending: 5,
					AgentsActive: 6, GardenersActive: 8,
					LastActive: "2026-06-04T12:00:00Z",
				},
			},
			Recent: []OperatorRecentBeadEntry{
				{OperatorID: 42, BeadID: "gd-a1", Title: "Alice newest", CreatedAt: "2026-06-04T12:00:00Z"},
			},
		})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	resp, err := c.OperatorStats(context.Background(), 5, 100, 24)
	if err != nil {
		t.Fatalf("OperatorStats: %v", err)
	}
	if gotPath != "/v1/operators/stats" {
		t.Errorf("path = %q, want /v1/operators/stats", gotPath)
	}
	if gotQuery != "recent_limit=100&recent_per_operator=5&recent_window_hours=24" {
		t.Errorf("query = %q, want recent_limit=100&recent_per_operator=5&recent_window_hours=24", gotQuery)
	}
	if len(resp.Stats) != 1 || resp.Stats[0].OperatorID != 42 || resp.Stats[0].OpenBeads != 7 {
		t.Errorf("stats = %+v, want one 42/7 entry", resp.Stats)
	}
	if s := resp.Stats[0]; s.InProgress != 3 || s.Created7d != 9 || s.Closed7d != 4 ||
		s.MRsOpen != 2 || s.MRsMerged7d != 1 || s.DecisionsPending != 5 ||
		s.AgentsActive != 6 || s.GardenersActive != 8 {
		t.Errorf("stats[0] counts = %+v, want in_progress=3 created7d=9 closed7d=4 mrs_open=2 mrs_merged7d=1 dec=5 agents=6 gardeners=8", s)
	}
	if len(resp.Recent) != 1 || resp.Recent[0].BeadID != "gd-a1" {
		t.Errorf("recent = %+v, want one gd-a1 chip", resp.Recent)
	}
}

// TestOperatorStats_OmitsZeroParams confirms zero caps produce a bare path
// (daemon applies its own defaults).
func TestOperatorStats_OmitsZeroParams(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(OperatorStatsResponse{})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if _, err := c.OperatorStats(context.Background(), 0, 0, 0); err != nil {
		t.Fatalf("OperatorStats: %v", err)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty", gotQuery)
	}
}

// TestOperatorStats_ServerError surfaces a non-2xx as an error.
func TestOperatorStats_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if _, err := c.OperatorStats(context.Background(), 5, 100, 24); err == nil {
		t.Fatal("expected error on 500")
	}
}
