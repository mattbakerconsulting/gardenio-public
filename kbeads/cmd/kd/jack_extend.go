package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

var jackExtendCmd = &cobra.Command{
	Use:   "extend <jack-id>",
	Short: "Extend a jack's TTL",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		ttlStr, _ := cmd.Flags().GetString("ttl")
		reason, _ := cmd.Flags().GetString("reason")

		if ttlStr == "" {
			return fmt.Errorf("--ttl is required")
		}

		ttl, err := time.ParseDuration(ttlStr)
		if err != nil {
			return fmt.Errorf("invalid TTL %q: %w", ttlStr, err)
		}
		if ttl > model.JackMaxSingleExtension {
			return fmt.Errorf("single extension %v exceeds maximum %v", ttl, model.JackMaxSingleExtension)
		}

		// Fetch current jack.
		bead, err := beadsClient.GetBead(context.Background(), id)
		if err != nil {
			return fmt.Errorf("getting jack %s: %w", id, err)
		}
		if string(bead.Type) != "jack" {
			return fmt.Errorf("%s is not a jack", id)
		}
		if string(bead.Status) == "closed" {
			return fmt.Errorf("jack %s is already closed", id)
		}

		var fields map[string]any
		if len(bead.Fields) > 0 {
			_ = json.Unmarshal(bead.Fields, &fields)
		}
		if fields == nil {
			fields = make(map[string]any)
		}

		// Check extension count.
		extCount := 0
		if v, ok := fields["jack_extension_count"].(float64); ok {
			extCount = int(v)
		}
		if extCount >= model.JackMaxExtensions {
			return fmt.Errorf("jack has reached maximum %d extensions", model.JackMaxExtensions)
		}

		// Check cumulative TTL.
		var cumulativeTTL time.Duration
		if v, ok := fields["jack_cumulative_ttl"].(string); ok && v != "" {
			cumulativeTTL, _ = time.ParseDuration(v)
		}
		if cumulativeTTL+ttl > model.JackMaxCumulativeTTL {
			return fmt.Errorf("cumulative TTL %v + %v exceeds maximum %v",
				cumulativeTTL, ttl, model.JackMaxCumulativeTTL)
		}

		// Save original TTL on first extension.
		if extCount == 0 {
			if v, ok := fields["jack_ttl"]; ok {
				fields["jack_original_ttl"] = v
			}
		}

		now := time.Now().UTC()
		existingExpiry, _ := fields["jack_expires_at"].(string)
		newExpiry := jackExtendedExpiry(existingExpiry, now, ttl)
		fields["jack_expires_at"] = newExpiry.Format(time.RFC3339)
		fields["jack_extension_count"] = extCount + 1
		fields["jack_cumulative_ttl"] = (cumulativeTTL + ttl).String()

		fieldsJSON, _ := json.Marshal(fields)

		_, err = beadsClient.UpdateBead(context.Background(), id, &client.UpdateBeadRequest{
			Fields: fieldsJSON,
		})
		if err != nil {
			return fmt.Errorf("updating jack %s: %w", id, err)
		}

		// Add comment recording extension.
		comment := fmt.Sprintf("Extended TTL by %s (extension %d/%d)", ttlStr, extCount+1, model.JackMaxExtensions)
		if reason != "" {
			comment += ": " + reason
		}
		_, _ = beadsClient.AddComment(context.Background(), id, actor, comment)

		if jsonOutput {
			bead, _ = beadsClient.GetBead(context.Background(), id)
			printBeadJSON(bead)
		} else {
			fmt.Printf("Jack extended: %s\n", id)
			fmt.Printf("  New expiry: %s\n", newExpiry.Format("2006-01-02 15:04:05"))
			fmt.Printf("  Extensions: %d/%d\n", extCount+1, model.JackMaxExtensions)
		}
		return nil
	},
}

// jackExtendedExpiry computes a jack's new expiry when extended by ttl. It extends
// from the jack's existing deadline (existingExpiry, RFC3339) rather than from now, so
// an extension always pushes the deadline OUT and never discards the time still left on
// the jack. The base is clamped to now: a jack whose deadline already lapsed (or whose
// stored expiry is missing/unparseable) buys ttl from the present, not retroactively
// from a stale past deadline.
func jackExtendedExpiry(existingExpiry string, now time.Time, ttl time.Duration) time.Time {
	base := now.UTC()
	if existingExpiry != "" {
		if existing, err := time.Parse(time.RFC3339, existingExpiry); err == nil && existing.After(base) {
			base = existing.UTC()
		}
	}
	return base.Add(ttl)
}

func init() {
	jackExtendCmd.Flags().String("ttl", "", "additional TTL duration (required)")
	jackExtendCmd.Flags().StringP("reason", "r", "", "why more time is needed")
}
