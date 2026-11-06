package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- type:project shape validation ---------------------------------------

func TestValidateProjectFields_EmptyOK(t *testing.T) {
	if res := validateProjectFields(nil, "gardenio"); res.RejectReason != "" {
		t.Fatalf("nil fields should be accepted, got reject %q", res.RejectReason)
	}
	if res := validateProjectFields(json.RawMessage(`{}`), "gardenio"); res.RejectReason != "" {
		t.Fatalf("empty object should be accepted, got reject %q", res.RejectReason)
	}
}

func TestValidateProjectFields_GoodBeadAccepted(t *testing.T) {
	// A well-formed project bead with nested-JSON structured fields and a
	// properly-prefixed secret. No reject, no warnings.
	fields := json.RawMessage(`{
		"prefix": "kd",
		"git_url": "https://gitlab.com/org/repo.git",
		"secrets": [{"env":"GIT_TOKEN","secret":"gardenio-git-token","key":"token"}],
		"repos": [{"url":"https://gitlab.com/org/other.git","role":"reference"}],
		"env": [{"name":"FOO","value":"bar"}],
		"prewarmed_pool": {"enabled":true,"min_size":1,"max_size":3,"role":"thread","mode":"crew"},
		"channel_roles": {"C123":"crew"},
		"claude_config": {"model":"claude-opus-4-8"}
	}`)
	res := validateProjectFields(fields, "gardenio")
	if res.RejectReason != "" {
		t.Fatalf("good project bead should be accepted, got reject %q", res.RejectReason)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("good project bead should have no warnings, got %v", res.Warnings)
	}
}

func TestValidateProjectFields_GoodBead_StringOfJSON(t *testing.T) {
	// Structured fields stored as strings-of-JSON (the alternate storage shape
	// the controller's ParseFieldsJSON tolerates) must validate identically.
	fields := json.RawMessage(`{
		"git_url": "https://gitlab.com/org/repo.git",
		"secrets": "[{\"env\":\"GIT_TOKEN\",\"secret\":\"gardenio-git-token\",\"key\":\"token\"}]",
		"repos": "[{\"url\":\"https://gitlab.com/org/x.git\"}]"
	}`)
	res := validateProjectFields(fields, "gardenio")
	if res.RejectReason != "" {
		t.Fatalf("string-of-JSON fields should be accepted, got reject %q", res.RejectReason)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", res.Warnings)
	}
}

func TestValidateProjectFields_MalformedSecretsRejected(t *testing.T) {
	// secrets must be an ARRAY of {env,secret,key}; an object is the silent-drop
	// shape mismatch we reject hard.
	fields := json.RawMessage(`{"git_url":"https://gitlab.com/o/r.git","secrets":{"env":"X"}}`)
	res := validateProjectFields(fields, "gardenio")
	if res.RejectReason == "" {
		t.Fatal("expected hard reject for object-shaped secrets")
	}
	if !strings.Contains(res.RejectReason, "secrets") {
		t.Fatalf("reject reason should name the field: %q", res.RejectReason)
	}
}

func TestValidateProjectFields_MalformedReposRejected(t *testing.T) {
	// repos as a bare string (not an array) is a shape mismatch.
	fields := json.RawMessage(`{"git_url":"https://gitlab.com/o/r.git","repos":"not-an-array"}`)
	res := validateProjectFields(fields, "gardenio")
	if res.RejectReason == "" {
		t.Fatal("expected hard reject for string-shaped repos")
	}
	if !strings.Contains(res.RejectReason, "repos") {
		t.Fatalf("reject reason should name the field: %q", res.RejectReason)
	}
}

func TestValidateProjectFields_TruncatedJSONRejected(t *testing.T) {
	// Truncated JSON inside a string-of-JSON secrets field.
	fields := json.RawMessage(`{"secrets":"[{\"env\":\"X\""}`)
	res := validateProjectFields(fields, "gardenio")
	if res.RejectReason == "" {
		t.Fatal("expected hard reject for truncated secrets JSON")
	}
}

func TestValidateProjectFields_MissingGitURLWarns(t *testing.T) {
	// No git_url, valid everything else → soft warning, NOT a hard reject.
	fields := json.RawMessage(`{"prefix":"kd"}`)
	res := validateProjectFields(fields, "gardenio")
	if res.RejectReason != "" {
		t.Fatalf("missing git_url must NOT hard-reject, got %q", res.RejectReason)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("missing git_url should produce a warning")
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "git_url") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a git_url warning, got %v", res.Warnings)
	}
}

func TestValidateProjectFields_GitURLPresentNoWarn(t *testing.T) {
	fields := json.RawMessage(`{"git_url":"https://gitlab.com/o/r.git"}`)
	res := validateProjectFields(fields, "gardenio")
	for _, w := range res.Warnings {
		if strings.Contains(w, "git_url") {
			t.Fatalf("git_url present should not warn, got %v", res.Warnings)
		}
	}
}

func TestValidateProjectFields_SecretPrefixMismatchWarns(t *testing.T) {
	// Secret name "wrongprefix-token" does not start with "gardenio-" → warn,
	// not reject.
	fields := json.RawMessage(`{
		"git_url":"https://gitlab.com/o/r.git",
		"secrets":[{"env":"GIT_TOKEN","secret":"wrongprefix-token","key":"token"}]
	}`)
	res := validateProjectFields(fields, "gardenio")
	if res.RejectReason != "" {
		t.Fatalf("prefix mismatch must NOT hard-reject, got %q", res.RejectReason)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("prefix mismatch should produce a warning")
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "wrongprefix-token") && strings.Contains(w, "gardenio-") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a prefix warning naming the secret + expected prefix, got %v", res.Warnings)
	}
}

func TestValidateProjectFields_SecretPrefixOKNoWarn(t *testing.T) {
	fields := json.RawMessage(`{
		"git_url":"https://gitlab.com/o/r.git",
		"secrets":[{"env":"GIT_TOKEN","secret":"gardenio-git-token","key":"token"}]
	}`)
	res := validateProjectFields(fields, "gardenio")
	if len(res.Warnings) != 0 {
		t.Fatalf("correctly-prefixed secret should not warn, got %v", res.Warnings)
	}
}

func TestValidateProjectFields_EmptyNameSkipsPrefixCheck(t *testing.T) {
	// When the project name is unknown, the prefix check is skipped (cannot
	// derive "<project>-").
	fields := json.RawMessage(`{
		"git_url":"https://gitlab.com/o/r.git",
		"secrets":[{"env":"GIT_TOKEN","secret":"anything-token","key":"token"}]
	}`)
	res := validateProjectFields(fields, "")
	for _, w := range res.Warnings {
		if strings.Contains(w, "prefix") {
			t.Fatalf("empty name should skip prefix check, got %v", res.Warnings)
		}
	}
}

func TestValidateProjectFields_LegacyTitlePrefixStrippedForSecretCheck(t *testing.T) {
	// A legacy project bead titled "Project: gardenio" resolves to name
	// "gardenio" (controller strips the "Project: " prefix), so a secret named
	// "gardenio-git-token" is correctly prefixed and must NOT warn.
	fields := json.RawMessage(`{
		"git_url":"https://gitlab.com/o/r.git",
		"secrets":[{"env":"GIT_TOKEN","secret":"gardenio-git-token","key":"token"}]
	}`)
	res := validateProjectFields(fields, "Project: gardenio")
	if len(res.Warnings) != 0 {
		t.Fatalf("legacy 'Project: ' title should resolve to bare name for the "+
			"prefix check; correctly-prefixed secret must not warn, got %v", res.Warnings)
	}
}

func TestValidateProjectFields_EmptySecretNameSkipped(t *testing.T) {
	// A secrets entry with an empty `secret` name is skipped by the prefix
	// check — no warning, no reject (the entry contributes no K8s Secret).
	fields := json.RawMessage(`{
		"git_url":"https://gitlab.com/o/r.git",
		"secrets":[{"env":"X","secret":"","key":"k"}]
	}`)
	res := validateProjectFields(fields, "gardenio")
	if res.RejectReason != "" {
		t.Fatalf("empty secret name must not reject, got %q", res.RejectReason)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "prefix") {
			t.Fatalf("empty secret name must not warn about prefix, got %v", res.Warnings)
		}
	}
}

// --- type:role shape validation ------------------------------------------

func TestValidateRoleFields_EmptyOK(t *testing.T) {
	if res := validateRoleFields(nil, "crew"); res.RejectReason != "" {
		t.Fatalf("nil fields should be accepted, got %q", res.RejectReason)
	}
	if res := validateRoleFields(json.RawMessage(`{}`), "crew"); res.RejectReason != "" {
		t.Fatalf("empty object should be accepted, got %q", res.RejectReason)
	}
}

func TestValidateRoleFields_GoodBeadAccepted(t *testing.T) {
	fields := json.RawMessage(`{
		"image": "registry/agent:latest",
		"secrets": [{"env":"TOK","secret":"crew-tok","key":"token"}],
		"env": [{"name":"FOO","value":"bar"}],
		"env_json": {"A":"1"},
		"claude_config": {"model":"claude-opus-4-8"}
	}`)
	res := validateRoleFields(fields, "crew")
	if res.RejectReason != "" {
		t.Fatalf("good role bead should be accepted, got reject %q", res.RejectReason)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("good role bead should have no warnings, got %v", res.Warnings)
	}
}

func TestValidateRoleFields_MalformedSecretsRejected(t *testing.T) {
	fields := json.RawMessage(`{"secrets":"[bad json"}`)
	res := validateRoleFields(fields, "crew")
	if res.RejectReason == "" {
		t.Fatal("expected hard reject for malformed role secrets")
	}
	if !strings.Contains(res.RejectReason, "secrets") {
		t.Fatalf("reject reason should name the field: %q", res.RejectReason)
	}
}

func TestValidateRoleFields_MalformedEnvJSONRejected(t *testing.T) {
	// env_json must be a map[string]string; an array is a shape mismatch.
	fields := json.RawMessage(`{"env_json":["not","a","map"]}`)
	res := validateRoleFields(fields, "crew")
	if res.RejectReason == "" {
		t.Fatal("expected hard reject for array-shaped env_json")
	}
	if !strings.Contains(res.RejectReason, "env_json") {
		t.Fatalf("reject reason should name the field: %q", res.RejectReason)
	}
}

func TestValidateRoleFields_NoGitURLWarning(t *testing.T) {
	// Role beads carry no git_url, so a role bead without one must NOT warn
	// about git_url (that check is project-only).
	fields := json.RawMessage(`{"image":"registry/agent:latest"}`)
	res := validateRoleFields(fields, "crew")
	for _, w := range res.Warnings {
		if strings.Contains(w, "git_url") {
			t.Fatalf("role beads should not get a git_url warning, got %v", res.Warnings)
		}
	}
}

func TestValidateRoleFields_SecretPrefixMismatchWarns(t *testing.T) {
	fields := json.RawMessage(`{"secrets":[{"env":"TOK","secret":"notcrew-tok","key":"token"}]}`)
	res := validateRoleFields(fields, "crew")
	if res.RejectReason != "" {
		t.Fatalf("prefix mismatch must NOT hard-reject, got %q", res.RejectReason)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "notcrew-tok") && strings.Contains(w, "crew-") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a role prefix warning, got %v", res.Warnings)
	}
}

func TestValidateRoleFields_SecretPrefixOKNoWarn(t *testing.T) {
	fields := json.RawMessage(`{"secrets":[{"env":"TOK","secret":"crew-tok","key":"token"}]}`)
	res := validateRoleFields(fields, "crew")
	if len(res.Warnings) != 0 {
		t.Fatalf("correctly-prefixed role secret should not warn, got %v", res.Warnings)
	}
}

// --- type:role privilege gate ---------------------------------------------

func TestValidateRoleFields_NeedsK8sAPIWithoutPrivilegedRejected(t *testing.T) {
	fields := json.RawMessage(`{"needs_k8s_api":true}`)
	res := validateRoleFields(fields, "k8s-live")
	if res.RejectReason == "" {
		t.Fatal("expected hard reject for needs_k8s_api without privileged")
	}
	if !strings.Contains(res.RejectReason, "needs_k8s_api") || !strings.Contains(res.RejectReason, "privileged") {
		t.Fatalf("reject reason should name the field and privileged: %q", res.RejectReason)
	}
}

func TestValidateRoleFields_NeedsK8sAPIWithPrivilegedAccepted(t *testing.T) {
	fields := json.RawMessage(`{"needs_k8s_api":true,"privileged":true}`)
	res := validateRoleFields(fields, "k8s-live")
	if res.RejectReason != "" {
		t.Fatalf("privileged=true should permit needs_k8s_api, got reject %q", res.RejectReason)
	}
}

func TestValidateRoleFields_NeedsK8sAPIOnSharedServiceAccountRejected(t *testing.T) {
	fields := json.RawMessage(`{"needs_k8s_api":true,"privileged":true,"service_account":"gardenio-agent"}`)
	res := validateRoleFields(fields, "k8s-live")
	if res.RejectReason == "" {
		t.Fatal("expected hard reject for needs_k8s_api on the shared gardenio-agent SA")
	}
	if !strings.Contains(res.RejectReason, "gardenio-agent") || !strings.Contains(res.RejectReason, "needs_k8s_api") {
		t.Fatalf("reject reason should name the field and the SA: %q", res.RejectReason)
	}
}

func TestValidateRoleFields_NeedsK8sAPIDedicatedServiceAccountAccepted(t *testing.T) {
	fields := json.RawMessage(`{"needs_k8s_api":true,"privileged":true,"service_account":"gc-k8s-live"}`)
	res := validateRoleFields(fields, "k8s-live")
	if res.RejectReason != "" || len(res.Warnings) != 0 {
		t.Fatalf("dedicated SA should pass clean, got reject %q warnings %v", res.RejectReason, res.Warnings)
	}
}

func TestValidateRoleFields_NeedsK8sAPIWithoutServiceAccountWarns(t *testing.T) {
	fields := json.RawMessage(`{"needs_k8s_api":true,"privileged":true}`)
	res := validateRoleFields(fields, "k8s-live")
	if res.RejectReason != "" {
		t.Fatalf("missing service_account must warn, not reject: %q", res.RejectReason)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "service_account") {
		t.Fatalf("expected one service_account warning, got %v", res.Warnings)
	}
}

func TestValidateRoleFields_ServiceAccountWithoutPrivilegedRejected(t *testing.T) {
	fields := json.RawMessage(`{"service_account":"gc-agent"}`)
	res := validateRoleFields(fields, "k8s-live")
	if res.RejectReason == "" {
		t.Fatal("expected hard reject for service_account without privileged")
	}
	if !strings.Contains(res.RejectReason, "service_account") {
		t.Fatalf("reject reason should name the field: %q", res.RejectReason)
	}
}

func TestValidateRoleFields_ZitiBindWithoutPrivilegedRejected(t *testing.T) {
	fields := json.RawMessage(`{"ziti_bind":true}`)
	res := validateRoleFields(fields, "crew")
	if res.RejectReason == "" {
		t.Fatal("expected hard reject for ziti_bind without privileged")
	}
	if !strings.Contains(res.RejectReason, "ziti_bind") {
		t.Fatalf("reject reason should name the field: %q", res.RejectReason)
	}
}

func TestValidateRoleFields_DockerWithoutPrivilegedRejected(t *testing.T) {
	fields := json.RawMessage(`{"docker":true}`)
	res := validateRoleFields(fields, "crew")
	if res.RejectReason == "" {
		t.Fatal("expected hard reject for docker without privileged")
	}
	if !strings.Contains(res.RejectReason, "docker") {
		t.Fatalf("reject reason should name the field: %q", res.RejectReason)
	}
}

func TestValidateRoleFields_NonDefaultExecutionTargetWithoutPrivilegedRejected(t *testing.T) {
	fields := json.RawMessage(`{"execution_target":"lambda_microvm"}`)
	res := validateRoleFields(fields, "crew")
	if res.RejectReason == "" {
		t.Fatal("expected hard reject for non-default execution_target without privileged")
	}
	if !strings.Contains(res.RejectReason, "execution_target") {
		t.Fatalf("reject reason should name the field: %q", res.RejectReason)
	}
}

func TestValidateRoleFields_DefaultExecutionTargetsAllowedWithoutPrivileged(t *testing.T) {
	for _, et := range []string{"", "kubernetes"} {
		fields := json.RawMessage(`{"execution_target":"` + et + `"}`)
		res := validateRoleFields(fields, "crew")
		if res.RejectReason != "" {
			t.Fatalf("execution_target=%q should not require privileged, got reject %q", et, res.RejectReason)
		}
	}
}

func TestValidateRoleFields_DockerFalseWithoutPrivilegedAccepted(t *testing.T) {
	// An explicit "false" is not a privilege grant — must not require privileged.
	fields := json.RawMessage(`{"docker":false,"needs_k8s_api":false}`)
	res := validateRoleFields(fields, "crew")
	if res.RejectReason != "" {
		t.Fatalf("explicit false privileged-gated fields should not reject, got %q", res.RejectReason)
	}
}

func TestValidateRoleFields_PlainRoleUnaffected(t *testing.T) {
	// Acceptance: existing role beads without any privileged-gated settings are
	// unaffected by this check.
	fields := json.RawMessage(`{"image":"registry/agent:latest","cpu_request":"500m"}`)
	res := validateRoleFields(fields, "crew")
	if res.RejectReason != "" {
		t.Fatalf("plain role bead should be unaffected, got reject %q", res.RejectReason)
	}
}

func TestValidateRoleFields_UpdateNotGrandfathered(t *testing.T) {
	// Unlike the shape checks, the privilege gate is a security rule: a prior
	// value byte-identical to the new one must still be rejected on update, not
	// grandfathered.
	prior := json.RawMessage(`{"needs_k8s_api":true}`)
	fields := json.RawMessage(`{"needs_k8s_api":true}`)
	res := validateRoleFieldsForUpdate(fields, prior, "k8s-live")
	if res.RejectReason == "" {
		t.Fatal("expected the privilege gate to reject even an unchanged prior value")
	}
}

// --- shared helpers -------------------------------------------------------

func TestParseFieldsToStringMap_NestedAndString(t *testing.T) {
	// A nested array and a string-of-JSON must both normalize to the same text.
	nested := parseFieldsToStringMap(json.RawMessage(`{"secrets":[{"env":"X"}]}`))
	asStr := parseFieldsToStringMap(json.RawMessage(`{"secrets":"[{\"env\":\"X\"}]"}`))
	if nested["secrets"] != asStr["secrets"] {
		t.Fatalf("nested vs string-of-JSON should normalize equal:\n nested=%q\n string=%q",
			nested["secrets"], asStr["secrets"])
	}
}

func TestParseFieldsToStringMap_NonObjectReturnsNil(t *testing.T) {
	if m := parseFieldsToStringMap(json.RawMessage(`["not","an","object"]`)); m != nil {
		t.Fatalf("non-object fields should return nil, got %v", m)
	}
}

func TestShapeParse_TrailingGarbageRejected(t *testing.T) {
	// json.Unmarshal accepts the first value but our decoder rejects trailing
	// data, matching the read-path detector.
	if err := shapeParse[[]projSecretEntry](`[] garbage`); err == nil {
		t.Fatal("expected trailing-data rejection")
	}
}
