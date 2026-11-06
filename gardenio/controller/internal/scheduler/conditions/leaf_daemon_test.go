package conditions

import (
	"context"
	"slices"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestEvaluate_BeadQuery_ShortTitleMissing pins the short-title groomer's
// pre-flight (gd-F3n1hBqhCt, plan gd-YZATAIOhfW): a bead_query leaf carrying
// short_title_missing narrows the daemon list to beads with no short_title, so
// the groomer cron only spawns a Haiku pod while there is something to fill.
func TestEvaluate_BeadQuery_ShortTitleMissing(t *testing.T) {
	t.Run("leaf forwards ShortTitleMissing into the query", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(3)}
		e := Expression{Leaf: Leaf{
			Type:              "bead_query",
			Op:                ">",
			Value:             "0",
			Project:           "gardenio",
			Types:             []string{"task", "bug"},
			Statuses:          []string{"open", "closed"},
			ShortTitleMissing: true,
		}}
		got, err := Evaluate(context.Background(), e, Env{Bead: bc})
		if err != nil || !got {
			t.Fatalf("got=%v err=%v", got, err)
		}
		if len(bc.calls) != 1 || !bc.calls[0].ShortTitleMissing {
			t.Errorf("expected ShortTitleMissing=true in query, got %+v", bc.calls)
		}
	})

	t.Run("absent flag leaves the query unfiltered", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(1)}
		e := Expression{Leaf: Leaf{Type: "bead_query", Op: ">", Value: "0", Types: []string{"task"}}}
		if _, err := Evaluate(context.Background(), e, Env{Bead: bc}); err != nil {
			t.Fatal(err)
		}
		if len(bc.calls) != 1 || bc.calls[0].ShortTitleMissing {
			t.Errorf("expected ShortTitleMissing=false in query, got %+v", bc.calls)
		}
	})

	t.Run("legacy conditions JSON carries the flag", func(t *testing.T) {
		// A trigger bead's fields.conditions is parsed through the legacy
		// Condition shape (scheduler.ParseConditions), so the key must survive
		// that parse and ToExpression, not just the Leaf struct.
		cs, err := ParseConditions(`[{"type":"bead_query","op":">","value":"0","types":["task"],"short_title_missing":true}]`)
		if err != nil {
			t.Fatal(err)
		}
		bc := &stubBeadClient{fn: count(0)}
		got, err := EvaluateConditions(context.Background(), bc, cs, "gardenio")
		if err != nil || got {
			t.Fatalf("got=%v err=%v (want false: no bead lacks short_title)", got, err)
		}
		if len(bc.calls) != 1 || !bc.calls[0].ShortTitleMissing {
			t.Errorf("expected ShortTitleMissing=true in query, got %+v", bc.calls)
		}
	})
}

// TestEvaluate_BeadQuery_HeldBy pins the escalation coordinator's backstop
// condition (gd-Khn7cXyokc): a held_by leaf counts the lane's escalations
// through the held_by query, so the 2h cron skips its fire on an empty lane
// and fires on a non-empty one.
func TestEvaluate_BeadQuery_HeldBy(t *testing.T) {
	lane := func(n int) func(beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
		return func(beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
			beads := make([]*beadsapi.BeadDetail, n)
			for i := range beads {
				beads[i] = &beadsapi.BeadDetail{Type: "escalation"}
			}
			return &beadsapi.ListBeadsResult{Beads: beads, Total: n}, nil
		}
	}
	cs, err := ParseConditions(`[{"type":"bead_query","op":">","value":"0","held_by":["coordinator:cluster"]}]`)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a non-empty lane fires, through the held_by query", func(t *testing.T) {
		bc := &stubBeadClient{fn: lane(2)}
		got, err := EvaluateConditions(context.Background(), bc, cs, "gardenio")
		if err != nil || !got {
			t.Fatalf("got=%v err=%v, want true", got, err)
		}
		q := bc.calls[0]
		if !slices.Equal(q.HeldBy, []string{"coordinator:cluster"}) || !slices.Equal(q.Types, []string{"escalation"}) {
			t.Errorf("query = %+v, want held_by coordinator:cluster on type escalation", q)
		}
		if !slices.Equal(q.Statuses, []string{"open", "in_progress"}) {
			t.Errorf("statuses = %v, want open+in_progress (approved escalations are in_progress)", q.Statuses)
		}
		if len(q.Labels) != 0 {
			t.Errorf("labels = %v, want none: a lane spans every project", q.Labels)
		}
	})

	t.Run("an empty lane skips the fire", func(t *testing.T) {
		got, err := EvaluateConditions(context.Background(), &stubBeadClient{fn: lane(0)}, cs, "gardenio")
		if err != nil || got {
			t.Fatalf("got=%v err=%v, want false", got, err)
		}
	})

	t.Run("a filter the lane query cannot apply is an error", func(t *testing.T) {
		for name, l := range map[string]Leaf{
			"types":  {Types: []string{"task"}},
			"labels": {Labels: []string{"project:gardenio"}},
			"parent": {Parent: "gd-x"},
		} {
			l.Type, l.Op, l.Value, l.HeldBy = "bead_query", ">", "0", []string{"coordinator:cluster"}
			bc := &stubBeadClient{fn: lane(1)}
			if _, err := Evaluate(context.Background(), Expression{Leaf: l}, Env{Bead: bc}); err == nil {
				t.Errorf("%s: want an error, got none", name)
			}
			if len(bc.calls) != 0 {
				t.Errorf("%s: queried the daemon %d times, want 0", name, len(bc.calls))
			}
		}
	})
}
