package main

import "github.com/spf13/cobra"

// markAsGroup makes a subcommands-only parent command ("group") reject unknown
// subcommands with a non-zero exit instead of silently dumping help and exiting
// 0 — the silent-exit-0 bug fixed for `kd dep` in ka-0000002832 and swept across
// every other group in ka-0000000976.
//
// Root cause it cures: cobra's Command.execute() short-circuits to flag.ErrHelp
// for any *non-runnable* command (no Run/RunE) BEFORE it ever reaches the Args
// validator (cobra command.go: `if !c.Runnable() { return flag.ErrHelp }`).
// flag.ErrHelp is turned into a help-print on STDOUT + exit 0 by ExecuteC. So a
// group whose only job is to host subcommands — `kd formula`, `kd label`,
// `kd refs`, … — swallows a stray/unknown subcommand (`kd label bogus`)
// silently: help to stdout, exit 0, no error. A `|| fallback` downstream never
// fires; an operator typo looks like success.
//
// The fix is mechanical and identical for every group: give the parent a RunE
// (so it becomes Runnable and the `!Runnable()` short-circuit is skipped) and a
// cobra.NoArgs validator (so an UNKNOWN subcommand — which arrives as a stray
// positional arg, because cobra's Find returns the parent when no child matches
// — is rejected with the standard `unknown command %q for %q` error, exit 1,
// matching how the ROOT command already rejects unknown commands). KNOWN
// subcommands match first in cobra's Find and run their own RunE/Args, so they
// never reach the parent's NoArgs. A bare `kd <group>` (no args) passes NoArgs
// and falls through to RunE → Help() → exit 0, preserving group-help behavior.
//
// Side effect of becoming runnable (same tradeoff accepted for `kd dep`): a
// bare `kd <group>` now runs the root PersistentPreRunE (token/credstore/lazy
// client setup) before printing help, like every leaf command — a non-runnable
// group skipped it. The root hook builds only a lazy client (no eager dial), so
// bare-group help still works offline; only an invalid global flag now errors
// here instead of being silently ignored.
//
// Use this helper for every NEW group too: a subcommands-only parent must be
// passed through markAsGroup, or it reintroduces the silent-exit-0 bug. The
// recursive regression test in group_noargs_test.go walks the whole command
// tree and fails if any non-leaf group is left non-runnable / accepts a bogus
// subcommand, so a forgotten group is caught in CI rather than in production.
//
// NOTE: `kd dep` keeps its own inline RunE+NoArgs (defined and documented in
// dep.go, shipped in !4895) and is intentionally NOT in the registry below — it
// already satisfies the same contract and the recursive test verifies it too.
// `kd comment` likewise keeps its own inline RunE+Args (comment.go) — its
// default form delegates the bare `kd comment <id> <text>` to `add` while still
// rejecting `kd comment bogus` as an unknown subcommand (ka-0000006021), so it
// too satisfies the contract without markAsGroup (which would overwrite that
// smarter default with NoArgs+Help). `kd mention` is a runnable LEAF (an alias
// for `kd refs mentions <bead-id>` with a real RunE and ExactArgs(1), no
// subcommands), so it must NOT be marked as a group — NoArgs would break
// `kd mention ka-X`.
func markAsGroup(cmd *cobra.Command) {
	cmd.Args = cobra.NoArgs
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		return c.Help()
	}
}

// init applies markAsGroup to every subcommands-only parent ("group") command
// in the kd CLI. This is the single point that enforces the no-silent-exit-0
// contract; the recursive test independently verifies completeness, so a new
// group missing from this list fails CI.
//
// markAsGroup only mutates the parent's RunE/Args — it does not depend on the
// parent's children being wired yet — so this init is order-independent with
// respect to the per-file init()s that call AddCommand.
func init() {
	groups := []*cobra.Command{
		// Top-level groups.
		adminCmd,  // kd admin
		adviceCmd, // kd advice   (deprecated → gb advice)
		auditCmd,  // kd audit
		bridgeCmd, // kd bridge
		// NOTE: commentCmd is intentionally NOT here. Like `kd dep`, it keeps its
		// own inline RunE+Args (comment.go) so the bare `kd comment <id> <text>`
		// form delegates to `add` while `kd comment bogus` is still rejected as an
		// unknown subcommand. markAsGroup would overwrite that with NoArgs+Help.
		configCmd,     // kd config   (deprecated → config beads)
		embedCmd,      // kd embed
		escalationCmd, // kd escalation
		eventsCmd,     // kd events
		fleetCmd,      // kd fleet
		formulaCmd,    // kd formula
		heraldryCmd,   // kd heraldry
		jackCmd,       // kd jack
		labelCmd,      // kd label
		molCmd,        // kd mol
		refsCmd,       // kd refs
		releaseCmd,    // kd release
		remoteCmd,     // kd remote   (has its own PersistentPreRunE; RunE still wins)
		scrubCmd,      // kd scrub
		taintCmd,      // kd taint
		teamCmd,       // kd team

		// Nested groups (subcommands-only parents under a top-level group).
		adminMigrateCmd,  // kd admin migrate
		adminBackfillCmd, // kd admin backfill
		bridgeTokenCmd,   // kd bridge token
		fleetTokenCmd,    // kd fleet token (deprecated → kd bridge token)
	}
	for _, g := range groups {
		markAsGroup(g)
	}
}
