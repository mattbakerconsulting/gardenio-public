package main

// gb bus emit --hook=Stop
// Reads Claude Code hook event JSON from stdin, resolves agent identity,
// calls POST /v1/hooks/emit, and exits with appropriate code.
//
// Exit codes:
//
//	0 — allow
//	1 — server error (retries exhausted)
//	2 — block (stderr: {"decision":"block","reason":"..."})

import (
	"encoding/json"
	"fmt"
	"os"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/harness"

	"github.com/spf13/cobra"
)

var busCmd = &cobra.Command{
	Use:     "bus",
	Short:   "Event bus operations",
	GroupID: "orchestration",
}

var busEmitCmd = &cobra.Command{
	Use:   "emit",
	Short: "Emit a hook event",
	Long: `Reads a Claude Code hook event JSON from stdin, resolves agent identity,
and calls POST /v1/hooks/emit on the kbeads server.

Exit codes:
  0 — allow (or no gates to check)
  1 — server error (retries exhausted)
  2 — block

Warnings are written to stdout as <system-reminder> tags for Claude Code.
Block reason is written to stderr as {"decision":"block","reason":"..."}.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		hookType, _ := cmd.Flags().GetString("hook")
		if hookType == "" {
			return fmt.Errorf("--hook is required (e.g. Stop, PreToolUse, UserPromptSubmit, PreCompact, PostCompact)")
		}

		cwdFlag, _ := cmd.Flags().GetString("cwd")

		// Read JSON from stdin (Claude Code hook event format).
		var stdinEvent map[string]any
		decoder := json.NewDecoder(os.Stdin)
		if err := decoder.Decode(&stdinEvent); err != nil {
			stdinEvent = map[string]any{}
		}

		// Resolve CWD: flag > stdin cwd field > os.Getwd().
		cwd := cwdFlag
		if cwd == "" {
			if v, ok := stdinEvent["cwd"].(string); ok && v != "" {
				cwd = v
			}
		}
		if cwd == "" {
			if wd, err := os.Getwd(); err == nil {
				cwd = wd
			}
		}

		claudeSessionID, _ := stdinEvent["session_id"].(string)
		toolName, _ := stdinEvent["tool_name"].(string)

		// Resolve agent_bead_id.
		agentBeadID, _ := resolveAgentID("")
		if agentBeadID == "" {
			agentBeadID = resolveAgentByActor(cmd.Context(), actor)
		}

		req := beadsapi.EmitHookRequest{
			AgentBeadID:     agentBeadID,
			HookType:        hookType,
			ClaudeSessionID: claudeSessionID,
			SessionID:       claudeSessionID,
			Harness:         harness.MustFromEnv(),
			CWD:             cwd,
			Actor:           actor,
			ToolName:        toolName,
		}
		resp, err := emitHookWithRetry(cmd.Context(), req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gb bus emit: server error after retries: %v\n", err)
			os.Exit(1)
		}

		// On SessionStart, inject the full prime context.
		if hookType == "SessionStart" {
			agentID := resolvePrimeAgentFromEnv(actor)
			outputPrimeForHook(os.Stdout, agentID)
		}

		// Write warnings as system-reminder tags to stdout.
		for _, w := range resp.Warnings {
			fmt.Printf("<system-reminder>%s</system-reminder>\n", w)
		}

		// Write inject content to stdout if present.
		if resp.Inject != "" {
			fmt.Print(resp.Inject)
		}

		// Block: write to stderr and exit 2.
		if resp.Block {
			blockJSON, _ := json.Marshal(map[string]string{
				"decision": "block",
				"reason":   resp.Reason,
			})
			fmt.Fprintf(os.Stderr, "%s\n", blockJSON)
			os.Exit(2)
		}

		return nil
	},
}

// resolvePrimeAgentFromEnv resolves agent identity from env vars and the global actor.
func resolvePrimeAgentFromEnv(globalActor string) string {
	if v := os.Getenv("KD_ACTOR"); v != "" {
		return v
	}
	if v := os.Getenv("KD_AGENT_ID"); v != "" {
		return v
	}
	if globalActor != "" && globalActor != "unknown" {
		return globalActor
	}
	return ""
}

func init() {
	busCmd.AddCommand(busEmitCmd)

	busEmitCmd.Flags().String("hook", "", "hook type: Stop|PreToolUse|UserPromptSubmit|PreCompact|PostCompact (required)")
	busEmitCmd.Flags().String("cwd", "", "working directory (default: current dir)")

	_ = busEmitCmd.MarkFlagRequired("hook")
}
