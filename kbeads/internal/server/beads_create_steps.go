package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// The named steps of createBead (beads.go), in the order it runs them.

// createLocks holds the unlocks for the mutexes createBead takes across the
// insert (release, channel, merge-request, card). release runs them in
// reverse order of acquisition, as the defers they replace did.
type createLocks []func()

func (l *createLocks) hold(unlock func()) { *l = append(*l, unlock) }

func (l createLocks) release() {
	for i := len(l) - 1; i >= 0; i-- {
		l[i]()
	}
}

// createPlan is what createBead settles before the insert: the operator
// stamp, the taint rows and the dep edges the transaction writes with the bead.
type createPlan struct {
	now                 time.Time
	operatedBy          string
	operatedByID        *int64
	pendingTaints       []pendingBeadTaint
	pendingAssignGrants []pendingTaintGrant
	ratesEdgeTo         string
	explicitParent      *model.Dependency
}

// dep builds an edge from the new bead, stamped like the bead itself.
func (p *createPlan) dep(bead *model.Bead, dependsOnID string, typ model.DependencyType) *model.Dependency {
	return &model.Dependency{
		BeadID:       bead.ID,
		DependsOnID:  dependsOnID,
		Type:         typ,
		CreatedAt:    p.now,
		CreatedBy:    bead.CreatedBy,
		OperatedBy:   bead.OperatedBy,
		OperatedByID: p.operatedByID,
	}
}

// scrubCreateInput redacts credential shapes from the prose fields and
// normalises the arms.
//
// F17 (R4-SEC-4): scrub bare credential SHAPES from agent-authored prose
// before it lands in durable storage. createBead is the shared choke point
// for every transport (gRPC, HTTP, escalation replay, formula pour), so one
// pass here covers them all. Title/Description/Notes are prose; `Fields` is
// deliberately NOT redacted — account beads store a litellm_key there and
// scrubbing it would break credential distribution.
func scrubCreateInput(in *createBeadInput) error {
	in.Title = redactCredShapes(in.Title)
	in.ShortTitle = model.NormalizeShortTitle(redactCredShapes(in.ShortTitle))
	in.Description = redactCredShapes(in.Description)
	in.Notes = redactCredShapes(in.Notes)
	canonicalArms, armsErr := normalizeHeraldry(in.Heraldry)
	if armsErr != nil {
		return armsErr
	}
	in.Heraldry = canonicalArms
	return nil
}

// resolveCreateType resolves the requested type's canonical name and config,
// after the caller's type allowlist.
func (s *BeadsServer) resolveCreateType(ctx context.Context, typ string) (model.BeadType, *model.TypeConfig, error) {
	beadType := canonicalCreateType(typ)

	// A scoped principal's type allowlist applies before anything about the
	// type is resolved or validated (the project half runs with authorizeWriteTitled).
	if denial := scopedTypeDenial(ctx, "create", beadType); denial != nil {
		return "", nil, denial
	}

	// Resolve type config to determine kind and field definitions.
	tc, err := s.resolveTypeConfig(ctx, beadType)
	if err != nil {
		return "", nil, fmt.Errorf("failed to resolve type config: %w", err)
	}
	if tc == nil {
		return "", nil, inputError("unknown bead type " + string(beadType))
	}
	return beadType, tc, nil
}

// canonicalCreateType resolves deprecated type aliases (template→formula,
// bundle→molecule).
func canonicalCreateType(typ string) model.BeadType {
	beadType := model.BeadType(typ)
	if canonical, ok := model.TypeAliases[beadType]; ok {
		beadType = canonical
	}
	return beadType
}

// withCreateLineageSeeds seeds the operator resolver from the create's own
// lineage fields.
func withCreateLineageSeeds(ctx context.Context, in createBeadInput) context.Context {
	// Fleet-branch spawn-chain backstop (ka-0000001404): a header-less
	// bridge/service CreateBead that carries its own fields.spawned_by (e.g. a
	// decision respawn stamping spawned_by = the prior agent bead id, per the
	// client fix ka-0000007919) seeds the resolver's fleet-branch chain walk
	// with that lineage id BEFORE resolution runs, so the daemon traces back to
	// the ultimate invoking human instead of binding the fleet sentinel. The
	// resolver only lifts a HUMAN-shaped operator from the walk, so this never
	// weakens the fail-closed boundary — see resolveOperatedByPure's fleet case.
	if seed := spawnedByFromFields(in.Fields); seed != "" {
		ctx = WithSpawnChainSubject(ctx, seed)
	}
	// Machine-branch trigger lineage (decision gd-dI7PfsFlvr): a create that
	// carries fields.trigger_id (the trigger-engine's spawn / task / pour
	// stamp) seeds the resolver with that trigger bead id. The resolver
	// dereferences it and lifts ONLY a human author of a trigger/schedule bead,
	// and only for a machine principal holding the trigger_lineage capability
	// — see resolveOperatedByPure's machine case.
	if seed := triggerIDFromFields(in.Fields); seed != "" {
		ctx = WithTriggerLineageSubject(ctx, seed)
	}
	return ctx
}

// newCreatePlan resolves the operator the new bead is stamped with.
//
// SECURITY (AUDIT §C3): derive CreatedBy from the authenticated identity,
// never from the client-supplied input. Every call site (gRPC, HTTP,
// escalation replay) passes through createBead, so this is the choke point.
//
// Post-cutover (migration 017): beads.operated_by_id is NOT NULL, so the
// resolver MUST return a non-zero id. R2 (ka-0000005772) made this
// fail-closed at the server: when no operator can be attributed we
// return InvalidArgument so misconfigured callers get a 400 instead of
// the request silently landing under the system:legacy-pre-attribution
// sentinel.
func (s *BeadsServer) newCreatePlan(ctx context.Context, now time.Time) (*createPlan, error) {
	operatedBy, operatedByID, _, ok := s.resolveAuditOperatorRequired(ctx)
	if !ok {
		return nil, newOperatorRefusal(ctx)
	}
	return &createPlan{now: now, operatedBy: operatedBy, operatedByID: &operatedByID}, nil
}

// checkCreateGates runs the authoring-time gates: the human-author gate, the
// short_title gate and the heraldry gate.
func (s *BeadsServer) checkCreateGates(ctx context.Context, in createBeadInput, beadType model.BeadType, id, operatedBy string) error {
	if err := checkAuthoringHumanGate(beadType, operatedBy); err != nil {
		return err
	}
	if err := checkShortTitleGate(ctx, beadType, in.ShortTitle); err != nil {
		return err
	}
	// heraldry gate (badges plan step 2, gd-fn7uPujOqj): an epic should carry
	// arms. Behind BEADS_HERALDRY_GATE (home garden, default warn) and
	// BEADS_HERALDRY_GATE_TENANT_LANDS (tenant lands, default off); enforce
	// refuses only a non-human caller's plan or phase epic — see
	// heraldry_gate.go.
	return s.applyHeraldryGate(ctx, in, beadType, id)
}

// checkAuthoringHumanGate is the authoring-time human-operator gate for
// AUTONOMOUS bead types (ka-0000006012). A trigger/schedule bead (and the
// gardener loop-specs they carry) authors future, unattended work: the
// scheduler/trigger engine spawns agents whose operated_by inherits this
// bead's author. If that author is a machine/sentinel, every downstream spawn
// inherits false (or no) accountability — the exact gap that let gap-spawned
// agents fail to even file the escalation reporting they were blocked (origin
// bug ka-0000002128). The general resolver gate only guarantees *some*
// attributable operator (a human OR the system:machine sink); this stronger
// gate requires a real HUMAN at creation time so autonomous work always traces
// to an accountable person.
//
// Behind BEADS_AUTHORING_HUMAN_GATE (default off) so the tightening can
// stage warn→enforce per the directive's rollout plan (ka-0000003187).
// Modes:
//
//	off     → no gate, no metric (zero behavior change).
//	warn    → never refuses; records M9 so the operator can see the exact
//	          non-human autonomous authoring a flip to enforce would
//	          refuse (action=allowed_warn) vs the healthy human authoring
//	          (action=allowed_human).
//	enforce → refuses a non-human autonomous author (action=refused).
func checkAuthoringHumanGate(beadType model.BeadType, operatedBy string) error {
	mode := authoringHumanGateModeFromEnv()
	if mode == authoringGateOff || !isAutonomousBeadType(beadType) {
		return nil
	}
	human := isHumanOperator(operatedBy)
	switch {
	case human:
		recordAuthoringGate(string(beadType), authoringGateActionAllowedHuman)
	case mode == authoringGateEnforce:
		recordAuthoringGate(string(beadType), authoringGateActionRefused)
		return inputError(
			"autonomous bead type " + string(beadType) +
				" requires a human operator (slack:U… / oidc:…@…); resolved operator " +
				operatedBy + " is not a human — set X-Operated-By or spawn with a human author",
		)
	default: // warn
		recordAuthoringGate(string(beadType), authoringGateActionAllowedWarn)
	}
	return nil
}

// checkShortTitleGate is the short_title gate (plan 2026-10-14-short-titles
// D2, gd-V5nGseRS5f): the human-facing types must carry a short_title. Behind
// BEADS_SHORT_TITLE_GATE (default warn — see short_title_gate.go):
//
//	off     → no gate, no metric.
//	warn    → never refuses; records action=allowed_warn and adds an
//	          X-Beads-Warning for an HTTP caller.
//	enforce → refuses a non-human caller (action=refused); a human
//	          caller is still only warned.
//
// action=supplied counts the compliant creates so the ratio is observable.
func checkShortTitleGate(ctx context.Context, beadType model.BeadType, shortTitle string) error {
	mode := shortTitleGateModeFromEnv()
	if mode == shortTitleGateOff || !isShortTitleGatedType(beadType) {
		return nil
	}
	caller := shortTitleGateCallerHuman
	if isNonHumanCaller(AuthIdentityFromContext(ctx)) {
		caller = shortTitleGateCallerAgent
	}
	switch {
	case strings.TrimSpace(shortTitle) != "":
		recordShortTitleGate(string(beadType), caller, shortTitleGateActionSupplied)
	case mode == shortTitleGateEnforce && caller == shortTitleGateCallerAgent:
		recordShortTitleGate(string(beadType), caller, shortTitleGateActionRefused)
		return inputError("short_title is required for type " + string(beadType) + ": " + model.ShortTitleHelp)
	default:
		recordShortTitleGate(string(beadType), caller, shortTitleGateActionAllowedWarn)
		addWarning(ctx, "short_title missing: "+model.ShortTitleHelp)
		slog.Warn("short_title missing", "type", beadType, "actor", actorFromContext(ctx), "mode", mode.String())
	}
	return nil
}

// settleCreateInput fills in what the caller may omit (inherited fields,
// idempotency key, owner) and resolves the taints into plan.
func (s *BeadsServer) settleCreateInput(ctx context.Context, in *createBeadInput, beadType model.BeadType, tc *model.TypeConfig, plan *createPlan) error {
	// Auto-inherit Fields[operated_by] and Fields[user_bead_id] from the
	// sanitized request context when the caller omitted them (ka-0000006800).
	// The opt-out X-No-Auto-Inherit header is honored via context. Idempotent
	// if the fields are already set: every per-site spawn handler that pushes
	// these explicitly continues to win.
	in.Fields = applyAutoInherit(ctx, in.Fields, tc)

	var err error
	plan.pendingTaints, plan.pendingAssignGrants, err = s.resolveCreateTaintSet(ctx, beadType, *in)
	if err != nil {
		return err
	}
	applyCreateIdempotencyKey(in, beadType)

	// Default Owner from the spawn-chain root human when the caller omitted
	// it (ka-0000000058, supersedes ka-0000000269 verbatim-X-Operated-By
	// behavior). Gated by BEADS_OWNER_DEFAULT_ENABLED; default off. Opt-out
	// via X-No-Owner-Default header, X-Owner-Override (explicit Owner), or
	// in.Owner == "-" sentinel. See auto_owner.go for the policy details.
	in.Owner = s.applyOwnerDefault(ctx, in.Owner)
	return nil
}

// resolveCreateTaintSet resolves the taints the new bead is born with and the
// grants an agent bead's assignment carries.
func (s *BeadsServer) resolveCreateTaintSet(ctx context.Context, beadType model.BeadType, in createBeadInput) ([]pendingBeadTaint, []pendingTaintGrant, error) {
	// Taint propagation (ADR docs/adr/taints.md §4.2 row 1, §10.3): resolve the
	// caller-agent ∪ parent-bead ∪ project-intake taint set here, between
	// applyAutoInherit and authorizeWriteTitled. There is no X-No-Auto-Inherit
	// opt-out for this — see resolveCreateTaints. The rows themselves are
	// written inside the create transaction (applyPendingBeadTaints), once the
	// bead exists.
	pendingTaints, err := s.resolveCreateTaints(ctx, in.ParentBeadID, in.Labels, in.Fields)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving taint propagation: %w", err)
	}

	// Taint propagation on assignment (ADR docs/adr/taints.md §4.2 row 2 /
	// §5.2, gd-Kebd0XJHZ2): a type=agent bead created already carrying a task
	// (fields.task_id) inherits that task's taints, gated per-taint on the
	// creating caller (the assigner) holding the taint's grant. See
	// resolveAssignTaints.
	var pendingAssignGrants []pendingTaintGrant
	if beadType == "agent" {
		if taskBeadID := taskIDFromFields(in.Fields); taskBeadID != "" {
			assignTaints, assignGrants, err := s.resolveAssignTaints(ctx, taskBeadID)
			if err != nil {
				return nil, nil, fmt.Errorf("resolving assign-taint propagation: %w", err)
			}
			pendingTaints = append(pendingTaints, assignTaints...)
			pendingAssignGrants = assignGrants
		}
	}
	return pendingTaints, pendingAssignGrants, nil
}

// applyCreateIdempotencyKey settles the create's idempotency key and stores
// it in Fields.
func applyCreateIdempotencyKey(in *createBeadInput, beadType model.BeadType) {
	// A release is unique on (project, tag) (releases.go): its key is the
	// daemon's, whatever the caller sent, so every writer converges on one bead.
	if beadType == model.TypeRelease {
		if key := releaseIdempotencyKeyForCreate(in.Fields); key != "" {
			in.IdempotencyKey = key
		}
	}

	// Persist the idempotency key into Fields before ValidateTypeFields so it
	// is both schema-validated (declared in auditFields) and stored, letting
	// the lookup-or-return find it via a fields-equality query.
	if in.IdempotencyKey != "" {
		in.Fields = setField(in.Fields, idempotencyKeyField, in.IdempotencyKey)
	}
}

// newBeadForCreate builds the bead to insert from the settled input, with the
// type's create-time field defaults applied.
func newBeadForCreate(ctx context.Context, in createBeadInput, id string, beadType model.BeadType, tc *model.TypeConfig, plan *createPlan) (*model.Bead, error) {
	initialStatus := model.StatusOpen
	if in.Status != "" {
		s := model.Status(in.Status)
		if !s.IsValid() {
			return nil, inputError("invalid status: " + in.Status)
		}
		initialStatus = s
	}

	bead := &model.Bead{
		ID:           id,
		Kind:         tc.Kind,
		Type:         beadType,
		Title:        in.Title,
		ShortTitle:   in.ShortTitle,
		Heraldry:     in.Heraldry,
		Description:  in.Description,
		Notes:        in.Notes,
		Status:       initialStatus,
		Priority:     in.Priority,
		Assignee:     in.Assignee,
		Owner:        in.Owner,
		CreatedAt:    plan.now,
		CreatedBy:    actorFromContext(ctx),
		OperatedBy:   plan.operatedBy,
		OperatedByID: plan.operatedByID,
		UpdatedAt:    plan.now,
		DueAt:        in.DueAt,
		DeferUntil:   in.DeferUntil,
		Labels:       in.Labels,
		Fleet:        FleetFromContext(ctx),
	}
	// Maintain status=closed ⇔ closed_at IS NOT NULL invariant when the
	// caller opts to create a bead in a closed state (e.g. ledger
	// records that have no work lifecycle — ka-0000004100).
	if initialStatus == model.StatusClosed {
		now := plan.now
		bead.ClosedAt = &now
	}

	if len(in.Fields) > 0 {
		bead.Fields = in.Fields
	}

	// Default type=doc `format` to "md" when the caller omitted it. The
	// type:doc schema (config.go) declares `format` as a required enum, so a
	// bare `kd create x --type=doc` would otherwise 400 with "format: is
	// required" — a discoverability trap, since there is no --format flag and
	// md is the dominant case (ka-0000006824). Applied at this choke point so
	// every create path (gRPC, HTTP, formula pour, escalation replay) is
	// covered, and only when absent so an explicit value still validates
	// against the enum.
	if beadType == "doc" {
		bead.Fields = setFieldIfAbsent(bead.Fields, "format", "md")
	}

	// A role bead is born with capability_tiers and privileged in step
	// (role_capability_tiers.go, gd-Bcqj2r4m7E), before the schema check
	// validates the tier names.
	if beadType == "role" {
		normalized, err := normalizeRoleTierFields(bead.Fields, bead.Fields)
		if err != nil {
			return nil, inputError(err.Error())
		}
		bead.Fields = normalized
	}
	return bead, nil
}

// validateNewBead runs the model, label and schema checks, then the
// per-type checks. A release's guard takes releaseWriteMu, held in locks.
func (s *BeadsServer) validateNewBead(ctx context.Context, bead *model.Bead, in *createBeadInput, tc *model.TypeConfig, locks *createLocks) error {
	if err := model.ValidateBead(bead); err != nil {
		return inputError("invalid bead: " + err.Error())
	}

	// Advice-targeting label rules apply on create for any advice bead.
	// See model.ValidateAdviceLabels for the full rule set and rationale.
	if bead.Type == model.TypeAdvice {
		if errs := model.ValidateAdviceLabels(bead.Labels); len(errs) > 0 {
			return inputError("invalid advice labels: " + formatFieldErrors(errs))
		}
	}

	if err := model.ValidateTypeFields(bead.Fields, tc); err != nil {
		// Runtime schema-drift signal: a create refused for a field-schema
		// reason (e.g. bridge sent a field the daemon's builtin doesn't
		// declare). Counter feeds the schema-drift PrometheusRule so the drift
		// pages in ~1m instead of accumulating silent error beads (ka-0000007249).
		recordBeadWriteRejection(string(bead.Type), err)
		return inputError("invalid fields: " + err.Error())
	}

	// The type sets the two below handle are disjoint, so running both is the
	// one switch over every type they replace.
	if err := s.validateCreateTypeFields(ctx, bead); err != nil {
		return err
	}
	if err := s.applyCreateTypeGuards(ctx, bead, in, locks); err != nil {
		return err
	}

	// R6 gardener-enable gate (task ka-0000006796, epic ka-0000006896): a gardener
	// bead may not be BORN enabled while bound to a still-authoring plan
	// (plan_state ∈ {draft, ready}). The plan-gardener bind path already births
	// gated-plan gardeners paused (plangardener.EnabledOnBind); this closes the
	// direct-API-create bypass so "no API caller can enable a gardener on a
	// draft/ready plan" holds at the daemon, not just in the cockpit.
	return s.validateGardenerEnableOnCreate(ctx, bead.Type, bead.Labels, bead.Fields)
}

// validateCreateTypeFields rejects a malformed definition of the types whose
// fields carry their own rules.
//
// Runaway-protection: reject pathological formula/schedule definitions
// before persisting. Applied at the createBead choke point so every
// creation path (gRPC, HTTP, escalation replay, formula pour) is covered.
func (s *BeadsServer) validateCreateTypeFields(ctx context.Context, bead *model.Bead) error {
	var err error
	switch bead.Type {
	case "formula":
		err = validateFormulaFields(bead.Fields)
	case "schedule":
		err = validateScheduleFields(bead.Fields)
	case "trigger":
		// Reject a spawn-capable trigger with no `project` field — it would
		// spawn a loginless, repo-less zombie agent (ka-0000000254,
		// ka-0000004625). The stored prod type:trigger config row leaves
		// project non-required and shadows the builtin, so a schema-only
		// `required` flip is inert (advice ka-0000000061) — this Go guard at
		// the shared createBead choke point always runs.
		err = validateTriggerFields(bead.Fields)
	case "doc":
		err = validateDocFields(bead.Fields)
		if err == nil {
			err = validateDocPayloadPlacement(bead.Fields, bead.Description)
		}
	case "widget-type":
		err = validateWidgetTypeFields(bead.Fields)
	case "widget-instance":
		err = validateWidgetInstanceFields(ctx, s.store, bead.Fields)
	case "project":
		return s.validateProjectCreate(ctx, bead)
	case typeCreditRate:
		err = validateCreditRateFields(bead.Fields)
	case "role":
		res := validateRoleFields(bead.Fields, bead.Title)
		if res.RejectReason != "" {
			return inputError(res.RejectReason)
		}
		logProjectRoleWarnings(ctx, "role", bead.Title, res.Warnings)
	case "spawn_policy":
		if reason := s.validateSpawnPolicy(ctx, bead.Fields); reason != "" {
			return inputError(reason)
		}
	case escalationPolicyType:
		if reason := validateEscalationPolicy(bead.Fields); reason != "" {
			return inputError(reason)
		}
	case model.TypeTask, model.TypeMolecule, model.TypeRichReport, model.TypeBundle:
		// Repro-harness gate: gate_passed=true must cite a valid evidence URL.
		// Enforced for every type the repro formula writes gate fields onto: the
		// emit step (task) and pour epic (molecule), plus the rich-report the
		// fsm-triage mirror targets (ka-0000003501) so a mirrored pass carries
		// the same evidence guarantee. type=bundle is the migration target for
		// fsm-triage off rich-report (ka-0000004066), so the mirror invariant
		// must hold there too. See validateReproFields (ka-0000002579).
		err = validateReproFields(bead.Fields)
	}
	if err != nil {
		return inputError(err.Error())
	}
	return nil
}

// validateProjectCreate checks a new project bead and stamps its default
// land-steward approvals.
func (s *BeadsServer) validateProjectCreate(ctx context.Context, bead *model.Bead) error {
	res := validateProjectFields(bead.Fields, bead.Title)
	if res.RejectReason != "" {
		return inputError(res.RejectReason)
	}
	if err := validateProjectIntakeTaints(ctx, s.store, bead.Fields); err != nil {
		return inputError(err.Error())
	}
	if err := tenantLandMirrorError(bead.Fields); err != nil {
		return inputError(err.Error())
	}
	if err := s.validateTenantLandUntrustedInput(ctx, bead, nil, "create"); err != nil {
		return err
	}
	if err := landStewardApprovesWriteError(bead.Type, nil, bead.Fields); err != nil {
		return err
	}
	stamped, err := stampDefaultLandStewardApproves(bead.Fields)
	if err != nil {
		return inputError("invalid fields: " + err.Error())
	}
	bead.Fields = stamped
	logProjectRoleWarnings(ctx, "project", bead.Title, res.Warnings)
	return nil
}

// applyCreateTypeGuards runs the guards of the types whose create depends on
// other beads: decisions, escalations, mail, requests and releases.
func (s *BeadsServer) applyCreateTypeGuards(ctx context.Context, bead *model.Bead, in *createBeadInput, locks *createLocks) error {
	beadType := bead.Type
	switch beadType {
	case model.TypeDecision:
		// Dedup-ack gate first (cheaper, more common rejection): reject when
		// open decisions already exist in the project unless the caller echoed
		// the current open-list hash. Behind BEADS_DEDUP_GATE_ENABLED (default
		// off). Force (CLI --force, escalation replay) bypasses; WARN-logged
		// below. The ack-only decision guard is independent and still runs.
		if err := s.runDedupAckGate(ctx, bead, *in, beadType); err != nil {
			return err
		}
		if err := s.applyDecisionGuard(ctx, bead, in.Force); err != nil {
			return err
		}
		s.applyDecisionRequestingAgentGuard(ctx, bead)
	case model.TypeEscalation:
		return s.applyEscalationCreateGuards(ctx, bead, *in)
	case model.BeadType("mail"):
		return s.applyMailControlGuard(ctx, bead)
	case model.TypeInvestigationRequest:
		return s.applyInvestigationRequestGuard(ctx, bead)
	case model.TypeScrubRequest:
		return s.applyScrubRequestGuard(ctx, bead)
	case model.TypeRelease:
		// Held across the insert: the derived previous reads the project's
		// releases, and a deployment rewrites them (releases.go).
		s.releaseWriteMu.Lock()
		locks.hold(s.releaseWriteMu.Unlock)
		return s.applyReleaseGuard(ctx, bead, in)
	}
	return nil
}

// applyEscalationCreateGuards checks who may file the escalation and stamps
// its audience.
func (s *BeadsServer) applyEscalationCreateGuards(ctx context.Context, bead *model.Bead, in createBeadInput) error {
	if err := s.applyOperationTypeRegistry(ctx, bead); err != nil {
		return err
	}
	// A lands-tenant agent files only a merge escalation about its own
	// land (Lands Track B §5.1); the shared check (restricted_ops.go).
	if d := s.restrictedOpDenial(ctx, restrictedOpRequest{Op: opEscalationFile, Action: "create", BeadType: model.TypeEscalation, Escalation: bead}); d != nil {
		return d
	}
	if err := s.runDedupAckGate(ctx, bead, in, bead.Type); err != nil {
		return err
	}
	if err := checkEscalationCreateFields(ctx, bead); err != nil {
		return err
	}
	if err := checkEscalationStampedFieldsOnCreate(ctx, bead); err != nil {
		return err
	}
	if err := s.stampEscalationAudience(ctx, bead); err != nil {
		return err
	}
	if err := s.checkEscalationTargets(ctx, bead); err != nil {
		return err
	}
	if err := checkEscalationOperationSpec(bead); err != nil {
		return err
	}
	if _, err := s.rulingApprover(ctx, bead); err != nil {
		return err
	}
	if err := s.checkGrantEscalationPrincipal(ctx, bead); err != nil {
		return err
	}
	s.applyEscalationRequestingAgentGuard(ctx, bead)
	return nil
}

// deriveCreateLabels adds the labels a bead's fields, token, thread or parent
// imply. It returns the project-label mismatch, which createBead refuses only
// after the authz checks so their denials come first.
func (s *BeadsServer) deriveCreateLabels(ctx context.Context, bead *model.Bead, tc *model.TypeConfig, parentBeadID string) error {
	// An agent bead carries project:<fields.project> and role:<fields.role>
	// labels: its own per-agent token is project-scoped, so without the project
	// label the agent cannot read or stop its own bead (403 "token not
	// authorized for this bead's project"), gb stop never sets stop_requested
	// and the pod re-runs its prompt every ~10 min (gd-9uztBqkBTn,
	// gd-gm5XivuyTB, gd-Qixci61Q3Q). The spawn client adds both labels AFTER a
	// successful create, so a create that was denied and escalated records a
	// label-less operation_spec, and the approve replay (a machine context, not
	// an agent token) used to create the bead exactly like that. Derive them
	// here, at the choke point every create path shares.
	if bead.Type == "agent" {
		bead.Labels = withDerivedAgentLabels(bead.Labels, bead.Fields)
	}

	// Every project-bearing type, not only agents: fields.project and the
	// project: label name the same project (project_label_invariant.go).
	// Before the agent-token stamp below, so a scoped token cannot paper its
	// own project over a bead whose fields name another.
	labels, projectLabelMismatch := applyProjectLabelInvariant(ctx, bead.Type, tc, bead.Labels, bead.Fields)
	bead.Labels = labels

	// A per-agent token is scoped to one project, and a bead without that
	// project label is invisible to it (beadMatchesTokenProject). The shared
	// token an agent used before had no scope, so `kd create` without -l
	// project:<name> worked and the agent could still read the bead back. Stamp
	// the token's own project on such a bead so the agent keeps seeing what it
	// creates. A bead that names a DIFFERENT project is still refused.
	if _, isAgent := agentBeadIDFromContext(ctx); isAgent {
		if proj := TokenProjectFromContext(ctx); proj != "" && !hasProjectLabel(bead.Labels) {
			bead.Labels = append(bead.Labels, "project:"+proj)
		}
	}
	// A thread reply the bridge writes carries thread_id and no project label;
	// it takes its thread head's (project_label_invariant.go).
	if bead.Type == "message" {
		bead.Labels = s.withThreadHeadProjectLabel(ctx, bead.Labels, bead.Fields)
	}
	// Any other label-less bead born under a parent takes the parent's.
	bead.Labels = s.withParentProjectLabel(ctx, bead.Type, bead.Labels, parentBeadID)
	// Label-less mail takes its recipient's, or the recipient never sees it.
	if bead.Type == "mail" {
		bead.Labels = s.withMailRecipientProjectLabel(ctx, bead.Labels, bead.Assignee)
	}
	return projectLabelMismatch
}

// authorizeCreate enforces the token's project scope, the type's write
// policy and the field-level gates, then refuses a project-label mismatch.
// It reports whether the bead is a person's post in their frozen land's own
// #support, which passes the freeze (land_frozen.go).
func (s *BeadsServer) authorizeCreate(ctx context.Context, bead *model.Bead, projectLabelMismatch error) (bool, error) {
	beadType := bead.Type
	if !beadMatchesTokenProject(ctx, bead.Labels) {
		return false, &AuthzDenial{
			Action:   "create",
			BeadType: string(beadType),
			Message:  "token not authorized for this bead's project",
		}
	}
	// beadMatchesTokenProject passes when ANY label is the token's project; a
	// tenant agent's bead must carry no other project: label.
	if denial := s.tenantAgentLabelDenial(ctx, "create", beadType, bead.Labels); denial != nil {
		return false, denial
	}

	supportPost := frozenSupportPost(ctx, s.store, bead)
	if denial := s.authorizeWriteFrozen(ctx, "create", beadType, labelsForAuthz(bead.Labels, bead.Fields), bead.Title, supportPost); denial != nil {
		return false, denial
	}
	if denial := scopedFieldsProjectDenial(ctx, "create", beadType, bead.Fields); denial != nil {
		return false, denial
	}
	// labelsForAuthz drops fields.project when a project label is present, so
	// a runner's fields.project is checked on its own (runner_auth.go).
	if denial := runnerProjectDenial(ctx, "create", nil, bead.Fields); denial != nil {
		return false, denial
	}
	if denial := runnerRuntimeIDDenial(ctx, "create", bead.Fields, nil); denial != nil {
		return false, denial
	}
	// Field-level gate: a machine caller may create a type:user bead but not
	// seed its identity fields (gd-uAvleAZUvV).
	if denial := s.authorizeUserIdentityFields(ctx, "create", beadType, bead.Fields, nil); denial != nil {
		return false, denial
	}

	// Field-level gate: only an admin may name a pod ServiceAccount or turn its
	// API token on from an agent bead (gd-Gpvi7wld87).
	if denial := s.authorizeAgentIdentityFields(ctx, "create", beadType, bead.Fields, nil); denial != nil {
		return false, denial
	}
	// Nor may an agent token start one with the purse watcher's park fields
	// set (agent_park_fields.go).
	if denial := agentParkFieldPatchDenial(ctx, "create", beadType, bead.Fields, nil); denial != nil {
		return false, denial
	}
	// Field-level gate: only an admin binds a channel to Slack
	// (channel_binding_authz.go, gd-ecFKkjtdvS). Before the land rules, so a
	// non-admin learns nothing from the slack_channel_id uniqueness check.
	if denial := s.authorizeChannelBindingFields(ctx, "create", beadType, bead.Fields, nil); denial != nil {
		return false, denial
	}
	// A tenant may not write a wish head's marks (wish_head_authz.go).
	if denial := s.wishHeadDenial(ctx, "create", beadType, changedWishHeadLabels(nil, bead.Labels), bead.Fields, nil); denial != nil {
		return false, denial
	}
	// Only an admin creates a project bead, and the Lands provisioner only a
	// tenant land's (land_switch_authz.go).
	if denial := s.authorizeProjectCreate(ctx, beadType, bead.Fields); denial != nil {
		return false, denial
	}
	// Field-level gate: only an admin sets a land's kind or freeze
	// (land_switch_authz.go).
	if denial := s.authorizeLandSwitchFields(ctx, "create", beadType, bead.Fields, nil); denial != nil {
		return false, denial
	}
	// Only an admin creates a project bead under a name another already has:
	// a duplicate could shadow a land's kill switch (land_switch_authz.go).
	if denial := s.authorizeProjectNameUnique(ctx, beadType, bead.Title); denial != nil {
		return false, denial
	}
	// The spawn-lineage seal is server-owned (spawn_lineage_seal.go): a create
	// may not carry one.
	if denial := spawnLineageSealPatchDenial("create", beadType, bead.Fields, nil); denial != nil {
		return false, denial
	}
	// impersonated_by is server-owned (impersonation.go): a create may carry
	// only the session's own impersonator, which the stamp below writes anyway.
	if denial := impersonatedByPatchDenial(ctx, "create", beadType, bead.Fields, nil); denial != nil {
		return false, denial
	}
	if projectLabelMismatch != nil {
		return false, projectLabelMismatch
	}
	return supportPost, nil
}

// routeAndAuthorizeSpawn stamps an escalation's route and authorizes an agent
// bead's spawn.
func (s *BeadsServer) routeAndAuthorizeSpawn(ctx context.Context, bead *model.Bead) error {
	// Escalation routing (escalation_policy.go): stamp approvers, execute_by,
	// held_by, held_at, operator_only, policy_rule, and audience from held_by
	// (D11). This runs after the agent-token project stamp, so the project is
	// final, and after #756's stampEscalationAudience, which validated the
	// filer's audience. A policy error leaves #756's audience and no held_by
	// (the sweep retries) and never fails the create.
	if bead.Type == model.TypeEscalation {
		_ = s.stampEscalationRoute(ctx, bead)
	}

	// Enforce spawn authorization for agent beads created by OIDC users.
	if bead.Type == "agent" {
		// An agent carrying a restrict taint spawns nothing (restricted_ops.go),
		// checked first so it never files a privileged spawn that could not run.
		if d := s.restrictTaintOpDenial(ctx, restrictedOpRequest{Op: opSpawn, Action: "create", BeadType: typeAgent, Role: roleFromFields(bead.Fields), Project: projectFromFields(bead.Fields)}); d != nil {
			return d
		}
		if err := authorizeAgentSpawn(s.withSameRoleContinuationAuth(s.withTriggerLineageSpawnAuth(ctx, bead.Fields), bead.Fields), s.store, bead.Fields); err != nil {
			return err
		}
		// Store the canonical role (gd-Yhbc5wdjwj): spawn_policy caps count
		// agents by exact role string, so " infra" must not be a separate
		// bucket from "infra".
		bead.Fields = canonicalizeRoleField(bead.Fields)
	}
	return nil
}

// checkCreateLandRules applies the Commons land rules to a conversation bead
// and the author rules to a message. A channel's create holds channelWriteMu
// in locks across the insert.
func (s *BeadsServer) checkCreateLandRules(ctx context.Context, bead *model.Bead, supportPost bool, locks *createLocks) error {
	beadType := bead.Type
	// Commons land rules (channel_validation.go): a channel, or a message that
	// names a channel, carries exactly one project: label, and the per-land and
	// cross-land uniqueness rules hold. Runs after the agent-token project
	// stamp and after authz, so a caller cannot probe another land's channel
	// names. channelWriteMu serialises the uniqueness lookup with the insert.
	if isLandBound(beadType, bead.Fields) {
		if beadType == typeChannel {
			s.channelWriteMu.Lock()
			locks.hold(s.channelWriteMu.Unlock)
		}
		if err := validateConversationLand(ctx, s.store, bead, s.landErrorVisibility(ctx)); err != nil {
			return inputError(err.Error())
		}
		// A person posts in a user channel or a DM only as one of its members
		// (channel_membership.go). Not in a frozen land's #support: nobody can
		// join a channel during a freeze, and every member may still write there.
		if denial := channelPostDenial(ctx, s.store, bead); denial != nil && !supportPost {
			return denial
		}
		// A mention's notification is to a member of the message's land, about
		// the caller's own message (notification.go).
		if beadType == typeNotification {
			if err := validateNotificationCreate(ctx, s.store, bead, s.landErrorVisibility(ctx)); err != nil {
				return inputError(err.Error())
			}
		}
	}
	// A message with origin=commons is a person's own words, written only by
	// that person (commons_origin.go).
	if denial := commonsOriginDenial(ctx, beadType, bead.Fields); denial != nil {
		return denial
	}
	// A message's author is its writer unless a bridge or a machine names it
	// (message_author.go).
	if denial := messageAuthorDenial(ctx, beadType, bead.Fields); denial != nil {
		return denial
	}
	// A message names its writer's GitHub account only as the daemon verified
	// it (gatehouse_request.go): a client value is refused, and an OIDC
	// person's message in the gatehouse is stamped.
	if denial := stampMessageGitHubAuthor(ctx, s.store, bead); denial != nil {
		return denial
	}
	if beadType == "message" {
		if err := validateMessageCardFields(bead.Fields); err != nil {
			return inputError(err.Error())
		}
	}
	return nil
}

// validateLandsCreate checks the Lands records whose create reads other
// beads. Each runs after authz, so a caller who may not file the record
// cannot use its lookups as a probe.
func (s *BeadsServer) validateLandsCreate(ctx context.Context, bead *model.Bead) error {
	var err error
	switch bead.Type {
	case typePlotApplication:
		// Lands intake (plot_application.go): one project label, lands-intake;
		// born submitted with no decision field; land_name not already a project.
		err = validatePlotApplicationCreate(ctx, s.store, bead)
	case typeCreditGrant:
		// Lands credit ledger (lands_ledger.go): one project label naming the
		// grant's land, a tenant land, a nonzero amount.
		err = validateCreditGrantCreate(ctx, s.store, bead)
	case typeLandInvite:
		// Lands member invites (land_invite.go): one project label naming a
		// tenant land, born pending.
		err = validateLandInviteCreate(ctx, s.store, bead)
	case typeInvite:
		// Sign-in invites (invite.go): one lowercase email, born pending, a land
		// only with a role and only a tenant land.
		err = validateInviteCreate(ctx, s.store, bead)
	case typeGatehouseRequest:
		// Gatehouse steward requests (gatehouse_request.go): in the gatehouse,
		// born pending, a known kind naming a message.
		err = validateGatehouseRequestCreate(ctx, s.store, bead)
	}
	if err != nil {
		return inputError(err.Error())
	}
	return nil
}

// findExistingForCreate runs the lookup-or-return paths. A non-nil bead is
// the one the create returns instead of inserting. For a rating it also
// returns the bead the new rating's `rates` edge points at. The merge-request
// and card mutexes it takes are held in locks across the insert.
func (s *BeadsServer) findExistingForCreate(ctx context.Context, bead *model.Bead, in createBeadInput, locks *createLocks) (*model.Bead, string, error) {
	// Idempotency-key lookup-or-return (ka-0000002856). Runs after authz so a
	// caller can't probe another project's keyed beads. On hit, return the
	// existing bead with no duplicate insert and no second create event. The
	// transaction catches a 23505 race and re-runs this lookup.
	if in.IdempotencyKey != "" {
		existing, err := s.lookupByIdempotencyKey(ctx, bead.Labels, bead.Fleet, in.IdempotencyKey)
		if err != nil {
			return nil, "", err
		}
		if existing != nil {
			return existing, "", nil
		}
	}
	// After the key lookup, so a keyed retry gets its bead back (create_dedupe.go).
	if err := s.checkOpenDuplicate(ctx, bead, in); err != nil {
		return nil, "", err
	}

	// type=merge-request: daemon-enforced dedup on fields.mr_url
	// (ka-0000002762). The gitlab-bridge upserts one MR bead per mr_url, but its
	// find-then-create is serialised only by an in-process client lock, so two
	// bridge replicas (or one across a restart) each miss the client-side find
	// and each CreateBead a duplicate — the store had no uniqueness guard, so
	// every insert landed (396 mr_urls with >1 canonical bead, measured
	// 2026-08-18). We take the per-mr_url mutex here and hold it (until
	// createBead returns) across BOTH the lookup-or-return AND the insert
	// transaction so a concurrent create for the same URL cannot interleave. On
	// a hit we return the existing bead verbatim — no duplicate insert, no
	// second BeadCreated event. See beads_mr_dedup.go.
	if bead.Type == "merge-request" {
		if mu := s.lockMergeRequestUpsert(bead.Fields); mu != nil {
			locks.hold(mu.Unlock)
		}
		existing, err := s.preCreateMergeRequest(ctx, bead)
		if err != nil {
			return nil, "", err
		}
		if existing != nil {
			return existing, "", nil
		}
	}

	// Commons card (card_validation.go): one card per (subject_id,
	// channel_id), never re-posted. A create for a pair that already has a
	// card returns that card. cardWriteMu is held across the lookup and the
	// insert, as the merge-request mutex is, so two racers converge.
	if subjectID, channelID, ok := cardKey(bead.Type, bead.Fields); ok {
		s.cardWriteMu.Lock()
		locks.hold(s.cardWriteMu.Unlock)
		existing, err := findCard(ctx, s.store, subjectID, channelID, "")
		if err != nil {
			return nil, "", err
		}
		if existing != nil {
			return existing, "", nil
		}
	}

	// Gatehouse request (gatehouse_request.go): one per (message_id, kind),
	// a bounded number an hour. gatehouseRequestMu is held across the lookup
	// and the insert, as cardWriteMu is.
	if bead.Type == typeGatehouseRequest {
		s.gatehouseRequestMu.Lock()
		locks.hold(s.gatehouseRequestMu.Unlock)
		existing, err := admitGatehouseRequest(ctx, s.store, bead, time.Now())
		if err != nil {
			return nil, "", err
		}
		if existing != nil {
			return existing, "", nil
		}
	}

	// Land invite (land_invite.go): one open invite per account, open_invite_cap.
	if bead.Type == typeLandInvite {
		if existing, err := s.admitLandInviteLocked(ctx, bead, locks); err != nil || existing != nil {
			return existing, "", err
		}
	}

	// type=rating: enforce idempotent upsert on (rater, rated_bead_id) and
	// auto-create the `rates` dep edge so the human-ratings plan-of-record
	// (gardenio/plans/beads-viewer-human-ratings.md §2 + §5.3) is satisfied at
	// the daemon layer — beads-viewer (T4b) can then issue a plain CreateBead
	// without needing to do its own lookup.
	if bead.Type == "rating" {
		existing, ratesTo, err := s.preCreateRating(ctx, bead, in)
		if err != nil {
			return nil, "", err
		}
		if existing != nil {
			return existing, "", nil
		}
		return nil, ratesTo, nil
	}
	return nil, "", nil
}

// admitLandInviteLocked takes landInviteMu, held across the lookup and the
// insert as gatehouseRequestMu is, so viewer replicas filing at once converge
// (gd-GPvddfQY6y), and runs admitLandInvite.
func (s *BeadsServer) admitLandInviteLocked(ctx context.Context, bead *model.Bead, locks *createLocks) (*model.Bead, error) {
	s.landInviteMu.Lock()
	locks.hold(s.landInviteMu.Unlock)
	return admitLandInvite(ctx, s.store, bead)
}

// prepareInsert runs the last checks and stamps before the insert: the arms
// register, the explicit parent, the agent handle and the spawn-lineage seal.
func (s *BeadsServer) prepareInsert(ctx context.Context, bead *model.Bead, parentBeadID string, plan *createPlan) error {
	// Arms (heraldry.go): the type may bear them, the bead has one project and
	// the project's register does not hold them yet. After authz, so the
	// register cannot be probed, and after the lookup-or-return paths, so a
	// replayed create returns its bead rather than colliding with it.
	if err := s.checkHeraldry(ctx, bead); err != nil {
		return err
	}

	explicitParent, err := s.explicitParentDep(ctx, bead, parentBeadID, plan)
	if err != nil {
		return err
	}
	plan.explicitParent = explicitParent

	// Stable, monotonic agent_handle (ka-0000002870): stamp a human-meaningful
	// `<gardener>-<N>` display handle onto a type=agent bead that carries a
	// resolved gardener_id and has none yet. Placed after authz + the
	// idempotency lookup-or-return so a rejected or deduped create never burns a
	// sequence, and before the insert so the handle lands in the same write.
	// Best-effort (never fails the create) — see maybeAllocateAgentHandle.
	s.maybeAllocateAgentHandle(ctx, bead)

	// A bead an impersonated session creates names the admin behind it, as
	// its author too (impersonation.go).
	stampImpersonatedCreate(ctx, bead)

	// Seal a trigger/schedule an OIDC gardenio-admin session authors so the
	// trigger-engine's spawns for it can be authorized as that admin
	// (spawn_lineage_seal.go). Runs after field validation, on the final fields.
	// A plan's gardener a thread binds after the owner approved the plan is
	// sealed to that approval (spawn_lineage_plan_approval.go).
	// The home garden's steward binding one of its plans' gardeners seals it
	// to the home garden's owner (spawn_lineage_home_steward.go), as is a plan's
	// gardener its thread binds after the owner asked for the plan there
	// (spawn_lineage_thread_ask.go). A release's gardener the cutting run of
	// the previous release's gardener binds is sealed from that gardener
	// (spawn_lineage_release_succession.go).
	if !s.stampSpawnLineage(ctx, bead, true) && !s.stampSpawnLineageFromPlanApproval(ctx, bead) &&
		!s.stampSpawnLineageFromThreadAsk(ctx, bead) && !s.stampSpawnLineageFromReleaseSuccession(ctx, bead) {
		s.stampSpawnLineageFromHomeSteward(ctx, bead)
	}
	return nil
}

// explicitParentDep validates the caller's parent_bead_id and returns the
// parent-child edge the transaction writes, or nil when none was given.
//
// Validate explicit parent target before the transaction so we surface
// `unknown parent` as 400 InvalidArgument rather than a generic FK error
// from the dep insert. Self-parent is also a client mistake (see
// auto-parent self-ref guard). The lookup is a single GetBead by id and
// runs in the same fleet scope as the caller.
func (s *BeadsServer) explicitParentDep(ctx context.Context, bead *model.Bead, parentBeadID string, plan *createPlan) (*model.Dependency, error) {
	if parentBeadID == "" {
		return nil, nil
	}
	if parentBeadID == bead.ID {
		return nil, inputError("parent_bead_id cannot equal the new bead's id")
	}
	parent, err := s.store.GetBead(ctx, parentBeadID)
	if err != nil {
		return nil, fmt.Errorf("failed to look up parent bead %q: %w", parentBeadID, err)
	}
	// A parent a tenant caller cannot see is answered as an unknown one
	// (edgeTargetHidden).
	if s.edgeTargetHidden(ctx, parent) {
		return nil, inputError("parent_bead_id refers to unknown bead " + parentBeadID)
	}
	return plan.dep(bead, parentBeadID, model.DepParentChild), nil
}

// writeCreatedBeadRows writes the new bead's taints, grants, labels and dep
// edges inside createBead's insert transaction, then reads the labels back.
//
// Bug 5 fix: wrap CreateBead + label inserts in a transaction.
//
// R2 (ka-0000002898): fold the born-on-labels verify-readback INTO this
// transaction. Before, callers that needed to trust the persisted label set
// (createPlanEpic's invisible-orphan guard) had to issue a SEPARATE GetBead
// round-trip after the create and re-assert the shape — a window in which
// the returned bead echoed the *input* labels, not what actually landed in
// the labels table. Reading the labels back inside the same tx makes the
// all-or-nothing guarantee a daemon property: the bead we return (and
// publish) carries exactly the labels that were committed, deduped by the
// AddLabel ON CONFLICT the same way a later read would see them. No caller
// can observe a bead whose labels differ from the persisted set.
func writeCreatedBeadRows(ctx context.Context, tx store.Store, bead *model.Bead, plan *createPlan) error {
	if err := applyPendingBeadTaints(ctx, tx, bead.ID, plan.pendingTaints, plan.operatedBy); err != nil {
		return err
	}
	if err := applyPendingAssignGrants(ctx, tx, bead.ID, plan.pendingAssignGrants, plan.now); err != nil {
		return err
	}
	for _, label := range bead.Labels {
		if err := tx.AddLabel(ctx, bead.ID, label); err != nil {
			return fmt.Errorf("failed to add label %q: %w", label, err)
		}
	}
	if plan.ratesEdgeTo != "" {
		if err := tx.AddDependency(ctx, plan.dep(bead, plan.ratesEdgeTo, model.DepRates)); err != nil {
			return fmt.Errorf("failed to add rates dep edge: %w", err)
		}
	}
	if plan.explicitParent != nil {
		if err := tx.AddDependency(ctx, plan.explicitParent); err != nil {
			return fmt.Errorf("failed to add parent-child dep edge: %w", err)
		}
	}
	// Read the persisted labels back within the SAME tx so the returned
	// bead reflects the committed set (deduped), not the raw input echo.
	// This is the atomic verify-readback the viewer's R2 guard used to do
	// out-of-band. GetLabels (not GetBead) keeps it a single cheap SELECT
	// and avoids re-hydrating deps/comments we already hold in-memory.
	persistedLabels, err := tx.GetLabels(ctx, bead.ID)
	if err != nil {
		return fmt.Errorf("reading back labels for created bead %s: %w", bead.ID, err)
	}
	bead.Labels = persistedLabels
	return nil
}

// recoverCreateConflict turns a failed insert into the winner of a create
// race when a unique index says one exists, and otherwise into the error the
// create returns.
func (s *BeadsServer) recoverCreateConflict(ctx context.Context, bead *model.Bead, idempotencyKey string, err error) (*model.Bead, error) {
	// Race backstop: a concurrent create won the insert and migration-029's
	// unique index raised a 23505. Honour "return existing, not error" by
	// re-running the lookup and returning the winner (ka-0000002856).
	if idempotencyKey != "" && isUniqueViolation(err) {
		existing, lookupErr := s.lookupByIdempotencyKey(ctx, bead.Labels, bead.Fleet, idempotencyKey)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if existing != nil {
			return existing, nil
		}
	}
	// type=merge-request race backstop (ka-0000005686 HALF-2): the
	// per-mr_url mutex serialises upserts WITHIN this daemon process,
	// but migration 042 promotes the fields.mr_url index to UNIQUE, so a
	// hypothetical multi-replica daemon (or any path that reached CreateBead
	// without the mutex) can still lose the insert race to a concurrent
	// create and get a 23505 from the unique index. Mirror the
	// idempotency-key path: re-run the by-URL lookup and return the winner
	// verbatim rather than surfacing the raw constraint error. This makes the
	// "exactly one MR bead per mr_url" guarantee correct at the STORE level,
	// not just within one process — the same lookup-or-return contract the
	// preCreateMergeRequest fast-path provides, closed on the losing side of
	// the race. See beads_mr_dedup.go.
	if bead.Type == "merge-request" && isUniqueViolation(err) {
		if mrURL := mrURLFromFields(bead.Fields); mrURL != "" {
			existing, lookupErr := s.lookupMergeRequestByURL(ctx, bead.Fleet, mrURL)
			if lookupErr != nil {
				return nil, lookupErr
			}
			if existing != nil {
				return existing, nil
			}
		}
	}
	// Arms race backstop: a concurrent write took the same arms in this
	// project after checkHeraldry read the register (migration 057's
	// unique index). The same 409 as the pre-write check.
	return nil, s.heraldryRaceConflict(ctx, bead, err)
}

// publishCreated records and publishes the events of a committed create and
// runs the best-effort follow-ups: auto-parent, thread_id copy and the
// opened-by stamp.
func (s *BeadsServer) publishCreated(ctx context.Context, bead *model.Bead, plan *createPlan) {
	if plan.ratesEdgeTo != "" {
		s.recordAndPublish(ctx, events.TopicDependencyAdded, bead.ID, bead.CreatedBy, events.DependencyAdded{
			Dependency: plan.dep(bead, plan.ratesEdgeTo, model.DepRates),
			Fleet:      FleetFromContext(ctx),
		})
	}

	s.recordAndPublish(ctx, events.TopicBeadCreated, bead.ID, bead.CreatedBy, events.BeadCreated{Bead: bead})
	s.recordCreate(ctx, bead.ID, bead.CreatedBy)

	if plan.explicitParent != nil {
		s.recordAndPublish(ctx, events.TopicDependencyAdded, bead.ID, bead.CreatedBy, events.DependencyAdded{
			Dependency: plan.explicitParent,
			Fleet:      FleetFromContext(ctx),
		})
	}

	// Server-side auto-parent (ka-0000007224): when an agent actor (static-
	// token caller whose Subject matches a known agent bead carrying a task_id
	// field) creates a new bead, wire a parent-child dep from the new bead to
	// the agent's current task so derivative work stays attached to the
	// originating task in the bead graph. Skips when the caller opted out via
	// X-No-Auto-Parent: 1, when the agent has no task_id, when the new bead is
	// itself the task, when the dep would be self-referential, or — D5,
	// ka-0000007622 — when the caller passed an explicit parent_bead_id at
	// create time (the caller's parent edge is the canonical lineage and we
	// must not stack a second auto-parent edge on top). Non-fatal: a
	// missing/invalid task_id is logged and swallowed so the bead create
	// itself never fails on the auto-parent path.
	if plan.explicitParent == nil {
		s.maybeAddAutoParent(ctx, bead, plan.now)
	}

	// Transitive thread_id (wish-membership) copy — ka-0000007300. Runs
	// regardless of whether an explicit parent was supplied: the explicit
	// parent is the cross-wish guard input (a bead parented under a different
	// wish must NOT inherit the acting agent's wish), so it cannot be skipped
	// in the explicit-parent branch the way the auto-parent dep is (D5). The
	// copy is a best-effort net-new field mutation, decoupled from the dep
	// above so neither failure undoes the other.
	s.maybeCopyThreadID(ctx, bead, plan.explicitParent, plan.now)
	// Who opened that thread, for a local runner's gb ready (spec §7 rule 2).
	s.maybeStampOpenedByOperator(ctx, bead, plan.now)

	if bead.Type == model.TypeAdvice {
		s.recordAndPublish(ctx, events.TopicAdviceCreated, bead.ID, bead.CreatedBy, events.AdviceCreated{Bead: bead})
	}

	if bead.Type == "mail" {
		s.recordAndPublish(ctx, events.TopicMailSent, bead.ID, bead.CreatedBy, events.MailSent{
			BeadID:  bead.ID,
			Channel: channelFromLabels(bead.Labels),
			From:    bead.CreatedBy,
			To:      bead.Assignee,
			Subject: bead.Title,
		})
	}
}
