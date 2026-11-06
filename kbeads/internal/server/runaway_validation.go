// Runaway-protection validation for formula and schedule beads.
//
// These checks run server-side at createBead time (before the bead is
// persisted) so that pathological definitions — enormous loops, deeply
// nested expansions, sub-minute crons — are rejected regardless of which
// client created them (gRPC, HTTP, escalation replay, formula pour).
//
// The caps below are conservative defaults: they are meant to reject
// obviously broken definitions, not to enforce project policy. If legitimate
// work trips these, the caps should be raised with a clear rationale.
package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Formula validation caps.
const (
	formulaMaxLoopCount       = 100 // per-step loop (values / count / range span)
	formulaMaxExpandDepth     = 5   // nested inline expansions
	formulaMaxTotalSteps      = 500 // cap on the flattened step tree
	formulaMaxOnCompleteDepth = 3   // nested on_complete expansions
)

// Schedule validation caps.
const (
	scheduleMinCronInterval = time.Minute // cron must fire no more often than once/minute
	scheduleMaxConcurrent   = 10          // hard ceiling on per-schedule concurrency
)

// validateFormulaFields parses a formula bead's fields and rejects definitions
// that exceed runaway-protection caps. Returns nil if the formula is either
// absent or within caps. The fields argument is the raw JSON from CreateBeadInput.
func validateFormulaFields(fields json.RawMessage) error {
	if len(fields) == 0 {
		return nil
	}
	var f struct {
		Steps []formulaStepCap `json:"steps"`
	}
	if err := json.Unmarshal(fields, &f); err != nil {
		// Structural JSON errors are caught by ValidateTypeFields; no-op here.
		return nil
	}
	if len(f.Steps) == 0 {
		return nil
	}

	total := 0
	if err := walkFormulaSteps(f.Steps, 0, 0, &total); err != nil {
		return err
	}
	if total > formulaMaxTotalSteps {
		return fmt.Errorf("formula has %d steps (max %d) — reduce step count or split into multiple formulas",
			total, formulaMaxTotalSteps)
	}
	return nil
}

// formulaStepCap is a minimal subset of FormulaStep used by runaway validation.
// Duplicated here (rather than shared from cmd/kd) to keep server layer free of
// CLI dependencies. Only the fields relevant to caps are decoded.
type formulaStepCap struct {
	ID         string           `json:"id"`
	Children   []formulaStepCap `json:"children,omitempty"`
	Loop       *loopCap         `json:"loop,omitempty"`
	Expand     string           `json:"expand,omitempty"`
	OnComplete *onCompleteCap   `json:"on_complete,omitempty"`
}

type loopCap struct {
	Values []string `json:"values,omitempty"`
	Count  int      `json:"count,omitempty"`
	Range  string   `json:"range,omitempty"`
}

type onCompleteCap struct {
	Expand string `json:"expand,omitempty"`
}

// walkFormulaSteps recursively checks each step's caps and tallies the total
// step count. expandDepth tracks the current expand nesting; onCompleteDepth
// tracks on_complete nesting. total is incremented per step seen (including
// children).
func walkFormulaSteps(steps []formulaStepCap, expandDepth, onCompleteDepth int, total *int) error {
	for _, s := range steps {
		*total++
		if *total > formulaMaxTotalSteps {
			return fmt.Errorf("formula exceeds %d total steps — reduce step count or split into multiple formulas",
				formulaMaxTotalSteps)
		}

		if s.Loop != nil {
			n, err := loopIterations(s.Loop)
			if err != nil {
				// Invalid range strings etc. are caught by the structural validator.
				n = 0
			}
			if n > formulaMaxLoopCount {
				return fmt.Errorf("step %q: loop has %d iterations (max %d)",
					s.ID, n, formulaMaxLoopCount)
			}
		}

		if s.Expand != "" {
			if expandDepth+1 > formulaMaxExpandDepth {
				return fmt.Errorf("step %q: expand nesting depth %d exceeds max %d",
					s.ID, expandDepth+1, formulaMaxExpandDepth)
			}
		}

		if s.OnComplete != nil && s.OnComplete.Expand != "" {
			if onCompleteDepth+1 > formulaMaxOnCompleteDepth {
				return fmt.Errorf("step %q: on_complete nesting depth %d exceeds max %d",
					s.ID, onCompleteDepth+1, formulaMaxOnCompleteDepth)
			}
		}

		if len(s.Children) > 0 {
			childExpand := expandDepth
			if s.Expand != "" {
				childExpand++
			}
			childOC := onCompleteDepth
			if s.OnComplete != nil && s.OnComplete.Expand != "" {
				childOC++
			}
			if err := walkFormulaSteps(s.Children, childExpand, childOC, total); err != nil {
				return err
			}
		}
	}
	return nil
}

// loopIterations returns the number of iterations a loop definition would
// produce. Returns 0 when indeterminate.
func loopIterations(l *loopCap) (int, error) {
	if l == nil {
		return 0, nil
	}
	if len(l.Values) > 0 {
		return len(l.Values), nil
	}
	if l.Count > 0 {
		return l.Count, nil
	}
	if l.Range != "" {
		parts := strings.SplitN(l.Range, "..", 2)
		if len(parts) != 2 {
			return 0, fmt.Errorf("invalid range")
		}
		start, err1 := parseIntTrim(parts[0])
		end, err2 := parseIntTrim(parts[1])
		if err1 != nil || err2 != nil || end < start {
			return 0, fmt.Errorf("invalid range")
		}
		return end - start + 1, nil
	}
	return 0, nil
}

func parseIntTrim(s string) (int, error) {
	s = strings.TrimSpace(s)
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// validateScheduleFields rejects schedule beads whose cron fires more often
// than once per minute or whose max_concurrent exceeds the per-schedule
// ceiling, and enforces that at least one of cron or triggers is set so the
// scheduler has something to fire on. cron field may be any expression
// understood by the scheduler; intervals are probed by comparing successive
// fire times from the current wall clock.
func validateScheduleFields(fields json.RawMessage) error {
	if len(fields) == 0 {
		return nil
	}
	var s struct {
		Cron          string          `json:"cron"`
		Triggers      json.RawMessage `json:"triggers"`
		MaxConcurrent int             `json:"max_concurrent"`
	}
	if err := json.Unmarshal(fields, &s); err != nil {
		return nil
	}

	if s.MaxConcurrent > scheduleMaxConcurrent {
		return fmt.Errorf("schedule max_concurrent=%d exceeds cap %d",
			s.MaxConcurrent, scheduleMaxConcurrent)
	}

	if s.Cron == "" && !hasTriggerContent(s.Triggers) {
		return fmt.Errorf("schedule requires cron or triggers")
	}

	if s.Cron != "" {
		if err := checkCronInterval(s.Cron); err != nil {
			return err
		}
	}
	return nil
}

// hasTriggerContent reports whether the raw triggers value carries any
// usable content. Empty strings, empty arrays, and JSON null all count as
// absent so a `triggers: ""` placeholder does not satisfy the cron-or-
// triggers requirement.
func hasTriggerContent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == `""` || trimmed == "[]" || trimmed == "{}" {
		return false
	}
	return true
}

// checkCronInterval enforces the minimum-interval invariant structurally:
// the kbeads scheduler uses standard 5-field cron (minute, hour, day,
// month, day-of-week), whose finest resolution is one minute. Rejecting
// 6+ field expressions rules out seconds-level crons that would fire
// sub-minute. This is a conservative check that doesn't require linking a
// cron parser into kbeads; full semantic parsing happens at scheduler
// evaluation time.
func checkCronInterval(expr string) error {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return nil
	}
	// Convenience aliases used by some cron implementations.
	switch trimmed {
	case "@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly":
		return nil
	}
	// Reject the @every alias at sub-minute intervals.
	if strings.HasPrefix(trimmed, "@every ") {
		dur := strings.TrimSpace(strings.TrimPrefix(trimmed, "@every "))
		d, err := time.ParseDuration(dur)
		if err == nil && d > 0 && d < scheduleMinCronInterval {
			return fmt.Errorf("schedule cron %q fires every %s — minimum interval is %s",
				expr, d, scheduleMinCronInterval)
		}
		return nil
	}
	fields := strings.Fields(trimmed)
	if len(fields) >= 6 {
		return fmt.Errorf("schedule cron %q has %d fields — only 5-field cron is supported (minimum interval %s)",
			expr, len(fields), scheduleMinCronInterval)
	}
	return nil
}

// validateTriggerFields rejects a type=trigger bead that will spawn agents
// (or spawn-producing formula pours) but carries no `project` field.
//
// Why the project FIELD, not the project: label or fleet: the controller
// derives BOAT_PROJECT / the per-operator OAuth secret / the pod's git clone
// from the trigger's `project` FIELD (bridge/triggers.go reads trig.Project).
// A project: label alone changed nothing — confirmed on the live root cause
// (ka-0000004625): a hand-created spawn_agent trigger with no project field
// spawned a loginless, repo-less agent that ran as a 14h zombie. Setting the
// field fixed it (ka-0000000254). The controller only WARNS about this at
// fire time (warnRepolessRepoPaths); this guard catches the misconfig at the
// source so the broken trigger never persists.
//
// Scope: only spawn-capable triggers are gated. A trigger spawns agents when
// its action is spawn_agent or apply_formula, or when it carries a `gardener`
// or multi-entry `triggers` block
// (gardeners and multi-triggers both fire spawn_agent through the same engine
// path and read the top-level project field — TriggerEntry has no per-entry
// project). action=notify / create_bead / assert triggers never spawn a pod
// and are left unconstrained.
func validateTriggerFields(fields json.RawMessage) error {
	if len(fields) == 0 {
		return nil
	}
	var t struct {
		Action   string          `json:"action"`
		Project  string          `json:"project"`
		Gardener json.RawMessage `json:"gardener"`
		Triggers json.RawMessage `json:"triggers"`
	}
	if err := json.Unmarshal(fields, &t); err != nil {
		// Malformed fields are caught by ValidateTypeFields upstream; don't
		// double-report here.
		return nil
	}

	spawnsAgent := t.Action == "spawn_agent" || t.Action == "apply_formula" ||
		hasTriggerContent(t.Gardener) || hasTriggerContent(t.Triggers)
	if !spawnsAgent {
		return nil
	}

	if strings.TrimSpace(t.Project) == "" {
		return fmt.Errorf(
			"agent-spawning trigger requires a non-empty `project` field " +
				"(the controller derives BOAT_PROJECT / the OAuth secret / the pod git clone " +
				"from it — a project: label or fleet is NOT enough); " +
				"set -f project=<name> so the spawn is not loginless",
		)
	}
	return nil
}
