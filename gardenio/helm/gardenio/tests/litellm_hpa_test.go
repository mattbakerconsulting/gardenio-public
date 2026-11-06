package tests

import (
	"strings"
	"testing"
)

// H5 (epic ka-0000005228): HorizontalPodAutoscaler for the in-cluster litellm
// proxy — the fleet LLM data-plane. The HPA holds an HA floor (minReplicas>=2)
// and adds CPU-driven scale-up headroom. These guards pin the shape that keeps
// it safe on the real cluster:
//
//   - CPU utilization is a Resource metric served by metrics-server
//     (metrics.k8s.io), present on prod-gardenio-eks + in-box k3s — always safe.
//   - the latency / in-flight custom metric needs a custom.metrics.k8s.io
//     adapter that is NOT installed today, so it is gated OFF and must not
//     render unless explicitly enabled (an unavailable metric can wedge HPA
//     scaling decisions).
//   - when the HPA is active the Deployment must omit `replicas` (owned by the
//     HPA); that half of the contract lives in litellm_strategy_test.go.

func TestLitellmHPA_DefaultShape(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/hpa.yaml", "litellm.enabled=true")
	if !strings.Contains(out, "apiVersion: autoscaling/v2") {
		t.Errorf("litellm HPA must be autoscaling/v2; got:\n%s", out)
	}
	if !strings.Contains(out, "kind: HorizontalPodAutoscaler") {
		t.Fatalf("litellm HPA must render by default; got:\n%s", out)
	}
	if !strings.Contains(out, "minReplicas: 2") {
		t.Errorf("litellm HPA floor must be minReplicas: 2; got:\n%s", out)
	}
	if !strings.Contains(out, "maxReplicas: 6") {
		t.Errorf("litellm HPA default ceiling must be maxReplicas: 6; got:\n%s", out)
	}
	// scaleTargetRef must point at the litellm Deployment (same fullname).
	if !strings.Contains(out, "kind: Deployment") || !strings.Contains(out, "name: test-litellm") {
		t.Errorf("litellm HPA must target the litellm Deployment; got:\n%s", out)
	}
	// CPU Utilization metric present.
	if !strings.Contains(out, "name: cpu") || !strings.Contains(out, "averageUtilization: 70") {
		t.Errorf("litellm HPA must include a CPU Utilization:70 metric; got:\n%s", out)
	}
	// Custom latency/in-flight and memory metrics are OFF by default.
	if strings.Contains(out, "type: Pods") {
		t.Errorf("latency/in-flight custom metric must be gated OFF by default (needs a custom.metrics adapter); got:\n%s", out)
	}
	if strings.Contains(out, "name: memory") {
		t.Errorf("memory metric must be OFF by default; got:\n%s", out)
	}
}

// The optional custom latency/in-flight metric renders only when explicitly
// enabled, as a Pods-type AverageValue metric.
func TestLitellmHPA_CustomMetric_OptIn(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/hpa.yaml",
		"litellm.enabled=true",
		"litellm.autoscaling.customMetric.enabled=true",
	)
	if !strings.Contains(out, "type: Pods") {
		t.Fatalf("enabled custom metric must render a Pods metric; got:\n%s", out)
	}
	if !strings.Contains(out, "name: litellm_requests_metric") {
		t.Errorf("custom metric must carry the configured series name; got:\n%s", out)
	}
	if !strings.Contains(out, "type: AverageValue") {
		t.Errorf("custom metric must target an AverageValue across pods; got:\n%s", out)
	}
	// CPU metric still present alongside it.
	if !strings.Contains(out, "name: cpu") {
		t.Errorf("CPU metric must remain when the custom metric is added; got:\n%s", out)
	}
}

// The optional memory Resource metric renders only when a target is set.
func TestLitellmHPA_MemoryMetric_OptIn(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/hpa.yaml",
		"litellm.enabled=true",
		"litellm.autoscaling.targetMemoryUtilizationPercentage=80",
	)
	if !strings.Contains(out, "name: memory") || !strings.Contains(out, "averageUtilization: 80") {
		t.Errorf("memory metric must render at Utilization:80 when set; got:\n%s", out)
	}
}

// Dropping the CPU target removes the CPU metric (so an operator can run a
// pure custom-metric HPA once an adapter exists).
func TestLitellmHPA_CPUTarget_CanBeDropped(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/hpa.yaml",
		"litellm.enabled=true",
		"litellm.autoscaling.targetCPUUtilizationPercentage=0",
		"litellm.autoscaling.customMetric.enabled=true",
	)
	if strings.Contains(out, "name: cpu") {
		t.Errorf("CPU metric must drop when targetCPUUtilizationPercentage is unset/0; got:\n%s", out)
	}
	if !strings.Contains(out, "type: Pods") {
		t.Errorf("custom metric must still render as the sole metric; got:\n%s", out)
	}
}

// The HPA is gated on litellm.enabled — nothing renders when the proxy is off.
func TestLitellmHPA_SuppressedWhenLitellmDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/litellm/hpa.yaml",
		"litellm.enabled=false")
	if err == nil && strings.Contains(out, "kind: HorizontalPodAutoscaler") {
		t.Errorf("HPA must not render when litellm is disabled; got:\n%s", out)
	}
}
