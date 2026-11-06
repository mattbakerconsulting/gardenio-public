# Customer-1 telemetry plan-of-record — user engagement, uniqueness & security events

**Bead:** ka-0000003532 (R0, research → plan-of-record)
**Parent:** ka-0000004205 — _Epic: Gardenio as customer-1 — user engagement, uniqueness & security-event telemetry_
**Umbrella:** ka-0000007704 — _gardenio time-series metrics + chart platform_
**Status:** research / design only. **No code lands from this doc.** Every implementation child is gated behind an operator `gb decision` (umbrella constraint). Operator: oidc:matthew.baker@acme.example.

Companion v1 research (already landed under the umbrella; this plan _extends_ them, does not re-open them):

- R1 — `timeseries-victoriametrics-ops.md` (vmsingle + vmagent scrape/push, cardinality budget)
- R2 — `timeseries-metrics-catalog.md` (taxonomy, label conventions, emitter sites)
- R3 — `timeseries-spend-attribution.md` (session-close push, exemplars for high-cardinality ids)
- R4 — `timeseries-beads-events-metrics.md` (beads lifecycle → metrics)
- R5 — `timeseries-chart-backend-arch.md` (in-house chart backend)
- R6 — `timeseries-charts-ux.md` (web + iOS chart UX)

Cross-referenced for this plan: `operated-by-transitivity-r1..r5`, `obs-centralization-w2-pii.md`, `per-agent-tokens-*.md`, `c1-token-ttl-refresh-design.md`, `forwardauth-403-force-logout*.md`, `per-user-homepage-design.md` §9.

---

## TL;DR

1. **The gap is real and the fix is small.** v1 instruments **agents** and **beads events** exhaustively but is blind to **humans (operators)** and to **security events**. The four mandated scopes (engagement, uniqueness, security, privacy) converge on one cardinality-safe answer: treat the human as a **bounded, first-class telemetry subject keyed on the already-built `operators` FK**, and emit security events at the kbeads/controller sites that **already record them** — no new consumers in v1.

2. **One backbone counter covers engagement + uniqueness.** `gardenio_operator_activity_total{operator_id, project, activity_type, surface}` is the single counter behind per-surface engagement *and* DAU/WAU/MAU. Uniqueness is computed **query-time** (`count(count by (operator_id) (...))` over rolling windows) — no HyperLogLog, no recording rules, no new infra at gardenio's scale. Precomputed rollup gauges are a v2 optimization gated on a benchmark.

3. **The "operators substrate doesn't exist" blocker is STALE.** Verified in-repo: migrations **015** (`operators` + `operator_aliases` tables), **017** (cutover), **018** (`created_by_operator_id`), **027** (email required), **028** (operator slugs); FK resolution lives in `kbeads/internal/server/audit.go` (`resolveAuditOperator` → `operators(id)`). `operator_id` is a ready, bounded, opaque, privacy-safe label **today**. There is no `BEADS_OPERATED_BY_ENABLED` flag in the Go source — the research-phase assumption that operator events were disabled is removed from this plan.

4. **Privacy is satisfied by labeling on `operator_id` only.** `oidc_email`, `slack_user_id`, display names, `session_id`, raw tokens, bead/trace ids are **never** live-series labels. `operator_id` → display/email resolves **server-side** in the chart backend, never in the hot metric path. This is the producer-side pre-hash discipline that `obs-centralization-w2-pii.md` calls defense-in-depth in front of the mandatory `pii-gateway`.

5. **Security events ride existing emit sites.** Authz denials at `kbeads/internal/server/audit_log.go` (`auditDenial`), decision/escalation lifecycle at the eventbus dispatch, token lifecycle at the controller issuance/revocation sites. Keycloak auth events and forwardauth deny-reason granularity are **explicitly deferred to v2** as genuinely-new infra with a documented contract.

6. **Everything is gated.** Per the umbrella constraint, the first actionable bead is an operator `gb decision` (label-dimension sign-off + DAU "active" semantics + token-revocation SLO). It **blocks all three implementation epics** — nothing builds until it resolves.

---

## 1. The gap, scope by scope

### 1.1 User engagement — who uses which surface, how often

Three surfaces, three operator-identity paths, all resolvable to the unified `operators` FK:

| Surface | Operator identity at entry | Evidence |
|---|---|---|
| **Slack bridge** | `slack:<user-id>` stamped via `WithOperator(ctx, …)` on every mention/command/reaction/callback | `gardenio/controller/internal/bridge/bot_mentions.go:28-37`, `bot_commands.go:42-50`, `bot_reactions.go:90`, `bot.go:777,1057,1117` |
| **beads-viewer web** | OIDC email via `oidcauth.EmailFromContext(ctx)`; already logged to slog telemetry | `gardenio/controller/cmd/beads-viewer/homepage_telemetry.go:28-66`, `oidcauth.go:35-39` |
| **iOS (Gardenio app)** | OIDC token (Keycloak); email in `UserDefaults`. No client telemetry today | `usr/Gardenio/Gardenio/OIDCAuthManager.swift:14,30,46-52`, `KbeadsClient.swift` |

**Key precedent already in the tree:** beads-viewer emits `homepage.user_home.view` / `homepage.inbox.view` / `homepage.search.view` structured slog events keyed on `viewer_email`, deliberately *without* a Prometheus dependency, with PII-bounded fields (`refererPath` strips query strings). This plan **promotes** that slog pattern to a Prometheus counter keyed on `operator_id` — it does not invent a new surface.

**Bridges have no metrics today** — `gardenio/controller/internal/bridge/` exposes no `/metrics` and imports no Prometheus client. R2 §1.4 already flags this. The engagement counter is the first metric the bridge emits.

**iOS is server-inferred, not client-instrumented (v1).** iOS is a thin client: every action is a kbeads API call carrying the OIDC token. The backend observes `operator_id` from the token claim at request time. iOS engagement (`surface=ios`) is derived from kbeads request observation — there is **zero client-side iOS telemetry in v1** (an honest limitation, see §6).

### 1.2 Uniqueness — DAU/WAU/MAU without a cardinality bomb

The naïve approach — a distinct time series per active user per window — is exactly the `session_id` cardinality trap flagged in R3 (`timeseries-spend-attribution.md:257,328`) and R1's budget (`timeseries-victoriametrics-ops.md:99`).

The safe answer exploits the asymmetry: **unique operators are bounded (O(50–500))** while unique *sessions* are unbounded. DAU/WAU/MAU therefore = **query-time distinct-operator count** over the backbone counter:

```promql
# DAU — distinct operators active in the last day
count( count by (operator_id) ( last_over_time(gardenio_operator_activity_total[1d]) ) )
# WAU / MAU — same with [7d] / [30d]
```

`last_over_time` (not raw `increase`) is used so a counter reset / pod restart inside the window does not drop an operator from the distinct set — distinct-membership only needs "did this series exist with samples in the window", which `last_over_time` answers correctly. At gardenio's series volume (low tens of thousands) vmsingle evaluates this without recording rules. **"Active" is defined (v1) as: an operator who authored a bead mutation, a spawn, or a session — not a passive page-viewer.** This is sub-question (b) of the operator decision gate.

Precomputed rollup gauges (`gardenio_operator_dau/wau/mau`, kbeads nightly push) are **deferred to v2**, built only if a benchmark shows query-time p99 > 500 ms, and would require a companion staleness gauge + a `>25h` alert.

### 1.3 Security events — emit where the event is already recorded

| Event class | Emit site (existing) | v1 disposition |
|---|---|---|
| **Daemon authz denials** | `kbeads/internal/server/audit_log.go:95` `auditDenial` (writes an audit bead today) | v1 — add counter |
| **Decision / escalation lifecycle** | eventbus dispatch for `EventDecisionCreated/Responded/Escalated/Expired` (`kbeads/internal/eventbus/types.go:34-37`) | v1 — add counters |
| **Agent token lifecycle** | controller token issuance / revocation sites (per `per-agent-tokens-lifecycle.md`, `c1-token-ttl-refresh-design.md`) | v1 — add counters + latency histogram |
| **Edge-auth 403 (forwardauth)** | landing `/403` handler (`forwardauth-403-force-logout*.md`) | v1 **conditional** — emit only if the handler exists; else document v2 contract |
| **Keycloak auth events** (login success/fail, token issuance) | Keycloak event-listener (does not exist) | **v2** — genuinely-new sidecar; contract documented only |

Metrics are designed so a future `vmalert` rule (deferred in v1 §7) can be written against them without re-instrumentation: bounded `reason`/`action`/`outcome` enums, role rather than identity on escalation, token SLO buckets.

### 1.4 Privacy boundary — `operator_id` is the whole game

`obs-centralization-w2-pii.md` classifies operator email as **`pii-direct`** (default action: **hash**) and mandates a redaction `pii-gateway` in front of the common collector. The correct discipline here is to **pre-hash at the emitter** — never emit the email at all; emit the opaque `operator_id`. The gateway rule then becomes a defense-in-depth catch-all, not the load-bearing tier for this data. See the full allow/deny list in §4.

---

## 2. Recommended approach

### 2.1 Backbone: one operator activity counter, reusing the existing push/scrape path

`gardenio_operator_activity_total{operator_id, project, activity_type, surface}` is the single counter behind engagement *and* uniqueness. Emit path **reuses what exists**:

- **Mutation** (`activity_type=mutation, surface=kbeads`) — kbeads resolves the operator FK on every write (`audit.go` `resolveAuditOperator`). Add the counter at the kbeads HTTP metrics exporter alongside the existing inventory (`kbeads/internal/server/http_metrics.go`).
- **Spawn** (`activity_type=spawn, surface=slack`) — the Slack bridge already stamps the operator (`bot_commands.go`); the spawned agent bead carries `operated_by_id`.
- **Session** (`activity_type=session`) + **per-operator spend** — extend the session-close push (`gardenio/controller/cmd/gb/session_metrics_push.go`). **See §3.1 — this is NOT a free label add.**
- **View** (`activity_type=view, surface=web`) — beads-viewer HTTP middleware reads OIDC identity, resolves to `operator_id` server-side, increments the counter. The `homepage_telemetry.go` slog pattern is the template, promoted to Prometheus.

### 2.2 Identity: lean on the mature `operators` FK, do not invent a format

`operators(id, kind, subject, display, email, slug)` + `operator_aliases(kind, subject → operator_id)` already unify `slack:U…`, `oidc:email`, `jira:…`, `gitlab:…` into one numeric `operator_id` (migrations 015/017/028). **All operator-keyed metrics label on `operator_id` only.** Resolution to display/email happens server-side in the chart backend / beads-viewer.

### 2.3 Genuinely-new infra / operator-decision items (flagged, not assumed)

1. The umbrella **`gb decision` to approve any implementation** (blocks everything).
2. Introducing **`operator_id` as a NEW metric label dimension** beyond the v1 set (`agent_name/project/role/model/env/task_id`) — cardinality-budget sign-off.
3. **Keycloak event-listener sidecar** (v2) and **oauth2-proxy deny-reason header** — new infra, deferred, contract documented only.
4. **Token-revocation SLO targets** (95%/5min, 99%/60min vs tighter) — affects janitor sweep cadence and provider rate-limit pressure.

---

## 3. Verified implementation realities (corrections from adversarial review)

A completeness critic verified the plan's code claims against the repo. Five corrections are folded into the bead descriptions below so no task starts on a false premise:

### 3.1 Session-push `operator_id` is a daemon lookup, not a free label
`session_metrics_push.go:96-97` `formatLabels` builds labels purely from env (`agent_name,project,account,session_id`); `session.go:307,463` pass env-derived values. **`operated_by_id` is not in scope at the push site.** Resolving `operator_id` requires the daemon to look up the agent bead (`KD_AGENT_ID`/`BOAT_AGENT_BEAD_ID`) and read `operated_by_id`. This is real work, scoped accordingly.

### 3.2 Manual / non-bridge spawns can have a NULL operator
`operated_by_id` is **NULLABLE** for legacy/sentinel rows (`audit.go:541,611`) and the S3/manual-resume path (`session.go:348-352`) has no guaranteed operator at push time. The backbone counter and spend metric **must define a fallback bucket** — `operator_id="0"` (the reserved "unattributed" sentinel) — rather than dropping the sample or emitting an empty label.

### 3.3 Authz `reason` does not exist as a field — it is derived
`AuthzDenial` (`kbeads/internal/server/write_authz.go:27-35`) has `Action, BeadType, RequiredRole, RequiredScope, Escalatable, Message` — **no `reason`**. The `reason` enum must be **derived at each denial branch** (`write_authz.go:73,203,219`; `beads.go:215,787`), not "validated at config-load". The denial-counter task includes building that mapping.

### 3.4 `bead_type` needs a hard allowlist at emit
`bead_type` is `string(beadType)` taken from the request (`write_authz.go:75`, `beads.go:217`) and kbeads supports config-defined custom types — so without a hard allowlist at the emit site, novel types inflate cardinality. The ~20-type cap with overflow → `"custom"` must be enforced in code, not assumed.

### 3.5 DAU/WAU/MAU correctness, not just latency
Distinct-operator counting over a **counter** is sensitive to resets/staleness; the query uses `last_over_time(...[window])` (§1.2) for membership, and the benchmark task asserts **correctness** (known distinct-operator fixture) in addition to p99 latency.

---

## 4. Privacy allow / deny list (applied to every proposed label)

| Field | Disposition | Rationale |
|---|---|---|
| `operator_id` | **label-ok** | Opaque numeric FK (migration 015). Bounded O(50-500), zero PII, internal cluster only. The single canonical operator-identifying label. `operator_id`→display/email resolves server-side, never in the hot path. |
| `project` | **label-ok** | Existing v1 label, bounded O(10-50), non-PII config value. |
| `role` / `escalated_by_role` | **label-ok** | Bounded enum (~15), non-PII. Preferred over `operator_id` on escalation metrics to keep override-pattern analysis identity-free. |
| `activity_type` · `surface` · `action` · `reason` · `bead_type` · `event` · `urgency` · `provider` · `status` · `revocation_type` · `outcome` | **label-ok** | Bounded enumerations. `reason` and `bead_type` MUST reject/bucket unrecognized values to prevent unbounded drift (§3.3, §3.4). |
| `route_template` | **label-ok** | Allowlisted Traefik route templates (≤15); unknown → `other`. NEVER the raw URL. |
| operator email (`oidc_email`) | **hash-then-label** | `pii-direct` per `obs-centralization-w2-pii.md` §2.2 (default action HASH). NEVER a direct label. If hash-keyed lookup is needed, use the optional `gardenio_operator_email_resolution{operator_id, email_hash}` gauge (HMAC-SHA256, `h_` prefix, internal-only, 1:1 with `operator_id`). Prefer `operator_id`. |
| `slack_user_id` | **never** | Private provider ID, forbidden as a label (R3 privacy). Maps to `operator_id` via `operator_aliases`. |
| operator display / real name | **never** | `pii-direct` cleartext. Lives in `operators.display`; resolved server-side only. |
| `session_id` | **never** (on operator counters) | High-cardinality. `session_metrics_push.go:97` emits it today only because session series are single-sample + short-lived; it **MUST NOT** propagate to the new operator-keyed counters (§5 cardinality note). |
| raw token / `provider_id` | **never** | Credential material / unbounded. Token metrics label `provider`+`status` only. |
| `bead_id` / `trace_id` / `span_id` | **aggregate-only** | High-cardinality; OpenMetrics exemplars (~1% sample) only, never live-series labels. |
| `error_message` / raw URL / commit SHA | **never** | Unbounded free-text. Route to logs/exemplars (metrics catalog §3.3). |

**Right-to-be-forgotten:** handled via soft-delete — `operator_id` persists in historical series (immutable), email resolves to `"deleted operator"` server-side. No metric rewrite required.

---

## 5. Consolidated metric design

`operator_id` is bounded O(50–500); all other labels are bounded enums or config values. Worst-case added series across the set is well under the R1 200k vmsingle budget (realistically O(2k–20k)).

| Metric | Type | Labels | Scope | Cardinality / notes |
|---|---|---|---|---|
| `gardenio_operator_activity_total` | counter | `operator_id, project, activity_type, surface` | engagement + uniqueness (backbone) | `activity_type`∈{spawn,mutation,session,view}; `surface`∈{slack,web,ios,cli,kbeads}. Dedupes the engagement scope's surface-view/feature-usage counters and the uniqueness scope's activity counter into ONE. DAU/WAU/MAU derived query-time (§1.2). NULL operator → `operator_id="0"` (§3.2). |
| `gardenio_operator_session_duration_seconds` | histogram | `operator_id, project, role` | engagement + uniqueness | Extends `gardenio_session_duration_seconds` with `operator_id`+`role`. Buckets 30s…1d. |
| `gardenio_operator_cost_usd_total` | counter | `operator_id, project, model` | customer-1 spend-by-human | Reuses session-close push; `operator_id` resolved per §3.1. Replaces the privacy scope's `gardenio_user_spend_usd_total`. |
| `gardenio_user_request_latency_seconds` | histogram | `surface, endpoint, status_code` | engagement (surface health) | Buckets 100ms…30s. **No** `operator_id` label (per-endpoint, not per-human); identity via exemplar. `endpoint` = bounded route template. |
| `gardenio_authz_denials_total` | counter | `action, reason, bead_type` | security (daemon authz) | At `audit_log.go` `auditDenial`. `reason` derived (§3.3); `bead_type` allowlisted (§3.4). No `operator_id` (drill-down via audit bead). |
| `gardenio_decisions_lifecycle_total` | counter | `event, urgency` | security + engagement | `event` maps 1:1 to eventbus `EventDecision*`. Reconciles the engagement scope's decision-resolution counter. |
| `gardenio_escalations_total` | counter | `escalated_by_role, outcome` | security (override signal) | Role, **not** `operator_id` (identity-free override analysis; identity in audit bead). |
| `gardenio_agent_token_provisions_total` | counter | `provider, status` | security (token lifecycle) | At controller issuance. Raw token id NEVER a label. |
| `gardenio_agent_token_revocations_total` | counter | `provider, revocation_type` | security (token lifecycle) | Companion to the latency histogram. |
| `gardenio_agent_token_revocation_latency_seconds` | histogram | `provider, revocation_type` | security (revocation SLO) | Buckets 5s…4h. Backs the operator-set SLO (decision gate). |
| `gardenio_forwardauth_403_total` | counter | `reason, route_template` | security (edge auth, **v1-conditional**) | Emit only if landing `/403` handler exists; deny-reason granularity is v2. No operator label. |
| `gardenio_auth_attempts_total` | counter | `event, provider, outcome` | security (Keycloak, **v2-deferred**) | Requires Keycloak event-listener sidecar. v1 documents the contract; no emit. |
| `gardenio_operator_email_resolution` | gauge | `operator_id, email_hash` | privacy (optional internal resolver) | HMAC-SHA256, `h_` prefix, internal-only, 1:1 with `operator_id`. **Default OFF** pending operator decision. |
| `gardenio_operator_dau / _wau / _mau` | gauge | `date` / `week_start` / `month_start` | uniqueness (**v2 optimization, deferred**) | Precomputed kbeads nightly rollup. Built only if query-time p99 > 500ms. Needs staleness gauge + `>25h` alert. |

---

## 6. Known limitations / honest gaps (v1)

- **iOS is server-inferred only** — no client-side telemetry; `surface=ios` is derived from kbeads request observation by `operator_id`. True iOS feature-level engagement (tab views, button taps) is a v2 client-instrumentation item.
- **Edge-auth / Keycloak coverage is partial in v1** — forwardauth 403 emits only if the landing handler exists; login success/fail and token-issuance events are v2 (Keycloak listener). A meaningful slice of the "security events" scope is deferred, by design, to avoid standing up new infra in v1.
- **DAU/WAU/MAU is query-time** — correctness depends on `last_over_time` membership semantics (§3.5); precomputed rollups are deferred.
- **Spend-by-human depends on `operated_by_id` resolution at session close** — null-operator sessions roll up to the `operator_id="0"` bucket (§3.2).

---

## 7. Sequenced implementation plan

### Phase 0 — OPERATOR DECISION GATE (blocks everything)
A `gb decision` packaging three sign-offs: (a) introduce `operator_id` as a new label dimension + cardinality-budget delta; (b) DAU/WAU/MAU "active" semantics (author-of-mutation/spawn/session, v1 excludes passive views) — or operator override; (c) token-revocation SLO target. **No implementation bead may move to in_progress until this resolves.**

### Phase 1 — Engagement + uniqueness backbone (after gate)
1. `operator_id` on the session-close push (§3.1) — smallest, highest-leverage; unlocks per-operator spend + session duration.
2. `gardenio_operator_activity_total` at the kbeads mutation path + Slack spawn path.
3. beads-viewer HTTP middleware promoting `homepage_telemetry` slog → Prometheus view counter.
4. DAU/WAU/MAU query-time panels + correctness/p99 benchmark; gate v2 rollups on failure.

### Phase 2 — Security telemetry (parallel to Phase 1, after gate)
5. Authz denial counter at `audit_log.go` (with derived `reason`, allowlisted `bead_type`).
6. Decision lifecycle + escalation counters at eventbus dispatch.
7. Token lifecycle counters + revocation latency histogram (SLO from Phase 0).
8. forwardauth 403 counter — conditional; else document the v2 contract.

### Phase 3 — Privacy enforcement + chart UX (gates Phases 1/2 to prod)
9. `pii-gateway` catalog catch-all rule + CI/relabel rule enforcing the bounded operator-label allow-list.
10. Chart backend + web/iOS UX: operator engagement dashboards, DAU/WAU/MAU panels, security overview — server-side `operator_id` → display resolution.

### Explicitly deferred to v2 (re-gated when reached)
Keycloak event-listener sidecar · oauth2-proxy deny-reason header · precomputed DAU/WAU/MAU rollups · `gb audit` query CLI.

---

## 8. Implementation bead tree (filed under ka-0000004205)

This plan files: **1 decision-gate task + 3 child epics + 10 implementation tasks**. Wiring: every task is `parent-child` to its epic; each epic is `parent-child` to ka-0000004205; the gate `blocks` all three epics; intra-phase ordering uses `blocks`. (The bead IDs are appended by the filing agent after creation.)

```text
ka-0000004205 (epic)
├── [GATE] Operator decision: approve user/security telemetry plan   ── blocks ──▶ all 3 epics
├── Epic A: User engagement instrumentation
│   ├── Add operator_id to session-close push          (§3.1)  ─ blocks ─▶ A.spend-panels
│   ├── Emit gardenio_operator_activity_total            (kbeads + Slack)
│   ├── beads-viewer view counter (slog → Prometheus)
│   └── DAU/WAU/MAU query-time panels + benchmark        (§3.5)
├── Epic B: Security-event telemetry
│   ├── Authz denial counter (derived reason, §3.3/§3.4)
│   ├── Decision lifecycle + escalation counters
│   ├── Token lifecycle counters + revocation latency histogram
│   └── Edge-auth 403 counter (conditional) + v2 contract doc
└── Epic C: Privacy enforcement + chart UX
    ├── pii-gateway rule + CI label allow-list enforcement
    └── Operator + security dashboards (server-side identity resolution)
```

---

_Research + plan only. No implementation, no provisioning, no cluster mutation performed under ka-0000003532. Methodology: parallel scope research (engagement / uniqueness / security / privacy) → synthesis → adversarial completeness+correctness critic; all load-bearing code claims re-verified against the repo before this doc was committed._
