package main

import (
	"testing"

	"gardenio/kbeads/internal/model"
)

func TestParseBatchOp(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantCmd string
		wantErr bool
	}{
		{"show", "show gd-ABC", "show", false},
		{"dep list", "dep list gd-ABC", "dep list", false},
		{"empty", "", "", true},
		{"show no args", "show", "", true},
		{"show too many args", "show gd-ABC gd-DEF", "", true},
		{"unknown cmd", "delete gd-ABC", "", true},
		{"dep unknown sub", "dep add gd-ABC gd-DEF", "", true},
		{"dep list no args", "dep list", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			op, err := parseBatchOp(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if op.cmd != tc.wantCmd {
				t.Fatalf("expected cmd=%q, got %q", tc.wantCmd, op.cmd)
			}
		})
	}
}

func TestBatchCommand(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-AAA"] = &model.Bead{
		ID:     "gd-AAA",
		Title:  "First bead",
		Type:   model.TypeTask,
		Status: model.StatusOpen,
	}
	mc.Beads["gd-BBB"] = &model.Bead{
		ID:     "gd-BBB",
		Title:  "Second bead",
		Type:   model.TypeBug,
		Status: model.StatusClosed,
		Labels: []string{"urgent"},
	}
	withMockClient(t, mc)

	err := batchCmd.RunE(batchCmd, []string{"show gd-AAA", "show gd-BBB"})
	if err != nil {
		t.Fatalf("batch command failed: %v", err)
	}
}

func TestBatchCommand_NotFound(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Should not return error from RunE — errors are printed per-op.
	err := batchCmd.RunE(batchCmd, []string{"show gd-MISSING"})
	if err != nil {
		t.Fatalf("batch command should not fail for individual op errors: %v", err)
	}
}

func TestBatchCommand_InvalidSubcommand(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	err := batchCmd.RunE(batchCmd, []string{"invalid gd-AAA"})
	if err == nil {
		t.Fatal("expected error for invalid subcommand")
	}
}
