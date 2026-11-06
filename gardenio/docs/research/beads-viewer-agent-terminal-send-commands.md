# ADR — beads-viewer agent terminal: send-commands UX + authorization

- **Bead:** [ka-0000002915](https://beads.gardenio.e2e.example.com/ka-0000002915)
- **Parent epic:** [ka-0000003802](https://beads.gardenio.e2e.example.com/ka-0000003802) — beads-viewer iOS-parity Phase 3 (agent-terminal custom view)
- **Depends on:** [ka-0000006916](https://beads.gardenio.e2e.example.com/ka-0000006916) — R1 architecture ADR (`beads-viewer-agent-terminal-architecture.md`)
- **Status:** Proposed (research-only; no implementation until human green-lights via `gb decision`)
- **Scope:** Defines *which* coopmux input verbs the browser exposes, *who* is allowed to send them, and *how* writes are audited. Excludes: renderer choice (R2), event interleaving (R4), iOS parity (R5), implementation plan (R6).

## TL;DR

> **Recommendation.** Read-only by default. Adopt a per-page **write toggle**
> ("Take control") gated by the existing `gardenio-operator` Keycloak role from
> `internal/oidcauth`. Beads-viewer exposes four POST verbs at
> `/agents/{id}/{verb}` — `input`, `keys`, `nudge`, `respond` — and proxies
> them to the corresponding coopmux endpoints with the static
> `COOP_MUX_AUTH_TOKEN`. Identity is stamped into beads-viewer's own structured
> access log on every write (the minimum-viable audit) and forwarded
> upstream as `X-Gardenio-User: <email>` + `X-Gardenio-Roles: <csv>` so a
> follow-up coopmux task can persist it into session logs without changing
> this ADR. Destructive raw bytes (Ctrl-C, Ctrl-D, Ctrl-\\) require a click
> confirmation; `/input/raw` is **not** wired in v1 (key sequences are
> covered by `/input/keys`).

The trade-off: we expose a smaller surface than coopmux supports (`/input/raw`
is omitted) and we add a per-session client-side toggle plus a destructive-key
confirmation step that the iOS app does not have. In return: a single
authorization gate, one place to log every keystroke, and zero coopmux
changes — consistent with the parent epic's anti-scope and with R1's load-
bearing claim that "audit/policy lives in beads-viewer."

## Context

### What R1 already pinned

Per the R1 ADR (Hybrid path), control verbs flow:

```text
browser ─► beads-viewer.<host>/agents/{id}/<verb>          [OIDC middleware]
            └─► gardenio-coopmux.<ns>.svc.cluster.local/api/v1/sessions/{id}/<verb>
                  Authorization: Bearer ${COOP_MUX_AUTH_TOKEN}
```

The user's JWT is validated by `internal/oidcauth.Middleware` and email +
roles land in `r.Context()` via `EmailFromContext` / `RolesFromContext`. The
`COOP_MUX_AUTH_TOKEN` is a single static M2M credential mounted into the
beads-viewer pod; it is not per-user and never leaves the pod. This ADR
relies on that path being decided — every write verb here is the Go-server
half of an htmx round-trip, never a direct browser-to-coopmux call.

### Coopmux input surface (today, unchanged by this ADR)

From `coop/crates/mux/src/transport/mod.rs:76-80` and
`coop/crates/mux/src/transport/http.rs:653-695`:

| Coopmux endpoint | Body shape | Purpose |
|---|---|---|
| `POST /api/v1/sessions/{id}/input` | `{"text": "..."}` (JSON) | Typed text — no special interpretation. |
| `POST /api/v1/sessions/{id}/input/raw` | `{"data": "<base64>"}` (JSON) | Raw bytes — bypasses key parsing. |
| `POST /api/v1/sessions/{id}/input/keys` | `{"keys": ["Ctrl-C", "Enter", ...]}` (JSON) | Symbolic key sequence; the upstream parses to bytes. |
| `POST /api/v1/sessions/{id}/agent/nudge` | agent-defined JSON | High-level "wake the agent" prod. |
| `POST /api/v1/sessions/{id}/agent/respond` | agent-defined JSON | Structured reply (e.g. resolves an `ask_user`). |

All five are forwarded by coopmux to the upstream session via `proxy_post`;
coopmux itself does not enforce a write/read distinction at the role level
— it accepts any caller carrying the static token *or* a valid OIDC JWT
(`coop/crates/mux/src/transport/auth.rs:68-86`). That is the gap this ADR
closes inside beads-viewer.

### Existing role hierarchy (from `internal/oidcauth/oidcauth.go:227-254`)

```
gardenio-viewer    (1)  read-only — bead/agent views, prime, logs
gardenio-operator  (2)  send-commands; mutate beads (priority, status, comment)
gardenio-admin     (3)  destructive ops; advice/role administration
```

`HasMinimumRole(roles, required)` is the existing helper; admin satisfies
operator, operator satisfies viewer. No new role is invented in this ADR.

### Anti-scope

- **No coopmux protocol changes.** `/input`, `/input/raw`, `/input/keys`,
  `/agent/nudge`, `/agent/respond` are consumed as-is. If a future
  enhancement requires e.g. a per-write user-identity body field, that
  belongs on a separate beads-viewer-blocked-by-coop task.
- **No new Keycloak role.** Reusing `gardenio-operator` keeps the RBAC story
  in one place (oidcauth + slack-bridge already gate at the same boundary).
- **No per-bead ACL invention.** Bead-label or owner-based policy is
  considered below and explicitly deferred to a follow-up.
- **No implementation.** This ADR only defines what the implementation
  must do; R6 rolls the design into a concrete plan.

## Input surface — what the viewer exposes

The viewer exposes **four** POST verbs, all rooted at `/agents/{id}/...` and
all htmx-friendly (form-encoded → fragment response). The session ID is
resolved from the agent bead's `last_session_id` field at request time
(per R1).

| Beads-viewer endpoint | Method | Form fields | Upstream target | UI affordance |
|---|---|---|---|---|
| `/agents/{id}/input` | POST | `text` (string) | `POST /api/v1/sessions/{id}/input` `{"text":...}` | Bottom-of-page text input + Enter or "Send" button. |
| `/agents/{id}/keys` | POST | `keys` (CSV: `Ctrl-C` / `Enter` / `Up` / ...) | `POST /api/v1/sessions/{id}/input/keys` `{"keys":[...]}` | Toolbar buttons + custom-key form (with confirmation for destructive set). |
| `/agents/{id}/nudge` | POST | (no body, or freeform `note` field) | `POST /api/v1/sessions/{id}/agent/nudge` | Single "Nudge" button in the agent toolbar. |
| `/agents/{id}/respond` | POST | `response` (string) — typed answer to current `ask_user` | `POST /api/v1/sessions/{id}/agent/respond` `{"response":...}` | Modal that surfaces only when the agent is in `ask_user` state. |

**`/input/raw` is intentionally omitted from v1.** Raw byte injection
bypasses key parsing and is the easiest way to corrupt a session
(unmatched escape sequences, misframed mouse reports). The
operator-shaped use cases — Ctrl-C, Ctrl-D, arrow keys, page up/down — are
all expressible via `/input/keys`. If a future task identifies a need for
raw bytes (e.g. paste a literal escape sequence into a tmux test), it is
filed as a separate bead with its own justification.

### Default posture: read-only

A user who can see the agent terminal can **read** indefinitely. Sending
*anything* — text, key, nudge, respond — requires:

1. The user's JWT carries the `gardenio-operator` realm role
   (`HasMinimumRole(roles, RoleOperator)`).
2. The user has explicitly clicked the **Take control** toggle on the page,
   which scopes the write capability to that page session.
3. (For destructive keys only) a one-click confirmation in a small inline
   prompt.

The toggle is a *UX guardrail*, not a security boundary. Defense-in-depth
is the role gate enforced server-side in beads-viewer middleware; the
client toggle exists so an operator who is sharing a screen, demoing the
viewer, or just reading along does not turn a misclick into a `Ctrl-C` to
production. This mirrors the iOS app's "Send mode" but with the toggle
visible in the page header rather than hidden behind a long-press.

### Per-bead permission gate — deferred

In principle a per-bead label (`ops-only`, `do-not-touch`) or per-owner
gate ("only the gardener who spawned this agent can write to it") could
narrow the operator role to a subset of beads. This ADR **does not**
adopt that — three reasons:

1. There is no existing label vocabulary for it; inventing one here
   couples R3 to a labels-policy decision the team has not made.
2. The agent bead already carries `owner` and `assignee`; if a future
   policy wants "gardener or owner only" we have the data without an ADR
   change here.
3. The audit trail captures *who* wrote *what* in real time. Mis-writes
   are recoverable (Ctrl-C the gardener, restart the agent) but a
   per-bead gate that blocks the on-call operator during an incident is
   not. We err on the operational-flexibility side and rely on audit
   plus role to keep blast-radius bounded.

**Open question OQ-3a:** Gardener-cycle agents currently spawn via
scheduled triggers without a stable human owner. If the team decides a
per-bead "writer" allow-list is required, this is the single place it
should land — as a thin check in the same middleware, before the role
gate. Filed as a follow-up rather than a blocking question for v1.

### Confirmation UI for destructive keys

The viewer keeps a **destructive key list** that the client confirms
inline before submitting `/agents/{id}/keys`:

```
Ctrl-C   Ctrl-D   Ctrl-\\   Ctrl-Z   Esc Esc Esc   :q!\n   :wq!\n
```

The list lives in the page (data attribute on the toolbar) so adding a
key does not require a server change. The server-side handler does not
re-check destructiveness — confirmation is purely a UX guardrail. The
audit log records the *raw* key payload, including any `Ctrl-C`, so a
miscategorized key is still attributable.

## Authorization mapping

### Endpoint → role

| Beads-viewer endpoint | Minimum role | Rationale |
|---|---|---|
| `GET /agents/{id}` (page) | `gardenio-viewer` | Already today; read-only. |
| `GET /agents/{id}/screen` (snapshot) | `gardenio-viewer` | Read-only equivalent of WS catchup. |
| `GET /agents/{id}/output` (catchup, R4) | `gardenio-viewer` | Read-only. |
| `POST /agents/{id}/input` | `gardenio-operator` | Write. |
| `POST /agents/{id}/keys` | `gardenio-operator` | Write (incl. destructive). |
| `POST /agents/{id}/nudge` | `gardenio-operator` | Write (semantic). |
| `POST /agents/{id}/respond` | `gardenio-operator` | Write (semantic). |

The four POST routes are wrapped with `oidcauth.RequireRole(RoleOperator,
logger)` (the existing helper at `internal/oidcauth/oidcauth.go:260`).
Behavior matches what `slack-bridge/main.go:795-816` does for
`POST /api/decisions/...` and `POST /api/escalations/...`: when OIDC is
configured but the request carries no roles, the request is denied
**403** (do not fall open — that was the C1 bypass referenced in the
slack-bridge tests). When OIDC is disabled (local dev with
`BEADS_VIEWER_DEV_AUTH_BYPASS=1`), the bypass middleware allows the
request through and the access log records `email=<unset>
roles=<bypass>`. The bypass refusal in a prod-like env
(`dev_auth.go:42-47` — SA token mounted) still applies.

### Why operator and not admin?

- Slack-bridge already gates *operator* on `POST /api/decisions/`. A
  human resolving a decision and a human nudging the same agent are
  the same operational role. Aligning on operator avoids inventing a
  third tier.
- `gardenio-admin` is reserved for advice/role-administration and
  destructive cluster ops. Sending Ctrl-C to an agent — while
  destructive in the agent's local sense — is bounded to a single
  session and is the operator-on-call's day job.

### Why not owner-only / gardener-only?

Gardener-cycle agents are spawned by schedules
(`sched-beads-viewer-gardener-sin-...`) without a stable human owner.
"Owner-only" would require the page to be aware of *spawn lineage*, not
just bead ownership; that data is not consistently surfaced today. An
"owner-or-operator" policy would still allow operator overrides for
on-call and is therefore equivalent to "operator" for the v1 cut.
Per-bead allow-list (OQ-3a) is the right place to add a tighter scope
later if it proves necessary.

### Bypass / dev story

Local dev with `BEADS_VIEWER_DEV_AUTH_BYPASS=1`:

- Browser → beads-viewer carries no JWT; `RolesFromContext` returns
  `nil`; per `RequireRole`, "no roles in context" is allowed through
  (treated as internal/unauthenticated caller). The audit log entry
  records `email=<unset> roles=<bypass>` and the request is forwarded.
- This matches the existing dev path for every other write in the
  viewer (priority, status, comment, etc.). It is acceptable because
  the bypass refuses to run in a prod-like env (SA token mounted).

The bypass refusal is **load-bearing** for this ADR — without it the
operator gate has a "no roles = allow" passthrough that Keycloak-on
deployments must trust. The slack-bridge tests
(`cmd/slack-bridge/rbac_test.go:153`) already encode the lesson; the
beads-viewer wiring must do the same.

## Audit trail — minimum-viable

### Requirement

> Every write to a coopmux session originating from the browser is
> attributable to a Keycloak-authenticated user (or the explicit dev
> bypass), with timestamp, target session ID, verb, and payload size.

The phrase "minimum-viable" is doing real work here: it explicitly does
*not* require coopmux to persist user identity in its own session log
in v1. Coopmux today sees only the static M2M token; persisting the
forwarded `X-Gardenio-User` header into its log is a coop-crate change
and out of scope per the parent epic.

### Where identity is stamped

Two layers:

1. **Beads-viewer structured access log (canonical).** Every POST handler
   logs at INFO level on the request path:

   ```
   level=INFO
   event=agent_terminal_write
   email=<EmailFromRequest>
   roles=<comma-separated RolesFromContext>
   bead_id=<{id}>
   session_id=<resolved last_session_id>
   verb=input|keys|nudge|respond
   bytes=<len(payload)>
   keys=<symbolic keys, only for /keys verb>
   ts=<RFC3339Nano>
   request_id=<X-Request-Id from middleware>
   result=ok|denied_role|denied_bypass_disallowed|upstream_5xx|...
   ```

   This is the source of truth. It survives coopmux restarts. It
   already lives next to all other beads-viewer writes (priority,
   status, comment) and uses the same logger configured by
   `dev_auth.go` and `middleware.go`.

2. **Forwarded HTTP headers (for future coopmux session log).** When
   beads-viewer makes the upstream `proxy_post` call, it adds:

   - `X-Gardenio-User: <email>`
   - `X-Gardenio-Roles: <comma-separated>`
   - `X-Gardenio-Request-Id: <request_id>`

   Coopmux does **not** read these in v1. Adding read-side handling
   (persisting them into the per-session input log) is a clean
   follow-up that does not require changing this ADR — coopmux's
   `proxy_post` (`coop/crates/mux/src/transport/http.rs:597-650`)
   today forwards the body and ignores extra headers, which is the
   shape we want until the persistence task lands.

### Where identity is NOT stamped

- **Not in the URL.** A path like `/api/v1/sessions/{id}/input?user=alice@`
  is logged by every intermediate proxy and is the most common way to
  leak PII into shared logs. Reject.
- **Not in the request body.** Mutating the JSON payload sent to coopmux
  to wedge in a `_user` field would be a coopmux protocol change (it
  would surface in the upstream agent's input stream as garbage on
  any session that does not strip it). Reject.
- **Header is the right place** because it is invisible to the upstream
  agent's PTY, attaches to exactly the request, and is already how
  `X-Auth-Request-Email` flows through oauth2-proxy.

### What the audit covers in v1

- The four POST verbs from beads-viewer.
- The `WS direct` read leg is *not* logged by beads-viewer
  (it does not pass through the Go server). Coopmux's existing access
  log records the OIDC verifier's accepted JWT for that leg; reads are
  read, not writes — read-side audit is out of scope.
- Failed requests (403, 503) are logged at WARN with the same fields
  except `bytes`/`keys` (those reflect the post-validation payload).

### Retention / shipping

Beads-viewer logs already ship to the cluster log aggregator (via the
existing pod logging path). No new sink is introduced. The
`event=agent_terminal_write` field is deliberately distinctive so a
future Mezmo / Loki query (`event:agent_terminal_write user:<email>
since:<t>`) is one filter away.

## UX sketches

All sketches assume the R1-decided page layout: bead header at the top,
streaming terminal below, control affordances at the bottom. Sketches
omit the constellation/sidebar chrome shared across `bead_show.html`.

### 1. Read-only default (operator viewing)

```
┌──────────────────────────────────────────────────────────────────┐
│ ka-aBc123  agent: gardenio-feature-delivery-...   [READ-ONLY 🔒]  │
│ session: sess-7c1a98  status: running  account: gardenio-claude   │
├──────────────────────────────────────────────────────────────────┤
│  ▶ /home/agent/bot ❯ kd ready --type=task                        │
│  kd-... [task] (priority=1) ...                                  │
│  kd-... [task] (priority=2) ...                                  │
│  ▶ /home/agent/bot ❯ █                                           │
│                                                                  │
│  (live PTY stream, scrollback ↑)                                 │
├──────────────────────────────────────────────────────────────────┤
│ [ Take control ]   [ Open transcript ]   [ Catch up ]            │
└──────────────────────────────────────────────────────────────────┘
```

**[READ-ONLY 🔒]** is the resting state for everyone. The "Take
control" button is rendered for every visitor, but clicking it makes
an `hx-post` to `/agents/{id}/control/claim` which the server rejects
**403** for non-operators — the rejection toast is the cue that the
user lacks the role. (Hiding the button entirely was considered and
rejected: a button that vanishes is a UX bug report waiting to happen.
A button that explains *why* you cannot click it — "Operator role
required to send commands" — is friendlier.)

### 2. Write-mode (operator after Take-control)

```
┌──────────────────────────────────────────────────────────────────┐
│ ka-aBc123  agent: gardenio-feature-delivery-...   [WRITE ✏ alice@]│
│ session: sess-7c1a98  status: running  account: gardenio-claude   │
├──────────────────────────────────────────────────────────────────┤
│   (live PTY stream, scrollback ↑)                                │
│   ▶ /home/agent/bot ❯ █                                          │
├──────────────────────────────────────────────────────────────────┤
│ Send: ┌────────────────────────────────────────────────────┐ [↵] │
│       │ kd ready --type=task                               │     │
│       └────────────────────────────────────────────────────┘     │
│ Keys: [Esc] [Tab] [↑] [↓] [Enter] [Ctrl-C ⚠] [Ctrl-D ⚠] [⋮ more] │
│ Agent: [ Nudge ] [ Respond… ]    [ Drop control ]                │
└──────────────────────────────────────────────────────────────────┘
```

Indicators:

- The header pill changes from `[READ-ONLY 🔒]` to
  `[WRITE ✏ <email>]`. The writer's email is *visible to other
  viewers of the same page* if a future R4 multiplex broadcasts a
  presence event — for v1, the indicator is local-only (each viewer
  sees their own state).
- Keys marked `⚠` open the inline confirmation prompt below.

### 3. Destructive-key confirmation

```
┌──────────────────────────────────────────────────────────────────┐
│ Send Ctrl-C to sess-7c1a98?                                      │
│ This will interrupt the running command in the agent's terminal. │
│ [ Cancel ]                              [ Send Ctrl-C ]          │
└──────────────────────────────────────────────────────────────────┘
```

The confirmation is rendered server-side (htmx swap on the
toolbar's `Ctrl-C` button: `hx-confirm` is *not* sufficient because
the message must adapt to the key being sent and the resolved
session ID). Confirming POSTs to `/agents/{id}/keys` with
`keys=Ctrl-C`. Audit log records the keystroke whether the user
confirmed or canceled (cancel logs `result=ux_canceled` with no
upstream call).

### 4. Respond modal (gated to ask_user state)

```
┌──────────────────────────────────────────────────────────────────┐
│ Agent is asking:                                                 │
│ "Two options for resolving the merge conflict in foo.go..."      │
│                                                                  │
│ Your response: ┌────────────────────────────────────────────┐    │
│                │ option B; rebase onto main first           │    │
│                └────────────────────────────────────────────┘    │
│ [ Cancel ]                              [ Respond ]              │
└──────────────────────────────────────────────────────────────────┘
```

Triggered by the WS event stream surfacing `agent_state=ask_user` (the
event shape is decided in R4). The modal POSTs to
`/agents/{id}/respond` with `response=<text>`. Closing without
sending leaves the agent in `ask_user` (operator nudges or types
manually instead).

### 5. Command palette (`Cmd-K` style; reuse existing `cmdk`)

```
┌──────────────────────────────────────────────────────────────────┐
│ ⌘K  type a command…                                              │
│                                                                  │
│  send keys ›  Ctrl-C                                             │
│  send keys ›  Esc Esc Esc                                        │
│  send keys ›  Down Down Down Enter                               │
│  agent     ›  Nudge                                              │
│  agent     ›  Respond                                            │
│  view      ›  Open transcript                                    │
│  view      ›  Catch up from offset                               │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

`cmdk.go` already exists in the viewer (palette pattern); the
agent-terminal page registers the entries above as palette items.
Each item is a thin wrapper around the same htmx routes as the
toolbar — palette is UX sugar, not a parallel API.

## Open questions (filed downstream, not blocking this ADR)

| ID | Question | Affects |
|---|---|---|
| OQ-3a | Should we add a per-bead writer allow-list (label `writers:alice@,gardener-foo`) on top of the operator role? Gardener-cycle agents have no stable human owner. | Future iteration; thin middleware addition |
| OQ-3b | Should destructive-key confirmation skip when the writer holds `gardenio-admin`? Mirrors the "expert-mode" toggle in some IDEs. | Pure UX |
| OQ-3c | Multi-writer collisions: two operators take control on the same page. Is "last-write-wins" acceptable, or do we need a soft lock? | R4 (presence/event interleaving) |
| OQ-3d | When (not if) coopmux persists `X-Gardenio-User`, do we emit a coopmux-side "user identity" record per write, or annotate the existing input log entry? | Separate coop-crate task |
| OQ-3e | `BEADS_VIEWER_DEV_AUTH_BYPASS=1` audit log says `email=<unset>`. Should the local dev path stamp `email=local-dev@<host>` instead, so the field is never empty in the canonical log query? | Hygiene |

## Decision

**Adopt the design above.** Reasoning:

1. **One gate, one log.** The role gate sits in beads-viewer middleware
   (the existing `RequireRole(RoleOperator)`); the access log sits in
   the same handlers. R1 paid for this property explicitly; this ADR
   spends it.
2. **Reuse the existing role hierarchy.** `gardenio-operator` already
   gates write-shaped routes on slack-bridge. Inventing a fourth role
   ("agent-writer") would split the operator concept across two
   bridges that mean the same thing.
3. **The toggle is UX, not security.** Read-only-by-default with an
   explicit "Take control" toggle prevents misclicks and signals
   intent in screen-shares and demos. Removing the toggle would not
   weaken the gate; adding it does not strengthen the gate. The role
   gate is the security boundary.
4. **`/input/raw` is omitted on purpose.** Raw bytes have no
   operator-shaped use case that key sequences cannot serve, and
   they are the most foot-gun-shaped verb of the five. We can add
   them later behind the same gate if a concrete need surfaces.
5. **Header-stamped identity is a clean handoff to coopmux.** If the
   team later wants user-attributable session logs in coopmux itself,
   the upstream call already carries the headers; the coop-crate
   change is read-only and small.

If R4 decides bead events and PTY output share a single multiplexed
WS, this ADR is **unaffected** — control verbs still go through the
beads-viewer Go server. If R4 decides keystrokes flow over the WS
(rather than `POST /input`), this ADR must be revisited because the
audit and authorization gate move from the Go handler to a WS message
filter — a strictly larger surface area than v1 contemplates.

## Acceptance for this ADR

- [x] Input surface enumerated against coopmux's actual route table
      (`mod.rs:76-80`, `http.rs:653-695`).
- [x] Default posture stated: read-only with explicit write toggle and
      destructive-key confirmation.
- [x] Authorization model mapped to `internal/oidcauth` roles
      (`gardenio-operator`) and reviewed against existing
      `RequireRole` usage in `slack-bridge/main.go:795-816`.
- [x] Audit-trail requirement explicit (canonical log in beads-viewer;
      header-forwarded for future coopmux persistence) and minimum-
      viable.
- [x] Input UX sketches (ASCII/wireframe) for: text input, raw key
      with confirmation, nudge, respond, plus command palette and
      read-only state.
- [x] No Go / template / JS / coopmux code changes proposed.
