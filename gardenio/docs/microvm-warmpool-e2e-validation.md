# MicroVM warm pool — E2E validation & opt-in rollout runbook (Phase 6)

**Plan:** MicroVM warm pool (bundle `ka-0000006755`, epic `ka-0000004145`).
**Phase/Task:** Phase 6 — E2E validation & opt-in rollout (`ka-0000000285` / impl `ka-0000003470`).
**Feature:** `ka-0000005825`. **Metrics frontier bundle:** `ka-0000006875`.
**Cost model companion:** `docs/microvm-suspend-resume-cost-model.md` §3c (the "when is the pool worth it" half).

This runbook is the **executable half** of Phase 6: a repeatable, **outcome-based** end-to-end
validation of the `lambda_microvm` warm pool on **one real size-1 pool for a test project**. Like the
suspend/resume runbook it verifies by *observable outcome* — agent-bead fields + Phase-5 Prometheus
metrics — never by pod logs, because the gardenio agent SA is affirmatively RBAC-denied
`kubectl logs`/`get pods` in every gardenio namespace (advice `ka-0000006777`). Every check below is a
`kd`/HTTP/PromQL read an agent can actually run.

---

## 0. The opt-in guard (pure code — SHIPPED, no infra needed)

The rollout half of Phase 6 is code, and it is done + merged on this branch. It makes the warm pool
**default 0/off per project for `lambda_microvm`, explicit opt-in** — the cost invariant the whole
feature rests on (feature `ka-0000005825`; cost model §3c). Idle MicroVM members bill for their entire
standby life and burn a `MaximumDurationInSeconds` cap, so a MicroVM pool must never mint warm members
the operator did not explicitly size.

Three surfaces enforce it, all execution-target aware:

1. **Config parse** (`beadsapi.PrewarmedPoolConfig.ConfiguredMinSize`) — the RAW operator-set min
   (pre-K8s-default). `ListProjectBeads` no longer pre-fills `min_size=2`; `EffectivePools` remains the
   single K8s-default normalization point so the K8s pool is byte-for-byte unchanged.
2. **Reconciler** (`poolmanager.Reconcile`) — a `lambda_microvm` pool whose `ConfiguredMinSize <= 0` is
   dropped before any create/recycle: it stays at 0/off and mints nothing. A K8s pool with an unset min
   still gets the historical default of 2 (idle K8s pods are free).
3. **CLI** (`gb pool enable`) — for a project/role that resolves to `lambda_microvm`, enable flips the
   flag but does NOT auto-fill `min_size`, and prints an opt-in hint. An explicit min is always preserved.

**Opt-in is therefore a single project-bead edit** — set a positive `min_size` on the `prewarmed_pool`
JSON field of the project's `type=project` config bead (resolve its id with `gb config dump` or
`kd list --type=project`), for a project whose `execution_target` (or role override) is `lambda_microvm`:

```bash
# Opt a test project into a size-1 MicroVM warm pool (the Phase-6 measurement config).
# --field replaces the whole prewarmed_pool JSON value, which is exactly what we want here.
kd update <PROJECT_BEAD_ID> --field \
  prewarmed_pool='{"enabled":true,"min_size":1,"max_size":1,"role":"gardenio-microvms","mode":"crew"}'
# (role must resolve execution_target=lambda_microvm; the gardenio-microvms role does, ka-0000007754.)
# gb pool enable alone will NOT size a MicroVM pool — by design it flips enabled only and prints an
# opt-in hint (§0.3); the explicit min_size above is the opt-in.
```

Unit tests pinning all three surfaces: `beadsapi.TestConfiguredMinSize*`,
`poolmanager.TestReconcile_Microvm*` / `TestReconcile_KubernetesPoolUnsizedStillDefaultsToTwo`,
`gb.TestRunPoolEnable_Microvm*`.

---

## 1. Preconditions (why the live measurement cannot "just run" today)

The Phase 1–5 code is merged to `main`, but a live E2E needs three gates a bare merge does not satisfy:

1. **Released.** P4 (lifetime/cost, `!6349`) and P5 (metrics, `!6351`) merged to `main` *after* tag
   `2026.211.2` was cut, so the currently-deployed staging code has warm-assign + identity-preservation
   but **not** the cost guard or the Phase-5 metrics. A fresh release must ship P4+P5 to staging
   (`gardenio-rwx`). Per CLAUDE.md, **E2E runs only on release (tag) pipelines** — a main-push does NOT
   deploy staging. Cut a release (`make release`; any agent may, advice `ka-0000005099`) that includes
   this branch's opt-in guard once it merges.
2. **A `lambda_microvm` test project with the size-1 pool opted in.** Apply the §0 `gb config set` on a
   test project. This is the only config action that turns the pool on; there is no Helm flag.
3. **AWS MicroVM standby spend authorized for that project.** A size-1 standby pool bills one ARM_64 /
   2048 MiB VM continuously for the whole measurement window (cost model §2 `P_run`). That is real
   spend on the Gardenio AWS account and must be operator-authorized before enabling.

Gates 1 and 3 are operator actions — file `gb escalation create --requesting-task ka-0000003470` for the
release-cut + spend authorization + (if needed) test-project provisioning, and squawk. Do NOT silently
block.

---

## 2. The validation sequence

Drive one real size-1 `lambda_microvm` pool. `PROJ` = the test project; `POOLBEAD` = the prewarmed
pool member's agent bead id; `ASSIGNEE` = the agent bead the assignment lands on (same bead — the whole
point). All verification is `kd show <BEAD> --json | jq` on the agent bead's `fields`, plus PromQL.

### Step 1 — pool fills (verify a warm MicroVM parks in standby)

After enabling (§0) and one reconcile pass (~30s, `PREWARMED_POOL_INTERVAL`), a single prewarmed
MicroVM member should exist and reach standby:

```bash
gb pool status --project "$PROJ"     # want: Enabled true, Min size 1, Prewarmed 1
# Resolve the pool member bead:
kd list --status=open --json | jq -r '.[] | select(.fields.project=="'"$PROJ"'" and .fields.agent_state=="prewarmed") | .id'
kd show "$POOLBEAD" --json | jq '{
  target: .fields.execution_target,   # want: lambda_microvm
  state:  .fields.agent_state,        # want: prewarmed  (in-VM standby, Phase 2)
  rstate: .fields.runtime_state,      # want: RUNNING
  vm:     .fields.runtime_id          # capture — MUST be identical after assign (no re-mint)
}'
```
Record `runtime_id` as **`VM0`** — the per-run Ziti lease + 2 Secrets Manager secrets are minted keyed
to `POOLBEAD`'s id at Launch (not stored as separate bead fields), so `runtime_id` staying `== VM0`
across the assign is the observable proxy that the identity binding held (no re-Launch → no new lease /
secrets). **Metric:** `gardenio_prewarmed_pool_depth{project="$PROJ",execution_target="lambda_microvm",state="prewarmed"}` = 1.

### Step 2 — trigger an assignment → measure trigger→RUNNING (the ~1–2s claim)

Trigger a thread assignment for `PROJ` (an @-mention in a mapped Slack thread, or `gb pool assign
--project "$PROJ" --role gardenio-microvms`). `AssignPrewarmed` flips the **same** `POOLBEAD`
prewarmed→assigning; the in-VM standby loop (500 ms `BOAT_STANDBY_POLL`) detects it and starts work.

```bash
kd show "$POOLBEAD" --json | jq '{
  state:  .fields.agent_state,        # want: assigning → working
  rstate: .fields.runtime_state,      # want: RUNNING (never re-booted)
  vm:     .fields.runtime_id,         # want: == VM0  ← SAME VM, no re-mint
  thread: .fields.slack_thread_ts     # now bound to the triggering thread
}'
```

**PASS criteria:** the assignment lands on the SAME bead (`POOLBEAD == ASSIGNEE`), `runtime_id == VM0`,
and the member reaches `working` in **~1–2s**, not the ~27s cold path.

**Warm-assign latency (Phase-5 metric — the headline number):**
```promql
histogram_quantile(0.5,  rate(gardenio_prewarmed_assign_duration_seconds_bucket{execution_target="lambda_microvm",outcome="hit"}[1h]))
histogram_quantile(0.95, rate(gardenio_prewarmed_assign_duration_seconds_bucket{execution_target="lambda_microvm",outcome="hit"}[1h]))
```
Compare against the cold path `gardenio_microvm_launch_duration_seconds` (~27s). Record p50/p95 into the
metrics bundle `ka-0000006875` and this bead.

### Step 3 — identity invariant (same ARNs, zero re-mint)

The single most important assertion, and exactly what the Phase-3 bind-once guard
(`Lifecycle.Launch`, `ka-0000006503`) enforces in unit tests — assert it live here:

```bash
kd show "$POOLBEAD" --json | jq '.fields.runtime_id'   # want: == VM0 (unchanged across the assign)
```
`runtime_id == VM0` means assignment did NOT re-enter `Lifecycle.Launch → backend.Launch`, so the per-run
Ziti lease + SM secrets minted at Step 1 (keyed to `POOLBEAD`) were reused verbatim — **zero** new leases
/ secrets. A changed id means assignment re-Launched (the failure the guard prevents). Operator-side (from
within the cluster network) the same invariant is confirmable directly against AWS: the VM id, Ziti lease
id, and SM secret ARNs for `POOLBEAD` are unchanged from Step 1. **Metric:** the assign was a `hit`
(`increase(gardenio_prewarmed_assigns_total{outcome="hit",project="$PROJ"}[10m]) >= 1`), NOT an `empty`
cold-mint.

### Step 4 — recycle + cost guard fire

**Refill:** after Step 2 consumed the warm member, the reconciler mints a fresh size-1 replacement
(`prewarmed_pool_depth{...state="prewarmed"}` returns to 1) — the pool self-heals to its opted-in depth.

**Recycle-before-cap (Phase 4):** an unassigned MicroVM member is closed once its standby age exceeds
`microvmRecycleAge` (half the effective `MicrovmMaximumDurationSeconds`, or an explicit
`PREWARMED_MICROVM_MAX_AGE`), well under the VM lifetime cap and every per-run credential TTL. To observe
it deterministically, pin a short `PREWARMED_MICROVM_MAX_AGE` (e.g. `2m`) on the test instance and watch:
```promql
increase(gardenio_prewarmed_agents_cycled_total{project="$PROJ"}[15m])   # want: >= 1 (aged member recycled)
```
The recycled member's bead goes `closed`; a fresh replacement is minted in the same pass (same-image,
new identity). Assigned/working members are never touched.

**Cost guard (the opt-in default-off, §0):** remove the opt-in (set `min_size` back to 0) and confirm the
pool drains to zero and mints nothing — the default-off invariant:
```bash
kd update <PROJECT_BEAD_ID> --field prewarmed_pool='{"enabled":true,"min_size":0,"max_size":1,"role":"gardenio-microvms","mode":"crew"}'
# after one reconcile:
kd list --status=open --json | jq -r '[.[] | select(.fields.project=="'"$PROJ"'" and .fields.agent_state=="prewarmed")] | length'  # want: 0
```

---

## 3. Expected result matrix

| Checkpoint | Field / metric | Expected |
|---|---|---|
| Pool filled | `agent_state` / `runtime_state` | `prewarmed` / `RUNNING` |
| Pool filled | `prewarmed_pool_depth{execution_target=lambda_microvm,state=prewarmed}` | 1 |
| After trigger | assignment bead | == the pool member bead (same bead flipped) |
| After trigger | `agent_state` / `runtime_id` | `working` / **== VM0** |
| After trigger | warm-assign p50/p95 (`prewarmed_assign_duration_seconds`, target=microvm) | **~1–2 s** (≪ 27 s cold) |
| After trigger | Ziti lease id + SM secret ARNs | **unchanged** (zero re-mint) |
| After trigger | `prewarmed_assigns_total{outcome=hit}` | +1 (not `empty`) |
| Steady state | pool refills to opted-in depth | `prewarmed_pool_depth` back to 1 |
| Aged member | `prewarmed_agents_cycled_total` | +1 (recycle-before-cap) |
| Opt-in off (min_size 0) | prewarmed member count | 0 (default-off cost guard) |

The **single most important assertion** is `runtime_id` + identity ARNs **unchanged across the assign**
— warm assignment reusing the exact bound identity, the entire per-run-identity parity claim vs a cold
mint.

---

## 4. Status of the live run

As of Phase-6 delivery, gates §1.1 (release shipping P4+P5) and §1.3 (AWS standby spend authorization)
are **not** satisfied in this session, so the live size-1 run has **not** been executed here — there is
no released+opted-in `lambda_microvm` pool to drive, and the agent role cannot read pod logs to verify by
any other means. The **opt-in rollout half (§0) is complete, merged, and unit-test covered**; the metrics
(Phase 5) and cost model (§3c) are in place. This runbook is the operator-runnable procedure to close the
live gate the moment the branch is released + a test project is opted in + spend is authorized; results
get captured back into this bead (`ka-0000003470`) and the metrics bundle (`ka-0000006875`).
