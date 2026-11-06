package conditions

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// stubBeadClient lets each test wire ListBeadsFiltered to a closure.
type stubBeadClient struct {
	calls []beadsapi.ListBeadsQuery
	fn    func(beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

func (s *stubBeadClient) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	s.calls = append(s.calls, q)
	return s.fn(q)
}

// stubStatusBeadClient is a stubBeadClient that also implements
// beadStatusClient, so it exercises the closed-root-epic short-circuit in
// evalBeadQuery (ka-0000008051). statuses maps bead-id → status; missing IDs
// fall through to statusErr (nil ⇒ "" status, no error).
type stubStatusBeadClient struct {
	stubBeadClient
	statuses    map[string]string
	statusErr   error
	statusCalls []string
}

func (s *stubStatusBeadClient) GetBeadStatus(_ context.Context, beadID string) (string, error) {
	s.statusCalls = append(s.statusCalls, beadID)
	if s.statusErr != nil {
		return "", s.statusErr
	}
	return s.statuses[beadID], nil
}

// stubAncestor returns canned answers keyed on (parent, child).
type stubAncestor struct {
	answers map[string]bool
	err     error
}

func (s *stubAncestor) IsAncestor(_ context.Context, parent, child string) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.answers[parent+"|"+child], nil
}

func count(n int) func(beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return func(beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
		return &beadsapi.ListBeadsResult{Total: n}, nil
	}
}

// TestEvaluate_DaemonLeaves covers each daemon-side leaf type. Behaviour is
// pinned against the legacy scheduler/condition.go tests (these were lifted
// from there).
func TestEvaluate_DaemonLeaves(t *testing.T) {
	t.Run("bead_query == 0 met when total is 0", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(0)}
		e := Expression{Leaf: Leaf{Type: "bead_query", Op: "==", Value: "0", Types: []string{"task"}}}
		got, err := Evaluate(context.Background(), e, Env{Bead: bc})
		if err != nil || !got {
			t.Fatalf("got=%v err=%v", got, err)
		}
		if len(bc.calls) != 1 || bc.calls[0].Statuses[0] != "open" {
			t.Errorf("expected default status filter [open], got %+v", bc.calls)
		}
	})

	t.Run("bead_query forwards ExcludeLabels into the query", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(2)}
		e := Expression{Leaf: Leaf{
			Type:          "bead_query",
			Op:            ">",
			Value:         "0",
			Types:         []string{"merge-request"},
			Statuses:      []string{"open"},
			ExcludeLabels: []string{"gardener-delivery-skip"},
		}}
		got, err := Evaluate(context.Background(), e, Env{Bead: bc})
		if err != nil || !got {
			t.Fatalf("got=%v err=%v", got, err)
		}
		if len(bc.calls) != 1 {
			t.Fatalf("expected 1 query, got %d", len(bc.calls))
		}
		if len(bc.calls[0].ExcludeLabels) != 1 || bc.calls[0].ExcludeLabels[0] != "gardener-delivery-skip" {
			t.Errorf("expected ExcludeLabels [gardener-delivery-skip] forwarded, got %+v", bc.calls[0].ExcludeLabels)
		}
	})

	t.Run("bead_query > 0 unmet when total is 0", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(0)}
		e := Expression{Leaf: Leaf{Type: "bead_query", Op: ">", Value: "0"}}
		got, _ := Evaluate(context.Background(), e, Env{Bead: bc})
		if got {
			t.Fatal("expected unmet")
		}
	})

	// Regression guard for ka-0000006847: a bead_query gate scopes by TYPE and
	// must NEVER inject a Kinds filter. The intake-groomer gate
	// (types:[task,bug,feature,epic]) intentionally spans kind=data work beads —
	// a type=task/kind=data bead is real groomable work the gardener must wake
	// for (ka-0000003300 sat ~6 weeks precisely because a kind=issue-only KPI
	// could not see it). Narrowing this query to Kinds:["issue"] would make the
	// gate blind to those beads, so pin Kinds empty.
	t.Run("bead_query never sets a Kinds filter (spans kind=data work beads)", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(1)}
		e := Expression{Leaf: Leaf{
			Type:     "bead_query",
			Op:       ">",
			Value:    "0",
			Project:  "gardenio",
			Labels:   []string{"needs-intake"},
			Types:    []string{"task", "bug", "feature", "epic"},
			Statuses: []string{"open"},
		}}
		if _, err := Evaluate(context.Background(), e, Env{Bead: bc}); err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if len(bc.calls) != 1 {
			t.Fatalf("expected 1 query, got %d", len(bc.calls))
		}
		if len(bc.calls[0].Kinds) != 0 {
			t.Errorf("bead_query must not narrow by kind (would hide kind=data work beads); got Kinds=%v", bc.calls[0].Kinds)
		}
	})

	// Repro for ka-0000003939 (recurrence of the ready_queue bug
	// ka-0000000231): bead_query dropped its `project` field entirely, so a
	// project-scoped pre-flight gate counted beads fleet-wide and fired even
	// when the gardener's own lane was empty. Assert the leaf's Project is
	// lowered into a "project:<p>" label ANDed ahead of the caller's labels.
	t.Run("bead_query applies project label from Leaf.Project", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(0)}
		e := Expression{Leaf: Leaf{
			Type:    "bead_query",
			Op:      ">",
			Value:   "0",
			Project: "gardenio",
			Labels:  []string{"needs-rescope"},
			Types:   []string{"task", "bug", "feature", "epic"},
		}}
		got, err := Evaluate(context.Background(), e, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if got {
			t.Fatal("expected unmet: zero beads carry the gating label within project scope")
		}
		if len(bc.calls) != 1 {
			t.Fatalf("expected 1 query, got %d", len(bc.calls))
		}
		wantLabels := []string{"project:gardenio", "needs-rescope"}
		if !slices.Equal(bc.calls[0].Labels, wantLabels) {
			t.Errorf("expected labels %v forwarded, got %v", wantLabels, bc.calls[0].Labels)
		}
	})

	// Project scope resolution mirrors evalReadyQueue/evalActiveAgents: an
	// empty Leaf.Project falls back to env.DefaultProject.
	t.Run("bead_query falls back to DefaultProject when Leaf.Project empty", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(0)}
		e := Expression{Leaf: Leaf{
			Type:   "bead_query",
			Op:     ">",
			Value:  "0",
			Labels: []string{"needs-intake"},
		}}
		_, err := Evaluate(context.Background(), e, Env{Bead: bc, DefaultProject: "gardenio"})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		wantLabels := []string{"project:gardenio", "needs-intake"}
		if len(bc.calls) != 1 || !slices.Equal(bc.calls[0].Labels, wantLabels) {
			t.Errorf("expected labels %v (DefaultProject fallback), got %+v", wantLabels, bc.calls)
		}
	})

	// Back-compat: with neither Leaf.Project nor env.DefaultProject set, the
	// query stays unscoped (no project label injected) so existing unscoped
	// bead_query triggers keep their exact old behaviour.
	t.Run("bead_query stays unscoped when no project anywhere", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(3)}
		e := Expression{Leaf: Leaf{
			Type:   "bead_query",
			Op:     ">",
			Value:  "0",
			Labels: []string{"some-label"},
		}}
		_, err := Evaluate(context.Background(), e, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if len(bc.calls) != 1 || !slices.Equal(bc.calls[0].Labels, []string{"some-label"}) {
			t.Errorf("expected only the caller's labels (no project scope), got %+v", bc.calls)
		}
	})

	// Regression for the exact live shape that motivated ka-0000003939: trigger
	// ka-0000007515's gate is {labels:[needs-rescope], project:gardenio, op:>0}.
	// Six open needs-rescope beads exist fleet-wide but ALL are in another
	// project — with correct scoping the gardenio count is 0 and the gate must
	// NOT fire. The stub keys its count on whether project:gardenio was ANDed,
	// simulating the daemon's label AND-semantics.
	t.Run("bead_query does not count out-of-project beads (ka-0000007515 repro)", func(t *testing.T) {
		bc := &stubBeadClient{fn: func(q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
			// Unscoped query sees all 6 (they live in project:monorepo);
			// once project:gardenio is ANDed in, none match.
			if contains(q.Labels, "project:gardenio") {
				return &beadsapi.ListBeadsResult{Total: 0}, nil
			}
			return &beadsapi.ListBeadsResult{Total: 6}, nil
		}}
		e := Expression{Leaf: Leaf{
			Type:     "bead_query",
			Op:       ">",
			Value:    "0",
			Project:  "gardenio",
			Labels:   []string{"needs-rescope"},
			Statuses: []string{"open"},
			Types:    []string{"task", "bug", "feature", "epic"},
		}}
		got, err := Evaluate(context.Background(), e, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if got {
			t.Fatal("expected gate NOT met: all 6 needs-rescope beads are out-of-project, gardenio lane is empty")
		}
	})

	// A caller that already put an explicit "project:<p>" label in Labels
	// (e.g. trigger ka-0000001007) is not broken by the auto-prepend: the
	// duplicate is harmless under AND-semantics. The auto-added label comes
	// from Leaf.Project, so a leaf that only carries the label in Labels (no
	// Project field) stays exactly as authored — no phantom scope added.
	t.Run("bead_query with explicit project label and no Project field is unchanged", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(2)}
		e := Expression{Leaf: Leaf{
			Type:   "bead_query",
			Op:     ">",
			Value:  "0",
			Labels: []string{"project:gardenio", "gardener-delivery"},
		}}
		got, err := Evaluate(context.Background(), e, Env{Bead: bc})
		if err != nil || !got {
			t.Fatalf("got=%v err=%v", got, err)
		}
		wantLabels := []string{"project:gardenio", "gardener-delivery"}
		if len(bc.calls) != 1 || !slices.Equal(bc.calls[0].Labels, wantLabels) {
			t.Errorf("expected labels %v unchanged (no Project field ⇒ no auto-prepend), got %+v", wantLabels, bc.calls)
		}
	})

	t.Run("ready_queue applies project label", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(7)}
		e := Expression{Leaf: Leaf{Type: "ready_queue", Op: ">=", Value: "1"}}
		_, _ = Evaluate(context.Background(), e, Env{Bead: bc, DefaultProject: "gardenio"})
		if len(bc.calls) != 1 || bc.calls[0].Labels[0] != "project:gardenio" {
			t.Errorf("expected project:gardenio label, got %+v", bc.calls)
		}
	})

	// Repro for ka-0000000231: a labels-gated ready_queue leaf silently
	// dropped l.Labels, so the count covered every open bead in the project
	// and `> 0` fired the trigger on every cron tick.
	t.Run("ready_queue forwards Labels alongside the project label", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(0)}
		e := Expression{Leaf: Leaf{
			Type:    "ready_queue",
			Op:      ">",
			Value:   "0",
			Project: "gardenio",
			Labels:  []string{"fluentbit-mr7"},
		}}
		got, err := Evaluate(context.Background(), e, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if got {
			t.Fatal("expected unmet: zero beads carry the gating label")
		}
		if len(bc.calls) != 1 {
			t.Fatalf("expected 1 query, got %d", len(bc.calls))
		}
		wantLabels := []string{"project:gardenio", "fluentbit-mr7"}
		if !slices.Equal(bc.calls[0].Labels, wantLabels) {
			t.Errorf("expected labels %v forwarded, got %v", wantLabels, bc.calls[0].Labels)
		}
	})

	t.Run("ready_queue forwards ExcludeLabels into the query", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(1)}
		e := Expression{Leaf: Leaf{
			Type:          "ready_queue",
			Op:            ">",
			Value:         "0",
			ExcludeLabels: []string{"gardener-delivery-skip"},
		}}
		got, err := Evaluate(context.Background(), e, Env{Bead: bc, DefaultProject: "gardenio"})
		if err != nil || !got {
			t.Fatalf("got=%v err=%v", got, err)
		}
		if !slices.Equal(bc.calls[0].ExcludeLabels, []string{"gardener-delivery-skip"}) {
			t.Errorf("expected ExcludeLabels forwarded, got %v", bc.calls[0].ExcludeLabels)
		}
	})

	t.Run("ready_queue counts only unblocked beads (NoOpenDeps)", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(0)}
		e := Expression{Leaf: Leaf{Type: "ready_queue", Op: ">", Value: "0"}}
		_, err := Evaluate(context.Background(), e, Env{Bead: bc, DefaultProject: "gardenio"})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !bc.calls[0].NoOpenDeps {
			t.Error("expected NoOpenDeps=true so dependency-blocked beads don't count as ready")
		}
	})

	t.Run("active_agents applies role+project", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(2)}
		e := Expression{Leaf: Leaf{Type: "active_agents", Op: "<", Value: "3", Role: "crew", Project: "gardenio"}}
		got, _ := Evaluate(context.Background(), e, Env{Bead: bc})
		if !got {
			t.Fatal("expected met")
		}
		got2 := bc.calls[0]
		if got2.Types[0] != "agent" {
			t.Errorf("expected types=[agent], got %v", got2.Types)
		}
		if !contains(got2.Labels, "project:gardenio") || !contains(got2.Labels, "role:crew") {
			t.Errorf("expected project+role labels, got %v", got2.Labels)
		}
	})

	t.Run("gate absent returns true when no matching bead", func(t *testing.T) {
		bc := &stubBeadClient{fn: func(beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
			return &beadsapi.ListBeadsResult{}, nil
		}}
		e := Expression{Leaf: Leaf{Type: "gate", Op: "absent", Value: "deploy-freeze"}}
		got, _ := Evaluate(context.Background(), e, Env{Bead: bc})
		if !got {
			t.Fatal("expected absent")
		}
	})

	t.Run("gate present returns true when bead matches by title", func(t *testing.T) {
		bc := &stubBeadClient{fn: func(beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
			return &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{{Title: "deploy-freeze"}}}, nil
		}}
		e := Expression{Leaf: Leaf{Type: "gate", Op: "present", Value: "deploy-freeze"}}
		got, _ := Evaluate(context.Background(), e, Env{Bead: bc})
		if !got {
			t.Fatal("expected gate present")
		}
	})

	t.Run("unknown daemon leaf type errors", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(0)}
		e := Expression{Leaf: Leaf{Type: "nope", Op: "==", Value: "0"}}
		_, err := Evaluate(context.Background(), e, Env{Bead: bc})
		if err == nil || !strings.Contains(err.Error(), "unknown daemon leaf type") {
			t.Errorf("expected unknown-type error, got %v", err)
		}
	})

	t.Run("daemon leaf with nil bead client errors", func(t *testing.T) {
		e := Expression{Leaf: Leaf{Type: "bead_query", Op: "==", Value: "0"}}
		_, err := Evaluate(context.Background(), e, Env{})
		if err == nil {
			t.Fatal("expected error for nil bead client")
		}
	})
}

// TestEvaluate_BeadQuery_ClosedRootEpic reproduces the schedule-gardener
// stop-gate bug (ka-0000008051): a closed root epic should auto-dormant its
// trigger rather than spawning forever off residual subtree work.
//
// Repro shape: trigger ka-0000003150 gated on
//
//	{"type":"bead_query","op":">","value":"0","parent":"ka-0000001579", ...}
//
// kept firing because two beads were grafted under the CLOSED root epic
// ~11h after it closed, so the recursive subtree count stayed at 2 > 0.
func TestEvaluate_BeadQuery_ClosedRootEpic(t *testing.T) {
	const rootEpic = "ka-0000001579"

	// The pre-flight leaf used by the real trigger: "fire while subtree has
	// open work". Subtree currently has 2 open descendants grafted under the
	// closed root.
	preflight := Leaf{
		Type:     "bead_query",
		Op:       ">",
		Value:    "0",
		Parent:   rootEpic,
		Types:    []string{"task", "bug", "epic"},
		Statuses: []string{"open", "in_progress", "blocked"},
	}

	t.Run("closed root epic short-circuits subtree count to terminal", func(t *testing.T) {
		bc := &stubStatusBeadClient{
			stubBeadClient: stubBeadClient{fn: count(2)}, // 2 residual open descendants
			statuses:       map[string]string{rootEpic: "closed"},
		}
		// Pre-flight ">0" must now be UNMET (root closed ⇒ effective count 0),
		// so the schedule stops firing.
		got, err := Evaluate(context.Background(), Expression{Leaf: preflight}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if got {
			t.Fatal("expected pre-flight UNMET (auto-dormant) when root epic is closed")
		}
		if len(bc.statusCalls) != 1 || bc.statusCalls[0] != rootEpic {
			t.Errorf("expected one GetBeadStatus call for %s, got %v", rootEpic, bc.statusCalls)
		}
	})

	t.Run("closed root epic makes gardener loop_end (==0) terminal", func(t *testing.T) {
		bc := &stubStatusBeadClient{
			stubBeadClient: stubBeadClient{fn: count(2)},
			statuses:       map[string]string{rootEpic: "closed"},
		}
		loopEnd := preflight
		loopEnd.Op = "=="
		loopEnd.Value = "0"
		// Gardener loop_end "==0" must now be MET (loop terminates) despite the
		// residual subtree work.
		got, err := Evaluate(context.Background(), Expression{Leaf: loopEnd}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !got {
			t.Fatal("expected gardener loop_end MET (terminal) when root epic is closed")
		}
	})

	t.Run("open root epic keeps legacy subtree-count behaviour", func(t *testing.T) {
		bc := &stubStatusBeadClient{
			stubBeadClient: stubBeadClient{fn: count(2)},
			statuses:       map[string]string{rootEpic: "open"},
		}
		got, err := Evaluate(context.Background(), Expression{Leaf: preflight}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !got {
			t.Fatal("expected pre-flight MET (keep firing) while root epic is open")
		}
	})

	t.Run("status fetch error falls back to subtree count (fail-safe)", func(t *testing.T) {
		bc := &stubStatusBeadClient{
			stubBeadClient: stubBeadClient{fn: count(2)},
			statusErr:      errors.New("daemon down"),
		}
		// On a status-fetch error we must NOT dormant a possibly-live loop:
		// fall back to the legacy ">0" = true.
		got, err := Evaluate(context.Background(), Expression{Leaf: preflight}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !got {
			t.Fatal("expected fail-safe to legacy subtree count on status-fetch error")
		}
	})

	t.Run("zero subtree count skips status fetch entirely", func(t *testing.T) {
		bc := &stubStatusBeadClient{
			stubBeadClient: stubBeadClient{fn: count(0)},
			statuses:       map[string]string{rootEpic: "closed"},
		}
		got, err := Evaluate(context.Background(), Expression{Leaf: preflight}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if got {
			t.Fatal("expected unmet for empty subtree")
		}
		if len(bc.statusCalls) != 0 {
			t.Errorf("expected no status fetch when count already 0, got %v", bc.statusCalls)
		}
	})

	t.Run("no parent scope leaves count untouched", func(t *testing.T) {
		// A bead_query with no Parent must never trigger a status fetch.
		bc := &stubStatusBeadClient{
			stubBeadClient: stubBeadClient{fn: count(3)},
			statuses:       map[string]string{rootEpic: "closed"},
		}
		leaf := Leaf{Type: "bead_query", Op: ">", Value: "0", Types: []string{"task"}}
		got, err := Evaluate(context.Background(), Expression{Leaf: leaf}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !got {
			t.Fatal("expected met for unparented count>0")
		}
		if len(bc.statusCalls) != 0 {
			t.Errorf("expected no status fetch without Parent scope, got %v", bc.statusCalls)
		}
	})

	t.Run("client without GetBeadStatus keeps legacy behaviour", func(t *testing.T) {
		// stubBeadClient does NOT implement beadStatusClient — the
		// short-circuit must be skipped entirely (no panic, legacy count).
		bc := &stubBeadClient{fn: count(2)}
		got, err := Evaluate(context.Background(), Expression{Leaf: preflight}, Env{Bead: bc})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !got {
			t.Fatal("expected legacy met when client lacks GetBeadStatus")
		}
	})
}

// TestEvaluate_FieldLeaves covers every field-leaf operator.
func TestEvaluate_FieldLeaves(t *testing.T) {
	payload := map[string]any{
		"parent": "gd-EPIC",
		"type":   "task",
		"labels": []string{"project:gardenio", "role:crew"},
		"raised_by_agent": map[string]any{
			"task_id": "gd-CHILD",
		},
	}

	cases := []struct {
		name string
		leaf Leaf
		want bool
	}{
		{"== match", Leaf{Field: "parent", Op: "==", Value: "gd-EPIC"}, true},
		{"== mismatch", Leaf{Field: "parent", Op: "==", Value: "gd-OTHER"}, false},
		{"!= match", Leaf{Field: "parent", Op: "!=", Value: "gd-OTHER"}, true},
		{"in match", Leaf{Field: "type", Op: "in", Values: []string{"task", "bug"}}, true},
		{"in miss", Leaf{Field: "type", Op: "in", Values: []string{"epic"}}, false},
		{"not_in match", Leaf{Field: "type", Op: "not_in", Values: []string{"epic"}}, true},
		{"not_in miss", Leaf{Field: "type", Op: "not_in", Values: []string{"task"}}, false},
		{"contains match", Leaf{Field: "parent", Op: "contains", Value: "EPIC"}, true},
		{"contains miss", Leaf{Field: "parent", Op: "contains", Value: "ZZZ"}, false},
		{"present existing field", Leaf{Field: "parent", Op: "present"}, true},
		{"present missing field", Leaf{Field: "missing", Op: "present"}, false},
		{"absent existing field", Leaf{Field: "parent", Op: "absent"}, false},
		{"absent missing field", Leaf{Field: "missing", Op: "absent"}, true},
		{"label present", Leaf{Field: "label:project:gardenio", Op: "present"}, true},
		{"label absent", Leaf{Field: "label:project:other", Op: "absent"}, true},
		{"label == returns label name when matched", Leaf{Field: "label:role:crew", Op: "==", Value: "role:crew"}, true},
		{"dotted path match", Leaf{Field: "raised_by_agent.task_id", Op: "==", Value: "gd-CHILD"}, true},
		{"dotted path miss", Leaf{Field: "raised_by_agent.task_id", Op: "==", Value: "gd-OTHER"}, false},
		{"missing field == is false (no implicit empty)", Leaf{Field: "missing", Op: "==", Value: ""}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Expression{Leaf: tc.leaf}
			got, err := Evaluate(context.Background(), e, Env{Payload: payload})
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if got != tc.want {
				t.Errorf("got=%v want=%v", got, tc.want)
			}
		})
	}

	t.Run("unknown field op errors", func(t *testing.T) {
		e := Expression{Leaf: Leaf{Field: "parent", Op: "nope", Value: "x"}}
		_, err := Evaluate(context.Background(), e, Env{Payload: payload})
		if err == nil || !strings.Contains(err.Error(), "unknown field op") {
			t.Errorf("expected unknown-op error, got %v", err)
		}
	})
}

// TestEvaluate_FieldLeaf_InSubtreeOf wires the operator through the
// AncestorClient interface.
func TestEvaluate_FieldLeaf_InSubtreeOf(t *testing.T) {
	payload := map[string]any{"raised_by_agent": map[string]any{"task_id": "gd-CHILD"}}
	anc := &stubAncestor{answers: map[string]bool{
		"gd-EPIC|gd-CHILD":  true,
		"gd-OTHER|gd-CHILD": false,
	}}

	t.Run("in subtree", func(t *testing.T) {
		e := Expression{Leaf: Leaf{Field: "raised_by_agent.task_id", Op: "in_subtree_of", Value: "gd-EPIC"}}
		got, err := Evaluate(context.Background(), e, Env{Payload: payload, Ancestor: anc})
		if err != nil || !got {
			t.Fatalf("got=%v err=%v", got, err)
		}
	})

	t.Run("not in subtree", func(t *testing.T) {
		e := Expression{Leaf: Leaf{Field: "raised_by_agent.task_id", Op: "in_subtree_of", Value: "gd-OTHER"}}
		got, _ := Evaluate(context.Background(), e, Env{Payload: payload, Ancestor: anc})
		if got {
			t.Fatal("expected false")
		}
	})

	t.Run("missing ancestor client errors", func(t *testing.T) {
		e := Expression{Leaf: Leaf{Field: "raised_by_agent.task_id", Op: "in_subtree_of", Value: "gd-EPIC"}}
		_, err := Evaluate(context.Background(), e, Env{Payload: payload})
		if err == nil {
			t.Fatal("expected error when no ancestor client wired")
		}
	})

	t.Run("ancestor client error propagates", func(t *testing.T) {
		ancErr := &stubAncestor{err: errors.New("boom")}
		e := Expression{Leaf: Leaf{Field: "raised_by_agent.task_id", Op: "in_subtree_of", Value: "gd-EPIC"}}
		_, err := Evaluate(context.Background(), e, Env{Payload: payload, Ancestor: ancErr})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

// TestEvaluate_Compositions covers all/any/not at multiple nesting depths.
// These are the cases that justify the DSL: today's flat conditions[] can't
// express any-of, and the scheduler gardener's stop block needs nested any-of
// over heterogeneous leaves.
func TestEvaluate_Compositions(t *testing.T) {
	const trueLeafJSON = "true"
	_ = trueLeafJSON
	tCount := func(t *testing.T, n int) BeadClient {
		t.Helper()
		return &stubBeadClient{fn: count(n)}
	}

	t.Run("all: short-circuits on first false", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(0)}
		e := Expression{All: []Expression{
			{Leaf: Leaf{Type: "bead_query", Op: ">", Value: "0"}},  // false
			{Leaf: Leaf{Type: "bead_query", Op: "==", Value: "0"}}, // would be true
		}}
		got, _ := Evaluate(context.Background(), e, Env{Bead: bc})
		if got {
			t.Fatal("expected false")
		}
		if len(bc.calls) != 1 {
			t.Errorf("expected short-circuit after first false, got %d calls", len(bc.calls))
		}
	})

	t.Run("any: short-circuits on first true", func(t *testing.T) {
		bc := &stubBeadClient{fn: count(5)}
		e := Expression{Any: []Expression{
			{Leaf: Leaf{Type: "bead_query", Op: ">", Value: "0"}},  // true
			{Leaf: Leaf{Type: "bead_query", Op: "==", Value: "0"}}, // would be false
		}}
		got, _ := Evaluate(context.Background(), e, Env{Bead: bc})
		if !got {
			t.Fatal("expected true")
		}
		if len(bc.calls) != 1 {
			t.Errorf("expected short-circuit after first true, got %d calls", len(bc.calls))
		}
	})

	t.Run("not: inverts inner", func(t *testing.T) {
		bc := tCount(t, 0)
		e := Expression{Not: &Expression{Leaf: Leaf{Type: "bead_query", Op: ">", Value: "0"}}}
		got, _ := Evaluate(context.Background(), e, Env{Bead: bc})
		if !got {
			t.Fatal("expected !false = true")
		}
	})

	t.Run("nested all/any/not (worked example §5.3 shape)", func(t *testing.T) {
		// all:
		//   bead_query > 0       (true: 7)
		//   any:
		//     active_agents < 3  (true: 2)
		//     gate absent        (true: 0 beads)
		bc := &stubBeadClient{fn: func(q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
			if contains(q.Types, "agent") {
				return &beadsapi.ListBeadsResult{Total: 2}, nil
			}
			if contains(q.Types, "gate") {
				return &beadsapi.ListBeadsResult{}, nil
			}
			return &beadsapi.ListBeadsResult{Total: 7}, nil
		}}
		e := Expression{All: []Expression{
			{Leaf: Leaf{Type: "bead_query", Op: ">", Value: "0", Types: []string{"task"}}},
			{Any: []Expression{
				{Leaf: Leaf{Type: "active_agents", Op: "<", Value: "3", Role: "crew"}},
				{Leaf: Leaf{Type: "gate", Op: "absent", Value: "deploy-freeze"}},
			}},
		}}
		got, err := Evaluate(context.Background(), e, Env{Bead: bc, DefaultProject: "gardenio"})
		if err != nil || !got {
			t.Fatalf("got=%v err=%v", got, err)
		}
	})

	t.Run("composition mixes daemon + field leaves", func(t *testing.T) {
		bc := tCount(t, 1)
		e := Expression{All: []Expression{
			{Leaf: Leaf{Type: "bead_query", Op: ">", Value: "0"}},
			{Leaf: Leaf{Field: "parent", Op: "==", Value: "gd-X"}},
		}}
		got, _ := Evaluate(context.Background(), e, Env{
			Bead:    bc,
			Payload: map[string]any{"parent": "gd-X"},
		})
		if !got {
			t.Fatal("expected true")
		}
	})

	t.Run("empty leaf evaluates to true (no-op)", func(t *testing.T) {
		got, err := Evaluate(context.Background(), Expression{}, Env{})
		if err != nil || !got {
			t.Fatalf("empty expression should be no-op true, got=%v err=%v", got, err)
		}
	})
}

// TestEvaluateCondition_LegacyCompat ensures the backward-compat shim
// behaves identically to the inline scheduler.EvaluateCondition it
// replaced.
func TestEvaluateCondition_LegacyCompat(t *testing.T) {
	bc := &stubBeadClient{fn: count(0)}
	c := Condition{Type: "bead_query", Op: "==", Value: "0", Types: []string{"task"}}
	got, err := EvaluateCondition(context.Background(), bc, c, "gardenio")
	if err != nil || !got {
		t.Fatalf("got=%v err=%v", got, err)
	}

	// nil client errors (matches legacy behavior).
	if _, err := EvaluateCondition(context.Background(), nil, c, ""); err == nil {
		t.Fatal("expected error on nil client")
	}

	// Empty list of conditions = true.
	got, _ = EvaluateConditions(context.Background(), bc, nil, "")
	if !got {
		t.Fatal("empty conditions slice must be true")
	}
}

// TestCompareCount pins the operator table.
func TestCompareCount(t *testing.T) {
	cases := []struct {
		count int
		op    string
		val   string
		want  bool
	}{
		{5, ">", "3", true},
		{5, ">", "5", false},
		{5, ">=", "5", true},
		{5, "<", "10", true},
		{5, "<=", "5", true},
		{5, "==", "5", true},
		{5, "eq", "5", true},
		{5, "!=", "3", true},
		{5, "ne", "3", true},
		{5, "??", "5", false},
		{5, "==", "not-an-int", false},
	}
	for _, tc := range cases {
		got := CompareCount(tc.count, tc.op, tc.val)
		if got != tc.want {
			t.Errorf("CompareCount(%d,%q,%q) = %v, want %v", tc.count, tc.op, tc.val, got, tc.want)
		}
	}
}

func contains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}
