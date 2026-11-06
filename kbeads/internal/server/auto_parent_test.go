package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// seedAgentBead inserts a synthetic agent bead whose Fields carry task_id, used
// by the server-side auto-parent step to look up the agent's current task.
func seedAgentBead(t *testing.T, ms *mockStore, name, taskID string) {
	t.Helper()
	// slack_user_id is required so the post-R2 resolver can attribute the
	// static-token write to a human operator via the agent-bead fallback.
	fields := map[string]any{"task_id": taskID, "slack_user_id": "U0OPERATOR2"}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}
	ms.beads[name] = &model.Bead{
		ID:     name,
		Kind:   model.KindConfig,
		Type:   "agent",
		Title:  name,
		Status: model.StatusOpen,
		Fields: raw,
	}
	// The task itself must exist so the post-create dep insert has a target,
	// but mockStore.AddDependency doesn't enforce FK so we keep it minimal.
	ms.beads[taskID] = &model.Bead{
		ID:     taskID,
		Kind:   model.KindIssue,
		Type:   "task",
		Title:  "task " + taskID,
		Status: model.StatusOpen,
	}
}

// seedThreadAgentBead inserts a synthetic thread-spawned agent bead. Unlike a
// pre-assigned crew agent it carries NO task_id — thread agents are opened from
// a Slack thread, not pre-assigned a task. The per-thread epic identity lives in
// the slack_thread_bead_id field, which the auto-parent step falls back to.
func seedThreadAgentBead(t *testing.T, ms *mockStore, name, threadBeadID string) {
	t.Helper()
	fields := map[string]any{
		"slack_thread_bead_id": threadBeadID,
		"slack_thread_channel": "C0THREAD",
		"slack_thread_ts":      "1780289972.820429",
		"slack_user_id":        "U0OPERATOR2",
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}
	ms.beads[name] = &model.Bead{
		ID:     name,
		Kind:   model.KindConfig,
		Type:   "agent",
		Title:  name,
		Status: model.StatusOpen,
		Fields: raw,
	}
	ms.beads[threadBeadID] = &model.Bead{
		ID:     threadBeadID,
		Kind:   model.KindIssue,
		Type:   "task",
		Title:  "thread bead " + threadBeadID,
		Status: model.StatusOpen,
	}
}

// TestCreateBead_AutoParent_FiresForThreadAgent is the acceptance test for
// ka-0000006984: a task bead created by a thread-spawned agent (no task_id, only
// slack_thread_bead_id) auto-parents to its thread bead at create time. Before
// the fix the auto-parent step only consulted task_id, so thread-agent `kd
// create` calls orphaned (e.g. ka-0000000816 had parent:None / empty deps).
func TestCreateBead_AutoParent_FiresForThreadAgent(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const agentName = "thread-1780289972-820429"
	const threadBeadID = "gd-thread-epic-aaaa"
	seedThreadAgentBead(t, ms, agentName, threadBeadID)

	body, _ := json.Marshal(map[string]any{
		"title": "derivative task from thread agent",
		"type":  "task",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", agentName)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, rec.Body.String())
	}
	if resp.ID == "" {
		t.Fatalf("expected bead id in response; body: %s", rec.Body.String())
	}

	var found *model.Dependency
	for _, d := range ms.deps[resp.ID] {
		if d.Type == model.DepParentChild && d.DependsOnID == threadBeadID {
			found = d
			break
		}
	}
	if found == nil {
		t.Fatalf("expected parent-child dep to thread bead %s, got deps=%+v", threadBeadID, ms.deps[resp.ID])
	}
	if found.CreatedBy != agentName {
		t.Fatalf("expected dep.CreatedBy=%q, got %q", agentName, found.CreatedBy)
	}
}

// TestCreateBead_AutoParent_TaskIDWinsOverThreadBead covers the resolution
// precedence: an agent carrying BOTH task_id and slack_thread_bead_id parents to
// task_id (the explicit claimed task), not the thread bead.
func TestCreateBead_AutoParent_TaskIDWinsOverThreadBead(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const agentName = "gardenio-test-agent-both"
	const taskID = "gd-task-both"
	const threadBeadID = "gd-thread-both"
	fields := map[string]any{
		"task_id":              taskID,
		"slack_thread_bead_id": threadBeadID,
		"slack_user_id":        "U0OPERATOR2",
	}
	raw, _ := json.Marshal(fields)
	ms.beads[agentName] = &model.Bead{
		ID: agentName, Kind: model.KindConfig, Type: "agent",
		Title: agentName, Status: model.StatusOpen, Fields: raw,
	}
	ms.beads[taskID] = &model.Bead{ID: taskID, Kind: model.KindIssue, Type: "task", Status: model.StatusOpen}
	ms.beads[threadBeadID] = &model.Bead{ID: threadBeadID, Kind: model.KindIssue, Type: "task", Status: model.StatusOpen}

	body, _ := json.Marshal(map[string]any{"title": "derivative", "type": "task"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", agentName)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	var pcEdges []*model.Dependency
	for _, d := range ms.deps[resp.ID] {
		if d.Type == model.DepParentChild {
			pcEdges = append(pcEdges, d)
		}
	}
	if len(pcEdges) != 1 {
		t.Fatalf("expected exactly one parent-child edge, got %d: %+v", len(pcEdges), pcEdges)
	}
	if pcEdges[0].DependsOnID != taskID {
		t.Fatalf("expected auto-parent to task_id %q (precedence over thread bead), got %q", taskID, pcEdges[0].DependsOnID)
	}
}

// TestCreateBead_AutoParent_FiresForAgentActor covers acceptance criterion #1:
// when X-Actor matches a known agent bead whose Fields carry task_id, the
// server wires a parent-child dep from the new bead to that task.
func TestCreateBead_AutoParent_FiresForAgentActor(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const agentName = "gardenio-test-agent-aaaa"
	const taskID = "gd-task-aaaa"
	seedAgentBead(t, ms, agentName, taskID)

	body, _ := json.Marshal(map[string]any{
		"title": "new derivative bead",
		"type":  "task",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", agentName)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, rec.Body.String())
	}
	if resp.ID == "" {
		t.Fatalf("expected bead id in response; body: %s", rec.Body.String())
	}

	deps := ms.deps[resp.ID]
	var found *model.Dependency
	for _, d := range deps {
		if d.Type == model.DepParentChild && d.DependsOnID == taskID {
			found = d
			break
		}
	}
	if found == nil {
		t.Fatalf("expected parent-child dep to %s, got deps=%+v", taskID, deps)
	}
	if found.CreatedBy != agentName {
		t.Fatalf("expected dep.CreatedBy=%q, got %q", agentName, found.CreatedBy)
	}
}

// seedRealisticAgentBead inserts an agent bead the way a real spawn does: the
// bead ID is a random-ish nanoid that is DISTINCT from the agent name, and the
// name lives only in fields.agent (+ Title). This mirrors SpawnAgent, which
// passes Title=agentName and lets CreateBead mint the id — so name != id for
// every production agent. Tests that seed ID==name (seedAgentBead) cannot catch
// the by-name resolution regression (ka-0000005744).
func seedRealisticAgentBead(t *testing.T, ms *mockStore, beadID, agentName, taskID string) {
	t.Helper()
	fields := map[string]any{
		"agent":         agentName,
		"task_id":       taskID,
		"slack_user_id": "U0OPERATOR2",
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}
	ms.beads[beadID] = &model.Bead{
		ID:     beadID,
		Kind:   model.KindConfig,
		Type:   "agent",
		Title:  agentName,
		Status: model.StatusInProgress,
		Fields: raw,
	}
	ms.beads[taskID] = &model.Bead{
		ID:     taskID,
		Kind:   model.KindIssue,
		Type:   "task",
		Title:  "task " + taskID,
		Status: model.StatusOpen,
	}
}

// TestCreateBead_AutoParent_FiresWhenActorIsAgentName is the regression guard
// for ka-0000005744: the X-Actor value is the agent NAME (KD_ACTOR =
// spec.AgentName), while the agent bead's id is a distinct nanoid. The
// server-side auto-parent step must resolve the acting agent by name
// (FindAgentByName) — not only by GetBead(id) — or every agent-authored
// `kd create` orphans, which is exactly why /plans clarifying-question
// decisions never appeared under their plan epic.
func TestCreateBead_AutoParent_FiresWhenActorIsAgentName(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	// Nanoid-shaped id, distinct from the name — the production shape.
	const beadID = "gd-qVEuOerawq"
	const agentName = "gardenio-plan-assistant-ka-0000003778-9b01"
	const taskID = "gd-plan-epic-xyz"
	seedRealisticAgentBead(t, ms, beadID, agentName, taskID)

	// A plain task stands in for any agent-authored bead — the auto-parent step
	// is type-agnostic. (In the field report the orphaned bead was a
	// type=decision clarifying question, but decisions require a prompt field;
	// the resolution path under test is identical.)
	body, _ := json.Marshal(map[string]any{
		"title": "derivative bead authored by the plan assistant",
		"type":  "task",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	// The actor is the agent NAME, exactly as the pod sends it (X-Actor =
	// KD_ACTOR = spec.AgentName).
	req.Header.Set("X-Actor", agentName)
	// X-Operated-By is set on every agent pod (BEADS_OPERATED_BY) and is what
	// the operator resolver uses in production. Provide it so this test isolates
	// the auto-parent behavior and does not also exercise the (separately
	// tracked) operator-resolver first-hop by-name gap.
	req.Header.Set("X-Operated-By", "oidc:matthew.baker@acme.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, rec.Body.String())
	}

	var found *model.Dependency
	for _, d := range ms.deps[resp.ID] {
		if d.Type == model.DepParentChild && d.DependsOnID == taskID {
			found = d
			break
		}
	}
	if found == nil {
		t.Fatalf("expected parent-child dep to task_id %s (auto-parent must resolve actor-name→agent bead), got deps=%+v", taskID, ms.deps[resp.ID])
	}
}

// TestCreateBead_AutoParent_OptOutHeader covers the X-No-Auto-Parent: 1 opt-out
// path: even with a valid agent actor + task_id, no dep is added.
func TestCreateBead_AutoParent_OptOutHeader(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const agentName = "gardenio-test-agent-optout"
	const taskID = "gd-task-optout"
	seedAgentBead(t, ms, agentName, taskID)

	body, _ := json.Marshal(map[string]any{
		"title": "opt-out bead",
		"type":  "task",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", agentName)
	req.Header.Set("X-No-Auto-Parent", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, rec.Body.String())
	}

	for _, d := range ms.deps[resp.ID] {
		if d.Type == model.DepParentChild && d.DependsOnID == taskID {
			t.Fatalf("expected no auto-parent dep when X-No-Auto-Parent: 1 set; got %+v", d)
		}
	}
}

// TestCreateBead_AutoParent_SkipsAgentBead is the regression guard for bug
// ka-0000002199: a gardener whose own task_id IS a goal-bearing epic spawns a
// crew agent bead (type=agent). The daemon auto-parent step must NOT wire a
// parent-child edge from that agent bead to the epic — doing so pollutes the
// epic's child set with a non-issue bead that never closes on the work
// lifecycle, making a "all children closed" terminal_goal unreachable (advice
// ka-0000002123). The agent's real linkage to its task is the `assigned` edge +
// fields.task_id, written by SpawnAgent, not this parent-child edge.
func TestCreateBead_AutoParent_SkipsAgentBead(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	// The acting gardener agent whose task_id is the (goal-bearing) epic.
	const gardenerName = "gardenio-e2e-coverage-sprint-gardener-xyz"
	const epicID = "gd-goal-epic-aaaa"
	seedAgentBead(t, ms, gardenerName, epicID)

	// The gardener creates a crew agent bead — exactly the SpawnAgent shape:
	// type=agent with the required agent/role/project fields.
	beadID := createBeadAsAgent(t, h, gardenerName, map[string]any{
		"title": "gardenio-crew-abc",
		"type":  "agent",
		"fields": map[string]any{
			"agent":   "gardenio-crew-abc",
			"role":    "crew",
			"project": "gardenio",
			"task_id": "gd-some-child-task",
		},
	}, map[string]string{"X-Operated-By": "slack:U0OPERATOR2"})

	if hasParentChildTo(ms, beadID, epicID) {
		t.Fatalf("expected NO auto-parent parent-child edge from the crew agent bead %s to the epic %s; got deps=%+v",
			beadID, epicID, ms.deps[beadID])
	}
	// Belt-and-suspenders: the skip is type-scoped, so NO parent-child edge of
	// any target should have been auto-wired for the agent bead.
	for _, d := range ms.deps[beadID] {
		if d.Type == model.DepParentChild {
			t.Fatalf("expected no auto-parent parent-child edge on a type=agent bead; got %+v", d)
		}
	}
}

// TestCreateBead_AutoParent_SkipsSessionBead is the sibling guard for bug
// ka-0000002199: a per-run session bead (type=session) minted by a gardener whose
// task_id is a goal-bearing epic must not be auto-parented under that epic. A
// session bead is tied to its agent by the `session_of` edge, not parent-child.
func TestCreateBead_AutoParent_SkipsSessionBead(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const gardenerName = "gardenio-e2e-coverage-sprint-gardener-sess"
	const epicID = "gd-goal-epic-bbbb"
	seedAgentBead(t, ms, gardenerName, epicID)

	beadID := createBeadAsAgent(t, h, gardenerName, map[string]any{
		"title": "session:gardenio-crew-abc:2026-08-05",
		"type":  "session",
		"fields": map[string]any{
			"agent":   "gardenio-crew-abc",
			"project": "gardenio",
		},
	}, map[string]string{"X-Operated-By": "slack:U0OPERATOR2"})

	if hasParentChildTo(ms, beadID, epicID) {
		t.Fatalf("expected NO auto-parent parent-child edge from the session bead %s to the epic %s; got deps=%+v",
			beadID, epicID, ms.deps[beadID])
	}
	for _, d := range ms.deps[beadID] {
		if d.Type == model.DepParentChild {
			t.Fatalf("expected no auto-parent parent-child edge on a type=session bead; got %+v", d)
		}
	}
}

// TestCreateBead_AutoParent_SkipsMessageBead is the comms-path guard for bug
// ka-0000004462: a squawk/notify bead (type=message) minted by a gardener whose
// task_id is a goal-bearing epic must NOT be auto-parented under that epic.
// Every `gb squawk` from a gardener whose task_id IS the epic mints one of these,
// and because a message bead auto-closes on the comms cadence (never on the WORK
// lifecycle) it both makes an "all children closed" terminal_goal unreachable
// and inflates the closed-child count gardeners quote as progress. A message
// bead's real linkage is its `thread_id` field + slack-thread labels, not a
// parent-child edge to the epic.
func TestCreateBead_AutoParent_SkipsMessageBead(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const gardenerName = "gardenio-e2e-coverage-sprint-gardener-msg"
	const epicID = "gd-goal-epic-dddd"
	seedAgentBead(t, ms, gardenerName, epicID)

	// The gardener squawks — a type=message bead, the shape `gb squawk` produces.
	// No fields: message is a type-config'd type that rejects agent/project, and
	// the skip keys on bead.Type alone, so the type is all this case needs.
	beadID := createBeadAsAgent(t, h, gardenerName, map[string]any{
		"title": "on it — extending the suppression test",
		"type":  "message",
	}, map[string]string{"X-Operated-By": "slack:U0OPERATOR2"})

	if hasParentChildTo(ms, beadID, epicID) {
		t.Fatalf("expected NO auto-parent parent-child edge from the message bead %s to the epic %s; got deps=%+v",
			beadID, epicID, ms.deps[beadID])
	}
	for _, d := range ms.deps[beadID] {
		if d.Type == model.DepParentChild {
			t.Fatalf("expected no auto-parent parent-child edge on a type=message bead; got %+v", d)
		}
	}
}

// TestCreateBead_AutoParent_SkipsMailBead is the sibling comms guard for bug
// ka-0000004462: a mail bead (type=mail) minted by a gardener whose task_id is a
// goal-bearing epic must NOT be auto-parented under that epic. Like a message
// bead it auto-closes on the comms cadence and pollutes the epic's child set;
// its real linkage is the `thread_id` field + comms labels.
func TestCreateBead_AutoParent_SkipsMailBead(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const gardenerName = "gardenio-e2e-coverage-sprint-gardener-mail"
	const epicID = "gd-goal-epic-eeee"
	seedAgentBead(t, ms, gardenerName, epicID)

	beadID := createBeadAsAgent(t, h, gardenerName, map[string]any{
		"title": "mail: cross-crew handoff note",
		"type":  "mail",
	}, map[string]string{"X-Operated-By": "slack:U0OPERATOR2"})

	if hasParentChildTo(ms, beadID, epicID) {
		t.Fatalf("expected NO auto-parent parent-child edge from the mail bead %s to the epic %s; got deps=%+v",
			beadID, epicID, ms.deps[beadID])
	}
	for _, d := range ms.deps[beadID] {
		if d.Type == model.DepParentChild {
			t.Fatalf("expected no auto-parent parent-child edge on a type=mail bead; got %+v", d)
		}
	}
}

// TestCreateBead_AutoParent_SkipsAdviceBead: a crew that runs `gb advice add`
// mints a type=advice bead, which must NOT be auto-parented under the crew's
// task. Advice never closes on the work lifecycle, and the author cannot remove
// the edge afterwards (advice updates need gardenio-admin) — gd-NoMRNVzAkp.
func TestCreateBead_AutoParent_SkipsAdviceBead(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const crewName = "gardenio-weekly-friction-review-advice"
	const taskID = "gd-review-task-ffff"
	seedAgentBead(t, ms, crewName, taskID)

	beadID := createBeadAsAgent(t, h, crewName, map[string]any{
		"title": "Goldens: re-bless only what changed",
		"type":  "advice",
	}, map[string]string{"X-Operated-By": "slack:U0OPERATOR2"})

	if hasParentChildTo(ms, beadID, taskID) {
		t.Fatalf("expected NO auto-parent parent-child edge from the advice bead %s to the task %s; got deps=%+v",
			beadID, taskID, ms.deps[beadID])
	}
	for _, d := range ms.deps[beadID] {
		if d.Type == model.DepParentChild {
			t.Fatalf("expected no auto-parent parent-child edge on a type=advice bead; got %+v", d)
		}
	}
}

// TestCreateBead_AutoParent_StillFiresForWorkBeadFromGardener proves the skip in
// TestCreateBead_AutoParent_SkipsAgentBead is scoped by TYPE, not a blanket
// disable: the SAME gardener (task_id == epic) creating an issue-kind work bead
// (type=task) still gets it auto-parented under the epic. Without this control a
// future over-broad guard (e.g. gating on kind!=issue, which would also kill
// decision/report auto-parenting — bug ka-0000005744) would pass the two skip
// tests above while silently regressing the feature.
func TestCreateBead_AutoParent_StillFiresForWorkBeadFromGardener(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const gardenerName = "gardenio-e2e-coverage-sprint-gardener-work"
	const epicID = "gd-goal-epic-cccc"
	seedAgentBead(t, ms, gardenerName, epicID)

	beadID := createBeadAsAgent(t, h, gardenerName, map[string]any{
		"title": "a real backfill task the gardener filed",
		"type":  "task",
	}, map[string]string{"X-Operated-By": "slack:U0OPERATOR2"})

	if !hasParentChildTo(ms, beadID, epicID) {
		t.Fatalf("expected the gardener's type=task bead %s to be auto-parented under the epic %s; got deps=%+v",
			beadID, epicID, ms.deps[beadID])
	}
}

// TestCreateBead_AutoParent_NoTaskIDOnAgent covers the case where the agent
// bead exists but carries no task_id field — no dep should be added.
func TestCreateBead_AutoParent_NoTaskIDOnAgent(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const agentName = "gardenio-test-agent-notask"
	// Agent bead with empty fields (no task_id).
	ms.beads[agentName] = &model.Bead{
		ID:     agentName,
		Kind:   model.KindConfig,
		Type:   "agent",
		Title:  agentName,
		Status: model.StatusOpen,
	}

	body, _ := json.Marshal(map[string]any{"title": "no-task bead", "type": "task"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", agentName)
	// Post-R2: provide operator explicitly since the agent bead lacks
	// slack_user_id for the resolver fallback.
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR2")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	for _, d := range ms.deps[resp.ID] {
		if d.Type == model.DepParentChild {
			t.Fatalf("expected no auto-parent dep when agent has no task_id; got %+v", d)
		}
	}
}

// TestCreateBead_AutoParent_SkippedWhenExplicitParent covers D5
// (ka-0000007622): when the caller passes parent_bead_id in the create body,
// the server wires that single parent-child dep inside the create transaction
// and SKIPS the static-actor auto-parent step — even if the actor is a known
// agent bead with task_id. Acceptance: the new bead carries exactly one
// parent-child edge, pointing at the explicit parent (the molecule in a
// formula pour), not the agent's claimed task.
func TestCreateBead_AutoParent_SkippedWhenExplicitParent(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const agentName = "gardenio-test-agent-explicit-parent"
	const taskID = "gd-task-explicit-parent"
	seedAgentBead(t, ms, agentName, taskID)

	// Seed the explicit parent (e.g. molecule bead) — must exist so the
	// pre-tx lookup resolves.
	const molID = "gd-molecule-aaaa"
	ms.beads[molID] = &model.Bead{
		ID:     molID,
		Kind:   model.KindIssue,
		Type:   "molecule",
		Title:  "molecule " + molID,
		Status: model.StatusOpen,
	}

	body, _ := json.Marshal(map[string]any{
		"title":          "step bead with explicit parent",
		"type":           "task",
		"parent_bead_id": molID,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", agentName)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, rec.Body.String())
	}

	deps := ms.deps[resp.ID]
	var pcEdges []*model.Dependency
	for _, d := range deps {
		if d.Type == model.DepParentChild {
			pcEdges = append(pcEdges, d)
		}
	}
	if len(pcEdges) != 1 {
		t.Fatalf("expected exactly one parent-child edge; got %d: %+v", len(pcEdges), pcEdges)
	}
	if pcEdges[0].DependsOnID != molID {
		t.Fatalf("expected parent-child target %q (explicit parent), got %q", molID, pcEdges[0].DependsOnID)
	}
	// Belt-and-suspenders: the agent's task must NOT have been wired as a
	// second parent. This is the regression D5 closes.
	for _, d := range deps {
		if d.Type == model.DepParentChild && d.DependsOnID == taskID {
			t.Fatalf("expected no auto-parent dep to agent task when parent_bead_id is set; got %+v", d)
		}
	}
}

// TestCreateBead_ExplicitParent_UnknownParent covers the 400 path: when
// parent_bead_id refers to a bead that doesn't exist, the create is rejected
// with InvalidArgument and no bead is persisted.
func TestCreateBead_ExplicitParent_UnknownParent(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const agentName = "gardenio-test-agent-unknown-parent"
	const taskID = "gd-task-unknown-parent"
	seedAgentBead(t, ms, agentName, taskID)

	body, _ := json.Marshal(map[string]any{
		"title":          "step bead with bogus parent",
		"type":           "task",
		"parent_bead_id": "gd-does-not-exist",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", agentName)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateBead_ExplicitParent_NoAgentActor covers the non-agent path: when
// the caller is not a static-token agent (no auto-parent would fire anyway),
// parent_bead_id still wires the parent-child edge transactionally so the new
// bead's lineage is set in one round-trip.
func TestCreateBead_ExplicitParent_NoAgentActor(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const molID = "gd-molecule-bbbb"
	ms.beads[molID] = &model.Bead{
		ID:     molID,
		Kind:   model.KindIssue,
		Type:   "molecule",
		Title:  "molecule " + molID,
		Status: model.StatusOpen,
	}

	body, _ := json.Marshal(map[string]any{
		"title":          "step bead, no agent",
		"type":           "task",
		"parent_bead_id": molID,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	// No X-Actor — primary token caller has no agent bead, so the
	// auto-parent path is a no-op regardless. We still want the explicit
	// parent edge.
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR2")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, rec.Body.String())
	}

	deps := ms.deps[resp.ID]
	if len(deps) != 1 {
		t.Fatalf("expected exactly one dep, got %d: %+v", len(deps), deps)
	}
	if deps[0].Type != model.DepParentChild || deps[0].DependsOnID != molID {
		t.Fatalf("expected single parent-child dep to %q, got %+v", molID, deps[0])
	}
}

// TestCreateBead_AutoParent_NoAgentBead covers the case where X-Actor names an
// identifier with no matching agent bead — no dep should be added and the
// create still succeeds.
func TestCreateBead_AutoParent_NoAgentBead(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	body, _ := json.Marshal(map[string]any{"title": "no-agent bead", "type": "task"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", "unknown-agent-no-bead")
	// Post-R2: provide operator since the X-Actor has no agent bead to
	// fall back to.
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR2")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(ms.deps[resp.ID]) != 0 {
		t.Fatalf("expected no deps when actor has no agent bead; got %+v", ms.deps[resp.ID])
	}
}

// --- Transitive thread_id (wish-membership) copy — ka-0000007300 ---

// seedWishAgentBead inserts a thread-spawned agent carrying the origin-agnostic
// thread_id wish pointer (Threads initiative 1a-1) and seeds the wish head bead
// it points at. Unlike seedThreadAgentBead it uses thread_id (not just the
// legacy slack_thread_bead_id alias) so the transitive-copy ladder is exercised
// on the canonical field.
func seedWishAgentBead(t *testing.T, ms *mockStore, name, wishID string) {
	t.Helper()
	fields := map[string]any{
		"thread_id":     wishID,
		"slack_user_id": "U0OPERATOR2",
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}
	ms.beads[name] = &model.Bead{
		ID: name, Kind: model.KindConfig, Type: "agent",
		Title: name, Status: model.StatusOpen, Fields: raw,
	}
	// The wish head carries the bare `thread` label and NO thread_id (it IS
	// the wish). Seed it so the auto-parent dep target + cross-wish lookup
	// resolve.
	ms.beads[wishID] = &model.Bead{
		ID: wishID, Kind: model.KindIssue, Type: "task",
		Title: "wish head " + wishID, Status: model.StatusOpen,
	}
	ms.labels[wishID] = []string{threadHeadLabel}
}

func createBeadAsAgent(t *testing.T, h http.Handler, agentName string, body map[string]any, extraHeaders map[string]string) string {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", agentName)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, rec.Body.String())
	}
	if resp.ID == "" {
		t.Fatalf("expected bead id in response; body: %s", rec.Body.String())
	}
	return resp.ID
}

func threadIDOf(t *testing.T, ms *mockStore, beadID string) string {
	t.Helper()
	b := ms.beads[beadID]
	if b == nil {
		t.Fatalf("bead %s not found in store", beadID)
	}
	return threadIDFromFields(b.Fields)
}

func hasParentChildTo(ms *mockStore, beadID, target string) bool {
	for _, d := range ms.deps[beadID] {
		if d.Type == model.DepParentChild && d.DependsOnID == target {
			return true
		}
	}
	return false
}

// TestCreateBead_ThreadIDCopy_DepAndField is the primary acceptance case: an
// agent carrying thread_id=gd-W creates a bead → the new bead gets BOTH a
// parent-child dep to gd-W AND fields.thread_id == gd-W.
func TestCreateBead_ThreadIDCopy_DepAndField(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const agentName = "thread-1780289972-wish"
	const wishID = "gd-wish-WWWW"
	seedWishAgentBead(t, ms, agentName, wishID)

	id := createBeadAsAgent(t, h, agentName, map[string]any{
		"title": "derivative in wish", "type": "task",
	}, nil)

	if !hasParentChildTo(ms, id, wishID) {
		t.Fatalf("expected parent-child dep to wish %s; got deps=%+v", wishID, ms.deps[id])
	}
	if got := threadIDOf(t, ms, id); got != wishID {
		t.Fatalf("expected fields.thread_id=%q, got %q", wishID, got)
	}
}

// TestCreateBead_ThreadIDCopy_GrandchildInherits proves transitivity: a bead
// created by an agent working a DERIVATIVE (which already carries thread_id)
// inherits the same wish — the grandchild copy.
func TestCreateBead_ThreadIDCopy_GrandchildInherits(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const wishID = "gd-wish-GGGG"

	// First agent works the wish head directly and creates a child.
	const agent1 = "thread-1780289972-gc1"
	seedWishAgentBead(t, ms, agent1, wishID)
	childID := createBeadAsAgent(t, h, agent1, map[string]any{
		"title": "child task", "type": "task",
	}, nil)
	if threadIDOf(t, ms, childID) != wishID {
		t.Fatalf("precondition: child should carry thread_id=%s", wishID)
	}

	// A second agent is spawned ON that child (task_id=childID) and ALSO
	// carries thread_id forward (the controller spawn-inherit, 1b-1). It
	// creates a grandchild, which must inherit the same wish.
	const agent2 = "gardenio-grandchild-agent"
	fields := map[string]any{
		"task_id":       childID,
		"thread_id":     wishID,
		"slack_user_id": "U0OPERATOR2",
	}
	raw, _ := json.Marshal(fields)
	ms.beads[agent2] = &model.Bead{
		ID: agent2, Kind: model.KindConfig, Type: "agent",
		Title: agent2, Status: model.StatusOpen, Fields: raw,
	}

	grandchildID := createBeadAsAgent(t, h, agent2, map[string]any{
		"title": "grandchild task", "type": "task",
	}, nil)

	if got := threadIDOf(t, ms, grandchildID); got != wishID {
		t.Fatalf("expected grandchild fields.thread_id=%q (transitive inherit), got %q", wishID, got)
	}
	// Dep target follows the task_id ladder rung (the claimed child), not the
	// wish head — both are correct lineage.
	if !hasParentChildTo(ms, grandchildID, childID) {
		t.Fatalf("expected grandchild parent-child dep to claimed task %s; got %+v", childID, ms.deps[grandchildID])
	}
}

// TestCreateBead_ThreadIDCopy_SuppressedByHeader proves X-No-Auto-Parent: 1
// suppresses BOTH the dep and the field copy.
func TestCreateBead_ThreadIDCopy_SuppressedByHeader(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const agentName = "thread-1780289972-supp"
	const wishID = "gd-wish-SUPP"
	seedWishAgentBead(t, ms, agentName, wishID)

	id := createBeadAsAgent(t, h, agentName, map[string]any{
		"title": "suppressed", "type": "task",
	}, map[string]string{"X-No-Auto-Parent": "1"})

	if hasParentChildTo(ms, id, wishID) {
		t.Fatalf("expected no auto-parent dep under X-No-Auto-Parent; got %+v", ms.deps[id])
	}
	if got := threadIDOf(t, ms, id); got != "" {
		t.Fatalf("expected no thread_id copy under X-No-Auto-Parent; got %q", got)
	}
}

// TestCreateBead_ThreadIDCopy_CrossWishSkips proves the cross-wish guard:
// agent works wish A but files a bead explicitly parented under a bead in wish
// B → the new bead is NOT stamped with wish A. (It binds to B via the explicit
// edge.)
func TestCreateBead_ThreadIDCopy_CrossWishSkips(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const agentName = "thread-1780289972-cross"
	const wishA = "gd-wish-AAAA"
	seedWishAgentBead(t, ms, agentName, wishA)

	// A member bead of a DIFFERENT wish B, used as the explicit parent.
	const wishB = "gd-wish-BBBB"
	const parentInB = "gd-member-of-B"
	bFields, _ := json.Marshal(map[string]any{"thread_id": wishB})
	ms.beads[parentInB] = &model.Bead{
		ID: parentInB, Kind: model.KindIssue, Type: "task",
		Title: "member of wish B", Status: model.StatusOpen, Fields: bFields,
	}

	id := createBeadAsAgent(t, h, agentName, map[string]any{
		"title":          "cross-wish derivative",
		"type":           "task",
		"parent_bead_id": parentInB,
	}, nil)

	// Explicit parent edge to B's member is present (the canonical lineage).
	if !hasParentChildTo(ms, id, parentInB) {
		t.Fatalf("expected explicit parent edge to %s; got %+v", parentInB, ms.deps[id])
	}
	// The agent's wish A must NOT have been stamped (cross-wish skip).
	if got := threadIDOf(t, ms, id); got == wishA {
		t.Fatalf("cross-wish mis-bind: new bead stamped with agent wish %q despite explicit parent in wish %q", wishA, wishB)
	}
}

// TestCreateBead_ThreadIDCopy_HalfLinkStillReachable proves Guard 3 (half-link
// durability): the auto-parent dep is written FIRST and independently, so when
// the field copy fails (UpdateBead errors) the bead is still dep-linked to the
// wish and therefore reachable by a parent-child-closure rollup.
func TestCreateBead_ThreadIDCopy_HalfLinkStillReachable(t *testing.T) {
	ms := newMockStore()
	ms.updateBeadErr = errors.New("simulated fields write failure")
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const agentName = "thread-1780289972-half"
	const wishID = "gd-wish-HALF"
	seedWishAgentBead(t, ms, agentName, wishID)

	id := createBeadAsAgent(t, h, agentName, map[string]any{
		"title": "half-linked", "type": "task",
	}, nil)

	// Field copy failed (UpdateBead errored), so no thread_id field...
	if got := threadIDOf(t, ms, id); got != "" {
		t.Fatalf("expected no thread_id field after UpdateBead failure; got %q", got)
	}
	// ...but the durable parent-child dep to the wish is still present, so the
	// bead remains reachable via the dep-closure UNION in the rollup query.
	if !hasParentChildTo(ms, id, wishID) {
		t.Fatalf("expected durable parent-child dep to wish %s despite field-copy failure; got %+v", wishID, ms.deps[id])
	}
}

// TestCreateBead_ThreadIDCopy_WishHeadNotStamped proves a bead that is itself a
// wish head (carries the bare `thread` label) is never given a thread_id — a
// head IS the wish and must not point at another.
func TestCreateBead_ThreadIDCopy_WishHeadNotStamped(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const agentName = "thread-1780289972-head"
	const wishID = "gd-wish-HEAD"
	seedWishAgentBead(t, ms, agentName, wishID)

	id := createBeadAsAgent(t, h, agentName, map[string]any{
		"title":  "a brand new wish head",
		"type":   "task",
		"labels": []string{threadHeadLabel},
	}, nil)

	if got := threadIDOf(t, ms, id); got != "" {
		t.Fatalf("expected no thread_id on a wish-head bead; got %q", got)
	}
}

// TestCreateBead_ThreadIDCopy_OIDCBypass proves Guard 1's identity table: an
// OIDC (human / Phase-2 composer) caller does NOT trigger the daemon-side copy
// — the composer stamps thread_id itself.
func TestCreateBead_ThreadIDCopy_OIDCBypass(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})

	// An agent bead carrying a wish exists, but the caller authenticates as
	// OIDC (no X-Actor static path), so the auto-parent step is bypassed.
	const agentName = "thread-1780289972-oidc"
	const wishID = "gd-wish-OIDC"
	seedWishAgentBead(t, ms, agentName, wishID)

	// Simulate an OIDC identity by injecting it on the context createBead
	// sees. The HTTP interceptor would set static for a primary-token call;
	// to exercise the OIDC branch we drive createBead through a context that
	// already carries an OIDC identity.
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "matthew.baker@acme.example"})
	bead, err := s.createBead(ctx, createBeadInput{Title: "oidc-created", Type: "task"})
	if err != nil {
		t.Fatalf("createBead under OIDC failed: %v", err)
	}
	if got := threadIDFromFields(bead.Fields); got != "" {
		t.Fatalf("expected no daemon-side thread_id copy for OIDC caller; got %q", got)
	}
	if len(ms.deps[bead.ID]) != 0 {
		t.Fatalf("expected no auto-parent dep for OIDC caller; got %+v", ms.deps[bead.ID])
	}
}
