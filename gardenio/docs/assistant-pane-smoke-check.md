# Slack agent-pane (agent_view DM) smoke-check

This is the operator runbook for keeping the gardenio **agent surface** — the
DM-style "Agent messaging experience" (`agent_view`) — delivering typed
messages. It exists because of a real, silent failure mode (`ka-0000000892`):
after the app is enabled/reinstalled, Slack can keep delivering the surface's
*lifecycle* events while dropping the *message* event — so the pane sets up fine
(suggested prompts post) but every typed message bounces with **"Slack couldn't
send this message."** There is no error log to grep for, only an absence.

> **agent_view migration (`ka-0000004985`).** The surface migrated from the
> deprecated `assistant_view` (AI container thread) to `agent_view` (a DM that
> "looks & feels the same as a regular direct message"). The lifecycle events
> changed accordingly: `assistant_thread_started` → **`app_home_opened`**
> (`tab == "messages"`), `assistant_thread_context_changed` →
> **`app_context_changed`**. The user's typed text still arrives via
> **`message.im`** — but under `agent_view` it is a plain top-level DM (no
> `assistant_app_thread` root), so the bridge records the `message` counter at
> the `message.im` choke point in `handleMessageEvent`, not in the legacy
> assistant-thread handler. The **metric label values are unchanged**
> (`thread_started` / `context_changed` / `message`) so the dashboard + alert
> below keep working across the migration. slack-go v0.18.0 has no typed struct
> for `app_context_changed` and its `AppHomeOpenedEvent` lacks the embedded
> `context`, so the bridge self-registers superset types in
> `agent_view_events.go` (no library bump needed). The `assistant_view` →
> `agent_view` switch is **one-way** — Slack cannot revert it.

**Root cause of the historical outage (fixed in source).** The committed
`slack/manifest.json` enabled the App Home Messages tab
(`messages_tab_enabled: true`) but never declared
`messages_tab_read_only_enabled`. For this surface an omitted value defaults to
**read-only**, which makes Slack bounce typed input client-side and never emit
`message.im` — exactly the silent symptom above. The manifest now pins
`messages_tab_read_only_enabled: false`, and a parity guard
(`TestManifestAgentMessagesTabWritable`) fails the build if it regresses. The
remaining operator step is to **re-apply the corrected manifest and reinstall**
so the live app matches source.

## The metric

`slack-bridge` exports `gardenio_assistant_event_total` on its `/metrics` endpoint
(`gardenio/controller/internal/bridge/assistant_event_metrics.go`). One bounded
`event` label (values held stable across the agent_view migration for dashboard
continuity):

| `event`           | Fires when                                                                   |
|-------------------|------------------------------------------------------------------------------|
| `thread_started`  | user opens the gardenio agent DM (`app_home_opened`, `tab=messages`)          |
| `context_changed` | user's active context changes while the app is open (`app_context_changed`)  |
| `message`         | a user DM was delivered (`message.im` handled in `handleMessageEvent`)       |

The whole point: `thread_started` / `context_changed` and `message` are delivered
by Slack independently. The healthy invariant is *if people are opening the pane,
some of them are also typing in it.* When `message` flatlines while
`thread_started` keeps climbing, inbound delivery is broken.

## The smoke signal (PromQL)

Panes opened but zero messages handled over the lookback window, while opens are
non-trivial:

```promql
# Messages handled in the pane over the last 24h
sum(increase(gardenio_assistant_event_total{event="message"}[24h]))

# Panes opened over the last 24h
sum(increase(gardenio_assistant_event_total{event="thread_started"}[24h]))
```

Alert when opens are happening but messages are not:

```promql
sum(increase(gardenio_assistant_event_total{event="thread_started"}[24h])) >= 1
and
sum(increase(gardenio_assistant_event_total{event="message"}[24h])) == 0
```

Ride out a short `for:` hold (~2h) before alerting — a single scrape-quiet
window is not the bug; a window of opens with zero typed messages handled is.

**Sizing the window and floor to your traffic (ka-0000000664).** Pane opens can
be rare: prod observed **~0.3 `thread_started`/day** (9 over 30d) against
**~26 `message`/day**. At that rate a 1h window with `>= 3` opens is effectively
*unfireable* — it matched zero samples over 30 days. The window must be wide
enough that real opens accrue at your traffic rate, and the opens floor low
enough that a single genuine open clears it. The chart default is therefore a
**24h window with `minOpens: 1`**. The false-positive guard is not the opens
floor but the `message == 0 over 24h` conjunct: with ~26 handled messages on a
normal day, a benign "peek at the pane and leave" cannot zero it out. Raising
`minOpens` buys little extra FP protection at this traffic while pushing
detection latency out to 1–2 weeks, so keep it low unless your workspace sees
many more opens per day.

> **Re-derive this baseline post-`agent_view` (ka-0000004985).** The
> **~0.3 opens/day** figure was collected under the old `assistant_view`, where
> `assistant_thread_started` fired once per *new* thread. Under `agent_view` the
> `thread_started` label is sourced from `app_home_opened(tab=messages)`, which
> Slack fires on **every open** of the DM/Messages tab — clicking into the DM,
> reloading Slack — so the observed open rate will be **materially higher and
> noisier**, measuring opens rather than thread starts. This does not change the
> alert's direction (opens climbing while `message == 0` still means broken
> inbound) or its real false-positive guard (`message == 0 over 24h`), so the
> `minOpens: 1` / `24h` default remains safe. But re-observe the actual
> post-migration open rate before trusting the "1h + `>= 3` is unfireable"
> sizing guidance above — that arithmetic no longer describes reality.

### Shipped as a chart-managed alert

This PromQL is no longer hand-rolled per env — the chart ships it as the
`AssistantPaneSilent` alert
(`gardenio/helm/gardenio/templates/slack-bridge/assistant-pane-alert.yaml`). It is
**off by default**; per-env overlays opt in:

```yaml
slackBridge:
  metrics:
    # Scrape the bridge :9093 /metrics so the alert has data. Enable whichever
    # your cluster's metrics operator supports — both can be on at once.
    serviceMonitor:
      enabled: true            # prometheus-operator (ServiceMonitor)
    prometheusRule:
      assistantPane:
        enabled: true
        severity: page         # page in prod, ticket in rwx, none in dev
        minOpens: 1            # opens floor over the window (1 at ~0.3 opens/day)
        window: "24h"          # lookback for both opens and messages
        for: "2h"              # sustained-hold before firing
        # Discovery label for the cluster's PrometheusRule ruleSelector:
        # prometheusRuleSelectorLabels: { release: kube-prometheus-stack }
```

vm-operator clusters (no ServiceMonitor CRD, e.g. `poc-gardenio-eks`) get the
parallel `VMServiceScrape` by setting `monitoring.victoriaMetrics.enabled=true`
instead — see `slack-bridge/vmservicescrape.yaml`. Until these toggles are on,
the bridge exports `gardenio_assistant_event_total` into the void: nothing
scrapes `:9093` and the alert has no data to evaluate.

### Quick manual check (no Prometheus)

```sh
# From inside the cluster / a port-forward to a slack-bridge pod.
# The metrics server listens on :9093 by default (resolveMetricsAddr,
# overridable via the metrics-addr env):
kubectl -n gardenio port-forward deploy/gardenio-slack-bridge 9093:9093 &
curl -s localhost:9093/metrics | grep gardenio_assistant_event_total
```

If `event="thread_started"` is rising across restarts but `event="message"` is
stuck at 0, you are in the `ka-0000000892` failure. Go to the recovery checklist.

## Operator recovery checklist — re-activate `message.im` delivery

The fix is operator-only (it touches live Slack app config at
<https://api.slack.com/apps>; the in-cluster bot/app tokens can't edit app config).

1. **Apply the committed manifest.** App config → **App Manifest** → paste
   `gardenio/slack/manifest.json` and save. It declares `features.agent_view`,
   the writable Messages tab, and the `app_home_opened` / `app_context_changed`
   / `message.im` bot events. **Note the `assistant_view` → `agent_view` switch
   is one-way** — once applied you cannot revert to `assistant_view`.
2. **Event Subscriptions → Subscribe to bot events.** Confirm `message.im`,
   `app_home_opened`, and `app_context_changed` are all listed (and not
   greyed/pending). This verifies the *live* app matches source.
3. **Reinstall the app to the workspace.** Settings → Install App → Reinstall.
   This is the step that actually (re)activates the events; just editing the
   manifest without a reinstall is what left the surface silent in
   `ka-0000000892`.
4. **Hard-refresh Slack** (Slack requires this after an `agent_view` change), then
   **verify**: open the gardenio DM, type any message. You should get a reply.
   Confirm the metric moved: `gardenio_assistant_event_total{event="message"}`
   incremented, and `{event="thread_started"}` incremented when you opened the DM.

If after a clean reinstall `message` still flatlines, investigate the surface
out-of-band — see the decision history on `ka-0000006952` / `ka-0000000892` and
the migration bead `ka-0000004985`. (The old "disable `assistant_view` to revert
to the plain DM path" unblock no longer applies — the `agent_view` switch is
one-way, and the plain DM `message.im` path is now the *primary* delivery path.)

## Warm-vs-cold delivery path + cold-respawn latency

A separate concern on the same surface: how *fast* a thread/DM agent answers a
follow-up. When a mention lands for a thread-bound agent the bridge forks one of
two ways (`bot_mentions.go`):

- **WARM** — the agent bead is still active, so the bridge delivers a live nudge
  via `nudgeAgentForMention`. The pod is already up; the reply is near-instant.
- **COLD** — the agent bead is gone (despawned/completed), so `respawnThreadAgent`
  re-creates it with `--resume` and the pod boots from scratch before the first
  token. This is the latency the **warm-idle keepalive** (`ka-0000005793`,
  epic `ka-0000006987`) exists to eliminate by keeping chat-window pods alive
  through the idle window.

Two metrics make this observable (`assistant_event_metrics.go`,
`agentstream.go`):

| Metric | Type | What it measures |
|---|---|---|
| `gardenio_mention_delivery_total{delivery_path="warm\|cold"}` | counter | One increment per mention delivery, by path. `delivery_path` is a HARD enum {`warm`,`cold`}. |
| `gardenio_mention_cold_respawn_seconds` | histogram | Cold-respawn boot latency: follow-up arrival (`respawnThreadAgent`) → first proof-of-life (`SessionStart` hook). Buckets 1s…180s. |

The histogram is armed at the top of `respawnThreadAgent` (`MarkColdRespawn`) and
closed in `HandleSessionStart` when the respawned agent's `SessionStart` hook
fires — the same "Claude booted in the container" signal that flips the stream
session `live`. Warm nudges never open a new session, so they never observe here.

### Warm-hit ratio (the KPI)

This is the proof artifact for the lower-latency goal — the fraction of mention
follow-ups that hit a still-warm pod instead of paying a cold respawn:

```promql
# Warm-hit ratio over the last 24h (1.0 = every follow-up hit a warm pod)
sum(increase(gardenio_mention_delivery_total{delivery_path="warm"}[24h]))
/
sum(increase(gardenio_mention_delivery_total[24h]))
```

Track it before/after enabling the warm-idle keepalive — the ratio should climb
toward 1.0 as cold respawns are converted to warm nudges. Watch the raw split too:

```promql
# Cold respawns per hour — should fall once keepalive is on
sum(rate(gardenio_mention_delivery_total{delivery_path="cold"}[1h])) * 3600

# Warm vs cold deliveries per hour, side by side
sum by (delivery_path) (rate(gardenio_mention_delivery_total[1h])) * 3600
```

### Cold-respawn latency (what warm removes)

```promql
# p50 / p95 cold-respawn boot latency over the last 6h
histogram_quantile(0.50, sum(rate(gardenio_mention_cold_respawn_seconds_bucket[6h])) by (le))
histogram_quantile(0.95, sum(rate(gardenio_mention_cold_respawn_seconds_bucket[6h])) by (le))

# Mean cold-respawn latency over the last 6h
sum(rate(gardenio_mention_cold_respawn_seconds_sum[6h]))
/
sum(rate(gardenio_mention_cold_respawn_seconds_count[6h]))
```

The p95 here is roughly the worst-case follow-up wait a user feels on a cold hit;
multiply by the cold-hit count to get aggregate latency the keepalive saves.

### Quick manual check (no Prometheus)

```sh
kubectl -n gardenio port-forward deploy/gardenio-slack-bridge 9093:9093 &
curl -s localhost:9093/metrics | grep -E 'gardenio_mention_delivery_total|gardenio_mention_cold_respawn_seconds'
```
