# W3 — alertmanager-bridge (beads ↔ alerts: rules-out + alerts-in)

> Phase 0 design doc for epic [[ka-0000006776]] (workstream W3, task [[ka-0000007649]]).
> **Status: design only.** No binary is built or merged until the Phase-1 decision gate
> ([[ka-0000004494]], resolved `approve_all`) is honored in the W4 rollout ordering.

## 1. Goal

Two-way integration between beads and alerting:

- **rules-out**: alert + recording **rules authored as beads** → rendered into vmalert rule
  config (gardenio in-chart vmalert [[ka-0000006536]], and eventually site-deployment
  `tg-templates/services/vmalert`).
- **alerts-in**: **firing alerts** → beads, building directly on the existing
  `controller/internal/bridge/trigger_source_webhook.go`, which already parses
  Alertmanager / VictoriaMetrics payloads.

Modeled on `push-bridge` / `slack-bridge` (standalone binary, SSE-subscribe + external
endpoint).

## 2. Substrate to build on

### alerts-in is already half-built

`trigger_source_webhook.go` (597 lines) is a working, source-agnostic webhook → trigger
path that is **already Alertmanager/VM-aware**:

- `buildWebhookDedupKey` (line 503) tries `groupKey` first — the Alertmanager/VM group
  identity — then falls back to other id fields, then `sha256(payload)`. So **Alertmanager
  grouping is already the dedup key** (line 512 comment: `"groupKey" // Alertmanager /
  VictoriaMetrics`).
- `extractWebhookTitle` (line 530) already reads `commonAnnotations.summary`,
  `commonAnnotations.description`, `commonLabels.alertname` (lines 536-539).
- `matchWebhookConditions` (line 378) supports JSONPath conditions like
  `{"path":"status","op":"=="} firing` and `{"path":"commonLabels.severity","op":"in"}`.
- HMAC verification (line 349) + a per-trigger `webhook_token` path (`/webhooks/{token}`).
- It plugs into the shared `TriggerEngine` as a `TriggerSource` (`Name() == "webhook"`),
  with `CatchUpKeys` (line 203) for restart-safe dedup.

**Implication: alerts-in needs almost no new parsing.** What's missing is *alert-shaped
routing semantics* on top of the generic webhook: firing-vs-resolved handling, severity →
bead-type routing, silence awareness, and a curated alert trigger bead rather than a raw
generic webhook trigger.

### rules-out has a clear render target

gardenio vmalert config lives in `templates/vmalert/configmap.yaml` (recording-rules-only
today, two rules). The bridge renders bead-authored rules into the same VM rule-group YAML
shape (`groups: [{name, interval, rules: [{record|alert, expr, for, labels, annotations}]}]`).

## 3. Bead schemas

### 3a. Alert rule bead — `alert_rule` (rules-out)

| Field | Notes |
|---|---|
| `type` | `alert_rule` |
| `title` | human name = the `alert:` name in the rule group |
| `rule_kind` | `alerting` \| `recording` |
| `rule_expr` | PromQL/MetricsQL |
| `rule_for` | e.g. `5m` (alerting only) |
| `rule_severity` | `page` \| `warn` \| `info` → label + routing |
| `rule_group` | VM rule-group name (batched into one configmap) |
| `rule_target` | `gardenio` \| `site:<env>` — which vmalert renders it |
| `rule_labels` / `rule_annotations` | JSON maps merged into the rendered rule |

The bridge collects all enabled `alert_rule` beads for a target, renders one rule-group
YAML, and writes it (gardenio: ConfigMap upsert + vmalert reload; site: out-of-scope for
MVP — emit the YAML as a bundle artifact for a human to land via terragrunt, since
site-deployment config is not in this repo).

### 3b. Firing-alert bead — reuse existing trigger output (alerts-in)

A firing alert becomes a bead via the trigger engine. The new piece is the **routing
decision**, driven by an `alert_route` policy on the alert trigger bead:

| `rule_severity` / label | → bead | rationale |
|---|---|---|
| `page` / `critical` | `type=decision` (operator must act) or `escalation` | needs a human now |
| `warn` | `type=bug` or `task` | actionable, not paging |
| `info` | `type=task` (low priority) or squawk-only | informational |

Routing is config, not code: the alert trigger bead carries `action` (`create_bead` /
`spawn_agent`) + a severity→type map, evaluated by the engine that already exists.

## 4. Flow — alerts-in

```text
vmalert (fires) ──▶ Alertmanager ──webhook_config──▶ POST /webhooks/{token}
                                                          │ (alertmanager-bridge HTTP)
                                                          ▼
                              trigger_source_webhook.HandleWebhook  (REUSED)
                                  ├─ HMAC verify
                                  ├─ dedup key = groupKey            (REUSED)
                                  ├─ match conditions (status==firing, severity in ...)
                                  └─ buffer → TriggerEngine.Evaluate
                                                          │
                                       alert_route policy (NEW thin layer)
                                          ├─ page  → decision/escalation
                                          ├─ warn  → bug/task
                                          └─ info  → task/squawk
                              resolved? (status==resolved) → close/annotate the bead
```

**Composition with the existing trigger engine — avoid double-handling.** The webhook
source already exists and may already be registered by another bridge process. To avoid two
processes both turning the same alert into beads:

- The alertmanager-bridge **owns** alert-shaped triggers (label `trigger:alert`), distinct
  from generic `trigger:webhook`. `LoadTriggers` filters to its own label set.
- Dedup is shared and restart-safe via `groupKey` + `CatchUpKeys`, so even if redelivered,
  one firing alert ⇒ one bead.
- **Resolved** events (Alertmanager sends `status: resolved`) must be handled: match
  `status==resolved`, look up the bead by `groupKey` (stored as `webhook_event_key`), and
  close/annotate it rather than creating a new one. This is the one genuinely new handler.

## 5. Silences, dedup, grouping

- **Grouping**: `groupKey` is already the dedup identity — Alertmanager's own grouping is
  honored for free.
- **Dedup**: engine `seen[trigger][dedupKey]` + `CatchUpKeys` on restart (both exist).
- **Silences**: two layers. (a) Honor Alertmanager-side silences — silenced alerts simply
  aren't delivered, so nothing to do. (b) Bead-side mute: an `alert_mute` field / matching
  silence bead lets operators suppress bead-creation for a noisy alert without touching
  Alertmanager — useful when gardenio doesn't own the Alertmanager config. The bridge checks
  active mute beads before routing.

## 6. Flow — rules-out

```text
alert_rule beads (enabled) ──SSE beads.bead.updated──▶ alertmanager-bridge
     └─ collect all enabled rules for target=gardenio
     └─ render VM rule-group YAML
     └─ target=gardenio: upsert vmalert ConfigMap + trigger vmalert reload
        target=site:<env>: emit YAML as bundle artifact (human lands via terragrunt)
```

Like grafana-bridge (W2), the gardenio write path is a **ConfigMap upsert** (reuses chart
wiring, less privilege, restart-safe). Site-deployment rule-out is deliberately
**advisory** in MVP — the bridge produces the YAML, a human applies it — because
site-deployment terraform is outside this repo and outside the gardenio trust boundary.

## 7. Build plan (Phase 1, after gate ordering)

1. Register `alert_rule` bead type + the firing-alert routing fields via `EnsureConfigs`.
2. `cmd/alertmanager-bridge/main.go` — push-bridge skeleton + register the **existing**
   `WebhookTriggerSource` scoped to `trigger:alert`.
3. alerts-in: add the `resolved`→close handler and the severity→bead-type `alert_route`
   layer. Everything else (parse, dedup, HMAC) is reused.
4. rules-out: `alert_rule` collection + VM rule-group renderer + gardenio ConfigMap upsert.
5. Silence/mute beads.
6. Helm: `alertmanagerBridge.*` values + Deployment (model: slack-bridge subchart) +
   wire gardenio Alertmanager (or vmalert `--notifier.url`) webhook receiver → the bridge.

**Why this is the highest-leverage-but-not-first workstream:** alerts-in reuses an existing
parser so it's cheap, but it depends on there being an Alertmanager actually firing at
gardenio (vmalert is recording-only today, [[ka-0000006536]]) and on W1 metrics being present
to alert *on*. See W4 ordering.

## 8. Open questions for the gate

- Does gardenio deploy its **own** Alertmanager, or point gardenio vmalert's
  `--notifier.url` at the existing ACME Alertmanager? (Affects who owns silences.)
- Default routing map: is `page → decision` (operator queue) right, or `page → escalation`?
- rules-out to site-deployment: advisory-YAML-only (recommended MVP) vs. eventually
  bridge-driven terragrunt PRs?
- Reuse `trigger:webhook` source directly vs. fork a dedicated alert source — recommend
  reuse with a distinct label.

## 9. Related

- Epic [[ka-0000006776]] · gate [[ka-0000004494]]
- Existing parser: `controller/internal/bridge/trigger_source_webhook.go`
- gardenio vmalert epic [[ka-0000006536]] (recording-only today)
- Sibling: W1 `obs-w1-victoria-to-grafana.md`, W2 `obs-w2-grafana-bridge.md`,
  W4 `obs-synthesis-rollout.md`
