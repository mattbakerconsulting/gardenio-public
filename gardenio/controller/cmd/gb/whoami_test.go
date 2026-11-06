package main

import (
	"strings"
	"testing"
)

func TestParseKubectlWhoami(t *testing.T) {
	// A real SelfSubjectReview from `kubectl auth whoami -o json` in a gardenio
	// agent pod — the SA path the ka-0000003844 miss failed to probe.
	data := []byte(`{
  "kind": "SelfSubjectReview",
  "status": {
    "userInfo": {
      "username": "system:serviceaccount:gardenio-system:gardenio-agent-cluster-admin",
      "uid": "e3ee4a2f-a5eb-47ce-b744-3a79e544679b",
      "groups": ["system:serviceaccounts", "system:serviceaccounts:gardenio-system", "system:authenticated"],
      "extra": {
        "authentication.kubernetes.io/node-name": ["ip-10-50-186-131.ec2.internal"],
        "authentication.kubernetes.io/pod-name": ["crew-gardenio-crew-x"]
      }
    }
  }
}`)
	var k whoamiK8s
	parseKubectlWhoami(data, &k)
	if k.Error != "" {
		t.Fatalf("unexpected error: %s", k.Error)
	}
	if k.ServiceAccount != "gardenio-agent-cluster-admin" {
		t.Errorf("ServiceAccount = %q, want gardenio-agent-cluster-admin", k.ServiceAccount)
	}
	if k.Namespace != "gardenio-system" {
		t.Errorf("Namespace = %q, want gardenio-system", k.Namespace)
	}
	if k.Username != "system:serviceaccount:gardenio-system:gardenio-agent-cluster-admin" {
		t.Errorf("Username = %q", k.Username)
	}
	if len(k.Groups) != 3 {
		t.Errorf("Groups len = %d, want 3", len(k.Groups))
	}
	if k.Node != "ip-10-50-186-131.ec2.internal" {
		t.Errorf("Node = %q", k.Node)
	}
	if k.PodName != "crew-gardenio-crew-x" {
		t.Errorf("PodName = %q", k.PodName)
	}
}

func TestParseKubectlWhoami_NonSA(t *testing.T) {
	// A human/OIDC user is not a ServiceAccount — ServiceAccount stays empty
	// but the username still resolves.
	data := []byte(`{"status":{"userInfo":{"username":"matthew.baker@acme.example","groups":["system:authenticated"]}}}`)
	var k whoamiK8s
	parseKubectlWhoami(data, &k)
	if k.ServiceAccount != "" {
		t.Errorf("ServiceAccount = %q, want empty for non-SA user", k.ServiceAccount)
	}
	if k.Username != "matthew.baker@acme.example" {
		t.Errorf("Username = %q", k.Username)
	}
}

func TestParseKubectlWhoami_Garbage(t *testing.T) {
	var k whoamiK8s
	parseKubectlWhoami([]byte("not json"), &k)
	if k.Error == "" {
		t.Error("expected an error for malformed input")
	}
}

func TestGitlabPathFromURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://gitlab.com/Acme/Platform/gardenio.git", "Acme/Platform/gardenio"},
		{"https://gitlab.com/Acme/Platform/gardenio.git:kd", "Acme/Platform/gardenio"},
		{"https://gitlab.com/Acme/Platform/monorepo:PE", "Acme/Platform/monorepo"},
		{"git@gitlab.com:Acme/Platform/gardenio.git", "Acme/Platform/gardenio"},
		{"ssh://git@gitlab.com/Acme/Platform/repo.git", "Acme/Platform/repo"},
		{"https://github.com/other/repo.git", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := gitlabPathFromURL(tt.in); got != tt.want {
			t.Errorf("gitlabPathFromURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestGitlabPathFromSpec(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"gardenio=https://gitlab.com/Acme/Platform/gardenio.git:kd", "Acme/Platform/gardenio"},
		{"monorepo=https://gitlab.com/Acme/Platform/monorepo:PE", "Acme/Platform/monorepo"},
		{"https://gitlab.com/Acme/Platform/gardenio.git", "Acme/Platform/gardenio"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := gitlabPathFromSpec(tt.in); got != tt.want {
			t.Errorf("gitlabPathFromSpec(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestGitlabAccessLevelName(t *testing.T) {
	tests := []struct {
		level int
		want  string
	}{
		{0, "No access"},
		{10, "Guest"},
		{20, "Reporter"},
		{30, "Developer"},
		{40, "Maintainer"},
		{50, "Owner"},
		{99, "level 99"},
	}
	for _, tt := range tests {
		if got := gitlabAccessLevelName(tt.level); got != tt.want {
			t.Errorf("gitlabAccessLevelName(%d) = %q, want %q", tt.level, got, tt.want)
		}
	}
}

func TestSecretPrefixOf(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"gardenio/prod/kd-beads-token", "gardenio/prod"},
		{"/gardenio/aws-poc/litellm/master-key", "gardenio/aws-poc"},
		{"gardenio/gardenio-gitlab-token", "gardenio/gardenio-gitlab-token"},
		{"single", "single"},
		{"a/b", "a/b"},
	}
	for _, tt := range tests {
		if got := secretPrefixOf(tt.in); got != tt.want {
			t.Errorf("secretPrefixOf(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsIAMRoleARNAndAccount(t *testing.T) {
	role := "arn:aws:iam::123456789012:role/gardenio-prod-agent"
	if !isIAMRoleARN(role) {
		t.Errorf("isIAMRoleARN(%q) = false, want true", role)
	}
	if got := accountFromARN(role); got != "123456789012" {
		t.Errorf("accountFromARN(%q) = %q, want 123456789012", role, got)
	}
	// A user ARN is not a role.
	user := "arn:aws:iam::123456789012:user/bob"
	if isIAMRoleARN(user) {
		t.Errorf("isIAMRoleARN(%q) = true, want false", user)
	}
	// An assumed-role STS ARN is not an iam role ARN.
	sts := "arn:aws:sts::123456789012:assumed-role/gardenio-prod-agent/session"
	if isIAMRoleARN(sts) {
		t.Errorf("isIAMRoleARN(%q) = true, want false", sts)
	}
	if got := accountFromARN("garbage"); got != "" {
		t.Errorf("accountFromARN(garbage) = %q, want empty", got)
	}
}

func TestDiscoverAssumableTargets_Env(t *testing.T) {
	// The current identity ARN must be excluded; other *_ROLE_ARN vars become
	// candidates.
	self := "arn:aws:iam::111111111111:role/current"
	t.Setenv("AWS_ROLE_ARN", self)
	t.Setenv("MICROVM_LAUNCHER_ROLE_ARN", "arn:aws:iam::222222222222:role/launcher")
	t.Setenv("NOT_A_ROLE", "arn:aws:iam::333333333333:role/ignored")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent/aws/config") // isolate from any real config

	targets := discoverAssumableTargets(self)
	var arns []string
	for _, tt := range targets {
		if tt.RoleARN == self {
			t.Errorf("current identity ARN should be excluded, found %q", tt.RoleARN)
		}
		arns = append(arns, tt.RoleARN)
	}
	if !containsString(arns, "arn:aws:iam::222222222222:role/launcher") {
		t.Errorf("expected launcher role among targets, got %v", arns)
	}
	if containsString(arns, "arn:aws:iam::333333333333:role/ignored") {
		t.Errorf("NOT_A_ROLE var should not be treated as an assume target, got %v", arns)
	}
	for _, tt := range targets {
		if tt.Status != "candidate" {
			t.Errorf("target %q status = %q, want candidate (no --probe)", tt.RoleARN, tt.Status)
		}
		if tt.RoleARN == "arn:aws:iam::222222222222:role/launcher" && tt.Account != "222222222222" {
			t.Errorf("launcher account = %q, want 222222222222", tt.Account)
		}
	}
}

func TestParseAWSConfigRoleARNs(t *testing.T) {
	body := `[default]
region = us-east-1

[profile ci]
role_arn = arn:aws:iam::444444444444:role/ci-deploy
source_profile = default

# a comment
[profile empty]
region = us-west-2
`
	got := parseAWSConfigRoleARNs(body)
	if len(got) != 1 {
		t.Fatalf("parseAWSConfigRoleARNs returned %d, want 1: %+v", len(got), got)
	}
	if got[0].RoleARN != "arn:aws:iam::444444444444:role/ci-deploy" {
		t.Errorf("RoleARN = %q", got[0].RoleARN)
	}
	if got[0].Name != "ci" {
		t.Errorf("Name = %q, want ci", got[0].Name)
	}
	if got[0].Account != "444444444444" {
		t.Errorf("Account = %q, want 444444444444", got[0].Account)
	}
}

func TestRenderWhoami_Smoke(t *testing.T) {
	// A fully-populated report renders every section without panicking, and the
	// section headers are present.
	spend := 12.5
	budget := 100.0
	r := whoamiReport{
		GeneratedAt: "2026-07-31T00:00:00Z",
		Pod:         whoamiPod{Agent: "agent-x", Role: "crew", Project: "gardenio", TaskID: "gd-abc"},
		AWS: whoamiAWS{
			RoleARN:        "arn:aws:iam::123456789012:role/gardenio-prod-agent",
			CallerIdentity: &awsCallerIdentity{Arn: "arn:aws:sts::123456789012:assumed-role/x/s", Account: "123456789012"},
			Region:         "us-east-1",
			AssumableTargets: []assumableTarget{
				{Name: "ci", RoleARN: "arn:aws:iam::444444444444:role/ci", Account: "444444444444", Source: "aws-config:ci", Status: "candidate"},
			},
		},
		Kubernetes: whoamiK8s{
			ServiceAccount: "gardenio-agent-cluster-admin",
			Namespace:      "gardenio-system",
			Groups:         []string{"system:authenticated"},
			Can: []canIResult{
				{Action: "get pods (all namespaces)", Allowed: true},
				{Action: "create jobs", Allowed: false},
			},
		},
		GitLab: whoamiGitLab{
			User:  &gitlabUser{ID: 1, Username: "bot", State: "active", Bot: true},
			Repos: []repoAccess{{Path: "Acme/Platform/gardenio", AccessLevel: 30, AccessName: "Developer", Via: "group"}},
		},
		Secrets: whoamiSecrets{
			Total:    2,
			Prefixes: []secretPrefix{{Prefix: "gardenio/prod", Count: 2}},
			Secrets:  []secretEntry{{Name: "gardenio/prod/a", LastAccessed: "2026-07-30T00:00:00Z"}},
		},
		LLM: &whoamiLLM{BaseURL: "http://litellm:4000", Spend: &spend, MaxBudget: &budget},
	}
	var sb strings.Builder
	renderWhoami(&sb, r)
	out := sb.String()
	for _, want := range []string{"AWS", "Kubernetes", "GitLab", "Secrets", "LLM",
		"gardenio-agent-cluster-admin", "Developer", "gardenio/prod", "arn:aws:iam::444444444444:role/ci"} {
		if !strings.Contains(out, want) {
			t.Errorf("render output missing %q\n---\n%s", want, out)
		}
	}
	// can-i outcomes render as ✓ / ✗.
	if !strings.Contains(out, "✓  can-i get pods") || !strings.Contains(out, "✗  can-i create jobs") {
		t.Errorf("can-i marks not rendered as expected:\n%s", out)
	}
}

// (containsString is defined in trigger_test.go)

func TestRenderWhoami_ErrorsFailOpen(t *testing.T) {
	// Every section errored — render still emits all headers and each error,
	// and does not panic (the fail-open contract).
	r := whoamiReport{
		AWS:        whoamiAWS{Error: "sts: denied", AssumableTargets: []assumableTarget{}},
		Kubernetes: whoamiK8s{Error: "kubectl: not found", Can: []canIResult{}},
		GitLab:     whoamiGitLab{Error: "glab: 401", Repos: []repoAccess{}},
		Secrets:    whoamiSecrets{Error: "list-secrets: AccessDenied"},
	}
	var sb strings.Builder
	renderWhoami(&sb, r)
	out := sb.String()
	for _, want := range []string{"sts: denied", "kubectl: not found", "glab: 401", "list-secrets: AccessDenied"} {
		if !strings.Contains(out, want) {
			t.Errorf("render output missing error %q\n---\n%s", want, out)
		}
	}
}
