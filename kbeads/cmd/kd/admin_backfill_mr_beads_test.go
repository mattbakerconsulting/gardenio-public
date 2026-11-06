package main

import (
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

func taskWithMRFields(id string, fields map[string]any) *model.Bead {
	raw, _ := json.Marshal(fields)
	return &model.Bead{
		ID:     id,
		Type:   "task",
		Kind:   model.KindIssue,
		Status: model.StatusOpen,
		Fields: raw,
	}
}

func mrBead(id, mrURL, status string) *model.Bead {
	raw, _ := json.Marshal(map[string]any{"mr_url": mrURL})
	return &model.Bead{
		ID:     id,
		Type:   "merge-request",
		Kind:   model.KindIssue,
		Status: model.Status(status),
		Fields: raw,
	}
}

func resetMRBackfillFlags(t *testing.T) {
	t.Helper()
	if err := adminBackfillMRBeadsCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("reset dry-run: %v", err)
	}
	if err := adminBackfillMRBeadsCmd.Flags().Set("apply", "false"); err != nil {
		t.Fatalf("reset apply: %v", err)
	}
	if err := adminBackfillMRBeadsCmd.Flags().Set("limit", "0"); err != nil {
		t.Fatalf("reset limit: %v", err)
	}
}

func TestAdminBackfillMRBeads_CreatesNewMRBead(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{
		// Source: one task bead with mr_url, status=opened.
		{taskWithMRFields("gd-task-1", map[string]any{
			"mr_url":            "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/2497",
			"mr_state":          "opened",
			"gitlab_mr_iid":     "2497",
			"mr_title":          "feat(controller): something",
			"source_branch":     "feat/abc",
			"target_branch":     "main",
			"gitlab_project_id": "12345678",
		})},
		// findMRBead: empty (no existing MR bead).
		{},
		// Source page 2: empty (terminates pagination).
		{},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMRBackfillFlags(t)

	_ = adminBackfillMRBeadsCmd.Flags().Set("apply", "true")
	_ = adminBackfillMRBeadsCmd.Flags().Set("dry-run", "false")

	captureStdout(t, func() {
		if err := adminBackfillMRBeadsCmd.RunE(adminBackfillMRBeadsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.CreateBeadCalls); got != 1 {
		t.Fatalf("expected 1 CreateBead, got %d", got)
	}
	created := mc.CreateBeadCalls[0]
	if created.Type != "merge-request" {
		t.Errorf("CreateBead.Type = %q, want merge-request", created.Type)
	}
	if created.Kind != "issue" {
		t.Errorf("CreateBead.Kind = %q, want issue", created.Kind)
	}
	var f map[string]string
	if err := json.Unmarshal(created.Fields, &f); err != nil {
		t.Fatalf("CreateBead.Fields not a string map: %v", err)
	}
	if f["mr_url"] != "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/2497" {
		t.Errorf("CreateBead.Fields[mr_url] = %q", f["mr_url"])
	}
	if f["mr_state"] != "opened" {
		t.Errorf("CreateBead.Fields[mr_state] = %q, want opened", f["mr_state"])
	}
	if _, hasOutcome := f["mr_outcome"]; hasOutcome {
		t.Errorf("opened MR should not stamp mr_outcome, got %q", f["mr_outcome"])
	}

	if got := len(mc.CloseBeadCalls); got != 0 {
		t.Errorf("opened MR should not be closed, got %d Close calls", got)
	}

	if got := len(mc.AddDepCalls); got != 1 {
		t.Fatalf("expected 1 AddDependency, got %d", got)
	}
	dep := mc.AddDepCalls[0]
	if dep.BeadID != "gd-task-1" || dep.Type != "produces" {
		t.Errorf("AddDep = %+v, want produces edge from gd-task-1", dep)
	}
}

func TestAdminBackfillMRBeads_UpdatesExistingMRBead(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{
		{taskWithMRFields("gd-task-2", map[string]any{
			"mr_url":   "https://gitlab.com/x/y/-/merge_requests/1",
			"mr_state": "opened",
		})},
		// findMRBead returns existing MR bead.
		{mrBead("gd-mr-existing", "https://gitlab.com/x/y/-/merge_requests/1", "open")},
		// Source page 2 empty.
		{},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMRBackfillFlags(t)

	_ = adminBackfillMRBeadsCmd.Flags().Set("apply", "true")
	_ = adminBackfillMRBeadsCmd.Flags().Set("dry-run", "false")

	captureStdout(t, func() {
		if err := adminBackfillMRBeadsCmd.RunE(adminBackfillMRBeadsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.CreateBeadCalls); got != 0 {
		t.Errorf("existing MR bead should not be re-created, got %d Create calls", got)
	}
	if got := len(mc.UpdateBeadCalls); got != 1 {
		t.Fatalf("expected 1 UpdateBead, got %d", got)
	}
	if mc.UpdateBeadCalls[0].ID != "gd-mr-existing" {
		t.Errorf("UpdateBead on %q, want gd-mr-existing", mc.UpdateBeadCalls[0].ID)
	}
	if got := len(mc.AddDepCalls); got != 1 {
		t.Fatalf("expected 1 AddDependency, got %d", got)
	}
}

func TestAdminBackfillMRBeads_ClosesOnMergedState(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{
		{taskWithMRFields("gd-task-merged", map[string]any{
			"mr_url":   "https://gitlab.com/x/y/-/merge_requests/9",
			"mr_state": "merged",
		})},
		{mrBead("gd-mr-9", "https://gitlab.com/x/y/-/merge_requests/9", "open")},
		{},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMRBackfillFlags(t)

	_ = adminBackfillMRBeadsCmd.Flags().Set("apply", "true")
	_ = adminBackfillMRBeadsCmd.Flags().Set("dry-run", "false")

	captureStdout(t, func() {
		if err := adminBackfillMRBeadsCmd.RunE(adminBackfillMRBeadsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.UpdateBeadCalls); got != 1 {
		t.Fatalf("expected 1 UpdateBead, got %d", got)
	}
	upd := mc.UpdateBeadCalls[0]
	if upd.Req.Status == nil || *upd.Req.Status != "closed" {
		t.Errorf("expected UpdateBead.Status=closed, got %v", upd.Req.Status)
	}
	var fields map[string]string
	if err := json.Unmarshal(upd.Req.Fields, &fields); err != nil {
		t.Fatalf("update fields: %v", err)
	}
	if fields["mr_outcome"] != "merged" {
		t.Errorf("mr_outcome = %q, want merged", fields["mr_outcome"])
	}
	if fields["mr_merged"] != "true" {
		t.Errorf("mr_merged = %q, want true", fields["mr_merged"])
	}
}

func TestAdminBackfillMRBeads_CreateWithClosedState(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{
		{taskWithMRFields("gd-task-closed", map[string]any{
			"mr_url":   "https://gitlab.com/x/y/-/merge_requests/3",
			"mr_state": "closed",
		})},
		{}, // no existing MR bead
		{}, // source page 2 empty
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMRBackfillFlags(t)

	_ = adminBackfillMRBeadsCmd.Flags().Set("apply", "true")
	_ = adminBackfillMRBeadsCmd.Flags().Set("dry-run", "false")

	captureStdout(t, func() {
		if err := adminBackfillMRBeadsCmd.RunE(adminBackfillMRBeadsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.CreateBeadCalls); got != 1 {
		t.Fatalf("expected 1 CreateBead, got %d", got)
	}
	if got := len(mc.CloseBeadCalls); got != 1 {
		t.Fatalf("expected new MR bead to be closed, got %d Close calls", got)
	}
	var fields map[string]string
	if err := json.Unmarshal(mc.CreateBeadCalls[0].Fields, &fields); err != nil {
		t.Fatalf("create fields: %v", err)
	}
	if fields["mr_outcome"] != "closed" {
		t.Errorf("mr_outcome = %q, want closed", fields["mr_outcome"])
	}
	if fields["mr_merged"] != "false" {
		t.Errorf("mr_merged = %q, want false", fields["mr_merged"])
	}
}

func TestAdminBackfillMRBeads_IdempotentDepSkip(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{
		{taskWithMRFields("gd-task-dup", map[string]any{
			"mr_url":   "https://gitlab.com/x/y/-/merge_requests/5",
			"mr_state": "opened",
		})},
		{mrBead("gd-mr-5", "https://gitlab.com/x/y/-/merge_requests/5", "open")},
		{},
	}
	mc.Deps["gd-task-dup"] = []*model.Dependency{
		{BeadID: "gd-task-dup", DependsOnID: "gd-mr-5", Type: model.DependencyType("produces")},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMRBackfillFlags(t)

	_ = adminBackfillMRBeadsCmd.Flags().Set("apply", "true")
	_ = adminBackfillMRBeadsCmd.Flags().Set("dry-run", "false")

	captureStdout(t, func() {
		if err := adminBackfillMRBeadsCmd.RunE(adminBackfillMRBeadsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.AddDepCalls); got != 0 {
		t.Errorf("expected dep to be skipped, got %d AddDep calls", got)
	}
}

func TestAdminBackfillMRBeads_DryRunDefault(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{
		{taskWithMRFields("gd-task-dry", map[string]any{
			"mr_url":   "https://gitlab.com/x/y/-/merge_requests/7",
			"mr_state": "opened",
		})},
		{}, // no existing MR bead
		{},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMRBackfillFlags(t)

	// Default is dry-run=true. Don't flip --apply.

	captureStdout(t, func() {
		if err := adminBackfillMRBeadsCmd.RunE(adminBackfillMRBeadsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.CreateBeadCalls); got != 0 {
		t.Errorf("dry-run made %d Create calls, want 0", got)
	}
	if got := len(mc.UpdateBeadCalls); got != 0 {
		t.Errorf("dry-run made %d Update calls, want 0", got)
	}
	if got := len(mc.AddDepCalls); got != 0 {
		t.Errorf("dry-run made %d AddDep calls, want 0", got)
	}
}

func TestAdminBackfillMRBeads_SkipsMalformedURL(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{
		{taskWithMRFields("gd-task-bad", map[string]any{
			"mr_url":   "not-a-real-url",
			"mr_state": "opened",
		})},
		{}, // source page 2 empty (no MR lookup expected)
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMRBackfillFlags(t)

	_ = adminBackfillMRBeadsCmd.Flags().Set("apply", "true")
	_ = adminBackfillMRBeadsCmd.Flags().Set("dry-run", "false")

	captureStdout(t, func() {
		if err := adminBackfillMRBeadsCmd.RunE(adminBackfillMRBeadsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.CreateBeadCalls); got != 0 {
		t.Errorf("malformed mr_url should be skipped, got %d Create calls", got)
	}
	if got := len(mc.AddDepCalls); got != 0 {
		t.Errorf("malformed mr_url should not link dep, got %d AddDep calls", got)
	}
	// The MR-lookup ListBeads call should NOT happen for the malformed URL,
	// so the only ListBeads calls are source-page lookups.
	for _, req := range mc.ListBeadsCalls {
		if len(req.Type) == 1 && req.Type[0] == "merge-request" {
			t.Errorf("malformed mr_url triggered MR lookup: %+v", req)
		}
	}
}

func TestAdminBackfillMRBeads_IgnoresBeadsWithoutMRURL(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{
		{
			taskWithMRFields("gd-task-no-mr", map[string]any{"some_other": "value"}),
		},
		{}, // source page 2 empty
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMRBackfillFlags(t)

	_ = adminBackfillMRBeadsCmd.Flags().Set("apply", "true")
	_ = adminBackfillMRBeadsCmd.Flags().Set("dry-run", "false")

	captureStdout(t, func() {
		if err := adminBackfillMRBeadsCmd.RunE(adminBackfillMRBeadsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.CreateBeadCalls) + len(mc.UpdateBeadCalls) + len(mc.AddDepCalls); got != 0 {
		t.Errorf("no-mr_url task should be a no-op, got %d total mutations", got)
	}
}

func TestProjectMRFields(t *testing.T) {
	in := map[string]any{
		"mr_url":            "https://gitlab.com/x/y/-/merge_requests/1",
		"mr_iid":            "1",
		"gitlab_mr_iid":     float64(1), // numeric MR IID coerces to "1"
		"gitlab_project_id": float64(12345678),
		"mr_state":          "opened",
		"unrelated":         "ignored",
		"mr_approved":       true,
		"mr_title":          "",
	}
	got := projectMRFields(in)
	if got["mr_url"] != "https://gitlab.com/x/y/-/merge_requests/1" {
		t.Errorf("mr_url projected as %q", got["mr_url"])
	}
	if got["gitlab_mr_iid"] != "1" {
		t.Errorf("numeric gitlab_mr_iid not coerced: %q", got["gitlab_mr_iid"])
	}
	if got["gitlab_project_id"] != "12345678" {
		t.Errorf("numeric gitlab_project_id not coerced: %q", got["gitlab_project_id"])
	}
	if got["mr_approved"] != "true" {
		t.Errorf("bool mr_approved not stringified: %q", got["mr_approved"])
	}
	if _, ok := got["unrelated"]; ok {
		t.Errorf("unrelated field leaked into MR projection")
	}
	if _, ok := got["mr_title"]; ok {
		t.Errorf("empty mr_title should be dropped, got %q", got["mr_title"])
	}
}

func TestInferMROutcome(t *testing.T) {
	if got := inferMROutcome("merged"); got != "merged" {
		t.Errorf("merged → %q", got)
	}
	if got := inferMROutcome("closed"); got != "closed" {
		t.Errorf("closed → %q", got)
	}
	if got := inferMROutcome("opened"); got != "closed" {
		t.Errorf("opened (terminal-fallback) → %q", got)
	}
}

// Sanity: client.AddDependencyRequest expects produces as a custom dep type.
// model.DependencyType admits any non-empty <=50 char string.
func TestProducesIsValidDepType(t *testing.T) {
	if !model.DependencyType("produces").IsValid() {
		t.Fatalf("produces should be a valid DependencyType")
	}
	// Round-trips through AddDependencyRequest without truncation.
	req := &client.AddDependencyRequest{Type: "produces"}
	if req.Type != "produces" {
		t.Fatalf("Type round-trip: %q", req.Type)
	}
}
