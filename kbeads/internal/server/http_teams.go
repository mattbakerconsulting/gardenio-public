package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"gardenio/kbeads/internal/model"
)

// This file implements the one read surface docs/adr/taints.md §9.1's Team
// page needs that no existing endpoint exposes: the taints explicitly
// GRANTED to a team (distinct from taints it OWNS, already visible via GET
// /v1/taints' owner_team field) and the open scrub-requests whose resolved
// approvers name it. Both are computed fresh on every call, mirroring
// handleListTaints' no-caching-beyond-the-request stance.
//
//   GET /v1/teams/{id}/taint-summary   handleTeamTaintSummary

// teamTaintGrant is one row of a team's explicitly held taint grants.
type teamTaintGrant struct {
	TaintName   string     `json:"taint_name"`
	OwnerTeam   string     `json:"owner_team"`
	Description string     `json:"description"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

// teamOpenApproval is one open/in_progress scrub-request bead whose
// resolved approvers (scrub_policy_eval.go, same evaluator
// handleApproveScrubRequest runs) name this team.
type teamOpenApproval struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	TaintName   string `json:"taint_name"`
	Kind        string `json:"kind"`
	RequestedBy string `json:"requested_by"`
}

// teamTaintSummary is the response body for GET /v1/teams/{id}/taint-summary.
type teamTaintSummary struct {
	HeldGrants    []teamTaintGrant   `json:"held_grants"`
	OpenApprovals []teamOpenApproval `json:"open_approvals"`
}

// handleTeamTaintSummary handles GET /v1/teams/{id}/taint-summary. Available
// to any authenticated caller (RoleViewer, per the route registration),
// matching type:team's own cross-fleet visibility (config.go) and GET
// /v1/taints' public listing — this exposes no more than "this team can act
// on taint X" and "this team is a named approver on request Y", never
// scrub_policy detail.
func (s *BeadsServer) handleTeamTaintSummary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	ctx := r.Context()

	team, err := s.store.GetBead(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || team == nil {
		writeError(w, http.StatusNotFound, "team not found")
		return
	}
	if err != nil {
		writeServerError(ctx, w, "handleTeamTaintSummary", err, "failed to look up team")
		return
	}
	if team.Type != model.TypeTeam {
		writeError(w, http.StatusNotFound, "team not found")
		return
	}

	grants, err := s.teamHeldGrants(ctx, id)
	if err != nil {
		writeServerError(ctx, w, "handleTeamTaintSummary", err, "failed to resolve held grants")
		return
	}
	approvals, err := s.teamOpenApprovals(ctx, id)
	if err != nil {
		writeServerError(ctx, w, "handleTeamTaintSummary", err, "failed to resolve open approvals")
		return
	}

	writeJSON(w, http.StatusOK, teamTaintSummary{HeldGrants: grants, OpenApprovals: approvals})
}

// teamHeldGrants resolves the taints explicitly granted to teamID
// (taint_grants rows, distinct from ownership) into their public listing
// shape, per ADR docs/adr/taints.md §5.1/§9.1. Per §9.1 ("policies and
// grants are visible only to the owner team and admins"), a grant is
// included only when the CALLER may see the granted taint's detail — a
// member of that taint's owner team, or an admin — the same visibility rule
// GET /v1/taints applies per-row (canSeeTaintDetail). Without this, any
// authenticated viewer could enumerate which taints an arbitrary team holds
// grants for, including taints owned by teams they have no relationship to.
func (s *BeadsServer) teamHeldGrants(ctx context.Context, teamID string) ([]teamTaintGrant, error) {
	taintIDs, err := s.store.GetGrantedTaintIDs(ctx, model.PrincipalTeam, []string{teamID})
	if err != nil {
		return nil, err
	}
	if len(taintIDs) == 0 {
		return []teamTaintGrant{}, nil
	}
	idSet := make(map[int64]bool, len(taintIDs))
	for _, tid := range taintIDs {
		idSet[tid] = true
	}

	taints, err := s.store.ListTaints(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]teamTaintGrant, 0, len(taintIDs))
	for _, t := range taints {
		if !idSet[t.ID] {
			continue
		}
		visible, err := s.canSeeTaintDetailCtx(ctx, t.OwnerTeam)
		if err != nil {
			return nil, err
		}
		if !visible {
			continue
		}
		g, err := s.store.GetTaintGrant(ctx, t.ID, model.PrincipalTeam, teamID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		var expires *time.Time
		if g != nil {
			expires = g.ExpiresAt
		}
		out = append(out, teamTaintGrant{
			TaintName:   t.Name,
			OwnerTeam:   t.OwnerTeam,
			Description: t.Description,
			ExpiresAt:   expires,
		})
	}
	return out, nil
}

// teamOpenApprovals scans open/in_progress scrub-request beads under the
// caller's own read scope and returns the ones whose resolved approvers
// (resolveScrubRuleForApproval, the same cap-aware evaluator
// handleApproveScrubRequest runs at approve time) name teamID —
// "team:<teamID>" per the pinned approvers-format convention
// (http_scrub_requests.go). An over-cap "auto" rule falls through to
// whichever rule/default actually names teamID, same as approve time —
// otherwise a request that would really land in team:A's queue (because its
// auto rule is over cap) never shows up here.
func (s *BeadsServer) teamOpenApprovals(ctx context.Context, teamID string) ([]teamOpenApproval, error) {
	filter := s.scopeListFilter(ctx, model.BeadFilter{
		Type:   []model.BeadType{model.TypeScrubRequest},
		Status: []model.Status{model.StatusOpen, model.StatusInProgress},
		Limit:  1000,
	})
	beads, _, err := s.store.ListBeads(ctx, filter)
	if err != nil {
		return nil, err
	}

	out := []teamOpenApproval{}
	taintCache := make(map[string]*model.Taint)
	// dailyCount caches each taint's today scrub-execute total, keyed by
	// taint ID, across every bead below — this can scan many requests
	// spanning few taints, so this collapses what would otherwise be one
	// ListTaintAudit query per request into one per distinct taint.
	dailyCount := make(map[int64]int)
	for _, b := range beads {
		fields, err := parseScrubRequestApprovalFields(b.Fields)
		if err != nil {
			continue
		}
		taint, cached := taintCache[fields.Taint]
		if !cached {
			t, err := s.store.GetTaint(ctx, fields.Taint)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					taintCache[fields.Taint] = nil
					continue
				}
				return nil, err
			}
			taint = t
			taintCache[fields.Taint] = t
		}
		if taint == nil {
			continue
		}
		var policy model.ScrubPolicy
		if len(taint.ScrubPolicy) > 0 {
			if err := json.Unmarshal(taint.ScrubPolicy, &policy); err != nil {
				continue
			}
		}
		_, approvers, err := s.resolveScrubRuleForApproval(ctx, taint.ID, &policy, scrubRequestPredicate(fields), len(fields.ResolvedBeadIDs), dailyCount)
		if err != nil {
			return nil, err
		}
		if !scrubApproverInApprovers([]string{teamID}, approvers) {
			continue
		}
		out = append(out, teamOpenApproval{
			ID:          b.ID,
			Title:       b.Title,
			TaintName:   fields.Taint,
			Kind:        fields.Kind,
			RequestedBy: fields.RequestedBy,
		})
	}
	return out, nil
}
