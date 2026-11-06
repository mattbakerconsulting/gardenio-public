package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
)

// TestForwardAuthEmailIgnoredForNonEdgeClients pins gd-uxLVWgL9qk: the
// client-controllable X-Auth-Request-Email header must not give a human
// identity to a valid realm token that lacks an email claim unless the token
// was minted for the edge forward-auth client. A service account carrying the
// kbeads audience would otherwise impersonate any human by setting the header.
func TestForwardAuthEmailIgnoredForNonEdgeClients(t *testing.T) {
	const victim = "victim@corp.example.com"
	roles := []string{RoleViewer, RoleOperator}
	verifier := &oidcIssuerMock{
		tokens: map[string]tokenInfo{
			"edge-tok":    {roles: roles, azp: "edge-client"},
			"sa-tok":      {roles: roles, azp: "service-account-bridge"},
			"no-azp-tok":  {roles: roles},
			"with-email":  {email: "real@corp.example.com", roles: roles, azp: "service-account-bridge"},
			"edge-w-mail": {email: "real@corp.example.com", roles: roles, azp: "edge-client"},
		},
	}

	tests := []struct {
		name        string
		edgeClients []string
		token       string
		wantSubject string
	}{
		{"edge client token takes the header", []string{"edge-client"}, "edge-tok", victim},
		{"service-account token ignores the header", []string{"edge-client"}, "sa-tok", ""},
		{"token without azp ignores the header", []string{"edge-client"}, "no-azp-tok", ""},
		{"no edge client configured ignores the header", nil, "edge-tok", ""},
		{"email claim wins over the header for a service account", []string{"edge-client"}, "with-email", "real@corp.example.com"},
		{"email claim wins over the header for the edge client", []string{"edge-client"}, "edge-w-mail", "real@corp.example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewBeadsServer(newMockStore(), &events.NoopPublisher{})
			srv.SetForwardAuthClients(tc.edgeClients)

			var got AuthIdentity
			probe := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = AuthIdentityFromContext(r.Context())
			})
			h := authMiddleware(srv.forwardAuthClients, withTestToken(nil, "primary"), verifier, probe)

			req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			req.Header.Set("X-Auth-Request-Email", victim)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (token itself is valid)", rec.Code)
			}
			if got.Type != "oidc" {
				t.Errorf("identity type = %q, want oidc", got.Type)
			}
			if got.Subject != tc.wantSubject {
				t.Errorf("subject = %q, want %q", got.Subject, tc.wantSubject)
			}
		})
	}
}
