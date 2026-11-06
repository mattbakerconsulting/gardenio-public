package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/formula"
	"github.com/spf13/cobra"
)

// expandOpts carries the resolved flags for an on_complete expansion run.
type expandOpts struct {
	StepBeadID string
	Values     string   // comma-separated literal values
	FromField  string   // dot-path into the step bead's fields (array source)
	VarPairs   []string // additional --var key=value overrides
	DryRun     bool
}

func newExpandCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "expand <step-bead-id> (--values <v1,v2,...> | --from-field <path>)",
		Short: "Trigger deferred on_complete expansion for a step bead",
		Long: `Manually trigger a deferred on_complete expansion on a step bead.

When a formula step has an on_complete handler, the expansion formula is not
inlined at pour time — instead, the on_complete spec is stored in the step
bead's fields for later triggering by the orchestrator or this command.

This command reads the on_complete spec from the step bead, iterates over a
set of values, and creates new step beads by expanding the referenced formula
once per value. The new steps are linked as children of the step bead.

The iteration values come from one of:
  --values a,b,c          explicit comma-separated values (host pod supplies them)
  --from-field <path>     a dot-path into the closed step's structured output,
                          e.g. dwf_result.succeeded — the value at that path must
                          be a JSON array of scalars and becomes the value list.

on_complete output threading
----------------------------
A step that hosts a dynamic-workflow run writes a structured result back onto
its own bead before closing — by convention the typed field dwf_result, e.g.:

  {"schema":"dwf.result.v1","succeeded":["controller","kbeads"],
   "failed":[{"item":"coop","reason":"lint"}],"total":3,"run_id":"wf_abc"}

--from-field dwf_result.succeeded resolves the success array directly into the
iteration values, so the host pod no longer has to hand-join them into --values.

Scalars from the same result can be threaded onto every expanded molecule using
{{step.output.X}} placeholders in expand_vars or --var, where X is a dot-path
into dwf_result. For example expand_vars {"run_id": "{{step.output.run_id}}"}
stamps wf_abc onto each molecule. Unresolvable {{step.output.X}} refs are left
in place (like any other unmatched placeholder).

Use this command to:
- Thread a closed step's dwf_result into a downstream expansion
- Manually trigger deferred expansions during development
- Simulate what the orchestrator does when a step completes

Examples:
  kd formula expand ka-scanStep --values auth,api,web
  kd formula expand ka-scanStep --from-field dwf_result.succeeded
  kd formula expand ka-scanStep --from-field dwf_result.succeeded --dry-run
  kd formula expand ka-scanStep --values auth,api,web --var env=prod`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			values, _ := cmd.Flags().GetString("values")
			fromField, _ := cmd.Flags().GetString("from-field")
			varPairs, _ := cmd.Flags().GetStringArray("var")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			return runExpand(expandOpts{
				StepBeadID: args[0],
				Values:     values,
				FromField:  fromField,
				VarPairs:   varPairs,
				DryRun:     dryRun,
			})
		},
	}
	cmd.Flags().String("values", "", "comma-separated values for on_complete iteration")
	cmd.Flags().String("from-field", "", "dot-path into the step bead's fields whose array value supplies the iteration values (e.g. dwf_result.succeeded)")
	// StringArray keeps comma-containing var values intact (ka-0000003349).
	cmd.Flags().StringArray("var", nil, "additional variable overrides (key=value, repeatable; one value per flag, commas kept verbatim)")
	cmd.Flags().Bool("dry-run", false, "preview expansion without creating beads")
	return cmd
}

func runExpand(opts expandOpts) error {
	ctx := context.Background()

	if opts.Values != "" && opts.FromField != "" {
		return fmt.Errorf("--values and --from-field are mutually exclusive: pick one source for the iteration values")
	}
	if opts.Values == "" && opts.FromField == "" {
		return fmt.Errorf("one of --values or --from-field is required: provide the iteration values or resolve them from a step field")
	}

	// Fetch the step bead and extract on_complete spec.
	stepBead, err := beadsClient.GetBead(ctx, opts.StepBeadID)
	if err != nil {
		return fmt.Errorf("fetching step bead: %w", err)
	}

	// Resolve the iteration values from the chosen source.
	var values []string
	if opts.FromField != "" {
		values, err = resolveFieldValues(stepBead.Fields, opts.FromField)
		if err != nil {
			return fmt.Errorf("--from-field %s: %w", opts.FromField, err)
		}
		if len(values) == 0 {
			fmt.Printf("--from-field %s resolved to an empty array; nothing to expand\n", opts.FromField)
			return nil
		}
	} else {
		values = strings.Split(opts.Values, ",")
		for i := range values {
			values[i] = strings.TrimSpace(values[i])
		}
	}

	var stepFields struct {
		OnComplete *struct {
			ForEach    string            `json:"for_each"`
			Var        string            `json:"var"`
			Expand     string            `json:"expand"`
			ExpandVars map[string]string `json:"expand_vars"`
		} `json:"on_complete"`
		SourceFormula string `json:"source_formula"`
		SourceStepID  string `json:"source_step_id"`
	}
	if len(stepBead.Fields) > 0 {
		if err := json.Unmarshal(stepBead.Fields, &stepFields); err != nil {
			return fmt.Errorf("parsing step bead fields: %w", err)
		}
	}

	if stepFields.OnComplete == nil {
		return fmt.Errorf("step bead %s has no on_complete handler", opts.StepBeadID)
	}

	oc := stepFields.OnComplete

	// Parse additional --var overrides, resolving any {{step.output.X}} refs
	// against this step's structured output (dwf_result) — these are
	// loop-invariant so we resolve them once, up front.
	baseVars := make(map[string]string, len(opts.VarPairs))
	for _, p := range opts.VarPairs {
		k, v, ok := splitField(p)
		if !ok {
			return fmt.Errorf("invalid --var %q: expected key=value", p)
		}
		baseVars[k] = resolveStepOutputRefs(v, stepBead.Fields)
	}

	// Pre-resolve {{step.output.X}} in the on_complete expand_vars too.
	ocExpandVars := make(map[string]string, len(oc.ExpandVars))
	for k, v := range oc.ExpandVars {
		ocExpandVars[k] = resolveStepOutputRefs(v, stepBead.Fields)
	}

	// Merge expand_vars from on_complete spec into base vars.
	for k, v := range ocExpandVars {
		if _, exists := baseVars[k]; !exists {
			baseVars[k] = v
		}
	}

	if opts.DryRun {
		fmt.Printf("Step: %s (%s)\n", stepBead.Title, opts.StepBeadID)
		fmt.Printf("Expansion formula: %s\n", oc.Expand)
		fmt.Printf("Loop variable: %s\n", oc.Var)
		if opts.FromField != "" {
			fmt.Printf("Values from %s (%d): %v\n\n", opts.FromField, len(values), values)
		} else {
			fmt.Printf("Values (%d): %v\n\n", len(values), values)
		}
		fmt.Println("Would expand:")
		for _, val := range values {
			iterVars := make(map[string]string, len(baseVars)+1)
			maps.Copy(iterVars, baseVars)
			iterVars[oc.Var] = val
			fmt.Printf("  Iteration %s=%s:\n", oc.Var, val)
			// Show the expand_vars after substitution.
			for k, v := range ocExpandVars {
				resolved := substituteVars(v, iterVars)
				fmt.Printf("    %s=%s\n", k, resolved)
			}
		}
		fmt.Printf("\nTotal: %d iterations × expansion %s\n", len(values), oc.Expand)
		return nil
	}

	// Fetch and validate the expansion formula.
	expansionBead, err := beadsClient.GetBead(ctx, oc.Expand)
	if err != nil {
		return fmt.Errorf("fetching expansion formula %s: %w", oc.Expand, err)
	}
	if string(expansionBead.Type) != "formula" {
		return fmt.Errorf("expansion target %s is type %q, not formula", oc.Expand, expansionBead.Type)
	}

	var expFields formulaFields
	if len(expansionBead.Fields) == 0 {
		return fmt.Errorf("expansion formula %s has no fields", oc.Expand)
	}
	if err := json.Unmarshal(expansionBead.Fields, &expFields); err != nil {
		return fmt.Errorf("parsing expansion formula %s: %w", oc.Expand, err)
	}
	if expFields.FormulaType != "expansion" {
		return fmt.Errorf("formula %s is type %q, not expansion", oc.Expand, expFields.FormulaType)
	}

	// Iterate over values and create expanded steps.
	totalCreated := 0
	for _, val := range values {
		iterVars := make(map[string]string, len(baseVars)+1)
		maps.Copy(iterVars, baseVars)
		iterVars[oc.Var] = val

		// Substitute expand_vars.
		resolvedExpandVars := make(map[string]string, len(ocExpandVars))
		for k, v := range ocExpandVars {
			resolvedExpandVars[k] = substituteVars(v, iterVars)
		}

		// Build merged vars for expansion.
		mergedVars := make(map[string]string, len(iterVars)+len(resolvedExpandVars))
		maps.Copy(mergedVars, iterVars)
		maps.Copy(mergedVars, resolvedExpandVars)

		// Apply expansion formula var defaults.
		for _, vd := range expFields.Vars {
			if _, ok := mergedVars[vd.Name]; !ok && vd.Default != "" {
				mergedVars[vd.Name] = vd.Default
			}
		}

		// Merge needs into depends_on.
		mergeNeedsRecursive(expFields.Steps)

		// Create step beads from expansion. Step titles/descriptions may
		// reference {{step.output.X}} too — resolve those against the host
		// step's structured output before applying the per-iteration vars.
		prefix := fmt.Sprintf("%s/%s", stepFields.SourceStepID, val)
		for _, s := range expFields.Steps {
			stepID := prefix + "/" + s.ID
			title := substituteVars(resolveStepOutputRefs(s.Title, stepBead.Fields), mergedVars)
			shortTitle := substituteVars(resolveStepOutputRefs(s.ShortTitle, stepBead.Fields), mergedVars)
			desc := substituteVars(resolveStepOutputRefs(s.Description, stepBead.Fields), mergedVars)

			typ := s.Type
			if typ == "" {
				typ = "task"
			}

			sf := map[string]any{
				"source_formula": oc.Expand,
				"source_step_id": s.ID,
				"expansion_var":  val,
			}

			// Step-role dispatch opt-in (ka-0000003347). The controller's
			// FormulaStepDispatcher (formula_step_dispatch.go) spawns a
			// role-matched agent when a step's last blocking dep clears, but
			// ONLY if the step bead carries suggest_new_agent=true — that field
			// is the hard eligibility gate (stepDispatchEligible). The bridge
			// pour (formula_instantiate.go), CLI apply (formula_apply.go), and
			// daemon HTTP pour (http_formula_pour.go) all stamp it; this
			// on_complete deferred-expansion path (a FOURTH pour) never did, so
			// a fanned-out step a formula opts in could never dispatch. Mirror
			// the other pours so on_complete steps dispatch identically.
			//
			// suggest_new_agent is a native JSON boolean (the schema type
			// registered in config.go) — createBead validates it at create
			// time, unlike a string PATCH which 400s at the daemon validator
			// (ka-0000002741). This deferred path has no molecule and no
			// cross-pour edges, so — like the CLI apply and HTTP pour — every
			// opted-in step is stamped at create with no deferral.
			if s.SuggestNewAgent {
				sf["suggest_new_agent"] = true
				// step_role is the resolved dispatch target the dispatcher reads
				// (stepDispatchRole) so it never re-parses role: labels. Step
				// roles override the expansion formula's default_roles; the
				// first non-empty resolved role wins — a pod runs exactly one
				// role, and the dispatcher only ever spawns for the first.
				stepRoles := s.Roles
				if len(stepRoles) == 0 {
					stepRoles = expFields.DefaultRoles
				}
				for _, role := range stepRoles {
					if r := strings.TrimSpace(substituteVars(role, mergedVars)); r != "" {
						sf["step_role"] = r
						break
					}
				}
			}

			sfJSON, _ := json.Marshal(sf)

			stepReq := &client.CreateBeadRequest{
				Title:       title,
				ShortTitle:  formula.ShortTitle(shortTitle, title),
				Description: desc,
				Type:        typ,
				Priority:    stepBead.Priority,
				Labels:      stepBead.Labels,
				CreatedBy:   actor,
				Fields:      sfJSON,
			}

			newBead, err := beadsClient.CreateBead(ctx, stepReq)
			if err != nil {
				return fmt.Errorf("creating expanded step %q: %w", stepID, err)
			}

			// Link as child of the trigger step.
			_, err = beadsClient.AddDependency(ctx, &client.AddDependencyRequest{
				BeadID:      newBead.ID,
				DependsOnID: opts.StepBeadID,
				Type:        "parent-child",
				CreatedBy:   actor,
			})
			if err != nil {
				return fmt.Errorf("linking expanded step %q to parent: %w", stepID, err)
			}

			fmt.Printf("  %s → %s: %s\n", stepID, newBead.ID, title)
			totalCreated++
		}
	}

	fmt.Printf("\nExpanded %d values × %d steps = %d beads created\n",
		len(values), len(expFields.Steps), totalCreated)
	return nil
}

// stepOutputPattern matches {{step.output.<dotpath>}} placeholders that thread a
// closed step's structured output (its dwf_result field) into expansion vars.
// The dotpath captures keys and nested object access, e.g. step.output.run_id
// or step.output.meta.region.
var stepOutputPattern = regexp.MustCompile(`\{\{step\.output\.([\w.]+)\}\}`)

// resolveStepOutputRefs replaces {{step.output.X}} placeholders in s with the
// scalar value found at dwf_result.X in the step bead's fields. Placeholders
// that cannot be resolved (missing field, non-scalar value) are left in place,
// matching substituteVars's leave-unmatched-as-is behavior.
func resolveStepOutputRefs(s string, fields json.RawMessage) string {
	if !strings.Contains(s, "{{step.output.") {
		return s
	}
	return stepOutputPattern.ReplaceAllStringFunc(s, func(match string) string {
		sub := stepOutputPattern.FindStringSubmatch(match)
		if len(sub) != 2 {
			return match
		}
		v, err := fieldValueAtPath(fields, "dwf_result."+sub[1])
		if err != nil {
			return match
		}
		str, err := scalarToString(v)
		if err != nil {
			return match
		}
		return str
	})
}

// resolveFieldValues resolves a dot-path into the step bead's fields, expecting
// a JSON array of scalars, and returns them as a string slice for iteration.
// This backs `kd formula expand --from-field`.
func resolveFieldValues(fields json.RawMessage, path string) ([]string, error) {
	v, err := fieldValueAtPath(fields, path)
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("value is not an array (got %T) — --from-field must point at a JSON array of values", v)
	}
	out := make([]string, 0, len(arr))
	for i, el := range arr {
		s, err := scalarToString(el)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w (--from-field array elements must be scalars)", i, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// fieldValueAtPath walks a dot-separated path into a parsed JSON fields blob and
// returns the value at that path. Numbers are decoded as json.Number so they
// render exactly as written rather than via float formatting.
func fieldValueAtPath(fields json.RawMessage, path string) (any, error) {
	if len(fields) == 0 {
		return nil, fmt.Errorf("step bead has no fields")
	}
	if path == "" {
		return nil, fmt.Errorf("empty field path")
	}
	dec := json.NewDecoder(bytes.NewReader(fields))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("parsing step bead fields: %w", err)
	}
	parts := strings.Split(path, ".")
	var cur any = root
	for i, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("path %q: %q is not an object", path, strings.Join(parts[:i], "."))
		}
		v, ok := m[p]
		if !ok {
			return nil, fmt.Errorf("path %q: key %q not found", path, p)
		}
		cur = v
	}
	return cur, nil
}

// scalarToString renders a JSON scalar (string, number, bool) as a string.
// Composite values (object, array, null) return an error.
func scalarToString(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case json.Number:
		return t.String(), nil
	case float64:
		// Fallback for blobs decoded without UseNumber.
		return strconv.FormatFloat(t, 'g', -1, 64), nil
	default:
		return "", fmt.Errorf("value is not a scalar (%T)", v)
	}
}
