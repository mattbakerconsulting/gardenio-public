package oidcauth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// saClaims builds a projected-ServiceAccount-shaped ID token payload for the
// fixture's issuer with the supplied audience, subject and expiry. Kubernetes
// SA tokens carry no email/roles claim — only sub + kubernetes.io — so the test
// intentionally omits them: the branch under test must SYNTHESIZE both.
func saClaims(iss, aud, sub string, exp time.Time) string {
	return fmt.Sprintf(`{
		"iss": %q,
		"aud": %q,
		"sub": %q,
		"exp": %d,
		"kubernetes.io": {"namespace": "gardenio-system", "serviceaccount": {"name": "audit"}}
	}`, iss, aud, sub, exp.Unix())
}

const (
	testSAAudience    = "gardenio-beads-viewer-audit"
	testSASubject     = "system:serviceaccount:gardenio-system:gardenio-audit"
	testSADomain      = "acme.example"
	testSAExpectEmail = "gardenio-system.gardenio-audit@acme.example"
)

// baseSACfg returns an enabled K8sSAConfig pinned to the fixture issuer, with
// the standard allow-list, synthetic domain and viewer role floor.
func baseSACfg(issuer string) K8sSAConfig {
	return K8sSAConfig{
		IssuerURL:            issuer,
		Audience:             testSAAudience,
		AllowedSubjects:      []string{testSASubject},
		SyntheticEmailDomain: testSADomain,
		RoleFloor:            RoleViewer,
	}
}

// TestSABranch_EndToEnd drives the REAL middleware (New → saBranch → go-oidc
// JWKS/iss/aud/exp verification) for an audience-pinned projected SA token and
// asserts the whole acceptance matrix from ka-0000006288: a valid allow-listed
// token is admitted with a SYNTHESIZED domain-valid email + role floor in
// context, and every reject case (wrong audience, wrong issuer, expired,
// un-allow-listed sub, apiserver/IRSA audience) is refused with NO fallthrough.
func TestSABranch_EndToEnd(t *testing.T) {
	eks := newOIDCFixture(t)

	// The human Keycloak/Entra path is configured alongside so the SA branch is
	// genuinely additive and the regression assertions below are meaningful.
	human := newOIDCFixture(t)
	cfg := Config{
		IssuerURL:           human.issuer,
		ClientID:            "human-client",
		RequiredRole:        RoleViewer,
		AllowedEmailDomains: []string{"acme.example", "acme.example"},
		K8sSA:               baseSACfg(eks.issuer),
	}

	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	tests := []struct {
		name       string
		token      string
		wantStatus int
		wantEmail  string
		wantRoles  []string
	}{
		{
			name:       "valid allow-listed SA token → 200 with synthetic email + role floor",
			token:      eks.signToken(t, saClaims(eks.issuer, testSAAudience, testSASubject, future)),
			wantStatus: http.StatusOK,
			wantEmail:  testSAExpectEmail,
			wantRoles:  []string{RoleViewer},
		},
		{
			name:       "wrong audience → 401",
			token:      eks.signToken(t, saClaims(eks.issuer, "some-other-audience", testSASubject, future)),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "apiserver audience → 401 (never accept a repurposed default token)",
			token:      eks.signToken(t, saClaims(eks.issuer, "https://kubernetes.default.svc", testSASubject, future)),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "IRSA/STS audience → 401 (never accept a repurposed web-identity token)",
			token:      eks.signToken(t, saClaims(eks.issuer, "sts.amazonaws.com", testSASubject, future)),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "expired token → 401",
			token:      eks.signToken(t, saClaims(eks.issuer, testSAAudience, testSASubject, past)),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "un-allow-listed subject → 403",
			token:      eks.signToken(t, saClaims(eks.issuer, testSAAudience, "system:serviceaccount:gardenio-system:not-listed", future)),
			wantStatus: http.StatusForbidden,
		},
		{
			// Signed by the EKS key with the EKS issuer claim, but not a
			// serviceaccount subject. It clears verification + the (adjusted)
			// allow-list only if listed, so add it to the allow-list-independent
			// path check: here it is NOT allow-listed, so it 403s on the
			// allow-list first — the malformed-subject guard is covered by a
			// dedicated unit test below.
			name:       "non-serviceaccount subject → 403",
			token:      eks.signToken(t, saClaims(eks.issuer, testSAAudience, "some-other-principal", future)),
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := New(context.Background(), cfg, slog.Default())
			waitReady(t, h)

			var gotEmail string
			var gotRoles []string
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotEmail = EmailFromContext(r.Context())
				gotRoles = RolesFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+tt.token)
			rec := httptest.NewRecorder()
			h.Middleware(inner).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantEmail != "" && gotEmail != tt.wantEmail {
				t.Errorf("EmailFromContext = %q, want %q", gotEmail, tt.wantEmail)
			}
			if tt.wantRoles != nil {
				if len(gotRoles) != len(tt.wantRoles) {
					t.Fatalf("RolesFromContext = %v, want %v", gotRoles, tt.wantRoles)
				}
				for i := range tt.wantRoles {
					if gotRoles[i] != tt.wantRoles[i] {
						t.Errorf("RolesFromContext[%d] = %q, want %q", i, gotRoles[i], tt.wantRoles[i])
					}
				}
			}
		})
	}
}

// TestSABranch_WrongIssuerRoutesToHumanPath proves the issuer-claim-based
// routing is exact: a token whose issuer is NOT the EKS issuer is handed to the
// base (human) chain, never the SA verifier. Here a token signed by the EKS
// fixture but carrying a different `iss` claim fails signature verification on
// the human path — the point is that it is NOT admitted as a machine identity.
func TestSABranch_WrongIssuerRoutesToHumanPath(t *testing.T) {
	eks := newOIDCFixture(t)
	human := newOIDCFixture(t)
	cfg := Config{
		IssuerURL:           human.issuer,
		ClientID:            "human-client",
		RequiredRole:        RoleViewer,
		AllowedEmailDomains: []string{"acme.example"},
		K8sSA:               baseSACfg(eks.issuer),
	}
	h := New(context.Background(), cfg, slog.Default())
	waitReady(t, h)

	// A token whose iss is neither issuer: routed to base, rejected there.
	tok := eks.signToken(t, saClaims("https://wrong-issuer.example", testSAAudience, testSASubject, time.Now().Add(time.Hour)))
	var admitted bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		admitted = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.Middleware(inner).ServeHTTP(rec, req)

	if admitted {
		t.Fatalf("token with foreign issuer was admitted; must be rejected (status %d)", rec.Code)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (routed to human path and rejected)", rec.Code)
	}
}

// TestSABranch_HumanEntraPathUnaffected is the regression guard required by the
// acceptance criteria: with the SA branch enabled, a legitimate human Entra
// token on the SAME Authorization: Bearer header is still verified and admitted
// through the Keycloak path exactly as before.
func TestSABranch_HumanEntraPathUnaffected(t *testing.T) {
	eks := newOIDCFixture(t)
	human := newOIDCFixture(t)
	cfg := Config{
		IssuerURL:           human.issuer,
		ClientID:            "human-client",
		ExpectedTenantID:    testEntraTenant,
		RequiredRole:        RoleViewer,
		AllowedEmailDomains: []string{"acme.example", "acme.example"},
		K8sSA:               baseSACfg(eks.issuer),
	}
	h := New(context.Background(), cfg, slog.Default())
	waitReady(t, h)

	humanClaims := fmt.Sprintf(`{
		"iss": %q,
		"aud": "human-client",
		"sub": "user-1",
		"exp": %d,
		"tid": %q,
		"email": "operator@acme.example",
		"roles": [%q]
	}`, human.issuer, time.Now().Add(time.Hour).Unix(), testEntraTenant, RoleOperator)

	var gotEmail string
	var gotRoles []string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEmail = EmailFromContext(r.Context())
		gotRoles = RolesFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+human.signToken(t, humanClaims))
	rec := httptest.NewRecorder()
	h.Middleware(inner).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("human Entra token rejected with SA branch enabled: status %d (body %q)", rec.Code, rec.Body.String())
	}
	if gotEmail != "operator@acme.example" {
		t.Errorf("EmailFromContext = %q, want operator@acme.example", gotEmail)
	}
	if len(gotRoles) != 1 || gotRoles[0] != RoleOperator {
		t.Errorf("RolesFromContext = %v, want [%s]", gotRoles, RoleOperator)
	}
}

// TestSABranch_DisabledIsPassthroughIdentical proves an unconfigured SA branch
// changes nothing: with K8sSA unset, the handle behaves exactly like the
// pre-existing Keycloak-only middleware (a valid human token is admitted, a
// missing token is 401'd).
func TestSABranch_DisabledIsPassthroughIdentical(t *testing.T) {
	human := newOIDCFixture(t)
	// validEntraClaims hardcodes aud "test-client", so pin ClientID to match.
	cfg := Config{IssuerURL: human.issuer, ClientID: "test-client", RequiredRole: RoleViewer, AllowedEmailDomains: []string{"acme.example"}}
	h := New(context.Background(), cfg, slog.Default())
	waitReady(t, h)

	// saDiscovered is satisfied immediately when the branch is disabled.
	if !h.saDiscovered() {
		t.Fatal("saDiscovered() should be true immediately when the SA branch is disabled")
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+human.signToken(t,
		human.validEntraClaims("user@acme.example", []string{RoleViewer})))
	rec := httptest.NewRecorder()
	h.Middleware(inner).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid human token not admitted with SA disabled: %d", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	rec2 := httptest.NewRecorder()
	h.Middleware(inner).ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("missing token should 401 with SA disabled, got %d", rec2.Code)
	}
}

// TestSABranch_FailClosedBeforeDiscovery asserts the load-bearing invariant: a
// request the SA branch CLAIMS (its issuer matches) before EKS discovery
// completes gets a 503 fail-closed response, never a passthrough. We force
// discovery to fail fast by pointing the SA issuer at a refused TCP endpoint.
func TestSABranch_FailClosedBeforeDiscovery(t *testing.T) {
	origAttempts, origDelay := providerRetryAttempts, providerRetryInitialDelay
	providerRetryAttempts = 1
	providerRetryInitialDelay = 0
	t.Cleanup(func() {
		providerRetryAttempts = origAttempts
		providerRetryInitialDelay = origDelay
	})

	human := newOIDCFixture(t)
	// A syntactically-valid but unreachable EKS issuer.
	saCfg := baseSACfg("https://127.0.0.1:1/id/never-discovers")
	cfg := Config{
		IssuerURL:           human.issuer,
		ClientID:            "human-client",
		AllowedEmailDomains: []string{"acme.example"},
		K8sSA:               saCfg,
	}
	h := New(context.Background(), cfg, slog.Default())

	// Craft a token whose unverified issuer matches the SA issuer so saBranch
	// routes it to the (still fail-closed) SA verifier.
	tok := signSAForIssuer(t, human, saCfg.IssuerURL)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.Middleware(inner).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("SA-claimed request before discovery = %d, want 503 (fail closed, never passthrough)", rec.Code)
	}
}

// signSAForIssuer mints a token with the given `iss` claim signed by f's key.
// Used to route a request to the SA branch by its unverified issuer without
// standing up a second live issuer.
func signSAForIssuer(t *testing.T, f *oidcFixture, iss string) string {
	t.Helper()
	return f.signToken(t, saClaims(iss, testSAAudience, testSASubject, time.Now().Add(time.Hour)))
}

// TestSyntheticSAEmail unit-tests the sub→email mapping directly, including the
// malformed-subject rejection the end-to-end table cannot reach (it 403s on the
// allow-list first).
func TestSyntheticSAEmail(t *testing.T) {
	tests := []struct {
		sub       string
		wantEmail string
		wantOK    bool
	}{
		{"system:serviceaccount:gardenio-system:audit", "gardenio-system.audit@acme.example", true},
		{"system:serviceaccount:ns:sa", "ns.sa@acme.example", true},
		{"system:serviceaccount:only-namespace", "", false},
		{"system:serviceaccount::sa", "", false},
		{"system:serviceaccount:ns:", "", false},
		{"not-a-serviceaccount", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.sub, func(t *testing.T) {
			email, ok := syntheticSAEmail(tt.sub, "acme.example")
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && email != tt.wantEmail {
				t.Errorf("email = %q, want %q", email, tt.wantEmail)
			}
		})
	}
}

// TestUnverifiedIssuer covers the routing peek — well-formed JWT payloads yield
// their iss; anything malformed yields "" (routed to base, never the SA path).
func TestUnverifiedIssuer(t *testing.T) {
	f := newOIDCFixture(t)
	valid := f.signToken(t, saClaims("https://issuer.example", testSAAudience, testSASubject, time.Now().Add(time.Hour)))
	if got := unverifiedIssuer(valid); got != "https://issuer.example" {
		t.Errorf("unverifiedIssuer(valid) = %q, want https://issuer.example", got)
	}
	for _, bad := range []string{"", "not-a-jwt", "only.two", "a.b.c.d", "!!!.!!!.!!!"} {
		if got := unverifiedIssuer(bad); got != "" {
			t.Errorf("unverifiedIssuer(%q) = %q, want empty", bad, got)
		}
	}
}
