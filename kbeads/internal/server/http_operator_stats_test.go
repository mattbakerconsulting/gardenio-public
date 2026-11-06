package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/kbeads/internal/store"
)

// TestHandleOperatorStats_ReturnsAggregate verifies GET /v1/operators/stats
// serializes the store's real per-operator aggregate (ka-0000001390): the
// open-count + last-active stats and the recent-chips list, with last_active
// rendered as RFC3339.
func TestHandleOperatorStats_ReturnsAggregate(t *testing.T) {
	_, ms, h := newTestServer()
	t0 := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	ms.operatorDirStats = &store.OperatorDirectoryStats{
		Stats: []store.OperatorStat{
			{
				OperatorID: 42, OpenBeads: 7,
				InProgress: 3, Created7d: 9, Closed7d: 4,
				MRsOpen: 2, MRsMerged7d: 1, DecisionsPending: 5,
				AgentsActive: 6, GardenersActive: 8,
				LastActive: t0,
			},
			{OperatorID: 99, OpenBeads: 0, LastActive: time.Time{}},
		},
		Recent: []store.OperatorRecentBead{
			{OperatorID: 42, BeadID: "gd-a1", Title: "Alice newest", CreatedAt: t0},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/operators/stats?recent_per_operator=5&recent_limit=100", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp OperatorStatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Stats) != 2 {
		t.Fatalf("stats len = %d, want 2", len(resp.Stats))
	}
	if resp.Stats[0].OperatorID != 42 || resp.Stats[0].OpenBeads != 7 {
		t.Errorf("stats[0] = %+v, want 42/7", resp.Stats[0])
	}
	if resp.Stats[0].LastActive != "2026-06-04T12:00:00Z" {
		t.Errorf("stats[0].LastActive = %q, want RFC3339", resp.Stats[0].LastActive)
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

// TestHandleOperatorStats_InvalidParam rejects a negative / non-numeric cap.
func TestHandleOperatorStats_InvalidParam(t *testing.T) {
	_, _, h := newTestServer()
	for _, q := range []string{"recent_per_operator=-1", "recent_limit=abc"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/operators/stats?"+q, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, w.Code)
		}
	}
}

// TestHandleOperatorStats_StoreError maps a store failure to 500.
func TestHandleOperatorStats_StoreError(t *testing.T) {
	_, ms, h := newTestServer()
	ms.operatorDirStatsErr = errors.New("boom")

	req := httptest.NewRequest(http.MethodGet, "/v1/operators/stats", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleOperatorStats_ParamsClampedAtStore pins that GET /v1/operators/stats
// bounds its three caller-supplied params through the shared OOM choke point
// before the store (ka-0000006889). handleOperatorStats is RoleViewer-gated and
// powers the public /people directory just like the sibling handleListOperators
// (already clamped), but its params flowed straight into the store with only an
// n<0 reject — a low-privilege caller passing huge values erased every bound
// (recent_limit -> LIMIT $2, recent_per_operator -> rn <= $1,
// recent_window_hours -> age interval) and made the store materialize the whole
// beads table into memory. This asserts the store RECEIVES the clamped values,
// which is what bounds the SQL — not the handler's locals.
func TestHandleOperatorStats_ParamsClampedAtStore(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  [3]int // {recentPerOperator, recentLimit, recentWindowHours}
	}{
		{"omitted preserves store defaults (0/0/0)", "", [3]int{0, 0, 0}},
		{"under caps preserved", "?recent_per_operator=5&recent_limit=100&recent_window_hours=24", [3]int{5, 100, 24}},
		{
			"recent_limit huge clamped, window huge clamped",
			"?recent_limit=2000000000&recent_per_operator=100000000&recent_window_hours=8760000",
			[3]int{maxListBeadsLimit, maxListBeadsLimit, maxOperatorStatsRecentWindowHours},
		},
		{
			"recent_limit at cap preserved, window under cap preserved",
			"?recent_limit=10000&recent_window_hours=876000",
			[3]int{0, maxListBeadsLimit, 876000},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			req := httptest.NewRequest(http.MethodGet, "/v1/operators/stats"+tc.query, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if ms.operatorDirStatsArgs != tc.want {
				t.Fatalf("store received %v, want clamped %v", ms.operatorDirStatsArgs, tc.want)
			}
		})
	}
}

// TestHandleOperatorStats_NegativeParamRejected pins that a negative param
// stays a 400 (unchanged by the clamp) — the clamp only bounds the upper end,
// it does not turn an invalid input into a silent default.
func TestHandleOperatorStats_NegativeParamRejected(t *testing.T) {
	for _, key := range []string{"recent_per_operator", "recent_limit", "recent_window_hours"} {
		t.Run(key, func(t *testing.T) {
			_, _, h := newTestServer()
			req := httptest.NewRequest(http.MethodGet, "/v1/operators/stats?"+key+"=-5", nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for negative %s; body=%s", w.Code, key, w.Body.String())
			}
		})
	}
}
