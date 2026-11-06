# Git Is Not All You Need

### How an object-oriented Postgres work graph (beads) enables things pure Git cannot

**Project:** gardenio · **Tracking epic:** [[ka-0000003159]]
**Operator:** Matt Baker · **Date:** 2026-06-10

---

## Abstract

Git is the substrate of modern software work, and a generation of tools has tried to
make it the substrate of *coordination* too: issues-as-files, plan-as-markdown,
state-in-commit-messages, automation-via-hooks. This paper argues that for an
autonomous, multi-agent engineering platform, **Git is not enough** — not because Git
is bad at what it does, but because the thing we need is categorically different from
what Git is.

Git is a content-addressed, immutable, distributed store of file *snapshots*. The
gardenio platform needs a **live, shared, queryable, event-emitting graph of typed work
objects** — one that recomputes derived state when neighbours change, dispatches a
ready-queue across a recursive dependency graph, attributes every mutation to a
validated human identity, and acts as the control plane that wakes a fleet of agents.
That object lives in beads: an object-oriented work database backed by PostgreSQL.

Every claim below was validated by a five-agent team against the **live gardenio
production database (27,251 beads)** and the source tree. Where the implementation is
weaker than the marketing, we say so explicitly — the corrections are part of the
argument, because the honest version is still decisive.

---

## 1. The category error

There is a recurring temptation to treat Git as a database. It has commits (rows),
branches (versions), a log (history), and hooks (triggers). With enough discipline you
can keep a `TODO.md`, encode status in a directory layout, and fire a `post-commit`
hook. Many teams do exactly this.

The temptation is a category error. Git's data model is:

- **Content-addressed and immutable.** An object's identity *is* the hash of its
  content. You cannot change an object; you can only write a new one. History is a DAG
  of frozen snapshots.
- **File-shaped.** The unit is a blob (file bytes) inside a tree (directory). There are
  no typed fields, no relations between work items, no per-item status.
- **Local and passive.** Hooks fire only on the machine performing the local Git
  operation, see only that operation's diff, and have no shared state to coordinate
  against.

What gardenio needs is the opposite on every axis:

| Need | Git's model | Beads' model |
|---|---|---|
| Mutable status per work item | Immutable snapshots | `beads.status` column, updated in place |
| Typed relations between items | Untyped commit-parent DAG | `deps(bead_id, depends_on_id, type)` edge table |
| "What's unblocked right now?" | No per-item status to query | Recursive SQL over the open/closed graph |
| Derived rankings that update when neighbours change | No recompute mechanism | Postgres triggers refresh dependent rows |
| Semantic / structured search | `git grep` over file bytes | pgvector ANN + a parameterised query builder |
| Validated human attribution | One freetext `author` string | FK to an `operators` identity table, enforced |
| Auditable, reversible deletion | History rewrite breaks all hashes | Soft-delete + GDPR redaction-in-place |
| Fleet orchestration | Local, advisory hooks | A live event bus the controller subscribes to |

The rest of this paper takes each row and shows the code that makes it real — and the
single concrete reason Git cannot follow.

---

## 2. The typed dependency graph and the ready-queue

**The thing Git can't do: answer "what work is unblocked right now, and who should do
it next?"**

Beads model work as a typed directed graph. Edges live in their own table, not in the
beads themselves:

```sql
-- kbeads/internal/store/postgres/migrations/001_initial.up.sql:37
CREATE TABLE deps (
    bead_id       TEXT NOT NULL REFERENCES beads(id) ON DELETE CASCADE,
    depends_on_id TEXT NOT NULL REFERENCES beads(id) ON DELETE CASCADE,
    type          TEXT NOT NULL,
    PRIMARY KEY (bead_id, depends_on_id, type)
);
```

Both endpoints are foreign keys; the primary key is `(source, target, type)`, so the
same pair of beads can carry several semantically distinct edges. The edge vocabulary is
a closed, typed set (`kbeads/internal/model/dependency.go`):
`blocks`, `parent-child`, `related`, `duplicates`, `supersedes`, `triggered-by`,
`rates`. These are *domain* relationships — "X blocks Y" means something different from
"X is the parent of Y" — not file diffs.

The payoff is the **ready-queue**. `gb ready` ("show beads ready to work on: open, not
blocked") compiles to a single server-side predicate
(`kbeads/internal/store/postgres/queries.go:625`):

```sql
NOT EXISTS (
  SELECT 1 FROM deps d JOIN beads dep ON d.depends_on_id = dep.id
  WHERE d.bead_id = beads.id
    AND d.type IN ('blocks', 'blocked-by', 'depends-on')
    AND dep.status IN ('open', 'in_progress', 'deferred')
)
```

A bead is *ready* iff it has no outgoing **blocking** edge to a still-open target.
Crucially, only blocking edge types gate readiness — `parent-child` and `related` are
deliberately excluded, so a child of an open epic still surfaces (a regression that was
caught and pinned by tests). Hierarchy traversal is a `WITH RECURSIVE` CTE that uses
`UNION` for visited-set dedup, so it terminates even on a malformed graph.

On the live database this returns **4,813 ready beads out of 27,251 total**, ranked
`priority, -created_at` so a P0 can never sink below the display limit. This same
`NoOpenDeps` engine powers agent priming, the trigger engine, and the viewer's health
rails — it is the platform's central work-dispatch primitive.

**Why Git can't.** Answering "what's unblocked" requires three things Git has no
first-class form of: (1) *typed inter-item edges* — Git's only graph is the
commit-parent DAG, which is untyped and connects snapshots, not work items; (2)
*queryable per-item open/closed state, cross-item* — the predicate joins each candidate
to the *status of its blockers*, and Git has no per-item status to join on; (3) a
*recursive query engine*. A `post-commit` hook sees one diff on one machine; it cannot
evaluate "the set of all open items with no open blocker, ranked by priority." The
moment you need that, you have re-implemented a typed database on the side — which is
the whole thesis.

> **Honesty note (validated correction).** The graph is a typed *digraph*, not a
> guaranteed DAG. `kd dep add` performs a bare `INSERT ... ON CONFLICT DO NOTHING` with
> **no write-time cycle detection** (`queries.go:1025`); the only loop guard is a
> create-time check that a bead can't be its own parent (`beads.go:286`). The read path
> is cycle-*tolerant* (recursive CTEs dedup via `UNION`; the spawn-lineage walker is
> hop-bounded), but the system does **not** prevent `A→B→C→A`. The accurate claim is
> "robustly queries a typed graph," not "enforces acyclicity at write time."

---

## 3. Live, computed, derived state

**The thing Git can't do: silently re-rank item Y when an unrelated item X changes
state.**

This is the sharpest distinction in the paper, because it cuts at Git's defining
property — immutability.

Each bead carries an `urgency_score`, computed by a Postgres function from priority,
age, and the number of open beads waiting on it
(`migrations/019_urgency_score.up.sql`):

```
urgency_score = priority_weight(priority)
              × (1 + min(age_in_days, 30))
              × (1 + downstream_blocked_count)
```

The interesting part is not the formula — it's that the database **maintains** it as the
graph mutates, via three triggers:

- `trg_beads_urgency_before_insup` — stamps a bead's own score on every insert/update.
- `trg_deps_urgency_after_change` — when a dependency edge is added or removed, refreshes
  the score of the *target* (its blocked-count just changed).
- `trg_beads_urgency_after_status` — **when a bead's status flips, refreshes the score of
  every bead it depends on.**

That last trigger is the one Git cannot follow. Closing item X must change the *correct
rank* of item Y, because Y's urgency depends on how many open items still block on it. In
Postgres that's an `AFTER UPDATE` trigger reaching over to rewrite Y's stored column. In
Git, the object encoding "X is closed" is frozen by its hash; it has no mechanism to
reach over and mutate Y, and doing so would change Y's hash and invalidate every object
that references it. Git can *re-derive everything from scratch* in an external tool — but
that external tool is exactly the database.

Two more pieces of server-maintained derived state corroborate the pattern:

- **`bead_references`** (`migrations/010`): when an author pastes a `ka-0000001713` ID
  into a description, note, or comment, the server parses it into a queryable, reverse-
  indexed edge. On every write the source's references are deleted and re-extracted, so
  the table is a pure function of current text — and dangling references (typos,
  cross-fleet IDs) are *deliberately preserved* (`target_bead_id` is intentionally **not**
  a foreign key) because a broken link is signal. To Git, that pasted ID is opaque bytes.
- **`mutation_history`** (`migrations/008`): an append-only, per-field change log —
  `(bead_id, field, old_value, new_value, actor, operated_by, created_at)` — written on
  every mutation. A Git commit attributes a whole tree to one author; it does not produce
  a per-field, per-actor old→new diff log queryable by `(bead_id, field, time)`.

> **Honesty notes (validated corrections).**
> (a) The urgency triggers are fully wired and fire live, but **no query surface sorts by
> `urgency_score` yet** — `kd list --sort` doesn't expose it, and the inbox/constellation
> sort is explicitly deferred follow-up work. The load-bearing evidence is the
> *trigger-driven cross-row recompute*, not a live sorted listing.
> (b) `downstream_blocked_count` is a computed *input* recomputed at trigger time, not a
> stored column; only `urgency_score` is persisted.
> (c) The promised nightly full-table refresh cron is not yet wired; per-write triggers
> keep scores fresh on real changes, but age-only drift is not swept.

---

## 4. Rich query and retrieval

**The thing Git can't do: "find all open P1 bugs assigned to X that block an open epic"
— or "find work conceptually similar to this sentence."**

Beads ship two retrieval layers, both impossible over a commit log.

**Structured query.** `queryListBeads` (`queries.go:382`) is a parameterised WHERE-clause
builder: filter by status/type/kind/priority/assignee/owner/created-by, AND/ANY/NOT over
labels, custom JSONB fields (`fields->>'key' = $val`, present/absent checks), reference
mentions (optionally narrowed by typed relationship), parent and recursive subtree, date
ranges, and a `--no-blockers` dependency-aware filter. Live: `kd list --type bug --status
open` → 3 of 97 bugs; `kd list --no-blockers` → 17,316 of 27,251.

**Semantic search.** pgvector backs a 1024-dim embedding per bead
(`migrations/005`), with an IVFFlat cosine index. `querySemanticSearch`
(`queries_embedding.go`) runs `ORDER BY embedding <=> $query::vector` — approximate
nearest-neighbour by *meaning* — and composes it with the same structured filters. Live:
`kd search "vector similarity embeddings" --semantic` returns ranked cosine hits from
model `bedrock-titan-embed-v2`.

**Typed, validated schemas.** Custom bead types are runtime config
(`type:decision '{"fields":[{"name":"outcome","type":"enum","values":[...],"required":true}]}'`),
and writes are validated: unknown keys rejected, enum membership enforced, integer
min/max bounded, timestamps RFC-3339 parsed (`model/validate_fields.go`).

**Why Git can't.** Commits have no type, status, label, assignee, priority, or JSONB —
those attributes do not exist, so the structured query is unrepresentable. `git
log --author` filters commit authorship over file history, not "assignee on an open
task." Git has no vector storage and no notion of meaning; `git grep` is lexical
substring/regex over file bytes. Every capability here requires a separate schema'd,
indexed object store.

> **Honesty note.** The *lexical* search path is `ILIKE '%query%'` substring matching over
> title/description — **not** a Postgres `tsvector`/`tsquery` full-text index. The
> "by-meaning" retrieval is the pgvector path. Don't read "full-text" as FTS.

---

## 5. Concurrent multi-agent writes, attribution, and identity

**The thing Git can't do: let dozens of agents mutate shared work state simultaneously
without merge ceremony, and record *which human, through which agent* made each change.**

gardenio runs many autonomous agents plus humans, all writing the same work graph at
once. Git's concurrency model is serialize-via-merge: any two writers who diverge from a
common commit must reconcile — even if they touched unrelated things — because the unit
of conflict is the commit graph.

Beads use **row-level transactional concurrency**. Every mutation runs in a Postgres
transaction (`RunInTransaction`, `postgres.go:574`); field updates are *atomic
single-statement* `UPDATE`s — e.g. counter increments use
`jsonb_set(... COALESCE(...)+ $n ...)` in one statement, "no read-modify-write race"
(`queries.go:830`) — and work claims flip ownership with a scoped
`UPDATE beads SET operated_by_id=$2 WHERE id=$1` returning rows-affected (1 = won, 0 =
no-op) (`dibs.go:37`). Two agents claiming *different* beads never contend; two agents
incrementing the *same* counter serialise correctly at the row.

**First-class identity.** Attribution is not a string — it's a foreign key. Migrations
012→027 build an `operators` table (kind, subject, email, with cross-platform aliases:
Slack ↔ GitLab ↔ GitHub ↔ email), then make `operated_by_id` **NOT NULL** on beads,
comments, and deps (022), drop the legacy text column entirely (024), and require every
operator to carry a real email (027). A separate `created_by_operator_id` records the
immortal originator. The server is **fail-closed**: if it cannot resolve a human
operator, `createBead` returns an operator-refusal error (gRPC `InvalidArgument` / HTTP
400), and `gb operator-refusal` files a bug bead and squawks. Idempotency keys
(migration 029) give safe create-retries: a unique partial index turns a concurrent
double-insert into "return the winning bead."

**Auditable deletion.** Soft-delete sets `deleted_at` and is reversible (`kd restore`,
migration 009). GDPR erasure (migration 030) redacts the human-identifying fields of an
operator *in place* — display and email resolve to a sentinel — while the opaque
`operator_id` and every FK stay intact, so audit continuity survives.

**Why Git can't.** A Git commit author is a single unvalidated `Name <email>` freetext
string; nothing enforces it points to a real identity, nothing separates "human X" from
"agent Y acting for X," and there is no per-field attribution. And true erasure in Git
requires a `filter-repo` history rewrite that recomputes every downstream hash, breaking
every reference, signature, and clone — whereas beads redacts one operator's PII without
touching any other record's identity.

> **Honesty notes (validated corrections).** The store uses **MVCC + atomic statements +
> unique-index backstops**, *not* pessimistic `SELECT ... FOR UPDATE` row locks (there are
> none). Default isolation is READ COMMITTED; correctness comes from atomic statements,
> not SERIALIZABLE. And the GDPR path is **redaction-in-place**, not row deletion — the
> correct audit-vs-privacy tradeoff. The lock-free framing is a *stronger* story than
> "row locks," so state it precisely.

---

## 6. The database as the control plane

**The thing Git can't do: be the live nervous system that wakes a distributed fleet when
a graph condition becomes true.**

This is where the argument compounds. Beads is not a passive store the agents read — it
is the **control plane** that drives them.

**Events.** Every mutation flows through one chokepoint, `recordAndPublish`
(`server.go:198`): it writes a durable `events` row *and* publishes to a NATS subject
*and* broadcasts to live SSE subscribers, on typed topics (`beads.bead.created/updated/
closed`, `beads.dependency.added`, `beads.mail.sent`, …). The controller subscribes:
`conn.Subscribe("beads.bead.closed", …)` feeds the trigger engine
(`scheduler/nats_bead_closed.go:108`). That is a shared, cross-machine event stream — a
`post-commit` hook fires only on the local machine and has no bus to publish to.

**Triggers and gardeners.** A *trigger bead* (`type=trigger`) wires an event to an action
— `notify`, `create_bead`, `apply_formula`, or `spawn_agent` — evaluated by a shared
engine; firing `spawn_agent` literally launches an agent pod (`bridge/triggers.go:1111`).
A *gardener* is a scheduled trigger pinned to an epic, with a daemon-side **preflight**
predicate evaluated at fire time: a bead-graph count query like "fire only while this
epic still has open children" (`scheduler/conditions/leaf_daemon.go:120`). The loop goes
dormant automatically when the epic closes. The cron engine is in-house
(`scheduler/cron.go`).

**Decision gates.** Human-in-the-loop checkpoints are queryable, blocking state. A
`session_gates` row (migration 002) holds `status` pending→satisfied; on an agent's
`Stop` hook the server checks `IsGateSatisfied` and, if not, **blocks the agent from
exiting** until a human resolves the decision via `gb yield` (`http_hooks.go:101`). That
is distributed, persistent human-in-the-loop control encoded as a DB row any controller
can query — Git has no shared mutable status row and no daemon to enforce one.

**Why Git can't.** Fleet orchestration demonstrably requires (1) a shared event stream
across machines, (2) typed queryable schedulable work items, (3) graph-condition wakes
("how many open descendants under epic X?"), (4) blocking shared state, and (5) a
scheduler. Git, even with hooks, offers only local, fire-and-forget, advisory callbacks
with no shared state and nothing typed to schedule against. The fleet runs *because* the
database is active.

> **Honesty notes (validated corrections).** (a) When the operator is unresolvable (e.g.
> auth-disabled smoke tests) the durable `events` *row* may be skipped while the bus
> publish still fires — so the events table is "a best-effort lower bound, never an
> authoritative replay log" (per its own code comment); live orchestration is
> unaffected. (b) Two distinct engines exist — the bridge engine (external sources) and
> the scheduler engine (gardeners) — sharing concepts but living in different packages;
> don't conflate them. (c) Some trigger kinds (`mail_received`, `webhook`,
> `slack_reaction`) are wired on the bridge side but deferred in the scheduler engine.

---

## 7. When Git *is* enough — and the honest boundary

This paper is not anti-Git. gardenio's *code* lives in Git, ships through Git-driven CI,
and is reviewed as Git diffs. Git is superb at what it is: a distributed, tamper-evident,
offline-capable store of file history with a battle-tested branching/merge model. For
versioning source, nothing here replaces it.

The thesis is narrower and sturdier: **the moment your problem is a live graph of typed
work objects with derived state, cross-item queries, validated identity, and event-driven
orchestration, you are no longer doing version control — you are running a database.** You
can simulate fragments of it on top of Git (issues-as-files, hooks-as-triggers), but each
fragment quietly rebuilds a worse database on the side: no transactions, no typed
queries, no triggers, no shared event bus, local-only automation. At that point the
honest move is to use the real thing.

And the corrections in this paper sharpen rather than soften the point. Beads does *not*
enforce acyclicity, does *not* yet sort by its live urgency score, uses MVCC rather than
row locks, and does lexical search with `ILIKE`. None of those gaps are expressible in Git
*at all* — Git has no graph to make acyclic, no derived score to maintain, no row-level
concurrency to tune, no work corpus to search. The weakest validated version of beads is
still doing a class of work that is simply outside Git's model.

Git is not all you need. For coordinating an autonomous engineering fleet, you need a
live object graph — and that graph is a database.

---

## Appendix A — Validation methodology

A five-agent team validated each section against the **live gardenio production database
(27,251 beads)** and the source tree, returning per-claim verdicts
(CONFIRMED / PARTIAL / NOT-SUPPORTED) with `file:line` citations and real command output.
Findings were folded back into the paper, including every "Honesty note." The one
NOT-SUPPORTED claim (write-time cycle detection, §2) was corrected rather than dropped.

Validation tracked under epic [[ka-0000003159]].

## Appendix B — Primary source map

| Claim | Source of truth |
|---|---|
| Typed dep graph | `kbeads/internal/store/postgres/migrations/001_initial.up.sql`; `kbeads/internal/model/dependency.go` |
| Ready-queue predicate | `kbeads/internal/store/postgres/queries.go:625`; `gardenio/controller/cmd/gb/ready.go` |
| Live urgency triggers | `kbeads/internal/store/postgres/migrations/019_urgency_score.up.sql` |
| Derived reference edges | `migrations/010_bead_references.up.sql`; `kbeads/internal/references/references.go` |
| Per-field audit log | `migrations/008_mutation_history.up.sql`; `kbeads/internal/server/mutation_history.go` |
| Semantic search | `migrations/005_bead_embeddings.up.sql`; `kbeads/internal/store/postgres/queries_embedding.go` |
| Structured query builder | `kbeads/internal/store/postgres/queries.go:382` |
| Typed/validated schemas | `kbeads/internal/model/type_config.go`, `validate_fields.go` |
| Concurrency / dibs | `kbeads/internal/store/postgres/postgres.go:574`, `dibs.go:37`, `queries.go:830` |
| Operator identity | `migrations/012,015,018,022,024,027`; `kbeads/internal/server/audit.go`, `beads.go:114` |
| Idempotency / soft-delete / GDPR | `migrations/029`, `009`, `030`; `kbeads/internal/store/postgres/operators.go` |
| Event bus | `kbeads/internal/server/server.go:198`; `kbeads/internal/events/events.go` |
| Triggers / gardeners / gates | `gardenio/controller/internal/bridge/triggers.go`, `scheduler/*`; `migrations/002`; `kbeads/internal/server/http_hooks.go:101` |
