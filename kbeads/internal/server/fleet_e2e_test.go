package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// newFleetTestServer creates a test server with AuthMiddleware configured for
// multi-fleet testing. It returns the handler, the mock store, and fleet config.
//
//	Primary (admin) token: "admin-token"
//	Fleet tokens: "alpha-token" → "alpha", "beta-token" → "beta"
func newFleetTestServer() (http.Handler, *mockStore, *FleetConfig) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	fc := NewFleetConfig("default", map[string]string{
		"alpha-token": "alpha",
		"beta-token":  "beta",
	})
	h := s.NewHTTPHandler(nil, withTestToken(fc, "admin-token"))
	return h, ms, fc
}

// fleetReq creates an HTTP request with the given fleet token.
func fleetReq(t *testing.T, method, path string, body any, token string) *http.Request {
	t.Helper()
	var req *http.Request
	var err error
	if body != nil {
		b, _ := json.Marshal(body)
		req, err = http.NewRequest(method, path, bytes.NewReader(b))
		if err != nil {
			t.Fatalf("creating request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, err = http.NewRequest(method, path, nil)
		if err != nil {
			t.Fatalf("creating request: %v", err)
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func doFleet(t *testing.T, h http.Handler, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := fleetReq(t, method, path, body, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- E2E Fleet Isolation Tests ---

// TestFleetE2E_BeadIsolation verifies that beads created in one fleet are
// invisible when listing from a different fleet's token.
// TestFleetE2E_FleetIsAuditAttributionOnly verifies the post-ka-0000000871
// model: writes still stamp `bead.fleet` for audit attribution (so the audit
// trail can identify which bridge/service made the write), but reads are no
// longer fleet-scoped. Every token sees every bead. The old per-token
// read-isolation gate (the CrossFleetRead opt-in) was a YAGNI multi-tenant
// trapdoor that bit ka-0000001804 (FSM-351) and ka-0000003707 (Coverage 2c).
func TestFleetE2E_FleetIsAuditAttributionOnly(t *testing.T) {
	h, _, _ := newFleetTestServer()

	// Create a bead under alpha — fleet tag still gets stamped for audit.
	rec := doFleet(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Alpha bead", "type": "task",
	}, "alpha-token")
	requireStatus(t, rec, 201)
	var alphaBead model.Bead
	decodeJSON(t, rec, &alphaBead)
	if alphaBead.Fleet != "alpha" {
		t.Fatalf("expected write to tag fleet=alpha (audit), got %q", alphaBead.Fleet)
	}

	// Create a bead under beta — same story.
	rec = doFleet(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Beta bead", "type": "task",
	}, "beta-token")
	requireStatus(t, rec, 201)
	var betaBead model.Bead
	decodeJSON(t, rec, &betaBead)
	if betaBead.Fleet != "beta" {
		t.Fatalf("expected write to tag fleet=beta (audit), got %q", betaBead.Fleet)
	}

	// Both alpha-token and beta-token MUST see both beads on list. The old
	// per-token read-isolation gate is gone.
	for _, tok := range []string{"alpha-token", "beta-token"} {
		rec = doFleet(t, h, "GET", "/v1/beads", nil, tok)
		requireStatus(t, rec, 200)
		var list struct {
			Beads []model.Bead `json:"beads"`
			Total int          `json:"total"`
		}
		decodeJSON(t, rec, &list)
		if list.Total != 2 {
			t.Fatalf("%s: expected 2 beads (cross-fleet read), got %d (%+v)", tok, list.Total, list.Beads)
		}
		seen := map[string]bool{}
		for _, b := range list.Beads {
			seen[b.Fleet] = true
		}
		if !seen["alpha"] || !seen["beta"] {
			t.Fatalf("%s: expected both alpha+beta beads visible, got fleets=%v", tok, seen)
		}
	}
}

// TestFleetE2E_CrossFleetQuery verifies that fleet=* returns beads from all fleets
// and is only available via admin token (X-Fleet override).
func TestFleetE2E_CrossFleetQuery(t *testing.T) {
	h, _, _ := newFleetTestServer()

	// Create beads in two different fleets.
	rec := doFleet(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Alpha task", "type": "task",
	}, "alpha-token")
	requireStatus(t, rec, 201)

	rec = doFleet(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Beta task", "type": "task",
	}, "beta-token")
	requireStatus(t, rec, 201)

	// Admin cross-fleet query with fleet=* query param.
	rec = doFleet(t, h, "GET", "/v1/beads?fleet=*", nil, "admin-token")
	requireStatus(t, rec, 200)
	var allList struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &allList)
	if allList.Total != 2 {
		t.Fatalf("cross-fleet: expected 2 beads, got %d", allList.Total)
	}
}

// TestFleetE2E_TokenScoping verifies that an invalid token is rejected.
func TestFleetE2E_TokenScoping(t *testing.T) {
	h, _, _ := newFleetTestServer()

	rec := doFleet(t, h, "GET", "/v1/beads", nil, "bad-token")
	requireStatus(t, rec, 401)
}

// TestFleetE2E_XFleetNonAdmin_WriteAuditTagIgnored verifies that non-admin
// tokens cannot use the X-Fleet header to spoof their write-side audit tag.
// (Reads are no longer fleet-filtered per epic ka-0000000871, so X-Fleet only
// has meaning on the write path.) The write must end up tagged with the
// caller's resolved fleet, not the X-Fleet override.
func TestFleetE2E_XFleetNonAdmin_WriteAuditTagIgnored(t *testing.T) {
	h, _, _ := newFleetTestServer()

	// Beta token tries to write a bead with X-Fleet=alpha. Override must be
	// ignored — the bead's audit tag must be beta (the caller's fleet).
	body, _ := json.Marshal(map[string]any{"title": "Spoofed write", "type": "task"})
	req := httptest.NewRequest("POST", "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer beta-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fleet", "alpha")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, 201)
	var written model.Bead
	decodeJSON(t, rec, &written)
	if written.Fleet != "beta" {
		t.Fatalf("non-admin X-Fleet override on write must be ignored: expected fleet=beta, got %q", written.Fleet)
	}
}

// TestFleetE2E_BeadTransfer verifies that transferring a bead moves its
// audit-attribution fleet tag from one fleet to another. Reads are no longer
// fleet-filtered per epic ka-0000000871, so the assertion is on the `fleet`
// field of the bead itself, not on per-token visibility.
func TestFleetE2E_BeadTransfer(t *testing.T) {
	// The fleet test server plus an OIDC verifier (every other bearer is an
	// OIDC admin), so the transfer can be made by the only caller allowed to.
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	h := s.NewHTTPHandler(&bearerSubjectVerifier{}, withTestToken(NewFleetConfig("default", map[string]string{
		"alpha-token": "alpha",
		"beta-token":  "beta",
	}), "admin-token"))

	// Create a bead in alpha.
	rec := doFleet(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Transferable", "type": "task",
	}, "alpha-token")
	requireStatus(t, rec, 201)
	var bead model.Bead
	decodeJSON(t, rec, &bead)
	if bead.Fleet != "alpha" {
		t.Fatalf("create: expected fleet=alpha, got %q", bead.Fleet)
	}

	// Transfer to beta: only an OIDC admin may (gd-i6GZX5jzHM); the primary
	// static token and fleet tokens are refused.
	for _, token := range []string{"admin-token", "alpha-token"} {
		rec = doFleet(t, h, "POST", "/v1/beads/"+bead.ID+"/transfer",
			map[string]string{"to_fleet": "beta"}, token)
		requireStatus(t, rec, http.StatusForbidden)
	}
	rec = doFleet(t, h, "POST", "/v1/beads/"+bead.ID+"/transfer",
		map[string]string{"to_fleet": "beta"}, "oidc-admin@example.com")
	requireStatus(t, rec, 200)
	var transferred model.Bead
	decodeJSON(t, rec, &transferred)
	if transferred.Fleet != "beta" {
		t.Fatalf("transfer: expected fleet=beta, got %q", transferred.Fleet)
	}

	// Re-fetch by ID and confirm the persisted tag is beta.
	rec = doFleet(t, h, "GET", "/v1/beads/"+bead.ID, nil, "admin-token")
	requireStatus(t, rec, 200)
	var fetched model.Bead
	decodeJSON(t, rec, &fetched)
	if fetched.Fleet != "beta" {
		t.Fatalf("post-transfer GET: expected fleet=beta, got %q", fetched.Fleet)
	}
}

// TestFleetE2E_AdminFleetQueryFilter verifies that the explicit `?fleet=<name>`
// query param still filters list results to a specific fleet's beads. (Used by
// audit / rotation tooling that wants to see one fleet's history at a time.)
// Per epic ka-0000000871, the default read filter is "all fleets"; the explicit
// param is the only way to narrow.
func TestFleetE2E_AdminFleetQueryFilter(t *testing.T) {
	h, _, _ := newFleetTestServer()

	// Create beads in alpha and beta.
	rec := doFleet(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Alpha only", "type": "task",
	}, "alpha-token")
	requireStatus(t, rec, 201)

	rec = doFleet(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Beta only", "type": "task",
	}, "beta-token")
	requireStatus(t, rec, 201)

	// Explicit ?fleet=alpha query param narrows the list to alpha beads.
	// Works for any caller (admin or otherwise) — it's a regular query
	// filter, not a security boundary.
	rec = doFleet(t, h, "GET", "/v1/beads?fleet=alpha", nil, "admin-token")
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("?fleet=alpha: expected 1 bead, got %d", result.Total)
	}
	if result.Beads[0].Title != "Alpha only" {
		t.Fatalf("expected 'Alpha only', got %q", result.Beads[0].Title)
	}
}

// TestFleetE2E_GetBeadCrossFleet verifies that getting a specific bead by ID
// works regardless of fleet context (GET /v1/beads/{id} is not fleet-filtered).
func TestFleetE2E_GetBeadCrossFleet(t *testing.T) {
	h, _, _ := newFleetTestServer()

	// Create a bead in alpha.
	rec := doFleet(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Alpha specific", "type": "task",
	}, "alpha-token")
	requireStatus(t, rec, 201)
	var bead model.Bead
	decodeJSON(t, rec, &bead)

	// Beta token can GET the bead by ID (GetBead is not fleet-filtered).
	rec = doFleet(t, h, "GET", "/v1/beads/"+bead.ID, nil, "beta-token")
	requireStatus(t, rec, 200)
	var fetched model.Bead
	decodeJSON(t, rec, &fetched)
	if fetched.ID != bead.ID {
		t.Fatalf("expected bead %s, got %s", bead.ID, fetched.ID)
	}
}

// TestFleetE2E_HealthExempt verifies that the health endpoint works without auth.
func TestFleetE2E_HealthExempt(t *testing.T) {
	h, _, _ := newFleetTestServer()

	req := httptest.NewRequest("GET", "/v1/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, 200)
}

// TestFleetE2E_BridgeWriteStillAuditTagsCorrectly verifies that a bridge
// token's writes still stamp `bead.fleet` with the bridge fleet name (audit
// attribution), even though read isolation is gone. Reads are unconstrained
// per epic ka-0000000871 / ka-0000003285; writes must still tag the source.
func TestFleetE2E_BridgeWriteStillAuditTagsCorrectly(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	fc := NewFleetConfig("default", map[string]string{
		"alpha-token": "alpha",
	})
	fc.RegisterBridgeToken("bridge-token", "bridge-fleet", "bridge:test")
	h := s.NewHTTPHandler(nil, withTestToken(fc, "admin-token"))

	// Writes through the bridge token still tag with bridge-fleet.
	rec := doFleet(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Bridge write", "type": "task",
	}, "bridge-token")
	requireStatus(t, rec, 201)
	var written model.Bead
	decodeJSON(t, rec, &written)
	if written.Fleet != "bridge-fleet" {
		t.Fatalf("bridge token write must tag fleet=bridge-fleet (audit), got %q", written.Fleet)
	}

	// Bridge token reads across all fleets (every token now does — the old
	// CrossFleetRead gate was removed). Create an alpha bead and confirm the
	// bridge token sees it on list.
	rec = doFleet(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Alpha work", "type": "task",
	}, "alpha-token")
	requireStatus(t, rec, 201)

	rec = doFleet(t, h, "GET", "/v1/beads", nil, "bridge-token")
	requireStatus(t, rec, 200)
	var bridgeList struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &bridgeList)
	if bridgeList.Total != 2 {
		t.Fatalf("bridge token list across fleets: expected 2 beads (1 alpha + 1 bridge), got %d", bridgeList.Total)
	}
}
