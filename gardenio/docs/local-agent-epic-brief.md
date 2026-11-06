# Handoff brief: local-agent + security epic triad

Scoped 2026-04-22 by Matthew Baker in conversation with Claude Code. This brief exists so you don't have to re-research what's already been decided. **Trust the content here** unless you find hard evidence it's wrong, in which case open a `gb decision` rather than silently drifting.

## The three epics you can pick work from

| Epic | Title |
|---|---|
| ka-0000000165 | Exercise and harden local agent mode (claudeless e2e) |
| ka-0000003552 | Rogue-agent containment: capability scoping and undo |
| ka-0000005919 | Internet-safe daemon: auth, scopes, rate limits |

Use `kd children <epic-id>` to see open leaf tasks. Claim with `kd claim <id>` after `gb news` confirms nobody else is on it.

## Why these exist

- `gb agent start --local` is speculative code — wired up but unexercised. Rot risk. Epic A exercises it using the existing `claudeless` harness (scenario TOMLs in `gardenio/tests/e2e/claudeless/`, runner `test-claudeless.sh`).
- Writing the e2e tests forced us to name the security posture. That surfaced two distinct threat dimensions:
  - **External attacker** — the daemon at `beads.gardenio.e2e.example.com` must eventually be internet-safe (VPN is belt-and-suspenders, not the whole defense).
  - **Rogue agent** — a well-authenticated but confused agent can wreck fleet state today; authz is coarse RBAC only.
- Epic A inherits the current VPN posture. Distribution of local agents beyond the author's laptop is informationally gated on B + C — not a hard bead block, just don't advertise local-mode to teammates until B + C ship.

## Decisions already made — do not re-litigate

- **Collapse** the speculative `local_claude` execution_target enum. Zero references in code. `local_gardenio` becomes just "local" after the prune (task ka-0000000120).
- **Coop-wrapped** local mode is mandatory. Daily-driver shape is `gb agent start --local`. Plain-claude opting in is deferred (not this epic).
- **Target daemon** for local agents: `gardenio` (not `gastown-next`, which is stale).
- **Forward-compat** for multi-local-agent (identity disambiguator, `~/.gardenio/local/<name>.json` registry, `gb agent list --local`). Do NOT build multi-spawn UX in this epic.
- **Reuse claudeless** rather than build a new Claude stand-in. Task A2 wires `--scenario` into `gb agent start --local` by mirroring `agent_start_mock.go`.
- **Separate epics** for rogue-agent and internet-attacker. Their fixes overlap (scoped tokens, audit trail) but don't fully cover each other.

## Live posture — DO NOT re-grep, trust this snapshot (2026-04-22)

### External-attacker baseline (Epic C)
- Auth enforced on every endpoint except `/v1/health`
- Error responses generic — no stack traces, no query-string tokens
- IDs are nanoid (not enumerable)
- Request size (1–4 MiB) and timeouts (30s + 60s write) exist
- Tokens in env only; never argv
- TLS handled by K8s ingress (outside kbeads scope)

### Epic C punchlist
- **C1** `ka-0000006930` — per-identity scoped tokens with TTL + refresh-token flow (today: opaque, never expire)
- **C2** `ka-0000006767` (shared with Epic B) — enforce existing fleet-token scope fields `project`/`type_scope`/`op_scope`; declared in `kbeads/internal/server/http.go:498-506` but **never checked by handlers** — landmine
- **C3** `ka-0000007026` — persistent token revocation (in-memory only today, lost on restart)
- **C4** `ka-0000003784` — rate limiting on mutation endpoints (none today)
- **C5** `ka-0000005524` — lock down hook-execute (any `RoleOperator` can spawn K8s workloads)
- **C6** `ka-0000005594` — NATS subject scoping + separate NATS auth from daemon bearer

### Rogue-agent live state (Epic B)
Root cause: no ownership enforcement in handlers. `UpdateBead`/`CloseBead`/`DeleteBead` in `kbeads/internal/server/beads.go:303,:570,:599` pass through `authorizeWrite()` which checks only type-level `MinRole` + `ProjectScoped` from TypeConfig.

**Live `write_policy` table from `kbeads/internal/server/config.go` `builtinConfigs` map (authoritative; do not re-grep):**

| Category | Types | Implication |
|---|---|---|
| `min_role: gardenio-admin` (protected) | `type:account`, `type:project`, `type:fleet`, `type:user` | Already fine — operators cannot rotate credentials, flip projects, etc. Nothing to do for these. |
| `project_scoped: true, escalatable: true` | `type:agent`, `type:schedule` | Mostly fine; operators can only spawn in their own project, escalation path exists. |
| **No `write_policy` set (WIDE OPEN)** | `type:advice`, `type:trigger`, `type:formula`, `type:role`, `type:rig`, `type:convoy`, `type:config` (generic), `type:audit`, `type:report`, `type:jira-poller`, `type:decision`, `type:gate` | **These are Epic B targets.** Any operator can create global inline advice (poisons every agent's prime), code-execution triggers, mass-spawn formulas, fake audit rows, falsified reports. Fix in task **B6 ka-0000001888**. |

### Other sharp edges from B1 research (2026-04-22)
- `handleResolveDecision` at `kbeads/internal/server/http_decisions.go:128` accepts client-supplied `responded_by`. Any agent can resolve a human decision with falsified attribution. Fix in task **B4 ka-0000000687** (smallest, cleanest spec — good first task).
- `AllowedRoles`/`AllowedProjects` on trigger beads are advisory. `trigger_permissions.go:26-49` trusts them at fire time. Operator sets `allowed_roles: ["*"]` and `action.role: admin` → controller spawns admin-role agent. Fix in task **B5 ka-0000002944**.
- No audit trail on successful mutations; `audit_log.go:25` only logs denials. Hard-delete only; no soft-delete, no undo. Fix in **B8 ka-0000004651** (may split).

## Recommended claim order (start small, prove the flow)

### Tier 1 — tight spec, single handler
- **B4 ka-0000000687** decision `responded_by` — one file, one behavior change, one test. Start here if you want a clean first MR.
- **B6 ka-0000001888** write_policy hardening — pure data edits to `builtinConfigs`. Adds `write_policy` blocks to the 12 still-open types. Some per-type judgment (gardenio-admin vs project_scoped+escalatable).
- **A1 ka-0000000120** prune `local_claude` — grep, delete dead code, commit. Zero-risk cleanup.

### Tier 2 — targeted implementation
- **A2 ka-0000006370** `--scenario` flag on `gb agent start --local`. Mirror the pattern in `gardenio/controller/cmd/gb/agent_start_mock.go`. Unblocks A3–A8 but is standalone work.
- **C2 ka-0000006767** (shared Epic B + Epic C) — enforce fleet-token scope fields. Landmine task — fields are read into context but never compared against anything.

### Tier 3 — larger scope, per-task design calls
- **B3 ka-0000001016** ownership middleware — touches many handlers; design the interceptor shape before writing code.
- **B5 ka-0000002944** trigger fire-time authz — re-validate spawn scope in `TriggerEngine.fireAction` at `gardenio/controller/internal/bridge/triggers.go:345`.
- **B7 ka-0000001572** runaway protection — controller spawn rate cap + formula loop-cap + schedule cron-floor.
- **C1 ka-0000006930** per-identity tokens with TTL. Coordinate with C2.
- **C3–C6** persistent revocation, rate limiting, hook-execute lockdown, NATS scoping.

### Tier 4 — biggest, may split during implementation
- **B8 ka-0000004651** mutation history + soft-delete + undo. Three pieces in one bead; if you claim and find it too large, split into B8a/B8b/B8c and note in the bead.

### Epic A tasks with a laptop-validation gate
These can be done as code but final "does it feel right as a daily driver" is human territory:
**A3 ka-0000008139** scenarios, **A4 ka-0000002599** runner, **A5 ka-0000000772** coop integration, **A6 ka-0000007522** escalation, **A7 ka-0000004657** `--task` flag, **A8 ka-0000000375** crash robustness, **A9 ka-0000007089** multi-local forward-compat, **A10 ka-0000004098** CI + runbook.

## Key code jump points

- `kbeads/internal/server/beads.go:303,:570,:599` — Update/Close/Delete bead handlers (ownership hole)
- `kbeads/internal/server/http_decisions.go:128` — `responded_by` client-trust hole
- `kbeads/internal/server/config.go:19+` — `builtinConfigs` map; per-type `write_policy`
- `kbeads/internal/server/audit_log.go:25` — denial-only audit logger
- `kbeads/internal/server/http.go:498-506` — fleet-token scope field extraction (dead code path)
- `gardenio/controller/internal/bridge/trigger_permissions.go:26-49` — advisory `AllowedRoles` check
- `gardenio/controller/internal/bridge/triggers.go:345` — `TriggerEngine.fireAction`
- `gardenio/controller/cmd/gb/agent_start.go` — local agent spawn path
- `gardenio/controller/cmd/gb/agent_start_mock.go` — claudeless pod spawn (template for A2)
- `gardenio/tests/e2e/claudeless/` — scenario TOMLs (template for A3)
- `gardenio/tests/e2e/scripts/test-claudeless.sh` — runner pattern for A4

## Workflow & conventions

- `kd` for CRUD, `gb` for orchestration. Before any work: `gb news`, then `kd claim <id>`.
- Commit scope prefixes: `fix(kbeads): ...`, `fix(gardenio/controller): ...`, `fix(coop): ...`.
- `make preflight` before `git push` — builds, lints, tests via RWX in parallel.
- `quench check` in `kbeads/` and `gardenio/controller/` when you touch Go code there.
- `helm lint gardenio/helm/gardenio/` when you touch helm.
- Open MRs; never merge your own security change. `gb yield` if blocked on review.
- On merge, `kd close <task-id>` with a brief note.

## What NOT to do

- Do not spawn additional agents recursively. The schedule handles cadence.
- Do not modify advice beads unless you are working on B6 (write_policy hardening touches them structurally).
- Do not modify this brief. If a design decision here is wrong, open `gb decision` naming the specific point.
- Do not close tasks you did not complete. Bounce off? Leave a note, `gb yield`, exit cleanly.
- Do not attempt to fully validate Epic A items that require laptop hardware (coop-attach ergonomics, daily-driver UX). Cluster agents do code and tests; laptop humans validate final feel.
- Do not commit or push without `make preflight` passing.
- Do not unilaterally decide fleet-wide questions (e.g., "what MinRole for type:advice on a non-global bead?"). Open a decision.

## Escalation

If your claimed task needs a design call not covered in this brief, open `gb decision` with specific options and `gb yield` on it. Do not guess and ship.

## Status at time of handoff (2026-04-22)

- Epics A, B, C created and populated with children.
- B1 (research) closed. B2 (design) closed — absorbed into the 6 implementation children.
- No code work started yet. B4 is the recommended opener.
