package podmanager

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestBuildPod_BothSidecars verifies that enabling Docker and Postgres
// simultaneously produces a pod with both sidecars and no conflicts
// (no duplicate volume names, env vars, or init containers).
func TestBuildPod_BothSidecars(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.Docker = true
	spec.Postgres = true
	pod := m.buildPod(spec)

	// ── Both sidecars present as native sidecars (initContainers) ──
	var dind, pg *corev1.Container
	for i := range pod.Spec.InitContainers {
		switch pod.Spec.InitContainers[i].Name {
		case DindContainerName:
			dind = &pod.Spec.InitContainers[i]
		case PostgresContainerName:
			pg = &pod.Spec.InitContainers[i]
		}
	}
	if dind == nil {
		t.Fatal("DinD sidecar not found in initContainers")
	}
	if pg == nil {
		t.Fatal("Postgres sidecar not found in initContainers")
	}

	// ── Images correct ──
	if dind.Image != DindImage {
		t.Errorf("DinD image = %q, want %q", dind.Image, DindImage)
	}
	if pg.Image != PostgresImage {
		t.Errorf("Postgres image = %q, want %q", pg.Image, PostgresImage)
	}

	// ── Both have restartPolicy: Always (native sidecar pattern) ──
	if dind.RestartPolicy == nil || *dind.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("DinD sidecar should have restartPolicy: Always")
	}
	if pg.RestartPolicy == nil || *pg.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("Postgres sidecar should have restartPolicy: Always")
	}

	// ── Both volumes present with distinct names ──
	volumeNames := make(map[string]bool)
	for _, v := range pod.Spec.Volumes {
		if volumeNames[v.Name] {
			t.Errorf("duplicate volume name: %s", v.Name)
		}
		volumeNames[v.Name] = true
	}
	if !volumeNames[VolumeDindStorage] {
		t.Error("missing dind-storage volume")
	}
	if !volumeNames[VolumePostgresData] {
		t.Error("missing postgres-data volume")
	}

	// ── Agent container has both DOCKER_HOST and PG* env vars ──
	agent := pod.Spec.Containers[0]
	envMap := make(map[string]string)
	for _, env := range agent.Env {
		envMap[env.Name] = env.Value
	}
	if envMap["DOCKER_HOST"] != "tcp://127.0.0.1:2375" {
		t.Errorf("DOCKER_HOST = %q, want tcp://127.0.0.1:2375", envMap["DOCKER_HOST"])
	}
	if envMap["PGHOST"] != "127.0.0.1" {
		t.Errorf("PGHOST = %q, want 127.0.0.1", envMap["PGHOST"])
	}
	if envMap["PGPORT"] != "5432" {
		t.Errorf("PGPORT = %q, want 5432", envMap["PGPORT"])
	}
	if envMap["PGUSER"] != PostgresUser {
		t.Errorf("PGUSER = %q, want %s", envMap["PGUSER"], PostgresUser)
	}
	if envMap["PGDATABASE"] != PostgresDB {
		t.Errorf("PGDATABASE = %q, want %s", envMap["PGDATABASE"], PostgresDB)
	}

	// ── No duplicate env var names on agent container ──
	envCount := make(map[string]int)
	for _, env := range agent.Env {
		envCount[env.Name]++
		if envCount[env.Name] > 1 {
			t.Errorf("duplicate env var on agent container: %s", env.Name)
		}
	}
}

// TestBuildPod_BothSidecars_SecurityContextIsolation verifies that
// each sidecar has its own security context and they don't interfere
// with each other or the pod-level context.
func TestBuildPod_BothSidecars_SecurityContextIsolation(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.Docker = true
	spec.Postgres = true
	pod := m.buildPod(spec)

	var dind, pg *corev1.Container
	for i := range pod.Spec.InitContainers {
		switch pod.Spec.InitContainers[i].Name {
		case DindContainerName:
			dind = &pod.Spec.InitContainers[i]
		case PostgresContainerName:
			pg = &pod.Spec.InitContainers[i]
		}
	}

	// DinD: privileged, root
	if !*dind.SecurityContext.Privileged {
		t.Error("DinD must run privileged")
	}
	if *dind.SecurityContext.RunAsUser != 0 {
		t.Errorf("DinD RunAsUser = %d, want 0", *dind.SecurityContext.RunAsUser)
	}

	// Postgres: non-privileged, UID 999
	if pg.SecurityContext.Privileged != nil && *pg.SecurityContext.Privileged {
		t.Error("Postgres must not run privileged")
	}
	if *pg.SecurityContext.RunAsUser != 999 {
		t.Errorf("Postgres RunAsUser = %d, want 999", *pg.SecurityContext.RunAsUser)
	}
	if !*pg.SecurityContext.RunAsNonRoot {
		t.Error("Postgres must have RunAsNonRoot: true")
	}

	// Pod-level: UID 1000 (agent)
	if *pod.Spec.SecurityContext.RunAsUser != AgentUID {
		t.Errorf("Pod RunAsUser = %d, want %d", *pod.Spec.SecurityContext.RunAsUser, AgentUID)
	}
}

// TestBuildPod_BothSidecars_VolumeIsolation verifies that DinD and
// Postgres sidecars mount their own volumes and don't cross-mount.
func TestBuildPod_BothSidecars_VolumeIsolation(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.Docker = true
	spec.Postgres = true
	pod := m.buildPod(spec)

	var dind, pg *corev1.Container
	for i := range pod.Spec.InitContainers {
		switch pod.Spec.InitContainers[i].Name {
		case DindContainerName:
			dind = &pod.Spec.InitContainers[i]
		case PostgresContainerName:
			pg = &pod.Spec.InitContainers[i]
		}
	}

	// DinD mounts dind-storage, not postgres-data
	for _, vm := range dind.VolumeMounts {
		if vm.Name == VolumePostgresData {
			t.Error("DinD should not mount postgres-data")
		}
	}
	hasDindMount := false
	for _, vm := range dind.VolumeMounts {
		if vm.Name == VolumeDindStorage && vm.MountPath == MountDindStorage {
			hasDindMount = true
		}
	}
	if !hasDindMount {
		t.Error("DinD missing dind-storage mount")
	}

	// Postgres mounts postgres-data, not dind-storage
	for _, vm := range pg.VolumeMounts {
		if vm.Name == VolumeDindStorage {
			t.Error("Postgres should not mount dind-storage")
		}
	}
	hasPGMount := false
	for _, vm := range pg.VolumeMounts {
		if vm.Name == VolumePostgresData && vm.MountPath == MountPostgresData {
			hasPGMount = true
		}
	}
	if !hasPGMount {
		t.Error("Postgres missing postgres-data mount")
	}
}
