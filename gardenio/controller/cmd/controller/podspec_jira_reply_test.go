package main

// Env-injection guards for gb jira reply (feature F2, plan ka-0000007401):
// Jira-spawned agent pods must carry JIRA_KEY (from the agent bead's jira_key
// field) and JIRA_BRIDGE_URL (from controller config) on BOTH spec builders —
// the reconciler path (BuildSpecFromBeadInfo) and the events path
// (buildAgentPodSpec) — so a pod restart never drops the binding.

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

func TestBuildSpecFromBeadInfo_JiraKeyMetadata(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		CoopImage:    "agent:latest",
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	metadata := map[string]string{"jira_key": "PE-1234"}
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "agent1", metadata)

	if spec.Env["JIRA_KEY"] != "PE-1234" {
		t.Errorf("expected JIRA_KEY=PE-1234, got %q", spec.Env["JIRA_KEY"])
	}
}

func TestBuildSpecFromBeadInfo_NoJiraKeyMetadata(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		CoopImage:    "agent:latest",
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "agent1", map[string]string{})

	if _, present := spec.Env["JIRA_KEY"]; present {
		t.Error("JIRA_KEY must not be set for non-Jira-bound agents")
	}
}

func TestBuildAgentPodSpec_JiraKeyMetadata(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		CoopImage:    "agent:latest",
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	event := subscriber.Event{
		Project:   "gardenio",
		Mode:      "crew",
		Role:      "crew",
		AgentName: "agent1",
		Metadata:  map[string]string{"jira_key": "DEVOPS-55"},
	}
	spec := buildAgentPodSpec(cfg, event)

	if spec.Env["JIRA_KEY"] != "DEVOPS-55" {
		t.Errorf("expected JIRA_KEY=DEVOPS-55 on the events path, got %q", spec.Env["JIRA_KEY"])
	}
}

func TestApplyCommonConfig_JiraBridgeURL(t *testing.T) {
	cfg := &config.Config{
		JiraBridgeURL: "http://gardenio-jira-bridge:8091",
		ProjectCache:  map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.Env["JIRA_BRIDGE_URL"] != "http://gardenio-jira-bridge:8091" {
		t.Errorf("expected JIRA_BRIDGE_URL, got %q", spec.Env["JIRA_BRIDGE_URL"])
	}
}

func TestApplyCommonConfig_NoJiraBridgeURL(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if _, present := spec.Env["JIRA_BRIDGE_URL"]; present {
		t.Error("JIRA_BRIDGE_URL must not be set when unconfigured")
	}
}
