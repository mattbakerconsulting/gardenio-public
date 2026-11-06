# CSP `'unsafe-inline'` deferral (audit F6)

**Status:** accepted residual risk — wontfix-by-policy.
**Bead:** ka-0000002937 ("track: SPA refactor to drop CSP unsafe-inline (F6)").
**Decision:** ka-0000006993 (resolved `defer-document`).
**Audit source:** `docs/audits/ingress-tls-static.md` §F6 (LOW) — not in this repo, the audits tree was excluded from the 2026-09-16 export.

## What the audit flagged

The chart's Traefik security-headers middleware sets a single global CSP whose
`script-src` and `style-src` directives include `'unsafe-inline'`. This negates the
XSS-defense value of CSP for the affected hosts.

The middleware is rendered once per release; the same CSP is applied to every gardenio
SPA route. Three SPAs are in scope:

- **beads-viewer** — Go server, `html/template` output. Roughly 20 templates with
  inline `<style>` and `<script>` blocks (notably `templates/layout.html`'s 280-line
  global stylesheet and the htmx bootstrap script). 5 inline `style="..."` attributes
  and 1 inline `onclick=` are also present.
- **beads3d** — Vite SPA served by nginx. `index.html` carries one large
  hand-authored inline `<style>` block (~3 000 lines of UI CSS) plus a single
  external module script.
- **landing** — static `index.html` served from a Helm `ConfigMap` via nginx. One
  inline `<style>` block; no inline JS.

## Why we are not fixing it now

Removing `'unsafe-inline'` for these surfaces is a meaningful refactor, not a
config tweak. It would require:

1. Per-request nonce generation in beads-viewer's Go middleware, plus a `nonce`
   attribute on every `<script>` and `<style>` tag in every template, plus
   refactoring the inline `style="..."` attrs and `onclick=` to classes/`data-*`
   handlers (since `'unsafe-hashes'` is poorly supported in some browsers).
2. A new code path on the nginx side (or a Go shim replacing nginx) for beads3d
   and landing to substitute either a per-request nonce or a build-time SHA-256
   hash into both the served HTML and the response CSP header.
3. Splitting the single Traefik global CSP middleware into per-route CSPs so the
   app-layer CSP is not double-set or contradicted.

We considered four options when triaging the work:

| Option | Cost | Outcome |
|---|---|---|
| Hybrid (nonce in beads-viewer, hash in static SPAs) | ~1 engineer-day | Strict CSP everywhere |
| Nonce everywhere (Go shim replaces nginx for static SPAs) | ~2 engineer-days | Strict CSP, uniform |
| Hash everywhere (build-time CSP regen for beads-viewer too) | ~1 day, high ongoing maintenance | Strict CSP, brittle |
| **Defer + document (chosen)** | minutes | Residual risk accepted |

F6 is rated LOW in the audit because the residual XSS risk is bounded by other
controls in place today (see "Compensating controls" below). The team chose to
spend the engineer-day on higher-rated audit findings (F1 TLSOption, F3 HTTP→HTTPS
redirect, F11 OIDC issuer validation) rather than on F6.

## Compensating controls (still in effect)

The XSS-via-inline-script blast radius is constrained by:

- `frame-ancestors 'none'` on every public route except beads3d (which uses
  `frame-ancestors 'self'` for embedding). Defeats clickjacking-into-XSS pivots.
- `X-Frame-Options: DENY` (`SAMEORIGIN` for the iframe variant) — same defense
  for legacy clients that ignore CSP.
- `X-Content-Type-Options: nosniff` — blocks one common XSS payload-delivery
  vector (script masquerading as image/other).
- `Referrer-Policy: strict-origin-when-cross-origin` — limits token leakage in
  the event of a successful XSS.
- OIDC + oauth2-proxy in front of every authenticated SPA. An attacker who lands
  XSS still needs a valid session cookie to do anything privileged; the cookie
  is `HttpOnly` + `Secure` + `SameSite=Lax` (chart defaults).
- IP allowlists on `beads.ingress` and the bridge webhook routes (when enabled).
- HSTS preload-eligible (`max-age=63072000`, `includeSubDomains`, `preload`)
  blocks downgrade-to-HTTP MITM script injection on subsequent visits.
- `connect-src 'self' wss://*.<apex>` (post-F5 fix) bounds where an XSS-injected
  script can exfiltrate to.
- `'unsafe-eval'` is **only** present in the iframe-variant CSP (beads3d) where
  Three.js shader compilation requires it. The default CSP does not include it.

These controls do not replace nonce/hash CSP — they reduce the cost of leaving
F6 open until we have engineer-time to do the refactor cleanly.

## Re-litigation triggers

Re-open ka-0000002937 and re-run the decision if any of the following change:

- An audit re-rates F6 above LOW (e.g. because a new threat model elevates
  XSS-via-third-party-content).
- We add a new authenticated SPA whose token surface is materially worse than
  today's (e.g. one that holds long-lived cloud creds in `localStorage`).
- We move beads-viewer or beads3d off OIDC and onto an unauthenticated public
  surface — the compensating controls above lean heavily on the OIDC gate.
- `'unsafe-eval'` ever leaks from the iframe variant into the default CSP.
- Browsers begin warning users on pages with `'unsafe-inline'` (no signal of
  this as of 2026-04).

## Where the work would land if/when we pick this back up

- `gardenio/helm/gardenio/templates/security-headers.yaml` — split into per-route
  middlewares; remove `'unsafe-inline'` from the default CSP after the SPAs are
  switched.
- `gardenio/controller/cmd/beads-viewer/middleware.go` — add a `cspNonce` middleware
  that injects a per-request 128-bit base64 nonce into a request-scoped context.
- `gardenio/controller/cmd/beads-viewer/templates/*.html` — add `nonce="{{.Nonce}}"`
  to every `<script>` and `<style>` tag; refactor `style="..."` and `onclick=` to
  class-based / `addEventListener` patterns.
- `beads3d/` — pick one of: (a) move the inline `<style>` block to a separate
  CSS file (cleanest, no CSP gymnastics), or (b) compute its SHA-256 at build
  time and inject the hash into the nginx `add_header Content-Security-Policy`
  via Vite plugin.
- `gardenio/helm/gardenio/templates/landing/configmap.yaml` — same shape as the
  beads3d hash path, or move the `<style>` to an external file under the same
  ConfigMap.
