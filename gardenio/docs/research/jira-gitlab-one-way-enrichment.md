# Jira / GitLab Beads — One-Way Enrichment & Comment Sync

**Status:** research / design proposal
**Date:** 2026-05-03
**Tracking bead:** ka-0000002498
**Drives:** new advice bead "How to treat GitLab/Jira-sourced beads"

## Goal

Treat the **external ticket / MR as the source of truth** and beads as a
read-only mirror enriched with everything an agent needs to act on the
ticket without round-tripping to Jira or GitLab. Two requirements:

1. **Enrich** Jira- and GitLab-sourced beads with the full set of fields,
   labels, attachments, and comments from the upstream ticket / MR.
2. **Keep them updated** continuously — drift between bead state and
   upstream state should self-heal within minutes.
3. **One-way only.** Data flows IN to beads; bead changes never write
   back to Jira or GitLab. Comment ingestion in particular must be
   **echo-loop-proof, idempotent, and safe across restarts**.

This is a scope shift. Today both bridges round-trip:

- `jira-bridge` posts comments on claim, MR-link, MR-merge, and bead
  closure; transitions issues; optionally self-assigns.
- `gitlab-bridge` writes a managed section into the MR description
  (`syncMRDescription`).

The proposal is to **switch off all outbound writes** behind a feature
flag, then layer in inbound enrichment & comment ingestion.

## Today's bridges in one paragraph each

### Jira (`gardenio/controller/cmd/jira-bridge`, `internal/bridge/jira*.go`)

- **Poller** (`jira_poller.go`): JQL search every 60s. **First-sight
  only**: creates a task bead with a snapshot of summary, description,
  labels, status, type, priority, reporter, assignee, parent, issue
  links count, and attachment counts. After that, the poller never
  re-syncs description, comments, status, labels, or assignee — it only
  updates `jira_xlinks` and reacts to assignee changes (close-on-human-claim).
- **Webhook** (`/webhook` on `jira-bridge`): handles `comment_created`
  events but **only for `@gardenio` mentions** (nudges/spawns). Plain
  comments are not ingested as bead comments.
- **Sync OUT** (`jira_sync.go`): SSE-driven; posts comments + transitions
  + remote links + self-assigns to Jira when bead state changes (claim,
  MR link, mr_merged, close). FSM project is blocked from any write-back.

### GitLab (`gardenio/controller/cmd/gitlab-bridge`, `internal/bridge/gitlab*.go`)

- **No MR poller.** Beads are typically created out-of-band by agents,
  with `mr_url` set after the agent opens an MR. There is no equivalent
  of the Jira `IN ("PE","DEVOPS")` JQL sweep.
- **Webhook** (`/webhook` on `gitlab-bridge`): MR/note/pipeline/approval
  events update `mr_state`, `mr_pipeline_status`, `mr_approved`,
  `mr_approvers`. **Review notes (with file position) are ingested as
  bead comments** — this is the only existing comment-sync path in
  either bridge.
- **SSE-driven enrichment** (`gitlab_sync.go`): when a bead with
  `mr_url` is updated, fetches MR state and stamps `mr_state`,
  `gitlab_mr_iid`, `gitlab_project_id`, `mr_pipeline_status`,
  `mr_pipeline_url`, `mr_merged`.
- **Sync OUT** (`syncMRDescription`): writes a `<!-- gardenio -->` section
  into the MR description with bead/JIRA status. This is the one path
  that pushes data from beads → GitLab.

## What the current shape gets wrong (under the new requirement)

| Gap                                                                                 | Impact |
|-------------------------------------------------------------------------------------|--------|
| Jira description / status / labels / priority captured **once at creation** only    | Beads grow stale within hours; agents act on outdated context |
| Jira comments not ingested at all (except `@gardenio`)                               | Agent can't see the back-and-forth that explains the ticket |
| GitLab MRs have no equivalent of the Jira poller                                    | New MRs never become beads unless an agent opens them |
| GitLab MR description, labels, milestones, draft state, conflicts, threads not ingested | Bead is mostly empty even when an MR exists |
| Outbound writes happen in both bridges                                              | Violates "one-way only"; also generates the loop-safety burden below |
| No persisted dedup of upstream comment IDs                                          | Webhook re-delivery and SSE replay can dupe bead comments |
| Edits/deletions of upstream comments not handled                                    | Once-a-comment-arrives, can never be corrected |

## Design

### Principle: external system is canonical, bead is the mirror

```
            ┌────────────────────┐       ┌────────────────────┐
            │   Jira / GitLab    │  one-way-IN              │
   author ─►│ (canonical)        │──────────────► │  bead │
            │ comments, fields,  │   webhook + poll          │
            │ description, ...   │                           │
            └────────────────────┘                           │
                                                            ▼
                                              agents read the bead
                                              (operate on real ticket)
```

There are exactly two write paths today (jira-sync, mrdesc-sync) and
both go away. From this point forward the bridges are **ingestion-only**
plus the existing `@gardenio` mention spawn (mention is a read of an
inbound comment that triggers a *bead-side* action — no upstream write).

### Phase 1 — Disable Jira outbound writes; keep GitLab MR-description as opt-in

Per operator (Matt, 2026-05-03 — Slack mention bead `ka-0000005199`):

> "we can keep that for Gitlab and make it optional and disable for Jira"

- **Jira: hard-disable outbound.** Remove (or always-skip) the calls to
  `AddComment`, `TransitionIssue`, `AddRemoteLink`, `AssignIssue` in
  `JiraSync.handleUpdated` / `handleClosed`. The simplest path is to
  replace the body of those handlers with logging-only and delete the
  `JiraSync` registration entirely once we are confident — but ship a
  `JIRA_DISABLE_WRITES=true` flag first (default `true`) so we have a
  one-flag rollback.
- **GitLab: keep `syncMRDescription` as the one outbound path, gated
  off by default.** Add `GITLAB_MR_DESCRIPTION_SYNC=false` (default
  `false`); flip to `true` only on namespaces where the operator wants
  the managed `<!-- gardenio -->` section in MR descriptions. Everything
  else (MR comments, approvals, labels, etc.) is read-only.

Existing precedent: `JIRA_DISABLE_TRANSITIONS` already exists and is
honoured by `JiraSync`.

`blockedJiraProjects = {FSM}` stays as a defense-in-depth guard
regardless — even with writes disabled globally, FSM should be
explicitly excluded so a future regression cannot reach customer-facing
projects.

GitLab review-comment ingestion → bead-comment continues unchanged
(that's an inbound path; the outbound concern is only the MR description
edit).

### Phase 2 — Continuous enrichment (Jira)

Extend `JiraPoller.poll` so the second pass updates already-tracked
beads, not just creates new ones.

1. Add `jira_updated_at` field to beads. On each poll cycle, search
   `<filter> AND updated >= -2m` (overlap with poll interval) so a
   transient bridge restart doesn't lose updates.
2. For each updated issue, diff against the bead's current `Fields` and
   description and write only the changed keys. Skip no-op updates
   (per `ka-0000005177`).
3. Capture additional fields the poller doesn't track today:
   - `jira_components` (CSV of `Fields.Components[].Name`)
   - `jira_fix_versions` (CSV)
   - `jira_due_date`
   - `jira_resolution`
   - `jira_story_points` (custom field; configurable id)
   - `jira_watchers_count`
   - `jira_comment_count` — useful drift signal even before each comment
     is ingested
4. Re-pull description, summary, status, priority, labels, assignee on
   every poll. Description goes into the bead's `description` (not a
   field). Use `MIN(updated, last_synced)` dedup so we don't write the
   same description twice.
5. Closed beads continue to be enriched (so post-merge discussion is
   captured). The bridge already persists closed-bead state via
   `JiraSync.CatchUp`.

### Phase 3 — GitLab enrichment poller (new)

Mirror the Jira pattern. New file: `internal/bridge/gitlab_poller.go`.

- Configured by a list of GitLab project paths and a label selector
  (e.g. `MR has label gardenio OR description contains kd-`).
- Runs every 60s; uses `GET /projects/:path/merge_requests?updated_after=`
  with a 2-minute overlap.
- On first sight: creates a task bead with `source:gitlab`,
  `gitlab-mr:<projectpath>!<iid>` label, `mr_url`, and a snapshot of:
  title, description, author, source_branch, target_branch, draft
  state, labels, milestone, web_url, sha, created_at.
- On subsequent updates: diffs and writes changed fields. The existing
  webhook path stays primary — the poller is a slow reconciler that
  catches drift when webhooks are missed.
- Reuses `GetMergeRequestByPath` and adds `ListProjectMRs(updatedAfter)`.

This also fixes the asymmetry where GitLab MRs only become beads via
agent action.

### Phase 4 — Safe one-way comment sync

This is the trickiest part. The threat model:

- **Webhook re-delivery** — Atlassian and GitLab both retry. Same
  comment can hit `/webhook` 2-5 times across hours.
- **SSE replay on restart** — bead `comments.added` events replay if the
  bridge state volume is ephemeral.
- **Backfill at first sight** — when a brand-new bead is created from a
  ticket that already has 30 comments, we must capture history without
  spamming the bead activity feed.
- **Echo loops via outbound** — once the outbound writes are off, this
  class of loop is structurally impossible. Until then, we must not
  ingest a comment that was authored by the bridge bot itself.
- **Edits / deletions** — Jira comments are mutable; GitLab notes can
  be edited and deleted. Bead comments are append-only.
- **Internal/private comments** — Jira's `properties.internal=true` and
  GitLab's `confidential` notes must be respected. FSM (customer-facing)
  is already blocked entirely.

#### Design

Add a `bead_comment_provenance` table mediated by the daemon (or, as a
no-DB-change alternative, embed the IDs in the comment text and grep
on read). Recommended path: extend `beadsapi.AddComment` with optional
`source` metadata; if that's a step too far, fall back to a
**provenance prefix** in the comment body so dedup can be done by
querying `GetComments` and substring-matching:

```
[jira:PE-1234#comment-987654 by Matt Baker (matt@…) at 2026-05-03T15:22Z]
<original comment text>
```

Algorithm per inbound comment event:

1. **Filter** — drop if any of:
   - Author is the bridge bot account (`JIRA_BOT_ACCOUNT_ID` or
     `GITLAB_BOT_USER_ID`).
   - Comment is internal/confidential and project is not in
     `JIRA_INGEST_INTERNAL_ALLOWLIST`.
   - Project is in `blockedJiraProjects`.
2. **Resolve target bead** — by `jira_key` field or `jira:<KEY>` label
   (Jira), or by `mr_url` field (GitLab).
3. **Dedup** — fetch the bead's existing comments (cached in-process
   for ~5 min) and look for the provenance prefix matching the upstream
   ID. Skip if found.
4. **Backfill on first sight** — if this is the first comment we've
   seen for this bead AND the upstream comment count > 1, fetch the
   last N (50 default, configurable) comments and replay them in
   chronological order before adding the trigger event. Cap total
   ingested comments per bead per minute (rate-limit, e.g. 30/min) to
   protect the activity feed.
5. **Edits** — model as new comment "[edited at T] …" with the same
   provenance prefix but a different upstream-event timestamp. Don't
   try to mutate prior comments.
6. **Deletions** — model as a tombstone comment "[jira:PE-1234#987654
   deleted at T by …]". Don't actually remove the prior comment.
7. **State** — write `jira_last_comment_id` / `gitlab_last_note_id` and
   `jira_last_comment_at` fields on the bead so the poller catch-up can
   replay-from-there if the webhook went silent.

#### Why this is safe even if outbound writes are still on (transition window)

- The bridge bot's own outbound comments are filtered at step 1, so
  they cannot loop back in.
- Dedup is keyed on **upstream comment ID**, which is preserved even
  across SSE replay or webhook retry.
- Backfill is bounded (last N + rate limit), so a 5000-comment legacy
  ticket cannot DoS the bead's activity feed.
- The bead activity feed only ever sees `AddComment` (no
  `UpdateComment`/`DeleteComment`), preserving the append-only invariant.

### Phase 5 — Reconciler (slow polling drift fix for both)

A 5-minute periodic job that:

- Lists all `source:jira` and `source:gitlab` beads (paginated).
- For each, fetches upstream and compares `jira_last_comment_id` /
  `gitlab_last_note_id` and `jira_updated_at`. Re-pulls comments from
  there if drift is detected.

This catches the case where webhook delivery is broken for hours and
no one noticed. Cheap to run because the diff is just on
already-stored monotonic IDs.

## Operational checklist

- [ ] Add `JIRA_DISABLE_WRITES` (default `true`) — gate all `JiraSync`
      outbound calls
- [ ] Add `GITLAB_MR_DESCRIPTION_SYNC` (default `false`) — gate the
      one remaining GitLab outbound path (`syncMRDescription`)
- [ ] Extend `JiraPoller.poll` to update tracked beads, not just create
- [ ] Add new Jira fields (components, fix_versions, due_date, etc.)
- [ ] Implement GitLab MR poller (new file)
- [ ] Add `jira_last_comment_id` / `gitlab_last_note_id` bead fields
- [ ] Implement comment-ingest with provenance prefix + dedup
- [ ] Backfill-on-first-sight bounded by N + rate-limit
- [ ] Tombstones for edits and deletions
- [ ] 5-min reconciler for drift recovery
- [ ] Update `gardenio/CLAUDE.md` and add an advice bead

## Out of scope (intentionally)

- Two-way sync, conflict resolution, CRDT-anything.
- Bead → Jira comment posting (we are removing it).
- Bead → MR description sync at module-default (it stays available as
  an opt-in flag per Matt's clarification).
- Real-time Jira changelog ingestion (status transitions are captured
  via the field re-poll, which is enough for "see current state").

## Open questions

1. **`JIRA_BOT_ACCOUNT_ID` is optional today.** If the operator hasn't
   set it, the loop-prevention filter (step 1) cannot work. We should
   require it once outbound is removed (no bot account → no loop risk),
   but during the transition we should fail-closed and warn loudly.
2. **GitLab bot user ID** — we don't currently track it. Need a new
   `GITLAB_BOT_USER_ID` env to mirror the Jira side.
3. **Custom-field ids for story points / sprint** vary per Jira tenant.
   Suggest a `JIRA_FIELD_MAP` env JSON: `{"story_points":"customfield_10024"}`.
4. **GitLab's `confidential` is per-issue, not per-MR.** MR notes have
   no confidential bit but project-level visibility may be private.
   Decision: trust project-level access; an attacker who can read the
   project can read the bead.
5. **Backfill cost on first cutover.** When we enable comment ingest
   for the first time on N existing beads with M comments each, that's
   N*M `AddComment` calls. Recommend a one-shot "seed" script with
   explicit progress reporting, distinct from the live webhook path.
