package tests

import (
	"os/exec"
	"strings"
	"testing"
)

// In-chart VictoriaTraces store (epic ka-0000000598, task ka-0000002444).
// A single-node vtsingle StatefulSet mirroring the vmsingle shape, default-off.
// These tests pin both the opt-in render AND the default-off invariant so the
// chart stays byte-identical for every env that does not set vtsingle.enabled.

// --- default-off invariant ---

func TestVtsingle_DefaultOff_NoResources(t *testing.T) {
	t.Parallel()
	// helm --show-only errors when the named template renders nothing, which is
	// exactly the default-off contract. Assert the StatefulSet does NOT render.
	out, err := exec.Command("helm", "template", "test", chartDir(t),
		"--show-only", "templates/vtsingle/statefulset.yaml",
	).CombinedOutput()
	if err == nil {
		t.Fatalf("vtsingle StatefulSet must NOT render when vtsingle.enabled is false; got:\n%s", out)
	}
}

func TestVtsingle_DefaultOff_AbsentFromFullRender(t *testing.T) {
	t.Parallel()
	out, err := exec.Command("helm", "template", "test", chartDir(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "component: vtsingle") {
		t.Error("no vtsingle resources should render when vtsingle.enabled is false")
	}
}

// --- StatefulSet shape ---

func TestVtsingle_StatefulSet_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vtsingle/statefulset.yaml",
		"vtsingle.enabled=true",
	)
	for _, want := range []string{
		"kind: StatefulSet",
		"name: test-victoria-traces",
		"-retentionPeriod=7d",
		"-storageDataPath=/victoria-traces-data",
		"-httpListenAddr=:10428",
		"containerPort: 10428",
		"mountPath: /victoria-traces-data",
		"storage: 20Gi",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in vtsingle StatefulSet; got:\n%s", want, out)
		}
	}
}

func TestVtsingle_DiskCapFlag_Default(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vtsingle/statefulset.yaml",
		"vtsingle.enabled=true",
	)
	if !strings.Contains(out, "-retention.maxDiskSpaceUsageBytes=16GiB") {
		t.Error("expected disk-cap self-trim flag to default to 16GiB (~80% of the 20Gi PVC)")
	}
}

func TestVtsingle_DiskCapFlag_Omittable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vtsingle/statefulset.yaml",
		"vtsingle.enabled=true",
		"vtsingle.retention.maxDiskSpaceUsageBytes=",
	)
	if strings.Contains(out, "maxDiskSpaceUsageBytes") {
		t.Error("disk-cap flag must be omitted when maxDiskSpaceUsageBytes is empty")
	}
}

// --- OTLP/gRPC optional listener (off by default) ---

func TestVtsingle_OtlpGRPC_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vtsingle/statefulset.yaml",
		"vtsingle.enabled=true",
	)
	if strings.Contains(out, "otlpGRPCListenAddr") || strings.Contains(out, "containerPort: 4317") {
		t.Error("OTLP/gRPC listener must be off by default; only :10428 HTTP should render")
	}
}

func TestVtsingle_OtlpGRPC_Enabled(t *testing.T) {
	t.Parallel()
	ss := helmTemplate(t, "templates/vtsingle/statefulset.yaml",
		"vtsingle.enabled=true",
		"vtsingle.otlpGRPC.enabled=true",
	)
	if !strings.Contains(ss, "-otlpGRPCListenAddr=:4317") || !strings.Contains(ss, "containerPort: 4317") {
		t.Errorf("expected OTLP/gRPC listener on :4317 when enabled; got:\n%s", ss)
	}
	svc := helmTemplate(t, "templates/vtsingle/service.yaml",
		"vtsingle.enabled=true",
		"vtsingle.otlpGRPC.enabled=true",
	)
	if !strings.Contains(svc, "name: otlp-grpc") || !strings.Contains(svc, "port: 4317") {
		t.Errorf("expected otlp-grpc Service port when enabled; got:\n%s", svc)
	}
}

// --- ephemeral fallback (throwaway test namespaces) ---

func TestVtsingle_Ephemeral_EmptyDir(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vtsingle/statefulset.yaml",
		"vtsingle.enabled=true",
		"vtsingle.persistence.enabled=false",
	)
	if strings.Contains(out, "volumeClaimTemplates") {
		t.Error("no volumeClaimTemplates when persistence disabled")
	}
	if !strings.Contains(out, "emptyDir: {}") {
		t.Error("expected emptyDir-backed data volume when persistence disabled")
	}
}

// --- Service ---

func TestVtsingle_Service_HTTPPort(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vtsingle/service.yaml",
		"vtsingle.enabled=true",
	)
	for _, want := range []string{"kind: Service", "type: ClusterIP", "port: 10428", "name: http"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in vtsingle Service; got:\n%s", want, out)
		}
	}
}

// --- PUSH-fallback IngressRoute (designed-but-disabled) ---

func TestVtsingle_Ingest_DefaultOff(t *testing.T) {
	t.Parallel()
	out, err := exec.Command("helm", "template", "test", chartDir(t),
		"--set", "vtsingle.enabled=true",
		"--show-only", "templates/vtsingle/ingressroute.yaml",
	).CombinedOutput()
	if err == nil {
		t.Fatalf("vtsingle ingest IngressRoute must NOT render when ingest.enabled is false; got:\n%s", out)
	}
}

func TestVtsingle_Ingest_RenderedExposesInsertOnly(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vtsingle/ingressroute.yaml",
		"vtsingle.enabled=true",
		"vtsingle.ingest.enabled=true",
		"vtsingle.ingest.host=traces.example.com",
		"vtsingle.ingest.auth.secretName=vt-ingest",
		"global.traefik.ingressRoutes.enabled=true",
	)
	if !strings.Contains(out, "PathPrefix(`/insert/opentelemetry/v1/traces`)") {
		t.Errorf("expected OTLP insert path prefix; got:\n%s", out)
	}
	if !strings.Contains(out, "port: 10428") {
		t.Errorf("ingest route must target the :10428 listener; got:\n%s", out)
	}
	// VT has no auth of its own — the select/admin surface must never be exposed.
	if strings.Contains(out, "/select/") {
		t.Error("ingest route must NOT expose the select/query API")
	}
	if !strings.Contains(out, "basicAuth") || !strings.Contains(out, "secret: vt-ingest") {
		t.Errorf("expected basicAuth middleware referencing the ingest secret; got:\n%s", out)
	}
}

// Bug ka-0000004920: the ingest route hardcoded entryPoints [web, websecure].
// On prod-gardenio-eks those are served only by the internal-scheme `traefik`
// NLB behind AWS Verified Access, so an out-of-cluster otel-collector could
// never reach the OTLP insert endpoint. entryPoints is now configurable
// (default unchanged), mirroring the vmsingle fix for ka-0000005734.

func TestVtsingle_Ingest_EntryPoints_DefaultUnchanged(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vtsingle/ingressroute.yaml",
		"vtsingle.enabled=true",
		"vtsingle.ingest.enabled=true",
		"vtsingle.ingest.host=traces.example.com",
		"vtsingle.ingest.auth.secretName=vt-ingest",
		"global.traefik.ingressRoutes.enabled=true",
	)
	for _, want := range []string{"- web", "- websecure"} {
		if !strings.Contains(out, want) {
			t.Errorf("default ingest route must keep entrypoint %q; got:\n%s", want, out)
		}
	}
}

func TestVtsingle_Ingest_EntryPoints_Overridable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vtsingle/ingressroute.yaml",
		"vtsingle.enabled=true",
		"vtsingle.ingest.enabled=true",
		"vtsingle.ingest.host=traces.example.com",
		"vtsingle.ingest.auth.secretName=vt-ingest",
		"vtsingle.ingest.entryPoints={webhooks}",
		// `webhooks` is internet-facing, so the exposure guard
		// (validate-tsdb-ingest-exposure.yaml, bug ka-0000001172) requires the
		// source allow-list here. This test is about the entrypoint knob, so
		// give it the safe posture rather than the acknowledgment escape hatch.
		"vtsingle.ingest.ipWhitelist.enabled=true",
		"vtsingle.ingest.ipWhitelist.sourceRange={198.51.100.7/32}",
		"global.traefik.ingressRoutes.enabled=true",
	)
	if !strings.Contains(out, "- webhooks") {
		t.Errorf("expected the overridden `webhooks` entrypoint; got:\n%s", out)
	}
	// The internal-only entrypoints must be gone, not merely joined — binding
	// both would leave the route on the internal NLB as well.
	entryBlock := out[strings.Index(out, "entryPoints:"):]
	entryBlock = entryBlock[:strings.Index(entryBlock, "routes:")]
	for _, unwanted := range []string{"- web\n", "- websecure"} {
		if strings.Contains(entryBlock, unwanted) {
			t.Errorf("override must REPLACE the default entrypoints, found %q in:\n%s", unwanted, entryBlock)
		}
	}
}

// The vtsingle grafana-dashboard ConfigMap was removed in ka-0000002799 (the
// chart no longer renders dashboard ConfigMaps — they double-stored ~200KB per
// revision in the release Secret and overflowed the K8s 1 MiB limit). The
// perf-traces and monorepo-distributed-traces boards now ride in the
// grafana-bridge image and are provisioned at runtime. The JSON CONTENT of both
// boards is guarded by the disk-reading tests in
// grafana_perf_traces_dashboard_test.go and
// grafana_monorepo_distributed_traces_dashboard_test.go.
