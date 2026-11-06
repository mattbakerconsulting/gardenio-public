// Unit tests for the jira-bridge entry point helpers. main() itself bootstraps
// a full bridge (beads client, HTTP server, poller, SSE stream) and is not
// practically testable in isolation; these tests cover the pure-Go helpers
// that parse environment configuration — envOrDefault, splitCSV,
// parseBoatProjects, parseConfig, and setupLogger — which is the bulk of the
// branch-carrying logic outside of runtime wiring.
package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

func TestEnvOrDefault(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		setValue string
		set      bool
		fallback string
		want     string
	}{
		{"unset returns fallback", "JIRA_TEST_A", "", false, "default", "default"},
		{"empty returns fallback", "JIRA_TEST_B", "", true, "default", "default"},
		{"set returns value", "JIRA_TEST_C", "hello", true, "default", "hello"},
		{"whitespace value preserved", "JIRA_TEST_D", " spaces ", true, "default", " spaces "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv(tt.key, tt.setValue)
			}
			got := envOrDefault(tt.key, tt.fallback)
			if got != tt.want {
				t.Errorf("envOrDefault(%q, %q) = %q, want %q", tt.key, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestSplitCSV(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"empty", "", nil},
		{"single", "foo", []string{"foo"}},
		{"multiple", "foo,bar,baz", []string{"foo", "bar", "baz"}},
		{"trim spaces", " foo , bar , baz ", []string{"foo", "bar", "baz"}},
		{"empty entries skipped", "foo,,bar,,,baz", []string{"foo", "bar", "baz"}},
		{"only commas yields empty result", ",,,", []string{}},
		{"only whitespace yields empty result", "   ", []string{}},
		{"trailing comma", "foo,bar,", []string{"foo", "bar"}},
		{"leading comma", ",foo,bar", []string{"foo", "bar"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitCSV(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitCSV(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

func TestUnionProjects(t *testing.T) {
	tests := []struct {
		name  string
		a     []string
		extra []string
		want  []string
	}{
		{"empty extra is a copy of a", []string{"PE", "DEVOPS"}, nil, []string{"PE", "DEVOPS"}},
		{"disjoint appended in order", []string{"PE", "DEVOPS"}, []string{"DOSE", "FSM"}, []string{"PE", "DEVOPS", "DOSE", "FSM"}},
		{"overlap de-duped, first-seen order", []string{"PE", "DEVOPS"}, []string{"DEVOPS", "DOSE"}, []string{"PE", "DEVOPS", "DOSE"}},
		{"blank entries skipped", []string{"PE"}, []string{"", "  ", "DOSE"}, []string{"PE", "DOSE"}},
		{"trims whitespace", []string{" PE "}, []string{"DOSE "}, []string{"PE", "DOSE"}},
		{"both empty yields empty", nil, nil, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unionProjects(tt.a, tt.extra)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("unionProjects(%#v, %#v) = %#v, want %#v", tt.a, tt.extra, got, tt.want)
			}
		})
	}
}

func TestParseCustomFieldsEnv(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  map[string]string
	}{
		{"empty", "", nil},
		{"single", "customfield_11976:jira_issue_url", map[string]string{"customfield_11976": "jira_issue_url"}},
		{"multiple with spaces", " customfield_11976:jira_issue_url , customfield_5:jira_env ", map[string]string{"customfield_11976": "jira_issue_url", "customfield_5": "jira_env"}},
		{"skips malformed", "customfield_1:a,nopair,:nofield,noid:", map[string]string{"customfield_1": "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCustomFieldsEnv(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseCustomFieldsEnv(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseBoatProjects(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  map[string]string
	}{
		{
			name:  "empty",
			input: "",
			want:  map[string]string{},
		},
		{
			name:  "single entry",
			input: "gardenio=https://github.com/org/gardenio.git:kd",
			want:  map[string]string{"KD": "gardenio"},
		},
		{
			name:  "multiple entries",
			input: "gardenio=https://github.com/org/gardenio.git:kd,monorepo=https://gitlab.com/org/repo:PE",
			want:  map[string]string{"KD": "gardenio", "PE": "monorepo"},
		},
		{
			name:  "prefix uppercased",
			input: "svc=https://example.com/repo:AbCd",
			want:  map[string]string{"ABCD": "svc"},
		},
		{
			name:  "prefix trimmed",
			input: "svc=https://example.com/repo:  pe  ",
			want:  map[string]string{"PE": "svc"},
		},
		{
			name:  "entries without '=' skipped",
			input: "gardenio=url:kd,noeq,other=url:pe",
			want:  map[string]string{"KD": "gardenio", "PE": "other"},
		},
		{
			name:  "entries without ':' skipped",
			input: "gardenio=url:kd,nocolon=plainurl,other=url:pe",
			want:  map[string]string{"KD": "gardenio", "PE": "other"},
		},
		{
			name:  "empty entries ignored",
			input: ",gardenio=url:kd,,",
			want:  map[string]string{"KD": "gardenio"},
		},
		{
			name:  "blank prefix ignored",
			input: "gardenio=url:",
			want:  map[string]string{},
		},
		{
			name:  "uses last colon for prefix",
			input: "svc=https://example.com:443/repo:PE",
			want:  map[string]string{"PE": "svc"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseBoatProjects(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseBoatProjects(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

func TestSetupLogger(t *testing.T) {
	tests := []struct {
		level   string
		enabled map[slog.Level]bool
	}{
		{"debug", map[slog.Level]bool{slog.LevelDebug: true, slog.LevelInfo: true, slog.LevelWarn: true, slog.LevelError: true}},
		{"info", map[slog.Level]bool{slog.LevelDebug: false, slog.LevelInfo: true, slog.LevelWarn: true, slog.LevelError: true}},
		{"warn", map[slog.Level]bool{slog.LevelDebug: false, slog.LevelInfo: false, slog.LevelWarn: true, slog.LevelError: true}},
		{"error", map[slog.Level]bool{slog.LevelDebug: false, slog.LevelInfo: false, slog.LevelWarn: false, slog.LevelError: true}},
		{"unknown-defaults-info", map[slog.Level]bool{slog.LevelDebug: false, slog.LevelInfo: true, slog.LevelWarn: true, slog.LevelError: true}},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			lg := setupLogger(tt.level)
			if lg == nil {
				t.Fatal("setupLogger returned nil")
			}
			ctx := context.Background()
			for lvl, want := range tt.enabled {
				if got := lg.Enabled(ctx, lvl); got != want {
					t.Errorf("level=%q Enabled(%v) = %v, want %v", tt.level, lvl, got, want)
				}
			}
		})
	}
}

// clearJiraEnv unsets every JIRA_* / BOAT_PROJECTS / LISTEN / STATE / LOG / BEADS
// env var parseConfig consumes so each test case starts from a known-clean slate.
func clearJiraEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"BEADS_HTTP_ADDR",
		"JIRA_BASE_URL", "JIRA_EMAIL", "JIRA_API_TOKEN",
		"JIRA_PROJECTS", "JIRA_SPAWN_PROJECTS", "JIRA_STATUSES", "JIRA_ISSUE_TYPES",
		"JIRA_FIX_VERSIONS", "JIRA_REQUIRED_LABELS", "JIRA_EXCLUDED_LABELS",
		"JIRA_FILTER_PROJECTS", "JIRA_CUSTOM_JQL",
		"BOAT_PROJECTS",
		"JIRA_POLL_INTERVAL", "JIRA_SKIP_ASSIGNED",
		"JIRA_DISABLE_TRANSITIONS", "JIRA_ENABLE_CLAIM_TRANSITION",
		"JIRA_BOT_ACCOUNT_ID",
		"JIRA_WEBHOOK_SECRET", "JIRA_WEBHOOK_DEV_MODE",
		"JIRA_MENTION_DISABLED", "JIRA_MENTION_TRIGGER",
		"JIRA_MENTION_BOT_TRIGGER", "JIRA_MENTION_PROJECT",
		"JIRA_LISTEN_ADDR", "LOG_LEVEL", "STATE_PATH",
		"METRICS_LISTEN_ADDR", "JIRA_METRICS_QUERIES", "JIRA_METRICS_INTERVAL",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

func TestParseConfig_Defaults(t *testing.T) {
	clearJiraEnv(t)

	cfg := parseConfig()

	if cfg.beadsHTTPAddr != "http://localhost:8080" {
		t.Errorf("beadsHTTPAddr = %q, want default", cfg.beadsHTTPAddr)
	}
	wantProjects := []string{"PE", "DEVOPS"}
	if !reflect.DeepEqual(cfg.jiraProjects, wantProjects) {
		t.Errorf("jiraProjects = %#v, want %#v", cfg.jiraProjects, wantProjects)
	}
	// JIRA_SPAWN_PROJECTS defaults empty ⇒ spawn set stays {PE,DEVOPS,GBFSM}
	// and the deadman scope stays the poller projects (ka-0000005656).
	if len(cfg.jiraSpawnProjects) != 0 {
		t.Errorf("jiraSpawnProjects = %#v, want empty by default", cfg.jiraSpawnProjects)
	}
	wantStatuses := []string{"To Do", "Ready for Development"}
	if !reflect.DeepEqual(cfg.jiraStatuses, wantStatuses) {
		t.Errorf("jiraStatuses = %#v, want %#v", cfg.jiraStatuses, wantStatuses)
	}
	wantTypes := []string{"Bug", "Task", "Story", "Epic"}
	if !reflect.DeepEqual(cfg.jiraIssueTypes, wantTypes) {
		t.Errorf("jiraIssueTypes = %#v, want %#v", cfg.jiraIssueTypes, wantTypes)
	}
	if cfg.jiraPollInterval != 60*time.Second {
		t.Errorf("jiraPollInterval = %v, want 60s", cfg.jiraPollInterval)
	}
	if cfg.jiraSkipAssigned {
		t.Error("jiraSkipAssigned = true, want false")
	}
	if cfg.jiraDisableTransitions {
		t.Error("jiraDisableTransitions = true, want false")
	}
	if cfg.jiraEnableClaimTransition {
		t.Error("jiraEnableClaimTransition = true, want false")
	}
	if cfg.jiraWebhookDevMode {
		t.Error("jiraWebhookDevMode = true, want false")
	}
	if cfg.jiraMentionDisabled {
		t.Error("jiraMentionDisabled = true, want false")
	}
	if cfg.jiraMentionTrigger != "@gardenio" {
		t.Errorf("jiraMentionTrigger = %q, want @gardenio", cfg.jiraMentionTrigger)
	}
	if cfg.jiraMentionProject != "gardenio" {
		t.Errorf("jiraMentionProject = %q, want gardenio", cfg.jiraMentionProject)
	}
	if cfg.listenAddr != ":8091" {
		t.Errorf("listenAddr = %q, want :8091", cfg.listenAddr)
	}
	if cfg.logLevel != "info" {
		t.Errorf("logLevel = %q, want info", cfg.logLevel)
	}
	if cfg.statePath != "/data/jira-bridge-state.json" {
		t.Errorf("statePath = %q, want /data/jira-bridge-state.json", cfg.statePath)
	}
	if cfg.metricsAddr != ":9093" {
		t.Errorf("metricsAddr = %q, want :9093", cfg.metricsAddr)
	}
	if cfg.metricsInterval != 120*time.Second {
		t.Errorf("metricsInterval = %v, want 120s", cfg.metricsInterval)
	}
	if len(cfg.metricsQueries) != 0 {
		t.Errorf("metricsQueries = %#v, want empty (collector disabled by default)", cfg.metricsQueries)
	}
	if cfg.ageInterval != 600*time.Second {
		t.Errorf("ageInterval = %v, want 600s", cfg.ageInterval)
	}
	if len(cfg.ageQueries) != 0 {
		t.Errorf("ageQueries = %#v, want empty (age collector disabled by default)", cfg.ageQueries)
	}
	if len(cfg.jiraFixVersions) != 0 || len(cfg.jiraRequiredLabels) != 0 ||
		len(cfg.jiraExcludedLabels) != 0 || len(cfg.jiraFilterProjects) != 0 {
		t.Errorf("expected empty optional filters, got: fix=%v required=%v excluded=%v filter=%v",
			cfg.jiraFixVersions, cfg.jiraRequiredLabels, cfg.jiraExcludedLabels, cfg.jiraFilterProjects)
	}
	if cfg.jiraCustomJQL != "" {
		t.Errorf("jiraCustomJQL = %q, want empty", cfg.jiraCustomJQL)
	}
	if len(cfg.jiraProjectMap) != 0 {
		t.Errorf("jiraProjectMap = %#v, want empty", cfg.jiraProjectMap)
	}
}

func TestParseConfig_Overrides(t *testing.T) {
	clearJiraEnv(t)
	t.Setenv("BEADS_HTTP_ADDR", "http://beads.example:9000")
	t.Setenv("JIRA_BASE_URL", "https://jira.example")
	t.Setenv("JIRA_EMAIL", "bot@example")
	t.Setenv("JIRA_API_TOKEN", "tok")
	t.Setenv("JIRA_PROJECTS", "FOO,BAR")
	t.Setenv("JIRA_SPAWN_PROJECTS", "BAZ, QUX")
	t.Setenv("JIRA_STATUSES", "Open")
	t.Setenv("JIRA_ISSUE_TYPES", "Bug")
	t.Setenv("JIRA_FIX_VERSIONS", "2.5.0")
	t.Setenv("JIRA_REQUIRED_LABELS", "approved")
	t.Setenv("JIRA_EXCLUDED_LABELS", "wip,hold")
	t.Setenv("JIRA_FILTER_PROJECTS", "FOO")
	t.Setenv("JIRA_CUSTOM_JQL", "labels in (urgent)")
	t.Setenv("BOAT_PROJECTS", "gardenio=url:KD")
	t.Setenv("JIRA_POLL_INTERVAL", "15s")
	t.Setenv("JIRA_SKIP_ASSIGNED", "true")
	t.Setenv("JIRA_DISABLE_TRANSITIONS", "1")
	t.Setenv("JIRA_ENABLE_CLAIM_TRANSITION", "true")
	t.Setenv("JIRA_BOT_ACCOUNT_ID", "acct-123")
	t.Setenv("JIRA_WEBHOOK_SECRET", "shh")
	t.Setenv("JIRA_WEBHOOK_DEV_MODE", "1")
	t.Setenv("JIRA_MENTION_DISABLED", "true")
	t.Setenv("JIRA_MENTION_TRIGGER", "@bot")
	t.Setenv("JIRA_MENTION_BOT_TRIGGER", "@gardenio-bot")
	t.Setenv("JIRA_MENTION_PROJECT", "gardenio")
	t.Setenv("JIRA_LISTEN_ADDR", ":9090")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("STATE_PATH", "/tmp/state.json")
	t.Setenv("METRICS_LISTEN_ADDR", ":9099")
	t.Setenv("JIRA_METRICS_INTERVAL", "300s")
	t.Setenv("JIRA_METRICS_QUERIES", `[{"name":"open_bugs","jql":"issuetype = Bug AND statusCategory != Done"},{"name":"by_pe","jql":"project = PE AND statusCategory != Done","project":"PE","interval":"600s"}]`)
	t.Setenv("JIRA_AGE_INTERVAL", "900s")
	t.Setenv("JIRA_AGE_QUERIES", `[{"name":"open_bugs_age","jql":"issuetype = Bug AND statusCategory != Done","project":"PE","interval":"600s","maxIssues":1500}]`)

	cfg := parseConfig()

	if cfg.beadsHTTPAddr != "http://beads.example:9000" {
		t.Errorf("beadsHTTPAddr = %q", cfg.beadsHTTPAddr)
	}
	if cfg.jiraBaseURL != "https://jira.example" {
		t.Errorf("jiraBaseURL = %q", cfg.jiraBaseURL)
	}
	if cfg.jiraEmail != "bot@example" {
		t.Errorf("jiraEmail = %q", cfg.jiraEmail)
	}
	if cfg.jiraAPIToken != "tok" {
		t.Errorf("jiraAPIToken = %q", cfg.jiraAPIToken)
	}
	if !reflect.DeepEqual(cfg.jiraProjects, []string{"FOO", "BAR"}) {
		t.Errorf("jiraProjects = %#v", cfg.jiraProjects)
	}
	if !reflect.DeepEqual(cfg.jiraSpawnProjects, []string{"BAZ", "QUX"}) {
		t.Errorf("jiraSpawnProjects = %#v, want [BAZ QUX]", cfg.jiraSpawnProjects)
	}
	if !reflect.DeepEqual(cfg.jiraStatuses, []string{"Open"}) {
		t.Errorf("jiraStatuses = %#v", cfg.jiraStatuses)
	}
	if !reflect.DeepEqual(cfg.jiraIssueTypes, []string{"Bug"}) {
		t.Errorf("jiraIssueTypes = %#v", cfg.jiraIssueTypes)
	}
	if !reflect.DeepEqual(cfg.jiraFixVersions, []string{"2.5.0"}) {
		t.Errorf("jiraFixVersions = %#v", cfg.jiraFixVersions)
	}
	if !reflect.DeepEqual(cfg.jiraRequiredLabels, []string{"approved"}) {
		t.Errorf("jiraRequiredLabels = %#v", cfg.jiraRequiredLabels)
	}
	if !reflect.DeepEqual(cfg.jiraExcludedLabels, []string{"wip", "hold"}) {
		t.Errorf("jiraExcludedLabels = %#v", cfg.jiraExcludedLabels)
	}
	if !reflect.DeepEqual(cfg.jiraFilterProjects, []string{"FOO"}) {
		t.Errorf("jiraFilterProjects = %#v", cfg.jiraFilterProjects)
	}
	if cfg.jiraCustomJQL != "labels in (urgent)" {
		t.Errorf("jiraCustomJQL = %q", cfg.jiraCustomJQL)
	}
	if !reflect.DeepEqual(cfg.jiraProjectMap, map[string]string{"KD": "gardenio"}) {
		t.Errorf("jiraProjectMap = %#v", cfg.jiraProjectMap)
	}
	if cfg.jiraPollInterval != 15*time.Second {
		t.Errorf("jiraPollInterval = %v, want 15s", cfg.jiraPollInterval)
	}
	if !cfg.jiraSkipAssigned {
		t.Error("jiraSkipAssigned = false, want true")
	}
	if !cfg.jiraDisableTransitions {
		t.Error("jiraDisableTransitions = false, want true (from '1')")
	}
	if !cfg.jiraEnableClaimTransition {
		t.Error("jiraEnableClaimTransition = false, want true")
	}
	if cfg.jiraBotAccountID != "acct-123" {
		t.Errorf("jiraBotAccountID = %q", cfg.jiraBotAccountID)
	}
	if cfg.jiraWebhookSecret != "shh" {
		t.Errorf("jiraWebhookSecret = %q", cfg.jiraWebhookSecret)
	}
	if !cfg.jiraWebhookDevMode {
		t.Error("jiraWebhookDevMode = false, want true (from '1')")
	}
	if !cfg.jiraMentionDisabled {
		t.Error("jiraMentionDisabled = false, want true")
	}
	if cfg.jiraMentionTrigger != "@bot" {
		t.Errorf("jiraMentionTrigger = %q", cfg.jiraMentionTrigger)
	}
	if cfg.jiraMentionBotTrigger != "@gardenio-bot" {
		t.Errorf("jiraMentionBotTrigger = %q", cfg.jiraMentionBotTrigger)
	}
	if cfg.jiraMentionProject != "gardenio" {
		t.Errorf("jiraMentionProject = %q", cfg.jiraMentionProject)
	}
	if cfg.listenAddr != ":9090" {
		t.Errorf("listenAddr = %q", cfg.listenAddr)
	}
	if cfg.logLevel != "debug" {
		t.Errorf("logLevel = %q", cfg.logLevel)
	}
	if cfg.statePath != "/tmp/state.json" {
		t.Errorf("statePath = %q", cfg.statePath)
	}
	if cfg.metricsAddr != ":9099" {
		t.Errorf("metricsAddr = %q, want :9099", cfg.metricsAddr)
	}
	if cfg.metricsInterval != 300*time.Second {
		t.Errorf("metricsInterval = %v, want 300s", cfg.metricsInterval)
	}
	wantQueries := []bridge.MetricQuery{
		{Name: "open_bugs", JQL: "issuetype = Bug AND statusCategory != Done"},
		{Name: "by_pe", JQL: "project = PE AND statusCategory != Done", Project: "PE", Interval: 600 * time.Second},
	}
	if !reflect.DeepEqual(cfg.metricsQueries, wantQueries) {
		t.Errorf("metricsQueries = %#v, want %#v", cfg.metricsQueries, wantQueries)
	}
	if cfg.ageInterval != 900*time.Second {
		t.Errorf("ageInterval = %v, want 900s", cfg.ageInterval)
	}
	wantAgeQueries := []bridge.AgeQuery{
		{Name: "open_bugs_age", JQL: "issuetype = Bug AND statusCategory != Done", Project: "PE", Interval: 600 * time.Second, MaxIssues: 1500},
	}
	if !reflect.DeepEqual(cfg.ageQueries, wantAgeQueries) {
		t.Errorf("ageQueries = %#v, want %#v", cfg.ageQueries, wantAgeQueries)
	}
}

func TestParseConfig_InvalidPollIntervalFallsBackToDefault(t *testing.T) {
	clearJiraEnv(t)
	t.Setenv("JIRA_POLL_INTERVAL", "not-a-duration")

	cfg := parseConfig()

	if cfg.jiraPollInterval != 60*time.Second {
		t.Errorf("jiraPollInterval = %v, want default 60s when unparseable", cfg.jiraPollInterval)
	}
}

func TestParseConfig_BoolFlagsOffForOtherValues(t *testing.T) {
	// Any value other than "true" or "1" should leave the flag off.
	clearJiraEnv(t)
	t.Setenv("JIRA_SKIP_ASSIGNED", "yes")
	t.Setenv("JIRA_DISABLE_TRANSITIONS", "on")
	t.Setenv("JIRA_ENABLE_CLAIM_TRANSITION", "0")
	t.Setenv("JIRA_WEBHOOK_DEV_MODE", "false")
	t.Setenv("JIRA_MENTION_DISABLED", "no")

	cfg := parseConfig()

	if cfg.jiraSkipAssigned {
		t.Error("jiraSkipAssigned = true for 'yes'")
	}
	if cfg.jiraDisableTransitions {
		t.Error("jiraDisableTransitions = true for 'on'")
	}
	if cfg.jiraEnableClaimTransition {
		t.Error("jiraEnableClaimTransition = true for '0'")
	}
	if cfg.jiraWebhookDevMode {
		t.Error("jiraWebhookDevMode = true for 'false'")
	}
	if cfg.jiraMentionDisabled {
		t.Error("jiraMentionDisabled = true for 'no'")
	}
}

// TestWritePolicyDefaultMode_IsShadow pins the SHIPPED default enforcement mode
// (G3 of gd-h0UVJuOrBH): with JIRA_FIELD_ALLOWLIST_MODE UNSET, the write policy
// the jira-bridge builds must resolve to SHADOW — out-of-policy writes are
// observe-only (logged, allowed) by default until an operator explicitly sets
// enforce (the ka-0000004831 cutover knob).
//
// This documents the intended rollout posture and guards against a reviewer (or
// a future refactor) misreading the gates' "fail closed" invariant as "drops
// live writes by default." The gates fail CLOSED on the verdict; the store's
// apply() then honors the MODE — and the default mode is shadow, so a denial is
// logged-but-allowed until enforce is turned on. Flipping this default is G2
// (ka-0000004831), the operator-owned enforce cutover, deliberately NOT here.
//
// The policy is constructed exactly the way cmd/jira-bridge/main.go does: read
// the env var into cfg via parseConfig, map it through AllowlistModeFromString,
// and stamp it onto DefaultWritePolicy's Mode before building the store.
func TestWritePolicyDefaultMode_IsShadow(t *testing.T) {
	clearJiraEnv(t)
	// clearJiraEnv does not touch JIRA_FIELD_ALLOWLIST_MODE / JIRA_OUTBOX_ENABLED
	// (they are not part of its key list), so unset them here to model the shipped
	// default. t.Setenv snapshots + restores the originals; os.Unsetenv then makes
	// them genuinely absent for this test, so os.Getenv returns "" as it would in
	// a stock deployment.
	t.Setenv("JIRA_FIELD_ALLOWLIST_MODE", "")
	t.Setenv("JIRA_OUTBOX_ENABLED", "")
	if err := os.Unsetenv("JIRA_FIELD_ALLOWLIST_MODE"); err != nil {
		t.Fatalf("unset JIRA_FIELD_ALLOWLIST_MODE: %v", err)
	}
	if err := os.Unsetenv("JIRA_OUTBOX_ENABLED"); err != nil {
		t.Fatalf("unset JIRA_OUTBOX_ENABLED: %v", err)
	}

	cfg := parseConfig()
	// Sanity: the env var truly read as empty, so we are exercising the UNSET path
	// (not accidentally a leaked "enforce" from the outer environment).
	if cfg.jiraFieldAllowlistMode != "" {
		t.Fatalf("jiraFieldAllowlistMode = %q, want empty (env unset)", cfg.jiraFieldAllowlistMode)
	}

	// Mirror main.go's construction verbatim.
	allowlistMode := bridge.AllowlistModeFromString(cfg.jiraFieldAllowlistMode)
	writePolicy := bridge.NewWritePolicyStore(
		func() bridge.WritePolicy {
			wp := bridge.DefaultWritePolicy(cfg.jiraOutboxEnabled)
			wp.Mode = allowlistMode
			return wp
		}(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	if got := writePolicy.Mode(); got != bridge.AllowlistModeShadow {
		t.Errorf("default write-policy mode = %v (%s), want shadow — out-of-policy "+
			"writes must be observe-only until an operator sets enforce (G2/ka-0000004831)",
			got, got.String())
	}
	if got := writePolicy.Mode().String(); got != "shadow" {
		t.Errorf("default write-policy mode string = %q, want \"shadow\"", got)
	}
	// Anchor the resolution rule itself: the empty env value maps to shadow.
	if got := bridge.AllowlistModeFromString(""); got != bridge.AllowlistModeShadow {
		t.Errorf("AllowlistModeFromString(\"\") = %v, want shadow (safe rollout default)", got)
	}
}

func TestRegisterHealthHandlers(t *testing.T) {
	mux := http.NewServeMux()
	registerHealthHandlers(mux, "1.2.3")

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	tests := []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{"/healthz", http.StatusOK, `{"status":"ok","version":"1.2.3"}`},
		{"/readyz", http.StatusOK, `{"status":"ok"}`},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tt.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if string(body) != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
		})
	}
}

func TestParseMetricsQueries(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []bridge.MetricQuery
	}{
		{
			name: "empty disables collector",
			in:   "",
			want: nil,
		},
		{
			name: "malformed JSON disables collector",
			in:   "{not json",
			want: nil,
		},
		{
			name: "entries missing name or jql are skipped",
			in:   `[{"name":"","jql":"x"},{"name":"y","jql":""},{"name":"ok","jql":"statusCategory != Done"}]`,
			want: []bridge.MetricQuery{{Name: "ok", JQL: "statusCategory != Done"}},
		},
		{
			name: "all-skipped yields nil",
			in:   `[{"name":"","jql":""}]`,
			want: nil,
		},
		{
			name: "full entry with project, help, and interval override",
			in:   `[{"name":"open_bugs","help":"open bug count","jql":"issuetype = Bug AND statusCategory != Done","project":"PE","interval":"300s"}]`,
			want: []bridge.MetricQuery{{
				Name:     "open_bugs",
				Help:     "open bug count",
				JQL:      "issuetype = Bug AND statusCategory != Done",
				Project:  "PE",
				Interval: 300 * time.Second,
			}},
		},
		{
			name: "bad interval falls back to zero (use collector default)",
			in:   `[{"name":"wip","jql":"statusCategory = \"In Progress\"","interval":"not-a-duration"}]`,
			want: []bridge.MetricQuery{{Name: "wip", JQL: `statusCategory = "In Progress"`}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseMetricsQueries(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseMetricsQueries(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseAgeQueries(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []bridge.AgeQuery
	}{
		{
			name: "empty disables age collector",
			in:   "",
			want: nil,
		},
		{
			name: "malformed JSON disables age collector",
			in:   "{not json",
			want: nil,
		},
		{
			name: "entries missing name or jql are skipped",
			in:   `[{"name":"","jql":"x"},{"name":"y","jql":""},{"name":"ok","jql":"statusCategory != Done"}]`,
			want: []bridge.AgeQuery{{Name: "ok", JQL: "statusCategory != Done"}},
		},
		{
			name: "all-skipped yields nil",
			in:   `[{"name":"","jql":""}]`,
			want: nil,
		},
		{
			name: "full entry with project, help, interval and maxIssues",
			in:   `[{"name":"open_bugs_age","help":"open bug ages","jql":"issuetype = Bug AND statusCategory != Done","project":"PE","interval":"600s","maxIssues":1500}]`,
			want: []bridge.AgeQuery{{
				Name:      "open_bugs_age",
				Help:      "open bug ages",
				JQL:       "issuetype = Bug AND statusCategory != Done",
				Project:   "PE",
				Interval:  600 * time.Second,
				MaxIssues: 1500,
			}},
		},
		{
			name: "bad interval falls back to zero and negative maxIssues clamps to zero",
			in:   `[{"name":"wip_age","jql":"statusCategory = \"In Progress\"","interval":"not-a-duration","maxIssues":-5}]`,
			want: []bridge.AgeQuery{{Name: "wip_age", JQL: `statusCategory = "In Progress"`}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseAgeQueries(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseAgeQueries(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuildMetricsServer(t *testing.T) {
	srv := buildMetricsServer(":9093")
	if srv.Addr != ":9093" {
		t.Errorf("Addr = %q, want :9093", srv.Addr)
	}
	// Serve the handler and confirm GET /metrics returns the exposition format.
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// The default gatherer always carries the Go runtime collectors.
	if !strings.Contains(string(body), "go_goroutines") {
		t.Errorf("/metrics body missing default Go collector series; got %d bytes", len(body))
	}
}

func TestBuildMentionHandler_Disabled(t *testing.T) {
	cfg := &config{jiraMentionDisabled: true}
	got := buildMentionHandler(cfg, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil)
	if got != nil {
		t.Errorf("buildMentionHandler(disabled=true) = %v, want nil", got)
	}
}

func TestBuildMentionHandler_Enabled(t *testing.T) {
	// Use a dummy beads client — the handler construction does not dial it.
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("dummy beadsapi: %v", err)
	}
	cfg := &config{
		jiraMentionDisabled:   false,
		jiraMentionTrigger:    "@gardenio",
		jiraMentionBotTrigger: "@gardenio-bot",
		jiraMentionProject:    "gardenio",
	}
	got := buildMentionHandler(cfg, daemon, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil)
	if got == nil {
		t.Fatal("buildMentionHandler(disabled=false) = nil, want non-nil")
	}
}

// TestBuildMentionHandlerConfig_ThreadsProjectMap pins the G3 wiring: the
// mention handler receives the SAME BOAT_PROJECTS-derived per-prefix map the
// poller gets, so mention spawns route to the mapped boat project instead of
// collapsing onto the single default (epic ka-0000007401).
func TestBuildMentionHandlerConfig_ThreadsProjectMap(t *testing.T) {
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("dummy beadsapi: %v", err)
	}
	cfg := &config{
		jiraMentionTrigger: "@gardenio",
		jiraMentionProject: "gardenio",
		jiraProjectMap:     map[string]string{"PE": "boatA", "DEVOPS": "boatB"},
	}
	got := buildMentionHandlerConfig(cfg, daemon, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil)
	if !reflect.DeepEqual(got.ProjectMap, cfg.jiraProjectMap) {
		t.Errorf("ProjectMap = %#v, want %#v", got.ProjectMap, cfg.jiraProjectMap)
	}
	if got.Project != "gardenio" {
		t.Errorf("Project = %q, want gardenio", got.Project)
	}
	if got.Trigger != "@gardenio" {
		t.Errorf("Trigger = %q, want @gardenio", got.Trigger)
	}
}

// TestBuildMentionHandlerConfig_WiresSpawnAck pins that a non-nil acknowledger
// is wired onto OnSpawn (Slack agent-card parity, plan ka-0000007401), and that
// a nil acknowledger leaves OnSpawn nil (legacy silent-until-first-reply).
func TestBuildMentionHandlerConfig_WiresSpawnAck(t *testing.T) {
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("dummy beadsapi: %v", err)
	}
	cfg := &config{jiraMentionTrigger: "@gardenio", jiraMentionProject: "gardenio"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// nil acknowledger → OnSpawn stays nil.
	if got := buildMentionHandlerConfig(cfg, daemon, nil, logger, nil, nil, nil); got.OnSpawn != nil {
		t.Error("OnSpawn should be nil when no acknowledger is wired")
	}

	// non-nil acknowledger → OnSpawn is set.
	ack := bridge.NewJiraSpawnAcknowledger(bridge.JiraSpawnAcknowledgerConfig{Logger: logger})
	if got := buildMentionHandlerConfig(cfg, daemon, nil, logger, nil, nil, ack); got.OnSpawn == nil {
		t.Error("OnSpawn should be set when an acknowledger is wired")
	}
}

func TestBuildJiraPollerConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config{
		jiraProjects:         []string{"PE"},
		jiraStatuses:         []string{"Open"},
		jiraIssueTypes:       []string{"Bug"},
		jiraFixVersions:      []string{"2.5"},
		jiraRequiredLabels:   []string{"approved"},
		jiraExcludedLabels:   []string{"wip"},
		jiraFilterProjects:   []string{"PE"},
		jiraCustomJQL:        "labels = urgent",
		jiraProjectMap:       map[string]string{"PE": "gardenio"},
		jiraPollInterval:     30 * time.Second,
		jiraSkipAssigned:     true,
		jiraBotAccountID:     "acct-1",
		jiraCustomFields:     map[string]string{"customfield_11976": "jira_issue_url"},
		jiraCaptureAllCustom: true,
	}
	got := buildJiraPollerConfig(cfg, logger)

	if !reflect.DeepEqual(got.CustomFields, cfg.jiraCustomFields) {
		t.Errorf("CustomFields mismatch: %v", got.CustomFields)
	}
	if !got.CaptureAllCustom {
		t.Error("CaptureAllCustom = false, want true")
	}

	if !reflect.DeepEqual(got.Projects, cfg.jiraProjects) {
		t.Errorf("Projects mismatch: %v", got.Projects)
	}
	if !reflect.DeepEqual(got.Statuses, cfg.jiraStatuses) {
		t.Errorf("Statuses mismatch: %v", got.Statuses)
	}
	if !reflect.DeepEqual(got.IssueTypes, cfg.jiraIssueTypes) {
		t.Errorf("IssueTypes mismatch: %v", got.IssueTypes)
	}
	if !reflect.DeepEqual(got.FixVersions, cfg.jiraFixVersions) {
		t.Errorf("FixVersions mismatch: %v", got.FixVersions)
	}
	if !reflect.DeepEqual(got.RequiredLabels, cfg.jiraRequiredLabels) {
		t.Errorf("RequiredLabels mismatch: %v", got.RequiredLabels)
	}
	if !reflect.DeepEqual(got.ExcludedLabels, cfg.jiraExcludedLabels) {
		t.Errorf("ExcludedLabels mismatch: %v", got.ExcludedLabels)
	}
	if !reflect.DeepEqual(got.FilterProjects, cfg.jiraFilterProjects) {
		t.Errorf("FilterProjects mismatch: %v", got.FilterProjects)
	}
	if got.CustomJQL != cfg.jiraCustomJQL {
		t.Errorf("CustomJQL = %q, want %q", got.CustomJQL, cfg.jiraCustomJQL)
	}
	if !reflect.DeepEqual(got.ProjectMap, cfg.jiraProjectMap) {
		t.Errorf("ProjectMap mismatch: %v", got.ProjectMap)
	}
	if got.PollInterval != cfg.jiraPollInterval {
		t.Errorf("PollInterval = %v, want %v", got.PollInterval, cfg.jiraPollInterval)
	}
	if !got.SkipAssigned {
		t.Error("SkipAssigned = false, want true")
	}
	if got.BotAccountID != cfg.jiraBotAccountID {
		t.Errorf("BotAccountID = %q, want %q", got.BotAccountID, cfg.jiraBotAccountID)
	}
	if got.Logger != logger {
		t.Error("Logger not forwarded")
	}
}

func TestInit_VersionFromEnvAlreadyApplied(t *testing.T) {
	// init() runs at package load; if VERSION is set in the test process env,
	// version should reflect it. Otherwise version stays "dev". Both are
	// acceptable — this test just guards against version being empty.
	if version == "" {
		t.Error("version should never be empty after init()")
	}
}

func TestNormalizeBasePath(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty stays empty", "", ""},
		{"whitespace stays empty", "   ", ""},
		{"slash only stays empty", "/", ""},
		{"trailing slash trimmed", "/jira/", "/jira"},
		{"multiple trailing slashes trimmed", "/jira///", "/jira"},
		{"missing leading slash added", "jira", "/jira"},
		{"already normalized", "/jira", "/jira"},
		{"nested prefix preserved", "/api/v1/jira", "/api/v1/jira"},
		{"whitespace around stripped", "  /jira  ", "/jira"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeBasePath(tt.in); got != tt.want {
				t.Errorf("normalizeBasePath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestWrapBasePath(t *testing.T) {
	inner := http.NewServeMux()
	inner.HandleFunc("/webhook", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("webhook-ok"))
	})
	inner.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("health-ok"))
	})

	t.Run("empty basePath returns inner directly", func(t *testing.T) {
		h := wrapBasePath(inner, "")
		if h != http.Handler(inner) {
			t.Errorf("expected inner handler when basePath is empty, got wrapper")
		}
	})

	t.Run("routes serve at root when basePath set", func(t *testing.T) {
		h := wrapBasePath(inner, "/jira")
		// k8s probes hit /healthz directly without any prefix.
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET /healthz: code = %d, want 200", rec.Code)
		}
		if rec.Body.String() != "health-ok" {
			t.Errorf("GET /healthz: body = %q, want health-ok", rec.Body.String())
		}
	})

	t.Run("routes also serve under basePath with prefix stripped", func(t *testing.T) {
		h := wrapBasePath(inner, "/jira")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/jira/webhook", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("POST /jira/webhook: code = %d, want 200", rec.Code)
		}
		if rec.Body.String() != "webhook-ok" {
			t.Errorf("POST /jira/webhook: body = %q, want webhook-ok", rec.Body.String())
		}
	})

	t.Run("unknown path under basePath 404s", func(t *testing.T) {
		h := wrapBasePath(inner, "/jira")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jira/does-not-exist", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /jira/does-not-exist: code = %d, want 404", rec.Code)
		}
	})

	t.Run("nested basePath strips correctly", func(t *testing.T) {
		h := wrapBasePath(inner, "/api/v1/jira")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jira/healthz", nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "health-ok" {
			t.Errorf("nested GET: code=%d body=%q, want 200/health-ok", rec.Code, rec.Body.String())
		}
	})
}
