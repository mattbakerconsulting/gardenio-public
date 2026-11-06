package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/edgeauth"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const testEdgeAuthKey = "edge-auth-test-key"

func signEdge(t *testing.T, op, auditID, path, ts string) string {
	t.Helper()
	m := hmac.New(sha256.New, []byte(testEdgeAuthKey))
	_, _ = m.Write([]byte(op + "\n" + auditID + "\n" + path + "\n" + ts))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func signEdgeV2(
	t *testing.T,
	op, authMode, roles, auditID, method, path, ts string,
) string {
	t.Helper()
	m := hmac.New(sha256.New, []byte(testEdgeAuthKey))
	_, _ = m.Write([]byte(strings.Join([]string{
		"v2",
		op,
		authMode,
		roles,
		auditID,
		strings.ToUpper(method),
		path,
		ts,
	}, "\n")))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func setV2EdgeHeaders(
	t *testing.T,
	r *http.Request,
	op, authMode, roles, auditID, ts, sig string,
) {
	t.Helper()
	r.Header.Set(edgeauth.HeaderOperator, op)
	r.Header.Set(edgeauth.HeaderAuditID, auditID)
	r.Header.Set(edgeauth.HeaderTimestamp, ts)
	r.Header.Set(edgeauth.HeaderVersion, "2")
	r.Header.Set(edgeauth.HeaderAuthMode, authMode)
	r.Header.Set(edgeauth.HeaderRoles, roles)
	r.Header.Set(
		edgeauth.HeaderSignature,
		signEdge(t, op, auditID, r.URL.Path, ts),
	)
	r.Header.Set(edgeauth.HeaderSignatureV2, sig)
}

func trustedHandlerSpy(called *bool, gotID *AuthIdentity) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		*gotID = AuthIdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func fallbackHandlerSpy(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		w.WriteHeader(http.StatusTeapot) // distinguishable
	})
}

func TestEdgeAuthMiddleware_NoOperator_Fallback(t *testing.T) {
	var trustedCalled, fallbackCalled bool
	var id AuthIdentity
	cfg := edgeauth.Config{HMACKey: []byte(testEdgeAuthKey)}
	h := EdgeAuthMiddleware(cfg,
		trustedHandlerSpy(&trustedCalled, &id),
		fallbackHandlerSpy(&fallbackCalled))

	r := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if trustedCalled {
		t.Error("trustedHandler called for request without X-Operator")
	}
	if !fallbackCalled {
		t.Error("fallback NOT called for request without X-Operator")
	}
	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", w.Code, http.StatusTeapot)
	}
}

func TestEdgeAuthMiddleware_HealthBypass(t *testing.T) {
	var trustedCalled, fallbackCalled bool
	var id AuthIdentity
	cfg := edgeauth.Config{HMACKey: []byte(testEdgeAuthKey), Enforce: true}
	h := EdgeAuthMiddleware(cfg,
		trustedHandlerSpy(&trustedCalled, &id),
		fallbackHandlerSpy(&fallbackCalled))

	r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	r.Header.Set(edgeauth.HeaderOperator, "alice@example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if !fallbackCalled {
		t.Error("health request should fall through to fallback")
	}
	if trustedCalled {
		t.Error("health request must not go through trustedHandler")
	}
}

func TestEdgeAuthMiddleware_ValidEnvelope_Trusted(t *testing.T) {
	edgeAuthVerifyTotal.Reset()
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := signEdge(t, "alice@example.com", "audit-1", "/v1/beads", ts)

	cfg := edgeauth.Config{
		HMACKey: []byte(testEdgeAuthKey),
		Now:     func() time.Time { return now },
	}
	var trustedCalled, fallbackCalled bool
	var id AuthIdentity
	h := EdgeAuthMiddleware(cfg,
		trustedHandlerSpy(&trustedCalled, &id),
		fallbackHandlerSpy(&fallbackCalled))

	r := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	r.Header.Set(edgeauth.HeaderOperator, "alice@example.com")
	r.Header.Set(edgeauth.HeaderAuditID, "audit-1")
	r.Header.Set(edgeauth.HeaderTimestamp, ts)
	r.Header.Set(edgeauth.HeaderSignature, sig)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if !trustedCalled {
		t.Fatal("trustedHandler NOT called for valid envelope")
	}
	if fallbackCalled {
		t.Error("fallback called for valid envelope")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body=%q)", w.Code, http.StatusOK, w.Body.String())
	}
	if id.Type != "edge" || id.Subject != "alice@example.com" {
		t.Errorf("AuthIdentity = %+v, want {Type:edge Subject:alice@example.com}", id)
	}
	if n := testutil.ToFloat64(edgeAuthVerifyTotal.WithLabelValues("ok", "false")); n != 1 {
		t.Errorf("verify_total{ok,false} = %v, want 1", n)
	}
}

func TestEdgeAuthMiddleware_BadSig_BurnIn_StillTrusted(t *testing.T) {
	edgeAuthVerifyTotal.Reset()
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)

	cfg := edgeauth.Config{
		HMACKey: []byte(testEdgeAuthKey),
		Enforce: false, // burn-in
		Now:     func() time.Time { return now },
	}
	var trustedCalled, fallbackCalled bool
	var id AuthIdentity
	h := EdgeAuthMiddleware(cfg,
		trustedHandlerSpy(&trustedCalled, &id),
		fallbackHandlerSpy(&fallbackCalled))

	r := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	r.Header.Set(edgeauth.HeaderOperator, "alice@example.com")
	r.Header.Set(edgeauth.HeaderAuditID, "audit-1")
	r.Header.Set(edgeauth.HeaderTimestamp, ts)
	r.Header.Set(edgeauth.HeaderSignature, base64.StdEncoding.EncodeToString([]byte("garbage")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("burn-in: status = %d, want %d", w.Code, http.StatusOK)
	}
	if !trustedCalled {
		t.Error("burn-in: trustedHandler should be called even on mismatch")
	}
	if id.Type != "edge" {
		t.Errorf("burn-in: id.Type = %q, want edge", id.Type)
	}
	if n := testutil.ToFloat64(edgeAuthVerifyTotal.WithLabelValues("mismatch", "false")); n != 1 {
		t.Errorf("verify_total{mismatch,false} = %v, want 1", n)
	}
}

func TestEdgeAuthMiddleware_BadSig_Enforce_Rejected(t *testing.T) {
	edgeAuthVerifyTotal.Reset()
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)

	cfg := edgeauth.Config{
		HMACKey: []byte(testEdgeAuthKey),
		Enforce: true,
		Now:     func() time.Time { return now },
	}
	var trustedCalled, fallbackCalled bool
	var id AuthIdentity
	h := EdgeAuthMiddleware(cfg,
		trustedHandlerSpy(&trustedCalled, &id),
		fallbackHandlerSpy(&fallbackCalled))

	r := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	r.Header.Set(edgeauth.HeaderOperator, "alice@example.com")
	r.Header.Set(edgeauth.HeaderAuditID, "audit-1")
	r.Header.Set(edgeauth.HeaderTimestamp, ts)
	r.Header.Set(edgeauth.HeaderSignature, base64.StdEncoding.EncodeToString([]byte("garbage")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("enforce: status = %d, want 401", w.Code)
	}
	if trustedCalled {
		t.Error("enforce: trustedHandler must NOT be called on bad sig")
	}
	if fallbackCalled {
		t.Error("enforce: fallback must NOT be called on bad sig")
	}
	if n := testutil.ToFloat64(edgeAuthVerifyTotal.WithLabelValues("mismatch", "true")); n != 1 {
		t.Errorf("verify_total{mismatch,true} = %v, want 1", n)
	}
}

func TestEdgeAuthMiddleware_Stale_Enforce_Rejected(t *testing.T) {
	edgeAuthVerifyTotal.Reset()
	now := time.Unix(1_700_000_000, 0)
	staleTs := strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10)
	sig := signEdge(t, "alice@example.com", "audit-1", "/v1/beads", staleTs)

	cfg := edgeauth.Config{
		HMACKey: []byte(testEdgeAuthKey),
		Enforce: true,
		Now:     func() time.Time { return now },
	}
	var trustedCalled, fallbackCalled bool
	var id AuthIdentity
	h := EdgeAuthMiddleware(cfg,
		trustedHandlerSpy(&trustedCalled, &id),
		fallbackHandlerSpy(&fallbackCalled))

	r := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	r.Header.Set(edgeauth.HeaderOperator, "alice@example.com")
	r.Header.Set(edgeauth.HeaderAuditID, "audit-1")
	r.Header.Set(edgeauth.HeaderTimestamp, staleTs)
	r.Header.Set(edgeauth.HeaderSignature, sig)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("enforce stale: status = %d, want 401", w.Code)
	}
	if trustedCalled {
		t.Error("enforce stale: trustedHandler must NOT be called")
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "edge-auth verification failed") {
		t.Errorf("body = %q, want failure message", string(body))
	}
}

func TestEdgeAuthMiddleware_OperatedBy_AttributedFromOperator(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := signEdge(t, "alice@example.com", "audit-1", "/v1/beads", ts)

	cfg := edgeauth.Config{
		HMACKey: []byte(testEdgeAuthKey),
		Now:     func() time.Time { return now },
	}
	var gotOperatedBy string
	trusted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOperatedBy = operatedByHeaderFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := EdgeAuthMiddleware(cfg, trusted, http.NotFoundHandler())

	r := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	r.Header.Set(edgeauth.HeaderOperator, "alice@example.com")
	r.Header.Set(edgeauth.HeaderAuditID, "audit-1")
	r.Header.Set(edgeauth.HeaderTimestamp, ts)
	r.Header.Set(edgeauth.HeaderSignature, sig)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if gotOperatedBy != "oidc:alice@example.com" {
		t.Errorf("OperatedBy = %q, want oidc:alice@example.com", gotOperatedBy)
	}
}

// TestEdgeAuthMiddleware_VerifiedV1KeepsMutationAuthority: a cryptographically
// VERIFIED v1 envelope (the shape PAT / public-AVA / generic-cookie /
// Slack-bridge callers produce) still reaches trustedHandler under Enforce, as
// authMethodEdge. Since gd-AvSKOUyqRz retired decision ka-0000001641 that
// method is non-role-bearing like the static token: operator-level mutations
// pass, RoleAdmin routes refuse it (TestEdgeAuthMiddleware_
// VerifiedV1RefusedOnAdminRoute).
func TestEdgeAuthMiddleware_VerifiedV1KeepsMutationAuthority(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := signEdge(t, "alice@example.com", "audit-1", "/v1/beads", ts)
	called := false
	var gotMethod authMethod
	var gotID AuthIdentity
	h := EdgeAuthMiddleware(
		edgeauth.Config{
			HMACKey: []byte(testEdgeAuthKey),
			Enforce: true, // live prod posture
			Now:     func() time.Time { return now },
		},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			gotMethod = authMethodFromContext(r.Context())
			gotID = AuthIdentityFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}),
		http.NotFoundHandler(),
	)

	r := httptest.NewRequest(http.MethodPost, "/v1/beads", nil)
	r.Header.Set(edgeauth.HeaderOperator, "alice@example.com")
	r.Header.Set(edgeauth.HeaderAuditID, "audit-1")
	r.Header.Set(edgeauth.HeaderTimestamp, ts)
	r.Header.Set(edgeauth.HeaderSignature, sig)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (verified v1 keeps pre-MR authority); body=%s", w.Code, w.Body.String())
	}
	if !called {
		t.Fatal("verified v1 mutation did not reach trustedHandler")
	}
	if gotMethod != authMethodEdge {
		t.Errorf("auth method=%q, want %q (verified v1)", gotMethod, authMethodEdge)
	}
	if gotID.Type != "edge" || gotID.Subject != "alice@example.com" {
		t.Errorf("AuthIdentity=%+v, want {Type:edge Subject:alice@example.com}", gotID)
	}
}

// TestEdgeAuthMiddleware_UnsignedEnvelopeRejectedUnderEnforce is the negative
// pair: with enforcement on, an X-Operator lacking a valid signature must NOT
// receive v1 (or any) mutation authority.
func TestEdgeAuthMiddleware_UnsignedEnvelopeRejectedUnderEnforce(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	called := false
	h := EdgeAuthMiddleware(
		edgeauth.Config{
			HMACKey: []byte(testEdgeAuthKey),
			Enforce: true,
			Now:     func() time.Time { return now },
		},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		}),
		http.NotFoundHandler(),
	)

	r := httptest.NewRequest(http.MethodPost, "/v1/beads", nil)
	r.Header.Set(edgeauth.HeaderOperator, "alice@example.com")
	r.Header.Set(edgeauth.HeaderAuditID, "audit-1")
	r.Header.Set(edgeauth.HeaderTimestamp, ts)
	// No signature header at all.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401 (unsigned envelope must not mutate)", w.Code)
	}
	if called {
		t.Fatal("unsigned envelope reached mutation handler")
	}
}

// TestEdgeAuthMiddleware_BurnInV1CannotMutate holds the operator invariant
// "burn-in-trusted invalid envelopes must not receive legacy mutation
// authority": with Enforce=false a badly-signed v1 envelope is Trusted for
// attribution but must be pinned to viewer (authMethodOIDC) and denied a
// mutation — it must NOT be granted authMethodEdge.
func TestEdgeAuthMiddleware_BurnInV1CannotMutate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	created := false
	// Wrap in the operator floor exactly as a native mutation route would.
	operatorRoute := requireRole(RoleOperator, func(w http.ResponseWriter, _ *http.Request) {
		created = true
		w.WriteHeader(http.StatusNoContent)
	})
	h := EdgeAuthMiddleware(
		edgeauth.Config{
			HMACKey: []byte(testEdgeAuthKey),
			Enforce: false, // burn-in
			Now:     func() time.Time { return now },
		},
		operatorRoute,
		http.NotFoundHandler(),
	)

	r := httptest.NewRequest(http.MethodPost, "/v1/beads", nil)
	r.Header.Set(edgeauth.HeaderOperator, "alice@example.com")
	r.Header.Set(edgeauth.HeaderAuditID, "audit-1")
	r.Header.Set(edgeauth.HeaderTimestamp, ts)
	r.Header.Set(edgeauth.HeaderSignature, base64.StdEncoding.EncodeToString([]byte("garbage")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (burn-in invalid v1 must not mutate)", w.Code)
	}
	if created {
		t.Fatal("burn-in trusted invalid v1 reached mutation handler")
	}
}

func TestEdgeAuthMiddleware_RefreshExchangeReachesTokenBoundary(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	const path = "/v1/auth/refresh"
	called := false
	h := EdgeAuthMiddleware(
		edgeauth.Config{
			HMACKey: []byte(testEdgeAuthKey),
			Now:     func() time.Time { return now },
		},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			if got := AuthIdentityFromContext(r.Context()).Subject; got != "alice@example.com" {
				t.Errorf("subject=%q, want alice@example.com", got)
			}
			// The real refresh handler validates the plaintext refresh token.
			// This distinct response proves the global role floor did not
			// intercept the exchange first.
			w.WriteHeader(http.StatusUnauthorized)
		}),
		http.NotFoundHandler(),
	)

	for _, tc := range []struct {
		name string
		v2   bool
	}{
		{name: "legacy v1 envelope"},
		{name: "v2 viewer envelope", v2: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			r := httptest.NewRequest(http.MethodPost, path, nil)
			if tc.v2 {
				sig := signEdgeV2(
					t,
					"alice@example.com",
					"cookie",
					RoleViewer,
					"audit-refresh",
					http.MethodPost,
					path,
					ts,
				)
				setV2EdgeHeaders(
					t,
					r,
					"alice@example.com",
					"cookie",
					RoleViewer,
					"audit-refresh",
					ts,
					sig,
				)
			} else {
				r.Header.Set(edgeauth.HeaderOperator, "alice@example.com")
				r.Header.Set(edgeauth.HeaderAuditID, "audit-refresh")
				r.Header.Set(edgeauth.HeaderTimestamp, ts)
				r.Header.Set(
					edgeauth.HeaderSignature,
					signEdge(t, "alice@example.com", "audit-refresh", path, ts),
				)
			}

			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d, want handler response 401; body=%s", w.Code, w.Body.String())
			}
			if !called {
				t.Fatal("refresh handler did not receive edge-authenticated request")
			}
		})
	}
}

func TestEdgeAuthMiddleware_V2ViewerDeniedAndOperatorAllowed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	var gotMethod authMethod
	var gotRoles []string
	operatorRoute := requireRole(RoleOperator, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = authMethodFromContext(r.Context())
		gotRoles = RolesFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	h := EdgeAuthMiddleware(
		edgeauth.Config{
			HMACKey: []byte(testEdgeAuthKey),
			Now:     func() time.Time { return now },
		},
		operatorRoute,
		http.NotFoundHandler(),
	)

	for _, tc := range []struct {
		name       string
		roles      string
		wantStatus int
	}{
		{name: "viewer", roles: RoleViewer, wantStatus: http.StatusForbidden},
		{name: "operator", roles: RoleOperator, wantStatus: http.StatusNoContent},
		{name: "admin", roles: RoleAdmin, wantStatus: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sig := signEdgeV2(
				t,
				"alice@example.com",
				"entra",
				tc.roles,
				"audit-2",
				http.MethodPost,
				"/v1/beads",
				ts,
			)
			r := httptest.NewRequest(http.MethodPost, "/v1/beads", nil)
			setV2EdgeHeaders(t, r, "alice@example.com", "entra", tc.roles, "audit-2", ts, sig)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.wantStatus {
				t.Fatalf("status=%d, want %d; body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantStatus < 300 {
				if gotMethod != authMethodOIDC {
					t.Errorf("auth method=%q, want oidc-equivalent", gotMethod)
				}
				if strings.Join(gotRoles, ",") != tc.roles {
					t.Errorf("roles=%v, want %s", gotRoles, tc.roles)
				}
				if got := w.Header().Get(edgeauth.HeaderAuditID); got != "audit-2" {
					t.Errorf("response audit id=%q, want audit-2", got)
				}
			}
		})
	}
}

// TestEdgeAuthMiddleware_TamperedV2RejectedUnderEnforce holds the operator
// invariant "malformed or invalid v2 must never downgrade to v1": a request
// carrying X-Edge-Auth-Version:2 whose v2 signature is forged must be REJECTED
// under enforcement, never silently re-verified as a v1 envelope that would
// carry pre-MR mutation authority.
func TestEdgeAuthMiddleware_TamperedV2RejectedUnderEnforce(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	r := httptest.NewRequest(http.MethodPost, "/v1/beads", nil)
	// A well-formed v1 signature is ALSO present (setV2EdgeHeaders stamps it),
	// so this proves the daemon does not fall back to it when v2 fails.
	setV2EdgeHeaders(
		t,
		r,
		"alice@example.com",
		"entra",
		RoleAdmin,
		"audit-3",
		ts,
		base64.StdEncoding.EncodeToString([]byte("forged")),
	)
	called := false
	h := EdgeAuthMiddleware(
		edgeauth.Config{
			HMACKey: []byte(testEdgeAuthKey),
			Enforce: true,
			Now:     func() time.Time { return now },
		},
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }),
		http.NotFoundHandler(),
	)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401 (tampered v2 rejected, no v1 downgrade)", w.Code)
	}
	if called {
		t.Fatal("tampered v2 reached mutation handler")
	}
}

func TestEdgeAuthMiddleware_TamperedV2CannotMutateDuringBurnIn(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	r := httptest.NewRequest(http.MethodPost, "/v1/beads", nil)
	setV2EdgeHeaders(
		t,
		r,
		"alice@example.com",
		"entra",
		RoleAdmin,
		"audit-3",
		ts,
		base64.StdEncoding.EncodeToString([]byte("forged")),
	)
	called := false
	h := EdgeAuthMiddleware(
		edgeauth.Config{
			HMACKey: []byte(testEdgeAuthKey),
			Enforce: false,
			Now:     func() time.Time { return now },
		},
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }),
		http.NotFoundHandler(),
	)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", w.Code)
	}
	if called {
		t.Fatal("burn-in trusted forged v2 reached mutation handler")
	}
}
