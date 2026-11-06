# Per-Agent Tokens — Current Codebase State (T0)

**Status:** Research / findings.
**Tracks:** Epic [ka-0000004236](https://beads-viewer.gardenio.e2e.example.com/ka-0000004236) — per-agent tokens.
**Trigger:** Human decision on the epic asked us to "review the current state of the codebase before you do anything."
**Author:** thread-1777175210-249509 (initial doc) + mr-1535-ovpxwp (§7 addendum on the beads-token attribution path, after the original doc was flagged for "barking up the wrong tree" — the operator's actual concern is the beads-daemon token, not GitLab/GitHub) + mr-1557-5wxazb (§7.7 addendum after the operator pointed out that "today most beads show `created_by = static`" — the §7.2 happy-path only covers agent pods; bridges and the scheduler create the majority of beads and don't set X-Actor).
**Date:** 2026-04-26.

This document is a state-of-the-code reference. It exists so the T1–T5 ADRs
don't re-derive what's already in the tree. **No design choices are made
here.** Design lives in the follow-on ADRs, gated on human approval.

> **Re-framing note (2026-04-26):** The epic body and §1–§6 below read
> "per-agent tokens" as primarily *upstream* (GitLab/GitHub) attribution.
> The operator (matthewbaker) clarified that the load-bearing concern is
> the **beads-daemon token's effect on `created_by` for every bead an
> agent creates**. §7 covers that path end-to-end and surfaces the
> structural blocker that breaks `created_by` if the existing
> `PER_AGENT_TOKENS=true` rollout is enabled as-is. Read §7 first if you
> are scoping T1–T5.

## TL;DR

1. There is **already a per-agent token machinery** in the controller for the
   beads daemon token: `gardenio/controller/cmd/controller/agenttoken.go`
   (`provisionAgentToken` / `revokeAgentToken`), wired into the agent pod
   create/cleanup paths in `main.go` (lines 804, 835, 865, 870), gated by the
   `PER_AGENT_TOKENS` env flag (default `false`,
   `gardenio/controller/internal/config/config.go:552`). Any GitLab/GitHub
   per-agent-token work should follow this exact pattern, not invent a new
   one.
2. **Two separable attribution problems** are conflated in the epic body:
   - **Commit-author attribution** (git `user.name` / `user.email`) — set in
     `gb agent start` workspace setup. Fixable without any new token
     issuance, but commits will only show as "verified" upstream if the email
     belongs to a registered upstream user with a signing key.
   - **MR/PR/API-actor attribution** ("Created By" in GitLab/GitHub UIs) —
     determined by the upstream auth token. Fixing this *does* require
     per-agent tokens.
3. **The synthetic git email is role-based, not agent-based**:
   `cfg.role + "@gardenio.local"` (e.g. `gardener@gardenio.local`,
   `crew@gardenio.local`) — `gardenio/controller/cmd/gb/agent_k8s_workspace.go:33,57`.
   The epic body mistakenly stated `<agent>@gardenio`. This matters for sizing
   T9: changing the synthetic email to be agent-name-based is a one-line
   change with no token cost.
4. **Provider surface today** is exactly two families of upstream tokens
   plumbed through the init container: `GIT_TOKEN`/`GIT_USERNAME` (GitHub)
   and `GITLAB_TOKEN` (GitLab). Jira/etc. are *not* in init-clone — they are
   per-project secrets injected as env vars only. This narrows the v1
   research scope.

## 1. Where shared tokens enter the agent today

### 1.1 Source: project-bead `secrets[]` and role-bead `secrets[]`

`gardenio/controller/cmd/controller/podspec.go:585–624` (`applyCommonConfig`)
and `:638–691` (`applyRoleSecrets`) read `entry.Secrets` from the
`ProjectCache` / `RoleCache`. Each entry is `{Env, Secret, Key}` —
i.e. "set env var `Env` from K8s Secret `Secret`, key `Key`".

The controller enforces a name prefix:
- project secrets must start with `"{project}-"` (e.g. `gardenio-gitlab-token`)
- role secrets must start with `"{role}-"` (e.g. `crew-github-token`)

For three special env names, the controller *also* records the secret
reference on the pod spec so the **init-clone** container can read it
(`podspec.go:600–606, :673–679`):

| Env var | Pod field set |
|---|---|
| `GIT_TOKEN`, `GIT_USERNAME` | `spec.GitCredentialsSecret` |
| `GITLAB_TOKEN` | `spec.GitlabTokenSecret` |

Everything else (e.g. `JIRA_API_TOKEN`, `RWX_ACCESS_TOKEN`) is a plain
secret-env-var injection — visible to the agent process but **not** to the
init-clone container.

### 1.2 Init-clone consumption

`gardenio/controller/internal/podmanager/initclone.go:109–161`:

- If `spec.GitCredentialsSecret != ""` or `spec.GitlabTokenSecret != ""`, the
  init container is given `GIT_USERNAME`/`GIT_TOKEN` and/or `GITLAB_TOKEN`
  via `secretKeyRef`.
- The init script writes `~/.git-credentials` lines:
  - `https://${GIT_USERNAME}:${GIT_TOKEN}@github.com` (if GitHub creds set)
  - `https://oauth2:${GITLAB_TOKEN}@gitlab.com` (preferred for GitLab) or
    `https://${GIT_USERNAME}:${GIT_TOKEN}@gitlab.com` (fallback)

The clone happens with `git clone -b <branch> <gitURL>` — it inherits
credentials from the credential-helper file.

Note `initclone.go:71` writes `git config user.name <agent-name>` and
`git config user.email <agent-name>@gardenio`. **This is per-agent already,
in the init container's local clone**, but it is then overwritten by the
main container at agent start (next subsection).

### 1.3 Main agent container: `gb agent start`

`gardenio/controller/cmd/gb/agent_k8s_workspace.go:30–48`:

```go
authorName := envOr("GIT_AUTHOR_NAME", cfg.role)
runGitGlobal("config", "--global", "user.name", authorName)
runGitGlobal("config", "--global", "user.email", cfg.role+"@gardenio.local")
```

Critical observations:

- `cfg.role` is the **role** (e.g. `gardener`, `crew`), not the agent name.
- The synthetic email is `<role>@gardenio.local`, **not** `<agent>@gardenio`
  as the epic body claimed.
- An escape hatch already exists: `GIT_AUTHOR_NAME` env override. There is
  **no matching `GIT_AUTHOR_EMAIL` override** — the email is hard-coded.
- The init-clone's per-agent `user.name` / `user.email` (set in 1.2 above)
  is silently overridden by this code on every pod start.

`writeGitCredentials` (`agent_k8s_workspace.go:269–289`) parses
`BOAT_PROJECTS` for hostnames and writes `~/.git-credentials` with one line
per host using the same shared `GIT_USERNAME`/`GIT_TOKEN` (or
`GITLAB_TOKEN`). This is the production credential file the agent uses for
all `git push`, `glab`, `gh` commands inside the running pod.

## 2. Existing per-agent token pattern (beads daemon only)

`gardenio/controller/cmd/controller/agenttoken.go` (113 lines) implements:

- **`provisionAgentToken(ctx, logger, daemon, k8s, beadID, project, ns)`**
  1. Generate a 32-byte hex token via `crypto/rand`.
  2. `daemon.RegisterFleetToken(ctx, token, fleet, project)` — registers
     with the beads daemon as a project-scoped fleet token.
  3. Create K8s `Secret/agent-token-<beadID>` in the agent namespace, label
     `gardenio.io/type: agent-token`, key `token`, value `<token>`.
  4. Return the secret name; caller uses it as `spec.DaemonTokenSecret`.
  5. On secret-create failure, rolls back the fleet-token registration.

- **`revokeAgentToken(ctx, logger, daemon, k8s, beadID, ns)`** — best-effort
  cleanup: revoke fleet token, delete K8s Secret, log warnings on either
  failure.

Wired in `main.go`:

| Call site | Purpose |
|---|---|
| `main.go:804` | On agent bead create / pod creation, generate token |
| `main.go:835` | On `agent.done` bead transition, revoke |
| `main.go:865` | On agent pod delete (out-of-band cleanup), revoke |
| `main.go:870` | (Re-provision path on respawn) |

Gated by `cfg.PerAgentTokens` (`PER_AGENT_TOKENS=true`), default `false`
(`config.go:552`). When false, agents share the global
`BEADS_DAEMON_TOKEN`.

**Why this matters for the epic:** the same shape — generate, register
upstream, store as K8s Secret, plumb via `secretKeyRef`, revoke on
done/kill — is exactly what is needed for GitLab/GitHub. The T6/T7
implementation tasks should be framed as *generalising this pattern across
providers*, not building from scratch. T11 ("legacy fallback flag") is
already the established pattern (`PER_AGENT_TOKENS` flag).

## 3. Provider scope today

The only **upstream-API tokens** currently flowing through agent pods, by
function:

| Provider | Env var | Used for | Plumbed by |
|---|---|---|---|
| GitHub | `GIT_USERNAME`, `GIT_TOKEN` | git clone/push, `gh` CLI, GitHub API | init-clone + main container |
| GitLab | `GITLAB_TOKEN` | git clone/push, `glab` CLI, GitLab API | init-clone + main container |
| Beads daemon | `BEADS_DAEMON_TOKEN` | `kd` / `gb` against the daemon | main container only (already per-agent under flag) |

Other secret env vars seen in role/project beads (`RWX_ACCESS_TOKEN`,
`JIRA_API_TOKEN`, anthropic keys, etc.) are injected via
`SecretEnv` but never special-cased for git or attribution.

**The `@gardenio` mention triggers** in `gitlab-bridge` and `jira-bridge`
(referenced in the epic) are **inbound** — they parse comments addressed to
the bot. They do not affect outbound attribution and are out of scope for
this epic.

## 4. Issues with the epic body as written

The epic at ka-0000004236 is structurally fine but has these factual
errors / gaps that the T1–T5 research authors should know about:

1. **References "<agent>@gardenio" as the current synthetic email.** Actual
   value is `<role>@gardenio.local`. The fix in T9 should rename to
   `<agent-name>@<verified-domain>` (where `<verified-domain>` is whatever
   ADR-T1/T2 settles on for upstream user registration), not flip
   `@gardenio` → `@verified`.

2. **No reference to existing `agenttoken.go`.** T6 ("controller token
   issuer subsystem") should be re-scoped as *"generalise the existing
   per-agent-token issuer to support N provider types"*, with a small
   abstraction: `Issuer{Type: gitlab|github|beads, Provision(...) →
   secretName, Revoke(...)}`. Otherwise we duplicate code.

3. **No reference to `PER_AGENT_TOKENS` flag.** T11's "legacy fallback
   behind `agent.use_static_token` flag" should reuse / extend the existing
   `PER_AGENT_TOKENS` pattern rather than introduce a new one.

4. **Provider table is broader than reality.** Jira / "other API tokens"
   are listed as in-scope, but they don't currently affect attribution in
   ways operators see in upstream UIs in the same way GitLab MR authors do
   (Jira issue comments are richer; we'd need a separate sub-decision on
   what "Created By" means there). T3 should narrow to "v1 = GitLab + GitHub
   only; document Jira as deferred unless evidence emerges otherwise."

5. **T9 ("git config user.email") is much smaller than the others** and
   could ship without any token work. It's an easy partial win for
   commit-author attribution if (a) we accept that the commits will not be
   "Verified" upstream until the email belongs to a real upstream user, and
   (b) we accept that the *pusher* (and thus MR author) is still the
   shared service account. Suggest splitting T9 out as a standalone "Phase
   0" task that fixes commit `Author:` lines independently of the
   token-issuance work.

## 5. Open questions for T1–T5 authors (carry into ADRs)

These fall out of the codebase review; they are not decided here.

- Where does the per-agent token's *upstream user* come from? Three
  candidates surface from the existing pattern:
  1. **Pre-provisioned bot pool** — a fixed set of upstream users (e.g.
     `gardenio-bot-001..099`), recycled per-agent-spawn. Bounded sprawl, but
     attribution shows pool slot, not stable agent identity.
  2. **One upstream user per agent name** — stable attribution, but
     unbounded user creation in GitLab/GitHub admin (and on GitLab SaaS,
     each non-bot member counts toward seat billing).
  3. **One short-lived API credential per agent spawn against a shared
     bot user** — keeps "Created By" the bot user (no improvement) but
     each token is per-agent for audit. Doesn't solve the stated problem.
- Storage scaling: the existing pattern creates one K8s Secret per agent
  bead. With N agents per project and T tokens per agent (GitLab + GitHub
  + beads = 3), we get 3N Secrets per project. Fine for hundreds, do we
  care about thousands? (`gardenio-rwx`, `gardenio-prod` peak counts should
  be measured.)
- Revocation SLO: existing `revokeAgentToken` is best-effort and
  synchronous on bead-done. Is that acceptable, or do we need a janitor
  loop (similar to `reconciler/`) that finds-and-revokes orphaned tokens
  on a schedule?
- Migration: in-flight MRs/PRs authored by the static service account
  cannot be retroactively re-attributed. The cutover plan must list these
  and decide whether they continue to be edited by the static identity or
  re-opened by the per-agent identity.

## 6. Pointers (read these before authoring T1–T5)

| Topic | File:lines |
|---|---|
| Existing per-agent token issuer | `gardenio/controller/cmd/controller/agenttoken.go` |
| Issuer call sites | `gardenio/controller/cmd/controller/main.go:804,835,865,870` |
| `PER_AGENT_TOKENS` flag | `gardenio/controller/internal/config/config.go:189,552` |
| Project secret → init-clone wiring | `gardenio/controller/cmd/controller/podspec.go:585–624` |
| Role secret → init-clone wiring | `gardenio/controller/cmd/controller/podspec.go:638–691` |
| Init-clone token consumption | `gardenio/controller/internal/podmanager/initclone.go:109–161` |
| Init-clone synthetic git identity | `gardenio/controller/internal/podmanager/initclone.go:40–71` |
| Agent-start git config (overrides init-clone) | `gardenio/controller/cmd/gb/agent_k8s_workspace.go:30–48` |
| Agent-start credential file writer | `gardenio/controller/cmd/gb/agent_k8s_workspace.go:269–289` |

## 7. The beads-daemon token and `created_by` on agent-created beads

§1–§6 covered the upstream-attribution path (GitLab/GitHub MR/PR
`Created By`). The operator clarified that the actual concern of this
epic is **the beads-daemon token used by every agent**, and how it
determines `created_by` on every bead that agent creates. This section
walks that path end-to-end so T1–T5 don't re-derive it. **Today's
default (`PER_AGENT_TOKENS=false`) gets `created_by` right by accident —
flipping the flag without changes will break it.**

### 7.1 How `created_by` is set on a bead

Server-side, in `kbeads/internal/server/beads.go:82`:

```go
bead := &model.Bead{
    ...
    CreatedBy: actorFromContext(ctx),
    ...
}
```

`actorFromContext` returns the authenticated identity's Subject
(`kbeads/internal/server/audit.go:33–49`). Per its doc-comment, the
Subject resolves to one of:

- an **email**, for OIDC-token callers;
- the **fleet name**, for fleet-token callers (Type `"fleet"`);
- `"static"`, for the primary bearer token — *unless* the caller sent a
  valid `X-Actor` header, in which case the Subject is replaced by the
  X-Actor value;
- `""`, for anonymous / auth-disabled callers.

The X-Actor override is implemented in
`kbeads/internal/server/interceptors.go:247–255` (gRPC) and
`:525–530` (HTTP):

```go
// Honor X-Actor from primary-static-token callers so agent pods can
// self-attribute audit fields …
if id.Type == "static" {
    if vals := md.Get("x-actor"); len(vals) > 0 {
        if actor, ok := sanitizeActorHeader(vals[0]); ok {
            id.Subject = actor
        }
    }
}
```

The override is **gated on `id.Type == "static"`**. Fleet tokens never
honor it. (Tests asserting this: `xactor_test.go:158` —
`TestAddComment_XActorIgnoredForFleetToken`.)

The audit comment (AUDIT-2026-04-18 §C3) is explicit that this is
intentional: static-primary callers already have full write access, so
letting them self-attribute keeps audit truth without expanding trust.
Fleet-scoped tokens stay at fleet-name Subject for the same security
reason — the fleet-token's identity *is* the fleet.

### 7.2 How agents send their name today (`PER_AGENT_TOKENS=false`, default)

Path on the agent side:

1. `gb agent start` exports `KD_ACTOR=<agent-name>` into the agent's
   environment (`gardenio/controller/cmd/gb/agent_start.go:367,450`).
2. `kd` / `gb` CLI calls `defaultActor()`
   (`kbeads/cmd/kd/main.go:24–36`), which prefers `$KD_ACTOR`, falls
   back to `git config user.name`, then `"unknown"`.
3. The CLI calls `client.SetActor(actor)`
   (`kbeads/cmd/kd/main.go:88,100`), which causes every request to
   carry `X-Actor: <agent-name>` (HTTP:
   `kbeads/internal/client/http.go:543`; gRPC:
   `kbeads/internal/client/grpc.go:73`).
4. The agent's `BEADS_DAEMON_TOKEN` env is set from the **shared,
   project-wide static token** (today's default). The verifier marks
   the request as `id.Type = "static"`, X-Actor is honored,
   `id.Subject = <agent-name>`, and the bead's `CreatedBy` is the
   agent's name. ✓

This is why every bead **created from inside an agent pod's CLI**
shows the agent's name (e.g. `mr-1535-ovpxwp`,
`thread-1777138722-838139`) in `Created By` — not because there's a
per-agent token, but because the static token's holder is allowed to
self-attribute via `X-Actor`. **This works only as long as agents
continue to use the static primary token.**

⚠ **Important correction (2026-04-26, this section is the load-bearing
update for ka-0000003968):** The earlier wording "every bead in the
system today shows the agent's name" was wrong. **Most beads in the
system today show `created_by = "static"`**, not the agent's name —
because most beads are not created by agents at all. They are created
by infrastructure components (the controller's scheduler, the GitLab
mention bridge, the Slack/JIRA/push/wl bridges, the trigger engine)
that hold the same primary static token but **do not call
`SetActor()`**, so no `X-Actor` header is sent and the daemon falls
back to `id.Subject = "static"`. Concrete data and the full call-site
list are in §7.7 below.

### 7.3 What happens if `PER_AGENT_TOKENS=true` is enabled as-is

The existing per-agent token machinery
(`gardenio/controller/cmd/controller/agenttoken.go`) does this on every
agent spawn:

1. Generate a 32-byte random token.
2. Register it with the daemon as a fleet token, fleet name
   `agent-<beadID>` (`agentTokenFleetName`, line 30–32).
3. Create a K8s Secret named `agent-token-<beadID>`.
4. Plumb it into the pod as the agent's `BEADS_DAEMON_TOKEN`.

When the agent calls the daemon with this token:

- The verifier identifies the request as `id.Type = "fleet"` with
  `id.Subject = "agent-<beadID>"`.
- The X-Actor override is **skipped** (gated on `id.Type == "static"`).
- `actorFromContext` returns `"agent-<beadID>"`.
- The bead's `CreatedBy` is set to `"agent-<beadID>"` — the **fleet
  token's bead ID**, not the agent's name.

In other words, flipping `PER_AGENT_TOKENS=true` today regresses
`created_by` from the agent's name to a synthetic
`"agent-ka-XXXXXXXX"` string. Every operator-facing surface that reads
`Created By` (beads-viewer, `kd show`, advice-targeting,
ownership-based authz in `write_authz.go:191`) is affected.

### 7.4 Knock-on effects beyond `created_by`

The Subject also flows into:

- **Comment authorship.** `AddComment` sets `Author:
  actorFromContext(ctx)`
  (`kbeads/internal/server/server.go:354`,
  `http_relations.go:346`). All comments by an agent become
  `agent-<beadID>` instead of the agent name.
- **Close attribution.** `closedBy` and the analogous
  `respondedBy`/`canceledBy` for decisions
  (`kbeads/internal/server/beads.go:615`,
  `http_decisions.go:116,189`).
- **Event-log actor.** Every mutation records an event keyed by the
  Subject. Audit reconstruction breaks when the event actor is the
  fleet-token name.
- **Ownership-based write authz**
  (`kbeads/internal/server/write_authz.go:145–200`). The middleware
  permits writes when `bead.CreatedBy == actor`. If a bead was created
  by an earlier-spawn fleet (e.g. `agent-ka-A`), a later spawn
  (`agent-ka-B`) cannot edit it even though "they're the same agent
  identity from the operator's POV." Cross-spawn continuity is silently
  broken.
- **Advice-targeting.** Advice beads filter on `created_by` /
  assignee. Per-fleet identity scrambles those queries.

### 7.5 The structural design question this surfaces

Rolling out per-agent beads tokens is **not** a one-flag flip. The epic
must answer one of three structural choices, none of which is decided
here:

1. **Make the fleet identity *be* the agent name.** Change
   `agentTokenFleetName(beadID)` to use the agent name (or some stable
   per-agent slug) instead of the agent-bead ID. Pros: `created_by`
   stays the agent name end-to-end; cross-spawn continuity preserved;
   no X-Actor logic touched. Cons: requires defining a stable agent
   identity that survives respawns; complicates revocation (when do
   you revoke a fleet that may legitimately be reused?).

2. **Allow fleet tokens to honor X-Actor under bounded conditions.**
   E.g. a fleet token may set X-Actor *only* to a value derivable from
   the fleet name (a sub-fleet identity), so a fleet `agent-pool-001`
   can self-attribute as `mr-1535-ovpxwp` if the controller has bound
   that sub-identity at provision time. Pros: keeps fleet tokens'
   project scoping. Cons: meaningfully widens the audit-spoofing trust
   boundary that AUDIT-2026-04-18 §C3 deliberately closed. Needs a
   security-review sub-decision.

3. **Keep `created_by` derived from a separate signal, not the auth
   token.** E.g. add a "trusted agent identity" header that the
   controller-issued init container injects into the K8s Secret
   alongside the token, and have the daemon trust it only when the
   request carries a fleet token whose name matches an agent the
   controller knows about. Pros: cleanest separation of "who is
   allowed to write" from "who did write." Cons: requires controller
   ↔ daemon trust relationship that doesn't exist today; new auth
   surface.

This is the design fork the epic should be filing a `gb decision`
against.

### 7.6 Where this lives in code (pointers)

| Topic | File:lines |
|---|---|
| Bead `CreatedBy` set from auth | `kbeads/internal/server/beads.go:82` |
| `actorFromContext` semantics | `kbeads/internal/server/audit.go:33–49` |
| X-Actor override (gated on static) — gRPC | `kbeads/internal/server/interceptors.go:247–255` |
| X-Actor override (gated on static) — HTTP | `kbeads/internal/server/interceptors.go:525–530` |
| Fleet token registration / agent-token name | `gardenio/controller/cmd/controller/agenttoken.go:30–32, 50–96` |
| `KD_ACTOR` injection on pod spawn | `gardenio/controller/cmd/gb/agent_start.go:367, 450` |
| `defaultActor()` resolution in CLI | `kbeads/cmd/kd/main.go:24–36` |
| `SetActor` / X-Actor header on every request | `kbeads/internal/client/http.go:42, 543`; `kbeads/internal/client/grpc.go:59, 66, 73` |
| Comment author | `kbeads/internal/server/server.go:354`, `http_relations.go:346` |
| ClosedBy / responder / canceler | `kbeads/internal/server/beads.go:615`, `http_decisions.go:116, 189` |
| Ownership write-authz reads `CreatedBy` | `kbeads/internal/server/write_authz.go:145–200` |
| X-Actor ignored-for-fleet test (locks behavior) | `kbeads/internal/server/xactor_test.go:155–185` |

### 7.7 The "most beads show static" gap (ka-0000003968)

The operator's correction on MR !1557 — *"but today in fact most
beads show 'created by' as static!"* — is correct and identifies a
real gap §7.2 glossed over.

#### Empirical data

Snapshot taken 2026-04-26 against the production gardenio-beads HTTP
API (`kd list --limit=200 --json` → group by `created_by`):

| Subject of `created_by` | Count (of 200 most recent beads) | Share |
|---|---:|---:|
| `static` | 143 | ~72% |
| `thread-…` (Slack-thread agents) | ~30 | ~15% |
| `sched-…` (scheduled agents that comment from inside their own pod) | ~10 | ~5% |
| `mr-…`, `gardenio-…` (other agent pods) | ~17 | ~8% |

The bead this addendum is filed against (`ka-0000003968`) itself
shows `Created By: static`, as does the predecessor task
(`ka-0000002398`), every `Scheduled: …` task in the last day, and
every `GitLab mention on MR !N` task — i.e. exactly the bead families
that are visible to operators in beads-viewer's recent-list and Slack
notifications.

#### Why — the call-sites that hold the static token but never call `SetActor`

Every component below constructs a `beadsapi.Client` with the
project-wide static `BEADS_DAEMON_TOKEN` and does **not** call
`client.SetActor(...)` afterwards. Each of them creates beads or
appends comments / events on its own behalf:

| Component (binary) | beadsapi construction | What it creates | File pointer |
|---|---|---|---|
| `controller` (scheduler) | `gardenio/controller/cmd/controller/main.go:109` (single `daemon` shared with the scheduler at `:210`) | All `Scheduled: …` task beads via `scheduler.go:821` | `gardenio/controller/internal/scheduler/scheduler.go:821` |
| `gitlab-bridge` | `cmd/gitlab-bridge/main.go:54` | `GitLab mention on MR !N: …` task beads | `gardenio/controller/internal/bridge/gitlab_mentions.go:171` |
| `slack-bridge` | `cmd/slack-bridge/main.go:48` | thread-agent task beads, decision-respond comments, mail-routing comments |  |
| `push-bridge` | `cmd/push-bridge/main.go:72` | push-event beads / comments |  |
| `jira-bridge` | `cmd/jira-bridge/main.go:56` | JIRA-mirrored beads / comments |  |
| `wl-bridge` | `cmd/wl-bridge/main.go:75` | watch-list / waitlist beads |  |
| `trigger-engine` | `cmd/trigger-engine/main.go:54` | trigger-fired beads (when run as a separate binary instead of in-controller) |  |
| `roles-server` | `cmd/roles-server/main.go:34` | role-config writes |  |
| `bridge` decisions/escalations SSE relays | `internal/bridge/api_decisions_sse.go:57`, `internal/bridge/api_escalations_sse.go:56` | per-request short-lived clients in HTTP handlers |  |

Reference: every line is a `beadsapi.New(beadsapi.Config{HTTPAddr:
…, Token: os.Getenv("BEADS_DAEMON_TOKEN")})` immediately followed by
`if err != nil` — there is no `daemon.SetActor(...)` anywhere in the
controller tree. The only places that *do* set the actor are the agent
CLI binaries (`kbeads/cmd/kd/main.go:89,101` and
`gardenio/controller/cmd/gb/main.go:100`), which run inside the agent
pod, not in the bridges.

So the precise statement is: **agent pods self-attribute correctly;
controller-side and bridge-side processes do not, and they create
the majority of beads on the system.**

#### Why this rephrases — but does not invalidate — §7.3

The §7.3 finding (flipping `PER_AGENT_TOKENS=true` regresses
agent-created beads from `<agent-name>` to `agent-<beadID>`) is
unaffected: it only described the path inside agent pods, which is the
~28% slice that *is* attributed today. The fleet-token regression
still bites — it just bites a smaller surface than the doc implied.

But the gap surfaces a second, independent regression that the epic
must also plan for:

#### A second design fork the epic must answer

Even before any per-agent-token work, the **bridge / controller /
scheduler call sites should call `SetActor`** with a stable component
identity (e.g. `"gardenio-scheduler"`, `"gitlab-bridge"`,
`"slack-bridge"`, etc.). This is mechanically safe today because:

- All these components run with the **primary static token**, so
  `id.Type == "static"` and the X-Actor override is honored.
- `SetActor` already exists and is safe to call unconditionally — the
  doc-comment on `kbeads/internal/client/http.go:42` and
  `gardenio/controller/internal/beadsapi/client.go:123` says so
  explicitly: *"setting it unconditionally is safe."*
- It is a one-line change per binary (right after `beadsapi.New(…)`
  succeeds).

Open sub-decisions for that fix (defer to the epic, do not decide
here):

1. **Naming convention.** Per-binary (`gitlab-bridge`,
   `slack-bridge`, `controller`)? Per-subsystem (`scheduler`,
   `mention-bridge`, `decision-bridge`)? Mixed (`controller` for
   non-scheduler controller writes, `scheduler` for scheduled-task
   writes)? This is the user-visible label that will replace `static`
   on the affected beads.
2. **Should the scheduler also pass through the *triggering*
   identity?** A `Scheduled: …` task is fired by a `type=schedule`
   bead whose `created_by` is the schedule's owner. The scheduler
   could either (a) attribute the spawned task to itself
   (`scheduler`), or (b) attribute it to the schedule's owner. (b)
   matches operator expectation ("who set up this recurring job") but
   conflates "who scheduled it" with "who created the artifact." This
   is a small but real audit-semantics call.
3. **Backfill.** ~1095 existing beads carry `created_by = "static"`.
   Do we leave them as-is (safer; fits "you can't rewrite history"),
   or run a one-shot backfill that re-attributes by inspection of the
   bead title / labels (e.g. all `GitLab mention on MR !N: …` beads
   become `gitlab-bridge`)? AUDIT trail concerns argue for the former.

These are **distinct from** the per-agent-tokens work the epic is
already planning. They could ship independently, on a much shorter
timeline, with no security review, since the X-Actor path is already
trusted-for-static. The epic should file this as a separate `gb
decision` rather than rolling it into the per-agent-tokens plan.

#### Pointers added by §7.7

| Topic | File:lines |
|---|---|
| Empirical snapshot command | `kd list --limit=200 --json \| jq '.[].created_by' \| sort \| uniq -c \| sort -rn` |
| Scheduler creates `Scheduled: …` beads | `gardenio/controller/internal/scheduler/scheduler.go:821` |
| Mention bridge creates `GitLab mention on MR !N` beads | `gardenio/controller/internal/bridge/gitlab_mentions.go:171` |
| Bridge call-sites that need `SetActor` | `gardenio/controller/cmd/{gitlab-bridge,slack-bridge,push-bridge,jira-bridge,wl-bridge,trigger-engine,roles-server}/main.go` (one site per binary, immediately after `beadsapi.New`) |
| Controller call-site that needs `SetActor` | `gardenio/controller/cmd/controller/main.go:109` (used by both controller writes and scheduler) |
| `SetActor` doc-comment ("setting it unconditionally is safe") | `kbeads/internal/client/http.go:42–46`; `gardenio/controller/internal/beadsapi/client.go:123–129` |
