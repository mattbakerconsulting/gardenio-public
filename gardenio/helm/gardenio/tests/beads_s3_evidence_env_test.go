package tests

import (
	"strings"
	"testing"
)

// The land door's decision evidence route (gd-U0WmpYaenI, kbeads
// landmap.PicturesFromEnv) reads COOP_S3_* on the beads daemon. The chart
// renders them from agents.s3 only when a bucket is set, so a chart without
// one keeps the route answering 503.

func TestBeadsS3EvidenceEnv_FromAgentsS3(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t,
		"--set", "agents.s3.bucket=gardenio-sessions",
		"--set", "agents.s3.prefix=coop/sessions",
		"--set", "agents.s3.region=us-east-1",
		"--set", "agents.s3.endpoint=http://gardenio-minio:9000",
	)
	for _, want := range []string{
		"- name: COOP_S3_BUCKET\n              value: \"gardenio-sessions\"",
		"- name: COOP_S3_PREFIX\n              value: \"coop/sessions\"",
		"- name: COOP_S3_REGION\n              value: \"us-east-1\"",
		"- name: COOP_S3_ENDPOINT\n              value: \"http://gardenio-minio:9000\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in beads deployment, got:\n%s", want, out)
		}
	}
}

func TestBeadsS3EvidenceEnv_K3sDeployValues(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t, "--values", k3sDeployValues)
	for _, want := range []string{"COOP_S3_BUCKET", "COOP_S3_PREFIX", "COOP_S3_REGION", "COOP_S3_ENDPOINT"} {
		if !strings.Contains(out, "- name: "+want) {
			t.Errorf("expected %s in the k3s beads deployment render", want)
		}
	}
}

// The env carries no credentials by default: in-box read credentials are
// opt-in through objectStore.beadsCredentials (gd-k3GmLJKW8R,
// beads_minio_evidence_credentials_test.go).
func TestBeadsS3EvidenceEnv_NoCredentials(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t,
		"--set", "agents.s3.bucket=gardenio-sessions",
		"--set", "agents.s3.endpoint=http://gardenio-minio:9000",
		"--set", "objectStore.enabled=true",
	)
	for _, off := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "COOP_S3_CREDENTIALS_SECRET"} {
		if strings.Contains(out, off) {
			t.Errorf("beads deployment must not render %s unless objectStore.beadsCredentials.enabled (gd-k3GmLJKW8R)", off)
		}
	}
}

func TestBeadsS3EvidenceEnv_AbsentWithoutBucket(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t,
		"--set", "agents.s3.prefix=coop/sessions",
		"--set", "agents.s3.region=us-east-1",
		"--set", "agents.s3.endpoint=http://gardenio-minio:9000",
	)
	if !strings.Contains(out, "BEADS_HTTP_ADDR") {
		t.Fatalf("control: beads Deployment did not render the daemon env:\n%s", out)
	}
	if strings.Contains(out, "COOP_S3_") {
		t.Errorf("COOP_S3_* must be absent when agents.s3.bucket is unset, got:\n%s", out)
	}
}
