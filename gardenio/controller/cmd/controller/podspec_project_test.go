package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

func TestApplyProjectDefaults_PlainEnvVars(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {
				EnvVars: []beadsapi.EnvEntry{
					{Name: "JIRA_BASE_URL", Value: "https://acme.atlassian.net"},
					{Name: "JIRA_PROJECT", Value: "PIH"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyProjectDefaults(cfg, spec)

	if spec.Env["JIRA_BASE_URL"] != "https://acme.atlassian.net" {
		t.Errorf("expected JIRA_BASE_URL=https://acme.atlassian.net, got %q", spec.Env["JIRA_BASE_URL"])
	}
	if spec.Env["JIRA_PROJECT"] != "PIH" {
		t.Errorf("expected JIRA_PROJECT=PIH, got %q", spec.Env["JIRA_PROJECT"])
	}
}

func TestApplyProjectDefaults_PlainEnvVarsDoNotOverrideExisting(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {
				EnvVars: []beadsapi.EnvEntry{
					{Name: "EXISTING_VAR", Value: "from-project"},
					{Name: "NEW_VAR", Value: "added"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{"EXISTING_VAR": "keep-this"},
	}
	applyProjectDefaults(cfg, spec)

	if spec.Env["EXISTING_VAR"] != "keep-this" {
		t.Errorf("expected EXISTING_VAR=keep-this (not overridden), got %q", spec.Env["EXISTING_VAR"])
	}
	if spec.Env["NEW_VAR"] != "added" {
		t.Errorf("expected NEW_VAR=added, got %q", spec.Env["NEW_VAR"])
	}
}

func TestApplyProjectDefaults_PlainEnvVarsNoProject(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "nonexistent",
		Env:     map[string]string{},
	}
	applyProjectDefaults(cfg, spec)

	if len(spec.Env) != 0 {
		t.Errorf("expected no env vars, got %d", len(spec.Env))
	}
}

func TestApplyProjectDefaults_PlainEnvVarsAndSecretsTogether(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {
				EnvVars: []beadsapi.EnvEntry{
					{Name: "JIRA_BASE_URL", Value: "https://acme.atlassian.net"},
				},
				Secrets: []beadsapi.SecretEntry{
					{Env: "JIRA_API_TOKEN", Secret: "myproject-jira", Key: "api-token"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyProjectDefaults(cfg, spec)

	if spec.Env["JIRA_BASE_URL"] != "https://acme.atlassian.net" {
		t.Errorf("expected JIRA_BASE_URL from env vars, got %q", spec.Env["JIRA_BASE_URL"])
	}

	// Secrets are applied by applyCommonConfig, not applyProjectDefaults.
	if len(spec.SecretEnv) != 0 {
		t.Errorf("expected no SecretEnv from applyProjectDefaults, got %d", len(spec.SecretEnv))
	}
}

func TestBuildSpecFromBeadInfo_IncludesProjectEnvVars(t *testing.T) {
	cfg := &config.Config{
		Namespace: "test",
		CoopImage: "agent:latest",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"monorepo": {
				EnvVars: []beadsapi.EnvEntry{
					{Name: "JIRA_BASE_URL", Value: "https://acme.atlassian.net"},
					{Name: "JIRA_PROJECT", Value: "PIH"},
				},
			},
		},
	}
	spec := BuildSpecFromBeadInfo(cfg, "monorepo", "crew", "crew", "agent1", map[string]string{})

	if spec.Env["JIRA_BASE_URL"] != "https://acme.atlassian.net" {
		t.Errorf("expected JIRA_BASE_URL from project env vars, got %q", spec.Env["JIRA_BASE_URL"])
	}
	if spec.Env["JIRA_PROJECT"] != "PIH" {
		t.Errorf("expected JIRA_PROJECT from project env vars, got %q", spec.Env["JIRA_PROJECT"])
	}
}

func TestApplyProjectDefaults_ResourceOverrides(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"heavy": {
				CPURequest:    "4",
				CPULimit:      "8",
				MemoryRequest: "4Gi",
				MemoryLimit:   "16Gi",
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "heavy",
		Env:     map[string]string{},
	}
	defaults := podmanager.DefaultPodDefaults("crew", "crew")
	podmanager.ApplyDefaults(spec, defaults)
	applyProjectDefaults(cfg, spec)

	if spec.Resources == nil {
		t.Fatal("expected Resources to be set")
	}
	cpuReq := spec.Resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "4" {
		t.Errorf("expected cpu request 4, got %s", cpuReq.String())
	}
	cpuLim := spec.Resources.Limits[corev1.ResourceCPU]
	if cpuLim.String() != "8" {
		t.Errorf("expected cpu limit 8, got %s", cpuLim.String())
	}
	memReq := spec.Resources.Requests[corev1.ResourceMemory]
	if memReq.String() != "4Gi" {
		t.Errorf("expected memory request 4Gi, got %s", memReq.String())
	}
	memLim := spec.Resources.Limits[corev1.ResourceMemory]
	if memLim.String() != "16Gi" {
		t.Errorf("expected memory limit 16Gi, got %s", memLim.String())
	}
}

func TestApplyProjectDefaults_PartialResourceOverride(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"partial": {
				CPURequest: "500m",
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "partial",
		Env:     map[string]string{},
	}
	defaults := podmanager.DefaultPodDefaults("crew", "crew")
	podmanager.ApplyDefaults(spec, defaults)
	applyProjectDefaults(cfg, spec)

	cpuReq := spec.Resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "500m" {
		t.Errorf("expected cpu request 500m, got %s", cpuReq.String())
	}
	memReq := spec.Resources.Requests[corev1.ResourceMemory]
	if memReq.String() != podmanager.DefaultMemoryRequest {
		t.Errorf("expected default memory request %s, got %s", podmanager.DefaultMemoryRequest, memReq.String())
	}
}

func TestApplyProjectDefaults_EnvOverrides(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"envtest": {
				EnvOverrides: map[string]string{
					"FEATURE_FLAG": "true",
					"API_URL":      "https://api.example.com",
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "envtest",
		Env:     map[string]string{"EXISTING": "keep"},
	}
	applyProjectDefaults(cfg, spec)

	if spec.Env["FEATURE_FLAG"] != "true" {
		t.Errorf("expected FEATURE_FLAG=true, got %s", spec.Env["FEATURE_FLAG"])
	}
	if spec.Env["API_URL"] != "https://api.example.com" {
		t.Errorf("expected API_URL, got %s", spec.Env["API_URL"])
	}
	if spec.Env["EXISTING"] != "keep" {
		t.Errorf("expected existing env to be preserved, got %s", spec.Env["EXISTING"])
	}
}

func TestBuildAgentPodSpec_SlackThreadMetadata(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	event := subscriber.Event{
		Project:   "gardenio",
		Role:      "crew",
		AgentName: "thread-agent",
		Metadata: map[string]string{
			"slack_thread_channel": "C-support",
			"slack_thread_ts":      "1234.5678",
		},
	}
	spec := buildAgentPodSpec(cfg, event)

	if spec.Env["SLACK_THREAD_CHANNEL"] != "C-support" {
		t.Errorf("expected SLACK_THREAD_CHANNEL=C-support, got %q", spec.Env["SLACK_THREAD_CHANNEL"])
	}
	if spec.Env["SLACK_THREAD_TS"] != "1234.5678" {
		t.Errorf("expected SLACK_THREAD_TS=1234.5678, got %q", spec.Env["SLACK_THREAD_TS"])
	}
}

func TestBuildAgentPodSpec_SlackThreadMetadataAbsent(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	event := subscriber.Event{
		Project:   "gardenio",
		Role:      "crew",
		AgentName: "normal-agent",
		Metadata:  map[string]string{},
	}
	spec := buildAgentPodSpec(cfg, event)

	if _, ok := spec.Env["SLACK_THREAD_CHANNEL"]; ok {
		t.Error("expected SLACK_THREAD_CHANNEL to not be set for non-thread agent")
	}
	if _, ok := spec.Env["SLACK_THREAD_TS"]; ok {
		t.Error("expected SLACK_THREAD_TS to not be set for non-thread agent")
	}
}

func TestBuildAgentPodSpec_SpawnSourceSlackThread_UsesCrewMode(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	event := subscriber.Event{
		Project:   "gardenio",
		Role:      "crew",
		AgentName: "thread-agent",
		Metadata: map[string]string{
			"spawn_source":         "slack-thread",
			"slack_thread_channel": "C-test",
			"slack_thread_ts":      "1.1",
		},
	}
	spec := buildAgentPodSpec(cfg, event)

	// Thread agents use crew mode (with PVC) so session resume works
	// across thread-resume cycles.
	if spec.Mode != "crew" {
		t.Errorf("expected mode=crew for slack-thread spawn, got %q", spec.Mode)
	}
}

func TestBuildAgentPodSpec_SpawnSourceSlackThread_ExplicitModePreserved(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	event := subscriber.Event{
		Project:   "gardenio",
		Mode:      "crew",
		Role:      "crew",
		AgentName: "thread-agent",
		Metadata: map[string]string{
			"spawn_source": "slack-thread",
		},
	}
	spec := buildAgentPodSpec(cfg, event)

	if spec.Mode != "crew" {
		t.Errorf("expected explicit mode=crew to be preserved, got %q", spec.Mode)
	}
}

func TestBuildAgentPodSpec_SpawnProvenanceMetadata(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	event := subscriber.Event{
		Project:   "gardenio",
		Role:      "k8s-live",
		AgentName: "spawned-agent",
		Metadata: map[string]string{
			"spawned_by":          "esc-123",
			"spawn_approval_mode": "human",
			"spawn_approved_by":   "oidc:operator@example.com",
		},
	}
	spec := buildAgentPodSpec(cfg, event)

	if spec.SpawnedBy != "esc-123" {
		t.Errorf("SpawnedBy = %q, want %q", spec.SpawnedBy, "esc-123")
	}
	if spec.SpawnApprovalMode != "human" {
		t.Errorf("SpawnApprovalMode = %q, want %q", spec.SpawnApprovalMode, "human")
	}
	if spec.SpawnApprovedBy != "oidc:operator@example.com" {
		t.Errorf("SpawnApprovedBy = %q, want %q", spec.SpawnApprovedBy, "oidc:operator@example.com")
	}
}

// Both spec builders carry spawn_source and wake_reason, which pick a
// gardener's PriorityClass (gd-Y2KaNE233g).
func TestBuildSpecs_SpawnSourceAndWakeReason(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	meta := map[string]string{"spawn_source": "schedule", "wake_reason": "timer"}
	event := subscriber.Event{Project: "gardenio", Role: "plan-gardener", AgentName: "g", Metadata: meta}
	for name, spec := range map[string]podmanager.AgentPodSpec{
		"event":      buildAgentPodSpec(cfg, event),
		"reconciler": BuildSpecFromBeadInfo(cfg, "gardenio", "", "plan-gardener", "g", meta),
	} {
		if spec.SpawnSource != "schedule" || spec.WakeReason != "timer" {
			t.Errorf("%s: SpawnSource=%q WakeReason=%q, want schedule and timer", name, spec.SpawnSource, spec.WakeReason)
		}
	}
}

func TestBuildAgentPodSpec_SpawnProvenanceAbsentByDefault(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	event := subscriber.Event{
		Project: "gardenio", Role: "crew", AgentName: "ordinary-agent",
		Metadata: map[string]string{},
	}
	spec := buildAgentPodSpec(cfg, event)

	if spec.SpawnedBy != "" || spec.SpawnApprovalMode != "" || spec.SpawnApprovedBy != "" {
		t.Errorf("expected empty spawn provenance for ordinary spawn, got SpawnedBy=%q SpawnApprovalMode=%q SpawnApprovedBy=%q",
			spec.SpawnedBy, spec.SpawnApprovalMode, spec.SpawnApprovedBy)
	}
}

func TestApplyProjectDefaults_NoOverrides(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"plain": {},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "plain",
		Env:     map[string]string{},
	}
	defaults := podmanager.DefaultPodDefaults("crew", "crew")
	podmanager.ApplyDefaults(spec, defaults)
	origResources := spec.Resources
	applyProjectDefaults(cfg, spec)

	if spec.Resources != origResources {
		t.Error("expected resources to remain unchanged")
	}
}

func TestApplyCommonConfig_TeamsResourceOverrides(t *testing.T) {
	cfg := &config.Config{
		ClaudeTeamsEnabled:       true,
		ClaudeTeamsCPURequest:    "4",
		ClaudeTeamsCPULimit:      "8",
		ClaudeTeamsMemoryRequest: "24Gi",
		ClaudeTeamsMemoryLimit:   "24Gi",
		ProjectCache:             map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	defaults := podmanager.DefaultPodDefaults("crew", "crew")
	podmanager.ApplyDefaults(spec, defaults)
	applyCommonConfig(cfg, spec)

	if spec.Resources == nil {
		t.Fatal("expected Resources to be set")
	}
	cpuReq := spec.Resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "4" {
		t.Errorf("expected cpu request 4, got %s", cpuReq.String())
	}
	cpuLim := spec.Resources.Limits[corev1.ResourceCPU]
	if cpuLim.String() != "8" {
		t.Errorf("expected cpu limit 8, got %s", cpuLim.String())
	}
	memReq := spec.Resources.Requests[corev1.ResourceMemory]
	if memReq.String() != "24Gi" {
		t.Errorf("expected memory request 24Gi, got %s", memReq.String())
	}
	memLim := spec.Resources.Limits[corev1.ResourceMemory]
	if memLim.String() != "24Gi" {
		t.Errorf("expected memory limit 24Gi, got %s", memLim.String())
	}
}

func TestApplyCommonConfig_TeamsDisabled_NoResourceOverride(t *testing.T) {
	cfg := &config.Config{
		ClaudeTeamsEnabled:       false,
		ClaudeTeamsCPURequest:    "4",
		ClaudeTeamsMemoryRequest: "24Gi",
		ProjectCache:             map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	defaults := podmanager.DefaultPodDefaults("crew", "crew")
	podmanager.ApplyDefaults(spec, defaults)
	applyCommonConfig(cfg, spec)

	cpuReq := spec.Resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != podmanager.DefaultCPURequest {
		t.Errorf("expected default cpu request %s, got %s", podmanager.DefaultCPURequest, cpuReq.String())
	}
	memReq := spec.Resources.Requests[corev1.ResourceMemory]
	if memReq.String() != podmanager.DefaultMemoryRequest {
		t.Errorf("expected default memory request %s, got %s", podmanager.DefaultMemoryRequest, memReq.String())
	}
}

func TestApplyCommonConfig_TeamsPartialResourceOverride(t *testing.T) {
	cfg := &config.Config{
		ClaudeTeamsEnabled:       true,
		ClaudeTeamsMemoryRequest: "32Gi",
		ClaudeTeamsMemoryLimit:   "32Gi",
		ProjectCache:             map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	defaults := podmanager.DefaultPodDefaults("crew", "crew")
	podmanager.ApplyDefaults(spec, defaults)
	applyCommonConfig(cfg, spec)

	memReq := spec.Resources.Requests[corev1.ResourceMemory]
	if memReq.String() != "32Gi" {
		t.Errorf("expected memory request 32Gi, got %s", memReq.String())
	}
	cpuReq := spec.Resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != podmanager.DefaultCPURequest {
		t.Errorf("expected default cpu request %s, got %s", podmanager.DefaultCPURequest, cpuReq.String())
	}
}

func TestApplyCommonConfig_S3EnvVars(t *testing.T) {
	cfg := &config.Config{
		CoopS3Bucket:       "gardenio-sessions",
		CoopS3Prefix:       "prod/sessions",
		CoopS3Region:       "us-west-2",
		CoopS3ImagesBucket: "gardenio-images",
	}
	spec := &podmanager.AgentPodSpec{
		Project: "testproj",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.Env["COOP_S3_BUCKET"] != "gardenio-sessions" {
		t.Errorf("COOP_S3_BUCKET = %q, want gardenio-sessions", spec.Env["COOP_S3_BUCKET"])
	}
	if spec.Env["COOP_S3_PREFIX"] != "prod/sessions" {
		t.Errorf("COOP_S3_PREFIX = %q, want prod/sessions", spec.Env["COOP_S3_PREFIX"])
	}
	if spec.Env["COOP_S3_REGION"] != "us-west-2" {
		t.Errorf("COOP_S3_REGION = %q, want us-west-2", spec.Env["COOP_S3_REGION"])
	}
	if spec.Env["COOP_S3_IMAGES_BUCKET"] != "gardenio-images" {
		t.Errorf("COOP_S3_IMAGES_BUCKET = %q, want gardenio-images", spec.Env["COOP_S3_IMAGES_BUCKET"])
	}
}

func TestApplyProjectDefaults_DockerEnabled(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"dockerproj": {DockerEnabled: true},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "dockerproj",
		Env:     map[string]string{},
	}
	applyProjectDefaults(cfg, spec)

	if !spec.Docker {
		t.Error("expected Docker=true when project has DockerEnabled")
	}
}

func TestApplyProjectDefaults_DockerDisabledByDefault(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"nodocker": {},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "nodocker",
		Env:     map[string]string{},
	}
	applyProjectDefaults(cfg, spec)

	if spec.Docker {
		t.Error("expected Docker=false when project does not enable it")
	}
}

func TestBuildSpecFromBeadInfo_DockerMetadataOverride(t *testing.T) {
	cfg := &config.Config{
		Namespace: "test",
		CoopImage: "agent:latest",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"proj": {DockerEnabled: true},
		},
	}

	// Agent metadata "docker"="false" should override project-level enable.
	spec := BuildSpecFromBeadInfo(cfg, "proj", "crew", "crew", "agent1", map[string]string{"docker": "false"})
	if spec.Docker {
		t.Error("expected Docker=false when metadata overrides project default")
	}

	// Agent metadata "docker"="true" should enable even without project-level.
	cfg.ProjectCache = map[string]config.ProjectCacheEntry{"proj": {}}
	spec = BuildSpecFromBeadInfo(cfg, "proj", "crew", "crew", "agent1", map[string]string{"docker": "true"})
	if !spec.Docker {
		t.Error("expected Docker=true when metadata enables it")
	}
}

func TestApplyProjectDefaults_PostgresEnabled(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"pgproj": {PostgresEnabled: true},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "pgproj",
		Env:     map[string]string{},
	}
	applyProjectDefaults(cfg, spec)

	if !spec.Postgres {
		t.Error("expected Postgres=true when project has PostgresEnabled")
	}
}

func TestApplyProjectDefaults_PostgresDisabledByDefault(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"nopg": {},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "nopg",
		Env:     map[string]string{},
	}
	applyProjectDefaults(cfg, spec)

	if spec.Postgres {
		t.Error("expected Postgres=false when project does not enable it")
	}
}

func TestBuildSpecFromBeadInfo_PostgresMetadataOverride(t *testing.T) {
	cfg := &config.Config{
		Namespace: "test",
		CoopImage: "agent:latest",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"proj": {PostgresEnabled: true},
		},
	}

	// Agent metadata "postgres"="false" should override project-level enable.
	spec := BuildSpecFromBeadInfo(cfg, "proj", "crew", "crew", "agent1", map[string]string{"postgres": "false"})
	if spec.Postgres {
		t.Error("expected Postgres=false when metadata overrides project default")
	}

	// Agent metadata "postgres"="true" should enable even without project-level.
	cfg.ProjectCache = map[string]config.ProjectCacheEntry{"proj": {}}
	spec = BuildSpecFromBeadInfo(cfg, "proj", "crew", "crew", "agent1", map[string]string{"postgres": "true"})
	if !spec.Postgres {
		t.Error("expected Postgres=true when metadata enables it")
	}
}

// ── Combined Docker + Postgres sidecar tests ──────────────────────────────

func TestApplyProjectDefaults_BothDockerAndPostgresEnabled(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"fullstack": {DockerEnabled: true, PostgresEnabled: true},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "fullstack",
		Env:     map[string]string{},
	}
	applyProjectDefaults(cfg, spec)

	if !spec.Docker {
		t.Error("expected Docker=true when project has DockerEnabled")
	}
	if !spec.Postgres {
		t.Error("expected Postgres=true when project has PostgresEnabled")
	}
}

func TestBuildSpecFromBeadInfo_BothSidecarsFromProject(t *testing.T) {
	cfg := &config.Config{
		Namespace: "test",
		CoopImage: "agent:latest",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"monorepo": {DockerEnabled: true, PostgresEnabled: true},
		},
	}
	spec := BuildSpecFromBeadInfo(cfg, "monorepo", "crew", "crew", "agent1", map[string]string{})

	if !spec.Docker {
		t.Error("expected Docker=true from project defaults")
	}
	if !spec.Postgres {
		t.Error("expected Postgres=true from project defaults")
	}
}

func TestBuildSpecFromBeadInfo_MetadataOverridesBothSidecars(t *testing.T) {
	cfg := &config.Config{
		Namespace: "test",
		CoopImage: "agent:latest",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"proj": {DockerEnabled: true, PostgresEnabled: true},
		},
	}

	// Metadata can disable both sidecars even when project enables them.
	spec := BuildSpecFromBeadInfo(cfg, "proj", "crew", "crew", "agent1",
		map[string]string{"docker": "false", "postgres": "false"})
	if spec.Docker {
		t.Error("expected Docker=false when metadata overrides")
	}
	if spec.Postgres {
		t.Error("expected Postgres=false when metadata overrides")
	}

	// Metadata can enable both sidecars even when project disables them.
	cfg.ProjectCache = map[string]config.ProjectCacheEntry{"proj": {}}
	spec = BuildSpecFromBeadInfo(cfg, "proj", "crew", "crew", "agent1",
		map[string]string{"docker": "true", "postgres": "true"})
	if !spec.Docker {
		t.Error("expected Docker=true when metadata enables it")
	}
	if !spec.Postgres {
		t.Error("expected Postgres=true when metadata enables it")
	}
}

func TestBuildAgentPodSpec_BothSidecarsFromEvent(t *testing.T) {
	cfg := &config.Config{
		Namespace: "test",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"monorepo": {DockerEnabled: true, PostgresEnabled: true},
		},
	}
	event := subscriber.Event{
		Project:   "monorepo",
		Role:      "crew",
		AgentName: "stamp-agent",
		Metadata:  map[string]string{},
	}
	spec := buildAgentPodSpec(cfg, event)

	if !spec.Docker {
		t.Error("expected Docker=true from project defaults via event path")
	}
	if !spec.Postgres {
		t.Error("expected Postgres=true from project defaults via event path")
	}
}

func TestApplyCommonConfig_S3EnvVars_Empty(t *testing.T) {
	cfg := &config.Config{}
	spec := &podmanager.AgentPodSpec{
		Project: "testproj",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if _, ok := spec.Env["COOP_S3_BUCKET"]; ok {
		t.Error("COOP_S3_BUCKET should not be set when CoopS3Bucket is empty")
	}
	if _, ok := spec.Env["COOP_S3_PREFIX"]; ok {
		t.Error("COOP_S3_PREFIX should not be set when CoopS3Prefix is empty")
	}
	if _, ok := spec.Env["COOP_S3_REGION"]; ok {
		t.Error("COOP_S3_REGION should not be set when CoopS3Region is empty")
	}
	if _, ok := spec.Env["COOP_S3_IMAGES_BUCKET"]; ok {
		t.Error("COOP_S3_IMAGES_BUCKET should not be set when CoopS3ImagesBucket is empty")
	}
}

// --- NeedsK8sAPI (automountServiceAccountToken opt-in; ka-0000006344 / C3) ---

func TestBuildAgentPodSpec_NeedsK8sAPIDefaultsFalse(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	event := subscriber.Event{
		Project: "gardenio", Role: "crew", AgentName: "a",
		Metadata: map[string]string{},
	}
	spec := buildAgentPodSpec(cfg, event)

	if spec.NeedsK8sAPI {
		t.Error("NeedsK8sAPI should default to false")
	}
}

func TestBuildAgentPodSpec_NeedsK8sAPIViaProjectCache(t *testing.T) {
	cfg := &config.Config{
		Namespace: "test",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {NeedsK8sAPI: true},
		},
	}
	event := subscriber.Event{
		Project: "gardenio", Role: "crew", AgentName: "a",
		Metadata: map[string]string{},
	}
	spec := buildAgentPodSpec(cfg, event)

	if !spec.NeedsK8sAPI {
		t.Error("NeedsK8sAPI should be true when project cache opts in")
	}
}

func TestBuildAgentPodSpec_NeedsK8sAPIViaRoleCache(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		ProjectCache: map[string]config.ProjectCacheEntry{},
		RoleCache: map[string]config.RoleCacheEntry{
			"gardener-e2e": {NeedsK8sAPI: true},
		},
	}
	event := subscriber.Event{
		Project: "gardenio", Role: "gardener-e2e", AgentName: "a",
		Metadata: map[string]string{},
	}
	spec := buildAgentPodSpec(cfg, event)

	if !spec.NeedsK8sAPI {
		t.Error("NeedsK8sAPI should be true when role cache opts in")
	}
}

func TestBuildAgentPodSpec_NeedsK8sAPIMetadataFalseOverridesProject(t *testing.T) {
	cfg := &config.Config{
		Namespace: "test",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {NeedsK8sAPI: true},
		},
	}
	event := subscriber.Event{
		Project: "gardenio", Role: "crew", AgentName: "a",
		Metadata: map[string]string{"needs_k8s_api": "false"},
	}
	spec := buildAgentPodSpec(cfg, event)

	if spec.NeedsK8sAPI {
		t.Error("agent-level needs_k8s_api='false' should override project opt-in")
	}
}

func TestApplyProjectDefaults_SessionResumeOptOut(t *testing.T) {
	optOut := false
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"noresume": {SessionResume: &optOut},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "noresume", Env: map[string]string{}}
	applyProjectDefaults(cfg, spec)

	if spec.SessionResume == nil || *spec.SessionResume {
		t.Errorf("expected SessionResume=&false, got %v", spec.SessionResume)
	}
}

func TestApplyProjectDefaults_SessionResumeUnsetLeavesNil(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"unset": {},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "unset", Env: map[string]string{}}
	applyProjectDefaults(cfg, spec)

	if spec.SessionResume != nil {
		t.Errorf("expected SessionResume=nil when project unset, got %v", *spec.SessionResume)
	}
}

func TestBuildSpecFromBeadInfo_SessionResumeMetadataOverride(t *testing.T) {
	optIn := true
	cfg := &config.Config{
		Namespace: "test",
		CoopImage: "agent:latest",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"proj": {SessionResume: &optIn},
		},
	}

	// Agent metadata "session_resume"="false" should override project opt-in.
	spec := BuildSpecFromBeadInfo(cfg, "proj", "crew", "crew", "a1",
		map[string]string{"session_resume": "false"})
	if spec.SessionResume == nil || *spec.SessionResume {
		t.Errorf("expected SessionResume=&false from metadata, got %v", spec.SessionResume)
	}

	// Agent metadata "session_resume"="true" should override project opt-out.
	optOut := false
	cfg.ProjectCache = map[string]config.ProjectCacheEntry{"proj": {SessionResume: &optOut}}
	spec = BuildSpecFromBeadInfo(cfg, "proj", "crew", "crew", "a1",
		map[string]string{"session_resume": "true"})
	if spec.SessionResume == nil || !*spec.SessionResume {
		t.Errorf("expected SessionResume=&true from metadata, got %v", spec.SessionResume)
	}
}
