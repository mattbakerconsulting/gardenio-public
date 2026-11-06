package tests

import (
	"os/exec"
	"strings"
	"testing"
)

// helmTemplateAllowFail is a variant of helmTemplate that returns the combined
// output and error instead of calling t.Fatalf, so tests can assert on
// expected-failure cases (e.g. chart-side {{ fail }} guards).
func helmTemplateAllowFail(t *testing.T, showOnly string, setFlags ...string) (string, error) {
	t.Helper()
	args := []string{"template", "test", chartDir(t)}
	if showOnly != "" {
		args = append(args, "--show-only", showOnly)
	}
	for _, s := range setFlags {
		args = append(args, "--set", s)
	}
	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// IRSA guard: when agents.s3.bucket is set, every ServiceAccount the chart
// creates for the controller / agent pods / cluster-admin path must carry
// the eks.amazonaws.com/role-arn annotation. Regression guard for
// ka-0000005411 + ka-0000007426.

const irsaRoleARN = `arn:aws:iam::123456789012:role/gardenio-sessions`

func setIRSA(path string) string {
	return path + `.annotations.eks\.amazonaws\.com/role-arn=` + irsaRoleARN
}

func TestS3IRSA_BucketUnset_Passes(t *testing.T) {
	t.Parallel()
	if _, err := helmTemplateAllowFail(t, "",
		"agents.enabled=true",
	); err != nil {
		t.Fatalf("expected helm template to succeed when agents.s3.bucket is unset; err: %v", err)
	}
}

func TestS3IRSA_BucketSet_AllAnnotated_Passes(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "",
		"agents.enabled=true",
		"agents.s3.bucket=gardenio-sessions",
		setIRSA("agents.agentServiceAccount"),
		setIRSA("agents.serviceAccount"),
		"agents.clusterAdmin.enabled=true",
		setIRSA("agents.clusterAdmin"),
	)
	if err != nil {
		t.Fatalf("expected helm template to succeed when bucket and IRSA annotation are both set; err: %v\n%s", err, out)
	}
}

func TestS3IRSA_BucketSet_AgentServiceAccountMissing_Fails(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "",
		"agents.enabled=true",
		"agents.s3.bucket=gardenio-sessions",
		// agentServiceAccount.create defaults to true; annotation deliberately omitted.
		setIRSA("agents.serviceAccount"),
	)
	if err == nil {
		t.Fatalf("expected helm template to fail when agents.s3.bucket is set without IRSA on agentServiceAccount; got success:\n%s", out)
	}
	if !strings.Contains(out, "agents.agentServiceAccount.annotations") {
		t.Errorf("error should reference agents.agentServiceAccount.annotations; got:\n%s", out)
	}
	if !strings.Contains(out, "eks.amazonaws.com/role-arn") {
		t.Errorf("error should mention the IRSA annotation key; got:\n%s", out)
	}
}

func TestS3IRSA_BucketSet_ControllerServiceAccountMissing_Fails(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "",
		"agents.enabled=true",
		"agents.s3.bucket=gardenio-sessions",
		setIRSA("agents.agentServiceAccount"),
		// serviceAccount.create defaults to true; annotation deliberately omitted.
	)
	if err == nil {
		t.Fatalf("expected helm template to fail when agents.s3.bucket is set without IRSA on the controller serviceAccount; got success:\n%s", out)
	}
	if !strings.Contains(out, "agents.serviceAccount.annotations") {
		t.Errorf("error should reference agents.serviceAccount.annotations; got:\n%s", out)
	}
	if !strings.Contains(out, "eks.amazonaws.com/role-arn") {
		t.Errorf("error should mention the IRSA annotation key; got:\n%s", out)
	}
}

func TestS3IRSA_BucketSet_ClusterAdminMissing_Fails(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "",
		"agents.enabled=true",
		"agents.s3.bucket=gardenio-sessions",
		setIRSA("agents.agentServiceAccount"),
		setIRSA("agents.serviceAccount"),
		"agents.clusterAdmin.enabled=true",
		// clusterAdmin annotation deliberately omitted.
	)
	if err == nil {
		t.Fatalf("expected helm template to fail when agents.s3.bucket is set with clusterAdmin enabled but no IRSA annotation; got success:\n%s", out)
	}
	if !strings.Contains(out, "agents.clusterAdmin.annotations") {
		t.Errorf("error should reference agents.clusterAdmin.annotations; got:\n%s", out)
	}
	if !strings.Contains(out, "eks.amazonaws.com/role-arn") {
		t.Errorf("error should mention the IRSA annotation key; got:\n%s", out)
	}
}

func TestS3IRSA_BucketSet_ClusterAdminDisabled_NoCheck(t *testing.T) {
	t.Parallel()
	// clusterAdmin defaults to disabled; the guard should not require an
	// annotation on a SA the chart isn't creating.
	if _, err := helmTemplateAllowFail(t, "",
		"agents.enabled=true",
		"agents.s3.bucket=gardenio-sessions",
		setIRSA("agents.agentServiceAccount"),
		setIRSA("agents.serviceAccount"),
	); err != nil {
		t.Fatalf("expected helm template to succeed when clusterAdmin is disabled; err: %v", err)
	}
}

func TestS3IRSA_AgentsDisabled_NoCheck(t *testing.T) {
	t.Parallel()
	// When agents.enabled=false, no agent SAs are created and the guard
	// should be a no-op even if a bucket is set.
	if _, err := helmTemplateAllowFail(t, "",
		"agents.enabled=false",
		"agents.s3.bucket=gardenio-sessions",
	); err != nil {
		t.Fatalf("expected helm template to succeed when agents.enabled=false; err: %v", err)
	}
}

func TestS3IRSA_InBoxEndpointSet_SkipsCheck(t *testing.T) {
	t.Parallel()
	// In-box object store (ka-0000004955): when agents.s3.endpoint is set the
	// store is an in-cluster S3-compatible server whose creds come from a k8s
	// secret (AWS_ACCESS_KEY_ID/SECRET), not IRSA. The guard must NOT require
	// an IRSA annotation in that mode, even with a bucket and no annotations.
	if _, err := helmTemplateAllowFail(t, "",
		"agents.enabled=true",
		"agents.s3.bucket=gardenio-sessions",
		"agents.s3.endpoint=http://minio.gardenio.svc.cluster.local:9000",
	); err != nil {
		t.Fatalf("expected helm template to succeed when an in-box S3 endpoint is set without IRSA; err: %v", err)
	}
}
