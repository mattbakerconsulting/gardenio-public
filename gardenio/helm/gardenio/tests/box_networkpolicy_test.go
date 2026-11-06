package tests

import (
	"strings"
	"testing"
)

// P3.4 NetworkPolicy default-on (ka-0000005564; ingress §7 ka-0000002581,
// exposure-hardening F1–F3 ka-0000003100). The base chart ships the whole
// NetworkPolicy layer OFF; the Gardenio-in-a-Box overlay
// (deploy-values/gardenio-inbox.yaml) flips it ON — the single most important
// fail-closed flip. These tests pin the overlay wiring.

// 1. DEFAULT OFF: no NetworkPolicy renders out of the box.
func TestBoxNetPol_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, []string{"values-test.yaml"})
	if strings.Contains(out, "kind: NetworkPolicy") {
		t.Error("no NetworkPolicy should render when networkPolicies.enabled is false (default)")
	}
}

//  5. ka-0000006038: the in-box overlay must trim the namespace LimitRange
//     defaultRequest so the resource-less privileged DinD sidecar does not
//     inherit a 2-core / 16Gi request. On the single ~8-vCPU box that default
//     makes every 4-core agent pod unschedulable (Pending/Insufficient cpu) and
//     the box goes silent. Pin the box-sized defaultRequest.
func TestBoxLimitRange_InboxOverlayTrimsDefaultRequest(t *testing.T) {
	t.Parallel()
	// The LimitRange trim lives in the chart values-inbox.yaml (the box layers
	// it under the deploy-values overlay; see deploy-values/gardenio-inbox.yaml
	// header). tls.mode is disabled here because the box supplies tls.hosts via
	// --set at bootstrap, which is out of scope for this assertion.
	out := helmTemplateFull(t, []string{"values-inbox.yaml"},
		"edgeAuth.dryRun=true",
		"tls.mode=disabled",
	)
	seg := sliceKind(out, "LimitRange")
	if seg == "" {
		t.Fatal("in-box overlay must render a LimitRange")
	}
	// The DinD-killing prod default is cpu: "2" in defaultRequest. The box
	// overlay must bring it down (we pin 250m).
	if !strings.Contains(seg, `cpu: "250m"`) {
		t.Error("in-box LimitRange defaultRequest.cpu must be trimmed to 250m so the DinD sidecar does not request 2 cores (ka-0000006038)")
	}
	if strings.Contains(seg, "defaultRequest:\n        cpu: \"2\"") {
		t.Error("in-box LimitRange must NOT keep the prod defaultRequest cpu=2 (ka-0000006038)")
	}
}

// sliceNetPol returns the YAML document for the NetworkPolicy whose name ends
// with nameSuffix, or "" if not found.
func sliceNetPol(out, nameSuffix string) string {
	for doc := range strings.SplitSeq(out, "\n---\n") {
		if strings.Contains(doc, "kind: NetworkPolicy") &&
			strings.Contains(doc, "name: ") &&
			strings.Contains(doc, nameSuffix+"\n") {
			return doc
		}
	}
	return ""
}

// sliceKind returns the first YAML document of the given kind, or "".
func sliceKind(out, kind string) string {
	for doc := range strings.SplitSeq(out, "\n---\n") {
		if strings.Contains(doc, "kind: "+kind+"\n") {
			return doc
		}
	}
	return ""
}
