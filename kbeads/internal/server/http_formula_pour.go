package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/formula"
	"gardenio/kbeads/internal/model"
)

// pourFormulaRequest is the JSON body for POST /v1/formulas/{id}/pour. It
// mirrors the kd CLI's formulaApplyOpts (`kd formula pour|wisp|apply`) so the
// endpoint and the CLI accept the same knobs.
type pourFormulaRequest struct {
	// Vars are the {{variable}} substitutions. Defaults from the formula's
	// var definitions fill any omitted names; required vars must be present.
	Vars map[string]string `json:"vars,omitempty"`
	// Labels are merged with the formula bead's labels onto the molecule and
	// every step (project/role scope propagation).
	Labels []string `json:"labels,omitempty"`
	// Assignee is the default assignee for created beads. Falls back to the
	// formula's assigned_agent field when empty.
	Assignee string `json:"assignee,omitempty"`
	// Ephemeral marks the molecule as a wisp (vapor phase, GC-eligible).
	Ephemeral bool `json:"ephemeral,omitempty"`
	// Force overrides the duplicate-molecule and unresolved-placeholder guards.
	Force bool `json:"force,omitempty"`
	// DryRun expands the formula and returns the step plan without creating
	// any beads — backs a confirm-before-pour UI.
	DryRun bool `json:"dry_run,omitempty"`
	// TriggerBeadIDs / SourceBeadIDs record provenance: each becomes a
	// triggered-by dep from the molecule (D1 ka-0000005967).
	TriggerBeadIDs []string `json:"trigger_bead_ids,omitempty"`
	SourceBeadIDs  []string `json:"source_bead_ids,omitempty"`
	// TargetPlan, when set, EMPLACES the poured molecule as a phase of the named
	// plan (epic ka-0000000759): the molecule is parented to the plan epic and
	// its id appended to the plan's phase_order, so the /plans cockpit renders it
	// as a first-class phase whose steps are the molecule's step children. Blank
	// = today's freestanding molecule (no plan membership). The container stays
	// type=molecule — the viewer's isPhaseContainer predicate accepts it as a
	// phase (resolved decision ka-0000003759). Ignored on a dry run.
	TargetPlan string `json:"target_plan,omitempty"`
	// TargetPhaseTitle overrides the poured molecule's title (ka-0000003236). The
	// molecule's title is what renders as the PHASE title when emplaced into a
	// plan (plan_phases.go reads the container's Title), so this is the pour form's
	// "Phase title" field: blank defaults to the var-substituted formula title,
	// a non-blank value names the phase explicitly ("Security review" rather than
	// "sec-review — Security review sequence"). Trimmed; blank/whitespace-only is
	// ignored (keeps the formula-title default). Applies to freestanding pours too
	// — it simply names the molecule — but its reason for being is the phase title.
	// Ignored on a dry run (no molecule is created).
	TargetPhaseTitle string `json:"target_phase_title,omitempty"`
	// TargetPosition, when set, EMPLACES the poured phase at this 0-based slot in
	// the target plan's phase_order instead of appending it at the end — the
	// "target cell" generalization (ka-0000001001). 0 makes the pour the FIRST
	// phase; a value equal to the current phase count is exactly a tail append
	// (the default when nil). Requires target_plan; a position without a plan is
	// rejected. Must be in [0, len(phase_order)] — a slot past the end is
	// rejected rather than silently clamped, so a stale UI index surfaces as an
	// error instead of quietly landing elsewhere. Ignored on a dry run (nothing
	// is emplaced). nil = today's tail append, byte-identical to before.
	TargetPosition *int `json:"target_position,omitempty"`
}

// pourPlanStep is one step in the dry-run plan (and the created-step echo).
type pourPlanStep struct {
	ID        string         `json:"id"`
	Title     string         `json:"title"`
	Type      string         `json:"type"`
	DependsOn []string       `json:"depends_on,omitempty"`
	BeadID    string         `json:"bead_id,omitempty"` // set on create (not dry-run)
	Children  []pourPlanStep `json:"children,omitempty"`
}

// pourFormulaResponse is the JSON response for POST /v1/formulas/{id}/pour.
type pourFormulaResponse struct {
	// Molecule is the created root bead (nil on dry-run).
	Molecule *model.Bead `json:"molecule,omitempty"`
	// Steps maps step ID → created bead ID (empty on dry-run).
	Steps map[string]string `json:"steps,omitempty"`
	// Plan is the expanded step tree (always set).
	Plan []pourPlanStep `json:"plan"`
	// TotalSteps counts the flattened plan.
	TotalSteps int `json:"total_steps"`
	// DryRun echoes the request flag.
	DryRun bool `json:"dry_run,omitempty"`
	// Warning carries non-fatal notices (phase mismatch, forced dedup).
	Warning string `json:"warning,omitempty"`
}

// storeFormulaGetter adapts the server's store to the shared formula engine's
// Getter (fetching inheritance parents / expansion targets / aspects).
type storeFormulaGetter struct{ s *BeadsServer }

func (g storeFormulaGetter) GetFormulaBead(ctx context.Context, id string) (*formula.Bead, error) {
	b, err := g.s.store.GetBead(ctx, id)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("bead %s not found", id)
	}
	return &formula.Bead{
		ID:          b.ID,
		Type:        string(b.Type),
		Title:       b.Title,
		Description: b.Description,
		Priority:    b.Priority,
		Labels:      b.Labels,
		Fields:      b.Fields,
	}, nil
}

// handlePourFormula handles POST /v1/formulas/{id}/pour — the daemon-side
// formula instantiation endpoint (ka-0000000251). It runs the same
// resolve → expand → create pipeline as `kd formula pour` (shared
// internal/formula engine), but server-side in one request, so HTTP callers
// (beads-viewer's operator-gated Pour button, bridges) don't need to
// orchestrate N create/dep calls client-side. Bead creation composes
// s.createBead, so attribution (AUDIT §C3), authz, type validation, events,
// and auto-inherit all apply exactly as they do for individual creates.
func (s *BeadsServer) handlePourFormula(w http.ResponseWriter, r *http.Request) {
	formulaID := r.PathValue("id")
	if formulaID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	var req pourFormulaRequest
	if !decodeJSONBodyOptional(w, r, &req) {
		return
	}
	ctx := r.Context()

	// ── Resolve ─────────────────────────────────────────────
	bead, err := s.store.GetBead(ctx, formulaID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && bead == nil) {
		writeError(w, http.StatusNotFound, "formula not found")
		return
	}
	if err != nil {
		writeServerError(ctx, w, "handlePourFormula", err, "failed to load formula")
		return
	}
	// A bead the caller cannot read answers as missing, so neither refusal
	// below names its type or its project (beadHiddenFromCaller).
	if string(bead.Type) != "formula" || !beadMatchesTokenProject(ctx, bead.Labels) {
		switch {
		case s.beadHiddenFromCaller(ctx, bead):
			writeError(w, http.StatusNotFound, "formula not found")
		case string(bead.Type) != "formula":
			writeError(w, http.StatusBadRequest, fmt.Sprintf("bead %s is type %q, not formula", formulaID, bead.Type))
		default:
			writeError(w, http.StatusForbidden, "token not authorized for this bead's project")
		}
		return
	}

	fields, err := formula.ParseFields(bead.Fields)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	getter := storeFormulaGetter{s}
	if len(fields.Extends) > 0 {
		fields, err = formula.ResolveInheritance(ctx, getter, formulaID, fields)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("resolving inheritance: %v", err))
			return
		}
	}
	if len(fields.Steps) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("formula %s has no steps", formulaID))
		return
	}
	if err := formula.RejectNonWorkflow(formulaID, fields); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(fields.Aspects) > 0 {
		fields.Steps, err = formula.ApplyAspects(ctx, getter, fields.Aspects, fields.Steps)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("applying aspects: %v", err))
			return
		}
	}

	// Phase-mismatch is a warning (like the CLI), never a block.
	var warning string
	if fields.Phase == "vapor" && !req.Ephemeral {
		warning = "formula recommends wisp (vapor phase); consider ephemeral=true"
	} else if fields.Phase == "liquid" && req.Ephemeral {
		warning = "formula recommends pour (liquid phase); consider ephemeral=false"
	}

	assignee := req.Assignee
	if assignee == "" && fields.AssignedAgent != "" {
		assignee = fields.AssignedAgent
	}

	vars := make(map[string]string, len(req.Vars))
	for k, v := range req.Vars {
		vars[k] = v
	}
	if err := formula.ApplyVarDefaults(fields, vars); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Unresolved-placeholder guard (ka-0000006473): a var VALUE still carrying
	// {{...}} would bake the literal template into every step title.
	if bad := formula.UnresolvedVarValues(vars); len(bad) > 0 && !req.Force {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("variable value(s) contain unresolved {{...}} placeholders: %v (resolve them, or set force=true)", bad))
		return
	}

	// ── Dedup guard ─────────────────────────────────────────
	// Reject when an open molecule from this formula already carries identical
	// applied_vars (force=true overrides), mirroring the CLI.
	if !req.DryRun {
		existing, _, derr := s.store.ListBeads(ctx, model.BeadFilter{
			Type:   []model.BeadType{"molecule"},
			Status: []model.Status{model.StatusOpen, model.StatusInProgress},
			Fields: map[string]string{"formula_id": formulaID},
			Limit:  200,
		})
		if derr == nil {
			for _, b := range existing {
				if b == nil || len(b.Fields) == 0 {
					continue
				}
				var f struct {
					AppliedVars map[string]string `json:"applied_vars"`
				}
				if json.Unmarshal(b.Fields, &f) != nil {
					continue
				}
				if formula.MapsEqual(f.AppliedVars, vars) {
					if !req.Force {
						writeError(w, http.StatusConflict,
							fmt.Sprintf("duplicate molecule detected: %s (%s) already open with identical vars (set force=true to override)", b.ID, b.Title))
						return
					}
					if warning != "" {
						warning += "; "
					}
					warning += "duplicate molecule " + b.ID + " exists (forced)"
					break
				}
			}
		}
	}

	// ── Expand ──────────────────────────────────────────────
	active, err := formula.ExpandSteps(ctx, getter, fields.Steps, vars)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("expanding steps: %v", err))
		return
	}
	if len(active) == 0 {
		writeError(w, http.StatusBadRequest, "all steps were filtered by conditions; nothing to create")
		return
	}

	// Fail loudly BEFORE creating any beads if a step's depends_on names a step
	// that will not exist — otherwise the blocks-edge pass below silently drops
	// that edge and the molecule pours ungated (ka-0000002771). Checked on dry-run
	// too so a preview call surfaces the same authoring bug.
	if err := formula.ValidateDependencies(active); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Target-plan emplacement guard (ka-0000004940): when a plan is named, verify
	// it is a real plan epic BEFORE creating any beads, so a bad target fails the
	// pour cleanly rather than orphaning a freestanding molecule. Skipped on a dry
	// run (no beads created, nothing to emplace).
	// target_position without a target_plan has nowhere to land — a caller
	// mistake we surface rather than silently drop (checked before any bead is
	// created, and independent of dry-run so a preview call catches it too).
	if req.TargetPosition != nil && req.TargetPlan == "" {
		writeError(w, http.StatusBadRequest, "target_position requires target_plan")
		return
	}

	var targetPlan *model.Bead
	if tp := req.TargetPlan; tp != "" && !req.DryRun {
		targetPlan, err = s.store.GetBead(ctx, tp)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && targetPlan == nil) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("target_plan %s not found", tp))
			return
		}
		if err != nil {
			writeServerError(ctx, w, "handlePourFormula", err, "failed to load target plan")
			return
		}
		if targetPlan.Type != model.TypeEpic || !hasLabel(targetPlan.Labels, planMarkerLabel) {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("target_plan %s is not a plan (needs a type=epic bead carrying the %q label)", tp, planMarkerLabel))
			return
		}
		// Closed-plan guard: a completed plan is done — never emplace a new phase
		// into it (mirrors the add-step / plan-step-signal closed-plan fail-close,
		// ka-0000000753). Reopen the plan first if the work really belongs to it.
		if targetPlan.Status == model.StatusClosed {
			writeError(w, http.StatusConflict,
				fmt.Sprintf("target_plan %s is closed — reopen it before emplacing a new phase", tp))
			return
		}
		if !beadMatchesTokenProject(ctx, targetPlan.Labels) {
			writeError(w, http.StatusForbidden, "token not authorized for the target plan's project")
			return
		}
		// target_position range guard: the new phase may land anywhere from the
		// head (0) to a tail append (== current phase count). A slot past the end
		// is a stale-index / caller mistake — reject it rather than clamp, so a UI
		// working from an out-of-date phase list gets a clear error instead of a
		// phase quietly appearing somewhere other than where the operator aimed.
		if pos := req.TargetPosition; pos != nil {
			phaseCount := len(planPhaseOrder(targetPlan))
			if *pos < 0 || *pos > phaseCount {
				writeError(w, http.StatusBadRequest,
					fmt.Sprintf("target_position %d is out of range: plan %s has %d phase(s), so valid positions are 0..%d", *pos, tp, phaseCount, phaseCount))
				return
			}
		}
	}

	resp := pourFormulaResponse{
		Plan:       buildPourPlan(active, nil),
		TotalSteps: formula.CountSteps(active),
		DryRun:     req.DryRun,
		Warning:    warning,
	}
	if req.DryRun {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// ── Create ──────────────────────────────────────────────
	// The molecule title defaults to the var-substituted formula title; a
	// non-blank target_phase_title overrides it (ka-0000003236). Because the
	// container's title is what renders as the phase title when emplaced into a
	// plan, this lets the pour form name the phase ("Security review") rather than
	// carry the recipe's full title. Whitespace-only is treated as absent so a
	// stray space never blanks the title.
	molTitle := formula.SubstituteVars(bead.Title, vars)
	if override := strings.TrimSpace(req.TargetPhaseTitle); override != "" {
		molTitle = override
	}
	appliedVarsJSON, _ := json.Marshal(vars)
	molFields := map[string]any{
		"formula_id":   formulaID,
		"applied_vars": json.RawMessage(appliedVarsJSON),
	}
	if req.Ephemeral {
		molFields["ephemeral"] = true
	}
	if len(req.TriggerBeadIDs) > 0 {
		molFields["trigger_bead_ids"] = req.TriggerBeadIDs
	}
	if len(req.SourceBeadIDs) > 0 {
		molFields["source_bead_ids"] = req.SourceBeadIDs
	}
	molFieldsJSON, _ := json.Marshal(molFields)
	molLabels := formula.MergeLabels(bead.Labels, req.Labels)

	// Emplacement (ka-0000004940): a target plan parents the molecule to the plan
	// epic in the SAME createBead transaction (D5, ka-0000007622) as the molecule
	// itself, so the parent-child edge and the molecule are all-or-nothing. The
	// phase_order append below is a SEPARATE follow-on write (a different bead), so
	// the two are not one transaction — see the append comment for the residual
	// state on its failure. molParent is "" for a freestanding pour.
	molParent := ""
	if targetPlan != nil {
		molParent = targetPlan.ID
	}

	// Idempotency (ka-0000005875): stamp a deterministic key on the molecule (and,
	// below, on each step) so a double-submit — an htmx double-fire, a browser
	// retry, or two concurrent requests both slipping past the open-molecule dedup
	// guard's TOCTOU window — collapses to ONE molecule + ONE step tree via
	// createBead's lookup-or-return (migration 029) instead of double-pouring. The
	// key hashes the same identity the dedup guard compares (formula id + canonical
	// applied vars), so it is that guard made race-safe. force=true means the caller
	// deliberately wants a duplicate molecule (it just overrode the dedup guard
	// above), so we mint NO key and let the create insert unconditionally.
	molKey := ""
	if !req.Force {
		molKey = pourIdempotencyKey(formulaID, vars, time.Now())
	}
	// short_title (gd-4Nrk1IDadk): molecule is not a gated type, but the
	// molecule renders as a plan phase, so it gets a name cut from its title.
	mol, err := s.createBead(ctx, createBeadInput{
		Title:          molTitle,
		ShortTitle:     formula.ShortTitle("", molTitle),
		Description:    formula.SubstituteVars(bead.Description, vars),
		Type:           "molecule",
		Priority:       bead.Priority,
		Labels:         molLabels,
		Assignee:       assignee,
		Fields:         molFieldsJSON,
		ParentBeadID:   molParent,
		IdempotencyKey: molKey,
	})
	if err != nil {
		writePourCreateError(ctx, w, r, s, err, "creating molecule")
		return
	}

	// Emplace the molecule in the plan's phase_order so it RENDERS as a phase —
	// the membership rule every phase-scan site enforces (ka-0000001034). The
	// parent-child edge above makes it a direct child; phase_order makes it a
	// phase. Both primitives are race-safe append-if-absent (ka-0000000988 /
	// ka-0000001001), so a concurrent phase add can never clobber this one:
	//   - no target_position → AppendBeadArrayField (tail append, byte-identical
	//     to before this generalization).
	//   - target_position=N  → InsertBeadArrayField at slot N (the "target cell":
	//     land the phase where the operator aimed, not always at the end). The
	//     range was validated above against the phase_order read at guard time;
	//     the insert clamps defensively, and a concurrent append that grew the
	//     list between guard and here still lands the phase at N (shifting the
	//     newcomer right) — the operator's chosen slot wins, which is the intent.
	//
	// This is a second write, AFTER the molecule create committed. If it fails
	// (transient store error) we 500, and the molecule + its steps exist parented
	// to the plan but ABSENT from phase_order — a direct child that isPhaseContainer
	// skips, so it renders as nothing until recovered. Recovery is a plain re-append
	// (idempotent append-if-absent) via a retried pour with force, or a direct
	// phase_order edit; the molecule is never lost, only unrendered.
	if targetPlan != nil {
		var aerr error
		if req.TargetPosition != nil {
			_, _, aerr = s.store.InsertBeadArrayField(ctx, targetPlan.ID, "phase_order", mol.ID, *req.TargetPosition)
		} else {
			_, _, aerr = s.store.AppendBeadArrayField(ctx, targetPlan.ID, "phase_order", mol.ID)
		}
		if aerr != nil {
			writeServerError(ctx, w, "handlePourFormula", aerr, "failed to emplace molecule in plan phase_order")
			return
		}
	}

	// Triggered-by lineage edges (D1 ka-0000005967 / D2 ka-0000004959).
	for _, tb := range append(append([]string{}, req.TriggerBeadIDs...), req.SourceBeadIDs...) {
		if tb == "" {
			continue
		}
		if err := s.addPourDependency(ctx, mol.ID, tb, model.DepTriggeredBy); err != nil {
			writeServerError(ctx, w, "handlePourFormula", err, "failed to add triggered-by dependency")
			return
		}
	}

	// Create step beads (recursively for nested children).
	//
	// Taint propagation (ADR docs/adr/taints.md §4.2 row 6: "formula pour /
	// molecule expansion | children get the molecule's taints |
	// inherit:parent") needs no bespoke code here — every step create below
	// passes parentBeadID (the molecule for a top-level step, the parent
	// step for a nested one), and that ParentBeadID wiring already runs
	// through resolveCreateTaints per row 1 (taint_propagate_create.go).
	// Because the molecule's own create call (above) has already committed
	// by the time this runs, a molecule tainted at pour time — by the
	// pouring agent (inherit:create) or by a tainted target_plan
	// (inherit:parent) — is already visible to GetTaintsForBead, so it
	// flows down the whole step tree automatically, one hop per level.
	stepBeadIDs := make(map[string]string)
	var createSteps func(steps []formula.ExpandedStep, parentBeadID string) error
	createSteps = func(steps []formula.ExpandedStep, parentBeadID string) error {
		for _, st := range steps {
			typ := st.Type
			if typ == "" {
				typ = "task"
			}
			stepAssignee := st.Assignee
			if stepAssignee == "" {
				stepAssignee = assignee
			}

			stepRoles := st.Roles
			if len(stepRoles) == 0 {
				stepRoles = fields.DefaultRoles
			}
			var roleLabels []string
			for _, role := range stepRoles {
				roleLabels = append(roleLabels, "role:"+formula.SubstituteVars(role, vars))
			}

			// Substitute {{vars}} in raw step labels too (ka-0000005050).
			stepLabels := make([]string, len(st.Labels))
			for i, l := range st.Labels {
				stepLabels[i] = formula.SubstituteVars(l, vars)
			}

			stepSkills := st.RequiresSkills
			if len(stepSkills) == 0 {
				stepSkills = fields.RequiresSkills
			}
			var skillLabels []string
			for _, sk := range stepSkills {
				skillLabels = append(skillLabels, "skill:"+formula.SubstituteVars(sk, vars))
			}

			stepFields := map[string]any{
				"source_formula": formulaID,
				"source_step_id": st.ID,
			}
			// Step-role dispatch opt-in (ka-0000004970). The controller's
			// FormulaStepDispatcher (formula_step_dispatch.go) spawns a
			// role-matched agent when a step's last blocking dep clears, but
			// ONLY if the step bead carries suggest_new_agent=true — that field
			// is the hard eligibility gate (stepDispatchEligible). The bridge
			// pour path (formula_instantiate.go) has always stamped it; this
			// daemon HTTP pour (backing beads-viewer + `gb plan pour`) never
			// did, so a molecule poured into a plan could never dispatch,
			// regardless of what the formula declared. Mirror the bridge stamps
			// here so plan-poured molecules dispatch identically.
			//
			// suggest_new_agent is written as a native JSON boolean (the
			// schema type registered in config.go) — createBead validates it
			// at create time, unlike a string PATCH which 400s at the daemon
			// validator (ka-0000002741). Unlike the bridge, this path has no
			// cross-pour edges, so there is no deferred opt-in: every opted-in
			// step is stamped at create, matching the bridge's intra-molecule
			// branch.
			if st.SuggestNewAgent {
				stepFields["suggest_new_agent"] = true
			}
			// step_role is the resolved dispatch target the dispatcher reads
			// (stepDispatchRole) so it never has to re-parse role: labels;
			// molecule_id scopes the spawn. Both mirror the bridge pour. The
			// first non-empty role wins — a pod runs exactly one role, and the
			// dispatcher only ever spawns for the first (the rest stay labels).
			for _, role := range stepRoles {
				if r := strings.TrimSpace(formula.SubstituteVars(role, vars)); r != "" {
					stepFields["step_role"] = r
					break
				}
			}
			stepFields["molecule_id"] = mol.ID
			if st.Gate != nil {
				stepFields["gate_type"] = st.Gate.Type
				if st.Gate.Timeout != "" {
					stepFields["timeout"] = st.Gate.Timeout
				}
				if st.Gate.Type == "decision" {
					typ = "decision"
					stepFields["prompt"] = st.Gate.Prompt
					if len(st.Gate.Options) > 0 {
						stepFields["options"] = st.Gate.Options
					}
				}
			}
			if st.WaitsFor != "" {
				stepFields["waits_for"] = st.WaitsFor
			}
			if st.OnComplete != nil {
				stepFields["on_complete"] = map[string]any{
					"for_each":    st.OnComplete.ForEach,
					"var":         st.OnComplete.Var,
					"expand":      st.OnComplete.Expand,
					"expand_vars": st.OnComplete.ExpandVars,
				}
			}
			stepFieldsJSON, _ := json.Marshal(stepFields)

			// ParentBeadID wires the molecule/parent-step parent-child dep
			// transactionally inside createBead (D5, ka-0000007622).
			// IdempotencyKey (ka-0000005875) is derived from the molecule key so a
			// re-submitted pour returns the existing step rather than duplicating it
			// under the (idempotently reused) molecule; empty molKey (force=true)
			// yields an empty step key, so a forced pour's steps are all new.
			// ShortTitle (gd-4Nrk1IDadk): task and decision are gated types, so
			// a step without one would fail the whole pour under
			// BEADS_SHORT_TITLE_GATE=enforce; the step's declared short_title
			// wins, else one is cut from its title.
			stepBead, err := s.createBead(ctx, createBeadInput{
				Title:          st.Title,
				ShortTitle:     formula.ShortTitle(st.ShortTitle, st.Title),
				Description:    st.Description,
				Type:           typ,
				Priority:       formula.PriorityOrDefault(st.Priority, bead.Priority),
				Labels:         formula.MergeLabels(formula.MergeLabels(formula.MergeLabels(molLabels, stepLabels), roleLabels), skillLabels),
				Assignee:       stepAssignee,
				Fields:         stepFieldsJSON,
				ParentBeadID:   parentBeadID,
				IdempotencyKey: stepIdempotencyKey(molKey, st.ID),
			})
			if err != nil {
				return fmt.Errorf("creating step %q: %w", st.ID, err)
			}
			stepBeadIDs[st.ID] = stepBead.ID

			if len(st.Children) > 0 {
				if err := createSteps(st.Children, stepBead.ID); err != nil {
					return err
				}
				// waits_for=all-children: parent step blocked by all children.
				if st.WaitsFor == "all-children" {
					for _, child := range st.Children {
						childBeadID, ok := stepBeadIDs[child.ID]
						if !ok {
							continue
						}
						if err := s.addPourDependency(ctx, stepBead.ID, childBeadID, model.DepBlocks); err != nil {
							return fmt.Errorf("adding waits_for dep %s→%s: %w", st.ID, child.ID, err)
						}
					}
				}
			}
		}
		return nil
	}
	if err := createSteps(active, mol.ID); err != nil {
		writeServerError(ctx, w, "handlePourFormula", err, "failed to create formula steps")
		return
	}

	// Blocks dependencies between steps (across all levels).
	var addBlocksDeps func(steps []formula.ExpandedStep) error
	addBlocksDeps = func(steps []formula.ExpandedStep) error {
		for _, st := range steps {
			for _, depStepID := range st.DependsOn {
				depBeadID, ok := stepBeadIDs[depStepID]
				if !ok {
					// ValidateDependencies above already rejects dangling refs
					// before any bead is created; reaching here means the created
					// step set diverged from the validated tree, so fail loudly
					// rather than silently dropping the gating edge (ka-0000002771).
					return fmt.Errorf("step %q depends_on %q, which was not created — refusing to pour an ungated molecule", st.ID, depStepID)
				}
				if err := s.addPourDependency(ctx, stepBeadIDs[st.ID], depBeadID, model.DepBlocks); err != nil {
					return fmt.Errorf("adding dependency %s→%s: %w", st.ID, depStepID, err)
				}
			}
			if err := addBlocksDeps(st.Children); err != nil {
				return err
			}
		}
		return nil
	}
	if err := addBlocksDeps(active); err != nil {
		writeServerError(ctx, w, "handlePourFormula", err, "failed to add step dependencies")
		return
	}

	resp.Molecule = mol
	resp.Steps = stepBeadIDs
	resp.Plan = buildPourPlan(active, stepBeadIDs)
	writeJSON(w, http.StatusCreated, resp)
}

// addPourDependency inserts one dep edge with the same attribution, event, and
// mutation-audit side effects as POST /v1/beads/{id}/dependencies.
func (s *BeadsServer) addPourDependency(ctx context.Context, beadID, dependsOnID string, depType model.DependencyType) error {
	operatedBy, operatedByID, _, ok := s.resolveAuditOperatorRequired(ctx)
	if !ok {
		return newOperatorRefusal(ctx)
	}
	dep := &model.Dependency{
		BeadID:       beadID,
		DependsOnID:  dependsOnID,
		Type:         depType,
		CreatedAt:    time.Now().UTC(),
		CreatedBy:    actorFromContext(ctx),
		OperatedBy:   operatedBy,
		OperatedByID: &operatedByID,
	}
	if err := s.addDependencyWithTaintPropagation(ctx, dep); err != nil {
		return err
	}
	s.recordAndPublish(ctx, events.TopicDependencyAdded, dep.BeadID, dep.CreatedBy, events.DependencyAdded{Dependency: dep, Fleet: FleetFromContext(ctx)})
	s.recordDependencyMutation(ctx, dep.BeadID, dep.DependsOnID, string(dep.Type), dep.CreatedBy, model.MutationTypeCreate)
	return nil
}

// pourDedupWindow is the coarse time bucket a pour's idempotency key folds the
// wall clock into (ka-0000005875). Two submits within the same window derive the
// SAME key and collapse to one molecule; a legitimate re-pour after the window
// (e.g. re-running a phase once its molecule has closed) lands in a later bucket
// and gets a fresh key. It only needs to comfortably exceed the double-submit /
// retry window (htmx double-fire, double-click, a browser retry, or the sub-
// second gap of two concurrent requests both slipping past the open-molecule
// dedup guard); a minute is generous for that while barely constraining a
// deliberate re-pour. Mirrors operatorTurnHash's 3s window (plans_assistant_prompt.go),
// scaled up for a heavier, retry-prone operation.
const pourDedupWindow = time.Minute

// pourIdempotencyKey derives the deterministic dedup key stamped on a pour's
// molecule create so a double-submit collapses to ONE molecule via createBead's
// lookup-or-return (migration 029) instead of double-pouring — the atomic
// backstop for the open-molecule dedup guard's TOCTOU race (two concurrent
// requests both read "no duplicate" before either commits). The key hashes the
// SAME identity that guard compares — the formula id and the canonical
// (sorted) applied vars — so it is exactly that guard made race-safe, plus a
// coarse time bucket (pourDedupWindow) so a later re-pour is not permanently
// pinned to the first molecule. now is passed in so the derivation is a pure,
// testable function.
func pourIdempotencyKey(formulaID string, vars map[string]string, now time.Time) string {
	h := sha256.New()
	_, _ = h.Write([]byte(formulaID))
	_, _ = h.Write([]byte{0})
	// Canonical applied_vars: sort keys so map iteration order can't change the
	// digest between two otherwise-identical submits.
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(vars[k]))
		_, _ = h.Write([]byte{0})
	}
	bucket := now.UTC().UnixNano() / int64(pourDedupWindow)
	_, _ = h.Write([]byte(strconv.FormatInt(bucket, 10)))
	return "pour:" + formulaID + ":" + hex.EncodeToString(h.Sum(nil))
}

// stepIdempotencyKey derives a step's idempotency key from the molecule key so
// every step create in a re-submitted pour also lookup-or-returns instead of
// duplicating. This is essential, not cosmetic: in a concurrent double-submit
// the losing request's molecule create returns the WINNER's molecule (idempotent
// hit), and the loser then walks createSteps against that shared molecule id —
// without per-step keys it would parent a second full step tree onto the winner's
// molecule. Step ids are unique within an expansion (nested/expanded steps carry
// prefixed ids), so molKey + step id is a stable per-step key. Empty molKey
// (force=true) yields empty here too, so a forced pour's steps are all genuinely
// new — never resolving to a prior forced pour's step beads.
func stepIdempotencyKey(molKey, stepID string) string {
	if molKey == "" {
		return ""
	}
	return molKey + ":step:" + stepID
}

// buildPourPlan projects the expanded step tree into the response plan shape,
// attaching created bead ids when available.
func buildPourPlan(steps []formula.ExpandedStep, beadIDs map[string]string) []pourPlanStep {
	out := make([]pourPlanStep, 0, len(steps))
	for _, s := range steps {
		typ := s.Type
		if typ == "" {
			typ = "task"
		}
		ps := pourPlanStep{
			ID:        s.ID,
			Title:     s.Title,
			Type:      typ,
			DependsOn: s.DependsOn,
		}
		if beadIDs != nil {
			ps.BeadID = beadIDs[s.ID]
		}
		if len(s.Children) > 0 {
			ps.Children = buildPourPlan(s.Children, beadIDs)
		}
		out = append(out, ps)
	}
	return out
}

// writePourCreateError maps createBead's error taxonomy (authz denial,
// operator refusal, input error) onto HTTP the same way handleCreateBead does.
func writePourCreateError(ctx context.Context, w http.ResponseWriter, r *http.Request, s *BeadsServer, err error, what string) {
	var denial *AuthzDenial
	if errors.As(err, &denial) {
		s.auditDenial(r, denial, "")
		writeAuthzDenial(w, denial)
		return
	}
	var refusal *operatorRefusalError
	if errors.As(err, &refusal) {
		writeOperatorRefusal(w, refusal.Source)
		return
	}
	var ie inputError
	if errors.As(err, &ie) {
		writeError(w, http.StatusBadRequest, what+": "+ie.Error())
		return
	}
	writeServerError(ctx, w, "handlePourFormula", err, "failed to pour formula")
}
