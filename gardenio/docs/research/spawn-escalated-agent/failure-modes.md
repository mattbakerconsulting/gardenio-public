# Failure modes & graceful degradation — `gb-daemon` spawn-escalated-agent

**Bead:** ka-0000004516 (research child 7 of 9)
**Epic:** ka-0000006791 — client-side `gb` daemon for "spawn escalated agent"
**Pairs with:** ka-0000004798 (child 3 — spawn-action-wire; the UI states this doc
degrades) and ka-0000002753 (child 5 — fleet integration; the telemetry events this
doc emits).
**Consumes:** ka-0000005882 (authn/authz), ka-0000004678 (permissions model),
ka-0000006867 (daemon lifecycle), ka-0000000166 (config-bead surface),
ka-0000006932 / ka-0000001720 (security review).
**Status:** research / recommendation only. No code lands until the epic's Phase-2
`gb decision` ratifies the daemon shape. This doc specifies the *failure taxonomy*,
the *state machine* that governs degradation, and the *telemetry contract* that
decision must ratify. Implementation is explicitly out of scope.

---

## TL;DR

1. **One rule governs every failure: never render a live-looking affordance that
   cannot succeed.** A spawn button that greys, hides, or explains beats a button
   that spins forever. Each failure below resolves to exactly one of five operator
   surfaces — `hidden`, `disabled+reason`, `retry`, `remediate`, `superseded` — so
   the viewer never has to invent behavior at runtime.

2. **Degradation is a *state machine*, not a set of error strings.** The
   spawn-action-wire (ka-0000004798) already defines the happy-path states
   (`idle → spawning → spawned`). This doc adds the *failure* states and the *legal
   transitions* between them, so the spawn-intent bead is a faithful, append-only
   audit of what actually happened. §3 is the canonical state table; §4 is the
   transition diagram.

3. **Failures split into two trust tiers.** *Pre-forward* failures (decision stale,
   permission-denied-by-policy, daemon-offline) are detectable by the controller
   **before** it signs an envelope — fail fast, cheap, no daemon hop, no side
   effects. *Post-forward* failures (working-dir invalid, repo not cloned, cred
   missing, spawn timeout) are only knowable on the operator's laptop — the daemon
   reports them back and the controller records them. The dividing line matters for
   idempotency (§6): a pre-forward failure never consumed a spawn slot.

4. **Fail closed, degrade open.** Security-review (ka-0000001720) requires the
   *authorization* path to fail closed — an unknown verdict denies. But the
   *availability* path degrades open: when the daemon is unreachable, the operator
   is never stranded; they always get the copy-paste-URL fallback (§7) that
   reconstructs the session by hand. Safety fails shut; convenience fails soft.

5. **Every failure emits exactly one telemetry event** (§8), typed and low-
   cardinality, so child 5's fleet dashboard can answer "which failure mode is
   costing operators the most spawns?" without scraping logs.

---

## 1. Design principles

These five principles are the *why* behind every row in §3. They resolve the open
questions the task poses ("button greys, queue+retry, copy-paste URL, hide
entirely?") into a consistent policy rather than nine ad-hoc answers.

### P1 — Never show a dead-live control.

A control that looks actionable but cannot act is the worst outcome: the operator
clicks, waits, and learns nothing. Every failure therefore resolves the affordance
to one of five *terminal surfaces* (§2). The viewer chooses the surface from the
spawn-intent `state` alone — it never inspects error strings to decide layout.

### P2 — Detect the cheapest failures earliest.

Order the checks by cost. The controller can know "this decision is already
resolved" or "no daemon has pinged in 30s" from its own state, for free, before it
mints and signs an envelope. Push those checks to the **pre-forward** phase so a
doomed spawn never crosses the wire, never wakes the laptop, and never needs an
idempotency slot. Only failures that *require* the laptop's filesystem (working
dir, clone state, creds) are allowed to be **post-forward**.

### P3 — Distinguish *transient* from *terminal*.

A transient failure (forward timeout, daemon momentarily asleep) offers **Recheck /
Retry** — the same envelope can be re-sent. A terminal failure (decision resolved
by another operator, envelope expired) offers **no retry** — retrying would either
double-act or re-fail identically. The state machine (§4) encodes which transitions
are legal; the UI reads legality from state, never guesses.

### P4 — Remediation over rejection.

When a failure is *fixable by the operator in one step* (repo not cloned, cred
missing, working dir absent), the surface is **remediate**: offer the one command
or one-click that fixes it, then auto-retry. A raw "denied" with no path forward is
reserved for genuine policy denials (permission profile forbids this repo), where
the fix is "edit the profile bead", not "click here".

### P5 — Degrade to copy-paste, never to nothing.

The daemon is an *accelerator*, not a *gate*. Every escalation can already be
handled the old way — operator reads the bead, opens their editor, checks out the
ref by hand. So the ultimate fallback for any availability failure is a
**copy-paste bundle** (§7): the exact `git checkout` + context the daemon *would*
have used, rendered as copyable text. The operator is never blocked, only slowed.

---

## 2. The five operator surfaces

Every failure maps to exactly one of these. This is the closed set the viewer
renders; adding a failure mode means assigning it a surface, not inventing UI.

| Surface | Chip color | Affordance | When |
|---|---|---|---|
| **`hidden`** | — (absent) | No spawn control rendered at all | The action is meaningless in context (decision already resolved/cancelled before first click; no daemon ever registered). Prevents zombie controls. |
| **`disabled+reason`** | grey | Greyed button + tooltip explaining why + link to fix the *config* | Policy says no (permission denied) or capability absent (daemon too old). The operator can't retry into success; they must change something durable first. |
| **`retry`** | amber | Live button reading "Recheck" / "Retry spawn" + `hx-trigger` re-poll | Transient: daemon offline, forward timeout. Same envelope can succeed on the next attempt. |
| **`remediate`** | amber | One-click / one-command fix + auto-retry on success | Operator-fixable in one step: repo not cloned, cred missing, working dir absent. |
| **`superseded`** | grey→fade | Chip flips to "Handled elsewhere" then the row hides on next poll | A race resolved the decision while this spawn was mid-flight. Terminal, informational. |

**Why five and not nine (one per failure):** collapsing failures onto a small
surface set is what keeps the viewer template dumb and the operator's mental model
small. "Amber = try again in some way; grey = go fix something; gone = nothing to
do here" is learnable; nine bespoke error cards are not.

---

## 3. Failure state-table (canonical)

Each row: the failure, where it is *detected*, the resulting spawn-intent `state`,
the operator surface (§2), the concrete affordance, the recovery semantics (§5), and
the telemetry event (§8). This is the single source of truth other children read.

| # | Failure mode | Detected | `state` | Surface | Affordance (operator sees) | Recovery | Telemetry event |
|---|---|---|---|---|---|---|---|
| F1 | **Daemon offline** — never installed | pre-forward | `daemon-offline` | `retry` | Amber "Recheck local daemon"; tooltip "Install `gb daemon` and run `gb daemon start`"; doc link | Recheck re-pings registry; copy-paste fallback always offered (§7) | `spawn.daemon_offline{reason=never_registered}` |
| F2 | **Daemon offline** — crashed / paused / stale ping | pre-forward | `daemon-offline` | `retry` | Amber "Recheck"; tooltip "Last seen 4m ago"; copy-paste fallback | Recheck; daemon auto-reregisters on restart (lifecycle ka-0000006867) | `spawn.daemon_offline{reason=stale_ping}` |
| F3 | **Network partition** — viewer↔daemon path down mid-forward | forward | `daemon-offline` | `retry` | Auto-flip from `spawning` to amber after 5s forward timeout | Retry re-sends same envelope (idempotent, §6); no slot consumed | `spawn.forward_timeout` |
| F4 | **Permission denied** — profile forbids this repo/dir/action | pre-forward* | `denied` | `disabled+reason` | Red/grey; tooltip = policy `Reason`; "Edit permissions" → profile bead | No retry into success; operator edits `type=daemon-permission-profile` bead, then affordance re-enables | `spawn.denied{axis=repo\|dir\|action\|dry_run}` |
| F5 | **Working directory invalid** — path missing/unwritable | post-forward | `failed` | `remediate` | Red chip; "Pick another dir" reopens dialog with dir field focused; or "Create dir?" one-click | Operator picks/creates dir; dialog re-submits (new nonce) | `spawn.failed{reason=workdir_invalid}` |
| F6 | **Repo not cloned locally** | post-forward | `needs-clone` | `remediate` | Amber; "Clone `<repo>` (~120 MB)" one-click → daemon clones → auto-continue | Daemon runs scoped clone (permissions §), then resumes same spawn_id | `spawn.needs_clone` → `spawn.clone_ok\|clone_failed` |
| F7 | **Credential not present** — repo auth flow lacks creds | post-forward | `failed` | `remediate` | Red; "Run `gb login <forge>`" hint with copy-icon; recheck after | Operator provisions cred locally; retry | `spawn.failed{reason=cred_missing,forge=…}` |
| F8 | **No operator identity** — not paired yet | pre-forward | `hidden`→CTA | `disabled+reason` | Affordance replaced by "Pair this machine: `gb daemon pair`" first-run CTA | Operator pairs (authn/authz ka-0000005882 device flow); affordance appears | `spawn.no_identity` |
| F9 | **Decision resolved by another operator** (race) | pre- or post-forward | `stale` / `superseded` | `superseded` | Grey "Handled elsewhere by @op"; row fades next poll | Terminal. If already forwarded, daemon told to abort spawn (§6 abort) | `spawn.superseded{by=slack:…}` |
| F10 | **Decision cancelled / dismissed** | pre-forward | `hidden` | `hidden` | Control not rendered; if mid-flight, flips to `superseded` | Terminal | `spawn.cancelled` |
| F11 | **Daemon version too old / incompatible** | pre-forward (handshake) | `denied` | `disabled+reason` | Grey; "Your `gb daemon` is v0.3.1; needs ≥ v0.4.0. Run `gb daemon upgrade`" | No retry until upgrade; recheck after | `spawn.version_incompatible{have=…,need=…}` |
| F12 | **Daemon-side spawn timeout** — process never came up | post-forward | `failed` | `retry` | Red; tooltip shows daemon `error`; "Retry spawn" | Retry with `force=1` (spawn-intent was `failed`, wire §Idempotency) | `spawn.failed{reason=spawn_timeout}` |
| F13 | **Envelope expired** (>5-min TTL elapsed before daemon acted) | post-forward | `failed` | `retry` | Red; "Spawn request expired; retry" | Retry mints fresh envelope (new `issued_at`), same `spawn_id` | `spawn.failed{reason=envelope_expired}` |

\* F4 is *pre-forward* for the axes the controller can evaluate from the named
profile (repo allowlist, action kind), and *post-forward* for axes that need the
laptop (e.g. a dir-writability rule). The permissions model (ka-0000004678) resolves
the authoritative grant **on the daemon**; the controller's pre-check is advisory
and fails *toward* forwarding so the daemon stays the enforcement point
(security-review M4: "the name is a selector, not a grant").

### Reading the table

- **`state` is what lands on the spawn-intent bead.** The viewer's
  `spawn-action-button` widget renders surface purely from `state` (P1).
- **Pre-forward failures never mint a signed envelope** and never consume an
  idempotency slot (P2, §6). They are cheap and side-effect-free.
- **`remediate` rows always pair a failure event with a follow-up event**
  (`needs_clone` → `clone_ok|clone_failed`) so child 5 can measure remediation
  success rate, not just failure rate.

---

## 4. Degradation state machine

The wire doc (ka-0000004798 §State diagram) owns the happy path. This is the full
graph including failure states. `spawn-intent.state` is **append-only in the audit
sense** — every transition posts a comment on the spawn-intent bead — but the
*current* state is a single field the widget reads.

```
                         click (pre-forward checks)
             ┌──────────────────────────────────────────┐
             │                                           │
             ▼        F8/F10 → hidden                    │
   ┌──────────────┐   F4/F11 → denied ──────────┐        │
   │     idle     │   F9 (pre) → stale           │        │
   └──────┬───────┘   F1/F2 → daemon-offline ◄─┐ │        │
          │ checks pass                        │ │        │
          ▼                                    │ │        │
   ┌──────────────┐   forward timeout (F3) ────┘ │        │
   │   spawning   │──────────────────────────────┘        │
   └──────┬───────┘                                        │
          │ daemon ack                                     │
          ▼                                                │
   ┌──────────────┐   F5/F7/F12/F13 ──► ┌──────────┐       │
   │   (running)  │───────────────────► │  failed  │──retry┘
   └──────┬───────┘                     └──────────┘  (force=1)
          │           F6 ──► ┌──────────────┐
          │                  │  needs-clone │──clone_ok──┐
          │                  └──────────────┘            │
          │                        │ clone_failed        │
          │                        ▼                     │
          │                     failed                   │
          ▼                                              │
   ┌──────────────┐  ◄──────────────────────────────────┘
   │   spawned    │──── decision resolved elsewhere (F9 post) ──► superseded
   └──────────────┘
```

**Legal transitions (enforced controller-side):**

| From | To | Trigger | Retry allowed? |
|---|---|---|---|
| `idle` | `hidden` | F8, F10 | n/a (no control) |
| `idle` | `denied` | F4, F11 | only after config change (P3 terminal) |
| `idle` | `daemon-offline` | F1, F2 | yes — Recheck (P3 transient) |
| `idle` | `stale`/`superseded` | F9 pre | no (terminal) |
| `idle` | `spawning` | checks pass | — |
| `spawning` | `daemon-offline` | F3 forward timeout | yes — Retry (idempotent) |
| `spawning` | `failed` | F13 envelope expired | yes — fresh envelope |
| `running` | `needs-clone` | F6 | auto (clone → resume) |
| `running` | `failed` | F5, F7, F12 | yes — Retry force=1 |
| `needs-clone` | `running` | clone_ok | — |
| `needs-clone` | `failed` | clone_failed | yes |
| `spawned` | `superseded` | F9 post-spawn race | no (abort local, §6) |

Any transition **not** in this table is rejected by the controller as a protocol
error and logged (`spawn.illegal_transition`) — this is how a buggy or malicious
daemon callback (security-review threat) is caught: the daemon cannot drive the
intent into an arbitrary state, only along legal edges.

---

## 5. Recovery & retry semantics

### 5.1 Retry classes

| Class | Failures | Mechanism | Envelope | Slot |
|---|---|---|---|---|
| **Recheck** | F1, F2 (daemon-offline) | Re-ping daemon registry; if online, re-enable idle button | none yet | none |
| **Re-send** | F3 (forward timeout) | Controller re-forwards the *same* signed envelope if unexpired, else mints fresh | reused/fresh | same `spawn_id` |
| **Force-retry** | F5, F7, F12, F13 (failed) | `POST /spawn/<id>/status?force=1`; mints fresh envelope, keeps `spawn_id` | fresh | same `spawn_id` |
| **Auto-resume** | F6 (needs-clone) | Daemon clones then continues without a new operator click | reused | same `spawn_id` |
| **Config-then-enable** | F4, F11 (denied/incompatible) | Operator edits profile bead / upgrades daemon; affordance polls capability and re-enables | n/a | n/a |
| **None** | F8*, F9, F10 | Terminal or requires pairing first | n/a | n/a |

\* F8 (no identity) is "none" for *spawn* but has its own first-run pairing path
(ka-0000005882); once paired the affordance appears fresh at `idle`.

### 5.2 Retry backoff & caps

- **Recheck** is operator-driven (button), so no backoff — but the widget's
  `hx-trigger="every 2s"` background poll for a `daemon-offline` intent uses
  exponential backoff (2s → 4s → 8s → cap 30s) to avoid hammering the registry when
  a laptop is closed for the night. After 10 min of continuous offline the poll
  stops and the chip reads "paused — click Recheck".
- **Force-retry** is capped at **3 automatic-looking attempts** per `spawn_id`;
  beyond that the surface becomes `disabled+reason` ("repeated spawn failures — see
  daemon logs: `gb daemon logs`") to break retry loops (P3). The cap is recorded on
  the intent (`retry_count`) and emitted (`spawn.retry_exhausted`).
- **Idempotency** guarantees retries never double-spawn (§6); the cap guarantees
  they never busy-loop.

### 5.3 Recovery ownership

| Failure | Who fixes | Where |
|---|---|---|
| Daemon offline / crashed | operator | start daemon on laptop |
| Permission denied | operator | edit `type=daemon-permission-profile` bead |
| Working dir / repo / cred | operator | one-click remediate on laptop |
| No identity | operator | `gb daemon pair` device flow |
| Version incompatible | operator | `gb daemon upgrade` |
| Decision race | nobody (informational) | — |
| Illegal transition | fleet owner | daemon bug — file bead from telemetry |

The task's framing question — "button greys, queue+retry, copy-paste URL, hide
entirely?" — is now answered per-failure by this column plus §2's surface:
**grey** for policy/capability (F4, F11), **retry** for transience (F1–F3, F12–F13),
**remediate** (a richer queue+retry) for one-step-fixable (F5–F7), **hide** for
meaningless (F8 pre-pair, F10), and **copy-paste** (§7) as the universal floor under
every availability failure.

---

## 6. Idempotency & race handling

Idempotency is the load-bearing property that makes retry safe. It reuses the wire
doc's deterministic `spawn_id = f(operator, decision_id, recommended_ref)`.

- **Pre-forward failures consume no slot.** F1/F2/F4/F8/F9-pre/F10/F11 are decided
  before an envelope is minted, so re-clicking is free and never leaves a
  half-spawn.
- **Re-click while `spawning`/`spawned`** returns the existing handle (wire §
  Idempotency: `state ∈ {spawned, spawning} → return existing`), so a double-click or
  a two-tab operator never gets two sessions.
- **Re-click while `failed`** requires `force=1` (explicit intent to retry), so a
  reflexive re-click doesn't silently re-run a spawn that failed for a real reason.
- **Re-click while `daemon-offline`** re-forwards (treated as a fresh attempt) —
  because offline is availability, not a completed action.

### Race: two operators, one decision (F9)

The decision bead is the arbiter. Two sub-cases:

1. **Both pre-forward:** the controller resolves the decision atomically (existing
   decision-resolve path); the loser's spawn attempt sees the decision already
   `resolved` and lands at `stale` → `superseded` surface. No envelope minted.
2. **One already `spawned`, other resolves server-side:** the controller, on
   resolving the decision, looks up any live spawn-intent for it and sends the
   daemon an **abort** (`spawn.abort{spawn_id}`); the local session is signaled to
   wrap up (not killed — operator may have unsaved work; §Security note). Intent →
   `superseded`. This is why `spawned → superseded` is a legal edge (§4).

The abort is *cooperative*: the daemon posts a comment ("decision resolved
elsewhere; wrap up when ready") into the local session rather than SIGKILL, honoring
the operator-has-local-state constraint the epic calls out (poking at prod, unsaved
edits).

---

## 7. The copy-paste fallback (universal availability floor)

Per P5, no availability failure ever leaves the operator with *nothing*. When the
daemon is offline (F1–F3) or the operator declines to install one, the affordance
offers a **"Copy manual spawn"** action that renders the exact context the daemon
*would* have used, as copyable plaintext:

```
# Spawn escalated agent by hand (daemon offline)
# Decision: ka-0000004798 — "Pick session strategy"  ·  option: «Use JWT»
# Operator: slack:U0OPERATOR1

cd ~/code/gardenio                       # recommended working dir
git fetch origin && \
  git checkout agent/kd-…-spawn-action-wire   # recommended ref
claude --resume-context ka-0000004798   # seed session with bead context
```

- The block is generated controller-side from the *same* payload that would fill
  the signed envelope, so the manual path and the daemon path stay in lockstep —
  there is no second source of truth for "what should the session start from".
- Copying emits `spawn.fallback_copied` (§8) so child 5 can measure how often the
  daemon is bypassed — a high rate is a signal the daemon install funnel needs work
  (feeds child 9, opt-out/first-run ka-0000005690).
- The fallback is *always present* under offline states, and additionally reachable
  via a small "handle manually ↗" link even in the healthy state, so an operator who
  simply prefers their own flow is never forced through the daemon.

This is the concrete answer to the task's "copy-paste URL" option: not a URL, but a
**reconstruction recipe**, because a bare URL can't check out the right ref.

---

## 8. Telemetry contract (feeds child 5)

Every failure emits **exactly one** typed event so the fleet-integration child
(ka-0000002753) can build a degradation dashboard without log-scraping. Events are
low-cardinality: a fixed `event` name + a small enumerated `reason`/`axis` label.
They are emitted **controller-side** (the trust anchor) even when the failure was
reported by the daemon, so a compromised daemon cannot suppress its own failure
telemetry (security-review requirement).

### 8.1 Event schema

```text
{
  "event":      "spawn.daemon_offline",   // enum, see §8.2
  "spawn_id":   "spawn-…",                // absent for pre-mint failures
  "decision_id":"kd-…",
  "operator":   "slack:U0OPERATOR1",      // hashed at rest per PII posture
  "phase":      "pre_forward|forward|post_forward",
  "reason":     "stale_ping",             // enumerated per event
  "retry_count": 0,
  "ts":         "2026-08-14T16:40:00Z"
}
```

### 8.2 Event catalog

| Event | Phase | Labels | Terminal? | Dashboard question it answers |
|---|---|---|---|---|
| `spawn.daemon_offline` | pre/forward | `reason∈{never_registered,stale_ping}` | no | How often is the laptop unreachable? Install-funnel health. |
| `spawn.forward_timeout` | forward | — | no | Network/path reliability viewer↔daemon. |
| `spawn.denied` | pre/post | `axis∈{repo,dir,action,dry_run}` | yes | Are permission profiles too tight? |
| `spawn.failed` | post | `reason∈{workdir_invalid,cred_missing,spawn_timeout,envelope_expired}` | yes | Which laptop-side prereq breaks most? |
| `spawn.needs_clone` | post | `repo` | no | How often is the repo missing locally? |
| `spawn.clone_ok` / `spawn.clone_failed` | post | `repo` | — | Clone remediation success rate. |
| `spawn.no_identity` | pre | — | yes | Pairing-funnel drop-off. |
| `spawn.version_incompatible` | pre | `have`,`need` | yes | Daemon fleet upgrade lag. |
| `spawn.superseded` | any | `by` (operator) | yes | Operator-collision rate on hot decisions. |
| `spawn.cancelled` | pre | — | yes | Decisions cancelled mid-consideration. |
| `spawn.fallback_copied` | — | — | — | Daemon-bypass rate (install-funnel signal). |
| `spawn.retry_exhausted` | post | — | yes | Retry-loop / chronic-failure detection. |
| `spawn.illegal_transition` | — | `from`,`to` | — | Buggy/hostile daemon callback detection. |

### 8.3 Derived signals

- **Spawn success rate** = `spawned` intents ÷ (all `idle→*` starts). A drop is the
  top-line health metric.
- **Degradation mix** = share of each failure event among non-success outcomes —
  tells the fleet owner *where* to invest (install funnel vs. permission tuning vs.
  clone UX).
- **Remediation success** = `clone_ok ÷ needs_clone`, `spawned-after-retry ÷
  failed` — measures whether the "remediate" surface actually recovers operators.
- **Bypass rate** = `fallback_copied ÷ (fallback_copied + spawned)` — high means the
  daemon isn't earning its place; low means it is.

These four are the dashboard child 5 should build; this doc fixes their numerators
and denominators so both children agree on definitions (advice ka-0000005924:
measure the same quantity two ways and make them reconcile — success-rate derived
from intents must reconcile with `1 − Σ failure-events/starts`).

---

## 9. Security notes (defer to ka-0000001720 for the full model)

Failure handling is a security surface, not just a UX one. Three constraints this
doc inherits and must not violate:

1. **Fail closed on authorization, open on availability (P4/P5).** An *unknown*
   permission verdict is `deny` (never `allow`), but an *unreachable* daemon degrades
   to the copy-paste floor — safety and convenience fail in opposite directions on
   purpose.
2. **The daemon cannot drive arbitrary state.** Callbacks may only walk the legal
   transition graph (§4); anything else is `spawn.illegal_transition` and dropped.
   This bounds a compromised daemon to *reporting* failures, not *forging* success.
3. **Telemetry is emitted controller-side.** A daemon cannot hide its failures by
   staying silent — the controller records timeout/abort/expiry from its own view of
   the exchange. The daemon *enriches* (adds `reason=cred_missing`) but does not
   *originate* the audit record.
4. **Cooperative abort, never kill (§6).** Because a local session may hold unsaved
   operator state (the epic's raison d'être — poking at prod, local secrets), the
   race/supersede path *signals* wrap-up rather than terminating, trading a slightly
   longer zombie window for never destroying operator work.

---

## 10. Open questions for the Phase-2 `gb decision`

Research-only; these are the forks the synthesis decision must pick, not answers.

1. **Offline poll lifetime.** §5.2 proposes 10-min background poll then stop. Is a
   longer "wake me when the laptop comes back" (push via controller→daemon on next
   ping) worth the complexity, or is manual Recheck enough?
2. **Abort semantics granularity.** Cooperative wrap-up (§6) vs. a "soft-hold"
   where the local session stays live but the decision is marked handled — which
   matches operator expectation on a race?
3. **Copy-paste floor as default vs. fallback.** Should "handle manually" be a
   co-equal first-class button always, or only surfaced on daemon-offline? (Bears on
   install-funnel metrics — child 9.)
4. **Denied-surface remediation depth.** For F4, do we deep-link to the *specific
   failing axis* in the profile bead editor, or just to the profile bead? Deep-link
   is better UX but couples the viewer to the permission schema.
5. **Version-skew policy.** Hard-block below min version (F11), or allow a
   compatibility shim for one minor version behind? Affects daemon upgrade urgency.

---

## Cross-references

- Happy-path states & wire: `spawn-action-wire.md` (ka-0000004798)
- Permission verdict shape (`allow`/`deny`/`prompt`): `permissions-model.md` (ka-0000004678)
- AuthN/AuthZ & pairing (F8): `authn-authz.md` (ka-0000005882)
- Daemon lifecycle, crash recovery, registration (F1/F2): `daemon-lifecycle.md` (ka-0000006867)
- Config-bead surface for permission profiles (F4): `config-bead-surface.md` (ka-0000000166)
- Threat model & fail-closed rationale (§9): `security-review.md` (ka-0000001720)
- Telemetry consumer: fleet integration (ka-0000002753)
