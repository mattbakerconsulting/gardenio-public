package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"gardenio/controller/internal/beadsapi"
)

// formulaInstantiator is the subset of bead-daemon methods needed to
// materialize a formula into a molecule + step beads. Both *beadsapi.Client
// (via BeadClient) and the trigger-engine's TriggerBeadClient satisfy it.
type formulaInstantiator interface {
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	// UpdateBeadFieldsTyped patches fields with their proper JSON types. The
	// cross-pour path needs it to stamp the BOOLEAN suggest_new_agent field
	// AFTER the entry step's cross-pour edge lands: a string PATCH of a
	// boolean-typed field 400s at the daemon's field validator (the exact class
	// that broke the pour in ka-0000002741), so the deferred opt-in must go
	// through the typed writer.
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
}

// instantiateFormula creates the root molecule bead and one child bead per
// step, wiring parent-child and blocks deps. It is the shared core used by
// both Slack `/formula pour` (Bot.instantiateFormulaSteps) and the
// apply_formula trigger action (actionApplyFormula).
//
// Lineage is the caller's responsibility: the function does not write any
// `triggered-by` deps itself. Callers stamp their own edges after the
// molecule ID is returned (e.g. slack-pour writes one edge to the thread
// bead; the trigger action writes edges to BOTH the trigger and source
// beads via writeTriggerLineage).
//
// crossPourUpstreamStep, when non-empty, is the bead ID of an UPSTREAM
// molecule's emit step. Every ENTRY step of this pour (a step with no
// intra-molecule blocker — "step 1") gets a `blocks` edge onto it, so the
// merged FormulaStepDispatcher handles the cross-molecule handoff uniformly:
// closing the upstream emit step unblocks this molecule's step 1 and the
// dispatcher spawns a role-matched agent onto it, with no live driver needed
// (decision ka-0000007596, plan exit criterion ka-0000007685).
//
// The edge MUST be typed `blocks` — the dispatcher (formula_step_dispatch.go)
// and the ready-queue both hard-code {blocks, blocked-by, depends-on}; any
// other type is a silent no-op that neither blocks nor dispatches.
//
// Returns the molecule bead ID and the number of step beads successfully
// created (so callers can report partial progress on error).
func instantiateFormula(
	ctx context.Context,
	daemon formulaInstantiator,
	logger *slog.Logger,
	formula *beadsapi.BeadDetail,
	steps []formulaStep,
	vars map[string]string,
	ephemeral bool,
	project string,
	createdBy string,
	crossPourUpstreamStep string,
) (string, int, error) {
	if logger == nil {
		logger = slog.Default()
	}

	molTitle := formulaSubstituteVars(formula.Title, vars)
	appliedVarsJSON, _ := json.Marshal(vars)
	molFields := map[string]any{
		"formula_id":   formula.ID,
		"applied_vars": json.RawMessage(appliedVarsJSON),
	}
	if ephemeral {
		molFields["ephemeral"] = true
	}
	molFieldsJSON, _ := json.Marshal(molFields)

	var labels []string
	if project != "" {
		labels = []string{"project:" + project}
	}

	molID, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       molTitle,
		Description: formulaSubstituteVars(formula.Description, vars),
		Type:        "molecule",
		Priority:    formula.Priority,
		Labels:      labels,
		Fields:      json.RawMessage(molFieldsJSON),
		CreatedBy:   createdBy,
	})
	if err != nil {
		return "", 0, fmt.Errorf("creating molecule %q: %w", molTitle, err)
	}

	stepBeadIDs := make(map[string]string, len(steps))
	crossProjectSteps := make(map[string]map[string]string)
	// deferOptIn collects entry-step formula IDs whose suggest_new_agent stamp
	// is intentionally WITHHELD from the initial create and applied only after
	// the cross-pour blocking edge lands (constraint 5, ka-0000007823). Both
	// pour impls create every step bead first and every blocks edge after, so a
	// step created already carrying suggest_new_agent=true is a live backfill
	// candidate in the gap before its blocker exists — the ≤2-minute
	// STEP_BACKFILL_INTERVAL window that would dispatch step 1 early and defeat
	// the whole cross-pour gate. Withholding the opt-in until the edge exists
	// closes that window: at no instant is an entry step both opted-in and
	// missing its cross-pour blocker.
	deferOptIn := make(map[string]bool, len(steps))
	for _, s := range steps {
		typ := s.Type
		if typ == "" {
			typ = "task"
		}
		pri := formula.Priority
		if s.Priority != nil {
			pri = *s.Priority
		}

		stepProject := project
		if s.Project != "" {
			stepProject = s.Project
		}
		stepLabels := formulaBuildStepLabels(labels, s.Labels, stepProject, project)
		roles := s.allRoles()
		for _, r := range roles {
			stepLabels = append(stepLabels, "role:"+r)
		}
		// A pod runs exactly one role, so only effectiveRole() (roles[0]) drives
		// the step-role dispatch; the rest land as role: labels but no agent is
		// spawned for them. Warn loudly at pour time so a two-role step is not a
		// silent truncation (ka-0000006285) — the author sees which role wins and
		// which are decorative before the molecule is live.
		if len(roles) > 1 {
			logger.Warn("formula: step declares multiple roles but only the first drives dispatch — the rest are labels only",
				"step", s.ID, "molecule", molID, "dispatch_role", s.effectiveRole(), "all_roles", roles)
		}

		// Dispatch metadata the step-role dispatcher (formula_step_dispatch.go)
		// reads when this step's blocking deps clear. source_step_id ties the
		// bead back to its formula step; step_role is the resolved dispatch
		// target so the dispatcher never has to re-parse labels; molecule_id
		// lets it scope the spawn to the right molecule without a graph walk.
		fieldMap := map[string]any{
			"source_formula": formula.ID,
			"source_step_id": s.ID,
			"molecule_id":    molID,
		}
		// A cross-pour entry step (no intra-molecule blocker) that opts in must
		// NOT carry suggest_new_agent until its cross-pour blocker edge lands —
		// otherwise the backfill sweep sees an opted-in, unblocked step and
		// dispatches it early (constraint 5). Withhold the stamp here and record
		// the step so it can be applied AFTER the edge below.
		if s.SuggestNewAgent {
			if crossPourUpstreamStep != "" && len(s.DependsOn) == 0 {
				deferOptIn[s.ID] = true
			} else {
				fieldMap["suggest_new_agent"] = true
			}
		}
		if r := s.effectiveRole(); r != "" {
			fieldMap["step_role"] = r
		}
		if stepProject != "" {
			fieldMap["step_project"] = stepProject
		}
		stepFieldsJSON, _ := json.Marshal(fieldMap)
		stepFields := json.RawMessage(stepFieldsJSON)

		stepID, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
			Title:       s.Title,
			Description: s.Description,
			Type:        typ,
			Priority:    pri,
			Labels:      stepLabels,
			Assignee:    s.Assignee,
			Fields:      stepFields,
			CreatedBy:   createdBy,
		})
		if err != nil {
			return molID, len(stepBeadIDs), fmt.Errorf("creating step %q: %w", s.ID, err)
		}
		stepBeadIDs[s.ID] = stepID

		if s.Project != "" && s.Project != project {
			crossProjectSteps[s.ID] = map[string]string{
				"project": s.Project,
				"bead_id": stepID,
			}
		}

		if err := daemon.AddDependency(ctx, stepID, molID, "parent-child", createdBy); err != nil {
			logger.Warn("formula: failed to add parent-child dep",
				"step", s.ID, "stepBead", stepID, "molecule", molID, "error", err)
		}
	}

	if len(crossProjectSteps) > 0 {
		xStepsJSON, _ := json.Marshal(crossProjectSteps)
		if err := daemon.UpdateBeadFields(ctx, molID, map[string]string{
			"cross_project_steps": string(xStepsJSON),
		}); err != nil {
			logger.Warn("formula: failed to store cross-project steps",
				"molecule", molID, "error", err)
		}
	}

	for _, s := range steps {
		for _, depStepID := range s.DependsOn {
			depBeadID, ok := stepBeadIDs[depStepID]
			if !ok {
				continue
			}
			if err := daemon.AddDependency(ctx, stepBeadIDs[s.ID], depBeadID, "blocks", createdBy); err != nil {
				logger.Warn("formula: failed to add blocks dep",
					"step", s.ID, "depends_on", depStepID, "error", err)
			}
		}
	}

	// Cross-pour edges: link every ENTRY step (no intra-molecule blocker) to the
	// upstream molecule's emit step with a `blocks` edge. This is what lets the
	// merged FormulaStepDispatcher handle the cross-molecule §7→§8 handoff
	// uniformly — closing the upstream emit step re-drives step 1 (decision
	// ka-0000007596). Orientation is (BeadID=blocked step1, DependsOnID=upstream
	// emit blocker), matching every other blocks edge in the graph.
	//
	// The edge is written BEFORE the deferred suggest_new_agent stamp so that a
	// backfill sweep can never observe an opted-in entry step without its
	// cross-pour blocker (constraint 5). Steps that opted in but are NOT entry
	// steps already carry the stamp from create; only entry steps were deferred.
	if crossPourUpstreamStep != "" {
		for _, s := range steps {
			if len(s.DependsOn) != 0 {
				continue // not an entry step — it has an intra-molecule blocker
			}
			stepID := stepBeadIDs[s.ID]
			if stepID == "" {
				continue
			}
			if err := daemon.AddDependency(ctx, stepID, crossPourUpstreamStep, "blocks", createdBy); err != nil {
				// Fail LOUD but do not abort: the molecule and its steps are
				// valid; the missing edge just means this seam won't auto-fire.
				// A childless molecule is worse than one that needs a nudge.
				logger.Error("formula: failed to add cross-pour blocks edge — step 1 will not auto-dispatch on upstream close",
					"step", s.ID, "stepBead", stepID, "upstream_emit", crossPourUpstreamStep, "molecule", molID, "error", err)
				continue
			}
			logger.Info("formula: linked cross-pour entry step to upstream emit step",
				"step", s.ID, "stepBead", stepID, "upstream_emit", crossPourUpstreamStep, "molecule", molID)

			// Now that the blocker exists, apply the withheld opt-in so the
			// dispatcher will spawn onto this step when the upstream emit closes.
			// Typed write: suggest_new_agent is a BOOLEAN schema field, and a
			// string PATCH of it 400s at the daemon validator (ka-0000002741).
			if deferOptIn[s.ID] {
				if err := daemon.UpdateBeadFieldsTyped(ctx, stepID, map[string]any{
					"suggest_new_agent": true,
				}); err != nil {
					logger.Error("formula: cross-pour edge landed but suggest_new_agent stamp FAILED — step 1 will not auto-dispatch",
						"step", s.ID, "stepBead", stepID, "molecule", molID, "error", err)
				}
			}
		}
	}

	return molID, len(stepBeadIDs), nil
}

// expandFormulaSteps applies variable substitution and condition evaluation
// to a list of formula steps, returning only the steps that pass conditions
// with their depends_on filtered to exclude skipped steps. This mirrors the
// expansion performed in Slack /formula pour and is shared with the
// apply_formula trigger action.
func expandFormulaSteps(steps []formulaStep, vars map[string]string) []formulaStep {
	type expanded struct {
		formulaStep
		skip bool
	}

	exp := make([]expanded, 0, len(steps))
	for _, s := range steps {
		es := expanded{formulaStep: s}
		if s.Condition != "" && !formulaEvalCondition(s.Condition, vars) {
			es.skip = true
		}
		es.Title = formulaSubstituteVars(s.Title, vars)
		es.Description = formulaSubstituteVars(s.Description, vars)
		es.Assignee = formulaSubstituteVars(s.Assignee, vars)
		es.Role = formulaSubstituteVars(s.Role, vars)
		// Substitute into the plural spelling too — a step declaring
		// roles: ["{{WORKER_ROLE}}"] must not pour a literal placeholder as
		// its role label (the singular-only bug's plural twin, ka-0000005050).
		if len(s.Roles) > 0 {
			roles := make([]string, len(s.Roles))
			for i, r := range s.Roles {
				roles[i] = formulaSubstituteVars(r, vars)
			}
			es.Roles = roles
		}
		es.Project = formulaSubstituteVars(s.Project, vars)
		exp = append(exp, es)
	}

	skipped := make(map[string]bool)
	for _, es := range exp {
		if es.skip {
			skipped[es.ID] = true
		}
	}

	out := make([]formulaStep, 0, len(exp))
	for _, es := range exp {
		if es.skip {
			continue
		}
		var deps []string
		for _, d := range es.DependsOn {
			if !skipped[d] {
				deps = append(deps, d)
			}
		}
		es.DependsOn = deps
		out = append(out, es.formulaStep)
	}
	return out
}
