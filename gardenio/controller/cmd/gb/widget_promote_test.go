package main

import (
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestWidgetPromoteCmdRegistered verifies the command is wired into the
// `gb widget` tree with a --help body, --note, and --force flags.
func TestWidgetPromoteCmdRegistered(t *testing.T) {
	if widgetPromoteCmd == nil {
		t.Fatal("widgetPromoteCmd is nil")
	}
	if widgetPromoteCmd.Use != "promote <slug-or-id>" {
		t.Errorf("Use=%q, want %q", widgetPromoteCmd.Use, "promote <slug-or-id>")
	}
	if strings.TrimSpace(widgetPromoteCmd.Long) == "" {
		t.Error("Long help body is empty — --help would render nothing useful")
	}
	if !strings.Contains(widgetPromoteCmd.Long, "widget-stage:stable") {
		t.Error("Long help should mention the target widget-stage:stable label")
	}

	// The command must be reachable from the widget root.
	if !slices.Contains(widgetCmd.Commands(), widgetPromoteCmd) {
		t.Error("widgetPromoteCmd is not attached to widgetCmd — `gb widget promote` would 404")
	}

	if f := widgetPromoteCmd.Flag("note"); f == nil {
		t.Error("missing --note flag")
	}
	if f := widgetPromoteCmd.Flag("force"); f == nil {
		t.Error("missing --force flag")
	}
}

// TestRunPromoteValidate_ForceSkipsGate confirms --force short-circuits
// the strict validate and returns the audit-comment summary.
func TestRunPromoteValidate_ForceSkipsGate(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-jcznwa4kfn",
		Fields: map[string]string{"slug": "incomplete", "widget_version": "1"},
	}
	summary, err := runPromoteValidate(bead, true)
	if err != nil {
		t.Fatalf("expected nil error with --force, got %v", err)
	}
	if !strings.Contains(summary, "SKIPPED") {
		t.Errorf("summary=%q, expected to mention SKIPPED", summary)
	}
}

// TestRunPromoteValidate_FailsOnStrictErrors verifies a bead missing the
// strict-required fields cannot be promoted without --force.
func TestRunPromoteValidate_FailsOnStrictErrors(t *testing.T) {
	// A widget-type with no schema / template will fail strict validation.
	bead := &beadsapi.BeadDetail{
		ID:     "gd-heqqlpeqcq",
		Fields: map[string]string{"slug": "blank"},
	}
	_, err := runPromoteValidate(bead, false)
	if err == nil {
		t.Fatal("expected strict-validate error for a blank widget-type, got nil")
	}
	if !strings.Contains(err.Error(), "validate --strict failed") {
		t.Errorf("error=%q, expected mention of validate --strict", err.Error())
	}
}
