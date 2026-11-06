# ADR — Account reset-proximity SIGN: keep use-it-or-lose-it, do not flip to long-runway

> **Status: PROPOSED (spike / decision record).** Outcome of the deferred
> Tension #1 spike (ka-0000002731), split off from the account-balancing work in
> ka-0000001331 / MR !5460. This ADR **models and measures** the steady-vs-burst
> account-inflow question that MR !5460 explicitly deferred, and recommends **not
> flipping** the reset-proximity sign. It changes no code; it records the
> decision and the evidence behind it, and proposes the lightweight
> instrumentation that would let us revisit it with data rather than intuition.
>
> **Reviewer TL;DR.** The current sign (`capacityResetBonus`, prefer accounts
> that reset **soon** — use-it-or-lose-it) is *provably correct in the only
> regime where the term is load-bearing* (bursty / near-saturated inflow), and
> gardenio's measured inflow is exactly that regime (Fano factor **6.4**, peak
> **8.9×** the median hour, most cognition accounts sitting at `util=1.0`). The
> specific harm Matt named — proximity pulling load onto near-exhausted accounts
> — was already neutralized by the MR !5460 `highUtilGuardrail` (urgency→0 above
> `util 0.7`) and the long-lived-role headroom bias. Flipping to a long-runway
> preference would **regress** the burst regime to help a steady regime that
> gardenio is empirically almost never in, and where the term barely affects
> throughput anyway. **Recommendation: keep the sign; add the ρ / burstiness
> instrumentation in §6 so any future change is data-driven.**

---

## 1. Background — what the sign is and where it lives

Account selection at spawn is a least-loaded spread over a project's healthy
account pool (`gardenio/controller/cmd/controller/account_spread.go`,
`selectAccount` / `selectSpreadAccountAt`; full pipeline in
[`account-routing.md`](account-routing.md)). The effective-load score is:

```
effective_load = live_pods + warning_penalty
                 − capacityResetBonus            ← this ADR
                 − spawn_weight
                 − longLivedHeadroom             (gardener / plan-gardener only)
```

`capacityResetBonus(acct, now)` (account_spread.go:419) is:

```
bonus  = unused × (baseline + (max − baseline) × urgency)
       = unused × (1 + 2 × urgency)              (baseline=1.0, max=3.0)
unused  = 1 − util(binding window)               binding = five_hour | seven_day
urgency = clamp01(1 − until_reset / horizon)     horizon = 2h, 0 outside it
```

So `urgency` ramps **0→1 as the reset APPROACHES**: an idle account in the last
two hours before its window rolls is the cheapest possible placement. This is
the **use-it-or-lose-it** sign — quota left unused when the window resets is lost
forever, so spend the most-perishable quota first.

**The proposed flip (Tension #1, Matt Baker 2026-07-14):** prefer the **highest
time-until-reset** ("long-runway") instead — bias `+= f(until_reset)` so a
low-util account gets the *whole* window to ramp to full utilization before it
resets, rather than being fed only in its final minutes. Rationale as stated:
*"low utilization accounts that might not get exhausted in time to get the full
reset benefit."*

These two signs are **opposite**, and MR !5460 correctly refused to flip on
intuition, deferring to this spike: *"use-it-or-lose-it vs long-runway optimize
different regimes (burst vs steady inflow) and which is right depends on
queued-load knowledge the selector does not have."* This ADR resolves that.

---

## 2. The model — why the sign is regime-dependent

Frame account selection as a scheduling problem. There are `N` accounts, each
with capacity `C` (quota) per window of length `W`; windows reset on **staggered
phases** (no longer a shared reset — see §4). Total inflow arrives at rate `λ`
pods/hour. Define the **window load factor**

```
ρ = λ · W / (N · C)        (fraction of total window capacity the inflow demands)
```

The reset-proximity term is a **deadline signal**: `until_reset` is the deadline
after which an account's *currently unused* quota is discarded. Two regimes:

### 2a. Saturated / bursty regime (ρ ≳ 1) → EDF is optimal → **current sign wins**

When inflow can fill the fleet's capacity within a window, the scheduling
objective is to **maximize quota consumed before it expires** (minimize wasted,
perishable quota). This is a classic single-machine-per-account deadline problem,
and the greedy optimum is **Earliest-Deadline-First**: place load on the account
whose quota expires soonest that still has headroom.

`capacityResetBonus`'s current sign **is EDF** — `urgency→1` as `until_reset→0`
pulls placement toward the soonest-resetting account with unused capacity. Under
a burst, that quota is genuinely about to vanish and the burst can consume it
*now*. Flipping to long-runway here **defers use of the perishable quota**,
guaranteeing more of it is lost at reset. **The flip is a strict regression in
this regime.**

### 2b. Starved / steady regime (ρ ≪ 1) → sign barely matters → **flip's benefit is second-order**

When inflow is a thin steady trickle that cannot fill even one account per
window (`λ·W < C`), **every pod finds a home regardless of routing** — the fleet
is under-loaded, so nothing is ever dropped and *throughput is `λ` independent of
the sign.* What Matt's argument is really about here is **per-window
utilization** (getting "the full reset benefit" = a fully-used subscription
window). But:

- Utilization efficiency in this regime is dominated by the **`unused` baseline
  term** (`capacityBaselineWeight`, always-on), *not* the proximity term — the
  bonus comment already notes the baseline "is the term that actually separates
  them day-to-day."
- The proximity term only engages inside the last 2h before a reset, a small
  fraction of a 5h (let alone 7-day) window, so it touches few placements.
- The genuine steady-regime harm — a *long-running* agent landing on an account
  that resets/exhausts mid-run — is **Tension #2, already shipped** in MR !5460
  (`longLivedHeadroomWeight` + `longLivedNearLimitPenalty` for gardener /
  plan-gardener: prefer absolute headroom, penalize `util>0.8`).

So in the steady regime the flip buys a **second-order** utilization tweak that
the baseline term and the least-loaded spread already largely deliver, while in
the bursty regime it is a **first-order regression**. The asymmetry is the whole
decision — *unless gardenio actually lives in the steady regime.* §3 measures it.

---

## 3. Measurement — which regime is gardenio actually in?

Two independent measurements, taken 2026-08-29 against live prod
(acct `123456789012`).

### 3a. Inflow is strongly BURSTY (not steady)

Agent spawns are the inflow. Hourly spawn counts for `project:gardenio` over the
trailing 7 days (`kd list --type=agent --created-after 7d`, n=1653 spawns across
169 hours):

| Statistic | Value | Interpretation |
|---|---|---|
| mean / hour | 9.8 | — |
| **Fano factor** (var/mean) | **6.43** | Poisson/steady = **1.0**; 6.4 ⇒ strongly over-dispersed = **bursty** |
| coefficient of variation | 0.81 | high hour-to-hour variability |
| median / p90 / max per hour | 7 / 20 / **62** | — |
| **peak / median ratio** | **8.9×** | order-of-magnitude bursts over the floor |
| share of spawns in busiest 10% of hours | **27%** | load concentrates into short windows |

A steady (memoryless Poisson) arrival process would show a Fano factor near 1.
gardenio's is **6.4** — the inflow is a low steady floor (~3–8/hr) punctuated by
gardener fan-outs / release / triage sweeps that spike to 60+/hr. This is the
**burst regime of §2a**, where the current sign is correct.

### 3b. The fleet frequently runs SATURATED (ρ ≳ 1)

Live cognition-account snapshot (`kd list --type=account -l project:gardenio`),
2026-08-29 ~06:40Z:

| Account | binding | util | throttle | resets |
|---|---|---|---|---|
| shannon | seven_day | **1.00** | rejected | 08-31 |
| gardenio | seven_day | **1.00** | rejected | 08-30 |
| gardenio2 | seven_day | **1.00** | rejected | 08-30 |
| gardenio3 | seven_day | **1.00** | rejected | 09-02 |
| matthewbaker-acme | seven_day | **1.00** | rejected | 09-01 |
| matthewbaker1110 | five_hour | 0.19 | allowed | 08-29 06:40 |
| fimbaz | five_hour | 0.05 | allowed | 09-01 |

**Five of seven** real cognition accounts are exhausted (`util=1.0`, throttle
`rejected`); only two five-hour accounts are serving. This is not an anomaly —
it mirrors the live snapshots recorded in ka-0000001331 itself (2026-07-14:
everything routing to `shannon` because it was the only allowed account). gardenio
routinely operates at or above `ρ=1`, where quota is genuinely scarce and
perishable — precisely the regime that rewards spending soonest-expiring quota
first.

### 3c. The original sign's premise is dead, but the fix is EDF, not a flip

The current sign's origin (ka-0000001522) assumed **all gardenio accounts share
one 5h reset**, so proximity didn't differentiate accounts and only the baseline
`unused` term mattered. That assumption is now **false**: the snapshot shows
mixed `five_hour` / `seven_day` binding windows with resets staggered across five
distinct days. This *activates* the proximity term (it now differentiates
accounts) — which is exactly why the sign became "live and load-bearing" and
worth a spike. But staggered deadlines make the problem *more* of a deadline-
scheduling problem, which **strengthens** the case for EDF (current sign), not
for the flip.

---

## 4. Decision

**Keep the reset-proximity sign as-is (use-it-or-lose-it / EDF). Do NOT flip to a
long-runway preference.** Rationale, in order of weight:

1. **The measured regime is bursty + saturated (§3), where the current sign is
   provably optimal (§2a) and the flip is a strict regression.**
2. **The harm that motivated the flip is already fixed by MR !5460.** The
   `highUtilGuardrail` zeroes `urgency` above `util 0.7`, so proximity can no
   longer pull load onto a near-exhausted account; the long-lived-role headroom
   bias keeps hours-long agents off near-limit accounts. The flip would be
   *re-solving an already-solved problem with a worse tool.*
3. **In the only regime the flip helps (steady/starved), its benefit is
   second-order** — dominated by the always-on `unused` baseline term and the
   least-loaded spread — and gardenio is empirically almost never in it.
4. **The selector still lacks the queued-load signal** the flip would need to be
   applied *conditionally*; a blind global flip trades a well-understood optimum
   for an intuition.

This **supersedes the Tension #1 flip proposal** in ka-0000001331. The
use-it-or-lose-it sign stands as the intended behavior, not a legacy accident.

---

## 5. Alternatives considered

- **(A) Flip to long-runway globally (Matt's original ask).** *Rejected.*
  Regresses the dominant burst/saturated regime (§2a, §3); the steady-regime
  benefit it targets is second-order and already substantially delivered by the
  baseline term + Tension #2. No measurement supports gardenio being in the
  regime it optimizes for.
- **(B) Regime-adaptive sign.** Measure recent inflow `λ` and binding-window
  capacity, apply EDF urgency when `ρ` is near/above saturation and a mild
  long-runway preference when starved. *Deferred, not rejected.* This is the
  "correct" answer in principle, but (i) it needs the queued-load / inflow-rate
  signal the selector does not yet have, (ii) §3 shows gardenio is almost always
  in the EDF branch, so the added complexity would rarely change a placement, and
  (iii) it should not be built before the §6 instrumentation proves the starved
  regime occurs often enough to matter. Revisit if the instrumentation shows
  sustained `ρ < 1`.
- **(C) Drop the proximity term entirely, keep only the `unused` baseline.**
  *Rejected.* Now that resets are staggered (§3c), the proximity term does useful
  EDF work under bursts; removing it forfeits that for no benefit.
- **(D) Keep sign, do nothing else.** This ADR's recommendation, plus §6
  instrumentation so the decision is revisitable with data.

---

## 6. Proposed follow-up — instrument the regime (the real gap)

The honest gap this spike exposes is not the sign — it is that **the controller
does not measure which regime it is in.** Before anyone revisits Alternative (B),
add cheap, read-only instrumentation so the question is answered by data:

1. **Window load factor `ρ` per project.** Export a gauge
   `gardenio_account_pool_load_factor{project}` = (trailing-window spawn count ×
   representative window length) / (Σ per-account capacity). Derivable today from
   the spawn stream (agent-bead creation) + the account cache the probe already
   maintains — no new provider calls.
2. **Inflow burstiness `Fano factor` per project.** Export
   `gardenio_account_inflow_fano{project}` over a trailing window. `>~2` confirms
   the burst regime (EDF); sustained `~1` with `ρ<1` would be the first real
   signal that Alternative (B) could pay off.
3. **Wasted-quota estimate.** At each account reset, log `unused × C` — the quota
   discarded. Summed per project, this directly measures the objective the whole
   debate is about; a persistently high value in a *steady* regime would be the
   trigger to reopen the flip.

These live naturally alongside the existing `ProbeMetrics`
(`gardenio/controller/cmd/controller/ratelimit_probe.go`) and the account cache.
They are proposed here, **not** built by this spike (which changes no code); file
as a follow-up if the team wants the regime observable. Per the "measure every
important quantity two independent ways" principle (ka-0000005924), `ρ` and the
wasted-quota estimate cross-check each other.

---

## 7. References

- **Tension source / deferral:** ka-0000002731 (this spike), ka-0000001331
  (parent feature), MR !5460 (headroom guardrail + Tension #2, merged).
- **Sign origin:** ka-0000001522 (reset-aware weighting, when all accounts shared
  one 5h reset), ka-0000001771 (spreader plan-of-record), ka-0000004861 (origin
  spreader).
- **Code:** `gardenio/controller/cmd/controller/account_spread.go`
  (`capacityResetBonus` account_spread.go:419, tuning constants ~L280–345),
  `podspec.go` (`applyAccountConfig` caller),
  `account_rotation_restart.go` (rotation re-pick).
- **Pipeline reference:** [`account-routing.md`](account-routing.md).
- **Evidence:** live account snapshot + 7-day spawn-inflow statistics,
  2026-08-29, reproducible via
  `kd list --type=account -l project:gardenio` and
  `kd list --type=agent -l project:gardenio --created-after 7d`.
