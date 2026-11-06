package server

import (
	"testing"
)

// Regression tests for ka-0000007023: the graph-family handlers (/v1/graph,
// /v1/ready) built a model.BeadFilter by hand and never passed
// through the maxListBeadsLimit OOM clamp — the sibling class of the gRPC
// ListBeads bypass (ka-0000003195 / MR !7128). A low-privilege authenticated
// caller could ask for an enormous limit and drive the shared kbeads daemon to
// OOM (fleet-wide DoS). handleGraph is the worst amplifier: one ListBeads per
// status plus per-bead batch hydrations.
//
// The load-bearing assertion in every case below is on the limit the STORE
// actually receives (ms.listFilters) — that is what bounds the query, not the
// handler's local variable.

func TestClampListBeadsLimit(t *testing.T) {
	cases := []struct {
		name  string
		limit int
		def   int
		want  int
	}{
		{"zero uses default", 0, 500, 500},
		{"negative uses default", -1, 500, 500},
		{"min-int negative uses default", -1 << 31, 200, 200},
		{"under cap preserved", 500, 200, 500},
		{"at cap preserved", maxListBeadsLimit, 200, maxListBeadsLimit},
		{"over cap clamped", maxListBeadsLimit + 1, 200, maxListBeadsLimit},
		{"huge over cap clamped", 2000000000, 500, maxListBeadsLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampListBeadsLimit(tc.limit, tc.def); got != tc.want {
				t.Fatalf("clampListBeadsLimit(%d, %d) = %d, want %d", tc.limit, tc.def, got, tc.want)
			}
		})
	}
}

// TestHandleGraph_LimitClampedAtStore proves the POST /v1/graph path — which
// fills args.Limit directly from JSON with no guard — cannot push an over-cap
// (or negative) limit into the store. Fault injection: before the fix,
// handleGraph set filter.Limit = args.Limit verbatim and these assertions fail.
func TestHandleGraph_LimitClampedAtStore(t *testing.T) {
	cases := []struct {
		name    string
		bodyLim any
		want    int
	}{
		{"huge clamped", 2000000000, maxListBeadsLimit},
		{"just over cap clamped", maxListBeadsLimit + 1, maxListBeadsLimit},
		{"negative uses default", -5, 500},
		{"omitted uses default", nil, 500},
		{"under cap preserved", 42, 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			body := map[string]any{}
			if tc.bodyLim != nil {
				body["limit"] = tc.bodyLim
			}
			rec := doJSON(t, h, "POST", "/v1/graph", body)
			requireStatus(t, rec, 200)

			if len(ms.listFilters) == 0 {
				t.Fatal("handleGraph did not call ListBeads")
			}
			for i, f := range ms.listFilters {
				if f.Limit != tc.want {
					t.Fatalf("ListBeads call %d: store received limit=%d, want clamped %d", i, f.Limit, tc.want)
				}
			}
		})
	}
}

// TestHandleGetReady_LimitClampedAtStore covers GET /v1/ready.
func TestHandleGetReady_LimitClampedAtStore(t *testing.T) {
	_, ms, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/ready?limit=2000000000", nil)
	requireStatus(t, rec, 200)
	if len(ms.listFilters) == 0 {
		t.Fatal("handleGetReady did not call ListBeads")
	}
	if got := ms.listFilters[0].Limit; got != maxListBeadsLimit {
		t.Fatalf("store received limit=%d, want clamped %d", got, maxListBeadsLimit)
	}
}
