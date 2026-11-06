package formula

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Bead is the minimal bead shape the expansion engine needs when it fetches
// referenced formulas (inheritance parents, expansion targets, aspects). Both
// the kd CLI (client.BeadsClient) and the daemon (store.Store) adapt their
// bead types onto it.
type Bead struct {
	ID          string
	Type        string
	Title       string
	Description string
	Priority    int
	Labels      []string
	Fields      json.RawMessage
}

// Getter fetches beads by id for inheritance / expansion / aspect resolution.
type Getter interface {
	GetFormulaBead(ctx context.Context, id string) (*Bead, error)
}

// ParseFields unmarshals a formula bead's Fields JSON (tolerant of the legacy
// double-encoded string forms via VarDefList / StepList).
func ParseFields(raw json.RawMessage) (Fields, error) {
	var f Fields
	if len(raw) == 0 {
		return f, fmt.Errorf("formula has no fields (empty formula)")
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("parsing formula fields: %w", err)
	}
	return f, nil
}

// RejectNonWorkflow returns an error when the formula's type cannot be
// instantiated directly — expansion and aspect formulas are used via
// composition only.
func RejectNonWorkflow(formulaID string, fields Fields) error {
	if fields.FormulaType == "expansion" {
		return fmt.Errorf("formula %s is type %q — expansion formulas cannot be instantiated directly (they are inlined into other formulas)", formulaID, fields.FormulaType)
	}
	if fields.FormulaType == "aspect" {
		return fmt.Errorf("formula %s is type %q — aspect formulas cannot be instantiated directly (they attach as hooks to other formulas)", formulaID, fields.FormulaType)
	}
	return nil
}

// ApplyVarDefaults fills defaults from the formula's var definitions into
// vars, then validates required / enum / pattern constraints. Mutates vars.
func ApplyVarDefaults(fields Fields, vars map[string]string) error {
	for _, vd := range fields.Vars {
		if _, ok := vars[vd.Name]; !ok {
			if vd.Default != "" {
				vars[vd.Name] = vd.Default
			} else if vd.Required {
				return fmt.Errorf("required variable {{%s}} not provided (use --var %s=<value>)", vd.Name, vd.Name)
			}
		}
		// Validate enum constraint.
		if len(vd.Enum) > 0 {
			if val, ok := vars[vd.Name]; ok {
				valid := false
				for _, e := range vd.Enum {
					if val == e {
						valid = true
						break
					}
				}
				if !valid {
					return fmt.Errorf("variable {{%s}} value %q not in allowed values: %v", vd.Name, val, vd.Enum)
				}
			}
		}
		// Validate pattern constraint.
		if vd.Pattern != "" {
			if val, ok := vars[vd.Name]; ok {
				re, err := regexp.Compile(vd.Pattern)
				if err != nil {
					return fmt.Errorf("variable {{%s}} has invalid pattern %q: %w", vd.Name, vd.Pattern, err)
				}
				if !re.MatchString(val) {
					return fmt.Errorf("variable {{%s}} value %q does not match pattern %s", vd.Name, val, vd.Pattern)
				}
			}
		}
	}
	return nil
}

// ResolveInheritance resolves the extends chain for a formula, merging vars
// and steps from parent formulas. Child formulas override parent vars/steps
// with the same name/ID. Detects circular inheritance and limits chain depth
// to 10.
func ResolveInheritance(ctx context.Context, g Getter, formulaID string, child Fields) (Fields, error) {
	const maxDepth = 10
	visited := map[string]bool{formulaID: true}
	return resolveInheritanceChain(ctx, g, child, visited, 0, maxDepth)
}

func resolveInheritanceChain(ctx context.Context, g Getter, child Fields, visited map[string]bool, depth, maxDepth int) (Fields, error) {
	if depth >= maxDepth {
		return child, fmt.Errorf("inheritance chain too deep (max %d)", maxDepth)
	}
	if len(child.Extends) == 0 {
		return child, nil
	}

	// Resolve each parent and merge.
	// Multiple extends: merge left to right, then child on top.
	merged := Fields{
		FormulaType:   child.FormulaType,
		AssignedAgent: child.AssignedAgent,
		Phase:         child.Phase,
	}

	for _, parentID := range child.Extends {
		// visited tracks only nodes on the CURRENT ancestor path, so a genuine
		// cycle (A->B->A) is caught while a diamond (child extends [A,B], both
		// A and B extends Z) is allowed — Z is popped off the path (below) once
		// A's subtree resolves, so re-visiting it via B is not a cycle. Without
		// the pop, visited becomes a "seen ever" set and diamonds are falsely
		// rejected as circular. MergeSteps/MergeVars dedup by ID/Name, so the
		// shared ancestor collapses cleanly with no duplicated steps.
		if visited[parentID] {
			return child, fmt.Errorf("circular inheritance detected: %s already in chain", parentID)
		}
		visited[parentID] = true

		parentBead, err := g.GetFormulaBead(ctx, parentID)
		if err != nil {
			return child, fmt.Errorf("fetching parent formula %s: %w", parentID, err)
		}
		if parentBead.Type != "formula" {
			return child, fmt.Errorf("extends target %s is type %q, not formula", parentID, parentBead.Type)
		}

		var parentFields Fields
		if len(parentBead.Fields) == 0 {
			return child, fmt.Errorf("parent formula %s has no fields", parentID)
		}
		if err := json.Unmarshal(parentBead.Fields, &parentFields); err != nil {
			return child, fmt.Errorf("parsing parent formula %s fields: %w", parentID, err)
		}

		// Recursively resolve parent's inheritance first.
		parentFields, err = resolveInheritanceChain(ctx, g, parentFields, visited, depth+1, maxDepth)
		if err != nil {
			return child, err
		}
		// Pop parentID off the active path now that its subtree is fully
		// resolved, so a later sibling that shares an ancestor with it (a
		// diamond) is not mistaken for a cycle.
		delete(visited, parentID)

		// Merge parent into accumulated result.
		merged.Vars = MergeVars(merged.Vars, parentFields.Vars)
		merged.Steps = MergeSteps(merged.Steps, parentFields.Steps)
		if len(merged.DefaultRoles) == 0 {
			merged.DefaultRoles = parentFields.DefaultRoles
		}
		if len(merged.RequiresSkills) == 0 {
			merged.RequiresSkills = parentFields.RequiresSkills
		}
		if merged.Phase == "" {
			merged.Phase = parentFields.Phase
		}
		if merged.FormulaType == "" {
			merged.FormulaType = parentFields.FormulaType
		}
		if merged.AssignedAgent == "" {
			merged.AssignedAgent = parentFields.AssignedAgent
		}
	}

	// Now merge child on top (child overrides).
	merged.Vars = MergeVars(merged.Vars, child.Vars)
	merged.Steps = MergeSteps(merged.Steps, child.Steps)
	if len(child.DefaultRoles) > 0 {
		merged.DefaultRoles = child.DefaultRoles
	}
	if len(child.RequiresSkills) > 0 {
		merged.RequiresSkills = child.RequiresSkills
	}
	if child.Phase != "" {
		merged.Phase = child.Phase
	}
	if child.FormulaType != "" {
		merged.FormulaType = child.FormulaType
	}
	if child.AssignedAgent != "" {
		merged.AssignedAgent = child.AssignedAgent
	}
	// Clear extends — already resolved.
	merged.Extends = nil
	return merged, nil
}

// InlineExpansion fetches an expansion-type formula and returns its steps
// with IDs prefixed by the expand step's ID. The expand step's depends_on
// is wired to the expansion's root steps (those with no internal dependencies).
func InlineExpansion(ctx context.Context, g Getter, expandStep Step, parentVars map[string]string) ([]Step, error) {
	expansionBead, err := g.GetFormulaBead(ctx, expandStep.Expand)
	if err != nil {
		return nil, fmt.Errorf("fetching expansion formula %s: %w", expandStep.Expand, err)
	}
	if expansionBead.Type != "formula" {
		return nil, fmt.Errorf("expand target %s is type %q, not formula", expandStep.Expand, expansionBead.Type)
	}

	var expFields Fields
	if len(expansionBead.Fields) == 0 {
		return nil, fmt.Errorf("expansion formula %s has no fields", expandStep.Expand)
	}
	if err := json.Unmarshal(expansionBead.Fields, &expFields); err != nil {
		return nil, fmt.Errorf("parsing expansion formula %s: %w", expandStep.Expand, err)
	}

	if expFields.FormulaType != "expansion" {
		return nil, fmt.Errorf("formula %s is type %q, not expansion — only expansion formulas can be inlined", expandStep.Expand, expFields.FormulaType)
	}

	if len(expFields.Steps) == 0 {
		return nil, fmt.Errorf("expansion formula %s has no steps", expandStep.Expand)
	}

	// Build merged vars: parent vars + expand_vars (expand_vars override).
	mergedVars := make(map[string]string, len(parentVars)+len(expandStep.ExpandVars))
	for k, v := range parentVars {
		mergedVars[k] = v
	}
	for k, v := range expandStep.ExpandVars {
		mergedVars[k] = v
	}

	// Apply defaults from expansion formula's var definitions.
	for _, vd := range expFields.Vars {
		if _, ok := mergedVars[vd.Name]; !ok && vd.Default != "" {
			mergedVars[vd.Name] = vd.Default
		}
	}

	// Prefix step IDs with the expand step's ID.
	prefix := expandStep.ID
	// Collect original step IDs for dependency rewriting.
	expStepIDs := make(map[string]bool, len(expFields.Steps))
	for _, s := range expFields.Steps {
		expStepIDs[s.ID] = true
	}

	// Merge needs into depends_on.
	MergeNeedsRecursive(expFields.Steps)

	// Find root steps (no internal dependencies) to wire the expand step's depends_on.
	rootSteps := make(map[string]bool)
	for _, s := range expFields.Steps {
		isRoot := true
		for _, dep := range s.DependsOn {
			if expStepIDs[dep] {
				isRoot = false
				break
			}
		}
		if isRoot {
			rootSteps[s.ID] = true
		}
	}

	// Build prefixed steps.
	result := make([]Step, 0, len(expFields.Steps))
	for _, s := range expFields.Steps {
		prefixed := s
		prefixed.ID = prefix + "/" + s.ID
		prefixed.Title = SubstituteVars(s.Title, mergedVars)
		prefixed.ShortTitle = SubstituteVars(s.ShortTitle, mergedVars)
		prefixed.Description = SubstituteVars(s.Description, mergedVars)
		if s.Assignee != "" {
			prefixed.Assignee = SubstituteVars(s.Assignee, mergedVars)
		}

		// Rewrite depends_on: prefix internal refs, keep external refs as-is.
		if len(s.DependsOn) > 0 {
			newDeps := make([]string, 0, len(s.DependsOn))
			for _, dep := range s.DependsOn {
				if expStepIDs[dep] {
					newDeps = append(newDeps, prefix+"/"+dep)
				} else {
					newDeps = append(newDeps, dep)
				}
			}
			prefixed.DependsOn = newDeps
		}

		// Root steps inherit the expand step's depends_on.
		if rootSteps[s.ID] && len(expandStep.DependsOn) > 0 {
			prefixed.DependsOn = append(expandStep.DependsOn, prefixed.DependsOn...)
		}

		// Store source tracing for expansion origin.
		prefixed.Expand = "" // clear expand field on inlined steps
		prefixed.ExpandVars = nil

		result = append(result, prefixed)
	}

	return result, nil
}

// ExpandBranch evaluates a branch step's variable against its cases and inlines
// the matching expansion formula's steps. Falls back to the default case if no
// case matches, or returns an error if no default is provided.
func ExpandBranch(ctx context.Context, g Getter, branchStep Step, parentVars map[string]string) ([]Step, error) {
	branch := branchStep.Branch

	// Evaluate the "on" expression by substituting variables.
	resolved := SubstituteVars(branch.On, parentVars)

	// Find the matching case.
	var selectedCase *BranchCase
	if bc, ok := branch.Cases[resolved]; ok {
		selectedCase = &bc
	} else if branch.Default != nil {
		selectedCase = branch.Default
	} else {
		return nil, fmt.Errorf("no case matches value %q and no default provided", resolved)
	}

	// Build a synthetic expand step from the branch step + selected case.
	expandStep := branchStep
	expandStep.Expand = selectedCase.Expand
	expandStep.ExpandVars = selectedCase.ExpandVars
	expandStep.Branch = nil // clear branch field

	return InlineExpansion(ctx, g, expandStep, parentVars)
}

// ApplyAspects resolves aspect formulas and injects their before/after steps
// around workflow steps whose IDs match the aspect's glob pattern.
func ApplyAspects(ctx context.Context, g Getter, aspectIDs []string, steps []Step) ([]Step, error) {
	for _, aspectID := range aspectIDs {
		aspectBead, err := g.GetFormulaBead(ctx, aspectID)
		if err != nil {
			return nil, fmt.Errorf("fetching aspect formula %s: %w", aspectID, err)
		}
		if aspectBead.Type != "formula" {
			return nil, fmt.Errorf("aspect target %s is type %q, not formula", aspectID, aspectBead.Type)
		}

		var af Fields
		if len(aspectBead.Fields) == 0 {
			return nil, fmt.Errorf("aspect formula %s has no fields", aspectID)
		}
		if err := json.Unmarshal(aspectBead.Fields, &af); err != nil {
			return nil, fmt.Errorf("parsing aspect formula %s: %w", aspectID, err)
		}

		if af.FormulaType != "aspect" {
			return nil, fmt.Errorf("formula %s is type %q, not aspect — only aspect formulas can be used in the aspects list", aspectID, af.FormulaType)
		}

		if af.Match == "" {
			return nil, fmt.Errorf("aspect formula %s has no match pattern", aspectID)
		}

		// Apply before/after to matching steps.
		steps = applyAspectToSteps(af, steps)
	}
	return steps, nil
}

// applyAspectToSteps injects before/after steps around steps matching the aspect's pattern.
func applyAspectToSteps(aspect Fields, steps []Step) []Step {
	var result []Step
	for _, s := range steps {
		if MatchGlob(aspect.Match, s.ID) {
			// Inject before steps: each gets a prefixed ID and depends_on the original step's deps.
			for _, b := range aspect.Before {
				prefixed := b
				prefixed.ID = s.ID + "/before/" + b.ID
				// Before steps inherit the matched step's dependencies.
				if len(s.DependsOn) > 0 && len(prefixed.DependsOn) == 0 {
					prefixed.DependsOn = make([]string, len(s.DependsOn))
					copy(prefixed.DependsOn, s.DependsOn)
				}
				result = append(result, prefixed)
			}

			// The original step now depends on the last before step (if any).
			modified := s
			if len(aspect.Before) > 0 {
				lastBefore := s.ID + "/before/" + aspect.Before[len(aspect.Before)-1].ID
				modified.DependsOn = append([]string{lastBefore}, modified.DependsOn...)
			}
			result = append(result, modified)

			// Inject after steps: each depends on the original step.
			for _, a := range aspect.After {
				prefixed := a
				prefixed.ID = s.ID + "/after/" + a.ID
				prefixed.DependsOn = []string{s.ID}
				result = append(result, prefixed)
			}
		} else {
			result = append(result, s)
		}
	}
	return result
}

// ExpandedStep is one step after expansion: branches resolved, expansions
// inlined, loops unrolled, {{vars}} substituted, and condition evaluated.
type ExpandedStep struct {
	Step
	Skip     bool // condition evaluated to false
	Children []ExpandedStep
}

// ExpandSteps runs the Expand phase: merges needs into depends_on, resolves
// branches/expansions/loops/conditions, substitutes variables, then filters
// out condition-skipped steps (dropping dangling depends_on references to
// them). Returns the ACTIVE step tree ready for instantiation.
func ExpandSteps(ctx context.Context, g Getter, steps []Step, vars map[string]string) ([]ExpandedStep, error) {
	// Merge `needs` into `depends_on` (needs is a human-friendly alias).
	MergeNeedsRecursive(steps)

	// expandSteps takes the ACTIVE variable scope (activeVars) explicitly rather
	// than closing over the outer `vars` — a loop step's children must be expanded
	// with the loop variable bound, so recursion into loop children passes the
	// per-iteration loopVars, not the loop-blind outer scope (ka-0000004104).
	var expandSteps func(steps []Step, activeVars map[string]string) ([]ExpandedStep, error)
	expandSteps = func(steps []Step, activeVars map[string]string) ([]ExpandedStep, error) {
		result := make([]ExpandedStep, 0, len(steps))
		for _, s := range steps {
			// Branch steps: evaluate variable and inline the matching case's expansion.
			if s.Branch != nil {
				inlinedSteps, err := ExpandBranch(ctx, g, s, activeVars)
				if err != nil {
					return nil, fmt.Errorf("branching step %q: %w", s.ID, err)
				}
				for _, is := range inlinedSteps {
					es := ExpandedStep{Step: is}
					if is.Condition != "" {
						if !EvaluateCondition(is.Condition, activeVars) {
							es.Skip = true
						}
					}
					es.Title = SubstituteVars(is.Title, activeVars)
					es.ShortTitle = SubstituteVars(is.ShortTitle, activeVars)
					es.Description = SubstituteVars(is.Description, activeVars)
					es.Assignee = SubstituteVars(is.Assignee, activeVars)
					result = append(result, es)
				}
				continue
			}

			// Inline expansions: replace step with expansion formula's steps.
			if s.Expand != "" {
				inlinedSteps, err := InlineExpansion(ctx, g, s, activeVars)
				if err != nil {
					return nil, fmt.Errorf("expanding step %q: %w", s.ID, err)
				}
				for _, is := range inlinedSteps {
					es := ExpandedStep{Step: is}
					if is.Condition != "" {
						if !EvaluateCondition(is.Condition, activeVars) {
							es.Skip = true
						}
					}
					es.Title = SubstituteVars(is.Title, activeVars)
					es.ShortTitle = SubstituteVars(is.ShortTitle, activeVars)
					es.Description = SubstituteVars(is.Description, activeVars)
					es.Assignee = SubstituteVars(is.Assignee, activeVars)
					result = append(result, es)
				}
				continue
			}

			// Expand loops: generate multiple steps from a single template.
			if s.Loop != nil {
				loopVals := ExpandLoopValues(s.Loop)
				for _, val := range loopVals {
					// Create a copy with loop var substituted.
					loopVars := make(map[string]string, len(activeVars)+1)
					for k, v := range activeVars {
						loopVars[k] = v
					}
					loopVars[s.Loop.Var] = val

					iteration := s
					iteration.Loop = nil // don't recurse loops
					iteration.ID = SubstituteVars(s.ID, loopVars)
					iteration.Title = s.Title // will be substituted below

					es := ExpandedStep{Step: iteration}
					if s.Condition != "" {
						if !EvaluateCondition(s.Condition, loopVars) {
							es.Skip = true
						}
					}
					es.Title = SubstituteVars(s.Title, loopVars)
					es.ShortTitle = SubstituteVars(s.ShortTitle, loopVars)
					es.Description = SubstituteVars(s.Description, loopVars)
					es.Assignee = SubstituteVars(s.Assignee, loopVars)
					// Substitute loop var in depends_on references too.
					if len(s.DependsOn) > 0 {
						expandedDeps := make([]string, len(s.DependsOn))
						for i, dep := range s.DependsOn {
							expandedDeps[i] = SubstituteVars(dep, loopVars)
						}
						es.DependsOn = expandedDeps
					}
					// Children of a loop step must expand with the loop variable
					// bound, or every iteration's children keep the literal
					// {{loopvar}} in their IDs — byte-identical across iterations,
					// which collides on stepIdempotencyKey and silently drops all
					// but the first iteration's children (ka-0000004104).
					if len(s.Children) > 0 {
						children, cerr := expandSteps(s.Children, loopVars)
						if cerr != nil {
							return nil, cerr
						}
						es.Children = children
					}
					result = append(result, es)
				}
				continue
			}

			es := ExpandedStep{Step: s}
			if s.Condition != "" {
				if !EvaluateCondition(s.Condition, activeVars) {
					es.Skip = true
				}
			}
			// Substitute active vars into the step's own ID and depends_on. This is
			// a no-op for placeholder-free top-level IDs, but a loop step's children
			// carry {{loopvar}} in their IDs/deps and must resolve per iteration
			// (ka-0000004104).
			es.ID = SubstituteVars(s.ID, activeVars)
			es.Title = SubstituteVars(s.Title, activeVars)
			es.ShortTitle = SubstituteVars(s.ShortTitle, activeVars)
			es.Description = SubstituteVars(s.Description, activeVars)
			es.Assignee = SubstituteVars(s.Assignee, activeVars)
			if len(s.DependsOn) > 0 {
				expandedDeps := make([]string, len(s.DependsOn))
				for i, dep := range s.DependsOn {
					expandedDeps[i] = SubstituteVars(dep, activeVars)
				}
				es.DependsOn = expandedDeps
			}
			if len(s.Children) > 0 {
				children, cerr := expandSteps(s.Children, activeVars)
				if cerr != nil {
					return nil, cerr
				}
				es.Children = children
			}
			result = append(result, es)
		}
		return result, nil
	}

	expanded, err := expandSteps(steps, vars)
	if err != nil {
		return nil, err
	}

	// Filter out skipped steps and update depends_on.
	skippedIDs := make(map[string]bool)
	var collectSkipped func(steps []ExpandedStep)
	collectSkipped = func(steps []ExpandedStep) {
		for _, es := range steps {
			if es.Skip {
				skippedIDs[es.ID] = true
			}
			collectSkipped(es.Children)
		}
	}
	collectSkipped(expanded)

	var filterActive func(steps []ExpandedStep) []ExpandedStep
	filterActive = func(steps []ExpandedStep) []ExpandedStep {
		var result []ExpandedStep
		for _, es := range steps {
			if es.Skip {
				continue
			}
			var filteredDeps []string
			for _, dep := range es.DependsOn {
				if !skippedIDs[dep] {
					filteredDeps = append(filteredDeps, dep)
				}
			}
			es.DependsOn = filteredDeps
			es.Children = filterActive(es.Children)
			result = append(result, es)
		}
		return result
	}

	return filterActive(expanded), nil
}

// CountSteps returns the total number of steps in the tree, recursing into
// children.
func CountSteps(steps []ExpandedStep) int {
	n := len(steps)
	for _, s := range steps {
		n += CountSteps(s.Children)
	}
	return n
}

// ValidateDependencies asserts that every `depends_on` reference in the ACTIVE
// (post-ExpandSteps) step tree resolves to a step that will actually be created,
// so a pour never silently drops a gating edge (ka-0000002771).
//
// The pour path creates all step beads, then wires blocks edges in a SECOND pass
// keyed by an in-memory step-id → bead-id map. A `depends_on` that names no
// created step used to hit an `if !ok { continue }` and vanish — the molecule
// came out looking correct (right steps, titles, parent) but with a step→step
// gate that gated nothing, handing a late step to `gb ready` before its
// prerequisite was done. This is the operator's requested pour-time assertion:
// run it BEFORE creating any beads so a dangling reference fails the whole pour
// loudly instead of producing a silently-ungated molecule.
//
// ExpandSteps already strips depends_on refs to condition-SKIPPED steps, so a
// residual dangling reference here is a genuine authoring bug (a typo, or a
// reference to a step in a different molecule) — not an expected filter. Callers
// run this on the active tree returned by ExpandSteps.
func ValidateDependencies(steps []ExpandedStep) error {
	// Collect every id that will become a bead (across all nesting levels).
	present := make(map[string]bool)
	var collect func(steps []ExpandedStep)
	collect = func(steps []ExpandedStep) {
		for _, s := range steps {
			present[s.ID] = true
			collect(s.Children)
		}
	}
	collect(steps)

	var dangling []string
	var check func(steps []ExpandedStep)
	check = func(steps []ExpandedStep) {
		for _, s := range steps {
			for _, dep := range s.DependsOn {
				if !present[dep] {
					dangling = append(dangling, fmt.Sprintf("%s depends_on %q", s.ID, dep))
				}
			}
			check(s.Children)
		}
	}
	check(steps)

	if len(dangling) > 0 {
		return fmt.Errorf("formula has %d unresolvable depends_on reference(s) that would be silently dropped, producing an ungated molecule: %s",
			len(dangling), strings.Join(dangling, "; "))
	}
	return nil
}
