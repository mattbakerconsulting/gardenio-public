package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

var batchCmd = &cobra.Command{
	Use:   "batch <subcommand>...",
	Short: "Execute multiple kd subcommands in a single invocation",
	Long: `Execute multiple kd subcommands in a single CLI call to reduce tool call overhead.

Each argument is a quoted subcommand string. Supported subcommands:
  show <id>           — Show bead details
  dep list <id>       — List bead dependencies

Examples:
  kd batch "show ka-ABC" "show ka-DEF"
  kd batch "show ka-ABC" "dep list ka-DEF"`,
	GroupID: "beads",
	Args:    cobra.MinimumNArgs(1),
	RunE:    runBatch,
}

// batchOp represents a parsed batch operation.
type batchOp struct {
	raw    string   // original argument string
	cmd    string   // resolved command name (e.g. "show", "dep list")
	args   []string // remaining args after command
	result string   // formatted output
	err    error
}

// parseBatchOp parses a subcommand string into a batchOp.
func parseBatchOp(raw string) (*batchOp, error) {
	parts := strings.Fields(raw)
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty subcommand")
	}

	op := &batchOp{raw: raw}

	switch parts[0] {
	case "show":
		if len(parts) != 2 {
			return nil, fmt.Errorf("show requires exactly 1 argument (bead ID)")
		}
		op.cmd = "show"
		op.args = parts[1:]
	case "dep":
		if len(parts) < 2 {
			return nil, fmt.Errorf("dep requires a subcommand (e.g. 'dep list <id>')")
		}
		switch parts[1] {
		case "list":
			if len(parts) != 3 {
				return nil, fmt.Errorf("dep list requires exactly 1 argument (bead ID)")
			}
			op.cmd = "dep list"
			op.args = parts[2:]
		default:
			return nil, fmt.Errorf("unsupported dep subcommand: %s", parts[1])
		}
	default:
		return nil, fmt.Errorf("unsupported batch subcommand: %s", parts[0])
	}

	return op, nil
}

func runBatch(cmd *cobra.Command, args []string) error {
	ops := make([]*batchOp, 0, len(args))
	for _, arg := range args {
		op, err := parseBatchOp(arg)
		if err != nil {
			return fmt.Errorf("parsing %q: %w", arg, err)
		}
		ops = append(ops, op)
	}

	ctx := context.Background()

	// Optimization: if all ops are "show", use the batch endpoint for a single
	// HTTP request instead of N concurrent ones.
	allShow := true
	for _, op := range ops {
		if op.cmd != "show" {
			allShow = false
			break
		}
	}

	if allShow {
		ids := make([]string, len(ops))
		for i, op := range ops {
			ids[i] = op.args[0]
		}
		results, err := beadsClient.BatchGetBeads(ctx, ids)
		if err == nil {
			for i, r := range results {
				if r.Error != "" {
					ops[i].err = fmt.Errorf("%s: %s", r.ID, r.Error)
				} else if r.Bead == nil {
					ops[i].err = fmt.Errorf("%s: not found", r.ID)
				} else {
					ops[i].result = formatBeadCompact(r.Bead)
				}
			}
		} else {
			// Fall back to concurrent individual requests.
			runBatchConcurrent(ctx, ops)
		}
	} else {
		runBatchConcurrent(ctx, ops)
	}

	// Print results in original order.
	for i, op := range ops {
		if i > 0 {
			fmt.Println()
		}
		fmt.Fprintf(os.Stdout, "=== %s ===\n", op.raw)
		if op.err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", op.err)
		} else {
			fmt.Print(op.result)
		}
	}

	return nil
}

// runBatchConcurrent executes ops concurrently with individual requests.
func runBatchConcurrent(ctx context.Context, ops []*batchOp) {
	var wg sync.WaitGroup
	for _, op := range ops {
		wg.Add(1)
		go func(op *batchOp) {
			defer wg.Done()
			executeBatchOp(ctx, op)
		}(op)
	}
	wg.Wait()
}

func executeBatchOp(ctx context.Context, op *batchOp) {
	switch op.cmd {
	case "show":
		id := op.args[0]
		bead, err := beadsClient.GetBead(ctx, id)
		if err != nil {
			op.err = fmt.Errorf("getting bead %s: %w", id, err)
			return
		}
		op.result = formatBeadCompact(bead)

	case "dep list":
		id := op.args[0]
		deps, err := beadsClient.GetDependencies(ctx, id)
		if err != nil {
			op.err = fmt.Errorf("listing deps for %s: %w", id, err)
			return
		}
		var buf strings.Builder
		if len(deps) == 0 {
			fmt.Fprintln(&buf, "No dependencies found.")
		} else {
			for _, d := range deps {
				fmt.Fprintf(&buf, "  %s  %s  %s\n", d.DependsOnID, d.Type, d.CreatedBy)
			}
		}
		op.result = buf.String()
	}
}

// formatBeadCompact produces a compact bead summary for batch output.
func formatBeadCompact(bead *model.Bead) string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "ID:          %s\n", bead.ID)
	fmt.Fprintf(&buf, "Title:       %s\n", bead.Title)
	fmt.Fprintf(&buf, "Type:        %s\n", bead.Type)
	fmt.Fprintf(&buf, "Status:      %s\n", bead.Status)
	fmt.Fprintf(&buf, "Priority:    %d\n", bead.Priority)
	fmt.Fprintf(&buf, "Assignee:    %s\n", bead.Assignee)
	if bead.Description != "" {
		fmt.Fprintf(&buf, "Description: %s\n", truncate(bead.Description, 200))
	}
	if len(bead.Labels) > 0 {
		fmt.Fprintf(&buf, "Labels:      %s\n", strings.Join(bead.Labels, ", "))
	}
	if len(bead.Dependencies) > 0 {
		fmt.Fprintln(&buf, "Depends On:")
		for _, d := range bead.Dependencies {
			fmt.Fprintf(&buf, "  %s (%s)\n", d.DependsOnID, d.Type)
		}
	}
	return buf.String()
}

func init() {
	rootCmd.AddCommand(batchCmd)
}
