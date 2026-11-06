package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"gardenio/kbeads/internal/model"
)

// CreateTaintRequest is the JSON body for POST /v1/taints.
//
// Preset and ScrubPolicy are mutually exclusive: Preset resolves to the
// docs/adr/taints.md §10.2 policy for OwnerTeam server-side, so any caller
// (not just the CLI) can use a preset. A preset writes an ordinary
// scrub_policy the owner can edit afterward; nothing records that the taint
// came from a preset.
type CreateTaintRequest struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	OwnerTeam   string           `json:"owner_team"`
	Preset      model.PresetName `json:"preset,omitempty"`
	ScrubPolicy json.RawMessage  `json:"scrub_policy,omitempty"`
	// Effects is the taint's effect set (docs/adr/taints.md §15.2). Nil
	// defaults to {gate} — today's only behavior. An explicit empty slice is
	// the legal {} trace-only-dye case; unknown values are rejected by
	// SetTaint with a FieldError.
	Effects []string `json:"effects,omitempty"`
}

// taintListEntry is one row of the taints registry as docs/adr/taints.md
// §9.1 describes it: name, owner team, description and lifecycle
// timestamps, visible to every viewer. It never carries scrub_policy.
type taintListEntry struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	OwnerTeam   string     `json:"owner_team"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	RetiredAt   *time.Time `json:"retired_at,omitempty"`
}

// taintDetail is the full taint row, including scrub_policy and effects.
// Returned only to the owner team and admins (ADR §9.1, §5).
type taintDetail struct {
	taintListEntry
	ScrubPolicy json.RawMessage `json:"scrub_policy"`
	Effects     []string        `json:"effects"`
}

// restrictedTaintView is what a caller who is neither the owner team nor an
// admin sees from GET /v1/taints/{name}: enough to confirm the taint exists,
// never the policy or grant detail (task gd-sygX3b8TAg acceptance
// criteria).
type restrictedTaintView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func taintListEntryFromModel(t *model.Taint) taintListEntry {
	return taintListEntry{
		ID:          t.ID,
		Name:        t.Name,
		Description: t.Description,
		OwnerTeam:   t.OwnerTeam,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
		RetiredAt:   t.RetiredAt,
	}
}

func taintDetailFromModel(t *model.Taint) *taintDetail {
	return &taintDetail{
		taintListEntry: taintListEntryFromModel(t),
		ScrubPolicy:    t.ScrubPolicy,
		Effects:        t.Effects,
	}
}

// taintRegistryStats carries the per-taint aggregate figures the registry
// page needs (docs/adr/taints.md §9.1: "bead count, open scrub requests,
// last scrub"). Computed fresh on every GET /v1/taints call — no caching
// beyond the request, per task gd-QXSHroFXwB's acceptance criteria that
// these figures stay live rather than cached stale across a page load.
type taintRegistryStats struct {
	// BeadCount is the number of beads carrying this taint under the
	// caller's own read scope (project scope + taint gate). Nil — rendered
	// as a dash by the viewer — when the caller holds no grant for this
	// taint at all, distinguishing "denied" from a genuine zero (ADR §9.1
	// acceptance criteria: "a viewer without a grant... sees its row with a
	// dash bead count, not an error or an omitted row").
	BeadCount *int `json:"bead_count"`
	// OpenScrubRequests is the count of open/in_progress scrub-request
	// beads naming this taint, under the same read scope.
	OpenScrubRequests int `json:"open_scrub_requests"`
	// LastScrubAt is the most recent approved scrub-request's completion
	// time (executed_at when recorded, else approved_at) for this taint,
	// under the same read scope. Nil when no scrub has ever been approved.
	LastScrubAt *time.Time `json:"last_scrub_at,omitempty"`
}

// taintListEntryWithStats is one GET /v1/taints row for a caller who may
// not see scrub_policy (docs/adr/taints.md §9.1): the public list fields
// plus the live aggregate stats.
type taintListEntryWithStats struct {
	taintListEntry
	taintRegistryStats
	// CanApply reports whether the caller holds this taint's grant (owner
	// team, admin, or an explicit taint_grants row) and so should see the
	// bead-page "Apply taint" action for it (task gd-nOnc1Q8Zcv acceptance
	// criteria). Computed the same way handleApplyTaint authorizes the
	// apply itself (callerHoldsTaintGrant), so this can never show the
	// action for a taint the API would then refuse.
	CanApply bool `json:"can_apply"`
}

// taintDetailWithStats is one GET /v1/taints row for the owner team or an
// admin: the full detail (including scrub_policy) plus the live aggregate
// stats.
type taintDetailWithStats struct {
	taintDetail
	taintRegistryStats
	CanApply bool `json:"can_apply"`
}

// taintRegistryStatsFor computes the live registry-row stats for one taint
// under the caller's own read scope. A store error resolving the gate or
// running a count query is propagated so the caller returns a 5xx rather
// than silently rendering wrong figures.
func (s *BeadsServer) taintRegistryStatsFor(ctx context.Context, t *model.Taint) (taintRegistryStats, error) {
	count, err := s.taintBeadCount(ctx, t.ID)
	if err != nil {
		return taintRegistryStats{}, err
	}
	open, err := s.taintOpenScrubCount(ctx, t.Name)
	if err != nil {
		return taintRegistryStats{}, err
	}
	last, err := s.taintLastScrubAt(ctx, t.Name)
	if err != nil {
		return taintRegistryStats{}, err
	}
	return taintRegistryStats{BeadCount: count, OpenScrubRequests: open, LastScrubAt: last}, nil
}

// taintBeadCount returns the number of beads carrying taintID that are
// visible under the caller's own read scope, or nil when the caller holds
// no grant for this taint at all. Distinguishing "zero beads" from "no
// grant" requires checking grant membership directly before running the
// count: a scoped COUNT query alone can't tell them apart, since the taint
// gate filters a grant-less caller's view of this taint down to zero rows
// either way.
func (s *BeadsServer) taintBeadCount(ctx context.Context, taintID int64) (*int, error) {
	grants, exempt, err := s.taintGateGrants(ctx)
	if err != nil {
		return nil, err
	}
	if !exempt && !taintIDsSubsetOfGrants([]int64{taintID}, grants) {
		return nil, nil // dash: caller holds no grant for this taint
	}
	filter := s.scopeListFilter(ctx, model.BeadFilter{TaintID: &taintID, CountOnly: true})
	_, total, err := s.store.ListBeads(ctx, filter)
	if err != nil {
		return nil, err
	}
	return &total, nil
}

// taintOpenScrubCount counts open/in_progress scrub-request beads naming
// taintName, under the caller's own read scope. Reuses the generic Fields
// custom-field filter rather than any taint-specific store method.
func (s *BeadsServer) taintOpenScrubCount(ctx context.Context, taintName string) (int, error) {
	filter := s.scopeListFilter(ctx, model.BeadFilter{
		Type:      []model.BeadType{model.TypeScrubRequest},
		Status:    []model.Status{model.StatusOpen, model.StatusInProgress},
		Fields:    map[string]string{"taint": taintName},
		CountOnly: true,
	})
	_, total, err := s.store.ListBeads(ctx, filter)
	return total, err
}

// taintLastScrubAt resolves the most recent approved scrub-request's
// completion time for taintName, under the caller's own read scope: the
// newest (by updated_at) approved scrub-request bead naming this taint,
// preferring its executed_at field over approved_at when both are set.
// Returns nil when no scrub has ever been approved (under this scope).
func (s *BeadsServer) taintLastScrubAt(ctx context.Context, taintName string) (*time.Time, error) {
	filter := s.scopeListFilter(ctx, model.BeadFilter{
		Type: []model.BeadType{model.TypeScrubRequest},
		Fields: map[string]string{
			"taint":           taintName,
			"approval_status": "approved",
		},
		Sort:  "-updated_at",
		Limit: 1,
	})
	beads, _, err := s.store.ListBeads(ctx, filter)
	if err != nil {
		return nil, err
	}
	if len(beads) == 0 {
		return nil, nil
	}
	fields := extractScrubRequestFields(beads[0])
	for _, key := range []string{"executed_at", "approved_at"} {
		raw, ok := fields[key].(string)
		if !ok || raw == "" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, raw); err == nil {
			return &ts, nil
		}
	}
	return nil, nil
}

// canSeeTaintDetail reports whether the caller may see a taint's
// scrub_policy: an admin, or a member of the taint's owner team (ADR §9.1:
// "policies and grants are visible only to the owner team and admins").
// This is a team-membership check, distinct from the role-based
// admin-only gate on POST /v1/taints (ADR §7).
func (s *BeadsServer) canSeeTaintDetail(r *http.Request, ownerTeam string) (bool, error) {
	return s.canSeeTaintDetailCtx(r.Context(), ownerTeam)
}

// canSeeTaintDetailCtx is the context-only form of canSeeTaintDetail, for
// callers (e.g. teamHeldGrants) that resolve visibility per-taint outside an
// http.Request-shaped call site.
func (s *BeadsServer) canSeeTaintDetailCtx(ctx context.Context, ownerTeam string) (bool, error) {
	if hasMinimumRole(RolesFromContext(ctx), RoleAdmin) {
		return true, nil
	}
	teams, err := s.TeamsOf(ctx)
	if err != nil {
		return false, err
	}
	for _, t := range teams {
		if t == ownerTeam {
			return true, nil
		}
	}
	return false, nil
}

// callerMayCreateTaint reports whether the caller may create a taint (ADR
// §7: role "admin" — a role check, not a team check). The route's
// requireRole(RoleAdmin) registration only enforces that role for
// OIDC-authenticated callers: by design it lets every non-OIDC auth method
// (the shared primary static token every agent and coopmux hold, or a
// fleet token) through unchecked, as the implicit service-to-service trust
// boundary the rest of the admin surface relies on. Taint creation cannot
// afford that bypass: OwnerTeam is caller-supplied on this very request,
// and creating a taint implicitly grants its owner team full read access
// with zero further checks and no taint_grants row (ADR §10.1) — so a bare
// static or fleet caller must never reach this handler, regardless of any
// relayed X-Operated-By identity. This deliberately has no owner-team
// carve-out (unlike callerMayManageTaint, used by grant/revoke on an
// *existing* taint's already-fixed owner): the team named in a create
// request has not been vetted as anything yet, so "caller belongs to the
// team named in their own request" would let any team member self-declare
// ownership of a brand-new taint — exactly the privilege escalation this
// admin-only gate exists to prevent.
func callerMayCreateTaint(ctx context.Context) bool {
	return authMethodFromContext(ctx) == authMethodOIDC && hasMinimumRole(RolesFromContext(ctx), RoleAdmin)
}

// handleCreateTaint handles POST /v1/taints. Admin-only (ADR §7 / task
// gd-sygX3b8TAg), enforced by callerMayCreateTaint rather than relying
// solely on the route's requireRole(RoleAdmin) wrapper (see that function's
// doc comment for why). Creating a taint implicitly grants its owner team
// full read access (ADR §10.1: "ownership implies the grant"); no
// taint_grants row is written here.
func (s *BeadsServer) handleCreateTaint(w http.ResponseWriter, r *http.Request) {
	if !callerMayCreateTaint(r.Context()) {
		writeError(w, http.StatusForbidden, "insufficient permissions: requires "+RoleAdmin)
		return
	}

	var req CreateTaintRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Preset != "" && len(req.ScrubPolicy) > 0 {
		writeError(w, http.StatusBadRequest, "preset and scrub_policy are mutually exclusive")
		return
	}

	policy := req.ScrubPolicy
	if req.Preset != "" {
		if !model.IsValidPreset(req.Preset) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown preset %q", req.Preset))
			return
		}
		encoded, err := json.Marshal(model.PresetScrubPolicy(req.Preset, req.OwnerTeam))
		if err != nil {
			writeServerError(r.Context(), w, "handleCreateTaint", err, "failed to encode preset scrub_policy")
			return
		}
		policy = encoded
	}

	t := &model.Taint{
		Name:        req.Name,
		Description: req.Description,
		OwnerTeam:   req.OwnerTeam,
		ScrubPolicy: policy,
		Effects:     req.Effects,
	}
	if msg := untrustedInputCreateError(t); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if err := s.store.SetTaint(r.Context(), t); err != nil {
		var ve *model.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, ve.Error())
			return
		}
		writeServerError(r.Context(), w, "handleCreateTaint", err, "failed to create taint")
		return
	}

	// A brand-new taint just flipped the registry from (possibly) empty to
	// non-empty; clear this process's cached "empty" verdict immediately so
	// this replica's own subsequent reads never wait out taintRegistryEmptyTTL
	// (gd-XxrObXvc8r). Other replicas still rely on the TTL.
	s.InvalidateTaintRegistryCache()
	// The owner team holds the new taint implicitly (§10.1), so its members'
	// cached grant sets are stale too.
	s.invalidateAllGrantSets()

	writeJSON(w, http.StatusCreated, taintDetailFromModel(t))
}

// handleListTaints handles GET /v1/taints. Every viewer sees name, owner
// team, description and timestamps; scrub_policy is included per-row only
// for the owner team or an admin (ADR §9.1).
func (s *BeadsServer) handleListTaints(w http.ResponseWriter, r *http.Request) {
	taints, err := s.store.ListTaints(r.Context())
	if err != nil {
		writeServerError(r.Context(), w, "handleListTaints", err, "failed to list taints")
		return
	}

	out := make([]any, 0, len(taints))
	for _, t := range taints {
		visible, err := s.canSeeTaintDetail(r, t.OwnerTeam)
		if err != nil {
			writeServerError(r.Context(), w, "handleListTaints", err, "failed to resolve team membership")
			return
		}
		stats, err := s.taintRegistryStatsFor(r.Context(), t)
		if err != nil {
			writeServerError(r.Context(), w, "handleListTaints", err, "failed to compute taint registry stats")
			return
		}
		canApply, err := s.callerHoldsTaintGrant(r.Context(), t)
		if err != nil {
			writeServerError(r.Context(), w, "handleListTaints", err, "failed to resolve taint grant")
			return
		}
		if visible {
			out = append(out, taintDetailWithStats{taintDetail: *taintDetailFromModel(t), taintRegistryStats: stats, CanApply: canApply})
		} else {
			out = append(out, taintListEntryWithStats{taintListEntry: taintListEntryFromModel(t), taintRegistryStats: stats, CanApply: canApply})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"taints": out})
}

// restrictedTaintViewWithCanApply and taintDetailWithCanApply carry the same
// CanApply flag ListTaints already computes per row (task gd-nOnc1Q8Zcv), so
// the viewer's /taints/{name} detail page can show its own "Apply taint"
// action from a single GET instead of re-fetching the whole registry to find
// this one row.
type restrictedTaintViewWithCanApply struct {
	restrictedTaintView
	CanApply bool `json:"can_apply"`
}

type taintDetailWithCanApply struct {
	taintDetail
	CanApply bool `json:"can_apply"`
}

// handleGetTaint handles GET /v1/taints/{name}. The owner team or an admin
// gets the full row including scrub_policy; anyone else gets name and
// description only, no policy (task gd-sygX3b8TAg acceptance criteria).
func (s *BeadsServer) handleGetTaint(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	t, err := s.store.GetTaint(r.Context(), name)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "taint not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleGetTaint", err, "failed to get taint")
		return
	}

	visible, err := s.canSeeTaintDetail(r, t.OwnerTeam)
	if err != nil {
		writeServerError(r.Context(), w, "handleGetTaint", err, "failed to resolve team membership")
		return
	}
	canApply, err := s.callerHoldsTaintGrant(r.Context(), t)
	if err != nil {
		writeServerError(r.Context(), w, "handleGetTaint", err, "failed to resolve taint grant")
		return
	}
	if !visible {
		writeJSON(w, http.StatusOK, restrictedTaintViewWithCanApply{
			restrictedTaintView: restrictedTaintView{Name: t.Name, Description: t.Description},
			CanApply:            canApply,
		})
		return
	}
	writeJSON(w, http.StatusOK, taintDetailWithCanApply{
		taintDetail: *taintDetailFromModel(t),
		CanApply:    canApply,
	})
}
