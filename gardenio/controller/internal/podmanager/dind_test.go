package podmanager

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestBuildPod_DockerDisabled(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.Docker = false
	pod := m.buildPod(spec)

	for _, ic := range pod.Spec.InitContainers {
		if ic.Name == DindContainerName {
			t.Fatal("DinD sidecar should not be present when Docker is disabled")
		}
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == VolumeDindStorage {
			t.Fatal("dind-storage volume should not be present when Docker is disabled")
		}
	}
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "DOCKER_HOST" {
			t.Fatal("DOCKER_HOST should not be set when Docker is disabled")
		}
	}
}

func TestBuildPod_DockerEnabled(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.Docker = true
	pod := m.buildPod(spec)

	// Verify DinD sidecar is present as a native sidecar (initContainer).
	var dind *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == DindContainerName {
			dind = &pod.Spec.InitContainers[i]
			break
		}
	}
	if dind == nil {
		t.Fatal("DinD sidecar not found in initContainers")
	}

	// Verify image.
	if dind.Image != DindImage {
		t.Errorf("DinD image = %q, want %q", dind.Image, DindImage)
	}

	// Verify restartPolicy: Always (K8s 1.29+ native sidecar pattern).
	if dind.RestartPolicy == nil || *dind.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("DinD sidecar should have restartPolicy: Always")
	}

	// Verify privileged mode.
	if dind.SecurityContext == nil || dind.SecurityContext.Privileged == nil || !*dind.SecurityContext.Privileged {
		t.Error("DinD sidecar should run in privileged mode")
	}

	// Verify RunAsUser: 0 (override pod-level runAsUser: 1000).
	if dind.SecurityContext.RunAsUser == nil || *dind.SecurityContext.RunAsUser != 0 {
		t.Error("DinD sidecar should run as root (RunAsUser: 0)")
	}

	// Verify RunAsNonRoot: false (override pod-level runAsNonRoot: true).
	if dind.SecurityContext.RunAsNonRoot == nil || *dind.SecurityContext.RunAsNonRoot {
		t.Error("DinD sidecar should have RunAsNonRoot: false")
	}

	// Verify DOCKER_TLS_CERTDIR="" env var.
	foundTLSEnv := false
	for _, env := range dind.Env {
		if env.Name == "DOCKER_TLS_CERTDIR" && env.Value == "" {
			foundTLSEnv = true
		}
	}
	if !foundTLSEnv {
		t.Error("DinD sidecar should have DOCKER_TLS_CERTDIR=\"\" env var")
	}

	// Verify dind-storage volume mount.
	foundMount := false
	for _, vm := range dind.VolumeMounts {
		if vm.Name == VolumeDindStorage && vm.MountPath == MountDindStorage {
			foundMount = true
		}
	}
	if !foundMount {
		t.Errorf("DinD sidecar should mount %s at %s", VolumeDindStorage, MountDindStorage)
	}

	// Verify dind-storage volume exists on the pod.
	foundVolume := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == VolumeDindStorage && v.VolumeSource.EmptyDir != nil {
			foundVolume = true
		}
	}
	if !foundVolume {
		t.Error("Pod should have dind-storage emptyDir volume")
	}

	// Verify DOCKER_HOST is set on the agent container.
	foundDockerHost := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "DOCKER_HOST" && env.Value == "tcp://127.0.0.1:2375" {
			foundDockerHost = true
		}
	}
	if !foundDockerHost {
		t.Error("Agent container should have DOCKER_HOST=tcp://127.0.0.1:2375")
	}
}

// ── DinD preflight (PSA admission) ─────────────────────────────────────────

func TestCheckDindAdmittable(t *testing.T) {
	cases := []struct {
		name       string
		nsLabel    string // value for pod-security.kubernetes.io/enforce; "" = label absent
		nsMissing  bool
		wantErr    bool
		wantSubstr string
	}{
		{name: "privileged_ok", nsLabel: "privileged"},
		{name: "no_label_ok", nsLabel: ""},
		{name: "baseline_rejected", nsLabel: "baseline", wantErr: true, wantSubstr: "baseline"},
		{name: "restricted_rejected", nsLabel: "restricted", wantErr: true, wantSubstr: "restricted"},
		{name: "unknown_value_allowed", nsLabel: "wat"},
		{name: "namespace_missing", nsMissing: true, wantErr: true, wantSubstr: "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewClientset()
			if !tc.nsMissing {
				labels := map[string]string{}
				if tc.nsLabel != "" {
					labels[PSAEnforceLabel] = tc.nsLabel
				}
				_, err := client.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: labels},
				}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("seed namespace: %v", err)
				}
			}
			m := New(client, slog.Default())
			err := m.checkDindAdmittable(context.Background(), "default")
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
					t.Errorf("error %q does not contain %q", err, tc.wantSubstr)
				}
				if tc.nsLabel != "" && !strings.Contains(err.Error(), "GARDENIO_PSA_PROFILE") {
					t.Errorf("PSA-rejection error should mention GARDENIO_PSA_PROFILE; got: %v", err)
				}
			} else if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

func TestCreateAgentPod_DockerInBaselineNamespace(t *testing.T) {
	client := fake.NewClientset()
	_, err := client.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "default",
			Labels: map[string]string{PSAEnforceLabel: "baseline"},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	m := New(client, slog.Default())
	spec := minimalSpec()
	spec.Docker = true
	err = m.CreateAgentPod(context.Background(), spec)
	if err == nil {
		t.Fatal("expected CreateAgentPod to fail preflight in baseline namespace")
	}
	if !strings.Contains(err.Error(), "dind preflight") {
		t.Errorf("error should be dind preflight; got: %v", err)
	}
	// Pod must NOT have been created.
	if _, err := client.CoreV1().Pods("default").Get(context.Background(), spec.PodName(), metav1.GetOptions{}); err == nil {
		t.Error("pod should not have been created when preflight fails")
	}
}
