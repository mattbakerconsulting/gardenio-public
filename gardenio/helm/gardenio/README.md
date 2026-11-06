# gardenio Helm chart

Helm chart for the gardenio platform — controller, coopmux, slack-bridge,
postgres, NATS, and the supporting viewers. See `values.yaml` for the full
list of values; this README covers things an operator needs out-of-band.

## Embedding backend (semantic search)

Setting `beads.embedding.enabled=true` wires the beads container to a remote
OpenAI-compatible embedding endpoint. No sidecar is rendered; embedding is a
single HTTP call out of the beads pod. The chart hardcodes
`BEADS_EMBED_BACKEND=openai` and sets:

- `BEADS_EMBED_URL` — defaults to `https://litellm.app.devops.acme.example`, our
  LiteLLM proxy re-exporting AWS Bedrock `amazon.titan-embed-text-v2:0`
  (1024-dim) under IRSA.
- `BEADS_EMBED_MODEL` — defaults to `bedrock-titan-embed-v2`.
- `BEADS_EMBED_API_KEY` — sourced via `secretKeyRef` from the K8s Secret
  named in `beads.embedding.apiKeySecret.name` (production:
  `litellm-master-key`), key defaults to `api-key`.

**Prerequisite: pgvector extension.** The beads database must have the
`vector` extension installed — migration `005_bead_embeddings` calls
`CREATE EXTENSION IF NOT EXISTS vector` and will leave the migration table
in a dirty state if the extension is unavailable.

The in-chart Postgres (`postgres.enabled=true`) defaults to
`pgvector/pgvector:pg17`, which ships pgvector preinstalled. If you
override `postgres.image.repository` / `postgres.image.tag`, make sure
the replacement ships pgvector — vanilla upstream `postgres:17-alpine`
(and most non-Bitnami community images) do NOT, and using them causes
kbeads to CrashLoopBackOff on startup.

For external Postgres instances, a DB superuser must run
`CREATE EXTENSION vector;` in the beads database, or grant the beads DB
user enough privileges to run it themselves (typically `rds_superuser` on
RDS or the `SUPERUSER` attribute elsewhere).

## Slack bridge — beads-viewer link target

`slackBridge.beadsViewerURL` is the public base URL of the read-only
beads-viewer UI. When set, the Slack bridge rewrites bead references in
outgoing messages into clickable Slack `mrkdwn` hyperlinks of the form
`<https://{beadsViewerURL}/{ka-id}|{label}>`, deep-linking each
`ka-XXXX` reference at `/{ka-id}`.

The link is rendered by the `beadLink` / `beadTitleLink` helpers in
`gardenio/controller/internal/bridge/bot.go`. Behavior:

| `beadsViewerURL`                        | Rendered Slack message                                         |
| --------------------------------------- | -------------------------------------------------------------- |
| `""` (unset)                            | Plain text label (e.g. `ka-abc My bead`) — no hyperlink.       |
| `"https://viewer.example.com"`          | `<https://viewer.example.com/ka-abc|My bead>` (clickable link). |
| `"https://viewer.example.com/"`         | Same as above; trailing slash is trimmed.                      |

A bead id that does not match the `kd-[A-Za-z0-9]+` whitelist falls
back to plain text — untrusted input cannot forge URL paths.

**Format**: an absolute `https://` URL with no trailing slash required,
pointing at the public beads-viewer ingress including its path prefix
(e.g. `https://gardenio.e2e.example.com/beads`). The bridge
appends `/{ka-id}` per link.

**Default**: empty string. When empty *and* `beadsViewer.ingress.enabled`
+ `beadsViewer.ingress.host` are set, the slack-bridge deployment
template computes
`https://{beadsViewer.ingress.host}{beadsViewer.ingress.pathPrefix}`
automatically and injects it as `BEADS_VIEWER_URL`. Set
`slackBridge.beadsViewerURL` explicitly to override (e.g. when the
viewer is hosted behind a different public hostname than the in-chart
ingress).

**Distinct from `BEADS_WEB_URL`** (derived from the unified host's
`/beads` path, served by beads-viewer), used for escalation deep links.

```yaml
slackBridge:
  beadsViewerURL: "https://beads-viewer.example.com"
```

## Coopmux — account assignment strategy

The coopmux credential broker assigns accounts to new agent sessions. Two
selection strategies are implemented in
`coop/crates/mux/src/credential/broker.rs`:

| Value          | Behavior                                                                                                                        |
| -------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| `least_loaded` | Pick the Healthy account with the fewest current sessions. Default. Can snap-to-zero pile up after drains and skew lifetime use. |
| `round_robin`  | Barrel rotator across the sorted Healthy eligible set; shared cursor advances on each assignment. Even cumulative utilization.  |

Set per install via:

```yaml
coopmux:
  assignmentStrategy: "round_robin"   # or "least_loaded", or "" (use coopmux default)
```

The value is wired to the `COOPMUX_ASSIGNMENT_STRATEGY` env var on the
coopmux pod. Unset / empty leaves the env var off and coopmux applies its
built-in default (`least_loaded`) — so an upgrade is a no-op.

### Rollout plan

The flag exists so we can ship the rotator dark and enable it per-cluster
without a rebuild. Stages:

1. **Ship default = `least_loaded`.** No behavior change on upgrade. This
   is the released state.
2. **Flip one cluster to `round_robin`.** The gate is evidence, not a clock
   (no bake-in, owner rule 2026-11-02): read these once the pool has assigned
   new sessions under real traffic, and revert the value if one regresses:
   - `GET /pool` — per-account `session_count` and (once
     ka-0000001196 lands) `cumulative_assignments` should converge
     toward even across Healthy accounts under steady traffic.
   - Per-account API token usage from the provider dashboard — max/min
     ratio should tighten.
   - Agent failure rate and Warming transitions — should be unchanged.
     The rotator never picks Warming/Throttled accounts, so health
     behavior is identical to `least_loaded` for that dimension.
3. **Flip prod** as soon as those checks are clean: set
   `coopmux.assignmentStrategy: "round_robin"` in the prod values and
   cut a release. The flag is what lets you revert without redeploying
   coopmux if fairness regresses.
4. **Remove the flag** (future cleanup) once no environment's values set
   `least_loaded`: then the flag and the `least_loaded` branch can be
   deleted. File a follow-up bead when stage 3 is green.

The flag is a deploy-time setting; flipping it requires a rolling
restart of the coopmux deployment (done automatically when the Helm
release upgrades).

### Operator runbook

**Flip the flag (per cluster)**

```bash
# Edit the cluster's values overlay (prod uses acme-helm-chart wrapper,
# staging uses values-fleet.yaml or a local override) to set:
#   coopmux:
#     assignmentStrategy: "round_robin"
#
# Then cut a chart release or apply via the deployment's usual path.
# Coopmux restarts on upgrade; the cursor is in-memory so it resets to 0.
```

To verify the flag is live on a running pod:

```bash
kubectl -n <ns> get pod -l app.kubernetes.io/name=coopmux \
  -o jsonpath='{.items[0].spec.containers[0].env}' | jq '.[] | select(.name=="COOPMUX_ASSIGNMENT_STRATEGY")'
```

Empty / missing env var means the default (`least_loaded`) is active.

**Spot-check fairness via `/pool`**

```bash
kubectl -n <ns> port-forward svc/<coopmux-svc> 9800:9800 &
curl -sH "Authorization: Bearer $COOPMUX_TOKEN" http://localhost:9800/pool | jq
```

Under `round_robin` with steady traffic, `session_count` per Healthy
account should stay within ±1 across equivalent-capacity accounts over
a few minutes. Large persistent skew (one account 3–4× its peers) is a
signal the rotator is not effective for this workload — capture a
`/pool` snapshot and escalate to the epic (ka-0000000790) before
reverting. Short-term skew right after a pool change (account added /
removed / transitioned out of Warming) is expected and resolves within
one rotation cycle.

Once the metrics follow-up (ka-0000001196) lands, `/pool` will also
include `cumulative_assignments` per account and the live `rotator_cursor`
— prefer those over `session_count` for fairness judgment, since
`session_count` is still a point-in-time view.

**Revert**

If the rotator needs to be turned off:

```yaml
coopmux:
  assignmentStrategy: "least_loaded"   # or "" to fall back to the coopmux default
```

Roll the chart. Coopmux restarts; all new sessions use the old
selector. Existing sessions are not reassigned — they keep the account
they were placed on, which is the same behavior as any broker restart.

Revert when: per-account error/throttle rates climb after the flip
without a matching load spike, fairness is clearly worse than
`least_loaded` on `/pool`, or any new regression correlates with the
flip. File a bug against epic ka-0000000790 with the `/pool` snapshot,
the time of the flip, and a sample of agent failures.

### Related work

- Epic: ka-0000000790 — account barrel rotator.
- Implementation: ka-0000005270 (landed, MR !1056).
- Fairness + regression tests: ka-0000002129 (landed).
- `/pool` metrics + coopmux dashboard surfacing: ka-0000001196.
- Design doc: `coop/docs/account-barrel-rotator.md` (MR !1049).

## ExternalSecret refreshInterval policy

The chart groups its `ExternalSecret` resources into three rotation-cadence
buckets (`rotated`, `static`, `oauth`) exposed under
`externalSecrets.refreshIntervals`. Templates pick a bucket via the
`gardenio.externalSecrets.refreshInterval` helper rather than hardcoding a
duration. See [`gardenio/docs/secrets-refresh.md`](../../docs/secrets-refresh.md)
for the per-secret mapping, defaults, and tuning guidance.

## Metrics scrape-target CRDs (ServiceMonitor vs VMServiceScrape)

By default the chart only emits `monitoring.coreos.com/v1 ServiceMonitor`
resources (gated by the per-component `serviceMonitor.enabled` toggles, e.g.
`agents.metrics.serviceMonitor.enabled`, `keycloak.runtime.serviceMonitor.enabled`).
This works on clusters running kube-prometheus-stack or the upstream
prometheus-operator.

Clusters that ship only the **VictoriaMetrics operator**
(`operator.victoriametrics.com`) without prometheus-operator do not have the
ServiceMonitor CRD installed — gardenio-system pods then go un-scraped by
default. To target those clusters, enable the VMServiceScrape emitter:

```yaml
monitoring:
  victoriaMetrics:
    enabled: true        # default false
    interval: 30s        # default; per-component intervals still win when set
```

When enabled, the chart emits `operator.victoriametrics.com/v1beta1
VMServiceScrape` resources for:

- `gardenio-beads` (port `metrics` → 9091)
- `gardenio-agents` (controller, port `metrics` → 9092)
- `gardenio-nats` (port `monitoring` → 8222)
- `gardenio-keycloak-runtime` (port `management`, when keycloak runtime is enabled)

The VMServiceScrapes are emitted **in parallel** with the ServiceMonitors —
both toggles can be on simultaneously without conflict, which is useful for
clusters where both operators coexist or during a migration window.

**Canonical use case:** the customer-poc-aws POC cluster `poc-gardenio-eks`
installs only vm-operator. Setting `monitoring.victoriaMetrics.enabled=true`
in that env's deploy-values is the long-term replacement for the temporary
`gardenio-beads-poc-temp` VMServiceScrape that was hand-applied during the
initial bring-up. See bead ka-0000005115.
