package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// newE2EServer creates a BeadsServer with full AuthMiddleware wrapping and
// starts an httptest.Server. adminToken is a privileged fleet token (it also
// registers further tokens, standing in for the token-mint machine principal;
// see withMinterRoutes). Requests to the returned server go through the
// complete auth pipeline: AuthMiddleware → requireRole → handler.
func newE2EServer(t *testing.T, adminToken string, fc *FleetConfig) (*httptest.Server, *BeadsServer, *mockStore) {
	t.Helper()
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.SetFleetAuth(fc)
	handler := withMinterRoutes(srv, adminToken, srv.NewHTTPHandler(nil, withTestToken(fc, adminToken)))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, srv, ms
}

// e2eRequest is a helper to issue an HTTP request with an optional JSON body
// and Bearer token against a running httptest.Server.
func e2eRequest(t *testing.T, ts *httptest.Server, method, path, token string, body any) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, bodyReader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Provide an operator so post-R2 fail-closed handlers don't 400 the
	// admin/static-token paths. Fleet tokens self-resolve and ignore this.
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// decodeBody reads and decodes an *http.Response body into the given pointer.
func decodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// TestProjectScopedFleetTokenE2E is the full integration test for project-scoped
// fleet tokens flowing through AuthMiddleware. It covers: register → auth →
// create (allowed/denied) → get (allowed/denied) → list (filtered) → revoke → 401.
func TestProjectScopedFleetTokenE2E(t *testing.T) {
	const (
		adminToken   = "admin-e2e-secret"
		projectToken = "proj-scope-tok-e2e"
		fleetName    = "proj-e2e-fleet"
		projectName  = "alpha"
	)

	fc := NewFleetConfig("gardenio", nil)
	ts, _, ms := newE2EServer(t, adminToken, fc)

	// ----------------------------------------------------------------
	// Phase 1: Register a project-scoped fleet token via the admin API.
	// ----------------------------------------------------------------
	t.Run("RegisterProjectScopedToken", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodPost, "/v1/fleet-tokens", adminToken,
			registerBridgeTokenRequest{
				Token:   projectToken,
				Fleet:   fleetName,
				Project: projectName,
			})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201, got %d; body: %s", resp.StatusCode, body)
		}

		// Verify in-memory registration.
		if got := fc.ResolveFleet(projectToken); got != fleetName {
			t.Fatalf("fleet = %q, want %q", got, fleetName)
		}
		if got := fc.ResolveProject(projectToken); got != projectName {
			t.Fatalf("project = %q, want %q", got, projectName)
		}
	})

	// ----------------------------------------------------------------
	// Phase 2: Authenticated bead creation — correct project label succeeds.
	// ----------------------------------------------------------------
	var createdBeadID string
	t.Run("CreateBead_CorrectProject_201", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodPost, "/v1/beads", projectToken,
			createBeadInput{
				Title:  "Alpha task",
				Type:   "task",
				Labels: []string{"project:" + projectName},
			})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201, got %d; body: %s", resp.StatusCode, body)
		}
		var bead model.Bead
		decodeBody(t, resp, &bead)
		if bead.ID == "" {
			t.Fatal("expected non-empty bead ID")
		}
		createdBeadID = bead.ID
	})

	// ----------------------------------------------------------------
	// Phase 3: Authenticated bead creation — wrong project label gets 403.
	// ----------------------------------------------------------------
	t.Run("CreateBead_WrongProject_403", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodPost, "/v1/beads", projectToken,
			createBeadInput{
				Title:  "Wrong project bead",
				Type:   "task",
				Labels: []string{"project:beta"},
			})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 403, got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 4: Create a bead in a different project (via admin) for contrast.
	// ----------------------------------------------------------------
	var otherBeadID string
	t.Run("AdminCreates_BetaBead", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodPost, "/v1/beads", adminToken,
			createBeadInput{
				Title:  "Beta task",
				Type:   "task",
				Labels: []string{"project:beta"},
			})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201, got %d; body: %s", resp.StatusCode, body)
		}
		var bead model.Bead
		decodeBody(t, resp, &bead)
		otherBeadID = bead.ID
	})

	// ----------------------------------------------------------------
	// Phase 5: GET bead — project-scoped token can read its own project.
	// ----------------------------------------------------------------
	t.Run("GetBead_OwnProject_200", func(t *testing.T) {
		if createdBeadID == "" {
			t.Skip("no bead created in phase 2")
		}
		resp := e2eRequest(t, ts, http.MethodGet, "/v1/beads/"+createdBeadID, projectToken, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 6: GET bead — project-scoped token cannot read other projects.
	// ----------------------------------------------------------------
	t.Run("GetBead_OtherProject_404", func(t *testing.T) {
		if otherBeadID == "" {
			t.Skip("no other bead created in phase 4")
		}
		resp := e2eRequest(t, ts, http.MethodGet, "/v1/beads/"+otherBeadID, projectToken, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 404 (as missing), got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 7: LIST beads — project-scoped token only sees its project's beads.
	// ----------------------------------------------------------------
	t.Run("ListBeads_FilteredByProject", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodGet, "/v1/beads", projectToken, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d; body: %s", resp.StatusCode, body)
		}
		var list struct {
			Beads []*model.Bead `json:"beads"`
			Total int           `json:"total"`
		}
		decodeBody(t, resp, &list)

		for _, b := range list.Beads {
			labels, _ := ms.GetLabels(context.Background(), b.ID)
			if !containsLabel(labels, "project:"+projectName) {
				t.Errorf("bead %s should not be visible — labels: %v", b.ID, labels)
			}
		}
		if len(list.Beads) == 0 {
			t.Fatal("expected at least one bead in filtered list")
		}
	})

	// ----------------------------------------------------------------
	// Phase 8: UPDATE bead — project-scoped token can update own project bead.
	// ----------------------------------------------------------------
	t.Run("UpdateBead_OwnProject_200", func(t *testing.T) {
		if createdBeadID == "" {
			t.Skip("no bead created in phase 2")
		}
		newTitle := "Alpha task (updated)"
		resp := e2eRequest(t, ts, http.MethodPatch, "/v1/beads/"+createdBeadID, projectToken,
			updateBeadInput{Title: &newTitle})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 9: UPDATE bead — project-scoped token cannot update other project.
	// ----------------------------------------------------------------
	t.Run("UpdateBead_OtherProject_404", func(t *testing.T) {
		if otherBeadID == "" {
			t.Skip("no other bead created in phase 4")
		}
		newTitle := "Hacked"
		resp := e2eRequest(t, ts, http.MethodPatch, "/v1/beads/"+otherBeadID, projectToken,
			updateBeadInput{Title: &newTitle})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 404 (as missing), got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 10: CLOSE bead — project-scoped token cannot close other project.
	// ----------------------------------------------------------------
	t.Run("CloseBead_OtherProject_404", func(t *testing.T) {
		if otherBeadID == "" {
			t.Skip("no other bead created in phase 4")
		}
		resp := e2eRequest(t, ts, http.MethodPost, "/v1/beads/"+otherBeadID+"/close", projectToken, map[string]string{})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 404 (as missing), got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 11: Fleet token appears in list (admin only).
	// ----------------------------------------------------------------
	t.Run("ListFleetTokens_ShowsFleet", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodGet, "/v1/fleet-tokens", adminToken, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		var list struct {
			Fleets []string `json:"fleets"`
		}
		decodeBody(t, resp, &list)
		found := false
		for _, f := range list.Fleets {
			if f == fleetName {
				found = true
			}
		}
		if !found {
			t.Fatalf("fleet %q not found in list: %v", fleetName, list.Fleets)
		}
	})

	// ----------------------------------------------------------------
	// Phase 12: Fleet token management blocked for non-admin.
	// ----------------------------------------------------------------
	t.Run("ListFleetTokens_NonAdmin_403", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodGet, "/v1/fleet-tokens", projectToken, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", resp.StatusCode)
		}
	})

	// ----------------------------------------------------------------
	// Phase 13: Revoke the project-scoped token.
	// ----------------------------------------------------------------
	t.Run("RevokeToken", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodDelete, "/v1/fleet-tokens/"+fleetName, adminToken, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 204, got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 14: After revocation, the token returns 401 (unauthenticated).
	// ----------------------------------------------------------------
	t.Run("RevokedToken_401", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodGet, "/v1/beads", projectToken, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 401, got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 15: Register replacement token for the same fleet.
	// ----------------------------------------------------------------
	const replacementToken = "proj-scope-tok-v2"
	t.Run("RegisterReplacementToken", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodPost, "/v1/fleet-tokens", adminToken,
			registerBridgeTokenRequest{
				Token:   replacementToken,
				Fleet:   fleetName,
				Project: projectName,
			})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201, got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 16: Replacement token works for same project.
	// ----------------------------------------------------------------
	t.Run("ReplacementToken_CreateBead_201", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodPost, "/v1/beads", replacementToken,
			createBeadInput{
				Title:  "Alpha task v2",
				Type:   "task",
				Labels: []string{"project:" + projectName},
			})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201, got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 17: Replacement token still enforces project scope.
	// ----------------------------------------------------------------
	t.Run("ReplacementToken_WrongProject_403", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodPost, "/v1/beads", replacementToken,
			createBeadInput{
				Title:  "Should fail",
				Type:   "task",
				Labels: []string{"project:gamma"},
			})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 403, got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 18: Old token still rejected after replacement registered.
	// ----------------------------------------------------------------
	t.Run("OldToken_StillRejected", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodGet, "/v1/beads", projectToken, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 401, got %d; body: %s", resp.StatusCode, body)
		}
	})
}

// TestUnscopedFleetTokenE2E verifies that a fleet token without project scope
// has unrestricted access to all beads through the full middleware stack.
func TestUnscopedFleetTokenE2E(t *testing.T) {
	const (
		adminToken = "admin-unscoped"
		fleetToken = "fleet-unscoped-tok"
		fleetName  = "general-fleet"
	)

	fc := NewFleetConfig("gardenio", nil)
	ts, _, _ := newE2EServer(t, adminToken, fc)

	// Register an unscoped fleet token.
	resp := e2eRequest(t, ts, http.MethodPost, "/v1/fleet-tokens", adminToken,
		registerBridgeTokenRequest{Token: fleetToken, Fleet: fleetName})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d", resp.StatusCode)
	}

	// Create beads in different projects using the unscoped token.
	for _, proj := range []string{"project:x", "project:y"} {
		resp := e2eRequest(t, ts, http.MethodPost, "/v1/beads", fleetToken,
			createBeadInput{
				Title:  "Bead in " + proj,
				Type:   "task",
				Labels: []string{proj},
			})
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create bead with label %s: expected 201, got %d", proj, resp.StatusCode)
		}
	}

	// List beads — should see both.
	resp = e2eRequest(t, ts, http.MethodGet, "/v1/beads", fleetToken, nil)
	defer resp.Body.Close()
	var list struct {
		Beads []*model.Bead `json:"beads"`
	}
	decodeBody(t, resp, &list)
	if len(list.Beads) < 2 {
		t.Fatalf("expected at least 2 beads, got %d", len(list.Beads))
	}
}

// TestNoTokenE2E verifies that requests with no token get 401 when auth is enabled.
func TestNoTokenE2E(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	ts, _, _ := newE2EServer(t, "admin-tok", fc)

	resp := e2eRequest(t, ts, http.MethodGet, "/v1/beads", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for no token, got %d", resp.StatusCode)
	}
}

// TestHealthExemptFromAuth verifies that /v1/health bypasses auth.
func TestHealthExemptFromAuth(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	ts, _, _ := newE2EServer(t, "admin-tok", fc)

	resp := e2eRequest(t, ts, http.MethodGet, "/v1/health", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for health (no auth), got %d", resp.StatusCode)
	}
}

// TestTokenRotationE2E verifies that replacing a bearer revokes the old one at
// once: the rotation window the BEADS_PREVIOUS_TOKEN env var used to open is
// gone with the shared static token (gd-ke6PC7bRto).
func TestTokenRotationE2E(t *testing.T) {
	const (
		oldAdmin = "old-admin-tok"
		newAdmin = "new-admin-tok"
	)

	fc := NewFleetConfig("gardenio", nil)
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.SetFleetAuth(fc)
	handler := srv.NewHTTPHandler(nil, withTestToken(fc, newAdmin))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	// New admin token works.
	t.Run("NewToken_Works", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodGet, "/v1/beads", newAdmin, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	// The old admin token is not accepted: there is no previous-token window.
	t.Run("OldToken_Rejected", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodGet, "/v1/beads", oldAdmin, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
	})

	// Unknown token fails.
	t.Run("UnknownToken_401", func(t *testing.T) {
		resp := e2eRequest(t, ts, http.MethodGet, "/v1/beads", "bogus", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
	})
}

// containsLabel checks if a label exists in a slice.
func containsLabel(labels []string, target string) bool {
	for _, l := range labels {
		if l == target {
			return true
		}
	}
	return false
}
