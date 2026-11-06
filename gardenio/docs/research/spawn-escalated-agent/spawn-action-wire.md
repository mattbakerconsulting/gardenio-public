# Spawn-action wire & UI

Research design for the "spawn escalated agent" affordance.
Epic: ka-0000006791.
Task: ka-0000004798.
Sister epic: ka-0000004527 (escalations surface).
Sibling research (in-flight): ka-0000005882 (authn/authz), ka-0000006867 (daemon lifecycle).

**Status: research-only. No implementation.** Concrete shape moves to a
`gb decision` at phase-2 synthesis.

## TL;DR

- Add a "Spawn locally" affordance on `/escalations` rows and
  `/decisions/<id>` option-pick widget that POSTs a typed action
  through beads-viewer to the operator's local `gb` daemon.
- Action is a typed, **server-signed** payload (decision/escalation
  id, recommended ref, working-dir hint, options snapshot).
  Daemon replies with a `spawn_id` and a local-session handle.
- Idempotency: server mints a deterministic `spawn_id` per
  (operator, decision_id, recommended_ref); re-clicking returns the
  existing spawn instead of double-spawning. Stored on the controller
  as a `spawn_intent` bead linked to the decision.
- Audit: every spawn is recorded as a **`type=spawn-intent`
  child-bead** of the originating decision (canonical), with a
  back-mention comment for surface visibility.
- States (idle / spawning / spawned / failed / daemon-offline) are
  rendered by a single free-htmx widget (`spawn-action-button`), polled
  via `hx-trigger="every 2s"` while in-flight.
- Slack is a stretch: same payload shape over the existing
  bridge-interactions channel, with daemon discovery via the operator's
  `gb daemon ping` registration to the bridge.

## Surfaces

Three call-sites; all share one payload schema and one widget.

| Surface                          | Trigger                              | Confirmation         |
|----------------------------------|--------------------------------------|----------------------|
| `/escalations` row               | "Spawn locally" inline button        | inline (no dialog)   |
| `/decisions/<id>` option strip   | "Spawn locally" extra chip per opt   | dialog (working dir) |
| `bead_show.html` for decision    | same chip as above                   | dialog               |
| Slack escalation message (str.)  | "Spawn locally on my laptop" button  | inline Slack swap    |

Why dialog on `/decisions/<id>` but not on `/escalations`: escalations
are top-of-funnel (operator is acknowledging "I'll handle this on my
machine"); decisions need the operator to pick an option **and**
optionally override working dir / ref. Defer the dialog until we hit
that path.

## Payload schema

### `POST /b/<decision-id>/spawn-local` — controller intake

The viewer / Slack handler sends this to **beads-viewer** (not directly
to the daemon — the controller is the trust anchor for signing the
forwarded payload). Fields in the form-encoded request:

| Field             | Required | Notes                                           |
|-------------------|----------|-------------------------------------------------|
| `option_id`       | iff decision | which option the operator is electing       |
| `working_dir`     | optional | override of project-default; daemon validates   |
| `ref`             | optional | override of `recommended_ref`                   |
| `client_nonce`    | required | UUIDv4 from browser; idempotency key            |
| `daemon_id`       | optional | if operator has >1 daemon (laptop + workstation)|

### Controller → daemon (signed envelope)

The controller signs and forwards a typed JSON payload over the
operator's daemon channel (mTLS or signed WS — pin shape in
ka-0000005882). Schema (informal):

```text
{
  "spawn_id":              "spawn-<base32-12>",     // deterministic; see Idempotency
  "envelope_version":      1,
  "issued_at":             "2026-05-15T15:40:00Z",
  "expires_at":            "2026-05-15T15:45:00Z",  // 5-min TTL
  "operator":              "slack:U01ABCD",         // mapped to OIDC sub
  "source": {
    "decision_id":         "ka-0000004798",         // OR escalation_id
    "escalation_id":       null,
    "option_id":           "fix-locally",           // null for raw escalation
    "option_label":        "Reproduce and fix on laptop",
    "originating_agent":   "gardenio-foo-9b2",
    "parent_task":         "ka-0000006791"
  },
  "spawn": {
    "working_dir":         "/Users/op/code/gardenio", // resolved by viewer from config
    "ref":                 "agent/ka-0000004798-spawn-action-wire",
    "repo_url":            "git@gitlab.com:.../gardenio.git",
    "ref_kind":            "branch",                 // "branch" | "tag" | "sha" | "mr-head"
    "permissions_profile": "default-local",          // pinned in ka-0000004678
    "claude_settings_bead": "kd-...",                // see config-bead research ka-0000000166
    "prompt":              "Resume work on …"        // pre-templated continuation prompt
  },
  "controller": {
    "audit_bead_id":       "kd-<spawn-intent>",      // child of decision (see Audit)
    "callback_url":        "https://beads.gardenio…/spawn/spawn-<id>/status"
  },
  "signature":             "ed25519:base64…"         // see ka-0000005882
}
```

### Daemon → controller (response)

Synchronous reply on the action POST; daemon also `POST`s lifecycle
updates to `callback_url` as the session progresses.

```text
{
  "spawn_id":       "spawn-<id>",
  "state":          "spawning",                // see state diagram below
  "session_handle": "coop://local/sess-abcd",  // null until ready
  "local_url":      "http://127.0.0.1:9182/sessions/sess-abcd",
  "agent_id":       "local-laptop-op-9b2",     // joins the fleet under a "local:" lane
  "error":          null,
  "diagnostics": {
    "working_dir_exists":   true,
    "repo_clean":           true,
    "ref_checked_out":      true,
    "claude_settings_ok":   true
  }
}
```

### Spawn-intent bead (audit)

Child of the decision/escalation. Created by the controller
**before** forwarding to the daemon, so even a failed forward leaves
a trace.

```yaml
type: spawn-intent
kind: data
parent: ka-0000004798       # decision (or escalation)
labels: [area:gb-daemon, source:beads-viewer, project:gardenio]
fields:
  spawn_id:        spawn-abc12345
  operator:        slack:U01ABCD
  daemon_id:       laptop-op
  option_id:       fix-locally
  ref:             agent/ka-0000004798-spawn-action-wire
  working_dir:     /Users/op/code/gardenio
  state:           spawning           # updated by lifecycle callback
  local_url:       http://127.0.0.1:9182/sessions/sess-abcd
  local_agent_id:  local-laptop-op-9b2
  failure_reason:  null
```

### What this is NOT

- Not a config bead. Spawn intent is per-action and short-lived;
  config-bead surface is scoped in ka-0000000166.
- Not a `gb decision`. The decision being acted on is the parent;
  the spawn is the operator electing to handle it locally.
- Not a comment. We discussed comment-only audit and rejected it: a
  comment is hard to query (no fields), hard to count idempotency
  against, and gets lost in long threads. We post a back-mention
  **comment** in addition to the child bead, but the child bead is
  canonical.

## Idempotency model

`spawn_id = base32(sha256(operator || decision_id || option_id || ref)[:12])`

- Deterministic over the tuple — clicking twice yields the same id.
- Browser also sends `client_nonce` (per-mount UUID). The controller
  keeps a 60-second LRU of `(operator, client_nonce) → spawn_id`. A
  duplicate POST within that window returns the existing spawn_intent
  bead without forwarding to the daemon.
- After 60s, the deterministic id still de-dupes; re-clicks return the
  existing spawn_intent with current `state` and `local_url`. The UI
  treats this as "open existing session" rather than "spawn new".
- Operator can force a fresh spawn by selecting a different `ref`
  (which mutates the id) — same way we'd handle "I picked the wrong
  branch" without inventing a separate "respawn" button.

**Re-click decision tree** (controller-side):

```text
POST /spawn-local
  ├── client_nonce seen in last 60s?  ── yes → return cached response
  └── compute spawn_id
        ├── spawn-intent bead exists?
        │     ├── state ∈ {spawned, spawning} → return existing handle (no daemon hop)
        │     ├── state = failed              → 409 "already failed, force=1 to retry"
        │     └── state = daemon-offline      → forward again (treat as new attempt)
        └── no  → create spawn-intent bead, forward to daemon
```

## State diagram

```
                       ┌────────────────────────┐
              click    │                        │  callback (state=spawning)
   idle ───────────────►        spawning        ◄────────────────┐
     ▲                  │                        │                │
     │                  └─────┬──────┬───────────┘                │
     │                        │      │                            │
     │      spawn ok          │      │   daemon error             │
     │      (state=spawned)   │      └────►   failed              │
     │                        │                                  │
     │                        ▼                                  │
     │                  ┌───────────┐                            │
     │                  │  spawned  │── operator stops local ───►│
     │                  └─────┬─────┘                            │
     │                        │                                  │
     │   forward timeout      │                                  │
     │   (no daemon reply     │   local agent gb stop            │
     │    in 5s)              ▼                                  │
     │              ┌──────────────────┐                         │
     └─────── (auto)│  daemon-offline  │── retry click ──────────┘
                    └──────────────────┘
```

States (operator-visible chip text):

- `idle` — default; button reads "Spawn locally".
- `spawning` — controller has forwarded; awaiting daemon ack. Chip
  shows a spinner; button disabled. `every 2s` polling.
- `spawned` — daemon returned a handle. Button morphs into "Open
  local session ↗" linking to `local_url`.
- `failed` — daemon returned non-ok. Chip is red, tooltip shows
  `error`; button reads "Retry spawn".
- `daemon-offline` — controller could not reach the daemon (no
  socket, no ping in last 30s). Chip is amber; tooltip explains
  "Install `gb daemon` and run `gb daemon start` on your laptop";
  button reads "Recheck".

Closed/expired decisions and escalations hide the spawn affordance
entirely (no zombie "spawn for last week's decision"). 7-day TTL on
`spawn-intent` beads keeps the audit history bounded.

## Low-fi UI mocks

### `/escalations` row (after)

```
┌────────────────────────────────────────────────────────────────────────┐
│ Rotate RWX_ACCESS_TOKEN  [public]  [internal]  [in progress]           │
│  ka-0000001006 · gardenio · agent foo-9b2 · for ka-0000006791 · 3h ago  │
│  ┌──────────────────┐ ┌──────────┐                                     │
│  │ Spawn locally ⤓  │ │ Review › │                                     │
│  └──────────────────┘ └──────────┘                                     │
└────────────────────────────────────────────────────────────────────────┘
```

When in-flight:

```
│  ┌────────────────────────┐                                     │
│  │ ⟳ Spawning sess-abcd…  │  (spinner; disabled; hx-trigger 2s) │
│  └────────────────────────┘                                     │
```

When spawned:

```
│  ┌─────────────────────────────────────┐ ┌─────────────┐        │
│  │ ↗ Open local session  sess-abcd     │ │ Stop local  │        │
│  └─────────────────────────────────────┘ └─────────────┘        │
```

When daemon-offline:

```
│  ┌────────────────────────────────┐  ⓘ daemon not reachable    │
│  │ Recheck local daemon  (offline)│   install: `brew gb`       │
│  └────────────────────────────────┘                            │
```

### `/decisions/<id>` option-pick widget (existing)

The current option strip (decisions.html:36-49) renders option chips
plus a Dismiss chip. We add a **secondary action row** below it:

```
┌────────────────────────────────────────────────────────────────┐
│ Decision: Pick session strategy for ka-0000004798              │
│                                                                │
│  [ Use JWT ]  [ Use sessions ]  [ Defer ]  [ Dismiss ]         │
│  (existing — resolves server-side)                             │
│                                                                │
│  ─ or handle locally ─                                         │
│  [ Spawn locally with «Use JWT» ⤓ ]                            │
│  [ Spawn locally with «Use sessions» ⤓ ]                       │
│  [ Spawn locally with «Defer» ⤓ ]                              │
└────────────────────────────────────────────────────────────────┘
```

The "Spawn locally with «<opt>»" rows open a confirmation dialog
mirroring `decision-resolve-dialog` (decisions.html:65-82), but with
fields:

```
┌──────────────────────────────────────────────────────────┐
│ Spawn locally with «Use JWT»                             │
│                                                          │
│   Working dir:  [/Users/op/code/gardenio        ▾]        │
│   Ref:          [agent/kd-…-spawn-action-wire  ▾]        │
│   Daemon:       [laptop-op (online · v0.4.2)   ▾]        │
│   Perm profile: [default-local                 ▾]        │
│                                                          │
│   Optional note (lands on the spawn-intent bead):        │
│   [                                                   ]  │
│   [                                                   ]  │
│                                                          │
│   [ Cancel ]                       [ Spawn locally → ]   │
└──────────────────────────────────────────────────────────┘
```

The dialog form-encodes the same fields as `POST /b/<id>/spawn-local`
above. On submit, we close the dialog and replace the action row with
the spawn-action-button widget rendered against the new
`spawn-intent` bead — same hx-poll lifecycle as `/escalations`.

### Bead-show page (decision detail)

`type_decision.html` partial gains the same "or handle locally" row
under the option chips. Once a spawn-intent child exists, the row is
**replaced** by the spawn-action-button widget bound to that
spawn_intent bead.

### Slack (stretch)

Slack message gains a third button per option ("⤓ Spawn locally on my
laptop"). Click → bridge looks up operator → bridge looks up most
recent daemon ping for that operator → bridge POSTs the same signed
envelope. Slack message updates in-place with daemon state chips, same
as today's resolve/dismiss in-place update.

If no daemon is online for that operator, button is omitted from the
Slack message at render time (no half-broken UI).

## Widget contract

We dogfood the free-htmx widget pattern (ka-0000006879). One new
widget-type:

- **`spawn-action-button`** (`widget-stage:draft` to start) — renders a
  single spawn-action affordance bound to either a decision id or an
  escalation id. Payload carries `decision_id`, `option_id?`,
  `default_working_dir`, `default_ref`, `daemon_id?`, and optional
  `spawn_intent_id`. The template emits an htmx button that either
  POSTs to `/b/<id>/spawn-local` (idle state) or polls
  `/spawn/<spawn_id>/status` (in-flight/done state) via
  `hx-trigger="every 2s"` until terminal.

The widget is instantiated:

- inline on `/escalations` rows (one per row, payload from row data),
- inline on `/decisions/<id>` per option chip,
- on bead-show partial for type=decision and type=escalation,
- on the spawn-escalated-agent rich-report bead (this research's
  dogfooded artifact).

Schema sketch:

```text
{
  "type": "object",
  "required": ["source"],
  "properties": {
    "source": {
      "type": "object",
      "required": ["kind", "id"],
      "properties": {
        "kind":  {"enum": ["decision", "escalation"]},
        "id":    {"type": "string"},
        "option_id": {"type": "string"}
      }
    },
    "default_working_dir": {"type": "string"},
    "default_ref":         {"type": "string"},
    "daemon_id":           {"type": "string"},
    "spawn_intent_id":     {"type": "string"}   // present after first click
  }
}
```

CSP / sanitizer constraints (per ka-0000006879 §Sandbox):

- only `hx-post` / `hx-get` / `hx-trigger` / `hx-swap` / `hx-target`,
- all `hx-*` URLs must match the beads-viewer origin allowlist,
- no inline `<script>`; lifecycle handled by an OOB swap on a
  span#spawn-state-<id> chip.

## Failure modes (handed off to ka-0000004516)

Listed here only to mark the seams; details belong in the failure-modes
research:

| Mode                       | Operator-visible behavior                                  |
|----------------------------|------------------------------------------------------------|
| Daemon offline             | Amber chip; "Recheck" button; doc link                     |
| Permission denied          | Red chip; tooltip with reason; "Edit perms" link to bead   |
| Working dir invalid        | Red chip; "Pick another dir" reopens dialog                |
| Repo not cloned locally    | Daemon offers `gb daemon clone <repo>` one-click; queued   |
| Credential missing         | Red chip; "Run `gb login`" hint                            |
| Forward timeout (>5s)      | Auto-flip to daemon-offline; spawn-intent.state=offline    |
| Concurrent re-click        | Idempotency cache returns same handle                      |
| Daemon-side spawn timeout  | spawn-intent.state=failed with `error` populated           |
| Decision resolved meantime | Action greys out; spawn-intent created with state=stale    |

## Audit & telemetry

- Spawn-intent beads (`type=spawn-intent`) are the canonical record.
  Indexed for `gb news` and a future `/spawns` queue surface.
- Comment back on the decision: short, machine-written:
  `gardenio-spawn-action: operator slack:U01… elected to handle locally → kd-<spawn-intent>`
  This is the back-mention seam for ka-0000002753 (fleet integration).
- Lifecycle callbacks from the daemon update the spawn-intent bead
  fields. Each transition posts a comment on the spawn-intent bead so
  beads-viewer activity-log renders a clean timeline.
- Local-spawned agents register with the controller under the
  `local:<operator-daemon-id>` namespace, so they appear in `/agents`,
  `gb news`, and the agent fleet tile — colored by a `lane=local`
  chip. The spawn-intent bead carries `local_agent_id` so we can pivot
  between the two views.

## Security shape (handed off to ka-0000006932)

This wire research is non-normative on auth — ka-0000005882 owns the
key shape. We only commit here to:

1. The forwarded envelope is **signed by the controller**, not by the
   browser. Browsers see only an opaque `spawn_id` and a status URL.
2. The envelope carries a 5-minute TTL; daemon rejects expired or
   replayed envelopes (nonce table keyed by `spawn_id`).
3. Daemon-side action gating reads `permissions_profile` from the
   envelope; the daemon does **not** trust browser-supplied
   permission strings.
4. The audit bead (`spawn-intent`) is created **before** the forward,
   so any client-side tampering is observable post-hoc.

## Open questions for design synthesis (phase-2 gb decision)

1. **Audit shape**: comment + child bead (proposed) vs. child bead
   only vs. comment only? Recommendation: comment + child bead.
2. **Working-dir resolution**: pick at click time (dialog) vs.
   per-project config bead default with override? Recommendation:
   config-bead default, dialog override on `/decisions/<id>`, no
   prompt on `/escalations`.
3. **Multi-daemon**: silent first-match vs. require operator pick
   when >1 daemon online? Recommendation: silent first-match in v0,
   surface the daemon picker only when ≥2 are online.
4. **Slack stretch**: ship with v0 vs. defer? Recommendation: defer
   one cycle — daemon discovery from bridge needs its own audit
   review (ka-0000006932 seam).
5. **Spawn-intent retention**: 7-day TTL vs. close-on-terminal vs.
   keep forever as audit? Recommendation: 7-day for non-terminal,
   keep forever (closed status) for terminal — same lifecycle as
   normal data beads.

## Inputs reviewed

- `gardenio/controller/cmd/beads-viewer/templates/escalations.html`
  (lines 27-55: row shape; chip-strip, meta strip, review link).
- `gardenio/controller/cmd/beads-viewer/templates/decisions.html`
  (lines 36-82: option-chip strip, resolve dialog).
- `gardenio/docs/research/escalation-beads-review-2026-05-05.md`
  (escalation vs decision surface map; current gaps).
- ka-0000006791 (parent epic — direction & scope).
- ka-0000004527 (sister epic — top-level escalations surface).
- ka-0000006879 (free-htmx widget pattern — dogfooding target).
- ka-0000005882, ka-0000006867 (in-flight sibling research — seams
  marked above).

## Out of scope

- Implementation.
- Daemon process model, install path, auto-update (→ ka-0000006867).
- AuthN/AuthZ wire shape and key provisioning (→ ka-0000005882).
- Permission profile schema (→ ka-0000004678).
- Config bead surface (→ ka-0000000166).
- Failure-mode UX details beyond chip text (→ ka-0000004516).
- Security threat model (→ ka-0000006932).
- Operator install / first-run (→ ka-0000005690).
- Non-spawn local actions (separate epic if daemon proves out).
