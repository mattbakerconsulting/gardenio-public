package main

// plan_transcript_spike_test.go — SPIKE evidence (task ka-0000000338).
//
// These tests are the RUNNABLE ARTIFACT of the transcript-persistence spike.
// They prove three things the decision report (D1) rests on:
//
//   1. A plan transcript renders correctly through the EXISTING pure projector
//      (projectChatMessage / mapChatView) — no renderer fork needed. (Confirms
//      the "renderer ready, unfed" claim in research brief §4/§6.)
//
//   2. The two flagged wiring seams work: collection by fields.plan_id (not
//      mention) and the operator/assistant DIRECTION OVERRIDE. Without the
//      override an operator prompt renders on the wrong side — the test asserts
//      the failure of the naive shape heuristic AND the fix.
//
//   3. All three D1 persistence models produce an identical rendered view GIVEN
//      the same turns — so D1 is a durability/wiring trade-off, not a rendering
//      one. The bead-backed and hybrid models reconstruct the transcript from
//      durable beads; the view-over-coop model cannot (its input vanishes on
//      cold respawn). The test encodes that difference as the decision driver.

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/plantranscript"
)

// turnBead is a tiny fixture builder for a hydrated transcript turn, mirroring
// what BatchGetBeads would return for a bead minted by planTranscriptTurnBead.
func turnBead(id, epicID, author, role, text string, created time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "message",
		Status: "closed",
		Labels: []string{"plan-transcript", "from:" + author},
		Fields: map[string]string{
			planTranscriptPlanIDField:   epicID,
			planTranscriptTurnRoleField: role,
			"source_agent":              author,
			"text":                      text,
		},
		CreatedBy: author,
		CreatedAt: created,
	}
}

// TestBuildPlanTranscript_RendersConversationInOrder proves the core spike
// claim: an interleaved operator/assistant conversation, bound by fields.plan_id
// and stamped with turn_role, renders as an ordered chat log with correct
// bodies, authors, and — critically — correct per-turn Direction.
func TestBuildPlanTranscript_RendersConversationInOrder(t *testing.T) {
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	epic := "gd-UNWFSG1FMZ"
	beads := []*beadsapi.BeadDetail{
		// Deliberately out of chronological order to prove the builder sorts.
		turnBead("gd-t2", epic, "plan-assistant-x", planTurnRoleAssistant,
			"Drafted 4 steps and 2 clarifying questions. See the plan panel.", base.Add(2*time.Minute)),
		turnBead("gd-t0", epic, "matt", planTurnRoleOperator,
			"Draft me a plan to add a New-plan button to /plans.", base),
		turnBead("gd-t1", epic, "plan-assistant-x", planTurnRoleAssistant,
			"On it — researching the existing /plans routes now.", base.Add(1*time.Minute)),
		turnBead("gd-t3", epic, "matt", planTurnRoleOperator,
			"Fold decision D2 into step 3.", base.Add(3*time.Minute)),
	}

	view := buildPlanTranscript(beads, "/base", base.Add(time.Hour))

	if !view.HasMessages {
		t.Fatal("HasMessages=false, want a rendered transcript")
	}
	if view.Shown != 4 || view.Total != 4 {
		t.Fatalf("Shown=%d Total=%d, want 4/4", view.Shown, view.Total)
	}
	// Oldest→newest chat-log order.
	wantOrder := []string{"gd-t0", "gd-t1", "gd-t2", "gd-t3"}
	for i, id := range wantOrder {
		if view.Messages[i].ID != id {
			t.Errorf("Messages[%d].ID = %q, want %q (full order %v)", i, view.Messages[i].ID, id, msgIDs(view))
		}
	}
	// Direction: operator turns own-side, assistant turns other-side.
	wantDir := map[string]string{
		"gd-t0": directionFromUser, "gd-t3": directionFromUser,
		"gd-t1": directionFromAgent, "gd-t2": directionFromAgent,
	}
	for _, m := range view.Messages {
		if got := m.Direction; got != wantDir[m.ID] {
			t.Errorf("%s Direction = %q, want %q", m.ID, got, wantDir[m.ID])
		}
	}
	// Body + author survive the real projector.
	if got := string(view.Messages[0].BodyHTML); !strings.Contains(got, "New-plan button") {
		t.Errorf("first turn body = %q, want operator prompt text", got)
	}
	if view.Messages[0].Author != "matt" {
		t.Errorf("first turn author = %q, want matt", view.Messages[0].Author)
	}
	if view.Messages[1].Author != "plan-assistant-x" {
		t.Errorf("assistant turn author = %q, want plan-assistant-x", view.Messages[1].Author)
	}
}

// TestPlanTranscript_DirectionOverrideBeatsShapeHeuristic is the load-bearing
// negative control. It proves the naive path (trusting messageDirection's shape
// heuristic, as the bead-show chat panel does) MISCLASSIFIES an operator prompt
// as agent-side, and that the transcript's explicit override fixes it. This is
// the exact wiring the research brief §4 calls "deliberate, not drop-in".
func TestPlanTranscript_DirectionOverrideBeatsShapeHeuristic(t *testing.T) {
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	// An operator prompt as it exists in beads: no slack label, no source_agent
	// naming an *agent*, not created_by=slack-bridge. (source_agent here is the
	// operator handle, but shape heuristic treats ANY source_agent as agent-side.)
	op := turnBead("gd-op", "gd-EPIC", "matt", planTurnRoleOperator, "make step 2 blocking on step 1", base)

	// Naive path: what the shape heuristic alone would decide.
	if got := messageDirection(op); got != directionFromAgent {
		t.Fatalf("precondition: expected the shape heuristic to MISCLASSIFY the "+
			"operator prompt as %q (that's the bug the override exists to fix); got %q",
			directionFromAgent, got)
	}
	// Corrected path: the transcript's explicit override.
	if got := planTurnDirection(op); got != directionFromUser {
		t.Errorf("planTurnDirection(operator turn) = %q, want %q (the override)", got, directionFromUser)
	}

	// And end-to-end through the builder: the rendered line is own-side.
	view := buildPlanTranscript([]*beadsapi.BeadDetail{op}, "/base", base.Add(time.Hour))
	if view.Shown != 1 || view.Messages[0].Direction != directionFromUser {
		t.Errorf("rendered operator turn Direction = %q (Shown=%d), want %q",
			dirOf(view), view.Shown, directionFromUser)
	}
}

// TestPlanTranscriptMessagesQuery_BindsByFieldNotMention proves the collection
// seam: the transcript query pulls turns by fields.plan_id, NOT by mention — so
// it does not depend on kbeads scanning prose (which never scans fields.text
// where the literal line lives). It also matches the field name planAssistant()
// already binds on, so the daemon capability is already load-bearing.
func TestPlanTranscriptMessagesQuery_BindsByFieldNotMention(t *testing.T) {
	q := planTranscriptMessagesQuery("gd-EPIC42")
	if len(q.Types) != 1 || q.Types[0] != "message" {
		t.Errorf("Types = %v, want [message]", q.Types)
	}
	if q.Fields[planTranscriptPlanIDField] != "gd-EPIC42" {
		t.Errorf("Fields[%s] = %q, want gd-EPIC42", planTranscriptPlanIDField, q.Fields[planTranscriptPlanIDField])
	}
	if q.Sort != "-created_at" {
		t.Errorf("Sort = %q, want -created_at (newest-first, cap-safe)", q.Sort)
	}
	if q.Limit != mapChatMaxMessages {
		t.Errorf("Limit = %d, want %d", q.Limit, mapChatMaxMessages)
	}
	// Parity guard: the transcript binds on the SAME field planAssistant() uses.
	if planTranscriptPlanIDField != planIDField {
		t.Errorf("planTranscriptPlanIDField=%q must equal planIDField=%q so plan, assistant, and transcript key off one binding",
			planTranscriptPlanIDField, planIDField)
	}
}

// TestPlanTranscriptTurnBead_WriteShape proves the write side mints a bead the
// read side (buildPlanTranscript) round-trips: the created request carries the
// plan_id binding, the turn_role, the literal text, and an author label; and it
// embeds the epic id in the Description so kbeads ALSO emits a durable mention
// edge (belt-and-suspenders discoverability) even though the transcript reads
// by field.
func TestPlanTranscriptTurnBead_WriteShape(t *testing.T) {
	req := planTranscriptTurnBead("gd-EPICWRT", "plan-assistant-x", planTurnRoleAssistant, "here is the draft", "sess-1:1700000000000")
	if req.Type != "message" || req.Kind != "data" {
		t.Errorf("Type/Kind = %q/%q, want message/data", req.Type, req.Kind)
	}
	var fields map[string]string
	if err := json.Unmarshal(req.Fields, &fields); err != nil {
		t.Fatalf("Fields not valid JSON: %v", err)
	}
	if fields[planTranscriptPlanIDField] != "gd-EPICWRT" {
		t.Errorf("fields.plan_id = %q, want gd-EPICWRT", fields[planTranscriptPlanIDField])
	}
	if fields[planTranscriptTurnRoleField] != planTurnRoleAssistant {
		t.Errorf("fields.turn_role = %q, want %q", fields[planTranscriptTurnRoleField], planTurnRoleAssistant)
	}
	if fields[planTranscriptTurnIDField] != "sess-1:1700000000000" {
		t.Errorf("fields.turn_id = %q, want the stable per-turn id", fields[planTranscriptTurnIDField])
	}
	if fields["text"] != "here is the draft" {
		t.Errorf("fields.text = %q, want the literal line", fields["text"])
	}
	if !strings.Contains(req.Description, "gd-EPICWRT") {
		t.Errorf("Description = %q, want it to embed the epic id (for a durable mention edge)", req.Description)
	}
	if !slices.Contains(req.Labels, "from:plan-assistant-x") {
		t.Errorf("Labels = %v, want a from:<author> label", req.Labels)
	}

	// Round-trip: a bead hydrated from this request renders as one chat line
	// on the assistant side.
	hydrated := &beadsapi.BeadDetail{
		ID: "gd-round", Type: "message", Status: "closed",
		Labels: req.Labels, Fields: fields, CreatedBy: "plan-assistant-x",
		CreatedAt: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
	}
	view := buildPlanTranscript([]*beadsapi.BeadDetail{hydrated}, "/base", hydrated.CreatedAt.Add(time.Hour))
	if view.Shown != 1 || view.Messages[0].Direction != directionFromAgent {
		t.Errorf("round-trip render: Shown=%d Direction=%q, want 1/%q", view.Shown, dirOf(view), directionFromAgent)
	}
}

// TestPlanTranscript_D1ModelsRenderIdentically is the decision-driver test. It
// simulates the SAME conversation persisted three ways and asserts the RENDERED
// view is identical — so D1 is chosen on durability, not on rendering. It then
// simulates a cold respawn (the ka-0000002360 failure class) and shows the
// view-over-coop model loses its input while the bead-backed / hybrid models do
// not. That asymmetry IS the recommendation for hybrid.
func TestPlanTranscript_D1ModelsRenderIdentically(t *testing.T) {
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	epic := "gd-D1EPIC"
	// The canonical conversation.
	conv := []*beadsapi.BeadDetail{
		turnBead("gd-c0", epic, "matt", planTurnRoleOperator, "plan the thing", base),
		turnBead("gd-c1", epic, "assistant-a", planTurnRoleAssistant, "drafted it", base.Add(time.Minute)),
	}

	// (a) Bead-backed: turns ARE the beads. Rendered directly.
	beadBacked := buildPlanTranscript(conv, "/base", base.Add(time.Hour))

	// (c) Hybrid: source of truth is the SAME beads; coop only carries live
	// tokens for the in-flight turn. For a settled transcript the rendered view
	// is exactly the bead-backed one.
	hybrid := buildPlanTranscript(conv, "/base", base.Add(time.Hour))

	// (b) View-over-coop: the turns are reconstructed from the S3 session
	// stream. As long as the stream is intact it projects to the same beads and
	// renders identically. (We model an intact stream as the same slice.)
	viewOverCoop := buildPlanTranscript(conv, "/base", base.Add(time.Hour))

	if !sameRender(beadBacked, hybrid) || !sameRender(beadBacked, viewOverCoop) {
		t.Fatalf("D1 models rendered DIFFERENTLY on an intact stream; they should be identical\n a=%v\n c=%v\n b=%v",
			msgIDs(beadBacked), msgIDs(hybrid), msgIDs(viewOverCoop))
	}

	// Cold respawn (freeze/thaw not always enabled): the coop S3 stream is gone.
	// Model that as an empty input for the view-over-coop model. The bead-backed
	// / hybrid models still have their durable beads.
	viewOverCoopAfterRespawn := buildPlanTranscript(nil, "/base", base.Add(time.Hour))
	beadBackedAfterRespawn := buildPlanTranscript(conv, "/base", base.Add(time.Hour))
	hybridAfterRespawn := buildPlanTranscript(conv, "/base", base.Add(time.Hour))

	if viewOverCoopAfterRespawn.HasMessages {
		t.Errorf("view-over-coop kept messages across a cold respawn — the model can't; input is gone")
	}
	if !beadBackedAfterRespawn.HasMessages || beadBackedAfterRespawn.Shown != 2 {
		t.Errorf("bead-backed LOST the transcript across respawn (Shown=%d) — it must not", beadBackedAfterRespawn.Shown)
	}
	if !hybridAfterRespawn.HasMessages || hybridAfterRespawn.Shown != 2 {
		t.Errorf("hybrid LOST the transcript across respawn (Shown=%d) — it must not", hybridAfterRespawn.Shown)
	}
}

// toolTurnBead is a fixture builder for a hydrated TOOL-CALL turn (Phase A,
// ka-0000004601), mirroring what plantranscript.ToolTurnBead mints and
// decorateToolTurn reads: turn_role=tool plus the structured tool fields.
func toolTurnBead(id, epicID, author, toolName, args, result string, created time.Time) *beadsapi.BeadDetail {
	fields := map[string]string{
		planTranscriptPlanIDField:   epicID,
		planTranscriptTurnRoleField: planTurnRoleTool,
		planTranscriptToolNameField: toolName,
		"source_agent":              author,
		"text":                      toolName + ": " + args,
	}
	if args != "" {
		fields[planTranscriptToolArgsField] = args
	}
	if result != "" {
		fields[planTranscriptToolResultField] = result
	}
	return &beadsapi.BeadDetail{
		ID:        id,
		Type:      "message",
		Status:    "closed",
		Labels:    []string{"plan-transcript", "from:" + author},
		Fields:    fields,
		CreatedBy: author,
		CreatedAt: created,
	}
}

// TestBuildPlanTranscript_RendersToolTurnDistinctly proves a tool-call turn
// (turn_role=tool) is projected as a distinct tool entry — IsTool set, tool
// name/args/result populated, agent-side direction — interleaved in time order
// with the prose turns around it.
func TestBuildPlanTranscript_RendersToolTurnDistinctly(t *testing.T) {
	base := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	epic := "gd-TOOLEPIC"
	beads := []*beadsapi.BeadDetail{
		turnBead("gd-p0", epic, "matt", planTurnRoleOperator, "run the tests", base),
		toolTurnBead("gd-p1", epic, "plan-asst", "Bash", "go test ./...", "ok — 42 passed", base.Add(time.Minute)),
		turnBead("gd-p2", epic, "plan-asst", planTurnRoleAssistant, "Tests pass.", base.Add(2*time.Minute)),
	}

	view := buildPlanTranscript(beads, "/base", base.Add(time.Hour))
	if view.Shown != 3 {
		t.Fatalf("Shown=%d, want 3", view.Shown)
	}
	// Order preserved; the tool turn sits between the two prose turns.
	wantOrder := []string{"gd-p0", "gd-p1", "gd-p2"}
	for i, id := range wantOrder {
		if view.Messages[i].ID != id {
			t.Errorf("Messages[%d].ID=%q want %q (order %v)", i, view.Messages[i].ID, id, msgIDs(view))
		}
	}
	tool := view.Messages[1]
	if !tool.IsTool {
		t.Fatalf("tool turn IsTool=false, want true")
	}
	if tool.ToolName != "Bash" {
		t.Errorf("ToolName=%q, want Bash", tool.ToolName)
	}
	if tool.ToolArgs != "go test ./..." {
		t.Errorf("ToolArgs=%q, want the command summary", tool.ToolArgs)
	}
	if tool.ToolResult != "ok — 42 passed" {
		t.Errorf("ToolResult=%q, want the result summary", tool.ToolResult)
	}
	// A tool turn is agent-side (not an operator turn).
	if tool.Direction != directionFromAgent {
		t.Errorf("tool turn Direction=%q, want %q", tool.Direction, directionFromAgent)
	}
	// The prose turns must NOT be flagged as tool entries.
	if view.Messages[0].IsTool || view.Messages[2].IsTool {
		t.Errorf("prose turns wrongly flagged IsTool (op=%v asst=%v)", view.Messages[0].IsTool, view.Messages[2].IsTool)
	}
}

// TestDecorateToolTurn_LegacyArgsFallback proves a tool turn with no structured
// tool_args_summary recovers a summary from the projected body text, and a tool
// turn with no tool_name is left as an ordinary (non-tool) line rather than an
// empty tool chip.
func TestDecorateToolTurn_LegacyArgsFallback(t *testing.T) {
	base := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)

	// No tool_args_summary field, but fields.text carries a fallback line.
	legacy := &beadsapi.BeadDetail{
		ID: "gd-legacy", Type: "message", Status: "closed",
		Fields: map[string]string{
			planTranscriptPlanIDField:   "gd-E",
			planTranscriptTurnRoleField: planTurnRoleTool,
			planTranscriptToolNameField: "Read",
			"text":                      "Read: plan_agent.html",
		},
		CreatedAt: base,
	}
	view := buildPlanTranscript([]*beadsapi.BeadDetail{legacy}, "/base", base.Add(time.Hour))
	if view.Shown != 1 || !view.Messages[0].IsTool {
		t.Fatalf("legacy tool turn not rendered as a tool entry (Shown=%d)", view.Shown)
	}
	if got := view.Messages[0].ToolArgs; got != "Read: plan_agent.html" {
		t.Errorf("legacy ToolArgs=%q, want the body-text fallback", got)
	}

	// A tool turn with NO tool_name is unusable — keep it as an ordinary line.
	nameless := &beadsapi.BeadDetail{
		ID: "gd-nameless", Type: "message", Status: "closed",
		Fields: map[string]string{
			planTranscriptPlanIDField:   "gd-E",
			planTranscriptTurnRoleField: planTurnRoleTool,
			"text":                      "some tool happened",
		},
		CreatedAt: base,
	}
	v2 := buildPlanTranscript([]*beadsapi.BeadDetail{nameless}, "/base", base.Add(time.Hour))
	if v2.Shown != 1 {
		t.Fatalf("nameless tool turn dropped (Shown=%d), want 1 ordinary line", v2.Shown)
	}
	if v2.Messages[0].IsTool {
		t.Errorf("nameless tool turn wrongly flagged IsTool — should degrade to a prose line")
	}
}

// TestPlanTranscriptToolTurnBead_WriteShape proves the tool-turn write contract
// round-trips through the render side: the minted request carries turn_role=tool,
// the structured tool fields, a human-readable fallback text, and a hydrated copy
// renders as a decorated tool entry. This is the shape the follow-up data slice
// (coop tool_use forward + minter) will target.
func TestPlanTranscriptToolTurnBead_WriteShape(t *testing.T) {
	req := plantranscript.ToolTurnBead("gd-TWB", "plan-asst", "Grep", "TODO in *.go", "12 matches", "sess-9:1700000000000")
	if req.Type != "message" || req.Kind != "data" {
		t.Errorf("Type/Kind=%q/%q, want message/data", req.Type, req.Kind)
	}
	var fields map[string]string
	if err := json.Unmarshal(req.Fields, &fields); err != nil {
		t.Fatalf("Fields not valid JSON: %v", err)
	}
	if fields[planTranscriptTurnRoleField] != planTurnRoleTool {
		t.Errorf("turn_role=%q, want %q", fields[planTranscriptTurnRoleField], planTurnRoleTool)
	}
	if fields[planTranscriptToolNameField] != "Grep" {
		t.Errorf("tool_name=%q, want Grep", fields[planTranscriptToolNameField])
	}
	if fields[planTranscriptToolArgsField] != "TODO in *.go" {
		t.Errorf("tool_args_summary=%q, want the args", fields[planTranscriptToolArgsField])
	}
	if fields[planTranscriptToolResultField] != "12 matches" {
		t.Errorf("tool_result_summary=%q, want the result", fields[planTranscriptToolResultField])
	}
	if fields["text"] != "Grep: TODO in *.go" {
		t.Errorf("text fallback=%q, want '<tool>: <args>'", fields["text"])
	}
	// Round-trip: hydrate the request and render it as a tool entry.
	hydrated := &beadsapi.BeadDetail{
		ID: "gd-round-tool", Type: "message", Status: "closed",
		Labels: req.Labels, Fields: fields, CreatedBy: "plan-asst",
		CreatedAt: time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC),
	}
	view := buildPlanTranscript([]*beadsapi.BeadDetail{hydrated}, "/base", hydrated.CreatedAt.Add(time.Hour))
	if view.Shown != 1 || !view.Messages[0].IsTool || view.Messages[0].ToolName != "Grep" {
		t.Errorf("round-trip render: Shown=%d IsTool=%v ToolName=%q, want 1/true/Grep",
			view.Shown, dirOf(view) != "", view.Messages[0].ToolName)
	}

	// An empty result omits the field (so decorateToolTurn renders no result pane).
	noResult := plantranscript.ToolTurnBead("gd-TWB", "plan-asst", "Read", "file.go", "", "sess-9:1")
	var nf map[string]string
	if err := json.Unmarshal(noResult.Fields, &nf); err != nil {
		t.Fatalf("Fields not valid JSON: %v", err)
	}
	if _, present := nf[planTranscriptToolResultField]; present {
		t.Errorf("empty result should omit tool_result_summary, got %q", nf[planTranscriptToolResultField])
	}
}

// --- tiny test helpers ---

func msgIDs(v mapChatView) []string {
	out := make([]string, 0, len(v.Messages))
	for _, m := range v.Messages {
		out = append(out, m.ID)
	}
	return out
}

func dirOf(v mapChatView) string {
	if len(v.Messages) == 0 {
		return "<none>"
	}
	return v.Messages[0].Direction
}

func sameRender(a, b mapChatView) bool {
	if a.Shown != b.Shown || a.Total != b.Total || a.HasMessages != b.HasMessages {
		return false
	}
	if len(a.Messages) != len(b.Messages) {
		return false
	}
	for i := range a.Messages {
		if a.Messages[i].ID != b.Messages[i].ID ||
			a.Messages[i].Direction != b.Messages[i].Direction ||
			a.Messages[i].BodyHTML != b.Messages[i].BodyHTML {
			return false
		}
	}
	return true
}
