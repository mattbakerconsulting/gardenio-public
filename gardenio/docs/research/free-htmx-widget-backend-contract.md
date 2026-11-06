# Free-htmx widget — backend contract

> Research deliverable for ka-0000007136 (parent: ka-0000006879).
> Sibling: ka-0000000776 (wire shape & template contract — in progress).
> Author: seer `gardenio-research-freehtmx-backend-l7e`, 2026-05-09.

## TL;DR

A free-htmx widget is just a Go `html/template` body that renders inside the
rich-report page. The "backend contract" is the small, named, audited set of
HTTP endpoints that template body is allowed to point `hx-get` / `hx-post` at,
plus the rules the server enforces around them.

Recommendation in one paragraph:

> Expose a single `/htmx/*` namespace on **beads-viewer** (already where the
> rich-report page lives, already OIDC-gated). Three sub-namespaces:
> `/htmx/bead/{id}/panel/{name}` for read fragments, `/htmx/decision/{id}/...`
> + `/htmx/action/{name}` for writes, `/htmx/api/...` for narrow read-only
> passthroughs to kbeads. **Authoritative auth = OIDC session cookie via
> oauth2-proxy ForwardAuth.** Widgets emit HTML fragments only; cross-widget
> updates use htmx OOB swaps (`hx-swap-oob`) and the `HX-Trigger` response
> header, never JSON. Server pre-injects `bead / parent / siblings /
> correlation_id / payload` into the template scope so widgets don't hx-get
> data they already have. Live refresh generalizes the existing Phase-8
> `/widgets/{slug}/{bead}` pattern: poll cadence is declared on the
> widget-type bead, not by the agent-author, so the operator caps the load.
> No new endpoints in **kbeads** — beads-viewer is the only thing widgets
> talk to. Per-IP token bucket + per-route concurrency cap in beads-viewer
> is **net-new**; rate limiting does not exist there today.

## Why this scope (and what it deliberately excludes)

**In scope:**
- The HTTP surface a free-htmx template body is permitted to reference via
  `hx-*` attributes.
- How those endpoints authenticate, authorize, and rate-limit.
- The response shape (HTML fragment + OOB swap conventions; no JSON).
- How a widget's template scope picks up data already on the rich-report
  bead (so widgets are not forced to round-trip to the server for it).
- Live-refresh patterns, generalized from the Phase-8 mezmo widget.
- The list of net-new beads-viewer routes this contract requires.

**Out of scope (handled by siblings or follow-ups):**
- Template language, `widget-type` bead schema, payload JSON Schema —
  ka-0000000776.
- Sandbox / sanitizer / CSP allowlist for `hx-*` attributes — sibling
  research (sandbox + CSP child of ka-0000006879; closed bead ka-0000000873).
  This doc references the sandbox boundary but does not redesign it.
- Migration of any specific built-in widget to free-htmx — separate child
  of ka-0000006879.
- `gb widget` CLI extensions — separate child.

The headline goal from Matt's direction was "fully leverage gardenio +
beads-viewer + beads-api." This doc takes the position that "leverage"
must not mean "expose the full beads-api surface to agent-authored HTML."
It means: define a tight, named, monitored backend contract that lets
templates do everything the predefined widgets do today, without growing
a kbeads endpoint per widget.

---

## 1. Endpoint surface

### 1.1 Single host, three namespaces

All free-htmx widget HTTP traffic targets **beads-viewer** at the same
origin the rich-report page was served from. No cross-origin calls; the
sandbox CSP (`connect-src 'self'`) rejects anything else by construction.
That means kbeads (`beads-api`) is **never** spoken to directly from a
widget — every "beads-api passthrough" is a viewer route that proxies a
narrow query. This is the single most important boundary in this design:

- Widgets cannot author kbeads writes. Period.
- Widgets cannot stream kbeads events.
- Widgets cannot read kbeads resources beads-viewer hasn't already vetted
  (e.g. fleet tokens, embeddings, raw audit logs).

The viewer is the trust boundary; kbeads stays on the safe side of it.

| Namespace                                | Verb(s) | Purpose                                                                                          |
| ---------------------------------------- | ------- | ------------------------------------------------------------------------------------------------ |
| `/htmx/bead/{id}/panel/{name}`           | `GET`   | Read-only HTML fragment for a named panel of a specific bead.                                    |
| `/htmx/bead/{id}/panel/{name}/live`      | `GET`   | Same, plus live-refresh polling envelope (cache headers + `HX-Trigger` push for cross-widget).   |
| `/htmx/decision/{id}/option/{opt}`       | `POST`  | Resolve an option on a decision bead. Operator-gated.                                            |
| `/htmx/action/{name}`                    | `POST`  | Server-side action handler (rate, claim, follow, dismiss, …). Each `name` is registered server-side; agents cannot invent new actions from the template. |
| `/htmx/api/list`                         | `GET`   | Narrow read of the bead list (filter by type/label/status/parent), returns rendered HTML rows.   |
| `/htmx/api/bead/{id}`                    | `GET`   | Narrow read of a single bead, returns the bead's preview-card HTML.                              |
| `/htmx/api/mezmo`                        | `GET`   | Pre-vetted Mezmo passthrough (histogram / group / dedup), returns rendered chart fragment.       |

Everything else is forbidden by the URL allowlist in the sandbox (CSP
`connect-src 'self' /htmx/*; form-action 'self' /htmx/*`). The sandbox
research will spell out the exact attribute syntax that enforces this; for
this doc the contract is: a free-htmx template **may only point hx-* at
URLs starting `/htmx/`**.

### 1.2 Where these live in code

| Endpoint                                     | Owner file                                              | Net-new?                                |
| -------------------------------------------- | ------------------------------------------------------- | --------------------------------------- |
| `/htmx/bead/{id}/panel/{name}`               | `gardenio/controller/cmd/beads-viewer/htmx_panels.go`    | **Net-new.** Wraps existing internal renderers (`bead_activity.go`, `bead_preview.go`, `bead_ownership.go`) under one allowlist-routed dispatcher. |
| `/htmx/bead/{id}/panel/{name}/live`          | same                                                    | **Net-new.** Generalizes `widgets_live.go` (`/widgets/{slug}/{bead}`); see §6. |
| `/htmx/decision/{id}/option/{opt}`           | `gardenio/controller/cmd/beads-viewer/htmx_decisions.go` | **Net-new wrapper.** Backed by existing `handleResolveDecision` (`decisions.go`). Difference: dispatch by option id in the URL, no form parsing. |
| `/htmx/action/{name}`                        | `gardenio/controller/cmd/beads-viewer/htmx_actions.go`   | **Net-new dispatcher** with a server-side action registry (`map[string]ActionFunc`). Initial registry: `rate`, `claim`, `release`, `inbox-toggle`, `follow`, `dismiss-decision`. |
| `/htmx/api/list`                             | `gardenio/controller/cmd/beads-viewer/htmx_api.go`       | **Net-new.** Calls `daemon.ListBeads` with a filter allowlist; renders `tbody` rows via the existing project-rows partial. |
| `/htmx/api/bead/{id}`                        | same                                                    | **Net-new.** Calls `daemon.GetBead`; renders the existing bead-preview partial (already used by `/b/{id}/preview`). |
| `/htmx/api/mezmo`                            | `gardenio/controller/cmd/beads-viewer/htmx_mezmo.go`     | **Mostly existing.** `widgets_mezmo*.go` already does the splicing today; this route exposes it under the unified `/htmx/api/*` namespace. |

The server-side dispatcher pattern keeps the contract documented as code:
new actions land via MR review (same gate as Phase-3 `/actions/{id}` today),
not via agent template authorship.

### 1.3 What this replaces

Today's per-widget routes (`/widgets/{slug}/{bead}`, `/b/{id}/decision/...`,
`/actions/{id}`, the inline editors at `/b/{id}/title`, etc.) keep working
unchanged for the bead-show page. The `/htmx/*` namespace is **the only
surface a free-htmx widget template is permitted to reference**; existing
routes continue to back the bead-show inline editors and the legacy
predefined widgets. We do not break old routes; we add a curated front
door for the free template path.

---

## 2. Authentication

### 2.1 Default: OIDC session cookie via oauth2-proxy ForwardAuth

The rich-report page is served by beads-viewer behind oauth2-proxy. Every
request reaching the viewer carries `X-Auth-Request-Access-Token` (set by
ForwardAuth) and `X-Auth-Request-Email` headers; `oidcauth.Middleware`
already verifies the access token, parses claims, and stamps the request
context with email + realm roles
(`gardenio/controller/internal/oidcauth/oidcauth.go`).

A free-htmx widget rendered inside the rich-report page **inherits this
session for free**. Every `hx-get` / `hx-post` it issues is a same-origin
fetch from the browser, so the OIDC cookie + ForwardAuth header round-trip
unchanged. There is no widget-level auth to design for the browser case.

The full set of routes under `/htmx/*` is wrapped in the existing OIDC
middleware:

- Reads (`GET`) require `RoleViewer` (which is "any authenticated user"
  in the realm role rank).
- Writes (`POST`) under `/htmx/decision/...` and `/htmx/action/...`
  require `RoleOperator` via `oidcauth.RequireRole`, the same gate that
  protects `/actions/{id}` today. Viewer-role users get 403 inline; htmx
  swaps in a small "operator-only" chip on the response.

### 2.2 Non-browser callers (iOS, CLI, server-side tooling)

The task brief asks "how do we authenticate htmx-from-iOS-or-CLI?" The
short answer: **we do not, in v1.** The free-htmx widget is rendered
inside the rich-report page, and that page is gated by OIDC. The only
realistic v1 caller of `/htmx/*` is a browser session that already has an
OIDC cookie.

For when this becomes a real requirement (probably with iOS):

**Option A — propagate the same access token.** The iOS/CLI client takes
the OIDC access token it already uses for kbeads (`Authorization: Bearer
…`) and sends it on `/htmx/*`. `oidcauth.Middleware` already accepts
`Authorization: Bearer` as a fallback. **No code change required**; this
"just works" today for any caller that has a valid token. The bottleneck
is that iOS clients need to be inside the OIDC realm — which they already
are if they talk to kbeads.

**Option B — signed widget-instance token (deferred).** Mint a short-lived
HMAC token (`widget_instance_id + bead_id + viewer_email + exp` signed with
a viewer-side secret) and embed it in the rendered widget HTML as an
`hx-headers='{"X-Widget-Token": "…"}'` attribute. The viewer accepts the
token in lieu of OIDC for `/htmx/*` calls scoped to the same instance.

We **do not recommend Option B for v1** because:
1. It introduces a second auth path with its own revocation story (token
   leaked → operator must wait for TTL or rotate the signing key).
2. Every `/htmx/*` write would need scope-binding logic (this token is
   only valid for *this* widget instance on *this* bead). That scope
   check is non-trivial and reviewer-intensive.
3. It doesn't actually solve the "iOS user without an OIDC session" case
   — the viewer needs to mint the token from *some* session, so a
   session must already exist somewhere upstream.

If iOS deeply needs this in the future, the right move is Option B but
gated behind a decision bead with concrete threat model + revocation
plan. For now: **OIDC access token only, no exceptions.** Document this
as the contract; revisit when there's a real client.

### 2.3 CSRF

`/htmx/*` writes are POST-only and same-origin. The OIDC access token is
not a cookie (it lives in the `X-Auth-Request-*` headers ForwardAuth
injects), so classic CSRF does not apply to the auth boundary itself.
Defense in depth: every write handler validates that `Origin` (when
present) matches the viewer's host, rejecting otherwise. This is one
shared middleware across all `/htmx/*` POST handlers; we already do this
implicitly via ForwardAuth but should be explicit at the application
layer.

---

## 3. Response shape

### 3.1 HTML fragments, never JSON

Every `/htmx/*` route returns a complete, server-rendered HTML fragment
ready for htmx to swap into the DOM. **No JSON.** This is the single most
important shape decision in the contract:

- The free-htmx widget template is the *only* place HTML rendering
  happens client-side. If endpoints returned JSON, the template would
  need a JSON-rendering helper, which would either pull in a JS
  templating library (out of scope; the widget is supposed to be
  htmx-shaped) or rebuild client-side rendering inside the agent's
  template body (which is the bug we are trying to avoid).
- HTML fragments mean the sandbox/sanitizer applies *once*, at template
  compile time — what comes back from `/htmx/*` is already fully escaped
  by the server. JSON-from-server-then-template-on-client doubles the
  sanitization surface and is a known footgun.
- HTML fragments are bit-identical with the initial server-side render
  of the rich-report page. The Phase-8 `/widgets/{slug}/{bead}` route
  already proves this: it dispatches through the same widget registry
  the rich-report page uses (`widgets_live.go:64`), so the swap-in
  fragment renders identically to the first paint.

### 3.2 Out-of-band swaps for cross-widget updates

When a single htmx request needs to mutate more than one place on the
page (e.g. resolving a decision should both replace the decision card
*and* update the activity-log widget below), the response uses
`hx-swap-oob` markers. Concrete pattern:

```html
<!-- primary swap target: replaces the decision card -->
<div id="decision-{{.ID}}">…resolved card…</div>

<!-- OOB swap: appends a row to the activity log widget -->
<tbody id="activity-rows" hx-swap-oob="afterbegin">
  <tr>… new entry …</tr>
</tbody>

<!-- OOB swap: clears any stale error chip -->
<div id="widget-error-chip" hx-swap-oob="true"></div>
```

We already use this pattern for project bulk edits (`project_bulk.go`
renders the cleared bulk bar plus an OOB row partial per updated bead).
Generalize it for widgets: any `/htmx/action/*` handler that should
visibly update other widgets on the same page emits OOB partials for
the target IDs.

The free-htmx template is responsible for declaring stable target IDs
(e.g. `id="widget-{instance_id}-rows"`) so OOB swaps can find them. The
sibling wire-shape doc (ka-0000000776) should standardize the ID format.
**Recommended convention:** `widget-{instance_id}-{slot}`, where `slot`
is a free-form name the widget author picks. Two widgets on the same
report cannot collide because `instance_id` is unique per widget bead.

### 3.3 `HX-Trigger` for fan-out events

For cross-widget signals where OOB-swapping every consumer is heavy
(e.g. "the cross-filter changed; every widget on this page should
re-poll with the new filter"), use the `HX-Trigger` response header.
Receiving widgets register `hx-trigger="cross-filter-changed from:body"`
on their poll element; one server response triggers a fan-out re-fetch
across widgets without the originating handler needing to know who is
listening. This is the same pattern the existing Phase-8 cross-filter
already encodes via JS (`liveAttrsFor` in `widgets_live.go:153`); we
formalize it as a documented header free-htmx widgets can rely on.

The viewer publishes a small allowlist of trigger names: `bead-changed`,
`decision-resolved`, `cross-filter-changed`, `rating-submitted`. Agents
do not author new trigger names from templates; they can only listen
for these.

### 3.4 Errors

Errors render as a small inline HTML chip (the same shape the live route
uses today: `<div class="ii-w-live-error" data-state="error">…</div>`),
**with the appropriate 4xx/5xx status code**. htmx surfaces the status
to the user (toast, ARIA live region) while still swapping the chip in.
This is the existing pattern from `decisions.go` (`renderDecisionError`
preserves the operator's draft response on 4xx) — generalize to all
`/htmx/*` writes.

---

## 4. Rate limits and cost ceilings

### 4.1 Today: nothing

beads-viewer has **no HTTP rate limiting** (verified by ripgrep:
`x/time/rate` and friends are not imported anywhere in
`gardenio/controller/cmd/beads-viewer/`). A naive or malicious free-htmx
widget could trivially `hx-get` on a 100ms `every` trigger and torch the
viewer. This is the largest deployment risk in the whole epic.

### 4.2 Net-new: per-IP token bucket + per-route concurrency cap

Add `gardenio/controller/internal/ratelimit/` (or pull from `x/time/rate`)
and wire as middleware on the `/htmx/*` mux:

| Tier                             | Burst | Steady-state | Rationale                                                                 |
| -------------------------------- | ----- | ------------ | ------------------------------------------------------------------------- |
| `/htmx/bead/*` reads             | 60    | 30 / 10s     | Polling widgets at 5s cadence, ≤6 widgets per page = 6 req per 5s = 1.2/s. 30 / 10s leaves comfortable headroom for an active operator clicking around. |
| `/htmx/api/*` reads              | 20    | 10 / 10s     | Lower because `list` queries fan out to kbeads; cap blast radius.         |
| `/htmx/decision/*` writes        | 10    | 5 / 10s      | Operator-gated; humans don't click that fast.                             |
| `/htmx/action/*` writes          | 20    | 10 / 10s     | Higher because `rate`, `follow`, etc. are debounceable.                   |
| `/htmx/api/mezmo`                | 10    | 5 / 10s      | Mezmo MCP is the most expensive backend; cap aggressively.                |

Bucket key: `(remote_ip, oidc_email, route_class)`. Per-IP keeps a
single misbehaving session from saturating the viewer; per-email keeps a
shared NAT'd network from punishing one user; per-route-class lets us
tune mezmo independently from cheap reads. 429 responses render the
same error chip (`<div class="ii-w-live-error">rate limited</div>`) so
htmx surfaces it without breaking layout; the next poll cycle naturally
recovers.

**Per-route concurrency cap.** A second middleware caps concurrent
in-flight requests per route class with a semaphore (e.g. 50 concurrent
`/htmx/api/list` requests viewer-wide). Excess is rejected with 503 +
`Retry-After`. This bounds tail latency when one widget pattern goes
viral.

### 4.3 Server-side cost ceilings

Beyond request rate, cap the *cost* of any single widget render:

- **Total fragment size**: 256 KiB hard cap per response. Anything bigger
  is a bug or an attack; truncate + log + return a small "fragment too
  large" chip.
- **Server-side render budget**: 4-second context timeout per request
  (already the convention in `widgets_live.go:78`); applies viewer-wide.
- **kbeads call budget**: at most 3 daemon calls per `/htmx/*` request.
  `daemon.ListBeads` + `daemon.GetBead` + one parent fetch = 3. Any
  widget that needs more should not be a single fragment; split it.
  Enforce by counting calls in a request-scoped context and erroring if
  exceeded.
- **Mezmo call budget**: at most 1 MCP call per `/htmx/api/mezmo`
  request. The existing client already has a 10s timeout
  (`widgets_mezmo_http.go:52`); add a "1 call only" guard.

These are MR-reviewable defaults, not agent-tunable. An agent template
that wants more either splits its widget into parts or escalates a
decision to raise the cap for the whole class.

### 4.4 Live-refresh defaults

The agent template **cannot** declare its own poll cadence. The cadence
is on the `widget-type` bead (`live_interval_secs` field, default 30s,
allowed range 5..300s; matches `widgets_live.go:liveWidgetPolicies`).
Operator review of the type bead caps the cadence; agent template
controls only what is in the swap target. This is the same bargain
Phase-8 already strikes: pollable widgets are an operator-curated
allowlist, not a free-for-all.

If the cadence must vary per instance (e.g. archived vs. active mode
for mezmo widgets, see `mezmoLiveIntervalSecs`), that's a constrained
multi-mode field on the type bead, not free-form on the instance.

---

## 5. Template scope: what data the widget already has

The brief asks: *how does a free-htmx widget reference data the
rich-report bead has "in scope" — server-side context-injection vs.
widget pulling via hx-get?*

**Recommendation: inject by default, hx-get for fresh / privileged data.**

### 5.1 Pre-injected scope (no HTTP round-trip needed)

When beads-viewer renders the rich-report page, every free-htmx widget
instance gets a pre-built scope dict. The template body sees:

| Variable         | Type                          | Source                                                       |
| ---------------- | ----------------------------- | ------------------------------------------------------------ |
| `.Bead`          | bead summary (id, title, type, status, labels, fields) | The rich-report bead the widget is rendered on. |
| `.Parent`        | bead summary or nil           | Walk the report bead's `parent` mention.                     |
| `.Siblings`      | `[]beadSummary`               | Other widget-instance + non-widget children of `.Bead`.      |
| `.Widget`        | bead summary                  | The widget-instance bead itself.                             |
| `.Payload`       | template-validated map        | The `data` field on `.Widget`, JSON-Schema-validated.        |
| `.CorrelationID` | string                        | `.Bead.Fields.correlation_id` if present, else `.Widget.Fields.correlation_id`. |
| `.Viewer`        | `{Email, Roles, IsOperator}`  | OIDC context.                                                |
| `.URLs`          | URL helpers (see below)       | Template helpers, not raw data.                              |

This means the *common* widget renders without making any HTTP request:
title, status, payload fields, bead-id links — all already in scope.
Round-tripping to the server for parent/sibling metadata would just be
a same-data re-fetch with extra latency.

### 5.2 URL helpers (the only sanctioned way to author hx-* targets)

The template scope ships URL helper funcs the widget body uses to
construct `hx-get` / `hx-post` targets. **The agent author does not
write `/htmx/...` paths by hand.** This is the lever that lets the
sanitizer enforce "every `hx-*` target starts with `/htmx/` and resolves
to a registered route" at compile time:

```
{{ panelURL .Bead "comments" }}
  → /htmx/bead/ka-XYZ/panel/comments

{{ panelURL .Parent "activity" "live=1" }}
  → /htmx/bead/ka-PARENT/panel/activity/live

{{ actionURL "rate" .Bead }}
  → /htmx/action/rate?bead=ka-XYZ

{{ decisionOptionURL .Bead "approve" }}
  → /htmx/decision/ka-XYZ/option/approve

{{ apiURL "list" "type=task" "parent=" .Bead.ID }}
  → /htmx/api/list?type=task&parent=ka-XYZ

{{ apiURL "mezmo" "kind=histogram" "query=app:kbeads" }}
  → /htmx/api/mezmo?kind=histogram&query=app%3Akbeads

{{ liveAttrs .Widget }}
  → hx-get="…/live" hx-trigger="every 30s" hx-swap="innerHTML"
```

The helpers reject unknown panel/action/decision-option/api names at
template compile time. An agent typo (`{{ actionURL "rat" .Bead }}`)
fails the sandbox; we never serve a template that references a route
that does not exist. The existing `widgets_live.go:liveAttrs` is the
direct ancestor of `liveAttrs` here; we just generalize.

This list is the **whole** allowlist of action/panel/api names a
template can reference. Adding a new action = MR review of the
server-side handler + adding a name to the helper allowlist. Same gate
we use for Phase-3 actions today.

### 5.3 When to hx-get instead of relying on injected scope

Inject by default; reach for hx-get when:
1. **The data must be live.** Mezmo charts, agent screen, decision card
   after a resolution.
2. **The data is privileged** (e.g. operator-only descendants of a
   private bead). Server-side injection would lift privilege; an hx-get
   is re-authorized per request.
3. **The data is large and rarely viewed.** Only render the comment
   feed when the operator clicks "show comments" — `hx-trigger="click"`
   on a placeholder.

---

## 6. Live refresh patterns

The Phase-8 mezmo widget proves the pattern works. We generalize it
without breaking the existing route.

### 6.1 Generalized polling envelope

```html
<div id="widget-{{.Widget.ID}}-body"
     {{ liveAttrs .Widget }}>
  …rendered fragment…
</div>
```

`liveAttrs` reads the widget-type bead's `live_interval_secs` field and
returns:

```html
hx-get="/htmx/bead/{{.Widget.ID}}/panel/body/live"
hx-trigger="every Ns [document.visibilityState=='visible']"
hx-swap="innerHTML"
hx-vals='js:{filter: window.IncidentIntel?.crossFilter?.serialize() ?? ""}'
```

The `[document.visibilityState=='visible']` guard suppresses polling on
hidden tabs (consistent with `bead_activity_test.go:204`). This drops
viewer load from background tabs by ~1 OOM in practice.

### 6.2 Cross-filter forwarding

`hx-vals='js:{filter: …}'` already forwards the active cross-filter to
the server. The handler decides whether to apply the filter
(`policy.ServerFilters`); for filters that are purely client-side, the
URL param is a harmless no-op. This is the existing
`widgets_live.go:applyServerCrossFilter` path — generalize as **any
free-htmx widget that opts in to live mode automatically forwards
cross-filter**.

### 6.3 Push instead of poll

Where push is feasible, use the existing `/v1/events/stream` SSE channel
on kbeads — but **mediated by the viewer**. A free-htmx widget cannot
open an SSE itself (SSE bypasses the rate limiter and the trust
boundary). Instead, the viewer fans out events from kbeads SSE to a
shared per-page `EventSource` mounted in the rich-report shell, and
widgets pick up events via `hx-trigger="bead-changed from:body"`.

This is a **future refinement**, not v1. Polling is the v1 pattern;
push is a ka-XYZ follow-up if poll volume becomes a real cost.

### 6.4 Live-mode cadence allowlist

The widget-type bead carries `live_interval_secs`. Allowed values:

| Mode      | Interval | Use case                                         |
| --------- | -------- | ------------------------------------------------ |
| off       | 0        | Static fragment; no polling.                     |
| fast      | 5s       | Active incidents (current Phase-8 default).      |
| medium    | 30s      | Default for new widgets; cheap.                  |
| slow      | 300s     | Archived / historical reports.                   |

Agent author picks a label; viewer maps to seconds. We do not let
agents author 1-second polling.

---

## 7. Worked examples

### 7.1 Reproduction-steps free-htmx widget (the migration prototype)

Today: `widgets_phase4.go` registers a built-in `reproduction-steps`
renderer; agents emit instances against bug rich-report beads. As a
free-htmx widget, the same effect:

```html
{{ define "reproduction-steps" }}
<section class="w-repro" id="widget-{{.Widget.ID}}">
  <h3 class="w-repro-title">Reproduction steps</h3>
  <ol class="w-repro-list">
    {{ range $i, $step := .Payload.steps }}
      <li class="w-repro-step" data-idx="{{$i}}">
        {{ $step.text }}
        {{ if $step.bead_ref }}
          <a href="{{ beadURL $step.bead_ref }}"
             hx-get="{{ panelURL $step.bead_ref "preview" }}"
             hx-trigger="mouseover once"
             hx-target="#preview-{{$i}}">
            {{ $step.bead_ref }}
          </a>
          <div id="preview-{{$i}}"></div>
        {{ end }}
      </li>
    {{ end }}
  </ol>

  {{ if .Viewer.IsOperator }}
    <button hx-post="{{ actionURL "rate" .Bead }}?score=helpful"
            hx-target="#widget-{{.Widget.ID}}-rating"
            hx-swap="outerHTML">
      Helpful
    </button>
    <span id="widget-{{.Widget.ID}}-rating"></span>
  {{ end }}
</section>
{{ end }}
```

Notes:
- All `hx-*` targets are constructed via helpers; the sandbox enforces
  this.
- The "Helpful" button is gated by `.Viewer.IsOperator` server-side; if
  a viewer-role user somehow renders the button anyway, the
  `/htmx/action/rate` route returns 403 and htmx surfaces it.
- No JSON, no client-side templating — the on-hover preview is a
  fragment from `/htmx/bead/.../panel/preview`.

### 7.2 Live mezmo histogram (existing Phase-8 widget, retargeted)

The existing widget already polls `/widgets/mezmo-histogram/{bead}` at
30s. Migrating to free-htmx:

```html
{{ define "mezmo-histogram" }}
<figure id="widget-{{.Widget.ID}}-body"
        {{ liveAttrs .Widget }}>
  {{ if .Payload.histogram }}
    {{ template "histogram-svg" .Payload.histogram }}
  {{ else }}
    <span class="w-empty">No data in window.</span>
  {{ end }}
</figure>
{{ end }}
```

The viewer-side widget-type bead carries `live_interval_secs=30` and
points the live-route handler at the existing `widgets_mezmo_http.go`
splice path (it's already wired for polled mezmo data; this is just a
naming change). Net new routes: zero. The server-side fragment is
bit-identical.

### 7.3 Decision-option dispatch (Phase-3 action, generalized)

A gardener emits a free-htmx widget on a decision bead's rich-report
that renders the option list as buttons:

```html
{{ define "decision-options" }}
<form id="widget-{{.Widget.ID}}-form">
  {{ range .Payload.options }}
    <button hx-post="{{ decisionOptionURL $.Bead .id }}"
            hx-target="#widget-{{$.Widget.ID}}-form"
            hx-swap="outerHTML"
            {{ if not $.Viewer.IsOperator }}disabled{{ end }}>
      {{ .label }}
    </button>
  {{ end }}
</form>
{{ end }}
```

Server-side, `/htmx/decision/{id}/option/{opt}` calls
`daemon.ResolveDecision` (existing path) and re-renders the resolved
fragment. The form swaps; if the decision bead is on the same page as
an activity-log widget, the response includes an OOB partial that
prepends the new event. Cross-widget feel; one HTTP request.

---

## 8. Net-new beads-viewer routes (the punch list)

In dependency order so an implementation MR can land them in slices:

1. **`/htmx/bead/{id}/panel/{name}` (read)** — `htmx_panels.go`. Wraps
   existing `bead_activity.go`, `bead_preview.go`, `bead_ownership.go`,
   `bead_actions.go`. Allowlisted panel names: `preview`, `activity`,
   `comments`, `deps`, `ownership`, `rating`, `body`. **Required for v1.**
2. **`/htmx/bead/{id}/panel/{name}/live` (read, polled)** — same file.
   Generalizes `widgets_live.go`. **Required for v1.**
3. **`/htmx/decision/{id}/option/{opt}` (write)** — `htmx_decisions.go`.
   Wrapper over `decisions.go:handleResolveDecision`; URL path replaces
   form parsing. Operator-gated. **Required for v1.**
4. **`/htmx/action/{name}` (write)** — `htmx_actions.go`. Action
   registry seeded with `rate`, `claim`, `release`, `inbox-toggle`,
   `follow`, `dismiss-decision`. New actions land via MR. Operator-gated
   except `rate` (which has its own per-bead viewer-allowed handler;
   match the existing `/b/{id}/rating` semantics). **Required for v1.**
5. **`/htmx/api/list` (read)** — `htmx_api.go`. Filter allowlist:
   `type`, `status`, `parent`, `label`, `assignee`, `limit≤25`. Renders
   the existing project-rows partial. **Required for v1.**
6. **`/htmx/api/bead/{id}` (read)** — same file. Renders existing
   bead-preview partial. **Required for v1.**
7. **`/htmx/api/mezmo` (read)** — `htmx_mezmo.go`. Pre-vetted Mezmo
   passthrough (histogram / group / dedup); `query` param goes through
   existing `applyMezmoSource`. **Required for v1.**
8. **Rate-limit middleware** — `internal/ratelimit/` + wiring on the
   `/htmx/*` mux. **Required for v1.** Bigger blast radius than any
   single endpoint above.
9. **CSRF Origin check middleware** — same mux. Defense in depth
   against future caller surprises. **Required for v1.**
10. **URL helper template funcs** — `htmx_helpers.go`. `panelURL`,
    `actionURL`, `decisionOptionURL`, `apiURL`, `liveAttrs`, `beadURL`.
    These get pulled into the free-htmx template execution context the
    sibling ka-0000000776 doc designs. **Required for v1.**
11. **Per-page event fan-out (SSE → htmx triggers)** — `htmx_events.go`.
    **Deferred.** Polling is fine for v1; add when load demands.
12. **Signed widget-instance token (Option B)** — **Deferred.** Add
    when iOS/CLI need it. Don't pre-build.

The v1 scope is items 1-10; items 11-12 are post-v1 follow-ups.

---

## 9. Security boundary summary

| Threat                                              | Defense                                                                 |
| --------------------------------------------------- | ----------------------------------------------------------------------- |
| Widget XSS via payload fields                       | Go template autoescape; every payload value is `string`, never `template.HTML`. |
| Widget exfiltration via `hx-get` to attacker domain | CSP `connect-src 'self'` + sandbox URL allowlist. Helpers refuse to mint cross-origin URLs. |
| Widget privilege escalation (bypass operator gate)  | Server-side `RequireRole` on every `/htmx/*` write; agent cannot affect this from the template. |
| Widget DoS via tight polling                        | Cadence locked on widget-type bead (operator MR); rate limiter on `/htmx/*`. |
| Widget DoS via expensive backend calls              | Per-request kbeads-call budget (3); per-request Mezmo budget (1); 4s timeout. |
| Widget reveals data in the wrong scope              | Template scope is pre-built server-side; widgets cannot pull arbitrary kbeads data, only what `/htmx/api/*` exposes. |
| Stale fragments served from caches                  | `Cache-Control: no-store` on every `/htmx/*` response (existing pattern). |
| Open-redirect via `beadURL` of attacker bead-id     | `beadURL` only emits internal `/kd-…` paths; bead ids are alphanumeric so no encoded scheme injection. |
| CSRF on `/htmx/*` writes                            | OIDC access token in `X-Auth-Request-*` headers; Origin check defense-in-depth. |

Sandbox / CSP for the *template body* itself (no `<script>`, attribute
allowlist, `hx-*` allowlist, class-name namespacing) is the sibling
sandbox research's domain. This doc just establishes the contract the
sandbox has to enforce: **all `hx-*` URLs start with `/htmx/`**.

---

## 10. Coordination with sibling research

- **Wire shape (ka-0000000776, in progress):** the helper allowlist
  (`panelURL`, `actionURL`, `apiURL`, `decisionOptionURL`, `liveAttrs`,
  `beadURL`) lives in this doc; the sibling should adopt these names
  in its template-helper allowlist or coordinate before diverging. I've
  sent a coordination DM to the sibling agent on 2026-05-09.
- **Sandbox + CSP (closed bead ka-0000000873, find linked deliverable):**
  the contract this doc relies on — `connect-src 'self'`, `hx-*`
  attribute allowlist enforces "URLs must start with `/htmx/`",
  `hx-delete` forbidden, `hx-on` forbidden — must be in that doc.
- **Migration plan:** §7.1 sketches the `reproduction-steps` migration
  but the full plan belongs in the migration child of ka-0000006879.
- **`gb widget` CLI:** `gb widget validate` should statically check that
  every `hx-*` target in a template body resolves through one of the
  helpers above. Belongs in the dogfooding/CLI child.

---

## 11. Open questions for design synthesis

1. **Action-name globbing.** Should `/htmx/action/rate?score=helpful` be
   a single action with a `score` param, or two actions
   (`rate-helpful`, `rate-unhelpful`)? Recommend params; matches
   existing `/b/{id}/rating` shape.
2. **Per-instance live-cadence override.** §6.4 forbids it. If the
   mezmo widget's active/archived mode logic generalizes to other
   widgets, we'll need a multi-mode field on the type bead, not free
   per-instance. Defer until a second consumer asks.
3. **`/htmx/api/list` filter expressivity.** The allowlist of
   `type/status/parent/label/assignee` is intentionally narrow. Free
   text search is **not** in v1; `kd search` semantics (semantic
   search, embeddings) are operator-only and not part of the widget
   surface.
4. **Push vs. poll.** Polling is v1; SSE fan-out is a follow-up. Keep
   the contract poll-shaped so we don't tie widgets to an event
   transport that may change.
5. **Mezmo cost accounting.** A single `/htmx/api/mezmo` request can
   drive a Mezmo MCP query that costs real money. We currently rely on
   the per-request budget (1 call) and the rate limiter (5/10s). If
   that proves too generous, add a viewer-side global token bucket on
   total Mezmo MCP calls per minute.

---

## 12. Implementation budget (rough)

Pre-implementation guesses; the implementation epic owners will refine.

| Slice                                                        | Eng-days |
| ------------------------------------------------------------ | -------- |
| `/htmx/*` mux + middleware (rate-limit, CSRF, cache headers) | 2        |
| `/htmx/bead/{id}/panel/{name}` (+ live)                      | 2        |
| `/htmx/decision/{id}/option/{opt}`                           | 1        |
| `/htmx/action/{name}` dispatcher + initial registry          | 2        |
| `/htmx/api/list` + `/htmx/api/bead/{id}`                     | 2        |
| `/htmx/api/mezmo` (lift existing splice into route)          | 1        |
| URL helper funcs + tests                                     | 1        |
| Docs + changelog                                             | 0.5      |
| Sandbox CSP wiring (lives with sibling)                      | (out)    |
| **Total**                                                    | **~11.5 eng-days** |

This is a one-MR slice with clean diffability; every sub-route is a
small handler + test backed by an existing internal renderer.

---

## Appendix A — endpoint summary table

| Method | Path                                            | Auth          | Purpose                                                        | Net-new? |
| ------ | ----------------------------------------------- | ------------- | -------------------------------------------------------------- | -------- |
| GET    | `/htmx/bead/{id}/panel/{name}`                  | viewer        | Read-only HTML panel for a bead                                | yes      |
| GET    | `/htmx/bead/{id}/panel/{name}/live`             | viewer        | Same, polling-shaped + cache headers                           | yes      |
| POST   | `/htmx/decision/{id}/option/{opt}`              | operator      | Resolve a decision option                                      | yes      |
| POST   | `/htmx/action/{name}`                           | viewer/op*    | Server-registered action handler                               | yes      |
| GET    | `/htmx/api/list`                                | viewer        | Bead list (allowlisted filters → HTML rows)                    | yes      |
| GET    | `/htmx/api/bead/{id}`                           | viewer        | Bead preview card                                              | yes      |
| GET    | `/htmx/api/mezmo`                               | viewer        | Pre-vetted Mezmo passthrough → rendered chart                  | yes      |

\*per-action; e.g. `rate` is viewer; `claim` is operator.

## Appendix B — file map (where the implementation lands)

```text
gardenio/controller/cmd/beads-viewer/
├── htmx_panels.go        (NEW) — /htmx/bead/{id}/panel/{name}[/live]
├── htmx_decisions.go     (NEW) — /htmx/decision/{id}/option/{opt}
├── htmx_actions.go       (NEW) — /htmx/action/{name} + action registry
├── htmx_api.go           (NEW) — /htmx/api/list, /htmx/api/bead/{id}
├── htmx_mezmo.go         (NEW) — /htmx/api/mezmo (lifts widgets_mezmo*.go splice)
├── htmx_helpers.go       (NEW) — panelURL/actionURL/apiURL/decisionOptionURL/liveAttrs/beadURL template funcs
├── htmx_middleware.go    (NEW) — rate-limit + CSRF Origin check + cache headers
└── server.go             (EDIT) — wire the new mux subtree under /htmx/*

gardenio/controller/internal/ratelimit/
└── (NEW) per-IP / per-email / per-route token bucket
```

End of doc.
