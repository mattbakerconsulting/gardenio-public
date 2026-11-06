package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

func TestEscalationCreate_RequiresTitle(t *testing.T) {
	cmd := escalationCreateCmd
	cmd.Flags().Set("title", "")
	cmd.Flags().Set("operation-type", "aws_iam")
	err := cmd.RunE(cmd, nil)
	if err == nil || err.Error() != "--title is required" {
		t.Fatalf("expected --title is required, got %v", err)
	}
}

func TestEscalationCreate_RequiresOperationType(t *testing.T) {
	cmd := escalationCreateCmd
	cmd.Flags().Set("title", "test")
	cmd.Flags().Set("operation-type", "")
	err := cmd.RunE(cmd, nil)
	if err == nil || err.Error() != "--operation-type is required" {
		t.Fatalf("expected --operation-type is required, got %v", err)
	}
}

func TestEscalationCreate_ValidatesSensitivity(t *testing.T) {
	cmd := escalationCreateCmd
	cmd.Flags().Set("title", "test")
	cmd.Flags().Set("operation-type", "test")
	cmd.Flags().Set("sensitivity", "ultra-secret")
	err := cmd.RunE(cmd, nil)
	if err == nil || err.Error() != "--sensitivity must be one of: public, internal, sensitive, secret" {
		t.Fatalf("expected sensitivity error, got %v", err)
	}
	cmd.Flags().Set("sensitivity", "") // reset
}

func TestEscalationCreate_ValidatesExecutionTarget(t *testing.T) {
	cmd := escalationCreateCmd
	cmd.Flags().Set("title", "test")
	cmd.Flags().Set("operation-type", "test")
	cmd.Flags().Set("execution-target", "magic")
	err := cmd.RunE(cmd, nil)
	if err == nil || err.Error() != "--execution-target must be one of: local, service_account, server" {
		t.Fatalf("expected execution-target error, got %v", err)
	}
	cmd.Flags().Set("execution-target", "") // reset
}

func TestEscalationCreate_AcceptsServerTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == "POST" {
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-test-srv"})
			return
		}
		// Return empty list for dedup check.
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("title", "test server target")
	cmd.Flags().Set("operation-type", "test")
	cmd.Flags().Set("execution-target", "server")

	err := cmd.RunE(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error for server target: %v", err)
	}
	cmd.Flags().Set("execution-target", "") // reset
}

func TestEscalationCreate_RejectsScriptAndScriptFile(t *testing.T) {
	cmd := escalationCreateCmd
	cmd.Flags().Set("title", "test")
	cmd.Flags().Set("operation-type", "test")
	cmd.Flags().Set("script", "echo hi")
	cmd.Flags().Set("script-file", "/tmp/nope.sh")
	err := cmd.RunE(cmd, nil)
	if err == nil || err.Error() != "use --script or --script-file, not both" {
		t.Fatalf("expected script conflict error, got %v", err)
	}
	cmd.Flags().Set("script", "")
	cmd.Flags().Set("script-file", "")
}

func TestEscalationCreate_ValidatesOperationSpecJSON(t *testing.T) {
	cmd := escalationCreateCmd
	cmd.Flags().Set("title", "test")
	cmd.Flags().Set("operation-type", "test")
	cmd.Flags().Set("operation-spec", "not-json")
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected error for invalid operation-spec JSON")
	}
	cmd.Flags().Set("operation-spec", "") // reset
}

func TestEscalationCreate_SuccessfulCreation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == "POST" {
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-test-esc"})
			return
		}
		// Return empty list for dedup check.
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("title", "rotate creds")
	cmd.Flags().Set("operation-type", "credential_rotation")
	cmd.Flags().Set("operation-spec", `{"target":"prod"}`)
	cmd.Flags().Set("script", "#!/bin/bash\necho done")
	cmd.Flags().Set("sensitivity", "secret")
	cmd.Flags().Set("execution-target", "server")
	cmd.Flags().Set("credentials", `[{"type":"aws_role","identifier":"arn:aws:iam::123:role/admin"}]`)
	cmd.Flags().Set("expected-outputs", `[{"name":"password","sensitivity":"secret"}]`)

	err := cmd.RunE(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Reset flags for other tests.
	cmd.Flags().Set("script", "")
	cmd.Flags().Set("sensitivity", "")
	cmd.Flags().Set("execution-target", "")
	cmd.Flags().Set("credentials", "")
	cmd.Flags().Set("expected-outputs", "")
}

// TestEscalationCreate_WritesBlocksEdgeToRequestingTask proves the keystone
// escalation→work link (ka-0000004626): create with --requesting-task must
// POST a blocks-typed dependency ON the requesting task, pointing at the new
// escalation — the edge shape the /plans blocker walk reads.
func TestEscalationCreate_WritesBlocksEdgeToRequestingTask(t *testing.T) {
	var depPath string
	var depBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == "POST":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-esc-edge"})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == "POST":
			depPath = r.URL.Path
			json.NewDecoder(r.Body).Decode(&depBody)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{})
		default:
			// Empty list for the dedup check.
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("title", "grant iam for edge test")
	cmd.Flags().Set("operation-type", "aws_iam")
	cmd.Flags().Set("requesting-task", "gd-step-blocked")
	defer cmd.Flags().Set("requesting-task", "")

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depPath != "/v1/beads/gd-step-blocked/dependencies" {
		t.Errorf("blocks edge posted to %q, want it on the requesting task", depPath)
	}
	if depBody["depends_on_id"] != "gd-esc-edge" {
		t.Errorf("depends_on_id = %q, want the new escalation id", depBody["depends_on_id"])
	}
	if depBody["type"] != "blocks" {
		t.Errorf("dependency type = %q, want blocks", depBody["type"])
	}
}

// TestEscalationCreate_AutoDerivesRequestingTaskFromAgentBead proves the
// Layer-3 source-prevention path (ka-0000002745, design ka-0000006833 §3):
// with --requesting-task omitted but KD_AGENT_ID resolving to an agent bead
// carrying fields.task_id, the blocks-edge is written to that derived task, so
// the escalation homes onto the agent's task instead of orphaning.
func TestEscalationCreate_AutoDerivesRequestingTaskFromAgentBead(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-agent-e1")
	var depPath string
	var depBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == "POST":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-esc-auto"})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == "POST":
			depPath = r.URL.Path
			json.NewDecoder(r.Body).Decode(&depBody)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{})
		case r.URL.Path == "/v1/beads/gd-agent-e1" && r.Method == "GET":
			// deriveRequestingTask reads the agent bead's fields.task_id.
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-agent-e1", "type": "agent", "status": "open",
				"fields": map[string]string{"task_id": "gd-esc-my-task"},
			})
		case strings.HasPrefix(r.URL.Path, "/v1/beads/") && r.Method == "GET":
			// resolveOwningPlan's GetBead on the derived task: no plan ancestor.
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-esc-my-task", "type": "task", "status": "open"})
		default:
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("title", "auto-derive escalation")
	cmd.Flags().Set("operation-type", "aws_iam")

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depPath != "/v1/beads/gd-esc-my-task/dependencies" {
		t.Errorf("blocks edge posted to %q, want it on the auto-derived task gd-esc-my-task", depPath)
	}
	if depBody["depends_on_id"] != "gd-esc-auto" {
		t.Errorf("depends_on_id = %q, want the new escalation id", depBody["depends_on_id"])
	}
	if depBody["type"] != "blocks" {
		t.Errorf("dependency type = %q, want blocks", depBody["type"])
	}
}

// TestEscalationCreate_CloseOnExecuteStampsLabel proves the --close-on-execute
// opt-in stamps the daemon-enforced close-on-escalation-execute contract label
// on the requesting task, so the daemon auto-closes that wrapper when the
// escalation executes (feature ka-0000001587). Without the flag, no label is
// stamped (covered by TestEscalationCreate_WritesBlocksEdgeToRequestingTask,
// which sets no label endpoint).
func TestEscalationCreate_CloseOnExecuteStampsLabel(t *testing.T) {
	var labelPath string
	var labelBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == "POST":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-esc-col"})
		case strings.HasSuffix(r.URL.Path, "/labels") && r.Method == "POST":
			labelPath = r.URL.Path
			json.NewDecoder(r.Body).Decode(&labelBody)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == "POST":
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

	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("title", "seed creds wrapper")
	cmd.Flags().Set("operation-type", "credential_provisioning")
	cmd.Flags().Set("requesting-task", "gd-wrapper")
	cmd.Flags().Set("close-on-execute", "true")
	defer cmd.Flags().Set("requesting-task", "")
	defer cmd.Flags().Set("close-on-execute", "false")

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if labelPath != "/v1/beads/gd-wrapper/labels" {
		t.Errorf("label posted to %q, want it on the requesting task", labelPath)
	}
	if labelBody["label"] != "close-on-escalation-execute" {
		t.Errorf("label = %q, want close-on-escalation-execute", labelBody["label"])
	}
}

// TestEscalationCreate_NoCloseOnExecuteByDefault proves the flag is opt-in:
// with --requesting-task but WITHOUT --close-on-execute, no label POST (stamp)
// is made, but a label DELETE IS made to clear any stale label a prior
// --close-on-execute escalation against the same task might have left behind
// (gd-91b7zMOR2z fix b) — RemoveLabel is a no-op on the daemon side when the
// label is absent, so this is safe to fire unconditionally.
func TestEscalationCreate_NoCloseOnExecuteByDefault(t *testing.T) {
	labelPostCalls := 0
	var labelDeletePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == "POST":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-esc-nocol"})
		case strings.HasSuffix(r.URL.Path, "/labels") && r.Method == "POST":
			labelPostCalls++
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{})
		case strings.Contains(r.URL.Path, "/labels/") && r.Method == "DELETE":
			labelDeletePath = r.URL.Path
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == "POST":
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

	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("title", "plain blocked task")
	cmd.Flags().Set("operation-type", "general")
	cmd.Flags().Set("requesting-task", "gd-plainblocked")
	defer cmd.Flags().Set("requesting-task", "")

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if labelPostCalls != 0 {
		t.Errorf("expected no label stamp calls without --close-on-execute, got %d", labelPostCalls)
	}
	if labelDeletePath != "/v1/beads/gd-plainblocked/labels/close-on-escalation-execute" {
		t.Errorf("label delete path = %q, want the stale label cleared on the requesting task", labelDeletePath)
	}
}

// TestEscalationCreate_NoEdgeWithoutRequestingTask proves the flag stays
// optional: no --requesting-task → creation succeeds and no dependency call
// is made.
func TestEscalationCreate_NoEdgeWithoutRequestingTask(t *testing.T) {
	depCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == "POST":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-esc-noedge"})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == "POST":
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

	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("title", "no requesting task")
	cmd.Flags().Set("operation-type", "general")

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depCalls != 0 {
		t.Errorf("expected no dependency calls without --requesting-task, got %d", depCalls)
	}
}

// TestEscalationCreate_EdgeFailureIsNonFatal proves the best-effort contract:
// a failing dependency write must not fail the create — the escalation exists
// and its id must still be printed.
func TestEscalationCreate_EdgeFailureIsNonFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == "POST":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-esc-edgefail"})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == "POST":
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "boom"})
		default:
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("title", "edge failure tolerated")
	cmd.Flags().Set("operation-type", "general")
	cmd.Flags().Set("requesting-task", "gd-step-gone")
	defer cmd.Flags().Set("requesting-task", "")

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("edge failure must be non-fatal, got %v", err)
	}
}

// TestResolveOwningPlan_WalksToPlanEpic proves the parent-child walk: a step
// whose parent is a phase sub-epic whose parent is a plan-labeled epic
// resolves to that epic (ka-0000006860).
func TestResolveOwningPlan_WalksToPlanEpic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/beads/gd-step":
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-step", "type": "task"})
		case "/v1/beads/gd-step/dependencies":
			json.NewEncoder(w).Encode(map[string]any{"dependencies": []map[string]string{
				{"bead_id": "gd-step", "depends_on_id": "gd-phase", "type": "parent-child"},
			}})
		case "/v1/beads/gd-phase":
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-phase", "type": "epic", "labels": []string{"project:gardenio"}})
		case "/v1/beads/gd-phase/dependencies":
			json.NewEncoder(w).Encode(map[string]any{"dependencies": []map[string]string{
				{"bead_id": "gd-phase", "depends_on_id": "gd-plan", "type": "parent-child"},
			}})
		case "/v1/beads/gd-plan":
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-plan", "type": "epic", "labels": []string{"project:gardenio", "plan"}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"dependencies": []any{}})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	if got := resolveOwningPlan(context.Background(), "gd-step"); got != "gd-plan" {
		t.Errorf("resolveOwningPlan = %q, want gd-plan", got)
	}
}

// TestResolveOwningPlan_NoPlanAncestor proves a task with no plan-labeled
// ancestor resolves to "" (the common non-plan escalation).
func TestResolveOwningPlan_NoPlanAncestor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/beads/gd-orphan":
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-orphan", "type": "task"})
		default:
			json.NewEncoder(w).Encode(map[string]any{"dependencies": []any{}})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	if got := resolveOwningPlan(context.Background(), "gd-orphan"); got != "" {
		t.Errorf("resolveOwningPlan = %q, want empty", got)
	}
}

// TestEscalationBackfillLinks proves the one-shot backfill (ka-0000006860):
// an open escalation with requesting_task and no existing edge gets linked; one
// whose task already blocks-depends on it is skipped; one with no
// requesting_task is skipped.
func TestEscalationBackfillLinks(t *testing.T) {
	var addedDeps []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == "GET":
			json.NewEncoder(w).Encode(map[string]any{"beads": []map[string]any{
				{"id": "gd-esc-new", "type": "escalation", "status": "open", "title": "needs link",
					"fields": map[string]string{"requesting_task": "gd-task-new"}},
				{"id": "gd-esc-linked", "type": "escalation", "status": "open", "title": "already linked",
					"fields": map[string]string{"requesting_task": "gd-task-linked"}},
				{"id": "gd-esc-orphan", "type": "escalation", "status": "open", "title": "no task",
					"fields": map[string]string{}},
			}, "total": 3})
		case r.URL.Path == "/v1/beads/gd-task-new/dependencies" && r.Method == "GET":
			json.NewEncoder(w).Encode(map[string]any{"dependencies": []any{}})
		case r.URL.Path == "/v1/beads/gd-task-linked/dependencies" && r.Method == "GET":
			json.NewEncoder(w).Encode(map[string]any{"dependencies": []map[string]string{
				{"bead_id": "gd-task-linked", "depends_on_id": "gd-esc-linked", "type": "blocks"},
			}})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == "POST":
			addedDeps = append(addedDeps, r.URL.Path)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{})
		default:
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := escalationBackfillLinksCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(addedDeps) != 1 || addedDeps[0] != "/v1/beads/gd-task-new/dependencies" {
		t.Errorf("addedDeps = %v, want exactly the un-linked task", addedDeps)
	}
}

func TestEscalationList_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := escalationListCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestEscalationList_DefaultReturnsAll asserts the default (no --limit) path
// page-walks the daemon and prints EVERY matching escalation — regression guard
// for bug ka-0000001245, where a hardcoded Limit:50 silently hid the oldest 39
// of 89 open escalations. The server returns 60 beads (> the old cap) in one
// short page; all 60 must appear in the table.
func TestEscalationList_DefaultReturnsAll(t *testing.T) {
	const n = 60
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The default path must NOT send a fixed limit=50; ListAllBeadsFiltered
		// owns paging with the daemon max page size.
		if got := r.URL.Query().Get("limit"); got == "50" {
			t.Errorf("default list must not send limit=50, got limit=%s", got)
		}
		offset := r.URL.Query().Get("offset")
		beads := []any{}
		if offset == "" || offset == "0" {
			for i := range n {
				beads = append(beads, map[string]any{
					"id": fmt.Sprintf("gd-esc%d", i), "title": fmt.Sprintf("esc %d", i),
					"type": "escalation", "kind": "data", "status": "open", "priority": 0,
				})
			}
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": n})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	out := captureStdout(t, func() error {
		cmd := escalationListCmd
		cmd.SetContext(context.Background())
		return cmd.RunE(cmd, nil)
	})

	rows := strings.Count(string(out), "gd-esc")
	if rows != n {
		t.Errorf("expected all %d escalations in table, got %d rows\noutput:\n%s", n, rows, out)
	}
	if strings.Contains(string(out), "showing") {
		t.Errorf("default (unbounded) output must not print a truncation banner\noutput:\n%s", out)
	}
}

// TestEscalationList_LimitPrintsTruncationBanner asserts that a bounded --limit
// caps the rows AND announces the true count, so a cap is never silent.
func TestEscalationList_LimitPrintsTruncationBanner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("limit"); got != "2" {
			t.Errorf("expected limit=2 forwarded to daemon, got %q", got)
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"beads": []any{
				map[string]any{"id": "gd-escA", "title": "a", "type": "escalation", "kind": "data", "status": "open", "priority": 0},
				map[string]any{"id": "gd-escB", "title": "b", "type": "escalation", "kind": "data", "status": "open", "priority": 0},
			},
			"total": 89,
		})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	if err := escalationListCmd.Flags().Set("limit", "2"); err != nil {
		t.Fatalf("set limit flag: %v", err)
	}
	defer escalationListCmd.Flags().Set("limit", "0")

	out := captureStdout(t, func() error {
		cmd := escalationListCmd
		cmd.SetContext(context.Background())
		return cmd.RunE(cmd, nil)
	})

	if !strings.Contains(string(out), "showing 2 of 89") {
		t.Errorf("expected truncation banner 'showing 2 of 89', got:\n%s", out)
	}
}

func TestEscalationShow_NotEscalation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "gd-task1",
			"title":  "a task",
			"type":   "task",
			"kind":   "issue",
			"status": "open",
		})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := escalationShowCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-task1"})
	if err == nil {
		t.Fatal("expected error for non-escalation bead")
	}
	if !contains(err.Error(), "not an escalation") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEscalationApprove_RequiresIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "gd-esc1",
			"title":  "test esc",
			"type":   "escalation",
			"kind":   "data",
			"status": "open",
		})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	oldActor := actor
	actor = ""
	oldKdActor := os.Getenv("KD_ACTOR")
	os.Unsetenv("KD_ACTOR")
	defer func() {
		daemon = oldDaemon
		actor = oldActor
		if oldKdActor != "" {
			os.Setenv("KD_ACTOR", oldKdActor)
		}
	}()

	cmd := escalationApproveCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("as", "")
	err := cmd.RunE(cmd, []string{"gd-esc1"})
	if err == nil {
		t.Fatal("expected error when no identity provided")
	}
	if !contains(err.Error(), "--as is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestEscalationApprove_ExecutionTargetRidesApproveRequest (P7 Task 12): the
// override goes in the approve body, recorded by the daemon with the
// approval, never as a separate PATCH that races it.
func TestEscalationApprove_ExecutionTargetRidesApproveRequest(t *testing.T) {
	var approveBody map[string]any
	var patches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch:
			patches++
		case r.Method == http.MethodPost && r.URL.Path == "/v1/escalations/gd-esc1/approve":
			_ = json.NewDecoder(r.Body).Decode(&approveBody)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-esc1", "title": "test esc", "type": "escalation", "kind": "data", "status": "open",
		})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := escalationApproveCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("as", "op@example.com")
	cmd.Flags().Set("execution-target", "server")
	if err := cmd.RunE(cmd, []string{"gd-esc1"}); err == nil || !contains(err.Error(), "must be local or service_account") {
		t.Fatalf("--execution-target server: err = %v", err)
	}
	cmd.Flags().Set("execution-target", "service_account")
	defer func() {
		cmd.Flags().Set("execution-target", "")
		cmd.Flags().Set("as", "")
	}()
	if err := cmd.RunE(cmd, []string{"gd-esc1"}); err != nil {
		t.Fatal(err)
	}
	if approveBody["execution_target"] != "service_account" {
		t.Errorf("approve body = %v, want execution_target=service_account", approveBody)
	}
	if patches != 0 {
		t.Errorf("approve sent %d PATCH requests, want none", patches)
	}
}

func TestEscalationReject_RequiresIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "gd-esc1",
			"title":  "test esc",
			"type":   "escalation",
			"kind":   "data",
			"status": "open",
		})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	oldActor := actor
	actor = ""
	oldKdActor := os.Getenv("KD_ACTOR")
	os.Unsetenv("KD_ACTOR")
	defer func() {
		daemon = oldDaemon
		actor = oldActor
		if oldKdActor != "" {
			os.Setenv("KD_ACTOR", oldKdActor)
		}
	}()

	cmd := escalationRejectCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("as", "")
	err := cmd.RunE(cmd, []string{"gd-esc1"})
	if err == nil {
		t.Fatal("expected error when no identity provided")
	}
	if !contains(err.Error(), "--as is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEscalationClose_RequiresStatus(t *testing.T) {
	cmd := escalationCloseCmd
	cmd.Flags().Set("status", "")
	err := cmd.RunE(cmd, []string{"gd-esc1"})
	if err == nil {
		t.Fatal("expected error when no status provided")
	}
	if !contains(err.Error(), "--status is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEscalationClose_ValidatesStatus(t *testing.T) {
	cmd := escalationCloseCmd
	cmd.Flags().Set("status", "unknown")
	err := cmd.RunE(cmd, []string{"gd-esc1"})
	if err == nil {
		t.Fatal("expected error for invalid status")
	}
	if !contains(err.Error(), "must be one of") {
		t.Fatalf("unexpected error: %v", err)
	}
	cmd.Flags().Set("status", "") // reset
}

// closeResultPayload runs `escalation close` against a stub daemon with the
// given --result value and returns the `result` field of the captured request
// body — i.e. what actually lands on the escalation bead.
func closeResultPayload(t *testing.T, resultFlag string) map[string]any {
	t.Helper()

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := escalationCloseCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("status", "success")
	cmd.Flags().Set("result", resultFlag)
	cmd.Flags().Set("executor", "tester")
	defer func() {
		cmd.Flags().Set("status", "")
		cmd.Flags().Set("result", "")
		cmd.Flags().Set("executor", "")
	}()

	if err := cmd.RunE(cmd, []string{"gd-esc1"}); err != nil {
		t.Fatalf("close with --result %q: unexpected error: %v", resultFlag, err)
	}
	return got
}

// A bare prose --result is the natural first instinct and used to hard-error
// ("must be valid JSON"), driving repeated malformed retries (ka-0000007111).
// It must now be accepted and wrapped as {"output": "<prose>"}.
func TestEscalationClose_WrapsProseResult(t *testing.T) {
	const prose = "MR merged, all good"
	got := closeResultPayload(t, prose)

	result, ok := got["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result to be an object, got %#v", got["result"])
	}
	if result["output"] != prose {
		t.Errorf("result.output = %#v, want %q", result["output"], prose)
	}
	if len(result) != 1 {
		t.Errorf("expected only an output key, got %#v", result)
	}
}

// Valid JSON must still pass through verbatim — the wrap is only a fallback on
// the previously-erroring path.
func TestEscalationClose_PassesThroughValidJSON(t *testing.T) {
	got := closeResultPayload(t, `{"output":"credentials rotated","attempts":2}`)

	result, ok := got["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result to be an object, got %#v", got["result"])
	}
	if result["output"] != "credentials rotated" {
		t.Errorf("result.output = %#v, want %q", result["output"], "credentials rotated")
	}
	if result["attempts"] != float64(2) {
		t.Errorf("result.attempts = %#v, want 2", result["attempts"])
	}
}

// Prose that happens to parse as a JSON scalar (a bare number, `true`, `null`)
// is still valid JSON, so it passes through unwrapped rather than being
// wrapped — documenting the boundary of the fallback.
func TestEscalationClose_ScalarJSONNotWrapped(t *testing.T) {
	got := closeResultPayload(t, "42")
	if got["result"] != float64(42) {
		t.Errorf("result = %#v, want 42", got["result"])
	}
}

func TestFuzzyTitleMatch(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"Rotate prod DB credentials", "Rotate prod DB credentials", true},
		{"Rotate prod DB credentials", "rotate prod db credentials", true},
		{"RWX_ACCESS_TOKEN expired in GitLab CI", "RWX_ACCESS_TOKEN expired in GitLab CI — E2E dispatch broken", true},
		{"Totally different escalation", "Rotate prod DB credentials", false},
		{"ab", "cd", false},
	}
	for _, tt := range tests {
		got := fuzzyTitleMatch(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("fuzzyTitleMatch(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestEscalationAge(t *testing.T) {
	now := time.Now()
	tests := []struct {
		t    time.Time
		want string
	}{
		{now.Add(-30 * time.Second), "30s"},
		{now.Add(-5 * time.Minute), "5m"},
		{now.Add(-2 * time.Hour), "2h"},
		{now.Add(-3 * 24 * time.Hour), "3d"},
		{now.Add(-14 * 24 * time.Hour), "2w"},
	}
	for _, tt := range tests {
		got := escalationAge(tt.t)
		if got != tt.want {
			t.Errorf("escalationAge(%v ago) = %q, want %q", now.Sub(tt.t), got, tt.want)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestIsE2EFixture(t *testing.T) {
	tests := []struct {
		createdBy string
		want      bool
	}{
		{"escalation-analyze", true},
		{"e2e-agent-v2", true},
		{"e2e-v3-foo", true},
		{"e2e-v1", true},
		{"e2e-other", false},
		{"gardener-delivery", false},
		{"Matthew.Baker", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isE2EFixture(tt.createdBy); got != tt.want {
			t.Errorf("isE2EFixture(%q) = %v, want %v", tt.createdBy, got, tt.want)
		}
	}
}

// runEscalationShow runs `escalation show <id>` against a fake daemon serving
// the given escalation fields and returns captured stdout.
func runEscalationShow(t *testing.T, fields map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "gd-esc1",
			"title":  "test escalation",
			"type":   "escalation",
			"kind":   "data",
			"status": "open",
			"fields": fields,
		})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	oldStdout := os.Stdout
	rp, wp, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = wp
	defer func() { os.Stdout = oldStdout }()

	cmd := escalationShowCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-esc1"}); err != nil {
		wp.Close()
		t.Fatalf("escalation show: %v", err)
	}
	wp.Close()

	buf := make([]byte, 8192)
	n, _ := rp.Read(buf)
	return string(buf[:n])
}

// TestEscalationShow_RendersOperatorNotification asserts the Operator
// Notification group renders the notified_at + channel when the bridge has
// stamped them (ka-0000003667).
func TestEscalationShow_RendersOperatorNotification(t *testing.T) {
	out := runEscalationShow(t, map[string]string{
		"operation_type": "credential_rotation",
		"notified_at":    "2026-07-10T21:00:00Z",
		"slack_channel":  "C0AHE7C0E1G",
	})
	if !strings.Contains(out, "── Operator Notification ──") {
		t.Errorf("expected Operator Notification group in output, got:\n%s", out)
	}
	if !strings.Contains(out, "2026-07-10T21:00:00Z") {
		t.Errorf("expected notified_at timestamp in output, got:\n%s", out)
	}
	if !strings.Contains(out, "C0AHE7C0E1G") {
		t.Errorf("expected slack_channel in output, got:\n%s", out)
	}
}

// TestEscalationShow_RendersOperatorDM asserts the personal-ping stamps
// (ka-0000002669) surface in the same group. This is a stronger signal than
// notified_at: the card routes by REQUESTING AGENT, so notified_at only means
// the escalation landed in some channel, while operator_dm_at means the human
// in operated_by was DMed directly.
func TestEscalationShow_RendersOperatorDM(t *testing.T) {
	out := runEscalationShow(t, map[string]string{
		"operation_type":      "gitlab_mr_merge",
		"operator_dm_at":      "2026-08-26T15:30:00Z",
		"operator_dm_channel": "D0A1B2C3D4E",
	})
	if !strings.Contains(out, "── Operator Notification ──") {
		t.Errorf("expected Operator Notification group when only the DM stamps are set, got:\n%s", out)
	}
	if !strings.Contains(out, "2026-08-26T15:30:00Z") {
		t.Errorf("expected operator_dm_at timestamp in output, got:\n%s", out)
	}
	if !strings.Contains(out, "D0A1B2C3D4E") {
		t.Errorf("expected operator_dm_channel in output, got:\n%s", out)
	}
}

// TestEscalationShow_OmitsOperatorNotificationWhenAbsent asserts legacy /
// pre-stamp escalations (no notified_at, no slack_channel, no DM stamps) render
// without the Operator Notification group.
func TestEscalationShow_OmitsOperatorNotificationWhenAbsent(t *testing.T) {
	out := runEscalationShow(t, map[string]string{
		"operation_type": "credential_rotation",
	})
	if strings.Contains(out, "── Operator Notification ──") {
		t.Errorf("did not expect Operator Notification group when fields absent, got:\n%s", out)
	}
}

// ── escalation stats classifier + latency (ka-0000007267) ────────────────

// TestClassifyEscalationOutcomes_UnrecordedNotFailure is the regression guard
// for the headline bug: a closed escalation with no result_status must land in
// the Unrecorded bucket, NOT be inferred (via approved_by) to be a failure.
func TestClassifyEscalationOutcomes_UnrecordedNotFailure(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{Fields: map[string]string{"result_status": "success"}},
		{Fields: map[string]string{"result_status": "failure"}},
		{Fields: map[string]string{"result_status": "partial"}},
		// Unset result_status but approved_by present — the OLD code counted
		// this as a success; the NEW code counts it as Unrecorded.
		{Fields: map[string]string{"approved_by": "matthew.baker"}},
		// Unset result_status and no approval — the OLD code counted this as a
		// failure; the NEW code counts it as Unrecorded.
		{Fields: map[string]string{}},
		{Fields: map[string]string{}},
		// E2E fixture — excluded from the denominator entirely.
		{CreatedBy: "e2e-agent-v2", Fields: map[string]string{}},
	}
	got := classifyEscalationOutcomes(beads)
	want := escalationOutcomeCounts{
		Total:       6,
		Success:     1,
		Failure:     1,
		Partial:     1,
		Unrecorded:  3,
		E2EExcluded: 1,
	}
	if got != want {
		t.Errorf("classifyEscalationOutcomes() = %+v, want %+v", got, want)
	}
}

func TestClassifyEscalationOutcomes_Empty(t *testing.T) {
	got := classifyEscalationOutcomes(nil)
	if got != (escalationOutcomeCounts{}) {
		t.Errorf("classifyEscalationOutcomes(nil) = %+v, want zero value", got)
	}
}

func TestEscalationClosureLatency(t *testing.T) {
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		bead    *beadsapi.BeadDetail
		wantOK  bool
		wantDur time.Duration
	}{
		{
			name: "uses closed_at over updated_at",
			bead: &beadsapi.BeadDetail{
				CreatedAt: base,
				ClosedAt:  base.Add(3 * time.Hour).Format(time.RFC3339),
				UpdatedAt: base.Add(99 * time.Hour), // ignored when closed_at parses
				Status:    "closed",
			},
			wantOK:  true,
			wantDur: 3 * time.Hour,
		},
		{
			name: "falls back to updated_at when closed_at empty",
			bead: &beadsapi.BeadDetail{
				CreatedAt: base,
				UpdatedAt: base.Add(2 * time.Hour),
				Status:    "closed",
			},
			wantOK:  true,
			wantDur: 2 * time.Hour,
		},
		{
			name:   "no timestamps → not ok",
			bead:   &beadsapi.BeadDetail{Status: "closed"},
			wantOK: false,
		},
		{
			name: "close before create → not ok",
			bead: &beadsapi.BeadDetail{
				CreatedAt: base,
				ClosedAt:  base.Add(-time.Hour).Format(time.RFC3339),
				Status:    "closed",
			},
			wantOK: false,
		},
		{
			name: "unparseable closed_at, not closed status → not ok",
			bead: &beadsapi.BeadDetail{
				CreatedAt: base,
				ClosedAt:  "not-a-timestamp",
				UpdatedAt: base.Add(5 * time.Hour),
				Status:    "open",
			},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDur, gotOK := escalationClosureLatency(tt.bead)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotOK && gotDur != tt.wantDur {
				t.Errorf("dur = %v, want %v", gotDur, tt.wantDur)
			}
		})
	}
}

func TestPercentileDuration(t *testing.T) {
	// 10 durations, 1h..10h ascending.
	var ds []time.Duration
	for i := 1; i <= 10; i++ {
		ds = append(ds, time.Duration(i)*time.Hour)
	}
	tests := []struct {
		p    int
		want time.Duration
	}{
		{50, 5 * time.Hour}, // ceil(0.50*10)=5 → sorted[4]=5h
		{90, 9 * time.Hour}, // ceil(0.90*10)=9 → sorted[8]=9h
		{100, 10 * time.Hour},
		{1, 1 * time.Hour}, // ceil(0.01*10)=1 → sorted[0]=1h
	}
	for _, tt := range tests {
		if got := percentileDuration(ds, tt.p); got != tt.want {
			t.Errorf("percentileDuration(p%d) = %v, want %v", tt.p, got, tt.want)
		}
	}
	if got := percentileDuration(nil, 50); got != 0 {
		t.Errorf("percentileDuration(nil) = %v, want 0", got)
	}
	if got := percentileDuration([]time.Duration{7 * time.Hour}, 90); got != 7*time.Hour {
		t.Errorf("percentileDuration(single, p90) = %v, want 7h", got)
	}
}

func TestFormatLatency(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1.5h"},
		{47 * time.Hour, "47.0h"},
		{72 * time.Hour, "3.0d"},
	}
	for _, tt := range tests {
		if got := formatLatency(tt.d); got != tt.want {
			t.Errorf("formatLatency(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// TestEscalationList_JSON asserts `gb escalation list --json` emits a parseable
// JSON array (not the human table) so `| jq` works (ka-0000007267 minor fix).
func TestEscalationList_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"beads": []any{
				map[string]any{
					"id": "gd-esc1", "title": "rotate key", "type": "escalation",
					"kind": "data", "status": "open", "priority": 1,
					"fields": map[string]string{"operation_type": "credential_rotation"},
				},
			},
			"total": 1,
		})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	oldJSON := jsonOutput
	jsonOutput = true
	defer func() { daemon = oldDaemon; jsonOutput = oldJSON }()

	out := captureStdout(t, func() error {
		cmd := escalationListCmd
		cmd.SetContext(context.Background())
		return cmd.RunE(cmd, nil)
	})

	var beads []map[string]any
	if err := json.Unmarshal(out, &beads); err != nil {
		t.Fatalf("output is not a JSON array: %v\noutput:\n%s", err, out)
	}
	if len(beads) != 1 || beads[0]["id"] != "gd-esc1" {
		t.Errorf("unexpected JSON beads: %+v", beads)
	}
}

func withEscalationDaemon(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = old })
}

func TestEscalationClaim_PostsClaim(t *testing.T) {
	var gotPath string
	withEscalationDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{"id": "gd-e1", "type": "escalation", "status": "open", "fields": map[string]string{"held_by": "coordinator:laptop", "claimed_by": "machine:gardenio-laptop-operator"}})
	})
	cmd := escalationClaimCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-e1"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /v1/escalations/gd-e1/claim" {
		t.Fatalf("request = %s", gotPath)
	}
}

func TestEscalationHandUp_RequiresReason(t *testing.T) {
	called := false
	withEscalationDaemon(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	cmd := escalationHandUpCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("reason", "")
	if err := cmd.RunE(cmd, []string{"gd-e1"}); err == nil || err.Error() != "--reason is required" {
		t.Fatalf("got %v, want --reason is required", err)
	}
	if called {
		t.Fatal("hand-up without a reason reached the daemon")
	}
}

func TestEscalationHandUp_PostsReason(t *testing.T) {
	var gotPath, gotBody string
	withEscalationDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(b)
		json.NewEncoder(w).Encode(map[string]any{"id": "gd-e1", "type": "escalation", "status": "open", "fields": map[string]string{"held_by": "operator"}})
	})
	cmd := escalationHandUpCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("reason", "touches deploy-values")
	defer cmd.Flags().Set("reason", "")
	if err := cmd.RunE(cmd, []string{"gd-e1"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/escalations/gd-e1/hand-up" || !strings.Contains(gotBody, `"reason":"touches deploy-values"`) {
		t.Fatalf("request = %s %s", gotPath, gotBody)
	}
}

func TestEscalationReroute_PostsApprovedUnexecuted(t *testing.T) {
	cmd := escalationRerouteCmd
	cmd.SetContext(context.Background())
	called := false
	withEscalationDaemon(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	if err := cmd.RunE(cmd, nil); err == nil || err.Error() != "--approved-unexecuted is required" {
		t.Fatalf("got %v, want --approved-unexecuted is required", err)
	}
	if called {
		t.Fatal("reroute without --approved-unexecuted reached the daemon")
	}

	var gotPath, gotBody string
	withEscalationDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.Method+" "+r.URL.Path, string(b)
		json.NewEncoder(w).Encode(map[string]any{"considered": 3, "dry_run": true, "rerouted": []any{map[string]any{"id": "gd-e1", "from": "coordinator:laptop", "execute_by": "operator", "held_by": "operator"}}})
	})
	_ = cmd.Flags().Set("approved-unexecuted", "true")
	_ = cmd.Flags().Set("dry-run", "true")
	defer func() {
		_ = cmd.Flags().Set("approved-unexecuted", "false")
		_ = cmd.Flags().Set("dry-run", "false")
	}()
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /v1/escalations/reroute" || !strings.Contains(gotBody, `"approved_unexecuted":true`) || !strings.Contains(gotBody, `"dry_run":true`) {
		t.Fatalf("request = %s %s", gotPath, gotBody)
	}
}

func TestEscalationList_HeldByAndNeedsMe(t *testing.T) {
	var gotPath, gotHeldBy string
	withEscalationDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHeldBy = r.URL.Path, r.URL.Query().Get("held_by")
		json.NewEncoder(w).Encode(map[string]any{"escalations": []any{}})
	})
	cmd := escalationListCmd
	cmd.SetContext(context.Background())
	defer func() {
		_ = cmd.Flags().Set("held-by", "")
		_ = cmd.Flags().Set("needs-me", "false")
		_ = cmd.Flags().Set("mine", "false")
	}()

	_ = cmd.Flags().Set("held-by", "coordinator:laptop,operator")
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/escalations" || gotHeldBy != "coordinator:laptop,operator" {
		t.Fatalf("--held-by request = %s held_by=%q", gotPath, gotHeldBy)
	}

	_ = cmd.Flags().Set("held-by", "")
	_ = cmd.Flags().Set("needs-me", "true")
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if gotHeldBy != "@me" {
		t.Fatalf("--needs-me held_by=%q, want @me", gotHeldBy)
	}

	_ = cmd.Flags().Set("held-by", "operator")
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("--held-by with --needs-me must be refused")
	}
	_ = cmd.Flags().Set("needs-me", "false")
	_ = cmd.Flags().Set("mine", "true")
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("--held-by with --mine must be refused")
	}
}

// --deployed-tag folds the helm_deploy roll into the result the daemon reads
// to record the release's deployment, keeping the prose under output.
func TestEscalationClose_DeployedTag(t *testing.T) {
	cmd := escalationCloseCmd
	cmd.Flags().Set("deployed-tag", "2026.304.1")
	cmd.Flags().Set("revision", "431")
	defer func() {
		cmd.Flags().Set("deployed-tag", "")
		cmd.Flags().Set("revision", "")
	}()
	got := closeResultPayload(t, "rolled in the 16:00Z window")
	result, ok := got["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result to be an object, got %#v", got["result"])
	}
	want := map[string]any{"output": "rolled in the 16:00Z window", "tag": "2026.304.1", "revision": "431"}
	for k, v := range want {
		if result[k] != v {
			t.Errorf("result[%s] = %#v, want %#v", k, result[k], v)
		}
	}
	if _, ok := result["target"]; ok {
		t.Errorf("result.target set without --target: %#v", result)
	}

	cmd.Flags().Set("status", "failure")
	defer cmd.Flags().Set("status", "")
	if err := cmd.RunE(cmd, []string{"gd-esc1"}); err == nil || !contains(err.Error(), "needs --status success") {
		t.Errorf("--deployed-tag with --status failure: err = %v, want a refusal", err)
	}
}

// gb escalation approve and reject hand a refused person the page (P10.B
// gd-4mEAhIE01U).
func TestEscalationApproveReject_403NamesThePage(t *testing.T) {
	t.Setenv("BEADS_VIEWER_URL", "https://garden.example.com")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-esc1", "type": "escalation", "status": "open"})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "escalation approval is not available to a `kd login` session"})
	}))
	defer srv.Close()
	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	for _, cmd := range []*cobra.Command{escalationApproveCmd, escalationRejectCmd} {
		cmd.SetContext(context.Background())
		_ = cmd.Flags().Set("as", "owner")
		err := cmd.RunE(cmd, []string{"gd-esc1"})
		if err == nil || !strings.Contains(err.Error(), "https://garden.example.com/gd-esc1") {
			t.Errorf("%s: error %v does not name the page", cmd.Name(), err)
		}
		_ = cmd.Flags().Set("as", "")
	}
}
