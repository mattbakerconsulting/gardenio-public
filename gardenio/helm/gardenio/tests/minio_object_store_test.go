package tests

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// Regression guards for ka-0000004955 / gd-FFGI1V8pRg — the in-box object store
// (MinIO) chart templates under templates/minio/:
//   - deployment.yaml: single-node single-drive MinIO server
//   - service.yaml:    ClusterIP S3 endpoint on :9000
//   - pvc.yaml:        local-path data PVC
//   - secret.yaml:     root creds (inline dev/test mode only)
//   - bootstrap-job.yaml: mc mb + mc ilm bucket bootstrap (Helm hook)
//
// All are OFF by default (objectStore.enabled=false). The in-box overlay
// (values-inbox.yaml) flips them on. The license decision (ka-0000004293)
// chose MinIO; these guards keep the gating + key shapes intact so a future
// store swap touches only the server template.

func TestObjectStore_DefaultDisabled(t *testing.T) {
	t.Parallel()
	for _, tmpl := range []string{
		"templates/minio/deployment.yaml",
		"templates/minio/service.yaml",
		"templates/minio/pvc.yaml",
		"templates/minio/secret.yaml",
		"templates/minio/bootstrap-job.yaml",
	} {
		out, err := helmTemplateAllowFail(t, tmpl)
		rendersNothing(t, out, err)
	}
}

func TestObjectStore_Deployment_SingleNodeSingleDrive(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/minio/deployment.yaml",
		"objectStore.enabled=true",
	)
	if !strings.Contains(out, "kind: Deployment") {
		t.Fatalf("expected a Deployment; got:\n%s", out)
	}
	// Recreate strategy: the RWO PVC can attach to only one pod; a rolling
	// update would deadlock on the volume.
	if !strings.Contains(out, "type: Recreate") {
		t.Errorf("expected Recreate strategy for the RWO PVC; got:\n%s", out)
	}
	// SNSD: `server /data`.
	if !strings.Contains(out, "/data") {
		t.Errorf("expected MINIO_VOLUMES=/data (server /data); got:\n%s", out)
	}
	if !strings.Contains(out, "name: test-minio") {
		t.Errorf("expected the test-minio name; got:\n%s", out)
	}
}

func TestObjectStore_Deployment_PSARestricted(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/minio/deployment.yaml",
		"objectStore.enabled=true",
	)
	for _, want := range []string{
		"runAsNonRoot: true",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		"drop:",
		"ALL",
		"type: RuntimeDefault",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected PSA-restricted securityContext to contain %q; got:\n%s", want, out)
		}
	}
}

func TestObjectStore_Deployment_CredsFromSecret(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/minio/deployment.yaml",
		"objectStore.enabled=true",
	)
	for _, want := range []string{"MINIO_ROOT_USER", "MINIO_ROOT_PASSWORD", "name: test-minio-credentials"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q wired from the credentials Secret; got:\n%s", want, out)
		}
	}
}

func TestObjectStore_Service_ClusterIPOn9000(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/minio/service.yaml",
		"objectStore.enabled=true",
	)
	if !strings.Contains(out, "type: ClusterIP") {
		t.Errorf("expected ClusterIP (never edge-exposed); got:\n%s", out)
	}
	if !strings.Contains(out, "port: 9000") {
		t.Errorf("expected the S3 port 9000; got:\n%s", out)
	}
}

func TestObjectStore_PVC_LocalPathFallback(t *testing.T) {
	t.Parallel()
	// With objectStore.persistence.storageClass empty but global.storageClass
	// set (the in-box default), the PVC inherits local-path.
	out := helmTemplate(t, "templates/minio/pvc.yaml",
		"objectStore.enabled=true",
		"global.storageClass=local-path",
	)
	if !strings.Contains(out, "storageClassName: local-path") {
		t.Errorf("expected the PVC to inherit global.storageClass=local-path; got:\n%s", out)
	}
	if !strings.Contains(out, "ReadWriteOnce") {
		t.Errorf("expected RWO (single-node single-drive); got:\n%s", out)
	}
}

func TestObjectStore_Secret_NotRenderedInOperatorSeededMode(t *testing.T) {
	t.Parallel()
	// RECOMMENDED mode: no rootPassword, no existingSecret -> the chart renders
	// NO Secret (deploy-byo.sh seeds the default-named Secret instead).
	out, err := helmTemplateAllowFail(t, "templates/minio/secret.yaml",
		"objectStore.enabled=true",
	)
	rendersNothing(t, out, err)
}

func TestObjectStore_Secret_RenderedInInlineMode(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/minio/secret.yaml",
		"objectStore.enabled=true",
		"objectStore.rootPassword=devsecret",
	)
	if !strings.Contains(out, "kind: Secret") {
		t.Fatalf("expected an inline Secret when rootPassword is set; got:\n%s", out)
	}
	if !strings.Contains(out, "secretkey:") || !strings.Contains(out, "accesskey:") {
		t.Errorf("expected accesskey/secretkey keys; got:\n%s", out)
	}
}

func TestObjectStore_Bootstrap_BucketAndILM(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/minio/bootstrap-job.yaml",
		"objectStore.enabled=true",
	)
	if !strings.Contains(out, "kind: Job") {
		t.Fatalf("expected a bootstrap Job; got:\n%s", out)
	}
	// Helm post-install/post-upgrade hook so it fires after the server exists.
	if !strings.Contains(out, "post-install,post-upgrade") {
		t.Errorf("expected a post-install/post-upgrade hook; got:\n%s", out)
	}
	if !strings.Contains(out, "mc mb --ignore-existing") {
		t.Errorf("expected idempotent bucket creation; got:\n%s", out)
	}
	if !strings.Contains(out, "mc ilm rule add") {
		t.Errorf("expected an ILM expiry rule; got:\n%s", out)
	}
}

func TestObjectStore_Bootstrap_ILMDisabledWhenZeroDays(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/minio/bootstrap-job.yaml",
		"objectStore.enabled=true",
		"objectStore.ilmExpiryDays=0",
	)
	if strings.Contains(out, "mc ilm rule add") {
		t.Errorf("expected NO ILM rule when ilmExpiryDays<=0; got:\n%s", out)
	}
	// Bucket creation must still happen.
	if !strings.Contains(out, "mc mb --ignore-existing") {
		t.Errorf("expected bucket creation even with ILM off; got:\n%s", out)
	}
}

func TestObjectStore_Bootstrap_GatedSeparately(t *testing.T) {
	t.Parallel()
	// objectStore on but bootstrap.enabled=false -> server renders, Job does not.
	out, err := helmTemplateAllowFail(t, "templates/minio/bootstrap-job.yaml",
		"objectStore.enabled=true",
		"objectStore.bootstrap.enabled=false",
	)
	rendersNothing(t, out, err)
}

func TestObjectStore_Bootstrap_MCHostAfterCreds(t *testing.T) {
	t.Parallel()
	// Kubernetes expands $(VAR) only from env entries defined EARLIER in the
	// list. With MC_HOST_local first, mc received the literal "$(ACCESS_KEY)"
	// and every bucket call failed with "Access Key Id ... does not exist".
	out := helmTemplate(t, "templates/minio/bootstrap-job.yaml",
		"objectStore.enabled=true",
	)
	host := strings.Index(out, "- name: MC_HOST_local")
	access := strings.Index(out, "- name: ACCESS_KEY")
	secret := strings.Index(out, "- name: SECRET_KEY")
	if host < 0 || access < 0 || secret < 0 {
		t.Fatalf("expected MC_HOST_local, ACCESS_KEY and SECRET_KEY env entries; got:\n%s", out)
	}
	if host < access || host < secret {
		t.Errorf("MC_HOST_local must be defined after ACCESS_KEY and SECRET_KEY; got:\n%s", out)
	}
}

func TestObjectStore_Bootstrap_PodNotSelectedByService(t *testing.T) {
	t.Parallel()
	// The gardenio-minio Service selects component: minio. The bootstrap pod
	// must not carry that label, or S3 traffic is routed to it while it runs.
	out := helmTemplate(t, "templates/minio/bootstrap-job.yaml",
		"objectStore.enabled=true",
	)
	// Only the pod template matters: the Job's own metadata labels are not
	// Service-selectable.
	i := strings.Index(out, "  template:")
	if i < 0 {
		t.Fatalf("expected a pod template; got:\n%s", out)
	}
	pod := out[i:]
	if strings.Contains(pod, "app.kubernetes.io/component: minio\n") {
		t.Errorf("bootstrap Job pod must not carry the minio Service selector label; got:\n%s", pod)
	}
	if !strings.Contains(pod, "app.kubernetes.io/component: minio-bootstrap") {
		t.Errorf("expected component: minio-bootstrap on the bootstrap pod; got:\n%s", out)
	}
}

func TestObjectStore_Bootstrap_BucketQuotaSet(t *testing.T) {
	t.Parallel()
	// local-path does not enforce the PVC size; the quota is the only hard cap
	// on the node disk MinIO shares with postgres.
	out := helmTemplate(t, "templates/minio/bootstrap-job.yaml",
		"objectStore.enabled=true",
		"objectStore.bucketQuota=4GiB",
	)
	if !strings.Contains(out, `mc quota set "local/gardenio-sessions" --size "4GiB"`) {
		t.Errorf("expected mc quota set with the configured size; got:\n%s", out)
	}
	if strings.Contains(out, "mc quota clear") {
		t.Errorf("expected no quota clear when a quota is set; got:\n%s", out)
	}
}

func TestObjectStore_Bootstrap_BucketQuotaClearedWhenEmpty(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/minio/bootstrap-job.yaml",
		"objectStore.enabled=true",
	)
	if strings.Contains(out, "mc quota set") {
		t.Errorf("expected no quota set by default; got:\n%s", out)
	}
	if !strings.Contains(out, `mc quota clear "local/gardenio-sessions"`) {
		t.Errorf("expected the quota cleared when bucketQuota is empty; got:\n%s", out)
	}
}

// gd-3Vgzf25MZY: Commons attachments get their own bucket with no ILM expiry
// and their own quota; unset, the Job and the viewer render exactly as before.
func TestObjectStore_Bootstrap_CommonsFilesBucket(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/minio/bootstrap-job.yaml",
		"objectStore.enabled=true",
		"objectStore.ilmExpiryDays=14",
		"objectStore.commonsFiles.bucket=gardenio-commons-files",
		"objectStore.commonsFiles.quota=5GiB",
	)
	if !strings.Contains(out, `mc mb --ignore-existing "local/gardenio-commons-files"`) {
		t.Errorf("expected the attachments bucket to be created; got:\n%s", out)
	}
	if !strings.Contains(out, `mc ilm rule remove --all --force "local/gardenio-commons-files"`) {
		t.Errorf("expected the attachments bucket to carry no ILM rule; got:\n%s", out)
	}
	if strings.Contains(out, `--expire-days "14" "local/gardenio-commons-files"`) {
		t.Errorf("the attachments bucket must never get the archive's expiry; got:\n%s", out)
	}
	if !strings.Contains(out, `mc quota set "local/gardenio-commons-files" --size "5GiB"`) {
		t.Errorf("expected the attachments quota; got:\n%s", out)
	}

	off := helmTemplate(t, "templates/minio/bootstrap-job.yaml", "objectStore.enabled=true")
	if strings.Contains(off, "attachments bucket") {
		t.Errorf("no attachments bucket when commonsFiles.bucket is empty; got:\n%s", off)
	}
}

func TestObjectStore_Viewer_CommonsFilesBucketEnv(t *testing.T) {
	t.Parallel()
	on := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"objectStore.enabled=true",
		"objectStore.commonsFiles.bucket=gardenio-commons-files",
	)
	if !strings.Contains(on, "name: COMMONS_FILES_BUCKET") || !strings.Contains(on, `value: "gardenio-commons-files"`) {
		t.Errorf("expected COMMONS_FILES_BUCKET on beads-viewer; got:\n%s", on)
	}
	off := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true", "objectStore.enabled=true")
	if strings.Contains(off, "COMMONS_FILES_BUCKET") {
		t.Error("COMMONS_FILES_BUCKET must not render when commonsFiles.bucket is empty")
	}
	storeOff := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"objectStore.commonsFiles.bucket=gardenio-commons-files",
	)
	if strings.Contains(storeOff, "COMMONS_FILES_BUCKET") {
		t.Error("COMMONS_FILES_BUCKET must not render when the object store is off")
	}
}

// gd-urSiLLwPIz: the bootstrap Job turns the agent Secret into a MinIO user
// whose policy covers only the coop sessions prefix. Agent pods (crews run
// untrusted repo code) get this user, never the root keys.

// agentPolicyFromJob extracts the policy JSON the Job writes between its
// POLICY heredoc markers.
func agentPolicyFromJob(t *testing.T, out string) map[string]any {
	t.Helper()
	m := regexp.MustCompile(`(?s)<<'POLICY'\n\s*(\{.*?\})\n\s*POLICY\n`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("expected a POLICY heredoc in the bootstrap Job; got:\n%s", out)
	}
	var policy map[string]any
	if err := json.Unmarshal([]byte(m[1]), &policy); err != nil {
		t.Fatalf("agent policy is not valid JSON: %v\n%s", err, m[1])
	}
	return policy
}

func TestObjectStore_Bootstrap_AgentUserAndPolicy(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/minio/bootstrap-job.yaml",
		"objectStore.enabled=true",
		"agents.s3.bucket=gardenio-sessions",
		"agents.s3.prefix=coop/sessions",
	)
	for _, want := range []string{
		`mc admin policy create local "gardenio-coop-agent" /tmp/agent-policy.json`,
		// Secret key on stdin, never in mc's argv.
		`printf '%s\n' "$AGENT_SECRET_KEY" | mc admin user add local "$AGENT_ACCESS_KEY"`,
		`mc admin policy attach local "gardenio-coop-agent" --user "$AGENT_ACCESS_KEY"`,
		// Fail loudly (rolling back an --atomic upgrade) on a missing Secret
		// or one that reuses the root user.
		`if [ -z "${AGENT_ACCESS_KEY:-}" ] || [ -z "${AGENT_SECRET_KEY:-}" ]; then`,
		`if [ "$AGENT_ACCESS_KEY" = "$ACCESS_KEY" ]; then`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in the bootstrap Job; got:\n%s", want, out)
		}
	}
	// The pinned mc image (pgsty/mc) ships coreutils and bash but no grep, so
	// a grep in the script fails the Job and rolls back an --atomic upgrade.
	script := out[strings.Index(out, "set -eu"):]
	if regexp.MustCompile(`\bgrep\b`).MatchString(strings.ReplaceAll(script, "has no grep", "")) {
		t.Errorf("bootstrap script must not call grep (absent from the mc image); got:\n%s", script)
	}
	if strings.Contains(out, `"$AGENT_ACCESS_KEY" "$AGENT_SECRET_KEY"`) {
		t.Errorf("the agent secret key must not be passed in mc's argv")
	}
	for _, env := range []string{"AGENT_ACCESS_KEY", "AGENT_SECRET_KEY"} {
		re := regexp.MustCompile(`(?s)- name: ` + env + `\s+valueFrom:\s+secretKeyRef:\s+name: (\S+)\s+key: \S+\s+optional: true`)
		m := re.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("expected %s from an optional secretKeyRef; got:\n%s", env, out)
		}
		if m[1] != "test-minio-agent-credentials" {
			t.Errorf("%s comes from %q, want the agent Secret test-minio-agent-credentials", env, m[1])
		}
	}
	// MC_HOST_local expands $(ACCESS_KEY)/$(SECRET_KEY); the agent entries
	// must not be inserted between them and it in a way that breaks that.
	if strings.Index(out, "- name: MC_HOST_local") < strings.Index(out, "- name: SECRET_KEY") {
		t.Errorf("MC_HOST_local must stay after SECRET_KEY")
	}
}

func TestObjectStore_Bootstrap_AgentPolicyIsLeastPrivilege(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/minio/bootstrap-job.yaml",
		"objectStore.enabled=true",
		"agents.s3.bucket=gardenio-sessions",
		"agents.s3.prefix=/coop/sessions/",
	)
	policy := agentPolicyFromJob(t, out)
	stmts, _ := policy["Statement"].([]any)
	if len(stmts) == 0 {
		t.Fatalf("agent policy has no statements: %v", policy)
	}
	allowedResources := map[string]bool{
		"arn:aws:s3:::gardenio-sessions":                 true,
		"arn:aws:s3:::gardenio-sessions/coop/sessions/*": true,
	}
	sawList := false
	for _, raw := range stmts {
		st := raw.(map[string]any)
		if st["Effect"] != "Allow" {
			t.Errorf("unexpected non-Allow statement: %v", st)
		}
		for _, r := range st["Resource"].([]any) {
			if !allowedResources[r.(string)] {
				t.Errorf("policy grants an unexpected resource %q", r)
			}
		}
		for _, a := range st["Action"].([]any) {
			action := a.(string)
			if strings.Contains(action, "*") || strings.Contains(action, "Delete") ||
				strings.HasPrefix(action, "admin:") || strings.Contains(action, "Bucket") && action != "s3:ListBucket" && action != "s3:GetBucketLocation" {
				t.Errorf("policy grants a forbidden action %q", action)
			}
			if action == "s3:ListBucket" {
				sawList = true
				cond, _ := st["Condition"].(map[string]any)
				like, _ := cond["StringLike"].(map[string]any)
				prefixes, _ := like["s3:prefix"].([]any)
				if len(prefixes) != 1 || prefixes[0] != "coop/sessions/*" {
					t.Errorf("s3:ListBucket must be limited to the sessions prefix, got condition %v", st["Condition"])
				}
			}
		}
	}
	if !sawList {
		t.Error("agent policy must allow s3:ListBucket (coop restore lists the session prefix)")
	}
}

func TestObjectStore_Bootstrap_AgentPolicyFollowsBucketAndPrefix(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/minio/bootstrap-job.yaml",
		"objectStore.enabled=true",
		"objectStore.bucket=other-sessions",
		"agents.s3.prefix=sessions",
		"objectStore.agentCredentials.policyName=custom-agent",
	)
	policy, _ := json.Marshal(agentPolicyFromJob(t, out))
	for _, want := range []string{"arn:aws:s3:::other-sessions/sessions/*", `"sessions/*"`} {
		if !strings.Contains(string(policy), want) {
			t.Errorf("expected %s in the agent policy (agents.s3.bucket unset falls back to objectStore.bucket); got %s", want, policy)
		}
	}
	if !strings.Contains(out, `mc admin policy attach local "custom-agent"`) {
		t.Errorf("expected the configured policy name; got:\n%s", out)
	}
}

// gd-CmjVAU3vgd: session images get their own bucket, quota and expiry, so the
// archive filling bucketQuota can never block gb image upload; unset, the Job,
// the policies and the env render exactly as before.
func TestObjectStore_Bootstrap_ImagesBucket(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/minio/bootstrap-job.yaml",
		"objectStore.enabled=true",
		"objectStore.ilmExpiryDays=4",
		"objectStore.images.bucket=gardenio-images",
		"objectStore.images.quota=3GiB",
	)
	for _, want := range []string{
		`mc mb --ignore-existing "local/gardenio-images"`,
		`mc ilm rule remove --all --force "local/gardenio-images"`,
		`mc ilm rule add --expire-days "90" "local/gardenio-images"`,
		`mc quota set "local/gardenio-images" --size "3GiB"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in the bootstrap Job; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, `--expire-days "4" "local/gardenio-images"`) {
		t.Errorf("the images bucket must never get the archive's expiry; got:\n%s", out)
	}

	noExpiry := helmTemplate(t, "templates/minio/bootstrap-job.yaml",
		"objectStore.enabled=true",
		"objectStore.images.bucket=gardenio-images",
		"objectStore.images.ilmExpiryDays=0",
	)
	if regexp.MustCompile(`--expire-days "\d+" "local/gardenio-images"`).MatchString(noExpiry) {
		t.Errorf("ilmExpiryDays=0 must add no images expiry rule; got:\n%s", noExpiry)
	}
	if !strings.Contains(noExpiry, `mc quota clear "local/gardenio-images"`) {
		t.Errorf("an empty images quota must be cleared; got:\n%s", noExpiry)
	}

	off := helmTemplate(t, "templates/minio/bootstrap-job.yaml", "objectStore.enabled=true")
	if strings.Contains(off, "images bucket") {
		t.Errorf("no images bucket when images.bucket is empty; got:\n%s", off)
	}
}

func TestObjectStore_ImagesBucketPolicies(t *testing.T) {
	t.Parallel()
	flags := append([]string{}, beadsMinioOn...)
	out := helmTemplate(t, "templates/minio/bootstrap-job.yaml",
		append(flags, "objectStore.images.bucket=gardenio-images")...)

	agent, _ := json.Marshal(agentPolicyFromJob(t, out))
	if !strings.Contains(string(agent), `"arn:aws:s3:::gardenio-images/coop/sessions/images/*"`) {
		t.Errorf("agent policy must cover the images tree of the images bucket; got %s", agent)
	}
	if strings.Contains(string(agent), `"arn:aws:s3:::gardenio-images/coop/sessions/*"`) {
		t.Errorf("agent policy must not cover more of the images bucket than images/; got %s", agent)
	}
	beads := beadsPolicyFromJob(t, out)
	stmts, _ := beads["Statement"].([]any)
	if len(stmts) != 1 {
		t.Fatalf("beads policy must stay one statement, got %v", beads)
	}
	st := stmts[0].(map[string]any)
	resources, _ := st["Resource"].([]any)
	want := []any{"arn:aws:s3:::gardenio-sessions/coop/sessions/images/*", "arn:aws:s3:::gardenio-images/coop/sessions/images/*"}
	if len(resources) != 2 || resources[0] != want[0] || resources[1] != want[1] {
		t.Errorf("beads policy resources = %v, want %v (old refs fall back to the session bucket)", resources, want)
	}
	if actions, _ := st["Action"].([]any); len(actions) != 1 || actions[0] != "s3:GetObject" {
		t.Errorf("beads policy must allow s3:GetObject only, got %v", actions)
	}
}

func TestObjectStore_ImagesBucketEnv(t *testing.T) {
	t.Parallel()
	on := []string{
		"objectStore.enabled=true",
		"objectStore.images.bucket=gardenio-images",
		"beadsViewer.enabled=true",
		"agents.enabled=true",
		"agents.s3.bucket=gardenio-sessions",
		"agents.s3.endpoint=http://gardenio-minio:9000",
	}
	for _, tpl := range []string{"templates/controller/deployment.yaml", "templates/beads-viewer/deployment.yaml"} {
		got := helmTemplate(t, tpl, on...)
		if !regexp.MustCompile(`- name: COOP_S3_IMAGES_BUCKET\s+value: "gardenio-images"`).MatchString(got) {
			t.Errorf("%s: expected COOP_S3_IMAGES_BUCKET=gardenio-images; got:\n%s", tpl, got)
		}
		if off := helmTemplate(t, tpl, "objectStore.enabled=true", "beadsViewer.enabled=true", "agents.enabled=true", "agents.s3.bucket=gardenio-sessions", "agents.s3.endpoint=http://gardenio-minio:9000"); strings.Contains(off, "COOP_S3_IMAGES_BUCKET") {
			t.Errorf("%s: COOP_S3_IMAGES_BUCKET must not render when images.bucket is empty", tpl)
		}
		if storeOff := helmTemplate(t, tpl, "objectStore.images.bucket=gardenio-images", "beadsViewer.enabled=true", "agents.enabled=true", "agents.s3.bucket=gardenio-sessions", "agents.s3.endpoint=http://gardenio-minio:9000"); strings.Contains(storeOff, "COOP_S3_IMAGES_BUCKET") {
			t.Errorf("%s: COOP_S3_IMAGES_BUCKET must not render when the object store is off", tpl)
		}
	}
	beads := helmBeadsDeployment(t, setFlags(on)...)
	if !regexp.MustCompile(`- name: COOP_S3_IMAGES_BUCKET\s+value: "gardenio-images"`).MatchString(beads) {
		t.Errorf("beads Deployment: expected COOP_S3_IMAGES_BUCKET=gardenio-images; got:\n%s", beads)
	}
}

func TestObjectStore_ImagesBucketK3sDeployValues(t *testing.T) {
	t.Parallel()
	out := helmBeadsDeployment(t, "--values", k3sDeployValues)
	if !regexp.MustCompile(`- name: COOP_S3_IMAGES_BUCKET\s+value: "gardenio-images"`).MatchString(out) {
		t.Errorf("k3s: expected the beads daemon to read gardenio-images; got:\n%s", out)
	}
}
