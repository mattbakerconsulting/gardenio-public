package beadsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// This file backs the viewer's /teams/{team} page (docs/adr/taints.md §9.1
// "Team page"): resolving a type:team config bead by its `team` slug field,
// and fetching the taint-grant / scrub-approval summary the daemon computes
// fresh on every call (GET /v1/teams/{id}/taint-summary, kbeads'
// internal/server/http_teams.go).

// TeamInfo is the type:team config bead's fields the viewer's team page
// needs, parsed out of BeadDetail.Fields the same way ProjectInfo is
// (projects.go) — members/owners/oidc_groups are JSON arrays re-marshaled to
// strings by fieldsMap(), so a second json.Unmarshal recovers the typed
// slice. members/owners entries are left as raw JSON (a caller-resolved
// operator id is a JSON number; an unresolved alias is a JSON string —
// kbeads' teamMatchesMember accepts either) so the viewer can render either
// shape without losing information.
type TeamInfo struct {
	BeadID     string
	Slug       string // the `team` field — the URL-addressable slug
	Display    string
	Status     string
	Members    []json.RawMessage
	Owners     []json.RawMessage
	OIDCGroups []string
	Projects   []string
}

// FindTeamBySlug looks up the type:team bead whose `team` field equals slug.
// Modeled directly on GetProjectBead/ListProjectBeads: a field-filtered list
// call (so the daemon does the matching, not a full-registry client-side
// scan) followed by a not-found error on a miss. Statuses is left
// unrestricted (nil) — a deferred or blocked team bead should still resolve
// on its own page, unlike the "active projects" listing.
func (c *Client) FindTeamBySlug(ctx context.Context, slug string) (TeamInfo, error) {
	resp, err := c.listBeadsWithFields(ctx, []string{"team"}, nil, map[string]string{"team": slug})
	if err != nil {
		return TeamInfo{}, fmt.Errorf("looking up team %q: %w", slug, err)
	}
	for _, b := range resp.Beads {
		fields := b.fieldsMap()
		if fields["team"] != slug {
			continue
		}
		return teamInfoFromFields(b.ID, b.Status, fields), nil
	}
	return TeamInfo{}, fmt.Errorf("team %q not found", slug)
}

// teamInfoFromFields builds a TeamInfo from a bead's re-marshaled Fields map.
func teamInfoFromFields(beadID, status string, fields map[string]string) TeamInfo {
	info := TeamInfo{
		BeadID:  beadID,
		Slug:    fields["team"],
		Display: fields["display"],
		Status:  status,
	}
	unmarshalProjectField(fields["team"], "members", fields["members"], &info.Members)
	unmarshalProjectField(fields["team"], "owners", fields["owners"], &info.Owners)
	unmarshalProjectField(fields["team"], "oidc_groups", fields["oidc_groups"], &info.OIDCGroups)
	unmarshalProjectField(fields["team"], "projects", fields["projects"], &info.Projects)
	return info
}

// TeamTaintGrant is one row of a team's explicitly held taint grants —
// mirrors kbeads' teamTaintGrant (internal/server/http_teams.go) exactly.
type TeamTaintGrant struct {
	TaintName   string     `json:"taint_name"`
	OwnerTeam   string     `json:"owner_team"`
	Description string     `json:"description"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

// TeamOpenApproval is one open scrub-request bead whose resolved approvers
// name this team — mirrors kbeads' teamOpenApproval exactly.
type TeamOpenApproval struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	TaintName   string `json:"taint_name"`
	Kind        string `json:"kind"`
	RequestedBy string `json:"requested_by"`
}

// TeamTaintSummary is the response body for GET /v1/teams/{id}/taint-summary.
type TeamTaintSummary struct {
	HeldGrants    []TeamTaintGrant   `json:"held_grants"`
	OpenApprovals []TeamOpenApproval `json:"open_approvals"`
}

// GetTeamTaintSummary calls GET /v1/teams/{id}/taint-summary, returning the
// taints explicitly granted to teamID and the open scrub-requests whose
// resolved approvers name it (distinct from taints the team OWNS, which come
// from ListTaints filtered by OwnerTeam).
func (c *Client) GetTeamTaintSummary(ctx context.Context, teamID string) (*TeamTaintSummary, error) {
	var result TeamTaintSummary
	if err := c.doJSON(ctx, http.MethodGet, "/v1/teams/"+url.PathEscape(teamID)+"/taint-summary", nil, &result); err != nil {
		return nil, fmt.Errorf("getting taint summary for team %s: %w", teamID, err)
	}
	return &result, nil
}
