# MicroVM suspend/resume — cost model (step 6)

**Plan:** MicroVM native suspend/resume (bundle `ka-0000004518`, epic `ka-0000004689`).
**Task:** step 6 — cost model + metrics + E2E validation, `ka-0000002861`.
**Feeds:** MicroVM warm-pool feature `ka-0000005825` (the pool-sizing cost knob).

This note answers the plan's step-6 question **"quantify the win"**: what does a *suspended*
MicroVM cost per hour of standby, how does that compare to a *warm-pool booted* VM and to *cold
launch+terminate*, and — the decision the warm-pool feature needs — **when does suspend beat warm
pool beat cold?**

It is deliberately a *model*, parameterised on the AWS Lambda MicroVM billing dimensions and the
gardenio-measured launch/resume latencies, not a table of invented dollar figures. Where a real
per-second rate is not yet published to us, the model states the dimension and the break-even
*formula* so the answer stays correct when the rate is filled in. All latency inputs are the
same ones the step-6 metrics emit (`gardenio_resume_latency_seconds{path="microvm-resume"}`,
`gardenio_microvm_launch_duration_seconds`, `gardenio_microvm_suspends_total`), so the model is
falsifiable against production the moment the path is enabled.

---

## 0. TL;DR

- **Cold launch+terminate** costs *only the active run* — zero standby — but pays the full **~27 s
  cold spin-up** on every wake (AWS VM boot + Ziti enroll + `gb init` 2× git clone + coop start),
  and re-clones the repo every time.
- **Warm-pool booted VM** removes the spin-up (assignment ≈ **1–2 s**, a bead-flip poll + prompt
  injection) but bills a **fully-running vCPU+memory VM** for the entire time it sits idle in the
  pool waiting to be claimed.
- **Suspended VM** also removes the spin-up (resume is a warm-boot, target well under the cold
  path) **and** bills at the (lower) **SUSPENDED** rate during standby, while preserving the exact
  disk + session so there is *no repo re-clone* on wake.

The three are not interchangeable — they occupy different points on a **(standby cost) × (resume
latency) × (state continuity)** surface:

| Strategy | Standby $/hr | Resume latency | Repo/session on wake | Who pays standby |
|---|---|---|---|---|
| **Cold** launch+terminate | **$0** (nothing parked) | ~27 s (full spin-up) | re-cloned cold | nobody |
| **Warm pool** | `P_run` (full running VM) | ~1–2 s (bead-flip) | fresh (booted, not the user's) | the pool, continuously |
| **Suspend/resume** | `P_susp` (suspended VM) | warm-boot + proof-of-life | **exact** disk+session preserved | the suspended VM, only while a specific agent is idle |

The **break-even rules** (derived in §3):

1. **Suspend beats cold** for a given idle gap `t_idle` whenever the standby it bills is cheaper
   than the cold re-spin it saves: `P_susp · t_idle  <  C_cold_spin`. With `P_susp > 0` this is a
   **maximum idle window** — park while the next message is likely soon, terminate (or let the
   `SuspendedDurationSeconds` backstop terminate) once the gap grows past it.
2. **Suspend beats warm pool** essentially always on the standby axis when `P_susp < P_run` **and**
   the workload is *per-agent resumable* (a specific thread agent that will be re-mentioned) rather
   than *anonymous-fungible* (any next spawn). Suspend parks *one identity's exact state*; the pool
   parks *fungible booted capacity*. They solve different problems (see §4).
3. **Warm pool beats suspend** only when the wake is a *fresh* agent with no prior state to preserve
   (a brand-new spawn), because then there is nothing to suspend — the pool's booted-but-unclaimed
   VM is the only way to skip cold spin-up.

---

## 1. The three strategies, precisely

All three target the same user-visible goal — a thread agent responds fast to a follow-up `@`-mention
— and differ only in **what is kept alive between messages**.

### 1a. Cold launch + terminate (today's default)

On `gb stop` the controller **terminates** the MicroVM (`Lifecycle.Terminate` → `TerminateMicrovm`,
per-run secret + Ziti lease revoked) and closes nothing is parked. The next `@`-mention **cold-launches
a brand-new VM**: `RunMicrovm` → in-VM boot → Ziti enroll → `gb init` (ssh-keyscan + **2 git clones** +
settings) → coop start + prompt injection.

- **Standby cost: $0.** Nothing is billed between messages.
- **Wake cost: the full cold spin-up.** Post the coopmux-token fix (`ka-0000006120` / gardenio!5687)
  the measured cold path is **~27 s**: ~7 s AWS VM boot+Ziti+listeners, ~6 s `gb init`, ~8 s coop
  start + startup bypass + prompt injection, plus ~1.2 s controller-side launch dispatch
  (the `gardenio_microvm_launch_duration_seconds` term). The repo is **re-cloned every time**.

### 1b. Warm pool (the `ka-0000005825` feature this model feeds)

Port the K8s prewarmed-pool machinery to lambda_microvm: park **booted + cloned VMs in entrypoint
standby**, so assignment costs only the bead-flip poll + prompt injection (~1–2 s), the same as the
K8s warm pool.

- **Standby cost: a fully-running VM, continuously.** A pooled VM is **RUNNING** (its vCPU +
  memory are live so it can flip to a task in ~1–2 s). It bills the full running rate `P_run` for
  its **entire** parked life, whether or not it is ever claimed. Pool size is therefore a direct,
  continuous cost knob — `N_pool · P_run` per hour regardless of hit rate.
- **Wake cost: ~1–2 s** (bead-flip + prompt injection). No boot, no clone-from-cold — but the
  cloned repo is the *pool VM's fresh clone*, **not** the user's prior working tree.
- **Constraint the K8s pool does not have** (`ka-0000005825`): a pooled VM's `MaximumDurationSeconds`
  clock is running while it waits, so standby eats the assigned run budget; the pool must recycle
  idle VMs well before the cap and prefer the freshest VM.

### 1c. Suspend / resume (this plan)

On `gb stop` of a freeze-eligible **thread** agent the controller **suspends** the VM
(`SuspendMicrovm`, SUSPENDING→SUSPENDED) and **leaves the bead open** at `agent_state=stopping` as
the resume target; per-run creds + Ziti leases are deliberately **not** revoked (12 h session creds
cover the standby window). The next `@`-mention **resumes the same VM in place** (`ResumeMicrovm`),
which fires the in-VM `resume` hook — a whole-userspace warm-boot that re-reads secrets + re-enrolls
Ziti + restarts the agent — gated by the step-5 fail-closed proof-of-life rendezvous before the
agent is marked `working`.

- **Standby cost: the SUSPENDED rate `P_susp`.** A SUSPENDED MicroVM has its vCPUs descheduled;
  what it still occupies is the **memory/disk snapshot** that lets it resume in place. This is the
  cost dimension the whole plan is betting is **materially below `P_run`** (a suspended VM is not
  scheduling a vCPU). The `SuspendedDurationSeconds` IdlePolicy member is the **standby-billing
  auto-terminate backstop** (`backend.go` `NewIdlePolicy`): egress-only topology means the idle
  clock never advances in-topology and auto-resume-on-inbound cannot fire, so `SuspendedDurationSeconds`
  is the operative knob that caps how long we pay `P_susp` before AWS reaps the VM.
- **Wake cost: the resume warm-boot + proof-of-life.** This is `gardenio_resume_latency_seconds
  {path="microvm-resume"}` — resume-request-accepted → the resumed VM's egress heartbeat re-crosses
  the overlay (step-5 M1). Crucially there is **no cold repo re-clone** (the VM disk survives
  suspend — the dominant gVisor create-path cost is simply absent), so even though it restarts the
  full userspace it should land **well under the ~27 s cold path** while preserving the **exact**
  working tree + session the user left.

---

## 2. Cost dimensions (what actually bills)

AWS Lambda MicroVM billing has two relevant states for this model. The gardenio launch pins these VM
sizing dimensions (in the image-registration tool deleted 2026-11-02, gd-jOl9itDY0D): **ARM_64**, **min 2048 MiB** memory, one CPU
configuration; `MaximumDurationInSeconds` defaults to **3600 s** (the epic raises it toward 8h30m,
`ka-0000002537`).

Let:

- `P_run`  = billed rate of a **RUNNING** MicroVM ($/hr) — vCPU **scheduled** + memory resident.
- `P_susp` = billed rate of a **SUSPENDED** MicroVM ($/hr) — vCPU **descheduled**, snapshot resident.
- `C_cold_spin` = the one-off **cost of a cold spin-up** = `P_run · T_cold`, where `T_cold ≈ 27 s`
  is the measured cold wake (§1a). (Cold spin-up bills at the running rate for its whole duration
  plus the controller dispatch; the repo re-clone is included in `T_cold`.)
- `C_resume` = the one-off cost of a **resume** = `P_run · T_resume`, where `T_resume` is the
  measured `resume_latency_seconds{path="microvm-resume"}` (warm-boot + proof-of-life; **no**
  re-clone). By construction `T_resume < T_cold`.
- `t_idle` = the wall-clock a specific agent sits idle between two messages (the standby window).

The **modelling invariant** the plan asserts and the metrics will confirm:

```
P_susp  <  P_run            (a suspended VM is cheaper than a running one)
T_resume <  T_cold          (a warm-boot resume is faster than a cold spin-up)
```

If a future AWS price sheet ever shows `P_susp ≈ P_run` (suspend bills like running because the
memory snapshot dominates), the suspend advantage **collapses to the latency axis only** — it would
still beat cold on wake speed and state continuity, but would no longer beat warm pool on standby
cost. That is the single most important number to confirm from real billing; the metrics below make
the *behavioural* half falsifiable regardless.

---

## 3. Break-even analysis

### 3a. Suspend vs cold — the maximum idle window

For one idle gap `t_idle`, compare the **total** cost of each strategy over `[stop, next-message]`:

- **Cold:** pay nothing during idle, then pay a full cold spin-up on wake:
  `Cost_cold = C_cold_spin = P_run · T_cold`.
- **Suspend:** pay `P_susp` for the whole idle window, then a resume on wake:
  `Cost_susp = P_susp · t_idle + C_resume = P_susp · t_idle + P_run · T_resume`.

**Suspend is cheaper than cold** when `Cost_susp < Cost_cold`:

```
P_susp · t_idle  +  P_run · T_resume   <   P_run · T_cold
P_susp · t_idle                        <   P_run · (T_cold − T_resume)
t_idle  <  (P_run / P_susp) · (T_cold − T_resume)      ── the break-even idle window t*
```

Interpretation: there is a **maximum idle window `t*`** below which suspend wins. `t*` grows with
the **cost ratio `P_run/P_susp`** (the cheaper standby is relative to running, the longer you can
afford to park) and with the **latency saved `T_cold − T_resume`** (the more expensive cold spin-up
is, the more a parked VM is worth). Beyond `t*`, the accumulated standby bill exceeds the cold
re-spin you avoided — terminate instead (or let `SuspendedDurationSeconds` do it).

- **Sanity, cheap-standby regime** (`P_susp ≪ P_run`, the plan's bet): `t*` is large — you can park
  for many multiples of `T_cold` before standby costs as much as one cold re-spin. Suspend wins for
  essentially all human-conversational gaps (seconds-to-minutes between thread replies).
- **Sanity, expensive-standby regime** (`P_susp → P_run`): `t* → (T_cold − T_resume)` — only worth
  parking for roughly the spin-up time you'd save, i.e. barely. This is the regime where you'd fall
  back to cold except for the state-continuity value (§4).

`SuspendedDurationSeconds` should be set at or below `t*` for the workload: it is the standby-billing
backstop that guarantees you never pay past the point where cold would have been cheaper.

### 3b. Suspend vs warm pool — standby cost per resumable agent

Warm pool bills `P_run` per pooled VM **continuously**, independent of whether that slot is claimed:
over an idle window `t_idle`, a pool slot held for that agent costs `P_run · t_idle`. Suspend costs
`P_susp · t_idle` for the same window. So on the pure standby axis, for the *same* idle window:

```
Cost_susp_standby / Cost_pool_standby  =  P_susp / P_run   <  1
```

Whenever `P_susp < P_run`, **suspend is strictly cheaper per parked agent than holding a warm-pool
slot for it** — and it preserves the exact session, which a fungible pool slot cannot. The catch is
that they are not substitutes for the same wake (see §4): the pool answers *fresh* spawns, suspend
answers *the same agent's* re-mention.

### 3c. Warm pool vs cold — the pool's own trade (for completeness)

The pool converts a per-wake `C_cold_spin` into a continuous `P_run` carrying cost. It wins when the
**aggregate** spin-up cost it eliminates exceeds the standby it burns:

```
(hit rate · wakes-per-hour · C_cold_spin)   >   N_pool · P_run
```

i.e. the pool is justified only at **high wake frequency + high hit rate**. This is why the
`ka-0000005825` feature defaults pool size to **0/off per project** and makes it explicit opt-in —
at low agent-wake frequency the pool burns `N_pool · P_run` for almost no spin-ups saved.

---

## 4. When to use which — the decision rule

The axis that actually decides is **"is there prior state worth preserving for a *specific* agent?"**

```
                       ┌─ same agent, will be re-mentioned, has a working tree/session
   thread agent   ───► │     └─► SUSPEND/RESUME  (cheapest standby, exact-state, warm resume)
   (per-identity)      │
                       └─ idle gap grows past t* (§3a)
                             └─► TERMINATE (cold next time) — or let SuspendedDurationSeconds reap

   fresh spawn    ───► no prior state to keep; want to skip cold spin-up for the *next* agent
   (fungible)           └─► WARM POOL  (booted-but-unclaimed capacity, ~1–2 s assignment)

   rare / one-shot ──► low wake frequency, no re-mention expected
                       └─► COLD launch+terminate  ($0 standby, accept the ~27 s wake)
```

- **Suspend/resume is the thread-agent parity path.** A thread agent bound to a Slack thread will
  very likely be re-mentioned, has a working tree + session the user expects to still be there, and
  sits idle for conversational gaps (well inside `t*`). Suspend wins on all three axes: cheapest
  standby (`P_susp < P_run`), warm resume (`T_resume < T_cold`), **exact** state continuity (no
  re-clone, same session).
- **Warm pool is the fresh-spawn accelerator.** It cannot preserve a specific agent's state (a pool
  VM is a *different* identity, freshly booted), so it does not compete with suspend for re-mention
  wakes. It competes with *cold* for **first** spawns, and only pays off at high spawn frequency ×
  hit rate. This is the feature `ka-0000005825` sizes; **this model's `P_run` vs `P_susp` split is
  the input that tells that feature a warm pool of size N costs `N · P_run/hr` — strictly more than
  suspending N idle agents at `N · P_susp/hr`.**
- **Cold is the floor.** Zero standby, correct for rare/one-shot agents where no wake is expected
  soon; the ~27 s wake is acceptable when it happens seldom.

**Composability:** suspend and warm pool are *complementary*, not alternatives. A mature deployment
suspends known thread agents (per-identity, cheap standby) **and** keeps a small warm pool for cold
*first* spawns (fungible, ~1–2 s). The cost model for the combined system is
`Σ P_susp · t_idle(agent)  +  N_pool · P_run` — minimise the first term by tuning
`SuspendedDurationSeconds` to `t*`, minimise the second by keeping `N_pool` at the smallest size
that holds the target first-spawn hit rate.

---

## 5. Metrics — how this model is falsified against production (step 6 part 2)

The model is only useful if it is checkable. Step 6 instruments the two behavioural terms so the
`P_run/P_susp` × latency surface can be read off live Prometheus/VictoriaMetrics (env label joins
across the estate):

| Metric | What it measures | Model term |
|---|---|---|
| `gardenio_resume_latency_seconds{path="microvm-resume",regime="microvm",runtime="microvm"}` | resume-request-accepted → step-5 proof-of-life (warm-boot + Ziti re-enroll + first egress heartbeat) | **`T_resume`** — only proof-*confirmed* resumes are bucketed |
| `gardenio_microvm_launch_duration_seconds` | controller-side cold launch dispatch (creds + SM writes + `RunMicrovm` accept) | the controller slice of **`T_cold`** (in-VM boot/enroll/init land in launcher-account CloudWatch) |
| `gardenio_microvm_suspends_total{outcome="suspended"}` | VMs put into standby on `gb stop` | denominator for the **resume rate** |
| `gardenio_microvm_suspends_total{outcome="fallback-terminated"}` | suspend attempts that failed and fell back to terminate | standby-entry failure rate |

**Headline derived signals** (PromQL):

- **Resume rate** (does parked standby actually get woken back up? — the core assumption behind
  paying *any* `P_susp`):
  ```promql
  rate(gardenio_resume_latency_seconds_count{path="microvm-resume"}[1h])
    / rate(gardenio_microvm_suspends_total{outcome="suspended"}[1h])
  ```
  A resume rate near 1 means almost every suspended VM is resumed → standby is well spent. A low
  resume rate means we are paying `P_susp` for VMs that get reaped by `SuspendedDurationSeconds`
  without ever being resumed → tighten `SuspendedDurationSeconds` toward `t*`, or terminate on stop.
- **Resume speed** (`T_resume`, to confirm `T_resume < T_cold` and thus the latency win):
  ```promql
  histogram_quantile(0.5, rate(gardenio_resume_latency_seconds_bucket{path="microvm-resume"}[1h]))
  histogram_quantile(0.95, rate(gardenio_resume_latency_seconds_bucket{path="microvm-resume"}[1h]))
  ```
- **Standby-entry health** (suspend vs fallback-terminate ratio):
  ```promql
  sum(rate(gardenio_microvm_suspends_total{outcome="fallback-terminated"}[1h]))
    / sum(rate(gardenio_microvm_suspends_total[1h]))
  ```

The **cost half** (`P_run`, `P_susp`) is the AWS billing dimension this model deliberately keeps as
a parameter: multiply the measured idle-window integral `Σ t_idle` (derivable from suspend→resume
timestamp deltas) by the published `P_susp`, and the running-equivalent by `P_run`, to get the
realised dollar saving. Once AWS surfaces the SUSPENDED per-second rate to the Gardenio account, drop
it into §2 and every break-even in §3 becomes a concrete number with no further modelling.

---

## 6. Open items / what a real number would sharpen

1. **The `P_susp/P_run` ratio is the whole ballgame.** Confirm the SUSPENDED per-second rate from
   AWS billing for the ARM_64 / 2048 MiB MicroVM shape and record it in §2. Everything downstream is
   a formula waiting on it.
2. **`T_resume` distribution** must be gathered from real resumes (the metric is now wired) — the
   model assumes `T_resume < T_cold` but the *margin* sets `t*`. Early prod samples go in the plan
   bundle.
3. **`t*` per workload** — set `SuspendedDurationSeconds` from the measured conversational idle-gap
   distribution so we never pay past break-even. Start conservative (short) and widen as the resume
   rate confirms parked VMs are actually resumed.
4. **Warm pool sizing** (`ka-0000005825`) consumes §3c + §4: pool only for *fresh-spawn*
   acceleration, size N to the smallest that holds the first-spawn hit target, and suspend
   everything that is a known thread agent — never pool what you can suspend more cheaply.
