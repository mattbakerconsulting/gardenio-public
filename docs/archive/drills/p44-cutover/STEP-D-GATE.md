# P4.4 Step-D data-write gate — execute the beads DB cutover

> Decision-ready artifact for the ONE operator gate that authorizes the
> irreversible prod DB cutover. Origin: cutover task **ka-0000004003**, plan
> **ka-0000006259**, exec task **ka-0000008220**. Operator: Matt Baker (said "go"
> 2026-06-28). Program hard rule §8: every data write needs a FRESH resolved gb
> decision — this is it.

## What you are authorizing

A single, bounded, in-VPC **freeze → dump → transfer → restore → verify** that
makes the new-prod RDS the source-of-truth for beads. Concretely:

1. **Freeze source** — scale `gardenio-beads` to 0 in acct-909 `gardenio` ns
   (sole direct DB writer; this takes my own kd/gb dark — I narrate via Slack).
2. **Dump (RO)** — pg17 `pg_dump -Fd -j4 --no-owner --no-acl` of the source
   `beads` DB (14 GB, schema 39, pgvector 0.8.0), as a source-cluster Job.
3. **Transfer** — tar the -Fd archive → S3 `gardenio-sessions-prod` (acct 133).
4. **Freeze dest daemon + DROP+CREATE** — scale dest `gardenio-beads` to 0, then
   `DROP DATABASE gardenio; CREATE DATABASE gardenio;` (dest DB is the P4.3 soak,
   replaced wholesale — see correction below).
5. **Restore (GATED WRITE)** — `pg_restore -j4` into dest db `gardenio`, as an
   in-VPC dest-cluster Job. Guarded by `--i-have-a-decision <this id>`.
6. **Verify (RO)** — per-table row counts + order-independent `bit_xor(md5(row))`
   checksums vs the source baseline, extension parity, sample pgvector NN query,
   and `revoked_tokens ≥ source`. Non-zero exit on ANY mismatch.
7. **On PASS** — bring the dest daemon up on the migrated DB. **On FAIL** —
   roll back (un-freeze source; source stays source-of-truth; no re-point).

## Scope / blast radius

- **Account 133 (dest) data write only**: DROP+CREATE+restore of db `gardenio` on
  `prod-gardenio-postgres`. NO change to RDS instance/EKS/network/KMS/S3 infra.
- **Account 909 (source)**: read-only dump + a `scale 0` (reversible) of
  `gardenio-beads`. No source data is mutated; rollback = `scale 1`.
- **Does NOT touch**: litellm DB (separate), any bridge (all OFF on dest), DNS,
  Slack/GitLab/JIRA re-point (those are step E, each its own later gate).

## Material correction vs the written plan (verified live 2026-06-28, RO)

- The dest beads daemon reads **db `gardenio`**, NOT a pristine `beads`. That DB
  is non-empty (20 tables, schema 39, ~51 soak beads). The restore therefore
  **DROP+CREATEs `gardenio`** rather than `CREATE DATABASE beads`. Content-level
  `pg_restore` moves the source `beads` dump into db `gardenio` cleanly.
- Schema parity holds: source 39 / dest image 2026.177.6 = schema 39; no 040 in
  flight. pgvector dest 0.8.1 ≥ source 0.8.0.

## Numbers (verified)

| Item | Value |
|---|---|
| Source volume | 14 GB — beads 478,974 / comments 90,595 / events 3,036,496 / embeddings 50,803 / revoked_tokens 0 (live RO re-verify 2026-06-30; grows ~live) |
| Rehearsal (12 GB, proven) | dump 5m31s + restore 9m06s = 14m37s, 19 tables bit-identical |
| 06-29 dry-run (pristine dest, ka-0000004728) | dump 366s + transfer ~5m + restore 605s + verify ~1m ≈ 22 min |
| S3 transfer (measured) | ~66–123 MB/s multipart → ~1–2 min/leg |
| **HONEST RTO estimate** | **~25–31 min** beads-API unavailability. The 22-min dry-run was a PRISTINE dest; the real cut ADDS source freeze + dest-daemon freeze + 2× Job pod-start/pg17-bootstrap + DROP/CREATE + 14-vs-12 GB scaling + control-plane re-point. This is OVER the 15-min target and at/over the 30-min ADR outer bound — get explicit operator agreement on a **35-min** bound before resolving step D. (review finding, ka-0000008220) |
| RTO monitor | `RTO_BUDGET_MIN=25`; "RTO EXCEEDED" is a Slack flag, NOT auto-rollback. Only a FAILED/unrecoverable Job rolls back. Operator is away → make T+budget an explicit page, not a silent log line. |
| Rollback | **TWO-SIDED** (split-brain guard): scale DEST `gardenio-beads` → 0 and assert down FIRST, THEN scale source → 1. Never both daemons at replicas≥1. |

## Hardening applied (review NO-GO blocking items B1–B6, 2026-06-30)

This gate is filed ONLY after the adversarial review's blocking items were closed
in code on this branch:
- **B1 (no surviving watcher / no idempotency):** `orchestrate.sh` now checks
  S3 `VERIFY_RESULT`==PASS before the destructive DROP and skips straight to
  bring-up; writes a terminal `CUTOVER_DONE` marker; a re-run is a safe no-op.
  `restore-job.sh` refuses to DROP when `CUTOVER_DONE` exists. **Still required
  at go-time:** run the driver itself as a Job (restartPolicy=OnFailure) and arm
  an independent S3-prefix Monitor that pages the away operator on PASS/FAIL.
- **B2 (second gardener roster):** `orchestrate.sh` asserts dest
  `trigger-engine`==0 AND `agents`==0 BEFORE scaling the daemon up; HALTS if not.
  `p44-cutover-freeze-values.yaml` overlay codifies both at `replicaCount:0` so a
  cutover-window helm apply cannot un-freeze them.
- **B3 (out-of-band actuator freeze clobbered by apply):** same overlay, pinned
  in `prod-real.tfvars` `gardenio_extra_values_files` (last-wins).
- **B4 (false-FAIL on TZ/DateStyle skew):** `checksum.sql` now pins
  `TimeZone=UTC`, `DateStyle='ISO, MDY'`, `intervalstyle=postgres`,
  `extra_float_digits=3` and echoes them into the baseline. Validated live on
  dest (md5 of a timestamptz row differs UTC vs America/New_York — the pin is
  necessary).
- **B5 (scoped-role):** keep the daemon on **master** through step D and step E;
  gate-2 creates the LOGIN ROLE + Secret ONLY; ownership transfer + re-point is a
  SEPARATE reversible post-cut gate. See `SCOPED-ROLE-GATE.md`.
- **B6 (cross-account S3 + KMS DataKey unproven):** `orchestrate.sh` now runs a
  pre-freeze (source-LIVE) preflight: `sts get-caller-identity` (must be acct
  133) + a real PUT-from-source / GET-from-dest S3 round-trip with the exact AKIA
  creds, proving `kms:GenerateDataKey`+`Decrypt`; aborts BEFORE any freeze on
  failure.

## Egress / identity (for the audit trail)

- Orchestrated from the source agent pod, egress IP **203.0.113.201**.
- Dest writes authenticate as IAM user `gardenio` in acct **123456789012** (the
  documented AKIA terraform/data exception, advice ka-0000007102).
- Restore is self-guarded: `restore-target.sh` refuses without the resolved
  decision id.

## Gate sequence (each its own fresh gb decision, operator-resolved)

The step-D data-write gate is the LAST of an ordered chain. Per program hard
rule §8 and the operator's 2026-06-30 directive, all of the following clear
FIRST, each as its own gb decision the operator resolves:

1. **Gate-1 — chart-pin apply.** `prod-real.tfvars` chart pin
   `2026.177.6 → 2026.179.11` (minimal pin carrying the gitlab-bridge ESO
   template; schema 039 preserved). The apply ALSO picks up the
   `p44-cutover-freeze-values.yaml` overlay → dest actuators stay frozen through
   the re-roll. Re-confirm no schema 040 at apply time.
2. **Gate-2 — scoped-role.** Create the `gardenio_app` LOGIN ROLE + seed its SM
   Secret ONLY. Daemon STAYS on master through step E. (`SCOPED-ROLE-GATE.md`.)
3. **Gate-3 — finalized step-D Job manifest + step-E overlay flip diff** review.
4. **This gate (step D) — the data write.** Resolve only after 1–3 + the
   pre-freeze preflight is demonstrated green in a same-day dry run.

## Full-harden deliverables (decision ka-0000005231 = hold_full_harden)

Per the operator's 2026-06-30 directive, ALL residual hardening landed in code
BEFORE the gate sequence begins (this MR, !5010). Beyond B1–B6:

- **WS1 — driver-as-Job + independent outcome pager (ka-0000001263).**
  `driver-job.sh`/`driver-entry.sh` run `orchestrate.sh` itself under a Job
  controller (`restartPolicy=OnFailure`, `backoffLimit=3`) in the SOURCE cluster,
  so a node reclaim/OOM during the window reschedules + RESUMES (safe via the B1
  S3 markers) instead of killing an inline driver. `s3-page-monitor.sh` /
  `page-monitor-job.sh` are an INDEPENDENT watcher (separate failure domain,
  talks only to S3 + Slack) that pages the away operator the instant
  `VERIFY_RESULT`/`CUTOVER_DONE` lands — or on a T+`DEADLINE_MIN` timeout
  (silence ≠ success). Arm the pager BEFORE launching the driver.
- **WS2 — baked pg17 (ka-0000003862).** `postgresql-client-17` is vendored into
  `/opt/pg17` in the agent image (clis.sh + versions/checksums + cache-epoch 14,
  mirrored in `.rwx/docker.yml`). `dump-job.sh`/`restore-job.sh` prefer the baked
  client and keep the runtime PGDG curl only as a guarded fallback — removing the
  apt.postgresql.org dependency from inside the frozen window. **IMAGE now pinned
  (ka-0000004003, 2026-07-01):** epoch-14 first shipped in release 2026.181.10
  (MR !5010 merge); `orchestrate.sh`, `driver-job.sh`, and `page-monitor-job.sh`
  `IMAGE=` are pinned to golden **2026.182.6** (>= .181.10, schema still 039).
  The B-C pre-freeze preflight STILL asserts the baked `/opt/pg17` client exists
  in the pinned IMAGE on BOTH clusters and ABORTS-no-freeze otherwise, so the pin
  is the sane default with the preflight as the real safety net. Override with
  `IMAGE=` at go-time to pin a different baked tag (must be >= 2026.181.10 /
  epoch-14). (Client-only; schema/parity unaffected.)
- **WS3 — step-E single-fleet ordering (ka-0000002957).** `step-e-repoint.sh`
  encodes the consumer re-point invariants; `orchestrate.sh` D.1 now ALSO
  quiesces source `gardenio-agents`+`trigger-engine`→0 and force-drains in-flight
  source agent pods, asserting 0 agent pods on BOTH clusters before any dest
  bring-up. Rollback restores source agents too.
- **WS4 — post-cut scoped-role (ka-0000001170).** `scoped-role-create.sql`
  (gate-2: role+secret only) + `scoped-role-transfer.sh` (post-cut, reversible:
  ALTER OWNER + boot-AS-`gardenio_app` preflight on a throwaway copy before any
  re-point). See `SCOPED-ROLE-GATE.md`.

A fresh adversarial re-review of the hardened toolkit runs after these land; the
gate sequence is proposed only on a GO verdict.

## If you approve (step D)

I run `orchestrate.sh` with `DECISION_ID=<this id>`. The script FIRST runs the
read-only pre-freeze preflight (AKIA identity + S3/KMS round trip + narration
check) while the source is still LIVE — aborting with NO freeze on any failure.
Only then do I freeze the source beads daemon, go quiet on beads, and narrate
freeze→dump→restore→verify via the (still-up) source slack-bridge. On verify
PASS the new daemon comes up on the migrated data **with actuators still frozen**
(no second fleet); I then re-point my own control plane, resume reporting, and
step E (consumer re-point + a SEPARATE actuator bring-up) proceeds gate-by-gate.
Source is left frozen as a rollback net; rollback is **two-sided** (demote dest
first). On verify FAIL: roll back, source stays source-of-truth, no re-point.
