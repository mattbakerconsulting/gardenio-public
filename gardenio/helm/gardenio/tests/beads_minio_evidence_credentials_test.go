package tests

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// gd-k3GmLJKW8R: the beads daemon's land-door evidence route reads
// <bucket>/<prefix>/images/ (landmap.Pictures). In endpoint mode it gets a
// read-only in-box MinIO user, opt-in through objectStore.beadsCredentials:
// the bootstrap Job creates the user with a GetObject-only policy and the
// beads Deployment reads the Secret as AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY.

const beadsMinioBootstrapTpl = "templates/minio/bootstrap-job.yaml"

var beadsMinioOn = []string{
	"agents.s3.bucket=gardenio-sessions",
	"agents.s3.prefix=coop/sessions",
	"agents.s3.endpoint=http://gardenio-minio:9000",
	"objectStore.enabled=true",
	"objectStore.beadsCredentials.enabled=true",
}

func setFlags(flags []string) []string {
	args := make([]string, 0, 2*len(flags))
	for _, f := range flags {
		args = append(args, "--set", f)
	}
	return args
}

func TestBeadsMinioCredentials_DefaultOff(t *testing.T) {
	t.Parallel()
	flags := []string{
		"agents.s3.bucket=gardenio-sessions",
		"agents.s3.endpoint=http://gardenio-minio:9000",
		"objectStore.enabled=true",
	}
	dep := helmBeadsDeployment(t, setFlags(flags)...)
	for _, off := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "minio-beads-credentials"} {
		if strings.Contains(dep, off) {
			t.Errorf("beads Deployment must not render %s with beadsCredentials off; got:\n%s", off, dep)
		}
	}
	job := helmTemplate(t, beadsMinioBootstrapTpl, flags...)
	for _, off := range []string{"BEADS_ACCESS_KEY", "beads user", "BEADSPOLICY"} {
		if strings.Contains(job, off) {
			t.Errorf("bootstrap Job must not configure the beads user with beadsCredentials off; got:\n%s", job)
		}
	}
}

func TestBeadsMinioCredentials_DeploymentReadsOwnSecret(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t, setFlags(beadsMinioOn)...)
	for _, env := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if got := secretKeyRefName(t, out, env); got != "test-minio-beads-credentials" {
			t.Errorf("%s comes from %q, want the chart-default beads Secret test-minio-beads-credentials", env, got)
		}
	}
	re := regexp.MustCompile(`(?s)name: AWS_ACCESS_KEY_ID\s+valueFrom:\s+secretKeyRef:\s+name: \S+\s+key: accesskey.*?name: AWS_SECRET_ACCESS_KEY\s+valueFrom:\s+secretKeyRef:\s+name: \S+\s+key: secretkey`)
	if !re.MatchString(out) {
		t.Errorf("expected accesskey/secretkey keys on the beads AWS_* secretKeyRefs; got:\n%s", out)
	}
	if strings.Contains(out, "test-minio-credentials") || strings.Contains(out, "test-minio-agent-credentials") {
		t.Errorf("beads Deployment must not reference the root or agent Secret; got:\n%s", out)
	}
	if !strings.Contains(out, "- name: COOP_S3_ENDPOINT") {
		t.Errorf("control: COOP_S3_ENDPOINT missing from the beads Deployment")
	}
}

func TestBeadsMinioCredentials_ExistingSecretOverride(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t, append(setFlags(beadsMinioOn),
		"--set", "objectStore.beadsCredentials.existingSecret=beads-evidence-s3")...)
	if got := secretKeyRefName(t, out, "AWS_ACCESS_KEY_ID"); got != "beads-evidence-s3" {
		t.Errorf("AWS_ACCESS_KEY_ID comes from %q, want beads-evidence-s3", got)
	}
	job := helmTemplate(t, beadsMinioBootstrapTpl, append(beadsMinioOn,
		"objectStore.beadsCredentials.existingSecret=beads-evidence-s3")...)
	if !strings.Contains(job, "configuring beads user from Secret beads-evidence-s3") {
		t.Errorf("bootstrap Job must read the overridden beads Secret; got:\n%s", job)
	}
}

// Endpoint unset is AWS S3: credentials come from IRSA, never a MinIO Secret.
func TestBeadsMinioCredentials_AWSModeRendersNoKeys(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t, setFlags([]string{
		"agents.s3.bucket=gardenio-sessions",
		"objectStore.enabled=true",
		"objectStore.beadsCredentials.enabled=true",
		setIRSA("agents.agentServiceAccount"),
		setIRSA("agents.serviceAccount"),
	})...)
	for _, off := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "COOP_S3_ENDPOINT"} {
		if strings.Contains(out, off) {
			t.Errorf("AWS/IRSA mode must not render %s on the beads Deployment; got:\n%s", off, out)
		}
	}
}

func TestBeadsMinioCredentials_K3sDeployValues(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t, "--values", k3sDeployValues)
	for _, env := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if got := secretKeyRefName(t, out, env); !strings.HasSuffix(got, "-minio-beads-credentials") {
			t.Errorf("k3s: %s comes from %q, want the beads Secret", env, got)
		}
	}
}

func beadsPolicyFromJob(t *testing.T, out string) map[string]any {
	t.Helper()
	m := regexp.MustCompile(`(?s)<<'BEADSPOLICY'\n\s*(\{.*?\})\n\s*BEADSPOLICY\n`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("expected a BEADSPOLICY heredoc in the bootstrap Job; got:\n%s", out)
	}
	var policy map[string]any
	if err := json.Unmarshal([]byte(m[1]), &policy); err != nil {
		t.Fatalf("beads policy is not valid JSON: %v\n%s", err, m[1])
	}
	return policy
}

func TestBeadsMinioCredentials_BootstrapUserAndPolicy(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, beadsMinioBootstrapTpl, beadsMinioOn...)
	for _, want := range []string{
		`mc admin policy create local "gardenio-beads-evidence" /tmp/beads-policy.json`,
		`printf '%s\n' "$BEADS_SECRET_KEY" | mc admin user add local "$BEADS_ACCESS_KEY"`,
		`mc admin policy attach local "gardenio-beads-evidence" --user "$BEADS_ACCESS_KEY"`,
		`if [ -z "${BEADS_ACCESS_KEY:-}" ] || [ -z "${BEADS_SECRET_KEY:-}" ]; then`,
		`if [ "$BEADS_ACCESS_KEY" = "$ACCESS_KEY" ] || [ "$BEADS_ACCESS_KEY" = "${AGENT_ACCESS_KEY:-}" ]; then`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in the bootstrap Job; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"$BEADS_ACCESS_KEY" "$BEADS_SECRET_KEY"`) {
		t.Errorf("the beads secret key must not be passed in mc's argv")
	}
	script := out[strings.Index(out, "set -eu"):]
	if regexp.MustCompile(`\bgrep\b`).MatchString(strings.ReplaceAll(script, "has no grep", "")) {
		t.Errorf("bootstrap script must not call grep (absent from the mc image); got:\n%s", script)
	}
	for _, env := range []string{"BEADS_ACCESS_KEY", "BEADS_SECRET_KEY"} {
		re := regexp.MustCompile(`(?s)- name: ` + env + `\s+valueFrom:\s+secretKeyRef:\s+name: (\S+)\s+key: \S+\s+optional: true`)
		m := re.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("expected %s from an optional secretKeyRef; got:\n%s", env, out)
		}
		if m[1] != "test-minio-beads-credentials" {
			t.Errorf("%s comes from %q, want test-minio-beads-credentials", env, m[1])
		}
	}
	if strings.Index(out, "- name: MC_HOST_local") < strings.Index(out, "- name: SECRET_KEY") {
		t.Errorf("MC_HOST_local must stay after SECRET_KEY")
	}
	// The agent user is still configured, unchanged, before the beads one.
	if strings.Index(out, "configuring agent user") > strings.Index(out, "configuring beads user") {
		t.Errorf("agent user must be configured before the beads user")
	}
	agent := agentPolicyFromJob(t, out)
	if stmts, _ := agent["Statement"].([]any); len(stmts) != 3 {
		t.Errorf("agent policy changed shape next to the beads policy: %v", agent)
	}
}

func TestBeadsMinioCredentials_PolicyIsGetObjectOnImagesOnly(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, beadsMinioBootstrapTpl, append(beadsMinioOn, "agents.s3.prefix=/coop/sessions/")...)
	policy := beadsPolicyFromJob(t, out)
	stmts, _ := policy["Statement"].([]any)
	if len(stmts) != 1 {
		t.Fatalf("beads policy must be exactly one statement, got %d: %v", len(stmts), policy)
	}
	st := stmts[0].(map[string]any)
	if st["Effect"] != "Allow" {
		t.Errorf("unexpected effect %v", st["Effect"])
	}
	actions, _ := st["Action"].([]any)
	if len(actions) != 1 || actions[0] != "s3:GetObject" {
		t.Errorf("beads policy must allow s3:GetObject only, got %v", actions)
	}
	resources, _ := st["Resource"].([]any)
	if len(resources) != 1 || resources[0] != "arn:aws:s3:::gardenio-sessions/coop/sessions/images/*" {
		t.Errorf("beads policy must cover <bucket>/<prefix>/images/* only, got %v", resources)
	}
	if _, hasCond := st["Condition"]; hasCond {
		t.Errorf("beads policy needs no condition (no ListBucket), got %v", st["Condition"])
	}
}

func TestBeadsMinioCredentials_PolicyFollowsBucketPrefixAndName(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, beadsMinioBootstrapTpl,
		"objectStore.enabled=true",
		"objectStore.beadsCredentials.enabled=true",
		"objectStore.beadsCredentials.policyName=custom-beads",
		"objectStore.bucket=other-sessions",
		"agents.s3.prefix=sessions",
	)
	policy, _ := json.Marshal(beadsPolicyFromJob(t, out))
	if !strings.Contains(string(policy), "arn:aws:s3:::other-sessions/sessions/images/*") {
		t.Errorf("expected the beads policy to follow objectStore.bucket and agents.s3.prefix; got %s", policy)
	}
	if !strings.Contains(out, `mc admin policy attach local "custom-beads"`) {
		t.Errorf("expected the configured policy name; got:\n%s", out)
	}
}

func TestBeadsMinioCredentials_RootOrAgentSecretFailsRender(t *testing.T) {
	t.Parallel()
	for name, flags := range map[string][]string{
		"root default name":  {"objectStore.beadsCredentials.existingSecret=test-minio-credentials"},
		"agent default name": {"objectStore.beadsCredentials.existingSecret=test-minio-agent-credentials"},
		"custom root name":   {"objectStore.existingSecret=my-root", "objectStore.beadsCredentials.existingSecret=my-root"},
		"custom agent name":  {"objectStore.agentCredentials.existingSecret=my-agent", "objectStore.beadsCredentials.existingSecret=my-agent"},
	} {
		out, err := helmTemplateAllowFail(t, "", append(append([]string{}, beadsMinioOn...), flags...)...)
		if err == nil {
			t.Errorf("%s: expected the render to fail when the beads Secret is the root or agent Secret", name)
			continue
		}
		if !strings.Contains(out, "gd-k3GmLJKW8R") {
			t.Errorf("%s: expected the beads-credentials guard message; got:\n%s", name, out)
		}
	}
}
