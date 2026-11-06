# ADR — beads-viewer agent terminal: browser ↔ coopmux path + auth

- **Bead:** [ka-0000006916](https://beads.gardenio.e2e.example.com/ka-0000006916)
- **Parent epic:** [ka-0000003802](https://beads.gardenio.e2e.example.com/ka-0000003802) — beads-viewer iOS-parity Phase 3 (agent-terminal custom view)
- **Status:** Proposed (research-only; no implementation until human green-lights via `gb decision`)
- **Scope:** Architectural decision for *how the browser reaches coopmux* when rendering the agent-detail terminal in `gardenio/controller/cmd/beads-viewer/`. Excludes: renderer choice (R2), send-commands UX (R3), event interleaving (R4), iOS parity (R5).

## TL;DR

> **Recommendation: Hybrid.** The browser reaches coopmux *directly* over its
> existing public IngressRoute (`coopmux.<host>`) for the WebSocket PTY stream
> (`/ws/{session_id}`), while htmx-driven control-plane operations
> (`/api/v1/sessions/{id}/screen`, `/input`, `/agent/nudge`, `/agent/respond`,
> `/transcripts/...`) flow through `beads-viewer`'s Go server, which proxies to
> the coopmux ClusterIP using the static `COOP_MUX_AUTH_TOKEN`. Auth is
> identical on both legs because both ingresses already sit behind the same
> oauth2-proxy ForwardAuth + Keycloak realm, so the browser never sees a
> static token.

The trade-off: two transports to maintain (WS + HTTP-proxy), and a small
amount of new wiring on the beads-viewer pod (mount `COOP_MUX_AUTH_TOKEN`
secret, new env: `COOPMUX_URL`). In return we get htmx-native fragment swaps
for the audit-sensitive control plane and zero-copy PTY fan-out for the
high-volume data plane, without inventing new infra or modifying coopmux's
HTTP surface.

## Context

### Goal

Phase 3 turns the static `templates/partials/type_agent.html` field-list into
a live terminal: the user opens `/agents/{id}` and sees streaming PTY output
from the agent's coopmux session, with the ability to send keystrokes, nudges,
and responses. Today the only "live" agent surface in beads-viewer is
`/agents/{id}/prime`, which is *not* a coopmux integration — `prime.go` shells
out to `gb prime --for <id>` and renders its markdown output. There is no
existing browser → coopmux path in beads-viewer; this ADR defines it.

### What already exists

**coopmux** (`coop/crates/mux/src/transport/mod.rs`) already exposes the full
shape of endpoints we need:

| Surface | Verb / Path | Notes |
|---|---|---|
| Cached snapshot | `GET /api/v1/sessions/{id}/screen` | rendered text + ANSI screen |
| Status | `GET /api/v1/sessions/{id}/status` | session metadata |
| PTY ring | `GET /api/v1/sessions/{id}/output` | raw bytes, offset/limit |
| Transcripts | `GET /api/v1/sessions/{id}/transcripts[/catchup]` | structured turn history |
| Recording | `GET /api/v1/sessions/{id}/recording[/catchup]` | replay envelope |
| Input | `POST /api/v1/sessions/{id}/input{,/raw,/keys}` | typed keystrokes |
| Nudge | `POST /api/v1/sessions/{id}/agent/nudge` | high-level prod |
| Respond | `POST /api/v1/sessions/{id}/agent/respond` | structured reply |
| Live stream | `GET /ws/{session_id}` | per-session WS |
| Mux stream | `GET /ws/mux` | multi-session WS |

The WS handler accepts auth from three places (`coop/crates/mux/src/transport/ws.rs:39-55`):

1. `?token=<COOP_MUX_AUTH_TOKEN>` query param
2. `Authorization: Bearer …` header
3. OIDC JWT (when `state.oidc_verifier` is configured) via either header or
   the `X-Auth-Request-Access-Token` header injected by oauth2-proxy
   ForwardAuth (`coop/crates/mux/src/transport/auth.rs:68-86`)

CORS is `CorsLayer::permissive()` (`coop/crates/mux/src/transport/mod.rs:113`).
Browsers do not enforce same-origin for WebSocket handshakes the way they do
for `fetch`, so the `Origin` mismatch between `beads-viewer.<host>` and
`coopmux.<host>` is not a hard blocker for WS — but it *is* relevant for any
HTTP fetches we make from JS (see "Direct" option below).

**beads-viewer** (`gardenio/controller/cmd/beads-viewer/`) is a Go htmx
server. Its OIDC middleware (`internal/oidcauth/oidcauth.go`) reads the user's
JWT from either `Authorization: Bearer …` or `X-Auth-Request-Access-Token`.
Email lands in `r.Context()` via `EmailFromRequest`. Local dev has an explicit
opt-out via `BEADS_VIEWER_DEV_AUTH_BYPASS=1` (`dev_auth.go`), refused if a K8s
ServiceAccount token is mounted.

The viewer pod *today* has **no** coopmux env wired in. The relevant deployment
template (`gardenio/helm/gardenio/templates/beads-viewer/deployment.yaml`) only
sets `BEADS_HTTP_ADDR`, `BEADS_DAEMON_TOKEN`, OIDC config, and `BASE_PATH`.
Adding `COOPMUX_URL` and `COOP_MUX_AUTH_TOKEN` is a small, scoped change.

**coopmux ingress** (`gardenio/helm/gardenio/templates/coopmux/ingress.yaml`)
already exposes `Host(coopmux.<host>)` with three path rules: `/mux` (HTML
dashboard), `/ws/` (WS feeds), and `/api/v1/sessions` (sessions API). All
three are protected by the shared ForwardAuth middleware (oauth2-proxy →
Keycloak), the same one beads-viewer uses. The header comment in that
template is explicit: *"ForwardAuth (Keycloak SSO) protects browser-facing
routes; oauth2-proxy sets the Authorization header with the OIDC access token,
and coopmux's OIDC verifier (`COOP_MUX_OIDC_*`) validates it. Internal M2M
callers (controller, bridges) hit the ClusterIP service directly with the
static bearer from the kd-beads-token Secret, bypassing Traefik entirely."*

### Agent-bead → coopmux session ID

The agent bead carries `last_session_id` in its fields
(`gardenio/controller/internal/bridge/bot_agent_kill.go:201-206`). That value
is the coopmux session ID and the path parameter for `/ws/{session_id}` and
`/api/v1/sessions/{id}/...`. The viewer already surfaces it as a read-only
field in `templates/partials/type_agent.html:23`. Resolving from
`bead.Fields["last_session_id"]` is sufficient; no new linkage is needed.

### Anti-scope

- Do **not** propose a new ingress, a new auth provider, a new ring buffer in
  beads-viewer, or any modification to coopmux's HTTP/WS API.
- Do **not** address renderer choice (xterm.js vs `<pre>` vs SSE fragments) —
  that is R2.
- Do **not** address authorization / role gating for sending commands — that
  is R3.
- Do **not** address replay or event interleaving — that is R4.

## Options considered

For each option below, "browser → X" describes the network path of one of the
two *kinds* of operation we need:

- **Stream** = the high-volume PTY byte feed (the WS).
- **Control** = the htmx-shaped HTTP requests (snapshot, input, nudge,
  respond, transcript catchup, recording).

### Option A — Direct browser → coopmux for both stream and control

Browser opens `wss://coopmux.<host>/ws/{session_id}` for the stream and
`https://coopmux.<host>/api/v1/sessions/{id}/...` for control. beads-viewer
serves only the wrapper page; once it boots, all live traffic bypasses the
viewer entirely.

**Auth.** Both endpoints sit behind the shared oauth2-proxy ForwardAuth on
the `coopmux.<host>` IngressRoute. The user authenticates at the IdP exactly
once for the host (or once each for `coopmux.<host>` and
`beads-viewer.<host>` if the oauth2-proxy cookie scope is per-host); after
that, both browser fetches and WS handshakes carry the proxy cookie, the
ForwardAuth middleware validates it, and oauth2-proxy injects the user's
access token as `X-Auth-Request-Access-Token`. coopmux's OIDC verifier
validates the JWT. **The browser never sees `COOP_MUX_AUTH_TOKEN`.**

**Reachability.**

```
[browser]
  ─► https://coopmux.<host>/api/v1/sessions/{id}/screen
       Traefik → ForwardAuth → coopmux Service → coopmux Pod
  ─► wss://coopmux.<host>/ws/{session_id}
       Traefik → ForwardAuth → coopmux Service → coopmux Pod
```

**Pros**

- Zero wiring on the beads-viewer pod. No new env, no new secret mount.
- Lowest latency for both legs (no extra hop).
- Reuses already-deployed infra end-to-end.

**Cons**

- **Cross-origin CORS surface.** Even though coopmux runs `CorsLayer::permissive()`,
  any *write*-shaped request (`POST /input`, `POST /agent/nudge`,
  `POST /agent/respond`) carries credentials and may trigger a preflight from
  the `beads-viewer.<host>` origin to `coopmux.<host>`. Permissive CORS plus
  cookies = browser will reject, because `Access-Control-Allow-Origin: *`
  cannot be combined with `Access-Control-Allow-Credentials: true`. We would
  have to either (a) tighten coopmux's CORS to echo the `beads-viewer.<host>`
  origin and set `Allow-Credentials: true` (a coopmux change — out of scope
  per anti-scope #1), or (b) abandon cookie-based auth for control HTTP and
  embed an OIDC token in the `Authorization` header from JS, which means
  *the page must be able to read the JWT*. oauth2-proxy by default does not
  expose the JWT to the browser; doing so is a security regression we should
  not take.
- **Audit & policy fragmentation.** Send-commands authorization (R3) wants
  to gate writes by role/email. With control going direct, the only place to
  enforce that is coopmux itself, which only knows the JWT — not beads-viewer
  policy. Any future "read-only by default, write toggle" gate (R3 likely
  output) lives in two services instead of one.
- **htmx hostility.** htmx swaps fragments returned by an origin server. If
  the control plane lives at `coopmux.<host>`, every htmx target attribute is
  cross-origin and we cannot use htmx swaps for control responses without
  giving up CORS protections. We would end up writing JS for control too —
  which defeats the point of staying htmx-native.
- **Two ForwardAuth cookies for one app.** The user authenticates once per
  hostname; even with realm SSO this is a UX papercut and a known support
  ask whenever oauth2-proxy cookie scopes drift between hosts.

### Option B — beads-viewer proxies both stream and control

Browser only ever talks to `beads-viewer.<host>`. The Go server proxies
HTTP control endpoints with a stdlib `httputil.ReverseProxy` and tunnels the
WS through a server-side `gorilla/websocket` (or stdlib) bridge.

**Auth.** Browser → beads-viewer is OIDC-protected as today. beads-viewer →
coopmux uses the static `COOP_MUX_AUTH_TOKEN` (mounted from the existing
`{{ include "gardenio.coopmux.authTokenSecretName" . }}` Secret) on every
upstream call. The viewer's middleware authorizes the user before opening
the upstream connection; identity & policy are evaluated exactly once, at
the proxy boundary.

**Reachability.**

```
[browser]
  ─► https://beads-viewer.<host>/agents/{id}/screen
       Traefik → ForwardAuth → beads-viewer Service → beads-viewer Pod
         ─► http://gardenio-coopmux.<ns>.svc.cluster.local:<port>/api/v1/sessions/{id}/screen
              + Authorization: Bearer ${COOP_MUX_AUTH_TOKEN}
  ─► wss://beads-viewer.<host>/agents/{id}/stream
       Traefik → ForwardAuth → beads-viewer Service → beads-viewer Pod
         ─► ws://gardenio-coopmux.<ns>.svc.cluster.local:<port>/ws/{session_id}
              + Authorization: Bearer ${COOP_MUX_AUTH_TOKEN}
```

**Pros**

- One origin for the browser. CORS, htmx swaps, auth, and audit all
  collapse onto a single host.
- Single chokepoint for write authorization (R3) and audit logging
  ("user e@example.com sent `\u0003` to session `sess-X` at t").
- Reuses the dev-auth bypass story (`BEADS_VIEWER_DEV_AUTH_BYPASS`) for
  local dev — no second secret to thread through `make run-beads-viewer`.

**Cons**

- **WS proxying in Go is non-trivial under load.** A naïve dual-goroutine
  `io.Copy` works but careless implementations leak connections on close
  asymmetry, drop binary frames, or break the close handshake. coopmux's WS
  handler explicitly fixes the close-handshake bug
  (`coop/crates/mux/src/transport/ws.rs:113-115`); a mid-tier proxy must
  reproduce that discipline. This is a real implementation hazard, not a
  theoretical one.
- **Adds a steady-state byte-shovel between two pods**. PTY output for a
  busy agent is in the order of hundreds of KB/s. Proxying through Go adds
  one extra TCP hop and one additional connection pool to size — small in
  absolute terms (intra-cluster, single-digit-ms RTT) but it is real, ongoing
  cost we don't have today. beads-viewer is not currently provisioned for
  long-lived WS fan-out.
- **Reconnect storms hit beads-viewer first.** Every reconnect on the
  browser side becomes a *fresh* upstream connect from beads-viewer to
  coopmux. The current `WsBridge` re-use logic in coopmux
  (`coop/crates/mux/src/transport/ws.rs:127-144`) is keyed off the
  *immediate downstream client*; with a proxy, every browser reconnect looks
  to coopmux like a brand-new upstream connection.

### Option C — Hybrid (recommended)

- **Stream:** browser → `coopmux.<host>/ws/{session_id}` direct.
- **Control:** browser → `beads-viewer.<host>/agents/{id}/<verb>` →
  beads-viewer Go server → coopmux ClusterIP.

The wrapper `templates/partials/type_agent.html` page is served by
beads-viewer, embeds a small JS shim that opens the WS straight at coopmux
(reusing the existing oauth2-proxy cookie scoped to the realm), and uses
plain htmx attributes (`hx-get`, `hx-post`) for all control verbs targeting
beads-viewer routes.

**Auth — both legs use the same OIDC realm.**

- *Stream* authentication: browser's existing oauth2-proxy session cookie
  authenticates the WS upgrade against `coopmux.<host>`'s ForwardAuth; the
  upgraded socket is auth'd by coopmux's OIDC verifier validating the JWT
  injected by oauth2-proxy into `X-Auth-Request-Access-Token`. No JS-visible
  token; no static `COOP_MUX_AUTH_TOKEN` in the browser. (Cross-host cookie
  scope is the only operational requirement — both hosts already share the
  same Keycloak realm and oauth2-proxy deployment.)
- *Control* authentication: browser → beads-viewer is the existing
  beads-viewer OIDC middleware (no change). beads-viewer → coopmux ClusterIP
  uses `Authorization: Bearer ${COOP_MUX_AUTH_TOKEN}` injected by the proxy
  handler. The token lives only in the pod; it is *not* per-user and *not*
  per-agent — it is a single static M2M credential, identical to how the
  controller and bridges already call coopmux.

**Reachability.**

```
[browser]
  ─► wss://coopmux.<host>/ws/{session_id}
       Traefik → ForwardAuth → coopmux Service → coopmux Pod
                              (OIDC verifier validates JWT)
  ─► https://beads-viewer.<host>/agents/{id}/<verb>
       Traefik → ForwardAuth → beads-viewer Service → beads-viewer Pod
                              (OIDC middleware validates JWT, applies policy)
         ─► http://gardenio-coopmux.<ns>.svc.cluster.local:<port>/api/v1/sessions/{id}/<verb>
              + Authorization: Bearer ${COOP_MUX_AUTH_TOKEN}
```

**Pros**

- htmx-native control plane. Every write goes through one Go handler we
  already know how to authorize and audit.
- High-volume PTY fan-out goes coopmux → browser with no Go middleman; we
  reuse coopmux's existing per-session `WsBridge` re-use logic.
- No coopmux changes; no new ingress; no new secret type. The only new
  artifacts are: (a) the `templates/partials/type_agent.html` rewrite and a
  small JS shim (out of scope here), (b) two new env vars on the
  beads-viewer pod (`COOPMUX_URL`, `COOP_MUX_AUTH_TOKEN` — sourced from the
  existing coopmux token Secret).
- Auth surface is *identical to today*. We are reusing the same realm, the
  same ForwardAuth, the same oauth2-proxy.

**Cons**

- Two transports in the page (WS direct + htmx HTTP via beads-viewer).
  Slightly higher complexity than "everything through one host."
- Cookie scope across `beads-viewer.<host>` and `coopmux.<host>` must keep
  working. Today both are protected by the same oauth2-proxy and the same
  realm, so this is "stay correct" rather than "make correct" — but if a
  future change splits them onto different oauth2-proxy deployments, the WS
  leg breaks until cookie scope is restored. *Open question:* is the
  oauth2-proxy cookie set with `Domain=<root>` so it covers both hosts? If
  not, we need to confirm the `coopmux.<host>` ForwardAuth flow also issues
  a usable session for users arriving from `beads-viewer.<host>`. (This is a
  one-line check on the deployed `oauth2-proxy` config and worth filing as
  an explicit verification step before R6 implementation.)
- WS write-direction: any future requirement to send PTY input *over the WS*
  (vs the current `POST /input`) would put writes back on the direct leg
  and re-introduce the audit fragmentation we paid Option A to avoid. The
  recommendation explicitly leans on *control verbs going through the Go
  proxy* and the WS being read-mostly. R3 should not paint us into a corner
  by deciding to send keystrokes over the WS.

## Decision

**Adopt Option C (Hybrid).**

The decision rests on three claims, in priority order:

1. **The audit/policy story for write verbs is materially better when the
   Go server is in the path.** R3 will define a write toggle and role gate;
   that gate belongs in beads-viewer where we already hold the user's
   identity and have a place to log it. Option A hands that responsibility
   to coopmux, which is the wrong layer.
2. **htmx-native control means less JS and a smaller surface area.** The
   rest of beads-viewer is htmx — the agent terminal becoming a JS-driven
   SPA is a step backward we don't need to take.
3. **Direct WS for high-volume PTY avoids inventing a Go WS proxy.**
   Implementing a robust WS reverse-proxy (close-handshake, binary frames,
   reconnect symmetry, backpressure) is doable but it is its own rabbit
   hole. We don't need to dig it.

If any of those three claims is overturned in R3 or R4 (e.g., R3 decides
all writes flow over the WS, or R4 decides we want a unified event multiplex
that interleaves PTY + bead events on a single WS), this ADR should be
revisited.

## Sequence diagrams

### Page boot

```text
Browser           beads-viewer         coopmux            Keycloak/oauth2-proxy
   │                   │                  │                       │
   │ GET /agents/{id} ─►                  │                       │
   │     (ForwardAuth challenge ─────────────────────────────────►│
   │                                                              │ OIDC login)
   │     ◄───────────────────────────────── set proxy cookie ─────┤
   │                   │                  │                       │
   │ GET /agents/{id} (cookie present) ─► │                       │
   │                   │ ForwardAuth ok; render bead_show.html    │
   │                   │  embed <agent-terminal data-session-id   │
   │                   │   ="{{.LastSessionID}}">                 │
   │ ◄─── HTML ────────┤                  │                       │
   │                   │                  │                       │
```

### Stream subscribe (read-mostly PTY feed)

```text
Browser                                              coopmux            
   │                                                    │              
   │ GET wss://coopmux.<host>/ws/{session_id}          │              
   │   Cookie: _oauth2_proxy=...                        │              
   │   (ForwardAuth → OIDC verifier → 101 Upgrade)      │              
   │ ─────────────────────────────────────────────────► │              
   │                                                    │              
   │ ◄────── pty bytes (Text frames) ────────────────── │              
   │ ◄────── pty bytes ────────────────────────────────│              
   │ ◄────── pty bytes ────────────────────────────────│              
   │                       (bidirectional;             │              
   │                        WS-level writes are NOT    │              
   │                        used in v1 — see R3)       │              
   │ ─── close ───────────────────────────────────────► │              
   │ ◄────── close-ack ─────────────────────────────── │              
```

### Control verb (snapshot, send-input, nudge, respond)

```text
Browser              beads-viewer              coopmux
   │                      │                       │
   │ POST /agents/{id}/input  (htmx)              │
   │   Cookie: _oauth2_proxy=...                  │
   │   form: text=<keystrokes>                    │
   │ ───────────────────► │                       │
   │                      │ middleware: validate OIDC; resolve user
   │                      │ middleware: dev-auth bypass refused if SA token mounted
   │                      │ R3 gate: writer role? else 403
   │                      │ POST /api/v1/sessions/{id}/input
   │                      │   Authorization: Bearer ${COOP_MUX_AUTH_TOKEN}
   │                      │ ────────────────────► │
   │                      │ ◄────── 200 OK ────── │
   │                      │ render htmx fragment (status pill / toast)
   │ ◄── 200 + fragment ──┤                       │
```

### Snapshot (cold start / reconnect catchup)

```text
Browser              beads-viewer              coopmux
   │                      │                       │
   │ GET /agents/{id}/screen   (htmx hx-get on mount)
   │ ───────────────────► │                       │
   │                      │ GET /api/v1/sessions/{id}/screen
   │                      │   Authorization: Bearer ${COOP_MUX_AUTH_TOKEN}
   │                      │ ────────────────────► │
   │                      │ ◄────── 200 + screen JSON ──
   │                      │ render htmx fragment (initial terminal state)
   │ ◄── 200 + fragment ──┤                       │
```

The screen snapshot is what bridges the gap between page load and the WS
catching up; it is the htmx equivalent of the iOS app's initial fetch.
Returning HTML (a renderable fragment) instead of JSON keeps the renderer
choice (R2) free.

## Network reachability

| From → To | Path | Auth | Notes |
|---|---|---|---|
| Browser → beads-viewer | `https://beads-viewer.<host>/...` | oauth2-proxy ForwardAuth → beads-viewer OIDC middleware | Existing path. No change. |
| Browser → coopmux WS | `wss://coopmux.<host>/ws/{session_id}` | oauth2-proxy ForwardAuth → coopmux OIDC verifier | Existing path. No change. Relies on shared cookie scope. |
| beads-viewer → coopmux | `http://gardenio-coopmux.<ns>.svc.cluster.local:<port>/api/v1/sessions/...` | static `COOP_MUX_AUTH_TOKEN` (Bearer) | New code in beads-viewer; reuses the existing controller/bridges pattern. |
| Local dev (port-forward) | beads-viewer ↔ port-forwarded coopmux ClusterIP | `BEADS_VIEWER_DEV_AUTH_BYPASS=1` + `COOP_MUX_AUTH_TOKEN` env | Same dev-loop story as today's `BEADS_HTTP_ADDR` port-forward. |

In all production environments (gardenio-rwx, gardenio) coopmux is deployed
alongside beads-viewer in the same release. The Helm chart already names the
Service via `gardenio.coopmux.fullname` and the auth token via
`gardenio.coopmux.authTokenSecretName`; both helpers exist today and are used
by the controller deployment template. Wiring beads-viewer to them is a
helm-only delta with no template invention.

If a deployment opts to *disable* coopmux (`coopmux.enabled: false`), the
agent terminal view should degrade to the current static field-list with a
banner ("agent terminal unavailable in this deployment"). This avoids
coupling the new view to a hard runtime dependency.

## Auth token model — where does the token live?

Two distinct credentials, neither of which is per-agent:

1. **User OIDC JWT** — issued by Keycloak when the user signs in via
   oauth2-proxy. Stored in the proxy session cookie. *Per-user, per-host
   cookie scope.* coopmux's OIDC verifier validates the JWT for the WS leg;
   beads-viewer's OIDC middleware validates the same JWT for the control
   leg. **The JWT is not exposed to JavaScript.**
2. **`COOP_MUX_AUTH_TOKEN`** — static M2M bearer mounted into the
   beads-viewer pod from the existing `gardenio-coopmux-token` Secret (same
   secret already mounted into the controller and bridges). *Single token
   per cluster*, not per-user and not per-agent. Used only on the
   intra-cluster leg; never leaves the pod; never rendered into a template.

There is *no* per-agent token. The `last_session_id` field on the agent
bead is sufficient to route the user's request to the right coopmux
session; authorization (which user can talk to which session) is enforced
by R3's policy gate in beads-viewer using bead labels and user role.

## Open questions (downstream tasks)

| ID | Question | Affects |
|---|---|---|
| OQ-1 | Is the oauth2-proxy cookie scoped (`Domain=<root>`) so it covers both `beads-viewer.<host>` and `coopmux.<host>` without re-login? Verify before R6 cuts implementation. | Hybrid, Option A |
| OQ-2 | Renderer (xterm.js / SSE fragments / `<pre>` re-render) determines the WS frame format we expect from coopmux. coopmux already encodes via `WsBridge`; renderer must consume what's there without coopmux changes. | R2 |
| OQ-3 | What is the *write* authorization model — read-only by default + explicit toggle? Per-bead policy via labels (`ops-only`)? Per-user role via Keycloak (`gardenio-operator`)? | R3 |
| OQ-4 | Should bead events (comments, state changes, mail) interleave with PTY output in the page? If yes, do we open a *second* stream (NATS-backed bead events via beads-viewer SSE) or extend coopmux's WS with a multiplex? The latter is a coopmux change and out of scope for the current epic. | R4 |
| OQ-5 | Reconnect/replay semantics: page reload re-fetches the screen snapshot (good); a network blip mid-stream needs catchup via `/output?offset=` or `/transcripts/catchup`. Which one is the canonical resume primitive? | R4 |
| OQ-6 | Agent rotation (`bot_agent_kill.go` carries forward `last_session_id`) means an agent bead's session ID can change while the page is open. Does the page poll for ID change and reconnect, or does it require a manual reload? | R4 |

## Anti-scope reminders

- **No new ingress.** The hybrid path uses two existing IngressRoutes
  unchanged.
- **No new auth provider.** Keycloak + oauth2-proxy + ForwardAuth, as
  deployed today, is the auth layer for both legs.
- **No coopmux HTTP/WS API changes.** Every endpoint listed in the table
  above already exists. If a future task identifies a gap (e.g. a
  multiplexed bead-events + PTY WS), it must be filed as a separate
  beads-viewer-blocked-by task on the coop crate, per the parent epic's
  anti-scope.

## Acceptance for this ADR

- [x] Three options compared with explicit trade-offs.
- [x] Recommendation (Hybrid) with the three load-bearing claims it rests on.
- [x] Sequence diagrams: page boot, stream subscribe, control verb, snapshot.
- [x] Reachability table covering production, dev, and degraded modes.
- [x] Auth-token model spelled out (user JWT vs static M2M; never per-agent).
- [x] Open questions filed for the downstream research tasks (R2–R6).
- [x] No Go / template / JS / coopmux code changes.
