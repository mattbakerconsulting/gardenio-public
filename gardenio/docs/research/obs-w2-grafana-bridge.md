# W2 — grafana-bridge (beads ↔ Grafana dashboards)

> Phase 0 design doc for epic [[ka-0000006776]] (workstream W2, task [[ka-0000006709]]).
> **Status: design only.** No binary is built or merged until the Phase-1 decision gate
> ([[ka-0000004494]], resolved `approve_all`) is honored in the W4 rollout ordering.

## 1. Goal

"Dashboards as beads." Author and version Grafana dashboards **in beads**, sync them to
Grafana, and reflect Grafana-side edits back into beads — so dashboard changes are
reviewable, attributable (`operated_by`), and live in the same graph as the work that
motivated them.

Modeled on the existing standalone bridge binaries (`gitlab-bridge`, `jira-bridge`,
`push-bridge`, `wl-bridge`), each of which: SSE-subscribes to bead events, polls/calls an
external system, and writes both ways.

## 2. Substrate to build on

- **Bridge skeleton** (`gardenio/controller/cmd/<x>-bridge/main.go` + `internal/bridge/`):
  every bridge imports `internal/beadsapi` (daemon HTTP client) and `internal/bridge`
  (`SSEStream`, `StateManager`, `Dedup`, `EnsureConfigs`, `TriggerEngine`). A new bridge =
  a new `cmd/grafana-bridge/main.go` that wires those plus a Grafana client. (See W1/W3
  docs and the Explore map; the shared `NewSSEStream` + `RegisterHandlers` + `go
  sseStream.Start(ctx)` startup pattern is identical across all five existing bridges.)
- **The write path already exists**: dashboards ship today as ConfigMaps labeled
  `grafana_dashboard="1"` (`templates/controller/grafana-dashboard-configmap.yaml`), JSON
  embedded from `gardenio/helm/gardenio/dashboards/{cost-overview,kbeads-embedding,operated-by}.json`,
  loaded by the Grafana sidecar (`grafana.sidecar`). This is the seed of "beads →
  dashboard."
- **Grafana API**: Grafana 11.4 behind AVA header-auth at `/grafana`. Two write surfaces:
  (a) the **HTTP dashboard API** (`POST /api/dashboards/db`), (b) **file/ConfigMap
  provisioning** via the sidecar. Read-back uses `GET /api/dashboards/uid/<uid>` and
  `GET /api/search`.

## 3. Bead schema — a dashboard as a bead

New bead type `dashboard` (registered via `EnsureConfigs` on bridge startup, same way
bridges register their types today):

| Field | Source | Notes |
|---|---|---|
| `type` | `dashboard` | new custom type |
| `title` | dashboard title | human label |
| `dashboard_uid` | Grafana `uid` | stable cross-rename identity; the join key |
| `dashboard_folder` | Grafana folder | provisioning target |
| `dashboard_version` | Grafana `version` int | optimistic-concurrency token |
| `dashboard_hash` | sha256 of normalized JSON model | cheap change-detection / loop-breaker |
| `dashboard_source` | `beads` \| `grafana` | who last authored — provenance |
| JSON model | **bundle doc**, not a field | see §4 |

**JSON model storage — bundle doc, not a bead field.** Dashboard JSON models are large
(cost-overview.json is ~21KB) and edited as whole documents. Per global advice
[[ka-0000004517]] (long-form artifacts belong in a bundle on the originating bead), store
the model as a `type=doc` bundle artifact attached to the dashboard bead, and keep only the
small metadata above as queryable fields. This keeps bead rows light and makes diffs a
bundle-doc revision rather than a giant field churn.

## 4. Sync directions

### 4a. beads → Grafana (author in beads)

```text
dashboard bead created/updated (bundle JSON changes)
  └─ SSE "beads.bead.updated" → grafana-bridge handler
       ├─ fetch JSON model from bundle
       ├─ compute hash; if == dashboard_hash field, skip (loop-breaker)
       └─ write path (configurable, default = provisioning):
            (P) render/update the grafana_dashboard="1" ConfigMap  → sidecar applies
            (A) OR POST /api/dashboards/db (uid, overwrite, message=bead id)
       └─ on success: stamp dashboard_version, dashboard_hash, dashboard_source=beads
```

Default write path = **ConfigMap/provisioning** (path P): it reuses the shipped sidecar,
is GitOps-friendly, and survives Grafana pod restarts. The **HTTP API** (path A) is offered
for fast iteration / folders the sidecar doesn't manage. A values flag picks the default
(`agents.metrics.grafanaDashboard` already gates the ConfigMap).

### 4b. Grafana → beads (reflect edits back)

```text
poll GET /api/search (every grafana_poll_interval)
  └─ for each dashboard: GET /api/dashboards/uid/<uid>
       ├─ compute hash of normalized model
       ├─ if hash == dashboard_hash on the matching bead → no-op
       └─ else (Grafana-side edit):
            ├─ update bundle JSON doc (new revision)
            └─ stamp dashboard_version, dashboard_hash, dashboard_source=grafana
```

No Grafana webhook for dashboard saves exists in OSS Grafana, so read-back is **poll-based**
(like jira-bridge's JQL poll), keyed on `uid`. `dashboard_hash` makes the poll cheap and
idempotent.

## 5. Versioning & conflict resolution

The loop-breaker is `dashboard_hash` + `dashboard_source`:

- A write in either direction stamps the new hash, so the *other* direction sees "hash
  already matches" and does nothing — no ping-pong.
- **Conflict** = bead JSON and Grafana JSON both changed since last sync (both hashes
  differ from the stored one). Policy (config flag `grafana_conflict_policy`):
  - `beads-wins` (default): bead is source of truth; overwrite Grafana, log the clobbered
    Grafana version to the bead's bundle as a `conflict-<version>.json` artifact for audit.
  - `grafana-wins`: pull Grafana into the bead, file the dropped bead edit as a bundle
    artifact.
  - `manual`: open a `type=decision` so the operator picks — only for dashboards flagged
    `dashboard_conflict_critical`.
- Grafana's own `version` int is carried as `dashboard_version` and sent as the
  optimistic-concurrency check on `POST /api/dashboards/db` (reject if stale → re-read →
  re-resolve).

## 6. Sequence (happy path, author-in-beads)

```text
operator/agent edits dashboard bundle on bead ka-XXXX
        │
        ▼  beads.bead.updated (SSE)
grafana-bridge ──hash changed?──▶ yes
        │
        ├─ upsert ConfigMap grafana_dashboard="1" (model JSON)
        │        │
        │        ▼ k8s-sidecar WATCH
        │   Grafana provisions dashboard uid=...
        │
        └─ stamp bead: version+1, hash, source=beads   (next Grafana poll is a no-op)
```

## 7. Auth

grafana-bridge → Grafana behind AVA header-auth. Either (a) a Grafana **service-account
token** (`Authorization: Bearer`) for the HTTP API, injected via k8s secret; or (b) for the
ConfigMap path, **no Grafana auth at all** — the bridge only touches the k8s API
(ConfigMaps), and the sidecar does the Grafana write in-pod. Path (b) is strictly less
privileged and is another reason to default to provisioning.

## 8. Build plan (Phase 1, after gate ordering)

1. ✅ Register `dashboard` bead type + fields via `EnsureConfigs` (W2-impl-a, ka-0000000055).
2. ✅ `cmd/grafana-bridge/main.go` — SSE + state skeleton over the shared `internal/bridge` lib.
3. ✅ Implement write path P (ConfigMap upsert) first — reuses existing sidecar label.
4. ✅ Implement read-back poll + hash; wire `dashboard_source`/`dashboard_hash` loop-breaker.
   *(The loop-breaker write side — hash compute + `dashboard_source=beads` stamp — landed in
   W2-impl-a; the Grafana→beads read-back poll landed in W2-impl-b, ka-0000001236.)*
5. ✅ Seed: import the three shipped `dashboards/*.json` as dashboard beads (one-time
   backfill via `GRAFANA_BACKFILL=1` + `GRAFANA_DASHBOARDS_DIR`) so existing dashboards
   become editable-as-beads.
6. ✅ Conflict policies behind a flag (`GRAFANA_CONFLICT_POLICY` = `grafana-wins` (default) /
   `beads-wins` / `manual`). *(W2-impl-b)* Write path A (HTTP API write-back) deferred — the
   read direction uses the Grafana HTTP API; the write direction stays on the ConfigMap
   provisioning path (§7 path b, least-privileged).
7. Helm: `grafanaBridge.*` values + Deployment, modeled on the slack-bridge subchart.
   *(breakdown #6 in obs-synthesis-rollout.md; mount the chart dashboards ConfigMap at
   `GRAFANA_DASHBOARDS_DIR` so the bridge can backfill on first boot.)*

> **W2-impl-a status (ka-0000000055):** type registration, `cmd/grafana-bridge`
> skeleton, ConfigMap write path, beads→Grafana sync with hash loop-breaker, and the
> 3-dashboard backfill all landed. The JSON model is stored as a `type=doc` bundle child
> of the dashboard bead (label `grafana:dashboard-model`) per §3/§4. Per-dashboard
> ConfigMaps are named `grafana-dashboard-<uid>` and labeled
> `grafana_dashboard=1` + `app.kubernetes.io/managed-by=grafana-bridge` so the bridge owns
> them independently of the chart-shipped bundle ConfigMap.

> **W2-impl-b status (ka-0000001236):** the Grafana→beads read-back poll landed. A
> `GrafanaClient` wraps the two read endpoints (`GET /api/search`, `GET
> /api/dashboards/uid/<uid>`); `GrafanaReadback` polls every `GRAFANA_READBACK_INTERVAL`
> (default 60s), normalizes+hashes each Grafana model, and reconciles against the matching
> dashboard bead. The `dashboard_hash` loop-breaker skips models already in sync; a plain
> Grafana edit is pulled into the bead's model doc with `dashboard_source=grafana`. When
> BOTH sides diverged it applies `GRAFANA_CONFLICT_POLICY`: `grafana-wins` (default — pull
> Grafana, archive the dropped beads model), `beads-wins` (keep beads, archive the Grafana
> model, sync re-pushes), or `manual` (open a `type=decision` bead, touch neither side).
> Dropped models are archived as `grafana:dashboard-conflict` docs under the bead. The
> poll is enabled only when `GRAFANA_URL` is set; otherwise the bridge runs write-only.

## 8b. `gb grafana render` — the agent author→render→look→edit loop (W2d)

Authoring a dashboard as a bead is only half the loop; to *iterate*, an agent must render
the current state and SEE it. `gb grafana render` closes that loop without a human eyeballing
Grafana (task [[ka-0000006672]]):

```
gb grafana render <dashboard-bead-id | grafana-uid> [--panel N] [--from --to] \
                  [--var k=v ...] [--width --height --theme --tz] [--out PATH] \
                  [--upload [--bead kd-...]]
```

- **Resolve** — a bead id is dereferenced to its `dashboard_uid` field (errors if the bead
  isn't a `dashboard` or hasn't synced a uid yet); a raw uid passes through (`--uid` forces it).
- **Render** — GETs Grafana's image-renderer: `/render/d/<uid>` (whole dashboard) or
  `/render/d-solo/<uid>?panelId=N` (one panel). Base URL is `--grafana-url`, else
  `GRAFANA_URL` (in-cluster Service), else `GRAFANA_BASE_URL` (public AVA base the viewer
  embeds). Auth is `GRAFANA_TOKEN` (Bearer) when set, else the persisted `gb login` AVA cookie.
- **Look** — the PNG is written to `--out` (default: a stable temp path, no timestamp, so
  re-renders overwrite in place) so the agent can `Read` it (Claude decodes PNG natively).
- **Optionally attach** — `--upload` pushes the PNG to the viewer S3 bucket via the same
  path as `gb image upload` (shared `putImageInput`); `--bead` (default: the rendered
  dashboard bead) scopes/attaches it.

The image-renderer sidecar is feature-flagged in the chart (cross-cutting [[ka-0000005807]]);
when it is not deployed `/render` 404s and the command says so plainly (pointing at the chart
flag) rather than writing a broken file. Code: `controller/cmd/gb/grafana.go`.

## 9. Open questions for the gate

- Default write path: ConfigMap-provisioning (recommended) vs HTTP API — confirm.
- Do we want folder-level sync (whole Grafana folders) or per-dashboard only? (Per-dashboard
  MVP.)
- Backfill: import all existing dashboards as beads, or only newly-authored ones?
- Conflict default `beads-wins` acceptable, or start `manual` until trust is established?

## 10. Related

- Epic [[ka-0000006776]] · gate [[ka-0000004494]]
- Long-form-artifact-in-bundle advice [[ka-0000004517]]
- gardenio Grafana epic [[ka-0000007895]]
- Sibling: W1 `obs-w1-victoria-to-grafana.md`, W3 `obs-w3-alertmanager-bridge.md`,
  W4 `obs-synthesis-rollout.md`
