# Farm history for the /garden time-lapse — design

Task: gd-CP6HlODpWC. Phase 4 of the Homestead World plan (gd-oi3liYjNRA, phase
gd-BrMFV0JbHL). Owner GO on Phase 4: gd-fnMBVK2Mqb (2026-10-23). This is the only
backend change in the plan, so it starts with this design and an owner decision on
**where farm history is stored and how long it is kept**. Nothing below is built
yet. Sections 3 and 4 are the proposal; section 9 lists the choices that are the
owner's.

## 1. Goal

A scrubber on `/garden` lets a viewer drag back through time and watch the farm
change: plans sprout from seeds, phases fill and close, gardeners walk between
plots, the meadow grows and is adopted. Press play and a week runs in a few seconds.
Release it and the farm is live again.

What a frame must carry is what the world draws: plans with their phases, counts
and listed tasks, seeds, gardeners and their plots, the meadow, closed plans. That
is the `/garden/farm` document ([`homestead-farm-read-model.md`](../../../gardenio/docs/homestead-farm-read-model.md)).
Bead descriptions, comments and the Mailbox letters are not in the document and are
not in history either.

Non-goals: an audit trail (`mutation_history` and `events` already are one), an
exact replay of every patch, history for `/plans` or the beads-viewer pages, and
undo.

## 2. What exists today

| Thing | State |
|---|---|
| Farm document | Built in memory in beads-viewer (`internal/gardenhub/farm.go`) from daemon reads; about **13 KB gzipped** on the live farm, about 40x smaller than raw. The fixture (`testdata/farm_fixture.json`) is 13.6 KB raw, 2.1 KB gzipped. |
| Farm patches | `{v, prev, plans/seeds/gardeners: {up, rm}, closed_plans?, meadow?, piles?, meta?, cost?}`: a whole plan or seed object per change, singletons only when they changed. Kept in a 256-patch in-memory replay ring for SSE catch-up. Nothing is stored. |
| beads-viewer storage | None. No database, no volume; it is a client of the daemon. Reads go out as the system (`beadsapi.WithSystemRead`); its machine principal (`gardenio-beads-viewer`) exists in the chart, off by default. |
| Daemon Postgres | `events` (topic, bead_id, full bead payload as JSONB, `created_at`) and `mutation_history` (per-field old/new) are append-only and **never pruned**, except by the land purge. `events` is indexed by `bead_id`, `topic`, `fleet` and `operated_by_id, created_at`, not by `created_at` alone. |
| MinIO | In-box object store; the viewer holds read credentials for the coop transcript bucket (the `/image` proxy). |
| Land purge | `PurgeBeads` (Lands 8.11) hard-deletes a closed land's beads, `events` and `mutation_history` rows 30 days after `land_closed_at`: the privacy policy's deletion promise. **Any history we keep must honour it.** |

## 3. Options

> **Decided (2026-10-23 21:38Z, gd-Vc4F5txThE): A, Postgres frames, with the
> recommended defaults.** That means 5-minute frames for 14 days, hourly to day 90,
> daily to 2 years, a 128 MB cap, and older frames stop rendering after a schema
> bump (choices 2-4 in section 9 were left at their recommendations). There is no
> backfill (D), no MinIO (B) and no replay (C). The build follows section 7, items
> 1-3. An owner hold set at 04:52Z ("let's hold the history and scrubber thing") is
> still on gd-CP6HlODpWC. On 2026-10-24 (gd-FNcrgzLH3l) the owner kept that hold and
> had this design merged as the record, so nothing is built until the owner lifts it.
> The build is split into held beads: gd-lRnOSsasXs (kbeads `farm_frames`),
> gd-ECYKo9E5II (beads-viewer recorder and `/garden/farm/history`) and gd-7BvezKcwee
> (`timelapse.js` and Menu > Time-lapse).

### A. Recorded frames in the daemon's Postgres (recommended)

beads-viewer already holds the document it would store. Every 5 minutes it takes the
in-memory snapshot (no daemon read), splits it into one **shard per project**,
encodes each shard against that project's hourly keyframe, and posts the gzipped
bytes to a new daemon endpoint. The daemon stores them as opaque bytes in one new
table and never parses them. Section 4 has the details.

- **Storage.** One table, `farm_frames`, in the existing kbeads Postgres. One
  migration, no new infrastructure, no chart change. It is backed up with the rest
  of the daemon's data.
- **Fidelity.** A frame is exactly what the live page drew at that moment, caps and
  all. Gardener moves are seen at 5-minute steps, and the client tweens between
  frames.
- **History starts the day it ships.** Nothing before that, unless option D
  backfills it.
- **Cost.** About 64 MB at steady state with the default tiers (section 5), one small
  POST every 5 minutes, and two indexed reads per scrub step.

### B. Recorded frames in MinIO

The same recorder writes an object per shard per frame
(`farm/<project>/<yyyy>/<mm>/<dd>/<hhmm>.json.gz`).

- Needs a chart change: write credentials for the viewer, a bucket or prefix, and a
  lifecycle rule. That is helm and deploy-values work, which goes through an
  operator escalation.
- S3 has no project-scoped auth, so only the viewer can read it, and every
  scoped-viewer check is ours to write anyway.
- Listing objects for the scrubber's index is slower than an indexed query. Tier
  downsampling is custom code either way, because lifecycle rules only expire by age.
- Purging a land is easy: delete its prefix.

It has no advantage over A at 64 MB and adds moving parts, so it is not recommended.

### C. Replay from the daemon's event history

Rebuild the farm at time T from `events`: take the newest event per bead at or before
T, rebuild the open set, deps and agents, then run the farm projection over that.

- Needs no new storage, and history would reach back to the oldest event.
- **It is costly to read.** Every frame is a `DISTINCT ON (bead_id)` scan across
  `events` up to T, over every work and agent bead. Agent beads update every few
  seconds, so the table is large. It needs a new `created_at` index on that table,
  and that index has a cost of its own.
- **It is not faithful.** The projection runs through `FarmSource` list semantics
  (status filters, children-by-status counts, `blocks` edges, the agent roster). A
  temporal copy of each would have to be written and maintained for every new farm
  field. Backfills that wrote SQL directly bypassed `events`. Payload shapes have
  drifted over time. Cost and purse figures come from VictoriaMetrics, which has its
  own shorter retention.
- It renders the past with today's projection code. That keeps the drawing
  consistent, but it is not what anyone saw on the page.

It is not recommended as the primary store.

### D. A, plus a one-off backfill from events (both)

Ship A. Then, as a separate step, run a one-off job that writes **one frame per day**
for the days before recording began, rebuilt from `events` the way C describes, at
the daily tier only. It is labelled `backfill` in the index, so the scrubber can say
"reconstructed". This brings C's correctness gaps into older history, and it is
about as much work again as A. Choose it if the owner wants the scrubber to reach
back to the start of the plan on day one.

### Rejected: in-memory history in beads-viewer

A ring of recent frames in the viewer's memory needs no daemon change, but every
deploy and restart wipes it, and releases roll out more than once a day. With more than one
replica, each replica would also hold a different history.

## 4. Option A in detail

### 4.1 Frames, shards and keyframes

A **frame** is the farm at one wall-clock slot, aligned to the 5-minute grid (`at`,
unix seconds, a multiple of 300). It is stored as one **shard** per project plus one
fleet shard:

- **Project shard** (`project = <name>`). A farm document cut to that project: its
  plans, seeds, closed plans and meadow rows, plus the gardeners standing on its
  plots. A gardener at home goes in its own project's shard. The shard also holds
  that project's `cost` row. It has the same shape as `GET /garden/farm`, so the
  reader merges shards by concatenating arrays.
- **Fleet shard** (`project = ""`). `piles`, `meta` and `built_at`. It is written at
  every slot, so it also records coverage: a slot without a fleet shard is a gap
  (the recorder was down).

Cross-project references are cut **when the frame is written**, with the owner's
`named` rule (gd-pnmEvqmfBW): a `blocked_by` id in another project is dropped and
only `blocked_by_project` keeps that project's name. A shard therefore never holds
another project's bead ids, so the scoped read and the land purge (4.5) stay simple.
The time-lapse draws that edge as a bridge labelled with the other project, as a
scoped viewer sees it live.

Encoding:

- At the top of every hour (`at % 3600 == 0`) each project's shard is written
  **whole**: that is its **keyframe** (`key_at = at`).
- At the other 11 slots, each shard is written as a **patch from its hour's
  keyframe**. It uses the existing farm patch model (`plans/seeds/gardeners
  {up, rm}` plus changed singletons) and the existing `diffEntries`. A delta is never
  chained to the previous delta, so any frame is one keyframe plus one patch, and
  dropping intermediate frames never breaks one.
- A delta identical to the previous slot's delta for that shard is not written. The
  reader takes the newest frame at or before `at`, so skipping it loses nothing.
  Keyframes are always written.
- A project whose shard became empty (its last plan closed and nothing is left in
  the meadow) gets one empty keyframe, a tombstone, so it does not linger.
- Bodies are gzipped JSON. Each one carries `meta.schema`. A reader that meets a
  newer schema than it knows skips that frame. A schema bump with no migration
  means frames older than the bump stop rendering (section 9, choice 4).

### 4.2 Daemon table and API (kbeads)

```sql
CREATE TABLE farm_frames (
    project    TEXT        NOT NULL,          -- '' = the fleet shard
    at         TIMESTAMPTZ NOT NULL,          -- slot, multiple of 5 min
    key_at     TIMESTAMPTZ NOT NULL,          -- = at for a keyframe
    tier       SMALLINT    NOT NULL DEFAULT 0,-- 0 fine, 1 hourly, 2 daily, 3 backfill
    body       BYTEA       NOT NULL,          -- gzip(JSON), opaque to the daemon
    bytes      INTEGER     NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project, at)
);
CREATE INDEX idx_farm_frames_at ON farm_frames (at);
```

| Route | What |
|---|---|
| `POST /v1/farm/frames` | `{frames: [{project, at, key_at, body}]}` (body base64). `INSERT ... ON CONFLICT DO NOTHING`: a second viewer replica posting the same slot is harmless. Rejects an `at` off the grid or in the future, a body over 1 MB, and more than 200 shards in one call. Allowed for the viewer's service identity only (4.6). |
| `GET /v1/farm/frames?at=&projects=` | For each project asked for (default: every project the caller may read, plus `""` for an admin), the newest keyframe at or before `at` and the newest delta in `(key_at, at]`. Two index range reads per project. |
| `GET /v1/farm/frames/index?from=&to=` | The fleet shard's slots as run-length segments, `[{from, to, step, tier}]`. Gaps are what lies between segments. |

The daemon decodes nothing. It only stores, selects, prunes and purges.

### 4.3 Recorder (beads-viewer)

There is one goroutine per `Farm`. It wakes at each 5-minute slot and takes the
memoized snapshot under the farm lock, with no daemon call. It cuts the snapshot
into shards and keeps each shard's current keyframe in memory, so it can diff
against it. It posts all of a slot's shards in one call. A failed post is retried
once at the next slot and then dropped. The scrubber shows a dropped slot as a gap,
and the live farm is never affected. It skips a slot when the farm has never
refreshed successfully, or its last refresh is more than 2 minutes old: recording a
stale farm would be a lie.

A restart re-reads the current hour's keyframes from the daemon, or writes a fresh
keyframe at the next slot when there is none. A delta always points at a keyframe
that exists.

### 4.4 Read API (beads-viewer, `/garden` route class)

| Route | What |
|---|---|
| `GET /garden/farm/history` | The index: `{now, segments: [{from, to, step, tier}]}`. The scrubber's track draws it, with gaps hatched. `Cache-Control: no-store`. |
| `GET /garden/farm/history?at=<unix>` | The farm document as of the newest frame at or before `at`. It has the same shape as `GET /garden/farm`, plus `history: {at, tier, reconstructed?}`, and `v` is the frame time in ms. 404 before the first frame. A client snaps `at` to a frame time from the index. A frame never changes, so the response is `Cache-Control: private, max-age=86400, immutable` with gzip. |

The viewer reassembles a frame: it fetches the shards from the daemon, applies each
delta to its keyframe with a Go `applyPatch` that mirrors the client's, merges the
shards, applies the viewer's cut (4.6) and gzips. Reassembled documents go into a
small LRU (64 entries, about 1 MB gzipped), so dragging back and forth over the
same hour makes no daemon read. A history document is never sent on the SSE stream,
and the client does not apply live patches while it shows one.

Each scrub step costs, worst case, one daemon call with about 2 x (number of
projects) index reads, and a few hundred KB of gunzip and JSON in the viewer. The
client prefetches the next few frames while playing.

### 4.5 Retention, downsampling and purge

A sweep in the daemon runs hourly under its own advisory lock (like
`audit_retention.go`):

| Tier | Keeps | For | Then |
|---|---|---|---|
| fine (0) | every 5-minute frame | 14 days | deltas deleted, hourly keyframes become tier 1 |
| hourly (1) | the keyframe at each hour | until day 90 | keyframes not at 00:00 UTC deleted, the midnight one becomes tier 2 |
| daily (2) | one keyframe a day | until day 730 | deleted |

**Byte cap.** If the table grows past `FARM_HISTORY_MAX_MB` (default 128), the sweep
deletes the oldest fine-tier deltas first, then hourly, then daily, until it is
under the cap. It logs what it dropped and exports `farm_history_bytes` and
`farm_history_dropped_total`. A farm that grows 10x never fills the disk: its fine
tier just gets shorter. The defaults are in code, so no chart value is needed; env
overrides exist for the daemon.

**Land purge.** `PurgeBeads` for label `project:<land>` also runs
`DELETE FROM farm_frames WHERE project = <land>`, in the same transaction. Because
shards never carry another project's bead ids (4.1), nothing of the land's beads is
left in other shards. Two things can remain, and they are named and accepted: the
fleet shard's pile counts, and a `blocked_by_project` naming the land on another
project's step. A tenant's gardener standing on an operator plot would also stay in
that plot's shard. The build checks whether that can happen, and if it can, the
shard cut files that gardener under its own land instead.

### 4.6 Who may read and write

- **Write:** `POST /v1/farm/frames` accepts only the viewer's service identity. That
  is the daemon token it already uses for system reads. Once the static token is
  retired (gd-ke6PC7bRto), it is the viewer's machine principal with a new realm
  role, `gardenio-machine-farm-history`. Adding that role to the
  `gardenio-beads-viewer` Keycloak client is a deploy-values change. I will file it
  as an operator escalation when the build reaches it, and I will not edit
  helm/deploy-values.
- **Read, today:** `/garden/farm/history` is `routeAdminOnly`, like every
  `/garden*` route. An admin sees every shard.
- **Read, once `/garden` opens to project-scoped viewers** (gd-U2pbNv3lLC, then
  reclassification gd-l7yYxZpoZo): the viewer fetches only the shards of the
  viewer's projects, never the fleet shard. Piles and `meta` counts are recomputed
  from those shards, as the live cut does. One extra rule applies: **you see the
  past only of what you can read now.** Every bead id in a reassembled frame is
  checked against the viewer's current read access in one batch call, cached for
  60 s. Any id the viewer cannot read today is dropped, with the same fail-closed
  rule as the live cut. This covers a bead that was later moved to another project
  or gate-tainted. Until the scoped cut lists history, a non-admin gets 403.

## 5. Size budget (option A, default tiers)

Measured: the live farm is about 13 KB gzipped, and the fixture is 2.1 KB. Assumed,
to be measured in the first week: a full set of per-project shards gzips slightly
worse than one document (15 KB), and an average delta from the hour's keyframe is
about 40% of a full shard set (6 KB).

| Tier | Per day | Kept | Total |
|---|---|---|---|
| fine | 24 keyframes x 15 KB + 264 deltas x 6 KB, about 1.9 MB | 14 days | about 27 MB |
| hourly | 24 x 15 KB, about 0.36 MB | days 15 to 90 | about 27 MB |
| daily | 15 KB | days 91 to 730 | about 10 MB |
| **Steady state** | | | **about 64 MB**, under the 128 MB cap |

About 10 projects x 288 slots means roughly 3,000 rows a day, and about 65,000 rows
at steady state. Write load is one POST of about 15 small shards every 5 minutes.
The recorder logs each slot's bytes, so the assumption can be checked on the live
farm in the first week, and the cap holds regardless.

## 6. The scrubber (after the decision)

- **Entry: the Menu.** Add one item, **Time-lapse** (key `T`), after Tend. It opens a
  card in the slot the Tend card already uses beside the map (`#tour`'s place). That
  is not new floating chrome (3b). The card holds the track, which draws the index
  segments with gaps hatched, plus a play/pause button, a speed control (1 day per
  5 s, 10 s or 30 s), the frame time ("Tue 21 Oct, 14:35"), and **Back to now**.
  Closing the card is Back to now.
- **On the map.** While time-lapse is on, the world draws the frame document instead
  of the live one. Between two frames the client computes the patch and feeds it to
  the same transitions the live stream uses (`web/transitions.js`), so plants grow
  and gardeners walk. The live SSE stays connected and keeps its version, but its
  patches are held. Back to now takes a fresh reset. The live indicator (`#live`)
  line says "Time-lapse: Tue 21 Oct 14:35, Back to now". Writes are off
  (Wake, Adopt, Answer), and the bead view opens the bead as it is now, with a note
  saying so.
- **Keyboard and a11y.** The track is a `role=slider` with `aria-valuetext` set to the
  frame time. Left and Right step one frame, PageUp and PageDown step a day, Space
  plays, and Esc goes back to now. A phone gets 44 px targets. A reduced-motion
  viewer gets frame steps without tweening.
- **Code.** `web/timelapse.js` is a pure module: it fetches the index, snaps to
  frames, prefetches, computes the patch between two frames, and runs the play clock.
  It has node tests like `curnav_test.js`. Wiring goes in `garden.js` and
  `garden.html`, and I grade it with `/commons-ux-acceptance` (phone, desktop, dark
  and keyboard captures).

## 7. Work breakdown (after the decision)

1. **kbeads** (PR 1): migration `057_farm_frames`, store, the three routes, the
   retention sweep, the purge hook, and tests (grid validation, idempotent insert,
   tiering, the cap, purge).
2. **gardenhub** (PR 2): the shard cut, the keyframe/delta encoder, the recorder,
   `applyPatch`, `/garden/farm/history`, the LRU, and tests. These include a
   round-trip test (record 24 h of a synthetic farm, then read any slot back
   byte-identical to the snapshot cut) and a budget test like `farm_test.go`'s. Doc:
   a History section in `homestead-farm-read-model.md`.
3. **Client** (PR 3): `timelapse.js`, the Menu item and card, and the UX acceptance.
4. ~~If D was chosen: the backfill job (PR 4).~~ Not chosen.
5. If a role is needed (4.6): one operator escalation for the Keycloak role.

Each PR is a child bead of gd-CP6HlODpWC.

## 8. Risks

- **Schema drift.** A farm field that changes meaning breaks older frames. Mitigation:
  `meta.schema` in each frame, readers skip unknown schemas, and the history doc
  says so.
- **Privacy.** History keeps titles that the live farm later drops, for example a bead
  retitled to remove something. The scoped read's "readable now" rule and the land
  purge cover access and deletion. A title changed for privacy stays in admin
  history until its tier expires (at most 730 days). Choice 3 in section 9 can
  shorten that.
- **Shipping before the scoped cut.** History stays admin-only, like the rest of
  `/garden`, until gd-U2pbNv3lLC lists it.

## 9. Choices for the owner

1. **Storage:** A (Postgres frames, recommended), B (MinIO), C (replay from events),
   or D (A plus a daily backfill from events).
2. **Granularity:** 5-minute frames (recommended), or 1-minute (about 5x the fine
   tier, about 130 MB for 14 days at the current size).
3. **Retention:** 5-minute for 14 days, hourly to 90 days, daily to 2 years, with a
   128 MB cap (recommended). Or another set: say the days and the cap.
4. **Schema bumps:** let older frames stop rendering (recommended, cheap), or require
   a frame migration with every schema bump.
