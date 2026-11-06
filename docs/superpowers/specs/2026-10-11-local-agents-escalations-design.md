# Local agents as full participants, and escalations across the local/cluster boundary: design

Status: design for plan P4, written 2026-10-11 from the owner's answers to eight
decision beads (03:06 to 03:12Z) and from code on `main` as of that date.
Author: laptop-claude for the owner. Findings bundle: `gd-I9dFR2el5V` (docs 4
and 7). Companion plan: `docs/superpowers/plans/2026-10-11-local-agents-escalations.md`
(to be written after this spec is approved).

This document is the decisions and the design. The plan is the work. A crew
executing a plan task reads the task, then the sections of this document the
task cites. **§0 is binding.**

## 0. Decisions (binding)

| # | Decision | Source | Why |
|---|---|---|---|
| D1 | A local agent runs with **the machine owner's own Claude Code login** in milestone one. A broker-leased fleet profile is a later, operator-granted option chosen at registration. | gd-ZvoDrmlGKK | Pet-project onboarding needs no fleet account; the fleet never holds the owner's key and never pays for strangers. |
| D2 | **Agent-inbound delivery uses the kbeads SSE event stream**, for pods and laptops alike. NATS and a gRPC stream are not used for this. | gd-BrJSQpIenT | The stream exists, already carries escalations to laptops, and needs no new credential type beyond D3. |
| D3 | **A laptop runner reaches the daemon over a public HTTPS front door with a per-runner bearer token.** Ziti enrollment is not required for runners. | gd-kWnpgXN7uq | Lowest friction for a stranger. Makes a public API surface a prerequisite (§4). |
| D4 | **Placement on a runner is explicit:** a `placement` field or runner tag on the task, or a Slack mention by the runner's owner inside a thread that owner started. Never a capacity fallback. | gd-waj4GARshK | Surprise code execution on a personal machine is the failure mode to design out. |
| D5 | **A disconnected runner holds its task.** The task stays assigned, the thread shows the agent as paused, and nothing re-places it until the runner returns or a human kills it. | gd-cqMqDltmO7 | Laptops come back. A human can always release the task from Slack. |
| D6 | **A local agent has the capabilities of its human owner as a beads-viewer user.** It acts under the owner's operator identity. It is not a sandboxed role; the owner's permissions are its ceiling. | gd-LhcBPiZBlo (free text) | Owner: "local agents will have the capabilities they have; similar to that of a user in beads-viewer." |
| D7 | **Cloud to local ships first (M1). Local to cloud is a strongly supported M2.** A cluster agent escalates to a named human machine; that human approves; their local agent executes. A local agent that needs cluster work normally just spawns a cloud agent; local-to-cloud escalation becomes necessary once *restricted agents* exist (agents whose capabilities are narrower than their owner's), which is planned and researched separately. | gd-LhcBPiZBlo, gd-xAmbe36mmR, gd-FXAp73Uag6 | Owner: "the default direction of escalation is from cloud to local ... ultimately escalations will have to go in both directions." |
| D8 | **A runner declares a project allowlist at registration.** The daemon filters work and events to those projects. Per-runner escalation policy hangs off the same record. | gd-7ZILDLU1wd | Cheap, and it keeps fleet-internal work off a stranger's machine. |

## 1. What exists today

- `gb agent start --local` (`gardenio/controller/cmd/gb/agent_start.go`) starts coop on the host, creates an agent bead, writes `coop_url` into the bead's notes, registers in an on-disk registry (`internal/localreg`), heartbeats (`agentHeartbeatLoop`), and deregisters on exit. `--task` pre-assigns a bead. `--test-scenario` runs the claudeless mock.
- `--watch-escalations` (`agent_start_escalation.go`) subscribes to the kbeads SSE stream, picks approved escalations whose `execution_target` is `local`, stamps itself as `executor`, nudges the local coop session with the escalation prompt, and writes the result back. **This is the cloud-to-local direction, working, unscoped:** any watching local agent in the project may take any such escalation.
- Everything that reaches an agent dials it. `NudgeAgent` (`internal/bridge/nudge.go:160`) resolves `coop_url` from the agent bead's notes, tries the channel endpoint derived from it, then falls back to coop's `POST /api/v1/agent/nudge`. Kill (`bot_agent_kill.go:97`), decision delivery (`bot_decisions_modal.go:776,863`), claimed-task nudges (`claimed.go:170`), and advice nudges (`advice_nudge.go:165`) all go through the same `coop_url`. A laptop behind NAT has a `coop_url` nobody can dial.
- The kbeads SSE endpoint `GET /v1/events/stream` (`kbeads/internal/server/sse.go:336`) takes `topics` and `fleet` filters, replays from `Last-Event-ID`, applies a project and taint scope per client, and requires `RoleOperator`.
- Agent beads already carry `execution_target`, `runtime_id`, `runtime_state`, `runtime_account`, `runtime_region`, `runtime_started_at`, `runtime_suspended_at` (from the microvm runtime), plus `slack_thread_*`, `slack_card_*`, `operated_by`, `user_bead_id`, `spawn_source`, `escalation_id`.
- Escalation beads carry `execution_target` (`local | service_account | server`), `executor`, `requesting_agent`, `approved_by`, `approval_method` (`web_ui | api_token | cli | policy`), `policy_rule`, `sensitivity`, `requires_four_eyes`, `result`, `result_status`.
- The daemon is reachable only over OpenZiti today. The public front door for humans is its own epic (gd-akt1qKz1Si, BrowZer-based, open).
- The design sketch `gd-o4dPgzzf1G` chose a pull-based coop runner, explicit placement, and broker-mediated credentials. D1 and D3 override its credential and identity choices; its placement and pull-based transport stand.

## 2. Architecture

Four pieces, in dependency order.

1. **Mail as the inbox (§3).** The agent subscribes to its own mail over SSE and delivers it to coop on loopback; the bridge stops dialing. Pods and laptops use the same code. No new bead type.
2. **Runner identity and front door (§4).** A `type:runner` bead per machine, a per-runner bearer token minted from an invite, a public HTTPS listener exposing only the endpoints a runner needs, and a project allowlist on the runner.
3. **Local runner lifecycle (§5, §6, §7).** `gb runner` subcommands wrap today's `gb agent start --local`: register once, join as an agent many times. Capabilities are the owner's. Placement is explicit. Disconnect holds.
4. **Escalations across the boundary (§8).** Cloud to local: a cluster agent raises an escalation naming a runner; the runner's owner approves; the runner executes. Local to cloud: a local agent raises an escalation; an operator approves; a privileged cluster executor runs it. Both ride mail.

Slack is the only human surface for all of it (§9).

## 3. Mail is the inbox: the agent watches its own mail instead of being dialed

Owner, 2026-10-11, on an earlier draft that proposed a new `inbox` bead type: "I would have thought you could just send messages." Correct. Gardenio already has the message: `type=mail`, created by `gb mail send <recipient>` with `assignee=<agent>`, `title`, `description`, labels `from:<sender>` and optionally `delivery:interrupt`. The daemon publishes `beads.mail.sent` on every mail create (`kbeads/internal/server/beads.go:684`). Decision resolution already sends the requester a mail (`internal/bridge/decisions.go:352`). The bridge's mail watcher (`internal/bridge/mail.go:40-110`) sees the create on SSE and, for urgent mail, **dials the recipient's coop** via `NudgeAgentWithBootRetry`. That dial is the only thing that does not work for a laptop. Nothing new is needed except moving the subscription from the bridge to the agent.

### 3.1 What changes

- **The agent subscribes to its own mail.** A small loop beside coop (in `gb agent start`, both `--k8s` and local) subscribes to `GET /v1/events/stream?topics=beads.bead.created&agent=<agentBead>` (new `agent` scope, §3.3), and on a `mail` bead whose `assignee` is this agent delivers it to coop on loopback using the existing channel-then-PTY sequence. For non-urgent mail the delivery is the same one-line "New mail from X: title" nudge the bridge sends today; for `delivery:interrupt` mail the body is delivered inline. The mail bead is closed by `gb mail read` as now.
- **Control messages are mail with a label.** Kill becomes `gb mail send <agent> --label control:kill -s "Stop" -b "<reason>"`; the agent-side loop handles `control:kill` by asking coop to shut down gracefully (the same path `bot_agent_kill.go` uses after it reaches coop) and then exits. Decision resolution keeps sending the mail it already sends; the loop delivers it. Review events from forges are mail from `source_agent=github-bridge` or `gitlab-bridge` with the forge URL in the body. No new bead type, no new fields; one reserved label prefix, `control:`.
- **The bridge stops dialing.** `NudgeAgent` becomes "create a mail with `delivery:interrupt`" and returns; `nudgeCoop`, `deriveChannelURL`, and the mail watcher's `nudgeAgent` are deleted; `coop_url` stops being written into agent bead notes for new agents. Respawn-on-undeliverable keys off a mail that is still open past a runtime-specific deadline (pods: 2 minutes; local runners: never, per D5) instead of a dial error.

### 3.2 Why this is enough

Delivery state is the mail bead's status: open means not yet read, closed means read. Ordering is creation order. Replay after a disconnect is `Last-Event-ID` plus, on reconnect, `gb mail inbox` to drain anything unread. Expiry is not needed: an unread kill is still a kill when the agent comes back, and an unread nudge about a task the agent has since finished is harmless. The `mail_received` trigger mode in the scheduler (`internal/scheduler/nats_mail.go`) is unaffected.

### 3.3 Daemon changes

- New SSE scope `agent=<bead id>`: restricts the stream to events on that agent bead and on beads whose `assignee` is that agent's name. Enforced server-side in `sseClientScope`.
- Runner tokens (§4) carry role `RoleRunner`; `GET /v1/events/stream` accepts `RoleRunner` only with an `agent` scope naming an agent bead owned by that runner. `RoleOperator` behaviour is unchanged.
- `control:` labels on mail may be set only by the bridge, the controller, an operator, or the runner's owner; an agent setting one on mail to another agent is refused. This keeps kill from being something one crew can do to another.

### 3.4 Removing the dial

Once both runtimes subscribe, pods keep a loopback-only coop and the bridge has no path to any agent except mail. That is the hardening side effect and it is in scope.

## 4. Runner identity, registration, and the front door

### 4.1 Runner bead

`type:runner` (kind `config`), one per machine:

| Field | Meaning |
|---|---|
| `owner` | operator id (`oidc:<email>`), the human whose capabilities the runner carries (D6). |
| `hostname`, `os_arch`, `tags` | From registration. Tags are free strings used by placement (§7). |
| `projects` | json list, the allowlist (D8). |
| `token_id` | id of the current runner token record; the secret is never stored on the bead. |
| `credential_mode` | enum `owner_login \| broker` (D1; milestone one accepts only `owner_login`). |
| `escalation_policy` | json, per-runner rules (§8.4). |
| `last_seen_at`, `state` (`online \| away \| revoked`) | Maintained by heartbeat. |

### 4.2 Invite and token

Registration is invite-based so a stranger cannot register against a fleet uninvited:

1. In Slack, the fleet operator runs `/runner invite @person project-a project-b`. The bridge creates a single-use, 24-hour invite bead and DMs the person a code.
2. On the laptop, `gb runner register --invite <code>` posts the code plus hostname, OS, arch, and tags to `POST /v1/runners/register`. The daemon creates the runner bead with `owner` set to the invited operator and `projects` from the invite, mints a runner token (random 32 bytes, stored as a hash in a `runner_tokens` table with `runner_id`, `created_at`, `revoked_at`), and returns the token once.
3. `gb` stores the token in the OS keychain where available, else `~/.config/gardenio/runner.token` with mode 0600, and never prints it.
4. `/runner revoke <hostname>` sets `revoked_at` and `state=revoked`; every request with that token then 401s and the runner's active agents are killed via a control:kill mail.

The token is a bearer with role `RoleRunner`. The audit found that today's `requireRole` admits every non-OIDC caller to operator routes (`interceptors.go:143-147`), so `RoleRunner` must be introduced only after that interceptor distinguishes agent, runner, and operator roles; otherwise a runner token inherits operator powers on escalation result, reject, and PATCH. With that fixed, it may: register and heartbeat its own runner, create and update agent beads whose `runtime_id` is its runner, read and claim beads in its allowlisted projects, subscribe to its own agents' mail scope, write squawks, comments, escalations and decisions as the owner, read its owner on `GET /v1/me`, look up agents by name and semantic-search within its allowlisted projects, link and unlink beads when both ends are in them, and read configs. It may not: read other projects, list runners, write configs or read the sensitive config rows (tokens, bus handlers, allowlists), or approve, reject, resolve or cancel anything.

Config reads are a departure from the first draft, which said the runner may not touch config beads (gd-JmUt9B2iLb): `kd` reads the `type:*` registry and field configs on `GET /v1/configs*`, and an agent session that cannot is crippled enough to reach for the owner's login instead. Writes stay refused, and the sensitive rows stay an OIDC admin's to read (`config_key_authz.go`). Every write it makes is stamped `operated_by=<owner>` by the resolver, which is how D6 falls out of the existing human-attribution model rather than a new permission system.

### 4.3 Front door

A public HTTPS listener in front of the daemon that exposes only:

```
POST /v1/runners/register
POST /v1/runners/{id}/heartbeat
GET  /v1/events/stream            (RoleRunner, agent scope only)
GET|POST|PATCH /v1/beads/...      (RoleRunner, project-filtered; escalations and decisions are created here)
POST|DELETE /v1/beads/{id}/dependencies (both ends in the allowlist)
POST /v1/escalations/{id}/result
GET  /v1/configs, /v1/configs/{key...} (read only, no sensitive rows)
GET  /v1/me                       (resolves to the owner)
GET  /v1/agents-by-name/{name}, POST /v1/search/semantic (project-filtered)
```

The exact list is `runnerAllowedPatterns` in `kbeads/internal/server/runner_auth.go`; the Traefik route and the nginx edge mirror it, and their chart tests fail on any drift.

Everything else returns 404 on the public listener. Implementation: a second Traefik IngressRoute on a public hostname with a path allowlist middleware and rate limiting, pointing at the same daemon Service. TLS from cert-manager. The hostname is a chart value; the default is unset and the listener is off. This is a smaller and separate thing from the BrowZer front door for humans (gd-akt1qKz1Si) and does not wait for it.

Bearer tokens on the public listener are only ever runner tokens; the shared `kd-beads-token` is rejected there by a middleware that checks the token prefix (runner tokens start with `gr_`). This matters because of gd-opMHKHiy9I.

## 5. Local runner lifecycle

`gb runner` is a new command group; `gb agent start --local` keeps working as the low-level form and is what `join` calls.

- `gb runner register --invite <code> [--tags ...]` (§4.2), once per machine.
- `gb runner join [--project p] [--dir .] [--name n]` starts coop on the host with the owner's Claude login (D1: `claude` as installed; `--command` override stays), creates an agent bead with `execution_target=local`, `runtime_id=<runner bead id>`, `operated_by=<owner>`, `project` from the flag or the directory's registered project, and `role=local`. It starts the mail subscriber and the heartbeat. It opens or reuses the owner's Slack thread for this agent (§9) and posts the agent card. It is attached; Ctrl-C leaves cleanly.
- `gb runner status` prints the runner bead, token state, and active agents from the local registry and the daemon.
- `gb runner leave` kills active local agents gracefully and marks the runner `away`.

Heartbeat: every 30 seconds to `POST /v1/runners/{id}/heartbeat` and the existing agent heartbeat. The stale reaper (`internal/statusreporter/stalereaper.go`) treats an agent whose `execution_target=local` and whose runner is `away` as **paused, not dead** (D5): it sets `agent_state=paused`, leaves `task_id` assigned, and posts one thread message. When heartbeats resume the state returns to `working` and the subscriber drains unread mail. A human releases a held task with the thread's kill reaction or `/kill`, which closes the agent bead and returns the task to ready.

## 6. Capabilities and trust (D6)

- The `local` role has no capability list of its own. Authorization is the owner's: the runner token resolves to the owner operator, and every daemon check that already gates humans in beads-viewer applies unchanged. If the owner can see a project in the viewer, their local agent can claim from it (within the allowlist); if the owner cannot, neither can the agent.
- Git identity is the owner's. Pushes and PRs carry the owner's name, as the owner's own `claude` session would. The agent card and thread header say "local, running as <owner> on <hostname>" so reviewers know a human's machine produced the work.
- Model credentials are the owner's Claude login (D1). The fleet records token counts from coop as for pods but makes no cost claims; `total_cost_usd` is left unset and the roster shows "owner-billed".
- Cluster-side things a local agent cannot do (read a Secret, spawn a privileged role, run `kubectl`) are exactly the things the owner cannot do from the viewer. Those go local to cloud through an escalation (§8.3).

## 7. Placement (D4)

A task lands on a runner only when one of these is true:

1. The task bead has `placement=local` and either `runner_tag=<tag>` matching a runner's tags or `runner_id=<runner bead id>`.
2. The Slack mention that spawns the agent is posted by the runner's owner inside a thread that owner opened. The bridge resolves the Slack user to an operator, finds that operator's online runners, and if exactly one is online uses it; if several, it asks in-thread which; if none, it says so and falls back to the cluster.
3. A `/spawn ... --runner <hostname>` flag.

Pool pressure, cluster capacity, and taints never choose a runner. `gb ready` on a runner shows only tasks that satisfy (1) for that runner or are unplaced tasks in allowlisted projects the owner started from their own thread (2).

## 8. Escalations across the boundary (D7)

### 8.1 Shared mechanics, corrected by the field audit (bundle doc 11)

Both directions use the existing escalation bead plus two new fields: `target_runner` (runner bead id, required when `execution_target=local`) and `requesting_runner` (for local to cloud). The audit found that the escalation lifecycle as implemented today cannot carry this design, so the following are **preconditions**, owned by plan P7's escalation-authz phase, not optional hardening:

- **Approval, result, and reject are server-stamped.** `approved_by`, `approved_at`, `approval_method`, `rejected_by`, `executor`, and `result_status` are set by the daemon from the authenticated identity and the verb used, never merged from a client body. The result handler's `fields` passthrough is removed. The PATCH carve-out that lets any static-token caller update an open escalation is closed for these fields and for `operation_spec`, `target_bead_id`, `execution_target`, `target_runner` after filing (extend the `requesting_agent` immutability pattern).
- **Requester is never resolver.** The daemon refuses result and reject from the requesting agent, the requesting runner, or an agent spawned from this escalation.
- **One executor, one claim.** Exactly one party executes an approved escalation: the server replay for its four operation types, otherwise the party named by `execution_target` and `target_runner`. The bridge's "execute whatever has a non-empty `approved_by`" trigger is deleted; execution is driven by the daemon emitting the `control:escalation-approved` mail from inside the approve handlers, after the claim is taken. No fallback from local to spawning a cluster crew.
- **`execution_target=local` means this runner.** The mail goes to the agent on `target_runner` only; the watcher filters on its own runner id, not the project. An escalation with `execution_target=local` and no `target_runner` is rejected at create.
- **Four-eyes on every verb.** `requires_four_eyes` is enforced on result and reject as well as approve, and is set by policy for `sensitivity` of `sensitive` or `secret`, not left to the author.
- **Replay-unfit operations are excluded everywhere** (decision gd-UQTnDZATTU): `terraform_apply`, live account push-sync, unpinned push are refused at create for every executor, not just the server path.
- **Mutation history.** Approve, result, and reject record a mutation, so "the result is recorded against the request" is true.
- **`service_account` means a service account.** Cluster execution runs under the privileged identity the spawn-escalation epic defines (dedicated ServiceAccount per privileged role), never a plain `crew`.

Delivery to the executor is a `control:escalation-approved` mail naming the escalation, emitted by the daemon, which replaces the watcher's own SSE filtering and the bridge's `approved_by` trigger.

### 8.2 Cloud to local (default direction)

1. A cluster agent needs something only a human's machine or identity can do: a browser session, local hardware, a private checkout, a command the owner would run by hand. It creates an escalation with `execution_target=local`, `target_runner=<runner>` (or `target_owner=<operator>` to let the bridge pick that owner's online runner), `operation_spec`, `reason`, `sensitivity`.
2. The bridge posts the request card (§9) **in the runner owner's DM or thread**, not a general channel. Only the runner's owner (or a fleet operator with four-eyes if `requires_four_eyes`) can approve; anyone else's click is refused with a message saying whose machine it is.
3. On approval the daemon stamps the approver, takes the execution claim, and sends a `control:escalation-approved` mail to the active local agent on `target_runner` (or leaves it unread for `gb runner join` to drain). Nothing else may execute it. The local agent executes the operation spec, writes `result`, `result_status`, `executor`, and the card collapses to the outcome.
4. The requesting cluster task, if it opted into `close-on-escalation-execute`, resumes.

### 8.3 Local to cloud

1. A local agent hits a wall the owner could not cross either. It creates an escalation with `execution_target=service_account` (or `server`), `requesting_runner=<runner>`, `spawn_role` if a privileged spawn is needed.
2. The card goes to the fleet operators' channel. Approval is by an operator, with policy rules (§8.4) able to auto-approve named operation types.
3. A privileged executor on the cluster runs it, exactly as the spawn-escalation epic gd-qbBJGHiGHo defines. The result returns to the local agent as mail and to its thread.

### 8.4 Policy

`escalation_policy` on the runner bead is a list of rules `{direction, operation_type, projects, decision: auto_approve | ask | deny}`. Defaults for a new runner: cloud to local is always `ask` the owner; local to cloud is `ask` operators. Policy-approved results are stamped `approval_method=policy` and `policy_rule=<rule id>`, and the card says "approved by policy" in place of a name. Fleet-wide policy for cloud executors remains in gd-qbBJGHiGHo.

## 9. Slack surface

- **Runner card** on `/runner status` or the first `join`: hostname, owner, tags, projects, state, active agents.
- **Agent card** for a local agent is the normal card plus a "local" badge and "running as <owner> on <hostname>"; roster rows show the same.
- **Escalation request card**: operation type, one-line spec summary, sensitivity, where it will run (hostname and owner for cloud to local; "cluster, role X" for local to cloud), buttons Approve, Reject, and Needs four eyes. Only the eligible approver's click works. After resolution the card collapses to the outcome, who, when, and `approval_method`.
- **Paused message** when a runner goes away (D5): "<agent> is paused: <hostname> is offline. Work is held. React with the kill emoji to release it."
- Decision cards and nudges in a local agent's thread work as they do for pods, because delivery is by mail.

## 10. Error handling

- Mail undeliverable to coop (503 NotReady, loopback refused): retry 3 times at 2-second spacing as the watcher does today, leave the mail open, post one thread message. Pods then follow the existing respawn path when urgent mail stays open past 2 minutes; local agents wait for a human (D5).
- Runner token rejected (401): `gb runner join` stops with "token revoked or expired; ask for a new invite". No silent retry.
- SSE disconnect: reconnect with backoff capped at 60 seconds and `Last-Event-ID`; on reconnect, `gb mail inbox` and drain unread mail.
- Escalation targeted at an offline runner: card is posted, approval is allowed, the mail waits unread until `join`, and the requester's thread says the target is offline.
- Two online runners for one owner and an owner-thread mention: ask in-thread, do not guess.
- Public listener abuse: rate limit per token and per IP at the ingress; registration endpoint limited to 10 per hour per IP; invalid invite codes return a constant-time 404.

## 11. Testing

- Unit: mail replaces each dial site (table test over `NudgeAgent`, kill, decision delivery, claimed, advice); SSE `agent` scope rejects other agents' events; runner token role matrix (allowed and denied endpoints); placement resolver cases (field, tag, owner-thread with 0/1/2 runners, flag); policy evaluation; stale reaper pauses local agents instead of reaping.
- e2e with the claudeless mock, in the existing `gardenio/tests/e2e` harness: (a) pod agent receives nudge, decision, kill via mail with the dial code deleted; (b) `gb runner register` with an invite, `join`, claim, squawk appears in the thread; (c) cloud-to-local escalation end to end with approval by the owner, including refusal of a non-owner click; (d) local-to-cloud escalation end to end; (e) disconnect: kill heartbeats, agent shows paused, resume, unread mail drains; (f) public listener exposes only the allowlisted paths and rejects the shared token.
- A planted positive control for every "cannot" test: a request that must fail is paired with the same request from an allowed identity that must succeed, per the house rule on guards that cannot fail.

## 12. Milestones

- **M1:** §3 mail subscription for pods and laptops with the dial removed; §4 runner bead, invite, token, public listener; §5 `gb runner register|join|status|leave` with hold-on-disconnect; §6 as stated; §7 placement; §9 agent and runner cards and paused message; §8.2 cloud-to-local escalation end to end with its card. Local agents that need cluster work in M1 spawn a cloud agent (`gb spawn`, which the owner's identity already permits).
- **M2 (strongly supported, not optional):** §8.3 local-to-cloud escalation, which is what makes *restricted agents* usable; per-runner policy (§8.4); S3 session resume for local agents; broker credential mode (D1, operator-granted).
- Not in either: Windows runners (WSL counts as Linux), multiple owners per runner, runner-to-runner escalation.

## 13. Non-goals

- A role-to-role privilege lattice or any new permission system. D6 reuses the viewer's human authorization.
- Replacing the BrowZer front door for humans. §4.3 is an API listener for runners only.
- Capacity scheduling onto laptops. Placement is explicit by D4.
- Billing or cost attribution for owner-login runners. The fleet records tokens, not dollars.

## 14. Open items

- The escalation preconditions in §8.1 are P7 work and gate P4 M1's escalation beat; P4's mail and runner work (§3 to §7) does not wait on them.
- Restricted agents (capabilities narrower than the owner's) are the reason local-to-cloud exists; their state and readiness are researched in bundle doc 10 and, if warranted, a separate plan.
- Whether the public listener hostname shares the Pages or marketing domain (plan P3 custom-domain decision) or gets its own.
- Keychain support per OS for the runner token: macOS Keychain and Linux secret-service first; file fallback everywhere.
