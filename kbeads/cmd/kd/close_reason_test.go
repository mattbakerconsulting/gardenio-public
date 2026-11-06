package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

// newCloseCmdForTest builds a STANDALONE copy of closeCmd that reuses the REAL
// RunE plus the REAL flag set (--escalate, --reason/-r) and the REAL
// aliasCloseReasonFlags normalizer, so tests exercise the actual parse → alias
// → RunE path the production `kd close` takes. It is parentless on purpose:
// cobra's Execute() re-roots to the command's Root(), and executing the global
// closeCmd would re-root to rootCmd and run its PersistentPreRunE — which
// swaps the mocked beadsClient for a live client. A parentless copy keeps the
// path hermetic. Re-registering the flags per call also resets any flag state
// so --reason cannot leak between subtests.
func newCloseCmdForTest() *cobra.Command {
	c := &cobra.Command{
		Use:          "close <id>...",
		Args:         cobra.MinimumNArgs(1),
		RunE:         closeCmd.RunE, // the real close body
		SilenceUsage: true,
	}
	c.Flags().Bool("escalate", false, "request elevated access if write is denied")
	c.Flags().StringP("reason", "r", "", "record a closure reason (adds a comment before closing); alias: --comment")
	c.Flags().Bool("allow-open-mr", false, "close even if the bead's linked MR is still open/unmerged")
	c.Flags().Bool("force", false, "operator only: close over an open PR")
	aliasCloseReasonFlags(c)
	return c
}

// execClose runs the standalone close command with the given args, capturing
// stdout+stderr, and returns the execute error. The mocked beadsClient must be
// installed by the caller (via withMockClient) before invoking.
func execClose(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := newCloseCmdForTest()
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs(args)
	err := c.Execute()
	return buf.String(), err
}

// TestCloseWithReason is the core regression lock for ka-0000007746 (recurrence
// of ka-0000006283): `kd close <id> --reason "..."` must record the reason as a
// comment (authored by the actor) BEFORE closing the bead, so closure audit
// text is never silently dropped.
func TestCloseWithReason(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	out, err := execClose(t, "gd-c3u76usvml", "--reason", "fixed by MR !4321")
	if err != nil {
		t.Fatalf("`close gd-c3u76usvml --reason ...` errored: %v\n%s", err, out)
	}
	if len(mc.AddCommentCalls) != 1 {
		t.Fatalf("AddComment called %d times, want exactly 1 (reason must be recorded)", len(mc.AddCommentCalls))
	}
	got := mc.AddCommentCalls[0]
	if got.BeadID != "gd-c3u76usvml" {
		t.Errorf("comment bead = %q, want gd-c3u76usvml", got.BeadID)
	}
	if got.Author != "test-actor" {
		t.Errorf("comment author = %q, want test-actor (the acting user)", got.Author)
	}
	if got.Text != "fixed by MR !4321" {
		t.Errorf("comment text = %q, want %q", got.Text, "fixed by MR !4321")
	}
	if len(mc.CloseBeadCalls) != 1 || mc.CloseBeadCalls[0].ID != "gd-c3u76usvml" {
		t.Fatalf("CloseBead calls = %+v, want one close of gd-c3u76usvml", mc.CloseBeadCalls)
	}
}

// TestCloseWithReason_MultiID proves the reason comment lands on EACH id when
// several beads are closed in one invocation — not just the first.
func TestCloseWithReason_MultiID(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	out, err := execClose(t, "gd-ey0quy71sg", "gd-kmmgekjlko", "--reason", "batch: superseded")
	if err != nil {
		t.Fatalf("multi-id close with reason errored: %v\n%s", err, out)
	}
	if len(mc.AddCommentCalls) != 2 {
		t.Fatalf("AddComment called %d times, want 2 (one per id)", len(mc.AddCommentCalls))
	}
	wantIDs := []string{"gd-ey0quy71sg", "gd-kmmgekjlko"}
	for i, want := range wantIDs {
		if mc.AddCommentCalls[i].BeadID != want {
			t.Errorf("comment %d bead = %q, want %q", i, mc.AddCommentCalls[i].BeadID, want)
		}
		if mc.AddCommentCalls[i].Text != "batch: superseded" {
			t.Errorf("comment %d text = %q, want %q", i, mc.AddCommentCalls[i].Text, "batch: superseded")
		}
	}
	if len(mc.CloseBeadCalls) != 2 {
		t.Fatalf("CloseBead called %d times, want 2", len(mc.CloseBeadCalls))
	}
}

// TestClose_NoReason_Unchanged is the regression guard that the no-reason path
// is byte-for-byte unchanged: a bare `kd close <id>` must post NO comment and
// still close the bead.
func TestClose_NoReason_Unchanged(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	out, err := execClose(t, "gd-c3u76usvml")
	if err != nil {
		t.Fatalf("bare `close gd-c3u76usvml` errored: %v\n%s", err, out)
	}
	if len(mc.AddCommentCalls) != 0 {
		t.Errorf("AddComment called %d times on a no-reason close; want 0", len(mc.AddCommentCalls))
	}
	if len(mc.CloseBeadCalls) != 1 || mc.CloseBeadCalls[0].ID != "gd-c3u76usvml" {
		t.Fatalf("CloseBead calls = %+v, want one close of gd-c3u76usvml", mc.CloseBeadCalls)
	}
}

// TestCloseWithReason_CommentAlias proves the `--comment` alias reaches the same
// reason path (via aliasCloseReasonFlags) — the exact spelling ka-0000006283
// guessed — instead of an "unknown flag" usage dump that drops the text.
func TestCloseWithReason_CommentAlias(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	out, err := execClose(t, "gd-c3u76usvml", "--comment", "via alias")
	if err != nil {
		t.Fatalf("`close --comment` (alias) errored: %v\n%s", err, out)
	}
	if len(mc.AddCommentCalls) != 1 || mc.AddCommentCalls[0].Text != "via alias" {
		t.Fatalf("AddComment calls = %+v, want one call with text %q (--comment must alias to --reason)", mc.AddCommentCalls, "via alias")
	}
	if len(mc.CloseBeadCalls) != 1 {
		t.Fatalf("CloseBead called %d times, want 1", len(mc.CloseBeadCalls))
	}
}

// TestCloseWithReason_Shorthand proves the -r shorthand resolves to --reason.
func TestCloseWithReason_Shorthand(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	out, err := execClose(t, "gd-c3u76usvml", "-r", "short form")
	if err != nil {
		t.Fatalf("`close -r` errored: %v\n%s", err, out)
	}
	if len(mc.AddCommentCalls) != 1 || mc.AddCommentCalls[0].Text != "short form" {
		t.Fatalf("AddComment calls = %+v, want one call with text %q", mc.AddCommentCalls, "short form")
	}
}

// TestCloseWithReason_CommentErrorAbortsClose proves the reason is never
// silently lost: if recording the reason fails (non-escalation), the close is
// aborted (CloseBead is NOT called) and the error surfaces. This also pins the
// ordering contract — the comment is written BEFORE the close, so a comment
// failure gates the close.
func TestCloseWithReason_CommentErrorAbortsClose(t *testing.T) {
	mc := newMockClient()
	mc.CommentErr = errors.New("daemon unavailable")
	withMockClient(t, mc)

	_, err := execClose(t, "gd-c3u76usvml", "--reason", "must not be lost")
	if err == nil {
		t.Fatal("close with a failing AddComment returned nil; want the comment error surfaced")
	}
	if !strings.Contains(err.Error(), "recording reason") {
		t.Errorf("error = %q; want it to mention \"recording reason\"", err.Error())
	}
	if len(mc.CloseBeadCalls) != 0 {
		t.Errorf("CloseBead called %d times after a failed reason comment; want 0 (reason must not be silently dropped)", len(mc.CloseBeadCalls))
	}
}

// TestCloseWithReason_EscalationRecordsReasonFirst locks the headline ordering
// contract the code comment promises: when the CLOSE escalates (CloseBead
// returns *client.EscalationCreated), the reason comment must ALREADY have been
// recorded, the command must exit 0, and it must print the escalation notice
// rather than erroring. This proves the reason "survives even if the close later
// escalates".
func TestCloseWithReason_EscalationRecordsReasonFirst(t *testing.T) {
	mc := newMockClient()
	mc.CloseErrs["gd-c3u76usvml"] = &client.EscalationCreated{
		EscalationID: "gd-egqpg0fc45",
		Message:      "write denied; escalatable",
		DeniedAction: "close",
		BeadType:     "task",
	}
	withMockClient(t, mc)

	// The escalation notice is printed via fmt.Printf → os.Stdout (matching
	// create.go/update.go), so capture os.Stdout, not the cobra buffer.
	var err error
	out := captureStdout(t, func() {
		_, err = execClose(t, "gd-c3u76usvml", "--reason", "why it closed")
	})
	if err != nil {
		t.Fatalf("close that escalates should exit 0 (notice printed), got err: %v\n%s", err, out)
	}
	if len(mc.AddCommentCalls) != 1 || mc.AddCommentCalls[0].Text != "why it closed" {
		t.Fatalf("reason must be recorded BEFORE the escalating close; AddComment calls = %+v", mc.AddCommentCalls)
	}
	if len(mc.CloseBeadCalls) != 1 {
		t.Fatalf("CloseBead should be attempted once (and escalate), got %d calls", len(mc.CloseBeadCalls))
	}
	if !strings.Contains(out, "Escalation created: gd-egqpg0fc45") {
		t.Errorf("output did not print the escalation notice:\n%s", out)
	}
}

// TestCloseWithReason_CommentEscalationSkipsClose covers finding #1 from the
// review (ka-0000007746): when the REASON COMMENT itself escalates (AddComment
// returns *client.EscalationCreated, e.g. `--escalate` on a policy-gated type),
// the command must print the escalation notice and SKIP the close for that id —
// not treat it as a fatal error and not file a second escalation for the close.
func TestCloseWithReason_CommentEscalationSkipsClose(t *testing.T) {
	mc := newMockClient()
	mc.CommentErrs["gd-c3u76usvml"] = &client.EscalationCreated{
		EscalationID: "gd-e9nf2apaxw",
		Message:      "comment write denied; escalatable",
		DeniedAction: "comment",
		BeadType:     "decision",
	}
	withMockClient(t, mc)

	var err error
	out := captureStdout(t, func() {
		_, err = execClose(t, "gd-c3u76usvml", "--escalate", "--reason", "why")
	})
	if err != nil {
		t.Fatalf("a comment that escalates should exit 0 (notice printed), got err: %v\n%s", err, out)
	}
	if len(mc.AddCommentCalls) != 1 {
		t.Fatalf("AddComment should be attempted once, got %d", len(mc.AddCommentCalls))
	}
	if len(mc.CloseBeadCalls) != 0 {
		t.Errorf("CloseBead must be skipped when the reason comment escalates; got %d calls", len(mc.CloseBeadCalls))
	}
	if !strings.Contains(out, "Escalation created: gd-e9nf2apaxw") {
		t.Errorf("output did not print the comment-escalation notice:\n%s", out)
	}
}

// TestCloseWithReason_MultiID_PartialCommentFailure documents the non-atomic
// multi-id behavior when a LATER id's reason comment fails: the earlier id got
// its comment+close, the failing id gets neither its close nor any later id
// processed, and the error names the failing id so the reason is never silently
// dropped.
func TestCloseWithReason_MultiID_PartialCommentFailure(t *testing.T) {
	mc := newMockClient()
	mc.CommentErrs["gd-kmmgekjlko"] = errors.New("daemon 500")
	withMockClient(t, mc)

	_, err := execClose(t, "gd-ey0quy71sg", "gd-kmmgekjlko", "gd-ttgypjvjqi", "--reason", "batch")
	if err == nil {
		t.Fatal("multi-id close with a failing middle comment returned nil; want the error surfaced")
	}
	if !strings.Contains(err.Error(), "recording reason on gd-kmmgekjlko") {
		t.Errorf("error = %q; want it to name the failing id gd-kmmgekjlko", err.Error())
	}
	// A got comment + close; B got its (failed) comment attempt but no close; C never reached.
	if len(mc.AddCommentCalls) != 2 {
		t.Errorf("AddComment calls = %d, want 2 (A ok, B failed, C never reached)", len(mc.AddCommentCalls))
	}
	if len(mc.CloseBeadCalls) != 1 || mc.CloseBeadCalls[0].ID != "gd-ey0quy71sg" {
		t.Errorf("CloseBead calls = %+v, want only gd-ey0quy71sg (loop aborts on B's comment failure)", mc.CloseBeadCalls)
	}
}

// TestCloseCmd_ReasonFlagContract operates on the REAL closeCmd to lock the
// wiring: the --reason flag with -r shorthand must exist, and the --comment
// alias must normalize to it. Guards against a future edit dropping the flag or
// the normalizer.
func TestCloseCmd_ReasonFlagContract(t *testing.T) {
	f := closeCmd.Flags().Lookup("reason")
	if f == nil {
		t.Fatal("closeCmd has no --reason flag — the closure-reason affordance was lost")
	}
	if f.Shorthand != "r" {
		t.Errorf("--reason shorthand = %q, want \"r\"", f.Shorthand)
	}
	// The normalize func must fold --comment onto --reason.
	norm := closeCmd.Flags().GetNormalizeFunc()
	if got := string(norm(closeCmd.Flags(), "comment")); got != "reason" {
		t.Errorf("normalize(\"comment\") = %q, want \"reason\" (--comment must alias --reason)", got)
	}
	// A non-aliased flag name must pass through unchanged.
	if got := string(norm(closeCmd.Flags(), "escalate")); got != "escalate" {
		t.Errorf("normalize(\"escalate\") = %q, want \"escalate\" (only --comment should be folded)", got)
	}
}

// TestCloseCmd_AdvertisesReason pins that the closeCmd help/usage advertises the
// affordance (so both `-h` and the unknown-flag usage dump surface it), closing
// the discoverability gap that made agents guess the flag.
func TestCloseCmd_AdvertisesReason(t *testing.T) {
	if !strings.Contains(closeCmd.Example, "--reason") {
		t.Errorf("closeCmd.Example does not advertise --reason:\n%s", closeCmd.Example)
	}
}
