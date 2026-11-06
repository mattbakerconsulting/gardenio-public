package tests

import (
	"strings"
	"testing"
)

// global.imageTag is the chart-wide image pin for gardenio-owned images,
// resolved by the gardenio.imageTag helper as:
//
//	per-component <c>.image.tag > global.imageTag > Chart.AppVersion
//
// (latestMode still overrides everything with "latest").
//
// Regression: an EKS-era terraform overlay emitted a top-level
// `image.tag` that NO chart template read, so image_tag_override was a
// structural no-op — P4.3 deployed gardenio_image_tag=2026.176.6 but every
// gardenio-owned pod ran on Chart.AppVersion=2026.171.8. (ka-0000003658)

func TestGlobalImageTag_DefaultFallsBackToAppVersion(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml")
	if strings.Contains(out, "image: \"registry.gitlab.com/acme/platform/gardenio/kbeads:\"") {
		t.Fatalf("beads image tag should never render empty; got:\n%s", out)
	}
	// AppVersion changes every release, so assert shape rather than the exact
	// calver: the kbeads image must carry a non-empty tag and NOT "latest".
	if strings.Contains(out, "gardenio/kbeads:latest") {
		t.Errorf("beads image should default to Chart.AppVersion, not latest; got:\n%s", out)
	}
}

func TestGlobalImageTag_PinsGardenioOwnedImages(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"global.imageTag=2026.176.6",
	)
	if !strings.Contains(out, "gardenio/kbeads:2026.176.6") {
		t.Errorf("beads image should use global.imageTag when per-component tag is empty; got:\n%s", out)
	}
}

func TestGlobalImageTag_AppliedToControllerAndAgentImage(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"global.imageTag=2026.176.6",
	)
	if !strings.Contains(out, "gardenio/controller:2026.176.6") {
		t.Errorf("controller image should use global.imageTag; got:\n%s", out)
	}
	// GARDENIO_AGENT_IMAGE (agents.agentImage) resolves through the same
	// helper, so the spawned agent pods are pinned too.
	if !strings.Contains(out, "gardenio/agent:2026.176.6") {
		t.Errorf("agent image env should use global.imageTag; got:\n%s", out)
	}
}

func TestGlobalImageTag_PerComponentTagWins(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"global.imageTag=2026.176.6",
		"beads.image.tag=1.2.3",
	)
	if !strings.Contains(out, "gardenio/kbeads:1.2.3") {
		t.Errorf("per-component image.tag should override global.imageTag; got:\n%s", out)
	}
}

func TestGlobalImageTag_LatestModeStillWins(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"global.imageTag=2026.176.6",
		"global.latestMode=true",
	)
	if !strings.Contains(out, "gardenio/kbeads:latest") {
		t.Errorf("latestMode should override global.imageTag; got:\n%s", out)
	}
}
