package main

// wrapup.go — Structured wrap-up message schema for agent despawn.
//
// When agents call gb stop (or gb done), they provide a wrap-up message
// summarizing their session. The wrap-up is stored as a JSON-serialized
// string in the agent bead's "wrapup" field, making it queryable and
// parseable by other tools (bridges, dashboards, successor agents).
//
// ## Storage approach
//
// The wrap-up is stored as a single JSON string in bead fields["wrapup"].
// This was chosen over alternatives because:
//   - Comments are immutable append-only text — not structured or queryable
//   - Child data beads add complexity and an extra entity to track
//   - A single JSON field is atomic, readable via kd show, and extensible
//
// The existing --reason flag is preserved for backward compatibility: if
// --reason is provided without --wrapup, behavior is unchanged (a plain
// comment is added). When --wrapup is provided, the structured wrap-up
// is stored AND a human-readable summary comment is also added.
//
// ## Extensibility via advice
//
// The WrapUpRequirements struct defines which fields are required vs
// optional, and can specify custom fields. Advice beads with category
// "wrapup" can override these defaults per role/project/agent scope. The
// requirements are injected into agent context via gb prime so agents
// know what's expected before they reach gb stop.
//
// ## Field reference (stored in bead fields["wrapup"])
//
//   tldr             — One-line outcome summary for the top of a Slack card (optional, nudged)
//   accomplishments  — What the agent did this session (required by default)
//   blockers         — What's preventing further progress (optional)
//   handoff_notes    — Context for the next agent picking up this work (optional)
//   beads_closed     — List of bead IDs closed during this session (optional, agent-supplied)
//   pull_requests    — List of PR URLs created during this session (optional)
//   custom           — Arbitrary key-value pairs for advice-driven extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"gardenio/controller/internal/advice"
)

// WrapUp is the structured wrap-up message stored on agent beads at stop time.
// It is serialized to JSON and stored in bead fields["wrapup"].
type WrapUp struct {
	// TLDR is a single-line outcome summary for the top of a Slack agent
	// card — the most useful one glance at the result. Optional but
	// strongly nudged. Validated server-side at gb stop time: collapsed to
	// a single line and rejected if it exceeds the hard cap (see
	// normalizeAndValidateTLDR). Stored RAW — escaping is a render-time
	// concern owned by each destination (ka-0000005806).
	TLDR string `json:"tldr,omitempty"`

	// Accomplishments summarizes what the agent did this session.
	// Required by default (enforced by WrapUpRequirements).
	Accomplishments string `json:"accomplishments"`

	// Blockers lists anything preventing further progress on claimed work.
	// Optional by default.
	Blockers string `json:"blockers,omitempty"`

	// HandoffNotes provides context for the next agent or human picking
	// up this work — what state things are in, what to do next.
	// Optional by default.
	HandoffNotes string `json:"handoff_notes,omitempty"`

	// BeadsClosed is an OPTIONAL, agent-supplied list of bead IDs the agent
	// closed during this session. It was historically documented as
	// "auto-populated by gb stop," but no code path ever derived it from the
	// session's close activity (audit finding F3, gardenio/docs/wrapup-
	// specification.md: 0/23 recent gardener wrap-ups carried it). The doc
	// contract was corrected to "optional, agent-supplied" (ka-0000005070):
	// gb stop stores and round-trips whatever the agent passes in --wrapup,
	// but never fabricates it. Scoring is unaffected — D2 throughput is
	// derived from the bead graph, not this field.
	BeadsClosed []string `json:"beads_closed,omitempty"`

	// PullRequests lists URLs of PRs created during this session.
	// Optional — agents can include this for visibility.
	PullRequests []string `json:"pull_requests,omitempty"`

	// HarnessGaps is an OPTIONAL, agent-supplied sensor reading of what the
	// harness itself lacked this run — a tool that was missing, a guard that
	// fired wrongly, a piece of context the agent had to reconstruct by hand, a
	// capability it wished for. It follows the MLD sensor pattern (Mistakes /
	// Learnings / Desires; ref lopopolo/harness-engineering) and is captured
	// here so friction with the platform surfaces at the moment it is felt,
	// instead of evaporating at despawn.
	//
	// STRICTLY TELEMETRY, NOT POLICY (plan ka-0000001183 §W6). A harness_gaps
	// note NEVER auto-becomes advice, a graduation candidate, or any enforced
	// rule. It is raw self-report — which is systematically unreliable (an agent
	// blames the harness for its own mistake, or asks for a capability it
	// already has). Promotion to anything actionable requires a downstream
	// groomer / CED pass that CORROBORATES the note against the session
	// transcript first, so self-report can never become self-fulfilling. This
	// field is only the durable capture point for that later, human-gated pass.
	//
	// Free text, no schema — agents write it however the friction reads. Empty
	// (the common case) == nothing to report. Optional and never enforced.
	HarnessGaps string `json:"harness_gaps,omitempty"`

	// Custom holds arbitrary key-value pairs for advice-driven extensions.
	// Advice beads can declare custom required/optional fields that agents
	// must populate. Keys are field names, values are their content.
	Custom map[string]string `json:"custom,omitempty"`

	// ProgressScore captures the gardener's self-assessment of how much
	// material progress this run delivered: 0 = nothing material, 1 =
	// incremental, ≥2 = substantive. Required when the schedule bead's
	// progress.surface.in_wrapup is true. See plan §T5.
	ProgressScore *int `json:"progress_score,omitempty"`

	// ProgressSummary is a one-line free-text justification of ProgressScore.
	// Required when the schedule bead's progress.surface.in_wrapup is true.
	ProgressSummary string `json:"progress_summary,omitempty"`

	// GoalSatisfied is the agent's y/n self-assessment of whether the
	// agent-bead's terminal_goal was met this run. Required (in Phase 2+)
	// when the agent bead has terminal_goal set. Values: "yes" | "no".
	// Empty = not answered. See gardenio/docs/design/terminal-goals.md.
	GoalSatisfied string `json:"goal_satisfied,omitempty"`

	// GoalReport is a one-to-three-line free-text justification of
	// GoalSatisfied. Required (in Phase 2+) when GoalSatisfied is set.
	GoalReport string `json:"goal_report,omitempty"`

	// RespawnAfter is the duration the agent requests before being
	// respawned when GoalSatisfied=="no". Parsed as a Go duration ("10m",
	// "1h"); the literal string "immediate" is shorthand for 0. Ignored
	// when goal is satisfied. Optional; defaults to controller policy.
	RespawnAfter string `json:"respawn_after,omitempty"`

	// RestartWhen is the agent's own free-text declaration of the condition
	// under which it expects to resume — the "wake-trigger" arm of the
	// restart-condition card (epic ka-0000000754, "think how cool it would be
	// if the agent printed its restart condition"). Examples: "when ka-X
	// merges", "when the operator resolves decision ka-Y". The consumer/bridge
	// side (bot_agents_wrapup.go wakeTriggerRestartLine) already renders this
	// verbatim as the "Wakes when …" line on the Slack agent card and counts it
	// as a progressing signal; this producer field + the wrap-up prompt nudge
	// are what let an agent actually declare it. Kept as-declared (free text) so
	// a bead id inside it survives to the reader. Optional; empty == no declared
	// wake condition (a plain terminal stop).
	RestartWhen string `json:"restart_when,omitempty"`

	// Successor is the persistent-agents succession intent (design memo
	// §2.5, task ka-0000002751). One of {none, same, new}; empty == none.
	// Validated at the gb stop boundary (ValidateSuccessor rejects any
	// other value). Semantics:
	//   - none (default): terminal. The card flips to terminal state.
	//   - same: recreate this name after wake; reuse card + coop --resume.
	//     Phase 1 records intent only (no-op until the Phase 2 wake scanner,
	//     task ka-0000002976) — renders like a terminal card today.
	//   - new: mint a fresh name + fresh card + fresh conversation, stamp
	//     prior_agent_id = old bead, close the predecessor with
	//     close_reason "superseded: continued as <id>".
	// The consumer/bridge side (bot_agents_wrapup.go) already reads this
	// enum; empty/none is fully inert. See gb stop --successor.
	Successor string `json:"successor,omitempty"`

	// GoalUnachievable is the crash-loop / goal-unachievable circuit breaker
	// (P3, task ka-0000005843). It is a DISTINCT signal from goal_satisfied:"no":
	//   - goal_satisfied:"no" means "not done THIS run — respawn me and try again"
	//     (a legitimate continuation, budget-capped by max_respawns).
	//   - goal_unachievable:true means "this goal is DETERMINISTICALLY impossible —
	//     retrying will fail identically, so do NOT burn any more pods on it."
	// When true it hard-overrides succession: whatever successor was requested
	// (same OR new) is coerced to none, so neither the successor=new mint
	// (stop.go) nor the successor=same wake-scanner revive (statusreporter/
	// wake_scanner.go) fires. The close also stamps goal_unachievable=true +
	// close_reason=goal_unachievable and clears any pre-existing wake metadata
	// (successor / next_respawn_at) the bead carried from an earlier park, so a
	// deterministically-failing identity converges to terminal instead of
	// crash-looping. Optional; false/absent == today's behavior.
	GoalUnachievable bool `json:"goal_unachievable,omitempty"`

	// Timestamp is set automatically when the wrap-up is created.
	Timestamp time.Time `json:"timestamp"`
}

// Successor enum values for WrapUp.Successor (design memo §2.5,
// task ka-0000002751). These mirror the consumer-side constants in
// internal/bridge/bot_agents_wrapup.go; empty is treated as SuccessorNone
// everywhere. Kept in sync as a load-bearing producer/consumer contract.
const (
	// SuccessorNone is the terminal default: this run ends, the card flips
	// to terminal state, and no successor is minted.
	SuccessorNone = "none"

	// SuccessorSame records intent to recreate this same agent name after a
	// wake. Phase 1 records intent only — a no-op until the wake scanner
	// (task ka-0000002976) lands.
	SuccessorSame = "same"

	// SuccessorNew mints a fresh successor agent (new name + new card) that
	// carries this agent's work forward; the predecessor is retired.
	SuccessorNew = "new"
)

// ValidSuccessorValues lists the accepted WrapUp.Successor enum values, in
// canonical order, for error messages and the wrap-up expectations output.
var ValidSuccessorValues = []string{SuccessorNone, SuccessorSame, SuccessorNew}

// NormalizeSuccessor folds a raw successor value to its canonical lowercase
// enum form, treating empty/whitespace as SuccessorNone. It does NOT reject
// unknown values — use ValidateSuccessor for that. Case and surrounding
// whitespace are ignored so "New" and " new " both resolve to "new".
func NormalizeSuccessor(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return SuccessorNone
	}
	return s
}

// ValidateSuccessor checks that a raw successor value is one of the accepted
// enum values (case/whitespace-insensitive) and returns the canonical form.
// Empty/whitespace normalizes to SuccessorNone with no error. Any other value
// is rejected with an error naming the accepted set — this is the "reject
// unknown at parse" contract from the task (design memo §2.5, §3.2).
func ValidateSuccessor(raw string) (string, error) {
	s := NormalizeSuccessor(raw)
	switch s {
	case SuccessorNone, SuccessorSame, SuccessorNew:
		return s, nil
	default:
		return "", fmt.Errorf("invalid successor %q — must be one of %s",
			strings.TrimSpace(raw), strings.Join(ValidSuccessorValues, ", "))
	}
}

// reconcileSuccessorGoal applies the design-memo §2.5 contradiction rule to a
// canonical (already-validated) successor value: goal_satisfied=="no" paired
// with successor=="none" is "giving up while claiming progress" — the run
// declares it did not meet its goal yet asks for no successor, so the ⏳
// progressing glyph would falsely promise "respawn coming." We coerce to none
// (leave it terminal) and return a warning so the agent sees why. Every other
// combination passes through unchanged, including a non-none successor with
// goal_satisfied=="no" (a legitimate continuation) and any goal value other
// than "no". goalSatisfied is compared case/whitespace-insensitively.
func reconcileSuccessorGoal(successor, goalSatisfied string) (string, string) {
	if successor == SuccessorNone &&
		strings.EqualFold(strings.TrimSpace(goalSatisfied), "no") {
		return SuccessorNone, "goal_satisfied=\"no\" with successor=\"none\" — no respawn will " +
			"happen; treating as a terminal stop. Set successor=same or successor=new " +
			"(or --respawn-after) if you want this identity to continue."
	}
	return successor, ""
}

// reconcileGoalUnachievable applies the goal-unachievable circuit breaker (P3,
// task ka-0000005843) to a canonical (already-validated) successor value. When
// the agent declares its goal DETERMINISTICALLY unachievable, any requested
// succession — same OR new — is coerced to none so no further pod is burned on a
// task that will fail identically on every retry. This is the distinct-signal
// half of the feature: goal_satisfied:"no" asks to retry, goal_unachievable:true
// forbids it. A non-none successor that gets coerced returns a warning naming why;
// an already-none successor (or goalUnachievable=false) passes through silently.
func reconcileGoalUnachievable(successor string, goalUnachievable bool) (string, string) {
	if !goalUnachievable {
		return successor, ""
	}
	if successor != SuccessorNone {
		return SuccessorNone, fmt.Sprintf("goal_unachievable=true overrides successor=%q — a "+
			"deterministically-unachievable goal is terminal; no successor will be minted or woken.", successor)
	}
	return SuccessorNone, ""
}

// Agent-bead field keys for the terminal-goals feature. These live in the
// bead `fields` map (no schema migration). Set by gb start --goal /
// gb spawn --goal in Phase 4 and read by gb prime, gb stop, and the
// reconciler in Phases 2-3. See gardenio/docs/design/terminal-goals.md.
const (
	// AgentBeadFieldTerminalGoal holds the plain-text goal statement
	// attached to an agent bead at spawn time.
	AgentBeadFieldTerminalGoal = "terminal_goal"

	// AgentBeadFieldTerminalGoalSetAt holds the RFC3339 timestamp at which
	// terminal_goal was attached to the bead.
	AgentBeadFieldTerminalGoalSetAt = "terminal_goal_set_at"

	// AgentBeadFieldRespawnCount holds the number of respawns triggered by
	// unsatisfied goals on this identity (string-coded int).
	AgentBeadFieldRespawnCount = "respawn_count"

	// AgentBeadFieldMaxRespawns holds the per-identity override for the
	// respawn budget cap (string-coded int). Default cap is 5.
	AgentBeadFieldMaxRespawns = "max_respawns"

	// AgentBeadFieldNextRespawnAt holds the RFC3339 timestamp at which the
	// controller should next spawn this identity. Cleared by the agent
	// entrypoint after the respawn fires.
	AgentBeadFieldNextRespawnAt = "next_respawn_at"

	// AgentBeadFieldGoalUnachievable is stamped "true" on the closed agent bead
	// when the run declared its goal deterministically unachievable (P3,
	// ka-0000005843). It is the durable marker the wake-scanner defensively reads
	// to refuse a revive, and it lets beads-viewer surface a deterministic
	// give-up distinctly from a normal terminal stop.
	AgentBeadFieldGoalUnachievable = "goal_unachievable"
)

// CloseReasonGoalUnachievable is the close_reason stamped on an agent bead that
// stops with goal_unachievable=true. Distinct from idle_ttl_expired / a normal
// done close so the audit trail names why the identity will never be respawned.
const CloseReasonGoalUnachievable = "goal_unachievable"

// WrapUpRequirements defines what a wrap-up must contain. Loaded from
// advice beads with category "wrapup" and merged by scope precedence
// (global < project < role < agent). See prime_advice.go for injection.
type WrapUpRequirements struct {
	// Required lists field names that must be non-empty in the wrap-up.
	// Default: ["accomplishments"]
	Required []string `json:"required"`

	// Optional lists field names that are encouraged but not enforced.
	// Default: ["blockers", "handoff_notes", "restart_when", "pull_requests"]
	Optional []string `json:"optional"`

	// CustomFields defines additional fields beyond the built-in set.
	// Each entry specifies a field name and whether it's required.
	CustomFields []CustomFieldDef `json:"custom_fields,omitempty"`

	// Enforce controls whether gb stop blocks on incomplete wrap-ups.
	// "hard" = error if required fields missing (default for production)
	// "soft" = warn but allow stop
	// "none" = no validation
	Enforce string `json:"enforce"`
}

// CustomFieldDef describes an advice-driven custom field for wrap-ups.
type CustomFieldDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required"`
}

// DefaultWrapUpRequirements returns the baseline requirements used when
// no advice bead overrides are present.
//
// tldr is hard-required alongside accomplishments: the validated one-line
// summary is what renders as the featured headline on the Slack agent card
// (buildWrapUpAgentCardBlocks). Without it the card has no outcome line and the
// operator has to read the accomplishments prose to learn what happened. Making
// it required with enforce=hard is what guarantees every wrap-up surfaces a
// "beautiful tldr" in Slack rather than agents skipping a merely-nudged field.
// Agents that genuinely cannot summarize can still `gb stop --force`, and a
// wrapup-config bead can relax this per role/project.
func DefaultWrapUpRequirements() WrapUpRequirements {
	return WrapUpRequirements{
		Required: []string{"tldr", "accomplishments"},
		Optional: []string{"blockers", "handoff_notes", "restart_when", "pull_requests", "harness_gaps"},
		Enforce:  "hard",
	}
}

// Validate checks a WrapUp against the given requirements. Returns a list
// of validation errors (empty if valid). Does not return an error for
// enforce="none". A nil wrap-up (the bare `gb stop` case) is treated as
// "all required fields empty" so the same enforcement path covers both a
// missing wrap-up and an incomplete one.
func (r WrapUpRequirements) Validate(w *WrapUp) []string {
	if r.Enforce == "none" {
		return nil
	}

	var issues []string
	for _, field := range r.Required {
		if !wrapUpFieldPresent(w, field) {
			issues = append(issues, fmt.Sprintf("required field %q is empty", field))
		}
	}
	for _, cf := range r.CustomFields {
		if cf.Required && (w == nil || w.Custom == nil || w.Custom[cf.Name] == "") {
			issues = append(issues, fmt.Sprintf("required custom field %q is empty", cf.Name))
		}
	}
	return issues
}

// wrapUpFieldPresent checks whether a named field has a non-empty value.
// A nil wrap-up reports every field as absent so callers can validate a
// bare `gb stop` (no --wrapup) without a separate code path.
func wrapUpFieldPresent(w *WrapUp, field string) bool {
	if w == nil {
		return false
	}
	switch field {
	case "tldr":
		return w.TLDR != ""
	case "accomplishments":
		return w.Accomplishments != ""
	case "blockers":
		return w.Blockers != ""
	case "handoff_notes":
		return w.HandoffNotes != ""
	case "restart_when":
		return w.RestartWhen != ""
	case "harness_gaps":
		return w.HarnessGaps != ""
	case "beads_closed":
		return len(w.BeadsClosed) > 0
	case "pull_requests":
		return len(w.PullRequests) > 0
	case "progress_score":
		return w.ProgressScore != nil
	case "progress_summary":
		return w.ProgressSummary != ""
	case "successor":
		// Present only when it carries real intent — "none"/empty is inert.
		return w.Successor != "" && w.Successor != SuccessorNone
	default:
		// Check custom fields.
		return w.Custom != nil && w.Custom[field] != ""
	}
}

// MarshalWrapUp serializes a WrapUp to a JSON string suitable for storage
// in bead fields["wrapup"].
func MarshalWrapUp(w *WrapUp) (string, error) {
	if w.Timestamp.IsZero() {
		w.Timestamp = time.Now().UTC()
	}
	data, err := json.Marshal(w)
	if err != nil {
		return "", fmt.Errorf("marshalling wrap-up: %w", err)
	}
	return string(data), nil
}

// WrapUpToComment renders a human-readable summary of the wrap-up for
// storage as a bead comment (in addition to the structured field).
func WrapUpToComment(w *WrapUp) string {
	var b []byte
	b = append(b, "gb stop wrap-up:\n"...)

	if w.TLDR != "" {
		b = append(b, "\nTLDR: "...)
		b = append(b, w.TLDR...)
	}

	b = append(b, "\nAccomplishments: "...)
	b = append(b, w.Accomplishments...)

	if w.Blockers != "" {
		b = append(b, "\nBlockers: "...)
		b = append(b, w.Blockers...)
	}
	if w.HandoffNotes != "" {
		b = append(b, "\nHandoff: "...)
		b = append(b, w.HandoffNotes...)
	}
	if w.RestartWhen != "" {
		b = append(b, "\nWakes when: "...)
		b = append(b, w.RestartWhen...)
	}
	if w.Successor != "" && w.Successor != SuccessorNone {
		b = append(b, "\nSuccessor: "...)
		b = append(b, w.Successor...)
	}
	if w.HarnessGaps != "" {
		b = append(b, "\nHarness gaps: "...)
		b = append(b, w.HarnessGaps...)
	}
	if len(w.BeadsClosed) > 0 {
		b = append(b, "\nBeads closed: "...)
		for i, id := range w.BeadsClosed {
			if i > 0 {
				b = append(b, ", "...)
			}
			b = append(b, id...)
		}
	}
	if len(w.PullRequests) > 0 {
		b = append(b, "\nPRs: "...)
		for i, url := range w.PullRequests {
			if i > 0 {
				b = append(b, ", "...)
			}
			b = append(b, url...)
		}
	}
	for k, v := range w.Custom {
		b = append(b, "\n"...)
		b = append(b, k...)
		b = append(b, ": "...)
		b = append(b, v...)
	}

	return string(b)
}

// WrapUpFieldName is the bead field key where the structured wrap-up is stored.
const WrapUpFieldName = "wrapup"

// wrapUpFieldShapes maps each WrapUp JSON field name to a placeholder matching
// its Go type. Reflected from the struct so a field or type change propagates
// to the invalid-JSON hint automatically instead of drifting (ka-0000006605:
// the hardcoded hint omitted pull_requests and taught agents a bare-string
// shape for a []string field).
func wrapUpFieldShapes() map[string]string {
	shapes := map[string]string{}
	t := reflect.TypeFor[WrapUp]()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		switch f.Type.Kind() {
		case reflect.Slice:
			shapes[name] = `["...", ...]`
		case reflect.String:
			shapes[name] = `"..."`
		default:
			shapes[name] = "..."
		}
	}
	return shapes
}

// wrapUpExpectedFormatHint renders a one-line JSON skeleton of the agent-facing
// wrap-up fields (the required + optional sets from DefaultWrapUpRequirements,
// in that order), each with a placeholder derived from the WrapUp struct's Go
// type via wrapUpFieldShapes. Used in the gb stop invalid---wrapup error.
func wrapUpExpectedFormatHint() string {
	shapes := wrapUpFieldShapes()
	reqs := DefaultWrapUpRequirements()
	var b strings.Builder
	b.WriteString("{")
	for _, name := range append(append([]string{}, reqs.Required...), reqs.Optional...) {
		shape, ok := shapes[name]
		if !ok {
			continue
		}
		if b.Len() > 1 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%q:%s", name, shape)
	}
	b.WriteString("}")
	return b.String()
}

// coerceWrapUpJSON forgives the most common wrap-up shape mistake — a bare
// string for pull_requests where the schema wants []string — by rewriting it
// to a 1-element array before the strict unmarshal, with a warning (forgiving-
// input pattern, cf ka-0000000244 / ka-0000001669). Anything else, including
// other type errors, is passed through untouched for the strict pass to judge.
func coerceWrapUpJSON(raw string) (string, []string) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return raw, nil // not a JSON object — let the strict pass report it
	}
	var s string
	if pr, ok := fields["pull_requests"]; !ok || json.Unmarshal(pr, &s) != nil {
		return raw, nil // absent, or already an array/other type
	}
	arr, err := json.Marshal([]string{s})
	if err != nil {
		return raw, nil
	}
	fields["pull_requests"] = arr
	out, err := json.Marshal(fields)
	if err != nil {
		return raw, nil
	}
	warn := `pull_requests should be a JSON array of URLs (["https://..."] even for one) — coerced the bare string to a 1-element array`
	return string(out), []string{warn}
}

// TLDR length bounds. The soft cap mirrors the ~one-sentence shape we want at
// the top of a Slack agent card and the existing 'tldr' decision-artifact
// convention (advice ka-0000001089); exceeding it warns but is allowed. The
// hard cap is enforced — a longer tldr is rejected so the agent retries with a
// shorter summary rather than the card silently truncating mid-word.
const (
	TLDRSoftCap = 280
	TLDRHardCap = 500
)

// normalizeAndValidateTLDR canonicalizes a wrap-up tldr for safe storage,
// returning the cleaned value, any non-fatal warnings (e.g. soft-cap exceeded,
// newlines collapsed), and a fatal error when the value cannot be accepted
// (hard-cap exceeded).
//
// Normalization, in order:
//   - trim surrounding whitespace
//   - collapse embedded newlines (and runs of whitespace) to single spaces, so
//     the tldr is guaranteed single-line — logged as a warning when it happens
//
// The tldr is stored RAW — angle brackets ('<'/'>') and comparison/arrow
// idioms ('<2h', 'P1->P0') are persisted verbatim. Escaping is a RENDER-TIME
// concern owned by each destination (ka-0000005806): the beads-viewer runs the
// value through Go html/template's auto-escape; the Slack card escapes for
// mrkdwn (escapeSlackMrkdwn) on the thread path and renders plain_text on the
// header path. Escaping here as well produced a DOUBLE escape — the stored
// "&lt;" was auto-escaped again to "&amp;lt;" and operators saw the literal
// "&lt;" on the gardener page.
//
// Length is measured in runes against the stored (raw) value. An
// empty/whitespace-only tldr normalizes to "" with no error — the field is
// optional.
func normalizeAndValidateTLDR(raw string) (clean string, warnings []string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil, nil
	}

	// Collapse any internal whitespace runs (including newlines/tabs) to a
	// single space so the result is single-line. strings.Fields splits on all
	// Unicode whitespace and drops empty tokens.
	collapsed := strings.Join(strings.Fields(trimmed), " ")
	if collapsed != trimmed {
		warnings = append(warnings, "tldr contained newlines or extra whitespace — collapsed to a single line")
	}

	n := utf8.RuneCountInString(collapsed)
	if n > TLDRHardCap {
		return "", warnings, fmt.Errorf("tldr too long (%d > %d chars); suggest a shorter one-line summary", n, TLDRHardCap)
	}
	if n > TLDRSoftCap {
		warnings = append(warnings, fmt.Sprintf("tldr is %d chars (soft cap %d) — consider tightening to one sentence", n, TLDRSoftCap))
	}

	return collapsed, warnings, nil
}

// OutputWrapUpExpectations writes a markdown section describing wrap-up
// requirements to w. Called by gb prime to inject expectations into agent
// context. If no config beads are found, outputs default expectations.
func outputWrapUpExpectations(w io.Writer, agentID string) {
	ctx := context.Background()
	reqs := LoadWrapUpRequirements(ctx, daemon, agentID)
	renderWrapUpExpectations(w, reqs)
}

// renderWrapUpExpectations writes the Wrap-Up Requirements markdown for the
// given (already-resolved) requirements. Split from outputWrapUpExpectations
// so the rendered block — the surface that teaches agents field shapes — is
// unit-testable without the daemon global.
func renderWrapUpExpectations(w io.Writer, reqs WrapUpRequirements) {
	// Don't output anything if enforcement is disabled.
	if reqs.Enforce == "none" {
		return
	}

	fmt.Fprintf(w, "\n## Wrap-Up Requirements\n\n")

	switch reqs.Enforce {
	case "hard":
		fmt.Fprintln(w, "You **must** provide a structured wrap-up when calling `gb stop`.")
	default:
		fmt.Fprintln(w, "You **should** provide a structured wrap-up when calling `gb stop`.")
	}

	fmt.Fprintln(w, "")

	if len(reqs.Required) > 0 {
		fmt.Fprint(w, "**Required fields:** ")
		for i, f := range reqs.Required {
			if i > 0 {
				fmt.Fprint(w, ", ")
			}
			fmt.Fprintf(w, "`%s`", f)
		}
		fmt.Fprintln(w)
	}

	if len(reqs.Optional) > 0 {
		fmt.Fprint(w, "**Optional fields:** ")
		for i, f := range reqs.Optional {
			if i > 0 {
				fmt.Fprint(w, ", ")
			}
			fmt.Fprintf(w, "`%s`", f)
		}
		fmt.Fprintln(w)
	}

	if len(reqs.CustomFields) > 0 {
		fmt.Fprintln(w, "**Custom fields:**")
		for _, cf := range reqs.CustomFields {
			reqStr := "optional"
			if cf.Required {
				reqStr = "required"
			}
			if cf.Description != "" {
				fmt.Fprintf(w, "- `%s` (%s) — %s\n", cf.Name, reqStr, cf.Description)
			} else {
				fmt.Fprintf(w, "- `%s` (%s)\n", cf.Name, reqStr)
			}
		}
	}

	fmt.Fprintf(w, "\nInclude a `tldr` — a single-line outcome summary (≤%d chars) that surfaces at the top of your Slack agent card. Keep it to one sentence; angle brackets are auto-escaped and newlines collapsed.\n", TLDRHardCap)

	fmt.Fprintln(w, "\n**Field shape:**")
	fmt.Fprintln(w, "- `accomplishments` — what you actually changed/merged/spawned this run. Lead with the outcome (KPI deltas, MR numbers, beads closed).")
	fmt.Fprintln(w, "- `blockers` — ONLY a real thing stopping further progress, stated as a concrete next-action someone must take. If nothing is blocking you, **omit the field entirely** — do NOT write \"none\", \"n/a\", or \"-\". A non-empty blockers field stamps your card ⛔ blocked.")
	fmt.Fprintln(w, "- `handoff_notes` — context the next agent/human needs to pick up where you left off. Omit if there's nothing to hand off. Present handoff (no blockers) shows as ↗️ handed-off; a fully clean run shows ✅ done.")
	fmt.Fprintln(w, "- `restart_when` — declare the ONE concrete condition under which you expect to resume, if you expect to at all. Free text, ideally naming the bead that wakes you: `\"when ka-X merges\"`, `\"when the operator resolves decision ka-Y\"`. It renders verbatim as the `↻ Restart:` line on your Slack agent card and marks the card ⏳ progressing rather than terminal — so the operator sees at a glance that you'll be back and why. Omit it for a truly terminal stop (nothing will wake you); do NOT invent a condition just to fill the field.")
	fmt.Fprintln(w, "- `pull_requests` — JSON ARRAY of MR/PR URLs you opened or advanced this run ([\"https://...\"] even for one), auto-linked to beads at stop.")
	fmt.Fprintln(w, "- `successor` — succession intent: `none` (default, terminal — almost always what you want), `same` (recreate this identity after a wake; records intent only today), or `new` (mint a fresh successor agent that carries this work forward). Set via the field or `gb stop --successor <value>`; unknown values are rejected. Leaving `goal_satisfied:\"no\"` with `successor:\"none\"` warns and stays terminal — pick `same`/`new` (or set `respawn_after`) if you want to continue.")
	fmt.Fprintln(w, "- `harness_gaps` — OPTIONAL telemetry: what the *harness itself* lacked this run — a missing tool, a guard that fired wrongly, context you had to reconstruct by hand, a capability you wished for (the MLD sensor pattern: Mistakes / Learnings / Desires). Free text; omit if nothing to report. This is STRICTLY a sensor reading, not a request: it never auto-becomes advice or policy — a later groomer/CED pass corroborates it against your transcript before anything acts on it. Report a genuine friction here; route an operator ACTION through `gb escalation`/`gb decision` as usual.")

	fmt.Fprintf(w, "\n**Example (clean run — no blockers, just leave the field out):**\n```bash\ngb stop --wrapup '{\"tldr\":\"Merged MR 3286; spawned 2 crew on unblocked epic children\",\"accomplishments\":\"Merged MR 3286 (SLACK_THREAD_CARDS plumbing); spawned crew on Change 2 + helm chore; merged_today 0->1\",\"handoff_notes\":\"Change 3/4 still blocked on Change 2; next cycle walk crew MRs to green\",\"pull_requests\":[\"https://gitlab.com/example-org/app/-/merge_requests/3286\"]}'\n```\n")

	// Shift-left routing nudge (epic ka-0000002541). An operator-directed ask
	// left only in blockers/handoff_notes gives the operator NO re-entry handle
	// and (for never-respawn roles) is lost forever. Steer agents to file the
	// right handle BEFORE gb stop, where the soft ask-gate would otherwise nudge.
	fmt.Fprintln(w, "\n**If your wrap-up asks the operator for something, file a handle FIRST:**")
	fmt.Fprintln(w, "- The operator must **DO** something privileged (mint a secret, approve/merge an MR, transition a JIRA ticket, grant IAM, deploy) → `gb escalation create`.")
	fmt.Fprintln(w, "- The operator must **CHOOSE** between divergent next actions (should we / which / A-or-B) → `gb decision create` + `gb yield`.")
	fmt.Fprintln(w, "- An ask left only in `tldr`/`blockers`/`handoff_notes` with no decision/escalation will trip the `gb stop` ask-gate (a one-shot nudge). Restate the wrap-up without the ask, or `--force`, if it was only a status note.")
}

// WrapUpConfigCategory is the config bead category for wrap-up requirements.
// Config beads with this title are resolved using the standard layered merge
// system (global < project < role < agent), allowing different roles to have
// different wrap-up expectations.
const WrapUpConfigCategory = "wrapup-config"

// LoadWrapUpRequirements resolves wrap-up requirements from config beads
// matching the agent's subscriptions. Falls back to DefaultWrapUpRequirements
// if no config beads are found.
//
// The config bead value is a JSON object matching the WrapUpRequirements
// schema. Example config bead (created via gb config load):
//
//	{
//	  "title": "wrapup-config",
//	  "labels": ["role:crew"],
//	  "value": {
//	    "required": ["accomplishments", "blockers"],
//	    "optional": ["handoff_notes", "pull_requests"],
//	    "enforce": "hard"
//	  }
//	}
func LoadWrapUpRequirements(ctx context.Context, lister configBeadLister, agentID string) WrapUpRequirements {
	subs := advice.BuildAgentSubscriptions(agentID, nil)
	merged, count, _ := ResolveConfigBeads(ctx, lister, WrapUpConfigCategory, subs)
	if count == 0 || merged == nil {
		return DefaultWrapUpRequirements()
	}

	// Re-serialize the merged map and unmarshal into WrapUpRequirements.
	data, err := json.Marshal(merged)
	if err != nil {
		return DefaultWrapUpRequirements()
	}

	var reqs WrapUpRequirements
	if err := json.Unmarshal(data, &reqs); err != nil {
		return DefaultWrapUpRequirements()
	}

	// Apply defaults for unset fields.
	if reqs.Enforce == "" {
		reqs.Enforce = "soft"
	}
	if len(reqs.Required) == 0 && len(reqs.Optional) == 0 {
		return DefaultWrapUpRequirements()
	}

	return reqs
}
