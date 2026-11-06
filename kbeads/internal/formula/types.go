// Package formula is the canonical implementation of the formula
// instantiation pipeline (resolve → expand → create) shared by the kd CLI
// (`kd formula apply|pour|wisp`) and the daemon's HTTP pour endpoint
// (POST /v1/formulas/{id}/pour). It was extracted from cmd/kd so the server
// could pour formulas without a second (drifting) implementation — the CLI
// re-exports these types via aliases, so the CLI test suite remains the
// behavioral spec for this package.
package formula

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// VarDef defines a variable that a formula accepts.
type VarDef struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Default     string   `json:"default,omitempty"`
	Type        string   `json:"type,omitempty"` // string, int, bool
	Enum        []string `json:"enum,omitempty"`
	Pattern     string   `json:"pattern,omitempty"` // regex pattern for value validation
}

// VarDefList is a tolerant []VarDef. Some formula beads were persisted with
// their `vars` field as a JSON STRING (a double-encoded array) rather than a
// native array — e.g. by an older write path or a dump/round-trip that
// stringified the value. A strict []VarDef unmarshal of such a bead fails,
// which broke `formula apply --dry-run` and the dump→create round-trip on the
// DB-load path (bug ka-0000001116). UnmarshalJSON accepts BOTH forms: a native
// array, or a JSON string that itself contains the array (decoded once more).
// It marshals back as a native array, so re-writing a tolerated bead
// normalizes it. The empty/null cases yield a nil slice, identical to the
// strict type's behavior.
type VarDefList []VarDef

func (v *VarDefList) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*v = nil
		return nil
	}
	// Native array form (the normal case).
	if trimmed[0] == '[' {
		var arr []VarDef
		if err := json.Unmarshal(data, &arr); err != nil {
			return err
		}
		*v = arr
		return nil
	}
	// Legacy string form: the value is a JSON string whose contents are the
	// array. Decode the string once, then parse its payload as the array.
	if trimmed[0] == '"' {
		var inner string
		if err := json.Unmarshal(data, &inner); err != nil {
			return err
		}
		inner = strings.TrimSpace(inner)
		if inner == "" || inner == "null" {
			*v = nil
			return nil
		}
		var arr []VarDef
		if err := json.Unmarshal([]byte(inner), &arr); err != nil {
			return fmt.Errorf("vars stored as string but not a valid var-array: %w", err)
		}
		*v = arr
		return nil
	}
	return fmt.Errorf("vars must be a JSON array or a JSON-string-encoded array, got %.40s", trimmed)
}

// StepList is a tolerant []Step. Like VarDefList (ka-0000001116), some formula
// beads were persisted with their `steps` field as a JSON STRING (a
// double-encoded array) rather than a native array (ka-0000001118).
// UnmarshalJSON accepts both forms; see VarDefList.
type StepList []Step

func (s *StepList) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*s = nil
		return nil
	}
	// Native array form (the normal case).
	if trimmed[0] == '[' {
		var arr []Step
		if err := json.Unmarshal(data, &arr); err != nil {
			return err
		}
		*s = arr
		return nil
	}
	// Legacy string form: the value is a JSON string whose contents are the
	// array. Decode the string once, then parse its payload as the array.
	if trimmed[0] == '"' {
		var inner string
		if err := json.Unmarshal(data, &inner); err != nil {
			return err
		}
		inner = strings.TrimSpace(inner)
		if inner == "" || inner == "null" {
			*s = nil
			return nil
		}
		var arr []Step
		if err := json.Unmarshal([]byte(inner), &arr); err != nil {
			return fmt.Errorf("steps stored as string but not a valid step-array: %w", err)
		}
		*s = arr
		return nil
	}
	return fmt.Errorf("steps must be a JSON array or a JSON-string-encoded array, got %.40s", trimmed)
}

// GateOption defines a choice for a decision gate.
type GateOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Gate defines a gate or decision point in a formula step.
type Gate struct {
	Type    string       `json:"type"`              // decision, sync, manual
	Prompt  string       `json:"prompt,omitempty"`  // question text (decision type)
	Options []GateOption `json:"options,omitempty"` // choices (decision type)
	Timeout string       `json:"timeout,omitempty"` // e.g. "24h"
}

// Loop defines iteration for a formula step.
type Loop struct {
	Var    string   `json:"var"`              // loop variable name
	Values []string `json:"values,omitempty"` // explicit list of values
	Count  int      `json:"count,omitempty"`  // iterate 0..count-1
	Range  string   `json:"range,omitempty"`  // "start..end" inclusive numeric range
}

// BranchCase defines a single case in a branch step — maps to an expansion formula.
type BranchCase struct {
	Expand     string            `json:"expand"`                // expansion formula ID
	ExpandVars map[string]string `json:"expand_vars,omitempty"` // variables to pass
}

// Branch defines a conditional expansion: evaluate a variable and expand the matching case.
type Branch struct {
	On      string                `json:"on"`                // variable expression to evaluate (e.g. "{{env}}")
	Cases   map[string]BranchCase `json:"cases"`             // value → expansion config
	Default *BranchCase           `json:"default,omitempty"` // fallback if no case matches
}

// OnComplete defines a deferred expansion that triggers when a step completes.
// The orchestrator (or kd formula expand) evaluates on_complete after the
// step's bead is closed, iterating over output values to spawn new steps.
type OnComplete struct {
	ForEach    string            `json:"for_each"`              // expression referencing step output (e.g. "{{scan.output.components}}")
	Var        string            `json:"var"`                   // loop variable name for each iteration
	Expand     string            `json:"expand"`                // expansion formula ID to instantiate per iteration
	ExpandVars map[string]string `json:"expand_vars,omitempty"` // variables to pass (can reference {{var}})
}

// Step defines a work item to create when a formula is applied.
type Step struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// ShortTitle is the <=40-rune name humans see for the step's bead
	// (model.ShortTitleHelp); {{vars}} are substituted like Title. When it is
	// empty the pour derives one from Title (ShortTitle in shorttitle.go).
	ShortTitle     string            `json:"short_title,omitempty"`
	Description    string            `json:"description,omitempty"`
	Type           string            `json:"type,omitempty"` // task, feature, bug, epic, chore
	Priority       *int              `json:"priority,omitempty"`
	Labels         []string          `json:"labels,omitempty"`
	DependsOn      []string          `json:"depends_on,omitempty"`
	Needs          []string          `json:"needs,omitempty"`       // alias for depends_on (human-friendly)
	Children       []Step            `json:"children,omitempty"`    // nested child steps (creates epic→task hierarchy)
	Loop           *Loop             `json:"loop,omitempty"`        // iterate to create multiple steps
	Gate           *Gate             `json:"gate,omitempty"`        // gate/decision point
	Expand         string            `json:"expand,omitempty"`      // expansion formula ID to inline
	ExpandVars     map[string]string `json:"expand_vars,omitempty"` // variables to pass to expansion
	Branch         *Branch           `json:"branch,omitempty"`      // conditional expansion based on variable value
	Assignee       string            `json:"assignee,omitempty"`
	Condition      string            `json:"condition,omitempty"`
	Roles          []string          `json:"roles,omitempty"`           // role requirements for this step (overrides formula default_roles)
	RequiresSkills []string          `json:"requires_skills,omitempty"` // skill requirements → skill:<name> labels
	// SuggestNewAgent opts this step into the controller's step-role dispatcher
	// (gardenio FormulaStepDispatcher, ka-0000001730): when the step's last
	// blocking dep closes, spawn a fresh agent carrying its Roles rather than
	// relying on the molecule's driver still being alive to walk into it.
	//
	// Opt-in per step, because most formulas WANT one driver holding context
	// across every step; this is for the seams where the upstream role's
	// lifecycle provably ends before the downstream step begins (the §7→§8
	// handoff that silently dropped FSM-472, bug ka-0000001755).
	SuggestNewAgent bool        `json:"suggest_new_agent,omitempty"`
	WaitsFor        string      `json:"waits_for,omitempty"`   // fanout sync: "all-children" blocks until all children complete
	OnComplete      *OnComplete `json:"on_complete,omitempty"` // deferred expansion triggered when step completes
}

// Fields is the parsed content of a formula bead's Fields JSON.
type Fields struct {
	FormulaType    string     `json:"formula_type,omitempty"` // workflow (default), expansion, aspect
	Extends        []string   `json:"extends,omitempty"`      // parent formula IDs to inherit from
	Vars           VarDefList `json:"vars"`                   // tolerant: accepts array or legacy string form (ka-0000001116)
	Steps          StepList   `json:"steps"`                  // tolerant: accepts array or legacy string form (ka-0000001118)
	DefaultRoles   []string   `json:"default_roles,omitempty"`
	RequiresSkills []string   `json:"requires_skills,omitempty"` // formula-level skill requirements
	AssignedAgent  string     `json:"assigned_agent,omitempty"`
	Phase          string     `json:"phase,omitempty"` // recommended phase: "vapor" or "liquid"
	// Workflow-only: aspect formula IDs to apply during expansion.
	Aspects []string `json:"aspects,omitempty"`
	// Aspect-only fields:
	Match  string   `json:"match,omitempty"`  // glob pattern to match step IDs
	Before StepList `json:"before,omitempty"` // tolerant (ka-0000001118); steps injected before matched steps
	After  StepList `json:"after,omitempty"`  // tolerant (ka-0000001118); steps injected after matched steps
}

// ParseRange parses a "start..end" range string into start and end integers (inclusive).
func ParseRange(r string) (int, int, error) {
	parts := strings.SplitN(r, "..", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid range %q: expected start..end", r)
	}
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid range start %q: %w", parts[0], err)
	}
	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid range end %q: %w", parts[1], err)
	}
	if start > end {
		return 0, 0, fmt.Errorf("invalid range %q: start must be <= end", r)
	}
	return start, end, nil
}
