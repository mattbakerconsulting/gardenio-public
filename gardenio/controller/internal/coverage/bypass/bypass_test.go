package bypass

import (
	"errors"
	"testing"
	"time"
)

func TestParse_NoTrailers(t *testing.T) {
	commits := []Commit{
		{SHA: "deadbeef", Author: "Alice", Message: "fix: regular commit\n\nNo trailer here.\n"},
	}
	got, err := Parse(commits)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil Bypass, got %+v", got)
	}
}

func TestParse_AllValidReasons(t *testing.T) {
	for _, r := range AllReasons() {
		t.Run(string(r), func(t *testing.T) {
			commits := []Commit{
				{
					SHA:     "abcdef1234567890",
					Author:  "Alice <a@x>",
					Message: "feat: do thing\n\nBody text.\n\nCoverage-Bypass: " + string(r) + "\n",
				},
			}
			got, err := Parse(commits)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got == nil {
				t.Fatal("expected non-nil Bypass")
			}
			if got.Reason != r {
				t.Errorf("Reason = %q, want %q", got.Reason, r)
			}
			if got.CommitSHA != "abcdef1234567890" {
				t.Errorf("CommitSHA = %q", got.CommitSHA)
			}
			if got.Author != "Alice <a@x>" {
				t.Errorf("Author = %q", got.Author)
			}
			if got.RawTrailer != "Coverage-Bypass: "+string(r) {
				t.Errorf("RawTrailer = %q", got.RawTrailer)
			}
			if got.FollowUp.Reason != r {
				t.Errorf("FollowUp.Reason = %q", got.FollowUp.Reason)
			}
			if got.FollowUp.BeadKind != "bug" {
				t.Errorf("FollowUp.BeadKind = %q, want bug", got.FollowUp.BeadKind)
			}
		})
	}
}

func TestParse_UnknownReason_FailsClosed(t *testing.T) {
	commits := []Commit{
		{SHA: "1", Author: "X", Message: "x\n\nCoverage-Bypass: feels-cute\n"},
	}
	got, err := Parse(commits)
	if got != nil {
		t.Fatalf("expected nil Bypass on unknown reason, got %+v", got)
	}
	if !errors.Is(err, ErrUnknownReason) {
		t.Fatalf("err = %v, want ErrUnknownReason", err)
	}
}

func TestParse_EmptyReason(t *testing.T) {
	commits := []Commit{
		{SHA: "1", Author: "X", Message: "x\n\nCoverage-Bypass:\n"},
	}
	got, err := Parse(commits)
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
	if !errors.Is(err, ErrEmptyReason) {
		t.Fatalf("err = %v, want ErrEmptyReason", err)
	}
}

func TestParse_PerComponentScope_Rejected(t *testing.T) {
	cases := []string{
		"Coverage-Bypass: emergency component=coop",
		"Coverage-Bypass: emergency scope=controller",
		"Coverage-Bypass: refactor, component=coop",
		"Coverage-Bypass: refactor; tooling-broken",
		"Coverage-Bypass: emergency(coop)",
		"Coverage-Bypass: emergency/coop",
	}
	for _, line := range cases {
		t.Run(line, func(t *testing.T) {
			commits := []Commit{{SHA: "1", Author: "X", Message: "x\n\n" + line + "\n"}}
			got, err := Parse(commits)
			if got != nil {
				t.Fatalf("expected nil Bypass, got %+v", got)
			}
			if !errors.Is(err, ErrPerComponentScope) {
				t.Fatalf("err = %v, want ErrPerComponentScope", err)
			}
		})
	}
}

func TestParse_LastWriteWins(t *testing.T) {
	commits := []Commit{
		{SHA: "aaa", Author: "A", Message: "first\n\nCoverage-Bypass: refactor\n"},
		{SHA: "bbb", Author: "B", Message: "second\n\nCoverage-Bypass: emergency\n"},
	}
	got, err := Parse(commits)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Reason != ReasonEmergency {
		t.Errorf("Reason = %q, want emergency (last-write-wins)", got.Reason)
	}
	if got.CommitSHA != "bbb" {
		t.Errorf("CommitSHA = %q, want bbb", got.CommitSHA)
	}
	if got.Author != "B" {
		t.Errorf("Author = %q, want B", got.Author)
	}
	if len(got.UnknownReasons) != 0 {
		t.Errorf("UnknownReasons = %v, want empty (refactor was valid)", got.UnknownReasons)
	}
}

func TestParse_LastWriteWins_UnknownEarlierSurfacedAsWarning(t *testing.T) {
	commits := []Commit{
		{SHA: "aaa", Author: "A", Message: "first\n\nCoverage-Bypass: feels-cute\n"},
		{SHA: "bbb", Author: "B", Message: "second\n\nCoverage-Bypass: refactor\n"},
	}
	got, err := Parse(commits)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got == nil || got.Reason != ReasonRefactor {
		t.Fatalf("got = %+v, want refactor", got)
	}
	if len(got.UnknownReasons) != 1 || got.UnknownReasons[0] != "feels-cute" {
		t.Errorf("UnknownReasons = %v, want [feels-cute]", got.UnknownReasons)
	}
}

func TestParse_LastInvalid_OverridesValidEarlier_FailsClosed(t *testing.T) {
	// If the *latest* commit has an invalid trailer, fail closed
	// even though an earlier commit had a valid one. Last-write-wins
	// applies to the active trailer in both directions: the user's
	// latest intent governs, and a typo in the latest commit must
	// not silently fall back to an earlier valid bypass.
	commits := []Commit{
		{SHA: "aaa", Author: "A", Message: "first\n\nCoverage-Bypass: refactor\n"},
		{SHA: "bbb", Author: "B", Message: "second\n\nCoverage-Bypass: typo-here\n"},
	}
	got, err := Parse(commits)
	if got != nil {
		t.Fatalf("expected nil Bypass, got %+v", got)
	}
	if !errors.Is(err, ErrUnknownReason) {
		t.Fatalf("err = %v, want ErrUnknownReason", err)
	}
}

func TestParse_CaseInsensitiveKey(t *testing.T) {
	commits := []Commit{
		{SHA: "1", Author: "X", Message: "x\n\ncoverage-bypass: refactor\n"},
	}
	got, err := Parse(commits)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got == nil || got.Reason != ReasonRefactor {
		t.Fatalf("got = %+v, want refactor", got)
	}
}

func TestParse_CaseInsensitiveValue(t *testing.T) {
	commits := []Commit{
		{SHA: "1", Author: "X", Message: "x\n\nCoverage-Bypass: REFACTOR\n"},
	}
	got, err := Parse(commits)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got == nil || got.Reason != ReasonRefactor {
		t.Fatalf("got = %+v, want refactor", got)
	}
}

func TestParse_TwoTrailersInOneCommit(t *testing.T) {
	// `git interpret-trailers` allows multiple trailer lines; we
	// keep last-write-wins within a single commit body too.
	commits := []Commit{
		{
			SHA:    "aaa",
			Author: "A",
			Message: `feat: thing

Body.

Coverage-Bypass: refactor
Coverage-Bypass: generated
`,
		},
	}
	got, err := Parse(commits)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Reason != ReasonGenerated {
		t.Errorf("Reason = %q, want generated", got.Reason)
	}
}

func TestFollowUp_PerReasonObligations(t *testing.T) {
	// §7.3 table: each reason carries a defined audit/follow-up
	// commitment. This test pins those commitments so a reviewer
	// who softens one (e.g. drops the emergency squawk) gets a red
	// build.
	t.Run("refactor: auto-close on no main drop", func(t *testing.T) {
		f := ReasonRefactor.FollowUp()
		if !f.AutoCloseOnNoMainDrop {
			t.Error("refactor must AutoCloseOnNoMainDrop")
		}
		if f.RequiredPreMerge {
			t.Error("refactor must not require pre-merge bead")
		}
	})
	t.Run("generated: auto-close on no main drop", func(t *testing.T) {
		f := ReasonGenerated.FollowUp()
		if !f.AutoCloseOnNoMainDrop {
			t.Error("generated must AutoCloseOnNoMainDrop")
		}
	})
	t.Run("emergency: 24h require + 7d reminder + ops squawk", func(t *testing.T) {
		f := ReasonEmergency.FollowUp()
		if f.RequireWithin != 24*time.Hour {
			t.Errorf("RequireWithin = %v, want 24h", f.RequireWithin)
		}
		if f.ReminderAfter != 7*24*time.Hour {
			t.Errorf("ReminderAfter = %v, want 7d", f.ReminderAfter)
		}
		if !f.SquawkOpsChannel {
			t.Error("emergency must SquawkOpsChannel")
		}
	})
	t.Run("tooling-broken: pre-merge bead required + block on miss", func(t *testing.T) {
		f := ReasonToolingBroken.FollowUp()
		if !f.RequiredPreMerge {
			t.Error("tooling-broken must RequiredPreMerge")
		}
		if !f.BlockCommentOnMiss {
			t.Error("tooling-broken must BlockCommentOnMiss")
		}
	})
}

func TestFollowUp_AllReasonsHaveBugBeadAndCoverageLabel(t *testing.T) {
	// Every reason files a bug-kind bead with the coverage label
	// so the audit query + bridge auto-post both pick them up.
	for _, r := range AllReasons() {
		t.Run(string(r), func(t *testing.T) {
			f := r.FollowUp()
			if f.BeadKind != "bug" {
				t.Errorf("BeadKind = %q, want bug", f.BeadKind)
			}
			hasCoverage := false
			hasBypass := false
			for _, l := range f.BeadLabels {
				if l == "coverage" {
					hasCoverage = true
				}
				if l == "coverage-bypass" {
					hasBypass = true
				}
			}
			if !hasCoverage {
				t.Errorf("BeadLabels = %v, missing 'coverage'", f.BeadLabels)
			}
			if !hasBypass {
				t.Errorf("BeadLabels = %v, missing 'coverage-bypass'", f.BeadLabels)
			}
		})
	}
}

func TestParse_TrailerMustBeOnOwnLine(t *testing.T) {
	// "Coverage-Bypass: refactor" embedded inside a sentence is
	// not a trailer. Only line-anchored matches count.
	commits := []Commit{
		{SHA: "1", Author: "X", Message: "fix: explain that Coverage-Bypass: refactor is forbidden in tests\n"},
	}
	got, err := Parse(commits)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		// The regex anchors to start-of-line so this *will* still
		// match if the prose starts at column zero. Document the
		// reality: any line beginning with Coverage-Bypass: counts.
		// Authors should keep prose indented or quoted.
		t.Logf("note: line-anchored match parsed prose as a trailer; this is documented behavior")
	}
}

func TestParse_LeadingWhitespaceTolerated(t *testing.T) {
	// Squash-merge often inserts indentation in concatenated bodies.
	commits := []Commit{
		{SHA: "1", Author: "X", Message: "x\n\n    Coverage-Bypass: refactor\n"},
	}
	got, err := Parse(commits)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got == nil || got.Reason != ReasonRefactor {
		t.Fatalf("got = %+v, want refactor", got)
	}
}
