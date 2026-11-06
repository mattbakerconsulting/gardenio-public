package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestParseCooldownMinutes(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", 5, false},
		{"0", 0, false},
		{"7", 7, false},
		{"5m", 5, false},
		{"90s", 1, false},
		{"2h", 120, false},
		{"-1", 0, true},
		{"-1m", 0, true},
		{"abc", 0, true},
	}
	for _, c := range cases {
		got, err := parseCooldownMinutes(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseCooldownMinutes(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("parseCooldownMinutes(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestJoinKeyValuePairs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"empty", nil, ""},
		{"single", []string{"FOO=bar"}, `{"FOO":"bar"}`},
		{"deterministic order", []string{"B=2", "A=1"}, `{"A":"1","B":"2"}`},
		{"empty value", []string{"K="}, `{"K":""}`},
		{"missing equals treated as empty value", []string{"K"}, `{"K":""}`},
		{"later overrides earlier", []string{"K=1", "K=2"}, `{"K":"2"}`},
		{"skips blank keys", []string{"=foo", "A=1"}, `{"A":"1"}`},
		{"escapes quotes", []string{`K="hi"`}, `{"K":"\"hi\""}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := joinKeyValuePairs(c.in)
			if got != c.want {
				t.Errorf("joinKeyValuePairs(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestApplyCommonFields_Validation(t *testing.T) {
	t.Run("invalid action", func(t *testing.T) {
		f := map[string]any{}
		err := applyCommonFields(commonTriggerOpts{action: "explode"}, f)
		if err == nil || !strings.Contains(err.Error(), "invalid --action") {
			t.Fatalf("want invalid action error, got %v", err)
		}
	})
	t.Run("apply_formula requires formula", func(t *testing.T) {
		f := map[string]any{}
		err := applyCommonFields(commonTriggerOpts{action: "apply_formula"}, f)
		if err == nil || !strings.Contains(err.Error(), "--formula") {
			t.Fatalf("want missing formula error, got %v", err)
		}
	})
	t.Run("spawn_agent requires role", func(t *testing.T) {
		f := map[string]any{}
		err := applyCommonFields(commonTriggerOpts{action: "spawn_agent"}, f)
		if err == nil || !strings.Contains(err.Error(), "--role") {
			t.Fatalf("want missing role error, got %v", err)
		}
	})
	t.Run("max-concurrent must be non-negative", func(t *testing.T) {
		f := map[string]any{}
		err := applyCommonFields(commonTriggerOpts{action: "notify", maxConcurrent: -1}, f)
		if err == nil {
			t.Fatalf("expected error for negative --max-concurrent")
		}
	})
}

// TestApplyCommonFields_SlackChannel verifies --slack-channel is written to the
// slack_channel field (trimmed) and omitted entirely when unset — the notify
// path reads it as TriggerDef.SlackChannel to route notifications (ka-0000008204).
func TestApplyCommonFields_SlackChannel(t *testing.T) {
	fields := map[string]any{}
	if err := applyCommonFields(commonTriggerOpts{action: "notify", slackChannel: "  C073CG65QUD  "}, fields); err != nil {
		t.Fatalf("applyCommonFields: %v", err)
	}
	if fields["slack_channel"] != "C073CG65QUD" {
		t.Errorf("slack_channel = %v, want C073CG65QUD (trimmed)", fields["slack_channel"])
	}

	fields = map[string]any{}
	if err := applyCommonFields(commonTriggerOpts{action: "notify"}, fields); err != nil {
		t.Fatalf("applyCommonFields: %v", err)
	}
	if _, ok := fields["slack_channel"]; ok {
		t.Errorf("slack_channel present when unset: %v", fields["slack_channel"])
	}
}

func TestApplyCommonFields_DefaultsAndPassthrough(t *testing.T) {
	fields := map[string]any{}
	opts := commonTriggerOpts{
		action:          "apply_formula",
		role:            "crew",
		agentProject:    "monorepo",
		prompt:          "Fix the failing pipeline",
		formula:         "gd-FORMULA",
		vars:            []string{"REF=main", "URL={{gitlab_pipeline_url}}"},
		cooldown:        "10m",
		maxConcurrent:   2,
		enabled:         true,
		allowedProjects: []string{"monorepo", "*"},
		allowedRoles:    []string{"crew"},
	}
	if err := applyCommonFields(opts, fields); err != nil {
		t.Fatalf("applyCommonFields: %v", err)
	}
	want := map[string]any{
		"action":           "apply_formula",
		"enabled":          true,
		"cooldown_minutes": 10,
		"max_concurrent":   2,
		"role":             "crew",
		"project":          "monorepo",
		"prompt":           "Fix the failing pipeline",
		"formula_id":       "gd-FORMULA",
		"formula_vars":     `{"REF":"main","URL":"{{gitlab_pipeline_url}}"}`,
		"allowed_projects": "monorepo,*",
		"allowed_roles":    "crew",
	}
	if !reflect.DeepEqual(fields, want) {
		t.Errorf("applyCommonFields fields mismatch\n got: %#v\nwant: %#v", fields, want)
	}
}

func TestApplyCommonFields_WakeIntent(t *testing.T) {
	// --wake-intent writes the wake_intent field (trimmed); absent leaves it out.
	fields := map[string]any{}
	opts := commonTriggerOpts{
		action:        "notify",
		cooldown:      "5m",
		maxConcurrent: 3,
		enabled:       true,
		wakeIntent:    "  Weekdays 9am PT, plus whenever open P0s > 0  ",
	}
	if err := applyCommonFields(opts, fields); err != nil {
		t.Fatalf("applyCommonFields: %v", err)
	}
	if fields["wake_intent"] != "Weekdays 9am PT, plus whenever open P0s > 0" {
		t.Errorf("wake_intent = %v, want trimmed intent", fields["wake_intent"])
	}

	// Empty/whitespace-only wake-intent must not write the field at all.
	fields2 := map[string]any{}
	if err := applyCommonFields(commonTriggerOpts{action: "notify", cooldown: "5m", maxConcurrent: 3, wakeIntent: "   "}, fields2); err != nil {
		t.Fatalf("applyCommonFields: %v", err)
	}
	if _, ok := fields2["wake_intent"]; ok {
		t.Errorf("wake_intent should be absent for whitespace-only intent, got %v", fields2["wake_intent"])
	}
}

func TestBuildGitLabPipelineFields(t *testing.T) {
	opts := commonTriggerOpts{
		action:        "notify",
		cooldown:      "5m",
		maxConcurrent: 3,
		enabled:       true,
		agentProject:  "monorepo",
	}
	fields, labels, err := buildGitLabPipelineFields(opts, "Acme/Platform/monorepo", "failed", "main", "", "6h", "")
	if err != nil {
		t.Fatalf("buildGitLabPipelineFields: %v", err)
	}
	if fields["event_type"] != "pipeline" {
		t.Errorf("event_type = %v, want pipeline", fields["event_type"])
	}
	if fields["project_path"] != "Acme/Platform/monorepo" {
		t.Errorf("project_path = %v", fields["project_path"])
	}
	if fields["pipeline_status"] != "failed" {
		t.Errorf("pipeline_status = %v", fields["pipeline_status"])
	}
	if fields["pipeline_ref"] != "main" {
		t.Errorf("pipeline_ref = %v", fields["pipeline_ref"])
	}
	if fields["pipeline_max_age"] != "6h" {
		t.Errorf("pipeline_max_age = %v, want 6h", fields["pipeline_max_age"])
	}
	if fields["action"] != "notify" {
		t.Errorf("action = %v", fields["action"])
	}
	if fields["cooldown_minutes"] != 5 {
		t.Errorf("cooldown_minutes = %v, want 5", fields["cooldown_minutes"])
	}
	if fields["max_concurrent"] != 3 {
		t.Errorf("max_concurrent = %v, want 3", fields["max_concurrent"])
	}
	if !containsString(labels, "trigger:gitlab") {
		t.Errorf("labels missing trigger:gitlab: %v", labels)
	}
	if !containsString(labels, "trigger-source:gitlab-pipeline") {
		t.Errorf("labels missing trigger-source:gitlab-pipeline: %v", labels)
	}
}

func TestBuildGitLabPipelineFields_OmitsEmptyRef(t *testing.T) {
	fields, _, err := buildGitLabPipelineFields(commonTriggerOpts{action: "notify", cooldown: "5m", maxConcurrent: 1, enabled: true}, "p/p", "failed", "", "", "", "")
	if err != nil {
		t.Fatalf("buildGitLabPipelineFields: %v", err)
	}
	if _, present := fields["pipeline_ref"]; present {
		t.Errorf("expected pipeline_ref to be omitted when --ref is empty, got fields=%v", fields)
	}
	if _, present := fields["pipeline_ref_pattern"]; present {
		t.Errorf("expected pipeline_ref_pattern to be omitted when --ref-pattern is empty, got fields=%v", fields)
	}
}

// TestBuildGitLabPipelineFields_RefPattern covers the --ref-pattern flag: empty
// omits the field, a valid Go regexp is persisted verbatim, and an invalid
// regexp fails at authoring time rather than being persisted and only
// fail-closing at runtime (which would silently stop the trigger firing).
func TestBuildGitLabPipelineFields_RefPattern(t *testing.T) {
	base := commonTriggerOpts{action: "notify", cooldown: "5m", maxConcurrent: 1, enabled: true}

	// Valid regexp ⇒ persisted verbatim; complements an exact ref.
	pattern := `^(main|refs/merge-requests/[0-9]+/train)$`
	fields, _, err := buildGitLabPipelineFields(base, "p/p", "failed", "", pattern, "", "")
	if err != nil {
		t.Fatalf("valid ref-pattern: %v", err)
	}
	if fields["pipeline_ref_pattern"] != pattern {
		t.Errorf("pipeline_ref_pattern = %v, want %q", fields["pipeline_ref_pattern"], pattern)
	}

	// Both exact ref and pattern may coexist (pattern narrows further).
	fields, _, err = buildGitLabPipelineFields(base, "p/p", "failed", "main", "^main$", "", "")
	if err != nil {
		t.Fatalf("ref + ref-pattern: %v", err)
	}
	if fields["pipeline_ref"] != "main" || fields["pipeline_ref_pattern"] != "^main$" {
		t.Errorf("expected both pipeline_ref and pipeline_ref_pattern set, got %v", fields)
	}

	// Invalid regexp ⇒ error at authoring time.
	if _, _, err := buildGitLabPipelineFields(base, "p/p", "failed", "", "[unterminated", "", ""); err == nil {
		t.Error("--ref-pattern with invalid regexp: expected error, got nil")
	}
}

// TestBuildGitLabPipelineFields_MaxAge covers the recency-window (--max-age)
// flag: empty omits the field (engine applies its default), a valid duration
// is persisted verbatim, and an invalid / non-positive value fails at
// authoring time rather than silently reopening the spawn-storm hole.
func TestBuildGitLabPipelineFields_MaxAge(t *testing.T) {
	base := commonTriggerOpts{action: "notify", cooldown: "5m", maxConcurrent: 1, enabled: true}

	// Empty ⇒ field omitted (engine default applies).
	fields, _, err := buildGitLabPipelineFields(base, "p/p", "failed", "main", "", "", "")
	if err != nil {
		t.Fatalf("empty max-age: %v", err)
	}
	if _, present := fields["pipeline_max_age"]; present {
		t.Errorf("expected pipeline_max_age omitted when --max-age empty, got %v", fields["pipeline_max_age"])
	}

	// Valid duration ⇒ persisted verbatim.
	fields, _, err = buildGitLabPipelineFields(base, "p/p", "failed", "main", "", "30m", "")
	if err != nil {
		t.Fatalf("valid max-age: %v", err)
	}
	if fields["pipeline_max_age"] != "30m" {
		t.Errorf("pipeline_max_age = %v, want 30m", fields["pipeline_max_age"])
	}

	// Invalid / non-positive ⇒ error at authoring time.
	for _, bad := range []string{"abc", "0", "-1h"} {
		if _, _, err := buildGitLabPipelineFields(base, "p/p", "failed", "main", "", bad, ""); err == nil {
			t.Errorf("--max-age %q: expected error, got nil", bad)
		}
	}
}

// TestBuildGitLabPipelineFields_MinDuration covers the slow-pipeline threshold
// (--min-duration) flag: empty omits the field, a valid duration is persisted
// verbatim, and an invalid / non-positive value fails at authoring time rather
// than fail-closing at runtime (which would silently stop the trigger firing).
func TestBuildGitLabPipelineFields_MinDuration(t *testing.T) {
	base := commonTriggerOpts{action: "notify", cooldown: "5m", maxConcurrent: 1, enabled: true}

	// Empty ⇒ field omitted (no threshold, pre-feature behaviour).
	fields, _, err := buildGitLabPipelineFields(base, "p/p", "success", "", "", "", "")
	if err != nil {
		t.Fatalf("empty min-duration: %v", err)
	}
	if _, present := fields["min_duration"]; present {
		t.Errorf("expected min_duration omitted when --min-duration empty, got %v", fields["min_duration"])
	}

	// Valid duration ⇒ persisted verbatim.
	fields, _, err = buildGitLabPipelineFields(base, "p/p", "success", "", "", "", "45m")
	if err != nil {
		t.Fatalf("valid min-duration: %v", err)
	}
	if fields["min_duration"] != "45m" {
		t.Errorf("min_duration = %v, want 45m", fields["min_duration"])
	}

	// Invalid / non-positive ⇒ error at authoring time.
	for _, bad := range []string{"abc", "0", "-5m"} {
		if _, _, err := buildGitLabPipelineFields(base, "p/p", "success", "", "", "", bad); err == nil {
			t.Errorf("--min-duration %q: expected error, got nil", bad)
		}
	}
}

func TestBuildGitLabMRFields(t *testing.T) {
	opts := commonTriggerOpts{action: "notify", cooldown: "5m", maxConcurrent: 1, enabled: true}
	fields, labels, err := buildGitLabMRFields(opts, "p/p", "merged", []string{"needs-review", "high"}, "alice", "main")
	if err != nil {
		t.Fatalf("buildGitLabMRFields: %v", err)
	}
	if fields["event_type"] != "merge_request" {
		t.Errorf("event_type = %q", fields["event_type"])
	}
	if fields["project_path"] != "p/p" {
		t.Errorf("project_path = %q", fields["project_path"])
	}
	if fields["state"] != "merged" {
		t.Errorf("state = %q", fields["state"])
	}
	if fields["gitlab_labels"] != "needs-review,high" {
		t.Errorf("gitlab_labels = %q", fields["gitlab_labels"])
	}
	if fields["author"] != "alice" {
		t.Errorf("author = %q", fields["author"])
	}
	if fields["target_branch"] != "main" {
		t.Errorf("target_branch = %q", fields["target_branch"])
	}
	if !containsString(labels, "trigger-source:gitlab-mr") {
		t.Errorf("labels missing trigger-source:gitlab-mr: %v", labels)
	}
}

func TestBuildJiraFields(t *testing.T) {
	opts := commonTriggerOpts{action: "spawn_agent", role: "crew", cooldown: "5m", maxConcurrent: 1, enabled: true}
	fields, labels, err := buildJiraFields(opts, "project = DEVOPS AND status = Open", "DEVOPS")
	if err != nil {
		t.Fatalf("buildJiraFields: %v", err)
	}
	if fields["jql"] != "project = DEVOPS AND status = Open" {
		t.Errorf("jql = %q", fields["jql"])
	}
	if fields["jira_project"] != "DEVOPS" {
		t.Errorf("jira_project = %q", fields["jira_project"])
	}
	if !containsString(labels, "trigger:jira") {
		t.Errorf("labels missing trigger:jira: %v", labels)
	}
}

func TestBuildWebhookFields(t *testing.T) {
	opts := commonTriggerOpts{action: "notify", cooldown: "0", maxConcurrent: 5, enabled: false}
	fields, labels, err := buildWebhookFields(opts, "deploy-failed", "supersecret",
		[]string{"event.type:==:deploy_failed", "severity:in:high,critical"})
	if err != nil {
		t.Fatalf("buildWebhookFields: %v", err)
	}
	if fields["webhook_token"] != "deploy-failed" {
		t.Errorf("webhook_token = %v", fields["webhook_token"])
	}
	if fields["webhook_secret"] != "supersecret" {
		t.Errorf("webhook_secret = %v", fields["webhook_secret"])
	}
	if fields["enabled"] != false {
		t.Errorf("enabled = %v, want false", fields["enabled"])
	}
	condStr, _ := fields["conditions"].(string)
	var got []webhookConditionJSON
	if err := json.Unmarshal([]byte(condStr), &got); err != nil {
		t.Fatalf("conditions JSON: %v (raw: %s)", err, condStr)
	}
	want := []webhookConditionJSON{
		{Path: "event.type", Op: "==", Value: "deploy_failed"},
		{Path: "severity", Op: "in", Value: "high,critical"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("conditions = %#v, want %#v", got, want)
	}
	if !containsString(labels, "trigger:webhook") {
		t.Errorf("labels missing trigger:webhook: %v", labels)
	}
}

func TestParseWebhookConditions_Errors(t *testing.T) {
	cases := []string{
		"missingop",   // no colon
		":op:value",   // empty path
		"path::value", // empty op
	}
	for _, c := range cases {
		if _, err := parseWebhookConditions([]string{c}); err == nil {
			t.Errorf("parseWebhookConditions(%q): expected error", c)
		}
	}
}

func TestParseWebhookConditions_PresentAbsent(t *testing.T) {
	got, err := parseWebhookConditions([]string{"alert.id:present", "removed:absent"})
	if err != nil {
		t.Fatalf("parseWebhookConditions: %v", err)
	}
	want := []webhookConditionJSON{
		{Path: "alert.id", Op: "present", Value: ""},
		{Path: "removed", Op: "absent", Value: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v want %#v", got, want)
	}
}

func TestUniqueStringsPreservesOrder(t *testing.T) {
	got := uniqueStrings([]string{"a", "b", "a", "", "c", "b"})
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("uniqueStrings = %v, want %v", got, want)
	}
}

func TestGenerateTriggerTitle(t *testing.T) {
	cases := []struct {
		name   string
		label  string
		fields map[string]any
		want   string
	}{
		{"gitlab pipeline", "trigger:gitlab", map[string]any{"event_type": "pipeline", "project_path": "p/p", "pipeline_status": "failed"}, "GitLab pipeline trigger: p/p (failed)"},
		{"gitlab mr", "trigger:gitlab", map[string]any{"event_type": "merge_request", "project_path": "p/p"}, "GitLab MR trigger: p/p"},
		{"jira w/ project", "trigger:jira", map[string]any{"jira_project": "DEVOPS"}, "JIRA trigger: DEVOPS"},
		{"jira w/o project", "trigger:jira", map[string]any{}, "JIRA trigger"},
		{"webhook", "trigger:webhook", map[string]any{"webhook_token": "foo"}, "Webhook trigger: foo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := generateTriggerTitle(c.label, c.fields); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestTriggerSourceFromLabels(t *testing.T) {
	cases := []struct {
		labels []string
		want   string
	}{
		{[]string{"trigger:gitlab", "project:monorepo"}, "gitlab"},
		{[]string{"project:foo", "trigger:webhook"}, "webhook"},
		{[]string{"project:foo"}, "?"},
	}
	for _, c := range cases {
		got := triggerSourceFromLabels(c.labels)
		if got != c.want {
			t.Errorf("triggerSourceFromLabels(%v) = %q, want %q", c.labels, got, c.want)
		}
	}
}

// TestTriggerBeadLabels guards the field→label mirroring (ka-0000002971): a
// spawn_agent trigger sets project/role only as fields, and the persisted bead
// must still carry the matching project:/role: LABELS so a label-only
// ground-truth query (the maps-beauty fleet audit, `kd list -l role:gardener`)
// agrees with the field-OR-label viewer query — node_vs_truth_delta == 0.
func TestTriggerBeadLabels(t *testing.T) {
	t.Run("spawn_agent gardener gets role:gardener + project label from fields", func(t *testing.T) {
		fields := map[string]any{
			"action":  "spawn_agent",
			"role":    "gardener",
			"project": "gardenio",
			"cron":    "11,41 * * * *",
		}
		got := triggerBeadLabels("trigger:schedule", fields, nil, nil)
		for _, want := range []string{"trigger:schedule", "role:gardener", "project:gardenio"} {
			if !containsString(got, want) {
				t.Errorf("labels %v missing %q", got, want)
			}
		}
	})

	t.Run("non-gardener role mirrors too", func(t *testing.T) {
		fields := map[string]any{"action": "spawn_agent", "role": "crew"}
		got := triggerBeadLabels("trigger:schedule", fields, nil, nil)
		if !containsString(got, "role:crew") {
			t.Errorf("labels %v missing role:crew", got)
		}
	})

	t.Run("no role field => no role label, no dupes", func(t *testing.T) {
		fields := map[string]any{"action": "notify"}
		got := triggerBeadLabels("trigger:schedule", fields, []string{"trigger:schedule"}, []string{"project:gardenio"})
		for _, l := range got {
			if strings.HasPrefix(l, "role:") {
				t.Errorf("unexpected role label %q in %v", l, got)
			}
		}
		// uniqueStrings must dedupe the doubled trigger:schedule.
		n := 0
		for _, l := range got {
			if l == "trigger:schedule" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("trigger:schedule appears %d times, want 1: %v", n, got)
		}
	})
}

// TestTriggerBeadLabelsMultiPromotion is the mislabel guardrail (bug
// ka-0000001688): a gardener authored with a NON-EMPTY triggers[] array must be
// labelled trigger:multi, never trigger:schedule. A trigger:schedule bead only
// fires its flat `cron` field — its triggers[] event wakes are silently dropped
// by trigger_source_schedule.go — so a schedule-labelled bead with empty cron +
// event triggers[] never fires. The promotion happens at the triggerBeadLabels
// chokepoint so the whole class cannot recur.
func TestTriggerBeadLabelsMultiPromotion(t *testing.T) {
	const multiTriggers = `[
		{"kind":"bead_event","events":["closed","updated"],
		 "match":{"all":[{"field":"parent","op":"==","value":"gd-EPIC"}]},
		 "throttle":"120s"},
		{"kind":"cron","cron":"23 17 * * 1","timezone":"America/Los_Angeles"}
	]`

	t.Run("non-empty triggers[] promotes trigger:schedule => trigger:multi", func(t *testing.T) {
		fields := map[string]any{
			"action":   "spawn_agent",
			"role":     "gardener",
			"project":  "gardenio",
			"cron":     "", // empty cron is the dangerous case
			"triggers": multiTriggers,
		}
		got := triggerBeadLabels("trigger:schedule", fields, nil, nil)
		if !containsString(got, "trigger:multi") {
			t.Errorf("labels %v missing trigger:multi (non-empty triggers[] must promote)", got)
		}
		if containsString(got, "trigger:schedule") {
			t.Errorf("labels %v still carry trigger:schedule — the mislabel guardrail did not promote", got)
		}
		// The field→label mirroring still applies on the promoted bead.
		for _, want := range []string{"role:gardener", "project:gardenio"} {
			if !containsString(got, want) {
				t.Errorf("labels %v missing %q after promotion", got, want)
			}
		}
	})

	t.Run("promotion also rewrites a caller-passed trigger:schedule source label", func(t *testing.T) {
		fields := map[string]any{"action": "spawn_agent", "role": "gardener", "triggers": multiTriggers}
		got := triggerBeadLabels("trigger:schedule", fields, []string{"trigger:schedule"}, nil)
		if containsString(got, "trigger:schedule") {
			t.Errorf("labels %v still carry trigger:schedule via sourceLabels — both must promote", got)
		}
		if !containsString(got, "trigger:multi") {
			t.Errorf("labels %v missing trigger:multi", got)
		}
	})

	t.Run("empty triggers[] stays trigger:schedule (legit single-cron gardener)", func(t *testing.T) {
		fields := map[string]any{
			"action":   "spawn_agent",
			"role":     "gardener",
			"cron":     "23 16 * * *",
			"triggers": "[]",
		}
		got := triggerBeadLabels("trigger:schedule", fields, nil, nil)
		if !containsString(got, "trigger:schedule") {
			t.Errorf("labels %v should keep trigger:schedule for an empty triggers[] bead", got)
		}
		if containsString(got, "trigger:multi") {
			t.Errorf("labels %v wrongly promoted an empty triggers[] bead to trigger:multi", got)
		}
	})

	t.Run("no triggers field stays trigger:schedule", func(t *testing.T) {
		fields := map[string]any{"action": "spawn_agent", "role": "gardener", "cron": "13 13 * * *"}
		got := triggerBeadLabels("trigger:schedule", fields, nil, nil)
		if !containsString(got, "trigger:schedule") {
			t.Errorf("labels %v should keep trigger:schedule when no triggers field is set", got)
		}
		if containsString(got, "trigger:multi") {
			t.Errorf("labels %v wrongly promoted a no-triggers bead", got)
		}
	})

	t.Run("malformed triggers[] does not promote (fail safe to schedule)", func(t *testing.T) {
		fields := map[string]any{"action": "spawn_agent", "triggers": "{not-an-array"}
		got := triggerBeadLabels("trigger:schedule", fields, nil, nil)
		if containsString(got, "trigger:multi") {
			t.Errorf("labels %v promoted on unparseable triggers[] — must fail safe to trigger:schedule", got)
		}
	})

	t.Run("non-schedule source label is never promoted", func(t *testing.T) {
		fields := map[string]any{"action": "notify", "triggers": multiTriggers}
		got := triggerBeadLabels("trigger:gitlab", fields, nil, nil)
		if !containsString(got, "trigger:gitlab") {
			t.Errorf("labels %v should preserve a non-schedule source label", got)
		}
		if containsString(got, "trigger:multi") {
			t.Errorf("labels %v wrongly rewrote a trigger:gitlab source label", got)
		}
	})
}

// containsString is a tiny test helper.
func containsString(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

// TestGlabMRListFlagForState pins the GitLab-API-state → glab-CLI-flag mapping
// used to print a RUNNABLE copy-paste "test" hint on gitlab-mr trigger create.
// glab has NO gh-style `--state` flag (ka-0000003906): the printed command must
// use bare list / -M / -c / -A, never `--state <value>`.
func TestGlabMRListFlagForState(t *testing.T) {
	cases := []struct {
		state string
		want  string
	}{
		{"", ""},       // default → open (bare list)
		{"opened", ""}, // open → bare list
		{"merged", "--merged"},
		{"closed", "--closed"},
		{"all", "--all"},
		{"bogus", ""}, // unknown → safe default (open)
	}
	for _, c := range cases {
		t.Run(c.state, func(t *testing.T) {
			got := glabMRListFlagForState(c.state)
			if got != c.want {
				t.Errorf("glabMRListFlagForState(%q) = %q, want %q", c.state, got, c.want)
			}
			// The rendered hint must never contain a gh-style --state flag.
			hint := strings.TrimSpace("glab -R proj mr list " + got)
			if strings.Contains(hint, "--state") {
				t.Errorf("rendered hint %q must not contain a gh-style --state flag", hint)
			}
		})
	}
}
