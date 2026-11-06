# Ownership Defaults Across Creation Paths

**Bead:** ka-0000000391 (parent epic: ka-0000005305)
**Author:** gardenio-research-ownership-defaults-o3j
**Date:** 2026-05-13
**Sibling research:** ka-0000007523 (attribution chain) — required reading; this
report builds on its terminology and recommendations.

## TL;DR

Beads-viewer shows `Owner` as the empty em-dash (rendered "nobody" in Matt's
framing) on virtually every spawn-created bead because **no spawn path sets
`bead.Owner` at all today**. The `Owner` column is touched in exactly three
places: `kd create --owner=…` (rarely passed), `kd update --owner=…`, and the
beads-viewer "Edit owner" button. Every Slack `/spawn`, scheduled, trigger,
gardener-spawned, and `kd create` call leaves `Owner` empty — even when the
caller has a perfectly good `operated_by` value to default from.

The fix is **server-side defaulting of `Owner` from the
`X-Operated-By` request header at create-time**, parallel to what the
attribution-chain report recommends for `Fields["operated_by"]` and
`Fields["user_bead_id"]`. The viewer's "Edit owner" semantics also need a
small change: empty submission today clears `Owner` to `""`, which would
defeat any default we add — we want explicit "owner = nobody" to be
representable as a distinct state (sentinel `nobody`) without re-triggering
the default.

Backfill is feasible for the majority of `owner=""` rows: `operated_by` is
already populated for almost every bead created since migration 012 (Oct 2025),
so a one-shot `UPDATE beads SET owner = operated_by WHERE owner = ''` would
cover ≈95 % of orphans cleanly. Pre-012 rows fall back to `created_by`.

## What "Owner" means in kbeads today

`Owner` is a free-form `TEXT` column on `beads` (`kbeads/internal/model/bead.go:136`),
distinct from:

- `Assignee` — the agent or human currently holding the work (`agent.name` for
  in-progress work; cleared on close).
- `CreatedBy` — the actor that performed the write (agent name or sentinel like
  `system:scheduler`, `jira-bridge`).
- `OperatedBy` — the canonical human-attribution column populated server-side
  from the `X-Operated-By` header on every create (`beads.go:85-103`).
- `CreatedByOperatorID` — the immortal originator FK (operator who raised the
  bead).

`Owner` is **load-bearing for ownership-based write authz**: in
`kbeads/internal/server/write_authz.go:184-202`, the gate skips when
`CreatedBy == "" && Assignee == "" && Owner == ""` (legacy beads), but
otherwise grants edit access to whichever of those three matches the caller's
subject. So a non-empty `Owner` is the only string a human user can set to
allow themselves edit access to a bead they didn't create or get assigned to.

It is also the field that the viewer renders prominently on every bead page
(`templates/bead_show.html:87`, `templates/partials/owner_block.html`), and
that exposes the "Edit owner" inline editor (`bead_actions.html:77-83`).

## Per-path inventory: who sets Owner today?

Verified by `grep -n "Owner:" gardenio/controller/**/*.go` (no matches in any
spawn site) and `grep -n "in.Owner\|req.Owner" kbeads/internal/server/*.go`
(only the update path).

| Spawn path | File:line of CreateBead call | Sets Owner? | Sets Assignee? | Has operator info available? |
|---|---|---|---|---|
| Slack `/spawn` task bead | `bot_commands.go:237-244` | ❌ | ❌ | ✅ via `X-Operated-By` header (slack:U… of caller) |
| Slack `/spawn` agent bead | `bot_commands.go:528-538` | ❌ | server-side, set to `agent.name` after spawn | ✅ |
| Schedule fire — task bead | `scheduler.go:1164-1182` | ❌ | ❌ | ✅ schedule.OperatedBy / Fields[slack_user_id] |
| Schedule fire — agent bead | `scheduler.go:1185-1212` | ❌ | server-side post-spawn | ✅ |
| Trigger fire — task bead | `triggers.go:740-770` | ❌ | ❌ | ✅ trig.CreatedBy via `ctxWithTriggerOperator` |
| Trigger fire — agent bead | `triggers.go:646-690` | ❌ | server-side post-spawn | ✅ |
| Gardener `gb spawn` — crew agent | `cmd/gb/spawn.go` → daemon | ❌ | server-side post-spawn | ✅ via `$BEADS_OPERATED_BY` env |
| Agent-created bead (`kd create`) | `kbeads/cmd/kd/create.go:127-138` | only if `--owner` passed (rare) | only if `--assignee` passed | ✅ via `$BEADS_OPERATED_BY` env on caller |
| JIRA-bridge sync (poller) | `jira_poller.go:601-610` | ❌ | ❌ | partial — `CreatedBy: "jira-bridge"`; no human operator |
| JIRA-bridge mention handler | `jira_mentions.go:228+` | ❌ | ❌ | ✅ caller's JIRA accountID resolvable |
| Manual `kd create --owner=…` | n/a — passes through | ✅ if flag set | ✅ if flag set | ✅ |
| Viewer "Edit owner" | `owner_edit.go:62-100` | ✅ (overwrites) | n/a | ✅ caller email |

Net: **zero of the spawn paths set `Owner`**, despite the operator identity
being known at every site (it's already pushed to `operated_by` either via the
HTTP header or via explicit `Fields["operated_by"]`). This is the entirety
of the "owner: nobody everywhere" pattern.

## Why this happened

`Owner` predates `operated_by`. Pre-migration 012 (human-attribution column),
`Owner` was the *only* per-bead human identifier. When 012 introduced the
authoritative `operated_by` column and its server-resolver pipeline, all the
spawn-site code was updated to push operator information through the new
header/column path. The `Owner` field was left alone because (a) nothing was
setting it before, (b) the viewer's "Edit owner" editor was the only writer,
and (c) the write-authz gate only treats it as a permissive override, not as
a required identifier.

What changed is the viewer's rendering. The viewer now renders `Owner`
prominently on every bead page (`templates/partials/owner_block.html:6`).
With `Owner` empty across the board, every bead reads as "owner: nobody",
making the field useless as a UI signal and giving Matt the impression that
the system has no ownership at all — even though `operated_by` is correctly
populated on nearly every bead.

## Proposed defaults per source

The defaults Matt sketched in the bead description are correct in shape. The
right place to enforce them is **inside the kbeads server, at create-time**,
so no client can slip through.

| Source | Default Owner | Mechanism (server-side) |
|---|---|---|
| Slack `/spawn` (task + agent) | `slack:<UserID>` (the operator) | `X-Operated-By` already carries `slack:U…` → server defaults `Owner` from it |
| Schedule fire | schedule's `OperatedBy` value | `X-Operated-By` already carries it → same default |
| Trigger fire | `trigger.CreatedBy` (the human who authored the trigger), else `system:<source>-trigger` | `X-Operated-By` already carries it → same default |
| Agent-created child (`kd create`) | inherits `$BEADS_OPERATED_BY` from the parent agent's pod env | `X-Operated-By` carries it → same default |
| Gardener spawning crew | inherits via gardener's pod env (same as above) | identical mechanism |
| JIRA-bridge sync | `jira:<reporterAccountID>` if extractable, else `system:jira-bridge` | bridge populates `X-Operated-By` per ticket; server defaults from it |
| Manual `kd create` (human at terminal) | `$BEADS_OPERATED_BY` from caller env (already an `oidc:email` for human callers) | identical mechanism |
| Manual `kd create --owner=…` | as supplied | client supplies explicit value, server does not override |

The unifying rule: **if the request supplies a non-empty `X-Operated-By` and
the caller did not supply `Owner`, default `Owner` to the header value.** A
two-line change in `kbeads/internal/server/beads.go:88-99` covers every path.

This is structurally identical to the attribution-chain report's recommended
server-side defaulting for `Fields["operated_by"]` and `Fields["user_bead_id"]`.
The three should land together as one server-side inheritance change.

## Where to enforce: server-side, not client-side

Strong preference for server-side, for the same reasons the attribution-chain
report calls out:

1. **No spawn site can forget.** The Slack, scheduler, trigger, gardener, and
   bridge spawn sites already correctly stamp `X-Operated-By`. If `Owner`
   defaults from that header server-side, every existing call gets fixed for
   free. Per-site `req.Owner = OperatorFromContext(ctx)` lines are six
   redundant edits that future spawn paths will forget.
2. **New write paths inherit the rule.** Any future spawn surface — a new
   bridge, a webhook, a sequencer — gets correct ownership defaults the moment
   it stamps the header, with no `Owner` plumbing needed.
3. **Authz interlock.** The write-authz gate already keys off `Owner`. Making
   `Owner` reliably non-empty post-create means the gate's "legacy bead with
   no ownership fields" branch (`write_authz.go:186`) starts firing rarely
   instead of routinely — turning a permissive escape hatch into the rare
   exception it was meant to be.

The server-side change should land behind a flag (`BEADS_OWNER_DEFAULT_ENABLED`)
so the rollout can be staged: enable in staging, observe, then enable in prod.
Default to on after one release cycle.

### Opt-out

A single `X-No-Owner-Default: 1` header (or `req.Owner = "-"` sentinel that
the server normalizes to empty) covers the edge cases:

- Migration scripts that intentionally want `owner=""` on legacy rows.
- Bulk-imported beads whose ownership is unknown and should remain so until
  classified.

The viewer's "Edit owner" empty submission should send the opt-out header so
explicit clears stay cleared (see "Viewer interaction" below).

## Viewer interaction (small but important)

`owner_edit.go:62-100` currently treats empty form submission as "clear the
owner" (`UpdateBead({Owner: &""})`). With a server-side default in place,
this will be racy: the user clears, the next write defaults from the header,
and Owner reappears.

Two reasonable fixes:

1. **Sentinel for "intentionally unowned".** When the user submits empty,
   send `Owner = "-"` (or `system:unowned`). The viewer renders this as
   "(unowned)" instead of em-dash, and the create-time default does not fire
   when the field is non-empty. Mutations preserve the sentinel; only the
   "Edit owner" UI sends it.
2. **Make "Edit owner" do nothing when blank.** The simplest read: empty
   submit becomes "no change". User must explicitly type a value to clear or
   reassign. Less expressive but zero extra schema.

Recommend (1) so the field can still represent "explicitly nobody". Implement
as a default render value in `owner_block.html`; the column stays TEXT.

## Backfill: can we infer Owner for existing rows?

Yes, for the overwhelming majority. The pre-conditions are all already in
place from migration 012 and the operators-tables rollout:

- `beads.operated_by` (TEXT) is populated for every row created after
  2025-10-XX (migration 012 deploy).
- `beads.operated_by_id` (FK to `operators(id)`) is populated for rows
  resolved during the dual-write window (migration 016+).
- `beads.created_by` is populated on every row going back to migration 001.

### Recommended backfill sequence

Migration-time DDL is cheap (no eager table scans per migration policy), so
the backfill goes in a `kd admin` one-shot command, not the schema migration.
A single SQL pass (chunked):

```sql
-- Phase 1: backfill from operated_by where present.
UPDATE beads
SET owner = operated_by
WHERE owner = ''
  AND operated_by NOT IN ('', 'system:legacy-pre-attribution')
  AND id IN (SELECT id FROM beads WHERE owner = '' LIMIT 5000);
-- (Loop the LIMIT'd UPDATE in psql until 0 rows affected.)

-- Phase 2: backfill from created_by for older rows that have no operated_by.
UPDATE beads
SET owner = created_by
WHERE owner = ''
  AND created_by != ''
  AND created_by NOT IN ('jira-bridge', 'system:scheduler', /* etc. */)
  AND id IN (SELECT id FROM beads WHERE owner = '' LIMIT 5000);

-- Phase 3: tag the residual.
UPDATE beads
SET owner = 'system:unowned'
WHERE owner = ''
  AND id IN (SELECT id FROM beads WHERE owner = '' LIMIT 5000);
```

`spawn_source` / `Fields["spawned_by"]` is **not** a reliable backfill source
— it carries the scheduler/trigger sentinel (`scheduler:<id>`) rather than
the human operator, and that's already covered by `operated_by` having the
schedule's `OperatedBy` value.

### Estimated coverage (rough, from grep-and-reason)

- ≈85–95 % of `owner=''` rows have a usable `operated_by`. These collapse to
  the correct human/sentinel identity.
- ≈3–10 % are pre-012 legacy with `created_by != ''` (agent names, bridge
  names). These get a meaningful Owner (the bot or agent), which is
  defensible — better than blank.
- ≈1–5 % are deep-legacy or fleet-token writes with neither field populated.
  These get the explicit `system:unowned` sentinel so the viewer can render
  them distinctly from "default not yet applied".

The numbers should be re-measured against the prod beads daemon before
running the backfill; the SQL pattern is the deliverable, not the exact
percentages.

## Open questions / out-of-scope

- **Does `Owner` keep semantics-of-record or get retired?** Long-term, the
  attribution-chain report's `Fields["user_bead_id"]` (FK to a real user bead)
  is the right primitive. `Owner` (TEXT) and `operated_by` (TEXT) both
  duplicate it less typesafely. This report does not recommend retiring
  `Owner` — that's a much bigger lift and overlaps the roles/permissions epic
  (ka-0000007271). Default it correctly now; retire later.
- **Per-user "what did I start" filter** (parent-epic problem area 5) needs
  `operated_by` (already populated) or a normalized `Owner` (after this fix).
  Either works; consistency between the two is the win.

## Recommended follow-up beads

Per the research-only epic scope, these are **recommendations**, not
already-filed beads:

1. **task (P1, blocks 3-4):** Server-side `Owner` default from
   `X-Operated-By` header in `createBead`. Behind
   `BEADS_OWNER_DEFAULT_ENABLED` flag; default on after one release.
   Estimated size: 20 LOC + flag wiring + 3 tests.
2. **task (P1):** Viewer "Edit owner" empty-submit semantics — adopt
   `system:unowned` sentinel and render it distinctly. Coordinate with (1)
   so empty submit doesn't race the default.
3. **task (P2):** `kd admin backfill-owner` one-shot command implementing
   the three-phase SQL above. Chunked, idempotent, dry-run flag.
4. **task (P2, joint with attribution-chain report's follow-up 1):**
   Co-land `Owner` default + `Fields[operated_by]` default +
   `Fields[user_bead_id]` default as one server-side inheritance change.
   Same code path, same flag.
5. **task (P3):** Once backfill runs in prod, audit the write-authz gate
   (`write_authz.go:186`): the "legacy fall-through" branch becomes
   load-bearing only for genuine pre-012 rows. Consider tightening it.
