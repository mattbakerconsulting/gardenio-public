package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestHTTPHandlersRejectOversizedBody exercises AUDIT-2026-04-18 §H1: every
// JSON mutation handler must wrap r.Body with http.MaxBytesReader so a large
// pre-auth payload cannot exhaust beads process memory before any policy
// check. A body larger than beadsMaxBodyBytes must be rejected with 413.
func TestHTTPHandlersRejectOversizedBody(t *testing.T) {
	// Construct a payload >1 MiB. JSON just needs to be syntactically valid up
	// through the point MaxBytesReader trips; 1.5 MiB of filler string is well
	// past the 1 MiB cap.
	filler := strings.Repeat("A", 1<<20+1024)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"CreateBead", "POST", "/v1/beads", `{"title":"` + filler + `","type":"task"}`},
		{"UpdateBead", "PATCH", "/v1/beads/gd-u1", `{"title":"` + filler + `"}`},
		{"IncrementBeadFields", "POST", "/v1/beads/gd-inc1/increment", `{"counter":1,"filler":"` + filler + `"}`},
		{"CloseBead", "POST", "/v1/beads/gd-close1/close", `{"note":"` + filler + `"}`},
		{"AddComment", "POST", "/v1/beads/gd-cmt1/comments", `{"text":"` + filler + `"}`},
		{"AddLabel", "POST", "/v1/beads/gd-lbl1/labels", `{"label":"` + filler + `"}`},
		{"AddDependency", "POST", "/v1/beads/gd-dep1/dependencies", `{"depends_on_id":"gd-other","note":"` + filler + `"}`},
		{"HookEmit", "POST", "/v1/hooks/emit", `{"agent_bead_id":"a","hook_type":"Stop","cwd":"` + filler + `"}`},
		{"HookPublish", "POST", "/v1/hooks/publish", `{"subject":"x","payload":` + filler[:0] + `"` + filler + `"}`},
		{"ExecuteHooks", "POST", "/v1/hooks/execute", `{"agent_id":"a","trigger":"t","cwd":"` + filler + `"}`},
		{"GraphPost", "POST", "/v1/graph", `{"note":"` + filler + `"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			// Seed referenced beads so the handlers don't 404 before the decode.
			ms.beads["gd-u1"] = &model.Bead{ID: "gd-u1", Status: model.StatusOpen}
			ms.beads["gd-close1"] = &model.Bead{ID: "gd-close1", Status: model.StatusOpen}
			ms.beads["gd-cmt1"] = &model.Bead{ID: "gd-cmt1", Status: model.StatusOpen}
			ms.beads["gd-lbl1"] = &model.Bead{ID: "gd-lbl1", Status: model.StatusOpen}
			ms.beads["gd-dep1"] = &model.Bead{ID: "gd-dep1", Status: model.StatusOpen}
			ms.beads["gd-inc1"] = &model.Bead{ID: "gd-inc1", Status: model.StatusOpen}

			req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("%s: expected 413, got %d; body=%s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHTTPHandlersAcceptModestBody verifies the bound is only tripped by
// genuinely oversized bodies — a normal-sized payload must still decode.
func TestHTTPHandlersAcceptModestBody(t *testing.T) {
	_, _, h := newTestServer()

	body := `{"title":"normal size","type":"task"}`
	req := httptest.NewRequest("POST", "/v1/beads", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("normal-sized body should not trip MaxBytesReader; got 413: %s", rec.Body.String())
	}
}

// TestSetConfigAllowsLargerBody confirms PUT /v1/configs accepts bodies
// between beadsMaxBodyBytes and beadsMaxConfigBodyBytes (formulas/type-config
// JSON need headroom).
func TestSetConfigAllowsLargerBody(t *testing.T) {
	_, _, h := newTestServer()

	// 1.5 MiB payload — over the default cap, under the config-specific cap.
	filler := strings.Repeat("B", (beadsMaxBodyBytes*3)/2)
	body := `{"value":{"s":"` + filler + `"}}`

	req := httptest.NewRequest("PUT", "/v1/configs/type:custom", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("SetConfig should accept 1.5 MiB body (under %d); got 413", beadsMaxConfigBodyBytes)
	}
}

// TestSetConfigRejectsHugeBody confirms PUT /v1/configs still caps oversized
// bodies.
func TestSetConfigRejectsHugeBody(t *testing.T) {
	_, _, h := newTestServer()

	// 5 MiB payload — over the config cap.
	filler := strings.Repeat("B", beadsMaxConfigBodyBytes+1024)
	body := `{"value":{"s":"` + filler + `"}}`

	req := httptest.NewRequest("PUT", "/v1/configs/type:custom", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("SetConfig should reject body over %d bytes; got %d: %s",
			beadsMaxConfigBodyBytes, rec.Code, rec.Body.String())
	}
}
