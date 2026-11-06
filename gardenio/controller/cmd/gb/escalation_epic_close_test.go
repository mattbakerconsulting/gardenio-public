package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

const epicStepPRURL = "https://github.com/mattbakerconsulting/gardenio/pull/1421"

// fakeEpicDaemon serves a small bead graph: beads by id, each with at most one
// parent, and records creates and blocks edges.
type fakeEpicDaemon struct {
	mu      sync.Mutex
	beads   map[string]map[string]any
	parent  map[string]string
	created []map[string]any
	blocks  []string // "<bead> blocks-> <escalation>"
	labeled []string
}

func (f *fakeEpicDaemon) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	switch {
	case path == "/v1/beads" && r.Method == "POST":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := fmt.Sprintf("gd-esc-%d", len(f.created)+1)
		f.created = append(f.created, body)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	case path == "/v1/beads" && r.Method == "GET":
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	case strings.HasSuffix(path, "/dependencies"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/dependencies")
		if r.Method == "POST" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.blocks = append(f.blocks, fmt.Sprintf("%s blocks-> %v", id, body["depends_on_id"]))
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		deps := []any{}
		if p := f.parent[id]; p != "" {
			deps = append(deps, map[string]any{"bead_id": id, "depends_on_id": p, "type": "parent-child"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": deps})
	case strings.HasSuffix(path, "/labels"):
		if r.Method == "POST" {
			f.labeled = append(f.labeled, strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/labels"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{})
	case strings.HasPrefix(path, "/v1/beads/") && r.Method == "GET":
		id := strings.TrimPrefix(path, "/v1/beads/")
		b, ok := f.beads[id]
		if !ok {
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		_ = json.NewEncoder(w).Encode(b)
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}
}

// newFakeEpicDaemon wires the plan from the incident: epic gd-plan, phase
// gd-phase under it, step gd-ymB1Pgg0fQ under the phase, and the agent
// gd-crew whose task is the step.
func newFakeEpicDaemon(t *testing.T) *fakeEpicDaemon {
	t.Helper()
	f := &fakeEpicDaemon{
		beads: map[string]map[string]any{
			"gd-plan":       {"id": "gd-plan", "type": "epic", "status": "open", "labels": []string{"plan"}},
			"gd-phase":      {"id": "gd-phase", "type": "epic", "status": "open"},
			"gd-ymB1Pgg0fQ": {"id": "gd-ymB1Pgg0fQ", "type": "task", "status": "in_progress"},
			"gd-elsewhere":  {"id": "gd-elsewhere", "type": "task", "status": "open"},
			"gd-crew":       {"id": "gd-crew", "type": "agent", "status": "open", "fields": map[string]string{"task_id": "gd-ymB1Pgg0fQ"}},
			"gd-plaintask":  {"id": "gd-plaintask", "type": "task", "status": "open"},
		},
		parent: map[string]string{"gd-phase": "gd-plan", "gd-ymB1Pgg0fQ": "gd-phase"},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	old := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = old })
	return f
}

// stubPRHead fakes gh: the step lookup gets branch and body, and the draft
// check gets a ready PR.
func stubPRHead(t *testing.T, branch, body string) {
	t.Helper()
	old := prGHRun
	prGHRun = func(_ context.Context, args ...string) (string, error) {
		if slices.Contains(args, "headRefName,body") {
			b, _ := json.Marshal(map[string]string{"headRefName": branch, "body": body})
			return string(b), nil
		}
		b, _ := json.Marshal(prView{Number: 1421, URL: epicStepPRURL, Title: "one EventSource"})
		return string(b), nil
	}
	t.Cleanup(func() { prGHRun = old })
}

func TestCloseOnExecuteTarget(t *testing.T) {
	cases := []struct {
		name, task, branch, body, want string
		wantErr                        bool
	}{
		{name: "non-epic task unchanged", task: "gd-plaintask", want: "gd-plaintask"},
		{name: "epic, step from branch suffix", task: "gd-plan",
			branch: "agent/one-eventsource/gd-ymB1Pgg0fQ", want: "gd-ymB1Pgg0fQ"},
		{name: "phase epic, step from branch suffix", task: "gd-phase",
			branch: "agent/one-eventsource/gd-ymB1Pgg0fQ", want: "gd-ymB1Pgg0fQ"},
		{name: "epic, step from Bead: trailer agent", task: "gd-plan",
			branch: "fix-sse", body: "## Summary\nx\n\nOperator: someone\nBead: gd-crew\n", want: "gd-ymB1Pgg0fQ"},
		{name: "epic, branch bead outside the epic refused", task: "gd-plan",
			branch: "agent/x/gd-elsewhere", wantErr: true},
		{name: "epic, branch names the epic itself refused", task: "gd-plan",
			branch: "agent/x/gd-phase", wantErr: true},
		{name: "epic, nothing to infer refused", task: "gd-plan", branch: "main-fix", wantErr: true},
		{name: "unknown task fails open", task: "gd-missing", want: "gd-missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newFakeEpicDaemon(t)
			stubPRHead(t, tc.branch, tc.body)
			got, err := closeOnExecuteTarget(context.Background(), tc.task, epicStepPRURL, &bytes.Buffer{})
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "--requesting-task <step-id>") {
					t.Fatalf("got %q, %v; want a refusal naming --requesting-task <step-id>", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// End to end, the incident: a plan gardener whose task_id is the plan epic
// files the merge escalation for a step's PR. The escalation names the step,
// and the blocks edge and label land on the step, not the plan.
func TestEscalationCreate_CloseOnExecuteEpicUsesStep(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("BOAT_PROJECT", "")
	f := newFakeEpicDaemon(t)
	stubPRHead(t, "agent/one-eventsource/gd-ymB1Pgg0fQ", "")

	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	defer resetEscalationCreateFlags(cmd)
	cmd.Flags().Set("title", "Merge PR #1421: one EventSource")
	cmd.Flags().Set("operation-type", "gitlab_mr_merge")
	cmd.Flags().Set("operation-spec", `{"mr_url":"`+epicStepPRURL+`"}`)
	cmd.Flags().Set("requesting-task", "gd-plan")
	cmd.Flags().Set("close-on-execute", "true")

	captureStdout(t, func() error { return cmd.RunE(cmd, nil) })

	if len(f.created) != 1 {
		t.Fatalf("daemon got %d creates, want 1", len(f.created))
	}
	fields, _ := f.created[0]["fields"].(map[string]any)
	if got := fields["requesting_task"]; got != "gd-ymB1Pgg0fQ" {
		t.Errorf("requesting_task = %v, want the step gd-ymB1Pgg0fQ", got)
	}
	for _, e := range f.blocks {
		if strings.HasPrefix(e, "gd-plan ") {
			t.Errorf("blocks edge on the plan epic: %s", e)
		}
	}
	if slices.Contains(f.labeled, "gd-plan") {
		t.Errorf("close-on-escalation-execute stamped on the plan epic")
	}
}

// With no step to find, nothing is filed.
func TestEscalationCreate_CloseOnExecuteEpicRefused(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("BOAT_PROJECT", "")
	f := newFakeEpicDaemon(t)
	stubPRHead(t, "hand-made-branch", "")

	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	defer resetEscalationCreateFlags(cmd)
	cmd.Flags().Set("title", "Merge PR #1421: one EventSource")
	cmd.Flags().Set("operation-type", "gitlab_mr_merge")
	cmd.Flags().Set("operation-spec", `{"mr_url":"`+epicStepPRURL+`"}`)
	cmd.Flags().Set("requesting-task", "gd-plan")
	cmd.Flags().Set("close-on-execute", "true")

	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "gd-plan is an epic") {
		t.Fatalf("err = %v, want the epic refusal", err)
	}
	if len(f.created) != 0 {
		t.Fatalf("daemon got %d creates, want 0", len(f.created))
	}
}
