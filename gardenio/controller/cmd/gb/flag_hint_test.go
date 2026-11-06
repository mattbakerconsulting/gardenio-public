package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// newHintTestCmd builds a root+leaf pair mirroring the real `gb mr status`
// wiring: the FlagErrorFunc is set on the ROOT only, so these tests also prove
// cobra's inheritance is what carries the hint to the leaf.
func newHintTestCmd(leafUse string) (*cobra.Command, *cobra.Command) {
	root := &cobra.Command{Use: "gb"}
	root.SetFlagErrorFunc(flagErrorWithPositionalHint)
	mid := &cobra.Command{Use: "mr"}
	leaf := &cobra.Command{
		Use:  leafUse,
		Args: cobra.MaximumNArgs(1),
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	mid.AddCommand(leaf)
	root.AddCommand(mid)
	return root, leaf
}

// TestFlagErrorHint_UnknownFlagOnPositionalCommand is the regression guard for
// bug ka-0000006140: `gb mr status --bead <id>` used to fail with a bare
// "unknown flag: --bead" that never mentioned the positional contract.
func TestFlagErrorHint_UnknownFlagOnPositionalCommand(t *testing.T) {
	root, _ := newHintTestCmd("status [<bead-id-or-mr-url>]")
	root.SetArgs([]string{"mr", "status", "--nonesuch", "gd-abc"})
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected an unknown-flag error, got nil")
	}
	got := err.Error()

	// The original pflag message must survive — callers/greps depend on it.
	if !strings.Contains(got, "unknown flag: --nonesuch") {
		t.Errorf("original pflag message lost, got %q", got)
	}
	// ...and the hint must name the full positional invocation.
	for _, want := range []string{
		"hint:",
		"gb mr status",
		"--nonesuch",
		"<bead-id-or-mr-url>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hint missing %q, got %q", want, got)
		}
	}
}

// TestFlagErrorHint_NoPositionalNoHint: a command declaring no positional
// placeholder (e.g. `gb mr list`) must pass the error through untouched — a hint
// pointing at a nonexistent positional form would be worse than none.
func TestFlagErrorHint_NoPositionalNoHint(t *testing.T) {
	root, _ := newHintTestCmd("list")
	root.SetArgs([]string{"mr", "list", "--nonesuch"})
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected an unknown-flag error, got nil")
	}
	if strings.Contains(err.Error(), "hint:") {
		t.Errorf("no-positional command should get no hint, got %q", err.Error())
	}
}

// TestFlagErrorHint_NonUnknownFlagErrorsPassThrough: only unknown-flag errors
// get the positional hint. A missing flag VALUE is a different mistake and the
// positional form is not the fix, so it must pass through unchanged.
func TestFlagErrorHint_NonUnknownFlagErrorsPassThrough(t *testing.T) {
	root := &cobra.Command{Use: "gb"}
	root.SetFlagErrorFunc(flagErrorWithPositionalHint)
	leaf := &cobra.Command{
		Use:  "status <bead-id-or-mr-url>",
		Args: cobra.MaximumNArgs(1),
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	var bead string
	leaf.Flags().StringVar(&bead, "bead", "", "bead id")
	root.AddCommand(leaf)
	root.SetArgs([]string{"status", "--bead"}) // flag present, value missing
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected a flag-needs-argument error, got nil")
	}
	if !strings.Contains(err.Error(), "needs an argument") {
		t.Fatalf("expected a needs-an-argument error, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "hint:") {
		t.Errorf("value-required error should get no positional hint, got %q", err.Error())
	}
}

func TestPositionalHintForCommand(t *testing.T) {
	tests := []struct {
		name        string
		use         string
		flagDisplay string
		want        string
		wantEmpty   bool
		subCommand  bool // give the command a child, making it a container
	}{
		{
			name:        "single required placeholder",
			use:         "status <bead-id-or-mr-url>",
			flagDisplay: "--bead",
			want:        "hint: gb status takes --bead positionally: gb status <bead-id-or-mr-url>",
		},
		{
			name:        "optional-wrapped placeholder still matches",
			use:         "status [<bead-id-or-mr-url>]",
			flagDisplay: "--bead",
			want:        "hint: gb status takes --bead positionally: gb status <bead-id-or-mr-url>",
		},
		{
			name:        "multiple placeholders are all listed",
			use:         "link <bead-id> <mr-url>",
			flagDisplay: "--bead",
			want:        "hint: gb link takes --bead positionally: gb link <bead-id> <mr-url>",
		},
		{
			name:        "empty flag name falls back to a generic subject",
			use:         "status <bead-id-or-mr-url>",
			flagDisplay: "",
			want:        "hint: gb status takes that value positionally: gb status <bead-id-or-mr-url>",
		},
		{
			name:        "no placeholder yields no hint",
			use:         "list",
			flagDisplay: "--owner",
			wantEmpty:   true,
		},
		{
			// A <command> placeholder on a container command means "a subcommand
			// goes here", not "a positional value" — see the HasSubCommands guard.
			name:        "container command placeholder yields no hint",
			use:         "mr <command>",
			flagDisplay: "--bead",
			wantEmpty:   true,
			subCommand:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := &cobra.Command{Use: "gb"}
			leaf := &cobra.Command{Use: tc.use}
			root.AddCommand(leaf)
			if tc.subCommand {
				leaf.AddCommand(&cobra.Command{Use: "status <bead-id>"})
			}

			got := positionalHintForCommand(leaf, tc.flagDisplay)
			if tc.wantEmpty {
				if got != "" {
					t.Errorf("want empty hint, got %q", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("hint = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFlagErrorHint_ShorthandRenderedWithSingleDash: an unknown SHORTHAND flag
// must be echoed as `-b`, not `--b`. pflag reports the offending character via
// the shorthand group, so naive "--"+name would name a flag that never existed.
func TestFlagErrorHint_ShorthandRenderedWithSingleDash(t *testing.T) {
	root, _ := newHintTestCmd("status [<bead-id-or-mr-url>]")
	root.SetArgs([]string{"mr", "status", "-b", "gd-abc"})
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected an unknown-shorthand-flag error, got nil")
	}
	got := err.Error()
	if !strings.Contains(got, "hint:") {
		t.Fatalf("expected a hint for an unknown shorthand, got %q", got)
	}
	if !strings.Contains(got, "takes -b positionally") {
		t.Errorf("shorthand should render as -b, got %q", got)
	}
	if strings.Contains(got, "--b ") {
		t.Errorf("shorthand must not render as --b, got %q", got)
	}
}

// TestFlagErrorHint_UnwrapsToOriginal guards that the wrapped error keeps the
// typed pflag error reachable via errors.As, so callers can still classify it.
func TestFlagErrorHint_UnwrapsToOriginal(t *testing.T) {
	_, leaf := newHintTestCmd("status [<bead-id-or-mr-url>]")
	orig := &pflag.NotExistError{}
	wrapped := flagErrorWithPositionalHint(leaf, orig)

	var notExist *pflag.NotExistError
	if !errors.As(wrapped, &notExist) {
		t.Fatalf("wrapped error no longer unwraps to *pflag.NotExistError: %v", wrapped)
	}
	if notExist != orig {
		t.Errorf("unwrapped to a different error instance")
	}
}
