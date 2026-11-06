package main

import (
	"testing"

	"gardenio/controller/internal/config"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/podmanager"
)

// Phase 0 instrumentation (ka-0000002142, epic ka-0000007694): applyAccountConfig
// stamps spec.AccountLayer with the layer that produced (or refused) the pod's
// account. These tests lock the mapping from each resolution path to its enum
// value so the gardenio_agent_account_layer_total series stays trustworthy — the
// evidence that de-risks Phases 2–3 (docs/account-routing.md §7). No behavior
// change is asserted here beyond the recording; the account/model/env outcomes
// are covered by the sibling podspec tests.

const layerLitellm = "http://litellm:4000"

// TestAccountLayer_Spread: the common path — selectAccount picks a healthy
// project-pool account, so the layer is "spread" and a credential is baked.
func TestAccountLayer_Spread(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-a": {BaseURL: layerLitellm, LiteLLMKey: "sk-a", Status: "active", ThrottleStatus: "allowed", Labels: []string{"project:gardenio"}},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", AgentName: "a"}
	applyAccountConfig(cfg, spec)
	if spec.AccountLayer != gardeniometrics.AccountLayerSpread {
		t.Fatalf("AccountLayer: got %q, want %q", spec.AccountLayer, gardeniometrics.AccountLayerSpread)
	}
	if spec.ResolvedAccount != "acct-a" {
		t.Errorf("ResolvedAccount: got %q, want acct-a", spec.ResolvedAccount)
	}
}

// TestAccountLayer_RolePin: a role pins an account, which wins over spread even
// when a spreadable pool exists.
func TestAccountLayer_RolePin(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"fsm-triage": {Account: "acct-pin"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-pin":  {BaseURL: layerLitellm, LiteLLMKey: "sk-p", Status: "active", ThrottleStatus: "allowed", Labels: []string{"project:gardenio"}},
			"acct-pool": {BaseURL: layerLitellm, LiteLLMKey: "sk-x", Status: "active", ThrottleStatus: "allowed", Labels: []string{"project:gardenio"}},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "fsm-triage", AgentName: "a"}
	applyAccountConfig(cfg, spec)
	if spec.AccountLayer != gardeniometrics.AccountLayerRolePin {
		t.Fatalf("AccountLayer: got %q, want %q", spec.AccountLayer, gardeniometrics.AccountLayerRolePin)
	}
	if spec.ResolvedAccount != "acct-pin" {
		t.Errorf("ResolvedAccount: got %q, want acct-pin", spec.ResolvedAccount)
	}
}

// Phase 2 (ka-0000004094, account-routing.md §7) removed the layer-3 project-cache
// and layer-4 DefaultAccount fall-throughs, so the former TestAccountLayer_
// ProjectCache and TestAccountLayer_DefaultAccount were deleted with the layers
// they pinned. Phase 3 (ka-0000007959) then removed the DefaultAccount config,
// env, and the AccountLayerDefaultAccount enum member entirely. The remaining
// AccountLayerProjectCache / AccountLayerRedirectChanged enum members stay valid
// (the metric still accepts these historical series) but are no longer stamped
// by applyAccountConfig.

// TestAccountLayer_NoCredentialBenign: a project with no account beads at all
// resolves nothing — the benign direct-provider-auth case. No credential baked,
// layer "no-credential-benign".
func TestAccountLayer_NoCredentialBenign(t *testing.T) {
	cfg := &config.Config{AccountCache: map[string]config.AccountCacheEntry{}}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", AgentName: "a"}
	applyAccountConfig(cfg, spec)
	if spec.AccountLayer != gardeniometrics.AccountLayerNoCredentialBenign {
		t.Fatalf("AccountLayer: got %q, want %q", spec.AccountLayer, gardeniometrics.AccountLayerNoCredentialBenign)
	}
	if spec.ResolvedAccount != "" {
		t.Errorf("ResolvedAccount: got %q, want empty", spec.ResolvedAccount)
	}
}

// TestAccountLayer_NoCredentialWarn: a project that HAS credentialed account
// beads but resolution excludes them all (the pool account is exhausted, no
// ProjectCache/DefaultAccount fallback) — the loud WARN case (ka-0000007343).
func TestAccountLayer_NoCredentialWarn(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			// Credentialed + project-labeled (so countCredentialedProjectAccounts
			// sees it) but exhausted, so selectAccount excludes it and there
			// is no fallback account name to resolve.
			"acct-dead": {BaseURL: layerLitellm, LiteLLMKey: "sk-x", Status: "exhausted", Labels: []string{"project:gardenio"}},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", AgentName: "a"}
	applyAccountConfig(cfg, spec)
	if spec.AccountLayer != gardeniometrics.AccountLayerNoCredentialWarn {
		t.Fatalf("AccountLayer: got %q, want %q", spec.AccountLayer, gardeniometrics.AccountLayerNoCredentialWarn)
	}
}

// TestAccountLayer_EscapeHatchBlocked: an account name resolves (via a role pin,
// the only non-spread selection path left after Phase 2 — ka-0000004094) but the
// finally-resolved account is capacity-exhausted and non-exclusive, so the
// pool-exhaustion escape hatch sets SpawnBlocked and records layer
// "escape-hatch-blocked" (ka-0000002560). The spread pool already excludes
// exhausted accounts, so a role pin is now the only way an exhausted account
// reaches the escape hatch.
func TestAccountLayer_EscapeHatchBlocked(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {Account: "acct-exhausted"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			// Role-pinned, exhausted + non-exclusive → escape hatch fires.
			"acct-exhausted": {BaseURL: layerLitellm, LiteLLMKey: "sk-x", Status: "exhausted"},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", AgentName: "a"}
	applyAccountConfig(cfg, spec)
	if spec.AccountLayer != gardeniometrics.AccountLayerEscapeHatchBlocked {
		t.Fatalf("AccountLayer: got %q, want %q", spec.AccountLayer, gardeniometrics.AccountLayerEscapeHatchBlocked)
	}
	if !spec.SpawnBlocked {
		t.Errorf("SpawnBlocked: got false, want true")
	}
}

// TestAccountLayer_UnknownAccount: a role pins an account name that is absent
// from AccountCache — config drift. No credential baked, layer "unknown-account".
func TestAccountLayer_UnknownAccount(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"crew": {Account: "acct-ghost"},
		},
		AccountCache: map[string]config.AccountCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", AgentName: "a"}
	applyAccountConfig(cfg, spec)
	if spec.AccountLayer != gardeniometrics.AccountLayerUnknownAccount {
		t.Fatalf("AccountLayer: got %q, want %q", spec.AccountLayer, gardeniometrics.AccountLayerUnknownAccount)
	}
}

// TestRecordAccountLayer_DropsOutOfEnum: the metric guard drops an empty or
// unknown layer so a mis-wired caller cannot inflate cardinality, and accepts a
// valid enum value.
func TestRecordAccountLayer_DropsOutOfEnum(t *testing.T) {
	em := gardeniometrics.NewEventMetrics("test")
	if em.RecordAccountLayer("", "gardenio", "crew") {
		t.Errorf("empty layer: got recorded=true, want false")
	}
	if em.RecordAccountLayer("bogus-layer", "gardenio", "crew") {
		t.Errorf("out-of-enum layer: got recorded=true, want false")
	}
	if !em.RecordAccountLayer(gardeniometrics.AccountLayerSpread, "gardenio", "crew") {
		t.Errorf("valid layer: got recorded=false, want true")
	}
	// ka-0000006314: the new layer must survive the bounded-enum filter, or the
	// series it exists to expose would be silently dropped.
	if !em.RecordAccountLayer(gardeniometrics.AccountLayerUnlabeledPin, "devsecops", "crew") {
		t.Errorf("unlabeled-pin layer: got recorded=false, want true")
	}
	// Nil receiver is a clean no-op.
	var nilEM *gardeniometrics.EventMetrics
	if nilEM.RecordAccountLayer(gardeniometrics.AccountLayerSpread, "gardenio", "crew") {
		t.Errorf("nil receiver: got recorded=true, want false")
	}
}

// TestAccountLayer_UnlabeledPin: the devsecops regression (ka-0000006314). A
// project bead pins `account: <name>` on an account that EXISTS but carries no
// project:<name> label. Phase 2 retired the layer-3 reader of that field, so
// role-pin / spread / opus-fallback all decline; countCredentialedProjectAccounts
// also keys off the label, so it reports 0 and the loud NoCredentialWarn branch
// does NOT fire. Before this fix the pod fell into the benign Debug branch and
// spawned with no ANTHROPIC_* env, 401-ing on request 1 with zero tokens. Assert
// the distinct, countable layer so the condition can never be silent again.
func TestAccountLayer_UnlabeledPin(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			// Healthy and credentialed, but labeled for a DIFFERENT project —
			// exactly gardenio2 vs project devsecops.
			"acct-pinned": {BaseURL: layerLitellm, LiteLLMKey: "sk-a", Status: "active", ThrottleStatus: "allowed", Labels: []string{"project:other"}},
		},
		ProjectCache: map[string]config.ProjectCacheEntry{
			"devsecops": {Account: "acct-pinned"},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "devsecops", Role: "crew", AgentName: "a"}
	applyAccountConfig(cfg, spec)
	if spec.AccountLayer != gardeniometrics.AccountLayerUnlabeledPin {
		t.Fatalf("AccountLayer: got %q, want %q", spec.AccountLayer, gardeniometrics.AccountLayerUnlabeledPin)
	}
	// Still a refusal, not a spawn-block: no credential is baked and the spec is
	// left spawnable, so a project with a vestigial account field is unchanged.
	if spec.ResolvedAccount != "" {
		t.Errorf("ResolvedAccount: got %q, want empty", spec.ResolvedAccount)
	}
	if spec.SpawnBlocked {
		t.Error("SpawnBlocked: got true, want false — an unlabeled pin warns, it does not block")
	}
	if _, ok := spec.Env["ANTHROPIC_BASE_URL"]; ok {
		t.Error("ANTHROPIC_BASE_URL must not be baked when no account resolves")
	}
}

// TestAccountLayer_UnlabeledPin_LabelPresentResolves is the positive control for
// the fix applied to the live account beads: adding project:<name> to the pinned
// account makes it resolve through the normal spread path and bake a credential.
func TestAccountLayer_UnlabeledPin_LabelPresentResolves(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-pinned": {BaseURL: layerLitellm, LiteLLMKey: "sk-a", Status: "active", ThrottleStatus: "allowed", Labels: []string{"project:other", "project:devsecops"}},
		},
		ProjectCache: map[string]config.ProjectCacheEntry{
			"devsecops": {Account: "acct-pinned"},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "devsecops", Role: "crew", AgentName: "a"}
	applyAccountConfig(cfg, spec)
	if spec.AccountLayer != gardeniometrics.AccountLayerSpread {
		t.Fatalf("AccountLayer: got %q, want %q", spec.AccountLayer, gardeniometrics.AccountLayerSpread)
	}
	if spec.ResolvedAccount != "acct-pinned" {
		t.Errorf("ResolvedAccount: got %q, want acct-pinned", spec.ResolvedAccount)
	}
	if spec.Env["ANTHROPIC_BASE_URL"] != layerLitellm {
		t.Errorf("ANTHROPIC_BASE_URL: got %q, want %q", spec.Env["ANTHROPIC_BASE_URL"], layerLitellm)
	}
}

// TestAccountLayer_UnknownPinStaysBenign: a project pinning an account name that
// does NOT exist in the cache (a dangling field, e.g. e2e audit fixtures) keeps
// the benign Debug classification. The unlabeled-pin warn is reserved for a pin
// that names a real account, so a dangling fixture cannot spam the log.
func TestAccountLayer_UnknownPinStaysBenign(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{},
		ProjectCache: map[string]config.ProjectCacheEntry{
			"audit-dangling-acct": {Account: "does-not-exist-acct"},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "audit-dangling-acct", Role: "crew", AgentName: "a"}
	applyAccountConfig(cfg, spec)
	if spec.AccountLayer != gardeniometrics.AccountLayerNoCredentialBenign {
		t.Fatalf("AccountLayer: got %q, want %q", spec.AccountLayer, gardeniometrics.AccountLayerNoCredentialBenign)
	}
}
