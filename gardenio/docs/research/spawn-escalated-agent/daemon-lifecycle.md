# Daemon lifecycle & install model — `gb-daemon`

> Research child 1 of 9 for **ka-0000006791** (epic: client-side `gb` daemon for
> "spawn escalated agent").
>
> **Bead:** ka-0000006867
> **Parent epic:** ka-0000006791
> **Sister pattern:** ka-0000004527 (escalations top-level surface)
> **Dogfood mandate:** ka-0000006879 (free-htmx rich-report widget instances)
>
> **Status:** research / recommendation only. No code lands until a
> phase-2 `gb decision` ratifies the daemon shape.

---

## TL;DR

| Question | Recommendation |
| --- | --- |
| **Process model** | A new mode on the existing `gb` binary — `gb daemon` — running as a per-user platform service (launchd `LaunchAgent` on macOS, `systemd --user` unit on Linux, Service Control Manager unit on Windows). Not a tray app. Not in-process inside `coop`. |
| **Open Q from epic** | The daemon is a **flag on `gb`** (`gb daemon`), not a new `gbd` binary and not an in-process mode of `coop`. One binary, one auth surface, one calver release line. |
| **Install path** | macOS: Homebrew tap `acme/gardenio` shipping `gb`, with `gb daemon install` writing the LaunchAgent plist and starting the service. Fallback: `curl ... | sh` redirecting through the same installer entrypoint. Linux soon (systemd user unit via the same `gb daemon install`). Windows later (deferred until the macOS + Linux pattern proves out). |
| **Auto-update** | Daemon polls a signed manifest at `https://beads.gardenio.e2e.example.com/daemon/manifest.json` once per hour, mirrors the gardenio calver release line, atomic-swap with `SIGUSR2` self-restart. Default channel `stable` (= prod gardenio tag); operators opt into `latest-rc`. Daemon refuses to talk to a controller more than one calver behind/ahead. |
| **Crash recovery** | Platform supervisor (launchd `KeepAlive`, systemd `Restart=always`) is the primary line of defence. In-process: per-action job lease + heartbeat back to controller; if daemon dies mid-spawn the controller flips the action to `failed` after a 30 s heartbeat miss and the operator gets a retry chip in `/escalations`. Health endpoint `GET /healthz` on the UDS at `~/.gardenio/gb-daemon.sock`. |

The rest of this document walks the precedent survey, the trade-offs, and the
rationale behind each recommendation. The companion **rich-report widget
instance** on the bead is the operator-facing summary.

---

## 1. Why this question is non-trivial

The naive answer to "we need a daemon on operator laptops" is "ship a tray
app." We deliberately reject that — see §3.1 — because the daemon's
**only consumer** is the controller, not the human at the keyboard. The
human's surface is **beads-viewer** (and Slack); the daemon is a quiet
mechanism that wakes up when a decision option says "spawn locally," does
its job, and goes back to sleep. Tray-app investment buys nothing here
and costs us a per-platform UI line that we'd need to maintain.

The other false-default is "make it part of `coop`." Also rejected — see
§3.2. `coop` is a **per-session** wrapper around a single agent process.
Its lifetime is tied to one PTY and one Claude/Gemini invocation. The
daemon must outlive any single session: it has to be reachable when no
session is running so that "spawn escalated agent" can start the
session in the first place.

So the answer must be: a **persistent, per-user, background process**
that owns no UI surface, talks to the controller over an authenticated
channel, and is cheap enough to install/uninstall that operators don't
push back.

## 2. Precedent survey

We surveyed six laptop-daemon precedents named in the bead, plus a few
more for comparative coverage. The table summarizes; the prose
sub-sections below pull out the specific design lessons.

| Precedent | Process model | Install path | Auto-update | Crash recovery | Notes for gb-daemon |
| --- | --- | --- | --- | --- | --- |
| **1Password CLI agent** (`op`) | Per-user background helper spawned by the desktop app; UDS for IPC; Touch ID gate per access | Bundled with the 1Password desktop app (DMG / MSIX / .deb); also standalone via Homebrew | Bundled with desktop app updates (Sparkle on macOS) | Desktop app respawns helper; helper itself is short-lived per request | Biometric-gated per-access UX is overkill for us, but the **UDS + per-user model** is exactly right |
| **Docker Desktop helper** | Heavyweight VM + tray Electron + privileged helper (`vmnetd`, `com.docker.vmnetd`, `socket_vmnet`) | DMG / MSI / .deb installer; root prompt at first launch for helper privileges | Sparkle on macOS, MSIX on Windows, package on Linux; staged channels | LaunchDaemon (root) for `vmnetd`; tray app crashes are tolerable | **Anti-pattern** for us: too heavy, requires root, multi-process. The cautionary tale. |
| **gh CLI auth helper** (`gh auth`) | **Not a daemon.** Short-lived CLI invocation; token stored in OS keyring | `brew install gh`, `apt install gh`, scoop, MSI | OS package manager updates | n/a (no long-running process) | Useful baseline: when you can avoid a daemon, do — but we can't (controller needs to push, not pull on demand). |
| **Tailscale** (`tailscaled`) | Persistent privileged daemon + per-user CLI (`tailscale`); LaunchDaemon (root) + LaunchAgent (user) on macOS; systemd unit on Linux; SCM service on Windows | Native installer per platform (pkg / msi / deb / rpm / brew tap) drops daemon plist/unit; `tailscale up` does coordinator pairing | Opt-in via `tailscale set --auto-update=true`; pinned channel; signed manifest | launchd `KeepAlive` / systemd `Restart=always`; coordinator pings on heartbeat | **Closest existing model.** Single Go binary, embedded service install, cross-platform, signed update manifest, opt-in auto-update. The recommendation cribs from this directly. |
| **Claude Code** (`claude` CLI) | **Not a daemon.** Foreground CLI; auth via OAuth device flow; token in OS keyring | `npm i -g @anthropic-ai/claude-code` (and similar) | npm-distributed; users pull on demand | n/a | Confirms: the user-facing CLI is fine staying short-lived; the daemon is a separate concern. |
| **coop** (`coop/crates/cli/`) | Per-session wrapper around an agent CLI; PTY + driver tiers + event log; lifetime = one session | Currently bundled into the gardenio controller image; not installed on operator laptops | Released with the gardenio calver line (image tag) | Per-session; if coop dies the session dies. coopmux is the long-lived sidecar inside K8s, not on the laptop | **Wrong lifetime** for gb-daemon. Coop is intentionally short-lived per session; the laptop daemon must outlive sessions. |
| 🅕 *Comparison: VSCode `code` tunnel* | Persistent per-user agent (`code tunnel`) that registers with a control plane; launchd plist + systemd unit | `code tunnel service install` (drops platform service) | Auto-updates with VSCode insiders/stable channel | launchd / systemd respawn | Very similar shape; reinforces the `gb daemon install` verb. |
| 🅕 *Comparison: GitHub Actions runner* | Persistent self-hosted runner; per-user systemd unit / launchd plist | `./svc.sh install` drops platform service unit | Runner self-updates from manifest, atomic swap, self-restart | platform supervisor + heartbeat back to GitHub | Reinforces signed manifest + atomic-swap-and-restart as the proven pattern. |
| 🅕 *Comparison: dropbox / syncthing* | Tray-anchored persistent daemon (drops to a tray menu, but the daemon is the load-bearing piece) | DMG / installer; .deb / .rpm | App-managed updates | platform supervisor | Tray icon is informational only; the daemon does the work. **We do not need the tray.** |

### Specific design lessons pulled out of the survey

**From Tailscale.**

- One Go binary, three install verbs, three platform service backends, one
  manifest format, one signing key. This is *exactly* the bar we want to
  hit. The implementation cost amortizes across platforms because the
  service-install boilerplate is the only platform-specific piece and is
  ~50 lines per platform (we have proof-of-existence in the
  `github.com/kardianos/service` ecosystem).
- Auto-update is **opt-in** by default in Tailscale, but the `pkg` install
  flow flips it on. Our calver release cadence is fast enough (multiple
  cuts/week) that we'd prefer opt-in `stable` channel **defaulted on** with
  `--no-auto-update` for skittish operators.
- Skew tolerance: Tailscale's CLI refuses to talk to a `tailscaled` more
  than ~6 versions behind. We'll do calver: refuse if `|controller_calver
  - daemon_calver| > 1`.

**From Docker Desktop (anti-pattern).**

- A tray app + privileged helper + VM stack is the upper bound of what
  laptop-daemon UX can be. It's miserable to maintain (Docker Inc. has
  literal teams just for the desktop helper). The cost grows superlinearly
  with the number of OS-version-specific code paths.
- Lesson: **do not** ship an Electron tray. Do not require root. Do not
  spawn helper processes beyond the single daemon.

**From `op` CLI agent.**

- Per-user UDS at a well-known path (`~/Library/Group Containers/...` on
  macOS; `$XDG_RUNTIME_DIR/op-daemon.sock` on Linux) — solves both the
  authentication problem (`stat` the socket peer credentials) and the
  discovery problem (no port-allocation churn). We'll do the same:
  `~/.gardenio/gb-daemon.sock` (with parent dir `0700`).
- 1Password's biometric-on-each-access prompt is excellent UX for
  credentials but **not** what we want for spawn-action. The daemon
  receives an already-signed token from the controller; the operator's
  presence/consent is established in the originating beads-viewer / Slack
  click. We do **not** want a Touch ID prompt every time the daemon
  spawns.

**From gh CLI auth + Claude CLI (no-daemon precedents).**

- These reinforce that we should not turn the daemon into a generic
  "everything `gb`" replacement. The CLI keeps doing what it does. The
  daemon is the **single new** persistent process and only because the
  controller needs a push channel.

**From coop's lifecycle.**

- The session lifecycle in `coop/crates/cli/src/run.rs` (~1.3k lines)
  already proves the team can build a robust local supervisor. Reusing
  that *code* inside `gb daemon` is fine; reusing the *process* is not
  (lifetime mismatch).
- The hook-FIFO and event-log primitives from coop translate cleanly to
  per-spawn job tracking inside the daemon. The daemon owns a job queue;
  each "spawn escalated agent" action becomes a job; per-job state lives
  in `~/.gardenio/jobs/<job-id>/` (mirror of
  `$XDG_STATE_HOME/coop/sessions/<session-id>/`).

## 3. Recommendation — and what we considered

### 3.1 Process model: `gb daemon` on the existing `gb` binary

**Reject: tray app (Electron / SwiftUI).**

- Forces a per-platform UI line (Electron for cross-platform, SwiftUI for
  macOS-native polish, .NET XAML for Windows). Each is a new build/test
  target and a new attack surface for code-signing rot.
- We have no operator-facing surface to put in the tray. beads-viewer +
  Slack are the operator surfaces. A tray icon would be informational at
  best.
- Docker Desktop is the cautionary tale; we don't want that maintenance
  bill.

**Reject: in-process inside `coop`.**

- `coop`'s lifetime is one PTY / one agent session. The daemon's job
  starts *before* a session exists and must persist after a session ends.
- `coop` is a Rust crate; embedding a long-lived controller-facing daemon
  in it would force us to choose between (a) running coop forever in the
  background (it's not designed for that) or (b) splitting coop into
  daemon + session layers (which is exactly what we'd be doing if we
  wrote `gb daemon` separately — so we should just write `gb daemon`).
- The credential-broker / coopmux pattern (gardenio controller ↔ coopmux
  via HTTP) is the right inspiration for **how** the daemon talks; it's
  not the right *home* for the daemon process.

**Reject: new `gbd` binary.**

- One more binary to build, sign, package, release, document. Two binaries
  means two install paths and two opportunities for skew (`gb` v15,
  `gbd` v12, controller wondering which is canonical).
- The daemon needs the same gardenio config-bead resolver, the same OIDC
  device flow, the same kbeads client. All of that is already in `gb`.
  Splitting it out means re-importing or re-implementing.
- Tailscale ships one binary (`tailscale` + `tailscaled` are conceptually
  the same module behind a build-tag flag and a service-install verb).
  Same playbook.

**Accept: `gb daemon` (a subcommand-mode of `gb`).**

- Reuses everything already in `gb`: config, auth, RPC clients, kbeads
  HTTP client, logging, signing.
- `gb daemon install` / `start` / `stop` / `status` / `logs` is a natural
  verb cluster operators already expect from CLI tools.
- One calver tag per release; no skew between gb-the-cli and gb-the-daemon.
- Implementation cost: a job queue, a UDS HTTP server, a websocket-back-to-
  controller client, a platform service installer. All small, all
  isolated under `gardenio/controller/cmd/gb/cmd/daemon/` (proposed
  layout) so the rest of `gb` is untouched.

**Process layout:**

```
gb daemon                           # foreground (used in dev + by launchd/systemd)
gb daemon install                   # write LaunchAgent plist / systemd --user unit
gb daemon uninstall                 # remove unit + stop
gb daemon start | stop | restart    # delegate to launchctl / systemctl --user
gb daemon status                    # health endpoint readout + recent jobs
gb daemon logs [--follow]           # tail ~/.gardenio/logs/gb-daemon.log
```

`gb daemon` runs an HTTP server bound to a UDS at
`~/.gardenio/gb-daemon.sock` (mode `0600`, parent dir `0700`), and a
long-lived outbound websocket to the controller (`wss://…/daemon/connect`)
over which the controller pushes spawn-action payloads.

### 3.2 Install path: `brew tap` first, `curl|sh` as a fallback, in-app prompt from beads-viewer

**macOS — primary surface.**

- Homebrew tap: `acme/gardenio` (creates `brew install gardenio/gb` →
  drops `gb` into `/opt/homebrew/bin`). Already the default install path
  most operators on the team use.
- `gb daemon install` writes `~/Library/LaunchAgents/com.gardenio.gb-daemon.plist`
  with `RunAtLoad=true`, `KeepAlive=true`, the binary path resolved at
  install time, and starts the service via `launchctl bootstrap
  gui/$UID …`.
- `gb daemon install` is **opt-in** — `brew install gardenio/gb` does
  NOT auto-start a daemon. Operators run `gb daemon install` when they
  decide they want the spawn-action affordance.
- beads-viewer drops an **"Install daemon"** info card on `/escalations`
  for operators whose presence isn't detected (no recent daemon
  heartbeat for this OIDC subject). The card links to a 1-page install
  guide and an audit-log of who in the org has it installed.

**Linux — soon.**

- systemd `--user` unit installed by `gb daemon install` (drops
  `~/.config/systemd/user/gb-daemon.service`, enables linger if not
  already enabled so the unit survives logout).
- Distribution-of-distributions concerns mostly defer: we install via the
  user binary's own installer verb, not via `apt`/`rpm`/`dnf`. (We can
  add a `.deb` later for fleet-managed laptops.)
- Same brew tap formula works on linuxbrew for the small number of users
  who use it.

**Windows — deferred.**

- Service Control Manager is well-understood (`sc create gb-daemon …`),
  the `github.com/kardianos/service` library already abstracts this, and
  WinGet is the obvious distribution channel.
- Defer until the macOS + Linux experience is steady-state — Windows is
  not on the operator critical path today.

**Curl-pipe-sh fallback.**

- For operators outside Homebrew, a `curl -fsSL
  https://beads.gardenio.e2e.example.com/install.sh | sh` that
  resolves the platform, downloads the latest signed binary, verifies the
  signature against an embedded public key, and runs `gb daemon install`
  on the operator's confirmation.
- Signed: GPG signature + SHA256 manifest committed to the release tag,
  served from the controller alongside the manifest used for
  auto-update.
- This is the same install entrypoint beads-viewer's "Install daemon"
  card links to — we do not maintain two installer flows.

### 3.3 Auto-update: manifest + atomic swap + self-restart, pinned to calver

The daemon's update flow mirrors the **gardenio calver release line** and is
deliberately boring:

1. Daemon polls `https://beads.gardenio.e2e.example.com/daemon/manifest.json`
   once per hour (jittered ±10 minutes to avoid thundering-herd at the
   top of the hour). Manifest body:

   ```json
   {
     "channel": "stable",
     "latest_version": "2026.135.9",
     "min_supported_version": "2026.130.0",
     "platforms": {
       "darwin/arm64": { "url": "...", "sha256": "...", "size": 12345678 },
       "darwin/amd64": { "url": "...", "sha256": "..." },
       "linux/amd64":  { "url": "...", "sha256": "..." },
       "linux/arm64":  { "url": "...", "sha256": "..." }
     },
     "signed_at": "2026-05-15T17:00:00Z",
     "signature": "<minisign / cosign signature>"
   }
   ```

2. Manifest verified against an embedded public key (rotated yearly, with
   a documented rotation playbook). Signature failure = no update + log
   + audit-bead.
3. New binary downloaded to `~/.gardenio/cache/gb-<version>`, sha256
   verified.
4. Atomic swap: rename `~/.gardenio/cache/gb-<version>` over the active
   `~/.gardenio/bin/gb-daemon` (the launchd plist `Program` path points
   here). On macOS this is atomic via `renameat2` semantics; on Linux
   `rename(2)` is atomic on the same filesystem.
5. Daemon sends `SIGUSR2` to itself, which triggers a graceful restart:
   drain in-flight jobs (default cap 30 s), close UDS, close websocket,
   `exec` the new binary in-place.
6. New binary handshakes with the controller and reports version + start
   reason (`"upgrade":"2026.130.4→2026.135.9"`) — controller logs this
   in an audit bead for the operator.

**Channels.**

- `stable`: the current prod gardenio tag (=`2026.NNN.X`, no `-rc` suffix).
  Default for operators.
- `latest-rc`: the latest `-rc` cut. For operators piloting features.
- Channel switch: `gb daemon set-channel latest-rc` (or via config bead).

**Skew tolerance.**

- Controller advertises `min_supported_daemon_version` in its bootstrap
  response. Daemon refuses (and surfaces a "please upgrade" log line +
  audit bead) if it sees a controller asking for a version more than one
  calver behind its own. Bidirectional: controller also refuses to push
  to daemons older than `min_supported`.

**Release-pipeline integration.**

- The gardenio release pipeline (root `CLAUDE.md` §Release) already builds
  per-platform binaries during the `docker.yml` stage. We add a step that
  uploads the daemon-mode binary as a signed artifact and writes the
  manifest on `promote-images` (same gate that flips `-rc` → `<calver>` +
  `latest`).
- No new pipeline; daemon shipping piggybacks on the existing calver line.

**Operator override.**

- `gb daemon set-channel manual` disables auto-update; operator must
  `gb daemon upgrade` to pull. For paranoid environments.
- `gb daemon pin-version 2026.130.0` for explicit pinning across restarts
  (no auto-update past the pin).

### 3.4 Crash recovery: platform supervisor + heartbeat + per-job lease

**Layer 1 — platform supervisor (primary).**

- macOS launchd: `KeepAlive=true`, `ThrottleInterval=10`,
  `ExitTimeOut=30`. Daemon restarts within seconds of any crash. launchd
  log captured to `~/Library/Logs/gb-daemon.log`.
- Linux systemd `--user`: `Restart=always`, `RestartSec=5s`,
  `StartLimitBurst=10`, `StartLimitIntervalSec=60s`. Mirrors launchd
  semantics. Journal captured via `journalctl --user -u gb-daemon`.
- Windows SCM (later): `Recovery actions = Restart` at 1st/2nd failure,
  `Reset after 60s`.

**Layer 2 — heartbeat to controller.**

- Daemon emits a small heartbeat on its outbound websocket every 30 s
  carrying `{daemon_id, operator_subject, version, uptime,
  active_jobs[], last_action}`.
- Controller writes the latest heartbeat into a per-operator daemon-state
  bead (kind=daemon, type=config, one per operator per laptop). When the
  bead's `last_heartbeat_at` is older than 90 s, controller marks the
  daemon `offline` and `/escalations` greys the spawn-action for that
  operator.
- Heartbeat-missing is **the** signal the controller uses to decide
  whether spawn-action is offered at all. Crash + restart cycles
  obviously cause heartbeat resumption, so for the operator the affordance
  just flickers briefly.

**Layer 3 — per-job lease + idempotency.**

- Each spawn-action carries an `action_id` (UUIDv7) issued by the
  controller when the operator clicks. Daemon acks on receipt and writes
  `~/.gardenio/jobs/<action_id>/state.json` (`pending|running|done|failed`).
- On startup, daemon recovers in-flight jobs from
  `~/.gardenio/jobs/*/state.json`. Jobs in `running` at startup are
  treated as **failed** (the daemon can't safely resume a Claude Code
  session it didn't itself spawn) and the controller is notified;
  operator sees a "spawn recovery failed — retry?" chip in
  `/escalations`.
- Idempotency: controller will re-deliver the same `action_id` until it
  sees a terminal ack. Daemon dedup is keyed on `action_id`. Re-deliveries
  are common during heartbeat-loss windows and must not cause duplicate
  Claude sessions.

**Health endpoint.**

```text
GET /healthz                 # via UDS
HTTP/1.1 200 OK
Content-Type: application/json
{
  "version":          "2026.135.9",
  "uptime_seconds":   45123,
  "platform":         "darwin/arm64",
  "operator_subject": "oidc:user-1234",
  "channel":          "stable",
  "controller_url":   "wss://beads.gardenio.e2e.example.com/daemon/connect",
  "controller_state": "connected",
  "last_action_at":   "2026-05-15T14:31:02Z",
  "active_jobs":      [{"action_id":"...","kind":"spawn-escalated-agent","started_at":"..."}],
  "pending_updates":  null
}
```

- `/healthz` is read by `gb daemon status` and by beads-viewer's "is the
  operator's daemon live?" check (via a short-lived signed UDS-relay over
  the websocket — see ka-0000000940 for the AuthN/AuthZ research).
- The same endpoint serves `/metrics` (basic Prometheus textfile) on the
  same UDS — operators who run laptop monitoring can scrape it; we don't
  ship it elsewhere.

## 4. Direct answer to the epic's open Q

> *"Is the daemon a new binary, an in-process mode of `coop`, or a flag
> on `gb`?"*

**A flag on `gb`** — specifically a subcommand mode `gb daemon`. The
trade-offs:

- vs. **new `gbd` binary**: rejected because the daemon needs all of
  `gb`'s existing config / auth / kbeads-client / signing surface area.
  Splitting the binary doubles the install/sign/release matrix without
  buying isolation we need.
- vs. **in-process inside `coop`**: rejected because `coop`'s lifetime is
  one PTY, the daemon's lifetime is "user logged in". Wrong scope.
  Reusing *code* from coop (event log, hook FIFOs, supervised
  subprocesses) inside `gb daemon` is fine and encouraged.

## 5. Open questions to surface into phase-2 synthesis

These are bounded follow-ups, not blockers. They belong in the
phase-2 `gb decision` that ratifies daemon shape, not in this child.

1. **Where does the daemon's outbound socket terminate?** This child
   assumes `wss://beads.gardenio.e2e.example.com/daemon/connect` (same
   ingress as beads-viewer). ka-0000000940 (AuthN/AuthZ child) owns the
   exact answer.
2. **Manifest signing key custody.** Where do we store the private key
   that signs the daemon manifest? Same place as the calver release
   signing key? AWS KMS? This is a security concern; surface to
   ka-0000004588 (security review child).
3. **First-time pairing UX.** `gb daemon install` works once the operator
   is `gb login`-ed. What if they aren't? Pop a `gb login` prompt on
   first daemon start. ka-0000000940 should specify the pairing exchange.
4. **Telemetry redaction.** The heartbeat carries `last_action`. We must
   ensure the daemon does not exfiltrate filesystem paths or job
   payloads in heartbeats — only metadata (action_id, kind, status).
   Surface to security review.
5. **Uninstall completeness.** `gb daemon uninstall` must remove the
   plist/unit AND any pinned-version cache AND any persistent job
   state. Define the "clean wipe" verb.

## 6. Linkbacks

- **Bead:** ka-0000006867
- **Parent epic:** ka-0000006791
- **Sister pattern:** ka-0000004527 (escalation surface)
- **Dogfood mandate:** ka-0000006879 (free-htmx widget research)
- **Originating operator bead:** ka-0000001035
- **Code anchors:**
  - `coop/crates/cli/src/run.rs` — local session supervisor we crib code from
  - `coop/crates/mux/` — controller-managed credential / state push pattern
  - `gardenio/controller/cmd/gb/` — host for `gb daemon` subcommand mode
  - `gardenio/controller/internal/podmanager/` — env injection / config-bead resolver
  - `kbeads/cmd/kd/config_bead.go` — config bead resolver
- **Tailscale source (precedent):** https://github.com/tailscale/tailscale — single-binary, embedded service install, signed manifest auto-update.
- **`github.com/kardianos/service`** — cross-platform service install library that abstracts launchd / systemd / SCM.

## 7. Recommendation — concrete next steps (post-phase-2 ratification)

These are *not* implementation steps for this child — they're the slices a
phase-3 gardener would dispatch once a `gb decision` ratifies the shape:

1. `gb daemon` subcommand skeleton (foreground only, no service install).
2. UDS HTTP server on `~/.gardenio/gb-daemon.sock` with `/healthz`.
3. Outbound websocket client to controller; heartbeat loop.
4. Job queue + `~/.gardenio/jobs/` persistence.
5. `gb daemon install` for macOS (launchd plist).
6. Manifest poll + atomic-swap + SIGUSR2 self-restart.
7. `gb daemon install` for Linux (systemd `--user`).
8. Release-pipeline manifest publication.
9. beads-viewer "Install daemon" info card on `/escalations`.

Each step is small and independently mergeable. Total surface is ~2k lines
of Go in `gb`, ~50 lines per platform of service-install boilerplate, and
one new pipeline step for manifest publication.
