package main

import (
	"reflect"
	"slices"
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// The land fence (Lands Track B §5.2, step 10.2): a tenant land's pods run in
// the land's namespace under the land RuntimeClass (LANDS_RUNTIME_CLASS; none
// by default) as land-agent, with no API token and no thaw-state hostPath,
// whatever a role, a pilot class, gVisor routing or a per-agent pin says. Both
// spec builders apply it.

// landFenceCfg is a controller with a tenant land "acme" in land-acme, an
// operator project "gardenio", and a role that tries every way out.
func landFenceCfg() *config.Config {
	return &config.Config{
		Namespace:         "gardenio",
		GVisorNamespace:   "gardenio-gvisor",
		FreezeThawEnabled: true,
		ProjectCache: map[string]config.ProjectCacheEntry{
			"acme":     {LandKind: "tenant", Namespace: "land-acme", RuntimeClassName: "runsc", ServiceAccount: "gardenio-agents", NeedsK8sAPI: true},
			"gardenio": {},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"escape": {RuntimeClassName: "runc", ServiceAccount: "cluster-admin-sa", NeedsK8sAPI: true, ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM},
			"plain":  {},
		},
	}
}

// pins are per-agent metadata pins that try to move a tenant pod.
func pins() map[string]string {
	return map[string]string{"namespace": "gardenio", "runtime_class": "runc", "execution_target": "kubernetes"}
}

// assertFenced checks a tenant pod under the default land RuntimeClass (none).
func assertFenced(t *testing.T, spec podmanager.AgentPodSpec) {
	t.Helper()
	assertFencedRC(t, spec, "")
}

func assertFencedRC(t *testing.T, spec podmanager.AgentPodSpec, rc string) {
	t.Helper()
	if spec.Namespace != "land-acme" {
		t.Errorf("namespace = %q, want land-acme", spec.Namespace)
	}
	if spec.RuntimeClassName != rc {
		t.Errorf("runtime class = %q, want %q", spec.RuntimeClassName, rc)
	}
	if spec.ServiceAccountName != "land-agent" {
		t.Errorf("service account = %q, want land-agent", spec.ServiceAccountName)
	}
	if spec.NeedsK8sAPI {
		t.Error("tenant pod mounts a Kubernetes API token")
	}
	if !spec.NoThawState {
		t.Error("tenant pod gets the thaw-state hostPath")
	}
	if spec.ExecutionTarget != podmanager.ExecutionTargetKubernetes {
		t.Errorf("execution target = %q, want kubernetes", spec.ExecutionTarget)
	}
	if spec.Env[EnvFreezeEligible] == "true" {
		t.Error("tenant pod is marked freeze-eligible")
	}
	if !spec.LandFenced {
		t.Error("tenant pod is not marked LandFenced (it would keep its role's priority class)")
	}
	if !reflect.DeepEqual(spec.Affinity, podmanager.LandTenantAffinity()) {
		t.Errorf("affinity = %+v, want podmanager.LandTenantAffinity (off fleet nodes)", spec.Affinity)
	}
	if !slices.Contains(spec.DNSSearches, "gardenio.svc.cluster.local") {
		t.Errorf("dns searches = %v, want the release namespace's", spec.DNSSearches)
	}
	if n := len(slices.DeleteFunc(slices.Clone(spec.DNSSearches), func(s string) bool { return s != "gardenio.svc.cluster.local" })); n != 1 {
		t.Errorf("release search domain appears %d times", n)
	}
}

func TestLandFence_EventPath(t *testing.T) {
	for _, role := range []string{"plain", "escape"} {
		t.Run(role, func(t *testing.T) {
			spec := buildAgentPodSpec(landFenceCfg(), subscriber.Event{
				Project: "acme", Role: role, AgentName: "acme-crew-1", Mode: "crew", Metadata: pins(),
			})
			assertFenced(t, spec)
		})
	}
}

func TestLandFence_ReconcilerPath(t *testing.T) {
	for _, role := range []string{"plain", "escape"} {
		t.Run(role, func(t *testing.T) {
			spec := BuildSpecFromBeadInfo(landFenceCfg(), "acme", "crew", role, "acme-crew-1", pins())
			assertFenced(t, spec)
		})
	}
}

// A tenant pod's placement is the land's whatever its role: an interactive
// thread or dm role gets no on-demand term (the k3s platform pair) and a
// gardener none of its preemption (gd-NDJIOutLsv).
func TestLandFence_PlacementWhateverTheRole(t *testing.T) {
	for _, role := range []string{"thread", "dm", "crew,thread", "plan-gardener"} {
		t.Run(role, func(t *testing.T) {
			assertFenced(t, BuildSpecFromBeadInfo(landFenceCfg(), "acme", "crew", role, "acme-crew-1", pins()))
			assertFenced(t, buildAgentPodSpec(landFenceCfg(), subscriber.Event{
				Project: "acme", Role: role, AgentName: "acme-crew-1", Mode: "crew", Metadata: pins(),
			}))
		})
	}
}

// The land RuntimeClass is the controller's setting alone: "" drops the
// project's and a role's runsc (and a pin's runc) so tenant pods run on the
// node's default runtime, and "runsc" puts every tenant pod under gVisor.
func TestLandFence_RuntimeClassSetting(t *testing.T) {
	for _, rc := range []string{"", "runsc"} {
		for _, role := range []string{"plain", "escape"} {
			t.Run(rc+"/"+role, func(t *testing.T) {
				cfg := landFenceCfg()
				cfg.LandsRuntimeClass = rc
				assertFencedRC(t, BuildSpecFromBeadInfo(cfg, "acme", "crew", role, "acme-crew-1", pins()), rc)
				assertFencedRC(t, buildAgentPodSpec(cfg, subscriber.Event{
					Project: "acme", Role: role, AgentName: "acme-crew-1", Mode: "crew", Metadata: pins(),
				}), rc)
			})
		}
	}
}

func TestLandFence_TenantWithNoNamespaceIsPinnedToItsName(t *testing.T) {
	cfg := landFenceCfg()
	e := cfg.ProjectCache["acme"]
	e.Namespace = ""
	cfg.ProjectCache["acme"] = e
	spec := BuildSpecFromBeadInfo(cfg, "acme", "crew", "plain", "acme-crew-1", map[string]string{})
	if spec.Namespace != "land-acme" {
		t.Errorf("namespace = %q, want land-acme (a missing namespace must fail the create, not fall back)", spec.Namespace)
	}
}

// The project bead's namespace field never places a pod: a tenant land whose
// bead names the release namespace (or any other) still lands in land-<land>.
func TestLandFence_BeadNamespaceIsNotTrusted(t *testing.T) {
	for _, bad := range []string{"gardenio", "kube-system", "land-other"} {
		t.Run(bad, func(t *testing.T) {
			cfg := landFenceCfg()
			e := cfg.ProjectCache["acme"]
			e.Namespace = bad
			cfg.ProjectCache["acme"] = e
			spec := BuildSpecFromBeadInfo(cfg, "acme", "crew", "plain", "acme-crew-1", map[string]string{})
			assertFenced(t, spec)
			spec = buildAgentPodSpec(cfg, subscriber.Event{
				Project: "acme", Role: "plain", AgentName: "acme-crew-1", Mode: "crew", Metadata: map[string]string{"namespace": bad},
			})
			assertFenced(t, spec)
		})
	}
}

func TestLandFence_OperatorLandsUntouched(t *testing.T) {
	cfg := landFenceCfg()
	spec := buildAgentPodSpec(cfg, subscriber.Event{
		Project: "gardenio", Role: "escape", AgentName: "gd-crew-1", Mode: "crew", Metadata: map[string]string{},
	})
	if spec.Namespace != "gardenio" || spec.RuntimeClassName != "runc" || spec.ServiceAccountName != "cluster-admin-sa" || spec.NoThawState {
		t.Errorf("operator pod changed by the land fence: ns=%q rc=%q sa=%q nothaw=%v",
			spec.Namespace, spec.RuntimeClassName, spec.ServiceAccountName, spec.NoThawState)
	}
	if spec.LandFenced || reflect.DeepEqual(spec.Affinity, podmanager.LandTenantAffinity()) {
		t.Error("operator pod got the tenant placement")
	}
}
