package bridge

import "testing"

func TestIsProtectedTargetBranch(t *testing.T) {
	cases := []struct {
		branch string
		want   bool
	}{
		{"main", true},
		{"master", true},
		{"2.5.x", true},
		{"1.x", true},
		{"10.20.x", true},
		{" main ", true}, // trimmed
		{"feature/foo", false},
		{"fix/gd-abc", false},
		{"release-2.5", false}, // not the *.x convention
		{"maintenance", false}, // must not prefix-match "main"
		{"", false},
	}
	for _, c := range cases {
		if got := IsProtectedTargetBranch(c.branch); got != c.want {
			t.Errorf("IsProtectedTargetBranch(%q) = %v, want %v", c.branch, got, c.want)
		}
	}
}

func TestEvaluateTicketResolvedCloseGate(t *testing.T) {
	t.Run("protected target + sibling-MR resolution on different branch → NOT closed", func(t *testing.T) {
		// Repro of bug ka-0000002060: PE-491 had !1268 (2.5.x, merged) and
		// !1267 (main, open). Resolving via !1268 must NOT close !1267.
		in := TicketResolvedGateInput{
			MR: RelevanceMR{IID: 1267, TargetBranch: "main", State: "opened"},
			SiblingMRs: []RelevanceMR{
				{IID: 1268, TargetBranch: "2.5.x", State: "merged"},
			},
		}
		got := EvaluateTicketResolvedCloseGate(in)
		if got.MayClose {
			t.Fatalf("expected MayClose=false (protected target, sibling resolution), got true; reason=%q", got.Reason)
		}
		if got.BlockingSiblingIID != 1268 {
			t.Errorf("BlockingSiblingIID = %d, want 1268", got.BlockingSiblingIID)
		}
	})

	t.Run("release-branch target + sibling on main → NOT closed", func(t *testing.T) {
		// Symmetric case: the open MR targets a release branch and the ticket
		// was resolved by a merge to main.
		in := TicketResolvedGateInput{
			MR: RelevanceMR{IID: 900, TargetBranch: "2.5.x", State: "opened"},
			SiblingMRs: []RelevanceMR{
				{IID: 901, TargetBranch: "main", State: "merged"},
			},
		}
		got := EvaluateTicketResolvedCloseGate(in)
		if got.MayClose {
			t.Fatalf("expected MayClose=false, got true; reason=%q", got.Reason)
		}
		if got.BlockingSiblingIID != 901 {
			t.Errorf("BlockingSiblingIID = %d, want 901", got.BlockingSiblingIID)
		}
	})

	t.Run("protected target + same-branch resolution → still closed", func(t *testing.T) {
		// The fix already shipped to this MR's own target branch via a sibling
		// merge — closing is correct.
		in := TicketResolvedGateInput{
			MR: RelevanceMR{IID: 1267, TargetBranch: "main", State: "opened"},
			SiblingMRs: []RelevanceMR{
				{IID: 1300, TargetBranch: "main", State: "merged"},
			},
		}
		got := EvaluateTicketResolvedCloseGate(in)
		if !got.MayClose {
			t.Fatalf("expected MayClose=true (same-branch resolution), got false; reason=%q", got.Reason)
		}
		if got.BlockingSiblingIID != 0 {
			t.Errorf("BlockingSiblingIID = %d, want 0", got.BlockingSiblingIID)
		}
	})

	t.Run("protected target + no sibling MR (non-sibling resolution, e.g. Won't-Fix) → still closed", func(t *testing.T) {
		in := TicketResolvedGateInput{
			MR:         RelevanceMR{IID: 1267, TargetBranch: "main", State: "opened"},
			SiblingMRs: nil,
		}
		got := EvaluateTicketResolvedCloseGate(in)
		if !got.MayClose {
			t.Fatalf("expected MayClose=true (no sibling resolution), got false; reason=%q", got.Reason)
		}
	})

	t.Run("non-protected target + sibling resolution → still closed (gate unaffected)", func(t *testing.T) {
		// A feature/topic-branch MR is never protected by this guard.
		in := TicketResolvedGateInput{
			MR: RelevanceMR{IID: 500, TargetBranch: "feature/foo", State: "opened"},
			SiblingMRs: []RelevanceMR{
				{IID: 501, TargetBranch: "main", State: "merged"},
			},
		}
		got := EvaluateTicketResolvedCloseGate(in)
		if !got.MayClose {
			t.Fatalf("expected MayClose=true (non-protected target), got false; reason=%q", got.Reason)
		}
	})

	t.Run("protected target + sibling on different branch but not merged → still closed", func(t *testing.T) {
		// An *open* sibling on another branch did not resolve the ticket, so it
		// does not block the gate.
		in := TicketResolvedGateInput{
			MR: RelevanceMR{IID: 1267, TargetBranch: "main", State: "opened"},
			SiblingMRs: []RelevanceMR{
				{IID: 1268, TargetBranch: "2.5.x", State: "opened"},
			},
		}
		got := EvaluateTicketResolvedCloseGate(in)
		if !got.MayClose {
			t.Fatalf("expected MayClose=true (sibling not merged), got false; reason=%q", got.Reason)
		}
	})

	t.Run("self in sibling list is ignored", func(t *testing.T) {
		// If the caller includes the MR itself in SiblingMRs, it must not be
		// treated as a blocking sibling even if (hypothetically) marked merged.
		in := TicketResolvedGateInput{
			MR: RelevanceMR{IID: 1267, TargetBranch: "main", State: "opened"},
			SiblingMRs: []RelevanceMR{
				{IID: 1267, TargetBranch: "main", State: "merged"},
			},
		}
		got := EvaluateTicketResolvedCloseGate(in)
		if !got.MayClose {
			t.Fatalf("expected MayClose=true (only self in sibling list), got false; reason=%q", got.Reason)
		}
	})
}
