package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// enqueueAgentsIndexEvent is a small test helper for the fleet-wide SSE
// fixtures below. Marshals a bead lifecycle event in the same wrapper shape
// kbeads publishes (`{"bead":{...}}`).
func enqueueAgentsIndexEvent(t *testing.T, ch chan<- beadsapi.SSEEvent, topic, id, btype, project string, fields map[string]any) {
	t.Helper()
	enqueueAgentsIndexEventStatus(t, ch, topic, id, btype, project, "in_progress", fields)
}

// enqueueAgentsIndexEventStatus is enqueueAgentsIndexEvent with an explicit
// bead status, so closed-card fixtures can assert the terminal render.
func enqueueAgentsIndexEventStatus(t *testing.T, ch chan<- beadsapi.SSEEvent, topic, id, btype, project, status string, fields map[string]any) {
	t.Helper()
	bead := map[string]any{
		"id":     id,
		"type":   btype,
		"title":  id,
		"status": status,
	}
	if project != "" {
		bead["labels"] = []string{"project:" + project}
	}
	if fields != nil {
		bead["fields"] = fields
	}
	enqueueAgentsIndexBead(t, ch, topic, bead)
}

// enqueueAgentsIndexBead marshals an arbitrary bead map into the daemon's
// `{"bead":{...}}` wrapper and enqueues it. Use this when a fixture needs to
// set top-level bead fields the daemon publishes but the helpers above don't
// expose (created_at, updated_at, description, owner, operated_by*) — the exact
// fields the SSE row builder must preserve (ka-0000000482).
func enqueueAgentsIndexBead(t *testing.T, ch chan<- beadsapi.SSEEvent, topic string, bead map[string]any) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"bead": bead})
	if err != nil {
		t.Fatalf("marshal SSE event: %v", err)
	}
	ch <- beadsapi.SSEEvent{Event: topic, Data: payload}
}

// drainAgentsEventsStream runs an SSE GET to completion (the daemon channel
// closes after the test enqueues + closes its ch), captures the body, and
// returns the rendered stream text for keyword assertions.
func drainAgentsEventsStream(t *testing.T, mux http.Handler, path string) string {
	t.Helper()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path, nil)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// TestHandleAgentsEvents_FiltersToAgentType feeds the daemon stream both an
// agent event and a task event; only the agent event should be forwarded.
func TestHandleAgentsEvents_FiltersToAgentType(t *testing.T) {
	f := newFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-ag1", "agent", "gardenio", map[string]any{
		"agent_state": "working",
		"role":        "crew",
	})
	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-task-1", "task", "gardenio", nil)
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/agents/events")
	if !strings.Contains(got, "gd-ag1") {
		t.Errorf("expected agent ID in stream, got: %s", got)
	}
	if strings.Contains(got, "gd-task-1") {
		t.Errorf("filter leak: task bead surfaced in /agents/events: %s", got)
	}
	if !strings.Contains(got, "event: updated") {
		t.Errorf("expected `event: updated` in stream, got: %s", got)
	}
	if !strings.Contains(got, `data-bead-id="gd-ag1"`) {
		t.Errorf("expected rendered row with data-bead-id, got: %s", got)
	}
	if !strings.Contains(got, `id="agent-row-gd-ag1"`) {
		t.Errorf("expected id=agent-row-<bead> on rendered row, got: %s", got)
	}
	if f.eventStreamCalls != 1 {
		t.Errorf("expected 1 EventStream call, got %d", f.eventStreamCalls)
	}
	if f.eventStreamLastTopics != "beads.bead.>" {
		t.Errorf("EventStream topics = %q, want beads.bead.>", f.eventStreamLastTopics)
	}
}

// TestHandleAgentsEvents_ProjectFilter narrows the stream to ?project=<name>;
// agent beads outside the project must not surface.
func TestHandleAgentsEvents_ProjectFilter(t *testing.T) {
	f := newFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-keep", "agent", "gardenio", nil)
	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-skip", "agent", "other", nil)
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/agents/events?project=gardenio")
	if !strings.Contains(got, "gd-keep") {
		t.Errorf("expected in-project agent in stream, got: %s", got)
	}
	if strings.Contains(got, "gd-skip") {
		t.Errorf("project filter leak: out-of-project agent surfaced: %s", got)
	}
}

// TestHandleAgentsEvents_ScheduleIDFilter narrows the stream to
// ?schedule_id=<gardener trigger bead ID>; only agent beads stamped with the
// matching schedule_id surface. Manual agents (no schedule_id) must NOT stream
// into a gardener-scoped subscription (design doc ka-0000003219 §8).
func TestHandleAgentsEvents_ScheduleIDFilter(t *testing.T) {
	f := newFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	// Iteration spawned by gardener gd-cap1 — should stream.
	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-iter", "agent", "gardenio", map[string]any{
		"schedule_id": "gd-cap1",
	})
	// Iteration of a different gardener — should not stream.
	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-other-cap", "agent", "gardenio", map[string]any{
		"schedule_id": "gd-cap2",
	})
	// Manual agent with no schedule_id — must not stream into a gardener thread.
	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-manual", "agent", "gardenio", map[string]any{
		"agent_state": "working",
	})
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/agents/events?schedule_id=gd-cap1")
	if !strings.Contains(got, "gd-iter") {
		t.Errorf("expected gardener-scoped iteration in stream, got: %s", got)
	}
	if strings.Contains(got, "gd-other-cap") {
		t.Errorf("schedule_id filter leak: other gardener's iteration surfaced: %s", got)
	}
	if strings.Contains(got, "gd-manual") {
		t.Errorf("schedule_id filter leak: manual agent (no schedule_id) surfaced: %s", got)
	}
}

// TestHandleAgentsEvents_GardenerScopeMatchesTriggerID is the core R5 fix: a
// gardener-scoped subscription must match iterations stamped with the gardener id
// under trigger_id (the modern bridge spawn path), not only schedule_id (the
// legacy scheduler path). Before the dual-key filter, every trigger-path
// gardener streamed zero live events even though its iterations exist.
func TestHandleAgentsEvents_GardenerScopeMatchesTriggerID(t *testing.T) {
	f := newFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	// Trigger-path iteration: id stamped under trigger_id, NO schedule_id.
	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-trig", "agent", "gardenio", map[string]any{
		"trigger_id": "gd-cap1",
	})
	// Legacy iteration of the same gardener under schedule_id — still matches.
	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-sched", "agent", "gardenio", map[string]any{
		"schedule_id": "gd-cap1",
	})
	// Another gardener's trigger-path iteration — must not surface.
	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-elsewhere", "agent", "gardenio", map[string]any{
		"trigger_id": "gd-cap2",
	})
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/agents/events?schedule_id=gd-cap1")
	if !strings.Contains(got, "gd-trig") {
		t.Errorf("trigger_id-stamped iteration missing from gardener stream: %s", got)
	}
	if !strings.Contains(got, "gd-sched") {
		t.Errorf("schedule_id-stamped iteration missing from gardener stream: %s", got)
	}
	if strings.Contains(got, "gd-elsewhere") {
		t.Errorf("dual-key filter leak: other gardener's trigger_id iteration surfaced: %s", got)
	}
}

// TestHandleAgentsEvents_GardenerScopeRendersLiCard verifies a gardener-scoped
// stream emits an <li> life-thread card (NOT the <tr> agents-table row) so the
// client can swap it into <ul class="gardener-thread">. A live (non-closed)
// iteration carries the "In progress" ribbon and the gardener-iter-live class,
// and suppresses the uptime/MR-final chips that only exist post-close.
func TestHandleAgentsEvents_GardenerScopeRendersLiCard(t *testing.T) {
	f := newFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-live", "agent", "gardenio", map[string]any{
		"trigger_id":  "gd-cap1",
		"agent_state": "working",
		"role":        "crew",
	})
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/agents/events?schedule_id=gd-cap1")
	if !strings.Contains(got, "<li") {
		t.Errorf("gardener-scoped stream must emit an <li> card, got: %s", got)
	}
	if strings.Contains(got, "<tr") {
		t.Errorf("gardener-scoped stream must NOT emit a <tr> agents-table row, got: %s", got)
	}
	if !strings.Contains(got, `data-bead-id="gd-live"`) {
		t.Errorf("gardener card missing data-bead-id swap key, got: %s", got)
	}
	if !strings.Contains(got, "gardener-iter-live") {
		t.Errorf("live iteration missing gardener-iter-live class, got: %s", got)
	}
	if !strings.Contains(got, "In progress") {
		t.Errorf("live iteration missing 'In progress' ribbon, got: %s", got)
	}
	// A live row must not advertise an uptime chip (uptime is post-close final).
	if strings.Contains(got, "agent-uptime") {
		t.Errorf("live iteration leaked an uptime chip (should be live-only), got: %s", got)
	}
}

// TestHandleAgentsEvents_GardenerScopeClosedCard verifies a closed event for a
// gardener-scoped stream renders the terminal (historical) card: the
// bead-row-terminated class and a "Completed" ribbon, NOT the live styling.
func TestHandleAgentsEvents_GardenerScopeClosedCard(t *testing.T) {
	f := newFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	enqueueAgentsIndexEventStatus(t, ch, "beads.bead.closed", "gd-done", "agent", "gardenio", "closed", map[string]any{
		"trigger_id":  "gd-cap1",
		"agent_state": "done",
	})
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/agents/events?schedule_id=gd-cap1")
	if !strings.Contains(got, "<li") {
		t.Errorf("gardener-scoped closed event must emit an <li> card, got: %s", got)
	}
	if !strings.Contains(got, "bead-row-terminated") {
		t.Errorf("closed gardener card missing bead-row-terminated class, got: %s", got)
	}
	if !strings.Contains(got, "Completed") {
		t.Errorf("closed gardener card missing 'Completed' ribbon, got: %s", got)
	}
	if strings.Contains(got, "gardener-iter-live") {
		t.Errorf("closed gardener card must not carry live styling, got: %s", got)
	}
}

// TestHandleAgentsEvents_FleetScopeStillRendersTr guards against a regression:
// the UNSCOPED /agents index stream must keep emitting <tr> table rows, not the
// gardener <li> card — the row-shape branch is keyed on the gardener scope only.
func TestHandleAgentsEvents_FleetScopeStillRendersTr(t *testing.T) {
	f := newFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-fleet", "agent", "gardenio", map[string]any{
		"agent_state": "working",
	})
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/agents/events")
	if !strings.Contains(got, `id="agent-row-gd-fleet"`) {
		t.Errorf("fleet stream must emit the <tr> agents-table row, got: %s", got)
	}
	if strings.Contains(got, "<li") {
		t.Errorf("fleet stream must NOT emit a gardener <li> card, got: %s", got)
	}
}

// TestHandleAgentsEvents_ClosedTransition verifies a closed event produces a
// row tagged with the closed kind so the client can transition it from the
// active table to the recent table.
func TestHandleAgentsEvents_ClosedTransition(t *testing.T) {
	f := newFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	enqueueAgentsIndexEvent(t, ch, "beads.bead.closed", "gd-bye", "agent", "gardenio", map[string]any{
		"agent_state": "done",
		"role":        "crew",
	})
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/agents/events")
	if !strings.Contains(got, "event: closed") {
		t.Errorf("expected `event: closed` in stream, got: %s", got)
	}
	if !strings.Contains(got, "agents-row-closed") {
		t.Errorf("expected row class agents-row-closed in stream, got: %s", got)
	}
	if !strings.Contains(got, `data-bead-kind="closed"`) {
		t.Errorf("expected data-bead-kind=closed marker, got: %s", got)
	}
	if !strings.Contains(got, `id="agent-row-gd-bye"`) {
		t.Errorf("expected id=agent-row-<bead> on closed row, got: %s", got)
	}
}

// TestHandleAgentsEvents_IgnoresUnknownTopic skips daemon events whose topic
// doesn't map to a known lifecycle kind.
func TestHandleAgentsEvents_IgnoresUnknownTopic(t *testing.T) {
	f := newFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	enqueueAgentsIndexEvent(t, ch, "beads.bead.something_else", "gd-ag1", "agent", "gardenio", nil)
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/agents/events")
	if strings.Contains(got, "gd-ag1") {
		t.Errorf("expected unknown-topic event to be skipped, got: %s", got)
	}
}

// TestRenderAgentsIndexRow_Shape sanity-checks the streamed row HTML so the SSE
// writer's `data:` line carries every expected hook the client JS relies on
// (id, data-bead-id, data-bead-kind, project) plus the load-bearing cell
// content. Exercised through the full server render (mkAgentServer) because the
// row now renders through the shared agents_row partial + agentRowFromBeadCtx
// rather than a hand-rolled string builder (ka-0000007652).
func TestRenderAgentsIndexRow_Shape(t *testing.T) {
	f := newFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-x", "agent", "gardenio", map[string]any{
		"agent":       "agent-x",
		"role":        "crew",
		"agent_state": "working",
		"pod_phase":   "Running",
		"task_id":     "gd-task-1",
		"task_title":  "do the thing",
	})
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/agents/events")
	for _, want := range []string{
		`agents-row-updated`,
		`id="agent-row-gd-x"`,
		`data-bead-id="gd-x"`,
		`data-bead-kind="updated"`,
		`data-project="gardenio"`,
		`agent-state-working`,
		`do the thing`,
		`/gd-x"`,
		`/gd-task-1"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stream missing %q\ngot: %s", want, got)
		}
	}
	// Each SSE data: line must be single-line — no CR inside the row markup.
	for line := range strings.SplitSeq(got, "\n") {
		if strings.HasPrefix(line, "data: ") &&
			strings.Contains(strings.TrimPrefix(line, "data: "), "\r") {
			t.Errorf("row contained a carriage return — SSE data: must be single-line:\n%s", line)
		}
	}
}

// countTDInFirstRow counts the <td> cells in the first <tr> in htmlStr. A tiny
// purpose-built parser (no DOM dep) for the column-congruence tests: locate the
// first "<tr", count "<td" up to the closing "</tr>". Returns 0 when no row.
func countTDInFirstRow(htmlStr string) int {
	trStart := strings.Index(htmlStr, "<tr")
	if trStart < 0 {
		return 0
	}
	rest := htmlStr[trStart:]
	if end := strings.Index(rest, "</tr>"); end >= 0 {
		rest = rest[:end]
	}
	return strings.Count(rest, "<td")
}

// countTHInTable counts the <th> cells inside the <thead> of the table whose
// opening tag carries matchClass. Selecting by class lets the compact vs
// recent header be counted independently.
func countTHInTable(htmlStr, matchClass string) int {
	idx := strings.Index(htmlStr, matchClass)
	if idx < 0 {
		return 0
	}
	rest := htmlStr[idx:]
	hs := strings.Index(rest, "<thead")
	if hs < 0 {
		return 0
	}
	rest = rest[hs:]
	// Skip past the "<thead ...>" opening tag itself so it isn't counted as a
	// "<th" cell (off-by-one: "<thead" has the "<th" prefix).
	if gt := strings.Index(rest, ">"); gt >= 0 {
		rest = rest[gt+1:]
	}
	if end := strings.Index(rest, "</thead>"); end >= 0 {
		rest = rest[:end]
	}
	return strings.Count(rest, "<th")
}

// TestAgentsRow_ColumnCongruence_Compact is the core regression guard for the
// "agents view changes when it updates from events vs static load" bug
// (ka-0000007652). It renders the SAME bead via the static /agents compact
// table AND via the SSE /agents/events stream, then asserts the streamed <tr>
// has exactly as many <td> cells as the static table has <th> header cells.
// Before the shared-partial refactor the SSE row had 8 cells vs the static
// table's 11 — so a live swap visibly misaligned the columns.
func TestAgentsRow_ColumnCongruence_Compact(t *testing.T) {
	beadFields := map[string]any{
		"agent":       "agent-x",
		"role":        "crew",
		"agent_state": "working",
		"pod_phase":   "Running",
		"task_id":     "gd-task-1",
		"task_title":  "do the thing",
	}
	beadDetailFields := map[string]string{}
	for k, v := range beadFields {
		beadDetailFields[k] = v.(string)
	}

	// Static render: GET /agents?density=compact with one active agent.
	fStatic := newFake()
	fStatic.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID:     "gd-x",
			Type:   "agent",
			Title:  "agent-x",
			Status: "in_progress",
			Labels: []string{"project:gardenio"},
			Fields: beadDetailFields,
		}},
		Total: 1,
	}
	muxStatic := mkAgentServer(t, fStatic, &fakeScreenClient{})
	reqStatic := httptest.NewRequest(http.MethodGet, "/agents?density=compact", nil)
	wStatic := httptest.NewRecorder()
	muxStatic.ServeHTTP(wStatic, reqStatic)
	if wStatic.Code != http.StatusOK {
		t.Fatalf("static GET /agents?density=compact: expected 200, got %d", wStatic.Code)
	}
	staticBody := wStatic.Body.String()
	wantTH := countTHInTable(staticBody, `class="agents-table"`)
	if wantTH == 0 {
		t.Fatalf("could not find compact table header in static render:\n%s", staticBody)
	}

	// SSE render: same bead over /agents/events.
	fSSE := newFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	fSSE.eventStreamCh = ch
	muxSSE := mkAgentServer(t, fSSE, &fakeScreenClient{})
	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-x", "agent", "gardenio", beadFields)
	close(ch)
	stream := drainAgentsEventsStream(t, muxSSE, "/agents/events")
	gotTD := countTDInFirstRow(stream)

	if gotTD != wantTH {
		t.Errorf("compact column drift: static table has %d <th>, SSE row has %d <td>\nstatic:\n%s\nstream:\n%s",
			wantTH, gotTD, staticBody, stream)
	}
}

// TestAgentsRow_ColumnCongruence_Recent guards the most user-visible facet of
// Matt's report: a CLOSED agent event is moved by the client into the 6-column
// Recent table, so the streamed closed row MUST be the 6-column recent shape —
// not the active-row shape, which column-shifted and overflowed the recent
// table before this fix.
func TestAgentsRow_ColumnCongruence_Recent(t *testing.T) {
	// Static render with one recent (closed) agent so the recent table renders.
	fStatic := newFake()
	fStatic.listResp = &beadsapi.ListBeadsResult{}
	fStatic.agentRecentResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID:     "gd-done",
			Type:   "agent",
			Title:  "agent-done",
			Status: "closed",
			Labels: []string{"project:gardenio"},
			Fields: map[string]string{"role": "crew", "agent_state": "done"},
			// Recent listing filters closed agents to a time window; stamp a
			// just-now updated_at so the recent TABLE renders.
			UpdatedAt: time.Now().Add(-time.Minute),
		}},
		Total: 1,
	}
	muxStatic := mkAgentServer(t, fStatic, &fakeScreenClient{})
	reqStatic := httptest.NewRequest(http.MethodGet, "/agents?density=compact", nil)
	wStatic := httptest.NewRecorder()
	muxStatic.ServeHTTP(wStatic, reqStatic)
	staticBody := wStatic.Body.String()
	wantTH := countTHInTable(staticBody, `class="agents-table agents-table-recent"`)
	if wantTH == 0 {
		t.Fatalf("could not find recent table header in static render:\n%s", staticBody)
	}

	// SSE closed event for the same bead.
	fSSE := newFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	fSSE.eventStreamCh = ch
	muxSSE := mkAgentServer(t, fSSE, &fakeScreenClient{})
	enqueueAgentsIndexEventStatus(t, ch, "beads.bead.closed", "gd-done", "agent", "gardenio", "closed", map[string]any{
		"role":        "crew",
		"agent_state": "done",
	})
	close(ch)
	stream := drainAgentsEventsStream(t, muxSSE, "/agents/events")
	gotTD := countTDInFirstRow(stream)

	if gotTD != wantTH {
		t.Errorf("recent column drift: recent table has %d <th>, closed SSE row has %d <td>\nstatic:\n%s\nstream:\n%s",
			wantTH, gotTD, staticBody, stream)
	}
	// The closed row must carry the terminated class so the client styles it as
	// a historical row, and must not be the live active-row shape.
	if !strings.Contains(stream, "bead-row-terminated") {
		t.Errorf("closed SSE row missing bead-row-terminated class:\n%s", stream)
	}
}

// TestAgentsRow_DegradedCellsArePresent locks in graceful degradation: a bead
// whose SSE payload lacks created_at / updated_at / operated_by still renders
// EVERY cell (Owner placeholder, Active cell) so the column count and alignment
// survive when the data is absent. Without this, an absent value dropping a
// <td> would re-introduce the misalignment.
func TestAgentsRow_DegradedCellsArePresent(t *testing.T) {
	f := newFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	// Minimal payload: no operated_by, no created_at/updated_at on the bead. The
	// SSE payload CAN carry them (ka-0000000482), but a bead that genuinely lacks
	// them must still render every cell rather than dropping a <td>.
	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-min", "agent", "gardenio", map[string]any{
		"agent_state": "working",
	})
	close(ch)
	stream := drainAgentsEventsStream(t, mux, "/agents/events")

	// Owner cell degrades to the no-owner placeholder, not a dropped cell.
	if !strings.Contains(stream, "owner-initials-empty") {
		t.Errorf("degraded row missing the no-owner placeholder cell:\n%s", stream)
	}
	// Active cell still renders ("active just now" when no updated_at).
	if !strings.Contains(stream, "active just now") {
		t.Errorf("degraded row missing the active placeholder cell:\n%s", stream)
	}
	// The full compact column count must still hold (12 cells with the project
	// column present — no ?project= filter on the fleet stream — including the
	// dedicated Context column added in ka-0000006399).
	if got := countTDInFirstRow(stream); got != 12 {
		t.Errorf("degraded row has %d <td>, want 12 (full compact shape):\n%s", got, stream)
	}
}

// TestAgentsRow_ProjectFilterColumnParity guards the project-scoped variant:
// when the stream is ?project=<name>-scoped the static compact table drops the
// Project column (one fewer <th>), and the SSE row must drop it too so the two
// stay congruent under a project filter (ka-0000007652).
func TestAgentsRow_ProjectFilterColumnParity(t *testing.T) {
	beadFields := map[string]any{"role": "crew", "agent_state": "working"}

	fStatic := newFake()
	fStatic.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID: "gd-x", Type: "agent", Title: "agent-x", Status: "in_progress",
			Labels: []string{"project:gardenio"},
			Fields: map[string]string{"role": "crew", "agent_state": "working"},
		}},
		Total: 1,
	}
	muxStatic := mkAgentServer(t, fStatic, &fakeScreenClient{})
	reqStatic := httptest.NewRequest(http.MethodGet, "/agents?density=compact&project=gardenio", nil)
	wStatic := httptest.NewRecorder()
	muxStatic.ServeHTTP(wStatic, reqStatic)
	staticBody := wStatic.Body.String()
	wantTH := countTHInTable(staticBody, `class="agents-table"`)
	if wantTH == 0 {
		t.Fatalf("could not find compact table header in project-filtered static render:\n%s", staticBody)
	}

	fSSE := newFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	fSSE.eventStreamCh = ch
	muxSSE := mkAgentServer(t, fSSE, &fakeScreenClient{})
	enqueueAgentsIndexEvent(t, ch, "beads.bead.updated", "gd-x", "agent", "gardenio", beadFields)
	close(ch)
	stream := drainAgentsEventsStream(t, muxSSE, "/agents/events?project=gardenio")
	gotTD := countTDInFirstRow(stream)

	if gotTD != wantTH {
		t.Errorf("project-filtered column drift: static %d <th>, SSE %d <td>\nstatic:\n%s\nstream:\n%s",
			wantTH, gotTD, staticBody, stream)
	}
}

// TestAgentEventProject prefers the project:<name> label, falls back to the
// project field, and finally to "untagged".
func TestAgentEventProject(t *testing.T) {
	cases := []struct {
		name   string
		bead   *agentEventBead
		fields map[string]string
		want   string
	}{
		{
			name: "label wins",
			bead: &agentEventBead{Labels: []string{"project:gardenio", "scope:test"}},
			want: "gardenio",
		},
		{
			name:   "field fallback",
			bead:   &agentEventBead{},
			fields: map[string]string{"project": "kbeads"},
			want:   "kbeads",
		},
		{
			name: "untagged",
			bead: &agentEventBead{},
			want: "untagged",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agentEventProject(c.bead, c.fields); got != c.want {
				t.Errorf("agentEventProject = %q, want %q", got, c.want)
			}
		})
	}
}

// TestHandleAgentsEventsStatus verifies the same-origin auth probe used by the
// SSE clients before opening EventSource. Reaching the handler means the
// upstream auth gate accepted the request, so the handler unconditionally
// returns 200 with a JSON body and no-store caching. Unauth requests never
// reach the handler — AVA/OIDC middleware returns 303/401 upstream and the
// client probe treats those as auth-expired.
func TestHandleAgentsEventsStatus(t *testing.T) {
	f := newFake()
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	req := httptest.NewRequest(http.MethodGet, "/agents/events-status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	body := strings.TrimSpace(w.Body.String())
	if body != `{"status":"ok"}` {
		t.Errorf("body = %q, want {\"status\":\"ok\"}", body)
	}
	// The probe must NOT open an SSE stream — that's the whole point of the
	// endpoint (Firefox would flag the SSE as "Blocked" on auth failure).
	if f.eventStreamCalls != 0 {
		t.Errorf("expected 0 EventStream calls, got %d", f.eventStreamCalls)
	}
}

// TestBeadDetailFromEvent_PreservesFields is the unit-level regression guard for
// ka-0000000482: the projector must carry EVERY field the daemon publishes onto
// the BeadDetail the row builder consumes. Before the fix, agentEventBead was a
// 9-field subset and these fields were silently dropped on unmarshal, blanking
// the streamed row's uptime/active-ago/description/owner/operator cells.
func TestBeadDetailFromEvent_PreservesFields(t *testing.T) {
	created := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 7, 23, 11, 30, 0, 0, time.UTC)
	opID := int64(42)
	eb := &agentEventBead{
		ID:                "gd-full",
		Type:              "agent",
		Title:             "agent-full",
		Status:            "in_progress",
		Assignee:          "gardenio-crew",
		Owner:             "matt",
		CreatedBy:         "gardener-x",
		ClosedBy:          "reaper",
		Labels:            []string{"project:gardenio"},
		Priority:          2,
		Description:       "doing the thing",
		Notes:             "pod_name=agent-full-pod",
		OperatedBy:        "slack:U07",
		OperatedByDisplay: "Matt Baker",
		OperatedByEmail:   "matt@acme.example",
		OperatedByID:      &opID,
		ClosedAt:          "2026-07-23T12:00:00Z",
		CreatedAt:         created,
		UpdatedAt:         updated,
	}
	fields := map[string]string{"role": "crew", "agent_state": "working"}

	got := beadDetailFromEvent(eb, fields)

	// OperatedByID must ride through so populateOperatedBySlug can resolve the
	// /u/<slug> link on the streamed node (ka-0000007344). Compare pointees.
	if got.OperatedByID == nil || *got.OperatedByID != opID {
		t.Errorf("OperatedByID = %v, want %d", got.OperatedByID, opID)
	}

	checks := []struct {
		name      string
		got, want any
	}{
		{"ID", got.ID, "gd-full"},
		{"Type", got.Type, "agent"},
		{"Title", got.Title, "agent-full"},
		{"Status", got.Status, "in_progress"},
		{"Assignee", got.Assignee, "gardenio-crew"},
		{"Owner", got.Owner, "matt"},
		{"CreatedBy", got.CreatedBy, "gardener-x"},
		{"ClosedBy", got.ClosedBy, "reaper"},
		{"Priority", got.Priority, 2},
		{"Description", got.Description, "doing the thing"},
		{"Notes", got.Notes, "pod_name=agent-full-pod"},
		{"OperatedBy", got.OperatedBy, "slack:U07"},
		{"OperatedByDisplay", got.OperatedByDisplay, "Matt Baker"},
		{"OperatedByEmail", got.OperatedByEmail, "matt@acme.example"},
		{"ClosedAt", got.ClosedAt, "2026-07-23T12:00:00Z"},
		{"CreatedAt", got.CreatedAt, created},
		{"UpdatedAt", got.UpdatedAt, updated},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if len(got.Labels) != 1 || got.Labels[0] != "project:gardenio" {
		t.Errorf("Labels = %v, want [project:gardenio]", got.Labels)
	}
	if got.Fields["role"] != "crew" || got.Fields["agent_state"] != "working" {
		t.Errorf("Fields = %v, want role=crew agent_state=working", got.Fields)
	}
}

// beadDetailFromEvent must never drop the fields agentEventBead's JSON tags
// declare — this test decodes a full daemon-shaped payload the same way the SSE
// handler does and asserts the projected BeadDetail is fully populated. Without
// the widened struct, json.Unmarshal silently discarded created_at/updated_at/
// description/owner/operated_by* (ka-0000000482).
func TestAgentEventBead_UnmarshalCarriesFullPayload(t *testing.T) {
	// The exact wire shape the daemon marshals for a model.Bead lifecycle event.
	payload := []byte(`{"bead":{
		"id":"gd-wire","type":"agent","title":"agent-wire","status":"in_progress",
		"assignee":"gardenio-crew","owner":"matt","created_by":"gardener-x",
		"labels":["project:gardenio"],"priority":2,"description":"streamed desc",
		"notes":"pod_name=p1","operated_by":"slack:U07","operated_by_display":"Matt Baker",
		"operated_by_email":"matt@acme.example","operated_by_id":42,"created_at":"2026-07-23T10:00:00Z",
		"updated_at":"2026-07-23T11:30:00Z","fields":{"role":"crew"}
	}}`)

	var wrapper agentEventSSE
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	eb := wrapper.Bead
	if eb == nil {
		t.Fatal("wrapper.Bead is nil")
	}
	if eb.Description != "streamed desc" {
		t.Errorf("Description = %q, want %q (dropped on unmarshal?)", eb.Description, "streamed desc")
	}
	if eb.OperatedByDisplay != "Matt Baker" {
		t.Errorf("OperatedByDisplay = %q, want %q", eb.OperatedByDisplay, "Matt Baker")
	}
	// operated_by_id must survive the unmarshal so the streamed row can resolve
	// the operator /u/<slug> link (ka-0000007344).
	if eb.OperatedByID == nil || *eb.OperatedByID != 42 {
		t.Errorf("OperatedByID = %v, want 42 (dropped on unmarshal?)", eb.OperatedByID)
	}
	if eb.Owner != "matt" {
		t.Errorf("Owner = %q, want matt", eb.Owner)
	}
	if eb.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero — created_at dropped on unmarshal")
	}
	if eb.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero — updated_at dropped on unmarshal")
	}
}

// TestParseAgentEventFields_NumericFieldsSurvive is the regression guard for
// ka-0000000324: the /agents SSE field projection must carry NUMERIC field
// values (live_turns, total_request_count — both schema type=integer, written as
// native JSON ints by reportLiveUsage / the session-close reconciler) through to
// the row builder, not drop them.
//
// The pre-fix parseAgentEventFields did a naive json.Unmarshal(raw,
// &map[string]string) and swallowed the error. json's number→string decode
// returns an error the instant a value is a JSON number, and the partial decode
// leaves every numeric-valued key at its zero value "" — so live_turns and
// total_request_count streamed BLANK (their string siblings survive, which is
// why the drop went unnoticed until turns rendering arrived). This is the SSE
// field-drop class the top-level-field siblings ka-0000003512 / ka-0000000482
// fixed for created_at/owner/operator*; this pins the same guard for the numeric
// fields-map values by delegating to the canonical beadsapi.ParseFieldsJSON.
func TestParseAgentEventFields_NumericFieldsSurvive(t *testing.T) {
	// The exact shape the daemon marshals: string fields alongside native-int
	// numeric fields. reportLiveUsage writes live_turns as a JSON integer, the
	// session-close reconciler writes total_request_count as one.
	raw := json.RawMessage(`{
		"role":"crew",
		"agent_state":"working",
		"live_turns":5,
		"total_request_count":10,
		"live_cost_usd":1.25
	}`)

	got := parseAgentEventFields(raw)

	// The numeric fields this task exists to carry must be present as their
	// string form — not dropped, not blank.
	if got["live_turns"] != "5" {
		t.Errorf("live_turns = %q, want %q (numeric field dropped on SSE projection?)", got["live_turns"], "5")
	}
	if got["total_request_count"] != "10" {
		t.Errorf("total_request_count = %q, want %q (numeric field dropped on SSE projection?)", got["total_request_count"], "10")
	}
	// A float numeric field re-marshals to its string form too.
	if got["live_cost_usd"] != "1.25" {
		t.Errorf("live_cost_usd = %q, want %q", got["live_cost_usd"], "1.25")
	}
	// The sibling STRING fields keep decoding correctly (they did even pre-fix —
	// which is exactly why the numeric drop hid until turns rendering landed).
	if got["role"] != "crew" {
		t.Errorf("role = %q, want %q", got["role"], "crew")
	}
	if got["agent_state"] != "working" {
		t.Errorf("agent_state = %q, want %q", got["agent_state"], "working")
	}
}

// TestParseAgentEventFields_StringFastPathAndEmpty pins the two unchanged
// behaviours the delegation must preserve: an all-string payload decodes on the
// fast path, and empty/nil input yields a non-nil empty map (so callers can
// index it without a nil check).
func TestParseAgentEventFields_StringFastPathAndEmpty(t *testing.T) {
	got := parseAgentEventFields(json.RawMessage(`{"role":"crew","project":"gardenio"}`))
	if got["role"] != "crew" || got["project"] != "gardenio" {
		t.Errorf("string fast path: got %v, want role=crew project=gardenio", got)
	}

	empty := parseAgentEventFields(nil)
	if empty == nil {
		t.Error("nil raw: got nil map, want non-nil empty map")
	}
	if len(empty) != 0 {
		t.Errorf("nil raw: got %v, want empty map", empty)
	}
}

// TestAgentsRow_SSEPreservesFields is the end-to-end regression guard for
// ka-0000000482: an SSE event carrying the full bead payload must render the
// uptime, active-ago, description-adjacent, owner, and operator-chip cells with
// their real values — not the blank/placeholder cells the pre-fix 9-field
// subset produced. Drives a real /agents/events stream (server → row builder →
// template) so it fails if any layer drops a field.
func TestAgentsRow_SSEPreservesFields(t *testing.T) {
	f := newFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	// created_at 5h ago (past the 4h long-running warn threshold) and updated_at
	// 5m ago so the uptime + active-ago cells carry non-placeholder values.
	now := time.Now().UTC()
	enqueueAgentsIndexBead(t, ch, "beads.bead.updated", map[string]any{
		"id":                  "gd-live",
		"type":                "agent",
		"title":               "agent-live",
		"status":              "in_progress",
		"labels":              []string{"project:gardenio"},
		"owner":               "matt",
		"operated_by":         "slack:U07",
		"operated_by_display": "Matt Baker",
		"created_at":          now.Add(-5 * time.Hour).Format(time.RFC3339Nano),
		"updated_at":          now.Add(-5 * time.Minute).Format(time.RFC3339Nano),
		"fields":              map[string]any{"role": "crew", "agent_state": "working"},
	})
	close(ch)
	stream := drainAgentsEventsStream(t, mux, "/agents/events")

	// Uptime cell: created_at 5h ago → "up 5h" (was blank on the pre-fix path,
	// which dropped created_at).
	if !strings.Contains(stream, "up 5h") {
		t.Errorf("streamed row missing uptime from created_at (want \"up 5h\"):\n%s", stream)
	}
	// Active cell: updated_at 5m ago → "active 5m", NOT the "active just now"
	// placeholder the zero-UpdatedAt path renders.
	if !strings.Contains(stream, "active 5m") {
		t.Errorf("streamed row missing active-ago from updated_at (want \"active 5m\"):\n%s", stream)
	}
	if strings.Contains(stream, "active just now") {
		t.Errorf("streamed row fell back to the zero-UpdatedAt placeholder despite a real updated_at:\n%s", stream)
	}
	// Operator chip: operated_by_display present → the owner initials avatar and
	// the "op" chip render (was the no-owner placeholder before the fix).
	if strings.Contains(stream, "owner-initials-empty") {
		t.Errorf("streamed row rendered the no-owner placeholder despite operated_by_display:\n%s", stream)
	}
	if !strings.Contains(stream, "Matt Baker") {
		t.Errorf("streamed row missing operator display name:\n%s", stream)
	}
	// Long-running warn: created_at is past the uptime warn threshold.
	if !strings.Contains(stream, "agent-uptime-warn") {
		t.Errorf("streamed row missing long-running warn class despite 3h uptime:\n%s", stream)
	}
}

// TestAgentsRow_SSETurnsChipRenders is the Phase-3 exit-criterion guard: an SSE
// /agents refresh must render the 🔁 turns chip, not drop it. live_turns is a
// schema type=integer field (written as a native JSON number by
// reportLiveUsage), exactly the numeric-field-drop class that streamed usage
// cells blank on the pre-fix path (ka-0000000324; siblings ka-0000003512 /
// ka-0000000482). Enqueue it as a real number so the test exercises the numeric
// round-trip end to end — event fields → parseAgentEventFields → buildAgentUsageView
// → agent_usage_chips — and asserts the chip lands in the streamed HTML.
func TestAgentsRow_SSETurnsChipRenders(t *testing.T) {
	f := newFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	now := time.Now().UTC()
	enqueueAgentsIndexBead(t, ch, "beads.bead.updated", map[string]any{
		"id":         "gd-live-turns",
		"type":       "agent",
		"title":      "agent-turns",
		"status":     "in_progress",
		"labels":     []string{"project:gardenio"},
		"created_at": now.Add(-5 * time.Minute).Format(time.RFC3339Nano),
		"updated_at": now.Add(-1 * time.Minute).Format(time.RFC3339Nano),
		// live_turns as a native JSON integer — the shape reportLiveUsage writes
		// and the exact case a naive string-map unmarshal would drop.
		"fields": map[string]any{
			"role":        "crew",
			"agent_state": "working",
			"live_turns":  47,
		},
	})
	close(ch)
	stream := drainAgentsEventsStream(t, mux, "/agents/events")

	if !strings.Contains(stream, "🔁 47") {
		t.Errorf("streamed row missing 🔁 turns chip (live_turns dropped on SSE projection?):\n%s", stream)
	}
}

// TestAgentsRow_SSEClosedPreservesDescription guards the recent (closed) row
// shape: its Desc cell is fed by the bead description, which the pre-fix subset
// dropped — so a closed SSE row showed an empty description cell until the next
// eager reload (ka-0000000482).
func TestAgentsRow_SSEClosedPreservesDescription(t *testing.T) {
	f := newFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	enqueueAgentsIndexBead(t, ch, "beads.bead.closed", map[string]any{
		"id":                  "gd-closed",
		"type":                "agent",
		"title":               "agent-closed",
		"status":              "closed",
		"labels":              []string{"project:gardenio"},
		"operated_by_display": "Matt Baker",
		"description":         "final run summary text",
		"updated_at":          time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		"fields":              map[string]any{"role": "crew", "agent_state": "done"},
	})
	close(ch)
	stream := drainAgentsEventsStream(t, mux, "/agents/events")

	if !strings.Contains(stream, "bead-row-terminated") {
		t.Fatalf("expected a closed/recent row shape:\n%s", stream)
	}
	// The recent row's Desc cell falls back to the bead description when there's
	// no wrap-up summary — that description must survive the SSE round-trip.
	if !strings.Contains(stream, "final run summary text") {
		t.Errorf("closed streamed row dropped the bead description:\n%s", stream)
	}
	// Owner initials avatar renders (not the no-owner placeholder) since
	// operated_by_display came through.
	if strings.Contains(stream, "owner-initials-empty") {
		t.Errorf("closed streamed row rendered the no-owner placeholder despite operated_by_display:\n%s", stream)
	}
}

// TestAgentsRow_SSEResolvesOperatorSlug is the regression guard for
// ka-0000007344: an SSE event carrying operated_by_id must render the operator
// chip's /u/<slug> homepage link on the streamed node — not just the display
// name as plain text. Before this fix beadDetailFromEvent dropped OperatedByID
// (the daemon publishes it, but agentEventBead never parsed it), so
// populateOperatedBySlug early-returned on the nil FK and the chip streamed
// link-less until the next eager reload. Drives a real /agents/events stream
// (server → slug resolve → row builder → template) end-to-end.
func TestAgentsRow_SSEResolvesOperatorSlug(t *testing.T) {
	f := newFake()
	f.opSlugResp = map[int64]*beadsapi.GetOperatorSlugResponse{
		42: {Slug: "matt-baker", Found: true},
	}
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	enqueueAgentsIndexBead(t, ch, "beads.bead.updated", map[string]any{
		"id":                  "gd-live",
		"type":                "agent",
		"title":               "agent-live",
		"status":              "in_progress",
		"labels":              []string{"project:gardenio"},
		"operated_by":         "slack:U07",
		"operated_by_display": "Matt Baker",
		"operated_by_id":      42,
		"updated_at":          time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339Nano),
		"fields":              map[string]any{"role": "crew", "agent_state": "working"},
	})
	close(ch)
	stream := drainAgentsEventsStream(t, mux, "/agents/events")

	// The operator chip must carry its /u/<slug> deep-link on the streamed node
	// (was plain text before the fix — link only appeared on the next reload).
	if !strings.Contains(stream, `href="/u/matt-baker"`) {
		t.Errorf("streamed row missing operator /u/<slug> link despite operated_by_id:\n%s", stream)
	}
	if !strings.Contains(stream, "Matt Baker") {
		t.Errorf("streamed row missing operator display name:\n%s", stream)
	}
}

// TestGardenerStreamCard_SSEResolvesOperatorSlug is the gardener-scoped twin of
// TestAgentsRow_SSEResolvesOperatorSlug: the gardener life-thread card renders
// the same operator chip via userLink, so a schedule-scoped SSE event carrying
// operated_by_id must resolve the /u/<slug> link on the streamed card too
// (ka-0000007344).
func TestGardenerStreamCard_SSEResolvesOperatorSlug(t *testing.T) {
	f := newFake()
	f.opSlugResp = map[int64]*beadsapi.GetOperatorSlugResponse{
		42: {Slug: "matt-baker", Found: true},
	}
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	enqueueAgentsIndexBead(t, ch, "beads.bead.updated", map[string]any{
		"id":                  "gd-iter1",
		"type":                "agent",
		"title":               "agent-iter1",
		"status":              "in_progress",
		"labels":              []string{"project:gardenio"},
		"operated_by":         "slack:U07",
		"operated_by_display": "Matt Baker",
		"operated_by_id":      42,
		"updated_at":          time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339Nano),
		"fields":              map[string]any{"role": "crew", "agent_state": "working", "schedule_id": "gd-cap1"},
	})
	close(ch)
	stream := drainAgentsEventsStream(t, mux, "/agents/events?schedule_id=gd-cap1")

	if !strings.Contains(stream, `href="/u/matt-baker"`) {
		t.Errorf("streamed gardener card missing operator /u/<slug> link despite operated_by_id:\n%s", stream)
	}
	if !strings.Contains(stream, "Matt Baker") {
		t.Errorf("streamed gardener card missing operator display name:\n%s", stream)
	}
}
