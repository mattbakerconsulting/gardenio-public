# Grafana enablement: findings and design

Status: verified 2026-09-21 against `main` (b415aa61), chart 2026.263.2, and the
live `gardenio` k3s release. Author: laptop-claude for the operator.
Companion plan: `docs/superpowers/plans/2026-09-21-grafana-enablement.md`.

This document is the evidence and the decisions. The plan is the work. **§0 is
binding, except G1 and G4, which are superseded (below).**

> **Superseded 2026-10-14: G1 and G4 (ziti-only exposure).** Plan
> `gd-P78ygCUgA9` ("Replace ziti with Keycloak at the public edge") deprecates
> ziti. Grafana is now published at `https://grafana.clonk.tv` behind the public-edge
> sign-in (pilot `gd-iS4Uxv9tfN`; edge sign-in `gd-hRltljHGon`; the Keycloak
> clients bead `gd-DAy1iWvO4y`), and Keycloak's browser-facing hostname is `auth.clonk.tv`. The
> ziti-specific text in §3 and the access paths in the companion plan's Phase 3
> (Tasks 5-7) are historical. G2, G3, G5-G10 still hold. Current operations:
> `gardenio/docs/grafana-operator-runbook.md`.

## 0. Decisions (binding)

| # | Decision | Why |
|---|---|---|
| G1 | ~~**Grafana is reached over ziti only.**~~ **SUPERSEDED by `gd-P78ygCUgA9`:** Grafana is reached at `https://grafana.clonk.tv` behind the edge sign-in (`gd-iS4Uxv9tfN`). *Original:* No Ingress, no LoadBalancer, no reopening of 443. | The chart renders no public route *by design*: `beadsViewer.devAuthBypass` is safe only because ziti is the single path in (`gardenio-ziti/ziti-host-deployment.yaml:11-13`). 443 was closed 2026-09-19 and the standing rule is "reopen only in the same change that lands an authenticated route". |
| G2 | **Auth is Keycloak OIDC** (operator's call, 2026-09-21), not the chart's header auth-proxy and not Grafana-local admin as the steady state. | Per-operator identity and role mapping. The local admin account remains only as break-glass. |
| G3 | **The chart has NO OIDC support today — it must be added.** `GF_AUTH_ANONYMOUS_ENABLED` is hardcoded `"false"` (`templates/grafana/deployment.yaml:424-425`) and the only auth knobs are `grafana.auth.proxy.*` (header-based) and a Traefik `forwardauth` middleware on the IngressRoute we are not rendering. There is no `GF_AUTH_GENERIC_OAUTH_*` anywhere. | This is a chart feature change, not a values flip. It is the single largest piece of work and the reason this is a plan rather than a one-line PR. |
| G4 | ~~**Keycloak must be exposed over ziti too, and `KC_HOSTNAME` changed.**~~ **SUPERSEDED by `gd-P78ygCUgA9`:** Keycloak is browser-reachable at `https://auth.clonk.tv/auth` via the public edge; no ziti service for Keycloak. The `KC_HOSTNAME` change still stands. | OIDC redirects happen in the BROWSER. The live realm's discovery doc returns `http://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8081/auth/...` for `authorization_endpoint` — an in-cluster URL no browser can reach. Grafana-over-ziti with an unreachable authorization endpoint is a login loop, not a login. |
| G5 | **`grafana.persistence.enabled=true` is mandatory, not optional.** | `readOnlyRootFilesystem: true` (`deployment.yaml:344`) and the `/var/lib/grafana` volume renders ONLY when persistence (or a plugin path) is set (`:580-596`, `:825-832`). Without it Grafana cannot create `grafana.db` and will not boot correctly. Accepted cost: a 1Gi RWO **local-path** PVC, node-local, lost with the node — same tradeoff vmsingle already takes. |
| G6 | **Admin password comes from a pre-created Secret** via `grafana.adminPasswordSecret`, key exactly `admin-password` (`deployment.yaml:359`). | Both `adminPassword` and `adminPasswordSecret` default to `""`, so no `GF_SECURITY_ADMIN_PASSWORD` renders at all and Grafana falls back to built-in `admin`/`admin`. Shipping that behind ziti is still shipping it. |
| G7 | **Do NOT enable any `*.externalSecret.enabled`.** Pre-seed Secrets by hand. | The three grafana ExternalSecret templates gate only on their own component flags, NOT on `externalSecrets.enabled` (which `deploy-values/gardenio-k3s.yaml:120` sets false because no ESO is installed). Enabling one renders an `external-secrets.io/v1` object against a missing CRD and fails the whole install. |
| G8 | **Dashboards arrive via the bridge, not the chart.** `grafana.sidecar.enabled` + `grafanaBridge.enabled` + `grafanaBridge.backfill.enabled`. | The chart deliberately renders no dashboard ConfigMaps — they were removed because ~200KB per revision overflowed the 1 MiB release-Secret limit (`values.yaml:11732-11734`). The 26 JSONs live at `gardenio/helm/gardenio/dashboards/` baked into the `grafana-bridge` image. |
| G9 | **Phase 1 lands Grafana WITHOUT OIDC, verified by port-forward.** OIDC is phase 3. | Proves the datasource, the PVC and the boot path independently of the largest and riskiest change. A failure in phase 3 then has one cause, not four. |
| G10 | **Out of scope:** vmalert, alertmanager, `vlsingle`/VictoriaLogs (and therefore the VictoriaLogs datasource), `vtsingle`/traces, `grafana.llm.*`, `grafana.assistant.*`, `imageRenderer`, `mcp-grafana`/`mcp-grafana-write`, `plugins.install`, and the external ACME-estate `grafanaApiKey` path. | Each is a separate tier with its own dependencies. Note `gd-q0Sy8rdaLc` (`gb logs` broken for all crews) is caused by `vlsingle: {enabled: false}` — real, adjacent, and deliberately NOT bundled here. |

## 1. What already exists (and is healthy)

**The metrics pipeline is done.** This is the reason enablement is cheap in
substance even though the auth work is not.

- `vmsingle` + `vmagent` have been up 2d7h. `vmsingle` is 30d retention, 5Gi
  local-path, pinned to the on-demand platform node
  (`deploy-values/gardenio-k3s.yaml:612-637`).
- **16 of 16 scrape targets UP, zero down** (`up == 0` returns empty). Scraped:
  `gardenio-agents` (the controller), `gardenio-beads`, `gardenio-beads-viewer`,
  `gardenio-coopmux`, `gardenio-trigger-engine`, and every live crew/gardener pod.
- **225 metric names stored, of which 165 are `gardenio_*` and 9 `kbeads_*`.**
  By family: `gardenio_beads` 35, `gardenio_agent` 33, `gardenio_session` 14,
  `gardenio_account` 11 (cost), `gardenio_jira` 9, `gardenio_advice` 7,
  `gardenio_claude` 6, `gardenio_formula` 6, `gardenio_thread` 6,
  `gardenio_trigger` 3, `gardenio_operator` 3, `kbeads_db` 9.
- Datasource target is already correct and already allowed by NetworkPolicy:
  **`http://gardenio-vmsingle:8428`** (`templates/grafana/configmap.yaml:20`
  resolves `gardenio.vmsingle.fullname` = `<release>-vmsingle`; confirmed by a
  local `helm template` render).

**26 dashboards already exist** at `gardenio/helm/gardenio/dashboards/`, covering
cost-overview (36 panels), jira-conversation-health (36), context-observability
(31), perf-overview (30), system-health (28), agent-latency-distribution (26),
jira-metrics (25), resume-latency (24), involuntary-deaths (22), operated-by
(22, audit), formula-step-dispatch (21), cicd-minutes (20), fleet-lifecycle (20),
account-capacity (19), and 12 more. No template reads them (`.Files.Glob` is not
used); they are baked into the `grafana-bridge` image at `/dashboards`.

**Grafana itself is deployed nowhere** — no pod in any namespace. The chart's
Grafana is hand-rolled (no subchart; `charts/` does not exist), image
`grafana/grafana:11.4.0` pulled from Docker Hub directly.

**`helm lint` and `helm template` both pass clean with Grafana on**, verified
locally against the k3s overlay. `grafana.enabled=true` alone renders exactly
three objects: ConfigMap, Service, Deployment.

## 2. What blocks a naive flip

Ordered by how much they hurt.

1. **No `/var/lib/grafana` write path.** See G5. A bare `grafana.enabled=true`
   renders only the `provisioning-datasources` and `tmp` volumeMounts.
2. **No OIDC in the chart.** See G3. This is a feature, not a flag.
3. **Keycloak is not browser-reachable.** See G4. `KC_HOSTNAME` is the in-cluster
   DNS name and there is no Ingress; the only IngressRoute in the cluster is the
   http→https redirect.
4. **`grafana.enabled=true` restarts the controller.** `controller/deployment.yaml:646-648`
   injects `GRAFANA_URL` whenever the flag is on. Expect a controller roll, and
   schedule it accordingly — do not flip this while a migration is mid-flight.
5. **No admin credential.** See G6.
6. **The ExternalSecret CRD trap.** See G7.
7. **Dashboards need the bridge image to actually be published.** The k3s overlay
   resolves it to
   `123456789012.dkr.ecr.us-east-1.amazonaws.com/ghcr/joyousaversion/gardenio/grafana-bridge:2026.263.2`.
   **Unverified** — confirm the tag exists before enabling the bridge, or phase 4
   fails on ImagePullBackOff.
8. **`values.schema.json` validates none of this.** The whole file is 61 lines and
   covers only `networkPolicies.traefik`. A typo in any `grafana.*` key is
   silently ignored — which is exactly the `global.project` failure class this
   repo just spent a plan fixing. Chart tests are the only guard.

Not a blocker, worth knowing: the pod runs `runAsUser: 65534`/`fsGroup: 65534`,
not the image's stock uid 472. Fine for a fresh PVC.

## 3. Access and auth design

Target shape:

```diagram
browser (ziti tunneler, #gc-admins)
  ├─ gardenio.grafana.ziti:3000  ──► ziti-host ──► gardenio-grafana:3000
  └─ gardenio.keycloak.ziti:8080 ──► ziti-host ──► gardenio-keycloak-runtime:8080
grafana pod ──► http://gardenio-vmsingle:8428        (datasource, in-cluster)
grafana pod ──► http://gardenio-keycloak-runtime:8080 (token/userinfo, in-cluster)
```

The split matters: Grafana's **back-channel** calls (token, userinfo) can stay on
the in-cluster Service, but `auth_url` and the issuer must be the **ziti**
hostname because the browser follows them. Keycloak must therefore be told its
public-facing hostname (`KC_HOSTNAME`) or it will keep advertising the
in-cluster URL in discovery and in redirects.

Adding a ziti service is **not** a chart-only change. Each service needs, on the
ziti controller: a `host.v1` config, an `intercept.v1` config, the service, a
Bind policy for `#gardenio-hosts`, a Dial policy for `#gc-admins`, and the
in-AWS router's named-identity admit rule (the "REQUIRED, easily missed" note in
`gardenio/helm/gardenio-ziti/README.md`). Then a binding line in
`ziti-host-deployment.yaml`, which today binds exactly two services
(`gardenio-viewer` → viewer:8095, `gardenio-beads` → beads:8080).

Keycloak client to create in realm `gardenio`: confidential, standard flow,
redirect URI `http://gardenio.grafana.ziti:3000/login/generic_oauth`, with a
group or role mapper feeding Grafana's `role_attribute_path`. The realm is live
and serving discovery at `/auth/realms/gardenio` (`KC_HTTP_RELATIVE_PATH=/auth`,
`KC_HOSTNAME_STRICT=false`).

## 4. Chart surface reference

`grafana:` block is `values.yaml:9587-10151`. Keys this plan touches:
`enabled` (9588, `false`), `adminUser` (9596, `admin`), `adminPasswordSecret`
(9601, `""`), `datasourceURL` (9604, `""` = auto), `rootURL` (9621, `""`),
`allowEmbedding` (9639, `true`), `auth.defaultOrgRole` (9651, `Viewer`),
`auth.disableLoginForm` (9655, `false`), `sidecar.enabled` (9681, `false`),
`persistence.enabled` (9774, `false`), `persistence.size` (9775, `1Gi`),
`ingress.enabled` (10117, `false`), `service.port` (10131, `3000`),
`resources` (10140-10146, req 100m/256Mi, lim 500m/512Mi), `nodeSelector`
(10149, `{}`), `tolerations` (10150, `[]`).

Templates: `templates/grafana/{deployment,configmap,service,ingressroute,pvc,rbac}.yaml`.
`rbac.yaml` and the Deployment's `serviceAccountName` are both gated on
`sidecar.enabled`, so there is no dangling-SA hazard.

A second `grafana:` at `values.yaml:11746` is **`grafanaBridge.grafana.*`** (the
bridge's read-back config), not a second install. Do not confuse them.

36 chart tests already touch grafana (`tests/grafana_test.go` ~30 cases,
`grafana_unified_host_test.go` 22, plus 17 per-dashboard tests and others).
Any new values key must come with tests in that style.

## 5. Non-goals

Everything in G10, plus: no change to the `grafana.auth.proxy.*` header path (it
stays off), no `unifiedHost.paths.grafana` fan-out, no public DNS, no SG change,
and no move of the 26 dashboards out of the bridge image into the chart.
