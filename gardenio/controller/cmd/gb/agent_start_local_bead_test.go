package main

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestLocalAgentCreateRequest_IsAnAgentBeadRunAsOwner(t *testing.T) {
	req := localAgentCreateRequest("local-laptop", "local", "pet", "gd-r1", "oidc:a@example.com")
	if req.Type != "agent" || req.Assignee != "local-laptop" || req.Title != "local-laptop" {
		t.Fatalf("bead shape wrong: %+v", req)
	}
	if req.CreatedBy != "oidc:a@example.com" {
		t.Errorf("created_by = %q, want the owner", req.CreatedBy)
	}
	var f map[string]string
	if err := json.Unmarshal(req.Fields, &f); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"agent": "local-laptop", "role": "local", "project": "pet", "mode": "crew", "execution_target": "laptop", "runtime_id": "gd-r1", "operated_by": "oidc:a@example.com", "agent_state": "working"} {
		if f[k] != want {
			t.Errorf("field %s = %q want %q", k, f[k], want)
		}
	}
	if _, ok := f["total_cost_usd"]; ok {
		t.Fatal("a local agent is owner-billed; total_cost_usd must stay unset")
	}
	if !slices.Contains(req.Labels, "project:pet") || !slices.Contains(req.Labels, "execution_target:laptop") {
		t.Fatalf("labels wrong: %v", req.Labels)
	}
	if slices.Contains(req.Labels, "kd:agent") {
		t.Fatalf("the roster finds a local agent by type, not the kd:agent label: %v", req.Labels)
	}
}

// Without a runner (a hand-run `gb agent start --local`) the bead still is an
// agent bead, but carries no runtime_id or operated_by the caller cannot back.
func TestLocalAgentCreateRequest_NoRunner(t *testing.T) {
	req := localAgentCreateRequest("local-box", "", "", "", "")
	var f map[string]string
	if err := json.Unmarshal(req.Fields, &f); err != nil {
		t.Fatal(err)
	}
	if f["role"] != "local" {
		t.Errorf("role = %q, want local", f["role"])
	}
	for _, k := range []string{"runtime_id", "operated_by"} {
		if _, ok := f[k]; ok {
			t.Errorf("field %s set without a runner: %q", k, f[k])
		}
	}
	if len(req.Labels) != 1 || req.Labels[0] != "execution_target:laptop" {
		t.Errorf("labels = %v, want just execution_target:laptop", req.Labels)
	}
}
