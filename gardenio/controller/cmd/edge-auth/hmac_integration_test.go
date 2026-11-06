// Integration test for the edge-auth HMAC reverse-proxy (W-B-9,
// ka-0000001042). Pairs the edge-auth signer in cmd/edge-auth/proxy.go
// with the daemon-side verifier in internal/edgeauth/verify.go — the
// same wire contract kbeads + coopmux apply in W-B-11
// (ka-0000001678). The test asserts the round-trip that real CLI
// traffic will exercise:
//
//   - happy path: edge-auth signs, downstream Verify() returns
//     OutcomeOK, trusts X-Operator, request succeeds.
//
// The tampered, missing-envelope, stale and burn-in outcomes are the
// verifier's alone (the shim is not on that path), so they are pinned in
// internal/edgeauth/verify_test.go TestVerify_BadSig_Outcomes.
//
// Scope-out: this file lives under cmd/edge-auth and imports
// internal/edgeauth. It does not import kbeads (separate Go module);
// W-B-11 ships an equivalent verify.go port. The wire contract is the
// authoritative interface: as long as both sides agree on the
// canonical HMAC message shape, both verifiers behave identically.
// The kbeads-side integration coverage lives under kbeads/internal/...

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

// hmacVerifyingUpstream is the test stand-in for an edge-auth-aware
// daemon. It runs the same edgeauth.VerifyConfig.Verify the kbeads
// W-B-11 middleware runs and, in enforce mode, returns 401 on any
// non-OK outcome. In burn-in mode it still trusts the asserted
// operator (matches the kbeads middleware burn-in fallthrough).
type hmacVerifyingUpstream struct {
	cfg        edgeauth.VerifyConfig
	body       string
	lastResult edgeauth.VerifyResult
	lastPath   string
	requests   int
	rejected   int
	accepted   int
}

func (u *hmacVerifyingUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u.requests++
		res := u.cfg.Verify(r)
		u.lastResult = res
		u.lastPath = r.URL.Path
		if !res.Trusted {
			u.rejected++
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"hmac_verify_failed"}`))
			return
		}
		u.accepted++
		// Echo the trusted operator on the response so the integration
		// caller can assert end-to-end attribution.
		w.Header().Set("X-Operated-By", res.Operator)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, u.body)
	}
}

// hmacFixture bundles the three real httptest servers — stub GitLab,
// daemon-side verifier, edge-auth shim — and shares a fixed clock +
// HMAC key between the signer and the verifier so the round-trip is
// deterministic.
type hmacFixture struct {
	edgeURL  string
	upstream *hmacVerifyingUpstream
	cleanup  func()
}

const fixtureFixedUnix = int64(1_700_000_000)

func newHMACIntegrationFixture(t *testing.T, hmacKey string, enforce bool) *hmacFixture {
	t.Helper()

	clock := func() time.Time { return time.Unix(fixtureFixedUnix, 0) }

	gitlab := newStubGitLab(t)

	upstream := &hmacVerifyingUpstream{
		cfg: edgeauth.VerifyConfig{
			HMACKey: []byte(hmacKey),
			Enforce: enforce,
			Now:     clock,
		},
		body: `{"ok":true}`,
	}
	upstreamServer := httptest.NewServer(upstream.handler())

	cfg := &config{
		gitlabBaseURL:   gitlab.URL,
		gitlabAuthGroup: "gardenio",
		hmacKey:         hmacKey,
		upstreamKbeads:  upstreamServer.URL,
	}
	edge := newServer(cfg, "test", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	edge.now = clock
	mux := http.NewServeMux()
	edge.RegisterRoutes(mux)
	edgeServer := httptest.NewServer(mux)

	return &hmacFixture{
		edgeURL:  edgeServer.URL,
		upstream: upstream,
		cleanup: func() {
			edgeServer.Close()
			upstreamServer.Close()
			gitlab.Close()
		},
	}
}

// TestHMACIntegration_HappyPath_DownstreamTrustsOperator is the core
// round-trip: the daemon receives the request edge-auth signed, the
// verifier returns OutcomeOK, and the daemon trusts X-Operator. The
// echoed X-Operated-By proves attribution flowed end-to-end.
func TestHMACIntegration_HappyPath_DownstreamTrustsOperator(t *testing.T) {
	const key = "shared-stage3-key"
	fx := newHMACIntegrationFixture(t, key, true)
	defer fx.cleanup()

	req, _ := http.NewRequest("GET", fx.edgeURL+"/kd/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer good-pat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d, body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Operated-By"); got != "alice@example.com" {
		t.Errorf("X-Operated-By=%q, want alice@example.com", got)
	}
	if fx.upstream.lastResult.Outcome != edgeauth.OutcomeOK {
		t.Errorf("downstream outcome=%q, want %q", fx.upstream.lastResult.Outcome, edgeauth.OutcomeOK)
	}
	if !fx.upstream.lastResult.Trusted {
		t.Error("downstream did not trust the request")
	}
	if fx.upstream.accepted != 1 || fx.upstream.rejected != 0 {
		t.Errorf("accepted=%d rejected=%d, want 1/0", fx.upstream.accepted, fx.upstream.rejected)
	}
	if fx.upstream.lastPath != "/v1/beads" {
		t.Errorf("upstream path=%q, want /v1/beads", fx.upstream.lastPath)
	}
}
