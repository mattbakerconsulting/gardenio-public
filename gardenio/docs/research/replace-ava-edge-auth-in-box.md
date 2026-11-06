# Research: Replace AWS Verified Access edge-auth with in-box authn (ka-0000006758)

**Epic:** [ka-0000001295] Gardenio in a Box — single-EC2 install, ZERO external cloud deps.
Public-internet safety is a HARD requirement (every public route auth-gated by default).

**Method:** code-grounded read of the edge-auth Go service, the Helm chart, the AVA
terraform module, and the daemon-side trust boundary, followed by adversarial verification
of the four central claims (all four returned **confirmed**).

**Bottom line:** AWS Verified Access (AVA) is **not coupled** to the gardenio auth stack.
It is an opt-in hardening layer that ships dark and is disabled by default. The chart
already ships a complete, AWS-agnostic Keycloak + oauth2-proxy ForwardAuth stack that is
**enabled by default**. Dropping AVA for the in-box build is primarily a matter of *not
instantiating* the AVA terraform module and leaving the AVA toggles at their defaults —
**no daemon code changes, no chart code changes**.

---

## 1. What the edge-auth deployment actually does

The edge-auth service (`gardenio/controller/cmd/edge-auth/`) is an authenticating reverse
proxy. It resolves a caller identity, then stamps a uniform **HMAC envelope** onto the
upstream request that the daemons verify. It supports four modes:

| Mode | Trigger | Default posture |
|---|---|---|
| **PAT** (GitLab Personal Access Token) | `GITLAB_BASE_URL` + `EDGE_AUTH_GITLAB_GROUP` set (`config.go:114-115`) | **Default-on** (when edge-auth is deployed) |
| **AVA** (AWS Verified Access JWT) | `EDGE_AUTH_AVA_TRUST_SIGNER` set (`config.go:68-79`, `server.go:70-78`) | **Opt-in, ships dark** |
| **Cookie / oauth2-proxy** | `EDGE_AUTH_OAUTH2_PROXY_AUTH_URL` set (`config.go:88-101`) | **Opt-in, ships dark** |
| **Forwardauth** (`/forwardauth/verify`) | endpoint always registered; functional only when AVA or PAT configured (`server.go:111-116`) | Conditional |

Regardless of mode, all paths converge on one signing call:

```
sig := signEdgeAuth([]byte(s.cfg.hmacKey), id.Email, auditID, upstreamPath, ts)   // proxy.go:308
```

producing the same four headers — `X-Operator`, `X-Auth-Audit-Id`, `X-Edge-Auth-Ts`,
`X-Edge-Auth-Sig` (`proxy.go:326-329`) — over the canonical message
`operator\nauditID\npath\nts` (`proxy.go:369-374`).

**Default posture for public safety:** edge-auth itself is **disabled by default** in the
chart (`edgeAuth.enabled: false`, `values.yaml:362`). The default public-route gating is
provided not by edge-auth but by Keycloak + oauth2-proxy ForwardAuth (section 3). Before
proxying, the edge **strips** all client-supplied `X-Auth-Request-*` headers
(`proxy.go:323-325`) to prevent identity smuggling — the only trusted identity downstream
is the HMAC-signed `X-Operator`.

---

## 2. Is it AVA-coupled?

**No.** AVA is gated behind a single config flag that defaults to empty, and the daemon
trust boundary is AVA-agnostic.

**Gating mechanism (edge-auth service):**
- `avaTrustSigner` defaults to `""` (`config.go:121`).
- When empty, `newAvaHandleVerifier()` returns `nil` (`ava.go:76-79`), so `s.avaVerifier == nil` (`server.go:73`).
- The AVA path in `handleProxy` runs only when `s.avaVerifier != nil` **and** the AVA header
  is present (`proxy.go:186`). With the flag empty, the AVA branch is unreachable and the
  shim behaves "exactly as the PAT-only edge" (documented `config.go:68-79`). Zero AVA code
  executes, zero AWS dependencies incurred.

**Gating mechanism (Helm chart):**
- `edgeAuth.ava.trustSigner: ""` (`values.yaml:473`) — empty disables the AVA branch entirely.
- `beadsViewer.avaIngress.enabled: false` (`values.yaml:3402`).
- `beads.ingress.avaIngress.enabled: false` (`values.yaml:2426`).

**Gating mechanism (daemon trust boundary):**
- kbeads (`kbeads/internal/edgeauth/verify.go:163-166`) and coopmux
  (`coop/crates/mux/src/transport/edge_auth.rs:222-232`) verify **only** the HMAC over
  `operator\nauditID\npath\nts` via constant-time comparison. There are zero
  auth-mode-specific branches; `auth_mode` is audit metadata only, never validated.

**Where AVA is actually wired (terraform, POC only):**
- The AVA terraform module is instantiated **only** in `terraform/composition/poc/edge_auth.tf:22`.
- `terraform/composition/canonical-prod/main.tf` calls only `network`, `data`, `eks`,
  `platform` — **no `edge_auth` module**.
- The module's single output `ava_instance_arn` (`modules/edge-auth/outputs.tf:6-9`) flows
  into the chart as `ava_trust_signer_arn` via `composition/poc/dns.tf:105` →
  `gardenio-chart/main.tf:655`. If the POC composition does not call the module, this variable
  is simply unset and the AVA branch stays off.

**Verdict:** AVA is a soft, opt-in layer scoped to the AWS POC composition. Nothing in the
daemons, the chart's default path, or the edge-auth shim's PAT/cookie paths requires it.

---

## 3. What the chart already ships for in-box auth

The chart ships a complete AWS-agnostic auth stack, **enabled by default**:

**Keycloak (realm + optional in-cluster runtime):**
- `keycloak.enabled: true` (`values.yaml:3999`) — renders KeycloakRealm / KeycloakClient /
  KeycloakGroup / KeycloakRealmRole CRDs.
- By default these reference an external ClusterKeycloak
  (`keycloak.realm.clusterKeycloakRef: "acme-keycloak"`).
- **In-cluster runtime exists but ships dark:** `keycloak.runtime.enabled: false`
  (`values.yaml:4017`). When enabled it deploys a PostgreSQL-backed Keycloak StatefulSet +
  Service + Traefik IngressRoute. **This is the toggle that makes the in-box build
  self-contained** (see section 4). Plan-of-record:
  `docs/research/keycloak-in-cluster-from-ecs.md`.
- Realm supports GitLab OIDC federation and generic corporate IdP federation; pre-configures
  OIDC clients for beads-viewer, coopmux, roles-server, iOS.

**oauth2-proxy ForwardAuth:**
- `keycloak.forwardAuth.enabled: true` (`values.yaml:4701`) — oauth2-proxy Deployment +
  Traefik Middleware.
- Middleware rendered when `traefik.enabled && keycloak.enabled && keycloak.forwardAuth.enabled`
  (`forwardauth/middleware.yaml:1`) — **zero dependency on edge-auth or AVA**.
- Emits `X-Auth-Request-User` / `-Email` / `-Groups` / `-Access-Token` to gated routes.

**Default route gating:**

| Route | Default gate | AWS dep? |
|---|---|---|
| beads-viewer (legacy host) | Keycloak ForwardAuth | No |
| coopmux (legacy host) | Keycloak ForwardAuth (+ optional IP allowlist) | No |
| kbeads daemon HTTP API | `skipForwardAuth: true` (`values.yaml:2354`); daemon validates `X-Operator` token | No |
| beads-viewer AVA route | AVA endpoint (`avaIngress.enabled: false`) | **Yes (off)** |
| beads daemon AVA route | AVA endpoint (`beads.ingress.avaIngress.enabled: false`) | **Yes (off)** |

TLS is sourced from `tls.existingSecretName` or cert-manager; ACM is optional, not required.

> **Note on `unifiedHost.forwardAuth`:** the unified-host ForwardAuth middleware
> (`values.yaml:325`, default `enabled: false`) points its `address` at the edge-auth
> `/forwardauth/verify` endpoint, which **requires `edgeAuth.ava.trustSigner` set or returns
> 404** (`forwardauth.go` dark-ship guard). For the in-box build, do **not** enable
> `unifiedHost.forwardAuth` against that address — use the per-host Keycloak oauth2-proxy
> middleware instead (the default), which is AWS-free. If a unified host is desired in-box,
> its ForwardAuth `address` should be repointed at the oauth2-proxy `/oauth2/auth` Service,
> not the AVA-only edge-auth verify endpoint.

---

## 4. Recommendation

For the in-box (single-EC2, zero-cloud-dep) build, **AVA is dropped by omission, not by
surgery.** The in-box path is Keycloak (in-cluster runtime) + oauth2-proxy ForwardAuth,
which is already the chart default for everything except the runtime backend.

### What already works out-of-the-box (no change)
- Keycloak ForwardAuth gating on browser routes (beads-viewer, coopmux) —
  `keycloak.enabled: true`, `keycloak.forwardAuth.enabled: true`.
- Daemon HMAC trust boundary — kbeads + coopmux verify the envelope identically regardless of
  mode; **no code changes**.
- AVA already off — `edgeAuth.ava.trustSigner: ""`, `beadsViewer.avaIngress.enabled: false`,
  `beads.ingress.avaIngress.enabled: false`.

### Terraform: exclude the AVA module
- Do **not** instantiate `module "edge_auth"` (the AVA module) for the in-box composition. It
  lives only in `composition/poc/edge_auth.tf:22`; the in-box composition simply should not
  call it (mirroring `canonical-prod`, which already omits it).
- Consequently drop the inputs the module needs and that the in-box build cannot satisfy
  without cloud: `hosted_zone_id` (Route53), `domain_certificate_arn` (ACM),
  `target_load_balancer_arn` (NLB), `oidc_secret_kms_key_id` (KMS CMK), and the Secrets
  Manager OIDC secret shell (`modules/edge-auth/main.tf:58-93`).
- Drop the ESO IRSA decrypt grant wired via `module.edge_auth.oidc_secret_arn`
  (`composition/poc/platform.tf:47-56`).
- Leave `ava_trust_signer_arn` unset in the chart values overlay (it is fed from
  `module.edge_auth` at `composition/poc/dns.tf:100-106` / `gardenio-chart/main.tf:638-657`);
  unset → AVA branch off.

### Chart values to set/unset
Set (turn on the self-contained backend):
- `keycloak.runtime.enabled: true` — deploy the in-cluster Keycloak StatefulSet so there is
  no external ClusterKeycloak / ECS dependency. Point `keycloak.realm.clusterKeycloakRef` (or
  the issuer URL the oauth2-proxy uses) at the in-cluster runtime.
- TLS: `tls.existingSecretName` (operator-provided or cert-manager/self-signed) instead of an
  ACM wildcard.

Leave at default (already correct for in-box):
- `keycloak.enabled: true`, `keycloak.forwardAuth.enabled: true`,
  `keycloak.forwardAuth.ingress.enabled: true`.
- `edgeAuth.enabled: false`, `edgeAuth.ava.trustSigner: ""`.
- `beadsViewer.avaIngress.enabled: false`, `beads.ingress.avaIngress.enabled: false`.

### IngressRoutes that change
- **Remove/never render** the AVA IngressRoutes: `beads-viewer/ava-ingressroute.yaml` (gated
  by `beadsViewer.avaIngress.enabled`) and the beads-daemon AVA route (gated by
  `beads.ingress.avaIngress.enabled`). They stay un-rendered at defaults, so no template edits
  are required.
- **Keep** the legacy-host IngressRoutes (beads-viewer, coopmux) gated by Keycloak
  ForwardAuth — these become the sole public entry points.
- The kbeads daemon HTTP IngressRoute keeps `skipForwardAuth: true` and relies on the
  `X-Operator` token / HMAC envelope; this is unchanged and AWS-free.

### Optional: edge-auth shim for CLI/CI
If the in-box build wants the HMAC envelope for CLI/CI bearer-token access (`gb`/`kd`), set
`edgeAuth.enabled: true` and run it in **PAT or cookie mode** only (GitLab PAT validation or
oauth2-proxy cookie validation). Both are AWS-free; AVA stays off. This is optional — browser
apps work via Keycloak ForwardAuth without it.

### Gaps / risks to close before public exposure
- **First-admin bootstrap.** The in-cluster Keycloak runtime needs a documented bootstrap for
  the initial realm admin / client secrets (the POC sources OIDC client creds from Secrets
  Manager, which is being dropped). An in-box equivalent (sealed secret, generated-on-install
  admin password, or operator-supplied values) must be defined. This is the single most
  important public-safety gap: a default or blank admin credential on a public route is
  unacceptable.
- **MFA.** AVA is *not* the MFA provider here (the GitLab OIDC trust provider is), so dropping
  AVA does not by itself remove MFA. MFA must be enforced in the Keycloak realm/IdP. Verify the
  in-box realm config enforces it where required.
- **Group → RBAC mapping.** AVA's Cedar group policy (`modules/edge-auth/main.tf:166-179`)
  keyed on GitLab group membership is removed. The equivalent must be realized as
  GitLab-subgroup → Keycloak-group mapping plus the existing app RBAC (e.g. beads-viewer
  default role `gardenio-viewer`, `values.yaml:3418`). Confirm the oauth2-proxy
  `X-Auth-Request-Groups` projection and Keycloak group mappers cover the same authorization
  surface AVA's Cedar policy did.
- **TLS without ACM.** A public single-EC2 box needs a real cert (cert-manager + Let's
  Encrypt, or operator-provided). Self-signed is acceptable only for closed trials, not public
  exposure. (Coordinate with the route53/ACM-replacement research child.)
- **Default-deny invariant.** Ensure no public IngressRoute is rendered without a ForwardAuth
  middleware once AVA routes are gone. The default-on Keycloak ForwardAuth satisfies this for
  browser routes; the kbeads daemon route remains protected by the HMAC/token boundary. Audit
  any custom overlay that might add an un-gated route. (Overlaps the ingress/edge-access-control
  research child ka-0000002581.)

---

## 5. Effort estimate & residual risks

**Effort: Low for the auth plumbing; Medium once bootstrap + RBAC hardening are included.**

| Work item | Effort | Notes |
|---|---|---|
| Exclude AVA terraform module from in-box composition | Low | Don't call `module "edge_auth"`; drop its 4 cloud inputs + IRSA grant. Mirrors canonical-prod. |
| Chart values (enable in-cluster Keycloak runtime, set TLS, leave AVA off) | Low | Toggles only; no template edits. AVA routes already un-rendered at defaults. |
| Daemon / chart code changes | None | HMAC envelope is mode-agnostic. |
| First-admin / Keycloak bootstrap for in-box | Medium | New, security-critical; needs design + docs. |
| GitLab-subgroup → Keycloak-group RBAC mapping | Medium | Re-implement AVA Cedar policy intent in Keycloak group mappers; verify parity. |
| MFA enforcement verification | Low–Medium | Config + test in the in-box realm. |
| TLS (cert-manager / operator cert) | Low–Medium | Required for public exposure. |

**Residual risks:**
- **Bootstrap credential exposure** — highest risk; a misconfigured first-admin on a public
  box is a direct compromise path. Must be gated/tested before any public default.
- **RBAC drift** — Keycloak group mapping may not perfectly reproduce AVA's Cedar
  default-allow-by-GitLab-group semantics; risk of over- or under-granting. Needs explicit
  parity testing.
- **DAEMON_HMAC_KEY assumption** — daemons must have the key configured; if absent,
  verification falls back to legacy auth paths. Ensure the in-box install provisions it.
- **Single point of failure** — in-cluster Keycloak runtime is now on the critical path for
  all browser auth; no managed-Keycloak fallback in-box. Acceptable for single-EC2 but worth
  noting for availability.
- **Accidental non-empty `trustSigner`** — the empty-default path is unambiguous, but guard
  against an overlay accidentally setting a non-empty `edgeAuth.ava.trustSigner` (which would
  reactivate the AVA branch and its AWS public-keys dependency).

---

### Key file references
`config.go:68-79,114-115,121`; `ava.go:76-79`; `proxy.go:186,308,323-325,326-329,369-374`;
`kbeads/internal/edgeauth/verify.go:163-166`; `coop/crates/mux/src/transport/edge_auth.rs:222-232`;
`values.yaml:325,362,473,2354,2426,3402,3999,4017,4701`; `forwardauth/middleware.yaml:1`;
`terraform/composition/poc/edge_auth.tf:22`; `terraform/composition/poc/dns.tf:100-106`;
`terraform/composition/poc/platform.tf:47-56`; `terraform/modules/edge-auth/main.tf:58-93,166-179`;
`terraform/modules/edge-auth/outputs.tf:6-9`; `terraform/composition/canonical-prod/main.tf`;
`gardenio-chart/main.tf:638-657,655`.

---

*Produced for ka-0000006758 under epic ka-0000001295. Operator: matthew.baker@acme.example.
Method: code-grounded investigation + adversarial verification (4/4 central claims confirmed).*
