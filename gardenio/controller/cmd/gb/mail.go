package main

import (
	"fmt"
	"os"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/mailproto"

	"github.com/spf13/cobra"
)

// resolveMailActor returns the actor name for mail operations.
func resolveMailActor() string {
	if v := os.Getenv("KD_ACTOR"); v != "" {
		return v
	}
	return actor
}

var mailCmd = &cobra.Command{
	Use:     "mail",
	Short:   "Agent-to-agent mail via beads",
	Long:    `Send and receive mail between agents. Mail items are beads with type="mail".`,
	GroupID: "orchestration",
}

// ── mail send ────────────────────────────────────────────────────────

var mailSendCmd = &cobra.Command{
	Use:   "send <recipient>",
	Short: "Send mail to another agent",
	Long: `Send mail to another agent. Mail is delivered via periodic hook checks.

Use --interrupt to trigger an immediate nudge to the recipient agent,
bypassing the periodic poll. Use --priority to set urgency (0=critical,
1=high, 2=normal, 3=low); priority ≤ 1 also triggers an immediate nudge.

Use --label (repeatable) to add labels after from:<sender>. control:<verb>
makes the mail a control message (kill, escalation-approved, permission); any other
control: verb is refused. See docs/agent-mail-protocol.md.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		recipient := args[0]
		subject, _ := cmd.Flags().GetString("subject")
		body, _ := cmd.Flags().GetString("body")
		priority, _ := cmd.Flags().GetInt("priority")
		interrupt, _ := cmd.Flags().GetBool("interrupt")

		if subject == "" {
			return fmt.Errorf("--subject (-s) is required")
		}

		// Expand curl-style @file / @- / @@ on --body (ka-0000003818). Mail
		// bodies are long-form agent-to-agent prose an agent commonly drafts in
		// a file, matching the `kd -d @file` convention; a silently-stored
		// "@/path" pointer would deliver a dangling path as the message. A
		// missing/typo'd path fails LOUD. --subject is the one-line title and is
		// left literal.
		if body != "" {
			var err error
			body, err = resolveTextValue(body)
			if err != nil {
				return fmt.Errorf("--body: %w", err)
			}
		}

		if priority < 0 || priority > 3 {
			return fmt.Errorf("--priority must be 0 (critical), 1 (high), 2 (normal), or 3 (low)")
		}

		sender := resolveMailActor()

		labels := []string{mailproto.FromLabel(sender)}
		if interrupt {
			labels = append(labels, mailproto.LabelDeliveryInterrupt)
		}
		extra, _ := cmd.Flags().GetStringArray("label")
		if err := mailproto.ValidateLabels(extra); err != nil {
			return fmt.Errorf("--label: %w", err)
		}
		labels = append(labels, extra...)

		id, err := daemon.CreateBead(cmd.Context(), beadsapi.CreateBeadRequest{
			Title:       subject,
			Type:        "mail",
			Kind:        "data",
			Description: body,
			Assignee:    recipient,
			Labels:      labels,
			CreatedBy:   sender,
			Priority:    priority,
		})
		if err != nil {
			return fmt.Errorf("sending mail: %w", err)
		}

		if jsonOutput {
			printJSON(map[string]string{"id": id})
		} else {
			fmt.Printf("Sent: %s → %s (id: %s)\n", sender, recipient, id)
		}
		return nil
	},
}

// ── mail inbox / inbox ────────────────────────────────────────────────

func runInbox(cmd *cobra.Command, args []string) error {
	me := resolveMailActor()
	limit, _ := cmd.Flags().GetInt("limit")

	result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
		Types:    []string{"mail"},
		Statuses: []string{"open"},
		Assignee: me,
		Sort:     "-created_at",
		Limit:    limit,
	})
	if err != nil {
		return fmt.Errorf("listing inbox: %w", err)
	}

	if jsonOutput {
		printJSON(result.Beads)
	} else if len(result.Beads) == 0 {
		fmt.Println("No mail")
	} else {
		for _, b := range result.Beads {
			printMailLine(b)
		}
		fmt.Printf("\n%d message(s)\n", len(result.Beads))
	}
	return nil
}

var mailInboxCmd = &cobra.Command{
	Use:   "inbox",
	Short: "Show your mail inbox",
	RunE:  runInbox,
}

var inboxCmd = &cobra.Command{
	Use:     "inbox",
	Short:   "Show your mail inbox (alias for 'mail inbox')",
	GroupID: "orchestration",
	RunE:    runInbox,
}

// ── mail read ────────────────────────────────────────────────────────

var mailReadCmd = &cobra.Command{
	Use:   "read <id>",
	Short: "Read a mail message",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		bead, err := daemon.GetBead(cmd.Context(), args[0])
		if err != nil {
			return fmt.Errorf("reading mail %s: %w", args[0], err)
		}

		if jsonOutput {
			printJSON(bead)
		} else {
			printMailDetail(bead)
		}
		return nil
	},
}

// ── mail list ────────────────────────────────────────────────────────

var mailListCmd = &cobra.Command{
	Use:   "list",
	Short: "List mail with optional filters",
	RunE: func(cmd *cobra.Command, args []string) error {
		status, _ := cmd.Flags().GetStringSlice("status")
		limit, _ := cmd.Flags().GetInt("limit")

		if len(status) == 0 {
			status = []string{"open"}
		}

		me := resolveMailActor()
		result, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
			Types:    []string{"mail"},
			Statuses: status,
			Assignee: me,
			Sort:     "-created_at",
			Limit:    limit,
		})
		if err != nil {
			return fmt.Errorf("listing mail: %w", err)
		}

		if jsonOutput {
			printJSON(result.Beads)
		} else if len(result.Beads) == 0 {
			fmt.Println("No mail")
		} else {
			for _, b := range result.Beads {
				printMailLine(b)
			}
			fmt.Printf("\n%d message(s)\n", len(result.Beads))
		}
		return nil
	},
}

// ── helpers ─────────────────────────────────────────────────────────

func printMailLine(b *beadsapi.BeadDetail) {
	sender := senderFromLabels(b.Labels)
	fmt.Printf("  %s  %-12s  %s\n", b.ID, sender, b.Title)
}

func printMailDetail(b *beadsapi.BeadDetail) {
	sender := senderFromLabels(b.Labels)
	fmt.Printf("From:    %s\n", sender)
	fmt.Printf("To:      %s\n", b.Assignee)
	fmt.Printf("Subject: %s\n", b.Title)
	if b.Description != "" {
		fmt.Printf("Body:    %s\n", b.Description)
	}
	fmt.Printf("ID:      %s\n", b.ID)
}

func init() {
	mailSendCmd.Flags().StringP("subject", "s", "", "mail subject (required)")
	mailSendCmd.Flags().StringP("body", "b", "", "mail body (@file reads the value from a file, @- reads stdin; @@ escapes a literal leading '@')")
	mailSendCmd.Flags().IntP("priority", "p", 2, "priority: 0=critical, 1=high, 2=normal, 3=low (≤1 triggers immediate nudge)")
	mailSendCmd.Flags().Bool("interrupt", false, "deliver immediately via nudge (adds delivery:interrupt label)")
	mailSendCmd.Flags().StringArray("label", nil, "extra label (repeatable); control:<verb> is reserved for kill, escalation-approved and permission")

	mailInboxCmd.Flags().Int("limit", 20, "maximum messages to show")
	mailListCmd.Flags().StringSlice("status", nil, "filter by status (default: open)")
	mailListCmd.Flags().Int("limit", 20, "maximum messages to show")
	inboxCmd.Flags().Int("limit", 20, "maximum messages to show")

	mailCmd.AddCommand(mailSendCmd)
	mailCmd.AddCommand(mailInboxCmd)
	mailCmd.AddCommand(mailReadCmd)
	mailCmd.AddCommand(mailListCmd)
}
