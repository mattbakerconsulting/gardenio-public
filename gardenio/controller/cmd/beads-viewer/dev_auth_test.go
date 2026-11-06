package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/oidcauth"
)

func TestDevAuthBypassEnabled(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"no":    false,
		"1":     true,
		"true":  true,
		"TRUE":  true,
		"yes":   true,
		"YES":   true,
	}
	for v, want := range cases {
		t.Setenv(devAuthBypassEnv, v)
		if got := devAuthBypassEnabled(); got != want {
			t.Errorf("BEADS_VIEWER_DEV_AUTH_BYPASS=%q: got %v, want %v", v, got, want)
		}
	}
}

func TestCheckDevAuthBypassAllowed(t *testing.T) {
	t.Run("no SA token -> allowed", func(t *testing.T) {
		t.Setenv(devAuthBypassAckAgentPodEnv, "")
		if err := checkDevAuthBypassAllowed(func(string) bool { return false }); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})
	t.Run("SA token present, no ack -> refused", func(t *testing.T) {
		t.Setenv(devAuthBypassAckAgentPodEnv, "")
		err := checkDevAuthBypassAllowed(func(path string) bool {
			return path == k8sServiceAccountTokenPath
		})
		if !errors.Is(err, errBypassInProdLikeEnv) {
			t.Fatalf("expected errBypassInProdLikeEnv, got %v", err)
		}
	})
	t.Run("SA token present, ack set -> allowed", func(t *testing.T) {
		t.Setenv(devAuthBypassAckAgentPodEnv, "1")
		err := checkDevAuthBypassAllowed(func(path string) bool {
			return path == k8sServiceAccountTokenPath
		})
		if err != nil {
			t.Fatalf("expected nil with ACK set, got %v", err)
		}
	})
	t.Run("no SA token, ack set -> allowed (unchanged)", func(t *testing.T) {
		t.Setenv(devAuthBypassAckAgentPodEnv, "1")
		if err := checkDevAuthBypassAllowed(func(string) bool { return false }); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})
}

func TestDevAuthBypassAckAgentPod(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"1":     true,
		"true":  true,
		"YES":   true,
	}
	for v, want := range cases {
		t.Setenv(devAuthBypassAckAgentPodEnv, v)
		if got := devAuthBypassAckAgentPod(); got != want {
			t.Errorf("%s=%q: got %v, want %v", devAuthBypassAckAgentPodEnv, v, got, want)
		}
	}
}

func TestForceLoopbackListenAddr(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"bind-all default", ":8095", "127.0.0.1:8095", false},
		{"bind-all explicit ipv4", "0.0.0.0:8095", "127.0.0.1:8095", false},
		{"bind-all explicit ipv6", "[::]:8095", "127.0.0.1:8095", false},
		{"loopback ipv4 unchanged", "127.0.0.1:8095", "127.0.0.1:8095", false},
		{"loopback ipv6 unchanged", "[::1]:8095", "[::1]:8095", false},
		{"localhost unchanged", "localhost:8095", "localhost:8095", false},
		{"non-loopback refused", "10.0.0.1:8095", "", true},
		{"invalid addr refused", "not-a-port", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := forceLoopbackListenAddr(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("forceLoopbackListenAddr(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseConfigWith_AckAgentPodAllowsBypassWithSAToken(t *testing.T) {
	t.Setenv(devAuthBypassEnv, "1")
	t.Setenv(devAuthBypassAckAgentPodEnv, "1")
	t.Setenv("LISTEN_ADDR", "")
	cfg, err := parseConfigWith(func(path string) bool {
		return path == k8sServiceAccountTokenPath
	})
	if err != nil {
		t.Fatalf("expected bypass to be allowed with ACK, got %v", err)
	}
	if cfg.listenAddr != "127.0.0.1:8095" {
		t.Errorf("expected listen addr forced to 127.0.0.1:8095, got %q", cfg.listenAddr)
	}
}

func TestParseConfigWith_AckAgentPodRejectsNonLoopback(t *testing.T) {
	t.Setenv(devAuthBypassEnv, "1")
	t.Setenv(devAuthBypassAckAgentPodEnv, "1")
	t.Setenv("LISTEN_ADDR", "10.0.0.1:8095")
	_, err := parseConfigWith(func(path string) bool {
		return path == k8sServiceAccountTokenPath
	})
	if err == nil {
		t.Fatal("expected non-loopback LISTEN_ADDR to be refused with ACK set")
	}
}

func TestParseConfigWith_AckIgnoredWithoutBypass(t *testing.T) {
	// ACK alone doesn't enable anything — bypass is still off, so OIDC is
	// required and the SA-token guard never runs.
	t.Setenv(devAuthBypassEnv, "")
	t.Setenv(devAuthBypassAckAgentPodEnv, "1")
	t.Setenv("OIDC_ISSUER_URL", "")
	t.Setenv("OIDC_CLIENT_ID", "")
	_, err := parseConfigWith(func(string) bool { return false })
	if err == nil {
		t.Fatal("expected error: ACK without bypass should still require OIDC")
	}
}

func TestParseConfigWith_BypassSkipsOIDCRequirement(t *testing.T) {
	t.Setenv(devAuthBypassEnv, "1")
	t.Setenv("OIDC_ISSUER_URL", "")
	t.Setenv("OIDC_CLIENT_ID", "")

	cfg, err := parseConfigWith(func(string) bool { return false })
	if err != nil {
		t.Fatalf("expected config to parse with bypass, got err=%v", err)
	}
	if !cfg.devAuthBypass {
		t.Error("expected devAuthBypass=true")
	}
}

func TestParseConfigWith_BypassRefusedInProdLikeEnv(t *testing.T) {
	t.Setenv(devAuthBypassEnv, "1")
	_, err := parseConfigWith(func(path string) bool {
		return path == k8sServiceAccountTokenPath
	})
	if !errors.Is(err, errBypassInProdLikeEnv) {
		t.Fatalf("expected errBypassInProdLikeEnv, got %v", err)
	}
}

func TestParseConfigWith_OIDCRequiredWhenBypassOff(t *testing.T) {
	t.Setenv(devAuthBypassEnv, "")
	t.Setenv("OIDC_ISSUER_URL", "")
	t.Setenv("OIDC_CLIENT_ID", "")

	_, err := parseConfigWith(func(string) bool { return false })
	if err == nil {
		t.Fatal("expected error when OIDC config missing and bypass off")
	}
}

// AVA edge-auth (aws-poc / ka-0000003205) — when AVA_TRUST_SIGNER is set,
// Keycloak OIDC env pair is optional. The deeper oidcauth.Validate
// (internal/oidcauth/validate.go) gates this; parseConfig must not
// short-circuit it.
func TestParseConfigWith_AVAOnlyAllowsEmptyOIDC(t *testing.T) {
	t.Setenv(devAuthBypassEnv, "")
	t.Setenv("OIDC_ISSUER_URL", "")
	t.Setenv("OIDC_CLIENT_ID", "")
	t.Setenv("AVA_TRUST_SIGNER", "arn:aws:ec2:us-east-1:123456789012:verified-access-instance/vai-0123456789abcdef0")

	cfg, err := parseConfigWith(func(string) bool { return false })
	if err != nil {
		t.Fatalf("expected AVA-only config to parse, got err=%v", err)
	}
	if cfg.avaTrustSigner == "" {
		t.Error("expected avaTrustSigner to be populated")
	}
	if cfg.oidcIssuerURL != "" || cfg.oidcClientID != "" {
		t.Error("expected OIDC fields empty for AVA-only path")
	}
}

func TestParseConfigWith_AVAWithBothOIDCFieldsAllowed(t *testing.T) {
	t.Setenv(devAuthBypassEnv, "")
	t.Setenv("OIDC_ISSUER_URL", "https://keycloak.example/realms/test")
	t.Setenv("OIDC_CLIENT_ID", "beads-viewer")
	t.Setenv("AVA_TRUST_SIGNER", "arn:aws:ec2:us-east-1:123456789012:verified-access-instance/vai-0123456789abcdef0")

	if _, err := parseConfigWith(func(string) bool { return false }); err != nil {
		t.Fatalf("expected AVA+OIDC config to parse, got err=%v", err)
	}
}

func TestParseConfigWith_AVAWithHalfOIDCRejected(t *testing.T) {
	for _, tc := range []struct {
		name, issuer, clientID string
	}{
		{"issuer-only", "https://keycloak.example/realms/test", ""},
		{"client-only", "", "beads-viewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(devAuthBypassEnv, "")
			t.Setenv("OIDC_ISSUER_URL", tc.issuer)
			t.Setenv("OIDC_CLIENT_ID", tc.clientID)
			t.Setenv("AVA_TRUST_SIGNER", "arn:aws:ec2:us-east-1:123456789012:verified-access-instance/vai-0123456789abcdef0")

			if _, err := parseConfigWith(func(string) bool { return false }); err == nil {
				t.Fatal("expected half-configured OIDC alongside AVA to be rejected")
			}
		})
	}
}

func TestDevBypassMiddleware_Passthrough(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})

	h := devBypassMiddleware(logger, "", nil)(next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/p/gardenio", nil)
	h.ServeHTTP(rec, req)

	if !called {
		t.Fatal("expected next handler to be invoked")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// TestDevBypassMiddleware_PromotesForwardAuthEmail is the AC#3 guard for
// ka-0000002979: on the Gardenio-in-a-Box forwardauth path (OIDC bypassed at the
// pod; Traefik's oauth2-proxy ForwardAuth OVERWRITES X-Auth-Request-Email from
// its verified auth response), the dev-bypass middleware must promote that
// trusted header into the verified-identity context so downstream handlers —
// which now read only EmailFromContext — still see the operator. Without this,
// migrating handlers off EmailFromRequest would blank identity on the box.
func TestDevBypassMiddleware_PromotesForwardAuthEmail(t *testing.T) {
	var gotCtxEmail string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCtxEmail = oidcauth.EmailFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	h := devBypassMiddleware(slog.Default(), "", nil)(next)
	req := httptest.NewRequest(http.MethodPost, "/b/gd-1/owner", nil)
	req.Header.Set("X-Auth-Request-Email", "operator@acme.example")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotCtxEmail != "operator@acme.example" {
		t.Errorf("EmailFromContext = %q, want operator@acme.example (forwardauth-stamped header must be promoted into ctx on the dev-bypass path)", gotCtxEmail)
	}
}

// TestForwardAuthTrustedEmailToContext covers the trust-seam helper directly:
// it promotes a present header, no-ops on an absent header, and never overrides
// an already-verified context email (defends against double-application).
func TestForwardAuthTrustedEmailToContext(t *testing.T) {
	t.Run("promotes present header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Auth-Request-Email", "  user@acme.example  ")
		got := oidcauth.EmailFromContext(forwardAuthTrustedEmailToContext(req).Context())
		if got != "user@acme.example" {
			t.Errorf("EmailFromContext = %q, want trimmed user@acme.example", got)
		}
	})

	t.Run("no-op when header absent", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if got := oidcauth.EmailFromContext(forwardAuthTrustedEmailToContext(req).Context()); got != "" {
			t.Errorf("EmailFromContext = %q, want empty", got)
		}
	})

	t.Run("verified context wins over header", func(t *testing.T) {
		// Simulates the AVA/Keycloak path: ctx already carries a verified email.
		// Even if a forged header is present, the helper must not overwrite it.
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(
			oidcauth.ContextWithEmail(t.Context(), "verified@acme.example"))
		req.Header.Set("X-Auth-Request-Email", "attacker@evil.example")
		if got := oidcauth.EmailFromContext(forwardAuthTrustedEmailToContext(req).Context()); got != "verified@acme.example" {
			t.Errorf("EmailFromContext = %q, want verified@acme.example (forged header must not override verified ctx)", got)
		}
	})
}

// TestDevBypassMiddleware_ProxySecret is the gd-kRyMsWiDNZ guard: with a proxy
// secret configured, a pod that reaches the viewer directly and forges
// X-Auth-Request-Email must get no identity; only the edge (which stamps the
// secret) is trusted, and the secret header never reaches handlers.
func TestDevBypassMiddleware_ProxySecret(t *testing.T) {
	const secret = "s3cret-value"
	cases := []struct {
		name      string
		secretHdr []string
		wantEmail string
	}{
		{"edge with valid secret", []string{secret}, "operator@acme.example"},
		{"forged email, no secret", nil, ""},
		{"forged email, wrong secret", []string{"nope"}, ""},
		{"forged email, empty secret", []string{""}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotEmail, gotSecretHdr string
			next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				gotEmail = oidcauth.EmailFromContext(r.Context())
				gotSecretHdr = r.Header.Get(devAuthProxySecretHeader)
			})
			req := httptest.NewRequest(http.MethodPost, "/b/gd-1/owner", nil)
			req.Header.Set("X-Auth-Request-Email", "operator@acme.example")
			for _, v := range tc.secretHdr {
				req.Header.Add(devAuthProxySecretHeader, v)
			}
			devBypassMiddleware(slog.Default(), secret, nil)(next).ServeHTTP(httptest.NewRecorder(), req)
			if gotEmail != tc.wantEmail {
				t.Errorf("EmailFromContext = %q, want %q", gotEmail, tc.wantEmail)
			}
			if gotSecretHdr != "" {
				t.Errorf("secret header leaked to handler: %q", gotSecretHdr)
			}
		})
	}
}
