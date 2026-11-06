# Config bead surface for the `gb-daemon`

> Research child **4 of 9** for **ka-0000006791** (epic: client-side `gb` daemon
> for the "spawn escalated agent" action).
>
> **Bead:** ka-0000000166
> **Parent epic:** ka-0000006791
> **Builds on:** child 1 daemon lifecycle (ka-0000006867 / `daemon-lifecycle.md`)
> and child 2 authn/authz (ka-0000005882 / `authn-authz.md`).
> **Sister children referenced:** child 3 spawn-action wire (`spawn-action-wire.md`),
> child 6 permissions model (ka-0000004678 / `permissions-model.md`).
> **Dogfood mandate:** ka-0000006879 (rich-report widget instances).
>
> **Status:** research / recommendation only. No code lands until a phase-2
> `gb decision` ratifies the config surface. **Out of scope: implementation.**

---

## TL;DR

| Question | Recommendation |
| --- | --- |
| **Where does daemon config live?** | Split three ways. **Fleet policy** (permission profiles, spawn-dir defaults, Claude-settings overlay) → **config beads**, resolved by the *existing* `claudeconfig` layered resolver. **Machine state** (pairing key, daemon id, last operator, spawn history, install version) → **daemon-local** file under `~/.gardenio/`, never a bead. **Per-spawn parameters** (ref, working-dir override) → the signed envelope from child 3, not stored config. |
| **New scope needed?** | Yes — exactly one. The current resolver scopes by `global → project → role → agent`, which are all **fleet** dimensions. The daemon is **operator-owned**, so we add an **`operator:<sub>`** scope that slots between `project` and `role` in specificity and is the primary owner axis for daemon config. |
| **Reuse Claude-settings shape?** | Reuse the **mechanism wholesale** (the `claudeconfig` layered-merge resolver, `type=config` bead + scope labels, one-level-deep map merge). Reuse the **`claude-settings` / `claude-mcp` beads verbatim** for the spawned local session's `env` / `model` / `mcpServers` / `permissions.allow`. Add **three new categories** for the daemon-specific knobs that have no Claude-settings analogue. |
| **New config-bead categories** | `daemon-settings` (operator-scoped daemon behavior), `daemon-permission-profile` (the child-6 permission surface, named-and-referenced from the envelope), `daemon-spawn-defaults` (per-project working-dir + ref + worktree defaults). |
| **Daemon-local config** | A single `~/.gardenio/daemon.json` (0600) holding pairing key/cert path, `daemon_id`, `last_operator_sub`, bounded `recent_spawns[]` ring, and `install_version`. Read-write by the daemon only; **never** synced to a bead (child-8 security: it is the device credential + PII, not fleet policy). |

The rest of this doc is: the knob→home mapping table (§3), the new-scope
argument (§4), the reuse map against `claude-settings` (§5), example bead bodies
for every new category (§6), the daemon-local file schema (§7), the resolution
& precedence model (§8), and open questions punted to phase-2 (§9).

---

## 1. Why this question is non-trivial

The daemon has knobs that look superficially like existing config beads —
"permissions", "env", "which directory" — so the naive answer is "make them all
config beads, reuse `claude-settings`, done." That is wrong in two directions at
once:

1. **Some knobs must NOT be beads.** The pairing key, the per-daemon client
   cert path, and the recent-spawn history are *device secrets and machine
   state* (child 2 §Layer-2, child 8 §4). A bead is a fleet-visible,
   operator-and-agent-readable row in a shared Postgres. Putting a device
   credential there is a security defect, not a convenience. Machine state also
   changes on every spawn — bead churn for data no other actor consumes.

2. **The existing scope model has no owner axis for "this operator's laptop."**
   Every `claude-settings` bead today is scoped `global | project | role |
   agent` — all **fleet** dimensions describing *cloud* agents. A daemon belongs
   to a *human operator*, and the same operator may pair several laptops. There
   is no `operator:` scope today, so we cannot express "Matt's daemons default to
   `~/code`, Tim's to `~/dev`" without one.

So the real design work is (a) drawing the bead/local/envelope boundary
correctly, (b) adding the one missing scope, and (c) reusing the *mechanism* —
not blindly the *categories* — of `claude-settings`.

---

## 2. What we are auditing (inputs)

| Input | What it gave us |
| --- | --- |
| `kbeads/cmd/kd/config_bead.go` | The **ka-side** config-bead resolver: `type=config` beads, title = category name, labels carry scope (`global`, `role:<x>`). Superset label matching (`labelsMatchConfig`)[^matcher]. This is the CLI read path; the controller has a richer one (below). |
| `gardenio/controller/internal/claudeconfig/` (`registry.go`, `resolve.go`, `settings.go`, `effective.go`) | The **authoritative** layered resolver. `configCategories` registry, `MergeStrategy` (Override / Concat / ConcatList), `resolveSortedLayers` scope-sorting via `advice.CategorizeScope` + `GroupSortKey`, `DefaultUserSettings()` fleet baseline, `MergeSettingsOntoDefaults`. **This is the reuse target.** |
| Live config beads (`kd list --type=config`) | `claude-settings` (`{"env":{"CLAUDE_CODE_EFFORT_LEVEL":"xhigh"}}`, `{"disableBundledSkills":true}`), `claude-mcp` (`{"mcpServers":{…}}`), `claude-instructions`, `prerun`, `nudge-prompts`, `wrapup-config`, `type:<name>`, `view`. All are small JSON docs whose title = category and labels = scope. |
| `gardenio/controller/internal/podmanager/manager.go` (`IdentityEnvVars`, `buildEnvVars`) | The **agent-pod env injection** pattern: identity env (`BOAT_ROLE`, `BEADS_ACTOR`, `BOAT_TASK_ID`, `BEADS_OPERATED_BY`, …) is built from the `AgentPodSpec`; secrets come via `SecretKeyRef`, never inline. This is the model the daemon **mirrors in spirit** but not literally (no k8s on a laptop). |
| `advice.CategorizeScope` (`internal/advice/matching.go`) | Scope derivation from labels: `agent: > role: > project: > global`. The place a new `operator:` scope must be threaded. |

---

## 3. Knob → home mapping table

The single most important artifact of this research. Each daemon knob is
assigned exactly one home: **config bead** (fleet-visible policy, layered),
**daemon-local** (machine state / device secret), or **envelope** (per-spawn,
from child 3).

| Knob | Home | Category / field | Scope owner | Rationale |
| --- | --- | --- | --- | --- |
| **permissions** — per-action allowlist (which decision kinds may spawn), per-repo, per-dir root, dry-run vs apply, network/fs scope | **config bead** | `daemon-permission-profile` (named; child-6 schema) | **operator** (default), overridable per-project | Fleet-auditable policy; the envelope names a profile, the daemon resolves the *actual* grant locally (child 6 §3, "name is a selector not a grant"). |
| **spawn directory** — per-project default working dir | **config bead** | `daemon-spawn-defaults` → `projects[].default_working_dir` | **operator** (paths are laptop-specific) | Operator's `~/code/gardenio` is not fleet-global; belongs to the operator scope. |
| **spawn directory — per-task override** | **envelope** | envelope `spawn.working_dir` | n/a (per-spawn) | Already resolved by the viewer at click-time (child 3 §envelope `working_dir`); daemon validates against the permission profile's dir roots. |
| **worktree toggle** — open a git worktree vs check out in place | **config bead** | `daemon-spawn-defaults` → `projects[].worktree` (bool, per-project default) + envelope override | **operator** | Sensible per-project default (monorepo → worktree); per-spawn override in envelope. |
| **repo-ref selection** — head / PR head / bead-specified ref | **envelope** (value) + **config bead** (default) | envelope `spawn.ref`; default in `daemon-spawn-defaults.projects[].default_ref` | n/a / operator | The concrete ref is a property of *this* decision (child 3 `default_ref`); the fallback default is operator policy. |
| **Claude env** (`CLAUDE_CODE_EFFORT_LEVEL`, `ENABLE_TOOL_SEARCH`, …) | **config bead** | **reuse `claude-settings`** `env` | global→operator→project→role | 1:1 reuse. The spawned local session is a Claude Code session; it wants the same env overlay (§5). |
| **model** | **config bead** | **reuse `claude-settings`** `model` | same | 1:1 reuse. |
| **hooks** | **config bead** | **reuse `claude-hooks`** (Concat merge) | same | 1:1 reuse — but see §5 caveat: local-machine hook paths differ. |
| **MCP servers** | **config bead** | **reuse `claude-mcp`** `mcpServers` | same | 1:1 reuse for cloud-parity servers; local-only servers via an operator-scoped `claude-mcp` bead. |
| **Claude permission list** (`permissions.allow`/`deny`) | **config bead** | **reuse `claude-settings`** `permissions` | same | Reuse for the *spawned Claude session's* tool permissions — distinct from the *daemon's* spawn-authorization permissions (`daemon-permission-profile`). Two different permission surfaces; §5.3. |
| **daemon behavior** — auto-update channel, heartbeat interval, spawn concurrency cap, default fleet-lane visibility | **config bead** | `daemon-settings` | **operator** | Operator policy, small, fleet-auditable; overlays a hardcoded daemon baseline (mirror of `DefaultUserSettings`). |
| **pairing key / client cert path** | **daemon-local** | `daemon.json` → `pairing.cert_path`, `pairing.key_path` | device | Device credential (child 2 Layer-2). MUST NOT be a bead (child 8). |
| **daemon id** | **daemon-local** | `daemon.json` → `daemon_id` | device | Minted at pairing; identifies *this* laptop. Referenced *by* beads (envelope `daemon_id`) but owned locally. |
| **last-used operator id** | **daemon-local** | `daemon.json` → `last_operator_sub` | device | Machine state; changes at pairing; no fleet consumer. |
| **recent-spawn history** | **daemon-local** | `daemon.json` → `recent_spawns[]` (bounded ring) | device | High-churn local audit cache. Authoritative audit is the decision-bead comment (child 3); the local ring is only for the daemon's own idempotency + a `gb daemon status` view. |
| **install version** | **daemon-local** | `daemon.json` → `install_version` | device | Set by the installer/auto-updater (child 1); the daemon compares it to the signed manifest. |

**Boundary rule of thumb:** *If the controller, another operator, or a cloud
agent would ever want to read or audit it → config bead. If only this daemon
process on this laptop consumes it, or it is a secret/credential/PII → local. If
it is specific to one spawn click → envelope.*

---

## 4. The one new scope: `operator:`

### 4.1 Why the existing four scopes don't cover it

`advice.CategorizeScope` derives scope from labels with fixed precedence
`agent: > role: > project: > global`. All four describe **cloud fleet** entities:
a *project* is a repo/product line, a *role* is a fleet job (crew, gardener), an
*agent* is one cloud pod. **None names a human operator or their laptop.**

The daemon's natural owner is the **operator** — the human who paired the
device. Daemon config ("my laptops default to `~/code`, dry-run only, stable
channel") is *per-operator*, sometimes further specialized *per-project* ("but
for the monorepo, use a worktree"). Neither `role:` nor `agent:` fits: there is
no fleet role "operator", and `agent:` is a pod name, not a person.

### 4.2 Proposal

Add **`operator:<sub>`** as a new scope label, where `<sub>` is the operator's
OIDC subject (the same identity child 2 pins at pairing time and the same
`BEADS_OPERATED_BY` / `operated_by` value already used fleet-wide — advice
ka-0000000014 / ka-0000005310 note these are `slack:U…` or `op-<id>` forms; the
daemon uses the OIDC `sub`).

Precedence slot — **between `project` and `role`**:

```
global  <  project  <  operator  <  role  <  agent
(least specific)                              (most specific)
```

Rationale for that slot: operator config should override a *project-wide* fleet
default (an operator's personal working-dir choice beats a project default) but
a *role*-scoped or *agent*-scoped bead (which only exist for the spawned session,
not the daemon) is more specific still. In practice daemon-category beads
(`daemon-settings`, `daemon-permission-profile`, `daemon-spawn-defaults`) are
authored at `operator:` (and occasionally `global` for fleet-wide daemon
defaults, or `operator:<sub>` + `project:<p>` for the per-project override, which
matches via superset labels). The reused Claude-categories (`claude-settings`
etc.) keep using their existing scopes, plus can now *also* be authored at
`operator:` for "my local sessions run xhigh."

### 4.3 Implementation seam (for phase-3, not now)

`CategorizeScope` gains one case:

```go
case strings.HasPrefix(clean, "operator:") && scope != "role":
    scope, target = "operator", strings.TrimPrefix(clean, "operator:")
```

and `GroupSortKey` gains the `operator` rank between `project` (1) and `role`
(2) — e.g. `"1^:operator:<sub>"`. The daemon's subscription list (analogous to
an agent's role subscriptions in `resolveSortedLayers`) is
`["global", "project:<p>", "operator:<sub>"]`, built from the pairing identity +
the envelope's project. **Everything else in `claudeconfig` is reused
unchanged** — the merge strategies, the superset label match, the newer-wins
tiebreak.

> This is the *only* code change the config surface implies, and it is
> deliberately tiny: one enum case + one sort rank. That is the whole point of
> reusing `claudeconfig` rather than inventing a parallel local-config loader.

---

## 5. Reuse map against `claude-settings`

### 5.1 Mechanism — reuse wholesale

| Mechanism | Reused? | Notes |
| --- | --- | --- |
| `type=config` bead, title = category, labels = scope | **1:1** | New categories are just new titles. |
| `claudeconfig` layered resolver (`resolveSortedLayers`) | **1:1** | + the one `operator:` scope case (§4.3). |
| One-level-deep map merge (`mergeSimpleLayers`) | **1:1** | `env`/`permissions`/`mcpServers`/`projects` all merge sub-key by sub-key. |
| Concat merge for hooks (`mergeHookLayers`) | **1:1** | If daemon reuses `claude-hooks`. |
| `DefaultUserSettings()` baseline + `MergeSettingsOntoDefaults` | **pattern** | Daemon gets its *own* `DefaultDaemonSettings()` baseline for `daemon-settings`, merged the same way. |
| Superset label match (`labelsMatchConfig`)[^matcher] | **1:1** | Lets `operator:<sub>` + `project:<p>` beads match a spawn in that project. |

### 5.2 Categories — 1:1 vs adapted vs new

| Claude-settings field / category | Daemon reuse | Verdict |
| --- | --- | --- |
| `claude-settings` → `env` | Spawned session env overlay | **1:1** — same bead can target `operator:`/`global`. |
| `claude-settings` → `model` | Spawned session model | **1:1** |
| `claude-settings` → `permissions.allow`/`deny` | Spawned session's *tool* permissions | **1:1** (but ≠ daemon spawn-authz; §5.3) |
| `claude-settings` → `alwaysThinkingEnabled`, `enableAllProjectMcpServers`, `skipDangerousModePermissionPrompt` | Spawned session | **1:1** — but reconsider `skipDangerousModePermissionPrompt` on a *laptop* with real filesystem access (child 8): default it to **false** for local spawns via the daemon baseline. **Adapted default.** |
| `claude-mcp` → `mcpServers` | Spawned session MCP | **1:1** for cloud-parity servers; **adapted** for local-only servers (paths/commands differ on a laptop) via an `operator:`-scoped `claude-mcp` bead. |
| `claude-hooks` → `hooks` | Spawned session hooks | **Adapted** — hook *scripts* live at fleet paths (`/…` in the pod image); a laptop has different paths. Either ship the same hooks in the local `gb` install or scope a local hooks bead at `operator:`. Flag for phase-2. |
| `prerun`, `nudge-prompts`, `claude-instructions`, `wrapup-config` | Spawned session bootstrap | **1:1 where the daemon wants fleet parity** — a locally-spawned agent that registers back into the fleet (child 5) should get the same `prime`/nudge/wrapup contract. No change. |
| *(none)* — daemon lifecycle knobs | — | **NEW → `daemon-settings`** (§6.1). |
| *(none)* — spawn authorization surface | — | **NEW → `daemon-permission-profile`** (§6.2, child-6 schema). |
| *(none)* — per-project working-dir/ref/worktree | — | **NEW → `daemon-spawn-defaults`** (§6.3). |

### 5.3 The two-permission-surfaces trap (call it out loudly)

There are **two** distinct permission concepts and they must not be conflated:

1. **Daemon spawn-authorization** — *"may this envelope spawn here, this way?"*
   → `daemon-permission-profile` bead, evaluated by the daemon before it
   launches anything (child 6). This is the security boundary.
2. **Spawned-session tool permissions** — *"once running, what Bash/Write/etc.
   may the Claude session do?"* → the reused `claude-settings.permissions`
   `allow`/`deny`, written into the spawned session's `~/.claude/settings.json`
   exactly as the pod path does today.

They reuse the *same bead mechanism* but are *different beads with different
scopes and different enforcement points*. A design that merges them would let a
loose Claude tool-allowlist silently widen the daemon's spawn blast radius.

---

## 6. Example bead bodies

All are `type=config`, `kind=config`, title = the category name, labels carry
scope. Bodies are the bead **description** (JSON), matching how `claude-settings`
stores `{"env":{…}}` today.

### 6.1 `daemon-settings` — operator-scoped daemon behavior

```text
// title: daemon-settings   labels: operator:U0OPERATOR1, project:gardenio
{
  "update_channel":       "stable",        // stable | latest-rc  (child 1 auto-update)
  "heartbeat_interval_s": 15,              // child 1 crash-recovery lease heartbeat
  "max_concurrent_spawns": 2,              // laptop resource bound
  "fleet_lane":           "local",         // how child-5 registers the session: local | cloud
  "offline_behavior":     "queue",         // queue | reject  (child 7 failure modes)
  "confirm_verdict_ui":   "touchid"        // child-6 `prompt` verdict relay: touchid | polkit | cli
}
```

Overlays a hardcoded `DefaultDaemonSettings()` baseline (the `DefaultUserSettings`
pattern), so an operator bead that sets only `{"update_channel":"latest-rc"}`
keeps every other default.

### 6.2 `daemon-permission-profile` — named spawn-authz profile (child-6 schema)

Named profiles: the title is `daemon-permission-profile` and the profile *name*
is a field, so an operator can keep several (`default-local`, `dry-run-only`).
The envelope references one by name (`permissions_profile: "default-local"`);
**the daemon resolves the actual grant from its own local copy** (child 6 §3 —
name is a selector, not a grant).

```text
// title: daemon-permission-profile   labels: operator:U0OPERATOR1
{
  "name": "default-local",
  "spawnable_sources": ["decision", "escalation"],   // which surfaces may spawn
  "allowed_projects":  ["gardenio", "monorepo"],
  "working_dir_roots": ["/Users/op/code"],           // dir MUST be under one (Clean+symlink-resolve first)
  "allowed_ref_kinds": ["head", "pr-head", "bead-ref"],
  "mode":              "dry-run",                     // dry-run | apply  (apply requires prompt verdict)
  "network_scope":     "default",                    // default | restricted | offline
  "filesystem_scope":  "workdir-only",               // workdir-only | home | unrestricted
  "on_apply":          "prompt"                       // allow | deny | prompt (Touch ID)
}
```

A tighter second profile the same operator can select per-click:

```text
// title: daemon-permission-profile   labels: operator:U0OPERATOR1
{
  "name": "dry-run-only",
  "spawnable_sources": ["decision"],
  "allowed_projects":  ["gardenio"],
  "working_dir_roots": ["/Users/op/code/gardenio"],
  "allowed_ref_kinds": ["head"],
  "mode":              "dry-run",
  "network_scope":     "restricted",
  "filesystem_scope":  "workdir-only",
  "on_apply":          "deny"
}
```

### 6.3 `daemon-spawn-defaults` — per-project working-dir / ref / worktree

```text
// title: daemon-spawn-defaults   labels: operator:U0OPERATOR1
{
  "projects": {
    "gardenio": {
      "default_working_dir": "/Users/op/code/gardenio",
      "default_ref":         "head",
      "worktree":            false
    },
    "monorepo": {
      "default_working_dir": "/Users/op/code/monorepo",
      "default_ref":         "pr-head",
      "worktree":            true          // big repo → isolate each spawn in a worktree
    }
  },
  "fallback": {
    "default_working_dir": "/Users/op/code",
    "default_ref":         "head",
    "worktree":            false
  }
}
```

The viewer reads this (via the controller's config resolver) to pre-fill the
envelope's `working_dir` / `ref` at click time (child 3 §envelope), so the
daemon receives a concrete path and only *validates* it against the permission
profile's `working_dir_roots`.

### 6.4 Reused `claude-settings` at the new `operator:` scope

No new category — just the existing `claude-settings` bead authored at
`operator:` so an operator's local sessions run at their preferred effort:

```jsonc
// title: claude-settings   labels: operator:U0OPERATOR1
{
  "env": { "CLAUDE_CODE_EFFORT_LEVEL": "xhigh" },
  "permissions": { "deny": ["Bash(rm -rf /*)"] },   // extra local guardrail
  "skipDangerousModePermissionPrompt": false        // adapted default for laptops (§5.2)
}
```

This merges *on top of* `DefaultUserSettings()` exactly as a role bead does today
(`MergeSettingsOntoDefaults`) — additive, sub-key by sub-key.

---

## 7. Daemon-local config file (`~/.gardenio/daemon.json`)

Single file, mode **0600**, owned and written by the daemon only. Never a bead,
never synced, never in the envelope. This is the machine-state / device-secret
side of the boundary (child 8 §4).

```jsonc
{
  "schema_version": 1,
  "daemon_id":      "dmn_7Fk3…",             // minted at pairing (child 2 Layer-2)
  "install_version":"2026.225.4",            // set by installer/auto-updater (child 1)
  "last_operator_sub": "U0OPERATOR1",        // machine state, changes at re-pair
  "pairing": {
    "cert_path": "~/.gardenio/daemon.crt",    // paths only; key never inlined
    "key_path":  "~/.gardenio/daemon.key",    // 0600, provisioned once (child 2)
    "controller_ca_path": "~/.gardenio/controller-ca.pem"
  },
  "recent_spawns": [                          // bounded ring (e.g. last 50); local idempotency + `gb daemon status`
    { "spawn_id": "spn_…", "decision_id": "kd-…", "nonce": "…",
      "started_unix": 0, "state": "done", "working_dir": "/Users/op/code/gardenio" }
  ]
}
```

Notes:
- `recent_spawns` is a **cache**, not the audit-of-record — the authoritative
  audit is the decision-bead comment/child written by the controller (child 3).
  The ring exists so the daemon can (a) reject a replayed nonce and (b) render a
  local status view without a round-trip.
- The private **key** is referenced by path, never embedded — mirrors the pod
  pattern of `SecretKeyRef` rather than inline secret values
  (`podmanager/manager.go`).
- `~/.gardenio/` is the same directory child 1 puts the UDS
  (`gb-daemon.sock`) and the LaunchAgent/systemd unit points at.

---

## 8. Resolution & precedence model (end to end)

How a single "spawn escalated agent" click resolves config, top to bottom:

1. **Operator clicks** in beads-viewer on a decision/escalation. The controller
   (child 3) builds the envelope. For any envelope field with a config default
   (`working_dir`, `ref`, `permissions_profile`), the controller resolves it via
   the **existing `claudeconfig` resolver** with subscriptions
   `["global", "project:<p>", "operator:<sub>"]`, reading `daemon-spawn-defaults`
   and the operator's default `daemon-permission-profile` name.
2. **Controller signs** the envelope (child 2 Layer-3 JWT), embeds the resolved
   `working_dir`, `ref`, and `permissions_profile` *name*.
3. **Daemon receives** the envelope over the paired channel (child 2 Layer-1).
   It **re-resolves** config from its own view of the beads (via the controller
   config API, or a cached pull) — it does **not** trust the envelope's resolved
   values as grants:
   - Resolves the named `daemon-permission-profile` (operator scope) → the
     *actual* grant.
   - Resolves `daemon-settings` (operator scope, over the daemon baseline).
   - Resolves `daemon-spawn-defaults` only to *validate* the envelope's
     `working_dir` is under an allowed root.
4. **Daemon evaluates** the permission profile against the envelope (child 6
   seven-axis check, `filepath.Clean` + symlink-resolve first).
5. **On allow**, the daemon writes the spawned session's `~/.claude/settings.json`
   by resolving the **reused** `claude-settings` / `claude-mcp` / `claude-hooks`
   at subscriptions `["global","project:<p>","operator:<sub>", "role:<r>"]` and
   merging onto `DefaultUserSettings()` — **the exact `MergeSettingsOntoDefaults`
   path the pod uses today**, minus k8s env injection.
6. **Daemon spawns** Claude Code in `working_dir` at `ref`, seeded with the bead
   context; records the spawn in `recent_spawns` and (via the controller) as a
   decision-bead comment.

Precedence within any category (from `resolveSortedLayers`, unchanged):
`global < project < operator < role < agent`, newer-wins tiebreak, superset
label match[^matcher]. **The daemon adds no new merge logic** — only the `operator:` scope
rank.

---

## 9. Open questions for phase-2 decision

These are genuine forks the config surface *surfaces* but does not own; the
phase-2 `gb decision` (or child-owners) should settle them:

1. **Daemon pull path for config beads.** Does the daemon read beads through the
   controller's config API over the paired channel (simplest, one auth surface),
   or get a signed config bundle pushed in the envelope (offline-capable but
   staleness risk)? §8 assumes the former; child 1/2 own the channel.
2. **Hook-script portability** (§5.2). Ship fleet hook scripts inside the local
   `gb` install for parity, or require operators to scope local hooks beads?
   Leaning: bundle a "local-parity" hook set in the install.
3. **`skipDangerousModePermissionPrompt` default on laptops** (§5.2). Recommend
   **false** by default for local spawns (real filesystem), overridable per
   operator. Confirm with child 8.
4. **Operator identity form for `operator:` labels.** OIDC `sub` vs the existing
   `slack:U…` / `op-<id>` forms already on beads (advice ka-0000000014). Recommend
   the OIDC `sub` (what child 2 pins at pairing) with a documented mapping to the
   Slack id; needs one canonical choice.
5. **Whether `daemon-permission-profile` should also be authorable at `global`**
   for a fleet-wide safety floor (a maximum blast radius no operator profile may
   exceed). Recommend **yes** — a `global` floor bead that the daemon intersects
   with the operator profile (deny-wins). Child 6 to confirm the intersection
   semantics.

---

## 10. Deliverable checklist (this child)

- [x] Knob → bead-vs-local mapping table (§3).
- [x] New-scope proposal with implementation seam (§4).
- [x] Reuse map against `claude-settings` — 1:1 / adapted / new (§5).
- [x] Example bead bodies for every new category + reused category at new scope (§6).
- [x] Daemon-local file schema (§7).
- [x] End-to-end resolution & precedence model (§8).
- [x] Open questions punted to phase-2 (§9).
- Rich-report widget instance + comment linking the MR: on the bead (ka-0000000166).

**Out of scope (per bead):** implementation. The single code seam this design
implies — one `operator:` case in `CategorizeScope` + one `GroupSortKey` rank —
lands in phase-3, not here.

---

[^matcher]: `kd`'s superset `labelsMatchConfig` and `claudeconfig`'s
    `MatchesSubscriptions` are **different matchers with different semantics**
    (superset OR-style vs. hard AND on `project:`/`agent:` + OR-gate on `role:`,
    `internal/advice/matching.go:216-233`) — this document describes the former.
    See `gardenio/docs/config-inventory.md`'s "Project scope contract" for the
    latter.
