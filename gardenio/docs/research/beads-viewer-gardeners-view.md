# beads-viewer "gardeners view" — research note

**Bead:** ka-0000004722 (research). Source mention: ka-0000001960 (Matt Baker, 2026-04-27, Slack).
**Scope:** `gardenio/controller/cmd/beads-viewer/` (Go + htmx server-side). NOT beads3d, NOT the iOS app.
**Status:** research/design only — no Go, template, or schema change in this MR. Implementation lands in follow-up tasks gated by the decision drafted in Appendix A.

---

## 1. TL;DR

- The mention asks two coupled questions: (a) what should a "gardeners view" in beads-viewer show? and (b) what reporting patterns — supplementary beads, association edges, structured fields — could gardeners adopt to make that view richer?
- Today the viewer can already answer most "what is in flight right now?" questions (`/active`, `/agents`, `/decisions`). What it cannot answer is **gardener-shaped** questions: "for the beads-viewer gardener, across the last 24h of cycles — what merged, what's stuck, which crew are alive, which decision has been pending longest, where are the failed wrapups?" That gap is the gardeners view.
- The associated-bead substrate already partly exists: scheduled-gardener bead per cycle (`schedule_id` field links to a `schedule` bead), child task beads scoped under gardener-owned epics, MR beads for merged work, decision beads for blockers, agent beads for the spawned gardener pod, and `wrapup` JSON in the agent stop event. None of these are surfaced as a *unit* anywhere — they are scattered across `/active`, `/agents`, `/schedules`, `/decisions`, and bead detail pages.
- Three fork options to take to the operator, sketched in §6:
  - **(A)** New `/gardeners` top-level route with a tab strip per gardener — a gardener-roster index plus a per-gardener page composing roster + cycles + crew + blockers + recent merges.
  - **(B)** Gardener lens layered on `/agents` — an `?as=gardener` query that re-pivots the existing agents list into a gardener-grouped tree (gardener → its in-flight crew → recent cycles), no new top-level route.
  - **(C)** Enriched epic-detail view — the gardener's *driver epic* page becomes the gardeners view: epic header + child beads (existing) + a gardener panel showing scheduled-gardener cycle history, crew, decisions, recent MRs.
- **Recommendation: (A)**, with the per-gardener page reusing the components built for (C). (B) is the cheapest but boxes the operator into "agent-shaped" framing; the gardener identity is *the schedule + the driver epic*, not the running pod.
- Reporting changes the operator should expect to ratify: (i) make `wrapup` a structured set of fields on the gardener agent bead (already partial — see §4); (ii) add an explicit `gardener_of:<schedule-id>` association tag from each scheduled-gardener agent bead to its schedule; (iii) make `driver_epic` a first-class field on the schedule bead so the viewer can pivot off it without parsing prompts; (iv) require gardeners to drop a `cycle_summary` comment (or supplementary bead, fork-dependent) at `gb stop`.
- The drafted `gb decision create` invocation is in **Appendix A**.

---

## 2. What is a "gardeners view"?

A "gardener" today is a scheduled, role-locked agent that owns a slice of work end-to-end across many short cycles. Examples on the schedule list right now (`kd list --type=schedule`, sample):

| Gardener | Schedule | Driver |
|---|---|---|
| Beads-Viewer Gardener | ka-0000002931 (every 20m) | ka-0000002538 |
| Gardenio Feature Delivery Gardener | ka-0000004432 | open `project:gardenio` ready beads |
| Gardenio gardener-release-owner | ka-0000006058 (hourly) | golden release path |
| Gardenio gardener-delivery | ka-0000000377 | READY → MR → release |
| gardener-embeddings | ka-0000001902 | embedding completeness |
| Gardener-cycle-watchdog | ka-0000001612 | detects FAILED gardener cycles |
| Code Coverage Gardener | ka-0000001052 | MR 442 (DEVOPS-312) |
| Gardenio Log Monitor Gardener | ka-0000003531 | log triage |
| Mezmo Alert Custodian | ka-0000005230 | alert hygiene |
| Monorepo Gardener-Observability | ka-0000007761 | Grafana dashboards |
| Gardenio CI/CD Gardener | ka-0000001155 | open CI/CD work |
| RWX CI/CD Gardener | ka-0000001232 | monorepo RWX port |
| Auditor | ka-0000001345 | daily session log analysis |
| Bug Tracker Update | ka-0000001017 | Confluence refresh |
| Monorepo Daily Standup | ka-0000003144 | daily summary |

A gardener is identified by **the schedule bead** (the durable identity; it persists across cycles and pods) plus, in most cases, **a driver epic** (single source of truth for the work the gardener advances). Each cycle materializes as **one scheduled-gardener bead** + **one agent pod** + **a tail of child task / MR / decision / wrapup artifacts**. That triplet is what the operator currently has to reconstruct by hand from three or four pages.

A gardeners view should answer, at a glance, four operator questions per gardener:

1. **Liveness** — is the gardener firing on schedule, completing cycles, or stuck/failing? (cron expression, last-fire, last-success, FAILED-cycle count, watchdog signal)
2. **Throughput** — what merged or progressed in the last N cycles? (MR beads closed, child tasks closed, decisions resolved, releases cut)
3. **Crew** — which crew agents has it spawned and what are they doing right now? (pod phase, claimed task, idle/active, mail backlog)
4. **Blockers** — what is waiting on a human? (open decisions raised by the gardener or its crew, FAILED cycles needing attention, stale-claim beads)

Plus one cross-gardener view: **roster** — all gardeners, sorted by liveness/blockers, so the operator can scan top-down for who needs them right now.

Today's `/active`, `/agents`, `/decisions`, `/schedules` cover slices of (1)–(4) but never *for one gardener* and never *together*. That is the unmet need.

---

## 3. Existing surfaces to build on

The viewer already does most of the data-fetching primitives a gardeners view needs. Mapping each surface to the gardener workflow it partially serves:

### 3.1 `agents.go` (`/agents`)

- Lists active + recent agent beads, grouped by `project:` label. Project chip strip filters one project. Pulls open + in_progress (active) and a tail of closed (recent).
- View model is `agentRowView{ID, Name, Role, Project, AgentState, PodPhase, TaskID, TaskTitle, …}`. All from `agent` bead fields.
- **Gardener workflow served:** crew liveness — operator can see "what's running for project X". Misses gardener identity; gardener pods and crew pods sit side-by-side with no parent-child indicator beyond the gardener's `schedule_id` field.
- **Reusable:** `agentRowFromBead` view-model + `agents.html` row template work as-is for the "Crew" panel of a per-gardener page.

### 3.2 `active.go` (`/active`)

- All `in_progress` beads, grouped by `project:` label. Carries a top-of-page summary bar (active / agents / decisions counts) and a decision banner when any decision is open.
- **Gardener workflow served:** sense of "load per project". Cannot scope to one gardener — there is no gardener → bead association on non-agent beads today.
- **Reusable:** the row partial (`active_row.html` via `beadActionsView`) and grouping logic are the building blocks for an "In flight under this gardener" panel. The pivot field becomes `gardener_of:<schedule>` instead of `project:`.

### 3.3 `prime.go` (`/agents/{id}/prime`)

- Shells `gb prime --for <agent-id>` and renders the markdown identity / advice / news bundle for an agent bead.
- **Gardener workflow served:** "what does this gardener see when it wakes up?" — gold for verifying advice + role context. Already linkable from any agent bead.
- **Reusable:** unchanged. The gardeners view links to it from the gardener pod's row.

### 3.4 `schedules.go` (`/schedules`)

- Lists `schedule` beads with cron, enabled, project, role, prompt, last-run, runs-spawned/completed, last-agent-id, next-fire (computed via robfig/cron).
- **Gardener workflow served:** liveness — most of what (1) needs is already here. Cron + last-fire + next-fire is enough to answer "is the gardener firing on schedule?".
- **Reusable:** the `scheduleView` view-model in `type_views.go` is the natural anchor for a per-gardener page header. A gardener-detail page is a `schedule` page that hydrates additional sections (cycles, crew, MRs, blockers).
- **Gap:** schedules don't carry a `driver_epic` field today; the prompt embeds it as text. See §4 for the proposed reporting change.

### 3.5 `costs.go` (`/costs`)

- Aggregates rated cost rows. Project filter + per-agent breakdown.
- **Gardener workflow served:** "which gardener is burning the budget?" — adjacent. Could be filtered per-gardener by joining on the gardener's spawned agents.
- **Reusable:** the existing aggregation function would need a `WHERE agent_id IN (gardener's spawned crew)` filter — see §4 association tags.

### 3.6 `ratings.go` (`/ratings`)

- Per-rated-bead 1–5 + multi-axis rollup. Already wired through to comment write path.
- **Gardener workflow served:** retrospective — "gardener X's last 50 MRs averaged 3.2". Useful for the per-gardener page's "trailing N cycles" footer panel; not gating.
- **Reusable:** rating widget + rollup queries can be invoked filtered on `bead.created_by_agent.gardener_of`.

### 3.7 `type_views.go` (per-type detail)

- Per-type view models for `decision`, `schedule`, `formula`, `agent`, `advice`, `mr`, `rating`, `generic`. Rendered inline on bead-show via the `type_<name>.html` partials.
- **Gardener workflow served:** every type the gardeners view shows already has a partial. No new types are required if the view composes existing partials.
- **Reusable:** the per-gardener page is mostly a composition of `type_schedule.html` + `type_agent.html` + `active_row.html` + `type_decision.html` + (new) `cycle_summary` partial.

### 3.8 `constellation.go` (per-bead)

- Per-bead SVG of similar / forward (depends-on) / reverse (depended-on-by) edges, hydrated with title + type.
- **Gardener workflow served:** structural — could render a small constellation of "this gardener → crew → child tasks". The gardener identity edge isn't expressible today (no `gardener_of` dep type).
- **Reusable:** rendering pipeline is fine; it just needs an edge-type filter knob.

### 3.9 `decisions.go` (`/decisions`)

- Open decisions across all projects with project filter and resolve actions.
- **Gardener workflow served:** blockers (4) — but cross-gardener. A per-gardener "open decisions raised by my crew" panel needs a `raised_by_agent` filter that joins through the spawning agent bead to its gardener.

### 3.10 What's missing as a primitive

| Need | Closest existing surface | What's missing |
|---|---|---|
| Gardener-as-identity lookup | `/schedules` row | No association edge from agent → schedule; only the cycle agent has `schedule_id` |
| Gardener → driver epic | none | `driver_epic` is in the prompt, not a field |
| Cycle summary (last N) | comments on schedule bead | No structured field; wrapup JSON lives on the agent bead |
| Crew → gardener | none | No `gardener_of` field on crew agent beads |
| MR → gardener | `/agents` (loosely, via project) | MR beads carry only `project` label, not the gardener that drove them |

These are the gaps §4 proposes to close.

---

## 4. Supplementary / associated bead patterns

This is the heart of the mention: "patterns of supplementary beads and associated beads to enrich such a view, even by increasing agent reporting requirements." Today's patterns, by maturity:

### 4.1 Already-emitted, partly structured

- **Scheduled-gardener bead per cycle.** Created by the scheduler, type=task, prompt baked in. Carries `fields.schedule_id` linking back to the `schedule` bead. Lifecycle ends with a `wrapup` JSON blob passed to `gb stop --wrapup '{...}'`.
- **Agent bead per cycle.** type=agent, fields include `agent_state`, `pod_phase`, `task_id`, `task_title`, `role`, `project`. The gardener agent's `task_id` points at the scheduled-gardener bead; crew agents' `task_id` points at the child task they were spawned against. There is no field that says "this crew agent's gardener is X" — that mapping has to be reconstructed from the spawn-time prompt.
- **Gardener comments on its scheduled bead.** Each cycle the gardener typically drops 1–N comments via `gb squawk` (which routes through the bridge), `kd comment`, or `gb stop --wrapup`. The `wrapup` content lands in the bead lifecycle event but is not consistently stored in a structured field on the agent bead.

### 4.2 Loosely-conventional

- **Decision beads filed by the gardener or its crew.** Type=decision, but no field linking back to the gardener. Operator finds them via `created_by` (agent ID) and a manual lookup.
- **MR beads created by the gardener or its crew.** Type=mr, label `project:<name>`, comment trail with CI/E2E status. No gardener association.
- **Child task beads under the driver epic.** Linked via `depends_on`/`blocks` to the epic. The epic is the gardener's de-facto root, but the link is gardener → epic via the prompt, not the schedule bead.
- **`gb squawk` posts.** Echoed to Slack by the bridge; a thread per spawning channel. Not all squawks become beads; comments-on-self or comments-on-driver-epic depend on agent style.

### 4.3 Ad-hoc

- **"Watch" beads.** A gardener occasionally pins a bead it cares about via comment or by cross-linking. No formal pattern.
- **Cycle reports.** Some gardeners drop a long-form comment on the schedule bead at `gb stop`; others just rely on the wrapup blob; a few don't report at all. There is no convention.
- **Blocker rollups.** Watchdog gardeners (ka-0000001612) detect FAILED cycles and file bug beads; this is one of the few cross-gardener summaries.

### 4.4 Proposed enrichment (research recommendation, not a contract)

The richest fork-ready substrate would extend the schema along three axes. Each adds a small reporting cost; the trade-offs are in §5.

| Pattern | Where it lives | Producer | Consumer (viewer) |
|---|---|---|---|
| `gardener_of:<schedule-id>` field on **scheduled-gardener agent bead** | `agent` bead `fields` map | controller / spawn path | per-gardener crew panel |
| `gardener_of:<schedule-id>` field on **crew agent bead** | `agent` bead `fields` map | spawn path (crew inherits from spawner) | per-gardener crew panel |
| `driver_epic:<bead-id>` field on **schedule bead** | `schedule` bead `fields` map | author of the schedule (one-time) | per-gardener header + epic shortcut |
| `cycle_summary` structured wrapup on **scheduled-gardener agent bead** | `agent` bead `fields` (existing wrapup blob, stored in a field) | gardener at `gb stop --wrapup` | per-gardener timeline |
| Optional supplementary `cycle_report` task bead per cycle | type=task, label `cycle-report`, parents the agent bead | gardener at end of cycle | per-gardener timeline (long-form) |
| `MR → gardener` association: copy `gardener_of` to MR beads created during the cycle | `mr` bead `fields` (or label) | controller MR-reporter | "merged this cycle" panel |

The **minimum viable** subset for the gardeners view is just the first three rows. Everything else is enrichment.

---

## 5. Agent reporting requirement changes — trade-offs

### 5.1 What we'd be asking gardeners to do

The status-quo reporting is light: `gb squawk` for narration, `gb stop --wrapup '{accomplishments, blockers, handoff_notes}'` for the closeout. The doc proposes adding two requirements:

1. **A `cycle_summary` field on the gardener's agent bead at `gb stop`** — already partly there as `wrapup`, but the viewer can't rely on it being structured. Make the schema explicit: `{ accomplishments, blockers, mr_links[], decisions_raised[], crew_spawned[], outcome: SUCCESS|PARTIAL|FAILED }`.
2. **Gardener-of association on every spawn** — the `gb spawn` codepath already knows the spawning agent; have it propagate `gardener_of:<spawning-gardener's schedule-id>` onto the new agent bead's `fields`. Crew never has to know it exists; gardeners don't have to remember to tag.

Two further options the doc *does not* recommend up front, listed for completeness:

3. **Periodic squawk → bead.** Every cycle, the gardener auto-drops a `cycle_report` task bead (single comment, machine-readable). Rich for the timeline panel; doubles bead-write volume per gardener per cycle.
4. **MR ↔ epic association tag.** Gardener copies `driver_epic` onto each MR it merges. Cheap when the MR is opened by the gardener itself; messy when crew opens the MR (crew has to know its gardener).

### 5.2 Trade-offs

| Change | Cost | Visibility win | Risk |
|---|---|---|---|
| Structured `cycle_summary` on stop | Gardeners already write wrapup; just type-pin the schema | Per-cycle accomplishments / blockers render directly in the timeline panel | Gardeners who don't conform produce empty cells; mitigation: lint at `gb stop` |
| `gardener_of` on agent spawn | Single-line change in spawn path; zero gardener effort | Crew → gardener pivot becomes a cheap query | Crew chains (crew spawning crew) inherit transitively — keep the *root* gardener, not the spawner |
| `driver_epic` on schedule | One-time, manual, by schedule author | Gardener header links straight to epic without prompt parsing | Schedules without a single-epic driver (e.g., Auditor) will leave it empty — fine |
| Supplementary `cycle_report` bead | Gardener runs one `kd create` per cycle | Long-form per-cycle timeline ("what changed since last fire") | Bead bloat: 24 cycles/day × ~15 gardeners × 365 days = ~130k beads/year — pgvector + index cost is real |
| MR ↔ gardener copy | Gardener-side bookkeeping or controller-side inference | Direct "merged this cycle" panel | Misattribution if MR is merged async by a different agent; mitigation: emit at MR-merge-event time, not spawn time |

The recommended floor (1) + (2) + (3) is cheap and gets the view 80% of the way. (4) and the `cycle_report` bead are explicit follow-ups gated on the chosen fork.

---

## 6. Three concrete fork sketches

Each fork is annotated with handler / template / route impact. None are mutually exclusive long-term — the question is which to ship first.

### 6.1 Fork A — `/gardeners` top-level route + per-gardener page

**One-line.** A new top-level page with a gardener-roster index plus per-gardener detail; mirrors the iOS `GardenersTab` shape if/when one is built.

**Mockup (roster, `/gardeners`):**

```
┌── Beads Viewer ─────────────────────────────────────────────────────────────┐
│ Active │ Agents │ Decisions │ Schedules │ Gardeners ◀ │ Costs │ Ratings    │
├──────────────────────────────────────────────────────────────────────────────┤
│ Gardeners                                                  Filter: project: ▾ │
├──────────────────────────────────────────────────────────────────────────────┤
│ ⚓ Beads-Viewer Gardener                              cron: */20 * * * *      │
│   driver: ka-0000002538  last fire: 4m ago  next: 16m  status: ✅ HEALTHY    │
│   crew: 2 active   open decisions: 0   merged this cycle: 1                  │
│ ─────────────────────────────────────────────────────────────────────────── │
│ ⚓ Gardenio Feature Delivery                          cron: 0 * * * *         │
│   driver: ready-beads filter   last fire: 38m ago  next: 22m  ⚠ STALE        │
│   crew: 0 active   open decisions: 1 (kd-…)   merged this cycle: 0           │
│ ─────────────────────────────────────────────────────────────────────────── │
│ ⚓ gardener-release-owner                             cron: 0 * * * *         │
│   driver: golden release path   last fire: 12m ago  next: 48m  status: ✅    │
│   crew: 0 active   open decisions: 0   merged this cycle: 0                  │
│ ─────────────────────────────────────────────────────────────────────────── │
│ ⚓ Gardener-cycle-watchdog                                                    │
│   ❌ FAILED cycles in last 6h: 3 (Mezmo Custodian, Code Coverage, Auditor)   │
│ ─────────────────────────────────────────────────────────────────────────── │
│   …                                                                          │
└──────────────────────────────────────────────────────────────────────────────┘
```

**Mockup (per-gardener, `/gardeners/{schedule-id}`):**

```
┌── ⚓ Beads-Viewer Gardener ──────────────────────────────────────────────────┐
│ schedule: ka-0000002931   driver epic: ka-0000002538                         │
│ cron: */20 * * * *   tz: America/Vancouver   enabled: ✅                     │
│ last fire: 2026-04-27 14:32   next: 14:52   role: beads-viewer-gardener       │
├── Crew (2 active, 5 recent) ────────────────────────────────────────────────┤
│ • gardenio-research-gardeners-view-8sm  task: ka-0000004722  pod: Running 14m  │
│ • gardenio-per-type-templates-mq3      task: ka-0000000704 pod: Running 8m   │
│ • gardenio-comments-write-path-f2s     task: ka-0000002865 pod: Closed (38m)  │
│   …                                                                          │
├── In flight under this gardener (3) ─────────────────────────────────────────┤
│ • ka-0000004722  Research: gardeners view                  P2  4m ago         │
│ • ka-0000000704  Per-type detail templates                P2  8m ago         │
│ • ka-0000002865  Comment write path                       P1  open           │
├── Cycle history (24h) ──────────────────────────────────────────────────────┤
│ 14:32 ✅ — merged 1 (kd-…), spawned 2, 0 decisions raised                    │
│ 14:12 ✅ — merged 0, spawned 1                                               │
│ 13:52 ⚠ PARTIAL — no merges, 1 decision raised (kd-…)                        │
│ …                                                                            │
├── Open decisions raised by this gardener (1) ───────────────────────────────┤
│ • kd-…  "Pick gardeners-view fork (A/B/C)"  raised 4m ago                     │
├── Recent merged MRs (24h, 4) ───────────────────────────────────────────────┤
│ • !1428  fix(beads-viewer): empty-state copy           merged 12m ago        │
│ • !1426  feat(beads-viewer): /agents recent tail       merged 2h ago         │
│ …                                                                            │
└──────────────────────────────────────────────────────────────────────────────┘
```

**Go handler impact (`gardeners.go`, ~250 LOC).**

```go
// new file: gardenio/controller/cmd/beads-viewer/gardeners.go
mux.HandleFunc("GET /gardeners",                  s.handleGardenersRoster)
mux.HandleFunc("GET /gardeners/{id}",             s.handleGardenerDetail)

// composes 5 daemon calls: schedules list, cycle agents list (filtered by
// fields.schedule_id), crew agents list (filtered by fields.gardener_of),
// in-flight non-agent beads (filtered by fields.gardener_of OR depends-on
// driver_epic), open decisions (filtered by created_by.gardener_of).
```

**Template impact.**

- New `templates/gardeners.html` (roster) and `templates/gardener.html` (detail).
- Layout reuses existing partials: `agents.html` row → gardener-crew panel; `active_row.html` → in-flight panel; `type_decision.html` → decisions panel; `type_schedule.html` → gardener header.
- Add `cycle_summary.html` partial for the timeline (renders the structured wrapup field — empty-state-friendly).

**Route impact.**

- Two new routes; nav adds "Gardeners" entry between Schedules and Costs.
- `/schedules` link from a schedule row now also gets a "view as gardener →" link to `/gardeners/<id>`.

**Data prerequisites.**

- Minimum: `gardener_of` on agent beads (single-line spawn-path change) and `driver_epic` on schedule beads (manual annotation).
- Ideal: structured `cycle_summary` field on gardener agent beads (low-cost change to `gb stop --wrapup` flow).

**Pros.**

- Discoverable: "Gardeners" sits in the nav.
- Composable: per-gardener page reuses 4 existing partials + 1 new.
- iOS parity-ready: when the iOS app gets a `GardenersTab`, the data shape is already proven server-side.

**Cons.**

- Requires nav and IA decision (already pending for iOS-parity phase 2 — see ka-0000002538).
- 250 LOC handler + 2 templates + 1 partial is the largest of the three forks.

---

### 6.2 Fork B — gardener lens layered on `/agents`

**One-line.** Add `?as=gardener` to `/agents`; pivot the existing list into a gardener-grouped tree without touching nav or routes.

**Mockup (`/agents?as=gardener`):**

```
┌── Beads Viewer ─────────────────────────────────────────────────────────────┐
│ Agents                              Group: ( Project ▾ | Gardener ◀ )         │
│ Project: All ▾                                                               │
├──────────────────────────────────────────────────────────────────────────────┤
│ ⚓ Beads-Viewer Gardener  (ka-0000002931)        last fire 4m  next 16m       │
│   ├─ gardenio-research-gardeners-view-8sm      task ka-0000004722  Running 14m │
│   ├─ gardenio-per-type-templates-mq3          task ka-0000000704  Running 8m  │
│   └─ (recent) gardenio-comments-write-path-f2s  Closed 38m ago                │
│                                                                              │
│ ⚓ Gardenio Feature Delivery  (ka-0000004432)    last fire 38m  next 22m  ⚠   │
│   └─ (no active crew)                                                        │
│                                                                              │
│ ⚓ gardener-release-owner  (ka-0000006058)       last fire 12m  next 48m      │
│   └─ (no active crew)                                                        │
│                                                                              │
│ Unaffiliated agents (3)                                                      │
│   ├─ thread-… (ad-hoc thread agent)                                          │
│   …                                                                          │
└──────────────────────────────────────────────────────────────────────────────┘
```

**Go handler impact (small, ~60 LOC delta to `agents.go`).**

```go
// agents.go — new branch when r.URL.Query().Get("as") == "gardener":
//   1. List schedules (already used by schedules.go).
//   2. List active+recent agents.
//   3. Bucket agents by agent.fields["gardener_of"] → schedule-id; spill
//      anything missing the field to "Unaffiliated".
//   4. Hydrate each schedule's last-fire/next-fire (reuse scheduleView).
// Render gardener-grouped via a new template branch.
```

**Template impact.**

- `agents.html` grows a `{{ if eq .Group "gardener" }}` branch — or split into `agents.html` + `agents_by_gardener.html` partial.
- No new templates beyond that. Existing row partials reused verbatim.

**Route impact.** None (no new routes; `?as=gardener` is a query).

**Data prerequisites.** `gardener_of` on agent beads — necessary; without it every agent falls into "Unaffiliated".

**Pros.**

- Smallest diff. ~60 LOC + one template branch.
- Zero IA disruption.
- Cheap follow-on to existing `/agents` work.

**Cons.**

- Only answers "crew per gardener". Doesn't show in-flight non-agent beads, recent merges, decisions raised, or cycle history.
- Gardener identity is *the schedule + the driver epic*, not the running pod — pivoting via `/agents` reinforces the wrong frame.
- No place for the cycle timeline (the highest-leverage panel).

---

### 6.3 Fork C — enriched epic-detail view (driver epic = gardeners view)

**One-line.** When an epic has a `driver_epic` back-reference from a schedule bead, the existing `bead_show.html` for that epic gains a "Gardener" pane — schedule header + cycle history + crew + recent merges — directly above the existing children/comments panes.

**Mockup (`/ka-0000002538`):**

```
┌── ka-0000002538 — Epic: beads-viewer iOS-parity phase 2 ───────────────────┐
│ status: in_progress   priority: P2   project: gardenio                       │
├── Gardener ──────────────────────────────────────────────────────────────────┤
│ ⚓ Beads-Viewer Gardener  (schedule ka-0000002931)                           │
│   cron */20 * * * *   last fire 4m   next 16m   ✅ HEALTHY                  │
│   Crew (2 active):  gardenio-research-gardeners-view-8sm,                     │
│                     gardenio-per-type-templates-mq3                          │
│   Cycle history (24h): 3✅  1⚠  0❌                                         │
├── Children (existing) ──────────────────────────────────────────────────────┤
│  • ka-0000006123  Research iOS-parity         closed                        │
│  • ka-0000000704  Per-type detail templates    in_progress                  │
│  • ka-0000002865  Comment write path           open                         │
│  …                                                                          │
├── Comments (existing) ──────────────────────────────────────────────────────┤
│ …                                                                           │
├── Constellation (existing) ─────────────────────────────────────────────────┤
│ …                                                                           │
└──────────────────────────────────────────────────────────────────────────────┘
```

**Go handler impact (small, ~80 LOC delta to `bead_show` + a hydrator).**

```go
// In bead_show handler: if bead.Type == "epic" {
//   look up schedule(s) where fields.driver_epic == bead.ID;
//   if any: hydrate gardenerPaneView (reuse scheduleView + agentRowView).
// }
// Pure additive; epics without a gardener render unchanged.
```

**Template impact.**

- New `templates/partials/gardener_pane.html` — composes `type_schedule.html` summary + condensed agent rows + cycle-history strip.
- `bead_show.html` gets one `{{ if .GardenerPane }}{{ template "gardener_pane" .GardenerPane }}{{ end }}`.

**Route impact.** None (lands on the existing `/{id}` route).

**Data prerequisites.**

- `driver_epic` field on schedule bead is **load-bearing** — without it the lookup is impossible. Manual annotation per schedule (one-time).
- `gardener_of` on agent beads to populate the crew strip (same as Forks A and B).

**Pros.**

- Zero new routes, zero IA disruption.
- Gardener context appears *exactly where the operator already goes* — the driver epic.
- Naturally degrades: epics without a driver schedule render as today.

**Cons.**

- Discoverability: a roster (Fork A) is still the natural top-of-funnel. Fork C does not produce one.
- Hard ceiling on density: epic detail page is already crowded (children + comments + constellation + ratings + actions). Adding a gardener pane risks turning it into a wall of partials.
- One gardener = one driver epic is the assumed shape; multi-epic gardeners (e.g., release-owner, watchdog) have nowhere to land.

---

### 6.4 Comparison

| Axis | A (`/gardeners`) | B (`/agents?as=gardener`) | C (epic-detail) |
|---|---|---|---|
| New routes | 2 | 0 | 0 |
| New handler LOC | ~250 | ~60 | ~80 |
| New templates | 2 + 1 partial | 0 (or 1 partial) | 1 partial |
| Discoverable | ✅ nav entry | partial (group toggle) | ❌ requires knowing the epic |
| Roster | ✅ | ✅ | ❌ |
| Cycle timeline | ✅ | ❌ | ✅ |
| In-flight non-agent | ✅ | ❌ | partial (children) |
| Recent merges | ✅ | ❌ | ❌ |
| Multi-epic gardeners | ✅ | ✅ | ❌ |
| Data prereqs | floor + ideal | floor only | floor + driver_epic |

The recommendation in §1 is **A**, with the per-gardener detail page composed from components that are also useful in B and C. Building A creates the substrate; B and C can follow as cheap lenses.

---

## 7. iOS / Slack parity gap

**iOS Gardenio app (`usr/Gardenio/Gardenio/BeadsTab.swift`, `BeadDetailView`).** Today shows: tabbed beads list (in-progress, agents, recent), bead detail with comments, spawn affordances. **No gardener-specific view.** No notion of schedule → cycle → crew rollup. Operator on iOS today reconstructs a gardener by hand: open the schedule bead, then jump to the agent bead, then jump to the driver epic.

**Slack.** The bridge auto-posts bead lifecycle events, comments, and `gb squawk` to the spawning thread. A gardener's cycle becomes a tail of messages in a long-lived thread — high-fidelity, narration-rich, but linear. A gardener's `gb stop --wrapup` posts as a single rendered message. There is no consolidated "give me the state of all gardeners" view — the operator scrolls.

**Gap the gardeners view closes.**

- **Cross-gardener liveness at a glance** — neither iOS nor Slack offers it. Slack threads are per-gardener; iOS has no gardener pivot.
- **Cycle timeline as structured rows** — Slack has narrative, beads-viewer can have grid. The structured wrapup field (proposed in §4.4) is the difference.
- **One-click pivot from "is the gardener alive?" → "what is its crew doing now?" → "what merged?"** — a single page in beads-viewer; multiple jumps everywhere else.

**Parity follow-up (not in this MR).** Once Fork A ships server-side, an iOS `GardenersTab` mirroring `/gardeners` is a natural follow-up, fed by the same daemon endpoints. It is **out of scope** for this research.

---

## 8. Recommended fork + reporting bundle

Recommended pairing for the decision in Appendix A:

- **Fork: A.** The `/gardeners` route. Discoverable, composable, the per-gardener page reuses Fork C's pane.
- **Reporting requirements floor:**
  - `gardener_of:<schedule-id>` field on every spawned agent (controller spawn-path change; no gardener effort).
  - `driver_epic:<bead-id>` field on each schedule (one-time manual annotation by schedule author; optional — schedules without it just don't get the epic shortcut).
  - Structured `cycle_summary` schema on `gb stop --wrapup`: `{ accomplishments, blockers, mr_links[], decisions_raised[], crew_spawned[], outcome }`. Linted at stop time; missing fields render as empty rows in the timeline rather than blocking the stop.
- **Reporting requirements deferred** (operator may add later): supplementary `cycle_report` task bead per cycle, MR ↔ gardener copy. Both can be retrofitted without changing the view.

If the operator picks Fork B or C instead, the floor changes:

- **B floor:** `gardener_of` only. Everything else is decorative.
- **C floor:** `gardener_of` + `driver_epic`. No structured wrapup needed (the timeline panel is not part of the epic-detail render in C).

---

## Appendix A — drafted `gb decision create` invocation

This is the **drafted** invocation for the decision bead the next iteration should file. **Do not execute it from this research bead** — let the gardener (or this MR's reviewer) file it after the doc is read.

```bash
gb decision create --no-wait \
  --prompt='Gardeners view for beads-viewer: which fork + what reporting floor? Research doc: gardenio/docs/research/beads-viewer-gardeners-view.md (ka-0000004722). Three forks evaluated; recommendation in §8 is Fork A with floor (gardener_of + driver_epic + structured cycle_summary).' \
  --options='[
    {
      "id": "fork-a-floor",
      "short": "Fork A + floor",
      "label": "Build /gardeners route + per-gardener page. Reporting floor: gardener_of on agent spawn, driver_epic on schedule bead, structured cycle_summary at gb stop. Defer cycle_report bead and MR↔gardener copy.",
      "artifact_type": "epic"
    },
    {
      "id": "fork-a-full",
      "short": "Fork A + full reporting",
      "label": "Build /gardeners route + per-gardener page AND adopt full reporting bundle (floor + cycle_report bead + MR↔gardener copy). Higher upfront cost; richer timeline panel from day one.",
      "artifact_type": "epic"
    },
    {
      "id": "fork-b",
      "short": "Fork B (lens)",
      "label": "Layer ?as=gardener on /agents. Smallest diff (~60 LOC). Reporting floor: gardener_of only. No timeline, no merges panel, no decisions panel.",
      "artifact_type": "plan"
    },
    {
      "id": "fork-c",
      "short": "Fork C (epic detail)",
      "label": "Add gardener pane to driver-epic bead-show. No new routes. Reporting floor: gardener_of + driver_epic. No top-of-funnel roster.",
      "artifact_type": "plan"
    },
    {
      "id": "stack-c-then-a",
      "short": "Stack: C now, A next",
      "label": "Ship Fork C first (cheap, lands gardener context where the operator already goes), then escalate to Fork A in a follow-up epic if a roster is still wanted.",
      "artifact_type": "epic"
    },
    {
      "id": "defer",
      "short": "Defer",
      "label": "Park the gardeners view. Capture the doc as state of the art; revisit when reporting changes have been adopted independently or when iOS app adds a GardenersTab.",
      "artifact_type": "report"
    }
  ]'
```

Notes for the operator:

- Every option has an `artifact_type` per the decision-interface advice (kd advice).
- "Defer" is a real option — none of the three forks is gated by an existing release; the gap exists today but is not blocking any gardener.
- The floor reporting changes (controller spawn-path `gardener_of` propagation, schedule `driver_epic` field, structured wrapup schema) are individually small and useful even without a gardeners view; if the decision is "defer", these can still be adopted as a separate follow-up.

---

## Appendix B — open questions surfaced during research

1. **Gardener identity vs. schedule identity.** A gardener whose schedule is rotated (cron tweak, role rename) gets a new schedule bead. Should the view treat that as a new gardener or roll history forward? Suggestion: roll forward via a `previous_schedule:<id>` field on the new schedule. Out of scope for this MR.
2. **Crew chains.** Crew that spawns crew — should `gardener_of` propagate transitively? Recommendation: yes, root gardener wins. Watch for chains in cost/rating attribution.
3. **Multi-epic gardeners** (release-owner, watchdog, auditor). Fork C cannot represent these; Fork A handles them via the schedule-as-identity pattern.
4. **Auditor and other read-only gardeners.** Some gardeners don't ship MRs at all. The "merged this cycle" panel renders empty for them — a feature, not a bug, but worth labeling.
5. **Cycle timeline density.** 24h × 20-min cadence = 72 cycles per gardener. Need a paging/folding strategy on the per-gardener page (suggested: 24h expanded, 7d collapsed, 30d on demand).

---

*End of research note.*
