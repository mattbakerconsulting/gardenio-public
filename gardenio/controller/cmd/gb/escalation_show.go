package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// ── escalation show ─────────────────────────────────────────────────────

var escalationShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show escalation details",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		jsonOut, _ := cmd.Flags().GetBool("json")

		bead, err := daemon.GetBead(cmd.Context(), id)
		if err != nil {
			return fmt.Errorf("getting escalation %s: %w", id, err)
		}
		if bead.Type != "escalation" {
			return fmt.Errorf("%s is not an escalation (type=%s)", id, bead.Type)
		}

		if jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(bead)
		}

		// Formatted output.
		fmt.Printf("ID:          %s\n", bead.ID)
		fmt.Printf("Title:       %s\n", bead.Title)
		fmt.Printf("Status:      %s\n", bead.Status)
		fmt.Printf("Priority:    P%d\n", bead.Priority)
		if bead.Description != "" {
			fmt.Printf("Description: %s\n", bead.Description)
		}
		if len(bead.Labels) > 0 {
			fmt.Printf("Labels:      %s\n", strings.Join(bead.Labels, ", "))
		}
		if bead.CreatedBy != "" {
			fmt.Printf("Created By:  %s\n", bead.CreatedBy)
		}
		fmt.Printf("Created:     %s (%s ago)\n", bead.CreatedAt.Format(time.RFC3339), escalationAge(bead.CreatedAt))
		fmt.Printf("Updated:     %s\n", bead.UpdatedAt.Format(time.RFC3339))

		fmt.Println()

		// Escalation-specific fields in logical groups.
		f := bead.Fields

		// Operation info.
		fmt.Println("── Operation ──")
		printField("  Type", f["operation_type"])
		if spec := f["operation_spec"]; spec != "" && spec != "{}" {
			printField("  Spec", formatJSONCompact(spec))
		}
		printField("  Context", f["context"])
		printField("  Sensitivity", f["sensitivity"])
		printField("  Audience", escalationAudienceLabel(f))
		printField("  Exec Target", f["execution_target"])

		// Script.
		if script := f["execution_script"]; script != "" {
			fmt.Println()
			fmt.Println("── Execution Script ──")
			if script == "manual" {
				fmt.Println("  (manual — requires human judgment)")
			} else {
				// Show first 10 lines of script.
				lines := strings.Split(script, "\n")
				max := min(10, len(lines))
				for _, line := range lines[:max] {
					fmt.Printf("  %s\n", line)
				}
				if len(lines) > 10 {
					fmt.Printf("  ... (%d more lines)\n", len(lines)-10)
				}
			}
		}

		// Credentials, data requirements, expected outputs.
		if creds := f["credential_requirements"]; creds != "" {
			fmt.Println()
			fmt.Println("── Credentials Required ──")
			fmt.Printf("  %s\n", formatJSONCompact(creds))
		}
		if data := f["data_requirements"]; data != "" {
			fmt.Println()
			fmt.Println("── Data Requirements ──")
			fmt.Printf("  %s\n", formatJSONCompact(data))
		}
		if out := f["expected_outputs"]; out != "" {
			fmt.Println()
			fmt.Println("── Expected Outputs ──")
			fmt.Printf("  %s\n", formatJSONCompact(out))
		}

		// Requesting info.
		if f["requesting_agent"] != "" || f["requesting_task"] != "" {
			fmt.Println()
			fmt.Println("── Requester ──")
			printField("  Agent", f["requesting_agent"])
			printField("  Task", f["requesting_task"])
		}

		// Operator notification info — where/when the bridge posted the
		// escalation card to Slack, so a parked agent can see the operator was
		// notified without grep-fishing (ka-0000003667). "Notified At" is the
		// post time, NOT a read-receipt. Absent on escalations created before
		// the stamping shipped (and on sensitive/secret escalations, which
		// stamp only the timestamp).
		// "Operator DM At" is the personal ping to the human in operated_by
		// (ka-0000002669), which is a stronger signal than the card: the card
		// routes by REQUESTING AGENT, so "Notified At" only means it landed in
		// some channel. Also a post time, not a read receipt. Absent when the
		// escalation has no human operator, when the operator's Slack id could
		// not be resolved unambiguously, or when they filed it themselves.
		if f["notified_at"] != "" || f["slack_channel"] != "" || f["operator_dm_at"] != "" {
			fmt.Println()
			fmt.Println("── Operator Notification ──")
			printField("  Notified At", f["notified_at"])
			printField("  Channel", f["slack_channel"])
			printField("  Operator DM At", f["operator_dm_at"])
			printField("  Operator DM Channel", f["operator_dm_channel"])
		}

		// Pingback / re-notification state (ka-0000004385) — manual pingbacks,
		// the engine's automatic backoff renotifies, and the 48h+ queue alert.
		if f["pingback_requested_at"] != "" || f["queue_alert_requested_at"] != "" {
			fmt.Println()
			fmt.Println("── Pingbacks ──")
			printField("  Count", f["pingback_count"])
			printField("  Last Requested", f["pingback_requested_at"])
			printField("  Last Source", f["pingback_source"])
			printField("  Last By", f["pingback_by"])
			printField("  Last Reason", f["pingback_reason"])
			printField("  Last Posted", f["pingback_notified_at"])
			printField("  Auto Count", f["auto_pingback_count"])
			printField("  Queue Alerted", f["queue_alert_requested_at"])
		}

		// Approval info.
		if f["approved_by"] != "" {
			fmt.Println()
			fmt.Println("── Approval ──")
			printField("  Approved By", f["approved_by"])
			printField("  Approved At", f["approved_at"])
			printField("  Method", f["approval_method"])
		}

		// Execution result.
		if f["result_status"] != "" || f["executor"] != "" {
			fmt.Println()
			fmt.Println("── Result ──")
			printField("  Status", f["result_status"])
			printField("  Executor", f["executor"])
			if result := f["result"]; result != "" {
				printField("  Result", formatJSONCompact(result))
			}
			if redacted := f["redacted_summary"]; redacted != "" {
				printField("  Summary", redacted)
			}
		}

		return nil
	},
}

// printField prints a labeled field if the value is non-empty.
func printField(label, value string) {
	if value != "" {
		fmt.Printf("%-16s %s\n", label+":", value)
	}
}

// formatJSONCompact tries to pretty-print a JSON string on one line. If it's
// too long (>100 chars), it falls back to indented multiline.
func formatJSONCompact(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s // not JSON, return as-is
	}
	compact, err := json.Marshal(v)
	if err != nil {
		return s
	}
	if len(compact) <= 100 {
		return string(compact)
	}
	pretty, err := json.MarshalIndent(v, "  ", "  ")
	if err != nil {
		return string(compact)
	}
	return string(pretty)
}
