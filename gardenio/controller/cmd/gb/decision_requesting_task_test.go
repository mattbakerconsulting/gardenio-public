package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/pflag"
)

// clearDecisionCreateFlags fully resets the mutated flags on the package-global
// decisionCreateCmd between tests. Set("option", "") does NOT clear a pflag
// StringArray — after the first Set it APPENDS, leaking entries into later tests
// that share this command (notably JSONOptions, which then trips the
// options/option mutual-exclusion guard). Use the SliceValue.Replace path to
// truly empty the array.
func clearDecisionCreateFlags(cmd interface{ Flags() *pflag.FlagSet }) {
	f := cmd.Flags()
	f.Set("prompt", "")
	f.Set("options", "")
	f.Set("no-wait", "false")
	f.Set("requesting-task", "")
	f.Set("subject", "")
	if opt := f.Lookup("option"); opt != nil {
		if sv, ok := opt.Value.(pflag.SliceValue); ok {
			sv.Replace(nil)
		}
	}
}

// TestDecisionCreate_WritesBlocksEdgeToRequestingTask proves the decision→work
// link (ka-0000003927): create with --requesting-task must POST a blocks-typed
// dependency ON the requesting task, pointing at the new decision — the same
// edge shape `gb escalation create --requesting-task` writes and the shape the
// /plans blocker walk + exclude_blocked_by_open_types gate filter read. Without
// it a decision-blocked task is invisible to the dependency graph.
func TestDecisionCreate_WritesBlocksEdgeToRequestingTask(t *testing.T) {
	var depPath string
	var depBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-dec-edge"})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == http.MethodPost:
			depPath = r.URL.Path
			json.NewDecoder(r.Body).Decode(&depBody)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasPrefix(r.URL.Path, "/v1/beads/") && r.Method == http.MethodGet:
			// resolveOwningPlan's GetBead on the requesting task: a plain task
			// with no plan ancestor, so no requesting_plan is recorded.
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-step-blocked", "type": "task", "status": "open"})
		default:
			// Empty list for the dedup check (GET /v1/beads).
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := decisionCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("prompt", "should we ship or hold, edge test")
	cmd.Flags().Set("option", "ship:Ship now:plan")
	cmd.Flags().Set("no-wait", "true")
	cmd.Flags().Set("requesting-task", "gd-step-blocked")
	defer clearDecisionCreateFlags(cmd)

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depPath != "/v1/beads/gd-step-blocked/dependencies" {
		t.Errorf("blocks edge posted to %q, want it on the requesting task", depPath)
	}
	if depBody["depends_on_id"] != "gd-dec-edge" {
		t.Errorf("depends_on_id = %q, want the new decision id", depBody["depends_on_id"])
	}
	if depBody["type"] != "blocks" {
		t.Errorf("dependency type = %q, want blocks", depBody["type"])
	}
}

// TestDecisionCreate_NoEdgeWithoutRequestingTask proves the flag stays optional:
// no --requesting-task → creation succeeds and no dependency call is made, so
// the existing edge-less behaviour is unchanged when the flag is omitted.
func TestDecisionCreate_NoEdgeWithoutRequestingTask(t *testing.T) {
	depCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-dec-noedge"})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == http.MethodPost:
			depCalls++
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{})
		default:
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	// Force no agent identity so the Layer-3 auto-derive path (ka-0000002745)
	// resolves nothing and the edge-less behaviour is exercised deterministically
	// regardless of the ambient KD_AGENT_ID in the test process.
	t.Setenv("KD_AGENT_ID", "")

	cmd := decisionCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("prompt", "no requesting task decision")
	cmd.Flags().Set("option", "a:Option A:report")
	cmd.Flags().Set("no-wait", "true")
	defer clearDecisionCreateFlags(cmd)

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depCalls != 0 {
		t.Errorf("expected no dependency calls without --requesting-task, got %d", depCalls)
	}
}

// TestDecisionCreate_AutoDerivesRequestingTaskFromAgentBead proves the Layer-3
// source-prevention path (ka-0000002745, design ka-0000006833 §3): when
// --requesting-task is omitted but KD_AGENT_ID resolves to an agent bead
// carrying fields.task_id, the blocks-edge is written to that derived task —
// so the decision homes onto the agent's task instead of orphaning.
func TestDecisionCreate_AutoDerivesRequestingTaskFromAgentBead(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-agent-1")
	var depPath string
	var depBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-dec-auto"})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == http.MethodPost:
			depPath = r.URL.Path
			json.NewDecoder(r.Body).Decode(&depBody)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{})
		case r.URL.Path == "/v1/beads/gd-agent-1" && r.Method == http.MethodGet:
			// deriveRequestingTask reads the agent bead's fields.task_id.
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-agent-1", "type": "agent", "status": "open",
				"fields": map[string]string{"task_id": "gd-my-task"},
			})
		case strings.HasPrefix(r.URL.Path, "/v1/beads/") && r.Method == http.MethodGet:
			// resolveOwningPlan's GetBead on the derived task: no plan ancestor.
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-my-task", "type": "task", "status": "open"})
		default:
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := decisionCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("prompt", "auto-derive requesting task decision")
	cmd.Flags().Set("option", "a:Option A:report")
	cmd.Flags().Set("no-wait", "true")
	defer clearDecisionCreateFlags(cmd)

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depPath != "/v1/beads/gd-my-task/dependencies" {
		t.Errorf("blocks edge posted to %q, want it on the auto-derived task gd-my-task", depPath)
	}
	if depBody["depends_on_id"] != "gd-dec-auto" {
		t.Errorf("depends_on_id = %q, want the new decision id", depBody["depends_on_id"])
	}
	if depBody["type"] != "blocks" {
		t.Errorf("dependency type = %q, want blocks", depBody["type"])
	}
}

// TestDecisionCreate_WarnsWhenNoHoming proves the no-homing warning
// (ka-0000002745): with neither --requesting-task nor an agent bead task_id to
// derive from, creation still succeeds, writes no edge, and emits a one-line
// warning to stderr pointing the operator at the flat inbox / /plans unhomed
// section.
func TestDecisionCreate_WarnsWhenNoHoming(t *testing.T) {
	// Agent identity resolves, but the agent bead carries no task_id, so nothing
	// homes — the warning path fires.
	t.Setenv("KD_AGENT_ID", "gd-agent-notask")
	depCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-dec-nohome"})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == http.MethodPost:
			depCalls++
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{})
		case r.URL.Path == "/v1/beads/gd-agent-notask" && r.Method == http.MethodGet:
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-agent-notask", "type": "agent", "status": "open"})
		default:
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	stderr := captureStderr(t, func() {
		cmd := decisionCreateCmd
		cmd.SetContext(context.Background())
		cmd.Flags().Set("prompt", "unhomed decision")
		cmd.Flags().Set("option", "a:Option A:report")
		cmd.Flags().Set("no-wait", "true")
		defer clearDecisionCreateFlags(cmd)
		if err := cmd.RunE(cmd, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	if depCalls != 0 {
		t.Errorf("expected no dependency calls when nothing homes, got %d", depCalls)
	}
	if !strings.Contains(stderr, "no requesting task or plan ancestor") {
		t.Errorf("expected no-homing warning on stderr, got: %q", stderr)
	}
}

// TestDecisionCreate_StampsRequestingTaskField proves --requesting-task is also
// persisted as the back-compat `requesting_task` string field on the decision
// bead (mirrors escalation.go), so provenance survives even if the graph edge
// is later severed.
func TestDecisionCreate_StampsRequestingTaskField(t *testing.T) {
	var createFields map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			var req struct {
				Fields json.RawMessage `json:"fields"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			json.Unmarshal(req.Fields, &createFields)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-dec-field"})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == http.MethodPost:
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasPrefix(r.URL.Path, "/v1/beads/") && r.Method == http.MethodGet:
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-task", "type": "task", "status": "open"})
		default:
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := decisionCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("prompt", "field-stamp decision")
	cmd.Flags().Set("option", "a:Option A:report")
	cmd.Flags().Set("no-wait", "true")
	cmd.Flags().Set("requesting-task", "gd-task")
	defer clearDecisionCreateFlags(cmd)

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(createFields["requesting_task"]); got != `"gd-task"` {
		t.Errorf("requesting_task field = %s, want \"gd-task\"", got)
	}
}

// TestDecisionCreate_RecordsRequestingPlan proves that when the requesting task
// sits under a plan-labeled epic, the owning plan id is pinned onto the decision
// via requesting_plan (mirrors escalation.go / ka-0000006860), so the decision
// stays attributable to its /plans plan even if the step is later reparented.
func TestDecisionCreate_RecordsRequestingPlan(t *testing.T) {
	var planPatchID string
	var planPatchBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-dec-plan"})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == http.MethodPost:
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{})
		case r.URL.Path == "/v1/beads/gd-dec-plan" && r.Method == http.MethodGet:
			// UpdateBeadFields reads the decision bead before patching it.
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-dec-plan", "type": "decision", "status": "open"})
		case r.URL.Path == "/v1/beads/gd-dec-plan" && (r.Method == http.MethodPatch || r.Method == http.MethodPut):
			planPatchID = "gd-dec-plan"
			// UpdateBeadFields wraps the patch as {"fields": {...}}; unwrap it.
			var wrapper struct {
				Fields json.RawMessage `json:"fields"`
			}
			json.NewDecoder(r.Body).Decode(&wrapper)
			json.Unmarshal(wrapper.Fields, &planPatchBody)
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{})
		case r.URL.Path == "/v1/beads/gd-step" && r.Method == http.MethodGet:
			// The requesting task: a plain step.
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-step", "type": "task", "status": "open"})
		case r.URL.Path == "/v1/beads/gd-step/dependencies" && r.Method == http.MethodGet:
			// Its parent is the plan epic.
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"dependencies": []map[string]string{
				{"bead_id": "gd-step", "depends_on_id": "gd-plan", "type": "parent-child"},
			}})
		case r.URL.Path == "/v1/beads/gd-plan" && r.Method == http.MethodGet:
			// The plan-labeled epic ancestor.
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-plan", "type": "epic", "status": "open", "labels": []string{"plan"}})
		default:
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := decisionCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("prompt", "plan-owned decision")
	cmd.Flags().Set("option", "a:Option A:report")
	cmd.Flags().Set("no-wait", "true")
	cmd.Flags().Set("requesting-task", "gd-step")
	defer clearDecisionCreateFlags(cmd)

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if planPatchID != "gd-dec-plan" {
		t.Fatalf("expected requesting_plan patch on the decision, got none")
	}
	if got, _ := planPatchBody["requesting_plan"].(string); got != "gd-plan" {
		t.Errorf("requesting_plan = %q, want gd-plan (patch body: %v)", got, planPatchBody)
	}
}
