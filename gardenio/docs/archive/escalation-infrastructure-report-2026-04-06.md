# Escalation Infrastructure & Framework: Status Report

**Date:** 2026-04-06
**Task:** ka-0000006671
**Author:** gardenio-investigate-the-infrastructure-rto

---

## Executive Summary

The privilege escalation pipeline is **substantially complete**. All three planned phases (Foundation, Core Pipeline, Polish + SSE) have been implemented and merged to main across PRs #628–#646. The system spans all four monorepo components (kbeads, gardenio controller/bridge, coop, beads3d) with ~4,800 lines of escalation-specific code plus ~2,400 lines of E2E tests. One execution target (`service_account`) remains unimplemented, and several enhancement opportunities exist.

---

## What Has Been Done

### 1. Bead Type Schema (`type:escalation`)

**Status: Complete** (PR #629)

A first-class `type:escalation` bead type is registered in kbeads builtins (`kbeads/internal/server/config.go`) with the following fields:

| Field | Type | Required | Purpose |
|---|---|---|---|
| `operation_type` | string | yes | e.g. "aws_iam", "k8s_rbac", "helm_deploy" |
| `operation_spec` | json | yes | Machine-parseable operation definition |
| `context` | string | no | Human-readable background |
| `requesting_agent` | string | no | Agent bead ID that requested the escalation |
| `requesting_task` | string | no | Task bead ID being worked on |
| `approved_by` | string | no | User bead ID (cryptographically verified) |
| `approved_at` | string | no | RFC3339 timestamp |
| `approval_method` | enum | no | `web_ui`, `api_token`, `cli` |
| `execution_target` | enum | no | `local_claude`, `local_gardenio`, `service_account` |
| `executor` | string | no | Who/what executed the operation |
| `result` | json | no | Execution output |
| `result_status` | enum | no | `success`, `failure`, `partial` |

The type is modeled as `kind: data` (not `issue`), separate from decisions. This is a deliberate architectural choice — escalations need structured operation specs, authenticated approval, different execution environments, and mandatory audit trails.

### 2. User Authorization Model

**Status: Complete** (PR #630)

The `UserIdentity` struct in `bridge/user_directory.go` now includes:

- **`Capabilities []string`** — e.g. `["approve:escalation", "aws:admin"]`
- **`AuthMethods []string`** — e.g. `["slack_oauth", "github_oauth"]`
- **`HasCapability(cap string) bool`** — supports wildcard matching (`approve:*` matches `approve:escalation`)

Capabilities are stored as a JSON array on `type:user` bead fields and cached in the in-memory `UserDirectory` (5-minute refresh interval).

### 3. Authentication Layer (OIDC)

**Status: Complete** (PR #628 research, PR #633 implementation)

The system uses the existing Keycloak OIDC infrastructure:

- **`oidcauth` package** (`controller/internal/oidcauth/oidcauth.go`) — HTTP middleware that validates Bearer tokens against Keycloak, extracts email + realm roles, and injects them into request context.
- **ForwardAuth flow** — Traefik → oauth2-proxy → Keycloak Authorization Code flow. The `X-Auth-Request-Email` header is the primary identity carrier.
- **`EmailFromRequest()`** — Checks ForwardAuth header first, then falls back to OIDC context.

**Helm chart additions** (`values.yaml`):
- Realm role: `escalation-approver`
- Realm group: `gardenio-escalation-approvers`
- OIDC client: `gardenio-escalation` (public, PKCE S256)

### 4. Escalation Web UI

**Status: Complete** (PR #633)

A dedicated single-page web UI at `bridge/web/escalations.html` (456 lines):

- Dark-themed card-based interface
- Real-time SSE updates (connected/reconnecting badge)
- User identity bar showing auth status and capabilities
- Pending/All filter tabs
- Card detail view with operation spec, context, agent info
- Approve/Reject actions with execution target selector
- Keyboard navigation (j/k navigate, Enter open, a approve, x reject)
- Capability-gated: approve/reject buttons only shown if user has `approve:escalation`

### 5. Escalation API Endpoints

**Status: Complete**

#### kbeads Server (`kbeads/internal/server/http_escalations.go`, 327 lines)

| Endpoint | Method | Purpose |
|---|---|---|
| `/v1/escalations` | GET | List escalations (filterable by status) |
| `/v1/escalations/{id}` | GET | Get escalation detail |
| `/v1/escalations/{id}/approve` | POST | Record approval + transition to `in_progress` |
| `/v1/escalations/{id}/reject` | POST | Record rejection + close |
| `/v1/escalations/{id}/result` | POST | Capture execution result + close |

#### Bridge API (`controller/internal/bridge/api_escalations.go`, 349 lines)

| Endpoint | Method | Purpose |
|---|---|---|
| `/api/escalations` | GET | List escalations (proxied with auth) |
| `/api/escalations/me` | GET | Authenticated user identity + capabilities |
| `/api/escalations/{id}` | GET | Show escalation detail |
| `/api/escalations/{id}/approve` | POST | Approve with OIDC-verified user bead ID |
| `/api/escalations/{id}/reject` | POST | Reject with reason |

#### SSE Proxy (`controller/internal/bridge/api_escalations_sse.go`, 117 lines)

| Endpoint | Method | Purpose |
|---|---|---|
| `/api/escalations/events` | GET (SSE) | Real-time escalation lifecycle events |

### 6. Execution Bridge

**Status: Complete** (PR #637)

The `Escalations` watcher (`bridge/escalations.go`, 439 lines) subscribes to the kbeads SSE event stream and handles the full lifecycle:

1. **Created** → Tracks requesting agent, notifies Slack
2. **Updated** (with `approved_by`) → Triggers execution on target
3. **Closed** → Nudges requesting agent with results, notifies Slack

**Execution targets implemented:**

| Target | Status | Mechanism |
|---|---|---|
| `local_claude` | **Working** | Spawns a new agent with escalation prompt |
| `local_gardenio` | **Working** | Nudges existing local agent via coop, falls back to spawn |
| `service_account` | **Not implemented** | Logs warning, records failure |

The execution bridge includes:
- Deduplication (prevents re-triggering on SSE reconnect)
- Pending agent tracking (for nudge on completion)
- Result recording and bead closure
- Dependency linking (execution agent → escalation bead)

### 7. Local Agent Escalation Watcher

**Status: Complete** (PR #637)

`gb agent start --local --watch-escalations` enables a local agent to receive approved escalations via SSE:

- `agent_start_escalation.go` (305 lines) — SSE watcher that filters for `execution_target=local_gardenio`
- Fetches full escalation details, builds prompt, nudges local coop session
- Captures execution result back to escalation bead
- Reconnects with exponential backoff on connection loss
- Project-scoped filtering

### 8. Slack Notifications

**Status: Complete** (PR #636)

Full Slack notification lifecycle via `EscalationNotifier` interface:

| Event | Slack Action |
|---|---|
| Created | Posts notification card with priority emoji + "Review in Web UI" link button |
| Approved | Updates card with green checkmark + approval details |
| Completed | Updates card with result status (success/failure/partial) |
| Rejected | Updates card with rejection notice, or deletes message |

Key design: Slack is **notification-only**. Approval happens in the authenticated web UI, not via Slack buttons. This is a deliberate security decision.

### 9. Decision-to-Escalation Promotion

**Status: Complete** (PR #632)

Existing high-priority decisions can be promoted to escalations when they need authenticated approval. This bridges the gap between the informal decision workflow and the formal escalation pipeline.

### 10. beads3d Visualization

**Status: Complete**

The 3D visualization supports escalation-related rendering:
- `escalate` dependency type with yellow color (`#d4a017`)
- Up-arrow link icon for escalation edges
- `DecisionEscalated` event rendering in agent windows
- Edge toggle in control panel

### 11. E2E Test Coverage

**Status: Complete** (PRs #638, #641, #642, #646)

Four test modules totaling 2,413 lines:

| Module | Lines | What it tests |
|---|---|---|
| `test-escalation-crud.sh` | 505 | Bead CRUD via kbeads API, SSE events, type validation |
| `test-escalation-approval.sh` | 666 | Bridge API approval flow, capability enforcement, auth |
| `test-escalation-execution.sh` | 698 | Full lifecycle: create → approve → agent spawn → result capture |
| `test-escalation-slack-notify.sh` | 544 | Slack message lifecycle: create → approve → complete → reject |

### 12. Unit Test Coverage

The bridge has comprehensive unit tests in `escalations_test.go` (392 lines) covering:
- Requesting agent tracking on creation
- Execution triggering on approval
- Deduplication of trigger events
- Requesting agent nudge on completion
- Local gardenio fallback when no agent available
- Prompt construction
- Result truncation

---

## What's Left To Do

### High Priority

#### 1. Service Account Execution Target
**Status: Not implemented** — Currently logs a warning and records failure.

This is the third execution target (`service_account`) from the original design. It would enable automated privileged operations without requiring a human's laptop. Implementation would involve:
- A dedicated K8s Job or CronJob executor
- Scoped credential vending (AWS STS assume-role, K8s service tokens)
- Sandboxed execution environment
- Result capture and timeout handling

**Recommendation:** This is the highest-value remaining work item. It closes the loop for fully automated infrastructure operations (e.g., rotating credentials on a schedule).

#### 2. API Key Authentication for CLI Workflows
**Status: Not started** — Mentioned in the auth recommendation as "Phase 2"

Currently, approval requires the web UI (OIDC flow). For CLI-based approval (`gb escalation approve <id>`), API key authentication is needed:
- Key generation and storage on user beads
- Key validation middleware (separate path from OIDC)
- `gb escalation approve` CLI command

**Recommendation:** Medium priority. Enables operators to approve escalations from their terminal without opening a browser.

#### 3. User Bead Auto-Provisioning
**Status: Not started** — Noted in auth recommendation as "Phase 2"

When a user authenticates via Keycloak but has no user bead, the system returns 403 with "user not found in gardenio directory." Auto-creation from OIDC token claims (email, name) would reduce friction.

**Recommendation:** Low priority for now. Manual user bead creation is an acceptable workflow for a small team.

#### 4. Keycloak Identity Brokering
**Status: Not started** — Noted as future enhancement

Federate upstream identity providers (Slack, GitHub, Google) through Keycloak so users can "Sign in with Slack" while the application only talks to Keycloak. Purely a Keycloak admin configuration change.

**Recommendation:** Nice-to-have. The current direct Keycloak login is functional.

### Medium Priority

#### 5. Escalation Timeout/Expiry
**Status: Not implemented**

There is no mechanism to auto-expire stale escalation requests. An escalation created but never approved sits open indefinitely.

**Recommendation:** Add a configurable TTL (e.g., 24h) after which unapproved escalations are auto-rejected with reason "expired." Could be implemented as a periodic sweep in the bridge.

#### 6. Escalation Priority Routing
**Status: Partial** — Priority field exists but isn't used for routing

Escalations inherit the bead priority field but there's no escalation-specific routing logic (e.g., P0 escalations auto-notify on-call via PagerDuty, P2 escalations can wait).

**Recommendation:** Wire priority-based notification channels. P0-1 could trigger additional notification paths beyond the Slack channel.

#### 7. Audit Log Export
**Status: Partial** — Audit trail exists on beads but isn't queryable as a dedicated log

The escalation bead fields capture a full audit trail (who requested, who approved, how, when, who executed, result), but there's no dedicated audit log view or export.

**Recommendation:** Add a `/api/escalations/audit` endpoint that returns a flattened audit log across all escalations, suitable for compliance reporting.

#### 8. Escalation Analytics/Dashboard
**Status: Not started**

No metrics or analytics on escalation volume, approval latency, success/failure rates, or common operation types.

**Recommendation:** Could be a beads3d feature or a dedicated Grafana dashboard. Low effort if built on the existing kbeads List API with aggregation.

### Low Priority

#### 9. Operation Spec Templates
**Status: Not started**

Currently, agents construct `operation_spec` JSON freeform. Pre-defined templates per `operation_type` would standardize specs and enable validation.

**Recommendation:** Create `type:escalation_template` config beads that define the expected JSON schema for each operation type.

#### 10. Approval Policies
**Status: Not started**

Currently, any user with `approve:escalation` capability can approve any escalation. There's no concept of:
- Multi-person approval (require 2 approvers for P0)
- Operation-type scoping (only `aws:admin` users can approve `aws_iam` escalations)
- Project scoping (can only approve escalations for projects you're assigned to)

**Recommendation:** Implement as a policy engine on top of the existing capability system. The `HasCapability` wildcard matching already supports `approve:*` patterns — extend to `approve:escalation:aws_iam`.

#### 11. Escalation CLI Commands
**Status: Partial** — `gb agent start --watch-escalations` exists, but no `gb escalation` subcommand

There's no `gb escalation create/list/approve/reject` CLI surface. Agents create escalations via `kd create --type=escalation`, and approval is web-only.

**Recommendation:** Add `gb escalation` subcommands for a complete CLI workflow.

---

## Architecture Summary

```
┌─────────────┐     ┌──────────────┐     ┌───────────────┐
│   Agent      │────>│   kbeads     │────>│   NATS Bus    │
│ (requesting) │     │ (bead store) │     │ (SSE events)  │
└─────────────┘     └──────────────┘     └───────┬───────┘
                           │                      │
                           │ HTTP API             │ SSE
                           ▼                      ▼
                    ┌──────────────┐     ┌───────────────┐
                    │  Web UI      │     │  Bridge       │
                    │ (OIDC auth)  │────>│ (escalations) │
                    └──────────────┘     └───────┬───────┘
                                                  │
                           ┌──────────────────────┼──────────────────┐
                           │                      │                  │
                           ▼                      ▼                  ▼
                    ┌──────────┐          ┌───────────┐      ┌──────────┐
                    │ Slack    │          │ local     │      │ service  │
                    │ (notify) │          │ claude/   │      │ account  │
                    └──────────┘          │ gardenio   │      │ (future) │
                                          └───────────┘      └──────────┘
```

---

## Code Inventory

| Component | File | Lines | Purpose |
|---|---|---|---|
| **kbeads** | `server/http_escalations.go` | 327 | REST API endpoints |
| **kbeads** | `server/escalation_test.go` | ~200 | Server unit tests |
| **kbeads** | `server/config.go` (section) | ~30 | Type schema registration |
| **kbeads** | `model/bead.go` (section) | 1 | `TypeEscalation` constant |
| **bridge** | `escalations.go` | 439 | SSE watcher + execution bridge |
| **bridge** | `escalations_test.go` | 392 | Unit tests |
| **bridge** | `bot_escalations.go` | 346 | Slack notification lifecycle |
| **bridge** | `api_escalations.go` | 349 | Web UI API endpoints |
| **bridge** | `api_escalations_sse.go` | 117 | SSE proxy for web UI |
| **bridge** | `api_escalations_test.go` | ~200 | API unit tests |
| **bridge** | `web/escalations.html` | 456 | Web UI (HTML/CSS/JS) |
| **bridge** | `user_directory.go` (capabilities) | ~50 | Authorization model |
| **oidcauth** | `oidcauth.go` | 125 | OIDC middleware |
| **gb CLI** | `agent_start_escalation.go` | 305 | Local agent SSE watcher |
| **gb CLI** | `agent_start_escalation_test.go` | ~100 | CLI unit tests |
| **beadsapi** | `client_escalations.go` | ~150 | Client methods |
| **beads3d** | various | ~50 | Visualization support |
| **E2E** | 4 test modules | 2,413 | End-to-end tests |
| **Plans** | 3 documents | ~700 | Design docs + auth recommendation |
| **Helm** | `values.yaml` (sections) | ~40 | Keycloak config |
| **TOTAL** | | **~5,800+** | |

---

## Suggestions

### Immediate Actions (This Sprint)

1. **Add escalation timeout** — Implement a 24h default TTL for unapproved escalations. A ~50-line periodic sweep in the bridge would prevent orphaned escalation beads.

2. **Add `gb escalation` CLI** — Even basic `gb escalation list` and `gb escalation create` would improve the agent-side ergonomics significantly.

### Next Sprint

3. **Implement `service_account` execution target** — This is the biggest remaining gap. Design recommendation: use a K8s Job with a dedicated ServiceAccount that has scoped RBAC. The Job spec is derived from `operation_spec`. The bridge watches the Job's completion status and captures logs as the result.

4. **Add API key auth** — Enable `gb escalation approve <id>` from the CLI by adding API key generation and validation to user beads.

### Future

5. **Operation-type scoped policies** — Extend `HasCapability` to support `approve:escalation:aws_iam` patterns. This is a natural extension of the existing wildcard matching.

6. **Metrics integration** — Expose escalation metrics (volume, latency, success rate) as Prometheus counters for Grafana dashboards.

7. **Consider promoting to first-class `issue` kind** — Currently escalations are `kind: data`, which means they don't appear in `gb ready` or `kd list` by default. If escalations should be operator-visible work items, consider changing to `kind: issue`.

---

## Conclusion

The privilege escalation pipeline is a well-architected, production-ready system. The implementation quality is high: clean separation of concerns across components, comprehensive test coverage (unit + E2E), proper authentication via OIDC, and a polished web UI. The main gap is the `service_account` execution target for fully automated privileged operations. The existing `local_claude` and `local_gardenio` targets cover the human-in-the-loop use case effectively.
