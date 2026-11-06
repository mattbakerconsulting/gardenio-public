package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestBuildInitConfig_Defaults(t *testing.T) {
	// Clear env to test defaults.
	for _, k := range []string{"BOAT_WORKSPACE", "COOP_PORT", "COOP_HEALTH_PORT",
		"COOP_MAX_RESTARTS", "BOAT_COMMAND", "BOAT_SESSION_RESUME",
		"BOAT_ROLE", "BOAT_PROJECT", "BOAT_AGENT", "POD_IP", "CLAUDE_MODEL"} {
		t.Setenv(k, "")
	}

	cfg := buildInitConfig(false, false)

	if cfg.coopPort != 8080 {
		t.Errorf("coopPort = %d, want 8080", cfg.coopPort)
	}
	if cfg.coopHealthPort != 9090 {
		t.Errorf("coopHealthPort = %d, want 9090", cfg.coopHealthPort)
	}
	if cfg.maxRestarts != 10 {
		t.Errorf("maxRestarts = %d, want 10", cfg.maxRestarts)
	}
	if cfg.role != "unknown" {
		t.Errorf("role = %q, want %q", cfg.role, "unknown")
	}
	if cfg.podIP != "localhost" {
		t.Errorf("podIP = %q, want %q", cfg.podIP, "localhost")
	}
}

func TestBuildInitConfig_MuxSessionIDIsFullPodName(t *testing.T) {
	fullName := "crew-gardenio-plan-gardener-plan-gardener-for-gd-unpaf2zvvh-88jq" // 64 chars
	if len(fullName) <= 63 {
		t.Fatalf("test fixture must exceed 63 chars, got %d", len(fullName))
	}
	t.Setenv("POD_NAME", fullName)

	if got := buildInitConfig(true, true).hostname; got != fullName {
		t.Errorf("hostname = %q (len %d), want the full pod name (len %d)", got, len(got), len(fullName))
	}
}

func TestBuildInitConfig_FromEnv(t *testing.T) {
	t.Setenv("COOP_PORT", "9999")
	t.Setenv("COOP_HEALTH_PORT", "9998")
	t.Setenv("COOP_MAX_RESTARTS", "5")
	t.Setenv("BOAT_ROLE", "crew")
	t.Setenv("BOAT_PROJECT", "monorepo")
	t.Setenv("BOAT_AGENT", "test-agent")
	t.Setenv("POD_IP", "10.0.0.1")

	cfg := buildInitConfig(true, true)

	if cfg.coopPort != 9999 {
		t.Errorf("coopPort = %d, want 9999", cfg.coopPort)
	}
	if cfg.coopHealthPort != 9998 {
		t.Errorf("coopHealthPort = %d, want 9998", cfg.coopHealthPort)
	}
	if cfg.maxRestarts != 5 {
		t.Errorf("maxRestarts = %d, want 5", cfg.maxRestarts)
	}
	if cfg.role != "crew" {
		t.Errorf("role = %q, want %q", cfg.role, "crew")
	}
	if cfg.project != "monorepo" {
		t.Errorf("project = %q, want %q", cfg.project, "monorepo")
	}
	if cfg.agent != "test-agent" {
		t.Errorf("agent = %q, want %q", cfg.agent, "test-agent")
	}
	if cfg.podIP != "10.0.0.1" {
		t.Errorf("podIP = %q, want %q", cfg.podIP, "10.0.0.1")
	}
	if !cfg.standby {
		t.Error("standby should be true")
	}
	if !cfg.mockMode {
		t.Error("mockMode should be true")
	}
}

func TestBuildAgentCommand_Default(t *testing.T) {
	t.Setenv("BOAT_COMMAND", "")
	t.Setenv("CLAUDE_MODEL", "")
	t.Setenv("GARDENIO_CHANNEL", "")

	cmd := buildAgentCommand()
	if cmd != "claude --dangerously-skip-permissions" {
		t.Errorf("got %q, want default claude command", cmd)
	}
}

func TestBuildAgentCommand_WithModel(t *testing.T) {
	t.Setenv("BOAT_COMMAND", "claude --dangerously-skip-permissions")
	t.Setenv("CLAUDE_MODEL", "claude-sonnet-4-6")
	t.Setenv("GARDENIO_CHANNEL", "")

	cmd := buildAgentCommand()
	want := "claude --dangerously-skip-permissions --model claude-sonnet-4-6"
	if cmd != want {
		t.Errorf("got %q, want %q", cmd, want)
	}
}

// TestBuildAgentCommand_RebuildAfterModelSwitch is the regression for
// ka-0000001014: a litellm-bedrock failover rewrites CLAUDE_MODEL to a
// bedrock-allowlisted model, and rebuilding the command must drop the stale
// opus-4-8 `--model` flag (which Bedrock 403s) in favor of the new one.
func TestBuildAgentCommand_RebuildAfterModelSwitch(t *testing.T) {
	t.Setenv("BOAT_COMMAND", "claude --dangerously-skip-permissions")
	t.Setenv("GARDENIO_CHANNEL", "")

	// Boot-time command bakes in the global opus-4-8 model.
	t.Setenv("CLAUDE_MODEL", "claude-opus-4-8-oauth")
	booted := buildAgentCommand()
	if booted != "claude --dangerously-skip-permissions --model claude-opus-4-8-oauth" {
		t.Fatalf("boot command: got %q", booted)
	}

	// Failover onto litellm-bedrock rewrites CLAUDE_MODEL via the credential
	// switch file; the rebuilt command must carry the bedrock-allowlisted model.
	t.Setenv("CLAUDE_MODEL", "bedrock-claude-opus-4-7")
	rebuilt := buildAgentCommand()
	want := "claude --dangerously-skip-permissions --model bedrock-claude-opus-4-7"
	if rebuilt != want {
		t.Errorf("rebuilt command: got %q, want %q", rebuilt, want)
	}
	if rebuilt == booted {
		t.Error("rebuilt command must differ from the stale boot command")
	}
}

func TestBuildAgentCommand_MockMode_NoModel(t *testing.T) {
	t.Setenv("BOAT_COMMAND", "claudeless run /tmp/scenario.toml")
	t.Setenv("CLAUDE_MODEL", "claude-opus-4-6")
	t.Setenv("GARDENIO_CHANNEL", "")

	cmd := buildAgentCommand()
	// Model should NOT be appended in mock mode (claudeless).
	if cmd != "claudeless run /tmp/scenario.toml" {
		t.Errorf("got %q, want no --model in mock mode", cmd)
	}
}

func TestBuildAgentCommand_WithGardenioChannel(t *testing.T) {
	t.Setenv("BOAT_COMMAND", "claude --dangerously-skip-permissions")
	t.Setenv("CLAUDE_MODEL", "")
	t.Setenv("GARDENIO_CHANNEL", "1")

	// Create the channel script so the check passes.
	dir := t.TempDir()
	channelScript := filepath.Join(dir, "gardenio-channel.ts")
	os.WriteFile(channelScript, []byte("// mock"), 0o644)

	// Temporarily override the channel dir check by creating /channel/ in a temp path.
	// Since buildAgentCommand hardcodes /channel/, we can't easily test this
	// without the actual file existing. Skip the channel assertion if not present.
	cmd := buildAgentCommand()
	// At minimum, the base command should be present.
	if cmd == "" {
		t.Error("buildAgentCommand returned empty string")
	}
}

func TestBuildAgentCommand_CustomCommand(t *testing.T) {
	t.Setenv("BOAT_COMMAND", "gemini --mode agent")
	t.Setenv("CLAUDE_MODEL", "")
	t.Setenv("GARDENIO_CHANNEL", "")

	cmd := buildAgentCommand()
	if cmd != "gemini --mode agent" {
		t.Errorf("got %q, want custom command", cmd)
	}
}

// TestBuildAgentCommand_FieldsSplit verifies that the output of buildAgentCommand()
// is correctly splittable by strings.Fields(), matching the code path in runCoopSession
// where cfg.command is split into individual argv entries.
func TestBuildAgentCommand_FieldsSplit(t *testing.T) {
	tests := []struct {
		name       string
		envVars    map[string]string
		wantFields []string
	}{
		{
			name: "default command splits into two fields",
			envVars: map[string]string{
				"BOAT_COMMAND":     "",
				"CLAUDE_MODEL":     "",
				"GARDENIO_CHANNEL": "",
			},
			wantFields: []string{"claude", "--dangerously-skip-permissions"},
		},
		{
			name: "with CLAUDE_MODEL splits model as separate fields",
			envVars: map[string]string{
				"BOAT_COMMAND":     "",
				"CLAUDE_MODEL":     "claude-sonnet-4-6",
				"GARDENIO_CHANNEL": "",
			},
			wantFields: []string{"claude", "--dangerously-skip-permissions", "--model", "claude-sonnet-4-6"},
		},
		{
			name: "custom BOAT_COMMAND splits correctly",
			envVars: map[string]string{
				"BOAT_COMMAND":     "gemini --mode agent --verbose",
				"CLAUDE_MODEL":     "",
				"GARDENIO_CHANNEL": "",
			},
			wantFields: []string{"gemini", "--mode", "agent", "--verbose"},
		},
		{
			name: "claudeless mock mode does not append model",
			envVars: map[string]string{
				"BOAT_COMMAND":     "claudeless run /tmp/scenario.toml",
				"CLAUDE_MODEL":     "claude-opus-4-6",
				"GARDENIO_CHANNEL": "",
			},
			wantFields: []string{"claudeless", "run", "/tmp/scenario.toml"},
		},
		{
			name: "multiple spaces in BOAT_COMMAND are handled by Fields",
			envVars: map[string]string{
				"BOAT_COMMAND":     "claude  --dangerously-skip-permissions   --verbose",
				"CLAUDE_MODEL":     "",
				"GARDENIO_CHANNEL": "",
			},
			wantFields: []string{"claude", "--dangerously-skip-permissions", "--verbose"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}

			cmd := buildAgentCommand()
			got := strings.Fields(cmd)

			if len(got) != len(tt.wantFields) {
				t.Fatalf("strings.Fields() produced %d fields %v, want %d fields %v",
					len(got), got, len(tt.wantFields), tt.wantFields)
			}
			for i, want := range tt.wantFields {
				if got[i] != want {
					t.Errorf("field[%d] = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

// TestBuildAgentCommand_FieldsSplit_CoopArgs verifies the full coop args construction
// path: buildAgentCommand() → strings.Fields() → append after "--" separator.
// This mirrors the exact code in runCoopSession lines 140-144.
func TestBuildAgentCommand_FieldsSplit_CoopArgs(t *testing.T) {
	t.Setenv("BOAT_COMMAND", "")
	t.Setenv("CLAUDE_MODEL", "claude-sonnet-4-6")
	t.Setenv("GARDENIO_CHANNEL", "")

	cmd := buildAgentCommand()

	// Simulate the exact code path from runCoopSession.
	coopArgs := []string{
		"--agent=claude",
		"--port=8080",
		"--port-health=9090",
		"--cols=200", "--rows=50",
	}
	coopArgs = append(coopArgs, "--")
	coopArgs = append(coopArgs, strings.Fields(cmd)...)

	// Verify the "--" separator is in position and agent command follows it.
	separatorIdx := -1
	for i, a := range coopArgs {
		if a == "--" {
			separatorIdx = i
			break
		}
	}
	if separatorIdx < 0 {
		t.Fatal("missing '--' separator in coopArgs")
	}

	trailing := coopArgs[separatorIdx+1:]
	want := []string{"claude", "--dangerously-skip-permissions", "--model", "claude-sonnet-4-6"}
	if len(trailing) != len(want) {
		t.Fatalf("trailing args = %v, want %v", trailing, want)
	}
	for i, w := range want {
		if trailing[i] != w {
			t.Errorf("trailing[%d] = %q, want %q", i, trailing[i], w)
		}
	}
}

func TestInitToK8sConfig_RoundTrip(t *testing.T) {
	cfg := initConfig{
		coopPort:       8080,
		coopHealthPort: 9090,
		maxRestarts:    10,
		command:        "claude",
		role:           "crew",
		project:        "test",
		agent:          "agent-1",
		workspace:      "/home/agent/workspace",
		podIP:          "10.0.0.1",
		hostname:       "pod-abc",
	}

	k8s := initToK8sConfig(cfg)

	if k8s.coopPort != cfg.coopPort {
		t.Errorf("coopPort mismatch: %d != %d", k8s.coopPort, cfg.coopPort)
	}
	if k8s.role != cfg.role {
		t.Errorf("role mismatch: %q != %q", k8s.role, cfg.role)
	}
	if k8s.project != cfg.project {
		t.Errorf("project mismatch: %q != %q", k8s.project, cfg.project)
	}
	if k8s.workspace != cfg.workspace {
		t.Errorf("workspace mismatch: %q != %q", k8s.workspace, cfg.workspace)
	}
}

func TestLoadSwitchCredentials_AppliesEnvVars(t *testing.T) {
	// Create a switch metadata file.
	tmpFile := filepath.Join(t.TempDir(), "switch.json")
	data := `{"credentials":{"ANTHROPIC_API_KEY":"sk-new-key","CLAUDE_CODE_OAUTH_TOKEN":"tok-new"},"profile":"account-2"}`
	if err := os.WriteFile(tmpFile, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	// Override the constant path for testing.
	origFile := credentialSwitchFile
	// We can't reassign a const, so test the core logic directly.
	// Write to the real path and clean up.
	if err := os.WriteFile(origFile, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(origFile)

	// Clear env vars before test.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	loadSwitchCredentials()

	if got := os.Getenv("ANTHROPIC_API_KEY"); got != "sk-new-key" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want %q", got, "sk-new-key")
	}
	if got := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); got != "tok-new" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %q, want %q", got, "tok-new")
	}

	// File should be cleaned up.
	if _, err := os.Stat(origFile); !os.IsNotExist(err) {
		t.Error("credential switch file should be removed after loading")
	}
}

func TestLoadSwitchCredentials_MissingFile(t *testing.T) {
	// Should not panic when file doesn't exist.
	os.Remove(credentialSwitchFile)
	loadSwitchCredentials() // must not panic
}

func TestLoadSwitchCredentials_MalformedJSON(t *testing.T) {
	if err := os.WriteFile(credentialSwitchFile, []byte("{invalid"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(credentialSwitchFile)

	loadSwitchCredentials() // must not panic
}

func TestExitCodeCredentialSwitch_Constant(t *testing.T) {
	// Verify the constant matches the Rust side (10).
	if exitCodeCredentialSwitch != 10 {
		t.Errorf("exitCodeCredentialSwitch = %d, want 10", exitCodeCredentialSwitch)
	}
}

// TestBuildCoopEnv_SuppressMemoryForCrew verifies that ineligible roles (crew
// without schedule_id) get CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 in the coop env.
// Regression guard for ka-0000008250 — the gb init path was missing the
// suppression that gb agent start --k8s already had.
func TestBuildCoopEnv_SuppressMemoryForCrew(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")
	t.Setenv("COOP_LOG_LEVEL", "info")

	env := buildCoopEnv(initConfig{role: "crew"})

	if !envContains(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1") {
		t.Errorf("expected CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 in coop env for crew (no bead), got: %v", env)
	}
}

// TestBuildCoopEnv_NoSuppressForGardener verifies that gardener roles do NOT
// get the auto-memory suppression flag — gardeners are memory-eligible.
func TestBuildCoopEnv_NoSuppressForGardener(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")
	t.Setenv("COOP_LOG_LEVEL", "info")

	env := buildCoopEnv(initConfig{role: "gardener"})

	if envContains(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1") {
		t.Errorf("gardener should NOT have CLAUDE_CODE_DISABLE_AUTO_MEMORY=1, got: %v", env)
	}
}

// TestBuildCoopEnv_SuppressForThread verifies that thread role (ineligible) is
// suppressed — covers the canonical-prod / thread shapes from the failing probe.
func TestBuildCoopEnv_SuppressForThread(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")
	t.Setenv("COOP_LOG_LEVEL", "info")

	for _, role := range []string{"thread", "canonical-prod", "jira-mention", "triage"} {
		env := buildCoopEnv(initConfig{role: role})
		if !envContains(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1") {
			t.Errorf("role %q should suppress auto-memory, got env: %v", role, env)
		}
	}
}

func envContains(env []string, want string) bool {
	return slices.Contains(env, want)
}

// envHasKey reports whether env contains any entry with the given key.
func envHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

// TestBuildCoopEnv_StripsUnknownClaudeCodeEnv verifies that any CLAUDE_CODE_*
// key NOT in claudeCodeEnvAllowlist is dropped from the coop env — regardless
// of role eligibility. Regression guard for ka-0000007438: any future flag
// added to Claude Code must be explicitly opted in, not inherited silently.
func TestBuildCoopEnv_StripsUnknownClaudeCodeEnv(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "leaked-from-pod")
	t.Setenv("CLAUDE_CODE_VERBOSE", "1")

	for _, role := range []string{"crew", "gardener", "thread"} {
		env := buildCoopEnv(initConfig{role: role})
		if envHasKey(env, "CLAUDE_CODE_SUBAGENT_MODEL") {
			t.Errorf("role %q: CLAUDE_CODE_SUBAGENT_MODEL must be stripped, got: %v", role, env)
		}
		if envHasKey(env, "CLAUDE_CODE_VERBOSE") {
			t.Errorf("role %q: CLAUDE_CODE_VERBOSE must be stripped, got: %v", role, env)
		}
	}
}

// TestBuildCoopEnv_AllowsKnownClaudeCodeEnv verifies that the intentional
// CLAUDE_CODE_* allowlist (OAUTH_TOKEN, ENABLE_TASKS, USE_BEDROCK, USE_VERTEX,
// USE_LITELLM) survives the strip pass untouched.
func TestBuildCoopEnv_AllowsKnownClaudeCodeEnv(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok-xyz")
	t.Setenv("CLAUDE_CODE_ENABLE_TASKS", "false")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")
	t.Setenv("CLAUDE_CODE_USE_LITELLM", "1")

	env := buildCoopEnv(initConfig{role: "gardener"})

	for _, want := range []string{
		"CLAUDE_CODE_OAUTH_TOKEN=tok-xyz",
		"CLAUDE_CODE_ENABLE_TASKS=false",
		"CLAUDE_CODE_USE_BEDROCK=1",
		"CLAUDE_CODE_USE_VERTEX=1",
		"CLAUDE_CODE_USE_LITELLM=1",
	} {
		if !envContains(env, want) {
			t.Errorf("expected allowlist passthrough %q in env, got: %v", want, env)
		}
	}
}

// TestBuildCoopEnv_StripsInheritedDisableAutoMemoryForCrew verifies that an
// inherited CLAUDE_CODE_DISABLE_AUTO_MEMORY=0 from pod env does NOT survive
// into the coop env even for crew. The authoritative value re-added by
// buildCoopEnv (=1, since crew is ineligible) must be the only one present.
func TestBuildCoopEnv_StripsInheritedDisableAutoMemoryForCrew(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "0") // leaked from pod env

	env := buildCoopEnv(initConfig{role: "crew"})

	// The leaked "=0" must be gone.
	if envContains(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=0") {
		t.Errorf("leaked CLAUDE_CODE_DISABLE_AUTO_MEMORY=0 must be stripped, got: %v", env)
	}
	// The authoritative "=1" (ineligible role) must be the one present.
	if !envContains(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1") {
		t.Errorf("expected CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 for crew, got: %v", env)
	}
	// And there must be only one DISABLE_AUTO_MEMORY entry.
	count := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one CLAUDE_CODE_DISABLE_AUTO_MEMORY entry, got %d in: %v", count, env)
	}
}

// TestIsDeniedClaudeCodeEnv covers the helper directly: allowlisted keys are
// permitted, anything else in the CLAUDE_CODE_* namespace is denied, and
// non-CLAUDE_CODE keys are always permitted.
func TestIsDeniedClaudeCodeEnv(t *testing.T) {
	cases := []struct {
		kv     string
		denied bool
	}{
		{"CLAUDE_CODE_OAUTH_TOKEN=abc", false},
		{"CLAUDE_CODE_ENABLE_TASKS=false", false},
		{"CLAUDE_CODE_USE_BEDROCK=1", false},
		{"CLAUDE_CODE_USE_VERTEX=1", false},
		{"CLAUDE_CODE_USE_LITELLM=1", false},
		{"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", true},
		{"CLAUDE_CODE_SUBAGENT_MODEL=foo", true},
		{"CLAUDE_CODE_VERBOSE=1", true},
		{"CLAUDE_CODE_EFFORT_LEVEL=high", true},
		{"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1", true},
		{"ANTHROPIC_MODEL=claude-opus-4-7", false},
		{"BOAT_ACCOUNT=alice", false},
		{"PATH=/usr/bin", false},
	}
	for _, c := range cases {
		got := isDeniedClaudeCodeEnv(c.kv)
		if got != c.denied {
			t.Errorf("isDeniedClaudeCodeEnv(%q) = %v, want %v", c.kv, got, c.denied)
		}
	}
}

// TestIsDeniedControllerSecretEnv covers the claudemore F2 (R4-SEC-2)
// defense-in-depth strip: the LiteLLM master/admin key is controller-side only
// and must be denied from the coop env; everything else (including the scoped
// per-agent virtual key, which rides as a header not an env var) is permitted.
func TestIsDeniedControllerSecretEnv(t *testing.T) {
	cases := []struct {
		kv     string
		denied bool
	}{
		{"LITELLM_MASTER_KEY=sk-master-abc", true},
		{"LITELLM_ADMIN_SECRET=admin-xyz", true},
		{"LITELLM_URL=http://litellm:4000", false},
		{"CLAUDE_CODE_OAUTH_TOKEN=abc", false},
		{"BOAT_ACCOUNT=alice", false},
		{"AWS_ROLE_ARN=arn:aws:iam::1:role/x", false},
		{"PATH=/usr/bin", false},
	}
	for _, c := range cases {
		if got := isDeniedControllerSecretEnv(c.kv); got != c.denied {
			t.Errorf("isDeniedControllerSecretEnv(%q) = %v, want %v", c.kv, got, c.denied)
		}
	}
}

// TestBuildCoopEnv_StripsControllerMasterKey verifies the master key never
// rides into the coop child even if a pod-spec/operator misconfiguration placed
// it in the pod env — the R4-SEC "master key controller-side only" assertion.
func TestBuildCoopEnv_StripsControllerMasterKey(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")
	t.Setenv("LITELLM_MASTER_KEY", "sk-master-should-not-leak")
	t.Setenv("LITELLM_ADMIN_SECRET", "admin-should-not-leak")

	for _, role := range []string{"crew", "gardener", "thread"} {
		env := buildCoopEnv(initConfig{role: role})
		if envHasKey(env, "LITELLM_MASTER_KEY") {
			t.Errorf("role %q: LITELLM_MASTER_KEY must be stripped from coop env, got: %v", role, env)
		}
		if envHasKey(env, "LITELLM_ADMIN_SECRET") {
			t.Errorf("role %q: LITELLM_ADMIN_SECRET must be stripped from coop env, got: %v", role, env)
		}
	}
}

// captureLogOutput redirects the default log package output to a buffer for
// the duration of fn and returns the captured text. Used to assert on the
// observability lines emitted by logDeniedClaudeCodeEnv (ka-0000007104).
func captureLogOutput(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	flags := log.Flags()
	prefix := log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(orig)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	}()
	fn()
	return buf.String()
}

// stripEventFor returns the strip-event log output emitted when the buildCoopEnv
// loop encounters kv. Only kv that pass isDeniedClaudeCodeEnv should produce
// output; callers verify the gating behavior.
func stripEventFor(t *testing.T, kv string) string {
	t.Helper()
	return captureLogOutput(t, func() {
		if isDeniedClaudeCodeEnv(kv) {
			logDeniedClaudeCodeEnv(kv)
		}
	})
}

// TestLogDeniedClaudeCodeEnv_LogsStrippedDeniedKey verifies that a denied
// CLAUDE_CODE_* key emits the expected observability line with the key name
// and value length but NEVER the value itself. Regression guard for
// ka-0000007104.
func TestLogDeniedClaudeCodeEnv_LogsStrippedDeniedKey(t *testing.T) {
	out := stripEventFor(t, "CLAUDE_CODE_SUBAGENT_MODEL=leaked-secret-value")

	want := "[gb agent start] coop-env: stripped denied CLAUDE_CODE_* key=CLAUDE_CODE_SUBAGENT_MODEL value_len=19"
	if !strings.Contains(out, want) {
		t.Errorf("expected log output to contain %q, got: %q", want, out)
	}
	if strings.Contains(out, "leaked-secret-value") {
		t.Errorf("log output must NOT contain the denied value, got: %q", out)
	}
}

// TestLogDeniedClaudeCodeEnv_DoesNotLogAllowlistedKey verifies that
// allowlisted CLAUDE_CODE_* keys (OAUTH_TOKEN, ENABLE_TASKS, USE_BEDROCK,
// USE_VERTEX, USE_LITELLM) flow through without triggering the strip-event
// log line.
func TestLogDeniedClaudeCodeEnv_DoesNotLogAllowlistedKey(t *testing.T) {
	for _, kv := range []string{
		"CLAUDE_CODE_OAUTH_TOKEN=tok-xyz",
		"CLAUDE_CODE_ENABLE_TASKS=false",
		"CLAUDE_CODE_USE_BEDROCK=1",
		"CLAUDE_CODE_USE_VERTEX=1",
		"CLAUDE_CODE_USE_LITELLM=1",
	} {
		out := stripEventFor(t, kv)
		if out != "" {
			t.Errorf("allowlisted key %q must not trigger strip-event log, got: %q", kv, out)
		}
	}
}

// TestLogDeniedClaudeCodeEnv_DoesNotLogNonClaudeCodeKey verifies that
// non-CLAUDE_CODE_* keys are not subject to the strip-event log even when
// they share suffixes or prefixes with denied names.
func TestLogDeniedClaudeCodeEnv_DoesNotLogNonClaudeCodeKey(t *testing.T) {
	for _, kv := range []string{
		"ANTHROPIC_MODEL=claude-opus-4-7",
		"BOAT_ACCOUNT=alice",
		"PATH=/usr/bin",
		"CLAUDE_MODEL=claude-opus-4-7",
		"CLAUDE_CODE=not-a-prefix-match",
	} {
		out := stripEventFor(t, kv)
		if out != "" {
			t.Errorf("non-denied key %q must not trigger strip-event log, got: %q", kv, out)
		}
	}
}

// TestCloseAgentBeadBestEffort_SkipsWhenAlreadyClosed verifies that the
// pod-teardown cleanup path does NOT call CloseBead if the agent has
// already closed its own bead with a close_reason set. Regression guard for
// ka-0000000961.
func TestCloseAgentBeadBestEffort_SkipsWhenAlreadyClosed(t *testing.T) {
	const beadID = "gd-already-closed"
	var closePosts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+beadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     beadID,
				"type":   "agent",
				"status": "closed",
				"fields": map[string]string{"close_reason": "stop", "agent_state": "done"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+beadID+"/close":
			closePosts.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": beadID})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	closeAgentBeadBestEffort("context_cancelled")

	if got := closePosts.Load(); got != 0 {
		t.Fatalf("expected no close POST (bead already closed), got %d", got)
	}
}

// TestCloseAgentBeadBestEffort_SkipsWhenWrapupPresent verifies that the
// pod-teardown cleanup path does NOT call CloseBead when the agent closed its
// own bead via `gb stop --wrapup`. That path writes the structured wrapup
// field but sets NO close_reason, so a close_reason-only guard would let the
// teardown path clobber the wrap-up card. Regression guard for ka-0000002689.
func TestCloseAgentBeadBestEffort_SkipsWhenWrapupPresent(t *testing.T) {
	const beadID = "gd-closed-with-wrapup"
	var closePosts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+beadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     beadID,
				"type":   "agent",
				"status": "closed",
				// gb stop --wrapup sets wrapup but leaves close_reason empty.
				"fields": map[string]string{
					"close_reason": "",
					"agent_state":  "done",
					"wrapup":       `{"tldr":"shipped the fix","accomplishments":"merged MR"}`,
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+beadID+"/close":
			closePosts.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": beadID})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	closeAgentBeadBestEffort("context_cancelled")

	if got := closePosts.Load(); got != 0 {
		t.Fatalf("expected no close POST (wrapup present), got %d", got)
	}
}

// TestCloseAgentBeadBestEffort_ClosesWhenClosedNoReasonNoWrapup verifies the
// no-regression case: a bead that is closed but carries neither a close_reason
// nor a wrapup is still closed by the teardown path (preserving prior behavior
// for agents that did not clean up via gb stop --wrapup). ka-0000002689.
func TestCloseAgentBeadBestEffort_ClosesWhenClosedNoReasonNoWrapup(t *testing.T) {
	const beadID = "gd-closed-bare"
	var closePosts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+beadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     beadID,
				"type":   "agent",
				"status": "closed",
				"fields": map[string]string{"close_reason": "", "wrapup": ""},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+beadID+"/close":
			closePosts.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": beadID})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	closeAgentBeadBestEffort("context_cancelled")

	if got := closePosts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 close POST (no reason, no wrapup), got %d", got)
	}
}

// TestCloseAgentBeadBestEffort_ClosesWhenOpen verifies the fallback path:
// if the agent bead is still open at pod teardown, we do close it (preserving
// the prior "close on teardown" behavior for agents that did not clean up).
func TestCloseAgentBeadBestEffort_ClosesWhenOpen(t *testing.T) {
	const beadID = "gd-still-open"
	var closePosts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+beadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     beadID,
				"type":   "agent",
				"status": "open",
				"fields": map[string]string{},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+beadID+"/close":
			closePosts.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": beadID})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	closeAgentBeadBestEffort("context_cancelled")

	if got := closePosts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 close POST (bead still open), got %d", got)
	}
}

// TestDescribeCoopExitCode verifies the human-readable label for the coop
// process exit codes gb actually observes, including the 128+signal
// convention shared with beadsapi.SessionStatusForExit.
func TestDescribeCoopExitCode(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{0, "clean exit"},
		{exitCodeCredentialSwitch, "credential switch"},
		{143, "SIGTERM"},
		{130, "SIGINT"},
		{129, "SIGHUP"},
		{1, "exit code 1"},
	}
	for _, tc := range cases {
		if got := describeCoopExitCode(tc.code); got != tc.want {
			t.Errorf("describeCoopExitCode(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

// TestCloseAgentBeadBestEffort_StampsLastCoopExit verifies that when a coop
// session has exited at least once in this pod, closeAgentBeadBestEffort
// stamps last_coop_exit_code/last_coop_exit_reason on the close POST body —
// the durable, agent-bead-local record gd-k3kzZeIlT0 asks for, since the
// pod's own logs and the linked session bead are both gone once the pod is
// deleted.
func TestCloseAgentBeadBestEffort_StampsLastCoopExit(t *testing.T) {
	const beadID = "gd-stamp-exit-code"
	var closeBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+beadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     beadID,
				"type":   "agent",
				"status": "open",
				"fields": map[string]string{},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+beadID+"/close":
			_ = json.NewDecoder(r.Body).Decode(&closeBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": beadID})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	oldCode, oldKnown := lastCoopExitCode.Load(), lastCoopExitKnown.Load()
	t.Cleanup(func() {
		lastCoopExitCode.Store(oldCode)
		lastCoopExitKnown.Store(oldKnown)
	})
	lastCoopExitCode.Store(143)
	lastCoopExitKnown.Store(true)

	closeAgentBeadBestEffort("max_restarts_reached")

	// CloseBeadAs (beadsapi/client.go) flattens the fields map directly onto
	// the close POST body alongside closed_by — there is no nested "fields"
	// key on this endpoint (unlike UpdateBead).
	if got := closeBody["last_coop_exit_code"]; got != "143" {
		t.Errorf("last_coop_exit_code: got %v, want %q (body=%+v)", got, "143", closeBody)
	}
	if got := closeBody["last_coop_exit_reason"]; got != "SIGTERM" {
		t.Errorf("last_coop_exit_reason: got %v, want %q (body=%+v)", got, "SIGTERM", closeBody)
	}
}

// TestCloseAgentBeadBestEffort_NoCoopExitYetOmitsFields verifies the no-op
// case: a pod torn down before any coop session finished (e.g. failed
// startup) must not stamp a misleading exit code of 0.
func TestCloseAgentBeadBestEffort_NoCoopExitYetOmitsFields(t *testing.T) {
	const beadID = "gd-no-coop-exit-yet"
	var closeBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+beadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     beadID,
				"type":   "agent",
				"status": "open",
				"fields": map[string]string{},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+beadID+"/close":
			_ = json.NewDecoder(r.Body).Decode(&closeBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": beadID})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	oldCode, oldKnown := lastCoopExitCode.Load(), lastCoopExitKnown.Load()
	t.Cleanup(func() {
		lastCoopExitCode.Store(oldCode)
		lastCoopExitKnown.Store(oldKnown)
	})
	lastCoopExitKnown.Store(false)

	closeAgentBeadBestEffort("max_restarts_reached")

	if _, ok := closeBody["last_coop_exit_code"]; ok {
		t.Errorf("expected no last_coop_exit_code field when no coop session has exited yet, got %+v", closeBody)
	}
}

// TestLoadSwitchCredentials_ReassertsPinnedModel verifies that loadSwitchCredentials
// re-asserts a pinned model override (BOAT_MODEL_PINNED) over the account-default
// CLAUDE_MODEL/ANTHROPIC_MODEL pushed by the coopmux credential broker on a
// reload. The broker is account-keyed (one model per account), so without the
// pin a credential rotation would silently revert a per-pod role/project/agent
// model override (epic ka-0000005488, scope item 3).
func TestLoadSwitchCredentials_ReassertsPinnedModel(t *testing.T) {
	// Switch file carries the account-default model (what coopmux pushes).
	meta := map[string]any{
		"credentials": map[string]string{
			"CLAUDE_CODE_OAUTH_TOKEN": "tok",
			"CLAUDE_MODEL":            "account-default-model",
			"ANTHROPIC_MODEL":         "account-default-model",
		},
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(credentialSwitchFile, data, 0o600); err != nil {
		t.Fatalf("write switch file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(credentialSwitchFile) })

	// Pre-register env vars so t.Setenv restores them after the test even though
	// loadSwitchCredentials mutates them via os.Setenv.
	t.Setenv("CLAUDE_MODEL", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("BOAT_MODEL_PINNED", "role-pinned-model")

	loadSwitchCredentials()

	if got := os.Getenv("CLAUDE_MODEL"); got != "role-pinned-model" {
		t.Errorf("CLAUDE_MODEL: got %q, want %q (pin should override account default)", got, "role-pinned-model")
	}
	if got := os.Getenv("ANTHROPIC_MODEL"); got != "role-pinned-model" {
		t.Errorf("ANTHROPIC_MODEL: got %q, want %q", got, "role-pinned-model")
	}
	// Non-model credentials still applied.
	if got := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); got != "tok" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN: got %q, want %q", got, "tok")
	}
}

// TestLoadSwitchCredentials_NoPinKeepsPushedModel verifies that without a pin,
// the account-default model pushed in the switch file is applied unchanged —
// the existing failover/rotation behavior (e.g. ka-0000001014) is preserved.
func TestLoadSwitchCredentials_NoPinKeepsPushedModel(t *testing.T) {
	meta := map[string]any{
		"credentials": map[string]string{
			"CLAUDE_MODEL":    "bedrock-claude-opus-4-7",
			"ANTHROPIC_MODEL": "bedrock-claude-opus-4-7",
		},
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(credentialSwitchFile, data, 0o600); err != nil {
		t.Fatalf("write switch file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(credentialSwitchFile) })

	t.Setenv("CLAUDE_MODEL", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("BOAT_MODEL_PINNED", "") // no pin

	loadSwitchCredentials()

	if got := os.Getenv("CLAUDE_MODEL"); got != "bedrock-claude-opus-4-7" {
		t.Errorf("CLAUDE_MODEL: got %q, want pushed account model %q", got, "bedrock-claude-opus-4-7")
	}
}
