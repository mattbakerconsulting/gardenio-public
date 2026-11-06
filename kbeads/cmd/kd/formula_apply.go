package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/formula"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// formulaApplyOpts holds the options for instantiating a formula.
type formulaApplyOpts struct {
	FormulaID string
	VarPairs  []string
	Labels    []string
	Assignee  string
	DryRun    bool
	Ephemeral bool // true for wisp (vapor phase), false for pour (liquid phase)
	Force     bool // override dedup block and create anyway
	// TriggerBeadIDs / SourceBeadIDs record provenance: each ID becomes a
	// `triggered-by` dep from the molecule to the named bead at create time.
	// TriggerBeadIDs is the trigger config bead (e.g. apply_formula trigger);
	// SourceBeadIDs is the input bead that matched (e.g. incoming work bead).
	// Both write the same dep type per D1 (ka-0000005967); the distinction is
	// preserved on the molecule's `trigger_bead_ids` / `source_bead_ids` fields.
	TriggerBeadIDs []string
	SourceBeadIDs  []string
}

// formulaFields is the parsed content of a formula bead's Fields JSON.
// Alias of the shared engine type (internal/formula) so the CLI and the
// daemon pour endpoint parse formulas identically.
type formulaFields = formula.Fields

// clientGetter adapts the CLI's beadsClient to the shared expansion engine's
// Getter interface (fetching inheritance parents / expansions / aspects).
type clientGetter struct{}

func (clientGetter) GetFormulaBead(ctx context.Context, id string) (*formula.Bead, error) {
	b, err := beadsClient.GetBead(ctx, id)
	if err != nil {
		return nil, err
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

// isFilePath returns true if the argument looks like a filesystem path rather than a bead ID.
func isFilePath(s string) bool {
	return strings.Contains(s, "/") || strings.Contains(s, "\\") ||
		strings.HasSuffix(s, ".json") || strings.HasPrefix(s, ".")
}

// loadFormulaFromFile reads a formula JSON file and returns a synthetic bead and parsed fields.
func loadFormulaFromFile(path string) (*model.Bead, formulaFields, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, formulaFields{}, fmt.Errorf("reading formula file %s: %w", path, err)
	}

	var fields formulaFields
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, formulaFields{}, fmt.Errorf("parsing formula file %s: %w", path, err)
	}

	// Merge needs into depends_on.
	mergeNeedsRecursive(fields.Steps)

	// Create a synthetic bead for the pipeline (no ID, title from filename).
	title := strings.TrimSuffix(strings.TrimSuffix(path, ".json"), "/")
	if idx := strings.LastIndex(title, "/"); idx >= 0 {
		title = title[idx+1:]
	}

	syntheticBead := &model.Bead{
		ID:     "file:" + path,
		Type:   "formula",
		Title:  title,
		Fields: data,
	}

	return syntheticBead, fields, nil
}

// runFormulaApply is the shared implementation for apply, pour, and wisp commands.
func runFormulaApply(opts formulaApplyOpts) error {
	ctx := context.Background()

	// ── Step 1: Resolve ─────────────────────────────────────
	var bead *model.Bead
	var fields formulaFields

	if isFilePath(opts.FormulaID) {
		// Load formula from filesystem.
		var err error
		bead, fields, err = loadFormulaFromFile(opts.FormulaID)
		if err != nil {
			return err
		}
	} else {
		// Load formula from database.
		var err error
		bead, err = beadsClient.GetBead(ctx, opts.FormulaID)
		if err != nil {
			return fmt.Errorf("resolving formula: %w", err)
		}
		if string(bead.Type) != "formula" {
			return fmt.Errorf("bead %s is type %q, not formula", opts.FormulaID, bead.Type)
		}

		if len(bead.Fields) == 0 {
			return fmt.Errorf("formula %s has no fields (empty formula)", opts.FormulaID)
		}
		if err := json.Unmarshal(bead.Fields, &fields); err != nil {
			return fmt.Errorf("parsing formula fields: %w", err)
		}
	}

	// Resolve inheritance chain if this formula extends others.
	if len(fields.Extends) > 0 {
		var err error
		fields, err = resolveInheritance(ctx, opts.FormulaID, fields)
		if err != nil {
			return fmt.Errorf("resolving inheritance: %w", err)
		}
	}

	if len(fields.Steps) == 0 {
		return fmt.Errorf("formula %s has no steps", opts.FormulaID)
	}

	// Reject non-workflow formula types — expansion and aspect are used via composition, not direct instantiation.
	if err := formula.RejectNonWorkflow(opts.FormulaID, fields); err != nil {
		return err
	}

	// Apply aspects if any are declared.
	if len(fields.Aspects) > 0 {
		var err error
		fields.Steps, err = applyAspects(ctx, fields.Aspects, fields.Steps)
		if err != nil {
			return fmt.Errorf("applying aspects: %w", err)
		}
	}

	// Warn if instantiation phase doesn't match formula's recommended phase.
	if fields.Phase == "vapor" && !opts.Ephemeral {
		fmt.Fprintf(os.Stderr, "Warning: this formula recommends wisp (vapor phase); use 'kd formula wisp' instead\n")
	} else if fields.Phase == "liquid" && opts.Ephemeral {
		fmt.Fprintf(os.Stderr, "Warning: this formula recommends pour (liquid phase); use 'kd formula pour' instead\n")
	}

	// Use formula's assigned_agent as default assignee if none specified on the command line.
	if opts.Assignee == "" && fields.AssignedAgent != "" {
		opts.Assignee = fields.AssignedAgent
	}

	// Parse --var key=value pairs.
	vars := make(map[string]string, len(opts.VarPairs))
	for _, p := range opts.VarPairs {
		k, v, ok := splitField(p)
		if !ok {
			return fmt.Errorf("invalid --var %q: expected key=value", p)
		}
		vars[k] = v
	}

	// Apply defaults and validate required / enum / pattern constraints.
	if err := formula.ApplyVarDefaults(fields, vars); err != nil {
		return err
	}

	// Reject var values that still carry {{...}} placeholders — the caller
	// passed a template that was never resolved (e.g. a trigger fire path
	// forwarding an unsupported placeholder like {{fire_time_iso}}). The
	// literal would be baked into the molecule's applied_vars and every step
	// title, stranding unclaimable template beads (ka-0000006473). --force
	// overrides for the rare intentional case.
	if bad := unresolvedVarValues(vars); len(bad) > 0 {
		if !opts.Force {
			return fmt.Errorf("variable value(s) contain unresolved {{...}} placeholders: %s (resolve them before apply, or use --force)", strings.Join(bad, ", "))
		}
		fmt.Fprintf(os.Stderr, "Warning: variable value(s) contain unresolved {{...}} placeholders: %s (proceeding due to --force)\n", strings.Join(bad, ", "))
	}

	// ── Dedup check ────────────────────────────────────────
	// Always check for existing molecules from the same formula with the
	// same applied_vars. Fails by default; use --force to override.
	if !opts.DryRun {
		existing, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
			Type:   []string{"molecule"},
			Status: []string{"open", "in_progress"},
			FieldFilters: map[string]string{
				"formula_id": opts.FormulaID,
			},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: dedup check failed: %v\n", err)
		} else if len(existing.Beads) > 0 {
			var matches []*model.Bead
			for _, b := range existing.Beads {
				var f struct {
					AppliedVars map[string]string `json:"applied_vars"`
				}
				if len(b.Fields) > 0 {
					json.Unmarshal(b.Fields, &f)
				}
				if mapsEqual(f.AppliedVars, vars) {
					matches = append(matches, b)
				}
			}
			if len(matches) > 0 {
				fmt.Fprintf(os.Stderr, "Error: found %d existing molecule(s) with identical vars:\n", len(matches))
				for _, b := range matches {
					fmt.Fprintf(os.Stderr, "  %s: %s [%s]\n", b.ID, b.Title, b.Status)
				}
				if !opts.Force {
					return fmt.Errorf("duplicate molecule detected (use --force to override)")
				}
				fmt.Fprintf(os.Stderr, "Proceeding anyway (--force)\n")
			}
		}
	}

	// ── Step 2: Expand ──────────────────────────────────────
	active, err := formula.ExpandSteps(ctx, clientGetter{}, fields.Steps, vars)
	if err != nil {
		return fmt.Errorf("expanding steps: %w", err)
	}

	if len(active) == 0 {
		return fmt.Errorf("all steps were filtered by conditions; nothing to create")
	}

	// Fail loudly BEFORE creating any beads if a step's depends_on names a step
	// that will not exist — otherwise the blocks-edge pass below silently drops
	// that edge and the molecule pours ungated (ka-0000002771). Runs on dry-run
	// too so a preview surfaces the same authoring bug.
	if err := formula.ValidateDependencies(active); err != nil {
		return err
	}

	// Phase label for output.
	phase := "molecule"
	if opts.Ephemeral {
		phase = "wisp"
	}

	// ── Dry run ─────────────────────────────────────────────
	if opts.DryRun {
		fmt.Printf("Formula: %s (%s)\n", bead.Title, opts.FormulaID)
		fmt.Printf("Variables: %v\n", vars)
		fmt.Printf("Phase: %s\n\n", phase)
		fmt.Println("Would create:")
		fmt.Printf("  %s: %s\n", capitalize(phase), substituteVars(bead.Title, vars))
		var printDryRunSteps func(steps []formula.ExpandedStep, indent string)
		printDryRunSteps = func(steps []formula.ExpandedStep, indent string) {
			for _, s := range steps {
				typ := s.Type
				if typ == "" {
					typ = "task"
				}
				deps := ""
				if len(s.DependsOn) > 0 {
					deps = fmt.Sprintf(" (after: %s)", strings.Join(s.DependsOn, ", "))
				}
				roles := s.Roles
				if len(roles) == 0 {
					roles = fields.DefaultRoles
				}
				roleStr := ""
				if len(roles) > 0 {
					roleStr = fmt.Sprintf(" roles:%v", roles)
				}
				skills := s.RequiresSkills
				if len(skills) == 0 {
					skills = fields.RequiresSkills
				}
				skillStr := ""
				if len(skills) > 0 {
					skillStr = fmt.Sprintf(" skills:%v", skills)
				}
				waitsStr := ""
				if s.WaitsFor != "" {
					waitsStr = fmt.Sprintf(" waits_for:%s", s.WaitsFor)
				}
				onCompleteStr := ""
				if s.OnComplete != nil {
					onCompleteStr = fmt.Sprintf(" on_complete:{for_each:%s var:%s expand:%s}", s.OnComplete.ForEach, s.OnComplete.Var, s.OnComplete.Expand)
				}
				fmt.Printf("%sStep %s: %s [%s]%s%s%s%s%s\n", indent, s.ID, s.Title, typ, deps, roleStr, skillStr, waitsStr, onCompleteStr)
				printDryRunSteps(s.Children, indent+"  ")
			}
		}
		printDryRunSteps(active, "  ")

		fmt.Printf("\nTotal: 1 %s + %d steps\n", phase, formula.CountSteps(active))
		return nil
	}

	// ── Step 3: Create ──────────────────────────────────────

	// Create the root molecule bead.
	molTitle := substituteVars(bead.Title, vars)
	appliedVarsJSON, _ := json.Marshal(vars)
	molFields := map[string]any{
		"formula_id":   opts.FormulaID,
		"applied_vars": json.RawMessage(appliedVarsJSON),
	}
	if opts.Ephemeral {
		molFields["ephemeral"] = true
	}
	if len(opts.TriggerBeadIDs) > 0 {
		molFields["trigger_bead_ids"] = opts.TriggerBeadIDs
	}
	if len(opts.SourceBeadIDs) > 0 {
		molFields["source_bead_ids"] = opts.SourceBeadIDs
	}
	molFieldsJSON, _ := json.Marshal(molFields)

	// Merge formula's labels with command-line labels so project/role scope propagates.
	molLabels := mergeLabels(bead.Labels, opts.Labels)

	molReq := &client.CreateBeadRequest{
		Title:       molTitle,
		ShortTitle:  formula.ShortTitle("", molTitle),
		Description: substituteVars(bead.Description, vars),
		Type:        "molecule",
		Priority:    bead.Priority,
		Labels:      molLabels,
		Assignee:    opts.Assignee,
		CreatedBy:   actor,
		Fields:      molFieldsJSON,
	}

	mol, err := beadsClient.CreateBead(ctx, molReq)
	if err != nil {
		return fmt.Errorf("creating %s: %w", phase, err)
	}

	// Write triggered-by lineage edges (D1 ka-0000005967 / D2 ka-0000004959).
	for _, tb := range opts.TriggerBeadIDs {
		if tb == "" {
			continue
		}
		if _, derr := beadsClient.AddDependency(ctx, &client.AddDependencyRequest{
			BeadID:      mol.ID,
			DependsOnID: tb,
			Type:        string(model.DepTriggeredBy),
			CreatedBy:   actor,
		}); derr != nil {
			return fmt.Errorf("adding triggered-by dep %s→%s: %w", mol.ID, tb, derr)
		}
	}
	for _, sb := range opts.SourceBeadIDs {
		if sb == "" {
			continue
		}
		if _, derr := beadsClient.AddDependency(ctx, &client.AddDependencyRequest{
			BeadID:      mol.ID,
			DependsOnID: sb,
			Type:        string(model.DepTriggeredBy),
			CreatedBy:   actor,
		}); derr != nil {
			return fmt.Errorf("adding triggered-by dep %s→%s: %w", mol.ID, sb, derr)
		}
	}

	// Create child beads for each step (recursively for nested children).
	stepBeadIDs := make(map[string]string) // step ID → bead ID (global across all levels)

	var createSteps func(steps []formula.ExpandedStep, parentBeadID string) error
	createSteps = func(steps []formula.ExpandedStep, parentBeadID string) error {
		for _, s := range steps {
			typ := s.Type
			if typ == "" {
				typ = "task"
			}
			stepAssignee := s.Assignee
			if stepAssignee == "" {
				stepAssignee = opts.Assignee
			}

			// Resolve roles: step-level roles override formula default_roles.
			stepRoles := s.Roles
			if len(stepRoles) == 0 {
				stepRoles = fields.DefaultRoles
			}
			var roleLabels []string
			for _, r := range stepRoles {
				roleLabels = append(roleLabels, "role:"+substituteVars(r, vars))
			}

			// Substitute {{vars}} in raw step labels too, so labels like
			// "role:{{WORKER_ROLE}}" or "iteration:{{ITERATION}}" render
			// instead of leaking the literal placeholder (ka-0000005050).
			stepLabels := make([]string, len(s.Labels))
			for i, l := range s.Labels {
				stepLabels[i] = substituteVars(l, vars)
			}

			// Resolve skills: step-level skills override formula requires_skills.
			stepSkills := s.RequiresSkills
			if len(stepSkills) == 0 {
				stepSkills = fields.RequiresSkills
			}
			var skillLabels []string
			for _, sk := range stepSkills {
				skillLabels = append(skillLabels, "skill:"+substituteVars(sk, vars))
			}

			// Source tracing + gate fields.
			stepFields := map[string]any{
				"source_formula": opts.FormulaID,
				"source_step_id": s.ID,
			}

			// Gate steps get special treatment.
			beadKind := "" // default: issue kind
			if s.Gate != nil {
				stepFields["gate_type"] = s.Gate.Type
				if s.Gate.Timeout != "" {
					stepFields["timeout"] = s.Gate.Timeout
				}
				if s.Gate.Type == "decision" {
					typ = "decision"
					beadKind = "data"
					stepFields["prompt"] = s.Gate.Prompt
					if len(s.Gate.Options) > 0 {
						stepFields["options"] = s.Gate.Options
					}
				}
			}
			_ = beadKind // reserved for future kind override support

			// waits_for: record sync mode in fields.
			if s.WaitsFor != "" {
				stepFields["waits_for"] = s.WaitsFor
			}

			// suggest_new_agent: opt this step into the controller's step-role
			// dispatcher, which spawns a fresh role-matched agent when the
			// step's last blocking dep closes (ka-0000001730). Also stamp the
			// resolved role so the dispatcher does not have to re-derive it
			// from labels; the role: labels are still written below for
			// queries and advice matching.
			if s.SuggestNewAgent {
				stepFields["suggest_new_agent"] = true
				if len(roleLabels) > 0 {
					stepFields["step_role"] = strings.TrimPrefix(roleLabels[0], "role:")
				}
			}

			// on_complete: store deferred expansion spec for the orchestrator.
			if s.OnComplete != nil {
				stepFields["on_complete"] = map[string]any{
					"for_each":    s.OnComplete.ForEach,
					"var":         s.OnComplete.Var,
					"expand":      s.OnComplete.Expand,
					"expand_vars": s.OnComplete.ExpandVars,
				}
			}

			stepFieldsJSON, _ := json.Marshal(stepFields)

			// D5 (ka-0000007622): pass parent_bead_id so the server wires
			// the molecule/parent-step parent-child dep transactionally and
			// skips the static-actor auto-parent step. Previously we created
			// the step then issued a separate AddDependency, which let the
			// auto-parent path also fire and stack a second parent edge to
			// whatever task the formula-apply actor had claimed.
			stepReq := &client.CreateBeadRequest{
				Title:        s.Title,
				ShortTitle:   formula.ShortTitle(s.ShortTitle, s.Title),
				Description:  s.Description,
				Type:         typ,
				Priority:     priorityOrDefault(s.Priority, bead.Priority),
				Labels:       mergeLabels(mergeLabels(mergeLabels(molLabels, stepLabels), roleLabels), skillLabels),
				Assignee:     stepAssignee,
				CreatedBy:    actor,
				Fields:       stepFieldsJSON,
				ParentBeadID: parentBeadID,
			}

			stepBead, err := beadsClient.CreateBead(ctx, stepReq)
			if err != nil {
				return fmt.Errorf("creating step %q: %w", s.ID, err)
			}
			stepBeadIDs[s.ID] = stepBead.ID

			// Recursively create children.
			if len(s.Children) > 0 {
				if err := createSteps(s.Children, stepBead.ID); err != nil {
					return err
				}

				// waits_for=all-children: parent step is blocked by all its children.
				if s.WaitsFor == "all-children" {
					for _, child := range s.Children {
						childBeadID, ok := stepBeadIDs[child.ID]
						if !ok {
							continue
						}
						_, err = beadsClient.AddDependency(ctx, &client.AddDependencyRequest{
							BeadID:      stepBead.ID,
							DependsOnID: childBeadID,
							Type:        "blocks",
							CreatedBy:   actor,
						})
						if err != nil {
							return fmt.Errorf("adding waits_for dep %s→%s: %w", s.ID, child.ID, err)
						}
					}
				}
			}
		}
		return nil
	}

	if err := createSteps(active, mol.ID); err != nil {
		return err
	}

	// Add blocks dependencies between steps (across all levels).
	var addBlocksDeps func(steps []formula.ExpandedStep) error
	addBlocksDeps = func(steps []formula.ExpandedStep) error {
		for _, s := range steps {
			for _, depStepID := range s.DependsOn {
				depBeadID, ok := stepBeadIDs[depStepID]
				if !ok {
					// ValidateDependencies above already rejects dangling refs
					// before any bead is created; reaching here means the created
					// step set diverged from the validated tree, so fail loudly
					// rather than silently dropping the gating edge (ka-0000002771).
					return fmt.Errorf("step %q depends_on %q, which was not created — refusing to pour an ungated molecule", s.ID, depStepID)
				}
				_, err = beadsClient.AddDependency(ctx, &client.AddDependencyRequest{
					BeadID:      stepBeadIDs[s.ID],
					DependsOnID: depBeadID,
					Type:        "blocks",
					CreatedBy:   actor,
				})
				if err != nil {
					return fmt.Errorf("adding dependency %s→%s: %w", s.ID, depStepID, err)
				}
			}
			if err := addBlocksDeps(s.Children); err != nil {
				return err
			}
		}
		return nil
	}

	if err := addBlocksDeps(active); err != nil {
		return err
	}

	totalSteps := formula.CountSteps(active)

	// Output results.
	if jsonOutput {
		result := map[string]any{
			phase:   mol,
			"steps": stepBeadIDs,
		}
		data, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(data))
	} else {
		fmt.Printf("Created %s %s: %s\n", phase, mol.ID, mol.Title)
		fmt.Printf("  Formula: %s\n", opts.FormulaID)
		fmt.Printf("  Steps:   %d\n", totalSteps)
		var printSteps func(steps []formula.ExpandedStep, indent string)
		printSteps = func(steps []formula.ExpandedStep, indent string) {
			for _, s := range steps {
				fmt.Printf("%s%s → %s: %s\n", indent, s.ID, stepBeadIDs[s.ID], s.Title)
				printSteps(s.Children, indent+"  ")
			}
		}
		printSteps(active, "    ")
	}
	return nil
}

// inlineExpansion fetches an expansion-type formula and returns its steps
// with IDs prefixed by the expand step's ID. Delegates to the shared engine.
func inlineExpansion(ctx context.Context, expandStep FormulaStep, parentVars map[string]string) ([]FormulaStep, error) {
	return formula.InlineExpansion(ctx, clientGetter{}, expandStep, parentVars)
}

// expandBranch evaluates a branch step's variable against its cases and inlines
// the matching expansion formula's steps. Delegates to the shared engine.
func expandBranch(ctx context.Context, branchStep FormulaStep, parentVars map[string]string) ([]FormulaStep, error) {
	return formula.ExpandBranch(ctx, clientGetter{}, branchStep, parentVars)
}

// applyAspects resolves aspect formulas and injects their before/after steps
// around workflow steps whose IDs match the aspect's glob pattern.
func applyAspects(ctx context.Context, aspectIDs []string, steps []FormulaStep) ([]FormulaStep, error) {
	return formula.ApplyAspects(ctx, clientGetter{}, aspectIDs, steps)
}

// mapsEqual returns true if two string maps have the same keys and values.
func mapsEqual(a, b map[string]string) bool {
	return formula.MapsEqual(a, b)
}

// capitalize returns the string with the first letter uppercased.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// instantiateRunE returns a RunE function for pour/wisp commands.
func instantiateRunE(ephemeral bool) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		varPairs, _ := cmd.Flags().GetStringArray("var")
		labels, _ := cmd.Flags().GetStringSlice("label")
		assignee, _ := cmd.Flags().GetString("assignee")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		force, _ := cmd.Flags().GetBool("force")
		triggerBeads, _ := cmd.Flags().GetStringSlice("trigger-bead")
		sourceBeads, _ := cmd.Flags().GetStringSlice("source-bead")
		return runFormulaApply(formulaApplyOpts{
			FormulaID:      args[0],
			VarPairs:       varPairs,
			Labels:         labels,
			Assignee:       assignee,
			DryRun:         dryRun,
			Ephemeral:      ephemeral,
			Force:          force,
			TriggerBeadIDs: triggerBeads,
			SourceBeadIDs:  sourceBeads,
		})
	}
}

// newPourCmd creates a new pour command instance.
func newPourCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pour <formula-id>",
		Short: "Instantiate formula as persistent molecule (liquid phase)",
		Long: `Pour a formula into a persistent molecule.

Phase transition: Formula (solid) -> pour -> Molecule (liquid)

If the formula declares phase: "vapor", a warning is printed suggesting
you use 'kd formula wisp' instead. Use pour only for persistent work.

WHEN TO USE POUR vs WISP:
  pour (liquid): Persistent work that needs audit trail
    - Feature implementations spanning multiple sessions
    - Work you may need to reference later

  wisp (vapor): Ephemeral work that auto-cleans up
    - Release workflows (one-time execution)
    - Health checks and diagnostics`,
		Args: cobra.ExactArgs(1),
		RunE: instantiateRunE(false),
	}
	addInstantiateFlags(cmd)
	return cmd
}

// newWispCmd creates a new wisp command instance.
func newWispCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wisp <formula-id>",
		Short: "Instantiate formula as ephemeral wisp (vapor phase)",
		Long: `Create an ephemeral wisp from a formula.

Phase transition: Formula (solid) -> wisp -> Wisp (vapor)

Wisps are ephemeral molecules marked with ephemeral=true, indicating
transient operational work that may be garbage-collected. Use
'kd mol gc --age <duration>' to clean up old wisps.

If the formula declares phase: "liquid", a warning is printed suggesting
you use 'kd formula pour' instead. Use wisp only for ephemeral work.

WHEN TO USE WISP vs POUR:
  wisp (vapor): Ephemeral work that auto-cleans up
    - Release workflows (one-time execution)
    - Health checks and diagnostics

  pour (liquid): Persistent work that needs audit trail
    - Feature implementations spanning multiple sessions`,
		Args: cobra.ExactArgs(1),
		RunE: instantiateRunE(true),
	}
	addInstantiateFlags(cmd)
	return cmd
}

// formulaApplyCmd is the generic apply command (defaults to persistent/pour).
var formulaApplyCmd = &cobra.Command{
	Use:   "apply <formula-id-or-file>",
	Short: "Apply a formula to create a molecule of beads",
	Long: `Apply a formula to create a molecule (an epic with child beads).

Equivalent to 'pour'. Use --ephemeral for wisp (vapor phase).

Accepts either a bead ID (ka-xxx) or a file path (.json) to load a formula
from the filesystem. File-based formulas support extends (resolved from DB).

The 3-step pipeline:
  1. Resolve — fetch the formula and parse its vars/steps
  2. Expand  — substitute {{variables}} with provided values
  3. Create  — create the molecule bead and child issue beads

Examples:
  kd formula apply ka-abc123 --var component=auth --var assignee=alice
  kd formula apply ka-abc123 --var component=auth --dry-run
  kd formula apply ka-abc123 --var component=auth --ephemeral
  kd formula apply ./formulas/deploy.json --var env=prod   # apply from file
  kd formula apply ka-abc123 --var x=y --trigger-bead ka-0000005603  # record lineage
  kd formula apply ka-abc123 --var x=y --source-bead ka-input456     # record source bead`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		varPairs, _ := cmd.Flags().GetStringArray("var")
		labels, _ := cmd.Flags().GetStringSlice("label")
		assignee, _ := cmd.Flags().GetString("assignee")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		ephemeral, _ := cmd.Flags().GetBool("ephemeral")
		force, _ := cmd.Flags().GetBool("force")
		triggerBeads, _ := cmd.Flags().GetStringSlice("trigger-bead")
		sourceBeads, _ := cmd.Flags().GetStringSlice("source-bead")
		return runFormulaApply(formulaApplyOpts{
			FormulaID:      args[0],
			VarPairs:       varPairs,
			Labels:         labels,
			Assignee:       assignee,
			DryRun:         dryRun,
			Ephemeral:      ephemeral,
			Force:          force,
			TriggerBeadIDs: triggerBeads,
			SourceBeadIDs:  sourceBeads,
		})
	},
}

// substituteVars replaces {{name}} placeholders with values from vars.
// Unmatched placeholders are left as-is.
func substituteVars(s string, vars map[string]string) string {
	return formula.SubstituteVars(s, vars)
}

// unresolvedVarValues returns `name="value"` strings for vars whose VALUES
// still contain {{...}} placeholders (ka-0000006473).
func unresolvedVarValues(vars map[string]string) []string {
	return formula.UnresolvedVarValues(vars)
}

// evaluateCondition evaluates a simple condition string against variables.
func evaluateCondition(cond string, vars map[string]string) bool {
	return formula.EvaluateCondition(cond, vars)
}

// priorityOrDefault returns the step priority if set, otherwise the default.
func priorityOrDefault(p *int, def int) int {
	return formula.PriorityOrDefault(p, def)
}

// mergeLabels combines two label slices, deduplicating.
func mergeLabels(base, extra []string) []string {
	return formula.MergeLabels(base, extra)
}

// expandLoopValues returns the iteration values for a step loop.
func expandLoopValues(loop *StepLoop) []string {
	return formula.ExpandLoopValues(loop)
}

// mergeNeedsRecursive merges `needs` into `depends_on` for a slice of steps, recursively.
func mergeNeedsRecursive(steps []FormulaStep) {
	formula.MergeNeedsRecursive(steps)
}

// resolveInheritance resolves the extends chain for a formula, merging vars and steps
// from parent formulas. Child formulas override parent vars/steps with the same name/ID.
func resolveInheritance(ctx context.Context, formulaID string, child formulaFields) (formulaFields, error) {
	return formula.ResolveInheritance(ctx, clientGetter{}, formulaID, child)
}

// addInstantiateFlags adds the common flags for apply/pour/wisp commands.
func addInstantiateFlags(cmd *cobra.Command) {
	// --var uses StringArray (not StringSlice) so comma-containing values pass
	// through intact — formula var values are free text (role/site/subject
	// descriptions) that naturally contain commas (ka-0000003349).
	cmd.Flags().StringArray("var", nil, "variable substitution (key=value, repeatable; one value per flag, commas kept verbatim)")
	cmd.Flags().StringSliceP("label", "l", nil, "labels for created beads (repeatable)")
	cmd.Flags().String("assignee", "", "default assignee for created beads")
	cmd.Flags().Bool("dry-run", false, "preview what would be created without creating anything")
	cmd.Flags().Bool("force", false, "override dedup check and create molecule even if duplicates exist")
	cmd.Flags().StringSlice("trigger-bead", nil, "bead id(s) that triggered this molecule; writes a triggered-by dep (repeatable)")
	cmd.Flags().StringSlice("source-bead", nil, "source bead id(s) that produced this molecule; writes a triggered-by dep (repeatable)")
	// Accept --labels as an alias of --label (ka-0000000987).
	aliasLabelFlags(cmd)
}

func init() {
	addInstantiateFlags(formulaApplyCmd)
	formulaApplyCmd.Flags().Bool("ephemeral", false, "create ephemeral wisp instead of persistent molecule")
}
