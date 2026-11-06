# beads-viewer local dev, screenshot harness, and E2E suite

Three adjacent workflows live in this doc:

1. **Local boot** — how to run `beads-viewer` against a real beads daemon
   without standing up Keycloak. Owned by ka-0000007138.
2. **Screenshot harness** — a Playwright suite that captures canonical pages
   at pinned viewports so styling MRs can attach before/after evidence.
   Owned by ka-0000001180.
3. **E2E behavioral suite** — a Playwright suite that asserts on DOM,
   navigation, and htmx swaps. Auto-boots the viewer. Phase 1 owned by
   ka-0000005681 under epic ka-0000004611.

## 1. Local boot

How to run `beads-viewer` on your laptop (or a dev container) against a real
beads daemon without standing up Keycloak.

### TL;DR

```sh
# 1. Port-forward the in-cluster beads daemon (separate terminal).
kubectl -n gardenio port-forward svc/gardenio-beads 8080:8080

# 2. Start beads-viewer with OIDC disabled.
make run-beads-viewer

# 3. Open http://localhost:8095/
```

Every template (index, search, `/p/<project>`, `/bead/<id>`) is reachable.
Comments and dependencies render against live cluster data.

### What `make run-beads-viewer` does

It first runs `make fetch-viewer-vendor`, which downloads `mermaid.min.js` and
`echarts.min.js` into `templates/static/` (pinned and sha256-checked by
`cmd/beads-viewer/fetch-vendored.sh`; they are gitignored, gd-wNQzR9ptZI). Run
that script yourself before a bare `go build` or `go test` of beads-viewer.

It sets:

- `BEADS_VIEWER_DEV_AUTH_BYPASS=1` — replaces the OIDC middleware with a
  logged passthrough. Default off.
- `BEADS_HTTP_ADDR=http://localhost:8080` — the port-forwarded daemon.
- `LISTEN_ADDR=:8095` — the beads-viewer HTTP port.
- `LOG_LEVEL=debug`.

Not set by the target, but useful: `BEADS_VIEWER_THEME=plain` turns off the Dawn
Meadow look (the scene, tokens and pixel sign shared with the login page, S1j
gd-cxU8eyiGyR) and renders the pre-theme pages; unset is the meadow. The chart
value is `beadsViewer.theme`.

Override on the make invocation:

```sh
make run-beads-viewer BEADS_VIEWER_ADDR=http://localhost:18080 BEADS_VIEWER_LISTEN=:8096
```

### Prod-env guard

`beads-viewer` refuses to start with the bypass flag set when it detects a
K8s service-account token mount at
`/var/run/secrets/kubernetes.io/serviceaccount/token`. This makes it
impossible to accidentally deploy bypass mode to a real cluster pod. If you
see this error, you are trying to run bypass mode somewhere you should not:

```
beads-viewer: BEADS_VIEWER_DEV_AUTH_BYPASS refused: K8s service account token present — this looks like a prod-like environment
```

### Agent-pod opt-out (`BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD`)

Audit and automation formulas (ka-0000006568 and friends) need to run
beads-viewer inside an agent pod, which always has a SA token mounted.
That trips the prod-env guard above. Set
`BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD=1` alongside the bypass flag
to opt out — but only in the contexts below.

When the ACK is honored:

- The SA-token guard is downgraded from refuse-to-start to allow.
- `LISTEN_ADDR` is **forced to a loopback interface**: a bind-all value
  (`:8095`, `0.0.0.0:8095`, `[::]:8095`) is rewritten to `127.0.0.1:8095`,
  and an explicit non-loopback `LISTEN_ADDR` refuses to start. The bypass
  surface can never be reached over the pod network.

Appropriate uses:

- Non-interactive audit formulas that run a one-shot scrape against the
  in-cluster beads daemon (Option 3 of the info-density audit).
- Local automation inside an agent pod where the operator owns the pod
  and only loopback consumers (curl, screenshot harness) need to reach
  the viewer.

Inappropriate uses:

- Anything user-facing or operator-facing. There is no OIDC; the ACK is
  not a substitute for real auth.
- Any pod that is reachable from a Service, Ingress, or NetworkPolicy
  selector. The loopback enforcement protects against accidental
  exposure, but the right tool there is OIDC, not the bypass.

### Authentication

With bypass active, there is no login flow. Every request is served as an
anonymous user. The handlers in `beads-viewer` do not read email/roles
from the context, so feature-for-feature parity with the deployed UI is
preserved except for the nav "log out" link, which has nothing to clear.

### Talking to the daemon

Data comes from the beads daemon at `BEADS_HTTP_ADDR`. Two common setups:

#### A. Port-forward the in-cluster daemon (recommended)

```sh
kubectl -n gardenio port-forward svc/gardenio-beads 8080:8080
```

If the daemon requires a token (it usually does in prod-like clusters), set
`BEADS_DAEMON_TOKEN`:

```sh
TOKEN=$(kubectl -n gardenio get secret gardenio-beads-token -o jsonpath='{.data.token}' | base64 -d)
BEADS_DAEMON_TOKEN=$TOKEN make run-beads-viewer
```

Pros: real data across every project, every template exercised by actual
content, no seed work. Cons: read-only only — do not mutate anything.

#### B. Local kbeads daemon

Run your own `kd serve` against a local Postgres, then point
`BEADS_HTTP_ADDR` at it. This is heavier (kbeads has no SQLite mode —
you need Postgres) and usually not worth it for viewer styling work.

See `kbeads/README.md` for daemon setup; then:

```sh
make run-beads-viewer BEADS_VIEWER_ADDR=http://localhost:8080
```

### Templates exercised

`beads-viewer` renders four page templates plus two partials. Canonical URLs
to hit against a populated daemon:

| Page              | URL                              |
|-------------------|----------------------------------|
| Index             | `/`                              |
| Project           | `/p/gardenio`                     |
| Bead show         | `/<some-bead-id>` (e.g. `/ka-abc123`) |
| Search            | `/search?q=release`              |
| Comments partial  | `/b/<id>/comments` (htmx)        |
| Deps partial      | `/b/<id>/deps` (htmx)            |

For a bead-show page with both comments and deps, pick any open epic from
the target project — most have dependencies and commentary.

### Logs

Bypass mode logs a WARN at startup and a DEBUG line per request. Grep
`dev auth bypass` in the output to confirm the bypass is active.

## 2. Screenshot harness (ka-0000001180)

Playwright suite at `gardenio/tests/beads-viewer-screenshots/`. Captures:

| Page          | URL                      |
| ------------- | ------------------------ |
| Index         | `/`                      |
| Project       | `/p/<project>`           |
| Bead-show     | `/<bead-id>`             |
| Search        | `/search?q=<query>`      |

Each page is captured at two viewports:

- **desktop** — 1280×800
- **narrow**  — 480×800

### Run

```sh
# Start beads-viewer locally (see section 1), then:
make screenshot-beads-viewer
```

Screenshots land in `test-results/beads-viewer-screenshots/<viewport>/<page>.png`
(relative to the harness directory). The MR formula consumes this directory
layout for before/after comparison.

### Env knobs

| Env                     | Default          | Meaning                                     |
| ----------------------- | ---------------- | ------------------------------------------- |
| `BEADS_VIEWER_URL`      | `http://localhost:8095` | Where the running viewer is reachable |
| `SCREENSHOT_PROJECT`    | `gardenio`        | Slug of a seeded project (≥1 open bead)     |
| `SCREENSHOT_BEAD_ID`    | `ka-0000001180`  | A seeded bead with comments + deps          |
| `SCREENSHOT_QUERY`      | `screenshot`     | A search query with ≥1 result               |

### Determinism

- Animations, transitions, caret blink, and scroll behavior are disabled via
  an injected stylesheet after page load.
- `reducedMotion: 'reduce'`, fixed `timezoneId: 'UTC'`, `colorScheme: 'light'`,
  and fixed `locale: 'en-US'` are pinned in the Playwright config.
- Viewports are fixed (no device scaling).
- Test data is pinned via `SCREENSHOT_*` env vars above.

Positions still drift when the underlying beads DB changes (e.g. new comments
land on `ka-0000001180`). For rigorous visual diffing, pin the harness to a
frozen seed dataset.

### Non-goals

- Visual regression **gating** on CI. For now artifacts + eyeball review are
  enough; `test-results/beads-viewer-screenshots/` is an MR attachment, not a
  fail-closed check.

### Troubleshooting

- `net::ERR_CONNECTION_REFUSED` → beads-viewer isn't running; see section 1.
- All pages render the OIDC redirect → local auth bypass isn't enabled; set
  `BEADS_VIEWER_DEV_AUTH_BYPASS=1`.
- `404` on bead-show → `SCREENSHOT_BEAD_ID` does not exist in the seed data;
  override via env.

## 3. E2E behavioral suite (ka-0000004611)

Playwright suite at `gardenio/tests/beads-viewer-e2e/`. Sibling to the
screenshot harness — that one is advisory/visual; this one is fail-loud
and intended to gate CI on viewer-touching MRs.

Plan of record: `gardenio/plans/archive/beads-viewer-playwright-tests.md` (archived; suite shipped).

### Run

```sh
# Port-forward the beads daemon in another terminal (T2 will replace this
# with a hermetic in-CI fixture):
kubectl -n gardenio port-forward svc/gardenio-beads 8080:8080

# From the monorepo root:
make test-beads-viewer-e2e
```

`make test-beads-viewer-e2e` installs deps, installs chromium, then
runs the suite. The Playwright config's `webServer:` block auto-boots
the viewer with `BEADS_VIEWER_DEV_AUTH_BYPASS=1` — you do **not** need
to run `make run-beads-viewer` separately.

When iterating on tests, leaving `make run-beads-viewer` running in
another shell is fine: outside CI, Playwright sets
`reuseExistingServer: true` and skips the boot.

### Env knobs

| Env                  | Default                    | Meaning                                    |
| -------------------- | -------------------------- | ------------------------------------------ |
| `BEADS_VIEWER_URL`   | `http://localhost:8095`    | Where the running viewer is reachable      |
| `BEADS_VIEWER_PORT`  | `8095`                     | Port the auto-booted viewer listens on     |
| `BEADS_HTTP_ADDR`    | `http://localhost:8080`    | Beads daemon HTTP address                  |
| `LOG_LEVEL`          | `info`                     | Viewer log level (`debug` for noisy boot)  |
| `CI`                 | unset                      | Disables `reuseExistingServer`, adds retry |

### htmx-idle wait pattern

htmx swaps are async. Tests must NOT use `page.waitForTimeout()` — use
the htmx in-flight counter helper instead. Source:
`gardenio/tests/beads-viewer-e2e/tests/_helpers.js`.

```js
import { waitForHtmxIdle } from './_helpers.js';

await page.locator('[hx-get]').first().click();
await waitForHtmxIdle(page);
await expect(page.locator('#swapped-target')).toContainText('expected');
```

The helper polls `document.body[hx-in-flight]` until it reads `0` (or the
attribute is absent), then yields one `requestAnimationFrame` so any
swap-driven layout work settles before the next assertion.

### Adding tests

The T1 scaffold contains exactly one smoke test (`tests/smoke.spec.js`)
that asserts the index page renders. Coverage expansion is staged across
T3–T17 of epic `ka-0000004611`:

- T3 — route smoke
- T4 — navigation + active-tab + breadcrumbs
- T5/T6 — per-type detail templates
- T7 — comment write path
- T8 — htmx partial swaps
- T9 — decision resolve
- T10 — search debounce
- T11 — type/status/label filters
- T12 — keyboard shortcuts
- T13 — drafts persistence
- T14 — auth middleware
- T15 — markdown + linkify
- T16 — a11y baseline (axe-core)
- T17 — responsive

Keep one `*.spec.js` file per concern — do **not** stuff everything into
`smoke.spec.js`.

### Layout decision (T1)

Sibling directory (this layout) was picked over folding into the
screenshot harness because behavioral config (parallel workers,
fail-loud, CI gate) and visual config (single worker, advisory,
deterministic rendering knobs) diverge enough that one config trying to
serve both gets gnarly. The plan-of-record recommended this layout; T1
went with the recommendation.

### Troubleshooting

- `net::ERR_CONNECTION_REFUSED` against `:8080` → port-forward to
  `gardenio-beads` is not running.
- Playwright reports `Timed out waiting for /healthz` → check the
  `webServer:` stdout (Playwright pipes it through); the viewer
  refuses to start with `BEADS_VIEWER_DEV_AUTH_BYPASS=1` if it
  detects a K8s SA token mount (you are inside a real cluster pod).
  Run the suite from a workstation or a non-cluster container.
- Tests pass locally but fail in CI → likely a fixture / test-data
  drift issue. The hermetic CI dataset is T2's deliverable; until that
  lands, treat live-daemon flakes as expected and pin to specific bead
  ids in the test (mirroring the screenshot harness pattern).

## 4. Agent terminal local dev

The agent-detail page at `/<agent-bead-id>` renders an xterm.js pane that
attaches to a coopmux WebSocket. To exercise the full PTY path locally you
need three things: the beads daemon (section 1), the running viewer
(section 1), and a coopmux session reachable over plain HTTP/WS so the
browser can attach without TLS.

### TL;DR

```sh
# T1: beads daemon (cluster).
kubectl -n gardenio port-forward svc/gardenio-beads 8080:8080

# T2: coopmux (cluster). Pick any free local port; 8090 is a convention.
kubectl -n gardenio port-forward svc/gardenio-coopmux 8090:80

# T3: pull the coopmux auth token (used by beads-viewer to call coopmux's
# REST API for the cached screen snapshot — NOT the WS attach, which the
# browser does directly).
COOP_MUX_AUTH_TOKEN=$(kubectl -n gardenio get secret gardenio-coopmux-auth \
  -o jsonpath='{.data.token}' | base64 -d)

# T4: viewer with bypass + coopmux pointer.
COOPMUX_URL=http://localhost:8090 \
COOP_MUX_AUTH_TOKEN=$COOP_MUX_AUTH_TOKEN \
make run-beads-viewer
```

Then open `http://localhost:8095/<agent-bead-id>` for any agent in
`open` or `in_progress` state with a `last_session_id` field. The page
renders the cached snapshot from coopmux first, then upgrades to the live
WS once `xterm.bundle.js` finishes booting.

### Why two coopmux env vars

`COOPMUX_URL` is the REST endpoint the **server** uses to fetch the cached
screen snapshot for the first paint and the htmx fallback. `COOP_MUX_AUTH_TOKEN`
authenticates that REST call. The browser's WebSocket attach
(`wss://coopmux.<host>/ws/<session-id>`) bypasses both — it is gated by
the same oauth2-proxy ForwardAuth flow as the rest of the deployment, so
the cookie that authenticates the page also authenticates the WS.

When running locally with `BEADS_VIEWER_DEV_AUTH_BYPASS=1`, there is **no
OIDC cookie** — the WS attach therefore goes through whatever auth the
local coopmux endpoint is configured for. With the port-forward above
there is no auth at all (you reach the in-cluster pod directly), so the
attach succeeds.

### Operator surfaces

- **Read-only fallback** is always rendered: a server-side `<pre>` of the
  last cached frame. Useful when the WS is broken — you still get text.
- **Take control** clears the take-control banner and lets the keys/nudge/
  respond surfaces submit. The `RoleOperator` middleware gate enforces
  this server-side; in dev bypass mode the gate is open.
- **Health dots** (`#term-health-pty`, `#term-health-events`) reflect WS
  and SSE state respectively. Cycle the port-forwards in T1 / T2 to
  observe them flip from `live` → `offline` → `connecting` → `live`.
- **Font-size selector** sits above the terminal; choice persists via
  the `agent_term_fontsize` cookie.
- **`?mode=slack`** writes the `agent_feed_mode` cookie. Reload without
  the query string and the cookie keeps the chosen mode.

### Coarse-pointer / mobile defaults

The viewer detects coarse pointers via the User-Agent on the request
(`isCoarsePointer` in `middleware.go`). Mobile sessions get a smaller
xterm scrollback (1000 vs 5000 rows) so the page is cheap on phones. To
force-test mobile defaults from a desktop browser, append `?coarse=1` to
any agent URL. The `data-coarse-pointer` attribute on `.agent-card`
exposes the choice to the client JS.

### Audit hardening

Every operator write logs a structured line through `s.logger.Info`:

```
event=agent_terminal_write subverb=keys|input|nudge|respond
  bead_id=<id> session_id=<sid> actor=<email> roles=<csv>
```

Tail the viewer log while clicking through the keys / nudge / respond
flows to confirm the audit lines land — these are the canonical record of
who did what to which agent. The bridge does not duplicate this stream,
so the viewer log is the source of truth.

### Common pitfalls

- `connect failed` in `#term-status` with the bypass server up → likely
  the coopmux port-forward is dead. Restart T2 above.
- WS attaches but no frames arrive → the bead's `last_session_id` is
  stale (the agent died and got a new pod). Reload the agent list, pick
  one with a recent `last checked` timestamp.
- `prefers-reduced-motion: reduce` set system-wide → the catching-up
  pulse animation is suppressed and `cursorBlink` is disabled at xterm
  init. Both are intended; verify with `matchMedia` in the browser
  console if you need to be sure.
