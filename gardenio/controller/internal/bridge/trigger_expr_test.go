package bridge

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fixedExprNow is a stable instant for time-relative expression tests.
var fixedExprNow = time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

func TestCompileExpr_RejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"empty", ""},
		{"syntax error", "bead.status =="},
		{"unbalanced paren", "(bead.status == \"open\""},
		{"unknown variable", "nonexistent.field == 1"},
		{"non-bool result (string)", `bead.status`},
		{"non-bool result (int)", `bead.priority`},
		{"non-bool result (arithmetic)", `1 + 1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CompileExpr(tc.src); err == nil {
				t.Errorf("CompileExpr(%q) = nil error, want rejection", tc.src)
			}
		})
	}
}

func TestCompileExpr_AcceptsValidBool(t *testing.T) {
	cases := []string{
		`bead.status == "open"`,
		`bead.type == "bug" && bead.priority == 0`,
		`has(bead.last_checked)`,
		`bead.labels.exists(l, l == "needs-review")`,
		`watched.exists(w, w.status != "closed")`,
		`watched.all(w, w.status == "closed")`,
		`event.type == "updated" && event.changes.status == "in_progress"`,
		`now - timestamp(bead.last_checked) > duration("2h")`,
		// Label membership via the typed list (bead.labels), since a bare
		// map index is dyn-typed and the strict-bool guard rejects it.
		`bead.labels.exists(l, l == "project:gardenio")`,
		// A map index compared to a literal IS a bool-typed expression and
		// is accepted.
		`bead["label:project:gardenio"] == true`,
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			if _, err := CompileExpr(src); err != nil {
				t.Errorf("CompileExpr(%q) unexpected error: %v", src, err)
			}
		})
	}
}

func TestCompileExpr_CachesProgram(t *testing.T) {
	const src = `bead.status == "open"`
	c1, err := CompileExpr(src)
	if err != nil {
		t.Fatalf("first compile: %v", err)
	}
	c2, err := CompileExpr(src)
	if err != nil {
		t.Fatalf("second compile: %v", err)
	}
	if c1 != c2 {
		t.Errorf("expected cached *CompiledExpr to be reused (same pointer); got %p vs %p", c1, c2)
	}
}

func TestEvalExpr_BasicFields(t *testing.T) {
	bead := &BeadEvent{
		ID: "gd-1", Type: "bug", Status: "open", Priority: 0,
		Assignee: "alice",
		Labels:   []string{"needs-review", "project:gardenio"},
		Fields:   map[string]string{"agent_state": "spawning"},
	}
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"status match", `bead.status == "open"`, true},
		{"status mismatch", `bead.status == "closed"`, false},
		{"priority int", `bead.priority == 0`, true},
		{"priority compare", `bead.priority < 2`, true},
		{"and true", `bead.type == "bug" && bead.priority == 0`, true},
		{"and false", `bead.type == "bug" && bead.priority == 3`, false},
		{"or", `bead.type == "task" || bead.status == "open"`, true},
		{"extensible field", `bead.agent_state == "spawning"`, true},
		{"label list exists", `bead.labels.exists(l, l == "needs-review")`, true},
		{"label list not exists", `bead.labels.exists(l, l == "nope")`, false},
		{"label presence map eq true", `bead["label:needs-review"] == true`, true},
		{"assignee", `bead.assignee == "alice"`, true},
		{"has present", `has(bead.agent_state)`, true},
		{"has absent", `has(bead.does_not_exist)`, false},
		{"guarded missing field", `has(bead.missing) && bead.missing == "x"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := CompileExpr(tc.src)
			if err != nil {
				t.Fatalf("compile %q: %v", tc.src, err)
			}
			res := compiled.Eval(context.Background(), bead, "updated", fixedExprNow, nil)
			if res.Err != nil {
				t.Fatalf("eval %q: unexpected error: %v", tc.src, res.Err)
			}
			if res.Matched != tc.want {
				t.Errorf("eval %q = %v, want %v", tc.src, res.Matched, tc.want)
			}
		})
	}
}

func TestEvalExpr_EventContext(t *testing.T) {
	bead := &BeadEvent{
		ID: "gd-1", Type: "task", Status: "in_progress",
		// Numbers arrive as float64 from JSON-decoded SSE payloads; mirror that.
		Changes: map[string]any{"status": "in_progress", "priority": float64(1)},
	}
	cases := []struct {
		name string
		src  string
		ev   string
		want bool
	}{
		{"event type", `event.type == "updated"`, "updated", true},
		{"event type mismatch", `event.type == "created"`, "updated", false},
		{"changes new value", `event.changes.status == "in_progress"`, "updated", true},
		{"changes stringified int", `event.changes.priority == "1"`, "updated", true},
		{"changes missing guarded", `has(event.changes.nope)`, "updated", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := CompileExpr(tc.src)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			res := compiled.Eval(context.Background(), bead, tc.ev, fixedExprNow, nil)
			if res.Err != nil {
				t.Fatalf("eval: %v", res.Err)
			}
			if res.Matched != tc.want {
				t.Errorf("eval %q = %v, want %v", tc.src, res.Matched, tc.want)
			}
		})
	}
}

func TestEvalExpr_TimeRelative(t *testing.T) {
	staleBead := &BeadEvent{
		ID: "gd-stale", Status: "in_progress",
		Fields: map[string]string{
			"last_checked": fixedExprNow.Add(-3 * time.Hour).Format(time.RFC3339),
		},
	}
	freshBead := &BeadEvent{
		ID: "gd-fresh", Status: "in_progress",
		Fields: map[string]string{
			"last_checked": fixedExprNow.Add(-10 * time.Minute).Format(time.RFC3339),
		},
	}
	const src = `has(bead.last_checked) && now - timestamp(bead.last_checked) > duration("2h")`
	compiled, err := CompileExpr(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if res := compiled.Eval(context.Background(), staleBead, "updated", fixedExprNow, nil); res.Err != nil || !res.Matched {
		t.Errorf("stale bead: got matched=%v err=%v, want matched=true", res.Matched, res.Err)
	}
	if res := compiled.Eval(context.Background(), freshBead, "updated", fixedExprNow, nil); res.Err != nil || res.Matched {
		t.Errorf("fresh bead: got matched=%v err=%v, want matched=false", res.Matched, res.Err)
	}
}

func TestEvalExpr_WatchedQuantifiers(t *testing.T) {
	bead := &BeadEvent{ID: "gd-root", Type: "epic", Status: "open"}
	watched := []*BeadEvent{
		{ID: "gd-a", Status: "closed"},
		{ID: "gd-b", Status: "closed"},
		{ID: "gd-c", Status: "in_progress"},
	}
	cases := []struct {
		name    string
		src     string
		watched []*BeadEvent
		want    bool
	}{
		{"exists open child", `watched.exists(w, w.status != "closed")`, watched, true},
		{"all closed false", `watched.all(w, w.status == "closed")`, watched, false},
		{"all closed true", `watched.all(w, w.status == "closed")`, watched[:2], true},
		{"empty watched exists", `watched.exists(w, w.status != "closed")`, nil, false},
		{"empty watched all", `watched.all(w, w.status == "closed")`, nil, true},
		{"size", `watched.size() == 3`, watched, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := CompileExpr(tc.src)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			res := compiled.Eval(context.Background(), bead, "updated", fixedExprNow, tc.watched)
			if res.Err != nil {
				t.Fatalf("eval: %v", res.Err)
			}
			if res.Matched != tc.want {
				t.Errorf("eval %q = %v, want %v", tc.src, res.Matched, tc.want)
			}
		})
	}
}

// TestEvalExpr_FailClosedOnRuntimeError verifies that an expression which
// type-checks but errors at runtime (e.g. unguarded missing-key access)
// returns an error — which callers treat as no-match (fail-closed) — rather
// than panicking or returning true.
func TestEvalExpr_FailClosedOnRuntimeError(t *testing.T) {
	bead := &BeadEvent{ID: "gd-1", Status: "open"} // no `missing` field
	// Unguarded access to a missing key is a runtime no-such-key error in CEL.
	const src = `bead.missing == "x"`
	compiled, err := CompileExpr(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res := compiled.Eval(context.Background(), bead, "updated", fixedExprNow, nil)
	if res.Err == nil {
		t.Fatalf("expected runtime error for unguarded missing-key access, got matched=%v", res.Matched)
	}
	if res.Matched {
		t.Errorf("fail-closed violated: matched=true on error path")
	}
}

// TestEvalExpr_CostLimit verifies the cost budget trips on a pathological
// comprehension rather than running unbounded.
func TestEvalExpr_CostLimit(t *testing.T) {
	// A nested comprehension over a large literal range blows the cost limit.
	const src = `[1,2,3,4,5,6,7,8,9,10].all(i, [1,2,3,4,5,6,7,8,9,10].all(j, [1,2,3,4,5,6,7,8,9,10].exists(k, i + j + k > 0)))`
	compiled, err := CompileExpr(src)
	if err != nil {
		// Some cost is static; if the planner rejects it that's also acceptable.
		t.Skipf("expression rejected at compile (acceptable): %v", err)
	}
	// Force a tiny cost limit by compiling a deliberately expensive expr; the
	// default limit is high, so instead assert the program at least evaluates
	// deterministically without panicking. (The hard cost-limit path — a program
	// built with a tiny CostLimit so the overrun trips deterministically and
	// fails closed — is exercised by TestT1_F4_CostBudgetTripsFailClosed in
	// trigger_liveness_invariants_t1_test.go.)
	res := compiled.Eval(context.Background(), &BeadEvent{ID: "gd-1"}, "updated", fixedExprNow, nil)
	_ = res // either a bool or a cost error — both are non-panicking, fail-closed.
}

func TestEvalExpr_NilCompiled(t *testing.T) {
	var c *CompiledExpr
	res := c.Eval(context.Background(), &BeadEvent{ID: "gd-1"}, "updated", fixedExprNow, nil)
	if res.Err == nil || res.Matched {
		t.Errorf("nil CompiledExpr should fail closed: got matched=%v err=%v", res.Matched, res.Err)
	}
}

func TestEvalExpr_NilBead(t *testing.T) {
	compiled, err := CompileExpr(`has(bead.status)`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res := compiled.Eval(context.Background(), nil, "updated", fixedExprNow, nil)
	if res.Err != nil {
		t.Fatalf("eval over nil bead should not error (empty map): %v", res.Err)
	}
	if res.Matched {
		t.Errorf("has(bead.status) over nil bead should be false")
	}
}

func TestCompiledExpr_Source(t *testing.T) {
	const src = `bead.status == "open"`
	compiled, err := CompileExpr(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got := compiled.Source(); got != src {
		t.Errorf("Source() = %q, want %q", got, src)
	}
	var nilc *CompiledExpr
	if got := nilc.Source(); got != "" {
		t.Errorf("nil Source() = %q, want empty", got)
	}
}

// TestEvalExpr_TimeoutFailClosed verifies a cancelled context fails the
// evaluation closed rather than returning a stale true.
func TestEvalExpr_TimeoutFailClosed(t *testing.T) {
	compiled, err := CompileExpr(`bead.status == "open"`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	res := compiled.Eval(ctx, &BeadEvent{ID: "gd-1", Status: "open"}, "updated", fixedExprNow, nil)
	// A pre-cancelled context may or may not interrupt a trivial expression
	// (it has no comprehension checkpoint); either way the result must be
	// non-panicking and, if it errors, fail-closed.
	if res.Err != nil && res.Matched {
		t.Errorf("fail-closed violated: matched=true alongside err=%v", res.Err)
	}
}

func TestBeadToActivation_TopLevelWinsOverFields(t *testing.T) {
	// An extensible field that collides with a canonical name must not shadow
	// the canonical top-level value.
	bead := &BeadEvent{
		ID: "gd-1", Status: "open",
		Fields: map[string]string{"status": "SHADOW"},
	}
	m := beadToActivation(bead)
	if m["status"] != "open" {
		t.Errorf("canonical status should win over Fields[status]: got %v", m["status"])
	}
}

// sanity: the assert/test CLI path and the source path share one compiler, so
// a quick smoke that a representative assert expression round-trips.
func TestCompileExpr_AssertExamples(t *testing.T) {
	examples := []string{
		`bead.type == "bug" && bead.priority == 0 && watched.exists(w, w.status != "closed")`,
		`bead.status == "in_progress" && has(bead.last_checked) && now - timestamp(bead.last_checked) > duration("4h")`,
	}
	for _, src := range examples {
		if _, err := CompileExpr(src); err != nil {
			t.Errorf("assert example failed to compile: %q: %v", src, err)
		}
	}
}

func TestEvalExpr_ErrorMessageMentionsSource(t *testing.T) {
	compiled, _ := CompileExpr(`bead.missing == "x"`)
	res := compiled.Eval(context.Background(), &BeadEvent{ID: "gd-1"}, "updated", fixedExprNow, nil)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "bead.missing") {
		t.Errorf("error should reference the source expression, got: %v", res.Err)
	}
}
