package podmanager

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestBuildPod_PostgresDisabled(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.Postgres = false
	pod := m.buildPod(spec)

	for _, ic := range pod.Spec.InitContainers {
		if ic.Name == PostgresContainerName {
			t.Fatal("Postgres sidecar should not be present when Postgres is disabled")
		}
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == VolumePostgresData {
			t.Fatal("postgres-data volume should not be present when Postgres is disabled")
		}
	}
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "PGHOST" {
			t.Fatal("PGHOST should not be set when Postgres is disabled")
		}
	}
}

func TestBuildPod_PostgresEnabled(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.Postgres = true
	pod := m.buildPod(spec)

	// Verify Postgres sidecar is present as a native sidecar (initContainer).
	var pg *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == PostgresContainerName {
			pg = &pod.Spec.InitContainers[i]
			break
		}
	}
	if pg == nil {
		t.Fatal("Postgres sidecar not found in initContainers")
	}

	// Verify image.
	if pg.Image != PostgresImage {
		t.Errorf("Postgres image = %q, want %q", pg.Image, PostgresImage)
	}

	// Verify restartPolicy: Always (K8s 1.29+ native sidecar pattern).
	if pg.RestartPolicy == nil || *pg.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("Postgres sidecar should have restartPolicy: Always")
	}

	// Verify RunAsUser: 999 (standard postgres container UID).
	if pg.SecurityContext == nil || pg.SecurityContext.RunAsUser == nil || *pg.SecurityContext.RunAsUser != 999 {
		t.Error("Postgres sidecar should run as UID 999")
	}

	// Verify RunAsNonRoot: true.
	if pg.SecurityContext.RunAsNonRoot == nil || !*pg.SecurityContext.RunAsNonRoot {
		t.Error("Postgres sidecar should have RunAsNonRoot: true")
	}

	// Verify POSTGRES_HOST_AUTH_METHOD=trust env var.
	foundAuthEnv := false
	for _, env := range pg.Env {
		if env.Name == "POSTGRES_HOST_AUTH_METHOD" && env.Value == "trust" {
			foundAuthEnv = true
		}
	}
	if !foundAuthEnv {
		t.Error("Postgres sidecar should have POSTGRES_HOST_AUTH_METHOD=trust env var")
	}

	// Verify PGDATA env var.
	foundPGData := false
	for _, env := range pg.Env {
		if env.Name == "PGDATA" && env.Value == MountPostgresData+"/pgdata" {
			foundPGData = true
		}
	}
	if !foundPGData {
		t.Errorf("Postgres sidecar should have PGDATA=%s/pgdata env var", MountPostgresData)
	}

	// Verify postgres-data volume mount.
	foundMount := false
	for _, vm := range pg.VolumeMounts {
		if vm.Name == VolumePostgresData && vm.MountPath == MountPostgresData {
			foundMount = true
		}
	}
	if !foundMount {
		t.Errorf("Postgres sidecar should mount %s at %s", VolumePostgresData, MountPostgresData)
	}

	// Verify postgres-data volume exists on the pod.
	foundVolume := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == VolumePostgresData && v.VolumeSource.EmptyDir != nil {
			foundVolume = true
		}
	}
	if !foundVolume {
		t.Error("Pod should have postgres-data emptyDir volume")
	}

	// Verify PGHOST is set on the agent container.
	foundPGHost := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "PGHOST" && env.Value == "127.0.0.1" {
			foundPGHost = true
		}
	}
	if !foundPGHost {
		t.Error("Agent container should have PGHOST=127.0.0.1")
	}

	// Verify PGUSER is set on the agent container.
	foundPGUser := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "PGUSER" && env.Value == PostgresUser {
			foundPGUser = true
		}
	}
	if !foundPGUser {
		t.Errorf("Agent container should have PGUSER=%s", PostgresUser)
	}

	// Verify PGPORT is set on the agent container.
	foundPGPort := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "PGPORT" && env.Value == "5432" {
			foundPGPort = true
		}
	}
	if !foundPGPort {
		t.Error("Agent container should have PGPORT=5432")
	}

	// Verify PGDATABASE is set on the agent container.
	foundPGDB := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "PGDATABASE" && env.Value == PostgresDB {
			foundPGDB = true
		}
	}
	if !foundPGDB {
		t.Errorf("Agent container should have PGDATABASE=%s", PostgresDB)
	}
}
