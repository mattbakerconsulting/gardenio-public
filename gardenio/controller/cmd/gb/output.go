package main

import (
	"encoding/json"
	"fmt"
	"os"

	"gardenio/controller/internal/jqout"
)

// compiledJQ holds the parsed --jq filter, compiled once in the root
// PersistentPreRunE. nil when --jq was not passed. printJSON consults it.
var compiledJQ *jqout.Filter

// printJSON is the single choke-point for gb's JSON output. When a --jq filter
// is set it projects v in-process (jq -r semantics); otherwise it marshals
// indented JSON exactly as before. Routing all `gb ... --json` printers through
// here is what makes `--jq` universal (ready/news/decision/...) with no
// per-command plumbing.
func printJSON(v any) {
	if compiledJQ != nil {
		if err := compiledJQ.Apply(os.Stdout, v); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
		return
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling JSON: %v\n", err)
		return
	}
	fmt.Println(string(data))
}
