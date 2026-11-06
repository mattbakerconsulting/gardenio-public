package main

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
)

// newDoneCmdForTest builds a STANDALONE copy of doneCmd reusing the REAL RunE,
// the REAL --comment/-m flag, and the REAL aliasDoneReasonFlags normalizer, so
// the test exercises the actual parse → alias → RunE path. Parentless on
// purpose (see newCloseCmdForTest for the rationale).
func newDoneCmdForTest() *cobra.Command {
	c := &cobra.Command{
		Use:          "done <id>...",
		Args:         cobra.MinimumNArgs(1),
		RunE:         doneCmd.RunE,
		SilenceUsage: true,
	}
	c.Flags().StringP("comment", "m", "", "completion comment to add before closing (alias: --reason)")
	aliasDoneReasonFlags(c)
	return c
}

func execDone(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := newDoneCmdForTest()
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs(args)
	err := c.Execute()
	return buf.String(), err
}

// TestDone_ReasonAliasesToComment proves `kd done --reason "..."` resolves to
// the canonical --comment path (cross-command symmetry with `kd close --reason`,
// review finding #2), recording the comment before closing.
func TestDone_ReasonAliasesToComment(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	out, err := execDone(t, "gd-c3u76usvml", "--reason", "shipped")
	if err != nil {
		t.Fatalf("`done --reason` errored: %v\n%s", err, out)
	}
	if len(mc.AddCommentCalls) != 1 || mc.AddCommentCalls[0].Text != "shipped" {
		t.Fatalf("AddComment calls = %+v, want one call text=%q (--reason must alias --comment)", mc.AddCommentCalls, "shipped")
	}
	if len(mc.CloseBeadCalls) != 1 {
		t.Fatalf("CloseBead called %d times, want 1", len(mc.CloseBeadCalls))
	}
}

// TestDone_CommentCanonicalUnchanged is the regression guard that the canonical
// --comment/-m spelling still works after adding the alias.
func TestDone_CommentCanonicalUnchanged(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	if _, err := execDone(t, "gd-c3u76usvml", "--comment", "via comment"); err != nil {
		t.Fatalf("`done --comment` errored: %v", err)
	}
	if _, err := execDone(t, "gd-lzwcnercx2", "-m", "via m"); err != nil {
		t.Fatalf("`done -m` errored: %v", err)
	}
	if len(mc.AddCommentCalls) != 2 {
		t.Fatalf("AddComment calls = %d, want 2 (--comment and -m both post)", len(mc.AddCommentCalls))
	}
	if mc.AddCommentCalls[0].Text != "via comment" || mc.AddCommentCalls[1].Text != "via m" {
		t.Errorf("comment texts = %q, %q; want \"via comment\", \"via m\"", mc.AddCommentCalls[0].Text, mc.AddCommentCalls[1].Text)
	}
}

// TestDoneCmd_ReasonAliasContract operates on the REAL doneCmd to lock the
// normalizer wiring: --reason must fold to --comment, and unrelated names pass
// through unchanged.
func TestDoneCmd_ReasonAliasContract(t *testing.T) {
	norm := doneCmd.Flags().GetNormalizeFunc()
	if got := string(norm(doneCmd.Flags(), "reason")); got != "comment" {
		t.Errorf("normalize(\"reason\") = %q, want \"comment\"", got)
	}
	if got := string(norm(doneCmd.Flags(), "comment")); got != "comment" {
		t.Errorf("normalize(\"comment\") = %q, want \"comment\" (canonical must pass through)", got)
	}
}
