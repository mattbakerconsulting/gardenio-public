package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func darkBead(id string, labels []string, fields map[string]string) *beadsapi.BeadDetail {
	if fields == nil {
		fields = map[string]string{}
	}
	return &beadsapi.BeadDetail{ID: id, Type: "task", Kind: "issue", Status: "open", Labels: labels, Fields: fields}
}

func TestPlanPromotionStamp_HappyPath(t *testing.T) {
	b := darkBead("bd-1", []string{"source:jira-dark", "jira-dark:PE-D7"}, nil)
	stamp, err := planPromotionStamp(b, "PE-4021", "https://acme.atlassian.net/", "oidc:matt")
	if err != nil {
		t.Fatal(err)
	}
	if stamp.NoOp {
		t.Fatal("unexpected no-op on a fresh promotion")
	}
	if stamp.Fields["jira_key"] != "PE-4021" || stamp.Fields["promoted_to_jira_key"] != "PE-4021" {
		t.Errorf("jira_key/promoted_to_jira_key not stamped: %#v", stamp.Fields)
	}
	if stamp.Fields["jira_url"] != "https://acme.atlassian.net/browse/PE-4021" {
		t.Errorf("jira_url = %v", stamp.Fields["jira_url"])
	}
	if stamp.Fields["promoted_by"] != "oidc:matt" {
		t.Errorf("promoted_by = %v", stamp.Fields["promoted_by"])
	}
	// Both dedup labels must be added.
	joined := strings.Join(stamp.AddLabels, ",")
	if !strings.Contains(joined, "source:jira") || !strings.Contains(joined, "jira:PE-4021") {
		t.Errorf("expected source:jira + jira:PE-4021 in AddLabels, got %v", stamp.AddLabels)
	}
}

func TestPlanPromotionStamp_IdempotentReplay(t *testing.T) {
	b := darkBead("bd-1",
		[]string{"source:jira-dark", "source:jira", "jira:PE-4021"},
		map[string]string{"promoted_to_jira_key": "PE-4021", "jira_key": "PE-4021"})
	stamp, err := planPromotionStamp(b, "PE-4021", "", "")
	if err != nil {
		t.Fatalf("idempotent replay must not error: %v", err)
	}
	if !stamp.NoOp {
		t.Fatalf("expected NoOp on same-key replay, got %#v", stamp)
	}
}

func TestPlanPromotionStamp_RefusesSecondUpstream(t *testing.T) {
	b := darkBead("bd-1",
		[]string{"source:jira-dark", "source:jira", "jira:PE-4021"},
		map[string]string{"promoted_to_jira_key": "PE-4021", "jira_key": "PE-4021"})
	if _, err := planPromotionStamp(b, "PE-9999", "", ""); err == nil {
		t.Fatal("expected refusal to re-promote to a different key")
	}
}

func TestPlanPromotionStamp_RefusesNonDark(t *testing.T) {
	b := darkBead("bd-1", []string{"source:jira", "jira:PE-1"}, map[string]string{"jira_key": "PE-1"})
	if _, err := planPromotionStamp(b, "PE-2", "", ""); err == nil {
		t.Fatal("expected refusal to promote a non-dark bead")
	}
}

func TestPlanPromotionStamp_RefusesSyntheticKey(t *testing.T) {
	b := darkBead("bd-1", []string{"source:jira-dark"}, nil)
	for _, bad := range []string{"PE-D7", "pear-1", "PE", "PE-0", "-1", "PE-01"} {
		if _, err := planPromotionStamp(b, bad, "", ""); err == nil {
			t.Errorf("expected %q to be rejected as an invalid upstream key", bad)
		}
	}
}

func TestPlanPromotionStamp_RefusesOverwriteNativeMirror(t *testing.T) {
	// source:jira-dark but already carries a different jira_key (native mirror
	// collision) with no promotion marker → refuse.
	b := darkBead("bd-1", []string{"source:jira-dark"}, map[string]string{"jira_key": "PE-111"})
	if _, err := planPromotionStamp(b, "PE-222", "", ""); err == nil {
		t.Fatal("expected refusal to overwrite an existing jira_key")
	}
}

func TestPlanPromotionStamp_NoDuplicateLabels(t *testing.T) {
	// Bead already carries source:jira (but not yet promoted): don't re-add it.
	b := darkBead("bd-1", []string{"source:jira-dark", "source:jira"}, nil)
	stamp, err := planPromotionStamp(b, "PE-5", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range stamp.AddLabels {
		if l == "source:jira" {
			t.Error("source:jira should not be re-added when already present")
		}
	}
	if len(stamp.AddLabels) != 1 || stamp.AddLabels[0] != "jira:PE-5" {
		t.Errorf("expected only jira:PE-5 to be added, got %v", stamp.AddLabels)
	}
}

func TestBuildDarkPromotionScript(t *testing.T) {
	script := buildDarkPromotionScript("bd-42", "PE", "Rotate signing key", "Task",
		"Line one\nHas a `backtick` and $var and 'quotes'.")
	// Structural invariants — the write happens exactly once, guarded.
	for _, want := range []string{
		"set -euo pipefail",
		"/rest/api/2/issue",
		"promoted_to_jira_key",         // idempotency pre-check
		"gb jira-dark stamp \"$BEAD\"", // self-stamp
		"BEAD='bd-42'",
		"PROJECT='PE'",
		"GBJIRADARKDESC",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\n---\n%s", want, script)
		}
	}
	// The description body is carried verbatim in the heredoc — never
	// command-substituted (backticks/$ must survive literally).
	if !strings.Contains(script, "Has a `backtick` and $var and 'quotes'.") {
		t.Errorf("description not carried verbatim:\n%s", script)
	}
	// Exactly one POST to the create endpoint.
	if n := strings.Count(script, "/rest/api/2/issue"); n != 1 {
		t.Errorf("expected exactly one create POST, found %d", n)
	}
}

func TestBuildDarkPromotionScript_DefaultIssueType(t *testing.T) {
	script := buildDarkPromotionScript("bd-1", "PE", "s", "", "d")
	if !strings.Contains(script, "ISSUETYPE='Task'") {
		t.Errorf("empty issuetype should default to Task:\n%s", script)
	}
}

func TestCheckPromotable(t *testing.T) {
	if err := checkPromotable(darkBead("bd-1", []string{"source:jira-dark"}, nil)); err != nil {
		t.Errorf("fresh dark bead should be promotable: %v", err)
	}
	if err := checkPromotable(darkBead("bd-1", []string{"project:gardenio"}, nil)); err == nil {
		t.Error("non-dark bead should not be promotable")
	}
	if err := checkPromotable(darkBead("bd-1", []string{"source:jira-dark"},
		map[string]string{"promoted_to_jira_key": "PE-1"})); err == nil {
		t.Error("already-promoted bead should not be re-promotable")
	}
}
