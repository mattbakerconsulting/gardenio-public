package server

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/model"
)

// T5 (epic ka-0000002541): the operatedByFromAgentBead resolver gains a final
// per-hop fallback that lifts a HUMAN-SHAPED operator from the agent bead's
// parent task (task_id) or defining trigger/schedule (schedule_id) bead, so a
// system-spawned crew whose own spawn chain roots in a machine identity can
// still attribute its writes to the human who authored the parent work. The
// source is gated behind BEADS_AUTHORING_HUMAN_GATE and NEVER binds a
// sentinel/machine identity (fail-closed boundary ka-0000005772).

// TestOperatorFromParentTaskBead_GateOff_NoOp: with the gate OFF the new source
// is dark — an agent resolvable ONLY via its parent task yields "" (pre-change
// behavior preserved).
func TestOperatorFromParentTaskBead_GateOff_NoOp(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "")
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"task_id":"gd-TASK1"}`),
		},
		&model.Bead{
			ID: "gd-TASK1", Type: "task",
			Fields: []byte(`{"operated_by":"slack:U0OPERATOR1"}`),
		},
	)
	if got := srv.operatedByFromAgentBead(context.Background(), "crew"); got != "" {
		t.Fatalf("gate OFF: parent-task source must be dark; got %q want \"\"", got)
	}
}

// TestOperatorFromParentTaskBead_GateOn_TaskOperatedBy: with the gate ON, a crew
// whose only attribution is its parent task's operated_by resolves to that
// human.
func TestOperatorFromParentTaskBead_GateOn_TaskOperatedBy(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"task_id":"gd-TASK1"}`),
		},
		&model.Bead{
			ID: "gd-TASK1", Type: "task",
			Fields: []byte(`{"operated_by":"slack:U0OPERATOR1"}`),
		},
	)
	if got := srv.operatedByFromAgentBead(context.Background(), "crew"); got != "slack:U0OPERATOR1" {
		t.Fatalf("gate ON: got %q; want slack:U0OPERATOR1 from parent task", got)
	}
}

// TestOperatorFromParentTaskBead_GateOn_ScheduleOperatedBy: a crew fanned out
// from a trigger/schedule bead resolves to the schedule author.
func TestOperatorFromParentTaskBead_GateOn_ScheduleOperatedBy(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"schedule_id":"gd-TRIG1"}`),
		},
		&model.Bead{
			ID: "gd-TRIG1", Type: "trigger",
			Fields: []byte(`{"operated_by":"oidc:matthew.baker@acme.example"}`),
		},
	)
	if got := srv.operatedByFromAgentBead(context.Background(), "crew"); got != "oidc:matthew.baker@acme.example" {
		t.Fatalf("gate ON: got %q; want oidc:matthew.baker@acme.example from parent trigger", got)
	}
}

// TestOperatorFromParentTaskBead_GateOn_UserBeadID: a parent task carrying only
// a user_bead_id pointer resolves through the user bead.
func TestOperatorFromParentTaskBead_GateOn_UserBeadID(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"task_id":"gd-TASK1"}`),
		},
		&model.Bead{
			ID: "gd-TASK1", Type: "task",
			Fields: []byte(`{"user_bead_id":"gd-USER1"}`),
		},
		&model.Bead{
			ID: "gd-USER1", Type: "user",
			Fields: []byte(`{"slack_id":"U0OPERATOR1"}`),
		},
	)
	if got := srv.operatedByFromAgentBead(context.Background(), "crew"); got != "slack:U0OPERATOR1" {
		t.Fatalf("gate ON: got %q; want slack:U0OPERATOR1 via parent-task user bead", got)
	}
}

// TestOperatorFromParentTaskBead_GateOn_SentinelParentRefuses pins the
// fail-closed boundary (ka-0000005772): a parent task whose ONLY attribution is
// a sentinel / machine identity must NOT be bound — the resolver returns "" so
// the missing human surfaces loudly at the spawner.
func TestOperatorFromParentTaskBead_GateOn_SentinelParentRefuses(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
	for _, sentinel := range []string{"system:legacy-pre-attribution", "fleet:gardenio", "system:machine"} {
		srv := newChainWalkServer(
			&model.Bead{
				ID: "crew", Type: "agent",
				Fields: []byte(`{"task_id":"gd-TASK1"}`),
			},
			&model.Bead{
				ID: "gd-TASK1", Type: "task",
				Fields: []byte(`{"operated_by":"` + sentinel + `"}`),
			},
		)
		if got := srv.operatedByFromAgentBead(context.Background(), "crew"); got != "" {
			t.Fatalf("sentinel parent %q must NOT bind; got %q want \"\"", sentinel, got)
		}
	}
}

// TestOperatorFromParentTaskBead_GateOn_AgentShapedParentRefuses: a parent task
// whose operated_by is an agent-shaped (non-human) id must not bind either.
func TestOperatorFromParentTaskBead_GateOn_AgentShapedParentRefuses(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"task_id":"gd-TASK1"}`),
		},
		&model.Bead{
			ID: "gd-TASK1", Type: "task",
			Fields: []byte(`{"operated_by":"gardenio-some-agent-x9z"}`),
		},
	)
	if got := srv.operatedByFromAgentBead(context.Background(), "crew"); got != "" {
		t.Fatalf("agent-shaped parent operated_by must NOT bind; got %q want \"\"", got)
	}
}

// TestOperatorFromParentTaskBead_GateOn_OwnFieldsWinFirst: the parent-task
// source is LAST-RESORT — an agent with its own human attribution resolves to
// that without consulting the parent task.
func TestOperatorFromParentTaskBead_GateOn_OwnFieldsWinFirst(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"operated_by":"slack:UOWN0001","task_id":"gd-TASK1"}`),
		},
		&model.Bead{
			ID: "gd-TASK1", Type: "task",
			Fields: []byte(`{"operated_by":"slack:UPARENT99"}`),
		},
	)
	if got := srv.operatedByFromAgentBead(context.Background(), "crew"); got != "slack:UOWN0001" {
		t.Fatalf("own field must win over parent task; got %q want slack:UOWN0001", got)
	}
}

// TestOperatorFromParentTaskBead_GateOn_TaskPreferredOverSchedule: when both
// task_id and schedule_id are present, the pre-assigned task is the more
// specific source and is consulted first.
func TestOperatorFromParentTaskBead_GateOn_TaskPreferredOverSchedule(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"task_id":"gd-TASK1","schedule_id":"gd-TRIG1"}`),
		},
		&model.Bead{
			ID: "gd-TASK1", Type: "task",
			Fields: []byte(`{"operated_by":"slack:UTASK0001"}`),
		},
		&model.Bead{
			ID: "gd-TRIG1", Type: "trigger",
			Fields: []byte(`{"operated_by":"slack:UTRIG0001"}`),
		},
	)
	if got := srv.operatedByFromAgentBead(context.Background(), "crew"); got != "slack:UTASK0001" {
		t.Fatalf("task_id must be preferred over schedule_id; got %q want slack:UTASK0001", got)
	}
}

// TestOperatorFromParentTaskBead_GateOn_SubjectParentAcrossChain pins that the
// parent-task fallback is consulted against the SUBJECT agent's parent linkage
// once after the spawn-chain walk — even when the subject is several hops from
// any human and an intermediate ancestor carries no parent linkage. The subject
// (crew) carries task_id; the walk exhausts self-attribution across crew →
// gardener → sched (none human), then the after-loop step resolves the crew's
// parent task author.
func TestOperatorFromParentTaskBead_GateOn_SubjectParentAcrossChain(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"spawned_by":"gardener","task_id":"gd-TASK1"}`),
		},
		&model.Bead{
			ID: "gardener", Type: "agent",
			Fields: []byte(`{"spawned_by":"sched"}`),
		},
		&model.Bead{
			ID: "sched", Type: "agent",
			Fields: []byte(`{}`),
		},
		&model.Bead{
			ID: "gd-TASK1", Type: "task",
			Fields: []byte(`{"operated_by":"slack:U0OPERATOR1"}`),
		},
	)
	if got := srv.operatedByFromAgentBead(context.Background(), "crew"); got != "slack:U0OPERATOR1" {
		t.Fatalf("subject parent-task across chain: got %q; want slack:U0OPERATOR1", got)
	}
}

// TestOperatorFromParentTaskBead_GateOn_AncestorParentNotConsulted pins the
// hot-path optimization: the parent-task fallback uses ONLY the subject agent's
// linkage, NOT an ancestor's. Here the subject has no parent task but an
// ancestor (gardener) does — the resolver must NOT bind the ancestor's parent
// (that would be the per-hop amplification we removed) and instead falls through
// to "".
func TestOperatorFromParentTaskBead_GateOn_AncestorParentNotConsulted(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"spawned_by":"gardener"}`),
		},
		&model.Bead{
			ID: "gardener", Type: "agent",
			Fields: []byte(`{"task_id":"gd-TASK1"}`),
		},
		&model.Bead{
			ID: "gd-TASK1", Type: "task",
			Fields: []byte(`{"operated_by":"slack:U0OPERATOR1"}`),
		},
	)
	if got := srv.operatedByFromAgentBead(context.Background(), "crew"); got != "" {
		t.Fatalf("ancestor parent task must NOT be consulted; got %q want \"\"", got)
	}
}

// TestOperatorFromParentTaskBead_GateOn_MissingParentBead: a dangling task_id
// (parent bead absent) falls through to "" without erroring the walk.
func TestOperatorFromParentTaskBead_GateOn_MissingParentBead(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"task_id":"gd-GONE"}`),
		},
	)
	if got := srv.operatedByFromAgentBead(context.Background(), "crew"); got != "" {
		t.Fatalf("dangling task_id: got %q want \"\"", got)
	}
}
