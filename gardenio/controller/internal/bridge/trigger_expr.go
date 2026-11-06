// Package bridge — sandboxed CEL expression evaluator for trigger conditions
// (feature F4, ka-0000008047).
//
// The legacy beadCondition DSL is AND-only, single-bead/single-field, and its
// only generic predicate (bead_query) is a count comparison. The `{"expr":
// "<CEL>"}` condition kind lifts all four limits: a single CEL expression can
// reference multiple fields, combine them with arbitrary boolean logic, do
// time-relative comparisons, and apply set-quantified predicates over an
// explicitly-resolved `watched` list.
//
// Design constraints (all enforced here):
//   - compile-on-write: malformed expressions are rejected at authoring time
//     (gb trigger create / kd create) via CompileExpr, never at the hot path.
//   - cached program: compiled cel.Program instances are memoised per source
//     string in a process-global LRU-ish cache so repeated Evaluate cycles do
//     not recompile.
//   - bounded cost: every program carries a cel-go CostLimit plus a
//     ContextEval timeout, so a pathological comprehension cannot stall the
//     Evaluate loop.
//   - no I/O, deterministic: the CEL environment exposes only the typed
//     context (bead, event, now, watched). `now` is passed in (captured once
//     per evaluation), and the `watched` set is resolved up front and frozen
//     into the activation — the expression itself performs zero I/O.
//   - FAIL-CLOSED: a compile error, a non-bool result, an eval error, a cost
//     overrun, or a timeout all yield false. A broken assertion must never
//     silently pass. This mirrors the in_subgraph_of / older_than stance.
package bridge

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
)

// Expression-evaluator tunables. Picked so a realistic assertion (a handful of
// field reads plus one quantifier over a bounded `watched` set) evaluates in
// well under a millisecond, while a pathological expression is cut off long
// before it can stall the shared Evaluate loop.
const (
	// exprCostLimit bounds the cel-go runtime cost (an abstract operation
	// count, indicative of CPU). 1,000,000 admits any sane trigger predicate
	// but trips on accidental quadratic comprehensions over a saturated set.
	exprCostLimit uint64 = 1_000_000

	// exprEvalTimeout bounds wall-clock per evaluation via ContextEval. The
	// cost limit is the primary guard; this is a belt-and-suspenders cap for
	// the (unlikely) case where a host extension blocks.
	exprEvalTimeout = 100 * time.Millisecond

	// exprInterruptCheckFreq is how often (in comprehension iterations) the
	// program checks for context cancellation. Low enough that the timeout is
	// observed promptly inside a large comprehension.
	exprInterruptCheckFreq uint = 256

	// exprProgramCacheCap bounds the compiled-program cache. Trigger sets are
	// small (hundreds at most); this cap only guards against an unbounded set
	// of distinct ad-hoc `gb trigger test` expressions accumulating.
	exprProgramCacheCap = 512
)

// CompiledExpr is a compiled, cached CEL program ready for repeated evaluation
// against bead events. Construct via CompileExpr. The zero value is not usable.
type CompiledExpr struct {
	src     string
	program cel.Program
}

// exprEnv is the shared CEL environment. Built once (the declarations are
// static) and reused across every compile — cel.Env is safe for concurrent
// use. Guarded by exprEnvOnce.
var (
	exprEnv     *cel.Env
	exprEnvErr  error
	exprEnvOnce sync.Once
)

// buildExprEnv constructs the typed CEL environment exposed to trigger
// expressions. The variable surface is deliberately small and I/O-free:
//
//	bead    map(string, dyn)  — the event bead's flattened fields (see beadToActivation)
//	event   map(string, dyn)  — event metadata (type, changes)
//	now     timestamp         — evaluation time, captured once per eval
//	watched list(map(string,dyn)) — explicitly-resolved related beads (bounded)
//
// Maps are typed map(string, dyn) so authors can read any bead field by name
// (bead.status, bead.labels, bead["label:project"]) without a per-field decl,
// while keeping the type-checker happy. The `older_than` / `newer_than`
// helpers from the JSON-DSL stopgap are expressible directly in CEL via
// timestamp arithmetic (e.g. `now - timestamp(bead.last_checked) > duration("2h")`),
// so no custom functions are needed.
func buildExprEnv() (*cel.Env, error) {
	exprEnvOnce.Do(func() {
		mapStrDyn := cel.MapType(cel.StringType, cel.DynType)
		exprEnv, exprEnvErr = cel.NewEnv(
			cel.Variable("bead", mapStrDyn),
			cel.Variable("event", mapStrDyn),
			cel.Variable("now", cel.TimestampType),
			cel.Variable("watched", cel.ListType(mapStrDyn)),
			// Keep the cost-estimation surface deterministic: disable macros
			// that could expand cost unpredictably is unnecessary — the
			// standard macros (has, all, exists, map, filter) are exactly the
			// set-quantified predicates F4 wants, and CostLimit already bounds
			// them.
		)
	})
	return exprEnv, exprEnvErr
}

// CompileExpr type-checks and compiles a CEL source string into a cached,
// cost-limited program. It is the single entry point used both at authoring
// time (compile-on-write rejection) and on the hot path (cache lookup).
//
// The expression MUST type-check to a boolean — a trigger condition is a
// predicate. A non-bool result type is rejected here so authors learn at
// create time, not at runtime.
//
// Compiled programs are memoised by source string; repeated calls with the
// same source return the cached program without recompiling.
func CompileExpr(src string) (*CompiledExpr, error) {
	if src == "" {
		return nil, fmt.Errorf("empty CEL expression")
	}
	if cached, ok := exprProgramCacheGet(src); ok {
		return cached, nil
	}

	env, err := buildExprEnv()
	if err != nil {
		return nil, fmt.Errorf("building CEL env: %w", err)
	}

	ast, iss := env.Compile(src)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("compiling CEL expression: %w", iss.Err())
	}
	if ast.OutputType() != cel.BoolType {
		return nil, fmt.Errorf("CEL expression must evaluate to bool, got %s", ast.OutputType())
	}

	program, err := env.Program(
		ast,
		cel.CostLimit(exprCostLimit),
		cel.InterruptCheckFrequency(exprInterruptCheckFreq),
	)
	if err != nil {
		return nil, fmt.Errorf("constructing CEL program: %w", err)
	}

	compiled := &CompiledExpr{src: src, program: program}
	exprProgramCachePut(src, compiled)
	return compiled, nil
}

// EvalExprResult carries the outcome of evaluating a compiled expression. Err
// is non-nil on any failure; callers treat a non-nil Err as FALSE (fail-closed)
// but may log it. Matched is only meaningful when Err is nil.
type EvalExprResult struct {
	Matched bool
	Err     error
}

// Eval evaluates the compiled program against the typed context derived from a
// bead event. now is captured by the caller (once per Evaluate cycle) so all
// time-relative predicates in a cycle see a single consistent clock. watched is
// the explicitly-resolved, bounded set of related beads (may be nil/empty).
//
// FAIL-CLOSED contract: every error path — eval error, cost overrun, timeout,
// or a non-bool runtime value — returns Matched=false with a non-nil Err. The
// caller MUST NOT fire the trigger on a non-nil Err.
func (c *CompiledExpr) Eval(ctx context.Context, bead *BeadEvent, eventType string, now time.Time, watched []*BeadEvent) EvalExprResult {
	if c == nil || c.program == nil {
		return EvalExprResult{Err: fmt.Errorf("nil compiled expression")}
	}

	activation := map[string]any{
		"bead":    beadToActivation(bead),
		"event":   eventToActivation(bead, eventType),
		"now":     now,
		"watched": watchedToActivation(watched),
	}

	evalCtx, cancel := context.WithTimeout(ctx, exprEvalTimeout)
	defer cancel()

	out, _, err := c.program.ContextEval(evalCtx, activation)
	if err != nil {
		return EvalExprResult{Err: fmt.Errorf("evaluating CEL expression %q: %w", c.src, err)}
	}

	// The program type-checked to bool, but a runtime value can still be an
	// error wrapper or unexpected type — coerce defensively and fail closed.
	b, ok := out.Value().(bool)
	if !ok {
		return EvalExprResult{Err: fmt.Errorf("CEL expression %q produced non-bool value %v (%T)", c.src, out.Value(), out.Value())}
	}
	return EvalExprResult{Matched: b}
}

// Source returns the original CEL source string (for logging / round-trip).
func (c *CompiledExpr) Source() string {
	if c == nil {
		return ""
	}
	return c.src
}

// ---------------------------------------------------------------------------
// Typed-context construction
// ---------------------------------------------------------------------------

// beadToActivation flattens a BeadEvent into a CEL-readable map. Top-level
// fields are exposed under their canonical names; the extensible Fields map is
// merged in (so bead.agent_state, bead["label:project:gardenio"] etc. resolve);
// labels are exposed both as a list (bead.labels) and as a presence map
// (bead["label:<name>"] == true) so authors can write set membership naturally.
//
// Field reads that don't resolve yield CEL's no-such-key error inside the
// expression unless guarded with has() — authors are expected to use has() or
// the bead map's typed accessors. To make the common case ergonomic and
// fail-soft, missing scalar fields are NOT pre-populated: that keeps has()
// meaningful. (An author who wants a default writes `has(bead.x) && bead.x ==
// ...` or `bead.labels.exists(l, l == "x")`.)
func beadToActivation(bead *BeadEvent) map[string]any {
	if bead == nil {
		return map[string]any{}
	}
	m := make(map[string]any, len(bead.Fields)+12)

	// Extensible fields first so canonical top-level fields win on conflict.
	for k, v := range bead.Fields {
		m[k] = v
	}

	m["id"] = bead.ID
	m["type"] = bead.Type
	m["title"] = bead.Title
	m["status"] = bead.Status
	m["assignee"] = bead.Assignee
	m["created_by"] = bead.CreatedBy
	m["priority"] = int64(bead.Priority)
	if bead.ClosedBy != "" {
		m["closed_by"] = bead.ClosedBy
	}

	// Labels as a list for quantifiers (bead.labels.exists(l, l == "x")).
	labels := make([]any, len(bead.Labels))
	for i, l := range bead.Labels {
		labels[i] = l
	}
	m["labels"] = labels

	// Label presence map so `bead["label:needs-review"]` reads as a bool.
	for _, l := range bead.Labels {
		m["label:"+l] = true
	}

	return m
}

// eventToActivation exposes event metadata: the event type ("created",
// "updated", "closed", ...) and, for updates, the Changes map (new values),
// stringified so authors can read event.changes.status etc.
func eventToActivation(bead *BeadEvent, eventType string) map[string]any {
	m := map[string]any{
		"type": eventType,
	}
	if bead != nil && len(bead.Changes) > 0 {
		changes := make(map[string]any, len(bead.Changes))
		for k, v := range bead.Changes {
			changes[k] = stringifyChangeValue(v)
		}
		m["changes"] = changes
	} else {
		m["changes"] = map[string]any{}
	}
	return m
}

// watchedToActivation converts the resolved watched beads into a list of
// CEL-readable maps (same shape as bead). nil-safe; returns an empty list so
// `watched.exists(...)` over a missing set is simply false rather than an error.
func watchedToActivation(watched []*BeadEvent) []any {
	out := make([]any, 0, len(watched))
	for _, w := range watched {
		if w == nil {
			continue
		}
		out = append(out, beadToActivation(w))
	}
	return out
}
