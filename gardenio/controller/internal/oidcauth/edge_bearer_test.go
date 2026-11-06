package oidcauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestEdgeBearerVerifier(t *testing.T) {
	f := newOIDCFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	v, err := NewEdgeBearerVerifier(ctx, EdgeBearerConfig{IssuerURL: f.issuer, Audience: "test-client"}, slog.Default())
	if err != nil {
		t.Fatalf("NewEdgeBearerVerifier: %v", err)
	}
	for deadline := time.Now().Add(10 * time.Second); !v.Ready(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("discovery did not succeed")
		}
	}

	claims := func(iss, aud string, exp time.Time, email, emailVerified string) string {
		return fmt.Sprintf(`{
			"iss": %q, "aud": %q, "sub": "u1", "exp": %d,
			"email": %q, "email_verified": %s,
			"realm_access": {"roles": ["gardenio-operator", "offline_access"]},
			"realm_roles": ["gardenio-operator", "gardenio-viewer"],
			"groups": ["/project-claimlayer"]
		}`, iss, aud, exp.Unix(), email, emailVerified)
	}
	hour := time.Now().Add(time.Hour)

	t.Run("valid", func(t *testing.T) {
		id, err := v.Verify(ctx, f.signToken(t, claims(f.issuer, "test-client", hour, "a@example.com", "true")))
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if id.Email != "a@example.com" {
			t.Errorf("email = %q", id.Email)
		}
		if want := []string{"gardenio-operator", "offline_access", "gardenio-viewer"}; !slices.Equal(id.Roles, want) {
			t.Errorf("roles = %v, want %v (merged, deduped)", id.Roles, want)
		}
		if !slices.Equal(id.Groups, []string{"/project-claimlayer"}) {
			t.Errorf("groups = %v", id.Groups)
		}
		if id.Impersonator != "" {
			t.Errorf("impersonator = %q on a token without the claim", id.Impersonator)
		}
	})

	t.Run("github sign-in", func(t *testing.T) {
		c := strings.Replace(claims(f.issuer, "test-client", hour, "a@example.com", "true"), `"sub": "u1",`, `"sub": "u1", "github_id": "583231", "github_login": "octocat",`, 1)
		id, err := v.Verify(ctx, f.signToken(t, c))
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if id.GitHubID != 583231 || id.GitHubLogin != "octocat" || id.Subject != "u1" {
			t.Errorf("github id, login, subject = %d, %q, %q; want the token's", id.GitHubID, id.GitHubLogin, id.Subject)
		}
	})

	t.Run("impersonated", func(t *testing.T) {
		c := strings.Replace(claims(f.issuer, "test-client", hour, "tenant@example.com", "true"), `"sub": "u1",`, `"sub": "u1", "impersonator": " admin ", "impersonator_id": "a1",`, 1)
		id, err := v.Verify(ctx, f.signToken(t, c))
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if id.Email != "tenant@example.com" || id.Impersonator != "admin" {
			t.Errorf("email, impersonator = %q, %q; want the tenant and the admin behind them", id.Email, id.Impersonator)
		}
	})

	impersonated := func(c string) string {
		return strings.Replace(c, `"sub": "u1",`, `"sub": "u1", "impersonator": "admin",`, 1)
	}
	refused := map[string]string{
		"expired":            claims(f.issuer, "test-client", time.Now().Add(-time.Minute), "a@example.com", "true"),
		"wrong issuer":       claims("https://evil.example", "test-client", hour, "a@example.com", "true"),
		"wrong audience":     claims(f.issuer, "other-client", hour, "a@example.com", "true"),
		"email not verified": claims(f.issuer, "test-client", hour, "a@example.com", "false"),
		"email_verified absent": fmt.Sprintf(`{"iss": %q, "aud": "test-client", "sub": "u1", "exp": %d, "email": "a@example.com",
			"realm_access": {"roles": ["gardenio-admin"]}}`, f.issuer, hour.Unix()),
		// The impersonator claim vouches for nothing: an impersonated token is refused on the same checks as any other.
		"impersonated, expired":            impersonated(claims(f.issuer, "test-client", time.Now().Add(-time.Minute), "a@example.com", "true")),
		"impersonated, wrong issuer":       impersonated(claims("https://evil.example", "test-client", hour, "a@example.com", "true")),
		"impersonated, wrong audience":     impersonated(claims(f.issuer, "other-client", hour, "a@example.com", "true")),
		"impersonated, email not verified": impersonated(claims(f.issuer, "test-client", hour, "a@example.com", "false")),
	}
	for name, c := range refused {
		t.Run(name, func(t *testing.T) {
			if id, err := v.Verify(ctx, f.signToken(t, c)); err == nil {
				t.Errorf("Verify accepted it: %+v", id)
			}
		})
	}
}

func TestEdgeBearerVerifier_NotReady(t *testing.T) {
	var v EdgeBearerVerifier
	if _, err := v.Verify(context.Background(), "x.y.z"); !errors.Is(err, ErrEdgeBearerNotReady) {
		t.Errorf("err = %v, want ErrEdgeBearerNotReady", err)
	}
	if v.Ready() {
		t.Error("Ready before discovery")
	}
}

func TestNewEdgeBearerVerifier_NeedsIssuerAndAudience(t *testing.T) {
	for _, cfg := range []EdgeBearerConfig{{IssuerURL: "https://kc.example/realms/g"}, {Audience: "c"}} {
		if _, err := NewEdgeBearerVerifier(context.Background(), cfg, slog.Default()); err == nil {
			t.Errorf("%+v: built a verifier", cfg)
		}
	}
}
