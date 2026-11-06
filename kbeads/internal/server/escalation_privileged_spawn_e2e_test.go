package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// TestPrivilegedSpawnEscalationReplayE2E is the regression test for
// gd-zErNIecGIc, reproducing the first real privileged spawn (role `infra`,
// escalation gd-XJfWq5QNOY, 2026-09-21): a non-OIDC caller is denied a
// privileged-role spawn as escalatable, files the escalation, an operator
// approves it through the static relay path beads-viewer and kd use
// (X-Operated-By), and the replay must actually create the agent bead instead
// of re-entering authorizeNonHumanSpawn and failing with the same denial.
//
// TestApproveEscalation_ReplayCreate_StampsSpawnProvenance never caught this:
// its store holds no privileged role bead, so the gate never fires there.
func TestPrivilegedSpawnEscalationReplayE2E(t *testing.T) {
	const (
		adminToken   = "admin-priv-spawn-e2e"
		adminOIDCTok = "oidc-admin-priv-spawn-e2e"
		fleetName    = "priv-spawn-fleet"
		operator     = "oidc:admin@example.com"
	)
	fc := NewFleetConfig(fleetName, nil)
	ts, _, ms := newEscalationE2EServer(t, adminToken, fc, &escOIDCMock{tokens: map[string]escTokenInfo{
		adminOIDCTok: {email: "admin@example.com", roles: []string{RoleAdmin}},
	}})
	makeRoleBead(ms, "gd-role-infra", "infra", true)

	// The real spawn client (beadsapi.Client.SpawnAgentResult) posts the agent
	// create with NO labels and adds project:/role: afterwards — follow-ups a
	// denied-and-escalated create never runs. The replay must still produce a
	// labelled bead, or the agent's project-scoped token cannot read or stop
	// its own bead (gd-9uztBqkBTn, gd-gm5XivuyTB, gd-Qixci61Q3Q).
	agentCreate := map[string]any{
		"title": "gardenio-infra-capacity-investigation-e2e",
		"type":  "agent",
		"fields": map[string]any{
			"agent":   "gardenio-infra-capacity-investigation-e2e",
			"mode":    "crew",
			"role":    "infra",
			"project": "gardenio",
		},
	}
	relay := map[string]string{"X-Operated-By": operator}

	// 1. The static (non-OIDC) caller is denied, escalatable, naming the role.
	resp := escRequest(t, ts, "POST", "/v1/beads", adminToken, agentCreate, relay)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("direct privileged spawn: expected 403, got %d: %s", resp.StatusCode, body)
	}
	var denial AuthzDenial
	if err := json.Unmarshal(body, &denial); err != nil {
		t.Fatalf("decoding denial: %v", err)
	}
	if !denial.Escalatable || denial.SpawnRole != "infra" {
		t.Fatalf("denial should be escalatable for role infra, got %+v", denial)
	}

	// 2. --escalate files the escalation bead.
	resp = escRequest(t, ts, "POST", "/v1/beads", adminToken, agentCreate,
		map[string]string{"X-Operated-By": operator, "X-Escalate": "true"})
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("escalate: expected 202, got %d: %s", resp.StatusCode, body)
	}
	var escResp EscalationResponse
	escDecode(t, resp, &escResp)
	resp.Body.Close()
	if escResp.EscalationID == "" {
		t.Fatal("expected an escalation id")
	}

	// 3. The operator approves with their own OIDC identity (gd-ZjiSd9Kzio
	//    round 3 removed the static-token X-Operated-By relay); the replay
	//    must succeed and produce the agent bead.
	resp = escRequest(t, ts, "POST", "/v1/escalations/"+escResp.EscalationID+"/approve", adminOIDCTok,
		map[string]any{"approval_method": "cli"}, nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("approve: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var approveResp map[string]any
	escDecode(t, resp, &approveResp)
	resp.Body.Close()

	replay, _ := approveResp["replay"].(map[string]any)
	if replay == nil {
		t.Fatalf("expected a replay result, got %v", approveResp)
	}
	if replay["status"] != "success" {
		t.Fatalf("replay status=%v (error=%v), want success — the approved privileged spawn was denied again", replay["status"], replay["error"])
	}
	beadID, _ := replay["bead_id"].(string)
	if beadID == "" {
		t.Fatal("replay should return the created agent bead id")
	}
	agent, err := ms.GetBead(context.Background(), beadID)
	if err != nil || agent == nil {
		t.Fatalf("replayed agent bead not found: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(agent.Fields, &fields); err != nil {
		t.Fatalf("decoding agent fields: %v", err)
	}
	if fields["role"] != "infra" || fields["spawned_by"] != escResp.EscalationID || fields["spawn_approval_mode"] != "human" {
		t.Errorf("agent fields = %v, want role infra, spawned_by %s, spawn_approval_mode human", fields, escResp.EscalationID)
	}
	if !hasLabel(agent.Labels, "project:gardenio") || !hasLabel(agent.Labels, "role:infra") {
		t.Errorf("replayed agent labels = %v, want project:gardenio and role:infra derived from fields (the agent's own token is project-scoped)", agent.Labels)
	}

	// 4. The escalation is closed as a success, not left in_progress/failure.
	esc, err := ms.GetBead(context.Background(), escResp.EscalationID)
	if err != nil || esc == nil {
		t.Fatalf("escalation bead not found: %v", err)
	}
	if esc.Status != model.StatusClosed {
		t.Errorf("escalation status=%q, want closed", esc.Status)
	}
	var escFields map[string]json.RawMessage
	_ = json.Unmarshal(esc.Fields, &escFields)
	var resultStatus string
	_ = json.Unmarshal(escFields["result_status"], &resultStatus)
	if resultStatus != "success" {
		t.Errorf("result_status=%q, want success", resultStatus)
	}

	// 5. The marker is scoped to the replay: a fresh direct spawn of the same
	//    privileged role by the same caller is still denied.
	resp = escRequest(t, ts, "POST", "/v1/beads", adminToken, agentCreate, relay)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("direct privileged spawn after an approval: expected 403, got %d", resp.StatusCode)
	}
}

// TestPrivilegedSpawnEscalationReplay_PatchedRoleRefused is the regression
// test for gd-UQtYJZ6RgQ (should-fix from the gd-zErNIecGIc review):
// operation_spec is a generic bead field on the escalation, patchable via
// PATCH /v1/beads/{id} between filing and approval. If the operator approves
// what the inbox showed them (spawn_role, stamped at denial time) while
// operation_spec.fields.role has since been changed to a different — and
// possibly more privileged — role, replay must refuse rather than spawn the
// patched role under an approval that never saw it.
func TestPrivilegedSpawnEscalationReplay_PatchedRoleRefused(t *testing.T) {
	const (
		adminToken   = "admin-priv-spawn-patched-e2e"
		adminOIDCTok = "oidc-admin-priv-spawn-patched-e2e"
		fleetName    = "priv-spawn-patched-fleet"
		operator     = "oidc:admin@example.com"
	)
	fc := NewFleetConfig(fleetName, nil)
	ts, _, ms := newEscalationE2EServer(t, adminToken, fc, &escOIDCMock{tokens: map[string]escTokenInfo{
		adminOIDCTok: {email: "admin@example.com", roles: []string{RoleAdmin}},
	}})
	makeRoleBead(ms, "gd-role-infra2", "infra", true)
	makeRoleBead(ms, "gd-role-k8slive", "k8s-live", true)

	agentCreate := map[string]any{
		"title":  "gardenio-infra-capacity-investigation-patched-e2e",
		"type":   "agent",
		"labels": []string{"project:gardenio"},
		"fields": map[string]any{
			"agent":   "gardenio-infra-capacity-investigation-patched-e2e",
			"mode":    "crew",
			"role":    "infra",
			"project": "gardenio",
		},
	}
	relay := map[string]string{"X-Operated-By": operator}

	// 1. Denied as escalatable for role infra, exactly as in the base case.
	resp := escRequest(t, ts, "POST", "/v1/beads", adminToken, agentCreate, relay)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("direct privileged spawn: expected 403, got %d", resp.StatusCode)
	}

	// 2. --escalate files the escalation bead; the inbox will show spawn_role=infra.
	resp = escRequest(t, ts, "POST", "/v1/beads", adminToken, agentCreate,
		map[string]string{"X-Operated-By": operator, "X-Escalate": "true"})
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("escalate: expected 202, got %d: %s", resp.StatusCode, body)
	}
	var escResp EscalationResponse
	escDecode(t, resp, &escResp)
	resp.Body.Close()
	if escResp.EscalationID == "" {
		t.Fatal("expected an escalation id")
	}

	// 3. Before the operator approves, operation_spec is changed so the role it names diverges from spawn_role —
	// the field the approver actually saw.
	patchedCreate := map[string]any{
		"title":  agentCreate["title"],
		"type":   "agent",
		"labels": []string{"project:gardenio"},
		"fields": map[string]any{
			"agent":   "gardenio-infra-capacity-investigation-patched-e2e",
			"mode":    "crew",
			"role":    "k8s-live",
			"project": "gardenio",
		},
	}
	patchBody := map[string]any{
		"fields": map[string]any{
			"operation_spec": patchedCreate,
		},
	}
	resp = escRequest(t, ts, "PATCH", "/v1/beads/"+escResp.EscalationID, adminToken, patchBody, relay)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("patch operation_spec: a non-admin requester must be refused (400), got %d: %s", resp.StatusCode, body)
	}

	// The API refuses the patch (gd-4gdUwaWr6i), so write the divergence
	// straight to the store: replay's own role check is the second line of
	// defence for an operation_spec amended by any other route.
	stored := ms.beads[escResp.EscalationID]
	var storedFields map[string]json.RawMessage
	_ = json.Unmarshal(stored.Fields, &storedFields)
	storedFields["operation_spec"] = mustMarshal(patchedCreate)
	stored.Fields, _ = json.Marshal(storedFields)

	// 4. The operator approves what they saw (spawn_role=infra), using their
	// own OIDC identity. The replay must refuse — not spawn k8s-live under
	// an approval that never saw it.
	resp = escRequest(t, ts, "POST", "/v1/escalations/"+escResp.EscalationID+"/approve", adminOIDCTok,
		map[string]any{"approval_method": "cli"}, nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("approve: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var approveResp map[string]any
	escDecode(t, resp, &approveResp)
	resp.Body.Close()

	replay, _ := approveResp["replay"].(map[string]any)
	if replay == nil {
		t.Fatalf("expected a replay result, got %v", approveResp)
	}
	if replay["status"] != "failure" {
		t.Fatalf("replay status=%v, want failure — the patched role must not be spawned under an approval that never saw it", replay["status"])
	}

	// No agent bead named by the patched spec was created.
	beads, _, _ := ms.ListBeads(context.Background(), model.BeadFilter{Type: []model.BeadType{"agent"}})
	for _, b := range beads {
		if b.Title == "gardenio-infra-capacity-investigation-patched-e2e" {
			t.Fatalf("replay must not have created an agent bead, found %s", b.ID)
		}
	}

	// The escalation records the failure rather than silently vanishing.
	esc, err := ms.GetBead(context.Background(), escResp.EscalationID)
	if err != nil || esc == nil {
		t.Fatalf("escalation bead not found: %v", err)
	}
	var escFields map[string]json.RawMessage
	_ = json.Unmarshal(esc.Fields, &escFields)
	var resultStatus string
	_ = json.Unmarshal(escFields["result_status"], &resultStatus)
	if resultStatus != "failure" {
		t.Errorf("result_status=%q, want failure", resultStatus)
	}
}

// TestPrivilegedSpawnInfraAgentLifecycleE2E is the end-to-end test for
// gd-MxjkdMOvPi (gd-vCFepmDZoL items 1 and 4): an infra agent spawned through
// the denied-then-approved replay path must be able to do its whole job with
// its OWN per-agent token — read and stamp its agent bead, comment on the task
// it was spawned for, set stop_requested and close — so it never has to file
// a "Despawn infra agent" escalation (gd-xkU3xGFYY2). Before gd-Qixci61Q3Q the
// replayed bead had no project label and every one of these calls was 403
// "token not authorized for this bead's project".
//
// The spawn is requested the way it is on k3s, by a crew on its own per-agent
// token, for a task that crew filed. Before gd-G5PmhWXsF9 the replay never ran
// the client's assigned-dep and pre-claim follow-ups, so the infra agent's
// `kd claim` and `kd close` of that task were 403 "ownership denied".
func TestPrivilegedSpawnInfraAgentLifecycleE2E(t *testing.T) {
	const (
		adminToken   = "admin-priv-spawn-lifecycle-e2e"
		adminOIDCTok = "oidc-admin-priv-spawn-lifecycle-e2e"
		fleetName    = "priv-spawn-lifecycle-fleet"
		operator     = "oidc:admin@example.com"
		agentName    = "crew-gardenio-infra-lifecycle-e2e"
		agentTok     = "agent-tok-priv-spawn-lifecycle-e2e"
		taskID       = "gd-infra-spawn-task"
		requesterID  = "gd-requester-crew"
		requester    = "gardenio-infrarole-agents-can-e2e"
		requesterTok = "agent-tok-requester-crew-e2e"
	)
	fc := NewFleetConfig(fleetName, nil)
	ts, _, ms := newEscalationE2EServer(t, adminToken, fc, &escOIDCMock{tokens: map[string]escTokenInfo{
		adminOIDCTok: {email: "admin@example.com", roles: []string{RoleAdmin}},
	}})
	makeRoleBead(ms, "gd-role-infra-lifecycle", "infra", true)
	// The crew that requests the spawn, on its own per-agent token, and the
	// open task it filed and spawns the infra agent for.
	seedBead(ms, requesterID, "agent", model.StatusOpen, []string{"project:gardenio", "role:crew"},
		map[string]any{"agent": requester, "role": "crew", "project": "gardenio", "operated_by": operator})
	fc.RegisterTokenFull(requesterTok, agentFleetPrefix+requesterID, "gardenio", nil, nil,
		time.Now().Add(24*time.Hour), requester, false)
	seedBead(ms, taskID, model.TypeTask, model.StatusOpen, []string{"project:gardenio"}, nil)
	ms.beads[taskID].CreatedBy = requester

	// The body beadsapi.Client.SpawnAgentResult posts for
	// `gb spawn --role infra --escalate --task <task>`: no labels, task_id in
	// fields. project:/role: and the assigned dep are follow-ups a denied
	// create never runs.
	agentCreate := map[string]any{
		"title":       agentName,
		"type":        "agent",
		"description": "Assigned to task: " + taskID,
		"fields": map[string]any{
			"agent":    agentName,
			"mode":     "crew",
			"role":     "infra",
			"project":  "gardenio",
			"task_id":  taskID,
			"seat_key": "gardenio/infra/crew-gardenio-infra",
		},
	}

	// 1. Denied, 2. escalated, 3. approved and replayed.
	resp := escRequest(t, ts, "POST", "/v1/beads", requesterTok, agentCreate, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("direct privileged spawn: expected 403, got %d", resp.StatusCode)
	}
	resp = escRequest(t, ts, "POST", "/v1/beads", requesterTok, agentCreate,
		map[string]string{"X-Escalate": "true"})
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("escalate: expected 202, got %d: %s", resp.StatusCode, body)
	}
	var escResp EscalationResponse
	escDecode(t, resp, &escResp)
	resp.Body.Close()
	resp = escRequest(t, ts, "POST", "/v1/escalations/"+escResp.EscalationID+"/approve", adminOIDCTok,
		map[string]any{"approval_method": "cli"}, nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("approve: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var approveResp map[string]any
	escDecode(t, resp, &approveResp)
	resp.Body.Close()
	replay, _ := approveResp["replay"].(map[string]any)
	agentID, _ := replay["bead_id"].(string)
	if replay["status"] != "success" || agentID == "" {
		t.Fatalf("replay = %v, want success with the agent bead id", replay)
	}
	// The replay ran the spawn client's follow-ups: the task is pre-claimed
	// for the infra agent, and the agent bead holds the assigned dep to it.
	if replay["task_claim"] != spawnTaskClaimed {
		t.Errorf("replay task_claim = %v, want %q", replay["task_claim"], spawnTaskClaimed)
	}
	if task, _ := ms.GetBead(context.Background(), taskID); task == nil ||
		task.Assignee != agentName || task.Status != model.StatusInProgress {
		t.Errorf("spawn task after replay = %+v, want in_progress assigned to %s", task, agentName)
	}
	if !hasDep(ms, agentID, taskID, assignedDependencyType) {
		t.Errorf("agent bead %s has no assigned dep to %s", agentID, taskID)
	}
	// The dep carries the operator FK that createBead stamped on the agent
	// bead; Postgres refuses a NULL deps.operated_by_id (ka-0000007722).
	for _, d := range ms.deps[agentID] {
		if d.DependsOnID == taskID && d.Type == assignedDependencyType && d.OperatedByID == nil {
			t.Errorf("assigned dep %s -> %s has a nil OperatedByID", agentID, taskID)
		}
	}

	// The controller mints the pod's token exactly like this (provisionAgentToken):
	// fleet agent-<bead>, project = fields.project, subject = agent name.
	fc.RegisterTokenFull(agentTok, agentFleetPrefix+agentID, "gardenio", nil, nil,
		time.Now().Add(24*time.Hour), agentName, false)

	// asAgent issues one request with the agent's own token and requires 2xx.
	asAgent := func(step, method, path string, body any) {
		t.Helper()
		resp := escRequest(t, ts, method, path, agentTok, body, nil)
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s (%s %s) with the agent's own token: got %d: %s — an agent that cannot do this files a despawn escalation", step, method, path, resp.StatusCode, b)
		}
	}

	// gb agent start: isStopRequested reads the agent bead.
	asAgent("isStopRequested", "GET", "/v1/beads/"+agentID, nil)
	// gb init: stampLastSessionID / last_progress_at.
	asAgent("stamp last_session_id", "PATCH", "/v1/beads/"+agentID,
		map[string]any{"fields": map[string]any{"last_session_id": "sess-1", "agent_state": "working"}})
	// The one-paragraph result on the task it was spawned for.
	asAgent("comment on spawn task", "POST", "/v1/beads/"+taskID+"/comments",
		map[string]any{"author": agentName, "text": "Read-only check done: nodes healthy."})
	// kd claim (idempotent after the pre-claim), then kd close of the task.
	asAgent("kd claim spawn task", "PATCH", "/v1/beads/"+taskID,
		map[string]any{"status": "in_progress", "assignee": agentName})
	asAgent("kd close spawn task", "POST", "/v1/beads/"+taskID+"/close",
		map[string]any{"closed_by": agentName})
	// gb stop: stop_requested, then close the agent bead.
	asAgent("gb stop: stop_requested", "PATCH", "/v1/beads/"+agentID,
		map[string]any{"fields": map[string]any{"stop_requested": "true"}})
	asAgent("gb stop: close agent bead", "POST", "/v1/beads/"+agentID+"/close",
		map[string]any{"closed_by": agentName, "agent_state": "done", "gate_satisfied_by": "operator"})

	// The comment landed, attributed to the agent.
	comments, err := ms.GetComments(context.Background(), taskID)
	if err != nil || len(comments) != 1 || comments[0].Author != agentName {
		t.Errorf("spawn task comments = %+v (err %v), want one comment by %s", comments, err, agentName)
	}
	if task, _ := ms.GetBead(context.Background(), taskID); task == nil || task.Status != model.StatusClosed {
		t.Errorf("spawn task = %+v, want closed by the infra agent", task)
	}
	// The agent bead is closed with stop_requested set: the pod exits on its own.
	agent, err := ms.GetBead(context.Background(), agentID)
	if err != nil || agent == nil {
		t.Fatalf("agent bead not found: %v", err)
	}
	if agent.Status != model.StatusClosed {
		t.Errorf("agent bead status = %q, want closed", agent.Status)
	}
	var agentFields map[string]any
	_ = json.Unmarshal(agent.Fields, &agentFields)
	if agentFields["stop_requested"] != "true" {
		t.Errorf("agent stop_requested = %v, want \"true\"", agentFields["stop_requested"])
	}

	// No despawn escalation: the only escalation is the spawn approval, closed.
	escs, _, err := ms.ListBeads(context.Background(), model.BeadFilter{Type: []model.BeadType{model.TypeEscalation}})
	if err != nil {
		t.Fatalf("listing escalations: %v", err)
	}
	for _, e := range escs {
		if e.ID != escResp.EscalationID {
			t.Errorf("unexpected escalation %s %q: the infra agent's lifecycle needed operator involvement", e.ID, e.Title)
		} else if e.Status != model.StatusClosed {
			t.Errorf("spawn escalation %s status = %q, want closed", e.ID, e.Status)
		}
	}
}

// hasDep reports whether ms holds a beadID -> dependsOnID dependency of typ.
func hasDep(ms *mockStore, beadID, dependsOnID string, typ model.DependencyType) bool {
	for _, d := range ms.deps[beadID] {
		if d.DependsOnID == dependsOnID && d.Type == typ {
			return true
		}
	}
	return false
}
