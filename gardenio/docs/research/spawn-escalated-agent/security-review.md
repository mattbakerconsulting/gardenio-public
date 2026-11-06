# Security review — compromised `gb-daemon` (spawn-escalated-agent)

**Bead:** ka-0000006932 (research child 8 of 9)
**Epic:** ka-0000006791 — client-side `gb` daemon for "spawn escalated agent"
**Siblings this leans on:** ka-0000005882 (authn/authz — the channel threat model),
ka-0000006867 (daemon lifecycle — install/update/crash), ka-0000004798
(spawn-action wire — envelope + audit seam), ka-0000004678 (permissions model — open).
**Status:** research / recommendation only. No code lands until the epic's Phase-2
`gb decision` ratifies the daemon shape. This doc specifies the security bar that
decision must clear.

---

## TL;DR

The single most important fact about this design, and the one that reframes every
threat below:

> **A daemon "spawn escalated agent" action is, by construction, arbitrary code
> execution on the operator's laptop — not a narrow, well-typed verb.**

The wire is well-typed (`spawn-escalated-agent`, `heartbeat`, `status`). But the
*payload* of that one verb tells the daemon to launch **Claude Code in the
operator's real environment**, and today gardenio's default agent launch command
*is* `claude --dangerously-skip-permissions`
(`gardenio/controller/cmd/gb/agent_start.go:158`,
`gardenio/controller/cmd/gb/init_agent_loop.go:825`,
`gardenio/controller/cmd/gb/agent_start_k8s.go:67`),
resolving a `claude_settings_bead` that can carry `hooks` (shell `command`
entries), `mcpServers`, and `permissions.allow: ["Bash(*)"]`
(`gardenio/controller/cmd/gb/config_resolve_test.go:88,219,423`). So the daemon does
not need a CMD-injection bug to run attacker code — **running third-party-authored
code in the operator's shell is the product.** The security model must therefore be
about *bounding and auditing that power*, not pretending the daemon only moves
well-typed data.

Two consequences fall straight out:

1. **The blast radius of a spawn is "everything the operator's laptop can reach"** —
   their SSH agent, `~/.aws/credentials`, `~/.config/gardenio/credentials.json`
   (the AVA/OIDC cookie + refresh token, 0600 but readable by the operator's own
   uid — `gardenio/clicreds/credstore/credstore.go:98`), kube contexts, prod
   access. Bounding this is the whole job.
2. **The trust anchor is the controller, not the daemon and not the browser.**
   The envelope is controller-signed (spawn-action-wire §Security shape). A
   compromised daemon cannot *mint* spawns; it can only *misuse* spawns it is
   handed, lie about outcomes, and exploit its foothold. A compromised controller
   is game-over and is explicitly out of scope for hardening here (it is the
   platform's root of trust everywhere else too).

**Mandatory mitigations** (M1–M12, §5) gate any Phase-3 implementation. The four
that matter most:

- **M1 — Default-deny permission profile enforced daemon-side**, never
  browser-supplied. No spawn without an operator-provisioned allowlist of
  (project, repo, working-dir-root, ref-kind).
- **M2 — Pin the `claude_settings_bead` origin.** The daemon must resolve Claude
  settings from a controller-signed snapshot whose provenance it can verify — not
  chase an arbitrary bead id handed to it in the envelope. This closes the "point
  the spawn at an attacker-authored hooks bead" hole.
- **M6 — Tamper-evident, append-only, hash-chained local audit log**, mirrored to
  a controller-side `spawn-intent` bead created *before* the forward.
- **M9 — Signed release + signed auto-update manifest with a documented key
  custody + rotation story.** This is the supply-chain gate for the daemon binary
  itself.

---

## 1. Scope

**In scope (this child owns):**

- Threat-model a **compromised daemon** as the central asset: what an attacker who
  owns the daemon process (or its binary, or its host) can do, and how to bound it.
- The **blast radius** of a spawn and how to shrink it (process isolation,
  filesystem scope, network egress, credential exposure).
- **Audit-log spec** — what every spawn writes locally and emits to the controller,
  and the tamper-evidence requirements.
- **Secret/key handling on disk** — the daemon cert, the manifest-verify key, and
  the operator credentials the daemon sits next to.
- Consolidated **mandatory vs nice-to-have** mitigation ladder.
- Security **open questions** for the Phase-2 synthesis decision.

**Out of scope (owned elsewhere / deferred):**

- The wire auth protocol (mTLS + per-action JWT + websocket) — ka-0000005882 owns
  it; this doc consumes its guarantees and stress-tests the residual "daemon is
  owned" case its T6 flagged and deferred here.
- The permissions *schema* (exact profile bead shape) — ka-0000004678 owns it; this
  doc specifies the *security requirements* that schema must satisfy (default-deny,
  daemon-side enforcement, non-forgeable).
- Install/update *mechanics* — ka-0000006867 owns them; this doc specifies the
  *supply-chain* requirements (signing, key custody, skew refusal).
- Implementation. A compromised **controller** (rogue platform admin with the
  signing key) — that is the system-wide root of trust; hardening it is a
  platform-wide concern, not daemon-specific.
- Mobile.

---

## 2. Assets — what an attacker wants

Ranked by what a spawn actually touches. This is the "what are we protecting"
column of the threat table.

| Asset | Where it lives | Why it matters |
|---|---|---|
| **A1. Operator's shell / laptop** | the whole machine | A spawn runs Claude Code as the operator. Everything the operator can do, the spawn can do. This is the crown jewel. |
| **A2. Operator cloud/prod credentials** | `~/.aws/`, kube contexts, SSH agent, `~/.config/gardenio/credentials.json` (AVA cookie + OIDC refresh token, 0600) | A laptop with prod access is a pivot into `gardenio-prod` / `acme-prod-v2`. The daemon sits *next to* these; a spawn can read them. |
| **A3. Daemon client cert + key** | `~/.config/gardenio/daemon/{cert,key}.pem` (0600, per ka-0000005882) | Impersonates *this daemon* to the controller. Bounded (cannot impersonate operator on OIDC), 90-day lifetime, operator-revocable. |
| **A4. Manifest-verify public key** | embedded in the `gb` binary (ka-0000006867 §3.3) | If an attacker can swap it, they own the auto-update channel → persistent RCE on every laptop. Supply-chain crown jewel. |
| **A5. Spawn envelope in flight** | controller → daemon over WS | Carries `prompt`, `working_dir`, `ref`, `claude_settings_bead`. Signed + 5-min TTL. Replay/forge target. |
| **A6. Local audit log + job state** | `~/.gardenio/jobs/`, `~/.gardenio/logs/` | The forensic record. An attacker who owns the daemon wants to erase or forge it. |
| **A7. The controller's trust in daemon-reported state** | controller-side beads | A compromised daemon can lie: claim a spawn succeeded that it subverted, or hide one it ran. |

---

## 3. Threat actors & the compromised-daemon threat table

The brief named five actor/scenario classes. Below, each is mapped to the assets it
threatens and the mitigations that bound it. Mitigation IDs (M#) are defined in §5.

### Threat table (actor × asset × mitigation)

| # | Actor / scenario | Primary assets | What they can do | Bounding mitigations |
|---|---|---|---|---|
| **TA1** | **Supply-chain compromise of the daemon binary** (poisoned install script, malicious dependency, compromised release artifact) | A4, A1, A2 | Ship code that runs on every laptop with the operator's full privilege the moment the daemon starts. Worst case in the whole model. | **M9** signed release + signed manifest; **M10** pinned manifest-verify key + rotation playbook; **M11** reproducible builds + SBOM; skew-refusal (ka-0000006867); **M12** minimal dependency surface |
| **TA2** | **Malicious / rogue controller** pushing a forged spawn (rogue platform admin, or controller RCE) | A5, A1 | Push a `spawn` envelope with an attacker prompt / attacker `claude_settings_bead`. If they hold the controller *signing key*, envelopes verify. | Largely **out of scope** (controller is the root of trust). But **M2** (pinned settings origin), **M3** (operator consent binding), **M4** (default-deny profile) and **M6** (audit) mean even a rogue controller cannot spawn *silently* or *outside the operator's provisioned scope* without leaving an audit trail and tripping the profile. Defence-in-depth, not a full stop. |
| **TA3** | **Local-machine attacker with shell as the operator's uid** | A2, A3, A6, A1 | Read the daemon cert, read operator creds, tamper with the audit log, use the *live* daemon socket to observe/queue. Already owns the laptop. | This is a *laptop-compromise* class; the daemon does not meaningfully widen it (the attacker already has the operator's shell). **M5** (UDS `0600`, dir `0700`, peer-cred check) stops a *different-uid* local attacker; **M6** hash-chaining makes tampering *evident*; **M7** no-new-secrets (daemon holds nothing the operator's shell didn't already expose). |
| **TA4** | **Stolen operator identity / stolen pairing key** (exfiltrated cert+key, or a stolen pairing code) | A3, A5 | Impersonate the daemon to the controller; receive spawns intended for the operator. Cannot mint spawns (needs operator click → controller JWT). | ka-0000005882 Layer-3 JWT requires operator interaction to mint; **M8** operator-visible device revocation (`/account/devices`) + 90-day cert TTL; **M3** consent binding; short pairing-code TTL (≤5 min, one-shot). Blast radius bounded to "receive spawns," and a received spawn still needs a valid profile (**M4**). |
| **TA5** | **Compromised daemon *process* used as a persistent foothold** (RCE in the daemon, or TA1 realized) | A1, A2, A6, A7 | Hold a live authenticated channel into the controller; run arbitrary local code under the operator; lie about outcomes; exfiltrate the operator's environment. | **M4** capability restriction (daemon exposes only spawn/heartbeat/status, cannot issue arbitrary controller RPCs); **M13** controller treats *all* daemon-reported input as untrusted (scrub before render/write); **M6** audit is append-only + mirrored server-side so lies are detectable by divergence; **M8** revocation; **M14** process sandbox limits what a subverted daemon reaches. |
| **TA6** | **CMD-injection vs well-typed actions** (the brief's explicit question) | A1, A5 | Try to smuggle shell metacharacters through `working_dir` / `ref` / `prompt` into a shell the daemon runs. | The wire is well-typed, **but see §4** — the *spawn verb itself is code execution by design*. **M15** strict envelope schema validation + no shell interpolation of envelope fields (exec-argv, never `sh -c`); **M2** pinned settings; **M4** profile gate. The honest answer: injection into the *daemon's own* shell is preventable; the *spawned agent* running arbitrary code is the intended behaviour, gated by M2/M4. |

### The boundary question, answered directly

> *Does the daemon ever run arbitrary commands from the controller, or only
> well-typed action verbs?*

**Both, and the distinction is the crux of this review.**

- **At the wire layer:** only well-typed verbs. The controller→daemon protocol is a
  fixed enum (`action.spawn-escalated-agent.request`, `daemon.heartbeat`,
  `daemon.status`, `daemon.shutdown` — ka-0000005882 §Layer-1). The daemon must
  **reject any message not in this enum** and must **never** expose a generic
  "run this command" verb. There is no `exec` verb, no `eval`, no shell passthrough.
- **At the effect layer:** the `spawn` verb *launches Claude Code*, which is an
  arbitrary-code-execution engine running as the operator, seeded with a
  controller-supplied `prompt` and `claude_settings_bead` (hooks = shell commands,
  MCP servers, `Bash(*)` allow), under `--dangerously-skip-permissions`.

So "only well-typed verbs" is true and worth enforcing (M15), but it is **not** the
security boundary people expect it to be. The real boundary is **M2 + M4**: *whose*
settings the spawned agent gets, and *whether the operator provisioned a profile
that permits this spawn at all.* Everything else is defence-in-depth around those
two gates.

---

## 4. The core reframe — a spawn is code execution, so bound it like one

Grounding, from the live code:

- The default agent launch command *is* `claude --dangerously-skip-permissions`
  (`gardenio/controller/cmd/gb/agent_start.go:158`, `init_agent_loop.go:825`,
  `agent_start_k8s.go:67`; the mock path at `podspec.go:327,509` mirrors it). In the
  pod world this is acceptable because the pod is a disposable, network-policied,
  non-prod sandbox. **On an operator laptop none of those containments exist by
  default.**
- Claude settings config beads merge `hooks`, `mcpServers`, and
  `permissions.allow` (`gardenio/controller/cmd/gb/config_resolve_test.go:88,104,219,423`;
  merge path `gardenio/controller/internal/claudeconfig/resolve.go`). A `hooks`
  entry is `{"type":"command","command":"<shell>"}` — it runs on session events.
  Whoever controls the resolved settings controls code that runs on the laptop.
- The spawn envelope references a `claude_settings_bead` by id
  (spawn-action-wire §Payload schema). **If the daemon naively resolves whatever
  bead id it is handed, an attacker who can influence that id (TA2) or author a bead
  (any fleet agent can `kd create`) has an RCE primitive.**

The design implication is a hard requirement, not a nicety:

> **M2 — The daemon MUST NOT resolve an arbitrary `claude_settings_bead` id from the
> envelope.** The controller resolves the effective settings server-side (it already
> owns `claudeconfig.ResolveEffectiveConfig`), snapshots them, and ships the
> **snapshot** inside the signed envelope — or ships a content-hash the daemon pins
> against a controller-served, auth-gated fetch. The daemon treats the settings as
> opaque, verifies the controller signature over them, and **strips or refuses**
> any `hooks`/`mcpServers` the operator's local permission profile has not
> allow-listed (M4). "Chase this bead id" is forbidden.

This single requirement collapses TA2's and TA6's most dangerous variants: you
cannot point a spawn at an attacker-authored hooks bead if the daemon never
dereferences an envelope-supplied bead id.

---

## 5. Mitigation ladder — mandatory vs nice-to-have

### Mandatory (M1–M15) — gate Phase-3 implementation

Each is stated as an enforceable requirement with the actor(s) it bounds.

| ID | Requirement | Bounds |
|---|---|---|
| **M1** | **Default-deny, opt-in permission profile.** No spawn executes unless the operator has provisioned a local profile (a `~/.config/gardenio/daemon/profile.json` or a signed profile bead) allow-listing (project, repo, working-dir root, ref-kind, apply-vs-dry-run). Empty/absent profile ⇒ every spawn refused with an operator-visible reason. | TA2, TA5, TA6 |
| **M2** | **Pinned settings origin** (see §4). Daemon never dereferences an envelope-supplied `claude_settings_bead` id; it consumes only controller-signed, snapshotted settings and strips non-allow-listed hooks/MCP. | TA2, TA6 |
| **M3** | **Operator-consent binding.** Every spawn is bound to a specific operator-initiated click in beads-viewer via the Layer-3 JWT (ka-0000005882): `sub`, `decision_id`, `nonce`, `exp≤60s`. The daemon verifies the JWT signature (pinned controller key), `aud==own daemon-id`, and that the bound decision is still `open` (defence-in-depth RPC). No valid JWT ⇒ no spawn. | TA2, TA4 |
| **M4** | **Daemon-side profile enforcement, never browser-supplied.** The `permissions_profile` string in the envelope is a *selector into the operator's local profiles*, not a grant. The daemon resolves the actual grant locally and enforces it. A spawn requesting a working-dir outside the allow-listed roots is refused even if the envelope says otherwise. | TA2, TA5, TA6 |
| **M5** | **UDS hardening.** Control socket at `~/.gardenio/gb-daemon.sock` mode `0600`, parent dir `0700`; verify peer credentials (`SO_PEERCRED`/`LOCAL_PEERCRED`) == daemon's own uid on every connection. No TCP listener, ever. | TA3 (different-uid) |
| **M6** | **Tamper-evident audit log** (full spec §6): append-only, hash-chained local log + a controller-side `spawn-intent` bead created **before** the forward. Server + local records must reconcile; divergence is an alarm. | TA3, TA5, TA7 |
| **M7** | **No new standing secrets.** The daemon holds only its own client cert/key (A3) and the embedded manifest-verify pubkey (A4). It does **not** cache operator OIDC refresh tokens, cloud creds, or prod tokens. It reads operator creds only transiently, only when a spawn's profile permits, and never persists them. | TA3, TA4, TA5 |
| **M8** | **Operator-visible revocation + short lifetimes.** `/account/devices` lists paired daemons; "revoke" fails the next reconnect closed. Cert TTL ≤90d; pairing code TTL ≤5min one-shot; JWT `exp≤60s`. | TA4, TA5 |
| **M9** | **Signed releases + signed auto-update manifest.** Daemon binary and the `manifest.json` are signed (minisign/cosign); the daemon verifies the signature before atomic-swap. Signature failure ⇒ no update + audit event (ka-0000006867 §3.3). | TA1 |
| **M10** | **Manifest-key custody + rotation.** The manifest-signing private key lives in KMS (or the same custody as the calver release-signing key), never in the repo or CI plaintext. Documented yearly rotation with an overlap window (daemon accepts old+new for the rotation period). The verify pubkey is embedded at build time and pinned. | TA1 |
| **M11** | **Reproducible builds + SBOM + provenance.** The daemon binary is built reproducibly from the tagged commit with an attestation (SLSA-style provenance), so a poisoned artifact is detectable by rebuild. | TA1 |
| **M12** | **Minimal, pinned dependency surface.** The daemon adds no new third-party runtime deps beyond what `gb` already vendors; all deps pinned + checksum-verified (`go.sum`); dependency additions in the daemon path get security review. | TA1 |
| **M13** | **Untrusted daemon input.** The controller treats every message *from* a daemon (status, logs, `last_action`, diagnostics) as untrusted: length-capped, schema-validated, HTML/markdown-escaped before being rendered in beads-viewer or written to a bead. A compromised daemon cannot inject content into operator surfaces or beads. | TA5 |
| **M14** | **Process sandbox for the spawned session.** The daemon launches the Claude Code session under an OS sandbox that scopes filesystem + network to the profile: macOS `sandbox-exec`/App Sandbox profile (or at minimum a dedicated working-dir + `TMPDIR`); Linux a `bwrap`/namespaces jail or systemd unit with `ProtectHome=`/`ReadWritePaths=`/`ReadOnlyPaths=` + seccomp. The *daemon itself* runs under a hardened systemd unit (`NoNewPrivileges=`, `ProtectSystem=strict`, `ProtectHome=read-only` except its own state dir, `RestrictAddressFamilies=`, seccomp allow-list). See §7. | TA5, TA6 |
| **M15** | **Strict envelope validation + no shell interpolation.** Envelope fields are schema-validated (type, length, charset). `working_dir`/`ref`/`repo_url` are passed to `git`/the launcher as **exec argv**, never concatenated into `sh -c`. `ref` restricted to `^[A-Za-z0-9._/-]+$`; `working_dir` canonicalized (`filepath.Clean` + symlink-resolve) and asserted under an allow-listed root (M4) *after* resolution, to defeat `../` and symlink escapes. | TA6 |

### Nice-to-have (N1–N8) — strengthen, not gate

| ID | Enhancement | Value |
|---|---|---|
| **N1** | **Per-spawn OS-level presence confirmation** (Touch ID / polkit prompt) as an operator-opt-in for high-sensitivity profiles (e.g. any profile that permits prod repos). 1Password-style. Trades UX for a hard human-in-the-loop gate. |
| **N2** | **Dry-run-by-default profiles.** A profile can require the *first* spawn on a new repo/ref to run read-only, with the operator promoting to apply. |
| **N3** | **Egress allow-list for the daemon** (not the spawned agent): the daemon only needs the controller WS host + the update host. A local firewall rule / `RestrictAddressFamilies` narrows exfil paths for a subverted daemon. |
| **N4** | **Rate-limit + anomaly alerts.** >N spawns/hour, spawns at unusual times, or a spawn to a never-before-seen repo raises a controller-side alert on the operator's card. |
| **N5** | **Secret-scanning the spawned session's diffs** before any push, to catch a subverted spawn exfiltrating creds through a commit. |
| **N6** | **Ephemeral worktree isolation** (spawn-action-wire "open a worktree" toggle): spawn into a throwaway `git worktree` under a scratch root, never the operator's primary checkout, limiting blast radius to a disposable dir. |
| **N7** | **Transparency log** of manifest hashes (Sigstore Rekor-style) so an operator can independently verify the update they got matches what the fleet got — detects targeted-update attacks. |
| **N8** | **`gb daemon uninstall --wipe`** completeness check (ka-0000006867 §5.5): remove unit/plist, cert+key, cached binaries, job state, audit logs (or archive them), and notify the controller to revoke. A half-uninstall that leaves a live cert is a latent A3. |

---

## 6. Audit-log specification

The audit log is the load-bearing control for the compromised-daemon case: we cannot
prevent a fully-owned daemon from misbehaving, but we can make misbehaviour
**evident and attributable**. Two records, reconciled.

### 6.1 Local audit log (on the operator laptop)

- **Location:** `~/.gardenio/logs/audit.log` (dir `0700`, file `0600`).
- **Format:** append-only JSON Lines, one event per line.
- **Tamper-evidence:** each line carries `seq` (monotonic) and
  `prev_hash` = SHA-256 of the previous line's canonical bytes, forming a hash
  chain. The daemon periodically (every N events and every heartbeat) emits the
  current `head_hash` to the controller over the WS. The controller stores the
  chain of heads. **An attacker who truncates or rewrites the local log cannot match
  the head hashes the controller already recorded** — divergence ⇒ alarm. (This is
  the affordable substitute for a TPM/secure-enclave-backed log; N7-style external
  anchoring is the stronger version.)
- **Rotation:** size/time-based; rotated segments keep the chain (new segment's
  first `prev_hash` = last segment's `head_hash`).

**Every spawn writes these events, locally:**

| Event | Fields (beyond `seq`, `ts`, `prev_hash`) |
|---|---|
| `spawn.received` | `spawn_id`, `action_id`, `decision_id`, `operator_sub`, `jwt_jti`, `envelope_hash`, `expires_at` |
| `spawn.authz.checked` | `spawn_id`, `jwt_valid`, `nonce_fresh`, `decision_state`, `profile_matched` (which profile), `verdict` (allow/deny), `deny_reason?` |
| `spawn.settings.pinned` | `spawn_id`, `settings_hash`, `hooks_stripped[]`, `mcp_stripped[]` |
| `spawn.launched` | `spawn_id`, `working_dir` (resolved, canonical), `ref`, `sandbox_profile`, `pid`, `session_handle` |
| `spawn.completed` / `spawn.failed` | `spawn_id`, `exit`, `duration_s`, `failure_reason?` |
| `spawn.denied` | `spawn_id`, `reason` (profile-miss / expired-jwt / replay / dir-escape / offline-decision) |
| `daemon.started` / `daemon.updated` | `version`, `prev_version?`, `manifest_hash`, `sig_verified` |
| `daemon.cert.rotated` | `fingerprint_old`, `fingerprint_new` |

**What the local log MUST NOT contain** (telemetry-redaction seam from
ka-0000006867 §5.4): the operator's credential *values*, the spawned session's file
*contents*, or full command output. It records *metadata about* the spawn, not the
spawn's data. `envelope_hash`/`settings_hash` capture "what was authorized" without
storing prompts verbatim where prompts might contain sensitive context (store a hash;
the full prompt lives on the controller-side decision/spawn-intent bead, which is
already the system of record).

### 6.2 Controller-side record (system of record)

- The **`spawn-intent` bead** (spawn-action-wire §Audit) is created **before** the
  forward, so even a dropped/failed forward — or a daemon that goes silent — leaves
  a trace. Fields: `spawn_id`, `operator`, `daemon_id`, `option_id`, `ref`,
  `working_dir`, `state`, `jwt_jti`, `envelope_hash`, `audit_head_hash` (the local
  chain head the daemon last reported).
- Lifecycle callbacks update the bead; **each transition also posts a comment**, so
  the beads-viewer activity-log renders a timeline (and the bridge auto-forwards it
  to the operator's Slack thread — advice ka-0000000409).
- **Reconciliation job:** a controller-side check compares the `spawn-intent` beads
  it expects against the audit heads the daemon reported. A daemon that ran a spawn
  it never received an envelope for (self-minted), or dropped one it acked, shows up
  as a mismatch. This is what catches TA5/TA7 — a lying daemon.

### 6.3 Tamper-evidence threat cases

- **Daemon deletes local log:** heads already at the controller ⇒ gap detected on
  next reconcile; the missing-heartbeat + missing-heads pattern flags the daemon.
- **Daemon forges a "success" it subverted:** the *content* of what ran is not in the
  daemon's control to prove; but the diff/push (if any) is separately visible in
  GitLab, and N5 secret-scanning + N4 anomaly alerts backstop. Full prevention of a
  fully-owned daemon lying about a local-only side effect is **not achievable** —
  §8 states this honestly as a residual risk.
- **Attacker rewrites history + replays old heads:** the controller stores heads
  monotonically with `seq`; a replayed older head with a lower `seq` than one
  already seen is rejected.

---

## 7. Process isolation & network

The pod defaults (`--dangerously-skip-permissions` in a network-policied,
non-prod, disposable container) do **not** transfer to a laptop. Two things must be
isolated: the **daemon process** and the **spawned session**.

### 7.1 The daemon process

Run under a hardened per-user service unit (ka-0000006867 picks launchd/systemd):

- **Linux systemd `--user` unit:** `NoNewPrivileges=yes`, `ProtectSystem=strict`,
  `ProtectHome=read-only` with `ReadWritePaths=%h/.gardenio`, `PrivateTmp=yes`,
  `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6`, `SystemCallFilter=@system-service`
  (seccomp allow-list), `MemoryDenyWriteExecute=yes`, `LockPersonality=yes`,
  `RestrictSUIDSGID=yes`. The daemon needs: its own state dir (rw), the control UDS,
  and outbound TLS to two hosts. Nothing else.
- **macOS launchd:** run as the user LaunchAgent (never a root LaunchDaemon — the
  Docker-Desktop anti-pattern from ka-0000006867). Ship an App Sandbox / `sandbox-exec`
  profile scoping the daemon to `~/.gardenio` + outbound network. Root is never
  required for this daemon's job.
- **No listening TCP port.** Control plane is the UDS only (M5). The only network is
  the daemon-initiated outbound WS + the update fetch.

### 7.2 The spawned session (the part that actually runs untrusted code)

- Launch the Claude Code session in a **restricted working dir** derived from the
  profile (M4), ideally an **ephemeral worktree** (N6) under a scratch root, not the
  operator's primary checkout.
- **Filesystem scope:** on Linux, wrap the session in `bwrap`/namespaces or a
  transient systemd scope with `ReadWritePaths=<worktree> <TMPDIR>`,
  `ReadOnlyPaths=<repo .git>` if needed, `ProtectHome=` for everything else the
  profile did not grant. On macOS, `sandbox-exec` with a generated profile pinned to
  the working dir. This is what keeps a spawned agent's `Bash(*)` from wandering into
  `~/.aws` or `~/.ssh`.
- **Network scope (open question — see §9):** the *whole point* of a local spawn is
  often to reach local/prod things the cloud fleet cannot, so a blanket egress-deny
  on the session defeats the feature. The profile must express network scope
  explicitly (default-deny with an operator allow-list), and this is the sharpest
  tension in the design (ka-0000004678 permissions child + §9 Q4 below).
- **Credential scope:** the session inherits only the env the profile grants. The
  daemon must **not** blanket-inherit its own environment into the session; it
  constructs a minimal env from the profile (M7). If a spawn needs AWS creds, the
  profile says so explicitly and the operator sees it in the audit trail.

### 7.3 Secret/key handling on disk (consolidated)

| Secret | Path | Perms | Lifetime / rotation |
|---|---|---|---|
| Daemon client cert + key (A3) | `~/.config/gardenio/daemon/{cert,key}.pem` | `0600`, dir `0700` | ≤90d, auto-renew over WS; revocable via `/account/devices` |
| Manifest-verify pubkey (A4) | embedded in binary | n/a (read-only in binary) | yearly rotation w/ overlap; private half in KMS (M10) |
| Operator creds (A2) | `~/.config/gardenio/credentials.json`, `~/.aws`, etc. | `0600` (credstore) | **Not owned by the daemon.** Daemon must never persist copies (M7). |
| Local audit log (A6) | `~/.gardenio/logs/audit.log` | `0600`, dir `0700` | rotated; hash-chained (M6) |

Prefer the OS keyring backend (`GB_CREDSTORE_BACKEND`, credstore already supports
macOS Keychain / libsecret — `credstore.go:147`) over the 0600 file fallback for the
daemon cert where available, so a flat-file read (TA3) is harder.

---

## 8. Residual risks (stated honestly)

The design bounds blast radius; it does not eliminate it. What remains:

1. **A fully-compromised laptop is game-over for that operator (TA3/TA5).** If an
   attacker already has the operator's shell, the daemon is not the weakest link —
   the operator's creds and SSH agent already are. The daemon's contribution to blast
   radius is *bounded* (M7 no-new-secrets, M14 sandbox, M8 revocation) but nonzero: a
   live daemon is a push channel an attacker can ride. Revocation + audit make it
   *detectable and stoppable*, not *impossible*.
2. **A fully-owned daemon can lie about local-only side effects (TA7).** If a
   subverted daemon runs a spawn and reports success while doing something else, and
   that something leaves no external artifact (no git push, no network call the
   allow-list logs), the controller cannot cryptographically disprove the report. M6
   reconciliation catches *structural* lies (self-minted or dropped spawns); it does
   not catch a semantically-truthful-but-subverted execution. Mitigation is N4/N5
   backstops + the fact that the interesting side effects (commits, prod changes)
   *do* leave external traces.
3. **A compromised controller with the signing key (TA2 worst case) is out of
   scope** and is game-over platform-wide, not just for the daemon. The daemon-side
   gates (M2/M4) still prevent *silent* or *out-of-profile* abuse, which is the most
   an out-of-scope-trust-anchor design can offer.
4. **Targeted auto-update attack** (serve a poisoned manifest to *one* laptop): M9
   signature stops an unsigned payload, but a signer-key compromise (M10) or a
   targeted-but-signed update is only caught by N7 transparency logging, which is
   nice-to-have. Recommend promoting N7 to mandatory if the operator cohort includes
   prod-privileged laptops.

---

## 9. Open security questions for Phase-2 synthesis

These need an explicit answer in the ratifying `gb decision`; this doc gives a
recommendation for each.

- **Q1 — Settings delivery: snapshot-in-envelope vs pinned-hash-fetch?** M2 requires
  one of them. Snapshot is self-contained (works offline, one signature); hash-fetch
  keeps envelopes small but adds a controller round-trip. **Recommend snapshot-in-
  envelope** (fewer moving parts, offline-tolerant), with the settings signed as part
  of the envelope.
- **Q2 — Sandbox baseline: mandatory or best-effort?** A per-OS sandbox (M14) is real
  work and macOS App Sandbox for a spawned interactive dev session is fiddly.
  **Recommend: mandatory working-dir confinement + credential-env minimization from
  v0; full seccomp/App-Sandbox as a fast-follow**, but *no prod-repo profile ships
  until the full sandbox lands.*
- **Q3 — Presence confirmation (N1): opt-in or mandatory for prod-scoped profiles?**
  **Recommend mandatory for any profile that permits a prod repo or apply-mode on a
  prod-adjacent working dir.** The consent-in-viewer click (M3) is sufficient for
  low-sensitivity spawns; a second local human gate is worth the friction for prod.
- **Q4 — Network scope for the spawned session.** The feature's value is local/prod
  reachability the cloud fleet lacks, so blanket-deny defeats it, but blanket-allow
  is an exfil highway. **Recommend: profile-declared, default-deny egress allow-list
  for the session**, owned jointly with ka-0000004678. This is the single hardest
  open question and should not be hand-waved in synthesis.
- **Q5 — Manifest transparency log (N7): promote to mandatory?** **Recommend yes if
  any operator in the initial cohort has prod-privileged laptop access** (targeted-
  update risk is the difference between "an operator" and "the operator who can reach
  `gardenio-prod`"). Otherwise Phase-4.
- **Q6 — Audit-head anchoring cadence.** How often the daemon reports `head_hash`
  trades detection latency against chattiness. **Recommend: every heartbeat (30s) +
  on every terminal spawn event.**
- **Q7 — What happens to the audit log on `uninstall`?** Wipe vs archive. **Recommend
  archive to a controller-uploaded blob on clean uninstall** (so the forensic record
  survives), wipe only on `--purge`.

---

## 10. Linkbacks

- **Bead:** ka-0000006932 (this child)
- **Parent epic:** ka-0000006791
- **Sibling research:** ka-0000005882 (authn/authz — channel threat model; this doc
  resolves its deferred T6 "compromised daemon foothold"), ka-0000006867 (daemon
  lifecycle — supply-chain, telemetry-redaction, uninstall seams resolved here as
  M9–M11, M6 §6.1, N8), ka-0000004798 (spawn-action wire — envelope + audit-bead-
  before-forward seam, resolved here as M2/M6), ka-0000004678 (permissions model —
  M1/M4/Q4 hand back to it)
- **Sister epic:** ka-0000004527 (escalations surface)
- **Code anchors (evidence, not implementation targets):**
  - `gardenio/controller/cmd/gb/agent_start.go:158`, `init_agent_loop.go:825`, `agent_start_k8s.go:67` — default launch is `claude --dangerously-skip-permissions` (mock mirror at `controller/cmd/controller/podspec.go:327,509`)
  - `gardenio/controller/cmd/gb/config_resolve_test.go:88,104,219,423` — hooks / mcpServers / `Bash(*)` in Claude settings beads
  - `gardenio/controller/internal/claudeconfig/{resolve,effective,settings}.go` — the settings resolve+merge path (M2)
  - `gardenio/clicreds/credstore/credstore.go:98,147` — on-disk cred path, 0600, keyring backend
  - `gardenio/controller/cmd/gb/login.go` — pairing-UX precedent (ka-0000005882)
