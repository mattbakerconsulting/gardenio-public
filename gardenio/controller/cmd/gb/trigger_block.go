// Package main — `gb trigger block`/`unblock`/`show` subcommands.
//
// These commands manage the `blocked_by` field on trigger beads, used by the
// engine's blocked-by gate (see internal/bridge/triggers.go::triggerBlockedBy).
// While ANY referenced bead (typically a decision or escalation) is still open,
// the engine skips evaluation of the trigger and does NOT update last_checked —
// so the trigger resumes on the very next cycle after the bead closes.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

var (
	triggerBlockDecision   string
	triggerUnblockDecision string
)

var triggerBlockCmd = &cobra.Command{
	Use:   "block <trigger-id> --decision <bead-id>",
	Short: "Append a bead ID to the trigger's blocked_by list",
	Long: `Append a bead ID (typically a decision or escalation) to the trigger's
blocked_by list. While ANY referenced bead is still open, the engine pauses
evaluation of the trigger without updating last_checked.

Example:
  gb trigger block ka-trigABCDE --decision ka-decFGHIJ`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if triggerBlockDecision == "" {
			return fmt.Errorf("--decision <bead-id> is required")
		}
		return mutateBlockedBy(cmd.Context(), args[0], triggerBlockDecision, true)
	},
}

var triggerUnblockCmd = &cobra.Command{
	Use:   "unblock <trigger-id> --decision <bead-id>",
	Short: "Remove a bead ID from the trigger's blocked_by list",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if triggerUnblockDecision == "" {
			return fmt.Errorf("--decision <bead-id> is required")
		}
		return mutateBlockedBy(cmd.Context(), args[0], triggerUnblockDecision, false)
	},
}

var triggerShowCmd = &cobra.Command{
	Use:   "show <trigger-id>",
	Short: "Display a trigger bead and its blocked_by status",
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerShow,
}

// mutateBlockedBy adds or removes a single bead ID from the trigger's
// blocked_by field. The field is stored as a JSON array on the bead so it
// round-trips cleanly through parseStringList in either CSV or JSON form.
func mutateBlockedBy(ctx context.Context, triggerID, decisionID string, add bool) error {
	bead, err := daemon.GetBead(ctx, triggerID)
	if err != nil {
		return fmt.Errorf("fetching trigger %s: %w", triggerID, err)
	}
	if bead == nil {
		return fmt.Errorf("trigger %s not found", triggerID)
	}
	if bead.Type != "trigger" {
		return fmt.Errorf("bead %s is type=%q, expected type=trigger", triggerID, bead.Type)
	}

	existing := parseBlockedByField(bead.Fields["blocked_by"])
	updated := existing[:0:0]
	seen := make(map[string]struct{}, len(existing)+1)
	for _, id := range existing {
		id = strings.TrimSpace(id)
		if id == "" || id == decisionID {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		updated = append(updated, id)
	}
	if add {
		updated = append(updated, decisionID)
	}

	var fieldValue any
	if len(updated) == 0 {
		fieldValue = ""
	} else {
		j, err := json.Marshal(updated)
		if err != nil {
			return fmt.Errorf("marshalling blocked_by: %w", err)
		}
		fieldValue = string(j)
	}

	if err := daemon.UpdateBeadFieldsTyped(ctx, triggerID, map[string]any{
		"blocked_by": fieldValue,
	}); err != nil {
		return fmt.Errorf("updating trigger %s: %w", triggerID, err)
	}

	if jsonOutput {
		printJSON(map[string]any{
			"id":         triggerID,
			"blocked_by": updated,
			"action":     map[bool]string{true: "block", false: "unblock"}[add],
			"decision":   decisionID,
		})
		return nil
	}
	verb := "Blocked"
	if !add {
		verb = "Unblocked"
	}
	fmt.Printf("%s trigger %s by %s\n", verb, triggerID, decisionID)
	if len(updated) == 0 {
		fmt.Println("  blocked_by: (empty — trigger will evaluate normally)")
	} else {
		fmt.Printf("  blocked_by: %s\n", strings.Join(updated, ", "))
	}
	return nil
}

func runTriggerShow(cmd *cobra.Command, args []string) error {
	id := args[0]
	bead, err := daemon.GetBead(cmd.Context(), id)
	if err != nil {
		return fmt.Errorf("fetching trigger %s: %w", id, err)
	}
	if bead == nil {
		return fmt.Errorf("trigger %s not found", id)
	}

	blocked := parseBlockedByField(bead.Fields["blocked_by"])
	type refStatus struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Title  string `json:"title,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	refs := make([]refStatus, 0, len(blocked))
	for _, ref := range blocked {
		rb, err := daemon.GetBead(cmd.Context(), ref)
		switch {
		case err != nil:
			refs = append(refs, refStatus{ID: ref, Status: "?", Error: err.Error()})
		case rb == nil:
			refs = append(refs, refStatus{ID: ref, Status: "missing"})
		default:
			refs = append(refs, refStatus{ID: ref, Status: rb.Status, Title: rb.Title})
		}
	}

	pourStalled := bead.Fields["pour_stall"] == "true"

	if jsonOutput {
		out := map[string]any{
			"id":         bead.ID,
			"title":      bead.Title,
			"type":       bead.Type,
			"status":     bead.Status,
			"labels":     bead.Labels,
			"fields":     bead.Fields,
			"blocked_by": refs,
		}
		out["pour_stall"] = pourStalled
		printJSON(out)
		return nil
	}

	cmd.Printf("Trigger %s\n", bead.ID)
	cmd.Printf("  title:  %s\n", bead.Title)
	cmd.Printf("  status: %s\n", bead.Status)
	cmd.Printf("  source: %s\n", triggerSourceFromLabels(bead.Labels))
	if v := bead.Fields["enabled"]; v != "" {
		cmd.Printf("  enabled: %s\n", v)
	}
	if v := bead.Fields["action"]; v != "" {
		cmd.Printf("  action: %s\n", v)
	}

	// Pouring-but-never-worked stall verdict (ka-0000000957), stamped by the
	// engine's pouring-stall sweep. A first-class warning banner so this class
	// of silent stall — a trigger that looks alive (fresh last_checked) while
	// pouring molecules nobody works — is caught at a glance.
	if pourStalled {
		cmd.Printf("  ⚠️  POURING-BUT-NEVER-WORKED: %s inert molecule(s), oldest quiet for %s (since %s)\n",
			defaultIfEmpty(bead.Fields["pour_stall_molecules"], "?"),
			defaultIfEmpty(bead.Fields["pour_stall_oldest_age"], "?"),
			defaultIfEmpty(bead.Fields["pour_stall_detected_at"], "?"))
		cmd.Println("      The trigger keeps pouring molecules on cadence but they are not being worked —")
		cmd.Println("      the driver spawn is likely failing or the drivers are making no progress.")
	}
	if len(refs) == 0 {
		cmd.Println("  blocked_by: (none)")
	} else {
		cmd.Println("  blocked_by:")
		for _, r := range refs {
			line := fmt.Sprintf("    - %s [%s]", r.ID, r.Status)
			if r.Title != "" {
				line += " " + r.Title
			}
			if r.Error != "" {
				line += " (error: " + r.Error + ")"
			}
			cmd.Println(line)
		}
	}
	cmd.Println("  other fields:")
	for _, k := range sortedFieldKeys(bead.Fields) {
		switch k {
		case "blocked_by", "enabled", "action",
			// Surfaced in the pouring-stall banner above; skip in the catch-all.
			"pour_stall", "pour_stall_molecules", "pour_stall_oldest_age", "pour_stall_detected_at":
			continue
		}
		cmd.Printf("    %s: %s\n", k, bead.Fields[k])
	}
	return nil
}

// parseBlockedByField mirrors parseStringList's accepted shapes (JSON array or
// CSV) for the CLI side. Kept separate to avoid coupling cmd/gb to bridge
// internals.
func parseBlockedByField(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "[") {
		var arr []string
		if err := json.Unmarshal([]byte(raw), &arr); err == nil {
			out := arr[:0]
			for _, s := range arr {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
			return out
		}
	}
	parts := strings.Split(raw, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sortedFieldKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func init() {
	triggerBlockCmd.Flags().StringVar(&triggerBlockDecision, "decision", "", "bead ID (decision or escalation) to add to blocked_by")
	triggerUnblockCmd.Flags().StringVar(&triggerUnblockDecision, "decision", "", "bead ID to remove from blocked_by")

	triggerCmd.AddCommand(triggerBlockCmd)
	triggerCmd.AddCommand(triggerUnblockCmd)
	triggerCmd.AddCommand(triggerShowCmd)
}
