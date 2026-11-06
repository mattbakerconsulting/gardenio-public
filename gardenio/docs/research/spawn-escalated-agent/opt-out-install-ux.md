# Operator opt-out & first-run install UX — `gb-daemon`

> Research child 9 of 9 for **ka-0000006791** (epic: client-side `gb` daemon
> for "spawn escalated agent").
>
> **Bead:** ka-0000005690
> **Parent epic:** ka-0000006791
> **Builds on:** ka-0000006867 (daemon lifecycle & install model — child 1),
> ka-0000005882 (authn/authz — child 2), and the spawn-action-wire child
> (which explicitly defers operator install / first-run to *this* doc — see
> `spawn-action-wire.md` §"deferred to ka-0000005690").
> **Sister pattern:** ka-0000004527 (escalations top-level surface)
> **Dogfood mandate:** ka-0000006879 (rich-report bundle deliverable)
>
> **Status:** research / recommendation only. No code lands until a
> phase-2 `gb decision` ratifies the daemon shape. This child specifies the
> *operator experience*, not the implementation.

---

## TL;DR

| Question | Recommendation |
| --- | --- |
| **Opt-out default** | The daemon is **strictly opt-in**. An operator who never installs it sees a fully-functional beads-viewer with **zero daemon nags**. The "Spawn locally" secondary action **renders but is disabled** (never hidden), with a one-line inline hint + a dismissible "Set up local spawn" info card that self-suppresses after one dismissal. Every escalation/decision remains fully actionable server-side without a daemon — local spawn is an *accelerator*, never a gate. |
| **Why disabled-not-hidden** | A hidden control teaches operators the capability does not exist; a disabled control with a hint teaches "you *could* have this." Hiding also breaks muscle memory for operators who have the daemon on one machine and not another. |
| **Install path** | **Primary: Homebrew tap** `acme/gardenio` → `brew install gardenio` then `gb daemon install`. **Fallback: `curl … | sh`** through the *same* installer entrypoint (never a second code path). **In-app: platform-detected copy-paste command** surfaced from beads-viewer (we detect OS from the `User-Agent` and show the right one-liner; we do **not** ship a binary download button). No GitHub release path — gardenio lives on GitLab; the tap formula points at the GitLab release assets. |
| **First-run / pairing** | **beads-viewer-seeded pairing code + `gb daemon pair <code>`**, reusing the `gb login` cookie-handoff UX shape and the Layer-2 mTLS pairing already specified in child 2. Not a QR code (desktop-to-desktop, no phone in the loop). Not raw OIDC device flow at pair time (the operator is *already* AVA-authenticated in the browser; a second device-code dance is redundant friction). |
| **Uninstall** | Two verbs. `gb daemon unpair` = revoke credentials, keep binary (controller CRLs the cert). `gb daemon uninstall` = unpair **and** remove the service unit + all `~/.config/gardenio/daemon` state + pinned-version cache. Uninstall always unpairs first so a removed daemon can never leave a live cert dangling. |
| **Multi-machine** | **Explicit named devices, no implicit primary, no last-write-wins.** Each paired daemon is a distinct `(operator_sub, daemon_id)` row with an operator-chosen label (`laptop-pro`, `desktop-home`). The Spawn-locally control offers a **device picker** when >1 daemon is online; a single online daemon auto-selects; zero online falls back to the disabled/offline state. Conflicts are impossible because the operator names the target per spawn. |

The rest of this document walks each question, the precedent, the trade-offs,
and the concrete operator-facing surfaces. The companion **rich-report
bundle** (per the dogfood mandate) is the operator-facing summary.

---

## 1. Framing — the daemon is an accelerator, not a dependency

The single most important product constraint, inherited from the epic and
from child 1 (`daemon-lifecycle.md` §1): the gb-daemon's **only consumer is
the controller**, and its only job is to turn one decision option — "spawn
this escalated agent on *my* machine instead of in the fleet" — from
impossible into a click. Everything an operator can do today in
`/escalations` and `/decisions` they must still be able to do with **no
daemon installed at all**:

- Resolve a decision by picking a server-side option.
- Approve/merge an MR via the escalation's server-side action.
- Dismiss, defer, comment, reassign.

The daemon adds exactly **one** capability: running the spawned agent session
in the operator's *local* trust and filesystem context (their unmerged
working tree, their local creds, their laptop's network position). That is
valuable — it is the whole point of the epic — but it is **additive**. The
opt-out design therefore starts from a hard rule:

> **Rule O-1.** No server-side capability may ever be gated behind daemon
> presence. If beads-viewer would show a "Spawn locally" action, it must
> *also* show at least one server-side path to resolve the same
> escalation/decision. The daemon can only ever *add* a faster path.

This rule is what makes an aggressive opt-out (below) safe: an operator who
declines the daemon forever loses nothing but a convenience.

---

## 2. Opt-out behavior — what the daemon-less operator sees

### 2.1 The decision: disabled, not hidden

Three candidate treatments for the "Spawn locally" secondary action when no
daemon is reachable:

| Treatment | Pro | Con | Verdict |
| --- | --- | --- | --- |
| **Hide entirely** | Cleanest UI; zero nag | Teaches "this capability doesn't exist"; breaks muscle memory across machines; makes the feature undiscoverable | ✗ reject |
| **Grey-out + inline hint** | Discoverable; honest ("you could have this"); one-line, low-nag | One extra disabled chip per card | ✓ **adopt** |
| **One-click "Install now" CTA inline** | Maximally discoverable | Nags on every card; pushes an install the operator may not want; conflates "resolve this escalation" with "change my machine setup" | ✗ reject as default (offer once, in a card — §2.2) |

We adopt **grey-out + inline hint**. This mirrors the shape already sketched
in `spawn-action-wire.md` for the *daemon-installed-but-offline* case
(amber "Recheck local daemon (offline)" chip), and extends it to the
*never-installed* case. The two states are visually distinct:

```
Never installed (opt-out default):
┌────────────────────────────────────────────────────────────────────────┐
│ Rotate RWX_ACCESS_TOKEN  [public]  [internal]  [in progress]           │
│  ka-0000001006 · gardenio · agent foo-9b2 · for ka-0000006791 · 3h ago  │
│  ┌──────────┐   ┌───────────────────────┐                              │
│  │ Review › │   │ Spawn locally ⤓ (off) │  ⓘ set up local spawn →     │
│  └──────────┘   └───────────────────────┘                              │
└────────────────────────────────────────────────────────────────────────┘
        ▲ server-side action, always live       ▲ disabled; hint links to card

Installed but offline (transient — see failure-modes child):
│  ┌────────────────────────────────┐  ⓘ daemon not reachable            │
│  │ Recheck local daemon  (offline)│   install: `brew gb`               │
│  └────────────────────────────────┘                                    │
```

The distinction matters: "never installed" links to a **one-time setup
card**; "installed but offline" links to a **recheck / restart** flow,
because the operator has already opted in and the right recovery is
"un-stick the service," not "install it."

### 2.2 The one-time setup card (dismissible, self-suppressing)

The first time an operator lands on `/escalations` or `/decisions` with at
least one spawn-capable action **and** no paired daemon, beads-viewer renders
a single dismissible banner above the queue:

```
┌────────────────────────────────────────────────────────────────────────┐
│ ⓘ Spawn agents on your own machine                                  ✕  │
│                                                                        │
│ Some escalations can be handled by spawning the agent in your local    │
│ working tree instead of the fleet. Set up the gb daemon once:          │
│                                                                        │
│   macOS:   brew install acme/gardenio/gardenio && gb daemon install  │
│   Linux:   curl -fsSL https://beads.gardenio…/install.sh | sh           │
│                                                                        │
│   Then:    gb daemon pair   (opens a pairing code here)                │
│                                                                        │
│   [ Copy command ]   [ Learn more ]   [ Not now ]   [ Don't show again ]│
└────────────────────────────────────────────────────────────────────────┘
```

Suppression rules:

- **"Not now"** hides it for the current session (a `sessionStorage` flag).
- **"Don't show again"** persists a per-operator preference
  (`operator_pref: daemon_setup_dismissed=true`, stored on the operator's
  config/user bead so it follows them across browsers) — the banner never
  returns unless they visit `/account/devices` and explicitly re-enable it.
- The banner **auto-suppresses permanently** the moment any daemon pairs
  successfully for this operator — success is the strongest possible signal
  that the nag has done its job.
- Platform detection: beads-viewer picks the macOS vs Linux one-liner from
  the request `User-Agent`; both are always copy-pasteable, and a "show all
  platforms" disclosure expands the rest.

> **Rule O-2.** The setup card is shown **at most once per operator** by
> default and is *always* dismissible with a durable "don't show again."
> The disabled chip (§2.1) is the persistent, low-nag discovery surface;
> the card is the one-time onboarding nudge. We never nag on every card.

### 2.3 The full opt-out matrix

| Operator state | `/escalations` & `/decisions` server-side actions | "Spawn locally" chip | Setup card | Notes |
| --- | --- | --- | --- | --- |
| **Never installed** (opt-out) | fully live | disabled + "set up" hint | shown once, then suppressed | Rule O-1 guarantees nothing is blocked |
| **Dismissed "don't show again"** | fully live | disabled + hint (still there) | never | chip persists as quiet discovery |
| **Installed, unpaired** | fully live | disabled + "pair this device" hint | replaced by "finish pairing" chip | one CLI step away |
| **Paired, online** | fully live | **enabled** | none | device picker if >1 (§5) |
| **Paired, offline** | fully live | "Recheck / offline" (amber) | none | recovery = restart, not install (→ failure-modes child) |
| **Paired, cert expired** | fully live | disabled + "re-pair this device" | none | 90-day rotation lapse (→ child 2) |
| **Uninstalled after use** | fully live | back to disabled + "set up" | shown once again *only if* they cleared the pref | treated as fresh opt-out |

The invariant across every row: **column 1 is always "fully live."** The
daemon column is the only thing that changes.

### 2.4 Recovery path for the operator who declines

An operator who never wants the daemon has a complete, permanent path:

1. Dismiss the setup card with "Don't show again."
2. The disabled chip remains but is unobtrusive; it never blocks a click on
   the real server-side action beside it.
3. If they later change their mind, `/account/devices` has a **"Set up local
   spawn"** button that re-opens the card and the pairing flow on demand.

No dead ends, no re-nagging, no lost capability. This is the whole promise of
opt-out.

---

## 3. Install paths — survey & recommendation

### 3.1 Candidates

The bead names four candidate install mechanisms. Verdicts:

| Path | What it is | Verdict | Rationale |
| --- | --- | --- | --- |
| **Homebrew tap** | `brew install acme/gardenio/gardenio`, then `gb daemon install` writes the launchd/systemd unit (child 1) | ✓ **primary (macOS)** | Matches Tailscale/`gh`/`op` precedent; operators expect `brew`; the tap formula versions with the calver line; upgrades are `brew upgrade` |
| **`curl … | sh`** | One-liner that downloads the platform binary and runs the *same* `gb daemon install` entrypoint | ✓ **fallback (Linux + no-brew macOS)** | Must funnel through the identical installer code path — never a divergent second flow. Signed binary + checksum verified in the script |
| **GitHub release** | Download binary from a GitHub releases page | ✗ **reject** | gardenio lives on **GitLab**, not GitHub. The tap/`curl` scripts point at **GitLab release assets** (`<registry>/gardenio`). No GitHub surface exists to maintain |
| **In-app binary download button** | beads-viewer serves the binary directly for the operator's OS | ✗ **reject** | Serving executables from the web app is a supply-chain and code-signing headache (Gatekeeper/notarization, SmartScreen). Instead beads-viewer shows the **platform-detected copy-paste command** that routes through brew/`curl`, where signing already lives |

### 3.2 The single-entrypoint rule

Both supported paths (`brew` and `curl|sh`) converge on the **same** binary
and the **same** `gb daemon install` verb from child 1. There is exactly one
place that writes the launchd plist / systemd user unit / SCM service. This
is non-negotiable: two install code paths is two things to keep in sync and
the source of "works on my machine" drift.

```
   brew install …            curl … | sh
         │                        │
         └────────────┬───────────┘
                      ▼
              gb daemon install          ← single entrypoint (child 1)
                      │
      ┌───────────────┼────────────────┐
      ▼               ▼                ▼
 launchd plist   systemd --user   SCM service   (~50 LoC each, child 1)
```

### 3.3 What beads-viewer actually renders

beads-viewer never ships a binary. It renders the **command** for the
detected platform (with a "show all platforms" disclosure), a **Copy** button,
and a **Learn more** link to `gardenio/docs/` install instructions. The
security review child (ka-0000006932) owns the manifest-signing and
`curl|sh` integrity story; this child only asserts that the operator-visible
surface is *copy a command*, not *download an exe*.

---

## 4. First-run / pairing flow

### 4.1 The candidates, and why not QR / device-flow

The bead lists four pairing mechanisms. All establish "this daemon belongs to
this operator." Verdicts, in the context that the operator is **already
AVA-authenticated in the browser** when they trigger pairing:

| Mechanism | Verdict | Why |
| --- | --- | --- |
| **beads-viewer button → pairing code → `gb daemon pair <code>`** | ✓ **adopt** | The operator's browser is already the trust root (AVA-gated). A short one-shot code (TTL ≤5 min) handed to the local CLI is the minimal secure bridge from "authenticated browser" to "local process." Mirrors the working `gb login` cookie-handoff UX (child 2 §"gb login"). |
| **CLI-first `gb daemon pair` (no browser)** | ~ **partial** | Supported as an entry point — `gb daemon pair` with no arg *opens the browser* to the pairing page, then waits. But the trust decision still happens in the AVA-gated browser. Pure-CLI-only pairing (no browser at all) is rejected: we'd have to re-implement AVA auth in the CLI. |
| **QR code** | ✗ **reject** | QR solves *phone-to-desktop* handoff. Here both ends are the same desktop (browser + terminal on one machine). A copy-paste code is strictly simpler and doesn't require a camera. |
| **OIDC device flow** | ✗ **reject at pair time** | The device-code dance ("visit this URL, enter ABCD-1234") is exactly what we'd need *if the operator were not already authenticated*. But they are — they're clicking a button inside the AVA-gated beads-viewer. Re-running a device flow is redundant friction. (OIDC/keycloak is still available as the *token signer* under child 2's Layer-3, just not as the pairing UX.) |

### 4.2 The first-run sequence

The pairing handshake, end to end (this refines child 2 §"Layer 2" with the
first-run operator's-eye view):

```
sequenceDiagram
    actor Op as Operator
    participant BV as beads-viewer (AVA-gated)
    participant Ctl as Controller
    participant D as gb daemon (local)

    Note over Op,D: Precondition — operator has run `gb login` (browser handoff)
    Op->>BV: click "Pair this device" (on setup card or /account/devices)
    BV->>Ctl: POST /api/v1/daemon/pair/new  (operator_sub from AVA)
    Ctl-->>BV: pairing_code=GARDENIO-A4F1-7E2C  (one-shot, TTL 5m)
    BV-->>Op: show code + spinner "waiting for daemon…"
    Op->>D: gb daemon pair GARDENIO-A4F1-7E2C
    D->>D: generate P-256 keypair
    D->>Ctl: POST /api/v1/daemon/pair?code=…  (CSR: CN=daemon_id, O=operator_sub)
    Ctl->>Ctl: verify code; mint client cert (daemon-CA, 90d); \
                record (daemon_id, operator_sub, fingerprint, label, paired_at)
    Ctl-->>D: signed client cert
    D->>D: write ~/.config/gardenio/daemon/{cert.pem,key.pem} (0600)
    D->>Ctl: open long-lived mTLS websocket (Layer 1)  ✅
    Ctl-->>BV: pair-status?code=… → "paired" (daemon_id + first-seen)
    BV-->>Op: ✅ "laptop-pro paired" — Spawn-locally chips light up
```

Operator-visible steps: **two** — click a button, paste a command. Everything
else is machine-to-machine. On success the disabled "Spawn locally" chips
across the queue flip to enabled without a page reload (htmx poll on
`pair-status`).

### 4.3 Naming the device

At step "click Pair this device" the operator is prompted for a **label**
(default suggested from the machine hostname, e.g. `matt-mbp`). This label is
what appears in the device picker (§5) and `/account/devices`. Labeling at
pair time (not later) means the multi-machine picker is never populated with
anonymous `daemon-7f3a…` ids.

### 4.4 First-run failure surfaces (brief; owned by failure-modes child)

| First-run failure | Operator sees |
| --- | --- |
| Not `gb login`-ed yet | `gb daemon pair` prints "run `gb login` first" and offers to open the browser |
| Pairing code expired (>5 min) | CLI: "code expired, click Pair again"; browser card auto-refreshes a new code |
| Code typo | "invalid pairing code" — no partial state created |
| Daemon can't reach controller | "cannot reach controller at <url> — check network/VPN"; no cert written |
| Already paired on this machine | "already paired as `laptop-pro` — use `gb daemon status`" (idempotent, no dupe) |

---

## 5. Multi-machine semantics

### 5.1 The rule: explicit named devices, no implicit primary

An operator with daemons on a laptop **and** a desktop must never get
surprising behavior. We reject **last-write-wins** and **implicit primary**
outright — both create "which machine did it actually run on?" ambiguity that
is unacceptable for a spawn that touches the operator's local filesystem and
creds.

Instead: **each spawn explicitly names its target device.**

- Each paired daemon = a distinct `(operator_sub, daemon_id)` row with an
  operator-chosen label (child 2 §"Multi-operator collision" establishes the
  row model; this child establishes the *picker UX*).
- When the operator clicks "Spawn locally":
  - **0 daemons online** → disabled/offline state (§2).
  - **exactly 1 online** → auto-select it; the chip label shows the target
    (`Spawn on laptop-pro ⤓`) so there's no hidden choice.
  - **≥2 online** → the chip opens a small **device picker** dropdown; the
    operator picks; the action JWT (child 2, Layer 3) is minted with
    `aud=gb-daemon:<chosen_daemon_id>` so it can *only* be redeemed by that
    daemon.

```
≥2 daemons online:
┌───────────────────────────────────────────┐
│ Spawn locally ⤓  ▾                         │
│   ┌─────────────────────────────────────┐ │
│   │ ● laptop-pro    (online, 2s ago)    │ │
│   │ ● desktop-home  (online, 5s ago)    │ │
│   │ ○ old-laptop    (offline 3d) — hide │ │
│   └─────────────────────────────────────┘ │
└───────────────────────────────────────────┘
```

### 5.2 Why not a primary?

A "primary machine" concept invites exactly the bug it's meant to prevent:
the operator sets laptop as primary, walks to their desktop, clicks spawn,
and the agent starts on the *laptop* they left at the office — touching the
wrong working tree. Explicit per-spawn selection makes the target
**visible and deliberate** every time. The only ergonomic cost is one extra
click when >1 machine is online, and we erase even that when only one is
online (the common case).

### 5.3 No conflict is possible

Because the action JWT is bound to one `daemon_id` (`aud`), two daemons can
never both pick up the same spawn — the non-targeted daemon rejects a JWT
whose `aud` isn't its own id. There is no race, no double-spawn, no
last-write-wins reconciliation to design. Multi-machine "conflict" is
**structurally impossible** rather than resolved after the fact.

### 5.4 Device list management

`/account/devices` in beads-viewer is the operator's control panel:

```
┌────────────────────────────────────────────────────────────────────────┐
│ Your paired machines                              [ + Pair a device ]   │
├────────────────────────────────────────────────────────────────────────┤
│ ● laptop-pro     paired 2026-05-01 · cert exp 2026-07-30 · online       │
│                  last spawn: sess-abcd (2h ago)         [ Revoke ]      │
│ ● desktop-home   paired 2026-04-10 · cert exp 2026-07-09 · online       │
│                                                        [ Revoke ]      │
│ ○ old-laptop     paired 2026-02-01 · offline 47d                        │
│                                                        [ Revoke ]      │
└────────────────────────────────────────────────────────────────────────┘
```

"Revoke" adds the daemon's cert fingerprint to the server-side CRL (child 2);
the next websocket reconnect fails closed. This is the operator's remote
kill-switch for a **lost or stolen laptop** — they can revoke `old-laptop`
from any browser without touching the machine.

---

## 6. Uninstall & revocation

Two distinct verbs, because "I don't want this to be able to spawn anymore"
and "I want the software gone" are different operator intents:

| Verb | Removes credentials? | Removes service unit? | Removes binary? | Controller-side effect |
| --- | --- | --- | --- | --- |
| `gb daemon unpair` | ✓ (cert+key) | ✗ | ✗ | CRLs the cert; row marked `unpaired`; can re-pair later |
| `gb daemon uninstall` | ✓ | ✓ | leaves `gb` binary (brew owns it) | unpairs first, then removes launchd/systemd unit + all `~/.config/gardenio/daemon` state + pinned-version cache |
| `brew uninstall gardenio` | — | — | ✓ | should be preceded by `gb daemon uninstall`; a `brew` post-uninstall hook calls it to avoid orphaning the service |

Two hard rules:

> **Rule U-1.** `gb daemon uninstall` **always unpairs first.** A removed
> daemon may never leave a live cert dangling controller-side. If the network
> is down at uninstall time, the local cert is destroyed anyway *and* the
> uninstall queues a best-effort revocation and prints "revoke `laptop-pro`
> from /account/devices to be safe" — the operator's browser-side kill-switch
> (§5.4) is the backstop.

> **Rule U-2.** Revocation is **controller-authoritative.** Even if local
> uninstall fails to reach the controller, the operator can revoke the device
> from `/account/devices`, and the CRL check on websocket reconnect (child 2)
> fails the daemon closed. Local state deletion is a convenience; the CRL is
> the security boundary.

### 6.1 The "clean wipe" definition

`gb daemon uninstall` removes, in order:

1. The launchd plist / systemd user unit / SCM service (stops the process).
2. `~/.config/gardenio/daemon/{cert.pem,key.pem}` (the credential).
3. `~/.gardenio/gb-daemon.sock` and `~/.gardenio/jobs/` (any queued job state —
   child 1).
4. The pinned-version / auto-update cache (child 1 §auto-update).
5. Sends `POST /api/v1/daemon/unpair` (revoke) — best-effort, non-blocking.

The `gb` binary itself is *not* removed (brew/`curl` own its lifecycle; the
operator may still want `gb login` / `gb news` without the daemon).

---

## 7. Operator-experience walkthrough (the happy path, end to end)

Putting §§2–6 together as a single narrative — the experience we are
committing to:

1. **Day 0 (opt-out).** New operator opens `/escalations`. Everything works.
   They see one dismissible card offering local spawn and a greyed
   "Spawn locally" chip. They ignore it; nothing is blocked.
2. **Day 3 (decide to install).** They want to run a repo-modifying agent in
   their own working tree. They click "Copy command" on the card:
   `brew install acme/gardenio/gardenio && gb daemon install`. Runs it.
3. **First run (pair).** `gb daemon install` finishes and prints
   "run `gb daemon pair` to link this machine." They run `gb daemon pair`; it
   opens the browser to the pairing page (already AVA-authenticated), they
   name the device `laptop-pro`, the CLI picks up the code, and in ~2s the
   chip goes green.
4. **Spawn.** They click "Spawn on laptop-pro ⤓" on an escalation; the agent
   session starts locally; an "Open local session" chip appears (spawn-action
   -wire child).
5. **Second machine.** They pair `desktop-home` the same way. Now the chip is
   a picker; they choose per spawn. No primary, no surprise.
6. **Retire a machine.** They sell `old-laptop`; before wiping it they click
   "Revoke" in `/account/devices`. The cert is dead controller-side even
   though they never opened a terminal on the old machine.
7. **Fully back out.** They decide the daemon isn't for them:
   `gb daemon uninstall` wipes local state and revokes; `/escalations` is
   exactly as functional as Day 0.

Every step is reversible, every capability is additive, and the daemon-less
experience is a complete product on its own.

---

## 8. Open questions to surface into phase-2 synthesis

Bounded follow-ups, not blockers — they belong in the phase-2 `gb decision`
that ratifies daemon shape:

1. **Where the "don't show again" preference lives.** This child proposes the
   operator's config/user bead so it follows them across browsers. Confirm
   the operator-preference storage surface with the config-bead child
   (config-bead-surface.md).
2. **`brew` post-uninstall hook.** Does the Homebrew formula reliably fire a
   post-uninstall hook to call `gb daemon uninstall`? If Homebrew's hook
   semantics are unreliable, we accept the orphaned-service risk and rely on
   the operator running `gb daemon uninstall` first (documented). Surface to
   the release/packaging owner.
3. **Windows first-run.** This child specs macOS + Linux first-run in detail;
   Windows (SCM service) pairing is identical in principle but the installer
   UX (SmartScreen, no brew) needs its own pass. Deferred with child 1's
   Windows deferral.
4. **Platform auto-detect fidelity.** `User-Agent` sniffing for the install
   one-liner is best-effort; the "show all platforms" disclosure is the
   safety net. Confirm acceptable.
5. **Re-nag policy after uninstall.** §2.3 last row: if an operator
   uninstalls, do we ever re-show the setup card? Proposal: only if they had
   *not* set "don't show again." Confirm this isn't surprising.

---

## 9. Deliverable notes (dogfood mandate)

Per the deprecation of widgets/reports in favor of bundles (advice
ka-0000007697), the operator-facing summary of this research is published as a
**`type=bundle`** with a `type=doc` narrative child rather than a legacy
rich-report widget instance. This markdown file is the engineering-facing
source of record; the bundle is the human-facing digest. Both link back to
bead ka-0000005690 and the parent epic ka-0000006791.

---

## 10. Linkbacks

- **Bead:** ka-0000005690
- **Parent epic:** ka-0000006791
- **Builds on:**
  - ka-0000006867 — daemon lifecycle & install model (child 1) →
    `daemon-lifecycle.md` (the `gb daemon install` entrypoint, auto-update,
    clean-wipe verbs)
  - ka-0000005882 — authn/authz (child 2) → `authn-authz.md` (Layer-2 pairing,
    per-daemon cert, CRL revocation, multi-operator rows, action JWT `aud`)
  - spawn-action-wire child → `spawn-action-wire.md` (the "Spawn locally"
    chip states; this child owns the never-installed + first-run + multi-
    machine cases it defers)
- **Sister pattern:** ka-0000004527 (escalations top-level surface)
- **Security review child:** ka-0000006932 (manifest signing, `curl|sh`
  integrity — owns the install *integrity* story this child references)
- **Config-bead child:** config-bead-surface.md (operator-preference storage)
- **Failure-modes child:** failure-modes.md (offline/expired/recheck recovery)
- **Dogfood mandate:** ka-0000006879 (bundle deliverable)
- **Code anchors:**
  - `gardenio/controller/cmd/gb/login.go` — cookie-handoff UX shape the pairing
    flow mirrors
  - `gardenio/controller/internal/credstore/` — where the daemon cert/key
    context fields live
  - `gardenio/controller/internal/oidcauth/`, `.../avareauth/` — AVA-gated
    browser trust root for pairing
  - `gardenio/controller/cmd/beads-viewer/` — host for `/escalations`,
    `/decisions`, and the new `/account/devices` surface
- **Precedent:** Tailscale (`tailscale up` coordinator pairing, per-device
  revocation, named devices), 1Password CLI (`op` per-user UDS + pairing),
  `gh` CLI (brew-first install), VSCode `code tunnel service install`
  (single-verb service install + device registration).
