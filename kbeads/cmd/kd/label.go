package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var labelCmd = &cobra.Command{
	Use:     "label",
	Short:   "Manage bead labels",
	GroupID: "beads",
	Long: `Manage bead labels.

To MOVE a label (re-scope, re-target, rename), use ` + "`kd label replace`" + ` or
` + "`kd label set`" + ` rather than an add+remove pair: they apply the addition and the
removal in ONE call, so the bead is never left carrying both the old and the new
label if the second call fails.

  kd label replace <id> <old> <new>   # swap one label, keep the rest
  kd label set     <id> <label>...    # state the exact end state`,
	// labelCmd is a parent group, but it MUST be runnable for cobra to reach the
	// Args validator: a non-runnable parent short-circuits to flag.ErrHelp
	// BEFORE ValidateArgs runs (cobra command.go: `if !c.Runnable() { return
	// flag.ErrHelp }`), printing help to STDOUT and exiting 0. That silent
	// exit-0 is what made the missing MOVE verb doubly misleading — a caller
	// scripting `kd label move ... || <fallback>` never saw the fallback fire
	// (ka-0000003820; same shape as depCmd's fix for ka-0000005587). With a RunE
	// present, cobra.NoArgs rejects any UNKNOWN subcommand with a non-zero exit,
	// matching how the ROOT kd command already rejects unknown commands. KNOWN
	// subcommands match first in cobra's Find and are validated by their own
	// Args. A bare `kd label` passes NoArgs and falls through to RunE → help.
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var labelAddCmd = &cobra.Command{
	Use:   "add <bead-id> <label>...",
	Short: "Add labels to a bead",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		labels := args[1:]

		for _, label := range labels {
			_, err := beadsClient.AddLabel(context.Background(), beadID, label)
			if err != nil {
				return fmt.Errorf("adding label %q: %w", label, err)
			}
		}

		fmt.Printf("Added label(s) to %s\n", beadID)
		return nil
	},
}

var labelRemoveCmd = &cobra.Command{
	Use:   "remove <bead-id> <label>...",
	Short: "Remove labels from a bead",
	Long: `Remove one or more labels from a bead.

Removing a label the bead does not carry is reported as an error (exit 1)
rather than a bare "Removed label(s)", so a mistyped label name or an
already-gone label cannot read as success. An agent scripting
` + "`kd label remove ... || <fallback>`" + ` never saw its fallback fire before,
because the no-op delete returned 0.

With multiple labels, every one that actually matched is removed; the command
still exits non-zero if ANY requested label was absent, and names exactly which.

Pass --if-exists for scripted/idempotent removal, where "already gone" is the
desired end state and should exit 0.`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		labels := args[1:]
		ifExists, _ := cmd.Flags().GetBool("if-exists")

		var removed, absent []string
		for _, label := range labels {
			ok, err := beadsClient.RemoveLabel(context.Background(), beadID, label)
			if err != nil {
				return fmt.Errorf("removing label %q: %w", label, err)
			}
			if ok {
				removed = append(removed, label)
			} else {
				absent = append(absent, label)
			}
		}

		// Report what actually changed. The old bare "Removed label(s) from X"
		// was unfalsifiable — it printed the same text whether every label
		// matched a row or none did.
		if len(removed) > 0 {
			fmt.Printf("Removed label(s) from %s: %s\n", beadID, strings.Join(removed, ", "))
		}
		if len(absent) == 0 {
			return nil
		}
		if ifExists {
			fmt.Printf("Not present on %s (nothing to remove): %s\n", beadID, strings.Join(absent, ", "))
			return nil
		}
		// stderr + non-zero: the whole point is that `... || fallback` and
		// `set -e` see this as the failure it is.
		return fmt.Errorf("no such label(s) on %s: %s\n"+
			"       nothing was removed for those — check the label spelling,\n"+
			"       list the real labels with `kd show %s`, or pass --if-exists to\n"+
			"       treat an already-absent label as success",
			beadID, strings.Join(absent, ", "), beadID)
	},
}

func init() {
	labelCmd.AddCommand(labelAddCmd)
	labelCmd.AddCommand(labelRemoveCmd)
	labelCmd.AddCommand(labelSetCmd)
	labelCmd.AddCommand(labelReplaceCmd)

	labelRemoveCmd.Flags().Bool("if-exists", false,
		"exit 0 when a label is already absent (idempotent/scripted removal)")

	labelSetCmd.Flags().Bool("dry-run", false, "print the resulting label set without writing")
	labelSetCmd.Flags().Bool("allow-empty", false, "permit removing every label (refused by default)")

	labelReplaceCmd.Flags().Bool("dry-run", false, "print the resulting label set without writing")
	labelReplaceCmd.Flags().Bool("if-exists", false,
		"when <old-label> is absent, just ensure <new-label> is present instead of erroring")
}
