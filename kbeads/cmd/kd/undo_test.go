package main

import (
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

func TestUndoCmd_HappyPath(t *testing.T) {
	mc := newMockClient()
	mc.UndoMutationResponse = &client.UndoMutationResponse{
		Bead: &model.Bead{ID: "gd-x", Title: "Old"},
		Undone: &model.MutationRow{
			ID: 42, BeadID: "gd-x", Field: "title", OldValue: "Old", NewValue: "New", MutationType: "update",
		},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := undoCmd.RunE(undoCmd, []string{"42"}); err != nil {
			t.Fatalf("undoCmd: %v", err)
		}
	})
	if len(mc.UndoMutationCalls) != 1 || mc.UndoMutationCalls[0] != 42 {
		t.Errorf("expected one call with id=42, got %v", mc.UndoMutationCalls)
	}
	for _, want := range []string{"Undid mutation 42", "gd-x", "title", "Old", "New"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestUndoCmd_NonNumericArg(t *testing.T) {
	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := undoCmd.RunE(undoCmd, []string{"not-a-number"})
	if err == nil {
		t.Fatal("expected error for non-numeric id")
	}
	if len(mc.UndoMutationCalls) != 0 {
		t.Errorf("server must not be called for invalid id, got calls=%v", mc.UndoMutationCalls)
	}
}

func TestUndoCmd_ServerError_Surfaces(t *testing.T) {
	mc := newMockClient()
	mc.UndoMutationErr = errors.New("403 forbidden")

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := undoCmd.RunE(undoCmd, []string{"7"})
	if err == nil {
		t.Fatal("expected error from server to propagate")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("expected forbidden in error, got %v", err)
	}
}
