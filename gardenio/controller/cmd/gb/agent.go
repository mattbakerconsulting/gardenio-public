package main

import (
	"fmt"

	"gardenio/controller/internal/beadsapi"
	"github.com/spf13/cobra"
)

var agentCmd = &cobra.Command{
	Use:     "agent",
	Short:   "Agent lifecycle commands",
	GroupID: "agent",
}

func init() {
	agentRosterCmd.Flags().String("task", "", "Only show agents working on the given task bead ID (e.g. ka-abc123)")
	agentRosterCmd.Flags().Bool("no-gardener", false, "Leak detector: only show agents with NO real gardener (synthetic/unowned gardener_id)")
	agentCmd.AddCommand(agentRosterCmd)
}

var agentRosterCmd = &cobra.Command{
	Use:   "roster",
	Short: "List active agents (use --task to find live crew on a bead)",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		agents, err := daemon.ListAgentBeads(ctx)
		if err != nil {
			return err
		}

		task, _ := cmd.Flags().GetString("task")
		agents = filterAgentsByTask(agents, task)

		noGardener, _ := cmd.Flags().GetBool("no-gardener")
		if noGardener {
			agents = filterAgentsNoGardener(agents)
		}

		if jsonOutput {
			printJSON(agents)
			return nil
		}

		if len(agents) == 0 {
			if noGardener {
				cmd.Println("No unowned agents (every active agent has a real gardener).")
				return nil
			}
			cmd.Println("No active agents.")
			return nil
		}

		// Header.
		cmd.Printf("%-20s %-12s %-6s %-10s %-30s %-14s %-24s %s\n",
			"ID", "PROJECT", "MODE", "STATE", "ROLE/NAME", "TASK", "GARDENER", "SPAWNED BY")

		for _, a := range agents {
			state := a.AgentState
			if state == "" {
				state = "unknown"
			}
			project := a.Project
			if project == "" {
				project = "-"
			}
			taskID := a.Metadata["task_id"]
			if taskID == "" {
				taskID = "-"
			}
			gardener := a.GardenerID
			if gardener == "" {
				gardener = "-"
			}
			spawner := formatSpawner(a.Metadata["spawned_by"])

			cmd.Printf("%-20s %-12s %-6s %-10s %-30s %-14s %-24s %s\n",
				a.ID, project, a.Mode, state,
				fmt.Sprintf("%s/%s", a.Role, a.AgentName),
				taskID, gardener, spawner)
		}
		return nil
	},
}

// filterAgentsByTask returns only the agents whose task_id metadata matches
// task. An empty task is a no-op (returns the input unchanged) so callers can
// pass the flag value through unconditionally.
func filterAgentsByTask(agents []beadsapi.AgentBead, task string) []beadsapi.AgentBead {
	if task == "" {
		return agents
	}
	filtered := make([]beadsapi.AgentBead, 0, len(agents))
	for _, a := range agents {
		if a.Metadata["task_id"] == task {
			filtered = append(filtered, a)
		}
	}
	return filtered
}

// filterAgentsNoGardener returns only the agents with NO real gardener — i.e.
// whose derived GardenerID is synthetic or empty (see
// beadsapi.IsSyntheticGardenerID). This is the roster's leak detector: an agent
// bucketed under a synthetic-gardener owner was never adopted by a real gardener
// (trigger) bead, so it surfaces spawn paths that forgot to set a gardener.
func filterAgentsNoGardener(agents []beadsapi.AgentBead) []beadsapi.AgentBead {
	filtered := make([]beadsapi.AgentBead, 0, len(agents))
	for _, a := range agents {
		if beadsapi.IsSyntheticGardenerID(a.GardenerID) {
			filtered = append(filtered, a)
		}
	}
	return filtered
}

// formatSpawner formats the spawned_by metadata into a readable string.
// If empty, returns "(unknown)". If it starts with "sched-", returns "(scheduler)".
func formatSpawner(spawner string) string {
	if spawner == "" {
		return "(unknown)"
	}
	if len(spawner) > 6 && spawner[:6] == "sched-" {
		return "(scheduler)"
	}
	return spawner
}
