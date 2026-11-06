package main

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestLabelAlias_ParsesIdenticalToCanonical pins the core contract of
// aliasLabelFlags (ka-0000000987): the pluralized `--labels` / `--labels-any`
// spellings parse identically to the canonical `-l/--label` / `--label-any`
// flags, and merge with any canonical occurrences in the same invocation.
//
// The alias is implemented as a pflag NormalizeFunc, which fires during
// Parse — so these cases go through Flags().Parse (NOT Flags().Set, which
// bypasses normalization).
func TestLabelAlias_ParsesIdenticalToCanonical(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantLabel   []string
		wantLabelAn []string
		wantExclude []string
	}{
		{
			name:      "labels_plural_equals_label",
			args:      []string{"--labels", "project:gardenio,project:monorepo"},
			wantLabel: []string{"project:gardenio", "project:monorepo"},
		},
		{
			name:      "shorthand_still_works",
			args:      []string{"-l", "a,b"},
			wantLabel: []string{"a", "b"},
		},
		{
			name:      "canonical_label_still_works",
			args:      []string{"--label", "a,b"},
			wantLabel: []string{"a", "b"},
		},
		{
			name:      "labels_merges_with_canonical_label",
			args:      []string{"--labels", "a,b", "--label", "c"},
			wantLabel: []string{"a", "b", "c"},
		},
		{
			name:        "labels_any_plural_equals_label_any",
			args:        []string{"--labels-any", "x,y"},
			wantLabelAn: []string{"x", "y"},
		},
		{
			name:        "labels_and_labels_any_independent",
			args:        []string{"--labels", "a", "--labels-any", "x,y"},
			wantLabel:   []string{"a"},
			wantLabelAn: []string{"x", "y"},
		},
		{
			name:        "exclude_labels_plural_equals_exclude_label",
			args:        []string{"--exclude-labels", "needs-intake,release-regression"},
			wantExclude: []string{"needs-intake", "release-regression"},
		},
		{
			name:        "exclude_label_canonical_still_works",
			args:        []string{"--exclude-label", "needs-intake"},
			wantExclude: []string{"needs-intake"},
		},
		{
			name:        "exclude_labels_merges_with_canonical",
			args:        []string{"--exclude-labels", "a", "--exclude-label", "b"},
			wantExclude: []string{"a", "b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh command per subtest so parse state never leaks. The flag
			// shape mirrors list/search exactly.
			cmd := &cobra.Command{Use: "list"}
			cmd.Flags().StringSliceP("label", "l", nil, "")
			cmd.Flags().StringSlice("label-any", nil, "")
			cmd.Flags().StringSlice("exclude-label", nil, "")
			aliasLabelFlags(cmd)

			if err := cmd.Flags().Parse(tc.args); err != nil {
				t.Fatalf("Parse(%v) returned error: %v", tc.args, err)
			}
			gotLabel, _ := cmd.Flags().GetStringSlice("label")
			gotLabelAny, _ := cmd.Flags().GetStringSlice("label-any")
			gotExclude, _ := cmd.Flags().GetStringSlice("exclude-label")
			if !sliceEq(gotLabel, tc.wantLabel) {
				t.Errorf("label = %v, want %v", gotLabel, tc.wantLabel)
			}
			if !sliceEq(gotLabelAny, tc.wantLabelAn) {
				t.Errorf("label-any = %v, want %v", gotLabelAny, tc.wantLabelAn)
			}
			if !sliceEq(gotExclude, tc.wantExclude) {
				t.Errorf("exclude-label = %v, want %v", gotExclude, tc.wantExclude)
			}
		})
	}
}

// TestLabelAlias_NoErrorOnPluralFlag is the regression guard for the reported
// symptom: `--labels foo` must NOT abort with "unknown flag: --labels".
func TestLabelAlias_NoErrorOnPluralFlag(t *testing.T) {
	cmd := &cobra.Command{Use: "list"}
	cmd.Flags().StringSliceP("label", "l", nil, "")
	cmd.Flags().StringSlice("label-any", nil, "")
	aliasLabelFlags(cmd)

	if err := cmd.Flags().Parse([]string{"--labels", "foo"}); err != nil {
		t.Fatalf("parsing --labels must not error, got: %v", err)
	}
}

// TestLabelAlias_InitWired proves every command that registers --label carries
// the alias through its OWN init() normalize func (not just a synthetic command).
// This pins the per-command wiring: dropping aliasLabelFlags(...) from any of
// these init()s fails this test. `kd <cmd> --labels a,b` therefore resolves to
// the canonical `--label` flag.
//
// Note: `--labels-any` resolves to `label-any` only on commands that actually
// register --label-any (list, search); on the others it stays "unknown flag"
// exactly as the genuine --label-any already does there — so we assert the
// labels→label mapping on all of them, and the labels-any→label-any mapping
// only where the canonical flag exists.
func TestLabelAlias_InitWired(t *testing.T) {
	// Every command wired with aliasLabelFlags that is reachable as a package
	// var. formulaApplyCmd exercises the shared addInstantiateFlags path; the
	// pour/wisp commands use the same helper but are built via factory funcs
	// with function-local vars, so they are covered transitively by that path.
	cmds := []struct {
		name            string
		cmd             *cobra.Command
		hasLabelAny     bool
		hasExcludeLabel bool
	}{
		{"list", listCmd, true, true},
		{"search", searchCmd, true, false},
		{"create", createCmd, false, false},
		{"advice add", adviceAddCmd, false, false},
		{"advice list", adviceListCmd, false, false},
		{"mol list", molListCmd, false, false},
		{"formula list", formulaListCmd, false, false},
		{"formula create", formulaCreateCmd, false, false},
		{"formula apply", formulaApplyCmd, false, false},
	}
	for _, c := range cmds {
		// --labels must resolve to --label on every wired command.
		if f := c.cmd.Flags().Lookup("labels"); f == nil || f.Name != "label" {
			t.Errorf("%s: Lookup(%q) = %v, want flag named %q", c.name, "labels", f, "label")
		}
		// --labels-any resolves to --label-any only where that flag is registered.
		if c.hasLabelAny {
			if f := c.cmd.Flags().Lookup("labels-any"); f == nil || f.Name != "label-any" {
				t.Errorf("%s: Lookup(%q) = %v, want flag named %q", c.name, "labels-any", f, "label-any")
			}
		}
		// --exclude-labels resolves to --exclude-label only where registered (list).
		if c.hasExcludeLabel {
			if f := c.cmd.Flags().Lookup("exclude-labels"); f == nil || f.Name != "exclude-label" {
				t.Errorf("%s: Lookup(%q) = %v, want flag named %q", c.name, "exclude-labels", f, "exclude-label")
			}
		}
	}
}

// TestLabelAlias_JackUpRealLabelsFlagUntouched is the safety regression: the
// `kd jack up` command registers a GENUINE `--labels` flag (jack_up.go). The
// alias must be per-command and must NEVER shadow it — `jack up --labels`
// still resolves to jack_up's own StringSlice("labels"), not a remapped
// "label" (which jackUpCmd does not even register).
func TestLabelAlias_JackUpRealLabelsFlagUntouched(t *testing.T) {
	f := jackUpCmd.Flags().Lookup("labels")
	if f == nil {
		t.Fatal("jackUpCmd lost its real --labels flag")
	}
	if f.Name != "labels" {
		t.Fatalf("jack up --labels resolved to %q, want the genuine 'labels' flag", f.Name)
	}
	// jackUpCmd must not have acquired a 'label' flag via a stray normalize func.
	if lf := jackUpCmd.Flags().Lookup("label"); lf != nil {
		t.Fatalf("jackUpCmd unexpectedly resolves --label to %q; alias leaked onto jack up", lf.Name)
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// compile-time anchor so an unused pflag import is impossible if the helper
// signature ever changes.
var _ = pflag.NormalizedName("")
