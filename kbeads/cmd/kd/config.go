package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:     "config",
	Short:   "Manage configs (deprecated — use config beads instead)",
	GroupID: "system",
}

var configCreateCmd = &cobra.Command{
	Use:        "create <key> <json-value>",
	Aliases:    []string{"new"},
	Short:      "Create or update a config (deprecated)",
	Args:       cobra.ExactArgs(2),
	Deprecated: "KV configs are superseded by config beads. Use 'gb config load' instead.",
	RunE: func(cmd *cobra.Command, args []string) error {
		key := args[0]
		value := []byte(args[1])

		if !json.Valid(value) {
			return fmt.Errorf("value must be valid JSON")
		}

		config, err := beadsClient.SetConfig(context.Background(), key, value)
		if err != nil {
			return fmt.Errorf("setting config %q: %w", key, err)
		}

		printConfigJSON(config)
		return nil
	},
}

var configGetCmd = &cobra.Command{
	Use:        "get <key>",
	Short:      "Get a config by key (deprecated)",
	Args:       cobra.ExactArgs(1),
	Deprecated: "KV configs are superseded by config beads. Use 'kd list --type=config' instead.",
	RunE: func(cmd *cobra.Command, args []string) error {
		config, err := beadsClient.GetConfig(context.Background(), args[0])
		if err != nil {
			return fmt.Errorf("getting config %q: %w", args[0], err)
		}

		printConfigJSON(config)
		return nil
	},
}

var configListCmd = &cobra.Command{
	Use:        "list [namespace]",
	Short:      "List configs by namespace (deprecated)",
	Args:       cobra.MaximumNArgs(1),
	Deprecated: "KV configs are superseded by config beads. Use 'kd list --type=config' instead.",
	RunE: func(cmd *cobra.Command, args []string) error {
		namespace := ""
		if len(args) > 0 {
			namespace = args[0]
		}
		if namespace == "" {
			return fmt.Errorf("namespace argument is required")
		}

		configs, err := beadsClient.ListConfigs(context.Background(), namespace)
		if err != nil {
			return fmt.Errorf("listing configs in %q: %w", namespace, err)
		}

		for _, c := range configs {
			printConfigJSON(c)
		}
		if len(configs) == 0 {
			fmt.Println("No configs found.")
		}
		return nil
	},
}

var configDeleteCmd = &cobra.Command{
	Use:        "delete <key>",
	Short:      "Delete a config by key (deprecated)",
	Args:       cobra.ExactArgs(1),
	Deprecated: "KV configs are superseded by config beads. Manage config beads via 'gb config' instead.",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Fprintf(os.Stderr, "Warning: this deletes from the legacy KV store, not from config beads.\n")
		if err := beadsClient.DeleteConfig(context.Background(), args[0]); err != nil {
			return fmt.Errorf("deleting config %q: %w", args[0], err)
		}

		fmt.Printf("Deleted config %q\n", args[0])
		return nil
	},
}

func printConfigJSON(c *model.Config) {
	var valueObj any
	_ = json.Unmarshal(c.Value, &valueObj)

	out := map[string]any{
		"key":   c.Key,
		"value": valueObj,
	}
	if !c.CreatedAt.IsZero() {
		out["created_at"] = c.CreatedAt.Format("2006-01-02T15:04:05Z")
	}
	if !c.UpdatedAt.IsZero() {
		out["updated_at"] = c.UpdatedAt.Format("2006-01-02T15:04:05Z")
	}

	emitJSON(out)
}

func init() {
	configCmd.AddCommand(configCreateCmd)
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configListCmd)
	configCmd.AddCommand(configDeleteCmd)
}
