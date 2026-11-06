# Privilege Escalation Pipeline

**Status:** Complete (2026-04-06) — Phases 1-3 delivered, E2E tests passing
**Epic:** ka-0000001184
**Author:** Agent crew (from operator voice-dictated brief)

## Problem Statement

Some infrastructure operations require elevated privileges that agents don't (and
shouldn't) have: creating AWS IAM roles, modifying K8s RBAC, rotating production
credentials, deploying Helm chart overrides, etc. Today these are handled ad-hoc
— an agent creates a decision bead asking a human to do the work manually. There
is no structured pipeline for:

1. An agent to **request** a privileged operation with a machine-parseable spec.
2. A human to **review and approve** the operation with real authentication (not
   just a Slack button click).
3. An **execution environment** (local laptop agent or system account) to carry
   out the approved operation.
4. An **audit trail** linking the request → approval → execution → result.

## Vision

A first-class "privilege escalation" workflow where:

- An agent needing elevated access creates an **escalation request** (a new bead
  type or a structured decision variant) specifying *what* needs to happen.
- The request is surfaced to authorized humans via Slack notification, but
  **resolved via a web UI** that requires real authentication (not anonymous Slack
  button clicks).
- On approval, the operation is executed by one of:
  - A **local Claude Code agent** running on the approver's laptop (sandbox
    mode, with the approved prompt injected).
  - A **local gardenio agent** (`gb agent start --local`) connected to the
    control plane via SSE.
  - A **system service account** with scoped credentials (future).
- The result is captured, linked to the original request, and the requesting
  agent is nudged to continue.

---

## Current System Landscape (Research Findings)

### Decisions (existing)

| Aspect | Current State |
|---|---|
| **Type schema** | `type:decision` — fields: `prompt`, `options` (JSON), `context`, `session`, `requested_by`, `requesting_agent_bead_id`, `chosen`, `rationale`, `response_text`, `responded_by`, `responded_at`, `required_artifact`, `artifact_status` |
| **Lifecycle** | Agent creates → Slack notification with buttons → human picks option → bead closed → agent nudged |
| **Escalation** | Label-based: adding `escalated` label triggers `NotifyEscalation` with dedup |
| **Web UI** | `bridge/web/index.html` — SSE-connected dashboard at `/decisions`. Lists pending decisions, keyboard shortcuts, resolve/dismiss actions. Calls `/api/decisions/{id}/resolve` |
| **SSE proxy** | `DecisionSSEProxy` at `/api/decisions/events` — filters kbeads event bus for decision-type beads |
| **Resolution API** | `POST /v1/decisions/{id}/resolve` — takes `selected_option`, `response_text`, `responded_by`. No authentication beyond fleet token |

**Gap:** Decisions are resolved with a `responded_by` string (free text). There
is no cryptographic authentication — anyone with access to the web UI or Slack
channel can resolve any decision. No distinction between "routine agent question"
and "privileged infrastructure operation."

### Users (new concept)

| Aspect | Current State |
|---|---|
| **Type schema** | `type:user` (kind: config) — fields: `slack_id`, `slack_channel_id`, `gitlab_username`, `github_username`, `email`, `display_name`, `team`, `jira_user`, `preferences` (JSON) |
| **UserDirectory** | `bridge/user_directory.go` — in-memory cache of user beads, indexed by slack_id, gitlab, github, email, jira. Auto-refreshes every 5 min |
| **Usage** | Used for cross-platform identity resolution (Slack → GitLab, etc.), DM delivery, agent spawn attribution. Used in `bot_mentions.go` to link `user_bead_id` on agents |
| **Auth model** | None. User beads are identity records, not auth principals. No roles, no permissions, no capability grants |

**Gap:** Users have identity but no authorization. No concept of "this user is
authorized to approve privilege escalations" or "this user has AWS admin access."
Need to add roles/capabilities to the user model.

### Permission System (existing)

| Aspect | Current State |
|---|---|
| **Permission callbacks** | `bot_permission.go` — gardenio-channel POSTs tool-approval requests to bridge, bridge posts Slack message with Allow/Deny buttons, verdict routed back to channel server |
| **Scope** | Per-tool-call permission on a single agent. Not designed for cross-agent escalation |
| **Auth** | Slack identity only — whoever clicks the button. No verification of authorization level |

**Gap:** Permission callbacks are per-tool-call and transient. They're the right
pattern for "should this agent run this Bash command" but not for "should we
create an AWS IAM role." Need a higher-level approval workflow.

### Gates (existing)

| Aspect | Current State |
|---|---|
| **Model** | `GateRow`: `{AgentBeadID, GateID, Status, SatisfiedAt}`. Simple pending/satisfied state |
| **API** | `POST /v1/agents/{id}/gates/{gate_id}/satisfy`, `DELETE .../gates/{gate_id}` |
| **Usage** | Stop gates, decision gates. Binary — gate is either open or closed |

**Gap:** Gates are boolean and agent-scoped. Could potentially be extended for
privilege gates, but would need richer metadata (who satisfied, what was
approved, audit trail).

### Local Agent Support (existing)

| Aspect | Current State |
|---|---|
| **`gb agent start --local`** | Creates agent bead with `execution_target:local` label, starts coop on localhost, registers with kbeads. Agent is visible in roster |
| **`gb agent start --docker`** | Builds/runs gardenio agent image in local Docker. Mounts $PWD |
| **Channel server** | `gardenio-channel.ts` on port 8789 — MCP-based nudge replacement. Bridge tries channel first, falls back to coop nudge |
| **SSE** | kbeads has a full SSE event bus (`sseHub`). Bridge already uses it. Local agents could subscribe directly |

**Gap:** Local agents exist but are "fire and forget" — they run whatever
command is given at startup. No mechanism to receive an approved prompt from
the escalation pipeline and execute it in a sandboxed context.

---

## Design Sketch

### New Bead Type: `type:escalation`

```
kind: data
fields:
  - operation_type    (string, required)  # e.g. "aws_iam", "k8s_rbac", "helm_deploy"
  - operation_spec    (json, required)    # machine-parseable operation definition
  - context           (string)            # human-readable background
  - requesting_agent  (string)            # agent bead ID
  - requesting_task   (string)            # task bead ID
  - approved_by       (string)            # user bead ID (not free text)
  - approved_at       (string)            # RFC3339 timestamp
  - approval_method   (enum)             # "web_ui", "api_token", "cli"
  - execution_target  (enum)             # "local_claude", "local_gardenio", "service_account"
  - executor          (string)            # who/what executed (agent bead ID or user bead ID)
  - result            (json)              # execution output
  - result_status     (enum)             # "success", "failure", "partial"
```

This is separate from `type:decision` because:
- Escalations need structured operation specs (not free-text prompts with options).
- Approval requires authentication (user bead ID, not `responded_by` string).
- Execution happens in a different environment than the requesting agent.
- Audit trail is mandatory, not optional.

### User Authorization Model

Extend `type:user` with:
```
  - capabilities      (json)   # e.g. ["approve:escalation", "aws:admin", "k8s:admin"]
  - auth_methods      (json)   # e.g. ["slack_oauth", "github_oauth", "api_key"]
```

Or create a separate `type:role` bead (already exists as kind:config) that
links users to capability sets. Design decision needed.

### Authenticated Approval Web UI

The existing decisions web UI (`bridge/web/index.html`) resolves decisions
with a simple POST. For escalations, we need:

1. **Authentication**: OAuth flow (GitHub, Google, or Slack) or API key.
   The web UI must know *who* is approving, verified cryptographically.
2. **Authorization**: Check that the authenticated user has the required
   capability for this escalation type.
3. **Notification flow**: Slack notification → link to web UI → authenticated
   approval → execution trigger.

This could be:
- An extension of the existing decisions web UI with an auth layer.
- A new dedicated escalation UI (more likely, given the different UX needs).

### Local Execution Pipeline

When an escalation is approved:

1. **For `local_claude`**: The approver's laptop runs a sandboxed Claude Code
   session with the approved prompt injected. Implementation:
   - Web UI shows "Run on your machine" button after approval.
   - Button triggers `gb agent start --local --prompt="<approved_prompt>"`
     or equivalent.
   - Agent executes, captures result, posts back to kbeads.

2. **For `local_gardenio`**: A `gb agent start --local` session connects via
   SSE and receives the escalation as a task. The agent executes with the
   local user's credentials (AWS, kubectl, etc.).

3. **For `service_account`** (future): A system process with scoped credentials
   picks up the escalation and executes it. Similar to a K8s Job.

### SSE Pipeline for Local Agents

Local agents need to subscribe to escalation events relevant to them:

```
GET /v1/events/stream?topics=beads.escalation.*&fleet=<fleet>
```

The existing SSE hub (`kbeads/internal/server/sse.go`) already supports
topic filtering and fleet scoping. Local agents can use the same mechanism
as the bridge.

---

## Epic Breakdown

### Phase 1: Foundation (type schema + user auth model)

1. **Design escalation bead type schema** — Define `type:escalation` fields,
   statuses, and lifecycle. Consider relationship to decisions.
2. **Extend user model with capabilities** — Add authorization fields to
   `type:user` or create a capability/role association model.
3. **Research authentication options for web UI** — Evaluate OAuth providers
   (Slack, GitHub, Google), API keys, and session management approaches.

### Phase 2: Core Pipeline (approval + execution)

4. **Authenticated escalation web UI** — New web page (or extension of
   decisions UI) with OAuth login, escalation list, approval flow.
5. **Escalation→local agent execution bridge** — When an escalation is
   approved, trigger a local Claude Code or gardenio agent session with the
   approved prompt/spec.
6. **Result capture and audit trail** — Local agent posts results back to the
   escalation bead. Full audit: request → approval (who, when, how) →
   execution (where, by whom) → result.

### Phase 3: Polish + SSE (streaming + generic local agents)

7. **SSE subscription for local agents** — Allow `gb agent start --local`
   to subscribe to escalation events and pick up approved work automatically.
8. **Slack notification integration** — Escalation created → Slack message
   with link to web UI (not inline buttons). Escalation approved → Slack
   update. Execution complete → Slack notification.
9. **Decision↔Escalation migration path** — Allow existing high-priority
   decisions to be "promoted" to escalations when they need authenticated
   approval.

### Delivery Summary (2026-04-06)

All planned phases (1-3) plus E2E test coverage have been implemented and merged to main:

| Phase | Item | PR/Commit | Status |
|---|---|---|---|
| **1** | type:escalation bead schema | PR #629 | Merged |
| **1** | User capabilities & auth model | PR #630 | Merged |
| **1** | Auth research for web UI | PR #628 | Merged |
| **2** | Authenticated escalation web UI | PR #633 | Merged |
| **2** | Escalation→local agent execution bridge | PR #637 | Merged |
| **2** | Audit trail API & result capture | f89fa77b | Merged |
| **3** | SSE escalation watcher for local agents | PR #637 | Merged |
| **3** | Slack notification integration | PR #636 | Merged |
| **3** | Decision→Escalation promotion path | PR #632 | Merged |
| **E2E** | Phase 1: schema & type tests | PR #638 | Merged |
| **E2E** | Phase 2: bridge API approval tests | PR #641 | Merged |
| **E2E** | Phase 3: execution bridge tests | PR #642 | Merged |
| **E2E** | Phase 4: Slack notification tests | PR #646 | Merged |

### Future: Service Account Execution

10. **Service account executor** — System-level execution target for
    automated privileged operations (no human laptop required).
11. **Scoped credential vending** — Short-lived, scoped credentials for
    execution environments (AWS STS, K8s service tokens).

---

## Open Questions (resolved or deferred)

1. **Should escalations be a subtype of decisions or a separate type?**
   Decisions have established UX and agent integration. Escalations could be
   decisions with `label:escalation` and additional required fields, or a
   wholly separate type. Trade-off: reuse vs. clarity.

2. **What authentication provider for the web UI?** Slack OAuth is natural
   (users already have Slack accounts) but may not meet compliance
   requirements. GitHub OAuth works if all users have GitHub accounts. Could
   support multiple providers mapped via user beads.

3. **How does the local agent receive the approved prompt?** Options:
   - CLI flag: `gb agent start --local --escalation=ka-XXXXX`
   - SSE subscription: agent listens for approved escalations.
   - Nudge: bridge POSTs to local agent's coop endpoint.

4. **Sandboxing for local Claude Code execution.** How do we ensure the
   approved prompt is executed in a constrained environment? Claude Code's
   permission system (allow/deny per tool) could be configured per
   escalation type.

5. **How do user capabilities interact with project scoping?** Should
   capabilities be global ("can approve any escalation") or project-scoped
   ("can approve escalations for project:gardenio only")?

---

## Relationship to Existing Systems

| System | Relationship |
|---|---|
| **Decisions** | Escalations are a higher-security variant of decisions. May share SSE/notification infrastructure |
| **Users** | Escalations require authenticated users with capabilities — drives the user auth model forward |
| **Permission callbacks** | Per-tool permission is the "small" version of this; escalations are the "big" version |
| **Gates** | Escalation approval could be modeled as a gate with richer metadata |
| **Local agents** | `gb agent start --local` is the execution environment for escalations |
| **gardenio-channel** | Channel server could relay escalation results back to the bridge |
| **Config beads** | Escalation types and approval policies could be configured via config beads |
