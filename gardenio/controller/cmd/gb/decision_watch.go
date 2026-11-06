package main

import (
	"fmt"
	"os"

	"gardenio/controller/internal/tui/decision"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

var decisionWatchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Interactive TUI for monitoring and responding to decisions",
	Long: `Launch an interactive terminal UI that monitors pending decisions,
lets you navigate between them, select options, add rationale, and
resolve or dismiss decisions — all with keyboard shortcuts.

Key bindings:
  j/k        Navigate between decisions
  1-4        Select option
  r          Add rationale before confirming
  Enter      Confirm selection
  d          Dismiss/cancel decision
  !          Filter to high urgency only
  a          Show all urgencies
  R          Refresh
  ?          Toggle help
  q/Ctrl+C   Quit`,
	RunE: func(cmd *cobra.Command, args []string) error {
		model := decision.New(daemon, actor)

		urgentOnly, _ := cmd.Flags().GetBool("urgent-only")
		if urgentOnly {
			model.SetFilter("high")
		}

		p := tea.NewProgram(model, tea.WithAltScreen())
		if _, err := p.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error running decision watch: %v\n", err)
			return err
		}

		return nil
	},
}

func init() {
	decisionWatchCmd.Flags().Bool("urgent-only", false, "Show only high urgency decisions")
	decisionCmd.AddCommand(decisionWatchCmd)
}
