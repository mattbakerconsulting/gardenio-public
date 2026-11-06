package tests

import (
	"os/exec"
	"strings"
	"testing"
)

// dev-traces relay (PULL transport, task ka-0000003142). A second relay
// instance — identical in mechanism to the perf-traces relay but sourcing the
// dev stack (https://dev.acme.example/victoria-traces) — OTLP-replays ALL dev spans
// into the same in-chart vtsingle. These tests pin the opt-in render, the
// default-off invariant, and the dev-traces-ingest ExternalSecret gating, so
// the chart stays byte-identical for envs that do not enable it and the live
// perf relay is unaffected.

// --- default-off invariant ---

func TestDevRelay_DefaultOff_NoDeployment(t *testing.T) {
	t.Parallel()
	out, err := exec.Command("helm", "template", "test", chartDir(t),
		"--set", "vtsingle.enabled=true",
		"--show-only", "templates/vtsingle/dev-relay-deployment.yaml",
	).CombinedOutput()
	if err == nil {
		t.Fatalf("dev relay Deployment must NOT render when devRelay.enabled is false; got:\n%s", out)
	}
}

func TestDevRelay_DefaultOff_AbsentFromFullRender(t *testing.T) {
	t.Parallel()
	out, err := exec.Command("helm", "template", "test", chartDir(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "component: dev-traces-relay") {
		t.Error("no dev relay resources should render when devRelay.enabled is false")
	}
}

// --- Deployment shape ---

func TestDevRelay_Deployment_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vtsingle/dev-relay-deployment.yaml",
		"vtsingle.enabled=true",
		"vtsingle.devRelay.enabled=true",
		"agents.image.tag=v1.2.3",
	)
	for _, want := range []string{
		"kind: Deployment",
		"name: test-dev-traces-relay",
		`args: ["perf-traces-relay"]`,
		// reuses the controller image (no new image)
		"gardenio/controller:v1.2.3",
		"name: PERF_TRACES_SOURCE_URL",
		"https://dev.example.com/victoria-traces",
		// dest defaults to this release's in-chart VT service
		"http://test-victoria-traces:10428",
		"value: /etc/dev-traces/username",
		"value: /etc/dev-traces/password",
		"secretName: gardenio-dev-traces",
		"readOnlyRootFilesystem: true",
		"runAsNonRoot: true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in dev relay Deployment; got:\n%s", want, out)
		}
	}
}

func TestDevRelay_DestURL_Overridable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vtsingle/dev-relay-deployment.yaml",
		"vtsingle.enabled=true",
		"vtsingle.devRelay.enabled=true",
		"vtsingle.devRelay.destURL=http://external-vt:10428",
	)
	if !strings.Contains(out, "http://external-vt:10428") {
		t.Errorf("expected overridden destURL; got:\n%s", out)
	}
}

func TestDevRelay_Services_Joined(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vtsingle/dev-relay-deployment.yaml",
		"vtsingle.enabled=true",
		"vtsingle.devRelay.enabled=true",
		"vtsingle.devRelay.services={sponsor-api,site-api,study-web}",
	)
	if !strings.Contains(out, "value: \"sponsor-api,site-api,study-web\"") {
		t.Errorf("expected comma-joined services env; got:\n%s", out)
	}
}

func TestDevRelay_GatedOnVtsingleEnabled(t *testing.T) {
	t.Parallel()
	// devRelay.enabled alone (vtsingle.enabled false) must not render — the
	// relay has no destination without the in-chart VT.
	out, err := exec.Command("helm", "template", "test", chartDir(t),
		"--set", "vtsingle.devRelay.enabled=true",
		"--show-only", "templates/vtsingle/dev-relay-deployment.yaml",
	).CombinedOutput()
	if err == nil {
		t.Fatalf("dev relay must be gated on vtsingle.enabled; got:\n%s", out)
	}
}

// The dev relay is independent of the perf relay: enabling one must not render
// the other. Guards against a copy-paste that crosses the two gates.
func TestDevRelay_IndependentOfPerfRelay(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "",
		"vtsingle.enabled=true",
		"vtsingle.devRelay.enabled=true",
	)
	if !strings.Contains(out, "component: dev-traces-relay") {
		t.Error("dev relay should render when devRelay.enabled is true")
	}
	if strings.Contains(out, "component: perf-traces-relay") {
		t.Error("perf relay must NOT render when only devRelay.enabled is set")
	}
}

// --- dev-traces-ingest ExternalSecret ---

func TestDevRelay_ExternalSecret_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=test-store",
		"vtsingle.enabled=true",
		"vtsingle.devRelay.enabled=true",
		"externalSecrets.devTracesIngest.remoteRef=sm/dev-traces",
	)
	for _, want := range []string{
		"kind: ExternalSecret",
		"name: gardenio-dev-traces",
		`key: "sm/dev-traces"`,
		`property: "username"`,
		`property: "password"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in dev-traces ExternalSecret; got render missing it", want)
		}
	}
}

func TestDevRelay_ExternalSecret_GatedOnRemoteRef(t *testing.T) {
	t.Parallel()
	// No remoteRef → operator provisions the Secret out-of-band; the
	// ExternalSecret must not render even though the relay Deployment (which
	// references the Secret by name) does.
	out := helmTemplate(t, "",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=test-store",
		"vtsingle.enabled=true",
		"vtsingle.devRelay.enabled=true",
	)
	if strings.Contains(out, "Dev VictoriaTraces basic-auth credentials") {
		t.Error("dev-traces ExternalSecret must NOT render without a remoteRef")
	}
}

// --- dev-vm-federation ExternalSecret ---

func TestDevVmFederation_ExternalSecret_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=test-store",
		"vmagent.enabled=true",
		"externalSecrets.devVmFederation.remoteRef=sm/dev-vm",
	)
	for _, want := range []string{
		"name: gardenio-dev-vm-federation",
		`key: "sm/dev-vm"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in dev-vm-federation ExternalSecret; got render missing it", want)
		}
	}
}

func TestDevVmFederation_ExternalSecret_GatedOnRemoteRef(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=test-store",
		"vmagent.enabled=true",
	)
	if strings.Contains(out, "name: gardenio-dev-vm-federation") {
		t.Error("dev-vm-federation ExternalSecret must NOT render without a remoteRef")
	}
}
