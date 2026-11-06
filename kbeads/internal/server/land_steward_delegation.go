package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Land steward delegation (escalation-routing D8, Task 11). A tenant land's
// owner lists, in its project bead's land_steward_approves, the operation
// types its steward may approve. landDelegatesToSteward
// (escalation_lane_members.go) reads the list; the only writer is
// POST /v1/lands/{land}/steward-delegation, which validates it. A generic
// create, PATCH or field endpoint that sets it is a 400 for every caller,
// admins included, so the validation has one way in.

const landStewardApprovesField = "land_steward_approves"

// stewardDelegationEndpoint is named in the 400 a generic write gets.
const stewardDelegationEndpoint = "POST /v1/lands/{land}/steward-delegation"

// defaultLandStewardApproves is what a new tenant land delegates to its
// steward (owner decision gd-fBSlZfEvmo): the steward approves the land's PR
// merges, the land owner approves the rest. The owner widens or narrows it
// through stewardDelegationEndpoint; an explicit [] is the owner's revoke.
var defaultLandStewardApproves = []string{"github_pr_merge"}

// stewardDelegableMerges are the merge operation types a land may delegate.
// scopedApprovableOperations marks them false (a scoped human never approves
// one), so they are listed here rather than read from it.
var stewardDelegableMerges = map[string]bool{"github_pr_merge": true, "gitlab_mr_merge": true}

// validateLandStewardApproves accepts a set of operation types a land may
// delegate to its steward: approvable ones only, never a D3 floor operation
// type (escalationFloorOperation: the one floor).
func validateLandStewardApproves(ops []string) error {
	seen := map[string]bool{}
	for _, op := range ops {
		if seen[op] {
			return inputError(fmt.Sprintf("land_steward_approves: duplicate %q", op))
		}
		seen[op] = true
		if escalationFloorOperation(op) != "" {
			return inputError(fmt.Sprintf("land_steward_approves: %q is operator-only (secrets, credentials or approval rights)", op))
		}
		if !stewardDelegableOperation(op) {
			return inputError(fmt.Sprintf("land_steward_approves: %q is not a delegable operation type", op))
		}
	}
	return nil
}

// stewardDelegableOperation reports whether op may be delegated: a merge, or
// an operation a scoped human may approve itself (create, update, close).
// Entries must be exactly as landDelegatesToSteward compares them, so a value
// that is not already normalized is refused rather than stored unreadable.
func stewardDelegableOperation(op string) bool {
	if op != normalizeOperationType(op) {
		return false
	}
	return stewardDelegableMerges[op] || scopedApprovableOperations[op]
}

// landStewardApprovesWriteError refuses a generic project write that adds,
// changes or removes land_steward_approves. Keys are matched in any case:
// landDelegatesToSteward decodes with encoding/json, which matches the tag
// case-insensitively. prior is nil on create.
func landStewardApprovesWriteError(beadType model.BeadType, prior, current []byte) error {
	if beadType != model.TypeProject {
		return nil
	}
	names := []string{landStewardApprovesField}
	if reflect.DeepEqual(escalationFieldValues(prior, names), escalationFieldValues(current, names)) {
		return nil
	}
	return inputError(landStewardApprovesField + " is written only by the land owner: use " + stewardDelegationEndpoint)
}

// stampDefaultLandStewardApproves gives a tenant land's project bead
// defaultLandStewardApproves on create. The daemon stamps it because no
// creator can: landStewardApprovesWriteError refuses the key on every create,
// and the provisioner's machine principal may not use the owner endpoint. It
// runs after that guard, so the key is absent; any other project is returned
// unchanged.
func stampDefaultLandStewardApproves(raw json.RawMessage) (json.RawMessage, error) {
	if kind, _, _ := exactFieldString(raw, "land_kind"); kind != landKindTenant {
		return raw, nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("project fields: %w", err)
	}
	if _, ok := fields[landStewardApprovesField]; ok {
		return raw, nil
	}
	ops, err := json.Marshal(defaultLandStewardApproves)
	if err != nil {
		return nil, err
	}
	fields[landStewardApprovesField] = ops
	return json.Marshal(fields)
}

// checkLandStewardApprovesFieldEndpoint is the same refusal for the field
// endpoints (/fields/{f}/edit|append|reorder, /increment) and a close body,
// which name the keys they write.
func checkLandStewardApprovesFieldEndpoint(_ context.Context, bead *model.Bead, fieldKeys ...string) error {
	if bead == nil || bead.Type != model.TypeProject {
		return nil
	}
	for _, k := range fieldKeys {
		if strings.EqualFold(k, landStewardApprovesField) {
			return inputError(landStewardApprovesField + " is written only by the land owner: use " + stewardDelegationEndpoint)
		}
	}
	return nil
}

// stewardDelegationRequest is the body of POST /v1/lands/{land}/steward-delegation.
type stewardDelegationRequest struct {
	OperationTypes []string `json:"operation_types"`
}

// stewardDelegationDenial authorizes a steward_delegate on land: a
// gardenio-admin, or a scoped principal whose tier in land allows
// actionStewardDelegate on its project bead (the owner). Machines, fleet
// tokens and the shared static token are neither, so they are refused.
func stewardDelegationDenial(ctx context.Context, land string) *AuthzDenial {
	if callerMayApproveAsAdmin(ctx) {
		return nil
	}
	refused := &AuthzDenial{
		Action:        actionStewardDelegate,
		BeadType:      string(model.TypeProject),
		RequiredScope: land,
		Message:       "only the owner of land " + land + " (or a gardenio-admin) may set its steward delegation",
	}
	if !isScopedPrincipal(ctx) {
		return refused
	}
	labels := []string{"project:" + land}
	if d := scopedProjectDenial(ctx, actionStewardDelegate, model.TypeProject, labels); d != nil {
		return d
	}
	if d := scopedTierDenial(ctx, actionStewardDelegate, model.TypeProject, labels); d != nil {
		return d
	}
	return nil
}

// handleSetStewardDelegation handles POST /v1/lands/{land}/steward-delegation
// (D8): the land owner sets land_steward_approves. An empty list revokes. A
// revoke does not re-route open escalations: membership is checked at approve
// time, so the steward's next approve is refused at once, and the idle-lane
// hand-up (D5) moves the escalation to land-owner:<land>.
func (s *BeadsServer) handleSetStewardDelegation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	land := strings.TrimSpace(r.PathValue("land"))
	if land == "" {
		writeError(w, http.StatusBadRequest, "land is required")
		return
	}
	// Authorize before any lookup, so the route is no oracle for land names.
	if d := stewardDelegationDenial(ctx, land); d != nil {
		s.auditDenial(r, d, land)
		writeAuthzDenial(w, d)
		return
	}
	var req stewardDelegationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.OperationTypes == nil {
		req.OperationTypes = []string{}
	}
	if err := validateLandStewardApproves(req.OperationTypes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	project, err := s.store.FindProjectByName(ctx, land)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && project == nil):
		writeError(w, http.StatusNotFound, "no such land")
		return
	case err != nil:
		writeServerError(ctx, w, "handleSetStewardDelegation", err, "failed to look up the land")
		return
	}
	if kind, _, _ := exactFieldString(project.Fields, "land_kind"); kind != landKindTenant {
		writeError(w, http.StatusBadRequest, "steward delegation is for tenant lands")
		return
	}
	ops := slices.Clone(req.OperationTypes)
	slices.Sort(ops)
	if err := s.mergeBeadFields(withMachineWrite(ctx), project.ID, map[string]any{landStewardApprovesField: ops}); err != nil {
		writeServerError(ctx, w, "handleSetStewardDelegation", err, "failed to set the steward delegation")
		return
	}
	updated, err := s.store.GetBead(ctx, project.ID)
	if err != nil || updated == nil {
		writeServerError(ctx, w, "handleSetStewardDelegation", fmt.Errorf("re-read project %s: %w", project.ID, err), "failed to re-read the land")
		return
	}
	actor := actorFromContext(ctx)
	s.recordAndPublish(ctx, events.TopicBeadUpdated, project.ID, actor, events.BeadUpdated{
		Bead:    updated,
		Changes: map[string]any{landStewardApprovesField: ops},
		Actor:   actor,
	})
	slog.Info("lands: steward delegation set", "land", land, "project", project.ID, "actor", actor, "operation_types", ops)
	writeJSON(w, http.StatusOK, map[string]any{"land": land, "operation_types": ops})
}
