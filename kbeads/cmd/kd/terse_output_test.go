package main

import (
	"bytes"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// terse_output_test.go pins the ka-0000005647 fix: single-id claim/close/done
// and comment-add print a terse ONE-LINE success by default instead of a full
// bead-table dump, so agents stop reflexively masking output with
// `2>/dev/null`/`tail` (the error-masking habit that silently ate a glab
// failure in the audited session). Structured output stays available via --json.

// --- kd close ---

// TestClose_SingleID_TerseOutput proves a single-id close prints exactly
// "Closed <id>" and does NOT dump the bead table (no Status:/Type:/Kind: rows).
func TestClose_SingleID_TerseOutput(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	var err error
	out := captureStdout(t, func() {
		_, err = execClose(t, "gd-c3u76usvml")
	})
	if err != nil {
		t.Fatalf("bare `close gd-c3u76usvml` errored: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "Closed gd-c3u76usvml" {
		t.Errorf("want terse 'Closed gd-c3u76usvml', got:\n%q", out)
	}
	for _, tableField := range []string{"Status:", "Type:", "Kind:", "Priority:"} {
		if strings.Contains(out, tableField) {
			t.Errorf("single-id close must not dump the bead table (found %q), got:\n%s", tableField, out)
		}
	}
}

// TestClose_MultiID_StillTerse guards that the already-terse multi-id path is
// unchanged: one "Closed <id>" line per bead.
func TestClose_MultiID_StillTerse(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	var err error
	out := captureStdout(t, func() {
		_, err = execClose(t, "gd-ey0quy71sg", "gd-kmmgekjlko")
	})
	if err != nil {
		t.Fatalf("multi-id close errored: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Closed gd-ey0quy71sg") || !strings.Contains(out, "Closed gd-kmmgekjlko") {
		t.Errorf("multi-id close should print one terse line per bead, got:\n%s", out)
	}
}

// --- kd done ---

// TestDone_SingleID_TerseOutput proves a single-id done prints exactly
// "Done <id>" and does NOT dump the bead table.
func TestDone_SingleID_TerseOutput(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	var err error
	out := captureStdout(t, func() {
		_, err = execDone(t, "gd-c3u76usvml")
	})
	if err != nil {
		t.Fatalf("bare `done gd-c3u76usvml` errored: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "Done gd-c3u76usvml" {
		t.Errorf("want terse 'Done gd-c3u76usvml', got:\n%q", out)
	}
	if strings.Contains(out, "Status:") || strings.Contains(out, "Type:") {
		t.Errorf("single-id done must not dump the bead table, got:\n%s", out)
	}
}

// --- kd claim ---

// newClaimCmdForTest builds a STANDALONE copy of claimCmd reusing the REAL RunE,
// parentless (see newCloseCmdForTest for the rationale). The caller sets
// claimForce so the pre-claim network checks are skipped and the test isolates
// the success-output path.
func newClaimCmdForTest() *cobra.Command {
	return &cobra.Command{
		Use:          "claim <id>",
		Args:         cobra.ExactArgs(1),
		RunE:         claimCmd.RunE,
		SilenceUsage: true,
	}
}

func execClaim(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := newClaimCmdForTest()
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs(args)
	err := c.Execute()
	return buf.String(), err
}

// TestClaim_SingleID_TerseOutput proves claim prints "Claimed <id> <title>" on
// one line (title truncated) and does NOT dump the bead table.
func TestClaim_SingleID_TerseOutput(t *testing.T) {
	mc := newMockClient()
	// The claimed bead returned by UpdateBead — no targeting labels, so the
	// advice-diff path short-circuits without a network call.
	mc.Beads["gd-c3u76usvml"] = &model.Bead{
		ID:     "gd-c3u76usvml",
		Title:  "Refactor auth middleware",
		Type:   model.TypeTask,
		Status: model.StatusInProgress,
	}
	withMockClient(t, mc)

	// Skip the pre-claim checks so the test exercises only the output path.
	origForce := claimForce
	claimForce = true
	t.Cleanup(func() { claimForce = origForce })

	var err error
	out := captureStdout(t, func() {
		_, err = execClaim(t, "gd-c3u76usvml")
	})
	if err != nil {
		t.Fatalf("`claim gd-c3u76usvml` errored: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "Claimed gd-c3u76usvml Refactor auth middleware" {
		t.Errorf("want terse 'Claimed gd-c3u76usvml Refactor auth middleware', got:\n%q", out)
	}
	for _, tableField := range []string{"Status:", "Type:", "Kind:", "Assignee:"} {
		if strings.Contains(out, tableField) {
			t.Errorf("claim must not dump the bead table (found %q), got:\n%s", tableField, out)
		}
	}
}

// TestClaim_TruncatesLongTitle proves an over-40-char title is truncated in the
// terse line so the output stays a single scannable line.
func TestClaim_TruncatesLongTitle(t *testing.T) {
	mc := newMockClient()
	longTitle := strings.Repeat("x", 60)
	mc.Beads["gd-c3u76usvml"] = &model.Bead{
		ID:     "gd-c3u76usvml",
		Title:  longTitle,
		Type:   model.TypeTask,
		Status: model.StatusInProgress,
	}
	withMockClient(t, mc)

	origForce := claimForce
	claimForce = true
	t.Cleanup(func() { claimForce = origForce })

	var err error
	out := captureStdout(t, func() {
		_, err = execClaim(t, "gd-c3u76usvml")
	})
	if err != nil {
		t.Fatalf("claim errored: %v\n%s", err, out)
	}
	if strings.Contains(out, longTitle) {
		t.Errorf("long title should be truncated in the terse line, got:\n%s", out)
	}
	if !strings.Contains(out, "...") {
		t.Errorf("truncated title should carry an ellipsis, got:\n%s", out)
	}
}

// --- kd comment add ---

// TestCommentAdd_TerseOutput proves comment-add prints the one-line
// "Comment <id> added to <bead>" form, not the multi-field record.
func TestCommentAdd_TerseOutput(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	var err error
	out := captureStdout(t, func() {
		_, err = execComment(t, "gd-c3u76usvml", "looks good")
	})
	if err != nil {
		t.Fatalf("`comment gd-c3u76usvml looks good` errored: %v\n%s", err, out)
	}
	if !strings.Contains(out, "added to gd-c3u76usvml") {
		t.Errorf("want terse 'Comment <id> added to gd-c3u76usvml', got:\n%q", out)
	}
	for _, tableField := range []string{"Author:", "Text:", "Bead:"} {
		if strings.Contains(out, tableField) {
			t.Errorf("comment add must not dump the multi-field record (found %q), got:\n%s", tableField, out)
		}
	}
}
