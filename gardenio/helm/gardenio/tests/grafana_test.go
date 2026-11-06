package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// chartDir returns the absolute path to the helm/gardenio chart directory.
func chartDir(t *testing.T) string {
	t.Helper()
	// Tests run from the package directory; chart is two levels up.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..")
}

// helmTemplate renders the chart with the given --set overrides and returns
// the raw YAML output filtered to the specified template file.
func helmTemplate(t *testing.T, showOnly string, setFlags ...string) string {
	t.Helper()
	return helmTemplateNS(t, "", showOnly, setFlags...)
}

// helmTemplateNS renders like helmTemplate but pins the release namespace via
// --namespace. Without a pin, helm defaults .Release.Namespace from the
// ambient environment (kube context on a laptop, the in-cluster SA namespace
// inside an agent pod), so any test asserting on the release namespace MUST
// pin it or it only passes where the ambient namespace happens to match
// (ka-0000003495).
func helmTemplateNS(t *testing.T, namespace, showOnly string, setFlags ...string) string {
	t.Helper()
	args := []string{"template", "test", chartDir(t)}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	if showOnly != "" {
		args = append(args, "--show-only", showOnly)
	}
	for _, s := range setFlags {
		args = append(args, "--set", s)
	}
	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

// helmTemplateGardenio renders with release name "gardenio" (not "test"), so the
// fullname-derived SA/label/endpoint names match the prod release.
func helmTemplateGardenio(t *testing.T, showOnly string, setFlags ...string) (string, error) {
	t.Helper()
	args := []string{"template", "gardenio", chartDir(t)}
	if showOnly != "" {
		args = append(args, "--show-only", showOnly)
	}
	for _, s := range setFlags {
		args = append(args, "--set", s)
	}
	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// --- GF_LOG_FILTERS ---

func TestGrafana_LogFilters_DefaultNotPresent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
	)
	if strings.Contains(out, "GF_LOG_FILTERS") {
		t.Error("GF_LOG_FILTERS should NOT be present when logFilters is empty")
	}
}

func TestGrafana_LogFilters_SetApplied(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.logFilters=provisioning.dashboard:warn",
	)
	if !strings.Contains(out, "GF_LOG_FILTERS") {
		t.Fatal("GF_LOG_FILTERS should be present when logFilters is set")
	}
	if !strings.Contains(out, "provisioning.dashboard:warn") {
		t.Error("GF_LOG_FILTERS value should be 'provisioning.dashboard:warn'")
	}
}

// --- iframe embedding: GF_SECURITY_ALLOW_EMBEDDING (ka-0000003842) ---

// Grafana sends X-Frame-Options: deny by default, refusing even the
// same-origin beads-viewer dashboard-bead iframe ("refused to connect",
// ka-0000001147). allowEmbedding defaults true so the env var renders.
func TestGrafana_AllowEmbedding_DefaultPresent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
	)
	if !strings.Contains(out, "GF_SECURITY_ALLOW_EMBEDDING") {
		t.Fatal("GF_SECURITY_ALLOW_EMBEDDING should be present by default (allowEmbedding defaults true)")
	}
	if !strings.Contains(out, `name: GF_SECURITY_ALLOW_EMBEDDING
              value: "true"`) {
		t.Error("GF_SECURITY_ALLOW_EMBEDDING should render value \"true\"")
	}
}

// Operators can opt out by setting grafana.allowEmbedding=false, which keeps
// Grafana's stock X-Frame-Options: deny behaviour.
func TestGrafana_AllowEmbedding_OmittedWhenDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.allowEmbedding=false",
	)
	if strings.Contains(out, "GF_SECURITY_ALLOW_EMBEDDING") {
		t.Error("GF_SECURITY_ALLOW_EMBEDDING should NOT be present when allowEmbedding=false")
	}
}

// Enabling GF_SECURITY_ALLOW_EMBEDDING in Grafana is not enough: the Traefik
// edge applies a security-headers middleware whose strict variant sets
// frameDeny:true (X-Frame-Options: DENY). The per-app grafana IngressRoute must
// therefore attach the iframe-variant middleware when allowEmbedding is on, or
// the edge re-adds the DENY header and the iframe refuses to connect
// (ka-0000003638).
func TestGrafana_IngressRoute_IframeSecurityHeaders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/ingressroute.yaml",
		"grafana.enabled=true",
		"grafana.ingress.enabled=true",
		"grafana.ingress.host=grafana.example.com",
	)
	if !strings.Contains(out, "security-headers-iframe") {
		t.Error("grafana IngressRoute should attach the iframe-variant security-headers middleware when allowEmbedding is on (default)")
	}
}

// With allowEmbedding=false the per-app route uses the strict frameDeny
// security-headers middleware.
func TestGrafana_IngressRoute_StrictHeadersWhenEmbeddingOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/ingressroute.yaml",
		"grafana.enabled=true",
		"grafana.ingress.enabled=true",
		"grafana.ingress.host=grafana.example.com",
		"grafana.allowEmbedding=false",
	)
	if strings.Contains(out, "security-headers-iframe") {
		t.Error("grafana IngressRoute should NOT attach the iframe middleware when allowEmbedding=false")
	}
	if !strings.Contains(out, "security-headers") {
		t.Error("grafana IngressRoute should attach the strict security-headers middleware when allowEmbedding=false")
	}
}

// SameSite is intentionally not relaxed for the same-origin auth-proxy embed —
// the cookie var must not appear just because embedding is enabled.
func TestGrafana_AllowEmbedding_DoesNotRelaxSameSite(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
	)
	if strings.Contains(out, "GF_SECURITY_COOKIE_SAMESITE") {
		t.Error("GF_SECURITY_COOKIE_SAMESITE should NOT be set: the embed is same-origin and auth rides the auth-proxy header")
	}
}

// --- Sidecar container ---

func TestGrafana_Sidecar_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
	)
	if strings.Contains(out, "grafana-sc-dashboard") {
		t.Error("sidecar container should NOT be present when sidecar.enabled=false")
	}
	if strings.Contains(out, "sidecar-dashboards") {
		t.Error("sidecar-dashboards volume should NOT be present when sidecar.enabled=false")
	}
}

func TestGrafana_Sidecar_EnabledPresent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.sidecar.enabled=true",
	)
	if !strings.Contains(out, "grafana-sc-dashboard") {
		t.Error("sidecar container should be present when sidecar.enabled=true")
	}
	if !strings.Contains(out, "sidecar-dashboards") {
		t.Error("sidecar-dashboards volume should be present when sidecar.enabled=true")
	}
}

func TestGrafana_Sidecar_WatchMethodDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.sidecar.enabled=true",
	)
	// Default watchMethod should be WATCH (efficient K8s watch API).
	if !strings.Contains(out, `value: "WATCH"`) {
		t.Error("sidecar METHOD should default to WATCH")
	}
}

func TestGrafana_Sidecar_LabelValue(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.sidecar.enabled=true",
		"grafana.sidecar.labelValue=my-dashboards",
	)
	if !strings.Contains(out, "LABEL_VALUE") {
		t.Error("LABEL_VALUE env var should be present when labelValue is set")
	}
	if !strings.Contains(out, "my-dashboards") {
		t.Error("LABEL_VALUE should contain the custom value")
	}
}

func TestGrafana_Sidecar_LabelValueOmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.sidecar.enabled=true",
	)
	if strings.Contains(out, "LABEL_VALUE") {
		t.Error("LABEL_VALUE env var should NOT be present when labelValue is empty")
	}
}

// --- Sidecar RBAC ---

func TestGrafana_RBAC_NotPresentWhenSidecarDisabled(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("helm", "template", "test", chartDir(t),
		"--set", "grafana.enabled=true",
		"--show-only", "templates/grafana/rbac.yaml",
	)
	out, _ := cmd.CombinedOutput()
	// When sidecar is disabled, rbac.yaml should produce empty output.
	trimmed := strings.TrimSpace(string(out))
	if trimmed != "" && !strings.Contains(trimmed, "could not find template") {
		t.Errorf("RBAC resources should not be rendered when sidecar is disabled, got:\n%s", trimmed)
	}
}

func TestGrafana_RBAC_PresentWhenSidecarEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/rbac.yaml",
		"grafana.enabled=true",
		"grafana.sidecar.enabled=true",
	)
	if !strings.Contains(out, "ServiceAccount") {
		t.Error("ServiceAccount should be present when sidecar is enabled")
	}
	if !strings.Contains(out, "Role") {
		t.Error("Role should be present when sidecar is enabled")
	}
	if !strings.Contains(out, "RoleBinding") {
		t.Error("RoleBinding should be present when sidecar is enabled")
	}
	if !strings.Contains(out, "configmaps") {
		t.Error("Role should grant access to configmaps")
	}
}

// --- ConfigMap provisioning ---

func TestGrafana_ConfigMap_DashboardProviderWhenSidecarEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
		"grafana.sidecar.enabled=true",
	)
	if !strings.Contains(out, "dashboards.yaml") {
		t.Error("dashboards.yaml provider should be present when sidecar is enabled")
	}
	if !strings.Contains(out, "updateIntervalSeconds") {
		t.Error("dashboard provider should include updateIntervalSeconds")
	}
}

func TestGrafana_ConfigMap_NoDashboardProviderWhenSidecarDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
	)
	if strings.Contains(out, "dashboards.yaml") {
		t.Error("dashboards.yaml provider should NOT be present when sidecar is disabled")
	}
}

// --- VictoriaTraces Jaeger datasource (ka-0000005235) ---

func TestGrafana_ConfigMap_TracesDatasourceWhenVTSingleEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
		"vtsingle.enabled=true",
	)
	if !strings.Contains(out, "name: VictoriaTraces") {
		t.Error("VictoriaTraces datasource should be present when vtsingle is enabled")
	}
	if !strings.Contains(out, "type: jaeger") {
		t.Error("VictoriaTraces datasource should use the core jaeger type")
	}
	if !strings.Contains(out, ":10428/select/jaeger") {
		t.Error("VictoriaTraces datasource should point at the vtsingle Jaeger select API on :10428")
	}
	if !strings.Contains(out, "uid: victoriametrics") {
		t.Error("VictoriaMetrics datasource should carry a stable uid for tracesToMetrics correlation")
	}
}

func TestGrafana_ConfigMap_NoTracesDatasourceWhenVTSingleDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
	)
	if strings.Contains(out, "name: VictoriaTraces") {
		t.Error("VictoriaTraces datasource should NOT be present when vtsingle is disabled")
	}
}

// --- Alertmanager datasource (ka-0000004459; epic ka-0000000770) ---
//
// Wires the in-chart prom/alertmanager (ka-0000003392) into Grafana as an
// alertmanager-type datasource so operators can view + silence alerts from the
// Grafana Alerting UI. Contract enforced here:
//   - present + correctly typed when alertmanager is enabled,
//   - implementation:prometheus (vanilla AM dialect, NOT the mimir default),
//   - handleGrafanaManagedAlerts:false (read/silence view of external alerts,
//     does not route Grafana-managed alerts outbound),
//   - absent when alertmanager is disabled (byte-identical default posture).

func TestGrafana_ConfigMap_AlertmanagerDatasourceWhenEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
		"alertmanager.enabled=true",
	)
	if !strings.Contains(out, "name: Alertmanager") {
		t.Error("Alertmanager datasource should be present when alertmanager is enabled")
	}
	if !strings.Contains(out, "type: alertmanager") {
		t.Error("Alertmanager datasource should use the core alertmanager type")
	}
	if !strings.Contains(out, ":9093") {
		t.Error("Alertmanager datasource should point at the alertmanager Service on :9093")
	}
	if !strings.Contains(out, "implementation: prometheus") {
		t.Error("Alertmanager datasource should declare the prometheus implementation (prom/alertmanager v0.28.1 dialect, not the mimir default)")
	}
	if !strings.Contains(out, "handleGrafanaManagedAlerts: false") {
		t.Error("Alertmanager datasource should NOT handle Grafana-managed alerts (read/silence view of externally-fired alerts)")
	}
	if !strings.Contains(out, "uid: alertmanager") {
		t.Error("Alertmanager datasource should carry a stable uid")
	}
}

func TestGrafana_ConfigMap_NoAlertmanagerDatasourceWhenDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
	)
	if strings.Contains(out, "name: Alertmanager") {
		t.Error("Alertmanager datasource should NOT be present when alertmanager is disabled")
	}
}

// TestGrafana_Egress_AlertmanagerPairedBothSides guards the NetworkPolicy
// pairing that makes the datasource actually reachable: grafana-egress must open
// :9093 to Alertmanager AND alertmanager-ingress must admit the grafana peer.
// A one-sided allow silently drops every datasource query (ka-0000004459).
func TestGrafana_Egress_AlertmanagerPairedBothSides(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"grafana.enabled=true",
		"alertmanager.enabled=true",
	)
	// egress side: grafana-egress opens :9093 to the alertmanager pod.
	if !strings.Contains(out, "grafana-egress") {
		t.Fatal("grafana-egress policy should render when grafana is enabled")
	}
	if !strings.Contains(out, "component: alertmanager\n      ports:\n        - protocol: TCP\n          port: 9093") &&
		!strings.Contains(out, "port: 9093") {
		t.Error("grafana-egress should open :9093 to Alertmanager when both are enabled")
	}
	// ingress side: alertmanager-ingress admits the grafana peer even when
	// vmalert/vmagent are off (the guard now includes grafana.enabled).
	amOnly := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"grafana.enabled=true",
		"alertmanager.enabled=true",
		"vmalert.enabled=false",
		"vmagent.enabled=false",
	)
	if strings.Contains(amOnly, "ingress: []") {
		t.Error("alertmanager-ingress must NOT fail closed when grafana is a peer (grafana datasource needs :9093)")
	}
	if !strings.Contains(amOnly, "component: grafana") {
		t.Error("alertmanager-ingress must admit the grafana peer so the datasource can query :9093")
	}
}

// --- ServiceAccount on deployment ---

func TestGrafana_ServiceAccount_SetWhenSidecarEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.sidecar.enabled=true",
	)
	if !strings.Contains(out, "serviceAccountName") {
		t.Error("serviceAccountName should be set when sidecar is enabled")
	}
}

func TestGrafana_ServiceAccount_NotSetWhenSidecarDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
	)
	if strings.Contains(out, "serviceAccountName") {
		t.Error("serviceAccountName should NOT be set when sidecar is disabled")
	}
}

// --- RWO PVC Multi-Attach: Recreate strategy (ka-0000003526) ---

// With a ReadWriteOnce data PVC, a RollingUpdate brings the new pod up before
// the old releases the RWO volume → FailedAttachVolume Multi-Attach deadlock
// that trips verify-prod. The deployment must use Recreate when persistence is on.
func TestGrafana_Strategy_RecreateWhenPersistent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.persistence.enabled=true",
	)
	if !strings.Contains(out, "type: Recreate") {
		t.Error("grafana Deployment should use Recreate strategy when persistence is enabled (RWO PVC Multi-Attach)")
	}
}

// emptyDir tolerates RollingUpdate, so no Recreate is forced when persistence is off.
func TestGrafana_Strategy_NoRecreateWhenEphemeral(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
	)
	if strings.Contains(out, "type: Recreate") {
		t.Error("grafana Deployment should NOT force Recreate when persistence is disabled (emptyDir)")
	}
}

// grafana-bridge always mounts an RWO PVC for loop-breaker/SSE-dedup state, so
// it must always use Recreate to avoid the Multi-Attach deadlock on every deploy.
func TestGrafanaBridge_Strategy_AlwaysRecreate(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana-bridge/deployment.yaml",
		"grafanaBridge.enabled=true",
	)
	if !strings.Contains(out, "type: Recreate") {
		t.Error("grafana-bridge Deployment should always use Recreate strategy (RWO PVC Multi-Attach)")
	}
}

// --- Image Renderer (bead ka-0000001982) ---

func TestGrafana_ImageRenderer_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
	)
	if strings.Contains(out, "grafana-image-renderer") {
		t.Error("image-renderer sidecar should NOT be present when imageRenderer.enabled is false")
	}
	if strings.Contains(out, "GF_RENDERING_SERVER_URL") {
		t.Error("GF_RENDERING_SERVER_URL should NOT be set when imageRenderer is disabled")
	}
}

func TestGrafana_ImageRenderer_EnabledPresent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.imageRenderer.enabled=true",
	)
	if !strings.Contains(out, "grafana-image-renderer") {
		t.Error("image-renderer sidecar container should be present when imageRenderer.enabled=true")
	}
	if !strings.Contains(out, "GF_RENDERING_SERVER_URL") {
		t.Error("GF_RENDERING_SERVER_URL should be set on the grafana container")
	}
	if !strings.Contains(out, "GF_RENDERING_CALLBACK_URL") {
		t.Error("GF_RENDERING_CALLBACK_URL should be set on the grafana container")
	}
	// Renderer listens on localhost only — server URL must point at 127.0.0.1.
	if !strings.Contains(out, "http://127.0.0.1:8081/render") {
		t.Error("GF_RENDERING_SERVER_URL should point at the localhost sidecar")
	}
	// Callback derives the in-cluster Service ROOT URL (no /grafana subpath).
	if !strings.Contains(out, "http://test-grafana:3000/") {
		t.Error("GF_RENDERING_CALLBACK_URL should derive the in-cluster Service root URL")
	}
	if strings.Contains(out, "test-grafana:3000/grafana") {
		t.Error("callback URL must NOT carry the /grafana subpath (in-cluster API is at root)")
	}
	if !strings.Contains(out, "renderer-tmp") {
		t.Error("renderer-tmp volume should be present when imageRenderer is enabled")
	}
}

// --- Full-dashboard render hardening (bug ka-0000006919) ---
//
// The whole-board /render/d/<uid> route returned HTTP 500 (or hung to timeout)
// on tall multi-panel pages, and the single renderer instance cascaded 500s
// under any concurrency with a ~60-75s recovery, while a single small d-solo
// panel rendered fine. The fix hardens the sidecar three ways, each guarded
// below: clustered rendering (isolated context per concurrent render behind a
// bounded queue), --disable-dev-shm-usage (route Chromium shm off the 64Mi K8s
// /dev/shm a tall page exhausts), and a memory bump so a tall render does not
// OOM-kill+relaunch the sidecar (the observed "recovery").

func TestGrafana_ImageRenderer_ClusteredRenderingByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.imageRenderer.enabled=true",
	)
	// The single-browser "default" mode cascades 500s under concurrency; the
	// hardened default is clustered with an isolated context per render.
	if !strings.Contains(out, `name: RENDERING_MODE
              value: "clustered"`) {
		t.Error("RENDERING_MODE should default to \"clustered\" (single-browser \"default\" cascades 500s under concurrency, bug ka-0000006919)")
	}
	if !strings.Contains(out, `name: RENDERING_CLUSTERING_MODE
              value: "context"`) {
		t.Error("RENDERING_CLUSTERING_MODE should be \"context\" (memory-safe isolated context per render)")
	}
	if !strings.Contains(out, "RENDERING_CLUSTERING_MAX_CONCURRENCY") {
		t.Error("RENDERING_CLUSTERING_MAX_CONCURRENCY should bound the render queue so overflow waits instead of OOMing")
	}
	if !strings.Contains(out, "RENDERING_CLUSTERING_TIMEOUT") {
		t.Error("RENDERING_CLUSTERING_TIMEOUT should be set so tall boards get longer than the 30s upstream default")
	}
}

func TestGrafana_ImageRenderer_DisablesDevShm(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.imageRenderer.enabled=true",
	)
	// The load-bearing flag: K8s mounts /dev/shm at only 64Mi, which a tall
	// multi-panel page exhausts → the render tab crashes → /render/d/<uid> 500s.
	// --disable-dev-shm-usage routes Chromium shm to the disk-backed /tmp emptyDir.
	if !strings.Contains(out, "RENDERING_ARGS") {
		t.Fatal("RENDERING_ARGS should be set to pass Chromium launch flags")
	}
	if !strings.Contains(out, "--disable-dev-shm-usage") {
		t.Error("RENDERING_ARGS must include --disable-dev-shm-usage (the 64Mi K8s /dev/shm crashes tall renders, bug ka-0000006919)")
	}
	// Setting RENDERING_ARGS replaces the image defaults, so --no-sandbox must be
	// restated (the sidecar runs non-root with all caps dropped → the setuid
	// sandbox cannot initialise; without --no-sandbox Chromium fails to launch).
	if !strings.Contains(out, "--no-sandbox") {
		t.Error("RENDERING_ARGS must restate --no-sandbox (non-root sidecar cannot use the setuid sandbox)")
	}
}

// A tall full-board render plus the disk-backed shm workspace OOM-killed the
// 512Mi sidecar; the kill+relaunch was the observed ~60-75s recovery. The limit
// is bumped so a tall render holds without an OOM restart.
func TestGrafana_ImageRenderer_MemoryLimitBumped(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.imageRenderer.enabled=true",
	)
	// Scope the assertion to the grafana-image-renderer container. Asserting
	// over the whole rendered Deployment made this test read its SIBLING
	// containers' limits too: raising the main `grafana` container to 512Mi
	// (ka-0000003606 — its own limit sat at 1.06x its measured peak) tripped the
	// "must not contain 512Mi" check even though the sidecar was still 1Gi.
	block := grafanaContainerBlock(t, out, "grafana-image-renderer")
	if strings.Contains(block, "memory: 512Mi") {
		t.Errorf("image-renderer memory limit should be bumped above 512Mi (512Mi OOM-killed tall full-board renders, bug ka-0000006919), got:\n%s", block)
	}
	if !strings.Contains(block, "memory: 1Gi") {
		t.Errorf("image-renderer memory limit should be 1Gi to hold a tall render plus concurrent isolated contexts, got:\n%s", block)
	}
}

// grafanaContainerBlock returns the rendered YAML for a single container stanza
// (from its `- name: <container>` line to the next container or the end of the
// containers list), so a per-container assertion cannot accidentally match a
// sibling container's fields.
func grafanaContainerBlock(t *testing.T, out, container string) string {
	t.Helper()
	marker := "- name: " + container + "\n"
	start := strings.Index(out, marker)
	if start < 0 {
		t.Fatalf("container %q not found in render:\n%s", container, out)
	}
	rest := out[start+len(marker):]
	// A sibling container starts at the same indentation as this one.
	indent := ""
	if ls := strings.LastIndex(out[:start], "\n"); ls >= 0 {
		indent = out[ls+1 : start]
	}
	if next := strings.Index(rest, "\n"+indent+"- name: "); next >= 0 {
		return rest[:next]
	}
	return rest
}

// Operators can pin the sidecar back to the single-browser "default" mode; the
// clustering env vars must then drop out so no stray RENDERING_CLUSTERING_* is
// left dangling.
func TestGrafana_ImageRenderer_DefaultModeDropsClusteringEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.imageRenderer.enabled=true",
		"grafana.imageRenderer.mode=default",
	)
	if !strings.Contains(out, `name: RENDERING_MODE
              value: "default"`) {
		t.Error("RENDERING_MODE should honour an explicit mode=default override")
	}
	if strings.Contains(out, "RENDERING_CLUSTERING_MODE") {
		t.Error("RENDERING_CLUSTERING_* env should NOT render when mode is not clustered")
	}
}

func TestGrafana_ImageRenderer_CallbackOverride(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.imageRenderer.enabled=true",
		"grafana.imageRenderer.callbackURL=http://custom:3000/",
	)
	if !strings.Contains(out, "http://custom:3000/") {
		t.Error("explicit imageRenderer.callbackURL should override the derived URL")
	}
}

// --- Subpath-mode render consistency (bug ka-0000001440) ---
//
// The image-renderer's headless browser reaches the Grafana pod DIRECTLY (no
// Traefik stripPrefix). When serveFromSubPath=true the pod owns /grafana
// end-to-end, so the derived GF_RENDERING_CALLBACK_URL must carry the subpath
// — otherwise the SPA <base href> resolves to /grafana/ while assets are
// fetched from a root base, every /grafana/public/build/*.js 404s, and the
// renderer captures Grafana's "failed to load application files" page instead
// of a PNG. serve_from_sub_path, root_url, and the callback must all agree.
func TestGrafana_ImageRenderer_CallbackCarriesSubpathWhenServeFromSubPath(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.imageRenderer.enabled=true",
		"grafana.serveFromSubPath=true",
		"grafana.rootURL=https://gardenio.app.e2e.dev.acme.example/grafana",
	)
	// All three must agree: serve_from_sub_path on, root_url subpath, callback
	// carrying /grafana on the in-cluster Service.
	if !strings.Contains(out, "GF_SERVER_SERVE_FROM_SUB_PATH") {
		t.Error("GF_SERVER_SERVE_FROM_SUB_PATH should render in subpath mode")
	}
	if !strings.Contains(out, "http://test-grafana:3000/grafana/") {
		t.Error("GF_RENDERING_CALLBACK_URL must carry the /grafana subpath in subpath mode (else the renderer 404s the SPA assets)")
	}
	// Probes must also follow the subpath so the pod stays Ready.
	if !strings.Contains(out, "path: /grafana/api/health") {
		t.Error("health probe should follow the subpath: /grafana/api/health")
	}
}

// In the legacy "proxy strips the prefix" mode (serveFromSubPath=false), the pod
// serves at the Service ROOT, so the derived callback must NOT carry /grafana.
func TestGrafana_ImageRenderer_CallbackAtRootWhenNotSubpath(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.imageRenderer.enabled=true",
	)
	if !strings.Contains(out, "http://test-grafana:3000/") {
		t.Error("derived callback should be the root Service URL when serveFromSubPath is off")
	}
	if strings.Contains(out, "test-grafana:3000/grafana") {
		t.Error("callback must NOT carry /grafana when serveFromSubPath is off (pod serves at root)")
	}
}

// The agent GRAFANA_URL the controller forwards is the SAME in-cluster base the
// renderer callback uses — so `gb grafana render` resolves the same working base.
// In subpath mode it must carry /grafana too (bug ka-0000001440).
func TestController_GrafanaURL_CarriesSubpathWhenServeFromSubPath(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"grafana.enabled=true",
		"grafana.serveFromSubPath=true",
		"grafana.rootURL=https://gardenio.app.e2e.dev.acme.example/grafana",
	)
	if !strings.Contains(out, "http://test-grafana:3000/grafana") {
		t.Error("GRAFANA_URL must carry the /grafana subpath in subpath mode so `gb grafana render` hits the working base")
	}
}

// --- GRAFANA_URL injection on the controller (bug ka-0000005842) ---

func TestController_GrafanaURL_InjectedWhenGrafanaEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"grafana.enabled=true",
	)
	if !strings.Contains(out, "GRAFANA_URL") {
		t.Error("GRAFANA_URL should be forwarded to agent pods when grafana.enabled=true")
	}
	if !strings.Contains(out, "http://test-grafana:3000") {
		t.Error("GRAFANA_URL should derive the in-cluster Service root URL")
	}
}

func TestController_GrafanaURL_NotInjectedWhenDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
	)
	if strings.Contains(out, "GRAFANA_URL") {
		t.Error("GRAFANA_URL should NOT be set when grafana is disabled and no override")
	}
}

func TestController_GrafanaAuthProxyHeader_InjectedWhenProxyEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"grafana.enabled=true",
		"grafana.auth.proxy.enabled=true",
	)
	if !strings.Contains(out, "GRAFANA_AUTH_PROXY_HEADER") {
		t.Error("GRAFANA_AUTH_PROXY_HEADER should be forwarded when auth-proxy is on")
	}
	if !strings.Contains(out, "X-Auth-Request-Email") {
		t.Error("GRAFANA_AUTH_PROXY_HEADER should carry the configured header name")
	}
}

func TestController_GrafanaAuthProxyHeader_NotInjectedWhenProxyOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"grafana.enabled=true",
	)
	if strings.Contains(out, "GRAFANA_AUTH_PROXY_HEADER") {
		t.Error("GRAFANA_AUTH_PROXY_HEADER should NOT be set when auth-proxy is off")
	}
}
