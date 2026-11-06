package main

// Deprecated stub commands for operations that moved to gb (gardenio CLI).
// These exist to guide users who still try the old kd invocations.
// See bd-fzkui.14.

import "github.com/spf13/cobra"

func init() {
	rootCmd.AddCommand(deprecatedAgentCmd)
	rootCmd.AddCommand(deprecatedDecisionCmd)
	rootCmd.AddCommand(deprecatedGateCmd)
	rootCmd.AddCommand(deprecatedBusCmd)
	rootCmd.AddCommand(deprecatedHookCmd)
	rootCmd.AddCommand(deprecatedMailCmd)
	rootCmd.AddCommand(deprecatedInboxCmd)
	rootCmd.AddCommand(deprecatedNewsCmd)
	rootCmd.AddCommand(deprecatedSetupCmd)
	rootCmd.AddCommand(deprecatedYieldCmd)
	rootCmd.AddCommand(deprecatedReadyCmd)
	rootCmd.AddCommand(deprecatedPrimeCmd)
}

var deprecatedAgentCmd = &cobra.Command{
	Use:        "agent",
	Short:      "Agent lifecycle commands (moved to gb)",
	Deprecated: "use 'gb agent' instead",
}

var deprecatedDecisionCmd = &cobra.Command{
	Use:        "decision",
	Short:      "Decision checkpoint commands (moved to gb)",
	Deprecated: "use 'gb decision' instead",
}

var deprecatedGateCmd = &cobra.Command{
	Use:        "gate",
	Short:      "Gate enforcement commands (moved to gb)",
	Deprecated: "use 'gb gate' instead",
}

var deprecatedBusCmd = &cobra.Command{
	Use:        "bus",
	Short:      "Event bus commands (moved to gb)",
	Deprecated: "use 'gb bus' instead",
}

var deprecatedHookCmd = &cobra.Command{
	Use:        "hook",
	Short:      "Hook commands (moved to gb)",
	Deprecated: "use 'gb hook' instead",
}

var deprecatedMailCmd = &cobra.Command{
	Use:        "mail",
	Short:      "Agent mail commands (moved to gb)",
	Deprecated: "use 'gb mail' instead",
}

var deprecatedInboxCmd = &cobra.Command{
	Use:        "inbox",
	Short:      "Agent inbox (moved to gb)",
	Deprecated: "use 'gb inbox' instead",
}

var deprecatedNewsCmd = &cobra.Command{
	Use:        "news",
	Short:      "Agent news feed (moved to gb)",
	Deprecated: "use 'gb news' instead",
}

var deprecatedSetupCmd = &cobra.Command{
	Use:        "setup",
	Short:      "Agent setup commands (moved to gb)",
	Deprecated: "use 'gb setup' instead",
}

var deprecatedYieldCmd = &cobra.Command{
	Use:        "yield",
	Short:      "Yield and wait for human response (moved to gb)",
	Deprecated: "use 'gb yield' instead",
}

var deprecatedReadyCmd = &cobra.Command{
	Use:        "ready",
	Short:      "Show agent workflow steps (moved to gb)",
	Deprecated: "use 'gb ready' instead",
}

var deprecatedPrimeCmd = &cobra.Command{
	Use:        "prime",
	Short:      "Render agent priming context (moved to gb)",
	Deprecated: "use 'gb prime' instead",
}
