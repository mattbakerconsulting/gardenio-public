package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestIsRunInstanceTaskDetail covers the run-instance detection used to scope
// the forced-stop truthful-close: source labels and fire-key fallback match,
// ordinary tasks do not.
func TestIsRunInstanceTaskDetail(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		fields map[string]string
		want   bool
	}{
		{"source:schedule label", []string{"source:schedule", "project:gardenio"}, nil, true},
		{"source:multi label", []string{"source:multi"}, nil, true},
		{"source:schedule-mirror label", []string{"source:schedule-mirror"}, nil, true},
		{"schedule_fire_key fallback", nil, map[string]string{"schedule_fire_key": "2026-07-11T02:00"}, true},
		{"multi_fire_key fallback", nil, map[string]string{"multi_fire_key": "1:1:x"}, true},
		{"ordinary task", []string{"project:gardenio", "kind:ced-audit"}, map[string]string{"foo": "bar"}, false},
		{"no labels no fields", nil, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &beadsapi.BeadDetail{Labels: c.labels, Fields: c.fields}
			if got := isRunInstanceTaskDetail(b); got != c.want {
				t.Errorf("isRunInstanceTaskDetail = %v, want %v", got, c.want)
			}
		})
	}
}

// TestCloseForcedRunInstanceTasks_ClosesRunInstanceWithTruthfulReason verifies
// that a claimed run-instance task is closed with a truthful close_reason naming
// the forced stop — and that an ordinary claimed task in the same result set is
// left untouched (ka-0000006105).
func TestCloseForcedRunInstanceTasks_ClosesRunInstanceWithTruthfulReason(t *testing.T) {
	closed := map[string]map[string]any{}
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			// Assigned in_progress issue-kind tasks: one run-instance, one ordinary.
			resp := map[string]any{
				"beads": []map[string]any{
					{
						"id": "gd-RUN", "type": "task", "kind": "issue",
						"status": "in_progress", "assignee": "groomer-agent",
						"labels": []string{"source:schedule", "project:gardenio"},
						"fields": map[string]string{"schedule_fire_key": "2026-07-11T02:00"},
					},
					{
						"id": "gd-ORD", "type": "task", "kind": "issue",
						"status": "in_progress", "assignee": "groomer-agent",
						"labels": []string{"project:gardenio"},
						"fields": map[string]string{},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/close")
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			closed[id] = body
			mu.Unlock()
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	n := closeForcedRunInstanceTasks(context.Background(), "groomer-agent")
	if n != 1 {
		t.Fatalf("closed count = %d, want 1 (only the run-instance task)", n)
	}

	mu.Lock()
	defer mu.Unlock()

	runClose, ok := closed["gd-RUN"]
	if !ok {
		t.Fatalf("run-instance task gd-RUN was not closed; closed=%v", closed)
	}
	reason, _ := runClose["close_reason"].(string)
	if !strings.Contains(reason, "gb stop --force") {
		t.Errorf("close_reason should name 'gb stop --force', got %q", reason)
	}
	if !strings.Contains(reason, "groomer-agent") {
		t.Errorf("close_reason should name the agent, got %q", reason)
	}
	if !strings.Contains(reason, "work not completed") {
		t.Errorf("close_reason should be truthful that work was not completed, got %q", reason)
	}
	// close attribution should be the agent, not the default "gardenio".
	if by, _ := runClose["closed_by"].(string); by != "groomer-agent" {
		t.Errorf("closed_by = %q, want groomer-agent", by)
	}

	// Critical: the ordinary claimed task must NOT be closed — that would
	// masquerade unfinished real work as complete.
	if _, ok := closed["gd-ORD"]; ok {
		t.Errorf("ordinary claimed task gd-ORD must NOT be closed on --force; closed=%v", closed)
	}
}

// TestCloseForcedRunInstanceTasks_NoClaimedTasks confirms a no-op (0 closed)
// when the agent holds no claimed in_progress tasks.
func TestCloseForcedRunInstanceTasks_NoClaimedTasks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/beads" {
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []map[string]any{}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	if n := closeForcedRunInstanceTasks(context.Background(), "idle-agent"); n != 0 {
		t.Errorf("closed count = %d, want 0", n)
	}
}

// TestCloseForcedRunInstanceTasks_NilDaemon is a defensive guard — a nil daemon
// or empty actor must be a safe no-op, never a panic.
func TestCloseForcedRunInstanceTasks_NilDaemon(t *testing.T) {
	oldDaemon := daemon
	daemon = nil
	t.Cleanup(func() { daemon = oldDaemon })
	if n := closeForcedRunInstanceTasks(context.Background(), ""); n != 0 {
		t.Errorf("nil daemon / empty actor should close 0, got %d", n)
	}
}

// TestClaimedWorkGate_OwnScheduleRun reproduces gd-mOnsZZyug9: a scheduled run
// holds the `Schedule:` task its trigger assigned it (task_id on the agent
// bead), and its `gb stop` was refused with "you have claimed in-progress
// work". That one task must pass the gate and be handed back for closing; any
// other claim, run-instance or not, must still trip it.
func TestClaimedWorkGate_OwnScheduleRun(t *testing.T) {
	oldActor := actor
	actor = "cicd-watchdog-p0vx"
	t.Cleanup(func() { actor = oldActor })

	runTask := map[string]any{
		"id": "gd-RUN01", "title": "[2026-10-30T18:05] Schedule: cicd watchdog",
		"type": "task", "status": "in_progress", "kind": "issue", "assignee": "cicd-watchdog-p0vx",
		"labels": []string{"source:schedule", "trigger:gd-TRIG01"},
		"fields": map[string]string{"schedule_fire_key": "2026-10-30T18:05", "trigger_id": "gd-TRIG01"},
	}
	otherRun := map[string]any{
		"id": "gd-RUN02", "title": "[2026-10-30T16:05] Schedule: cicd watchdog",
		"type": "task", "status": "in_progress", "kind": "issue", "assignee": "cicd-watchdog-p0vx",
		"labels": []string{"source:schedule"},
	}
	ordinary := map[string]any{
		"id": "gd-ORD01", "title": "a real bug", "type": "task",
		"status": "in_progress", "kind": "issue", "assignee": "cicd-watchdog-p0vx",
	}
	agentBead := func(taskID string) map[string]map[string]any {
		return map[string]map[string]any{"gd-AGENT01": {
			"id": "gd-AGENT01", "type": "agent", "status": "open",
			"fields": map[string]string{"task_id": taskID, "source": "schedule"},
		}}
	}

	t.Run("own run-instance task passes and is returned", func(t *testing.T) {
		parkFixture(t, []map[string]any{runTask}, nil, agentBead("gd-RUN01"))
		own, err := claimedWorkGate(context.Background(), "gd-AGENT01", nil)
		if err != nil {
			t.Fatalf("a scheduled run's own task must not trip the gate, got: %v", err)
		}
		if own != "gd-RUN01" {
			t.Errorf("own run task = %q, want gd-RUN01", own)
		}
		if err := checkClaimedWork(context.Background(), "gd-AGENT01"); err != nil {
			t.Errorf("checkClaimedWork: %v", err)
		}
	})

	for _, tc := range []struct {
		name    string
		claimed []map[string]any
		agent   map[string]map[string]any
		want    string
	}{
		{"a run-instance task the trigger did not assign this run", []map[string]any{otherRun}, agentBead("gd-RUN01"), "gd-RUN02"},
		{"own run task does not mask an ordinary claim", []map[string]any{runTask, ordinary}, agentBead("gd-RUN01"), "gd-ORD01"},
		{"an ordinary task named by task_id is not a run-instance", []map[string]any{ordinary}, agentBead("gd-ORD01"), "gd-ORD01"},
		{"agent bead unreadable: fail closed", []map[string]any{runTask}, nil, "gd-RUN01"},
		{"agent bead has no task_id", []map[string]any{runTask}, agentBead(""), "gd-RUN01"},
	} {
		t.Run(tc.name+" -> gate fires", func(t *testing.T) {
			parkFixture(t, tc.claimed, nil, tc.agent)
			own, err := claimedWorkGate(context.Background(), "gd-AGENT01", nil)
			if err == nil {
				t.Fatal("gate returned nil; want claimed work pending")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should name %s, got: %v", tc.want, err)
			}
			if own != "" {
				t.Errorf("a refused stop must not hand back a task to close, got %q", own)
			}
		})
	}

	t.Run("a task whose PR the park held passes and keeps its claim", func(t *testing.T) {
		parkFixture(t, []map[string]any{ordinary}, nil, agentBead("gd-ORD01"))
		held := map[string]string{"gd-ORD01": "task gd-ORD01 has the pr:hold label"}
		own, err := claimedWorkGate(context.Background(), "gd-AGENT01", held)
		if err != nil {
			t.Fatalf("held PR task must pass the gate, got: %v", err)
		}
		if own != "" {
			t.Errorf("a held task keeps its claim; got own=%q to close", own)
		}
		// The hold covers only the task it names.
		parkFixture(t, []map[string]any{ordinary, otherRun}, nil, agentBead("gd-ORD01"))
		if _, err := claimedWorkGate(context.Background(), "gd-AGENT01", held); err == nil || !strings.Contains(err.Error(), "gd-RUN02") {
			t.Errorf("an unheld claim must still trip the gate, got: %v", err)
		}
	})

	t.Run("own run task parked behind a handle stays parked, not closed", func(t *testing.T) {
		handles := agentBead("gd-RUN01")
		handles["gd-ESC01"] = map[string]any{
			"id": "gd-ESC01", "type": "escalation", "status": "open",
			"fields": map[string]string{"requesting_task": "gd-RUN01"},
		}
		parkFixture(t, []map[string]any{runTask}, map[string][]string{"gd-RUN01": {"gd-ESC01"}}, handles)
		own, err := claimedWorkGate(context.Background(), "gd-AGENT01", nil)
		if err != nil {
			t.Fatalf("parked task must pass the gate, got: %v", err)
		}
		if own != "" {
			t.Errorf("a parked run task keeps its claim; got own=%q to close", own)
		}
	})
}

// TestCloseOwnRunInstanceTask closes the run's task as the agent, with a
// close_reason that names the clean stop rather than a crash.
func TestCloseOwnRunInstanceTask(t *testing.T) {
	var got map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close") {
			gotPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	if !closeOwnRunInstanceTask(context.Background(), "gd-RUN01", "cicd-watchdog-p0vx") {
		t.Fatal("closeOwnRunInstanceTask returned false")
	}
	if gotPath != "/v1/beads/gd-RUN01/close" {
		t.Errorf("closed %q, want /v1/beads/gd-RUN01/close", gotPath)
	}
	if by, _ := got["closed_by"].(string); by != "cicd-watchdog-p0vx" {
		t.Errorf("closed_by = %q, want the agent", by)
	}
	reason, _ := got["close_reason"].(string)
	if !strings.Contains(reason, "scheduled run ended") || !strings.Contains(reason, "cicd-watchdog-p0vx") {
		t.Errorf("close_reason = %q, want it to name the scheduled run's stop and agent", reason)
	}
	if closeOwnRunInstanceTask(context.Background(), "", "x") {
		t.Error("empty task id must be a no-op")
	}
}
