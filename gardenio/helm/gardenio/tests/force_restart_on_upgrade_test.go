package tests

import (
	"regexp"
	"strings"
	"testing"
)

// gd-sqfOLZE7i7: the controller and coopmux Deployments stamped
// rollout.kubernetes.io/restartedAt with {{ now }} unconditionally, so every
// `helm upgrade` — for any reason, even a change to an unrelated component's
// image tag — rolled both pods. coopmux keeps agent sessions in memory, so
// this disturbed every live crew on every upgrade. The stamp is now gated
// behind forceRestartOnUpgrade (default false); ordinary config/image
// changes still roll the pod via normal Deployment spec diffing, since
// almost nothing either template renders is fetched at runtime from a
// ConfigMap/Secret — the secretKeyRef-sourced exceptions (nats-infra-creds
// on both controller and coopmux, plus minio-credentials on the controller)
// get their own checksum/ annotations instead (see
// TestControllerChecksumAnnotations_ChangeWithSecretContent).

func TestForceRestartOnUpgrade_DefaultOmitsRestartedAtStamp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		template string
		setFlags []string
	}{
		{"controller", "templates/controller/deployment.yaml", []string{"agents.enabled=true"}},
		{"coopmux", "templates/coopmux/deployment.yaml", []string{"coopmux.enabled=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := helmTemplate(t, tc.template, tc.setFlags...)
			if strings.Contains(out, "rollout.kubernetes.io/restartedAt") {
				t.Errorf("default render of %s must not carry rollout.kubernetes.io/restartedAt — it forces a restart on every helm upgrade regardless of what changed; got:\n%s", tc.template, out)
			}
		})
	}
}

// checksumAnnotation extracts a checksum/<name> annotation value from
// rendered YAML, e.g. checksumAnnotation(out, "nats-infra-creds").
func checksumAnnotation(t *testing.T, out, name string) string {
	t.Helper()
	re := regexp.MustCompile(`checksum/` + regexp.QuoteMeta(name) + `: (\S+)`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("checksum/%s annotation not found in rendered deployment:\n%s", name, out)
	}
	return m[1]
}

// gd-sqfOLZE7i7 (coordinator send-back, PR #68): the controller consumes the
// nats-infra-creds and minio-credentials Secrets via secretKeyRef, so their
// content isn't part of the pod spec the native Deployment diff already
// rolls on. Now that the restartedAt stamp is opt-in, a values-only rotation
// of either Secret's inline (non-ESO) content must still roll the controller
// pod — that's what these checksum/ annotations are for.
func TestControllerChecksumAnnotations_ChangeWithSecretContent(t *testing.T) {
	t.Parallel()
	natsA := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true", "nats.enabled=true", "nats.auth.enabled=true",
		"nats.auth.subjectIsolation.enabled=true", "nats.auth.subjectIsolation.infraToken=tokenA")
	natsB := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true", "nats.enabled=true", "nats.auth.enabled=true",
		"nats.auth.subjectIsolation.enabled=true", "nats.auth.subjectIsolation.infraToken=tokenB")
	if a, b := checksumAnnotation(t, natsA, "nats-infra-creds"), checksumAnnotation(t, natsB, "nats-infra-creds"); a == b {
		t.Errorf("checksum/nats-infra-creds must change when the rendered infra token changes; got the same value %q for tokenA and tokenB", a)
	}

	minioA := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true", "objectStore.enabled=true", "objectStore.rootPassword=passA")
	minioB := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true", "objectStore.enabled=true", "objectStore.rootPassword=passB")
	if a, b := checksumAnnotation(t, minioA, "minio-credentials"), checksumAnnotation(t, minioB, "minio-credentials"); a == b {
		t.Errorf("checksum/minio-credentials must change when the rendered root password changes; got the same value %q for passA and passB", a)
	}

	// coopmux reads COOP_MUX_NATS_TOKEN/COOP_MUX_NATS_RELAY_TOKEN from the
	// same nats-infra-creds Secret when subjectIsolation is enabled, so a
	// rotated infraToken must still roll coopmux even with the stamp gated.
	coopA := helmTemplate(t, "templates/coopmux/deployment.yaml",
		"coopmux.enabled=true", "nats.enabled=true", "nats.auth.enabled=true",
		"nats.auth.subjectIsolation.enabled=true", "nats.auth.subjectIsolation.infraToken=tokenA")
	coopB := helmTemplate(t, "templates/coopmux/deployment.yaml",
		"coopmux.enabled=true", "nats.enabled=true", "nats.auth.enabled=true",
		"nats.auth.subjectIsolation.enabled=true", "nats.auth.subjectIsolation.infraToken=tokenB")
	if a, b := checksumAnnotation(t, coopA, "nats-infra-creds"), checksumAnnotation(t, coopB, "nats-infra-creds"); a == b {
		t.Errorf("coopmux checksum/nats-infra-creds must change when the rendered infra token changes; got the same value %q for tokenA and tokenB", a)
	}

	// coopmux never reads minio-credentials, so it gets no checksum for that
	// one even with subjectIsolation on.
	if strings.Contains(coopA, "checksum/minio-credentials") {
		t.Errorf("coopmux deployment should not carry checksum/minio-credentials; got:\n%s", coopA)
	}
}

func TestForceRestartOnUpgrade_OptInEmitsRestartedAtStamp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		template string
		setFlags []string
	}{
		{"controller", "templates/controller/deployment.yaml", []string{"agents.enabled=true", "agents.forceRestartOnUpgrade=true"}},
		{"coopmux", "templates/coopmux/deployment.yaml", []string{"coopmux.enabled=true", "coopmux.forceRestartOnUpgrade=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := helmTemplate(t, tc.template, tc.setFlags...)
			if !strings.Contains(out, "rollout.kubernetes.io/restartedAt") {
				t.Errorf("forceRestartOnUpgrade=true must still emit rollout.kubernetes.io/restartedAt for %s; got:\n%s", tc.template, out)
			}
		})
	}
}
