package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// meTaintsReq builds a GET/POST request against path with the given viewer
// email already promoted into the verified-identity context slot — the same
// seam production auth (oidcauth.ContextWithEmail) populates, bypassing the
// X-Auth-Request-Email/edgeAuth header dance since these tests call the
// handlers directly rather than through mkServer's mux.
func meTaintsReq(method, path, email string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if email != "" {
		req = req.WithContext(oidcauth.ContextWithEmail(req.Context(), email))
	}
	return req
}

func TestHandleMeTaints_Unauthenticated_RedirectsToLogin(t *testing.T) {
	srv := NewServer(&fakeDaemon{}, slog.Default(), "", "http://test", "", nil)
	req := meTaintsReq(http.MethodGet, "/me/taints", "")
	rec := httptest.NewRecorder()
	srv.handleMeTaints(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login?next=") {
		t.Errorf("Location = %q, want a /login?next=... redirect", loc)
	}
	if !strings.Contains(loc, "%2Fme%2Ftaints") && !strings.Contains(loc, "/me/taints") {
		t.Errorf("Location = %q, want it to preserve the original path", loc)
	}
}

// TestHandleMeTaints_OwnsAndCreatedUnion asserts myTaintedBeads unions the
// owns facet (Owner filter) and the created facet (OperatedByAliases
// filter), deduping a bead present in both.
func TestHandleMeTaints_OwnsAndCreatedUnion(t *testing.T) {
	owned := &beadsapi.BeadDetail{ID: "gd-owned", Title: "Owned bead", Type: "task", Status: "open", Owner: "oidc:dev@acme.example"}
	shared := &beadsapi.BeadDetail{ID: "gd-shared", Title: "Owned and created", Type: "task", Status: "open", Owner: "oidc:dev@acme.example"}
	created := &beadsapi.BeadDetail{ID: "gd-created", Title: "Created bead", Type: "task", Status: "open"}

	f := &fakeDaemon{
		meTaintsOwnResp:     &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{owned, shared}},
		meTaintsCreatedResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{shared, created}},
		beadTaints: map[string][]beadsapi.BeadTaint{
			"gd-owned":   {{TaintName: "pii"}},
			"gd-shared":  {{TaintName: "pii"}},
			"gd-created": {{TaintName: "pii"}},
		},
	}
	srv := NewServer(f, slog.Default(), "", "http://test", "", nil)
	req := meTaintsReq(http.MethodGet, "/me/taints", "dev@acme.example")
	rec := httptest.NewRecorder()
	srv.handleMeTaints(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"gd-owned", "gd-shared", "gd-created", "pii"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected body to contain %q\n--- body ---\n%s", want, body)
		}
	}
	// gd-shared must render as exactly one row despite being returned by both
	// facets — count the href attribute specifically since .Href is also
	// emitted a second time per row as the hx-get attribute.
	if n := strings.Count(body, `href="/gd-shared"`); n != 1 {
		t.Errorf(`href="/gd-shared" appeared %d times in body, want exactly 1 (dedup failure)`+"\n%s", n, body)
	}

	// Both facets were queried with the canonical alias.
	wantAlias := canonicalOIDC("dev@acme.example")
	sawOwns, sawCreated := false, false
	for _, q := range f.listAllQ {
		if q.Owner == wantAlias {
			sawOwns = true
		}
		if len(q.OperatedByAliases) == 1 && q.OperatedByAliases[0] == wantAlias {
			sawCreated = true
		}
	}
	if !sawOwns {
		t.Errorf("expected an owns-facet query (Owner=%q); queries=%+v", wantAlias, f.listAllQ)
	}
	if !sawCreated {
		t.Errorf("expected a created-facet query (OperatedByAliases=[%q]); queries=%+v", wantAlias, f.listAllQ)
	}
}

// TestHandleMeTaints_GroupsByTaint asserts beads carrying multiple taints
// appear under each taint's group, and the owner-team accent is resolved
// from the taint registry.
func TestHandleMeTaints_GroupsByTaint(t *testing.T) {
	b1 := &beadsapi.BeadDetail{ID: "gd-b1", Title: "Bead one", Type: "task", Status: "open", Owner: "oidc:dev@acme.example"}
	b2 := &beadsapi.BeadDetail{ID: "gd-b2", Title: "Bead two", Type: "task", Status: "in_progress", Owner: "oidc:dev@acme.example"}

	f := &fakeDaemon{
		meTaintsOwnResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{b1, b2}},
		beadTaints: map[string][]beadsapi.BeadTaint{
			"gd-b1": {{TaintName: "pii"}, {TaintName: "secret"}},
			"gd-b2": {{TaintName: "pii"}},
		},
		taintRegistryResp: []beadsapi.TaintRegistryEntry{
			{Name: "pii", OwnerTeam: "privacy"},
			{Name: "secret", OwnerTeam: "security"},
		},
	}
	srv := NewServer(f, slog.Default(), "", "http://test", "", nil)
	req := meTaintsReq(http.MethodGet, "/me/taints", "dev@acme.example")
	rec := httptest.NewRecorder()
	srv.handleMeTaints(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"pii", "secret", "privacy", "security", "gd-b1", "gd-b2"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected body to contain %q\n--- body ---\n%s", want, body)
		}
	}
	// gd-b1 must render under BOTH the pii and secret groups. Count the href
	// attribute specifically since .Href is also emitted a second time per row
	// as the hx-get attribute (a bare "gd-b1" substring count would double-count).
	if n := strings.Count(body, `href="/gd-b1"`); n != 2 {
		t.Errorf(`href="/gd-b1" appeared %d times, want 2 (once per taint group)`+"\n%s", n, body)
	}
}

// TestHandleMeTaints_OwnsFacetError fails the page — owns is the
// authoritative facet.
func TestHandleMeTaints_OwnsFacetError(t *testing.T) {
	f := &fakeDaemon{meTaintsOwnErr: errors.New("daemon down")}
	srv := NewServer(f, slog.Default(), "", "http://test", "", nil)
	req := meTaintsReq(http.MethodGet, "/me/taints", "dev@acme.example")
	rec := httptest.NewRecorder()
	srv.handleMeTaints(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (inline banner)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Failed to load your beads.") {
		t.Errorf("error banner missing; body=\n%s", rec.Body.String())
	}
}

// TestHandleMeTaints_CreatedFacetErrorDegrades asserts a created-facet error
// narrows the view (owns-only) rather than failing the page.
func TestHandleMeTaints_CreatedFacetErrorDegrades(t *testing.T) {
	owned := &beadsapi.BeadDetail{ID: "gd-owned", Title: "Owned bead", Type: "task", Status: "open", Owner: "oidc:dev@acme.example"}
	f := &fakeDaemon{
		meTaintsOwnResp:    &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{owned}},
		meTaintsCreatedErr: errors.New("created facet down"),
		beadTaints:         map[string][]beadsapi.BeadTaint{"gd-owned": {{TaintName: "pii"}}},
	}
	srv := NewServer(f, slog.Default(), "", "http://test", "", nil)
	req := meTaintsReq(http.MethodGet, "/me/taints", "dev@acme.example")
	rec := httptest.NewRecorder()
	srv.handleMeTaints(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gd-owned") {
		t.Errorf("expected the owns-facet bead to still render; body=\n%s", rec.Body.String())
	}
}

// TestHandleMeTaints_BeadTaintsErrorDegrades asserts a per-bead
// GetBeadTaints failure skips that bead rather than failing the page.
func TestHandleMeTaints_BeadTaintsErrorDegrades(t *testing.T) {
	owned := &beadsapi.BeadDetail{ID: "gd-owned", Title: "Owned bead", Type: "task", Status: "open", Owner: "oidc:dev@acme.example"}
	f := &fakeDaemon{
		meTaintsOwnResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{owned}},
		beadTaintsErr:   errors.New("taints lookup down"),
	}
	srv := NewServer(f, slog.Default(), "", "http://test", "", nil)
	req := meTaintsReq(http.MethodGet, "/me/taints", "dev@acme.example")
	rec := httptest.NewRecorder()
	srv.handleMeTaints(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "None of your beads carry a taint.") {
		t.Errorf("expected the empty-state copy when taint lookups fail; body=\n%s", rec.Body.String())
	}
}

// TestHandleMeTaints_EmptyState asserts the empty-state copy renders when the
// viewer owns/created no tainted beads.
func TestHandleMeTaints_EmptyState(t *testing.T) {
	f := &fakeDaemon{}
	srv := NewServer(f, slog.Default(), "", "http://test", "", nil)
	req := meTaintsReq(http.MethodGet, "/me/taints", "dev@acme.example")
	rec := httptest.NewRecorder()
	srv.handleMeTaints(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "None of your beads carry a taint.") {
		t.Errorf("empty-state copy missing; body=\n%s", rec.Body.String())
	}
}

// TestHandleMeTaints_TaintLookupTruncatedBanner asserts the page warns the
// viewer when their bead count exceeds meTaintsTaintLookupLimit (operator
// review 2026-09-23: the per-bead taint lookup is bounded, so beads past the
// limit silently get no taint lookup — the page must say so rather than just
// quietly omitting them).
func TestHandleMeTaints_TaintLookupTruncatedBanner(t *testing.T) {
	beads := make([]*beadsapi.BeadDetail, meTaintsTaintLookupLimit+1)
	for i := range beads {
		beads[i] = &beadsapi.BeadDetail{
			ID:     fmt.Sprintf("gd-b%d", i),
			Title:  "Bead",
			Type:   "task",
			Status: "open",
			Owner:  "oidc:dev@acme.example",
		}
	}
	f := &fakeDaemon{meTaintsOwnResp: &beadsapi.ListBeadsResult{Beads: beads}}
	srv := NewServer(f, slog.Default(), "", "http://test", "", nil)
	req := meTaintsReq(http.MethodGet, "/me/taints", "dev@acme.example")
	rec := httptest.NewRecorder()
	srv.handleMeTaints(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "most recently updated beads only") {
		t.Errorf("expected the truncation banner; body=\n%s", rec.Body.String())
	}
}

// TestHandleMeTaintsScrubAll_Unauthenticated_RedirectsToLogin mirrors the GET
// handler's auth gate on the POST action.
func TestHandleMeTaintsScrubAll_Unauthenticated_RedirectsToLogin(t *testing.T) {
	srv := NewServer(&fakeDaemon{}, slog.Default(), "", "http://test", "", nil)
	req := meTaintsReq(http.MethodPost, "/me/taints/scrub", "")
	rec := httptest.NewRecorder()
	srv.handleMeTaintsScrubAll(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
}

// TestHandleMeTaintsScrubAll_FilesOnePerTaint is the direct regression guard
// for the ADR §9.1 acceptance criterion: "request detaint for all of mine"
// files exactly one scrub request per distinct taint shown, each scoped to
// exactly that viewer's own beads carrying it.
func TestHandleMeTaintsScrubAll_FilesOnePerTaint(t *testing.T) {
	b1 := &beadsapi.BeadDetail{ID: "gd-b1", Title: "Bead one", Type: "task", Status: "open", Owner: "oidc:dev@acme.example"}
	b2 := &beadsapi.BeadDetail{ID: "gd-b2", Title: "Bead two", Type: "task", Status: "open", Owner: "oidc:dev@acme.example"}
	b3 := &beadsapi.BeadDetail{ID: "gd-b3", Title: "Bead three", Type: "task", Status: "open", Owner: "oidc:dev@acme.example"}

	f := &fakeDaemon{
		meTaintsOwnResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{b1, b2, b3}},
		beadTaints: map[string][]beadsapi.BeadTaint{
			"gd-b1": {{TaintName: "pii"}},
			"gd-b2": {{TaintName: "pii"}},
			"gd-b3": {{TaintName: "secret"}},
		},
	}
	srv := NewServer(f, slog.Default(), "", "http://test", "", nil)
	req := meTaintsReq(http.MethodPost, "/me/taints/scrub", "dev@acme.example")
	rec := httptest.NewRecorder()
	srv.handleMeTaintsScrubAll(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	if len(f.scrubRequests) != 2 {
		t.Fatalf("filed %d scrub requests, want exactly 2 (one per distinct taint); got %+v", len(f.scrubRequests), f.scrubRequests)
	}
	byTaint := map[string]beadsapi.CreateScrubRequestInput{}
	for _, r := range f.scrubRequests {
		byTaint[r.Taint] = r
	}
	pii, ok := byTaint["pii"]
	if !ok {
		t.Fatalf("no scrub request filed for taint 'pii'; got %+v", f.scrubRequests)
	}
	if pii.Kind != "detaint" {
		t.Errorf("pii request Kind = %q, want detaint", pii.Kind)
	}
	if got := append([]string{}, pii.BeadIDs...); !equalUnordered(got, []string{"gd-b1", "gd-b2"}) {
		t.Errorf("pii request BeadIDs = %v, want [gd-b1 gd-b2]", got)
	}
	secret, ok := byTaint["secret"]
	if !ok {
		t.Fatalf("no scrub request filed for taint 'secret'; got %+v", f.scrubRequests)
	}
	if !equalUnordered(secret.BeadIDs, []string{"gd-b3"}) {
		t.Errorf("secret request BeadIDs = %v, want [gd-b3]", secret.BeadIDs)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "detaint requested for 2 beads") {
		t.Errorf("expected pii success banner (2 beads); body=\n%s", body)
	}
	if !strings.Contains(body, "detaint requested for 1 bead") {
		t.Errorf("expected secret success banner (1 bead); body=\n%s", body)
	}
}

// TestHandleMeTaintsScrubAll_PartialFailureDoesNotBlockOthers asserts a
// failure filing one taint's scrub request does not prevent the others from
// being filed, and surfaces per-taint in the result banner.
func TestHandleMeTaintsScrubAll_PartialFailureDoesNotBlockOthers(t *testing.T) {
	b1 := &beadsapi.BeadDetail{ID: "gd-b1", Title: "Bead one", Type: "task", Status: "open", Owner: "oidc:dev@acme.example"}
	b2 := &beadsapi.BeadDetail{ID: "gd-b2", Title: "Bead two", Type: "task", Status: "open", Owner: "oidc:dev@acme.example"}

	f := &fakeDaemon{
		meTaintsOwnResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{b1, b2}},
		beadTaints: map[string][]beadsapi.BeadTaint{
			"gd-b1": {{TaintName: "pii"}},
			"gd-b2": {{TaintName: "secret"}},
		},
		scrubRequestErrByTaint: map[string]error{
			"pii": errors.New("policy rejected"),
		},
	}
	srv := NewServer(f, slog.Default(), "", "http://test", "", nil)
	req := meTaintsReq(http.MethodPost, "/me/taints/scrub", "dev@acme.example")
	rec := httptest.NewRecorder()
	srv.handleMeTaintsScrubAll(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	if len(f.scrubRequests) != 2 {
		t.Fatalf("filed %d scrub requests, want 2 (both attempted despite one failing); got %+v", len(f.scrubRequests), f.scrubRequests)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "policy rejected") {
		t.Errorf("expected the pii failure reason in the result banner; body=\n%s", body)
	}
	if !strings.Contains(body, "detaint requested for 1 bead") {
		t.Errorf("expected the secret success banner despite the pii failure; body=\n%s", body)
	}
}

// TestMyTaintedBeads_DropsOwnerMismatch is the regression guard for the
// rollout-skew fail-open the operator flagged on PR #141 (2026-09-23): ?owner=
// is a query param this daemon just learned, so a pod still running the prior
// build ignores it and could return every bead visible to the caller instead
// of just the viewer's own. myTaintedBeads must re-check Owner client-side and
// drop any mismatch rather than trust the (possibly-skewed) server response.
func TestMyTaintedBeads_DropsOwnerMismatch(t *testing.T) {
	mine := &beadsapi.BeadDetail{ID: "gd-mine", Title: "Mine", Type: "task", Status: "open", Owner: "oidc:dev@acme.example"}
	notMine := &beadsapi.BeadDetail{ID: "gd-not-mine", Title: "Someone else's", Type: "task", Status: "open", Owner: "oidc:other@acme.example"}

	f := &fakeDaemon{
		meTaintsOwnResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{mine, notMine}},
		beadTaints: map[string][]beadsapi.BeadTaint{
			"gd-mine":     {{TaintName: "pii"}},
			"gd-not-mine": {{TaintName: "pii"}},
		},
	}
	srv := NewServer(f, slog.Default(), "", "http://test", "", nil)
	req := meTaintsReq(http.MethodGet, "/me/taints", "dev@acme.example")
	rec := httptest.NewRecorder()
	srv.handleMeTaints(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "gd-mine") {
		t.Errorf("expected the viewer's own bead to render; body=\n%s", body)
	}
	if strings.Contains(body, "gd-not-mine") {
		t.Errorf("bead owned by a different alias must be dropped, not rendered; body=\n%s", body)
	}
}

// TestHandleMeTaintsScrubAll_NonOperatorForbidden asserts POST
// /me/taints/scrub is role-gated (operator review 2026-09-23, PR #141 item
// 3): a viewer-role caller is rejected before the handler files any scrub
// request, even though the daemon separately requires a grant + approval per
// request.
func TestHandleMeTaintsScrubAll_NonOperatorForbidden(t *testing.T) {
	f := &fakeDaemon{}
	mux := mkServer(t, f)

	req := httptest.NewRequest(http.MethodPost, "/me/taints/scrub", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(f.scrubRequests) != 0 {
		t.Error("viewer-role POST must not file any scrub request")
	}
}

// equalUnordered reports whether a and b contain the same elements,
// disregarding order.
func equalUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	count := map[string]int{}
	for _, v := range a {
		count[v]++
	}
	for _, v := range b {
		count[v]--
	}
	for _, n := range count {
		if n != 0 {
			return false
		}
	}
	return true
}
