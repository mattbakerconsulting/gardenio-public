package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// cobraBuiltinGroups are the parent commands cobra GENERATES for us. They are
// not part of the kd surface we own, so the no-silent-exit-0 contract below
// does not apply to them and the recursive walk skips them (and their subtree):
//
//   - completion: cobra's `completion {bash,zsh,fish,powershell}` group. Its
//     parent is non-runnable by cobra's design and we do not redefine it.
//   - help: cobra's `help` command (a leaf, listed here defensively).
var cobraBuiltinGroups = map[string]bool{
	"completion": true,
	"help":       true,
}

// collectGroups walks the command tree rooted at cmd and returns every
// NON-LEAF GROUP — a command that HasSubCommands() — excluding the cobra
// built-in groups above (and their subtrees). These are exactly the commands
// that must reject an unknown subcommand instead of silently exiting 0.
//
// A LEAF like `kd mention` (an alias for `kd refs mentions <bead-id>` with a
// real RunE and ExactArgs(1), no subcommands) is intentionally NOT collected:
// it has no subcommands, so HasSubCommands() is false. Marking it NoArgs would
// wrongly reject its `<bead-id>` positional.
func collectGroups(cmd *cobra.Command) []*cobra.Command {
	var groups []*cobra.Command
	for _, sub := range cmd.Commands() {
		if cobraBuiltinGroups[sub.Name()] {
			continue // skip cobra-generated group + its subtree
		}
		if sub.HasSubCommands() {
			groups = append(groups, sub)
			groups = append(groups, collectGroups(sub)...)
		}
	}
	return groups
}

// TestAllGroups_RejectUnknownSubcommand is the permanent regression lock for
// ka-0000000976 (the sweep) and ka-0000002832 (the original `kd dep` fix). It
// walks the REAL rootCmd tree and asserts that EVERY non-leaf group rejects a
// bogus subcommand with a non-zero error — not a silent help dump + exit 0.
//
// Why this catches future regressions: any newly-added subcommands-only parent
// that is not passed through markAsGroup() (group.go) will appear in
// collectGroups(rootCmd), be Runnable()==false / accept a stray arg, and fail
// the assertions below. So the test fails the moment a new group reintroduces
// the bug, in CI rather than in production.
//
// Root cause recap (see group.go for the long form): cobra's execute() returns
// flag.ErrHelp (→ help print + exit 0) for any non-runnable command BEFORE the
// Args validator runs. The fix makes each group Runnable (RunE→Help) so
// cobra.NoArgs is reached and rejects the unknown arg. The two halves the test
// asserts per group — Runnable() AND ValidateArgs(["bogus"]) errors — are
// jointly equivalent to "exec rejects the bogus subcommand", because cobra runs
// ValidateArgs immediately after the `!Runnable()` short-circuit and before any
// PersistentPreRunE. That ordering also makes the end-to-end exec assertion
// hermetic: ValidateArgs fails first, so no client/credstore/network setup runs.
func TestAllGroups_RejectUnknownSubcommand(t *testing.T) {
	groups := collectGroups(rootCmd)

	// Sanity floor: the kd CLI has many groups (comment, formula, label, refs,
	// config, remote, admin, embed, jack, mol, audit, fleet, bridge, events,
	// advice, dep + nested admin migrate/backfill, bridge token, fleet token).
	// If the walk collapses to a handful, the recursion or wiring broke and the
	// per-group assertions would vacuously pass — fail loudly instead.
	if len(groups) < 15 {
		t.Fatalf("collectGroups(rootCmd) found only %d groups; expected >=15 — recursion or command wiring is broken", len(groups))
	}

	for _, g := range groups {
		path := g.CommandPath()
		t.Run(path, func(t *testing.T) {
			// 1. The load-bearing invariant: the group MUST be runnable, else
			//    cobra short-circuits to flag.ErrHelp/exit-0 and NoArgs never
			//    runs — the silent-exit-0 bug.
			if !g.Runnable() {
				t.Fatalf("group %q is not Runnable (no Run/RunE) — cobra will exit 0 on an unknown subcommand; markAsGroup() was not applied", path)
			}

			// 2. A stray/unknown subcommand (arrives as a positional arg once
			//    cobra's Find fails to match a child) must be REJECTED. NoArgs
			//    returns `unknown command %q for %q`.
			err := g.ValidateArgs([]string{"__bogus__"})
			if err == nil {
				t.Fatalf("group %q accepts a stray arg (ValidateArgs([\"__bogus__\"]) = nil) — `%s __bogus__` would silently exit 0; want cobra.NoArgs rejection", path, path)
			}
			if !strings.Contains(err.Error(), "unknown command") {
				t.Errorf("group %q ValidateArgs error = %q; expected it to mention \"unknown command\" (cobra.NoArgs)", path, err.Error())
			}

			// 3. A bare `<group>` (no args) must still PASS validation and fall
			//    through to RunE → Help (exit 0) — group help is preserved.
			if err := g.ValidateArgs(nil); err != nil {
				t.Errorf("group %q ValidateArgs(nil) = %v; expected nil (bare `%s` should show help, not error)", path, err, path)
			}
		})
	}
}

// TestAllTopLevelGroups_ExecRejectsUnknownSubcommand proves the contract on the
// real end-to-end execute path (not just ValidateArgs in isolation): dispatching
// `kd <group> __bogus__` through rootCmd.Execute returns a non-nil error whose
// message names the unknown command. This is the user-visible "nonzero exit +
// stderr" behavior from the acceptance criteria.
//
// Hermetic by construction: cobra runs ParseFlags + ValidateArgs BEFORE any
// PersistentPreRunE, so the NoArgs rejection fires before the root hook would
// build a client — no daemon, token, or credstore is touched. Output is
// captured to a buffer and the RETURNED error is asserted (independent of
// SilenceErrors), and SetArgs is reset on cleanup so no state leaks to sibling
// tests in the package.
func TestAllTopLevelGroups_ExecRejectsUnknownSubcommand(t *testing.T) {
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	// Restore the root's out/err writers after the test.
	savedOut, savedErr := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr()
	t.Cleanup(func() {
		rootCmd.SetOut(savedOut)
		rootCmd.SetErr(savedErr)
	})

	// Only the TOP-LEVEL groups are exercised via Execute (nested groups like
	// `admin migrate` are covered structurally by the test above; reaching them
	// through Execute would add no signal). Derive them from the tree so a new
	// top-level group is automatically included.
	for _, g := range rootCmd.Commands() {
		if cobraBuiltinGroups[g.Name()] || !g.HasSubCommands() {
			continue
		}
		name := g.Name()
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			rootCmd.SetOut(&buf)
			rootCmd.SetErr(&buf)
			rootCmd.SetArgs([]string{name, "__bogus__"})

			err := rootCmd.Execute()
			if err == nil {
				t.Fatalf("`kd %s __bogus__` returned nil error (silent exit-0 regression); expected a non-zero error", name)
			}
			if !strings.Contains(err.Error(), "unknown command") {
				t.Errorf("`kd %s __bogus__` error = %q; expected it to mention \"unknown command\"", name, err.Error())
			}
		})
	}
}

// TestAllTopLevelGroups_BareExecShowsHelp asserts the complementary half of the
// contract through the REAL execute path: a bare `kd <group>` (no args) must
// still PASS NoArgs, fall through to RunE → Help(), and return nil (exit 0) —
// group help is preserved after the runnable change.
//
// This is also the only test that exercises, end-to-end, the side effect of a
// group becoming runnable: a bare invocation now runs the root PersistentPreRunE
// (lazy client/credstore setup) before Help(). The root hook builds only a lazy
// client (no eager dial), so this stays offline-safe; the assertion guards
// against a future change making bare-group help error.
func TestAllTopLevelGroups_BareExecShowsHelp(t *testing.T) {
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	savedOut, savedErr := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr()
	t.Cleanup(func() {
		rootCmd.SetOut(savedOut)
		rootCmd.SetErr(savedErr)
	})
	// PersistentPreRunE sets the package-global beadsClient as a side effect;
	// save/restore so this test does not leak a client into sibling tests.
	savedClient := beadsClient
	t.Cleanup(func() { beadsClient = savedClient })

	for _, g := range rootCmd.Commands() {
		if cobraBuiltinGroups[g.Name()] || !g.HasSubCommands() {
			continue
		}
		name := g.Name()
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			rootCmd.SetOut(&buf)
			rootCmd.SetErr(&buf)
			rootCmd.SetArgs([]string{name})

			if err := rootCmd.Execute(); err != nil {
				t.Fatalf("bare `kd %s` returned %v; expected nil (group help, exit 0)", name, err)
			}
			if !strings.Contains(buf.String(), "Usage:") {
				t.Errorf("bare `kd %s` output did not contain a usage/help block:\n%s", name, buf.String())
			}
		})
	}
}

// TestMentionIsLeafNotGroup pins the one deliberate exclusion: `kd mention` is a
// runnable LEAF (alias for `kd refs mentions <bead-id>`), NOT a group. If a
// future change accidentally gives it subcommands or strips its RunE, this
// fails — guarding against either (a) it being swept into markAsGroup and
// breaking `kd mention gd-X`, or (b) it silently regressing.
func TestMentionIsLeafNotGroup(t *testing.T) {
	if mentionCmd.HasSubCommands() {
		t.Error("mentionCmd unexpectedly has subcommands — it is meant to be a leaf alias for `kd refs mentions <bead-id>`")
	}
	if !mentionCmd.Runnable() {
		t.Error("mentionCmd is not Runnable — the `kd mention <bead-id>` leaf lost its RunE")
	}
	// It must ACCEPT a single positional bead-id (ExactArgs(1)), the opposite of
	// a NoArgs group — proving it must never be marked as a group.
	if err := mentionCmd.ValidateArgs([]string{"gd-X"}); err != nil {
		t.Errorf("mentionCmd.ValidateArgs([\"gd-X\"]) = %v; expected nil — `kd mention <bead-id>` must accept its id arg", err)
	}
}
