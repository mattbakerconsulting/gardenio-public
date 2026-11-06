package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

func TestApplyRoleDefaults_BasicOverrides(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {
				Image:          "ghcr.io/joyousaversion/gardenio/agent:custom",
				CPURequest:     "500m",
				CPULimit:       "2000m",
				MemoryRequest:  "1Gi",
				MemoryLimit:    "4Gi",
				ServiceAccount: "crew-sa",
				StorageClass:   "fast-ssd",
				DockerEnabled:  true,
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Role:             "crew",
		Env:              map[string]string{},
		WorkspaceStorage: &podmanager.WorkspaceStorageSpec{},
	}
	applyRoleDefaults(cfg, spec)

	if spec.Image != "ghcr.io/joyousaversion/gardenio/agent:custom" {
		t.Errorf("image: got %q, want custom image", spec.Image)
	}
	if spec.ServiceAccountName != "crew-sa" {
		t.Errorf("service_account: got %q, want %q", spec.ServiceAccountName, "crew-sa")
	}
	if spec.WorkspaceStorage.StorageClassName != "fast-ssd" {
		t.Errorf("storage_class: got %q, want %q", spec.WorkspaceStorage.StorageClassName, "fast-ssd")
	}
	if !spec.Docker {
		t.Error("docker: expected true")
	}
	if spec.Resources == nil {
		t.Fatal("expected resources to be set")
	}
	if got := spec.Resources.Requests[corev1.ResourceCPU]; got.String() != "500m" {
		t.Errorf("cpu_request: got %q, want %q", got.String(), "500m")
	}
	if got := spec.Resources.Limits[corev1.ResourceMemory]; got.String() != "4Gi" {
		t.Errorf("memory_limit: got %q, want %q", got.String(), "4Gi")
	}
}

func TestApplyRoleDefaults_RuntimeClassName(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"gvisor-poc": {
				RuntimeClassName: "runsc",
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Role: "gvisor-poc",
		Env:  map[string]string{},
	}
	applyRoleDefaults(cfg, spec)

	if spec.RuntimeClassName != "runsc" {
		t.Errorf("runtime_class: got %q, want %q", spec.RuntimeClassName, "runsc")
	}
}

func TestApplyRoleDefaults_NoMatchingRole(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"gardener": {Image: "gardener-image"},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Role:  "crew",
		Image: "default-image",
		Env:   map[string]string{},
	}
	applyRoleDefaults(cfg, spec)

	if spec.Image != "default-image" {
		t.Errorf("image should remain unchanged, got %q", spec.Image)
	}
}

func TestApplyRoleDefaults_EmptyRoleCache(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Role:  "crew",
		Image: "default-image",
		Env:   map[string]string{},
	}
	applyRoleDefaults(cfg, spec)

	if spec.Image != "default-image" {
		t.Errorf("image should remain unchanged, got %q", spec.Image)
	}
}

func TestApplyRoleDefaults_EmptyRole(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {Image: "crew-image"},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Role:  "",
		Image: "default-image",
		Env:   map[string]string{},
	}
	applyRoleDefaults(cfg, spec)

	if spec.Image != "default-image" {
		t.Errorf("image should remain unchanged for empty role, got %q", spec.Image)
	}
}

func TestApplyRoleDefaults_MultiRole_FirstRoleWins(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"thread": {
				Image:       "thread-image",
				CPURequest:  "250m",
				MemoryLimit: "2Gi",
			},
			"crew": {
				Image:       "crew-image",
				CPURequest:  "500m",
				MemoryLimit: "4Gi",
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Role: "thread,crew",
		Env:  map[string]string{},
	}
	applyRoleDefaults(cfg, spec)

	// First role (thread) wins for non-additive fields.
	if spec.Image != "thread-image" {
		t.Errorf("image: got %q, want %q (first role wins)", spec.Image, "thread-image")
	}
	if got := spec.Resources.Requests[corev1.ResourceCPU]; got.String() != "250m" {
		t.Errorf("cpu_request: got %q, want %q (first role wins)", got.String(), "250m")
	}
}

func TestApplyRoleDefaults_MultiRole_EnvMerged(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"thread": {
				EnvOverrides: map[string]string{"THREAD_VAR": "from-thread"},
				EnvVars: []beadsapi.EnvEntry{
					{Name: "SHARED_VAR", Value: "from-thread"},
				},
			},
			"crew": {
				EnvOverrides: map[string]string{"CREW_VAR": "from-crew"},
				EnvVars: []beadsapi.EnvEntry{
					{Name: "SHARED_VAR", Value: "from-crew"}, // should NOT override thread's
					{Name: "CREW_ONLY", Value: "crew-value"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Role: "thread,crew",
		Env:  map[string]string{},
	}
	applyRoleDefaults(cfg, spec)

	// EnvOverrides are additive — both roles contribute.
	if spec.Env["THREAD_VAR"] != "from-thread" {
		t.Errorf("THREAD_VAR: got %q, want %q", spec.Env["THREAD_VAR"], "from-thread")
	}
	if spec.Env["CREW_VAR"] != "from-crew" {
		t.Errorf("CREW_VAR: got %q, want %q", spec.Env["CREW_VAR"], "from-crew")
	}

	// EnvVars: first role sets SHARED_VAR, second role should NOT override.
	if spec.Env["SHARED_VAR"] != "from-thread" {
		t.Errorf("SHARED_VAR: got %q, want %q (first role wins for EnvVars)", spec.Env["SHARED_VAR"], "from-thread")
	}
	if spec.Env["CREW_ONLY"] != "crew-value" {
		t.Errorf("CREW_ONLY: got %q, want %q", spec.Env["CREW_ONLY"], "crew-value")
	}
}

func TestApplyRoleDefaults_PostgresEnabled(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"data": {PostgresEnabled: true},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Role: "data",
		Env:  map[string]string{},
	}
	applyRoleDefaults(cfg, spec)

	if !spec.Postgres {
		t.Error("postgres: expected true")
	}
}

func TestBuildSpecFromBeadInfo_RoleOverridesProject(t *testing.T) {
	cfg := &config.Config{
		Namespace: "test",
		CoopImage: "default-image",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {
				Image:       "project-image",
				CPURequest:  "250m",
				MemoryLimit: "2Gi",
			},
		},
		AccountCache: map[string]config.AccountCacheEntry{},
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {
				Image:       "role-image",
				CPURequest:  "500m",
				MemoryLimit: "4Gi",
			},
		},
	}

	spec := BuildSpecFromBeadInfo(cfg, "myproject", "crew", "crew", "test-agent", map[string]string{})

	// Role overrides project for non-additive fields.
	if spec.Image != "role-image" {
		t.Errorf("image: got %q, want %q (role overrides project)", spec.Image, "role-image")
	}
	if got := spec.Resources.Requests[corev1.ResourceCPU]; got.String() != "500m" {
		t.Errorf("cpu_request: got %q, want %q (role overrides project)", got.String(), "500m")
	}
}

func TestApplyRoleSecrets_InjectsSecrets(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {},
		},
		AccountCache: map[string]config.AccountCacheEntry{},
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {
				Secrets: []beadsapi.SecretEntry{
					{Env: "GITHUB_TOKEN", Secret: "crew-gh-token", Key: "token"},
					{Env: "CUSTOM_SECRET", Secret: "crew-custom", Key: "value"},
				},
				EnvVars: []beadsapi.EnvEntry{
					{Name: "ROLE_CONFIG", Value: "from-role"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Role:    "crew",
		Env:     map[string]string{},
	}
	applyRoleSecrets(cfg, spec)

	// Check secrets were injected.
	found := map[string]bool{}
	for _, se := range spec.SecretEnv {
		found[se.EnvName] = true
		if se.EnvName == "GITHUB_TOKEN" && se.SecretName != "crew-gh-token" {
			t.Errorf("GITHUB_TOKEN secret: got %q, want %q", se.SecretName, "crew-gh-token")
		}
	}
	if !found["GITHUB_TOKEN"] {
		t.Error("expected GITHUB_TOKEN secret to be injected")
	}
	if !found["CUSTOM_SECRET"] {
		t.Error("expected CUSTOM_SECRET secret to be injected")
	}

	// Check plain env vars.
	if spec.Env["ROLE_CONFIG"] != "from-role" {
		t.Errorf("ROLE_CONFIG: got %q, want %q", spec.Env["ROLE_CONFIG"], "from-role")
	}
}

func TestApplyRoleSecrets_PrefixEnforcement(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {
				Secrets: []beadsapi.SecretEntry{
					{Env: "GOOD_SECRET", Secret: "crew-good", Key: "token"}, // valid prefix
					{Env: "BAD_SECRET", Secret: "other-bad", Key: "token"},  // invalid prefix
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Role: "crew",
		Env:  map[string]string{},
	}
	applyRoleSecrets(cfg, spec)

	found := map[string]bool{}
	for _, se := range spec.SecretEnv {
		found[se.EnvName] = true
	}
	if !found["GOOD_SECRET"] {
		t.Error("expected GOOD_SECRET (valid prefix) to be injected")
	}
	if found["BAD_SECRET"] {
		t.Error("expected BAD_SECRET (invalid prefix) to be skipped")
	}
}

func TestApplyRoleSecrets_OverridesProjectSecrets(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {
				Secrets: []beadsapi.SecretEntry{
					{Env: "GITHUB_TOKEN", Secret: "myproject-gh", Key: "token"},
				},
			},
		},
		AccountCache: map[string]config.AccountCacheEntry{},
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {
				Secrets: []beadsapi.SecretEntry{
					{Env: "GITHUB_TOKEN", Secret: "crew-gh", Key: "token"}, // overrides project
				},
			},
		},
	}

	spec := BuildSpecFromBeadInfo(cfg, "myproject", "crew", "crew", "test-agent", map[string]string{})

	// Role's GITHUB_TOKEN should override project's.
	for _, se := range spec.SecretEnv {
		if se.EnvName == "GITHUB_TOKEN" {
			if se.SecretName != "crew-gh" {
				t.Errorf("GITHUB_TOKEN secret: got %q, want %q (role overrides project)", se.SecretName, "crew-gh")
			}
			return
		}
	}
	t.Error("expected GITHUB_TOKEN in SecretEnv")
}

func TestApplyRoleSecrets_GitCredentialUpdate(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"ops": {
				Secrets: []beadsapi.SecretEntry{
					{Env: "GIT_TOKEN", Secret: "ops-git-creds", Key: "token"},
					{Env: "GITLAB_TOKEN", Secret: "ops-gitlab", Key: "token"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Role: "ops",
		Env:  map[string]string{},
	}
	applyRoleSecrets(cfg, spec)

	if spec.GitCredentialsSecret != "ops-git-creds" {
		t.Errorf("GitCredentialsSecret: got %q, want %q", spec.GitCredentialsSecret, "ops-git-creds")
	}
	if spec.GitlabTokenSecret != "ops-gitlab" {
		t.Errorf("GitlabTokenSecret: got %q, want %q", spec.GitlabTokenSecret, "ops-gitlab")
	}
}

func TestBuildSpecFromBeadInfo_AgentMetadataOverridesRole(t *testing.T) {
	cfg := &config.Config{
		Namespace: "test",
		CoopImage: "default-image",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"myproject": {},
		},
		AccountCache: map[string]config.AccountCacheEntry{},
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {
				DockerEnabled:   true,
				PostgresEnabled: true,
			},
		},
	}

	// Agent metadata "docker"="false" should override role's DockerEnabled.
	spec := BuildSpecFromBeadInfo(cfg, "myproject", "crew", "crew", "test-agent", map[string]string{
		"docker":   "false",
		"postgres": "false",
	})

	if spec.Docker {
		t.Error("docker: expected false (agent metadata overrides role)")
	}
	if spec.Postgres {
		t.Error("postgres: expected false (agent metadata overrides role)")
	}
}

func TestApplyRoleDefaults_SessionResumeOverridesProject(t *testing.T) {
	// Project opts in; role opts out — role wins because it's applied later
	// in the layering order (project < role).
	projectIn := true
	roleOut := false
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"proj": {SessionResume: &projectIn},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"ephemeral": {SessionResume: &roleOut},
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "proj",
		Role:    "ephemeral",
		Env:     map[string]string{},
	}
	applyProjectDefaults(cfg, spec)
	applyRoleDefaults(cfg, spec)

	if spec.SessionResume == nil || *spec.SessionResume {
		t.Errorf("expected SessionResume=&false (role overrides project), got %v", spec.SessionResume)
	}
}

func TestApplyRoleDefaults_SessionResumeUnsetPreservesProject(t *testing.T) {
	// Role unset — keep the project's setting.
	projectOut := false
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"proj": {SessionResume: &projectOut},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {}, // SessionResume nil
		},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "proj",
		Role:    "crew",
		Env:     map[string]string{},
	}
	applyProjectDefaults(cfg, spec)
	applyRoleDefaults(cfg, spec)

	if spec.SessionResume == nil || *spec.SessionResume {
		t.Errorf("expected project setting preserved (false), got %v", spec.SessionResume)
	}
}
