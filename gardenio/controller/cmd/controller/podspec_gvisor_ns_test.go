package main

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// gVisor namespace routing (bug ka-0000004301, epic ka-0000008224): a runsc
// agent pod carries a thaw-state hostPath that PodSecurity baseline forbids in
// the main namespace, so when cfg.GVisorNamespace is set the controller routes
// runtime_class=runsc agents into that dedicated PSA-privileged namespace.

// runscRoleCfg builds a config whose `gvisor-poc` role pins runtime_class=runsc,
// mirroring the real canary role bead (ka-0000003266).
func runscRoleCfg(gvisorNS string) *config.Config {
	return &config.Config{
		Namespace:       "gardenio",
		GVisorNamespace: gvisorNS,
		ProjectCache:    map[string]config.ProjectCacheEntry{},
		RoleCache: map[string]config.RoleCacheEntry{
			"gvisor-poc": {RuntimeClassName: podmanager.RuntimeClassRunsc},
		},
	}
}

func TestBuildAgentPodSpec_GVisorNamespace_RoutesRunscAgent(t *testing.T) {
	cfg := runscRoleCfg("gardenio-gvisor")
	event := subscriber.Event{
		Project:   "gardenio",
		Role:      "gvisor-poc",
		AgentName: "canary",
		Metadata:  map[string]string{},
	}
	spec := buildAgentPodSpec(cfg, event)

	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		t.Fatalf("expected RuntimeClassName=runsc, got %q", spec.RuntimeClassName)
	}
	if spec.Namespace != "gardenio-gvisor" {
		t.Errorf("runsc agent should route to gVisor namespace, got %q", spec.Namespace)
	}
}

// Cross-namespace DNS (bug ka-0000001058): when a runsc agent is routed into
// the gVisor namespace, it must carry the release-namespace search domain so
// the controller's bare service names (gardenio-coopmux, gardenio-nats) resolve;
// without it the coopmux PTY never registers and NATS never connects (the
// canary boot-wedge).
func TestBuildAgentPodSpec_GVisorNamespace_AppendsReleaseSearchDomain(t *testing.T) {
	cfg := runscRoleCfg("gardenio-gvisor")
	event := subscriber.Event{
		Project:   "gardenio",
		Role:      "gvisor-poc",
		AgentName: "canary",
		Metadata:  map[string]string{},
	}
	spec := buildAgentPodSpec(cfg, event)

	want := "gardenio.svc.cluster.local"
	var found bool
	for _, s := range spec.DNSSearches {
		if s == want {
			found = true
		}
	}
	if !found {
		t.Errorf("runsc agent in gVisor ns must carry DNS search %q, got %+v", want, spec.DNSSearches)
	}
}

// A non-runsc agent stays in the main namespace and needs no extra search
// domain.
func TestBuildAgentPodSpec_GVisorNamespace_NonRunscNoSearchDomain(t *testing.T) {
	cfg := &config.Config{
		Namespace:       "gardenio",
		GVisorNamespace: "gardenio-gvisor",
		ProjectCache:    map[string]config.ProjectCacheEntry{},
	}
	event := subscriber.Event{
		Project:   "gardenio",
		Role:      "crew",
		AgentName: "normal",
		Metadata:  map[string]string{},
	}
	spec := buildAgentPodSpec(cfg, event)
	if len(spec.DNSSearches) != 0 {
		t.Errorf("non-runsc agent must not get extra DNS searches, got %+v", spec.DNSSearches)
	}
}

// With GVisorNamespace unset (single-namespace deployment) the runsc agent
// stays in cfg.Namespace, so no cross-ns search domain is needed.
func TestBuildAgentPodSpec_GVisorNamespace_UnsetNoSearchDomain(t *testing.T) {
	cfg := runscRoleCfg("")
	event := subscriber.Event{
		Project:   "gardenio",
		Role:      "gvisor-poc",
		AgentName: "canary",
		Metadata:  map[string]string{},
	}
	spec := buildAgentPodSpec(cfg, event)
	if len(spec.DNSSearches) != 0 {
		t.Errorf("single-namespace runsc agent must not get extra DNS searches, got %+v", spec.DNSSearches)
	}
}

func TestBuildAgentPodSpec_GVisorNamespace_NonRunscStaysPut(t *testing.T) {
	cfg := &config.Config{
		Namespace:       "gardenio",
		GVisorNamespace: "gardenio-gvisor",
		ProjectCache:    map[string]config.ProjectCacheEntry{},
	}
	event := subscriber.Event{
		Project:   "gardenio",
		Role:      "crew",
		AgentName: "normal",
		Metadata:  map[string]string{},
	}
	spec := buildAgentPodSpec(cfg, event)

	if spec.RuntimeClassName != "" {
		t.Fatalf("expected no RuntimeClassName, got %q", spec.RuntimeClassName)
	}
	if spec.Namespace != "gardenio" {
		t.Errorf("non-runsc agent must stay in the main namespace, got %q", spec.Namespace)
	}
}

func TestBuildAgentPodSpec_GVisorNamespace_UnsetKeepsRunscInMainNS(t *testing.T) {
	// GVisorNamespace deliberately empty — single-namespace deployment.
	cfg := runscRoleCfg("")
	event := subscriber.Event{
		Project:   "gardenio",
		Role:      "gvisor-poc",
		AgentName: "canary",
		Metadata:  map[string]string{},
	}
	spec := buildAgentPodSpec(cfg, event)

	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		t.Fatalf("expected RuntimeClassName=runsc, got %q", spec.RuntimeClassName)
	}
	if spec.Namespace != "gardenio" {
		t.Errorf("with GVisorNamespace unset, runsc agent stays in cfg.Namespace, got %q", spec.Namespace)
	}
}

func TestBuildAgentPodSpec_GVisorNamespace_ExplicitNamespaceWins(t *testing.T) {
	cfg := runscRoleCfg("gardenio-gvisor")
	event := subscriber.Event{
		Project:   "gardenio",
		Role:      "gvisor-poc",
		AgentName: "canary",
		Metadata: map[string]string{
			"namespace": "explicit-ns",
		},
	}
	spec := buildAgentPodSpec(cfg, event)

	if spec.Namespace != "explicit-ns" {
		t.Errorf("an explicit event namespace must win over gVisor routing, got %q", spec.Namespace)
	}
}

// The teardown paths (AgentStop/Done/Kill/Stuck in handleEvent) resolve the
// pod's namespace via buildAgentPodSpec(...).Namespace rather than
// namespaceFromEvent, so freeze-on-stop and pod deletion target the gVisor
// namespace a runsc agent actually lives in (bug ka-0000004301). This asserts
// the resolution the teardown paths depend on for a stop-shaped event.
func TestBuildAgentPodSpec_GVisorNamespace_TeardownResolvesRunscNamespace(t *testing.T) {
	cfg := runscRoleCfg("gardenio-gvisor")
	event := subscriber.Event{
		Type:      subscriber.AgentStop,
		Project:   "gardenio",
		Role:      "gvisor-poc",
		AgentName: "gardenio-canary",
		Metadata:  map[string]string{},
	}
	if ns := buildAgentPodSpec(cfg, event).Namespace; ns != "gardenio-gvisor" {
		t.Errorf("teardown of a runsc agent must resolve the gVisor namespace, got %q", ns)
	}
}

// The RECONCILER creates agent pods via BuildSpecFromBeadInfo, not
// buildAgentPodSpec. That path originally lacked the gVisor-namespace routing,
// so a runsc agent was created in the PSA=baseline `gardenio` namespace and
// rejected every reconcile ("violates PodSecurity baseline:latest: hostPath
// volumes (volume thaw-state)"). These tests pin the reconciler path so the two
// spec builders can never drift again (regression ka-0000003657).

func TestBuildSpecFromBeadInfo_GVisorNamespace_RoutesRunscAgent(t *testing.T) {
	cfg := runscRoleCfg("gardenio-gvisor")
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "gvisor-poc", "canary", map[string]string{})

	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		t.Fatalf("expected RuntimeClassName=runsc, got %q", spec.RuntimeClassName)
	}
	if spec.Namespace != "gardenio-gvisor" {
		t.Errorf("reconciler path must route a runsc agent to the gVisor namespace, got %q", spec.Namespace)
	}
}

func TestBuildSpecFromBeadInfo_GVisorNamespace_NonRunscStaysPut(t *testing.T) {
	cfg := &config.Config{
		Namespace:       "gardenio",
		GVisorNamespace: "gardenio-gvisor",
		ProjectCache:    map[string]config.ProjectCacheEntry{},
		RoleCache:       map[string]config.RoleCacheEntry{},
	}
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "normal", map[string]string{})

	if spec.RuntimeClassName != "" {
		t.Fatalf("expected no RuntimeClassName, got %q", spec.RuntimeClassName)
	}
	if spec.Namespace != "gardenio" {
		t.Errorf("non-runsc agent must stay in the main namespace, got %q", spec.Namespace)
	}
}

func TestBuildSpecFromBeadInfo_GVisorNamespace_UnsetKeepsRunscInMainNS(t *testing.T) {
	cfg := runscRoleCfg("")
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "gvisor-poc", "canary", map[string]string{})

	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		t.Fatalf("expected RuntimeClassName=runsc, got %q", spec.RuntimeClassName)
	}
	if spec.Namespace != "gardenio" {
		t.Errorf("with GVisorNamespace unset, runsc agent stays in cfg.Namespace, got %q", spec.Namespace)
	}
}

func TestBuildSpecFromBeadInfo_GVisorNamespace_ExplicitNamespaceWins(t *testing.T) {
	cfg := runscRoleCfg("gardenio-gvisor")
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "gvisor-poc", "canary",
		map[string]string{"namespace": "explicit-ns"})

	if spec.Namespace != "explicit-ns" {
		t.Errorf("an explicit metadata namespace must win over gVisor routing, got %q", spec.Namespace)
	}
}

// Project-level runtime_class=runsc (no role pin, no agent metadata) pins every
// agent in the project to runsc. This is the wiring that makes "thread agents
// resuming in the gvisor project auto-thaw" work without tagging each spawn:
// the project bead carries runtime_class=runsc, every pod lands on runsc, and a
// resume with a persisted snapshot takes the thaw path (epic ka-0000007417,
// task ka-0000006969).
func TestBuildAgentPodSpec_ProjectRuntimeClass_RoutesRunscAgent(t *testing.T) {
	cfg := &config.Config{
		Namespace:       "gardenio",
		GVisorNamespace: "gardenio-gvisor",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gvisor": {RuntimeClassName: podmanager.RuntimeClassRunsc},
		},
		RoleCache: map[string]config.RoleCacheEntry{},
	}
	event := subscriber.Event{
		Project:   "gvisor",
		Role:      "thread",
		AgentName: "thread-agent",
		Metadata:  map[string]string{},
	}
	spec := buildAgentPodSpec(cfg, event)

	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		t.Fatalf("project-level runtime_class=runsc must pin the pod, got %q", spec.RuntimeClassName)
	}
	if spec.Namespace != "gardenio-gvisor" {
		t.Errorf("a project-runsc agent must route to the gVisor namespace, got %q", spec.Namespace)
	}
}

// Precedence: a role pin overrides the project default (global < project <
// role < agent). applyRoleDefaults runs after applyProjectDefaults, so a role
// that pins a different runtime class wins over the project's.
func TestBuildAgentPodSpec_ProjectRuntimeClass_RoleOverridesProject(t *testing.T) {
	cfg := &config.Config{
		Namespace:       "gardenio",
		GVisorNamespace: "gardenio-gvisor",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gvisor": {RuntimeClassName: podmanager.RuntimeClassRunsc},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"special": {RuntimeClassName: "kata"},
		},
	}
	event := subscriber.Event{
		Project:   "gvisor",
		Role:      "special",
		AgentName: "thread-agent",
		Metadata:  map[string]string{},
	}
	spec := buildAgentPodSpec(cfg, event)

	if spec.RuntimeClassName != "kata" {
		t.Fatalf("role-level runtime_class must override the project default, got %q", spec.RuntimeClassName)
	}
}

// Precedence on the reconciler path: agent metadata is the highest layer, so a
// per-agent runtime_class overrides the project default.
func TestBuildSpecFromBeadInfo_ProjectRuntimeClass_AgentMetadataWins(t *testing.T) {
	cfg := &config.Config{
		Namespace:       "gardenio",
		GVisorNamespace: "gardenio-gvisor",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gvisor": {RuntimeClassName: podmanager.RuntimeClassRunsc},
		},
		RoleCache: map[string]config.RoleCacheEntry{},
	}
	spec := BuildSpecFromBeadInfo(cfg, "gvisor", "thread", "thread", "thread-agent",
		map[string]string{"runtime_class": "kata"})

	if spec.RuntimeClassName != "kata" {
		t.Fatalf("agent-level runtime_class must override the project default, got %q", spec.RuntimeClassName)
	}
}

func TestBuildSpecFromBeadInfo_ProjectRuntimeClass_RoutesRunscAgent(t *testing.T) {
	cfg := &config.Config{
		Namespace:       "gardenio",
		GVisorNamespace: "gardenio-gvisor",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gvisor": {RuntimeClassName: podmanager.RuntimeClassRunsc},
		},
		RoleCache: map[string]config.RoleCacheEntry{},
	}
	spec := BuildSpecFromBeadInfo(cfg, "gvisor", "thread", "thread", "thread-agent", map[string]string{})

	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		t.Fatalf("reconciler path: project runtime_class=runsc must pin the pod, got %q", spec.RuntimeClassName)
	}
	if spec.Namespace != "gardenio-gvisor" {
		t.Errorf("reconciler path: a project-runsc agent must route to the gVisor namespace, got %q", spec.Namespace)
	}
}

// Agent-level runtime_class=runsc (no role pin) must also route on the
// reconciler path — the canary bead can carry runtime_class in its own
// metadata.
func TestBuildSpecFromBeadInfo_GVisorNamespace_AgentMetadataRuntimeClass(t *testing.T) {
	cfg := &config.Config{
		Namespace:       "gardenio",
		GVisorNamespace: "gardenio-gvisor",
		ProjectCache:    map[string]config.ProjectCacheEntry{},
		RoleCache:       map[string]config.RoleCacheEntry{},
	}
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "canary",
		map[string]string{"runtime_class": podmanager.RuntimeClassRunsc})

	if spec.Namespace != "gardenio-gvisor" {
		t.Errorf("agent-level runtime_class=runsc must route to the gVisor namespace, got %q", spec.Namespace)
	}
}
