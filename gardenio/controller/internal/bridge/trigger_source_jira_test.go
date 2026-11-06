package bridge

import (
	"context"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestJiraTriggerSource_Name pins the source's stable string identifier.
// The TriggerEngine routes by Name(), so a typo here would silently break
// every JIRA trigger.
func TestJiraTriggerSource_Name(t *testing.T) {
	src := NewJiraTriggerSource(nil)
	if got := src.Name(); got != "jira" {
		t.Errorf("Name() = %q, want %q", got, "jira")
	}
}

func TestJiraTriggerSource_LoadTriggers_FiltersDisabledAndMissingJQL(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: triggerBeads(
			triggerBead("t1", "Normal JIRA trigger", "trigger:jira", map[string]string{
				"jql":    "project = PE AND status = Open",
				"action": "spawn_agent",
			}),
			triggerBead("t2", "Disabled trigger", "trigger:jira", map[string]string{
				"jql":     "project = FSM",
				"enabled": "false",
			}),
			triggerBead("t3", "Missing JQL", "trigger:jira", map[string]string{
				"action": "notify",
			}),
			// Different source label — must be excluded by label filter.
			triggerBead("t4", "Bead trigger", "trigger:bead", map[string]string{
				"jql": "ignored",
			}),
		),
	}

	src := NewJiraTriggerSource(nil)
	triggers, err := src.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers: %v", err)
	}

	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger (t1 only), got %d", len(triggers))
	}
	if triggers[0].ID != "t1" {
		t.Errorf("triggers[0].ID = %q, want t1", triggers[0].ID)
	}
	data, ok := triggers[0].SourceData.(jiraTriggerData)
	if !ok {
		t.Fatalf("SourceData type = %T, want jiraTriggerData", triggers[0].SourceData)
	}
	if data.JQL != "project = PE AND status = Open" {
		t.Errorf("JQL = %q", data.JQL)
	}
}

func TestJiraTriggerSource_CatchUpKeys_ReturnsJiraKeysFromMirrors(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: triggerBeads(
			triggerBeadWithKind("m1", "PE-100 mirror", "issue", "trigger:my-trig", map[string]string{
				"jira_key": "PE-100",
			}),
			triggerBeadWithKind("m2", "PE-200 mirror", "issue", "trigger:my-trig", map[string]string{
				"jira_key": "PE-200",
			}),
			triggerBeadWithKind("m3", "No jira_key", "issue", "trigger:my-trig", map[string]string{}),
		),
	}

	src := NewJiraTriggerSource(nil)
	keys, err := src.CatchUpKeys(context.Background(), daemon, "my-trig")
	if err != nil {
		t.Fatalf("CatchUpKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d (%v)", len(keys), keys)
	}
	want := map[string]bool{"PE-100": true, "PE-200": true}
	for _, k := range keys {
		if !want[k] {
			t.Errorf("unexpected key %q", k)
		}
	}
}

// TestJiraIssueToMatch_PopulatesTemplateVars exercises the helper that turns
// a JiraIssue into a TriggerMatch with every {{jira_*}} template var the
// notifier and spawn action expand.
func TestJiraIssueToMatch_PopulatesTemplateVars(t *testing.T) {
	issue := JiraIssue{
		Key: "PE-1234",
		Fields: JiraIssueFields{
			Summary:   "Fix flaky e2e",
			Status:    &JiraNamedRef{Name: "In Progress"},
			IssueType: &JiraNamedRef{Name: "Bug"},
			Priority:  &JiraNamedRef{Name: "P1"},
			Reporter:  &JiraUser{DisplayName: "Alice"},
			Assignee:  &JiraUser{DisplayName: "Bob"},
			Labels:    []string{"flake", "e2e"},
		},
	}

	m := jiraIssueToMatch(issue)

	if m.Key != "PE-1234" {
		t.Errorf("Key = %q", m.Key)
	}
	if m.Title != "Fix flaky e2e" {
		t.Errorf("Title = %q", m.Title)
	}

	cases := map[string]string{
		"{{jira_key}}":       "PE-1234",
		"{{jira_key_lower}}": "pe-1234",
		"{{jira_summary}}":   "Fix flaky e2e",
		"{{jira_url}}":       "https://acme.atlassian.net/browse/PE-1234",
		"{{jira_status}}":    "In Progress",
		"{{jira_type}}":      "Bug",
		"{{jira_priority}}":  "P1",
		"{{jira_reporter}}":  "Alice",
		"{{jira_assignee}}":  "Bob",
		"{{jira_labels}}":    "flake, e2e",
	}
	for k, want := range cases {
		if got := m.TemplateVars[k]; got != want {
			t.Errorf("TemplateVars[%q] = %q, want %q", k, got, want)
		}
	}

	// Mirror-bead fields used for later lineage / persistence.
	if m.Fields["jira_key"] != "PE-1234" {
		t.Errorf("Fields[jira_key] = %q", m.Fields["jira_key"])
	}
	if m.Fields["jira_url"] != "https://acme.atlassian.net/browse/PE-1234" {
		t.Errorf("Fields[jira_url] = %q", m.Fields["jira_url"])
	}

	// Source label feeds CatchUpKeys and mirror lookups.
	foundLabel := false
	for _, l := range m.Labels {
		if l == "jira:PE-1234" {
			foundLabel = true
		}
	}
	if !foundLabel {
		t.Errorf("missing label jira:PE-1234, got %v", m.Labels)
	}
}

func TestJiraIssueToMatch_UnassignedFallback(t *testing.T) {
	issue := JiraIssue{
		Key: "PE-9",
		Fields: JiraIssueFields{
			Summary:  "Triage",
			Assignee: nil,
		},
	}
	m := jiraIssueToMatch(issue)
	if got := m.TemplateVars["{{jira_assignee}}"]; got != "Unassigned" {
		t.Errorf("assignee fallback = %q, want Unassigned", got)
	}
	if got := m.TemplateVars["{{jira_description}}"]; got != "" {
		t.Errorf("empty description = %q, want \"\"", got)
	}
}

// TestJiraIssueToMatch_CarriesReporterIdentity verifies the reporter's
// resolvable keys (accountId, email) are carried on the match fields so the
// trigger engine's MatchOperatorResolver can attribute an auto-spawn to the
// reporter (ka-0000001781). DisplayName alone is not a lookup key, so absent
// id/email means no reporter field is emitted.
func TestJiraIssueToMatch_CarriesReporterIdentity(t *testing.T) {
	withIDs := jiraIssueToMatch(JiraIssue{
		Key: "FSM-1",
		Fields: JiraIssueFields{
			Summary:  "has reporter",
			Reporter: &JiraUser{DisplayName: "Jane", AccountID: "acct-jane", EmailAddress: "jane@acme.example"},
		},
	})
	if got := withIDs.Fields["jira_reporter_id"]; got != "acct-jane" {
		t.Errorf("jira_reporter_id = %q, want acct-jane", got)
	}
	if got := withIDs.Fields["jira_reporter_email"]; got != "jane@acme.example" {
		t.Errorf("jira_reporter_email = %q, want jane@acme.example", got)
	}

	// A reporter with only a display name carries no resolvable keys.
	nameOnly := jiraIssueToMatch(JiraIssue{
		Key: "FSM-2",
		Fields: JiraIssueFields{
			Summary:  "display-name-only reporter",
			Reporter: &JiraUser{DisplayName: "Anon"},
		},
	})
	if _, ok := nameOnly.Fields["jira_reporter_id"]; ok {
		t.Errorf("jira_reporter_id should be absent for a display-name-only reporter")
	}
	if _, ok := nameOnly.Fields["jira_reporter_email"]; ok {
		t.Errorf("jira_reporter_email should be absent for a display-name-only reporter")
	}
}

// TestJiraUserToOperator pins the slack-then-oidc preference of the shared
// operator mapping (ka-0000001781) that both the @-mention and auto-spawn paths
// rely on.
func TestJiraUserToOperator(t *testing.T) {
	cases := []struct {
		name string
		u    *UserIdentity
		want string
	}{
		{"nil", nil, ""},
		{"slack preferred", &UserIdentity{SlackID: "U1", Email: "a@b.co"}, "slack:U1"},
		{"oidc fallback", &UserIdentity{Email: "a@b.co"}, "oidc:a@b.co"},
		{"neither", &UserIdentity{DisplayName: "x"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jiraUserToOperator(tc.u); got != tc.want {
				t.Errorf("jiraUserToOperator = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReporterEmailOperator pins the email→oidc fallback shape (ka-0000004653):
// only a well-formed email (dotted domain) becomes an "oidc:<email>" human
// operator; empty / malformed addresses return "" so a bogus reporter email
// never lands as a value the kbeads sanitizer would reject as
// header_non_human.
func TestReporterEmailOperator(t *testing.T) {
	cases := []struct {
		name  string
		email string
		want  string
	}{
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"well-formed", "fran.ortiz@acme.example", "oidc:fran.ortiz@acme.example"},
		{"trimmed", "  jane@example.com  ", "oidc:jane@example.com"},
		{"no domain dot", "bob@localhost", ""},
		{"no local part", "@acme.example", ""},
		{"no at", "not-an-email", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reporterEmailOperator(tc.email); got != tc.want {
				t.Errorf("reporterEmailOperator(%q) = %q, want %q", tc.email, got, tc.want)
			}
		})
	}
}

// TestJiraReporterOperatorResolver pins the auto-spawn attribution ladder
// (ka-0000004653 / ka-0000001781): a matched Jira issue's reporter is mapped to
// a HUMAN operator so the auto-spawned pod boots with a resolvable
// BEADS_OPERATED_BY.
//
//  1. Fleet-directory hit wins (slack:<id> / oidc:<fleet-email>).
//  2. Directory MISS falls back to the reporter's raw email as oidc:<email>
//     (the regression this test guards — previously returned "" and the pod
//     booted unattributed, hard-400ing every operator-attributed write).
//  3. No reporter identity, or an unusable email on a miss, returns "".
func TestJiraReporterOperatorResolver(t *testing.T) {
	users := &fakeJiraUserDir{
		byAccount: map[string]*UserIdentity{
			"acct-fleet": {BeadID: "gd-user-jane", SlackID: "U0JANE", Email: "jane@fleet.example.com"},
		},
		byEmail: map[string]*UserIdentity{
			"jane@fleet.example.com": {BeadID: "gd-user-jane", SlackID: "U0JANE", Email: "jane@fleet.example.com"},
		},
	}
	resolve := newJiraReporterOperatorResolver(users, slog.Default())

	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name:   "no reporter identity",
			fields: map[string]string{},
			want:   "",
		},
		{
			name:   "fleet user via accountId",
			fields: map[string]string{"jira_reporter_id": "acct-fleet", "jira_reporter_email": "jane@fleet.example.com"},
			want:   "slack:U0JANE",
		},
		{
			name:   "directory miss falls back to reporter email",
			fields: map[string]string{"jira_reporter_id": "acct-unknown", "jira_reporter_email": "fran.ortiz@acme.example"},
			want:   "oidc:fran.ortiz@acme.example",
		},
		{
			name:   "email-only reporter, directory miss, email fallback",
			fields: map[string]string{"jira_reporter_email": "someone@acme.example"},
			want:   "oidc:someone@acme.example",
		},
		{
			name:   "directory miss with unusable email returns empty",
			fields: map[string]string{"jira_reporter_id": "acct-unknown", "jira_reporter_email": "bob@localhost"},
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolve(context.Background(), TriggerMatch{Key: "DEVOPS-1", Fields: tc.fields})
			if got != tc.want {
				t.Errorf("resolver = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExpandIssueTemplate_RoundTrip ensures the legacy wrapper still expands
// the canonical placeholders the same way as ExpandTemplate over TemplateVars.
func TestExpandIssueTemplate_RoundTrip(t *testing.T) {
	issue := JiraIssue{
		Key: "OPS-7",
		Fields: JiraIssueFields{
			Summary: "Rotate creds",
			Status:  &JiraNamedRef{Name: "To Do"},
		},
	}
	tmpl := "Issue {{jira_key}}: {{jira_summary}} (status={{jira_status}})"
	got := expandIssueTemplate(tmpl, issue)
	want := "Issue OPS-7: Rotate creds (status=To Do)"
	if got != want {
		t.Errorf("expandIssueTemplate = %q, want %q", got, want)
	}
}

// triggerBeadWithKind is a small variant of triggerBead that sets Kind, which
// CatchUpKeys filters on (Kinds: ["issue"]).
func triggerBeadWithKind(id, title, kind, label string, fields map[string]string) *beadsapi.BeadDetail {
	b := triggerBead(id, title, label, fields)
	b.Kind = kind
	return b
}

// --- Project scoping (ka-0000006237) ---------------------------------------
//
// A sandbox jira-bridge misconfigured onto the prod daemon loaded prod
// trigger:jira beads and stamped their last_checked ahead of every prod poll,
// permanently tripping prod's cooldown gate (epic ka-0000005107). The
// JIRA_TRIGGER_PROJECTS scope guarantees two bridges sharing a daemon never
// load the same trigger bead.

// scopedTriggerBead builds a trigger:jira bead carrying an extra project label.
func scopedTriggerBead(id, projectLabel string) *beadsapi.BeadDetail {
	b := triggerBead(id, "scoped "+id, "trigger:jira", map[string]string{
		"jql":    "project = FSM",
		"action": "spawn_agent",
	})
	if projectLabel != "" {
		b.Labels = append(b.Labels, projectLabel)
	}
	return b
}

func TestJiraTriggerSource_LoadTriggers_ProjectScope(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: triggerBeads(
			scopedTriggerBead("t-mono", "project:monorepo"),
			scopedTriggerBead("t-other", "project:sandbox-lab"),
			scopedTriggerBead("t-unlabeled", ""),
		),
	}

	src := NewJiraTriggerSourceScoped(nil, []string{"monorepo"}, nil)
	triggers, err := src.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers: %v", err)
	}
	if len(triggers) != 1 {
		t.Fatalf("expected only the project:monorepo trigger, got %d: %+v", len(triggers), triggers)
	}
	if triggers[0].ID != "t-mono" {
		t.Errorf("triggers[0].ID = %q, want t-mono", triggers[0].ID)
	}
}

// TestJiraTriggerSource_LoadTriggers_MultiProjectScope pins that the filter
// is an OR over the configured projects.
func TestJiraTriggerSource_LoadTriggers_MultiProjectScope(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: triggerBeads(
			scopedTriggerBead("t-mono", "project:monorepo"),
			scopedTriggerBead("t-gb", "project:gardenio"),
			scopedTriggerBead("t-other", "project:sandbox-lab"),
		),
	}

	src := NewJiraTriggerSourceScoped(nil, []string{"monorepo", "gardenio"}, nil)
	triggers, err := src.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers: %v", err)
	}
	if len(triggers) != 2 {
		t.Fatalf("expected 2 in-scope triggers, got %d", len(triggers))
	}
}

// TestJiraTriggerSource_LoadTriggers_NoScopeLoadsAll pins back-compat: with
// no JIRA_TRIGGER_PROJECTS configured, every trigger:jira bead loads exactly
// as before — including unlabeled ones.
func TestJiraTriggerSource_LoadTriggers_NoScopeLoadsAll(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: triggerBeads(
			scopedTriggerBead("t-mono", "project:monorepo"),
			scopedTriggerBead("t-other", "project:sandbox-lab"),
			scopedTriggerBead("t-unlabeled", ""),
		),
	}

	src := NewJiraTriggerSource(nil)
	triggers, err := src.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers: %v", err)
	}
	if len(triggers) != 3 {
		t.Fatalf("unscoped source must load all triggers, got %d", len(triggers))
	}
}

// TestJiraTriggerSource_ScopeBlankEntriesIgnored pins that whitespace-only
// scope entries (e.g. a trailing comma in JIRA_TRIGGER_PROJECTS) do not
// accidentally activate or corrupt the filter.
func TestJiraTriggerSource_ScopeBlankEntriesIgnored(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: triggerBeads(
			scopedTriggerBead("t-unlabeled", ""),
		),
	}

	src := NewJiraTriggerSourceScoped(nil, []string{" ", ""}, nil)
	triggers, err := src.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers: %v", err)
	}
	if len(triggers) != 1 {
		t.Fatalf("blank-only scope must behave as unscoped, got %d triggers", len(triggers))
	}
}
