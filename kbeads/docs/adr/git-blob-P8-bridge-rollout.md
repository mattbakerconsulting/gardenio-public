# ADR: `git-blob` Bead Type — Bridge Integration and Rollout

**Status**: Proposed (research only — implementation gated on operator approval of parent epic ka-0000003043)
**Date**: 2026-05-03
**Author**: gardenio-p8-gitblob-adr-2kf
**Phase**: P8 of research epic ka-0000003043 (git-blob bead type)
**Bead**: ka-0000002408
**Supersedes**: nothing
**Depends on**:
- Plan-of-record `kbeads/docs/research-git-blob-bead-type.md` (§ 7.2 — bridge integration; § 11 — phased work breakdown)
- P0 ADR `kbeads/docs/adr-git-blob-naming-schema.md` (ka-0000006442) — locks `kind: data`, the field set (including `origin_bead`, `source`, `gitlab_*` fields), and the `project_scoped: true, escalatable: true` write policy
- P1 ADR `kbeads/docs/adr/git-blob-P1-identity-dedup.md` (ka-0000002044) — locks the `(repo, commit_sha, path)` natural key, the partial unique index, and the upsert-on-conflict semantics that auto-registration depends on for idempotence
- P2 ADR `kbeads/docs/adr/git-blob-P2-storage-tiering.md` (ka-0000004323) — locks the three-tier storage routing and per-fleet rate limits in the create handler
- P3 ADR `kbeads/docs/adr/git-blob-P3-security-redaction.md` (ka-0000007332) — locks the gitleaks-on-create scan, redact-not-reject default, and `--no-redact` admin override that bridge writes inherit unchanged
- P4 spec `kbeads/docs/...` (ka-0000004396 — ingestion CLI / API) — locks the write surface the bridge calls (`POST /v1/git-blob:add` and equivalents)
- P5 ADR (ka-0000002819) — locks `origin_bead` semantics and the typed-mention edge surface the bridge uses to link blobs to issue beads
- P6 ADR `kbeads/docs/adr/git-blob-P6-embedding-search.md` (ka-0000005801) — locks the per-repo embedding opt-in flag (D-8) that bridge writes inherit
- P7 ADR `kbeads/docs/adr/git-blob-P7-lifecycle-gc.md` (ka-0000005504) — locks the orphan GC sweeper, immutability rules, and repo-removed `403` behavior that bridge writes obey
**Resolves**: no plan-of-record decision IDs (D-1 through D-8 are owned by P0–P6). One new internal decision is recorded here: **R-1** — bridge auto-registration is **off-by-default per fleet AND opt-in per repo**, behind a feature flag with a graduated dev → rwx → prod rollout.
**Defers**: nothing — P8 is the last research phase before P9 (implementation epic).

## TL;DR

This ADR fixes the **bridge integration and rollout contract** for `git-blob` so that the eventual P9 implementation has a single specification for the optional gitlab-bridge auto-registration path, the per-environment rollout sequence, the historical-backfill story, and the feature-flag matrix. Decisions:

- **Trigger**: the existing `gitlab-bridge` webhook handler is extended in-place. On a `merge_request` event with `action=merge` (the same branch that today flips `mr_merged=true` on the issue bead, see `gardenio/controller/internal/bridge/gitlab_sync.go:582`), the bridge fans out to fetch the MR's changed-file list and registers a `git-blob` per file. The pre-existing **5-minute polling fallback** (`GitLabPoller`, `gitlab_sync.go:313`) inherits the same code path so a missed webhook still produces blobs eventually.
- **Per-file write**: each registration is a single `kd git-blob add` (or its HTTP/gRPC equivalent locked by P4). Writes go through the standard create handler, which means: gitleaks scan (P3), `project_scoped` policy check against the fleet's allowed-projects (P5), per-fleet rate limit (P2 § 4 / P3 § 8), three-tier storage routing (P2), and the partial unique index on `(repo, commit_sha, path)` (P1) which makes the call idempotent. The bridge never bypasses scanner or rate-limit; it gets a small **bridge-specific burst budget** (§ 6.2) layered on top.
- **Link strategy**: each newly-registered `git-blob` carries `origin_bead = <issue-bead-id>` and `source = "gitlab-bridge"` (P0 fields). The bridge does NOT write a typed-mention edge in the issue bead's description — the `origin_bead` field is sufficient for graph reachability and for orphan-GC's L-1 predicate (P7 § 3.1, condition O-4). A single follow-up comment on the issue bead enumerates the new blob IDs.
- **Feature flag** (decision **R-1**): a three-layer kill matrix. (1) **Fleet-level** env knob `GARDENIO_GITLAB_BRIDGE_BLOBS=off` (default off) is the global on/off. (2) **Repo-level** opt-in via a new `git_blob_register: true` field on the project bead — defaults to off, must be set per project. (3) **MR-level** opt-out via an MR description marker `[no-blobs]` for sensitive merges. All three are AND-gated: a registration happens only if fleet=on AND repo=on AND MR-not-marked. This matches the per-repo embedding opt-in shape from P6 (D-8) and makes "register everything" a deliberate, repo-by-repo operator action.
- **Rollout sequence**: dev → rwx → prod, in that order, with a quench gate between each. Dev (`gardenio-next` or whatever the agent develops against) flips fleet=on first and one low-volume repo opts in. After 7 days of clean operation, rwx (`gardenio-rwx`) flips fleet=on and 1–2 repos opt in. After another 7 days, prod (`gardenio`) flips fleet=on and the operator opts repos in deliberately. No automatic prod opt-in.
- **Backfill**: **no automatic historical sweep**. Existing merged MRs are not back-registered when the flag flips on. An admin-only tool `kd admin git-blob backfill --repo <R> --since <date>` (specified here, implemented in P9) gives the operator an explicit, rate-bounded path for backfilling a specific repo over a specific window, with a per-run cap and a dry-run mode. The tool runs as `actor=system:git-blob-backfill` so its events distinguish it from live bridge writes. Backfill respects the same gitleaks scan, project-scope check, and rate limit as live writes.
- **Migration sequence**: zero schema migrations. P8 ships entirely on top of P0–P7's tables. The only "migration" is rolling the feature flags forward in the deployment configs, in the order specified here. The plan-of-record's reference to "migration sequence" in § 11 means the *deployment* migration (env flag flips, project-bead field flips) — not a database migration.

This ADR ships **no code**. It is the contract the eventual P9 implementation will register at the gitlab-bridge webhook handler, the `kd admin git-blob backfill` command, and the per-fleet config schema. There is one operator decision: **R-1** (the three-layer flag matrix and the dev→rwx→prod sequence) which is surfaced as a single `gb decision` so the operator either accepts the recommended defaults (off-by-default per fleet, off-by-default per repo, dev-rwx-prod with 7-day soak gates) or directs a specific override.

## 1. Context

### 1.1 What the plan-of-record asks

`kbeads/docs/research-git-blob-bead-type.md` § 7.2 frames bridge integration in two sentences:

> The gitlab-bridge already touches MR state on issue beads. Optional, deferred follow-up: when an MR merges, the bridge could batch-register `git-blob` beads for the changed files at the merge commit, link them under the issue. **Out of scope for the initial epic** — it's infrastructure work that depends on the rest of the design landing first.

§ 11 then schedules P8 explicitly:

> **P8 — Bridge & rollout** | ADR on optional gitlab-bridge auto-registration on merge, migration sequence, backfill story, feature flag plan. Depends on P0–P7.

P8's job is to (a) turn that two-sentence § 7.2 sketch into an integration spec the P9 implementer can execute against, (b) lock the rollout sequence so it does not accidentally light up in prod, (c) decide how (or whether) to backfill historical MRs, and (d) decide the feature-flag granularity.

### 1.2 What P0–P7 already locked that bridge writes must obey

The bridge is not exempt from any of the prior contracts. Specifically:

| Contract | Source | What bridge writes inherit |
|---|---|---|
| `kind: data`, identity field set, `project_scoped: true, escalatable: true` write policy. | P0 | Bridge writes with kind `data`, must satisfy project-scope, can escalate via the existing escalation path. |
| `(repo, commit_sha, path)` natural key, partial unique index, upsert on conflict, identity-field immutability. | P1 § 3.1, § 3.5 | Two webhook deliveries for the same merge produce one bead, not two. A late poll-fallback registration of an already-bridged file is a no-op. |
| Three-tier storage routing (Tier A inline / Tier B `git_blob_content` / Tier C S3) chosen by `file_size` and `encoding`. | P2 § 3 | Bridge writes pick their tier the same way CLI writes do; no special bridge tier. |
| Gitleaks scan on every create, redact-not-reject default, `--no-redact` admin override. | P3 § 2, § 5 | The bridge has **no `--no-redact` channel**. It runs as a regular agent, not as an admin. Redaction is opaque to the bridge. |
| Per-fleet rate limit on `git-blob` creates. | P3 § 8 | The bridge gets its own per-(fleet, repo) burst window (§ 6.2 below) layered on the per-fleet limit. |
| `origin_bead` field, typed-mention `produces` (deferred to typed-mention research per P5 / D-7). | P0 § 7.2, P5 § 3 | Bridge sets `origin_bead = <issue-bead-id>`. It does NOT write a typed-mention `produces(ka-X)` edge today (deferred); it can be added later without re-rolling the bridge by running a one-shot recompute, see § 5.4. |
| Per-repo embedding opt-in (D-8). | P6 § 3 | Bridge does not touch embedding; the embedding pipeline picks up new blobs via the existing post-create hook. The per-repo opt-in flag is independent of the bridge opt-in. |
| Orphan GC at 90 days, refcount on hard-delete, repo-removed-403 on writes. | P7 § 3, § 4, § 5 | A bridge-registered blob with no further mentions ages out via orphan GC at 90+30 days. A bridge write to a repo that has been removed from the allowed-projects list returns 403 — the bridge does not get a bypass (P7 § 5.4 makes this explicit). |

Net effect: the bridge is "the same write path everyone else uses, fired by a webhook instead of a human." P8's job is to fix the **trigger surface**, the **rate-limit overlay**, the **link semantics**, and the **rollout/backfill plan**. Everything else falls out of P0–P7.

### 1.3 What the gitlab-bridge already does today

The gitlab-bridge (see `gardenio/CLAUDE.md` and `gardenio/controller/internal/bridge/`) terminates webhooks at `/webhook` (`gitlab_sync.go:135`) and runs a 5-minute polling fallback (`GitLabPoller`, `gitlab_sync.go:313`). On `merge_request` `action=merge` (`gitlab_sync.go:582-637`), it:

1. Lists task beads via `daemon.ListTaskBeads(ctx)`.
2. Finds beads where `bead.Fields["mr_url"]` matches the event's MR URL.
3. Calls `daemon.UpdateBeadFields(ctx, bead.ID, fields)` with `mr_merged=true`, `mr_state=merged`, `gitlab_mr_iid=<iid>`, `gitlab_project_id=<id>`.
4. Returns 200 OK.

That handler is the integration point. P8 specifies a fan-out from step 4: after the fields update succeeds, an async worker enqueues a per-MR job that fetches the merge commit's changed file list and registers blobs.

The polling fallback (`GitLabPoller.poll`, `gitlab_sync.go:365-432`) calls the same downstream code paths via `ListMergedMRs` (group-scoped, `state=merged`, `updated_after=<lastPoll>`). P8's contract is that **both** the webhook handler and the poller route the merge into the same blob-registration job, so a missed webhook does not produce a missing-blob hole.

### 1.4 Why the trigger has to be MR-merge, not MR-open or push

Three options for "when to register":

| Trigger | Pros | Cons | Verdict |
|---|---|---|---|
| **Push to default branch** | Catches commits that bypass MR (rebase-merge, fast-forward, force-push). Simplest mental model: "what's on `main`". | Default-branch protection varies per repo; some teams force-push. Bridge would re-register blobs on every force-push. Mass-rebases produce floods. | **Rejected.** |
| **MR open** | Earliest signal; catches WIP files. | Most files churn before merge; would register stale snapshots that GC immediately ages out. Wasted work. | **Rejected.** |
| **MR merge** | Stable snapshot (the merge commit doesn't move). One write per merged file per repo. Aligns with the existing handler. | Misses non-MR commits. | **Accepted.** |

P8 picks MR merge. The "non-MR commit" gap is acknowledged in § 9.5 — the operator can still register those files manually with `kd git-blob add`, and the proportion of non-MR commits in the gardenio fleet's repos is near zero.

## 2. Decision: trigger surface — webhook handler + poller, both routing to one job

### 2.1 The integration point

Inside the existing `merge_request` `action=merge` branch in `gitlab_sync.go:582-637`, after the issue-bead `mr_merged=true` update succeeds, the bridge enqueues a job:

```
on merge_request webhook with action=merge:
  // 1. existing path: update issue bead fields (unchanged).
  for each task bead with mr_url = event.url:
    UpdateBeadFields(bead.ID, {mr_merged:"true", mr_state:"merged", ...})
    issueBeadID := bead.ID  // capture for the blob job

  // 2. new path: enqueue blob registration if feature-flag-on.
  if FleetFlag.EnableGitBlobBridge && RepoFlag(event.project.path).EnableGitBlobBridge:
    if not MRDescriptionContains(event, "[no-blobs]"):
      EnqueueBlobRegistrationJob(BlobJob{
        ProjectPath: event.project.path,
        MRIID:       event.object_attributes.iid,
        MergeCommit: event.object_attributes.merge_commit_sha,
        IssueBeadID: issueBeadID,
        FleetID:     ctx.fleet,
        EnqueuedAt:  NOW(),
      })
```

The poller fallback (`GitLabPoller.poll`) does the same thing for any merged MR it discovers. To prevent double-registration when both webhook and poll land, the job uses a **per-(project, MR-IID, merge-commit-SHA) idempotency key**: the worker takes a Postgres advisory lock on `hash(project|iid|sha)`, registers, and releases. A duplicate fires the lock, sees the per-file natural-key conflict on every blob, and exits as a no-op (P1's idempotence).

### 2.2 Why an enqueued job, not synchronous registration

A merge can touch hundreds of files. Synchronously registering all of them inside the webhook handler would:

- Stretch the webhook beyond GitLab's 10-second response budget. A timeout retries the webhook, multiplying writes.
- Hold the HTTP handler open while gitleaks scans (P3) run on each file. Per-file scan is microseconds for 64KB files, milliseconds for 1MB files; aggregate over 100 files is seconds.
- Couple webhook latency to kbeads create-handler latency. Bad isolation.

The asynchronous worker path lets the webhook return 200 immediately (preserving the existing `mr_merged=true` semantics), and lets blob registration run on its own scheduling budget. The job runs inside the bridge process; it is not a separate service.

### 2.3 Job durability

The worker is in-process. If the bridge restarts mid-job, the partial-progress is lost. Recovery is the poller fallback: on the next 5-minute tick, the poller re-discovers the merged MR (`updated_after=<lastPoll>`), re-enqueues, and the natural-key idempotence picks up where the previous run left off. Files already registered are no-ops; remaining files register.

We do not introduce a new persistent job table for P8. If poller-driven recovery proves insufficient under load, P9 can add a `pending_blob_jobs` table behind the same flag — but the default plan ships zero new tables.

### 2.4 What about non-merge MR transitions

The webhook handler already ignores `merge_request` `action != merge` (and routes `approved`/`unapproved` to `handleApprovalWebhook`, `gitlab_sync.go:587`). P8 does not change that. A closed-without-merge MR does not register blobs. A reopened MR does not register blobs. Only the `merge` transition fires the job.

## 3. Decision: per-merge file enumeration and content fetch

### 3.1 The per-merge worker pseudocode

```text
worker BlobRegistrationJob(j BlobJob):
  // Acquire idempotency lock; if another worker holds it, abort.
  lockKey := hash64("blob-job:" + j.ProjectPath + ":" + j.MRIID + ":" + j.MergeCommit)
  if not pg_try_advisory_lock(lockKey):
    return  // another worker is on this; let it run

  // 1. List changed files at the merge commit.
  files, err := gitlab.GetMRChangesByPath(j.ProjectPath, j.MRIID)
    // wraps GET /projects/:id/merge_requests/:iid/changes
  if err != nil:
    log.Warn("blob job: failed to list changes", "err", err); return

  // 2. For each non-deleted file, fetch contents at merge commit.
  for each f in files:
    if f.deleted: continue                   // file is gone; nothing to register
    if f.size > BlobMaxBytes: skip+log       // P2 § 4 / P3 § 8 size cap
    if SkipPath(f.new_path):                  // global skip-list (§ 3.4)
      continue
    contents, err := gitlab.GetFileContents(j.ProjectPath, f.new_path, j.MergeCommit)
    if err != nil: log.Warn(...); continue

    // 3. Register via the standard create RPC.
    req := GitBlobAddRequest{
      Repo:        j.ProjectPath,
      CommitSHA:   j.MergeCommit,
      Path:        f.new_path,
      Content:     contents,                  // server scans + redacts (P3) + tiers (P2)
      Encoding:    detectEncoding(contents),
      Source:      "gitlab-bridge",
      OriginBead:  j.IssueBeadID,
      GitlabMRIID: j.MRIID,
      // commit_message, commit_author, committed_at, branch fetched separately (§ 3.3)
    }
    _, err := beads.GitBlobAdd(ctx, req)
    if err == ErrConflict (natural-key dup):
      continue                                 // already registered — idempotent no-op
    if err == ErrRateLimited:
      backoff_and_retry_one_file_at_a_time()
    if err == ErrProjectScopeDenied:
      // repo is not in fleet allowed-projects; abort whole job — log once.
      log.Warn("blob job: project not allowed; bridge flag must be reconciled", ...)
      return

  // 4. Post a single summary comment on the issue bead (§ 5.3).
  if anyRegistered:
    beads.AddComment(j.IssueBeadID, "Auto-registered N git-blob beads from merge commit ...")

  pg_advisory_unlock(lockKey)
```

### 3.2 Why use `/merge_requests/:iid/changes`, not the diff/commits API

The MR-changes endpoint returns the **net changed-file list relative to the target branch** at the merge moment, with `new_path`, `old_path`, `deleted_file`, `new_file`, and per-file diff stats. This matches the conceptual "what did this merge add or modify" — which is what we want to register. The commits-on-MR API would give us per-commit diffs (over-counts when files churn within an MR), and the diff endpoint requires manually summing across commits. The changes endpoint already does the right summarization.

The existing `GitLabClient` does not expose this endpoint today (`gardenio/controller/internal/bridge/gitlab.go` has `GetMergeRequest`, `ListMergedMRs`, etc., but not `GetMRChanges`). P9 adds the wrapper. It is a single GET; no schema or auth changes.

### 3.3 Provenance fields the worker also fetches

P0 § 7.2 includes provenance fields the bridge should populate when registering. Per file, the worker also captures (one shared API call per merge, not per file):

- `commit_message`, `commit_author`, `committed_at` — from `GET /projects/:id/repository/commits/:sha` for the merge commit. One call per job.
- `branch` — from `event.object_attributes.target_branch`.
- `parent_commit_sha` — from the same commit fetch (`parent_ids[0]`).
- `gitlab_project_id` — from the webhook event.

Per-file fetches that *do* happen per file: contents (`/repository/files/:path/raw?ref=:sha`) and `blob_sha` (returned by the contents API).

The `prior_blob_id` field (P0 § 7.2; chain-of-revisions) is **not** populated by the bridge in P8. Looking up the previous version requires an additional API call per file and a kbeads lookup per (repo, prior-commit, path). P9 may add it as a follow-up; P8 keeps the bridge simple. Orphan GC's L-1 predicate (P7 § 3.1) treats a missing `prior_blob_id` correctly — it just means no chain edge contributes.

### 3.4 The path skip-list

Some paths should never be auto-registered, regardless of fleet/repo opt-in:

| Path pattern | Reason |
|---|---|
| `**/*.lock`, `**/package-lock.json`, `**/go.sum`, `**/Cargo.lock`, `**/yarn.lock`, `**/pnpm-lock.yaml` | Lockfiles churn enormously and have low information value as snapshots. The lockfile *delta* is what reviewers care about, not the snapshot. |
| `**/node_modules/**`, `**/vendor/**`, `**/.terraform/**` | Vendored dependencies. Not source. |
| `**/*.min.js`, `**/*.min.css`, `**/dist/**`, `**/build/**` | Build artifacts. Reproducible from source; storing them is duplicative. |
| Files that gitleaks-config-allowlists for paths (e.g., the `[allowlist].paths` in `.gitleaks.toml`) | Already trusted to contain placeholder secrets; registering them risks loud false positives. |

The skip-list is a **fleet-level configuration**, not a per-repo override, because it expresses universal "this is not source code worth snapshotting" judgments. P8 specifies the default list; P9 wires the YAML/env knob (`GARDENIO_GITLAB_BRIDGE_BLOB_SKIP_PATHS`, comma-separated globs).

A more aggressive operator can shorten the list; a more conservative one can extend it. The skip is silent (no error, no comment on the issue bead).

### 3.5 What if the MR itself is huge (10k+ files)

A monorepo-wide refactor MR is the worst case. Two protections:

1. **Per-job file cap**: `GARDENIO_GITLAB_BRIDGE_BLOB_MAX_FILES_PER_MR` (default 500). If exceeded, the worker registers the first 500 (lexicographic by `new_path` for determinism), posts a comment listing the truncation count and a manual-backfill suggestion (`kd admin git-blob backfill --repo <R> --merge <sha>`), and exits.
2. **Per-fleet rate limit**: P3 § 8 already throttles total `git-blob` creates per fleet. The worker respects 429s with exponential backoff (one-file-at-a-time mode) instead of dropping files silently.

The 500 cap is generous (a typical merge touches < 50 files) and protects against runaway. The operator can override per-repo via a project-bead field (`git_blob_register_max_files_per_mr`), within global ceiling.

## 4. Decision: feature-flag matrix (R-1)

### 4.1 The three layers

```
register?(fleet, repo, mr) =
       FleetFlag.EnableGitBlobBridge
   AND RepoFlag(repo).EnableGitBlobBridge
   AND NOT MRDescriptionContains(mr, "[no-blobs]")
```

| Layer | Where stored | Default | Operator action to flip |
|---|---|---|---|
| **Fleet** | Env var `GARDENIO_GITLAB_BRIDGE_BLOBS` on the gitlab-bridge deployment (`off`/`on`). | `off` | Helm chart value bump + redeploy. |
| **Repo** | New field `git_blob_register: true|false` on the project bead (one project bead per allowed repo). | `false` | `kd update <project-bead-id> -f git_blob_register=true`. |
| **MR** | Marker `[no-blobs]` anywhere in the MR description (case-insensitive, matched as a literal substring). | not present | MR author edits the description before merging. |

All three are AND-gated. Any one of {fleet=off, repo=off, MR-marked} suppresses registration. There is no "force-on" override at the bridge layer; if an operator wants a one-off registration without flipping flags, they run `kd git-blob add` directly.

### 4.2 Why three layers, not one

A single fleet-level switch is too coarse: turning it on makes every allowed-projects repo start filling the store, and a sensitive repo (e.g. a credentials-template repo, even with gitleaks) cannot be excluded short of removing it from allowed-projects entirely.

A single repo-level switch is too coarse: turning it on for a repo where most files are fine still floods the store on a one-off mass-rename MR.

A single MR-level marker is too noisy: every merge author would have to opt in. Default-on at the merge level is unworkable.

The three layers map cleanly to three different operator concerns:

- **Fleet** answers "is this feature live for this deployment at all?" (operations / deployment decision).
- **Repo** answers "is this repo's churn worth snapshotting?" (per-team product decision).
- **MR** answers "is THIS specific merge a special case?" (per-merge author decision).

Each level is the right level for the question it answers. Three flags is a small budget for the orthogonality.

### 4.3 The `[no-blobs]` MR marker

A literal substring search is intentionally simple. Alternatives considered:

| Marker form | Why rejected |
|---|---|
| GitLab MR labels | Requires API access to read labels; works but adds a fetch per merge. The description is already in the webhook payload. |
| Title prefix (`[NO-BLOBS] ...`) | Clutters MR titles. Description is invisible after merge to most readers. |
| Trailer in commit message | Requires fetching the merge commit message before deciding. The webhook already carries the description. |

The webhook payload includes `event.object_attributes.description`. A `strings.ContainsAny(description, "[no-blobs]")` lookup is O(n) on the description size and runs once. It is good enough.

If the marker proves too easy to forget, P9 can add a stricter requirement (e.g. a regex or a YAML frontmatter); P8 ships the simple version.

### 4.4 The repo flag's ergonomics

Project beads already exist (one per allowed repo). Adding a single `git_blob_register: bool` field to the project type's `fields` map is a one-line type-config change. No new table, no new bead type.

The operator inspects the current opt-in set with:

```
kd list --type=project -f git_blob_register=true
```

And opts a repo in with:

```
kd update kd-<projectBeadId> -f git_blob_register=true
```

There is no "all repos at once" flip. If the operator wants every repo on, they iterate. The friction is intentional — opt-in must be deliberate per repo.

## 5. Decision: link semantics — `origin_bead` only, summary comment, no typed-mention edge

### 5.1 What the bridge writes onto each new blob

Per-blob `fields` map written by the bridge:

```text
{
  "repo":              "<event.project.path>",
  "commit_sha":        "<event.merge_commit_sha>",
  "path":              "<f.new_path>",
  "blob_sha":          "<from contents API>",
  "file_size":         "<bytes>",
  "encoding":          "<utf8|binary|omitted>",     // detected
  "content":           "...",                       // tier A inline; or absent if tier B/C
  "content_ref":       "...",                       // tier C; absent otherwise
  "source":            "gitlab-bridge",
  "origin_bead":       "<issue-bead-id>",            // populated when matched; empty otherwise
  "gitlab_mr_iid":     "<mr-iid>",
  "gitlab_project_id": "<project-id>",
  "branch":            "<target-branch>",
  "commit_message":    "...",
  "commit_author":     "...",
  "committed_at":      "<ISO-8601>",
  "parent_commit_sha": "...",
}
```

### 5.2 Why `origin_bead` is the only inbound graph edge the bridge writes

P5 (ka-0000002819) makes `origin_bead` a typed-mention-equivalent edge for graph reachability: a bead with `fields ->> 'origin_bead' = blob.id` keeps the blob reachable for orphan-GC purposes (P7 § 3.1, condition O-4). That is sufficient.

A typed-mention edge (e.g. `produces(ka-blob-X)`) in the issue bead's *description* would be redundant, would mutate the issue bead's description on every merge (creating noisy edits), and is **deferred per D-7 to typed-mention-research-recompute** (plan-of-record § 10). When that recompute lands, it can scan blobs with `source = "gitlab-bridge"` and synthesize the typed edges; no bridge change needed.

Counter-argument considered: *"Just add the edge now via `kd ref add`."* Rejected. (a) `kd ref add` writes a row to `bead_references`, not to a typed-mention text edge; the typed-mention research is exactly about where these edges should live. (b) Pre-typing now means re-doing it later. Better to write `origin_bead` + `source` and let the recompute pass do the canonical synthesis.

### 5.3 The issue-bead summary comment

After per-file registration, the worker posts ONE comment on the issue bead:

```
Auto-registered N git-blob beads from merge commit <sha>:
  - kd-<blobId1> (path/to/fileA)
  - kd-<blobId2> (path/to/fileB)
  ... (truncated at 25 entries; see kd list -t git-blob -f gitlab_mr_iid=<iid> for the full set)
```

One comment per merge, not one comment per file. The comment author is `system:gitlab-bridge` (the existing bridge actor). The comment is `kind=comment`, no special handling.

If zero blobs were registered (everything was skipped or all failed), no comment is posted — the issue bead is unchanged.

### 5.4 Forward compatibility with typed mentions

When typed-mention research lands (ka-0000001333) and the recompute pass runs, the pass can:

```
for each blob B with source = 'gitlab-bridge' AND origin_bead != '':
  source_bead := beads.Get(B.fields.origin_bead)
  IF source_bead.description does not already contain produces(B.id):
    -- synthesize a typed-mention edge in bead_references
    INSERT INTO bead_references (source_bead_id, target_bead_id, source_field, relationship_type)
      VALUES (source_bead.id, B.id, 'origin_bead_synthesized', 'produces')
```

That is a one-pass, idempotent, post-hoc enrichment. P8 makes no assumptions about its timing — `origin_bead` alone is enough for orphan-GC and beads-viewer rendering today, and the recompute is a strict addition.

## 6. Decision: rate limiting and backpressure overlay

### 6.1 What P3 already gives us

P3 § 8 specifies a per-fleet rate limit on `git-blob` creates (the exact knob name is P9's wiring, but it exists). That limit is the floor: any `git-blob` write — CLI, agent, or bridge — counts against it.

### 6.2 Bridge-specific overlay

On top of the per-fleet limit, the bridge gets a **per-(fleet, repo) burst budget**:

```
GARDENIO_GITLAB_BRIDGE_BLOB_BURST_PER_REPO_PER_HOUR  default 200
GARDENIO_GITLAB_BRIDGE_BLOB_BURST_PER_FLEET_PER_HOUR default 1000
```

If a repo blows past 200 in an hour (e.g. a 500-file refactor MR), the worker pauses per-file registrations for that repo until the next hour boundary, leaving partial progress. The poller will pick up the remainder on the next tick after the budget refills.

If the fleet blows past 1000 in an hour, all bridge workers pause for 5 minutes and retry. This is the protection against an MR-flood across many repos.

The bridge **never breaks the fleet limit**. The bridge overlay is strictly tighter than the fleet limit.

### 6.3 Why this matters operationally

Without an overlay, a single 800-file refactor MR exhausts the per-fleet quota and starves CLI / agent writes for an hour. With the overlay, the bridge self-throttles to 200/repo/hour and CLI writes still go through. The remaining files trickle in over subsequent hours without operator intervention.

### 6.4 Errors as backpressure signal

The worker treats any of `429`, `503`, `context deadline exceeded` from `beads.GitBlobAdd` as backpressure. It **does not retry the same file in a tight loop**. Backoff is exponential with a 30-minute ceiling; on persistent failure, the file is dropped from this run and the poller is the safety net.

## 7. Decision: rollout sequence — dev → rwx → prod, 7-day soak gates

### 7.1 The sequence

| Stage | Fleet | Repo opt-in (initial) | Soak before next stage | Quench gates |
|---|---|---|---|---|
| **0. Pre-rollout** | none | none | n/a | All P9 implementation merged. Migration `015` (P7 § 6) applied. Bridge image published. |
| **1. Dev** | `gardenio-next` (or whatever the development fleet is at rollout time): fleet flag = `on`. | One low-volume repo (TBD by the rollout operator). | **7 days.** | Bead count for the repo grows monotonically; no spikes. Orphan sweeper sees the new blobs but does not delete (within 90d window). gitleaks scan logs show zero unredacted writes. Per-fleet rate-limit budget < 50% used. |
| **2. RWX** | `gardenio-rwx`: fleet flag = `on`. | 1–2 medium-volume repos. | **7 days.** | Same gates as dev. Additionally: bridge worker latency p95 < 30s per merge; no webhook timeouts in 7 days. |
| **3. Prod** | `gardenio`: fleet flag = `on`. | Operator picks repos one at a time, with a 24-hour soak between each opt-in. | n/a | Per-repo opt-in is a deliberate operator action; no automatic batch flip. |

### 7.2 What the soak measures

Each soak is 7 days because:

- 7 days covers a full release cycle in this fleet (weekly cadence).
- 7 days is long enough for a blob-count cliff to surface (bridges that register too aggressively show up by day 2-3; a 7-day window catches them).
- 7 days is long enough for the orphan sweeper to run 7 times (it's daily, see P7 § 3.3) — confirming that orphan-GC interactions don't unexpectedly delete a bridge-registered blob mid-soak.

If a stage fails its quench gates, the operator flips the fleet flag back to `off`, files an incident bead, and the rollout pauses until the gate's underlying cause is fixed. P8 specifies the gates; it does not specify the recovery procedure (that's an operational runbook for P9 / the implementation epic).

### 7.3 Per-repo opt-in in prod is deliberate

In prod, no repo opts in by default. Each project bead's `git_blob_register` field stays `false` until the operator explicitly flips it. A failed opt-in is a single `kd update <projectBeadId> -f git_blob_register=false` rollback. There is no "set it everywhere" command. This friction is the right tradeoff against accidental fleet-wide opt-in.

### 7.4 Counter-arguments considered

- *"Why not flip prod and dev together — gardenio has no real production users yet."* No. The plan-of-record § 12 explicitly calls out volume blow-up as the top risk. The cheap defense is staging.
- *"7-day soak is too long; ship it in a day."* No. The orphan-sweeper interaction (90d) and the embedding cost interaction (P6 § 4) are not visible in 24 hours. 7 days is the floor for catching second-order effects.

## 8. Decision: backfill story

### 8.1 No automatic historical sweep

When the fleet+repo flags flip on, **only future merges are registered**. Existing merged MRs in the same repo are not back-registered. This is the conservative default.

Rationale:

- A repo with 5 years of MR history and 10k merged MRs touching 100k files would generate a 100k-blob registration burst on first opt-in. Even with the rate-limit overlay, that's a multi-week sweep at 200/hour — and most of those snapshots have low value (old code that no current bead references).
- Orphan GC at 90 days (P7) ages out anything that wasn't going to be referenced anyway. Backfilling and then GC-ing is wasted compute.
- The operator can always backfill a specific repo over a specific window deliberately.

### 8.2 Admin backfill tool

A new admin command (specified here, implemented in P9):

```
kd admin git-blob backfill \
   --repo <project-path> \
   --since <ISO-8601> \
   [--until <ISO-8601>] \
   [--max-mrs <N>] \
   [--max-files-per-mr <N>] \
   [--dry-run]
```

Behavior:

1. Lists merged MRs for `--repo` between `--since` and `--until` (default `--until` = NOW).
2. For each MR (oldest first), enqueues a backfill job equivalent to the live-bridge job, but with `actor=system:git-blob-backfill` so events distinguish.
3. Honors the per-fleet rate limit and the bridge burst overlay.
4. Caps total MRs at `--max-mrs` (default 100) so operators don't accidentally walk a 5-year history.
5. `--dry-run` lists the MRs and a per-MR file count without registering anything.
6. Refuses to run if `repo` is not in the fleet's allowed-projects (403 inherited from P7 § 5.4).
7. Refuses to run if `repo`'s project bead has `git_blob_register=false` (consistency with the bridge flag).

The backfill is a **one-shot tool**, not a scheduled job. The operator runs it deliberately, watches it, and either lets it complete or aborts it (Ctrl+C cancels the enqueue loop; in-flight workers complete their current MR before exiting).

### 8.3 Why admin-only

Backfill at scale interacts with rate limits, embedding-pipeline cost (P6), and gitleaks scan throughput (P3). An average agent should not initiate it. The `gardenio-admin` role gate matches the `--no-redact` precedent from P3 — ops-shaped operations that mutate the fleet's storage profile.

### 8.4 Backfill audit

Each backfill run emits one summary `audit`-kind bead at completion:

```text
type: audit
fields:
  actor:        gardenio-admin (operator)
  command:      "kd admin git-blob backfill --repo R --since S --until U"
  mrs_scanned:  N
  blobs_added:  M
  blobs_skipped: K
  duration_ms:  T
  fleet_id:     <fleet>
```

This is the same `audit`-kind precedent P3 § 4.3 establishes for `--no-redact` overrides — operator-initiated, irreversible-shaped operations get one audit bead each.

### 8.5 What backfill does NOT do

- It does not retroactively populate `prior_blob_id` chains. The bridge does not write that field today (§ 3.3); a backfill of a 2-year history won't produce a continuous chain. A separate one-shot recompute can populate `prior_blob_id` once the chain is desired (specified for P9 follow-up, not P8).
- It does not re-register already-registered blobs. The natural-key idempotence (P1) makes a re-run of `--since X --until Y` a no-op for blobs already in store.
- It does not delete or update existing blobs. It only adds.

## 9. Decision: migration sequence (deployment, not schema)

### 9.1 What "migration" means in P8's title

The plan-of-record § 11 title for P8 reads "ADR on optional gitlab-bridge auto-registration on merge, **migration sequence**, backfill story, feature flag plan." In context, "migration sequence" is the *deployment* migration — flag flips, project-bead field flips, image-tag bumps — not a database schema migration. P0–P7 own all schema migrations (the only new-in-P7 ones are `015_git_blob_content_refcount_repair` and the partial expression indexes from P7 § 3.3).

P8 itself adds **zero schema migrations**. The contract is purely runtime config + handler logic.

### 9.2 The deployment sequence

1. **Pre-flight**: P9 implementation merges. The bridge image is rebuilt to include the new webhook handler branch and the worker. Migration `015` (from P7) lands in the kbeads deployment.
2. **Quench-pre**: `make preflight` passes. E2E in `gardenio-rwx` confirms the bridge worker can register a blob from a synthetic merge.
3. **Dev rollout**: `GARDENIO_GITLAB_BRIDGE_BLOBS=on` in the dev Helm values. One repo's project bead gets `git_blob_register=true`.
4. **7-day soak**: monitor blob count, gitleaks-scan logs, sweeper interaction. Quench gate per § 7.1.
5. **RWX rollout**: same flag flips for `gardenio-rwx`. 1–2 repos opt in.
6. **7-day soak**.
7. **Prod rollout**: same flag flip for `gardenio`. Operator opts in repos one at a time over weeks.

### 9.3 Rollback

At any stage, rollback is:

```
# Stage rollback
helm upgrade ... --set gardenio.gitlabBridge.enableGitBlobBridge=false
```

This stops new bridge writes immediately. Existing bridge-registered blobs stay (orphan GC will age out the unreferenced ones at 90d as a side effect).

A repo-level rollback is:

```
kd update kd-<projectBeadId> -f git_blob_register=false
```

An MR-level emergency rollback after a merge already fired is not possible (the blobs are written). The operator soft-deletes the offending blobs explicitly.

### 9.4 The skip-list as a runtime knob

`GARDENIO_GITLAB_BRIDGE_BLOB_SKIP_PATHS` (§ 3.4) is environment-tunable. A deployment that wants tighter scope adds patterns; a deployment that wants looser scope removes them. Default values are baked into P9; the env var override is the operator's escape valve without a code change.

## 10. Open follow-ups (not blocking P8)

| # | Follow-up | Recommendation |
|---|---|---|
| F-1 | Persistent job queue (`pending_blob_jobs` table) for crash-recovery beyond what the 5-minute poller provides. | P9 implementer's call; ship the in-process worker first, add the table only if poller-driven recovery is insufficient under measured load. |
| F-2 | `prior_blob_id` chain population (chain-of-revisions). | One-shot recompute pass owned by the P9 implementer. P8 leaves the field unset. The orphan-GC predicate (P7 § 3.1, O-3) handles the unset case correctly. |
| F-3 | Webhook payload size limit / pagination. The MR-changes endpoint can return very large payloads on monorepo MRs. | The per-job 500-file cap (§ 3.5) bounds work; pagination of the GitLab API call itself is P9's concern. |
| F-4 | A bridge-internal metric for "how many blobs would have registered if both flags were on" for each merge in shadow-mode, for sizing prod opt-in cost. | P9 may add a shadow-counter per repo; useful but not blocking. |
| F-5 | Cross-fleet shared MR awareness. A merge in a repo that two fleets both watch produces two registrations. | Out of scope for P8. P7 F-4 already documents cross-fleet dedup as a future revisit. |
| F-6 | Mirror to a `git-mr` bead (a new type that points at the MR itself, separate from the per-file blobs). | Out of scope. P0–P8 are about file snapshots; MR-level metadata already lives on the issue bead. |
| F-7 | Non-merge-commit triggers (push, tag) for repos where MRs are not the canonical merge surface. | Acknowledge in § 1.4. Operator can register manually. P8 does not add a push trigger. |

## 11. What this ADR does NOT decide

- **Schema for `git-blob`** (P0).
- **Identity / dedup** rules (P1).
- **Storage tiering** (P2).
- **Secret-scanning policy** (P3).
- **CLI / API write surface** (P4) — the bridge calls whatever P4 locks; P8 just specifies it does call.
- **Graph integration** rules (P5) — the bridge writes `origin_bead`, which is a P5-locked field.
- **Embedding policy** (P6) — the bridge does not opt blobs into embedding; the per-repo embedding flag (D-8) is independent.
- **Lifecycle / GC** (P7) — bridge-registered blobs age out via the same predicate.
- **Implementation work** — every "the bridge does X" in this ADR is the P9 implementer's job to write. P8 is the spec.

P8 raises **one** `gb decision` (R-1: the three-layer flag matrix and the dev→rwx→prod rollout sequence) when the implementation epic begins. That decision is the operator's only fork on this ADR; everything else flows from P0–P7.

## 12. Consequences

### 12.1 For the rest of the research epic

- **P0–P7** are unchanged. P8 sits on top of them.
- The research epic (ka-0000003043) is complete after P8 lands. The next step is the operator authorizing the implementation epic (P9, plan-of-record § 11) — not a research task.

### 12.2 For the eventual P9 implementation

A non-exhaustive checklist:

1. Add the project-bead field `git_blob_register: bool` to the `type:project` config.
2. Extend `gardenio/controller/internal/bridge/gitlab_sync.go:582-637` to enqueue a `BlobRegistrationJob` after `mr_merged=true` is set, gated on the three-layer flag check.
3. Implement `BlobRegistrationJob` worker as an in-process goroutine pool. Bound concurrency (default 4 workers per bridge replica).
4. Add a `GitLabClient.GetMRChanges(projectPath, iid)` wrapper around `GET /projects/:id/merge_requests/:iid/changes`.
5. Add a `GitLabClient.GetFileContents(projectPath, path, ref)` wrapper around `GET /repository/files/:path/raw?ref=:sha` if not present.
6. Wire the env knobs: `GARDENIO_GITLAB_BRIDGE_BLOBS`, `GARDENIO_GITLAB_BRIDGE_BLOB_SKIP_PATHS`, `GARDENIO_GITLAB_BRIDGE_BLOB_MAX_FILES_PER_MR`, `GARDENIO_GITLAB_BRIDGE_BLOB_BURST_PER_REPO_PER_HOUR`, `GARDENIO_GITLAB_BRIDGE_BLOB_BURST_PER_FLEET_PER_HOUR`. Default values per § 3-§ 6.
7. Add the path-skip evaluator with default list per § 3.4.
8. Add the `[no-blobs]` MR-description marker check.
9. Add `kd admin git-blob backfill` per § 8. Admin-only via `min_role: gardenio-admin`. Emit one summary audit bead per run.
10. Add Prometheus counters: `git_blob_bridge_registered_total{repo}`, `git_blob_bridge_skipped_total{repo,reason}`, `git_blob_bridge_rate_limited_total{repo}`, `git_blob_bridge_job_duration_seconds`.
11. Document the rollout runbook in `gardenio/docs/git-blob-bridge-rollout.md`.

### 12.3 For operators

- A new fleet-level switch and a new per-project switch. Both default off; rolling forward is a deliberate Helm + `kd update` action.
- A new MR description marker (`[no-blobs]`) for sensitive merges.
- A new admin tool (`kd admin git-blob backfill`) for selective historical sweeps.
- Two new audit-bead sources: backfill summaries (one per run) and the existing `system:gitlab-bridge` actor in events (one entry per registered blob).

### 12.4 Cost / risk

- **Volume blow-up.** The top risk per plan-of-record § 12. Mitigated by: off-by-default per fleet, off-by-default per repo, MR-level opt-out, per-repo burst overlay, per-job file cap, no automatic backfill, dev→rwx→prod soak. Six independent levers must all be in the wrong position to flood storage.
- **Webhook reliability.** Mitigated by the existing 5-minute poller fallback and the natural-key idempotence (a duplicate registration is a no-op).
- **Bridge process restart loses in-flight worker progress.** Mitigated by poller recovery on the next tick. If measured load makes this insufficient, F-1 (persistent job table) is the escape valve.
- **Operator forgets `[no-blobs]` on a sensitive merge.** Mitigated by gitleaks scan (P3); an unredacted secret never lands in store. The marker is for *content the operator wants to keep out for non-secret reasons* (e.g., legal-sensitive prose). For those cases, the operator's recovery is to soft-delete the offending blobs immediately after the merge — the orphan-GC pathway will hard-delete in 30 days.
- **Rollback after partial registration.** A flipped-back flag stops new writes but leaves the already-written blobs. Orphan GC handles the cleanup at 90+30 days; an operator who wants faster cleanup uses `kd admin purge-repo` (P7 § 5.1) or per-blob `kd delete`.

## 13. Acceptance criteria

This ADR is accepted when:

1. The ADR doc lands on `main` via MR.
2. P0–P7 ADRs are merged (already true at branch creation: P0/P1/P2/P3/P6/P7 on main; P4/P5 in flight).
3. The accompanying `gb decision` (R-1) is resolved by the operator either accepting the recommended defaults (off-by-default fleet+repo, `[no-blobs]` marker, dev→rwx→prod with 7-day soaks, no auto-backfill, admin backfill tool) or directing a specific override.
4. P9 (implementation epic) can begin against the contract locked here, with no further ADR work needed for bridge integration / rollout / backfill / feature flagging.

This ADR is **not** the green light to extend the gitlab-bridge handler, write the `BlobRegistrationJob` worker, ship the `kd admin git-blob backfill` command, or flip any flag in any environment. Those are P9 deliverables, after the operator authorizes the implementation epic per plan-of-record § 11.

## 14. References

- Plan-of-record: `kbeads/docs/research-git-blob-bead-type.md` (§ 7.2 bridge integration; § 11 P8 row; § 12 risks).
- P0 ADR: `kbeads/docs/adr-git-blob-naming-schema.md` (ka-0000006442).
- P1 ADR: `kbeads/docs/adr/git-blob-P1-identity-dedup.md` (ka-0000002044).
- P2 ADR: `kbeads/docs/adr/git-blob-P2-storage-tiering.md` (ka-0000004323).
- P3 ADR: `kbeads/docs/adr/git-blob-P3-security-redaction.md` (ka-0000007332).
- P6 ADR: `kbeads/docs/adr/git-blob-P6-embedding-search.md` (ka-0000005801).
- P7 ADR: `kbeads/docs/adr/git-blob-P7-lifecycle-gc.md` (ka-0000005504).
- Existing webhook handler: `gardenio/controller/internal/bridge/gitlab_sync.go:582` (`merge_request` `action=merge` branch).
- Existing poller fallback: `gardenio/controller/internal/bridge/gitlab_sync.go:313` (`GitLabPoller`).
- Existing GitLab MR client: `gardenio/controller/internal/bridge/gitlab.go:96` (`GetMergeRequest`), `gitlab.go:120` (`ListMergedMRs`).
- Existing webhook entry point: `gardenio/controller/cmd/gitlab-bridge/main.go:135-156`.
- Audit-type precedent: `kbeads/internal/server/config.go` (`type:audit`, `min_role: gardenio-admin`).
- Event-recording contract: `kbeads/CLAUDE.md` § "Landing the plane" (every mutation calls `store.RecordEvent` + publisher).
- Companion phases:
  - P4 (ka-0000004396) — ingestion CLI / API spec; locks the write surface the bridge calls.
  - P5 (ka-0000002819) — graph integration; locks `origin_bead` semantics the bridge writes.
