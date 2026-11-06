# Permissions model — `gb-daemon` (spawn-escalated-agent)

**Bead:** ka-0000004678 (research child 6 of 9)
**Epic:** ka-0000006791 — client-side `gb` daemon for "spawn escalated agent"
**Pairs with:** ka-0000000166 (child 4 — config-bead surface; permissions *live as*
config beads, this doc defines their *shape and evaluation*).
**Informs:** ka-0000006932 / ka-0000001720 (child 8 — security review; this doc
satisfies the security requirements M1/M2/M4 that review specifies).
**Consumes:** ka-0000005882 (authn/authz — the JWT/cert that proves *who* asks) and
ka-0000004798 (spawn-action-wire — the envelope that carries `permissions_profile`).
**Status:** research / recommendation only. No code lands until the epic's Phase-2
`gb decision` ratifies the daemon shape. This doc specifies the permission *schema*
and *evaluation flow* that decision must ratify.

---

## TL;DR

1. **The permission decision is not "may this operator act?" — that is authn/authz's
   job (ka-0000005882). It is "given a verified operator and a verified spawn
   envelope, may *this spawn* run *here*, *this way*?"** A spawn is arbitrary code
   execution on the operator's laptop (security-review §4), so the permission model's
   entire job is to *bound and audit* that execution.

2. **There is no need to invent a new evaluation engine.** gardenio already ships four
   permission-evaluation patterns. The daemon model is a **near-copy of the trigger
   spawn-authz pattern** (`trigger_permissions.go` / `spawn_authz.go`): a
   default-permissive-but-scopeable, **fail-closed**, multi-axis, two-phase check
   (advisory-at-author-time, authoritative-at-fire-time) whose scopes live on a
   config bead and are re-derived from an authoritative source at the moment of use.

3. **The permission surface is a `type=daemon-permission-profile` config bead** (the
   schema in §4), operator-scoped, referenced by name from the envelope
   (`permissions_profile: "default-local"`). The **name is a selector, not a grant**
   (security-review M4): the browser/envelope names a profile; the daemon resolves
   the *actual* grant from its own local, operator-provisioned copy and enforces
   that.

4. **The verdict is a tristate — `allow` / `deny` / `prompt`.** `allow`/`deny` reuse
   the `TriggerAuthResult{Allowed, Reason}` shape. `prompt` (defer-to-human) reuses
   the existing Slack/OS prompt-relay pattern (`bot_permission.go`) — but on the
   laptop it becomes a **local OS confirmation** (Touch ID / polkit), which is
   security-review's nice-to-have N1 promoted to a first-class verdict.

5. **Seven axes** are evaluated, in a fixed order, first-deny-wins for hard axes and
   accumulate-reasons for soft ones: `(project, repo, working-dir-root, ref-kind,
   apply-vs-dry-run, network-scope, filesystem-scope)`. `filepath.Clean` +
   symlink-resolve **before** the working-dir-root assertion (security-review M15) is
   load-bearing, not optional.

---

## 1. Scope

**In scope (this child owns):**

- The **permission schema** — the exact `type=daemon-permission-profile` config-bead
  shape, its fields, and their semantics (§4).
- The **evaluation flow** — the ordered, deterministic algorithm that turns
  `(verified operator, verified envelope, local profile)` into
  `allow`/`deny`/`prompt` with a reason (§5).
- **Example profiles** for common operator postures (read-only tourist, single-repo
  fixer, trusted power-operator) (§6).
- A **reuse map** onto the four existing evaluation patterns, so implementation is a
  fill-in-the-blanks against known-good code (§3, §8).

**Out of scope (owned elsewhere):**

- *Who* the operator is and *whether the request is genuine* — the cert/JWT/consent
  binding — ka-0000005882. This doc assumes a verified `operator` and a
  controller-signed envelope as **inputs**.
- The **wire** that carries `permissions_profile` — ka-0000004798.
- Which knobs are config-bead vs daemon-local, operator- vs fleet-scoped — ka-0000000166
  (this doc proposes the *permissions* knob's shape; child 4 places it in the wider
  config map).
- Threat modelling a compromised daemon — ka-0000006932. This doc *implements the
  requirements* that review set (M1/M2/M4/M15); it does not re-derive them.
- Implementation. No code lands before the Phase-2 synthesis `gb decision`.

---

## 2. Framing — what a "permission" decides here, and what it does not

The epic's brief lists the axes (per-action, per-repo, per-directory, dry-run vs
apply, network scope, filesystem scope, confirm-vs-auto). It is tempting to read
these as "a firewall for the daemon." They are not. The security review's central
reframe governs everything below:

> A daemon "spawn escalated agent" action is, by construction, arbitrary code
> execution on the operator's laptop — the default launch command *is*
> `claude --dangerously-skip-permissions` (`gardenio/controller/cmd/gb/agent_start.go:158`),
> and the resolved Claude settings can carry `hooks` (shell commands), `mcpServers`,
> and `permissions.allow: ["Bash(*)"]`
> (`gardenio/controller/internal/claudeconfig/settings.go:19-23`).

So the daemon permission model is **not** trying to sandbox what the spawned agent
*does once running* — Claude Code's own permission list (see §7) and the OS sandbox
(security-review M14) do that. The daemon permission model decides the **strictly
prior** question:

> **Should this spawn be launched *at all*, in *this* directory, from *this* repo/ref,
> in *apply* or *dry-run* mode — and if the answer is "maybe", ask the human at the
> keyboard.**

This is exactly the granularity of the existing **trigger spawn-authz** check
(`recheckSpawnAuthzAtFire`), which gates "may this creator spawn *this* (project,
role)?" *before* any agent pod starts. The daemon is the same check moved to the
laptop, with three extra axes (working-dir, ref-kind, apply-mode) that only make
sense on a real filesystem.

### 2.1 Two layers, don't conflate them

| Layer | Owner | Question | Mechanism |
|---|---|---|---|
| **Daemon permission profile** (this doc) | operator, local config bead | "May this spawn launch here, this way?" | §4 schema + §5 flow → `allow`/`deny`/`prompt` |
| **Claude Code permission list** (§7) | claude-settings config bead, controller-pinned | "Once running, which *tools* may the agent call?" | `permissions.allow`/`deny` in `settings.json` |

The daemon profile is the **outer gate** (spawn / don't spawn). The Claude list is the
**inner gate** (tool calls within a spawned session). The security review's M2 ("pin
the `claude_settings_bead` origin") is what stops the inner gate from being
attacker-supplied; this doc's M4-compliance is what stops the outer gate from being
browser-supplied. They compose; neither replaces the other.

---

## 3. Prior art in-repo — the four evaluation patterns (and which to copy)

gardenio already contains four distinct permission-evaluation patterns. **None share
a verdict type**; picking one is a design decision this model makes explicitly.

| # | Pattern | Verdict type | Axes | Fail-mode | Anchor |
|---|---|---|---|---|---|
| 1 | **Slack tool-approval relay** — human-in-the-loop "prompt", not an evaluator | `PermissionVerdict{Behavior string}` (`"allow"`/`"deny"`) | action-kind (tool), agent id, human approver | human decides | `gardenio/controller/internal/bridge/bot_permission.go:41,204` |
| 2 | **Decision-resolve owner check** — single identity axis | `decisionResolvePermission{Allowed, OwnerUserID}` | clicking user vs owning operator | **fail-open** (transient error / ownerless ⇒ allow) | `gardenio/controller/internal/bridge/bot_decisions_permission.go:12,36` |
| 3 | **Trigger spawn-authz** — multi-axis, two-phase, authoritative-source | `TriggerAuthResult{Allowed, Reason}` | creator identity, project, role, action-kind | **fail-closed** (no user bead / lookup error ⇒ deny) | `gardenio/controller/internal/bridge/trigger_permissions.go:14,92`; kbeads `internal/server/spawn_authz.go:23` |
| 4 | **MR security gate** — deterministic content/provenance gate, accumulate-all-reasons | `SecurityGateDecision{Pass, Reasons[]}` | scan-status, scanner-version-pin, review-tier | **fail-closed** (missing/garbled ⇒ fail) | `gardenio/controller/internal/bridge/security_gate.go:280,290` |

There is also one **existing local-execution path** worth naming, because it is the
nearest precedent for "an approved out-of-band action flows to the operator's machine":
`gb agent start --local --watch-escalations` (`gardenio/controller/cmd/gb/agent_start_escalation.go`).
It subscribes to the kbeads SSE bus, filters for an **approved** escalation with
`execution_target == "local"` and a non-empty `approved_by`
(`agent_start_escalation.go:241-256`), and then *nudges a live coop session* with the
escalation prompt — it does **not** spawn a fresh process, and it performs **no
permission evaluation** beyond the project-label match. The daemon's spawn action is the
generalization of this path: a real *spawn* (not a nudge) that inserts the permission
gate (§5) this watcher does not have. The escalation watcher's readiness-gate + nudge
mechanics are reusable for the launch step; its authorization posture (approval alone,
no local scope check) is exactly the gap this model fills.

Today there is **no runtime permission broker at all** for a launched agent: every spawn
runs `claude --dangerously-skip-permissions`, `~/.claude.json` sets
`bypassPermissionsModeAccepted=true` (`agent_k8s_workspace.go:412-415`), and the *only*
permission surface is the `permissions.allow`/`deny` list written into `settings.json`
*before* launch and evaluated by Claude Code itself (§7). So the daemon profile is not
replacing an existing broker — it is the **first spawn-time gate** the system has ever
had on the client side, and it must therefore be self-contained and fail-closed rather
than lean on a runtime enforcer that does not exist.

**Recommendation — copy #3 for the actor axes, borrow #4's `Reasons[]` shape, and
borrow #1 for the `prompt` verdict.** Concretely:

- The **structure** (default-permissive-but-scopeable, two-phase advisory→authoritative,
  fail-closed, `containsOrWildcard` list membership) is #3 verbatim. The daemon *is* a
  spawn-authz check; it should look like the one gardenio already trusts.
- The **verdict** widens #3's `{Allowed bool, Reason string}` to a tristate
  `{Verdict, Reasons[]}` where `Verdict ∈ {allow, deny, prompt}` and `Reasons` is #4's
  accumulate-all-reasons list (so a denial explains *every* failing axis, not just the
  first — better operator UX for a laptop dialog).
- The **`prompt` path** reuses #1's defer-to-human transport, retargeted from Slack to
  a local OS confirmation.

The two load-bearing disciplines from #3 carry forward unchanged and are **mandatory**
(they are the same properties security-review M1/M4 require):

1. **Authoritative-source, not envelope-supplied.** `trigger_permissions.go:71-91`
   spells out the B1 finding: the trigger bead's own `AllowedProjects`/`AllowedRoles`
   are *advisory client data and MUST NOT be trusted at fire time*; the real scopes are
   re-derived from the creator's user bead. The daemon obeys the identical rule: the
   envelope's `permissions_profile` is a **name**, and the grant is re-derived from the
   operator's **local** profile bead/file. (security-review M4.)
2. **Fail-closed.** `spawn_authz.go:56-64` denies on lookup error and on a missing user
   bead. The daemon denies on: absent profile, unresolvable profile name, or any axis
   evaluation error. (security-review M1: empty/absent profile ⇒ every spawn refused.)

---

## 4. Permission schema — `type=daemon-permission-profile`

A profile is an **operator-scoped config bead** (mirroring how `type=user` beads carry
`allowed_projects`/`allowed_roles` today). The daemon also keeps a **local pinned copy**
(`~/.config/gardenio/daemon/profiles/<name>.json`) so that a spawn can be evaluated even
if the daemon is offline from the controller, and — critically — so the grant the
daemon enforces is one the *operator* provisioned locally, never one handed to it in
the envelope (M4). The bead is the shareable/auditable source of record; the local file
is the enforced copy. Divergence between them is an alarm (audit, security-review M6).

### 4.1 Bead body (YAML shown; stored as JSON `fields`)

```yaml
type: daemon-permission-profile
kind: config
title: "daemon-permission-profile: default-local"
labels: [area:gb-daemon, project:gardenio]
fields:
  # --- identity of the profile ---
  profile_name:      default-local          # the selector named by the envelope
  operator:          slack:U0OPERATOR1      # OIDC-mapped; whose laptop this governs
  version:           1                      # bumped on edit; daemon logs which it enforced

  # --- Axis 1: per-action (which spawn kinds may run locally) ---
  # Matches the envelope's source: decision option vs raw escalation vs task.
  allowed_actions:   ["decision", "escalation"]   # "*" = any; e.g. deny raw "task" spawns

  # --- Axis 2: per-project ---
  allowed_projects:  ["gardenio", "kbeads"]  # containsOrWildcard; "*" = any project

  # --- Axis 3: per-repo ---
  allowed_repos:                            # git remote allow-list; matched canonicalized
    - "git@gitlab.com:acme/platform/gardenio.git"
    - "git@gitlab.com:acme/platform/kbeads.git"

  # --- Axis 4: per-directory (working-dir roots) ---
  # Spawn working_dir must resolve (Clean + symlink-resolve) UNDER one of these.
  allowed_dir_roots: ["/Users/op/code"]     # deny anything outside; never "/"
  denied_dir_roots:  ["/etc", "/var", "/Users/op/.ssh", "/Users/op/.aws"]  # belt-and-suspenders

  # --- Axis 5: ref-kind (which refs may be checked out) ---
  allowed_ref_kinds: ["branch", "mr-head"]  # subset of {branch, tag, sha, mr-head}
  # optional ref allow/deny globs (protect main):
  denied_refs:       ["main", "release/*"]  # never auto-spawn onto protected lines

  # --- Axis 6: apply vs dry-run ---
  default_mode:      dry-run                 # dry-run | apply
  apply_requires:    prompt                  # allow | deny | prompt — how "apply" is gated
  # (dry-run = read-only worktree, no push/apply; apply = full read-write)

  # --- Axis 7a: filesystem scope ---
  filesystem:
    mode:            worktree                # worktree | in-place
    #  worktree  = spawn into a throwaway `git worktree` under scratch_root (N6)
    #  in-place  = operator's primary checkout (higher blast radius; usually prompt)
    scratch_root:    "/Users/op/.gardenio/worktrees"
    readonly:        false                   # true = mount working-dir read-only (dry-run)

  # --- Axis 7b: network scope (of the spawned session, enforced via OS sandbox M14) ---
  network:
    egress:          allowlist               # deny | allowlist | any
    allow_hosts:     ["gitlab.com", "beads.gardenio.acme.example", "*.anthropic.com"]

  # --- verdict defaults & consent ---
  on_unmatched:      deny                    # what an axis with no rule does: deny | prompt
  confirm:           auto                    # auto | prompt-always | prompt-first-time-per-repo
  claude_settings_ref: pinned                # MUST be "pinned" (M2); "envelope" is forbidden
```

### 4.2 Field semantics (the ones that are not self-evident)

- **`allowed_actions`** — the "per-action allowlist" axis from the brief, keyed on the
  envelope's `source` kind (`decision` / `escalation` / raw `task`). Lets an operator
  say "I'll let decisions spawn locally, but never a raw task." `containsOrWildcard`.
- **`allowed_dir_roots` / `denied_dir_roots`** — the per-directory axis. The daemon
  **canonicalizes** the envelope's `working_dir` (`filepath.Clean` + `EvalSymlinks`)
  **first**, then asserts it is under an `allowed_dir_roots` entry and not under a
  `denied_dir_roots` entry. Order matters: canonicalize-then-check defeats `../` and
  symlink escapes (security-review M15). Deny wins over allow.
- **`apply_requires` + `default_mode`** — the dry-run/apply axis. A spawn envelope may
  request `apply`; `apply_requires: prompt` means "let it, but ask the human first."
  `default_mode` is what an envelope that names no mode gets. Dry-run maps to
  `filesystem.readonly: true` + no push + worktree.
- **`on_unmatched`** — the fail-mode *for a single axis that has no rule at all* (e.g.
  `allowed_repos` empty). `deny` = default-deny (recommended, security-review M1);
  `prompt` = ask. This is distinct from the *global* fail-closed behavior (absent
  profile ⇒ deny), which is not configurable.
- **`confirm`** — the confirm-vs-auto axis. `auto` = no human prompt if all axes
  `allow`. `prompt-always` = every spawn asks. `prompt-first-time-per-repo` = ask once
  per (repo, ref) then remember (bounded by a local, audited nonce table).
- **`claude_settings_ref: pinned`** — enforces security-review M2. The daemon consumes
  only the controller-signed, snapshotted Claude settings inside the envelope; it must
  **refuse** to dereference an arbitrary `claude_settings_bead` id. `envelope` is listed
  only to be explicitly forbidden.

### 4.3 Scoping — operator vs fleet

Following child 4's operator-vs-fleet split and the `type=user` precedent:

- A profile is **operator-scoped** by default (`operator` field). An operator may hold
  several (`default-local`, `prod-readonly`, `sandbox-anything`) and the envelope names
  which.
- A **fleet-scoped ceiling** is a separate, admin-written `type=daemon-permission-policy`
  bead (`min_role: gardenio-admin` write-policy, mirroring `config.go:389`) that the
  daemon intersects with the operator profile: **the effective grant is
  `operator_profile ∩ fleet_ceiling`**, never a union. This lets the fleet say "no
  daemon may ever spawn `apply` onto `acme-prod-v2`" and no operator profile can widen
  it. (This is the analog of how `authorizeAgentSpawn` layers admin bypass over
  per-user scopes — inverted to a *ceiling* rather than a bypass, because on a laptop
  the operator is not the admin.)

---

## 5. Evaluation flow — `allow` / `deny` / `prompt`

The daemon evaluates a spawn in a fixed, deterministic order. Inputs are all
*already verified* (ka-0000005882): a `VerifiedEnvelope` (controller signature ok, TTL
ok, JWT-consent ok) and the operator identity. Output is a `DaemonPermVerdict`.

```go
// Proposed shape — mirrors TriggerAuthResult, widened to tristate + Reasons[].
type PermVerdict string
const (
    PermAllow  PermVerdict = "allow"
    PermDeny   PermVerdict = "deny"
    PermPrompt PermVerdict = "prompt"
)
type DaemonPermVerdict struct {
    Verdict  PermVerdict
    Reasons  []string   // one line per axis that denied or requires prompt (SecurityGateDecision shape)
    Mode     string     // resolved "dry-run" | "apply" after gating
    Profile  string     // which profile (name@version) was enforced — for audit
}
```

### 5.1 Algorithm

```text
EvaluateSpawn(env VerifiedEnvelope, operator Identity) DaemonPermVerdict:

  # ---- GLOBAL FAIL-CLOSED PRECONDITIONS (not configurable) ----
  0. Load the LOCAL profile named env.spawn.permissions_profile for `operator`.
       - absent / unreadable / name mismatch  -> return DENY  ["no local profile '<name>'"]   (M1, M4)
       - profile.operator != operator          -> return DENY  ["profile not owned by caller"]
  0b. Load fleet ceiling policy (if any). effective = profile ∩ ceiling.
  0c. Assert env.spawn.claude_settings are controller-signed snapshot, not a bead id.
       - a bare claude_settings_bead id present -> return DENY  ["settings must be pinned"]     (M2)

  # ---- PER-AXIS EVALUATION (accumulate reasons; classify each as ok/deny/prompt) ----
  reasons = []
  hardDeny = false
  needPrompt = false

  1. action:   containsOrWildcard(effective.allowed_actions, env.source.kind)?
                 no -> if on_unmatched==deny: hardDeny, reasons+= "action '<kind>' not allowed"
                       else:                  needPrompt, reasons+= "action '<kind>' needs confirm"
  2. project:  containsOrWildcard(effective.allowed_projects, env.source.project)? (same shape)
  3. repo:     canonicalize(env.spawn.repo_url) ∈ effective.allowed_repos?         (same shape)
  4. dir:      root = EvalSymlinks(Clean(env.spawn.working_dir))                    (M15 — BEFORE the check)
                 root under a denied_dir_roots entry     -> hardDeny  ["dir under denied root"]
                 root NOT under any allowed_dir_roots     -> hardDeny  ["dir outside allowed roots"]
  5. ref-kind: env.spawn.ref_kind ∈ effective.allowed_ref_kinds?  AND
                 env.spawn.ref not matching denied_refs globs?     (deny on protected line)
  6. mode:     requested = env.spawn.mode or effective.default_mode
                 if requested==apply:
                   switch effective.apply_requires:
                     allow  -> Mode=apply
                     deny   -> hardDeny  ["apply not permitted by profile"]
                     prompt -> needPrompt, Mode=apply, reasons+= "apply requires confirm"
                 else Mode=dry-run
  7. fs/net:   resolve filesystem.mode/readonly + network scope into the sandbox spec (M14).
                 (These do not gate allow/deny by themselves; they parameterize the launch.
                  A profile that cannot satisfy its own net allowlist -> hardDeny.)

  # ---- COMBINE ----
  if hardDeny:                         return {DENY,   reasons, Mode, profile@ver}
  # confirm posture can force a prompt even when all axes passed:
  if effective.confirm != auto:        needPrompt = true; reasons += "profile requires confirm"
  if needPrompt:                       return {PROMPT, reasons, Mode, profile@ver}
  return                                      {ALLOW,  reasons, Mode, profile@ver}
```

### 5.2 What each verdict *does*

- **`allow`** → daemon proceeds to launch (into a worktree or in-place per `filesystem`,
  under the OS sandbox per `network`/`readonly`), writes `spawn.allowed` to the audit
  chain (security-review §6), and reports `spawning` on the callback.
- **`deny`** → daemon refuses, writes `spawn.denied` + `reasons` to the audit chain,
  and returns an operator-visible error to the callback (`error: "spawn denied: <reasons>"`).
  The `/decisions` button shows the reason chip (spawn-action-wire failure feedback).
  **No process is launched.**
- **`prompt`** → daemon raises a **local OS confirmation** — Touch ID / `polkit` /
  a tray dialog listing `(project, repo, working_dir, ref, mode)` and the `reasons`.
  This is security-review N1 promoted to a verdict. The human's click becomes the final
  `allow`/`deny`, is recorded in the audit chain (`spawn.prompted` + `spawn.confirmed`/
  `spawn.cancelled`), and — for `prompt-first-time-per-repo` — remembered in a local,
  bounded, audited nonce table. **The prompt is on the laptop, not in Slack** (the
  operator is already at the keyboard; a Slack round-trip would be strictly worse UX
  and a second attack surface). The Slack relay (`bot_permission.go`) is the *pattern*
  we borrow, not the *channel*.

### 5.3 Two-phase, like the trigger model

Mirroring `checkTriggerAuth` (author-time, advisory) vs `recheckSpawnAuthzAtFire`
(fire-time, authoritative):

- **Phase A — viewer pre-flight (advisory).** When the operator opens `/decisions/<id>`,
  beads-viewer *may* pre-check the profile it *thinks* applies, to grey out the "spawn
  locally" button or pre-fill the working-dir. This is advisory UX only.
- **Phase B — daemon at spawn (authoritative).** The daemon re-runs §5.1 against its
  **own local** profile the instant it receives the envelope, and that verdict is the
  only one that gates a launch. Phase A's opinion is never trusted (M4). This is the
  exact discipline `trigger_permissions.go:71-91` documents.

---

## 6. Example profiles — common operator postures

### 6.1 `read-only-tourist` — safest default for a first-time operator

```yaml
fields:
  profile_name: read-only-tourist
  allowed_actions:   ["decision", "escalation"]
  allowed_projects:  ["gardenio", "kbeads"]
  allowed_repos:     ["git@gitlab.com:acme/platform/gardenio.git"]
  allowed_dir_roots: ["/Users/op/code"]
  allowed_ref_kinds: ["branch", "mr-head", "sha"]
  default_mode:      dry-run
  apply_requires:    deny          # never apply — read-only always
  filesystem: { mode: worktree, scratch_root: "/Users/op/.gardenio/worktrees", readonly: true }
  network:    { egress: allowlist, allow_hosts: ["gitlab.com", "*.anthropic.com"] }
  on_unmatched: deny
  confirm:      prompt-first-time-per-repo
  claude_settings_ref: pinned
```

Effect: can reproduce/inspect anything in two repos in a throwaway read-only worktree;
can never push or apply; asks once per repo. Blast radius ≈ a disposable dir + read
access.

### 6.2 `single-repo-fixer` — the daily driver

```yaml
fields:
  profile_name: single-repo-fixer
  allowed_actions:   ["decision", "escalation"]
  allowed_projects:  ["gardenio"]
  allowed_repos:     ["git@gitlab.com:acme/platform/gardenio.git"]
  allowed_dir_roots: ["/Users/op/code/gardenio"]
  allowed_ref_kinds: ["branch", "mr-head"]
  denied_refs:       ["main", "release/*"]     # fix on feature branches, never main
  default_mode:      apply
  apply_requires:    prompt                     # apply is fine, but confirm each time
  filesystem: { mode: worktree, scratch_root: "/Users/op/.gardenio/worktrees", readonly: false }
  network:    { egress: allowlist, allow_hosts: ["gitlab.com", "beads.gardenio.acme.example", "*.anthropic.com"] }
  on_unmatched: deny
  confirm:      auto
  claude_settings_ref: pinned
```

Effect: full fix-and-push loop on one repo's feature branches, apply gated by a local
confirm, protected lines refused outright.

### 6.3 `trusted-power-operator` — widest, still fail-closed on the dangerous axes

```yaml
fields:
  profile_name: trusted-power-operator
  allowed_actions:   ["*"]
  allowed_projects:  ["*"]
  allowed_repos:     ["*"]                       # any repo the operator can clone
  allowed_dir_roots: ["/Users/op/code"]          # still bounded to a code root — NOT "/"
  denied_dir_roots:  ["/etc", "/var", "/Users/op/.ssh", "/Users/op/.aws", "/Users/op/.config/gardenio"]
  allowed_ref_kinds: ["branch", "mr-head", "tag", "sha"]
  denied_refs:       []
  default_mode:      apply
  apply_requires:    allow
  filesystem: { mode: in-place, readonly: false }   # primary checkout; higher blast radius
  network:    { egress: any }
  on_unmatched: prompt                            # unknown axis -> ask, don't silently deny
  confirm:      prompt-always                      # widest scope -> always confirm (N1)
  claude_settings_ref: pinned
```

Effect: near-unrestricted, but the two irreducible dangers — writing to config/secret
dirs and silent execution — are still bounded: secret dirs are denied even here, and
`prompt-always` keeps a human in the loop. **Even the most trusted profile cannot turn
off `claude_settings_ref: pinned` or reach `/` or `~/.aws`.** These are the non-negotiable
floors.

### 6.4 Fleet ceiling (admin-only) — the outer bound

```yaml
type: daemon-permission-policy         # write_policy: {min_role: gardenio-admin}
kind: config
fields:
  # Intersected with EVERY operator profile; operator can only ever be MORE restrictive.
  deny_projects:        ["acme-prod-v2"]         # customer prod — never spawn locally, full stop
  deny_apply_projects:  ["gardenio-prod", "legacy-prod"]  # inspect ok, apply never
  max_ref_kinds:        ["branch", "mr-head", "tag", "sha"]
  require_pinned_settings: true                  # cannot be waived by any operator profile
  require_worktree_for:    ["gardenio-prod", "legacy-prod"]  # prod always in a throwaway worktree
```

---

## 7. Comparison to Claude Code's own permission list

The brief asks explicitly for a comparison to "existing Claude permission lists / hook
configs." Here it is, because the mapping is the reuse argument.

### 7.1 What Claude's list is

`gardenio/controller/internal/claudeconfig/settings.go:16-58` defines the fleet baseline
`settings.json`:

```go
"permissions": map[string]any{
    "allow": []any{"Bash(*)", "Read(*)", "Write(*)", "Edit(*)",
                   "Glob(*)", "Grep(*)", "WebFetch(*)", "WebSearch(*)"},
    "deny":  InteractiveToolDenyList(),   // EnterPlanMode, ExitPlanMode, AskUserQuestion, ...
}
```

- It is a **tool-call allow/deny list**, keyed by `Tool(argpattern)` — e.g. `Bash(*)`,
  `Read(*)`. `deny` wins; the interactive-tool deny list turns "please don't" into a
  hard block (`settings.go:145-168`), and the crew role gets extra denies
  (`CrewToolDenyList` — `settings.go:160-169`).
- It is **merged** onto the baseline from a `claude_settings_bead`
  (`MergeSettingsOntoDefaults`, `settings.go:63`) — additive, sub-map by sub-map. A role
  bead adding `{"ultracode": true}` keeps the baseline permissions.
- It is applied **inside** a running Claude Code session, evaluated by Claude Code, not
  by gardenio.

### 7.2 How it differs from the daemon profile — and why we need both

| Dimension | Claude permission list | Daemon permission profile (this doc) |
|---|---|---|
| **Question** | "may this *tool call* run inside a live session?" | "may this *session* launch here, this way?" |
| **Granularity** | per tool + arg-pattern (`Bash(git push:*)`) | per (project, repo, dir, ref, mode, net, fs) |
| **Timing** | continuously, during the session | once, before the session |
| **Evaluator** | Claude Code | the daemon (§5) |
| **Verdict** | allow / deny (+ the Slack "prompt" relay) | allow / deny / prompt (local OS confirm) |
| **Trust anchor** | the pinned `claude_settings_bead` snapshot (M2) | the operator's local profile (M4) |
| **Blast-radius role** | inner gate (bounds *what the agent does*) | outer gate (bounds *whether/where it spawns*) |

They are **complementary layers, not substitutes** (§2.1). Two design lessons carry
across from Claude's list into the daemon profile:

1. **`deny` beats `allow`, and some denies are non-waivable.** Just as
   `InteractiveToolDenyList` is force-merged so no role bead can re-enable
   `AskUserQuestion`, the daemon's `denied_dir_roots` floor (`~/.ssh`, `~/.aws`,
   `~/.config/gardenio`) and `require_pinned_settings` are non-waivable even by
   `trusted-power-operator`.
2. **Additive merge, most-specific-wins.** Claude settings merge role-specific over
   baseline (`roleInlineSpecificity`, `config_resolve.go:34`). The daemon does the
   analog: `effective = operator_profile ∩ fleet_ceiling`, with the *ceiling* the more
   authoritative layer (intersection, not override, because it is a bound not a default).

### 7.3 Reuse the *representation*, not just the idea

Where a daemon profile axis maps cleanly to Claude's `Tool(argpattern)` shape, **use the
same string grammar** so an operator who already reads `settings.json` reads the profile
for free. Example: `denied_refs: ["main"]` can be surfaced to the spawned session as a
merged-in `Bash(git push origin main:*)` deny in the pinned settings snapshot — the outer
gate (don't spawn onto main) and the inner gate (don't `git push` to main) then agree,
defence-in-depth. (Child 4 owns the exact bead-field encoding; this doc only asserts the
grammar should be shared.)

---

## 8. Reuse map — implementation is fill-in-the-blanks

| Daemon component | Copy from | Anchor |
|---|---|---|
| Verdict struct (`{Verdict, Reasons[]}`) | `SecurityGateDecision{Pass, Reasons[]}` widened to tristate over `TriggerAuthResult{Allowed, Reason}` | `security_gate.go:280`, `trigger_permissions.go:14` |
| List-membership (`allowed_*` + `*` wildcard) | `containsOrWildcard` | `trigger_permissions.go:56` |
| Two-phase advisory→authoritative | `checkTriggerAuth` / `recheckSpawnAuthzAtFire` | `trigger_permissions.go:26,92` |
| Fail-closed lookup (absent profile ⇒ deny) | `authorizeAgentSpawn` / `lookupSpawnPermissions` | `spawn_authz.go:23,84` |
| Scope fields on a config bead (`allowed_projects`/`allowed_roles`) | `type=user` spawn scopes | `spawn_authz.go:84-108`; `user_provision.go:37` |
| Admin-min-role write policy for the fleet ceiling | `write_policy: {min_role: gardenio-admin}` | `kbeads/internal/server/config.go:389` |
| `prompt` (defer-to-human) transport | `bot_permission.go` relay, retargeted Slack→local OS | `bot_permission.go:41,191` |
| Path canonicalize-then-assert (M15) | *new* (`filepath.Clean` + `EvalSymlinks` before root check) | security-review M15 |
| Append-only, hash-chained audit of each verdict | security-review §6 audit spec | `security-review.md §6` |

**Net:** ~1 new evaluator file (`daemon_perm.go`, ≈ the size of `spawn_authz.go` +
`trigger_permissions.go` combined), 1 new config-bead type, 1 fleet-ceiling type, and
the OS-confirm shim for `prompt`. No new *evaluation paradigm* — this is the trigger
spawn-authz check with three filesystem axes and a tristate verdict.

---

## 9. Open questions for the Phase-2 synthesis `gb decision`

1. **`prompt` transport on Linux headless-ish laptops.** Touch ID / polkit is clean on
   macOS + desktop Linux; for a headless operator box, `prompt` may have to degrade to
   `deny` (fail-closed) or a Slack DM confirm. Pick a default: recommend
   **`prompt`-unavailable ⇒ deny**, with a Slack-confirm opt-in.
2. **Ceiling intersection vs override.** This doc recommends **intersection** (ceiling
   is a hard bound). Confirm the fleet never wants a ceiling that *grants* (it should
   not — grants belong to the operator profile).
3. **Per-repo remembered confirms — TTL and revocation.** `prompt-first-time-per-repo`
   needs a local nonce table; how long does a remembered confirm live, and does a
   profile edit (`version` bump) invalidate all remembered confirms? Recommend: **yes,
   version bump clears the table**; entries also expire at 30 days.
4. **Dry-run fidelity.** "dry-run" here = read-only worktree + no push. Is that enough,
   or does dry-run also need to intercept network writes (API POSTs, `kd create`)? If
   so, dry-run must lean on the network sandbox (M14), not just `readonly`.
5. **Profile provisioning UX.** Is the first profile written by `gb daemon init`
   (local file) and *then* mirrored to a bead, or bead-first? (Ties to child 9 install
   UX and child 4 config surface.) Recommend **local-file-first, bead-mirror-for-audit**,
   since the enforced copy must be local (M4).
6. **Does `apply` onto `gardenio-prod` ever get a local profile at all**, or is prod
   apply always a cloud-agent job? The fleet ceiling in §6.4 denies it by default;
   confirm that is the intended posture.

---

## 10. Linkbacks

- Epic: ka-0000006791
- Pairs: ka-0000000166 (config-bead surface)
- Consumes: ka-0000005882 (authn/authz), ka-0000004798 (spawn-action-wire)
- Informs: ka-0000006932 / ka-0000001720 (security review — M1/M2/M4/M15 satisfied here)
- This doc: `gardenio/docs/research/spawn-escalated-agent/permissions-model.md`

**Code anchors (for the synthesis MR, not this research MR):**
`gardenio/controller/internal/bridge/trigger_permissions.go`,
`kbeads/internal/server/spawn_authz.go`,
`gardenio/controller/internal/bridge/security_gate.go`,
`gardenio/controller/internal/bridge/bot_permission.go`,
`gardenio/controller/internal/claudeconfig/settings.go`,
`gardenio/controller/cmd/gb/config_resolve.go`,
`gardenio/controller/cmd/gb/agent_start.go:158`.
