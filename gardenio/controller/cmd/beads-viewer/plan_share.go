package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/planwindow"
)

// The cockpit's Share panel (Lands: the Window, PW.4 gd-tJe0GrtFDw; narrative
// PW-D7): a land owner or an operator mints a read-only public window on the
// plan, sees the exact page a visitor will get before minting (window.go's
// render, served as the preview), is shown the URL once, and lists and revokes
// the plan's windows with their view counts.
//
//	GET  /plans/{id}/share                       the panel: withheld fields, mint form, windows
//	GET  /plans/{id}/share/preview               the window page a visitor would see
//	POST /plans/{id}/share                       mint; the panel again with the URL, once
//	POST /plans/{id}/share/{window}/revoke       revoke; the panel again
//
// kbeads is the gate: /v1/windows admits only the owner tier of the plan's land
// or an unrestricted operator, on their own sign-in, and refuses the viewer's
// service credential. So every daemon call here goes out on the signed-in
// person's bearer alone (beadsapi.WithUserBearerOnly), never falling back to
// the viewer's own token, and the routes are routeScoped: a scoped owner
// reaches them and kbeads decides. The disclosure on /plans/{id} renders only
// for a caller planShareOffered admits, so a member is not shown a button
// that would be refused.

// planShareExpiries are the lifetimes the mint form offers, in days.
var planShareExpiries = []int{7, 30, 90, 365}

// planShareDefaultDays is the form's preselected lifetime (kbeads' default).
const planShareDefaultDays = 30

// planShareGrants is the plain sentence about what a window link grants
// (the same words gb plan share prints).
const planShareGrants = "Anyone who has this link can read the plan: its title, state, steps, decisions, crew and narrative, with no account, until it expires or is revoked. Chat apps that unfurl links (Slack, Discord, iMessage) will fetch the page too."

// planShareOffered reports whether the Share disclosure is shown to the
// caller for a plan in project: an unrestricted operator, or a scoped owner of
// that land. kbeads checks again on every call.
func planShareOffered(r *http.Request, project string) bool {
	return windowShareOffered(r.Context(), project)
}

// windowShareOffered is planShareOffered on a context: the plan's Share
// disclosure and the land's Share tab (land_share.go) are offered alike.
func windowShareOffered(ctx context.Context, project string) bool {
	sc := viewerScopeFromContext(ctx)
	if !sc.Scoped() {
		roles := oidcauth.RolesFromContext(ctx)
		return len(roles) == 0 || oidcauth.HasMinimumRole(roles, oidcauth.RoleOperator) // viewerCanOperate's rule
	}
	return project != "" && sc.landRole(project) == conversation.LandRoleOwner
}

// planShareCtx is ctx on which every daemon call authenticates as the signed-in
// person and nothing else (with no bearer the call fails closed, unsent).
func planShareCtx(ctx context.Context) context.Context {
	return beadsapi.WithUserBearerOnly(ctx, beadsapi.UserBearerFromContext(ctx))
}

// planShareWindowURL is the absolute URL of a window path (/w/<token>): the
// viewer's public origin, with no base path, where the edge serves /w/.
func planShareWindowURL(r *http.Request, path string) string {
	return viewerBaseFromRequest(r, "") + path
}

// planShareRefusal turns a daemon error into the sentence the panel shows.
func planShareRefusal(err error) string {
	return windowShareRefusal(err, "Only the owner of this plan's land or an operator can share it.")
}

// windowShareRefusal is planShareRefusal with denied as the words for a 401
// or 403 (land_share.go says it of a land).
func windowShareRefusal(err error, denied string) string {
	var apiErr *beadsapi.APIError
	switch {
	case errors.Is(err, beadsapi.ErrUserBearerRequired):
		return "Sharing needs your own sign-in, and this session carries none. Sign in again and reopen Share."
	case errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusUnauthorized):
		return denied
	case errors.As(err, &apiErr) && apiErr.StatusCode < 500:
		return apiErr.Message
	}
	return "The windows could not be loaded just now. Try again in a moment."
}

// planShareAPI is the subset of beadsapi.Client the Share panel uses, kept off
// beadAPI so the viewer's daemon fakes need not grow it. (windowAPI, window.go,
// is the public page's one read on a window token.)
type planShareAPI interface {
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
	CreateWindow(ctx context.Context, planID string, expiresInDays int) (*beadsapi.MintedWindow, error)
	ListWindows(ctx context.Context, land string) ([]beadsapi.Window, error)
	RevokeWindow(ctx context.Context, id string) (*beadsapi.Window, error)
	PreviewWindow(ctx context.Context, planID string, expiresInDays int) (json.RawMessage, error)
}

// errNoPlanShareAPI means the viewer's daemon client cannot manage windows.
var errNoPlanShareAPI = errors.New("daemon client has no window routes")

// shareAPI returns the daemon client as a planShareAPI, nil when it is not one.
func (s *Server) shareAPI() planShareAPI {
	if s == nil || s.daemon == nil {
		return nil
	}
	c, _ := s.daemon.(planShareAPI)
	return c
}

// planSharePreviewCSP is the window page's policy with no script at all: the
// owner's preview is the page as first served, and window.js would poll a
// plan.json that only /w/{token} has.
var planSharePreviewCSP = strings.Replace(windowContentSecurityPolicy, "script-src 'self'", "script-src 'none'", 1)

// planShareSnapshot reads the preview payload on the person's bearer and
// builds it as the window page does (buildWindowSnapshot, window.go).
func (s *Server) planShareSnapshot(ctx context.Context, api planShareAPI, planID string, days int) (*windowSnapshot, error) {
	raw, err := api.PreviewWindow(ctx, planID, days)
	if err != nil {
		return nil, err
	}
	snap, err := buildWindowSnapshot(raw, time.Now())
	if err != nil {
		s.logger.Warn("plan share: preview payload did not decode", "plan", planID, "error", err)
		return nil, err
	}
	return snap, nil
}

// planSharePanel builds the panel's data: the fields the window would
// withhold, a link to the preview, the plan's windows and the mint form. A
// refusal or failure fills Error and leaves the rest empty.
func (s *Server) planSharePanel(r *http.Request, planID string, days int) map[string]any {
	data := map[string]any{
		"PlanID":   planID,
		"Expiries": planShareExpiries,
		"Days":     days,
		"Grants":   planShareGrants,
	}
	api := s.shareAPI()
	if api == nil {
		data["Error"] = planShareRefusal(errNoPlanShareAPI)
		return data
	}
	ctx := planShareCtx(r.Context())
	snap, err := s.planShareSnapshot(ctx, api, planID, days)
	if err != nil {
		data["Error"] = planShareRefusal(err)
		return data
	}
	data["Previewed"] = true
	data["Withheld"] = snap.view.Withheld
	data["WithheldText"] = planwindow.WithheldText

	plan, err := api.GetBead(ctx, planID)
	if err != nil {
		data["Error"] = planShareRefusal(err)
		return data
	}
	land := projectFromLabels(plan.Labels)
	windows, err := api.ListWindows(ctx, land)
	if err != nil {
		data["Error"] = planShareRefusal(err)
		return data
	}
	mine := make([]beadsapi.Window, 0, len(windows))
	for _, w := range windows {
		if w.PlanID == planID {
			mine = append(mine, w)
		}
	}
	data["Windows"] = mine
	data["Land"] = land
	return data
}

// planShareDays reads the requested lifetime, falling back to the default for
// anything the form does not offer.
func planShareDays(raw string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
		for _, d := range planShareExpiries {
			if n == d {
				return n
			}
		}
	}
	return planShareDefaultDays
}

// planShareReadable reads the plan on the person's own bearer and answers 404
// when they cannot read it, missing or in another land alike (beadMissing), so
// the panel never tells them a plan they cannot see exists. Any other failure
// passes: the panel shows its own refusal for it.
func (s *Server) planShareReadable(w http.ResponseWriter, r *http.Request, id string) bool {
	if _, err := s.daemon.GetBead(planShareCtx(r.Context()), id); beadMissing(err) {
		http.Error(w, "Plan not found", http.StatusNotFound)
		return false
	}
	return true
}

// handlePlanShare handles GET /plans/{id}/share: the panel body the
// disclosure lazy-loads (and re-renders when the lifetime changes, since the
// preview's footer names the expiry).
func (s *Server) handlePlanShare(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !s.planShareReadable(w, r, id) {
		return
	}
	data := s.planSharePanel(r, id, planShareDays(r.URL.Query().Get("days")))
	w.Header().Set("Cache-Control", "private, no-store")
	s.renderPartial(w, r, "templates/partials/plan_share.html", "plan_share_panel", data)
}

// handlePlanShareMint handles POST /plans/{id}/share: mint a window and answer
// the panel with its URL, shown this once (the daemon keeps only its hash).
func (s *Server) handlePlanShareMint(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !s.planShareReadable(w, r, id) {
		return
	}
	days := planShareDays(r.FormValue("days"))
	w.Header().Set("Cache-Control", "private, no-store")
	var minted *beadsapi.MintedWindow
	err := errNoPlanShareAPI
	if api := s.shareAPI(); api != nil {
		minted, err = api.CreateWindow(planShareCtx(r.Context()), id, days)
	}
	if err != nil {
		data := s.planSharePanel(r, id, days)
		data["MintError"] = planShareRefusal(err)
		s.renderPartial(w, r, "templates/partials/plan_share.html", "plan_share_panel", data)
		return
	}
	data := s.planSharePanel(r, id, days)
	data["Minted"] = map[string]any{
		"URL":       planShareWindowURL(r, minted.Path),
		"ID":        minted.Window.ID,
		"ExpiresAt": minted.Window.ExpiresAt,
	}
	s.renderPartial(w, r, "templates/partials/plan_share.html", "plan_share_panel", data)
}

// handlePlanShareRevoke handles POST /plans/{id}/share/{window}/revoke.
func (s *Server) handlePlanShareRevoke(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !s.planShareReadable(w, r, id) {
		return
	}
	days := planShareDays(r.FormValue("days"))
	w.Header().Set("Cache-Control", "private, no-store")
	err := errNoPlanShareAPI
	if api := s.shareAPI(); api != nil {
		_, err = api.RevokeWindow(planShareCtx(r.Context()), strings.TrimSpace(r.PathValue("window")))
	}
	data := s.planSharePanel(r, id, days)
	if err != nil {
		data["MintError"] = planShareRefusal(err)
	} else {
		data["Revoked"] = r.PathValue("window")
	}
	s.renderPartial(w, r, "templates/partials/plan_share.html", "plan_share_panel", data)
}

// handlePlanSharePreview handles GET /plans/{id}/share/preview: the exact page
// a window minted now would show a visitor (window.html's window_page, built
// as /w/{token} builds it), on the person's own bearer, with every withheld
// field reading planwindow.WithheldText where it stands. It carries the
// window page's headers, minus its script.
func (s *Server) handlePlanSharePreview(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !s.planShareReadable(w, r, id) {
		return
	}
	days := planShareDays(r.URL.Query().Get("days"))
	setWindowHeaders(w)
	w.Header().Set("Content-Security-Policy", planSharePreviewCSP)
	err := errNoPlanShareAPI
	var snap *windowSnapshot
	if api := s.shareAPI(); api != nil {
		snap, err = s.planShareSnapshot(planShareCtx(r.Context()), api, id, days)
	}
	if err != nil {
		status := http.StatusBadGateway
		var apiErr *beadsapi.APIError
		if errors.Is(err, beadsapi.ErrUserBearerRequired) {
			status = http.StatusUnauthorized
		} else if errors.As(err, &apiErr) && apiErr.StatusCode < 500 {
			status = apiErr.StatusCode
		}
		http.Error(w, planShareRefusal(err), status)
		return
	}
	s.renderWindow(w, http.StatusOK, "window_page", s.windowPage(snap))
}
