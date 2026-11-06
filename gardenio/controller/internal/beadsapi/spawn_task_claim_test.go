package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// claimDaemon is a fake daemon for ClaimSpawnTask: it serves one agent bead,
// one task bead, a set of agents by name and status, and records the PATCHes
// it receives on each.
type claimDaemon struct {
	agent        map[string]any
	task         map[string]any
	agents       map[string]string // agent name -> status of its newest bead
	patchCode    int
	patches      []map[string]any // on the task
	agentPatches []map[string]any // on the agent bead
}

func (d *claimDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/gd-agent":
		_ = json.NewEncoder(w).Encode(d.agent)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/gd-task":
		_ = json.NewEncoder(w).Encode(d.task)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/"):
		name := strings.TrimPrefix(r.URL.Path, "/v1/agents-by-name/")
		status, ok := d.agents[name]
		// The daemon filters by the requested statuses; "closed" is asked only
		// on the fallback lookup.
		wantClosed := r.URL.Query().Get("status") == "closed"
		if !ok || (status == "closed") != wantClosed {
			http.Error(w, `{"error":"agent not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-agent-" + name, "type": "agent", "status": status})
	case r.Method == http.MethodPatch && r.URL.Path == "/v1/beads/gd-agent":
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		d.agentPatches = append(d.agentPatches, parsed)
		_ = json.NewEncoder(w).Encode(d.agent)
	case r.Method == http.MethodPatch && r.URL.Path == "/v1/beads/gd-task":
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		d.patches = append(d.patches, parsed)
		if d.patchCode != 0 {
			http.Error(w, `{"error":"ownership denied"}`, d.patchCode)
			return
		}
		_ = json.NewEncoder(w).Encode(d.task)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
	}
}

// singleClaimAgent is the agent bead a `gb spawn --task gd-task` creates,
// spawned by a thread agent that does not own gd-task.
func singleClaimAgent(extra map[string]any) map[string]any {
	fields := map[string]any{
		"agent": "crew-new", "project": "gardenio", "task_id": "gd-task",
		FieldClaimTaskAtSpawn: "true",
	}
	for k, v := range extra {
		fields[k] = v
	}
	return map[string]any{"id": "gd-agent", "type": "agent", "status": "open", "created_by": "thread-1", "fields": fields}
}

// taskBead builds a task bead in the gardenio project, created by another thread.
func taskBead(kv ...any) map[string]any {
	t := map[string]any{"id": "gd-task", "type": "task", "status": "open", "created_by": "thread-2", "labels": []string{"project:gardenio"}}
	for i := 0; i+1 < len(kv); i += 2 {
		t[kv[i].(string)] = kv[i+1]
	}
	return t
}

type claimSpawnTaskCase struct {
	name      string
	agent     map[string]any
	task      map[string]any
	agents    map[string]string
	patchCode int
	want      string
	wantErr   bool
	wantPatch bool
	wantStamp bool
	// wantClearReaped: the claim patch deletes ReapedAssigneeField.
	wantClearReaped bool
}

var claimSpawnTaskCases = []claimSpawnTaskCase{
	{
		name:      "unassigned open task is claimed",
		task:      taskBead(),
		want:      SpawnTaskClaimed,
		wantPatch: true,
		wantStamp: true,
	},
	{
		name:      "task held by a dead crew is reclaimed",
		task:      taskBead("status", "in_progress", "assignee", "crew-b7m"),
		agents:    map[string]string{"crew-b7m": "closed"},
		want:      SpawnTaskClaimed,
		wantPatch: true,
		wantStamp: true,
	},
	{
		name:      "bug held by a dead crew is reclaimed",
		task:      taskBead("type", "bug", "status", "in_progress", "assignee", "crew-b7m"),
		agents:    map[string]string{"crew-b7m": "closed"},
		want:      SpawnTaskClaimed,
		wantPatch: true,
		wantStamp: true,
	},
	{
		name:      "task the spawner created, outside the agent's project, is claimed",
		task:      taskBead("created_by", "thread-1", "labels", []string{"project:other"}),
		want:      SpawnTaskClaimed,
		wantPatch: true,
		wantStamp: true,
	},
	{
		name:      "task in another project the spawner did not create is left unclaimed",
		task:      taskBead("labels", []string{"project:other"}),
		want:      SpawnTaskOutOfScope,
		wantStamp: true,
	},
	{
		name:      "task with no project label the spawner did not create is left unclaimed",
		task:      taskBead("labels", []string{}),
		want:      SpawnTaskOutOfScope,
		wantStamp: true,
	},
	{
		name:  "spawn that did not opt into single-claim is left alone",
		agent: singleClaimAgent(map[string]any{FieldClaimTaskAtSpawn: nil}),
		task:  taskBead(),
		want:  SpawnTaskNotSingleClaim,
	},
	{
		name:  "a recreate does not claim again",
		agent: singleClaimAgent(map[string]any{FieldSpawnTaskClaim: SpawnTaskClaimed}),
		task:  taskBead(),
		want:  SpawnTaskRecreate,
	},
	{
		name:      "task held by a live agent is left alone",
		task:      taskBead("status", "in_progress", "assignee", "crew-live"),
		agents:    map[string]string{"crew-live": "in_progress"},
		want:      SpawnTaskLiveAssignee,
		wantStamp: true,
	},
	{
		name:      "task held by a human is left alone",
		task:      taskBead("status", "in_progress", "assignee", "someone@example.com"),
		want:      SpawnTaskLiveAssignee,
		wantStamp: true,
	},
	{
		name:      "task held by a name with no agent bead is left alone",
		task:      taskBead("status", "in_progress", "assignee", "laptop-claude"),
		want:      SpawnTaskLiveAssignee,
		wantStamp: true,
	},
	{
		name:      "already assigned to the agent",
		task:      taskBead("status", "in_progress", "assignee", "crew-new"),
		want:      SpawnTaskAlreadyAssigned,
		wantStamp: true,
	},
	{
		name:      "unassigned bug is claimed under DispatchClaim",
		task:      taskBead("type", "bug"),
		want:      SpawnTaskClaimed,
		wantPatch: true,
		wantStamp: true,
	},
	{
		name:            "reaped bug is claimed and sheds its reap record",
		task:            taskBead("type", "bug", "fields", map[string]any{ReapedAssigneeField: "crew-b7m"}),
		want:            SpawnTaskClaimed,
		wantPatch:       true,
		wantStamp:       true,
		wantClearReaped: true,
	},
	{
		name:            "reaped task sheds its reap record as it is claimed",
		task:            taskBead("fields", map[string]any{ReapedAssigneeField: "crew-b7m"}),
		want:            SpawnTaskClaimed,
		wantPatch:       true,
		wantStamp:       true,
		wantClearReaped: true,
	},
	{
		name:            "bug whose reap record names a human is claimed under DispatchClaim",
		task:            taskBead("type", "bug", "fields", map[string]any{ReapedAssigneeField: "someone@example.com"}),
		want:            SpawnTaskClaimed,
		wantPatch:       true,
		wantStamp:       true,
		wantClearReaped: true,
	},
	{
		name:      "closed task",
		task:      taskBead("status", "closed"),
		want:      SpawnTaskClosed,
		wantStamp: true,
	},
	{
		name:      "epic is never claimed",
		task:      taskBead("type", "epic"),
		want:      SpawnTaskUnclaimableType,
		wantStamp: true,
	},
	{
		name:      "a refused write is an error",
		task:      taskBead(),
		patchCode: http.StatusForbidden,
		wantErr:   true,
		wantPatch: true,
	},
}

// TestClaimSpawnTask covers the session-sweep evidence (gd-Yo28zKgkGx): a crew
// spawned onto a task with no assignee, or one a dead crew still holds, gets
// the task claimed for it; a live or human holder, a closed task and an epic
// are left alone; and only a single-claim spawn's first pod create claims,
// within the spawner's scope.
func TestClaimSpawnTask(t *testing.T) {
	for _, tc := range claimSpawnTaskCases {
		t.Run(tc.name, func(t *testing.T) {
			agent := tc.agent
			if agent == nil {
				agent = singleClaimAgent(nil)
			}
			d := &claimDaemon{agent: agent, task: tc.task, agents: tc.agents, patchCode: tc.patchCode}
			srv := httptest.NewServer(d)
			defer srv.Close()
			c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

			got, err := c.ClaimSpawnTask(context.Background(), "gd-agent")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("outcome = %q, want %q", got, tc.want)
			}
			if tc.wantStamp {
				if len(d.agentPatches) != 1 || !strings.Contains(mustJSON(d.agentPatches[0]), `"`+FieldSpawnTaskClaim+`":"`+tc.want+`"`) {
					t.Errorf("agent patches = %v, want %s=%s stamped once", d.agentPatches, FieldSpawnTaskClaim, tc.want)
				}
			} else if len(d.agentPatches) != 0 {
				t.Errorf("agent patches = %v, want no stamp", d.agentPatches)
			}
			if !tc.wantPatch {
				if len(d.patches) != 0 {
					t.Fatalf("patched %v, want no write", d.patches)
				}
				return
			}
			if len(d.patches) != 1 {
				t.Fatalf("patches = %v, want exactly one", d.patches)
			}
			p := d.patches[0]
			if p["assignee"] != "crew-new" || p["status"] != "in_progress" {
				t.Errorf("patch = %v, want assignee=crew-new status=in_progress", p)
			}
			fields, _ := p["fields"].(map[string]any)
			v, sent := fields[ReapedAssigneeField]
			if cleared := sent && v == nil; cleared != tc.wantClearReaped {
				t.Errorf("patch fields = %v, want %s deleted: %v", fields, ReapedAssigneeField, tc.wantClearReaped)
			}
		})
	}
}

func TestClaimSpawnTask_NoTask(t *testing.T) {
	d := &claimDaemon{agent: singleClaimAgent(map[string]any{"task_id": "  "})}
	srv := httptest.NewServer(d)
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	got, err := c.ClaimSpawnTask(context.Background(), "gd-agent")
	if got != "" || err != nil || len(d.agentPatches) != 0 {
		t.Fatalf("got (%q, %v, %v), want no-op", got, err, d.agentPatches)
	}
}

// TestClaimSpawnTask_ReviveReclaims is the session-sweep evidence of
// gd-D83DkxrquP: a crew claimed its task at spawn and parked on CI, a merge-desk
// send-back cleared the assignee, and the timer wake revived it in place. Its
// stamp still read "claimed", so the recreate skipped the claim and every write
// to its own task was a 403. A revive clears the stamp, so the claim runs again.
func TestClaimSpawnTask_ReviveReclaims(t *testing.T) {
	agent := singleClaimAgent(map[string]any{FieldSpawnTaskClaim: SpawnTaskClaimed})
	d := &claimDaemon{agent: agent, task: taskBead()}
	srv := httptest.NewServer(d)
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	if got, err := c.ClaimSpawnTask(context.Background(), "gd-agent"); got != SpawnTaskRecreate || err != nil {
		t.Fatalf("before revive: got (%q, %v), want (%q, nil)", got, err, SpawnTaskRecreate)
	}
	// Apply the revive PATCH as the daemon merges it (RFC 7386: nil deletes).
	fields := agent["fields"].(map[string]any)
	for k, v := range ReviveAgentFields(1, WakeReasonTimer) {
		if v == nil {
			delete(fields, k)
		} else {
			fields[k] = v
		}
	}
	got, err := c.ClaimSpawnTask(context.Background(), "gd-agent")
	if got != SpawnTaskClaimed || err != nil {
		t.Fatalf("after revive: got (%q, %v), want (%q, nil)", got, err, SpawnTaskClaimed)
	}
	if len(d.patches) != 1 || d.patches[0]["assignee"] != "crew-new" {
		t.Errorf("task patches = %v, want one claim for crew-new", d.patches)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
