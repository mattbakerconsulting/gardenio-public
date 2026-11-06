package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- Unit tests for parseForwardAuthGroups ---

func TestParseForwardAuthGroups_SingleGroup(t *testing.T) {
	got := parseForwardAuthGroups("/project-alpha")
	if len(got) != 1 || got[0] != "/project-alpha" {
		t.Errorf("expected [/project-alpha], got %v", got)
	}
}

func TestParseForwardAuthGroups_MultipleGroups(t *testing.T) {
	got := parseForwardAuthGroups("/project-alpha,/project-beta,/admin")
	if len(got) != 3 {
		t.Fatalf("expected 3 groups, got %d: %v", len(got), got)
	}
	if got[0] != "/project-alpha" || got[1] != "/project-beta" || got[2] != "/admin" {
		t.Errorf("unexpected groups: %v", got)
	}
}

func TestParseForwardAuthGroups_WithSpaces(t *testing.T) {
	got := parseForwardAuthGroups("/project-alpha , /project-beta")
	if len(got) != 2 {
		t.Fatalf("expected 2 groups, got %d: %v", len(got), got)
	}
	if got[0] != "/project-alpha" || got[1] != "/project-beta" {
		t.Errorf("unexpected groups: %v", got)
	}
}

func TestParseForwardAuthGroups_EmptyString(t *testing.T) {
	got := parseForwardAuthGroups("")
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestParseForwardAuthGroups_TrailingComma(t *testing.T) {
	got := parseForwardAuthGroups("/project-alpha,")
	if len(got) != 1 || got[0] != "/project-alpha" {
		t.Errorf("expected [/project-alpha], got %v", got)
	}
}

// --- stubGroupExtractor implements RoleExtractor + GroupExtractor for testing ---

type stubGroupExtractor struct {
	roles  []string
	groups []string
	azp    string // authorized-party claim; "" = token has none
	err    error
}

func (s *stubGroupExtractor) ExtractClientID(_ context.Context, _ string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.azp, nil
}

// testEdgeClient is the forward-auth client allowlist the header-trusting tests
// run with; stubs minted "for the edge" carry this azp.
const testEdgeClient = "edge-client"

var testEdgeClients = map[string]struct{}{testEdgeClient: {}}

func (s *stubGroupExtractor) Verify(_ context.Context, _ string) error {
	return s.err
}

func (s *stubGroupExtractor) ExtractRoles(_ context.Context, _ string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.roles, nil
}

func (s *stubGroupExtractor) ExtractGroups(_ context.Context, _ string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.groups, nil
}

// --- Integration tests: ForwardAuth groups header through AuthMiddleware ---

func TestAuthMiddleware_ForwardAuthGroupsHeader_UsedWhenNoClaim(t *testing.T) {
	var capturedGroups []string
	handler := authMiddleware(testEdgeClients, withTestToken(nil, "secret"), &stubGroupExtractor{
		roles: []string{RoleOperator},
		azp:   testEdgeClient,
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedGroups = GroupsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer oidc-jwt")
	req.Header.Set("X-Auth-Request-Groups", "/project-alpha,/project-beta")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	// The token has no groups claim, so the edge client's header is used.
	if len(capturedGroups) != 2 {
		t.Fatalf("expected 2 groups from header, got %d: %v", len(capturedGroups), capturedGroups)
	}
	if capturedGroups[0] != "/project-alpha" || capturedGroups[1] != "/project-beta" {
		t.Errorf("unexpected groups: %v", capturedGroups)
	}
}

func TestAuthMiddleware_NoGroupsHeader_FallsBackToJWT(t *testing.T) {
	var capturedGroups []string
	handler := AuthMiddleware(withTestToken(nil, "secret"), &stubGroupExtractor{
		roles:  []string{RoleOperator},
		groups: []string{"/project-gamma"},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedGroups = GroupsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer oidc-jwt")
	// No X-Auth-Request-Groups header — should fall back to JWT extraction.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	// Should use JWT-extracted groups.
	if len(capturedGroups) != 1 || capturedGroups[0] != "/project-gamma" {
		t.Errorf("expected [/project-gamma] from JWT fallback, got %v", capturedGroups)
	}
}

func TestAuthMiddleware_EmptyGroupsHeader_FallsBackToJWT(t *testing.T) {
	var capturedGroups []string
	handler := AuthMiddleware(withTestToken(nil, "secret"), &stubGroupExtractor{
		roles:  []string{RoleOperator},
		groups: []string{"/project-delta"},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedGroups = GroupsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer oidc-jwt")
	req.Header.Set("X-Auth-Request-Groups", "") // empty header
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Empty header should trigger JWT fallback.
	if len(capturedGroups) != 1 || capturedGroups[0] != "/project-delta" {
		t.Errorf("expected [/project-delta] from JWT fallback, got %v", capturedGroups)
	}
}

func TestAuthMiddleware_GroupsClaimPreferredOverHeader(t *testing.T) {
	var capturedGroups []string
	handler := authMiddleware(testEdgeClients, withTestToken(nil, "secret"), &stubGroupExtractor{
		roles:  []string{RoleOperator},
		azp:    testEdgeClient,
		groups: []string{"/jwt-group"},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedGroups = GroupsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer oidc-jwt")
	req.Header.Set("X-Auth-Request-Groups", "/header-group")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// gd-Wmom75wBdl: the token's own claim takes precedence over the header.
	if len(capturedGroups) != 1 || capturedGroups[0] != "/jwt-group" {
		t.Errorf("expected [/jwt-group] from the claim (not the header), got %v", capturedGroups)
	}
}

func TestAuthMiddleware_ForwardAuthGroups_ViaAccessTokenHeader(t *testing.T) {
	var capturedGroups []string
	handler := authMiddleware(testEdgeClients, withTestToken(nil, "secret"), &stubGroupExtractor{
		roles: []string{RoleOperator},
		azp:   testEdgeClient,
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedGroups = GroupsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	// Use X-Auth-Request-Access-Token instead of Authorization header.
	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("X-Auth-Request-Access-Token", "forwarded-jwt")
	req.Header.Set("X-Auth-Request-Groups", "/project-epsilon")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	if len(capturedGroups) != 1 || capturedGroups[0] != "/project-epsilon" {
		t.Errorf("expected [/project-epsilon], got %v", capturedGroups)
	}
}
