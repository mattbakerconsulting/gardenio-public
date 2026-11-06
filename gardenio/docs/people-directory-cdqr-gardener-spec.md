# Users Directory (`/people`) Observing-Scoring Gardener — Spec

**Tracking bead:** ka-0000004156
**Operator:** Matt Baker (`oidc:matthew.baker@acme.example`)
**Origin:** Slack — *"the Users page is still a total drag. lots of dupe users, no
top level stats displayed... we need a proper observing scoring gardener like we did
for inbox and stuff to get this up to spec."*

---

## 1. What page are we actually talking about?

The "Users" page is the **operator directory** served at **`GET /people`**
(handler `handlePeople`, `gardenio/controller/cmd/beads-viewer/people.go`, template
`templates/people.html`). Its `<h1>` literally reads **"Users (N)"**.

> ⚠️ This is **not** `/u/{slug}` (the per-user *homepage*). Two gardeners already
> cover `/u/{slug}`:
> - **Users-Page Polish Gardener** `ka-0000000505` → epic `ka-0000000467`
> - **Users-Page CDQR Audit** formula `ka-0000000967` → epic `ka-0000007423`
>
> Both audit the *homepage*. **The `/people` directory is genuinely uncovered.**
> That is the gap this gardener fills, and the naming (`people-cdqr`, not
> `users-cdqr`) is deliberately chosen so we don't collide with the homepage work.

## 2. The two complaints, grounded in code

### Complaint A — "lots of dupe users"
`handlePeople` calls `s.daemon.ListOperators(ctx, peopleListLimit)` and renders one
row per returned operator, with **no alias collapsing**. The same human shows up as
multiple rows because each identity alias (OIDC `oidc:matthew.baker@…`, Slack
`slack:U…`, agent/bot subjects) can resolve to a distinct operator row.

The platform *already has* the merge primitive —
`internal/beadsapi/client_admin_operator_aliases_link.go` exposes
`POST /v1/admin/operators/link-aliases` (with `MergedFromIDs`) — but the directory
neither (a) renders the *canonical* identity nor (b) surfaces *unmerged dupe
clusters* so an operator can fix them. So "dupes" is two sub-problems:
1. **Display dedup** — collapse alias rows into one canonical operator row.
2. **Dupe detection** — flag likely-same humans (same email localpart, same display
   name, overlapping Slack/OIDC) that *haven't* been linked yet, with a one-click
   merge affordance.

### Complaint B — "no top level stats displayed"
`people.html` renders **per-row** chips (open-beads, last-active) but there is **no
page-level aggregate header**. There is no "X humans · Y active now · Z open beads
across the fleet · N unmerged dupe clusters" summary band. The page is a flat list
with no top-of-page orientation.

## 3. The gardener shape (model: `gardener-cdqr`)

We mirror the freshest, best-working precedent: the **Gardener-Page CDQR** gardener
(formula `ka-0000005307`, schedule `ka-0000006085`, epic `ka-0000001553`), which
Matt already shaped by adding an **Operability** dimension. Same six-step pour:
`boot-and-warmup → capture → measure → judge → file-issues → report`, fired daily by
a schedule trigger so regressions surface as bug beads automatically.

### Scoring dimensions — **CDQR + D̂** (Dedup)

Standard four CDQR dimensions, **plus one page-specific dimension** that directly
encodes Matt's complaint so the score can't go green while dupes persist:

| Dim | Name | What it scores on `/people` |
|---|---|---|
| **C** | Correctness | Counts/links accurate; canonical identity shown; no template leaks/5xx; per-row open-bead + last-active match the daemon. |
| **D** | Density | Tufte signal-per-pixel: `near_white_ratio`, `data_fields`, `dom_element_count`. A flat list of names with acres of white scores low. |
| **Q** | Quality | Alignment, hierarchy, chip styling, empty-states, narrow-viewport layout. |
| **R** | Relevance | Is the directory ordered/segmented usefully (active-first, humans vs agents/bots), and does it answer "who is on this fleet and who's busy?" |
| **D̂** | **Dedup & Top-Stats** | **The headline dimension.** (1) Are alias rows collapsed to canonical humans? (2) Are likely-dupe clusters detected + surfaced with a merge affordance? (3) Is there a page-level aggregate stats header (total humans, active-now, fleet-wide open beads, unmerged-dupe-cluster count)? Score ≤ 3 while raw dupes render or no stats header exists — this drives the bug beads that fix Matt's exact complaints. |

`overall.score` = mean of the five. Each dimension scoring **≤ 6** files one bug bead
parented to the epic with concrete recommendations + before-screenshot evidence.

### Objective metrics the `measure` step adds (beyond density)
- `alias_dupe_clusters` — operators grouped by (lowercased email localpart ∪ display
  name); any cluster of size > 1 that isn't already alias-linked = an unmerged dupe.
- `rows_rendered` vs `distinct_canonical_humans` — the dedup gap, numerically.
- `has_stats_header` (bool) + which aggregate fields are present.
- standard correctness counters (`template_leaks`, `error_markers`, `broken_links`).

## 4. Beads to create (the "turn it on" wiring)

1. **Epic** `feat: Users directory (/people) up to spec — dedup + top-stats`
   (`type=epic`, labels `area:beads-viewer`, `kind:people-cdqr`). The gardener's work
   queue + parent for all filed bugs.
2. **Formula** `People-Directory CDQR Audit` (`type=formula`, `kind:people-cdqr`),
   six steps as above, vars `TARGET_URL`, `ITERATION`, `PARENT_EPIC`. Authored from
   `gardenio/docs/people-directory-cdqr-formula.json` (committed alongside this spec).
3. **Schedule trigger** `People-Directory CDQR Gardener` (`type=trigger`,
   `trigger:schedule`), `apply_formula` → the formula above, daily cron
   (e.g. `37 7 * * *` UTC — staggered off the gardener-cdqr `47 7`), `max_concurrent=1`,
   `cooldown_minutes=30`, `formula_vars={TARGET_URL: http://localhost:8095,
   ITERATION: {{fire_time_iso}}, PARENT_EPIC: <epic-id>}`. **Created disabled**;
   flipped on only after operator approval (§6).

## 5. Why a gardener and not just "go fix it"

The complaint is recurring ("despite our best efforts… *still* a drag") — point
fixes regress. A scheduled observing-scoring gardener keeps a daily numeric pulse on
the directory, so the dedup + top-stats work both gets *started* (D̂ scores low now,
files bugs) and *stays* fixed (D̂ regressions re-file bugs). Same reason inbox and
the gardener page each got one.

## 6. Enablement gate

Turning a scheduled auto-spawning gardener **on** is an operator decision. After this
spec + formula JSON land, the thread presents a `gb decision` with: (a) enable the
daily schedule as specced, (b) run one manual baseline pour first then decide, or
(c) adjust dimensions/cadence. The schedule trigger ships **disabled** until that
decision resolves.
