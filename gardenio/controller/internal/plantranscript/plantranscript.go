// Package plantranscript is the ONE shared WRITE contract for /plans chat
// transcript turns — the type=message turn-bead shape, its field/role
// vocabulary, the process-independent dedup key, and the plan-assistant
// identity predicate — used by BOTH the beads-viewer (which mints operator
// turns synchronously and renders the transcript) and the always-on
// AssistantTurnMinter (which mints assistant turns from coop's NATS
// assistant_text stream).
//
// WHY A SHARED PACKAGE (task ka-0000006891, P3 of plan ka-0000000187): the
// AssistantTurnMinter is being rehomed off the viewer process into the
// controller so plan transcripts persist independent of whether an operator
// has a cockpit open. The minter builds the SAME turn bead the viewer's
// operator-turn mint builds and keys on the SAME dedup namespace, so both mint
// sites — now in two different binaries — must agree byte-for-byte on the
// write contract. Before this package that contract lived in package main
// under cmd/beads-viewer (plan_transcript_spike.go) and was unimportable by the
// controller; lifting the pure builders + constants here makes the contract a
// single authority both binaries share, exactly as internal/plangardener is the
// one gardener-spec builder shared by `gb plan gardener` and the viewer.
//
// PURE by design: no NATS, no HTTP, no daemon — just request builders and
// predicates over beadsapi types, so it is trivially unit-testable and carries
// no runtime dependency into either importer. The render side of the transcript
// (buildPlanTranscript / projectChatMessage) stays in the viewer; only the
// write contract and identity are shared.
package plantranscript

import (
	"encoding/json"
	"slices"

	"gardenio/controller/internal/beadsapi"
)

// PlanIDField is the message-bead field that binds one transcript turn to its
// plan epic — and, on an agent bead, binds the plan's assistant to the plan.
// Deliberately the SAME field name for both so a plan's assistant, its steps,
// and its transcript all key off one stable, queryable, purpose-named id — no
// reliance on ka-mention prose scanning (the collection seam the transcript
// research brief calls "deliberate wiring"). kbeads extracts mention edges only
// from Title/Description/Notes/comments, never from fields.text, so a
// field-scoped binding is the durable, respawn-proof edge.
const PlanIDField = "plan_id"

// TaskIDField is the agent-bead field a plan gardener iteration also carries
// (set to the same epic) so the assistant's activity lane + rollups treat the
// plan as its task. IsPlanAssistantBead accepts a binding via EITHER field.
const TaskIDField = "task_id"

// TurnRoleField records who authored a turn — operator / assistant / tool — so
// the transcript renderer can stamp chat Direction EXPLICITLY instead of
// inferring it from bead shape (which misfires for operator prompts). Absent /
// unknown is treated as an assistant turn (the common agent-side case).
const TurnRoleField = "turn_role"

// TurnIDField carries a stable per-turn identifier so the paths that can
// persist the same conversational turn — the AssistantTurnMinter observing a
// coop assistant_text event, and a live-vs-backfill re-delivery of that same
// event — converge on ONE transcript row. It is the render-side half of the
// dedup contract; the mint-time IdempotencyKey (below) is the authoritative,
// process-independent guard. Absent on legacy/spike beads → the render-side
// seen-set falls back to bead.ID, unchanged.
const TurnIDField = "turn_id"

// Turn-role values stamped into TurnRoleField.
const (
	// TurnRoleOperator marks a turn authored by the human operator (a /plans
	// message). Renders on the operator (own) side of the direction-bubble log.
	TurnRoleOperator = "operator"
	// TurnRoleAssistant marks a turn authored by the plan agent (its reply).
	// Renders on the agent (other) side.
	TurnRoleAssistant = "assistant"
	// TurnRoleTool marks a turn that is a TOOL CALL the plan agent made (not
	// prose). It renders on the agent side like an assistant turn but is painted
	// as a compact, collapsible tool entry rather than a chat bubble.
	TurnRoleTool = "tool"
)

// Tool-turn field names (Phase A). A tool turn is an ordinary type=message turn
// bead (turn_role=tool) that additionally carries these fields so the renderer
// can paint a structured tool entry. Mirrors coop's tool-cause vocabulary so a
// tool_use event maps 1:1:
//
//   - ToolNameField        the tool invoked ("Bash", "Read", "Grep", …).
//   - ToolArgsSummaryField a compact one-line arg summary.
//   - ToolResultSummaryField an OPTIONAL short result summary shown on expand.
//
// fields.text still carries a human-readable fallback line so a tool turn
// degrades gracefully through the plain projector and any non-plan surface.
const (
	ToolNameField          = "tool_name"
	ToolArgsSummaryField   = "tool_args_summary"
	ToolResultSummaryField = "tool_result_summary"
)

// PlanGardenerRole is the unified, first-class plan agent role: a single agent
// that is BOTH the plan's watchdog gardener and its interactive agent. Every
// plan agent bead carries this role, and both the viewer and the minter resolve
// the plan's agent by it alongside fields.plan_id. Kept canonical here so the
// two binaries never drift on the role name; mirrors plangardener.SpawnRole.
const PlanGardenerRole = "plan-gardener"

// PlanGardenerRoleLabel is the sole role label the resolver keys on (alongside
// fields.plan_id) — stamped on every plan agent bead.
const PlanGardenerRoleLabel = "role:" + PlanGardenerRole

// TurnBead builds the CreateBead request for one prose transcript turn under
// the hybrid persistence model — the write side both the viewer's operator-turn
// mint and the AssistantTurnMinter's assistant-turn mint call:
//
//   - fields.plan_id binds the turn to the plan (the queryable edge).
//   - fields.turn_role stamps the authored side (the direction override input).
//   - fields.turn_id is the stable per-turn dedup key. The caller ALSO wires it
//     into CreateBeadRequest.IdempotencyKey (via IdempotencyKey below) so a
//     re-delivered coop event or a second replica converges on ONE bead.
//   - fields.text carries the literal line the projector renders.
//   - a from:<author> label + fields.source_agent keep the turn attributable.
//   - the plan epic id is embedded in the Description so kbeads emits a durable
//     mention edge too (belt-and-suspenders: the transcript reads by field, but
//     the mention keeps the turn discoverable from the epic's constellation for
//     free — kbeads extracts mention edges from Description, never fields.text).
//
// The caller sets CreateBeadRequest.IdempotencyKey (this helper stays a pure
// request builder testable without a turn-id policy); an empty turnID omits the
// field (legacy/test shape) and the render-side seen-set falls back to bead.ID.
func TurnBead(epicID, author, turnRole, text, turnID string) beadsapi.CreateBeadRequest {
	title := text
	if len(title) > 500 {
		title = title[:497] + "..."
	}
	fieldsMap := map[string]string{
		PlanIDField:    epicID,
		TurnRoleField:  turnRole,
		"source_agent": author,
		"text":         text,
	}
	if turnID != "" {
		fieldsMap[TurnIDField] = turnID
	}
	// json.Marshal of a map[string]string never fails, but mirror the spawn
	// path's explicit-error handling shape by discarding the (always-nil) error.
	fieldsJSON, _ := json.Marshal(fieldsMap)
	return beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "message",
		Kind:        "data",
		Description: "Plan transcript turn for " + epicID,
		Labels:      []string{"plan-transcript", "from:" + author},
		Fields:      json.RawMessage(fieldsJSON),
		CreatedBy:   author,
	}
}

// ToolTurnBead builds the CreateBead request for one TOOL-CALL transcript turn
// (Phase A). It is the tool-turn counterpart to TurnBead: the same base turn
// shape (type=message, plan_id binding, turn_id dedup key, from:<author> label,
// epic mention in the Description) with turn_role=tool PLUS the structured tool
// fields the renderer reads. fields.text is a human-readable fallback line
// ("<tool>: <args>", or just "<tool>" when no args) so the turn degrades
// gracefully through the plain projector. Empty tool-summary fields are omitted
// so a legacy/plain tool turn round-trips. As with TurnBead the caller sets the
// IdempotencyKey.
func ToolTurnBead(epicID, author, toolName, argsSummary, resultSummary, turnID string) beadsapi.CreateBeadRequest {
	text := toolName
	if argsSummary != "" {
		text = toolName + ": " + argsSummary
	}
	req := TurnBead(epicID, author, TurnRoleTool, text, turnID)

	// Fold the structured tool fields into the base turn's field set. Unmarshal →
	// set → re-marshal keeps TurnBead the single authority for the base shape.
	var fields map[string]string
	if err := json.Unmarshal(req.Fields, &fields); err == nil {
		fields[ToolNameField] = toolName
		if argsSummary != "" {
			fields[ToolArgsSummaryField] = argsSummary
		}
		if resultSummary != "" {
			fields[ToolResultSummaryField] = resultSummary
		}
		if remarshalled, err := json.Marshal(fields); err == nil {
			req.Fields = remarshalled
		}
	}
	return req
}

// IdempotencyKey is the shared dedup-key builder for a plan-transcript turn,
// the authoritative process-independent guard (kbeads lookup-or-return). It is
// what makes a re-delivered event, a concurrent live+replay observation, a
// second viewer/controller replica, and a future additional observer all
// converge on ONE bead. Assistant turns key on the coop turn id
// ("<session>:<ts>:<hash>"); operator turns key on "op:<hash>". Keeping the
// prefix convention in one place documents the namespace both mint sites share.
func IdempotencyKey(planID, turnID string) string {
	return "plan-turn:" + planID + ":" + turnID
}

// IsPlanAssistantBead reports whether an agent bead is a plan's bound agent: it
// carries the role:plan-gardener label AND is bound to the epic via
// fields.plan_id / fields.task_id. The role-label check keeps a same-name
// non-plan agent from being mistaken for a plan agent; the field check scopes
// it to the right plan. Mirrors the viewer's resolver so the surfaces never
// drift.
func IsPlanAssistantBead(b *beadsapi.BeadDetail, epicID string) bool {
	if b == nil || !slices.Contains(b.Labels, PlanGardenerRoleLabel) {
		return false
	}
	if b.Fields[PlanIDField] == epicID || b.Fields[TaskIDField] == epicID {
		return true
	}
	return false
}
