# R2 — Helm template-var plumbing that feeds link formation (all envs)

**Task:** ka-0000002793 · **Epic:** ka-0000006413 (URL-unification research/audit)
**Scope:** RESEARCH ONLY. Maps every Helm knob → link-forming env var → consuming
Go code, documents the `unifiedHost` contract + MUST-set invariants, and tabulates
the current per-env state across the gardenio chart and the SEPARATE `acme-helm-chart`
deploy-values + the customer-poc terraform PoC. **No prod flips.**

Companion to R1 (ka-0000003583 — code-side link-formation sites). R2 = the
chart/values plumbing that *feeds* those code sites their base URLs.

Line numbers are as of commit `e2b8c00fe` (2026-06-05). Chart paths are relative to
`gardenio/helm/gardenio/` in the monorepo unless noted. Deploy-values paths are in the
separate `acme-helm-chart` repo (`charts/gardenio/values/`).

---

## 1. The `unifiedHost` contract (chart `values.yaml` ~L219)

A single AVA-gated host that fans out by path prefix. Today it is **off by default**
(`unifiedHost.enabled: false`) and **no environment overrides it** (see §4).

```yaml
unifiedHost:
  enabled: false
  host: ""                 # e.g. gardenio.e2e.example.com
  paths:
    beads:   "/beads"      # → beads-viewer Service (HTMX UI + mezmo widgets)
    kd:      "/kd"         # → kbeads HTTP API (root-routed, path-agnostic)
    mux:     "/mux"        # → coopmux dashboard + WebSocket
    nats:    "/nats"       # → kbeads NATS WebSocket
    jira:    "/jira"       # → jira-bridge UI
    grafana: ""            # → grafana Service:3000 (set "/grafana" to fan out)
  viaShim: []              # paths routed through gardenio-edge-auth HMAC shim
  routePriority: 100       # Traefik router priority for viaShim routes
  legacyRedirects:
    enabled: true          # 301 each legacy host → unified host/<path>
    exclude: []            # per-host opt-out (hold back kd, jira)
  forwardAuth:
    enabled: false         # ForwardAuth middleware gating every unified route
```

### MUST-set invariants (from `values.yaml` L209–215 header)

When an operator flips `unifiedHost.enabled=true`, the upstream `BASE_PATH`-equiv
env on each app MUST match the Traefik strip-prefix, or the app generates links
under the wrong prefix:

| Invariant | Why |
|---|---|
| `coopmux.basePath` **==** `unifiedHost.paths.mux` | coopmux SPA prefixes its fetch/WS URLs with `BASE_PATH` |
| `beadsViewer.ingress.pathPrefix` **==** `unifiedHost.paths.beads` | viewer's `BASE_PATH` drives every relative link / asset |
| `jiraBridge.basePath` **==** `unifiedHost.paths.jira` | jira-bridge UI asset/link prefix |
| kbeads daemon (`/kd`) — **none** | serves at root, path-agnostic; no upstream env |

> **Hard gate:** do NOT flip `unifiedHost.enabled=true` for prod (gardenio) without
> operator sign-off (ka-0000000026 runbook §"Hard gates"). The infra cutover lives
> under epic ka-0000006723, not here.

---

## 2. Knob → env-var → consuming-code map

### 2.1 slack-bridge (`templates/slack-bridge/deployment.yaml` L231–264)

| Env var | Helm derivation | Consuming code |
|---|---|---|
| `COOPMUX_PUBLIC_URL` | `https://{{ coopmux.ingress.host }}/mux` — **gated on `coopmux.enabled && coopmux.ingress.host`**. Hardcodes literal `/mux` suffix (NOT `unifiedHost.paths.mux`). | `cmd/slack-bridge/main.go:652` |
| `BEADS_WEB_URL` | `slackBridge.beadsWebURL` override **else** `https://{{ beads3d.ingress.host }}` (only if beads3d up). beads3d is **DEPRECATED**. | `cmd/slack-bridge/main.go:653` |
| `BEADS_VIEWER_URL` | precedence: `slackBridge.beadsViewerURL` → `https://{{ beadsViewer.avaIngress.host }}` (if avaIngress.enabled) → `https://{{ beadsViewer.ingress.host }}{{ beadsViewer.ingress.pathPrefix }}` | `cmd/slack-bridge/main.go` (bridge linkers); also `cmd/gb/bundle.go:490`, `cmd/gb/login.go` |

**Gap:** all three derive from **per-app legacy hosts** (`coopmux.ingress.host`,
`beads3d.ingress.host`, `beadsViewer.ingress/avaIngress.host`), never from
`unifiedHost.host`. The `/mux` suffix is a hardcoded literal, not `paths.mux`.

### 2.2 beads-viewer (`templates/beads-viewer/deployment.yaml`)

| Env var | Helm derivation | Consuming code |
|---|---|---|
| `BASE_PATH` (L116–118) | `{{ beadsViewer.ingress.pathPrefix }}` (only if non-empty) | viewer server — root path prefix for all relative links |
| `COOPMUX_PUBLIC_URL` (L183) | `https://{{ coopmux.ingress.host }}/mux` (gated on coopmux.enabled + ingress.host) | `cmd/beads-viewer/main.go:414`, `server.go:2200`, `agents.go:1462` |
| `COOPMUX_URL` (L175) | in-cluster `gardenio.coopmux.serviceURL` helper (not a public link) | `cmd/beads-viewer/main.go` |
| `GRAFANA_BASE_URL` (L186–204) | `beadsViewer.grafanaBaseURL` override **else** derive `https://{{ unifiedHost.host }}{{ unifiedHost.paths.grafana }}` **only when** `grafana.enabled && unifiedHost.enabled && unifiedHost.host && paths.grafana != ""` | `cmd/beads-viewer/main.go:417`, `server.go:301`, `charts.go:162` |

**Note:** `GRAFANA_BASE_URL` is the **one** env var that already derives from
`unifiedHost` (and only when fully enabled). Everything else is per-app-host.

### 2.3 grafana (`templates/grafana/deployment.yaml` L1–25, L94–98)

| Env var | Helm derivation | Notes |
|---|---|---|
| `GF_SERVER_ROOT_URL` (L94) | `grafana.rootURL` override **else** derive `https://{{ unifiedHost.host }}{{ paths.grafana }}` **only when** `serveFromSubPath && unifiedHost.enabled && unifiedHost.host` | Also drives in-pod probe prefix (regex-strips host from rootURL) |
| `GF_SERVER_SERVE_FROM_SUB_PATH` (L98) | `"true"` when `grafana.serveFromSubPath` | |
| `GF_AUTH_PROXY_*` | `grafana.auth.proxy.*` — trusts `X-Auth-Request-Email` from edge-auth forwardAuth | AVA header auth-proxy, no second login |

### 2.4 jira-bridge (`templates/jira-bridge/deployment.yaml` L151–153)

| Env var | Helm derivation | Consuming code |
|---|---|---|
| `BASE_PATH` | `{{ jiraBridge.basePath }}` (only if non-empty) | jira-bridge UI link/asset prefix |
| `JIRA_BASE_URL` (L71) | `jiraBridge.jira.baseURL` | linkifier target (Atlassian host, not gardenio) |

### 2.5 gitlab-bridge (`templates/gitlab-bridge/deployment.yaml` L68)

| Env var | Helm derivation | Notes |
|---|---|---|
| `GITLAB_BASE_URL` | `gitlabBridge.gitlab.baseURL` | external GitLab host, not a gardenio self-link |

No gardenio-self-link formation; out of scope for unification (external target).

### 2.6 coopmux (`templates/coopmux/deployment.yaml` L142–144)

| Env var | Helm derivation | Notes |
|---|---|---|
| `BASE_PATH` | `{{ coopmux.basePath }}` (only if non-empty) | SPA fetch/WS URL prefix. MUST == `paths.mux` when unified. **Set in no env today (all "").** |

### 2.7 edge-auth (`templates/edge-auth/deployment.yaml`)

HMAC shim that fronts kbeads/coopmux for `viaShim` paths. Not a link-former, but the
auth substrate the unified host depends on. Key knobs: `edgeAuth.host` (public hostname,
fails fast if empty when enabled), `EDGE_AUTH_UPSTREAM_{KBEADS,COOPMUX,NATS}`,
`edgeAuth.ava.trustSigner` (AVA JWT issuer).

---

## 3. Code consumers (env var → file:line)

| Env var | Reader(s) |
|---|---|
| `COOPMUX_PUBLIC_URL` | `cmd/beads-viewer/main.go:414`; `cmd/slack-bridge/main.go:652`; used in `beads-viewer/server.go:2200`, `agents.go:1462` |
| `BEADS_WEB_URL` | `cmd/slack-bridge/main.go:653`; ref'd `internal/bridge/coverage_alert.go:48` |
| `BEADS_VIEWER_URL` | `cmd/gb/bundle.go:490`; `cmd/gb/login.go:50,83`; slack-bridge linkers |
| `GRAFANA_BASE_URL` | `cmd/beads-viewer/main.go:417`; `server.go:301`; `charts.go:162` |

(Full controller-side link-former inventory is R1's deliverable, ka-0000003583. R2
stops at the env-var boundary.)

---

## 4. Per-environment current-state table

Two value layers compose: the **chart defaults** (`gardenio/helm/gardenio/values.yaml`,
monorepo) and the **env deploy-values** (`acme-helm-chart/charts/gardenio/values/*.yaml`,
separate repo). The PoC composes a third via terraform tftpl.

### 4.1 `unifiedHost` adoption

| Env | `unifiedHost` set anywhere? | Result |
|---|---|---|
| **chart default** | n/a | `enabled: false`, `host: ""` |
| **prod** (`gardenio.yaml`) | **NO** | unified host OFF — falls to chart default |
| **gardenio-next** | **NO** | OFF |
| **gardenio-rwx** | **NO** | OFF |
| **gardenio-e2e / -test** | **NO** | OFF |
| **customer-poc PoC** (`values-aws-poc.yaml.tftpl`) | **NO** | OFF (uses `avaIngress` on per-app host instead) |

> **Headline finding:** `unifiedHost.enabled=true` is set in **zero** environments
> today. The epic's "VERIFIED GROUND TRUTH" line saying prod already has
> `unifiedHost.enabled=true` is **NOT reflected in the `acme-helm-chart` deploy-values
> in this checkout** — `unifiedHost` appears in no env file. Either prod's live state
> diverges from the committed deploy-values, or that ground-truth note refers to the
> cutover epic ka-0000006723's planned/in-flight state. **This discrepancy must be
> resolved before any cutover** (flagged to epic; see §6).

### 4.2 Per-app host knobs (what each env sets TODAY)

| Knob | prod (gardenio) | gardenio-next | gardenio-rwx | PoC (aws-poc) | chart default |
|---|---|---|---|---|---|
| `beads.ingress.host` | `beads.gardenio.e2e.example.com` | `beads.gardenio-next.…` | `beads.gardenio-rwx.…` | (derived) | `""` |
| `coopmux.ingress.host` | `coopmux.gardenio.…` | `coopmux.gardenio-next.…` | `coopmux.gardenio-rwx.…` | (derived) | `""` |
| `coopmux.basePath` | **""** | **""** | **""** | **""** | `""` |
| `beadsViewer.ingress.host` | `beads-viewer.gardenio.…` | (n/a) | (n/a) | — | `""` |
| `beadsViewer.ingress.pathPrefix` | **""** | **""** | **""** | **""** | `""` |
| `beadsViewer.avaIngress` | **not set** (chart default off) | not set | not set | **enabled, host=`${beads_viewer_host}`, trustSigner** | `enabled: false` |
| `slackBridge.beadsViewerURL` | `https://beads-viewer.gardenio.…` (explicit) | not set | not set | — | `""` |
| `slackBridge.beadsWebURL` | not set | not set | not set | — | `""` |
| `jiraBridge.basePath` | **""** | **""** | **""** | — | `""` |
| `grafana.enabled` | **false** (no grafana block) | false | false | false | `false` |
| `grafana.rootURL` / `serveFromSubPath` | n/a | n/a | n/a | n/a | `""` / `false` |
| landing/edge host | `gardenio.e2e.example.com` | `gardenio-next.e2e.example.com` | `gardenio-rwx.e2e.example.com` | `aws-poc.gardenio.acme.example` | `""` |

Empty `pathPrefix`/`basePath` everywhere ⇒ apps serve at **root of their per-app
host** today; links are formed as `https://<per-app-host>/...`, NOT
`https://<unified-host>/<path>/...`.

### 4.3 What unified requires vs. today (the delta)

For a clean cutover, **each env** would need to set (driven from ONE host knob):

```yaml
unifiedHost:
  enabled: true
  host: "<gardenio | gardenio-next | gardenio-rwx | aws-poc>.…acme.example"
  viaShim: ["mux", "kd"]      # restore HMAC envelope for coopmux/kbeads
  forwardAuth: {enabled: true}
# AND the matching invariants:
coopmux:      {basePath: "/mux"}        # == paths.mux   (today "")
beadsViewer:  {ingress: {pathPrefix: "/beads"}}  # == paths.beads (today "")
jiraBridge:   {basePath: "/jira"}       # == paths.jira  (today "" / jira off)
grafana:      {serveFromSubPath: true}  # if /grafana fanned out
```

Plus the link-forming env vars currently derived from per-app hosts must be
re-derived from `unifiedHost.host`+`paths.*`:
- slack-bridge `COOPMUX_PUBLIC_URL`, `BEADS_VIEWER_URL`
- beads-viewer `COOPMUX_PUBLIC_URL`, `BASE_PATH`

**These derivations are the remediation surface** — today only `GRAFANA_BASE_URL`
and grafana `GF_SERVER_ROOT_URL` honor `unifiedHost`. A single-host-knob design
(see epic deliverable #2) would add `unifiedHost`-aware fallbacks to the
`COOPMUX_PUBLIC_URL` / `BEADS_VIEWER_URL` derivations in both deployments.

---

## 5. Knob coupling summary (one host → everything)

The desired end state: operator sets `unifiedHost.host` (+ `enabled`) **once per env**,
and the chart derives every link var. Current reality: link vars come from **5+
independent per-app host knobs** (`beads.ingress.host`, `coopmux.ingress.host`,
`beadsViewer.ingress.host`, `beadsViewer.avaIngress.host`, `beads3d.ingress.host`)
plus explicit `slackBridge.beadsViewerURL` overrides. The chart templates that need
`unifiedHost`-aware fallbacks added:

1. `slack-bridge/deployment.yaml` L231–234 (`COOPMUX_PUBLIC_URL`)
2. `slack-bridge/deployment.yaml` L255–264 (`BEADS_VIEWER_URL`)
3. `beads-viewer/deployment.yaml` L181–185 (`COOPMUX_PUBLIC_URL`)
4. `beads-viewer/deployment.yaml` L116–118 (`BASE_PATH` — already keyed on
   `ingress.pathPrefix`, which the invariant ties to `paths.beads`)

`GRAFANA_BASE_URL` (beads-viewer L186–204) and grafana `GF_SERVER_ROOT_URL` are the
template for how the others should derive: explicit override wins, else derive from
`unifiedHost` when fully enabled, else stay empty/legacy.

---

## 6. Open items flagged to epic ka-0000006413

1. **Ground-truth discrepancy (BLOCKER for cutover):** epic says prod already has
   `unifiedHost.enabled=true`; the committed `acme-helm-chart` deploy-values show it
   set in NO env. Resolve live-vs-committed drift before designing the cutover.
2. **Remediation (chart):** add `unifiedHost`-aware fallback to the four link-var
   derivations in §5 so one host knob drives all links. Mirror the existing
   `GRAFANA_BASE_URL` pattern.
3. **Remediation (deploy-values, separate repo):** once chart supports it, add a
   `unifiedHost` block to each env's deploy-values + set the three MUST-set invariants.
4. **Couples with gated cutover tasks:** ka-0000004747 (default-URL switch),
   ka-0000007283 (legacyRedirects flip), ka-0000003829. R2 files no flips; these
   remain gated under ka-0000006723.

---

*Generated by gardenio-r2-inventory-helm-hme (crew). Operator: Matt Baker.*
