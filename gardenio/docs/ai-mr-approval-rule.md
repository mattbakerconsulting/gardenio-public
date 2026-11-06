# AI-authored MR human-approval enforcement (DEVOPS-730)

> This doc covers the **enforcement mechanism**. For the surrounding **review
> policy and human workflow** (required reviewer count, adoption workflow,
> fixVersion gating, rollout checklist), see
> [`ai-mr-review-policy.md`](./ai-mr-review-policy.md) (DEVOPS-781).

The gitlab-bridge can automatically attach a **required human-approval rule** to
every merge request opened by the Gardenio bot, so an AI-authored MR cannot merge
without sign-off from a designated reviewer group. This is the **Option A
(webhook + internal service)** approach from DEVOPS-730: the bridge already
receives `merge_request` webhook events, so enforcement is a small addition to
the existing `open`/`reopen` handler rather than a new service.

## How it works

1. GitLab fires a `merge_request` webhook (`action=open` / `reopen`) to the
   bridge's `/webhook` endpoint.
2. If the MR author is one of the configured bot usernames, the bridge calls
   `GET .../merge_requests/:iid/approval_rules` to check whether the rule is
   already present (idempotency), then — if absent — `POST`s a rule named
   `AI-generated change review` requiring N approvals from the configured
   reviewer group/users.
3. Non-bot authors are skipped; existing rules are never duplicated.

The enforcement point is `AIApprovalEnforcer.EnsureRule`
(`internal/bridge/gitlab_ai_approval.go`), called from the webhook handler in
`internal/bridge/gitlab_sync.go`. Failures are logged and non-fatal — a
transient GitLab error never rejects the webhook or blocks MR-bead upserts.

## Idempotency

`EnsureRule` is safe to call on every `open`/`reopen` delivery:

- The rule is matched by name (case-insensitive). If a rule with the configured
  name already exists on the MR, nothing is created.
- Webhook re-deliveries, MR re-opens, and updates therefore never stack
  duplicate rules.

## Configuration

Opt-in and **off by default** (`AI_APPROVAL_ENABLED` unset). All policy values
are config-driven so the mechanism can ship dark and be turned on once the team
agrees the policy (reviewer count, the `ai-reviewers` group).

| Env var | Helm value (`gitlabBridge.aiApproval.*`) | Default | Purpose |
|---|---|---|---|
| `AI_APPROVAL_ENABLED` | `enabled` | `false` | Master switch. |
| `AI_APPROVAL_RULE_NAME` | `ruleName` | `AI-generated change review` | Rule name (matched verbatim for idempotency). |
| `AI_APPROVAL_REQUIRED` | `approvalsRequired` | `1` | Required human approvals (the 1-vs-2 decision). |
| `AI_APPROVAL_BOT_USERNAMES` | `botUsernames` | `GITLAB_BOT_USERNAME` | CSV of GitLab usernames treated as AI authors. |
| `AI_APPROVAL_REVIEWER_GROUP_IDS` | `reviewerGroupIDs` | *(none)* | CSV of GitLab group IDs (e.g. the `ai-reviewers` group). |
| `AI_APPROVAL_REVIEWER_USER_IDS` | `reviewerUserIDs` | *(none)* | CSV of GitLab user IDs (alt/supplement to groups). |

At least one of `reviewerGroupIDs` / `reviewerUserIDs` must be set. With neither,
the enforcer logs and skips so it never creates a rule no human can satisfy.

### Example (Helm)

```yaml
gitlabBridge:
  aiApproval:
    enabled: true
    approvalsRequired: 1
    reviewerGroupIDs: "12345"   # the ai-reviewers group ID
```

## Requirements & limitations

- **GitLab tier**: MR-level approval rules are a Premium feature. On
  Community-Edition the `approval_rules` endpoints return 404; the enforcer
  treats that as "no approval gate" and degrades to a logged no-op rather than
  failing.
- **Token scope**: the token in `gitlabBridge.gitlab.secretName` must have `api`
  scope (write), not just `read_api`, to create approval rules.
- **Reviewer visibility**: the bot's token must be able to see the reviewer
  group/users. A group whose access is only inherited from a higher group the
  bot cannot see will be silently dropped by GitLab (see advice ka-0000002292).

## Open policy questions (per the ticket)

These are deliberately left as config rather than hard-coded, pending the team's
watercooler/retro decision:

- Required reviewer count: 1 vs 2 (`approvalsRequired`).
- The reviewer group membership (`ai-reviewers`).
- Whether a Gardenio MR adopter/assignee model is required before requesting
  review.

## Related

- The ticket also mentions filtering Gardenio's daily digest to MRs whose linked
  Jira has a `fixVersion`. That is a separate change to the digest/posting logic
  and is **not** part of this enforcer.
- GitLab Approvals API: <https://docs.gitlab.com/api/merge_request_approvals/>
