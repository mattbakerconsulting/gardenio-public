package server

import (
	"testing"
)

// Regression tests for ka-0000001252: the RoleViewer-gated list handlers
// handleListDecisions (/v1/decisions) and handleListEscalations
// (/v1/escalations) built a model.BeadFilter by hand and
// set filter.Limit straight from the user-supplied ?limit= query param with only
// an n>0 lower guard — bypassing the maxListBeadsLimit OOM clamp (ka-0000006889).
// This is the same sibling class as the gRPC ListBeads bypass (ka-0000003195 /
// MR !7128) and the graph-family bypass (ka-0000007023 / MR !7150): a low-privilege
// caller could ask for an enormous limit and drive the single shared kbeads
// daemon to OOM (fleet-wide DoS).
//
// The load-bearing assertion in every case is on the limit the STORE actually
// receives (ms.listFilters) — that is what bounds the query, not the handler's
// local variable. Fault injection: revert any handler to `filter.Limit = n` /
// `if filter.Limit == 0 { filter.Limit = 50 }` and the over-cap cases fail.

func TestHandleListDecisions_LimitClampedAtStore(t *testing.T) {
	assertListLimitClamped(t, "/v1/decisions", 50)
}

func TestHandleListEscalations_LimitClampedAtStore(t *testing.T) {
	assertListLimitClamped(t, "/v1/escalations", 50)
}

// assertListLimitClamped drives the given GET list route with a range of
// ?limit= values and asserts the store received the clamped/defaulted limit.
func assertListLimitClamped(t *testing.T, path string, def int) {
	t.Helper()
	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"omitted uses default", "", def},
		{"under cap preserved", "?limit=42", 42},
		{"at cap preserved", "?limit=10000", maxListBeadsLimit},
		{"over cap clamped", "?limit=10001", maxListBeadsLimit},
		{"huge over cap clamped", "?limit=2000000000", maxListBeadsLimit},
		{"negative uses default", "?limit=-5", def},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			rec := doJSON(t, h, "GET", path+tc.query, nil)
			requireStatus(t, rec, 200)
			if len(ms.listFilters) == 0 {
				t.Fatalf("%s did not call ListBeads", path)
			}
			for i, f := range ms.listFilters {
				if f.Limit != tc.want {
					t.Fatalf("%s%s: ListBeads call %d received limit=%d, want clamped %d",
						path, tc.query, i, f.Limit, tc.want)
				}
			}
		})
	}
}
