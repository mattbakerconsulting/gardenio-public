package main

// Legible gardener rendering (task ka-0000005782 / I2.4, design ka-0000006103 /
// D1.2). A gardener's behavior is governed by opaque JSON blobs — the trigger
// bead's `cron` / `conditions` / `triggers` firing fields plus the GardenerSpec
// `stop` block — which the viewer previously dumped as raw `<pre>` JSON ("look
// terrible", the originating Slack thread). This file DECODES those blobs into
// four at-a-glance human summaries the design calls for:
//
//   - wakes-when — what fires this gardener (cron cadence / world-state gate /
//     event / decision-resolved / manual).
//   - chasing    — what the gardener is driving (its terminal goal + how many
//     crew iterations are live right now, "X crews").
//   - stops-when — the loop_end / run_end / hard_stop conditions decoded into
//     prose (a bead query satisfied, a gate appears, a budget exhausts, ...).
//   - health     — the trajectory verdict, already built by
//     buildGardenerTrajectory; carried here only so the four read as one set.
//
// The decode is best-effort and never errors: a malformed/absent blob yields an
// empty section rather than a stack trace, so the surfaces always render. The
// raw JSON stays reachable behind a collapsed <details> audit escape-hatch on
// the detail/preview surfaces, but it is no longer the primary render.
//
// gardenerLegible is embedded on gardenerView (built in gardenerViewFromBead,
// which holds both the trigger bead and the parsed spec) so all three surfaces
// — /gardeners list rows, the /beads/<id> preview, and the /gardener/<id> detail
// page — share ONE decode authority and never drift.

import (
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
)

// gardenerWake is one thing that fires a gardener, decoded into prose. Kind is a
// small CSS/label hook ("cron" | "gate" | "event" | "decision" | "manual");
// Summary is the human one-liner; Detail is an optional tooltip carrying the
// underlying expression (cron code, parent id, ...) for the operator who wants
// the exact predicate without reading raw JSON.
type gardenerWake struct {
	Kind    string
	Summary string
	Detail  string
}

// gardenerStop is one stop condition, decoded into prose. Slot is the stop slot
// this entry lives in ("loop_end" | "run_end" | "hard_stop"); SlotLabel is its
// human framing ("ends the loop" | "ends the run" | "kills the run"); Summary
// is the decoded condition; Kind is the raw stop kind for a CSS hook.
type gardenerStop struct {
	Slot      string
	SlotLabel string
	Summary   string
	Kind      string
}

// gardenerLegible is the decoded, human-readable rendering of a gardener's
// firing + stop config, replacing the raw JSON dumps. Zero-valued (HasData
// false on each section) when the gardener carries no config to decode — the
// templates then suppress the empty section.
type gardenerLegible struct {
	// Wakes decodes what fires the gardener (the trigger bead's cron / conditions
	// / triggers fields). WakesSummary is the compact one-line form for a list
	// row; Wakes is the full per-source list for the detail/preview cards.
	Wakes        []gardenerWake
	WakesSummary string

	// Stops decodes the GardenerSpec `stop` block. StopsSummary is the compact
	// one-line form for a list row; Stops is the full per-condition list.
	Stops        []gardenerStop
	StopsSummary string
	// OnLoopEnd names the cleanup action when the loop ends ("close" |
	// "notify_slack" | "spawn_followup"), decoded to prose. Empty for the
	// default "close".
	OnLoopEnd string

	// ChasingGoal names what the gardener is driving (terminal goal / owned epic
	// / target bead) when resolvable, for the "chasing" lane. It is STATIC (from
	// the operator-authored spec), so it is safe to compute at header-build
	// time. The LIVE crew count is NOT baked here — it is filled after
	// gardenerViewFromBead (by the runs section) — so the "chasing (X crews)"
	// one-liner is composed at render time by gardenerView.ChasingSummary /
	// gardenerRowView.ChasingSummary, which read the live count off the surface.
	ChasingGoal string
}

// HasWakes / HasStops gate the detail/preview template sections.
func (l gardenerLegible) HasWakes() bool { return len(l.Wakes) > 0 }
func (l gardenerLegible) HasStops() bool { return len(l.Stops) > 0 }

// buildGardenerLegible decodes a gardener's firing + stop config into the legible
// view-model. It reads the firing fields off the trigger bead (b) and the stop
// block off the parsed spec, reusing the header's already-computed cron/next-fire
// projection (v) so the cadence prose and the schedule strip never disagree.
//
// spec may be nil (a bare bead-trigger gardener with no GardenerSpec): the firing
// decode still runs off the bead fields, and the stop section is simply empty.
func buildGardenerLegible(b *beadsapi.BeadDetail, spec *bridge.GardenerSpec, v *gardenerView) gardenerLegible {
	l := gardenerLegible{}
	if b == nil {
		return l
	}

	l.Wakes = decodeGardenerWakes(b, v)
	l.WakesSummary = summarizeWakes(l.Wakes)

	if spec != nil {
		l.Stops = decodeGardenerStops(spec)
		l.StopsSummary = summarizeStops(l.Stops)
		l.OnLoopEnd = decodeOnLoopEnd(spec)
	}

	l.ChasingGoal = chasingGoalLabel(v)
	return l
}

// decodeGardenerWakes decodes every firing source on a gardener bead into prose.
// Precedence mirrors how the gardener actually fires (gardenerTriggerSubKind):
// a clock cron, a world-state conditions gate, a multi-source triggers array,
// each decoded to a human line. Returns nil when nothing fires it (a manual /
// bare gardener), so the template shows a plain "fired manually" fallback.
func decodeGardenerWakes(b *beadsapi.BeadDetail, v *gardenerView) []gardenerWake {
	var wakes []gardenerWake

	// 1. Clock cadence. Reuse the header's cron/next-fire projection so the
	//    cadence prose matches the schedule strip exactly (v.Cron already folds
	//    the legacy cron field, the trigger:multi fallback cron, and the
	//    triggers[] cadence fallback).
	if v.Cron != "" {
		wakes = append(wakes, gardenerWake{
			Kind:    "cron",
			Summary: cronWakeSummary(v),
			Detail:  cronWakeDetail(v),
		})
	}

	// 2. World-state pre-flight gate: the bare `conditions` array `gb gardener
	//    start` writes. Each leaf is a daemon-side predicate checked at fire time.
	if raw := strings.TrimSpace(b.Fields["conditions"]); raw != "" {
		if exprs, err := conditions.ParseExpressions(raw); err == nil {
			for _, e := range exprs {
				if s := describeExpression(e); s != "" {
					wakes = append(wakes, gardenerWake{
						Kind:    "gate",
						Summary: "only when " + s,
						Detail:  "pre-flight gate (conditions[]) — evaluated at each cron tick",
					})
				}
			}
		}
	}

	// 3. Multi-source `triggers` array: cron / bead_event / decision_resolved /
	//    manual entries. The clock cron inside is already surfaced above via
	//    v.Cron, so skip cron entries here to avoid a duplicate cadence line.
	def := bridge.ParseTriggerDef(b, "schedule")
	for _, t := range def.Triggers {
		if w, ok := decodeTriggerEntry(t); ok {
			wakes = append(wakes, w)
		}
	}

	return wakes
}

// decodeTriggerEntry decodes one triggers[] entry into a wake line. Returns
// ok=false for a cron entry (its cadence is surfaced via the header projection)
// and for an entry we cannot describe, so the caller drops it.
func decodeTriggerEntry(t scheduler.TriggerEntry) (gardenerWake, bool) {
	switch t.Kind {
	case scheduler.TriggerKindBeadEvent:
		summary := "on bead activity"
		if evs := strings.Join(t.Events, ", "); evs != "" {
			summary = "when a bead is " + evs
		}
		if t.Match != nil {
			if m := describeExpression(*t.Match); m != "" {
				summary += " (" + m + ")"
			}
		}
		return gardenerWake{Kind: "event", Summary: summary, Detail: triggerEntryDetail(t)}, true
	case scheduler.TriggerKindDecisionResolved:
		summary := "when an operator resolves a decision"
		if t.Match != nil {
			if m := describeExpression(*t.Match); m != "" {
				summary += " (" + m + ")"
			}
		}
		return gardenerWake{Kind: "decision", Summary: summary, Detail: triggerEntryDetail(t)}, true
	case scheduler.TriggerKindManual:
		return gardenerWake{Kind: "manual", Summary: "fired manually", Detail: ""}, true
	default:
		return gardenerWake{}, false
	}
}

// triggerEntryDetail composes a tooltip for a triggers[] entry, naming any
// preflight predicate + throttle/debounce so the operator sees the guard rails
// without reading raw JSON. Empty when the entry carries no extra detail.
func triggerEntryDetail(t scheduler.TriggerEntry) string {
	var parts []string
	if t.Preflight != nil {
		if p := describeExpression(*t.Preflight); p != "" {
			parts = append(parts, "gated on "+p)
		}
	}
	if t.Throttle != "" {
		parts = append(parts, "throttled to one fire per "+t.Throttle)
	}
	if t.Debounce != "" {
		parts = append(parts, "debounced "+t.Debounce)
	}
	return strings.Join(parts, "; ")
}

// cronWakeSummary phrases the clock cadence: leads with the human next-fire
// relative when the gardener is live, degrades to a plain "on a schedule" for a
// closed/disabled gardener (which will not fire) or an unparseable cron.
func cronWakeSummary(v *gardenerView) string {
	switch {
	case v.NextFireRelative != "":
		return "on a schedule — fires " + v.NextFireRelative
	case v.NextFireError != "":
		return "on a schedule (cron error)"
	case v.IsClosed:
		return "on a schedule (not firing — closed)"
	case !v.Enabled:
		return "on a schedule (not firing — paused)"
	default:
		return "on a schedule"
	}
}

// cronWakeDetail composes the cron tooltip: the raw expression + timezone +
// absolute next fire, so the exact cadence is one hover away without a JSON dump.
func cronWakeDetail(v *gardenerView) string {
	d := "cron: " + v.Cron
	if v.Timezone != "" {
		d += " (" + v.Timezone + ")"
	}
	if v.NextFire != "" {
		d += " · next fire " + v.NextFire
	}
	if v.NextFireError != "" {
		d += " · parse error: " + v.NextFireError
	}
	return d
}

// summarizeWakes collapses the wake list into one compact line for a list row.
// Empty list ⇒ "manually" (a gardener with no decodable firing source is fired
// by hand / run-now). Multiple sources join with " · " and the whole is capped
// so the row stays one scan-line.
func summarizeWakes(wakes []gardenerWake) string {
	if len(wakes) == 0 {
		return "fired manually"
	}
	parts := make([]string, 0, len(wakes))
	for _, w := range wakes {
		parts = append(parts, w.Summary)
	}
	return clampLine(strings.Join(parts, " · "), 90)
}

// decodeGardenerStops decodes the GardenerSpec `stop` block into prose, one entry
// per stop condition across the three slots. Precedence order (hard_stop >
// loop_end > run_end) is preserved so the most-severe brakes read first.
// Returns nil when the spec carries no stop block or it fails to parse.
func decodeGardenerStops(spec *bridge.GardenerSpec) []gardenerStop {
	raw := strings.TrimSpace(string(spec.Stop))
	if raw == "" {
		return nil
	}
	parsed, err := scheduler.ParseStopSpec(raw)
	if err != nil || parsed.IsZero() {
		return nil
	}
	var stops []gardenerStop
	appendSlot := func(slot, label string, entries []scheduler.StopEntry) {
		for _, e := range entries {
			stops = append(stops, gardenerStop{
				Slot:      slot,
				SlotLabel: label,
				Summary:   describeStopEntry(e),
				Kind:      e.Kind,
			})
		}
	}
	appendSlot(scheduler.StopSlotHardStop, "kills the run", parsed.HardStop.Any)
	appendSlot(scheduler.StopSlotLoopEnd, "ends the loop", parsed.LoopEnd.Any)
	appendSlot(scheduler.StopSlotRunEnd, "ends the run", parsed.RunEnd.Any)
	return stops
}

// summarizeStops collapses the stop list into one compact line for a list row,
// leading with the loop-ending (goal/terminal) condition an operator most cares
// about. Empty ⇒ "" so the row shows a muted placeholder.
func summarizeStops(stops []gardenerStop) string {
	if len(stops) == 0 {
		return ""
	}
	// Prefer a loop_end condition for the one-liner (that is the gardener's
	// "done" signal); fall back to whatever is first.
	lead := stops[0].Summary
	for _, s := range stops {
		if s.Slot == scheduler.StopSlotLoopEnd {
			lead = s.Summary
			break
		}
	}
	if len(stops) > 1 {
		return clampLine(fmt.Sprintf("%s (+%d more)", lead, len(stops)-1), 90)
	}
	return clampLine(lead, 90)
}

// decodeOnLoopEnd renders the on_loop_end cleanup action as prose, or "" for
// the default "close".
func decodeOnLoopEnd(spec *bridge.GardenerSpec) string {
	raw := strings.TrimSpace(string(spec.Stop))
	if raw == "" {
		return ""
	}
	parsed, err := scheduler.ParseStopSpec(raw)
	if err != nil {
		return ""
	}
	switch parsed.OnLoopEnd {
	case "notify_slack":
		return "notify Slack on loop end"
	case "spawn_followup":
		return "spawn a follow-up on loop end"
	default:
		return ""
	}
}

// describeStopEntry decodes one StopEntry into a human clause. Every supported
// stop kind (scheduler.StopKind*) is covered; an unknown kind renders verbatim
// so a future engine kind stays legible rather than silently dropped.
func describeStopEntry(e scheduler.StopEntry) string {
	switch e.Kind {
	case scheduler.StopKindMaxRuns:
		if e.Value != "" {
			return "after " + e.Value + " runs"
		}
		return "the run budget is exhausted"
	case scheduler.StopKindWallClock:
		if e.Duration != "" {
			d := "after " + e.Duration + " of wall-clock time"
			if e.When == "yielded" {
				d += " spent yielding"
			}
			return d
		}
		return "the wall-clock budget elapses"
	case scheduler.StopKindBeadQuery:
		if s := describeBeadQuery(e.Op, e.Value, e.Parent, e.Project, e.Types, e.Statuses, e.Labels); s != "" {
			return "when " + s
		}
		return "when a bead query is satisfied"
	case scheduler.StopKindSpecificBeadClosed:
		if e.BeadID != "" {
			return "when " + e.BeadID + " closes"
		}
		return "when the target bead closes"
	case scheduler.StopKindGatePresent:
		if e.Value != "" {
			return "when the gate " + quote(e.Value) + " appears"
		}
		return "when a gate appears"
	case scheduler.StopKindGateAbsent:
		if e.Value != "" {
			return "when the gate " + quote(e.Value) + " clears"
		}
		return "when a gate clears"
	case scheduler.StopKindNoProgressFor:
		if e.Runs > 0 {
			return fmt.Sprintf("after %s with no progress", runsPhrase(e.Runs))
		}
		return "after a stretch with no progress"
	case scheduler.StopKindConsecutiveFailures:
		if e.Value != "" {
			return "after " + e.Value + " consecutive failures"
		}
		return "after too many consecutive failures"
	case scheduler.StopKindManual:
		if e.ReasonField != "" {
			return "when the field " + quote(e.ReasonField) + " is set"
		}
		return "when stopped by hand"
	case scheduler.StopKindManualForce:
		if e.Label != "" {
			return "when the label " + quote(e.Label) + " is applied"
		}
		return "when force-stopped by hand"
	case scheduler.StopKindMRMerged:
		if e.Parent != "" {
			return "when every tracked MR under " + e.Parent + " is merged"
		}
		return "when every tracked MR merges"
	case scheduler.StopKindGoalSatisfied:
		return "when the gardener declares its terminal goal met"
	case scheduler.StopKindYieldedThenWokenThenIdle:
		return "when a woken run goes idle"
	case scheduler.StopKindNoActionableState:
		return "when nothing is left to work"
	case "":
		return "an unspecified condition"
	default:
		return e.Kind
	}
}

// describeExpression decodes a condition Expression into prose, recursing
// through all/any/not compositions and describing each leaf. Empty for a no-op
// leaf (no constraint). This is the shared humanizer for wake gates,
// triggers[] preflight/match predicates, and (via describeBeadQuery) stop
// bead-query entries — one decode authority for the whole condition DSL.
func describeExpression(e conditions.Expression) string {
	if e.IsComposition() {
		if len(e.All) > 0 {
			return joinClauses(e.All, " and ")
		}
		if len(e.Any) > 0 {
			return joinClauses(e.Any, " or ")
		}
		if e.Not != nil {
			inner := describeExpression(*e.Not)
			if inner == "" {
				return ""
			}
			return "not (" + inner + ")"
		}
		return ""
	}
	return describeLeaf(e.Leaf)
}

// joinClauses describes each sub-expression and joins the non-empty ones with
// sep, parenthesizing when more than one survives so nested composition stays
// unambiguous.
func joinClauses(exprs []conditions.Expression, sep string) string {
	parts := make([]string, 0, len(exprs))
	for _, e := range exprs {
		if s := describeExpression(e); s != "" {
			parts = append(parts, s)
		}
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	default:
		return strings.Join(parts, sep)
	}
}

// describeLeaf decodes one condition leaf (daemon-side or field) into prose.
func describeLeaf(l conditions.Leaf) string {
	switch l.Type {
	case "bead_query":
		return describeBeadQuery(l.Op, l.Value, l.Parent, l.Project, l.Types, l.Statuses, l.Labels)
	case "gate":
		switch l.Op {
		case "absent":
			return "the gate " + quote(l.Value) + " is absent"
		default: // "present" or empty
			return "the gate " + quote(l.Value) + " is present"
		}
	case "ready_queue":
		return countPhrase(l.Op, l.Value) + " ready in the queue"
	case "active_agents":
		who := "active agents"
		if l.Role != "" {
			who = quote(l.Role) + " agents are active"
			return countPhrase(l.Op, l.Value) + " " + who
		}
		return countPhrase(l.Op, l.Value) + " " + who
	case "script":
		if l.ScriptBead != "" {
			return "the probe " + l.ScriptBead + " passes"
		}
		return "a probe passes"
	case "":
		// Field leaf (in-process) or a no-op leaf.
		if l.Field != "" {
			return describeFieldLeaf(l)
		}
		return ""
	default:
		return ""
	}
}

// describeFieldLeaf decodes an in-process field leaf (field/op/value) into
// prose. These match against an event/bead payload.
func describeFieldLeaf(l conditions.Leaf) string {
	field := quote(l.Field)
	switch l.Op {
	case "present":
		return field + " is set"
	case "absent":
		return field + " is unset"
	case "==", "eq":
		return field + " is " + quote(l.Value)
	case "!=", "ne":
		return field + " is not " + quote(l.Value)
	case "in":
		return field + " is one of " + quote(strings.Join(l.Values, ", "))
	case "not_in":
		return field + " is none of " + quote(strings.Join(l.Values, ", "))
	case "contains":
		return field + " contains " + quote(l.Value)
	case "in_subtree_of":
		return field + " is under " + l.Value
	default:
		if l.Value != "" {
			return field + " " + l.Op + " " + quote(l.Value)
		}
		return field
	}
}

// describeBeadQuery decodes a bead_query predicate — the workhorse leaf/stop
// kind — into prose: a count phrase ("no", "any", "at least 3") over a filter
// phrase ("open task beads under ka-X in gardenio labelled foo").
func describeBeadQuery(op, value, parent, project string, types, statuses, labels []string) string {
	count := countPhrase(op, value)

	var filt []string
	if len(statuses) > 0 {
		filt = append(filt, strings.Join(statuses, "/"))
	}
	if len(types) > 0 {
		filt = append(filt, strings.Join(types, "/"))
	}
	noun := "beads"
	if len(filt) > 0 {
		noun = strings.Join(filt, " ") + " beads"
	}

	clause := count + " " + noun
	if parent != "" {
		clause += " under " + parent
	}
	if project != "" {
		clause += " in " + project
	}
	if len(labels) > 0 {
		clause += " labelled " + strings.Join(labels, ", ")
	}
	return clause
}

// countPhrase turns a comparison (op, value) over a bead count into an English
// quantifier. "==0" ⇒ "no", ">0"/">=1" ⇒ "any", ">=N" ⇒ "at least N",
// "<N" ⇒ "fewer than N", etc. Falls back to a literal "op value" phrasing for
// an operator it does not special-case.
func countPhrase(op, value string) string {
	v := strings.TrimSpace(value)
	switch op {
	case "==", "eq":
		if v == "0" {
			return "no"
		}
		return "exactly " + v
	case "!=", "ne":
		if v == "0" {
			return "any"
		}
		return "not " + v
	case ">":
		if v == "0" {
			return "any"
		}
		return "more than " + v
	case ">=":
		if v == "1" {
			return "any"
		}
		return "at least " + v
	case "<":
		return "fewer than " + v
	case "<=":
		return "at most " + v
	default:
		if v != "" {
			return op + " " + v
		}
		return "some"
	}
}

// ChasingSummary composes the detail/preview "chasing (X crews)" one-liner from
// the LIVE crew count (filled after the header build by the runs section) and
// the static driven goal. Rendered at template time so the live count is
// current. See summarizeChasing for the phrasing.
func (v gardenerView) ChasingSummary() string {
	return summarizeChasing(v.LiveCount, v.Legible.ChasingGoal, v.IsClosed)
}

// ChasingSummary composes the list-row "chasing (X crews)" one-liner. A row
// carries the live count on IsRunning + its own goal; gardenerRowView does not
// hold LiveCount, so the row uses its running flag as the live-crew proxy (a
// gardener with an in-flight last run has one live crew). See summarizeChasing.
func (r gardenerRowView) ChasingSummary() string {
	live := 0
	if r.IsRunning {
		live = 1
	}
	goal := ""
	if g := strings.TrimSpace(r.TerminalGoal); g != "" {
		goal = clampLine(g, 60)
	} else if p := strings.TrimSpace(r.Prompt); p != "" {
		// Prompt fallback (bug ka-0000003596): a prompt-only gardener has no
		// terminal goal, so lead the "chasing" line with a clamped prompt one-liner
		// instead of a bare "idle" with no hint of what it drives.
		goal = clampLine(p, 60)
	}
	return summarizeChasing(live, goal, r.IsClosed)
}

// PromptOneLiner is the clamped, single-line rendering of the gardener's per-run
// prompt for the index row's prompt-only fallback (bug ka-0000003596). The full
// text lives in the row's title tooltip; this keeps the scan-band tight.
func (r gardenerRowView) PromptOneLiner() string {
	return clampLine(strings.TrimSpace(r.Prompt), 100)
}

// chasingGoalLabel names what the gardener is driving, for the "chasing" lane:
// its terminal goal (clamped), else its live goal-target epic, else a
// prompt-derived one-liner, else "". The terminal goal is the operator-authored
// intent, so it leads; the prompt fallback (bug ka-0000003596) keeps a
// prompt-only gardener — one authored the sanctioned way, with a prompt and no
// goal — from reading "idle" with no hint of what it does.
func chasingGoalLabel(v *gardenerView) string {
	if g := strings.TrimSpace(v.TerminalGoal); g != "" {
		return clampLine(g, 60)
	}
	if v.TargetEpicID != "" {
		return "drives " + v.TargetEpicID
	}
	if p := strings.TrimSpace(v.Prompt); p != "" {
		return clampLine(p, 60)
	}
	return ""
}

// summarizeChasing composes the "chasing (X crews)" one-liner: how many crew
// iterations are live now, paired with the driven target. A closed gardener
// reads "done"; an idle open gardener reads "idle" so the operator can tell a
// quiet-but-live gardener from a running one at a glance.
func summarizeChasing(liveCrews int, goal string, isClosed bool) string {
	var lead string
	switch {
	case isClosed:
		lead = "done"
	case liveCrews == 1:
		lead = "1 crew live"
	case liveCrews > 1:
		lead = fmt.Sprintf("%d crews live", liveCrews)
	default:
		lead = "idle"
	}
	if goal != "" {
		return clampLine(lead+" · "+goal, 90)
	}
	return lead
}

// quote wraps a value in typographic quotes for prose, or returns a bare
// placeholder when empty so a clause never reads "the gate ” is present".
func quote(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(unnamed)"
	}
	return "“" + s + "”"
}

// clampLine truncates a one-line summary to n runes with an ellipsis, so a list
// row stays a single scan-line. The full text stays on the tooltip / detail
// card.
func clampLine(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
