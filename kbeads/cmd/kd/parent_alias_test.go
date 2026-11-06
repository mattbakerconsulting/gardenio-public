package main

import (
	"testing"

	"github.com/spf13/cobra"
)

// TestParentAlias_ParsesIdenticalToCanonical pins the core contract of
// aliasParentTaskFlag (ka-0000003191): `--parent gd-X` on `kd create` parses
// identically to the canonical `--parent-task gd-X`.
//
// The alias is a pflag NormalizeFunc, which fires during Parse — so these
// cases go through Flags().Parse (NOT Flags().Set, which bypasses
// normalization).
func TestParentAlias_ParsesIdenticalToCanonical(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "parent_alias_sets_parent_task",
			args: []string{"--parent", "gd-EPIC1"},
			want: "gd-EPIC1",
		},
		{
			name: "canonical_parent_task_still_works",
			args: []string{"--parent-task", "gd-EPIC1"},
			want: "gd-EPIC1",
		},
		{
			name: "parent_alias_last_write_wins_over_canonical",
			args: []string{"--parent-task", "gd-A", "--parent", "gd-B"},
			want: "gd-B",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh command per subtest so parse state never leaks. The flag
			// shape mirrors create exactly, including the pre-existing label
			// alias, so the compose-onto-existing-normalizer path is exercised.
			cmd := &cobra.Command{Use: "create"}
			cmd.Flags().StringP("label", "l", "", "")
			cmd.Flags().String("parent-task", "", "")
			aliasLabelFlags(cmd)
			aliasParentTaskFlag(cmd)

			if err := cmd.Flags().Parse(tc.args); err != nil {
				t.Fatalf("Parse(%v) returned error: %v", tc.args, err)
			}
			got, _ := cmd.Flags().GetString("parent-task")
			if got != tc.want {
				t.Errorf("parent-task = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParentAlias_NoErrorOnAliasFlag is the regression guard for the reported
// symptom: `kd create ... --parent gd-X` must NOT abort with
// "unknown flag: --parent".
func TestParentAlias_NoErrorOnAliasFlag(t *testing.T) {
	cmd := &cobra.Command{Use: "create"}
	cmd.Flags().String("parent-task", "", "")
	aliasParentTaskFlag(cmd)

	if err := cmd.Flags().Parse([]string{"--parent", "gd-X"}); err != nil {
		t.Fatalf("parsing --parent must not error, got: %v", err)
	}
}

// TestParentAlias_ComposesWithLabelAlias proves the chained normalizer keeps
// BOTH aliases live: `--labels` still resolves to `--label` (aliasLabelFlags)
// AND `--parent` resolves to `--parent-task` (aliasParentTaskFlag), in one
// invocation. This is the regression guard against a future edit that installs
// aliasParentTaskFlag in a way that clobbers the label normalizer (or vice
// versa) — pflag allows only one normalize func per FlagSet, so they MUST
// chain.
func TestParentAlias_ComposesWithLabelAlias(t *testing.T) {
	cmd := &cobra.Command{Use: "create"}
	cmd.Flags().StringSliceP("label", "l", nil, "")
	cmd.Flags().String("parent-task", "", "")
	aliasLabelFlags(cmd)
	aliasParentTaskFlag(cmd)

	if err := cmd.Flags().Parse([]string{"--labels", "a,b", "--parent", "gd-P"}); err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	gotLabel, _ := cmd.Flags().GetStringSlice("label")
	if !sliceEq(gotLabel, []string{"a", "b"}) {
		t.Errorf("label = %v, want [a b] (label alias must survive parent-alias chaining)", gotLabel)
	}
	gotParent, _ := cmd.Flags().GetString("parent-task")
	if gotParent != "gd-P" {
		t.Errorf("parent-task = %q, want %q (parent alias must survive label-alias chaining)", gotParent, "gd-P")
	}
}

// TestParentAlias_InitWired proves the real createCmd carries the alias through
// its OWN init() normalize func: `kd create --parent` resolves to the
// `parent-task` flag. Dropping aliasParentTaskFlag(createCmd) from init() fails
// this test.
func TestParentAlias_InitWired(t *testing.T) {
	if f := createCmd.Flags().Lookup("parent"); f == nil || f.Name != "parent-task" {
		t.Errorf("create: Lookup(%q) = %v, want flag named %q", "parent", f, "parent-task")
	}
	// The label alias must still be live on createCmd too — the two helpers
	// compose in init(), they do not compete.
	if f := createCmd.Flags().Lookup("labels"); f == nil || f.Name != "label" {
		t.Errorf("create: Lookup(%q) = %v, want flag named %q (label alias regressed)", "labels", f, "label")
	}
}
