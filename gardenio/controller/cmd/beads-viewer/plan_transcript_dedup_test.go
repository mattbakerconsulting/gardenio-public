package main

// plan_transcript_dedup_test.go — the render-side dedup + transcript-query
// invariants that used to live beside the AssistantTurnMinter's tests before it
// was rehomed to the controller (task ka-0000006891). These exercise the
// viewer's buildPlanTranscript projector and planTranscriptMessagesQuery, so
// they stay in the viewer; the minter's own logic moved to
// internal/assistantturnminter.

import (
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// turnBeadWithTurnID is turnBead plus an explicit fields.turn_id, for exercising
// the render-side turn_id dedup.
func turnBeadWithTurnID(id, epicID, author, role, text, turnID string, created time.Time) *beadsapi.BeadDetail {
	b := turnBead(id, epicID, author, role, text, created)
	b.Fields[planTranscriptTurnIDField] = turnID
	return b
}

// TestBuildPlanTranscript_DedupsOnTurnID proves the render-side belt-and-
// suspenders: two beads sharing a fields.turn_id (a transient double the mint-
// time idempotency key would normally prevent) collapse to ONE row, and the
// row with the LONGER (non-truncated) text wins.
func TestBuildPlanTranscript_DedupsOnTurnID(t *testing.T) {
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	turnID := "sess-9:1700000000000"
	clipped := turnBeadWithTurnID("gd-a", "gd-EPIC", "asst", planTurnRoleAssistant, "clipped", turnID, base)
	full := turnBeadWithTurnID("gd-b", "gd-EPIC", "asst", planTurnRoleAssistant, "the full untruncated body", turnID, base.Add(time.Second))

	view := buildPlanTranscript([]*beadsapi.BeadDetail{clipped, full}, "/base", base.Add(time.Hour))
	if view.Shown != 1 {
		t.Fatalf("two beads sharing turn_id must render ONE row; got Shown=%d", view.Shown)
	}
	if body := string(view.Messages[0].BodyHTML); !strings.Contains(body, "full untruncated body") {
		t.Errorf("dedup must keep the LONGER body; got %q", body)
	}
}

// TestBuildPlanTranscript_FallsBackToBeadIDWithoutTurnID proves beads WITHOUT a
// turn_id (legacy/spike shape) still dedup on bead.ID — unchanged behavior.
func TestBuildPlanTranscript_FallsBackToBeadIDWithoutTurnID(t *testing.T) {
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	a := turnBead("gd-a", "gd-EPIC", "asst", planTurnRoleAssistant, "one", base)
	b := turnBead("gd-b", "gd-EPIC", "asst", planTurnRoleAssistant, "two", base.Add(time.Minute))
	dupOfA := turnBead("gd-a", "gd-EPIC", "asst", planTurnRoleAssistant, "one", base)

	view := buildPlanTranscript([]*beadsapi.BeadDetail{a, b, dupOfA}, "/base", base.Add(time.Hour))
	if view.Shown != 2 {
		t.Errorf("distinct-id beads render once each (dup of gd-a collapses); got Shown=%d", view.Shown)
	}
}

// TestSquawkBeadExcludedFromTranscriptQuery is the load-bearing dedup INVARIANT:
// a squawk-shaped message bead carries NO fields.plan_id, so it can never match
// planTranscriptMessagesQuery and can never double-render against a server-minted
// turn. The query MUST be field-scoped on plan_id.
func TestSquawkBeadExcludedFromTranscriptQuery(t *testing.T) {
	q := planTranscriptMessagesQuery("gd-EPIC")
	if q.Fields[planTranscriptPlanIDField] != "gd-EPIC" {
		t.Fatalf("transcript query must scope on fields.plan_id; got %v", q.Fields)
	}
	if len(q.Fields) != 1 {
		t.Errorf("transcript query must filter ONLY on plan_id (a squawk with no plan_id is thus excluded); got %v", q.Fields)
	}
	if len(q.Labels) != 0 || len(q.LabelsAny) != 0 {
		t.Errorf("transcript query must not be label-scoped (that could accidentally pull squawks); labels=%v any=%v", q.Labels, q.LabelsAny)
	}
}
