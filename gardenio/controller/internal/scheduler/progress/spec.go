// Package progress implements the schedule-bead `progress:` instrumentation
// described in gardenio/plans/epic-mention-looping-gardener.md §3.
//
// A schedule bead may carry a `progress` JSON field shaped as:
//
//	{
//	  "events":  [ {kind, ...filter fields} ],
//	  "windows": [ {name, scope, n?, duration?} ],
//	  "surface": {in_prime, in_squawk_on_loop_end, in_wrapup}
//	}
//
// Events name what counts as forward progress; windows name the
// time-or-run scopes against which counts are aggregated; surface
// controls where the rendered progress shows up (the prime block, the
// loop-end squawk, and the wrap-up enforcement).
//
// This file defines the on-disk shape and parsing. Counters and event
// records live in `state.go`; daemon-side evaluation in `evaluator.go`;
// markdown rendering in `render.go`.
package progress

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Event kinds enumerate progress-event sources. Each kind maps to a
// specific daemon-side query in evaluator.go. Adding a new kind means
// adding a new case in the evaluator AND in the matcher's filter shape.
const (
	EventKindChildClosed      = "child_closed"
	EventKindDecisionResolved = "decision_resolved"
	EventKindMRMerged         = "mr_merged"
	EventKindCrewDispatched   = "crew_dispatched"
	EventKindWrapupField      = "wrapup_field"
)

// Window scopes describe how counts are aggregated for one window entry.
const (
	WindowScopeSinceRunStart = "since_run_start"
	WindowScopeLastNRuns     = "last_n_runs"
	WindowScopeSince         = "since"
)

// EventMatcher is one entry in the spec's `events:` array. Different
// kinds use different subsets of these fields; unused fields stay
// zero-valued. Field meanings:
//
//	Parent                    — restrict to descendants of this bead
//	                            (template `{{assign_bead}}` resolved
//	                            against the schedule at evaluation time).
//	RaisedByAgentSubtreeOf    — for decision_resolved: only count
//	                            decisions raised by agents under this
//	                            bead.
//	AuthorLabel               — for mr_merged: only count MRs whose
//	                            gardener-label matches.
//	Role                      — for crew_dispatched: filter by role:.
//	Field/Op/Value            — for wrapup_field: count runs whose
//	                            wrap-up has Field <op> Value.
type EventMatcher struct {
	Kind                   string `json:"kind"`
	Parent                 string `json:"parent,omitempty"`
	RaisedByAgentSubtreeOf string `json:"raised_by_agent_subtree_of,omitempty"`
	AuthorLabel            string `json:"author_label,omitempty"`
	Role                   string `json:"role,omitempty"`
	Field                  string `json:"field,omitempty"`
	Op                     string `json:"op,omitempty"`
	Value                  string `json:"value,omitempty"`
}

// Window is one named aggregation window. Scope is one of
// WindowScope*; N is required for last_n_runs; Duration is required
// for since. Empty Name causes ParseSpec to reject the spec.
type Window struct {
	Name     string `json:"name"`
	Scope    string `json:"scope"`
	N        int    `json:"n,omitempty"`
	Duration string `json:"duration,omitempty"`
}

// ParseDuration returns Duration parsed as a Go duration.
// Returns 0 with no error when Duration is empty.
func (w Window) ParseDuration() (time.Duration, error) {
	if strings.TrimSpace(w.Duration) == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(w.Duration)
	if err != nil {
		return 0, fmt.Errorf("window %q duration: %w", w.Name, err)
	}
	return d, nil
}

// Surface controls visibility of progress to the operator and the
// gardener. All zero-value flags mean "do not surface" — the spec is
// recorded but does not feed prime / squawk / wrap-up enforcement.
type Surface struct {
	InPrime           bool `json:"in_prime,omitempty"`
	InSquawkOnLoopEnd bool `json:"in_squawk_on_loop_end,omitempty"`
	InWrapup          bool `json:"in_wrapup,omitempty"`
}

// Spec is the parsed `progress` JSON field on a schedule bead.
type Spec struct {
	Events  []EventMatcher `json:"events,omitempty"`
	Windows []Window       `json:"windows,omitempty"`
	Surface Surface        `json:"surface"`
}

// IsZero reports whether the spec carries no events, no windows, and
// no surface flags.
func (s Spec) IsZero() bool {
	return len(s.Events) == 0 && len(s.Windows) == 0 &&
		!s.Surface.InPrime && !s.Surface.InSquawkOnLoopEnd && !s.Surface.InWrapup
}

// ParseSpec parses the schedule bead's `progress` JSON field. Empty
// input returns a zero spec with no error so callers can treat
// "no field" as "no constraints". Validation is structural only —
// per-event filter validity is checked at evaluation time.
func ParseSpec(raw string) (Spec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Spec{}, nil
	}
	var spec Spec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return Spec{}, fmt.Errorf("progress: %w", err)
	}
	for i, w := range spec.Windows {
		if strings.TrimSpace(w.Name) == "" {
			return Spec{}, fmt.Errorf("progress: window[%d] missing name", i)
		}
		switch w.Scope {
		case WindowScopeSinceRunStart, WindowScopeLastNRuns, WindowScopeSince:
		case "":
			return Spec{}, fmt.Errorf("progress: window %q missing scope", w.Name)
		default:
			return Spec{}, fmt.Errorf("progress: window %q unknown scope %q", w.Name, w.Scope)
		}
		if w.Scope == WindowScopeLastNRuns && w.N <= 0 {
			return Spec{}, fmt.Errorf("progress: window %q scope=last_n_runs requires n>0", w.Name)
		}
		if w.Scope == WindowScopeSince {
			if _, err := w.ParseDuration(); err != nil {
				return Spec{}, err
			}
		}
	}
	for i, e := range spec.Events {
		if strings.TrimSpace(e.Kind) == "" {
			return Spec{}, fmt.Errorf("progress: events[%d] missing kind", i)
		}
	}
	return spec, nil
}

// ResolveTemplates expands `{{assign_bead}}` and `{{schedule_id}}`
// placeholders in the spec's filter fields against the supplied
// values. Returns a new Spec; the receiver is not modified.
func (s Spec) ResolveTemplates(assignBead, scheduleID string) Spec {
	out := s
	out.Events = make([]EventMatcher, len(s.Events))
	for i, e := range s.Events {
		e.Parent = expandTemplate(e.Parent, assignBead, scheduleID)
		e.RaisedByAgentSubtreeOf = expandTemplate(e.RaisedByAgentSubtreeOf, assignBead, scheduleID)
		e.AuthorLabel = expandTemplate(e.AuthorLabel, assignBead, scheduleID)
		out.Events[i] = e
	}
	return out
}

func expandTemplate(s, assignBead, scheduleID string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "{{assign_bead}}", assignBead)
	s = strings.ReplaceAll(s, "{{schedule_id}}", scheduleID)
	return s
}
