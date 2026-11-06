package oidcauth

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestForgedEmailHeader_AVAEdge_DefeatedByContext is the end-to-end security
// regression for ka-0000002979: on the AWS Verified Access edge the raw
// X-Auth-Request-Email header is CLIENT-SUPPLIED and unstripped, so a handler
// that trusts it over the verified context forges the caller's identity.
//
// The test drives the REAL AVA middleware (New → authBranch → ES384 JWT
// verification) with a legitimately-signed AVA token for alice@acme.example AND
// a forged `X-Auth-Request-Email: attacker@evil.example` header on the same
// request, then asserts EmailFromContext (the verified-only seam every handler
// reads) returns the AVA-verified alice — the forgery is defeated. The pre-fix
// helper that let the header win, EmailFromRequest, is gone (gd-Fhizmbk9Ee).
func TestForgedEmailHeader_AVAEdge_DefeatedByContext(t *testing.T) {
	const (
		verified = "alice@acme.example"
		forged   = "attacker@evil.example"
	)

	f := newAVAFixture(t)
	cfg := Config{
		// Keycloak disabled → the AVA branch is the whole middleware. A request
		// carrying a valid AVA header is verified; the AVA-derived email lands
		// in ctxEmail via authBranch.
		AVA: AVAConfig{
			TrustSigner:    testTrustSigner,
			Region:         "test-region",
			publicKeyURLFn: func(_ string, kid string) string { return f.jwksSrv.URL + "/" + kid },
			httpClient:     f.jwksSrv.Client(),
		},
	}
	h := New(t.Context(), cfg, slog.Default())

	var gotContext string
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotContext = EmailFromContext(r.Context())
	})

	req := httptest.NewRequest(http.MethodPost, "/b/gd-1/owner", nil)
	req.Header.Set(AVAHeaderName, f.signAVA(t, nil, validPayload(t, verified)))
	// The attack: a browser on the AVA edge appends its own X-Auth-Request-Email.
	// AVA does not strip it, so it reaches the pod verbatim.
	req.Header.Set("X-Auth-Request-Email", forged)

	rec := httptest.NewRecorder()
	h.Middleware(inner).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("AVA request was not admitted: status %d", rec.Code)
	}
	// The fix: verified context wins, forged header is ignored.
	if gotContext != verified {
		t.Errorf("EmailFromContext = %q, want %q (forged header must not override the AVA-verified identity)", gotContext, verified)
	}
}
