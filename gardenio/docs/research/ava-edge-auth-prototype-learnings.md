# AVA Edge-Auth Prototype Learnings (2026-05-12)

Reference doc capturing hard-won AVA learnings from the 2026-05-12 prototype rollout
(sibling epic [ka-0000005862](https://beads.gardenio.e2e.example.com/beads/ka-0000005862)).
Intended as input to the canonical-prod identity design.

## Context

AVA (AWS Verified Access) is the edge-auth substrate we are evaluating for canonical-prod.
This document captures non-obvious gotchas surfaced during the prototype rollout — each one
cost us material time tonight. The terraform module for canonical-prod (Epic 2,
[ka-0000005315](https://beads.gardenio.e2e.example.com/beads/ka-0000005315))
and the identity design (parent epic
[ka-0000007440](https://beads.gardenio.e2e.example.com/beads/ka-0000007440)) need to
factor them in before we cut production terraform.

### Related beads

- **Parent epic**: [ka-0000007440](https://beads.gardenio.e2e.example.com/beads/ka-0000007440) — Identity
- **Grandparent**: [ka-0000005972](https://beads.gardenio.e2e.example.com/beads/ka-0000005972) — canonical-prod root
- **Sibling**: [ka-0000005862](https://beads.gardenio.e2e.example.com/beads/ka-0000005862) — the prototype this is learning FROM
- **Downstream**: [ka-0000005315](https://beads.gardenio.e2e.example.com/beads/ka-0000005315) — canonical-prod terraform module (Epic 2)

## Headline Learnings

### 1. `ApplicationDomain` is exact-match — no wildcards

AVA's `ApplicationDomain` field on a Verified Access endpoint is exact-match only.
There is no wildcard form (`*.example.com`).

Consequences:

- Each long-lived hostname needs its own AVA endpoint.
- Each endpoint carries its own ~$197/mo base bill.
- You **cannot** CNAME multiple hostnames to one endpoint and expect them to share auth:
  the session cookie that AVA writes is scoped to the `ApplicationDomain` value, not
  to the request `Host` header. The browser will dutifully present the cookie to the
  canonical domain only, and any CNAME alias will look unauthenticated to AVA.

This is the single biggest cost lever in the design space. Fanning out by **path**
under one `ApplicationDomain` (see learning #5) is the only way to keep the per-env
endpoint count bounded.

### 2. `ApplicationDomain` and `LoadBalancerArn` are CREATE-ONLY

Both `ApplicationDomain` and `LoadBalancerArn` on an AVA endpoint are immutable
after creation. Changing either requires **delete + recreate**, which:

- Allocates a new `EndpointDomain` (the AWS-managed hostname the endpoint exposes).
- Forces a Route53 update to point our CNAME at the new `EndpointDomain`.
- Carries a propagation gap during which the old endpoint is gone and the new one
  is not yet wired.

The K8s AWS Load Balancer Controller is a particular foot-gun here: it has a habit
of regenerating NLB ARNs on `Service` recreate (e.g. when a Service is deleted and
re-applied, or when certain spec fields change). If the AVA endpoint references
the old ARN, the endpoint silently goes dark.

Mitigations:

- **Pin the NLB**: never delete the Service backing the AVA target. Use targeted
  `kubectl patch`/Helm upgrades that preserve the existing `Service` object.
- Or, **reconcile on drift**: have a controller watch the Service's annotation/ARN
  and update the AVA endpoint's `LoadBalancerArn` (which is delete+recreate under
  the hood) automatically when drift is detected. This is the higher-cost option
  but tolerates more aggressive K8s churn.

### 3. AVA puts JWT `exp` in the JOSE header, not the payload

The signed JWT that AVA forwards to the upstream (via the `x-amzn-ava-jwt`
header) carries the standard `exp` claim **in the JOSE header**, not in the
payload.

Most off-the-shelf JWT libraries default to reading `payload.exp` for expiry
validation. Wired up naively, they will either:

- Reject every legit token (no `exp` in payload → treated as expired or invalid).
- Or worse, silently accept tokens with no expiry validation at all (if the lib
  treats missing-`exp` as "no expiry to check").

The reference verifier in this repo at `gardenio/controller/internal/oidcauth/ava.go`
reads `header.exp` correctly. Any non-Go service that consumes AVA JWTs needs to
either:

- Reproduce the same header-aware expiry logic.
- Or, use a wrapper that lifts `header.exp` into the payload before handing the
  token to the standard verifier.

### 4. OCSF deny logs: read the right class

When AVA denies a request and CloudWatchLogs is wired up, the denial shows up as
an OCSF event. Two different classes mean two different debugging paths:

| OCSF signal                                      | Meaning                                                | Where to look                                 |
| ------------------------------------------------ | ------------------------------------------------------ | --------------------------------------------- |
| `class: Access Logs` with deny outcome           | **Cedar policy denied** — user identified, not authorized | Cedar policy doc, group membership, claim values |
| `class: Access Activity: Access Deny`, `actor=null` | **AVA could not identify the user**                       | Cookie scoping, OIDC trust provider config, missing claim |

In the prototype we burned time chasing Cedar policy when the real signal was
`actor=null` — AVA never authenticated the request in the first place because
the cookie was scoped to the wrong `ApplicationDomain`. Always check `actor`
before opening the Cedar policy.

### 5. Cost: path-based fanout under one host

For our canonical-prod topology (gardenio-controller + beads-viewer + coopmux + mezmo
+ jira-bridge + ...), the natural shape is "one AWS account per env, N apps per env".
Two endpoint topologies are possible:

| Topology              | Endpoints per env | ~Cost per env  |
| --------------------- | ----------------- | -------------- |
| Host-per-app          | N (~9 today)      | ~$1.77K/mo     |
| Path-per-app, one host | 1                | ~$197/mo       |

The path-per-app topology routes everything under a single `ApplicationDomain`
(e.g. `gardenio.app.canonical-prod.example.com`) and fans out inside the cluster
via Traefik:

```
gardenio.app.canonical-prod.example.com/beads/...  → beads-viewer
gardenio.app.canonical-prod.example.com/mux/...    → coopmux
gardenio.app.canonical-prod.example.com/mezmo/...  → mezmo proxy
```

**Estimated savings: ~$1.7K/mo per env** at steady-state (vs. host-per-app).
Across our env footprint (prod + canonical-prod + a few long-lived dev envs)
this compounds quickly.

Requirements for path-per-app to work:

- Each upstream app must support a `BASE_PATH` (or equivalent) configuration
  so that internally-generated URLs and asset paths are prefixed correctly.
- Assets must be served via **relative** paths, or the prefix must be threaded
  through the asset URL builder.
- The Traefik routing rules need to strip or preserve the prefix consistently
  per app — pick one convention and codify it in the chart.

## Cost Analysis

### Per-endpoint pricing

AVA endpoint base cost: **~$197/mo** per endpoint (24/7), excluding per-request
data charges. Verify against the current AWS pricing page before finalizing —
the number above is what we observed billed on the prototype.

### Env-level cost model

Assume 9 apps per env (current gardenio footprint):

| Topology       | Endpoints | Monthly cost |
| -------------- | --------- | ------------ |
| Host-per-app   | 9         | ~$1.77K      |
| Path-per-app   | 1         | ~$197        |
| **Delta**      |           | **~$1.57K/mo**|

Across 3 long-lived envs (prod + canonical-prod + dev), the path-per-app topology
saves roughly **$4.7K/mo, or ~$56K/yr** at steady-state. This is the dominant
cost line in the AVA design space and overwhelms most other knobs.

### What stays IP-whitelist

Short-lived envs — MR-ephemeral, RWX-`*`, e2e-ephemeral — should **not** get an
AVA endpoint. The per-env base cost dominates their short lifetime. Keep these
on the existing IP-whitelist substrate (the gardenio-rwx pattern).

## Recommendations for canonical-prod Terraform

Concrete recommendations for the canonical-prod terraform module
([ka-0000005315](https://beads.gardenio.e2e.example.com/beads/ka-0000005315)):

### Endpoint topology

- **Default to one AVA endpoint per env**, fan out by path inside Traefik.
- Encode the path-per-app routing rules in the gardenio Helm chart so the same
  shape works in canonical-prod, dev, and any future env without per-env
  divergence.
- Reserve host-per-app for the (rare) case where an app genuinely cannot run
  under a `BASE_PATH` — document the exception, file a follow-up to fix the app.

### Short-lived envs

- **MR-ephemeral** and **RWX-`*`** envs stay on **IP-whitelist**, not AVA.
  The per-env AVA base cost is not justified for envs that live for minutes
  to hours.
- The terraform module should make this an explicit toggle (`enable_ava = bool`)
  with the default chosen per env-class.

### NLB stability

- **Pin the NLB**: structure the K8s `Service` so that recreates do not regenerate
  the NLB ARN. Use existing-LB reuse annotations where the AWS Load Balancer
  Controller supports them.
- **Or, reconcile on ARN drift**: ship a controller (or terraform run loop) that
  detects when the AVA endpoint's `LoadBalancerArn` no longer matches the live
  NLB, and triggers a delete+recreate of the endpoint with a coordinated Route53
  update. This is the more resilient option but costs a propagation window per
  drift event.
- Whichever path is chosen, **document the rebuild-cost** of an `ApplicationDomain`
  or `LoadBalancerArn` change in the terraform module README so operators do not
  treat these fields as edit-in-place.

### Observability defaults

- **Always enable `CloudWatchLogs`** on the Verified Access instance from terraform.
  The OCSF deny logs are the only way to debug authn vs authz failures (see
  learning #4) and turning them on after the fact requires an instance update
  with attendant blast radius.
- **Always enable `IncludeTrustContext`** so the trust provider's claim payload
  is captured in the deny logs. Without it, you cannot distinguish a missing
  claim from a wrong-value claim.
- Pipe the CloudWatchLogs group through to our standard log substrate so
  AVA-deny events appear in the same place as application logs.

### JWT verification contract

- **Standardize on the `oidcauth.AVAConfig` JWT verifier** for Go services.
  It reads `header.exp` correctly and handles the AVA-specific quirks documented
  here.
- For non-Go services, **document the contract** in the canonical-prod identity
  design: header-aware expiry, expected claim shape, key rotation cadence.
  A wrong implementation here fails closed (rejects legit traffic) or fails open
  (accepts unexpired tokens forever) — both bad.

## CLI accessibility — raw-cookie passthrough + silent refresh

**Status:** Resolved. Original Stage 1 report
([ka-0000007408](https://beads.gardenio.e2e.example.com/beads/ka-0000007408))
treated CLI / programmatic access through AVA as an open problem — AVA's OIDC
trust provider is auth-code-only, so neither a device-code grant nor
"PATs-as-AVA-bearers" was feasible, and an auth-shim service with a bearer
fallback required a split route between browser and CLI traffic. The resolved
design is implemented under epic
[ka-0000005293](https://beads.gardenio.e2e.example.com/beads/ka-0000005293)
and is the working model for canonical-prod.

### Design in one paragraph

The CLI carries the operator's browser cookies. A single in-app endpoint
inside beads-viewer (`/auth/cli-handoff`, AVA-gated) reads the operator's
`Cookie` header, stashes it under a 30-second one-shot exchange code, and
302s the browser back to a loopback URL on the CLI. The CLI redeems the
code at `/auth/cli-exchange` (not auth-gated — the code itself is the bearer
secret) and persists the bundle locally. From then on the CLI replays the
cookie header verbatim. When an AVA cookie expires, the CLI follows the
`AVA → GitLab → AVA` redirect chain with a real cookie jar; the GitLab
session cookie captured in the same dance silently re-auths, and the CLI
captures the fresh AVA `Set-Cookie` mid-chain. The operator sees nothing
until the GitLab session itself expires (days to weeks).

### Why this shape

- **No split route.** AVA gates one endpoint uniformly. There is no
  separate "browser path" and "CLI path" with different auth contracts —
  the CLI consumes a browser session, full stop.
- **No auth-shim service.** The keystone endpoint is a couple of
  handlers inside beads-viewer; no new deploy unit, no new identity
  trust, no new secret-rotation surface.
- **No daemon changes.** kbeads keeps verifying AVA JWTs the same way
  it does for browser traffic. The CLI is indistinguishable from a
  browser to anything past the AVA edge.
- **Silent refresh is free.** Standard `net/http/cookiejar` semantics
  plus a `Set-Cookie` capture transport. The cookie jar carries the
  GitLab session cookie forward across the redirect chain; we just
  persist what falls out.

### Trade we accepted

One carefully-audited HttpOnly-bypass endpoint (`/auth/cli-handoff`) is
the keystone. Threat model = an attacker who tricks a logged-in operator
into clicking a crafted handoff URL with a malicious return target.
Mitigations (see `gardenio/controller/cmd/beads-viewer/auth_cli.go` for
the full rationale):

1. **Strict loopback-only `return_url`** — numeric IPv4/IPv6 loopback only;
   `localhost` rejected (DNS-controllable); `http` scheme; explicit port;
   no userinfo / fragment.
2. **Short TTL** (30s) on the exchange code.
3. **One-shot consumption** — the first `/auth/cli-exchange` deletes the
   entry, so an in-window captured code cannot be replayed.
4. **Auth required on `/auth/cli-handoff`** — anonymous callers never
   reach the handler.
5. **State nonce** echoed back to the loopback — defends against
   cross-tab confusion when multiple CLIs race.

The CLI ends up storing the GitLab session cookie locally; that is a
material credential. OS secret store integration (Keychain / libsecret
with file fallback) is tracked under
[ka-0000001272](https://beads.gardenio.e2e.example.com/beads/ka-0000001272)
and lands incrementally. The file fallback today is
`~/.config/gardenio/credentials.json` with `0600` perms.

### Implications for canonical-prod identity design

- The AVA endpoint does **not** need a separate bearer/PAT trust path
  for CLI clients. One Verified Access endpoint with one OIDC trust
  provider serves both browsers and CLIs.
- The terraform module ([ka-0000005315](https://beads.gardenio.e2e.example.com/beads/ka-0000005315))
  needs to route `/auth/cli-handoff` and `/auth/cli-exchange` to
  beads-viewer like any other path. No special-casing.
- Cedar policy can treat CLI requests identically to browser requests
  (same actor, same claim shape, same cookie). The policy author does
  not need to special-case `User-Agent` or `actor.client_type`.
- Path-per-app fanout (learning #5) composes cleanly: the CLI auth
  endpoints sit under the single `ApplicationDomain` like everything
  else.

### See also

- User-facing runbook: `gardenio/docs/gb-login-runbook.md` (how to
  authenticate, where cookies live, when re-auth is required).
- Epic [ka-0000005293](https://beads.gardenio.e2e.example.com/beads/ka-0000005293)
  — interactive CLI auth via AVA + silent refresh.
- Superseded Stage 1 report
  [ka-0000007408](https://beads.gardenio.e2e.example.com/beads/ka-0000007408)
  — the device-code / PAT / auth-shim option space considered before
  the cookie-passthrough design landed.

## Open Questions

- **Key rotation cadence**: how often does AVA rotate the signing key, and what
  is the verifier's expected key-fetch / cache strategy? Needs measurement on
  the prototype before we lock the verifier contract.
- **Cedar policy hot-reload**: how long after a policy update does it take effect
  at the endpoint? Affects rollout strategy for permission changes.
- **Path-strip semantics in Traefik**: which apps want the prefix stripped, which
  want it preserved? Needs an inventory pass before we codify the chart routing
  rules.

## See Also

- Prototype rollout retro: sibling epic [ka-0000005862](https://beads.gardenio.e2e.example.com/beads/ka-0000005862)
- Canonical-prod identity design (parent): [ka-0000007440](https://beads.gardenio.e2e.example.com/beads/ka-0000007440)
- Canonical-prod terraform module (downstream Epic 2): [ka-0000005315](https://beads.gardenio.e2e.example.com/beads/ka-0000005315)
- Canonical-prod root: [ka-0000005972](https://beads.gardenio.e2e.example.com/beads/ka-0000005972)
- Reference verifier: `gardenio/controller/internal/oidcauth/ava.go`
- CLI auth runbook (operator-facing): `gardenio/docs/gb-login-runbook.md`
- CLI auth epic: [ka-0000005293](https://beads.gardenio.e2e.example.com/beads/ka-0000005293)
