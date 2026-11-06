package main

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// postRequestPeel drives handleRequestPeel for the given MR bead id as the
// given operator email, returning the recorder.
func postRequestPeel(t *testing.T, srv *Server, mrBeadID, email string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/peel-catalog/"+mrBeadID+"/request", nil)
	req.SetPathValue("id", mrBeadID)
	if email != "" {
		req = req.WithContext(oidcauth.ContextWithEmail(req.Context(), email))
	}
	rec := httptest.NewRecorder()
	srv.handleRequestPeel(rec, req)
	return rec
}

// TestHandleRequestPeel_FilesRequestBead is the core write-path assertion: a
// request files ONE type=task peel-request bead (label peel-request), wires the
// produces edge to the MR bead, stamps peel:requested on the MR, and opens NO
// upstream MR.
func TestHandleRequestPeel_FilesRequestBead(t *testing.T) {
	mr := peelMRBead("gd-mr1", "peelable slice", []string{peelCatalogLabelEligible, "risk:low"}, map[string]string{
		"mr_url": "https://dark.test/g/p/-/merge_requests/9",
	})
	daemon := &fakeDaemon{
		beads:      map[string]*beadsapi.BeadDetail{"gd-mr1": mr},
		deps:       map[string][]beadsapi.Dependency{},
		createResp: "gd-peelreq1",
	}
	srv := newPeelTestServer(t, daemon)

	rec := postRequestPeel(t, srv, "gd-mr1", "op@acme.example")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}

	// Exactly one bead created, and it is a task carrying the peel-request label.
	if daemon.createCallsOfType("task") != 1 {
		t.Fatalf("task creates = %d, want 1", daemon.createCallsOfType("task"))
	}
	if n := daemon.createCallsOfType("merge-request"); n != 0 {
		t.Fatalf("merge-request creates = %d, want 0 — request must NOT open an upstream MR", n)
	}
	var req *beadsapi.CreateBeadRequest
	for i := range daemon.createReqs {
		if daemon.createReqs[i].Type == "task" {
			req = &daemon.createReqs[i]
		}
	}
	if req == nil {
		t.Fatal("no task create request recorded")
	}
	if !slices.Contains(req.Labels, peelRequestLabel) {
		t.Errorf("peel-request bead labels = %v, want to include %q", req.Labels, peelRequestLabel)
	}
	if req.IdempotencyKey != "peel-request:gd-mr1" {
		t.Errorf("IdempotencyKey = %q, want peel-request:gd-mr1", req.IdempotencyKey)
	}
	// Fields carry the MR back-reference + requester.
	fields := beadsapi.ParseFieldsJSON(req.Fields)
	if fields[peelRequestFieldMRBead] != "gd-mr1" {
		t.Errorf("field %s = %q, want gd-mr1", peelRequestFieldMRBead, fields[peelRequestFieldMRBead])
	}
	if fields[peelRequestFieldRequestedBy] != "op@acme.example" {
		t.Errorf("field %s = %q, want op@acme.example", peelRequestFieldRequestedBy, fields[peelRequestFieldRequestedBy])
	}

	// related edge: peel-request --related--> MR bead (NOT produces — the dark
	// MR already exists; the request merely points at it).
	if daemon.addDepLastType != "related" || daemon.addDepLastBead != "gd-peelreq1" || daemon.addDepLastDep != "gd-mr1" {
		t.Errorf("edge = %s --%s--> %s, want gd-peelreq1 --related--> gd-mr1",
			daemon.addDepLastBead, daemon.addDepLastType, daemon.addDepLastDep)
	}
	// peel:requested stamped on the MR bead.
	if !slices.Contains(daemon.addLabelAll, peelRequestedMRLabel) {
		t.Errorf("addLabelAll = %v, want to include %q on the MR bead", daemon.addLabelAll, peelRequestedMRLabel)
	}
	// The operator is stamped on the write context (audit).
	if daemon.createLastOpBy == "" {
		t.Error("expected the operator to be stamped on the create context (X-Operated-By)")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "peel requested") || !strings.Contains(body, "gd-peelreq1") {
		t.Errorf("result fragment missing confirmation; body=\n%s", body)
	}
}

// TestHandleRequestPeel_Idempotent confirms a second request on an
// already-requested MR files nothing new and reports "already requested".
func TestHandleRequestPeel_Idempotent(t *testing.T) {
	mr := peelMRBead("gd-mr2", "already asked", []string{peelCatalogLabelEligible, peelRequestedMRLabel}, nil)
	daemon := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-mr2": mr}}
	srv := newPeelTestServer(t, daemon)

	rec := postRequestPeel(t, srv, "gd-mr2", "op@acme.example")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if daemon.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0 — duplicate request must file nothing", daemon.createCalls)
	}
	if !strings.Contains(rec.Body.String(), "already requested") {
		t.Errorf("expected 'already requested'; body=\n%s", rec.Body.String())
	}
}

// TestHandleRequestPeel_Unauthenticated rejects a signed-out POST.
func TestHandleRequestPeel_Unauthenticated(t *testing.T) {
	daemon := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{}}
	srv := newPeelTestServer(t, daemon)
	rec := postRequestPeel(t, srv, "gd-mr3", "") // no email
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if daemon.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0", daemon.createCalls)
	}
}

// TestHandleRequestPeel_NonMRBead refuses to file a request against a bead that
// is not a merge-request.
func TestHandleRequestPeel_NonMRBead(t *testing.T) {
	task := &beadsapi.BeadDetail{ID: "gd-task", Type: "task", Fields: map[string]string{}}
	daemon := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-task": task}}
	srv := newPeelTestServer(t, daemon)
	rec := postRequestPeel(t, srv, "gd-task", "op@acme.example")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if daemon.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0 for a non-MR bead", daemon.createCalls)
	}
}
