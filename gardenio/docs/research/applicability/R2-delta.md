# R2 Applicability Delta — metric taxonomy & emitters vs tip-of-main

**Task:** ka-0000003104 · **Parent:** ka-0000001798 (E0) · **Umbrella:** ka-0000007704
**Re-validates:** [R2 — Time-Series Metrics Catalog](../timeseries-metrics-catalog.md) (ka-0000007788, committed `b775eadea` 2026-04-25)
**Audited at:** 2026-05-29, tip-of-main `755d74f69`

This doc answers the four E0 questions for R2: (1) which claims still hold, (2) what
drifted since 2026-04-25, (3) the shortest path to the v1 deliverable now, and (4) the
updated decision-point recommendation. It is the pinned cleanup + emitter diff list for E2.

---

## 0. TL;DR

- **R2's controller inventory (§1.1) is exactly correct** — all 26 controller metrics
  match name-for-name, the `_total`-suffixed gauges bug is real and unchanged, and the
  per-session push path (`gb session_metrics_push.go`) is intact.
- **R2's biggest factual claim is WRONG and was wrong at authoring time**: coop is *not*
  metrics-free. Coop has shipped a Prometheus `/metrics` endpoint since **2026-04-04**
  (PR #577, commit `7d0227494`) — three weeks *before* R2 was written. §1.3 ("No metrics
  emission") and §4.2 ("v1 adds metrics-exporter-prometheus") are both invalidated.
- **kbeads drifted substantially** since 2026-04-25: a whole new `kbeads_*` metric family
  (operated_by, edge-auth, xactor, token-revocation) plus `bead_references_*`, and the
  fleet collector gained a `project` label R2 didn't record. The `gardenio_*` vs `beads_*`
  vs **`kbeads_*`** prefix split is now a *three*-way split, worse than R2 reported.
- **slack-bridge now emits and serves metrics** (`gardenio_bridge_slack_oidc_*` on `:9093`,
  scraped) — R2 §1.4 said the bridges had no `/metrics` server. Partially obsolete.
- **coopmux also serves `/metrics`** (edge-auth verify counters) but is **NOT scraped** —
  no `prometheus.io/scrape` annotation on its deployment. Same for the in-pod coop
  endpoint: it exists but the agent pod template carries no scrape annotation.
- **vmagent still has no `env`/`cluster` external_labels** (R2 §3.2 action item) — the
  `global:` block is `scrape_interval`/`scrape_timeout` only. Still open.

**Net:** R2's *conventions* (§3), *cardinality discipline* (§3.5), *where-counters-live*
decision (§5), and *cost-metric shape* (§6) all still hold and are good. R2's *current-state
inventory* (§1) is stale in three of five subsystems. The shortest path to v1 is now
**cleanup-first**, because the drift introduced new naming inconsistencies (`kbeads_*`
prefix, unscraped endpoints) that compound the ones R2 already flagged.

---

## 1. Claims still holding

### 1.1 Controller `:9092` — **fully accurate** ✅

Every metric in R2 §1.1 exists with the stated name, type, and labels. Verified against
`gardenio/controller/internal/metrics/metrics.go` (namespace `gardenio`, agentLabels =
`{agent, project, role}`) and `internal/reconciler/metrics.go`:

- All 11 `agent_*` gauges, all 11 `account_*` gauges, `metrics_last_scrape_timestamp_seconds`
  (gauge), `metrics_scrape_errors_total` (counter), `agents_deferred_total` (gauge),
  `agent_deferred_wait_seconds` (histogram). 26 total, matching R2.
- **The `_total`-on-a-gauge bug (R2 §1.1 footnote ¹) is real and unchanged.**
  `agent_input_tokens_total`, `agent_output_tokens_total`, `agent_cache_read_tokens_total`,
  `agent_cache_write_tokens_total`, `agent_requests_total`, `agent_sessions_total`,
  `agent_tool_calls_total`, `agent_errors_total`, `agent_compactions_total` and the
  matching `account_*_total` set are all `prometheus.NewGaugeVec`. Still mislabeled.
- The per-session push path (`gb session_metrics_push.go`) is intact and pushes
  `gardenio_session_*` (cost_usd, tokens_total, duration_seconds, request_count,
  tool_calls_total, errors_total, compactions_total) to vmagent. R2 §4.1 correctly
  identified this as the one legitimate push case. Still true.

### 1.2 Conventions & design decisions — **still sound** ✅

- §3.1 naming scheme (`gardenio_<subsystem>_<thing>_<unit>`), §3.3 bounded label set, and
  §3.4 cardinality budget (≤200k) are unaffected by drift and remain the right targets.
- §3.5 forbidden-labels list (no `agent`/`bead_id`/`user` on event counters; grandfather
  `agent` on poller gauges only) is correct and is, if anything, *more* important now: the
  newly-shipped coop and coopmux endpoints both put `agent_name`/`account` labels directly
  on counters (see §2.2 below), so the discipline needs to be enforced at review time.
- §5 "kbeads server emits beads-event counters in the same path as the NATS publish" is
  still the right call. Nothing changed in the publisher topology; kbeads still runs one
  replica per the Helm chart, so the HA double-counting caveat is still deferred-to-v2.
- §6 cost-metric counter shape (`gardenio_agent_spend_usd_total{project,role,model,account}`)
  is unbuilt but uncontradicted — R3 still owns it; the contract holds.
- §7 query-time PromQL over recording rules — unaffected, still recommended.

### 1.3 kbeads embedding metrics — **mostly accurate** ✅ (renamed-source, same names)

R2 §1.2 listed `beads_embed_duration_seconds`, `beads_embed_errors_total`,
`beads_semantic_search_duration_seconds`, `beads_semantic_search_results_total`,
`beads_embedding_coverage_ratio`, `beads_embedding_stale_count`. All still exist with
those exact names — but the source moved from `internal/server/http_metrics.go` into a
dedicated `kbeads/internal/metrics/` package (`embedding.go`, `references.go`), registered
via `metrics.RegisterEmbedding(reg, …)` / `metrics.RegisterReferences(reg)` in
`http_metrics.go:205-206`. This is the §4.3 "shared `metrics/` package" pattern R2
*recommended* — it has been partially adopted for kbeads already. Note label drift:
`beads_embed_duration_seconds` now carries `{backend, op}` (R2 said `{model, backend}`),
and `semantic_search_duration_seconds` is split by `{phase}` (R2 said no labels).

---

## 2. Drift since 2026-04-25

### 2.1 ❌ Coop already had `/metrics` — R2 §1.3 / §4.2 invalidated

R2's headline claim "**Coop (Rust) has no metrics today** — `tracing` only … v1 adds
`metrics-exporter-prometheus`" is **false, and was false when R2 was written.**

`coop/crates/cli/src/transport/http/metrics.rs` — added **2026-04-04** in PR #577
(`7d0227494`, "feat(coop): add per-session Prometheus /metrics endpoint") — renders the
Prometheus text format **by hand** (not via the `metrics` crate), exposing six per-session
counters off the `UsageState` snapshot:

| Metric | Type | Labels |
|---|---|---|
| `gardenio_session_input_tokens` | counter | `agent_name, project, account` |
| `gardenio_session_output_tokens` | counter | `agent_name, project, account` |
| `gardenio_session_cache_read_tokens` | counter | " |
| `gardenio_session_cache_write_tokens` | counter | " |
| `gardenio_session_request_count` | counter | " |
| `gardenio_session_api_ms_total` | counter | " |

Route registered at `coop/crates/cli/src/transport/mod.rs:494` (`/metrics`), auth-bypassed
(`auth.rs:87`). **Implications for v1:**
- §4.2's recommendation ("add `metrics` crate + `metrics-exporter-prometheus`") should be
  *re-scoped*: coop already emits via hand-rolled text format. The decision is now
  "keep hand-rolled vs migrate to the `metrics` crate", not "greenfield".
- These counters lack the `_total` suffix that Prometheus convention wants on a counter
  (`gardenio_session_input_tokens` should be `…_total`). New naming bug, coop-side.
- The labels `agent_name` and `account` are **directly on counters** — `agent_name` is a
  §3.5-forbidden high-cardinality label. Tolerable today only because the endpoint is
  per-pod and the series die with the pod, but it must not be aggregated cluster-wide
  without dropping `agent_name` at scrape time.

### 2.2 ❌ coopmux `/metrics` shipped — and is NOT scraped

`coop/crates/mux/src/transport/http.rs:130` serves `/metrics` (route at
`transport/mod.rs:98`), rendering edge-auth verify counters
(`s.edge_auth_metrics.render_prometheus`, `transport/edge_auth.rs:245`). Added
2026-05-22 (`3a2538fff`, "feat(coop/mux): daemon-side edge-auth HMAC verify").

**Gap:** `gardenio/helm/gardenio/templates/coopmux/deployment.yaml` has **no
`prometheus.io/scrape` annotation** (only `rollout.kubernetes.io/restartedAt`). The
endpoint exists but vmagent never scrapes it. R2 §4.1 assigned coopmux a "new scrape
target" — that work is half-done (endpoint up, annotation missing).

### 2.3 ❌ In-pod coop endpoint not scraped either

The agent pod template (`gardenio/controller/internal/podmanager/`) carries **no
`prometheus.io/scrape` annotation** — confirmed by grep (0 matches in podmanager `.go`).
So the §2.1 per-session counters are exposed inside every agent pod but never collected.
The push path (§1.1) is what actually delivers per-session data today; the scrape endpoint
is dark. R2 §9 decision #5 ("coop scrape-target = agent pod or coopmux only? **Both**") is
unimplemented on both legs.

### 2.4 ❌ slack-bridge now emits + serves metrics — R2 §1.4 partially obsolete

`gardenio/controller/cmd/slack-bridge/helpers.go:277` mounts `GET /metrics` (promhttp) on a
real metrics server (`main.go:248`, default `:9093` via `resolveMetricsAddr`), and the
slack-bridge deployment **is** scraped (`templates/slack-bridge/deployment.yaml:26-28`,
port 9093, path /metrics). The emitted metrics
(`internal/bridge/slack_oidc_operator_alias_resync.go:44-57`):

| Metric | Type |
|---|---|
| `gardenio_bridge_slack_oidc_alias_coverage` | gauge |
| `gardenio_bridge_slack_oidc_resync_last_success_timestamp_seconds` | gauge |
| `gardenio_bridge_slack_oidc_resync_aliases_inserted_total` | counter |
| `gardenio_bridge_slack_oidc_resync_operators_merged_total` | counter |

These already follow R2 §2.4's `gardenio_bridge_*` namespace — good. **But there is a
registry-wiring bug to watch**: these four are declared with `promauto.NewGauge/NewCounter`
(default registry), while the actual `/metrics` handler in `helpers.go:273` builds a
**fresh `prometheus.NewRegistry()`** and registers only the Process + Go collectors on it.
The resync code runs inside `internal/bridge/bot.go:617` (`startSlackOIDCOperatorAliasResync`),
which is hosted by the **controller** binary, not the slack-bridge binary — so on the
controller's `:9092` (which *does* swap `DefaultRegisterer`, `main.go:226-227`) they appear,
but on the slack-bridge's own `:9093` they may **not** appear because that handler uses a
non-default registry. **E2 must verify which binary actually exposes these four series and
on which port** — there is a real chance they are emitted-but-not-scraped on the path the
deployment annotation points at. (Pinned as E2 action A-7.)

### 2.5 ❌ New `kbeads_*` metric family — three-way prefix split

R2 §1.2 reported a two-way `gardenio_fleet_*` / `beads_*` split and recommended normalizing
to `gardenio_*`. Since then a **third** prefix `kbeads_*` appeared, registered in
`http_metrics.go:191-206`:

| Metric | Type | Source | Added |
|---|---|---|---|
| `kbeads_token_revocations_total` | counter | `token_revocation_metrics.go` | (pre-existing as `beads_tokens_revoked_total`, renamed) |
| `kbeads_revoked_tokens` | gauge | " | " |
| `kbeads_xactor_override_total` | counter | `xactor_metrics.go` | new |
| `kbeads_edge_auth_verify_total` | counter | `edge_auth_metrics.go` | 2026-05-22 (`162a7563a`) |
| `kbeads_operated_by_header_arrival_total` | counter | `operated_by_metrics.go` | 2026-04-30 (`973a3e560`, P6a) |
| `kbeads_operated_by_writes_total` | counter | " | " |
| `kbeads_operated_by_resolver_errors_total` | counter | " | " |
| `kbeads_operated_by_fallback_total` | counter | " | " |
| `kbeads_operated_by_ambiguous_agent_total` | counter | " | " |
| `kbeads_writes_with_agent_operator_total` | counter | " | " |
| `bead_references_extracted_total` | counter | `internal/metrics/references.go` | 2026-04-28 (`c512ccdec`) |
| `bead_references_extract_duration_seconds` | histogram | " | " |

Note R2's §1.2 `beads_tokens_revoked_total` / `beads_tokens_revoked_current` are now named
`kbeads_token_revocations_total` / `kbeads_revoked_tokens` — already partially renamed but
to the *wrong* target (`kbeads_*`, not the `gardenio_beads_*` R2 §2.2 prescribed). And
`bead_references_*` has **no prefix at all**. So the live prefix set is now:
`gardenio_*` (controller, fleet, bridge, session) · `beads_*` (embedding) · `kbeads_*`
(auth/operated-by/xactor/tokens) · `bead_references_*` (unprefixed). **Four** naming
conventions in flight. R2's normalization recommendation (§9 decision #2) is now larger
in scope than R2 estimated.

### 2.6 ⚠ Fleet collector gained a `project` label

R2 §1.2 listed `gardenio_fleet_total_cost_usd`, `gardenio_fleet_active_agents`,
`gardenio_fleet_sessions_total` as having **no labels**. They now all carry `{project}`
(`http_metrics.go:50,55,60`), and `gardenio_fleet_sessions_total` is correctly emitted as
`prometheus.CounterValue` while the others are `GaugeValue`. Minor, positive drift — but it
means the §3 cardinality back-of-envelope shifts slightly and the `_total` suffix on
`fleet_sessions_total` is now (correctly) a counter, unlike its controller `_total` cousins.

### 2.7 ⚠ New dashboard, vmagent external_labels still missing

- New Grafana dashboard `gardenio/helm/gardenio/dashboards/operated-by.json` (P6a, shipped
  2026-04-30) joins `cost-overview.json` and `kbeads-embedding.json`. R2 §1.5 listed only
  the latter two.
- **vmagent still has no `env`/`cluster` external_labels.** The `global:` block in
  `templates/vmagent/configmap.yaml:11-13` is `scrape_interval` + `scrape_timeout` only.
  R2 §3.2's explicit action item is **still open** — cross-environment dashboards still
  alias. This is the single highest-leverage cleanup (one config block, unblocks every
  multi-env panel).

---

## 3. Shortest path to the v1 deliverable now

R2's plan assumed a cleaner floor than reality. The drift means v1 must **clean up four
naming conventions and two dark endpoints** before (or alongside) adding new metrics.
Re-ordered for least churn / most unblocking:

**Phase E2-a — config + wiring (no code churn, unblocks dashboards & dark endpoints):**
1. Add `env` + `cluster` `external_labels` to vmagent `global:` (R2 §3.2). One block.
2. Add `prometheus.io/scrape`/`port`/`path` annotations to the **coopmux deployment** and
   the **agent pod template** (podmanager) so the already-shipped coop/coopmux `/metrics`
   endpoints actually get collected (R2 §9 #5). Decide first whether per-pod coop scrape is
   wanted at all given the push path already delivers per-session data — may be a no-op by
   choice, in which case *document* that the endpoint is intentionally unscraped.
3. Verify slack-bridge metric exposure (A-7, §2.4): confirm `gardenio_bridge_slack_oidc_*`
   land on the scraped port; fix the default-vs-fresh-registry mismatch in
   `slack-bridge/helpers.go` if they don't.

**Phase E2-b — naming cleanup (dashboard churn, do as one breaking pass):**
4. Normalize prefixes → `gardenio_*`: `beads_*` (embedding) → `gardenio_beads_embed_*`,
   `kbeads_*` → `gardenio_beads_*`, `bead_references_*` → `gardenio_beads_references_*`
   (R2 §9 #2, now 4-way instead of 2-way). Rewrite the three dashboards in the same MR.
5. Fix the `_total`-on-gauge bug. Two sub-paths, pick per R2 §1.1 footnote:
   - controller poller gauges (`agent_*_total`, `account_*_total`): rename to `_current`
     (they can't be true counters from a poller). R2 §9 #1.
   - coop session counters (`gardenio_session_*`): add the missing `_total` suffix
     (they *are* monotonic counters, just unsuffixed) — new, coop-side, not in R2.

**Phase E2-c — new emitters (the actual v1 feature work, R2 §2):**
6. `gardenio_beads_events_total{event,type,fleet}` in the kbeads NATS-publish path (R2 §2.2,
   §5) — the core operator-brief deliverable ("beads events by type").
7. `gardenio_agent_lifecycle_events_total` + decision/session/advice event counters (R2 §2.1–2.2).
8. Defer R2 §2.5 (`gardenio_mr_*`) and the cost counter §6 to their owners (gitlab-bridge / R3).

**Critical-path insight:** items 1–3 are pure config/wiring and unblock everything else with
zero risk. Item 6 is the smallest code change that satisfies the operator's literal brief
("beads events by type"). If v1 scope must be cut, **ship 1, 3, 6** and call it v1; defer
the prefix-normalization breaking pass (4, 5) to a v1.1 once dashboards are owned.

---

## 4. Updated decision-point recommendation (R2 §9 re-rated)

| R2 §9 decision | R2 rec | Updated rec @ 2026-05-29 | Why changed |
|---|---|---|---|
| #1 Rename `agent_*_total` gauges → `_current` | yes | **yes, + add `_total` to coop session counters** | coop drift added a *second* class of suffix bug (counter missing `_total`) |
| #2 Normalize `beads_*` → `gardenio_beads_*` | yes | **yes, but now 4-way** (`beads_*`, `kbeads_*`, `bead_references_*` → `gardenio_beads_*`) | `kbeads_*` + `bead_references_*` families shipped since R2 |
| #3 Drop `gardenio_agent_cost_usd` gauge after R3 counter | yes | **unchanged — still after R3** | R3 hasn't landed; no new info |
| #4 HA kbeads double-count | defer v2 | **unchanged — still one replica** | Helm chart still single-replica |
| #5 Coop scrape target = agent pod + coopmux | both | **endpoints exist, annotations missing — decide intent first** | both endpoints shipped but unscraped; per-pod coop may be redundant with push path |
| #6 Bridge ports 9094/9095/9096 | assigned | **OBSOLETE — slack-bridge already uses `:9093`** | slack-bridge shipped on 9093, not 9094; jira/gitlab bridges still have no endpoint |

**New decision for the synthesis pass (not in R2 §9):**
- **D-7: Migrate coop's hand-rolled `/metrics` to the `metrics` crate, or keep it?**
  Recommend **keep hand-rolled for v1** — it works, it's six counters, and adding the
  `metrics` crate is dependency churn for no functional gain. Revisit only when coop needs
  histograms (tool-call latency, R2 §2.6), which the hand-rolled writer can't cleanly do.

---

## 5. Pinned E2 cleanup + emitter diff list

Actionable checklist for the E2 implementation epic:

- **A-1** vmagent: add `env` + `cluster` external_labels. `templates/vmagent/configmap.yaml`.
- **A-2** coopmux deployment: add `prometheus.io/scrape|port|path` annotations.
- **A-3** agent pod template: add scrape annotation **or** document intentional non-scrape.
- **A-4** Fix `_total`-on-gauge: controller `agent_*_total`/`account_*_total` → `_current`.
- **A-5** Fix coop counters missing `_total`: `gardenio_session_*` → `gardenio_session_*_total`.
- **A-6** Prefix normalization (4-way) → `gardenio_beads_*`; rewrite 3 dashboards same MR.
- **A-7** Verify `gardenio_bridge_slack_oidc_*` land on the scraped port; fix registry split.
- **A-8** New: `gardenio_beads_events_total{event,type,fleet}` in kbeads publish path.
- **A-9** New: `gardenio_agent_lifecycle_events_total` + decision/session/advice counters.
- **A-10** Defer: `gardenio_mr_*` (gitlab-bridge), cost counter §6 (R3).

---

## 6. Sources (tip-of-main `755d74f69`, 2026-05-29)

- `gardenio/controller/internal/metrics/metrics.go` — controller poller (verified §1.1)
- `gardenio/controller/internal/reconciler/metrics.go` — deferred histogram
- `gardenio/controller/cmd/gb/session_metrics_push.go` — per-session push
- `gardenio/controller/cmd/slack-bridge/helpers.go:273-277` + `main.go:244-248` — bridge `/metrics`
- `gardenio/controller/internal/bridge/slack_oidc_operator_alias_resync.go:44-57` — bridge metrics
- `gardenio/controller/internal/bridge/bot.go:617` — resync hosted in controller binary
- `kbeads/internal/server/http_metrics.go` — fleet collector + registrations
- `kbeads/internal/metrics/embedding.go`, `references.go` — shared metrics package
- `kbeads/internal/server/{operated_by,edge_auth,xactor,token_revocation}_metrics.go` — `kbeads_*`
- `coop/crates/cli/src/transport/http/metrics.rs` + `mod.rs:494` — coop `/metrics` (since 2026-04-04)
- `coop/crates/mux/src/transport/http.rs:130` + `mod.rs:98` — coopmux `/metrics` (since 2026-05-22)
- `gardenio/helm/gardenio/templates/vmagent/configmap.yaml` — no external_labels
- `gardenio/helm/gardenio/templates/{coopmux,controller,beads,slack-bridge}/deployment.yaml` — scrape annotations
- `gardenio/helm/gardenio/dashboards/` — cost-overview, kbeads-embedding, **operated-by** (new)
