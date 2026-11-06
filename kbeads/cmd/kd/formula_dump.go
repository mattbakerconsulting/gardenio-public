package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// runFormulaDump fetches a formula and writes its fields JSON.
// If filePath is empty, output goes to stdout.
func runFormulaDump(id, filePath string) error {
	bead, err := beadsClient.GetBead(context.Background(), id)
	if err != nil {
		return fmt.Errorf("getting formula: %w", err)
	}

	if string(bead.Type) != "formula" {
		return fmt.Errorf("bead %s is type %q, not formula", id, bead.Type)
	}

	if len(bead.Fields) == 0 {
		return fmt.Errorf("formula %s has no fields", id)
	}

	// Re-marshal for pretty printing.
	var raw json.RawMessage
	if err := json.Unmarshal(bead.Fields, &raw); err != nil {
		return fmt.Errorf("parsing fields: %w", err)
	}
	pretty, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("formatting fields: %w", err)
	}
	pretty = append(pretty, '\n')

	if filePath != "" {
		if err := os.WriteFile(filePath, pretty, 0644); err != nil {
			return fmt.Errorf("writing file: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Wrote %s fields to %s\n", id, filePath)
		return nil
	}

	fmt.Print(string(pretty))
	return nil
}

var formulaDumpCmd = &cobra.Command{
	Use:   "dump <formula-id>",
	Short: "Dump formula definition as JSON",
	Long: `Dump the vars and steps of a formula as JSON.

Output goes to stdout by default; use --file to write to a file.
Round-trip with 'kd formula update' to edit a formula:

  kd formula dump ka-abc123 > formula.json
  $EDITOR formula.json
  kd formula update ka-abc123 --file formula.json`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		filePath, _ := cmd.Flags().GetString("file")
		return runFormulaDump(args[0], filePath)
	},
}

func init() {
	formulaDumpCmd.Flags().StringP("file", "f", "", "write output to file instead of stdout")
}
