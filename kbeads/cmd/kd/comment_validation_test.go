package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestResolveCommentText is the regression lock for ka-0000000244: `kd comment
// add` must accept the comment text from EITHER a positional arg or the
// --body/-m flag, must reject supplying both, and must reject empty text. The
// XOR/neither/empty logic lives in resolveCommentText (not cobra's Args/flag
// layer), so this table-tests the pure helper directly. The cobra wiring that
// feeds it is exercised separately by TestCommentAdd_FlagParsing.
func TestResolveCommentText(t *testing.T) {
	cases := []struct {
		name     string
		bodySet  bool
		body     string
		textArgs []string
		want     string
		wantErr  string
	}{
		{"body_flag", true, "hello world", nil, "hello world", ""},
		{"positional_single", false, "", []string{"hello"}, "hello", ""},
		{"positional_joined", false, "", []string{"hello", "world"}, "hello world", ""},
		{"body_wins_when_no_positional", true, "from-flag", nil, "from-flag", ""},
		{"both_sources_rejected", true, "from-flag", []string{"from-pos"}, "", "not both"},
		{"neither_source_rejected", false, "", nil, "", "comment text required"},
		{"empty_body_flag_rejected", true, "", nil, "", "comment text required"},
		{"empty_positional_rejected", false, "", []string{""}, "", "comment text required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveCommentText(tc.bodySet, tc.body, tc.textArgs)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (text=%q)", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// newCommentAddCmdForTest mirrors commentAddCmd's Args + flag contract with a
// RunE that resolves the text via the real helper and stashes the result, so
// the test asserts on the same parse → resolve path the real command takes
// (minus the daemon AddComment call, which needs a live beadsClient).
func newCommentAddCmdForTest(resolved *string) *cobra.Command {
	var body string
	c := &cobra.Command{
		Use:  "add <bead-id> [<text>...]",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			text, err := resolveCommentText(cmd.Flags().Changed("body"), body, args[1:])
			if err != nil {
				return err
			}
			*resolved = text
			return nil
		},
	}
	c.Flags().StringVarP(&body, "body", "m", "", "comment text (alternative to positional <text>)")
	aliasCommentBodyFlags(c)
	return c
}

// TestCommentAdd_FlagParsing locks the cobra wiring: the --body long flag, the
// -m shorthand, and the legacy positional form must all parse and resolve to
// the comment text, while the no-args and both-sources invocations must error.
func TestCommentAdd_FlagParsing(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantText string
		wantErr  string
	}{
		{"body_long_flag", []string{"gd-aaa", "--body", "hello"}, "hello", ""},
		{"body_short_flag", []string{"gd-aaa", "-m", "hello"}, "hello", ""},
		{"content_alias", []string{"gd-aaa", "--content", "hello"}, "hello", ""},
		{"message_alias", []string{"gd-aaa", "--message", "hello"}, "hello", ""},
		{"positional_legacy", []string{"gd-aaa", "hello", "world"}, "hello world", ""},
		{"no_args", []string{}, "", "requires at least 1 arg"},
		{"bead_id_only", []string{"gd-aaa"}, "", "comment text required"},
		{"both_sources", []string{"gd-aaa", "positional", "--body", "flag"}, "", "not both"},
		{"content_alias_and_positional_rejected", []string{"gd-aaa", "positional", "--content", "flag"}, "", "not both"},
		{"unknown_flag", []string{"gd-aaa", "--nope", "x"}, "", "unknown flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resolved string
			c := newCommentAddCmdForTest(&resolved)
			var buf bytes.Buffer
			c.SetOut(&buf)
			c.SetErr(&buf)
			c.SetArgs(tc.args)
			err := c.Execute()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q for %v, got nil", tc.wantErr, tc.args)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %v: %v", tc.args, err)
			}
			if resolved != tc.wantText {
				t.Fatalf("resolved %q, want %q", resolved, tc.wantText)
			}
		})
	}
}

// newCommentGroupForTest builds a STANDALONE `comment` command tree that wires
// the REAL commentArgs validator, the REAL default RunE (bare-form `comment <id>
// <text>` → add delegation, from comment.go), the REAL runCommentAdd add body,
// and a --body/-m flag. It is parentless on purpose: cobra's Command.Execute()
// re-roots to the command's Root(), so executing the global commentCmd would
// re-root to rootCmd and run its PersistentPreRunE — which overwrites the mocked
// beadsClient with a live HTTP/gRPC client. A parentless copy keeps the path
// hermetic. Each call also re-registers --body via StringVarP, which resets the
// shared commentBody global to "", so the flag's Changed state cannot leak
// between subtests.
func newCommentGroupForTest() *cobra.Command {
	c := &cobra.Command{
		Use:          "comment",
		Args:         commentArgs,
		RunE:         commentCmd.RunE, // the real bare-form delegation RunE
		SilenceUsage: true,
	}
	c.Flags().StringVarP(&commentBody, "body", "m", "", "comment text (alternative to positional <text>)")
	aliasCommentBodyFlags(c)

	add := &cobra.Command{
		Use:  "add <bead-id> [<text>...]",
		Args: cobra.MinimumNArgs(1),
		RunE: runCommentAdd, // the real shared add body
	}
	add.Flags().StringVarP(&commentBody, "body", "m", "", "comment text (alternative to positional <text>)")
	aliasCommentBodyFlags(add)
	c.AddCommand(add)
	return c
}

// execComment runs the standalone test tree with the given args, capturing
// stdout+stderr, and returns the execute error. The mocked beadsClient must be
// installed by the caller.
func execComment(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := newCommentGroupForTest()
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs(args)
	err := c.Execute()
	return buf.String(), err
}

// TestCommentBareForm_DelegatesToAdd is the regression lock for ka-0000006021:
// `kd comment <id> <text...>` (no `add` subcommand) must POST the comment, not
// error `unknown command "<id>" for "kd comment"` and silently drop the text.
// It asserts AddComment is called exactly once with the bead id and the joined
// positional text.
func TestCommentBareForm_DelegatesToAdd(t *testing.T) {
	mc := newMockClient()
	orig := beadsClient
	beadsClient = mc
	t.Cleanup(func() { beadsClient = orig })

	out, err := execComment(t, "gd-c3u76usvml", "hello", "world")
	if err != nil {
		t.Fatalf("`comment gd-c3u76usvml hello world` errored: %v\n%s", err, out)
	}
	if len(mc.AddCommentCalls) != 1 {
		t.Fatalf("AddComment called %d times, want exactly 1 (text must not be dropped)", len(mc.AddCommentCalls))
	}
	got := mc.AddCommentCalls[0]
	if got.BeadID != "gd-c3u76usvml" {
		t.Errorf("AddComment bead = %q, want gd-c3u76usvml", got.BeadID)
	}
	if got.Text != "hello world" {
		t.Errorf("AddComment text = %q, want %q (positional args must be joined)", got.Text, "hello world")
	}
}

// TestCommentBareForm_BodyFlag covers the bare form with the --body flag instead
// of positional text: `kd comment <id> --body 'x'` must post "x".
func TestCommentBareForm_BodyFlag(t *testing.T) {
	mc := newMockClient()
	orig := beadsClient
	beadsClient = mc
	t.Cleanup(func() { beadsClient = orig })

	out, err := execComment(t, "gd-c3u76usvml", "--body", "via flag")
	if err != nil {
		t.Fatalf("`comment gd-c3u76usvml --body 'via flag'` errored: %v\n%s", err, out)
	}
	if len(mc.AddCommentCalls) != 1 || mc.AddCommentCalls[0].Text != "via flag" {
		t.Fatalf("AddComment calls = %+v, want one call with text %q", mc.AddCommentCalls, "via flag")
	}
}

// TestCommentBareForm_ContentAndMessageAliases is the regression lock for
// ka-0000001669: the bare `kd comment <id> --content 'x'` / `--message 'x'`
// forms (group-fallback path, which parses with commentCmd's own flags) must
// normalize the alias to --body and POST the text — not error `unknown flag:
// --content` and silently drop it. Both the add subcommand and this bare path
// carry aliasCommentBodyFlags, so both spellings resolve on either route.
func TestCommentBareForm_ContentAndMessageAliases(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag string
	}{
		{"content_alias", "--content"},
		{"message_alias", "--message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMockClient()
			orig := beadsClient
			beadsClient = mc
			t.Cleanup(func() { beadsClient = orig })

			out, err := execComment(t, "gd-c3u76usvml", tc.flag, "aliased body")
			if err != nil {
				t.Fatalf("`comment gd-c3u76usvml %s 'aliased body'` errored: %v\n%s", tc.flag, err, out)
			}
			if len(mc.AddCommentCalls) != 1 || mc.AddCommentCalls[0].Text != "aliased body" {
				t.Fatalf("AddComment calls = %+v, want one call with text %q", mc.AddCommentCalls, "aliased body")
			}
		})
	}
}

// TestCommentAddSubcommand_ContentAlias covers the explicit `kd comment add <id>
// --content 'x'` route (as opposed to the bare form above), proving the alias is
// wired on the add subcommand too.
func TestCommentAddSubcommand_ContentAlias(t *testing.T) {
	mc := newMockClient()
	orig := beadsClient
	beadsClient = mc
	t.Cleanup(func() { beadsClient = orig })

	out, err := execComment(t, "add", "gd-c3u76usvml", "--content", "via content")
	if err != nil {
		t.Fatalf("`comment add gd-c3u76usvml --content 'via content'` errored: %v\n%s", err, out)
	}
	if len(mc.AddCommentCalls) != 1 || mc.AddCommentCalls[0].Text != "via content" || mc.AddCommentCalls[0].BeadID != "gd-c3u76usvml" {
		t.Fatalf("AddComment calls = %+v, want one call bead=gd-c3u76usvml text=%q", mc.AddCommentCalls, "via content")
	}
}

// TestCommentBareForm_BodyAndPositionalRejected proves the flag-XOR-positional
// contract (resolveCommentText) survives the bare delegation path: supplying
// BOTH --body and positional text errors, and NO comment is posted.
func TestCommentBareForm_BodyAndPositionalRejected(t *testing.T) {
	mc := newMockClient()
	orig := beadsClient
	beadsClient = mc
	t.Cleanup(func() { beadsClient = orig })

	_, err := execComment(t, "gd-c3u76usvml", "--body", "x", "hello")
	if err == nil {
		t.Fatal("`comment gd-id --body x hello` returned nil; want flag-XOR-positional error")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("error = %q; want it to mention \"not both\"", err.Error())
	}
	if len(mc.AddCommentCalls) != 0 {
		t.Errorf("AddComment called %d times on a rejected invocation; want 0", len(mc.AddCommentCalls))
	}
}

// TestCommentAddSubcommand_Unchanged is the regression guard that the explicit
// `kd comment add <id> <text>` path is untouched by the bare-form refactor — it
// still routes to the add subcommand and posts the text.
func TestCommentAddSubcommand_Unchanged(t *testing.T) {
	mc := newMockClient()
	orig := beadsClient
	beadsClient = mc
	t.Cleanup(func() { beadsClient = orig })

	out, err := execComment(t, "add", "gd-c3u76usvml", "hello")
	if err != nil {
		t.Fatalf("`comment add gd-c3u76usvml hello` errored: %v\n%s", err, out)
	}
	if len(mc.AddCommentCalls) != 1 || mc.AddCommentCalls[0].Text != "hello" || mc.AddCommentCalls[0].BeadID != "gd-c3u76usvml" {
		t.Fatalf("AddComment calls = %+v, want one call bead=gd-c3u76usvml text=hello", mc.AddCommentCalls)
	}
}

// TestCommentBareForm_NonIDFirstTokenRejected pins that a non-bead-id first
// token (a real typo, not a comment target) is rejected as an unknown
// subcommand — NOT mistaken for a bead id, and NOT a panic. This preserves the
// no-silent-exit-0 group contract (group_noargs_test.go) for `kd comment bogus`.
func TestCommentBareForm_NonIDFirstTokenRejected(t *testing.T) {
	mc := newMockClient()
	orig := beadsClient
	beadsClient = mc
	t.Cleanup(func() { beadsClient = orig })

	_, err := execComment(t, "frobnicate")
	if err == nil {
		t.Fatal("`comment frobnicate` returned nil; want an unknown-command error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error = %q; want it to mention \"unknown command\"", err.Error())
	}
	if len(mc.AddCommentCalls) != 0 {
		t.Errorf("AddComment called on a non-id token; want 0 calls")
	}
}

// TestCommentBareForm_NoArgsShowsHelp asserts a bare `kd comment` (no args)
// still falls through to Help() and exits 0 without posting anything.
func TestCommentBareForm_NoArgsShowsHelp(t *testing.T) {
	mc := newMockClient()
	orig := beadsClient
	beadsClient = mc
	t.Cleanup(func() { beadsClient = orig })

	out, err := execComment(t)
	if err != nil {
		t.Fatalf("bare `comment` returned %v; want nil (help, exit 0)", err)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("bare `comment` output did not contain a usage block:\n%s", out)
	}
	if len(mc.AddCommentCalls) != 0 {
		t.Errorf("bare `comment` posted a comment; want 0 AddComment calls")
	}
}

// TestCommentBareForm_BodyWithoutBeadIDErrors covers the footgun where an agent
// runs `kd comment --body 'note'` and forgets the bead id: the missing-id case
// must ERROR (not silently show help + exit 0 + drop the text, which would be
// the same silent-drop class as the bug this MR fixes). A truly-empty
// `kd comment` (no --body) still shows help, asserted by NoArgsShowsHelp above.
func TestCommentBareForm_BodyWithoutBeadIDErrors(t *testing.T) {
	mc := newMockClient()
	orig := beadsClient
	beadsClient = mc
	t.Cleanup(func() { beadsClient = orig })

	_, err := execComment(t, "--body", "orphan note")
	if err == nil {
		t.Fatal("`comment --body 'orphan note'` returned nil; want a missing-bead-id error (not silent help)")
	}
	if !strings.Contains(err.Error(), "bead id is required") {
		t.Errorf("error = %q; want it to mention \"bead id is required\"", err.Error())
	}
	if len(mc.AddCommentCalls) != 0 {
		t.Errorf("AddComment called %d times with no bead id; want 0", len(mc.AddCommentCalls))
	}
}

// TestCommentCmd_BareDelegationContract operates on the REAL commentCmd (not a
// test copy) to lock the wiring that makes the bare form work, guarding against
// a future change re-adding commentCmd to group.go's markAsGroup registry
// (which sets cobra.NoArgs and would reject the `kd comment <id> <text>` form):
//
//   - commentCmd must be Runnable (have the delegating RunE).
//   - it must ACCEPT a bead-id-led positional (`gd-… hi`) — the delegation form.
//   - it must still REJECT a non-id first token as an unknown subcommand.
//   - a bare invocation (no args) must validate (→ Help).
func TestCommentCmd_BareDelegationContract(t *testing.T) {
	if !commentCmd.Runnable() {
		t.Fatal("commentCmd is not Runnable — the bare-form delegation RunE was lost")
	}
	if err := commentCmd.ValidateArgs([]string{"ka-0000001713", "hi"}); err != nil {
		t.Errorf("commentCmd.ValidateArgs([gd-…, hi]) = %v; want nil — `kd comment <id> <text>` must validate (commentCmd must not be NoArgs/markAsGroup'd)", err)
	}
	err := commentCmd.ValidateArgs([]string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("commentCmd.ValidateArgs([bogus]) = %v; want an \"unknown command\" rejection", err)
	}
	if err := commentCmd.ValidateArgs(nil); err != nil {
		t.Errorf("commentCmd.ValidateArgs(nil) = %v; want nil — bare `kd comment` should show help", err)
	}
}

// TestCommentCmd_RealRunE_Posts invokes the REAL commentCmd.RunE (the exact
// function value the production `kd comment <id> <text>` dispatches to — not a
// reconstructed copy) with a mocked client, and asserts it posts the joined
// text. Together with TestCommentCmd_BareDelegationContract (Args wiring) and
// TestRealRootCmd-style Find checks, this closes the loop: the actual posting
// path is exercised, so a regression that breaks delegation can't hide behind a
// green parentless-copy test.
func TestCommentCmd_RealRunE_Posts(t *testing.T) {
	mc := newMockClient()
	orig := beadsClient
	beadsClient = mc
	t.Cleanup(func() { beadsClient = orig })
	// commentCmd shares the &commentBody global with commentAddCmd; ensure a
	// clean (unset) flag state so resolveCommentText reads the positional text.
	commentCmd.Flags().Set("body", "")
	commentCmd.Flags().Lookup("body").Changed = false
	t.Cleanup(func() {
		commentCmd.Flags().Set("body", "")
		commentCmd.Flags().Lookup("body").Changed = false
	})

	if err := commentCmd.RunE(commentCmd, []string{"gd-c2okEOYlzo", "hi", "there"}); err != nil {
		t.Fatalf("commentCmd.RunE([gd-c2okEOYlzo hi there]) = %v; want nil (must post)", err)
	}
	if len(mc.AddCommentCalls) != 1 {
		t.Fatalf("AddComment called %d times via the real RunE; want exactly 1", len(mc.AddCommentCalls))
	}
	if got := mc.AddCommentCalls[0]; got.BeadID != "gd-c2okEOYlzo" || got.Text != "hi there" {
		t.Errorf("real RunE posted bead=%q text=%q; want bead=gd-c2okEOYlzo text=%q", got.BeadID, got.Text, "hi there")
	}
}
