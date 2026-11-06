package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// funcPtr returns the code pointer of a function value, for identity
// comparison (func values are not directly comparable with ==).
func funcPtr(fn any) uintptr {
	return reflect.ValueOf(fn).Pointer()
}

// TestDepGroup_RejectsUnknownSubcommand is the regression lock for
// ka-0000005587 / ka-0000002832: `kd dep <unknown>` (e.g. `kd dep tree X`
// before the pass-through existed, or `kd dep bogus X`) must FAIL with a
// non-zero exit, not silently dump help to stdout and exit 0.
//
// Root cause it guards against: cobra's execute() returns flag.ErrHelp (which
// ExecuteC turns into a help-print + exit 0) for any *non-runnable* command
// BEFORE it ever runs the Args validator. So a parent group with subcommands
// only — no RunE — swallows a stray subcommand silently. The fix makes depCmd
// runnable (RunE) so cobra.NoArgs is reached and rejects the unknown arg.
//
// The two halves below assert the contract on the REAL depCmd object plus an
// end-to-end execute-path proof (with a negative control reproducing the bug).
func TestDepGroup_RejectsUnknownSubcommand(t *testing.T) {
	t.Run("real_depCmd_is_runnable_and_validates_args", func(t *testing.T) {
		// The load-bearing invariant: depCmd MUST be runnable, otherwise cobra
		// short-circuits to ErrHelp/exit-0 and NoArgs never runs.
		if !depCmd.Runnable() {
			t.Fatal("depCmd is not Runnable (no Run/RunE) — cobra will exit 0 on unknown subcommands; the silent-exit-0 bug is back")
		}
		// A stray non-subcommand arg must be rejected by the Args validator.
		if err := depCmd.ValidateArgs([]string{"bogus"}); err == nil {
			t.Error("depCmd.ValidateArgs([\"bogus\"]) = nil; expected an error (cobra.NoArgs should reject stray args)")
		}
		// A bare `kd dep` (no args) must pass validation and fall through to
		// RunE (which prints help) — exit 0, unchanged group-help behavior.
		if err := depCmd.ValidateArgs(nil); err != nil {
			t.Errorf("depCmd.ValidateArgs(nil) = %v; expected nil (bare `kd dep` should show help, not error)", err)
		}
	})

	t.Run("pass_throughs_resolve_to_canonical_runE", func(t *testing.T) {
		// `kd dep tree` / `kd dep children` must resolve to real subcommands
		// that reuse the SAME run logic as the top-level `kd tree`/`kd children`
		// — so the agent's natural `kd dep tree <epic>` works and can never
		// drift from the canonical command.
		for _, tc := range []struct {
			sub  string
			want uintptr
		}{
			{"tree", funcPtr(runTreeCmd)},
			{"children", funcPtr(runChildrenCmd)},
		} {
			cmd, _, err := depCmd.Find([]string{tc.sub, "gd-X"})
			if err != nil {
				t.Fatalf("depCmd.Find([%q,...]) error: %v", tc.sub, err)
			}
			if cmd == nil || cmd.Name() != tc.sub {
				t.Fatalf("`kd dep %s` resolved to %v, want a subcommand named %q", tc.sub, cmd, tc.sub)
			}
			if cmd.RunE == nil || funcPtr(cmd.RunE) != tc.want {
				t.Errorf("`kd dep %s` RunE is not the canonical run function — alias has drifted from `kd %s`", tc.sub, tc.sub)
			}
			// The pass-through must carry the same arg contract as the leaf.
			if err := cmd.ValidateArgs([]string{"gd-X", "extra"}); err == nil {
				t.Errorf("`kd dep %s` accepted 2 positional args; expected ExactArgs(1) rejection", tc.sub)
			}
		}
	})

	// End-to-end execute-path proof on a throwaway tree that mirrors the real
	// wiring (same pattern as silence_usage_test.go, which avoids dialing the
	// daemon via rootCmd). The negative control proves the fix is necessary:
	// the non-runnable variant reproduces the original silent-exit-0.
	t.Run("execute_path_fixed_vs_buggy", func(t *testing.T) {
		newRoot := func() *cobra.Command {
			r := &cobra.Command{Use: "kd", SilenceErrors: true, SilenceUsage: true}
			r.SetArgs(nil)
			return r
		}

		// FIXED group: runnable + NoArgs, mirroring the production depCmd.
		fixed := &cobra.Command{
			Use:  "dep",
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
		}
		fixed.AddCommand(&cobra.Command{
			Use: "add", Args: cobra.ExactArgs(2),
			RunE: func(*cobra.Command, []string) error { return nil },
		})

		// BUGGY group: the pre-fix shape — subcommands only, no RunE/Args.
		buggy := &cobra.Command{Use: "depbuggy"}
		buggy.AddCommand(&cobra.Command{
			Use: "add", Args: cobra.ExactArgs(2),
			RunE: func(*cobra.Command, []string) error { return nil },
		})

		// FIXED: unknown subcommand → non-nil error.
		{
			r := newRoot()
			r.AddCommand(fixed)
			var buf bytes.Buffer
			r.SetOut(&buf)
			r.SetErr(&buf)
			r.SetArgs([]string{"dep", "bogus", "gd-X"})
			err := r.Execute()
			if err == nil {
				t.Error("FIXED: `kd dep bogus gd-X` returned nil (silent exit-0 regression); expected an error")
			} else if !strings.Contains(err.Error(), "unknown command") {
				t.Errorf("FIXED: error = %q; expected it to mention \"unknown command\"", err.Error())
			}
		}

		// FIXED: bare group → help, no error (exit 0 preserved).
		{
			r := newRoot()
			r.AddCommand(fixed)
			var buf bytes.Buffer
			r.SetOut(&buf)
			r.SetErr(&buf)
			r.SetArgs([]string{"dep"})
			if err := r.Execute(); err != nil {
				t.Errorf("FIXED: bare `kd dep` returned %v; expected nil (help, exit 0)", err)
			}
		}

		// NEGATIVE CONTROL: assert the buggy non-runnable shape DOES reproduce
		// the silent-exit-0 (unknown subcommand swallowed → nil error). This is
		// what makes the `fixed` assertions above meaningful: it proves the bug
		// is real on the pinned cobra and that the RunE+NoArgs is what cures it.
		// If a future cobra bump changes non-runnable-parent handling so this no
		// longer returns nil, FAIL LOUDLY (not skip) — that shift is exactly the
		// event this suite must surface, since the whole fix rests on cobra's
		// `!Runnable() → flag.ErrHelp` ordering.
		{
			r := newRoot()
			r.AddCommand(buggy)
			var buf bytes.Buffer
			r.SetOut(&buf)
			r.SetErr(&buf)
			r.SetArgs([]string{"depbuggy", "bogus", "gd-X"})
			if err := r.Execute(); err != nil {
				t.Errorf("negative control: buggy non-runnable group returned %v for an unknown subcommand; "+
					"expected nil (silent exit-0). cobra's !Runnable()->flag.ErrHelp behavior may have changed — "+
					"re-validate that depCmd still needs a RunE to reach NoArgs", err)
			}
		}
	})
}
