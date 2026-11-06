# Per-user homepage / inbox v2 — design synthesis

> **Status:** Design proposal, pending operator approval.
> **Parent research epic:** [`ka-0000003855`](#) — *Research: per-user homepage / inbox v2*.
> **Supersedes:** [`inbox-constellation-design.md`](inbox-constellation-design.md)
> (locked v1 inbox draft; this doc replaces that route concept with `/u/:slug`).
>
> This document synthesizes seven research rounds (R1–R7) into a single
> implementable design. Each section links back to the corresponding
> R-doc and bundle for full rationale. Implementation does not begin
> until the operator green-lights the open questions in §10.

---

## 0. TL;DR

A **per-user homepage** at `/u/:slug` becomes the canonical "what is
this human doing on the platform" surface, replacing the legacy
`/inbox` and absorbing four scattered views (`/active`, `/decisions`,
`/agents`, `/news`).

- **Identity:** every non-sentinel `operators` row gets a homepage.
  Slug = email local-part, with `/u/op-<id>` fallback. Agents do NOT
  get their own page; they roll up under their spawner via
  `operated_by_id` (R7).
- **Routing:** `/u/:slug` is the primary route. `/me` is a server
  redirect to the viewer's own slug. `/inbox` keeps working through
  M3 then 308s to `/u/me?view=dashboard`.
- **Default sections:** 8 default sections (profile card, pinned +
  status, active agents, open decisions, open epics, recently touched
  7d, recent MRs 14d, recent agent runs).
- **Customization:** profile-level v1 (section toggle/order, pins,
  identity visibility, 140-char status). Per-bead privacy deferred to v2.
- **Cross-user browsing:** anyone authenticated can view anyone's
  homepage (Mode A relaxed). 22 actor render surfaces collapse into one
  `personChip` component.
- **Performance:** reuse `ListBeadsFiltered` with parallel `errgroup`
  fan-out. p95 ~80 ms LAN / ~180 ms WAN on the 22-bead fixture.
- **Migration:** three phases — M1 plumb `/u/:slug`, M2 absorb sections,
  M3 fold `/inbox`. Three operational sprints.

### Decision matrix

| Question | Recommended | Alternates |
|---|---|---|
| Route shape | `/u/:slug` primary, `/me` redirect | `/me` only; `/people/:id`; subdomain |
| Slug source | Email local-part + `/u/op-<id>` fallback | Display name; opaque ID only |
| Agent attribution | Roll up via `operated_by_id` to spawner | Agents get own `/u/:slug` |
| Visibility default | `authenticated` (anyone logged in) | `public`; `self` |
| Customization v1 | Profile-level only | Per-bead privacy; tagged feeds |
| Migration of `/inbox` | Three-phase, ends in 308 | Hard cutover; keep both |
| New SQL endpoint | None v1 — reuse `ListBeadsFiltered` | Dedicated `/u/:slug` RPC |

---

## 1. Identity & scope of the homepage

**Source:** R1 — Identity & scope ([[ka-0000004506]]) ·
Bundle [`/bundle/ka-0000000363`](#)

### 1.1 Who gets a page

- **Every non-sentinel `operators` row** = a person who can host a
  homepage. This includes pre-OIDC humans stubbed from user beads.
- **Sentinels** (`system`, `fleet`, bridge accounts) do NOT get pages.
  Their writes render as **bylines** linking to a synthetic
  `/fleet/<bridge>` index, never as authoritative authors.
- **Agents** (operator rows of `kind=agent`) do NOT get their own
  `/u/:slug`. Agent activity rolls up under the **spawner human** via
  `operated_by_id` (see §7).

### 1.2 Slug source

Canonical source: email local-part of the operator's primary identity.

```
matt.baker@acme.example  →  /u/matt.baker
no email yet             →  /u/op-<id>
```

- A new table `operator_slug_aliases(slug, operator_id, retired_at)`
  preserves old slugs on rename so existing links keep resolving.
- Reserved-word list blocks slugs that collide with platform routes
  (`me`, `new`, `search`, `admin`, …) and forces the `op-<id>` form.
- Stub-from-user-bead: humans who exist as user beads but have never
  authenticated get a deterministic stub `operators` row at
  user-bead-write time. Slug = best-effort from the user bead's
  `email` or `display_name`; otherwise `op-<id>`.

### 1.3 Three-class visibility

| Class | Who can view the page |
|---|---|
| `public` | Anyone (unauthenticated reads OK) |
| `authenticated` | Any logged-in operator (default) |
| `self` | Only the operator themselves |

Defaults: human operators → `authenticated`; stub operators → `self`
until claimed; agent rows have no page so the class is moot.

---

## 2. Routing

**Source:** R2 — Routing ([[ka-0000005283]]) ·
Bundle [`/bundle/ka-0000005710`](#)

### 2.1 URL shape

- `/u/:slug` — primary, stable, canonical
- `/me` — server-side 307 redirect to `/u/<viewer-slug>` (login required)
- `/u/op-<id>` — fallback for slug-less operators
- `/people` — directory (alphabetical, recent-activity sort)
- `/fleet/<bridge>` — sentinel byline index

### 2.2 Schema additions

```sql
ALTER TABLE operators ADD COLUMN slug TEXT UNIQUE;
CREATE TABLE operator_slug_aliases (
    slug         TEXT PRIMARY KEY,
    operator_id  BIGINT NOT NULL REFERENCES operators(id),
    retired_at   TIMESTAMPTZ
);
CREATE INDEX ON operator_slug_aliases (operator_id);
```

Slugify algorithm: lowercase email local-part → strip non-`[a-z0-9_.-]`
→ collapse runs of `.` and `-` → trim leading/trailing punctuation →
fall back to `op-<id>` if empty or reserved.

### 2.3 Mention chip

The unified `personChip` (see §6) emits two links:

- **Primary** (chip click / `g h` shortcut): `/u/:slug`
- **Secondary** (overflow menu): `/search?operated_by_id=<id>` for
  raw activity feed.

Team chips, `on_call`, and `raise_only` chips retire — they are folded
into person chips or live under `/fleet/<bridge>` indexes.

### 2.4 `/inbox` migration

| Phase | Behavior |
|---|---|
| M1 | `/u/:slug` lands. `/inbox` unchanged. `/me` redirects work. |
| M2 | `/u/:slug` absorbs all `/inbox` sections. `/inbox` shows a banner: "moved to your homepage." |
| M3 | `/inbox` returns `308` → `/u/me?view=dashboard`. Old constellation view `/u/me?view=constellation` opt-in. |

---

## 3. Default sections

**Source:** R3 — Default sections ([[ka-0000001179]]) ·
Bundle [`/bundle/ka-0000001194`](#)

Eight default sections, in render order:

| # | Section | Query shape | Window |
|---|---|---|---|
| 1 | **Profile card** | `operators(id=…)` + status_message | n/a |
| 2 | **Pinned + status** | `bead_id IN ProfilePref.PinnedBeadIDs` | n/a |
| 3 | **Active agents** | `type=agent AND operated_by_id=? AND status=in_progress` | live |
| 4 | **Open decisions** | `type=decision AND operated_by_id=? AND status=open` | live |
| 5 | **Open epics** | `type=epic AND operated_by_id=? AND status IN (open, in_progress)` | live |
| 6 | **Recently touched** | `operated_by_id=? AND updated_at > now()-7d` | 7d |
| 7 | **Recent MRs** | `mention.kind=gitlab_mr AND operated_by_id=? AND updated_at > now()-14d` | 14d |
| 8 | **Recent agent runs** | `type=agent AND operated_by_id=? AND closed_at > now()-7d` | 7d |

**Section 9 (recent comments)** is deferred to v2 — needs a comment-index
schema first.

Window constants live in one file (`profileRecentTouchedWindow`,
`profileRecentMRWindow`, `profileRecentAgentWindow`) so they're tunable
without editing query call sites.

---

## 4. Query model and performance

**Source:** R4 — Query model & perf ([[ka-0000000199]]) ·
Bundle [`/bundle/ka-0000002018`](#)

### 4.1 No new endpoint v1

Reuse the existing `ListBeadsFiltered` daemon RPC. Resolve the slug to
an `operator_id` **once** at request entry, then fan out per-section
queries in parallel via `errgroup`:

```go
g, ctx := errgroup.WithContext(ctx)
sections := struct{
    activeAgents, openDecisions, openEpics,
    recentTouched, recentMRs, recentAgentRuns []*beadspb.Bead
}{}
g.Go(func() error { /* …  ListBeadsFiltered call … */ })
// 6 parallel fan-outs total
```

### 4.2 OperatedByAliases predicate fix

Today the predicate generates per-alias `OR` clauses. Replace with a
single `(kind, subject) IN VALUES (…)` join — pre-collapses aliases
across platforms (slack, gitlab, email, agent-id) and removes the
N-statements blow-up.

### 4.3 Performance budget

| Surface | p50 | p95 | Round trips |
|---|---|---|---|
| 22-bead fixture, LAN | ~30 ms | ~80 ms | 4 |
| 1k-bead synthetic, WAN | ~90 ms | ~180 ms | 4 |

Batch the 6 parallel section queries into ≤4 round trips by reusing a
single `Filter` connection. **No new index** in v1; existing
`operated_by_id` index is sufficient. Re-evaluate once we cross
~100 daily-active operators.

---

## 5. Customization

**Source:** R5 — Customization ([[ka-0000002704]]) ·
Bundle [`/bundle/ka-0000000792`](#)

### 5.1 Two layers, only v1 lands

**Profile-level (v1):**

```go
type ProfilePref struct {
    Sections            []string  // section IDs to render
    SectionOrder        []string  // explicit order; missing IDs appended
    PinnedBeadIDs       []string  // pins for §2
    IdentityVisibility  string    // public | authenticated | self
    StatusMessage       string    // 140 chars max
}
```

Extends the existing user-bead `Preferences` struct (no new table; the
JSON column already supports nested fields).

**Bead-level (v2, deferred):** per-bead privacy controls
(e.g. "hide this decision from my profile") deferred to v2 to preserve
the **viewer-sees-every-bead** invariant in v1 — your profile may hide a
bead from *your* homepage but anyone can still find it through search
or its canonical URL.

### 5.2 New identity field

`status_message` — 140-char free text rendered in the profile card.
Empty by default. Validated server-side, no markdown rendering, raw text
with URL autolinking only.

### 5.3 What's intentionally NOT in v1

- Per-bead privacy / hide-from-profile
- Tagged feed groups ("docs only", "infra only")
- Avatar upload (profile photo) — reuse existing operator avatar
- Custom CSS / themes

---

## 6. Cross-user browsing

**Source:** R6 — Cross-user browsing ([[ka-0000001792]]) ·
Bundle [`/bundle/ka-0000000296`](#)

### 6.1 Unified `personChip`

The viewer renders **22 actor surfaces** today across 8 component
groups. All collapse into a single chip:

```go
type personChip struct {
    Slug         string  // /u/:slug target
    Display      string  // canonical render
    RawID        int64   // operator_id
    Kind         string  // human | agent | sentinel
    Email        string
    AgentBeadID  string  // when Kind=agent — links to spawner via operated_by_id
    Alive        bool    // for agents
    IsViewerSelf bool
    FilterHref   string  // /search?operated_by_id=…
}
```

### 6.2 Navigation surfaces

- `/people` — directory, alphabetical w/ recent-activity secondary sort
- Hover card on every chip → `/u/{slug}/card` partial (HTMX)
- `cmd-k` autosuggest seeded with recent operators
- Keyboard: `g h` → home (own page), `g p` → people directory, `u`
  on a focused chip → that person's page

### 6.3 Label changes

- "My beads" → **"Me"**
- "Inbox" → **"Home"** (during M3+; alias kept indefinitely)

### 6.4 Rollout (4 milestones, runs in parallel with M1–M3)

1. Ship `personChip` and replace all 22 surfaces (no behavior change)
2. Land `/u/:slug` + flip default home link
3. Ship `/people` directory + hover card
4. Mention chip emits `/u/:slug`; keyboard shortcuts; label renames

---

## 7. Agent / bot attribution

**Source:** R7 — Attribution chains ([[ka-0000006005]]) ·
Bundle [`/bundle/ka-0000000363`](#) (filed under R1 bundle)

### 7.1 `operated_by_id` as canonical lens

The homepage uses `operated_by_id` (NOT `created_by_operator_id`) as the
canonical "whose work is this" lens. The spawn-chain walk already runs
at write time in `audit.go:719` — agents inherit the human spawner's
`operated_by_id` recursively. No new computation needed at read time.

### 7.2 Agents render on the spawner's page

- Active agents → §3 / Section 3 (in_progress)
- Closed agent runs → §3 / Section 8 (last 7d)
- Schedules owned by the spawner → new optional section
  **"Their automations"** (type=schedule, owned)

### 7.3 Originator lens

`created_by_operator_id` provides a secondary "who first opened this"
lens. Surfaced only on the bead detail page, not the homepage.

### 7.4 Spawn chain breadcrumb

On an agent's bead detail page, render the spawn chain as a breadcrumb:

```
Matt → gardener-foo-bar (agent) → crew-baz-quux (agent) → this bead
```

Each step is a personChip. Final hop = the bead itself.

### 7.5 Backfill

A one-shot backfill walks all legacy `type=schedule` and `type=agent`
beads with NULL `operated_by_id` and resolves via the spawn chain or
`created_by_operator_id` fallback. Backfill is idempotent — runnable as
a `kd formula` against any environment.

### 7.6 Sentinel writes

Sentinel writes (`system`, `fleet`) render as **bylines** with a
`/fleet/<bridge>` link, never as the owning operator. They do not
populate `operated_by_id` (the spawn-walk skips them).

---

## 8. Migration plan

### M1 — Plumb `/u/:slug` (sprint 1)

- Schema migration: `operators.slug`, `operator_slug_aliases`,
  reserved-word list
- Slugify all existing operators in-place
- New route `/u/:slug` rendering profile card + pinned + active agents
  + open decisions (subset of §3 sections)
- `/me` redirect
- Ship `personChip` and replace all 22 actor surfaces (no behavior
  change, parallel R6 milestone 1)
- Backfill `operated_by_id` for legacy schedules/agents (R7 §7.5)

### M2 — Absorb sections (sprint 2)

- Full 8-section render (R3)
- ProfilePref customization layer (R5 §5.1)
- `status_message` field + UI
- `/people` directory + hover card (R6 milestones 2–3)
- Banner on `/inbox`: "moved to your homepage"
- Mention chips emit `/u/:slug` (R6 milestone 4)
- Keyboard shortcuts (R6 §6.2)

### M3 — Fold `/inbox` (sprint 3)

- `/inbox` returns 308 to `/u/me?view=dashboard`
- Legacy constellation view available at `/u/me?view=constellation`
- Retire `team` / `on_call` / `raise_only` chips (R2 §2.3)
- Label renames complete ("My beads" → "Me", "Inbox" → "Home")
- Sentinel `/fleet/<bridge>` indexes ship
- "Their automations" section enabled by default for operators with
  >0 active schedules

---

## 9. Telemetry & success metrics

- **`/u/:slug` p95 latency** ≤ 200 ms WAN, ≤ 100 ms LAN
- **Bounce rate** on `/inbox` post-M3 should track within 10 % of
  pre-cutover `/inbox` baseline
- **Cross-user views** (non-self `/u/:slug` hits) — telemetry baseline
  established at M2 landing; growth = signal Mode A relaxation was right
- **Customization adoption** — % of operators who set any ProfilePref
  field at 30 days post-M2
- **Mention-chip click-through** — primary `/u/:slug` link vs. secondary
  `/search?operated_by_id=…` — informs whether the chip default should
  ever flip

---

## 10. Open questions for operator approval

These are the questions the implementation epic should NOT start until
the operator answers. Each is a decision-checkpoint candidate:

1. **Slug source.** Email local-part is the proposed default. Accept,
   or prefer display-name slugification / opaque ID only?
2. **Default visibility class.** `authenticated` is proposed.
   Acceptable or should the default be `self` with explicit opt-in to
   broader visibility?
3. **Agent attribution.** Confirm agents do NOT get their own
   `/u:slug` and roll up via `operated_by_id`. Any pushback from
   gardener-loop / fleet operator workflows?
4. **Section list.** The 8 default sections in §3 — any add/remove
   before M2 lands? (Notable: "recent comments" is deferred to v2.)
5. **Migration cadence.** Three sprints (M1/M2/M3). Acceptable or do
   we want a longer co-existence window before M3 cutover?
6. **Customization scope v1.** Per-bead privacy deferred to v2. Confirm
   acceptable.
7. **`/inbox` final disposition.** 308 redirect to `/u/me?view=dashboard`
   in M3, or keep `/inbox` indefinitely as a saved-view alias?

---

## 11. Cross-references

### R-doc bundles

- R1 Identity & scope — [`/bundle/ka-0000000363`](#) (doc: `ka-0000004506`)
- R2 Routing — [`/bundle/ka-0000005710`](#) (doc: `ka-0000005283`)
- R3 Default sections — [`/bundle/ka-0000001194`](#) (doc: `ka-0000001179`)
- R4 Query model — [`/bundle/ka-0000002018`](#) (doc: `ka-0000000199`)
- R5 Customization — [`/bundle/ka-0000000792`](#) (doc: `ka-0000002704`)
- R6 Cross-user browsing — [`/bundle/ka-0000000296`](#) (doc: `ka-0000001792`)
- R7 Attribution chains — filed under R1 bundle [`/bundle/ka-0000000363`](#)
  (doc: `ka-0000006005`)

### Adjacent context

- Locked v1 design (superseded): [`inbox-constellation-design.md`](inbox-constellation-design.md)
- Spawn-chain attribution: [`attribution-chain-spawn-paths.md`](attribution-chain-spawn-paths.md)
- Operated-by transitivity series: `operated-by-transitivity-r1…r5.md`
- Viewer default operated-by filter: `viewer-default-operated-by-synthesis.md`

### Code touch-points (rough estimate, M1)

- `gardenio/controller/cmd/beads-viewer/` — new route handler, partials
- `gardenio/controller/internal/operators/` — slug generation, alias table
- `kbeads/internal/server/` — `ListBeadsFiltered` predicate fix
  (`OperatedByAliases` → `IN VALUES`)
- `kbeads/migrations/` — `operators.slug`, `operator_slug_aliases`
- Component library — `personChip` + replace 22 actor surfaces

---

*Synthesis prepared by `gardenio-r8-synthesize-design-pad`
([`ka-0000005961`](#)) from R1–R7 research rounds. Awaiting operator
decision checkpoint before implementation epic is filed.*
