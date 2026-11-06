# Stage 3 plan: AVA CLI access via E+B

**Status**: draft, operator-confirmation gates marked inline.
**Bug**: [ka-0000002333] · **Epic**: [ka-0000003384] · **Plan task**: [ka-0000002789]
**Inputs**: Stage 1 design [ka-0000007408] · Stage 2 decision [ka-0000002489] (chose `e_plus_b`).
**Refs**: `gardenio/docs/research/ava-edge-auth-prototype-learnings.md`, `gardenio/controller/internal/oidcauth/ava.go`.

## What was decided (Stage 2)

Two workstreams, intentionally orthogonal:

- **E** — Path-prefix unification under a single AVA endpoint per env. Cost saver
  (~$56K/yr at 3-env steady-state per the learnings doc). Does **not** solve CLI on
  its own; browser-only.
- **B** — Auth-shim service (`gardenio-edge-auth`) on a non-AVA hostname per env, fronting
  in-cluster Services with GitLab-PAT-rooted auth. Solves the CLI gap.

E and B share no surface and can ship in either order; this plan sequences E first because
it lowers the per-env baseline before adding a second public hostname (B).

## End-state architecture

Per env (long-lived only — MR-ephemeral / RWX-`*` stay IP-whitelist, unchanged):

```text
                                  Internet
                                     |
                +---------------------------------------+
                |                                       |
       gardenio.<env>                            cli.gardenio.<env>
       (one AVA endpoint)                       (no AVA — non-AVA edge)
                |                                       |
       +-------------+                         +-----------------+
       | AVA + OIDC  |                         | gardenio-edge-   |
       | (browser)   |                         | auth (Go svc)   |
       +------+------+                         +--------+--------+
              |                                         |
              | X-Amzn-Ava-User-Context (signed JWT)    | X-Operator: <email>
              |                                         | (after PAT verify)
              v                                         v
        +---------------------- Traefik (in-cluster) ----------------------+
        | path fanout:                                                      |
        |   /beads/  -> beads-viewer Service                                |
        |   /kd/     -> kbeads Service (HTTP API)                           |
        |   /mux/    -> coopmux Service                                     |
        |   /mezmo/  -> mezmo-viewer Service                                |
        |   /jira/   -> jira-bridge Service                                 |
        +-------------------------------------------------------------------+
```

Trust model:

- AVA path: upstream verifies `X-Amzn-Ava-User-Context` per `oidcauth/ava.go`. No change.
- CLI path: upstream trusts a `X-Operator` (and `X-Auth-Audit-Id`) header **only** when
  the request arrives via the edge-auth shim Service (NetworkPolicy + an internal-only
  pre-shared HMAC tag the shim signs into the header to defend against header
  forgery from inside the cluster). Daemons keep `BEADS_DAEMON_TOKEN` for the
  agent path; the shim path is additive, not a replacement.

> **Operator confirmation gate #1**: We can either run **two** trust paths in the daemon
> (AVA JWT *or* shim-signed `X-Operator`) or have the shim mint short-lived AVA-shaped
> JWTs and reuse the existing `ava.go` verifier. The former is simpler; the latter
> unifies the verifier but adds a key-management surface in the shim. Recommendation:
> start with the two-path model; revisit once we see request volume.

## Workstream E — path-prefix unification

Single AVA endpoint per env at `gardenio.<env-suffix>` (e.g. `gardenio.e2e.example.com`),
fanning out by path in Traefik. Today's host-per-app pattern (`beads.gardenio.app...`,
`mux.gardenio.app...`, etc.) becomes a redirect to the unified host's `/beads`, `/mux`, etc.

### Routing convention

**Strip the prefix at Traefik** before forwarding to the upstream Service, and
require each upstream to honor a `BASE_PATH` env var for outbound URL generation
(asset URLs, link templates, redirects). This matches what `beads-viewer` already
does (`s.basePath + "/active"` etc.) and lets the upstream stay path-agnostic
for its server-side routing.

Rationale: prefix-strip keeps internal handler trees clean; `BASE_PATH` is needed
anyway for generated links. Prefix-preserve would force every upstream to handle
the prefix in its router *and* in its link generator — twice the surface.

### Per-component retrofit

| Component | Current | Needed for path-fanout |
|---|---|---|
| `beads-viewer` (`gardenio/controller/cmd/beads-viewer/`) | Full `BASE_PATH` support, deployment `env: BASE_PATH` already wired. | Verify against `/beads` prefix; add Traefik `StripPrefix` middleware. |
| `kbeads` HTTP API | Daemon serves `/v1/...` from root. | Add `BASE_PATH` honoring `/kd` prefix on incoming requests *or* let Traefik strip — pick: strip. Daemon stays root-routed; clients call `https://gardenio.<env>/kd/v1/...` and Traefik forwards `/v1/...` to the Service. |
| `coopmux` (Rust, `coop/crates/mux/`) | No `BASE_PATH`. WS + HTTP routes mounted at root. | Add `BASE_PATH` env honored by the Axum router for asset/template URLs; HTTP routes stay root after Traefik strip. WS upgrade path needs the same treatment. |
| `mezmo-viewer` | TBD — locate cmd (no `cmd/mezmo-viewer/` under controller; likely a separate image or part of another component). | Investigate first; if it lives in the chart but not in `controller/cmd/`, retrofit lives in the actual source. File a sub-task that begins with discovery. |
| `jira-bridge` | Has a webhook surface (`POST` from Jira) and likely a small UI. | Webhook path can stay root if Jira's webhook target URL is configurable; UI path follows the BASE_PATH pattern. |
| `Helm chart` | Per-app `IngressRoute` + per-app `avaIngress.host`. | Add unified-host `IngressRoute` with per-path Service routing; gate by `unifiedHost.enabled`. Keep per-app `IngressRoute`s as 301 redirects to the unified host (mirrors the existing legacy-host pattern in `beads-viewer/ingressroute.yaml`). |

### Per-env rollout (E)

1. Land the chart unified-host plumbing behind a feature flag (`unifiedHost.enabled=false` default).
2. Enable on `gardenio-rwx` (staging) first. Verify browser path through every app.
3. Run E2E. Smoke test agent → kbeads in-cluster path stays unchanged.
4. Promote to `gardenio` (prod), then `canonical-prod`, then long-lived dev envs.
5. Once the unified host is the canonical link, mark per-app hosts redirect-only and
   plan the AVA-endpoint deletes (one at a time, with the `LoadBalancerArn` immutability
   gotcha from learning #2 in mind).

## Workstream B — auth-shim service (`gardenio-edge-auth`)

A small Go service under `gardenio/controller/cmd/edge-auth/` (next to `beads-viewer`,
`controller`, `slack-bridge`).

### Responsibilities

1. Accept inbound CLI requests on `cli.gardenio.<env>` (non-AVA edge, regular ACM cert).
2. Extract `Authorization: Bearer <gitlab-pat>` or `--token` query (the latter only on a
   `/auth/login` endpoint, never on data paths).
3. Validate the PAT against `https://gitlab.com/api/v4/user` (resolves the operator
   identity) + a cached group-membership check against the gardenio group. Cache positive
   results for ~5 min, negative for ~30 s, key by PAT hash.
4. Reverse-proxy the request to the in-cluster Service for the requested upstream,
   adding:
   - `X-Operator: <email>` — operator identity, used for audit + ownership defaults.
   - `X-Auth-Audit-Id: <ulid>` — correlation id stamped into both shim and upstream logs.
   - `X-Edge-Auth-Sig: <hmac>` — HMAC of (operator + audit-id + path + ts) keyed by a
     shared secret known only to shim and daemon, defends against header forgery from
     any other in-cluster caller.
5. Audit-log every authn outcome (success + failure) and every proxied request
   in OCSF Access-Activity shape, sink CloudWatch + Mezmo (matches AVA's OCSF deny logs
   from the learnings doc).

### Wire shape

```
CLI:
  $ kd list --http-url https://cli.gardenio.e2e.example.com/kd --token $GARDENIO_TOKEN
  -> GET https://cli.gardenio.e2e.example.com/kd/v1/beads
     Authorization: Bearer <gitlab-pat>

shim (gardenio-edge-auth):
  - validate PAT against gitlab.com/api/v4/user (cached)
  - check group membership for gardenio (cached)
  - proxy:
      GET http://kbeads.<ns>.svc.cluster.local:<port>/v1/beads
      X-Operator: alice@example.com
      X-Auth-Audit-Id: 01J...
      X-Edge-Auth-Sig: <hmac-base64>

kbeads (daemon):
  - if X-Edge-Auth-Sig present AND HMAC verifies AND remote == shim Service IP:
      trust X-Operator, set request actor, proceed.
  - else if Authorization: Bearer == BEADS_DAEMON_TOKEN:
      legacy in-cluster path, unchanged.
  - else if AVA JWT path (already-existing oidcauth middleware):
      browser path, unchanged.
  - else:
      401.
```

### Token shape

> **Operator confirmation gate #2**: short-lived JWT vs raw PAT-on-every-call.
> Recommendation: **start with raw PAT** validated per request, with positive-result
> caching at the shim (~5 min). This is simpler, GitLab is the source of truth, and the
> attribution chain is `PAT → operator email` per request. Move to short-lived JWTs only
> if we need TTL parity with AVA session cookies (~hours) or if GitLab API rate limits
> bite us; both can be deferred.

### Authz scope

> **Operator confirmation gate #3**: Cedar parity vs gardenio-group membership.
> Recommendation: **gardenio-group membership for v1.** Cedar parity on the CLI path adds
> meaningful complexity (we'd need to translate the AVA-provided Cedar policy doc into
> shim-evaluable form, or embed a Cedar evaluator) and the v1 surface area is small
> (operators + CI tokens). File the Cedar-parity uplift as a follow-up bead.

### Helm + ingress

- New deployment `gardenio-edge-auth` under `helm/gardenio/templates/edge-auth/`.
- New `IngressRoute` for `cli.gardenio.<env>` on the same Traefik + ACM cert as the
  `*.app.<env>` chain — no new TLS cert work.
- NetworkPolicy: shim can reach `kbeads`, `coopmux`, `mezmo-viewer`, `jira-bridge` Services.
  Daemons accept `X-Operator`-bearing traffic *only* from the shim's Pod IP (or via the
  HMAC signature path above, which is the load-bearing guard either way).
- External Secret: GitLab API base URL + the shim HMAC key for daemons.

### Per-env rollout (B)

1. Land the shim code + chart bits behind `edgeAuth.enabled=false` default.
2. Enable on `gardenio-rwx` first. Smoke test `kd list --http-url https://cli.gardenio-rwx... --token $PAT`.
3. Smoke test `coop` against `cli.gardenio-rwx`.
4. Wire OCSF audit logs into Mezmo; verify denies show up.
5. Promote to `gardenio` (prod), then `canonical-prod`, then long-lived dev envs.

## CLI surface

- `kd`: existing `--http-url` continues to work. Add `--token` flag (already exists?
  verify in `kbeads/cmd/kd/`) that maps to `Authorization: Bearer <token>` on outbound.
  Add env var `GARDENIO_TOKEN` (preferred) with `KBEADS_TOKEN` as legacy alias for the
  in-cluster `BEADS_DAEMON_TOKEN` use.
- `coop`: same shape. Add `--token` + `GARDENIO_TOKEN` env in `coop/crates/cli/`.
- Discovery: documented in `gardenio/docs/agent-pod-keycloak-auth.md`-style runbook —
  operator generates a GitLab PAT with the `read_api` scope, exports it, runs
  `kd list --http-url https://cli.gardenio.e2e.example.com/kd`.

## Audit log schema (OCSF Access Activity)

Per-request shim record (JSON, lined into CloudWatchLogs and forwarded to Mezmo):

```
{
  "class": "Access Activity: Access",
  "time": "<iso8601>",
  "actor": { "user": { "email_addr": "alice@example.com" } },
  "src_endpoint": { "ip": "1.2.3.4" },
  "http_request": {
    "url": { "path": "/kd/v1/beads" },
    "http_method": "GET"
  },
  "metadata": {
    "audit_id": "01J...",
    "upstream_service": "kbeads",
    "shim_version": "..."
  },
  "status_id": 1,
  "status_detail": "ok | deny:not-in-gardenio | deny:invalid-pat | deny:rate-limit"
}
```

Deny entries set `actor` from whatever the PAT resolved to (or `null` on PAT-invalid),
mirroring the AVA `actor=null` semantic in the learnings doc.

## Open questions (Stage 1) — resolved positions

| Stage 1 question | Recommendation in this plan | Confidence | Follow-up if wrong |
|---|---|---|---|
| Per-operator attribution required? | Yes for v1 (via PAT-resolved email). | High. | Drop to "any gardenio member" with a single shared CI token; cheaper but blocks audit. |
| Forge / external CI traffic volume? | Assume low (under 10 rps steady-state); single-replica shim with PAT caching is enough. | Medium. | Add HPA + Redis-backed PAT cache. |
| TTL parity with AVA session cookie? | Not needed for v1. PAT TTL is operator-set in GitLab. | High. | Issue short-lived JWTs if leak window becomes a concern. |
| Cedar parity for CLI path? | No for v1 — gardenio-group membership is enough. | Medium. | File a Cedar-evaluator follow-up if any endpoint needs finer-grained authz. |

## Risk + rollback

- **Per workstream**: E and B each ship behind a chart-level feature flag; flipping it
  off reverts to today's host-per-app + browser-only AVA.
- **AVA endpoint deletion** is irreversible without a propagation window (learning #2).
  Stage E so the unified host is live + redirects in place for at least one release
  cycle before any per-app AVA endpoint is deleted.
- **Shim outage**: kills CLI; browser path unaffected. Daemons keep working in-cluster.
  No production data path runs through the shim.
- **PAT leakage** is the new operator-side risk. Mitigate via OCSF audit (we'll see the
  hits) and operator runbook ("rotate the GitLab PAT, don't commit it").

## Child-task breakdown (to be filed under epic ka-0000003384)

Filed after this plan lands and the operator-confirmation gates close. Anticipated set:

1. `coopmux`: add `BASE_PATH` support to Axum router + asset/template URLs.
2. `mezmo-viewer`: locate source, add `BASE_PATH`.
3. `jira-bridge`: audit endpoints, add `BASE_PATH` for UI surface.
4. `kbeads`: verify path-fanout works under `/kd` Traefik strip; no daemon change expected.
5. Helm chart: unified-host `IngressRoute` template, per-app redirect templates, feature flag.
6. POC: enable `unifiedHost.enabled=true` on `gardenio-rwx`, end-to-end verify, file delta as bugs.
7. `gardenio-edge-auth` Go service: scaffold `cmd/edge-auth/` + tests.
8. `gardenio-edge-auth`: GitLab PAT validation + group cache.
9. `gardenio-edge-auth`: reverse-proxy + `X-Operator` + HMAC signature header.
10. `gardenio-edge-auth`: OCSF audit logging + Mezmo wiring.
11. Daemon-side: trust `X-Operator` behind HMAC verification (kbeads, coopmux, future).
12. Helm chart: `edge-auth` deployment + `cli.gardenio.<env>` IngressRoute + ExternalSecret for HMAC key.
13. `kd` CLI: `--token` + `GARDENIO_TOKEN` env wiring.
14. `coop` CLI: `--token` + `GARDENIO_TOKEN` env wiring.
15. Per-env rollout: gardenio-rwx → gardenio (prod) → canonical-prod → long-lived dev envs.
16. Operator runbook: PAT provisioning, CLI URL discovery, audit-log location.

## Operator runbook

End-user docs for the shipped surface (PAT provisioning, CLI URL discovery,
audit-log location, troubleshooting): [`gardenio/docs/edge-auth-operator-runbook.md`](../docs/edge-auth-operator-runbook.md).

## Out of scope

- MR-ephemeral / RWX-`*` envs stay IP-whitelist (unchanged, explicit in `ka-0000005862`).
- Retiring Keycloak — separate epic.
- Replacing the in-cluster `BEADS_DAEMON_TOKEN` agent path — orthogonal; the shim is for
  external CLI traffic.

[ka-0000002333]: https://beads.gardenio.e2e.example.com/beads/ka-0000002333
[ka-0000003384]: https://beads.gardenio.e2e.example.com/beads/ka-0000003384
[ka-0000002789]: https://beads.gardenio.e2e.example.com/beads/ka-0000002789
[ka-0000007408]: https://beads.gardenio.e2e.example.com/beads/ka-0000007408
[ka-0000002489]: https://beads.gardenio.e2e.example.com/beads/ka-0000002489
