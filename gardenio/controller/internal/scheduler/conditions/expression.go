// Package conditions defines the shared condition DSL used by the scheduler's
// stop/exit conditions, the trigger engine's preflight/match expressions, and
// the progress evaluator's event matching.
//
// The DSL has three node kinds:
//
//   - Boolean composition (all/any/not).
//   - Daemon-side leaves (type=bead_query|gate|ready_queue|active_agents)
//     evaluated against the bead store.
//   - Field leaves (field/op/value) evaluated in-process against an event or
//     bead payload.
//
// Operators on field leaves: ==, !=, in, not_in, contains, present, absent,
// in_subtree_of. The in_subtree_of operator asks the daemon whether the field
// value is a descendant of Value via the parent-child dependency chain
// (beadsapi.Client.IsAncestor).
//
// Per the plan-of-record (gardenio/plans/epic-mention-looping-gardener.md §4.1)
// this package is the canonical home for Condition; the scheduler package
// re-exports the legacy Condition shape as a backward-compatible alias.
package conditions

import (
	"encoding/json"
	"strings"
)

// Expression is a recursive condition expression. Exactly one of the
// composition fields (All/Any/Not) MAY be set; if none is set the expression
// is interpreted as a leaf using the embedded Leaf fields.
//
// JSON shape:
//
//	{"all":[<expr>, <expr>]}                       // boolean AND
//	{"any":[<expr>, <expr>]}                       // boolean OR
//	{"not": <expr>}                                // boolean NOT
//	{"type":"bead_query","op":"==","value":"0"}    // daemon leaf
//	{"field":"parent","op":"==","value":"ka-X"}    // field leaf
type Expression struct {
	All []Expression `json:"all,omitempty"`
	Any []Expression `json:"any,omitempty"`
	Not *Expression  `json:"not,omitempty"`

	// Leaf is embedded so leaf fields render flat in JSON
	// ({"type":"bead_query",...} not {"leaf":{"type":"bead_query",...}}).
	Leaf
}

// Leaf is the per-leaf shape. Whether it is a daemon-side leaf or a field
// leaf is determined by which fields are populated:
//
//   - Type set, Field empty   ⇒ daemon-side (bead_query | gate |
//     ready_queue | active_agents).
//   - Field set, Type empty   ⇒ in-process field leaf.
//
// A leaf with neither Type nor Field is a no-op leaf and evaluates to true
// (used for the "no constraint" case).
type Leaf struct {
	// Type names a daemon-side leaf evaluator. One of:
	// "ready_queue", "active_agents", "gate", "bead_query", or
	// "quiet_gardeners" (the count of a land's building plans whose gardener
	// is quiet or timed, leaf_quiet_gardeners.go; Project names the land).
	Type string `json:"type,omitempty"`

	// Field names an in-process field leaf. Path syntax:
	//   "parent"                       — top-level field
	//   "raised_by_agent.task_id"      — dotted nested map lookup
	//   "label:project:gardenio"        — checks payload["labels"] for membership
	Field string `json:"field,omitempty"`

	// Op is the comparison operator. The valid set depends on the leaf kind:
	//   daemon: > >= < <= == != eq ne   (gate also accepts present | absent)
	//   field:  == != in not_in contains present absent in_subtree_of
	Op string `json:"op,omitempty"`

	// Value is the comparison RHS. Integer for count-based daemon leaves;
	// title for "gate"; bead-id for "in_subtree_of"; literal for ==/!=.
	Value string `json:"value,omitempty"`

	// Values is the RHS for set operators (in / not_in).
	Values []string `json:"values,omitempty"`

	// --- Daemon-side filters (bead_query / ready_queue / active_agents) ---

	// Project filters to a specific project (optional).
	Project string `json:"project,omitempty"`
	// Role filters active_agents by role (optional).
	Role string `json:"role,omitempty"`
	// Parent filters bead_query to descendants of the given bead id
	// (optional). Wired by T2 — accepted by parser today, no-op until then.
	Parent string `json:"parent,omitempty"`
	// Labels filters bead_query by labels (optional).
	Labels []string `json:"labels,omitempty"`
	// ExcludeLabels drops beads carrying ANY of these labels from a
	// bead_query count (optional). Lets the gardener-delivery cron preflight
	// count open MRs while skipping operator-flagged opt-out MRs.
	ExcludeLabels []string `json:"exclude_labels,omitempty"`
	// ExcludeBlockedByOpenTypes subtracts from a bead_query count any matched
	// bead that is blocked_by an OPEN bead whose type is in this set (e.g.
	// ["escalation","decision"]). Empty (the default) keeps the legacy
	// count-only behaviour — fully backward-compatible. It lets a gardener
	// loop_end distinguish open+waiting (legitimately blocked on an operator
	// escalation/decision) from open+stalled (needs work): an externally
	// blocked child no longer keeps the gardener looping forever
	// (ka-0000002198). Fail-safe: any uncertainty (missing client capability,
	// transport error, scan truncation) excludes NOTHING, so a gardener never
	// stops while real work remains.
	ExcludeBlockedByOpenTypes []string `json:"exclude_blocked_by_open_types,omitempty"`
	// ShortTitleMissing narrows a bead_query to beads whose short_title is
	// unset (optional). The short-title groomer's pre-flight uses it so the
	// cron spawns a Haiku pod only while there is something to fill
	// (gd-V5nGseRS5f).
	ShortTitleMissing bool `json:"short_title_missing,omitempty"`
	// Types filters bead_query by bead types (optional).
	Types []string `json:"types,omitempty"`
	// Statuses filters bead_query by statuses (optional, defaults to ["open"]).
	Statuses []string `json:"statuses,omitempty"`
	// HeldBy counts open+in_progress escalations held by these lane refs. A
	// lane spans every project, so any other filter is an error (Types may only
	// be ["escalation"]); gardenio/docs/escalation-coordinator-prompt.md.
	HeldBy []string `json:"held_by,omitempty"`

	// --- Time-window leaf (type=time_window) ---

	// Timezone is the IANA zone name the time_window leaf reads the wall clock
	// in (e.g. "America/Los_Angeles"). Empty ⇒ UTC. Resolved via
	// time.LoadLocation, which needs embedded tzdata — present in the
	// controller, absent in shells. Also reused as a per-entry cron timezone
	// override in scheduler.TriggerEntry (a different struct); here it is only
	// consumed by the time_window leaf.
	Timezone string `json:"timezone,omitempty"`
	// Window is the recurring daily clock range "HH:MM-HH:MM" (24-hour local
	// time) the time_window leaf gates on. A range where start > end wraps past
	// midnight. Consumed only by the time_window leaf.
	Window string `json:"window,omitempty"`
	// Days restricts the time_window to a set of weekdays (e.g.
	// ["Mon","Tue","Wed","Thu","Fri"], or the shorthands "weekdays"/"weekends").
	// Empty ⇒ every day. Consumed only by the time_window leaf.
	Days []string `json:"days,omitempty"`

	// --- Script leaf (type=script) ---

	// ScriptBead is the id of the type=script bead whose most recent COMPLETED
	// probe verdict this leaf reads (probe-pods plan ka-0000000544, increment
	// 4 / ka-0000006663). It is the ONLY field the "script" daemon leaf
	// consumes. The leaf never executes anything — a probe pod (increment 1,
	// `gb probe run`) runs the script out-of-band and stamps exit_code /
	// agent_state on this bead; the leaf reads that stamped verdict. This keeps
	// the conditions package pure (no os/exec, no net/http) exactly as the two
	// hard constraints on the task require. Op defaults to "nonzero".
	ScriptBead string `json:"script_bead,omitempty"`
}

// IsComposition reports whether this expression is a boolean composition
// (any of all/any/not is set).
func (e Expression) IsComposition() bool {
	return len(e.All) > 0 || len(e.Any) > 0 || e.Not != nil
}

// IsLeaf reports whether this expression is a leaf. Compositions are not
// leaves; everything else is (including the empty no-op leaf).
func (e Expression) IsLeaf() bool {
	return !e.IsComposition()
}

// IsFieldLeaf reports whether this leaf is a field leaf. Only meaningful
// when IsLeaf() is true.
func (e Expression) IsFieldLeaf() bool {
	return e.Field != ""
}

// IsDaemonLeaf reports whether this leaf is a daemon-side leaf. Only
// meaningful when IsLeaf() is true.
func (e Expression) IsDaemonLeaf() bool {
	return e.Type != ""
}

// ParseExpression parses one expression from a JSON string.
func ParseExpression(raw string) (Expression, error) {
	var e Expression
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &e); err != nil {
		return Expression{}, err
	}
	return e, nil
}

// ParseExpressions parses a JSON array of expressions. Unlike ParseConditions
// (which lowers into the legacy Condition shape that has NO `field` member and
// therefore silently drops every field-leaf condition to a no-op leaf), this
// preserves field leaves, daemon leaves, AND boolean compositions intact. Use
// it wherever a caller must inspect the FULL condition tree — e.g. to tell a
// store-evaluable daemon gate apart from an event-payload field gate — rather
// than only evaluate it against the store.
func ParseExpressions(raw string) ([]Expression, error) {
	var es []Expression
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &es); err != nil {
		return nil, err
	}
	return es, nil
}

// ReferencesEventPayload reports whether this expression tree contains any
// field leaf (Field set) anywhere in its composition. A field leaf can only be
// evaluated against an in-process event/bead payload: it has no meaning against
// a point-in-time store snapshot with no payload, because lookupField returns
// not-present for every path, so every non-present-style op is false and
// present/absent are meaningless. A gate probe that AND-evaluates such
// conditions against an empty payload therefore produces a garbage verdict, and
// must instead report that it cannot evaluate the gate. Daemon leaves (Type
// set) and pure no-op leaves are store-evaluable and do not trip this.
func (e Expression) ReferencesEventPayload() bool {
	if e.IsComposition() {
		for _, c := range e.All {
			if c.ReferencesEventPayload() {
				return true
			}
		}
		for _, c := range e.Any {
			if c.ReferencesEventPayload() {
				return true
			}
		}
		if e.Not != nil && e.Not.ReferencesEventPayload() {
			return true
		}
		return false
	}
	return e.IsFieldLeaf()
}

// Condition is the legacy single-leaf shape preserved for backward
// compatibility with scheduler.Condition. New code should use Expression.
type Condition struct {
	Type                      string   `json:"type"`
	Op                        string   `json:"op"`
	Value                     string   `json:"value"`
	Project                   string   `json:"project,omitempty"`
	Role                      string   `json:"role,omitempty"`
	Labels                    []string `json:"labels,omitempty"`
	ExcludeLabels             []string `json:"exclude_labels,omitempty"`
	ExcludeBlockedByOpenTypes []string `json:"exclude_blocked_by_open_types,omitempty"`
	ShortTitleMissing         bool     `json:"short_title_missing,omitempty"`
	Types                     []string `json:"types,omitempty"`
	Statuses                  []string `json:"statuses,omitempty"`
	HeldBy                    []string `json:"held_by,omitempty"`
	Parent                    string   `json:"parent,omitempty"`
	ScriptBead                string   `json:"script_bead,omitempty"`
	Timezone                  string   `json:"timezone,omitempty"`
	Window                    string   `json:"window,omitempty"`
	Days                      []string `json:"days,omitempty"`
}

// ToExpression lowers a legacy Condition into an Expression leaf.
func (c Condition) ToExpression() Expression {
	return Expression{
		Leaf: Leaf{
			Type:                      c.Type,
			Op:                        c.Op,
			Value:                     c.Value,
			Project:                   c.Project,
			Role:                      c.Role,
			Labels:                    c.Labels,
			ExcludeLabels:             c.ExcludeLabels,
			ExcludeBlockedByOpenTypes: c.ExcludeBlockedByOpenTypes,
			ShortTitleMissing:         c.ShortTitleMissing,
			Types:                     c.Types,
			Statuses:                  c.Statuses,
			HeldBy:                    c.HeldBy,
			Parent:                    c.Parent,
			ScriptBead:                c.ScriptBead,
			Timezone:                  c.Timezone,
			Window:                    c.Window,
			Days:                      c.Days,
		},
	}
}

// ParseCondition unmarshals a single legacy Condition from a JSON string.
func ParseCondition(raw string) (Condition, error) {
	var c Condition
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &c); err != nil {
		return Condition{}, err
	}
	return c, nil
}

// ParseConditions unmarshals a JSON array of legacy Conditions.
func ParseConditions(raw string) ([]Condition, error) {
	var cs []Condition
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &cs); err != nil {
		return nil, err
	}
	return cs, nil
}
