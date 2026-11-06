package main

import (
	"context"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/oidcauth"
)

// viewerLaneRefs is the set of owner-audience lanes (D11) the signed-in viewer
// holds: operator for a gardenio-admin (or an identity from the trusted edge
// with no roles and no verified token, the dev bypass and AVA, which
// scopeFromRequest already treats as unrestricted), and land-owner:<land> for
// each land whose owners group the token names. It decides what is SHOWN as
// needing the viewer, never what they may do: the buttons ask the daemon.
// Nil for an anonymous request; surfaces then behave exactly as #756 left them.
func (s *Server) viewerLaneRefs(ctx context.Context, email string) []string {
	if canonicalOIDC(email) == "" {
		return nil
	}
	lanes := []string{}
	roles := oidcauth.RolesFromContext(ctx)
	if oidcauth.HasMinimumRole(roles, oidcauth.RoleAdmin) ||
		(len(roles) == 0 && !oidcauth.OIDCVerifiedFromContext(ctx)) {
		lanes = append(lanes, beadsapi.LaneOperator)
	}
	var owned []string
	for land, role := range conversation.LandRolesFromGroups(oidcauth.GroupsFromContext(ctx)) {
		if role == conversation.LandRoleOwner {
			owned = append(owned, "land-owner:"+land)
		}
	}
	slices.Sort(owned)
	return append(lanes, owned...)
}

// escalationOwnerRow is D9 on top of #756: whether an escalation is listed as
// the viewer's own (owner) or folds into In hand, and whether it folds as
// another land owner's (landFold). #756's EscalationForOwner decides the
// audience; the only refinement is that a land owner's escalation is that
// land owner's. Nil lanes (anonymous) is #756 unchanged.
func escalationOwnerRow(fields map[string]string, lanes []string) (owner, landFold bool) {
	if !beadsapi.EscalationForOwner(fields) {
		return false, false
	}
	held := beadsapi.HeldByFromFields(fields)
	if _, ok := beadsapi.LandOwnerLane(held); ok && lanes != nil && !slices.Contains(lanes, held) {
		return false, true
	}
	return true, false
}

// escalationLaneNote is the card's plain line on who an escalation waits on
// (gd-cuTEquiXqq). An open, unapproved item off the floor that a coordinator
// or steward lane holds is that lane's to approve, even when the viewer may
// approve it too. An approved one whose executor is such a lane, with nothing
// dispatched and no result, moves only when that lane does the work, so the
// approval alone runs nothing. "" in every other case.
func escalationLaneNote(v *escalationView, f map[string]string) string {
	if !v.Open || v.OperatorOnly || v.ClaimedBy != "" || beadsapi.EscalationAudienceForced(f) {
		return ""
	}
	if !v.Approved {
		if agentLane(v.HeldBy) {
			return v.HeldByLabel + " holds this and may approve it; it does not need the owner."
		}
		return ""
	}
	exec := strings.TrimSpace(f["execute_by"])
	if d := strings.TrimSpace(f["execution_dispatch"]); v.ResultStatus == "" && agentLane(exec) && (d == "" || d == "none") {
		return "Approved. It waits for " + beadsapi.LaneLabel(exec) + " to do the work; the approval runs nothing by itself."
	}
	return ""
}

// agentLane reports whether ref is a coordinator or steward lane: one an agent
// or machine staffs, not a person.
func agentLane(ref string) bool {
	kind, name, _ := strings.Cut(strings.TrimSpace(ref), ":")
	return (kind == "coordinator" || kind == "steward") && name != ""
}

// escalationPresenceAPI reads held_by_presence; the daemon client implements
// it, and a fake without it shows no offline line.
type escalationPresenceAPI interface {
	EscalationHolderPresence(ctx context.Context, escalationID string) (*beadsapi.LanePresence, error)
}

// fillLaneOffline sets ev.LaneOffline for an open item whose holder is a
// presence lane that is away (gd-t41kYFbLUt). A read error shows nothing.
func (s *Server) fillLaneOffline(ctx context.Context, ev *escalationView) {
	api, ok := s.daemon.(escalationPresenceAPI)
	if !ok || ev == nil || !ev.Open || !agentLane(ev.HeldBy) {
		return
	}
	lp, err := api.EscalationHolderPresence(ctx, ev.BeadID)
	if err != nil {
		s.logger.Debug("escalation holder presence unavailable", "id", ev.BeadID, "error", err)
		return
	}
	ev.LaneOffline = laneOfflineLine(lp, s.now())
}

// laneOfflineLine is "laptop offline since 22:06Z" (the day too when it is
// not today), "laptop offline, not seen yet" for a lane never seen, and ""
// for a lane that is online or not presence-tracked.
func laneOfflineLine(lp *beadsapi.LanePresence, now time.Time) string {
	if lp == nil || lp.Online {
		return ""
	}
	_, name, _ := strings.Cut(lp.Lane, ":")
	seen, err := time.Parse(time.RFC3339, lp.LastSeenAt)
	if err != nil {
		return name + " offline, not seen yet; it holds this until it is back."
	}
	seen = seen.UTC()
	at := seen.Format("15:04Z")
	if y, m, d := seen.Date(); y != now.UTC().Year() || m != now.UTC().Month() || d != now.UTC().Day() {
		at = seen.Format("Jan 2 15:04Z")
	}
	return name + " offline since " + at + "; it holds this until it is back."
}
