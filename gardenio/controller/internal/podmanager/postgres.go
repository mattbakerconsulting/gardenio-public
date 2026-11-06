package podmanager

import corev1 "k8s.io/api/core/v1"

// buildPostgresSidecar returns a PostgreSQL sidecar using the K8s 1.29+
// native sidecar pattern (initContainer with restartPolicy: Always). The
// sidecar runs postgres:16-alpine with trust authentication on localhost,
// emptyDir storage, and is accessible at 127.0.0.1:5432 from the agent.
func (m *K8sManager) buildPostgresSidecar() corev1.Container {
	restartAlways := corev1.ContainerRestartPolicyAlways
	return corev1.Container{
		Name:  PostgresContainerName,
		Image: PostgresImage,
		Env: []corev1.EnvVar{
			{Name: "POSTGRES_HOST_AUTH_METHOD", Value: "trust"},
			{Name: "PGDATA", Value: MountPostgresData + "/pgdata"},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:    intPtr(999),
			RunAsNonRoot: boolPtr(true),
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: VolumePostgresData, MountPath: MountPostgresData},
		},
		RestartPolicy: &restartAlways,
	}
}
