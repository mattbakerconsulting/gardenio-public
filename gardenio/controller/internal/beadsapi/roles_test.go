package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListRoleBeads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("type"); got != "role" {
			t.Errorf("expected type=role, got %q", got)
		}

		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:    "role-1",
					Title: "crew",
					Type:  "role",
					Fields: json.RawMessage(`{
						"image":"ghcr.io/joyousaversion/gardenio/agent:custom",
						"cpu_request":"500m",
						"cpu_limit":"2000m",
						"memory_request":"1Gi",
						"memory_limit":"4Gi",
						"docker":"true",
						"service_account":"crew-sa",
						"runtime_class":"runsc",
						"execution_target":"lambda_microvm",
						"account":"gardenio",
						"model":"claude-fable-5-oauth",
						"secrets":[{"env":"GITHUB_TOKEN","secret":"crew-gh","key":"token"}],
						"env":[{"name":"LOG_LEVEL","value":"debug"}],
						"env_json":"{\"CUSTOM_VAR\":\"custom_value\"}"
					}`),
				},
				{
					ID:    "role-2",
					Title: "gardener",
					Type:  "role",
					Fields: json.RawMessage(`{
						"memory_limit":"8Gi",
						"postgres":"true"
					}`),
				},
			},
			Total: 2,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	roles, err := c.ListRoleBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(roles) != 2 {
		t.Fatalf("expected 2 roles, got %d", len(roles))
	}

	crew, ok := roles["crew"]
	if !ok {
		t.Fatal("expected role 'crew' in map")
	}
	if crew.Image != "ghcr.io/joyousaversion/gardenio/agent:custom" {
		t.Errorf("image: got %q, want custom image", crew.Image)
	}
	if crew.CPURequest != "500m" {
		t.Errorf("cpu_request: got %q, want %q", crew.CPURequest, "500m")
	}
	if crew.CPULimit != "2000m" {
		t.Errorf("cpu_limit: got %q, want %q", crew.CPULimit, "2000m")
	}
	if crew.MemoryRequest != "1Gi" {
		t.Errorf("memory_request: got %q, want %q", crew.MemoryRequest, "1Gi")
	}
	if crew.MemoryLimit != "4Gi" {
		t.Errorf("memory_limit: got %q, want %q", crew.MemoryLimit, "4Gi")
	}
	if !crew.DockerEnabled {
		t.Error("docker: expected true")
	}
	if crew.ServiceAccount != "crew-sa" {
		t.Errorf("service_account: got %q, want %q", crew.ServiceAccount, "crew-sa")
	}
	if crew.RuntimeClassName != "runsc" {
		t.Errorf("runtime_class: got %q, want %q", crew.RuntimeClassName, "runsc")
	}
	if crew.ExecutionTarget != "lambda_microvm" {
		t.Errorf("execution_target: got %q, want %q", crew.ExecutionTarget, "lambda_microvm")
	}
	if len(crew.Secrets) != 1 {
		t.Fatalf("secrets: expected 1, got %d", len(crew.Secrets))
	}
	if crew.Secrets[0].Env != "GITHUB_TOKEN" {
		t.Errorf("secrets[0].env: got %q, want %q", crew.Secrets[0].Env, "GITHUB_TOKEN")
	}
	if len(crew.EnvVars) != 1 {
		t.Fatalf("env_vars: expected 1, got %d", len(crew.EnvVars))
	}
	if crew.EnvVars[0].Name != "LOG_LEVEL" {
		t.Errorf("env_vars[0].name: got %q, want %q", crew.EnvVars[0].Name, "LOG_LEVEL")
	}
	if crew.EnvOverrides == nil || crew.EnvOverrides["CUSTOM_VAR"] != "custom_value" {
		t.Errorf("env_overrides: expected CUSTOM_VAR=custom_value, got %v", crew.EnvOverrides)
	}
	if crew.Account != "gardenio" {
		t.Errorf("account: got %q, want %q", crew.Account, "gardenio")
	}
	// Model override: a role can pin a model independent of its account so one
	// account backs multiple models without cloning account beads (ka-0000005488).
	if crew.Model != "claude-fable-5-oauth" {
		t.Errorf("model: got %q, want %q", crew.Model, "claude-fable-5-oauth")
	}

	gardener, ok := roles["gardener"]
	if !ok {
		t.Fatal("expected role 'gardener' in map")
	}
	if gardener.MemoryLimit != "8Gi" {
		t.Errorf("memory_limit: got %q, want %q", gardener.MemoryLimit, "8Gi")
	}
	if !gardener.PostgresEnabled {
		t.Error("postgres: expected true")
	}
	if gardener.Image != "" {
		t.Errorf("image: expected empty, got %q", gardener.Image)
	}
	// No model field on the gardener bead → empty (inherits account default).
	if gardener.Model != "" {
		t.Errorf("model: expected empty for unset field, got %q", gardener.Model)
	}
}

func TestListRoleBeads_SkipsEmptyTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:     "role-empty",
					Title:  "",
					Type:   "role",
					Fields: json.RawMessage(`{"image":"should-be-skipped"}`),
				},
			},
			Total: 1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	roles, err := c.ListRoleBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(roles) != 0 {
		t.Errorf("expected 0 roles (empty title skipped), got %d", len(roles))
	}
}

func TestListRoleBeads_AccountWhitelist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:     "role-json",
					Title:  "jsonrole",
					Type:   "role",
					Fields: json.RawMessage(`{"account_whitelist":"[\"acct-a\",\"acct-b\"]"}`),
				},
				{
					ID:     "role-csv",
					Title:  "csvrole",
					Type:   "role",
					Fields: json.RawMessage(`{"account_whitelist":"acct-c, acct-d ,acct-e"}`),
				},
				{
					ID:     "role-empty",
					Title:  "emptyrole",
					Type:   "role",
					Fields: json.RawMessage(`{"account_whitelist":""}`),
				},
				{
					ID:     "role-none",
					Title:  "norole",
					Type:   "role",
					Fields: json.RawMessage(`{}`),
				},
			},
			Total: 4,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	roles, err := c.ListRoleBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	jsonrole := roles["jsonrole"]
	if len(jsonrole.AccountWhitelist) != 2 || jsonrole.AccountWhitelist[0] != "acct-a" || jsonrole.AccountWhitelist[1] != "acct-b" {
		t.Errorf("jsonrole whitelist = %v, want [acct-a acct-b]", jsonrole.AccountWhitelist)
	}

	csvrole := roles["csvrole"]
	if len(csvrole.AccountWhitelist) != 3 || csvrole.AccountWhitelist[0] != "acct-c" || csvrole.AccountWhitelist[1] != "acct-d" || csvrole.AccountWhitelist[2] != "acct-e" {
		t.Errorf("csvrole whitelist = %v, want [acct-c acct-d acct-e]", csvrole.AccountWhitelist)
	}

	if emptyrole := roles["emptyrole"]; emptyrole.AccountWhitelist != nil {
		t.Errorf("emptyrole whitelist = %v, want nil", emptyrole.AccountWhitelist)
	}
	if norole := roles["norole"]; norole.AccountWhitelist != nil {
		t.Errorf("norole whitelist = %v, want nil", norole.AccountWhitelist)
	}
}

// TestListRoleBeads_SkillToggle verifies the per-role skill toggle fields (Adam
// feedback #1, epic ka-0000004989) parse from a JSON array OR a comma-separated
// string, and that empty/absent fields yield nil — mirroring account_whitelist.
func TestListRoleBeads_SkillToggle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:     "role-json",
					Title:  "jsonrole",
					Type:   "role",
					Fields: json.RawMessage(`{"enabled_skills":"[\"onboarding\",\"video\"]","disabled_skills":"[\"deploy\"]","enabled_agents":"[\"reviewer.md\"]","disabled_agents":"helm-deployer.md, extra.md"}`),
				},
				{
					ID:     "role-csv",
					Title:  "csvrole",
					Type:   "role",
					Fields: json.RawMessage(`{"enabled_skills":"a, b ,c","disabled_skills":"x ,y"}`),
				},
				{
					ID:     "role-none",
					Title:  "norole",
					Type:   "role",
					Fields: json.RawMessage(`{}`),
				},
			},
			Total: 3,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	roles, err := c.ListRoleBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	jsonrole := roles["jsonrole"]
	if want := []string{"onboarding", "video"}; !equalStrs(jsonrole.EnabledSkills, want) {
		t.Errorf("jsonrole EnabledSkills = %v, want %v", jsonrole.EnabledSkills, want)
	}
	if want := []string{"deploy"}; !equalStrs(jsonrole.DisabledSkills, want) {
		t.Errorf("jsonrole DisabledSkills = %v, want %v", jsonrole.DisabledSkills, want)
	}
	// The sub-agent toggle parses the same lenient shapes (ka-0000006908).
	if want := []string{"reviewer.md"}; !equalStrs(jsonrole.EnabledAgents, want) {
		t.Errorf("jsonrole EnabledAgents = %v, want %v", jsonrole.EnabledAgents, want)
	}
	if want := []string{"helm-deployer.md", "extra.md"}; !equalStrs(jsonrole.DisabledAgents, want) {
		t.Errorf("jsonrole DisabledAgents = %v, want %v", jsonrole.DisabledAgents, want)
	}

	csvrole := roles["csvrole"]
	if want := []string{"a", "b", "c"}; !equalStrs(csvrole.EnabledSkills, want) {
		t.Errorf("csvrole EnabledSkills = %v, want %v", csvrole.EnabledSkills, want)
	}
	if want := []string{"x", "y"}; !equalStrs(csvrole.DisabledSkills, want) {
		t.Errorf("csvrole DisabledSkills = %v, want %v", csvrole.DisabledSkills, want)
	}

	if norole := roles["norole"]; norole.EnabledSkills != nil || norole.DisabledSkills != nil {
		t.Errorf("norole skill toggle = (%v,%v), want (nil,nil)", norole.EnabledSkills, norole.DisabledSkills)
	}
	if norole := roles["norole"]; norole.EnabledAgents != nil || norole.DisabledAgents != nil {
		t.Errorf("norole agent toggle = (%v,%v), want (nil,nil)", norole.EnabledAgents, norole.DisabledAgents)
	}
}

// equalStrs is a tiny slice-equality helper for the skill-toggle assertions.
func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseStringList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"[]", nil},
		{`["a","b"]`, []string{"a", "b"}},
		{`["a", " b ", ""]`, []string{"a", "b"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ,, c ", []string{"a", "b", "c"}},
		{"single", []string{"single"}},
		{`[malformed`, []string{"[malformed"}}, // falls back to comma-split
	}
	for _, tc := range cases {
		got := parseStringList(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("parseStringList(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseStringList(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

func TestListRoleBeads_ClaudeConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:    "role-cfg",
					Title: "ops",
					Type:  "role",
					Fields: json.RawMessage(`{
						"claude_config":"{\"claude-settings\":{\"model\":\"opus\"}}"
					}`),
				},
			},
			Total: 1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	roles, err := c.ListRoleBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ops, ok := roles["ops"]
	if !ok {
		t.Fatal("expected role 'ops' in map")
	}
	if ops.ClaudeConfig == nil {
		t.Fatal("expected claude_config to be parsed")
	}
	if _, ok := ops.ClaudeConfig["claude-settings"]; !ok {
		t.Error("expected claude-settings key in claude_config")
	}
}

func TestListRoleBeads_DuplicateTitleDeterministicWinner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Two beads with the same title "crew". The one with the
		// lexicographically lower ID ("role-aaa") must win deterministically,
		// regardless of the order the daemon returns them.
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID: "role-zzz", Title: "crew", Type: "role", Status: "open",
					Fields: json.RawMessage(`{"image":"loser-image","cpu_limit":"9"}`),
				},
				{
					ID: "role-aaa", Title: "crew", Type: "role", Status: "open",
					Fields: json.RawMessage(`{"image":"winner-image","cpu_limit":"1"}`),
				},
			},
			Total: 2,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	roles, err := c.ListRoleBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(roles) != 1 {
		t.Fatalf("expected 1 role after dedup, got %d", len(roles))
	}
	crew := roles["crew"]
	if crew.Image != "winner-image" {
		t.Errorf("duplicate winner: got image %q, want winner-image (lowest bead ID)", crew.Image)
	}
	if crew.CPULimit != "1" {
		t.Errorf("duplicate winner: got cpu_limit %q, want 1", crew.CPULimit)
	}
}

// TestListRoleBeads_DuplicateModelBearingWinsOverModelLess is the regression
// guard for bug ka-0000004910: two open type:role beads shared the title
// "gardener-delivery". The older, lexicographically-lower-ID bead (gd-0mahP7KW8C
// here "role-aaa") carried only resource limits; the newer, higher-ID bead
// (gd-y0LBcF566Y here "role-zzz") carried Matt's haiku model pin + account. The
// original lowest-bead-ID dedup shadowed the model pin, so gardener-delivery ran
// opus. The fix: a duplicate that sets model/account is NEVER shadowed by one
// that sets neither, regardless of bead-ID ordering or return order.
func TestListRoleBeads_DuplicateModelBearingWinsOverModelLess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				// Lower bead ID, but only resource limits — no model/account.
				// Under the old lowest-ID rule this would have won and blanked
				// the pin.
				{
					ID: "role-aaa", Title: "gardener-delivery", Type: "role", Status: "open",
					Fields: json.RawMessage(`{"cpu_limit":"2000m","memory_limit":"2Gi","session_resume":"false"}`),
				},
				// Higher bead ID, but carries the operator's model pin + account.
				{
					ID: "role-zzz", Title: "gardener-delivery", Type: "role", Status: "open",
					Fields: json.RawMessage(`{"model":"anthropic/claude-haiku-4-5","account":"gardenio","session_resume":"false"}`),
				},
			},
			Total: 2,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	roles, err := c.ListRoleBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(roles) != 1 {
		t.Fatalf("expected 1 role after dedup, got %d", len(roles))
	}
	cd := roles["gardener-delivery"]
	if cd.Model != "anthropic/claude-haiku-4-5" {
		t.Errorf("model-bearing duplicate was shadowed: got model %q, want the haiku pin", cd.Model)
	}
	if cd.Account != "gardenio" {
		t.Errorf("account from the winning bead lost: got %q, want gardenio", cd.Account)
	}
}

// TestListRoleBeads_DuplicateRecencyTiebreak: when two duplicates carry equal
// config signal (both set a model), the more recently updated bead wins so a
// fresher operator edit takes effect, independent of bead-ID ordering.
func TestListRoleBeads_DuplicateRecencyTiebreak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				// Lower bead ID, older edit, stale model.
				{
					ID: "role-aaa", Title: "crew", Type: "role", Status: "open",
					UpdatedAt: "2026-01-01T00:00:00Z",
					Fields:    json.RawMessage(`{"model":"stale-model"}`),
				},
				// Higher bead ID, newer edit, current model.
				{
					ID: "role-zzz", Title: "crew", Type: "role", Status: "open",
					UpdatedAt: "2026-07-01T00:00:00Z",
					Fields:    json.RawMessage(`{"model":"fresh-model"}`),
				},
			},
			Total: 2,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	roles, err := c.ListRoleBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := roles["crew"].Model; got != "fresh-model" {
		t.Errorf("recency tiebreak: got model %q, want fresh-model (newer updated_at)", got)
	}
}

func TestValidateAgentRoles(t *testing.T) {
	roles := map[string]RoleInfo{
		"polecat": {Name: "polecat"},
	}
	builtins := map[string]bool{"crew": true, "gardener": true}
	agents := []AgentBead{
		{AgentName: "a1", Role: "polecat"},  // known via type:role bead
		{AgentName: "a2", Role: "crew"},     // known via builtin
		{AgentName: "a3", Role: "gremlin"},  // unknown → warning
		{AgentName: "a4", Role: "phantom"},  // unknown → warning
		{AgentName: "a5", Role: ""},         // empty → skipped
		{AgentName: "a6", Role: "  crew  "}, // trimmed → builtin
	}

	warnings := ValidateAgentRoles(agents, roles, builtins)
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d: %v", len(warnings), warnings)
	}
	// Sorted output: "gremlin" agent (a3) before "phantom" agent (a4).
	if !strings.Contains(warnings[0], "gremlin") {
		t.Errorf("warnings[0] = %q, want it to mention gremlin", warnings[0])
	}
	if !strings.Contains(warnings[1], "phantom") {
		t.Errorf("warnings[1] = %q, want it to mention phantom", warnings[1])
	}
}

func TestValidateAgentRoles_NilBuiltins(t *testing.T) {
	roles := map[string]RoleInfo{"crew": {Name: "crew"}}
	agents := []AgentBead{
		{AgentName: "a1", Role: "crew"},
		{AgentName: "a2", Role: "unknown"},
	}
	warnings := ValidateAgentRoles(agents, roles, nil)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning with nil builtins, got %d: %v", len(warnings), warnings)
	}
}

func TestListRoleBeads_ParsesSessionResume(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID: "r-out", Title: "ephemeral", Type: "role", Status: "open",
					Fields: json.RawMessage(`{"session_resume":"false"}`),
				},
				{
					ID: "r-in", Title: "persistent", Type: "role", Status: "open",
					Fields: json.RawMessage(`{"session_resume":"true"}`),
				},
				{
					ID: "r-unset", Title: "default", Type: "role", Status: "open",
					Fields: json.RawMessage(`{}`),
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	roles, err := c.ListRoleBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if r := roles["ephemeral"]; r.SessionResume == nil || *r.SessionResume {
		t.Errorf("ephemeral: expected SessionResume=&false, got %v", r.SessionResume)
	}
	if r := roles["persistent"]; r.SessionResume == nil || !*r.SessionResume {
		t.Errorf("persistent: expected SessionResume=&true, got %v", r.SessionResume)
	}
	if r := roles["default"]; r.SessionResume != nil {
		t.Errorf("default: expected SessionResume=nil, got %v", *r.SessionResume)
	}
}

// TestListRoleBeads_ParsesUltracode proves the tri-state "ultracode" role field
// (ka-0000001714) parses into RoleInfo.Ultracode: "true"→&true, "false"→&false,
// absent→nil (unset). This is the field the gardenio-microvms role sets to opt
// its role-default MicroVM agents into ultracode/workflow mode.
func TestListRoleBeads_ParsesUltracode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID: "r-uc-on", Title: "microvms", Type: "role", Status: "open",
					Fields: json.RawMessage(`{"ultracode":"true"}`),
				},
				{
					ID: "r-uc-off", Title: "plain", Type: "role", Status: "open",
					Fields: json.RawMessage(`{"ultracode":"false"}`),
				},
				{
					ID: "r-uc-unset", Title: "default", Type: "role", Status: "open",
					Fields: json.RawMessage(`{}`),
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	roles, err := c.ListRoleBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if r := roles["microvms"]; r.Ultracode == nil || !*r.Ultracode {
		t.Errorf("microvms: expected Ultracode=&true, got %v", r.Ultracode)
	}
	if r := roles["plain"]; r.Ultracode == nil || *r.Ultracode {
		t.Errorf("plain: expected Ultracode=&false, got %v", r.Ultracode)
	}
	if r := roles["default"]; r.Ultracode != nil {
		t.Errorf("default: expected Ultracode=nil, got %v", *r.Ultracode)
	}
}

// TestListRoleBeads_Harness: the harness field is parsed (trimmed, not
// validated here — the controller refuses a bad value per spawn), and a
// harness-bearing duplicate is not shadowed by a harness-less one.
func TestListRoleBeads_Harness(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(listBeadsResponse{
			Beads: []beadJSON{
				{ID: "role-1", Title: "codexrole", Type: "role", Status: "open", Fields: json.RawMessage(`{"harness":" codex "}`)},
				{ID: "role-2", Title: "plainrole", Type: "role", Status: "open", Fields: json.RawMessage(`{}`)},
				{ID: "role-3", Title: "badrole", Type: "role", Status: "open", Fields: json.RawMessage(`{"harness":"gpt"}`)},
				{ID: "role-aaa", Title: "dup", Type: "role", Status: "open", Fields: json.RawMessage(`{"cpu_limit":"2"}`)},
				{ID: "role-zzz", Title: "dup", Type: "role", Status: "open", Fields: json.RawMessage(`{"harness":"codex"}`)},
			},
			Total: 5,
		})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	roles, err := c.ListRoleBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for name, want := range map[string]string{"codexrole": "codex", "plainrole": "", "badrole": "gpt", "dup": "codex"} {
		if got := roles[name].Harness; got != want {
			t.Errorf("role %q harness = %q, want %q", name, got, want)
		}
	}
}
