package bridge

import (
	"context"
	"testing"
)

// TestHandleMention_CreateBead_CarriesProjectLabelAndCreatedBy pins
// gd-VnWbYkGT59 (ADR docs/adr/taints.md §10.3 / §14 phase 4): a ticket
// ingested by the gitlab bridge arrives tainted with no bridge-side taint
// code at all — the daemon's generic resolveCreateTaints hook
// (kbeads/internal/server/taint_propagate_create.go) reads the new bead's
// own "project:<name>" label to resolve the project's intake_taints
// (kbeads/internal/server/taint_propagate_project_intake.go, proved against
// this exact request shape by
// TestCreateBead_GitlabBridgeShapedRequest_InheritsProjectIntakeTaints).
//
// That server-side mechanism has exactly one input contract on the bridge
// side: the CreateBeadRequest must carry a "project:<name>" label naming the
// project bead that declares intake_taints. This test pins that contract at
// the source — the mention handler's create-task-bead call
// (gitlab_mentions.go) — so a future refactor that dropped or renamed the
// label, or the CreatedBy stamp downstream tooling keys on, would fail here
// even though nothing downstream of the daemon boundary would notice (the
// mock daemon used by every other gitlab_mentions test does not run taint
// logic).
func TestHandleMention_CreateBead_CarriesProjectLabelAndCreatedBy(t *testing.T) {
	daemon := &mockMentionDaemon{
		createID: "gd-ticket",
		spawnID:  "gd-agent",
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "acme",
		Users:   testUserLookup(),
		Logger:  discardLogger(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio ticket ingested from GitLab",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 1, URL: "https://gitlab.com/group/project/-/merge_requests/1", Title: "MR"},
		DiscussionID: "disc-1",
	}

	handled := h.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected mention to be handled")
	}
	if len(daemon.created) != 1 {
		t.Fatalf("expected 1 bead created, got %d", len(daemon.created))
	}

	req := daemon.created[0]
	if req.CreatedBy != "gitlab-bridge" {
		t.Errorf("CreatedBy = %q; want %q", req.CreatedBy, "gitlab-bridge")
	}
	wantLabel := "project:acme"
	found := false
	for _, l := range req.Labels {
		if l == wantLabel {
			found = true
		}
	}
	if !found {
		t.Errorf("expected label %q (the daemon's project-intake-taint lookup key), got labels: %v", wantLabel, req.Labels)
	}
}
