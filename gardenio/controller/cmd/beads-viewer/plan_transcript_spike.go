package main

// plan_transcript_spike.go — SPIKE (task ka-0000000338, Phase 1 of plan
// ka-0000005794). Validates the D1=hybrid transcript-persistence wiring by
// feeding type=message beads through the EXISTING pure chat projector
// (projectChatMessage / mapChatView, chat_message.go) with the two seams the
// research brief flags as "deliberate wiring, not drop-in":
//
//  1. COLLECTION by fields.plan_id (NOT by ka-mention). The brief warns
//     bead_chat.go is mention-scoped; the spike below proves WHY that path is
//     the wrong one for a transcript and what to use instead. kbeads extracts
//     mention edges only from Title/Description/Notes/comments — never from
//     fields.text where `gb squawk` stores the literal line
//     (kbeads/internal/store/postgres/queries_references.go:44-49). So a plan
//     turn minted the squawk way carries NO reliable edge to the plan epic
//     (short messages accidentally leak a Title mention when the epic id lands
//     in the first ~497 chars; long ones do not — an unstable basis for a
//     transcript). The hybrid model instead binds each turn bead to the plan by
//     a purpose-named field (fields.plan_id == epicID) — the SAME binding
//     planAssistant() already uses for the assistant agent bead
//     (plans_assistant.go:55,94) — and queries it directly. Durable,
//     respawn-proof, reconstructable; no dependence on prose scanning.
//
//  2. DIRECTION OVERRIDE. projectChatMessage classifies direction from bead
//     SHAPE (messageDirection, chat_message.go:239) — good enough for the
//     bead-show panel but WRONG for a plan transcript: an operator prompt bead
//     with no slack label / no source_agent / not created_by=slack-bridge
//     falls through to directionFromAgent and renders on the wrong side. The
//     user inbox already dodges this by OVERRIDING msg.Direction from the query
//     population it came from (user_inbox.go:191-192). The transcript does the
//     same: it knows, per turn, whether the bead is an operator prompt or an
//     assistant reply (from fields.turn_role) and stamps Direction explicitly
//     rather than trusting the heuristic.
//
// This file is a PROTOTYPE, not a shipped surface. It reuses the real
// projector verbatim (no fork), so if it renders correctly the Phase 2 build
// is "mint the beads + call this builder", not "rewrite the renderer". The
// accompanying golden test (plan_transcript_spike_test.go) exercises all three
// D1 model variants (bead-backed / view-over-coop / hybrid) against this
// builder so the decision is evidence-backed.

import (
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/plantranscript"
)

// The plan-transcript WRITE contract (turn-bead field/role vocabulary, the
// turn-bead builders, the dedup key) is the shared internal/plantranscript
// package, so the viewer's operator-turn mint and the rehomed
// assistantturnminter (now controller-hosted, task ka-0000006891) agree
// byte-for-byte on the shape. These aliases keep the viewer's render-side and
// mint-side call sites reading against local names while the definitions live
// once in plantranscript.
const (
	planTranscriptPlanIDField   = plantranscript.PlanIDField
	planTranscriptTurnRoleField = plantranscript.TurnRoleField
	planTranscriptTurnIDField   = plantranscript.TurnIDField

	planTurnRoleOperator  = plantranscript.TurnRoleOperator
	planTurnRoleAssistant = plantranscript.TurnRoleAssistant
	planTurnRoleTool      = plantranscript.TurnRoleTool

	planTranscriptToolNameField   = plantranscript.ToolNameField
	planTranscriptToolArgsField   = plantranscript.ToolArgsSummaryField
	planTranscriptToolResultField = plantranscript.ToolResultSummaryField
)

// planTranscriptMessagesQuery is the ListBeads query that pulls a plan's
// transcript turns: every type=message bead bound to the epic by
// fields.plan_id, newest-first so the panel survives the mapChatMaxMessages cap
// (buildPlanTranscript re-sorts oldest→newest for display, mirroring
// agentAuthoredMessagesQuery / buildBeadChat). This is the collection seam the
// brief calls "deliberate wiring": we bind + query by field, NOT by mention.
//
// NOTE: whether the daemon can filter on an arbitrary custom field is the one
// capability this query assumes. planAssistant() already relies on exactly this
// (ListBeadsFiltered with Fields:{plan_id:epicID}, plans_assistant.go:91-95),
// so the assumption is already load-bearing in shipped code — the spike does
// not introduce a new daemon requirement.
func planTranscriptMessagesQuery(epicID string) beadsapi.ListBeadsQuery {
	return beadsapi.ListBeadsQuery{
		Types:  []string{"message"},
		Fields: map[string]string{planTranscriptPlanIDField: epicID},
		Sort:   "-created_at",
		Limit:  mapChatMaxMessages,
	}
}

// planTurnDirection maps a turn bead to its chat Direction from the EXPLICIT
// fields.turn_role marker, never from bead shape. operator → own-side bubble
// (directionFromUser); everything else (assistant, or an unmarked agent turn)
// → other-side (directionFromAgent). This is the override that keeps operator
// prompts on the operator's side of the feed.
func planTurnDirection(b *beadsapi.BeadDetail) string {
	if b != nil && b.Fields != nil {
		if strings.EqualFold(strings.TrimSpace(b.Fields[planTranscriptTurnRoleField]), planTurnRoleOperator) {
			return directionFromUser
		}
	}
	return directionFromAgent
}

// isToolTurn reports whether a turn bead is a TOOL CALL (turn_role=tool) rather
// than a prose turn (Phase A, ka-0000004601). A tool turn renders as a compact
// collapsible tool entry; everything else renders as a chat bubble.
func isToolTurn(b *beadsapi.BeadDetail) bool {
	return b != nil && b.Fields != nil &&
		strings.EqualFold(strings.TrimSpace(b.Fields[planTranscriptTurnRoleField]), planTurnRoleTool)
}

// planTranscriptToolArgsMax bounds the inline args summary so one pathological
// tool invocation (a giant Bash command, a long URL) cannot blow out the chat
// log's single-line entry. coop's extract_tool_label already truncates to
// 30-50 chars, but the render side re-applies a defensive ceiling so a turn
// minted by any future producer — or a hand-authored/legacy one — stays compact.
const planTranscriptToolArgsMax = 120

// planTranscriptToolResultMax bounds the (expanded) result summary. Kept modest:
// the transcript is a glanceable record of WHAT the agent did, not a full tool
// output log — the agent's own reply (the assistant turn) carries the narrative.
const planTranscriptToolResultMax = 400

// decorateToolTurn stamps the tool-specific fields onto an already-projected
// chat line when the underlying bead is a tool turn. It reads the structured
// tool fields (tool_name / tool_args_summary / tool_result_summary) the tool-turn
// bead contract defines, defensively re-truncates the free-text summaries, and
// falls back to the projected body text for the args summary when the structured
// field is absent (a legacy/plain tool turn). A tool turn with no resolvable name
// is left as an ordinary line (IsTool stays false) so it never renders as an
// empty tool chip.
func decorateToolTurn(m chatMessage, b *beadsapi.BeadDetail) chatMessage {
	if !isToolTurn(b) {
		return m
	}
	name := strings.TrimSpace(b.Fields[planTranscriptToolNameField])
	args := strings.TrimSpace(b.Fields[planTranscriptToolArgsField])
	result := strings.TrimSpace(b.Fields[planTranscriptToolResultField])
	if name == "" {
		// A tool turn with no tool_name is unusable as a tool chip — keep it as an
		// ordinary agent-side line (fields.text still rendered by the projector)
		// rather than paint a nameless tool entry.
		return m
	}
	if args == "" {
		// Legacy/plain tool turn: recover a summary from the human-readable
		// fallback line the projector already put in the body.
		args = strings.TrimSpace(messageBodyText(b))
	}
	m.IsTool = true
	m.ToolName = truncate(name, 60)
	m.ToolArgs = truncate(args, planTranscriptToolArgsMax)
	m.ToolResult = truncate(result, planTranscriptToolResultMax)
	return m
}

// buildPlanTranscript projects a plan's hydrated transcript turn beads into the
// SAME mapChatView the bead-show chat panel and user inbox render — so the
// existing bead_chat.html direction-bubble template renders it verbatim. The
// only differences from buildBeadChat are (a) the input is collected by
// fields.plan_id rather than by mention, and (b) Direction is stamped from
// fields.turn_role (the operator/assistant override) rather than trusted from
// the shape heuristic. PURE (no http, no daemon) → unit-testable, exactly like
// its buildBeadChat sibling.
//
// beads is the hydrated BeadDetail set (order-independent; the builder sorts by
// time). Non-message / nil beads are skipped. Newest mapChatMaxMessages kept;
// dropped older count reported via OlderHidden/HiddenCount.
func buildPlanTranscript(
	beads []*beadsapi.BeadDetail,
	basePath string,
	now time.Time,
) mapChatView {
	if len(beads) == 0 {
		return mapChatView{}
	}

	type stamped struct {
		ts  time.Time
		msg chatMessage
	}
	// dedupKey identifies a logical turn: fields.turn_id when the mint stamped
	// one (the load-bearing dedup key), else the bead id (legacy/spike shape).
	// Two beads sharing a turn_id are the SAME turn observed twice (a live coop
	// event plus a JetStream re-delivery, or two viewer replicas racing the
	// mint before the daemon idempotency key converged them) — render once. The
	// mint-time IdempotencyKey is the authoritative guard; this is the render-
	// side belt-and-suspenders for a transient window where two rows slipped
	// through (e.g. an idempotency miss during a migration).
	dedupKey := func(b *beadsapi.BeadDetail) string {
		if b.Fields != nil {
			if t := strings.TrimSpace(b.Fields[planTranscriptTurnIDField]); t != "" {
				return "turn:" + t
			}
		}
		return "id:" + b.ID
	}
	rows := make([]stamped, 0, len(beads))
	// rowByKey indexes into rows so a duplicate turn can UPGRADE the kept row to
	// the one with the longer (non-truncated) fields.text — a >256KB reply
	// persisted truncated by one observer must lose to the same turn persisted
	// full by another.
	rowByKey := make(map[string]int, len(beads))
	for _, b := range beads {
		if !isMessageBeadDetail(b) || b.ID == "" {
			continue
		}
		key := dedupKey(b)

		// Prefer created-time for chat ordering (mirrors buildBeadChat): a chat
		// log reads by when each line was said, not when it was last edited.
		ts, ok := pickAgeTimestamp(b.CreatedAt, b.UpdatedAt)
		if !ok {
			ts = time.Time{} // sorts to the front; still rendered
		}

		msg := projectChatMessage(b, basePath, now, LinkifyOpts{}) // REUSED VERBATIM
		msg.Direction = planTurnDirection(b)                       // ...with the direction override
		msg = decorateToolTurn(msg, b)                             // ...and the tool-turn decoration (Phase A)

		if idx, dup := rowByKey[key]; dup {
			// Same turn seen twice: keep the row with the longer body (the
			// non-truncated copy wins) so a truncated-then-full re-mint renders
			// the full text. Ties keep the first (stable).
			if len(string(msg.BodyHTML)) > len(string(rows[idx].msg.BodyHTML)) {
				rows[idx] = stamped{ts: ts, msg: msg}
			}
			continue
		}
		rowByKey[key] = len(rows)
		rows = append(rows, stamped{ts: ts, msg: msg})
	}
	if len(rows) == 0 {
		return mapChatView{}
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].ts.Equal(rows[j].ts) {
			return rows[i].ts.Before(rows[j].ts)
		}
		return rows[i].msg.ID < rows[j].msg.ID // stable tiebreak for golden tests
	})

	total := len(rows)
	hidden := 0
	if total > mapChatMaxMessages {
		hidden = total - mapChatMaxMessages
		rows = rows[total-mapChatMaxMessages:]
	}

	msgs := make([]chatMessage, 0, len(rows))
	for _, r := range rows {
		msgs = append(msgs, r.msg)
	}
	return mapChatView{
		Messages:    msgs,
		Shown:       len(msgs),
		Total:       total,
		OlderHidden: hidden > 0,
		HiddenCount: hidden,
		HasMessages: true,
	}
}

// planTranscriptTurnBead builds the CreateBead request for one transcript turn.
// It delegates to the shared plantranscript.TurnBead write contract so the
// viewer's operator-turn mint (handlePlanAssistantPrompt) and the rehomed,
// controller-hosted assistantturnminter produce byte-identical turn beads.
// Kept as a thin local wrapper so the viewer's render-side + mint-side call
// sites read against a local name.
func planTranscriptTurnBead(epicID, author, turnRole, text, turnID string) beadsapi.CreateBeadRequest {
	return plantranscript.TurnBead(epicID, author, turnRole, text, turnID)
}

// planTurnIdempotencyKey is the shared dedup-key builder for a plan-transcript
// turn, delegating to the plantranscript write contract. Assistant turns key on
// the coop turn id ("<session>:<ts>:<hash>"); operator turns key on "op:<hash>"
// (handlePlanAssistantPrompt).
func planTurnIdempotencyKey(planID, turnID string) string {
	return plantranscript.IdempotencyKey(planID, turnID)
}
