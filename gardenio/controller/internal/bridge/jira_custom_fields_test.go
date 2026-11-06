package bridge

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestCustomFieldString(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"url string", `"https://study-last.acme.uat.acme.net"`, "https://study-last.acme.uat.acme.net", true},
		{"plain string", `"hello"`, "hello", true},
		{"option object", `{"value":"Minor","id":"11851"}`, "Minor", true},
		{"user object", `{"displayName":"Jane Doe","accountId":"abc"}`, "Jane Doe", true},
		{"empty string", `""`, "", false},
		{"null", `null`, "", false},
		{"whitespace string", `"   "`, "", false},
		{"array unsupported", `["a","b"]`, "", false},
		{"number unsupported", `42`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CustomFieldString(json.RawMessage(tc.raw))
			if ok != tc.ok || got != tc.want {
				t.Errorf("CustomFieldString(%s) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestJiraIssue_UnmarshalJSON_PopulatesRawAndTyped(t *testing.T) {
	data := []byte(`{
		"key": "FSM-381",
		"id": "1001",
		"fields": {
			"summary": "CRF Tab Load Time",
			"customfield_11976": "https://study-last.acme.uat.acme.net"
		}
	}`)

	var issue JiraIssue
	if err := json.Unmarshal(data, &issue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if issue.Key != "FSM-381" {
		t.Errorf("Key = %q, want FSM-381", issue.Key)
	}
	if issue.Fields.Summary != "CRF Tab Load Time" {
		t.Errorf("Summary = %q", issue.Fields.Summary)
	}
	raw, ok := issue.RawFields["customfield_11976"]
	if !ok {
		t.Fatal("RawFields missing customfield_11976")
	}
	if v, ok := CustomFieldString(raw); !ok || v != "https://study-last.acme.uat.acme.net" {
		t.Errorf("custom field value = (%q, %v)", v, ok)
	}
}

func TestApplyCustomFields_PromotesConfiguredField(t *testing.T) {
	p := &JiraPoller{cfg: JiraPollerConfig{
		CustomFields: map[string]string{"customfield_11976": "jira_issue_url"},
		Logger:       discardLogger(),
	}}

	issue := JiraIssue{
		Key: "FSM-381",
		RawFields: map[string]json.RawMessage{
			"customfield_11976": json.RawMessage(`"https://study-last.acme.uat.acme.net"`),
			"customfield_99999": json.RawMessage(`"ignored"`),
		},
	}

	fields := map[string]string{}
	p.applyCustomFields(t.Context(), issue, fields)

	if got := fields["jira_issue_url"]; got != "https://study-last.acme.uat.acme.net" {
		t.Errorf("jira_issue_url = %q", got)
	}
	if _, ok := fields["jira_custom_fields"]; ok {
		t.Error("jira_custom_fields should not be set when CaptureAllCustom is false")
	}
}

func TestApplyCustomFields_MissingFieldNoop(t *testing.T) {
	p := &JiraPoller{cfg: JiraPollerConfig{
		CustomFields: map[string]string{"customfield_11976": "jira_issue_url"},
		Logger:       discardLogger(),
	}}

	issue := JiraIssue{Key: "FSM-380", RawFields: map[string]json.RawMessage{}}
	fields := map[string]string{}
	p.applyCustomFields(t.Context(), issue, fields)

	if _, ok := fields["jira_issue_url"]; ok {
		t.Error("jira_issue_url should be absent when the custom field is unpopulated")
	}
}

func TestRequestFields(t *testing.T) {
	t.Run("base only", func(t *testing.T) {
		p := &JiraPoller{cfg: JiraPollerConfig{}}
		got := p.requestFields()
		if len(got) != len(baseRequestFields) {
			t.Errorf("expected base fields, got %v", got)
		}
	})

	t.Run("with configured custom fields", func(t *testing.T) {
		p := &JiraPoller{cfg: JiraPollerConfig{
			CustomFields: map[string]string{"customfield_11976": "jira_issue_url"},
		}}
		got := p.requestFields()
		if !slices.Contains(got, "customfield_11976") {
			t.Errorf("expected customfield_11976 in request fields, got %v", got)
		}
	})

	t.Run("capture all uses wildcard", func(t *testing.T) {
		p := &JiraPoller{cfg: JiraPollerConfig{CaptureAllCustom: true}}
		got := p.requestFields()
		if len(got) != 1 || got[0] != "*all" {
			t.Errorf("expected [*all], got %v", got)
		}
	})
}

func TestParseCustomFieldMap(t *testing.T) {
	got := parseCustomFieldMap("customfield_11976:jira_issue_url, customfield_123:jira_env , bad_entry, :nofield, noid:")
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %v", got)
	}
	if got["customfield_11976"] != "jira_issue_url" {
		t.Errorf("customfield_11976 = %q", got["customfield_11976"])
	}
	if got["customfield_123"] != "jira_env" {
		t.Errorf("customfield_123 = %q", got["customfield_123"])
	}
	if parseCustomFieldMap("") != nil {
		t.Error("empty input should yield nil")
	}
}

func TestFormatCustomFieldMap_RoundTrip(t *testing.T) {
	m := map[string]string{"customfield_123": "jira_env", "customfield_11976": "jira_issue_url"}
	s := formatCustomFieldMap(m)
	// Sorted by id: customfield_11976 < customfield_123 lexically.
	want := "customfield_11976:jira_issue_url,customfield_123:jira_env"
	if s != want {
		t.Errorf("formatCustomFieldMap = %q, want %q", s, want)
	}
	if formatCustomFieldMap(nil) != "" {
		t.Error("nil map should format to empty string")
	}
}

func TestJiraConfigWatcher_ParseEventFields_CustomFields(t *testing.T) {
	w := &JiraConfigWatcher{envCfg: JiraPollerConfig{
		Projects:     []string{"PE"},
		CustomFields: map[string]string{"customfield_11976": "jira_issue_url"},
	}}

	cfg := w.parseEventFields(map[string]string{
		"custom_fields":      "customfield_555:jira_repro_url",
		"capture_all_custom": "true",
	})

	if cfg.CustomFields["customfield_555"] != "jira_repro_url" {
		t.Errorf("custom_fields override not applied: %v", cfg.CustomFields)
	}
	if !cfg.CaptureAllCustom {
		t.Error("capture_all_custom not applied")
	}
}

func TestJiraConfigWatcher_ParseEventFields_CustomFields_KeepsEnvDefault(t *testing.T) {
	w := &JiraConfigWatcher{envCfg: JiraPollerConfig{
		CustomFields: map[string]string{"customfield_11976": "jira_issue_url"},
	}}

	cfg := w.parseEventFields(map[string]string{})
	if cfg.CustomFields["customfield_11976"] != "jira_issue_url" {
		t.Errorf("env default custom fields lost: %v", cfg.CustomFields)
	}
	if cfg.CaptureAllCustom {
		t.Error("CaptureAllCustom should default false")
	}
}

func TestJiraConfigWatcher_SeedConfigBead_IncludesCustomFields(t *testing.T) {
	daemon := newJiraConfigMock()

	envCfg := JiraPollerConfig{
		Projects:         []string{"FSM"},
		Statuses:         []string{"To Do"},
		IssueTypes:       []string{"Bug"},
		PollInterval:     60 * time.Second,
		CustomFields:     map[string]string{"customfield_11976": "jira_issue_url"},
		CaptureAllCustom: true,
		Logger:           discardLogger(),
	}

	poller := NewJiraPoller(nil, daemon, envCfg)
	w := NewJiraConfigWatcher(JiraConfigWatcherConfig{
		Daemon: daemon,
		Poller: poller,
		Logger: discardLogger(),
		EnvCfg: envCfg,
	})

	w.LoadOrSeed(context.Background())

	bead := daemon.beads[w.beadID]
	if bead == nil {
		t.Fatal("expected seeded config bead")
	}
	if got := bead.Fields["custom_fields"]; got != "customfield_11976:jira_issue_url" {
		t.Errorf("seeded custom_fields = %q", got)
	}
	if got := bead.Fields["capture_all_custom"]; got != "true" {
		t.Errorf("seeded capture_all_custom = %q", got)
	}
}
