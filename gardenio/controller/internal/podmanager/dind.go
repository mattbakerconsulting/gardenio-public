package podmanager

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PSAEnforceLabel is the Pod Security Admission "enforce" label key
// stamped on namespaces. Values: "privileged", "baseline", "restricted".
const PSAEnforceLabel = "pod-security.kubernetes.io/enforce"

// buildDindSidecar returns a Docker-in-Docker sidecar using the K8s 1.29+
// native sidecar pattern (initContainer with restartPolicy: Always). The
// sidecar runs docker:dind in privileged mode with TLS disabled, exposing
// the Docker daemon on tcp://127.0.0.1:2375 for the agent container.
func (m *K8sManager) buildDindSidecar() corev1.Container {
	restartAlways := corev1.ContainerRestartPolicyAlways
	return corev1.Container{
		Name:  DindContainerName,
		Image: m.dindImage(),
		Env: []corev1.EnvVar{
			{Name: "DOCKER_TLS_CERTDIR", Value: ""},
		},
		SecurityContext: &corev1.SecurityContext{
			Privileged:   boolPtr(true),
			RunAsUser:    intPtr(0),
			RunAsNonRoot: boolPtr(false),
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: VolumeDindStorage, MountPath: MountDindStorage},
		},
		RestartPolicy: &restartAlways,
	}
}

// checkDindAdmittable verifies the target namespace permits the privileged
// DinD sidecar before pod creation. The sidecar sets privileged=true /
// runAsUser=0 / runAsNonRoot=false, which the PSA "baseline" and
// "restricted" profiles reject. Returns an actionable error pointing at
// docs/runbooks/dind-namespace-requirements.md when the namespace would
// reject admission, so the failure surfaces with a clear message rather
// than a cryptic admission-webhook denial after Pod create.
//
// If the namespace is missing the enforce label, admission is permitted
// (PSA defaults to no enforcement) — we do not invent a denial the
// cluster would not produce.
func (m *K8sManager) checkDindAdmittable(ctx context.Context, namespace string) error {
	ns, err := m.client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("dind preflight: namespace %q not found", namespace)
		}
		// Don't block pod creation on a transient namespace-get failure.
		// A real PSA denial will surface from the Pod create call below
		// with the cluster's own error message.
		m.logger.Warn("dind preflight: namespace lookup failed, skipping PSA check",
			"namespace", namespace, "error", err)
		return nil
	}
	enforce := ns.Labels[PSAEnforceLabel]
	switch enforce {
	case "", "privileged":
		return nil
	case "baseline", "restricted":
		return fmt.Errorf(
			"dind preflight: namespace %q has %s=%q which rejects the privileged DinD sidecar; "+
				"set GARDENIO_PSA_PROFILE=privileged on the controller before creating the fleet "+
				"namespace, or relabel the namespace with %s=privileged. "+
				"See docs/runbooks/dind-namespace-requirements.md for the opt-in path",
			namespace, PSAEnforceLabel, enforce, PSAEnforceLabel,
		)
	default:
		// Unknown profile value — treat as permissive but log so an
		// operator notices the typo.
		m.logger.Warn("dind preflight: unknown PSA enforce value, allowing pod create",
			"namespace", namespace, "enforce", enforce)
		return nil
	}
}
