# Full release-DAG rearchitecture (gardenio CI)

- **Bead:** ka-0000006850
- **Parent epic:** ka-0000004002 (Cut GitLab CI-minute spend ~90%)
- **Resolved decision:** ka-0000001414 → `full_release_dag`
- **Control-plane precedent:** ka-0000000408 (dispatch-only deploy; RWX does not post commit statuses)
- **Status:** design / implementation-gated (see "Implementation gate" below)

## Why this document exists

The original task — "drop `rwx results --wait` and let RWX report pass/fail as a
GitLab commit status" — was investigated and proven **not viable as scoped**
(ka-0000006850 comment 75440). Three independent facts kill the naive version:

1. **The four target jobs are not MR gates.** `rwx-deploy-rwx`,
   `rwx-deploy-rwx-singleton`, `rwx-smoke`, and `rwx-e2e` run on
   `main`-push / tag / schedule — never on `merge_request_event`. So the
   monorepo "manual-on-MR" lever (DEVOPS-640/641/652) does not transfer.

2. **The real gate is GitLab `needs:`-sequencing, not a commit status.** The
   tag/release chain is `rwx-deploy-rwx → rwx-e2e → promote → rwx-deploy-prod`,
   each `needs:` the prior. GitLab `needs:` can only wait on **same-pipeline
   jobs**; it cannot consume an external commit status. An RWX-posted status
   would therefore not gate `promote` or `rwx-deploy-prod`.

3. **RWX posts no commit status for these workflows.** They are
   `on.dispatch`-triggered. RWX-native status-checks fire only for
   GitLab-**event**-triggered runs (the `gitlab:` block in `ci.yml`).
   `deploy-rwx.yml` and `smoke-rwx.yml` explicitly document that they do not
   POST a status (control-plane decision ka-0000000408).

Dropping `--wait` per-job breaks **both** the release-sequencing gate (#2) and
deploy→test ordering (tests would run against a half-deployed namespace).

The resolved path, `full_release_dag`, sidesteps all three by **moving the
sequencing and gating inside a single RWX workflow DAG** so GitLab issues one
fire-and-forget dispatch instead of standing up four (or more) sleeping
runner jobs chained by `needs:`.

## Current architecture (as-built)

GitLab CI is the control plane. Each release step is a native GitLab job that
dispatches an RWX workflow and **blocks** on `rwx results --wait`
(`.rwx-dispatch-and-wait` anchor, `.gitlab-ci.yml:65`). The runner sleeps while
RWX does the real compute on its own infra; the GitLab job's exit code becomes
the native commit status.

### Tag / release pipeline (the full DAG)

```text
rwx-deploy-rwx ──┬─> rwx-e2e ──┬─> promote ──┬─> rwx-deploy-prod ─> publish-release-metadata
                 │             │             └─> create-release
rwx-deploy-rwx-singleton       └────────────────> bundle-airgap (manual, allow_failure)
```

| GitLab job                  | dispatches              | `--wait` | timeout | `needs:` |
|-----------------------------|-------------------------|----------|---------|----------|
| `rwx-deploy-rwx`            | `gardenio-deploy-rwx`    | yes      | 30m     | —        |
| `rwx-deploy-rwx-singleton` | `gardenio-deploy-rwx`    | yes      | 30m     | deploy-rwx |
| `rwx-e2e`                  | `gardenio-e2e`           | yes      | (def)   | deploy-rwx |
| `promote`                  | `gardenio-promote`       | yes      | 30m     | e2e      |
| `rwx-deploy-prod`          | `gardenio-deploy`        | yes      | 45m     | e2e, promote |
| `bundle-airgap` (manual)   | `gardenio-bundle`        | yes      | 50m     | promote  |
| `create-release`           | — (release-cli)         | n/a      | (def)   | e2e, promote |
| `publish-release-metadata` | — (curl)                | n/a      | (def)   | deploy-prod |

### Main-push pipeline

```
rwx-deploy-rwx ──> rwx-smoke
rwx-deploy-rwx-singleton
```

### Scheduled pipeline

```
rwx-deploy-rwx ──> rwx-e2e (full suite) + rwx-gvisor-e2e + mirror-refresh
rwx-deploy-rwx-singleton
```

### Cost (from epic diagnosis ka-0000006989)

Most idle-wait minutes are **main-push** volume: ~983 deploy + ~797 smoke
runs/mo. The tag/release chain is low-volume but it is the actual production
gate, so its `--wait` minutes are fewer but its correctness bar is highest.
Task estimate: ~9,500–10,500 min/mo recoverable across the chain.

## Target architecture

Collapse each pipeline's chained dispatch-and-wait jobs into **one RWX
workflow** whose internal task graph encodes the ordering that GitLab `needs:`
currently encodes. GitLab issues **one** dispatch and (optionally) one short
status reporter, instead of N sleeping runner jobs.

### Composition primitive

RWX expresses intra-workflow ordering with `use: [taskA, taskB]` (already used
throughout — e.g. `deploy-rwx.yml:249 use: [code, verify-images]`). A single
release workflow can therefore declare:

```text
deploy-ephemeral ─┬─> e2e ──> promote ──> deploy-prod
deploy-singleton  │
                  └─ (e2e use: [deploy-ephemeral])
```

…all as `use:`-ordered tasks in **one** RWX run. RWX runs them on its own infra;
GitLab is not billed to wait between them.

### Three workflow targets (one per pipeline shape)

1. **`gardenio-release` (tag push)** — one DAG: `deploy-ephemeral` +
   `deploy-singleton` → `e2e` → `promote` → `deploy-prod`, plus the
   GitLab-Release + metadata steps that today live in `create-release` /
   `publish-release-metadata`. The prod-deploy freeze gate
   (`.rwx/PROD_DEPLOY_FROZEN`, ka-0000004932) moves into the workflow as a
   pre-`deploy-prod` task that fails the run.

2. **`gardenio-main` (main push)** — one DAG: `deploy-ephemeral` +
   `deploy-singleton` → `smoke`.

3. **`gardenio-scheduled` (schedule)** — one DAG: `deploy-ephemeral` +
   `deploy-singleton` → `e2e` (full) + `gvisor-e2e` + `mirror-refresh`.

These reuse the existing `gardenio-deploy-rwx`, `gardenio-e2e`,
`gardenio-promote`, `gardenio-deploy`, `gardenio-bundle` task bodies — composed,
not rewritten.

### Reporting back to GitLab (replacing the `needs:` gate)

GitLab `needs:` can't consume an external status, and dispatch-triggered RWX
runs post nothing. So the new workflow must **report its own terminal status**:

- **Option R1 — keep one thin `--wait` reporter job.** GitLab dispatches the
  single workflow and one short job blocks on `rwx results --wait --fail-fast`.
  This still bills one runner-sleep, but **one instead of four**, and it keeps
  the native GitLab commit status + merge/release gate **exactly as today** (no
  reversal of ka-0000000408, no new token scope). Savings come from collapsing
  4–6 sleeping jobs into 1.

- **Option R2 — RWX POSTs a commit status itself.** A terminal RWX task POSTs
  to `…/repository/commits/<sha>/statuses` via `GITLAB_CLONE_TOKEN`. Precedent:
  `coverage.yml` already calls the GitLab v4 API with this token
  (`coverage.yml:357,370`). GitLab fires the dispatch and forgets — **zero**
  runner-sleep. Requires: (a) verifying `GITLAB_CLONE_TOKEN` has `api` scope
  for `/statuses` (it currently does `read_repository`-style package + read
  calls), (b) reversing ka-0000000408 for these workflows, (c) a watchdog so a
  silently-dead RWX run does not leave the SHA permanently `pending`.

**Recommendation: ship R1 first, then R2.** R1 captures the bulk of the savings
(4–6 jobs → 1) with zero gate-semantics change and zero new token surface — it
is the low-risk 80%. R2 is a follow-up that removes the last runner-sleep once
the token scope and watchdog are proven. This staging also lets the prod path
be validated under R1's identical-to-today gate before any fire-and-forget
behavior touches `deploy-prod`.

## Risks and constraints (must survive)

| Constraint | Source | How the design preserves it |
|---|---|---|
| Prod never deploys against -rc-only images | ka-0000008209 | `deploy-prod` task `use: [promote]` inside the DAG |
| Single prod deploy at a time (last-writer race) | ka-0000006240 | RWX run-level concurrency / queue keyed on `prod-deploy`; verify RWX queue semantics match GitLab `resource_group` |
| Single singleton deploy at a time | ka-0000003721 | same; `resource_group: deploy-rwx-singleton` → RWX queue |
| Prod-deploy freeze marker honored | ka-0000004932 | pre-`deploy-prod` task fails on `.rwx/PROD_DEPLOY_FROZEN` |
| Tag vs main instance-id collision avoided | ka-0000000423 | preserve `tag-<calver>` vs `<short-sha>` instance-id derivation |
| e2e attaches to the namespace deploy created | E2 plan-of-record | pass `instance-id` between tasks in-DAG (no race) |
| `bundle-airgap` never gates prod | (in-file) | keep manual/opt-in; out of the gating DAG |
| GitLab Release published only post-promote | 2026.113.4 incident | release-creation task `use: [promote]` |

**Open verification items before implementation:**

- V1. RWX run-level **concurrency/queue** primitive equivalent to GitLab
  `resource_group` (serialize prod + singleton deploys). If RWX has no
  exclusive queue, prod-deploy must stay a GitLab job with `resource_group`.
- V2. `GITLAB_CLONE_TOKEN` API scope for `POST …/statuses` (needed only for R2).
- V3. How the GitLab **Release object** (`create-release`, native `release:`
  keyword) is produced if the release step moves RWX-side — GitLab `release:`
  requires a GitLab job; likely keep a thin release-cli job `needs:` the single
  reporter, rather than moving Release creation into RWX.
- V4. RWX **DAG failure semantics** vs `rwx results --fail-fast` — confirm a
  failed upstream task aborts downstream tasks and surfaces a non-zero terminal
  status the reporter/POST can read.

## Phased implementation plan (proposed child tasks)

Sequenced cheap-and-safe first; nothing touching prod merges before its gate is
proven identical-to-today.

- **P0 — Verify primitives (V1–V4).** No pipeline edits. Output: a findings
  comment + go/no-go on R1 vs R2 and on moving prod-deploy RWX-side at all.
- **P1 — main-push DAG (R1). ✅ SHIPPED** (ka-0000004275, `.rwx/main.yml`).
  Collapsed `rwx-deploy-rwx` + `-singleton` + `rwx-smoke` into `gardenio-main`;
  one `--wait` reporter. Captures the largest single block of savings
  (~983+797 runs/mo).
- **P2 — scheduled DAG (R1). ✅ SHIPPED** (ka-0000007436, `.rwx/scheduled.yml`,
  MR !4401). Collapsed the scheduled deploy + singleton + full-e2e fan-out.
  Also fixed a latent hazard: `rwx-e2e` had a `schedule` rule while its
  `needs: rwx-deploy-rwx` upstream was tag-only, so on a scheduled pipeline the
  deploy job was never created. `rwx-e2e` is now tag-only; the scheduled path
  runs the embedded DAG.
- **P3 — tag/release DAG (R1). 🟡 MR open, merge operator-gated** (ka-0000007165,
  `.rwx/release.yml`). Collapsed `deploy→e2e` into `gardenio-release` with one
  reporter; `promote` / `rwx-deploy-prod` / `create-release` stay separate GitLab
  jobs that `needs:` the reporter. **Key design point:** `promote` is NOT embedded
  `after: e2e` because the reporter preserves the MAX_MODULE_FAILURES stopgap
  (release proceeds when the e2e run fails but ≤N modules failed); RWX `after:`
  would run promote only on a fully-succeeded e2e sub-run, silently deleting that
  tolerance. prod-deploy stays separate to keep `resource_group: prod-deploy`,
  the `.rwx/PROD_DEPLOY_FROZEN` gate, and the monotonic-version guard (V1 caveat
  #2: a run-level pool would serialize the whole release run, and a run can hold
  only one exclusive lease). The embedded e2e shard logs are addressed by task ID
  via `rwx results --json` recursion (bare `--task e2e-shard-N` does not reach
  nested subtasks). **Operator decision required before merge** (prod-touching).
- **P4 — R2 fire-and-forget (optional).** Once V2 + watchdog land, replace the
  remaining `--wait` reporter(s) with an RWX-side status POST. Separate decision.

P1–P2 deliver the bulk of the savings at near-zero gate risk. P3 is the
prod-touching step and is the one that strictly needs operator sign-off.

## Relationship to sibling work

- **ka-0000000557** (collapse the two deploy dispatches + add
  `auto_cancel.on_new_commit: interruptible`) is a strict **subset** of P1's
  deploy collapse. Recommend folding ka-0000000557 into P1 (or sequencing P1
  after it) to avoid two MRs editing the same deploy jobs.
- This design **does not** weaken any gate; it relocates the same ordering from
  GitLab `needs:` into an RWX `use:` DAG and keeps (R1) or replicates (R2) the
  commit status. The merge/release gate continues to block on a green status.
```
