# CI/CD Integration

Gardenio integrates with GitHub, GitLab, Jira, and CI pipelines. It closes the loop on code review for both
**GitHub** and **GitLab**.
Each forge has its own bridge (`gitlab-bridge`, `github-bridge`). They write the same bead fields,
send the same nudges to the agent, and put the same status lines in the agent's Slack thread, so what
you read in Slack does not depend on which forge the PR or MR lives on.

| | GitLab | GitHub |
|---|---|---|
| Bridge | `gitlab-bridge` | `github-bridge` |
| Auth | GitLab API token | GitHub App: webhook secret in, one-hour installation tokens out (no PAT) |
| Change request | Merge request | Pull request |
| Bead fields | `mr_*` | the same `mr_*`, plus `forge=github` |
| Link a request to a bead | `gb mr link <bead> <url>` | `gb mr link <bead> <pr-url>` (sets `mr_url` + `forge=github`), or open the PR from the agent's `agent/<role>/<bead-id>` branch |
| Review comments to the agent | Nudge to the agent | Mail to the agent, plus the comment quoted in the Slack thread |
| Squawks as comments | `gb squawk --to-mr` | `gb squawk --to-mr` |
| Unresolved-thread resolver | Event-driven | Periodic sweep (default every 5 minutes) |
| Approvals mirrored to bead fields | Yes (`mr_approved`, `mr_approvers`) | No: an approval shows in the thread, not on the bead |
| Pipeline status | One pipeline per MR | Last completed check suite or workflow run wins |

Where a row says "the same", the code is shared or deliberately mirrored; the runbooks list the exact
gaps: [`github-bridge.md`](../../gardenio/docs/runbooks/github-bridge.md).

## GitLab Integration

### MR Linking

After creating a GitLab merge request, link it to the bead:

```bash
glab mr create --title "fix: handle nil pointer" --target-branch main
# → MR created at https://gitlab.com/.../merge_requests/415

gb mr link ka-ABC123 https://gitlab.com/.../merge_requests/415
```

**What linking enables:**
- Review comment notifications: GitLab comments nudge the agent directly
- MR state tracking: `mr_state`, `mr_merged`, `mr_pipeline_status` auto-update on the bead
- Agent resolver: If the agent crashes, a new agent spawns to handle review comments

### GitLab Bridge Features

- **MR sync**: Automatically updates bead fields when MR status changes
- **Review comment forwarding**: GitLab review comments are forwarded to the agent
- **Squawk forwarding**: Agent squawk messages posted as MR comments
- **Agent resolver**: Auto-spawns agents to handle unresolved review comments

### Pipeline Integration

```bash
gb pipeline artifacts    # List CI artifacts
gb pipeline log          # View pipeline logs
```

## Jira Integration

### Jira Bridge

The Jira bridge syncs Jira tickets to beads:
- **Import**: Jira tickets → beads (with status, priority, labels)
- **Sync**: Bidirectional status updates
- **Webhooks**: Real-time sync via Jira webhooks

### Viewing Jira Context

```bash
gb jira show PE-1234     # Show Jira ticket details in agent context
```

## GitHub Integration

### PR Linking

Open the pull request the way you would anywhere else:

```bash
gh pr create --title "fix: handle nil pointer" --base main
# → https://github.com/<org>/<repo>/pull/415
```

The bridge finds the bead for a PR in this order, and a PR that matches no bead is ignored (it never
creates beads for PRs it does not own):

1. A task bead that already carries the PR URL in `mr_url` (set by an earlier event).
2. The bead id in the head branch, `agent/<role>/<bead-id>`: the branch every launched agent works on.
3. Bead ids mentioned in the PR body (at most five are checked).

`gb mr link <bead> https://github.com/<org>/<repo>/pull/N` sets `mr_url` and `forge=github` on the
bead explicitly. Without it the branch name is how a PR gets attached: launched agents already use it.
A PR from a branch that does not follow the convention needs the bead id in its body, or it is ignored.

### What You See

A GitHub event lands in three places:

- **Bead fields.** The PR's merge-request bead carries `mr_url`, `mr_state` (`opened`, `closed`,
  `merged`, the same words GitLab uses), `mr_outcome`, `mr_pipeline_status`, `mr_pipeline_url`, branches,
  title and author, plus `forge=github` so the viewer labels the link. The task bead gets `mr_url`,
  `forge`, and `mr_has_review_comments=true` when a reviewer asks for something.
- **The agent.** Review comments, change requests, and closed or reopened PRs arrive as a normal-priority
  mail: the agent reads it on its next turn, it does not interrupt one. Local agents get the same mail
  as pod agents.
- **The agent's Slack thread.** One line per event: PR opened (or draft, reopened, ready for review,
  merged by someone, closed without merging), CI passed or failed with a link to the run, and each review
  comment quoted with a link to the exact comment. Pushes and title edits update the bead and stay quiet.

### GitHub Bridge Features

- **PR sync**: `pull_request` events update the bead fields above.
- **CI status**: a completed `check_suite` or `workflow_run` sets `mr_pipeline_status` using the GitLab
  vocabulary (`success`, `failed`, `canceled`, `skipped`, `manual`). A thread line is posted when the
  verdict changes, so a failure is never silent. With several workflows on one PR the field shows the most
  recent completed run, not a rollup.
- **Review forwarding**: submitted reviews, inline review comments and PR conversation comments are
  forwarded to the agent. Comments from bots, including the bridge's own App, are dropped so agents never
  react to their own squawks.
- **Squawk forwarding**: `gb squawk --to-mr` posts the squawk as a PR comment, once, even if the event
  is replayed.
- **Agent resolver**: when a PR has unresolved review threads and its agent is gone, a worker is
  respawned on the task with the threads listed in its first prompt (by default at most three attempts per set of
  threads; then the Slack thread gets a "needs a human" line).

Squawks and the resolver need the GitHub App configured with an app id. Without one, the bridge still
receives and verifies webhooks and updates beads.

### Setup

Operators: register the App and enable `githubBridge` per the
[github-bridge runbook](../../gardenio/docs/runbooks/github-bridge.md). The chart's
`slackBridge.github` block is unrelated: it is a legacy `/unreleased` source that no template reads, not
this integration.

### Releases

A calver tag push is what releases: `images.yml` on GitHub Actions builds and publishes every image to
the deployment's container registry as `:<calver>`. Nothing deploys automatically; the cluster is
upgraded by hand. See `gardenio/docs/release-pipeline.md`.

## GitHub Actions Pipelines

GitHub Actions is the only CI. The workflows live in `.github/workflows/`:

| Workflow | Runs on | What it does |
|---|---|---|
| `ci.yml` | PRs and pushes to `main` | Go (controller, kbeads) and coop build + test, path-filtered per component. A PR runs only the light tier (build + vet + changed-package tests, `cargo check`); the full suite runs on `main`, nightly and on dispatch |
| `checks.yml` | PRs and pushes to `main` | golangci-lint, shell guards, terraform, helm lint + chart tests, secret and security scans, cargo audit/deny. On a PR only its gate and the required `checks` job run; the jobs run on `main`, nightly and on dispatch |
| `images.yml` | calver tags, dispatch | Never runs on a PR; a calver tag push publishes every image as `:<calver>` (plus `:latest` and `:<sha>`) to `<your-registry>/gardenio/<image>`. Dispatch takes an `images` list to rebuild only some |
| `e2e.yml` | Nightly cron, dispatch (no PR trigger) | Optional end-to-end suites; never a required check |
| `retry-lost-runners.yml` | Failed runs | Re-runs jobs whose spot runner was reclaimed mid-job |

### Dispatching E2E

`e2e.yml` is optional and never blocks a merge. Pull requests run the light tier only, so there is no
PR label any more: dispatch it, on main or on your branch (`--ref <branch>`), with a `suites` list (leave
it empty to run all; unknown names are dropped, so a typo selects nothing):

```bash
gh workflow run e2e.yml -f suites="viewer coop"
```

Suites: `coop`, `shell-hermetic`, `viewer`, `go-integration`, `go-e2e`, `live-smoke`, `live-shards`.

## Release Flow

A release is a calver tag on `main`'s head. The tag, not `main`, is what CI tests: `ci.yml`
and `checks.yml` run on the tag push (not on `main` pushes), beside `images.yml`:

```bash
# Preview which commit would be tagged
gardenio/scripts/release-cut.sh --dry-run
```

**Release pipeline:**
1. The tag push makes `images.yml` publish every image as `:<calver>`, and runs `ci.yml` and
   `checks.yml` in full on the tag
2. `gardenio/scripts/release-deploy.sh` (the `release-deployer` CronJob) rolls the tag with
   `--set global.imageTag=<calver>` only once all three of the tag's runs are green; a red tag is
   never rolled. It files one escalation per red workflow, and the fix is a quarantine row on `main`
   plus a re-run of the tag's failed jobs, or a revert on `main` and a new cut

Only the release captain or the operator cuts a tag. See `gardenio/docs/release-pipeline.md` for the
full runbook.

## Unreleased Changes

```bash
/unreleased    # Slack command: show commits since last release
```
