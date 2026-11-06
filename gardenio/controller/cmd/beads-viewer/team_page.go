package main

// GET  /teams/{team}          — the team page (ADR docs/adr/taints.md §9.1
//                                "Team page", task gd-NeEllAWKFw).
// POST /teams/{team}/members  — membership edit (add/remove a members/owners
//                                entry). In-handler gated to the team's own
//                                owners + platform admins, matching the
//                                daemon's authorizeTeamMembershipBypass gate
//                                (kbeads' team_membership.go) so a UI lever
//                                is never shown that the route would reject.
//
// The page renders the six sections the acceptance criteria name: members,
// owners, the IdP groups the team follows, taints it owns, grants it holds,
// and its open approvals. Owned taints come from ListTaints filtered
// client-side by OwnerTeam == the resolved team bead ID (the same registry
// /taints reads); held grants + open approvals come from the daemon's
// GET /v1/teams/{id}/taint-summary (kbeads' http_teams.go), computed fresh
// under the caller's own read scope on every call.

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// teamMemberEntryView is one rendered row of a members/owners list. Key is
// the exact string a membership-edit POST must echo back to identify this
// entry for removal (rawEntryKey's output).
type teamMemberEntryView struct {
	Key     string
	Display string
}

// teamOwnedTaintView is one row of the "taints it owns" section, reusing the
// registry's own bead-count/last-scrub display conventions (taints_registry.go).
type teamOwnedTaintView struct {
	Name              string
	Description       string
	BeadCountDisplay  string
	OpenScrubRequests int
	Retired           bool
}

// teamPageView is the data passed to templates/team.html.
type teamPageView struct {
	Team    beadsapi.TeamInfo
	Members []teamMemberEntryView
	Owners  []teamMemberEntryView

	OwnedTaints   []teamOwnedTaintView
	HeldGrants    []beadsapi.TeamTaintGrant
	OpenApprovals []beadsapi.TeamOpenApproval

	CanEditMembership bool
	Error             string
	MembershipError   string
}

// rawEntryKey renders a members/owners JSON entry (a resolved operator id
// number, or an unresolved "kind:subject" / bare-email alias string —
// kbeads' teamMatchesMember accepts either shape) into the stable string a
// membership-edit POST identifies it by, and that displayForEntry renders
// from.
func rawEntryKey(raw json.RawMessage) string {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var asNumber json.Number
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &asNumber); err == nil {
		return asNumber.String()
	}
	return strings.Trim(string(raw), `"`)
}

// displayForEntry renders a members/owners entry for the page: an alias
// string has its "kind:" scheme prefix stripped (bare email/subject reads
// better than "oidc:alice@example.com"); a bare resolved operator id number
// renders as "operator #<id>" since this client has no operator-id → name
// lookup wired in here.
func displayForEntry(raw json.RawMessage) string {
	key := rawEntryKey(raw)
	if _, err := strconv.ParseInt(key, 10, 64); err == nil {
		return "operator #" + key
	}
	if idx := strings.Index(key, ":"); idx > 0 {
		return key[idx+1:]
	}
	return key
}

// teamMemberEntryViews converts a raw members/owners field into sorted,
// display-ready rows.
func teamMemberEntryViews(raw []json.RawMessage) []teamMemberEntryView {
	out := make([]teamMemberEntryView, 0, len(raw))
	for _, r := range raw {
		out = append(out, teamMemberEntryView{Key: rawEntryKey(r), Display: displayForEntry(r)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Display < out[j].Display })
	return out
}

// viewerOwnsTeam reports whether the request's viewer email matches an entry
// in the team's own `owners` field, mirroring kbeads' callerIsTeamOwner
// closely enough for a UI-level affordance gate — the daemon is the
// authoritative check on the actual write.
func viewerOwnsTeam(r *http.Request, owners []json.RawMessage) bool {
	email := strings.ToLower(strings.TrimSpace(oidcauth.EmailFromContext(r.Context())))
	if email == "" {
		return false
	}
	for _, raw := range owners {
		key := strings.ToLower(rawEntryKey(raw))
		if key == email {
			return true
		}
		if idx := strings.Index(key, ":"); idx > 0 && key[idx+1:] == email {
			return true
		}
	}
	return false
}

// handleTeamPage renders GET /teams/{team}.
func (s *Server) handleTeamPage(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("team"))
	if slug == "" {
		http.Error(w, "Missing team", http.StatusBadRequest)
		return
	}

	team, err := s.daemon.FindTeamBySlug(r.Context(), slug)
	if err != nil {
		s.logger.Error("resolving team", "team", slug, "error", err)
		http.NotFound(w, r)
		return
	}

	view := teamPageView{
		Team:              team,
		Members:           teamMemberEntryViews(team.Members),
		Owners:            teamMemberEntryViews(team.Owners),
		CanEditMembership: viewerCanAdmin(r) || viewerOwnsTeam(r, team.Owners),
	}

	taints, err := s.daemon.ListTaints(r.Context())
	if err != nil {
		s.logger.Error("listing taints for team page", "team", slug, "error", err)
		view.Error = "Failed to load this team's owned taints."
	} else {
		for _, t := range taints {
			if t.OwnerTeam != team.BeadID {
				continue
			}
			view.OwnedTaints = append(view.OwnedTaints, teamOwnedTaintView{
				Name:              t.Name,
				Description:       t.Description,
				BeadCountDisplay:  beadCountDisplay(t.BeadCount),
				OpenScrubRequests: t.OpenScrubRequests,
				Retired:           t.RetiredAt != nil,
			})
		}
		sort.Slice(view.OwnedTaints, func(i, j int) bool { return view.OwnedTaints[i].Name < view.OwnedTaints[j].Name })
	}

	summary, err := s.daemon.GetTeamTaintSummary(r.Context(), team.BeadID)
	if err != nil {
		s.logger.Error("loading team taint summary", "team", slug, "error", err)
		if view.Error == "" {
			view.Error = "Failed to load this team's held grants and open approvals."
		}
	} else {
		view.HeldGrants = summary.HeldGrants
		view.OpenApprovals = summary.OpenApprovals
	}

	s.render(w, r, "team.html", map[string]any{"View": view})
}

// teamMembershipEditableFields is the whitelist of type:team Fields keys a
// membership edit may touch, mirroring kbeads' teamMembershipFieldKeys
// exactly (team_membership.go) — the daemon rejects anything else anyway,
// but the UI never even offers the lever.
var teamMembershipEditableFields = map[string]bool{"members": true, "owners": true}

// handleTeamMembershipEdit handles POST /teams/{team}/members — add or
// remove one members/owners entry. Gated in-handler to the team's own owners
// + admins (route-registered viewerOnly, same as every other authenticated
// write). Under the chart's default auth settings (requireUserBearerForWrites
// = false, edgeAuthSigning = false) this handler writes to the daemon with
// the unscoped static token plus X-Operated-By, and authorizeWrite passes an
// unscoped static token through unconditionally — so the daemon's
// authorizeTeamMembershipBypass gate never actually runs for this write path.
// viewerOwnsTeam (below) is therefore the only real gate today; keep it
// correct rather than relying on defense-in-depth from the daemon side until
// user-bearer writes are turned on.
func (s *Server) handleTeamMembershipEdit(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("team"))
	if slug == "" {
		http.Error(w, "Missing team", http.StatusBadRequest)
		return
	}
	team, err := s.daemon.FindTeamBySlug(r.Context(), slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !(viewerCanAdmin(r) || viewerOwnsTeam(r, team.Owners)) {
		http.Error(w, "Only this team's own owners or a platform admin may edit membership", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}

	field := strings.TrimSpace(r.PostForm.Get("field"))
	action := strings.TrimSpace(r.PostForm.Get("action"))
	value := strings.TrimSpace(r.PostForm.Get("value"))
	if !teamMembershipEditableFields[field] {
		http.Error(w, "Unknown field", http.StatusBadRequest)
		return
	}
	if value == "" {
		http.Error(w, "value is required", http.StatusBadRequest)
		return
	}

	current := team.Members
	if field == "owners" {
		current = team.Owners
	}

	var next []any
	switch action {
	case "add":
		next = rawEntriesToAny(current)
		for _, raw := range current {
			if strings.EqualFold(rawEntryKey(raw), value) {
				// Already present — no-op, not an error.
				s.redirectToTeamPage(w, r, slug)
				return
			}
		}
		next = append(next, value)
	case "remove":
		for _, raw := range current {
			if strings.EqualFold(rawEntryKey(raw), value) {
				continue
			}
			var v any
			if err := json.Unmarshal(raw, &v); err == nil {
				next = append(next, v)
			}
		}
	default:
		http.Error(w, "action must be add or remove", http.StatusBadRequest)
		return
	}
	if next == nil {
		next = []any{}
	}

	if err := s.daemon.UpdateBeadFieldsTyped(r.Context(), team.BeadID, map[string]any{field: next}); err != nil {
		s.logger.Error("editing team membership", "team", slug, "field", field, "action", action, "error", err)
		msg := "Failed to update membership."
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden {
			msg = "You are no longer authorized to edit this team's membership."
		}
		http.Error(w, msg, http.StatusBadGateway)
		return
	}

	s.redirectToTeamPage(w, r, slug)
}

// rawEntriesToAny converts a members/owners raw-JSON slice into []any for
// UpdateBeadFieldsTyped, preserving each entry's original JSON type (number
// vs string).
func rawEntriesToAny(raw []json.RawMessage) []any {
	out := make([]any, 0, len(raw))
	for _, r := range raw {
		var v any
		if err := json.Unmarshal(r, &v); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// redirectToTeamPage sends the browser back to the team page after a
// successful membership edit, using HX-Redirect for an htmx form post and a
// plain redirect otherwise.
func (s *Server) redirectToTeamPage(w http.ResponseWriter, r *http.Request, slug string) {
	dest := s.basePath + "/teams/" + slug
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}
