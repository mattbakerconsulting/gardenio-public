package main

// gb agent list — list agents (optionally filtered to current user's spawned agents)
// gb agent status <name-or-id> — show detailed status of a specific agent
// gb agent stop <name-or-id> — request graceful stop (advisory) for an agent
// gb agent kill <name-or-id> — immediately terminate a stuck agent (closes its bead)

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gardenio/clicreds/credstore"
	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/localreg"

	"github.com/spf13/cobra"
)

var agentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List agents (with optional --mine / --local filters)",
	Long: `List active agents. By default shows all agents from the daemon.

  gb agent list                    # all active agents (via daemon)
  gb agent list --mine             # only agents you spawned
  gb agent list --task ka-abc123   # only agents working on that task bead
  gb agent list --local            # local agents from on-disk registry (no daemon)
  gb agent list --local --prune    # also remove dead entries from the registry`,
	RunE: func(cmd *cobra.Command, args []string) error {
		mine, _ := cmd.Flags().GetBool("mine")
		local, _ := cmd.Flags().GetBool("local")
		prune, _ := cmd.Flags().GetBool("prune")

		if local {
			return runAgentListLocal(cmd, prune)
		}

		var spawner string
		if mine {
			spawner = currentUserEmail()
			if spawner == "" {
				return fmt.Errorf("cannot determine your identity — run 'gb login' first")
			}
		}

		agents, err := daemon.ListAgentBeadsBySpawner(cmd.Context(), spawner)
		if err != nil {
			return err
		}

		task, _ := cmd.Flags().GetString("task")
		agents = filterAgentsByTask(agents, task)

		if jsonOutput {
			printJSON(agents)
			return nil
		}

		if len(agents) == 0 {
			if mine {
				cmd.Println("No agents spawned by you.")
			} else {
				cmd.Println("No active agents.")
			}
			return nil
		}

		cmd.Printf("%-22s %-12s %-6s %-12s %-22s %s\n",
			"NAME", "PROJECT", "ROLE", "STATE", "TASK", "AGE")

		for _, a := range agents {
			state := a.AgentState
			if state == "" {
				state = "unknown"
			}
			project := a.Project
			if project == "" {
				project = "-"
			}
			task := a.Metadata["task_id"]
			if task == "" {
				task = "-"
			}
			age := agentAge(a.CreatedAt)

			cmd.Printf("%-22s %-12s %-6s %-12s %-22s %s\n",
				a.AgentName, project, a.Role, state, task, age)
		}
		return nil
	},
}

var agentStatusCmd = &cobra.Command{
	Use:   "status <name-or-id>",
	Short: "Show detailed agent status",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		nameOrID := args[0]

		agent, err := resolveAgent(cmd, nameOrID)
		if err != nil {
			return err
		}

		if jsonOutput {
			printJSON(agent)
			return nil
		}

		state := agent.AgentState
		if state == "" {
			state = "unknown"
		}

		cmd.Printf("Agent:      %s\n", agent.AgentName)
		cmd.Printf("Bead:       %s\n", agent.ID)
		cmd.Printf("Project:    %s\n", agent.Project)
		cmd.Printf("Role:       %s\n", agent.Role)
		cmd.Printf("Mode:       %s\n", agent.Mode)
		cmd.Printf("State:      %s\n", state)

		if task := agent.Metadata["task_id"]; task != "" {
			cmd.Printf("Task:       %s\n", task)
		}
		if pod := agent.Metadata["pod_name"]; pod != "" {
			cmd.Printf("Pod:        %s\n", pod)
		}
		if phase := agent.PodPhase; phase != "" {
			cmd.Printf("Pod Phase:  %s\n", phase)
		}
		if spawner := agent.Metadata["spawned_by"]; spawner != "" {
			cmd.Printf("Spawned By: %s\n", formatSpawner(spawner))
		}
		cmd.Printf("Created:    %s (%s ago)\n", agent.CreatedAt.Format(time.RFC3339), agentAge(agent.CreatedAt))

		return nil
	},
}

var agentStopCmd = &cobra.Command{
	Use:   "stop <name-or-id>",
	Short: "Request graceful agent stop (advisory)",
	Long: `Request a graceful stop for an agent. This is ADVISORY — it sets
agent_state=stop_requested on the bead, and the agent must reach a checkpoint
to honor it. A stuck agent (in an unresponsive tool call, hung process, etc.)
will NOT respond to this.

For immediate termination of a stuck agent, use:
  gb agent kill <name-or-id>     # closes the bead; reconciler deletes the pod
  kd close <agent-bead-id>       # equivalent low-level form

Ownership: only works for agents you spawned or operate (operated_by).
Use --override-owner to bypass the ownership check (still advisory —
does NOT escalate to a kill).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		nameOrID := args[0]
		overrideOwner, _ := cmd.Flags().GetBool("override-owner")
		// Back-compat: --force was previously aliased to ownership bypass. We
		// keep accepting it for that meaning so existing scripts/docs keep
		// working, but new callers should use --override-owner. For kill
		// semantics, use `gb agent kill`.
		if forceFlag, _ := cmd.Flags().GetBool("force"); forceFlag {
			overrideOwner = true
			cmd.PrintErrln("warning: --force on 'gb agent stop' is deprecated and only bypasses the ownership check (advisory stop). For immediate termination use 'gb agent kill'.")
		}

		agent, err := resolveAgent(cmd, nameOrID)
		if err != nil {
			return err
		}

		// A land's steward kills any agent of its own land, whatever the
		// grant and whoever spawned it, and says so in #machines (Lands step
		// 9.5, steward amendment 2026-10-23 §5).
		if land, ok := stewardKillLand(agent.Project, agent.Role); ok {
			reason, _ := cmd.Flags().GetString("reason")
			return runStewardKill(cmd, agent, land, reason)
		}

		if !overrideOwner {
			if err := checkAgentOwner(agent); err != nil {
				return err
			}
		}

		if err := daemon.RequestAgentStop(cmd.Context(), agent.ID); err != nil {
			return fmt.Errorf("requesting stop for agent %q: %w", agent.AgentName, err)
		}

		if jsonOutput {
			printJSON(map[string]string{
				"id":    agent.ID,
				"name":  agent.AgentName,
				"state": "stop_requested",
			})
			return nil
		}

		cmd.Printf("Stop requested for agent %s (%s) — advisory only; use 'gb agent kill' if it doesn't terminate.\n", agent.AgentName, agent.ID)
		return nil
	},
}

var agentKillCmd = &cobra.Command{
	Use:   "kill <name-or-id>",
	Short: "Immediately terminate a stuck agent (closes its bead)",
	Long: `Force-terminate an agent by closing its bead (with stop_requested set).
The reconciler observes the closed bead on its next tick and deletes the
underlying pod.

Unlike 'gb agent stop' (advisory, requires the agent to reach a checkpoint),
'gb agent kill' is unconditional — use it when an agent is stuck, hung, or
otherwise unresponsive to the graceful-stop signal.

Ownership: only works for agents you spawned or operate (operated_by).
Use --override-owner to bypass the ownership check. A Lands steward kills
any agent of its own land (not itself, not another steward) with --reason,
and the kill is said in the land's #machines.

The kill does not rely on the agent: it works on an agent whose own token
cannot read its bead, so its stop check fails.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		nameOrID := args[0]
		overrideOwner, _ := cmd.Flags().GetBool("override-owner")

		agent, err := resolveAgent(cmd, nameOrID)
		if err != nil {
			return err
		}

		if !overrideOwner {
			if err := checkAgentOwner(agent); err != nil {
				return err
			}
		}

		// The kill never depends on the pod: an agent whose own token cannot
		// read its bead (HTTP 403) never sees stop_requested, so closing the
		// bead is what ends it. The reconciler deletes the pod of a closed
		// agent bead on its next pass (gd-ZqcqDbjyt9). stop_requested is set
		// too, for an agent that can still read its bead and checks before the
		// pod goes.
		err = daemon.CloseBead(cmd.Context(), agent.ID, map[string]string{
			"agent_state":    "killed",
			"failure_reason": "killed-by-operator",
			"stop_requested": "true",
		})
		if err != nil {
			return fmt.Errorf("killing agent %q: %w", agent.AgentName, err)
		}

		if jsonOutput {
			printJSON(map[string]string{
				"id":     agent.ID,
				"name":   agent.AgentName,
				"state":  "killed",
				"action": "bead_closed",
			})
			return nil
		}

		cmd.Printf("Killed agent %s (%s) — bead closed; reconciler will delete the pod within one tick.\n", agent.AgentName, agent.ID)
		return nil
	},
}

var agentReapLocalCmd = &cobra.Command{
	Use:   "reap-local",
	Short: "Reap dead local-agent registry entries and close their orphaned beads",
	Long: `Scan the on-disk local-agent registry ($GARDENIO_HOME/local/) for entries
whose process is no longer alive (e.g. SIGKILL, laptop power loss) and:

  1. Close the associated agent bead on the daemon (best-effort —
     continues on daemon errors so offline hosts can still clean up).
  2. Remove the stale registry file.

Safe to run repeatedly. Entries whose PID is still alive are left alone.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		return runAgentReapLocal(cmd, dryRun)
	},
}

func runAgentReapLocal(cmd *cobra.Command, dryRun bool) error {
	entries, bad, err := localreg.List()
	if err != nil {
		return fmt.Errorf("list local registry: %w", err)
	}

	type reapResult struct {
		Name       string `json:"name"`
		BeadID     string `json:"bead_id,omitempty"`
		PID        int    `json:"pid"`
		BeadClosed bool   `json:"bead_closed"`
		BeadError  string `json:"bead_error,omitempty"`
		Reaped     bool   `json:"reaped"`
	}
	var results []reapResult

	for _, e := range entries {
		if localreg.Alive(e) {
			continue
		}
		res := reapResult{Name: e.Name, BeadID: e.BeadID, PID: e.PID}
		if !dryRun {
			if e.BeadID != "" && daemon != nil {
				if err := closeOrphanedLocalBead(cmd.Context(), e); err != nil {
					res.BeadError = err.Error()
				} else {
					res.BeadClosed = true
				}
			}
			if err := localreg.Deregister(e.Name); err != nil {
				return fmt.Errorf("deregister %s: %w", e.Name, err)
			}
			res.Reaped = true
		}
		results = append(results, res)
	}

	if jsonOutput {
		printJSON(map[string]any{
			"dry_run":      dryRun,
			"reaped":       results,
			"bad_registry": bad,
		})
		return nil
	}

	if len(results) == 0 {
		cmd.Println("No dead local-agent entries to reap.")
	} else {
		verb := "Reaped"
		if dryRun {
			verb = "Would reap"
		}
		cmd.Printf("%s %d local-agent entr%s:\n", verb, len(results), pluralEntry(len(results)))
		for _, r := range results {
			bead := r.BeadID
			if bead == "" {
				bead = "(no bead_id)"
			}
			status := "bead-close: skipped"
			if dryRun {
				status = "bead-close: dry-run"
			} else if r.BeadClosed {
				status = "bead-close: ok"
			} else if r.BeadError != "" {
				status = "bead-close: " + r.BeadError
			}
			cmd.Printf("  %-22s pid=%d bead=%s  %s\n", r.Name, r.PID, bead, status)
		}
	}
	if len(bad) > 0 {
		cmd.Printf("\nWarning: %d malformed registry file(s) skipped: %s\n",
			len(bad), strings.Join(bad, ", "))
	}
	return nil
}

// closeOrphanedLocalBead marks the agent bead failed and closes it on the
// daemon. A 404 is treated as success — the bead may already be gone, and a
// fake bead_id in a test fixture should not block registry cleanup.
func closeOrphanedLocalBead(ctx context.Context, e localreg.Entry) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := daemon.CloseBead(ctx, e.BeadID, map[string]string{
		"agent_state":    "failed",
		"failure_reason": "local-agent-crashed",
	})
	if err == nil {
		return nil
	}
	var apiErr *beadsapi.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
		return nil
	}
	return err
}

func init() {
	agentListCmd.Flags().Bool("mine", false, "Only show agents you spawned")
	agentListCmd.Flags().String("task", "", "Only show agents working on the given task bead ID (e.g. ka-abc123)")
	agentListCmd.Flags().Bool("local", false, "List local agents from on-disk registry instead of the daemon")
	agentListCmd.Flags().Bool("prune", false, "With --local, remove dead registry entries before listing")
	agentStopCmd.Flags().Bool("override-owner", false, "Send the advisory stop regardless of who spawned the agent")
	agentStopCmd.Flags().Bool("force", false, "(deprecated) Alias for --override-owner. Does NOT escalate to a kill — use 'gb agent kill' for immediate termination.")
	agentKillCmd.Flags().Bool("override-owner", false, "Kill any agent regardless of who spawned it")
	agentKillCmd.Flags().String("reason", "", "Why (required when a land's steward kills one of its agents; said in #machines)")
	agentReapLocalCmd.Flags().Bool("dry-run", false, "List what would be reaped without making changes")

	agentCmd.AddCommand(agentListCmd)
	agentCmd.AddCommand(agentStatusCmd)
	agentCmd.AddCommand(agentStopCmd)
	agentCmd.AddCommand(agentKillCmd)
	agentCmd.AddCommand(agentReapLocalCmd)
}

// currentUserEmail returns the authenticated user's email from the credential store,
// or empty string if not logged in. It is a var so tests can override it.
var currentUserEmail = func() string {
	store, err := credstore.Load("")
	if err != nil {
		return ""
	}
	cur := store.Current()
	if cur == nil || cur.User == nil {
		return ""
	}
	return cur.User.Email
}

// checkAgentOwner refuses a stop or kill of an agent the caller did not
// spawn. The caller owns it when they spawned it, or when they operate it
// (operated_by, the human a thread or gardener spawned it for): an agent a
// Slack thread spawned for you has spawned_by=thread-…, and you must not need
// --override-owner to kill it (gd-vCFepmDZoL).
func checkAgentOwner(agent *beadsapi.AgentBead) error {
	email := currentUserEmail()
	spawner := agent.Metadata["spawned_by"]
	if email == "" || spawner == "" || spawner == email {
		return nil
	}
	if op := agent.Metadata["operated_by"]; op == email || op == "oidc:"+email {
		return nil
	}
	return fmt.Errorf("agent %q was spawned by %s, not you — use --override-owner to override",
		agent.AgentName, spawner)
}

// resolveAgent finds an agent by name or bead ID.
func resolveAgent(cmd *cobra.Command, nameOrID string) (*beadsapi.AgentBead, error) {
	ctx := cmd.Context()

	// If it looks like a bead ID, try direct lookup.
	if strings.HasPrefix(nameOrID, "gd-") || strings.HasPrefix(nameOrID, "ka-") {
		bead, err := daemon.GetBead(ctx, nameOrID)
		if err != nil {
			return nil, fmt.Errorf("looking up agent %q: %w", nameOrID, err)
		}
		if bead.Type != "agent" {
			return nil, fmt.Errorf("%q is a %s bead, not an agent", nameOrID, bead.Type)
		}
		mode := bead.Fields["mode"]
		if mode == "" {
			mode = "crew"
		}
		return &beadsapi.AgentBead{
			ID:         bead.ID,
			Title:      bead.Title,
			Project:    bead.Fields["project"],
			Mode:       mode,
			Role:       bead.Fields["role"],
			AgentName:  bead.Fields["agent"],
			AgentState: bead.Fields["agent_state"],
			PodPhase:   bead.Fields["pod_phase"],
			CreatedAt:  bead.CreatedAt,
			UpdatedAt:  bead.UpdatedAt,
			Metadata:   bead.Fields,
		}, nil
	}

	// Search by agent name field.
	agents, err := daemon.ListAgentBeadsBySpawner(ctx, "")
	if err != nil {
		return nil, err
	}
	for i, a := range agents {
		if a.AgentName == nameOrID {
			return &agents[i], nil
		}
	}
	return nil, fmt.Errorf("no active agent found with name %q", nameOrID)
}

// runAgentListLocal enumerates local agents from the on-disk registry at
// $GARDENIO_HOME/local. It does not require the daemon to be reachable, so it
// works on a laptop that's currently offline. Pass prune=true to clean up
// entries whose PID is no longer alive before listing.
func runAgentListLocal(cmd *cobra.Command, prune bool) error {
	if prune {
		removed, err := localreg.Prune()
		if err != nil {
			return fmt.Errorf("prune local registry: %w", err)
		}
		if len(removed) > 0 && !jsonOutput {
			cmd.Printf("Pruned %d dead entr%s: %s\n",
				len(removed), pluralEntry(len(removed)), strings.Join(removed, ", "))
		}
	}

	entries, bad, err := localreg.List()
	if err != nil {
		return fmt.Errorf("list local registry: %w", err)
	}

	if jsonOutput {
		type row struct {
			localreg.Entry
			Alive bool `json:"alive"`
		}
		out := make([]row, 0, len(entries))
		for _, e := range entries {
			out = append(out, row{Entry: e, Alive: localreg.Alive(e)})
		}
		printJSON(out)
		return nil
	}

	if len(entries) == 0 {
		cmd.Println("No local agents registered.")
		if len(bad) > 0 {
			cmd.Printf("Warning: %d malformed registry file(s) skipped: %s\n",
				len(bad), strings.Join(bad, ", "))
		}
		return nil
	}

	cmd.Printf("%-22s %-14s %-6s %-7s %-6s %-26s %s\n",
		"NAME", "BEAD", "ROLE", "ALIVE", "PID", "COOP-URL", "AGE")
	for _, e := range entries {
		alive := "no"
		if localreg.Alive(e) {
			alive = "yes"
		}
		role := e.Role
		if role == "" {
			role = "-"
		}
		bead := e.BeadID
		if bead == "" {
			bead = "-"
		}
		coop := e.CoopURL
		if coop == "" {
			coop = "-"
		}
		cmd.Printf("%-22s %-14s %-6s %-7s %-6d %-26s %s\n",
			e.Name, bead, role, alive, e.PID, coop, agentAge(e.StartedAt))
	}
	if len(bad) > 0 {
		cmd.Printf("\nWarning: %d malformed registry file(s) skipped: %s\n",
			len(bad), strings.Join(bad, ", "))
	}
	return nil
}

func pluralEntry(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// agentAge formats a human-readable age from a creation timestamp.
func agentAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
