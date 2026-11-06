package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var dmCmd = &cobra.Command{
	Use:     "dm",
	Short:   "Send and track direct messages to engineers via Slack",
	Long:    `Send DMs to Slack users through the bridge. DMs are tracked as type=dm beads.`,
	GroupID: "orchestration",
}

// ── dm send ─────────────────────────────────────────────────────────

var dmSendCmd = &cobra.Command{
	Use:   "send <user> [user2 user3 ...]",
	Short: "Send a DM to one or more Slack users",
	Long: `Creates a dm bead that the bridge delivers as a Slack DM.

Targets can be Slack user IDs (U...) or usernames from the user directory.
The bridge watches for new dm beads and calls SendDM to deliver them.

With a single target, a regular DM is sent. With multiple targets, a group
DM (multi-party conversation) is created with all specified users.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		subject, _ := cmd.Flags().GetString("subject")
		// DELIBERATELY LITERAL — no @file/@-/@@ expansion on --body
		// (ka-0000003818). A DM is a short Slack-facing message rendered
		// straight into a conversation where a leading "@" is mention syntax,
		// not a curl-style file pointer. Same reasoning as `gb squawk`: silent
		// file-reads or fail-loud-on-"@user" would surprise the common case.
		// Long file-sourced prose belongs in `gb mail send --body`, which DOES
		// expand @file. See TestDMSend_LeadingAtStoredLiteral.
		body, _ := cmd.Flags().GetString("body")

		if subject == "" && body == "" {
			return fmt.Errorf("at least one of --subject (-s) or --body (-b) is required")
		}

		sender := resolveMailActor()

		// Collect all targets, expanding comma-separated values.
		var targets []string
		for _, arg := range args {
			for _, t := range strings.Split(arg, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					targets = append(targets, t)
				}
			}
		}

		isGroup := len(targets) > 1

		fields := map[string]string{
			"target_user":  targets[0],
			"source_agent": sender,
		}
		if isGroup {
			fields["target_users"] = strings.Join(targets, ",")
		}
		if subject != "" {
			fields["subject"] = subject
		}
		if body != "" {
			fields["body"] = body
		}

		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("marshalling fields: %w", err)
		}

		title := subject
		if title == "" {
			title = truncateDMTitle(body)
		}

		labels := []string{fmt.Sprintf("from:%s", sender)}
		for _, t := range targets {
			labels = append(labels, fmt.Sprintf("to:%s", t))
		}
		if isGroup {
			labels = append(labels, "dm-group")
		}

		id, err := daemon.CreateBead(cmd.Context(), beadsapi.CreateBeadRequest{
			Title:     title,
			Type:      "dm",
			Kind:      "data",
			Labels:    labels,
			Fields:    json.RawMessage(fieldsJSON),
			CreatedBy: sender,
			Priority:  2,
		})
		if err != nil {
			return fmt.Errorf("creating dm bead: %w", err)
		}

		targetDisplay := strings.Join(targets, ", ")
		if jsonOutput {
			printJSON(map[string]string{"id": id, "status": "queued", "group": fmt.Sprintf("%v", isGroup)})
		} else {
			if isGroup {
				fmt.Printf("Group DM queued: %s → [%s] (id: %s)\n", sender, targetDisplay, id)
			} else {
				fmt.Printf("DM queued: %s → %s (id: %s)\n", sender, targetDisplay, id)
			}
		}
		return nil
	},
}

// ── dm list ─────────────────────────────────────────────────────────

var dmListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recent outbound DMs",
	RunE: func(cmd *cobra.Command, args []string) error {
		status, _ := cmd.Flags().GetStringSlice("status")
		limit, _ := cmd.Flags().GetInt("limit")

		if len(status) == 0 {
			status = []string{"open", "closed"}
		}

		sender := resolveMailActor()
		result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
			Types:    []string{"dm"},
			Statuses: status,
			Labels:   []string{fmt.Sprintf("from:%s", sender)},
			Sort:     "-created_at",
			Limit:    limit,
		})
		if err != nil {
			return fmt.Errorf("listing DMs: %w", err)
		}

		if jsonOutput {
			printJSON(result.Beads)
		} else if len(result.Beads) == 0 {
			fmt.Println("No DMs sent")
		} else {
			for _, b := range result.Beads {
				replied := ""
				if b.Fields["replied"] == "true" {
					replied = " [replied]"
				}
				fmt.Printf("  %s  %-12s  %s%s\n", b.ID, b.Fields["target_user"], b.Title, replied)
			}
			fmt.Printf("\n%d DM(s)\n", len(result.Beads))
		}
		return nil
	},
}

// ── dm replies ──────────────────────────────────────────────────────

var dmRepliesCmd = &cobra.Command{
	Use:   "replies <dm-id>",
	Short: "Check for replies to a DM",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		bead, err := daemon.GetBead(cmd.Context(), args[0])
		if err != nil {
			return fmt.Errorf("fetching DM %s: %w", args[0], err)
		}

		if jsonOutput {
			printJSON(bead)
		} else {
			fmt.Printf("DM:      %s\n", bead.ID)
			fmt.Printf("To:      %s\n", bead.Fields["target_user"])
			fmt.Printf("Subject: %s\n", bead.Title)
			if bead.Fields["replied"] == "true" {
				fmt.Printf("Reply:   %s\n", bead.Fields["reply_text"])
			} else {
				fmt.Println("Reply:   (no reply yet)")
			}
			fmt.Printf("Status:  %s\n", bead.Status)
		}
		return nil
	},
}

// ── dm ask ──────────────────────────────────────────────────────────

var dmAskCmd = &cobra.Command{
	Use:   "ask <slack-user-id-or-username> <question>",
	Short: "Ask an engineer a question via DM",
	Long: `Creates a dm bead in question mode. The bridge delivers the question as a Slack DM
with task context. When the engineer replies in the DM thread, the bridge captures
the reply, updates the bead, and nudges the requesting agent.

Use gb dm replies <dm-id> to check for a response.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		target := args[0]
		question := args[1]
		taskID, _ := cmd.Flags().GetString("task")
		timeout, _ := cmd.Flags().GetInt("timeout")

		sender := resolveMailActor()

		// Auto-detect task context from environment.
		if taskID == "" {
			taskID = os.Getenv("BOAT_TASK_ID")
		}

		fields := map[string]any{
			"target_user":     target,
			"source_agent":    sender,
			"body":            question,
			"dm_mode":         "question",
			"timeout_minutes": timeout,
		}
		if taskID != "" {
			fields["task_context"] = taskID
		}

		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("marshalling fields: %w", err)
		}

		title := truncateDMTitle(question)

		id, err := daemon.CreateBead(cmd.Context(), beadsapi.CreateBeadRequest{
			Title:     title,
			Type:      "dm",
			Kind:      "data",
			Labels:    []string{fmt.Sprintf("from:%s", sender), fmt.Sprintf("to:%s", target), "dm-question"},
			Fields:    json.RawMessage(fieldsJSON),
			CreatedBy: sender,
			Priority:  2,
		})
		if err != nil {
			return fmt.Errorf("creating question dm bead: %w", err)
		}

		if jsonOutput {
			printJSON(map[string]string{"id": id, "status": "queued", "mode": "question"})
		} else {
			fmt.Printf("Question DM queued: %s → %s (id: %s)\n", sender, target, id)
			fmt.Printf("Check for reply with: gb dm replies %s\n", id)
		}
		return nil
	},
}

// ── dm wait ─────────────────────────────────────────────────────────

var dmWaitCmd = &cobra.Command{
	Use:   "wait <dm-id>",
	Short: "Wait for a reply to a question DM",
	Long: `Polls the dm bead until a reply is received or timeout is reached.
Returns the reply text on success. Useful for agents that want to block
until an engineer responds.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pollInterval, _ := cmd.Flags().GetInt("poll")
		maxWait, _ := cmd.Flags().GetInt("max-wait")

		deadline := maxWait * 60 // seconds
		elapsed := 0

		for elapsed < deadline {
			bead, err := daemon.GetBead(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("fetching DM %s: %w", args[0], err)
			}

			if bead.Fields["replied"] == "true" {
				if jsonOutput {
					printJSON(map[string]string{
						"id":         bead.ID,
						"replied":    "true",
						"reply_text": bead.Fields["reply_text"],
						"reply_user": bead.Fields["reply_user"],
					})
				} else {
					fmt.Printf("Reply received from %s:\n%s\n", bead.Fields["reply_user"], bead.Fields["reply_text"])
				}
				return nil
			}

			if bead.Status == "closed" && bead.Fields["replied"] != "true" {
				if jsonOutput {
					printJSON(map[string]string{"id": bead.ID, "replied": "false", "status": "closed"})
				} else {
					fmt.Println("DM was closed without a reply")
				}
				return nil
			}

			// Sleep before next poll.
			cmd.SilenceUsage = true
			select {
			case <-cmd.Context().Done():
				return cmd.Context().Err()
			case <-sleepChan(pollInterval):
			}
			elapsed += pollInterval
		}

		if jsonOutput {
			printJSON(map[string]string{"id": args[0], "replied": "false", "reason": "timeout"})
		} else {
			fmt.Println("Timed out waiting for reply")
		}
		return nil
	},
}

// sleepChan returns a channel that receives after n seconds.
func sleepChan(seconds int) <-chan time.Time {
	return time.After(time.Duration(seconds) * time.Second)
}

// ── dm review ──────────────────────────────────────────────────────

var dmReviewCmd = &cobra.Command{
	Use:   "review <mr-url> [--reviewer <user>]",
	Short: "Request an MR review via DM",
	Long: `Creates a dm bead in review mode. The bridge delivers a review request DM
to the specified reviewer with the MR title, description summary, CI status,
and a link. When the reviewer replies in the DM thread, the bridge captures
the reply, updates the bead, and nudges the requesting agent.

The reviewer can be a Slack user ID (U...) or a username from the user directory.
If --reviewer is omitted, the command requires you to specify one explicitly.

Use gb dm replies <dm-id> to check for a response.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mrURL := args[0]
		reviewer, _ := cmd.Flags().GetString("reviewer")
		taskID, _ := cmd.Flags().GetString("task")
		timeout, _ := cmd.Flags().GetInt("timeout")
		message, _ := cmd.Flags().GetString("message")

		if reviewer == "" {
			return fmt.Errorf("--reviewer is required (Slack user ID or username)")
		}

		sender := resolveMailActor()

		// Auto-detect task context from environment.
		if taskID == "" {
			taskID = os.Getenv("BOAT_TASK_ID")
		}

		fields := map[string]any{
			"target_user":     reviewer,
			"source_agent":    sender,
			"mr_url":          mrURL,
			"dm_mode":         "review",
			"timeout_minutes": timeout,
		}
		if taskID != "" {
			fields["task_context"] = taskID
		}
		if message != "" {
			fields["body"] = message
		}

		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("marshalling fields: %w", err)
		}

		title := fmt.Sprintf("Review request: %s", truncateDMTitle(mrURL))

		id, err := daemon.CreateBead(cmd.Context(), beadsapi.CreateBeadRequest{
			Title:     title,
			Type:      "dm",
			Kind:      "data",
			Labels:    []string{fmt.Sprintf("from:%s", sender), fmt.Sprintf("to:%s", reviewer), "dm-review"},
			Fields:    json.RawMessage(fieldsJSON),
			CreatedBy: sender,
			Priority:  2,
		})
		if err != nil {
			return fmt.Errorf("creating review dm bead: %w", err)
		}

		if jsonOutput {
			printJSON(map[string]string{"id": id, "status": "queued", "mode": "review"})
		} else {
			fmt.Printf("Review DM queued: %s → %s (id: %s)\n", sender, reviewer, id)
			fmt.Printf("MR: %s\n", mrURL)
			fmt.Printf("Check for reply with: gb dm replies %s\n", id)
		}
		return nil
	},
}

// ── helpers ─────────────────────────────────────────────────────────

// truncateDMTitle creates a title from the body text, truncated to 100 chars.
func truncateDMTitle(body string) string {
	if len(body) <= 100 {
		return body
	}
	return body[:97] + "..."
}

func init() {
	dmSendCmd.Flags().StringP("subject", "s", "", "DM subject line")
	dmSendCmd.Flags().StringP("body", "b", "", "DM body text")

	dmAskCmd.Flags().String("task", "", "task bead ID for context (default: $BOAT_TASK_ID)")
	dmAskCmd.Flags().Int("timeout", 30, "minutes before sending reminder (0 to disable)")

	dmReviewCmd.Flags().String("reviewer", "", "Slack user ID or username of the reviewer (required)")
	dmReviewCmd.Flags().String("task", "", "task bead ID for context (default: $BOAT_TASK_ID)")
	dmReviewCmd.Flags().Int("timeout", 60, "minutes before sending reminder (0 to disable)")
	dmReviewCmd.Flags().StringP("message", "m", "", "optional message to include with the review request")

	dmWaitCmd.Flags().Int("poll", 10, "poll interval in seconds")
	dmWaitCmd.Flags().Int("max-wait", 60, "maximum wait time in minutes")

	dmListCmd.Flags().StringSlice("status", nil, "filter by status (default: open,closed)")
	dmListCmd.Flags().Int("limit", 20, "maximum DMs to show")

	dmCmd.AddCommand(dmSendCmd)
	dmCmd.AddCommand(dmAskCmd)
	dmCmd.AddCommand(dmReviewCmd)
	dmCmd.AddCommand(dmWaitCmd)
	dmCmd.AddCommand(dmListCmd)
	dmCmd.AddCommand(dmRepliesCmd)
}
