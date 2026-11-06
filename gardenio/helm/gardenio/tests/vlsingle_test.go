package tests

import (
	"os/exec"
	"strings"
	"testing"
)

// In-chart VictoriaLogs store (task ka-0000007261, epic ka-0000007666).
// A single-node vlsingle StatefulSet mirroring the vmsingle/vtsingle shape,
// default-off. These tests pin both the opt-in render AND the default-off
// invariant so the chart stays byte-identical for every env that does not set
// vlsingle.enabled. They also pin the contract that binds this store to the
// LogsQL rule evaluator: the Service MUST render as `test-victoria-logs`:9428,
// which is what gardenio.vmalertLogs.datasourceURL auto-wires to.

// --- default-off invariant ---

func TestVlsingle_DefaultOff_NoResources(t *testing.T) {
	t.Parallel()
	// helm --show-only errors when the named template renders nothing, which is
	// exactly the default-off contract. Assert the StatefulSet does NOT render.
	out, err := exec.Command("helm", "template", "test", chartDir(t),
		"--show-only", "templates/vlsingle/statefulset.yaml",
	).CombinedOutput()
	if err == nil {
		t.Fatalf("vlsingle StatefulSet must NOT render when vlsingle.enabled is false; got:\n%s", out)
	}
}

func TestVlsingle_DefaultOff_AbsentFromFullRender(t *testing.T) {
	t.Parallel()
	out, err := exec.Command("helm", "template", "test", chartDir(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "component: vlsingle") {
		t.Error("no vlsingle resources should render when vlsingle.enabled is false")
	}
}

// --- StatefulSet shape ---

func TestVlsingle_StatefulSet_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vlsingle/statefulset.yaml",
		"vlsingle.enabled=true",
	)
	for _, want := range []string{
		"kind: StatefulSet",
		"name: test-victoria-logs",
		"-retentionPeriod=30d",
		"-storageDataPath=/victoria-logs-data",
		"-httpListenAddr=:9428",
		"containerPort: 9428",
		"mountPath: /victoria-logs-data",
		"storage: 10Gi",
		// Hardened pod: nonroot + read-only rootfs + dropped caps (mirrors vmsingle).
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in vlsingle StatefulSet; got:\n%s", want, out)
		}
	}
}

func TestVlsingle_DiskCapFlag_Default(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vlsingle/statefulset.yaml",
		"vlsingle.enabled=true",
	)
	if !strings.Contains(out, "-retention.maxDiskSpaceUsageBytes=8GiB") {
		t.Error("expected disk-cap self-trim flag to default to 8GiB (~80% of the 10Gi PVC)")
	}
}

func TestVlsingle_DiskCapFlag_Omittable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vlsingle/statefulset.yaml",
		"vlsingle.enabled=true",
		"vlsingle.retention.maxDiskSpaceUsageBytes=",
	)
	if strings.Contains(out, "maxDiskSpaceUsageBytes") {
		t.Error("disk-cap flag must be omitted when maxDiskSpaceUsageBytes is empty")
	}
}

// --- ephemeral fallback (throwaway test namespaces) ---

func TestVlsingle_Ephemeral_EmptyDir(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vlsingle/statefulset.yaml",
		"vlsingle.enabled=true",
		"vlsingle.persistence.enabled=false",
	)
	if strings.Contains(out, "volumeClaimTemplates") {
		t.Error("no volumeClaimTemplates when persistence disabled")
	}
	if !strings.Contains(out, "emptyDir: {}") {
		t.Error("expected emptyDir-backed data volume when persistence disabled")
	}
}

// --- Service ---

func TestVlsingle_Service_HTTPPort(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vlsingle/service.yaml",
		"vlsingle.enabled=true",
	)
	for _, want := range []string{"kind: Service", "type: ClusterIP", "port: 9428", "name: http", "name: test-victoria-logs"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in vlsingle Service; got:\n%s", want, out)
		}
	}
}

// --- datasource contract: vmalert-logs auto-wires to this Service ---
// The whole point of standing up vlsingle is that the LogsQL detection evaluator
// (vmalert-logs) can query it with NO explicit datasourceURL override. Pin that
// the evaluator's -datasource.url resolves to this store's Service name + port.

func TestVlsingle_VmalertLogsDatasource_AutoWires(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert-logs/deployment.yaml",
		"vlsingle.enabled=true",
		"vmalertLogs.enabled=true",
	)
	if !strings.Contains(out, "-datasource.url=http://test-victoria-logs:9428") {
		t.Errorf("expected vmalert-logs -datasource.url to auto-wire to test-victoria-logs:9428; got:\n%s", out)
	}
}

// --- NetworkPolicy ---

func TestVlsingle_NetworkPolicy_IngressRendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"vlsingle.enabled=true",
		"vmalertLogs.enabled=true",
	)
	if !strings.Contains(out, "name: test-vlsingle-ingress") {
		t.Errorf("expected vlsingle-ingress NetworkPolicy; got:\n%s", out)
	}
	// The evaluator peer must be allowed to reach the :9428 select API.
	if !strings.Contains(out, "component: vmalert-logs") {
		t.Error("vlsingle-ingress must allow the vmalert-logs peer when it is enabled")
	}
	if !strings.Contains(out, "port: 9428") {
		t.Error("vlsingle-ingress must open the :9428 listener")
	}
}

func TestVlsingle_NetworkPolicy_TransportPeer_Gated(t *testing.T) {
	t.Parallel()
	// The CloudWatch->VL transport carve-out renders only when its pod labels are
	// configured (the transport is a sibling task, ka-0000007889, not yet live).
	off := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"vlsingle.enabled=true",
	)
	if strings.Contains(off, "cw-vl-transport") {
		t.Error("transport carve-out must not render until ingest.podSelectorLabels is set")
	}
	on := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"vlsingle.enabled=true",
		"vlsingle.ingest.podSelectorLabels.app=cw-vl-transport",
	)
	if !strings.Contains(on, "app: cw-vl-transport") {
		t.Error("transport carve-out must render when ingest.podSelectorLabels is set")
	}
}

// --- self-scrape (ka-0000001302) ---

func TestVlsingle_MetricsScrape_DefaultOff(t *testing.T) {
	t.Parallel()
	// vlsingle enabled but metrics.scrape unset: the pod must NOT be annotated,
	// so the chart stays byte-identical for envs that do not opt in.
	out := helmTemplate(t, "templates/vlsingle/statefulset.yaml",
		"vlsingle.enabled=true",
	)
	if strings.Contains(out, "prometheus.io/scrape") {
		t.Errorf("vlsingle must NOT carry a scrape annotation unless metrics.scrape=true; got:\n%s", out)
	}
}

func TestVlsingle_MetricsScrape_OptIn(t *testing.T) {
	t.Parallel()
	// metrics.scrape=true annotates the pod on the :9428 /metrics listener so
	// the in-chart vmagent discovers it and lands vl_* in vmsingle — the
	// datasource the VlsingleDiskUsageHigh rule needs (else it never fires).
	out := helmTemplate(t, "templates/vlsingle/statefulset.yaml",
		"vlsingle.enabled=true",
		"vlsingle.metrics.scrape=true",
	)
	for _, want := range []string{
		`prometheus.io/scrape: "true"`,
		`prometheus.io/port: "9428"`,
		`prometheus.io/path: "/metrics"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q on the scraped vlsingle pod; got:\n%s", want, out)
		}
	}
}

func TestVlsingle_NetworkPolicy_VmagentCarveOut_Gated(t *testing.T) {
	t.Parallel()
	// The vmagent :9428 carve-out in vlsingle-ingress renders only when BOTH
	// vmagent is enabled AND metrics.scrape is on — matching the pod annotation
	// gate, so the scrape is not blackholed by default-deny under enforcement.
	off := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"vlsingle.enabled=true",
		"vmagent.enabled=true",
	)
	if strings.Contains(vlsingleIngressBlock(t, off), "component: vmagent") {
		t.Error("vlsingle-ingress must NOT allow vmagent until vlsingle.metrics.scrape=true")
	}
	on := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"vlsingle.enabled=true",
		"vmagent.enabled=true",
		"vlsingle.metrics.scrape=true",
	)
	if !strings.Contains(vlsingleIngressBlock(t, on), "component: vmagent") {
		t.Error("vlsingle-ingress must allow the vmagent peer when metrics.scrape=true")
	}
}

// vlsingleIngressBlock returns just the vlsingle-ingress NetworkPolicy document
// from a multi-doc render, so peer assertions do not leak into the following
// policies (e.g. the vmagent-egress policy, which legitimately names vmagent).
func vlsingleIngressBlock(t *testing.T, out string) string {
	t.Helper()
	start := strings.Index(out, "name: test-vlsingle-ingress")
	if start < 0 {
		t.Fatalf("vlsingle-ingress NetworkPolicy not found in render:\n%s", out)
	}
	rest := out[start:]
	if end := strings.Index(rest, "\n---\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}
