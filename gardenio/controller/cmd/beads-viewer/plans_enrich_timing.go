package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// plans_enrich_timing.go — Phase 1 instrumentation for the /plans index enrich
// pass (plan ka-0000002224, task ka-0000005453). It measures the actual
// wall-clock split across the batch-enrich sections (planIndexEnrichTimed) so
// the /plans render cost is MEASURED, not guessed, and so a regression that
// reintroduces a per-row daemon fan-out shows up as a section time that scales
// with plan count instead of staying flat.
//
// The timing is surfaced two ways, both zero-cost on the happy path:
//   - a Server-Timing response header (browser devtools + `curl -sI` both read
//     it), one metric per section, so an operator can watch the split live;
//   - a single structured debug log line, gated behind the server logger's Debug
//     level, so a running deploy can be flipped to verbose without a code change.
//
// Instrumentation is READ-ONLY: it issues no daemon calls of its own. Every lap
// is a monotonic time delta around work the enrich pass already does.

// planEnrichTiming is the per-section wall-clock breakdown of one
// planIndexEnrichTimed call. Each entry names a section and how long it took;
// order is preserved so the header/log read top-to-bottom like the code.
type planEnrichTiming struct {
	// PlanCount is how many plan epics the pass enriched — the denominator that
	// makes a per-section time comparable across pages of different sizes, and the
	// key signal for spotting an N+1: a bounded section stays flat as PlanCount
	// grows, a fan-out grows with it.
	PlanCount int
	// ListQuery is the wall-clock of the /plans index list round-trip (the "1 list"
	// in the 1-list + bulk-joins round-trip budget). It is measured by handlePlans
	// — the enrich pass never issues it — and stitched in here so the header/log
	// carry the FULL render split (list query + enrich), not just the enrich pass.
	// Zero when the caller did not measure it (the thin planIndexEnrich wrapper and
	// the direct-enrich tests), in which case it is omitted from the header/log.
	ListQuery time.Duration
	// Sections is the ordered (name, duration) split of the SEQUENTIAL phases —
	// wave1, wave2, assemble. Total() sums them, which is the enrich wall clock
	// because the phases run one after another (a barrier separates the waves).
	Sections []planEnrichSection
	// Joins is the per-bulk-join wall clock WITHIN the waves. Unlike Sections these
	// OVERLAP — the joins in a wave fan out concurrently — so they do NOT sum to the
	// wave time. Each is the cost of that one join's branch: the signal that names
	// WHICH join dominates its wave and, read against PlanCount across pages, whether
	// any single join scales with plan count (a surviving per-row fan-out).
	Joins []planEnrichSection
	// joinMu guards concurrent appends to Joins: the joins in a wave fan out under an
	// errgroup and each records its own branch, so timeJoin appends race.
	joinMu sync.Mutex
}

// planEnrichSection is one measured span of the enrich pass.
type planEnrichSection struct {
	Name string
	Dur  time.Duration
}

// Total is the enrich wall-clock: the sum of the SEQUENTIAL sections (wave1,
// wave2, assemble). The per-join splits are intentionally excluded — they run
// concurrently within a wave and so do not add to the critical path.
func (t *planEnrichTiming) Total() time.Duration {
	var sum time.Duration
	for _, s := range t.Sections {
		sum += s.Dur
	}
	return sum
}

// timeJoin runs fn, records its wall clock under name in Joins, and is safe to
// call from concurrent wave branches (the append is mutex-guarded). A nil timing
// still runs fn — the measurement is a no-op but the work always happens — so a
// caller can wrap a join unconditionally. The name is the join's metric label
// (e.g. "join_assistants"); keep it a simple identifier for the Server-Timing
// token grammar.
func (t *planEnrichTiming) timeJoin(name string, fn func()) {
	if t == nil {
		fn()
		return
	}
	start := time.Now()
	fn()
	d := time.Since(start)
	t.joinMu.Lock()
	t.Joins = append(t.Joins, planEnrichSection{Name: name, Dur: d})
	t.joinMu.Unlock()
}

// ServerTimingHeader renders the timing as an RFC-8674 Server-Timing header
// value: `list;dur=8.2, enrich_total;dur=12.3, wave1;dur=1.1, join_assistants;dur=0.9, …`.
// Durations are milliseconds with one decimal, the unit Server-Timing expects. A
// `plans` metric carries the plan count as its dur so devtools shows the
// denominator alongside the splits. The metric order is: plan count, the list
// query (when measured), the enrich total, the sequential wave/assemble sections,
// then every per-join branch. The per-join metrics OVERLAP their wave (the joins
// fan out concurrently) — devtools will show them shorter than the wave that
// contains them, which is exactly the "which join dominates" signal.
func (t *planEnrichTiming) ServerTimingHeader() string {
	parts := make([]string, 0, len(t.Sections)+len(t.Joins)+3)
	parts = append(parts, fmt.Sprintf("plans;desc=\"plan count\";dur=%d", t.PlanCount))
	if t.ListQuery > 0 {
		parts = append(parts, fmt.Sprintf("list;dur=%.1f", ms(t.ListQuery)))
	}
	parts = append(parts, fmt.Sprintf("enrich_total;dur=%.1f", ms(t.Total())))
	for _, s := range t.Sections {
		parts = append(parts, fmt.Sprintf("%s;dur=%.1f", serverTimingMetricName(s.Name), ms(s.Dur)))
	}
	for _, j := range t.Joins {
		parts = append(parts, fmt.Sprintf("%s;dur=%.1f", serverTimingMetricName(j.Name), ms(j.Dur)))
	}
	return strings.Join(parts, ", ")
}

// LogArgs renders the timing as a flat slog key/value slice for one structured
// debug line: plan_count, list_ms (when measured), total_ms, then per-section and
// per-join "<name>_ms" keys. Kept flat (not a nested attr) so it greps cleanly out
// of a text log — `grep join_assistants_ms` pulls the one join's cost across every
// render. The join keys carry a `join_` prefix in their section names already, so
// they never collide with a wave/assemble section key.
func (t *planEnrichTiming) LogArgs() []any {
	args := make([]any, 0, 6+2*(len(t.Sections)+len(t.Joins)))
	args = append(args, "plan_count", t.PlanCount)
	if t.ListQuery > 0 {
		args = append(args, "list_ms", round1(ms(t.ListQuery)))
	}
	args = append(args, "total_ms", round1(ms(t.Total())))
	for _, s := range t.Sections {
		args = append(args, s.Name+"_ms", round1(ms(s.Dur)))
	}
	for _, j := range t.Joins {
		args = append(args, j.Name+"_ms", round1(ms(j.Dur)))
	}
	return args
}

// enrichLapper accumulates section timings against a wall clock. Each lap()
// records the delta since the previous lap (or since construction, for the first
// section). It is not safe for concurrent use — the enrich pass laps
// sequentially between its bulk joins.
type enrichLapper struct {
	timing *planEnrichTiming
	last   time.Time
}

// newEnrichLapper starts a lapper at the current instant. It intentionally uses
// time.Now() rather than the server's injectable clock: this is a monotonic
// stopwatch for latency measurement, not a wall-clock timestamp the tests pin —
// the deltas it records are what a test asserts on (or, for a fake with no
// per-call delay, are simply near-zero and unasserted).
func newEnrichLapper(timing *planEnrichTiming) *enrichLapper {
	return &enrichLapper{timing: timing, last: time.Now()}
}

// lap records the elapsed time since the previous lap under name and resets the
// mark. A nil lapper or nil timing is a no-op so a caller can pass one
// unconditionally.
func (l *enrichLapper) lap(name string) {
	if l == nil || l.timing == nil {
		return
	}
	now := time.Now()
	l.timing.Sections = append(l.timing.Sections, planEnrichSection{Name: name, Dur: now.Sub(l.last)})
	l.last = now
}

// ms converts a duration to fractional milliseconds.
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// round1 rounds to one decimal place so log values stay compact.
func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

// serverTimingMetricName sanitizes a section name into a Server-Timing metric
// token (the grammar forbids most punctuation). Our section names are already
// simple identifiers, so this only guards against a future rename slipping a
// space or comma in.
func serverTimingMetricName(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, name)
}
