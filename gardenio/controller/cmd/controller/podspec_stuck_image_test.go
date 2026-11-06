package main

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/subscriber"
)

// TestBuildAgentPodSpec_FallsBackToCoopImage proves a synthetic event with no
// metadata["image"] still produces a spec with an image set.
//
// The stuck publisher (internalStuckPublisher.PublishStuck) sends only
// {reason, source} in Metadata, so before the fallback the recreated pod's
// Image depended entirely on a warm ProjectCache/RoleCache. With a cold cache
// the API rejected the pod with "spec.containers[0].image: Required value" —
// and since the AgentStuck handler DELETES the old pod before creating the new
// one, that turned a "restart" into a permanent kill (ka-0000000084, a
// recurrence of ka-0000005268 on the live-agent path).
func TestBuildAgentPodSpec_FallsBackToCoopImage(t *testing.T) {
	cfg := &config.Config{
		CoopImage: "registry.example.com/agent:2026.210.20",
		// Deliberately cold: no ProjectCache/RoleCache entry to supply an image.
		ProjectCache: map[string]config.ProjectCacheEntry{},
		RoleCache:    map[string]config.RoleCacheEntry{},
	}
	ev := subscriber.Event{
		Type:      subscriber.AgentStuck,
		Project:   "monorepo",
		Role:      "monorepo-repro",
		AgentName: "repro-1",
		Metadata:  map[string]string{"reason": "no coop seq advance", "source": "stuck-heuristic"},
	}

	spec := buildAgentPodSpec(cfg, ev)

	if spec.Image == "" {
		t.Fatal("spec.Image is empty — K8s would reject this pod with " +
			`"spec.containers[0].image: Required value" and the agent would be deleted without a replacement`)
	}
	if spec.Image != cfg.CoopImage {
		t.Errorf("spec.Image = %q, want the configured CoopImage %q", spec.Image, cfg.CoopImage)
	}
}

// TestBuildAgentPodSpec_ExplicitImageWins verifies the fallback never shadows
// an image the event actually carried.
func TestBuildAgentPodSpec_ExplicitImageWins(t *testing.T) {
	cfg := &config.Config{
		CoopImage:    "registry.example.com/agent:default",
		ProjectCache: map[string]config.ProjectCacheEntry{},
		RoleCache:    map[string]config.RoleCacheEntry{},
	}
	ev := subscriber.Event{
		Project:   "monorepo",
		Role:      "monorepo-repro",
		AgentName: "repro-1",
		Metadata:  map[string]string{"image": "registry.example.com/agent:pinned"},
	}

	if spec := buildAgentPodSpec(cfg, ev); spec.Image != "registry.example.com/agent:pinned" {
		t.Errorf("spec.Image = %q, want the explicit per-event image", spec.Image)
	}
}
