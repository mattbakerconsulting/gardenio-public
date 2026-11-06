package advice

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestExtractSupersededIDs(t *testing.T) {
	tests := []struct {
		name        string
		description string
		want        []string
	}{
		{
			name:        "empty description",
			description: "",
			want:        nil,
		},
		{
			name:        "no supersession keyword",
			description: "This is advice referencing gd-abc but does not supersede.",
			want:        nil,
		},
		{
			name:        "natural prose with parenthesized gd-ID",
			description: "Foo bar. This supersedes the earlier advice (ka-0000004853) for the gardenio project.",
			want:        []string{"ka-0000004853"},
		},
		{
			name:        "typed-mention colon form",
			description: "Permissive override. supersedes: gd-AAA111",
			want:        []string{"gd-AAA111"},
		},
		{
			name:        "typed-mention paren form",
			description: "Override rule. supersedes(gd-BBB222)",
			want:        []string{"gd-BBB222"},
		},
		{
			name:        "supersede singular",
			description: "This advice supersede gd-CCC333 for our project.",
			want:        []string{"gd-CCC333"},
		},
		{
			name:        "superseded past tense",
			description: "Note: gd-DDD444 is superseded by this advice (gd-DDD444).",
			want:        []string{"gd-DDD444"},
		},
		{
			name:        "superseding gerund",
			description: "This advice is superseding gd-EEE555 going forward.",
			want:        []string{"gd-EEE555"},
		},
		{
			name:        "case insensitive",
			description: "SUPERSEDES gd-FFF666.",
			want:        []string{"gd-FFF666"},
		},
		{
			name:        "multiple supersession edges",
			description: "This supersedes gd-GGG777. Also supersedes gd-HHH888 in newer contexts.",
			want:        []string{"gd-GGG777", "gd-HHH888"},
		},
		{
			name:        "deduplicates repeated targets",
			description: "Supersedes gd-III999. The gd-III999 advice was wrong. Supersedes gd-III999 again.",
			want:        []string{"gd-III999"},
		},
		{
			name:        "sentence boundary stops mis-attribution",
			description: "This rule supersedes the old way of doing things. Unrelated: see gd-UNRELATED for context.",
			want:        nil,
		},
		{
			name:        "no supersession when gd-ID is far past sentence end",
			description: "Supersedes the legacy advice. The new rule replaces it. See gd-FAR000.",
			want:        nil,
		},
		{
			name:        "ignores the word in unrelated contexts",
			description: "We previously had thought about supersession but never committed; see gd-ZZZ999.",
			want:        nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractSupersededIDs(&beadsapi.BeadDetail{Description: tt.description})
			if !stringSlicesEqual(got, tt.want) {
				t.Errorf("ExtractSupersededIDs(%q) = %v, want %v", tt.description, got, tt.want)
			}
		})
	}
}

func TestExtractSupersededIDs_NilBead(t *testing.T) {
	if got := ExtractSupersededIDs(nil); got != nil {
		t.Errorf("ExtractSupersededIDs(nil) = %v, want nil", got)
	}
}

// TestApplySupersessions_BothFitBudget covers acceptance criterion (a):
// when a project advice supersedes a global advice and both are matched,
// the superseder wins precedence — the superseded global advice is dropped
// from the rendered list. Budget pressure is irrelevant; supersession is
// applied during selection, upstream of any byte budget.
func TestApplySupersessions_BothFitBudget(t *testing.T) {
	global := MatchedAdvice{
		Bead: &beadsapi.BeadDetail{
			ID:          "ka-0000004853",
			Title:       "Agents may self-merge when operator authorizes",
			Description: "Restrictive global advice — wait for explicit authorization.",
			Labels:      []string{"global"},
		},
		Scope:       "global",
		ScopeHeader: "Global",
	}
	project := MatchedAdvice{
		Bead: &beadsapi.BeadDetail{
			ID:          "ka-0000002230",
			Title:       "Agents may self-merge gardenio MRs without asking",
			Description: "Permissive project override. This supersedes the earlier advice (ka-0000004853) for the gardenio project.",
			Labels:      []string{"project:gardenio"},
		},
		Scope:       "project",
		ScopeTarget: "gardenio",
		ScopeHeader: "Project: gardenio",
	}

	got := ApplySupersessions([]MatchedAdvice{global, project})

	if len(got) != 1 {
		t.Fatalf("expected 1 advice after supersession, got %d (%+v)", len(got), got)
	}
	if got[0].Bead.ID != "ka-0000002230" {
		t.Errorf("expected superseder ka-0000002230 to remain, got %s", got[0].Bead.ID)
	}
}

// TestApplySupersessions_ProjectUnderBudgetCutoff covers acceptance criterion (b):
// when project advice supersedes a global advice and the project advice would
// otherwise be dropped under budget pressure (low priority/confidence), the
// supersession filter drops the superseded global instead. Downstream the
// project advice survives the byte budget because the global it replaces is
// no longer competing for budget slots.
//
// We assert the filter's contract — superseded global is gone, project
// superseder remains — independent of any downstream budget cap. The bug's
// observed failure mode was that BOTH the restrictive global AND the project
// override would compete for budget, with the project override losing on a
// priority tiebreak. Dropping the superseded global removes the competition.
func TestApplySupersessions_ProjectUnderBudgetCutoff(t *testing.T) {
	// Simulate confidence/priority asymmetry as in the bug: global high,
	// project low. Supersession must still drop the global so the project
	// override is what reaches the budget gate.
	global := MatchedAdvice{
		Bead: &beadsapi.BeadDetail{
			ID:          "ka-0000004853",
			Title:       "Restrictive global",
			Description: "Strict global rule.",
			Labels:      []string{"global"},
			Fields:      map[string]string{"confidence": "0.9"},
		},
		Scope:       "global",
		ScopeHeader: "Global",
	}
	project := MatchedAdvice{
		Bead: &beadsapi.BeadDetail{
			ID:          "ka-0000002230",
			Title:       "Project override (low priority)",
			Description: "Project-specific rule. This supersedes the earlier advice (ka-0000004853) for the gardenio project.",
			Labels:      []string{"project:gardenio"},
			Fields:      map[string]string{"confidence": "0.3"},
		},
		Scope:       "project",
		ScopeTarget: "gardenio",
		ScopeHeader: "Project: gardenio",
	}

	got := ApplySupersessions([]MatchedAdvice{global, project})

	if len(got) != 1 {
		t.Fatalf("expected superseded global dropped, got %d items: %+v", len(got), got)
	}
	if got[0].Bead.ID != "ka-0000002230" {
		t.Errorf("expected project superseder to survive, got %s", got[0].Bead.ID)
	}
}

// TestApplySupersessions_NoSupersessionUnchanged covers acceptance criterion (c):
// when no advice references another via "supersedes", the matched list is
// returned untouched — existing behavior is preserved.
func TestApplySupersessions_NoSupersessionUnchanged(t *testing.T) {
	a := MatchedAdvice{Bead: &beadsapi.BeadDetail{ID: "gd-AAA", Description: "first piece of advice"}}
	b := MatchedAdvice{Bead: &beadsapi.BeadDetail{ID: "gd-BBB", Description: "second piece of advice"}}
	c := MatchedAdvice{Bead: &beadsapi.BeadDetail{ID: "gd-CCC", Description: "third piece of advice referencing gd-AAA for context"}}

	in := []MatchedAdvice{a, b, c}
	got := ApplySupersessions(in)

	if len(got) != 3 {
		t.Fatalf("expected all 3 advice items preserved, got %d", len(got))
	}
	for i, m := range got {
		if m.Bead.ID != in[i].Bead.ID {
			t.Errorf("order changed at index %d: got %s, want %s", i, m.Bead.ID, in[i].Bead.ID)
		}
	}
}

func TestApplySupersessions_SelfReferenceIgnored(t *testing.T) {
	// A bead that says "supersedes gd-X" where gd-X is its own ID must not
	// drop itself from the matched list.
	self := MatchedAdvice{
		Bead: &beadsapi.BeadDetail{
			ID:          "gd-SELF",
			Description: "This advice supersedes gd-SELF (self-reference; should be ignored).",
		},
	}
	got := ApplySupersessions([]MatchedAdvice{self})
	if len(got) != 1 || got[0].Bead.ID != "gd-SELF" {
		t.Errorf("self-reference should not drop the bead; got %+v", got)
	}
}

func TestApplySupersessions_EmptyAndSingleton(t *testing.T) {
	if got := ApplySupersessions(nil); got != nil {
		t.Errorf("ApplySupersessions(nil) = %v, want nil", got)
	}
	one := []MatchedAdvice{{Bead: &beadsapi.BeadDetail{ID: "gd-only"}}}
	got := ApplySupersessions(one)
	if len(got) != 1 || got[0].Bead.ID != "gd-only" {
		t.Errorf("singleton slice should pass through, got %+v", got)
	}
}

func TestApplySupersessions_MultiHopChainsOneLevel(t *testing.T) {
	// Supersession is a one-level filter: if A supersedes B, B is dropped.
	// If B supersedes C, C is also dropped (each superseder independently
	// claims its targets — there is no transitivity beyond what's spelled
	// out in each description).
	a := MatchedAdvice{Bead: &beadsapi.BeadDetail{ID: "gd-A", Description: "Top rule. supersedes(gd-B)"}}
	b := MatchedAdvice{Bead: &beadsapi.BeadDetail{ID: "gd-B", Description: "Middle rule. supersedes(gd-C)"}}
	c := MatchedAdvice{Bead: &beadsapi.BeadDetail{ID: "gd-C", Description: "Old rule."}}

	got := ApplySupersessions([]MatchedAdvice{a, b, c})

	if len(got) != 1 {
		t.Fatalf("expected only gd-A to survive (B superseded by A, C superseded by B); got %d items: %+v", len(got), got)
	}
	if got[0].Bead.ID != "gd-A" {
		t.Errorf("expected gd-A to survive, got %s", got[0].Bead.ID)
	}
}

func stringSlicesEqual(a, b []string) bool {
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

func TestSupersededOpenIDs(t *testing.T) {
	openAdvice := func(id, desc string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{ID: id, Status: "open", Description: desc}
	}

	t.Run("nil and empty", func(t *testing.T) {
		if got := SupersededOpenIDs(nil); len(got) != 0 {
			t.Errorf("nil pool = %v, want empty", got)
		}
		if got := SupersededOpenIDs([]*beadsapi.BeadDetail{}); len(got) != 0 {
			t.Errorf("empty pool = %v, want empty", got)
		}
	})

	t.Run("basic supersession retires the target", func(t *testing.T) {
		pool := []*beadsapi.BeadDetail{
			openAdvice("gd-new", "Better rule. supersedes(gd-old)"),
			openAdvice("gd-old", "Old rule."),
			openAdvice("gd-other", "Unrelated."),
		}
		got := SupersededOpenIDs(pool)
		if len(got) != 1 || got["gd-old"] != "gd-new" {
			t.Errorf("got %v, want {gd-old: gd-new}", got)
		}
	})

	t.Run("target absent from pool is not retired", func(t *testing.T) {
		// gd-gone is referenced but not in the open pool (already closed) —
		// nothing to compact.
		pool := []*beadsapi.BeadDetail{
			openAdvice("gd-new", "supersedes(gd-gone)"),
		}
		if got := SupersededOpenIDs(pool); len(got) != 0 {
			t.Errorf("got %v, want empty (target not in pool)", got)
		}
	})

	t.Run("superseder that is closed does not retire", func(t *testing.T) {
		pool := []*beadsapi.BeadDetail{
			{ID: "gd-new", Status: "closed", Description: "supersedes(gd-old)"},
			openAdvice("gd-old", "Old rule."),
		}
		if got := SupersededOpenIDs(pool); len(got) != 0 {
			t.Errorf("got %v, want empty (superseder closed)", got)
		}
	})

	t.Run("fixture never triggers a close on either side", func(t *testing.T) {
		fixtureSuperseder := openAdvice("gd-fix", "supersedes(gd-real)")
		fixtureSuperseder.Labels = []string{"fixture:e2e:advice"}
		pool := []*beadsapi.BeadDetail{fixtureSuperseder, openAdvice("gd-real", "Real rule.")}
		if got := SupersededOpenIDs(pool); len(got) != 0 {
			t.Errorf("got %v, want empty (fixture superseder ignored)", got)
		}

		fixtureTarget := openAdvice("gd-fixt", "Fixture target.")
		fixtureTarget.Labels = []string{"fixture:e2e:advice"}
		pool2 := []*beadsapi.BeadDetail{openAdvice("gd-new", "supersedes(gd-fixt)"), fixtureTarget}
		if got := SupersededOpenIDs(pool2); len(got) != 0 {
			t.Errorf("got %v, want empty (fixture target ignored)", got)
		}
	})

	t.Run("expired bead is never used as superseder", func(t *testing.T) {
		expired := openAdvice("gd-exp", "supersedes(gd-old)")
		expired.Fields = map[string]string{"expires_at": "2000-01-01T00:00:00Z"}
		pool := []*beadsapi.BeadDetail{expired, openAdvice("gd-old", "Old rule.")}
		if got := SupersededOpenIDs(pool); len(got) != 0 {
			t.Errorf("got %v, want empty (expired superseder ignored)", got)
		}
	})

	t.Run("bead never supersedes itself", func(t *testing.T) {
		pool := []*beadsapi.BeadDetail{openAdvice("gd-self", "This note supersedes(gd-self).")}
		if got := SupersededOpenIDs(pool); len(got) != 0 {
			t.Errorf("got %v, want empty (self-supersession)", got)
		}
	})

	t.Run("mutual supersession leaves both open", func(t *testing.T) {
		pool := []*beadsapi.BeadDetail{
			openAdvice("gd-a", "supersedes(gd-b)"),
			openAdvice("gd-b", "supersedes(gd-a)"),
		}
		if got := SupersededOpenIDs(pool); len(got) != 0 {
			t.Errorf("got %v, want empty (mutual supersession is ambiguous)", got)
		}
	})

	t.Run("chain retires both middle and tail", func(t *testing.T) {
		pool := []*beadsapi.BeadDetail{
			openAdvice("gd-A", "supersedes(gd-B)"),
			openAdvice("gd-B", "supersedes(gd-C)"),
			openAdvice("gd-C", "Oldest."),
		}
		got := SupersededOpenIDs(pool)
		if len(got) != 2 || got["gd-B"] != "gd-A" || got["gd-C"] != "gd-B" {
			t.Errorf("got %v, want {gd-B: gd-A, gd-C: gd-B}", got)
		}
	})
}
