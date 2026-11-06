package main

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planwindow"
)

// The land's Share tab (Lands: the land window, plan gd-4iVYF9tGsn, step LW.5
// gd-aUQRspcbeS): the plan Share panel's land arm (plan_share.go, PW.4). A
// land's owner or an operator mints a read-only public window on the whole
// land, sees the exact page a visitor will get before minting, is shown the
// URL once, and lists and revokes the land's windows with their view counts.
// The tab lives on the land's own surface in /garden, the project panel
// (gardenhub web/project.js), which asks these routes for JSON:
//
//	GET  /p/{project}/share                    the panel: withheld fields, preview link, the land's windows
//	GET  /p/{project}/share/preview            the land window page a visitor would see (HTML)
//	POST /p/{project}/share                    mint; the panel again with the URL, once
//	POST /p/{project}/share/{window}/revoke    revoke; the panel again
//
// The gate is kbeads', as for a plan: every daemon call goes out on the
// signed-in person's own bearer alone (planShareCtx), the routes are
// routeScoped, and the project panel offers the tab only to a caller
// windowShareOffered admits (ProjectDoc.CanShare).
//
// The preview is window.go's own render of the land page (window_land, with
// /w/static/land.js), built from the daemon's preview payload exactly as
// /w/{token} builds a minted window's. Its one difference is where the scene
// reads the land from: land.js asks land.json beside the page with no
// credentials, which an owner's signed-in preview cannot answer, so the page
// carries the same document inline (windowPageView.Seed) and land.js draws
// that instead and does not poll.

// landShareDenied is the words for a refusal on a land.
const landShareDenied = "Only the owner of this land or an operator can share it."

// landShareAPI is the subset of beadsapi.Client the land's Share tab uses,
// kept apart from planShareAPI so the plan panel's fakes need not grow it.
type landShareAPI interface {
	CreateLandWindow(ctx context.Context, land string, expiresInDays int) (*beadsapi.MintedWindow, error)
	ListWindows(ctx context.Context, land string) ([]beadsapi.Window, error)
	RevokeWindow(ctx context.Context, id string) (*beadsapi.Window, error)
	PreviewLandWindow(ctx context.Context, land string, expiresInDays int) (json.RawMessage, error)
}

// landShareAPI returns the daemon client as a landShareAPI, nil when it is
// not one.
func (s *Server) landShareAPI() landShareAPI {
	if s == nil || s.daemon == nil {
		return nil
	}
	c, _ := s.daemon.(landShareAPI)
	return c
}

// landShareWithheld is one field the window would withhold.
type landShareWithheld struct {
	Path string `json:"path"`
	Rule string `json:"rule"`
}

// landShareMinted is a new window's link, shown this once.
type landShareMinted struct {
	URL       string    `json:"url"`
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// landSharePanel is the Share tab's document. A refusal or failure to read
// the panel fills Error and leaves the rest empty; a refused mint or revoke
// is ActionError, with OK false, over a panel read again.
type landSharePanel struct {
	OK       bool   `json:"ok"`
	Land     string `json:"land"`
	Grants   string `json:"grants"`
	Expiries []int  `json:"expiries"`
	Days     int    `json:"days"`
	// Preview is the preview page's path (with the viewer's base path);
	// Plans counts the plans its list shows.
	Preview      string              `json:"preview,omitempty"`
	Plans        int                 `json:"plans"`
	Withheld     []landShareWithheld `json:"withheld"`
	WithheldText string              `json:"withheld_text"`
	// Windows are the land's windows on the whole land, newest first.
	Windows     []beadsapi.Window `json:"windows"`
	Minted      *landShareMinted  `json:"minted,omitempty"`
	Revoked     string            `json:"revoked,omitempty"`
	Error       string            `json:"error,omitempty"`
	ActionError string            `json:"action_error,omitempty"`
}

// landShareSnapshot reads the land preview payload on the person's bearer and
// builds it as the window page does (buildWindowSnapshot, window.go).
func (s *Server) landShareSnapshot(ctx context.Context, api landShareAPI, land string, days int) (*windowSnapshot, error) {
	raw, err := api.PreviewLandWindow(ctx, land, days)
	if err != nil {
		return nil, err
	}
	snap, err := buildWindowSnapshot(raw, time.Now())
	if err == nil && snap.land == nil {
		err = errors.New("the preview is not a land view")
	}
	if err != nil {
		s.logger.Warn("land share: preview payload did not decode", "land", land, "error", err)
		return nil, err
	}
	return snap, nil
}

// landSharePreviewPath is the preview page's path for land at days.
func (s *Server) landSharePreviewPath(land string, days int) string {
	return s.basePath + "/p/" + url.PathEscape(land) + "/share/preview?days=" + strconv.Itoa(days)
}

// buildLandSharePanel reads the tab: what the window would withhold, the
// preview link and the land's windows.
func (s *Server) buildLandSharePanel(r *http.Request, land string, days int) landSharePanel {
	p := landSharePanel{
		OK: true, Land: land, Grants: planwindow.LandGrants, Expiries: planShareExpiries, Days: days,
		Withheld: []landShareWithheld{}, WithheldText: planwindow.WithheldText, Windows: []beadsapi.Window{},
	}
	api := s.landShareAPI()
	if api == nil {
		p.Error = windowShareRefusal(errNoPlanShareAPI, landShareDenied)
		return p
	}
	ctx := planShareCtx(r.Context())
	snap, err := s.landShareSnapshot(ctx, api, land, days)
	if err != nil {
		p.Error = windowShareRefusal(err, landShareDenied)
		return p
	}
	p.Preview, p.Plans = s.landSharePreviewPath(land, days), len(snap.land.Plans)
	for _, w := range snap.land.Withheld {
		p.Withheld = append(p.Withheld, landShareWithheld{Path: w.Path, Rule: w.Rule})
	}
	windows, err := api.ListWindows(ctx, land)
	if err != nil {
		p.Error = windowShareRefusal(err, landShareDenied)
		return p
	}
	for _, w := range windows {
		if w.TargetKind == "land" {
			p.Windows = append(p.Windows, w)
		}
	}
	return p
}

// landShareStatus is the HTTP status a refused mint or revoke answers.
func landShareStatus(err error) int {
	var apiErr *beadsapi.APIError
	switch {
	case errors.Is(err, beadsapi.ErrUserBearerRequired):
		return http.StatusUnauthorized
	case errors.As(err, &apiErr) && apiErr.StatusCode < 500:
		return apiErr.StatusCode
	}
	return http.StatusBadGateway
}

// handleLandShare handles GET /p/{project}/share.
func (s *Server) handleLandShare(w http.ResponseWriter, r *http.Request) {
	land := strings.TrimSpace(r.PathValue("project"))
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSONStatus(w, http.StatusOK, s.buildLandSharePanel(r, land, planShareDays(r.URL.Query().Get("days"))))
}

// handleLandShareMint handles POST /p/{project}/share: mint a window on the
// land and answer the panel with its URL, shown this once.
func (s *Server) handleLandShareMint(w http.ResponseWriter, r *http.Request) {
	land := strings.TrimSpace(r.PathValue("project"))
	days := planShareDays(r.FormValue("days"))
	w.Header().Set("Cache-Control", "private, no-store")
	var minted *beadsapi.MintedWindow
	err := errNoPlanShareAPI
	if api := s.landShareAPI(); api != nil {
		minted, err = api.CreateLandWindow(planShareCtx(r.Context()), land, days)
	}
	p := s.buildLandSharePanel(r, land, days)
	if err != nil {
		p.OK, p.ActionError = false, windowShareRefusal(err, landShareDenied)
		writeJSONStatus(w, landShareStatus(err), p)
		return
	}
	p.Minted = &landShareMinted{URL: planShareWindowURL(r, minted.Path), ID: minted.Window.ID, ExpiresAt: minted.Window.ExpiresAt}
	writeJSONStatus(w, http.StatusOK, p)
}

// handleLandShareRevoke handles POST /p/{project}/share/{window}/revoke.
func (s *Server) handleLandShareRevoke(w http.ResponseWriter, r *http.Request) {
	land := strings.TrimSpace(r.PathValue("project"))
	id := strings.TrimSpace(r.PathValue("window"))
	days := planShareDays(r.FormValue("days"))
	w.Header().Set("Cache-Control", "private, no-store")
	err := errNoPlanShareAPI
	if api := s.landShareAPI(); api != nil {
		_, err = api.RevokeWindow(planShareCtx(r.Context()), id)
	}
	p := s.buildLandSharePanel(r, land, days)
	if err != nil {
		p.OK, p.ActionError = false, windowShareRefusal(err, landShareDenied)
		writeJSONStatus(w, landShareStatus(err), p)
		return
	}
	p.Revoked = id
	writeJSONStatus(w, http.StatusOK, p)
}

// handleLandSharePreview handles GET /p/{project}/share/preview: the exact
// page a window minted now on the land would show a visitor (window.html's
// window_land, built as /w/{token} builds it, with its script), on the
// person's own bearer, every withheld field reading planwindow.WithheldText
// where it stands. It carries the window page's headers.
func (s *Server) handleLandSharePreview(w http.ResponseWriter, r *http.Request) {
	land := strings.TrimSpace(r.PathValue("project"))
	days := planShareDays(r.URL.Query().Get("days"))
	setWindowHeaders(w)
	err := errNoPlanShareAPI
	var snap *windowSnapshot
	if api := s.landShareAPI(); api != nil {
		snap, err = s.landShareSnapshot(planShareCtx(r.Context()), api, land, days)
	}
	if err != nil {
		http.Error(w, windowShareRefusal(err, landShareDenied), landShareStatus(err))
		return
	}
	page := s.windowLandPage(snap)
	page.Seed = template.JS(snap.doc) //nolint:gosec // json.Marshal output, which escapes <, > and &
	s.renderWindow(w, http.StatusOK, "window_land", page)
}
