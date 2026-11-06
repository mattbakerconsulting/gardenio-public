package main

// GardenerStopSpecBuilder — a schema-driven authoring component for a gardener's
// stop spec (task ka-0000000897 / I2.3, epic ka-0000001611 "Gardener
// Configuration Surface"; builder-UX decision ka-0000004391 → "guided" form
// rows per condition type).
//
// Before this, the ONLY way to author a gardener's stop spec through the viewer
// was a raw JSON textarea (gardener_config.html) — the operator hand-authored
// engine JSON and, worse, could persist a spec that silently NEVER FIRES (a
// `max_runs` loop_end on a gardener whose run_budget is 0; a `wall_clock` with a
// zero/garbage duration; a `bead_query` with an unknown op). The engine treats
// each of those as not-met and loops forever — the exact "silent no-op" this
// component exists to catch.
//
// The component is three cooperating pieces, all keyed off ONE schema registry
// (stopKindSchemas) so the form, the validator, and the human-readable summary
// can never drift from each other:
//
//   1. buildStopSpecBuilderView — decode the current gardener.stop JSON into a
//      per-slot list of guided rows (loop_end / run_end / hard_stop) the
//      template renders as schema-driven form rows.
//   2. parseStopSpecFromForm     — read the guided-row form fields back into a
//      scheduler.StopSpec, canonicalised through the SAME parse types the
//      engine reads (scheduler.ParseStopSpec round-trips it).
//   3. validateStopSpec          — reject combos the engine would silently
//      no-op (blocking errors) and surface softer advisories.
//
// summarizeStopSpec renders the same spec as human-readable lines ("End the
// loop when the run budget is exhausted", "Kill the pod after 720h of
// wall-clock time") for the gardener edit form + the legible gardener card.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/scheduler"
)

// stopFieldType is the input widget a stop-entry field renders as.
type stopFieldType string

const (
	stopFieldText     stopFieldType = "text"     // free text (gate title, bead id, project)
	stopFieldDuration stopFieldType = "duration" // Go duration string (e.g. 720h)
	stopFieldInt      stopFieldType = "int"      // non-negative integer count/threshold
	stopFieldSelect   stopFieldType = "select"   // one-of (bead_query op)
	stopFieldList     stopFieldType = "list"     // comma-separated multi-value
)

// stopFieldSchema describes one editable field of a stop kind. Name is BOTH the
// form-field suffix and the StopEntry JSON key, so the schema is the single
// source of truth for round-tripping.
type stopFieldSchema struct {
	Name        string
	Label       string
	Type        stopFieldType
	Options     []string // for stopFieldSelect
	Placeholder string
	Help        string
	// Required means an empty value is a blocking validation error (the entry
	// would silently never fire without it).
	Required bool
}

// stopKindSchema describes one condition kind: the widget fields it needs and
// enough metadata to label, summarize, and slot-scope it.
type stopKindSchema struct {
	Kind   string
	Label  string
	Help   string
	Fields []stopFieldSchema
	// Slots the kind is meaningful in. Empty == all three.
	Slots []string
	// Advisory, when non-empty, is a persistent note shown under the row (e.g.
	// scheduler-only kinds not enforced on type=trigger gardeners).
	Advisory string
}

// ValidInSlot reports whether the kind may be placed in slot. Exported so the
// stop-builder template can filter the per-slot add menu.
func (k stopKindSchema) ValidInSlot(slot string) bool {
	if len(k.Slots) == 0 {
		return true
	}
	for _, s := range k.Slots {
		if s == slot {
			return true
		}
	}
	return false
}

// beadQueryOps are the count comparison operators conditions.CompareCount
// accepts (scheduler/conditions/eval.go). A bead_query entry with any other op
// is a silent no-op (CompareCount returns false), so the builder restricts the
// choice to exactly this set.
var beadQueryOps = []string{"==", "!=", "<", "<=", ">", ">="}

// stopKindSchemas is the ordered registry every stop-builder surface reads. The
// order is the add-a-condition menu order (most common first). It is a function
// (not a package var) so it is trivially safe to mutate the returned slice.
func stopKindSchemas() []stopKindSchema {
	return []stopKindSchema{
		{
			Kind:  scheduler.StopKindMaxRuns,
			Label: "Run budget reached",
			Help:  "Fires once the gardener has spawned its Run budget worth of runs. Needs a positive Run budget above — a 0/unset budget makes this a no-op.",
		},
		{
			Kind:  scheduler.StopKindWallClock,
			Label: "Wall-clock elapsed",
			Help:  "Fires after a fixed amount of wall-clock time since the gardener was created (or last revived).",
			Fields: []stopFieldSchema{
				{Name: "duration", Label: "Duration", Type: stopFieldDuration, Placeholder: "e.g. 720h", Required: true,
					Help: "Go duration (h/m/s). Must be positive."},
			},
		},
		{
			Kind:  scheduler.StopKindNoProgressFor,
			Label: "No progress for N runs (stale window)",
			Help:  "Fires when the last N finalized runs all scored zero progress — the gardener is spinning without moving the work.",
			Fields: []stopFieldSchema{
				{Name: "runs", Label: "Runs", Type: stopFieldInt, Placeholder: "e.g. 3", Required: true,
					Help: "Rolling window of runs (a positive integer)."},
			},
		},
		{
			Kind:  scheduler.StopKindConsecutiveFailures,
			Label: "Consecutive failures",
			Help:  "Fires when the gardener's consecutive-failure streak reaches the threshold.",
			Fields: []stopFieldSchema{
				{Name: "value", Label: "Threshold", Type: stopFieldInt, Placeholder: "e.g. 5", Required: true,
					Help: "A positive integer failure count."},
			},
		},
		{
			Kind:  scheduler.StopKindBeadQuery,
			Label: "Bead-query count",
			Help:  "Fires when the count of beads matching a query compares (op) against a threshold — e.g. \"open children of this epic == 0\" (all work done).",
			Fields: []stopFieldSchema{
				{Name: "op", Label: "Op", Type: stopFieldSelect, Options: beadQueryOps, Required: true,
					Help: "Count comparison operator."},
				{Name: "value", Label: "Count", Type: stopFieldInt, Placeholder: "e.g. 0", Required: true,
					Help: "Integer threshold the matched count is compared against."},
				{Name: "parent", Label: "Parent bead", Type: stopFieldText, Placeholder: "gd-…",
					Help: "Scope the query to descendants of this bead."},
				{Name: "project", Label: "Project", Type: stopFieldText, Placeholder: "gardenio"},
				{Name: "types", Label: "Types", Type: stopFieldList, Placeholder: "task, bug",
					Help: "Comma-separated bead types."},
				{Name: "statuses", Label: "Statuses", Type: stopFieldList, Placeholder: "open, in_progress",
					Help: "Comma-separated statuses."},
				{Name: "labels", Label: "Labels", Type: stopFieldList, Placeholder: "area:x",
					Help: "Comma-separated labels."},
				{Name: "exclude_blocked_by_open_types", Label: "Exclude blocked-by open types", Type: stopFieldList,
					Placeholder: "escalation, decision",
					Help:        "Subtract matches blocked_by an OPEN bead of these types (don't loop forever on operator-blocked work)."},
			},
		},
		{
			Kind:  scheduler.StopKindSpecificBeadClosed,
			Label: "Specific bead closed",
			Help:  "Fires when a named bead reaches status=closed.",
			Fields: []stopFieldSchema{
				{Name: "bead_id", Label: "Bead ID", Type: stopFieldText, Placeholder: "gd-…", Required: true},
			},
		},
		{
			Kind:  scheduler.StopKindGoalSatisfied,
			Label: "Goal satisfied (gardener declares done)",
			Help:  "Fires when the freshest run wrap-up recorded goal_satisfied=yes — the canonical \"gardener declares itself done\" signal.",
		},
		{
			Kind:  scheduler.StopKindGatePresent,
			Label: "Gate present",
			Help:  "Fires while an open gate bead with this exact title exists.",
			Fields: []stopFieldSchema{
				{Name: "value", Label: "Gate title", Type: stopFieldText, Placeholder: "exact gate title", Required: true},
			},
		},
		{
			Kind:  scheduler.StopKindGateAbsent,
			Label: "Gate absent",
			Help:  "Fires when NO open gate bead with this title exists.",
			Fields: []stopFieldSchema{
				{Name: "value", Label: "Gate title", Type: stopFieldText, Placeholder: "exact gate title", Required: true},
			},
		},
		{
			Kind:  scheduler.StopKindMRMerged,
			Label: "Block until MRs merged",
			Help:  "Parks the gardener dormant while any tracked MR under the parent is unmerged, waking it when all merge. Loop-end only (park, not close).",
			Slots: []string{scheduler.StopSlotLoopEnd},
			Fields: []stopFieldSchema{
				{Name: "parent", Label: "Parent bead", Type: stopFieldText, Placeholder: "gd-… (defaults to frontier epic)",
					Help: "Subtree whose MRs are tracked. Empty falls back to the gardener's frontier epic."},
			},
		},
		{
			Kind:  scheduler.StopKindManual,
			Label: "Manual (operator field set)",
			Help:  "Fires when a named field on the gardener bead is set — an operator's manual close switch.",
			Fields: []stopFieldSchema{
				{Name: "reason_field", Label: "Reason field", Type: stopFieldText, Placeholder: "operator_close_reason",
					Help: "Bead field whose presence trips the stop (defaults to operator_close_reason)."},
			},
		},
		{
			Kind:  scheduler.StopKindManualForce,
			Label: "Manual force (label present)",
			Help:  "Fires when a named label is present on the gardener bead.",
			Fields: []stopFieldSchema{
				{Name: "label", Label: "Label", Type: stopFieldText, Placeholder: "force-close",
					Help: "Label whose presence trips the stop (defaults to force-close)."},
			},
		},
	}
}

// schedulerOnlyStopKinds are kinds the free-function trigger evaluator treats as
// not-met (they need the legacy scheduler's run-state machinery). The builder
// still round-trips them when already present on a spec, but flags them.
var schedulerOnlyStopKinds = map[string]bool{
	scheduler.StopKindNoActionableState:        true,
	scheduler.StopKindYieldedThenWokenThenIdle: true,
}

// stopKindSchemaFor returns the schema for a kind, and ok=false for an unknown
// kind (round-tripped as a bare kind row with no guided fields).
func stopKindSchemaFor(kind string) (stopKindSchema, bool) {
	for _, k := range stopKindSchemas() {
		if k.Kind == kind {
			return k, true
		}
	}
	return stopKindSchema{}, false
}

// stopBuilderSlots is the fixed slot order the builder renders top-to-bottom.
// hard_stop is listed first because it is the highest-precedence brake, then
// loop_end (ends the loop), then run_end (ends one run).
var stopBuilderSlots = []struct {
	Slot  string
	Label string
	Help  string
}{
	{scheduler.StopSlotLoopEnd, "Loop end", "Any of these firing CLOSES the gardener (no more runs)."},
	{scheduler.StopSlotHardStop, "Hard stop", "Any of these firing KILLS the running pod and closes the gardener. Highest precedence."},
	{scheduler.StopSlotRunEnd, "Run end", "Any of these firing ends the CURRENT run (debits a run; the loop continues)."},
}

// --- View models -----------------------------------------------------------

// stopFieldValue is one populated field on a row, ready for the template.
type stopFieldValue struct {
	stopFieldSchema
	Value string
}

// stopRowView is one guided condition row: its kind, the schema-driven fields
// (populated with the current value), and rendering hints.
type stopRowView struct {
	Slot     string
	Index    int
	Kind     string
	KindOK   bool // false → unknown kind, rendered as a raw fallback
	Label    string
	Help     string
	Advisory string
	Fields   []stopFieldValue
	// Summary is the human-readable one-liner for this row.
	Summary string
}

// stopSlotView groups the rows for one slot.
type stopSlotView struct {
	Slot  string
	Label string
	Help  string
	Rows  []stopRowView
}

// stopSpecBuilderView is the whole component's render model.
type stopSpecBuilderView struct {
	Slots     []stopSlotView
	OnLoopEnd string
	// Kinds is the schema registry, for the add-a-condition menu + the
	// per-kind field templates the client clones.
	Kinds []stopKindSchema
	// Summary is the full human-readable rendering of the spec (all slots).
	Summary []string
	// RunBudget is the gardener's run budget, needed to validate the max_runs
	// no-op client-side too.
	RunBudget int
	// RawJSON is the pretty-printed canonical stop JSON, shown in the advanced
	// escape-hatch textarea so a power author can see/round-trip the spec the
	// guided rows produce.
	RawJSON string
	// Errors / Advisories from a validation pass (populated on a rejected save).
	Errors     []string
	Advisories []string
}

// buildStopSpecBuilderView decodes a gardener's stop spec into the guided-row
// view. runBudget is the gardener's run budget (max_runs gates on it, not on a
// per-entry field), used for the no-op validation summary.
func buildStopSpecBuilderView(spec scheduler.StopSpec, runBudget int) stopSpecBuilderView {
	v := stopSpecBuilderView{
		OnLoopEnd: spec.OnLoopEnd,
		Kinds:     builderMenuKinds(),
		RunBudget: runBudget,
	}
	for _, s := range stopBuilderSlots {
		entries := slotEntries(spec, s.Slot)
		rows := make([]stopRowView, 0, len(entries))
		for i, e := range entries {
			rows = append(rows, buildStopRowView(s.Slot, i, e))
		}
		v.Slots = append(v.Slots, stopSlotView{Slot: s.Slot, Label: s.Label, Help: s.Help, Rows: rows})
	}
	v.Summary = summarizeStopSpec(spec)
	if raw, err := stopSpecToJSON(spec); err == nil {
		v.RawJSON = prettyRawJSON(raw)
	}
	adv := validateStopSpec(spec, runBudget)
	v.Errors = adv.Errors
	v.Advisories = adv.Advisories
	return v
}

// builderMenuKinds is the schema list restricted to kinds an operator should be
// offered in the add menu (scheduler-only kinds are round-tripped but never
// offered fresh).
func builderMenuKinds() []stopKindSchema {
	all := stopKindSchemas()
	out := make([]stopKindSchema, 0, len(all))
	for _, k := range all {
		if schedulerOnlyStopKinds[k.Kind] {
			continue
		}
		out = append(out, k)
	}
	return out
}

// slotEntries returns the entries for a named slot.
func slotEntries(spec scheduler.StopSpec, slot string) []scheduler.StopEntry {
	switch slot {
	case scheduler.StopSlotLoopEnd:
		return spec.LoopEnd.Any
	case scheduler.StopSlotHardStop:
		return spec.HardStop.Any
	case scheduler.StopSlotRunEnd:
		return spec.RunEnd.Any
	}
	return nil
}

// buildStopRowView populates a single guided row from a parsed StopEntry.
func buildStopRowView(slot string, index int, e scheduler.StopEntry) stopRowView {
	row := stopRowView{Slot: slot, Index: index, Kind: e.Kind, Summary: summarizeStopEntry(slot, e)}
	sc, ok := stopKindSchemaFor(e.Kind)
	row.KindOK = ok
	if !ok {
		row.Label = e.Kind
		row.Help = "Unknown stop kind — preserved verbatim. The engine treats an unrecognised loop_end kind as terminal."
		return row
	}
	row.Label = sc.Label
	row.Help = sc.Help
	row.Advisory = sc.Advisory
	if schedulerOnlyStopKinds[e.Kind] {
		row.Advisory = "Not enforced on this gardener type (scheduler-only). Preserved, but treated as not-met."
	}
	for _, f := range sc.Fields {
		row.Fields = append(row.Fields, stopFieldValue{stopFieldSchema: f, Value: stopEntryFieldValue(e, f.Name)})
	}
	return row
}

// stopEntryFieldValue extracts the string form of a StopEntry field by its JSON
// name (the schema field Name). List fields join on ", ".
func stopEntryFieldValue(e scheduler.StopEntry, name string) string {
	switch name {
	case "duration":
		return e.Duration
	case "value":
		return e.Value
	case "op":
		return e.Op
	case "bead_id":
		return e.BeadID
	case "project":
		return e.Project
	case "parent":
		return e.Parent
	case "runs":
		if e.Runs == 0 {
			return ""
		}
		return strconv.Itoa(e.Runs)
	case "reason_field":
		return e.ReasonField
	case "label":
		return e.Label
	case "types":
		return strings.Join(e.Types, ", ")
	case "statuses":
		return strings.Join(e.Statuses, ", ")
	case "labels":
		return strings.Join(e.Labels, ", ")
	case "exclude_blocked_by_open_types":
		return strings.Join(e.ExcludeBlockedByOpenTypes, ", ")
	}
	return ""
}

// --- Form parsing -----------------------------------------------------------

// stopBuilderMarker is the hidden form field the guided-row edit form submits so
// the save handler knows to parse rows (vs the legacy raw-JSON textarea path).
const stopBuilderMarker = "stop_builder"

// stopBuilderMaxRows caps the per-slot row scan so a crafted form cannot force
// an unbounded loop. Far above any real stop spec.
const stopBuilderMaxRows = 64

// parseStopSpecFromForm reconstructs a scheduler.StopSpec from the guided-row
// form fields. Row fields are named stop_<slot>_<i>_<field>; a row exists when
// its stop_<slot>_<i>_kind field is present and non-empty. Rows are read in
// index order; gaps are skipped (a client that removes row 1 need not renumber).
func parseStopSpecFromForm(form url.Values) scheduler.StopSpec {
	var spec scheduler.StopSpec
	for _, s := range stopBuilderSlots {
		entries := parseSlotEntries(form, s.Slot)
		switch s.Slot {
		case scheduler.StopSlotLoopEnd:
			spec.LoopEnd.Any = entries
		case scheduler.StopSlotHardStop:
			spec.HardStop.Any = entries
		case scheduler.StopSlotRunEnd:
			spec.RunEnd.Any = entries
		}
	}
	// "close" is the engine default (scheduler.StopSpec.OnLoopEnd doc), so
	// normalise it to the omitted zero value — otherwise the builder would add
	// `"on_loop_end":"close"` to every spec (even ones authored without it),
	// breaking byte-equivalent round-trips and canonical-form comparisons.
	if v := strings.TrimSpace(form.Get("on_loop_end")); v != "" && v != "close" {
		spec.OnLoopEnd = v
	}
	return spec
}

func parseSlotEntries(form url.Values, slot string) []scheduler.StopEntry {
	var out []scheduler.StopEntry
	for i := 0; i < stopBuilderMaxRows; i++ {
		prefix := fmt.Sprintf("stop_%s_%d_", slot, i)
		kind := strings.TrimSpace(form.Get(prefix + "kind"))
		if kind == "" {
			continue
		}
		out = append(out, parseStopEntry(form, prefix, kind))
	}
	return out
}

// parseStopEntry builds one StopEntry from a row's fields. Only the fields the
// kind's schema declares are read, so a row that switched kinds does not leak a
// stale field from the previous kind. Unknown kinds keep just their Kind.
func parseStopEntry(form url.Values, prefix, kind string) scheduler.StopEntry {
	e := scheduler.StopEntry{Kind: kind}
	sc, ok := stopKindSchemaFor(kind)
	if !ok {
		return e
	}
	for _, f := range sc.Fields {
		raw := strings.TrimSpace(form.Get(prefix + f.Name))
		if raw == "" {
			continue
		}
		switch f.Name {
		case "duration":
			e.Duration = raw
		case "value":
			e.Value = raw
		case "op":
			e.Op = raw
		case "bead_id":
			e.BeadID = raw
		case "project":
			e.Project = raw
		case "parent":
			e.Parent = raw
		case "runs":
			// Best-effort parse; validateStopSpec re-checks and blocks a bad one.
			if n, err := strconv.Atoi(raw); err == nil {
				e.Runs = n
			}
		case "reason_field":
			e.ReasonField = raw
		case "label":
			e.Label = raw
		case "types":
			e.Types = splitCSV(raw)
		case "statuses":
			e.Statuses = splitCSV(raw)
		case "labels":
			e.Labels = splitCSV(raw)
		case "exclude_blocked_by_open_types":
			e.ExcludeBlockedByOpenTypes = splitCSV(raw)
		}
	}
	return e
}

// splitCSV splits a comma-separated list, trimming whitespace and dropping
// empties.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// --- Validation -------------------------------------------------------------

// stopValidation is the outcome of validateStopSpec: blocking Errors (the save
// is refused) and non-blocking Advisories (the save proceeds with a note).
type stopValidation struct {
	Errors     []string
	Advisories []string
}

// OK reports whether the spec is safe to persist (no blocking errors).
func (v stopValidation) OK() bool { return len(v.Errors) == 0 }

// validateStopSpec checks a stop spec for combinations the engine silently
// no-ops on — the core "no silent no-ops like max_runs=0" acceptance criterion.
// runBudget is the gardener's run budget (the max_runs kind gates on it).
//
// Each check names the exact slot+kind so the operator can fix the offending
// row rather than hunt through JSON.
func validateStopSpec(spec scheduler.StopSpec, runBudget int) stopValidation {
	var v stopValidation
	for _, s := range stopBuilderSlots {
		for i, e := range slotEntries(spec, s.Slot) {
			validateStopEntry(&v, s.Slot, i, e, runBudget)
		}
	}
	return v
}

// where formats a stable "loop_end row 1 (Run budget reached)" locator.
func stopEntryWhere(slot string, index int, kind string) string {
	label := kind
	if sc, ok := stopKindSchemaFor(kind); ok && sc.Label != "" {
		label = sc.Label
	}
	return fmt.Sprintf("%s row %d (%s)", slot, index+1, label)
}

func validateStopEntry(v *stopValidation, slot string, index int, e scheduler.StopEntry, runBudget int) {
	where := stopEntryWhere(slot, index, e.Kind)
	switch e.Kind {
	case "":
		v.Errors = append(v.Errors, fmt.Sprintf("%s row %d: pick a condition type.", slot, index+1))

	case scheduler.StopKindMaxRuns:
		// The canonical silent no-op: max_runs gates on the gardener's run
		// budget, NOT on a per-entry value. A 0/unset budget means it never
		// fires (engine: RunBudget > 0 && RunsSpawned >= RunBudget).
		if runBudget <= 0 {
			v.Errors = append(v.Errors, fmt.Sprintf(
				"%s will never fire: the \"Run budget reached\" condition needs a positive Run budget, but the budget is %d. Set a Run budget above, or remove this condition.",
				where, runBudget))
		}

	case scheduler.StopKindWallClock:
		if d, err := time.ParseDuration(strings.TrimSpace(e.Duration)); err != nil || d <= 0 {
			v.Errors = append(v.Errors, fmt.Sprintf(
				"%s will never fire: Duration %q is not a positive Go duration (e.g. 720h, 90m).", where, e.Duration))
		}

	case scheduler.StopKindNoProgressFor:
		if e.Runs <= 0 {
			v.Errors = append(v.Errors, fmt.Sprintf(
				"%s will never fire: Runs must be a positive integer (duration-based windows are not supported on this gardener type).", where))
		}

	case scheduler.StopKindConsecutiveFailures:
		if n, err := strconv.Atoi(strings.TrimSpace(e.Value)); err != nil || n <= 0 {
			v.Errors = append(v.Errors, fmt.Sprintf(
				"%s will never fire: Threshold %q must be a positive integer.", where, e.Value))
		}

	case scheduler.StopKindBeadQuery:
		if !isValidBeadQueryOp(e.Op) {
			v.Errors = append(v.Errors, fmt.Sprintf(
				"%s will never fire: Op %q is not one of %s.", where, e.Op, strings.Join(beadQueryOps, " ")))
		}
		if _, err := strconv.Atoi(strings.TrimSpace(e.Value)); err != nil {
			v.Errors = append(v.Errors, fmt.Sprintf(
				"%s will never fire: Count %q must be an integer.", where, e.Value))
		}
		if e.Parent == "" && e.Project == "" && len(e.Types) == 0 && len(e.Statuses) == 0 && len(e.Labels) == 0 {
			v.Advisories = append(v.Advisories, fmt.Sprintf(
				"%s is unscoped (no parent/project/types/statuses/labels) — it counts across the whole project. Add a scope to be safe.", where))
		}

	case scheduler.StopKindSpecificBeadClosed:
		if strings.TrimSpace(e.BeadID) == "" {
			v.Errors = append(v.Errors, fmt.Sprintf("%s will never fire: Bead ID is required.", where))
		}

	case scheduler.StopKindGatePresent, scheduler.StopKindGateAbsent:
		if strings.TrimSpace(e.Value) == "" {
			v.Errors = append(v.Errors, fmt.Sprintf("%s will never fire: Gate title is required.", where))
		}

	case scheduler.StopKindMRMerged:
		if slot != scheduler.StopSlotLoopEnd {
			v.Errors = append(v.Errors, fmt.Sprintf(
				"%s: \"Block until MRs merged\" is only valid in loop_end (it parks the gardener, it does not stop a run or kill a pod).", where))
		}

	case scheduler.StopKindGoalSatisfied, scheduler.StopKindManual, scheduler.StopKindManualForce:
		// Always well-formed (manual/manual_force default their field/label).

	case scheduler.StopKindNoActionableState, scheduler.StopKindYieldedThenWokenThenIdle:
		v.Advisories = append(v.Advisories, fmt.Sprintf(
			"%s is a scheduler-only kind — this gardener type treats it as not-met, so it will not fire. Consider removing it.", where))

	default:
		v.Advisories = append(v.Advisories, fmt.Sprintf(
			"%s: unknown kind %q — the engine treats an unrecognised loop_end kind as terminal (fail-safe close).", where, e.Kind))
	}
}

func isValidBeadQueryOp(op string) bool {
	for _, o := range beadQueryOps {
		if o == op {
			return true
		}
	}
	return false
}

// --- Human-readable summary -------------------------------------------------

// summarizeStopSpec renders the whole spec as human-readable lines, one per
// entry, grouped by slot. Empty spec → nil (caller renders "no stop conditions").
func summarizeStopSpec(spec scheduler.StopSpec) []string {
	if spec.IsZero() {
		return nil
	}
	var out []string
	for _, s := range stopBuilderSlots {
		for _, e := range slotEntries(spec, s.Slot) {
			out = append(out, summarizeStopEntry(s.Slot, e))
		}
	}
	if spec.OnLoopEnd != "" && spec.OnLoopEnd != "close" {
		out = append(out, fmt.Sprintf("On loop end: %s.", humanOnLoopEnd(spec.OnLoopEnd)))
	}
	return out
}

func humanOnLoopEnd(v string) string {
	switch v {
	case "notify_slack":
		return "notify Slack (do not close)"
	case "spawn_followup":
		return "spawn a follow-up"
	case "close", "":
		return "close the gardener"
	}
	return v
}

// slotVerb is the human clause prefix for a slot.
func slotVerb(slot string) string {
	switch slot {
	case scheduler.StopSlotLoopEnd:
		return "End the loop"
	case scheduler.StopSlotHardStop:
		return "Kill the pod"
	case scheduler.StopSlotRunEnd:
		return "End the run"
	}
	return "Stop"
}

// summarizeStopEntry renders one entry as a single human sentence.
func summarizeStopEntry(slot string, e scheduler.StopEntry) string {
	verb := slotVerb(slot)
	switch e.Kind {
	case scheduler.StopKindMaxRuns:
		return verb + " when the run budget is exhausted."
	case scheduler.StopKindWallClock:
		return fmt.Sprintf("%s after %s of wall-clock time.", verb, orPlaceholder(e.Duration, "an unset duration"))
	case scheduler.StopKindNoProgressFor:
		return fmt.Sprintf("%s after %s runs with no progress.", verb, intOrPlaceholder(e.Runs))
	case scheduler.StopKindConsecutiveFailures:
		return fmt.Sprintf("%s after %s consecutive failures.", verb, orPlaceholder(e.Value, "an unset threshold"))
	case scheduler.StopKindBeadQuery:
		return fmt.Sprintf("%s when the count of %s %s %s%s.", verb,
			beadQueryScope(e), orPlaceholder(e.Op, "?"), orPlaceholder(e.Value, "?"), beadQueryExclude(e))
	case scheduler.StopKindSpecificBeadClosed:
		return fmt.Sprintf("%s when %s is closed.", verb, orPlaceholder(e.BeadID, "an unset bead"))
	case scheduler.StopKindGoalSatisfied:
		return verb + " when the gardener declares its goal satisfied."
	case scheduler.StopKindGatePresent:
		return fmt.Sprintf("%s while gate %q is open.", verb, e.Value)
	case scheduler.StopKindGateAbsent:
		return fmt.Sprintf("%s when gate %q is absent.", verb, e.Value)
	case scheduler.StopKindMRMerged:
		return fmt.Sprintf("Sleep until MRs under %s merge.", orPlaceholder(e.Parent, "the frontier epic"))
	case scheduler.StopKindManual:
		return fmt.Sprintf("%s when field %q is set.", verb, orPlaceholder(e.ReasonField, "operator_close_reason"))
	case scheduler.StopKindManualForce:
		return fmt.Sprintf("%s when label %q is present.", verb, orPlaceholder(e.Label, "force-close"))
	case scheduler.StopKindNoActionableState:
		return verb + " when there is no actionable state (scheduler-only — not enforced here)."
	case scheduler.StopKindYieldedThenWokenThenIdle:
		return verb + " when yielded-then-woken-then-idle (scheduler-only — not enforced here)."
	default:
		return fmt.Sprintf("%s on %q.", verb, e.Kind)
	}
}

// beadQueryScope renders the "which beads" clause of a bead_query summary.
func beadQueryScope(e scheduler.StopEntry) string {
	var parts []string
	if len(e.Statuses) > 0 {
		parts = append(parts, strings.Join(e.Statuses, "/"))
	}
	if len(e.Types) > 0 {
		parts = append(parts, strings.Join(e.Types, "/"))
	} else {
		parts = append(parts, "bead")
	}
	scope := strings.Join(parts, " ") + "s"
	if len(e.Labels) > 0 {
		scope += " labelled " + strings.Join(e.Labels, "/")
	}
	if e.Parent != "" {
		scope += " under " + e.Parent
	} else if e.Project != "" {
		scope += " in " + e.Project
	}
	return scope
}

func beadQueryExclude(e scheduler.StopEntry) string {
	if len(e.ExcludeBlockedByOpenTypes) == 0 {
		return ""
	}
	return " (excluding those blocked by open " + strings.Join(e.ExcludeBlockedByOpenTypes, "/") + ")"
}

func orPlaceholder(s, placeholder string) string {
	if strings.TrimSpace(s) == "" {
		return placeholder
	}
	return s
}

func intOrPlaceholder(n int) string {
	if n <= 0 {
		return "an unset number of"
	}
	return strconv.Itoa(n)
}

// stopSpecToJSON marshals a StopSpec to compact CANONICAL JSON for persistence.
// A fully-empty spec marshals to nil (the omitted zero) so an operator who
// cleared every row stores no stop field rather than an empty-slot husk.
//
// It marshals through a local struct that omits EMPTY slots. scheduler.StopSpec
// tags its three slot fields WITHOUT omitempty, so a direct json.Marshal emits
// `"run_end":{}` (and `"loop_end":{}`/`"hard_stop":{}`) for slots the operator
// left empty — a husk that (a) diverges from the minimal hand-authored form the
// rest of the fleet writes and (b) breaks byte-equivalent round-trip assertions.
// Emitting only the populated slots keeps the builder's output identical to a
// hand-authored spec. scheduler.ParseStopSpec reads either form identically
// (empty slots decode to empty StopSlot regardless), so this is purely a
// serialization-shape normalization, not a semantic change.
func stopSpecToJSON(spec scheduler.StopSpec) (json.RawMessage, error) {
	if spec.IsZero() && strings.TrimSpace(spec.OnLoopEnd) == "" {
		return nil, nil
	}
	canonical := struct {
		LoopEnd   *scheduler.StopSlot `json:"loop_end,omitempty"`
		RunEnd    *scheduler.StopSlot `json:"run_end,omitempty"`
		HardStop  *scheduler.StopSlot `json:"hard_stop,omitempty"`
		OnLoopEnd string              `json:"on_loop_end,omitempty"`
	}{OnLoopEnd: spec.OnLoopEnd}
	if len(spec.LoopEnd.Any) > 0 {
		canonical.LoopEnd = &spec.LoopEnd
	}
	if len(spec.RunEnd.Any) > 0 {
		canonical.RunEnd = &spec.RunEnd
	}
	if len(spec.HardStop.Any) > 0 {
		canonical.HardStop = &spec.HardStop
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}
