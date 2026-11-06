package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/subscriber"
)

const (
	infraRoleSA = "gardenio-infra-reader"
	defaultSA   = "gardenio-agent"
)

// identityCfg has a privileged role "infra" (its own SA, API token on) beside a
// plain role "crew" that sets neither, with a chart-default SA for everything else.
func identityCfg() *config.Config {
	return &config.Config{
		Namespace:          "test",
		CoopImage:          "agent:latest",
		CoopServiceAccount: defaultSA,
		ProjectCache:       map[string]config.ProjectCacheEntry{},
		RoleCache: map[string]config.RoleCacheEntry{
			"infra": {ServiceAccount: infraRoleSA, NeedsK8sAPI: true},
			"crew":  {},
		},
	}
}

// build runs both spec builders (reconciler and events) so the two paths cannot
// drift apart on who may choose a pod's Kubernetes identity.
func build(t *testing.T, role string, metadata map[string]string, check func(t *testing.T, path, sa string, api bool)) {
	t.Helper()
	spec := BuildSpecFromBeadInfo(identityCfg(), "gardenio", role, role, "agent1", metadata)
	check(t, "reconciler", spec.ServiceAccountName, spec.NeedsK8sAPI)
	ev := buildAgentPodSpec(identityCfg(), subscriber.Event{Project: "gardenio", Role: role, AgentName: "agent1", Metadata: metadata})
	check(t, "events", ev.ServiceAccountName, ev.NeedsK8sAPI)
}

func TestAgentMetadataCannotChooseServiceAccount(t *testing.T) {
	// Agent metadata names a different SA under the privileged role: the role's SA wins.
	build(t, "infra", map[string]string{"service_account": "X"}, func(t *testing.T, path, sa string, _ bool) {
		if sa != infraRoleSA {
			t.Errorf("%s: infra agent with metadata SA X got %q, want role SA %q", path, sa, infraRoleSA)
		}
	})
	// Positive control: with no metadata SA the same role gets the same SA.
	build(t, "infra", map[string]string{}, func(t *testing.T, path, sa string, _ bool) {
		if sa != infraRoleSA {
			t.Errorf("%s: infra agent with no metadata SA got %q, want role SA %q", path, sa, infraRoleSA)
		}
	})
	// The attack: a non-privileged agent naming the infra SA stays on the chart default.
	build(t, "crew", map[string]string{"service_account": infraRoleSA}, func(t *testing.T, path, sa string, _ bool) {
		if sa != defaultSA {
			t.Errorf("%s: crew agent naming %q got %q, want chart default %q", path, infraRoleSA, sa, defaultSA)
		}
	})
}

func TestAgentMetadataServiceAccountIgnoredIsCounted(t *testing.T) {
	c := agentK8sIdentityIgnoredTotal.WithLabelValues("service_account")
	before := testutil.ToFloat64(c)
	build(t, "crew", map[string]string{"service_account": infraRoleSA}, func(*testing.T, string, string, bool) {})
	if got := testutil.ToFloat64(c) - before; got != 2 { // one per builder
		t.Errorf("ignored-service_account counter moved by %v, want 2", got)
	}
	// Positive control: a metadata value equal to the resolved SA is not an override.
	before = testutil.ToFloat64(c)
	build(t, "crew", map[string]string{"service_account": defaultSA}, func(*testing.T, string, string, bool) {})
	if got := testutil.ToFloat64(c) - before; got != 0 {
		t.Errorf("counter moved by %v for a no-op service_account, want 0", got)
	}
}

func TestAgentMetadataCannotEnableK8sAPI(t *testing.T) {
	// Non-privileged agent asking for the token gets none.
	build(t, "crew", map[string]string{"needs_k8s_api": "true"}, func(t *testing.T, path, _ string, api bool) {
		if api {
			t.Errorf("%s: crew agent with needs_k8s_api=true in metadata got an API token", path)
		}
	})
	// Positive control: the same agent under a role that grants it gets the token.
	build(t, "infra", map[string]string{"needs_k8s_api": "true"}, func(t *testing.T, path, _ string, api bool) {
		if !api {
			t.Errorf("%s: infra role needs_k8s_api=true did not yield an API token", path)
		}
	})
	build(t, "infra", map[string]string{}, func(t *testing.T, path, _ string, api bool) {
		if !api {
			t.Errorf("%s: infra role with no metadata lost its API token", path)
		}
	})
}

func TestAgentMetadataMayNarrowK8sAPI(t *testing.T) {
	build(t, "infra", map[string]string{"needs_k8s_api": "false"}, func(t *testing.T, path, sa string, api bool) {
		if api {
			t.Errorf("%s: needs_k8s_api=false did not narrow the role's token", path)
		}
		if sa != infraRoleSA {
			t.Errorf("%s: narrowing the token changed the SA to %q, want %q", path, sa, infraRoleSA)
		}
	})
}

// TestResumedInfraAgentKeepsRoleServiceAccount is the restart pin: the reconciler
// rebuilds the spec from bead metadata on every pod delete, drain and resume, and
// must land on the role-derived SA again, whether or not the metadata still
// carries the value the spawn path once stamped there.
func TestResumedInfraAgentKeepsRoleServiceAccount(t *testing.T) {
	for _, md := range []map[string]string{
		{},
		{"service_account": infraRoleSA},
		{"service_account": "X", "agent_state": "working"},
	} {
		spec := BuildSpecFromBeadInfo(identityCfg(), "gardenio", "infra", "infra", "agent1", md)
		if spec.ServiceAccountName != infraRoleSA || !spec.NeedsK8sAPI {
			t.Errorf("resumed infra agent (metadata %v): SA %q api=%v, want %q api=true", md, spec.ServiceAccountName, spec.NeedsK8sAPI, infraRoleSA)
		}
	}
}

func TestProjectServiceAccountStillApplies(t *testing.T) {
	cfg := identityCfg()
	cfg.ProjectCache["gardenio"] = config.ProjectCacheEntry{ServiceAccount: "project-sa"}
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "agent1", map[string]string{"service_account": "X"})
	if spec.ServiceAccountName != "project-sa" {
		t.Errorf("got %q, want project SA %q", spec.ServiceAccountName, "project-sa")
	}
}

// TestVerifierRoleGetsVerifierServiceAccount: a role with verifier=true runs as
// the chart's read-only verifier SA with its token mounted (P9.2,
// gd-6ujfeTZfNL), on both spec builders; agent metadata cannot turn it on.
func TestVerifierRoleGetsVerifierServiceAccount(t *testing.T) {
	const verifierSA = "gardenio-verifier"
	cfgWith := func(verifierSA string) *config.Config {
		cfg := identityCfg()
		cfg.VerifierServiceAccount = verifierSA
		cfg.RoleCache["crew"] = config.RoleCacheEntry{Verifier: true}
		cfg.RoleCache["infra"] = config.RoleCacheEntry{ServiceAccount: infraRoleSA, NeedsK8sAPI: true, Verifier: true}
		cfg.RoleCache["thread"] = config.RoleCacheEntry{}
		return cfg
	}
	cases := []struct {
		name, role, verifierSA string
		md                     map[string]string
		wantSA                 string
		wantAPI                bool
	}{
		{"verifier role", "crew", verifierSA, nil, verifierSA, true},
		{"verifier role, chart SA not rendered", "crew", "", nil, defaultSA, false},
		{"role's own SA wins", "infra", verifierSA, nil, infraRoleSA, true},
		{"agent metadata cannot ask for it", "thread", verifierSA, map[string]string{"verifier": "true", "needs_k8s_api": "true"}, defaultSA, false},
		{"agent metadata may turn the token off", "crew", verifierSA, map[string]string{"needs_k8s_api": "false"}, verifierSA, false},
	}
	for _, c := range cases {
		md := c.md
		if md == nil {
			md = map[string]string{}
		}
		spec := BuildSpecFromBeadInfo(cfgWith(c.verifierSA), "gardenio", c.role, c.role, "agent1", md)
		ev := buildAgentPodSpec(cfgWith(c.verifierSA), subscriber.Event{Project: "gardenio", Role: c.role, AgentName: "agent1", Metadata: md})
		for path, got := range map[string][2]any{"reconciler": {spec.ServiceAccountName, spec.NeedsK8sAPI}, "events": {ev.ServiceAccountName, ev.NeedsK8sAPI}} {
			if got[0] != c.wantSA || got[1] != c.wantAPI {
				t.Errorf("%s (%s): SA %q api=%v, want %q api=%v", c.name, path, got[0], got[1], c.wantSA, c.wantAPI)
			}
		}
	}
}
