package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// ka-0000001542: `kd show` caps comment display to the last
// commentDisplayLimit by default. Long-lived schedule and epic beads
// accumulate hundreds of comments and dump tens of KB into agent
// terminals; the cap protects future agents from historical bloat.
// --json keeps emitting the full list (preserves machine consumers).

func makeComments(n int) []*model.Comment {
	out := make([]*model.Comment, n)
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := range n {
		out[i] = &model.Comment{
			Author:    fmt.Sprintf("u%03d", i),
			Text:      fmt.Sprintf("comment-%03d", i),
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
	}
	return out
}

func TestPrintShowComments_BelowLimit_RendersAll(t *testing.T) {
	var b strings.Builder
	printShowComments(&b, makeComments(5), false)
	got := b.String()

	for i := range 5 {
		want := fmt.Sprintf("comment-%03d", i)
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
	if strings.Contains(got, "older comments suppressed") {
		t.Errorf("footer must not appear when count <= limit:\n%s", got)
	}
}

func TestPrintShowComments_AboveLimit_DefaultCapsToLastN(t *testing.T) {
	total := commentDisplayLimit + 17
	var b strings.Builder
	printShowComments(&b, makeComments(total), false)
	got := b.String()

	// Footer must announce the suppressed count and point to --all-comments.
	wantFooter := fmt.Sprintf("… %d older comments suppressed. Use --all-comments to view full history.", total-commentDisplayLimit)
	if !strings.Contains(got, wantFooter) {
		t.Errorf("missing suppressed footer %q:\n%s", wantFooter, got)
	}

	// The last commentDisplayLimit entries are present, the earlier ones absent.
	for i := total - commentDisplayLimit; i < total; i++ {
		want := fmt.Sprintf("comment-%03d", i)
		if !strings.Contains(got, want) {
			t.Errorf("missing recent comment %q:\n%s", want, got)
		}
	}
	for i := range total - commentDisplayLimit {
		banned := fmt.Sprintf("comment-%03d ", i) // trailing space disambiguates 001 vs 0010
		if strings.Contains(got, banned) {
			t.Errorf("default capped output unexpectedly contains old %q:\n%s", banned, got)
		}
	}
}

func TestPrintShowComments_AllOverride_RendersFullHistory(t *testing.T) {
	total := commentDisplayLimit + 5
	var b strings.Builder
	printShowComments(&b, makeComments(total), true)
	got := b.String()

	if strings.Contains(got, "older comments suppressed") {
		t.Errorf("--all-comments must not print suppressed footer:\n%s", got)
	}
	for i := range total {
		want := fmt.Sprintf("comment-%03d", i)
		if !strings.Contains(got, want) {
			t.Errorf("--all-comments missing %q:\n%s", want, got)
		}
	}
}

func TestPrintShowComments_Empty_NoOutput(t *testing.T) {
	var b strings.Builder
	printShowComments(&b, nil, false)
	if b.String() != "" {
		t.Errorf("expected no output for empty comments, got %q", b.String())
	}
}

// salvageText builds a realistic involuntary-termination salvage notice with
// the stable recognizer prefix plus a variable tail (worktree count, S3 path,
// agent id) so the fold matches on prefix, not exact equality.
func salvageText(agentID string, worktrees int) string {
	return fmt.Sprintf("Killed mid-work (involuntary_termination). Uncommitted WIP for %d worktree(s) salvaged to s3://bucket/salvage/%s\nRecover with:\n```\ngb salvage restore --agent-bead %s\n```", worktrees, agentID, agentID)
}

// ka-0000003686 / ka-0000007338: a flapping loop's plan bead accumulates a run
// of identical involuntary_termination salvage notices as its MOST-RECENT
// comments. A count-based cap spends its whole window on that flood and buries
// older signal comments. The fold collapses the run to one synthetic line
// BEFORE the cap, so signal survives; --all-comments still shows every raw notice.
func TestPrintShowComments_FoldsSalvageRun(t *testing.T) {
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	mk := func(offset int, author, text string) *model.Comment {
		return &model.Comment{Author: author, Text: text, CreatedAt: base.Add(time.Duration(offset) * time.Minute)}
	}

	// 2 signal comments, then a run of 5 salvage notices (most recent).
	comments := []*model.Comment{
		mk(0, "matt", "STACK TIP: pick up at kbeads/cmd/kd/show.go"),
		mk(1, "matt", "acceptance evidence: go test green"),
	}
	for i := range 5 {
		comments = append(comments, mk(2+i, "controller", salvageText(fmt.Sprintf("gd-agent%03d", i), i+1)))
	}

	var b strings.Builder
	printShowComments(&b, comments, false)
	got := b.String()

	// The salvage run renders as exactly one folded line naming the count.
	if strings.Count(got, "involuntary_termination salvage notices") != 1 {
		t.Errorf("expected exactly one folded salvage line, output:\n%s", got)
	}
	if !strings.Contains(got, "5× involuntary_termination salvage notices (kd show --all-comments to expand)") {
		t.Errorf("folded line missing count/hint, output:\n%s", got)
	}
	// The raw salvage recover-command must NOT appear in folded output.
	if strings.Contains(got, "gb salvage restore") {
		t.Errorf("folded output must not contain raw salvage bodies, output:\n%s", got)
	}
	// Both signal comments survive the fold.
	if !strings.Contains(got, "STACK TIP: pick up at") {
		t.Errorf("signal comment 1 buried by salvage flood, output:\n%s", got)
	}
	if !strings.Contains(got, "acceptance evidence: go test green") {
		t.Errorf("signal comment 2 buried by salvage flood, output:\n%s", got)
	}
	if strings.Contains(got, "older comments suppressed") {
		t.Errorf("folded count is under the cap; no footer expected, output:\n%s", got)
	}
}

// A single salvage notice is NOT folded (K>=2 required) — one machine notice
// carries real signal and should render verbatim.
func TestPrintShowComments_SingleSalvageNotFolded(t *testing.T) {
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	comments := []*model.Comment{
		{Author: "matt", Text: "signal", CreatedAt: base},
		{Author: "controller", Text: salvageText("gd-agentXYZ", 1), CreatedAt: base.Add(time.Minute)},
	}
	var b strings.Builder
	printShowComments(&b, comments, false)
	got := b.String()

	if strings.Contains(got, "involuntary_termination salvage notices") {
		t.Errorf("single salvage notice must not fold, output:\n%s", got)
	}
	if !strings.Contains(got, "gb salvage restore --agent-bead gd-agentXYZ") {
		t.Errorf("single salvage notice must render raw, output:\n%s", got)
	}
}

// The fold protects signal that a count-based cap would otherwise bury: with a
// salvage run larger than the display window, the folded line keeps every
// signal comment visible instead of being pushed out by the flood.
func TestPrintShowComments_FoldRescuesSignalUnderCap(t *testing.T) {
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	mk := func(offset int, author, text string) *model.Comment {
		return &model.Comment{Author: author, Text: text, CreatedAt: base.Add(time.Duration(offset) * time.Minute)}
	}
	// One signal comment first, then a salvage run longer than the cap.
	comments := []*model.Comment{mk(0, "matt", "STACK TIP survive me")}
	runLen := commentDisplayLimit + 10
	for i := range runLen {
		comments = append(comments, mk(1+i, "controller", salvageText(fmt.Sprintf("gd-a%03d", i), 1)))
	}

	var b strings.Builder
	printShowComments(&b, comments, false)
	got := b.String()

	if !strings.Contains(got, "STACK TIP survive me") {
		t.Errorf("fold failed to rescue signal below a >cap salvage run, output:\n%s", got)
	}
	if !strings.Contains(got, fmt.Sprintf("%d× involuntary_termination salvage notices", runLen)) {
		t.Errorf("expected the whole run folded into one line, output:\n%s", got)
	}
	// Folded to 2 lines total (signal + one folded line) — well under the cap,
	// so no suppressed footer.
	if strings.Contains(got, "older comments suppressed") {
		t.Errorf("folded output should be under cap; no footer, output:\n%s", got)
	}
}

// --all-comments bypasses the fold: every raw salvage notice is rendered.
func TestPrintShowComments_AllComments_NoFold(t *testing.T) {
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	comments := []*model.Comment{{Author: "matt", Text: "signal", CreatedAt: base}}
	for i := range 5 {
		comments = append(comments, &model.Comment{
			Author:    "controller",
			Text:      salvageText(fmt.Sprintf("gd-agent%03d", i), 1),
			CreatedAt: base.Add(time.Duration(i+1) * time.Minute),
		})
	}
	var b strings.Builder
	printShowComments(&b, comments, true)
	got := b.String()

	if strings.Contains(got, "involuntary_termination salvage notices") {
		t.Errorf("--all-comments must not fold, output:\n%s", got)
	}
	// All 5 raw recover commands present.
	for i := range 5 {
		want := fmt.Sprintf("gb salvage restore --agent-bead gd-agent%03d", i)
		if !strings.Contains(got, want) {
			t.Errorf("--all-comments missing raw salvage notice %q, output:\n%s", want, got)
		}
	}
}

// TestShowCmd_AllCommentsFlag exercises the flag end-to-end through RunE so
// the wiring (flag → printShowComments) is locked in; the byte-level format
// is covered by the helper tests above.
func TestShowCmd_AllCommentsFlag(t *testing.T) {
	mc := newMockClient()
	beadID := "gd-OeAdOS58ss"
	mc.Beads[beadID] = &model.Bead{
		ID:       beadID,
		Title:    "cap test",
		Status:   model.StatusOpen,
		Comments: makeComments(commentDisplayLimit + 9),
	}

	prevClient := beadsClient
	prevAll := showAllComments
	prevTTY := isStdoutTTY
	prevOnly := showMentionsOnly
	t.Cleanup(func() {
		beadsClient = prevClient
		showAllComments = prevAll
		isStdoutTTY = prevTTY
		showMentionsOnly = prevOnly
	})
	beadsClient = mc
	isStdoutTTY = func() bool { return false }
	showMentionsOnly = false

	// Default: cap engages.
	showAllComments = false
	got := captureShowStdout(t, func() {
		if err := showCmd.RunE(showCmd, []string{beadID}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if !strings.Contains(got, "older comments suppressed") {
		t.Errorf("default kd show should cap comments; output:\n%s", got)
	}
	if !strings.Contains(got, "Use --all-comments to view full history.") {
		t.Errorf("footer should reference --all-comments; output:\n%s", got)
	}

	// --all-comments: full history, no footer.
	showAllComments = true
	got = captureShowStdout(t, func() {
		if err := showCmd.RunE(showCmd, []string{beadID}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if strings.Contains(got, "older comments suppressed") {
		t.Errorf("--all-comments must not cap; output:\n%s", got)
	}
	if !strings.Contains(got, "comment-000") || !strings.Contains(got, fmt.Sprintf("comment-%03d", commentDisplayLimit+8)) {
		t.Errorf("--all-comments must render full range; output:\n%s", got)
	}
}
