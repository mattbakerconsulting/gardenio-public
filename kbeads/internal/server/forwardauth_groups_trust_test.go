package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// groupsProbe runs one request through authMiddleware and returns the groups
// the downstream handler saw.
func groupsProbe(t *testing.T, edge map[string]struct{}, v TokenVerifier, token, header string) []string {
	t.Helper()
	var got []string
	h := authMiddleware(edge, withTestToken(nil, "primary"), v, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = GroupsFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if header != "" {
		req.Header.Set("X-Auth-Request-Groups", header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (token itself is valid)", rec.Code)
	}
	return got
}

// TestForwardAuthGroupsIgnoredForNonEdgeClients pins gd-FQIjyyCxYY: the
// client-controllable X-Auth-Request-Groups header must not set the groups of
// a valid realm token unless it was minted for the edge forward-auth client
// and carries no groups claim of its own (gd-Wmom75wBdl).
// Groups become allowed_projects on the caller's user bead and feed team
// membership and taint read grants, so honouring the header for any signed-in
// caller let them widen their own read scope.
func TestForwardAuthGroupsIgnoredForNonEdgeClients(t *testing.T) {
	const forged = "/gardenio-admins,/project-secret"
	forgedGroups := []string{"/gardenio-admins", "/project-secret"}
	own := []string{"/own-group"}

	tests := []struct {
		name  string
		edge  map[string]struct{}
		azp   string
		want  []string
		hdr   string
		noExt bool
	}{
		{"edge client with a groups claim keeps the claim over a conflicting header", testEdgeClients, testEdgeClient, own, forged, false},
		{"viewer-cli token keeps its own claim", testEdgeClients, "gardenio-cli", own, forged, false},
		{"service-account token keeps its own claim", testEdgeClients, "service-account-bridge", own, forged, false},
		{"token without azp keeps its own claim", testEdgeClients, "", own, forged, false},
		{"no edge client configured keeps own claim", nil, testEdgeClient, own, forged, false},
		{"edge client without header keeps its own claim", testEdgeClients, testEdgeClient, own, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := &stubGroupExtractor{roles: []string{RoleViewer}, groups: own, azp: tc.azp}
			if got := groupsProbe(t, tc.edge, v, "oidc-jwt", tc.hdr); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("groups = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("positive control: edge client without a groups claim takes the header", func(t *testing.T) {
		v := &stubGroupExtractor{roles: []string{RoleViewer}, azp: testEdgeClient}
		if got := groupsProbe(t, testEdgeClients, v, "oidc-jwt", forged); !reflect.DeepEqual(got, forgedGroups) {
			t.Errorf("groups = %v, want %v", got, forgedGroups)
		}
	})

	t.Run("no groups claim and no trust yields no groups", func(t *testing.T) {
		v := &stubGroupExtractor{roles: []string{RoleViewer}, azp: "gardenio-cli"}
		if got := groupsProbe(t, testEdgeClients, v, "oidc-jwt", forged); len(got) != 0 {
			t.Errorf("groups = %v, want none", got)
		}
	})
}

// TestForwardAuthGroupsSignedToken runs the same checks against the real OIDC
// verifier with genuinely signed tokens, so the azp and groups come from the
// verified JWT rather than a stub.
func TestForwardAuthGroupsSignedToken(t *testing.T) {
	kc := newFakeKeycloak(t)
	v, err := NewOIDCVerifierWithDiscovery(context.Background(), testPublicIssuer, kc.discoveryURL(), testAudience)
	if err != nil {
		t.Fatal(err)
	}
	const forged = "/gardenio-admins,/project-secret"
	claims := func(azp string) map[string]any {
		return map[string]any{
			"azp":          azp,
			"groups":       []string{"/own-group"},
			"realm_access": map[string]any{"roles": []string{RoleViewer}},
		}
	}

	// The edge client's signed token keeps its own claim over the header
	// (gd-Wmom75wBdl)...
	edgeTok := kc.mint(t, testPublicIssuer, testAudience, claims(testEdgeClient))
	if got, want := groupsProbe(t, testEdgeClients, v, edgeTok, forged), []string{"/own-group"}; !reflect.DeepEqual(got, want) {
		t.Errorf("edge client groups = %v, want %v", got, want)
	}

	// ...and only falls back to the header when it carries no groups claim.
	noClaim := claims(testEdgeClient)
	delete(noClaim, "groups")
	bareTok := kc.mint(t, testPublicIssuer, testAudience, noClaim)
	if got, want := groupsProbe(t, testEdgeClients, v, bareTok, forged), []string{"/gardenio-admins", "/project-secret"}; !reflect.DeepEqual(got, want) {
		t.Errorf("edge client without claim groups = %v, want %v", got, want)
	}

	// A gardenio-cli viewer token sending the same header keeps only the
	// groups in its own signed claim.
	cliTok := kc.mint(t, testPublicIssuer, testAudience, claims("gardenio-cli"))
	if got, want := groupsProbe(t, testEdgeClients, v, cliTok, forged), []string{"/own-group"}; !reflect.DeepEqual(got, want) {
		t.Errorf("gardenio-cli groups = %v, want %v", got, want)
	}
}
