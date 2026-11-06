package main

import (
	"net/http"
	"net/url"
	"strings"
)

// The tenant lands on their plot (Lands Track B step 8.8, gd-vJK94LpTOm, spec
// §3.6). "/" sends a scoped principal with one land to /garden?land=<land>,
// where the camera opens on that land's region and the Commons pane on its
// #general (gardenhub web/garden.js and web/commons.js). With several lands it
// sends them to /lands/mine, which lists them.
//
// The Lands home (/lands/) is public and served without a session, so the
// server cannot tell who is reading it. Its page asks /lands/mine for the
// caller's lands instead (Accept: application/json, on the caller's own
// session; signed out, the edge refuses it and the page stays as it is): one
// land and the reader came from elsewhere, it goes on to /garden?land=; several,
// it lists them.
//
// The gatehouse (lands_gatehouse.go) is not a land of one's own: a person
// whose only land it is keeps the gatehouse's #general, and someone with a
// land too is sent to that land.

// landHref is the /garden link that opens on land: the camera on its region
// and the Commons on its #general. The mailer's "your land is ready" link is
// the same (internal/mailer, landLink).
func (s *Server) landHref(land string) string {
	return s.basePath + "/garden?land=" + url.QueryEscape(land)
}

// ownLands is the caller's lands, the gatehouse left out: nil for an
// unrestricted caller, an applicant, and a gatehouse guest with no other land.
func (s *Server) ownLands(r *http.Request) []string {
	sc := viewerScopeFromContext(r.Context())
	if !sc.Scoped() {
		return nil
	}
	var out []string
	for _, p := range sc.Projects {
		if p != "" && p != s.landsGatehouse {
			out = append(out, p)
		}
	}
	return out
}

// landHome is where "/" sends r's caller when it holds a land: the land's
// /garden view with one, /lands/mine with several, else "". It needs the
// /garden page wired.
func (s *Server) landHome(r *http.Request) string {
	if s.garden == nil {
		return ""
	}
	switch lands := s.ownLands(r); len(lands) {
	case 0:
		return ""
	case 1:
		return s.landHref(lands[0])
	default:
		return s.basePath + "/lands/mine"
	}
}

// landsMineLand is one row of /lands/mine.
type landsMineLand struct {
	Name string `json:"name"`
	Href string `json:"href"`
}

// handleLandsMine serves GET /lands/mine (routeApplicant): the caller's own
// lands, read from the caller's own token (its project groups), so it shows
// nothing of anyone else. With Accept: application/json it answers
// {"lands": [{name, href}]}, which the Lands home's script reads. An
// unrestricted caller has every land on the farm and is sent to /garden.
func (s *Server) handleLandsMine(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Vary", "Accept")
	sc := viewerScopeFromContext(r.Context())
	lands := []landsMineLand{}
	for _, l := range s.ownLands(r) {
		lands = append(lands, landsMineLand{Name: l, Href: s.landHref(l)})
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSONStatus(w, http.StatusOK, map[string]any{"lands": lands})
		return
	}
	if !sc.Scoped() && s.garden != nil {
		http.Redirect(w, r, s.basePath+"/garden", http.StatusFound)
		return
	}
	v, ok := s.landsBase(r, "Your lands")
	if !ok {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	v.Lands = lands
	s.renderLands(w, http.StatusOK, "lands_mine", v)
}
