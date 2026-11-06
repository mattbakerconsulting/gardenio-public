# In-box DNS + TLS — replacing Route53 + ACM (Gardenio in a Box)

> **Bead:** [ka-0000004570](#) — Research: replace Route53 + ACM with in-box DNS + TLS.
> **Parent epic:** [ka-0000001295](#) — Gardenio in a Box: single-EC2 full install, in-box kube, zero external cloud deps.
> **Status:** research / recommendation. No implementation lands until the operator green-lights specific build steps.
> **Sibling research:** [ka-0000002581](#) (ingress/edge), [ka-0000004895](#) (SSO/identity), [ka-0000006758](#) (replace AVA edge-auth) — coordinate on the host/SNI scheme and ForwardAuth callback host.

This document recommends how to drop the Route53 + ACM + external-dns DNS+TLS
stack and serve the full gardenio stack on a single EC2 instance with **no AWS
API at runtime**, while keeping it **safe to expose on the public internet**
(a hard requirement added by the operator, [ka-0000000837](#)).

---

## 1. What we replace today

| Layer | Today (AWS-POC / prod) | Source |
| --- | --- | --- |
| Public zone | `modules/route53` — public hosted zone `gardenio.acme.example`, per-env workload subdomains as records | `gardenio/terraform/modules/route53/`, `composition/poc/dns.tf` |
| Wildcard cert | `modules/acm` — ACM wildcard for `aws-poc.gardenio.acme.example`, validated via **Route53 DNS-01** | `gardenio/terraform/modules/acm/`, `dns.tf` |
| In-cluster TLS | cert-manager `Certificate` → `ClusterIssuer letsencrypt-production`, **DNS-01**, wildcard `*.DOMAIN` + apex, ECDSA P-256, 90d/30d | `gardenio/helm/gardenio/templates/tls.yaml` |
| DNS record mgmt | external-dns `DNSEndpoint` CNAME-ing `*.DOMAIN` + apex → NLB (TLS passthrough to Traefik `websecure`) | `templates/tls.yaml` |
| TLS posture | Traefik `TLSOption` — Mozilla intermediate, TLS 1.2+, `sniStrict: true` | `templates/tls-option.yaml` |
| Platform addons | cert-manager + external-dns installed via `modules/platform` (IRSA + Helm) | `gardenio/terraform/modules/platform/` |

**Routing shape that drives the cert requirement.** Every gardenio service is its
own *single-label* subdomain IngressRoute — `auth.`, `beads.`, `grafana.`,
`coopmux.`, `slack-bridge.`, `gitlab-bridge.`, `jira-bridge.`, `beads-viewer.`,
`landing.`, `roles-server.`, `trigger-engine.`, `unified-host.`, plus the **apex**
(ForwardAuth callback). ~12–14 hostnames. That is why the chart issues **one
wildcard cert** today. Any in-box plan must answer: keep the wildcard, or change
the routing.

**Two seams the chart already gives us (no template rewrites needed):**

1. `gardenio.tls.secretName` helper (`templates/_helpers.tpl:325-331`) — every
   IngressRoute references the TLS secret *by name through this helper*, returning
   `.Values.tls.existingSecretName` when set, else `<release>-wildcard-tls`. **No
   IngressRoute hardcodes a secret name.** Swapping the cert source is a values
   change.
2. `tls.yaml` is gated `if and .Values.tls.enabled (not .Values.tls.existingSecretName)`,
   and the `DNSEndpoint` half is gated on `tls.dns.target`. Setting
   `existingSecretName` (or leaving `tls.dns.target` empty) **already disables the
   entire Route53/ACM/external-dns cert-manager block** with zero template edits.
   `tls.issuer` is also parameterized.

So "remove the AWS DNS+TLS runtime surface" is mostly a values overlay; the new
templates are limited to a cert *issuer chain* (and, for the wildcard-DNS default,
swapping one wildcard `Certificate` for N per-host ones).

---

## 2. The load-bearing constraint (verified)

**Let's Encrypt HTTP-01 cannot issue wildcard certs — only DNS-01 can.** Confirmed
against LE docs and the chart's own DNS-01 wildcard design. HTTP-01 proves control
per-hostname by serving a file on `:80`; a wildcard would require proving control of
an unbounded subdomain space, which only a DNS TXT challenge (DNS-01) can do.

The corollary the original task framing missed, and that the verification pass
surfaced: **you do not need a wildcard to keep subdomain routing.** gardenio's
subdomain set is *finite and known at chart-render time*, so you can issue **one
HTTP-01 cert per subdomain** (or a single multi-SAN cert) with **no DNS provider,
no DNS-01, no wildcard** — as long as `:80` is publicly reachable for the ACME
challenge. This reframes the decision and is the basis for the recommended default.

---

## 3. Recommendation — three paths

The chart's existing "default + escape-hatch" shape maps cleanly onto three modes.
Pick by what the operator is willing to provide (domain? public internet? air-gap?):

| Mode | DNS | TLS | Trust distribution | Use when |
| --- | --- | --- | --- | --- |
| **DEFAULT — sslip.io + per-host HTTP-01** | public `*.<dashed-ip>.sslip.io` (zero records, no domain) | Let's Encrypt **HTTP-01**, one cert per subdomain | **none** (publicly-trusted) | turnkey public box, no domain to hand |
| **OPTIONAL — real domain** | operator-owned `A` + wildcard-`A` → instance IP (any registrar, **not** Route53) | LE **DNS-01** wildcard (smallest chart diff) *or* HTTP-01 per-host (credential-free) | **none** (publicly-trusted) | production / vanity hostnames |
| **FALLBACK — self-signed local CA** | in-box CoreDNS/`/etc/hosts` or any resolvable name | cert-manager **SelfSigned → CA → leaf** (offline wildcard) | **required** (import root CA into browser/iOS/CLI) | air-gapped / no inbound internet |

### 3.1 DEFAULT: magic wildcard DNS (sslip.io) + per-host Let's Encrypt HTTP-01

**Why this is the default for a *public* box.** The original task assumed
self-signed-default. For a box explicitly required to be **safe on the public
internet** and reachable by **browsers, an iOS app, and the `gb` CLI**, the
dominant cost of self-signed is **trust distribution** — and it is brutal across
those three client classes (iOS needs a two-step manual trust toggle; Go's CLI
needs the CA in a scanned location; every browser needs a manual import). The
sslip.io + HTTP-01 path **eliminates that entire problem** by yielding
**publicly-trusted** certs. That single property outweighs the wildcard-vs-per-host
cost.

**How it works.** The box derives its public IP and uses hostnames like
`auth.<ip-dashed>.sslip.io`, `beads.<ip-dashed>.sslip.io`, …. The public
sslip.io/nip.io nameservers extract the embedded IP from any label and answer with
it — wildcard-style resolution for *any* public client, **zero DNS records, no
domain owned**. Because the public service does **not** issue wildcard certs (the
shared registered domain hits LE's per-registered-domain limit; sslip.io negotiated
a raised ceiling for **per-host HTTP-01** only), cert-manager issues **one
publicly-trusted cert per subdomain via HTTP-01**, Traefik solving the `:80`
challenge. Drop external-dns/`DNSEndpoint` entirely.

**Chart changes (proposed, gated behind an in-box values mode):**
- Add `tls.mode: perHost | wildcard` (default `wildcard` to preserve current
  behavior; in-box overlay sets `perHost`).
- In `perHost` mode, render N `Certificate`s (or one multi-SAN cert) over the
  enumerated service hostnames instead of the single `*.DOMAIN` cert; `issuerRef`
  → an HTTP-01 `ClusterIssuer` (LE prod).
- `tls.domainName` accepts `<dashed-ip>.sslip.io`; the per-service `*.ingress.host`
  values are derived from it.
- Keep `DNSEndpoint` gated off (`tls.dns.target` empty) — neither default needs it.

**Trade-offs:** ~8–10 certs instead of one wildcard (more ACME orders / renewal
surface); a **soft runtime dependency on sslip.io** (a one-person OSS service —
June 2026 nameserver churn caused transient SERVFAIL); the box's public IP is
embedded in every hostname. **Mitigations:** pin an **Elastic IP** (IP change ⇒
every hostname + cert reissues); document **self-hosting the open-source sslip.io
Go server** as the contingency exit; treat sslip.io as dev/PoC-grade DNS with a
documented escalation to a real domain.

### 3.2 OPTIONAL: operator-owned real domain (no Route53)

Operator owns e.g. `box.example.com` and creates `box.example.com A <ip>` +
`*.box.example.com A <ip>` at **any** registrar (this satisfies "no Route53").
TLS choice:
- **DNS-01 wildcard** (cert-manager webhook for their provider, e.g. Cloudflare):
  smallest diff to today's `tls.yaml` — keep the wildcard `Certificate`, just swap
  the issuer/solver. Cost: cert-manager holds a **DNS-provider API credential** at
  runtime (a secret in-cluster; not AWS, but not credential-free).
- **HTTP-01 per-host**: credential-free, same per-host shape as the default.

This is the most production-appropriate path when a real domain is acceptable:
stable vanity hostnames that don't leak the IP and survive a re-IP, publicly-trusted
TLS, zero trust distribution.

### 3.3 FALLBACK: self-signed local CA (cert-manager, fully offline)

For air-gapped / no-inbound-internet boxes only. cert-manager three-stage chain,
all in-cluster, **zero network**:
1. `ClusterIssuer{selfSigned:{}}`
2. root `Certificate{isCA:true, commonName/subject set, ~10y, ECDSA P-256,
   secretName: gardenio-ca}` in the cert-manager **cluster-resource namespace**
3. `ClusterIssuer{ca:{secretName: gardenio-ca}}`, then repoint the chart's wildcard
   `Certificate` at it (`tls.issuer: gardenio-ca-issuer`).

A private CA issues `*.DOMAIN` + apex directly (HTTP-01 wildcard ban doesn't apply),
so the **entire subdomain layout stays unchanged** and the leaf auto-renews forever
with no rate limits — ideal for frequently-reinstalled boxes. **The cost is purely
trust distribution**, which is why this is a fallback, not the public default:
- Export the root CA PEM at install; serve it from an unauthenticated well-known URL
  / the landing page.
- **Linux/CLI:** `/usr/local/share/ca-certificates` + `update-ca-certificates` →
  Go's `gb` CLI trusts it via `SystemCertPool`; offer `SSL_CERT_FILE` / `--cacert`
  as the no-root escape hatch.
- **iOS:** ship a `.mobileconfig`; document the **mandatory two steps** (install
  profile **and** flip *Settings → General → About → Certificate Trust Settings*).
- **Browsers:** import the same PEM.

Alternatively, the absolute-smallest-footprint variant: a one-shot
`openssl`/`step certificate create` wildcard cert + `kubectl create secret tls`,
wired via the existing `tls.existingSecretName` — **zero controllers, zero CRDs, no
new templates**, at the cost of manual renewal. Use a stable root CA + short leaf so
clients trust the *root* (re-import once), not each leaf.

**Do not default to step-ca:** it adds a CA *server* (pod + DB/PVC) to solve a job
cert-manager's CA issuer already does in-cluster; only justified if you later need
ACME-to-clients or SSH-CA.

---

## 4. cert-manager footprint

Modest on k3s: 3 deployments (controller, webhook, cainjector), ~64–160Mi requested
at idle, 1 replica each. It buys **hands-off leaf auto-renewal** that a static cert
does not. Worth keeping for all three modes. **CRDs must be installed before the
chart's `Certificate` resources apply** — gate the cert templates behind a
CRD-present check (mirroring the existing `tls.createOption` guard for Traefik CRDs)
or document the install-order dependency.

---

## 5. Gotchas (carry into implementation)

1. **HTTP-01 needs `:80` reachable and the ACME-challenge path un-swallowed.** The
   chart's catch-all `:80→:443` redirect IngressRoute must **not** intercept
   `/.well-known/acme-challenge/`. cert-manager's solver creates a higher-priority
   route, but verify ordering on k3s Traefik.
2. **Pin an Elastic IP** for the sslip.io default — IP change reissues every
   hostname and cert.
3. **sslip.io is a single point of failure** for the default path. Document
   self-hosting the OSS server and escalation to a real domain.
4. **`sniStrict: true`** (already in the `TLSOption`) means the served cert must
   cover **every** hostname clients use. Keep all services at **one label** under
   the apex — a single-label wildcard does **not** match `a.b.DOMAIN`, and SNI fails.
5. **Self-signed root CA `Certificate` must set `subject`/`commonName`** — RFC 5280
   requires a non-empty Issuer DN; the chart's current wildcard `Certificate` has no
   subject, so the new root CA cert must add one.
6. **cert-manager does not propagate a renewed *root* CA to existing leaves** — use a
   **10y root** so re-import is a once-a-decade event; persist/back up the CA secret
   across reinstalls or every rebuild forces client re-trust.
7. **DNS-01 (real-domain wildcard) puts a DNS-provider API credential in-cluster** —
   not AWS, but not credential-free. Call this out for the real-domain path.
8. **`existingSecretName` is cross-namespace by name only** — the secret must be
   reflected/replicated into the release namespace (values.yaml already notes this).
9. **The two in-box cert modes are mutually exclusive:** CA-issuer mode must **not**
   set `existingSecretName` (it wants the chart to keep creating the `Certificate`,
   just with a different `issuerRef`); static-cert mode **does** set it. Don't set
   both.
10. **Avoid pre-issued free wildcard services.** `traefik.me`'s `*.traefik.me`
    wildcard was discontinued and its cert revoked in 2025 — concrete warning against
    depending on any single free wildcard-cert provider; issue your own.
11. **DNS-rebind protection** in some resolvers drops answers pointing at private
    IPs — irrelevant for a public box, a trap if anyone tests sslip.io with a private
    IP.

---

## 6. Proposed acceptance for the build phase

- A values overlay (`values-inbox.yaml`) that puts the chart into the **sslip.io +
  per-host HTTP-01** default with **no AWS API at runtime** (no Route53, ACM,
  external-dns, IRSA for DNS/TLS).
- `tls.mode: perHost|wildcard` switch; `perHost` renders per-subdomain HTTP-01
  `Certificate`s; `wildcard` preserves today's behavior.
- The real-domain path = same overlay + a real `tls.domainName` (+ optional DNS-01
  issuer); the air-gapped path = SelfSigned→CA issuer chain or
  `tls.existingSecretName` static cert.
- HTTPS reachable on a fresh single EC2 over `https://<service>.<dashed-ip>.sslip.io`
  with a **publicly-trusted** cert and **no client trust import**.
- Documented trust-distribution runbook for the self-signed fallback (browser, iOS,
  CLI).

---

## 7. Open questions for the operator

- **OQ-1 — default DNS basis:** accept the sslip.io runtime dependency for the
  turnkey default, or require a real domain from the start (drop sslip.io to a
  PoC-only convenience)?
- **OQ-2 — host/SNI scheme:** coordinate with [ka-0000002581](#) (ingress) and
  [ka-0000006758](#) (replace AVA). Does the ForwardAuth callback stay on the apex,
  and is the per-service-subdomain layout retained (favoring per-host HTTP-01) or
  collapsed toward single-host paths (a larger app change — Keycloak issuer URLs,
  OIDC redirect URIs, bridge webhook paths, coopmux websockets all path-sensitive)?
- **OQ-3 — Elastic IP:** is pinning an EIP acceptable as a hard install prerequisite
  (required for stable hostnames/certs on the default path)?
