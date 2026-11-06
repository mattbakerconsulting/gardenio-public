package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// The named steps of updateBead (beads.go), in the order it runs them. Each
// step reads and writes one beadUpdate, so the order of checks, writes and
// side effects is exactly the order updateBead lists them in.

// beadUpdate is one updateBead call in flight: the patch, the bead it is
// applied to, what was true of the bead before the patch, what authorization
// decided, and the change set the patch produced.
type beadUpdate struct {
	in   *updateBeadInput
	bead *model.Bead

	// Before the patch. Spawn-lineage seal bookkeeping (spawn_lineage_seal.go):
	// whether the bead was validly sealed BEFORE this patch, and the transient
	// reseal control key (consumed here, never stored).
	sealedBeforePatch    bool
	priorStatus          model.Status
	wasClosed            bool
	wasEnabled           bool
	resealRequested      bool
	resealExpectedDigest string
	priorAssignee        string
	priorLabels          []string

	// What authorization decided.
	writeAction string
	// reactionsOnly: neither a reaction nor a join authors the bead, so no
	// dibs flip.
	reactionsOnly bool
	// shortTitleFill: the ownership gate passed only through the short-title
	// fill exception, so the write must re-check the stored short_title
	// (short_title_fill_authz.go, gd-JOOXA7UqLh).
	shortTitleFill bool
	// lockMove re-checks a lock bead's holder under a row lock in the write
	// transaction (lock_bead.go).
	lockMove bool
	// selfIdle: a gardener idling its own trigger (gardener_self_idle.go);
	// selfIdleSeal is the seal the trigger held before the patch, carried over
	// the patched content.
	selfIdle     bool
	selfIdleSeal *spawnLineageSeal
	// stewardRearm: the home steward re-arming an engine-closed plan
	// gardener, which keeps its seal (spawn_lineage_rearm.go).
	stewardRearm bool
	// releaseCut: the patch cuts an open release, re-checked under
	// releaseWriteMu before the write (releases_open.go).
	releaseCut releaseCut

	changes   map[string]any
	oldValues map[string]any
	// fieldChanges holds the per-key transitions of the extensible Fields blob
	// (bare key -> new value), overlaid onto the PUBLISHED Changes map only so
	// declarative trigger watchers can match a single custom-field rotation.
	// Kept separate from `changes` so mutation-history still records one
	// canonical "fields" diff, not a row per key (ka-0000004185).
	fieldChanges map[string]any
}

// newBeadUpdate snapshots what updateBead needs to know about the bead before
// the patch, and takes the reseal control key out of the patch.
func newBeadUpdate(bead *model.Bead, in *updateBeadInput) *beadUpdate {
	u := &beadUpdate{
		in:                in,
		bead:              bead,
		sealedBeforePatch: sealedSpawnAuthor(bead) != "",
		priorStatus:       bead.Status,
		wasClosed:         bead.Status == model.StatusClosed,
		wasEnabled:        effectiveGardenerEnabled(bead.Type, bead.Fields),
		changes:           make(map[string]any),
		oldValues:         make(map[string]any),
	}
	in.Fields, u.resealRequested, u.resealExpectedDigest = takeResealFlag(in.Fields)
	return u
}

// scrubProse scrubs bare credential SHAPES from the patch's prose and
// normalizes its short title and arms.
func (in *updateBeadInput) scrubProse() error {
	if in.Title != nil {
		redacted := redactCredShapes(*in.Title)
		in.Title = &redacted
	}
	if in.ShortTitle != nil {
		normalized := model.NormalizeShortTitle(redactCredShapes(*in.ShortTitle))
		in.ShortTitle = &normalized
	}
	if in.Heraldry != nil {
		canonical, err := normalizeHeraldry(*in.Heraldry)
		if err != nil {
			return err
		}
		in.Heraldry = &canonical
	}
	if in.Description != nil {
		redacted := redactCredShapes(*in.Description)
		in.Description = &redacted
	}
	if in.Notes != nil {
		redacted := redactCredShapes(*in.Notes)
		in.Notes = &redacted
	}
	return nil
}

// authorizeUpdate runs every gate a patch must pass before it is applied.
func (s *BeadsServer) authorizeUpdate(ctx context.Context, u *beadUpdate) error {
	if err := s.authorizeUpdatePolicy(ctx, u); err != nil {
		return err
	}
	if err := s.authorizeUpdateScope(ctx, u); err != nil {
		return err
	}
	if err := s.authorizeUpdateFieldGates(ctx, u); err != nil {
		return err
	}
	if err := s.authorizeUpdateOwnership(ctx, u); err != nil {
		return err
	}
	// Taint write gate (ADR docs/adr/taints.md §9.4, gd-KT7mKtvYwt): narrows
	// further than the write-policy/ownership checks above, regardless of
	// which branch allowed the write — a caller with no grant for a taint
	// this bead carries may not patch it even if it owns the bead outright.
	// Exception: an assignee-only patch is the propagation mechanism itself
	// (patchOnlyChangesAssignee's doc comment) and is never gated here.
	if !u.in.patchOnlyChangesAssignee() {
		if denial := s.authorizeWriteTaintGate(ctx, "update", u.bead); denial != nil {
			return denial
		}
	}
	return nil
}

// authorizeUpdatePolicy enforces the token project scope and the write policy.
func (s *BeadsServer) authorizeUpdatePolicy(ctx context.Context, u *beadUpdate) error {
	in, bead := u.in, u.bead
	// Enforce token project scope.
	if !beadMatchesTokenProject(ctx, bead.Labels) {
		return &AuthzDenial{
			Action:   "update",
			BeadType: string(bead.Type),
			Message:  "token not authorized for this bead's project",
		}
	}

	// Enforce write policy authorization. Project-scope / type-scope checks
	// always apply. The ownership gate is skipped when the patch ONLY touches
	// service-managed observability fields for this bead type (e.g. the
	// jira-bridge stamping `last_checked` on a long-lived trigger bead it did
	// not create). ka-0000003039.
	//
	// type:team is a config-kind bead, so authorizeOwnership below is always a
	// no-op for it (B6) and this min_role check (gardenio-admin) is normally
	// the ENTIRE gate. authorizeTeamMembershipBypass carves out one narrow
	// exception, per ADR docs/adr/taints.md §9.1: a caller already listed in
	// the team's own `owners` field may still patch members/owners (and only
	// those keys) despite lacking gardenio-admin.
	//
	// The bypass may ONLY override the min_role denial (denial.RequiredRole
	// set — the OIDC branch of authorizeWriteTitled denying an insufficiently-
	// privileged human). It must never override a non-OIDC caller's
	// checkTokenScopes denial or an OIDC caller's project-scope denial —
	// neither of those sets RequiredRole. Operator review on PR #145: a
	// fleet token scoped to disallow this exact write, paired with a
	// caller-supplied X-Operated-By naming a team owner, was overriding its
	// own scope restriction and rewriting arbitrary teams' owners.
	//
	// A scoped principal's reactions-only patch on a message is checked as
	// scopedActionReact rather than "update" (message is create-only for it
	// otherwise), may touch only its own reactions, and skips ownership below:
	// reacting to someone else's message is the point. A members-only patch
	// of a channel is scopedActionJoin the same way (channel_membership.go).
	u.writeAction = scopedUpdateAction(ctx, bead, in)
	// Neither a reaction nor a join authors the bead: no dibs flip below.
	u.reactionsOnly = messageReactionsOnly(bead, in) || channelMembershipOnly(bead, in)
	denial := s.authorizeWriteTitled(ctx, u.writeAction, bead.Type, bead.Labels, bead.Title)
	if denial == nil && u.writeAction == scopedActionReact {
		denial = scopedReactionDenial(ctx, bead, in.Fields)
	}
	if denial == nil && u.writeAction == scopedActionJoin {
		denial = scopedMembershipDenial(ctx, bead, in.Fields)
	}
	if denial == nil && u.writeAction == scopedActionEditHolds {
		denial = scopedHoldsDenial(bead, in.Fields)
	}
	if denial == nil && in.Title != nil && *in.Title != bead.Title {
		// A rename must satisfy the title-scoped machine grant too, or a
		// permitted config could be renamed into claude-hooks.
		denial = s.authorizeWriteTitled(ctx, "update", bead.Type, bead.Labels, *in.Title)
	}
	if denial != nil {
		if denial.RequiredRole == "" || !s.authorizeTeamMembershipBypass(ctx, bead, in) {
			return denial
		}
	}
	return nil
}

// authorizeUpdateScope keeps a scoped principal, a tenant agent or a runner
// inside its projects, and refuses case-variant field keys.
func (s *BeadsServer) authorizeUpdateScope(ctx context.Context, u *beadUpdate) error {
	in, bead := u.in, u.bead
	// A scoped principal may not move a bead out of its projects: the new label
	// set and any fields.project must stay inside them (the current labels were
	// checked above).
	if in.labelsSet {
		if denial := scopedWriteDenial(ctx, "update", bead.Type, in.Labels); denial != nil {
			return denial
		}
		if denial := s.tenantAgentLabelDenial(ctx, "update", bead.Type, in.Labels); denial != nil {
			return denial
		}
		if denial := s.wishHeadDenial(ctx, "update", bead.Type, changedWishHeadLabels(bead.Labels, in.Labels), nil, nil); denial != nil {
			return denial
		}
		// Nor into a frozen land (land_frozen.go).
		if denial := frozenLandDenial(ctx, s.store, "update", bead.Type, in.Labels); denial != nil {
			return denial
		}
		// Nor may a frozen land's agent drop its agent bead's land label.
		if denial := frozenAgentLabelDenial(ctx, s.store, bead, in.Labels); denial != nil {
			return denial
		}
		// Nor may any tenant agent strip a project label (gd-U9AV7jG8hF).
		if denial := s.tenantAgentProjectLabelDenial(ctx, bead.Type, bead.Labels, in.Labels); denial != nil {
			return denial
		}
		// Nor may the home steward take advice out of its project.
		if denial := s.stewardAdviceLabelsDenial(ctx, bead, in.Labels); denial != nil {
			return denial
		}
	}
	// Nor give advice a hook (escalation_steward_advice.go).
	if denial := s.stewardAdviceHookDenial(ctx, bead, in.Fields); denial != nil {
		return denial
	}
	// Nor may a runner touch one whose fields.project is outside its allowlist
	// (the current labels were checked above), or move one into such a project
	// by label or fields.project (runner_auth.go).
	if denial := runnerProjectDenial(ctx, "update", nil, bead.Fields); denial != nil {
		return denial
	}
	if in.labelsSet || len(in.Fields) > 0 {
		if denial := runnerProjectDenial(ctx, "update", in.Labels, in.Fields); denial != nil {
			return denial
		}
	}
	// Nor stamp its id onto an agent it does not run (runner_auth.go).
	if denial := runnerRuntimeIDDenial(ctx, "update", in.Fields, bead); denial != nil {
		return denial
	}
	// Refuse case-variant keys before any field guard reads the patch
	// (field_key_case.go).
	if len(in.Fields) > 0 {
		var patch map[string]any
		if json.Unmarshal(in.Fields, &patch) == nil {
			if err := s.fieldKeyCaseError(ctx, bead, patch); err != nil {
				return inputError("invalid fields: " + err.Error())
			}
		}
	}
	if denial := scopedFieldsProjectDenial(ctx, "update", bead.Type, in.Fields); denial != nil {
		return denial
	}
	return nil
}

// authorizeUpdateFieldGates runs the field-level gates: each guards the keys
// of one kind of bead that only some callers may write.
func (s *BeadsServer) authorizeUpdateFieldGates(ctx context.Context, u *beadUpdate) error {
	in, bead := u.in, u.bead
	// Field-level gate on type:user identity fields (gd-uAvleAZUvV). Runs even
	// when the machine allow-list admitted the (update, user) pair above.
	if denial := s.authorizeUserIdentityFields(ctx, "update", bead.Type, in.Fields, bead.Fields); denial != nil {
		return denial
	}
	// Setting or changing `role` on an existing agent bead clears the same
	// privileged-role gate as a spawn (gd-Yhbc5wdjwj): the controller derives the
	// pod's ServiceAccount from the role on the next (re)start, so an update
	// is a spawn by another route. Runs even when the machine allow-list
	// admitted the (update, agent) pair above.
	if newRole, changed := agentRoleFieldChange(in.Fields, bead.Fields); bead.Type == "agent" && changed {
		if d := s.restrictTaintOpDenial(ctx, restrictedOpRequest{Op: opSpawn, Action: "update", BeadType: typeAgent, Role: newRole, Project: projectFromFields(bead.Fields)}); d != nil {
			return d
		}
	}
	if err := authorizeAgentRoleUpdate(ctx, s.store, bead, in.Fields); err != nil {
		return err
	}
	if bead.Type == "agent" {
		in.Fields = canonicalizeRoleField(in.Fields)
	}
	// Field-level gate on the pod's Kubernetes identity (gd-Gpvi7wld87).
	if denial := s.authorizeAgentIdentityFields(ctx, "update", bead.Type, in.Fields, bead.Fields); denial != nil {
		return denial
	}
	// An agent's thread and trigger links decide which plan it may write
	// (plan_scope_authz.go, gd-Mk64Bko3XO); an agent token may not move them.
	if denial := s.planScopeBindingPatchDenial(ctx, "update", bead, in.Fields); denial != nil {
		return denial
	}
	// Field-level gate on a channel's Slack binding (gd-ecFKkjtdvS).
	if denial := s.authorizeChannelBindingFields(ctx, "update", bead.Type, in.Fields, bead.Fields); denial != nil {
		return denial
	}
	// Nor a wish head's promotion links (wish_head_authz.go).
	if denial := s.wishHeadDenial(ctx, "update", bead.Type, nil, in.Fields, bead.Fields); denial != nil {
		return denial
	}
	// The Lands kill switches (land_switch_authz.go): a land's kind and freeze,
	// and a status change or rename of a project bead or the lands-pause gate.
	// Run even when the machine allow-list admitted the (update, project) pair.
	if denial := s.authorizeLandSwitchFields(ctx, "update", bead.Type, in.Fields, bead.Fields); denial != nil {
		return denial
	}
	if denial := s.authorizeKillSwitchLifecycle(ctx, "update", bead, in.Status, in.Title); denial != nil {
		return denial
	}
	if denial := spawnLineageSealPatchDenial("update", bead.Type, in.Fields, bead.Fields); denial != nil {
		return denial
	}
	if denial := impersonatedByPatchDenial(ctx, "update", bead.Type, in.Fields, bead.Fields); denial != nil {
		return denial
	}
	if denial := commonsOriginPatchDenial(ctx, bead, in.Fields); denial != nil {
		return denial
	}
	if denial := messageAuthorPatchDenial(ctx, bead, in.Fields); denial != nil {
		return denial
	}
	if denial := messageGitHubAuthorPatchDenial("update", bead, in.Fields); denial != nil {
		return denial
	}
	if denial := commonsMessageEditDenial(ctx, bead, in); denial != nil {
		return denial
	}
	if denial := autonomousCounterPatchDenial(ctx, bead.Type, in.Fields, bead.Fields); denial != nil {
		return denial
	}
	if denial := agentParkFieldPatchDenial(ctx, "update", bead.Type, in.Fields, bead.Fields); denial != nil {
		return denial
	}
	if denial := frozenAgentPatchDenial(ctx, s.store, bead, in.Fields); denial != nil {
		return denial
	}
	// A caller that may not resolve a decision may not write its answer fields
	// through update either, even on a decision it filed (gd-Du6wHclx4g).
	if denial := s.authorizeDecisionAnswerFields(ctx, "update", bead, decisionAnswerKeysInPatch(in.Fields)...); denial != nil {
		return denial
	}
	return nil
}

// authorizeUpdateOwnership runs the per-bead ownership gate, or the one
// narrow exception that admits this patch without it.
func (s *BeadsServer) authorizeUpdateOwnership(ctx context.Context, u *beadUpdate) error {
	in, bead := u.in, u.bead
	switch {
	case u.writeAction == scopedActionReact:
		// Own reactions only, checked by scopedReactionDenial above.
	case u.writeAction == scopedActionJoin:
		// Joining someone else's channel is the point; scopedMembershipDenial
		// above decided whose entries the caller may change.
	case u.writeAction == scopedActionEditHolds:
		// The steward's notes are not the person's; scopedHoldsDenial above
		// confined the patch to the Holds section.
	case patchOnlyChangesServiceManagedFields(bead.Type, in, bead.Fields):
		if denial := s.authorizeServiceFieldBypass(ctx, "update", bead); denial != nil {
			// Bypass-eligible patch, but the writer-fleet policy refused the
			// bypass (foreign fleet stamping a shared trigger/schedule bead —
			// the ka-0000006237 cross-env starvation class) and the fallback
			// ownership gate denied too.
			return denial
		}
	case authorizeScifApprovalStamp(ctx, bead, in):
		// acme-scif Gate-1: the approval portal is a THIRD identity, neither
		// the crew agent that filed the request nor the human approver, so it
		// owns no request bead and ownership denied every approval. Scoped to
		// the portal's server-derived auth subject + type=investigation-request
		// + the exact approval-state keys its stamp writes; `requested_by`
		// (the four-eyes input) is deliberately excluded. See
		// scif_approval_authz.go (ka-0000001628).
	case authorizeBridgeMessageStamp(ctx, bead, in):
		// The Slack bridge's handled_at and Slack-ref stamps on a message a
		// person or an agent wrote: its fleet token, type message, those
		// fields only, each set once. See bridge_message_stamp.go
		// (gd-2IEp8qx7bq).
	case s.authorizeLaptopMailDeliveryStamp(ctx, bead, in):
		// A laptop agent's runner or operator stamping delivered_at on that
		// agent's mail: mail only, delivered_at only, set once. See
		// mail_delivery_stamp.go (gd-GTMwKmHM7o).
	case s.authorizeDecisionArtifactBySeat(ctx, bead, in):
		// A later occupant of the decision's owner_seat marking the artifact
		// its seat owes as submitted: artifact_status only, "submitted" only.
		// See decision_artifact_seat.go (gd-JPfZCDbKFp).
	case s.authorizeReflagBySeat(ctx, bead, in):
		// A later occupant of an open decision's or escalation's owner_seat
		// reflagging it: priority down, reflag/pingback fields, or the
		// "escalated" label, one per patch. See reflag_seat.go (gd-Nu4lxqkWwL).
	case s.gardenerMayIdleOwnTrigger(ctx, bead, in):
		// A plan's gardener idling its own trigger until a named bead closes:
		// enabled (off only), cron, wake_on and awaiting, awaiting set. See
		// gardener_self_idle.go (gd-V68T7v5JPP).
		u.selfIdle, u.selfIdleSeal = true, validSpawnSeal(bead)
		authzOwnershipAdmitsTotal.WithLabelValues(gardenerSelfIdleGrant, authzPrincipalKind(ctx), "update", authzBeadTypeLabel(string(bead.Type))).Inc()
	case s.homeStewardMayRearm(ctx, bead, in):
		// The home steward reopening and turning on an engine-closed, sealed
		// plan gardener of a building plan in its land: status open and
		// enabled true only; the seal is kept. See spawn_lineage_rearm.go
		// (gd-miuZqFACwL).
		u.stewardRearm = true
		authzOwnershipAdmitsTotal.WithLabelValues(stewardRearmGrant, authzPrincipalKind(ctx), "update", authzBeadTypeLabel(string(bead.Type))).Inc()
	case machineMayStampFields(ctx, bead, in):
		// A machine principal stamping the bookkeeping fields its
		// FieldOwnership grant names (the controller's plan_spend_* rollup on
		// a plan epic), those fields only. See machine_grants.go
		// (gd-Hk70sS1XuS).
	case s.machineMayClaimDispatchedTask(ctx, bead, in):
		// The controller claiming an unassigned bug, feature or chore at pod
		// create for the agent dispatched onto it, status and assignee only.
		// See dispatched_task_authz.go (gd-GrJRP7kLBY).
	default:
		if denial := s.authorizeOwnership(ctx, machinePatchOwnershipAction(ctx, bead, in), bead); denial != nil {
			if !authorizeShortTitleFill(ctx, bead, in) {
				return denial
			}
			// An agent naming a bead in its project that has no short_title
			// yet: short_title only, set once, never a decision. Naming a bead
			// is not authoring it, so no dibs flip either.
			u.shortTitleFill = true
			u.reactionsOnly = true
		}
	}
	return nil
}

// dropClosedStrayWrites drops writes to transient operational fields on a
// bead that stays closed, and reports whether the patch is then empty.
//
// Closed beads ignore writes to transient operational fields. The status
// reporter keeps PATCHing agent_state/notes during the K8s pod grace
// period after gb stop has closed the bead — the agent's final state
// must win. Legitimate edits (description, title, labels, comments) are
// untouched. Mirrors the close-idempotency rule in handleCloseBead
// (ka-0000005253).
//
// EXCEPTION — a REOPEN is not a grace-period stray write (ka-0000001583):
// when the SAME patch that touches transient fields ALSO transitions the bead
// OUT of closed (status set to a non-closed value), it is a deliberate revive,
// not the status reporter's post-close echo. The plan-gardener wake
// (planagent.Engine.Wake) reopens a closed gardener and clears its terminal
// agent_state in one PATCH so the reopen re-emits AgentSpawn with a CLEARED
// state; stripping agent_state here would leave it "done", and the reconciler
// leak-closer would immediately re-reap the just-reopened bead as a
// podless_leak — the infinite reopen→reap loop that made messaging a stopped
// plan-gardener a silent no-op. Only drop transient fields when the bead STAYS
// closed through this write.
func (u *beadUpdate) dropClosedStrayWrites() (empty bool) {
	reopening := u.in.Status != nil && model.Status(*u.in.Status) != model.StatusClosed
	if u.bead.Status == model.StatusClosed && !reopening {
		u.in.dropTransientClosedFields()
		if !u.in.hasAnyChange() {
			return true
		}
	}
	return false
}

// checkUpdateCloseGuards refuses a close or a lock move the generic update
// path may not make, and notes whether the lock holder must be re-checked.
func (s *BeadsServer) checkUpdateCloseGuards(ctx context.Context, id string, u *beadUpdate) error {
	in, bead := u.in, u.bead
	// Escalation resolve-surface guard on the PATCH update path (ka-0000006558,
	// defect 2): refuse a status→closed PATCH that would terminate an OPEN
	// escalation through the generic update path, for parity with the two generic
	// close paths that already consult escalationBlocksGenericClose (gRPC
	// CloseBead, HTTP handleCloseBead). Without this, `kd update <esc> --status
	// closed` terminates an escalation while skipping operation_spec replay and
	// result_status, and — because this path only ever publishes
	// TopicBeadUpdated, never TopicBeadClosed — emits no closed event, blinding
	// every closed-event watcher (including the cross-operator notifier). Gate on
	// the ACTUAL transition INTO closed: a patch that leaves an already-closed
	// escalation closed (the idempotent case handled above) or does not touch
	// status is untouched, so the dedicated approve/reject/result handlers —
	// which set status via store.CloseBead, not this path — stay unaffected.
	if in.Status != nil && model.Status(*in.Status) == model.StatusClosed &&
		escalationBlocksGenericClose(bead) {
		return &escalationDivertError{id: id}
	}
	// Open-PR close guard (gd-lIKFejVISw), for parity with the two generic
	// close paths: `kd update <task> --status closed` must not close a task
	// whose PR is still open either.
	if in.Status != nil && model.Status(*in.Status) == model.StatusClosed {
		if refusal := s.openPRCloseRefusal(ctx, bead); refusal != nil {
			return refusal
		}
		if err := openReleaseCloseError(bead, in.Fields); err != nil {
			return err
		}
	}
	// A lock bead's live holder may not be replaced or cleared by another run
	// (lock_bead.go). lockMove re-checks the holder under a row lock in the
	// write transaction below.
	if err := s.lockTakeRefusal(ctx, bead, in); err != nil {
		return err
	}
	u.lockMove = lockChange(bead, in)
	return nil
}

// applyColumns applies the patch's column changes to the bead, recording
// each old and new value.
func (u *beadUpdate) applyColumns() {
	in, bead, changes, oldValues := u.in, u.bead, u.changes, u.oldValues
	if in.Title != nil {
		oldValues["title"] = bead.Title
		bead.Title = *in.Title
		changes["title"] = bead.Title
	}
	if in.ShortTitle != nil {
		if in.OnlyIfShortTitleEmpty && strings.TrimSpace(bead.ShortTitle) != "" {
			// Groomer guard: an authored value always wins (plan D5).
		} else {
			oldValues["short_title"] = bead.ShortTitle
			bead.ShortTitle = *in.ShortTitle
			changes["short_title"] = bead.ShortTitle
		}
	}
	if in.Heraldry != nil && *in.Heraldry != bead.Heraldry {
		// A change releases the old arms back to the register (R7).
		oldValues["heraldry"] = bead.Heraldry
		bead.Heraldry = *in.Heraldry
		changes["heraldry"] = bead.Heraldry
	}
	if in.Description != nil {
		oldValues["description"] = bead.Description
		bead.Description = *in.Description
		changes["description"] = bead.Description
	}
	if in.Notes != nil {
		oldValues["notes"] = bead.Notes
		bead.Notes = *in.Notes
		changes["notes"] = bead.Notes
	}
	if in.Status != nil {
		oldValues["status"] = bead.Status
		bead.Status = model.Status(*in.Status)
		changes["status"] = bead.Status
	}
	if in.Priority != nil {
		oldValues["priority"] = bead.Priority
		bead.Priority = *in.Priority
		changes["priority"] = bead.Priority
	}
	u.priorAssignee, u.priorLabels = bead.Assignee, bead.Labels
	if in.Assignee != nil {
		oldValues["assignee"] = bead.Assignee
		bead.Assignee = *in.Assignee
		changes["assignee"] = bead.Assignee
	}
	if in.Owner != nil {
		oldValues["owner"] = bead.Owner
		bead.Owner = *in.Owner
		changes["owner"] = bead.Owner
	}

	// Bug 3 fix: treat zero time as "clear the field".
	if in.dueAtSet {
		oldValues["due_at"] = bead.DueAt
		bead.DueAt = clearedIfZero(in.DueAt)
		changes["due_at"] = bead.DueAt
	}
	if in.deferUntilSet {
		oldValues["defer_until"] = bead.DeferUntil
		bead.DeferUntil = clearedIfZero(in.DeferUntil)
		changes["defer_until"] = bead.DeferUntil
	}
}

// clearedIfZero is t, or nil when t is the zero time: a zero time in a patch
// clears the field.
func clearedIfZero(t *time.Time) *time.Time {
	if t != nil && t.IsZero() {
		return nil
	}
	return t
}

// applyFieldsPatch merge-patches the patch's fields onto the bead's, then
// refuses the fields the patch may not change.
func (s *BeadsServer) applyFieldsPatch(ctx context.Context, u *beadUpdate) error {
	if u.in.Fields == nil {
		return nil
	}
	if err := u.mergeFields(); err != nil {
		return err
	}
	return s.checkFieldsPatchGuards(ctx, u)
}

// mergeFields merges the patch's fields into the bead's (RFC 7386 patch
// semantics) and records the per-key transitions.
func (u *beadUpdate) mergeFields() error {
	in, bead := u.in, u.bead
	u.oldValues["fields"] = string(bead.Fields)
	// Merge incoming fields into existing fields (patch semantics).
	existing := make(map[string]any)
	if len(bead.Fields) > 0 {
		_ = json.Unmarshal(bead.Fields, &existing)
	}
	var patch map[string]any
	if err := json.Unmarshal(in.Fields, &patch); err == nil {
		// Capture per-field transitions BEFORE the wholesale merge so a
		// declarative `field_changed` / `changed_to` watcher can observe a
		// custom-field rotation (e.g. `enabled changed_to false` on a
		// trigger/gardener disable — bug B3, ka-0000004185). Without this,
		// the published Changes map only ever carries the opaque "fields"
		// key because the blob is patch-merged wholesale, so no per-bead
		// event marks a gardener going dark (subscriber sse.go documents the
		// same limitation for agent_state, ka-0000000194).
		u.fieldChanges = perFieldChanges(existing, patch)
		for k, v := range patch {
			// RFC 7386 merge-patch semantics: a top-level JSON null
			// DELETES the key rather than storing it. This is the only
			// way to restore a field to "absent" (distinct from empty
			// string or a stored null), and it fixes the `-f model=null`
			// footgun at the source — a stored null used to re-marshal to
			// the literal string "null" and 401 every pod under the role
			// (ka-0000001419 / bug ka-0000007246). Both transports funnel
			// here, so gRPC and HTTP get delete-on-null identically.
			if v == nil {
				delete(existing, k)
				continue
			}
			existing[k] = v
		}
	}
	merged, mergeErr := json.Marshal(existing)
	if mergeErr != nil {
		return fmt.Errorf("failed to merge fields: %w", mergeErr)
	}
	bead.Fields = merged
	// Keep a role's capability_tiers and the privileged flag derived from
	// it in step (role_capability_tiers.go, gd-Bcqj2r4m7E). The schema and
	// role checks below then validate the normalized pair.
	if bead.Type == "role" {
		normalized, err := normalizeRoleTierFields(bead.Fields, in.Fields)
		if err != nil {
			return inputError(err.Error())
		}
		bead.Fields = normalized
	}
	u.changes["fields"] = bead.Fields
	return nil
}

// checkFieldsPatchGuards refuses a fields patch that changes a field fixed
// after create or owned by the daemon, and applies the server's own stamps.
func (s *BeadsServer) checkFieldsPatchGuards(ctx context.Context, u *beadUpdate) error {
	in, bead := u.in, u.bead
	priorFields := json.RawMessage(u.oldValues["fields"].(string))
	if err := checkDecisionRequestingAgentImmutable(bead, priorFields); err != nil {
		return err
	}
	// Server-stamped after the immutability check, which refused any
	// client-written stamp (decision_resolver_authz.go).
	if writesDecisionAnswer(bead, decisionAnswerKeysInPatch(in.Fields)) {
		for k, v := range s.decisionResolverStamp(ctx) {
			bead.Fields = setFieldValue(bead.Fields, k, v)
		}
		u.changes["fields"] = bead.Fields
	}

	if err := checkDecisionQuestionImmutable(bead, priorFields); err != nil {
		return err
	}

	statusBefore := bead.Status
	if st, ok := u.oldValues["status"].(model.Status); ok {
		statusBefore = st
	}
	if err := checkEscalationApprovalFieldsImmutable(ctx, bead, priorFields, statusBefore); err != nil {
		return err
	}
	if err := checkEscalationFrozenFields(ctx, bead, priorFields); err != nil {
		return err
	}
	// Spec D8 (P7 Task 11): an escalation's decision fields are the daemon's
	// and its filing fields are frozen, for every caller, admins included; the
	// admin-exempt guards above keep their own messages.
	// It subsumes the requesting_agent immutability of gd-8eu41KlMr2 (PR #63):
	// replay attributes taints to requesting_agent at approval time
	// (WithTaintAttributionAgent in replayEscalation), so no PATCH may move it.
	if err := checkEscalationFieldsImmutable(ctx, bead, priorFields, statusBefore); err != nil {
		return err
	}

	// gd-pNBzMpyTDO (PR #66, send-back finding 1): "born inert" was only
	// true at create — type:scrub-request is project_scoped, so the
	// requester owns their own request bead and this same PATCH path
	// would otherwise let approval_status/scope/resolved_bead_ids/etc be
	// rewritten after the create-time grant/readability evaluation ran.
	// See checkScrubRequestUpdateGuard (scrub_request.go).
	if err := s.checkScrubRequestUpdateGuard(bead, priorFields); err != nil {
		return err
	}

	// A release keeps its key, and its status and deployments stay the
	// daemon's; status is recomputed from the merged fields (releases.go).
	// The cut of an open release closes it (releases_open.go).
	if bead.Type == model.TypeRelease {
		fields, cut, err := s.checkReleaseUpdate(ctx, bead, priorFields)
		if err != nil {
			return err
		}
		bead.Fields = fields
		u.changes["fields"] = bead.Fields
		if cut.tag != "" {
			_, cut.patchContents = releaseFieldMap(in.Fields)["contents"]
			u.releaseCut = cut
			if _, ok := u.oldValues["status"]; !ok {
				u.oldValues["status"] = u.priorStatus
			}
			bead.Status = model.StatusClosed
			u.changes["status"] = bead.Status
		}
	}
	return nil
}

// applyLabelsPatch replaces the bead's labels with the patch's, then refuses
// a label or status change that control: mail may not take.
func (s *BeadsServer) applyLabelsPatch(ctx context.Context, u *beadUpdate) error {
	in, bead := u.in, u.bead
	if in.labelsSet {
		// A credit_grant stays in the land it was granted to (lands_ledger.go).
		if bead.Type == typeCreditGrant {
			if err := creditGrantLabelsUpdateError(bead.Labels, in.Labels); err != nil {
				return inputError(err.Error())
			}
		}
		// So does a land_invite (land_invite.go).
		if bead.Type == typeLandInvite {
			if err := landInviteLabelsUpdateError(bead.Labels, in.Labels); err != nil {
				return inputError(err.Error())
			}
		}
		u.oldValues["labels"] = strings.Join(bead.Labels, ",")
		bead.Labels = in.Labels
		u.changes["labels"] = bead.Labels
	}
	// control: mail labels are reserved on update as at create
	// (mail_control.go): adding one, removing one, or retargeting mail that
	// carries one. This runs before reconcileLabels' add/remove loops below.
	if denial := s.mailControlUpdateDenial(ctx, bead, u.priorLabels, u.priorAssignee); denial != nil {
		return denial
	}
	// Nor may it close or reopen control: mail; that path is close only.
	if bead.Status != u.priorStatus {
		if denial := s.mailControlCloseDenial(ctx, "update", &model.Bead{Type: bead.Type, Assignee: u.priorAssignee, Labels: u.priorLabels}, nil); denial != nil {
			return denial
		}
	}
	return nil
}

// reconcileClosedAt keeps ClosedAt in step with the patched Status.
func (u *beadUpdate) reconcileClosedAt() {
	bead := u.bead
	if bead.Status == model.StatusClosed && bead.ClosedAt == nil {
		u.oldValues["closed_at"] = bead.ClosedAt
		now := time.Now().UTC()
		bead.ClosedAt = &now
		u.changes["closed_at"] = bead.ClosedAt
	}
	if bead.Status != model.StatusClosed && bead.ClosedAt != nil {
		u.oldValues["closed_at"] = bead.ClosedAt
		bead.ClosedAt = nil
		u.changes["closed_at"] = bead.ClosedAt
	}
}

// validateUpdatedBead validates the patched bead as a whole: its invariants,
// its arms and its labels.
func (s *BeadsServer) validateUpdatedBead(ctx context.Context, u *beadUpdate) error {
	in, bead := u.in, u.bead
	if err := checkInProgressInvariant(in.Status, bead); err != nil {
		return err
	}
	if err := model.ValidateBead(bead); err != nil {
		return inputError("invalid bead: " + err.Error())
	}

	// A project-bearing bead keeps the label of the project its fields name
	// (project_label_invariant.go).
	if _, projectChanged := u.fieldChanges["project"]; projectChanged || in.labelsSet {
		tc, err := s.resolveTypeConfig(ctx, bead.Type)
		if err != nil {
			return fmt.Errorf("failed to resolve type config: %w", err)
		}
		if err := projectLabelUpdateError(ctx, bead.Type, tc, u.priorLabels, bead.Labels, bead.Fields); err != nil {
			return err
		}
	}

	// Arms (heraldry.go): new arms, or a label change that may move the bead
	// to another project's register or leave it without a single project.
	if _, armsChanged := u.changes["heraldry"]; armsChanged || in.labelsSet {
		if err := s.checkHeraldry(ctx, bead); err != nil {
			return err
		}
	}

	// A plot application stays in lands-intake and nowhere else
	// (plot_application.go).
	if bead.Type == typePlotApplication && in.labelsSet {
		if err := plotApplicationLabelError(bead.Labels); err != nil {
			return inputError(err.Error())
		}
	}

	// Advice-targeting label rules only kick in on update when the labels
	// themselves are being changed. Existing advice with grandfathered
	// gotcha combos can still be updated on other fields; the migration
	// (kd admin migrate advice-targeting) is the supported path for
	// normalizing the labels.
	if bead.Type == model.TypeAdvice {
		if _, ok := u.changes["labels"]; ok {
			if errs := model.ValidateAdviceLabels(bead.Labels); len(errs) > 0 {
				return inputError("invalid advice labels: " + formatFieldErrors(errs))
			}
		}
	}
	return nil
}

// validateUpdatedFields validates the patched fields, when the patch changed
// them: against the type config, then by the rules of the bead's type.
func (s *BeadsServer) validateUpdatedFields(ctx context.Context, u *beadUpdate) error {
	if _, ok := u.changes["fields"]; !ok {
		return nil
	}
	if err := s.validateUpdatedFieldsSchema(ctx, u); err != nil {
		return err
	}
	if err := s.validateUpdatedFieldShapes(ctx, u); err != nil {
		return err
	}
	return s.validateUpdatedFieldTransitions(ctx, u)
}

// validateUpdatedFieldsSchema validates the patched fields against the type
// config, grandfathering what was already stored.
func (s *BeadsServer) validateUpdatedFieldsSchema(ctx context.Context, u *beadUpdate) error {
	bead := u.bead
	tc, err := s.resolveTypeConfig(ctx, bead.Type)
	if err != nil {
		return fmt.Errorf("failed to resolve type config: %w", err)
	}
	if tc == nil {
		return nil
	}
	// Grandfather the values already stored on the bead before this
	// update. Two things a fresh create would reject are preserved:
	// (1) an unknown key that was already present (a schema migration
	// that drops a field, or a field written by a code path the daemon
	// no longer recognises) must not wedge subsequent PATCHes —
	// including the gb stop write that sets stop_requested
	// (ka-0000007952); (2) a DEFINED field whose value is unchanged from
	// what was already stored but fails type validation (a legacy value
	// in the wrong JSON type, e.g. a boolean gardener_stuck_escalated
	// where the registry declares a string) must not wedge every
	// subsequent fields-PATCH of OTHER fields against the bead
	// (ka-0000007257). A newly-added unknown key OR a CHANGED value is
	// still validated and still rejected.
	var prior map[string]any
	if priorJSON, ok := u.oldValues["fields"].(string); ok && priorJSON != "" {
		_ = json.Unmarshal([]byte(priorJSON), &prior)
	}
	if err := model.ValidateTypeFieldsForUpdate(bead.Fields, tc, prior); err != nil {
		// Runtime schema-drift signal on the update path — the mutation-
		// heaviest surface (every bridge PATCHes type=agent fields). Same
		// counter as the create site; see recordBeadWriteRejection
		// (ka-0000007249). The update path grandfathers already-stored
		// unknown keys, so this fires only on a NEWLY-introduced unknown
		// field — exactly the drift signal, not legacy noise.
		recordBeadWriteRejection(string(bead.Type), err)
		return inputError("invalid fields: " + err.Error())
	}
	return nil
}

// validateUpdatedFieldShapes validates the shape of the patched fields for
// the bead types whose rules read only the post-merge fields.
func (s *BeadsServer) validateUpdatedFieldShapes(ctx context.Context, u *beadUpdate) error {
	in, bead := u.in, u.bead
	if bead.Type == "doc" {
		if err := validateDocFields(bead.Fields); err != nil {
			return inputError(err.Error())
		}
	}
	// Repro-harness gate on task/molecule/rich-report beads (ka-0000002579,
	// ka-0000003501). bead.Fields is the post-merge effective state, so a
	// PATCH that flips gate_passed=true is validated against the repro_evidence
	// already stored on the bead, and a PATCH that only writes repro_evidence
	// is validated against the stored gate_passed — either write order
	// enforces "a passed gate cites a URL". rich-report is included because the
	// fsm-triage mirror PATCHes gate_passed onto the triage bead one field at a
	// time, so the gate must hold on the mirror target too; bundle is included
	// as the migration target for that same mirror (ka-0000004066).
	if bead.Type == model.TypeTask || bead.Type == model.TypeMolecule || bead.Type == model.TypeRichReport || bead.Type == model.TypeBundle {
		if err := validateReproFields(bead.Fields); err != nil {
			return inputError(err.Error())
		}
	}
	if bead.Type == "widget-type" {
		if err := validateWidgetTypeFields(bead.Fields); err != nil {
			return inputError(err.Error())
		}
	}
	if bead.Type == "widget-instance" {
		if err := validateWidgetInstanceFields(ctx, s.store, bead.Fields); err != nil {
			return inputError(err.Error())
		}
	}
	if bead.Type == model.TypeTeam {
		if err := validateTeamOwnersNotEmptied(in.Fields, bead.Fields); err != nil {
			return err
		}
	}
	return nil
}

// validateUpdatedFieldTransitions validates the patched fields for the bead
// types whose rules compare them with the fields stored before the patch.
func (s *BeadsServer) validateUpdatedFieldTransitions(ctx context.Context, u *beadUpdate) error {
	bead := u.bead
	// Project/role shape validation on update grandfathers byte-identical
	// prior field values (priorFields), so a legacy bead carrying a
	// malformed structured field is not wedged against unrelated-field
	// PATCHes — mirroring the ValidateTypeFieldsForUpdate grandfather above.
	var priorFields json.RawMessage
	if pj, ok := u.oldValues["fields"].(string); ok && pj != "" {
		priorFields = json.RawMessage(pj)
	}
	if bead.Type == "project" {
		if err := s.validateProjectUpdate(ctx, bead, priorFields); err != nil {
			return err
		}
	}
	if bead.Type == "role" {
		res := validateRoleFieldsForUpdate(bead.Fields, priorFields, bead.Title)
		if res.RejectReason != "" {
			return inputError(res.RejectReason)
		}
		logProjectRoleWarnings(ctx, "role", bead.Title, res.Warnings)
	}
	if bead.Type == "spawn_policy" {
		if reason := s.validateSpawnPolicy(ctx, bead.Fields); reason != "" {
			return inputError(reason)
		}
	}
	if bead.Type == escalationPolicyType {
		if reason := validateEscalationPolicy(bead.Fields); reason != "" {
			return inputError(reason)
		}
	}
	if err := validateLandsRecordUpdate(bead, priorFields); err != nil {
		return err
	}
	if bead.Type == typePlotApplication {
		if err := plotApplicationNameRecheckError(ctx, s.store, priorFields, bead.Fields); err != nil {
			return inputError(err.Error())
		}
	}
	// R6 gardener-enable gate (task ka-0000006796, epic ka-0000006896):
	// reject a write that ENABLES a plan gardener while its bound plan is
	// still being authored (plan_state ∈ {draft, ready}). Mirrors the
	// cockpit's gardenerEnableGatedByState so NO API caller — not just the
	// /plans UI — can start a gardener on a half-baked plan. Fires only on
	// the transition INTO enabled (priorFields disabled → bead.Fields
	// enabled), so the engine's frequent operational writes on an
	// already-(dis)abled gardener never pay the extra plan lookup.
	return s.validateGardenerEnableTransition(ctx, bead.Type, bead.Labels, priorFields, bead.Fields)
}

// validateProjectUpdate validates a patched project bead's fields against
// the fields stored before the patch.
func (s *BeadsServer) validateProjectUpdate(ctx context.Context, bead *model.Bead, priorFields json.RawMessage) error {
	res := validateProjectFieldsForUpdate(bead.Fields, priorFields, bead.Title)
	if res.RejectReason != "" {
		return inputError(res.RejectReason)
	}
	if err := validateProjectIntakeTaintsForUpdate(ctx, s.store, bead.Fields, priorFields); err != nil {
		return inputError(err.Error())
	}
	if err := tenantLandMirrorError(bead.Fields); err != nil {
		return inputError(err.Error())
	}
	if err := s.validateTenantLandUntrustedInput(ctx, bead, priorFields, "update"); err != nil {
		return err
	}
	if err := landStewardApprovesWriteError(bead.Type, priorFields, bead.Fields); err != nil {
		return err
	}
	logProjectRoleWarnings(ctx, "project", bead.Title, res.Warnings)
	return nil
}

// validateLandsRecordUpdate validates a legal state move on a plan epic or a
// Lands record, and refuses an edit of a ledger record.
func validateLandsRecordUpdate(bead *model.Bead, priorFields json.RawMessage) error {
	// Plan-lifecycle state machine (task ka-0000004011, epic ka-0000006896):
	// reject illegal plan_state jumps on a plan epic (draft→complete,
	// building→ready, …). The enum VALUES are already constrained by the
	// type:epic schema above; this adds the missing transition BEHAVIOR. An
	// UNSET plan_state stays ungated (legacy/hand-marked plans opt in, or clear
	// back out, freely) — backward-compat is load-bearing. priorFields is the
	// byte-identical pre-patch blob captured above.
	if bead.Type == model.TypeEpic {
		if err := validatePlanStateTransition(priorFields, bead.Fields); err != nil {
			return inputError(err.Error())
		}
	}
	// Lands plot application (plot_application.go): field shapes on the
	// post-merge bead and a legal state move.
	if bead.Type == typePlotApplication {
		if err := validatePlotApplicationUpdate(priorFields, bead.Fields); err != nil {
			return inputError(err.Error())
		}
	}
	// Lands member invite (land_invite.go): who, what role and until when
	// are fixed; a legal state move.
	if bead.Type == typeLandInvite {
		if err := validateLandInviteUpdate(priorFields, bead.Fields); err != nil {
			return inputError(err.Error())
		}
	}
	// Sign-in invite (invite.go): who, to what and until when are fixed;
	// a legal state move.
	if bead.Type == typeInvite {
		if err := validateInviteUpdate(priorFields, bead.Fields); err != nil {
			return inputError(err.Error())
		}
	}
	// Gatehouse steward request (gatehouse_request.go): what the steward
	// filed is fixed; a legal state move.
	if bead.Type == typeGatehouseRequest {
		if err := validateGatehouseRequestUpdate(priorFields, bead.Fields); err != nil {
			return inputError(err.Error())
		}
	}
	// Lands credit ledger (lands_ledger.go): grants and rate versions are
	// never edited.
	if err := ledgerRecordUpdateError(bead.Type, priorFields, bead.Fields); err != nil {
		return inputError(err.Error())
	}
	return nil
}

// validateDocPayloadUpdate is the doc payload-placement guard on update
// (ka-0000003524). Fires only when THIS patch writes the description — a
// mis-author can slip a doc body into `description` in a description-only
// patch that never touches fields, and the fields-block guard in
// validateUpdatedFieldShapes would miss it. Gating on the description change
// (not on fields) deliberately grandfathers a legacy blank doc that already
// carries a big description: an unrelated PATCH (status, labels, position)
// must never be wedged, and the documented fix — `kd update <doc> -f
// content=@file` — passes because it supplies content. bead.Fields /
// bead.Description are the post-merge effective state.
func (u *beadUpdate) validateDocPayloadUpdate() error {
	if u.bead.Type == "doc" {
		if _, descChanged := u.changes["description"]; descChanged {
			if err := validateDocPayloadPlacement(u.bead.Fields, u.bead.Description); err != nil {
				return inputError(err.Error())
			}
		}
	}
	return nil
}

// checkUpdatedConversation re-checks the Commons land rules and a message's
// card rules on the patched bead, and that a gatehouse request is answered
// once. A channel, card or gatehouse answer write takes its write lock here;
// the caller holds it until updateBead returns by deferring the returned
// release, which is never nil.
func (s *BeadsServer) checkUpdatedConversation(ctx context.Context, u *beadUpdate) (release func(), err error) {
	in, bead := u.in, u.bead
	var unlocks []func()
	release = func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
	// Commons land rules on update (channel_validation.go): re-checked whenever
	// the fields, labels or status change, against the post-merge bead. A
	// channel's land is fixed at create, since its messages are in that land.
	_, fieldsChanged := u.changes["fields"]
	_, statusChanged := u.changes["status"]
	if isLandBound(bead.Type, bead.Fields) && (fieldsChanged || statusChanged || in.labelsSet) {
		if bead.Type == typeChannel {
			s.channelWriteMu.Lock()
			unlocks = append(unlocks, s.channelWriteMu.Unlock)
			if prior, ok := u.oldValues["labels"].(string); ok && in.labelsSet {
				if err := channelLandChangeError(strings.Split(prior, ","), bead.Labels); err != nil {
					return release, inputError(err.Error())
				}
			}
		}
		if bead.Type == typeNotification {
			if prior, ok := u.oldValues["labels"].(string); ok && in.labelsSet {
				if err := notificationLandChangeError(strings.Split(prior, ","), bead.Labels); err != nil {
					return release, inputError(err.Error())
				}
			}
		}
		if err := validateConversationLand(ctx, s.store, bead, s.landErrorVisibility(ctx)); err != nil {
			return release, inputError(err.Error())
		}
	}
	if bead.Type == "message" && fieldsChanged {
		if err := validateMessageCardFields(bead.Fields); err != nil {
			return release, inputError(err.Error())
		}
		if _, _, ok := cardKey(bead.Type, bead.Fields); ok {
			s.cardWriteMu.Lock()
			unlocks = append(unlocks, s.cardWriteMu.Unlock)
			if err := cardUpdateError(ctx, s.store, bead); err != nil {
				return release, inputError(err.Error())
			}
		}
	}
	if bead.Type == typeGatehouseRequest && fieldsChanged {
		s.gatehouseRequestMu.Lock()
		unlocks = append(unlocks, s.gatehouseRequestMu.Unlock)
		if err := gatehouseAnswerOnceError(ctx, s.store, bead.ID, in.Fields); err != nil {
			return release, err
		}
	}
	if u.releaseCut.tag != "" {
		s.releaseWriteMu.Lock()
		unlocks = append(unlocks, s.releaseWriteMu.Unlock)
		stored, cutErr := s.checkReleaseCutRace(ctx, bead, u.releaseCut)
		if cutErr != nil {
			return release, cutErr
		}
		if stored != nil {
			// The cut took contents appended since its read: diff against them.
			u.oldValues["fields"] = string(stored)
			u.changes["fields"] = bead.Fields
		}
	}
	return release, nil
}

// resealSpawnLineage voids or re-stamps the spawn-lineage seal the patch
// affects, and records a resulting fields change.
//
// Re-seal on a gardenio-admin OIDC write: only when the bead was already
// validly sealed (the admin is continuing their own work), the admin
// explicitly adopted the current content, or the admin's write reopens or
// re-enables it (turning it on is consent to run it). Otherwise a one-field
// admin edit would bless whatever a non-admin rewrote in the meantime.
//
// A reopen or re-enable by anyone but such a session strips the seal first:
// content equality cannot see it (spawn_lineage_seal.go). When that write
// turns on a plan's gardener the owner approved, the approval seals it
// (spawn_lineage_plan_approval.go), as does the owner's ask in the plan's
// thread (spawn_lineage_thread_ask.go). A gardener idling its own trigger keeps
// the seal it had (gardener_self_idle.go), and so does an engine-closed plan
// gardener the home steward or the laptop lane re-arms (spawn_lineage_rearm.go).
func (s *BeadsServer) resealSpawnLineage(ctx context.Context, u *beadUpdate) error {
	bead := u.bead
	before := string(bead.Fields)
	reactivated := reactivates(bead, u.wasClosed, u.wasEnabled)
	if !u.stewardRearm {
		voidSpawnLineageOnReactivation(ctx, bead, u.wasClosed, u.wasEnabled)
	}
	// A reseal that pins the digest the admin was shown adopts exactly that
	// content: bead.Fields is the final merged state the seal would cover, so a
	// concurrent rewrite (or a patch in this same request) fails here.
	if u.resealRequested && u.resealExpectedDigest != "" && isAutonomousBeadType(bead.Type) {
		if current := spawnLineageDigest(bead.Fields); current != u.resealExpectedDigest {
			return &spawnLineageDigestMismatchError{expected: u.resealExpectedDigest, current: current}
		}
	}
	if !s.stampSpawnLineage(ctx, bead, u.sealedBeforePatch || u.resealRequested || reactivated) &&
		!(reactivated && s.stampSpawnLineageFromPlanApproval(ctx, bead)) &&
		!(reactivated && s.stampSpawnLineageFromThreadAsk(ctx, bead)) &&
		!(u.selfIdle && carrySpawnSealAcrossSelfIdle(bead, u.selfIdleSeal)) {
		s.stampSpawnLineageFromHomeSteward(ctx, bead)
	}
	// An update by an impersonated session names the admin behind it
	// (impersonation.go).
	stampImpersonatedBy(ctx, bead)
	if string(bead.Fields) != before {
		u.changes["fields"] = bead.Fields
		if _, ok := u.oldValues["fields"]; !ok {
			// No fields patch, so before is still the stored blob. History and
			// the persisted event diff against it.
			u.oldValues["fields"] = before
		}
	}
	return nil
}

// updateAssignTaints is the taint propagation an assignment carries onto the
// assignee agent bead, resolved before the write so the write can apply it in
// the same transaction as the assignee change.
type updateAssignTaints struct {
	// agent is the assignee agent bead, or nil when there is nothing to
	// propagate onto.
	agent     *model.Bead
	taints    []pendingBeadTaint
	grants    []pendingTaintGrant
	appliedBy string
	now       time.Time
}

// resolveUpdateAssignTaints resolves the taint propagation on assignment.
//
// Taint propagation on assignment (ADR docs/adr/taints.md §4.2 row 2 /
// §5.2, gd-Kebd0XJHZ2): setting this bead's assignee to a type=agent bead
// propagates this (the task) bead's taints onto that agent bead, gated
// per-taint on this caller (the assigner) holding the taint's grant. See
// resolveAssignTaints. Resolved here (read-only) so the write below can
// apply it inside the same transaction as the assignee change itself —
// a failure between the two must never leave an agent assigned to a
// tainted task with no taint (the bug the coordinator review on PR #39
// caught: this used to run through s.store AFTER UpdateBead had already
// committed).
func (s *BeadsServer) resolveUpdateAssignTaints(ctx context.Context, u *beadUpdate) (*updateAssignTaints, error) {
	bead := u.bead
	plan := &updateAssignTaints{}
	if u.in.Assignee != nil && bead.Assignee != "" {
		ab, aErr := s.store.GetBead(ctx, bead.Assignee)
		// A not-found assignee is a legitimate skip (nothing to propagate
		// onto); any other error — a transient DB failure included — must
		// NOT be swallowed here, or the assignee change below commits with
		// taint propagation silently skipped (the exact fails-open shape a
		// coordinator review caught: see the transactional-apply comment
		// above for the sibling bug on the same code path).
		if aErr != nil && !errors.Is(aErr, sql.ErrNoRows) {
			return nil, fmt.Errorf("resolving assignee bead for taint propagation: %w", aErr)
		}
		if ab != nil && ab.Type == "agent" {
			plan.agent = ab
		}
	}
	// Fail-closed BEFORE resolveAssignTaints runs, mirroring createBead's
	// resolveAuditOperatorRequired gate: an unresolvable operator assigning a
	// task to an agent bead must refuse the write, not silently apply the
	// taint marker under an empty applied_by and surface the store's opaque
	// "applied_by: is required" constraint error instead. Fail-closed here is
	// correct per D2 (the marker itself is unconditional once an operator is
	// attributable) — only the caller's identity gates.
	plan.appliedBy, _ = s.resolveOperatedByPure(ctx)
	if plan.agent != nil && plan.appliedBy == "" {
		return nil, newOperatorRefusal(ctx)
	}
	if plan.agent != nil {
		var rErr error
		plan.taints, plan.grants, rErr = s.resolveAssignTaints(ctx, bead.ID)
		if rErr != nil {
			return nil, fmt.Errorf("resolving assign-taint propagation: %w", rErr)
		}
	}
	plan.now = time.Now().UTC()
	return plan, nil
}

// checkLockHolder is the lock bead's compare-and-set inside the write
// transaction: the holder must still be the one checked before the patch.
func (u *beadUpdate) checkLockHolder(ctx context.Context, tx store.Store) error {
	if !u.lockMove {
		return nil
	}
	// Compare-and-set: the holder must still be the one checked above.
	holder, err := tx.LockBeadAssignee(ctx, u.bead.ID)
	if err != nil {
		return fmt.Errorf("failed to lock bead: %w", err)
	}
	if holder != u.priorAssignee {
		return &lockHeldError{id: u.bead.ID, holder: holder, raced: true}
	}
	return nil
}

// fillShortTitle is the write of a patch admitted only as a short-title
// fill: the emptiness check and the write are one statement, and nothing but
// short_title is written.
func (u *beadUpdate) fillShortTitle(ctx context.Context, tx store.Store) error {
	filled, err := tx.FillShortTitle(ctx, u.bead)
	if err != nil {
		return fmt.Errorf("failed to update bead: %w", err)
	}
	if !filled {
		return shortTitleFillLostRace(u.bead)
	}
	return nil
}

// apply applies the resolved taints and grants to the assignee agent bead
// inside the write transaction.
func (a *updateAssignTaints) apply(ctx context.Context, tx store.Store) error {
	if a.agent == nil {
		return nil
	}
	if err := applyPendingBeadTaints(ctx, tx, a.agent.ID, a.taints, a.appliedBy); err != nil {
		return err
	}
	return applyPendingAssignGrants(ctx, tx, a.agent.ID, a.grants, a.now)
}

// reconcileUpdatedLabels reconciles a patched label set in the store.
//
// Bug 1 fix: reconcile labels in the store. A project move re-keys the
// arms (beads.project, migration 057); checkHeraldry above checked the
// new register, and a race past it surfaces as the same conflict.
func (s *BeadsServer) reconcileUpdatedLabels(ctx context.Context, u *beadUpdate) error {
	if _, ok := u.changes["labels"]; !ok {
		return nil
	}
	if err := s.reconcileLabels(ctx, u.bead.ID, u.bead.Labels); err != nil {
		if isHeraldryRegisterViolation(err) {
			return s.heraldryRaceConflict(ctx, u.bead, err)
		}
		return fmt.Errorf("failed to reconcile labels: %w", err)
	}
	return nil
}

// publishChanges is the Changes map updateBead publishes: the per-field
// transitions overlaid with the change set.
//
// Overlay per-field transitions onto the published Changes map (the opaque
// "fields" key stays for back-compat). Reserved top-level keys already in
// `changes` win, so a custom field that happens to share a reserved name
// (e.g. a type-config field "status") never clobbers a real column
// transition. publishChanges is a separate map so mutation-history keeps
// recording the single canonical "fields" diff.
func (u *beadUpdate) publishChanges() map[string]any {
	if len(u.fieldChanges) == 0 {
		return u.changes
	}
	publish := make(map[string]any, len(u.changes)+len(u.fieldChanges))
	maps.Copy(publish, u.fieldChanges)
	maps.Copy(publish, u.changes)
	return publish
}

// publishUpdate publishes the update and records it in mutation history.
func (s *BeadsServer) publishUpdate(ctx context.Context, u *beadUpdate) {
	bead := u.bead
	var fieldsBefore json.RawMessage
	if prior, ok := u.oldValues["fields"].(string); ok {
		fieldsBefore = json.RawMessage(prior)
	}
	s.recordAndPublish(ctx, events.TopicBeadUpdated, bead.ID, "", events.BeadUpdated{
		Bead:         bead,
		Changes:      u.publishChanges(),
		Actor:        actorFromContext(ctx),
		FieldsBefore: fieldsBefore,
	})
	if u.reactionsOnly {
		// Reacting to a message or joining a channel is not authoring it: no
		// dibs flip, or the bead would be re-attributed to whoever came last.
		s.recordUpdatesNoDibs(ctx, bead.ID, actorFromContext(ctx), u.changes, u.oldValues)
	} else {
		s.recordUpdates(ctx, bead.ID, actorFromContext(ctx), u.changes, u.oldValues)
	}

	if bead.Type == model.TypeAdvice {
		s.recordAndPublish(ctx, events.TopicAdviceUpdated, bead.ID, "", events.AdviceUpdated{
			Bead:    bead,
			Changes: u.changes,
		})
	}
}

// runUpdateFollowUps runs the best-effort follow-ups of a status change, and
// of a land's freeze.
func (s *BeadsServer) runUpdateFollowUps(ctx context.Context, u *beadUpdate) {
	// Auto-link M3 (ka-0000003164): when this update is the "claim" transition
	// (status → in_progress) and the caller propagated X-Trigger-Bead from
	// BOAT_TRIGGER_BEAD_ID, wire a `triggered-by` lineage edge from the claimed
	// bead to the trigger bead. Catches the agent-creates-own-tracking-task
	// case M2 cannot reach. No-op when the claimed bead is already parented.
	if newStatus, ok := u.changes["status"].(model.Status); ok && newStatus == model.StatusInProgress {
		s.maybeAddTriggeredByOnClaim(ctx, u.bead, time.Now().UTC())
	}

	// Auto-close cascade (ka-0000001587): a status→closed PATCH (e.g. `kd update
	// <id> --status closed`, or the CLI close that routes through UpdateBead)
	// may drain a completed parent epic or release a close-on-escalation
	// wrapper. Gate on the ACTUAL transition into closed — a re-write of an
	// already-closed bead is short-circuited far above, and a non-status patch
	// never carries changes["status"]. Best-effort; never blocks the update.
	if newStatus, ok := u.changes["status"].(model.Status); ok && newStatus == model.StatusClosed {
		if prev, _ := u.oldValues["status"].(model.Status); prev != model.StatusClosed {
			s.recordCloseClient(ctx, u.bead.ID, actorFromContext(ctx))
			s.afterBeadClosed(ctx, u.bead)
		}
	}
	s.revokeFrozenLandAgents(ctx, u)
}
