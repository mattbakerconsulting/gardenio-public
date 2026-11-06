# MicroVM suspend/resume — E2E validation runbook (step 6)

**Plan:** MicroVM native suspend/resume (bundle `ka-0000004518`, epic `ka-0000004689`).
**Task:** step 6 — cost model + metrics + E2E validation, `ka-0000002861`.
**Companion:** `docs/microvm-suspend-resume-cost-model.md` (the "quantify the win" half).

This runbook is the **executable half** of step 6's "prove it works": a repeatable, **outcome-based**
end-to-end validation of the full suspend→resume lifecycle on **one real `lambda_microvm` thread
agent**. It verifies by *observable outcome* — agent-bead fields + the step-6 Prometheus metrics —
never by pod logs, because the gardenio agent SA is affirmatively RBAC-denied `kubectl logs`/`get pods`
in every gardenio namespace (advice `ka-0000006777`). Every check below is a `kd`/HTTP/PromQL read an
agent can actually run.

---

## 0. Preconditions (why this cannot "just run" today)

The suspend/resume code (steps 1–5) is merged to `main`, but a live E2E needs three gates that are
**not** satisfied by a bare merge:

1. **Released.** Steps 1–5 must be in a released chart. As of this writing the latest release
   (`2026.201.5`) predates the backend-surface commit — cut a release
   (`./scripts/release.sh`; any agent may, advice `ka-0000005099`) so the controller running in prod
   actually contains `Suspend`/`Resume`/`handleMicrovmResume`/rehydrate.
2. **Enabled in prod.** `agents.microvm.suspendOnStop.enabled` defaults **false** and the prod
   values (`deploy-values/gardenio-k3s.yaml`) do **not** set it, so
   a `gb stop` today **terminates** the VM (byte-for-byte pre-plan behaviour). To exercise the path,
   the prod values must set:
   ```yaml
   agents:
     microvm:
       suspendOnStop:
         enabled: true            # gb stop → SuspendMicrovm (leave bead open)
       resumeRehydrate:
         enabled: true            # default-on; the step-5 fail-closed proof-of-life gate
       # OPTIONAL standby-billing backstop (recommended for first enablement — see cost model §3a):
       idlePolicy:
         enabled: true
         autoResumeEnabled: false        # cannot fire on egress-only topology
         maxIdleDurationSeconds: 0       # idle clock never advances in-topology
         suspendedDurationSeconds: 900   # cap standby at ~15 min before AWS auto-terminates
   ```
   Prod deploy is `terraform apply -target=module.gardenio` (NOT `helm upgrade`), so this lands via
   the release pipeline's deploy-prod stage after the overlay change merges. **Enabling native
   suspend changes prod behaviour for every `gardenio-microvms` thread agent** — it is a deliberate
   rollout decision, gated behind the operator (see the step-6 decision on `ka-0000002861`), not
   flipped unilaterally in the metrics/cost MR.
3. **A live thread agent on `lambda_microvm`.** The `gardenio-microvms` role resolves
   `execution_target=lambda_microvm` (`ka-0000007754`), and suspend eligibility additionally
   requires a **Slack thread binding** (`slack_thread_channel` + `slack_thread_ts`) — only thread
   agents suspend, because only a re-mention gives something to resume.

---

## 1. The validation sequence

Drive one real thread agent through the full lifecycle. `AGENT` = the agent name; `BEAD` = its agent
bead id (`kd show`/`kd search` resolves it). All verification is `kd show <BEAD> --json | jq` on the
agent bead's `fields`, plus the step-6 metrics.

### Step 1 — mention → work (baseline)

@-mention a `gardenio-microvms` thread agent in its Slack thread and let it do a trivial unit of work
(e.g. write a scratch file / make a note). Capture the baseline:

```bash
kd show "$BEAD" --json | jq '{
  target: .fields.execution_target,      # want: lambda_microvm
  state:  .fields.agent_state,           # want: working
  rstate: .fields.runtime_state,         # want: RUNNING
  vm:     .fields.runtime_id             # capture — MUST be identical after resume (M-continuity)
}'
```
Record `runtime_id` as **`VM0`** — the whole point is that resume keeps the *same* VM.

### Step 2 — `gb stop` → SUSPEND (verify SUSPENDED + bead OPEN)

Have the agent `gb stop` (its normal graceful despawn). Because it is a suspend-eligible thread
agent, the controller calls `SuspendMicrovm` and **leaves the bead open**:

```bash
kd show "$BEAD" --json | jq '{
  status: .status,                       # want: open  (NOT closed — closing would terminate the VM)
  state:  .fields.agent_state,           # want: stopping  (suspended-awaiting-resume)
  rstate: .fields.runtime_state,         # want: SUSPENDED
  vm:     .fields.runtime_id             # want: == VM0 (unchanged)
}'
```

**PASS criteria:** `status=open`, `agent_state=stopping`, `runtime_state=SUSPENDED`, `runtime_id=VM0`.
A `status=closed` here is the failure the plan's guard protects against (a closed bead fires
`AgentDone` → `Terminate` and destroys the resume target).

**Metric check** — one standby-entry was counted:
```promql
increase(gardenio_microvm_suspends_total{outcome="suspended",role="gardenio-microvms"}[10m])  # want: ≥ 1
```

### Step 3 — re-mention → RESUME (verify same VM, continuity, rehydration ack)

Post a **new** message in the same thread. The bridge asks the controller to resume in place
(`POST /api/v1/microvm/resume`); the controller `ResumeMicrovm`s **`VM0`** and — with the
rehydration gate on — stamps `resuming`, then runs the step-5 fail-closed proof-of-life rendezvous.

**While resuming** (the ~warm-boot window):
```bash
kd show "$BEAD" --json | jq '{state: .fields.agent_state, vm: .fields.runtime_id}'
# want during: agent_state=resuming, runtime_id == VM0
```

**After proof-of-life** (the resumed VM's egress heartbeat re-crossed the overlay to beads):
```bash
kd show "$BEAD" --json | jq '{
  state:  .fields.agent_state,           # want: working  (proof-of-life confirmed, step-5 M1)
  rstate: .fields.runtime_state,         # want: RUNNING
  vm:     .fields.runtime_id             # want: == VM0  ← SESSION CONTINUITY: same VM, same disk+session
}'
```

**PASS criteria:** `runtime_id` **unchanged** (`== VM0`) — a *new* id means the resume abandoned the
suspended VM and cold-started, which is a continuity failure. `agent_state` settles to `working`
(NOT `resume_failed`). The agent answers the new message with its prior working tree intact (no
re-clone — verify the scratch file from Step 1 is still present, e.g. ask it to read it back).

**Rehydration-ack check** — the controller-internal status endpoint reports the rendezvous settled
(operator-side, from within the cluster network):
```bash
curl -s "$CONTROLLER_URL/api/v1/microvm/resume/status?agent=$AGENT" | jq
# rehydrating:true while in flight → false once settled (working) — the MicroVM analog of thaw/status
```

### Step 4 — measure resume latency + standby cost

**Resume latency** (the step-6 metric — only proof-confirmed resumes are bucketed):
```promql
histogram_quantile(0.5,  rate(gardenio_resume_latency_seconds_bucket{path="microvm-resume"}[1h]))
histogram_quantile(0.95, rate(gardenio_resume_latency_seconds_bucket{path="microvm-resume"}[1h]))
```
Expected: **well under the ~27 s cold path** (§1a of the cost model) — a warm-boot resume with no
repo re-clone. Record the p50/p95 into the plan bundle.

**Resume rate** (was the standby actually resumed, not reaped unused?):
```promql
rate(gardenio_resume_latency_seconds_count{path="microvm-resume"}[1h])
  / rate(gardenio_microvm_suspends_total{outcome="suspended"}[1h])
```
For a single validated agent this is ≈ 1 (one suspend → one resume).

**Standby cost** = `P_susp × t_idle`, where `t_idle` = (resume request time − suspend time). Read the
idle window from the suspend→resume timestamp delta (bead `updated_at` transitions, or the metric
scrape timestamps), and multiply by the AWS SUSPENDED per-second rate for the ARM_64 / 2048 MiB VM
shape (cost model §2 `P_susp`). Compare against `P_run × t_idle` (what a warm-pool slot would have
billed for the same window) and against `$0 + P_run × T_cold` (what cold would have cost on wake) to
land the concrete per-agent saving.

---

## 2. Expected result matrix

| Checkpoint | Field / metric | Expected |
|---|---|---|
| After work | `agent_state` / `runtime_state` | `working` / `RUNNING` |
| After `gb stop` | `status` | `open` (never `closed`) |
| After `gb stop` | `agent_state` / `runtime_state` | `stopping` / `SUSPENDED` |
| After `gb stop` | `microvm_suspends_total{outcome=suspended}` | +1 |
| During resume | `agent_state` / `runtime_id` | `resuming` / **== VM0** |
| After proof-of-life | `agent_state` / `runtime_state` / `runtime_id` | `working` / `RUNNING` / **== VM0** |
| After proof-of-life | scratch file from Step 1 | still present (no re-clone) |
| Resume latency | `resume_latency_seconds{path=microvm-resume}` p50/p95 | ≪ 27 s |
| Fail path (negative) | if proof-of-life times out | `agent_state=resume_failed`, VM terminated, next message cold-starts |

The **single most important assertion** is `runtime_id` **unchanged across the whole cycle** — that
is native suspend/resume actually keeping the same VM (memory + disk + session) alive, which is the
entire parity claim vs the gVisor freeze/thaw path.

---

## 3. Status of the live run

As of step-6 delivery, gates §0.1 (release) and §0.2 (prod enablement) are **not** satisfied, so the
live single-agent run has **not** been executed in this session — there is no `lambda_microvm` agent
running with `suspendOnStop` enabled to drive, and the agent role cannot read pod logs to verify by
any other means. The metrics (part 2) and cost model (part 1) are complete and the path is unit-test
covered end to end (`microvm_resume_endpoint_test.go`, `podspec_microvm_suspend_test.go`,
`resume_latency_test.go`). This runbook is the operator-runnable procedure to close the live gate the
moment the path is released + enabled; results get captured back into the plan bundle `ka-0000004518`.
