package tests

import (
	"strings"
	"testing"
)

// Edge middlewares (task ka-0000006519; ingress research doc §4 / ka-0000002581
// G4/G5). Two NEW gated Traefik Middleware resources — inFlightReq (concurrency
// cap) and buffering (request-body cap) — that did not exist in the chart
// before. Both DEFAULT OFF in the base chart and turned ON by the
// Gardenio-in-a-Box overlay (deploy-values/gardenio-inbox.yaml). When enabled
// they are prepended (cheapest-reject-first) to every IngressRoute's middleware
// chain via the gardenio.edgeMiddlewares helper.

// 1. DEFAULT OFF: neither middleware renders out of the box.
func TestEdgeMiddlewares_DefaultDisabled(t *testing.T) {
	t.Parallel()
	// The whole edge-middlewares.yaml template is suppressed when neither is
	// enabled, so --show-only errors; render the full chart and assert absence.
	out := helmTemplateFull(t, nil)
	if strings.Contains(out, "edge-buffering") {
		t.Error("edge-buffering middleware must NOT render when edge.buffering.enabled is false (default)")
	}
	if strings.Contains(out, "edge-inflightreq") {
		t.Error("edge-inflightreq middleware must NOT render when edge.inFlightReq.enabled is false (default)")
	}
}

// 2. inFlightReq enabled: the Middleware renders with the configured amount.
func TestEdgeMiddlewares_InFlightReqEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/edge-middlewares.yaml",
		"edge.inFlightReq.enabled=true",
		"edge.inFlightReq.amount=37",
	)
	if !strings.Contains(out, "name: test-edge-inflightreq") {
		t.Fatalf("inFlightReq Middleware should render when enabled:\n%s", out)
	}
	if !strings.Contains(out, "inFlightReq:") {
		t.Error("rendered Middleware should carry an inFlightReq spec")
	}
	if !strings.Contains(out, "amount: 37") {
		t.Errorf("inFlightReq.amount should be 37 (the --set override), not found in:\n%s", out)
	}
	// buffering left off → must be absent.
	if strings.Contains(out, "edge-buffering") {
		t.Error("buffering Middleware must NOT render when only inFlightReq is enabled")
	}
}

// 3. buffering enabled: the Middleware renders with the configured caps.
func TestEdgeMiddlewares_BufferingEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/edge-middlewares.yaml",
		"edge.buffering.enabled=true",
		"edge.buffering.maxRequestBodyBytes=3000000",
		"edge.buffering.memRequestBodyBytes=524288",
	)
	if !strings.Contains(out, "name: test-edge-buffering") {
		t.Fatalf("buffering Middleware should render when enabled:\n%s", out)
	}
	if !strings.Contains(out, "buffering:") {
		t.Error("rendered Middleware should carry a buffering spec")
	}
	if !strings.Contains(out, "maxRequestBodyBytes: 3000000") {
		t.Errorf("buffering.maxRequestBodyBytes should be 3000000, not found in:\n%s", out)
	}
	if !strings.Contains(out, "memRequestBodyBytes: 524288") {
		t.Errorf("buffering.memRequestBodyBytes should be 524288, not found in:\n%s", out)
	}
	// inFlightReq left off → must be absent.
	if strings.Contains(out, "edge-inflightreq") {
		t.Error("inFlightReq Middleware must NOT render when only buffering is enabled")
	}
}

// 4. Default amounts (~50 / ~2 MB) when only the enable flag is flipped.
func TestEdgeMiddlewares_Defaults(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/edge-middlewares.yaml",
		"edge.inFlightReq.enabled=true",
		"edge.buffering.enabled=true",
	)
	if !strings.Contains(out, "amount: 50") {
		t.Errorf("default inFlightReq.amount should be 50, not found in:\n%s", out)
	}
	if !strings.Contains(out, "maxRequestBodyBytes: 2000000") {
		t.Errorf("default buffering.maxRequestBodyBytes should be 2000000 (~2 MB), not found in:\n%s", out)
	}
	if !strings.Contains(out, "memRequestBodyBytes: 1048576") {
		t.Errorf("default buffering.memRequestBodyBytes should be 1048576 (1 MiB), not found in:\n%s", out)
	}
}

//  5. ATTACHED to a route when enabled, in cheap-reject-first order
//     (buffering → inFlightReq), ahead of the existing security-headers /
//     forwardAuth chain. grafana is a representative forwardAuth-gated route.
func TestEdgeMiddlewares_AttachedToRouteWhenEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/ingressroute.yaml",
		"grafana.enabled=true",
		"grafana.ingress.enabled=true",
		"edge.inFlightReq.enabled=true",
		"edge.buffering.enabled=true",
	)
	bufIdx := strings.Index(out, "- name: test-edge-buffering")
	flrIdx := strings.Index(out, "- name: test-edge-inflightreq")
	secIdx := strings.Index(out, "- name: test-security-headers")
	if bufIdx < 0 || flrIdx < 0 {
		t.Fatalf("both edge middlewares should be attached to the grafana route:\n%s", out)
	}
	if !(bufIdx < flrIdx) {
		t.Error("buffering (413) must be chained before inFlightReq (429) — cheapest reject first")
	}
	if secIdx >= 0 && !(flrIdx < secIdx) {
		t.Error("edge middlewares must be chained before security-headers")
	}
}

// 6. NOT attached to a route when disabled (default).
func TestEdgeMiddlewares_NotAttachedWhenDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/ingressroute.yaml",
		"grafana.enabled=true",
		"grafana.ingress.enabled=true",
	)
	if strings.Contains(out, "edge-buffering") || strings.Contains(out, "edge-inflightreq") {
		t.Errorf("edge middlewares must NOT be attached to routes when disabled:\n%s", out)
	}
}
