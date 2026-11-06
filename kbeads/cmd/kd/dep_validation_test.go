package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newDepAddCmdForTest returns a command with the same Args + flag contract as
// depAddCmd, but a no-op RunE so the test exercises only cobra's
// argument/flag validation (the part that must reject malformed invocations
// before any daemon call). depAddCmd's real RunE needs a live beadsClient.
func newDepAddCmdForTest() *cobra.Command {
	c := &cobra.Command{
		Use:  "add <bead-id> <depends-on-id>",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, _ []string) error { return nil },
	}
	c.Flags().StringP("type", "t", "blocks", "dependency type")
	return c
}

// TestDepAdd_RejectsMalformedInvocations is the regression lock for the first
// defect of ka-0000005388: a bad `kd dep add` invocation (the historical
// `kd dep add --parent <id>` mistake, or a stray third positional arg) must
// FAIL LOUDLY rather than silently no-op. A silent no-op on a dependency link
// is what let monorepo-dark Phase-0 children go unlinked, which in turn let a
// goal-completion gardener close at 0 runs.
func TestDepAdd_RejectsMalformedInvocations(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{"unknown_parent_flag", []string{"gd-aaa", "gd-bbb", "--parent", "gd-ccc"}, "unknown flag"},
		{"extra_positional_arg", []string{"gd-aaa", "gd-bbb", "gd-ccc"}, "accepts 2 arg"},
		{"too_few_args", []string{"gd-aaa"}, "accepts 2 arg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newDepAddCmdForTest()
			var buf bytes.Buffer
			c.SetOut(&buf)
			c.SetErr(&buf)
			c.SetArgs(tc.args)
			err := c.Execute()
			if err == nil {
				t.Fatalf("expected error for %v, got nil (silent no-op regression)", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}
