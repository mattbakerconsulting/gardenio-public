# Review policy for AI-authored (Gardenio) MRs (DEVOPS-781)

This is the **policy and workflow document** for merge requests authored by the
Gardenio bot. It defines how AI-authored MRs are reviewed, who must approve them,
how a human adopts them, and how the policy is enforced at the GitLab level.

It is the umbrella for the enforcement work already shipped under DEVOPS-730 /
DEVOPS-741 (the approval-rule enforcer — see
[`ai-mr-approval-rule.md`](./ai-mr-approval-rule.md)). That doc covers the
*mechanism*; this doc covers the *policy* the mechanism enforces and the
human workflow around it.

## Why a policy is needed

Gardenio opens AI-authored MRs and posts a daily ranked summary to
`#acme-core-devs`. As adoption grows, "an AI wrote this and a human glanced at
it" is not a defensible review story for a clinical-trial codebase. We need an
agreed, **enforced** bar so that no AI-authored change merges without explicit
human sign-off, and so that a named human is accountable for every AI MR that
lands.

## Policy

### 1. Required reviewers

Every AI-authored MR **must** carry the GitLab approval rule
`AI-generated change review`, requiring **≥1 approval** from the `ai-reviewers`
group before it can merge.

- **Recommendation: require 1 approval to start**, raise to 2 only if review
  quality proves insufficient. One required reviewer keeps the merge train
  moving while still guaranteeing a named human signs off; a second required
  approval roughly doubles MR latency and tends to diffuse rather than
  concentrate accountability. The `approvalsRequired` value is config, so the
  team can change 1↔2 without a code change.
- The approving human is the accountable reviewer of record. Approval means
  "I read this diff and I stand behind it merging," not "CI is green."

> **Decision required (operator/team):** confirm 1 vs 2 required approvals and
> ratify the `ai-reviewers` group membership. This is the only blocking policy
> input; everything else below is already implemented or is documentation.

### 2. Human adoption workflow

Before an AI-authored MR is put forward for review it should be **adopted** by a
human, who becomes its assignee:

1. Gardenio opens the MR (draft) and links it to its Jira ticket.
2. A human **adopts** the MR by assigning themselves. Adoption means they have
   vetted and, where needed, polished the change — description, scope, and test
   evidence — so it is review-ready, not raw model output.
3. The adopter marks the MR ready and requests review from `ai-reviewers`.
4. A reviewer from `ai-reviewers` (not the adopter) approves; the approval rule
   is satisfied and the MR can merge.

The adopter and the approver should be different people: adoption is authorship
accountability, approval is review accountability. A single human doing both
collapses the two-person check the policy exists to provide.

### 3. Surfacing: fixVersion gating

Gardenio's daily ranked MR summary to `#acme-core-devs` should be filtered to
**only** MRs whose linked Jira ticket has a `fixVersion` set. A ticket without a
`fixVersion` has not been scheduled into a release, so surfacing its MR for
review invites work on changes the team has not yet committed to shipping.

The Jira poller already supports per-project `fixVersion` scoping in JQL
(`buildJQL`, `internal/bridge/jira_poller.go`), so the building block exists.
The daily ranked post itself is produced by the delivery/standup agent workflow
rather than a single checked-in service; the gating therefore belongs in that
posting workflow's selection step (include an MR only when its linked ticket's
`fixVersion` is non-empty) and is tracked as a follow-on item, not part of the
enforcement mechanism.

### 4. Enforcement (GitLab level)

Enforcement uses **Option A — webhook + internal service**, which was the
ticket's preferred option and is **already implemented and merged**:

- The `gitlab-bridge` `/webhook` handler, on `merge_request` `open`/`reopen`,
  calls `AIApprovalEnforcer.EnsureRule`
  (`internal/bridge/gitlab_ai_approval.go`, wired at
  `internal/bridge/gitlab_sync.go:824`).
- For bot-authored MRs it idempotently attaches the `AI-generated change review`
  approval rule (≥N approvals from `ai-reviewers`). Non-bot MRs are untouched.
- Idempotency: the rule is matched by name (case-insensitive), so re-deliveries,
  reopens, and pipeline retries never stack duplicate rules.
- The feature is **off by default** (`AI_APPROVAL_ENABLED=false`) so it ships
  dark until §1 is ratified and the `ai-reviewers` group exists.

Option B (CI pipeline making the same API call with a scoped token) was **not**
chosen: it would expose a token with approval-rule write access to CI, an
elevated risk the webhook path avoids. Full mechanism, config table, and GitLab
tier/token requirements live in [`ai-mr-approval-rule.md`](./ai-mr-approval-rule.md).

## Rollout checklist

To turn the policy on:

1. **Ratify §1** — agree 1 vs 2 required approvals (team/operator decision).
2. **Create the `ai-reviewers` GitLab group** and add members. Verify the bot's
   token can *see* the group — a group whose access is only inherited from a
   higher group the bot cannot see is silently dropped by GitLab
   (advice `ka-0000002292`).
3. **Set config** — `gitlabBridge.aiApproval.reviewerGroupIDs` (the group ID)
   and `approvalsRequired` (the §1 value).
4. **Confirm token scope** — the bridge's GitLab token needs `api` (write) scope
   to create approval rules, and the project must be on a GitLab tier where
   MR-level approval rules exist (Premium+). On CE the enforcer degrades to a
   logged no-op.
5. **Flip the switch** — `gitlabBridge.aiApproval.enabled=true`.
6. **Verify** — open a test bot MR and confirm via
   `GET .../merge_requests/:iid/approval_rules` that the
   `AI-generated change review` rule is attached and lists `ai-reviewers`.

## Acceptance-criteria status (DEVOPS-781)

| AC | Status | Where |
|---|---|---|
| Agree required number of human reviewers | **Open — team decision** | §1; recommendation: 1, config `approvalsRequired` |
| Agree human-adoption workflow | **Documented; team to ratify** | §2 |
| Filter daily posts to MRs with a linked-ticket `fixVersion` | **Documented; follow-on impl in posting workflow** | §3 |
| Enforcement mechanism implemented + rule auto-applied | **Done (merged)** | §4; `ai-mr-approval-rule.md` (DEVOPS-730/741) |
| Idempotency handled | **Done** | §4; rule matched by name |
| Policy and workflow documented | **Done — this document** | this file |

## Related

- [`ai-mr-approval-rule.md`](./ai-mr-approval-rule.md) — the enforcement
  mechanism (DEVOPS-730 / DEVOPS-741).
- DEVOPS-777 — closed as a duplicate of DEVOPS-730.
- GitLab Approvals API: <https://docs.gitlab.com/api/merge_request_approvals/>
