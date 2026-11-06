package podmanager

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// findInitContainer returns the named init container, or nil.
func findInitContainer(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	return nil
}

// Defect A (bug ka-0000001058): a runsc pod must carry the thaw-state-perms
// root init container that chowns the thaw-state hostPath to the agent UID, so
// the in-pod gb+coop watchers can write their armed sentinels at boot.
func TestBuildPod_ThawStatePermsInit_Runsc(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.RuntimeClassName = RuntimeClassRunsc
	pod := m.buildPod(spec)

	ic := findInitContainer(pod, ThawStatePermsInitName)
	if ic == nil {
		t.Fatalf("runsc pod missing %q init container", ThawStatePermsInitName)
	}
	// Must run as root (uid 0) to chown a root-owned hostPath; admissible
	// because runsc pods land in the PSA-privileged gVisor namespace.
	if ic.SecurityContext == nil || ic.SecurityContext.RunAsUser == nil || *ic.SecurityContext.RunAsUser != 0 {
		t.Errorf("thaw-state-perms init must RunAsUser=0, got %+v", ic.SecurityContext)
	}
	if ic.SecurityContext.RunAsNonRoot == nil || *ic.SecurityContext.RunAsNonRoot {
		t.Errorf("thaw-state-perms init must set RunAsNonRoot=false")
	}
	// Must mount the thaw-state volume at the canonical mount path.
	var mounted bool
	for _, vm := range ic.VolumeMounts {
		if vm.Name == VolumeThawState && vm.MountPath == MountThawState {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("thaw-state-perms init must mount %q at %q, got %+v",
			VolumeThawState, MountThawState, ic.VolumeMounts)
	}
	// The command must chown to the agent UID/GID on the mount path.
	cmd := ""
	if len(ic.Command) > 0 {
		cmd = ic.Command[len(ic.Command)-1]
	}
	if want := "1000:1000"; !strings.Contains(cmd, want) || !strings.Contains(cmd, MountThawState) {
		t.Errorf("thaw-state-perms command = %q, want chown %s on %s", cmd, want, MountThawState)
	}
	// It must run before init-clone so the dir is writable when needed.
	idxPerms, idxClone := -1, -1
	for i, c := range pod.Spec.InitContainers {
		switch c.Name {
		case ThawStatePermsInitName:
			idxPerms = i
		case InitCloneName:
			idxClone = i
		}
	}
	if idxClone != -1 && idxPerms != -1 && idxPerms > idxClone {
		t.Errorf("thaw-state-perms (idx %d) must precede init-clone (idx %d)", idxPerms, idxClone)
	}
}

// A non-runsc pod never carries the thaw-state volume, so it must NOT get the
// perms init container.
func TestBuildPod_ThawStatePermsInit_NonRunscAbsent(t *testing.T) {
	m := newTestManager()
	pod := m.buildPod(minimalSpec()) // no RuntimeClassName
	if ic := findInitContainer(pod, ThawStatePermsInitName); ic != nil {
		t.Errorf("non-runsc pod must NOT carry %q init container", ThawStatePermsInitName)
	}
}

// Defect B (bug ka-0000001058): when DNSSearches is set, buildPod attaches a
// pod DNSConfig with those searches and keeps DNSPolicy ClusterFirst.
func TestBuildPod_DNSSearches(t *testing.T) {
	m := newTestManager()

	// Default: no DNSSearches → no DNSConfig override.
	pod := m.buildPod(minimalSpec())
	if pod.Spec.DNSConfig != nil {
		t.Errorf("default pod must not set DNSConfig, got %+v", pod.Spec.DNSConfig)
	}

	spec := minimalSpec()
	spec.DNSSearches = []string{"gardenio.svc.cluster.local"}
	pod = m.buildPod(spec)
	if pod.Spec.DNSConfig == nil || len(pod.Spec.DNSConfig.Searches) != 1 ||
		pod.Spec.DNSConfig.Searches[0] != "gardenio.svc.cluster.local" {
		t.Fatalf("DNSConfig.Searches = %+v, want [gardenio.svc.cluster.local]", pod.Spec.DNSConfig)
	}
	if pod.Spec.DNSPolicy != corev1.DNSClusterFirst {
		t.Errorf("DNSPolicy = %q, want ClusterFirst", pod.Spec.DNSPolicy)
	}
}
