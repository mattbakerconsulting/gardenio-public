package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/oidcauth"
)

// shareFake is the viewer's daemon with the window routes: the preview, one
// window on this plan and one on another plan of the same land.
type shareFake struct {
	*fakeDaemon
	err      error
	minted   []int
	revoked  []string
	previews []int
}

const sharePreviewPayload = `{
  "plan": {"title": "Orchard seed swap", "state": "building", "current_phase": 1, "steps_done": 1, "steps_total": 2},
  "phases": [{"number": 1, "title": "Groundwork", "status": "open", "steps": [
    {"number": 1, "title": "Pick a name", "status": "closed", "kind": "task", "done_at": "2026-10-20T09:15:00Z"},
    {"number": 2, "title": "Ask someone@example.com about the card", "status": "open", "kind": "task"}
  ]}],
  "narrative": {"headline": "Building the swap", "docs": [{"title": "Narrative", "content": "We are building a seed swap."}]},
  "expires_at": "2026-11-28T00:00:00Z",
  "updated_at": "2026-10-29T00:00:00Z"
}`

func (f *shareFake) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	return &beadsapi.BeadDetail{ID: id, Title: "Orchard seed swap", Type: "epic", Labels: []string{"plan", "project:claimlayer"}}, nil
}

func (f *shareFake) PreviewWindow(_ context.Context, _ string, days int) (json.RawMessage, error) {
	f.previews = append(f.previews, days)
	if f.err != nil {
		return nil, f.err
	}
	return json.RawMessage(sharePreviewPayload), nil
}

func (f *shareFake) CreateWindow(_ context.Context, planID string, days int) (*beadsapi.MintedWindow, error) {
	f.minted = append(f.minted, days)
	if f.err != nil {
		return nil, f.err
	}
	return &beadsapi.MintedWindow{
		Window: beadsapi.Window{ID: "gd-wnew", PlanID: planID, Land: "claimlayer", State: "live", ExpiresAt: time.Now().AddDate(0, 0, days)},
		Token:  "gw_TOKEN", Path: "/w/gw_TOKEN",
	}, nil
}

func (f *shareFake) ListWindows(_ context.Context, land string) ([]beadsapi.Window, error) {
	if f.err != nil {
		return nil, f.err
	}
	seen := time.Now().Add(-time.Hour)
	return []beadsapi.Window{
		{ID: "gd-w1", PlanID: "gd-plan", Land: land, State: "live", ViewCount: 42, LastViewedAt: &seen, ExpiresAt: time.Now().AddDate(0, 0, 20), CreatedBy: "owner@claimlayer.example"},
		{ID: "gd-w0", PlanID: "gd-plan", Land: land, State: "revoked", ExpiresAt: time.Now().AddDate(0, 0, 5)},
		{ID: "gd-wother", PlanID: "gd-other", Land: land, State: "live", ExpiresAt: time.Now().AddDate(0, 0, 5)},
	}, nil
}

func (f *shareFake) RevokeWindow(_ context.Context, id string) (*beadsapi.Window, error) {
	f.revoked = append(f.revoked, id)
	if f.err != nil {
		return nil, f.err
	}
	return &beadsapi.Window{ID: id, State: "revoked"}, nil
}

func shareServer(t *testing.T, f *shareFake) *Server {
	t.Helper()
	return NewServer(f, slog.Default(), "", "", "", nil)
}

func shareRequest(method, target string, form url.Values) *http.Request {
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.Host = "gardenio.clonk.tv"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.SetPathValue("id", "gd-plan")
	return r.WithContext(beadsapi.WithUserBearer(r.Context(), "user-jwt"))
}

func TestPlanShare_PanelPreviewAndWindows(t *testing.T) {
	f := &shareFake{fakeDaemon: &fakeDaemon{}}
	s := shareServer(t, f)
	rec := httptest.NewRecorder()
	s.handlePlanShare(rec, shareRequest(http.MethodGet, "/plans/gd-plan/share?days=7", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"What a visitor sees", `href="/plans/gd-plan/share/preview?days=7"`,
		"1 field is withheld", "withheld: contains an internal reference", "phases[0].steps[1].title",
		"Anyone who has this link", "unfurl",
		"gd-w1", "42", "gd-w0", `/plans/gd-plan/share/gd-w1/revoke`,
		`<option value="7" selected>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("panel lacks %q", want)
		}
	}
	for _, leak := range []string{"someone@example.com", "gd-wother", "/share/gd-w0/revoke", "gw_"} {
		if strings.Contains(body, leak) {
			t.Errorf("panel carries %q", leak)
		}
	}
	if len(f.previews) != 1 || f.previews[0] != 7 {
		t.Errorf("previews = %v, want one of 7 days", f.previews)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q", cc)
	}
}

// TestPlanShare_Preview: the preview is the window page itself (window.html's
// window_page), withheld fields in place, with the window's headers and no
// script.
func TestPlanShare_Preview(t *testing.T) {
	f := &shareFake{fakeDaemon: &fakeDaemon{}}
	s := shareServer(t, f)
	rec := httptest.NewRecorder()
	s.handlePlanSharePreview(rec, shareRequest(http.MethodGet, "/plans/gd-plan/share/preview?days=90", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`class="card w-window" id="w-main"`, "A window on a plan · read-only",
		"Orchard seed swap", "Groundwork", "Pick a name", "We are building a seed swap.",
		"withheld: contains an internal reference", "This link stops working on 28 November 2026.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview lacks %q", want)
		}
	}
	for _, leak := range []string{"someone@example.com", "gw_"} {
		if strings.Contains(body, leak) {
			t.Errorf("preview carries %q", leak)
		}
	}
	if len(f.previews) != 1 || f.previews[0] != 90 {
		t.Errorf("previews = %v, want one of 90 days", f.previews)
	}
	h := rec.Header()
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
	if h.Get("Referrer-Policy") != "no-referrer" || !strings.Contains(h.Get("Cache-Control"), "no-store") {
		t.Errorf("headers = %v", h)
	}

	// A refusal is the daemon's status and the panel's sentence, no page.
	f.err = &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "no"}
	rec = httptest.NewRecorder()
	s.handlePlanSharePreview(rec, shareRequest(http.MethodGet, "/plans/gd-plan/share/preview", nil))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Only the owner of this plan's land") || strings.Contains(rec.Body.String(), "w-window") {
		t.Errorf("refused preview: %d\n%s", rec.Code, rec.Body.String())
	}
}

func TestPlanShare_MintShowsURLOnce(t *testing.T) {
	f := &shareFake{fakeDaemon: &fakeDaemon{}}
	s := shareServer(t, f)
	rec := httptest.NewRecorder()
	s.handlePlanShareMint(rec, shareRequest(http.MethodPost, "/plans/gd-plan/share", url.Values{"days": {"90"}}))
	body := rec.Body.String()
	for _, want := range []string{"https://gardenio.clonk.tv/w/gw_TOKEN", "shown once", "Anyone who has this link"} {
		if !strings.Contains(body, want) {
			t.Errorf("mint answer lacks %q:\n%s", want, body)
		}
	}
	if len(f.minted) != 1 || f.minted[0] != 90 {
		t.Errorf("minted = %v, want one of 90 days", f.minted)
	}
	// A lifetime the form does not offer falls back to the default.
	s.handlePlanShareMint(httptest.NewRecorder(), shareRequest(http.MethodPost, "/plans/gd-plan/share", url.Values{"days": {"9999"}}))
	if f.minted[1] != planShareDefaultDays {
		t.Errorf("minted with days=9999: %d, want %d", f.minted[1], planShareDefaultDays)
	}
	// The panel the GET answers never carries the URL again.
	rec = httptest.NewRecorder()
	s.handlePlanShare(rec, shareRequest(http.MethodGet, "/plans/gd-plan/share", nil))
	if strings.Contains(rec.Body.String(), "gw_TOKEN") {
		t.Error("the panel shows the token again")
	}
}

func TestPlanShare_Revoke(t *testing.T) {
	f := &shareFake{fakeDaemon: &fakeDaemon{}}
	s := shareServer(t, f)
	r := shareRequest(http.MethodPost, "/plans/gd-plan/share/gd-w1/revoke", url.Values{})
	r.SetPathValue("window", "gd-w1")
	rec := httptest.NewRecorder()
	s.handlePlanShareRevoke(rec, r)
	if len(f.revoked) != 1 || f.revoked[0] != "gd-w1" || !strings.Contains(rec.Body.String(), "Window gd-w1 is revoked") {
		t.Errorf("revoked %v, body:\n%s", f.revoked, rec.Body.String())
	}
}

// TestPlanShare_ScopedMember: kbeads refuses a member, and the panel says who
// may share instead of rendering anything of the plan.
func TestPlanShare_ScopedMember(t *testing.T) {
	f := &shareFake{fakeDaemon: &fakeDaemon{}, err: &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "only the owner of land claimlayer or an operator, on their own sign-in, may manage its windows"}}
	s := shareServer(t, f)
	for _, run := range []func(http.ResponseWriter, *http.Request){s.handlePlanShare, s.handlePlanShareMint} {
		rec := httptest.NewRecorder()
		run(rec, shareRequest(http.MethodPost, "/plans/gd-plan/share", url.Values{}))
		body := rec.Body.String()
		if !strings.Contains(body, "Only the owner of this plan&#39;s land or an operator can share it.") || strings.Contains(body, "What a visitor sees") {
			t.Errorf("refused panel:\n%s", body)
		}
	}
}

func TestPlanShare_NoBearerIsRefusedUnsent(t *testing.T) {
	if got := planShareRefusal(beadsapi.ErrUserBearerRequired); !strings.Contains(got, "own sign-in") {
		t.Errorf("refusal for a missing bearer = %q", got)
	}
	if got := planShareRefusal(errNoPlanShareAPI); strings.Contains(got, "owner") {
		t.Errorf("refusal for a client without window routes = %q", got)
	}
}

func TestPlanShareOffered(t *testing.T) {
	req := func(roles []string, sc *viewerScope) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/plans/gd-plan", nil)
		ctx := oidcauth.ContextWithRoles(r.Context(), roles)
		if sc != nil {
			ctx = withViewerScope(ctx, *sc)
		}
		return r.WithContext(ctx)
	}
	owner := viewerScope{Projects: []string{"claimlayer"}, Roles: map[string]conversation.LandRole{"claimlayer": conversation.LandRoleOwner}}
	member := viewerScope{Projects: []string{"claimlayer"}, Roles: map[string]conversation.LandRole{"claimlayer": conversation.LandRoleMember}}
	for name, c := range map[string]struct {
		r       *http.Request
		project string
		want    bool
	}{
		"operator":             {req([]string{oidcauth.RoleOperator}, nil), "claimlayer", true},
		"unrestricted viewer":  {req([]string{oidcauth.RoleViewer}, nil), "claimlayer", false},
		"owner of the land":    {req([]string{oidcauth.RoleProjectMember}, &owner), "claimlayer", true},
		"owner of another":     {req([]string{oidcauth.RoleProjectMember}, &owner), "gardenio", false},
		"member of the land":   {req([]string{oidcauth.RoleProjectMember}, &member), "claimlayer", false},
		"owner, plan unlabled": {req([]string{oidcauth.RoleProjectMember}, &owner), "", false},
	} {
		if got := planShareOffered(c.r, c.project); got != c.want {
			t.Errorf("%s: planShareOffered = %v, want %v", name, got, c.want)
		}
	}
}
