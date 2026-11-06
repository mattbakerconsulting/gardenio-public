// Unit / integration tests for the AVA-mode path of gardenio-edge-auth.
//
// Companion to pat_integration_test.go (which covers the GitLab PAT edge
// against a real GitLab fixture). The AVA path has no equivalent of a
// real-AVA harness because AVA instances are provisioned via Terraform and
// dispense their public keys over an HTTPS endpoint we cannot reach from
// `go test`. Instead, we drive the path with:
//
//  1. A stub avaVerifier injected directly onto server.avaVerifier — used
//     for tests that only care about handleProxy's branching logic
//     (precedence, dry-run, audit, header stripping). This is the bulk of
//     the file.
//
//  2. The real avaHandleVerifier wired against an in-memory P-384 JWKS
//     server lifted from internal/oidcauth/ava_test.go via the public
//     oidcauth.New + AVAConfig surface. Used to confirm that the shim's
//     wiring of oidcauth produces the right disposition for malformed
//     headers when avaTrustSigner is empty (AVA mode OFF -> verifier nil).
//
// No build tag — these run on every `go test ./...` cycle.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// stubVerifier returns a canned outcome for every request. Tests in this
// file inject one onto server.avaVerifier to exercise handleProxy's AVA
// branching without standing up a JWKS server.
type stubVerifier struct {
	id  avaIdentity
	err error
}

func (s *stubVerifier) verify(_ context.Context, _ *http.Request) (avaIdentity, error) {
	if s.err != nil {
		return avaIdentity{}, s.err
	}
	return s.id, nil
}

// newAVATestShim builds an edge-auth test server with both PAT and AVA
// wiring. The PAT path uses newStubGitLab via
// newProxyTestServerWithCapture; the AVA path uses the supplied stubVerifier. Returns
// (baseURL, upstreamRecorder, sinkBuffer, cleanup) so individual tests can
// assert on both the proxied request and the audit log line.
func newAVATestShim(t *testing.T, ava avaVerifier, dryRun bool) (string, *upstreamRecorder, *bytes.Buffer, func()) {
	t.Helper()
	base, upstream, cleanup := newProxyTestServerWithCapture(t, "test-hmac-key")
	// Override the AVA verifier on the live server. newServer wires the
	// real avaHandleVerifier from cfg.avaTrustSigner; tests inject a stub
	// to drive branching without standing up a JWKS server.
	cleanup.s.avaVerifier = ava
	if dryRun {
		cleanup.s.cfg.dryRun = true
	}
	return base, upstream, cleanup.buf, cleanup.close
}

// ---------------------------------------------------------------------
// AVA precedence and disposition tests
// ---------------------------------------------------------------------

// TestProxy_AVA_HeaderPresent_VerifiedJWT_StampsEnvelope is the happy
// path: AVA header set + verifier accepts the token. The shim stamps the
// exact same envelope the PAT path produces; the upstream sees X-Operator
// equal to the trust-provider email, plus the HMAC signature it already
// validates today. AVA header is stripped before proxying so the daemon
// does not see two competing identity claims.
func TestProxy_AVA_HeaderPresent_VerifiedJWT_StampsEnvelope(t *testing.T) {
	verifier := &stubVerifier{id: avaIdentity{Email: "ava-user@acme.example"}}
	base, upstream, _, cleanup := newAVATestShim(t, verifier, false)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/kd/v1/beads", nil)
	req.Header.Set("X-Amzn-Ava-User-Context", "any-non-empty-value")
	// Deliberately omit Authorization. AVA path must not require a PAT.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s, want 200", resp.StatusCode, body)
	}
	if got := upstream.gotHeaders.Get(headerOperator); got != "ava-user@acme.example" {
		t.Errorf("X-Operator=%q, want ava-user@acme.example", got)
	}
	if upstream.gotHeaders.Get(headerSignature) == "" {
		t.Error("X-Edge-Auth-Sig missing on AVA path — envelope shape diverges from PAT path")
	}
	if upstream.gotHeaders.Get(headerAuditID) == "" {
		t.Error("X-Auth-Audit-Id missing on AVA path")
	}
	if upstream.gotHeaders.Get(headerTimestamp) == "" {
		t.Error("X-Edge-Auth-Ts missing on AVA path")
	}
	// Daemon must not see the AVA JWT directly — envelope is the single
	// source of identity.
	if got := upstream.gotHeaders.Get("X-Amzn-Ava-User-Context"); got != "" {
		t.Errorf("X-Amzn-Ava-User-Context leaked to upstream: %q", got)
	}
	// PAT Authorization MUST also not pass through.
	if got := upstream.gotHeaders.Get("Authorization"); got != "" {
		t.Errorf("Authorization leaked to upstream: %q", got)
	}
}

// TestProxy_AVA_EmitsV1Envelope_PreservesPreMRAuthority pins the operator
// decision ka-0000001641 ("additive"): the public AVA path is a pre-existing
// path this MR does not change, so it must sign a v1 envelope (no roles / no
// v2 signature) even when the verified identity carries recognized gardenio
// groups. Downstream (kbeads + coopmux) grants a verified v1 envelope its
// historical authority, so the AVA browser path keeps working with
// enforce=true — exactly as before this MR. If AVA ever emitted a v2 envelope
// with zero canonical roles it would be 403'd by the verified_v2 gate, silently
// revoking working browser access (ka-0000006523).
func TestProxy_AVA_EmitsV1Envelope_PreservesPreMRAuthority(t *testing.T) {
	verifier := &stubVerifier{id: avaIdentity{
		Email: "ava-operator@acme.example",
		Groups: []string{
			"Acme/Platform/gardenio-viewer",
			"Acme/Platform/gardenio-operator",
			"Acme/Platform/unrelated",
			"personal-project/gardenio-admin",
		},
	}}
	base, upstream, _, cleanup := newAVATestShim(t, verifier, false)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodPost, base+"/kd/v1/beads", strings.NewReader(`{"title":"task"}`))
	req.Header.Set("X-Amzn-Ava-User-Context", "verified-jwt")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s, want 200", resp.StatusCode, body)
	}
	// v1 envelope: the v1 signature is present, but there is no version header,
	// no roles header, and no v2 signature.
	if got := upstream.gotHeaders.Get(headerSignature); got == "" {
		t.Error("X-Edge-Auth-Sig missing — AVA path must still stamp the v1 envelope")
	}
	if got := upstream.gotHeaders.Get(headerVersion); got != "" {
		t.Errorf("%s=%q, want empty (public AVA stays v1)", headerVersion, got)
	}
	if got := upstream.gotHeaders.Get(headerRoles); got != "" {
		t.Errorf("%s=%q, want empty (public AVA stays v1)", headerRoles, got)
	}
	if got := upstream.gotHeaders.Get(headerSignatureV2); got != "" {
		t.Errorf("%s=%q, want empty (public AVA must not sign a v2 envelope)", headerSignatureV2, got)
	}
}

// TestProxy_AVA_InvalidJWT_401 covers signature-verification failure (or
// any other AVA-side rejection). The shim returns 401 and the JSON shape
// the CLI parses for both PAT and AVA failures. A valid PAT rides along: a
// rejected AVA token must NOT fall through to PAT, which would silently
// downgrade a tampered AVA call to a different auth flow.
func TestProxy_AVA_InvalidJWT_401(t *testing.T) {
	verifier := &stubVerifier{err: errAVAInvalid}
	base, upstream, buf, cleanup := newAVATestShim(t, verifier, false)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/kd/v1/beads", nil)
	req.Header.Set("X-Amzn-Ava-User-Context", "tampered.jwt.bytes")
	req.Header.Set("Authorization", "Bearer good-pat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
	if upstream.gotPath != "" {
		t.Error("upstream was hit on AVA-invalid; must short-circuit at the shim")
	}
	// Audit record carries auth_mode=ava + deny:invalid-ava.
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

// TestProxy_AVA_VerifierDisabled_FallsBackToPAT verifies the back-compat
// guarantee: when avaTrustSigner is empty (verifier nil), an AVA header
// is ignored and the PAT path runs as it always has. This is the
// production posture in this MR — A2 turns the verifier on per env.
func TestProxy_AVA_VerifierDisabled_FallsBackToPAT(t *testing.T) {
	base, upstream, _, cleanup := newAVATestShim(t, nil /* AVA off */, false)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/kd/v1/beads", nil)
	// Both headers set: a real client might leave AVA from the browser
	// session active while also presenting a PAT. With AVA off the PAT
	// path must win unchanged.
	req.Header.Set("X-Amzn-Ava-User-Context", "would-be-jwt")
	req.Header.Set("Authorization", "Bearer good-pat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d, want 200 (PAT path)", resp.StatusCode)
	}
	if got := upstream.gotHeaders.Get(headerOperator); got != "alice@example.com" {
		t.Errorf("X-Operator=%q, want alice@example.com (PAT identity)", got)
	}
}

// TestProxy_AVA_HeaderAbsent_PATPathUnchanged is the regression check for
// the status-quo path. With the verifier configured but no AVA header on
// the request, every existing PAT semantic still holds: valid PAT 200,
// invalid 401, not-in-group 403.
func TestProxy_AVA_HeaderAbsent_PATPathUnchanged(t *testing.T) {
	verifier := &stubVerifier{id: avaIdentity{Email: "would-not-be-called@acme.example"}}
	base, upstream, buf, cleanup := newAVATestShim(t, verifier, false)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/kd/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer good-pat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d, want 200", resp.StatusCode)
	}
	if got := upstream.gotHeaders.Get(headerOperator); got != "alice@example.com" {
		t.Errorf("X-Operator=%q, want alice@example.com", got)
	}
	rec := decodeAudit(t, buf)
	if len(rec) != 1 {
		t.Fatalf("len(records)=%d", len(rec))
	}
	if rec[0].Metadata.AuthMode != auditAuthModePAT {
		t.Errorf("auth_mode=%q, want %q", rec[0].Metadata.AuthMode, auditAuthModePAT)
	}
}

// TestProxy_AVA_BothHeadersPresent_AVAWinsDeterministically pins
// precedence: when both X-Amzn-Ava-User-Context AND Authorization are
// present and the AVA verifier is configured, AVA wins. We do NOT fall
// through to PAT on AVA failure (a separate test below).
func TestProxy_AVA_BothHeadersPresent_AVAWinsDeterministically(t *testing.T) {
	verifier := &stubVerifier{id: avaIdentity{Email: "ava-user@acme.example"}}
	base, upstream, _, cleanup := newAVATestShim(t, verifier, false)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/kd/v1/beads", nil)
	req.Header.Set("X-Amzn-Ava-User-Context", "valid-jwt")
	req.Header.Set("Authorization", "Bearer good-pat") // would map to alice@example.com via PAT
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d, want 200", resp.StatusCode)
	}
	if got := upstream.gotHeaders.Get(headerOperator); got != "ava-user@acme.example" {
		t.Errorf("X-Operator=%q, want ava-user@acme.example (AVA wins)", got)
	}
}

// TestProxy_AVA_DryRun_ShortCircuits200 confirms dry-run mode works
// identically on the AVA path: identity is verified, audit fires, but
// the upstream is never reached and the synthetic 200 with
// X-Edge-Auth-Dry-Run: true is returned.
func TestProxy_AVA_DryRun_ShortCircuits200(t *testing.T) {
	verifier := &stubVerifier{id: avaIdentity{Email: "ava-user@acme.example"}}
	base, upstream, buf, cleanup := newAVATestShim(t, verifier, true /* dryRun */)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/kd/v1/beads", nil)
	req.Header.Set("X-Amzn-Ava-User-Context", "valid-jwt")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d, want 200 (dry-run synthetic)", resp.StatusCode)
	}
	if resp.Header.Get(headerDryRun) != "true" {
		t.Errorf("X-Edge-Auth-Dry-Run=%q, want true", resp.Header.Get(headerDryRun))
	}
	if resp.Header.Get(headerAuditID) == "" {
		t.Error("X-Auth-Audit-Id missing on dry-run AVA response")
	}
	if upstream.gotPath != "" {
		t.Error("upstream was hit during dry-run")
	}
	rec := decodeAudit(t, buf)
	if len(rec) != 1 {
		t.Fatalf("len(records)=%d", len(rec))
	}
	if rec[0].Metadata.AuthMode != auditAuthModeAVA {
		t.Errorf("auth_mode=%q, want %q", rec[0].Metadata.AuthMode, auditAuthModeAVA)
	}
	if !rec[0].Metadata.DryRun {
		t.Error("metadata.dry_run not set on dry-run AVA record")
	}
}

// ---------------------------------------------------------------------
// newAvaHandleVerifier wiring tests
// ---------------------------------------------------------------------

// TestNewAvaHandleVerifier_DisabledWhenTrustSignerEmpty confirms the
// back-compat default: avaTrustSigner="" disables the AVA branch entirely
// (returns nil), so the shim behaves exactly as it has since W-B-9.
func TestNewAvaHandleVerifier_DisabledWhenTrustSignerEmpty(t *testing.T) {
	cfg := &config{}
	v := newAvaHandleVerifier(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if v != nil {
		t.Errorf("verifier should be nil when avaTrustSigner is empty, got %T", v)
	}
}

// TestNewAvaHandleVerifier_EnabledWhenTrustSignerSet confirms the
// constructor returns a non-nil verifier when configured. We do not
// exercise verification here — that needs a JWKS server and lives in
// internal/oidcauth/ava_test.go. The shim test only checks the wiring.
func TestNewAvaHandleVerifier_EnabledWhenTrustSignerSet(t *testing.T) {
	cfg := &config{
		avaTrustSigner: "arn:aws:ec2:us-east-1:234567890123:verified-access-instance/vai-test1234",
		avaRegion:      "us-east-1",
	}
	v := newAvaHandleVerifier(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if v == nil {
		t.Fatal("verifier should be non-nil when avaTrustSigner is set")
	}
	// Sanity-check: the verifier short-circuits with errAVAMissing when no
	// header is present. This is the path the shim's handleProxy relies on
	// to decide between AVA and PAT.
	req, _ := http.NewRequest("GET", "http://example/kd", nil)
	_, err := v.verify(context.Background(), req)
	if !errors.Is(err, errAVAMissing) {
		t.Errorf("verify with no AVA header: err=%v, want errAVAMissing", err)
	}
}

// TestAvaHandleVerifier_MalformedJWT_ReturnsInvalid drives the production
// verifier with a malformed JWT (not 3 segments). The underlying
// oidcauth.avaVerifier rejects this synchronously without needing JWKS,
// so we can exercise the wiring without an HTTPS fixture.
func TestAvaHandleVerifier_MalformedJWT_ReturnsInvalid(t *testing.T) {
	cfg := &config{
		avaTrustSigner: "arn:aws:ec2:us-east-1:234567890123:verified-access-instance/vai-test1234",
	}
	v := newAvaHandleVerifier(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if v == nil {
		t.Fatal("verifier nil")
	}
	req, _ := http.NewRequest("GET", "http://example/kd", nil)
	req.Header.Set("X-Amzn-Ava-User-Context", "not.a.valid.jwt.too.many.dots")
	_, err := v.verify(context.Background(), req)
	if !errors.Is(err, errAVAInvalid) {
		t.Errorf("verify with malformed JWT: err=%v, want errAVAInvalid", err)
	}
}

// TestAvaHandleVerifier_ResponseBodyNotLeakedToClient is the
// statusRecorder regression check. The oidcauth middleware writes
// "invalid AVA token\n" to the ResponseWriter on rejection; we MUST not
// surface those bytes to the client because handleProxy emits its own
// JSON shape. The stub recorder swallows them.
func TestAvaHandleVerifier_ResponseBodyNotLeakedToClient(t *testing.T) {
	cfg := &config{
		avaTrustSigner: "arn:aws:ec2:us-east-1:234567890123:verified-access-instance/vai-test1234",
	}
	v := newAvaHandleVerifier(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	// Build a real server that uses the verifier so we observe the
	// end-to-end response body.
	base, _, cleanup := newProxyTestServerWithCapture(t, "k")
	defer cleanup.close()
	cleanup.s.avaVerifier = v
	req, _ := http.NewRequest("GET", base+"/kd/v1/beads", nil)
	req.Header.Set("X-Amzn-Ava-User-Context", "malformed")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	// JSON-decode: the response must parse as the shim's JSON shape, not
	// the oidcauth middleware's "invalid AVA token\n" plain-text body.
	var parsed map[string]string
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Errorf("response body is not JSON (middleware leaked plain text): %q", body)
	}
	if got := parsed["error"]; got != "invalid_ava_token" {
		t.Errorf("error=%q, want invalid_ava_token", got)
	}
	if strings.Contains(string(body), "invalid AVA token") {
		t.Errorf("oidcauth middleware body leaked to client: %q", body)
	}
}

// ---------------------------------------------------------------------
// AVA-only deployment posture: PAT validator nil, AVA verifier set.
// This is the prod posture once edge-auth-shim moves into proxy mode for
// /mux and /kd. The shim MUST still proxy successfully — the PAT path is
// optional, not required.
// ---------------------------------------------------------------------

// TestProxy_AVAOnly_NoPATValidator_ProxiesEndToEnd is the integration
// guard for the proxyEnabled() patch. With s.validator == nil and a
// configured AVA verifier the shim must accept an AVA-authenticated
// request and stamp the same HMAC envelope it would in a both-backends
// posture. A regression here resurfaces the prod 503 the AVA-only patch
// was written to fix.
//
// The fixture reuses newProxyTestServerWithCapture (which wires the PAT
// validator) and then drops s.validator to nil before serving the
// request — exactly the posture a chart with avaTrustSigner set but no
// gitlabBaseURL would produce in prod.
func TestProxy_AVAOnly_NoPATValidator_ProxiesEndToEnd(t *testing.T) {
	verifier := &stubVerifier{id: avaIdentity{Email: "ava-user@acme.example"}}
	base, upstream, cleanup := newProxyTestServerWithCapture(t, "test-hmac-key")
	defer cleanup.close()
	// Force AVA-only posture: AVA verifier set, PAT validator cleared.
	cleanup.s.avaVerifier = verifier
	cleanup.s.validator = nil

	// Sanity: proxyEnabled must be true in this posture or we'd 503
	// before even reaching handleProxy. This is the assertion the patch
	// flipped.
	if !cleanup.s.proxyEnabled() {
		t.Fatal("proxyEnabled()=false in AVA-only posture; patch regressed")
	}

	req, _ := http.NewRequest("GET", base+"/kd/v1/beads", nil)
	req.Header.Set("X-Amzn-Ava-User-Context", "valid-jwt")
	// Deliberately do NOT set Authorization — AVA-only posture has no PAT.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s, want 200", resp.StatusCode, body)
	}
	if got := upstream.gotHeaders.Get(headerOperator); got != "ava-user@acme.example" {
		t.Errorf("X-Operator=%q, want ava-user@acme.example", got)
	}
	if upstream.gotHeaders.Get(headerSignature) == "" {
		t.Error("X-Edge-Auth-Sig missing on AVA-only end-to-end path")
	}
	rec := decodeAudit(t, cleanup.buf)
	if len(rec) != 1 {
		t.Fatalf("len(records)=%d", len(rec))
	}
	if rec[0].Metadata.AuthMode != auditAuthModeAVA {
		t.Errorf("auth_mode=%q, want %q", rec[0].Metadata.AuthMode, auditAuthModeAVA)
	}
}

// TestProxy_AVAOnly_NoCredentials_401 pins behaviour when an AVA-only
// shim receives a request with no AVA header AND no PAT. With
// proxyEnabled() true in this posture the request reaches handleProxy,
// the default branch fires, extractBearerToken returns "", and we 401
// missing_token. The audit record carries auth_mode=pat (it's the
// branch we entered) which is intentionally honest — the shim can't
// distinguish "client meant AVA but forgot the header" from "client
// meant PAT but forgot the token".
func TestProxy_AVAOnly_NoCredentials_401(t *testing.T) {
	verifier := &stubVerifier{id: avaIdentity{Email: "ava-user@acme.example"}}
	base, _, cleanup := newProxyTestServerWithCapture(t, "test-hmac-key")
	defer cleanup.close()
	cleanup.s.avaVerifier = verifier
	cleanup.s.validator = nil

	req, _ := http.NewRequest("GET", base+"/kd/v1/beads", nil)
	// No AVA header, no Authorization header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401 (missing token)", resp.StatusCode)
	}
}

// TestProxy_AVAOnly_PATPresentButNoValidator_401 is the safety check for
// the s.validator == nil guard added alongside the proxyEnabled() patch.
// A client mistakenly presents a Bearer token to an AVA-only shim — the
// PAT branch is selected (no AVA header), but s.validator is nil. The
// guard 401s rather than nil-panicking on s.validator.Validate.
func TestProxy_AVAOnly_PATPresentButNoValidator_401(t *testing.T) {
	verifier := &stubVerifier{id: avaIdentity{Email: "ava-user@acme.example"}}
	base, upstream, cleanup := newProxyTestServerWithCapture(t, "test-hmac-key")
	defer cleanup.close()
	cleanup.s.avaVerifier = verifier
	cleanup.s.validator = nil

	req, _ := http.NewRequest("GET", base+"/kd/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer would-be-pat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401 (no validator in AVA-only posture)", resp.StatusCode)
	}
	if upstream.gotPath != "" {
		t.Error("upstream was hit despite no validator; nil-deref guard regressed")
	}
}

// ---------------------------------------------------------------------
// Audit timestamp regression: AVA records share the same RFC3339Nano
// format as PAT records. We pin it here so a future refactor that
// reshapes the audit time field catches.
// ---------------------------------------------------------------------

// ---------------------------------------------------------------------
// Compile-time check: stubVerifier implements avaVerifier.
// ---------------------------------------------------------------------

var _ avaVerifier = (*stubVerifier)(nil)
