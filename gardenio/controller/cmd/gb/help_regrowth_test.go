package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestRemediatedHelpBlocksStayTrim is the durable regression guard for the
// Opus/Fable-5 tool-description audit findings F2 (`gb escalation create`) and
// F3 (`gb decision create`) — bundle gd-FZZF0RWbw5, remediated under task
// gd-kK2DSmhGrG. Those two `Long` help strings had inlined a full runbook /
// multiple worked-example invocations that ALREADY lived in a reference doc or
// were carried by the flags themselves. The remediation cut each to a short
// "why + the one gotcha + a pointer" block. Per Anthropic's Rule 6 (rich
// references over inlined specs) and Rule 2 (design interfaces, not examples),
// that content belongs behind the pointer, not re-pasted into `--help`.
//
// The failure this guards against: a future edit silently re-inflates the Long
// back toward its pre-remediation size (re-pasting the runbook / worked
// examples), recreating the two-copies-of-one-spec drift the audit removed. A
// pure byte ceiling catches the re-inline; the mustContain pointer assertion
// catches the subtler regression where someone trims the pointer away while
// growing the prose, which would sever the agent from the on-demand detail.
//
// The ceilings sit ~1.5–2× current size (escalation 878 B → 1800; decision
// 1341 B → 2000) — generous headroom for a genuine clarification, but far below
// the pre-remediation blocks (escalation 5480 B; decision's two worked-example
// invocations plus the RunE re-inlines) so a spec re-paste trips it. A
// deliberate expansion re-baselines HERE, in the same MR, exactly as the
// snapshot guards force a deliberate re-baseline (ka-0000006923). The kd side
// (`kd formula create`, finding F1) is guarded by the sibling test in
// kbeads/cmd/kd/help_regrowth_test.go.
func TestRemediatedHelpBlocksStayTrim(t *testing.T) {
	// path is the space-joined command path; maxLongBytes is the ceiling on the
	// rendered Long string; mustContain are substrings the pointer/gotcha must
	// keep so a trim cannot sever the agent from the referenced detail.
	cases := []struct {
		path         string
		maxLongBytes int
		mustContain  []string
	}{
		{
			// F2: was a 5480 B / 107-line runbook copy; now a why + the two
			// non-obvious flag gotchas + a pointer at the advice bead that holds
			// the capability check and worked examples.
			path:         "gb escalation create",
			maxLongBytes: 1800,
			mustContain: []string{
				"gb decision create", // the decision-vs-escalation fork (the keeper judgment cue)
				"ka-0000001089",      // the on-demand reference pointer
			},
		},
		{
			// F3: was two full worked invocations in Long plus a third/fourth
			// re-inlined in RunE error strings; now one teaching example + the
			// interface (option format string) + a pointer at the --options flag
			// help for the JSON form.
			path:         "gb decision create",
			maxLongBytes: 2000,
			mustContain: []string{
				`"id:label:artifact_type"`, // the self-describing interface format
				"--options",                // pointer to the flag help that carries the JSON form
			},
		},
	}

	for _, tc := range cases {
		cmd := findCommandByPath(rootCmd, tc.path)
		if cmd == nil {
			t.Errorf("command %q not found in the gb command tree — was it renamed? update this guard (finding F2/F3, bundle gd-FZZF0RWbw5)", tc.path)
			continue
		}
		if n := len(cmd.Long); n > tc.maxLongBytes {
			t.Errorf("`%s` Long is %d bytes > %d ceiling — a tool-description trim (finding F2/F3, bundle gd-FZZF0RWbw5) is regrowing. The runbook / worked examples belong behind the pointer (Rule 6/Rule 2), not re-inlined into --help. Trim it back, or if the expansion is deliberate, raise the ceiling in this guard in the same MR.",
				tc.path, n, tc.maxLongBytes)
		}
		for _, want := range tc.mustContain {
			if !strings.Contains(cmd.Long, want) {
				t.Errorf("`%s` Long no longer contains %q — a trim severed the reference pointer / interface cue the remediation kept (finding F2/F3, bundle gd-FZZF0RWbw5). Keep the pointer so the agent can reach the on-demand detail.",
					tc.path, want)
			}
		}
	}
}

// findCommandByPath resolves a space-joined command path (e.g. "gb escalation
// create") to its *cobra.Command, or nil if any segment is missing. The first
// segment must match the root command's name.
func findCommandByPath(root *cobra.Command, path string) *cobra.Command {
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
