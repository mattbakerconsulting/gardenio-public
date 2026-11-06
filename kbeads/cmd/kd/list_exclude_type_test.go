package main

import (
	"testing"

	"github.com/spf13/pflag"
)

// TestList_ExcludeTypeFlag pins the end-to-end CLI wiring of
// `kd list --exclude-type`: the flag value must land in req.ExcludeTypes on
// the ListBeadsRequest the client receives, so the server applies its
// `type NOT IN (...)` filter (ka-0000007843). This is the type analogue of
// TestList_ExcludeLabelFlag — the server + store + HTTP `exclude_type` param
// already existed (beads-viewer /active used them); only the CLI read+append
// and the client-request field were absent, which is what this fix wires up.
//
// Values are validated + trimmed through validateTypeFilter, so the recorded
// slice is the NORMALIZED value the server will match verbatim.
func TestList_ExcludeTypeFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"unset", nil, nil},
		{"single", []string{"--exclude-type", "message"}, []string{"message"}},
		{"comma", []string{"--exclude-type", "message,event"}, []string{"message", "event"}},
		{"repeated", []string{"--exclude-type", "message", "--exclude-type", "event"}, []string{"message", "event"}},
		// A comma split leaves a leading space on the second value
		// (`message, event` → `["message", " event"]`); validateTypeFilter must
		// trim it, or the server's verbatim type match would silently miss the
		// noise the flag exists to drop.
		{"trims", []string{"--exclude-type", "message, event"}, []string{"message", "event"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMockClient()
			origClient := beadsClient
			beadsClient = mc
			defer func() { beadsClient = origClient }()

			resetExcludeTypeFlag(t)
			// listCmd is package-global; a value left on --exclude-type would
			// leak into other tests sharing it (a poisoned value fails their
			// buildListRequest). Reset on the way out too.
			t.Cleanup(func() { resetExcludeTypeFlag(t) })
			// Parse goes through the command's flag set so the StringSlice
			// comma-splitting fires exactly as a real `kd list ...` would.
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
			got := mc.ListBeadsCalls[0].ExcludeTypes
			if !sliceEq(got, tc.want) {
				t.Errorf("ExcludeTypes = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestList_ExcludeTypeFlag_UnknownFailsLoud pins that an unknown --exclude-type
// value fails LOUD (like --type) instead of silently excluding nothing — an
// unrecognized negative filter that matches no rows would leave the noise it
// targets in the result, indistinguishable from the flag working.
func TestList_ExcludeTypeFlag_UnknownFailsLoud(t *testing.T) {
	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	resetExcludeTypeFlag(t)
	t.Cleanup(func() { resetExcludeTypeFlag(t) })
	if err := listCmd.Flags().Parse([]string{"--exclude-type", "zzznotatype"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var runErr error
	captureStdout(t, func() {
		runErr = listCmd.RunE(listCmd, nil)
	})
	if runErr == nil {
		t.Fatal("expected an error for unknown --exclude-type value, got nil")
	}
	if len(mc.ListBeadsCalls) != 0 {
		t.Errorf("expected no ListBeads call on validation failure, got %d", len(mc.ListBeadsCalls))
	}
}

// resetExcludeTypeFlag clears the --exclude-type StringSlice between subtests
// so a prior parse never leaks. StringSlice accumulates across Set calls and
// carries the Changed bit between RunE invocations within one process, and
// f.Value.Set(f.DefValue) does NOT reset a slice flag — so we Replace the
// underlying slice, mirroring resetExcludeLabelFlag (ka-0000007843).
func resetExcludeTypeFlag(t *testing.T) {
	t.Helper()
	listCmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "exclude-type" {
			f.Changed = false
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace([]string{})
			}
		}
	})
}
