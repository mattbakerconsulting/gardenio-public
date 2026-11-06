# Escalation beads review (2026-05-05)

A review of the state of escalation beads across gardenio and beads-viewer,
plus a series of decisions for the operator. Written in response to a
Slack request: "review the state of escalation beads in gardenio and beads
viewer and make suggestions to make them better; as a series of
decisions."

## TL;DR

- Escalations and decisions are deliberately separated (distinct types,
  distinct purposes). That part is good.
- Two human-facing surfaces have grown apart: beads-viewer renders
  decisions richly but does not render escalations at all. Operators
  must check the bridge SPA at `/api/escalations/...` for one queue and
  beads-viewer for the other.
- 22 open escalations sit in the queue right now, including P0s aged
  ~3 weeks, plus 4 near-duplicate "RWX_ACCESS_TOKEN expired" rows from
  different agents. Symptoms of: no aging signal, no dedup, no surface
  on the dashboard, mixed semantics (request vs incident report).
- Five decisions below propose concrete forks. They are independent —
  pick any subset.

## Context: what is what

### Escalation (`type:escalation`, `kind:data`)
- Privileged or audit-worthy operation request (rotate creds, mint
  tokens, grant IAM, replay a denied write).
- Approval requires authenticated OIDC + capability; lives in the
  bridge SPA (`gardenio/controller/internal/bridge/web/escalations.html`).
- Slack notification only (no inline approve buttons — phishing-resistant
  by design).
- Lifecycle: `open` → `in_progress` (post-approve) → `closed`.
- Files of record:
  - `kbeads/internal/server/http_escalations.go`
  - `gardenio/controller/internal/bridge/api_escalations.go`
  - `gardenio/controller/internal/bridge/escalations.go`
  - `gardenio/controller/internal/bridge/bot_escalations.go`
  - `gardenio/controller/cmd/gb/escalation.go`

### Decision (`type:decision`)
- Informal human judgment fork ("which option?").
- Approval surface: beads-viewer `/decisions` list page with chip-resolve
  + dialog (`gardenio/controller/cmd/beads-viewer/decisions.go`,
  `templates/decisions.html`, `templates/partials/type_decision.html`).
- Slack message has clickable option buttons (`bot_decisions.go`).
- Lifecycle: `open` → `closed` once chosen/dismissed.
- Has anti-spam validation in `kbeads/internal/server/decision_validation.go`
  (rejects single `[ack]`/`[done]` options, etc.).

### Surface map (current)

| Surface                           | Decisions       | Escalations    |
| --------------------------------- | --------------- | -------------- |
| beads-viewer `/decisions` page    | yes             | no             |
| beads-viewer health tile          | yes             | no             |
| beads-viewer bead detail partial  | `type_decision` | falls back to `type_generic` (loses operation_type, sensitivity, script, creds, outputs) |
| beads-viewer `/<id>` deep link    | rich            | generic        |
| Bridge web SPA                    | no              | yes (auth+caps) |
| Slack inline buttons              | yes             | no (link-out)  |
| Slack message updates on resolve  | yes             | yes            |

## What is wrong today

1. **No escalation surface in beads-viewer.** The dashboard shows
   "Decisions waiting on operator" but never "Escalations waiting on
   operator". A growing escalation queue is invisible from the place
   most operators land first. (Confirmed: `grep -r escalation
   gardenio/controller/cmd/beads-viewer/` returns nothing.)
2. **Bead detail view loses escalation structure.** Clicking
   `ka-0000001006` in beads-viewer renders the generic partial, hiding
   `operation_type`, `sensitivity`, `execution_script`,
   `credential_requirements`, `expected_outputs`, `requesting_agent`,
   `result_status` — exactly the fields that distinguish an escalation.
3. **Stale queue.** 22 open escalations; oldest P0 from 2026-04-14
   (~3 weeks). No aging signal, no nag, no auto-close.
4. **Duplication.** 4 separate "RWX_ACCESS_TOKEN expired" escalations
   filed by 4 different agent runs (ka-0000001006, ka-0000006645,
   ka-0000008093, ka-0000006958). The agents had no signal that one was
   already open and waiting.
5. **Fuzzy bead-type boundary.** Some "escalations" are really
   incident reports ("X is broken, please fix"), not privileged
   operation requests. Bug/incident triage is poorer for it, and the
   escalation queue gets noisier.
6. **Slack-to-approval friction.** Operators read Slack constantly but
   approve from a separate authenticated SPA. The hop is small but
   non-zero, and combined with no aging signal it produces the stale
   queue.
7. **Missing escalation `slug` on every bead I sampled.** Probably a
   minor bug in `gb escalation create`.

## Decisions (the asks)

### Decision 1 — beads-viewer should show escalations
**Problem:** operators have to check two places for human-blocking
work. Today escalations are invisible from the dashboard most
operators land on.

| Option           | Action                                                                                                                                                              | Cost                                          |
| ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------- |
| **1A** unify     | Add `/escalations` page + `type_escalation` partial + health tile. Read-only there; approve/reject still link to authenticated bridge SPA (caps live there).        | ~1 day, mostly templates + 1 handler.         |
| **1B** status quo | Keep escalations in bridge SPA only; add a clear cross-link from beads-viewer.                                                                                      | ~1 hour, just a banner.                       |
| **1C** consolidate | Move escalation approval into beads-viewer behind OIDC; deprecate bridge SPA.                                                                                     | Bigger — auth/caps refactor; 2-3 days.        |

**Recommend: 1A.** Read-only listing + structured detail view in
beads-viewer keeps the auth boundary intact (caps still gate
approve/reject in the SPA) while restoring single-pane-of-glass.

---

### Decision 2 — escalation deduplication
**Problem:** 4 near-identical "RWX_ACCESS_TOKEN expired" escalations.
Agents repeatedly file because they have no way to see an existing one.

| Option              | Action                                                                                                                                                                                                | Cost                                          |
| ------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------- |
| **2A** fingerprint  | `gb escalation create` computes a fingerprint over `(operation_type, requesting_task or stable subset of operation_spec)`. If an open escalation with that fingerprint exists, **comment + reflag**, do not create new. | ~1 day in `cmd/gb/escalation.go` + server-side. |
| **2B** reflag-only  | Server returns 409 + existing escalation ID when an open dup is detected; agent prints "joined existing escalation" instead of filing a new one.                                                      | ~half day; thinner.                           |
| **2C** status quo   | Operator triages duplicates manually.                                                                                                                                                                 | 0 cost, ongoing pain.                         |

**Recommend: 2A.** Reflagging an existing escalation is more useful
than failing the create — agents adding fresh context as a comment is
strictly better signal than nothing.

---

### Decision 3 — escalation aging / staleness signal
**Problem:** P0 escalations from 2026-04-14 are still open. The
dashboard does not communicate that.

| Option                  | Action                                                                                                                                                                                              | Cost                  |
| ----------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------- |
| **3A** dashboard tile   | Add `health_tile_escalations` with pending count + oldest age + per-priority SLA breach pill (P0>4h, P1>24h, P2>72h, P3>7d).                                                                        | ~half day.            |
| **3B** Slack digest     | Daily 09:00 ops-channel post listing open escalations oldest-first with SLA pills.                                                                                                                  | ~half day.            |
| **3C** both             | Tile for at-a-glance + digest for a forced-attention nudge.                                                                                                                                         | ~1 day.               |
| **3D** no signal        | Trust operators to check the SPA.                                                                                                                                                                   | 0; risk of recurrence. |

**Recommend: 3C.** Tile is for the operator who lands on the dashboard;
digest is for the operator who never does. They are complementary.

---

### Decision 4 — clean up bead-type boundary (escalation vs bug/incident)
**Problem:** several "escalations" in the queue are incident reports
that should be `bug` or `incident` beads. The line is muddy and the
escalation queue is being used as an incident queue.

| Option                       | Action                                                                                                                                                                                                                                | Cost                                                              |
| ---------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------- |
| **4A** tighten validator     | `kbeads/internal/server/decision_validation.go`-style guard for escalations: require non-empty `operation_type` from a closed enum (`aws_iam`, `k8s_rbac`, `credential_rotation`, `vault_provisioning`, `replay_denied_write`, `other`). Reject `other` without `--force`. Add a hint pointing agents at `bug`/`incident` types when they describe an outage. | ~half day.                                                        |
| **4B** subtype field         | Add `escalation_kind: request \| incident_report` field. Surfaces in UI; doesn't gate creation.                                                                                                                                       | ~half day; less behaviorally useful but more flexible.            |
| **4C** triage script         | Operator-side script that bulk-converts escalations matching incident-shaped patterns to `bug` type.                                                                                                                                  | ~hours; doesn't prevent recurrence.                               |
| **4D** status quo            | Accept the boundary fuzz; document it.                                                                                                                                                                                                | 0 cost, ongoing dilution.                                          |

**Recommend: 4A.** Validators are how we already enforce decision
hygiene; mirror that pattern.

---

### Decision 5 — TTL / auto-close for stale escalations
**Problem:** escalations live forever. The currently-open queue
contains items where the underlying need has likely passed.

| Option                  | Action                                                                                                                                                                              | Cost                                                |
| ----------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------- |
| **5A** per-priority TTL | P0 14d, P1 21d, P2 30d, P3+ 14d. On expiry: auto-close with `result_status=expired` and a comment instructing the requesting agent to re-file with fresh context.                  | ~half day in `kbeads` reaper.                        |
| **5B** TTL via `--ttl`  | Add `--ttl` to `gb escalation create`, mirroring advice beads. Default off; agents may opt in. Reaper closes on expiry.                                                              | ~hours; opt-in means stale ones stay stale.         |
| **5C** soft nag only    | Reaper posts a Slack ping at SLA breach; never auto-closes.                                                                                                                          | ~half day; nagging without resolution.              |
| **5D** status quo       | Manual.                                                                                                                                                                              | 0 cost, ongoing decay.                              |

**Recommend: 5A.** Aligns with "if we did not action it in N days, the
need is probably stale" reality.

---

## Out of scope here, worth a follow-up

- Empty `slug` on every escalation bead — small bug to chase in
  `gb escalation create`.
- `service_account` execution target is still unimplemented; that has
  its own design space and is independent of the UI questions above.
- Escalation-on-write-denial replay flow is good but undocumented in
  the beads-viewer detail view; if 1A lands, rendering the
  `operation_spec` of a write-denial replay surfaces the original
  request inline.

## Operator feedback round 1 (2026-05-05)

Operator (@matthew.baker) resolved 4/5 of the original decisions:

| # | Resolution                                                                                                                                                                                                                       | Status |
| - | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------ |
| 1 | **1C — consolidate into beads-viewer behind OIDC; deprecate bridge SPA.** Bigger than my recommendation; auth/caps refactor lives in beads-viewer.                                                                                | done   |
| 2 | **2C — manual triage.** No fingerprint logic; operator absorbs duplicate cost.                                                                                                                                                   | done   |
| 3 | Rejected current options. Want **more alternatives that live in beads-viewer**, not Slack/email channels. See "D3 — revised" below.                                                                                              | reopen |
| 4 | Rejected enum-based validator. *"Main purpose of escalation is to get a human-driven agent in the loop; mostly important to be specific about what access is needed and who has it and how to get it; enumerated fields not terribly helpful."* See "D4 — revised" below. | reopen |
| 5 | Pending.                                                                                                                                                                                                                          | open   |

Follow-up decisions filed:

- **D3 revised:** ka-0000005606 (aging signal options entirely within beads-viewer)
- **D4 revised:** ka-0000003919 (access_needed / who_has_it / how_to_get_it free-text schema)

## Operator feedback round 2 (2026-05-05)

| # | Resolution                                                                                                                                                            | Status |
| - | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------ |
| 3 (revised) | *"Nah we are still not there yet… throw some new ideass at me."* All 7 visual-decoration options dismissed. Need a different framing — see "D3 — revised v2" below. | reopen |
| 4 (revised) | Pending.                                                                                                                                                              | open   |
| 5 (orig)    | Pending.                                                                                                                                                              | open   |

## Operator feedback round 3 (2026-05-05)

| # | Resolution                                                                                                                                                                                                                                                                                                                                                                                                                                       | Status |
| - | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ------ |
| 3 (revised v2) | **3R2-B — unified `/operator-inbox`.** *"Single page ranks all human-blocking work (escalations + decisions + bugs) by composite urgency score; aging is implicit in ranking."* Rationale: *"Operator inbox is convergently essential but needs care in its design."* The aging problem is reframed as a ranking problem: stale items sink only if newer/more-urgent items outrank them, which is itself the signal that aging matters. | done   |
| 4 (revised)    | Pending.                                                                                                                                                                                                                                                                                                                                                                                                                                          | open   |
| 5 (orig)       | Pending. **Note**: D3-r2's resolution materially weakens the case for D5 (auto-close). With aging implicit in ranking, stale items lose visibility automatically; the original "auto-close after N days" lever is now belt-and-suspenders rather than primary. The operator may want to revise D5 toward "soft-archive after N days" or withdraw it entirely.                                                                                       | open   |

### D3 — resolution implications

Picking 3R2-B doesn't moot the round-3 alternatives that *complement* a unified
inbox; it just makes them sub-decisions of the inbox design. Specifically:

- **3R2-D self-healing verification** — an inbox that ranks stale items still
  benefits from collapsing items whose underlying need is verifiably resolved.
  Worth keeping as a follow-up; reduces the size of the inbox without changing
  ranking semantics.
- **3R2-E snooze and resurface** — natural fit for a unified inbox: snooze
  pushes a row out of the ranked feed until a deadline or trigger.
- **3R2-H next-up tile** — degenerates to "the top row of the inbox", which is
  how a ranked inbox already behaves. Subsumed.
- **3R2-A rescue gardener**, **3R2-C banner-on-blocker**, **3R2-F predicted
  self-resolve**, **3R2-G on-call rotation** — orthogonal to the inbox; can
  layer later.

### Follow-up: design epic for the unified `/operator-inbox`

Per the rationale "needs care in its design", the next deliverable is a design
doc, not an implementation. Planned as an epic with these research tasks:

1. **What counts as "human-blocking"?** Inventory the bead types and predicates
   that should feed the inbox: `type=escalation status=open`, `type=decision
   status=open`, `type=bug priority<=1`, `type=task` flagged for operator. Where
   does the predicate live (server query, viewer filter, separate index)?
2. **Composite urgency score.** Define the formula. Strawman: `score = priority_weight(p) × age_factor(t/sla) × downstream_blocked_count`. Decide
   `priority_weight`, `age_factor` shape (linear? logistic? per-type SLAs?),
   how `downstream_blocked_count` is computed (graph walk vs cached counter).
   Sanity-check on the current 22-bead queue: does the ranking match what the
   operator would have picked by hand?
3. **Per-type rendering.** A unified inbox row needs to render heterogeneously
   (escalation = access fields; decision = options; bug = title + repro). What
   does a row partial look like that doesn't lose the per-type richness?
4. **Action surface per row.** What can the operator do without leaving the
   page? Decision = pick option (already exists). Escalation = approve/reject
   (after 1C lands behind OIDC). Bug = comment / claim. Task = reassign /
   close. Where does the line live between "act inline" and "drill into bead
   detail"?
5. **Interaction with existing `/decisions` page.** Replace it, or keep
   `/decisions` as a typed view of the unified inbox? Same question for any
   future `/escalations` page that 1C produces.
6. **Visibility / privacy.** Inbox is per-operator (on-call rotation, see
   3R2-G later) or shared? If shared, how do we avoid two operators acting on
   the same row?
7. **Performance.** Composite ranking on every request vs cached. The query
   touches all open beads of N types — what's the index story?

Each item is a research task that lands a section in
`gardenio/docs/research/operator-inbox-design.md`. The epic is locked to
research/design until the operator approves a target shape.

### D3 — revised: aging signal **inside beads-viewer**

Same problem (operators don't see that P0 escalations are 3 weeks stale). New
constraint: signal lives in beads-viewer, not Slack/digest channels. Options
are NOT mutually exclusive — a real implementation will combine cheap pieces.

| Option                       | What renders, where                                                                                                                                                                                                                            | Cost              |
| ---------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------- |
| **3R-A** age-pill column     | `/escalations` list grows an **age** column with a colored pill (green ≤ SLA, amber 1–2× SLA, red > 2× SLA). Default sort is `oldest first`. Mirrors the way decisions list shows priority chips. | ~half day templates + handler. |
| **3R-B** dashboard tile      | Health tile on `/` — pending count + oldest age + per-priority SLA breach pill (P0>4h, P1>24h, P2>72h, P3>7d). Click-through goes to filtered `/escalations?breached=1`.                                                                       | ~half day.        |
| **3R-C** breach banner       | Persistent top-of-page banner on every beads-viewer page when ≥1 escalation is past SLA: *"⚠ 2 escalations past SLA — oldest 21d (kd-…)"*. Dismissible per-session.                                                                            | ~hours.           |
| **3R-D** detail-view chip    | On the bead detail page (`/<id>`) for any open escalation, render a colored "age vs SLA" chip + the SLA threshold; surfaces aging when an operator opens any single escalation.                                                                | ~hours.           |
| **3R-E** sticky stale group  | `/escalations` page splits into three collapsible sections: **Past SLA**, **Aging (≥ 50% of SLA)**, **Fresh**. Forces past-SLA into the visual top of the page even if newer beads were just filed.                                            | ~half day.        |
| **3R-F** tab title badge     | Browser tab title becomes `(N) beads-viewer` whenever any escalation is past SLA, where N is the breached count. Catches operators with the tab in the background.                                                                            | ~hours; small JS. |
| **3R-G** combine A + B + C   | The minimum that gives at-a-glance, in-list, and forced-attention coverage without venturing outside beads-viewer.                                                                                                                             | ~1 day.           |
| **3R-H** none                | No aging signal in beads-viewer. Operator triages by hand.                                                                                                                                                                                     | 0; status quo.    |

**Recommend: 3R-G.** Tile (B) for the dashboard landing experience, age-pill
list (A) for working through the queue, breach banner (C) so an unrelated page
visit still surfaces overdue P0s. All three reuse data the server already has.

### D3 — revised v2: reframe aging from "visual decoration" to "action-driving"

Round 2 (visual pills, dashboard tiles, banners, badges, sticky sections) did
not resonate. Operator wants fresh angles, not a permutation of the same
"decorate the queue" theme. The reframe: **the purpose of a staleness signal
is to recruit a human-driven agent into the loop or to remove the escalation
from the queue altogether — passive UI does neither**. So instead of options
that "make the operator notice", these options "make the system act".

| Option                                | What changes                                                                                                                                                                                                                                                                                                                                                                              | Cost                                          |
| ------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------- |
| **3R2-A** auto-spawn rescue gardener   | At SLA breach, spawn a `role:gardener` agent on the escalation bead. It re-reads the request, verifies the underlying need is still real (e.g. is the cred actually expired?), comments with status, escalates the priority, or auto-closes if the need has dissolved. Aging becomes a trigger, not a UI element.                                                                          | ~1 day; uses existing `gb spawn` + scheduler. |
| **3R2-B** unified `/operator-inbox`   | Single page that ranks ALL human-blocking work (escalations + decisions + bugs flagged for operator) by composite urgency = `priority × age_factor × downstream_blocked_count`. Operator opens one URL to see "next thing to handle". Aging is implicit in ranking; no separate queue per type.                                                                                            | ~1 day; reuses decisions list code.           |
| **3R2-C** banner on the blocking bead | Don't decorate `/escalations` at all — instead, every bead that has a downstream escalation waiting on it grows a top-of-page banner: "AGENT WAITING ON YOU FOR: aws_iam grant — 21d ago". Aging surfaces in context with the work the operator is already looking at, not in a queue they have to remember to visit.                                                                       | ~half day; new partial + viewer hook.         |
| **3R2-D** self-healing verification   | Each escalation registers a `check()` predicate (e.g. credential rotation: try the cred; IAM grant: probe IAM; replay: try the write). Server runs it every N minutes; if it now passes, auto-close with `result_status=resolved_externally`. Aging only shows on escalations whose underlying need is *verifiably still real* — collapses noise dramatically.                            | ~1.5 days; per-type `check()` registry.       |
| **3R2-E** snooze and resurface        | Replace always-on aging UX with operator-driven aging: every open escalation has a "snooze 24h / 3d / 1w" control. Snoozed = quiet. Past SLA without snooze = pinned to top of `/operator-inbox`. Mirrors how email/Asana/Linear handle aging — operator acts on staleness rather than passively absorbing a pill.                                                                          | ~half day; one new field + UI control.        |
| **3R2-F** predicted self-resolve      | Server estimates `P(self-resolve)` from history of resolved escalations with the same fingerprint (e.g. "RWX_ACCESS_TOKEN expired" resolved itself 3/4 times when a new agent deployed). Aging includes that probability: detail page shows "21d old, 92% prob auto-resolved already, candidate for auto-close". One-click bulk-close for high-probability stale items.                  | ~1.5 days; needs fingerprint store + history. |
| **3R2-G** on-call rotation primitive  | Beads-viewer adds an on-call concept: at any time, exactly one human is "primary escalation operator" (defined via a schedule). New escalations route to their inbox; aging signal is "items in YOUR inbox > SLA". Catches "everyone thought someone else would do it". Aging becomes personal accountability, not collective drift.                                                       | ~2 days; schedule + assignment routing.        |
| **3R2-H** next-up action tile         | Dashboard surfaces ONE escalation at a time — the oldest open one — with full context, structured access fields, and approve / reject / "needs more info" buttons. Resolve it, the next-oldest replaces it. Forces sequential drainage. No queue UI, no aging colors; the queue becomes invisible *because it's actively being drained*.                                                  | ~1 day; new dashboard component.              |
| **3R2-I** combine D + E + H           | Self-heal kills noise (D) → snooze gives operator agency over what's left (E) → next-up tile force-drains the residue (H). Together: the aging problem largely solves itself, no decoration of stale items required.                                                                                                                                                                       | ~3 days; staged across components.            |
| **3R2-J** none of these               | Try yet another framing.                                                                                                                                                                                                                                                                                                                                                                  | n/a                                           |

**Recommend: 3R2-I (D + E + H combo).** Each piece attacks a different cause of
the stale queue: D removes escalations whose underlying need has dissolved
(probably most of the current 22-item queue), E gives the operator a way to
actively triage what remains without dismissing it, H forces the small
residual queue to keep moving. None of these are visual decoration — every
one of them removes work from the queue. If only one piece is in budget,
**3R2-D** alone would take the biggest bite out of the current backlog.

---

### D4 — revised: simplify schema to access-centric free-text

Operator framing: an escalation exists to **get a human-driven agent in the
loop and tell them what access they need, who has it, and how to get it**.
Closed enums (`operation_type`, etc.) are not buying their context cost.

| Option                              | Schema                                                                                                                                                                                                                                                                                                                                                                                                                                                | Cost                                                       |
| ----------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------- |
| **4R-A** three free-text fields     | Replace `operation_type` / `operation_spec` / `credential_requirements` / `execution_script` / `expected_outputs` with three required free-text fields: `access_needed`, `who_has_it`, `how_to_get_it`. Server validator: all three non-empty, each ≥ N chars. Detail view renders the three as the primary content. `result_status` and `requesting_agent` stay as-is. Migration: keep old fields read-only on legacy beads, write new fields on creates. | ~1 day in `gb escalation create`, server validator, type_escalation partial. |
| **4R-B** A + free-form `notes`      | Same three fields, plus an optional `notes` free-text field for everything else (links, traceback, replay payload). Keeps the schema small; gives a place for context that doesn't fit the access framing.                                                                                                                                                                                                                                            | same as A; trivial extra field.                            |
| **4R-C** A but unvalidated          | Same three fields, no server-side validator (CLI-only hint). Avoids breaking legacy callers; risks weak fills.                                                                                                                                                                                                                                                                                                                                          | ~half day.                                                 |
| **4R-D** keep current; render diff. | Don't change the schema; only change how the detail view renders it: extract any phrasing that looks like access/permissions and surface it at the top, push enum + script down. Cheap but doesn't fix CLI/agent ergonomics.                                                                                                                                                                                                                          | ~hours.                                                    |

**Recommend: 4R-B.** Three required access-centric fields are exactly the
operator's framing; an optional `notes` field absorbs the genuinely free-form
context (replay payloads, traceback links, screenshots). Validator is the
existing pattern from `decision_validation.go`. CLI prompts agents through
the three fields, which is how we get better escalation hygiene.

---

## Operator feedback round 4 (2026-05-05)

> *"one big question is which way or ways to organize the inbox at a top
> level.. by epic? Agent? I had a thought that making 'bundles' at a high
> level would be useful, with beads and grandbeads enumerated by category
> which isn't something we have defined today: intake beads (represent
> artifacts in the real world MRs, Jira tickets, slack mentions), and also
> epics (though may be parented by an intermediate bead epic and viewed
> that way — epics are the only type that is both intermediate and intake;
> intake if no epic/intake parent, intermediate if epic/intake parent),
> intermediate beads which represent pure internal work in progress like
> epics tasks and bugs, with which agents can be very free, and action
> beads which require human intervention like decisions and escalations…
> if we could organize and inbox around this principle we might get great
> power"*

This reframes the design epic ka-0000004660: the inbox isn't just a ranked
feed of action items, it's a *category-aware* view across the whole bead
graph. The operator is proposing a taxonomy we don't have today.

### Proposed taxonomy

| Bundle           | Members                                                                                            | Operator stance toward agents                                                |
| ---------------- | -------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------- |
| **intake**       | beads representing real-world artifacts: MRs, Jira tickets, Slack mentions. Epics *with no epic/intake parent*. | Read-mostly mirror of upstream truth; agents triage but don't author.        |
| **intermediate** | tasks, bugs, epics *with an epic/intake parent*. Pure internal work-in-progress.                   | "Agents can be very free" — agents own this layer.                            |
| **action**       | decisions, escalations.                                                                            | Cannot be drained without a human; this is what blocks the operator inbox.   |

Notes:

- **Epic is the only type that crosses bundles.** A top-level epic with no
  parent is `intake` (it represents a chunk of work that came in from
  outside); the same epic, once nested under another epic, becomes
  `intermediate`. The category is a function of `(type, parent.category)`,
  not a static field on the bead.
- The current data model already separates these *informally*: `source:jira`
  / `source:gitlab` labels mark intake beads; decisions and escalations are
  their own types; tasks/bugs/epics are everything else. The proposal is to
  formalize the categorization so it's queryable, navigable, and rendered.
- This is a question that lives *above* the inbox: agreed taxonomy benefits
  beads-viewer broadly (browse by category, filter `kd list` by category,
  scope advice by category, etc.), not just the inbox page.

### Implications for the design epic

If a category bundle becomes the inbox's top-level grouping (D6 below), the
seven research tasks under ka-0000004660 gain a sibling: **what *is* a
category, formally?** That feeds D7 below. The other seven tasks
(predicate, urgency formula, per-type rendering, action surface, decisions
interaction, on-call routing, performance) are unchanged but now scoped to
*the action bundle* rather than the whole inbox.

| # | Decision                                                                                                  | Status |
| - | --------------------------------------------------------------------------------------------------------- | ------ |
| 6 | New: top-level grouping for `/operator-inbox`. See D6 below.                                              | open   |
| 7 | New (depends on 6C / 6E): formalize the bundle taxonomy. See D7 below.                                    | open   |
| 4 (revised) | Pending.                                                                                        | open   |
| 5 (orig)    | Pending (likely revisable per round 3 note).                                                    | open   |

### D6 — Top-level grouping for `/operator-inbox`

The operator's three named candidates are *epic*, *agent*, *bundles*. Add a
flat ranked list (the literal output of D3-r2's urgency score, no grouping)
and a few hybrid framings so the fork is honest.

| Option                                            | What the inbox looks like                                                                                                                                                                                                                                                                                                                                                                                                                            | Cost                                                              |
| ------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------- |
| **6A** group by epic                              | Top level lists open epics with action-bead counts; each expands to its action items, ranked. "No epic" bucket at the bottom for orphans. Familiar from project trackers.                                                                                                                                                                                                                                                                       | medium; needs tree walk per epic.                                  |
| **6B** group by agent (spawner / owner)           | Top level lists active agents (or recently-stopped) with their action items underneath. "No agent" bucket for human-filed items. Highlights *whose* work is blocked.                                                                                                                                                                                                                                                                              | low–medium; agent identity already tracked.                       |
| **6C** group by category bundle (operator's idea) | Top level shows **Action**, **Intake**, **Intermediate** as three sections (probably in that visual order). Inside Action: ranked list of escalations + decisions + bugs flagged for operator. Inside Intake: open MRs, Jira tickets, Slack mentions, top-level epics. Inside Intermediate: in-flight tasks/bugs/sub-epics — collapsed by default since "agents can be very free" here. Single page; one mental model.                                                              | medium; depends on D7 (taxonomy formalization).                   |
| **6D** flat ranked list                           | No grouping. The composite-urgency feed from D3-r2, rendered top to bottom. Simplest possible inbox; the operator scrolls top to bottom and acts.                                                                                                                                                                                                                                                                                                | low; the cheap version of the design epic.                         |
| **6E** combo: bundle primary, epic secondary      | Like 6C, but inside the Action section, items are sub-grouped by epic (or "no epic") so cluster context survives. Bundles still the primary axis; epic groupings nest one level down.                                                                                                                                                                                                                                                            | medium-high; 6C + 6A's tree walk.                                  |
| **6F** user-toggleable view                       | Tabs at top: *Bundles / By epic / By agent / Flat*. Operator picks. Defers the choice; risks all four being mediocre.                                                                                                                                                                                                                                                                                                                          | high (4× UI surface).                                             |
| **6G** action-only inbox; bundles below the fold  | `/operator-inbox` shows ONLY the action bundle (escalations + decisions + flagged bugs), ranked. Intake and intermediate live on adjacent pages (`/intake`, `/wip`). Inbox stays uncompromisingly focused on "what blocks me."                                                                                                                                                                                                                  | low; close to the design-epic strawman.                           |
| **6H** none — try yet another framing             | Send the operator more options.                                                                                                                                                                                                                                                                                                                                                                                                                  | n/a                                                               |

**Recommend: 6C.** It is the cleanest expression of the operator's
"bundles" framing: three sections, action first, one page. It composes
cleanly with the design epic — the urgency-ranked feed becomes the
**Action** section's contents — and it gives intake and intermediate a
natural home so they're not invisible. 6E is a reasonable second choice
if cluster-by-epic context inside the action section turns out to matter
during prototyping; we can promote 6C to 6E without rewriting the page.
6G is the conservative fallback if D7 (taxonomy) stalls.

### D7 — Formalize the bundle taxonomy (predicated on 6C / 6E)

If categories become a first-class operator concept, beads-viewer (and
`kd list`) need a way to ask "is this bead intake/intermediate/action?"
without re-deriving from `(type, parent)` at every callsite.

| Option                                  | How `category` is computed and exposed                                                                                                                                                                                                                                                                                                                                                                                                                                                  | Cost / risk                                                                                                       |
| --------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| **7A** server-derived field on read     | Server computes `category ∈ {intake, intermediate, action}` from `(type, parent.type, source:label, mentions)`. Exposed in bead JSON, `kd list --category=…`, list filter on `/operator-inbox`. Recomputed on read, so a re-parented epic shifts category automatically.                                                                                                                                                                                                              | ~1 day; one rule table; needs care for re-parenting cascades.                                                     |
| **7B** client-only classification       | Beads-viewer JS knows the rules; classifies after fetch. No schema or API change. Fastest to ship.                                                                                                                                                                                                                                                                                                                                                                                       | ~half day; CLI / mail / Slack still can't filter by category, so the abstraction stays viewer-only.                |
| **7C** auto-applied label               | Server stamps `category:intake` / `category:intermediate` / `category:action` on every bead at create-time and on parent change. Reuses existing label query infrastructure (`-l category:action`).                                                                                                                                                                                                                                                                                       | ~1 day; risky on re-parenting (must rewrite labels on tree edits); labels become eventually-consistent with truth. |
| **7D** explicit column with override    | Migration adds `category` column to beads, populated by the rule table at insert + update. Optional manual override (`kd update --category=…`) for edge cases the rule table gets wrong.                                                                                                                                                                                                                                                                                                | ~1.5 days; schema migration; override is a foot-gun.                                                              |
| **7E** don't formalize                  | Hard-code which types live in which bundle inside beads-viewer. Type+parent rules live in one Go file. No new field, label, or column. Cheapest; works for 6C as long as the inbox is the only consumer.                                                                                                                                                                                                                                                                                | ~hours; locks the taxonomy out of CLI / API forever; re-introduces the cost if a second consumer wants it.        |
| **7F** none — drop categories           | If 6C / 6E are not picked, this question is moot. Stay with type+parent everywhere.                                                                                                                                                                                                                                                                                                                                                                                                       | 0.                                                                                                                |

**Recommend: 7A.** Compute-on-read is the most resilient to bead-graph
edits (no rewrite-on-reparent), and exposing `category` as a field plus a
query parameter unlocks `kd list -f category=action`, advice scoping
(`category:action` advice for decision/escalation hygiene), and any future
bridge or CLI that wants the same lens. 7E is acceptable if the operator
wants to keep this inbox-only; 7C / 7D introduce sync hazards we don't
need.

### Open question that 6C / D7 expose

The taxonomy is *almost* well-defined by `(type, parent)`, but a few
edges need a ruling before D7 lands:

- **Tasks under no epic.** Are they `intermediate` (default) or do they
  promote to `intake` when they have a `source:` label (e.g. an
  agent-filed bug from a Jira ticket)?
- **Decisions / escalations under an intermediate parent.** Stay `action`
  regardless of where they live in the tree, presumably — but worth
  declaring rather than implying.
- **Bugs flagged "operator-blocking".** The design epic mentions bugs as
  candidates for the action bundle; the rule table needs an explicit
  predicate for "bug ∈ action vs bug ∈ intermediate" (priority? a label?
  a flag set by the agent?).

These are sub-decisions for D7's rule-table draft, not separate
checkpoints; flagging here so the implementer doesn't re-discover them.

---

## Round 5 — D6 + D7 resolutions (operator)

| Decision  | Bead             | Outcome                                                                                                       |
| --------- | ---------------- | ------------------------------------------------------------------------------------------------------------- |
| **D6**    | ka-0000000079    | **6C**: top-level grouping is *Action / Intake / Intermediate*, action first.                                 |
| **D7**    | ka-0000001123    | **7A**: server computes `category` from `(type, parent)` on read; exposed in JSON + `kd list --category=…`.   |

D6=6C confirms the operator's bundles framing as the inbox's mental model.
D7=7A makes `category` a first-class read-time attribute of every bead, so
the same lens works for `/operator-inbox`, `kd list`, advice scoping, and
any future bridge or surface — not just the inbox.

### D7 plan — server-derived `category` field

This is the plan artifact submitted on ka-0000001123.

#### 1. Domain

```
category ∈ {intake, intermediate, action, config}
```

The category is a property of every bead, derived purely from its
`(type, parent.type, labels)` tuple. It is computed on read; it is never
persisted, written, or stamped as a label.

The `config` category was added per operator round-5 follow-up
(Slack/bead ka-0000006247): "another category is *config* which should
be same as bead type". Read as: configuration-shaped beads form their
own bundle, and membership is decided by type alone — no parent walk
needed. This keeps the inbox cleaner (config doesn't masquerade as
in-flight intermediate work) and lets operators browse `/config` as
its own surface when they want to reason about how the system is
parameterized rather than what it is doing.

#### 2. Rule table

The classifier evaluates rules top-to-bottom; the first match wins.

| # | Predicate                                                                                                       | Category       |
| - | --------------------------------------------------------------------------------------------------------------- | -------------- |
| 1 | `type ∈ {decision, escalation}`                                                                                 | `action`       |
| 2 | `type = bug` AND has label `flag:operator-blocking` (or priority ≤ 1 with no assignee for ≥ 24h — TBD)          | `action`       |
| 3 | `type = epic` AND `parent ∈ {epic, source-tagged bead}`                                                         | `intermediate` |
| 4 | `type = epic` AND no parent (or parent is `intake`-classified)                                                  | `intake`       |
| 5 | Has any `source:*` label (e.g. `source:jira`, `source:gitlab`, `source:slack`) AND no parent classified `intermediate` | `intake`       |
| 6 | `type ∈ {task, bug}` AND parent is `intermediate`-classified (epic, sub-epic)                                   | `intermediate` |
| 7 | `type ∈ {task, bug}` AND no parent                                                                              | `intermediate` (default; promotes to `intake` only via rule 5) |
| 8 | `type ∈ {config, account, project, schedule, advice, formula, agent}`                                            | `config`       |
| 9 | (fallback)                                                                                                      | `intermediate` |

Rule 8 is the operator's "config" bundle. The qualifying types are the
ones whose purpose is to *configure* the system (credentials, projects,
schedules, advice rules, formulas, agent definitions) rather than to
represent work in flight. A future `type=config` slots in trivially
(top of rule 8). Membership is type-only; no parent walk required —
which is what the operator's "should be same as bead type" meant.

Edge cases the operator's open-question table flagged are now resolved
by this table:

- **Tasks under no epic** → `intermediate` by default; promote to
  `intake` only when they carry a `source:*` label (rule 5).
- **Decisions / escalations under an intermediate parent** → stay
  `action` (rule 1 fires before any parent-based rule).
- **Operator-blocking bugs** → `action` only when explicitly flagged
  (`flag:operator-blocking`). The rationale for an explicit flag rather
  than priority-only: priority rolls reflect engineering urgency; a
  separate flag keeps the inbox under the operator's control. The
  flagging mechanism is an implementation detail of the rule table; the
  /operator-inbox design epic owns the UX for raising and clearing it
  (subtask 4: action surface).

#### 3. API surface

- **JSON read paths.** Every bead JSON response includes a top-level
  `category` field. Sites: `GET /api/v1/beads/{id}`, `GET /api/v1/beads`,
  `GET /api/v1/decisions`, `GET /api/v1/escalations`, the SSE bead
  stream payload, and the gRPC equivalents in `kbeads/internal/server/`.
- **List filter.** `kd list --category=action` (and `=intake`,
  `=intermediate`). The HTTP server accepts `?category=…` on the list
  endpoint. Multiple values comma-separated (`--category=action,intake`).
- **Negation.** `--category=^intermediate` (or `--no-category=…`) to
  exclude — useful for "show me anything that *isn't* intermediate"
  inbox queries.
- **Beads-viewer.** New `category` chip on bead detail; new top-level
  category filter on `/list`; `/operator-inbox` (design epic
  ka-0000004660) uses `category=action` as its primary predicate for
  the Action section, `category=intake` for Intake, `category=intermediate`
  for Intermediate. The `config` category lives off the inbox: a
  separate `/config` browse page (or a collapsed fourth section if
  the operator decides config belongs on the inbox after all — open
  question for the design epic).

#### 4. Implementation outline

- New file `kbeads/internal/server/category.go` exposes
  `func DeriveCategory(b *model.Bead, parent *model.Bead) Category`.
  Pure function; no DB calls. The caller passes a hydrated parent (or
  `nil` for parentless beads).
- The list query in `kbeads/internal/server/http_beads.go` already
  hydrates parent type for sort/group purposes (verify; if not, add a
  one-shot LEFT JOIN to fetch `parent.type` and parent labels needed
  by rule 5). Compute `category` per row and stamp on the response
  struct before serialization.
- Singleton reads (`GET /api/v1/beads/{id}`) take one extra parent
  fetch; tolerable. Bulk reads must not become N+1: the join above
  is the only viable shape.
- For SSE/NATS payloads, the publisher already reads the bead before
  emitting; piggyback the parent fetch and stamp `category` on the
  emitted event.

#### 5. Performance / index story

- No new column. No migration. No write-path changes.
- The list query gains one column in the projection (`parent.type`,
  `parent.labels`) — the join already exists for the type/parent filter
  on most callsites.
- Index check: confirm `(parent_id)` is indexed on `beads` (it should
  be; this is the FK). No new indexes required for the rule table.
- Sanity benchmark: `kd list --category=action` over the current
  ~22 open action-bead queue should be O(N) on the queue size, dominated
  by the existing query's where-clause cost.

#### 6. Re-parenting & re-classification

- Because category is derived on read, a re-parent of an epic
  (e.g. `kd update ka-X --parent ka-Y`) automatically flips the category
  on the next read. No background job, no label rewrite, no row update.
- The single test for this: re-parent an epic from "no parent" to
  "under another epic" and confirm category transitions from
  `intake` → `intermediate` on the next `GET`.

#### 7. Testing

- Unit tests on `DeriveCategory` — one per row of the rule table, plus
  a parentless-vs-parented matrix for tasks, bugs, and epics.
- Integration: smoke `kd list --category=action` against a seeded test
  DB; assert the expected bead IDs surface and excluded ones do not.
- Regression: existing list/detail tests gain an assertion that the
  `category` field is present and matches the expected value for a few
  fixture beads (one per category).

#### 8. Out of scope (for D7's plan)

- Manual override (`--category=…` on `kd update`). Not in 7A — the
  field is derived. If a category mistake survives the rule table, the
  fix is to refine the rule table, not to override per-bead. (7D
  proposed override and was rejected.)
- The `flag:operator-blocking` UX (where the operator sets/clears the
  flag, where it shows up on the bug bead). Owned by the
  /operator-inbox design epic, subtask 4.
- The operator-inbox page itself — D6=6C settles its top-level shape;
  the seven existing subtasks under ka-0000004660 (predicate, urgency
  formula, per-type rendering, action surface, /decisions interaction,
  on-call routing, performance) ship the page, and an 8th subtask
  (category derivation rule table → this plan) ships the classifier.

#### 9. Implications for the design epic (ka-0000004660)

The 7 existing subtasks remain unchanged in scope but are now scoped
to the **Action** section of the inbox. An 8th subtask is added:

> **8. Category derivation rule table.** Implement the rule table
> from D7's plan as `DeriveCategory(b, parent)`. Wire into list/detail
> JSON responses, the SSE/NATS payload publisher, and the `kd list
> --category=…` filter. Owns the `flag:operator-blocking` flag's
> classifier semantics; UX for raising the flag stays in subtask 4.

D6=6C also implies two layout details for subtasks 3 (per-type
rendering) and 5 (interaction with /decisions):

- The page presents three sections in the order *Action → Intake →
  Intermediate*, with the Action section open by default and the
  others collapsible.
- `/decisions` becomes a typed view (filter `category=action AND
  type=decision`) on top of the same data; the standalone page is kept
  as a deep link but is no longer the primary surface.

### Status of the open queue

| #           | Decision              | Status                                                                                                |
| ----------- | --------------------- | ----------------------------------------------------------------------------------------------------- |
| D1 (1C)     | ka-0000006950         | resolved (round 1)                                                                                    |
| D2 (2C)     | ka-0000004001         | resolved (round 1)                                                                                    |
| D3-r2 (B)   | ka-0000001363         | resolved (round 3) → design epic ka-0000004660                                                        |
| D4-revised  | ka-0000003919         | closed without a clear pick — operator's "throw new ideas at me" applied; treat as deferred           |
| D5          | ka-0000001330         | open, but **weakened** by D3-r2 (stale items already sink in ranking); operator may withdraw / revise |
| **D6 (6C)** | **ka-0000000079**     | **resolved (round 5)**                                                                                |
| **D7 (7A)** | **ka-0000001123**     | **resolved (round 5); plan artifact = this section**                                                  |

---

## Round 7 — operator follow-up: prove the bundles taxonomy via the constellation view (2026-05-05)

> Slack: *"an easy way to proof this concept would be to make the
> default beads constellation view group things by category
> (configurable by user) in lanes in the image. We could explore
> making the constellation significantly more interactive, maybe even
> using its primitives to make a primary interaction surface, with
> category as one of the primary axes of organization. Research
> epic?"*

The taxonomy from D6/D7 (action / intake / intermediate / config) is
currently a grouping primitive proposed for a single page
(`/operator-inbox`). The operator wants to test it earlier, on a
surface humans already browse, by *spatializing* the categories: the
current bead-show constellation already renders similar /
forward-deps / reverse-deps in fixed regions of a 1000×500 viewBox
(see `gardenio/controller/cmd/beads-viewer/constellation.go` lines
1–60). Repurpose that primitive: replace the "similar / forward /
reverse" partition with a *category-lane* layout, where each lane is
one category, and let the user toggle the active grouping axis.

Why this proves the concept cheaply:

- The constellation already exists, is keyboard-navigable, hydrates
  bead summaries on hover, and is rendered as plain SVG (no separate
  3D engine). A lane re-layout is a self-contained change to one Go
  template + view-model.
- The graph is the natural place for a taxonomy to *fail visibly*:
  if category membership is wrong, the eye sees a bead in the wrong
  lane immediately.
- It exercises `DeriveCategory` (D7 / ka-0000003787) on real data
  before we wire the inbox page, so taxonomy bugs surface against
  human intuition rather than against the inbox spec.
- It opens a path toward Matt's broader hypothesis — that the
  constellation can be a *primary* interaction surface — without
  committing to that scope yet. We get a falsifier first; promotion
  to primary surface is a follow-up decision.

### What a research epic should cover

Locked to research/design — no changes to the constellation
template, server view-model, or routing until the operator
green-lights a specific design. Subtasks should each land a section
in a new `gardenio/docs/research/constellation-categories-design.md`.

1. **Layout primitive — what does "lanes by category" mean on the
   1000×500 viewBox?** Stacked horizontal bands (action top → config
   bottom)? Vertical columns? Concentric rings around the center
   bead? The current layout uses semicircles for similar +
   half-planes for forward/reverse; lanes are a different metaphor
   and need to handle the empty-category and overflow cases. Sketch
   2–3 layouts with the current 22-bead operator queue + a typical
   busy week as test inputs.

2. **What does the center bead mean in lane mode?** Today the center
   is the bead being shown; everything else is *related to* it via
   similarity / dep edges. In lane mode the natural views diverge:
   *bead-centric* (lanes show the categories of related beads) vs.
   *project-centric* (no center; all beads in scope laid out by
   category). Pick the default and the toggle.

3. **Configurability — what axes does the user toggle?** Category
   (D6/D7) is the proposed primary axis. Plausible secondary axes:
   priority, age, assignee, agent_state, status. How is the toggle
   surfaced (URL param? cmdk? on-page selector?), and which axes are
   implemented in v1 vs. left as future work?

4. **Edge semantics in lane mode.** Today edges are
   similarity-weighted strokes between center and ring nodes. In
   lane mode, do edges still encode dependency, or do they encode
   *cross-lane traversal* (i.e. "this intake bead has spawned this
   intermediate work")? Cross-lane edges are the visual story for
   how work flows through the taxonomy — worth foregrounding if we
   can keep the SVG legible.

5. **Interactivity bar — what makes the constellation a primary
   interaction surface?** Drilling into a bead inline (peek panel)
   without navigating away; raising/clearing the
   `flag:operator-blocking` flag; reparenting a task into an epic;
   resolving a decision; commenting. Identify the smallest set that
   would let an operator drive a triage session entirely from the
   constellation, and which of those need new server endpoints vs.
   reuse existing ones.

6. **Performance / data shape.** Current constellation hydrates a
   bounded set per page (≤8 similar, ≤6 forward, ≤6 reverse). A
   project-wide lane view needs to handle the realistic upper bound
   (hundreds of beads) without N+1 hydrate calls. Probe whether the
   existing list-API + category field is sufficient or whether we
   need a constellation-specific endpoint that returns lane-bucketed
   summaries.

7. **Promotion criteria — when is the constellation "the" primary
   surface?** Define the bar for graduating from "alternate view
   behind a feature flag" to "default for /operator-inbox" or even
   "default for `/`". This protects against scope creep: we want a
   proof, not a v1 product.

### Implication for the design epic (ka-0000004660)

This work is a **parallel** research epic, not a child. The
`/operator-inbox` page (ka-0000004660) and the constellation
proof-of-concept share `DeriveCategory` (D7 plan, ka-0000003787)
but diverge on layout, interaction model, and audience (inbox =
single-page list; constellation = graph). If the constellation
proof exposes a taxonomy bug, both consumers benefit; if the proof
shows lanes work better than a sectioned list, that's a future
*D8-shaped* fork between the two surfaces — out of scope for this
round.

### Implication for `beads3d/`

`beads3d/` is deprecated (see repo CLAUDE.md). The proof goes in
the live `beads-viewer` constellation, not `beads3d`. If lanes
need richer interactivity than SVG can support, that's a follow-up
decision; we should not resurrect `beads3d` as a default surface
without an explicit operator pick.

---

## Round 8 — operator pivot: a new constellation view IS the inbox (2026-05-05)

> Slack: *"let's make a new constellation view (with components common
> to the current one which should remain unchanged for now) that can be
> primary for an inbox. Intake beads operated by the current user is
> the primary index into the inbox; clicking on one produces the
> constellation that it's a part of"*

This is a directional pivot from round 7. Round 7 framed the work as
*research only* — repurpose the existing constellation in place to
prove the bundles taxonomy. Round 8 is a build directive with a
concrete shape:

- **A new view** sits alongside the existing bead-show constellation.
  The existing one is untouched ("which should remain unchanged for
  now"). Shared primitives (SVG view-model, age opacity, status
  fills, edge styling, hydrate fan-out caps) are reused, not
  modified.
- **The view IS the inbox.** It supersedes the list-page
  `/operator-inbox` shape from ka-0000004660 as the primary surface.
  The list page may still exist as a fallback/typed view, but the
  default operator landing for human-blocking work is the new
  constellation surface.
- **The primary index is "intake beads operated by the current
  user".** Not all beads in scope, not just escalations — *intake
  beads* (per D7 taxonomy: external-source artifacts + epics that
  are not parented by another epic/intake) that the current operator
  has some operational claim on. This is a left-rail / sidebar /
  cmdk-style list, not the constellation itself.
- **Click an intake row → the constellation it is part of.** The
  bead's enclosing dependency graph is what gets rendered. This
  makes "the constellation" the unit of work, not "the bead". A
  single epic's worth of work, including its action / intermediate
  / config descendants, becomes one navigable picture.

### Why this is the right shape

- It collapses two earlier surfaces (the list-page operator-inbox and
  the lane-mode constellation) into one. The intake list answers
  *"what work is mine?"* and the constellation answers *"what's
  inside this work?"* Both questions are first-class for an operator
  who is the human end of N agent loops.
- It honors the bundles taxonomy without demanding the user
  conceptualize it. The index is intake-only; the per-intake
  constellation naturally exposes the rest of the categories
  (action, intermediate, config) as the descendants of that intake.
  Category becomes a *coloring / lane-grouping* axis inside the
  graph, not a flat top-level partition the operator must decide
  between.
- It is cheap to prove. The intake-by-user query is one new server
  endpoint; "the constellation a bead is part of" is one new
  view-model derivation off the existing primitives.
- It preserves the existing constellation view. No regression risk
  for the bead-show route; this is purely additive.

### Concrete shape (subject to design epic)

```
/inbox                                              (new route)

┌──────────────────────────┬───────────────────────────────────────────┐
│ Intake beads (yours)     │  Constellation of: <selected intake>      │
│                          │                                           │
│ ▸ kd-… "MR review"       │     ●(intake, center)                     │
│   kd-… "epic: ingest"    │      |    \                               │
│ ▸ kd-… "Slack: bridge…"  │      ●     ●  (action: decision)          │
│   kd-… "Jira CFC-123"    │     / \   /                               │
│   kd-… "epic: cleanup"   │    ●   ● ●  (intermediate: tasks)         │
│                          │           \                               │
│ [filter: mine ▾]         │            ● (config: schedule)           │
│ [sort: priority ▾]       │                                           │
└──────────────────────────┴───────────────────────────────────────────┘
```

Default selection = top of the index. Keyboard navigates the index;
clicking or hitting enter swaps the right pane. The right pane *is*
a constellation (reused primitive), not a list.

### Open questions / inline TBDs for the design epic

These do not need separate decisions yet; they are forks the design
epic should resolve in its doc draft. The one exception — the
predicate for "operated by the current user" — is filed as **D8**
below because every other artifact depends on it.

- **What is "the constellation a bead is part of"?** The connected
  component containing it in the dep graph, walked to the topmost
  intake/epic ancestor and then expanded N levels down? The
  enclosing epic's full subtree, regardless of how the bead got
  there? Capped at some node count for legibility?
- **Where does the existing bead-show constellation live now?** It
  remains the default view of `/bead/<id>`. The new `/inbox`
  constellation may *also* be reachable from a bead's page (button:
  "open inbox view rooted here"). They are sibling renderings, not
  one replacing the other.
- **Index ordering.** Priority? Composite urgency from D3-r2? Last
  activity? User-pinnable? v1 ships with one default and a single
  toggle.
- **Action beads not under an intake.** A decision/escalation that is
  not parented under any intake (rare but possible) cannot appear by
  drilling into an intake. Either: (a) treat them as their own
  pseudo-intake row in the index, (b) require they always have an
  intake ancestor (taxonomy fix), or (c) surface them via a banner /
  separate list. Likely (a).
- **Multi-user / on-call.** Predicate D8 picks a default. The index
  filter chip lets the operator switch between "mine", "team",
  "on-call". Not v1 unless cheap.
- **What happens to the round-7 research epic ka-0000004309?** Its
  subtasks 1, 4, 6, 7 (lane layout, edge semantics in lane mode,
  perf, promotion criteria) feed directly into this design epic.
  Subtasks 2, 3, 5 (center-bead semantics, configurable axes,
  interactivity bar) become *features of the new view*, not research
  questions about the existing one. We close ka-0000004309 in favor
  of the new design epic and pull its research findings forward.

### D8 — predicate for "intake beads operated by the current user"

The index is "intake beads operated by the current user". *Operated
by* needs a definition; each option produces a different inbox.

| Option | Predicate | Inbox feel | Artifact |
| --- | --- | --- | --- |
| **8A** raised-by | `intake.created_by == user`                                               | "what I filed"                   | plan |
| **8B** assignee  | `intake.assignee == user`                                                 | "what I'm responsible for" (canonical work-tracker semantics) | plan |
| **8C** subscribed | `user ∈ intake.watchers ∪ intake.assignee ∪ intake.created_by`           | "what I want to know about"      | plan |
| **8D** descendant-owner | `∃ descendant of intake where descendant.assignee == user OR descendant has open decision routed to user` | "where the work that needs me lives" | plan |
| **8E** composite (recommended) | `8B OR 8D` (assignee of intake OR descendant-owner) — covers both top-down assignment and bottom-up bubbling without false positives from raise-only / passive-watch | "where I am the human end of work, however I got there" | plan |
| **8F** raise-and-forget | `8A AND not 8B` — explicitly *only* intake the user filed, used as a separate filter chip, not the default | "stuff I dropped over the wall" | plan |

Recommend **8E**. Rationale:

- 8B alone misses the common case where the user is on-call for a
  decision under an intake assigned to a bot/gardener. 8D catches
  that.
- 8C is too noisy — watchers tend to accumulate over a bead's life
  and dilute the inbox.
- 8A on its own collides with how operators delegate: filing a Jira
  ticket and handing it to an agent should remove it from your
  inbox; 8A would keep it forever.
- The composite stays cheap: both halves are existing indexed
  fields. 8D requires a one-step descendant walk — bounded because
  the intake's subtree is what we render anyway.

### Implications for the existing artifacts

- **ka-0000004660 (operator-inbox design epic)** — *superseded* by
  the new constellation-primary view. Its 8 subtasks (predicate,
  composite urgency formula, per-type rendering, action surface,
  interaction with /decisions, on-call routing, perf/index,
  DeriveCategory implementation) split: the urgency formula and
  per-type rendering live on inside the constellation as
  *secondary-axis* sorts and node decorators; the rest move to the
  new epic or close as out-of-scope. Recommend closing
  ka-0000004660 after the new epic is filed and the moved work is
  re-parented.
- **ka-0000004309 (round-7 research epic on category-organized
  constellation)** — *absorbed* by the new epic. Its findings on
  layout / edge semantics / perf are inputs to the new view's
  design doc. Recommend closing.
- **ka-0000003787 (D7 plan: `DeriveCategory`)** — *unchanged*.
  Category is still computed server-side on read; the new view
  consumes it for node coloring / lane grouping inside the per-
  intake constellation, and the index uses it implicitly (intake-
  filter = `category=intake`).
- **D5 / ka-0000001330 (auto-close stale escalations)** — *further
  weakened*. Stale escalations under an intake are visually
  obvious in the constellation (low-opacity nodes via the existing
  age fade). Auto-close is now redundant on top of *both* D3-r2
  ranking and the constellation's age-opacity. Recommend the
  operator withdraw D5.
- **D4-revised / ka-0000003919 (escalation schema)** — *unchanged
  but revisitable*. Three free-text fields still beat enums; the
  constellation surfaces escalations as nodes with the same hover
  preview as any other bead, so schema simplicity matters more for
  authoring than for surfacing.

---

## Pointers

- Bead types: `kbeads/internal/model/bead.go`
- Schema: `kbeads/internal/server/config.go`
- Escalation HTTP: `kbeads/internal/server/http_escalations.go`
- Bridge API: `gardenio/controller/internal/bridge/api_escalations.go`
- Bridge SPA: `gardenio/controller/internal/bridge/web/escalations.html`
- Slack: `gardenio/controller/internal/bridge/bot_escalations.go`
- Decisions list (mirror to copy): `gardenio/controller/cmd/beads-viewer/decisions.go`
- Decisions partial (mirror to copy): `gardenio/controller/cmd/beads-viewer/templates/partials/type_decision.html`
- Constellation primitive (reused, not modified): `gardenio/controller/cmd/beads-viewer/constellation.go` (1100 lines; layout, age-opacity, hydrate, edge styling)
- Constellation tests (regression bound): `gardenio/controller/cmd/beads-viewer/constellation_test.go`

## Round 9 — operator pushback on D8: unpack "operated by" properly (2026-05-05)

### Operator response

D8 (`ka-0000006258`) closed by the operator with the rationale
**"Wha about operated by semantics?"** and the round-8 session was
rated 1★. The pushback is not about which option to pick — it is
about the *shape* of the question. Round 8 listed jira-style
predicates (`assignee`, `created_by`, `watchers`, `descendant
owner`) as if "operated by" were an unknown to be mapped onto a
canonical work-tracker field. It is not. **`operated_by` is
already a first-class concept in this system, with a fully
populated column and a bridge contract.** The right question is
about identity *resolution*, not field selection.

### What the system already knows

Reviewed the schema and the bridge code:

| Source                       | Where intake bead's `operated_by` comes from                                               |
| ---------------------------- | ------------------------------------------------------------------------------------------ |
| Slack thread mention         | `slack:U…` of the user who @-mentioned the bot (`bridge/bot_commands.go:530`)             |
| GitLab MR mention            | `gitlab:<username>` of the MR author (`bridge/gitlab_mentions.go:202`)                    |
| Jira ticket sync             | `jira:<accountId>` of the assignee/reporter (`bridge/jira_mentions.go:216`)                |
| Direct `kd create` from CLI  | `oidc:<email>` from the operator's auth token (`server/beads.go:91`, `audit.go:178`)       |
| Scheduler-driven (autonomous)| `system:scheduler` sentinel                                                                |
| Fleet-token writes           | `fleet:<name>` sentinel                                                                    |

Migration `012_human_attribution.up.sql` adds the column to every
relevant table (`beads`, `comments`, `deps`, `events`,
`mutation_history`) with partial indexes. `/active` already filters
by `?operated_by=` and defaults the value from the viewer's OIDC
email. There is also a `user_identity` bead type
(`gb identity add`) that links one human's `slack_id` ↔
`gitlab_username` ↔ `github_username` ↔ `email` ↔ `jira_user`.

So when the operator says "intake beads operated by the current
user is the primary index", they are describing a query the system
*almost* supports today: `category=intake AND operated_by=<viewer>`.
The semantic gap is **identity resolution across bridges**, not
predicate selection.

### What round 8 got wrong

Round 8's D8 framing assumed "operated by" was a phrase to be
disambiguated against canonical work-tracker fields. That treated
the operator like a PM choosing between Jira-style pivots
(`Reporter` vs `Assignee` vs `Watcher`). In reality, the operator
was naming a property the system already records — and asking how
"current user" should be defined when that user has multiple
identities (`slack:U…`, `oidc:matt@…`, `gitlab:matthewbaker`,
`jira:abc123`) and the bridges stamp different ones depending on
which platform created the intake.

### D8-r2 — identity resolution for "operated by current user"

Each option produces a meaningfully different inbox. None of them
re-litigate which schema field to use; the field is
`bead.operated_by`. They differ on how a human's identity is
expanded into the set of `operated_by` values that count as "me".

| ID    | Predicate                                                                                                                                                     | Surface coverage                                                              | Cost                                                                                                              |
| ----- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| **8R-A** | `bead.operated_by == viewer.oidc_id`                                                                                                                      | Only intake the viewer raised via OIDC-authenticated CLI / `/active` browse  | Misses every Slack thread, GitLab MR, and Jira ticket they own — small inbox, high precision                       |
| **8R-B** | `bead.operated_by IN viewer.identity_ids` — expand viewer's `user_identity` bead into all linked ids; match any                                            | All four bridges, requires `gb identity add` to be populated                  | One join. False negatives if a user's `user_identity` bead is missing or stale                                     |
| **8R-C** | viewer appears in `events.operated_by` for the intake (any audit-trail mention, not just `bead.operated_by`)                                              | Includes intake the user has *touched* (commented, statused, decided) but did not raise | Larger query (audit table); inbox grows over time as users brush against more intake; "drove past" ≠ "owns"        |
| **8R-D** | 8R-B OR 8R-C — currently-stamped owner OR has touched in audit trail                                                                                      | Most permissive without crossing into "subscribed/watching"                  | Two indexed lookups; rationale: matches operator's mental model of "stuff I'm involved in"                          |
| **8R-E** | 8R-B + per-bridge fallback: when an intake's `operated_by` doesn't match any `user_identity` row, derive from the intake's `source:*` label and platform-specific field (e.g. `gitlab:` from MR author URL, `slack:` from thread author from labels) | All four bridges even when `user_identity` is unset/stale                    | Bridge-specific resolver per source. Mostly redundant if `user_identity` is healthy; useful as a backstop          |
| **8R-F** | New explicit `operator` field on intake beads (set at create, immutable), separate from per-mutation `operated_by`                                          | Author-stamped; survives operator-of-record drift; introduces ambiguity vs. existing column | Schema migration. Likely overengineered: `bead.operated_by` *already* freezes at create unless we update it later |

### Recommendation: 8R-B with 8R-E backstop deferred

- **Pick 8R-B** as the index predicate. It rests on infrastructure
  the system already has (`user_identity` beads, `bead.operated_by`
  column populated by every bridge) and produces an inbox that
  matches the operator's mental model: "intake I raised, regardless
  of which platform I raised it from."
- **Defer 8R-E**. Per-bridge fallback resolvers add real complexity
  and only matter when `user_identity` is missing — solvable by
  *requiring* a `user_identity` bead for any human who wants to use
  the inbox. A pre-flight check on the index page can warn unset
  users: "Add a user_identity bead at /identity to populate your
  inbox."
- **Reject 8R-A** as too narrow (Slack-thread intake invisible).
- **Reject 8R-C / 8R-D** as too broad — "I touched it once" doesn't
  mean "I'm driving it." Better as an additional filter chip on the
  same view ("show intake I've interacted with") than as the
  default.
- **Reject 8R-F** as duplicative of `bead.operated_by`.

### Implications for the design epic (`ka-0000006157`)

- Subtask 1 (index predicate) becomes: *implement
  `viewer.identity_ids` resolver from OIDC email → `user_identity`
  bead → set of operator ids; filter intake bead list with
  `category=intake AND operated_by IN identity_ids`.*
- Subtask 4 (index UI) gains a missing-identity banner: when the
  resolver returns only `oidc:<email>` (no linked identities), show
  a banner pointing at `gb identity add` so users opt in to the
  cross-platform expansion.
- Add a 9th subtask (or fold into subtask 1): exposing
  `--operated-by` filter on `kd list --type=intake` to mirror the
  HTTP one, so CLI users can reproduce the inbox query
  deterministically.
- The existing operator-chip on `/active` (`operator_chip.go`) is
  the right primitive to reuse for the inbox header — it already
  resolves `?operated_by=me` to `email` from OIDC and renders a
  filter chip. The new view should *delegate* to that primitive,
  not reinvent identity resolution.

### Why this re-framing matters beyond D8

`bead.operated_by` is a load-bearing column in this codebase, but
the *human-identity* layer on top of it is currently scattered:
`/active` resolves OIDC → email, the bridges stamp platform-prefixed
ids, and `user_identity` beads sit unused as a bridge between them.
Round 9 surfaces that identity resolution is the missing primitive,
not the inbox view. The new design epic should land that primitive
(call it `IdentityResolver`) as its first deliverable; every view
that wants "things owned by me" — `/active`, the new constellation
inbox, `kd list --operated-by=me`, the operator chip — consumes it.
This is a single lever that improves more than the inbox.


## Round 10 — operator resolution of D8-r2: pointer to Option B operators epic

Operator resolved D8-r2 (`ka-0000005011`) as **8R-B** with response:

> "We should have other work to unify identity at bead creation time;
> find and refer to it here"

The pointer is **Epic ka-0000003625 — Pivot operated_by to Option B
(operators FK + aliases) — unwind R2 Option A**. ADR
`docs/adr/human-attribution-identity-v2.md` already landed via MR
[!2103](https://gitlab.com/example-org/gardenio/-/merge_requests/2103),
superseding the R2 ADR (Option A) and R10 decision `ka-0000001224`.

### What Option B actually delivers

Two new tables under `kbeads`:

```
operators(id BIGSERIAL PK, kind, subject, display, email, …)
operator_aliases(operator_id, kind, subject, source, …)
```

`operated_by TEXT` becomes `operated_by_id BIGINT REFERENCES operators(id)`
on every audit-bearing table (beads, comments, deps, events,
mutation_history). The resolver in the controller and bridges writes
operator ids, not opaque text. Migration 015 introduces the schema, 016
walks existing rows and rewrites them, 017 drops the old TEXT column.

The unification mechanism is `operator_aliases`: one human, one
`operators` row, many alias rows linking that operator to every platform
identity (`slack:U0OPERATOR1`, `oidc:matt@…`, `gitlab:matthewbaker`,
`jira:abc123`). The "expand viewer's identity into all stamped values"
question that Round 9 framed as *application-layer JOIN against
`user_identity` beads* is, under Option B, **a single FK lookup**.

### Why this matters for the inbox design epic

The `IdentityResolver` primitive Round 9 named as the missing piece —
the thing `/active`, the new constellation inbox, `kd list
--operated-by=me`, and `operator_chip.go` should all delegate to —
*becomes the `operators` table*. There is no resolver to write at the
application layer; the schema does the work.

This re-shapes the design epic (`ka-0000006157`) materially:

- **Subtask 1 (index predicate)**: under Option B the predicate is
  `category=intake AND operated_by_id = :viewer_operator_id`, where
  `viewer_operator_id` is resolved from the OIDC subject through
  `operators` (one query, one join). The "expand `user_identity` bead
  into a set of ids" branch from Round 9 is no longer needed —
  `operator_aliases` *is* that expansion, applied at write time, not
  read time.
- **Subtask 4 (index UI)**: the missing-identity banner reframes from
  "add a `user_identity` bead" to "you have no `operators` row / your
  alias set is missing platform `<X>`." Onboarding is `gb identity
  add` → upserts an alias row.
- **Sequencing (new)**: the design epic's *interesting* implementation
  steps now block on Option B's substrate landing. Specifically:
  - Option B P1 (operators tables + FK columns + sentinel rows) must
    merge before subtask 1's clean form can ship.
  - Option B P2 (push at create — bridges write `operated_by_id` instead
    of TEXT) must reach the bridges before the inbox query gives
    correct results.
  - Until Option B P1/P2 land, the design epic can ship a **v0.5
    interim** that uses the Round-9 application-layer expansion against
    `user_identity` beads. That's a throwaway path, not a parallel
    architecture — the v1 path replaces it with a single FK lookup once
    P2 has soaked.
- **Subtask 8 (`DeriveCategory` from D7 plan `ka-0000003787`)** is
  **unchanged**. Category derivation runs on `(type, parent, labels)` —
  the operator field schema doesn't touch it.

### Why I missed this in Round 9

Round 9 corrected Round 8's framing (jira-style field selection →
identity resolution) but stopped one level short: I pointed at
`user_identity` beads as the resolution mechanism without checking
whether a *better* mechanism was already in flight. The `operators`
table makes `user_identity` redundant for this purpose. Lesson encoded
on top of Round 9's: when proposing an application-layer primitive,
search for an in-flight schema-layer fix first — `kd list -l
project:gardenio --status=open` filtered to `epic` + grep for the
domain term takes 2 seconds and would have surfaced
`ka-0000003625` immediately.

### Cross-references filed

- D8-r2 (`ka-0000005011`) — comment added pointing to this section
  and to `ka-0000003625` (and ADR v2 MR !2103) so the next reader
  doesn't repeat the lookup.
- Design epic (`ka-0000006157`) — comment added rescoping subtask 1
  (predicate becomes `operated_by_id = :viewer_operator_id`) and
  subtask 4 (missing-identity banner reframed), plus the v0.5 / v1
  sequencing note and the dependency on Option B P1/P2.
- Option B epic (`ka-0000003625`) — comment added flagging the inbox
  design epic as a downstream consumer of the substrate, so when Option
  B's P-series gets sequenced, the inbox surface is in the
  consumer-list.

### Carryover decisions still open

- D5 auto-close (`ka-0000001330`) — further weakened with each round.
  Constellation age-opacity (Round 8 mock) makes stale visually obvious;
  ranking absorbs aging signal (Round 4); operator may want to withdraw.
- D4-revised escalation schema (`ka-0000003919`) — closed without a
  clear pick; operator may want fresh ideas if the design epic gets
  built.
