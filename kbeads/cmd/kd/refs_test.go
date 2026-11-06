package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

func TestRefsMentionedBy_Table(t *testing.T) {
	mc := newMockClient()
	mc.MentionedBy["gd-target00001"] = []*model.BeadReferenceSummary{
		{ID: "gd-ldkpaoah1s", Title: "alpha", Type: "task", Status: "open", SourceField: "description"},
		{ID: "gd-hsr6nhjuuy", Title: "beta", Type: "epic", Status: "in_progress", SourceField: "comment:7"},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := refsMentionedByCmd.RunE(refsMentionedByCmd, []string{"gd-target00001"}); err != nil {
			t.Fatalf("refsMentionedByCmd: %v", err)
		}
	})

	for _, want := range []string{"MENTIONED_BY", "gd-ldkpaoah1s", "alpha", "task", "open", "description", "gd-hsr6nhjuuy", "comment:7"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
	if got := mc.MentionedByCalls; len(got) != 1 || got[0] != "gd-target00001" {
		t.Errorf("MentionedByCalls = %v, want [gd-target00001]", got)
	}
}

func TestRefsMentions_DanglingTargetRendersDash(t *testing.T) {
	mc := newMockClient()
	mc.Mentions["gd-ldkpaoah1s"] = []*model.BeadReferenceSummary{
		{ID: "gd-FNHTKRBDIQ", Title: "real", Type: "task", Status: "open", SourceField: "description"},
		{ID: "gd-ZACQZ1SGLD", Title: "", Type: "", Status: "", SourceField: "description"},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := refsMentionsCmd.RunE(refsMentionsCmd, []string{"gd-ldkpaoah1s"}); err != nil {
			t.Fatalf("refsMentionsCmd: %v", err)
		}
	})

	if !strings.Contains(out, "MENTIONS") {
		t.Errorf("expected MENTIONS header, got:\n%s", out)
	}
	if !strings.Contains(out, "gd-ZACQZ1SGLD") {
		t.Errorf("expected dangling target id, got:\n%s", out)
	}
	// refOrDash should render the missing fields as "—" so the row doesn't
	// collapse — check at least one dash made it onto the dangling line.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "gd-ZACQZ1SGLD") && !strings.Contains(line, "—") {
			t.Errorf("dangling target row missing '—' placeholder: %q", line)
		}
	}
}

func TestRefsMentionedBy_NoResults(t *testing.T) {
	mc := newMockClient()

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := refsMentionedByCmd.RunE(refsMentionedByCmd, []string{"gd-nobody00001"}); err != nil {
			t.Fatalf("refsMentionedByCmd: %v", err)
		}
	})
	if !strings.Contains(out, "No references found.") {
		t.Errorf("expected empty notice, got:\n%s", out)
	}
}

func TestRefsMentions_ClientError(t *testing.T) {
	mc := newMockClient()
	mc.MentionsErr = errors.New("rpc failed")

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := refsMentionsCmd.RunE(refsMentionsCmd, []string{"gd-ldkpaoah1s"})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "rpc failed") {
		t.Errorf("error = %v, want it to wrap 'rpc failed'", err)
	}
}

func TestRefOrDash(t *testing.T) {
	if got := refOrDash(""); got != "—" {
		t.Errorf("refOrDash(\"\") = %q, want '—'", got)
	}
	if got := refOrDash("task"); got != "task" {
		t.Errorf("refOrDash(\"task\") = %q, want unchanged", got)
	}
}

// TestRefsMentionedBy_TypeFilter_Verb checks that --type=parent threads
// through to the client as the typed-mention filter. Acceptance criterion
// from gd-rZchVhjXC3 (T6, kbeads/docs/research-typed-mention-syntax.md § 10).
func TestRefsMentionedBy_TypeFilter_Verb(t *testing.T) {
	mc := newMockClient()
	parentRel := "parent"
	mc.MentionedBy["gd-target00001"] = []*model.BeadReferenceSummary{
		{ID: "gd-qcmoiejzjo", Title: "typed child", Type: "task", Status: "open", SourceField: "description", RelationshipType: &parentRel},
		{ID: "gd-untyped0001", Title: "plain mention", Type: "task", Status: "open", SourceField: "description"},
	}

	withMockClient(t, mc)

	// Reset the flag between subtests so other tests don't see leftover state.
	t.Cleanup(func() { _ = refsMentionedByCmd.Flags().Set("type", "") })

	if err := refsMentionedByCmd.Flags().Set("type", "parent"); err != nil {
		t.Fatalf("set --type: %v", err)
	}
	out := captureStdout(t, func() {
		if err := refsMentionedByCmd.RunE(refsMentionedByCmd, []string{"gd-target00001"}); err != nil {
			t.Fatalf("refsMentionedByCmd: %v", err)
		}
	})

	if mc.MentionedByRelType == nil || *mc.MentionedByRelType != "parent" {
		t.Fatalf("client received MentionedByRelType=%v, want pointer to 'parent'", mc.MentionedByRelType)
	}
	if !strings.Contains(out, "gd-qcmoiejzjo") {
		t.Errorf("expected typed row in output, got:\n%s", out)
	}
	if strings.Contains(out, "gd-untyped0001") {
		t.Errorf("untyped row should not appear when --type=parent, got:\n%s", out)
	}
}

// TestRefsMentions_TypeFilter_NullOnly checks --type=mentions (alias for "")
// passes a non-nil empty-string filter through, narrowing the page to
// untyped (relationship_type IS NULL) rows.
func TestRefsMentions_TypeFilter_NullOnly(t *testing.T) {
	mc := newMockClient()
	parentRel := "parent"
	mc.Mentions["gd-ldkpaoah1s"] = []*model.BeadReferenceSummary{
		{ID: "gd-qcmoiejzjo", Title: "typed", Type: "task", Status: "open", SourceField: "description", RelationshipType: &parentRel},
		{ID: "gd-untyped0001", Title: "untyped", Type: "task", Status: "open", SourceField: "description"},
	}

	withMockClient(t, mc)
	t.Cleanup(func() { _ = refsMentionsCmd.Flags().Set("type", "") })

	if err := refsMentionsCmd.Flags().Set("type", "mentions"); err != nil {
		t.Fatalf("set --type=mentions: %v", err)
	}
	out := captureStdout(t, func() {
		if err := refsMentionsCmd.RunE(refsMentionsCmd, []string{"gd-ldkpaoah1s"}); err != nil {
			t.Fatalf("refsMentionsCmd: %v", err)
		}
	})

	if mc.MentionsRelType == nil || *mc.MentionsRelType != "" {
		t.Fatalf("client received MentionsRelType=%v, want pointer to empty string (NULL-only)", mc.MentionsRelType)
	}
	if !strings.Contains(out, "gd-untyped0001") {
		t.Errorf("expected untyped row in output, got:\n%s", out)
	}
	if strings.Contains(out, "gd-qcmoiejzjo") {
		t.Errorf("typed row should not appear when --type=mentions, got:\n%s", out)
	}
}

// TestRefsMentions_NoTypeFlag_NoFilter ensures the CLI sends a nil pointer
// (no filter) when the flag is unset, preserving back-compat with the
// untyped read path.
func TestRefsMentions_NoTypeFlag_NoFilter(t *testing.T) {
	mc := newMockClient()
	mc.Mentions["gd-ldkpaoah1s"] = []*model.BeadReferenceSummary{
		{ID: "gd-qcmoiejzjo", Title: "t", Type: "task", Status: "open", SourceField: "description", RelationshipType: strPtr("parent")},
		{ID: "gd-untyped0001", Title: "u", Type: "task", Status: "open", SourceField: "description"},
	}

	withMockClient(t, mc)
	// Defensive: ensure --type starts unset.
	_ = refsMentionsCmd.Flags().Set("type", "")
	refsMentionsCmd.Flags().Lookup("type").Changed = false

	captureStdout(t, func() {
		if err := refsMentionsCmd.RunE(refsMentionsCmd, []string{"gd-ldkpaoah1s"}); err != nil {
			t.Fatalf("refsMentionsCmd: %v", err)
		}
	})

	if mc.MentionsRelType != nil {
		t.Fatalf("MentionsRelType = %v, want nil (no filter)", *mc.MentionsRelType)
	}
}

// TestRefsMentions_JSON_RelationshipTypeRoundTrip is the acceptance test for
// the JSON surface from gd-rZchVhjXC3: typed rows render as
// "relationship_type": "<verb>", untyped rows render as
// "relationship_type": null. Both shapes appear in `kd refs mentions --json`
// output.
func TestRefsMentions_JSON_RelationshipTypeRoundTrip(t *testing.T) {
	mc := newMockClient()
	parentRel := "parent"
	mc.Mentions["gd-ldkpaoah1s"] = []*model.BeadReferenceSummary{
		{ID: "gd-FNHTKRBDIQ", Title: "typed", Type: "task", Status: "open", SourceField: "description", RelationshipType: &parentRel},
		{ID: "gd-HKBGWAOTHB", Title: "untyped", Type: "task", Status: "open", SourceField: "description"},
	}

	withMockClient(t, mc)
	origJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = origJSON })

	out := captureStdout(t, func() {
		if err := refsMentionsCmd.RunE(refsMentionsCmd, []string{"gd-ldkpaoah1s"}); err != nil {
			t.Fatalf("refsMentionsCmd: %v", err)
		}
	})

	// Parse the JSON envelope and assert on the relationship_type field of
	// each row. We unmarshal into the same shape the client serializes so
	// the test fails loudly if the JSON tag drifts.
	type refRow struct {
		ID               string  `json:"id"`
		RelationshipType *string `json:"relationship_type"`
	}
	var resp struct {
		References []refRow `json:"references"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal JSON output: %v\nraw:\n%s", err, out)
	}
	if len(resp.References) != 2 {
		t.Fatalf("len(refs) = %d, want 2; raw:\n%s", len(resp.References), out)
	}
	if resp.References[0].RelationshipType == nil || *resp.References[0].RelationshipType != "parent" {
		t.Errorf("typed row relationship_type = %v, want 'parent'", resp.References[0].RelationshipType)
	}
	if resp.References[1].RelationshipType != nil {
		t.Errorf("untyped row relationship_type = %v, want null", *resp.References[1].RelationshipType)
	}
	// Belt-and-suspenders: the literal `"relationship_type": null` substring
	// must be present (catches a regression where the field is omitted on
	// nil instead of rendered as null).
	if !strings.Contains(out, `"relationship_type": null`) {
		t.Errorf("expected literal `\"relationship_type\": null` in JSON, got:\n%s", out)
	}
	if !strings.Contains(out, `"relationship_type": "parent"`) {
		t.Errorf("expected literal `\"relationship_type\": \"parent\"` in JSON, got:\n%s", out)
	}
}
