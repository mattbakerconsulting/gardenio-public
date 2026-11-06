# S3 Session Lifecycle (ka-0000002503)

Coop persists per-session artifacts to S3 under a per-spawn key layout so that
multiple agent spawns under the same bead can coexist, and orphaned prefixes
(beads that have been closed/deleted) can be expired by a bucket lifecycle rule.

## Key layout

```
<configured-prefix>/<session-key>/{meta.json,session.jsonl,transcripts/N.jsonl,recording/lo-hi.jsonl}
```

Where `<session-key>` is composed from the launch flags as follows:

| `--s3-session-key` (bead id) | `--s3-resume-session` | resulting `session-key`     |
| ---------------------------- | --------------------- | --------------------------- |
| set (e.g. `ka-abc123`)       | (any)                 | `<bead-id>/<coop-uuid>`     |
| unset                        | set                   | `<resume-id>` (flat)        |
| unset                        | unset                 | `<coop-uuid>` (flat)        |

When a bead id is set, every spawn writes under its own `<bead>/<coop-uuid>/`
sub-prefix. The bead-id segment is the canonical "directory" for everything
related to that agent bead; the per-spawn segment disambiguates multiple coop
processes that have been launched against the bead over time (restarts,
retries, multi-session-per-bead workflows).

### Restore semantics

`pick_restore_key` selects the prefix to restore from at session start:

1. If `--s3-session-key` is set, list `<bead>/*/meta.json` and pick the spawn
   with the greatest `started_at` (lex tie-break by spawn id, descending).
2. If no spawn metas exist under the bead (e.g. legacy flat layout, or the bead
   has never been used), fall back to `<bead>` directly.
3. If only `--s3-resume-session` is set, restore from `<resume-id>`.
4. Otherwise, fresh start (no restore).

This preserves backwards compatibility with sessions written under the older
flat layout while letting new sessions branch under per-spawn sub-keys.

## Codex sessions (gd-JeZ78WY3we, ADR 0004 §9)

Upload is unchanged — the file registered as the session log is copied to
`session.jsonl` — and `meta.json` gains `"harness": "codex"` (older metas fall
back to `agent_type`). The Codex session log is the **rollout**,
`$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<thread-id>.jsonl`. The app-server
backend publishes its path from the `thread/start` response; the `exec` backend
finds it by thread id once the file exists. Nothing else under `$CODEX_HOME`
(sqlite state, `auth.json`) is uploaded, and there is no `workflow/` companion
dir.

Restore, for a `--agent codex` start:

1. Pick the spawn prefix as above, then read its `meta.json`. If its harness is
   not `codex` (the bead used to run Claude) the restore is skipped: no
   cross-harness migration.
2. Download `session.jsonl`, read the thread id and start time from its leading
   `session_meta` record, and copy it to the canonical path under `$CODEX_HOME`
   (`~/.codex` if unset). A local rollout at least as large is kept: the file is
   append-only, so the larger one is the fresher.
3. Set `--resume` to the thread id. The backends resume by id (`thread/resume` in
   app-server mode, `codex exec resume <id>` in exec mode); the Claude
   `--resume <uuid>` path and transcript materialization are not used.

`--s3-no-restore` skips all of this. `POST /api/v1/session/retire` renames a
Codex rollout to `*.jsonl.stale`, which Codex's by-id lookup no longer matches.

Usage for Codex comes from the stream coop already tees: `turn.completed.usage`
(`exec`) and growth of the running total in `thread/tokenUsage/updated`
(app-server). The rollout itself is not tailed. Cost stays 0 (subscription).

## Bucket lifecycle rule

Orphan prefixes accumulate when a bead is closed/deleted but its S3 artifacts
remain. Configure the bucket with an expiration lifecycle rule keyed off
prefix and object age. Recommended retention: **30 days** since last
modification, applied to the entire coop session prefix.

### AWS CLI form

```sh
aws s3api put-bucket-lifecycle-configuration \
  --bucket "${BUCKET}" \
  --lifecycle-configuration '{
    "Rules": [
      {
        "ID": "coop-session-retention",
        "Status": "Enabled",
        "Filter": { "Prefix": "'"${COOP_S3_PREFIX:-coop/}"'" },
        "Expiration": { "Days": 30 },
        "AbortIncompleteMultipartUpload": { "DaysAfterInitiation": 1 },
        "NoncurrentVersionExpiration": { "NoncurrentDays": 7 }
      }
    ]
  }'
```

### Terraform form

```hcl
resource "aws_s3_bucket_lifecycle_configuration" "coop_sessions" {
  bucket = aws_s3_bucket.coop_sessions.id

  rule {
    id     = "coop-session-retention"
    status = "Enabled"

    filter { prefix = var.coop_s3_prefix }

    expiration { days = 30 }

    abort_incomplete_multipart_upload { days_after_initiation = 1 }

    noncurrent_version_expiration { noncurrent_days = 7 }
  }
}
```

### Notes

- The rule expires every object under the prefix on age, regardless of whether
  the owning bead is open or closed. This is intentional: even live beads
  rotate transcripts on every spawn, and the most recent spawn under a live
  bead will be re-uploaded on the next coop run, refreshing its
  `LastModified`. Spawns belonging to closed/deleted beads (orphans) simply
  age out.
- 30 days is a default. Tune per-environment via the `coop_s3_prefix` /
  `expiration_days` knobs in the site-deployment caller.
- Multipart abort + noncurrent-version expiration are belt-and-suspenders —
  coop does not currently use multipart or versioned writes, but configuring
  them up front avoids surprise charges if behavior changes later.
- The lifecycle rule is the *only* mechanism that deletes orphan prefixes.
  Coop itself never reaches into S3 to delete sessions; its own writes are
  append-only.
