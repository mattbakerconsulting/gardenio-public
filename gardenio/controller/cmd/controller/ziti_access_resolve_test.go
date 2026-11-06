package main

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// baseZitiCfg returns a config with the broker + tunneler configured (the
// master switch ON) so resolution can produce a populated ZitiAccess.
func baseZitiCfg() *config.Config {
	return &config.Config{
		ZitiAccessBrokerURL:     "http://broker.ziti.svc",
		ZitiAccessTokenAudience: "gardenio-ziti-broker",
		ZitiTunnelerImage:       "openziti/ziti-edge-tunnel:1.0.0",
		ZitiAccessDefaultTTL:    "30m",
		ProjectCache:            map[string]config.ProjectCacheEntry{},
		RoleCache:               map[string]config.RoleCacheEntry{},
	}
}

func newZitiSpec(project, role string) *podmanager.AgentPodSpec {
	return &podmanager.AgentPodSpec{
		Project: project,
		Role:    role,
		BeadID:  "gd-task-1",
		Env:     map[string]string{},
	}
}

// TestApplyZitiAccess_MasterSwitchOff: even if a project opts in, no broker URL
// configured => feature stays OFF. This is the can't-wedge-a-broker-less-cluster
// guarantee.
func TestApplyZitiAccess_MasterSwitchOff(t *testing.T) {
	cfg := baseZitiCfg()
	cfg.ZitiAccessBrokerURL = "" // master switch off
	cfg.ProjectCache["p"] = config.ProjectCacheEntry{ZitiAccessEnabled: true, ZitiServiceID: "svc-1"}

	spec := newZitiSpec("p", "dev")
	applyZitiAccess(cfg, spec, map[string]string{})

	if spec.ZitiAccess != nil {
		t.Error("ZitiAccess should be nil when broker URL is unset")
	}
}

// TestApplyZitiAccess_NotEnabled: broker configured but no bead opts in => OFF.
func TestApplyZitiAccess_NotEnabled(t *testing.T) {
	cfg := baseZitiCfg()
	spec := newZitiSpec("p", "dev")
	applyZitiAccess(cfg, spec, map[string]string{})
	if spec.ZitiAccess != nil {
		t.Error("ZitiAccess should be nil when no project/role/agent enables it")
	}
}

// TestApplyZitiAccess_ProjectEnabled: a project opt-in with a service id
// produces a populated ZitiAccess, with the controller's default TTL applied.
func TestApplyZitiAccess_ProjectEnabled(t *testing.T) {
	cfg := baseZitiCfg()
	cfg.ProjectCache["p"] = config.ProjectCacheEntry{
		ZitiAccessEnabled: true,
		ZitiServiceID:     "svc-proj",
	}
	spec := newZitiSpec("p", "dev")
	applyZitiAccess(cfg, spec, map[string]string{})

	if spec.ZitiAccess == nil {
		t.Fatal("ZitiAccess should be populated")
	}
	if spec.ZitiAccess.ServiceID != "svc-proj" {
		t.Errorf("ServiceID = %q, want svc-proj", spec.ZitiAccess.ServiceID)
	}
	if spec.ZitiAccess.TTL != "30m" {
		t.Errorf("TTL = %q, want controller default 30m", spec.ZitiAccess.TTL)
	}
	if spec.ZitiAccess.TunnelerImage != cfg.ZitiTunnelerImage {
		t.Errorf("TunnelerImage = %q", spec.ZitiAccess.TunnelerImage)
	}
}

// TestApplyZitiAccess_EnabledButNoServiceID: opt-in without a resolvable service
// id => feature OFF (no broken pod), not a populated-but-invalid spec.
func TestApplyZitiAccess_EnabledButNoServiceID(t *testing.T) {
	cfg := baseZitiCfg()
	cfg.ProjectCache["p"] = config.ProjectCacheEntry{ZitiAccessEnabled: true} // no ServiceID
	spec := newZitiSpec("p", "dev")
	applyZitiAccess(cfg, spec, map[string]string{})
	if spec.ZitiAccess != nil {
		t.Error("ZitiAccess should be nil when enabled but no service id resolved")
	}
}

// TestApplyZitiAccess_EnabledButNoTunnelerImage: opt-in with a service id but no
// tunneler image configured => OFF (fail-safe).
func TestApplyZitiAccess_EnabledButNoTunnelerImage(t *testing.T) {
	cfg := baseZitiCfg()
	cfg.ZitiTunnelerImage = ""
	cfg.ProjectCache["p"] = config.ProjectCacheEntry{ZitiAccessEnabled: true, ZitiServiceID: "svc-1"}
	spec := newZitiSpec("p", "dev")
	applyZitiAccess(cfg, spec, map[string]string{})
	if spec.ZitiAccess != nil {
		t.Error("ZitiAccess should be nil when no tunneler image is configured")
	}
}

// TestApplyZitiAccess_RoleOverridesProject: role-level service id + bind wins.
func TestApplyZitiAccess_RoleOverridesProject(t *testing.T) {
	cfg := baseZitiCfg()
	cfg.ProjectCache["p"] = config.ProjectCacheEntry{
		ZitiAccessEnabled: true,
		ZitiServiceID:     "svc-proj",
		ZitiTTL:           "10m",
	}
	cfg.RoleCache["special"] = config.RoleCacheEntry{
		ZitiAccessEnabled: true,
		ZitiServiceID:     "svc-role",
		ZitiBind:          true,
		ZitiTTL:           "5m",
	}
	spec := newZitiSpec("p", "special")
	applyZitiAccess(cfg, spec, map[string]string{})

	if spec.ZitiAccess == nil {
		t.Fatal("ZitiAccess should be populated")
	}
	if spec.ZitiAccess.ServiceID != "svc-role" {
		t.Errorf("ServiceID = %q, want role override svc-role", spec.ZitiAccess.ServiceID)
	}
	if !spec.ZitiAccess.Bind {
		t.Error("Bind should be true from role override")
	}
	if spec.ZitiAccess.TTL != "5m" {
		t.Errorf("TTL = %q, want role override 5m", spec.ZitiAccess.TTL)
	}
}

// TestApplyZitiAccess_AgentMetadataKillSwitch: metadata ziti_access=false
// disables even when project/role enabled it.
func TestApplyZitiAccess_AgentMetadataKillSwitch(t *testing.T) {
	cfg := baseZitiCfg()
	cfg.ProjectCache["p"] = config.ProjectCacheEntry{ZitiAccessEnabled: true, ZitiServiceID: "svc-1"}
	spec := newZitiSpec("p", "dev")
	applyZitiAccess(cfg, spec, map[string]string{"ziti_access": "false"})
	if spec.ZitiAccess != nil {
		t.Error("agent metadata ziti_access=false should disable the feature")
	}
}

// TestApplyZitiAccess_AgentMetadataOptIn: metadata can enable + parameterize on
// its own (no project/role config).
func TestApplyZitiAccess_AgentMetadataOptIn(t *testing.T) {
	cfg := baseZitiCfg()
	spec := newZitiSpec("p", "dev")
	applyZitiAccess(cfg, spec, map[string]string{
		"ziti_access":     "true",
		"ziti_service_id": "svc-meta",
		"ziti_bind":       "true",
		"ziti_ttl":        "2m",
		"ziti_optional":   "true",
	})
	if spec.ZitiAccess == nil {
		t.Fatal("ZitiAccess should be populated from agent metadata")
	}
	if spec.ZitiAccess.ServiceID != "svc-meta" {
		t.Errorf("ServiceID = %q, want svc-meta", spec.ZitiAccess.ServiceID)
	}
	if !spec.ZitiAccess.Bind {
		t.Error("Bind should be true from metadata")
	}
	if spec.ZitiAccess.TTL != "2m" {
		t.Errorf("TTL = %q, want 2m", spec.ZitiAccess.TTL)
	}
	if !spec.ZitiAccess.Optional {
		t.Error("Optional should be true from metadata")
	}
}

// TestApplyZitiAccess_TokenAudiencePassthrough: a custom audience flows into the
// spec.
func TestApplyZitiAccess_TokenAudiencePassthrough(t *testing.T) {
	cfg := baseZitiCfg()
	cfg.ZitiAccessTokenAudience = "my-aud"
	cfg.ProjectCache["p"] = config.ProjectCacheEntry{ZitiAccessEnabled: true, ZitiServiceID: "svc-1"}
	spec := newZitiSpec("p", "dev")
	applyZitiAccess(cfg, spec, map[string]string{})
	if spec.ZitiAccess == nil || spec.ZitiAccess.TokenAudience != "my-aud" {
		t.Errorf("TokenAudience passthrough failed: %+v", spec.ZitiAccess)
	}
}
