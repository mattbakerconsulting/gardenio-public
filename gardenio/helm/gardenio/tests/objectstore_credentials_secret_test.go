package tests

import (
	"regexp"
	"strings"
	"testing"
)

// COOP_S3_CREDENTIALS_SECRET wiring (gd-W8OwRoHLLn): the controller reads
// only the object-store credentials Secret NAME from its own env (never a
// value) and podspec.go (podmanager.ObjectStoreCredentialsSecret) projects
// that Secret onto each agent pod as a tmpfs-backed volume, mirroring the
// NATS token pattern. This is endpoint-mode-only chart plumbing: AWS/IRSA
// deployments (agents.s3.endpoint unset) must leave the controller's own env
// byte-for-byte unaffected — no COOP_S3_CREDENTIALS_SECRET, no
// AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY — since IRSA supplies credentials
// instead.

const objectStoreCredsTpl = "templates/controller/deployment.yaml"

// secretNameFromMinioSecretVar pulls the secret name off a rendered
// `valueFrom.secretKeyRef.name: <name>` block immediately following a given
// env var name, so the test does not need to reimplement the
// gardenio.minio.secretName helper's release-name templating.
func secretKeyRefName(t *testing.T, out, envName string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)name: ` + regexp.QuoteMeta(envName) + `\s+valueFrom:\s+secretKeyRef:\s+name: (\S+)`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("could not find secretKeyRef.name following env %q in:\n%s", envName, out)
	}
	return m[1]
}

// TestObjectStoreCredentialsSecret_EndpointAndObjectStoreEnabled_Renders
// verifies that when both an in-box S3 endpoint and the in-box MinIO
// objectStore are enabled, the controller deployment carries
// COOP_S3_CREDENTIALS_SECRET set to the least-privilege agent Secret, NOT the
// root Secret the controller's own AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY
// come from (gd-urSiLLwPIz): agent pods mount that Secret, and the root keys
// can read, overwrite and delete every bucket.
func TestObjectStoreCredentialsSecret_EndpointAndObjectStoreEnabled_Renders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, objectStoreCredsTpl,
		"agents.enabled=true",
		"agents.s3.bucket=gardenio-sessions",
		"agents.s3.endpoint=http://minio.gardenio.svc.cluster.local:9000",
		"objectStore.enabled=true",
		"objectStore.rootPassword=devsecret",
	)

	accessKeySecret := secretKeyRefName(t, out, "AWS_ACCESS_KEY_ID")
	secretKeySecret := secretKeyRefName(t, out, "AWS_SECRET_ACCESS_KEY")
	if accessKeySecret != secretKeySecret {
		t.Fatalf("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must reference the same Secret; got %q vs %q",
			accessKeySecret, secretKeySecret)
	}

	got := coopS3CredentialsSecret(t, out)
	if got == accessKeySecret {
		t.Errorf("COOP_S3_CREDENTIALS_SECRET = %q is the MinIO root Secret; agent pods must get the agent user", got)
	}
	if got != "test-minio-agent-credentials" {
		t.Errorf("COOP_S3_CREDENTIALS_SECRET = %q, want the chart-default agent Secret %q", got, "test-minio-agent-credentials")
	}
	// The controller refuses any Secret name containing this marker (the
	// root Secret) as an agent-pod mount, so the agent default must not.
	if strings.Contains(strings.ToLower(got), "minio-credentials") {
		t.Errorf("agent Secret %q contains the controller's forbidden marker minio-credentials", got)
	}
}

func coopS3CredentialsSecret(t *testing.T, out string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)name: COOP_S3_CREDENTIALS_SECRET\s+value: (\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("expected COOP_S3_CREDENTIALS_SECRET to render; got:\n%s", out)
	}
	return m[1]
}

// TestObjectStoreCredentialsSecret_AgentExistingSecretOverride: an
// operator-named agent Secret flows through to the controller.
func TestObjectStoreCredentialsSecret_AgentExistingSecretOverride(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, objectStoreCredsTpl,
		"agents.enabled=true",
		"agents.s3.bucket=gardenio-sessions",
		"agents.s3.endpoint=http://minio.gardenio.svc.cluster.local:9000",
		"objectStore.enabled=true",
		"objectStore.agentCredentials.existingSecret=coop-agent-s3",
	)
	if got := coopS3CredentialsSecret(t, out); got != "coop-agent-s3" {
		t.Errorf("COOP_S3_CREDENTIALS_SECRET = %q, want %q", got, "coop-agent-s3")
	}
}

// TestObjectStoreCredentialsSecret_RootSecretAsAgentFailsRender pins the
// chart guard (validate-minio-agent-credentials.yaml): pointing the agent
// Secret at the root Secret, by the default name or a custom existingSecret,
// fails the render instead of handing agent pods the root keys.
func TestObjectStoreCredentialsSecret_RootSecretAsAgentFailsRender(t *testing.T) {
	t.Parallel()
	for name, flags := range map[string][]string{
		"default root name": {"objectStore.agentCredentials.existingSecret=test-minio-credentials"},
		"custom root name":  {"objectStore.existingSecret=my-root", "objectStore.agentCredentials.existingSecret=my-root"},
	} {
		args := append([]string{
			"agents.enabled=true",
			"agents.s3.bucket=gardenio-sessions",
			"agents.s3.endpoint=http://minio.gardenio.svc.cluster.local:9000",
			"objectStore.enabled=true",
		}, flags...)
		out, err := helmTemplateAllowFail(t, "", args...)
		if err == nil {
			t.Errorf("%s: expected the render to fail when the agent Secret is the root Secret", name)
			continue
		}
		if !strings.Contains(out, "gd-urSiLLwPIz") {
			t.Errorf("%s: expected the agent-credentials guard message; got:\n%s", name, out)
		}
	}
}

// TestObjectStoreCredentialsSecret_EndpointSetObjectStoreDisabled_DoesNotRender
// covers a BYO-endpoint deployment: agents.s3.endpoint points at an
// externally-managed S3-compatible store, but the chart's own in-box MinIO
// (objectStore.enabled) is off. Without objectStore.enabled, the chart has no
// Secret to name, so COOP_S3_CREDENTIALS_SECRET must not render — an operator
// wiring an external endpoint is expected to configure agent credentials
// through a different path, not this chart-owned MinIO secret.
func TestObjectStoreCredentialsSecret_EndpointSetObjectStoreDisabled_DoesNotRender(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, objectStoreCredsTpl,
		"agents.enabled=true",
		"agents.s3.bucket=gardenio-sessions",
		"agents.s3.endpoint=http://external-minio.example.com:9000",
	)
	for _, unwanted := range []string{"COOP_S3_CREDENTIALS_SECRET", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("objectStore.enabled=false must not render %s; got:\n%s", unwanted, out)
		}
	}
}

// TestObjectStoreCredentialsSecret_AWSMode_RegressionUnchanged is the
// regression the acceptance criteria call out explicitly: AWS/IRSA mode
// (agents.s3.endpoint unset) must render none of this feature's output, even
// with objectStore.enabled=true (a nonsensical but possible combination) —
// the credential-secret wiring is strictly gated on endpoint mode.
func TestObjectStoreCredentialsSecret_AWSMode_RegressionUnchanged(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, objectStoreCredsTpl,
		"agents.enabled=true",
		"agents.s3.bucket=gardenio-sessions",
		"objectStore.enabled=true",
		"objectStore.rootPassword=devsecret",
		// validate-irsa.yaml requires these annotations whenever
		// agents.s3.bucket is set and agents.s3.endpoint is not — a real
		// AWS/IRSA deployment.
		setIRSA("agents.agentServiceAccount"),
		setIRSA("agents.serviceAccount"),
	)
	for _, unwanted := range []string{"COOP_S3_CREDENTIALS_SECRET", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "COOP_S3_ENDPOINT"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("AWS/IRSA mode (endpoint unset) must not render %s; got:\n%s", unwanted, out)
		}
	}
}
