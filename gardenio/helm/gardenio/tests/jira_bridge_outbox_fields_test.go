package tests

import (
	"strings"
	"testing"
)

// GBFSM outbox field-id passthrough (Phase 6a author-ahead plumbing,
// gd-wm6WH9Meo7; epic ka-0000002637, Phase 6 ka-0000000885).
//
// jiraBridge.jira.{outboxAnalysisIssueType,outboxLabelsField,outboxVersionField}
// each guard a like-named env var (JIRA_OUTBOX_ANALYSIS_ISSUE_TYPE /
// JIRA_OUTBOX_LABELS_FIELD / JIRA_OUTBOX_VERSION_FIELD) consumed by
// cmd/jira-bridge/main.go. The load-bearing invariant is that with all three
// unset the default render omits them entirely, so prod's render is unchanged
// until the operator wires the prod field ids in at cutover.

// outboxFieldEnvVars are the exact env var names parsed in
// cmd/jira-bridge/main.go (os.Getenv). The names MUST match char-for-char.
var outboxFieldEnvVars = []string{
	"JIRA_OUTBOX_ANALYSIS_ISSUE_TYPE",
	"JIRA_OUTBOX_LABELS_FIELD",
	"JIRA_OUTBOX_VERSION_FIELD",
}

func TestJiraBridge_OutboxFieldIDs_DefaultOmitsAll(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/jira-bridge/deployment.yaml",
		"jiraBridge.enabled=true",
	)
	for _, name := range outboxFieldEnvVars {
		if strings.Contains(out, name) {
			t.Errorf("%s should NOT be present in the default render "+
				"(all three outbox field-id values unset); got:\n%s", name, out)
		}
	}
}

func TestJiraBridge_OutboxFieldIDs_EachInjectsWhenSet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key    string // values key under jiraBridge.jira
		envVar string // rendered env var name
		val    string // value to set
	}{
		{"outboxAnalysisIssueType", "JIRA_OUTBOX_ANALYSIS_ISSUE_TYPE", "GBFSM Analysis"},
		{"outboxLabelsField", "JIRA_OUTBOX_LABELS_FIELD", "customfield_12001"},
		{"outboxVersionField", "JIRA_OUTBOX_VERSION_FIELD", "customfield_12002"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			out := helmTemplate(t, "templates/jira-bridge/deployment.yaml",
				"jiraBridge.enabled=true",
				"jiraBridge.jira."+tc.key+"="+tc.val,
			)
			if !strings.Contains(out, "- name: "+tc.envVar) {
				t.Fatalf("%s env var should be present when jiraBridge.jira.%s is set; got:\n%s",
					tc.envVar, tc.key, out)
			}
			if !strings.Contains(out, `value: "`+tc.val+`"`) {
				t.Errorf("%s should carry the quoted value %q; got:\n%s", tc.envVar, tc.val, out)
			}
			// Setting only this key must not leak the other two.
			for _, other := range outboxFieldEnvVars {
				if other == tc.envVar {
					continue
				}
				if strings.Contains(out, other) {
					t.Errorf("setting only %s leaked %s into the render:\n%s", tc.key, other, out)
				}
			}
		})
	}
}

func TestJiraBridge_OutboxFieldIDs_AllThreeTogether(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/jira-bridge/deployment.yaml",
		"jiraBridge.enabled=true",
		"jiraBridge.jira.outboxAnalysisIssueType=GBFSM Analysis",
		"jiraBridge.jira.outboxLabelsField=customfield_12001",
		"jiraBridge.jira.outboxVersionField=customfield_12002",
	)
	for _, name := range outboxFieldEnvVars {
		if !strings.Contains(out, "- name: "+name) {
			t.Errorf("%s should be present when all three values are set; got:\n%s", name, out)
		}
	}
}
