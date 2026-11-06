package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// gd-0o4Ggf9ngP (Lands Track B 7.3): a user who signs in through Keycloak's
// GitHub IdP carries github_id and github_login claims (the chart's
// github-identity client scope), and kbeads reads both into the AuthIdentity.
// These run the real go-oidc verifier against signed tokens.

const testGitHubEmail = "octo@example.test"

func gitHubClaims(extra map[string]any) map[string]any {
	claims := map[string]any{
		"azp":            testEdgeClient,
		"email":          testGitHubEmail,
		"email_verified": true,
		"realm_access":   map[string]any{"roles": []string{RoleViewer}},
	}
	for k, v := range extra {
		claims[k] = v
	}
	return claims
}

func TestOIDCVerifier_ExtractGitHub(t *testing.T) {
	kc, v := newEmailVerifiedVerifier(t)
	ctx := context.Background()

	cases := []struct {
		name      string
		claims    map[string]any
		wantID    int64
		wantLogin string
	}{
		{"number id and login", map[string]any{"github_id": 583231, "github_login": "octocat"}, 583231, "octocat"},
		{"string id", map[string]any{"github_id": "583231", "github_login": "octocat"}, 583231, "octocat"},
		{"hyphenated login", map[string]any{"github_id": 1, "github_login": "mona-lisa-2"}, 1, "mona-lisa-2"},
		{"not a github user", nil, 0, ""},
		{"zero id", map[string]any{"github_id": 0}, 0, ""},
		{"negative id", map[string]any{"github_id": -5}, 0, ""},
		{"fractional id", map[string]any{"github_id": 1.5}, 0, ""},
		{"non-numeric id", map[string]any{"github_id": "octocat"}, 0, ""},
		{"login with a leading hyphen", map[string]any{"github_login": "-octocat"}, 0, ""},
		{"login with a double hyphen", map[string]any{"github_login": "octo--cat"}, 0, ""},
		{"login with a space", map[string]any{"github_login": "octo cat"}, 0, ""},
		{"numeric login, id kept", map[string]any{"github_id": 7, "github_login": 4242}, 7, ""},
		{"login with markup", map[string]any{"github_login": "<b>octo</b>"}, 0, ""},
		{"login of 39 characters", map[string]any{"github_login": strings.Repeat("a", 39)}, 0, strings.Repeat("a", 39)},
		{"login of 40 characters", map[string]any{"github_login": strings.Repeat("a", 40)}, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := kc.mint(t, testPublicIssuer, testAudience, gitHubClaims(tc.claims))
			id, login, err := v.(GitHubExtractor).ExtractGitHub(ctx, tok)
			if err != nil {
				t.Fatalf("ExtractGitHub: %v", err)
			}
			if id != tc.wantID || login != tc.wantLogin {
				t.Fatalf("ExtractGitHub = (%d, %q), want (%d, %q)", id, login, tc.wantID, tc.wantLogin)
			}
		})
	}

	t.Run("unverified email is refused", func(t *testing.T) {
		tok := kc.mint(t, testPublicIssuer, testAudience, gitHubClaims(map[string]any{
			"email_verified": false, "github_id": 583231, "github_login": "octocat",
		}))
		if id, login, err := v.(GitHubExtractor).ExtractGitHub(ctx, tok); err == nil {
			t.Fatalf("accepted an unverified-email token as GitHub user (%d, %q)", id, login)
		}
	})
}

// The holder and the client role ceiling wrap the real verifier in
// production; both must pass ExtractGitHub through.
func TestOIDCVerifierWrappers_ExtractGitHub(t *testing.T) {
	kc, v := newEmailVerifiedVerifier(t)
	tok := kc.mint(t, testPublicIssuer, testAudience, gitHubClaims(map[string]any{"github_id": 42, "github_login": "octocat"}))

	holder := NewOIDCVerifierHolder()
	if _, _, err := holder.ExtractGitHub(context.Background(), tok); err == nil {
		t.Fatal("an empty holder must not extract")
	}
	ceiling, err := NewClientRoleCeilingVerifier(v, map[string]string{DeviceGrantClientID: RoleViewer})
	if err != nil {
		t.Fatal(err)
	}
	holder.Set(ceiling)
	for name, gv := range map[string]TokenVerifier{"holder": holder, "ceiling": ceiling} {
		ge, ok := gv.(GitHubExtractor)
		if !ok {
			t.Fatalf("%s does not implement GitHubExtractor", name)
		}
		id, login, err := ge.ExtractGitHub(context.Background(), tok)
		if err != nil || id != 42 || login != "octocat" {
			t.Fatalf("%s: ExtractGitHub = (%d, %q, %v), want (42, octocat, nil)", name, id, login, err)
		}
	}
}

func TestAuthMiddleware_GitHubIdentity(t *testing.T) {
	kc, v := newEmailVerifiedVerifier(t)
	serve := func(tok string) (int, AuthIdentity) {
		var seen AuthIdentity
		h := authMiddleware(testEdgeClients, withTestToken(nil, "primary"), v, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = AuthIdentityFromContext(r.Context())
			w.WriteHeader(http.StatusNoContent)
		}))
		req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, seen
	}

	t.Run("github user", func(t *testing.T) {
		code, seen := serve(kc.mint(t, testPublicIssuer, testAudience, gitHubClaims(map[string]any{"github_id": "583231", "github_login": "octocat"})))
		if code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", code)
		}
		if seen.Subject != testGitHubEmail || seen.GitHubID != 583231 || seen.GitHubLogin != "octocat" {
			t.Fatalf("identity = %+v, want subject %q github (583231, octocat)", seen, testGitHubEmail)
		}
	})
	t.Run("not a github user", func(t *testing.T) {
		code, seen := serve(kc.mint(t, testPublicIssuer, testAudience, gitHubClaims(nil)))
		if code != http.StatusNoContent || seen.GitHubID != 0 || seen.GitHubLogin != "" {
			t.Fatalf("status = %d identity = %+v, want 204 and no GitHub account", code, seen)
		}
	})
	// A GitHub account with no verified email cannot sign in (Track B §2.2
	// item 2): the claims never reach an identity.
	t.Run("github user without a verified email", func(t *testing.T) {
		code, seen := serve(kc.mint(t, testPublicIssuer, testAudience, gitHubClaims(map[string]any{
			"email_verified": false, "github_id": 583231, "github_login": "octocat",
		})))
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d identity = %+v, want 401", code, seen)
		}
	})
}

func TestAuthInterceptor_GitHubIdentity(t *testing.T) {
	kc, v := newEmailVerifiedVerifier(t)
	tok := kc.mint(t, testPublicIssuer, testAudience, gitHubClaims(map[string]any{"github_id": 583231, "github_login": "octocat"}))
	var seen AuthIdentity
	handler := func(ctx context.Context, _ any) (any, error) {
		seen = AuthIdentityFromContext(ctx)
		return "ok", nil
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+tok))
	if _, err := AuthInterceptor(withTestToken(nil, "secret"), v)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, handler); err != nil {
		t.Fatal(err)
	}
	if seen.GitHubID != 583231 || seen.GitHubLogin != "octocat" {
		t.Fatalf("identity = %+v, want github (583231, octocat)", seen)
	}
}
