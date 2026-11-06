package podmanager

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// Runtime coverage instrumentation for dynamically-built agent pods (C2 of the
// e2e-coverage epic, ka-0000000508). These tests assert the podmanager wires
// GOCOVERDIR + LLVM_PROFILE_FILE (plus the entrypoint gate GARDENIO_AGENT_COVERAGE)
// and the shared coverage PVC mount ONLY when coverage is active, and is a
// full no-op otherwise.

// coverageManager returns a test manager with coverage active (enabled + a
// shared-PVC claim wired), matching the chart-on path.
func coverageManager() *K8sManager {
	m := newTestManager()
	m.Coverage = CoverageConfig{
		Enabled:   true,
		ClaimName: "gardenio-e2e-agents-coverage",
	}
	return m
}

func envMapOf(envVars []corev1.EnvVar) map[string]corev1.EnvVar {
	out := make(map[string]corev1.EnvVar, len(envVars))
	for _, ev := range envVars {
		out[ev.Name] = ev
	}
	return out
}

func TestBuildEnvVars_CoverageEnabled(t *testing.T) {
	m := coverageManager()
	spec := minimalSpec()
	env := envMapOf(m.buildEnvVars(spec))

	// PodName() for minimalSpec (crew-gardenio-dev-test-1) is the per-pod path
	// segment — identical to the pod's metadata.name, so the counters land in a
	// per-pod subtree that concurrent agents never collide on.
	pod := spec.PodName()
	wantGoCoverDir := "/data/coverage/agents/" + pod + "/go"
	wantProfile := "/data/coverage/agents/" + pod + "/coop-%p-%m.profraw"

	checks := map[string]string{
		"GARDENIO_AGENT_COVERAGE": "1",
		"GOCOVERDIR":              wantGoCoverDir,
		"LLVM_PROFILE_FILE":       wantProfile,
	}
	for name, want := range checks {
		ev, ok := env[name]
		if !ok {
			t.Errorf("coverage enabled: missing env var %s", name)
			continue
		}
		if ev.Value != want {
			t.Errorf("coverage env %s = %q, want %q", name, ev.Value, want)
		}
		// Coverage env is composed at build time (no downward-API valueFrom).
		if ev.ValueFrom != nil {
			t.Errorf("coverage env %s should be a plain value, got ValueFrom", name)
		}
	}
}

func TestBuildEnvVars_CoverageDisabled(t *testing.T) {
	// Default manager: coverage inactive (zero-value CoverageConfig).
	m := newTestManager()
	spec := minimalSpec()
	env := envMapOf(m.buildEnvVars(spec))

	for _, name := range []string{"GARDENIO_AGENT_COVERAGE", "GOCOVERDIR", "LLVM_PROFILE_FILE"} {
		if _, ok := env[name]; ok {
			t.Errorf("coverage disabled: env var %s should be absent", name)
		}
	}
}

func TestBuildEnvVars_CoverageEnabledButNoClaimIsNoOp(t *testing.T) {
	// Enabled but no claim → inactive (nowhere for counters to land). Guards a
	// partial env from producing an unmountable pod spec.
	m := newTestManager()
	m.Coverage = CoverageConfig{Enabled: true}
	spec := minimalSpec()
	env := envMapOf(m.buildEnvVars(spec))

	for _, name := range []string{"GARDENIO_AGENT_COVERAGE", "GOCOVERDIR", "LLVM_PROFILE_FILE"} {
		if _, ok := env[name]; ok {
			t.Errorf("coverage enabled w/o claim: env var %s should be absent", name)
		}
	}
	// And no coverage volume/mount either.
	for _, v := range m.buildVolumes(spec) {
		if v.Name == VolumeCoverageData {
			t.Error("coverage enabled w/o claim: coverage volume should be absent")
		}
	}
}

func TestBuildEnvVars_CoverageMountOverride(t *testing.T) {
	m := newTestManager()
	m.Coverage = CoverageConfig{
		Enabled:   true,
		ClaimName: "gardenio-e2e-agents-coverage",
		MountPath: "/mnt/cov",
	}
	spec := minimalSpec()
	env := envMapOf(m.buildEnvVars(spec))

	pod := spec.PodName()
	if got, want := env["GOCOVERDIR"].Value, "/mnt/cov/agents/"+pod+"/go"; got != want {
		t.Errorf("GOCOVERDIR = %q, want %q", got, want)
	}
	if got, want := env["LLVM_PROFILE_FILE"].Value, "/mnt/cov/agents/"+pod+"/coop-%p-%m.profraw"; got != want {
		t.Errorf("LLVM_PROFILE_FILE = %q, want %q", got, want)
	}
}

func TestBuildVolumes_CoverageEnabled(t *testing.T) {
	m := coverageManager()
	spec := minimalSpec()

	var cov *corev1.Volume
	for i, v := range m.buildVolumes(spec) {
		if v.Name == VolumeCoverageData {
			cov = &m.buildVolumes(spec)[i]
			break
		}
	}
	if cov == nil {
		t.Fatal("coverage enabled: coverage-data volume missing")
	}
	if cov.PersistentVolumeClaim == nil {
		t.Fatal("coverage volume should be a PersistentVolumeClaim source")
	}
	if got, want := cov.PersistentVolumeClaim.ClaimName, "gardenio-e2e-agents-coverage"; got != want {
		t.Errorf("coverage claim = %q, want %q", got, want)
	}
}

func TestBuildVolumes_CoverageDisabled(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	for _, v := range m.buildVolumes(spec) {
		if v.Name == VolumeCoverageData {
			t.Error("coverage disabled: coverage-data volume should be absent")
		}
	}
}

func TestBuildVolumeMounts_CoverageEnabled(t *testing.T) {
	m := coverageManager()
	spec := minimalSpec()

	var mount *corev1.VolumeMount
	mounts := m.buildVolumeMounts(spec)
	for i := range mounts {
		if mounts[i].Name == VolumeCoverageData {
			mount = &mounts[i]
			break
		}
	}
	if mount == nil {
		t.Fatal("coverage enabled: coverage-data mount missing")
	}
	if got, want := mount.MountPath, MountCoverageData; got != want {
		t.Errorf("coverage mount path = %q, want %q", got, want)
	}
	if mount.ReadOnly {
		t.Error("coverage mount must be read-write (agents write counters)")
	}
}

func TestBuildVolumeMounts_CoverageDisabled(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	for _, mt := range m.buildVolumeMounts(spec) {
		if mt.Name == VolumeCoverageData {
			t.Error("coverage disabled: coverage-data mount should be absent")
		}
	}
}
