package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// Escalation audience on beads-viewer (gd-bmOiteQi36).

func audienceQueue(now time.Time) []beadsapi.EscalationDetail {
	return []beadsapi.EscalationDetail{
		{Issue: makeEscBead("gd-own", "Pick the IdP", "policy_choice", "internal", "open", now.Add(-3*time.Hour), nil)},
		{Issue: makeEscBead("gd-desk", "Merge PR #722", "gitlab_mr_merge", "internal", "open", now.Add(-2*time.Hour), map[string]string{"audience": ""})},
		{Issue: makeEscBead("gd-ops", "Apply the manifest", "k8s_apply", "internal", "open", now.Add(-time.Hour), map[string]string{"audience": "operator"})},
		{Issue: makeEscBead("gd-cred", "Rotate with sudo", "gitlab_mr_merge", "internal", "open", now.Add(-4*time.Hour), map[string]string{
			"audience": "desk", "credential_requirements": `[{"type":"github_sudo"}]`,
		})},
	}
}

func TestHandleEscalations_FoldsInHand(t *testing.T) {
	now := time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)
	srv := newTestServerForEsc(t, &fakeDaemon{escalationsResp: audienceQueue(now)})
	rec := httptest.NewRecorder()
	srv.handleEscalations(rec, httptest.NewRequest(http.MethodGet, "/escalations", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	fold := strings.Index(body, `id="escalations-inhand"`)
	if fold < 0 {
		t.Fatalf("no In hand fold:\n%s", body)
	}
	if !strings.Contains(body, "<summary>In hand: 2 (desk 1, operator 1)</summary>") {
		t.Errorf("fold line wrong:\n%s", body)
	}
	if !strings.Contains(body, `<span class="meta">(2)</span>`) {
		t.Errorf("heading should count the owner's 2, not all 4")
	}
	for _, id := range []string{"gd-own", "gd-cred"} { // the owner's, credentials forcing gd-cred
		if i := strings.Index(body, `id="escalation-row-`+id+`"`); i < 0 || i > fold {
			t.Errorf("%s should be listed above the fold", id)
		}
	}
	for _, id := range []string{"gd-desk", "gd-ops"} {
		if i := strings.Index(body, `id="escalation-row-`+id+`"`); i < fold {
			t.Errorf("%s should be inside the fold", id)
		}
	}
}

func TestHandleEscalations_OnlyInHand(t *testing.T) {
	now := time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)
	q := audienceQueue(now)[1:3]
	srv := newTestServerForEsc(t, &fakeDaemon{escalationsResp: q})
	rec := httptest.NewRecorder()
	srv.handleEscalations(rec, httptest.NewRequest(http.MethodGet, "/escalations", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Nothing here needs you.") || !strings.Contains(body, "In hand: 2") {
		t.Errorf("an all-in-hand queue should say nothing needs you and fold the rest:\n%s", body)
	}
}

func TestBuildEscalationsTileView_CountsTheOwnersOnly(t *testing.T) {
	now := time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)
	srv := newTestServerForEsc(t, &fakeDaemon{escalationsResp: audienceQueue(now)})
	v := srv.buildEscalationsTileView(context.Background(), now)
	if v.Pending != 2 || v.InHand != 2 {
		t.Errorf("Pending = %d, InHand = %d; want 2 and 2", v.Pending, v.InHand)
	}
	for _, r := range v.Next {
		if r.ID == "gd-desk" || r.ID == "gd-ops" {
			t.Errorf("an in-hand escalation %s is in the tile's next list", r.ID)
		}
	}
}

func TestEscalationView_Audience(t *testing.T) {
	for _, c := range []struct {
		fields         map[string]string
		want           string
		forced, isDflt bool
	}{
		{map[string]string{"operation_type": "gitlab_mr_merge"}, "desk", false, true},
		{map[string]string{"operation_type": "gitlab_mr_merge", "audience": "owner"}, "owner", false, false},
		{map[string]string{"operation_type": "k8s_rbac", "audience": "operator", "sensitivity": "secret"}, "owner", true, false},
	} {
		v := buildEscalationView(&beadsapi.BeadDetail{ID: "gd-x", Type: "escalation", Status: "open", Fields: c.fields}, nil, "", time.Now())
		if v.Audience != c.want || v.AudienceForced != c.forced || v.AudienceDefault != c.isDflt {
			t.Errorf("%v: audience %q forced %v default %v; want %q %v %v", c.fields, v.Audience, v.AudienceForced, v.AudienceDefault, c.want, c.forced, c.isDflt)
		}
		for _, a := range v.RouteTo {
			if a == v.Audience {
				t.Errorf("RouteTo offers the current audience %q", a)
			}
		}
		for _, f := range v.Fields {
			if f.Key == "audience" {
				t.Error("audience is repeated in the other-fields tail")
			}
		}
	}
}

// routePost posts a re-route as email holding roles, through the real routes.
func routePost(t *testing.T, fake *fakeDaemon, audience string, roles ...string) *httptest.ResponseRecorder {
	t.Helper()
	srv := NewServer(fake, slog.Default(), "", "http://test", "", nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/b/gd-esc/escalation/route", strings.NewReader("audience="+audience))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := oidcauth.ContextWithEmail(req.Context(), "approver@acme.example")
	ctx = oidcauth.ContextWithRoles(ctx, roles)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

func TestRouteEscalation_OperatorReRoutesThroughTheDaemon(t *testing.T) {
	fake := escActionFake("open", `[]`)
	rec := routePost(t, fake, "owner", oidcauth.RoleOperator)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(fake.routeEscIDs) != 1 || fake.routeEscIDs[0] != "gd-esc" || fake.routeEscCalls[0].Audience != "owner" {
		t.Errorf("routed %v %v, want gd-esc to owner", fake.routeEscIDs, fake.routeEscCalls)
	}
	if fake.updateFieldsCalls != 0 || fake.addCommentCalls != 0 {
		t.Errorf("wrote the field or the comment itself (%d, %d); the daemon's route endpoint does both", fake.updateFieldsCalls, fake.addCommentCalls)
	}
	if body := rec.Body.String(); !strings.Contains(body, "for owner") {
		t.Errorf("re-rendered card does not show the new audience:\n%s", body)
	}
}

func TestRouteEscalation_RefusesNonOperators(t *testing.T) {
	for _, roles := range [][]string{nil, {oidcauth.RoleViewer}} {
		fake := escActionFake("open", `["approve:escalation"]`) // may approve, still may not route
		rec := routePost(t, fake, "owner", roles...)
		if rec.Code != http.StatusForbidden {
			t.Errorf("roles %v: status = %d, want 403", roles, rec.Code)
		}
		if len(fake.routeEscIDs) != 0 {
			t.Errorf("roles %v: a refused route reached the daemon", roles)
		}
	}
}

func TestRouteEscalation_ForcedOwnerStaysWithTheOwner(t *testing.T) {
	fake := escActionFake("open", `[]`)
	fake.beads["gd-esc"].Fields["sensitivity"] = "secret"
	rec := routePost(t, fake, "desk", oidcauth.RoleAdmin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if len(fake.routeEscIDs) != 0 {
		t.Error("a forced-owner escalation was routed away")
	}
}

func TestRouteEscalation_RejectsUnknownAudience(t *testing.T) {
	fake := escActionFake("open", `[]`)
	if rec := routePost(t, fake, "everyone", oidcauth.RoleAdmin); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestEscalationApprove_IgnoresAudience: approval authz does not change with
// the audience. The viewer forwards whatever the daemon's can-approve answers,
// so a yes approves a desk, an operator and an owner escalation alike, and a no
// is refused alike.
func TestEscalationApprove_IgnoresAudience(t *testing.T) {
	for _, aud := range []string{"", "desk", "operator", "owner"} {
		for _, c := range []struct {
			allowed bool
			want    int
		}{{true, http.StatusOK}, {false, http.StatusForbidden}} {
			fake := escActionFake("open", `["approve:escalation"]`)
			fake.beads["gd-esc"].Fields["audience"] = aud
			fake.canApproveResp["gd-esc"] = beadsapi.CanApproveResult{Allowed: c.allowed}
			rec := escPost(t, fake, "gd-esc", "approve", "approver@acme.example", "")
			if rec.Code != c.want {
				t.Errorf("audience %q allowed %v: status = %d, want %d", aud, c.allowed, rec.Code, c.want)
			}
			if got := len(fake.approveEscCalls); got != map[bool]int{true: 1, false: 0}[c.want == http.StatusOK] {
				t.Errorf("audience %q allowed %v: %d approve calls", aud, c.allowed, got)
			}
		}
	}
}
