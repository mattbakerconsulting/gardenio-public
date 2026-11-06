package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
)

// Lands Track B B-D1, step 7.5: an applicant is a stranger who signed in
// through the realm (GitHub) and holds no gardenio role and no group. The
// viewer's front of house serves them; kbeads must give them nothing. This
// walks every route registerRoutes registers, through the real HTTP chain, as
// such a caller, and requires a 403 on all of them except the few that
// describe only the caller themself.

// applicantOpenRoutes are the routes an applicant may reach: health probes and
// the caller's own identity and terms (no bead, no other person). Anything
// added here is a hole in the Gate and needs a reason.
var applicantOpenRoutes = map[string]string{
	"GET /v1/me":                "who am I, from the token alone (me.go)",
	"GET /v1/users/me/terms":    "the caller's own accepted terms version (step 7.5)",
	"POST /v1/users/me/terms":   "accept the terms on the caller's own user bead (step 7.4)",
	"POST /v1/auth/refresh":     "refresh-token exchange: the credential is the refresh token in the body, not the bearer",
	"POST /v1/runners/register": "runner registration: the credential is the single-use invite code in the body (runners.go)",
}

// applicantRequest serves one request as the roleless applicant and returns the
// status. The deadline keeps a stream route that slipped past its role gate
// from hanging the walk: it would show up as a non-403 instead.
func applicantRequest(t *testing.T, h http.Handler, method, path string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte("{}"))).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer stranger@example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestApplicant_ForbiddenOnEveryBeadRoute(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.SetRateLimit(RateLimitConfig{})
	h := srv.NewHTTPHandler(rolelessVerifier{})

	patterns := registeredRoutePatterns()
	var leaks []string
	walked := 0
	for _, pattern := range patterns {
		if _, open := applicantOpenRoutes[pattern]; open || isHealthRoute(pattern) {
			continue
		}
		method, path := routeRequest(pattern)
		walked++
		if code := applicantRequest(t, h, method, path); code != http.StatusForbidden {
			leaks = append(leaks, method+" "+path+" = "+http.StatusText(code))
		}
	}
	sort.Strings(leaks)
	if len(leaks) > 0 {
		t.Errorf("routes that do not refuse an applicant (no role, no group) with 403:\n  %s", strings.Join(leaks, "\n  "))
	}
	if walked < 150 {
		t.Fatalf("only %d routes walked; the registration walk is broken", walked)
	}

	// Positive control: the open routes are open, so the 403s above are the role
	// gate and not a broken chain refusing everyone. (The refresh exchange has no
	// manager in this fixture and answers 503, which is still not a refusal.)
	for pattern := range applicantOpenRoutes {
		if !containsPattern(patterns, pattern) {
			t.Errorf("applicantOpenRoutes names %q, which is not registered", pattern)
			continue
		}
		method, path := routeRequest(pattern)
		if code := applicantRequest(t, h, method, path); code == http.StatusForbidden || code == http.StatusUnauthorized {
			t.Errorf("%s as applicant = %d, want it served", pattern, code)
		}
	}
}

// isHealthRoute: liveness and readiness probes carry no bead data.
func isHealthRoute(pattern string) bool {
	_, path, _ := strings.Cut(pattern, " ")
	return path == "/healthz" || path == "/readyz" || strings.HasPrefix(path, "/v1/health")
}

func containsPattern(patterns []string, p string) bool {
	for _, q := range patterns {
		if q == p {
			return true
		}
	}
	return false
}

// GET /v1/users/me/terms reads the caller's own acceptance, any role or none,
// and is empty for someone who never accepted.
func TestMyTerms_ReadsOwnAcceptance(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	h := srv.NewHTTPHandler(rolelessVerifier{})
	do := func(method string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/v1/users/me/terms", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer stranger@example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodGet, "")
	requireStatus(t, rec, http.StatusOK)
	var before UserTermsResponse
	decodeJSON(t, rec, &before)
	if before.TermsVersion != "" || before.TermsAcceptedAt != "" {
		t.Fatalf("never accepted, got %+v", before)
	}

	requireStatus(t, do(http.MethodPost, `{"terms_version":"2026-10-19-draft.1"}`), http.StatusOK)
	rec = do(http.MethodGet, "")
	requireStatus(t, rec, http.StatusOK)
	var after UserTermsResponse
	decodeJSON(t, rec, &after)
	if after.TermsVersion != "2026-10-19-draft.1" || after.TermsAcceptedAt == "" || after.UserBeadID == "" {
		t.Fatalf("after accepting, got %+v", after)
	}
}

// A non-OIDC caller has no "me" to read.
func TestMyTerms_RefusesNonOIDC(t *testing.T) {
	s, _, _ := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/v1/users/me/terms", nil)
	rec := httptest.NewRecorder()
	s.handleMyTerms(rec, req)
	requireStatus(t, rec, http.StatusForbidden)
}
