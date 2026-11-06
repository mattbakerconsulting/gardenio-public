package main

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// TestWarnOnPipeLabels is the unit-level contract for the pipe-OR footgun
// guard (ka-0000002308): a label value containing "|" emits exactly one
// warning that names the flag, echoes the offending value, states "|" is not
// an OR operator, and suggests the copy-pasteable --label-any comma form.
// Values without a pipe stay silent, and the whole thing writes to the
// provided io.Writer without touching exit semantics.
func TestWarnOnPipeLabels(t *testing.T) {
	cases := []struct {
		name     string
		flag     string
		values   []string
		wantWarn bool
		// substrings every emitted warning line must contain (only checked
		// when wantWarn is true)
		wantSubstrings []string
	}{
		{
			name:           "pipe value warns and suggests label-any",
			flag:           "-l/--label",
			values:         []string{"a|b|c"},
			wantWarn:       true,
			wantSubstrings: []string{`"a|b|c"`, "not an OR operator", "--label-any a,b,c"},
		},
		{
			name:           "pipe in one of several values",
			flag:           "-l/--label",
			values:         []string{"project:gardenio", "preflight-broken|release-stuck"},
			wantWarn:       true,
			wantSubstrings: []string{"-l/--label", `"preflight-broken|release-stuck"`, "--label-any preflight-broken,release-stuck"},
		},
		{
			name:           "flag name is echoed for exclude-label",
			flag:           "--exclude-label",
			values:         []string{"x|y"},
			wantWarn:       true,
			wantSubstrings: []string{"--exclude-label", "--label-any x,y"},
		},
		{name: "single plain label is silent", flag: "-l/--label", values: []string{"project:gardenio"}, wantWarn: false},
		{name: "comma-AND value is silent", flag: "-l/--label", values: []string{"a,b"}, wantWarn: false},
		{name: "repeated labels are silent", flag: "-l/--label", values: []string{"a", "b"}, wantWarn: false},
		{name: "empty is silent", flag: "-l/--label", values: nil, wantWarn: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			warnOnPipeLabels(&buf, tc.flag, tc.values)
			got := buf.String()

			if tc.wantWarn && got == "" {
				t.Fatalf("expected a warning for %v, got none", tc.values)
			}
			if !tc.wantWarn && got != "" {
				t.Fatalf("expected no warning for %v, got %q", tc.values, got)
			}
			for _, sub := range tc.wantSubstrings {
				if !strings.Contains(got, sub) {
					t.Errorf("warning %q missing substring %q", got, sub)
				}
			}
		})
	}
}

// TestWarnOnPipeLabels_OneWarningPerPipeValue confirms N pipe values produce
// exactly N warning lines — not one merged line, not one per pipe character.
func TestWarnOnPipeLabels_OneWarningPerPipeValue(t *testing.T) {
	var buf strings.Builder
	warnOnPipeLabels(&buf, "-l/--label", []string{"a|b", "plain", "c|d|e"})
	if n := strings.Count(buf.String(), "\n"); n != 2 {
		t.Errorf("expected 2 warning lines, got %d: %q", n, buf.String())
	}
}

// TestList_PipeLabelWarning pins the end-to-end CLI wiring on `kd list`: a
// `-l "x|y"` value prints the pipe warning to stderr while the list still runs
// (a ListBeads call is issued — exit semantics and result stream unchanged),
// and a normal `-l x` value prints nothing.
func TestList_PipeLabelWarning(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantWarn bool
	}{
		{"pipe value warns", []string{"-l", "x|y"}, true},
		{"plain value silent", []string{"-l", "x"}, false},
		{"label-any pipe warns", []string{"--label-any", "x|y"}, true},
		{"exclude-label pipe warns", []string{"--exclude-label", "x|y"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMockClient()
			withMockClient(t, mc)
			withStdoutTTY(t, false) // suppress the operational nudge (single call)

			resetPipeLabelFlags(t)
			if err := listCmd.Flags().Parse(tc.args); err != nil {
				t.Fatalf("Parse(%v): %v", tc.args, err)
			}

			stderr := captureStderr(t, func() {
				captureStdout(t, func() {
					if err := listCmd.RunE(listCmd, nil); err != nil {
						t.Fatalf("RunE: %v", err)
					}
				})
			})

			// The list must still run regardless of the warning — the guard
			// never aborts (scripts keep their exit code and result stream).
			if len(mc.ListBeadsCalls) != 1 {
				t.Fatalf("expected 1 ListBeads call (warning must not abort), got %d", len(mc.ListBeadsCalls))
			}

			gotWarn := strings.Contains(stderr, "not an OR operator")
			if gotWarn != tc.wantWarn {
				t.Errorf("stderr warning = %v, want %v; stderr=%q", gotWarn, tc.wantWarn, stderr)
			}
			if tc.wantWarn && !strings.Contains(stderr, "--label-any x,y") {
				t.Errorf("warning must suggest the comma form; stderr=%q", stderr)
			}
		})
	}
}

// resetPipeLabelFlags clears the three label StringSlice flags this test
// mutates so a prior subtest's value never leaks (StringSlice accumulates and
// carries the Changed bit across RunE calls; f.Value.Set(DefValue) does NOT
// reset a slice — mirror resetExcludeLabelFlag and Replace the slice).
func resetPipeLabelFlags(t *testing.T) {
	t.Helper()
	listCmd.Flags().VisitAll(func(f *pflag.Flag) {
		switch f.Name {
		case "label", "label-any", "exclude-label":
			f.Changed = false
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace([]string{})
			}
		}
	})
}
