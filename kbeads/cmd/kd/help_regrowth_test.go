package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestFormulaCreateHelpStaysTrim is the durable regression guard for finding F1
// of the Opus/Fable-5 tool-description audit (bundle gd-FZZF0RWbw5, remediated
// under task gd-kK2DSmhGrG). `kd formula create` had inlined the ENTIRE 138-line
// Formula DSL by hand — the single largest help block in either CLI at 6,673
// bytes — even though docs/formula-reference.md already documents the identical
// DSL and a global advice bead (ka-0000004524) already points agents at it. The
// remediation cut Long to a one-paragraph "what a formula is" + a tiny orienting
// JSON skeleton + a pointer to the reference doc, killing the drift between two
// hand-maintained copies of one spec (Rule 6: rich references over inlined
// specs; Rule 2: an orienting skeleton, not the full field enumeration).
//
// The failure this guards against: a future edit re-pastes the field-by-field
// DSL back into Long, recreating the wall of enumerated fields the blog says now
// HURTS Opus/Fable 5 (read on every --help) and re-forking the doc/help copies.
// A byte ceiling catches the re-inline; the mustContain assertion keeps the
// reference pointer alive so a trim cannot sever the agent from the full DSL.
//
// The ceiling sits ~2× the remediated size — headroom for a genuine
// clarification, far below the 6,673 B pre-remediation block so a spec re-paste
// trips it. A deliberate expansion re-baselines here in the same MR. The gb side
// (`gb escalation create` F2, `gb decision create` F3) is guarded by the sibling
// test in gardenio/controller/cmd/gb/help_regrowth_test.go.
func TestFormulaCreateHelpStaysTrim(t *testing.T) {
	const maxLongBytes = 2400

	cmd := findKdCommandByPath(rootCmd, "kd formula create")
	if cmd == nil {
		t.Fatal("command `kd formula create` not found in the kd command tree — was it renamed? update this guard (finding F1, bundle gd-FZZF0RWbw5)")
	}

	if n := len(cmd.Long); n > maxLongBytes {
		t.Errorf("`kd formula create` Long is %d bytes > %d ceiling — the Formula DSL is regrowing back into --help (finding F1, bundle gd-FZZF0RWbw5). The full field-by-field DSL belongs in docs/formula-reference.md (Rule 6), not re-inlined. Trim it back to a skeleton + pointer, or if the expansion is deliberate, raise the ceiling in this guard in the same MR.",
			n, maxLongBytes)
	}

	// The pointer to the reference doc must survive any trim, or the agent loses
	// the on-demand path to the full DSL.
	if !strings.Contains(cmd.Long, "docs/formula-reference.md") {
		t.Errorf("`kd formula create` Long no longer points at docs/formula-reference.md — a trim severed the reference pointer the remediation kept (finding F1, bundle gd-FZZF0RWbw5). Keep the pointer so the agent can reach the full DSL on demand.")
	}
}

// findKdCommandByPath resolves a space-joined command path (e.g. "kd formula
// create") to its *cobra.Command, or nil if any segment is missing. The first
// segment must match the root command's name.
func findKdCommandByPath(root *cobra.Command, path string) *cobra.Command {
	fields := strings.Fields(path)
	if len(fields) == 0 || fields[0] != root.Name() {
		return nil
	}
	cur := root
	for _, seg := range fields[1:] {
		var next *cobra.Command
		for _, sub := range cur.Commands() {
			if sub.Name() == seg {
				next = sub
				break
			}
		}
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur
}
