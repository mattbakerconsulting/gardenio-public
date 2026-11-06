package main

import (
	"errors"
	"testing"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

func TestSimilarCommand_PassesFlagsToClient(t *testing.T) {
	mc := newMockClient()
	mc.FindSimilarResp = &client.FindSimilarResponse{
		SourceID: "gd-AAA",
		Model:    "test-model",
		Results: []*client.FindSimilarResult{
			{
				Bead:  &model.Bead{ID: "gd-BBB", Title: "Neighbor", Type: model.TypeTask, Status: model.StatusOpen},
				Score: 0.91,
			},
		},
	}
	withMockClient(t, mc)

	if err := similarCmd.Flags().Set("limit", "25"); err != nil {
		t.Fatalf("set limit: %v", err)
	}
	if err := similarCmd.Flags().Set("min-score", "0.5"); err != nil {
		t.Fatalf("set min-score: %v", err)
	}
	t.Cleanup(func() {
		_ = similarCmd.Flags().Set("limit", "10")
		_ = similarCmd.Flags().Set("min-score", "0")
	})

	if err := similarCmd.RunE(similarCmd, []string{"gd-AAA"}); err != nil {
		t.Fatalf("similar RunE: %v", err)
	}

	if len(mc.FindSimilarCalls) != 1 {
		t.Fatalf("expected 1 FindSimilar call, got %d", len(mc.FindSimilarCalls))
	}
	call := mc.FindSimilarCalls[0]
	if call.BeadID != "gd-AAA" {
		t.Errorf("BeadID: got %q, want gd-AAA", call.BeadID)
	}
	if call.Limit != 25 {
		t.Errorf("Limit: got %d, want 25", call.Limit)
	}
	if call.MinScore != 0.5 {
		t.Errorf("MinScore: got %v, want 0.5", call.MinScore)
	}
}

func TestSimilarCommand_EmptyResults(t *testing.T) {
	mc := newMockClient()
	mc.FindSimilarResp = &client.FindSimilarResponse{SourceID: "gd-AAA", Results: nil}
	withMockClient(t, mc)

	if err := similarCmd.RunE(similarCmd, []string{"gd-AAA"}); err != nil {
		t.Fatalf("similar RunE: %v", err)
	}
}

func TestSimilarCommand_ClientError(t *testing.T) {
	mc := newMockClient()
	mc.FindSimilarErr = errors.New("injected failure")
	withMockClient(t, mc)

	err := similarCmd.RunE(similarCmd, []string{"gd-AAA"})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}
