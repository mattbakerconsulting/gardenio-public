package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// taintBadgeView is the bead-show template's view model for one taint badge
// (ADR docs/adr/taints.md §9.1: "A taint badge per taint in the header,
// coloured by the taint's owner-team accent. Hover or tap shows the chain").
//
// RequestScrubHref is the bead-page "Request scrub" action (task
// gd-nOnc1Q8Zcv acceptance criteria: "pre-fills a detaint request for that
// bead") — POSTing here creates a kind=detaint scrub-request scoped to just
// this bead+taint. Populated whenever the viewer is authenticated
// (CanWrite); the daemon's ordinary createBead choke point is the real
// gate, same as every other bead-page write action.
type taintBadgeView struct {
	Name             string
	OwnerTeam        string
	AccentColor      string
	SourceLabel      string
	SourceBeadID     string
	SourceHref       string
	RequestScrubHref string
}

// taintApplyOption is one entry in the bead-page "Apply taint" dropdown
// (task gd-nOnc1Q8Zcv acceptance criteria: "'Apply taint' … visible only to
// grant holders"). Built from the taint registry's CanApply flag, which the
// daemon computes from callerHoldsTaintGrant — the same check
// POST /v1/beads/{id}/taints re-enforces, so this can only ever show a
// taint the caller can actually apply.
type taintApplyOption struct {
	Name      string
	OwnerTeam string
}

// taintActionsView is the bead-show template's full taint panel: badges plus
// the "Request scrub" / "Apply taint" actions gd-nOnc1Q8Zcv adds. CanWrite
// mirrors bead_actions' own OperatorEmail-presence gate — anonymous viewers
// see the badges (if any) but no action controls.
type taintActionsView struct {
	BeadID       string
	Badges       []taintBadgeView
	ApplyOptions []taintApplyOption
	CanWrite     bool
	Error        string
}

// taintSourceLabels maps a BeadTaint.Source value (docs/adr/taints.md §4.2's
// propagation rows) to the human-readable text the hover/tap detail shows.
// An unrecognized source (future propagation kind, or a daemon ahead of this
// viewer build) falls back to the raw value rather than hiding the badge.
var taintSourceLabels = map[string]string{
	"explicit":       "Applied explicitly",
	"inherit:create": "Inherited on create",
	"inherit:assign": "Inherited on assign",
	"inherit:read":   "Inherited on read",
	"inherit:write":  "Inherited on write",
	"inherit:parent": "Inherited from parent",
}

func taintSourceLabel(source string) string {
	if label, ok := taintSourceLabels[source]; ok {
		return label
	}
	return source
}

// buildTaintActions assembles the full bead-page taint panel: badges plus
// the "Request scrub" / "Apply taint" actions (task gd-nOnc1Q8Zcv). email is
// the authenticated viewer's address ("" for anonymous, which suppresses
// every action control — same convention as buildBeadActions).
//
// Both the badges and the "Apply taint" dropdown share one ListTaints call
// (registry owner_team for accent color; can_apply for the dropdown), so a
// registry-fetch failure degrades both to their empty/neutral state rather
// than failing the page.
func (s *Server) buildTaintActions(ctx context.Context, bead *beadsapi.BeadDetail, email string) taintActionsView {
	if bead == nil {
		return taintActionsView{}
	}
	view := taintActionsView{BeadID: bead.ID, CanWrite: email != ""}

	beadTaints, err := s.daemon.GetBeadTaints(ctx, bead.ID)
	if err != nil {
		s.logger.Debug("getting bead taints", "id", bead.ID, "error", err)
		beadTaints = nil
	}
	applied := make(map[string]struct{}, len(beadTaints))
	for _, bt := range beadTaints {
		applied[bt.TaintName] = struct{}{}
	}

	registry, err := s.daemon.ListTaints(ctx)
	if err != nil {
		s.logger.Debug("listing taint registry", "id", bead.ID, "error", err)
		registry = nil
	}
	ownerByName := make(map[string]string, len(registry))
	for _, t := range registry {
		ownerByName[t.Name] = t.OwnerTeam
	}

	badges := make([]taintBadgeView, 0, len(beadTaints))
	for _, bt := range beadTaints {
		owner := ownerByName[bt.TaintName]
		b := taintBadgeView{
			Name:        bt.TaintName,
			OwnerTeam:   owner,
			AccentColor: ownerInitialsHue(owner),
			SourceLabel: taintSourceLabel(bt.Source),
		}
		if bt.SourceBeadID != "" {
			b.SourceBeadID = bt.SourceBeadID
			b.SourceHref = s.basePath + "/" + bt.SourceBeadID
		}
		if view.CanWrite {
			b.RequestScrubHref = s.basePath + "/b/" + url.PathEscape(bead.ID) + "/taints/" + url.PathEscape(bt.TaintName) + "/request-scrub"
		}
		badges = append(badges, b)
	}
	sort.Slice(badges, func(i, j int) bool { return badges[i].Name < badges[j].Name })
	view.Badges = badges

	if view.CanWrite {
		for _, t := range registry {
			if !t.CanApply || t.RetiredAt != nil {
				continue
			}
			if _, ok := applied[t.Name]; ok {
				continue
			}
			view.ApplyOptions = append(view.ApplyOptions, taintApplyOption{Name: t.Name, OwnerTeam: t.OwnerTeam})
		}
		sort.Slice(view.ApplyOptions, func(i, j int) bool { return view.ApplyOptions[i].Name < view.ApplyOptions[j].Name })
	}

	return view
}

// handleRequestScrubForBead handles POST /b/{id}/taints/{taint}/request-scrub
// — the bead-page "Request scrub" action (task gd-nOnc1Q8Zcv acceptance
// criteria: "pre-fills a p2-scrub-request-api call scoped to the single
// bead"). kind is always "detaint": this button removes a specific taint
// from a specific bead, as opposed to a broader purge sweep. Standard relay
// authentication (s.daemon.CreateScrubRequest) — creating a scrub-request is
// an ordinary bead-creation write, unlike the OIDC-forced approve/reject.
func (s *Server) handleRequestScrubForBead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	taint := r.PathValue("taint")
	if id == "" || taint == "" {
		http.Error(w, "Missing bead id or taint", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}

	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for request-scrub", "id", id, "error", err)
		http.Error(w, "Failed to load bead", http.StatusInternalServerError)
		return
	}

	created, err := s.daemon.CreateScrubRequest(r.Context(), beadsapi.CreateScrubRequestInput{
		Taint:   taint,
		Kind:    "detaint",
		BeadIDs: []string{id},
	})
	if err != nil {
		s.logger.Error("creating scrub-request", "id", id, "taint", taint, "actor", email, "error", err)
		s.renderTaintActionsPartial(w, r, bead, email, "Failed to create scrub-request: "+err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("HX-Redirect", s.basePath+"/"+created.Bead.ID)
	s.renderTaintActionsPartial(w, r, bead, email, "", http.StatusOK)
}

// handleApplyTaintToBead handles POST /b/{id}/apply-taint — the bead-page
// "Apply taint" action (task gd-nOnc1Q8Zcv acceptance criteria: "calls
// p1-taint-grants's apply endpoint and is visible only to grant holders").
// The dropdown itself is already filtered to CanApply taints
// (buildTaintActions), but the daemon's callerHoldsTaintGrant check on
// POST /v1/beads/{id}/taints is what actually enforces this — a stale or
// tampered form value fails there, not here.
func (s *Server) handleApplyTaintToBead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	taint := r.PostForm.Get("taint")

	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for apply-taint", "id", id, "error", err)
		http.Error(w, "Failed to load bead", http.StatusInternalServerError)
		return
	}
	if taint == "" {
		s.renderTaintActionsPartial(w, r, bead, email, "Select a taint to apply.", http.StatusBadRequest)
		return
	}

	if _, err := s.daemon.ApplyTaint(r.Context(), id, taint); err != nil {
		s.logger.Error("applying taint", "id", id, "taint", taint, "actor", email, "error", err)
		s.renderTaintActionsPartial(w, r, bead, email, "Failed to apply taint: "+err.Error(), http.StatusBadGateway)
		return
	}

	s.renderTaintActionsPartial(w, r, bead, email, "", http.StatusOK)
}

// renderTaintActionsPartial re-renders the #taint-actions-panel fragment
// (the htmx swap target for both actions above).
func (s *Server) renderTaintActionsPartial(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, email, errMsg string, status int) {
	if respondAction(w, r, bead.ID, actionFailure(errMsg, status)) {
		return
	}
	view := s.buildTaintActions(r.Context(), bead, email)
	view.Error = errMsg
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/taint_badges.html", "taint_badges", map[string]any{
		"Actions":  view,
		"BasePath": s.basePath,
	})
}
