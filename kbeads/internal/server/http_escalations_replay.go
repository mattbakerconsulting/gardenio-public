package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// replayEscalation attempts to replay the denied operation stored in the
// escalation bead's operation_spec field. Uses a machine-write context
// (withMachineWrite) to bypass write policy checks — the operator's approval
// already authorized this exact operation, so a raw static-token bypass would
// be gd-ZjiSd9Kzio's bug again. Records the result on the escalation bead and closes it.
// Returns a map with replay details, or nil if no replay was attempted.
func (s *BeadsServer) replayEscalation(ctx context.Context, escalation *model.Bead, approvedBy string, grantUntil *time.Time) map[string]any {
	if len(escalation.Fields) == 0 {
		return nil
	}

	var fields struct {
		OperationType   string          `json:"operation_type"`
		OperationSpec   json.RawMessage `json:"operation_spec"`
		TargetBeadID    string          `json:"target_bead_id"`
		RequestingAgent string          `json:"requesting_agent"`
		SpawnRole       string          `json:"spawn_role"`
	}
	if err := json.Unmarshal(escalation.Fields, &fields); err != nil {
		return nil
	}
	if fields.OperationType == "" || len(fields.OperationSpec) == 0 {
		return nil
	}

	// Idempotency guard (Phase 2 2-replica spike, gd-XxrObXvc8r): claim
	// exclusive right to replay THIS escalation before doing anything with
	// side effects. The prior guard (bead.Status == StatusOpen in the caller)
	// covers a single sequential approve call but not two concurrent ones —
	// double-click, client retry, or two requests landing on two different
	// replicas could each reach here and execute the approved operation
	// twice. ClaimEscalationReplay is a single guarded UPDATE, atomic across
	// any number of concurrent callers/replicas.
	claimed, err := s.store.ClaimEscalationReplay(ctx, escalation.ID)
	if err != nil {
		slog.Warn("escalation replay: failed to claim replay lock", "escalation", escalation.ID, "error", err)
		return nil
	}
	if !claimed {
		slog.Info("escalation replay: skipped, already claimed by a concurrent replay",
			"escalation", escalation.ID)
		return map[string]any{
			"operation_type": fields.OperationType,
			"replayed":       false,
			"status":         "skipped",
			"reason":         "already claimed by a concurrent replay",
		}
	}

	// The operator's approval (handleApproveEscalation already required an
	// OIDC-authenticated escalationApprover before calling here) IS the
	// authorization for the replayed write — the approver need not also
	// personally hold whatever role/scope the target write itself requires
	// (gd-ZjiSd9Kzio: withMachineWrite makes authorizeWriteTitled bypass write
	// policy entirely for this replay, the same way withApprovedSpawnReplay
	// already does for the narrower privileged-spawn gate below). Prior to
	// gd-ZjiSd9Kzio this comment claimed a "static-token context" bypassed
	// write policy here, but ctx's auth method was always the approver's own
	// OIDC — that bypass never actually ran; the replay was really riding on
	// the approver's own OIDC role happening to satisfy the target's
	// min_role, which silently re-denied an operator-approved replay whenever
	// it didn't. withMachineWrite closes that gap.
	//
	// Stamp the machine operator so post-R2 (ka-0000005772) fail-closed
	// resolution attributes the replayed write to the approving system path,
	// not to whatever (possibly anonymous) identity the original requester had.
	// This is a machine-origin replay (the daemon re-issuing an approved op),
	// so it routes through the single allowlisted system:machine sink
	// (ka-0000006012) and stays alive once BEADS_AUTHORING_HUMAN_GATE narrows
	// the resolver override tail to that sink.
	//
	// A scoped principal's approval is NOT that authority: its replay keeps the
	// scoped write checks on (no withMachineWrite), so the write is held to the
	// same project and type rules as a direct one.
	replayCtx := WithMachineOperator(ctx)
	if !isScopedPrincipal(ctx) {
		replayCtx = withMachineWrite(replayCtx)
	}
	// An approved status→closed update closes over an open PR, as the close
	// replay below (store.CloseBead) does: the approval is the operator's
	// override of the open-PR close guard (gd-lIKFejVISw).
	replayCtx = withForceClose(replayCtx)

	// Taint attribution for replay (ADR docs/adr/taints.md §4.2 row 7): a
	// replayed create/update runs under the approver's identity above (only
	// operated_by is overridden — AuthIdentity still resolves to whoever
	// approved), but the write's inherit:write/inherit:create taint
	// propagation must attribute to the original REQUESTING agent, not the
	// approver. Scoped to create/update only, per that same ADR row; "close"
	// replay below does not go through propagateWriteTaint at all today.
	replayCtx = WithTaintAttributionAgent(replayCtx, fields.RequestingAgent)

	var (
		resultBead *model.Bead
		replayErr  error
		// grantExtra carries the "grant" case's result fields (granted
		// taints, principal, expiry) — there is no resultBead for a grant
		// replay, so these are merged into result separately below.
		grantExtra map[string]any
		// spawnTaskClaim is the outcome of the pre-claim of a replayed
		// spawn's task ("" when the replay spawned no agent with a task).
		spawnTaskClaim string
	)

	// The approved operation does not run on behalf of a requesting agent
	// that carries a restrict taint it holds no grant for (the shared check,
	// restricted_ops.go): what it asked for may have been steered by
	// untrusted input. Running no case below records the refusal.
	//
	// Only the writes this replay itself performs are checked. A merge,
	// terragrunt or mint_secret escalation is executed by the bridge from the
	// approval event, not here, so refusing it would refuse nothing and record
	// a false failure (ADR §15.7: the merge desk can still move a tenant's
	// PRs). A grant is exempt too: the approver writes it, and it is how a
	// tainted agent gets the very grant this check asks for.
	opType := fields.OperationType
	if fields.RequestingAgent != "" && replayWritesAsRequester(opType) {
		if d := s.restrictedOpDenial(replayCtx, restrictedOpRequest{Op: opEscalationReplay, Action: fields.OperationType, BeadType: model.TypeEscalation, Agent: fields.RequestingAgent}); d != nil {
			replayErr = d
			opType = ""
		}
	}

	switch opType {
	case "create":
		var in createBeadInput
		if err := json.Unmarshal(fields.OperationSpec, &in); err != nil {
			replayErr = err
			break
		}
		// Provenance stamps on a replayed agent spawn (design doc
		// gd-sGW5Y1B8sg §5): spawned_by/spawn_approval_mode/spawn_approved_by
		// are computed here, server-side, from the approving escalation —
		// never trusted from the originally-denied create body, which predates
		// approval and could not have set them meaningfully anyway. The
		// controller mirrors all three onto the pod as gardenio.io/spawned-by,
		// gardenio.io/spawn-approval-mode, gardenio.io/spawn-approved-by.
		if in.Type == "agent" {
			// gd-UQtYJZ6RgQ: operation_spec is a generic bead field, patchable
			// between filing and approval via PATCH /v1/beads/{id}. spawn_role
			// is what the approver actually saw in the inbox (stamped at
			// denial time — see AuthzDenial.SpawnRole in write_authz.go). If
			// the two have diverged, the operator approved a role that is no
			// longer the role about to be spawned, so the replay must refuse
			// rather than trust operation_spec.fields.role as-is.
			role := roleFromFields(in.Fields)
			if strings.TrimSpace(role) == "" {
				replayErr = errors.New("replay refused: agent create has no resolvable role")
				break
			}
			if fields.SpawnRole != "" && role != fields.SpawnRole {
				replayErr = fmt.Errorf("replay refused: operation_spec role %q no longer matches the escalation's approved spawn_role %q (operation_spec may have been patched after filing)", role, fields.SpawnRole)
				break
			}
			in.Fields = stampSpawnProvenance(in.Fields, escalation.ID, approvedBy)
			// The replay runs under a non-OIDC identity, so without this
			// marker createBead's authorizeAgentSpawn would re-enter the
			// very privileged-role gate whose denial created this
			// escalation, and the approval would be recorded then discarded
			// (gd-zErNIecGIc, observed on the first real privileged spawn).
			replayCtx = withApprovedSpawnReplay(replayCtx, escalation.ID, approvedBy)
		}
		// §9 of the dedup-ack design (epic ka-0000004599): the replay path
		// opts out of the dedup-ack gate. The operator just approved this
		// grant; forcing a re-ack of the open list after approval is bad UX,
		// and the operator's approval already constitutes the "look". We mark
		// the context rather than set in.Force so the pre-existing ack-only
		// decision guard behavior is untouched — this bypass is scoped to the
		// dedup gate alone and is WARN-logged in runDedupAckGate.
		resultBead, replayErr = s.createBead(withDedupAckReplayBypass(replayCtx), in)
		// The spawn client's follow-ups on the task the agent was spawned
		// for (the assigned dep and the pre-claim), which a denied create
		// never ran (escalation_spawn_task_claim.go, gd-G5PmhWXsF9).
		if replayErr == nil && in.Type == "agent" {
			spawnTaskClaim = s.claimReplayedSpawnTask(ctx, escalation, resultBead)
		}

	case "update":
		if fields.TargetBeadID == "" {
			replayErr = errors.New("target_bead_id missing for update replay")
			break
		}
		var in updateBeadInput
		if err := json.Unmarshal(fields.OperationSpec, &in); err != nil {
			replayErr = err
			break
		}
		if in.DueAt != nil {
			in.dueAtSet = true
		}
		if in.DeferUntil != nil {
			in.deferUntilSet = true
		}
		if in.Labels != nil {
			in.labelsSet = true
		}
		// An update that sets an agent bead's role was denied by the same
		// privileged-role gate as a spawn (authorizeAgentRoleUpdate). The
		// approval is the authorization for exactly the role the approver saw
		// and handleApproveEscalation vetted, both derived from operation_spec
		// (escalationSpawnTarget), never the requester-writable spawn_role
		// (gd-UQtYJZ6RgQ, gd-Yhbc5wdjwj, gd-4gdUwaWr6i).
		if role, isSpawn, rerr := escalationSpawnTarget(parseSpawnEscalationFields(escalation)); isSpawn {
			if rerr != nil {
				replayErr = fmt.Errorf("replay refused: %w", rerr)
				break
			}
			if fields.SpawnRole != "" && role != fields.SpawnRole {
				replayErr = fmt.Errorf("replay refused: operation_spec role %q no longer matches the escalation's approved spawn_role %q (operation_spec may have been patched after filing)", role, fields.SpawnRole)
				break
			}
			replayCtx = withApprovedSpawnReplay(replayCtx, escalation.ID, approvedBy)
		}
		resultBead, replayErr = s.updateBead(replayCtx, fields.TargetBeadID, in)

	case "close":
		if fields.TargetBeadID == "" {
			replayErr = errors.New("target_bead_id missing for close replay")
			break
		}
		var body map[string]any
		_ = json.Unmarshal(fields.OperationSpec, &body)
		closedBy, _ := body["closed_by"].(string)
		if closedBy == "" {
			closedBy = approvedBy
		}
		// The close writes straight to the store, so a scoped approver's
		// replay re-checks the target here rather than relying on a write path.
		if isScopedPrincipal(replayCtx) {
			if denied, derr := s.scopedReplayDenial(replayCtx, escalation); derr != nil || denied != "" {
				replayErr = errors.New("replay refused: scoped approver may not close this bead")
				break
			}
		}
		resultBead, replayErr = s.store.CloseBead(replayCtx, fields.TargetBeadID, closedBy)
		if replayErr == nil && resultBead != nil {
			s.recordAndPublish(replayCtx, events.TopicBeadClosed, resultBead.ID, closedBy, events.BeadClosed{
				Bead:     resultBead,
				ClosedBy: closedBy,
			})
		}

	case "grant":
		// ADR docs/adr/taints.md §9.4: approving a taint grant escalation
		// writes a taint_grants row rather than replaying any write against
		// TargetBeadID (there usually is none — a direct kd taint
		// grant-request never denied a write in the first place).
		var spec grantEscalationSpec
		if err := json.Unmarshal(fields.OperationSpec, &spec); err != nil {
			replayErr = err
			break
		}
		if len(spec.TaintNames) == 0 || spec.PrincipalKind == "" || spec.Principal == "" {
			replayErr = errors.New("grant escalation spec missing taint_names or principal")
			break
		}
		if err := s.checkGrantReplay(ctx, escalation, spec); err != nil {
			replayErr = err
			break
		}

		// granted_by is the APPROVER, resolved from replayCtx — which still
		// carries the approver's AuthIdentity (WithMachineOperator above only
		// overrides operated_by, per its own doc comment), matching the same
		// server-derived-actor invariant handleGrantTaint enforces.
		granterID, gok := s.granterOperatorID(replayCtx)
		if !gok {
			replayErr = errors.New("approver identity cannot be resolved to an operator")
			break
		}

		expiresAt := grantUntil
		if expiresAt == nil {
			t := time.Now().UTC().Add(7 * 24 * time.Hour)
			expiresAt = &t
		} else {
			t := expiresAt.UTC()
			expiresAt = &t
		}

		granted, gerr := s.grantTaintsForEscalation(ctx, spec.TaintNames, spec.PrincipalKind, spec.Principal, granterID, expiresAt)
		if gerr != nil {
			replayErr = gerr
			break
		}
		grantExtra = map[string]any{
			"granted_taints": granted,
			"principal":      spec.PrincipalKind + ":" + spec.Principal,
			"expires_at":     expiresAt.Format(time.RFC3339),
		}

	case "":
		// Refused by the shared check above; replayErr records why.

	default:
		slog.Warn("unknown operation type for escalation replay",
			"escalation", escalation.ID, "operation_type", fields.OperationType)
		return nil
	}

	// Record result on the escalation bead.
	result := map[string]any{
		"operation_type": fields.OperationType,
		"replayed":       true,
	}

	if replayErr != nil {
		slog.Warn("escalation replay failed",
			"escalation", escalation.ID, "error", replayErr)
		result["status"] = "failure"
		result["error"] = replayErr.Error()

		_ = s.mergeBeadFields(ctx, escalation.ID, map[string]any{
			"result_status": "failure",
			"result":        map[string]string{"error": replayErr.Error()},
			"executor":      "auto-replay",
		})

		// Release the claim on failure so a retry (another approve call, or
		// an operator-triggered re-approve) can attempt the replay again —
		// preserves the pre-existing retry-after-failure behavior. On
		// success the claim is deliberately left set: the escalation is
		// closed below, so nothing will legitimately try to replay it again.
		if err := s.store.ReleaseEscalationReplayClaim(ctx, escalation.ID); err != nil {
			slog.Warn("escalation replay: failed to release replay claim after failure",
				"escalation", escalation.ID, "error", err)
		}
	} else {
		result["status"] = "success"
		if resultBead != nil {
			result["bead_id"] = resultBead.ID
		}
		// grantExtra is only populated by the "grant" case — there is no
		// resultBead for a grant replay, so its fields (granted taints,
		// principal, expiry) are merged in here instead.
		for k, v := range grantExtra {
			result[k] = v
		}
		if spawnTaskClaim != "" {
			result["task_claim"] = spawnTaskClaim
		}

		_ = s.mergeBeadFields(ctx, escalation.ID, map[string]any{
			"result_status":    "success",
			"executor":         "auto-replay",
			"execution_target": "server",
		})
		if resultBead != nil {
			_ = s.mergeBeadFields(ctx, escalation.ID, map[string]any{
				"result": map[string]string{"bead_id": resultBead.ID},
			})
		}
		if grantExtra != nil {
			_ = s.mergeBeadFields(ctx, escalation.ID, map[string]any{
				"result": grantExtra,
			})
		}

		// Close the escalation on success.
		if closed, err := s.store.CloseBead(ctx, escalation.ID, "auto-replay"); err == nil && closed != nil {
			s.recordAndPublish(ctx, events.TopicBeadClosed, closed.ID, "auto-replay", events.BeadClosed{
				Bead:     closed,
				ClosedBy: "auto-replay",
			})
			// Auto-close cascade (ka-0000001587): an executed escalation
			// releases its close-on-escalation-execute wrapper tasks.
			s.afterBeadClosed(ctx, closed)
		}
	}

	return result
}

// replayWritesAsRequester reports whether replayEscalation performs opType's
// write itself on the requesting agent's behalf (create, update, close), so
// the restrict-taint check applies to it.
func replayWritesAsRequester(opType string) bool {
	switch opType {
	case "create", "update", "close":
		return true
	}
	return false
}

// stampSpawnProvenance returns fields with spawned_by, spawn_approval_mode and
// spawn_approved_by set (overwriting any prior value), for a replayed agent
// create whose escalation just got approved. escalationID becomes spawned_by
// so an agent bead traces to the escalation that authorized it rather than to
// whatever spawned_by the original denied request happened to carry; the
// other two are derived from approvedBy via spawnApprovalMode (design doc
// gd-sGW5Y1B8sg §5).
func stampSpawnProvenance(fields json.RawMessage, escalationID, approvedBy string) json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(fields) > 0 {
		_ = json.Unmarshal(fields, &m)
	}
	setJSONStringField(m, "spawned_by", escalationID)
	setJSONStringField(m, "spawn_approval_mode", spawnApprovalMode(approvedBy))
	setJSONStringField(m, "spawn_approved_by", approvedBy)
	out, err := json.Marshal(m)
	if err != nil {
		return fields
	}
	return out
}

// setJSONStringField sets key to value (JSON-encoded) in m, a decoded bead
// Fields blob.
func setJSONStringField(m map[string]json.RawMessage, key, value string) {
	b, err := json.Marshal(value)
	if err != nil {
		return
	}
	m[key] = b
}

// spawnApprovalMode reports whether an escalation approval came from the
// policy evaluator ("auto", approved_by = system:spawn-policy) or a human
// ("human", approved_by = an oidc: identity) — the two-cases table in design
// doc gd-sGW5Y1B8sg §"The two cases, and how to tell them apart". The
// evaluator's system:spawn-policy identity is the only sentinel this pathway
// ever writes to approved_by, so a "system:" prefix is unambiguous auto; the
// identity-derived / OIDC-only correction to who else may approve is a
// separate task (gd-731gBeGgl7).
func spawnApprovalMode(approvedBy string) string {
	if strings.HasPrefix(approvedBy, "system:") {
		return "auto"
	}
	return "human"
}
