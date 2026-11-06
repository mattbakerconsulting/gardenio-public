# LiteLLM Proxy — Hardening & HA Plan

**Bead:** ka-0000006118 · **Operator:** Matt Baker · **Date:** 2026-06-15

The in-cluster `gardenio-litellm` proxy is now the LLM data-plane for the entire
prod fleet (epic ka-0000000560, Stage-3 ka-0000002573). Every agent
completion, embedding, and the telepathy thinking-capture feed flows through
**one pod**. This doc captures the current posture, the gaps, and a prioritized
remediation plan.

## Current state (prod `gardenio` ns, observed 2026-06-15)

| Dimension | Current | Risk |
|---|---|---|
| Replicas | **1** (`litellm.replicaCount: 1`) | **SPOF** — pod/node loss = fleet-wide LLM outage |
| PodDisruptionBudget | **none** | node drain / upgrade evicts the only pod with no guard |
| HPA / autoscaling | **none** | no headroom under load spikes |
| Rollout strategy | `RollingUpdate maxSurge=1 maxUnavailable=0` | correct, but with 1 replica a new pod must go Ready before old terminates — brief tail latency, acceptable |
| Spend / key state | Postgres (`STORE_MODEL_IN_DB`, `DATABASE_URL` → shared RDS) | **good** — shared state means scale-out is safe |
| Redis | **none** | required for multi-replica router coordination (rate-limit, cooldown, cache) |
| `router_settings` | **absent** | no `num_retries`, no `allowed_fails`, no cooldown tuning |
| `fallbacks` | **absent** (`Available Model Group Fallbacks=None` in agent logs) | **by design, not a gap** — cross-role Bedrock fallback is forbidden (fsm-triage-only, policy ka-0000002660). Non-triage spend-cap → 30m park is intended. |
| Resources | req `200m`/`1Gi`, lim `2`/`2Gi` | fine for 1 replica; revisit with HPA |
| Probes | liveness `/health/liveliness` 30s, readiness `/health/readiness` 10s | present & correct |
| Bedrock (chat) | IRSA `gardenio-litellm-bedrock` live-verified 2026-06-15 | **role-pinned to `fsm-triage` only** — NOT a general fallback for other roles |

## The connection to "why pods needed a nudge" (ka-0000008022)

Agents hit `rate_limit_error: This request would exceed your account's monthly
spend limit` on model group `claude-opus-4-8-oauth`, with
`Available Model Group Fallbacks=None`. With no fallback group, `gb init` parks
the pod for a fixed 30m and re-injects.

**CORRECTION (operator, Matt 2026-06-16):** an earlier draft of this doc
proposed wiring a LiteLLM fallback (OAuth groups → Bedrock) as the fix. **That
is wrong and was withdrawn** (Draft MR !4425 closed). Bedrock is **not** a
general fallback target — it is partitioned **strictly by role** (policy
ka-0000002660): the `fsm-triage` role uses Bedrock and **only** Bedrock, and
**every other role is deliberately forbidden from spilling onto Bedrock** — the
controller surfaces the auth failure rather than silently consume Bedrock spend.
So the 30m park on a spend cap is **intended behavior** for non-triage roles,
not a bug to mask with cross-role routing.

> **Enforcement note (2026-06-28):** the partition is no longer a hardcoded
> `bedrockOnlyRole = "fsm-triage"` constant. It is now driven by the
> `exclusive_to_role` field on the account bead (`litellm-bedrock →
> exclusive_to_role=fsm-triage`), read generically by the controller — see
> [`account-routing.md`](account-routing.md) and
> [`bedrock-dehardcode-reaudit.md`](bedrock-dehardcode-reaudit.md). Adding a
> second Bedrock-only role/account no longer needs a code change (prod already
> runs a second one: `glm-bedrock → exclusive_to_role=glm-lab`).

The legitimate, narrower follow-ups for the stall are **H7** (smarter backoff
than a blunt fixed 30m) and surfacing "account spend limit reached" distinctly
in agent state / Slack. Neither touches model routing or the role partition.

## Remediation plan (prioritized)

### P0 — stop the bleeding (cheap, high-leverage)

- **H1 · PodDisruptionBudget.** Add `templates/litellm/pdb.yaml` mirroring the
  coopmux PDB (gated on `replicaCount > 1` + `litellm.pdb.enabled`). No effect
  at 1 replica, but a prerequisite so H2 is safe. **(MERGED — !4418.)**
- **~~H3 · Cross-role fallbacks~~ — WITHDRAWN.** An earlier version proposed
  mapping OAuth chat groups → Bedrock as a `router_settings.fallbacks` chain.
  This **violates the role partition** (Bedrock is `fsm-triage`-only, policy
  ka-0000002660) and was withdrawn (MR !4425 closed). Do **not** reintroduce
  cross-role Bedrock fallbacks. A `router_settings` block may still be useful
  later for `num_retries` / Redis (see H4), but **not** for cross-role routing.

### P1 — real HA (needs Redis for correctness)

- **H2 · Scale to ≥2 replicas** behind a **topologySpreadConstraint** (spread
  across nodes/AZs) + soft anti-affinity. Spend/key state is already in Postgres
  so two replicas share truth; **but** per-instance router state (rate-limit
  counters, model cooldowns, in-memory cache) diverges without Redis.
- **H4 · Redis** (in-chart or managed) wired via `router_settings.redis_*`
  so multi-replica rate-limiting, cooldowns, and cache are coordinated. Without
  it, two replicas double-count and cooldown independently — tolerable for
  availability, wrong for spend/rate accuracy.

### P2 — elasticity & polish

- **H5 · HPA** on CPU + (ideally) in-flight-request / latency custom metric,
  min=2 max=N. Bump requests so HPA has a sane target.
- **H6 · PriorityClass** so the data-plane proxy is not evicted before agent
  pods under node pressure.
- **H7 · Backoff smarting (controller side, overlaps ka-0000008022 fix B).**
  `rateLimitBackoff` (cmd/gb/init_agent.go:870) falls back to a fixed 30m when
  the message carries no reset time (the spend-limit case). Since cross-role
  Bedrock fallback is off the table (see the correction above), this — a
  jittered exponential backoff plus a distinct "account spend limit reached"
  signal — is the **primary** legitimate mitigation for the spend-cap stall,
  not a belt-and-suspenders afterthought.

## Sequencing

```
H1 (PDB)        ──►  prerequisite for H2  [MERGED !4418]
H4 (Redis)      ──►  prerequisite for correct H2
H2 (replicas≥2) ──►  real HA, after H1+H4
H5 (HPA), H6 (priority), H7 (backoff)  ──►  polish / stall mitigation
(H3 cross-role fallbacks — WITHDRAWN, violates the fsm-triage Bedrock partition)
```

## Validation per change

- H2/H4: roll one replica, confirm zero dropped requests; assert rate-limit
  counters are shared (two replicas, one budget) via `/key/info` spend parity.
- H5: synthetic load → HPA scales 2→N → settles.

All changes are additive Helm values + templates; each is independently
reversible by reverting the value (`replicaCount`, `pdb.enabled`,
`router_settings`, etc.).
