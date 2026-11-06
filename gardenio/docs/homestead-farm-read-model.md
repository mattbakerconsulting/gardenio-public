# Homestead farm read model (`/garden/farm`)

Phase 1 data track of the Homestead plan (gd-f7r05Pm9eM, task gd-L9fjdMGV7P).
This is the contract the art track builds against. The Go types are in
`controller/internal/gardenhub/farm_model.go`; a real-shaped example is
`controller/internal/gardenhub/testdata/farm_fixture.json`. A golden test
(`TestFarmFixtureGolden`) fails if the JSON drifts from the fixture, so the
fixture, this page and the code cannot disagree silently. The fixture carries
every cost field (see [Costs](#costs)); the same purse a minute later with the
costs rollup down is `testdata/farm_fixture_cost_down.json`, pinned by the same
test. Regenerate both with
`go test ./internal/gardenhub -run FarmFixtureGolden -update-farm-fixture`.

Schema version: `meta.schema = 1`. Additive fields do not bump it; a rename,
removal or meaning change does.

## Endpoints

All live on the existing `/garden*` route class (see Authorization). Read-only.

| Route | What |
|---|---|
| `GET /garden/farm` | The whole farm as one JSON document, built from memory (no daemon call on the request path). `ETag: W/"f<v>"` (a revisit at the same version is a bodyless 304), `Server-Timing` (`snapshot`, `gzip`, `snapshot-age`, `total`; see `internal/servertiming`), gzip when the client accepts it (about 40x; the typical farm is about 5 KB on the wire). A project-scoped viewer gets the document cut to its projects, under `ETag: W/"s<hash>"` (see Authorization). |
| `GET /garden/farm/events?since=<v>` | SSE. Send the `v` of the document you hold (or rely on `Last-Event-ID` on reconnect). A project-scoped viewer is on its cut's own stream, under the cut's versions (see Authorization). |
| `GET /garden/farm/activity/{id}?cursor=&since=&before=&limit=` | One gardener's transcript: its last finished calls, newest first, at most 200 (see Transcript). 404 for an id the viewer's cut does not keep or that is no gardener. |
| `GET /garden/farm/activity?scope=farm\|project:<p>\|plot:<id>&cursor=&since=&before=&limit=` | The transcripts of every gardener in the scope that the viewer's cut keeps, merged newest first, at most 100, each row with its `id` (see Transcript). |
| `GET /garden/bead/{id}` | One bead for the in-garden bead view (gd-BdhucNqv0i, `bead.go`): title, state, type, priority, assignee, project, description and notes (cut at 20k runes), fields (first 60, values cut at 400), parents, children, waits-on, holds-up and other links with each linked bead's title (60 in all, the rest counted in `rel_more`), and the newest 40 comments (`comments_total` counts them all). On a plan epic waiting to be approved (`plan_state=ready`, or blocked on an open decision: asked) each `children[]` row also carries `body`, that step's description cut at 1,200 runes, so the bead view reads every step from this one request (staked plots step 1, gd-AXIwI51ejX; additive, absent on any other bead). Unlike the document above this reads the daemon on the request: one bead, on a click, current. `Cache-Control: no-store`; 404 for a bead the viewer cannot see; `partial` names a part the daemon did not answer. A bead in a thread also carries `kin`, the thread's conversation, work and agents, read on the same request ([The bead view's kin](#the-bead-views-kin-get-gardenbeadid), gd-58mkDiWkNq). |

### SSE protocol

Same contract as the `/garden` page stream:

- `event: patch`, `id: <v>`: a patch that takes you from `prev` to `v` (below). Apply in order. If `patch.prev` is not the version you hold, you missed one: reconnect with `since` set to the version you hold (`web/plans.js` does), and take the catch-up.
- **Catch-up** (P1.4, gd-DuHnvvBYMr, `farm_delta.go`). Connecting with a `since` the stream has moved past sends **one patch from `since` to now** (`prev` is your version): every row changed since, once and whole at its current value, the removals of rows you held (never a row you were not sent), the blobs that moved, and the `exits` behind those removals. The stream keeps a change log per row and blob for about the last 4096 versions, so this works long after the 256-patch replay ring has rolled over. When the ring still reaches `since` and its patches are fewer bytes, they are sent instead. A connection that fell behind is caught up the same way from the last version it was sent, and the patches queued behind that catch-up are skipped.
- `event: reset`, `id: <v>`: `data` is the **full document** (identical shape to `GET /garden/farm`). Replace everything. Sent only when no catch-up can: `since` is 0 or unknown, below the change log's floor, from the future (the server restarted; versions are wall-clock based so this always resolves to a reset), or a version the stream never issued (another cut's).
- `event: hb`: heartbeat every 15 s.
- Ephemeral, with no `id:` line: `activity`, `doing` and `busy` (see Activity) and, only to a stream holding a
  transcript open, `call` (see Transcript).

A patch body:

```text
{
  "v": 1792000123456, "prev": 1792000123455,
  // Sets: per-entity upserts/removals, keyed by "id". Absent when untouched.
  "plans": { "up": [ /* FarmPlan, whole object */ ], "rm": ["gd-..."] },
  "seeds": { "up": [ /* FarmSeed */ ],               "rm": ["gd-..."] },
  "gardeners": { "up": [ /* FarmGardener */ ],       "rm": ["gd-..."] },
  // Patch-only: why each id in gardeners.rm left (see FarmGardener). Never in a reset.
  "exits": [ /* FarmExit */ ],
  // Singleton blobs: present ONLY when they changed, then carry the full new value.
  "closed_plans": { /* FarmClosedPlans */ },
  "meadow":       { /* FarmMeadow */ },
  "wishes":       { /* FarmWishes */ },
  "home":         { /* FarmHomes: every project's key, whole */ },
  "escalations":  { /* FarmEscalations */ },
  "releases":     { /* FarmReleases: every project's key, whole */ },
  "piles":        { /* FarmPiles */ },
  "meta":         { /* FarmMeta */ },
  "cost":         { /* FarmCost */ }
}
```

`v` is not always `prev + 1`: a project-scoped viewer's stream takes its
versions from a counter every cut shares (see Authorization), so chain on
`prev`, never on arithmetic.

A seed is always replaced whole. A plan is too, except that in a **live patch**
(not a catch-up, never a reset) each phase you already hold byte for byte comes
as a stub, `{"id": "<phase id>", "same": 1}`, and an unchanged `unphased` row as
`{"same": 1}`: put back the one you hold (`web/plans.js` `fillPlan`). A stub you
cannot fill is a gap, so reconnect. A step that moves re-sends its phase, not
its plan: at 10x about 2.4 KB against 8.5 KB.
A seed that is approved to build appears as `seeds.rm` + `plans.up` in one patch.
Patches are unordered within an array; the snapshot arrays are sorted
(`plans`/`seeds` by `activity`/`updated` descending, then id; `gardeners` by id).

## Document

```jsonc
{
  "v": 1792000123456,            // version; also the SSE id
  "built_at": "2026-10-15T22:00:00Z",
  "plans":  [FarmPlan],          // being built
  "gardeners": [FarmGardener],   // the live agents, on the plot of their task or at home
  "seeds":  [FarmSeed],          // draft / ready / asked plans (slim)
  "closed_plans": { "total": 36, "shown": [FarmClosedPlan] },   // newest 24
  "meadow": { "total": 212, "with_mr": 2, "shown": [FarmTask] },// newest 40
  "wishes": { "open": [FarmWish], "open_total": 48, "closed": [FarmClosedWish], "closed_total": 129 }, // see Wishes
  "home": { "gardenio": FarmHome, "": FarmHome },  // see Home: each home strip in counts, "" the Commons's; {} when nothing is at home
  "escalations": { "open": [FarmWait], "total": 23, "by_project": {} }, // every open escalation, fleet-wide; see Escalations
  "releases": { "gardenio": FarmProjectReleases },  // see Releases; {} before the first read or with no release source wired
  "piles":  { "closed_decisions": 8, "closed_escalations": 47, "closed_work": 1830, "closed_plans": 36,
              "orphan_mr_total": 1, "orphan_mrs": [{ "url": "https://github.com/o/r/pull/77", "iid": "77", "title": "Fix the gate latch",
                "repo": "o/r", "why": "bead_closed", "bead": "gd-orph", "assignee": "flo", "project": "gardenio", "since": 1792090000 }] },
  "meta":   { "schema": 1, "truncated": false, "row_cap": 2500, "rows": 640 },
  // meta may also carry "gardeners_truncated": true and "gardeners_as_of": <unix s> (see FarmGardener)
  "cost":   { "scope": "fleet", "has": true, "today_usd": 6.75, "week_usd": 104.65, "has_budget": true,
              "budget_usd": 600, "spent_usd": 230.5, "remaining_usd": 380,
              "projects": [FarmCostProject] }  // see Costs; absent when the viewer has no costs rollup wired
}
```

### FarmPlan

| field | meaning |
|---|---|
| `id`, `title`, `project` | the plan epic; `project` is its `project:` label |
| `short_title` | the epic's short title, trimmed (short titles below); omitted when it has none |
| `state` | raw `plan_state` (`building`, `complete`, `abandoned`, or `""` when unset). Never `draft`/`ready`: those are seeds |
| `status` | bead status of the epic (`open`, `in_progress`, ...) |
| `operator`, `owner` | lowercased `operated_by` alias (`oidc:<email>`), and the bead's `owner` |
| `priority` | 0 = P0 ... 4 = P4 |
| `created`, `updated`, `activity` | unix seconds. `activity` = newest `updated_at` across the epic and its open tasks |
| `heraldry` | the epic's arms in the canonical form (heraldry below); omitted when it bears none |
| `heraldry_name` | those arms in plain words (heraldry below); omitted with `heraldry` |
| `phases[]` | in `phase_order`. A **closed** phase is still listed (`status: "closed"`, `tasks: []`, `counts.closed` = its pile) |
| `unphased` | same shape as a phase, `id: ""`, `title: "Other"`: steps that reach the plan without a phase |
| `counts` | sum over `phases` and `unphased` |
| `open_decisions`, `open_escalations` | open `type=decision` / `type=escalation` tasks in the plan (also present as signpost/chest tasks) |
| `needs_human` | how many of those wait on a human (`waiting[].human`). Only these are "needs you" (the gold ring, the pulse, the need counts): an approved escalation is open but waits on its executor, unless that wait falls to the operator (below) |
| `waiting[]` | every open decision and escalation, saying what it waits on (FarmWait below). Human waits first, then the longest wait first; at most 12 listed. Omitted when there are none |
| `shipped` | the tag of the release that carries the plan's newest merged PR (see Releases), whatever that release's status; omitted when no release read carries it yet |
| `produce[]`, `produce_more` | the plan's produce rows (FarmProduce below, gd-HqcG2zcwnJ), read by the bead view, the harvest cells and the goats' kin: the merge requests its steps carry, closed steps included, and the bundle and report beads under it. By kind: reports, then bundles, then MRs (gd-wgeINbOQMC); within a kind not-ready first, then newest first. Every row of a real plan is listed; the 300-row safety bound counts the rest in `produce_more`. Both omitted when there is nothing |
| `cost` | what the plan's crew have spent (Plan cost below): `{has, usd, synced_at?, crew, live?, stale?}`. Always present; `has:false` and `usd:null` until the controller has rolled the plan up |

A FarmWait is `{id, title, kind, state, held?, human, waits_on?, due?, since, project?, op?, family?, executor?}` (gd-TEl9BfDL2z).
`kind` is `decision` or `escalation`; `since` is when the current wait began
(unix seconds). `state` is read from the fields kbeads writes, never from the
status alone:

| kind | `state` | when | waits on |
|---|---|---|---|
| decision | `unanswered` | no `chosen` / `response_text` | a human (`human: true`) |
| decision | `answered` | answered but not yet closed (rare: resolving closes it) | the asking agent: `requesting_agent_bead_id`, `requested_by`, else `created_by`; `since` = `responded_at` |
| escalation | `pending` | open, no `approved_at` | a human (`human: true`) |
| escalation | `approved` | `approved_at` set (approval also moves it to `in_progress`) and not closed | the operator when `execution_target=local` or `operation_type=manual_operator_action` (`human: true`, `due` = `due_at`); else the first live one of `executor`, `assignee`, `requesting_agent`, `created_by`; `since` = `approved_at` |
| escalation | `rejected` / `failed` | `result_status=failure` but still open (rare: reject and a failed run close it). `rejected` when `result` carries the reject marker | `requesting_agent`, else `created_by` |

An escalation wait that would be human is the owner's only when both who it is
for and who holds it say so, the rule /up's needs-you pick reads (gd-vFUcVoRCU4):

- its effective audience (`beadsapi.EscalationAudience`, the forced-owner rule
  included) is `desk` or `operator`: `human: false`, `waits_on` = `the merge desk`
  or `the operators` (gd-bmOiteQi36);
- else its `held_by` names a lane other than `operator` or `land-owner:<land>`
  (a coordinator, steward, requester or server lane): `human: false`, `waits_on`
  = the lane's label (`coordinator laptop`), even for a forced-owner item. A
  land owner's stays a human wait; an unstamped one follows the audience.

So the fleet's human escalation waits are the open ones `held_by=operator`
(plus land owners' and unstamped owner-audience ones) that are unapproved or run by hand.

An escalation also carries (gd-VyP9DLgvih), and a decision never does:

- `project`: its `project:` label, omitted when it has none (an unprojected
  escalation, which the Commons strip holds).
- `op`: its `fields.operation_type` verbatim, omitted when unset.
- `family`: its normalised kind from one Go table (`farm_escalations.go`
  `escalationFamilies`, read lowercased and trimmed). Always set, never empty:

  | `family` | operation types |
  |---|---|
  | `merge` | `gitlab_mr_merge`, `github_pr_merge`, `gitlab_merge` |
  | `deploy` | `helm_deploy`, `deploy` |
  | `config` | `helm_chart_edit`, `helm_chart_change` |
  | `prompt` | `trigger_prompt_install`, `trigger_prompt_reseal`, `trigger_prompt_amend`, `trigger_reseal`, and any other `trigger_prompt_*` |
  | `beads` | `beads_admin_write` and any `kbeads_*` not listed under access |
  | `access` | `aws_iam`, `iam_grant`, `k8s_rbac`, `github_token_scope`, `kbeads_project_access`, `kbeads_authz_change` |
  | `infra` | `infrastructure`, `terraform_apply`, `github_workflow_dispatch` |
  | `read` | `db_readonly_query`, `cluster_read` |
  | `other` | anything else, an empty `op` included |

- `executor`: the name of the gardener working it, the live agent whose
  `fields.escalation_id` is this escalation (the join that stands it on the
  plot, FarmGardener's `escalation`). Read from the same agent read as
  `gardeners`, else the last good gardener set; two on one escalation name the
  newest. Omitted when none is. It is the agent's own name, a synthetic
  `escalation-<id>` included, so a client can find it among `gardeners[]`;
  `waits_on` still hides a synthetic name.
- `held`: on an escalation, the agent lane holding it when it is not a floor
  item (`gardenhub.AgentLaneHolder`: a `coordinator:` or `steward:` `held_by`,
  `the merge desk` for `coordinator:merge-desk`). Its cell's answer box then
  says that lane has it and offers only "Approve anyway", no primary Approve
  (gd-Mq0uDXHswK). Omitted for a floor item, one the owner or a land owner
  holds, and an unstamped one.

`waits_on` is a name: where the field holds an agent bead id (`requesting_agent`
may be either) the farm reads the agent bead once and caches its name.

It names a **live** party (gd-g7jfH27efD). The farm re-reads the live agents
(open or `in_progress` agent beads not in a dead `agent_state`: `done`, `failed`,
`stopping`, `budget_exceeded`) on every refresh, from the same agent read as
`gardeners`, and looks up once per name
whether an agent bead carries it. A ref that is a known agent missing from the
roster is finished and is skipped for the next ref; a ref that is not an agent
(a person, `laptop-claude` without an agent bead) is named as is. When every
named party is finished the wait names `the operator` and sets `human: true`, so
it counts in `needs_human`. A synthetic executor name (`escalation-<id>`) is never
shown: it reads `its executor` while live, and `its executor (finished)` when its
bead said so but the roster is unavailable, so nobody can be checked for
liveness. A failed or truncated roster read keeps the last good one for 2
minutes; past that no ref is called finished.

A phase is `{id, title, short_title?, status, counts, tasks[], more?, heraldry?, heraldry_name?, closed[]?}`;
`short_title` is the phase container's short title (short titles below), never on `unphased`;
`heraldry` is the phase container's arms (heraldry below), omitted when it bears
none, so never on `unphased`, and `heraldry_name` those arms in plain words. `counts` is
`{in_progress, review, blocked, open, deferred, closed}`; the first five are
exact tallies of the open rows **including** tasks the cap omitted, `closed` is a
count of direct closed children. `tasks` lists at most 60
per phase: the cap keeps the in-flight ones (`in_progress`, `review`, `blocked`, `open`,
`deferred`, then priority, then newest change), and the listed ones are then ordered by
`created`, then id, the order the bed plants them in, so a step keeps its cell through
every status change (gd-wDdun2AVwq); `more` is how many open tasks were left
out. `closed[]` lists the phase's newest closed steps (by `updated`), each a slim
closed row (FarmClosedStep below), only as many as its 9x9 bed has cells left after
`tasks` and the `+N` crate (81 − `len(tasks)` − 1 when `more` > 0), so the cursor
can land on a closed step and open it (gd-0Fj6E2vPql). A plan lists at most 24
closed steps across its phases and the document at most 480 across its plans, the
newest change first in both (gd-8fjCFsoAKI); a seed's take none of that room. The
rest of `counts.closed` is count-only stubble. The slow tier reads the newest 2000
closed work beads for this, so a long-finished phase's closed steps may be
count-only. The plan's 12
newest listed closed rows (by `closed_at`, across all its phases) carry a
`harvest` object (FarmHarvest below); every other closed row is plain stubble.

A step nested below the phase (a task under a sub-epic under the phase) belongs
to the phase; the intermediate sub-epic is itself a task of type `epic`.

### FarmTask (a plant)

```jsonc
{ "id": "gd-A1a", "title": "...", "type": "feature", "state": "review",
  "priority": 2, "project": "gardenio", "assignee": "mo", "updated": 1792000900,
  "mr": { "url": "https://.../412", "iid": "!412", "state": "opened" },
  "blocked_by": ["gd-A2a"], "parent": "" }
```

- `type`: `task` (turnip), `feature` (pumpkin), `chore` (carrot), `bug`, `decision` (signpost), `escalation` (its family's sprite, `family` below), `epic`/`molecule` (a non-phase sub-container).
- `state`: `in_progress`, `review`, `blocked`, `open`, `deferred`. Closed beads are never tasks.
  - `review` = status open/in_progress AND a merge request is linked (`gb mr link` sets `mr_url`/`mr_iid`/`mr_state` on the bead) AND `mr.state` is not `merged`/`closed`.
  - `blocked` is the bead status. A task waiting on a `blocks` edge keeps its own state and carries `blocked_by`: the art track draws the rock/ditch from that.
- `blocked_by`: ids of **still-open** beads this task has a `blocks` dependency on (at most 8). Edges can cross plans. An edge vanishes the refresh after its blocker closes. New edges can take up to 20 s to appear (they are read in the slow tier).
- `blocked_by_project` (additive, gd-CNhEh30Ykf): for each id in `blocked_by` whose `project:` label differs from this task's, that label (`""` for a blocker with none). A blocker it does not name shares the task's project; the map is omitted when every blocker does. It lets a client draw a cross-project edge to a blocker the document does not carry (past a phase cap, inside a seed, beyond the meadow cap): `"blocked_by_project": {"gd-K1": "kbeads"}`.
- `parent`: meadow beads only; empty for a bead with no parent.
- `op`, `family`, `wait` (additive, gd-UDx80F47JN): an escalation's `op` and `family` as on its FarmWait, and `wait`, its FarmWait `state` (`pending`, `approved`, `rejected`, `failed`) while it is open, so its plot draws the Yard's family sprite and state dressing (`web/art.js` `escRows`). Omitted on every other type; `wait` omitted on a closed escalation.
- `heraldry` (additive, gd-XIMjgmciwD): the bead's arms in the canonical form (heraldry below); omitted when it bears none.
- `short_title` (additive, gd-MpWy5X34hh): the bead's short title (short titles below); omitted when it has none.

### Short titles (a plan, a phase, a task, a closed step)

`short_title` on FarmPlan, a phase row, FarmTask and FarmClosedStep is the bead's
`short_title` column (at most 40 runes, the name humans see), trimmed, beside its
full `title` (gd-MpWy5X34hh). The key is left out where the bead has none, and
always on `unphased`, so a farm without short titles is the same size it was. A
label where space is tight reads it first: `TITLES.name(x)` (`web/titles.js`) is
`short_title`, else `title`, else the id; the tooltip keeps the title. A closed
phase keeps the short title it had when the slow tier first read it. A ready or
asked FarmSeed has its own `short_title` (FarmSeed below). Additive, so
`meta.schema` stays 1.

A task on the map is read back to its plan and phase without a search: its bed
cell in the layout index (`web/world.js`) names its plot (the plan id) and its
`phase` (the phase id, `""` for the Other bed), and `TITLES.chain(index, id)`
returns the Plan › Phase › Task parts, `[{kind, id, name, title}]`, the phase
left out for an unphased step.

### Heraldry (arms on a plan, a phase, a task)

`heraldry` on FarmPlan, a phase row and FarmTask is the bead's `heraldry` column
as the daemon stores it, byte for byte: the canonical form of
[heraldry spec §4.1](superpowers/specs/2026-10-25-heraldry-design.md), a colour
word and five places, `"blue 🌱 . 🔥 . 📦"`. It is **opaque** and never shown:
the client fetches the picture from the renderer by it (spec §6.4) and draws that
image; no rune clip ever applies to it. On a plan and a phase row (not a task:
tasks bear no arms on the World, spec R6) `heraldry_name` (additive, step 4.3,
gd-Eskj7oA4Kp) is the same arms in plain words, `"arms on blue: compass
top-left, fire centre, package bottom-right"` (spec §8), which the plot's and the
phase stake's hot links carry as their accessible name: the page has no charge
library to name them with. It is omitted with `heraldry`, and when the renderer
refuses the form, so the page then neither names nor fetches a picture. The farm
carries nothing else for arms: no picture, no sprite, no emoji run, no colour value. A closed phase
keeps the arms it had when the slow tier first read it (closed is terminal). A ready or
asked FarmSeed's `steps[]` carry their step's `heraldry` (no name); the seed itself,
FarmClosedStep and the other rows carry no arms. Additive, so `meta.schema` stays 1.

### FarmClosedStep (a listed closed step)

```jsonc
{ "id": "gd-A1y", "title": "...", "short_title": "...", "type": "task", "state": "closed",
  "assignee": "mo", "updated": 1792000021, "created": 1792000003,
  "harvest": { /* FarmHarvest */ } }
```

A cursor stop on the bed: only what the cell, its tooltip and its status line
(`closed · @who · 3d`) show, and its `short_title` (short titles above) when it has one. `state` is always `closed`. `created` is its place
in the bed: the plants, open tasks and listed closed steps together, sit in
created-then-id order, so a step keeps its cell when it closes (gd-wDdun2AVwq). It has no `priority`,
`project`, `mr` or `blocked_by` (gd-8fjCFsoAKI): the bead view reads those from the
bead, and a merged MR's number is in `harvest.mr`.

- `harvest`: only on the plan's 12 newest listed closed steps; see FarmHarvest.

### FarmHarvest (what a closed step yielded)

```jsonc
{ "id": "gd-A1y", "state": "closed", "updated": 1792000021, /* ...the closed row... */
  "harvest": { "closed": 1792000021, "base": "sheaf", "mr": "!405", "ux": [5, 5], "pics": 4,
               "tests": 3, "ci": "success", "rounds": 1 } }
```

The facts a harvest cell is drawn from (harvest spec
[`2026-10-22-lands-harvest-icons-design.md`](superpowers/specs/2026-10-22-lands-harvest-icons-design.md)
§3, §4; `farm_harvest.go`). For each plan, the 12 newest listed closed rows by `closed_at`, across
all its phases, carry one; every other closed row stays plain stubble. Zero and empty facts are
left out, so a step with nothing recorded is `{"closed": ...}`.

- `closed`: kbeads `closed_at`, unix seconds (the row's `updated` when the daemon sent none).
- `base`: `sheaf` or `trampled`; absent for bare stubble. First match wins: **trampled** when
  `close_reason` starts with `wontfix`, `abandon`, `duplicate`, `obsolete` or `supersede`
  (any case), or the MR was closed unmerged and there is no other yield; **sheaf** when the MR
  merged, a ready produce row hangs off the step (FarmProduce's read joined by `task`,
  uncapped), or an evidence bead was filed on it. The bead view's Harvest section uses the same
  rule.
- `mr`: the merged MR's number as its forge writes it, `#825` (GitHub) or `!825` (GitLab).
  Absent unless merged.
- `ux`: the newest `kind=ux` evidence bead's `[score, of]` (gates passed, gates graded).
- `pics`: that bead's picture links (its `urls` field) plus the step's `goldens_touched`.
- `tests`: the step's `tests_touched`, when above 0.
- `ci`: the step's `ci_state` at merge, raw. Only `success` lights the lantern; anything else is
  a word in the status line.
- `rounds`: the step's `review_rounds` (1 = merged first time).

`ci_state`, `tests_touched`, `goldens_touched` and `review_rounds` are written on the step at merge
(github-bridge and the merge desk, harvest spec §2.3, §2.4) and read from the closed row the slow
tier already has. Evidence is one more slow-tier list: `type=evidence`, every status, newest first,
600 rows, indexed by each bead's `step` field (its parent when the field is missing). A row whose
step is not among a plan's 12 is unused. A failed evidence read keeps the last index and never
fails the refresh; with no building plan it is not read. An evidence bead event refreshes the farm
(`relevance.go`).

### FarmProduce (a plan's produce rows)

```jsonc
{ "id": "gd-A1a", "kind": "mr", "title": "Token exchange for agent pods", "url": "https://.../412", "iid": "!412",
  "state": "opened", "ready": false, "task": "gd-A1a", "phase": "gd-A1", "updated": 1792000900 }
```

What a plan produced, or is about to. The rows are open work only and nothing closed
is sent as a bead, so without this list a merged MR on a finished step, a bundle or a
report is invisible. It is read in the slow tier (`farm_produce.go`): one list of the
work beads carrying an `mr_url` and one of the `bundle` and `report` beads, both across
every status, newest first, 600 each; every row is walked up to its plan by parent-child
edges. Open ancestors are in the rows; a closed one (a finished step, a closed sub-epic,
a closed phase) is learned by one forward `parent-child` edge read, in rounds (one per
unknown hop, at most 6 rounds and 60 ids a tick) and cached, because closed is terminal.
A read that fails keeps the last rows and never fails the refresh.

- `kind`: `mr`, `bundle`, `report`.
- `id`: the bead the item stands for. For an MR it is the **step** that carries it (the
  step's bead view has the link; a plan never lists one URL twice, whichever step linked
  it). For a bundle or report it is that bead.
- `ready`: produced. An MR is ready once merged (`mr_state` merged, or `mr_merged`); an
  opened or draft one is **expected** produce, shown grayed ahead of the merged MRs. An MR
  closed without merging is compost, never listed. A bundle or report is ready by
  existing, whatever its status. `state` is the raw `mr_state` (`merged`, `opened`,
  `draft`) or the document's bead status.
- `task`, `phase`: the step the item hangs off and that step's phase, the cell a client
  zooms the plot to. For an MR, its own step. For a document, its parent when that is a
  step (a step nested under a sub-epic is a task of its phase, as the plot draws it);
  `""` when it sits directly under the plan or a phase (a plan's narrative bundle).
- `title`: `mr_title` when the forge wrote one, else the step's title; the bead title for
  a document. `url` and `iid` are the MR's.

The page's bin by the plot (`web/bin.js`) drew these rows and listed the plan's recent work from its
phases (gd-2qgcDRZUX6); the bin was retired in plan gd-PJB3D7glwF (step gd-vDauUoOkj8), and
`produce[]` is now read by the bead view, the harvest cells and the goats' kin.

An MR's state is the step's `mr_state`, which the forge bridge writes with the MR bead;
the farm does not read the MR bead itself. A `bundle` or `report` bead event refreshes the
farm (`relevance.go`); an MR's state change arrives as a field change on its step.

### FarmGardener (a person)

The live agents (gd-DKMSc1Y91I). The design, with the client's placement,
sprites and transitions, is [`homestead-agents-as-people.md`](homestead-agents-as-people.md)
sections 2 and 3; this is the wire contract.

```jsonc
{ "id": "gd-gWork1", "agent": "g-work-1", "role": "crew", "project": "gardenio",
  "operator": "oidc:matt@example.com", "state": "working", "bucket": "alive",
  "task": "gd-A1a", "task_title": "Token exchange for agent pods",
  "plot": { "plan": "gd-planA", "phase": "gd-A1" }, "since": 1792009380,
  "href": "/garden?bead=gd-gWork1", "chat": "/garden/c/bead/gd-gWork1", "reason": null,
  "cost": { "spent_usd": 3.2, "live_usd": null, "at_least": true, "gardener_usd": null,
            "text": "Spent at least $3.20; this session is counted in full when it ends." } }
// a thread agent at home: { ..., "role": "thread", "task": "", "plot": null,
//   "chat": "/garden/c/bead/gd-gThread", "thread_title": "a pen for the goats", ... }
```

- **Who.** One entry per open or `in_progress` agent bead, except a prewarmed pool shell nobody engaged
  (`agent_state=prewarmed`, or `queued`/`idle` with a `prewarmed-` name and no thread or task) and an agent
  on its way out (`done`, `stopping`, `stop_requested`, `expired`). `failed` and `budget_exceeded` stay:
  they are the scarecrows.
- `agent`: `fields.agent`, else the title. `role`: raw `fields.role` (may be a comma list). `project`: the
  agent's own `project:` label, else `fields.project` (where it comes from, not where it works).
  `operator`: lowercased `operated_by`, as on plans.
- `state`: raw `agent_state` (`""` when unset). `bucket`: `alive` (`working`, `spawning`, `yielding`),
  `stuck` (`failed`, `rate_limited`, `budget_exceeded`), `dormant`, or `idle` for everything else. It is
  beads-viewer's `agentStateBucket`; `TestAgentBucketMatchesGardenhub` keeps the two in agreement.
- `task`: raw `task_id` (`""` when unset). `task_title`: the task's title, cut at 120 runes: from the
  open work rows read, else from the farm's own read of it (a closed task, one past the row cap:
  `readRefTitles`, below). Absent only when neither has it, so the Porch says "Assigned: <title>",
  not the id (gd-YvdhZD2UtL).
- `plot`: `{plan, phase}` from the same ancestor walk (up to 6 hops) that places the task's plant, so a
  gardener stands on the plot its task is drawn on. The task may be a step (its phase, or `""` for
  "Other"), a phase container (that phase) or the plan epic (`phase: ""`, the gate). `null` (at home)
  when there is no task, or it is a seed's, a meadow bead, closed, or not read.
- `since`: `last_progress_at`, else the bead's `updated_at`, unix seconds floored to the minute. Nothing
  that ticks with each turn (tokens, turns) is in the entry, and the running session's cost moves only in
  $0.50 steps (`cost.live_usd`), so a working gardener upserts about once a minute at most.
- `href`: the in-garden bead view. `chat`: the agent's Commons place route, present only when the bead
  has `thread_id` (or the legacy `slack_thread_bead_id`). Both are paths below the viewer root; the
  place route does the per-viewer land cut, so a `chat` link can 404 for a viewer outside the land.
- `thread_title` (5b, gd-1kFVOmUjiK): the title of the thread head the bead names (`thread_id`, else
  `slack_thread_bead_id`), without the bridge's `Commons thread: ` or `Slack thread: ` lead, cut at 120
  runes. Present when the head is among the open work rows read; for a thread agent (a `role` token
  `thread` or `dm`) whose head is not (closed, or past the row cap), the farm reads it itself: one
  `BatchGetBeads` of at most 20 beads per refresh, each cached until no live gardener names it
  (`readRefTitles`, `farm_gardeners.go`). The same read names a gardener's `task`, `tends` and
  `escalation` when they are not among the rows. A thread agent has no task under a plan, so it is at home;
  the client says it is "In the Commons: <thread_title>" (`homestead-agents-as-people.md` 4.1). Absent
  otherwise; a consumer that does not know the key ignores it, as with any other.
- `tends`, `tends_title`: present only for a gardener the bead view's "Send a gardener" started
  (gd-I3Qx07esJj, `tend.go`, `web/tend.js`): the bead it was sent to, read from the mention that
  started its thread (the agent bead's `slack_root_text` opens with `@gardenio tend <bead>:`), and that
  bead's title (as `task_title` is), cut at 120 runes. With no `task` of its own
  the gardener stands on that bead's `plot`; with one, its task places it. A gardener's exit
  (`FarmExit.tends`) names the same bead, so the bead view can show what it said when it left.
- `escalation`, `escalation_title`: present only for the bridge's escalation executor, the agent spawned
  to run an approved escalation (`escalation_id` on the agent bead; bridge `escalations.go`
  `spawnExecutionAgent`): that escalation, and its title (as `task_title` is), cut at 120 runes. With no `task` and no `tends` the executor stands on the escalation's `plot` (an
  escalation is parented to the task that filed it, so it resolves as that task does); otherwise the
  task, then the send, places it. Before this an executor stood at the barn for its whole run
  (gd-sXAaYVo2UB). Its exit (`FarmExit.escalation`) names the same escalation, so the escalation's bead
  view lists it while it runs and shows what it said when it left (`web/tend.js`); the Porch and the
  gardener card say "Running: <escalation_title>".
- `task_arms`: present when the line of the bead that places the gardener (its `task`, else `tends`, else
  `escalation`) bears arms: `{canonical, n, name}`, the nearest armed ancestor's run (`badge.ResolveArms`,
  heraldry step 5.9, gd-QboM9sPYCg), resolved over the open rows the farm already read and its own
  reads of a closed task (`farm_gardeners.go` `gardenerArms`), never a daemon read of its own. The
  Porch row draws it before the step, and the figure wears it as a badge at the Plan and Bead levels; a
  plan's "Gardeners here" does not (budget B1, decision gd-C5WGUm5SLu).
- `reason`: always present. `null` for a gardener that is simply working; otherwise `{kind, text, href}`
  built server-side (`farm_reason.go`) so every client shows the same words. Only the agent bead and the
  open decisions and escalations the farm already read feed it; nothing comes from kubectl. The first kind
  that holds wins:

  | `kind` | when | `text` |
  |---|---|---|
  | `down` | `bucket` is `stuck`, or `pod_phase` is `failed`/`unknown`, or (agent not alive) the wrap-up has `blockers` | `Failed`, `Rate limited`, `Out of budget`, `Pod failed` or `Pod lost`, then `: ` and the wrap-up's `blockers` else `close_reason`; `Blocked: <blockers>` alone |
  | `parked` | an open decision still unanswered, or an escalation still pending, names this agent (`requesting_agent_bead_id`, `requesting_agent`, `requested_by` or creator: bead id or name) or parks its task (`requesting_task`); the oldest wins. A `working` agent counts only once it is also stale | `Waiting on <handle title>`; `href` is the handle's in-garden bead view, which carries the Mailbox's answer row |
  | `sleeping` | `agent_state=dormant`, or (agent not alive) the wrap-up has `restart_when` | `Sleeping`, or `Sleeping until <restart_when>` (a leading `when `/`until ` dropped) |
  | `stale` | `bucket` is `alive` and `last_progress_at` (else `updated_at`) is 1 h old or more: the `/agents` threshold, pinned by `TestAgentBucketMatchesGardenhub` | `No progress for <N>h` (whole hours, so it upserts once an hour) |
  | `starting` | `spawning`, `assigning`, `queued` | `Starting up`, `Getting its task`, `Waiting for a key in the account pool` |

  `text` is cut at 140 runes; `href` is omitted unless set and is a path below the viewer root. A wrap-up
  is read only while the agent is not alive, so a respawned agent is not judged by its last run. A handle
  past the row cap is not seen, and its gardener is not parked. A consumer must ignore a `kind` it does not
  know (Lands 9.7 reserves `purse_empty`).
- `cost` (gd-0VeG81uW3w, `farm_gardener_cost.go`): what the gardener has spent, always present, `null`
  only when nothing is known (a `dormant` agent with no reconciled total and no trigger rollup). Every
  amount is US dollars rounded to the cent; a figure not known is `null`, never `0`. The project to cut
  it by (the per-viewer cut) is the entry's own `project`; a scoped viewer gets it only on an operator
  land and without `gardener_usd` (see Authorization).
  - `spent_usd`: beads-viewer's `agentSpendUSD`, so `/garden` and `/agents` agree: `total_cost_usd` (the
    sessions the controller's reconciler has closed) plus, while `bucket` is `alive`, the running
    session's `live_cost_usd` floored to $0.50. `null` when neither is above 0.
  - `live_usd`: the running session's part of `spent_usd`, `null` when it is not counted (not `alive`, or
    under one step). On the LiteLLM path `live_cost_usd` stays 0 until the session closes (bug
    gd-UIlh0DR2VG), so today it is usually `null`: a 0 there is "not known", not "free".
  - `at_least`: `true` unless the gardener is `dormant`: a session may still be open whose cost is
    counted in full only when it closes, so `spent_usd` is a lower bound. A client marks the figure as
    such ("at least"), in words and in its `aria-label`.
  - `gardener_usd`: on a gardener's run (the agent bead has `trigger_id`, else the legacy `schedule_id`),
    its trigger's `gardener_spend_usd`: the controller's rollup over every run of that gardener and the
    crew they sent (`cmd/controller/gardener_spend_sync.go`, every 10 minutes; it sums closed sessions,
    so it too is a lower bound while a run is live). The farm reads it with one `BatchGetBeads` of the
    triggers the live gardeners name (at most 50), each re-read at most every 5 minutes and a new one
    at once; a failed read keeps the last figures. `null` on any other agent, or when the trigger has
    no rollup yet.
  - `text`: the words every client shows on hover, e.g. `Spent at least $0.67; this session is counted
    in full when it ends. Its gardener has spent at least $40.79 over all its runs and their crew.`,
    or `Nothing counted yet; this session is counted when it ends.` when no figure is known.
- **What reads `reason` on the page.** The sprite's pose (`web/reason.js`: `down` the scarecrow, `parked` the
  speech bubble, whose click opens `href`'s Mailbox letter, `sleeping` zzz, `stale` wilting, `starting` a
  sprout); the card's reason line; the gardener's Commons thread, whose quick replies are built from it
  (`quick()` in `web/chat.js`: an Answer button for `parked`, a question for `down` and `stale`); and the
  Wake / Restart button (`web/wake.js`: **Wake** for `sleeping`, **Restart** for `down`, nothing for the rest),
  which posts `POST /garden/gardener/{id}/wake` (`wake.go`, admin only, the one write here). None of them
  needs a field beyond this entry. A click on the sprite itself opens `chat`, on desktop and phone alike (3b R7, #865).
  Design: agents-as-people sections 12 and 13.
- **Bounds and failure.** At most 300; past that the newest by `updated_at` are kept and
  `meta.gardeners_truncated` is `true`. A failed agent read, or one the daemon cut (more than 500 live
  agents), never fails the refresh: the last good set stays, and once that read is older than 2 minutes
  `meta.gardeners_as_of` (unix seconds) says when it was taken. While reads are good the key is omitted.

A consumer must ignore keys it does not know and treat `plot: null` as "at home", unless it draws
the gardener's `task` (else `tends`) somewhere else itself: the garden stands such a gardener beside
its Meadow wildflower, Nursery pot, Orchard tree, headstone or goat (`homestead-agents-as-people.md`
4.1, gd-6FCoWelnx2).

**Exits** ride on the patch that removes a gardener (`gardeners.rm`), at most 20 per refresh, and only
when this agent read and the previous one were both complete (an id dropped by the cap or a bad read is
not an exit). They are never in the document or a reset, and a client never stores them.

```jsonc
{ "id": "gd-...", "agent": "...", "kind": "finished", "tldr": "Merged MR 3286 ...",
  "plot": { "plan": "gd-...", "phase": "gd-..." }, "at": 1792420500 }
```

| `kind` | when | `tldr` |
|---|---|---|
| `finished` | `stop_requested=true`, and the task is closed, is a plan or phase container, or there is none | `wrapup.tldr`, cut at 140 runes |
| `parked` | `stop_requested=true`, and the task is still an open work step (stopped behind a merge escalation or a decision) | `wrapup.tldr` |
| `interrupted` | no `stop_requested`: killed, pod lost, reaped, expired | omitted |

`plot` is the gardener's last plot (`null` if it was at home); `at` is when the farm saw it leave.

### Activity (what a gardener is doing)

Step 4a of plan gd-gAKb8Bf0Nm (gd-H4jk9KgUAG; spec gd-zE5JlcI54m section 4a). The birds (4b) and the
poses (4c) are built on this contract, so its shapes are fixed here.

**Source.** beads-viewer subscribes on core NATS (no durable consumer, no replay: a missed bird does not
matter) to `hooks.*.PreToolUse`, `hooks.*.PostToolUse`, `hooks.*.PostToolUseFailure`,
`hooks.*.SubagentStart` and `hooks.*.SubagentStop`, which `gb hook relay` publishes for every agent
(`cmd/beads-viewer/activity_feed.go`), and hears each telepathy thought (`ThinkingHub.OnThought`) and each
assistant text block coop forwards on `coop.events.messages.*.assistant_text` (`ThinkingHub.OnSay`,
`Farm.ObserveSay`: what the gardener said, gd-HHGjxt59Mr). Events
are keyed by `agent_bead_id`; one without it is dropped. A call from inside a subagent
(`is_sidechain: true`) is not the gardener's: the subagent stands for it. Everything lives in the hub's
memory (`internal/gardenhub/activity.go`); nothing is written to a bead.

**What leaves the server.** The class, a tool word and, on a call's start, a scrubbed caption. The tool
word is `toollatency.ClassifyCmd` of a Bash command (`go_test`, `git_push`, `kd_other`, `other`, ...: the
same bounded domain as the `bash_cmd_latency_seconds` label), else the tool name kept to
`[A-Za-z0-9_.-]` and 40 bytes, or `subagent` for the two subagent events. The caption (`cap`, below) is
made from at most one `tool_input` field per call and scrubbed. `tool_input` is otherwise read only to
classify a Bash command and is never kept, nor are the session id, cwd, tool_use_id or anything else in
the payload (`TestActivityNoToolInputOnTheWire` plants secrets, paths, emails and session ids in each,
`tool_description` and `tool_file_path` included, and reads the stream and the document: none arrives,
and a planted plain description arrives scrubbed).

**The caption** (`cap`; bird captions step 2, gd-gCHKhIROvB; spec
`docs/superpowers/specs/2026-10-28-bird-captions-design.md` sections 5-7; `internal/gardenhub/caption.go`)
says what a call is **for**, in a line of words. It is made once per call in the hub, by
`captionOf(tool, tool_input)` without a model, first match wins; only a Bash call that row 1 leaves empty may go to
Haiku (below):

| # | Tool | Caption |
|---|---|---|
| 1 | Bash, Monitor, Gemini `run_shell_command` | its `description` |
| 2 | Read, Edit, Write, MultiEdit, NotebookRead, NotebookEdit (Gemini `read_file`, `write_file`, `replace`) | "Read" / "Edit" / "Write" and the path (`file_path`, else `notebook_path`) |
| 3 | Grep / Glob | `Search for "<pattern>"` / `Find files <pattern>`, the pattern at most 40 runes |
| 4 | WebFetch / WebSearch | `Fetch <host>` (the host only) / `Search the web: <query>`, the query at most 50 runes |
| 5 | Agent, Task | its `description` (the hawk's caption) |
| 6 | ToolSearch, Skill, TaskStop, ScheduleWakeup, Monitor with no description, ... | a fixed phrase: "Load tools", "Use skill <name>", ... |
| 7 | `mcp__<server>__<tool>` | `<server>: <tool>` in words ("slack: post message"); an MCP input is never read |
| 8 | a Bash call with no description (Codex's, always) | **Haiku**, late (below); until it answers, or with Haiku off, empty |
| 9 | anything else | empty: the tool word stands |

Every caption is scrubbed (`scrubCaption`): one line with control characters dropped and whitespace
collapsed; `toollatency.RedactText`; a URL cut to its host; emails removed; an absolute or `~/` path shown
inside a repo checkout (a `work` or `workspace` directory) as the path inside it (its last two segments when
the whole caption would overflow), and anywhere else as its basename, while a URL route such as `/w/` (at
most two segments, no dot in the last) is kept; a run of 32 or more `[A-Za-z0-9_-+/=]` that is not a repo
path becomes `…`; a pair of quotes round the whole is dropped; then a cut at a word boundary to **80 runes**
ending in `…`. Bead ids are kept for an unrestricted viewer and **masked to "a bead" for a scoped one**
(on `activity`, `doing` and the cut document), since an id may name another project's bead. For a scoped viewer
`maskScoped` also takes out a dollar amount ("an amount": fleet spend, which the cut never sends), an ARN, an
`*.amazonaws.com` host, a cluster host (`*.svc`, `*.cluster.local`, `*.internal`, `*.local`), an IPv4 address
and a 12-digit AWS account (gd-Kvh3fdmFg0).

**The Haiku fallback** (step 3, gd-TBAYacR31x; spec section 8; `internal/gardenhub/captioner`). A gardener's Bash
PreToolUse with no caption (never a pool shell's, never a sidechain call) is asked of Haiku
(`AGENT_DEFAULT_HAIKU_MODEL`, `anthropic/claude-haiku-4-5`, through LiteLLM with
`x-litellm-tags: component:beads-viewer,feature:bird-captions`). Haiku is sent **only the scrubbed command**
(RedactText, paths reduced as above, a leading `cd <dir> &&` and scratch-variable assignments stripped, at most 400
bytes) and a fixed prompt, never the cwd, the session, the agent or its task. Its reply is untrusted: it passes the
same scrubber, and one that is empty, multi-line or over 80 runes is dropped. **A bird never waits**: a cached
caption goes on the start at once, otherwise the start goes out with no `cap` and Haiku's caption arrives as one
`event: doing` for that gardener (the same cut and view rules as `activity`), and on the transcript row. Bounds, every
one counted when it bites: an LRU of 4096 entries (24 h, keyed by the command's SHA-256), 30 calls a minute farm-wide,
4 in flight, a 3 s timeout with no retry, 5000 calls a UTC day, `max_tokens` 32 at temperature 0. Expected spend is
about $0.40 a day (about 8% of Bash calls have no description); the daily ceiling holds it near $2. Switches:
[Captions: switches, bounds, metrics](#captions-switches-bounds-metrics).

`gb hook relay` cuts `tool_input` to 1024 bytes of JSON with its keys sorted, which lost a Bash
`description` (after `command`) on 14% of calls and every long Write's `file_path` (after `content`). So
on PreToolUse it copies those two into `tool_description` and `tool_file_path` first (each RedactText'd,
at most 256 bytes; `cmd/gb/hook_relay.go`), and the hub prefers them. From a relay older than that, the hub
reads them from `tool_input`, and from a `tool_input` cut to a string it still reads a field whose value
ended in the kept bytes (an Agent's `description` sorts first).

**Classes** (`doing`, a bounded domain; by tool, never by purpose):

| class | from |
|---|---|
| `read` | Read, Grep, Glob, LSP, NotebookRead, ToolSearch |
| `edit` | Edit, Write, MultiEdit, NotebookEdit |
| `test` | Bash `go_test`, `cargo_test`, `npm_test`, `make` |
| `build` | Bash `go_build`, `cargo_build`, `docker_build` |
| `git` | Bash `git_*`, `gh_*` |
| `beads` | Bash `kd_*`, `gb_*` |
| `deploy` | Bash `helm_*`, `kubectl_*`, `aws_*` |
| `web` | WebFetch, WebSearch |
| `delegate` | the Agent (or Task) tool; SubagentStart until its SubagentStop |
| `shell` | any other Bash class, and any other tool (MCP tools included) |
| `think` | a thought while no call or subagent is out |
| `say` | what the gardener said: an assistant text block, scrubbed as a caption is. No tool; `dur` 0; a row of its transcript at once (below). Flies a bird (the robin) but is no call: the page counts none for it |
| `waiting` | a call started 5 s ago or more with no finish. Derived: never in an `activity` event |

**The SSE `activity` event** on `/garden/farm/events`, between the patches. It has no `id:` line, so it
never moves `Last-Event-ID`: it is outside the versioned patch chain, and a reconnect or a reset does not
replay it. About 60 bytes:

```jsonc
{ "id": "gd-gWork1", "doing": "test", "tool": "go_test", "at": 1792870452, "dur": 0, "cap": "Run the gardenhub tests" }
// a fold of several events (see below) may add "n": 3
```

- `id`: the gardener's agent bead id (`gardeners[].id`). `at`: unix seconds the event happened.
- `dur`: `0` when the call started (PreToolUse, SubagentStart, or a thought); else how long it took in ms,
  at least 1 (PostToolUse / PostToolUseFailure: `duration_ms`, else from its PreToolUse; SubagentStop:
  from its SubagentStart).
- `cap`: the caption, at most 80 runes, omitted when empty. Only on an event whose calls started (`n` >= 1,
  or a call's start alone): a finish, a thought and the hawk's take-off or landing carry none. A fold
  carries the caption of **its newest start**, not of its newest event. A `say` carries the start of what
  was said, at the same 80 runes (the row keeps 280); the captions switch does not drop it.
- **Coalesced to at most 4 a second per gardener.** The first event after a quiet 250 ms goes at once;
  the ones inside the next 250 ms fold into one event, sent when the 250 ms is up, that carries the
  newest event's fields. `n` is then the calls started in the fold, set only when it is not what the
  event alone says. So **the calls an event started** are `n` when present, else 1 when `dur` is 0 and it
  is a call (not `think`, not tool `subagent`), else 0. That is the number of birds (4b); a `subagent`
  event is the hawk taking off (`dur` 0) or landing.
- **Cut per viewer, like `gardeners[]`.** An event is sent only for an id in the current `gardeners`
  set (never for a pool shell, an agent on its way out, or one past the gardener cap), and only to a
  viewer whose cut keeps that gardener (`Handler.SeesGardener`). An unrestricted viewer keeps every
  gardener; a scoped viewer (the route is `routeScoped` since gd-l7yYxZpoZo) keeps only the gardeners its cut keeps
  (Authorization below). A slow
  viewer's queue (64) drops new events rather than block.
- **Then cut to the page's viewport** (gd-fBGGNdgrLc). The stream opens with `event: hello`,
  `{"sid": "<random>"}`. The page (`web/viewcut.js`) POSTs its view for that stream to
  `/garden/farm/events/view` as `{"sid", "level", "ids"}`, again whenever the view changes (at most every
  300 ms while the camera moves) and at once on a new stream. The answer is 204, or 404 for a stream that
  is gone. Until the first view lands the stream sends every call the cut keeps, as before. A view only
  narrows the cut, never widens it:
  - **Plan and Bead level** (`level` >= 2): only the calls of the gardeners named in `ids`. The page names
    the gardeners standing on a plot inside the view, or within a quarter of the view's size round it, so
    a call heard while the page catches up with a pan still gets its bird. At most 256 ids are kept, and
    an id that is not a gardener is ignored, never an error. Each named gardener that was not in the last
    view gets one `event: doing`, `{"id", "class", "tool", "at", "calls", "cap"?}` (its `gardeners[].doing`), so
    it is posed before its next call.
  - **World and Project level** (`level` < 2): no calls at all. Every 5 s, `event: busy`,
    `{"busy": {"<project>": <s ago>}}`, lists the projects (of gardeners the cut keeps) heard since the last
    one, and nothing is sent when none were. When the view drops to this level, one is sent at once for
    the last 30 s. `BIRDS.sign` needs only "busy in the last 30 s".
  - Cost, measured by `TestActivityViewportBytes` with 60 gardeners at 4 calls/s, every start captioned
    (the spec's section 2.3 samples, median 36 runes as on the live hook stream): an Everyone page, with no
    view or framing the whole farm at the Plan level, gets 9.7 KB/s on the stream budget below (29.8 KB/s
    without it; 6.9 KB/s with no captions). A 10-plot Plan view gets 7.9 KB/s in the worst case, where
    every plot in view and in the margin has a busy gardener (16 named; 5.6 KB/s with no captions). A
    Project-level page gets 25 B/s. The test asserts the captions' ceilings, 12 KB/s and 10 KB/s; over
    them the wire caption drops to 48 runes (`beadsViewer.captions.wireCap`, `FARM_CAPTION_WIRE_CAP`) before
    anything else changes; the transcript keeps the full 80. The live figure is the
    beads-viewer metric `gardenio_farm_activity_bytes_total` (bytes as written, framing included), with
    `_events_total`, `_dropped_total`, `_views_total` and the gauge `gardenio_farm_activity_clients`.
- **Then folded to a budget per stream** (gd-iAq4va9XDt), so no page's cost grows with the number of
  gardeners in its view. Each gardener's calls fold (as above) over one of five gaps: 250 ms, 500 ms, 1 s,
  2 s or 4 s. Each stream is put on the shortest gap at which its busy gardeners (those in its view, or
  every gardener its cut keeps before a view lands, heard in the last 10 s) come to at most 64 events a
  second. The gap is chosen again every second and whenever the view changes. So up to 16 busy gardeners
  keep 250 ms, a whole farm of 60 folds over 1 s, and a full 256-id view over 4 s. A longer fold changes
  nothing on the wire: it is the same event with `n` the calls started in it, and the tool word of the
  newest event. `birds.js` flies it as a flock of up to 3 and keeps at most 24 birds live, so a busy farm
  still fills the sky (`world_frame_test.js`, the `birds_folded` sweep). A gardener calling slower than its
  stream's gap is sent every call as before. Streams per gap and the bytes of calls sent at each are
  `gardenio_farm_activity_fold_clients{fold}` and `gardenio_farm_activity_call_bytes_total{fold}`, with
  `fold` one of `250ms`, `500ms`, `1s`, `2s`, `4s`.
  - Honest limit: a card or outline row for a gardener out of view shows the last doing heard (its age
    grows) until the gardener comes into view again.

**`gardeners[].doing`** on the document and in a reset: `{class, tool, at, calls, cap?}`, the last thing
heard, so a fresh page poses each gardener before its next call. `calls` counts the calls started this
session (a new `session_id` starts it again). `cap` is the caption of the newest call started (absent when
it had none), also on `event: doing`. `class` is `delegate` while a subagent is out, and `waiting` while the
newest event is a call 5 s old or more with no finish. The key is absent until the hub has heard from
the agent (as on every viewer restart), and it is an added key under `meta.schema=1`, so older clients
ignore it. **A patch never carries `doing`**: the entry in `gardeners.up` is the bead's, without it. A
client keeps doing beside the entry (`web/activity.js`), seeds it from a load or a reset, and moves it on
with each `activity` event: `class`=`doing`, `tool`, `at`, `calls` += the calls it started. A client
derives `waiting` itself from a start 5 s old. The document bakes `doing` in at most every 2 s at one
version; the `/garden/farm` ETag (`W/"f<v>-a<epoch>"`) names the bake.

**On the page.** `web/plans.js` dispatches each event as a `farmactivity` DOM event; `web/activity.js`
holds the doing per gardener and gives the card its line, `doing: go test · 4 s ago · 212 calls this
session`, the pose a gardener at work wears (4c; `homestead-agents-as-people.md` section 5.1) and its word
in the outline. `Farm.Stats()` counts `activity_clients`, `activity_sent`, `activity_bytes` and
`activity_dropped`, for measuring the SSE bytes per page (4d); no route serves it yet.

### Transcript (what a gardener did)

Step 7 of plan gd-jMl4oF2cJO (gd-MsTDi7JYVs; bird captions spec
`docs/superpowers/specs/2026-10-28-bird-captions-design.md` sections 11.1-11.3 and 12;
`internal/gardenhub/transcript.go`). The panels that read it are steps 8 and 9.

**Storage.** Each gardener in the current `gardeners` set keeps a ring of its last **200** finished calls
(`beadsViewer.transcript.rows`, `FARM_TRANSCRIPT_ROWS`, 1 to 1000; the one-gardener route's `limit` follows it) in the
hub's memory, beside its activity state. Nothing is written to a bead (about 14k calls a day; `HOOK_EVENTS` and the
toollatency ledger are the durable records). A restart empties every ring, a new `session_id` clears the gardener's,
and it is forgotten with the agent (an hour unheard once it is no gardener). A pool shell, or any agent not in the
set, keeps none. About 135 bytes a row on the wire; at most 200 rows per gardener in memory.

**A row is written when its call finishes**, so it has its duration and outcome:

```jsonc
{ "at": 1792870452, "class": "shell", "tool": "other", "cap": "Trial-merge main into #1282…", "dur": 5120, "ok": true }
```

- `at`: unix seconds the call **started**. `class` and `tool`: as on the `activity` event.
- `cap`: the call's caption at its full 80 runes, omitted when it had none. A Bash call whose caption Haiku is
  still writing waits for it, up to 3.5 s from its start (the captioner's 3 s timeout and a margin), before its row
  is written; a caption that lands after the call's start went out (a late `doing`) is on the row, whether or not
  the call is still the newest. Past the wait, the tool word stands.
- `dur`: ms, at least 1 (`duration_ms`, else from its start). `ok`: `false` for `PostToolUseFailure`.
- A call that never finishes is written when it is let go (30 min), with `dur` 0 and no `ok`. A finish heard without
  its start (after a restart) is written with no caption, `at` its finish less `dur`.
- A thought, a sidechain call and the hawk's SubagentStart/Stop are not rows; the Agent tool call itself is.
- **What the gardener said is a row at once** (class `say`, gd-HHGjxt59Mr): each assistant text block coop forwards
  (`coop.events.messages.*.assistant_text`), scrubbed as a caption is (paths shortened, secrets and emails out, one
  line) and cut at a word to **280 runes** as `cap`, with `tool` empty, `dur` 0 and no `ok` (it has no finish; the
  panel says neither a duration nor "unfinished"). It is written whether or not a call is out, since a turn's text
  comes just before the calls it makes. About 300 bytes a row at most; the `FARM_CAPTIONS` switch leaves it, as the
  text is the row, not a caption of it.

```jsonc
{ "at": 1792870460, "class": "say", "tool": "", "cap": "Pushed the fix; CI is green. Next I will…", "dur": 0 }
```

**Endpoints** (`routeAdminOnly`: they have no per-viewer cut of their own, though `/garden/farm` is `routeScoped`; a
scoped viewer's transcript rows ride its own farm stream, cut like its activity events). `Cache-Control: no-store`, JSON:

```jsonc
// GET /garden/farm/activity/{id}?cursor=<next|prev>&since=<unix>&before=<unix>&limit=<n ≤ 200, default 200>
{ "id": "gd-gWork1", "rows": [ /* rows, newest first, without id */ ], "more": true,
  "next": "o1792870452.8812", "prev": "n1792870460.8840" }
// GET /garden/farm/activity?scope=farm|project:<p>|plot:<id>&cursor=&since=&before=&limit=<n ≤ 100, default 100>
{ "scope": "project:gardenio", "rows": [ { "id": "gd-gWork1", "at": ..., ... } ], "prev": "n1792870460.8840" }
```

- Newest first by `at`, then rows written later first within one second. Each row also has a place no other row
  shares (its `at` and a number stamped when it was written, across every gardener; not on the row), and paging
  goes by it, so **a page cut inside a second loses no row and repeats none**, at farm scope too (gd-LEmM2G7iaa).
- **Paging back: `next`.** Set when older rows than the page matched; ask again with `cursor=<next>` for the `limit`
  rows just older than the page. `more` is set when more rows matched than `limit` (with no cursor or `next`'s, the
  same as `next` being set).
- **Catching up: `prev`.** Set whenever the page has rows; `cursor=<prev>` asks for the rows newer than the page, the
  `limit` nearest it (`more` then says newer ones still past them: ask again with that page's `prev`). The page above
  the newest is empty and hands back the same `prev`, to ask again later. This is how the panel loads the rows a
  `call` event's `skipped` counted. A row is placed by when its call started, so a long call that finishes after a
  page was read lands among older rows, behind that page's `prev` (as it fell before `since`); the `call` event
  carries it.
- Cursors are opaque: hand back what an answer gave. A restart empties every ring, and a cursor from before it then
  finds nothing older.
- `since` and `before` still work, alongside a cursor or alone: `since` keeps rows that started at or after it,
  `before` rows that started strictly before it. Paging with `before` set to the oldest `at` held drops the rest of
  that second's rows when a page cut into it; prefer `next`.
- A bad number, a cursor no answer gave, `limit=0`, or a scope that names one gardener on the scope route is a 400.
- `plot:<id>` covers a gardener whose plot is that plan's (`gardeners[].plot.plan`) or whose task is that id.
  `scope` defaults to `farm`.
- **Cut per viewer, exactly like `gardeners[]`**: only gardeners `SeesGardener` keeps and, for a project-scoped
  viewer, its cut's gardeners (the same set as its stream's activity). One gardener the cut does not keep is a
  **404**, the same as an id that is no gardener, never a 403 that would say it exists. For a scoped viewer every
  bead-id-shaped token in `cap` is masked to "a bead", and amounts, hosts and accounts as `maskScoped` does (spec
  12), as on `activity`.

**Live rows: `event: call`** on `/garden/farm/events`, only to a stream whose page holds a transcript open. The
page POSTs it on the view endpoint, beside or instead of its view:

```jsonc
// POST /garden/farm/events/view
{ "sid": "<hello's>", "transcript": "gd-gWork1" }   // or "farm", "project:<p>", "plot:<id>"; "" closes it
```

A body with `transcript` and no `level` or `ids` leaves the stream's view as it was; one stream holds one
transcript, and a new one replaces it. 204, 404 for a stream that is gone, 400 for a transcript that names nothing
(an empty project or plot, a value past 128 bytes or with whitespace). The stream is then sent the rows written
from then on (what was written before is the endpoint's), for gardeners in the scope **and** in its cut, masked as
above, whatever its view:

```jsonc
// event: call — at most one a second
{ "rows": [ { "id": "gd-gWork1", "at": 1792870452, "class": "read", "tool": "Read", "cap": "Read web/birds.js", "dur": 12, "ok": true } ], "skipped": 4 }
```

- Batched to **one event a second** with **at most 20 rows**, newest first. Past 20, the oldest go and `skipped`
  counts them (absent when 0): the panel says "n more calls" and loads them with its newest page's `prev` (or `since`).
- **Cost**, measured by `TestTranscriptCallBytes` (60 gardeners at 4 calls a second, every call captioned): a
  stream holding the farm's transcript gets 20 rows a second at about 134 B a row, **2.7 KB/s**; the test asserts
  3 KB/s. A stream with no transcript open (every farm page that has not opened a panel) gets **no `call` bytes**:
  its budget above is untouched. Live: `gardenio_farm_transcript_bytes_total`, `_rows_total` and the gauge
  `gardenio_farm_transcript_clients` (beads-viewer).

**The panel** (`web/transcript.js`, the Workbench's transcript kind; steps 8 and 9, gd-kl2CL3X9jQ and gd-X5dM2VRgDd):
`/garden?transcript=<id>` is one gardener's ring in one page; `/garden?transcript=farm|project:<p>|plot:<plan>` is the scope
route's first page (one request, at most 100 rows), merged newest first with each run of one gardener's rows headed by its
name (which opens its own transcript) and its plot's plan arms. Show older asks with `next`; a `call` event's `skipped` is
loaded with the newest page's `prev` (at most three pages); never `since` or `before`. It holds at most 300 rows: past that,
live rows push the oldest out and Show older goes with them. It is reached from a gardener's card and the Glass (one
gardener), a mapped plan's face ("Every call on its plot"), the project panel's Agents tab, and the scope panels' own links up
(plot, its project, the farm). `TestTranscriptFarmPanel` holds the first screen of 60 gardeners at 4 calls a second to 100 rows
and 160 B a row, and walks a quiet plot's ten minutes in two pages.

### Captions: switches, bounds, metrics

Step 10 of plan gd-jMl4oF2cJO (gd-FOzMlrXga6; spec sections 8 and 14). beads-viewer reads these at start
(`gardenhub.CaptionSettingsFromEnv`, `captioner.ConfigFromEnv`); the chart renders them from its values, and a value
that does not parse, or is out of range, keeps the default. The spec calls the values `farm.captions.*`; they live
under `beadsViewer` because the viewer is what makes them.

| Helm value | Env | Default | What it does |
|---|---|---|---|
| `beadsViewer.captions.enabled` | `FARM_CAPTIONS` | `true` (`on`) | **Kill switch for every caption.** `off`: no `cap` on `activity`, `doing` or `gardeners[].doing`, none on a transcript row, no caption counted and no Haiku call. The wire is as it was before captions, and each bird keeps its tool word. |
| `beadsViewer.captions.wireCap` | `FARM_CAPTION_WIRE_CAP` | `80` | Runes of `cap` on the wire (1 to 80). The transcript keeps 80. |
| `beadsViewer.captions.haiku.enabled` | `FARM_CAPTION_HAIKU` | `false` (`off`) | **Haiku kill switch.** Renders `on` only with `captions.enabled` and `haiku.secretName` both set; off, captions stay deterministic. The k3s deploy turns it on. |
| `beadsViewer.captions.haiku.secretName` / `secretKey` | `FARM_CAPTION_LITELLM_KEY`, `LITELLM_URL` | `""` / `api-key` | The scoped LiteLLM virtual key (claude-haiku-4-5 only, $3 a day, rpm 30; k3s: `gardenio-beads-viewer-litellm`), and the NetworkPolicy legs to LiteLLM. Without it Haiku is off. |
| `beadsViewer.captions.haiku.ratePerMinute` | `FARM_CAPTION_HAIKU_RATE_PER_MINUTE` | `30` | Calls a minute, farm-wide (a token bucket). |
| `beadsViewer.captions.haiku.concurrency` | `FARM_CAPTION_HAIKU_CONCURRENCY` | `4` | Calls in flight. |
| `beadsViewer.captions.haiku.timeout` | `FARM_CAPTION_HAIKU_TIMEOUT` | `3s` | Per call, no retry. |
| `beadsViewer.captions.haiku.dailyMax` | `FARM_CAPTION_HAIKU_DAILY_MAX` | `5000` | Calls a UTC day. |
| `beadsViewer.captions.haiku.cacheSize` | `FARM_CAPTION_HAIKU_CACHE_SIZE` | `4096` | LRU entries. |
| `beadsViewer.transcript.rows` | `FARM_TRANSCRIPT_ROWS` | `200` | Rows a gardener's transcript ring keeps (1 to 1000). |

**A run of failures pauses Haiku** (gd-k0v6m9C43L; `captioner.go`, not a Helm value): 10 calls in a row with no answer
(an error or a timeout) pause the calls for 5 minutes, during which a Caption is skipped as `paused`
(`gardenio_farm_caption_haiku_skipped_total{why="paused"}`) and reaches LiteLLM not at all; after the pause one call
goes, and a failure pauses again while an answer clears the run. The first failure of a run and every 50th after it
are logged by beads-viewer ("farm captions: Haiku call failed", keys redacted), where before they were only counted.

The plate's linger (6 s) and its size stay constants in `web/birds.js`: they are presentation, not cost. The viewer
logs what it started with (`farm captions on=… haiku=… wire_cap=… transcript_rows=…`).

**Metrics** (beads-viewer): `gardenio_farm_caption_total{src}` (`description`, `path`, `pattern`, `host`, `agent`,
`builtin`, `mcp`, `haiku`, `cache`, `none`), `gardenio_farm_uncaptioned_total{tool}` (a start the hub made no caption
for, before Haiku, by tool: a name `captionOf` knows, `mcp` or `other`; gd-Ty2eNoZipX), the histogram `gardenio_farm_caption_haiku_seconds`,
`gardenio_farm_caption_haiku_skipped_total{why}` (`off`, `rate`, `concurrency`, `daily`, `timeout`, `error`),
`gardenio_farm_caption_haiku_tokens_total{type}` (`input`, `output`: what each reply said it cost), and the
transcript's `gardenio_farm_transcript_rows_total`, `_bytes_total` and `gardenio_farm_transcript_clients`. They are
charted on Grafana's **Gardenio Garden Health** board, row "Farm: bird captions" (board only; panel ids 14-18,
`grafana_garden_health_farm_captions_test.go`): caption sources, Haiku latency against the 3 s timeout, Haiku spend
over the window (tokens at $1 and $5 per million; LiteLLM's spend for `feature:bird-captions` is the bill of record),
Haiku calls skipped, and transcript bytes per viewer against the 3 KB/s budget.

### FarmSeed (frosted, ready or asked)

`{id, title, state, status, project, operator, priority, created, updated, phases, tasks, decision?, steps?, summary?, short_title?, cost}`.
`cost` is the slim plan cost `{has, usd, crew?}` (Plan cost under Costs): a seed may have had a
planning crew.
`state: "frosted"` is `plan_state=draft` (still being authored); `"ready"` is
`plan_state=ready` (waiting for "Approve & build"); `"asked"` overrides either while
the plan's decision is open (waiting on its owner's answer). `phases` is the length of
`phase_order`, `tasks` the number of open steps authored so far. A frosted seed
carries counts only, no steps and no narrative, so 20+ drafts stay cheap.

`steps` and `summary` (staked plots step 1, gd-AXIwI51ejX) are set only on a `ready`
or `asked` seed, so its pot, tip and bead view can read the plan before it is approved.
`steps` is `[{id, title, n, status, heraldry?}]`: the plan's open work beads (`task`,
`feature`, `bug`, `chore`, under the plan or one of its phases; a decision or escalation
is not a step) in the plan's `step_order`, those it does not list after them by
`created_at`, at most 24. `n` numbers them from 1 in that order, `status` is the bead's
status and `heraldry` its arms when it bears any. A closed step is not in the work rows,
so it is not listed. `summary` is the plan epic's description, trimmed and cut at 600
runes (an ellipsis ends a cut one). The work rows omit descriptions (`farmRowOmit`), so
the farm reads them in one `BatchGetBeads` for the ready or asked seeds whose
`updated_at` it has not seen, and keeps each until the plan changes or stops being such
a seed (`farm_seeds.go`); a failed read leaves `summary` empty and is tried again next
refresh. Both fields are additive and omitted when empty, so `meta.schema` stays 1.
`short_title` (staked plots step 3, gd-53IHFkfPLk) is the plan epic's short title, trimmed, on a `ready` or `asked` seed
only: what its staked plot's sign reads (homestead-world-design.md 26.14). Empty, and omitted, when the plan has none.

`decision` (9.11b, gd-m38yKRX0hE) is the open decision that names the plan in its
`requesting_plan` (else `requesting_task`), the newest when there are more: in a Land,
the steward's "Plan <id>: <title> - run it?" that `steward.WritePlan` blocks the plan on.
`{id, prompt, options: [{id, text, label?}], for?, since}`: `prompt` drops the
"Plan <id>: <title> - " the pot already says ("Run it?"), `options` are the answer
buttons as the Mailbox reads them, `for` is the decision's `operated_by` (lower-case)
and `since` its `created_at`. The work rows omit a decision's `prompt` and `options`,
so the farm reads them once per decision (`BatchGetBeads`, cached by id until the
decision closes, `farm_seeds.go`); a failed read leaves the title as the prompt and no
options (the card offers a written answer) and is tried again next refresh.

On `/garden` a pot's tip (hover or focus on a desktop, the first tap on a tablet) and
the bead view of the seed (a phone's tap; its "In the Nursery" section comes first) show
the plan's title, its state in words (drafting, waiting on your answer, ready) and the
decision's card: the Mailbox's own answer box (`BEAD.answerBox`), which `web/answer.js`
answers through the same `POST /b/{id}/decision/resolve`. There is no "Mark ready" or
"Approve & build" in the garden. An approve leaves the steward to move the plan to
`building` (`gb lands plan answer`), and the next patch is `seeds.rm` + `plans.up`.

### Meadow

Loose work beads (`task`, `feature`, `bug`, `chore`) that reach no plan: no
parent, or a parent that is not a plan or one of its phases. `total` is exact
for the rows read (see `meta.truncated`); `shown` is the newest 40. `with_mr`
counts the "orphaned with a PR" ones (their `mr` field is set). Open decisions and
escalations that belong to no plan are mailbox letters, not meadow plants, and
are not in this document (the existing `/garden` page lists open decisions);
their count per project is in `home` (below).

### Wishes

A wish is a thread head: the `type=task` bead with the `thread` label that a
Slack or Commons thread opens (`internal/bridge/thread_bead.go`). Everything
the thread gave rise to (steps, plan epics minted from it) carries
`fields.thread_id` = the head's id; the head itself carries none. **A head is
never a plant**: it is not a meadow flower, not a step on a plot, and the weeds
rules (`unlabelled`, `wilt`) do not see it. It is a wish (gd-EGpbDpqsec; the
goats of Phase 2 are drawn from this blob).

```jsonc
"wishes": {
  "open": [{                        // newest asked first, at most 40 per project
    "id": "gd-wish1", "title": "exchange behind the edge",   // the short title when the head has one
    "project": "gardenio", "source": "slack",                // source: slack | commons, from the source:<name> label
    "asker": "oidc:matt@example.com",                        // lowercased operated_by: who asked
    "opened": 1792000020, "updated": 1792000310,
    "kin": ["gd-A1a", "gd-A9"],      // open rows whose thread_id is this wish, newest change first, at most 12
    "kin_total": 3, "kin_closed": 0, // exact over the rows read / over the kept closed rows
    "plots": [{ "plan": "gd-planA", "phase": "gd-A1" }, { "plan": "gd-planA", "phase": "" }], // distinct, in kin order
    "tending": true,                 // some kin is in progress or in review: solid, not a ghost
    "arms": [{ "canonical": "🧭🔥📦", "n": 3, "name": "arms: compass, fire, package" }],   // its kin's line arms; omitted when none
    "href": "/garden/c/bead/gd-wish1"                        // its thread in the Commons
  }],
  "open_total": 2,                   // exact over the rows read
  "open_more": { "gardenio": 8 },    // per project, past the cap; omitted when nothing is left out
  "closed": [{                       // newest close first, at most 24 per project
    "id": "gd-wishx1", "title": "Title gd-wishx1", "project": "gardenio", "source": "slack", "asker": "oidc:matt@example.com",
    "closed": 1792000035,            // closed_at, else updated_at
    "kin": 1,                        // open and closed rows that carry its thread_id
    "outcome": "expired",            // done | abandoned | expired
    "reason": "slack-thread-idle",   // close_reason, else triage_reason; omitted when neither is set
    "produce": [{ "id": "gd-X1k", "kind": "mr", "title": "Idle threads close themselves", "url": "https://example.com/mr/77",
      "iid": "#77", "state": "merged", "ready": true, "task": "gd-X1k", "updated": 1792000025 }],   // its kin's merged MRs, newest first, at most 6
    "produce_more": 0,               // merged MRs past the 6; omitted when none
    "href": "/garden/c/bead/gd-wishx1"   // its thread in the Commons
  }],
  "closed_total": 129                // the daemon's count of closed heads
}
```

- `kin` and `plots` are what a goat grazes by: a kin inside a seed (a draft or
  ready plan) has no plot, as a gardener on such a task has none; a wish whose
  kin are all in seeds, or that has none, has no `plots`.
- The page draws each open wish as a goat (`web/goats.js`, steps 2.1
  gd-ODxztYntyd and 2.2 gd-kqTYEsMvhv): it laps the path round the first of its
  `plots` that is on the map and every other one of them in that plot's project
  (the ring round the box of those plots), now and then turning up a gutter
  between the beds where its body fits (the Bead level from 1.5x). The goats of
  one ring are spread evenly round it and keep their distance; none grazes on a
  bed, under a gardener standing on a plot, or on the ground that gardener's
  birds fly out over. Plan and Bead levels only, at most 12 grazing in view (the
  newest), the rest counted on their plot's sign as "+N wishes"; at the Project
  level a project's sign says how many wishes it has open (`open` plus
  `open_more`), "3 wishes". Reduced motion: each goat grazes still at its place
  round the ring.
- Every listed open wish also has a cell in the pen, a fenced band under the
  Yard (step 3.1, gd-kVKuWqIK1v; `homestead-world-design.md` section 26.10): the
  land's `open` wishes in the document's order (newest asked first), at most 63.
  A wish with no plot on the map grazes in its cell; one out grazing by its kin
  leaves an open gate there. The pen's sign counts the wishes past its cap and
  `open_more` as "+N not in the pen".
- `tending` is true while any kin is `in_progress` or `review`.
- `arms` are the line arms of its kin (heraldry 5.10, gd-WUqr6IkgkM): each kin's
  nearest armed ancestor's, as the Yard's rows show them (`line_arms.go`), read
  through the hub's line-arms cache (`Farm.ShareLineArms`). The first armed
  kin's run, then a second distinct run when kin sit in two armed lines, in kin
  order; a wish never bears arms of its own (R6). The goat's card and its pen
  gate's (`GOATS.card`, drawn by `web/stip.js`) show them before the wish's
  title; the goat sprite wears none. A headstone shows none: a closed wish's row
  carries a kin count, not its kin.
- `outcome` is read from the fields the bridge stamps at close
  (`thread_task_autoclose.go`, `bot_reactions_wish_close.go`): `expired` for an
  idle autoclose (`close_reason` `slack-thread-idle` or
  `slack-thread-idle-backfill`; historical only, as the idle auto-closer has
  only hinted since gd-Zhcpn9IZVl); `abandoned` when the head was marked
  needs-triage because its agent failed (`triage_reason`) or the close reason
  says failed or abandoned; `done` otherwise (a reaction on the card, `kd close`,
  an agent finishing).
- `reason` and `produce` are the headstone's hover (step 3.2, gd-s9KzRk6boh):
  why it closed, as the bridge wrote it (the page says the bridge's slugs in
  words), and what its kin merged. `produce` comes from FarmProduce's read
  (`farm_produce.go`, the MR rows across every status): the merged MRs whose
  row carries this wish's `thread_id`, parented or loose, one per URL. A farm
  with no plan reads no produce, so its stones list none.
- The page draws each closed wish as a headstone in its project's graveyard, a
  band between the Orchard and the plots (`homestead-world-design.md` section
  26.11).
- The page's jump list (the camera column's Go, or `/`) lists the pen, each
  penned goat, and each project's graveyard, so a phone reaches them without the
  arrows (step 3.3, gd-NbWjIdLTnd; section 26.12). A goat or a stone is found by
  its wish's `id`, its title, or "wish".
- Cost: the open heads and their kin come from the open rows the fast tier
  already reads (the head by its label, the kin by their own `thread_id`), the
  closed heads and closed kin from the closed rows the slow tier keeps, so
  `kin_closed` and the listed stones cover the newest 2000 closed rows. The
  one added daemon call is the count of closed heads, in the slow tier with
  the other counts. Nothing is read per wish.
- A wish's own conversation (its replies, squawks, cards) is not in this
  document: the bead view reads it on a click ([The bead view's
  kin](#the-bead-views-kin-get-gardenbeadid)), and the steward's node reads a
  summary of each open wish's kin in one list (`kin.SummaryQuery`).

### Home

Each biome's home strip in counts (`homestead-world-design.md` section 28,
gd-GYo4PWlGUb; `farm_home.go`): one `FarmHome` per project key, `""` for the
Commons (the items with no project). A project's sign at the Project level says
"3 letters · 2 growing · 4 at the barn" from it without walking the ledger, and
`layout()` reads from it which projects have home items and so are biomes
(28.3). A key whose counts are all zero is left out, so a missing key is a strip
with nothing in it.

```jsonc
"home": {
  "gardenio": {
    "letters": 3,   // open decisions and escalations with this project: label, on a plan or not (the Mailbox)
    "field": 5,     // loose work beads in any open state, exact past the meadow's 40 (the Growing field)
    "growing": 2,   // of field, those in progress or in review: drawn growing, the rest as wildflowers
    "barn": 4,      // gardeners at home in this barn
    "wishes": 7     // open wishes, listed and open_more (the Pen)
  },
  "": { "letters": 1, "field": 0, "growing": 0, "barn": 2, "wishes": 0 }
}
```

- **Letters** are counted over the rows read, status `open` only, as the hub's
  `decision` section lists them; an in-hand escalation (`inhand` on its ledger
  row) counts, since the Mailbox shows it. A letter past the row cap
  (`meta.truncated`) is not counted.
- **Field and growing** count the meadow's rows (a loose `task`, `feature`,
  `bug` or `chore`), not a plan step: a bead on a plot is drawn on its plot only
  (28.2). An in-progress ledger `work` row the document places nowhere (a step
  past its phase's 60-task listing) is not counted.
- **Barn** follows the page's `gardenerSpots`: a gardener is at home unless it is
  alive or stuck with a plot (then it is out on that plot, and its ghost is not
  counted) or it is a steward with a project (it stands at its land's sign). A
  thread agent (role `thread` or `dm`) is in the Commons's barn whatever its
  project (28.4, H-D10); every other gardener is in its own project's barn, a
  gardener with no project in the Commons's.
- **Wishes** is the project's open wishes: those in `wishes.open` plus its
  `wishes.open_more`.
- **The ledger says the same projects.** Every row of the `/garden` page's
  ledger (`GET /garden/state`, `/garden/events`) carries its project as
  `data-pj` on its `<li>`, in every section: `decision` (decisions and
  escalations), `work` and `agent`. It is the `project:` label; an agent bead
  with none takes its `fields.project`, as a gardener here does, so a gardener
  and its ledger row stand in the same barn. `""` means no project: the Commons.
  `hub_test.go` (`TestLedgerRowsCarryProject`) pins it.

### Escalations

```text
"escalations": { "open": [FarmWait], "total": 23, "by_project": { "gardenio": 20, "": 3 } }
```

Every open escalation the farm read, fleet-wide (gd-VyP9DLgvih): in a plan or
not, with or without a project, pending, approved, rejected but open and failed
but open; a closed escalation never. Each is a FarmWait as above, so it carries
`project`, `op`, `family` and `executor`, and its `state`, `human` and
`waits_on` are what the same escalation says in its plan's `waiting[]`. The
Escalations cell reads it (`homestead-world-design.md` 28.13, in the
farmstead's courtyard since 29.2): each project's cell filters `open` by
`project`, and the Commons's cell takes the ones with none.

- **Order**: the waits' order, human waits first, then the longest wait first,
  then id.
- **Bounds**: `open` lists at most 120; `total` is exact for the rows read, and
  `by_project` is it per project as `project` spells it (`""` the unprojected
  ones), so a strip's "+N" crate counts what the cap left out (gd-4FIF5pzC6O).
- **Wire**: a blob, sent whole in a patch when it changed,
  `{"open":[],"total":0,"by_project":{}}` before the first refresh.

### Releases

A release is a `type=release` bead, one tag of one project: the merged PRs it
carries and where it runs, with a status the daemon derives (`cut`, `built`,
`live`, `superseded`; [`releases-design.md`](releases-design.md) §3, plan
gd-UZKZ9H25UD step 5, `farm_releases.go`). The release truck, a plot's shipped
stamp and the signs' release words on /garden read them from here. The blob is
keyed by project. A project with no `release_tag_pattern` on its project bead
has no key; one with a pattern but no release yet has a key with a null
`latest` and no truck.

```jsonc
"releases": {
  "gardenio": {
    // The newest release by cut_at: what is on the truck. null when none.
    "latest": {
      "id": "gd-rel3", "tag": "2026.301.1", "status": "cut",
      "cut_at": 1792009000,          // unix s; 0 when the record has none
      "built_at": 1792009900,        // omitted when not built (or no build step)
      "live_since": 1792010000,      // the earliest open deployment's since; omitted unless live
      "count": 4,                    // PRs it carries (its contents rows)
      "plans": [{ "id": "gd-done1", "count": 2 }, { "id": "gd-seed2", "count": 1 }],  // most PRs first, then id
      "unplanned": 1                 // rows with no plan; omitted when 0
    },
    // What runs on each target now, by target name: the open row of the newest release that holds the target.
    // [] is "no deployments yet", never an error.
    "deployments": [{ "target": "prod", "kind": "k3s", "tag": "2026.300.1", "release": "gd-rel2", "since": 1792007200, "revision": "411" }],
    // The newest 12 by cut_at; plans is the number of distinct plans its PRs came from.
    "recent": [{ "id": "gd-rel3", "tag": "2026.301.1", "status": "cut", "cut_at": 1792009000, "count": 4, "plans": 2 }],
    "shipping": true                 // latest is cut or built: loaded, not yet live
  }
}
```

Go types: `FarmReleases` (a map by project), `FarmProjectReleases`
(`latest`, `deployments`, `recent`, `shipping`), `FarmRelease`,
`FarmReleasePlan`, `FarmReleaseDeployment` and `FarmReleaseRow`
(`farm_model.go`).

- **Deployments here are not the history.** The release bead keeps one row
  per Helm revision per target, closed ones included (`releases-design.md`
  §3.1). The blob keeps only what runs now: for each target, the open row of
  the newest of the 12 releases read that holds it, with that row's `since`
  and `revision`. A re-roll of the same tag at a new revision moves `since`
  and `revision` on and changes nothing else.
- **What a plot shows.** A plan's `shipped` (FarmPlan) is the tag of the
  release whose contents carry the plan's newest merged PR: the first merged MR
  in its `produce[]` (merged MRs come newest first), matched by PR URL. When
  that PR is in no release read, the plan has not shipped yet, even if an older
  PR of it has. A plan whose `produce[]` lists no merged MR falls back to its newest PR
  (by `merged_at`) in any release read. Only a release of the plan's own project
  answers for it. The surfaces read that release's status and targets from
  `recent` and `deployments`; "skipped" (a release never deployed while a
  newer one has run) is a reading they make, not a status. Produce rows carry
  no `release` of their own.
- **How the page reads it** (`web/truck.js` `TRUCK.norm`). Per project it
  builds `{key, project, tag, id, status, shipping, cut, built, live, count,
  plans, unplanned, targets, recent, total, shipped}`. `targets` are the
  `deployments` rows with no `until`. `live` is `latest.live_since`, else the
  earliest `since` of a target running the latest tag. Two keys come from the
  page, not the server: `total` is `recent.length` (so the Project sign's
  "N releases" counts at most 12), and `shipped` is an empty map (a plot's tag
  comes from `FarmPlan.shipped`). `norm` also takes step 1's draft shape
  (`newest`, `targets`, `recent` as tags), the blob as an array or under
  `projects`, and `plans` as `[{id, count}]`, `{id: count}` or ids; the server
  sends none of those.
- **Patches.** `releases` is a singleton blob: a patch carries it only when it
  changed, and then whole (every project's key), like `wishes`. A cut
  (first paint or stream) carries the keys of the set's projects, each whole
  (`cutReleases`).
- **Cost.** The read rides the slow tier at most every 60 s
  (`farmReleasesEvery`), apart from the other slow reads, inside a 3 s budget:
  one list of the live project beads that carry a `release_tag_pattern`, then
  the daemon's `GET /v1/releases?project=&limit=12` per such project (newest
  cut first). Nothing is read per PR: the counts come from each release's
  `contents` rows. A failed read keeps the last one read (a failed project
  keeps its own), and never fails the refresh. Without a release source wired
  (`Farm.SetReleaseSource`; the viewer wires the daemon's) the blob is `{}`
  and no plan has shipped.
- **Bounds.** A target held only by a release older than the newest 12 is not
  listed, and a plan whose newest merged PR shipped in such a release has no
  `shipped`. About 100 bytes per recent row and 300 for `latest` with a few
  plans: under 2 KB per project.

### Piles

Counts only, from the daemon's count query: `closed_decisions`,
`closed_escalations`, `closed_work` (task/feature/bug/chore), `closed_plans`.
Per-phase and per-plan closed counts are on the phases (`counts.closed`) and
cover **direct** closed children of the phase/plan; they do not split by type.
A per-plan escalation archive is not available without a per-plan query; if the
art track needs "this plan's 31 closed escalations" that is a follow-up.

`orphan_mrs` is the one list in the piles (gd-ytTNc3Fu9M): open pull requests
that no open work bead carries, because the bead that linked them is closed
(`why: "bead_closed"`, with `bead` and that bead's `project` and last `assignee`,
whom the chore tour pings) or no work bead links them (`why: "no_bead"`). Each row
is `{url, iid, title, repo, why, bead, assignee, project, since}`; `since` is when the bead closed, else when the PR bead was
created. PR state comes from the `type=merge-request` beads github-bridge keeps
current (`forge=github`, open while the PR is open), never from a task bead's
`mr_state`. The slow tier reads the newest 200 open PR beads; a PR no open row
carries costs one lookup by `mr_url` (at most 10 per tick, cached while the PR
stays open). Newest first, at most 24 rows; `orphan_mr_total` is the count.
Both fields are omitted when there are none. The page's weeds lint
(`web/weeds.js`) draws them as weeds on the project's region.

## Bounds (what keeps it small)

| Bound | Value |
|---|---|
| Open-bead rows read per refresh | 2500, newest first, in 500-row pages (`meta.truncated` says when the cap was hit; the counts are then a floor) |
| Listed tasks per phase | 60 (`more` counts the rest) |
| `blocked_by` per task | 8 |
| `heraldry` per plan, phase or task | one canonical form: at most 5 emoji and one colour word (heraldry spec §6.5), about 40 bytes with its key for single-codepoint charges and under 150 with five long ZWJ sequences; omitted on the many beads with none, so no new cap. `heraldry_name` on a plan or phase adds five library names and places, under 250 bytes |
| Steps and `summary` per seed | ready or asked only: 24 steps (about 150 bytes each with an 80-char title) and 600 runes; none on a frosted seed |
| Meadow shown / closed plans shown | 40 / 24 |
| Wishes listed per project (open / closed) and kin ids per wish | 40 / 24 and 12 (`open_total`, `open_more`, `kin_total`, `closed_total` carry the rest) |
| Escalations listed (`escalations.open`) | 120, fleet-wide (`total` is exact); each about 300 bytes with an 80-char title |
| Releases read and listed per project | 12, newest cut first (`releases.<project>.recent`) |
| Gardeners / exits per patch | 300 (`meta.gardeners_truncated`) / 20 |
| Activity | 4 events a second per gardener, about 60 bytes each plus a caption of at most 80 runes on a start; about 64 events a second per stream (a gardener's calls fold over up to 4 s); 1000 agents tracked; 64 queued per viewer; 256 ids per view; a `busy` event at most every 5 s below the Plan level |
| Closed steps sent as rows | per phase, the cells its 9x9 bed has left (`phases[].closed[]`); at most 24 per plan and 480 per document, newest first; from the newest 2000 closed work beads |
| Bytes per closed step | about 210 with an 80-char title and an assignee; about 330 with its `harvest` |
| Produce per plan | every row, up to a 300-row safety bound (`produce_more` counts the rest; it was 24 before gd-wgeINbOQMC); the two produce lists read 600 rows each; 60 closed-ancestor lookups a tick |
| `harvest` objects per plan | 12, on the newest closed rows by `closed_at` |
| Evidence rows read per slow tick | 600, newest first |
| Bytes per `harvest` object | at most 120 (110 with every fact set and five-digit numbers; 11 more for the key) |
| Cost rows (`cost.projects`) | one per `type:project` bead, read with the project list's 500-row `listLimit`; about 200 B per operator row with four-digit amounts (220 B budget), and a tenant land's row up to about 900 B with its `purse` and `steward` (671 B measured) |
| `cost` on a plan, a gardener | about 85 B per plan with every field set (100 B budget; seeds and closed plans carry the slim `{has, usd, crew?}`); at most about 230 B per gardener, its `text` built from fixed words around at most two amounts (260 B budget), so at most about 78 KB over `maxGardeners` |

Tests pin the budgets (`farm_test.go`): a farm of 36 plans, 25 drafts (the 13 ready
ones each with a full 600-rune summary and 6 steps), 300 loose beads, 60 archived
escalations and 14 harvested closed steps per plan is under 420 KB raw (412 KB
measured: 230 KB of open work, the 480 listed closed steps, and 18 KB of ready seeds'
steps and summaries; 394 KB before seeds carried them); a 10x worst case, with 300 gardeners and 40 closed steps per
plan, is cut off by the row cap at under 700 KB raw, and so is the 360-plan farm
(83 plans in the document, 678 KB); a plan entry stays under 12 KB; a serving
request makes no daemon call. Before the closed-step caps and the slim row, 12
closed steps per plan took the 36-plan farm to 433 KB and 40 to 757 KB. With 14 closed steps per plan and every fact set, the
harvest objects add 52 KB to the 36-plan farm (432 objects). At 10x (360 plans) the row cap keeps
83 plans in the document, so they add at most 83 x 12 x 121 B, about 120 KB (46 KB measured, since
those plans share the newest 2000 closed rows), well inside the harvest spec's 520 KB estimate, so
no fact is cut (`farm_harvest_test.go`). The closed rows that carry
them cost more (about 210 B each) and are bounded by the closed-step caps above.
`TestFarmCostIsBounded` (`farm_cost_test.go`) adds every cost field to that 10x
worst case: every plan rolled up, 320 gardeners each with a reconciled total, a
live estimate and a run of one of 60 triggers, and 44 cost rows (one a tenant
land with its purse and steward). The document is 518 KB (485 KB without them,
still under the 700 KB budget): the cost blob is 8.9 KB and the gardeners' `cost`
66 KB. At the 500-row project cap the blob would be about 110 KB of operator rows;
the fleet has a handful.

Daemon cost per refresh: fast tier = up to 5 list pages plus one list of the live
agent beads (it feeds both `gardeners` and the `waits_on` roster); slow tier (closed
counts by status for plans and phases, `blocks` edges, 3 count queries, the
closed-plans list, closed phase titles, the newest closed steps, the two produce lists and their
closed-ancestor edge reads, the evidence list) at most every 20 s (the releases, a project list and one
release list per release-enabled project, at most every 60 s), or immediately when
a bead left the open set (so a close updates the closed pile in the same
refresh). A daemon event triggers a refresh after a 250 ms settle (and at least 1 s between refreshes); a
60 s reconcile repairs a lost event. The farm rides the `/garden` hub's single
upstream event subscription (`Hub.OnEvent`).

## Authorization

`GET /garden`, `GET /garden/farm`, `GET /garden/farm/events` and `POST
/garden/farm/events/view` are `routeScoped` in `cmd/beads-viewer/route_classes.go`
(gd-l7yYxZpoZo); every other `/garden*` read without its own cut, and every
`/garden` write outside the Commons, stays `routeAdminOnly`. The hub reads as the
system (`beadsapi.WithSystemRead`) and composes one document per version, which
shows **every project's** plans, with the `project` field on each; an unrestricted
viewer gets it byte for byte, and a project-scoped human gets the per-viewer cut
below. The page's own lists (Mailbox, Growing, Gardeners) are the hub's items cut
to the viewer's projects (`Hub.CutSnapshot`, an unlabelled item dropped). The hub's
stream `GET /garden/events` and `GET /garden/state` carry every project's items with
no per-viewer cut, so they stay admin-only: a scoped page (`body[data-cut]`) paints
its lists once and does not open that stream.

`GET /garden/bead/{id}` is `routeAdminOnly` too, but it reads with the
request's context, so the viewer's own bearer travels to the daemon and the
bead, its comments and its linked beads are cut exactly as on the beads-viewer
bead page it replaces.

**Project-scoped viewers, as built.** Two owner decisions set the cut, and both
are built (`farm_cut.go`, `farm_cut_stream.go`, `readonly.go`):

- **The whole document is cut per viewer** (decision gd-QPIHOFzCXc, 2026-10-18,
  option `project`; built by gd-6UG1OGlqYg, gd-UKO0ZpXeha and gd-U2pbNv3lLC, the
  routes opened by gd-l7yYxZpoZo). A project-scoped human sees only their projects'
  plans, seeds, closed plans, meadow rows, wishes, escalations, releases and gardeners, with
  `piles`, `meta.rows` and the purse recomputed for that project set. Of the cost
  figures, it gets its own projects' cost rows (a tenant land's in credits only),
  never the fleet's figures or account budgets, and a plan's or a gardener's cost
  only on an operator land. The two sections below are the field-by-field rules.
- **Writes stay admin-only.** Every `/garden` write outside the Commons is
  `routeAdminOnly`, and the page hides every write it offers from a scoped viewer
  (`Handler.ReadOnly`, gd-GuZ3tbnTFI), since the button could only fail. The
  Commons' writes are scoped and gated in the handler (`route_classes.go`); an
  escalation's Approve and Reject on a card run on the viewer's own bearer, and
  kbeads lets a scoped approver resolve only an escalation of its own project on
  an allowlisted operation (`escalation_scope.go`, gd-tUi5IuttJn).
- **A blocker in another project is named, not shown** (gd-pnmEvqmfBW, 2026-10-20,
  option `named`). The foreign id is removed from `blocked_by` (and never sent in
  any patch), and its project's name goes into `blocked_by_foreign` (`""` for a
  blocker with no `project:` label, read as "another project"). The client draws
  a bridge off the viewer's region labelled with it, so they know whom to ask.
  They never get the bead's id, title or status. This leaks project names, and the
  owner accepted that.
- **Gardeners on foreign plots are dropped**, from resets and from patches
  (including their `rm` exits). A walking gardener who leaves the viewer's region
  just exits.
- **Fail closed.** A field `farm_cut.go` does not list is not in a scoped
  document, so a new field that can carry a foreign id (or a fleet figure added to
  `FarmCost`) is not sent to a scoped viewer until the cut lists it.

### The first-paint cut (`GET /garden/farm`, gd-6UG1OGlqYg)

The viewer passes the hub the requester's cut (`gardenhub.Handler.Cut`, from
`viewerScope` in `cmd/beads-viewer/scope.go`): the whole farm for an
unrestricted caller, else the scoped principal's projects. An applicant, or a
scoped principal with no project groups, is scoped to nothing and gets an empty,
valid farm. The code is `internal/gardenhub/farm_cut.go`.

- **The admin path is unchanged.** An unrestricted caller gets the same bytes and
  the same `W/"f<v>-a<epoch>"` ETag as before the cut;
  `TestFarmCutAdminGolden` pins them against `testdata/farm_admin_golden.json`,
  written by the code before the cut existed.
- **Built from memory, memoised per project set.** The refresh keeps, beside each
  entry, what a cut needs that the capped entry lost (every open step before the
  phase cap, every letter before `maxWaiting`, every orphaned PR, the meadow rows
  past `maxMeadowShown`, the open rows per project). A scoped document is built
  from that on request, memoised by the sorted, deduplicated project set (with
  its gzip), and never reads the daemon on the request path. Its ETag,
  `W/"s<hash>"`, hashes the cut's content without `v` and `built_at`, so one
  set's ETag never matches another set's body and a version that moved only
  other lands still revalidates to a 304. `Cache-Control` stays `private`.
- **Plans and seeds** whose `project` is in the set. Inside one, placement
  decides: a step, a closed step, a letter or a produce item shows unless its own
  `project:` label names a project outside the set. A dropped step leaves its
  phase's `counts`, its `more` crate, the plan's `counts`, `open_decisions`,
  `open_escalations`, `needs_human` and `waiting` recomputed without it. A
  phase's `counts.closed` is the daemon's count of its closed children and is
  kept. A seed drops a step or its `decision` the same way.
- **`blocked_by`** keeps only blockers whose project is in the set (a blocker's
  project is its `blocked_by_project` entry, else the step's own); an unlabelled
  blocker cannot be decided and goes. `blocked_by_project` keeps only the kept
  blockers' entries. Each removed blocker leaves its project's name, never its
  id, in **`blocked_by_foreign`** (gd-U2pbNv3lLC): one entry per project, sorted,
  `""` for an unlabelled blocker, e.g. `"blocked_by_foreign": ["", "kbeads"]`. No
  id, title, status or count of foreign blockers beyond that. The field is only
  ever in a scoped document. `web/edges.js` turns each entry into an edge that
  links nowhere: the bead view, the region sentence and the hot links' names say
  "Blocked by a bead in kbeads" ("another project" for `""`). On the map it is a
  bridge (gd-DgEhq38EnY): a short channel from the waiting plot (the region's
  middle at the World level) out of the region by a side no other region lies
  beyond, stopping short of any neighbour, under a footbridge named for the
  project.
- **Meadow rows** whose project is in the set (an unlabelled row goes), with
  `total` and `with_mr` recounted; a row's `parent` only when the parent is an
  open row in the set.
- **Gardeners** of a project in the set, on no plot or on a plan in the set, whose
  `task`, `tends` and `escalation` are each unset or a bead read in the set, so a
  gardener that walks onto a foreign plot leaves the cut and one that walks in
  appears. `chat` and `thread_title` are dropped, since either can name a bead
  outside the set. `reason` keeps its `kind` in fixed words and no `href`
  (gd-U2pbNv3lLC): parked reads "Waiting on a person", sleeping "Sleeping", down
  keeps only its head ("Pod failed", never the wrap-up's blockers), stale and
  starting keep their words; a kind not listed is dropped.
  A gardener's `cost` is kept only on an operator land (its cost row has no
  `purse`), with `gardener_usd` null and `text` rewritten to match, since that
  rollup counts crew the cut cannot place in the set. On a tenant land, or any
  land while the farm has no cost blob, `cost` is `null`: a tenant land sees no
  at-cost dollar figure (gd-0VeG81uW3w).
- **`cost`** is `FarmCost.ForProjects` (below): the set's rows, and the purse
  summed over them with `scope:"project"`, never the fleet's figures or account
  budgets. A tenant land's row carries its `purse` as credits only
  (`Purse.CreditsOnly`) and no at-cost dollar figure (`budget_usd`, `today_usd`,
  `week_usd`, `spent_usd`, `remaining_usd` null, `has_budget` false, no `level`), and adds
  nothing to the purse's sums. A set with no row gets `{"projects":[]}`.
  The two cost cuts are independent: the purse's sums come from the rows (the
  spend rollup by project), never from `gardeners[].cost`, so a scoped viewer's
  `today_usd` is not the sum of the gardener figures it sees (those are lifetime
  totals per agent, and a gardener's own cost follows its land's row: kept on an
  operator land, `null` on a tenant land whose row is credits only).
- **Plan cost** (`plans[].cost`, `seeds[].cost`, gd-kcy4IFNzv5) is kept only on an
  operator land (a cost row with no `purse`), and only when the cut hides none of
  the plan's or seed's steps: the figure sums the crew of every step, so a hidden
  step's crew would be in it. Otherwise it is the unknown cost
  (`{"has":false,"usd":null}`), as a tenant land gets no at-cost dollar figure on
  its cost row, and every land does while the farm has no cost blob. A closed plan
  (`closed_plans.shown[].cost`) never carries its cost in a scoped document: the
  cut cannot see its steps. `TestFarmCutPlanCost` pins this.
- **`piles` and `closed_plans`** are summed over the set's own archives: per
  project, the three closed counts, the newest closed plans and the closed
  wishes' count (for `wishes.closed_total`), the fleet
  queries with the project's label. They are read lazily: a scoped request marks
  its projects wanted and the next refresh reads any not read in the last
  `farmSlowEvery`; a project nobody has asked for in 10 minutes is dropped, so
  nothing is read while no scoped viewer is about. Until a project's archive is
  read, `piles` is `{}` and `closed_plans` empty, as before the first slow read.
  The orphaned PRs are the set's, recounted.
- **`meta.rows`** counts the open rows in the set; the rest of `meta` describes
  the read (`truncated`, `row_cap`, the roster's freshness) and stays.
- **`wishes`** (gd-gUeBN96RRL) are the set's open and closed wishes, each
  project capped as for the whole farm (40 open, 24 closed), with `open_total`
  and `open_more` the set's. Inside a wish, a kin and a headstone's produce item
  show only when their own `project:` label is in the set (an unlabelled one
  goes), and a plot only when its plan is; `kin`, `kin_total`, `kin_closed`, a
  headstone's `kin` and `tending` are recounted over what shows. A headstone's
  `reason` is dropped, since a `kd close` reason can name a bead outside the set.
  `closed_total` is summed over the set's archives, which also count each
  project's closed heads (the fleet count with the project's label); until they
  are read it is the set's closed heads among the kept closed rows. The refresh
  keeps the wishes before the caps (`wishSet`), so the cut reads nothing.
- **`home`** keeps the keys of the set's projects only, never the Commons's `""`
  (an item with no project cannot be decided, so it goes, as on the ledger's
  cut). `barn` counts the gardeners the cut admits and `wishes` the cut's own
  `wishes`, so a gardener the cut hides (one sent to a bead in another land) is
  not counted. `TestFarmHomeCounts` and
  `TestFarmHomeCutCountsOnlyAdmittedGardeners` pin it.
- **`escalations`** are the set's: an escalation whose `project:` label is in
  the set (an unprojected one goes) and whose plan or seed, when it has one,
  is in the set too, as its steps are. Its `executor` is named only when the
  executor's own project is in the set; `total` counts what the set keeps.
  A plan's `waiting[]` follows the same executor rule, so an escalation of the
  set run by another project's gardener names no executor in either place.
  `TestFarmWaitEscalationYardCut` pins this.
- **`releases`** are the keys of the set's projects, each whole; nothing of
  another project's. A plan's `shipped` rides its plan, and only a release of
  the plan's own project sets it.
- **`v`** is the cut stream's version, not the farm's (next section).

### The stream cut (`GET /garden/farm/events`, gd-UKO0ZpXeha)

A scoped viewer's stream is not the whole farm's stream filtered: it is a
stream of its own, one per project set (`internal/gardenhub/farm_cut_stream.go`),
whose patches are the difference between two cuts of the farm. What it is sent
is therefore exactly what its first-paint document holds.

- **One stream per project set**, made by the first request for that set (a
  first paint or a connect) and advanced by every refresh, whether or not the
  whole farm moved (an archive arriving moves only the cut). A stream nobody is
  connected to is dropped once nobody has asked for it in 10 minutes.
- **Rows** (plans, seeds, gardeners) are upserted when they are in the cut and
  their cut entry changed, and removed only when the viewer's last view had them:
  an `rm` never names a row the viewer was not sent. A plan, step or gardener
  that leaves the cut (moved to a foreign plot, relabelled) is a plain removal.
- **Blobs** (`closed_plans`, `meadow`, `wishes`, `home`, `escalations`, `releases`, `piles`, `meta`, `cost`) are sent
  whole, recut for the set, when the cut's value changed.
- **`exits`** ride the patch whose `gardeners.rm` they explain, for the removed
  gardeners only, without `tldr`, `tends` and `escalation` (each can name a bead
  outside the set), and with `plot` only when its plan was or is in view.
- **Its own versions.** A cut's `v` moves only when its view does, so a viewer
  hears nothing when another land moves. Every cut takes its versions from one
  farm-wide counter in microseconds (the whole farm's counts in milliseconds),
  so a version names one set's state: a viewer holding another set's `v`, or the
  whole farm's, never matches the ring and gets a `reset` to its own cut. The
  first-paint document carries the same `v`, so `?since=` and `Last-Event-ID`
  work exactly as on the whole farm's stream: none, one patch from the cut's own
  change log or a patch chain from its ring, or one reset to the cut's document.
  A laggard is caught up the same way, from its cut's log.
- **Activity** (`activity`, `busy`, `doing`) goes only for the gardeners in the
  cut's document, on top of the viewer's `SeesGardener`, so a gardener of the
  viewer's land that the document drops (one on a foreign plot) is not named by
  its calls either.
- **The admin stream is unchanged**: an unrestricted viewer subscribes to the
  whole farm's stream as before. `TestFarmCutStream` runs a twin farm nobody
  scoped looks at through the same changes and compares the admin patches byte
  for byte.

The opening landed in that order: the two cuts above, the Phase 3 fields
(gd-U2pbNv3lLC), hiding the write affordances (gd-GuZ3tbnTFI) and the isolation
tests (gd-16UJtQh3Ua, `farm_cut_isolation_test.go`), and last the route
reclassification (gd-l7yYxZpoZo), which opened the four routes named above.

## The Forest view on /garden

The Plans panel has a **Crops | Forest** toggle (remembered per browser). Forest is the `/plans?view=tree` zoomable
forest (`cmd/beads-viewer/plans_tree.go`) drawn from this same document, so it needs no extra field or request:
one SVG tree per plan, a limb per phase (`phases[]`, phase 1 lowest, plus an unlabelled limb for `unphased`), a leaf
per step coloured by state. The farm document carries per-phase **counts** (closed steps are never sent as beads), so
`tree()` in `web/plans.js` places one leaf per counted step in the order closed, review, growing, blocked, planted,
dormant; the silhouette matches `/plans`, the leaves are not individual beads. Caps match the Go glyph: 9 limbs, 12
leaves per limb, and the clip is printed (`+N`, `+N phases`), dropping finished work first so a blocked step is never
the one that disappears. Trees stand in three groups: **Needs you** (open decision/escalation, gold halo), **Growing**
(a step in progress or in review; green dot at the base while work is in flight) and **Planted**. Canopy / Grove / Tree
only change size and chrome (phase labels appear at Tree). Clicking a tree, like a crop tile, opens the plan's bead view (3b R12).
The Plans panel opens from the Menu's Plans item (3b R9).

## Postcards (Phase 4, gd-WxRuyfdUtR, #963)

The Menu's **Postcard** item (after Snap) sends the plot at the cursor to its plan's Slack thread as a picture with a
one-line caption. It adds no field to this document and no endpoint: `web/postcard.js` builds the card in the browser.

- **What it reads from the plan entry.** `id`, `title` (cut at 120 runes), `phases[]` (the first one not `closed` gives
  "phase n of N: title"), `counts` (closed over all six counts gives "3/9 steps"), `state` else `status`, and `project`.
  The caption reads `Postcard: <title> · phase 2 of 4: <phase> · building · 3/9 steps`, and a part with nothing to say is
  left out.
- **The picture.** The farm canvas as drawn, cropped to the plot's rect on screen plus a 12 px margin and cut to the
  viewport, in device pixels. It is drawn again at 1x when it is over the Commons' 10 MB file cap. Labels over the canvas
  are not in it. When less than 24 CSS px of the plot is in view, nothing is sent and a toast says so. The file is named
  `postcard-<land>-<plan id>-<when>.png`. It carries a farm snapshot context (`COMMONS.snapContext`, the message's
  `fields.snap`) that frames the plan, so the Commons and Slack link back to the view at the plot (`/garden?at=`).
- **Where it goes.** The plan's thread, as the Commons places it (`GET /garden/c/bead/{id}`, read with the viewer's own
  scope). With no thread it goes to the first channel of the plan's project land. The card waits in that Commons
  composer with the caption filled in, unless the viewer already has a draft there. **Send** posts it as the viewer's
  message. The bridge's thread forwarder (`bridge/thread_message_forward.go`) then uploads the attachment into the Slack
  thread, using the Commons mirror's file fetcher (`BEADS_VIEWER_INTERNAL_URL` and the bridge's machine principal).
  Without those, the attachment goes to Slack as a viewer link.
- **Who sees it.** The item shows only to signed-in viewers who have a Commons, since it posts as them. Its only reads
  are this document and the Commons place route, so the farm's visibility rules (Authorization above) are unchanged.

## Freshness, accessibility, failure states (Phase 5, gd-OAPnUppWQn)

- **Freshness.** Measured on the live daemon (event written by `kd`, to the farm patch arriving in a browser stream): before
  1.0 to 2.0 s (median 1.8 s, the old 1.5 s coalesce), after 0.37 to 0.52 s. The refresher now settles 250 ms after the first
  event and leaves at least 1 s between the end of one refresh and the start of the next, so a steady event stream costs the
  daemon at most one fast tier (up to 5 list pages and the agent read) per second. `TestFarmFreshnessLatencyAndRate` pins both numbers.
  Slow-tier facts (new `blocks` edges, closed counts by phase) still lag up to 20 s by design.
- **Wire size.** `/garden` HTML (about 120 KB of inline CSS, JS and art) is gzipped for clients that accept it (about 36 KB), cached per
  snapshot version and viewer; `/garden/farm` was already gzipped (about 13 KB on the live farm).
- **Plans failure states.** While the farm loads the slot says so; if it fails it names the status, retries with backoff (3 s, 6 s … 30 s)
  and offers "Try again now". A farm stream that goes quiet (no event or heartbeat for 45 s) is reconnected, and after 20 s without
  one the slot says the plans are as of N ago. The ledger and the scene never depend on the farm.
- **Accessibility.** Skip link and a single `<main>`; the live indicator announces only connection-state changes (the ticking "synced Ns ago"
  is `aria-hidden`); the Porch and Mailbox are two buttons that each open their pane (`aria-expanded`, 3b R5); coarse pointers get 44 px targets.
  The palette is held to WCAG AA text contrast in both schemes by `TestPaletteTextContrastAA` (it caught light-mode warn/ok/alert text and
  the dark-mode alert badge below 4.5:1).
- **Scoping.** "Mine" is the viewer's own `operated_by` on plans and seeds and the ledger rows. A viewer with no identity (no email on the
  session) is put on Everyone and the Mine button is disabled, rather than shown an empty page while the plans show everything. The route
  is `routeScoped`: a project-scoped viewer gets the cut decided under Authorization above.

## Costs

The document carries four cost figures, each from its own rollup, and none is
summed into another:

| Figure | Where | From | Section |
|---|---|---|---|
| The purse: fleet spend today and over 7 days, and the account budgets | `cost` (`scope`, `has`, `today_usd`, `week_usd`, `as_of`, `has_budget`, `budget_usd`, `spent_usd`, `remaining_usd`) | the costs rollup by project, and the account beads (gd-594kYy1IYo) | [Project cost](#project-cost) |
| Per-project spend and budget, a tenant land's purse and steward | `cost.projects[]` | the same rollup and the `type:project` beads (gd-T6zwDjWLBI, Lands 8.2) | [Project cost](#project-cost) |
| What a plan's crew spent | `plans[].cost`, `seeds[].cost`, `closed_plans.shown[].cost` | the controller's plan spend sync, on the epic (gd-kcy4IFNzv5) | [Plan cost](#plan-cost-gd-kcy4ifnzv5) |
| What a gardener spent, and its gardener over every run | `gardeners[].cost` | the agent bead, and its trigger's rollup (gd-0VeG81uW3w) | [Gardener cost](#gardener-cost-gd-0veg81uw3w) |

`testdata/farm_fixture.json` shows every one: a row under a month budget
(`warn`), a row over a 7d budget (`over`, `remaining_usd: 0`), a row with no
budget, a tenant land with its purse and steward, and the fleet's figures and
account budgets beside them. `testdata/farm_fixture_cost_down.json` is the same
purse with the rollup down: every row `has: false` with `as_of`, and the fleet's
`today_usd` and `week_usd` gone, while the account budgets, the purse and the
steward stay known.

**Live cost is a lower bound while gd-UIlh0DR2VG is open.** On the LiteLLM path
an agent bead's `live_cost_usd` stays 0 until its session closes, so whatever
counts a running session is low until then: a gardener's `spent_usd` and
`live_usd` (usually `null`), a plan's `usd` while `live > 0`, the gardener's
`gardener_usd` (it sums closed sessions only), and a tenant purse's
`in_flight_credits`. The purse's own figures and the per-project rows are not
affected: they come from the real-bill rollup, which is at most about 5 minutes
behind. A client says "at least" wherever `at_least` or `stale` is set.

### Plan cost (gd-kcy4IFNzv5)

`plans[].cost` is what a plan's crew have spent. The farm does not walk agent beads for
it: the controller's plan spend sync (`cmd/controller/plan_spend_sync.go`, walk and
per-agent figure in `internal/planspend`) rolls it up every 10 minutes onto fields of the
plan epic, and the farm reads them off the epic row it already has.

- **Which crew.** Exactly the crew the `/plans` crew card counts: agent beads with an
  `assigned` edge into one of the plan's rendered steps (issue-type children of the epic,
  and of each phase container in its `phase_order`) or into the epic itself, minus the
  plan-gardener. One agent's dollars are its reconciled `total_cost_usd`, plus its
  `live_cost_usd` estimate while it is live (open, alive state, heartbeat under 1h). A
  sub-plan (a child epic not in `phase_order`) is not the plan's. The viewer test
  `plans_crew_rollup_agree_test.go` keeps the two to the cent.
- **Fields on the epic.** `plan_spend_usd` (4-decimal string), `plan_spend_synced_at`
  (RFC3339), `plan_spend_crew` and `plan_spend_live` (integers), declared on `type:epic`.
  The controller writes them only when the figure moves by a cent or the crew or live
  count changes, never to re-stamp an unchanged figure: each write wakes the plan's
  gardener (gd-vwMhcJgts2).
- **Closed plans.** Walked once more after `closed_at`, then left alone; reopened and
  closed again, walked once more.

```jsonc
// FarmPlan.cost
{
  "has": true,              // false until the controller has written a figure; usd is then null
  "usd": 12.35,             // US dollars, rounded to the cent
  "synced_at": 1792009700,  // unix seconds of the last write
  "crew": 3,                // crew agent beads summed
  "live": 1,                // of which live at synced_at; omitted when 0
  "stale": true             // omitted unless: live > 0 and no plan in this build has a
                            // synced_at under 30 minutes (3 intervals) old. The rollup pass
                            // stopped while the figure was moving, so it is a lower bound.
                            // A settled figure is never stale.
}
// FarmSeed.cost and closed_plans.shown[].cost: the slim {has, usd, crew?}.
```

A scoped viewer gets a plan's or a seed's cost only on an operator land, and only
when the cut hides none of its steps; a closed plan's never. See the first-paint
cut under Authorization.

### Gardener cost (gd-0VeG81uW3w)

`gardeners[].cost` is what one gardener has spent, and, on a gardener's run, what
that gardener has spent over all its runs and their crew. The wire shape and every
field are under [FarmGardener](#farmgardener-a-person) above (`spent_usd`,
`live_usd`, `at_least`, `gardener_usd`, `text`); `farm_gardener_cost.go` builds
it. It is read off the agent bead the fast tier already has, plus one
`BatchGetBeads` of the triggers the live gardeners name (at most 50, each re-read
at most every 5 minutes), so it adds no daemon read per gardener.

- **Not in the purse.** The blob's figures never add the gardener figures in: those
  are lifetime totals per agent, the purse is spend over a window by project.
- **Visibility.** A scoped viewer gets it only on an operator land, and without
  `gardener_usd`; on a tenant land, or while the farm has no cost blob, it is
  `null` (see Authorization).

### Project cost

The `cost` blob is the purse. Its figures come from the costs rollup: the
real-bill per-project spend gauge that `/costs` rolls up by project
(`gardenio_tag_cost_usd{dim="project"}`, refreshed about every 5 minutes), read
unranked with the same expression, so `/garden` and `/costs` agree. The farm
re-reads it on the slow tier at most once a minute and sends `cost` in a patch
only when a figure changes. The blob carries the purse's own figures and one row
per `type:project` bead. What each gardener has spent is not in this blob but on
its entry (`gardeners[].cost`, above); the blob's figures never add it in.

```jsonc
// FarmCost: the purse (gd-594kYy1IYo).
{
  "scope": "fleet",        // "fleet" | "project": a project-scoped viewer's cut (below)
  "has": true,             // omitted (false): the rollup could not be read; today_usd and week_usd are omitted
  "today_usd": 41.2,       // since 00:00 UTC, summed over every project the rollup reports
  "week_usd": 512.9,       // rolling 7 days, the same sum
  "as_of": 1792000000,     // only while has=false: the stale marker, as on a row
  "has_budget": true,      // omitted (false): no account has a budget (or none read yet); show a dash
  "budget_usd": 600,       // the budgeted accounts' budgets, summed; omitted when !has_budget
  "spent_usd": 230.5,      // their spend since the last reset, summed; omitted when !has_budget
  "remaining_usd": 380,    // their budget - spent, clamped at 0 per account, summed; omitted when !has_budget
  "level": "ok",           // BudgetLevel(budget_usd - remaining_usd, budget_usd): the money gone, not spent_usd; omitted when !has_budget
  "projects": [ /* FarmCostProject rows */ ]
}
```

- **Fleet spend** sums the rollup's whole project dimension, so a project with
  spend and no `type:project` bead still counts; it can be more than the rows add up to.
- **Account budgets** are the `/costs` budget rollup (`costsSummaryView.BudgetCount`
  and friends, read through `farmSpendSource.FleetBudgetUSD`): per open account, the
  budget is `spend_limit_usd` (LiteLLM `max_budget`, synced by spend_sync), else the
  operator `max_budget` hint, and only above 0; spent is `total_cost_usd -
  spend_at_last_reset` floored at 0, a missing baseline counting as 0 as the spawn
  path's `accountOverBudget` does; a shared LiteLLM key is booked once. Remaining is
  clamped per account, so one overspent account does not eat another's money; an
  overspend shows as `spent_usd` near or past `budget_usd`. The budgets come from the
  account beads, not the rollup, so `has:false` leaves them known, and a failed read
  keeps the last ones read.
- **Unknown is absent, never 0.** Unlike a row, the blob omits a figure it does
  not know and a false flag, so a client reads a missing `today_usd` or
  `remaining_usd` as a dash, and a cut with nothing in it is `{"projects":[]}`.
- **The project-scoped cut** (decision gd-QPIHOFzCXc) is `FarmCost.ForProjects`,
  which `cutCost` serves every scoped viewer: `scope:"project"`, only the viewer's
  rows, `today_usd`/`week_usd` summed over its rows in dollars (omitted, with the
  oldest `as_of`, when any of them is down), and no account budgets: a scoped
  viewer's "$ remaining" is each row's own `remaining_usd`, never summed. A tenant
  land's row is credits only and is not summed, so a viewer of tenant lands alone
  gets `scope:"project"` with no dollar figure and reads its purse's credits.
- **The header purse** (gd-H9LerJ0v3g, `web/costs.js` `paintPurse`) is the
  coin-box in `/garden`'s header controls, painted from each document: for
  `scope:"fleet"` one row, Fleet, `today_usd` and `remaining_usd` (a dash with no
  account budget), and for a viewer who may read the charts a link to
  `?costs=fleet`; for `scope:"project"` one row per row in dollars, named for its
  project, its `remaining_usd` hidden only when `has_budget` is false, and no link.
  `has:false` shows a dash for today and the `as_of` time, never $0. `$ remaining`
  carries its `level` in a word as well as a colour (low, over budget).

The per-project budget is read from the `type:project` bead: `budget_usd` (US
dollars; unset = no budget, never $0) and `budget_period` (`month` by default, or
`7d`). How to set them:
[Project budget](onboarding-new-project-and-role.md#step-4d--optional-project-budget).

```jsonc
// FarmCostProject: one per project, sorted by name. Budgets are per project,
// so a viewer of several projects gets one row each, never a sum.
{
  "project": "gardenio",
  "has": true,            // false: the rollup could not be read; every figure below is null
  "has_budget": true,     // false: budget_usd blank (or not a number above 0); hide "$ remaining"
  "budget_usd": 250,      // null when !has_budget
  "period": "month",      // "month" | "7d"; omitted when !has_budget
  "today_usd": 4.1,       // since 00:00 UTC
  "week_usd": 61.75,      // rolling 7 days
  "spent_usd": 190.2,     // over the budget period; null unless has && has_budget
  "remaining_usd": 59.8,  // budget_usd - spent_usd, clamped at 0; null unless has && has_budget
  "level": "warn",        // "ok" | "warn" | "over"; omitted unless has && has_budget
  "as_of": 1792000000     // only while has=false: when the figures were last read well (stale marker)
}
```

- **Periods.** `month` is the UTC calendar month to date and resets at 00:00 UTC
  on the 1st; `7d` is the same rolling 7 days as `week_usd`. Amounts are rounded
  to the cent. The rollup's 5-minute grid makes the first minutes of a day or
  month read low.
- **Unknown is null, never $0.** A failed rollup read sets `has:false` on every row
  and nulls every spend figure, so a client shows a dash and the `as_of` stale
  marker. A project with no spend in the window reads `0` (the read succeeded).
- **Overspend** reads `remaining_usd: 0`; `spent_usd` > `budget_usd` says by how much.
- **Level** buckets `spent_usd` against `budget_usd` like the gardener run-budget
  meter (`gardenerBudgetBurn`): `ok` under 75%, `warn` from 75% to 99%, `over` at
  100% or more (`gardenhub.BudgetLevel`, on the cent-rounded figures). A client
  maps it straight to a CSS class. It is display only: nothing gates a spawn or
  pauses a gardener on it; enforcement would be a separate decision.
- **`/costs` agrees.** The `/costs` rollup's by-project rows show each project's
  budget, what is left and the level, copied from this blob
  (`Farm.ProjectCost`), not recomputed. They cover the budget's own period, not
  the rollup's range, and read "— left" while the farm's spend read is down.
- **Units.** Every amount is US dollars and says so in its name, except inside
  `purse`, below; the `*_usd` fields keep their meaning on every row.

A **tenant land's** row (`land_kind=tenant` on the project bead) also carries its
`purse` (Lands 8.2, gd-QSzQBkO1lk; `internal/lands/purse`, Track B spec §3.4):
the land's credit ledger against its own LiteLLM account, not the rollup above.
Operator lands have no `purse`. The same object is `GET /api/lands/{land}/purse`
and `gb lands purse <land> --json`.

```jsonc
"purse": {
  "land": "tomato-patch",
  "state": "ok",               // "ok" | "wall" | "unknown"
  "blocked": false,            // true at the wall and when unknown: new spawns are refused
  "reason": "",                // why it is unknown
  "granted_credits": 100,      // sum of the land's credit_grant beads
  "spent_credits": 15,         // account total_cost_usd x llm_multiplier; null when unknowable
  "in_flight_credits": 3,      // running agents' live cost since the last spend sync, x llm_multiplier
  "remaining_credits": 82,     // granted - spent - in_flight
  "margin_credits": 5,         // max(2, 5% of granted); wall = remaining <= margin
  "used_pct": 18,              // (spent + in_flight) / granted x 100; null when nothing is granted
  "rate_version": 1, "llm_multiplier": 1.5,
  "granted_usd": 66.66,        // the same amounts at cost (credits / llm_multiplier): the operator's figures
  "spent_usd": 10, "in_flight_usd": 2, "remaining_usd": 54.66,
  "running_agents": 1,
  "spend_synced_at": "2026-10-19T19:58:00Z",  // when spend_sync last stamped the land's account
  "read_at": "2026-10-19T20:00:00Z"
}
```

- **Unknown is the wall.** The purse is `unknown` (and `blocked`) when no credit
  rate is in force, the land does not bill to its own `lands-<land>` account, the
  spend has not synced for more than 15 minutes, or the read failed. A stale purse
  keeps its last figures beside `spend_synced_at`; a failed read has none.
- **Credits are what a tenant sees.** The unrestricted document's row carries
  the at-cost `*_usd` figures too; a scoped viewer's cut (`cutCost`, step 8.8)
  drops them (`Purse.CreditsOnly`) along with the row's budget and spend dollars.

A land with a **steward** (Lands 9.2, `internal/steward`, Track B spec §4.3) also
carries `steward` on its row: the daily run cap the land header shows when it is
hit (gd-srGLTTTxPb). It is read on the cost tier from the land's steward trigger,
the open, in-progress or blocked (paused) `type:trigger` bead labelled `steward`
whose one `gardener-land:<land>` label names a `project:<land>` label it also
carries. A land with no steward has no `steward`.

```jsonc
"steward": {
  "id": "gd-abc123",              // the steward's trigger bead
  "href": "/garden?bead=gd-abc123",
  "max_runs_per_day": 24,          // 0 = no daily cap
  "runs_today": 24,                // runs spawned since 00:00 UTC
  "capped": true,                  // runs_today >= max_runs_per_day: no wake spawns it today
  "resumes_at": 1792800000,        // the next 00:00 UTC (unix seconds); only while capped
  "agent": "tomato-steward",       // its name, <land>-steward, as its runs post (Lands 9.8)
  "reason": {                      // what the map's steward says on hover and tap; null when nothing
    "kind": "in_flight",           // sleeping | in_flight | promise | parked
    "text": "Plan X: crew on step 2, PR #14 open",
    "href": ""                     // parked only: the handle the line names
  },
  "notes": "gd-nts123"             // the steward-notes:<land> bead the land header links; omitted when the land has none
}
```

- **The notes link (gd-q4NgYNGpy8).** `notes` is the id of the bead the reason
  is read from, so the land header can link the steward's memory with no read of
  its own. The link opens `GET /garden/notes/{land}` (gardenhub `land_notes.go`,
  routeScoped): the notes whole, read on the viewer's bearer, and whether the
  viewer may edit Holds (owner and member, `conversation.ActionEditHolds`).
  `POST /garden/notes/{land}/holds` re-reads the notes, splices the new Holds
  body between the stored text before and after it, and patches
  `fields.content` alone on the viewer's bearer, which kbeads' `edit_holds`
  check holds to the Holds section.

- **The reason (Lands 9.8, amendment 2026-10-23 §8).** At the purse wall
  (`purse.state` `wall`) it is `sleeping`, "Parked: the purse is empty".
  Otherwise its text comes from the steward's memory, never recomputed: the
  first entry of the `In flight` section of `steward-notes:<land>`, else of
  `Promises` (first line, emphasis and code marks dropped, clipped to 140
  runes). A line that names a decision or escalation still waiting on a person
  (among the farm's open rows) is `parked`, with that handle's `href`, so the
  bubble leads to the letter. The notes are read on the cost tier; a failed
  read keeps the last body read. `TestFarmStewardReadsMemory` in
  `internal/steward` pins the notes title, the sections and the role.

- **The engine's count.** `runs_today` is the engine-owned counter in the trigger's
  `gardener` blob (`runs_day` + `runs_today`), and `capped` is
  `bridge.GardenerDailyRunCapHit`; `TestFarmStewardMatchesEngine` pins the two.
  The cap is read against the clock at every cost read, so it lifts at midnight
  even while a failed trigger read keeps the last triggers seen.

- **Visibility.** An unrestricted viewer gets every project row. A scoped
  viewer's cut (see [Authorization](#authorization)) carries only its projects'
  rows, and a tenant land's in credits only.

## The bead view's kin (`GET /garden/bead/{id}`)

Plan gd-gO1fTT7oGC (one bead view) gave the bead document a `kin` block: the
beads bound to the bead's thread by `fields.thread_id`, projected once by the
shared package `internal/kin`, which gardenhub and the Commons both import so
the bead view and the Commons thread agree on who wrote what (step 2,
gd-58mkDiWkNq). It is read on the request, like the rest of the bead
document, never from the farm document.

**The read** (`bead.go` `readKin`, in the same wait group as the comments and
the edges):

- The thread is the bead itself when it is a wish head (`kin.IsWishHead`: any
  bead but a message or channel with the `thread` label), else its
  `fields.thread_id` (`kin.ThreadOf`).
- One list: every bead with `thread_id` = the head, of every type, newest
  first, at most 500 (`kin.Query`, `kin.Max`); `more` says the read hit the cap.
- When the bead is not the head, the head itself (`GetBead`), for `head`; a
  head the viewer may not see is left out.
- The messages out of the thread that name the bead in their text: the
  daemon's reverse mentions (`GetMentionedBy`, at most 50, `kin.MentionMax`),
  messages only, then one `BatchGetBeads` (step 9, gd-UXDp9GUeMB). A failed
  read leaves them out and the conversation stands without them.

Three daemon calls at most, all in parallel; nothing is read per row. A bead in
no thread that no message names has no `kin`. A bead in no thread that messages
name has `kin` of those messages alone (`no_thread: true`). When the list does
not answer, `kin` is absent and `partial` names `kin`.

```jsonc
"kin": {
  "head": { "id": "gd-wish1", "title": "…", "short_title": "…",
            "where": "slack", "channel": "C0123" },   // only when this bead is not the head; where: slack | commons
  "conversation": [{                                    // oldest first; messages only
    "id": "gd-m1", "author": "oidc:matt@example.com",
    "voice": "person",                                  // person | agent | steward
    "via": "slack",                                     // slack | squawk | card | commons | say | mention
    "text": "…", "at": 1792000020, "slack_ts": "1792000020.000100",
    "subject": "gd-ag1",                                // a card's subject bead (the agent, a decision)
    "deleted": false
  }],
  "work": [{ "id": "gd-t1", "type": "task", "state": "in_progress", "pr": "https://…/77",
             "pr_state": "merged", "parent": "gd-plan1" }],   // task, bug, feature, chore, epic ("plan" for an epic labelled plan)
  "agents": [{ "id": "gd-ag1", "name": "gardenio-x", "state": "working", "live": true }],
  "sessions": 1,                                        // a count only
  "more": false, "no_thread": false
}
```

- **Who speaks** is `kin.Speaker`, the voice rules of the Commons rubric
  (`commons-ux-rubric.md` C11, Voices): a Slack reply is the person's, a squawk
  the agent's named from `from:<agent>`, a card the agent's state line, a
  steward run's line the steward's. Text is cut at 4,000 runes (`kin.TextMax`).
- **Work** rows are in the order the thread gained them; `parent` is the plan
  or epic in the list whose `step_order` or `phase_order` names the row, `""`
  for a root. The bead itself and the head are never repeated in `work` or
  `agents`.
- **Agents** are live while open and neither `done` nor `failed`, as the
  Commons agent card reads them.
- **Who reads it.** The panel's wish face takes its counts line from it, its
  Conversation section draws `conversation` with the bead's comments as one
  timeline (`web/beadtalk.js`), and its header's `Part of wish` chip reads
  `head`. The Work section is not drawn from `work`: it is the Commons node's
  tree, embedded (Workbench spec WB-19).
- **Live.** The open panel patches its conversation from the Commons stream's
  message events for its head's thread, without reading the bead document
  again (step 10, gd-MpqfrPhLZP).

## Not in this document

Four things the page shows or will show are not carried here:

- **History.** The document is always now; the 256-patch ring and the change log behind a
  catch-up are for SSE catch-up only and nothing is stored.
- **Time-lapse (parked).** The farm-history scrubber is not built. It was cut from Phase 4 and
  parked in epic gd-mnDfHWSfHR (step gd-CP6HlODpWC). Its design (frames, retention, the
  `/garden/farm/history` read API, Postgres frames chosen) is in
  [`2026-10-23-farm-history-timelapse-design.md`](../../docs/superpowers/specs/2026-10-23-farm-history-timelapse-design.md).

- **The Mailbox letters.** A plan's handles are in its `waiting[]`, and a parked
  gardener's `reason.href` names the one it waits on, but the letter itself (a
  decision's prompt and options, an escalation's context and operation) comes from
  the `/garden` page snapshot's `decision` section (`GET /garden/state`,
  `/garden/events`), and `web/answer.js` answers, approves or rejects it in place.
  A decision or escalation that belongs to no plan is only there. Decision cards
  inside a Commons thread are Commons II slice 3 of the Lands plan; until they land,
  the Mailbox is where a decision is answered.
- **Plan gates.** The document says which seeds are `ready` and each plan's
  `state`, and a seed's open plan decision is answered from its pot (FarmSeed
  `decision`, above), but Approve & build, Mark complete and Abandon have no write on
  `/garden`. They stay on `/plans` and `gb plan`. No plan has taken them on yet.

Agents as people are done and are in this document (`gardeners`, `reason` and the
patch-only `exits`, above). Chat added no field: a gardener's `chat` is the path to
its Commons thread, and the thread's context chip and quick replies read the
entry's `task`, `task_title`, `project` and `reason`. Wake and restart are the
admin-only `POST /garden/gardener/{id}/wake`; the next snapshot shows the gardener
`starting`.
