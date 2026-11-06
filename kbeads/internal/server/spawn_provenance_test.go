package server

import (
	"encoding/json"
	"testing"
)

func TestSpawnApprovalMode(t *testing.T) {
	tests := []struct {
		approvedBy string
		want       string
	}{
		{"system:spawn-policy", "auto"},
		{"oidc:operator@example.com", "human"},
		{"", "human"},
	}
	for _, tc := range tests {
		if got := spawnApprovalMode(tc.approvedBy); got != tc.want {
			t.Errorf("spawnApprovalMode(%q) = %q, want %q", tc.approvedBy, got, tc.want)
		}
	}
}

func TestStampSpawnProvenance(t *testing.T) {
	in, err := json.Marshal(map[string]any{
		"agent":      "deploy-bot",
		"spawned_by": "stale-value",
	})
	if err != nil {
		t.Fatal(err)
	}

	out := stampSpawnProvenance(in, "esc-1", "oidc:operator@example.com")

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("stampSpawnProvenance produced invalid JSON: %v", err)
	}
	if got["agent"] != "deploy-bot" {
		t.Errorf("expected unrelated field to survive, got %v", got["agent"])
	}
	if got["spawned_by"] != "esc-1" {
		t.Errorf("spawned_by = %v, want escalation id to overwrite stale value", got["spawned_by"])
	}
	if got["spawn_approval_mode"] != "human" {
		t.Errorf("spawn_approval_mode = %v, want %q", got["spawn_approval_mode"], "human")
	}
	if got["spawn_approved_by"] != "oidc:operator@example.com" {
		t.Errorf("spawn_approved_by = %v, want %q", got["spawn_approved_by"], "oidc:operator@example.com")
	}
}

func TestStampSpawnProvenance_EmptyInput(t *testing.T) {
	out := stampSpawnProvenance(nil, "esc-1", "system:spawn-policy")

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("stampSpawnProvenance produced invalid JSON: %v", err)
	}
	if got["spawned_by"] != "esc-1" {
		t.Errorf("spawned_by = %v, want %q", got["spawned_by"], "esc-1")
	}
	if got["spawn_approval_mode"] != "auto" {
		t.Errorf("spawn_approval_mode = %v, want %q", got["spawn_approval_mode"], "auto")
	}
}
