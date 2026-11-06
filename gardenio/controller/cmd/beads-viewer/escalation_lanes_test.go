package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// oidcViewerCtx is a request context as the Keycloak path builds it: a
// verified token with roles and groups.
func oidcViewerCtx(email string, roles, groups []string) context.Context {
	ctx := oidcauth.ContextWithEmail(context.Background(), email)
	ctx = oidcauth.ContextWithOIDCVerified(ctx)
	ctx = oidcauth.ContextWithRoles(ctx, roles)
	return oidcauth.ContextWithGroups(ctx, groups)
}

func TestViewerLaneRefs(t *testing.T) {
	srv := newTestServerForEsc(t, &fakeDaemon{})
	cases := []struct {
		name  string
		ctx   context.Context
		email string
		want  []string
	}{
		{"anonymous", context.Background(), "", nil},
		{"admin", oidcViewerCtx("Op@Acme.example", []string{oidcauth.RoleAdmin}, nil), "Op@Acme.example", []string{"operator"}},
		{"land owner, not admin", oidcViewerCtx("o@acme.example", []string{oidcauth.RoleProjectMember}, []string{"/project-acme/owners", "/project-beta"}), "o@acme.example",
			[]string{"land-owner:acme"}},
		{"viewer role holds no lane", oidcViewerCtx("v@acme.example", []string{oidcauth.RoleViewer}, nil), "v@acme.example", []string{}},
		// The trusted edge (dev bypass, AVA) carries no roles and no verified
		// token; the viewer already treats it as unrestricted.
		{"edge identity", oidcauth.ContextWithEmail(context.Background(), "e@acme.example"), "e@acme.example", []string{"operator"}},
	}
	for _, c := range cases {
		if got := srv.viewerLaneRefs(c.ctx, c.email); !slices.Equal(got, c.want) || (got == nil) != (c.want == nil) {
			t.Errorf("%s: lanes = %#v, want %#v", c.name, got, c.want)
		}
	}
}

func landEscalations(now time.Time) []beadsapi.EscalationDetail {
	return []beadsapi.EscalationDetail{
		// #756 shapes: an unstamped one (audience only) and a stamped owner one.
		{Issue: makeEscBead("gd-legacy", "legacy, unstamped", "manual_owner_action", "internal", "open", now.Add(-4*time.Hour),
			map[string]string{"audience": "owner"})},
		{Issue: makeEscBead("gd-mine", "held by the owner", "manual_owner_action", "internal", "open", now.Add(-3*time.Hour),
			map[string]string{"held_by": "operator", "audience": "owner"})},
		{Issue: makeEscBead("gd-acme", "acme merge, held by its owner", "gitlab_mr_merge", "internal", "open", now.Add(-2*time.Hour),
			map[string]string{"held_by": "land-owner:acme", "audience": "owner"})},
		{Issue: makeEscBead("gd-laptop", "held by the laptop", "helm_deploy", "internal", "open", now.Add(-1*time.Hour),
			map[string]string{"held_by": "coordinator:laptop", "audience": "operator"})},
	}
}

// TestHandleEscalations_LandOwnerFoldsForOthers pins Review Focus 6 and D9: a
// land owner's escalation is owner audience but not the gardenio owner's; it
// folds into In hand as "land owners" for anyone who does not own the land.
func TestHandleEscalations_LandOwnerFoldsForOthers(t *testing.T) {
	srv := newTestServerForEsc(t, &fakeDaemon{escalationsResp: landEscalations(time.Now())})
	get := func(ctx context.Context) string {
		rec := httptest.NewRecorder()
		srv.handleEscalations(rec, httptest.NewRequest(http.MethodGet, "/escalations", nil).WithContext(ctx))
		return rec.Body.String()
	}
	body := get(oidcViewerCtx("op@acme.example", []string{oidcauth.RoleAdmin}, nil))
	listed, inHand, _ := strings.Cut(body, `id="escalations-inhand"`)
	if !strings.Contains(listed, `id="escalation-row-gd-mine"`) || strings.Contains(listed, `id="escalation-row-gd-acme"`) {
		t.Errorf("gardenio owner: gd-mine must be listed, gd-acme folded\n%s", body)
	}
	if !strings.Contains(inHand, `id="escalation-row-gd-acme"`) || !strings.Contains(body, "In hand: 2 (operator 1, land owners 1)") {
		t.Errorf("gardenio owner: in-hand fold wrong\n%s", body)
	}
	// Positive control: acme's owner sees it as theirs.
	body = get(oidcViewerCtx("o@acme.example", []string{oidcauth.RoleProjectMember}, []string{"/project-acme/owners"}))
	listed, _, _ = strings.Cut(body, `id="escalations-inhand"`)
	if !strings.Contains(listed, `id="escalation-row-gd-acme"`) {
		t.Errorf("acme owner: gd-acme not listed as theirs\n%s", body)
	}
	// Anonymous (no lanes): #756's behaviour, every owner-audience row listed.
	body = get(context.Background())
	listed, _, _ = strings.Cut(body, `id="escalations-inhand"`)
	if !strings.Contains(listed, `id="escalation-row-gd-acme"`) {
		t.Errorf("anonymous: gd-acme folded; want #756's listing\n%s", body)
	}
}

// TestHandleEscalations_UnstampedFollowsAudience pins Review Focus 5: an
// escalation with no held_by is shown where #756's audience puts it, never
// dropped; and In-hand rows name the holding lane.
func TestHandleEscalations_UnstampedFollowsAudience(t *testing.T) {
	now := time.Now()
	escs := append(landEscalations(now),
		beadsapi.EscalationDetail{Issue: makeEscBead("gd-old-desk", "legacy desk merge", "github_pr_merge", "internal", "open", now, map[string]string{"audience": "desk"})})
	srv := newTestServerForEsc(t, &fakeDaemon{escalationsResp: escs})
	rec := httptest.NewRecorder()
	srv.handleEscalations(rec, httptest.NewRequest(http.MethodGet, "/escalations", nil).WithContext(oidcViewerCtx("op@acme.example", []string{oidcauth.RoleAdmin}, nil)))
	body := rec.Body.String()
	listed, inHand, _ := strings.Cut(body, `id="escalations-inhand"`)
	if !strings.Contains(listed, `id="escalation-row-gd-legacy"`) {
		t.Errorf("unstamped owner-audience escalation not listed\n%s", body)
	}
	if !strings.Contains(inHand, `id="escalation-row-gd-old-desk"`) {
		t.Errorf("unstamped desk escalation vanished instead of folding\n%s", body)
	}
	if !strings.Contains(inHand, ">coordinator laptop</span>") {
		t.Errorf("in-hand row does not name its lane\n%s", body)
	}
	if !strings.Contains(inHand, ">desk</span>") {
		t.Errorf("an unstamped in-hand row lost #756's audience chip\n%s", body)
	}
}

func TestEscalationCounts_FoldOtherLands(t *testing.T) {
	now := time.Now()
	srv := newTestServerForEsc(t, &fakeDaemon{escalationsResp: landEscalations(now)})
	ctx := oidcViewerCtx("op@acme.example", []string{oidcauth.RoleAdmin}, nil)
	if v := srv.buildEscalationsTileView(ctx, now); v.Pending != 2 || v.InHand != 2 {
		t.Errorf("tile = pending %d in hand %d, want 2 and 2 (gd-acme folds)", v.Pending, v.InHand)
	}
	if v := srv.computeIndexStats(ctx); !v.EscalationsOK || v.EscalationsPending != 2 {
		t.Errorf("index banner = %d (ok=%v), want 2", v.EscalationsPending, v.EscalationsOK)
	}
	// No identity: #756's counts exactly.
	if v := srv.buildEscalationsTileView(context.Background(), now); v.Pending != 3 {
		t.Errorf("anonymous tile Pending = %d, want 3", v.Pending)
	}
}
