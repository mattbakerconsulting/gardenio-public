# Plan-of-record — beads-viewer agent terminal

- **Bead:** [ka-0000001758](https://beads.gardenio.e2e.example.com/ka-0000001758) (R6)
- **Parent epic:** [ka-0000003802](https://beads.gardenio.e2e.example.com/ka-0000003802) — beads-viewer iOS-parity Phase 3 (agent-terminal custom view)
- **Status:** **Delivered (phases 1–4).** Corrected 2026-09-21 — this line previously
  read "Research-only until a human green-lights phase 1", which was stale: the agent
  terminal is in production code under `gardenio/controller/cmd/beads-viewer/` and is
  deployed at `mux.clonk.tv`. Treat the phase sections below as a record of what was
  built, not as pending work. Remaining follow-ups, if any, belong on a new `gd-` bead.
- **Scope:** `gardenio/controller/cmd/beads-viewer/` (Go htmx). NOT beads3d, NOT iOS, NOT Slack bridge.

## What this document is

This rolls R1–R5 into a single phased rollout. ADR text lives in the per-research
docs; this plan recaps the choice in 2–3 lines and defines the phased
implementation children that will be cut, in order, after human approval.

| Round | Topic | Doc | Bead |
|---|---|---|---|
| R1 | Architecture (browser ↔ coopmux + auth) | `gardenio/docs/research/beads-viewer-agent-terminal-architecture.md` | [ka-0000006916](https://beads.gardenio.e2e.example.com/ka-0000006916) |
| R2 | Renderer choice | `gardenio/docs/research/beads-viewer-agent-terminal-renderer.md` | [ka-0000003333](https://beads.gardenio.e2e.example.com/ka-0000003333) |
| R3 | Send-commands UX + auth | `gardenio/docs/research/beads-viewer-agent-terminal-send-commands.md` | [ka-0000002915](https://beads.gardenio.e2e.example.com/ka-0000002915) |
| R4 | Streaming model + reconnect | `gardenio/docs/research/beads-viewer-agent-terminal-events.md` | [ka-0000004621](https://beads.gardenio.e2e.example.com/ka-0000004621) |
| R5 | iOS-parity feature matrix | `gardenio/docs/research/beads-viewer-agent-terminal-ios-parity.md` | [ka-0000003585](https://beads.gardenio.e2e.example.com/ka-0000003585) |

## ADR recap

### R1 — Architecture: hybrid (browser → coopmux WS direct; control via beads-viewer Go proxy)

The browser opens `wss://coopmux.<host>/ws/{session_id}` directly for the high-volume
PTY stream (auth via the existing oauth2-proxy ForwardAuth + Keycloak realm — no
JS-visible token). All control verbs (`/screen`, `/input`, `/keys`, `/agent/nudge`,
`/agent/respond`) flow through beads-viewer's Go server, which proxies to the coopmux
ClusterIP using the static `COOP_MUX_AUTH_TOKEN`. One auth surface for writes,
zero-copy fan-out for reads, no coopmux changes.

### R2 — Renderer: xterm.js, lazy-loaded on `/agents/{id}` only; `<pre>` fallback for first paint and `<noscript>`

xterm.js v5 + FitAddon + WebLinksAddon (~86 KB gz, capped at 125 KB gz). Server-side
renders the `/screen` snapshot as a `<pre>` first paint so the page is meaningful
before JS attaches; xterm hides the fallback once it owns the canvas. Plain `<pre>`
re-render and SSE→fragment swap rejected: both throw away R1's WS architecture, lose
alt-screen fidelity, and reimplement an ANSI VM in Go for no win.

### R3 — Send-commands: read-only by default; "Take control" toggle gated on `gardenio-operator`

Four POST verbs at `/agents/{id}/{input,keys,nudge,respond}`. `/input/raw` deliberately
omitted in v1. Destructive keys (`Ctrl-C`, `Ctrl-D`, `Ctrl-\`, `Ctrl-Z`) require
inline click confirmation. Identity stamped into beads-viewer's structured access log
(`event=agent_terminal_write`) and forwarded upstream as `X-Gardenio-User` /
`X-Gardenio-Roles` headers (coopmux ignores them in v1; persistence is a clean
follow-up). Reuses the existing `oidcauth.RequireRole(RoleOperator)` helper.

### R4 — Streaming: PTY-only on the live transport; bead events as parallel SSE lane

Two independent live channels, two independent reconnect cursors: PTY uses byte
offset (`/output?offset=` + `total_written` from the coopmux ring buffer); bead events
use a wall-clock `since` cursor against a new `/agents/{id}/events` SSE proxy that
filters the daemon's existing event stream. Agent rotation (new `last_session_id` on
`bead.updated`) is detected on the events lane and triggers a WS swap on the PTY lane
without page reload. Backpressure is handled at the JS edge: per-frame render budget;
sustained burst collapses to a fresh `/screen` snapshot.

### R5 — iOS parity: Full on every event-feed feature; Exceeds on PTY pane, deep-links, copy/paste, key input, audit

iOS `AgentDetailView` is a structured event feed with one mail-bead text input — no
PTY pane, no nudge, no respond, no keys. R1–R4 already commit beads-viewer to a real
PTY pane plus a parallel bead-events lane plus four write verbs, so the viewer meets
full parity on the event surface and exceeds it everywhere a browser can. Omissions
are SwiftUI-only idioms with no web analog (haptics, navigation-stack push, local
"clear activity" trash, sheet detents, swipe-to-respawn — the last lives on the agent
list, not the detail view).

## Phased implementation children

Each phase is a child task to cut **after** human go/no-go on phase 1. Each phase
ships independently, its own MR, its own bead. Acceptance criteria are observable
on a deployed gardenio-rwx; effort estimates are calibrated against the existing
`type_agent.html` work.

### Phase 1 — Skeleton template + read-only PTY stream (FIRST CHILD)

**Goal.** Replace the static `templates/partials/type_agent.html` field-list with the
new agent-detail layout (header strip, decisions block, empty event feed, `<pre>`
screen fallback, lazy-loaded xterm.js attached to the WS read-only). Read-only:
no input form, no keys row, no nudge / respond. iOS-parity bar matches header +
decisions + empty event feed; the PTY pane is the parent epic's headline feature
delivered behind a feature toggle.

**Files touched.**

| File | Change |
|---|---|
| `gardenio/controller/cmd/beads-viewer/templates/partials/type_agent.html` | Rewrite from 34-line field-list to layout: `<header>` (state pill, coop dot, started-ago, project/role, claimed task link, tool/session/cwd), `<section id="agent-decisions">`, `<section id="agent-terminal">` containing `<pre id="term-fallback">{{.ScreenAnsiText}}</pre>` + `<div id="term" hidden>` + `<div id="term-status">`, `<aside id="agent-events">` (empty for now). |
| `gardenio/controller/cmd/beads-viewer/agents.go` | Resolve `bead.Fields["last_session_id"]`; wire `ScreenAnsiText` into the template by calling `coopmux.GET /api/v1/sessions/{id}/screen`. New helper `getScreenSnapshot(ctx, sessionID) (string, error)`. Degrade to fallback banner when `last_session_id` is empty or coopmux is disabled. |
| `gardenio/controller/cmd/beads-viewer/server.go` | New routes: `GET /agents/{id}/screen` (htmx fragment for catchup) and a small env-config block (`COOPMUX_URL`, `COOP_MUX_AUTH_TOKEN`) loaded once at startup. New thin coopmux client (`internal/coopmuxapi/`) with `GetScreen`, `GetOutput`, no writes yet. |
| `gardenio/controller/cmd/beads-viewer/templates/static/xterm.bundle.js` | Vendored (or content-hashed via build step) xterm.js core + FitAddon + WebLinksAddon. Bundle budget: ≤125 KB gz. |
| `gardenio/controller/cmd/beads-viewer/templates/static/agent_terminal.js` | New ~5 KB glue: open WS at `wss://coopmux.<host>/ws/{session_id}`, `term.write` on each frame, hide `#term-fallback` on first frame, on close issue htmx GET `/agents/{id}/screen` then reopen WS with backoff. |
| `gardenio/controller/cmd/beads-viewer/templates/layout.html` | Conditionally `<script defer src="/static/xterm.bundle.js">` only on `/agents/{id}` route; nothing on other routes. |
| `gardenio/helm/gardenio/templates/beads-viewer/deployment.yaml` | Add `COOPMUX_URL` env (from `gardenio.coopmux.fullname`) and `COOP_MUX_AUTH_TOKEN` env-from-secret (from existing `gardenio.coopmux.authTokenSecretName`). |
| `gardenio/controller/cmd/beads-viewer/agents_test.go` | Tests for screen-snapshot rendering (success / no-session / coopmux-disabled). |
| `.rwx/build-beads-viewer.yml` (or equivalent) | Bundle-size assertion: fail CI if `xterm.bundle.js.gz` exceeds 125 KB. |

**Acceptance.**

- Loading `/agents/{id}` for an agent with `last_session_id` set renders the new
  layout, with the current `/screen` snapshot painted into `#term-fallback` server
  side, before any JS executes (verify with `curl` or JS-disabled browser).
- With JS enabled, xterm.js attaches, `#term-fallback` hides, live PTY bytes flow
  into the canvas. Verified manually against a busy gardenio-rwx agent.
- Network blip: WS closes → page issues `GET /agents/{id}/screen` → fallback
  re-renders → WS reopens. No frozen UI.
- `/agents/{id}` for an agent with empty `last_session_id` (or with
  `coopmux.enabled: false`) renders the new header + a one-line "agent terminal
  unavailable in this deployment" banner; no broken WS attempts.
- xterm `screenReaderMode: true` enabled; terminal `<div>` carries
  `role="application"` + accessible name.
- `xterm.bundle.js.gz` size assertion in CI is wired and green.
- Existing `type_agent.html` regression tests (`type_views_test.go`) all pass against
  the rewritten template.

**Effort.** ~3 days. Largest risks: vendoring xterm.js and the bundle-size CI gate;
the rest is mostly template + small Go handler work.

**Anti-scope for phase 1.** No input form. No keys row. No nudge / respond. No
bead-events SSE lane (the `<aside>` is empty markup). No write authorization gate
(no writes exist yet).

### Phase 2 — Bead-events SSE lane + input (write toggle)

**Goal.** Wire R4's bead-events lane into `<aside id="agent-events">` and ship R3's
text-input write surface. "Take control" toggle gated on `gardenio-operator`.

**Files touched.**

| File | Change |
|---|---|
| `gardenio/controller/cmd/beads-viewer/agents.go` | New handler `GET /agents/{id}/events` (SSE proxy of `beadsapi.Client.EventStream`, filtered to `bead_id ∈ {agent, parent task, descendants}`); new handler `POST /agents/{id}/input` (htmx-friendly form → coopmux `/input`); new handler `POST /agents/{id}/control/claim` (read-only-vs-write toggle handler that returns the rendered toolbar fragment for the current role). |
| `gardenio/controller/cmd/beads-viewer/middleware.go` | Wrap the new POSTs with `oidcauth.RequireRole(RoleOperator)`. Structured access-log line `event=agent_terminal_write`. Forward `X-Gardenio-User` / `X-Gardenio-Roles` / `X-Gardenio-Request-Id` headers on the upstream `proxy_post`. |
| `gardenio/controller/cmd/beads-viewer/templates/partials/type_agent.html` | Populate `<aside id="agent-events">`: `hx-ext="sse"`, `sse-connect="/agents/{id}/events"`, `sse-swap="..."` and an empty-state with mode toggle. Add `<form hx-post="/agents/{id}/input">` (textarea + Send button) below the terminal, gated by data-attribute `data-mode="readonly|write"`. Add `<button hx-post="/agents/{id}/control/claim">Take control</button>`. |
| `gardenio/controller/cmd/beads-viewer/templates/static/agent_terminal.js` | Manage the `lastSeenAt` cursor for the events lane (resume `?since=<rfc3339>` on EventSource error); handle `bead.updated` events that change `last_session_id` by closing+reopening the WS. |
| `gardenio/controller/cmd/beads-viewer/templates/static/agent_events.css` (new) | Styles for the 16 `ActivityLineType` cases: icons, color tokens, fail/crash row tint, squawk prose font. |
| `gardenio/controller/cmd/beads-viewer/agents_test.go` | Tests for SSE filter, event-row rendering per type, role-gate refusal (403 when `RolesFromContext` is empty in OIDC-on env), input proxy success path. Reuse the slack-bridge `rbac_test.go` pattern. |
| `gardenio/controller/cmd/beads-viewer/dev_auth.go` | Audit-log: stamp `email=local-dev@<host>` on bypass writes (R3 OQ-3e) so the canonical query never sees `email=<unset>`. |

**Acceptance.**

- Operator role: page loads in read-only; clicking "Take control" reveals the input
  form with the `[WRITE ✏ <email>]` indicator. Typing + Send delivers `text` to the
  PTY (visible in xterm output).
- Viewer role: clicking "Take control" returns 403 with a one-line toast
  ("Operator role required to send commands"); no input form is exposed.
- Bead-events SSE renders one row per matching event with the right icon/color;
  Slack-mode toggle filters as in iOS.
- Audit log line is emitted on every POST, with the right fields, in both write and
  bypass-dev paths. Verified with a `level=INFO event=agent_terminal_write` grep on
  beads-viewer logs.
- Agent rotation: kill+respawn the agent; the open page reattaches the WS to the new
  session_id without a manual reload (verified end-to-end on gardenio-rwx).

**Effort.** ~4 days. Two transport reconnect paths plus the role gate and audit log
are the bulk; the SSE proxy itself is one short handler that mirrors
`bridge/api_decisions_sse.go`.

### Phase 3 — Keys row + nudge + respond + destructive-key confirmation

**Goal.** Ship the remaining R3 verbs and their UX guardrails.

**Files touched.**

| File | Change |
|---|---|
| `gardenio/controller/cmd/beads-viewer/agents.go` | New handlers: `POST /agents/{id}/keys` (CSV `keys` form → coopmux `/input/keys`); `POST /agents/{id}/nudge`; `POST /agents/{id}/respond`. All wrapped in `RequireRole(RoleOperator)` and the same audit-log pattern. |
| `gardenio/controller/cmd/beads-viewer/templates/partials/type_agent.html` | Keys toolbar (`Esc`, `Tab`, `↑`, `↓`, `Enter`, `Ctrl-C ⚠`, `Ctrl-D ⚠`, `Ctrl-\\ ⚠`, custom-key form). Nudge button. Respond modal triggered by `agent_state=ask_user` event from the events lane. |
| `gardenio/controller/cmd/beads-viewer/templates/partials/agent_confirm_destructive.html` (new) | Server-rendered confirmation fragment (`hx-confirm` is not enough — message must adapt to key + session ID). |
| `gardenio/controller/cmd/beads-viewer/cmdk.go` | Register agent-detail palette items: send-keys / nudge / respond / open-transcript / catch-up. Each is a thin wrapper around the htmx routes. |
| `gardenio/controller/cmd/beads-viewer/agents_test.go` | Tests for keys CSV parsing, destructive-key audit (cancel logs `result=ux_canceled`), respond modal gating on `ask_user` state. |

**Acceptance.**

- Toolbar keys deliver to the PTY. Destructive keys show the inline confirmation
  fragment; cancel emits an audit line with `result=ux_canceled` and no upstream
  call; confirm emits the standard write line.
- Nudge button works; visible side-effect on the agent.
- Respond modal appears only when an `ask_user` event lands and dismisses on send.
- `Cmd-K` palette includes the new agent-detail entries; each one round-trips
  through the same htmx route as the toolbar (palette is sugar, not a parallel API).

**Effort.** ~3 days.

### Phase 4 — Polish + audit hardening + accessibility + follow-ups

**Goal.** Close the long tail from R2/R3/R5: a11y, font-size selector, hover-for-cwd,
deep-links, search, OQ verifications.

**Files touched.**

| File | Change |
|---|---|
| `gardenio/controller/cmd/beads-viewer/templates/partials/type_agent.html` | Font-size cookie (small/medium/large); hover `<span title="...">` for cwd; auto-link bead IDs in squawk rows; per-transport health dots in the toolbar (PTY-WS + events-SSE); fast-forward "catching up…" pill UX. |
| `gardenio/controller/cmd/beads-viewer/middleware.go` | Mobile defaults: smaller scrollback fetch and queue high-watermark when `User-Agent` matches a coarse-pointer media query (or feature-detect via a query param the JS sets). |
| `gardenio/controller/cmd/beads-viewer/templates/static/agent_terminal.js` | Page Visibility API: hold WS open when hidden; force a hard reconnect on resume from background after >30 s. xterm `screenReaderMode` mirror to a sibling `aria-live="polite"` region. |
| `gardenio/controller/cmd/beads-viewer/agents.go` | Persist filter mode (`?mode=slack` ↔ cookie `agent_feed_mode`) and density (`agent_feed_density`). |
| `gardenio/controller/cmd/beads-viewer/static/agent_terminal.css` (or inline) | Keyboard-focus ring on the terminal container; respect `prefers-reduced-motion` for xterm cursor blink. |
| `gardenio/docs/beads-viewer-dev.md` | Document local dev for the agent terminal: port-forward coopmux, set `COOPMUX_URL=http://localhost:<port>` + `COOP_MUX_AUTH_TOKEN` from the secret, `BEADS_VIEWER_DEV_AUTH_BYPASS=1`. |
| `tests/playwright/agent_terminal.spec.ts` (or wherever the beads-viewer Playwright suite lives) | E2E: load page, see screen fallback, see WS attach, send keystroke, see audit-log line. Screen-reader assertion: `aria-live` region reflects PTY content. |

**Acceptance.**

- A11y check: keyboard-only navigation reaches the terminal and can read scrollback
  via the live region.
- Font-size + filter-mode cookies persist across reloads.
- Operator-friendly degraded states: events SSE down + PTY WS up renders with
  "events stale" badge; vice versa.
- Open question OQ-1 (oauth2-proxy cookie scope across `coopmux.<host>` and
  `beads-viewer.<host>`) verified in writing on a deployed gardenio-rwx — captured in
  this phase's MR description, not just in chat.

**Effort.** ~3 days.

### Out-of-band follow-ups (file alongside phase 1, not blocking)

These are R5-flagged "out of scope for the detail view" items the phase-1 child
should file as separate beads so the human approver sees them but they do not
block phase 1:

- **F-1.** "Respawn" button in the agent header strip (iOS swipe-to-respawn, but on
  the *list* row — out of scope for detail-view parity; consider as a future
  ergonomics ticket).
- **F-2.** Side-by-side compare (split-pane in `bead_show.html` or persistent
  tab-strip). Two browser tabs is enough at v1.
- **F-3.** xterm.js Search addon (`Ctrl-F` already searches the rendered DOM at v1).
- **F-4.** coopmux-side persistence of `X-Gardenio-User` / `X-Gardenio-Roles` in the
  per-session input log (R3 OQ-3d). Lives on the coop crate, not beads-viewer.
- **F-5.** Per-bead writer allow-list on top of the operator role (R3 OQ-3a).

## Total effort and sequencing

- Phase 1 alone ships the parent epic's headline feature (a real PTY pane).
  ~3 days, ships independently.
- Phase 2 is the iOS-parity event feed + write surface. ~4 days, ships
  independently after phase 1.
- Phase 3 is the rest of the write affordances. ~3 days, ships independently
  after phase 2.
- Phase 4 is a11y + polish. ~3 days, ships independently after phase 3.

Total: ~13 working days across four MRs, four beads, four release windows. Each
phase is independently rollback-safe and gated by its own acceptance.

## Decisions that are NOT being asked for here

This plan does not propose changes to:

- coopmux's HTTP/WS API surface (per parent-epic anti-scope).
- The Keycloak realm / role hierarchy (`gardenio-viewer` / `-operator` / `-admin`
  unchanged).
- The deployment topology (no new ingress, no new state backend).
- The iOS Gardenio app (one-way mapping iOS → viewer).
- beads3d (out of scope; this work is beads-viewer only).

## Appendix — `gb decision create` invocation

The decision below is the human go/no-go on cutting the **phase 1** child task. The
options carry `artifact_type` per the global decision-interface rules. Phases 2–4
are queued behind phase 1 — a separate decision (or an explicit "cut all four" in
the option `label`) can authorize the rest later.

```bash
gb decision create --no-wait \
  --prompt="R6 plan-of-record committed in MR for ka-0000001758. Recapped R1–R5 ADRs
and phased implementation children (phase-1 skeleton + read-only PTY stream → phase-2
input + bead-events lane → phase-3 keys/nudge/respond → phase-4 polish/audit/a11y).
Each phase has file-level scope and rough effort (~3–4 days each).

Phase 1 cuts the headline feature: live PTY in xterm.js for /agents/{id}, read-only,
behind the existing OIDC auth. No writes yet; no bead-events lane yet.

Approve cutting the phase-1 task bead now?" \
  --options='[
    {"id":"approve","short":"Approve phase 1","label":"Cut the phase-1 child task (skeleton template + read-only PTY stream) and assign it to a beads-viewer crew. Phases 2-4 queued behind it.","artifact_type":"epic"},
    {"id":"defer","short":"Defer / questions","label":"Hold; produce a written report addressing the specific concerns (state them in the decision response). No child task cut yet.","artifact_type":"report"},
    {"id":"split","short":"Split phase 1","label":"Phase 1 is too large; split it (e.g. skeleton template + screen fallback first; xterm + WS in a follow-up). File the split as a 2-child epic.","artifact_type":"epic"}
  ]'
gb yield
```

## Acceptance for this plan

- [x] Single committed plan-of-record at `gardenio/plans/beads-viewer-agent-terminal.md`.
- [x] Each ADR (R1–R5) recapped in 2–3 lines with a pointer to the full doc.
- [x] Four phased implementation children listed with **file-level** scope, acceptance
      criteria, and rough effort — not vague themes.
- [x] Out-of-band follow-ups enumerated so the human approver sees them but they
      do not block phase 1.
- [x] Exact `gb decision create` invocation in the appendix, with `artifact_type`
      on every option, prompts and labels concrete enough to act on.
- [x] No production Go / template / JS code touched in this MR.
