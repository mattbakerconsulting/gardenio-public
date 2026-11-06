package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// threadAgentBead is a requesting agent bound to a Slack thread, the way a
// thread-spawned demo agent is.
func threadAgentBead(id, state string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Type: "agent", Status: "open", Fields: map[string]string{
		"agent": "thread-agent", "agent_state": state, "project": "gardenio", "role": "crew", "task_id": "gd-task-1",
		"slack_thread_channel": "C0DEMO", "slack_thread_ts": "1700000000.000100",
		"slack_spawn_channel": "C0DEMO", "slack_user_id": "U0HUMAN", "thread_id": "gd-wish-1",
	}}
}

func assertThreadRouting(t *testing.T, who string, fields map[string]string) {
	t.Helper()
	for k, want := range map[string]string{
		"slack_thread_channel": "C0DEMO", "slack_thread_ts": "1700000000.000100",
		"slack_spawn_channel": "C0DEMO", "slack_user_id": "U0HUMAN",
		beadsapi.FieldThreadID: "gd-wish-1", beadsapi.FieldSlackThreadBeadID: "gd-wish-1",
	} {
		if got := fields[k]; got != want {
			t.Errorf("%s %s = %q, want %q", who, k, got, want)
		}
	}
}

// The follow-through crew that resumes the blocked task keeps posting in the
// task's thread (gd-Q553TCVjN4).
func TestEscalations_FollowThroughInheritsRequesterThread(t *testing.T) {
	client := newMockEscalationBeadClient()
	client.beads["gd-agent-req"] = threadAgentBead("gd-agent-req", "done")
	esc := NewEscalations(EscalationsConfig{Daemon: client, Logger: slog.Default()})
	event := BeadEvent{ID: "gd-esc-ft", Type: "escalation", Fields: map[string]string{
		"operation_type": "cluster_read", "result_status": "success", "result": "3 nodes Ready",
		"requesting_agent": "gd-agent-req", "requesting_task": "gd-task-1",
	}}
	data, _ := json.Marshal(map[string]any{"bead": event})

	esc.handleClosed(context.Background(), data)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.spawnedAgentFields) != 1 {
		t.Fatalf("spawned %d agents, want 1 follow-through crew", len(client.spawnedAgentFields))
	}
	assertThreadRouting(t, "follow-through crew", client.spawnedAgentFields[0])
}

func TestEscalationApprovalLine(t *testing.T) {
	cases := []struct{ approver, method, rule, want string }{
		{"alex@x.example", "web_ui", "", "Approved by alex@x.example in the web UI"},
		{"alex@x.example", "cli", "", "Approved by alex@x.example from the command line"},
		{"", "policy", "floor:read_only", "Approved automatically by policy `floor:read_only`"},
		{"system", "policy", "", "Approved automatically by policy"},
		{"", "coordinator", "", "Approved by the coordinator"},
		{"alex@x.example", "", "", "Approved by alex@x.example"},
		{"alex@x.example", "something_new", "", "Approved by alex@x.example"},
		{"", "", "", ""},
	}
	for _, tc := range cases {
		if got := escalationApprovalLine(tc.approver, tc.method, tc.rule); got != tc.want {
			t.Errorf("escalationApprovalLine(%q, %q, %q) = %q, want %q", tc.approver, tc.method, tc.rule, got, tc.want)
		}
	}
}

func TestEscalationResultFallback(t *testing.T) {
	// The live push that read "Escalation success: Escalation: create on type
	// agent denied" (gd-Sv85FM7JFd) now leads with the outcome in words.
	if got := escalationResultFallback("success", "Spawn an infra reader"); got != "Done: Spawn an infra reader" {
		t.Errorf("success fallback = %q", got)
	}
	for status, want := range map[string]string{"partial": "Partly done", "failure": "Failed", "": "Finished", "odd": "Finished"} {
		if got := escalationOutcomeLabel(status); got != want {
			t.Errorf("escalationOutcomeLabel(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestEscalationResultExcerpt(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"internal result is quoted", map[string]string{"sensitivity": "internal", "result": "3 nodes\n  Ready"}, "> 3 nodes Ready"},
		{"summary wins", map[string]string{"sensitivity": "public", "result": "raw", "redacted_summary": "all good"}, "> all good"},
		{"mrkdwn is escaped", map[string]string{"sensitivity": "public", "result": "<!channel> a & b"}, "> &lt;!channel&gt; a &amp; b"},
		{"unlabelled shows nothing", map[string]string{"result": "token=abc"}, ""},
		{"secret shows nothing", map[string]string{"sensitivity": "secret", "result": "token=abc"}, ""},
		{"empty result", map[string]string{"sensitivity": "internal"}, ""},
	}
	for _, tc := range cases {
		if got := escalationResultExcerpt(tc.fields); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	long := escalationResultExcerpt(map[string]string{"sensitivity": "internal", "result": strings.Repeat("x", 400)})
	if n := len([]rune(long)); n > escalationResultExcerptRunes+2 {
		t.Errorf("excerpt is %d runes, want at most %d", n, escalationResultExcerptRunes+2)
	}
}
