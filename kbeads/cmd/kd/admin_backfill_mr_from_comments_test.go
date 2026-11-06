package main

import (
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
)

func beadNoFields(id string) *model.Bead {
	return &model.Bead{
		ID:     id,
		Type:   "task",
		Kind:   model.KindIssue,
		Status: model.StatusOpen,
	}
}

func resetMRFromCommentsFlags(t *testing.T) {
	t.Helper()
	_ = adminBackfillMRFromCommentsCmd.Flags().Set("dry-run", "true")
	_ = adminBackfillMRFromCommentsCmd.Flags().Set("apply", "false")
	_ = adminBackfillMRFromCommentsCmd.Flags().Set("limit", "0")
}

func TestAdminBackfillMRFromComments_SetsMRURLFromComment(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{
		{beadNoFields("gd-task-a")},
		{}, // end of pages
	}
	const mrURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/916"
	mc.CommentsByBead = map[string][]*model.Comment{
		"gd-task-a": {
			{ID: 1, BeadID: "gd-task-a", Text: "starting work"},
			{ID: 2, BeadID: "gd-task-a", Text: "MR opened: " + mrURL},
		},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMRFromCommentsFlags(t)

	_ = adminBackfillMRFromCommentsCmd.Flags().Set("apply", "true")
	_ = adminBackfillMRFromCommentsCmd.Flags().Set("dry-run", "false")

	captureStdout(t, func() {
		if err := adminBackfillMRFromCommentsCmd.RunE(adminBackfillMRFromCommentsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.UpdateBeadCalls); got != 1 {
		t.Fatalf("expected 1 UpdateBead, got %d", got)
	}
	upd := mc.UpdateBeadCalls[0]
	if upd.ID != "gd-task-a" {
		t.Errorf("UpdateBead on %q, want gd-task-a", upd.ID)
	}
	var f map[string]string
	if err := json.Unmarshal(upd.Req.Fields, &f); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if f["mr_url"] != mrURL {
		t.Errorf("patch mr_url = %q, want %q", f["mr_url"], mrURL)
	}
}

func TestAdminBackfillMRFromComments_DryRunNoWrite(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{
		{beadNoFields("gd-task-a")},
		{},
	}
	mc.CommentsByBead = map[string][]*model.Comment{
		"gd-task-a": {{ID: 1, Text: "MR opened: https://gitlab.com/x/y/-/merge_requests/5"}},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMRFromCommentsFlags(t)

	// dry-run is the default; assert no writes happen.
	captureStdout(t, func() {
		if err := adminBackfillMRFromCommentsCmd.RunE(adminBackfillMRFromCommentsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.UpdateBeadCalls); got != 0 {
		t.Errorf("dry-run issued %d UpdateBead calls, want 0", got)
	}
}

func TestAdminBackfillMRFromComments_SkipsBeadsAlreadyLinked(t *testing.T) {
	mc := newMockClient()
	withMR := beadNoFields("gd-task-linked")
	withMR.Fields, _ = json.Marshal(map[string]any{
		"mr_url": "https://gitlab.com/x/y/-/merge_requests/1",
	})
	mc.ListBeadsPages = [][]*model.Bead{
		{withMR},
		{},
	}
	// Comment mentions a *different* MR; must be ignored because mr_url is set.
	mc.CommentsByBead = map[string][]*model.Comment{
		"gd-task-linked": {{ID: 1, Text: "MR opened: https://gitlab.com/x/y/-/merge_requests/2"}},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMRFromCommentsFlags(t)

	_ = adminBackfillMRFromCommentsCmd.Flags().Set("apply", "true")
	_ = adminBackfillMRFromCommentsCmd.Flags().Set("dry-run", "false")

	captureStdout(t, func() {
		if err := adminBackfillMRFromCommentsCmd.RunE(adminBackfillMRFromCommentsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.UpdateBeadCalls); got != 0 {
		t.Errorf("bead already carrying mr_url should be skipped, got %d UpdateBead calls", got)
	}
}

func TestAdminBackfillMRFromComments_NoMRURLInComments(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{
		{beadNoFields("gd-task-a")},
		{},
	}
	mc.CommentsByBead = map[string][]*model.Comment{
		"gd-task-a": {
			{ID: 1, Text: "no link here"},
			{ID: 2, Text: "see https://gitlab.com/x/y/-/issues/3"}, // issue, not MR
		},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMRFromCommentsFlags(t)

	_ = adminBackfillMRFromCommentsCmd.Flags().Set("apply", "true")
	_ = adminBackfillMRFromCommentsCmd.Flags().Set("dry-run", "false")

	captureStdout(t, func() {
		if err := adminBackfillMRFromCommentsCmd.RunE(adminBackfillMRFromCommentsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.UpdateBeadCalls); got != 0 {
		t.Errorf("no MR URL in comments → no write, got %d UpdateBead calls", got)
	}
}

func TestAdminBackfillMRFromComments_TakesFirstMRURL(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{
		{beadNoFields("gd-task-a")},
		{},
	}
	const first = "https://gitlab.com/x/y/-/merge_requests/10"
	mc.CommentsByBead = map[string][]*model.Comment{
		"gd-task-a": {
			{ID: 1, Text: "MR opened: " + first},
			{ID: 2, Text: "superseded by https://gitlab.com/x/y/-/merge_requests/20"},
		},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMRFromCommentsFlags(t)

	_ = adminBackfillMRFromCommentsCmd.Flags().Set("apply", "true")
	_ = adminBackfillMRFromCommentsCmd.Flags().Set("dry-run", "false")

	captureStdout(t, func() {
		if err := adminBackfillMRFromCommentsCmd.RunE(adminBackfillMRFromCommentsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.UpdateBeadCalls); got != 1 {
		t.Fatalf("expected 1 UpdateBead, got %d", got)
	}
	var f map[string]string
	if err := json.Unmarshal(mc.UpdateBeadCalls[0].Req.Fields, &f); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if f["mr_url"] != first {
		t.Errorf("patch mr_url = %q, want first match %q", f["mr_url"], first)
	}
}
