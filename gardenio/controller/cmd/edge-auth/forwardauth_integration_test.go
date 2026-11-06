// Integration tests for the Traefik forwardAuth-compatible endpoint
// (handleForwardauth). Companion to ava_integration_test.go which covers
// the PAT/AVA fork of handleProxy; this file covers the standalone
// /forwardauth/verify path used by coopmux's Traefik IngressRoute to
// retire oauth2-proxy → Keycloak (epics ka-0000004294, ka-0000005862,
// ka-0000004118).
//
// Test strategy mirrors ava_integration_test.go: we inject a stub
// avaVerifier onto the live server rather than standing up a JWKS
// server. The real verifier wiring is exercised in ava_integration_test.go
// already.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
)

// newForwardauthTestShim builds an edge-auth server with both PAT and
// AVA wiring identical to newAVATestShim, then returns the request
// builder + audit buffer + cleanup hook. Tests inject a stub verifier
// (or nil) onto server.avaVerifier to drive each branch of
// handleForwardauth.
func newForwardauthTestShim(t *testing.T, ava avaVerifier) (string, *bytes.Buffer, func()) {
	t.Helper()
	base, _, cleanup := newProxyTestServerWithCapture(t, "test-hmac-key")
	cleanup.s.avaVerifier = ava
	return base, cleanup.buf, cleanup.close
}

// TestForwardauth_VerifiedJWT_202_StampsHeaders is the happy path.
// Verifier returns an identity → 202 + the 3 X-Auth-Request-* headers +
// X-Auth-Audit-Id. Traefik's forwardAuth middleware reads these off the
// response and stamps them onto the request before forwarding upstream.
func TestForwardauth_VerifiedJWT_202_StampsHeaders(t *testing.T) {
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
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s, want 202", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Auth-Request-User"); got != "ava-user@acme.example" {
		t.Errorf("X-Auth-Request-User=%q, want ava-user@acme.example", got)
	}
	if got := resp.Header.Get("X-Auth-Request-Email"); got != "ava-user@acme.example" {
		t.Errorf("X-Auth-Request-Email=%q, want ava-user@acme.example", got)
	}
	if got := resp.Header.Get("X-Auth-Request-Groups"); got != "gardenio,acme-admins" {
		t.Errorf("X-Auth-Request-Groups=%q, want gardenio,acme-admins", got)
	}
	if resp.Header.Get("X-Auth-Audit-Id") == "" {
		t.Error("X-Auth-Audit-Id missing on forwardauth 202")
	}
}

// TestForwardauth_NoAVAHeader_401 covers a bare request: no
// X-Amzn-Ava-User-Context. The endpoint 401s with missing_ava_token and
// emits no X-Auth-Request-* headers — Traefik must not see a partial
// identity claim.
func TestForwardauth_NoAVAHeader_401(t *testing.T) {
	verifier := &stubVerifier{id: avaIdentity{Email: "would-not-be-called@acme.example"}}
	base, buf, cleanup := newForwardauthTestShim(t, verifier)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/forwardauth/verify", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]string
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("response is not JSON: %q", body)
	}
	if got := parsed["error"]; got != "missing_ava_token" {
		t.Errorf("error=%q, want missing_ava_token", got)
	}
	for _, h := range []string{"X-Auth-Request-User", "X-Auth-Request-Email", "X-Auth-Request-Groups"} {
		if v := resp.Header.Get(h); v != "" {
			t.Errorf("%s=%q on 401, want empty", h, v)
		}
	}
	rec := decodeAudit(t, buf)
	if len(rec) != 1 {
		t.Fatalf("len(records)=%d", len(rec))
	}
	if rec[0].StatusDetail != auditDetailDenyMissingAVAHeader {
		t.Errorf("status_detail=%q, want %q", rec[0].StatusDetail, auditDetailDenyMissingAVAHeader)
	}
}

// TestForwardauth_InvalidJWT_401 covers the verifier rejecting the
// token (bad signature, expired, signer mismatch, …). 401 +
// invalid_ava_token and no auth headers leak through.
func TestForwardauth_InvalidJWT_401(t *testing.T) {
	verifier := &stubVerifier{err: errAVAInvalid}
	base, buf, cleanup := newForwardauthTestShim(t, verifier)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/forwardauth/verify", nil)
	req.Header.Set("X-Amzn-Ava-User-Context", "tampered.jwt.bytes")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]string
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("response is not JSON: %q", body)
	}
	if got := parsed["error"]; got != "invalid_ava_token" {
		t.Errorf("error=%q, want invalid_ava_token", got)
	}
	for _, h := range []string{"X-Auth-Request-User", "X-Auth-Request-Email", "X-Auth-Request-Groups"} {
		if v := resp.Header.Get(h); v != "" {
			t.Errorf("%s=%q on invalid-JWT 401, want empty", h, v)
		}
	}
	rec := decodeAudit(t, buf)
	if len(rec) != 1 {
		t.Fatalf("len(records)=%d", len(rec))
	}
	if rec[0].StatusDetail != auditDetailDenyInvalidAVA {
		t.Errorf("status_detail=%q, want %q", rec[0].StatusDetail, auditDetailDenyInvalidAVA)
	}
	if rec[0].Metadata.AuthMode != auditAuthModeAVA {
		t.Errorf("auth_mode=%q, want %q", rec[0].Metadata.AuthMode, auditAuthModeAVA)
	}
}

// TestForwardauth_VerifierNil_404 covers the dark-ship guard: when
// avaTrustSigner was not set, the verifier is nil and the endpoint
// 404s with forwardauth_not_configured. Traefik surfaces this to the
// browser, so the misconfiguration is loud rather than silent.
func TestForwardauth_VerifierNil_404(t *testing.T) {
	base, buf, cleanup := newForwardauthTestShim(t, nil /* AVA off */)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/forwardauth/verify", nil)
	req.Header.Set("X-Amzn-Ava-User-Context", "would-be-jwt")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]string
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("response is not JSON: %q", body)
	}
	if got := parsed["error"]; got != "forwardauth_not_configured" {
		t.Errorf("error=%q, want forwardauth_not_configured", got)
	}
	rec := decodeAudit(t, buf)
	if len(rec) != 1 {
		t.Fatalf("len(records)=%d", len(rec))
	}
	if rec[0].StatusDetail != auditDetailDenyForwardauthOff {
		t.Errorf("status_detail=%q, want %q", rec[0].StatusDetail, auditDetailDenyForwardauthOff)
	}
}

// TestForwardauth_EmptyEmailFromVerifier_401 is defence in depth: a
// verifier that accepts a token but returns an empty Email field must
// not produce a 202 — otherwise Traefik would forward
// X-Auth-Request-Email="" to coopmux and credit an unauthenticated
// principal.
func TestForwardauth_EmptyEmailFromVerifier_401(t *testing.T) {
	verifier := &stubVerifier{id: avaIdentity{Email: ""}}
	base, _, cleanup := newForwardauthTestShim(t, verifier)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/forwardauth/verify", nil)
	req.Header.Set("X-Amzn-Ava-User-Context", "valid-sig-but-no-email")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]string
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("response is not JSON: %q", body)
	}
	if got := parsed["error"]; got != "invalid_ava_token" {
		t.Errorf("error=%q, want invalid_ava_token", got)
	}
	if v := resp.Header.Get("X-Auth-Request-Email"); v != "" {
		t.Errorf("X-Auth-Request-Email=%q on DiD 401, want empty", v)
	}
}

// TestForwardauth_EmptyGroups_NoGroupsHeader matches the oauth2-proxy
// convention: when the verified identity carries no groups, omit
// X-Auth-Request-Groups entirely rather than emit a blank value. Coopmux
// cannot distinguish "no groups" from "header dropped" otherwise.
func TestForwardauth_EmptyGroups_NoGroupsHeader(t *testing.T) {
	verifier := &stubVerifier{id: avaIdentity{
		Email:  "ava-user@acme.example",
		Groups: nil, // verifier returned no groups
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
	if _, has := resp.Header["X-Auth-Request-Groups"]; has {
		t.Errorf("X-Auth-Request-Groups was emitted with empty Groups slice: %q",
			resp.Header.Get("X-Auth-Request-Groups"))
	}
	// User + Email must still be stamped.
	if got := resp.Header.Get("X-Auth-Request-Email"); got != "ava-user@acme.example" {
		t.Errorf("X-Auth-Request-Email=%q, want ava-user@acme.example", got)
	}

	// Also defend against the "only whitespace" case — joinNonEmptyGroups
	// must collapse it to the empty header-omission case.
	verifier2 := &stubVerifier{id: avaIdentity{
		Email:  "ava-user@acme.example",
		Groups: []string{"", "   ", ""},
	}}
	base2, _, cleanup2 := newForwardauthTestShim(t, verifier2)
	defer cleanup2()
	req2, _ := http.NewRequest("GET", base2+"/forwardauth/verify", nil)
	req2.Header.Set("X-Amzn-Ava-User-Context", "valid-jwt")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if _, has := resp2.Header["X-Auth-Request-Groups"]; has {
		t.Errorf("X-Auth-Request-Groups was emitted with whitespace-only Groups: %q",
			resp2.Header.Get("X-Auth-Request-Groups"))
	}
}

// TestForwardauth_AuditRecordStampedWithEndpointForwardauth pins the
// new auditEndpointForwardauth constant on every record handleForwardauth
// emits — happy path and failure. Dashboards that pivot on
// metadata.endpoint to separate kbeads CLI flow from coopmux web flow
// depend on this.
func TestForwardauth_AuditRecordStampedWithEndpointForwardauth(t *testing.T) {
	cases := []struct {
		name     string
		verifier avaVerifier
		header   string
		want     string
	}{
		{"happy", &stubVerifier{id: avaIdentity{Email: "ava-user@acme.example"}}, "valid-jwt", auditDetailOK},
		{"invalid", &stubVerifier{err: errAVAInvalid}, "tampered.jwt", auditDetailDenyInvalidAVA},
		{"missing-header", &stubVerifier{id: avaIdentity{Email: "x@y"}}, "", auditDetailDenyMissingAVAHeader},
		{"verifier-off", nil, "anything", auditDetailDenyForwardauthOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, buf, cleanup := newForwardauthTestShim(t, tc.verifier)
			defer cleanup()

			req, _ := http.NewRequest("GET", base+"/forwardauth/verify", nil)
			if tc.header != "" {
				req.Header.Set("X-Amzn-Ava-User-Context", tc.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			rec := decodeAudit(t, buf)
			if len(rec) != 1 {
				t.Fatalf("len(records)=%d, want 1", len(rec))
			}
			if rec[0].Metadata.Endpoint != auditEndpointForwardauth {
				t.Errorf("metadata.endpoint=%q, want %q", rec[0].Metadata.Endpoint, auditEndpointForwardauth)
			}
			if rec[0].Metadata.AuthMode != auditAuthModeAVA {
				t.Errorf("auth_mode=%q, want %q", rec[0].Metadata.AuthMode, auditAuthModeAVA)
			}
			if rec[0].StatusDetail != tc.want {
				t.Errorf("status_detail=%q, want %q", rec[0].StatusDetail, tc.want)
			}
		})
	}
}

// TestForwardauth_VerifierUnavailable_502 covers a verifier that
// returns a non-sentinel error (e.g. JWKS-fetch timeout). The endpoint
// emits 502 verifier_unavailable so Traefik / the client know to retry,
// rather than 401 which would imply the token itself was bad.
func TestForwardauth_VerifierUnavailable_502(t *testing.T) {
	verifier := &stubVerifier{err: errors.New("transient: jwks fetch timeout")}
	base, buf, cleanup := newForwardauthTestShim(t, verifier)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/forwardauth/verify", nil)
	req.Header.Set("X-Amzn-Ava-User-Context", "valid-looking-jwt")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status=%d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]string
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("response is not JSON: %q", body)
	}
	if got := parsed["error"]; got != "verifier_unavailable" {
		t.Errorf("error=%q, want verifier_unavailable", got)
	}
	rec := decodeAudit(t, buf)
	if len(rec) != 1 {
		t.Fatalf("len(records)=%d", len(rec))
	}
	if rec[0].StatusDetail != auditDetailDenyVerifierUnavailable {
		t.Errorf("status_detail=%q, want %q", rec[0].StatusDetail, auditDetailDenyVerifierUnavailable)
	}
}

// TestJoinNonEmptyGroups exercises the small comma-joiner directly. Edge
// cases: nil slice, all-empty, mixed empty + non-empty, whitespace-only
// elements (trimmed).
func TestJoinNonEmptyGroups(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{""}, ""},
		{[]string{"   "}, ""},
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a,b"},
		{[]string{"", "a", ""}, "a"},
		{[]string{" a ", "b"}, "a,b"},
	}
	for _, tc := range cases {
		if got := joinNonEmptyGroups(tc.in); got != tc.want {
			t.Errorf("joinNonEmptyGroups(%v)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- CLI path: GitLab PAT bearer at the forwardauth endpoint (ka-0000004118) ---
// A Bearer PAT authenticates programmatic clients at the AVA host without the
// browser OIDC flow, emitting the same X-Auth-Request-* headers as the AVA-JWT
// path. avaVerifier is nil in these tests to prove the PAT branch is independent
// of AVA wiring (and takes precedence — a Bearer request is a CLI).

func TestForwardauth_PAT_202_StampsHeaders(t *testing.T) {
	base, _, cleanup := newForwardauthTestShim(t, nil)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/forwardauth/verify", nil)
	req.Header.Set("Authorization", "Bearer good-pat") // → alice@example.com, member of gardenio
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s, want 202", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Auth-Request-User"); got != "alice@example.com" {
		t.Errorf("X-Auth-Request-User=%q, want alice@example.com", got)
	}
	if got := resp.Header.Get("X-Auth-Request-Email"); got != "alice@example.com" {
		t.Errorf("X-Auth-Request-Email=%q, want alice@example.com", got)
	}
	if got := resp.Header.Get("X-Auth-Request-Groups"); got != "gardenio" {
		t.Errorf("X-Auth-Request-Groups=%q, want gardenio (the verified membership)", got)
	}
	if resp.Header.Get("X-Auth-Audit-Id") == "" {
		t.Error("X-Auth-Audit-Id missing on forwardauth PAT 202")
	}
}

func TestForwardauth_PAT_NotInGroup_403(t *testing.T) {
	base, _, cleanup := newForwardauthTestShim(t, nil)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/forwardauth/verify", nil)
	req.Header.Set("Authorization", "Bearer notin-group-pat") // valid user, not a member
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s, want 403", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Auth-Request-Email") != "" {
		t.Error("denied PAT must not stamp X-Auth-Request-Email")
	}
}

func TestForwardauth_PAT_InvalidToken_401(t *testing.T) {
	base, _, cleanup := newForwardauthTestShim(t, nil)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/forwardauth/verify", nil)
	req.Header.Set("Authorization", "Bearer nope")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s, want 401", resp.StatusCode, body)
	}
}

// TestForwardauth_PAT_PrecedesAVA proves a Bearer PAT wins over a present AVA
// JWT header — a request carrying a PAT is unambiguously a CLI, so we attribute
// it to the PAT identity (alice) not the AVA identity.
func TestForwardauth_PAT_PrecedesAVA(t *testing.T) {
	verifier := &stubVerifier{id: avaIdentity{Email: "ava-user@acme.example"}}
	base, _, cleanup := newForwardauthTestShim(t, verifier)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/forwardauth/verify", nil)
	req.Header.Set("Authorization", "Bearer good-pat")
	req.Header.Set("X-Amzn-Ava-User-Context", "valid-jwt")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d, want 202", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Auth-Request-Email"); got != "alice@example.com" {
		t.Errorf("X-Auth-Request-Email=%q, want alice@example.com (PAT wins over AVA)", got)
	}
}
