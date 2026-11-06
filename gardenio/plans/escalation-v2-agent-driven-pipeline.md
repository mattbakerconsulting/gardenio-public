# Escalation V2: Agent-Driven Pipeline

**Status:** Planned
**Epic:** ka-0000004165
**Date:** 2026-04-07
**Predecessor:** [Escalation V1](archive/privilege-escalation-pipeline.md) (Phases 1-3, complete)

---

## Problem Statement

Escalation V1 delivered a working privilege escalation pipeline — agents can request
elevated operations, humans approve via authenticated web UI, and execution agents
carry out the work. But real-world usage has surfaced structural weaknesses:

1. **The "Escalate" button fills the wrong form.** When a human clicks "Escalate"
   on a decision in Slack, the human fills out operation_type and execution_target
   in a modal. But the *agent* knows what it needs — the human doesn't. The button
   should return control to the calling agent and force it to author the escalation.

2. **Escalation forms are too thin.** The current schema captures `operation_type`
   and a freeform `operation_spec` JSON blob. This is not enough for an operator to
   act on. A real escalation needs: an executable script, a credential requirements
   checklist, expected input data, and expected output descriptions.

3. **No executable scripts.** The `operation_spec` is documentation, not action.
   Escalations should include a script that can actually be *run* — shell, Python,
   or a structured command sequence with conditionals and error handling.

4. **No credential estimation.** Agents don't declare what credentials the operator
   will need. The operator has to read the operation_spec and guess. The agent
   should provide its best assessment: "you'll need an AWS IAM role with X policy"
   or "you'll need kubectl access to the production cluster."

5. **No sensitive data return path.** Escalations frequently produce sensitive
   results — rotated credentials, API tokens, audit data, PII extracts. Today,
   results land in the `result` field on the escalation bead and can leak into
   Slack notifications and agent logs. There is no mechanism to respect data
   sensitivity.

## Vision

An escalation should be **everything an escalated-privilege operator needs** to
fulfill the entirety of the elevated-permissions-requiring part of the problem:

- **What to run** — a concrete, executable script (not a prose description)
- **What access is needed** — credentials, roles, contexts, tokens
- **What inputs are expected** — env vars, files, parameters
- **What will be produced** — expected outputs with sensitivity classifications
- **How to return results safely** — a secure report bead that respects sensitivity

The agent authors the escalation. The human reviews and approves. The executor runs
the script. Results flow back through a sensitivity-aware channel.

---

## What V1 Built (Recap)

| Component | Status | Key files |
|---|---|---|
| `type:escalation` bead schema | Complete | `kbeads/internal/server/config.go` |
| User authorization model (capabilities) | Complete | `bridge/user_directory.go` |
| OIDC authentication (Keycloak) | Complete | `controller/internal/oidcauth/` |
| Escalation web UI | Complete | `bridge/web/escalations.html` |
| Escalation API (kbeads + bridge) | Complete | `server/http_escalations.go`, `bridge/api_escalations.go` |
| SSE real-time updates | Complete | `bridge/api_escalations_sse.go` |
| Execution bridge (local_claude, local_gardenio) | Complete | `bridge/escalations.go` |
| Local agent escalation watcher | Complete | `gb/agent_start_escalation.go` |
| Slack notifications | Complete | `bridge/bot_escalations.go` |
| Decision-to-escalation promotion | Complete | `bridge/api_decisions.go`, `bridge/bot_decisions_modal.go` |
| E2E tests (4 modules, 2,413 lines) | Complete | `tests/e2e/scripts/test-escalation-*.sh` |

Total V1: ~5,800 lines across all components.

---

## V2 Design

### Theme 1: Agent Authors the Escalation

**Current flow (V1):**
```
Agent creates decision → Human clicks "Escalate" in Slack →
Human fills out operation_type + execution_target in modal →
Escalation bead created → Approval → Execution
```

**New flow (V2):**
```
Agent creates decision → Human clicks "Return to Agent for Escalation" in Slack →
Agent is nudged: "Create a full escalation for this decision" →
Agent runs `gb escalation create` with structured form →
Escalation bead created with full spec → Approval → Execution
```

The key insight: when a human clicks "Escalate" on a decision, they're saying
"this needs elevated privileges" — but they don't know *what* the agent needs.
The button should tell the agent: "I agree this needs escalation, now you fill
out the form."

**Implementation:**
- Modify the Slack "Escalate" button handler to nudge the requesting agent
  instead of opening the human promote modal
- The nudge message instructs the agent to run `gb escalation create`
- The existing promote modal becomes a fallback for orphaned decisions
  (where the requesting agent is gone)

### Theme 2: Structured, Validated Escalation Form

**Current `type:escalation` fields:**
```text
operation_type    (string, required)   # e.g. "aws_iam"
operation_spec    (json, required)     # freeform JSON blob
context           (string)            # human-readable narrative
```

**V2 additions:**
```text
execution_script         (string, required)   # the actual script to run
credential_requirements  (json)               # what the operator needs
data_requirements        (json)               # what inputs the script expects
expected_outputs         (json)               # what will be produced, with sensitivity
sensitivity              (enum)               # overall classification
```

#### execution_script

The execution_script is the actual runnable artifact. Not a prompt, not a
description — a script. It can be:

```bash
#!/bin/bash
# Rotate the production database credentials
set -euo pipefail

# 1. Generate new password
NEW_PASS=$(openssl rand -base64 32)

# 2. Update the RDS instance
aws rds modify-db-instance \
  --db-instance-identifier prod-primary \
  --master-user-password "$NEW_PASS" \
  --apply-immediately

# 3. Wait for the modification to complete
aws rds wait db-instance-available \
  --db-instance-identifier prod-primary

# 4. Update the K8s secret
kubectl create secret generic db-credentials \
  --from-literal=password="$NEW_PASS" \
  --dry-run=client -o yaml | kubectl apply -f -

# 5. Restart the application pods to pick up new creds
kubectl rollout restart deployment/api-server -n production

echo '{"new_password_prefix":"'"${NEW_PASS:0:4}"'...","rds_status":"available","pods_restarted":true}'
```

The script can include logic, conditionals, error handling, retries — whatever
the operation requires. The executor runs it, captures stdout/stderr, and
creates a report bead with the output.

#### credential_requirements

Structured declaration of what access the operator needs:

```json
[
  {
    "type": "aws_role",
    "identifier": "arn:aws:iam::123456789:role/rds-admin",
    "purpose": "Modify RDS instance master password",
    "scope": "rds:ModifyDBInstance, rds:DescribeDBInstances"
  },
  {
    "type": "k8s_context",
    "identifier": "production-cluster",
    "purpose": "Update secrets and restart deployments",
    "scope": "secrets:create, deployments:patch in namespace:production"
  }
]
```

This serves two purposes:
1. The approver can verify they have the required access before approving
2. For `service_account` execution, the system can vend scoped credentials

#### data_requirements

What inputs the script expects:

```json
[
  {
    "name": "DB_INSTANCE_ID",
    "type": "env_var",
    "description": "RDS instance identifier",
    "default": "prod-primary"
  },
  {
    "name": "K8S_NAMESPACE",
    "type": "env_var",
    "description": "Target Kubernetes namespace",
    "default": "production"
  }
]
```

#### expected_outputs

What the escalation will produce, with sensitivity tags:

```json
[
  {
    "name": "new_password",
    "description": "The new database master password",
    "sensitivity": "secret",
    "format": "string"
  },
  {
    "name": "rds_status",
    "description": "Final RDS instance status",
    "sensitivity": "internal",
    "format": "string"
  },
  {
    "name": "pods_restarted",
    "description": "Whether application pods were restarted",
    "sensitivity": "public",
    "format": "boolean"
  }
]
```

### Theme 3: Sensitive Data Return Path

**The problem:** When an escalation produces a rotated credential, an API token,
or audit data containing PII, that result currently lands in the `result` JSON
field on the escalation bead. From there it can:
- Appear in Slack notifications via `NotifyEscalationResult`
- Be logged in the bridge's info-level logs
- Be visible to any agent that reads the bead
- Persist indefinitely in the bead store

**The solution: Report Beads with Sensitivity Controls**

A new `type:escalation_report` bead (or extension of the existing `type:report`)
serves as the secure container for escalation results:

```text
type: escalation_report
kind: data
fields:
  escalation_id      (string, required)   # parent escalation
  content            (string, required)   # the actual report body
  sensitivity        (enum, required)     # public | internal | sensitive | secret
  content_type       (enum)               # text | json | script_output | credential
  redacted_summary   (string)             # safe-to-display summary
  access_log         (json)               # who has viewed this report
  expires_at         (string)             # optional TTL for content scrubbing
```

**Access control model:**

| Sensitivity | Slack | Agent logs | Web UI | Access logged |
|---|---|---|---|---|
| `public` | Full content | Full content | Open | No |
| `internal` | Full content | Summary only | Authenticated | No |
| `sensitive` | Redacted summary only | Redacted summary only | Authenticated | Yes |
| `secret` | Redacted summary only | Not visible | Re-auth required | Yes |

**Key design principles:**
- Sensitive data never appears in Slack messages — only `redacted_summary`
- The report bead stores the full content, accessed only through the web UI
- Access to `sensitive` and `secret` reports is logged on the bead
- `secret` reports require re-authentication (can't rely on session cookie)
- Reports can have a TTL (`expires_at`) — after which content is scrubbed
  but the redacted_summary and access_log survive for audit

**What this looks like in practice:**

```
┌─ Slack notification ──────────────────────────────────────┐
│ :rotating_light: Escalation completed: credential rotation │
│                                                            │
│ Result: DB credentials rotated successfully.               │
│ RDS instance available. Pods restarted.                    │
│ :lock: Sensitive data available in report.                 │
│                                                            │
│ [View Full Report]  ← links to authenticated web UI        │
└────────────────────────────────────────────────────────────┘
```

The operator clicks "View Full Report" → authenticates via OIDC → sees the full
report including the new credential → access is logged.

---

## Epic Breakdown

### Phase 1: Schema + Foundation

| # | Task | Bead | Blocked by |
|---|---|---|---|
| 1 | Escalation schema V2: execution_script, credential_requirements, expected_outputs | ka-0000001401 | — |
| 2 | Sensitive report bead: secure result return path | ka-0000002955 | — |
| 3 | Escalation validation: server-side schema enforcement | ka-0000004102 | #1 |

Phase 1 can start immediately. Tasks 1 and 2 are independent foundations.
Task 3 depends on the schema being defined first.

### Phase 2: Agent Authoring + Execution

| # | Task | Bead | Blocked by |
|---|---|---|---|
| 4 | `gb escalation create`: structured CLI command | ka-0000001702 | #1 |
| 5 | Decision-to-escalation: return control to calling agent | ka-0000002127 | #4 |
| 6 | Execution bridge V2: script runner with credential injection | ka-0000000882 | #1, #2 |

Phase 2 builds the agent-side tooling. The CLI command (task 4) must exist
before the decision-to-escalation flow (task 5) can instruct agents to use it.
The execution bridge (task 6) needs both the schema and the report bead type.

### Phase 3: UI + Notifications + Tests

| # | Task | Bead | Blocked by |
|---|---|---|---|
| 7 | Escalation web UI V2: script viewer, credential checklist | ka-0000006732 | #1, #2 |
| 8 | Slack notification V2: redacted summaries | ka-0000003353 | #2 |
| 9 | E2E tests for escalation V2 pipeline | ka-0000003408 | #4, #6, #8 |

Phase 3 is the user-facing polish. Web UI and Slack notifications can proceed
in parallel once their dependencies are met. E2E tests come last.

### Dependency Graph

```
                    ┌─────────────────┐
                    │ #1 Schema V2    │
                    │ ka-0000001401   │
                    └────┬──┬──┬──┬───┘
                         │  │  │  │
              ┌──────────┘  │  │  └──────────────┐
              ▼             │  │                  ▼
     ┌────────────┐        │  │         ┌────────────────┐
     │ #3 Validate │        │  │         │ #4 gb escalation│
     │ ka-PROy2xi  │        │  │         │ ka-0000001702  │
     └─────────────┘        │  │         └───┬──────┬──────┘
                            │  │             │      │
                    ┌───────┘  │             │      │
                    ▼          ▼             ▼      │
              ┌────────────────────┐  ┌──────────┐ │
              │ #6 Exec Bridge V2  │  │#5 Return │ │
              │ ka-0000000882      │  │to Agent  │ │
              └──────┬─────────────┘  │ka-6fFxyp │ │
                     │                └──────────┘ │
                     │                             │
    ┌────────────────┤                             │
    │                │                             │
    │  ┌─────────────┘                             │
    │  │    ┌──────────────────┐                   │
    │  │    │ #2 Report Bead   │                   │
    │  │    │ ka-0000002955    │                   │
    │  │    └──┬──┬──┬─────────┘                   │
    │  │       │  │  │                             │
    │  │       │  │  └────────────┐                │
    │  │       │  │               ▼                │
    │  │       │  │      ┌─────────────────┐       │
    │  │       │  │      │ #8 Slack V2     │       │
    │  │       │  │      │ ka-0000003353   │       │
    │  │       │  │      └───────┬─────────┘       │
    │  │       │  ▼              │                 │
    │  │       │ ┌───────────────┤                 │
    │  │       │ │ #7 Web UI V2  │                 │
    │  │       │ │ ka-0000006732 │                 │
    │  │       │ └───────────────┘                 │
    │  │       │                 │                 │
    │  │       │                 │                 │
    ▼  ▼       ▼                 ▼                 ▼
    ┌──────────────────────────────────────────────┐
    │            #9 E2E Tests V2                   │
    │            ka-0000003408                     │
    └──────────────────────────────────────────────┘
```

---

## Open Questions

### 1. Should execution_script be required or optional?

**For required:** Forces agents to think concretely about what needs to happen.
Prevents vague "please rotate the credentials" escalations.

**For optional with strong validation:** Some escalations are genuinely
unscriptable — "review this architecture decision" or "make a judgment call
about this security finding." These need human judgment, not automation.

**Recommendation:** Required, but allow `execution_script: "manual"` as an
explicit opt-out that signals "this requires human judgment, not a script."

### 2. How far should sensitivity enforcement go?

**Option A: Advisory only.** Sensitivity tags are metadata. The web UI shows
badges. Slack respects them for display purposes. But there's no hard access
control — anyone with web UI access can see any report.

**Option B: Enforced per-bead ACLs.** Secret-level reports require specific
capabilities to read (e.g., `read:escalation_report:secret`). Access is gated
by the existing capability system.

**Recommendation:** Start with Option A (advisory) for V2. The infrastructure
for Option B exists (capabilities, OIDC auth) and can be layered on once the
data model proves itself. The access logging on the report bead provides
auditability even without hard enforcement.

### 3. What happens to the existing promote modal?

**Option A: Remove it.** The promote modal is replaced entirely by the
"return control to agent" flow.

**Option B: Keep as fallback.** The promote modal becomes a fallback for cases
where the requesting agent is dead/unreachable. The human can still create a
minimal escalation when the agent can't.

**Recommendation:** Option B. The agent-authored path is primary, but the human
fallback is valuable for orphaned decisions.

### 4. Should report beads be a new type or extend `type:report`?

The existing `type:report` has `decision_id`, `report_type`, `content`,
`format`. It's tightly coupled to the decision workflow.

**Option A: New `type:escalation_report`.** Clean separation. Report beads for
decisions stay as-is. Escalation reports have their own schema.

**Option B: Extend `type:report`.** Add `escalation_id`, `sensitivity`,
`redacted_summary`, `access_log`, `expires_at` to the existing type. Use
`report_type` to distinguish decision reports from escalation reports.

**Recommendation:** Option B — extend `type:report`. The fields are additive
and the sensitivity model is useful for decision reports too. A decision
about a security finding might also need sensitivity controls.

### 5. Content scrubbing for expired reports

When a report's `expires_at` passes, should the content be:

**Option A: Hard-deleted.** Content field set to empty. Unrecoverable.

**Option B: Replaced with a tombstone.** Content replaced with
`"[SCRUBBED: expired 2026-04-10T00:00:00Z. Redacted summary preserved.]"`
Access log and metadata survive.

**Recommendation:** Option B. The audit trail (who created it, who accessed
it, what the redacted summary said) is valuable even after the sensitive
content is gone.

---

## Relationship to V1 Systems

| V1 Component | V2 Impact |
|---|---|
| `type:escalation` schema | Extended with 5 new fields |
| Promote modal (Slack) | Becomes fallback; primary path returns control to agent |
| `PromoteDecisionToEscalation()` | Updated to support agent-authored escalations |
| Execution bridge (`escalations.go`) | Script runner replaces prompt builder |
| `buildEscalationPrompt()` | Replaced by `buildEscalationExecutionPlan()` |
| Slack notifications (`bot_escalations.go`) | Sensitivity-aware; redacted summaries |
| Web UI (`escalations.html`) | Major overhaul: script viewer, credential checklist |
| E2E tests (4 modules) | Extended with V2 scenarios |
| `type:report` bead | Extended with sensitivity fields |

---

## Estimated Scope

| Phase | Tasks | Estimated lines | Components touched |
|---|---|---|---|
| Phase 1: Schema + Foundation | 3 | ~300 | kbeads, bridge |
| Phase 2: Agent Authoring + Execution | 3 | ~800 | gb CLI, bridge |
| Phase 3: UI + Notifications + Tests | 3 | ~1,500 | web UI, bridge, E2E |
| **Total** | **9** | **~2,600** | **All 4 monorepo components** |

V1 was ~5,800 lines. V2 is a more focused enhancement, not a rebuild.
