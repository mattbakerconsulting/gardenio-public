package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Operator directive 2026-08-02 (Matt Baker, Slack thread ka-0000004862):
// widgets and reports are deprecated in favor of bundles. The two widget
// commands that CREATE beads must refuse to run unless the caller
// acknowledges the deprecation; every read-only command must be unaffected.
// Tracking: ka-0000007561.

// newDeprecationTestCmd builds a bare command carrying the opt-out flag so the
// gate can be exercised without running a real widget command's body.
func newDeprecationTestCmd(ack bool) *cobra.Command {
	cmd := &cobra.Command{Use: "emit"}
	cmd.Flags().Bool(widgetDeprecationFlag, false, "")
	if ack {
		if err := cmd.Flags().Set(widgetDeprecationFlag, "true"); err != nil {
			panic(err)
		}
	}
	return cmd
}

func TestCheckWidgetDeprecatedBlocksWithoutAck(t *testing.T) {
	err := checkWidgetDeprecated(newDeprecationTestCmd(false), "widget-instance")
	if err == nil {
		t.Fatal("expected an error without the ack flag — a silent pass would keep " +
			"letting agents create widget beads, which is what the deprecation stops")
	}
	// The message has to route the caller to the replacement, not just say "no".
	for _, want := range []string{"DEPRECATED", "widget-instance", "gb bundle create", widgetDeprecationFlag} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q; got:\n%s", want, err.Error())
		}
	}
}

func TestCheckWidgetDeprecatedAllowsWithAck(t *testing.T) {
	if err := checkWidgetDeprecated(newDeprecationTestCmd(true), "widget-instance"); err != nil {
		t.Fatalf("ack flag should permit the command (escape hatch for maintaining "+
			"already-published widgets), got: %v", err)
	}
}

// The gate is only useful if it is actually wired to the bead-creating
// commands and the flag is registered on them.
func TestWidgetAuthoringCommandsExposeDeprecationFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
	}{
		{"emit", widgetEmitCmd},
		{"scaffold-htmx", widgetScaffoldHTMXCmd},
	} {
		if tc.cmd.Flags().Lookup(widgetDeprecationFlag) == nil {
			t.Errorf("%s: --%s not registered — the gate would be unbypassable",
				tc.name, widgetDeprecationFlag)
		}
	}
}

// Read-only widget commands must NOT carry the gate: inspecting already
// published widgets stays free.
func TestReadOnlyWidgetCommandsAreNotGated(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
	}{
		{"list", widgetListCmd},
		{"show", widgetShowCmd},
		{"similar", widgetSimilarCmd},
	} {
		if tc.cmd.Flags().Lookup(widgetDeprecationFlag) != nil {
			t.Errorf("%s is read-only and must not require the deprecation ack", tc.name)
		}
	}
}

// The deprecation has to be discoverable from --help, since that is where an
// agent looks before authoring.
func TestWidgetHelpAnnouncesDeprecation(t *testing.T) {
	if !strings.Contains(widgetCmd.Short, "DEPRECATED") {
		t.Error("`gb widget --help` Short should announce the deprecation")
	}
	if !strings.Contains(widgetCmd.Long, "bundle") {
		t.Error("`gb widget --help` Long should point at bundles as the replacement")
	}
}
