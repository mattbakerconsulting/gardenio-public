package main

import (
	"testing"

	"github.com/spf13/pflag"
)

// TestList_ExcludeLabelFlag pins the end-to-end CLI wiring of
// `kd list --exclude-label`: the flag value must land in
// req.ExcludeLabels on the ListBeadsRequest the client receives, so the
// server applies its `label NOT IN (...)` filter. This is the missing link
// the fix wires up (ka-0000007103 / gd-ugmzbCTMCQ): the server + store +
// HTTP-client param were already plumbed; only the CLI read+append was absent.
func TestList_ExcludeLabelFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"unset", nil, nil},
		{"single", []string{"--exclude-label", "needs-intake"}, []string{"needs-intake"}},
		{"comma", []string{"--exclude-label", "needs-intake,release-regression"}, []string{"needs-intake", "release-regression"}},
		{"repeated", []string{"--exclude-label", "needs-intake", "--exclude-label", "release-regression"}, []string{"needs-intake", "release-regression"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMockClient()
			origClient := beadsClient
			beadsClient = mc
			defer func() { beadsClient = origClient }()

			resetExcludeLabelFlag(t)
			// Parse goes through the command's flag set so the alias
			// NormalizeFunc and StringSlice comma-splitting both fire, exactly
			// as a real `kd list ...` invocation would.
			if err := listCmd.Flags().Parse(tc.args); err != nil {
				t.Fatalf("Parse(%v): %v", tc.args, err)
			}

			captureStdout(t, func() {
				if err := listCmd.RunE(listCmd, nil); err != nil {
					t.Fatalf("RunE: %v", err)
				}
			})

			if len(mc.ListBeadsCalls) != 1 {
				t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
			}
			got := mc.ListBeadsCalls[0].ExcludeLabels
			if !sliceEq(got, tc.want) {
				t.Errorf("ExcludeLabels = %v, want %v", got, tc.want)
			}
		})
	}
}

// resetExcludeLabelFlag clears the --exclude-label StringSlice between subtests
// so a prior parse never leaks. StringSlice accumulates across Set calls and
// carries the Changed bit between RunE invocations within one process, and
// f.Value.Set(f.DefValue) does NOT reset a slice flag — so we Replace the
// underlying slice, mirroring resetListFieldFlag (gd-ugmzbCTMCQ).
func resetExcludeLabelFlag(t *testing.T) {
	t.Helper()
	listCmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "exclude-label" {
			f.Changed = false
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace([]string{})
			}
		}
	})
}
