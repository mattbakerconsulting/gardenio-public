package tests

import (
	"strings"
	"testing"
)

// P3.5 host-layer hardening, chart half (ka-0000006978; exposure-hardening F13
// ka-0000003100). The gvisor-probe is node-root (privileged + hostPath / +
// hostPID) and pulls an external image, so a mutable tag is the highest-value
// supply-chain target on the box. gvisor.probe.digest pins it by sha256.

func probeArgs(extra ...string) []string {
	base := []string{
		"gvisor.enabled=true",
		"gvisor.namespace.name=gardenio-gvisor",
		"gvisor.probe.create=true",
	}
	return append(base, extra...)
}

// 1. No digest: the probe image is the bare tag (back-compat).
func TestGvisorProbe_NoDigestUsesTag(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/gvisor-namespace.yaml", probeArgs()...)
	if !strings.Contains(out, `image: "public.ecr.aws/docker/library/ubuntu:22.04"`) {
		t.Errorf("probe without digest should use the tag image, got:\n%s", out)
	}
	if strings.Contains(out, "@sha256:") {
		t.Error("probe without digest must NOT render an @sha256 reference")
	}
}

// 2. digest set: the probe image is pinned image@sha256:…
func TestGvisorProbe_DigestPins(t *testing.T) {
	t.Parallel()
	digest := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	out := helmTemplate(t, "templates/gvisor-namespace.yaml",
		probeArgs("gvisor.probe.digest="+digest)...)
	want := "public.ecr.aws/docker/library/ubuntu:22.04@" + digest
	if !strings.Contains(out, want) {
		t.Errorf("probe with digest should render %q, got:\n%s", want, out)
	}
}
