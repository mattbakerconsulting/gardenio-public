package conditions

import (
	"context"
	"fmt"
	"log/slog"

	"gardenio/controller/internal/beadsapi"
)

// beadStatusClient is an optional capability: clients that can fetch a single
// bead's status enable the closed-parent terminal short-circuit in
// evalBeadQuery (ka-0000008051). The production *beadsapi.Client implements it;
// stubs that don't satisfy this interface simply keep the legacy
// subtree-count-only behaviour.
type beadStatusClient interface {
	GetBeadStatus(ctx context.Context, beadID string) (string, error)
}

// blockedByResolver is an optional capability: clients that can batch-fetch
// dependencies and beads enable the exclude_blocked_by_open_types filter in
// evalBeadQuery (ka-0000002198). It lets a gardener loop_end subtract children
// that are legitimately blocked_by an OPEN escalation/decision from the count,
// so a gardener does not loop forever while its only open work is waiting on an
// operator. The production *beadsapi.Client implements both methods; stubs that
// don't satisfy this interface fall back (fail-safe) to counting every matched
// bead.
type blockedByResolver interface {
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
}

// Compile-time proof that the production daemon client satisfies BOTH optional
// capabilities. If a refactor drops either method, evalBeadQuery would silently
// fall back to legacy behaviour in production (the closed-root short-circuit or
// the externally-blocked exclusion would become a no-op) — these assertions
// turn that regression into a build failure instead.
var (
	_ beadStatusClient  = (*beadsapi.Client)(nil)
	_ blockedByResolver = (*beadsapi.Client)(nil)
)

// beadQueryBlockedScanCap bounds how many matched rows evalBeadQuery pulls when
// the exclude_blocked_by_open_types filter is active. A subtree larger than
// this cannot be enumerated in one page, so the filter fails safe (excludes
// nothing) rather than under-counting — see subtractExternallyBlocked.
const beadQueryBlockedScanCap = 500

// maxTransitiveBlockDepth bounds how many hops the externally-blocked exclusion
// walks along the forward blocking-dep chain before giving up on a path. Real
// multi-phase epics chain ~3-4 deep (phase task → intermediate task →
// operator escalation); 8 leaves generous headroom while capping the fan-out. A
// path not resolved within this depth is treated as unprovable — fail-safe: its
// dependent is NOT subtracted, so the gardener keeps looping (ka-0000004214).
const maxTransitiveBlockDepth = 8

// maxTransitiveBlockNodes bounds the total number of distinct beads the walk
// pulls detail for (matched children plus their transitive blockers). Past this
// the walk stops expanding and any still-undiscovered blocker is treated as
// unprovable (fail-safe). Guards against a pathologically wide block graph
// turning one gate evaluation into an unbounded fetch storm.
const maxTransitiveBlockNodes = 2000

// blockingDepTypes mirrors the daemon's FilterNoOpenDeps allow-list (kbeads
// internal/store/postgres/queries.go): dep types that mean the source bead is
// genuinely waiting on the target. Structural/relational types (parent-child,
// related, duplicates, supersedes, …) link beads but do NOT mean the source is
// blocked. Kept in lockstep with the daemon so the exclusion filter's notion of
// "blocked_by" matches `gb ready` / `kd list --no-blockers`.
var blockingDepTypes = map[string]struct{}{
	"blocks":     {},
	"blocked-by": {},
	"depends-on": {},
}

func isBlockingDepType(t string) bool {
	_, ok := blockingDepTypes[t]
	return ok
}

// isOpenBlockerStatus reports whether a blocker bead is still "live" — i.e. it
// keeps its dependent blocked. Matches the daemon's NoOpenDeps status set
// (open|in_progress|blocked|deferred); only a terminal-for-progression status
// (closed|partial) is history, not a wait edge. `blocked` is itself
// non-terminal, so a blocker in `blocked` status still holds its dependent back
// — kept in lockstep with the daemon predicate aligned in ka-0000006285.
func isOpenBlockerStatus(s string) bool {
	switch s {
	case "open", "in_progress", "blocked", "deferred":
		return true
	default:
		return false
	}
}

// nonActionableChildTypes are bead types a gardener can never self-serve to
// closure by spawning crew — so a matched child of one of these types is always
// subtracted from the actionable count (ka-0000004214). Today this is just
// `bundle`: a plan-shaped epic always carries one narrative plan-of-record
// bundle child whose lifecycle is driven by the plan machinery, not by a crew
// agent. Without this, that lone bundle floors the wedge count at 1 for every
// /plans plan gardener even after every real task is operator-blocked, and the
// dormancy brake never engages. Subtracting a bundle is unconditionally correct
// (a bundle is never agent-work), so it introduces no false-positive risk.
var nonActionableChildTypes = map[string]struct{}{
	"bundle": {},
}

func isNonActionableChildType(t string) bool {
	_, ok := nonActionableChildTypes[t]
	return ok
}

// assigneeIsHumanOperator reports whether an assignee string is POSITIVELY a
// human operator rather than a generated agent name. Generated gardenio agent
// names are strictly lowercase [a-z0-9] segments joined by hyphens (e.g.
// "gardenio-gardener-dormancy-brake-vbc", "thread-1785114367-427259"); a human
// assignee carries a signal an agent name never does — an uppercase letter, a
// dot, an '@', or a "slack:"/"oidc:" style colon (e.g. "Matthew.Baker",
// "matthew.baker@acme.example"). We only return true on such a positive human
// signal, so a mis-shaped agent name can never be read as a human. This keeps
// the exclusion fail-safe: an operator-assigned child (which no crew agent can
// pick up) is treated as non-actionable, but any doubt falls to false, leaving
// the child counted and the gardener awake (ka-0000004214, fix option 3).
func assigneeIsHumanOperator(assignee string) bool {
	if assignee == "" {
		return false
	}
	for _, r := range assignee {
		if r >= 'A' && r <= 'Z' {
			return true
		}
		switch r {
		case '.', '@', ':':
			return true
		}
	}
	return false
}

// evalDaemonLeaf dispatches to the per-Type evaluator. Lifted from the
// scheduler package's condition.go so the DSL has a single home; behaviour
// preserved exactly for the existing types.
func evalDaemonLeaf(ctx context.Context, l Leaf, env Env) (bool, error) {
	// The time_window leaf consults only the wall clock and its own fields —
	// no bead store — so it is dispatched BEFORE the nil-client guard. This
	// lets it gate a legacy schedule trigger's top-level conditions and a
	// cron preflight identically, even in an Env with no BeadClient.
	if l.Type == "time_window" {
		return evalTimeWindow(l)
	}
	if env.Bead == nil {
		return false, fmt.Errorf("conditions: nil bead client for daemon leaf %q", l.Type)
	}
	switch l.Type {
	case "ready_queue":
		return evalReadyQueue(ctx, env.Bead, l, env.DefaultProject)
	case "active_agents":
		return evalActiveAgents(ctx, env.Bead, l, env.DefaultProject)
	case "gate":
		return evalGate(ctx, env.Bead, l)
	case "bead_query":
		return evalBeadQuery(ctx, env.Bead, l, env.DefaultProject)
	case "acceptance_metric":
		return evalAcceptanceMetric(ctx, env.Bead, l)
	case "script":
		return evalScript(ctx, env.Bead, l)
	case QuietGardenersLeaf:
		return evalQuietGardeners(ctx, env.Bead, l, env.DefaultProject)
	default:
		return false, fmt.Errorf("conditions: unknown daemon leaf type %q", l.Type)
	}
}

// evalReadyQueue counts beads with `gb ready` semantics: open kind=issue
// beads with no open dependencies (NoOpenDeps). A label-gated leaf must
// forward l.Labels/l.ExcludeLabels — dropping them made a schedule trigger
// count every open bead in the project, so `> 0` fired on every cron tick
// (ka-0000000231). NoOpenDeps closes the same incident's second half:
// counting dependency-blocked beads as "ready" spawns gardeners that have
// nothing claimable to do.
func evalReadyQueue(ctx context.Context, client BeadClient, l Leaf, defaultProject string) (bool, error) {
	project := l.Project
	if project == "" {
		project = defaultProject
	}
	var labels []string
	if project != "" {
		labels = append(labels, "project:"+project)
	}
	labels = append(labels, l.Labels...)
	result, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:         []string{"issue"},
		Statuses:      []string{"open"},
		Labels:        labels,
		ExcludeLabels: l.ExcludeLabels,
		NoOpenDeps:    true,
		Limit:         1,
	})
	if err != nil {
		return false, fmt.Errorf("ready_queue query: %w", err)
	}
	return CompareCount(result.Total, l.Op, l.Value), nil
}

func evalActiveAgents(ctx context.Context, client BeadClient, l Leaf, defaultProject string) (bool, error) {
	project := l.Project
	if project == "" {
		project = defaultProject
	}
	var labels []string
	if project != "" {
		labels = append(labels, "project:"+project)
	}
	if l.Role != "" {
		labels = append(labels, "role:"+l.Role)
	}
	result, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress"},
		Labels:   labels,
		Limit:    1,
	})
	if err != nil {
		return false, fmt.Errorf("active_agents query: %w", err)
	}
	return CompareCount(result.Total, l.Op, l.Value), nil
}

func evalGate(ctx context.Context, client BeadClient, l Leaf) (bool, error) {
	gateTitle := l.Value
	if gateTitle == "" {
		// Nothing to gate on; treat as satisfied.
		return true, nil
	}
	result, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"gate"},
		Statuses: []string{"open", "in_progress"},
		Search:   gateTitle,
		Limit:    10,
	})
	if err != nil {
		return false, fmt.Errorf("gate query: %w", err)
	}
	gateExists := false
	for _, b := range result.Beads {
		if b.Title == gateTitle {
			gateExists = true
			break
		}
	}
	switch l.Op {
	case "absent", "":
		return !gateExists, nil
	case "present":
		return gateExists, nil
	default:
		return !gateExists, nil
	}
}

func evalBeadQuery(ctx context.Context, client BeadClient, l Leaf, defaultProject string) (bool, error) {
	count, err := evalBeadQueryCount(ctx, client, l, defaultProject)
	if err != nil {
		return false, err
	}
	return CompareCount(count, l.Op, l.Value), nil
}

// EvaluateBeadQueryCount returns the EFFECTIVE bead_query count — the exact
// number evalBeadQuery compares against the leaf's op/value, i.e. AFTER the
// exclude_labels/exclude_blocked_by_open_types subtraction and the closed-root
// short-circuit. Exposed so the dark-lane detector (bug ka-0000001127) can
// compare this effective (subtracted) count against a raw (excludes-stripped)
// count to spot a lane that goes dark because its pre-flight condition is stuck
// at 0 while matching beads really exist — the condition-suppression class the
// concurrency-only dark-lane sweep is blind to. Fail-safe semantics are
// identical to evalBeadQuery: any uncertainty leaves the count un-subtracted.
func EvaluateBeadQueryCount(ctx context.Context, client BeadClient, l Leaf, defaultProject string) (int, error) {
	return evalBeadQueryCount(ctx, client, l, defaultProject)
}

// evalBeadQueryCount is the count-returning core of evalBeadQuery: it runs the
// list query, applies the externally-blocked exclusion and closed-root
// short-circuit, and returns the effective count without the final
// op/value comparison.
func evalBeadQueryCount(ctx context.Context, client BeadClient, l Leaf, defaultProject string) (int, error) {
	if len(l.HeldBy) > 0 {
		return evalHeldByCount(ctx, client, l)
	}
	statuses := l.Statuses
	if len(statuses) == 0 {
		statuses = []string{"open"}
	}

	// Project scope. beadsapi.ListBeadsQuery has no Project field — scope is
	// expressed purely as a "project:<name>" entry in Labels, exactly as
	// evalReadyQueue/evalActiveAgents do. Dropping this made a project-scoped
	// bead_query count beads in EVERY project, so a `> 0` pre-flight gate fired
	// even when the gardener's own lane was empty and burned a no-op Opus pod
	// per cron tick (ka-0000003939; recurrence of the ready_queue bug
	// ka-0000000231). Build a fresh slice — never mutate l.Labels, which the
	// caller may reuse across evaluations. A caller that already put an
	// explicit "project:<x>" in Labels just yields a harmless duplicate under
	// AND-semantics.
	project := l.Project
	if project == "" {
		project = defaultProject
	}
	var labels []string
	if project != "" {
		labels = append(labels, "project:"+project)
	}
	labels = append(labels, l.Labels...)

	// When the exclude_blocked_by_open_types filter is active we need the
	// matched ROWS (to inspect their blocking deps), not just a count — so
	// widen the limit to the scan cap. Otherwise the query stays count-only
	// (Limit 1) exactly as before.
	limit := 1
	filterBlocked := len(l.ExcludeBlockedByOpenTypes) > 0
	if filterBlocked {
		limit = beadQueryBlockedScanCap
	}

	// Intentionally NO Kinds filter (ka-0000006847). A bead_query gate scans by
	// TYPE (l.Types, e.g. task/bug/feature/epic), spanning every kind — a
	// type=task/kind=data bead is a real work bead a gardener must be woken for.
	// Do NOT "helpfully" add Kinds:["issue"] here to mirror the CLI's default
	// `kd list` behavior: that default (kbeads/cmd/kd/list.go implicitIssueKind)
	// only injects kind=issue when NO --type is passed, and narrowing the gate to
	// kind=issue would leave a kind=data work bead (e.g. ka-0000003300, which sat
	// ~6 weeks) undiscovered forever — the exact regression this comment guards.
	// The KPI query in docs/runbooks/intake-groomer.md §2 is widened to match
	// this row set (explicit --type list) rather than the reverse.
	result, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:             l.Types,
		Statuses:          statuses,
		Labels:            labels,
		ExcludeLabels:     l.ExcludeLabels,
		Parent:            l.Parent,
		ShortTitleMissing: l.ShortTitleMissing,
		Limit:             limit,
	})
	if err != nil {
		return 0, fmt.Errorf("bead_query: %w", err)
	}

	count := result.Total

	// Externally-blocked exclusion (ka-0000002198). Subtract any matched bead
	// that is blocked_by an OPEN bead of a configured type (escalation /
	// decision) so a gardener loop_end does not treat legitimately-waiting
	// children as "work still to do". Fail-safe in every direction: on any
	// uncertainty the count is left untouched, so the gardener keeps looping
	// rather than stopping while real work remains. Applied before the
	// closed-root short-circuit below so both can compose (a closed root still
	// wins and forces the count to 0).
	if filterBlocked && count > 0 {
		count = subtractExternallyBlocked(ctx, client, l, result, count)
	}

	// Closed-root-epic terminal short-circuit (ka-0000008051).
	//
	// When a bead_query is scoped to a Parent subtree, the count it returns is
	// "open work anywhere in the transitive subtree" — it never inspects the
	// root's own status. A closed root epic is the operator's "this is done"
	// signal; grafting follow-on work under it (a new sub-epic, a stray task)
	// keeps the subtree count > 0 and would otherwise spawn a schedule/gardener
	// forever. So when the named Parent is itself closed, treat the subtree as
	// terminal (count 0) regardless of residual descendants, and log loudly
	// naming what was masked so the operator can re-home that work.
	if l.Parent != "" && count > 0 {
		if sc, ok := client.(beadStatusClient); ok {
			status, serr := sc.GetBeadStatus(ctx, l.Parent)
			if serr != nil {
				// Fail-safe: if we can't determine the parent's status, fall
				// back to the legacy subtree-count behaviour rather than
				// silently dormanting a live loop.
				slog.Warn("bead_query: could not fetch parent status; using subtree count",
					"parent", l.Parent, "error", serr)
			} else if status == "closed" {
				slog.Warn("bead_query: root epic is CLOSED — treating subtree as terminal (auto-dormant), residual open work in subtree is being ignored",
					"parent", l.Parent, "residual_open_in_subtree", count)
				count = 0
			}
		}
	}

	return count, nil
}

// beadQueryHeldByScanCap bounds a held_by count: the endpoint returns rows, not a total.
const beadQueryHeldByScanCap = 500

// evalHeldByCount counts the escalations a lane holds (Leaf.HeldBy), in every project.
func evalHeldByCount(ctx context.Context, client BeadClient, l Leaf) (int, error) {
	if len(l.Types) > 0 && (len(l.Types) != 1 || l.Types[0] != "escalation") {
		return 0, fmt.Errorf("bead_query: held_by counts escalations only, got types %v", l.Types)
	}
	if l.Project != "" || l.Parent != "" || len(l.Labels) > 0 || len(l.ExcludeLabels) > 0 ||
		len(l.ExcludeBlockedByOpenTypes) > 0 || l.ShortTitleMissing {
		return 0, fmt.Errorf("bead_query: held_by takes no project, parent, label, exclusion or short_title filter")
	}
	statuses := l.Statuses
	if len(statuses) == 0 {
		statuses = []string{"open", "in_progress"}
	}
	result, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"escalation"},
		Statuses: statuses,
		HeldBy:   l.HeldBy,
		Limit:    beadQueryHeldByScanCap,
	})
	if err != nil {
		return 0, fmt.Errorf("bead_query held_by: %w", err)
	}
	return len(result.Beads), nil
}

// subtractExternallyBlocked returns count minus the number of matched beads
// that are provably NON-ACTIONABLE — i.e. no crew agent could pick them up this
// cycle even in principle (ka-0000002198, transitive fix ka-0000004214). It is
// only called when l.ExcludeBlockedByOpenTypes is non-empty and count > 0.
//
// A matched child is non-actionable when ANY of:
//
//   - TRANSITIVELY OPERATOR-BLOCKED (fix option 1): every path down its forward
//     blocking-dep chain terminates in an OPEN bead of a type in
//     l.ExcludeBlockedByOpenTypes (escalation / decision). The original exclusion
//     was ONE HOP — it only subtracted a child DIRECTLY blocked by an open
//     escalation/decision. But real multi-phase epics gate later phases behind
//     an intermediate open TASK that is itself blocked by the escalation, so the
//     one-hop check counted every transitively-gated child as actionable and the
//     dormancy brake never engaged (225 runs / $762 on ka-0000001163). We now
//     walk the chain to a fixpoint (see resolvesToOperator below).
//   - OPERATOR-ASSIGNED with no live agent-actionable blocker (fix option 3): a
//     leaf child with a human-operator assignee (e.g. an operator-owned policy
//     decision task like ka-0000006926) and no OPEN non-escalation blocker. A
//     human owns it; no crew agent can self-serve it.
//   - A NON-ACTIONABLE TYPE (fix, bundle handling): a type=bundle plan-of-record
//     narrative child, which crew never work to closure.
//
// Fail-safe contract (unchanged): this function only ever REDUCES the count, and
// only for beads it can positively prove non-actionable. Every uncertainty — the
// client lacks the batch capability, the row scan was truncated, a
// dependency/bead fetch errored, a blocking chain runs past the depth/node cap,
// or a dep target's detail could not be fetched — leaves that bead COUNTED, so
// the gardener keeps looping rather than stopping while real work may remain. It
// never returns a value below zero. A blocking cycle resolves to "not proven"
// (counted), never to a phantom subtract.
//
// "Blocked by" mirrors the daemon's FilterNoOpenDeps semantics exactly: a
// FORWARD blocking-typed dep (blocks / blocked-by / depends-on) whose target
// bead is OPEN (open|in_progress|deferred). The escalation→work link
// (gb escalation create --requesting-task) is written as a forward `blocks`
// dep from the blocked task to the escalation, so this is precisely the edge
// that pauses the work.
func subtractExternallyBlocked(ctx context.Context, client BeadClient, l Leaf, result *beadsapi.ListBeadsResult, count int) int {
	resolver, ok := client.(blockedByResolver)
	if !ok {
		// Client can't resolve deps — keep the full count (fail-safe).
		slog.Warn("bead_query: exclude_blocked_by_open_types set but client lacks batch capability; keeping full count",
			"parent", l.Parent, "count", count)
		return count
	}

	// We need every matched row to inspect its deps. If the daemon returned a
	// truncated page (Total exceeds what we scanned), we cannot know which of
	// the un-scanned rows are blocked, so we must not exclude anything.
	if len(result.Beads) < result.Total {
		slog.Warn("bead_query: matched set exceeds scan cap; skipping externally-blocked exclusion (fail-safe)",
			"parent", l.Parent, "total", result.Total, "scanned", len(result.Beads), "cap", beadQueryBlockedScanCap)
		return count
	}

	wanted := make(map[string]struct{}, len(l.ExcludeBlockedByOpenTypes))
	for _, t := range l.ExcludeBlockedByOpenTypes {
		wanted[t] = struct{}{}
	}

	// detailByBead / depsByBead accumulate the local slice of the block graph we
	// need to decide each child. Children are seeded from the matched rows (which
	// already carry type/status/assignee); blockers are fetched level-by-level.
	detailByBead := make(map[string]*beadsapi.BeadDetail, len(result.Beads))
	childIDs := make([]string, 0, len(result.Beads))
	for _, b := range result.Beads {
		if b == nil || b.ID == "" {
			continue
		}
		if _, dup := detailByBead[b.ID]; dup {
			continue
		}
		detailByBead[b.ID] = b
		childIDs = append(childIDs, b.ID)
	}
	if len(childIDs) == 0 {
		return count
	}

	depsByBead := make(map[string][]beadsapi.Dependency, len(childIDs))
	failed := false // set on any hard fetch error → keep full count (fail-safe)

	// Bounded BFS: expand the forward blocking-dep chain outward from the matched
	// children. depsByBead[id] is populated for every bead whose deps we fetched;
	// detailByBead[id] for every bead whose type/status/assignee we resolved. A
	// bead missing from either map at evaluation time is treated as unprovable.
	frontier := append([]string(nil), childIDs...)
	for depth := 0; depth < maxTransitiveBlockDepth && len(frontier) > 0 && !failed; depth++ {
		// (a) fetch forward deps for frontier beads we don't have them for yet.
		need := make([]string, 0, len(frontier))
		for _, id := range frontier {
			if _, have := depsByBead[id]; !have {
				need = append(need, id)
			}
		}
		if len(need) > 0 {
			deps, err := resolver.BatchGetDependencies(ctx, need, "forward")
			if err != nil {
				slog.Warn("bead_query: could not fetch deps for externally-blocked exclusion; keeping full count (fail-safe)",
					"parent", l.Parent, "depth", depth, "error", err)
				failed = true
				break
			}
			for _, id := range need {
				depsByBead[id] = deps[id] // nil (no edges) is a valid, terminal answer
			}
		}

		// (b) collect the distinct blocking-dep targets we have no detail for yet.
		nextIDs := make([]string, 0)
		queued := make(map[string]struct{})
		for _, id := range frontier {
			for _, d := range depsByBead[id] {
				if !isBlockingDepType(d.Type) || d.DependsOnID == "" {
					continue
				}
				if _, known := detailByBead[d.DependsOnID]; known {
					continue
				}
				if _, dup := queued[d.DependsOnID]; dup {
					continue
				}
				queued[d.DependsOnID] = struct{}{}
				nextIDs = append(nextIDs, d.DependsOnID)
			}
		}
		if len(nextIDs) == 0 {
			break
		}
		// Node-budget guard: past the cap, stop expanding. The still-undiscovered
		// blockers stay absent from detailByBead and evaluate as unprovable
		// (fail-safe: their dependents remain counted).
		if len(detailByBead)+len(nextIDs) > maxTransitiveBlockNodes {
			slog.Warn("bead_query: externally-blocked walk hit node cap; leaving deep blockers unresolved (fail-safe)",
				"parent", l.Parent, "nodes", len(detailByBead), "cap", maxTransitiveBlockNodes)
			break
		}

		// (c) fetch detail for the next level so we can read its type/status.
		beads, err := resolver.BatchGetBeads(ctx, nextIDs)
		if err != nil {
			slog.Warn("bead_query: could not fetch blocker beads for externally-blocked exclusion; keeping full count (fail-safe)",
				"parent", l.Parent, "depth", depth, "error", err)
			failed = true
			break
		}
		// (d) the next frontier is the newly-discovered OPEN, non-terminal
		// blockers — the only ones whose OWN deps we must still walk. An operator
		// blocker (open escalation/decision) is a terminal accept; a closed
		// blocker is not a live wait; either way there is nothing to recurse into.
		nextFrontier := make([]string, 0, len(nextIDs))
		for _, id := range nextIDs {
			b := beads[id]
			if b == nil {
				continue // fetch miss → stays unprovable
			}
			detailByBead[id] = b
			if !isOpenBlockerStatus(b.Status) {
				continue
			}
			if _, isOperator := wanted[b.Type]; isOperator {
				continue
			}
			nextFrontier = append(nextFrontier, id)
		}
		frontier = nextFrontier
	}
	if failed {
		return count
	}

	// The block graph is now materialized in detailByBead / depsByBead. Two
	// related-but-distinct questions are answered over it, and keeping them
	// distinct is the whole fix for ka-0000006656:
	//
	//   - blockerResolvesToOperator(id): does a BLOCKER bead keep its dependent
	//     operator-gated? It is an operator wait if it is ITSELF an OPEN bead of a
	//     wanted type (escalation/decision — or task/bug when a trigger opts them
	//     in), or if every live blocking path out of it bottoms out at one.
	//   - childIsNonActionable(id): is a MATCHED CHILD one no crew agent can pick
	//     up this cycle? A child is judged by its BLOCKERS, its assignee, and its
	//     type — it is NEVER accepted merely for having a wanted type ITSELF.
	//
	// The asymmetry is the bug fix. The old single function terminal-accepted ANY
	// wanted-type open bead, then reused that same function for the matched
	// children in the outer loop. That was harmless while the wanted set was
	// {escalation,decision} (a matched child is never those types), but the moment
	// task/bug joined the set (escalation ka-0000002943) EVERY matched task/bug
	// child self-accepted on the first line of the recursion — before its blockers
	// were ever inspected — flooring the effective count to 0 and dark-ing the
	// intake-groomer lane for two days (ka-0000006656). A matched child's OWN type
	// must never trip the terminal accept; only its blockers may.
	memo := make(map[string]bool, len(detailByBead))
	onStack := make(map[string]bool)

	// liveOpenBlockerIDs returns id's live (open) forward blocking-dep targets.
	// ok=false when id's deps were never fetched (past the depth/node horizon) or a
	// blocker's detail is unresolvable — either way the caller must treat id as NOT
	// proven non-actionable (fail-safe: leave it counted).
	liveOpenBlockerIDs := func(id string) (ids []string, ok bool) {
		deps, haveDeps := depsByBead[id]
		if !haveDeps {
			// Deps never fetched (past the depth/node horizon): we cannot enumerate
			// this bead's blockers, so we cannot prove it is fully operator-gated.
			return nil, false
		}
		for _, d := range deps {
			if !isBlockingDepType(d.Type) || d.DependsOnID == "" {
				continue
			}
			t := detailByBead[d.DependsOnID]
			if t == nil {
				return nil, false // blocker detail unresolved → unprovable path
			}
			if !isOpenBlockerStatus(t.Status) {
				continue // closed blocker: not a live wait
			}
			ids = append(ids, d.DependsOnID)
		}
		return ids, true
	}

	// blockerResolvesToOperator reports whether a BLOCKER bead id keeps its
	// dependent operator-gated. Memoized; a bead currently on the recursion stack
	// (a cycle) resolves to false, and so does any bead whose detail/deps we could
	// not resolve. Only ever entered for beads reached AS A BLOCKER (a forward
	// blocking-dep target) — never for a matched child being decided.
	var blockerResolvesToOperator func(id string) bool
	blockerResolvesToOperator = func(id string) bool {
		if v, done := memo[id]; done {
			return v
		}
		if onStack[id] {
			return false // cycle → not proven (fail-safe)
		}
		b := detailByBead[id]
		if b == nil {
			return false // unknown bead (fetch miss / past horizon) → not proven
		}
		// Terminal accept: an OPEN blocker of a wanted type IS the operator/blocked
		// wait this filter subtracts on. Legitimate here because id was reached as
		// a blocker of some other bead — see childIsNonActionable for why a matched
		// child must NOT be accepted on its own type.
		if _, isOperator := wanted[b.Type]; isOperator && isOpenBlockerStatus(b.Status) {
			memo[id] = true
			return true
		}
		onStack[id] = true
		defer delete(onStack, id)

		blockers, ok := liveOpenBlockerIDs(id)
		if !ok {
			memo[id] = false
			return false
		}
		if len(blockers) == 0 {
			// Leaf: no live blocking deps. Operator-assigned (human) leaves are
			// non-actionable (fix option 3); anything else is self-servable work.
			r := assigneeIsHumanOperator(b.Assignee)
			memo[id] = r
			return r
		}
		// Every live blocking path must itself bottom out at operator action.
		for _, t := range blockers {
			if !blockerResolvesToOperator(t) {
				memo[id] = false
				return false
			}
		}
		memo[id] = true
		return true
	}

	// childIsNonActionable decides a MATCHED CHILD. Unlike a blocker it is never
	// accepted on its own type — only for being a non-actionable type (bundle), an
	// operator-assigned leaf, or having EVERY live blocking path terminate in an
	// operator wait. This is what keeps a matched task/bug child (the intake-groomer
	// lane's rows) counted when it is genuinely unblocked (ka-0000006656).
	childIsNonActionable := func(id string) bool {
		b := detailByBead[id]
		if b == nil {
			return false
		}
		if isNonActionableChildType(b.Type) {
			return true // bundle etc. — never crew-actionable
		}
		blockers, ok := liveOpenBlockerIDs(id)
		if !ok {
			return false // couldn't enumerate its blockers → not proven
		}
		if len(blockers) == 0 {
			// Unblocked leaf: actionable work unless a human operator owns it.
			return assigneeIsHumanOperator(b.Assignee)
		}
		for _, t := range blockers {
			if !blockerResolvesToOperator(t) {
				return false
			}
		}
		return true
	}

	excluded := 0
	for _, id := range childIDs {
		if childIsNonActionable(id) {
			excluded++
		}
	}

	if excluded == 0 {
		return count
	}
	effective := max(count-excluded, 0)
	slog.Info("bead_query: excluded non-actionable children from loop_end count",
		"parent", l.Parent, "raw_count", count, "non_actionable", excluded,
		"effective_count", effective, "blocked_by_types", l.ExcludeBlockedByOpenTypes)
	return effective
}
