package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

var jackListCmd = &cobra.Command{
	Use:   "list",
	Short: "List jacks",
	RunE: func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")
		expiredOnly, _ := cmd.Flags().GetBool("expired")

		statuses := []string{"in_progress"}
		if all {
			statuses = []string{"open", "in_progress", "closed"}
		}

		resp, err := beadsClient.ListBeads(context.Background(), &client.ListBeadsRequest{
			Type:   []string{"jack"},
			Status: statuses,
			Limit:  100,
		})
		if err != nil {
			return fmt.Errorf("listing jacks: %w", err)
		}

		if jsonOutput {
			printBeadListJSONWithTotal(resp.Beads, resp.Total, jsonOutput)
			return nil
		}

		now := time.Now().UTC()
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSTATUS\tTARGET\tTTL\tEXPIRY\tCHANGES\tLABELS")

		for _, bead := range resp.Beads {
			var fields map[string]any
			if len(bead.Fields) > 0 {
				_ = json.Unmarshal(bead.Fields, &fields)
			}

			target, _ := fields["jack_target"].(string)
			ttlStr, _ := fields["jack_ttl"].(string)
			expiresStr, _ := fields["jack_expires_at"].(string)
			changes := 0
			if v, ok := fields["jack_changes"].([]any); ok {
				changes = len(v)
			}

			expiry := "?"
			isExpired := false
			if expiresAt, err := time.Parse(time.RFC3339, expiresStr); err == nil {
				if now.After(expiresAt) {
					expiry = "EXPIRED"
					isExpired = true
				} else {
					expiry = expiresAt.Sub(now).Truncate(time.Second).String()
				}
			}

			if expiredOnly && !isExpired {
				continue
			}

			lbls := filterJackLabels(bead.Labels)

			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d/%d\t%s\n",
				bead.ID,
				bead.Status,
				truncate(target, 30),
				ttlStr,
				expiry,
				changes, model.JackMaxChanges,
				strings.Join(lbls, ","),
			)
		}
		w.Flush()
		fmt.Printf("\n%d jacks\n", len(resp.Beads))
		return nil
	},
}

func filterJackLabels(labels []string) []string {
	var result []string
	for _, l := range labels {
		if strings.HasPrefix(l, "jack:") {
			result = append(result, l)
		}
	}
	return result
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func init() {
	jackListCmd.Flags().BoolP("all", "a", false, "include closed jacks")
	jackListCmd.Flags().Bool("expired", false, "show only expired jacks")
}
