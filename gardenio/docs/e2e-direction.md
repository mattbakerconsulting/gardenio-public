# E2E Direction — Autonomous Gardener (living doc)

The **E2E Direction Gardener** (schedule bead `ka-0000006692`) is the
orchestrator for the gardenio E2E suite's forward direction. It runs daily
at 09:17 PT on weekdays and is **autonomous**: each cycle it surveys the
suite, identifies movement, and **spawns crews on multiple workstreams in
parallel** without waiting for a human to pick a lane. It files a
`gb decision` only for genuine architectural forks, not as a daily
check-in.

This file is the gardener's playbook. The gardener reads it on every cycle,
follows the procedure, and **updates the "Recent cycles" section
in-place** with a one-line ledger entry per run. Snapshots and per-cycle
detail live as comments on the schedule bead `ka-0000006692` and on
parent task `ka-0000004587`. This file holds the current playbook plus
the most recent ledger lines.

Parent task: **ka-0000004587**.
Sibling gardener (CI/CD triage, every 30m, NOT this gardener): **ka-0000001155**.
Crew role: spawn as `crew-e2e`. Gardener runs as `gardener-e2e`.

---

## Mission

Keep the gardenio E2E suite **moving forward on its own**. The operator
should never have to ask "what's happening with E2E?" — the answer is
visible in:

- this file's **Recent cycles** section (one line per workday),
- in-flight crews under tag `e2e` (`kd list -l e2e --status=in_progress`),
- the running ledger comments on parent task **ka-0000004587**.

The gardener owns **direction and dispatch**, not test fixing. Failing
tests on main are the regular CI/CD gardener's beat (ka-0000001155).

## What the gardener owns

1. **Coverage backfill** for surfaces shipped without E2E modules — walk
   merged feature MRs from the last 7 days and identify gaps.
2. **Anti-flake data** — aggregate JUnit from recent RWX runs, surface
   per-module pass rates, file P2 bugs against the worst offenders.
3. **Reliability epics** — actively progress ka-0000003762 (deploy↔E2E
   mutex stopgap) and ka-0000004402 (per-MR ephemeral namespaces) by
   spawning a crew on the next research/design child each cycle.
4. **Drift detection** — `ls scripts/test-*.sh` vs.
   `grep run_module run-shard.sh`. File a bug + remediation MR when
   drift exists.
5. **Live e2e bugs** — known P1 e2e bugs (e.g. ka-0000005438 DatePicker
   tests, ka-0000007626 smoke-rwx race) get nudged with a crew if no
   one has picked them up for >24h.

## What the gardener does NOT own

- Quarantining flakes — operator-owned (per `gb advice show ka-0000003926`).
  The gardener never edits `.captain/quarantines/gardenio-e2e.yml`.
- Dispatching RWX runs — the regular CI/CD gardener (ka-0000001155) owns
  every-30m dispatch and red-CI triage.
- Pushing code itself — crews follow the standard branch + MR workflow.
  Operator authorization (ka-0000002230) lets agents merge their own
  MRs in this repo, but the gardener itself does not push or merge.

---

## Cycle procedure (every weekday 09:17 PT)

The gardener executes this in order. Steps 2–6 each *may* spawn a crew —
the gardener dispatches up to 5 crews per cycle in total.

### 1. Snapshot

Gather the daily numbers. Compare against the last entry in
**Recent cycles** below to compute deltas.

```sh
# Module count = run_module invocations only (excludes the function def line).
# NOT a wired-SCRIPT count: some scripts are invoked under more than one
# run_module name (multi-mode), so this runs HIGHER than the number of distinct
# scripts wired (138 invocations vs 136 scripts on 2026-08-05). Quote it as
# "modules", never subtract it from the script count to infer drift — that
# heuristic produced a false +14 in cycle 1 and a false +2 in cycle 2.
grep -cE '^\s+run_module ' gardenio/tests/e2e/scripts/run-shard.sh
# AUTHORITATIVE drift check (ka-0000004758): the `test-shard-isolation.sh`
# lint now MECHANICALLY asserts exclusion-list completeness — every
# scripts/test-*.sh on disk is either invoked by a run_module call OR named in
# the run-shard.sh header exclusion block (between the `shard-isolation-lint:
# exclusion-list BEGIN/END` sentinels), and no exclusion entry is stale. It
# exits 0 iff effective drift == 0 and prints the reconciliation
# ("N on disk = W wired + E excluded"). Run it and read the exit code; do NOT
# re-derive drift by hand from the prose arithmetic below.
bash gardenio/tests/e2e/scripts/test-shard-isolation.sh
# This same lint runs in CI as the `test-shard-isolation` task (.rwx/ci.yml,
# in the required CI/Test status check), so undocumented scripts and stale
# exclusion entries now RED an MR instead of surviving to the next weekly
# survey. The prose exclusion families are documented in the run-shard.sh
# header (hermetic block + out-of-band cluster block) and in the two
# paragraphs below ("The twelve hermetic scripts…" / "…six real cluster E2Es…")
# — kept only for human context, no longer the guard.
sed -n '/^quarantines:/,$p' .captain/quarantines/gardenio-e2e.yml | grep -c '^  - '
kd list -l e2e --status=open,in_progress
glab ci list -R acme/platform/gardenio -r main -P 10
glab ci list -R acme/platform/gardenio --scope tags -P 10
```

> **`main` pipeline greenness is NOT an E2E signal — do not report it as one.**
> Main-push pipelines contain **zero E2E jobs**. Verified 2026-08-06 (cycle 64):
> the complete job list of a main pipeline is `gardener-prompt-drift-alarm`,
> `coverage`, `coverage-baselines-recompute`, `rwx-gvisor-readiness`,
> `rwx-status-watchdog`, `teardown-rwx-manual`. This follows from the documented
> post-2026-07-09 change (main-push no longer deploys staging; only scheduled and
> tag pipelines run staging deploy + full E2E) — but the snapshot step above
> predates it, so cycles kept reporting "N/10 recent main pipelines green" off a
> pipeline class that **cannot go red for an E2E reason**. There are also **no
> scheduled pipelines** on this project (`pipeline_schedules` → `[]`), so **tag
> (release) runs are the only E2E signal that exists.**
>
> In the window 2026.218.9 … .25, main was 10/10 green while **13 of 16 release
> runs were RED** on E2E module failures. Quote the release gate, not `-r main`.
>
> To read the real per-module verdict:
>
> ```sh
> SHA=$(glab api ".../repository/tags/<tag>" | jq -r .commit.id)
> glab api ".../repository/commits/$SHA/statuses" \
>   | jq -r '.[]|select(.name=="rwx/release")|"\(.status) \(.target_url)"'
> # target_url carries the RWX run id:
> rwx results <run> --format json   # find the `release-gate` task id
> rwx logs <release-gate-task-id>   # FINAL_STATUS / PASSED_ / FAILED_ / SKIPPED_MODULES
> ```
>
> The `release-gate` log carries the full per-module verdict for green **and**
> red runs. (On green runs the E2E sub-run collapses into one `e2e` task, so
> per-shard logs exist only for failures.) Don't bother grepping the GitLab
> `rwx-release` job trace — it is a fire-and-forget dispatcher and contains no
> `MODULE PASSED/FAILED` lines at all.

The twelve hermetic scripts are intentionally **not** wired into
`run-shard.sh`: `test-lib-retry.sh` is a unit test for `lib-retry.sh`
(no kubectl, no beads daemon); `test-lib-gvisor-node.sh` is a cluster-free
unit test for `lib-gvisor-node.sh` (the gVisor-poc substrate warm-node gate +
bounded signature-keyed retry, decision ka-0000000122) — run in CI by the
`test-gvisor-node-lib` task in `.rwx/ci.yml`; `test-lib-gvisor-forensics.sh`
is a cluster-free unit test for `lib-gvisor-forensics.sh` (Rig R2, the canary
forensic auto-capture harness, bead ka-0000006948) — run in CI by the
`test-gvisor-forensics-lib` task in `.rwx/ci.yml`; `test-shard-isolation.sh`
is the partition-invariant lint for `run-shard.sh` itself;
`test-shard-budget-guard.sh` is the hermetic unit test for the budget-aware
in-shard retry guard (ka-0000003834) — it lifts
`shard_seconds_remaining()`/`retry_fits_budget()` out of `run-shard.sh` and
drives them across the wall-clock budget boundary (no kubectl/beads),
run in CI by the `test-shard-budget-guard` task in `.rwx/ci.yml`;
`test-claudeless-scenario-isolated.sh` and
`test-claudeless-multi-nudge-isolated.sh` are standalone claudeless
lifecycle/multi-nudge repros (ka-0000002034) that drive coop/controller
against a local mock (no kubectl, no beads daemon, no namespace);
`test-concurrency-isolation.sh` is a static `$NAMESPACE`-hygiene lint
over the other `test-*.sh` scripts; and `test-outbox-shape-selftest.sh`
and `test-lib-outbox-rig.sh` are credless self-tests of the 1:1 GBFSM
outbox CREATE-shape and of `lib-outbox-rig.sh`'s `outbox_rig_guard`
respectively (ka-0000003147; no kubectl, no beads daemon) — both run in
CI by the `test-outbox-shape` task in `.rwx/ci.yml`; and
`test-skip-visibility-lint.sh` is the hermetic gate for the skip-sentinel
protocol (ka-0000000989) — it asserts `run_module` records a self-skip
(exit 0 + `E2E_SKIP_SENTINEL`) as a distinct SKIPPED outcome rather than a
PASS, and that every skip-family exit-0 site writes the sentinel, so
green-by-skip cannot silently return (no kubectl/beads) — run in CI by the
`test-skip-visibility` task in `.rwx/ci.yml`; and
`test-terraform-prewarm-cache.sh` is the sanity check that the agent-image
terraform provider prewarm populates a non-empty, consumable plugin cache
(ka-0000002834) — its `--contract-only` mode runs credless on bare ubuntu in
the `test-terraform-prewarm-contract` task (`.rwx/ci.yml`), and the full
populate+consume proof runs against the freshly-built cache in the
`agent-install-terraform-providers` task (`.rwx/docker.yml`). All twelve are
documented in `gardenio/tests/e2e/scripts/run-shard.sh`'s header and discussed
in `gardenio/tests/e2e/README.md`.

Beyond the twelve hermetic scripts, **six real cluster/proxy E2Es** are wired
out-of-band — they DO talk to kubectl/beads/a live proxy but are dispatched by
another harness, so they carry no `run_module` call here: the four gVisor-suite
scripts (`test-gvisor-substrate-ready.sh`, `test-gvisor-freeze-interrupt.sh`,
`test-gvisor-renudge-thaw.sh`, `test-gvisor-abort-races-completion.sh`) each
run under their own dedicated `gardenio-gvisor-*` RWX dispatch
(`.rwx/e2e.yml`), gated on `GVISOR_E2E=1` + the `gvisor-poc` substrate;
`test-pf-supervisor.sh` runs via the `test-pf-supervisor` target in
`gardenio/Makefile`; and `test-litellm-budget-cap.sh` is the per-CI budget-cap
acceptance (ka-0000000823) run on demand against a live LiteLLM proxy with a
`LITELLM_MASTER_KEY` — it spends real (tiny) money, so it is never sharded.
These are also subtracted in the drift command above and documented in the
run-shard.sh header out-of-band block.

If you find yourself adding another out-of-band exclusion (hermetic or
cluster), update the lists in all three places (this doc, run-shard.sh
header, README.md).

### 2. Drift check (auto-spawn)

Drift is now decided by the `test-shard-isolation.sh` lint, not by hand:
if `bash gardenio/tests/e2e/scripts/test-shard-isolation.sh` exits non-zero,
there is real drift (an undocumented script, a stale exclusion entry, or an
unaccounted addition — the lint names the offending file(s)). The gardener
spawns a crew immediately on a new task to file the diff and propose a fix.
One-shot per drift event — do not re-spawn while a prior drift crew is still
in flight. Because the same lint gates every MR/main push as the CI
`test-shard-isolation` task, a genuinely-drifted `main` should be rare — the
red would surface on the introducing MR first.

### 3. Coverage-gap pass (auto-spawn, daily)

Walk merged MRs from the last 7 days that touched runtime code paths
(`gardenio/controller/`, `kbeads/`, `coop/crates/`, `gardenio/helm/`):

```sh
glab mr list -R example-org/gardenio --merged -P 50
```

For each, check whether the MR description references an e2e test file
or whether `git log --since=7days -- gardenio/tests/e2e/` includes a
matching test. Surfaces with no e2e coverage get a single tracking task
filed (idempotent: check for existing `coverage-backfill` label first).
The gardener then spawns ONE `crew-e2e` agent across the whole day's
coverage-backfill list — that crew picks the highest-priority gap,
drafts a test plan, and writes the test if scope is small.

### 4. Reliability epic progress (auto-spawn, daily)

For each of the two reliability epics:

- **ka-0000003762** (deploy↔E2E mutex stopgap, P1)
- **ka-0000004402** (per-MR ephemeral namespaces, P2)

If the epic has open child tasks with `status=open` and no assignee,
spawn a `crew-e2e` agent on the highest-priority unblocked child. If
the epic has no open children, spawn a crew on the epic itself with the
prompt "draft the next research/design child and file it; do not
implement until human approves the umbrella plan."

Cap: one crew per epic per day. If yesterday's crew is still running,
do not spawn a new one for that epic.

### 5. Anti-flake data (retired)

There is no flake-report source today. The per-module table
(`e2e-flake-report.md`, last stamped 2026-07-29) came from the RWX Captain
flaky-tests API through `cmd/e2e-flake-report`; RWX was deleted on 2026-09-16
and the tool is gone, so the report is archived at
[`docs/archive/rwx/docs/gardenio-docs/e2e-flake-report.md`](../../docs/archive/rwx/docs/gardenio-docs/e2e-flake-report.md).
Until a GitHub Actions JUnit source replaces it (the `shell-hermetic` job in
`.github/workflows/e2e.yml` uploads `shell-hermetic-junit`), file flake bugs
from the run history you can read, not from this table.

### 6. P1 e2e bug nudge

For each open P1 bug labeled `e2e` or with e2e/gardenio-e2e in the title
that has been open >24h with no assignee, spawn a `crew-e2e` agent to
investigate root cause and propose a fix MR. Cap one crew per bug.

### 7. Decision filing — ONLY for genuine forks

The gardener files a `gb decision` only when its analysis has surfaced a
real architectural choice that needs operator judgment, e.g.:

- Two viable approaches to a reliability problem (mutex via ConfigMap
  vs. annotation; ephemeral-ns implemented via Argo vs. controller
  reconciler).
- Coverage-vs-velocity tradeoff (back-fill 30 missing modules or pause
  at 5 and ship the deploy-mutex stopgap first).
- A flake-rate report has identified a load-bearing module at <50% pass
  rate (quarantine vs. fix vs. rewrite).

The gardener does **NOT** file a "what should I work on today" decision.
That is the gardener's job to figure out via steps 2–6. Cycle-wrap
"ack-only" decisions are explicitly forbidden (see `gb advice show
ka-0000000345` smell test).

When a fork exists, file:

```sh
gb decision create --no-wait \
  --prompt='<concrete fork — what was tried, what is blocked, what is recommended>' \
  --options='[
    {"id":"<id>","short":"<short>","label":"<concrete next action>","artifact_type":"<plan|epic|diff-summary|report>"}
  ]'
```

Do **not** call `gb yield`. The gardener runs daily and does not block.
Tomorrow's run picks up resolved decisions in step 8.

### 8. Resolved-decision follow-up

Before stopping, check whether any prior gardener-filed decision was
resolved overnight. If so:

- Read the chosen option's `artifact_type`.
- Spawn ONE `crew-e2e` agent on a freshly filed task that carries the
  artifact expectation. Mention the resolved decision id in the task
  description.

### 9. Ledger update + squawk + stop

- Append a one-line entry to **Recent cycles** in this file (oldest
  trimmed at 30 lines).
- Comment a fuller summary on parent task **ka-0000004587**.
- `gb squawk` a one-line summary that names what was dispatched.
- `gb stop` (NEVER `gb gate mark decision`).

The squawk shape is deliberate: it tells the operator what the gardener
**did**, not what it considered doing. Example:

> 2026-05-04: 87 mod / 90 scripts / 0 quar / 7 of last 8 main green.
> Spawned crew on ka-0000003762 child (mutex-acq); coverage-gap crew on
> bridge T7b/T7c; flake-report regen scheduled. No decision filed.

---

## Crew budget and concurrency

- **Max 5 crew agents in flight from this gardener at once.** Check
  `gb crew` before spawning.
- One crew per workstream per day. Workstreams: `drift`, `coverage`,
  `epic-RD1Ji2SzRe`, `epic-Ws4SgH3CGC`, `flake-report`, `p1-bug-nudge`.
- If a prior crew is `in_progress` on a workstream, do not spawn a
  second on the same workstream.
- All crews use `gb spawn --task <id> --role crew-e2e`.
- Crews follow the standard branch + MR + label + close workflow.

## Hard rules

- Direction / orchestration gardener, NOT triage / fix gardener. Failed
  pipelines belong to ka-0000001155.
- Do not edit `.captain/quarantines/gardenio-e2e.yml` (operator-owned).
- Do not dispatch RWX runs.
- Do not file daily check-in decisions. Decisions are for forks only.
- Do not push directly — spawn crews who push via standard MR flow.
- Respect `gb advice show ka-0000007569` (operator-action authority for
  gardener-e2e), `ka-0000004631` (stuck -rc), `ka-0000006307` (HTTP addr
  must target -rwx/-e2e/localhost), `ka-0000003449` (gardenio-rwx is the
  canonical E2E env).

## North star

Every workday morning, the operator opens Slack to one of:

1. A one-line squawk: "spawned 3 crews on coverage / mutex epic / flake
   report; no decision needed." → operator skims, moves on.
2. A one-line squawk + a `gb decision` link with 2–4 real options.
   → operator clicks the option that matches their judgment.

Either way, the suite is moving forward on its own. The operator never
has to ask the gardener "did anything happen yesterday?" — the answer is
already in this doc's Recent cycles section and in the parent task's
ledger.

---

## Recent cycles

(Gardener appends one line per run. Oldest pruned at ~30 entries.
Per-cycle detail lives on **ka-0000004587** as comments and on the
schedule bead **ka-0000006692** as comments.)

- 2026-08-06 (cycle 70, gardener ka-0000004729): **200 modules / 215 scripts
  = 198 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`
  exit 0, PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines; 10/10 recent main
  pipelines green. Epic issue-kind: 3 in_progress, **all verified LIVE** (each
  appears in `gb news --active-only`, all updated within the last 50 min) —
  0 stale to reap. Backlog had no unclaimed child, so this cycle's leverage
  was the **coverage-gap pass**: of 19 merges in the last 8h touching runtime
  code, 12 shipped **no** co-committed e2e. Filed + dispatched 2:
  **ka-0000001124 (P2)** — `internal/contextwindow` (0901b9fa9) centralizes the
  occupancy denominator (`OccupancyPct`, `AutoCompactFraction = 0.92`,
  per-model 200k/1M window) and **no e2e asserts any percentage**;
  `grep -rn OccupancyPct gardenio/tests/` → 0 hits. Worse, the nearest module's
  own comment is already stale at HEAD: `test-agent-cost-context.sh:312` says
  `128000 / 200000 = 64%`, but the denominator is now the auto-compact
  threshold — 128000/184000 = **69%**. It asserts neither number, so nothing
  caught the drift. Acceptance demands a **long-context (1M) leg**, because a
  hardcoded-200k regression passes the default leg and fails only there.
  **ka-0000003422 (P2)** — 8edcbe2e1 added
  `gardenio_slackbridge_respawn_phase_seconds` / `_slow_total` plus the
  `SlackBridgeRespawnPhaseSlow` vmalert rule; `grep -rn respawn_phase
  gardenio/tests/` → **0 hits**. Reachable today with no new plumbing: the
  bridge serves `/metrics` off the same mux as the escalation API
  (`helpers.go:341`), which run-shard.sh already port-forwards
  (`run-shard.sh:583`, `BRIDGE_HTTP_URL`). Guards the exact regression
  `buildMetricsServer`'s own comment records — a bespoke registry that
  exposed none of the bridge series.
  **Guardrail earned its keep:** `/claude-config` (c6f510207, 16 files) looked
  like a textbook gap and is **already covered** by
  `beads-viewer-e2e/tests/claude_config.spec.js` — grepping only the shell
  suite would have filed it. Per advice ka-0000000850, always grep BOTH suites.
  Also skipped e226e89c4 (recycle-keyless-gained-key): it is in-scope for live
  child ka-0000001464, and filing it would have put two crews in one file.
  **!6699 leak trap still closed** — 0 parent-child edges on both new agent
  beads and on my session bead. 5 crews in flight = budget. Decision = none.
- 2026-08-06 (cycle 69, gardener ka-0000000155): **200 modules / 215 scripts
  = 198 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`
  exit 0, PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines; 10/10 recent main
  pipelines green. Epic went **1 open / 1 in_progress → 0 open / 3 in_progress**.
  **Coverage-gap pass filed + dispatched ka-0000001464 (P1):** the
  `accountRotationRestarter` family has FOUR reconcile sweeps, three wired live
  in `main.go:1545/1552/1565`, and **only one has any e2e**. I found it by
  mapping every `a.logger.Info` decision string in
  `cmd/controller/account_rotation_restart.go` against BOTH suites rather than
  grepping symbol names — the two markers the covered sibling asserts
  (`test-credential-rotation.sh:663-664`) are the *only* two of six that appear
  anywhere. `ReconcileKeylessGainedKey` shipped **today** (4c8992813) with 7 new
  unit tests and zero e2e; all 22 unit tests drive a fake pod manager, so
  nothing exercises account-bead → cache-refresh → real baked
  `ANTHROPIC_CUSTOM_HEADERS` → real delete. **Method note worth keeping:** a
  bare `grep -rl stranded` returns three files and looks like coverage — none is
  this sweep (gVisor thaw, stale-reaper). Marker-mapping is the check that
  survives; name-matching is the one that lies.
  **Second live confirmation of advice ka-0000004373** (rate-limit livelock,
  first confirmed cycle 68): crew `…claudemore-replay-fx0` on ka-0000006222 has
  now burned **101 minutes** Running 1/1, `agent_state=working`, listed healthy
  by `gb news --active-only`, doing nothing but 429 → backoff → re-inject (25
  rate-limit lines in 30m). Account gardenio3 (ka-0000001761) is
  `status=exhausted` / `org_spend_cap_reached`, self-resetting 15:50Z. I did
  **not** file a classifier bug: `overageReasonBlocks` (`podspec.go:2607`)
  treats `org_spend_cap_reached` as non-blocking **deliberately** — that is the
  operator-corrected contract (ka-0000000998), so leaving those pods to recover
  in place is correct behavior, not a defect. I re-bound the bead to its live
  crew (`in_progress` + assignee) so the next gardener cannot double-dispatch it
  the way cycle 68 nearly did. New crew was placed on `shannon`, not gardenio3.
  Run-budget question from cycle 40 is settled: decision **ka-0000002283** is
  closed `dismissed`, `run_budget: 78` vs `runs_spawned: 69` — the lane keeps
  running, so I did not re-file it.
- 2026-08-06 (cycle 68, gardener ka-0000006470): **200 modules / 215 scripts
  = 198 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`
  exit 0, PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines; 10/10 recent main
  pipelines green. Epic **1 open / 1 in_progress**, 2 crews live. Cycle 67's
  gap **ka-0000003773** landed: !6930 merged (367932f2a) — `verify-prod-images`
  is now a shard-0 hermetic module. I verified it before merge rather than on
  sight: test-merged main into the branch (it was 2 commits behind),
  re-ran `test-shard-isolation.sh` on the MERGED tree (exit 0, 200 modules),
  and **executed the module locally — 5/5 assertions, exit 0, no SKIP**. That
  last step is the one that matters: `grep`ping for `E2E_SKIP_SENTINEL` tells
  you a module *can* skip, only running it tells you it *didn't*. Its crew
  self-merged mid-verification, which is fine — the verification was still
  worth doing, and would have caught a vacuous module before it became the
  next gardener's problem. **Rescued a livelocked crew** (advice ka-0000004373,
  first live confirmation): `gb news --stale-only` flagged ka-0000006222 (P1),
  but the pod was **alive at 74m** — idle 68 of them on an *account monthly
  spend-limit* 429 (`claude-opus-4-8-oauth`, fallback `claude-opus-5-oauth`
  also 429), recovered 14:40Z. Two lessons: (1) bead-staleness and pod-liveness
  disagree under rate-limit, so check BOTH before reaping — I had already
  cleared the assignee when I found the pod; (2) `gb spawn` on a task a live
  agent holds returns the EXISTING agent and **silently discards your prompt**
  (`reason=single_claim`), so the re-verified gap + anti-vacuity mandate went
  via `gb mail` instead. Fleet-wide the 429 hit 2 crew pods + litellm (800
  log lines) — not an e2e problem, noted for whoever owns spend. **Filed +
  dispatched one new gap: ka-0000007410** — `gb hook prime`'s `## Assignment`
  elision (`hook_assignment.go`, `assignmentFieldMaxBytes=512`) has no
  assertion anywhere; its 30 unit tests stub `runKD`, so they never see real
  `kd show` TEXT — and the renderer parses TEXT on purpose (for `Depends On` /
  `Children` / `Mentions`, absent from `--json`). Proved live from this pod:
  `gb hook prime` emits `prompt: … [6.3KB elided — full text: kd show …]`.
  Regression is silent and costs tokens on every fleet-wide SessionStart.
  **I then walked into cycle 67's own warning and am recording it loudly**:
  my first sweep globbed only `e2e/scripts` + `beads-viewer-e2e`, and
  `e2e/claudeless/local-prime-fetch.toml:71` *does* run `gb hook prime` (wired
  at `test-local-mode.sh:930`). The gap survives — that scenario asserts exit 0,
  `<system-reminder>`, actor name, check-mail, and the context pointer; raise
  the ceiling to infinity or drop it to 1 and **all five still pass** — but the
  bead was wrong when I filed it, so I posted a correction comment before the
  crew read it. **The five-tree grep is not optional and knowing about it is
  not enough** (`e2e/scripts`, `e2e/claudeless`, `beads-viewer-e2e`,
  `beads-viewer-screenshots`, `slack-e2e`, `slackmock`). Also **rejected one
  candidate**: `gb viewer-login` — `beadsViewer.k8sSA.enabled` is `false` and
  set nowhere, so a module would always-SKIP; this independently reconfirms
  cycle 67's rejection of the same surface (ka-0000007798).
- 2026-08-06 (cycle 67, gardener ka-0000000006): **198 modules / 213 scripts
  = 196 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`
  exit 0); 0 quarantines. Epic **172 closed / 4 in_progress / 0 open** — all 4
  crews verified LIVE (heartbeat 14:11), none stale, so no rescues and no
  re-dispatch. **Fixed a terminal-goal blocker**: cycle 66 parent-child'd
  advice bead `ka-0000007273` (`kind=data`) into this goal-bearing epic —
  advice never closes, so "all children closed" was unreachable by
  construction (advice ka-0000002123). Dropped the parent-child edge, kept
  `related`. Coverage-gap pass over 451 commits/7d filed **ONE** new gap and
  dispatched it: **ka-0000003773** — `gb verify-prod-images` (the third line
  of defense against a `-cover` image reaching prod, wired into the release
  runbook at release-pipeline.md:307) has zero e2e AND zero unit coverage of
  its orchestrator; its 10 unit tests all hit pure helpers, leaving the
  exit-code + stable-JSON contract unexercised — a regression fails silently
  in the false-PASS direction. **A second candidate (`gb viewer-login`) was
  REJECTED, not filed**: it is the already-retired false-premise gap
  ka-0000007798 (squawk ka-0000005848, 12h earlier) — the k8sSA branch is
  dark everywhere e2e runs, so a module would always-SKIP and be vacuous.
  Non-vacuity was therefore checked explicitly for ka-0000003773 before
  filing: `--skip-cluster --values <fixture>` is a pure file scan with no
  flag and no dark path. **Two sweep bugs worth inheriting** (they generated
  5 false gaps before correction): (1) `grep -rE 'a\|b'` — `\|` is a LITERAL
  in ERE, so every alternation query silently matches nothing; (2) globbing
  only `tests/e2e/scripts/` misses the other five trees — `claudeless/` (49
  .toml) holds gardener-epic-reaper and `execution_target=probe` coverage.
- 2026-08-06 (cycle 66, gardener ka-0000005764): **198 modules / 213 scripts
  = 196 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`
  exit 0, PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines; 0 open P0/P1 e2e
  bugs at cycle start. Backlog opened at **2 open / 2 in_progress**; both open
  children HEAD-verified real and dispatched (ka-0000006517 perf-span charts,
  ka-0000000405 MultiTriggerSource) → **backlog fully dispatched, 5 crews in
  flight (budget cap)**. **A release broke, and it exposed a real e2e gap.**
  Tag `2026.218.26` is the first non-green release since .22: `rwx/release=failed`
  with `deploy-ephemeral.deploy-rwx` dead and **E2E never running** — so the tag
  produced no e2e signal at all. Root-caused it to a realm-name mismatch in
  `deploy-values/gardenio-rwx-ephemeral-prod.yaml.tmpl`: the overlay *creates*
  realm `${RELEASE_NAME}` (:928/:940) but three consumers hardcode literal
  `gardenio` (:758, :1293, and the twin pair in the poc overlay), so both
  forwardauth pods CrashLoopBackOff on `404 {"error":"Realm does not exist"}`.
  `git log -L` pins it to `c780f7506` (!6888) — which swapped the *host* on
  those lines and left the *realm path segment* literal. Root cause handed to
  the release lane on **ka-0000000829** (flagged, not claimed — deploy-stage
  failures are not this gardener's lane). **The e2e-side lesson is the durable
  part**: no test in either suite could have caught this, and none ever will,
  because the failure happens *before any module runs*. `grep -rn "realms/"
  gardenio/tests/` hits only the two keycloak libs, which *consume* a realm at
  runtime and assert nothing about overlay self-consistency; the only existing
  deploy-values lint (`check-deploy-values-dup-keys.sh`, `.rwx/ci.yml:2162`)
  checks duplicate YAML keys only. Filed **ka-0000007361 (P1)** for a
  cluster-free static guard — created-realm vs every `realms/<name>` issuer
  segment, compared *post*-`${RELEASE_NAME}`-expansion — and dispatched it.
  **Generalization for successors:** a coverage gap is not always a missing
  module. When the thing that broke runs upstream of the suite, the correct
  backfill is a *static lint wired into CI*, and writing an e2e module for it
  would be a test that structurally cannot fire. Its anti-vacuity bar is stated
  on the bead: the guard must exit non-zero on `c780f7506` itself, or it is
  worthless.

- 2026-08-06 (cycle 65, gardener ka-0000003086): **197 modules / 212 scripts
  = 195 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`
  exit 0, PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines; **0 stale
  in_progress** (`gb news --stale-only` empty); 0 open P0/P1 e2e bugs; 0 open
  bugs matching e2e/flake across project:gardenio. Backlog opened at **0 open /
  4 in_progress issue-kind children**, all four with live crews — so no
  dispatch slot was needed and no crew was spawned. **ka-0000005155 closed
  mid-cycle** when its crew self-merged !6911 (helm honest-empty stat exprs +
  `http_status_code` unit) with all 20 RWX statuses green.
  **Throughput note:** +32 modules in 24h (165 → 197) across 64 merged
  `test/*` MRs — the sprint is converting gaps to coverage faster than the
  coverage-gap pass files them, which is why the backlog keeps hitting zero.
  **Corrected a 26-cycle-stale gardener prompt.** The trigger (ka-0000005355)
  carried "coop 55.3% below floor; weakest crate `mux` 42.4% —
  credential/throttle.rs, upstream/poller.rs, lib.rs all 0%" and called coop
  "the one that matters for a *required* Coverage/Patch gate". All three named
  files have dedicated test modules at HEAD (`throttle_tests.rs`,
  `poller_tests.rs`, `lib_tests.rs`), and the Coverage/Patch gate is
  **baseline-driven** (`.rwx/coverage.yml` fetches `baselines.json`; there is
  no hardcoded floor), reading `coverage=success` on tags .23/.24/.25. Cycle 39
  flagged this staleness and only *recorded* it; this cycle **rewrote the
  trigger prompt field** so the next gardener cannot dispatch a duplicate-work
  coop crew off it. Generalization: a stale premise noted in a ledger comment
  keeps firing — fix it at the source bead, or every successor re-derives it.
  **Also re-confirmed**: main-push pipelines still carry zero E2E jobs, so the
  e2e signal read this cycle was the `rwx/release` commit status on release
  tags (not `glab ci list -r main`).
- 2026-08-06 (cycle 64, gardener ka-0000004858): **197 modules / 212 scripts
  = 195 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`
  exit 0, PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines; **0 stale
  in_progress** (`gb news --stale-only` empty — all 3 entry crews genuinely
  live); 10/10 recent main pipelines green; 0 open P0/P1 e2e bugs. Backlog
  opened at **0 open / 3 in_progress**, so no dispatch slot was needed and
  this cycle's leverage was research + hygiene rather than fan-out.
  **Merged !6908 mid-cycle** (ka-0000003835, LiteLLM `router_settings.fallbacks`
  opus-4-8 arm asserted against the DEPLOYED stack) — the crew self-merged AND
  closed its own child, the counter-example to the strand mode that recurred in
  cycles 15/16/42. **GitLab mechanic worth carrying forward: `detailed_merge_status`
  sat at `checking` for ~100s while every external commit status on the head SHA
  was already `success`.** `checking` is a stale mergeability cache, not a gate —
  a gardener that reads it as "blocked" will either wait forever or dispatch a
  redundant rescue crew. Read the commit statuses on the head SHA instead.
  **Coverage-gap pass: 10 runtime merges audited, 9 ALREADY-COVERED, 1 filed.**
  The striking result is the 9: reopen-debounce, FSM backlog picker,
  `--with-blockers`, `/claude-config`, `/up` keymap, trigger account-cost/reaction
  schema, `gb spawn` token, `defaultAccount`, claudemore replay all had a
  `test(...)` follow-up land **within hours** of the feature commit — the backfill
  machine is now keeping pace with feature velocity, which is the sprint's actual
  goal. Filed **ka-0000005155** (P3): `554e25b4b` shipped 5 `or vector(0)`
  honest-empty stat exprs + an `http_status_code` `unit:none` override with
  **zero** guarding assertions (`grep -c` on both target test files = 0).
  **The routing lesson on that one: it belongs in the Go helm tests, not either
  e2e suite.** `grafana.enabled=false` in every ephemeral namespace
  (`run-shard.sh:1721`), so a shell fixture for a Grafana panel is *unreachable*,
  not merely missing — the same "unreachable fixture ≠ coverage gap" distinction
  cycle 36 drew for `kd create --type=schedule`. Precedent to copy already exists
  at `grafana_formula_step_dispatch_dashboard_test.go:106`.
  **Ledger hygiene: pruned 136 lines.** This file had grown to **74 entries /
  3054 lines** against its own documented ~30-entry cap, and every gardener reads
  it first every cycle — the overflow is a straight context tax on all successors.
  Removed the 11 May-2026 entries from the *original* sprint (they describe an
  87-module suite vs 197 today), leaving a tombstone. Now 63 entries / 2918 lines.
  **Also noted, not fixed: the entry list is no longer date-sorted** (cycle 53
  sits after cycle 29), so "the last entry" is not reliably the newest — a
  successor computing deltas should grep for the highest cycle number, not `tail`.
  Mission-brief coverage-floor item is **resolved**: all three formerly-0% coop
  `mux` modules now have tests on main (`throttle_tests.rs`, `poller_tests.rs`,
  `lib.rs` 2 test blocks).
  **The cycle's most important finding was about this role's own instrumentation:
  "N/10 recent main pipelines green" — quoted by 60+ cycles, including my own
  entry snapshot above — is not an E2E signal at all.** Main-push pipelines
  contain zero E2E jobs; there are no scheduled pipelines; tag runs are the only
  E2E gate. In this very window main read 10/10 green while **13 of 16 release
  runs were RED**. Fixed at the snapshot step in !6912, with the reachability
  recipe (`rwx/release` status → RWX run id → `release-gate` task log). The
  general lesson: a health metric that *cannot go red for the reason you care
  about* is worse than no metric — it manufactures confidence. Cross-check any
  green you report against the gate that would actually fail.
  **No flake bead filed, and that is the correct outcome:** all 13 reds were
  deterministic, git-traceable regressions already fixed (`claudemore-hooks` →
  `33f601632`; `trigger-changed-to-seam` was born-red at `b508edab9` → fixed by
  `de0af7f0e`; `trigger-system` was test-vs-product lag after `9fba741f0` made
  the daemon *accept* a write the test still expected to be rejected → `ab23c9d8b`).
  Zero retry-flake signals: the in-shard connectivity retry never fired, and the
  gardener targeted-retry fired 7× and reproduced the identical assertion every
  time — a retry that never converts fail→pass is itself evidence of a real break.
  One **watch-only** item deliberately left unfiled: `agent-state-enum` failed in
  exactly 1 of 16 runs (`.16`) with an empty diff across `.16..25`, so unlike the
  others it has no git explanation; 1/16 does not meet the intermittency bar, and
  quarantining a module that guards a real silent-drop invariant would cost more
  than it saves. Hypothesis on the bead if it recurs.
  **Also filed ka-0000002712 (P2) — a real bug, not a coverage gap:** `fef109705`
  claims dashboard UID `gardenio-perf-traces`, which the chart-shipped
  `helm/gardenio/dashboards/perf-traces.json` already owns, so `gen-dashboards`
  and `GrafanaSync` now fight over one UID — and two bridge tests use that exact
  UID as their canonical *chart-owned* example while the catalog doc comment
  (`charts_grafana_dashboard.go:124-125`) asserts no catalog UID is chart-owned.
  Both sides assert their own view in isolation, which is exactly why no existing
  test catches it. Decision = none — no genuine architectural fork.
- 2026-08-06 (cycle 63, gardener ka-0000000111): **196 modules / 211 scripts
  = 194 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`
  exit 0, PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines; 0 stale in_progress;
  8/8 recent main pipelines green. Backlog opened at **0 open / 2 in_progress**
  (both cycle-62 crews live). Structural pass clean — all 3 non-closed children
  are `type=task`, no advice-bead-in-epic trap (4th cycle running the check).
  **Coverage-gap pass filed 1 new child**, symbol-grepped at HEAD `786b708ba`
  across BOTH suites before filing: **ka-0000002850** (P1) —
  `BeadStateTriggerSource.EventReady` (`6dc37b0de`, the prod fix for bug
  ka-0000001376's silently-dropped bead-trigger fires) has **zero** e2e in
  either suite (`EventReady` / `effectiveCooldown` / `CooldownEventRace` all 0
  files). Crew `gardenio-e2e-beadsource-eventready-oyt` dispatched.
  **The transferable lesson this cycle: a fix whose own e2e routes AROUND it is
  worse than no e2e.** `test-trigger-action-fidelity.sh` is the module in this
  fix's blast radius, and its flake repair (`de0af7f0e`) added
  `--ignore-cooldown` — the `cooldownIgnored` **test seam** at `triggers.go:2085`,
  a sibling branch of the same `if` that is checked *before* the `EventReady`
  bypass at `:2093`. So the suite reads as covering the area while never
  executing the production path. A feature-name grep ("cooldown" → 8 files) says
  covered; the symbol grep says zero. Extend the rule from cycle 35 — grep the
  SYMBOL, then check whether the module that hits it takes a **test-seam
  shortcut past the code under test**. Unit coverage exists
  (`TestBeadStateTriggerSource_CooldownEventRace`) but hand-stamps `LastChecked`
  over a mock daemon, and the bug was a *cross-evaluator* race against a live
  30s-poll engine on a shared daemon — the one condition in-process tests
  cannot model, and the one that hard-failed release 2026.218.14 on shard 3.
- 2026-08-06 (cycle 62, gardener ka-0000000328): **196 modules / 211 scripts
  = 194 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`
  exit 0, PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines; 0 stale in_progress
  (`gb news --stale-only` empty; ka-0000002910 confirmed live). Backlog opened
  at **0 open issue-children** — cycle 61 drained it.
  **Cleared a terminal-goal blocker**: advice bead ka-0000004373 (`type=advice`
  / `kind=data`) had been parented into the epic at 10:48 by the prior gardener,
  which makes "all children closed" unreachable by construction — an advice bead
  never closes (advice ka-0000002123; same trap as cycle 14, third recurrence).
  Removed with `kd dep remove … --type=parent-child`. **Note for next gardener:
  `kd dep remove` defaults to `--type=blocks` and errors misleadingly ("no such
  dependency") on a parent-child edge — pass the type explicitly.**
  **Coverage-gap pass filed 2 new children**, both re-verified per-term at HEAD
  (`01e5c1a93`) by me across BOTH suites before filing:
  (1) **ka-0000003835** (P1) LiteLLM `router_settings.fallbacks` opus-4-8→opus-5
  arm (9843c4a0f) — `router_settings`/`claude-opus-5-oauth`/`No fallback model
  group` all 0 files in shell AND Playwright. The existing
  `litellm_router_alias_test.go` is a chart-render test over *default* values,
  so a `deploy-values` override that drops the arm renders green. Ranked P1 on
  *observed* blast radius, not theory: this is the exact shape of open bug
  ka-0000002264 (merge lane dark 4h20m) and ka-0000007458 — a missing arm makes
  an exhausted key spin-loop on 429 while the pod stays Running and
  probe-green, invisible to `gb news --active-only`.
  (2) **ka-0000004378** (P2) Slack card prune starvation guard (ab7f62aa7) —
  `pruneTokenFloor`/`waitForPruneToken`/`slackCardLimiter` 0 files in both
  trees, unit-tested only. Scoped at the **fail-open**: `floor := min(1.0,
  Burst()-1)` degrades to a bare `Allow()` if burst drops to 1, so the guard
  silently becomes a no-op with nothing red.
  Both briefs carry the two-suite rule, a mandatory mutation-proof requirement
  (advice ka-0000001521), and an explicit "close with evidence instead of
  writing a duplicate if already covered". Spawned 2 crew; 3 in flight.
  **Rejections recorded** (would each have burned a crew slot): `/claude-config`
  → `claude_config.spec.js`; `/up` keymap → `up_keymap.spec.js`; cron fallback →
  `cadence_fallback.spec.js`; `DEFAULT_ACCOUNT` → `test-airgap-image-
  distribution.sh` legs 8+9; trigger `threshold` → `test-trigger-system.sh`
  L480–579; escalation verb → `test-escalation-crud.sh` 5c; reopen-debounce →
  covered at Go level by `bot_reopen_debounce_e2e_test.go`;
  `planEnrichJoinConcurrency` → commit itself states no behavioral change.

- 2026-08-06 (cycle 61, gardener ka-0000005634): **194 modules / 209 scripts
  = 192 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`,
  PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines; **8-of-8** recent main
  pipelines green; 0 stale in_progress to reap (both live e2e bugs
  ka-0000007982/ka-0000002910 confirmed attended via `gb news --active-only`).
  **Backlog fully drained**: re-verified all 3 open children per-term at HEAD
  myself before dispatch — all real (runtime present, 0 hits across the shell +
  Playwright + chart-Go suites) — and spawned 3 crew, one each on
  ka-0000003966 (P1, `threadTaskHintInsteadOfClose` helm→env seam),
  ka-0000002610 (P2, `kd list --with-blockers` `open_blockers[]` projection),
  and ka-0000006873 (P2, keycloak realm `frontendUrl` conditional render).
  Budget then full at 5.
  **Coverage-gap pass filed ZERO new children — all 3 candidates REJECTED as
  already-covered.** Recording the negative result so the next gardener does not
  re-derive it: (1) `/claude-config` role×knob matrix (3c9021bbc) — 79 raw
  grep hits looked like noise, but `tests/beads-viewer-e2e/tests/
  claude_config.spec.js` is a purpose-built behavioral module for exactly this
  route; the *shell*-suite-only grep that would have missed it is the trap
  advice ka-0000000850 names. (2) backlog-mode FSM ticket picker (3b191ac8b) —
  `test-jira-backlog-pick.sh` (552 lines) already drives the real `gb jira
  backlog-pick`. (3) account-cost + slack-reaction trigger source fields
  (9fba741f0) — `test-trigger-system.sh:545` cites commit `9fba741f0` by SHA
  and asserts all three match fields round-trip. Each would have burned a crew
  slot writing a duplicate.
  **Methodology note — a low grep count is evidence, a HIGH one is not.**
  The three REAL gaps all returned literal 0 across every suite. The three
  rejects returned 79 / 2 / 46. The count alone never decided anything: the
  0-hit cases were confirmed by checking the runtime still exists at HEAD, and
  the high-hit cases were resolved only by opening the files. Candidate (2)
  also shows why the `run_module` grep is not a coverage index — its module is
  wired at run-shard.sh:1761 behind a `should_run` shard pin, so a naive
  module-name grep can miss a module that demonstrably runs.

- 2026-08-06 (cycle 60, gardener ka-0000004791): **194 modules / 209 scripts
  = 192 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`,
  PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines; **10-of-10** recent main
  pipelines green. Reaped 1 stale child (ka-0000002610 — crew `agent_state:
  idle`, no branch, no MR; returned to the queue with the assignee CLEARED, as
  a status-only reset strands a bead). Re-verified both open children per-term
  at HEAD before dispatch — both real (runtime present, 0 hits across the
  shell + Playwright + chart-Go suites) — and spawned 1 crew on the P1
  ka-0000001890 (`agents.defaultAccount` -> `DEFAULT_ACCOUNT`); budget then
  full at 5. Coverage-gap pass over 25 merges filed **ka-0000003966 (P1)**.
  **Finding — "well-tested" can mean every test sits DOWNSTREAM of the seam.**
  `cecaaaf43` flipped `threadTaskHintInsteadOfClose: true` in staging and
  shipped a 9-scenario, 464-line acceptance suite, so the flag reads as
  thoroughly covered. It is — *below* the helm->env boundary. All 4 grep hits
  fail per-term: 3 are prose comments (the idle-hint module's own header even
  records "returned ZERO hits" as an authoring note), and the strongest-looking
  hit, `thread_task_autoclose_livedaemon_e2e_test.go:137-145`, sets the Go
  field `HintInsteadOfClose: true` *literally* while its comment says it is
  "the configuration the bridge wires when SLACK_THREAD_TASK_HINT_INSTEAD_OF_
  CLOSE=true" — it ASSUMES the seam it appears to prove. Delete the whole
  `{{- if }}` block and all 4 stay green. Ranked P1 not for the flag but for
  the evidence: that values file makes the staging soak the gate on the prod
  flip, `os.Getenv` of a missing var is a legal "off", so a broken seam makes
  the soak measure the *un-flipped* world and report orphan-threads FLAT — the
  exact success criterion — fabricating the green that authorizes prod. **The
  generalization: for a boolean feature flag, count only assertions that cross
  the delivery boundary (chart render / real env), and treat a test that sets
  the downstream struct field as evidence AGAINST seam coverage, not for it.**
  Same shape as ka-0000001890, already in flight — two independent instances in
  one cycle suggests the helm->env seam is a systematic blind spot. Decision =
  none; no genuine architectural fork.
- 2026-08-06 (cycle 59, gardener ka-0000005693): **194 modules / 209 scripts
  = 192 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`,
  PASS at TOTAL_SHARDS=4/5/8/16, re-run after the merge below); 0 quarantines;
  **9-of-9** completed main pipelines green. Landed cycle 58's P1: **!6887
  merged**, ka-0000005441 closed — the top-level green-by-skip hole is shut and
  the three Slack modules now record SKIPPED, not PASSED. 10 epic children
  closed today. Dispatched 2 crews (ka-0000007443 reopen-debounce, ka-0000007982
  below); backlog ended the cycle with **zero unassigned children** — all 4 open
  have live crews.
  **Found: the flake report was 8 days stale and materially misleading.**
  `gardenio/docs/e2e-flake-report.md` is stamped 2026-07-29 and still names
  `decisions-yield` (14/7d) as the standing worst offender. Regenerating it
  (`go run ./gardenio/controller/cmd/e2e-flake-report`) shows that picture is
  **entirely obsolete**: decisions-yield is fixed and no longer flagged at all
  (ka-0000008032 + ka-0000000164, closed), and the flagged set moved from
  **3-of-26 to 8-of-31** — every current offender is absent from the checked-in
  file. A gardener trusting the committed report would have chased a solved bug
  and missed all eight live ones.
  New worst offender: **`trigger-action-fidelity`, 11 flakes/7d** — and it is
  *also* a hard-fail on release 2026.218.14 (ka-0000001376, 174/179, RWX run
  `9c8de5722dd741c99f2a85ad586575a3`). Two independent signals agreeing is the
  definition of load-bearing, so it was filed P2 as **ka-0000007982** with 1
  crew. Remaining flagged, recorded here so next cycle need not re-derive them:
  news-alive-freshness 6, cred-rotation-resume 6, agent-state-enum 4,
  pool-enable-disable-rmw 2, credential-rotation 2, pool-manager 1,
  coop-live-usage 1.
  **Dedup that mattered:** the only beads naming "flaky" (ka-0000003689,
  ka-0000005698) are generic buckets last touched **2026-03-02** — 5 months
  stale and pre-dating this module. Folding a specific named-module flake into
  either would have buried it; they are not dedup coverage.
  **Lesson for the next gardener:** step 5 says *regenerate* the flake report,
  not *read* it. Reading the committed file is a name-match trap in report form
  — it looks like current data and is not. The regen is one command and cheap;
  do it before believing any flake claim, including this entry's.
  Decision=none.

- 2026-08-06 (cycle 58, gardener ka-0000001800): **194 modules / 209 scripts
  = 192 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`,
  PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines; **12-of-12** recent main
  pipelines green; 3 live crews on arrival, 1 stale bead (not this epic's).
  **Found: green-by-skip is back — in a shape the lint that killed it cannot
  see (ka-0000005441, P1, 1 crew).**
  `slack-cards`, `slack-dashboard`, `slack-threads` are all `run_module`-wired
  and all exit 0 having asserted **nothing** on every `.rwx/e2e.yml` shard run
  — recorded as MODULE **PASSED**, counting toward the tally the release gate
  rests on. Proven by running each with `SLACK_BOT_TOKEN`/`SLACK_CHANNEL`
  unset: `exit=0`, sentinel **not** written, which is exactly what run_module
  classifies as PASSED (`run-shard.sh:736-745`).
  The credentials are not merely missing — they are **structurally
  unreachable**: `grep -rln E2E_SLACK_BOT_TOKEN .rwx/` returns only
  `smoke-rwx.yml` + `release.yml`. `e2e.yml`, which dispatches the sharded
  suite where all three are wired, sets no Slack env at all; smoke-rwx does,
  but runs `SMOKE_ONLY=1` whose subset contains none of the three; release's
  use is the failure-ping, not a test run.
  **Lesson — a guard is only as wide as its scan.** ka-0000000989 closed this
  exact class and its lint is **19/19 green** — Part B only matches a clean
  `exit 0` inside a function named `skip*`, and these three guard at top level
  (`if [ -z "$SLACK_BOT_TOKEN" ]; then echo SKIP; exit 0; fi`). No function,
  no name match, invisible. So the bead's load-bearing half is **widening the
  lint**, not patching 3 files; the mutation test is the lint going RED at
  origin/main naming all three. Three near-misses (`stale-reaper-grace-
  reconcile`, `thaw-reaper-exemption`, `microvm-execution-target`) echo `SKIP`
  as a decision-table **verdict**, not a process exit — a lint that reds on
  those is over-broad, and the bead says so.
  Coverage-gap pass: 76-commit / 36h audit, every candidate grepped across
  **both** suites (ka-0000000850). Rejected as already-covered — recorded so
  no one re-derives them: `3562b73d7` min_duration -> test-gitlab-pipeline-
  minduration.sh; `74cd09f0e` pipeline_ref_pattern -> …-refpattern.sh;
  `544d9d0a1`/`e19d2b438` dispatch cooldown + aging nudge -> test-mr-dispatch-
  gate-e2e.sh; `568969352` agent_state enum -> test-agent-state-enum.sh;
  `bb8645d16` status_message -> user_status_message_auth.spec.js (Playwright);
  `7e55a87a8` formula-update `extends` -> test-formula-lifecycle.sh scenario 11
  (with negative control); `283942476` gb spawn signal -> test-gb-spawn-
  success-token.sh. Ledger MR **!6886**.

- 2026-08-06 (cycle 57, gardener ka-0000000738): **194 modules / 209 scripts
  = 192 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`,
  PASS at TOTAL_SHARDS=4/5/8/16, re-run AFTER both merges); 0 quarantines;
  18-of-20 recent main pipelines green; `gb news --stale-only` **empty** for
  the third cycle running.
  **Merged 2 orphaned MRs; caught a false-"fixed"; filed 2 gaps from a
  15-commit audit.**
  **Lesson — a red pipeline is not always a red diff.** !6880's only failure
  was `lint-mr-proof`, and the defect was in the MR **description**, not the
  code: `scripts/lint-mr-proof.sh` `field_value()` reads the value on the
  SAME LINE as `- **Evidence:**`, and the author had put the substance on
  nested sub-bullets — so a richly-evidenced MR parsed as "Evidence (no
  value)". Rewrote the field line in place (agent-authored MR, ka-0000007845),
  validated locally with `bash scripts/lint-mr-proof.sh <file>` before
  pushing, retried the job, merged. Worth knowing generally: **sub-bullets
  under a PROOF field are invisible to the linter.**
  Before editing the proof I re-verified the code rather than certifying
  evidence I had not checked — 15/15 green, and with the fix reverted to
  origin/main the new test FAILS with `REAL_FAILURE_SUMMARY_LINE not found`
  and the block filled by `teardown/next-shard preamble line 21..59`.
  Anti-vacuity confirmed by execution, not by reading.
  **The find — a fourth failure mode: FIXED-BY-ASSERTION.** ka-0000007771's
  claudemore-hooks scope was closed as "already fixed on main by 3c2e2a9d5,
  the next release absorbs it". It is not. `git merge-base --is-ancestor
  3c2e2a9d5 2026.218.13` and `... 2026.218.14` both return **YES**, and both
  tags still fail T5/T6 with `miss_count=1`. The module builds claudemore+gb
  **from source at the checked-out tag**, so the shard genuinely exercises
  the fixed code. This is the THIRD cycle where a green run at HEAD **on an
  agent pod** was mistaken for a shard fix (first `seed_claude_home`
  ka-0000000461, now the volatile-strip). An agent pod carries its own
  `~/.claude` and its own launch environment — it is not a shard, so a green
  run there is not evidence. Carved out to **ka-0000002910** (P1, crew
  dispatched) with a hard "reproduce at a TAG, not at HEAD" gate; closed the
  4-way umbrella so it stops reading as live.
  Narrowed the lead before dispatch: the module's `--setting-sources ""`
  determinism defense is present on **both** legs (record:294, replay:333),
  so this is not module asymmetry; and only the **multi-turn** scenarios
  (T5/T6) miss while single-shot ones pass — pointing at a volatile field on
  turn-2+ request bodies.
  **Coverage-gap pass: 15 runtime commits audited by 2 parallel read-only
  auditors, each required to produce proof-of-absence greps over BOTH suites
  plus an explicit inverse-trap check. Result: 13 COVERED, 2 GAP.** Filing
  only 2 of 15 is the point — grep-zero is where a coverage claim starts.
  - **ka-0000002610** (P2, crew dispatched): `kd list --with-blockers`
    (`da5fc505f`). Zero hits across all THREE suites. Unit coverage is broad
    but **shallow where it matters** — the store tests are `go-sqlmock`, so
    the SQL join is asserted as a *string* and **never executed**; sqlmock
    cannot catch a malformed join, wrong column order, or a project-scope
    leak. Failure mode is silent wrong data: an agent would read a gated bead
    as ready.
  - **ka-0000007443** (P3, deliberately NOT dispatched): reopen debounce
    (`4a4c77185`). Real gap, but 12 unit tests / 433 lines cover every
    entrypoint and the window boundary, and the failure is **loud** in prod
    (visibly competing pods), so it must not jump the queue ahead of
    silent-failure surfaces.
  **Inverse-trap rejections worth keeping:** `test-dependencies.sh` looks
  like `--with-blockers` coverage and is not (ready-queue membership, not the
  per-row `open_blockers[]` projection); `test-gvisor-duplicate-concurrent-spawn.sh`
  looks like debounce coverage and is not (`spawnInFlight[channel:threadTS]`
  is the pre-existing per-thread lock the commit explicitly did NOT touch —
  different guard, different key).
- 2026-08-06 (cycle 56, gardener ka-0000005061): **194 modules / 209 scripts
  = 192 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`,
  PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines; 11-of-12 recent main
  pipelines green (the 1 red is the CI/CD gardener's beat, not this lane);
  `gb news --stale-only` **empty** for the second cycle running.
  **Merged an orphaned MR, then filed + dispatched 1 new gap.**
  **Lesson — an idle agent with a green MR is finished, not stuck.**
  ka-0000002238's crew was `idle` with MR !6874 sitting `mergeable`,
  non-draft, pipeline `success`. Per ka-0000003385 that is the
  finished-but-unmerged shape, not a blocked one — nobody had pressed merge.
  Merged it, verified `b508edab9` on origin/main **and** that the module was
  really wired (`run-shard.sh:2038 run_module "trigger-changed-to-seam"`)
  before closing the child. Verifying the wiring is the step that matters:
  a merged test file that no shard ever invokes is coverage on paper only.
  **New gap filed — ka-0000000682** (P2, crew dispatched): `3c9021bbc`
  shipped `/claude-config` + `/claude-config/{role}`
  (`beads-viewer/server.go:1832-1833`) plus a ~1150-line
  `internal/claudeconfig/` package that `gb setup`/`config_resolve.go` were
  refactored onto (gb shed 649 lines). Both suites grepped per
  ka-0000000850: **zero** coverage.
  **The name-match trap, live:** `grep -ri claude.config gardenio/tests/`
  returns 10+ hits and every one is the **`claude_config` FIELD on a config
  bead** (`test-config-beads.sh:243` sets `-f claude_config=...`;
  `lib-config-cleanup.sh:48` closes stale config beads) — not one issues a
  request to the route. Same string, unrelated surface. I required an
  anti-vacuity positive control on the dispatch: a knob differing between
  two roles must render differently, else an empty grid passes every
  assertion.
  **Rejected 4 candidates with evidence** rather than filing on grep-zero:
  honest-empty stats (`554e25b4b` = Grafana dashboard JSON, no runtime
  surface), scoped-role (`d776c5757` = p44 cutover shell + tfvars, operator
  procedure), channel_roles (!6411 still **open** — not merged, so not yet a
  gap), and the claudemore REPLAY-hash change (already owned by live bug
  ka-0000007771, whose T5/T6 cassette-miss diagnosis is exactly that code).
  Grep-zero is where a coverage claim *starts*, not where it ends.
- 2026-08-06 (cycle 55, gardener ka-0000008061): **193 modules / 208 scripts
  = 191 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`,
  PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines; 10-of-10 recent main
  pipelines green; `gb news --stale-only` **empty**. At the 5-crew cap all
  cycle, so **0 dispatched** — spent the cycle verifying instead, which is
  the right trade when every slot is busy.
  **Lesson — "the crew is on that file" is not "the crew is fixing that."**
  ka-0000001304 (filed by a live crew as P2) claims all ~17 dedicated
  `gardenio-gvisor-*` RWX dispatches die at init validation. I re-derived it
  rather than trusting it: parsed every `- key:` block in `.rwx/e2e.yml` and
  diffed declared `init:` keys against the 14 keys shared tasks actually
  reference (`init.host-domain` :741, `init.version`/`ref-name` :1303-1304,
  `init.namespace`/`instance-id` :1365+). **18 dispatches, 1 complete
  (14/14), 17 declaring 2-4.** Then I ran the same parser against the crew's
  branch — **16 of 18 still broken there**; they fix only
  `gardenio-gb-stop-suspend`. The bug body's "the other ~16 need the same
  fix" reads as backlog; it is actually the whole outage, unowned.
  **Raised P2 -> P1**, because the main `gardenio-e2e` dispatch sets
  `golden-gate: "true"` and deliberately ABSORBS the gVisor substrate exits,
  sound *only* because the file promises those regressions "are still caught
  hard by the dedicated gardenio-gvisor-* dispatches below". That promise is
  false for 16 of 17 — a silent hole under the release gate, not a broken
  convenience command. Root cause is additive drift (`version` de504afe0
  2026-05-06; `host-domain` 87df90b45 2026-06-14 added to shared tasks
  without backfilling dispatch init blocks), so it re-breaks without a lint —
  and the ~20-line parser above IS that lint.
  **Filed ka-0000002936** (P2): swept every `*_test.py` against the `python3
  <path>` lines in `.rwx/ci.yml`. 3 orphans; 1 covered by in-flight !6869;
  **2 are not** — `scripts/coverage/normalize-paths_test.py` (36 tests) and
  `scripts/sanitize/apply-patches_test.py` (9). Both pass standalone in <1s;
  **45 assertions invoked by nothing.** No `pytest.ini`/`setup.cfg`/
  `pyproject.toml`/`tox.ini` exists anywhere, so there is no auto-discovery
  path that could be quietly running them. `normalize-paths.py` rewrites LCOV
  for the *required* Coverage gate — a regression there corrupts the number
  rather than failing the gate.
  **Also unblocked cycle 54's own ledger MR !6866**, red on `lint-mr-proof`
  and orphaned by its despawned gardener: the PROOF block was missing the
  required `Not established` field (`REQUIRED_FIELDS`,
  `scripts/lint-mr-proof.sh:58-63`). Patched the description, verified with
  `bash scripts/lint-mr-proof.sh <file>` locally *before* pushing, repipelined
  -> green -> merged. Worth noting the shape: a docs-only MR can sit red on a
  metadata lint indefinitely, and nothing reaps it once its author is gone.
  Decision = none; nothing here is an architectural fork.

- 2026-08-06 (cycle 54, gardener ka-0000000690): **193 modules / 208 scripts
  = 191 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`,
  partitioning PASSes at TOTAL_SHARDS=4/5/8/16); 0 quarantines; 20-of-20
  recent main pipelines green. Backlog: **231 closed / 0 open** work-typed
  children — every in-flight child had a LIVE pod (`gb crew --all`), zero
  stale to reap. Dispatched 2 crews on gaps I verified at HEAD myself.
  **Lesson — pull the RWX logs before believing an auto-filed failure
  report.** Release .11's bug (ka-0000003034) rendered all 4 failure blocks
  as `(no output captured)`, which reads as "undiagnosable." The output was
  never missing — only unattached; `rwx runs show <run>` + `rwx logs
  <task-id>` returned it in full. Acting on the report alone would have cost
  a crew two wrong premises: (a) claudemore-hooks looked like a REGRESSION of
  the .10 fix, but the shard log shows **10 passed / 2 failed** (was 0/12) —
  the `seed_claude_home` login fix WORKS and every hook leg is green; only
  the T5/T6 replay-cassette hash misses (`record_rc=0 replay_rc=1
  miss_count=1`) remain, which is the volatility class ka-0000000029 already
  deferred as "non-gating"; (b) trigger-system printed **every assertion
  passing** and then died with no failure line. Localized it to
  `test-trigger-system.sh` step 1e: `create_trigger()` does an explicit
  `return 1` (line 140) and is called in a command substitution under `set
  -euo pipefail` (line 26), so the script exits instantly and the `else ...
  fail` branch at line 410 is **unreachable** — a hard failure that reports
  nothing. Both corrections filed on ka-0000007771 before the crew started.
  Also filed **ka-0000001020** from a coverage-gap audit of 73a4aa2ed: the
  `run_module` skip protocol is well covered by `test-skip-visibility-lint.sh`
  Part C, but the release-summary half — `.rwx/release.yml:573-575`
  `SKIPPED_MODULES` cross-shard `comm -23` and the `:1007-1011` headline
  render — has **zero** assertions in either suite (`grep -rn
  "SKIPPED_MODULES" gardenio/tests/ scripts/` is empty). That is the text a
  human reads to judge a release, and it fails green-looking.

- 2026-08-06 (cycle 52, gardener ka-0000005623): **191 modules / 206 scripts
  = 189 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`'s
  `exclusion-list complete` line, re-run at post-merge HEAD); 0 quarantines.
  **Merged !6834** (ka-0000004253, `gb spawn` SPAWNED success-token, 512-line
  hermetic module + negative control) and **!6855** (ka-0000007208, thread-task
  idle-hint demotion, live-daemon Go e2e + module); both beads auto-closed.
  **New failure mode found — a WEDGED crew reads LIVE to every liveness check.**
  Two crew pods sat `1/1 Running`, health-probe green, and listed by
  `gb news --active-only`, while spin-looping a rate-limit error **78× over ~3h**
  (`gb news --stale-only` was EMPTY the whole time). Cause: the `crew-e2e` role
  bead ka-0000004014 pins `claude-opus-4-8-oauth`, which has **no LiteLLM
  fallback group** (only fable-5 has one), so per-agent budget exhaustion has
  nowhere to fail over and the backoff never terminates — logging
  "agent working again" on every pass. Discriminator was pod AGE, not config:
  the two oldest (3h51m, 4h3m) were wedged, the two youngest healthy with
  byte-identical model env. This defeats the documented stale-triage rule
  (`Running` + not-stale ⇒ "nothing to rescue") and had stranded a green,
  mergeable MR. Filed **ka-0000007458** (P1); reaped both pods, requeued
  ka-0000002157, redispatched. **Lesson: `Running` is not `working` — for a
  long-lived crew, check log PROGRESS, not pod phase.**
  Coverage pass: the `dark-hook deadman` lead was a false positive (fully
  covered — guardrail held), but its **sibling** was not: filed
  **ka-0000004554** (jira-webhook dark deadman has unit tests only, while the
  near-identical GitLab sibling has a module + Go e2e + wiring). Reusable
  heuristic: **when a suspected gap turns out covered, check its siblings** — a
  covered X makes an untested near-identical Y a *stronger* signal, not weaker.
  4 crews in flight; decision = none.
- 2026-08-06 (cycle 51, gardener ka-0000007115): **188 modules / 203 scripts
  = 186 wired + 17 excluded, drift 0** (read from
  `test-shard-isolation.sh`'s `exclusion-list complete` line); 0 quarantines;
  **20-of-20 recent main pipelines green**. **Merged !6851** (ka-0000003390,
  `/up` phase-2 footer + global keymap) — but the merge was pressed by its
  *own crew*, not stolen: the MR sat non-draft/mergeable/all-jobs-green while
  the crew flickered `idle`, so per ka-0000003385 I **mailed the crew to merge
  it** rather than adopting it, and it did (05:46:32Z). Restraint over
  reflex — a live crew that is nearly done still owns its delivery. Its diff
  is worth noting beyond the test: backfilling the gap **surfaced a real
  two-driver keydown collision** (`up.js` self-gated OFF elsewhere, but
  `keys.js` had no `/up` guard, so `?` opened *two* dialogs), and the fix is
  the symmetric mirror gate. Coverage work found a live bug — the argument
  for e2e over unit tests, made concretely. Counts are unchanged because it
  is a **Playwright** surface (no `run_module` wiring) — the two-suite rule
  (ka-0000000850) cuts both ways: it governs where you *add* coverage, not
  just where you *look* for it.
  **Two goal-reachability defects repaired, both instances of ka-0000002123:**
  (1) a `type=mail` bead (ka-0000001949) was a `parent-child` child of the
  epic — a non-issue child makes "all children closed" unreachable *by
  construction*; (2) the epic itself read `status=blocked` on a `blocks` edge
  to ka-0000002283, a decision the operator had **already resolved and
  closed** — a stale edge silently gating the whole lane. Removed both
  **edges** (never the beads) and reopened the epic. Worth internalizing the
  recurrence rate: I then created a mail bead myself mid-cycle
  (ka-0000003801, the merge nudge) and it **auto-parented into the epic too**
  — so this is not a one-off authoring slip but a standing property of
  `gb mail send` from a gardener anchored to an epic. Audit non-issue children
  at the *end* of a cycle, not only the start; the ones you create yourself
  are the ones you will not think to look for.
- 2026-08-06 (cycle 50, gardener ka-0000003514): **188 modules / 203 scripts
  = 186 wired + 17 excluded, drift 0** (`test-shard-isolation.sh`'s
  `exclusion-list complete` line — note bare `grep -c run_module` reads 188 and
  agrees only by luck); 0 quarantines. **Merged !6847** (ka-0000001161,
  escalation `/result` fields-merge live-daemon leg — green, non-draft,
  test-only, self-merged per ka-0000002230). Backlog: 4 in_progress children,
  all with live crews (`gb news --stale-only` empty — nothing to reap).
  **Filed + dispatched 1 HEAD-verified gap: ka-0000007208** (crew
  ka-0000002172) — merge `7d35eb2ce` (ka-0000000287) shipped the thread-task
  idle-hint demotion **unit-only (464 lines) AND flipped
  `threadTaskHintInsteadOfClose: true` in `deploy-values/gardenio-rwx.yaml`**.
  Helm default is still `false`, so the death rule inverted *in the exact
  namespace e2e runs against* with nothing asserting it. All 6 emitted
  selectors (`gardenio-idle-hint`, `idleHint`, `HintInsteadOfClose`, …) have 0
  hits across **both** suites (ka-0000000850). **TRAP, 4th cycle running:**
  `grep -rln 'auto-close'` returns 10 files incl. `test-auto-close-cascade.sh`
  and `test-stale-reaper.sh`, but grepping `thread_task` *inside* those three
  returns **zero** — they cover the parent-child close cascade and the
  stale-AGENT reaper, different subsystems from the slack-**thread-task**
  auto-closer. Same verb, different subsystem. Cumulative rule holds: **grep
  the SELECTOR the handler emits — not the feature name (c25), the sibling
  verb (c24), the page (c26), or the shared verb (c50).** No decision filed —
  nothing this cycle was a genuine architectural fork.
- 2026-08-06 (cycle 49, gardener ka-0000006297): **188 modules / 203 scripts
  = 186 wired + 17 excluded, drift 0** (read off `test-shard-isolation.sh`'s
  `exclusion-list complete` line, not `grep -c run_module`); 0 quarantines;
  **10/10 recent main pipelines green**; 0 open P0/P1 e2e bugs. Epic backlog:
  3 in_progress children, all three with genuinely live crews
  (`gb news --stale-only` empty — nothing to reap). **Filed + dispatched 1
  HEAD-verified gap: ka-0000003390** (crew ka-0000001806). Merge `6bb42051d`
  (MR !6842, ka-0000004641) shipped the GV phase-2 `/up` action footer + global
  keymap — 1327 lines, **unit-only** (329 test lines) — and every selector it
  emits (`up-card-footer`, `up-card-action-row`, `up-drawer`, `up-action-open`,
  `upMnemonic`, …) is at **0 hits across BOTH suites**; no spec `goto`s `/up`
  at all. **The hazard, not merely the absence:** `/up` is the first viewer page
  to install a *second* document-level `keydown` listener over the same keys as
  the global one, and neither side yields. `up.js:476` never calls
  `stopPropagation` (grep -c = 0); `keys.js:245` has no `data-surface` guard
  (only up.js mentions it); `layout.html` loads keys.js (3248) **before** up.js
  (3267), so on a shared key keys.js runs first. The gate is **asymmetric** —
  up.js self-gates OFF on non-/up pages, keys.js never gates OFF on /up — so on
  `/up` both run: `?` toggles two independent `<dialog>`s (keys.js lazily
  `appendChild`s its own to `document.body`, and up.js's `helpOpen()` can only
  see its own), and `j`/`g`/`/` double-dispatch. The `c` collision is *latent*
  by one character: keys.js guards on `#comment-text` while up.html:135 names
  its textarea `up-comment-text`. Unit tests structurally cannot catch a
  two-listener race — `up_asset_test.go` only string-matches contract markers,
  and neither test file ever POSTs to a resolved `upEp*` path
  (`NewRequest("POST"` => 0 hits). Ranked #1 because GV epic ka-0000006841 has
  **5 open children** incl. the chatbox PRD that will add a *third* key handler
  to this same page — up.js's §8.4 `chatboxFocused()` gate is already a stub
  written in anticipation. Backfill targets the **Playwright** suite (no
  `run_module` wiring). Cumulative rule extended: grep the SELECTOR the handler
  emits, in **both** suites (ka-0000000850) — and when a merge adds a global
  listener, the gap is the *interaction*, not the feature. 4 crews in flight,
  under the 5 cap. Decision = none — no genuine architectural fork.

- 2026-08-06 (cycle 48, gardener ka-0000007868): **188 modules / 203 scripts
  = 186 wired + 17 excluded, drift 0**; 0 quarantines; **10/10 recent main
  pipelines green**; 0 open e2e bugs. Epic backlog: 3 in_progress children at
  cycle start, **all three carrying genuinely live crews** (pods Running 53m /
  133m / 146m — `gb news --active-only` and the pod list agreed).
  **Filed + dispatched 1 HEAD-verified gap: ka-0000001161.** Merge `385bab22e`
  (ka-0000000207) repointed `EscalationReaper.reap()` from the generic
  `CloseBead` to `CaptureEscalationResult` (POST `/v1/escalations/{id}/result`)
  and gave the daemon's `captureResultRequest` a `Fields` map merged atomically
  with the result+close, typed fields winning on collision. Shipped unit+probe
  only. Zero-coverage proven at HEAD: `reaped_by`/`reaped_at`/`reaped_reason`
  have **0 hits** anywhere in `gardenio/tests/`, and there are **0 live POSTs**
  to `/result` — real e2e traffic resolves via `/approve` (8) and `/reject` (4).
  **FINDING — a mock that re-implements the daemon is not daemon coverage.**
  `test-escalation-auto-reaper.sh` looks like it covers this: it drives the
  built-from-source `escalation-reap-probe`, and that probe's `mockdaemon.go`
  was edited in the very same merge to model the new `Fields` merge semantics.
  But modelling the contract in Go beside the code under test proves only that
  the mock and the reaper agree. If the real handler stopped honoring `Fields`,
  or let caller fields clobber `result_status`, every existing test stays green.
  Generalizing cycle 47's "a module's header comment is a claim, not evidence":
  **a hand-written mock of the far side is a claim too** — the only evidence for
  a client/server contract is a leg that crosses the wire. Corollary seen here:
  the two `/result` mentions in `test-escalation-crud.sh:418` and
  `test-escalation-execution.sh:464` are *prose describing the verb*, so a
  path-grep scores 2 hits on an endpoint no test has ever called.
  Coverage-gap pass also **retired a stale mission premise**: the brief's
  standing coop item ("mux 42.4%, throttle.rs/poller.rs/lib.rs all 0%") is
  **closed** — all three now carry `#[path]` sibling suites
  (`throttle_tests.rs` 16 tests, `poller_tests.rs` 6, `lib_tests.rs` 6).
  Ledger: MR !6845. Decision filed: none — no architectural fork this cycle.

- 2026-08-06 (cycle 47, gardener ka-0000002472): **188 modules / 203 scripts
  = 186 wired + 17 excluded, drift 0** (per the authoritative hermetic lint
  `test-shard-isolation.sh`, which also re-verified the shard partition at
  TOTAL_SHARDS=4/5/8/16); 0 quarantines; **10/10 recent main pipelines green**;
  0 open e2e bugs. Epic backlog at cycle start: **500 children, 496 closed,
  1 open, 3 in_progress** — every in_progress one carrying a live crew.
  **Spawned 2 crews**: ka-0000000407 (the last open child — seed-shadow guard is
  single-file) and ka-0000004600 (filed this cycle). 5 in flight = at budget.
  **Coverage-gap pass**: audited the 5 merges since 2026-08-05T16:00Z that landed
  runtime code with no `gardenio/tests/` delta (close-MR race, jira re-read tool
  advertisement, SubagentStop output, jira-crosslink no-respawn, jira-bridge 502).
  **All 5 were already covered** — the backfills simply landed in later commits
  than the merges themselves, which is exactly why a merge-shaped scan flags them.
  **FINDING — a module's own header comment is not evidence; it is a claim.**
  The one real gap this cycle (ka-0000004600) is inside an otherwise strong module.
  `test-mr-close-live-recheck.sh:39-40` documents itself as covering "the
  PRIVATE-TOKEN header, the GITLAB_TOKEN->GLAB_TOKEN env fallback" — and covers
  neither. Its stub branches purely on the URL path and never reads a request
  header; every leg sets `GITLAB_TOKEN`, so no leg makes `GLAB_TOKEN` the only
  token present. Delete either code path in `close_mr_live.go` and all 6 legs stay
  green. This fails *silently closed*: `fetchMRLiveState` returns `mrLiveUnknown`
  on every failure by design, and `mrLiveUnknown` preserves the refusal — so the
  regression re-arms the fleet-wide self-merge-then-close block with a green suite.
  Generalizing cycle 24's sibling-verb trap and cycle 25's two-renderers trap one
  step further: those were about greping the wrong *token*; this is about trusting
  *prose* over an assertion. When a script's header claims a contract, grep the
  stub/assertions for it — a self-described coverage claim is the easiest kind to
  inherit unchallenged, because it reads as the testimony of someone who knew.

- 2026-08-06 (cycle 46, gardener ka-0000000788): **186 modules / 201 scripts
  = 184 wired + 17 excluded, drift 0**; 0 quarantines; 9/10 recent main pipelines
  green (10th running, 1 canceled); **0 open e2e bugs**. **Spawned 0 crews** — all
  6 open epic children already carry a LIVE crew (`agent_state` working/
  rate_limited, every one updated within the minute), so at 6-in-flight I am over
  the 5-crew budget and dispatched nothing. 3 of the 6 have MRs up (**!6830**,
  **!6832** both `mergeable`+green, **!6834** still Draft); I did NOT merge them —
  advice ka-0000003385 is about *orphaned* MRs, and a liveness probe showed all
  three owners still working, so merging would have raced a live crew mid-run.
  **Drift-check false positive worth recording:** comparing script *basenames*
  against `run_module` *labels* reported 2 phantom unwired scripts
  (`mr-close-live-recheck`, `mr-dispatch-gate-e2e`) — both ARE wired, under labels
  that differ from their filename (`mr-close-live`, `mr-dispatch-gate`), and a
  third (`test-edge-auth.sh`) lives outside `scripts/`. Drift must be computed on
  the **script path** in the `run_module` line, never the label. **Coverage-gap
  pass** over the 5h runtime-merge window: the a11y (ka-0000007487/ka-0000002181),
  spawn-token (ka-0000000982) and jira-nowake (ka-0000004442) merges all landed
  with matching e2e or are already the in-flight children — **1 new gap** filed:
  **ka-0000000407** (P1, queued, uncrewed by budget). The seed-shadow guard added
  by ka-0000003677 hard-codes `seedScriptRelPath` to a **single file**, so it is
  blind to the **7 other e2e scripts that PUT builtin-authoritative type configs**
  (trigger/escalation/user/project/report). Non-vacuous: test-trigger-system.sh
  PUTs type:trigger with 35 fields vs the builtin's 44, **dropping 17 — including
  `gardener`**, the nested blob the whole gardener-authoring flow reads. The PUT is
  namespace-wide and `cleanup()` never DELETEs it, so the shadow leaks into every
  later module in the shard (test-gardener-start-argv-contract.sh and
  test-plans-wake-button.sh both read trigger `gardener` fields) — a
  shard-ordering-dependent false green. Bead carries an anti-vacuity requirement:
  the generalized guard must be demonstrated RED on today's tree first.
- 2026-08-06 (cycle 45, gardener ka-0000002421): **186 modules / 201 scripts
  = 184 wired + 17 excluded, drift 0** (`test-shard-isolation.sh` exit 0, re-run
  after both merges). 0 quarantines; **10/10** recent main pipelines green.
  **Merged 2 orphaned MRs and closed their children.** Both crews had squawked
  "watching CI to green, then self-merge", gone idle, and left a green mergeable
  MR sitting — the exact shape of advice ka-0000003385. **!6828** (ka-0000001762)
  adds a second `run_leg` to test-gitlab-webhook-deadman.sh pinning
  `gardenio_gitlab_webhook_deliveries_total{outcome}` on the **served** exposition
  (the existing unit test reads the registry via `testutil.ToFloat64`, which stays
  green even if the vec never marshals to scrape format) plus the load-bearing
  security property that `token_failed` must NOT stamp receive-liveness — else a
  wrong-secret hook reads healthy and the deadman never pages. **!6829**
  (ka-0000007022, decision ka-0000004333 option `split`) installs a pinned,
  sha512-checked `claude` on shard 0 so **test-claudemore-hooks.sh RUNS instead of
  counting green-by-skip on every run** — a HARD shard-0 gate that had been
  contributing 0 assertions to the release-gate tally; claudemore-agentchoice is
  now documented in-tree as an ACCEPTED opt-in-tier skip so it is not re-filed.
  **Coverage-gap pass: 4 candidates surfaced, 3 were already the in-flight
  children** (good dedup — the live crews are on the right targets), so exactly
  **1 new gap** was filed + crewed: **ka-0000005870** (P1) — `gb admin
  strip-primary` has **zero** e2e in either suite (`gb admin` as a whole has
  none). This one is time-critical: **ka-0000000367 is OPEN to actually EXECUTE
  that irreversible migration**, so a persistent-state mutation is queued against
  real data with no end-to-end proof. Name-match trap recorded on the bead: the 19
  `primary_account` hits in test-session-resume/-credential-rotation assert
  *preserve* and *clear-to-empty-string*, **none asserts removal** — and the
  load-bearing assertion is that the field ends up ABSENT, not `""` and not the
  string `"null"` (the unit test's `fakeStripClient` is a map whose unset is
  `delete()`, so it cannot see a degraded null-delete). **Cleared stale decision
  ka-0000002283** (`gb decision cancel`, which does not respawn the dead filer):
  it asked whether to extend a `max_runs=40` budget "hit THIS cycle", but at HEAD
  the trigger reads `run_budget: 52`, I ran as spawn 45, and `stop.loop_end.any`
  now carries a **`bead_query` on open children of ka-0000000958** — dormancy is
  gated on the epic, not an arbitrary run count. **Auto-parent leak is GONE**: the
  spawn I made this cycle produced an `assigned` edge, not parent-child, so the
  c8a12e488 fix has reached prod and the standing manual-detach step is retired.
  8 children, all issue-kind, all in_progress with live crews. Decision = none.
- 2026-08-06 (cycle 44, gardener ka-0000005635): **186 modules / 201 scripts
  = 184 wired + 17 excluded, drift 0** (`test-shard-isolation.sh` exit 0 at
  HEAD 074fad412). 0 quarantines; 9/10 recent main pipelines green (1 canceled,
  0 failed). All 7 open epic children were **in_progress with LIVE crews**
  (`gb news --active-only`; 0 stale), so no re-dispatch — the cycle was an
  orphaned-MR sweep plus a coverage-gap pass.
  **Closed MR !6818 as SUPERSEDED (not rejected).** It was agent-authored,
  non-draft, `detailed_merge_status=conflict`, bead ka-0000000325 open and
  unassigned — the orphan shape from advice ka-0000003385. But the fix was
  already on main: `1b2806c61` (ka-0000005065) landed 01:28:34Z, 11 min after
  !6818's commit (01:17:56Z), doing the same `project:`-label stamp on the
  `mk_epic` fixture. Verified before closing that main's `mk_epic` carries
  `-l "project:${project}"` with the same env precedence, that `grep -n 'kd
  create.*--type=epic'` returns exactly ONE (labelled) site, and that main's
  rationale comment is a superset. Rebasing would have produced an empty diff.
  Filed 2 new gaps, dispatched 2 `crew-e2e`, both HEAD-verified before spawn.
  **FINDING — a merged fix can INVERT an existing wired module, and the unit
  tests will not tell you.** ka-0000002313 (P1): `test-jira-conversational.sh`
  (wired, run-shard.sh:2150) asserts via `bridgeClosureStillWakes` that the Jira
  bead-closure notice **still wakes** the assignee — `jira-conv-probe/main.go:1911`
  fails unless it produces exactly 1 respawn. That pin was correct when written
  and is **false at HEAD**: `77e7e0f82` (ka-0000004442) added the closure marker
  to `bridgeGeneratedMarkers` (jira_webhook.go:740), suppressing it. The fix
  author updated the Go unit legs (`jira_webhook_test.go:1454-1456`) and not the
  e2e probe — probe last touched 01:13:52Z, fix merged 01:45:38Z, 32 min apart.
  Generalized: **a fix that flips a behavior must be grepped against tests that
  PIN THE OLD behavior, not just tests that lack the new one.** Coverage-gap
  passes look for absent assertions; this failure mode is a PRESENT assertion
  that silently became a lie. Cheap detector: when a diff adds an entry to a
  matcher/allow/marker table, grep e2e for the bead id in the *removed* guard's
  comments — here `ka-0000004442` appears in 6 probe lines all narrating it as
  open. (Mirror of cycle 20's lesson; cycle 21 said grep the COMMAND not the
  FIELD — this one says grep the BEAD ID the old test cites.)
  Second gap ka-0000004253 (P2): `283942476` gave `gb spawn` the `^SPAWNED
  agent=… bead=… status=…` token whose entire purpose is letting gardeners
  confirm a spawn by grep — the commit message records a gardener re-dispatching
  **three times** because the old prose line did not match. 238 lines of
  `spawn_test.go` shipped, but `grep -rn 'SPAWNED' tests/e2e/scripts/` returns
  4 hits that are ALL unrelated (a local `SPAWNED_AGENTS` array + the word
  "RE-SPAWNED" in a comment). Briefed to assert the token anchors at **line 1**
  and that a FAILING spawn emits none — a whole-stream `grep -q` would pass on
  the exact drift the contract exists to prevent.
- 2026-08-06 (cycle 43, gardener ka-0000004435): **186 modules / 201 scripts
  = 184 wired + 17 excluded, drift 0** (`test-shard-isolation.sh` exit 0 at
  HEAD 769600dcf, re-run immediately before this write per cycle 42's note).
  0 quarantines; 8/8 recent main pipelines green. Epic opened the cycle with
  the backlog **drained to 1 open child** (itself blocked on operator decision
  ka-0000004333, since resolved `split` and now in flight as ka-0000007022).
  So the cycle was **all coverage-gap pass**: swept 253 commits from the last
  3 days across controller/kbeads/coop/helm, filed **5 new gaps**, dispatched
  5 `crew-e2e` — ka-0000002157 (`gb stop --successor=new` succession chain),
  ka-0000007798 (`gb viewer-login`), ka-0000007059 (canonical `gardener_id`
  stamp), ka-0000001762 (webhook `deliveries_total{outcome}`), ka-0000007900
  (`gb pipeline retrigger` MR-endpoint). Every gap re-verified at HEAD by the
  gardener before spawn, and the sweep also recorded ~13 candidates REJECTED as
  already-covered (with the covering file) so they are not re-derived.
  **FINDING — the one-noun-two-features trap, twice in one sweep.** Both near
  misses would have read as "covered" to a bare grep: (1) `gardener_successor.spec.js`
  and `schedule_prior_agent_id` look like `gb stop --successor` coverage but are
  the C3 card-graph PENDING-heir surface and the gardener-run-chain field — two
  unrelated mechanisms sharing a noun; (2) `test-gitlab-webhook-deadman.sh` is
  wired, good, and about the *same feature*, yet asserts a **disjoint metric
  series** (`_dark`/`_evaluations_total`, never `deliveries_total`). Generalized:
  a name-match must be disqualified by reading the matching test's *header/scope*,
  not by its filename — and for metrics, coverage is per-SERIES, not per-feature.
  Also recorded: `viewer-login` sat on cycle 16's "recorded VACUOUS surfaces"
  list as un-drivable, but the chart now ships the viewer-side `K8S_SA_*` env,
  so **deferral lists go stale and need re-checking against the chart**, not
  trusting.

- 2026-08-06 (cycle 42, gardener ka-0000008000): **186 modules / 201 scripts
  = 184 wired + 17 excluded, drift 0** (`test-shard-isolation.sh` exit 0 — the
  authority; the bare grep reads 186/198 and is NOT a drift input). Snapshot
  note: my first read said 200/16 and was stale by the time I wrote it —
  `test-skip-visibility-lint.sh` landed mid-cycle via a live crew's !6814. The
  suite moves *during* a gardener cycle, so re-run the hermetic check
  immediately before writing the ledger rather than trusting the step-1
  snapshot; the second run is what is recorded here. 0
  quarantines; **11/12** recent main pipelines green (the one failure #24019 is
  already owned by the auto-spawn triage agent — CI/CD gardener's beat, not
  mine); 0 open P0/P1 e2e bugs. **Merged 2, spawned 2, filed 1.**
  **THE FINDING — two finished MRs were orphaned at the finish line, not
  blocked.** Both crews did complete, correct work, pushed a green branch, then
  despawned in the last minutes before merging. ka-0000005455 read
  `in_progress` with a dead assignee (`gb news --stale-only` caught it);
  ka-0000002181 had already reverted to `open`/unassigned. Neither is visible
  as "needs a gardener" from a bead-status scan alone — **an in_progress bead
  with a dead assignee and an open MR is the highest-value thing a gardener can
  look at, and it looks identical to healthy work in `kd list`.** Merged
  **!6813** (jira-reply 422 reclassify + ADF code-mark + type:message schema,
  20/20 required green) and **!6807** (WCAG AA contrast on /gardeners + /plans,
  19/19 green, and it adds both routes to the a11y `ROUTES` suite so the fix is
  CI-gated going forward). Next gardener: run `gb news --stale-only` AND check
  `mr_url` on every open child before concluding the backlog is just "in
  flight".
  **A required gate and an advisory bot disagreed; the advisory was right about
  the number and wrong about the conclusion.** !6813's sticky patch-cov comment
  said `controller 0.0% (need >=60%)` while required `RWX: Coverage / Patch`
  passed. Resolved it rather than merging past it: the only controller file
  touched was `cmd/jira-conv-probe/main.go`, and `internal/coverage/exclusions`
  `DefaultExcludes` has no `cmd/*-probe/` glob — but those probes are e2e-only
  infrastructure built solely by `.rwx/e2e.yml`, never shipped in a prod image,
  and MR-event coverage runs are unit-only *by design* (`.rwx/ci.yml:435-460`).
  So 0% unit coverage there is structural and permanent. Precedent confirms it
  is chronic, not a one-off: `9be64947d` (582 lines to this same file) and
  `c9e8160866` (160 lines, this sprint) both merged green with no
  `Coverage-Bypass` trailer. Filed **ka-0000007875** for the exclusion-list gap
  — an advisory that is *always* wrong for a known file class trains agents to
  ignore coverage, which is the real cost. The `Coverage-Bypass` trailer is the
  wrong instrument (none of its four reasons means "this file is test
  infrastructure").
  Spawned **2** crews, each gap re-verified by me at HEAD 4b449298e first:
  **ka-0000007487** (dark-skin contrast — cycle 41 held it on a `layout.html`
  scope collision with ka-0000002181; that merge resolved it, and I confirmed
  `#595959` still live at layout.html:478/515/527/636/659 plus **0** matches
  for `dark-terminal` in `a11y.spec.js`, i.e. the suite is still baseline-only)
  and **ka-0000007875**. Held **ka-0000004427** deliberately: every option
  routes through operator decision ka-0000004333, so a crew would only
  re-derive the same fork.
  **Epic hygiene:** swept a `type=decision`/`kind=data` parent-child leak
  (ka-0000002283) that made the terminal goal unreachable per ka-0000002123 —
  its `blocks` edge was preserved, so the decision still surfaces on /plans.
  **!6699 verified holding**: both crews I spawned this cycle produced agent
  beads with **zero** parent-child edges into the epic — the leak that cycles
  17/18/19 each hand-cleaned did not recur, now that a post-fix image is
  deployed.

- 2026-08-06 (cycle 41, gardener ka-0000001725): **186 modules / 200 scripts
  = 184 wired + 16 excluded, drift 0** (`test-shard-isolation.sh` exit 0); 0
  quarantines; **10/10** recent main pipelines green; 0 open P0/P1 e2e bugs.
  Epic (issue-kind): 20 closed / 5 in_progress / 3 open; all 5 in_progress
  crews verified LIVE via `gb news --stale-only` (0 stale to reap). Spawned
  **2** crews into my free slots, each gap re-verified at HEAD by me first:
  **ka-0000001413** (status_message round-trip) and **ka-0000004018** (chain
  `join_parent`/`branch_decision_key` heir preflight; read the op/parent flip
  in `gardener_chain_spawn.go` directly to confirm). **Caught a scope collision
  before dispatch:** ka-0000001413's "secondarily" step edits
  `seed-types-and-users.sh:45`, which is the exact scope of the LIVE
  ka-0000003677 crew — fenced the Playwright leg only on the bead, so the two
  crews cannot conflict on a 1-line PUT. Held **ka-0000007487** (P3 dark-skin
  contrast) deliberately: its acceptance edits the same layout.html token its
  still-live parent ka-0000002181 is changing; hold reason + dispatch condition
  recorded on the bead so the next gardener does not re-triage it. **Verified
  !6699 holds** — the `gb spawn` agent/session-bead leak that cycles 17/18/19
  each hand-cleaned did NOT recur: 0 parent-child edges on either new agent
  bead or my own session bead. Also corrected a false alarm worth not
  re-raising: `kd list --parent` is TRANSITIVE, so 4 non-issue beads
  (advice/role/session/trigger) appear under this epic as **grandchildren of
  already-closed children**, not as the ka-0000002123 hygiene leak — and the
  goal query filters `types=[task,bug,epic]`, so they cannot block terminal
  goal. Decision = none new; **ka-0000002283** (run budget) remains open from
  cycle 40 — but I **corrected its central premise on the bead**: it says
  `max_runs: 40` is binding and cycle 40 was the last allowed. It is not.
  `gardener_evaluator.go:250` gates on **`RunBudget`** (`run_budget: 42` in this
  blob), so `max_runs` is inert here and `runs_spawned=41 < 42` is why I fired
  at all. **Cycle 42 is the real last run, and an operator answering `extend`
  must bump `run_budget` — bumping `max_runs` alone does nothing.** Exhausting
  it is treated as a *premature* stop and routes through
  `gardenerResumeOrEscalate` (re-arm, then escalate), not a silent close. I first
  speculated `max_runs` was fleet-wide unenforced and **retracted that on the
  bead after reading the gate** — it is the bridge gardener-spec path where
  `run_budget` is the analogue; schedule-kind loops are unaffected. Diagnosing
  from one lane's symptom instead of reading the gate is the same
  persistence-!=-behavior error the backfills on this epic exist to catch.

- 2026-08-06 (cycle 40, gardener ka-0000005364): **186 modules / 200 scripts
  = 184 wired + 16 excluded, drift 0** (`test-shard-isolation.sh` exit 0, PASS
  at TOTAL_SHARDS=4/5/8/16); 0 quarantines; 0 open P0/P1 e2e bugs. Epic
  (issue-kind only): 20 closed / 5 in_progress / 3 open. Spawned **1** crew
  (ka-0000004842, news-alive-freshness Scenario 4) into the single free slot —
  4 inherited crews were live, so 5/5 at budget. **Filed decision ka-0000002283
  (P1): this trigger hits `max_runs=40` THIS cycle** while 8 children are still
  live — the run counter is about to end a lane whose own `bead_query == 0`
  goal condition says it is unfinished. Raising my own ceiling is a
  self-modification of a lifecycle bound, so it went to the operator.
  **Coverage-gap pass over 7 days / 398 commits found NO new gaps** — the four
  thin greps all resolved to real coverage on inspection (`gate_type` →
  test-formula-gate-pour.sh asserts the stamped field; dangling `depends_on` →
  test-formula-lifecycle.sh Scenario 12 is a live file-path pour leg;
  transparency-proof → test-session-spend-attribution.sh runs hard `go test`
  sub-gates). Recording the negative result deliberately: a cycle that finds
  nothing after checking is evidence of convergence, and without it the next
  gardener re-greps the same six patterns. **Trap 4 is FIXED** — `c8a12e488`
  ("don't auto-parent agent/session beads into a goal-bearing epic") landed, and
  the crew bead I spawned carried only its `assigned` edge, no parent-child into
  the epic. The `xargs` detach recipe the last several ledger entries prescribe
  is now dead weight; drop it when ka-0000002199 closes.

- 2026-08-05 (cycle 39, gardener ka-0000007581): **184 modules / 198 scripts
  = 182 wired + 16 excluded, drift 0** (`test-shard-isolation.sh` exit 0, PASS
  at TOTAL_SHARDS=4/5/8/16); 0 quarantines. Epic (task+bug only): 119 closed /
  5 in_progress / 4 open. Heeded cycle 38's rule and did **not** quote
  `glab ci list -r main` as e2e health — main-push pipelines run coverage only.
  **Merged !6802** (ka-0000008180, flaky `TestHandleFormulaPour_Conditional
  StepFiltering`). Its author crew was still Running, so I applied cycle 17's
  correction: read the diff, re-fetched `.sha` immediately before the PUT, and
  confirmed it had not moved off the commit I reviewed (6ca64168). The fix is
  real on both legs — the poll now waits for molecule + BOTH step beads instead
  of just the molecule, *and* it takes `daemon.mu` around the final
  `filterTaskBeads` read, closing a pre-existing unlocked read of a map the
  async writers mutate. Worth noting the shape: the old predicate returned true
  on the FIRST bead written, so the test's own success condition was the race.
  **Dispatched 1 crew** — ka-0000004259 (P1, `/plans "⚠ N orphaned"` re-scope).
  Now at the max-5 crew budget (a11y ka-0000002181, agent_state ka-0000000270,
  claudemore ka-0000006183, green-by-skip ka-0000000989, + this one), so the
  3 remaining open children (ka-0000005455, ka-0000003677, ka-0000004842,
  ka-0000004018) wait for a slot rather than being mass-spawned.
  **HEAD re-verification caught a stale path in a bead body** — the guardrail
  cuts both ways. ka-0000004259 cites `internal/beadsviewer/plans_stall.go`,
  which **does not exist**; the runtime is `cmd/beads-viewer/plans_stall.go`.
  Every line number in the bead is correct *against that file* (:110 `Orphaned
  bool`, :121/:137/:149, :516 `reconcileStallWithCrew`), so the gap is real and
  only the prefix rotted. A crew that trusted the path would have found nothing
  and could plausibly have closed the child as already-fixed. Corrected as a
  comment on the bead before spawning. Same pass re-confirmed ka-0000005455 at
  HEAD (the "retry as plain text" string is live at `jira_reply.go:309` and
  still has **zero** hits under `gardenio/tests/`). Reminder carried forward for
  whoever takes ka-0000004259: per advice ka-0000000850 that is a **viewer**
  surface, so it belongs in the Playwright suite, not a shell `test-*.sh` —
  and its neighbour `plans_signal_strip.spec.js:303` disclaims the branch in
  prose, which is exactly why a name-match audit scores it covered.
  Decision=none. Stale crews=0.
- 2026-08-05 (cycle 38, gardener ka-0000001552): **184 modules / 198 scripts
  = 182 wired + 16 excluded, drift 0** (`test-shard-isolation.sh` exit 0, PASS
  at TOTAL_SHARDS=4/5/8/16); 0 quarantines.
  **STOP QUOTING `glab ci list -r main` AS E2E HEALTH — it cannot see e2e at
  all.** Every cycle including mine opens by reporting "N-of-N recent main
  pipelines green" (I had written "19-of-19"). Main-push pipelines run only
  coverage + manual jobs. **E2E runs on TAG/RELEASE pipelines**, dispatched to
  RWX, and surfaces in GitLab *only* as an external commit status
  `rwx/release` — not a job — so the jobs API returns an empty failed set and
  `test_report_summary` returns `{"total":{"count":0}}`. The green main number
  is real but it is evidence about `coverage`, not about the suite. Over the
  same window the actual e2e record was **77/100 tag pipelines green, with 9
  distinct e2e-module failures**. Correct recipe, no vault needed:
  `glab api "projects/acme%2Fplatform%2Fgardenio/pipelines?per_page=100&scope=tags"`
  → map SHA via `.../repository/commits/<sha>/statuses` → read the `rwx/release`
  `target_url` → `rwx results <run-id> --json` (per-shard TestCount /
  FailedTestCount / **AttemptCount**) and `rwx logs <task-id>` for the
  `MODULE PASSED:`/`MODULE FAILED:` sequence. **A fail-then-pass pair in ONE log
  is a flake; fail-fail is a deterministic regression** — that single
  distinction separated the one real flake below from four regressions that
  were already fixed. (`rwx artifacts download` would give JUnit directly but
  needs a vault unlock this token lacks.)
  Epic (task+bug only): 115 closed / 5 in_progress / 2 open. (Counts
  moved twice mid-cycle as my own crews' MRs landed — 182/196 at entry,
  184/198 at ledger-write. Always re-run the lint immediately before writing
  this line; a snapshot taken at step 1 is stale by step 7 on a busy day.)
  **The whole cycle has one theme: three separate GREEN tests that cannot
  fail.** Each was found the same way — not by looking for missing tests, but by
  asking of an existing passing test "what would have to break for this to go
  red?" That question is now the recommended first move of the coverage pass;
  a name-match audit finds none of these, because the test *is* there and it
  *is* green.
  1. **ka-0000004462 (P1, dispatched)** — `test-auto-parent-suppression.sh`
     asserts exactly `type=agent` (:396) and `type=session` (:405), which are
     exactly the two entries in the runtime skip list `autoParentSkipTypes`
     (auto_parent.go:84-87). **The test mirrors the fix, not the invariant.**
     `grep -ciE 'message|mail|squawk'` over the module = 0 — and `type=message`
     leaks *right now*: 41→42 message beads are direct children of this epic
     during this very cycle, 37 of them created after the fix deployed. My own
     `gb squawk` (ka-0000006254) is one: **a gardener cannot report status
     without polluting its own terminal goal.** This is why cycle 37 published
     "trap 4 RESOLVED, regression-tested live" — it read a green test that never
     checked the third type. Corrected inline above. Closed-count inflation on
     this epic is now **167 closed vs 115 real task/bug = 1.45x**.
  2. **ka-0000000270 (P1)** — the `agent_state` enum backfill (queued /
     model_rate_limited / stop_requested, 568969352). Zero live writes of any of
     the three across BOTH suites. The apparent coverage,
     test-microvm-execution-target.sh:452, calls `state_accepted queued` — but
     `state_accepted()` (:430-436) is a **shell loop over a hardcoded literal on
     :429**. It asserts a string against itself, never contacts the daemon, and
     passes with kbeads uninstalled. Third recurrence of the silent-PATCH-drop
     class (ka-0000000585 → ka-0000006367 → this) and the net cannot catch a
     fourth: all three call sites only LOG the write error, so a rejected value
     means the state silently never lands.
  3. **ka-0000001413 (P2)** — `status_message` (bb8645d16). The one spec bearing
     the identifier asserts `expect(res.status()).not.toBe(403)` — which
     **passes on the exact 502 the bug produces.** Its own header comment says
     so, and that comment went stale 34 minutes after it was written (verified:
     `git merge-base --is-ancestor bb8645d16 c6f07ebe3` = false). Two further
     stale masks: fixtures.mjs:1636 deliberately never seeds the field, and
     `seed-types-and-users.sh:45` PUTs a stored type:user config omitting it, so
     the shell-suite namespace **shadows the fixed builtin** — a live in-repo
     instance of the same shadowing that masked the original bug in prod.
  The same lens found four more, filed for the next cycle:
  **ka-0000005455 (P1)** — jira-reply 422 reclassification (4a0ce2a75). The
  jira-conv-probe *does* wire the real `JiraReplyAPI`, so it looks covered —
  but its stub Jira has exactly two WriteHeader calls, `201` and `200`. **There
  is no path in the probe that makes Jira return 4xx**, so
  `classifyReplyPostError` is unreachable and the leg passes with the whole
  function deleted. Generalize this: *"the probe wires the handler" is not
  coverage — check the probe can drive it into the branch under test.* A stub
  that only ever succeeds makes every error-classification path dead code.
  **ka-0000006183 (P1)** — claudemore REPLAY `fail_on_miss` (424 +
  `X-Claudemore-Replay: miss`), 0 hits in both suites. The existing zero-miss
  assertions count `cassette MISS` *log lines*, so a regression to a
  pass-through to upstream produces no miss log and a successful run: **every
  assertion still passes while the "$0 deterministic" tier silently spends real
  tokens.** Test it with upstream pointed at a closed port — that is the
  anti-vacuity control.
  **ka-0000003677 (P2)** — `seed-types-and-users.sh` PUTs stored type configs
  that shadow the kbeads builtins entirely (resolveTypeConfig prefers the
  store), and they have drifted: type:trigger omits `pipeline_ref_pattern`,
  type:user omits `status_message` (both `grep -c` = 0). A passing field
  round-trip may be exercising a shape prod does not have.
  **ka-0000000989 (P2)** — green-by-skip: `test-claudemore-hooks.sh` and
  `-agentchoice.sh` `skip_clean` (exit 0) without a `claude` binary, and CI
  installs *claudeless*, not `claude`. Careful — `grep -riE 'install.*claude'`
  over `.rwx/` returns 11 reassuring hits that are **all the substring
  "claudeless"**. So those modules contribute to both the wired count and the
  green count while asserting nothing, and no aggregate today can tell
  "passed" from "skipped". This is the counting-trap family turned on the
  suite itself; the durable fix is emitting JUnit `<skipped/>` + a per-shard
  skip count, not installing one binary.
  Also dispatched **ka-0000008180** (P2 flake, root-caused by my own crew:
  `assertEventually` waits only for the molecule and races the step-bead writes;
  same class as 89a2e61c5) — real flake signal, and the crew correctly cleared
  MR !6795 of blame rather than assuming its own MR caused it. And
  **ka-0000004842** (P2) — the one true flake in the release-pipeline history:
  news-alive-freshness Scenario 4 leaves its terminal fixture OPEN while
  asserting, so the reconciler (which treats an open `type=agent` bead as a
  desired pod) flips `agent_state` and `gb news` *correctly* reports
  `alive:true`. 6 of 13 shard-0 attempts; **two of the five affected pipelines
  are GREEN overall because the retry masks it.** The fix is already precedent
  in the same file — Scenario 2 closes its fixture *before* asserting with the
  comment "pod-spawn hazard"; Scenario 4 does it in the wrong order.
  **Rejection worth recording so it is not re-derived:** the standing prompt says
  coop is the below-floor component with `throttle.rs`/`poller.rs`/`lib.rs` at
  0%. **That is now stale** — `throttle_tests.rs` (16 tests) and
  `poller_tests.rs` (6) exist and are wired via `#[path]` include exactly like
  the known-good control `broker_tests.rs`; 20334b93b/73b072e7a cleared the 60%
  floor. Checking against a control before believing the "0%" saved a crew.
  Four other candidates were rejected as genuinely covered, with the covering
  file:line recorded on each so they are not re-derived: formula `depends_on`
  fail-loud (test-formula-lifecycle.sh:1139 Scenario 12), gate_type/timeout pour
  (test-formula-gate-pour.sh:283), `kd close` MR live-recheck
  (test-mr-close-live-recheck.sh:401 plus the three refusal legs), and the
  idle:active gauges (test-idle-active-metrics.sh). The Account-Capacity
  dashboard is **correctly** not an e2e target — it is `.helmignore`'d out of
  the chart and grafana is off in ephemeral namespaces, so a module would red on
  absence; its chart-render test is the right altitude.
  The viewer pass added two more, and the P1 has the same fingerprint as
  everything else this cycle: **ka-0000004259 (P1)** — the /plans
  `⚠ N orphaned` re-scope (777dcdc88) has **zero** assertions; all three
  `orphaned` hits in the corpus are comments. Both neighbours route around it
  *in writing*: `plans_signal_strip.spec.js:303` asserts `⚠ stalled` with the
  comment "not the re-scoped 'orphaned'", and `test-plans-stall-badge.sh:534`
  builds a fixture that sets the step's *Assignee* to the live crew (so
  `anyStepAgentLive` clears deadSteps) and wires no `assigned` edge, so
  `reconcileStallWithCrew` never runs. Two modules named for the feature, both
  testing the pre-change path. Needs the two-agent fixture neither builds; goes
  in the shell suite for the SQL backdate rig.
  **ka-0000004018 (P2)** — chain `join_parent`/`branch_decision_key` are
  **persist-tested but never behavior-tested**: the spec proves the field lands
  on the bead, nothing proves the spawn path translates it into a
  `==0 open under <parent>` preflight. A regression makes the "ship" heir
  inherit the default `>0 open under my own epic` and fire immediately against
  an unfinished fan-in — bead looks correctly authored, spawn succeeds, nothing
  reds. *Persistence is not behavior* is worth adding to the trap list.
  5 crews at cap; **8 beads queued for next cycle** (ka-0000001413,
  ka-0000004842, ka-0000005455, ka-0000003677, ka-0000000989, ka-0000004259,
  ka-0000004018 — P1s first). Two P3 maps CSS hooks (`.pill.pending`,
  `.maps-arrow.is-pending`/"will chain to") were deliberately **not** filed as
  beads: they are a ~6-line addition to the fixture `gardener_successor.spec.js`
  already builds, and filing them separately would put two crews in one file.
  Decision = none.
- 2026-08-05 (cycle 37, gardener ka-0000001151): **182 modules / 196 scripts
  = 180 wired + 16 excluded, drift 0** (`test-shard-isolation.sh` exit 0 twice,
  PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines; 10-of-10 recent main pipelines
  green and **0 failed jobs across the last 20 main pipelines** (no flake signal
  to file). Epic at entry: 161 closed / 2 in_progress (both live) / **0 open**.
  **Counting trap 4 is RESOLVED and I regression-tested it live:** ka-0000002199
  closed, all 10 `agent`/`session` direct children are closed, and after I ran
  `gb spawn` the new agent bead (ka-0000007638) was **not** parented into the
  epic — 0 leaks. Downgraded the trap-4 detach recipe from mandatory
  remediation to cheap verification (see the note under trap 4).
  [**CORRECTED by cycle 38 — this "RESOLVED" is only two-thirds true.** The
  `agent`/`session` half is genuinely fixed, but `type=message` was never in the
  runtime skip list and leaks *today*: 41 message beads are direct children, 37
  of them created AFTER the fix deployed. The verification above could not see
  it because it checked only the two types the fix names. See cycle 38.]
  **Trap 2 re-fired via a different door, so read it as load-bearing, not
  historical:** `kd list --parent <epic> --all-types` surfaced 4 open non-issue
  rows (ka-0000000193 advice, ka-0000004014 role, ka-0000004413 session,
  ka-0000005355 trigger) that `kd dep children -t parent-child` does not.
  I verified each has a *different* direct parent, and that all 4 of those
  parents are themselves closed direct children — i.e. they are
  **grandchildren**. Publishing the first derivation would have manufactured a
  detach pass against edges that do not exist. Also: `kd dep children --json`
  emits `kind: null`, so filter it on `.type`, never `.kind`.
  **Two structural audits, both clean negatives worth recording** (they cost
  little and retire a standing worry): (a) **196/196 modules propagate
  failure** — swept for a trailing unconditional `exit 0` without `set -e` or a
  `FAIL` guard; every hit turned out to be `exit 1`-on-`FAIL>0` (spot-checked
  `test-gb-whoami.sh`, `test-gb-audit-fetch.sh`, `test-escalation-auto-reaper.sh`,
  `test-pf-supervisor.sh:141`). No vacuous-green gate exists today. (b) The
  ka-0000002902 **inert-on-fan-out** class is genuinely fixed: `reproCockpit`
  is now present **and `enabled: true`** in all three ephemeral templates (was
  `gardenio-rwx.yaml` only), so the fail-closed deployment-host allowlist
  regression test finally gates on every dispatch.
  **Coverage-gap pass — 2 filed from 9 HEAD-verified candidates, and the 7
  refusals are the point.** Epic had 0 open children, so instead of inventing
  work I swept 72h of merged runtime commits (touching `controller/`, `kbeads/`,
  `coop/crates/`, `helm/` with no test file in the same commit) and dispatched
  read-only verifiers mandated to grep **both** suites, open any name-matching
  test, and state whether it would still pass with the feature deleted. Filed
  **ka-0000002607** (P2, idp-log-shipper ASN-table gate: `main.go:139
  loadASNTable` fail-fast is unasserted *anywhere*; scope is the hermetic
  helm-render leg only — the runtime leg needs real CloudWatch via IRSA and
  `idpLogShipper` appears in **zero** deploy-values) and **ka-0000000216** (P3,
  jira prompt affordances). Refused as COVERED or NOT-E2E-TESTABLE, with
  reasons recorded on each bead so they are not re-filed: cron/timezone
  fallback (`cadence_fallback.spec.js`, non-vacuous — fixtures omit legacy
  cron), the dropped squawk fallback (negative invariant already pinned),
  `SetLimit(4)` on a 4-goroutine group (provable no-op), and the region-hint
  commit (a `{{- fail }}` string + a YAML comment — byte-identical render).
  **Best find of the cycle, and a shape to reuse:** ka-0000000216 is invisible
  to a name grep *and* to the module that looks like it covers it.
  `test-jira-bridge.sh:354-375` greps as `gb jira show` but passes on **any**
  exit code and would pass with the commit reverted; its real mention->spawn
  scenario never asserts `fields.prompt` and self-skips because
  `deploy-values/gardenio-rwx.yaml:435-437` disables `jiraBridge`. The wired
  hermetic probe drives the exact builders but **structurally discards the
  strings** — `mockdaemon.go:164` takes the prompt as `_` and `Nudge` drops the
  message. A test harness that discards the argument under test is a blind spot
  no coverage grep can see; when a module "should" cover something and doesn't,
  check the mock's signature before believing the module.
  Spawned 2 crews (4 in flight, budget 5), each carrying a named negative
  control (delete `jira_mentions.go:1090`; invert `deployment.yaml:106-113`)
  because both targets are the shape that still passes when the feature is
  deleted. `gb news --stale-only` empty. No decision filed — nothing here is an
  architectural fork.
  **Successors: my gardener prompt's coverage context is STALE.** It says coop
  55.3% is below floor and is "the one that matters for a *required*
  Coverage/Patch gate". ka-0000000457 and ka-0000007572 are both **closed**, all
  three named 0% files carry tests, and the `coverage` status on main HEAD is
  **success**. Re-measure before spawning a coop crew off that text.
- 2026-08-05 (cycle 36, gardener ka-0000002742): 180 modules / 194 scripts
  = 178 wired + 16 excluded, **drift 0** (`test-shard-isolation.sh` exit 0, PASS
  at TOTAL_SHARDS=4/5/8/16); 0 quarantines; 10-of-10 recent main pipelines green.
  Epic: 156 closed / 3 in_progress (all with live crews) / 0 open at entry.
  **Cleared a strand + merged !6788**: crew `…playwright-phb` reached
  `agent_state=done` with all 4 sessions closed but never called `kd close`, so
  ka-0000004549 sat `open`+unassigned with a fully green MR idle — the same
  strand mode as cycle 15's ka-0000004197. HEAD-verified all 20 external commit
  statuses on `dcf28d661` (CI Build/Lint/Test/Bench/Quench/Gitleaks/
  Internal-Strings, Coverage/Patch, Helm + Helm/Security, Docker, MicroVM,
  lint-mr-proof) before merging as f8cfd9c9a. That landed
  `gardenio/tests/slack-e2e/`, which makes the already-merged runner
  `test-slack-ui.sh` **Gate 1 reachable for the first time** (it had
  `skip_or_fail`-ed unconditionally at :76-78). Not a sprint child — merged only
  because it blocked a live runner.
  **Coverage-gap pass: 8 targets verified at HEAD, 6 already covered, 2 filed.**
  The 6 negatives are worth recording because three of them look like textbook
  gaps and are not: ka-0000004966-style *route-list* misses aside,
  `6d2ce1567` (`SetLimit` on /plans enrich waves) is **NOT-E2E-TESTABLE** — each
  wave has exactly 4 joins and the limit IS 4, so the semaphore never blocks and
  there is zero observable delta vs the revert (the commit says so itself); and
  `1d937f7e2` (drop the Live-agents squawk fallback) is a pure dead-code
  deletion whose standing negatives already exist at
  `index_home_rail.spec.js:379,403-404` and pass identically pre- and
  post-revert. Writing e2e for either would be textbook vacuity. Filed:
  **ka-0000007896** — `/index` is missing from `a11y.spec.js`'s hand-maintained
  `ROUTES` list (spec:85-96). Critically, `/` does **not** cover it: `server.go:1316`
  maps `GET /{$}` to `handleFeed` and `:1317` maps `GET /index` to `handleIndex`,
  with no redirect between them (`routes.spec.js:48-53` documents the move). So
  the new nested `<ul class="index-active-card-events">` markup from `e2b876ce9`
  has never been axe-scanned. **This is the 2nd instance of this exact pattern**
  (cycle 16 filed the same for `/feed`); `/gardeners` + `/plans` are also absent
  and in scope. **ka-0000001242** — the `gb gardener start` argv contract from
  `7c6aba74a` is unit-only, and the scenario that *looks* like coverage
  (`gardener-mention-entrypoint.toml:69`) **hardcodes the flag in the shell** as a
  hand-copied replica rather than calling `gardenerStartArgs`
  (`bot_mentions.go:2570`) — revert the bridge and it stays green. That replica
  is how ka-0000006141 shipped. Scoped the bead away from the Slack ingress
  (socket-mode websocket, no injectable event endpoint, `lib-slackmock.sh` is
  outbound-only) and onto the argv contract, with all three vacuity traps named.
  Spawned 2 crews (5 = budget full). No decision filed.
- 2026-08-05 (cycle 35, gardener ka-0000000549): 179 modules / 193 scripts
  = 177 wired + 16 excluded, **drift 0** (`test-shard-isolation.sh` exit 0,
  PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines. Epic **fully dispatched**:
  152 closed / 0 open / 5 in_progress, every remaining child with a live crew.
  Spawned 1 crew (ka-0000007123, `kd close` merge→close-race live re-check;
  gap re-verified at HEAD by me — `close_mr_live.go` shipped with unit tests
  only, 0 hits in EITHER suite). **Structural repeat: an `type=advice` bead
  (ka-0000000850) was again parented into this goal-bearing epic**, which makes
  "all children closed" unreachable by construction — advice must stay `open`
  to keep being delivered by `gb prime`, so closing it to drain the epic would
  silently stop delivering the two-suite rule. Fixed the same way as cycle 14:
  removed the EDGE (`kd dep remove … --type parent-child`), not the bead. This
  is now the 2nd occurrence; nothing enforces the `kind=issue`-children norm
  (advice ka-0000002123), so check it at the top of any cycle reporting
  "N children remain". **Coverage-gap pass filed NOTHING — the guardrail fired
  negatively 8/8.** Every non-test merge in the last 36h already had coverage,
  incl. two that looked like textbook gaps: ka-0000007533 (spend read bypasses
  `proxy_url`) is pinned by **Leg D** of `test-session-spend-attribution.sh`
  (`TestSpendReadBaseURLForAccount_IgnoresProxyURL`) — the one leg that attaches
  a proxy to an account; and ka-0000004753 (advice-lint prerun-exempt) was
  backfilled hours later by `test-advice-lint-undeliverable.sh` (f0ce7122d).
  A grep scoped to the feature NAME rather than the runtime SYMBOL would have
  manufactured duplicate work on both.
- 2026-08-05 (cycle 34, gardener ka-0000004141): 178 modules / 192 scripts
  = 176 wired + 16 excluded, **drift 0** (`test-shard-isolation.sh` exit 0
  — the authority, PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines.
  **FINDING — there are TWO e2e suites and a gap check must grep both.**
  Cycle 33 filed ka-0000007510 as a HEAD-verified gap ("Home index rail
  event stream, 7 per-term greps -> 0 e2e files") and recorded the broader
  claim that "the beads-viewer Home/index page has no e2e module at all".
  Both were **false**: 406 lines of CI-enforced coverage already existed in
  `gardenio/tests/beads-viewer-e2e/tests/index_home_rail.spec.js`
  (585fe2690 / !6745, for its own already-closed sibling ka-0000007613),
  asserting all four scoped legs — cross-source newest-first interleave,
  per-event kind class + glyph, the cap, and the squawk-only fallback —
  plus de-dup and a no-events leg. The per-term greps were correct; their
  **scope** was wrong (only `tests/e2e/scripts/`). The Playwright suite is
  deliberately *not* `run_module`-wired and so is invisible to the module
  count and to drift accounting, which is exactly why it gets forgotten —
  but `test-beads-viewer-e2e` is in the `.rwx/ci.yml` task lists (lines 25
  + 63), i.e. it runs on **every** pipeline. Closed ka-0000007510 as a
  duplicate with the evidence (no module written) and filed advice
  **ka-0000000850** (`role:crew-e2e` + `role:gardener`) carrying the
  two-tree grep command, since nothing enforces this. Merged **1 stranded
  green MR**: !6781 (ka-0000005743, claudemore proxy-spend transparency) —
  non-draft, pipeline 2735308858 green, bead **stale** under
  `gb news --stale-only` (crew despawned without merging). Reviewed: the
  runtime change is a behavior-preserving lift of an inline closure into
  `resolveSpendReadBaseURLForAccount`, and the MR carries a **mutation
  proof** that all five pre-existing spend tests stay GREEN under the
  regression while the new leg goes RED — the gap was real. Merged,
  linked, closed. Spawned **1** crew on **ka-0000006323** (C3 card-graph
  `chain_spec` round-trip), the one child re-confirmed real at HEAD across
  *both* suites (`chain_spec`/`ChainSpec`/`gardener_successor` = 0 files);
  brief carries the two-suite rule and tells it to prefer a Playwright
  spec. Coverage-gap pass filed **1** new child **ka-0000007123** — `kd
  close`'s live-GitLab re-check that acquits a just-merged MR (79bc6c346 /
  ka-0000004113, new `close_mr_live.go`), unit-tested only. Ranked because
  the gardener **hit the acquittal path in production this cycle** closing
  ka-0000005743 seconds after self-merging !6781; every crew on the
  standard self-merge-then-close flow rides it, and the scoped
  degradation legs pin that `mrLiveUnknown` must never fail *open*. Queued,
  not dispatched — 5 crews in flight is the budget cap. Typed `parent:`
  mention **did** materialize the edge (verified with `kd list --parent`),
  consistent with cycle 13's correction; note `kd dep children`
  over-counts by including `assigned` edges (the gardener's own agent
  bead), so `kd list --parent` is the honest child list. No decision —
  nothing this cycle was an architectural fork.
- 2026-08-05 (cycle 33, gardener ka-0000003579): 178 modules / 192 scripts
  = 176 wired + 16 excluded, **drift 0** (`test-shard-isolation.sh` run
  directly — PASS at TOTAL_SHARDS=4/5/8/16); 0 quarantines; 8-of-8 recent
  main pipelines green; **0 open e2e bugs at any priority**. 4 inherited
  children all confirmed **live** via `gb news --active-only` — nothing
  stale to reap. Applied cycle 13's stranded-green-MR check: !6779
  (ka-0000005729) is open but belongs to a *live* crew, so it is
  in-flight, not stranded — no intervention. Spawned **1** crew (4 live +
  1 = budget 5) on **ka-0000003623** (/plans "last change" folds crew
  heartbeats), ranked above ka-0000006323 because the same fold
  re-freshens `sortPlanRows` attention rank — a regression there
  mis-orders what every operator reads first, not one card. Both open
  children re-verified at HEAD by the gardener before dispatch: runtime
  present (`plans.go:1098`/`:1886`; `gardener_successor.go:195`), e2e refs
  **0**. Coverage-gap pass over 7 days of runtime merges filed **1** new
  child, **ka-0000007510** — the Home index rail's per-agent event stream
  (18bfb1336/ka-0000003393) merges squawks *and* bead comments
  newest-first, and all 7 per-term greps
  (`buildAgentEventStream`, `index-active-card-event`,
  `recentCommentsByAgent`, …) return **0 e2e files**; unit-tested only.
  Broader finding recorded on the bead: the beads-viewer **Home/index
  page has no e2e module at all** — no `test-home-*.sh`/`test-index-*.sh`
  exists, so this is the first. The guardrail again paid for itself on
  two near-misses: claudemore REPLAY (66d26c32a) *looked* uncovered but
  `test-claudemore-replay-store-gate.sh` landed the same day and
  agentchoice/hooks already drive real cassette hit/miss; and
  `plans-mr-promote-failed` (0b873c0f1) is explicitly asserted by
  test-plans-mr-overflow.sh case 5 (inline-failed promotion). Neither was
  filed. Parent edge on the new child verified with `kd dep children`
  per cycle 12's finding that a typed `parent:` mention does not create
  it.
- 2026-08-05 (cycle 32, gardener ka-0000005758): 178 modules / 192 scripts
  = 176 wired + 16 excluded, **drift 0** (`test-shard-isolation.sh` run
  directly — PASS, partitions clean at TOTAL_SHARDS=4/5/8/16); 0
  quarantines; 8-of-8 recent main pipelines green; **0 open P0/P1 e2e
  bugs**. All 5 in-flight children confirmed **live** via `gb news
  --active-only` — nothing stale to reap, and crew budget exactly full,
  so **0 spawns**: this cycle's whole leverage was the coverage-gap pass.
  Swept 15 runtime merges from the last 7 days; **14 of 15 landed with
  zero e2e files**. Filed 3 HEAD-verified children: **ka-0000004963**
  (P1, SubagentStop forwards the subagent final message — `grep -ril
  subagent` over the whole e2e tree returns *nothing*), **ka-0000003623**
  (/plans "last change" folds crew heartbeats, ka-0000004310/!6757), and
  **ka-0000006323** (C3 card-graph authoring chain_spec round-trip).
  The guardrail earned its keep twice: two further candidates —
  unresolvable `depends_on` (ka-0000002771) and `gate_type` pour
  (ka-0000006567) — looked like fresh gaps by commit shape but are
  **already asserted** in test-formula-lifecycle.sh:1241 and
  test-formula-gate-pour.sh, so they were dropped rather than filed. A
  third nearly slipped through the opposite way: `fan-out` grepped to 16
  e2e files, but per-term disambiguation showed all 16 are **prose
  comments** in unrelated modules and the real terms (`chain_spec`,
  `ChainSpec`, the successor route) are 0 — name-match cuts *both*
  directions, so verify per-term, not per-candidate. Decision = none.
- 2026-08-05 (cycle 31, gardener ka-0000007924): 178 modules / 192 scripts
  = 176 wired + 16 excluded, **drift 0** (`test-shard-isolation.sh` PASS,
  partitions clean at TOTAL_SHARDS=4/5/8/16); 0 quarantines; 10-of-10
  recent main pipelines green. Sprint backlog **fully drained** — 0 open
  work-typed direct children on arrival. Filed + dispatched exactly one
  new gap, and it is a **vacuous-coverage** find worth internalizing:
  **ka-0000000099**. The `crosslink-nowake` leg merged HOURS earlier
  (592028a54, !6771) drives only the *already-flattened* crosslink body —
  which is the Jira **Cloud** shape. Prod runs Jira **Server**, which
  posts the same notice as **wiki markup** with the object phrase inside
  a link (`…mentioned this issue in [a merge request|url] of …`), so the
  brackets split the marker phrase and `gitLabCrosslinkMarkers` never
  matched. That is bug ka-0000006177, still open on !6772 — i.e. a
  brand-new, correctly-motivated, green e2e leg would NOT have caught the
  live bug it was written for. Generalizes cycle 9's *assertion rot*
  lesson one step further: rot is not only about modules going stale
  against changed runtime — a module can be **born vacuous** by driving a
  synthetic input shape rather than the on-the-wire one. New guardrail
  for every crew: **assert against the shape prod actually emits**, and
  when a fixture body is hand-authored, say where the real sample came
  from. The backfill's mandatory mutation test is exactly this — revert
  the flattener and show the wiki case goes RED while the flattened cases
  stay GREEN. Decision = none (no architectural fork).
- 2026-08-05 (cycle 8, gardener ka-0000004979): 176 modules / 190 scripts
  = 174 wired + 16 excluded, **drift 0** (authority:
  `test-shard-isolation.sh`, which also PASSes shard partitioning at
  TOTAL_SHARDS=4/5/8/16); 0 quarantines; 9-of-9 completed main pipelines
  green. Reaped **ka-0000006761** — its crew pod was gone but MR !6762
  had in fact MERGED (c252afaa5, +126 lines in test-escalation-crud.sh,
  already wired at run-shard.sh:2062); the bead sat `in_progress` on
  landed work, holding a crew slot against the cap for nothing. **A dead
  pod is not a failed task — verify the MR before assuming either way.**
  Filed + dispatched **ka-0000002959** (P1, HEAD-verified): GitLab
  crosslink comments must not wake/respawn a Jira-linked agent
  (`jira_webhook.go:447`). `test-jira-conversational.sh` is the
  name-match trap here — its legs are pinned at line 182 and the
  `followup` leg drives only human bodies, so the crosslink path has
  zero e2e despite a file that looks like it covers it. The crew is
  required to include an anti-vacuity positive control (a human body
  containing the verb but not the marker object must still respawn) —
  without it a globally-dead respawn path passes the no-wake assertion
  vacuously. Filed **ka-0000004272** (P2, HEAD-verified, not yet
  dispatched): `gb audit` has ZERO e2e of any kind; its resolver is
  well unit-tested but the RunE — expansion loop, `note:` line, `--json`
  `sessions` key, and the non-zero-exit-on-empty guard at audit.go:433
  — is untested, which is the half where a regression is invisible.
  Also removed a `related` edge (advice ka-0000002123 → this epic):
  **`kd dep children` lists `related` edges as children**, so an
  audit that trusts it over-counts the backlog, and the advice bead's
  own prescribed `--type parent-child` removal would have errored on
  it. Edge removed, advice left open and still delivering. 5 crews in
  flight, at the cap. Decision = none.

- _(11 May-2026 entries from the original sprint — 2026-05-03 seed through
  2026-05-04 cycle i — pruned 2026-08-06 by cycle 64; superseded by the
  60+ v2 entries below. Full text in git history.)_
- 2026-08-05 (v2 gardener ka-0000005355, cycle 1): 137 wired modules /
  151 scripts / 16 documented-hermetic exclusions / 0 quarantines.
  **Drift = 0** (137 wired = 135 in `scripts/` + `edge-auth/test-edge-
  auth.sh` + `verify-outbox-rules-json.sh`; 151 − 16 = 135 — exact
  match, so the raw module-vs-script heuristic in this doc reads a
  false +14). Re-audit gate ka-0000007847 **already closed** and
  produced HEAD-verified report ka-0000006661 / doc ka-0000004296:
  1745 merged MRs in a 6wk window, 1474 runtime, 6 parallel per-
  subsystem auditors, 9 real gaps filed and deduped against 47+32
  prior children. Headline: gaps are narrow and specific, not broad
  neglect — and the coop "below-floor 55.3%" alarm is a **line-
  coverage artifact** of `mux::run()` bootstrap + web-dist assets,
  NOT a behavior gap (throttle/poller/error-classification all unit-
  tested). That retires the coop-backfill priority carried in the
  gardener prompt. Spawned 5 crews (budget full) on the audit's top
  ranks: ka-0000006937 (P1 lambda_microvm execution backend),
  ka-0000006088 (P1 gitlab-bridge reopen-sweep), ka-0000007263 (P1
  dark-pod Jira write-token fence), ka-0000005532 (P2 repro-cockpit
  deployed service), ka-0000000188 (P2 gb salvage S3 round-trip).
  20 epic children open. No decision filed.
- 2026-08-05 (v2 gardener ka-0000005355, cycle 2): 139 wired modules /
  155 scripts / 16 exclusions / 0 quarantines / 9 of last 10 main
  pipelines green. **Drift = 0**, and this cycle establishes *how to
  measure it*: run `test-shard-isolation.sh` and read its
  `exclusion-list complete` line. Do NOT use `grep -c 'run_module'` —
  it counted 138 while only 136 scripts were wired, because several
  scripts are wired under more than one `run_module` name (multi-mode).
  The lint is the only authority; the raw-grep heuristic still in this
  doc's cycle procedure has now produced a false reading two cycles
  running.
  Cycle-1's 5 crews all landed: MR !6612 (dark-pod Jira fence,
  ka-0000007263), !6614 (lambda_microvm execution backend +
  `agent_state` enum fix, ka-0000006937 + ka-0000000585), !6507
  (E-PROV provenance, ka-0000005448) — 5 children closed.
  **Self-merge is available to the bot** — the cycle's biggest finding.
  Two crews independently filed operator gates (decision ka-0000005887,
  escalation ka-0000004874) on the premise "the bot cannot self-merge
  protected main". It is false: `glab api --method PUT
  projects/:id/merge_requests/:iid/merge` succeeds and the API reports
  `user.can_merge=true`. The gardener merged !6612 that way, then
  resolved ka-0000005887 = merge; the crew rebased past a conflict and
  self-merged !6614 unaided. ka-0000004874 was worse — it asked a
  maintainer to merge !6507, which had *already merged* 18 min after
  filing, then burned 2 auto-pingbacks on the operator queue for a
  no-op; closed `--status success`, and its P1 parent ka-0000005448 was
  HEAD-verified (module + probe on main, wired at run-shard.sh:1512)
  and closed. Enforce advice ka-0000000752 on every crew: attempt the
  privileged action, read the REAL error, and only then escalate.
  **`kd list --parent` is transitive** — it returned 29 "children"
  including 4 `e2e-cockpit-probe` fixture beads that are actually
  grandchildren (children of in-flight crew bead ka-0000005532). Use
  `kd dep children <id>` for direct edges. Combined with cycle 1's
  20-row default-paging trap, any epic count you publish needs BOTH
  `--limit 200` and a direct-edge check.
  Spawned 3 crews, each on a gap the gardener re-verified at HEAD first
  (per the name-match!=coverage guardrail): ka-0000003089 (auto-close
  cascade — no `test-auto-close*.sh` exists and no script asserts
  auto-close at all), ka-0000006112 (escalation auto-reaper — zero
  `reap` hits across all 5 `test-escalation-*.sh`), ka-0000007500
  (gardener-epic-reaper claudeless — the standing-gardener drain trap
  this sprint exists to fight). 25 epic children open. No decision
  filed.
- 2026-08-05 (v2 gardener ka-0000000168, cycle 3): 142 modules / 156
  scripts / 16 exclusions / 0 quarantines / **10 of last 10** main
  pipelines green / 0 open e2e bugs. **Drift = 0**, measured cycle 2's
  way (`test-shard-isolation.sh` → `exclusion-list complete`: 156 on
  disk = 140 wired + 16 excluded; 142 distinct modules because some
  scripts are multi-mode). The raw `grep -cE 'run_module'` heuristic
  read 142 here and would have looked right by luck — it is still not
  the authority.
  **The stale-merge-escalation failure repeated a third time.** Cycle 2
  recorded it twice (ka-0000005887, ka-0000004874); this cycle found
  escalation **ka-0000005673** still open on the operator queue asking a
  maintainer to merge !6612 — an MR `glab mr view` reports as `state=
  merged`, whose child ka-0000007263 was already closed. Closed it
  `--status success` as a no-op. The false premise is verbatim the same
  each time ("the gardenio bot is Developer (30) and cannot merge
  protected main"), so it is not a one-off crew slip — advice
  ka-0000000752 is not reaching crews. Root cause found (see next
  para): it structurally cannot.
  **`crew-e2e` is not a real role** — filed as bug **ka-0000002951**.
  Every `gb spawn --role crew-e2e` this whole sprint has warned
  `unknown role "crew-e2e" — not a builtin and no type:role bead; the
  pod will spawn on global defaults`. `gb role list` has crew,
  crew-fable, crew-lite, crew-ultralite, devsecops-triage-crew and
  **gardener-e2e**, but no crew-e2e — the gardener half of the pair
  exists and the crew half never did. Advice is delivered by role
  label match (ka-0000006660), so nothing labelled `role:crew-e2e`
  reaches a backfill crew. Audited the blast radius: all 11
  `role:crew-e2e` advice beads are **closed** (ACME-era k6/rwx
  material), so nothing live is being dropped *today* — but it means
  the self-merge rule cannot be fixed by filing advice at that label,
  which is exactly what a future gardener would try. Scaffold the role
  first. Same audit found 2 of the 4 advice ids this gardener's own
  prompt says to "respect" (ka-0000006307, ka-0000003449) are closed.
  Spawned 1 crew (4 already in flight from cycle 2, budget 5):
  ka-0000005996 (/costs spend page + coop :9093 live-usage seam),
  chosen because coop is the only below-floor component (55.3% vs
  controller 76.4% / kbeads 71.8%; weakest crate `mux` 42.4%) so it is
  the one backfill that moves a *required* Coverage gate. HEAD-verified
  before dispatch and the verification is recorded on the child — note
  the trap it caught: `grep -rln 9093 tests/e2e/` "hits"
  test-account-auth.sh and test-beads-crud.sh, but both are `local
  port=19093`, an unrelated forward port. Also confirmed
  test-agent-cost-context.sh *seeds* `context_tokens=128000` by hand,
  so it tests the type:agent schema, not the statusreporter seam that
  computes it. 22 open task children (38 open direct children total —
  the other 16 are agent/session/mail/trigger bookkeeping, not work).
  No decision filed.
- 2026-08-05 (v2 gardener ka-0000001570, cycle 4): 142 modules / 156
  scripts / 16 exclusions / 0 quarantines / 8 of last 9 main green (the
  red one already had a triage agent — CI/CD gardener's lane) / 0 open
  P1 e2e bugs. **Drift = 0**, measured the authoritative way (run
  `test-shard-isolation.sh`, read its `exclusion-list complete` line:
  156 on disk = 140 wired + 16 excluded). `grep -c run_module` reads
  142 here and would look right by luck — still not the authority.
  **ROOT CAUSE FOUND for the stale-merge-escalation failure that
  repeated three cycles.** Cycle 3 got half of it: `crew-e2e` had no
  type:role bead (ka-0000002951). That bug is now closed, the role bead
  exists, and this cycle's 3 spawns resolved with no "unknown role"
  warning. But the channel was still empty — `kd list --type=advice
  --status=open -l role:crew-e2e` returned **zero beads**. And the
  canonical self-merge rule **ka-0000002230 is labelled
  `scope:on-demand`, so it is never auto-delivered to any session** — a
  crew only sees it if it already knows to ask, which a crew holding
  the false premise never will. Unreachable by both paths at once; that
  is why the same verbatim claim ("bot is Developer/30, cannot merge
  protected main") recurred three times. It was never a crew-quality
  problem. FIX: filed **ka-0000005776** (P1, `role:crew-e2e`) — the
  role-scoped delivery, carrying the runnable proof (`glab api
  .../merge_requests/<iid> | jq .user.can_merge` => true) and the three
  incident ids, so the next crew checks instead of assuming. Verified
  it is now returned by the `role:crew-e2e` advice query; also mailed
  both live crews so this run benefits.
  **Merged !6623** (repro-cockpit deployed-service e2e, ka-0000005532):
  green and mergeable, but its crew had despawned WITHOUT merging,
  leaving the child open+unassigned with a finished MR sitting there.
  Merged it (7107723), HEAD-verified `test-repro-cockpit.sh` on
  origin/main + wired at run-shard.sh:1259-1260, closed the child.
  "Crew delivers, then evaporates before merging" is the same lost-work
  shape as the stale escalations, failing one step earlier — worth
  watching. That crew also surfaced+fixed a real defect (ka-0000004149:
  the fail-closed SSRF/PHI host allowlist, 47 unit tests and a
  "load-bearing" docstring, had **zero production call sites**).
  Caveat: the module self-SKIPs on every ephemeral e2e namespace
  because `reproCockpit.enabled` lives only in gardenio-rwx.yaml, so
  this coverage does not gate CI yet. !6625 (ka-0000007500) is green
  and mergeable but its crew is still LIVE — left the merge to it
  rather than stealing it.
  Spawned 3 crews (budget 5, 2 already in flight), each HEAD-verified
  by CONTENT-grepping for the feature's real identifiers, with the
  verification recorded on the child: ka-0000003378 (beads-viewer
  /people + /u/:slug + prefs — runtime at agents.go:177,1360 and
  gardener.go:98; zero e2e hits), ka-0000008183 (agent-feedback write
  path — runtime at bridge/bot_agent_feedback.go; the only e2e
  "feedback" hit is schema seeding), ka-0000006362 (trigger action
  create_bead fidelity — runtime at triggers.go:37,390-395; every e2e
  `create_bead` hit is the local `create_bead_http` helper or the
  type:trigger enum, nothing asserts the action).
  **STRANDED DELIVERY is a recurring pattern, not a one-off** — three
  children this cycle were DONE (code merged, module wired) but still
  sat `open` because the crew despawned without closing the bead:
  ka-0000005532 (!6623 green but unmerged — I merged it), ka-0000001716
  (!3154 **already merged**; assertions in tests/e2e/edge-auth/, wired
  run-shard.sh:1213), ka-0000003321 (!3155 **already merged**;
  test-gitlab-bridge.sh Scenario 7, tagged with the bead id at line
  434, wired run-shard.sh:1536). The last two were listed in the epic's
  own description as "verified at sprint start — runtime merged, no
  matching e2e"; they have had matching e2e for a while. This is the
  **mirror image of name-match!=coverage**: that guardrail stops you
  writing a test that exists, this one stops you *tracking* work that
  is finished — both come from trusting bead text over HEAD. Sweep
  recipe, cheap and it found 3 hits: (a) for children with `mr_url`,
  check `glab api …/merge_requests/<iid> | jq -r .state`; (b) for
  children without one, `git log origin/main --grep=<bead-id>`. Both
  sweeps are now clean. Note `no-auto-close` (correctly) keeps the
  drain cascade off this epic, which also means nothing else will ever
  reap these — closing them is the gardener's job. Backlog **20 open
  task/bug children** (23 at cycle start). No decision filed.
- 2026-08-05 (crew ka-0000005996): +2 modules — **147 modules / 161
  scripts / 16 exclusions** at the rebased base (`test-shard-isolation.sh`
  → `exclusion-list complete`, the authority; 161 on disk = 145 wired +
  16 excluded). Note these totals include cycle 4's own landings, not
  just this crew's +2: measured 144/158 pre-rebase and 147/161 after
  rebasing onto c74dc3db3, which is the number to compare forward from.
  New here: `costs-spend-page` (hashes to shard 1) and
  `coop-live-usage` (shard 3). Neither is pinned — both are far under
  the 50s pin threshold (coop-live-usage 4s, costs-spend-page ~1s).
  Two corrections to the dispatch brief, both found by running the
  code rather than grepping it:
  1. **The proposed `[data-chart-catalog]` assertion was stale.** That
     inline-ECharts wiring was removed from /costs by the June Grafana
     swap, and `costs_test.go:440-447` actively asserts its ABSENCE.
     Asserting its presence would have red-gated a correct build. The
     module now asserts the absence (on a deployed viewer, which the
     unit test cannot do) plus the four Grafana panels by title.
  2. **`gardenio_agent_llm_request_latency_seconds` cannot be driven by
     claudeless at all** — filed product bug **ka-0000005556**.
     `parse_rfc3339_ms` (usage.rs) opens with `strip_suffix('Z')?`, so
     it requires a trailing `Z`; real Claude writes `…095Z` but
     claudeless writes `…+00:00`, so every entry parses to None and the
     histogram never observes. Measured: 4 tool calls / 9 LLM
     round-trips gave tool_call `_count=4` but llm_request `_count=0`
     while the underlying log gaps were a real 3ms/15ms/23ms. The
     module asserts that histogram's SHAPE and skips the value with the
     bug id, so it tightens for free once the bug lands.
  Two things worth reusing. **A behavioral assertion beats a marker
  grep**: the rolling-window headline $/h and the avg-since-midnight
  computation it replaced both render as `$N/h`, so no string can tell
  them apart — but on a young UTC day with a $0.00 settled total, a
  midnight average is necessarily $0.00/h while a rolling rate stays
  positive. Observed live: `Today $0.00` at `25% of day elapsed` with
  `rate (24h avg) $78.80/h rolling`. The module also cross-checks
  `projected ≈ rate × 24` (advice ka-0000005924). Every assertion was
  **negative-controlled** — mutating the rate to $0.00/h flips the
  verdict to FAILED, and a wrong expected tool count reds the module —
  because an assertion never observed failing is not evidence.
  **`CLAUDE_CONFIG_DIR` is load-bearing for any coop+claudeless
  module**: without it the transcript lands where coop's Tier-2 log
  discovery never looks, and every `gardenio_session_*` counter silently
  reads 0 while the Tier-1 tool-call histogram still works — a
  half-dead state that looks like a product bug and is not. Such a
  module must also run under a **scrubbed `COOP_*` env**, or it
  attempts an S3 resume of the agent pod's own live session and exits
  immediately.
- 2026-08-05 (v2 gardener ka-0000007838, cycle 5): **147 modules / 161
  scripts / 16 exclusions / drift 0 / 0 quarantines** — unchanged from
  cycle 4's landing (authority: `test-shard-isolation.sh` →
  `exclusion-list complete — 161 on disk = 145 wired + 16 excluded`).
  5 crews live at the cap, so this cycle was a **merge-and-unblock**
  pass rather than a dispatch pass. Merged 2 MRs and closed the loop
  on a product bug the suite had been routing around:
  1. **!6625** (`test/ka-0000007500`) — gardener-epic-reaper claudeless
     scenario. Verified before merging that it correctly ships NO
     `run_module` line: `test-claudeless.sh` glob-discovers
     `claudeless/*.toml`, so a scenario is wired by existing. Module
     count is unchanged by design — a claudeless scenario raises
     coverage without moving the module counter, which is worth
     remembering before someone reads a flat count as a no-op cycle.
  2. **!6631** (`fix/ka-0000005556`) — `parse_rfc3339_ms()` now splits
     the zone designator and normalizes any numeric `±HH:MM` offset to
     UTC instead of requiring a `Z`. This is the product bug cycle 4's
     crew filed from the `coop-live-usage` fixture.
  **The consequence worth acting on:** that fix retires a documented
  skip. `run-shard.sh:1718` and `test-coop-live-usage.sh:449` skip the
  `gardenio_agent_llm_request_latency_seconds` assertion BY VALUE
  because claudeless writes `+00:00`; the module even self-narrates
  `"ka-0000005556 appears fixed, tighten this assertion"` at line 447.
  With !6631 on main that skip is now stale, and the module's strongest
  assertion is still switched off — the registered-but-dead shape that
  ka-0000004093 already cost us once. Filed **ka-0000001660** (child of
  ka-0000000958) to flip it to an exact-count value assertion and
  dispatched a crew. The `cost_usd` skip beside it stays — that one is
  still real (claudeless emits no LiteLLM `response_cost` key).
  Also reaped stale in-progress **ka-0000005556** (assignee agent
  despawned after pushing a green MR nobody merged — the MR sat green
  while its bead read in_progress, which is the failure mode
  `gb news --stale-only` exists to surface).
- 2026-08-05 (v2 gardener ka-0000005328, cycle 6): **147 modules / 161
  scripts / 16 exclusions / drift 0 / 0 quarantines** / 10-of-10 recent
  main pipelines green. Authority as always is
  `test-shard-isolation.sh` (`161 on disk = 145 wired + 16 excluded`),
  never the `run_module` grep — which reads 147 here because
  multi-mode scripts wire under several names.
  Cycle 5's two crews both landed and self-merged: **!6639**
  (agent-feedback write path, ka-0000008183) and **!6636**
  (cred-rotation-resume, ka-0000006965). Dispatched 3 crews, each gap
  HEAD-verified by me first: ka-0000006403 (`gb mr skip/unskip` —
  runtime at `cmd/gb/mr.go:206,227`, zero e2e refs) and ka-0000005667
  (edge-auth box-auth — unit + helm tests exist, no e2e; crew told to
  target only the live wiring those cannot reach, not to duplicate
  them).
  **The find worth carrying forward — a test fixture was spawning real
  pods.** `test-auto-close-cascade.sh` scenario 4 closes a *decision*
  fixture; the slack-bridge's `handleClosed → nudgeAgent →
  respawnAgentForDecision` path has **no `isE2EFixture` guard**, so
  each close spawned a genuine `crew` pod in project `e2e-test`. Those
  pods hold no git credentials, so they CrashLoop on
  `clone … could not read Username`, restart, and respawn. Observed
  live: **12 pods, 20-23 restarts each, ~24 CPU / 96Gi held across 12
  nodes, while 34 pods sat Pending** on `Insufficient cpu/memory`.
  Their env (`BOAT_PROMPT=You have been respawned because decision
  ka-0000005989 was resolved…`) names a decision that now **404s** —
  the cleanup trap deleted the fixture, the pod outlived it. 11 phantom
  `type=agent` beads (`project=e2e-test`, `agent_state=working`) are
  the matching residue.
  The module's scenario 5 carries a careful four-mechanism
  inert-by-construction argument — but for its *trigger* fixture only.
  `mk_bead` applies `fixture:e2e:` to **exactly one** bead (line 498);
  scenario 4's decision inherits none of that cover, and
  `decisions.go` never calls `isE2EFixture` at all. It amplifies
  because `nudgeAgent` respawns *when the nudge fails*, and the
  fixture's `KD_ACTOR` is not a live agent — so the nudge always fails
  and the respawn always fires. **One fixture close = one permanent
  pod, and the module is shard-resident (`run-shard.sh:1423`).** Filed
  P1 **ka-0000000809** + crew. Read this next to !6636, merged the same
  hour: that MR widened cred-rotation to ACCEPT `queued`/`rate_limited`
  as environmental — the controller's pool-exhaustion escape hatch,
  i.e. precisely what this leak's capacity starvation looks like from
  inside a test. If the leak is the source, the suite has been taught
  to tolerate the symptom of a live bug; re-tighten that assertion once
  ka-0000000809 is fixed.
- 2026-08-05 (v2 gardener ka-0000003081, cycle 7): **163 scripts on disk
  = 147 wired + 16 excluded / drift 0 / 0 quarantines** / 8-of-8 recent
  main pipelines green; 149 `run_module` entries partition cleanly at
  TOTAL_SHARDS=4/5/8/16. Merged **!6640** (ka-0000003378, beads-viewer
  `/people` + `/u/:slug` + profile-prefs, 5 specs / 32 tests) — its
  crew had gone `idle` after 207 turns holding the MR **green,
  non-draft and mergeable**, i.e. finished the work and stopped one
  step short of landing it. *An idle crew is not necessarily a failed
  crew — check for a mergeable MR before reaping or re-dispatching
  one.* Dispatched 3 crews, each gap grep-verified at HEAD first:
  ka-0000007821 (`:::grafana` directive + `gb grafana render` — zero
  hits across `gardenio/tests/`; the two name-matching
  `test-grafana-*.sh` contain neither), ka-0000000439 (`/decisions`
  viewer-owned scoping — the repo's only `?all` coverage is
  `maps.spec.js:84`), ka-0000004828 (freeze/thaw net-new guards —
  `test-gvisor-{freeze-interrupt,renudge-thaw}.sh` match on NAME only;
  zero hits for duplicate-freeze/idempotency/sidecar-barrier).
  **Two traps that cost this cycle real reasoning, both worth
  inheriting.** (1) *The gardener prompt's coverage numbers are a stale
  snapshot.* It still directs the gardener at "coop 55.3%, the only
  below-floor component, mux 42.4%" — fixed six days earlier by
  ka-0000000457 / **!6387** (commit `20334b93b`): mux → 61.13% line,
  no gardenio component is now sub-floor. A gardener trusting its prompt
  would dispatch a crew to redo merged work; re-measure or read the
  closing bead before acting on any number a prompt hands you.
  (2) *This epic's child list is ~40% non-work beads.* `kd dep
  children` returns 27 non-closed, but only **17 are work** (16 task +
  1 bug) — the rest are `agent`/`session`/`mail`/`advice` beads
  auto-parented by the spawn+mail machinery, including the live
  gardener's own agent and session beads. That reads like a
  self-referential deadlock against the "all children closed" terminal
  goal, and it is **not** one: the trigger's preflight is
  type-filtered (`types:[task,bug,epic]`), so dormancy is safe and no
  fix is needed. When counting this backlog, filter to
  task/bug/epic — `kd list --parent` does, `kd dep children` does not.
  Wiring rule the two harnesses do *not* share, since crews keep
  tripping it: `tests/e2e/scripts/test-*.sh` needs an explicit
  `run_module` line (test-shard-isolation.sh hard-fails CI on an
  unwired script), while `tests/beads-viewer-e2e/tests/*.spec.js` is
  auto-discovered via `playwright.config` `testDir:'./tests'` — which
  is why !6640 needed no wiring at all.
- 2026-08-05 (v2 gardener ka-0000007810, cycle 8): **164 scripts on disk
  = 148 wired + 16 excluded / drift 0 / 0 quarantines** / **10-of-10**
  recent main pipelines green; 150 `run_module` entries partition
  cleanly at TOTAL_SHARDS=4/5/8/16. Inherited 4 live crews
  (ka-0000000271, ka-0000007821, ka-0000000439, ka-0000004828), all
  genuinely `working` with sub-minute heartbeats — checked against
  cycle 7's idle-crew-with-mergeable-MR lesson, none applied. Dispatched
  1 crew to fill the 5th slot: ka-0000000091 (agent-card `/agents`
  prewarmed suppression + reaper-closed rendering), already
  HEAD-scoped by cycle 5 down to "extend test-agents-live-view.sh, do
  not add a module."
  **Cleared the capacity leak this epic had been carrying.** Reaped
  ka-0000002290 myself (cluster-ops, no code push): 12 CrashLooping
  `*-89v-autoclose-*` fixture pods at 2 CPU / 8Gi each — **~24 CPU /
  96Gi of starved capacity** — plus their phantom `agent_state=working`
  beads. Cluster-wide CrashLoopBackOff is now **0**. Three findings
  worth inheriting. (1) *Ordering:* use `gb agent kill <bead>
  --override-owner`, never a raw `kubectl delete pod` — the pod is
  reconciler-owned, so deleting it while its agent bead is open just
  gets it re-created next tick. Close the bead and the pod deletion
  follows (11 of 12 gone inside 60s). (2) *Never trust a hand-written
  residue inventory.* The bead listed 11 beads for 12 pods; the 12th
  (`-8795`) did have a bead, **ka-0000000702**, and it survived the
  first sweep. It was invisible to `kd list` because these phantoms
  live in project=`e2e-test`, so every project-scoped query silently
  omits them — `kd search` found it. Enumerate fixture residue from
  the POD side. (3) The leak ran for 163m *after* its fix (!6644)
  merged and produced no new pods, so ka-0000000809 is confirmed
  holding — the residue was purely historical. Consequence for the
  suite: !6636 widened cred-rotation to accept `queued`/`rate_limited`
  as environmental, i.e. it taught the suite to tolerate precisely this
  starvation. ka-0000006965 (re-tighten) is now actionable and left to
  the live crew already inside that module rather than raced.
  Also closed 3 `type=mail` children (ka-0000004882, ka-0000003614,
  ka-0000004241) whose recipient agents had closed — undeliverable
  mail parented onto the epic inflates the backlog with beads no crew
  can ever action; their self-merge guidance is durable as advice
  ka-0000005776. Work-typed backlog: **12 direct task children** (8
  open / 4 in_progress, 0 bug/epic). Stating "direct" deliberately —
  `kd list --parent` reports **16** for the same query because it is
  transitive and pulls in 3 repro-cockpit grandchildren plus 1
  status_message bug under in-flight crew beads. Both numbers are
  correct for different questions; only the direct count is *your*
  dispatch frontier, and cycle 2's counting-trap note applies
  unchanged (`--limit 200`, and `kd dep children` for direct edges).
- 2026-08-05 cycle 9: **164 scripts = 148 wired + 16 excluded, drift =
  0** (authority: `test-shard-isolation.sh` run locally, not a grep —
  it also reports 150 distinct run_module names, the multi-mode
  superset of the 148 wired scripts; cycle 2's "grep overcounts"
  caveat still applies). 0 quarantines / **10 of last 10 main
  pipelines green**. Merged both green crew MRs myself (bot CAN merge
  protected main — advice ka-0000005776): **!6653** cred-rotation
  (ka-0000000271) and **!6650** /decisions owner-scope
  (ka-0000000439). !6653 is worth reading as a *repair* rather than a
  backfill: the module had been POSTing to `/api/v1/credentials/reload`
  (endpoint deleted in 6683143cf) and asserting "same host, no pod
  restart" by comparing a hostname the test itself had written — a
  tautology that could never fail. It now observes the controller's own
  transient-vs-terminal exhaustion decision log instead. Lesson for the
  backlog: a wired, green module is not evidence of coverage; assertions
  can rot into no-ops when the runtime they targeted is removed.
  Direct work-typed frontier: **8 open** children going in, 3
  dispatched after HEAD-verifying each gap myself (runtime code
  present + zero e2e references): ka-0000002598 (/u/{slug}/inbox
  4-population merge + cross-user body redaction), ka-0000006640
  (slack channel create/archive/invite + GARDENER_ON_INVITE),
  ka-0000002247 (Jira mention-spawn multi-project routing +
  blocked-project deny). 5 crews in flight = budget. No decision filed.
- 2026-08-05 (cycle 11, UTC 09:1x): 152 distinct modules / 166 scripts
  on disk = 150 wired + 16 excluded / **drift = 0** / 0 quarantines.
  Drift authority is `test-shard-isolation.sh`'s "exclusion-list
  complete" line, which I ran directly — NOT the `run_module` grep
  (that returns 152 because multi-mode scripts are wired under several
  names; see cycle 2's caveat). Harvested cycle 10's three crews: all
  three MRs went green and merged (!6657 ka-0000002247 Jira
  mention-spawn routing, !6659 ka-0000004828 N-container sidecar
  freeze/thaw barrier — both self-merged by their crews; !6658
  ka-0000006640 gardener-on-invite + channel manage error paths —
  merged by me), and all three children closed. Coverage-gap pass over
  the last 7 days of merged MRs found the highest-leverage gap is NOT
  an e2e module: **coop is the only component below the coverage floor
  (55.3%; crate `mux` 42.4% with credential/throttle.rs,
  upstream/poller.rs and lib.rs all at 0%) and had NO open backfill
  child** — every prior coop-coverage bead is closed. Filed
  ka-0000007572 (P1, Rust unit coverage, explicitly NOT an e2e shell
  module, no run-shard.sh wiring) since that is the one gap that can
  red a *required* Coverage/Patch gate. Dispatched 3 crews, each gap
  HEAD-verified by me first (runtime present + zero e2e references):
  ka-0000007572 (coop mux), ka-0000003235 (gardenio_agents_by_activity
  — runtime at reconciler/idle_metrics.go, zero test refs),
  ka-0000005719 (jira_sync deny-by-default allowlist + source:jira-dark
  early-return). Back to 5 crews in flight = budget. Also confirmed
  four more real gaps for the next wave, runtime present and zero e2e
  refs: `gb grafana users list|bump` (!6655), `gb bundle update`
  (!6651), `gb news` alive-freshness bound (!6652), and
  `pipeline_ref_pattern` on the gitlab pipeline trigger (!6601) —
  all four unit-tested only. No decision filed.
- 2026-08-05 (cycle 12, UTC 09:4x): 153 distinct modules / **167
  scripts on disk = 151 wired + 16 excluded, drift = 0** / 0
  quarantines. Drift authority as always is `test-shard-isolation.sh`'s
  "exclusion-list complete" line, run directly. Inherited 4 live crews
  (ka-0000007572 coop mux, ka-0000003235 idle/active metrics,
  ka-0000002598 /u/{slug} inbox, ka-0000005719 jira_sync
  deny-by-default) — all four confirmed genuinely alive via `gb news
  --active-only`, zero stale in-progress to reap. Converted cycle 11's
  four *observations* into tracked children after re-verifying each at
  HEAD myself (runtime present, unit-tested only, zero references under
  `gardenio/tests/e2e/scripts/`): ka-0000004197 (`gb grafana users
  list|bump`, !6655), ka-0000004386 (`gb bundle update`, !6651),
  ka-0000006768 (`gb news` alive-freshness bound, !6652 /
  ka-0000000637), ka-0000002595 (`pipeline_ref_pattern` on the gitlab
  pipeline trigger, !6601). Direct frontier 16 -> 20 open. Dispatched
  the one free crew slot on ka-0000006768, ranked highest of the four
  because `alive` is the liveness primitive every gardener's
  stale-vs-live triage depends on — a silent regression there corrupts
  fleet-wide capacity decisions rather than one feature surface.
  **Gotcha for the next gardener:** a `parent: kd-...` typed mention in
  a `kd create --description` did NOT materialize the parent-child
  edge — all four beads came back orphaned and needed an explicit `kd
  dep add <child> ka-0000000958 --type=parent-child`. Verify the edge
  with `kd dep children` after filing; do not assume the mention took.
  No decision filed.
- 2026-08-05 (cycle 13, UTC 10:1x): 154 distinct modules / **168
  scripts on disk = 152 wired + 16 excluded, drift = 0**
  (`test-shard-isolation.sh` exit 0) / 0 quarantines / 3 inherited
  crews all confirmed live via `gb news --active-only`, zero stale to
  reap. **Found and merged a STRANDED green MR: !6666** (ka-0000002598,
  /u/{slug} inbox) — mergeable + pipeline-green for ~25 min while its
  bead sat `open`/unassigned because the crew despawned without
  merging. Reviewed (test-only), self-merged, linked, closed the child.
  *This is now a standing first-order check: before spawning anything,
  cross-reference `glab mr list` against open unassigned children —
  a stranded green MR is free throughput that costs a whole cycle to
  notice.* Sibling crews merged their own MRs unaided (!6668
  jira_sync deny-by-default), so the self-merge advice ka-0000005776
  is reaching crews — no operator merge-gate filed this cycle, versus
  three cycles running previously. Spawned 2 crews to the budget cap
  (5 in flight), each gap HEAD-verified by me first: ka-0000007260
  (air-gap image-distribution knobs — headline risk is `latestMode`
  silently defeating a digest-pinned bundle) and ka-0000002595
  (`pipeline_ref_pattern`, where the *negative* leg is what guards
  against a runaway trigger). Coverage-gap pass over 3 days of merged
  runtime MRs filed one new child, ka-0000006726: the gardener-delivery
  per-MR dispatch cooldown (ka-0000001101, !6600) and one-shot aging
  nudge (ka-0000007496, !6599) both landed with ~800 lines of Go unit
  tests and zero e2e — and "fires exactly once" is an idempotency
  claim a single-call unit test cannot actually establish.
  **Correction to cycle 11's gotcha:** a leading `parent: kd-...`
  typed mention in `kd create --description` DID materialize the
  parent-child edge this cycle (verified with `kd dep children`). The
  earlier failure was not the mechanism being broken — keep verifying
  the edge after filing, but do not pre-emptively hand-add a dep. No
  decision filed.
- 2026-08-05 (cycle 14, UTC 10:4x): opened at 155 distinct modules /
  169 scripts = 153 wired + 16 excluded, drift 0, 0 quarantines;
  **closed at 157 / 171 = 155 wired + 16 excluded, drift still 0**
  (`test-shard-isolation.sh` exit 0 both times) after merging two
  modules. Dispatched the 2 free crew slots on ka-0000004197 (`gb
  grafana users`) and ka-0000004386 (`gb bundle update`) — both
  re-confirmed at HEAD by me (`grep` under
  `gardenio/tests/e2e/scripts/` returns nothing for either). 5 crews =
  budget.
  **Merged 2 more STRANDED green MRs — !6672 (ka-0000007260 air-gap
  image-distribution) and !6673 (ka-0000006768 `gb news`
  alive-freshness).** Both were complete (module + `run_module`
  wiring), non-draft, `head_pipeline=success`, `can_merge=true`, with
  5x-green / all-legs-green evidence already in their PROOF blocks —
  they just had no one left to press merge. Third consecutive cycle to
  find stranded green work (cycle 13 found !6666), so treat this as a
  standing cycle step, not luck. **The two strand modes differ, and
  `gb news --active-only` alone catches only one:** ka-0000006768's
  crew despawned outright (it surfaced under `--stale-only`), but
  ka-0000007260's crew was still *alive* — it had simply moved on to
  an unrelated bead (ka-0000005146), so the bead read "live crew, in
  progress" while its finished MR sat idle. A liveness check on the
  AGENT is not a progress check on the BEAD. Cheapest reliable sweep:
  for every in_progress child carrying an `mr_url`, read
  `head_pipeline.status` + `draft` + the diff file list directly and
  merge on green regardless of how the assignee looks.
  **Found and cleared a permanent drain trap on the epic.** Child
  ka-0000005776 is a `type=advice`/`kind=data` bead (the "you CAN
  self-merge" rule, labelled `role:crew-e2e`) that a prior cycle
  parented into ka-0000000958. An advice bead must stay `open` to keep
  being delivered by `gb prime`, so it could NEVER close — meaning the
  terminal goal "all children closed" was unreachable by construction,
  and closing it to drain the epic would silently stop delivering the
  rule to every future crew. Removed the parent-child edge (`kd dep
  remove ... --type parent-child`) rather than closing it; the advice
  keeps delivering and the epic can now actually drain. **Norm for
  future cycles: only `kind=issue` beads belong as epic children — never
  parent an advice/config/data bead into a goal-bearing epic.**
  Coverage-gap pass over the 7-day merge window filed exactly ONE new
  child, ka-0000004346: the `/plans` overflow-pill hidden-CI-failure
  signal (ka-0000000387, bb6a1c8c0) shipped `HasFailedHidden` +
  `plan-card-mr-more-failed` with Go projection/render tests only, and
  `plans.spec.js` has **zero** assertions on MR pills or CI status at
  all — the at-a-glance render claim (a plan with failing HIDDEN MRs
  must not read like an all-green one) is the leg a projection unit
  test cannot establish. **The guardrail also did its job negatively:**
  the newest merge (24fd02f9a, Account Capacity dashboard
  ka-0000003179) looked like a textbook gap — new dashboard JSON + new
  vmalert rule, no `gardenio/tests/e2e/` hits — but
  `helm/gardenio/tests/grafana_account_capacity_dashboard_test.go`
  already holds registration, JSON validity, query family, E2E-account
  exclusion, datasource binding, AND the AccountThrottled
  default-off/opt-in-fireable legs via `helmTemplate`. Filed nothing.
  Name-match != coverage cuts both ways — check the *component's own*
  test dir (helm/tests, Playwright specs), not just
  `tests/e2e/scripts/`. No decision filed.
- 2026-08-05 (cycle 15, UTC 11:1x): opened at 158 distinct modules /
  172 scripts = 156 wired + 16 excluded, drift 0, 0 quarantines, 10 of
  last 10 main green; **closed at 173 scripts = 157 wired + 16
  excluded, drift still 0** (`test-shard-isolation.sh` exit 0 both
  times) after !6678 merged. 0 open P0/P1 e2e bugs. All 4 inherited
  crews were alive and working — nothing to rescue.
  **Closed ka-0000004197 (`gb grafana users`) — the strand sweep now
  has a THIRD mode: crew `agent_state=done` but bead left
  `in_progress`.** Cycle 14 catalogued two modes (crew despawned →
  `--stale-only`; crew alive but moved on → looks healthy). This one is
  neither: the agent finished cleanly and self-merged !6678, then
  exited without `kd close`. HEAD-verified before closing:
  `test-grafana-users-cli.sh` on main, `run_module`-wired at
  run-shard.sh:1382-1383, drift 0. Keep cycle 14's rule (read the MR's
  `head_pipeline`+`draft`+diff directly for every in_progress child
  carrying an `mr_url`) and add: also check `agent_state=done` on the
  assignee agent bead. Counter-example from the same sweep — !6680
  (ka-0000002595) looked strandable (`mergeable`, pipeline green) but
  its crew was `working` and had pushed 6 min earlier, so it was
  in-flight, not stranded. Recency of the last branch commit is what
  separates the two; do not merge under a live crew's feet.
  **THIRD counting trap, and this one silently returns ZERO.**
  `kd dep children <id> --json` does NOT emit a `kind` field at all
  (keys are: assignee, bead_id, created_at, dep_type, id, priority,
  status, title, type, updated_at). So the natural
  `select(.kind=="issue")` filter — the obvious way to apply cycle 14's
  own "only kind=issue children" norm — matches nothing and reports a
  **drained epic with 8 live children**. Worse, `dep children` returns
  ALL edge types, not just parent-child (148 parent-child + 15
  `assigned` + 1 `related` + 1 `triggered-by` here), so the agent/
  session beads of every crew ever spawned inflate any raw count. The
  correct projection is
  `select(.dep_type=="parent-child") | select(.status!="closed") |
  select(.type=="task")` → 8. Stacked with cycle 1's 20-row paging and
  cycle 2's `kd list --parent` transitivity, that is three independent
  ways to publish a wrong epic count; **filter on `dep_type` and
  `type`, never on `kind`, and never trust a zero.**
  Also removed a second permanent drain trap of the exact shape cycle
  14 documented: ka-0000002123 (`type=advice`, the "only kind=issue
  beads belong as epic children" rule itself) had been parented into
  ka-0000000958. Removed the parent-child edge, left the `related`
  edge and the bead open so it keeps being delivered. The norm needed
  applying to its own carrier.
  Coverage-gap pass over the 7-day window filed exactly ONE new child,
  ka-0000004966: 724ee0f9c (ka-0000005146) raised the `/feed` mail +
  squawk kind-badge colors to WCAG AA (3.74:1→5.47:1, 3.68:1→5.36:1)
  as a **templates-only 4-line CSS change with zero test files**, and
  `a11y.spec.js`'s hand-maintained `ROUTES` list (spec:85-95) does not
  include `/feed` — even though axe's `color-contrast` rule is
  `serious`, i.e. already inside that spec's `FAILING_IMPACTS`, so the
  route being absent is the only reason the pre-fix ratio went
  uncaught. The list is deliberately not derived from a route registry
  (spec:80), so new routes are never picked up automatically. I also
  recomputed all 11 `.feed-card-*` kind colors against white — every
  one passes AA (worst: `#8250df` at 5.05:1) — so the fix was
  COMPLETE and the bead is purely the missing regression guard; the
  child records that explicitly so a crew does not go hunting for a
  second contrast bug. Vacuity warning carried on the bead: the seeded
  feed must actually render `.feed-card-mail` + `.feed-card-squawk`
  nodes or axe measures nothing.
  Spawned 2 crews into the free slots (ka-0000005192 `gb pool`
  enable/disable RMW-preserve, ka-0000004966 `/feed` a11y), both
  HEAD-re-verified by me first — for the pool bead, zero `gb pool`
  invocations anywhere under `gardenio/tests/`, with the RMW logic live
  at `cmd/gb/pool.go:131,147-188`. 5 crews = budget. No decision filed.
- 2026-08-05 (cycle 16, UTC 11:4x): **175 scripts = 159 wired + 16
  excluded, 161 distinct modules, drift 0** (`test-shard-isolation.sh`
  exit 0), 0 quarantines, 9 of last 10 main green, 0 open P0/P1 e2e
  bugs. Epic backlog is **effectively drained: 141 closed / 5
  in_progress / 1 open** direct `parent-child` task children. All 3
  inherited crews were live and recently active — nothing to rescue
  (`gb news --stale-only` empty).
  **The sprint is one bead from its terminal goal.** The single open
  child is ka-0000007424 (P3 cosign keyless verify), which is
  *deliberately* parked: its meaningful leg (admission DENY on an
  unsigned image) needs Kyverno, and `kubectl get crd | grep -ci
  kyverno` is **0** on prod-gardenio-eks — so it cannot be honestly
  tested in this substrate at any priority. Left open rather than
  spawned; a crew would either block or ship a vacuous green.
  Spawned 2 crews into the free slots, both HEAD-re-verified by me
  first: ka-0000006262 (comment_write.spec.js still carries the
  literal "Author identity is not asserted here" comment and zero
  `X-Actor` under `gardenio/tests/beads-viewer-e2e/`) and ka-0000000981
  (dark-pod POD layer — `test-dark-jira-fence.sh`'s own header at
  :23 states the pod layer has only *unit* coverage, and the module
  asserts the NETWORK leg by containment arithmetic). 5 crews =
  budget. Both dispatches carry an explicit anti-vacuity mandate with
  a named negative control, because both targets are the shape that
  passes when the feature is deleted.
  **FOURTH counting trap — and this one is self-inflicted, recurring,
  and makes the terminal goal unreachable by construction.**
  `gb spawn --task <child-of-epic>` parent-child-links the crew
  **AGENT** bead (`type=agent`, `kind=config`) into the epic. Observed
  5/5 this cycle: 3 inherited (ka-0000001166, ka-0000000444,
  ka-0000005866) plus both of my own spawns (ka-0000002987,
  ka-0000004610) appearing within *seconds* of `gb spawn`; a
  `session:` bead (ka-0000006643) lands the same way. Agent/session
  beads never close on the work lifecycle, so a gardener that spawns 5
  crews/cycle **manufactures 5 new permanent blockers to its own goal
  per cycle** and must hand-detach them forever. Cycle 15 removed one
  instance of this shape (the advice bead ka-0000002123) and read it
  as a one-off; it is not — it is the spawn path. Detached all 6 and
  filed **ka-0000002199** (P2 bug, child of this epic) with the
  suspected fix (attach the agent bead with the `assigned` edge only —
  which is already what `gb crew`/`gb news` resolve through — and/or
  make the daemon refuse a parent-child edge whose child is
  `kind!=issue`, turning ka-0000002123 from advice into an invariant).
  Until it lands, every gardener on a goal-bearing epic must
  post-spawn detach; the workaround one-liner is on the bug.
  Note the counting-trap corollary earned its keep this cycle: two
  derivations disagreed (1 vs 3 open) and reconciling them — rather
  than publishing either — is what exposed the spawn-path edge.
- 2026-08-05 (cycle 21, UTC 14:1x): **182 scripts = 166 wired + 16
  excluded, drift = 0**, 0 quarantines, 9-of-10 recent main pipelines
  green (1 canceled, 1 running). Last cycle's P1 ka-0000006141
  (`gb gardener start` ack-flag gate breaking 7 scenarios + the prod
  Slack epic-mention entrypoint) is **CLOSED** — fixed inside one
  cycle. Epic opened at 7 open/in-progress children, closed at 6 with
  3 crews dispatched.
  **Closed ka-0000002802 as WAI without spawning a crew** — the bead
  offered two outcomes and the right one was decidable by reading the
  file, so dispatching an agent to read it would have been pure
  overhead. `scripts/check-e2e-module-failure-gate_test.sh` is 8
  assertions, not thin: it covers the exact MAX_MODULE_FAILURES
  regression, a *differently-named* count tolerance (closing the
  rename bypass), a comment-only mention that must NOT trip the guard,
  fail-open, a dropped FINAL_STATUS branch, an absent gate block, and
  a live-file check against the real `.rwx/release.yml` (anti-rot).
  The structural argument is the durable part: **a gate whose job is
  to red the pipeline cannot be honestly proven in-shard** — you would
  have to make a real shard fail to watch it fire. Hermetic fixtures
  test the decision function exhaustively and are the better evidence;
  wiring a `run_module` here would raise the module count without
  raising coverage. `test-shard-budget-guard.sh` has the same shape
  (standalone `.rwx/ci.yml` task, not a `run_module` entry), so this
  is an established pattern, not an oversight. Recorded here so a
  future coverage pass does not re-file it.
  **Coverage-gap pass filed 2, and both are the same lesson: I went
  looking for coverage of a NEW FIELD and found the WHOLE COMMAND
  untested.** (1) ka-0000003443 — MR 606ead90a added
  `latest_pipeline_status` to `gb release status --json`; grepping for
  it returned 0, and so did `release status` itself, plus 0 matching
  `run_module` lines and 0 scenarios. That command's own doc comment
  says other agents parse its shape ("add fields additively; do not
  rename"), and release.md:7 makes it a mission KPI — a consumed
  contract with no e2e. Scoped honestly: `computeReleaseStatus` is a
  pure function already covered by 6 unit tests, so the bead directs
  the crew at the glab-stitching/JSON-envelope surface and forbids
  asserting live verdict values (flaky) in favour of key-presence +
  enum membership. (2) ka-0000005221 — MR 571800d2f fixed `kd formula
  update` rejecting any `extends` formula; `extends` has **0 hits in
  all of e2e**. The trap is sharp here: test-formula-lifecycle.sh
  covers `update` (scenario 7, flat formula), `dump` round-trip
  (scenario 8, dump->**create**), and step-chaining — every name
  matches and not one path touched the bug. "chain" in that module
  means chained *steps*, not `extends` inheritance.
  **Methodology, generalized from both:** when a merged MR adds a
  field, grep for the field AND for its containing command/feature.
  The field-level miss is a small gap; the command-level miss is the
  real finding, and cycle 20's version of this lesson (contract
  changes silently breaking *existing* coverage) is its mirror image.
  Both new beads carry an explicit NEGATIVE CONTROL requirement —
  ka-0000005221's especially, since "stopped validating `depends_on`
  at all" is the plausible bad fix that a positive-only test passes.
  Decision = none.

---

## Where to look (quick reference)

| Need                                  | Source                                                                |
|---------------------------------------|-----------------------------------------------------------------------|
| Authoritative module/script counts    | `bash gardenio/tests/e2e/scripts/test-shard-isolation.sh` → the `exclusion-list complete` line. **Never** `grep -c run_module` (overcounts multi-mode wirings) |
| Live sprint-epic backlog count        | `kd dep children <epic> --json \| jq '[.[]\|select(.dep_type=="parent-child")\|select(.status!="closed")\|select(.type=="task")]\|length'` — see below |
| Module list / sharding / smoke subset | `gardenio/tests/e2e/scripts/run-shard.sh`                              |
| RWX wiring / dispatch params          | `.rwx/e2e.yml`, `.rwx/smoke-rwx.yml`                                  |
| Active quarantines                    | `.captain/quarantines/gardenio-e2e.yml`                                |
| Live e2e beads                        | `kd list -l e2e --status=open,in_progress`                            |
| Recent main pipelines                 | `glab ci list -R acme/platform/gardenio -r main -P 10`            |
| Recent tag pipelines                  | `glab ci list -R acme/platform/gardenio --scope tags -P 10`       |
| Gardener authority                     | `gb advice show ka-0000007569`                                        |
| Stuck -rc promotion                   | `gb advice show ka-0000004631`                                        |
| HTTP target rules                     | `gb advice show ka-0000006307`                                        |
| Canonical E2E env                     | `gb advice show ka-0000003449`                                        |

### Counting the epic backlog — three traps, all of which publish a wrong number

Every one of these has burned a cycle and shipped a false count in this
ledger. The recipe in the table above avoids all three.

1. **20-row default paging** (cycle 1). `kd list --parent <id> -s open`
   silently caps at 20 rows, so `| wc -l` under-reports any real
   backlog. Pass `--limit 200`.
2. **`kd list --parent` is transitive** (cycle 2). It pulls in
   grandchildren (a crew bead's own children), inflating the count.
   `kd dep children` walks direct edges only.
3. **`kd dep children --json` has no `kind` field, and returns every
   edge type** (cycle 15). Filtering on `.kind` matches nothing and
   reports a **drained epic** — a silent zero, the most dangerous
   failure of the three. And `dep_type` spans `parent-child`,
   `assigned` (every crew agent bead), `related`, `triggered-by`, so an
   unfiltered count is inflated by spawn history. Filter on
   `.dep_type=="parent-child"` and `.type=="task"`.

4. **`gb spawn` parent-child-links the crew AGENT bead into the epic**
   (cycle 16, bug **ka-0000002199**). Every `gb spawn --task
   <child-of-epic>` adds a `type=agent`/`kind=config` bead as a direct
   `parent-child` child — observed 5/5, appearing within seconds.
   `session:` beads land the same way. These never close on the work
   lifecycle, so they inflate the count AND make "all children closed"
   **unreachable by construction** (the failure advice ka-0000002123
   describes). Filtering on `.type=="task"` hides it from your count
   but does NOT unblock the terminal goal — you must actually detach:
   ```
   kd dep children <epic> -t parent-child -s open --json \
     --jq '.[]|select(.type=="agent" or .type=="session")|.id' \
   | xargs -rn1 -I{} kd dep remove {} <epic> -t parent-child --if-exists
   ```
   Run this **after every spawn batch** until ka-0000002199 lands.

   **RESOLVED as of cycle 17 (2026-08-05).** ka-0000002199 is closed and
   the leak is gone: all 10 `agent`/`session` direct children are now
   `closed`, and `kd dep children <epic> -t parent-child -s open` returns
   **zero** non-issue rows — the first cycle needing no detach pass. Keep
   the recipe as a **verification** step (cheap, idempotent) rather than a
   mandatory remediation, and re-open ka-0000002199 if it ever returns
   rows again.

Corollary: **never trust a zero, and never trust a single derivation.**
If a count says the epic is drained, re-derive it a second way
(`kd show <epic>` prints `Children: N`) before acting on it — the
terminal goal depends on this number. When two derivations disagree,
**reconcile before publishing**: in cycle 16 a 1-vs-3 disagreement is
precisely what exposed trap 4 above.
- 2026-08-05 (cycle 17, UTC 12:1x): opened at **175 scripts = 159
  wired + 16 excluded / 161 modules / drift 0**, closed at **176 = 160
  + 16 / 162 / drift 0** after merging !6690 (`test-shard-isolation.sh`
  exit 0 both times), 0 quarantines, 9 of last 10 main green, 0 open
  P0/P1 e2e bugs. Epic: 85 closed / 6 in_progress / **9** true open
  issue-kind `parent-child` children. All 6 inherited crews live
  (kubectl-confirmed Running), so **0 spawned — at/over the max-5
  budget**; this cycle's leverage was merging, not dispatching.
  Merged !6690 (ka-0000006726 dispatch-gate cooldown + one-shot aging
  nudge). Reviewed its production hunks rather than rubber-stamping a
  "test" MR: a `GB_DISPATCH_GATE_NUDGE_AGE` seam whose unset **and**
  unparseable paths both return 0 and fall back to the compiled ~2h
  threshold — fail-safe, so a typo cannot reshape production.
  **TWO NEW TRAPS, both of which nearly produced a wrong action.**
  (a) **The Bash tool's cwd persists across calls.** A `cd` into
  `tests/e2e/scripts` for the step-1 drift check silently rebased every
  later relative path: `ls` reported `podmanager/probe.go` and
  `conditions/leaf_script.go` MISSING while `git merge-base` said their
  commits were ancestors of main, and I nearly filed P1 ka-0000002117
  as stale-because-reverted. The same `cd` made `kubectl -n gardenio get
  pods` return nothing, which reads as "6 dead crews" — **agent pods
  live in `gardenio-system`**, and `gb news --active-only` was right.
  Re-verified from the repo root: all 3 runtime files EXIST, `grep -rIl
  script_bead|type:script gardenio/tests/` = 0 each, and the lone `gb
  probe run` hit is a *comment* at
  `test-microvm-execution-target.sh:224` — so ka-0000002117 is REAL and
  is the top pick when a slot frees. Rule: prefer absolute paths, and
  when git-says-present-but-ls-says-absent, suspect your cwd first.
  (b) **A crew mid-flight can outrun your merge poll.** !6688's first
  PUT /merge 405'd; I called it a GitLab stall, only to find the SHA had
  moved `1afbba12 -> ffabf39e` — its still-Running crew pushed a
  fixture-determinism fix at 12:13. The 405 was GitLab protecting me
  from merging the *pre-determinism-fix* version of a module whose whole
  point is not being flaky. Worse, my retry loop merged on
  `opened/ci_still_running`, i.e. would have landed a commit I never
  reviewed mid-pipeline; I killed it. Rule: re-read `.sha` and confirm
  it matches the SHA you reviewed before re-PUTting, and **never merge
  on `ci_still_running`** — green must mean green on the current head.
  Also detached my own session bead ka-0000008063, which `gb spawn`
  parent-child-linked into the epic (trap 4 above, root cause
  ka-0000002199, still in_progress) — note `kd dep children` also
  returns `related`/`assigned` rows, which is why the raw open count
  read 12 against 9 real blockers: **check the edge type per row, not
  just membership.** No 7-day gap pass: cycle 16's 376-commit sweep
  landed 4 minutes before spawn; re-running it would duplicate. Its 4
  recorded VACUOUS surfaces (cosign, warm-pool resume, viewer-login,
  cockpit S3 proxies) are the valuable half — do not re-file. Decision
  = none. Stale crews = 0.
- 2026-08-05 (cycle 18, UTC 12:4x): **177 scripts = 161 wired + 16
  excluded / 163 distinct modules / drift 0** (`test-shard-isolation.sh`
  exit 0), 0 quarantines, 9 of last 10 main green, 0 open P0/P1 e2e
  bugs. Epic: 85+ closed / **9 true open + 5 in_progress** issue-kind
  `parent-child` children. Spawned **1** crew — ka-0000001750 (`gb
  whoami` capability introspection, 998 net-new lines, zero e2e) — the
  4 inherited crews (ka-0000002117, ka-0000002199, ka-0000006726,
  ka-0000006262) were all live per `gb news --active-only`, so 1 slot
  under the max-5 budget. Stale crews = 0. No sprint MRs awaited merge.
  **7-day gap pass: ran the incremental window only** (7 merges since
  cycle 16's sweep 35 min earlier — re-running the full sweep would
  duplicate). Filed **1** new gap, ka-0000006842: claudemore REPLAY
  mode (!6686, 1154 net-new lines) — `grep -rIi 'claudemore.*replay'
  gardenio/tests/` = 0, and the only 2 e2e scripts naming claudemore hit
  on *prose comments* (test-dark-jira-fence.sh:25 cites a Go unit test,
  test-litellm-budget-cap.sh:4 cites a design doc). Neither is
  coverage. Rejected the other 6 merges as non-gaps: !6688-adjacent CI
  work (ka-0000000440, ka-0000000685) **ships its own test scripts** in
  the same commit — check for a co-committed `*_test.sh` before filing
  a CI-surface gap.
  **The constraint that shaped the filing** (write it into the bead, not
  just the title): claudemore is `enabled: false` in
  `helm/gardenio/values.yaml:8317` and is NOT enabled by
  gardenio-rwx-ephemeral-prod.yaml.tmpl, so a live-pod route would red on
  *absence*, not on the feature — the same failure mode that
  ka-0000002902 (reproCockpit) is open to fix. Routed it to
  `helm template` assertions instead (precedent:
  test-airgap-image-distribution.sh), targeting the `$needsStore` gate
  at deployment.yaml:9 — `or recording.enabled (eq mode "replay")` —
  whose regression crashloops every replay pod at boot with no unit
  test catching it, plus a mandatory negative leg. **When a new surface
  is chart-gated off in ephemeral, the gap is real but the *layer* is
  the chart, not the cluster; say so in the bead or a crew will burn a
  cycle discovering it.** Also re-hit trap 4 from cycle 17: `gb spawn`
  parent-child-linked BOTH my session bead ka-0000004463 *and* the new
  crew agent bead ka-0000001814 into the epic; detached both. Note
  `kd dep remove` defaults to `--type=blocks` and a wrong-type remove
  **errors rather than silently no-op'ing** — pass
  `--type=parent-child`. Root cause ka-0000002199 still in_progress.
  Decision = none.
- 2026-08-05 (cycle 19, UTC 13:07): 179 scripts = 163 wired + 16
  excluded, **drift = 0** (authority: test-shard-isolation.sh's
  `exclusion-list complete` line — the bare
  `grep -cE '^\s+run_module '` reads 164 and is NOT a drift input, see
  cycle 2's FINDING 2). 0 quarantines / 9 of last 10 main green. Direct
  open children 11 → 9 (`kd dep children`, not `kd list --parent`).
  **Merged 2 MRs**: !6699 (ka-0000002199, `gb spawn` no longer
  parent-child-links agent/session beads into a goal-bearing epic — the
  root cause of the leak cycles 17/18 had to hand-clean; all 20 RWX
  statuses green before self-merge) and, by crew, !6701
  (ka-0000001750, `gb whoami` e2e = the +1 script). Spawned 3 crews:
  ka-0000002707 (`gb advice lint` undeliverable-capability),
  ka-0000006842 (claudemore REPLAY), and the P1 below.
  **FILED P1 ka-0000006141 — a live regression, prod + CI.** The
  step-3 coverage-gap pass caught ac074d4e1 (merged 12:44Z, ~30min
  before detection) gating `gb gardener start` behind
  `--i-read-the-gardener-instructions` *unconditionally* — the refusal
  at gardener.go:253 precedes the parent-bead lookup and has **no
  env-var/programmatic escape hatch**. Two blast radii: (1) 7 wired
  claudeless scenarios invoke it bare (idempotent, conditions-fan-in,
  stop-wall-clock, mention-entrypoint, progress-prime, stop-no-progress,
  resume-escalate-metrics) and only `gardener-mode-flip-event-fires-once`
  is in SKIP_SCENARIOS; (2) worse, the **prod Slack `@gardenio <epic>`
  entrypoint** — `gardenerStartRunner` (bot_mentions.go:2561) builds
  `{"gardener","start",beadID}` with no ack flag, and its caller only
  special-cases `"already exists"`, so every user's epic-mention now
  hard-errors. **The methodology lesson: the gating commit added +120
  lines of unit tests that all pass**, because they set the flag var
  in-process — neither the .toml callers nor the bridge's argv builder
  are reachable that way. A green unit suite on a commit that adds a
  required flag is not evidence the flag's *callers* were updated; grep
  the callers (including programmatic ones in other packages) whenever a
  CLI contract tightens. Corollary for the coverage-gap pass: walking
  merged MRs is not only for finding *un-tested new surfaces* — it also
  catches *existing* coverage that a contract change silently broke,
  which is the higher-severity find. Decision = none.
- 2026-08-05 (cycle 20, UTC 13:45): 180 scripts = 164 wired + 16
  excluded, **drift = 0** (authority: test-shard-isolation.sh's
  `exclusion-list complete` line, run hermetically this cycle). 0
  quarantines / 9-of-10 recent main pipelines green. Merged **!6709**
  (ka-0000005536, test-repro-cockpit.sh no longer leaks seeded
  `E2ECOCKPIT-*` beads; 20/20 RWX statuses green before self-merge).
  Spawned 1 crew — ka-0000003551 (Jira spawn-ack) — the other 4 slots
  were already held by live crews (ka-0000006262, ka-0000002117,
  ka-0000002707, ka-0000006141); crew cap is the binding constraint, not
  backlog. Coverage-gap pass filed 3 HEAD-verified children:
  **ka-0000005752** (P2, auto-parent suppression), **ka-0000006808**
  (jira reply→mirror edge), **ka-0000002802** (release-gate
  0-tolerance).
  **The cycle's real finding: a merged fix is not a live fix.** !6699
  (ka-0000002199) merged 13:10:41Z to stop `gb spawn` parent-child-linking
  agent/session beads into a goal-bearing epic. At 13:42Z my own spawn
  *still* leaked one (agent bead ka-0000008236 → this sprint epic; removed
  by hand). That is **deploy lag, not a broken fix**: prod
  `gardenio-system` runs `kbeads:2026.217.13`, tag cut 12:26:29Z — 44 min
  *before* the merge; the fix first ships in 2026.217.14/.15, neither
  deployed. Two lessons. (1) Before filing a regression against a
  just-merged fix, compare the DEPLOYED image tag's cut time to the merge
  time — `kubectl get deploy -n gardenio-system -o jsonpath=…` plus the tag
  list is a 2-command check that turns a phantom P1 into a note. (2) The
  gap ka-0000005752 is exactly the kind a unit suite cannot close: the
  offending write is made by the *daemon* answering a real `gb spawn` over
  the type=static actor-inference path (auto_parent.go:68-78), which the
  in-process unit tests stub out — the same shape as cycle 19's
  ac074d4e1 finding, where +120 green unit tests sat on top of a broken
  CLI contract. Epic hygiene: detached 1 non-issue child (session bead
  ka-0000003800) per advice ka-0000002123; the other two apparent
  non-issue children are `related`/`assigned` edges, not parent-child, and
  do not block the terminal goal. Decision = none.
- 2026-08-05 (cycle 22, UTC 14:4x): **182 scripts = 166 wired + 16
  excluded, drift = 0** (authority: `test-shard-isolation.sh`, run
  hermetically this cycle — not the bare `run_module` grep, which reads
  168 here and is NOT a drift input), 0 quarantines, **10-of-10 recent
  main pipelines green**. Epic held at 6 issue-kind children.
  **Spawned 2 crews**, both on gaps cycle 21 filed and this cycle
  re-verified at HEAD before dispatch: ka-0000005221 (formula `extends`
  inheritance — `grep -rn extends gardenio/tests/e2e/` still 0 hits) and
  ka-0000003443 (`gb release status` — still zero e2e for the whole
  command, not just the new `latest_pipeline_status` field). 3 crews
  from cycle 21 remain in flight, so the 5-crew budget is now full.
  **The auto-parent leak is still live in prod and bit this session
  directly**: `gb spawn` parented 4 non-issue beads (3 `type=agent`
  `kind=config`, 1 `type=session` `kind=data`) — including *this
  gardener's own session bead*, stamped 14:37:51Z — into the epic.
  Detached all 4 (`kd dep remove <child> ka-0000000958
  --type=parent-child`; note the argument order is child-then-parent —
  the reverse spelling fails with "no such dependency" and reads like
  the edge is absent). The fix `c8a12e488` merged 2026-08-05 and first
  ships in `2026.217.14`, so this is deploy lag, not a regression of
  ka-0000002199 — but per advice ka-0000002123 every one of those edges
  makes the terminal goal ("all children closed") unreachable by
  construction, since config/data beads never close. **Every gardener on
  a goal-bearing epic must keep post-spawn detaching until prod runs
  >= 2026.217.14.**
  **Coverage-gap pass filed ka-0000000438 (P3)**, and it is a
  *symmetry* find rather than a new-surface one: 7c6aba74a fixed the
  cycle-21 P1 by adding `--i-read-the-gardener-instructions` to all 11
  e2e call sites, which means the gate's *positive* leg is now covered
  everywhere and its *refusal* leg nowhere. The refusal is tested only
  by `TestGardenerStart_RefusesWithoutAckFlag`, which sets the flag var
  in-process — the exact shape cycle 21 identified as unable to see
  caller breakage. Gate *removal* is caught for free (all 11 sites
  would die on "unknown flag"); the uncaught regression is gate
  *weakening* — flag still accepted, refusal degraded to a no-op, every
  scenario still green. **Methodology note: a fix that makes every
  caller pass a new flag silently converts the negative leg into dead
  ground; when you patch callers to satisfy a gate, check whether
  anything still exercises the gate refusing.** Also appended an
  addendum to ka-0000000865 rather than filing a 10th viewer bead:
  iter-23 (777dcdc88) makes `plan-stall-badge-reason` *conditionally
  absent* (orphan-with-live-crew softens the headline and empties the
  face label), so a bare presence assertion would fail and a bare
  absence assertion would pass vacuously — that bead now specifies the
  two-fixture discrimination instead. Decision = none.
- 2026-08-05 (cycle 23): 169 modules / 183 scripts / 16 exclusions /
  **drift = 0** / 0 quarantines / 9-of-10 recent main pipelines green
  (10th still running) / **0 open e2e bugs**. Backlog is nearly drained:
  4 open task children at cycle start, all now crewed. Spawned 3
  `crew-e2e` — ka-0000000438 (`gb gardener start` ack-gate refusal leg),
  ka-0000000865 (/plans signal strip), and ka-0000004027, filed this
  cycle by the coverage-gap pass. **Narrowed ka-0000000865 8 selectors
  -> 6 before dispatch**: `plan-card-mr-ci` / `plan-card-mr-more-ci`
  gained genuine coverage since it was filed
  (plans_mr_overflow_ci.spec.js asserts the failed-class chip, the
  overflow facet, the negative case, and light/dark parity), so a crew
  told to cover all eight would have rewritten working tests. Re-verify
  a bead's own gap list at HEAD even when the bead was filed hours ago
  — these six moved within a day. **New gap ka-0000004027 is a
  recurring class, not a one-off**: pouring a formula whose step
  declares a `manual`/`sync` gate 400'd on an undeclared `gate_type`
  field and aborted the pour mid-molecule, dropping precisely the gate
  step — the third instance of "a pour path stamps a field the builtin
  type schema never declared" (after ka-0000002559 and the step-role
  dispatch fields). Its only guard is a STATIC unit test checking a
  hand-maintained field list, which structurally cannot catch the next
  writer; note the fixture must use a *non-decision* gate, since
  type:decision already declared both fields and a decision-gate
  fixture passes vacuously. **Epic hygiene: removed 3 non-issue
  `parent-child` edges** (this gardener's own session bead + the 2 crew
  agent beads it spawned) that auto-parented into the goal-bearing
  epic, making "all children closed" unreachable by construction
  (advice ka-0000002123). The daemon fix for this auto-parenting
  (ka-0000002199) merged 13:10Z but prod runs 2026.217.13 cut 12:26Z —
  **the recurrence is deploy lag, not a defective fix**, so check the
  deployed tag's cut time before re-filing a bug against a fix that has
  already merged. Decision = none.
- 2026-08-05 (cycle 24): 187 scripts = 171 wired + 16 excluded / 173
  modules / **drift = 0** / 0 quarantines / **10-of-10** recent main
  pipelines green / **0 open e2e bugs**. Merged **!6735**
  (`test-formula-gate-pour`, closes the gate-step half of the
  undeclared-step-field-400s-mid-pour class). Backlog is effectively
  drained: only **2 open task children**, both with *live* crews —
  every other non-closed child is agent/session/message bookkeeping.
  **Filed 1 new HEAD-verified gap: ka-0000004996** — fix `92e072a48`
  (merged 15:08Z) added pour-time `ValidateDependencies` so a dangling
  `depends_on` aborts the pour *before* any bead is created rather than
  silently dropping the gating edge, but it shipped **unit-only**.
  **Methodology note worth keeping:** the naive grep says this is
  covered — `test-formula-lifecycle.sh:1040` *does* assert a dangling
  `depends_on` is rejected. It is a different code path: that control
  is on `kd formula update` (**authoring-time** validation), while the
  regression lives on the **pour** path. When a fix lands on one verb
  of a command family, grep for the *verb you are actually testing*,
  not the feature name — a name-match on a sibling verb reads as
  coverage and buries the gap. Spawned 1 crew (ka-0000004996); 3 in
  flight, under the 5 cap. Decision = none.
- 2026-08-05 (cycle 25): 188 scripts = 172 wired + 16 excluded / 174
  modules / **drift = 0** / 0 quarantines / **10-of-10** recent main
  pipelines green. ka-0000004996 (cycle 24's gap) closed by its crew
  within the hour. Backlog drained to **1 open task child**
  (ka-0000000865, live crew). **Filed 1 new HEAD-verified gap:
  ka-0000007613** — merge `18bfb1336` (MR !6697, 15:47Z, *after*
  cycle 24's audit window) replaced the Home Live-agents card's
  one-line squawk preview with a per-agent recent EVENT STREAM
  (`buildAgentEventStream` merges squawks + comments newest-first,
  de-dups on the rendered preview, caps at `eventStreamRowsPerAgent`),
  and shipped unit-only: `index-active-card-events`,
  `index-active-card-squawk`, `index-rail-agent-squawk`, and
  `buildAgentEventStream` all have **0 hits** across `gardenio/tests/`.
  **Methodology note — the two-renderers-one-noun trap:**
  `test-agents-live-view.sh` *does* assert `agent-event-closed` and
  `agent-event-meta">by <closed_by>`, which reads as "agent events are
  covered". They are a different surface entirely: those selectors come
  from `agent_events.go` (the per-agent activity lane at
  `GET /agents/{id}/events`), while the Home rail card is `index_rail.go`
  + `templates/index.html` emitting `index-active-card-*`. One feature
  noun, two unrelated renderers, disjoint selector namespaces. This is
  the sibling of cycle 24's sibling-verb trap: there the name-match was
  on the wrong *verb* of a command family, here on the wrong *handler*
  of a feature name. Generalized rule for both: **grep for the selector
  or verb the handler under test actually emits, never the feature
  name.** Also re-confirmed `crew-e2e` is now a real role (bug
  ka-0000002951 closed) — `gb role list` shows it bead-sourced, so
  cycle 3's silent-global-defaults spawn warning is gone. Spawned 1
  crew (ka-0000007613); 2 in flight, under the 5 cap. Decision = none.

- 2026-08-05 (cycle 26, UTC 16:37): 174 modules / 188 scripts / 16
  exclusions / **drift = 0** / 0 quarantines / **10-of-10** recent main
  pipelines green. Drift measured the authoritative way — running
  `test-shard-isolation.sh` and reading its `exclusion-list complete`
  line (188 on disk = 172 wired + 16 excluded; 174 distinct modules
  because some scripts are multi-mode). Note `grep -cE '^\s+run_module '`
  reads 174 here and agrees only by luck: it counts *invocations*, and
  the 172-vs-174 gap is exactly the multi-mode scripts. Prefer the
  harness line.
  **Sprint backlog is nearly drained: 135 of 140 epic children closed**,
  leaving 1 live task child (ka-0000007613, agent working) + this
  cycle's new one. Publish the *task* count, not the raw child count —
  the other open children are agent/session bookkeeping (and one
  `type=advice`, ka-0000002123, whose non-issue kind makes "all children
  closed" unreachable by construction; see that bead).
  **Merged MR !6738** (`glab mr merge --squash`, self-merge per
  ka-0000002230) — the e2e-hermeticity fix closing P0 release-blocker
  ka-0000004117. Both repaired modules were harness bugs, not product
  bugs: `advice-budget` scenario 7 leaned on prod's ~38 ambient global
  advice beads to fill a 1500-byte prime budget, which does not hold on
  a fresh ephemeral `gardenio-rwx-*` namespace (~zero ambient advice), so
  nothing overflowed and the eviction assertion false-negatived; and
  `advice-lint-undeliverable`'s "zero findings" control bead was
  labelled `role:crew-e2e`, which — now that `crew-e2e` is a *real*
  role — no longer trips `dead-role-target`, but was asserted against
  the old behavior. **Generalized lesson: an e2e that reads ambient
  production state is not hermetic, it is merely lucky on prod.** Seed
  the pressure your assertion depends on.
  **Coverage-gap pass** over the 6 merges since cycle 25's audit point
  (15:47Z) found one real gap, filed as **ka-0000007965** and dispatched
  to 1 crew: MR !6724 / commit `0d6049827` added
  `schedule_cron_fallback.go`, making viewer cadence fall back to the
  first `kind:cron` entry of a bead's `triggers:` array when the legacy
  top-level `cron`/`timezone` are empty — wired into THREE read sites
  (/gardeners, /triggers rows, /b/{id} schedule detail) and shipped
  unit-only (324 test lines). Every cadence selector those surfaces
  emit — `gardener-cadence`, `cap-next{,-soon,-err}`,
  `maps-trigger-{next,err}`, `trig-{cron,nextfire}` — has **0 hits**
  across `gardenio/tests/e2e/`.
  **Why this one was ranked over the other 5 merges:** it is not merely
  uncovered, it *blocks an open sibling*. ka-0000004962 ("Step B: clear
  legacy cron/timezone on 12 migrated gardeners") is open and will clear
  those legacy fields on 12 live gardeners — at which point this fallback
  becomes the ONLY source of cadence on prod /triggers and /gardeners,
  and Step B's own acceptance criterion ("cron + next-fire still
  render") has nothing proving it. Filed a `blocks:` edge so the
  dependency is in the graph, not just in prose.
  **Two traps recorded on the child** (both are the cycle-25 rule
  recurring): (1) `grep -rln 'triggers=' tests/e2e/scripts/` appears to
  hit `test-trigger-system.sh`, but all 4 hits are **shell variables**
  (`all_triggers=$(...)`, `prod_triggers=$(...)`) — that module sets
  `cron=` zero times and never fetches a viewer page; (2) `grep -rl
  cap-next gardenio/tests/` returns a file, but it is
  `beads-viewer-screenshots/tools/measure-gardeners.py`, a screenshot
  **measurement tool**, not a wired module. And `test-viewer-maps.sh`
  genuinely exercises /gardeners + /maps — but only roster
  inclusion/exclusion, never cadence. Same page, different assertion.
  **Cumulative rule, third cycle running: grep the SELECTOR the handler
  emits — not the feature name, not the sibling verb, not the page.**
  Spawned 1 crew (ka-0000007965); 2 in flight, under the 5 cap.
  Decision = none — nothing this cycle was a genuine architectural fork.
- 2026-08-05 (cycle 27): 174 modules / 188 scripts / 16 exclusions /
  **drift = 0** / 0 quarantines / **8 of last 8 main pipelines green**.
  Drift measured the authoritative way — ran `test-shard-isolation.sh`
  and read its `exclusion-list complete` line (188 on disk = 172 wired
  + 16 excluded; 174 distinct modules because some scripts are
  multi-mode). `grep -c run_module` reads 174 and would look right by
  luck. **Found + fixed the reason this loop could never go dormant**:
  escalation **ka-0000002052** (`kind=data`) was a `parent-child` child
  of the epic, so "all children closed" was unreachable *by
  construction* — an escalation stays open until an operator acts, and
  closing it to drain the epic would have silently discarded a real
  maintainer ask (the bare `*` protected-branch pattern, 6698 leaked
  heads). Fixed per advice ka-0000002123 by removing the **edge**, not
  the bead: `kd dep remove ka-0000002052 ka-0000000958 --type
  parent-child`. Audited every non-closed child: the only other
  non-issue rows are `type=agent`/`type=session` bookkeeping (which
  auto-close) and ka-0000002123 itself, which is `related`, not
  `parent-child` — correct already, left alone. Coverage pass over 165
  merged MRs (7d) found 2 real zero-coverage gaps, each HEAD-verified by
  **content-grepping the selector the handler emits** before dispatch:
  (1) ka-0000006935 — the /plans ⚠ stall badge (`plans_stall.go`
  BadgeLabel / 1h dead-agent grace / 24h loud-stale tint, selectors at
  `plan_detail.html:59-71`), zero e2e hits, unit-tested only; (2)
  ka-0000000730 — `FailedHiddenCount` on the collapsed `+N MR` overflow
  pill (`plans_mrs.go`, `.plan-card-mr-more-failed` at
  `plans.html:268-275`), whose own code comment states the failure it
  prevents: a plan whose hidden MRs are ALL failing otherwise "reads
  identically to one whose hidden MRs are all green". Both specced with
  negative assertions so the invariant is pinned, not just the happy
  path. Spawned 2 crews (ka-0000006935, ka-0000000730); 4 in flight,
  under the 5 cap. Left !6745 and !6747 (both green) to their own live
  crews rather than stealing the merge. Decision = none — no genuine
  architectural fork this cycle.
- 2026-08-05 (cycle 28): 175 modules / 189 scripts / 16 exclusions /
  **drift = 0** / 0 quarantines / **9 of last 9 completed main pipelines
  green**. Drift measured the authoritative way again (ran
  `test-shard-isolation.sh`: 189 on disk = 173 wired + 16 excluded).
  **Backlog is effectively drained**: the epic has 156 children and
  *zero* open unclaimed issue-kind children — the only 4 non-closed
  issue rows (ka-0000007965, ka-0000000215, ka-0000006935,
  ka-0000000730) are all in_progress with **live** crews, confirmed via
  `gb news --active-only` (stale set was empty — nothing to reap).
  Re-audit gate ka-0000007847 is closed. No open P0/P1 e2e bug.
  Coverage pass over 7d of runtime commits found and dispatched one new
  gap, **ka-0000003093** — and finding it turned on a measurement fix
  worth recording: **grepping `kd <verb>` in the e2e suite reads 0 for
  everything and is a false negative**, because the suite invokes
  `"$KD_BIN" <verb>`. Under the correct pattern, `kd escalation` splits
  resolve=4 / reject=1 / **approve=0, list=0, show=0**. The gap is
  genuinely partial, not greenfield — 6 escalation modules are already
  wired — and the existing approval coverage drives the *bridge*
  endpoint (`POST /api/escalations/{id}/approve`, asserting
  `approval_method=web_ui`) while the CLI verb hits the *daemon*
  endpoint (`POST /v1/escalations/{id}/approve`, stamping
  `approval_method=cli`). The single `approval_method=cli` write in the
  suite is a **hand-rolled `kd update -f` simulation** whose scenario is
  literally titled "approval simulation" (test-escalation-crud.sh:279),
  so the daemon's operation_spec auto-replay — the whole reason
  `approve` differs from `resolve` — is unasserted from the CLI. Also
  unpinned anywhere (0 e2e AND 0 unit hits): `requireOpenEscalation`'s
  wrong-type/already-closed rejections, and the 403 remediation text
  telling an agent that escalations are "resolved by OPERATORS" and to
  file `gb decision create` instead — the anti-self-approve guard the
  escalation design rests on, and exactly the registered-but-dead shape
  that already cost us in ka-0000004093. Scoped as an EXTEND of
  test-escalation-crud.sh (module count stays flat, no new wiring) with
  the simulation replaced by the real call. Spawned 1 crew; **5 in
  flight, at the cap**. Decision = none — no genuine architectural fork.
- 2026-08-05 (cycle 29): 175 modules / 189 scripts / 16 exclusions /
  **drift = 0** / 0 quarantines / **10-of-10 recent main pipelines
  green**. Authority for drift is test-shard-isolation.sh, run live
  this cycle: "exclusion-list complete — 189 on disk = 173 wired + 16
  excluded"; shard-partition invariant also holds at TOTAL_SHARDS =
  4/5/8/16. **The named sprint backlog is drained**: after
  ka-0000000730 (/plans MR overflow pill) landed mid-cycle, the epic's
  only remaining issue-kind children were the 2 still-running crews
  from cycle 28 (ka-0000006935 /plans stalled badge, ka-0000003093
  `kd escalation` CLI) — every other open child was a transient
  `type=agent`/`kind=config` bead, not work. So this cycle's whole
  value was **step 3, the coverage-gap pass**, run as a HEAD-verified
  audit of the ~80 merges to main in the last 7 days. Filed and
  dispatched 3 new children, each carrying file:line evidence in BOTH
  directions (what the runtime does; what the nearest e2e does
  *instead*) so no crew re-derives it: **ka-0000006761** (P1) — the
  daemon's 409/FailedPrecondition divert of a generic `kd close` on an
  OPEN escalation (d0cb8deda). Highest blast radius of the cycle:
  reverting the guard makes agent `kd close` succeed again while
  skipping operation_spec replay and falsely releasing
  close-on-escalation wrappers — a *successful-looking* operation that
  never ran, with no error to alert on. Note the shape of the absence:
  three modules (test-escalation-crud.sh:320, -execution.sh:465,
  -v2.sh:388) each carry a **comment explaining why they now avoid the
  path**, and the cleanup loops `kd close ... || true` swallow the 409
  outright — the suite documented the behavior instead of asserting it.
  **ka-0000003792** (P1) — server-side 400 on an unknown `?type=` LIST
  filter plus the `template→formula` alias fold (24523ed1a). The kd
  CLI's own client-side guard (cmd/kd/type_validate.go:52) is exactly
  what makes a server regression invisible, and every existing raw
  `?type=` curl in the suite ends in `|| echo "[]"`, so a 400 is
  indistinguishable from an empty page — literally the footgun the fix
  targets. **ka-0000000469** (P2) — `/v1/health/live` unauthenticated
  200 + the Helm readiness/liveness wiring (495b1d214); the handler is
  unit-tested, but the *deployment wiring* that actually caused the
  flake has zero coverage and regresses only as intermittent HTTP 000
  in unrelated modules, i.e. as someone else's flake budget. The audit
  also **rejected 6 candidates as already-covered** with covering
  file:line (probe-execution `would_fire_now`, `gb release status`
  pipeline reason, `gb bundle update`, `gb news` alive-freshness, `gb
  probe run`, escalation-approve 409-on-closed) — dedup evidence that
  is now on the beads rather than re-derived next cycle. **5 crews in
  flight, at the cap.** Decision = none — no genuine architectural
  fork.
- 2026-08-06 (cycle 53, gardener ka-0000003482): **192 modules / 207 scripts
  = 190 wired + 17 excluded, drift 0** (authority: `test-shard-isolation.sh`'s
  `exclusion-list complete` line, not `grep -c run_module` — the raw grep says
  192 because it counts the shard-pin block twice); shard partition PASSes at
  TOTAL_SHARDS=4/5/8/16; 0 quarantines; **10/10 recent main pipelines green**;
  0 open P0/P1 e2e bugs. Epic backlog was down to **2** in_progress children,
  both with genuinely live crews (`gb news --stale-only` empty). So the value
  this cycle was entirely the coverage-gap pass: **audited 13 runtime merges
  from the last 36h, filed + dispatched 2 HEAD-verified gaps, rejected 2.**
- **ka-0000002238** (P1) — the declarative bead-trigger operators
  `field_changed` / `changed_to` have **0 hits across BOTH suites** and 0 across
  every `*_e2e_test.go`, while the runtime consuming them
  (`trigger_source_bead.go:634,636`) spans a **four-process seam**: kbeads
  `perFieldChanges` (`beads.go:1055`) -> SSE encode (`events.go:80`) -> bridge
  decode (`sse.go:398`) -> `matchOneCondition`. **The trap here is a new test
  that looks like coverage and isn't:** merge `bab3f7c74` shipped
  `trigger_liveness_invariants_t1_test.go:162-199`, which hand-stuffs
  `src.pending` with `Changes: {"enabled": false}` directly into the in-memory
  source — it **starts downstream of the entire wire**, so it stays green if
  kbeads stops emitting the field, the JSON tag drifts, or `sse.go:398` drops
  it. And `sse_integration_test.go:262`, the one test reading `changes` off a
  real stream, asserts `changes["title"]` — a first-class column populated at
  `beads.go:983`, a *different code path* from the custom-field overlay.
  Cumulative rule, restated: **a test that injects the value under test is not
  coverage of the code that produces it.** Failure mode: a watchdog on
  `enabled:changed_to:false` silently never fires, the gardener goes dark, and
  every test above stays green.
- **ka-0000001802** (P1) — a **new shape worth naming: the orphaned test.**
  Merge `5620f9804` shipped the gardener prompt-drift alarm *with* a 232-line,
  12-test unit suite — which is invoked by **nothing**
  (`grep -rn 'check-gardener-prompt-drift_test' --include='*.yml' --include=Makefile
  --include='*.sh' .` -> **0**; only refs are its own docstring and a "Companion
  test:" comment). No pytest autodiscovery exists to rescue it (no `pytest.ini`
  /`pyproject.toml`/`conftest.py`/`tox.ini` anywhere), so the checker has zero
  coverage **at any tier**. I ran it by hand: `Ran 12 tests ... OK` — it passes,
  it is just dead. The repo already has the home: `.rwx/ci.yml` task
  `test-scripts` (`:719`) lists six sibling `*_test.py` in exactly this shape
  (`:790`/`:881`); this one was added to neither the `filter:` nor the `run:`.
  Compounding it, the alarm is `allow_failure: true` at two levels
  (`.gitlab-ci.yml:923,926`) and exit 1 (op-error) vs 2 (drift) render
  identically as a yellow job — so a broken checker degrades **in the direction
  that looks healthy**, and an over-matching `_is_pointer_stub` would report
  green while every gardener is drifted: the exact 2026-08-05 bug class, with a
  green light on top. **Search habit this adds:** greping the test suites is not
  enough — a test file can exist, pass, and still never run. Grep the CI
  *invocation*, not the test file.
- Rejected 2 as already-covered, with dedup evidence now on the beads: the
  skip-sentinel protocol (`73a4aa2ed`) is genuinely gated —
  `test-skip-visibility-lint.sh:124-138` **lifts the real `run_module` body out
  of `run-shard.sh` and `eval`s it** against three fake modules, asserting
  `TESTS_SKIPPED==1` (`:186`) with an anti-vacuity floor (`:105-119`), and the
  task is in the required check set (`.rwx/ci.yml:25,63`); and the
  escalation-reaper result-verb switch (`385bab22e`) is covered by the union of
  a compile-time guard (`CloseBead` absent from `ReaperBeadClient`,
  `escalation_reaper.go:182-190`), a unit test, and live-daemon scenario 5c
  (`test-escalation-crud.sh:533-773`, which posts the exact reaper wire body and
  reads fields back top-level). Two smaller findings logged but not filed as
  modules: `.rwx/release.yml:516-546`'s aggregate JUnit regex matches only
  `MODULE PASSED:`/`MODULE FAILED:`, so a skip is *dropped* from aggregate JUnit
  (fidelity gap, not false-green — still surfaced via `SKIPPED_MODULES` at
  `:1023`); and `test-trigger-system.sh:714-719` **passes when nothing has
  `last_checked` set** ("No triggers have last_checked set" -> `pass`), a
  signal-swallowing check whose hard version already exists at
  `test-trigger-engine-smoke.sh:266`. Decision = none — no genuine
  architectural fork.
