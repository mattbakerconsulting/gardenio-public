# Sequencer ↔ report linking + decision-block visibility — research doc

**Epic:** ka-0000005836 (on ice — research/scoping only)
**Scoping task:** ka-0000005458
**Sibling epics (frozen):** ka-0000008153 (artifact bundles + multi-doc viewer), ka-0000003558 (project view + epic sequencer)
**Author:** gardenio-scope-kd5rjgbi9vdz-sequencerreport-ll5 (2026-05-09)

## TL;DR

Two independent legibility wins on top of the **project-view sequencer** (ka-0000003558) and the **bundle viewer** (ka-0000008153):

1. **`has_report` predicate** — render a "📄 report" badge on any sequencer card or list row whose bead has a deliverable attached. Predicate is the union of three concrete shapes that already exist: child `type=rich-report`, child `type=report` (decision-artifact), or future child `type=bundle` (ka-0000008153). No schema change required for v1; an indexed materialized column is a v2 perf optimization.
2. **`blocked_on_decision` predicate** — render a "⏸ waiting on you" badge on any bead whose subtree currently holds an open `type=decision` bead. Predicate is "exists open `type=decision` bead reachable from this bead via parent-child or `requesting_agent_bead_id` field". The agent-bead `gate_satisfied_by` field is a stronger signal for the *agent itself* but not for the parent task; we surface both.

Both predicates are pure graph queries against existing fields. Sibling epic ka-0000008153 adds a new flavor (`bundle`) — wire `has_report` to recognize it when that epic ships, no rework needed.

## Scope and non-scope

**In scope (this research):**
- Canonical predicates for `has_report` and `blocked_on_decision`.
- UX shape of the badge in the project-view sequencer (ka-0000003558) and in `/active`, `/decisions`, bead detail pages.
- CLI filter shape (`kd list --has-report`, `kd list --blocked-on-decision`).
- Storage tradeoff (graph traversal vs indexed column) with a v1/v2 split.
- Acceptance criteria for each child task so they can be picked up by an implementer.

**Out of scope (will be revisited when the parent epic unfreezes):**
- Implementation. This doc files child tasks; nothing here is built.
- Bundle bead schema (sibling ka-0000008153 owns it). This doc only commits to the *protocol*: "if a child bead is `type=bundle`, treat the parent as `has_report=true`."
- Project-view sequencer rendering (sibling ka-0000003558). This doc commits to the slot: "the sequencer card has a small badge area below the title."

## Context: what already exists in the codebase

These are facts pulled from the current source tree, not aspirational:

| Concept | Where | Notes |
|---|---|---|
| `type=report` bead | `kbeads/internal/server/config.go:286` | Decision-artifact bead. Required field `decision_id`. Created by `gb decision report`. |
| `type=rich-report` bead | `kbeads/internal/server/config.go:307` (issue-kind) | Incident-intel container with widget-instance children. Rendered by `gardenio/controller/cmd/beads-viewer/rich_report.go`. |
| `type=decision` bead | `kbeads/internal/server/config.go:248` | Has `requesting_agent_bead_id` field linking back to the agent that opened it. |
| Agent gate state | `type=agent.gate_satisfied_by` field | Set to `yield` or `operator`; cleared on next gate cycle. |
| Agent → open decision | `type=agent.decision_bead_id` field | Populated while agent is yielded on a decision. |
| Subtree query | `ListBeadsQuery.Parent` (`gardenio/controller/internal/beadsapi/client_query.go:24`) | Daemon-side transitive parent-child traversal. We already use this; v1 predicate piggybacks on it. |
| Reverse mention lookup | `kd refs mentioned-by <id> --type=parent` | Typed-mention edges (`parent(ka-X)`) are indexed via `bead_references.relationship_type` (kbeads CLAUDE.md). |
| Decisions list page | `gardenio/controller/cmd/beads-viewer/decisions.go` | Already filters `Types=["decision"], Statuses=["open"]`. The `/active` page already counts open decisions for its banner. |
| Constellation type icons | `gardenio/controller/cmd/beads-viewer/constellation.go:1233` | `decision` → `?`, `rich-report` already known. Badge work can reuse this registry. |

## Predicate 1: `has_report`

### Definition

> A bead has a report iff at least one **direct or transitive child** of the bead has type ∈ {`report`, `rich-report`, `bundle`} **and** status ≠ `deleted`.

Intentionally inclusive: child types may grow (sibling epic ka-0000008153 adds `bundle`). Children carrying these types are the *artifact*; the parent bead is the *task that produced it*.

**Why "direct or transitive child"** — most agent reports are filed as direct children of the originating task (advice ka-0000008096 pushes this). But epics often own a deliverable through a child task that itself owns the report. Two-level traversal is enough in practice; we still scope it to subtree because that's what the daemon already supports cheaply via `ListBeadsQuery.Parent`.

**Why ignore typed-mention edges** for v1 — typed `parent(ka-X)` mentions are auto-promoted to dep edges (kbeads CLAUDE.md), so a report that says `parent(ka-X)` already shows up as a parent-child dep. We do not need a separate query path.

### Shapes intentionally excluded

- **Untyped mentions** ("the report at /docs/foo.md") — too noisy. If an agent wants the badge, it must file the report as a child bead. This is the lever that makes advices ka-0000000831 and ka-0000008096 operational.
- **Decision-artifact reports without `parent` linkage** — `type=report` is created with `decision_id` but no automatic parent-child edge to the original task. We file a follow-up task to add that edge at creation time (see child task list below). Until that lands, decision-artifact `type=report` beads are visible on the *decision* bead's report badge but not the *task* bead's report badge — which is correct; the report is "of" the decision.
- **Bead-description content** — descriptions are not deliverables. `has_report` means "click here to read a thing", not "scroll the bead body".

### v1 implementation sketch

Daemon side, in `kbeads/internal/store/postgres/`:

```sql
-- inside the existing ListBeads pipeline, when has_report=true is requested
EXISTS (
  SELECT 1
  FROM deps d
  JOIN beads c ON c.id = d.child_id
  WHERE d.parent_id = b.id
    AND d.dep_type = 'parent-child'
    AND c.type IN ('report', 'rich-report', 'bundle')
    AND c.status <> 'deleted'
) AS has_report
```

For the *transitive* version, recurse one more level (depth 2 only) — beyond that the predicate loses meaning ("any descendant has a report" is too generous for an epic). v1 uses depth-1; v2 may extend to depth-2 if user feedback asks for it.

### v2 (perf only): materialized column

If the `EXISTS` subquery becomes a hot path on the project-view sequencer (which renders many beads at once), materialize it as `beads.has_report BOOLEAN` updated by trigger on `deps` insert/delete and on `beads.type/status` change. **Not v1.** Filed as a follow-up; the trigger work is non-trivial and `EXISTS` is cheap on indexed `deps(parent_id, dep_type)`.

## Predicate 2: `blocked_on_decision`

### Definition

> A bead is blocked on a decision iff at least one **open `type=decision` bead** has any of:
> - parent-child dep edge to this bead (or to a descendant of this bead, depth ≤ 2);
> - `requesting_agent_bead_id` pointing to an agent bead that is parented under this bead.

**Two pathways, same result:**
- The "decision filed against the task" pathway — operator escalation flow, where `gb decision create` is filed with a parent task.
- The "agent yielded mid-task" pathway — `gb decision create` from inside an agent session creates a decision whose `requesting_agent_bead_id` points back at the agent. The agent itself is parented under the task it was working on.

Both pathways already exist in the codebase. v1 surfaces both.

### Why not just use `type=agent.gate_satisfied_by`

Two reasons:
1. `gate_satisfied_by` is *agent-scoped*. The project-view sequencer renders tasks/epics, not agents. We need a predicate on the *task*.
2. `gate_satisfied_by` resets after each gate cycle, while a parent task can be blocked on a still-open decision after the agent has been despawned. The agent bead state is fragile; the open decision bead is durable.

We do still surface `gate_satisfied_by` on agent rows (it already drives stop-gate UI). The two badges can co-exist: an agent row shows "🟡 yielded" and the parent task row shows "⏸ waiting on you" because of the same underlying decision.

### v1 implementation sketch

```sql
-- "decision filed against this bead or its subtree"
EXISTS (
  SELECT 1
  FROM beads d
  JOIN deps dep ON dep.child_id = d.id
  WHERE d.type = 'decision'
    AND d.status = 'open'
    AND dep.dep_type = 'parent-child'
    AND dep.parent_id IN (b.id, /* descendants depth ≤ 2 */ ...)
)
OR
-- "agent yielded under this bead has an open decision"
EXISTS (
  SELECT 1
  FROM beads ag
  JOIN deps dep ON dep.child_id = ag.id
  WHERE ag.type = 'agent'
    AND ag.fields->>'decision_bead_id' IS NOT NULL
    AND dep.dep_type = 'parent-child'
    AND dep.parent_id IN (b.id, /* descendants ... */)
    AND EXISTS (SELECT 1 FROM beads d2
                WHERE d2.id = ag.fields->>'decision_bead_id'
                  AND d2.status = 'open')
)
```

The second branch is the messy one — JSON field traversal plus a join. v2 may add a materialized `beads.blocked_on_decision_bead_id` column if this becomes a hot path. v1 sticks with the query.

### Edge cases worth nailing in tests

- A decision filed against an unrelated bead must not flag this bead.
- A *closed* decision must clear the badge immediately. Tests should write open → closed and assert the predicate flips.
- An agent bead with `decision_bead_id` set but the referenced decision *closed* must not flag — covers the "agent crashed before clearing" case.
- A `type=decision` bead whose `requesting_agent_bead_id` points at an agent that is no longer parented under any task (orphaned) must not crash the query; it just doesn't flag any task.

## UX

### Badge surface in the project-view sequencer (sibling ka-0000003558)

Each sequencer card already has: status color (open=green, closed=red), title, type icon. **Add a small badge row below the title:**

- `📄` (filled if `has_report=true`, dim if false). Hover: "N reports attached — click to open the bundle viewer". Click: navigates to the bundle/report (route TBD by sibling ka-0000008153 — for v1, navigates to the first child report bead's detail page).
- `⏸` (visible iff `blocked_on_decision=true`). Hover: "Blocked on open decision: <prompt>". Click: navigates to the decision bead.

**Visual budget:** two icon slots, monochrome, fits within the 16px card metadata strip already in the project_row template. No layout change to the sequencer beyond reserving those two slots.

### Same badges in `/active` and bead detail header

`/active` already groups beads by project and shows status chips. Add the same two badges next to the title in the row template (`gardenio/controller/cmd/beads-viewer/templates/active.html`). Bead detail page `bead_show.html` gets them in the title bar.

`/decisions` does **not** need the `blocked_on_decision` badge — every row on that page is a decision, so the predicate is always true and adds noise. It does benefit from the `has_report` badge: a resolved decision that has a `decision report` artifact attached can show 📄.

## CLI

Two new filters on `kd list`:

```
kd list --has-report                  # bool, default false
kd list --blocked-on-decision         # bool, default false
kd list --has-report=false            # explicit negation (find tasks that haven't produced a deliverable)
```

Wire-format: query params `has_report=true`, `blocked_on_decision=true` on `GET /v1/beads`. Both filters compose with existing `--type`, `--status`, `--label`, `--parent` etc. with AND semantics.

**Why no `--has-decision` filter** — `kd list -t decision -s open` already does this and is the right surface. `--blocked-on-decision` is the *parent-side* filter and that's the new capability.

**Why no `--has-bundle` separate from `--has-report`** — bundles are one of three shapes that satisfy `has_report`; surfacing them separately encourages writers to file untyped reports instead of bundles, which fights the goal of advice ka-0000008096. Keep one knob.

## Storage tradeoff (v1 vs v2)

| Question | v1 answer | v2 answer (only if needed) |
|---|---|---|
| Where does the predicate live? | Subquery in `ListBeads` SQL | Materialized `beads.has_report` / `beads.blocked_on_decision_bead_id` columns |
| What triggers updates? | n/a (computed at query time) | Triggers on `deps` insert/delete + `beads.type/status` update |
| What does it cost on the project view? | One extra `EXISTS` per row × N rows | Index lookup, ~free |
| Migration cost? | Zero | New columns, backfill via batched `UPDATE ... LIMIT N` per kbeads migration policy |

**Recommendation:** ship v1 with the subquery and a metric on `ListBeads` p95 latency. Promote to v2 only if the project-view sequencer (sibling ka-0000003558) actually loads enough rows to make this matter. The kbeads migration policy explicitly forbids eager backfills; doing v2 prematurely would burn migration budget.

## Open design forks (resolved by author, flag for review during implementation)

1. **Depth of subtree traversal for `has_report`.** Author picks **depth 1 (direct children only)** for v1. Rationale: simpler SQL, matches the pattern reports are actually filed in. If the project-view sequencer renders epics whose deliverables live two levels deep (epic → task → report), we'll see it in user feedback and bump to depth 2. This is one knob change in the query.
2. **Should `type=rich-report` count as a report for the badge?** **Yes.** Same purpose (a renderable deliverable), same UX intent. Keeps the predicate honest.
3. **Decision-artifact `type=report` lacking parent-child edge to the original task.** Author files a child task to add that edge at creation time (in `gb decision report`). Once that lands, the badge surfaces decision artifacts on the *originating task*, which is what an operator scanning the project view actually wants.
4. **Bundle support.** Wire `has_report` to recognize `type=bundle` *now* even though the type doesn't exist yet — sibling epic ka-0000008153 will create it, and a no-op `IN (..., 'bundle')` clause is harmless until then.

No design fork is severe enough to warrant a `gb decision`; if the implementer disagrees with #1 or #3, file a comment on the relevant child task and re-scope.

## Coordination with sibling scoping crew

Sibling scoping tasks for ka-0000008153 (bundles) and ka-0000003558 (project view) are running in parallel. This doc:

- **Depends on** ka-0000008153 for the eventual bundle bead schema. Mention by ID, no duplication: when their doc lands, the bundle predicate clause becomes a real type. If they pick a non-`bundle` name (e.g. `artifact-set`), the implementer of child task **R3** swaps the constant in one line.
- **Depends on** ka-0000003558 for the project-view rendering surface. Mention by ID: this doc commits to a "two-icon badge slot under the card title" — if their doc picks a different layout, the implementer of child task **R5** adapts.
- **Sibling scoping crews should not file overlapping child tasks.** This doc owns the predicates and badges; bundle authoring/viewing belongs to ka-0000008153; sequencer layout/colors belong to ka-0000003558.

## Child task list (filed under ka-0000005836)

Ordered. Each task is single-purpose; each cites this doc + acceptance criteria.

| # | Title | Why ordered here | Blocks |
|---|---|---|---|
| R1 | Daemon: add `has_report` predicate to `ListBeads` (+ HTTP query param) | Foundation; everything else needs it | R2, R5 |
| R2 | Daemon: add `blocked_on_decision` predicate to `ListBeads` (+ HTTP param) | Sibling of R1, independent | R5 |
| R3 | `gb decision report`: file child parent-child edge from `type=report` to the originating task | Makes decision-artifact reports show up on the task badge | (none — independent ship) |
| R4 | `kd list`: surface `--has-report` and `--blocked-on-decision` flags | Operator-facing CLI lens | (none — depends only on R1 + R2) |
| R5 | beads-viewer: render `📄` and `⏸` badges in `/active`, project-view sequencer card, and bead detail header | The visible deliverable | (none — terminal) |
| R6 | beads-viewer: extend `has_report` SQL to recognize `type=bundle` once sibling ka-0000008153 ships the schema | Wired-on-arrival; trivial follow-up | (none — depends on sibling epic landing) |
| R7 | (v2, defer) Materialize `has_report` / `blocked_on_decision_bead_id` columns + triggers + backfill | Perf only; only if R5 reveals latency | (none — defer) |

### Acceptance criteria

**R1 — daemon `has_report` predicate.**
- New SQL predicate as defined above (depth-1 subtree, types ∈ `{report, rich-report, bundle}`, status ≠ `deleted`).
- New `ListBeadsQuery.HasReport *bool` field; nil = no filter, `*true` = only beads with reports, `*false` = only beads without.
- HTTP query param `has_report=true|false` on `GET /v1/beads`.
- Field returned on every bead row in the response: `has_report bool`.
- Unit tests: bead with one `type=report` child returns true; bead with `type=task` child returns false; bead with `deleted` `type=report` child returns false; bead with `type=bundle` child returns true (will be vacuously satisfied until R6).
- Integration test against real Postgres covering the three shapes.

**R2 — daemon `blocked_on_decision` predicate.**
- New SQL predicate as defined above (open `type=decision` filed against this bead/subtree OR via `requesting_agent_bead_id` of an agent under this bead).
- New `ListBeadsQuery.BlockedOnDecision *bool`.
- HTTP query param `blocked_on_decision=true|false`.
- Field returned per row: `blocked_on_decision bool`, plus `blocked_on_decision_bead_id string` (the ka-id of the open decision so the UI can link to it; first one if multiple).
- Unit tests: each pathway (parent-child decision, agent-yielded decision), each negative case from "Edge cases" above.

**R3 — link `type=report` to originating task.**
- `gb decision report` creates a parent-child dep from the new `type=report` bead to the bead the decision was filed against (i.e. the parent of the `type=decision` bead, or the parent of the `requesting_agent_bead_id` agent).
- If the decision has no inferable originating task (operator-created standalone decision), no edge is added — log at INFO level, no error.
- Backfill: one-shot `kd admin link-decision-reports` command that walks existing closed decisions with reports and adds the missing edges. Bounded `LIMIT N` per kbeads migration policy.
- Test: spawn a decision under a task, submit a report, assert the new dep edge exists and `has_report` (after R1) flips to true on the task.

**R4 — `kd list` flags.**
- Two new boolean flags `--has-report` and `--blocked-on-decision`. Both default false (no filter); explicit `=false` means "exclude".
- Flags compose with existing filters via AND.
- Tests: golden CLI test that constructs the right query params; client unit test for round-trip.

**R5 — beads-viewer badges.**
- New 16-px badge slot in `templates/partials/project_row.html`, `templates/active.html` row, `templates/bead_show.html` header.
- `📄` filled when `has_report=true`, dimmed/hidden when false (pick one — author leans hidden, implementer decides).
- `⏸` shown only when `blocked_on_decision=true`. Hover tooltip: "Blocked on open decision". Click: navigates to `blocked_on_decision_bead_id`.
- Decision page: only the 📄 badge applies (every row is a decision, so the ⏸ badge is suppressed).
- Snapshot tests for each template variant.
- Manual test: open `/active` against a dev daemon, verify both badges render and link correctly.

**R6 — bundle wire-up.**
- After ka-0000008153 lands, ensure the `IN ('report','rich-report','bundle')` clause matches the actual bundle bead type name.
- If the sibling epic chose a different name, swap the constant; if they chose multiple types, extend the IN list.
- Test: a bundle child returns `has_report=true` on its parent.

**R7 — v2 materialized columns (defer).**
- Only file/start if R5 metrics show `ListBeads` p95 latency degradation > 50 ms attributable to the new predicates.
- Two new BOOL/TEXT columns + triggers on `deps`, `beads.type`, `beads.status`.
- Backfill via batched `UPDATE … LIMIT N` per kbeads migration policy (kbeads CLAUDE.md "Migration policy").
- Switch `ListBeads` to read materialized columns when present; keep subquery as fallback for one release.

## Appendix: pointers

- Epic frozen idea: `kd show ka-0000005836`.
- Sibling epics: `kd show ka-0000008153`, `kd show ka-0000003558`.
- Reporting/linking advices (the philosophical motivation): `gb advice show ka-0000000831`, `gb advice show ka-0000008096`.
- Existing `decisions` page handler: `gardenio/controller/cmd/beads-viewer/decisions.go`.
- Existing `active` page banner counting open decisions: `gardenio/controller/cmd/beads-viewer/active.go:139`.
- `rich-report` rendering: `gardenio/controller/cmd/beads-viewer/rich_report.go`.
- Type registry where new predicates' SQL and field defs live: `kbeads/internal/server/config.go`.
- Existing subtree query primitive: `gardenio/controller/internal/beadsapi/client_query.go:24` (`ListBeadsQuery.Parent`).
- Migration policy (don't eager-backfill, batched updates): `kbeads/CLAUDE.md` "Migration policy" section.
