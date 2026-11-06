# Inbox categories: revisit `intake` / `action` / `intermediate` post-pre-alpha

Research artifact for **ka-0000008046** (parent epic: **ka-0000002474** — inbox revamp).

> Operator framing (Matt Baker): *"the whole 'intake/actions/intermediate' idea
> might be a bit broken... what I'm expecting to see in an 'inbox' is any
> decisions I need to make, all open agents and what they're working on,
> open epics I'm assigned to, and a way to nominate work as inbox-worthy."*

This doc evaluates whether the existing four-category split is still the right
primary axis for `/inbox`, names what the operator wants there now, and
recommends a direction.

---

## 1. Current behavior

### 1.1 Where categories are defined

The category model lives in **kbeads**, not gardenio. Truth source:

- `kbeads/internal/server/category.go` — `DeriveCategory(bead, parent)` Go function.
- `kbeads/internal/store/postgres/migrations/020_beads_category_column.up.sql` —
  SQL trigger that mirrors `DeriveCategory` and stamps `beads.category` on
  INSERT/UPDATE of `beads` and INSERT/UPDATE/DELETE on `deps` (parent edge).

Four values, derived from a `(type, parent.type, parent.category, labels)`
tuple by a top-to-bottom rule table:

| Category       | Examples (steady state)                                                                          |
| -------------- | ------------------------------------------------------------------------------------------------ |
| `intake`       | top-level epics; beads carrying any `source:*` label (Slack/Jira/GitLab-sourced) without an intermediate parent |
| `action`       | every `decision` and `escalation`; `bug` carrying `flag:operator-blocking`                       |
| `intermediate` | in-flight `task` / `bug` under an intermediate ancestor; epic-of-epics children; fallback        |
| `config`       | `config`, `account`, `project`, `schedule`, `advice`, `formula`, `agent`, `fleet`                |

The rule table is canonical in two places (Go function + SQL trigger);
parity is enforced by `kbeads/internal/server/category_test.go`.

### 1.2 Where categories are read

Categories are an **inbox-only abstraction in practice**. A repo-wide grep
shows the only consumers in beads-viewer are inbox handlers:

- `gardenio/controller/cmd/beads-viewer/inbox.go`:
  - `handleInbox` / `handleInboxBead` / `handleInboxOrphans` — left-rail
    index query is `Categories: ["intake"], Statuses: [open, in_progress],
    OperatedByID: viewer` (lines 126, 225, 321).
  - `inboxCountForViewer` — `[Inbox · N]` chrome badge counts the same
    `category=intake` predicate (line 702).
  - `computeInboxOrphansFor` — the orphan pseudo-row right pane lists
    `category=action` open beads with no parent-child dep (line 581).
- `gardenio/controller/internal/beadsapi/client_query.go` — the
  `Categories []string` filter on `ListBeadsQuery`.
- Database hot-path index: `idx_beads_inbox_predicate ON beads
  (operated_by_id, category) WHERE category = 'intake'`.

`/active`, `/decisions`, `/agents`, and `/epics` do **not** filter on
`category`. Outside the inbox, agents already navigate by `type`, `status`,
`labels`, `operated_by_id`, and parent — not by category.

### 1.3 Where categories are written

Nowhere, by anyone, by hand. The SQL trigger in migration 020 stamps
`beads.category` on every INSERT and UPDATE. Bridges (`jira-bridge`,
`gitlab-bridge`, `slack-bridge`), the agent intake flow, and `kd create`
do not touch the category column directly — they set `type`, `labels`
(notably `source:jira` / `source:gitlab` / `source:slack`), and
`parent-child` deps, and the trigger derives the rest.

This is load-bearing: there is no human override and no API path to
manually re-categorize a bead. The model is "the rule table is reality,
and reality is the rule table."

### 1.4 What works about the current model

- **Inbox-count badge stays bounded.** `category=intake` per operator is
  ~24 beads in the median (per the `/operator-inbox` constellation
  design doc); the index on `(operated_by_id, category)` is small and fast.
- **Bridges feed the inbox automatically.** A new Slack-thread bead lands
  with `source:slack` → trigger derives `intake` → it appears in `/inbox`
  with no extra glue. Same for `jira-bridge` and `gitlab-bridge`. This is
  the "inbox as zero-config landing pad" property.
- **Decisions / escalations are reliably surfaced** somewhere
  (`category=action` is uniformly stamped) — even if not on `/inbox`
  specifically.
- **Top-level epics are the home page.** Operator confirmed (epic copy)
  that "open epics assigned to me" + click-through to constellation is
  *valued* and should not regress. Top-level epics → `intake`, so the
  current model already does this correctly.

### 1.5 What's broken about the current model

Synthesized from the operator's quote on the parent epic and the v1
`/inbox` shape:

1. **Decisions are missing from `/inbox`.** `decision` beads are
   `category=action`, not `intake`. `/inbox` filters strictly to
   `intake`, so pending decisions don't show up. The operator currently
   has to navigate to `/decisions` separately. This is the most concrete
   "the inbox is broken" complaint.
2. **Agents aren't on `/inbox` by default.** Recently fixed: ka-0000006349
   added an active+recent agents roster. But the *categorization* didn't
   help — agents are `category=config` (rule 8), so they were
   *excluded* from the inbox by the predicate. We had to bypass the
   category filter and run a separate `Types=[agent]` query. This
   suggests the category axis is not the right primary axis even for the
   inbox-roster surface.
3. **The bridge between "this bead is on my inbox" and operator
   intent is rigid.** A bead is intake iff its `(type, parent, labels)`
   tuple says so. There's no operator gesture for "promote this thing to
   my inbox" or "demote this thing — I'm not driving it." That gap is
   exactly what ka-0000007385 proposes to fill.
4. **`intermediate` is purely a residual category.** Rule 9 is the
   fallback; rule 6/7 fold task/bug into it. Nothing reads it for any
   surface today. It's a bookkeeping bucket with no UI and no behavior
   attached.
5. **Re-parenting cascades aren't fully handled.** Migration 020
   intentionally defers the recursive cascade for ancestor-flips
   (ka-0000000879 context). Practically rare, but it means `category`
   can drift on long-lived parent edits.

The problem isn't "the rule table is wrong." It's "the inbox needs to
union across categories *and* support operator-curated nominations,
which the category axis can't express."

---

## 2. What the operator wants in the inbox now

From the parent epic (ka-0000002474) themes plus the in-flight subtasks:

| Surface                                | Source bead                          | Maps to (current model)                                                                          |
| -------------------------------------- | ------------------------------------ | ------------------------------------------------------------------------------------------------ |
| Pending decisions, in-line             | ka-0000007648                        | `type=decision` (always `category=action`) scoped to viewer                                      |
| Active + recent agents and their tasks | ka-0000006349 *(closed)*             | `type=agent` (always `category=config`) scoped to viewer                                         |
| Open epics assigned to me              | ka-0000007839                        | top-level `type=epic` → `category=intake` (already in the predicate, but slot promotion needed)  |
| Nominate / promote / demote            | ka-0000007385                        | label-based, NOT in current category model                                                       |
| Create gardener (with mentor)           | ka-0000004891                        | spawn affordance, orthogonal to inbox query                                                      |
| Style drift fix                        | ka-0000003527 *(closed)*             | layout, orthogonal                                                                               |
| Forward bead to another operator       | ka-0000004114                        | scoping, related to nomination                                                                   |

Three of the five listed *content* surfaces don't fit `category=intake`.
Decisions and agents specifically need cross-category visibility on
`/inbox`. That alone closes the question of whether the current
single-category predicate stays as the inbox query.

---

## 3. Options

### 3a. Keep categories; inbox queries union across them as needed

Stop treating `category=intake` as the inbox predicate. Each inbox
surface (left-rail intake list, decisions panel, agents roster,
epics list) issues its own scoped query:

- left-rail: `category=intake AND status IN (open, in_progress)`
- decisions panel: `type=decision AND status=open`
- agents roster: `type=agent` (already implemented this way)
- epics list: `type=epic AND status IN (open, in_progress)` (subset of intake)

The category model survives unchanged; we stop pretending it is the
single axis. The inbox becomes a composition of small queries, each on
the right key for its domain.

**Pros**

- Zero schema change, zero migration. Today's data stays correct.
- Composes naturally with ka-0000006349's already-shipped pattern
  (the agent roster bypasses category and queries by `type=agent`).
- Categories stay useful as a *bookkeeping* layer (e.g. the inbox-count
  badge can keep using `category=intake` if we want a "primary intake
  count" semantic).

**Cons**

- Doesn't address the operator's "nominate this as inbox-worthy" ask —
  that need is real and orthogonal.
- The left-rail predicate stays rigid. Slack threads / Jira tickets that
  the operator no longer cares about can't be cheaply demoted off the
  inbox without closing them.

### 3b. Replace categories with a `flag:inbox-worthy` nomination model

Introduce a per-bead `flag:inbox-worthy` (and optionally
`flag:inbox-pinned`) label. Inbox query becomes
`labels @> [flag:inbox-worthy] AND status IN (open, in_progress)`. Drop
or deprecate `beads.category`.

This is the ka-0000007385 direction taken to its logical extreme. The
operator hand-curates the inbox; the rule table becomes (at most) a
seeding heuristic — when a Slack-sourced bead is created, the bridge
*also* stamps `flag:inbox-worthy` for the operator on whose behalf it
was created.

**Pros**

- Direct match to operator intent. The inbox is what the operator says
  it is.
- Demote/promote is a single label flip — no re-parenting, no
  re-typing.
- The rule table stops being load-bearing for inbox semantics; it can
  remain as a derived hint or be removed.

**Cons**

- **Loses the auto-landing property.** Today, a new Slack thread bead
  appears on the operator's inbox with zero gesture. If we drop
  category and require `flag:inbox-worthy`, every bridge has to learn
  to stamp the flag at create time, and the operator pays attention
  cost on the first interaction with each bridge.
- **Migration debt.** ~24 beads/operator × N operators × backfill
  policy. Not enormous in this env but non-trivial — and we explicitly
  put schema migrations under the "no eager backfills" rule
  (kbeads/CLAUDE.md).
- Would orphan the `beads.category` column and the
  `idx_beads_inbox_predicate` index until cleanup.
- Replaces the "system semantics" the SQL trigger gives us — there's
  currently exactly one consistent answer to "is this a decision?" and
  "is this an action item?" The rule table is the spec.
- Overlaps heavily with **ka-0000007385**, which proposes nomination
  *as a union* with the existing intake category, not as a replacement.

### 3c. Hybrid — keep categories for system semantics, add nomination on top

Keep `beads.category` and the trigger unchanged. Inbox queries:

- **left-rail (default):** `category=intake OR labels @> [flag:inbox-worthy]`
  scoped to the viewer.
- **decisions strip:** scoped query against `type=decision`.
- **agents roster:** scoped query against `type=agent`.
- **epics list:** scoped query against `type=epic` (subset of intake by
  rule 4, surfaced separately for visual prominence).
- **inbox-count badge:** UNION count, deduped on bead id.

Add the `flag:inbox-worthy` toggle from ka-0000007385 as an *additive*
mechanism. Add a sibling `flag:inbox-suppressed` (or
`flag:not-inbox-worthy`) that subtracts from the union for cases where
the rule table over-includes (e.g. an old Slack thread the operator no
longer cares about, but doesn't want to close).

```text
inbox(operator) := { b :
    b.status ∈ {open, in_progress}
  ∧ b.operated_by = operator
  ∧ ( b.category = 'intake'
      ∨ 'flag:inbox-worthy' ∈ b.labels )
  ∧ 'flag:inbox-suppressed' ∉ b.labels
}
```

**Pros**

- Preserves the auto-landing property (Slack/Jira/GitLab beads still
  arrive on the inbox with zero ceremony).
- Adds operator agency without taking away system semantics. The rule
  table still answers "is this a decision?" / "is this action-shaped?"
- ka-0000007385's design already proposes this exact union (see its
  description: *"the inbox query unions: `category=intake` beads
  (existing behavior), AND `flag:inbox-worthy` beads (new)"*). 3c is
  basically "do ka-0000007385, and also stop pretending other inbox
  surfaces (decisions, agents) live on the category axis."
- Keeps the index. The new union can be served either by issuing two
  small queries and merging in the handler (cheaper than refactoring
  the index), or by a future composite predicate.

**Cons**

- Two systems coexist; the operator must understand both
  ("auto-by-source" + "manual nomination"). Mitigated by the
  inbox-row chip (ka-0000007385 open question 1: render a "nominated"
  badge so the operator knows *why* it's there).
- `flag:inbox-suppressed` is new surface; we should only add it when
  there's a concrete demote case the union can't already express.

---

## 4. Recommendation

**Take option 3c (hybrid).** Specifically:

1. **Keep the `beads.category` column, the trigger, and the rule table
   exactly as-is.** They are the right answer to *system* questions
   ("is this a decision?", "is this auto-landed from a source?"). No
   migration. No drift between bridges. No regression to the
   auto-landing property.
2. **Stop treating `category=intake` as the sole inbox predicate.**
   `/inbox` becomes a composition of scoped queries:
   - left-rail intake list (the union from §3c)
   - decisions strip (`type=decision`)
   - agents roster (`type=agent`, already shipped)
   - epics-assigned-to-me strip (`type=epic`, top-level, scoped)
3. **Adopt `flag:inbox-worthy` per ka-0000007385** as an additive
   nomination layer. Render nominated rows with a chip so the operator
   sees why a non-intake bead is on the list.
4. **Defer `flag:inbox-suppressed`** until we have a concrete case
   where the rule-table over-includes. Suppression is easy to add later
   and easy to misuse early.
5. **Update `inboxCountForViewer`** to count the union, deduped. This
   is a small change in `inbox.go:693-712`.

The relationship to **ka-0000007385** is direct: this doc *endorses*
that task's design (union, additive flag) and frames it as the right
answer to the broader "intake/action/intermediate is broken" question.
ka-0000007385 can proceed under the current category model without
waiting on this research; nothing in this recommendation invalidates it.

The other in-flight subtasks (ka-0000007648 decisions, ka-0000007839
epics list, ka-0000004891 create-gardener) all fit naturally as
*independent inbox surfaces* under this framing — each queries on the
right key (`type=decision`, `type=epic`, etc.) rather than trying to
re-categorize beads to land them on the inbox.

---

## 5. Decisions that need operator sign-off

Before any refactor lands:

1. **Endorse 3c (hybrid) over 3a (compose-only) and 3b (replace).**
   The hybrid is the recommendation, but 3a is a defensible "do less
   now" alternative if the operator considers `flag:inbox-worthy`
   premature. 3b should be explicitly ruled out before any cleanup
   work touches `beads.category`.
2. **Confirm the inbox-count badge semantics.** Should `[Inbox · N]`
   count the union, or stay as the strict `category=intake` count for
   continuity? Recommendation: count the union (otherwise the badge
   under-counts when the operator's nominated work is what they're
   actually behind on).
3. **Confirm we should NOT add `flag:inbox-suppressed` in this round.**
   Easy to add later; easy to misuse early. Recommendation: defer.
4. **Confirm we should NOT drop or rename `beads.category` / the
   trigger.** They keep being useful as system semantics even after
   `/inbox` stops being the only consumer. Recommendation: leave alone.

---

## 6. Out of scope

- Migrating existing data (no schema change in the recommended path).
- Implementing the chosen model. Follow-up tasks live under
  ka-0000002474 — ka-0000007385 (nomination), ka-0000007648 (decisions
  on inbox), and the others already in flight.
- Re-evaluating `category=action` / `category=intermediate` /
  `category=config` semantics outside the inbox. The trigger is
  correct; nothing else queries those values today.

---

## 7. Pointers

- `gardenio/controller/cmd/beads-viewer/inbox.go` — every site where
  `Categories: []string{"intake"}` (or `"action"`) is used today.
- `gardenio/controller/internal/beadsapi/client_query.go:25` —
  `Categories []string` filter on `ListBeadsQuery`.
- `kbeads/internal/server/category.go` — `DeriveCategory` (Go spec).
- `kbeads/internal/store/postgres/migrations/020_beads_category_column.up.sql`
  — SQL trigger (materialization). Mirrors `DeriveCategory`.
- `gardenio/docs/research/escalation-beads-review-2026-05-05.md` §D7 —
  prior taxonomy work that landed the four-category model.
- `gardenio/docs/research/inbox-constellation-design.md` — `/inbox` v1
  design that fixed the predicate at `category='intake' AND
  operated_by_id = :viewer_operator_id`.
- Parent epic: ka-0000002474. Sibling tasks: ka-0000007385 (overlap
  called out above), ka-0000007648, ka-0000007839, ka-0000004891,
  ka-0000004114.
