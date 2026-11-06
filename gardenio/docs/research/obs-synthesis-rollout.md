# W4 — Synthesis & rollout plan: Victoria → Grafana + grafana/alertmanager bridges

> Phase 0 synthesis for epic [[ka-0000006776]] (workstream W4, task [[ka-0000008233]]).
> Reconciles W1 ([[ka-0000003078]]), W2 ([[ka-0000006709]]), W3 ([[ka-0000007649]]) into one
> ordered Phase-1 plan. The Phase-1 implementation gate [[ka-0000004494]] resolved
> **`approve_all`** (operator Matthew Baker, 2026-06-09) — this doc is the ordering the
> approval refers to.

## 1. The three pieces and how they relate

| WS | Deliverable | Shape | Depends on |
|---|---|---|---|
| **W1** | site Victoria → gardenio vmsingle | **config-flip** (2nd remote-write), no binary | nothing (substrate exists) |
| **W2** | grafana-bridge (dashboards ↔ beads) | **new binary** `cmd/grafana-bridge` | nothing hard; better once W1 gives real data to dashboard |
| **W3** | alertmanager-bridge (alerts ↔ beads) | **new binary** `cmd/alertmanager-bridge` | alerts-in reuses existing parser; needs an Alertmanager firing + (for value) W1 metrics to alert on |

Key relationship: **W1 is data, W2/W3 are control planes over that data.** W2 lets you
*build* dashboards on the data; W3 lets you *alert* on it. Neither bridge is *blocked* by
W1 (you can author a dashboard or a rule before ACME data arrives), but both are *more
valuable* once W1 lands ACME signals — you have something real to chart and to alert on.

## 2. One process or three binaries?

**Recommendation: two new binaries (grafana-bridge, alertmanager-bridge), not one fused
"obs-bridge," and W1 stays binary-less.**

- W1 is pure config (remote-write target) — no process to write. Folding it into a binary
  would invent a dependency the config-flip constraint ([[ka-0000001783]]) explicitly
  forbids.
- W2 and W3 are genuinely different external systems (Grafana HTTP/ConfigMap vs.
  Alertmanager/vmalert) with different poll cadences, auth, and failure modes. The existing
  fleet convention is **one bridge per external system** (gitlab/jira/slack/push/wl) — five
  precedents. Following it keeps each binary small, independently deployable, and
  independently failing. A fused binary couples a Grafana outage to alert routing.
- They *do* share the `internal/bridge` skeleton (SSE, StateManager, Dedup, TriggerEngine,
  EnsureConfigs) — so "two binaries" is not "two copies of everything," it's two thin
  `main.go`s over the same shared library. That's the right amount of sharing.

## 3. Recommended ordering (the approved plan)

```
Step 1  ──  W1 metrics MVP (devel)          [config-flip, lowest risk, unblocks value]
Step 2  ──  W2 grafana-bridge MVP            [author dashboards-as-beads over real data]
Step 3  ──  W3 alertmanager-bridge alerts-in [cheap: reuses existing parser]
Step 4  ──  W3 rules-out + W1 promote to perf/prod + W2/W3 hardening
```

**Why this order:**

1. **W1 first.** It is the only config-flip (no binary, no merge risk to the controller),
   it is the cheapest to ship, and it produces the *data* that makes W2 and W3 worth
   building. Shipping W1 to `devel` validates the ECS→EKS path and cost model before any
   code lands. This is the natural "prove the pipe" step.

2. **W2 second.** A bridge binary, but the write path already exists (the
   `grafana_dashboard="1"` sidecar) and the default ConfigMap path needs *no Grafana auth*.
   It turns the ACME data from Step 1 into iterable dashboards — the operator's headline
   ask ("iterate on it"). Lower integration risk than W3 (no external Alertmanager to wire).

3. **W3 alerts-in third.** Highest *leverage-per-line* because it reuses
   `trigger_source_webhook.go` (already groupKey-dedups and parses Alertmanager). But its
   *value* depends on (a) an Alertmanager actually firing at gardenio and (b) W1 metrics to
   alert on — so it sequences after W1+W2. The only real new code is the `resolved→close`
   handler and severity→bead-type routing.

4. **Step 4 hardening.** W3 rules-out (authoring rules as beads), promoting W1 from devel →
   perf → prod (gated on PII/PHI sign-off + cost), conflict-policy and silence/mute
   polish.

This ordering is also a **risk gradient**: config-flip → low-priv bridge → parser-reuse
bridge → cross-env promotion. Each step de-risks the next.

## 4. Cross-cutting dependencies & shared decisions

- **PII/PHI** ([[ka-0000001783]]): W1 composes with the source-side strip; metrics-first
  keeps labels bounded. **Logs/traces fan-out is explicitly out of MVP** and must wait for
  the full strip pipeline. This is a hard gate before W1 prod promotion.
- **Write-path convention**: both W2 and W3 default to **ConfigMap upsert + sidecar/reload**
  over direct HTTP API — less privilege, GitOps-friendly, restart-safe. Adopt this as the
  shared bridge-write idiom.
- **Bead-type registration**: W2 adds `dashboard`; W3 adds `alert_rule` + alert-routing
  fields. Both via `EnsureConfigs` on bridge startup (existing mechanism). Register in the
  same Phase-1 sub-task to avoid two migrations.
- **Site-deployment is outside this repo**: W1's site-side remote-write and W3's
  site-targeted rules-out are **advisory** — the bridge/doc produces the config (terragrunt
  var, rule YAML); a human lands it in site-deployment. gardenio never writes site terraform.
- **Trust direction**: every recommendation keeps site-deployment authoritative and
  gardenio a *consumer* (W1 push, not pull) and a *non-authoritative* control plane for site
  (W3 advisory). gardenio owning its own Alertmanager vs. pointing at ACME's is the one open
  infra question (W3 §8).

## 5. Risk & cost summary

| Risk | Mitigation | WS |
|---|---|---|
| ACME cardinality blows up gardenio vmsingle | `writeRelabelConfigs: keep` allow-list; re-size after perf phase; consider dedicated instance | W1 |
| PHI leaks via metric labels/logs | source-side strip (ka-0000001783) + gardenio labeldrop backstop; metrics-first; logs/traces deferred | W1 |
| Dashboard sync ping-pong | hash + `dashboard_source` loop-breaker; conflict policy | W2 |
| Two processes double-handle one alert | distinct `trigger:alert` label + shared groupKey dedup + CatchUpKeys | W3 |
| ECS→EKS network path unknown | devel via public-ingress+bearer first; promote to private (peering/PrivateLink) before prod | W1 |
| Controller regression from new binaries | two small bridges over shared lib; independently deployable; feature-flagged via Helm values | W2/W3 |

## 6. Phase-1 work breakdown (to be filed as child tasks under the epic once we start)

1. W1-impl: second remote-write on site devel vmagent → gardenio ingest; allow-list; bearer
   auth; ingress rule. (config + advisory site terragrunt)
2. W2-impl-a: register `dashboard` type; `cmd/grafana-bridge` skeleton; ConfigMap write
   path; backfill three shipped dashboards as beads.
3. W2-impl-b: Grafana read-back poll + hash loop-breaker + conflict policy.
4. W3-impl-a: register `alert_rule` type; `cmd/alertmanager-bridge` skeleton reusing
   `WebhookTriggerSource`; `resolved→close` + severity routing.
5. W3-impl-b: rules-out renderer (gardenio vmalert ConfigMap) + silence/mute beads.
6. Cross: Helm `grafanaBridge.*` / `alertmanagerBridge.*` values + Deployments; wire
   Alertmanager receiver → bridge.
7. W1-promote: devel → perf → prod, each gated on cost + PII sign-off.

## 7. Decision gate status

[[ka-0000004494]] resolved **`approve_all`** — Phase 1 is approved to proceed in the order
above. No further operator gate is required to *start* Step 1; the prod-promotion of W1
(Step 4 / breakdown #7) carries its own PII/cost checkpoint, which should be a fresh
`gb decision` at that time, not a blanket pre-approval.

## 8. Related

- Epic [[ka-0000006776]] · gate [[ka-0000004494]] · gardener trigger [[ka-0000008052]]
- W1 `obs-w1-victoria-to-grafana.md` · W2 `obs-w2-grafana-bridge.md` ·
  W3 `obs-w3-alertmanager-bridge.md`
- Centralization/PII [[ka-0000001783]] · Grafana epic [[ka-0000007895]] ·
  vmalert epic [[ka-0000006536]]
