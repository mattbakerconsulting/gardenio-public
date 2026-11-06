# AuthN/AuthZ between the controller and the gb-daemon

**Bead:** ka-0000005882 (research child 2 of 9)
**Epic:** ka-0000006791 — client-side `gb` daemon for "spawn escalated agent"
**Sister epic:** ka-0000004527 — top-level escalations surface
**Status:** Research-only — no implementation. Synthesis decision lives in epic Phase 2.

---

## TL;DR — Recommendation

Adopt a **three-layer** model. None of the surveyed options stand alone; each
covers one of three distinct trust questions.

1. **Channel identity (daemon ↔ controller)** — **long-lived authenticated
   websocket from daemon to controller, daemon-initiated**. The daemon's
   client cert is provisioned by a one-time pairing flow (Layer 2). The
   controller authenticates with its TLS server cert (terminated at the AVA
   edge today). This sidesteps NAT/firewall on operator laptops and gives us
   a single long-lived authenticated channel for actions, heartbeats, and
   future capabilities (file open, log tail, etc.).
2. **Daemon identity (which operator's laptop is this?)** — **one-time
   pairing flow seeded from beads-viewer + per-daemon mTLS client cert**.
   The operator clicks "Pair this device" in beads-viewer (AVA-gated), gets
   a short-lived pairing code, runs `gb daemon pair <code>` locally, and
   the controller mints a per-daemon client certificate bound to the
   operator's identity. The cert is the daemon's long-lived credential;
   private key never leaves the laptop. Operator OIDC identity is **not**
   used at the channel layer — it is consumed once at pairing time, then
   carried by the cert.
3. **Action authorization (this specific spawn is real and current)** —
   **short-lived JWT bound to the decision bead**, minted by the controller
   when an operator clicks "spawn escalated agent" in beads-viewer, with
   `aud=gb-daemon:<daemon_id>`, `sub=<operator-sub>`, `decision_id`,
   `nonce`, `exp ≤ 60s`, and signed with a controller key (KMS-backed or
   keycloak-issued). The daemon verifies the JWT, checks the bound decision
   bead is still in `open` state, records the nonce in a short replay-
   protection cache, and acts. The decision bead is the audit-of-record.

Why this combination wins: Layer 1 alone (channel pinning) does not prove
*which* operator initiated this action. Layer 2 alone (operator OIDC) is too
heavy for every action and can be replayed if the operator's token is
stolen. Layer 3 alone is fine cryptographically but gives no story for
identifying which daemon to deliver the action to. Together they answer
"who is this daemon?", "is it talking to the real controller?", and "is
this specific spawn live and authorised?" with one mechanism each.

Open questions answered:

- **"How does the daemon discover its operator identity?"** Once, via the
  beads-viewer-seeded pairing flow. After that, the operator identity is
  embedded in the per-daemon cert subject + a server-side mapping.
- **"Do we require the daemon to be reachable from the controller, or pull
  over a long-lived websocket?"** Long-lived websocket, daemon-initiated.

---

## Scope & out-of-scope

In scope:

- AuthN: how the daemon proves it is *this operator's* daemon to the
  controller, and how the controller proves it is the real controller to
  the daemon.
- AuthZ for the "spawn escalated agent" action specifically: the daemon
  must be sure the action came from a real, unresolved decision bead.
- The threat model: replay, MITM, stolen-token, multi-operator collision,
  daemon impersonation.
- Wire-shape implications (websocket vs HTTPS poll vs callable-controller).

Out of scope:

- Implementation. Synthesis decision belongs to the epic.
- Mobile (iOS) spawn flow.
- Daemon lifecycle / install model — covered by sister task ka-0000006867.
- Spawn-action wire & UI — covered by sister task in the epic's research
  tree.
- The permissions model the daemon enforces *after* a spawn is authorised
  (per-action allowlist, working-dir constraints, dry-run vs apply) —
  belongs to the permissions-model research child.

---

## Background — what already exists

The gardenio platform already has three patterns we can lean on rather than
inventing in a vacuum.

### `gb login` (browser cookie handoff)

`gardenio/controller/cmd/gb/login.go` ships a working browser-handoff flow
today. `gb login` spawns a one-shot 127.0.0.1 listener, opens the browser
to `<viewer>/auth/cli-handoff`, captures the operator's AVA + GitLab
session cookie bundle, and persists it via `credstore` at
`~/.config/gardenio/credentials.json` (0600). State nonce defends the
loopback against drive-by `/cb?code=` hits.

The daemon pairing flow should look identical from the operator's POV. The
difference is what comes back: instead of stashing a cookie header, the
controller mints a per-daemon client certificate.

### Keycloak client-credentials

`gardenio/tests/e2e/scripts/lib-keycloak-client-creds.sh` plus
`gardenio/docs/agent-pod-keycloak-auth.md` document the existing OAuth2
client-credentials grant flow used to mint bearer tokens for service-to-
service identity. Agent pods already use it. The same machinery can mint
short-lived JWTs for the per-action layer if we keep keycloak in the
critical path; if we prefer to avoid that, the controller can sign with a
local KMS-managed key.

### AVA edge + OIDC

`gardenio/controller/internal/oidcauth/` + `gardenio/controller/internal/avareauth/`
implement the realistic auth surface today: AVA at the edge for the
beads-viewer hostname, OIDC bearer for API clients, AVA user-context
header trust-mode when the request comes through the AVA-gated path.

AVA at the edge is **good news** for the websocket model: the daemon ↔
controller connection terminates TLS at the AVA endpoint, so the AVA
auth-decision is already in the middle of every connection. We do not
need to reinvent edge auth.

### Existing CLI credstore

`gardenio/controller/internal/credstore/Context` already has fields for
`Cookie`, `AccessToken`, `IDToken`, `RefreshToken`, `ExpiresAt`, and a
`User`. Adding a `ClientCert` + `ClientKey` (file-paths, or PEM blobs with
0600 enforcement) is a straightforward additive change.

---

## Inputs surveyed (with verdicts)

The task brief named five candidate inputs. Below: what each one is, what
it solves, what it does not, and the verdict (adopt / part of stack /
reject).

### 1. mTLS with an operator-provisioned key (one-time pairing)

**What it is.** The daemon holds a client cert + private key on disk. The
controller's TLS handler authenticates the client cert against a CA we
manage (or a per-daemon SPKI pin). The operator runs a pairing command
once to provision the cert.

**Pros.**

- Long-lived, low per-request cost. After pairing, no token refresh dance,
  no token-expiry edge cases on the hot path.
- The private key never leaves the laptop (modulo backup hygiene).
- The cert's subject can carry operator identity + daemon ID, which is
  exactly the information we need at the controller.
- Resists replay at the channel layer: every TLS session uses an ephemeral
  key exchange; an attacker who captures a session cannot replay it.
- Matches the way Tailscale, 1Password CLI's pairing flow, and SSH
  certificates work — operators recognise the shape.

**Cons.**

- Operator UX for first-time pairing has to be airtight. "Where is my key
  file?" "Why does my daemon say PAIRING_REQUIRED?" are real failure
  modes.
- Certificate rotation is a real chore. We need a story for: cert nearing
  expiry (auto-renew over the existing channel), cert revoked (CRL or
  short cert lifetimes), laptop lost (operator-side "revoke this device"
  action in beads-viewer).
- The AVA edge complicates server-side cert termination — AVA terminates
  TLS at the edge today; passing the client cert through to the controller
  requires either AVA mTLS-passthrough (supported but not currently wired)
  or a parallel ingress that bypasses AVA for daemon traffic.
- Cert compromise is invisible to the operator if the laptop is
  compromised — same blast radius as a stolen bearer token, but harder to
  rotate quickly.

**Verdict.** **Adopt as Layer 2** (daemon identity). The private-key-never-
leaves-laptop property is the strongest authentication primitive on the
table for laptop-resident daemons. The cert-rotation and AVA-passthrough
costs are real but bounded; we already pay them for service-to-service
identity in other parts of the platform.

### 2. OIDC device flow with refresh token (mirror `gb login`)

**What it is.** The daemon prompts the operator at first run to visit
`<viewer>/auth/cli-handoff` (or a device-flow code page), captures the
operator's OIDC tokens, and refreshes them periodically. Every action
carries the operator's bearer token to the controller.

**Pros.**

- Reuses 100% of the existing `gb login` machinery. Less new code.
- Operator identity is unambiguous — the controller sees the operator's
  OIDC subject on every request.
- Tokens are short-lived; refresh-token compromise is easier to revoke
  (single Keycloak admin click) than cert revocation.

**Cons.**

- Per-action token presentation puts the operator's *primary* OIDC creds
  on the hot path. A daemon compromise lifts the refresh token →
  attacker can now impersonate the operator against *every* OIDC-protected
  service, not just the daemon's spawn capability. Blast radius is
  unbounded.
- Refresh tokens have to live on disk. We already pay this cost for
  `gb login`, but a long-running daemon process holding a refresh token
  is a fatter attack target than an interactive CLI session.
- Device-flow + refresh assumes the daemon can always reach Keycloak. If
  the operator is offline, the daemon has nothing to present.
- No clean story for "this action came from this specific decision bead" —
  bearer-token semantics give us "this operator is allowed to act" but
  not "this operator clicked this button at this time on this bead."

**Verdict.** **Reject as the primary mechanism**; **adopt as the
authentication step inside the pairing flow** (Layer 2). The pairing
flow uses the existing browser handoff to prove operator identity once,
mint a per-daemon cert keyed to that identity, and never touch the
operator's refresh token again on the daemon side.

### 3. Long-lived websocket back to controller, pinned with AVA-signed key

**What it is.** The daemon initiates a websocket to the controller and
keeps it open. The controller pushes spawn-action payloads down the
socket. The daemon does not need to be reachable from the public internet;
NAT/firewall is a non-issue because the daemon connects out. The socket is
authenticated by either an AVA-issued key or the mTLS cert from option 1.

**Pros.**

- **Solves the NAT/firewall problem**, which is the gating constraint for
  any laptop-resident daemon. We do not need port forwarding, ngrok-style
  tunnels, or a public-internet listener on operator laptops.
- Long-lived channels amortise the TLS handshake + auth cost. After
  pairing, the controller is talking to a known-authenticated daemon for
  hours.
- Bidirectional. The daemon can stream heartbeats, status, and (in the
  future) richer client-side telemetry back without the controller having
  to poll.
- Plays well with AVA at the edge: the websocket upgrade carries the
  same AVA-gated TLS termination as every other gardenio HTTPS request.

**Cons.**

- Long-lived sockets are harder to operate. Reconnect storms, half-closed
  sockets, and load-balancer idle timeouts are real failure modes. AWS
  ALBs have a 60s idle default; we will need either heartbeats or a
  K8s-controller-aware load balancer.
- The "this operator's daemon is uniquely identified" question still
  lives on top of the socket. The socket itself is a transport;
  authentication is a layer above.
- A compromised daemon with a live socket is a persistent foothold inside
  the controller's logical perimeter — narrower blast radius than a
  refresh token, but still significant.

**Verdict.** **Adopt as Layer 1** (channel). The websocket is the
transport; the cert from Layer 2 is what proves identity over the socket;
the JWT from Layer 3 is what authorises each action carried by the socket.
"Pinned with AVA-signed key" is a fine description for the server-side
half — the daemon validates the server cert chain like any TLS client.

### 4. Existing `gb login` + Keycloak client-creds patterns

**What it is.** The shape of existing identity mechanisms in the platform.

`gb login` (cookie handoff) is appropriate for *interactive* CLI users.
Re-using it verbatim for a long-running daemon would put a session cookie
on disk for hours — equivalent to the OIDC-refresh-token problem from
option 2.

Client-credentials (`lib-keycloak-client-creds.sh`) is appropriate for
*service-to-service* identity where a fleet of agent pods all share an
issuer-trusted credential. A daemon-per-operator does not fit cleanly:
each daemon needs its own identity, not a fleet-shared one.

**Verdict.** **Reuse the cookie-handoff UX shape for pairing**; **reuse
the keycloak-client-creds machinery for short-lived per-action JWT minting
on the controller side, only if we decide to keep Keycloak in the critical
path for daemon JWTs**. If we want to avoid that, the controller can sign
JWTs with a local KMS key — simpler, fewer moving parts, but loses the
keycloak audit trail.

### 5. Per-action signed payload bound to the decision bead

**What it is.** Independent of how the daemon authenticates, every spawn
action carries a short-lived JWT minted by the controller at the moment
the operator clicks the action button. The JWT carries `decision_id`,
`aud`, `sub`, `nonce`, `exp`, signed with a controller key.

**Pros.**

- Tightly couples the action to a specific decision bead. The daemon can
  verify the JWT independently — no controller round-trip needed.
- Replay-resistant: short `exp` (≤60s) + nonce stored in a short-lived
  daemon-side cache makes replay infeasible.
- Decouples "the operator is logged in" from "this specific decision was
  acted on." The decision bead is the artifact; the JWT is the receipt.
- Standard, well-understood, easy to audit.

**Cons.**

- Adds a JWT verification path on the daemon side (libraries are
  ubiquitous, but it is still one more crypto surface).
- Requires the daemon to trust a specific controller signing key. That
  trust is established at pairing time (controller publishes its public
  key during pairing; daemon pins it).

**Verdict.** **Adopt as Layer 3** (per-action authorisation). The JWT is
the action receipt; the decision bead is the audit trail; the daemon's
replay cache prevents re-use. This is the layer that answers "is this
spawn real and current?"

---

## Comparison table

| Property                           | mTLS pairing             | OIDC device + refresh    | Long-lived websocket     | Cookie handoff (`gb login`) | Keycloak client-creds | Per-action JWT bound to decision |
|------------------------------------|--------------------------|--------------------------|--------------------------|-----------------------------|----------------------|---------------------------------|
| Solves NAT/firewall (laptop)       | only as a transport      | yes (HTTPS out)          | **yes**                  | yes (HTTPS out)             | yes (HTTPS out)      | n/a — orthogonal                |
| Proves operator identity           | yes (cert subject)       | **yes** (sub claim)      | no (transport only)      | yes (cookie)                | no (service identity)| yes (sub claim)                 |
| Proves controller identity to daemon | yes (CA / SPKI pin)    | yes (issuer pin)         | yes (TLS server cert)    | yes (TLS server cert)       | yes (issuer pin)     | yes (signer pin)                |
| Binds action to decision bead      | no                       | no                       | no                       | no                          | no                   | **yes**                         |
| Replay resistant                   | session-level only       | refresh-replayable       | session-level only       | cookie-replayable on TLS     | token-replayable     | **yes** (exp + nonce)           |
| Blast radius of stolen secret      | this daemon only         | **all OIDC services**    | this daemon only         | beads-viewer cookie only    | the whole client     | one spawn (60s window)          |
| Disk footprint on operator laptop  | cert + key (PEM)         | refresh token            | (cert + key)             | cookie bundle               | client_secret        | nothing persistent              |
| Operator-revocable in beads-viewer | yes (revoke device)      | partially (sign-out)     | yes (close session)      | yes (sign-out)              | admin only           | n/a — short-lived               |
| Per-request crypto cost            | TLS handshake amortised  | bearer header            | amortised                | cookie header               | bearer header        | JWT verify                      |
| Operator UX cost (first run)       | pairing flow (one click) | sign-in (one click)      | n/a — under another      | sign-in (one click)         | n/a — service        | n/a — under another             |
| Plays with AVA at edge             | passthrough required     | yes (today)              | yes (today)              | **yes** (this is how it works) | yes (today)        | yes — independent               |
| Long-running daemon friendly       | **yes**                  | refresh-token risk       | **yes** (purpose-built)  | cookie expiry risk          | yes (service)        | yes — independent               |
| Audit story                        | TLS session logs         | OIDC issuer audit        | controller session log   | viewer access log           | issuer audit         | **JWT + decision bead**          |
| Net verdict                        | **Layer 2**              | **Pairing step only**    | **Layer 1**              | **Pairing UX shape**        | **Optional signer**  | **Layer 3**                     |

---

## Recommended architecture

The three layers, end-to-end.

### Layer 1 — Channel: long-lived authenticated websocket

- Daemon initiates a TLS connection to `wss://<viewer-host>/api/v1/daemon/stream`.
- The websocket terminates at the AVA edge (or a sibling ingress without
  AVA — see Decision D1 below).
- Server cert is the controller's normal TLS server cert; daemon
  validates it like any TLS client (system trust store).
- Client cert (Layer 2) is presented on the handshake; controller's
  websocket handler rejects connections without a valid client cert
  matching a known daemon.
- Heartbeats every 30s (configurable) keep the socket alive through
  ALB idle timeouts. Daemon reconnects with exponential backoff on
  disconnect.
- Per-message envelope: `{type, message_id, payload}`. Types include
  `action.spawn-escalated-agent.request`, `action.*.response`,
  `daemon.heartbeat`, `daemon.status`, `daemon.shutdown`.

### Layer 2 — Daemon identity: one-time pairing → per-daemon client cert

Pairing UX (mirrors `gb login`):

1. Operator clicks "Pair this device" in beads-viewer (AVA-gated; the
   operator is already authenticated).
2. beads-viewer renders a pairing page with a short pairing code (e.g.
   `GARDENIO-A4F1-7E2C`) and a "waiting…" spinner. The code is a server-side
   one-shot token with TTL ≤5 minutes.
3. Operator runs `gb daemon pair GARDENIO-A4F1-7E2C` on the laptop.
4. The daemon generates a P-256 keypair, sends a CSR with subject
   `CN=<daemon-id>, O=<operator-sub>` to `POST /api/v1/daemon/pair?code=…`.
5. Controller verifies the pairing code, mints a client cert (signed by
   the gardenio daemon-CA, valid for 90 days), records `(daemon_id,
   operator_sub, fingerprint, paired_at)`, and returns the signed cert.
6. Daemon stores cert + key at `~/.config/gardenio/daemon/{cert.pem,key.pem}`
   with 0600.
7. beads-viewer pairing page polls `/api/v1/daemon/pair-status?code=…`,
   flips to "paired" once the cert is minted, and shows the daemon-id +
   first-seen timestamp.

Operator self-service:

- `/account/devices` page in beads-viewer lists paired daemons. Revoking
  a daemon adds its fingerprint to a server-side CRL; the next websocket
  reconnect fails closed.
- `gb daemon status` shows pairing state, cert expiry, last connect time.
- `gb daemon unpair` cleanly tears down (delete cert, notify controller).

Cert rotation:

- 30 days before expiry, daemon presents a renewal CSR over the existing
  websocket; controller issues a new cert. No re-pairing needed.
- If the daemon is offline at expiry, it must re-pair from the laptop
  (one click in beads-viewer + one CLI command). This is the same UX as
  first pairing and is rare enough (90 days) to be acceptable.

Multi-operator collision:

- Each (operator_sub, daemon_id) is a unique row. Two daemons paired to
  the same operator each have their own cert and show up separately in
  `/account/devices`. The operator picks "Spawn on… laptop-pro" from a
  dropdown in beads-viewer; the action JWT names the chosen daemon by id.
- Two operators pairing on the same physical laptop is supported via
  separate credstore contexts (already a concept in `credstore`).

### Layer 3 — Per-action authorisation: short-lived JWT bound to decision bead

When the operator clicks "spawn escalated agent" in beads-viewer on
decision bead `ka-XXXX` targeting daemon `daemon-Y`:

- beads-viewer (server-side) verifies (a) the operator is signed in,
  (b) the decision bead is in `open` state and is in the operator's
  scope, and (c) `daemon-Y` is paired to the same operator.
- Controller signs a JWT with:
  - `iss=https://<viewer-host>`
  - `aud=daemon-Y`
  - `sub=<operator-sub>`
  - `decision_id=ka-XXXX`
  - `action=spawn-escalated-agent`
  - `nonce=<random-128-bit>`
  - `iat`, `exp=iat+60s`
- Controller pushes `{type: action.spawn-escalated-agent.request,
  message_id, payload: {jwt, decision_snapshot}}` down the daemon
  websocket.
- Daemon verifies the JWT (signature against the controller signing key
  pinned at pairing time, `aud` matches own daemon-id, `exp` not passed,
  `sub` matches the cert's operator binding).
- Daemon checks its nonce-cache (in-memory ring, 5-minute window). If
  seen, reject as replay. Otherwise insert + proceed.
- Daemon hits `GET /api/v1/decision/ka-XXXX/state` to confirm the bead is
  still `open` (defence-in-depth against an attacker who somehow obtained
  an unused JWT for a since-resolved decision). For the spawn action this
  is a cheap RPC; the response carries the latest state + a short
  validity window.
- Daemon executes the spawn. Records `daemon.action.completed` with
  the message_id back over the socket. Controller writes an audit
  comment on the decision bead.

Signing key choice:

- **Preferred**: controller-local signing key managed by KMS (AWS KMS or
  in-cluster softHSM). Pinned in the daemon at pairing time. Simpler,
  fewer external dependencies, well-understood rotation story (publish
  next key, daemon accepts both for the rotation window, retire old).
- **Alternative**: Keycloak-issued tokens via the existing client-creds
  machinery. Reuses more infra, gives a unified audit trail in Keycloak,
  but puts Keycloak in the spawn-action critical path. Acceptable, not
  preferred.

Synthesis decision will pick one (see "Decision needed").

---

## Threat model

The brief named five threat scenarios. Each one below: what the attack
looks like, what the recommended stack does about it, residual risk.

### T1. Replay of an old spawn payload

**Attack.** Attacker captures a `spawn-escalated-agent` payload on the
wire (or from logs, or from a compromised intermediary) and replays it
later, hoping the daemon will spawn an agent on the operator's behalf.

**Mitigations.**

- Layer 1: TLS provides session-level confidentiality + integrity.
  Capturing on the wire requires breaking the TLS session, which requires
  Layer-2-grade attacker access already.
- Layer 3: JWT `exp ≤ 60s`. By the time the JWT is captured, it is
  almost certainly expired.
- Layer 3: nonce + daemon-side replay cache (5min window) — a replay
  inside the 60s window is rejected.
- Layer 3: defence-in-depth decision-state check — even if everything
  above somehow fails, the daemon refuses to spawn for a decision bead
  that is no longer `open`.

**Residual risk.** Vanishingly low. An attacker would need to capture
a JWT inside its ≤60s validity window AND defeat the daemon's nonce cache
AND get the daemon to act before the decision bead is resolved. The only
realistic path is full control of the daemon process, which is its own
threat class (T3).

### T2. MITM on the controller→daemon channel

**Attack.** Attacker on the network path between daemon and controller
(coffee-shop wifi, hostile ISP, compromised corporate proxy) attempts to
read or inject messages.

**Mitigations.**

- TLS server cert pinned in the daemon at pairing time. The daemon
  validates the cert chain against the system trust store *and* checks
  that the server's leaf cert is signed by a CA that issued an
  intermediate the daemon trusts. (Standard TLS, no DIY pinning.)
- Layer 2: daemon presents its client cert. A MITM cannot inject a
  forged client connection without the daemon's private key.
- Layer 3: JWT signature. Even if the channel were somehow broken, a
  forged action payload without a valid JWT signature is rejected.

**Residual risk.** Negligible against passive eavesdroppers and routine
TLS MITM (corporate proxies). A nation-state attacker holding a private
CA the operator's machine trusts could theoretically MITM TLS — same
threat class as MITMing any HTTPS site. The cert-pin model we recommend
(daemon pins the controller's leaf via the issuing CA fingerprint) makes
even that case harder.

### T3. Stolen daemon token / stolen operator OIDC token

**Attack.** Attacker compromises the operator's laptop and exfiltrates
either the daemon's client cert + private key, or the operator's OIDC
refresh token from `gb login`.

**Mitigations.**

- The recommended stack does **not** hand the operator's OIDC refresh
  token to the daemon at all. Operator OIDC is used once during pairing;
  the daemon never sees it. A laptop compromise can still steal an OIDC
  refresh token from `~/.config/gardenio/credentials.json` if `gb login`
  was used — but that surface is unchanged by this design and is shared
  by `kd`, `gb`, and `coop`.
- If the daemon's client cert + key is exfiltrated, the attacker can
  impersonate this daemon to the controller. The blast radius is bounded
  to the operator's spawn-allowed actions — they cannot impersonate the
  operator on Keycloak, GitLab, or any other OIDC-protected service.
- Operator-visible revocation (`/account/devices` → "revoke") flips the
  cert to revoked on the next reconnect attempt. Cert lifetime is 90
  days; a stolen cert is useless after that even without revocation.
- Layer 3 JWTs cannot be minted without operator interaction in beads-
  viewer. Even with a stolen daemon cert, the attacker cannot spawn an
  agent without the operator clicking the action button (or having an
  attacker-controlled session to the viewer — which is a different
  threat).

**Residual risk.** A laptop compromise is a serious event regardless;
this design narrows the blast radius from "all OIDC services" to "this
operator's spawn capability, until cert is revoked." That is the best we
can reasonably do for a laptop daemon.

### T4. Daemon proves identity to controller; controller proves the action came from a real unresolved escalation

This is two distinct asks.

**Daemon → controller authentication.** Layer 2 cert. The daemon's
private key is the proof; the controller validates the client cert at
the TLS handshake. No tokens to refresh, no per-request liveness check.

**Controller → daemon: "this action came from a real escalation."**
Layer 3 JWT. The JWT names the decision bead by id, is short-lived,
nonce-tracked, and bound to a specific daemon. The daemon's defence-in-
depth state-check confirms the decision bead is still open before
acting.

**Residual risk.** None on the protocol level. The only way for the
controller to push a fake spawn is if the controller itself is
compromised — same trust assumption as everywhere else in the system.

### T5. Multi-operator collision (two daemons paired to one operator account)

**Attack class.** Not an "attack" so much as a UX hazard: an operator
with two laptops pairs both. The controller has to know which daemon to
push to.

**Mitigations.**

- Each daemon gets a unique daemon-id at pairing time. The `/account/
  devices` page lists all paired daemons with their hostnames and last-
  seen times.
- The "spawn escalated agent" action in beads-viewer renders a target
  dropdown when the operator has >1 paired daemon. Default is the most-
  recently-connected daemon.
- The action JWT names the chosen daemon by id (`aud=daemon-Y`); the
  controller pushes only down that daemon's socket. The other daemon
  never sees the action.
- If a daemon is offline at action time, beads-viewer shows a "this
  daemon is offline — choose another or queue" affordance. Queued
  spawns are bounded (e.g. expire in 5 minutes) and visible in
  beads-viewer until the daemon reconnects.

**Residual risk.** Low. Worst-case: operator picks the wrong daemon and
the spawn lands on the wrong laptop. The audit trail (decision bead
comment) names the daemon by id, so the operator notices quickly.

### T6 (additional). Compromised daemon as a persistent foothold

Not in the original brief but worth flagging.

**Attack.** Attacker compromises the daemon process (RCE in the daemon
itself, a malicious dependency, an operator running a malicious install
script). Daemon now has a live, authenticated channel into the
controller.

**Mitigations.**

- The daemon's *capabilities* over the channel are restricted to the
  daemon-protocol message types (spawn, heartbeat, status). It cannot
  issue arbitrary controller RPCs.
- Layer 3 JWTs are required for every action; the compromised daemon
  cannot mint them, so cannot self-spawn agents.
- The controller treats every message *from* the daemon as untrusted
  input — log lines, status reports, etc. are scrubbed before being
  shown to operators or written to beads.
- All daemon-originated state changes are surfaced as audit events on
  the relevant bead (decision bead, agent bead, etc.).

**Residual risk.** A compromised daemon can lie about its status,
silently drop spawn actions, or exfiltrate the operator's local
environment. None of this is *worse* than a laptop compromise in
general. The mitigation is daemon code review, supply-chain hygiene
(signed releases, reproducible builds — covered by the daemon-lifecycle
research child), and operator-visible revocation.

---

## Open questions answered

From the epic's open-questions list, this research is responsible for two.

**Q: How does the daemon discover its operator identity?**

A: One-time browser-handoff pairing flow, modelled on `gb login`. The
operator clicks "Pair this device" in beads-viewer (AVA-gated, so they
are already authenticated). beads-viewer mints a short-lived pairing code,
the operator runs `gb daemon pair <code>` locally, the controller
verifies the code in the operator's session, mints a per-daemon client
cert bound to the operator's `sub`, and records the pairing. Operator
identity is consumed *once* and then carried by the cert.

The daemon does not re-authenticate against the operator's OIDC tokens
on every action. It does not need to — the cert is the proof, and Layer
3 JWTs (signed by the controller in response to operator clicks in
beads-viewer) provide per-action authorisation.

**Q: Do we require the daemon to be reachable from the controller, or
pull over a long-lived websocket?**

A: Long-lived websocket, daemon-initiated. Reasons:

- Solves NAT/firewall on operator laptops without exotic
  ngrok/cloudflared-style tunnels.
- Lets the controller push spawn actions without polling.
- Provides a single authenticated channel for future capabilities
  (heartbeat, status, future-action telemetry).
- Matches the deployment model for other "daemon talks to cloud
  controller" tools the operator may already run (Tailscale, 1Password
  CLI agent).

The daemon does not need a public-internet listener. Reconnect logic is
the same shape as Tailscale's `tailscaled` connection to the
coordination server.

---

## Decisions needed (synthesis bead)

Items below are deliberately *not* decided in this doc; they belong to
the epic's Phase 2 synthesis decision. This doc gives the recommendation
+ evidence; synthesis picks.

**D1. AVA edge: mTLS passthrough or sibling ingress?**

The daemon needs to present its client cert to the controller. AVA
terminates TLS at the edge today. Two options:

- (a) Enable mTLS passthrough on the AVA endpoint. AVA supports this for
  IAM-policy purposes. Wires cleanly into the existing
  `beads.gardenio.e2e.example.com` hostname.
- (b) Add a sibling ingress (e.g. `daemon.beads.gardenio.app.e2e.dev.acme.
  ai`) that bypasses AVA and terminates client cert auth directly at the
  controller / Traefik. Each AVA endpoint is ~$197/mo (see
  `ava-edge-auth-prototype-learnings.md`), so this is a cost decision as
  much as an arch decision.

Recommend (a) — single ingress, single auth surface, fewer moving parts.
Synthesis should validate AVA mTLS-passthrough actually works against our
specific Verified Access setup. (Spike work: ~½ day.)

**D2. JWT signer: controller-local KMS, or Keycloak?**

Both work. Controller-local KMS is simpler and faster; Keycloak gives a
unified audit trail. The doc recommends controller-local KMS; synthesis
should confirm whether platform-team's auth-audit story needs Keycloak
in the path.

**D3. Cert lifetime + rotation cadence**

90 days is a reasonable default (matches Let's Encrypt cadence,
balances rotation cost against revocation latency). Synthesis can revisit
once we have a feel for operator usage patterns. Auto-renewal over the
existing websocket means cert rotation is invisible to the operator;
the 90-day number primarily affects "how long is a stolen cert useful?"

**D4. Daemon-id allocation: client-chosen or server-allocated?**

Server-allocated is safer (no client-side collision; controller fully
controls the namespace). Use `kd-`-style short ids
(`gbd-<base32-8>`). Daemon stores the id in `~/.config/gardenio/daemon/
daemon-id`. Operator-visible in `/account/devices` and in `gb daemon
status`.

**D5. Offline operator: queue or refuse?**

When the daemon is offline at action time, beads-viewer can either (a)
queue the spawn until the daemon reconnects (TTL ≤5 minutes), or
(b) show a clear "daemon offline" error and let the operator retry.

Recommend (b) by default; (a) as an explicit per-daemon opt-in. Queued
spawns increase ambiguity about *when* the action took effect; better to
make offline failures visible.

---

## What this doc does NOT decide

- The on-disk format for the cert + key (PEM vs DER, file-paths vs single
  blob). Trivial implementation detail.
- The exact websocket message envelope (protobuf vs JSON vs MessagePack).
  Synthesis can pick; recommendation is JSON for parity with the existing
  controller HTTP surface, switching to protobuf only if profiling
  demands it.
- The CLI ergonomics of `gb daemon pair / status / unpair`. Sister task
  on daemon lifecycle (ka-0000006867) owns the CLI surface; this doc
  only assumes those subcommands exist.
- The permissions model the daemon enforces *after* a spawn is
  authorised. Separate research child.
- The audit-trail format on the decision bead (comment? structured
  event? widget instance on a rich-report?). Spawn-action-wire research
  child owns this.

---

## Code anchors (for the synthesis MR, not for this research MR)

- `gardenio/controller/cmd/gb/login.go` — pairing UX pattern source.
- `gardenio/controller/internal/credstore/credstore.go` — extend with
  daemon cert/key field, or add a sibling `daemonstore` package.
- `gardenio/controller/internal/oidcauth/oidcauth.go` — middleware
  pattern; daemon-cert verification can plug in as a parallel handle.
- `gardenio/controller/internal/avareauth/reauth.go` — AVA cookie
  refresh pattern; the daemon-pair endpoint must run *under* AVA.
- `gardenio/controller/internal/edgeauth/` — ForwardAuth pattern; if D1
  picks mTLS-passthrough, the controller must accept client certs from
  the edge.
- `gardenio/docs/agent-pod-keycloak-auth.md` — client-creds reference.
- `gardenio/tests/e2e/scripts/lib-keycloak-client-creds.sh` — if D2 picks
  Keycloak, JWT minting plumbs through this.
- `kbeads/internal/server/decision_*.go` — decision-state RPC; the
  daemon's defence-in-depth check hits a small endpoint here.

---

## Linkbacks

- Bead: ka-0000005882
- Parent epic: ka-0000006791
- Sister research: ka-0000006867 (daemon lifecycle), ka-0000005690 +
  ka-0000006932 + ka-0000004516 (other Phase 1 children)
- Sister epic: ka-0000004527 (top-level escalations surface)
- Dogfooding pattern: ka-0000006879 (free-htmx widget research)
- Reference: `gardenio/docs/research/ava-edge-auth-prototype-learnings.md`
- Reference: `gardenio/docs/agent-pod-keycloak-auth.md`
- Reference: `gardenio/docs/research/forwardauth-403-force-logout.md`
