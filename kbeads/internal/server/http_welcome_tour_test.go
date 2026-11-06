package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The welcome tour's seen flag (step 9.14b): a person stamps their own, once,
// and nobody else's.

func welcomeTourRequest(ctx context.Context, method string) *http.Request {
	return httptest.NewRequest(method, "/v1/users/me/welcome-tour", nil).WithContext(ctx)
}

func TestMyWelcomeTour_PersonStampsOwnOnce(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLinkUserBead(ms, "u-alice", "auto-provision", map[string]any{"email": "alice@example.com"})
	seedLinkUserBead(ms, "u-bob", "auto-provision", map[string]any{"email": "bob@example.com"})
	bobBefore := string(ms.beads["u-bob"].Fields)
	// a gatehouse guest: a land group, the member role only
	alice := WithGroups(oidcCtx("alice@example.com", []string{RoleProjectMember}), []string{"/project-gatehouse"})

	var resp WelcomeTourResponse
	rec := httptest.NewRecorder()
	s.handleMyWelcomeTour(rec, welcomeTourRequest(alice, http.MethodGet))
	requireStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &resp)
	if resp.UserBeadID != "u-alice" || resp.SeenAt != "" {
		t.Fatalf("before the tour: %+v, want alice's bead and no seen_at", resp)
	}

	rec = httptest.NewRecorder()
	s.handleSeenMyWelcomeTour(rec, welcomeTourRequest(alice, http.MethodPost))
	requireStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &resp)
	first := resp.SeenAt
	if first == "" || welcomeTourSeenAt(ms.beads["u-alice"]) != first {
		t.Fatalf("seen_at not stamped on alice's bead: resp %+v, fields %s", resp, ms.beads["u-alice"].Fields)
	}
	if string(ms.beads["u-bob"].Fields) != bobBefore {
		t.Fatalf("another person's bead changed")
	}

	// A second device ending the tour keeps the first time.
	ms.beads["u-alice"].Fields = []byte(`{"email":"alice@example.com","welcome_tour_seen_at":"2026-01-02T03:04:05Z"}`)
	rec = httptest.NewRecorder()
	s.handleSeenMyWelcomeTour(rec, welcomeTourRequest(alice, http.MethodPost))
	requireStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &resp)
	if resp.SeenAt != "2026-01-02T03:04:05Z" || welcomeTourSeenAt(ms.beads["u-alice"]) != "2026-01-02T03:04:05Z" {
		t.Fatalf("a second stamp moved seen_at: resp %+v, fields %s", resp, ms.beads["u-alice"].Fields)
	}
}

func TestMyWelcomeTour_Refusals(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLinkUserBead(ms, "u-alice", "auto-provision", map[string]any{"email": "alice@example.com"})
	before := string(ms.beads["u-alice"].Fields)

	// Static, agent, machine and land principals have no "me". The land
	// principal carries an oidc-typed identity on purpose: only the bare
	// != authMethodOIDC test refuses it (a !roleBearing test would admit it).
	for name, ctx := range map[string]context.Context{
		"static token":    WithAuthIdentity(withAuthMethod(context.Background(), authMethodStatic), AuthIdentity{Type: "static", Subject: "static"}),
		"agent fleet":     agentFleetCtx("gd-crew", "crew-a"),
		"machine account": WithAuthIdentity(withAuthMethod(context.Background(), authMethodMachine), AuthIdentity{Type: "machine", Subject: "machine:beads-viewer"}),
		"land principal":  WithAuthIdentity(withAuthMethod(context.Background(), authMethodLand), AuthIdentity{Type: "oidc", Subject: "alice@example.com"}),
		"applicant":       oidcCtx("alice@example.com", nil),
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := httptest.NewRecorder()
			if method == http.MethodPost {
				s.handleSeenMyWelcomeTour(rec, welcomeTourRequest(ctx, method))
			} else {
				s.handleMyWelcomeTour(rec, welcomeTourRequest(ctx, method))
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s: status %d, want 403", name, method, rec.Code)
			}
		}
	}
	if string(ms.beads["u-alice"].Fields) != before {
		t.Fatalf("a refused request wrote: %s", ms.beads["u-alice"].Fields)
	}

	rec := httptest.NewRecorder()
	s.handleSeenMyWelcomeTour(rec, welcomeTourRequest(oidcCtx("nobody@example.com", []string{RoleOperator}), http.MethodPost))
	requireStatus(t, rec, http.StatusNotFound)
}
