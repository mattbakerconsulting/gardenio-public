package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// feed_phase3_test.go — covers the Phase 3 multi-family multiplexer
// (task ka-0000006994, epic ka-0000004907): squawk, mail, agent spawn/stop,
// gate transitions, MR open, and pipeline events — all classified off the same
// bead-lifecycle stream the prototype already consumes.

// TestFeedTypeSurfaced confirms the multiplexer re-admits the meta families it
// surfaces (decision, mail, agent) while keeping pure-noise meta types filtered
// and letting work + merge-request + message beads through.
func TestFeedTypeSurfaced(t *testing.T) {
	cases := []struct {
		typ  string
		want bool
	}{
		{"task", true},
		{"bug", true},
		{"merge-request", true},
		{"message", true}, // squawk beads
		{"decision", true},
		{"mail", true},
		{"agent", true},
		{"schedule", false},
		{"advice", false},
		{"rating", false},
		{"generic", false},
		{"config", false},
		// Feed-local noise types (task ka-0000000782): dropped by /feed even
		// though they are NOT in the shared /index filter.
		{"account", false},
		{"trigger", false},
		{"event", false},
		{"session", false},
		{"audit", false},
		{"report", false},
	}
	for _, c := range cases {
		if got := feedTypeSurfaced(c.typ); got != c.want {
			t.Errorf("feedTypeSurfaced(%q) = %v, want %v", c.typ, got, c.want)
		}
	}
}

// TestFeedKindForBead_Phase3Families exercises feedKindForBead across every new
// family + its noise cases.
func TestFeedKindForBead_Phase3Families(t *testing.T) {
	// fieldsChange is the daemon's changes map for an event that wrote the
	// fields blob: the daemon emits the whole merged blob under the "fields"
	// key, never a per-subfield key (see beads.go updateBead). The agent family
	// is now dropped regardless of changes (ka-0000003549), so these maps just
	// exercise that no agent variant produces a card.
	fieldsChange := map[string]any{"fields": map[string]any{}}
	// notesChange is a notes-only heartbeat update — the fields blob was NOT
	// written this event, so agent_state/gate_satisfied_by present on the
	// snapshot is stale, not a fresh transition.
	notesChange := map[string]any{"notes": "pod_phase: Running"}

	cases := []struct {
		name    string
		base    feedKind
		bead    *feedEventBead
		status  string
		changes map[string]any
		want    feedKind
	}{
		// Squawk: a created message bead with the squawk label.
		{"squawk on create", feedKindCreated,
			&feedEventBead{Type: "message", Labels: []string{"squawk", "from:agent-x"}}, "open", nil, feedKindSquawk},
		{"message without squawk label is dropped", feedKindCreated,
			&feedEventBead{Type: "message", Labels: []string{"channel:ops"}}, "open", nil, feedKind{}},
		{"squawk edit (update) is dropped", feedKindUpdated,
			&feedEventBead{Type: "message", Labels: []string{"squawk"}}, "open", nil, feedKind{}},

		// Mail.
		{"mail on create", feedKindCreated, &feedEventBead{Type: "mail"}, "open", nil, feedKindMail},
		{"mail update is dropped", feedKindUpdated, &feedEventBead{Type: "mail"}, "open", nil, feedKind{}},

		// Agent family — spawn / stop / gate / heartbeat — is now dropped
		// ENTIRELY (ka-0000003549, Matt 2026-06-17): agent-lifecycle gate churn is
		// pure orchestration noise, so no agent-bead event produces a card. Spawn
		// and stop were already dropped; gate transitions (on a fields write, on a
		// lingering notes-only update, or with nil changes) are now dropped too.
		{"agent spawn on create is dropped", feedKindCreated, &feedEventBead{Type: "agent"}, "open", nil, feedKind{}},
		{"agent stop on close is dropped", feedKindClosed, &feedEventBead{Type: "agent"}, "closed", nil, feedKind{}},
		{"agent stop via stopping state on fields write is dropped", feedKindUpdated,
			&feedEventBead{Type: "agent", Fields: json.RawMessage(`{"agent_state":"stopping"}`)}, "in_progress", fieldsChange, feedKind{}},
		{"agent gate transition on fields write is dropped", feedKindUpdated,
			&feedEventBead{Type: "agent", Fields: json.RawMessage(`{"agent_state":"working","gate_satisfied_by":"yield"}`)}, "in_progress", fieldsChange, feedKind{}},
		{"agent heartbeat update is dropped", feedKindUpdated,
			&feedEventBead{Type: "agent", Fields: json.RawMessage(`{"agent_state":"working"}`)}, "in_progress", fieldsChange, feedKind{}},
		{"agent gate value lingers on notes-only update is dropped", feedKindUpdated,
			&feedEventBead{Type: "agent", Fields: json.RawMessage(`{"agent_state":"working","gate_satisfied_by":"yield"}`)}, "in_progress", notesChange, feedKind{}},
		{"agent stopping lingers on notes-only update is dropped", feedKindUpdated,
			&feedEventBead{Type: "agent", Fields: json.RawMessage(`{"agent_state":"stopping"}`)}, "in_progress", notesChange, feedKind{}},
		{"agent gate with nil changes is dropped", feedKindUpdated,
			&feedEventBead{Type: "agent", Fields: json.RawMessage(`{"gate_satisfied_by":"yield"}`)}, "in_progress", nil, feedKind{}},

		// Merge-request: opened / merged / pipeline / churn.
		{"MR opened on create", feedKindCreated,
			&feedEventBead{Type: "merge-request", Fields: json.RawMessage(`{"mr_state":"opened"}`)}, "open", nil, feedKindMROpened},
		{"MR merged wins over pipeline", feedKindUpdated,
			&feedEventBead{Type: "merge-request", Fields: json.RawMessage(`{"mr_state":"merged","mr_pipeline_status":"success"}`)}, "open", nil, feedKindMRMerged},
		{"MR pipeline success", feedKindUpdated,
			&feedEventBead{Type: "merge-request", Fields: json.RawMessage(`{"mr_state":"opened","mr_pipeline_status":"success"}`)}, "open", nil, feedKindPipeline},
		{"MR pipeline failed", feedKindUpdated,
			&feedEventBead{Type: "merge-request", Fields: json.RawMessage(`{"mr_state":"opened","mr_pipeline_status":"failed"}`)}, "open", nil, feedKindPipeline},
		{"MR pipeline running is churn, dropped", feedKindUpdated,
			&feedEventBead{Type: "merge-request", Fields: json.RawMessage(`{"mr_state":"opened","mr_pipeline_status":"running"}`)}, "open", nil, feedKind{}},
		{"MR closed unmerged is a close", feedKindClosed,
			&feedEventBead{Type: "merge-request", Fields: json.RawMessage(`{"mr_state":"closed"}`)}, "closed", nil, feedKindClosed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feedKindForBead(c.base, c.bead, c.status, c.changes); got != c.want {
				t.Errorf("feedKindForBead = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestClassifyFeedEvent_Phase3 drives the full parse+classify path for the new
// families and confirms noise types/events are dropped (ok=false).
func TestClassifyFeedEvent_Phase3(t *testing.T) {
	// wrap builds an SSE wrapper payload. changesJSON, when non-empty, sets the
	// event's `changes` map so the full classify path exercises the agent
	// transition gate (ka-0000007057). An updated agent event with no `changes`
	// key falls back to field-presence (callers without the map).
	wrap := func(t string, fields, labels, changesJSON string) json.RawMessage {
		b := map[string]any{"id": "gd-x", "type": t}
		if fields != "" {
			b["fields"] = json.RawMessage(fields)
		}
		if labels != "" {
			var l []string
			_ = json.Unmarshal([]byte(labels), &l)
			b["labels"] = l
		}
		wrapper := map[string]any{"bead": b}
		if changesJSON != "" {
			wrapper["changes"] = json.RawMessage(changesJSON)
		}
		payload, _ := json.Marshal(wrapper)
		return payload
	}

	cases := []struct {
		name    string
		topic   string
		data    json.RawMessage
		wantOK  bool
		wantKnd string
	}{
		{"squawk created", "beads.bead.created", wrap("message", "", `["squawk"]`, ""), true, "squawk"},
		{"mail created", "beads.bead.created", wrap("mail", "", "", ""), true, "mail"},
		// The whole agent family is dropped (ka-0000003549): spawn, stop, gate (on
		// a fields write, lingering on a notes-only update, or with no changes
		// key), and heartbeat all classify as ok=false — no agent-bead card.
		{"agent spawn dropped", "beads.bead.created", wrap("agent", "", "", ""), false, ""},
		{"agent stop (close) dropped", "beads.bead.closed", wrap("agent", "", "", ""), false, ""},
		{"agent gate on fields write dropped", "beads.bead.updated",
			wrap("agent", `{"gate_satisfied_by":"yield"}`, "", `{"fields":{"gate_satisfied_by":"yield"}}`), false, ""},
		{"agent gate lingering on notes-only update dropped", "beads.bead.updated",
			wrap("agent", `{"gate_satisfied_by":"yield"}`, "", `{"notes":"pod_phase: Running"}`), false, ""},
		{"agent gate with no changes key dropped", "beads.bead.updated",
			wrap("agent", `{"gate_satisfied_by":"yield"}`, "", ""), false, ""},
		{"pipeline failed", "beads.bead.updated", wrap("merge-request", `{"mr_pipeline_status":"failed"}`, "", ""), true, "pipeline"},
		{"schedule noise dropped", "beads.bead.updated", wrap("schedule", "", "", ""), false, ""},
		{"advice noise dropped", "beads.bead.created", wrap("advice", "", "", ""), false, ""},
		{"agent heartbeat dropped", "beads.bead.updated", wrap("agent", `{"agent_state":"working"}`, "", `{"fields":{"agent_state":"working"}}`), false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, _, _, ok := classifyFeedEvent(c.topic, c.data)
			if ok != c.wantOK {
				t.Fatalf("classifyFeedEvent ok = %v, want %v", ok, c.wantOK)
			}
			if ok && kind.Name != c.wantKnd {
				t.Errorf("classifyFeedEvent kind = %q, want %q", kind.Name, c.wantKnd)
			}
		})
	}
}

// TestRenderFeedCard_PipelineStatusModifier confirms a pipeline card carries
// the ok/failed CSS modifier so green and red are distinguishable under the one
// "pipeline" kind, and the status surfaces as the actor.
func TestRenderFeedCard_PipelineStatusModifier(t *testing.T) {
	ok := renderFeedCard("/b", feedKindPipeline,
		&feedEventBead{ID: "gd-p1", Type: "merge-request", Title: "CI green",
			Fields: json.RawMessage(`{"mr_pipeline_status":"success"}`)}, "", fixedFeedNow(t))
	if !strings.Contains(ok, "feed-card-pipeline-ok") {
		t.Errorf("success pipeline missing ok modifier:\n%s", ok)
	}
	if !strings.Contains(ok, "success") {
		t.Errorf("success pipeline should show status as actor:\n%s", ok)
	}

	failed := renderFeedCard("/b", feedKindPipeline,
		&feedEventBead{ID: "gd-p2", Type: "merge-request", Title: "CI red",
			Fields: json.RawMessage(`{"mr_pipeline_status":"failed"}`)}, "", fixedFeedNow(t))
	if !strings.Contains(failed, "feed-card-pipeline-failed") {
		t.Errorf("failed pipeline missing failed modifier:\n%s", failed)
	}
}

// TestRenderFeedCard_NewFamilyClasses spot-checks the card class + label for
// each new family so the proposal colour taxonomy is wired end-to-end.
func TestRenderFeedCard_NewFamilyClasses(t *testing.T) {
	cases := []struct {
		kind      feedKind
		bead      *feedEventBead
		wantClass string
		wantLabel string
	}{
		{feedKindSquawk, &feedEventBead{ID: "gd-s", Type: "message", Title: "heads up", CreatedBy: "agent-x"}, "feed-card-squawk", "squawk"},
		{feedKindMail, &feedEventBead{ID: "gd-m", Type: "mail", Title: "inbox", CreatedBy: "agent-x"}, "feed-card-mail", "mail"},
		{feedKindSpawned, &feedEventBead{ID: "gd-a", Type: "agent", Title: "crew-1", Assignee: "crew-1"}, "feed-card-spawned", "spawned"},
		{feedKindStopped, &feedEventBead{ID: "gd-a2", Type: "agent", Title: "crew-2"}, "feed-card-stopped", "stopped"},
		{feedKindGate, &feedEventBead{ID: "gd-g", Type: "agent", Title: "gate", Fields: json.RawMessage(`{"gate_satisfied_by":"yield"}`)}, "feed-card-gate", "gate"},
		{feedKindMROpened, &feedEventBead{ID: "gd-o", Type: "merge-request", Title: "open MR"}, "feed-card-mr-opened", "MR opened"},
	}
	for _, c := range cases {
		t.Run(c.wantClass, func(t *testing.T) {
			row := renderFeedCard("/b", c.kind, c.bead, "", fixedFeedNow(t))
			if !strings.Contains(row, c.wantClass) {
				t.Errorf("card missing class %q:\n%s", c.wantClass, row)
			}
			if !strings.Contains(row, c.wantLabel) {
				t.Errorf("card missing label %q:\n%s", c.wantLabel, row)
			}
		})
	}
}

// TestFeedActor_Phase3 confirms each new family resolves a sensible actor line.
func TestFeedActor_Phase3(t *testing.T) {
	cases := []struct {
		name     string
		kind     feedKind
		bead     *feedEventBead
		closedBy string
		want     string
	}{
		{"squawk by creator", feedKindSquawk, &feedEventBead{CreatedBy: "agent-x"}, "", "by agent-x"},
		{"mail by creator", feedKindMail, &feedEventBead{CreatedBy: "agent-x"}, "", "by agent-x"},
		{"spawned shows assignee", feedKindSpawned, &feedEventBead{Assignee: "crew-1"}, "", "@crew-1"},
		{"stopped shows closer", feedKindStopped, &feedEventBead{}, "operator", "by operator"},
		{"gate shows satisfier", feedKindGate, &feedEventBead{Fields: json.RawMessage(`{"gate_satisfied_by":"yield"}`)}, "", "via yield"},
		{"pipeline shows status", feedKindPipeline, &feedEventBead{Fields: json.RawMessage(`{"mr_pipeline_status":"failed"}`)}, "", "failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feedActor(c.kind, c.bead, c.closedBy); got != c.want {
				t.Errorf("feedActor = %q, want %q", got, c.want)
			}
		})
	}
}
