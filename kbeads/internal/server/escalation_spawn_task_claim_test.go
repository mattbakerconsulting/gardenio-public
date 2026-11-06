package server

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// spawnTaskClaimFixture seeds a requesting crew (agent bead gd-req), the infra
// agent the replay just created (gd-spawned, task_id gd-task) and the
// escalation that requested it. verified sets requesting_agent_verified and
// requesting_agent_project the way applyEscalationRequestingAgentGuard does
// for a per-agent-token filer, and created_by is the filer's token subject.
func spawnTaskClaimFixture(t *testing.T, verified bool) (*BeadsServer, *mockStore, *model.Bead, *model.Bead) {
	t.Helper()
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seedBead(ms, "gd-req", "agent", model.StatusClosed, []string{"project:gardenio"},
		map[string]any{"agent": "crew-requester", "role": "crew", "project": "gardenio", "operated_by": "oidc:op@example.com"})
	seedBead(ms, "gd-spawned", "agent", model.StatusOpen, []string{"project:gardenio"},
		map[string]any{"agent": "infra-spawned", "role": "infra", "project": "gardenio", "task_id": "gd-task"})
	escFields := map[string]any{"operation_type": "create", "requesting_agent": "gd-req"}
	if verified {
		escFields[escalationRequestingAgentVerifiedField] = "true"
		escFields[escalationRequestingAgentProjectField] = "gardenio"
	}
	seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusOpen, nil, escFields)
	ms.beads["gd-esc"].CreatedBy = "crew-requester"
	// createBead stamps the operator on every live agent bead it writes;
	// the assigned dep inherits it (deps.operated_by_id is NOT NULL).
	opID := int64(42)
	ms.beads["gd-spawned"].OperatedBy = "oidc:op@example.com"
	ms.beads["gd-spawned"].OperatedByID = &opID
	return s, ms, ms.beads["gd-esc"], ms.beads["gd-spawned"]
}

// assignedDep returns the agent bead's assigned dep on gd-task, or nil.
func assignedDep(ms *mockStore, agentID string) *model.Dependency {
	for _, d := range ms.deps[agentID] {
		if d.DependsOnID == "gd-task" && d.Type == assignedDependencyType {
			return d
		}
	}
	return nil
}

func seedSpawnTask(ms *mockStore, createdBy, assignee string, status model.Status) {
	seedSpawnTaskIn(ms, "gardenio", createdBy, assignee, status)
}

func seedSpawnTaskIn(ms *mockStore, project, createdBy, assignee string, status model.Status) {
	seedBead(ms, "gd-task", model.TypeTask, status, []string{"project:" + project}, nil)
	ms.beads["gd-task"].CreatedBy = createdBy
	ms.beads["gd-task"].Assignee = assignee
}

func TestClaimReplayedSpawnTask(t *testing.T) {
	cases := []struct {
		name         string
		verified     bool
		createdBy    string
		assignee     string
		status       model.Status
		liveAssignee bool // seed an open agent bead named by assignee
		goneAssignee bool // seed a closed agent bead named by assignee
		want         string
		wantAssignee string
		wantStatus   model.Status
	}{
		{
			name: "requester filed the open task: pre-claimed", verified: true,
			createdBy: "crew-requester", status: model.StatusOpen,
			want: spawnTaskClaimed, wantAssignee: "infra-spawned", wantStatus: model.StatusInProgress,
		},
		{
			name: "live assignee is never clobbered", verified: true,
			createdBy: "crew-requester", assignee: "crew-busy", liveAssignee: true, status: model.StatusInProgress,
			want: spawnTaskLiveAssignee, wantAssignee: "crew-busy", wantStatus: model.StatusInProgress,
		},
		{
			name: "human assignee is never clobbered", verified: true,
			createdBy: "crew-requester", assignee: "someone@example.com", status: model.StatusInProgress,
			want: spawnTaskLiveAssignee, wantAssignee: "someone@example.com", wantStatus: model.StatusInProgress,
		},
		{
			name: "gone assignee is reclaimed for the new agent", verified: true,
			createdBy: "crew-requester", assignee: "crew-dead", goneAssignee: true, status: model.StatusInProgress,
			want: spawnTaskClaimed, wantAssignee: "infra-spawned", wantStatus: model.StatusInProgress,
		},
		{
			name: "requester without ownership gets no pre-claim", verified: true,
			createdBy: "someone-else", status: model.StatusOpen,
			want: spawnTaskRequesterDenied, wantAssignee: "", wantStatus: model.StatusOpen,
		},
		{
			name: "unverified requester gets no pre-claim", verified: false,
			createdBy: "crew-requester", status: model.StatusOpen,
			want: spawnTaskUnverified, wantAssignee: "", wantStatus: model.StatusOpen,
		},
		{
			name: "closed task is left closed", verified: true,
			createdBy: "crew-requester", status: model.StatusClosed,
			want: spawnTaskClosed, wantAssignee: "", wantStatus: model.StatusClosed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ms, esc, agent := spawnTaskClaimFixture(t, tc.verified)
			seedSpawnTask(ms, tc.createdBy, tc.assignee, tc.status)
			if tc.liveAssignee {
				seedBead(ms, "gd-assignee", "agent", model.StatusInProgress, nil, map[string]any{"agent": tc.assignee})
			}
			if tc.goneAssignee {
				seedBead(ms, "gd-assignee", "agent", model.StatusClosed, nil, map[string]any{"agent": tc.assignee})
			}

			got := s.claimReplayedSpawnTask(context.Background(), esc, agent)
			if got != tc.want {
				t.Errorf("outcome = %q, want %q", got, tc.want)
			}
			task, _ := ms.GetBead(context.Background(), "gd-task")
			if task.Assignee != tc.wantAssignee || task.Status != tc.wantStatus {
				t.Errorf("task = status %q assignee %q, want status %q assignee %q",
					task.Status, task.Assignee, tc.wantStatus, tc.wantAssignee)
			}
			// The assigned dep is added whatever happens to the claim, as the
			// client does.
			dep := assignedDep(ms, "gd-spawned")
			if dep == nil {
				t.Fatalf("agent bead has no assigned dep to the task")
			}
			// mockStore.AddDependency accepts a nil FK; Postgres does not
			// (deps.operated_by_id NOT NULL, ka-0000007722).
			if dep.OperatedByID == nil {
				t.Fatalf("assigned dep OperatedByID is nil; would violate deps.operated_by_id NOT NULL in Postgres")
			}
			if *dep.OperatedByID != *agent.OperatedByID || dep.OperatedBy != agent.OperatedBy {
				t.Errorf("assigned dep operator = %q/%d, want the agent bead's %q/%d",
					dep.OperatedBy, *dep.OperatedByID, agent.OperatedBy, *agent.OperatedByID)
			}
		})
	}
}

func TestClaimReplayedSpawnTask_NoTaskID(t *testing.T) {
	s, ms, esc, _ := spawnTaskClaimFixture(t, true)
	seedBead(ms, "gd-adhoc", "agent", model.StatusOpen, nil, map[string]any{"agent": "infra-adhoc", "role": "infra"})
	if got := s.claimReplayedSpawnTask(context.Background(), esc, ms.beads["gd-adhoc"]); got != "" {
		t.Errorf("outcome = %q, want \"\" for an agent with no task_id", got)
	}
	if len(ms.deps["gd-adhoc"]) != 0 {
		t.Errorf("deps = %v, want none", ms.deps["gd-adhoc"])
	}
}

func TestClaimReplayedSpawnTask_MissingTask(t *testing.T) {
	s, ms, esc, agent := spawnTaskClaimFixture(t, true)
	if got := s.claimReplayedSpawnTask(context.Background(), esc, agent); got != spawnTaskNotFound {
		t.Errorf("outcome = %q, want %q", got, spawnTaskNotFound)
	}
	if len(ms.deps["gd-spawned"]) != 0 {
		t.Errorf("deps = %v, want none for a missing task", ms.deps["gd-spawned"])
	}
}

// An agent bead without an operator FK gets no assigned dep (the insert would
// violate deps.operated_by_id NOT NULL), but the pre-claim still runs.
func TestClaimReplayedSpawnTask_NoOperatorFKSkipsDep(t *testing.T) {
	s, ms, esc, agent := spawnTaskClaimFixture(t, true)
	seedSpawnTask(ms, "crew-requester", "", model.StatusOpen)
	agent.OperatedByID = nil
	if got := s.claimReplayedSpawnTask(context.Background(), esc, agent); got != spawnTaskClaimed {
		t.Errorf("outcome = %q, want %q", got, spawnTaskClaimed)
	}
	if dep := assignedDep(ms, "gd-spawned"); dep != nil {
		t.Errorf("assigned dep = %+v, want none without an operator FK", dep)
	}
}

// The pre-claim's identity comes from the filer's credential, never from the
// requester's agent bead: that bead is config-kind, so the agent can PATCH its
// own fields.agent and fields.project. Each case is a deny path; the task must
// stay unclaimed (gd-G5PmhWXsF9, merge desk send-back on f8085fa7).
func TestClaimReplayedSpawnTask_RefusesUnverifiedIdentity(t *testing.T) {
	cases := []struct {
		name        string
		taskProject string
		taskOwner   string
		setup       func(ms *mockStore)
		want        string
	}{
		{
			// The agent renames itself to the victim on its bead. created_by
			// still says crew-requester, which does not own the victim's task.
			name: "spoofed fields.agent on the requester bead", taskProject: "gardenio", taskOwner: "victim-crew",
			setup: func(ms *mockStore) {
				ms.beads["gd-req"].Fields = mustMarshal(map[string]any{"agent": "victim-crew", "role": "crew", "project": "gardenio"})
			},
			want: spawnTaskRequesterDenied,
		},
		{
			name: "spoofed agent title with blank fields.agent", taskProject: "gardenio", taskOwner: "victim-crew",
			setup: func(ms *mockStore) {
				ms.beads["gd-req"].Title = "victim-crew"
				ms.beads["gd-req"].Fields = mustMarshal(map[string]any{"role": "crew", "project": "gardenio"})
			},
			want: spawnTaskRequesterDenied,
		},
		{
			// An escalation filed with no verified project (or before the
			// stamp existed) is refused, never run with no project scope.
			name: "no verified project on the escalation", taskProject: "gardenio", taskOwner: "crew-requester",
			setup: func(ms *mockStore) {
				ms.beads["gd-esc"].Fields = deleteFieldKey(ms.beads["gd-esc"].Fields, escalationRequestingAgentProjectField)
			},
			want: spawnTaskUnverified,
		},
		{
			name: "blank verified project on the escalation", taskProject: "gardenio", taskOwner: "crew-requester",
			setup: func(ms *mockStore) {
				ms.beads["gd-esc"].Fields = setField(ms.beads["gd-esc"].Fields, escalationRequestingAgentProjectField, " ")
			},
			want: spawnTaskUnverified,
		},
		{
			name: "blank fields.project on the requester bead and no stamp", taskProject: "gardenio", taskOwner: "crew-requester",
			setup: func(ms *mockStore) {
				ms.beads["gd-req"].Fields = mustMarshal(map[string]any{"agent": "crew-requester", "role": "crew", "project": ""})
				ms.beads["gd-esc"].Fields = deleteFieldKey(ms.beads["gd-esc"].Fields, escalationRequestingAgentProjectField)
			},
			want: spawnTaskUnverified,
		},
		{
			// The agent moves its bead to another project to reach a task
			// there. The claim still runs in the token's stamped project.
			name: "changed fields.project on the requester bead", taskProject: "other", taskOwner: "crew-requester",
			setup: func(ms *mockStore) {
				ms.beads["gd-req"].Fields = mustMarshal(map[string]any{"agent": "crew-requester", "role": "crew", "project": "other"})
			},
			want: spawnTaskRequesterDenied,
		},
		{
			name: "no created_by on the escalation", taskProject: "gardenio", taskOwner: "crew-requester",
			setup: func(ms *mockStore) { ms.beads["gd-esc"].CreatedBy = "" },
			want:  spawnTaskUnverified,
		},
		{
			name: "created_by is not an agent token subject", taskProject: "gardenio", taskOwner: "op@example.com",
			setup: func(ms *mockStore) { ms.beads["gd-esc"].CreatedBy = "op@example.com" },
			want:  spawnTaskUnverified,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ms, esc, agent := spawnTaskClaimFixture(t, true)
			seedSpawnTaskIn(ms, tc.taskProject, tc.taskOwner, "", model.StatusOpen)
			tc.setup(ms)

			if got := s.claimReplayedSpawnTask(context.Background(), esc, agent); got != tc.want {
				t.Errorf("outcome = %q, want %q", got, tc.want)
			}
			task, _ := ms.GetBead(context.Background(), "gd-task")
			if task.Assignee != "" || task.Status != model.StatusOpen {
				t.Errorf("task = status %q assignee %q, want it open and unassigned", task.Status, task.Assignee)
			}
		})
	}
}

// The requester bead's own fields play no part: with them blanked, the
// credential stamps alone still let the real owner's spawn pre-claim.
func TestClaimReplayedSpawnTask_IgnoresRequesterBeadFields(t *testing.T) {
	s, ms, esc, agent := spawnTaskClaimFixture(t, true)
	seedSpawnTask(ms, "crew-requester", "", model.StatusOpen)
	ms.beads["gd-req"].Fields = mustMarshal(map[string]any{"role": "crew"})
	if got := s.claimReplayedSpawnTask(context.Background(), esc, agent); got != spawnTaskClaimed {
		t.Errorf("outcome = %q, want %q", got, spawnTaskClaimed)
	}
}
