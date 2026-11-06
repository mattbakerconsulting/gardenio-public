# P4.4 DB cutover — full-harden plan (decision ka-0000005231 = `hold_full_harden`)

> **Operator directive (Matt Baker, 2026-06-30, resolving ka-0000005231, option 2):**
> "Hold and land ALL residual hardening in code first — driver-as-Job + S3
> operator-page monitor, bake pg17 into the agent image to drop the in-window
> apt fetch, step-E single-fleet ordering, post-cut scoped-role ownership
> transfer — then re-review, THEN start the gate sequence. RTO posture settled:
> ~25-31min (35-min-bounded) beads-API blackout is ACCEPTABLE — do NOT pursue
> ADR snapshot/physical/defer-index escape hatches for RTO. Continue holding at
> every future gate; self-approve no apply/data-write."

This is the plan artifact that decision required. It covers **only code** landed
on the existing cutover MR (**!5010**, branch `fix/ka-0000008220`). **No freeze,
no data write, no AWS apply, no helm/terraform apply** happens under this plan.
Every apply and the step-D data write remain their own future operator-resolved
`gb decision`s. Source (acct 909) stays live and source-of-truth throughout.

---

## 0. Scope boundary (what this plan does NOT do)

- Does **not** begin the gate sequence (gate-1 chart-pin apply, gate-2
  scoped-role create, same-day preflight dry-run, step-D data write). Those start
  only AFTER this hardening lands AND a re-review clears it.
- Does **not** chase the RTO escape hatches (RDS snapshot/physical restore,
  defer-indexes). Operator settled the RTO posture: the ~25-31min logical
  freeze→dump→restore→verify path stands; a 35-min outer bound is accepted.
- Does **not** mutate any live system. All validation is local/offline
  (`bash -n`, `shellcheck`, `helm lint`, YAML render, `helm template` unit).

---

## 1. Workstreams (the four the operator named) → residual beads

| WS | Operator phrase | Residual bead | Deliverable (code) |
|----|-----------------|---------------|--------------------|
| **WS1** | "driver-as-Job + S3 operator-page monitor" | ka-0000001263 | `driver-job.yaml` (driver runs as a K8s Job, `restartPolicy=OnFailure`, `backoffLimit≥3`, in the SOURCE cluster) + `s3-page-monitor.sh`/`page-monitor-job.yaml` (independent watcher of `s3://…/<RUN_ID>/` that PAGES the away operator the instant `VERIFY_RESULT`/`CUTOVER_DONE` appears) |
| **WS2** | "bake pg17 into the agent image to drop the in-window apt fetch" | ka-0000003862 | `postgresql-client-17` baked into the agent image (Dockerfile **and** `.rwx/docker.yml`, via `versions.env` + `checksums.env` + cache-epoch bump); `dump-job.sh`/`restore-job.sh` use the baked pg17 with the runtime PGDG curl kept ONLY as a guarded fallback |
| **WS3** | "step-E single-fleet ordering" | ka-0000002957 | `step-e-repoint.sh` encoding the single-fleet invariants (source-agent quiesce at freeze, per-bridge stop-source→assert-0→seed-dest→enable→re-point-webhook, slack `--for=delete` wait, gitlab ESO secret-materialized check, gated actuator bring-up as separate micro-flips) + `orchestrate.sh` D.1 also quiesces source `gardenio-agents`→0 and force-drains in-flight source agent pods |
| **WS4** | "post-cut scoped-role ownership transfer" | ka-0000001170 | `scoped-role-create.sql` (gate-2: LOGIN role + SM secret only) + `scoped-role-transfer.sh` (post-cut: ALTER OWNER + boot-AS-`gardenio_app` preflight on a throwaway copy + reversible re-point), both idempotent and master-run |

A 5th, cross-cutting deliverable falls out of WS2: a **same-day dry-run
runbook** (`DRY-RUN.md`) wiring `DRY_RUN=1` then `REHEARSE_DUMP=1`, so ka-0000003862's
"prove the pre-freeze preflight green, same-day, before the window" can be
executed verbatim at go-time. (The dry-run itself is NOT run now — it is a
go-time RO action; this plan just makes it a single documented command.)

---

## 2. WS1 — driver-as-Job + independent S3 operator-page monitor (ka-0000001263)

**Problem.** `orchestrate.sh` is currently an inline bash process in the agent
pod. A node reclaim / OOM / pod eviction during the ~25-31min irreversible window
kills the driver mid-flight — a single point of failure. And the outcome
(`VERIFY_RESULT` PASS/FAIL) reaches the away operator only via the driver's own
`gb slack reply`; if the driver pod dies before that call, the operator never
hears the result.

**Fix (code).**
1. `driver-job.yaml` — a K8s Job template that runs `orchestrate.sh` itself in
   the SOURCE cluster: `restartPolicy: OnFailure`, `backoffLimit: 3`,
   `activeDeadlineSeconds` set to the 35-min bound + slack, `gardenio.agent`
   toleration, PSS-restricted securityContext (UID 1000, drop ALL,
   seccomp RuntimeDefault), the cutover scripts shipped via the same ConfigMap
   pattern the inner Jobs use. The orchestrator's S3 idempotency markers
   (`VERIFY_RESULT==PASS` skip-guard + `CUTOVER_DONE`) already make a re-run a
   safe no-op, so an OnFailure restart resumes instead of re-DROPping (B1, already
   in code — WS1 makes the restart actually happen by running the driver under a
   Job controller).
2. `s3-page-monitor.sh` + `page-monitor-job.yaml` — an INDEPENDENT watcher
   (separate Job/CronJob, not a child of the driver) that polls
   `s3://gardenio-sessions-prod/p44-cutover/<RUN_ID>/` and, the instant
   `VERIFY_RESULT` (PASS **or** FAIL) or `CUTOVER_DONE` appears, fires a
   high-signal page to the operator (`gb slack reply` to the cutover thread +,
   if available, a `gb escalation`/`gb decision`-grade ping). It also pages on a
   **timeout** (no marker by T+35min) so silence ≠ success. Runs in a cluster
   whose control plane is NOT frozen (it talks only to S3 + Slack bridge, never
   beads), so it survives the self-freeze.

**Why a Job, not just `nohup`:** the Job controller reschedules on node loss;
`nohup` in the dying pod does not. Idempotency markers guarantee the reschedule
is safe.

**Acceptance:** `kubectl apply --dry-run=client` renders both manifests;
`bash -n`/`shellcheck` clean on the monitor; the driver Job's command path is the
unchanged `orchestrate.sh` (no behavior fork between inline and Job runs).

---

## 3. WS2 — bake postgresql-client-17 into the agent image (ka-0000003862)

**Problem.** Both `dump-job.sh` and `restore-job.sh` `curl` a **hardcoded** pg17
`.deb` from `apt.postgresql.org` at runtime, INSIDE the frozen window, with
`backoffLimit:0`. The agent image ships `postgresql-client` = **pg16**
(transitive dep, confirmed `.rwx/docker.yml:1725`). pg16's `pg_dump` REFUSES the
PG17.7 source ("server version mismatch"), so pg17 is mandatory — but fetching it
live means: (a) a frozen-window dependency on an external mirror being up and
reachable from BOTH the 909 and 133 pod networks; (b) a pinned URL that 404s if
PGDG repackages (the URL pins `17.10-1.pgdg24.04+1`).

**Fix (code).** Bake `postgresql-client-17` into the agent image so the Jobs use
a local binary:
1. `versions.env` — add an explicit `POSTGRES_CLIENT_MAJOR=17` (and document the
   PGDG apt source). Replace the implicit transitive `postgresql-client` (pg16)
   with `postgresql-client-17` in `AGENT_PACKAGES`, installed from the PGDG apt
   repo (`apt.postgresql.org`) added in the agent stage.
2. `Dockerfile` (`agent` stage) — add the PGDG repo + key, `apt-get install
   postgresql-client-17`; symlink/expose `/usr/lib/postgresql/17/bin` on PATH (or
   `/usr/bin/pg_dump`→17 via `pg_wrapper`). Keep it in the `agent` stage only
   (base stays minimal).
3. `.rwx/docker.yml` — mirror the SAME PGDG repo + `postgresql-client-17` in the
   `agent-install-syspackages` task so the CI-assembled image matches the
   Dockerfile (the two build paths MUST stay byte-aligned — versions.env is the
   shared source of truth). Bump `agent-syspackages.lock` `cache-epoch` to force
   an RWX rebuild. Add the PGDG `.deb` sha to `checksums.env` if the CI extract
   path needs it.
4. `dump-job.sh` / `restore-job.sh` — detect a baked
   `/usr/lib/postgresql/17/bin/pg_dump` (or `pg_restore`) and use it; the
   existing runtime PGDG curl is retained ONLY as a guarded fallback behind a
   `[[ -x "$BAKED_PG17" ]] || <curl>` so an image that predates the bake still
   works. This removes the external dependency from the frozen window on the new
   image while keeping the script runnable on the current one.

**Risk noted in plan:** baking pg17 changes the agent image → it must be built
and rolled to BOTH clusters before the cut, and the dump/restore Jobs must run
the **new** image tag (the orchestrator's `IMAGE=` pin must point at the baked
tag). This is itself an image release; flagged for the go-time checklist, not a
data write. Until the new image ships, the fallback curl keeps the toolkit
functional. Schema/parity unaffected (client-only change).

**Acceptance:** `helm lint`/`hadolint` clean on the Dockerfile; `docker.yml`
yaml-valid; versions.env/checksums.env consistent; a documented note that the
image tag in `orchestrate.sh IMAGE=` advances to the baked build at go-time.

---

## 4. WS3 — step-E single-fleet ordering + source-agent quiesce (ka-0000002957)

**Problem.** Step E (consumer re-point) is currently **uncoded**, and the
adversarial review found split-brain hazards: (a) at D.1 freeze the orchestrator
scales only the source *daemon* to 0, NOT source *agents* — the reconciler
fail-safe leaves in-flight source agent pods alive, and they can still `git push`
/ comment on gitlab group 14176856 / hit Jira directly, bypassing the frozen
daemon; (b) bringing the dest fleet live and re-pointing bridges has ordering
invariants that, if violated, run two fleets or two bridge endpoints against one
external token.

**Fix (code).**
1. `orchestrate.sh` D.1 — in addition to `gardenio-beads→0`, scale source
   `gardenio-agents→0` and force-drain/`--grace-period=0 --force` delete in-flight
   source agent pods, then ASSERT 0 running agent pods on BOTH clusters before
   any dest bring-up. (Source `trigger-engine`→0 too, so it can't spawn during
   the window.)
2. `step-e-repoint.sh` — a new, separately-invoked, gated script encoding:
   - **Actuator bring-up is SEPARATE gated micro-flips AFTER re-point**, not part
     of step D: first reconcile restored in-flight agent beads to terminal, THEN
     revert the `p44-cutover-freeze-values.yaml` overlay (`replicaCount` back to
     run values) as its own apply.
   - **Per-bridge ordering:** stop source bridge → assert 0 source endpoints →
     seed dest secret → enable dest → re-point webhook URL. **gitlab last + most
     gated.**
   - **Slack:** scale source `slack-bridge`→0 and `kubectl wait --for=delete`
     for full pod termination before enabling dest (single Socket-Mode token).
   - **gitlab-bridge ESO:** the chart-pin bump makes the TEMPLATE present, but
     the secret materializes only when `gitlabBridge.enabled=true` +
     `externalSecrets.gitlabToken.remoteRef` is set — `step-e-repoint.sh` runs a
     `kubectl get secret` assertion that the synced key exists BEFORE declaring
     gitlab re-pointable.
   - Honors the existing `assert_not_slack_bridge` guard for the source narration
     path until the explicit slack cut-over step.
3. The rollback path in `orchestrate.sh` is extended so a verify-FAIL also
   restores source agents (scale back to run value) alongside the daemon, leaving
   source whole.

**Acceptance:** `bash -n`/`shellcheck` clean; `step-e-repoint.sh` is
DRY_RUN-capable (prints the ordered plan + assertions without mutating); no live
call is made now.

---

## 5. WS4 — post-cut scoped-role ownership transfer (ka-0000001170)

**Problem.** `SCOPED-ROLE-GATE.md` documents the SQL but ships no executable,
guarded scripts. Re-pointing the daemon to a non-owner scoped role mid-cut
CrashLoops it (WithInstance needs CREATE on schema public) and breaks the next
migration (DDL needs ownership) — so this is strictly POST-cut and reversible.

**Fix (code), matching the gate doc exactly.**
1. `scoped-role-create.sql` — gate-2 (pre-cut): idempotent `CREATE/ALTER ROLE
   gardenio_app WITH LOGIN PASSWORD :'scoped_pw'` only. Plus a tiny wrapper note
   for seeding `gardenio/prod/beads-scoped-db-password` in acct-133 SM + the dest
   ExternalSecret. Daemon stays on master.
2. `scoped-role-transfer.sh` — post-cut (SEPARATE reversible gate): runs as
   master; `ALTER DATABASE gardenio OWNER TO gardenio_app` + per-object
   `ALTER TABLE/SEQUENCE/FUNCTION … OWNER TO gardenio_app` + `GRANT ALL ON SCHEMA
   public`; then **boots AS `gardenio_app` on a throwaway copy** (the WithInstance
   preflight `CREATE TABLE IF NOT EXISTS _scoped_boot_probe(); DROP` + a
   representative SELECT/INSERT/DELETE on `beads`) BEFORE any re-point; emits the
   one-line `postgresExternal.username`→`gardenio_app` flip diff but does NOT
   apply it. Reversible (documents flip-back to master). Notes the
   future-new-extension caveat (master cannot grant rds_superuser → keep
   migrate-job on master or split migrate/runtime roles).

**Acceptance:** SQL parses (`psql -f --dry`/`-c` against a local pg is not
available offline → static review + `bash -n` on the wrapper); script is
idempotent and gated; nothing is run against live RDS now.

---

## 6. Validation (all offline; no live system touched)

- `bash -n` + `shellcheck` on every `.sh` (orchestrate, dump-job, restore-job,
  s3-page-monitor, step-e-repoint, scoped-role-transfer).
- `kubectl apply --dry-run=client -f` (or `--dry-run=server` only if it is a pure
  read — otherwise client) on every new manifest (driver-job, page-monitor-job).
- `helm lint gardenio/helm/gardenio` + `helm template` with the freeze overlay to
  confirm the trigger-engine `replicaCount:0` still renders (regression guard on
  the B2/B3 chart bugfix already in !5010).
- `hadolint` on the Dockerfile; YAML lint on `.rwx/docker.yml`.
- versions.env ↔ checksums.env ↔ cache-epoch consistency check.

---

## 7. Adversarial re-review (the operator's "THEN re-review")

After the four workstreams land, run a fresh adversarial pre-flight review
(multi-skeptic, grounded in the new branch code, same method that produced the
B1-B6 NO-GO) over the hardened toolkit. Any new blocking finding is fixed in
code on the same MR before the gate sequence is proposed. The re-review verdict
is recorded as a new doc under bundle ka-0000004665 and summarized to the
operator. **Only if the re-review is GO** do I (in a later, separate turn) file
gate-1 as a fresh `gb decision` — never auto-advancing.

---

## 8. Delivery & guardrails

- All code lands on **MR !5010** (`fix/ka-0000008220`), description updated with a
  "full-harden" section; `gb mr link` already set.
- Residual beads ka-0000001263 / ka-0000003862 / ka-0000002957 / ka-0000001170 get
  their code-complete deliverables and are closed (or moved to "ready for
  re-review") with a comment pointing at the commit.
- **Hard guardrails honored:** no freeze, no `scale 0` of any live deployment, no
  DROP/CREATE/restore, no `aws … apply`, no `terraform apply`, no `helm upgrade`,
  no merge of !5010 to main as an apply. Source 909 remains live + source-of-truth.
- The step-D data-write gate is NOT filed under this plan. The gate sequence
  begins only after re-review GO, each gate its own operator-resolved `gb
  decision`; I hold at every one and self-approve nothing.

Refs: exec ka-0000008220 · plan ka-0000006259 · MR !5010 · risk register
/beads/bundle/ka-0000004665 · gate docs STEP-D-GATE.md + SCOPED-ROLE-GATE.md.
