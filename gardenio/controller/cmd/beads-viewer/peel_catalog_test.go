package main

import (
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// peelMRBead is a test helper for a type=merge-request bead carrying the given
// labels + fields.
func peelMRBead(id, title string, labels []string, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{}
	maps.Copy(f, fields)
	return &beadsapi.BeadDetail{
		ID:     id,
		Title:  title,
		Type:   "merge-request",
		Status: "closed",
		Labels: labels,
		Fields: f,
	}
}

// TestBuildPeelCatalogRows_RankingAndSplit pins the core menu semantics:
// eligible slices rank by score desc (ties → smaller size first), parked
// slices land in their own section, and unknown scores/sizes don't fabricate.
func TestBuildPeelCatalogRows_RankingAndSplit(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		peelMRBead("gd-a", "add coverage for foo", []string{peelCatalogLabelEligible, "risk:low"}, map[string]string{
			"peel_score": "72", "peel_size_loc": "120",
		}),
		peelMRBead("gd-b", "bump dep", []string{peelCatalogLabelEligible, "risk:medium"}, map[string]string{
			"peel_score": "90", "peel_size_loc": "40",
		}),
		peelMRBead("gd-c", "risky refactor", []string{peelCatalogLabelParked, "risk:high"}, map[string]string{
			"peel_score": "30",
		}),
		// Same score as gd-a but a smaller slice → must sort ahead of gd-a.
		peelMRBead("gd-d", "tiny doc fix", []string{peelCatalogLabelEligible, "risk:low"}, map[string]string{
			"peel_score": "72", "peel_size_loc": "10",
		}),
		nil, // tolerated
	}

	eligible, parked := buildPeelCatalogRows(beads)

	if len(eligible) != 3 {
		t.Fatalf("eligible count = %d, want 3", len(eligible))
	}
	wantOrder := []string{"gd-b", "gd-d", "gd-a"} // 90, then 72/small, then 72/large
	for i, id := range wantOrder {
		if eligible[i].BeadID != id {
			t.Errorf("eligible[%d] = %s, want %s (order=%v)", i, eligible[i].BeadID, id,
				[]string{eligible[0].BeadID, eligible[1].BeadID, eligible[2].BeadID})
		}
	}
	if len(parked) != 1 || parked[0].BeadID != "gd-c" {
		t.Fatalf("parked = %+v, want [gd-c]", parked)
	}
	if !parked[0].ScoreKnown || parked[0].Score != 30 {
		t.Errorf("parked score = %d known=%v, want 30/true", parked[0].Score, parked[0].ScoreKnown)
	}
	if parked[0].SizeKnown {
		t.Error("gd-c has no size field — SizeKnown must be false, not a fabricated 0")
	}
}

// TestPeelCatalogRowFromBead_Fields covers the per-bead extraction: risk from
// label, size from additions+deletions, coverage phrase, provenance fallback,
// and the requested marker.
func TestPeelCatalogRowFromBead_Fields(t *testing.T) {
	b := peelMRBead("gd-mr", "peelable slice",
		[]string{peelCatalogLabelEligible, "risk:high", peelRequestedMRLabel},
		map[string]string{
			"peel_score":        "58",
			"mr_url":            "https://dark.gitlab.test/grp/proj/-/merge_requests/42",
			"mr_additions":      "30",
			"mr_deletions":      "12",
			"mr_files_changed":  "3",
			"peel_tests_added":  "true",
			"peel_ci_result":    "success",
			"originating_ticket": "gd-tick",
		})
	row := peelCatalogRowFromBead(b)

	if !row.Eligible {
		t.Error("Eligible should be true (peel:eligible label present)")
	}
	if !row.Requested {
		t.Error("Requested should be true (peel:requested label present)")
	}
	if row.Score != 58 || !row.ScoreKnown {
		t.Errorf("Score = %d known=%v, want 58/true", row.Score, row.ScoreKnown)
	}
	if row.Risk != peelRiskHigh {
		t.Errorf("Risk = %q, want high", row.Risk)
	}
	if !row.SizeKnown || row.SizeLOC != 42 || row.SizeFiles != 3 {
		t.Errorf("Size = %d LOC / %d files known=%v, want 42/3/true", row.SizeLOC, row.SizeFiles, row.SizeKnown)
	}
	if !row.CoverageKnown || !strings.Contains(row.Coverage, "CI green") {
		t.Errorf("Coverage = %q, want a 'CI green' phrase", row.Coverage)
	}
	if row.MRShort != "grp/proj!42" {
		t.Errorf("MRShort = %q, want grp/proj!42", row.MRShort)
	}
	if row.OriginatingTicket != "gd-tick" {
		t.Errorf("OriginatingTicket = %q, want gd-tick", row.OriginatingTicket)
	}
	if !strings.Contains(row.Provenance, "gd-tick") {
		t.Errorf("Provenance = %q, want it to name the originating ticket", row.Provenance)
	}
}

// TestParsePeelScore covers absent/float/out-of-range handling.
func TestParsePeelScore(t *testing.T) {
	cases := []struct {
		raw       string
		want      int
		wantKnown bool
	}{
		{"", 0, false},
		{"garbage", 0, false},
		{"72", 72, true},
		{"72.9", 72, true},
		{"150", 100, true}, // clamped
		{"-5", 0, true},    // clamped
	}
	for _, c := range cases {
		got, known := parsePeelScore(c.raw)
		if got != c.want || known != c.wantKnown {
			t.Errorf("parsePeelScore(%q) = %d/%v, want %d/%v", c.raw, got, known, c.want, c.wantKnown)
		}
	}
}

// TestNormalizePeelRisk covers label-first resolution, field fallback, and
// medium synonyms.
func TestNormalizePeelRisk(t *testing.T) {
	if got := normalizePeelRisk([]string{"risk:low"}, nil); got != peelRiskLow {
		t.Errorf("label low = %q, want low", got)
	}
	if got := normalizePeelRisk([]string{"risk:med"}, nil); got != peelRiskMedium {
		t.Errorf("label med = %q, want medium", got)
	}
	if got := normalizePeelRisk(nil, map[string]string{"risk": "critical"}); got != peelRiskHigh {
		t.Errorf("field critical = %q, want high", got)
	}
	if got := normalizePeelRisk(nil, nil); got != peelRiskUnknown {
		t.Errorf("no signal = %q, want unknown", got)
	}
}

func newPeelTestServer(t *testing.T, daemon beadAPI) *Server {
	t.Helper()
	return NewServer(daemon, slog.Default(), "", "http://test", "", nil)
}

// TestHandlePeelCatalog_HappyPath drives the read handler end-to-end: eligible
// rows ranked, a parked section, and the request affordance rendered.
func TestHandlePeelCatalog_HappyPath(t *testing.T) {
	daemon := &fakeDaemon{
		mrResp: &beadsapi.ListBeadsResult{
			Beads: []*beadsapi.BeadDetail{
				peelMRBead("gd-hi", "high scorer", []string{peelCatalogLabelEligible, "risk:low"}, map[string]string{
					"peel_score": "88", "peel_size_loc": "55",
					"mr_url": "https://dark.test/g/p/-/merge_requests/7",
				}),
				peelMRBead("gd-lo", "parked one", []string{peelCatalogLabelParked, "risk:high"}, map[string]string{
					"peel_score": "20",
				}),
			},
		},
	}
	srv := newPeelTestServer(t, daemon)
	req := httptest.NewRequest(http.MethodGet, "/peel-catalog", nil)
	req = req.WithContext(oidcauth.ContextWithEmail(req.Context(), "op@acme.example"))
	rec := httptest.NewRecorder()
	srv.handlePeelCatalog(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Peel catalog",
		"high scorer",
		"gd-hi",
		"88",
		"risk: low",
		"request peel",           // affordance rendered for authenticated viewer
		"Parked slices",          // parked section present
		"parked one",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// The read handler must query merge-request beads via the any-of peel label
	// filter — never a broad unscoped scan.
	if daemon.listLastQ == nil {
		t.Fatal("expected a ListBeadsFiltered call")
	}
}

// TestHandlePeelCatalog_Empty renders the empty state without error.
func TestHandlePeelCatalog_Empty(t *testing.T) {
	daemon := &fakeDaemon{mrResp: &beadsapi.ListBeadsResult{}}
	srv := newPeelTestServer(t, daemon)
	req := httptest.NewRequest(http.MethodGet, "/peel-catalog", nil)
	rec := httptest.NewRecorder()
	srv.handlePeelCatalog(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "peel catalog is empty") {
		t.Errorf("expected empty-state copy; body=\n%s", rec.Body.String())
	}
}

// TestPeelCatalogRoutesRegistered drives the read + write routes through the
// real mux (RegisterRoutes + edgeAuth), proving GET /peel-catalog and POST
// /peel-catalog/{id}/request are wired and the {id} path value reaches the
// handler.
func TestPeelCatalogRoutesRegistered(t *testing.T) {
	mr := peelMRBead("gd-route-mr", "routable slice", []string{peelCatalogLabelEligible, "risk:low"}, nil)
	f := &fakeDaemon{
		beads:      map[string]*beadsapi.BeadDetail{"gd-route-mr": mr},
		deps:       map[string][]beadsapi.Dependency{},
		mrResp:     &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{mr}},
		createResp: "gd-route-req",
	}
	mux := mkServer(t, f)

	// GET is reachable.
	getReq := httptest.NewRequest(http.MethodGet, "/peel-catalog", nil)
	getReq.Header.Set("X-Auth-Request-Email", "op@acme.example")
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /peel-catalog status = %d, want 200", getRec.Code)
	}

	// POST reaches the handler with the {id} path value bound.
	postReq := httptest.NewRequest(http.MethodPost, "/peel-catalog/gd-route-mr/request", nil)
	postReq.Header.Set("X-Auth-Request-Email", "op@acme.example")
	postRec := httptest.NewRecorder()
	mux.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusOK {
		t.Fatalf("POST /peel-catalog/{id}/request status = %d, want 200; body=\n%s", postRec.Code, postRec.Body.String())
	}
	if f.createCallsOfType("task") != 1 {
		t.Errorf("task creates = %d, want 1 (path value bound + handler ran)", f.createCallsOfType("task"))
	}
}

// TestHandlePeelCatalog_UnauthenticatedHidesRequest confirms a signed-out
// viewer sees the menu but NOT the request affordance (CanRequest=false).
func TestHandlePeelCatalog_UnauthenticatedHidesRequest(t *testing.T) {
	daemon := &fakeDaemon{
		mrResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			peelMRBead("gd-x", "a slice", []string{peelCatalogLabelEligible, "risk:low"}, map[string]string{"peel_score": "70"}),
		}},
	}
	srv := newPeelTestServer(t, daemon)
	req := httptest.NewRequest(http.MethodGet, "/peel-catalog", nil) // no email in ctx
	rec := httptest.NewRecorder()
	srv.handlePeelCatalog(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, ">request peel<") {
		t.Error("request-peel button must not render for an unauthenticated viewer")
	}
	if !strings.Contains(body, "sign in to request") {
		t.Error("expected a sign-in hint in place of the request button")
	}
}

// TestHandlePeelCatalog_ViewerRoleHidesRequest confirms an authenticated
// viewer-role (non-operator) user does NOT get the request button — the POST is
// operatorOnly, so the UI must not offer a button that 403s (finding: dead
// button). A pass-through (no-roles) authenticated user still gets it.
func TestHandlePeelCatalog_ViewerRoleHidesRequest(t *testing.T) {
	daemon := &fakeDaemon{
		mrResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			peelMRBead("gd-vr", "a slice", []string{peelCatalogLabelEligible, "risk:low"}, map[string]string{"peel_score": "70"}),
		}},
	}
	srv := newPeelTestServer(t, daemon)
	req := httptest.NewRequest(http.MethodGet, "/peel-catalog", nil)
	ctx := oidcauth.ContextWithEmail(req.Context(), "viewer@acme.example")
	ctx = oidcauth.ContextWithRoles(ctx, []string{oidcauth.RoleViewer})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handlePeelCatalog(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, ">request peel<") {
		t.Error("request-peel button must not render for a viewer-role (non-operator) user")
	}
}
