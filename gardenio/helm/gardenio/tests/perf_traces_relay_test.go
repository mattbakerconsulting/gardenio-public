package tests

import (
	"os/exec"
	"strings"
	"testing"
)

// perf-traces relay (PULL transport, epic ka-0000000598, task ka-0000004875).
// A tiny in-chart poller (the controller image's `perf-traces-relay`
// subcommand) reads perf's public authed VictoriaTraces Jaeger query API and
// OTLP-replays spans into the in-chart vtsingle. These tests pin both the
// opt-in render AND the default-off invariant, plus the perf-traces-ingest
// ExternalSecret gating.

// --- default-off invariant ---

func TestRelay_DefaultOff_NoDeployment(t *testing.T) {
	t.Parallel()
	out, err := exec.Command("helm", "template", "test", chartDir(t),
		"--set", "vtsingle.enabled=true",
		"--show-only", "templates/vtsingle/relay-deployment.yaml",
	).CombinedOutput()
	if err == nil {
		t.Fatalf("relay Deployment must NOT render when relay.enabled is false; got:\n%s", out)
	}
}

func TestRelay_DefaultOff_AbsentFromFullRender(t *testing.T) {
	t.Parallel()
	out, err := exec.Command("helm", "template", "test", chartDir(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "component: perf-traces-relay") {
		t.Error("no relay resources should render when relay.enabled is false")
	}
}

// --- Deployment shape ---

func TestRelay_Deployment_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vtsingle/relay-deployment.yaml",
		"vtsingle.enabled=true",
		"vtsingle.relay.enabled=true",
		"agents.image.tag=v1.2.3",
	)
	for _, want := range []string{
		"kind: Deployment",
		"name: test-perf-traces-relay",
		`args: ["perf-traces-relay"]`,
		// reuses the controller image (no new image)
		"gardenio/controller:v1.2.3",
		"name: PERF_TRACES_SOURCE_URL",
		"https://perf.example.com/victoria-traces",
		// dest defaults to this release's in-chart VT service
		"http://test-victoria-traces:10428",
		"value: /etc/perf-traces/username",
		"value: /etc/perf-traces/password",
		"secretName: gardenio-perf-traces",
		"readOnlyRootFilesystem: true",
		"runAsNonRoot: true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in relay Deployment; got:\n%s", want, out)
		}
	}
}

func TestRelay_DestURL_Overridable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vtsingle/relay-deployment.yaml",
		"vtsingle.enabled=true",
		"vtsingle.relay.enabled=true",
		"vtsingle.relay.destURL=http://external-vt:10428",
	)
	if !strings.Contains(out, "http://external-vt:10428") {
		t.Errorf("expected overridden destURL; got:\n%s", out)
	}
}

func TestRelay_Services_Joined(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vtsingle/relay-deployment.yaml",
		"vtsingle.enabled=true",
		"vtsingle.relay.enabled=true",
		"vtsingle.relay.services={keycloak,site-api,sponsor-api}",
	)
	if !strings.Contains(out, "value: \"keycloak,site-api,sponsor-api\"") {
		t.Errorf("expected comma-joined services env; got:\n%s", out)
	}
}

func TestRelay_GatedOnVtsingleEnabled(t *testing.T) {
	t.Parallel()
	// relay.enabled alone (vtsingle.enabled false) must not render — the relay
	// has no destination without the in-chart VT.
	out, err := exec.Command("helm", "template", "test", chartDir(t),
		"--set", "vtsingle.relay.enabled=true",
		"--show-only", "templates/vtsingle/relay-deployment.yaml",
	).CombinedOutput()
	if err == nil {
		t.Fatalf("relay must be gated on vtsingle.enabled; got:\n%s", out)
	}
}

// --- perf-traces-ingest ExternalSecret ---

func TestRelay_ExternalSecret_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=test-store",
		"vtsingle.enabled=true",
		"vtsingle.relay.enabled=true",
		"externalSecrets.perfTracesIngest.remoteRef=sm/perf-traces",
	)
	for _, want := range []string{
		"kind: ExternalSecret",
		"name: gardenio-perf-traces",
		`key: "sm/perf-traces"`,
		`property: "username"`,
		`property: "password"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in perf-traces ExternalSecret; got render missing it", want)
		}
	}
}

func TestRelay_ExternalSecret_GatedOnRemoteRef(t *testing.T) {
	t.Parallel()
	// No remoteRef → operator provisions the Secret out-of-band; the
	// ExternalSecret must not render even though the relay Deployment (which
	// references the Secret by name) does.
	out := helmTemplate(t, "",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=test-store",
		"vtsingle.enabled=true",
		"vtsingle.relay.enabled=true",
	)
	if strings.Contains(out, "Perf VictoriaTraces basic-auth credentials") {
		t.Error("perf-traces ExternalSecret must NOT render without a remoteRef")
	}
}
