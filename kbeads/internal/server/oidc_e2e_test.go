package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// oidcIssuerMock simulates an OIDC issuer that accepts a set of known tokens.
// Each token maps to an email and optional roles. It implements TokenVerifier,
// IdentityVerifier, and RoleExtractor.
type oidcIssuerMock struct {
	// tokens maps raw token string → tokenInfo (email, roles).
	tokens map[string]tokenInfo
}

type tokenInfo struct {
	email string
	roles []string
	azp   string // authorized-party claim; "" = token has none
	// groups is the token's verified groups claim (Keycloak group paths).
	// nil = a token with no groups claim.
	groups []string
	// aud is the token's audience claim beyond the client's own, e.g. the
	// per-land "land-claimlayer" (land_audience.go).
	aud []string
	// audErr makes ExtractAudiences fail for a token that otherwise verifies.
	audErr bool
}

func (m *oidcIssuerMock) Verify(_ context.Context, rawToken string) error {
	if _, ok := m.tokens[rawToken]; ok {
		return nil
	}
	return fmt.Errorf("unknown token")
}

func (m *oidcIssuerMock) VerifyAndIdentify(_ context.Context, rawToken string) (string, error) {
	if info, ok := m.tokens[rawToken]; ok {
		return info.email, nil
	}
	return "", fmt.Errorf("unknown token")
}

func (m *oidcIssuerMock) ExtractRoles(_ context.Context, rawToken string) ([]string, error) {
	if info, ok := m.tokens[rawToken]; ok {
		return info.roles, nil
	}
	return nil, fmt.Errorf("unknown token")
}

func (m *oidcIssuerMock) ExtractClientID(_ context.Context, rawToken string) (string, error) {
	if info, ok := m.tokens[rawToken]; ok {
		return info.azp, nil
	}
	return "", fmt.Errorf("unknown token")
}

// ExtractGroups implements GroupExtractor for the mock.
func (m *oidcIssuerMock) ExtractGroups(_ context.Context, rawToken string) ([]string, error) {
	if info, ok := m.tokens[rawToken]; ok {
		return info.groups, nil
	}
	return nil, fmt.Errorf("unknown token")
}

// ExtractAudiences implements AudienceExtractor for the mock.
func (m *oidcIssuerMock) ExtractAudiences(_ context.Context, rawToken string) ([]string, error) {
	if info, ok := m.tokens[rawToken]; ok {
		if info.audErr {
			return nil, fmt.Errorf("aud claim unreadable")
		}
		return info.aud, nil
	}
	return nil, fmt.Errorf("unknown token")
}

// multiIssuerMock simulates the multiOIDCVerifier by trying multiple issuers.
type multiIssuerMock struct {
	issuers []*oidcIssuerMock
}

func (m *multiIssuerMock) Verify(ctx context.Context, rawToken string) error {
	var lastErr error
	for _, issuer := range m.issuers {
		if err := issuer.Verify(ctx, rawToken); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

func (m *multiIssuerMock) VerifyAndIdentify(ctx context.Context, rawToken string) (string, error) {
	var lastErr error
	for _, issuer := range m.issuers {
		if email, err := issuer.VerifyAndIdentify(ctx, rawToken); err == nil {
			return email, nil
		} else {
			lastErr = err
		}
	}
	return "", lastErr
}

func (m *multiIssuerMock) ExtractRoles(ctx context.Context, rawToken string) ([]string, error) {
	var lastErr error
	for _, issuer := range m.issuers {
		if roles, err := issuer.ExtractRoles(ctx, rawToken); err == nil {
			return roles, nil
		} else {
			lastErr = err
		}
	}
	return nil, lastErr
}

// ExtractGroups implements GroupExtractor across the issuers.
func (m *multiIssuerMock) ExtractGroups(ctx context.Context, rawToken string) ([]string, error) {
	var lastErr error
	for _, issuer := range m.issuers {
		if groups, err := issuer.ExtractGroups(ctx, rawToken); err == nil {
			return groups, nil
		} else {
			lastErr = err
		}
	}
	return nil, lastErr
}

// ExtractAudiences implements AudienceExtractor across the issuers.
func (m *multiIssuerMock) ExtractAudiences(ctx context.Context, rawToken string) ([]string, error) {
	var lastErr error
	for _, issuer := range m.issuers {
		if auds, err := issuer.ExtractAudiences(ctx, rawToken); err == nil {
			return auds, nil
		} else {
			lastErr = err
		}
	}
	return nil, lastErr
}

// ExtractClientID implements ClientIDExtractor across the issuers.
func (m *multiIssuerMock) ExtractClientID(ctx context.Context, rawToken string) (string, error) {
	var lastErr error
	for _, issuer := range m.issuers {
		if id, err := issuer.ExtractClientID(ctx, rawToken); err == nil {
			return id, nil
		} else {
			lastErr = err
		}
	}
	return "", lastErr
}

// newMultiIssuerE2EServer creates a full-stack test server with a multi-issuer
// OIDC mock verifier and AuthMiddleware. Requests go through the complete
// pipeline: AuthMiddleware → UserProvisioner → requireRole → handler.
func newMultiIssuerE2EServer(t *testing.T, adminToken string, fc *FleetConfig, verifier TokenVerifier) (*httptest.Server, *BeadsServer, *mockStore) {
	t.Helper()
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.SetFleetAuth(fc)
	handler := srv.NewHTTPHandler(verifier, withTestToken(fc, adminToken))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, srv, ms
}

// oidcE2ERequest issues an HTTP request with an optional Bearer token and
// JSON body against a running httptest.Server.
func oidcE2ERequest(t *testing.T, ts *httptest.Server, method, path, token string, body any) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = ioReader(b)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// ioReader wraps a byte slice in a bytes.Reader without importing bytes again.
func ioReader(b []byte) io.Reader {
	return &byteReader{b: b, off: 0}
}

type byteReader struct {
	b   []byte
	off int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}

// TestMultiIssuerOIDCE2E tests the full auth pipeline with a multi-issuer OIDC
// verifier. Covers: primary issuer auth, secondary issuer auth, unknown token
// rejection, role-based access, and user bead auto-provisioning.
func TestMultiIssuerOIDCE2E(t *testing.T) {
	const adminToken = "admin-oidc-e2e"

	// Issuer 1: Keycloak — has operator and admin roles.
	keycloak := &oidcIssuerMock{
		tokens: map[string]tokenInfo{
			"kc-alice-jwt":  {email: "alice@corp.example.com", roles: []string{RoleViewer, RoleOperator}},
			"kc-admin-jwt":  {email: "admin@corp.example.com", roles: []string{RoleViewer, RoleOperator, RoleAdmin}},
			"kc-viewer-jwt": {email: "viewer@corp.example.com", roles: []string{RoleViewer}},
		},
	}

	// Issuer 2: GitLab — CLI SSO, typically viewer+operator role.
	gitlab := &oidcIssuerMock{
		tokens: map[string]tokenInfo{
			"gl-bob-jwt":   {email: "bob@corp.example.com", roles: []string{RoleViewer, RoleOperator}},
			"gl-carol-jwt": {email: "carol@corp.example.com", roles: []string{RoleViewer}},
		},
	}

	verifier := &multiIssuerMock{issuers: []*oidcIssuerMock{keycloak, gitlab}}
	fc := NewFleetConfig("gardenio", nil)
	ts, _, ms := newMultiIssuerE2EServer(t, adminToken, fc, verifier)

	// ----------------------------------------------------------------
	// Phase 1: Primary issuer (Keycloak) — operator can create beads.
	// ----------------------------------------------------------------
	var aliceBeadID string
	t.Run("Keycloak_Operator_CreateBead", func(t *testing.T) {
		resp := oidcE2ERequest(t, ts, http.MethodPost, "/v1/beads", "kc-alice-jwt",
			createBeadInput{Title: "Alice's task", Type: "task"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201, got %d; body: %s", resp.StatusCode, body)
		}
		var bead model.Bead
		json.NewDecoder(resp.Body).Decode(&bead)
		aliceBeadID = bead.ID
	})

	// ----------------------------------------------------------------
	// Phase 2: Primary issuer — viewer can read but not write.
	// ----------------------------------------------------------------
	t.Run("Keycloak_Viewer_ReadBead", func(t *testing.T) {
		if aliceBeadID == "" {
			t.Skip("no bead from phase 1")
		}
		resp := oidcE2ERequest(t, ts, http.MethodGet, "/v1/beads/"+aliceBeadID, "kc-viewer-jwt", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d; body: %s", resp.StatusCode, body)
		}
	})

	t.Run("Keycloak_Viewer_CreateBead_403", func(t *testing.T) {
		resp := oidcE2ERequest(t, ts, http.MethodPost, "/v1/beads", "kc-viewer-jwt",
			createBeadInput{Title: "Should fail", Type: "task"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 403, got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 3: Secondary issuer (GitLab) — operator can create beads.
	// ----------------------------------------------------------------
	t.Run("GitLab_Operator_CreateBead", func(t *testing.T) {
		resp := oidcE2ERequest(t, ts, http.MethodPost, "/v1/beads", "gl-bob-jwt",
			createBeadInput{Title: "Bob's task", Type: "task"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201, got %d; body: %s", resp.StatusCode, body)
		}
	})

	t.Run("GitLab_Viewer_CreateBead_403", func(t *testing.T) {
		resp := oidcE2ERequest(t, ts, http.MethodPost, "/v1/beads", "gl-carol-jwt",
			createBeadInput{Title: "Should fail", Type: "task"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 403, got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 4: Unknown token — rejected by all issuers → 401.
	// ----------------------------------------------------------------
	t.Run("UnknownToken_401", func(t *testing.T) {
		resp := oidcE2ERequest(t, ts, http.MethodGet, "/v1/beads", "unknown-jwt-xyz", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 401, got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 5: No token at all → 401.
	// ----------------------------------------------------------------
	t.Run("NoToken_401", func(t *testing.T) {
		resp := oidcE2ERequest(t, ts, http.MethodGet, "/v1/beads", "", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 401, got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 6: OIDC admin cannot manage fleet tokens (isAdminRequest
	// checks the static admin token, not OIDC roles).
	// ----------------------------------------------------------------
	t.Run("OIDC_Admin_CannotManageFleetTokens", func(t *testing.T) {
		resp := oidcE2ERequest(t, ts, http.MethodGet, "/v1/fleet-tokens", "kc-admin-jwt", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 403 for OIDC admin on fleet-tokens, got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 7: Static admin token still works alongside OIDC.
	// ----------------------------------------------------------------
	t.Run("StaticAdmin_StillWorks", func(t *testing.T) {
		resp := oidcE2ERequest(t, ts, http.MethodGet, "/v1/beads", adminToken, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d; body: %s", resp.StatusCode, body)
		}
	})

	// ----------------------------------------------------------------
	// Phase 8: User bead auto-provisioned on first OIDC login.
	// ----------------------------------------------------------------
	t.Run("UserBead_AutoProvisioned", func(t *testing.T) {
		// Alice used Keycloak token in phase 1 — a user bead should exist.
		var found bool
		for _, b := range ms.beads {
			if b.Type != "user" {
				continue
			}
			var fields map[string]string
			_ = json.Unmarshal(b.Fields, &fields)
			if fields["email"] == "alice@corp.example.com" {
				found = true
				if b.Kind != "config" {
					t.Errorf("user bead kind = %q, want config", b.Kind)
				}
				if b.Title != "alice" {
					t.Errorf("user bead title = %q, want alice", b.Title)
				}
				if b.CreatedBy != "auto-provision" {
					t.Errorf("created_by = %q, want auto-provision", b.CreatedBy)
				}
				break
			}
		}
		if !found {
			t.Fatal("expected auto-provisioned user bead for alice@corp.example.com")
		}
	})

	t.Run("UserBead_SecondIssuer_AutoProvisioned", func(t *testing.T) {
		// Bob used GitLab token in phase 3 — user bead should also exist.
		var found bool
		for _, b := range ms.beads {
			if b.Type != "user" {
				continue
			}
			var fields map[string]string
			_ = json.Unmarshal(b.Fields, &fields)
			if fields["email"] == "bob@corp.example.com" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("expected auto-provisioned user bead for bob@corp.example.com")
		}
	})

	// ----------------------------------------------------------------
	// Phase 9: X-User-Bead-ID header is set for OIDC requests.
	// ----------------------------------------------------------------
	t.Run("UserBeadID_Header_Set", func(t *testing.T) {
		resp := oidcE2ERequest(t, ts, http.MethodGet, "/v1/beads", "kc-alice-jwt", nil)
		defer resp.Body.Close()
		beadID := resp.Header.Get("X-User-Bead-ID")
		if beadID == "" {
			t.Fatal("expected X-User-Bead-ID header for OIDC request")
		}
	})

	t.Run("UserBeadID_Header_NotSet_ForStatic", func(t *testing.T) {
		resp := oidcE2ERequest(t, ts, http.MethodGet, "/v1/beads", adminToken, nil)
		defer resp.Body.Close()
		if h := resp.Header.Get("X-User-Bead-ID"); h != "" {
			t.Fatalf("expected no X-User-Bead-ID for static token, got %q", h)
		}
	})

	// ----------------------------------------------------------------
	// Phase 10: Second request for same user — same bead ID (cached).
	// ----------------------------------------------------------------
	t.Run("UserBead_Cached_NoDuplicate", func(t *testing.T) {
		// Count user beads before.
		userBeadsBefore := 0
		for _, b := range ms.beads {
			if b.Type == "user" {
				userBeadsBefore++
			}
		}

		// Make another request with Alice's token.
		resp := oidcE2ERequest(t, ts, http.MethodGet, "/v1/beads", "kc-alice-jwt", nil)
		resp.Body.Close()

		userBeadsAfter := 0
		for _, b := range ms.beads {
			if b.Type == "user" {
				userBeadsAfter++
			}
		}
		if userBeadsAfter != userBeadsBefore {
			t.Errorf("expected no new user beads: before=%d, after=%d", userBeadsBefore, userBeadsAfter)
		}
	})
}

// TestOIDCRoleHierarchyE2E verifies that RBAC role hierarchy works through the
// full middleware stack: admin > operator > viewer.
func TestOIDCRoleHierarchyE2E(t *testing.T) {
	verifier := &oidcIssuerMock{
		tokens: map[string]tokenInfo{
			"viewer-tok":   {email: "v@example.com", roles: []string{RoleViewer}},
			"operator-tok": {email: "op@example.com", roles: []string{RoleOperator}},
			"admin-tok":    {email: "a@example.com", roles: []string{RoleAdmin}},
			"norole-tok":   {email: "nr@example.com", roles: []string{}},
		},
	}

	fc := NewFleetConfig("gardenio", nil)
	ts, _, _ := newMultiIssuerE2EServer(t, "static-admin", fc, verifier)

	tests := []struct {
		name     string
		token    string
		method   string
		path     string
		wantCode int
	}{
		// Viewer can read.
		{"Viewer_GET_beads_200", "viewer-tok", http.MethodGet, "/v1/beads", 200},
		// Viewer cannot create (requires operator).
		{"Viewer_POST_beads_403", "viewer-tok", http.MethodPost, "/v1/beads", 403},
		// Operator can create.
		{"Operator_POST_beads_201", "operator-tok", http.MethodPost, "/v1/beads", 201},
		// Operator cannot delete (requires admin).
		{"Operator_DELETE_beads_403", "operator-tok", http.MethodDelete, "/v1/beads/nonexistent", 403},
		// Admin can do everything — admin role has the highest rank.
		{"Admin_GET_beads_200", "admin-tok", http.MethodGet, "/v1/beads", 200},
		{"Admin_POST_beads_201", "admin-tok", http.MethodPost, "/v1/beads", 201},
		// No roles at all → forbidden on everything.
		{"NoRole_GET_beads_403", "norole-tok", http.MethodGet, "/v1/beads", 403},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var body any
			if tc.method == http.MethodPost {
				body = createBeadInput{Title: "Test", Type: "task"}
			}
			resp := oidcE2ERequest(t, ts, tc.method, tc.path, tc.token, body)
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantCode {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected %d, got %d; body: %s", tc.wantCode, resp.StatusCode, b)
			}
		})
	}
}

// TestForwardAuthEmailE2E verifies that when an OIDC token has no email claim
// and was minted for the edge forward-auth client, the X-Auth-Request-Email
// header (set by oauth2-proxy) is used as fallback. Other clients are refused
// the fallback: see TestForwardAuthEmailIgnoredForNonEdgeClients.
func TestForwardAuthEmailE2E(t *testing.T) {
	// This issuer returns no email for the token — simulating the oauth2-proxy
	// edge client's token without an email claim.
	verifier := &oidcIssuerMock{
		tokens: map[string]tokenInfo{
			"svc-token": {email: "", roles: []string{RoleViewer, RoleOperator}, azp: "edge-client"},
		},
	}

	fc := NewFleetConfig("gardenio", nil)
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.SetFleetAuth(fc)
	srv.SetForwardAuthClients([]string{"edge-client"})
	handler := srv.NewHTTPHandler(verifier, withTestToken(fc, "admin-fwd"))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	// Include X-Auth-Request-Email header (set by oauth2-proxy ForwardAuth).
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer svc-token")
	req.Header.Set("X-Auth-Request-Email", "fwd@corp.example.com")
	b, _ := json.Marshal(createBeadInput{Title: "Forward Auth Test", Type: "task"})
	req.Body = io.NopCloser(ioReader(b))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d; body: %s", resp.StatusCode, body)
	}

	// The user bead should be provisioned with the forwarded email.
	var found bool
	for _, bead := range ms.beads {
		if bead.Type != "user" {
			continue
		}
		var fields map[string]string
		_ = json.Unmarshal(bead.Fields, &fields)
		if fields["email"] == "fwd@corp.example.com" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected auto-provisioned user bead for fwd@corp.example.com (from X-Auth-Request-Email)")
	}
}

// TestOIDCWithFleetTokenCoexistenceE2E verifies that OIDC tokens and fleet
// tokens can coexist: fleet tokens use the token-to-fleet mapping, while
// OIDC tokens fall through to the verifier after static token check fails.
func TestOIDCWithFleetTokenCoexistenceE2E(t *testing.T) {
	verifier := &oidcIssuerMock{
		tokens: map[string]tokenInfo{
			"oidc-user-jwt": {email: "user@corp.example.com", roles: []string{RoleViewer, RoleOperator}},
		},
	}

	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterToken("fleet-tok-xyz", "agent-fleet")
	ts, _, ms := newMultiIssuerE2EServer(t, "admin-coexist", fc, verifier)

	// Fleet token creates a bead.
	t.Run("FleetToken_CreateBead", func(t *testing.T) {
		resp := oidcE2ERequest(t, ts, http.MethodPost, "/v1/beads", "fleet-tok-xyz",
			createBeadInput{Title: "Fleet task", Type: "task"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201, got %d; body: %s", resp.StatusCode, body)
		}
		// No user bead should be created for fleet tokens.
		for _, b := range ms.beads {
			if b.Type == "user" {
				var fields map[string]string
				_ = json.Unmarshal(b.Fields, &fields)
				if fields["email"] != "" {
					t.Errorf("unexpected user bead for fleet token: %v", fields)
				}
			}
		}
	})

	// OIDC token creates a bead and provisions a user bead.
	t.Run("OIDCToken_CreateBead_WithUserProvision", func(t *testing.T) {
		resp := oidcE2ERequest(t, ts, http.MethodPost, "/v1/beads", "oidc-user-jwt",
			createBeadInput{Title: "OIDC task", Type: "task"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201, got %d; body: %s", resp.StatusCode, body)
		}

		// User bead should exist for OIDC user.
		var found bool
		for _, b := range ms.beads {
			if b.Type != "user" {
				continue
			}
			var fields map[string]string
			_ = json.Unmarshal(b.Fields, &fields)
			if fields["email"] == "user@corp.example.com" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("expected user bead for OIDC user")
		}
	})

	// Unknown token rejected even with fleet tokens registered.
	t.Run("UnknownToken_401", func(t *testing.T) {
		resp := oidcE2ERequest(t, ts, http.MethodGet, "/v1/beads", "bogus-token", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
	})
}
