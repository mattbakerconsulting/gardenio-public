# Gardenio-in-a-Box — Ingress / Edge Access Control (Research)

**Status:** Research / design input.
**Bead:** ka-0000002581 (child of mega-epic ka-0000001295 — "Gardenio in a Box: single-EC2 full install").
**Sibling research:** ka-0000003100 (public-exposure hardening / host firewall / k8s-API), ka-0000004895 (SSO / identity).
**Operator:** Matt Baker (oidc:matthew.baker@acme.example).
**Scope:** Define the edge stack for a public single-EC2 box with **no AWS Verified Access (AVA)** and no AWS managed edge services (ALB/ACM/Route53/WAF). What ships today, what's missing, and a concrete fail-closed recommendation.

---

## TL;DR

The chart already has a **strong, well-audited edge toolkit** — Traefik as sole ingress, forced HTTP→HTTPS redirect, a strict security-headers middleware, a pinned TLS option (Mozilla intermediate), oauth2-proxy forwardauth, per-route rate-limit/ip-allowlist Middleware CRs, and a default-deny NetworkPolicy set with agent-to-agent isolation. **The pieces exist; the gap for a public box is in the defaults and in coverage uniformity, not in the primitives.**

Six things must change to make the box safe on the public internet:

1. **Today's edge auth assumes AVA terminates OIDC upstream.** With no AVA, the chart's own **oauth2-proxy forwardauth** (already on by default, `keycloak.forwardAuth.enabled=true`) becomes the *only* gate. It must front **every** browser route, with **no AVA-host carve-out routes** rendered.
2. **`trustForwardHeader: true` is unsafe when Traefik is directly internet-facing.** With no upstream ALB, a client can forge `X-Forwarded-Proto`/`X-Forwarded-For`. The in-box profile must run Traefik with `forwardedHeaders.trustedIPs` **unset** and switch every per-IP middleware to **RemoteAddr (depth 0)** — and the chart's `trustForwardHeader` should be flipped off in the in-box values.
3. **NetworkPolicies and default-deny-egress default to OFF** (`networkPolicies.enabled=false`, `defaultDenyEgress=false`). For a public box both must be **on by default**, so a compromised agent pod cannot reach the control plane, postgres, or node IMDS.
4. **No `inFlightReq` (concurrency cap) and no `buffering` (request-body cap) middleware exists** anywhere in the chart. Both are cheap, both belong default-on at the edge for a small box.
5. **There is no WAF-lite layer.** Recommendation: ship **CrowdSec** (agent + Traefik bouncer plugin, streaming + fail-open) as the default behavioral/IP-reputation layer; reserve Coraza/full-CRS for larger boxes only.
6. **TLS provisioning today is DNS-01 via cert-manager + external-dns** — both AWS-coupled. The in-box default should be **Traefik built-in ACME (HTTP-01)** when a real DNS name exists, with a **step-ca / self-signed** fallback for IP-only / air-gapped installs.

**Default posture finding (verified):** In a vanilla `helm install` with no value overrides, **no application data routes render at all** — every service route is gated behind its own `*.enabled` / `*.ingress.enabled` flag (all false by default) or fail-fasts on an empty required `host`. The only IngressRoutes that render by default are the oauth2-proxy `/oauth2/` login-callback surface and the HTTP→HTTPS redirect. The box is therefore fail-*closed by omission* today — but "in a box" means we will turn services **on**, so we need them fail-closed **by gate**, not by being switched off.

---

## 1. Today's edge stack (ground truth from `gardenio/helm/gardenio/`)

### 1.1 Traefik as the single ingress

All public surface is expressed as Traefik `IngressRoute` CRs (`traefik.io/v1alpha1`). There is no second ingress controller. Every app route lists **both** the `web` (:80) and `websecure` (:443) entrypoints; plain HTTP is caught by a single catch-all redirect (see §1.2). The chart does **not** ship Traefik itself — it assumes a cluster-level Traefik install. *For the in-box case this becomes the k3s-bundled Traefik*, configured via a `HelmChartConfig` (see §6).

### 1.2 Forced HTTPS redirect — `templates/https-redirect.yaml`

A single web-only IngressRoute matches every host (`HostRegexp(\`^.+$\`)`, priority 1) on the `:80` entrypoint and applies a `redirectScheme: {scheme: https, permanent: true}` middleware (308). App IngressRoutes only list `websecure`, so plain HTTP never reaches a real backend. **Default: `tls.redirect.enabled=true`** — good. It is deliberately independent of `tls.enabled` so it works even when TLS terminates upstream. (Audit F3/F4.)

### 1.3 Security-headers middleware — `templates/security-headers.yaml`

Default-on (`global.securityHeaders.enabled=true`). Renders three Middleware variants:
- **`-security-headers`** (strict, default): HSTS (`customResponseHeaders` so it's emitted unconditionally), `contentTypeNosniff`, `frameDeny` (X-Frame-Options: DENY), `browserXssFilter`, `referrerPolicy`, `permissionsPolicy`, and a CSP that defaults to `default-src 'self'; … frame-ancestors 'none'` with `connect-src` scoped to `'self' + wss://*.<apex>` derived from `tls.domainName` (audit F5 — no unbounded `wss:`).
- **`-security-headers-iframe`** (SAMEORIGIN + `frame-ancestors 'self'`) for embeddable services.
- **`-security-headers-strict-htmx`** (drops `'unsafe-inline'` from `script-src`, adds `report-uri`) for free-htmx widget routes.

**Gap:** the strict CSP keeps `'unsafe-inline'` in `script-src` for the default variant (audit F6, still open) because the Vite SPAs bundle inline JS. Not blocking for the box, but worth tracking.

### 1.4 TLS — `templates/tls.yaml`, `templates/tls-option.yaml`

- **Certificate** (`tls.yaml`): a cert-manager `Certificate` (ECDSA P-256, 90d/30d renew, audit F2/F19) via a **Let's Encrypt DNS-01 ClusterIssuer**, plus an **external-dns `DNSEndpoint`** pointing the wildcard + apex at the NLB. **Both are AWS-coupled** (DNS-01 needs a Route53 credential; external-dns writes Route53). Skipped when `tls.existingSecretName` is set.
- **TLSOption** (`tls-option.yaml`, default `tls.createOption=true`, name `gardenio-tls-intermediate`): Mozilla "intermediate" — `minVersion: VersionTLS12`, AEAD/ECDHE-only cipher suites, `curvePreferences: [CurveP256, X25519]`, `sniStrict: true`. Referenced by every IngressRoute's `tls.options`. **This is reusable as-is on the box** (it's pure Traefik config, no cloud dep).

### 1.5 Edge auth — two distinct mechanisms

There are **two** auth gates in the chart; this is the single most important thing to understand for the box:

| Mechanism | What it is | Default | In-box relevance |
|---|---|---|---|
| **oauth2-proxy forwardauth** (`templates/forwardauth/*`) | Traefik `ForwardAuth` Middleware → in-cluster oauth2-proxy (`:4180`, 2 replicas, locked-down securityContext). Unauthenticated → 302 to Keycloak; authenticated → 202 + `X-Auth-Request-*` headers. | **`keycloak.forwardAuth.enabled=true`** | **This is the in-box gate.** Keep it; make it cover *all* routes. |
| **edge-auth shim** (`templates/edge-auth/*`) | Gardenio Go shim for **CLI / non-browser** callers; validates a PAT + stamps an HMAC envelope (`X-Operator` + `X-Edge-Auth-Sig`). Built for the **AVA** production front door (`/forwardauth/verify` validates an AVA JWT). | `edgeAuth.enabled=false` | The AVA-JWT path is **not** usable without AVA. The PAT/HMAC CLI path *could* be repurposed for `gb`/`kd` CLI auth on the box, but that's SSO-track work (ka-0000004895). |

The forwardauth Deployment supports `--provider=oidc` (Keycloak, the chart default) **or** `--provider=gitlab` (prod/staging override, gates on GitLab group membership). For the in-box default we assume **Keycloak in-box** (sibling research ka-0000004895 decides self-hosted-vs-federated; the operator flagged this for his call). Hardening already present: `--cookie-secure`, `--cookie-samesite=lax`, `--code-challenge-method=S256`, per-request CSRF, S256 PKCE, HTTPS-only OIDC issuer (template `fail`s on a plaintext issuer — audit F11), and `Authorization` deliberately kept out of `authResponseHeaders` (audit ka-0000000839).

### 1.6 Per-route protective middlewares (rate-limit / ip-allowlist / strip-prefix)

Each service template defines its own Middleware CRs inline:
- **`rateLimit`** (`average`/`burst` from values) — token bucket.
- **`ipWhiteList`** (`sourceRange` from values; **fail-fasts if enabled with an empty range**).
- **`stripPrefix`** for path-routed services.

Defaults are mixed: the global edge-auth knobs default off (`ipWhitelist.enabled=false`, `rateLimit.enabled=false`), **but** `coopmux.ingress.ipWhitelist.enabled=true`, `beads.ingress.ipWhitelist.enabled=true`, and the beads `/ci` + `/nats` route rate-limits default **true** (all with placeholder CIDRs that fail-fast if the route renders).

### 1.7 NetworkPolicies — `templates/networkpolicies/*`

Gated behind **`networkPolicies.enabled=false`** (off by default). When on, it renders:
- **`default-deny-ingress`** (`podSelector: {}`, all pods).
- **Optional `default-deny-egress`** (`defaultDenyEgress=false` by default) + per-component allow-egress for controller, beads, coopmux, NATS, postgres, forwardauth, roles-server, landing, vmsingle (audit F0).
- Per-service ingress allow-lists (beads, postgres←beads only, NATS, coopmux, forwardauth←Traefik only, roles-server, landing, grafana, the bridges, edge-auth, vmsingle).
- **Agent-to-agent isolation** (the key control for the box's threat model): `agent-ingress` allows only the controller (+ vmagent); **no agent-to-agent ingress**. `agent-egress` allows coopmux, beads (HTTP only — gRPC 9090 deliberately excluded), NATS, slack-bridge, kube-DNS, and internet 443/80 **with RFC1918 + `169.254.0.0/16` (IMDS) + `100.64.0.0/10` (CGNAT) excluded** — so a compromised agent can't reach the control plane, postgres, other agents, or steal node IAM creds via IMDS.

Cross-namespace Traefik reachability is templated via `networkPolicies.traefik.{namespaceSelector,podSelector}` (empty by default — **must be set** to the k3s `kube-system`/traefik selectors in-box, or all Traefik→service ingress is dropped). kube-DNS pinned via `networkPolicies.kubeDns.*` (defaults match kube-system/CoreDNS).

---

## 2. Route inventory — current public vs auth-gated

Classification of every `IngressRoute` in the chart. "Auth-gated" = carries the oauth2-proxy `-forwardauth` middleware (active because `keycloak.forwardAuth.enabled=true` by default). "App-layer" = no Traefik auth, but the backend validates a signature/token. "Public" = genuinely unauthenticated.

### 2.1 Browser routes — gated by oauth2-proxy forwardauth (when enabled + rendered)

| Route | File | Gate |
|---|---|---|
| beads daemon (main) | `beads/ingress.yaml` | forwardauth — **but** `beads.ingress.skipForwardAuth=true` by default ⚠ (relies on AVA/edge-auth instead; must NOT skip in-box) |
| grafana | `grafana/ingressroute.yaml` | forwardauth (if `forwardAuth.enabled`) |
| roles-server | `roles-server/ingressroute.yaml` | forwardauth |
| slack-bridge UI (non-webhook) | `slack-bridge/ingressroute.yaml` | forwardauth |
| landing | `landing/ingressroute.yaml` | forwardauth **unless `landing.public=true`** (default false) |
| coopmux (`/mux`, `/ws`, `/api`) | `coopmux/ingress.yaml` via `_helpers.tpl` `coopmux.ingressMiddlewares` | forwardauth (+ ipWhitelist default-on) |
| unified-host (`/beads`,`/kd`,`/mux`,`/nats`,`/jira`,`/grafana`) | `unified-host/ingressroute.yaml` | `unifiedHost.forwardAuth` (separate middleware, **default false**) OR edge-auth shim per `viaShim` |

### 2.2 App-layer-authenticated routes (no Traefik auth — by design)

| Route | File | Backend auth |
|---|---|---|
| beads `/ci` | `beads/ingress.yaml` | bearer token in daemon |
| beads `/nats` websocket | `beads/ingress.yaml` | ipWhitelist only (NATS authz) |
| slack webhook `/slack/*` | `slack-bridge/ingressroute.yaml` | Slack signing-secret HMAC |
| gitlab webhook `/webhook` | `gitlab-bridge/ingressroute.yaml` | GitLab signing secret |
| jira webhook `/webhook` | `jira-bridge/ingressroute.yaml` | Atlassian HMAC |
| trigger-engine `/webhooks/` | `trigger-engine/ingressroute.yaml` | signed secret |
| edge-auth shim (main) | `edge-auth/ingressroute.yaml` | PAT/HMAC inside shim |

### 2.3 Intentionally public (login/redirect plumbing — no app data)

| Route | File | Why public |
|---|---|---|
| oauth2-proxy `/oauth2/` | `forwardauth/ingressroute.yaml` | the login/callback/sign-out flow itself |
| keycloak runtime | `keycloak-runtime/ingressroute.yaml` | the OIDC provider |
| edge-auth `/auth/cli-exchange` | `edge-auth/ingressroute.yaml` | deliberate anonymous CLI-token exchange |
| HTTP→HTTPS redirect | `https-redirect.yaml` | scheme rewrite only |
| beads-viewer AVA / legacy-redirect | `beads-viewer/*` | **AVA-coupled — do not render in-box** |

### 2.4 Routes to disable / not render in the in-box profile

- **All AVA-host routes** (`*.avaIngress.*`, beads-viewer `ava-ingressroute.yaml`, beads AVA-host routes): AVA terminates auth upstream and **does not exist** in-box → these would be **unauthenticated holes**. Set every `avaIngress.enabled=false`.
- **beads `skipForwardAuth`** must be **false** in-box (it defaults true because prod uses AVA/edge-auth in front).
- **`unifiedHost.forwardAuth.enabled`** must be **true** in-box if unified-host is used (it's the consolidated front door and defaults false).
- The **`/ci`** and **`/nats`** routes: keep app-layer auth but additionally restrict by `ipWhitelist` to the operator CIDR, or leave disabled unless needed.

---

## 3. Gaps for a public, no-AVA box

| # | Gap | Severity | Fix |
|---|---|---|---|
| G1 | Edge-auth design assumes AVA terminates OIDC; AVA routes are unauth holes if rendered | **Critical** | Disable all `avaIngress.*`; rely on oauth2-proxy forwardauth; set `beads.skipForwardAuth=false`, `unifiedHost.forwardAuth.enabled=true` |
| G2 | `trustForwardHeader: true` + no upstream LB ⇒ `X-Forwarded-*` spoofable (auth-decision + rate-limit + HSTS scheme bypass) | **Critical** | In-box: Traefik `forwardedHeaders.trustedIPs` unset; flip `trustForwardHeader` off; per-IP middlewares use RemoteAddr (depth 0) |
| G3 | `networkPolicies.enabled=false` + `defaultDenyEgress=false` by default | **High** | Default **both true** in the in-box values; set `networkPolicies.traefik.*` to k3s kube-system/traefik selectors |
| G4 | No concurrency cap (`inFlightReq`) anywhere | **High** | Add a global `inFlightReq` Middleware (amount ~50, tune to box) applied to every route |
| G5 | No request-body cap (`buffering.maxRequestBodyBytes`) | **Medium** | Add a `buffering` Middleware (~2 MB default; raise per upload route) |
| G6 | No WAF-lite layer | **Medium** | Ship CrowdSec agent + Traefik bouncer (streaming, fail-open); see §5 |
| G7 | TLS provisioning is DNS-01 + external-dns (AWS-coupled) | **High** | In-box default: Traefik built-in ACME HTTP-01; fallback step-ca/self-signed (§6) |
| G8 | forwardauth coverage is per-template, easy to miss a new route | **Medium** | Add a chart-level lint/test asserting every non-allowlisted IngressRoute carries forwardauth (§7) |
| G9 | HSTS correctness depends on Traefik scheme detection, which G2 undermines | Medium | Fixed transitively by G2; do **not** submit to HSTS preload on a single box (irreversible) |

---

## 4. Recommended in-box edge middleware chain

Apply, in order (coarse → fine), to **every** rendered route. Order matters: cheap rejects first.

```text
1. buffering        (413 — cap request body, protect node memory/disk)
2. inFlightReq      (429 — global concurrency ceiling)
3. rateLimit        (429 — per-IP token bucket, RemoteAddr / depth 0)
4. security-headers  (HSTS, CSP, frame-deny, nosniff, …)
5. crowdsec-bouncer (403 — IP-reputation / behavioral, fail-open)   [§5]
6. forwardauth      (302→Keycloak — the auth gate)  [browser routes only]
   └ ipAllowList    (403 — optional operator-CIDR pin on admin/ci/nats routes)
```

Webhook routes (slack/gitlab/jira/trigger-engine) skip step 6 (they use app-layer HMAC) but **keep 1–5**. Login plumbing (`/oauth2/`, keycloak, cli-exchange) skips 6 but keeps 1–5.

**Critical config rule for direct exposure (no ALB):** every per-IP middleware (`rateLimit`, `ipAllowList`) must bucket/match on the **real RemoteAddr** — i.e. **omit `ipStrategy` / `depth: 0`**. Setting `depth>0` without a trusted proxy in front lets any client forge `X-Forwarded-For` and get their own infinite bucket or spoof an allowed IP. `depth` = number of trusted proxies in front (0 = direct, 1 = single LB, N = N proxies).

New Middleware CRs to add to the chart (gated behind an `edge.inFlightReq.*` / `edge.buffering.*` values block, default-on for the in-box profile):

```yaml
# inFlightReq — global concurrency cap
spec: { inFlightReq: { amount: 50 } }      # tune 20–100 to box size
---
# buffering — request body cap
spec: { buffering: { maxRequestBodyBytes: 2000000, memRequestBodyBytes: 1048576 } }
```

---

## 5. WAF-lite recommendation

Constraint: single small EC2, no AWS WAF, no budget for Traefik Hub. Findings:

| Option | What it stops | Cost on a small box | Verdict |
|---|---|---|---|
| **CrowdSec** (agent + Traefik bouncer plugin) | IP reputation (community blocklist) + behavioral scenarios (brute-force, scanners, HTTP probing); optional **AppSec** component for SQLi/XSS | ~100 MB RAM, modest CPU in streaming mode | **Recommended default-on** |
| **Coraza** (OWASP CRS) via OSS Traefik WASM plugin | Signature SQLi/XSS/LFI (CRS) | **CRS not even bundled in the OSS plugin; full CRS observed consuming many CPUs / tens of GB RAM** | **Avoid on a small box** |
| **fail2ban** on Traefik access logs | IP bans from log patterns | Near-zero per-request overhead; you maintain regex + real-client-IP/DOCKER-USER handling | Fallback / host-level SSH jail supplement |

**Recommendation:** ship **CrowdSec** as the default WAF-lite layer — agent + the Traefik **bouncer plugin** in **streaming mode** with **failure tolerance fail-open (`-1`)** so a CrowdSec hiccup never takes the site down. Trim costly scenarios (`http-bad-user-agent`) and unneeded collections to keep CPU low. If signature-based SQLi/XSS blocking is required, enable **CrowdSec's AppSec component** rather than adding the OSS Coraza WASM plugin (one fewer moving part, already integrated). Reserve Coraza/native-CRS for a future larger-instance profile only. Keep **fail2ban** as a host-level SSH/log jail supplement, not the primary HTTP defense.

Caveat: Traefik yaegi/WASM plugins run in-process — vet the bouncer plugin version and pin it. CrowdSec depends on correct real-client-IP, which ties back to the G2 trusted-IPs rule.

---

## 6. TLS + DNS for the box (no Route53/ACM/external-dns)

A single public IP rules out automated **DNS-01** (no DNS-provider API). Use **HTTP-01** (needs only inbound :80) or **TLS-ALPN-01** (needs only :443). Decision tree:

```text
Real public DNS name → box's public IP, and :80 (or :443) open?
├─ YES → ACME / Let's Encrypt, automated:
│   ├─ Single box, single Traefik (k3s default):
│   │     → TRAEFIK BUILT-IN certResolver (httpChallenge on :80, or
│   │       tlsChallenge on :443 if :80 must stay closed).
│   │       Persist acme.json (0600 / PVC); test on LE *staging* first.
│   │   [simplest — preferred for the box]
│   └─ Need certs as K8s Secrets / multiple controllers / future HA:
│         → CERT-MANAGER + LE HTTP-01, solver ingressClassName: traefik
│   HSTS: max-age=1y, includeSubDomains only if you control all subdomains, NO preload.
├─ ONLY AN IP (no DNS name) → public CA cannot issue:
│     → LOCAL CA: prefer step-ca (internal ACME) + cert-manager auto-renew;
│       else cert-manager SelfSigned→CA issuer; else static self-signed.
│       Put the IP in an IP: SAN. Cost: install CA root in client trust stores.
└─ AIR-GAPPED → step-ca offline as internal CA (+ cert-manager ACME via caServer).
```

**Recommendation for the in-box default:** Traefik **built-in ACME with HTTP-01** when a real DNS name is present (drop cert-manager + external-dns + the DNS-01 ClusterIssuer from the in-box profile). The existing `tls.existingSecretName` escape hatch already lets an externally-managed cert be referenced by name — reuse that for the step-ca / self-signed fallback. Keep the `tls-option.yaml` TLSOption as-is (cloud-independent). LE rate-limit traps to document: **5 duplicate certs / 7 days** (no override) and **5 failed-auth / hour** — always validate on staging first. Never submit a single box to the **HSTS preload** list (removal takes months and an unprotected subdomain becomes hard-unreachable).

---

## 7. NetworkPolicy posture for the box

The existing policies are well-designed; the box needs three changes:

1. **Default `networkPolicies.enabled=true` and `defaultDenyEgress=true`** in the in-box values profile. The chart comment already prescribes the safe rollout (staging soak then prod) — for a fresh box install we accept default-on.
2. **Set `networkPolicies.traefik.namespaceSelector/podSelector`** to the k3s bundled-Traefik selectors (e.g. `kubernetes.io/metadata.name: kube-system` + `app.kubernetes.io/name: traefik`), or every Traefik→service ingress is dropped by default-deny. Same for `kubeDns.*` if the distro differs (defaults already match kube-system/CoreDNS).
3. **Keep agent isolation exactly as-is** — it is the core control answering "a compromised agent pod cannot reach the control plane": no agent↔agent traffic, beads HTTP-only (no gRPC), IMDS/CGNAT/RFC1918 excluded from agent egress. This is already correct; just ensure it's *on* (point 1).

This composes with the host-level controls in sibling research ka-0000003100 (k8s API bound to loopback/private, SG + nftables exposing only 22/80/443, NodePort range never exposed). Edge access control (this doc) and host exposure (ka-0000003100) are the two halves of "only 80/443/22 reachable; control plane unreachable from outside."

---

## 8. Concrete checklist for the in-box values profile

- [ ] `keycloak.enabled=true`, `keycloak.forwardAuth.enabled=true`, `provider=oidc` (in-box Keycloak — confirm with ka-0000004895).
- [ ] **Disable every `*.avaIngress.enabled`** and the beads-viewer AVA/legacy routes. No AVA carve-outs render.
- [ ] `beads.ingress.skipForwardAuth=false`; `unifiedHost.forwardAuth.enabled=true` (if unified-host used); `landing.public=false`.
- [ ] Traefik (k3s `HelmChartConfig`): `forwardedHeaders.trustedIPs: []`, never `insecure: true`. Flip chart `trustForwardHeader` off in-box; per-IP middlewares depth 0.
- [ ] Add + enable global `inFlightReq` (amount ~50) and `buffering` (maxRequestBodyBytes ~2 MB) Middleware on all routes.
- [ ] Ship CrowdSec agent + Traefik bouncer (streaming, fail-open); trim scenarios; enable AppSec only if SQLi/XSS blocking required.
- [ ] TLS: Traefik built-in ACME HTTP-01 (real DNS name) or step-ca/self-signed via `tls.existingSecretName` (IP/air-gapped). Keep TLSOption. HSTS on, **no preload**.
- [ ] `networkPolicies.enabled=true`, `defaultDenyEgress=true`; set `traefik.*` + `kubeDns.*` selectors for k3s.
- [ ] Add a chart test asserting every rendered non-allowlisted IngressRoute carries the forwardauth middleware (regression guard for G8).
- [ ] `tls.redirect.enabled=true` (already default); `global.securityHeaders.enabled=true` (already default); `createOption=true` (already default).

---

## 9. Open questions for the operator / sibling tracks

1. **SSO source (ka-0000004895):** self-hosted Keycloak with local users vs federate to an external IdP. Matt flagged this for his call — it changes whether the box holds credentials. This doc assumes in-box Keycloak as the default forwardauth provider; the edge chain is identical either way (only oauth2-proxy's `--provider` + issuer change).
2. **CLI auth (`gb`/`kd`) on the box:** the edge-auth shim's AVA-JWT path is dead without AVA, but its PAT/HMAC path could front the CLI. Decide in the SSO track whether to repurpose it or rely on beads bearer tokens behind forwardauth.
3. **WAF default-on vs opt-in:** recommend CrowdSec default-on (fail-open makes it low-risk), but confirm the resource budget on the target instance size.
4. **`inFlightReq`/`buffering` values:** defaults proposed (50 / 2 MB) need tuning to the chosen EC2 size and the largest legitimate upload (e.g. coop recordings, image uploads).

---

### Appendix — primary sources

Chart (ground truth): `gardenio/helm/gardenio/templates/{https-redirect,security-headers,tls,tls-option}.yaml`, `templates/forwardauth/*`, `templates/edge-auth/*`, `templates/networkpolicies/*`, all `*/ingressroute.yaml`, `templates/_helpers.tpl`, `values.yaml`. Existing audits/runbooks: `docs/audits/ingress-tls-static.md`, `docs/audits/forwardauth-authorization-collision.md`, `docs/runbooks/ingress-trusted-ips.md`, `docs/research/beads-viewer-public-internet-readiness.md`, `docs/research/ava-edge-auth-prototype-learnings.md`.

External (cited in research): Traefik middleware docs (inFlightReq, rateLimit, ipAllowList, buffering, forwardedHeaders.trustedIPs); CrowdSec Traefik bouncer + AppSec docs; Coraza/Traefik WASM resource-usage reports; Let's Encrypt rate-limits + challenge-types; cert-manager HTTP-01; Traefik built-in ACME certResolver; k3s server CLI (bind-address/tls-san/disable); hstspreload.org.
