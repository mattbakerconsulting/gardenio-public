package tests

import (
	"strings"
	"testing"
)

// bridge-log-shipper — the non-AVA webhook-audit transport (GAP-B of the webhook
// carve-out, task ka-0000000320, epic ka-0000000914). A purpose-built Go streamer
// (gardenio/controller/cmd/bridge-log-shipper) that opens FOLLOW streams of the
// jira-bridge + gitlab-bridge pods' stdout via the Kubernetes pod-log API, keeps
// only the webhook receive-path slog records (by `msg` prefix), stamps each with
// its logical door name, and POSTs them to the in-chart VictoriaLogs
// /insert/jsonline — the durable audit-log pipeline the non-AVA webhook carve-out
// needs (control 1 "good LOGS of all non-AVA bridge traffic"). It reuses the
// sanctioned stream->filter->sink shape of traefik-log-shipper.
//
// These tests pin:
//   - the default-off invariant (chart byte-identical until an env opts in),
//   - the opt-in render of every component object,
//   - the source-guard (fails fast when NEITHER bridge is enabled),
//   - the per-door env config contract (selectors, msg prefixes, VL endpoint),
//   - the conditional per-door env (only the enabled bridge's vars render),
//   - the monorepo image ref (built by images.yml, at chart calver),
//   - the same-namespace RBAC (Role + RoleBinding + SA in the release namespace),
//   - the NetworkPolicy both-sides reachability (shipper egress to VL+apiserver,
//     vlsingle ingress from the shipper).

const blsVLEndpoint = "http://gardenio-victoria-logs:9428"

// blsEnabledFlags returns the minimal --set flags that render the shipper: the
// component on plus at least one bridge source (both, to exercise the full
// two-door render). Uses release name "gardenio" via helmTemplateGardenio where the
// fullname-derived names matter.
func blsEnabledFlags(extra ...string) []string {
	base := []string{
		"bridgeLogShipper.enabled=true",
		"jiraBridge.enabled=true",
		"gitlabBridge.enabled=true",
	}
	return append(base, extra...)
}

// --- default-off invariant --------------------------------------------------

func TestBridgeLogShipper_DefaultOff_DeploymentAbsent(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/bridge-log-shipper/deployment.yaml")
	// --show-only on a template that renders nothing errors with "could not
	// find template ... in chart" — that IS the default-absent proof.
	if err == nil && strings.Contains(out, "kind: Deployment") {
		t.Errorf("bridge-log-shipper Deployment must NOT render when bridgeLogShipper.enabled is false; got:\n%s", out)
	}
}

func TestBridgeLogShipper_DefaultOff_AbsentFromFullRender(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "")
	if strings.Contains(out, "component: bridge-log-shipper") {
		t.Error("no bridge-log-shipper resources should render when bridgeLogShipper.enabled is false")
	}
}

func TestBridgeLogShipper_DefaultOff_NoEgressPolicy(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
	)
	if strings.Contains(out, "bridge-log-shipper-egress") {
		t.Errorf("bridge-log-shipper egress NetworkPolicy must NOT render when the component is disabled; got:\n%s", out)
	}
}

// --- source guard (fail-fast when neither bridge is enabled) -----------------

func TestBridgeLogShipper_Guard_NoBridgeEnabled(t *testing.T) {
	t.Parallel()
	_, err := helmTemplateAllowFail(t, "templates/bridge-log-shipper/deployment.yaml",
		"bridgeLogShipper.enabled=true",
		"jiraBridge.enabled=false",
		"gitlabBridge.enabled=false",
	)
	if err == nil {
		t.Fatal("expected render to FAIL when bridgeLogShipper.enabled=true but NEITHER jiraBridge nor gitlabBridge is enabled (the shipper would have no source pod to follow)")
	}
}

func TestBridgeLogShipper_Guard_OneBridgeIsEnough(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/bridge-log-shipper/deployment.yaml",
		"bridgeLogShipper.enabled=true",
		"jiraBridge.enabled=true",
		"gitlabBridge.enabled=false",
	)
	if err != nil {
		t.Fatalf("render must succeed with just one bridge enabled; got err %v\n%s", err, out)
	}
	if !strings.Contains(out, "BRIDGE_JIRA_POD_SELECTOR") {
		t.Errorf("jira door env must render when only jiraBridge is enabled; got:\n%s", out)
	}
	if strings.Contains(out, "BRIDGE_GITLAB_POD_SELECTOR") {
		t.Errorf("gitlab door env must NOT render when gitlabBridge is disabled; got:\n%s", out)
	}
}

// --- Deployment shape --------------------------------------------------------

func TestBridgeLogShipper_Deployment_CoreShape(t *testing.T) {
	t.Parallel()
	// Release name "gardenio" so the fullname-derived SA name resolves to
	// gardenio-bridge-log-shipper (matches the prod release).
	out, err := helmTemplateGardenio(t, "templates/bridge-log-shipper/deployment.yaml", blsEnabledFlags()...)
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	checks := map[string]string{
		"serviceAccountName: gardenio-bridge-log-shipper": "SA wiring (pods/log read)",
		"automountServiceAccountToken: true":              "SA token must be mounted to reach the apiserver",
		"readOnlyRootFilesystem: true":                    "hardened rootfs",
		"runAsNonRoot: true":                              "non-root",
		"replicas: 1":                                     "single replica (no double-read of a stream)",
		"type: Recreate":                                  "Recreate strategy (no overlapping readers)",
	}
	for needle, why := range checks {
		if !strings.Contains(out, needle) {
			t.Errorf("Deployment missing %q (%s); got:\n%s", needle, why, out)
		}
	}
}

func TestBridgeLogShipper_Deployment_UsesMonorepoImageAtChartVersion(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/bridge-log-shipper/deployment.yaml", blsEnabledFlags()...)
	if !strings.Contains(out, "/gardenio/bridge-log-shipper:") {
		t.Errorf("image must be the monorepo gardenio/bridge-log-shipper ref; got:\n%s", out)
	}
	if strings.Contains(out, "/mirror/") {
		t.Errorf("shipper image must NOT come from the upstream mirror namespace; got:\n%s", out)
	}
}

// --- Deployment env-var config contract -------------------------------------

func TestBridgeLogShipper_Env_SourcesAndSink(t *testing.T) {
	t.Parallel()
	// Release "gardenio" so the auto-wired vlsingle endpoint resolves to
	// gardenio-victoria-logs (the prod release name).
	out, err := helmTemplateGardenio(t, "templates/bridge-log-shipper/deployment.yaml",
		blsEnabledFlags("vlsingle.enabled=true")...)
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	for _, needle := range []string{
		"name: BRIDGE_VICTORIALOGS_ENDPOINT",
		blsVLEndpoint, // base URL auto-wired to in-chart vlsingle
		"name: BRIDGE_POD_NAMESPACE",
		"name: BRIDGE_JIRA_POD_SELECTOR",
		"app.kubernetes.io/component=jira-bridge",
		"name: BRIDGE_JIRA_MSG_PREFIX",
		"jira-webhook:",
		"name: BRIDGE_GITLAB_POD_SELECTOR",
		"app.kubernetes.io/component=gitlab-bridge",
		"name: BRIDGE_GITLAB_MSG_PREFIX",
		"name: BRIDGE_RECONNECT_BACKOFF",
		"name: BRIDGE_STREAM_SINCE_SECONDS",
		"name: BRIDGE_FLUSH_INTERVAL",
		"name: BRIDGE_MAX_BATCH_LINES",
	} {
		if !strings.Contains(out, needle) {
			t.Errorf("Deployment env missing %q; got:\n%s", needle, out)
		}
	}
}

func TestBridgeLogShipper_Env_ExplicitEndpointOverride(t *testing.T) {
	t.Parallel()
	const custom = "http://out-of-chart-vl:9428"
	out := helmTemplate(t, "templates/bridge-log-shipper/deployment.yaml",
		blsEnabledFlags("bridgeLogShipper.victoriaLogs.endpoint="+custom)...)
	if !strings.Contains(out, custom) {
		t.Errorf("explicit victoriaLogs.endpoint must win over the vlsingle auto-wire; got:\n%s", out)
	}
}

// --- RBAC (same-namespace pod-log read) -------------------------------------

func TestBridgeLogShipper_RBAC_SameNamespaceLeastPrivilege(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio-system", "templates/bridge-log-shipper/rbac.yaml", blsEnabledFlags()...)
	for _, needle := range []string{
		"kind: ServiceAccount",
		"kind: Role",
		"kind: RoleBinding",
		"namespace: gardenio-system",
		`resources: ["pods"]`,
		`resources: ["pods/log"]`,
	} {
		if !strings.Contains(out, needle) {
			t.Errorf("RBAC missing %q; got:\n%s", needle, out)
		}
	}
	// Least-privilege: no write verbs, no cluster scope.
	for _, forbidden := range []string{"ClusterRole", `"create"`, `"update"`, `"delete"`, `"patch"`} {
		if strings.Contains(out, forbidden) {
			t.Errorf("RBAC must be read-only + namespaced; found forbidden %q in:\n%s", forbidden, out)
		}
	}
}

// --- NetworkPolicy both-sides reachability ----------------------------------

func TestBridgeLogShipper_NetPol_EgressToVLAndApiserver(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio-system", "templates/networkpolicies/default-deny-egress.yaml",
		"bridgeLogShipper.enabled=true",
		"jiraBridge.enabled=true",
		"gitlabBridge.enabled=true",
		"vlsingle.enabled=true",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
	)
	if !strings.Contains(out, "bridge-log-shipper-egress") {
		t.Fatalf("bridge-log-shipper egress NetworkPolicy must render when enabled; got:\n%s", out)
	}
	for _, needle := range []string{
		"component: bridge-log-shipper", // policy selects the shipper
		"port: 9428",                    // vlsingle sink
		"port: 53",                      // kube-dns
		"port: 443",                     // apiserver (public EKS endpoint)
	} {
		if !strings.Contains(out, needle) {
			t.Errorf("egress policy missing %q; got:\n%s", needle, out)
		}
	}
}

func TestBridgeLogShipper_NetPol_VLSingleIngressAdmitsShipper(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"bridgeLogShipper.enabled=true",
		"jiraBridge.enabled=true",
		"gitlabBridge.enabled=true",
		"vlsingle.enabled=true",
		"networkPolicies.enabled=true",
	)
	// The vlsingle-ingress policy must list the shipper as an allowed peer, or
	// the POSTs to /insert/jsonline are dropped by default-deny.
	if !strings.Contains(out, "vlsingle-ingress") {
		t.Fatalf("vlsingle-ingress policy must render; got:\n%s", out)
	}
	if !strings.Contains(out, "component: bridge-log-shipper") {
		t.Errorf("vlsingle-ingress must admit the bridge-log-shipper peer; got:\n%s", out)
	}
}
