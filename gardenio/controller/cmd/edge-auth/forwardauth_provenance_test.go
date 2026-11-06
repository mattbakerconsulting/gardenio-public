// Tests for the forwardauth identity-provenance envelope edge-auth stamps on
// its /forwardauth/verify success responses (task ka-0000004151). An upstream
// served behind this middleware (the scif approval portal) verifies the
// envelope before trusting the X-Auth-Request-* identity, so a caller that
// bypasses Traefik and forges X-Auth-Request-Email cannot name itself the
// approver. These tests assert the envelope is emitted on every success path,
// round-trips through the real verifier, and is ABSENT with no key / on denial.

package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/controller/internal/edgeauth"
)

// fixedTestTime is the clock the proxy test fixtures pin (see proxy_test.go).
var fixedTestTime = time.Unix(1_700_000_000, 0)

// assertProvenanceRoundTrips reads the identity + envelope headers off a
// forwardauth success response and asserts the real upstream verifier trusts
// them in ENFORCE mode — i.e. the signature edge-auth stamped verifies against
// the shared key over the exact identity it emitted.
func assertProvenanceRoundTrips(t *testing.T, resp *http.Response, key string) {
	t.Helper()
	sig := resp.Header.Get(edgeauth.ForwardAuthHeaderSig)
	ts := resp.Header.Get(edgeauth.ForwardAuthHeaderTs)
	if sig == "" || ts == "" {
		t.Fatalf("provenance envelope missing: sig=%q ts=%q", sig, ts)
	}
	// Rebuild the upstream request as Traefik would: copy the auth response's
	// X-Auth-Request-* + audit + envelope headers onto a fresh request.
	up := httptest.NewRequest("GET", "/scif/approve", nil)
	for _, h := range []string{
		edgeauth.ForwardAuthHeaderEmail,
		edgeauth.ForwardAuthHeaderGroups,
		edgeauth.ForwardAuthHeaderAuditID,
		edgeauth.ForwardAuthHeaderSig,
		edgeauth.ForwardAuthHeaderTs,
	} {
		if v := resp.Header.Get(h); v != "" {
			up.Header.Set(h, v)
		}
	}
	verifier := edgeauth.ForwardAuthVerifyConfig{
		HMACKey: []byte(key),
		Enforce: true,
		Now:     func() time.Time { return fixedTestTime },
	}
	got := verifier.Verify(up)
	if got.Outcome != edgeauth.FwdOutcomeOK || !got.Trusted {
		t.Fatalf("verifier rejected the stamped envelope: outcome=%q trusted=%v", got.Outcome, got.Trusted)
	}
	if got.Email != resp.Header.Get(edgeauth.ForwardAuthHeaderEmail) {
		t.Fatalf("verifier email=%q, want %q", got.Email, resp.Header.Get(edgeauth.ForwardAuthHeaderEmail))
	}
}

// TestForwardauth_AVA_StampsProvenanceEnvelope: the AVA-JWT success path emits a
// provenance envelope that round-trips through the verifier (groups present).
func TestForwardauth_AVA_StampsProvenanceEnvelope(t *testing.T) {
	verifier := &stubVerifier{id: avaIdentity{
		Email:  "ava-user@acme.example",
		Groups: []string{"gardenio", "acme-admins"},
	}}
	base, _, cleanup := newForwardauthTestShim(t, verifier)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/forwardauth/verify", nil)
	req.Header.Set("X-Amzn-Ava-User-Context", "valid-jwt")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d, want 202", resp.StatusCode)
	}
	assertProvenanceRoundTrips(t, resp, "test-hmac-key")
}

// TestForwardauth_PAT_StampsProvenanceEnvelope: the PAT success path stamps an
// envelope too (single-group identity).
func TestForwardauth_PAT_StampsProvenanceEnvelope(t *testing.T) {
	base, _, cleanup := newForwardauthTestShim(t, nil)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/forwardauth/verify", nil)
	req.Header.Set("Authorization", "Bearer good-pat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d, want 202", resp.StatusCode)
	}
	assertProvenanceRoundTrips(t, resp, "test-hmac-key")
}

// TestForwardauth_EmptyGroups_ProvenanceRoundTrips: with no groups header
// emitted, the signature is over groups="" and must still verify — the sign and
// verify sides both use the raw (absent => "") header value.
func TestForwardauth_EmptyGroups_ProvenanceRoundTrips(t *testing.T) {
	verifier := &stubVerifier{id: avaIdentity{Email: "ava-user@acme.example", Groups: nil}}
	base, _, cleanup := newForwardauthTestShim(t, verifier)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/forwardauth/verify", nil)
	req.Header.Set("X-Amzn-Ava-User-Context", "valid-jwt")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d, want 202", resp.StatusCode)
	}
	if _, has := resp.Header[edgeauth.ForwardAuthHeaderGroups]; has {
		t.Fatalf("groups header should be omitted for an empty group set")
	}
	assertProvenanceRoundTrips(t, resp, "test-hmac-key")
}

// TestForwardauth_Denied_NoProvenanceEnvelope: a denied request (401/403/404)
// must never stamp a provenance envelope — there is no verified identity to
// vouch for, and a stray envelope on an error response could confuse an
// upstream that reads headers before status.
func TestForwardauth_Denied_NoProvenanceEnvelope(t *testing.T) {
	cases := []struct {
		name     string
		verifier avaVerifier
		auth     string
		ava      string
	}{
		{"invalid-ava", &stubVerifier{err: errAVAInvalid}, "", "tampered.jwt"},
		{"missing-token", &stubVerifier{id: avaIdentity{Email: "x@y"}}, "", ""},
		{"pat-not-in-group", nil, "Bearer notin-group-pat", ""},
		{"pat-invalid", nil, "Bearer nope", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, _, cleanup := newForwardauthTestShim(t, tc.verifier)
			defer cleanup()

			req, _ := http.NewRequest("GET", base+"/forwardauth/verify", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			if tc.ava != "" {
				req.Header.Set("X-Amzn-Ava-User-Context", tc.ava)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusAccepted {
				t.Fatalf("expected a denial, got 202")
			}
			if v := resp.Header.Get(edgeauth.ForwardAuthHeaderSig); v != "" {
				t.Errorf("denied request stamped %s=%q, want empty", edgeauth.ForwardAuthHeaderSig, v)
			}
			if v := resp.Header.Get(edgeauth.ForwardAuthHeaderTs); v != "" {
				t.Errorf("denied request stamped %s=%q, want empty", edgeauth.ForwardAuthHeaderTs, v)
			}
		})
	}
}

// TestStampForwardAuthProvenance_NoKey_NoEnvelope: with no HMAC key configured
// (edgeAuth.dryRun / unprovisioned secret) the stamper emits nothing — the
// dark-ship posture that lets the pod run before the shared key exists, which
// the upstream verifier's burn-in tolerates.
func TestStampForwardAuthProvenance_NoKey_NoEnvelope(t *testing.T) {
	srv := &server{
		cfg:    &config{hmacKey: ""},
		logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		now:    func() time.Time { return fixedTestTime },
	}
	rec := httptest.NewRecorder()
	srv.stampForwardAuthProvenance(rec, "a@b.c", "grp", "aud")
	if v := rec.Header().Get(edgeauth.ForwardAuthHeaderSig); v != "" {
		t.Errorf("no-key stamp emitted %s=%q, want empty", edgeauth.ForwardAuthHeaderSig, v)
	}
	if v := rec.Header().Get(edgeauth.ForwardAuthHeaderTs); v != "" {
		t.Errorf("no-key stamp emitted %s=%q, want empty", edgeauth.ForwardAuthHeaderTs, v)
	}
}
