# gardenio-edge-auth E2E

End-to-end smoke for the Stage 3 `gardenio-edge-auth` shim (epic
[`ka-0000003384`](https://beads-viewer.app.e2e.dev.acme.example/bead/ka-0000003384)).
Test pyramid bead: [`ka-0000008114`](https://beads-viewer.app.e2e.dev.acme.example/bead/ka-0000008114).

## What lives here

| File | Purpose |
|---|---|
| `test-edge-auth.sh` | Runner — mirrors `tests/e2e/scripts/test-local-mode.sh` shape. Gates cover pod deployed, `/healthz`, `/auth/check` no-token + PAT, `/<prefix>/healthz` proxy, the AVA `/forwardauth/verify` endpoint, AVA-mode HMAC parity, unified-host BUG#1 regression (M/N), cookie reject/accept (O/P), WS-through-shim, the `/mux` single `text/html` front-door gate (T), the `/kd/nats` WS-route gate (U), and the consolidated auth-matrix write-attribution gates Q (GATING) + R. |
| `../scripts/test-edge-auth-box-auth.sh` | **Box-auth profile** probe (`EDGE_AUTH_PAT_DISABLED`, ka-0000006087 §6). Builds + boots the real `cmd/edge-auth` binary from source under the box-auth env and asserts the air-gapped-box readiness contract: Ready-with-cookie, NotReady-without, PAT-bearing `503`, and **zero outbound `gitlab.com` calls** — with a known-good PAT-enabled control proving the no-call assertion is non-vacuous (advice ka-0000002586). Needs no cluster (the box-auth profile is deployed only on the portable Gasbox, never in an e2e ns), so it runs GREEN in every shard with a Go toolchain + python3. Registered as the `edge-auth-box-auth` module in `run-shard.sh`. |
| `../claudeless/edge-auth-forwardauth-identity-write.toml` | Claudeless companion to Gate Q — the write-through-forwardauth-identity lifecycle (create → comment → assert `operated_by=oidc:<email>` on the event log). Runs in the `claudeless` module. |
| `README.md` | This file. |

## Scope

Twenty-plus gates (A–U) covering the full edge-auth surface that's shipped (W-B-7/8/9/10/11/12, W-B-13/14, W-E-6 all closed; gb `--token` parity via MR !3117 against ka-0000003384) plus the AVA forwardauth path (epics ka-0000004294 / ka-0000005862; commits a8d7e5ad8 / 5b7159ca1 / 08e9b496d). Gates skip cleanly rather than fail when their pre-reqs are absent (no deployment, no PAT, no AVA token, no `kd`/`gb` in PATH) — safe everywhere; tightens up when env supplies them. The exit code is `0` when every non-skipped gate passes.

The `/kd/nats` NATS-WS edge route now exists (commit 6be7d224, ka-0000006000) and is exercised live by **Gate U** — the local-loop was previously blocked on it. Remaining out-of-scope (separate beads): tampered-HMAC, revoked-PAT cache invalidation, daemon-side OCSF log assertions, and the full `gb agent start --local` loop end-to-end (its realtime bridge reaches NATS through the Gate U route, but the complete agent-start dance is broader than this module).

## Running

```bash
# Laptop, public Ingress, with a real PAT
EDGE_AUTH_URL=https://cli.gardenio-e2e.app.e2e.dev.acme.example \
  GARDENIO_TOKEN=<gitlab-pat> \
  ./test-edge-auth.sh

# Laptop, port-forwarded Service (auto-discovers deploy/gardenio-edge-auth)
NAMESPACE=gardenio-e2e \
  GARDENIO_TOKEN=<gitlab-pat> \
  ./test-edge-auth.sh

# CI / smoke without a PAT (only pod-deployed + /healthz + no-token 401 gates)
NAMESPACE=gardenio-e2e ./test-edge-auth.sh

# From repo root (Makefile)
make -C gardenio test-edge-auth
```

### Knobs

| Variable | Default | Purpose |
|---|---|---|
| `EDGE_AUTH_URL` | *(unset)* | If set, skip kubectl discovery and curl this URL directly. |
| `NAMESPACE` | `gardenio-e2e` | Kubernetes namespace to look up `deploy/gardenio-edge-auth` in. |
| `EDGE_AUTH_DEPLOYMENT` | `gardenio-edge-auth` | Deployment name probed for ready replicas. |
| `EDGE_AUTH_SERVICE` | `gardenio-edge-auth` | Service name for the port-forward fallback. |
| `EDGE_AUTH_PORT` | `8080` | Service port. |
| `EDGE_AUTH_PROXY_PREFIX` | `/kd/` | Upstream prefix exercised by the proxy gate. |
| `EDGE_AUTH_PROXY_PROBE` | `v1/health` | Path appended to the prefix for the proxy probe (kbeads canonical health). |
| `EDGE_AUTH_FORWARDAUTH_PATH` | `/forwardauth/verify` | AVA forwardauth endpoint exercised by Gates I/J/K. |
| `AVA_HEADER_NAME` | `X-Amzn-Ava-User-Context` | Request header carrying the AWS Verified Access signed JWT. |
| `EDGE_AUTH_AVA_TOKEN` | *(unset)* | Valid AVA user-context JWT — required for the AVA success/parity gates (K, L). Skips when unset. |
| `GARDENIO_TOKEN` | *(unset)* | GitLab PAT — required for `/auth/check`, PAT proxy gates, and Gate R (unified-host member PAT write). |
| `UNIFIED_HOST_URL` | *(unset)* | AVA unified host base (e.g. `https://gardenio.app.e2e.dev.acme.example`) whose Traefik fans `/kd` through the edge-auth shim. Drives Gates M/N and R. Unset in the ephemeral ns → those gates SKIP. |
| `BEADS_HTTP_URL` | *(unset)* | In-cluster kbeads daemon base. Exported by `run-shard.sh`. Drives **Gate Q** (the GATING assertion). Unset (laptop run) → Gate Q SKIPs. |
| `BEADS_TOKEN` | *(unset)* | kbeads daemon static token. Exported by `run-shard.sh`; authenticates the Gate Q write (attribution comes from `X-Operated-By`). |
| `EDGE_AUTH_FWDIDENTITY_EMAIL` | `gb-login-e2e@gardenio.dev` | Synthetic human identity (`oidc:<local>@<domain>`) injected for the Gate Q write. Deliberately distinct from the shard's ambient `BEADS_OPERATED_BY` so the assertion proves the per-request identity wins. |
| `EDGE_AUTH_NONMEMBER_PAT` | *(unset)* | Valid GitLab PAT for a user OUTSIDE `edgeAuth.gitlabGroup` — drives Gate R's non-member → 403 assertion. SKIPs when unset. |
| `EDGE_AUTH_COOKIE_NAME` | `_gardenio_oauth2` | oauth2-proxy session cookie name used by the cookie gates (O/P/S). |
| `EDGE_AUTH_TEST_COOKIE` | *(unset)* | A real oauth2-proxy session value. Drives the cookie SUCCESS gates: P (read) and S (write+attribution). SKIPs when unset — see the cookie gap below. |
| `CURL_TIMEOUT` | `10` | Per-request curl timeout in seconds. |
| `REQUIRE_DEPLOYMENT` | `0` | When `1`, missing `deploy/$EDGE_AUTH_DEPLOYMENT` is a hard fail (scaffold default: skip). Flip in CI once W-B-12 lands. |
| `LOG_ARCHIVE_DIR` | *(unset)* | Where to drop curl transcripts for post-mortem. |

## Gates

1. **Gate A — pod deployed**. `kubectl -n $NAMESPACE get deploy/$EDGE_AUTH_DEPLOYMENT` exists and reports `readyReplicas >= 1`. Skipped when `EDGE_AUTH_URL` is preset or `kubectl` is absent.
2. **Gate B — `/healthz` reachable**. Unauthenticated probe; should return HTTP 200.
3. **Gate C — `/auth/check` rejects missing token**. Expects HTTP 401 (or 503 pre-validator-config).
4. **Gate D — `/auth/check` with PAT**. `Authorization: Bearer $GARDENIO_TOKEN`. Expects HTTP 200 + a JSON body with `username` or `email`.
5. **Gate F — invalid PAT at proxy → 401**. Sends a deliberately-bogus bearer to `${EDGE_AUTH_PROXY_PREFIX}healthz`. Regression guard against the "shim passes anything-looking-like-a-bearer through" failure mode (and the `/groups/<g>` 404-denies-everyone misconfig pattern).
6. **Gate E — proxy reaches daemon**. `${EDGE_AUTH_PROXY_PREFIX}${EDGE_AUTH_PROXY_PROBE}` with the PAT → 200. The default probe (`v1/health`) targets kbeads' canonical health endpoint.
7. **Gate G — `kd --token` end-to-end**. `kd --http-url $EDGE_AUTH_URL/kd --token $GARDENIO_TOKEN list --limit 1` exits 0. Locks in W-B-13 (gd-DyEsW3BsVC).
8. **Gate H — `gb --token` end-to-end**. `gb --http-url $EDGE_AUTH_URL/kd --token $GARDENIO_TOKEN news` exits 0. Locks in the gb `--token` fix (MR !3117 against ka-0000003384). Skips cleanly when the `gb` in PATH predates that change (regression-safe for rolling agent-image rebuilds).
9. **Gate I — forwardauth rejects missing AVA token**. `GET $EDGE_AUTH_FORWARDAUTH_PATH` with no `X-Amzn-Ava-User-Context`. Asserts the dark-ship/disposition contract: `404 forwardauth_not_configured` when AVA mode is OFF (the e2e default) or `401 missing_ava_token` when ON. A 2xx here is a hard fail — the endpoint must never allow a request with no AVA context. Always runs.
10. **Gate J — forwardauth rejects malformed AVA JWT**. Sends a garbage value in `X-Amzn-Ava-User-Context`. With AVA mode ON the shim must verify the signature and return `401 invalid_ava_token` (no pass-through); with AVA mode OFF the dark-ship `404` wins and the signature check is skipped cleanly. Always runs (signature-check assertion gated on AVA mode ON).
11. **Gate K — forwardauth success → 202 + headers**. With a valid `EDGE_AUTH_AVA_TOKEN`, the verified path returns `202` and stamps `X-Auth-Request-User`, `X-Auth-Request-Email`, and `X-Auth-Audit-Id` (commit a8d7e5ad8 — oauth2-proxy-shaped headers coopmux/viewer consume). Skips without a valid AVA token / AVA-mode-ON endpoint.
12. **Gate L — AVA-mode HMAC parity**. Drives the SAME proxy upstream as Gate E but authenticates with the AVA JWT instead of a PAT; a `200` proves the AVA arm of `handleProxy` stamps the identical `X-Operator`/`X-Edge-Auth-Sig`/`X-Edge-Auth-Ts`/`X-Auth-Audit-Id` envelope the daemon already trusts (commit 5b7159ca1). Skips without a valid AVA token / AVA-mode-ON endpoint.

### Unified-host & cookie regression gates (BUG#1 / BUG#2)

13. **Gate M — unified-host `/kd` not shadowed**. No-auth request to `$UNIFIED_HOST_URL/kd/...` must reach the shim (`401`), not `303` to oauth2-proxy. Locks in BUG#1 (ka-0000004502). Skips without `UNIFIED_HOST_URL`.
14. **Gate N — unified-host `/kd` with PAT**. Member PAT through the unified host → `200`. Companion success to Gate M. Skips without `UNIFIED_HOST_URL`/`GARDENIO_TOKEN`.
15. **Gate O — forged session cookie rejected**. A junk `_gardenio_oauth2` cookie with no Bearer → `401`/`403`. Portable; always runs when an endpoint resolves.
16. **Gate P — valid session cookie authenticates**. With `EDGE_AUTH_TEST_COOKIE` set to a real oauth2-proxy session, a no-PAT request → `200`. Skips without a real session (see the cookie gap below).

### Consolidated auth-matrix gates (ka-0000007461)

17. **Gate Q — forwardauth-identity WRITE attribution (GATING)**. Runs **unconditionally** via `BEADS_HTTP_URL`, *outside* the edge-auth endpoint guard. Creates a bead + comment carrying an injected human identity (`X-Operated-By: oidc:<email>`), then `GET /v1/beads/{id}/events` and asserts every write event resolved to that `operated_by` — not the ambient shard operator, not empty, not a refusal. This is the **server-side equivalent of the gb-login cookie WRITE path** and the one assertion that is **green-able in the ephemeral e2e ns** (where edge-auth/unified-host are not deployed). A wrong/empty operator is a hard fail — that is the exact attribution bug the gb-login work exists to prevent. SKIPs only when `BEADS_HTTP_URL` is unset (laptop run of this script alone).
18. **Gate R — unified-host PAT WRITE + attribution + non-member 403**. The PAT companion to Gate Q, against the AVA unified host (`gardenio.app/kd`). Three assertions: (R1) member Platform PAT WRITE → `2xx`; (R2) the write attributes `operated_by=oidc:<email>` for the PAT's GitLab identity; (R3) a valid non-member PAT → `403` (authenticated, not authorized). SKIPs cleanly in the ephemeral ns exactly like Gates M–P — the unified host is not deployed there and the CI job carries no GitLab PAT secret. Needs `UNIFIED_HOST_URL` + `GARDENIO_TOKEN` (+ `EDGE_AUTH_NONMEMBER_PAT` for R3).
19. **Gate S — gb-login cookie WRITE + attribution (ka-0000001722)**. The cookie companion to Gate R's PAT write: a no-PAT request carrying a real `_gardenio_oauth2` session must (W1) create a bead → `2xx` through the shim's cookie branch and (W2) attribute `operated_by=oidc:<email>` for the email oauth2-proxy attests. The shim validates the cookie via an `/oauth2/auth` subrequest, lifts the attested `X-Auth-Request-Email`, and stamps it into the SAME HMAC envelope kbeads trusts (commit d52f2a60f) — so a green Gate S proves the cookie WRITE resolves identically to a PAT write. **The server-side, green-able-in-CI equivalent is Gate Q.** Gate S itself SKIPs cleanly in the ephemeral e2e ns (and any run without `EDGE_AUTH_TEST_COOKIE`): a real session is minted only by a browser + Cloudflare OAuth dance, not reproducible headlessly, and `cookieAuth.enabled` is off-by-default (only the long-lived staging singleton + an operator-gated deploy-values flip turn it on). The real-browser cookie WRITE + `>3m` silent-refresh validation against live `gardenio.app` is the **operator-only step** documented in [`gardenio/docs/gb-login-runbook.md`](../../../docs/gb-login-runbook.md) (escalation under epic ka-0000006278).

### Front-door content-type regression (ka-0000007632)

20. **`/mux` front door served as single `text/html`** (`gate_mux_html_content_type`, code "Gate T"). A PAT request to `$EDGE_AUTH_URL/mux/` must return `200` with **exactly one** `Content-Type` header whose value is `text/html` — never `application/json`, never a doubled `application/json, text/html`. This locks in the fix for the "coopmux front door returns a bunch of JavaScript" report: `handleProxy` sets `application/json` for its deny/error paths, and `httputil.ReverseProxy` copies upstream headers with `Add` (not `Set`), so without an explicit `Header.Del("Content-Type")` before the proxy handoff the dashboard SPA came back two-valued and browsers rendered the markup as text. SKIPs when `GARDENIO_TOKEN` is unset or the coopmux upstream returns `404`/`503` (same gating as the WS-through-shim gate). The unit-level guard is `TestProxy_ForwardsUpstreamContentType` in `gardenio/controller/cmd/edge-auth/proxy_test.go`.

### NATS-WS edge route (ka-0000006000)

21. **`/kd/nats` WS upgrade reaches NATS through the shim** (`gate_kd_nats_ws_route`, code "Gate U"). An authenticated WebSocket upgrade to `$EDGE_AUTH_URL/kd/nats/` must return `101 Switching Protocols` — reaching `gardenio-nats:8443`, **not** a `502` from falling through to the kbeads daemon (which has no WS handler). The shim registers `/kd/nats/` **before** `/kd/` (`buildUpstreams` spec order) because `matchUpstream` is first-match-by-order; without that ordering `gb agent start --local`'s realtime bridge dials `wss://<host>/kd/nats`, lands on kbeads, and `502`s. SKIPs when `GARDENIO_TOKEN` is unset or the NATS WS upstream isn't wired (`nats.websocket.enabled=false` → `404`/`503`, same gating as the WS-through-shim gate). The unit-level guard on the ordering is `TestBuildUpstreams_NatsRoutePrecedesKbeads` in `gardenio/controller/cmd/edge-auth/proxy_test.go`.

### Box-auth profile — `EDGE_AUTH_PAT_DISABLED` (ka-0000006087 §6)

The box-auth readiness contract is exercised by a **separate from-source probe** (`../scripts/test-edge-auth-box-auth.sh`, module `edge-auth-box-auth`), not by a gate in `test-edge-auth.sh` — the profile is deployed only on the portable Gasbox, so there is no e2e-namespace shim to port-forward. The probe builds + boots the real `cmd/edge-auth` binary three ways: (A) `PAT_DISABLED` + oauth2-proxy cookie → `/readyz` `200`, PAT-bearing `/auth/check` → `503`, and **zero** `GET /api/v4/user` calls to a mock GitLab; (B) `PAT_DISABLED` + no cookie/AVA → `/readyz` `503` (NotReady, reason surfaced); (C) a **known-good control** with the PAT path ENABLED whose mock-GitLab hit counter **must** increment — proving instance A's "hits=0" is real coverage, not a dead mock (advice ka-0000002586). The unit-level guards are `TestParseConfig_PATDisabledFlag`, `TestPATDisabled_NoValidatorBuilt`, and `TestPATDisabled_Readiness` in `cmd/edge-auth/pat_disabled_test.go`.

## Ephemeral e2e ns vs the long-lived staging singleton

**Architecture fact (confirmed).** The CI E2E suite (`e2e.yml` live lanes) deploys each run into a *leased pool namespace* (`gardenio-e2e-0`/`-1`). That namespace deploys **kbeads + the controller, but NOT `edgeAuth` / `unifiedHost`**, and the CI job carries **no GitLab PAT secret**. Therefore:

- **Shim gates (E–P) and Gate R inherently SKIP** in the ephemeral ns — there is no edge-auth deployment to port-forward, no unified host to curl, and no member/non-member PAT to send.
- **Gate Q is THE green-able gating assertion** in the ephemeral ns. It talks to the kbeads daemon the shard already port-forwards (`BEADS_HTTP_URL`) and exercises the same `operated_by` resolution contract the prod forwardauth → kbeads hop relies on — so it proves the auth-matrix write-attribution invariant without needing edge-auth deployed.
- The **shim/unified-host gates (E–R) only run against the long-lived staging singleton**, which deploys `edgeAuth` + `unifiedHost` with the same rigging as prod. Point `UNIFIED_HOST_URL`/`EDGE_AUTH_URL` + a member `GARDENIO_TOKEN` (+ `EDGE_AUTH_NONMEMBER_PAT`) at that singleton to exercise the full matrix.

## The gb-login cookie / forwardauth gap

The epic's success criterion #2 — *`gb login` against `gardenio.app`, then `kd`/`gb` (cookie only, no PAT) read+write succeed* — has a **headless-mint gap** that this suite documents rather than fully closes:

- The real cookie (`_gardenio_oauth2`, scoped to `gardenio.app`) is minted by a **browser + Cloudflare** OAuth flow (GitLab OIDC via oauth2-proxy). There is **no non-interactive way to mint a fresh, valid session cookie** inside a CI runner — the Cloudflare wall blocks a headless full re-auth, and no seeded test GitLab device-session or oauth2-proxy bypass token is wired into CI today.
- Consequently **Gate P** (valid-cookie success) SKIPs in CI: it needs `EDGE_AUTH_TEST_COOKIE` set to a real session value, which we cannot produce headlessly. Gate O (forged-cookie reject) is portable and always runs.
- **What we assert instead** (the server-side equivalent): **Gate Q** drives a WRITE that carries only the forwardauth-injected human identity (`X-Operated-By: oidc:<email>`) and proves kbeads records `operated_by=oidc:<email>`. This is exactly the identity the AVA forwardauth → kbeads hop stamps for a cookie-authenticated browser session, so a green Gate Q proves the *daemon-side* half of the cookie write path end-to-end. The browser→forwardauth→cookie half remains covered by manual verification (or a future seeded-device-session mechanism) — that is the documented gap.
- **Closing the gap** would require one of: a seeded test GitLab user with a long-lived device session whose cookie is injected as a CI secret, or a test-only oauth2-proxy bypass token. Both are out of scope for this module (test code only) and tracked under epic ka-0000006278.

## Wiring into CI

This module is wired into `gardenio/tests/e2e/scripts/run-shard.sh` (look for `should_run "edge-auth"`) and runs as part of the standard E2E shard. The Makefile target `make -C gardenio test-edge-auth` runs it locally against a port-forwarded service or the public Ingress (set `EDGE_AUTH_URL`).

To enable the PAT-bearing gates (D, E, F, G, H) inside CI, supply a `GARDENIO_TOKEN` for a GitLab user who is a member of the configured `edgeAuth.gitlabGroup` (currently `Acme/Platform` on staging) via a CI secret; without it those gates SKIP cleanly.
