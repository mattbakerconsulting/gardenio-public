# HSTS preload list submission — runbook

**Audit:** `docs/audits/ingress-tls-static.md` §F4 (not in this repo — the audits tree was excluded from the 2026-09-16 export)
**Bead:** ka-0000006090 (depends on F3 / ka-0000002211 — shipped)
**Affected chart objects:**
[`helm/gardenio/templates/security-headers.yaml`](../../helm/gardenio/templates/security-headers.yaml)
(emits `Strict-Transport-Security`),
[`helm/gardenio/templates/https-redirect.yaml`](../../helm/gardenio/templates/https-redirect.yaml)
(catch-all 308 → https on the `web` entrypoint).

> **Operator action required.** This runbook describes a submission to a
> third-party public list ([hstspreload.org](https://hstspreload.org/)). The
> submission is performed by a human operator who owns the apex domain and
> can demonstrate that ownership in the form Chrome's preload site requires.
> Agents must NOT submit. Agents may run the verification curl commands and
> file a decision bead with the results.

## What HSTS preload is

A Chrome-maintained list of hostnames that ship with browsers and force
HTTPS for every request, including the very first one — eliminating the
trust-on-first-use window that a plain `Strict-Transport-Security` header
leaves open. Other browsers (Firefox, Safari, Edge) consume the same list.

Submission criteria (Chrome, current as of 2026):

1. Serve a valid HTTPS certificate on the apex.
2. Redirect HTTP → HTTPS on the apex (301 or 308).
3. Serve all subdomains over HTTPS.
4. Respond with `Strict-Transport-Security:` on HTTPS, with:
   - `max-age` ≥ 31536000 (1 year — Chrome enforces this minimum)
   - `includeSubDomains`
   - `preload`

The chart's [`security-headers.yaml`](../../helm/gardenio/templates/security-headers.yaml)
sets `stsSeconds: 63072000` (2 years), `stsIncludeSubdomains: true`,
`stsPreload: true` — these match the criteria. The
[`https-redirect.yaml`](../../helm/gardenio/templates/https-redirect.yaml)
template catches every HTTP host and returns 308.

## Why this is a one-way door

A hostname on the preload list ships with a Chrome release. **Removing it**
means filing a removal request on hstspreload.org, then waiting for the
removal to roll through Chrome stable. Practically:

- Removal request acknowledged: hours-to-days.
- Removal lands in Chrome Canary: weeks.
- Removal reaches Chrome stable: **months** (a full Chrome release cycle
  plus the long tail of users who don't auto-update).
- Other browsers consume the Chrome list at their own cadence.

Until removal lands in a user's installed browser, that user's Chrome
**will refuse** any non-HTTPS connection to the host or any subdomain. If
HSTS pinning was accidentally extended to a sibling that doesn't speak
HTTPS, that sibling is dark for the same window.

Treat the preload list like a code-signing certificate: you can revoke it,
but you cannot un-publish it.

## Preload target — the apex you submit, not necessarily the cert apex

Submit `gardenio.e2e.example.com` (the gardenio apex), **not**
`e2e.example.com` or anything higher up the tree. With
`includeSubDomains` set, Chrome will pin every host below the submitted
name. That is fine when the submitted name is the boundary the gardenio
team owns and intends to keep on HTTPS forever; it is harmful at any
higher level the team does not control.

Operators submitting from a different deployment of this chart (e.g.
`gardenio.<your-domain>`) must do the same: submit the **gardenio-owned
apex**, not the host's parent domain.

## Preconditions before submission

All four MUST be satisfied. Skip any one of them and Chrome will reject
the submission, or accept it and leave a sibling subdomain dark.

1. **F3 shipped and live.** The `https-redirect.yaml` IngressRoute must
   be rendered (requires `tls.enabled: true` in chart values) AND the
   cluster-Traefik install must expose a `web` (HTTP/80) entrypoint on the
   same LB that serves `websecure`. If the chart does not own the HTTP
   entrypoint (e.g. an upstream ALB does TLS termination and answers
   HTTP→HTTPS itself), confirm that the upstream layer redirects 301/308.
2. **HSTS actually emitted on HTTPS.** Run the `curl` recipe below
   against the apex AND every gardenio subdomain. Every response must
   carry `Strict-Transport-Security: max-age=63072000; includeSubDomains; preload`
   (or equivalent values ≥1 year). If the header is missing, see the
   troubleshooting section — the most common cause is upstream TLS
   termination + untrusted `X-Forwarded-Proto`, in which case Traefik
   sees plain HTTP and Traefik's `Headers` middleware skips HSTS. That is
   a blocker; resolve via [`ingress-trusted-ips.md`](ingress-trusted-ips.md)
   first.
3. **Cert covers every subdomain you submit.** The chart's wildcard
   covers `*.gardenio.<apex>` plus the apex itself. Any sibling that uses a
   cert from a different issuer or that is intermittently unreachable
   over HTTPS will trigger Chrome's "all subdomains must serve HTTPS"
   reject.
4. **No HTTP-only sibling under the same name.** `includeSubDomains` is
   permanent for the preload window. If `legacy.gardenio.<apex>` is on
   plain HTTP for any reason, submission breaks it.

## Domain inventory (gardenio e2e prod)

The gardenio e2e production deployment publishes the following hosts.
Inventory is sourced from
`acme-helm-chart/charts/gardenio/values/gardenio.yaml` (and is the same
host shape — `<service>.gardenio.e2e.example.com` — under any other
gardenio deployment of this chart).

Submission target (apex):

- `gardenio.e2e.example.com`

Subdomains pinned by `includeSubDomains`:

- `beads.gardenio.e2e.example.com` (kbeads HTTP API)
- `beads-viewer.gardenio.e2e.example.com` (beads viewer SPA)
- `beads3d.gardenio.e2e.example.com` (3D force graph)
- `coopmux.gardenio.e2e.example.com` (terminal multiplexer)
- `gitlab-bridge.gardenio.e2e.example.com` (GitLab webhook receiver)

Routes that share the apex Host (counted as the apex, not separate
subdomains):

- `gardenio.e2e.example.com/oauth2/*` (forwardauth callback)
- `gardenio.e2e.example.com/` (landing page)

Operators MUST re-run the inventory before submission — adding a new
ingress between this doc and the submission would otherwise leave a
sibling unprotected. To regenerate:

```sh
# Run from a cluster-admin context in the gardenio namespace
kubectl get ingressroute -n gardenio \
  -o jsonpath='{range .items[*]}{range .spec.routes[*]}{.match}{"\n"}{end}{end}' \
  | grep -oE 'Host\(`[^`]+`\)' \
  | sort -u
```

…or, from the chart values, directly:

```sh
# From the deployment-repo checkout (e.g. acme-helm-chart):
yq '.. | select(has("ingress")) | .ingress.host // empty' \
  charts/gardenio/values/gardenio.yaml \
  | sort -u
```

## Verification (curl)

Run from outside the cluster and outside any LB-internal CIDR — the goal
is to see the response a browser sees. The following must hold for every
host in the inventory.

### 1. HTTPS returns HSTS

```sh
curl -sI https://<host>/ | grep -i '^strict-transport-security:'
```

Expected output (exact `max-age` may differ; anything ≥ 31536000
satisfies Chrome):

```
strict-transport-security: max-age=63072000; includeSubDomains; preload
```

If the header is missing, **stop**. Common causes:

- Upstream TLS termination + Traefik's own scheme observation says HTTP.
  The `Headers` middleware skips HSTS in that case. Fix via
  [`ingress-trusted-ips.md`](ingress-trusted-ips.md) (set the cluster
  Traefik's `forwardedHeaders.trustedIPs` to the LB CIDR so Traefik
  trusts `X-Forwarded-Proto: https` from the LB, OR terminate TLS at
  Traefik directly).
- `securityHeaders.enabled: false` in the chart values (default is
  `true` — check the wrapper chart).
- Chart's `tls.enabled: false` AND the upstream LB does not run the
  Traefik chain. The chart's `Middleware` resources still apply but the
  request path may bypass them depending on how the upstream LB
  forwards.

To confirm Traefik is the box emitting the response (vs an upstream LB
serving an error page), look for `content-security-policy` and
`referrer-policy: strict-origin-when-cross-origin` in the same response —
those come from the same chart middleware and prove the response went
through it.

### 2. HTTP returns 308 to the HTTPS scheme

```sh
curl -sI http://<host>/ | head -2
```

Expected:

```
HTTP/1.1 308 Permanent Redirect
Location: https://<host>/
```

A 301/302 to https:// is also acceptable for hstspreload.org, but the
chart's `https-redirect.yaml` is configured for 308 (`permanent: true`
in `redirectScheme`). Anything else — a 200, an HTML response, a
redirect to a different host, a redirect that drops the path — is a
blocker.

### 3. Subdomain HTTPS coverage

For each subdomain in the inventory, repeat steps 1 and 2. A single
unreachable subdomain (e.g. `getent hosts <subdomain>` returns nothing,
or HTTPS times out) means the submission will fail Chrome's "all
subdomains must serve HTTPS" check.

```sh
# DNS resolution check (no `dig` in the agent image — getent works in pods)
getent hosts <subdomain> || echo "DNS NOT RESOLVING: <subdomain>"

# HTTPS reachability check
curl -sI --max-time 10 https://<subdomain>/ -o /dev/null -w '%{http_code}\n'
```

Any HTTP status code (including 4xx and 5xx) is acceptable for the
preload check — Chrome only requires that a TLS handshake completes and
that the response carries HSTS. A connection failure or TLS failure is a
blocker.

### 4. One-shot script

The following condenses 1–3 into a single pass over the inventory.

```sh
HOSTS=(
  gardenio.e2e.example.com
  beads.gardenio.e2e.example.com
  beads-viewer.gardenio.e2e.example.com
  beads3d.gardenio.e2e.example.com
  coopmux.gardenio.e2e.example.com
  gitlab-bridge.gardenio.e2e.example.com
)

for h in "${HOSTS[@]}"; do
  printf '\n=== %s ===\n' "$h"
  printf 'DNS:   %s\n' "$(getent hosts "$h" | head -1 || echo MISSING)"
  printf 'HTTP:  %s\n' "$(curl -sI --max-time 10 "http://$h/" \
    | awk 'NR==1 || tolower($1)=="location:"' | tr '\n' ' ')"
  printf 'HSTS:  %s\n' "$(curl -sI --max-time 10 "https://$h/" \
    | awk 'BEGIN{IGNORECASE=1} /^strict-transport-security:/' \
    || echo MISSING)"
done
```

A passing run shows `MISSING` nowhere, an `HTTP/1.1 308` (or `301`/`302`)
plus a `Location: https://<same-host>/` for every entry, and a
`strict-transport-security:` line with `max-age` ≥ 31536000,
`includeSubDomains`, `preload` for every entry.

## Submission procedure

> **Performed by a human operator only.** Agents must not click submit on
> hstspreload.org. The form requires a Google account that owns or
> represents the domain.

1. Run the verification script above. Save the output. If anything is
   `MISSING` or red, **stop** and resolve the underlying issue first —
   re-running until clean is cheaper than removing a misfired preload
   entry.
2. Open <https://hstspreload.org/>.
3. Enter the apex `gardenio.e2e.example.com` in the "Domain" field.
4. The site auto-runs its own checks against the live host. They should
   match what the verification script saw — apex serves HSTS with
   `max-age` ≥ 31536000, `includeSubDomains`, `preload`; HTTP redirects
   to HTTPS; cert covers the apex; cert covers every subdomain the site
   discovers.
5. If the site reports any failure, **do not click "Submit"**. Resolve
   the failure (most are fixable in chart values: bumping `stsSeconds`,
   ensuring `securityHeaders.enabled: true`, fixing a sibling subdomain
   on the wildcard cert) and re-run the site's check.
6. Once all checks pass, tick the **acknowledgement** boxes — the form
   makes you confirm that you understand:
   - The submission is irreversible on a months-long timescale.
   - `includeSubDomains` will pin every current and future subdomain.
   - HTTP-only services under the apex will break.
7. Click **Submit**.
8. Save the submission acknowledgement (the success page is the only
   confirmation; there is no follow-up email).
9. File a `gb decision report` (or comment on this bead) with:
   - The submission timestamp.
   - The full output of the verification script at submission time.
   - The hstspreload.org status URL for the apex
     (`https://hstspreload.org/?domain=gardenio.e2e.example.com`).
10. Track the apex's progression to the Chrome list. The status URL
    above shows whether the host is "pending submission", "scheduled",
    or "preloaded". The full path from submission to Chrome stable is
    typically 6–12 weeks.

## Post-submission: ongoing obligations

These are permanent for as long as the host is on the preload list.

- **Every gardenio subdomain MUST stay on HTTPS.** Adding a new
  subdomain that does not speak HTTPS will silently break it for any
  user whose Chrome has consumed the preload list. Add new ingress
  routes with the wildcard cert from day one.
- **`stsSeconds` MUST stay ≥ 31536000.** Lowering it does not remove the
  pin (browsers cache the longer value); raising it does no harm. The
  chart default of 63072000 (2y) is fine.
- **`stsIncludeSubdomains` MUST stay `true`** while the apex is on the
  preload list. Flipping it to `false` does not narrow the pin.
- **`stsPreload` MUST stay `true`** so the directive sent to browsers
  matches what is on the list.

If the chart values drift away from these (e.g. someone disables
`securityHeaders` to debug a CSP issue), the preload pin still
applies — but the next apex re-submission audit (Chrome periodically
re-checks listed hosts and flags non-compliant ones for removal) will
flag the drift and may delist the host. That removal is itself a
months-long process, and during the lag window users cannot reach the
host on HTTP.

## Rollback / removal procedure

Removal is **expensive and slow** — preferable to never need it. Below
is the path operators should know exists.

1. **Stop emitting `preload` from HSTS responses** by setting
   `global.securityHeaders.stsPreload: false` in the chart values and
   redeploying. Browsers that have already cached the previous header
   will continue to respect it for `stsSeconds`, but new hits no longer
   carry the directive.
2. **Submit a removal request** at <https://hstspreload.org/removal/>.
   Enter the apex. The site verifies that the apex no longer emits
   `preload` (so step 1 is a precondition).
3. **Wait.** Removal lands in Chrome Canary in ~1–2 weeks, Chrome stable
   in ~6–12 weeks, and other browsers at their own cadence. Until
   removal reaches a user's installed browser, that user's Chrome
   refuses any HTTP request to the host or any subdomain.
4. **During the wait window, do not break HTTPS.** A subdomain going
   HTTP-only during the removal window is dark for every user whose
   Chrome has not yet consumed the removal.

## Troubleshooting

### `strict-transport-security` header is missing on HTTPS responses

Most likely cause: upstream TLS termination + Traefik does not trust
`X-Forwarded-Proto: https`, so Traefik's `Headers` middleware sees
plain HTTP and skips HSTS. Confirm by:

```sh
# From a cluster admin context
kubectl exec -n traefik <traefik-pod> -- traefik version
# Then look at the entrypoint config — see ingress-trusted-ips.md
```

Fix per [`ingress-trusted-ips.md`](ingress-trusted-ips.md) (set the
cluster-Traefik's `entryPoints.websecure.forwardedHeaders.trustedIPs` to
the LB CIDR), OR move TLS termination to Traefik itself.

A second possible cause: the response is being served by an upstream LB
(e.g. an ALB error page) and never reaches Traefik. Confirm the response
is from Traefik by looking for `content-security-policy` and the chart's
`referrer-policy` value in the same response.

### `https-redirect.yaml` IngressRoute is not in effect

The redirect template is gated on `tls.enabled: true`. If the wrapper
chart leaves `tls.enabled: false` and relies on an upstream layer for
TLS termination + HTTP→HTTPS, the chart's redirect IngressRoute is not
rendered and the upstream layer must answer 308 (or 301/302) on its
own. Verify with `curl -sI http://<host>/`. If the upstream layer
returns 200 / a real response, F3 is not actually shipped on this
deployment despite the chart change merging — do not submit.

### A subdomain returns a TLS error

The wildcard cert is `*.gardenio.<apex>`. A `Common Name does not match`
error on a subdomain typically means the subdomain is being served by a
different ingress that does not use the wildcard cert. Inventory all
IngressRoutes (the inventory script above) and confirm every Host is
covered by the same secret.

## References

- HSTS spec: [RFC 6797](https://datatracker.ietf.org/doc/html/rfc6797)
- Chrome preload site: <https://hstspreload.org/>
- Removal: <https://hstspreload.org/removal/>
- Audit: `docs/audits/ingress-tls-static.md` §F3, §F4, §F9 (not in this repo, see note above)
- Sister runbook (TLS termination / forwarded headers): [`ingress-trusted-ips.md`](ingress-trusted-ips.md)
