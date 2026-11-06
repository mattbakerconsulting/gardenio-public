package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/idgen"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// createBeadInput holds transport-agnostic parameters for creating a bead.
type createBeadInput struct {
	Title       string          `json:"title"`
	ShortTitle  string          `json:"short_title"`
	Heraldry    string          `json:"heraldry"` // arms in any input form; normalised to canonical (heraldry.go)
	Kind        string          `json:"kind"`
	Type        string          `json:"type"`
	Description string          `json:"description"`
	Notes       string          `json:"notes"`
	Priority    int             `json:"priority"`
	Assignee    string          `json:"assignee"`
	Owner       string          `json:"owner"`
	Labels      []string        `json:"labels"`
	CreatedBy   string          `json:"created_by"`
	Fields      json.RawMessage `json:"fields"`
	DueAt       *time.Time      `json:"due_at,omitempty"`
	DeferUntil  *time.Time      `json:"defer_until,omitempty"`

	// Status lets the caller create a bead in a non-open initial state.
	// Empty defaults to "open" (the existing behavior). The primary use
	// case is append-only ledger records (type=event audit beads) that
	// have no lifecycle and should never appear in ready/news queries —
	// see ka-0000004100. When Status == "closed", ClosedAt is stamped to
	// the create timestamp so the data invariant
	// (status=closed ⇔ closed_at IS NOT NULL) holds.
	Status string `json:"status,omitempty"`

	// Force bypasses content-shape guards that would otherwise reject the
	// bead (currently the decision ack-only / no-fork guard). Set via
	// `gb decision create --force` for legitimate edge cases. Never derived
	// from authentication; clients can always opt to bypass — the guard is a
	// nudge, not a security boundary. Each bypass is logged so the
	// operator can audit usage.
	Force bool `json:"force,omitempty"`

	// ParentBeadID, when set, wires a parent-child dep from the new bead to
	// the given bead inside the same transaction as the create, and
	// suppresses the post-create server-side auto-parent step (ka-0000007622,
	// D5 of epic ka-0000007507). Use this whenever the caller has an
	// explicit lineage target (formula apply linking steps to molecule,
	// `/formula pour` linking the molecule to the originating Slack thread
	// bead, etc.) so the bead doesn't accumulate a second auto-parent edge
	// to the agent's claimed task. Wire field: proto field 14 in
	// beads.v1.CreateBeadRequest.
	ParentBeadID string `json:"parent_bead_id,omitempty"`

	// IdempotencyKey, when set, makes createBead lookup-or-return instead of
	// always inserting. See beads_idempotency.go for the full contract
	// (per-project scope, no second create event, migration-029 race
	// backstop). Wire: proto field 15, HTTP body/query param idempotency_key,
	// CLI `kd create --idempotency-key`. (ka-0000002856)
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// SawOpenHash carries the caller's acknowledgement of the current open
	// decision/escalation list for the dedup-ack gate (epic ka-0000004599,
	// dedup_ack.go). When creating a decision/escalation and one or more open
	// beads of that type already exist in the project, the daemon rejects the
	// create with a 409 dedup_ack_required body containing the open list and a
	// hash; the caller re-submits with this field set to that hash. The gate
	// is behind BEADS_DEDUP_GATE_ENABLED (default off). Wire: HTTP body field
	// saw_open_hash (or query param saw_open_decisions / saw_open_escalations),
	// CLI `kd create --saw-open-decisions=<hash>`. This is a top-level input
	// field, NOT a bead `field`, so it is exempt from the type schema's
	// unknown-field validation (which would reject a saw_open_* entry in the
	// strict decision/escalation schemas). gRPC round-trip is a follow-up
	// (protoc v6 regen required to add the proto field).
	SawOpenHash string `json:"saw_open_hash,omitempty"`
}

// createBead validates input, persists a new bead with labels, and publishes
// a BeadCreated event. Returns inputError for validation failures.
func (s *BeadsServer) createBead(ctx context.Context, in createBeadInput) (*model.Bead, error) {
	// Mutexes a step takes across the insert are released when createBead
	// returns (createLocks, beads_create_steps.go).
	var locks createLocks
	defer func() { locks.release() }()

	if in.Title == "" {
		return nil, inputError("title is required")
	}
	if err := scrubCreateInput(&in); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	id, err := idgen.Generate()
	if err != nil {
		return nil, fmt.Errorf("failed to generate ID: %w", err)
	}

	beadType, tc, err := s.resolveCreateType(ctx, in.Type)
	if err != nil {
		return nil, err
	}

	ctx = withCreateLineageSeeds(ctx, in)
	plan, err := s.newCreatePlan(ctx, now)
	if err != nil {
		return nil, err
	}
	if err := s.checkCreateGates(ctx, in, beadType, id, plan.operatedBy); err != nil {
		return nil, err
	}
	if err := s.settleCreateInput(ctx, &in, beadType, tc, plan); err != nil {
		return nil, err
	}

	bead, err := newBeadForCreate(ctx, in, id, beadType, tc, plan)
	if err != nil {
		return nil, err
	}
	if err := s.validateNewBead(ctx, bead, &in, tc, &locks); err != nil {
		return nil, err
	}

	projectLabelMismatch := s.deriveCreateLabels(ctx, bead, tc, in.ParentBeadID)
	supportPost, err := s.authorizeCreate(ctx, bead, projectLabelMismatch)
	if err != nil {
		return nil, err
	}
	if err := s.routeAndAuthorizeSpawn(ctx, bead); err != nil {
		return nil, err
	}
	if err := s.checkCreateLandRules(ctx, bead, supportPost, &locks); err != nil {
		return nil, err
	}
	if err := s.validateLandsCreate(ctx, bead); err != nil {
		return nil, err
	}

	existing, ratesEdgeTo, err := s.findExistingForCreate(ctx, bead, in, &locks)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	plan.ratesEdgeTo = ratesEdgeTo

	if err := s.prepareInsert(ctx, bead, in.ParentBeadID, plan); err != nil {
		return nil, err
	}

	// One transaction writes the bead and its rows (writeCreatedBeadRows).
	err = s.store.RunInTransaction(ctx, func(tx store.Store) error {
		if err := tx.CreateBead(ctx, bead); err != nil {
			return fmt.Errorf("failed to create bead: %w", err)
		}
		return writeCreatedBeadRows(ctx, tx, bead, plan)
	})
	if err != nil {
		return s.recoverCreateConflict(ctx, bead, in.IdempotencyKey, err)
	}
	if len(plan.pendingAssignGrants) > 0 {
		s.invalidateAgentGrantSet(bead.ID)
	}
	s.publishCreated(ctx, bead, plan)

	// Standing consent (gd-QV2WuUjely) and owner rulings (P7.5) answer what
	// they cover here, by the daemon, never by the agent that filed it.
	bead = s.answerByStandingRules(ctx, bead)

	return bead, nil
}

// channelFromLabels extracts the first `channel:<name>` label off a bead
// (returning "<name>"), or "" if no such label is present. Used to populate
// MailSent.Channel — mail beads do not have a dedicated channel field.
func channelFromLabels(labels []string) string {
	for _, l := range labels {
		if rest, ok := strings.CutPrefix(l, "channel:"); ok {
			return rest
		}
	}
	return ""
}

// preCreateRating runs the type=rating-specific gates before a new rating bead
// is persisted: rate-of-rating rejection and the idempotent
// (rater, rated_bead_id) upsert lookup that turns a repeat CreateBead from the
// same rater into an UpdateBead in place. Returns the existing bead (and nil
// for ratesEdgeTo) when an update was performed; otherwise returns ratesEdgeTo
// set to the rated bead id so the caller wires the
// `rating-bead --rates--> rated-bead` edge inside the create transaction.
//
// Plan-of-record: gardenio/plans/beads-viewer-human-ratings.md §2.2, §5.3.
func (s *BeadsServer) preCreateRating(ctx context.Context, bead *model.Bead, in createBeadInput) (*model.Bead, string, error) {
	var rf struct {
		RatedBeadID string `json:"rated_bead_id"`
		Rater       string `json:"rater"`
		Score       *int   `json:"score"`
		Source      string `json:"source"`
	}
	if len(bead.Fields) > 0 {
		_ = json.Unmarshal(bead.Fields, &rf)
	}
	if rf.RatedBeadID == "" || rf.Rater == "" {
		// ValidateTypeFields already rejects this; defensive guard so the rest of
		// this helper can assume both are populated.
		return nil, "", nil
	}

	// T4a-2: score=0 is the slack-feedback-only sentinel and is only legal
	// when paired with source=slack-feedback-only. The schema allows score>=0
	// (relaxed from min=1) so the validator accepts both viewer rows (1..5)
	// and Slack feedback-only rows. This runtime guard rejects the otherwise-
	// underspecified score=0 + non-feedback-only combination.
	// See gardenio/plans/beads-viewer-human-ratings-slack-fold.md §2, §3.3.
	if rf.Score != nil && *rf.Score == 0 && rf.Source != "slack-feedback-only" {
		return nil, "", inputError("score=0 is only valid when source=slack-feedback-only")
	}

	rated, err := s.store.GetBead(ctx, rf.RatedBeadID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, "", fmt.Errorf("failed to load rated bead: %w", err)
	}
	if rated == nil {
		return nil, "", inputError("rated_bead_id " + rf.RatedBeadID + " not found")
	}
	if rated.Type == "rating" {
		return nil, "", inputError("cannot rate a rating bead (no rate-of-rating loop)")
	}

	// Idempotent upsert: lookup by (rater, rated_bead_id). On hit, merge the
	// new score/axes/tags/comment into the existing rating bead via updateBead
	// (patch semantics) and return the updated bead.
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type: []model.BeadType{"rating"},
		Fields: map[string]string{
			"rated_bead_id": rf.RatedBeadID,
			"rater":         rf.Rater,
		},
		Limit: 1,
	})
	if err != nil {
		return nil, "", fmt.Errorf("failed to lookup existing rating: %w", err)
	}
	if len(beads) > 0 {
		ex := beads[0]
		patch := updateBeadInput{
			Title:       &in.Title,
			Description: &in.Description,
			Fields:      bead.Fields,
		}
		updated, uErr := s.updateBead(ctx, ex.ID, patch)
		if uErr != nil {
			return nil, "", uErr
		}
		return updated, "", nil
	}

	return nil, rf.RatedBeadID, nil
}

// CreateBead validates the request, persists a new bead, publishes a BeadCreated event,
// and returns the full bead.
func (s *BeadsServer) CreateBead(ctx context.Context, req *beadsv1.CreateBeadRequest) (*beadsv1.CreateBeadResponse, error) {
	// SECURITY (AUDIT §C3): ignore req.CreatedBy — the server sets CreatedBy
	// from the authenticated identity inside createBead(), which is the only
	// authoritative source of truth.
	bead, err := s.createBead(ctx, createBeadInput{
		Title:          req.GetTitle(),
		Kind:           req.GetKind(),
		Type:           req.GetType(),
		Description:    req.GetDescription(),
		Notes:          req.GetNotes(),
		Priority:       int(req.GetPriority()),
		Assignee:       req.GetAssignee(),
		Owner:          req.GetOwner(),
		Labels:         req.GetLabels(),
		Fields:         json.RawMessage(req.GetFields()),
		DueAt:          protoTimestamp(req.GetDueAt()),
		DeferUntil:     protoTimestamp(req.GetDeferUntil()),
		ParentBeadID:   req.GetParentBeadId(),
		IdempotencyKey: req.GetIdempotencyKey(),
		ShortTitle:     req.GetShortTitle(),
		Heraldry:       req.GetHeraldry(),
	})
	if err != nil {
		var denial *AuthzDenial
		if errors.As(err, &denial) {
			return nil, status.Error(codes.PermissionDenied, denial.Error())
		}
		var refusal *operatorRefusalError
		if errors.As(err, &refusal) {
			return nil, grpcOperatorRefusal(refusal.Source)
		}
		var dedup *dedupAckError
		if errors.As(err, &dedup) {
			// gRPC has no structured-body channel like HTTP's 409 JSON, so
			// embed the machine-readable payload as JSON in the status message
			// (mirroring grpcOperatorRefusal). A gRPC client that wants to
			// retry-with-hash parses this; the human message leads. The kd CLI
			// defaults to HTTP transport, where the full structured body is
			// available. See ka-0000004599.
			payload, _ := json.Marshal(map[string]any{
				"code":       dedup.Code(),
				"bead_type":  dedup.BeadType,
				"project":    dedup.Project,
				"hash":       dedup.Hash,
				"total_open": dedup.TotalOpen,
				"retry_with": dedup.RetryWith,
			})
			return nil, status.Errorf(codes.FailedPrecondition, "%s %s", dedup.Error(), string(payload))
		}
		var dup *duplicateOpenError
		if errors.As(err, &dup) {
			return nil, status.Error(codes.AlreadyExists, dup.Error())
		}
		if hc, ok := asHeraldryConflict(err); ok {
			return nil, status.Error(codes.AlreadyExists, hc.Error())
		}
		var ie inputError
		if errors.As(err, &ie) {
			return nil, status.Error(codes.InvalidArgument, ie.Error())
		}
		return nil, status.Errorf(codes.Internal, "%v", err)
	}

	return &beadsv1.CreateBeadResponse{Bead: beadToProto(bead)}, nil
}

// GetBead retrieves a single bead by ID.
func (s *BeadsServer) GetBead(ctx context.Context, req *beadsv1.GetBeadRequest) (*beadsv1.GetBeadResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}

	bead, err := s.store.GetBead(ctx, req.GetId())
	if err != nil {
		return nil, storeError(err, "bead")
	}
	// A bead the caller cannot read answers as missing (beadHiddenFromCaller).
	if bead == nil || s.beadHiddenFromCaller(ctx, bead) {
		return nil, status.Error(codes.NotFound, "bead not found")
	}
	if s.readHidesBead(ctx, bead) {
		return nil, status.Error(codes.PermissionDenied, errReadTypeDenied.Error())
	}
	// The taint read gate and inherit:read, as handleGetBead applies them.
	if denied, err := s.subresourceTaintDenied(ctx, bead.ID); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to resolve taint grants: %v", err)
	} else if denied {
		recordTaintReadDenial(ctx, "read", string(bead.Type))
		return nil, status.Error(codes.NotFound, "bead not found")
	}
	if err := s.propagateReadTaint(ctx, bead.ID); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to propagate read taint: %v", err)
	}

	return &beadsv1.GetBeadResponse{Bead: beadToProto(bead)}, nil
}

// ListBeads returns a filtered, paginated list of beads.
func (s *BeadsServer) ListBeads(ctx context.Context, req *beadsv1.ListBeadsRequest) (*beadsv1.ListBeadsResponse, error) {
	filter, err := buildListFilter(req)
	if err != nil {
		return nil, err
	}
	// Reject unknown `--type` filter values server-side (ka-0000008041) — the
	// single choke point that covers every ListBeads client, not just the kd
	// CLI guard. filter.Type is already alias-resolved by buildListFilter.
	// validateFilterTypes returns inputError for an unknown type (mapped to
	// InvalidArgument, matching the create path) and a gRPC-status error for an
	// internal store fault; both propagate correctly.
	if err := s.validateFilterTypes(ctx, filter.Type); err != nil {
		var ie inputError
		if errors.As(err, &ie) {
			return nil, status.Error(codes.InvalidArgument, ie.Error())
		}
		return nil, err
	}
	// All tokens read across all fleets. `bead.fleet` is an audit-attribution
	// field only — writes still stamp the resolved fleet (see beadToBead /
	// CreateBead). The old per-token read-isolation gate (`CrossFleetRead`)
	// was a YAGNI multi-tenant escape hatch that only ever bit bridge tokens
	// that needed to read across fleets to do their job. Removed under epic
	// ka-0000000871 / ka-0000003285 (resolves ka-0000001804 + ka-0000003707).
	filter.Fleet = model.FleetAll

	// Enforce project scope for project-bound fleet tokens, OIDC read-side
	// project isolation, and the taint read gate (ADR docs/adr/taints.md
	// §5.1/§10.1: taintsOf(bead) ⊆ grantsOf(caller)) — the third, narrower
	// check a caller who passes both project gates must still satisfy.
	filter = s.scopeListFilter(ctx, filter)

	beads, total, err := s.store.ListBeads(ctx, filter)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list beads: %v", err)
	}
	if err := s.propagateReadTaintBeads(ctx, beadIDsOf(beads)); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to propagate read taint: %v", err)
	}

	pbBeads := make([]*beadsv1.Bead, 0, len(beads))
	for _, b := range beads {
		pbBeads = append(pbBeads, beadToProto(b))
	}

	return &beadsv1.ListBeadsResponse{
		Beads: pbBeads,
		Total: int32(total),
	}, nil
}

// updateBeadInput holds transport-agnostic parameters for updating a bead.
// Pointer fields indicate optionality: nil means "don't change".
type updateBeadInput struct {
	Title       *string         `json:"title,omitempty"`
	ShortTitle  *string         `json:"short_title,omitempty"`
	Heraldry    *string         `json:"heraldry,omitempty"` // "" or "none" removes the arms
	Description *string         `json:"description,omitempty"`
	Notes       *string         `json:"notes,omitempty"`
	Status      *string         `json:"status,omitempty"`
	Priority    *int            `json:"priority,omitempty"`
	Assignee    *string         `json:"assignee,omitempty"`
	Owner       *string         `json:"owner,omitempty"`
	DueAt       *time.Time      `json:"due_at,omitempty"`
	DeferUntil  *time.Time      `json:"defer_until,omitempty"`
	Fields      json.RawMessage `json:"fields,omitempty"`
	Labels      []string        `json:"labels,omitempty"`

	// OnlyIfShortTitleEmpty makes ShortTitle a fill, not a replace: it is
	// applied only when the bead has no short_title yet, so the groomer never
	// overwrites an authored value (plan 2026-10-14-short-titles D5).
	OnlyIfShortTitleEmpty bool `json:"only_if_short_title_empty,omitempty"`

	// dueAtSet / deferUntilSet track whether the field was provided at all
	// (since a nil *time.Time means "clear the field", distinct from "not provided").
	dueAtSet      bool
	deferUntilSet bool
	labelsSet     bool
}

// transientClosedBeadFieldKeys are field keys that the controller's status
// reporter writes during a pod's grace period after `gb stop` has already
// closed the bead. Dropping them on closed beads mirrors the close-idempotent
// rule from handleCloseBead so the agent's final state remains the record of
// truth (ka-0000005253).
var transientClosedBeadFieldKeys = []string{
	"agent_state",
	"pod_phase",
	"pod_name",
	"pod_namespace",
	"coop_url",
	"backend",
}

// serviceManagedFieldsByType maps bead Type → field keys that are written by
// automation services (bridges, schedulers, evaluators) as observability /
// bookkeeping state rather than user-authored content. Updates that ONLY
// touch these fields bypass the ownership gate so long-lived shared beads
// (e.g. trigger config beads created in a Slack thread, then evaluated for
// months by a fleet-token jira-bridge) can have their `last_checked` stamp
// advanced without an explicit ownership transfer or admin role.
//
// Project-scope and write-policy checks still apply — this only relaxes the
// per-bead created_by/assignee/owner gate. (ka-0000003039)
var serviceManagedFieldsByType = map[model.BeadType][]string{
	// FIRE-STATE ONLY. Every key here must be written by the TriggerEngine as
	// a record of what an evaluation cycle DID — never trigger configuration
	// (enabled, jql, cooldown_minutes, prompt, formula_id, role, the `gardener`
	// spec blob). A service actor that could patch config on a bead it does
	// not own would be a privilege escalation, not an observability bypass.
	//
	// KEEP THIS IN SYNC with the TriggerEngine stamp helpers in
	// gardenio/controller/internal/bridge/triggers.go. Adding a field to a stamp
	// patch without adding it here silently breaks the ENTIRE patch: the
	// bypass is all-or-nothing, so one unlisted key sends the whole write to
	// authorizeOwnership, which denies it for a bridge that did not create the
	// bead. That is exactly the ka-0000004245 outage — commit 6a8511bce
	// (2026-08-14) folded last_outcome/last_outcome_at into the last_checked
	// patch, froze last_checked on three prod jira triggers for 11 days, and
	// emitted ~180 authz-denial audit beads/hour. last_run kept working only
	// because stampLastRun writes a separate, fully-allowlisted patch.
	"trigger": {
		// stampCheckedOutcome — the once-per-cycle liveness stamp.
		"last_checked",
		"last_outcome",
		"last_outcome_at",
		// stampLastRun / gardenerPostFire — successful fire. last_match is the
		// persisted fireKey (gd-pLf5LSHDEh) so catch-up can skip an
		// already-fired match across a restart without the in-memory seen set.
		"last_run",
		"last_agent_id",
		"last_match",
		// stampFireError / clearFireError — failed fire (non-gardener arm).
		"last_fire_error",
		"last_fire_error_at",
	},
	"schedule": {
		// NOTE: "last_checked" was removed here (ka-0000005214). It was a DEAD
		// entry: type:schedule never declared a `last_checked` field, so such a
		// write 400s at field validation long before authz, and no scheduler
		// stamp helper writes it (the only producer of `last_checked` is
		// TriggerEngine.stampCheckedOutcome, on TRIGGER beads). The generic
		// declaration drift guard surfaced it — see
		// TestServiceOwnedDeclarationMatchesAllowlist.
		"last_checked_agent",
		"last_run",
		"last_agent_id",
		"runs_spawned",
		"runs_completed",
		"consecutive_failures",
		"progress_state",
		"run_log",
		"trigger_state",
	},
	"agent": {
		"context_tokens",
		"live_cost_usd",
		"live_turns",
	},
}

// patchOnlyChangesServiceManagedFields reports whether the update would
// change nothing except the service-managed fields registered for this bead
// type. It diffs the incoming patch against the existing stored fields so
// keys whose values are unchanged (echoed back by a read-modify-write client
// like beadsapi.Client.UpdateBeadFields) do not block the bypass.
//
// Returns false if there are no service-managed fields for the type, if the
// patch touches any non-fields column (title/status/labels/etc.), if the
// patch makes no net change, or if any changed field key falls outside the
// allowed set. Project-scope, type-scope, and write-policy still apply.
func patchOnlyChangesServiceManagedFields(beadType model.BeadType, in *updateBeadInput, existingFields json.RawMessage) bool {
	return patchOnlyChangesFields(serviceManagedFieldsByType[beadType], in, existingFields)
}

// patchOnlyChangesFields is the type-agnostic core of
// patchOnlyChangesServiceManagedFields: it answers "does this update change
// nothing outside `allowed`?" for an arbitrary allowlist. Split out so a
// second, identity-scoped allowlist can reuse the EXACT same semantics rather
// than growing a near-copy that drifts (see authorizeScifApprovalStamp,
// ka-0000001628). The semantics that matter and must not be duplicated
// approximately:
//
//   - a patch touching any non-fields column disqualifies entirely;
//   - unchanged keys echoed back by a read-modify-write client do not count as
//     a change (beadsapi.Client.UpdateBeadFields / UpdateBeadFieldsTyped);
//   - a net-zero patch returns false, so a no-op cannot buy a bypass.
func patchOnlyChangesFields(allowed []string, in *updateBeadInput, existingFields json.RawMessage) bool {
	if len(allowed) == 0 {
		return false
	}
	// Any non-fields column update disqualifies.
	if in.Title != nil || in.ShortTitle != nil || in.Heraldry != nil || in.Description != nil || in.Notes != nil ||
		in.Status != nil || in.Priority != nil || in.Assignee != nil ||
		in.Owner != nil || in.dueAtSet || in.deferUntilSet || in.labelsSet {
		return false
	}
	if len(in.Fields) == 0 {
		return false
	}
	var patch map[string]any
	if err := json.Unmarshal(in.Fields, &patch); err != nil {
		return false
	}
	var existing map[string]any
	if len(existingFields) > 0 {
		if err := json.Unmarshal(existingFields, &existing); err != nil {
			return false
		}
	}
	if existing == nil {
		existing = map[string]any{}
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, k := range allowed {
		allowedSet[k] = struct{}{}
	}
	// updateBead uses merge semantics for fields (see the Fields branch in
	// updateBead): keys present in the patch override existing values, keys
	// absent from the patch are preserved. So only keys present in the patch
	// can cause a net change.
	changed := false
	for k, pv := range patch {
		ev, ok := existing[k]
		if ok && reflect.DeepEqual(pv, ev) {
			continue
		}
		changed = true
		if _, allowedKey := allowedSet[k]; !allowedKey {
			return false
		}
	}
	return changed
}

// dropTransientClosedFields removes transient operational keys from the patch
// and clears Notes. Used on closed beads only.
func (in *updateBeadInput) dropTransientClosedFields() {
	in.Notes = nil
	if in.Fields != nil {
		var patch map[string]any
		if err := json.Unmarshal(in.Fields, &patch); err == nil {
			for _, k := range transientClosedBeadFieldKeys {
				delete(patch, k)
			}
			if len(patch) == 0 {
				in.Fields = nil
			} else if merged, mErr := json.Marshal(patch); mErr == nil {
				in.Fields = merged
			}
		}
	}
}

// hasAnyChange reports whether the input would mutate any field. Used after
// dropping transient keys on closed beads to decide whether to short-circuit.
func (in *updateBeadInput) hasAnyChange() bool {
	return in.Title != nil || in.ShortTitle != nil || in.Heraldry != nil || in.Description != nil || in.Notes != nil ||
		in.Status != nil || in.Priority != nil || in.Assignee != nil ||
		in.Owner != nil || in.dueAtSet || in.deferUntilSet ||
		in.Fields != nil || in.labelsSet
}

// patchOnlyChangesAssignee reports whether this patch touches nothing but
// the assignee field. Assignment is the ADR docs/adr/taints.md §4.2 row 2 /
// §10.1 propagation mechanism itself ("an agent is assigned tainted work ->
// it is granted for the taint until its agent bead closes") — the assigner
// need not already hold a grant for the task's taints to hand the task off,
// or the zero-config bootstrap of an agent's very first grant would be
// circular. A patch that ALSO touches bead content (title, description,
// fields, ...) alongside the assignee still goes through the taint write
// gate for that content change.
func (in *updateBeadInput) patchOnlyChangesAssignee() bool {
	return in.Assignee != nil &&
		in.Title == nil && in.ShortTitle == nil && in.Heraldry == nil && in.Description == nil && in.Notes == nil &&
		in.Status == nil && in.Priority == nil && in.Owner == nil &&
		!in.dueAtSet && !in.deferUntilSet && in.Fields == nil && !in.labelsSet
}

// machinePatchOwnershipAction is the ownership action for a PATCH: "close"
// when a machine principal's patch moves the bead into closed, else "update".
// A machine's update-only grants (LabeledOwnership with Actions update,
// ReclaimOwnership) must not close a bead through the generic update path
// (gd-RZJk5gspFv). Other callers keep "update": their ownership does not
// depend on the action.
func machinePatchOwnershipAction(ctx context.Context, bead *model.Bead, in *updateBeadInput) string {
	if authMethodFromContext(ctx) == authMethodMachine && in.Status != nil &&
		model.Status(*in.Status) == model.StatusClosed && bead.Status != model.StatusClosed {
		return "close"
	}
	return "update"
}

// updateBead applies partial updates to an existing bead, persists them,
// and publishes a BeadUpdated event. Returns inputError for validation failures.
// Its steps are in beads_update_steps.go.
func (s *BeadsServer) updateBead(ctx context.Context, id string, in updateBeadInput) (*model.Bead, error) {
	// F17 (R4-SEC-4): scrub bare credential SHAPES from agent-authored prose on
	// the update path too — a reasoning agent can leak a key into a `kd update
	// --description/--notes` just as into a create. Mirrors createBead; `Fields`
	// is deliberately left untouched (account beads store litellm_key there).
	if err := in.scrubProse(); err != nil {
		return nil, err
	}

	bead, err := s.store.GetBead(ctx, id)
	if err != nil {
		return nil, err
	}
	if bead == nil {
		return nil, sql.ErrNoRows
	}
	u := newBeadUpdate(bead, &in)

	if err := s.authorizeUpdate(ctx, u); err != nil {
		var denial *AuthzDenial
		if errors.As(err, &denial) && s.beadHiddenFromCaller(ctx, bead) {
			denial.hidden = true
		}
		return nil, err
	}
	if u.dropClosedStrayWrites() {
		return bead, nil
	}
	if err := s.checkUpdateCloseGuards(ctx, id, u); err != nil {
		return nil, err
	}

	u.applyColumns()
	if err := s.applyFieldsPatch(ctx, u); err != nil {
		return nil, err
	}
	if err := s.applyLabelsPatch(ctx, u); err != nil {
		return nil, err
	}
	u.reconcileClosedAt()
	bead.UpdatedAt = time.Now().UTC()

	if err := s.validateUpdatedBead(ctx, u); err != nil {
		return nil, err
	}
	if err := s.validateUpdatedFields(ctx, u); err != nil {
		return nil, err
	}
	if err := u.validateDocPayloadUpdate(); err != nil {
		return nil, err
	}
	// A channel or card write lock is held until the update is published.
	release, err := s.checkUpdatedConversation(ctx, u)
	defer release()
	if err != nil {
		return nil, err
	}

	if err := s.resealSpawnLineage(ctx, u); err != nil {
		return nil, err
	}
	assign, err := s.resolveUpdateAssignTaints(ctx, u)
	if err != nil {
		return nil, err
	}
	if err := s.store.RunInTransaction(ctx, func(tx store.Store) error {
		if err := u.checkLockHolder(ctx, tx); err != nil {
			return err
		}
		if u.shortTitleFill {
			if err := u.fillShortTitle(ctx, tx); err != nil {
				return err
			}
		} else if err := tx.UpdateBead(ctx, bead); err != nil {
			return fmt.Errorf("failed to update bead: %w", err)
		}
		return assign.apply(ctx, tx)
	}); err != nil {
		return nil, s.heraldryRaceConflict(ctx, bead, err)
	}
	if assign.agent != nil && len(assign.grants) > 0 {
		s.invalidateAgentGrantSet(assign.agent.ID)
	}
	if err := s.reconcileUpdatedLabels(ctx, u); err != nil {
		return nil, err
	}

	// Taint propagation on write (ADR docs/adr/taints.md §4.2 row 4): a
	// tainted writer's taints union onto the bead it just updated. Shared by
	// both the HTTP and gRPC UpdateBead transports, which both call this
	// function.
	if err := s.propagateWriteTaint(ctx, bead.ID, actorFromContext(ctx)); err != nil {
		return nil, fmt.Errorf("failed to propagate write taint: %w", err)
	}

	s.publishUpdate(ctx, u)
	s.runUpdateFollowUps(ctx, u)
	return bead, nil
}

// perFieldChanges diffs an incoming extensible-fields patch against the bead's
// existing fields and returns one entry per key whose value actually changed
// (bare key -> new value). A key absent from `existing` counts as a change
// (nil -> v). A null patch value means "delete the key" (RFC 7386): it counts
// as a change only when the key was actually present, so an unset of an
// already-absent key is a genuine no-op and never surfaces a phantom nil->nil
// transition. Values that are deep-equal are omitted so a no-op re-write of a
// field never surfaces as a transition. The result feeds the published
// BeadUpdated.Changes map so declarative `field_changed` / `changed_to`
// watchers can match a single custom-field rotation (ka-0000004185).
func perFieldChanges(existing, patch map[string]any) map[string]any {
	if len(patch) == 0 {
		return nil
	}
	out := make(map[string]any, len(patch))
	for k, v := range patch {
		prev, ok := existing[k]
		if v == nil {
			// Delete intent: only a change if the key was present.
			if !ok {
				continue
			}
			out[k] = nil
			continue
		}
		if ok && reflect.DeepEqual(prev, v) {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// maybeAddTriggeredByOnClaim wires a `triggered-by` lineage edge from the
// claimed bead to the trigger bead carried in ctx via X-Trigger-Bead. The
// edge is suppressed when:
//
//   - no trigger bead was propagated (header missing or rejected at the
//     interceptor),
//   - the trigger bead id equals the claimed bead (self-reference),
//   - the claimed bead is already parented by parent-child OR triggered-by
//     (the M2 spawn paths already wrote lineage, or a prior claim already
//     added the edge; either way the lineage is already recorded), or
//   - the store rejects the AddDependency call (logged, swallowed — the
//     claim itself must not roll back on a best-effort lineage write).
//
// Mirrors the maybeAddAutoParent shape and idempotency guarantees: the
// underlying AddDependency is ON CONFLICT DO NOTHING so a duplicate concurrent
// claim is harmless.
func (s *BeadsServer) maybeAddTriggeredByOnClaim(ctx context.Context, bead *model.Bead, now time.Time) {
	if bead == nil || s == nil || s.store == nil {
		return
	}
	trigger := triggerBeadFromContext(ctx)
	if trigger == "" || trigger == bead.ID {
		return
	}
	// A tenant caller's trigger must be a bead it can see: no edge to (and no
	// taints from) another land's or the fleet's bead (edgeTargetHidden).
	if s.callerIsTenant(ctx) {
		tb, err := s.store.GetBead(ctx, trigger)
		if err != nil || s.edgeTargetHidden(ctx, tb) {
			return
		}
	}

	deps, err := s.store.GetDependencies(ctx, bead.ID)
	if err != nil {
		slog.Warn("triggered-by claim: failed to fetch existing deps",
			"bead_id", bead.ID, "trigger", trigger, "err", err.Error())
		return
	}
	for _, d := range deps {
		if d == nil {
			continue
		}
		if d.Type == model.DepParentChild || d.Type == model.DepTriggeredBy {
			return
		}
	}

	dep := &model.Dependency{
		BeadID:       bead.ID,
		DependsOnID:  trigger,
		Type:         model.DepTriggeredBy,
		CreatedAt:    now,
		CreatedBy:    actorFromContext(ctx),
		OperatedBy:   bead.OperatedBy,
		OperatedByID: bead.OperatedByID,
	}
	if err := s.addDependencyWithTaintPropagation(ctx, dep); err != nil {
		slog.Warn("triggered-by claim: dep insert failed",
			"bead_id", bead.ID, "trigger", trigger, "err", err.Error())
		return
	}
	s.recordAndPublish(ctx, events.TopicDependencyAdded, bead.ID, dep.CreatedBy, events.DependencyAdded{
		Dependency: dep,
		Fleet:      FleetFromContext(ctx),
	})
}

// reconcileLabels compares the desired labels with the existing labels in
// the store and adds/removes as needed.
func (s *BeadsServer) reconcileLabels(ctx context.Context, beadID string, newLabels []string) error {
	existing, err := s.store.GetLabels(ctx, beadID)
	if err != nil {
		return err
	}

	existingSet := make(map[string]struct{}, len(existing))
	for _, l := range existing {
		existingSet[l] = struct{}{}
	}
	newSet := make(map[string]struct{}, len(newLabels))
	for _, l := range newLabels {
		newSet[l] = struct{}{}
	}

	// Remove labels that are no longer desired. This is an idempotent reconcile:
	// `existing` was just read, so every removal is expected to match a row, and
	// the rows-affected signal is intentionally discarded here (ka-0000000424).
	for _, l := range existing {
		if _, ok := newSet[l]; !ok {
			if _, err := s.store.RemoveLabel(ctx, beadID, l); err != nil {
				return err
			}
		}
	}
	// Add labels that are new.
	for _, l := range newLabels {
		if _, ok := existingSet[l]; !ok {
			if err := s.store.AddLabel(ctx, beadID, l); err != nil {
				return err
			}
		}
	}

	return nil
}

// UpdateBead applies partial updates to an existing bead.
func (s *BeadsServer) UpdateBead(ctx context.Context, req *beadsv1.UpdateBeadRequest) (*beadsv1.UpdateBeadResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}

	in := updateBeadInput{}
	if req.Title != nil {
		in.Title = req.Title
	}
	if req.ShortTitle != nil {
		in.ShortTitle = req.ShortTitle
	}
	in.OnlyIfShortTitleEmpty = req.GetOnlyIfShortTitleEmpty()
	if req.Heraldry != nil {
		in.Heraldry = req.Heraldry
	}
	if req.Description != nil {
		in.Description = req.Description
	}
	if req.Notes != nil {
		in.Notes = req.Notes
	}
	if req.Status != nil {
		in.Status = req.Status
	}
	if req.Priority != nil {
		p := int(*req.Priority)
		in.Priority = &p
	}
	if req.Assignee != nil {
		in.Assignee = req.Assignee
	}
	if req.Owner != nil {
		in.Owner = req.Owner
	}
	if req.DueAt != nil {
		in.DueAt = protoTimestamp(req.DueAt)
		in.dueAtSet = true
	}
	if req.DeferUntil != nil {
		in.DeferUntil = protoTimestamp(req.DeferUntil)
		in.deferUntilSet = true
	}
	if req.Fields != nil {
		in.Fields = json.RawMessage(req.Fields)
	}
	if len(req.Labels) > 0 {
		in.Labels = req.Labels
		in.labelsSet = true
	}

	bead, err := s.updateBead(ctx, req.GetId(), in)
	if err != nil {
		var denial *AuthzDenial
		if errors.As(err, &denial) {
			if denial.hidden {
				return nil, status.Error(codes.NotFound, "bead not found")
			}
			return nil, status.Error(codes.PermissionDenied, denial.Error())
		}
		var refusal *operatorRefusalError
		if errors.As(err, &refusal) {
			return nil, grpcOperatorRefusal(refusal.Source)
		}
		var divert *escalationDivertError
		if errors.As(err, &divert) {
			return nil, status.Error(codes.FailedPrecondition, divert.Error())
		}
		var openPR *openPRCloseError
		if errors.As(err, &openPR) {
			return nil, status.Error(codes.FailedPrecondition, openPR.Error())
		}
		var held *lockHeldError
		if errors.As(err, &held) {
			return nil, status.Error(codes.FailedPrecondition, held.Error())
		}
		var relConflict releaseConflict
		if errors.As(err, &relConflict) {
			return nil, status.Error(codes.FailedPrecondition, relConflict.Error())
		}
		var stale *spawnLineageDigestMismatchError
		if errors.As(err, &stale) {
			return nil, status.Error(codes.FailedPrecondition, stale.Error())
		}
		if hc, ok := asHeraldryConflict(err); ok {
			return nil, status.Error(codes.AlreadyExists, hc.Error())
		}
		var ie inputError
		if errors.As(err, &ie) {
			return nil, status.Error(codes.InvalidArgument, ie.Error())
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "bead not found")
		}
		return nil, status.Errorf(codes.Internal, "%v", err)
	}

	return &beadsv1.UpdateBeadResponse{Bead: beadToProto(bead)}, nil
}

// CloseBead marks a bead as closed.
func (s *BeadsServer) CloseBead(ctx context.Context, req *beadsv1.CloseBeadRequest) (*beadsv1.CloseBeadResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}

	// §C2 (ka-0000006767): enforce project scope + write_policy (which also
	// checks fleet-token type/op scopes) before mutating. gRPC previously
	// skipped these checks entirely — HTTP handleCloseBead did enforce them.
	// authorizeWriteForBead also runs the ownership gate (ka-0000001016).
	existing, err := s.store.GetBead(ctx, req.GetId())
	if err != nil {
		return nil, storeError(err, "bead")
	}
	if existing == nil {
		return nil, status.Error(codes.NotFound, "bead not found")
	}
	// Each refusal below answers a bead the caller cannot read as missing.
	if !beadMatchesTokenProject(ctx, existing.Labels) {
		return nil, s.refusedGRPC(ctx, existing, status.Error(codes.PermissionDenied, "token not authorized for this bead's project"))
	}
	// Escalation resolve-surface guard (ka-0000005516): divert a generic close
	// of an OPEN escalation to its dedicated approve/reject/result verbs so
	// operation_spec replay and close-on-escalation wrapper release stay
	// correct. Mirrors handleCloseBead; see escalationBlocksGenericClose.
	if escalationBlocksGenericClose(existing) {
		return nil, status.Errorf(codes.FailedPrecondition, escalationResolveHint, req.GetId())
	}
	if denial := s.authorizeWriteForBead(ctx, "close", existing); denial != nil {
		return nil, s.refusedGRPC(ctx, existing, status.Error(codes.PermissionDenied, denial.Error()))
	}
	if denial := commonsMessageLockDenial(ctx, "close", "close it", existing); denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Error())
	}
	// gRPC close carries no fields, so it cannot stamp control_acted
	// (mail_control.go).
	if denial := s.mailControlCloseDenial(ctx, "close", existing, nil); denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Error())
	}

	// Idempotent close: if the bead is already closed, return it as-is
	// without re-stamping closed_at/closed_by or emitting a second
	// beads.bead.closed event. Mirrors handleCloseBead — see ka-0000000961.
	if existing.Status == model.StatusClosed {
		return &beadsv1.CloseBeadResponse{Bead: beadToProto(existing)}, nil
	}
	// Open-PR close guard (gd-lIKFejVISw). gRPC carries no force flag, so an
	// operator override goes through HTTP (`kd close --force`).
	if refusal := s.openPRCloseRefusal(ctx, existing); refusal != nil {
		return nil, status.Error(codes.FailedPrecondition, refusal.Error())
	}
	if err := openReleaseCloseError(existing, nil); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	// SECURITY (AUDIT §C3): ignore req.ClosedBy — derive from auth identity.
	closedBy := actorFromContext(ctx)

	if err := s.stampImpersonatedByOn(ctx, req.GetId()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to stamp impersonated_by: %v", err)
	}
	bead, err := s.store.CloseBead(ctx, req.GetId(), closedBy)
	if err != nil {
		return nil, storeError(err, "bead")
	}
	if bead == nil {
		return nil, status.Error(codes.NotFound, "bead not found")
	}

	// Taint propagation on write (ADR docs/adr/taints.md §4.2 row 4).
	if err := s.propagateWriteTaint(ctx, bead.ID, closedBy); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to propagate write taint: %v", err)
	}

	s.recordAndPublish(ctx, events.TopicBeadClosed, bead.ID, closedBy, events.BeadClosed{
		Bead:     bead,
		ClosedBy: closedBy,
	})
	s.recordMutation(ctx, bead.ID, closedBy, "status", string(existing.Status), string(bead.Status), model.MutationTypeUpdate)
	s.recordCloseClient(ctx, bead.ID, closedBy)

	if bead.Type == model.TypeAdvice {
		s.recordAndPublish(ctx, events.TopicAdviceDeleted, bead.ID, closedBy, events.AdviceDeleted{BeadID: bead.ID, Fleet: bead.Fleet})
	}

	// Auto-close cascade (ka-0000001587): drain a completed parent epic /
	// release a close-on-escalation wrapper. Best-effort; never blocks this close.
	s.afterBeadClosed(ctx, bead)

	return &beadsv1.CloseBeadResponse{Bead: beadToProto(bead)}, nil
}

// DeleteBead removes a bead by ID.
func (s *BeadsServer) DeleteBead(ctx context.Context, req *beadsv1.DeleteBeadRequest) (*beadsv1.DeleteBeadResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}

	// §C2 (ka-0000006767): enforce project scope + write_policy (which also
	// checks fleet-token type/op scopes) before mutating. gRPC previously
	// skipped these checks entirely.
	// authorizeWriteForBead also runs the ownership gate (ka-0000001016).
	existing, err := s.store.GetBead(ctx, req.GetId())
	if err != nil {
		return nil, storeError(err, "bead")
	}
	if existing == nil {
		return nil, status.Error(codes.NotFound, "bead not found")
	}
	// Each refusal below answers a bead the caller cannot read as missing.
	if !beadMatchesTokenProject(ctx, existing.Labels) {
		return nil, s.refusedGRPC(ctx, existing, status.Error(codes.PermissionDenied, "token not authorized for this bead's project"))
	}
	if denial := s.authorizeWriteForBead(ctx, "delete", existing); denial != nil {
		return nil, s.refusedGRPC(ctx, existing, status.Error(codes.PermissionDenied, denial.Error()))
	}
	if denial := commonsMessageLockDenial(ctx, "delete", "delete it", existing); denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Error())
	}
	if denial := s.mailControlCloseDenial(ctx, "delete", existing, nil); denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Error())
	}

	if err := s.store.DeleteBead(ctx, req.GetId()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "bead not found")
		}
		var nfErr interface{ NotFound() bool }
		if errors.As(err, &nfErr) && nfErr.NotFound() {
			return nil, status.Error(codes.NotFound, "bead not found")
		}
		return nil, status.Errorf(codes.Internal, "failed to delete bead: %v", err)
	}

	s.recordAndPublish(ctx, events.TopicBeadDeleted, req.GetId(), "", events.BeadDeleted{BeadID: req.GetId(), Fleet: FleetFromContext(ctx)})
	s.recordDelete(ctx, req.GetId(), actorFromContext(ctx))

	return &beadsv1.DeleteBeadResponse{}, nil
}

// reportTypeFromLabels extracts the report template name from labels.
// E.g., "report:summary" → "summary". Returns "" if no report label found.
func reportTypeFromLabels(labels []string) string {
	for _, l := range labels {
		if strings.HasPrefix(l, "report:") {
			return strings.TrimPrefix(l, "report:")
		}
	}
	return ""
}
