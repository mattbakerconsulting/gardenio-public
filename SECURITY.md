# Security

gardenio runs autonomous coding agents on a Kubernetes cluster and gives
humans a work tracker (beads), a Slack bridge, and an approval path for the
privileged things agents may not do themselves. This file says what we defend
against, what is closed and how we know, and what is still open.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting on this repository
("Security" tab, "Report a vulnerability"). Please do not open a public issue.
We aim to acknowledge within three working days.

## Threat model

| Actor | Assumed able to | Must not be able to |
|---|---|---|
| An agent (including a prompt-injected one) | read and write beads in its own project, run code in its own pod, call the Slack bridge's agent endpoints, start a `kd login` device sign-in | approve, reject, or resolve its own escalation; mint credentials; act as a human; read the node's cloud role |
| Another pod in the cluster | open TCP connections to cluster services | use the Slack bridge's web API, reach the EC2 metadata service |
| A human with a Slack account in the workspace | press buttons on cards addressed to them | approve escalations, unless that Slack account is linked to their sign-in (off by default, and capped at operator) |
| The repository reader | read all code, history, and docs | find credentials, internal hostnames that grant access, or Secret values |

Out of scope: a compromised cluster administrator, a compromised AWS account,
and the shared Slack agent token (see "What stays open").

## What is closed

| Area | Change | Test that proves it | PR |
|---|---|---|---|
| Agent credentials | Agents hold a per-agent beads token named after the agent; the primary token left agent pods; a provisioning failure refuses the spawn | `TestAgentToken_CannotMintBridgeToken`, `TestProvisionAgentToken_FailureRefusesSpawn`, e2e `per-agent-token-identity` | PR #273, PR #351 |
| Coopmux token | Agents' coopmux token is separate from the beads token | `TestOverlay_PerAgentTokensOn`, `TestCoopmux_DaemonBearerIsMachinePrincipalNotBeadsToken` | PR #351 |
| Relay header | `X-Operated-By` is attribution only, except from a bridge identity | `TestTrustedRelayOperator`, `TestTeamsOf_RelayOnlyFromBridge` | PR #1166 |
| Edge envelopes | Only verified v2 envelopes (with roles) count as a human approver | `TestEscalationApprover_EdgeV1Refused_V2Allowed` | PR #1166 |
| Slack bridge web API | Fails closed without OIDC; web approve/reject removed | `TestBridgeRBAC_NoOIDC_FailsClosed` | PR #272 |
| Secret annotations | No Secret carries `last-applied-configuration`; CI rejects `kubectl apply` of a Secret in scripts and runbooks | `check-secret-last-applied_test.sh`, `lint-kubectl-apply-secret_test.py` | PR #269 |
| EC2 metadata (IMDS) | NetworkPolicy denies pod access; checked from a CI runner pod on every PR | `probe-imds-unreachable.sh` in `script-selftests` | PR #270, PR #352 |
| Viewer admin mode | The viewer fails closed for roleless sessions | (identity lane) | PR #251 |
| Alias self-link | Linking an operator alias is admin-only | (identity lane) | PR #246 |
| Human approval path | The daemon verifies OIDC; an escalation is approved only from a browser session the human started (beads viewer, edge) or a linked Slack account, never from a `kd login` device-grant token, which any agent pod could start | `TestApproveEscalation_DeviceGrantClientRefused`, `TestEscalationApprover_EdgeV1Refused_V2Allowed` | PR #249, PR #1166 |
| Shared static beads token | The daemon ignores `BEADS_AUTH_TOKEN` and accepts only fleet, machine, runner, land, window and OIDC credentials; the old token was proven to get a 401, then rotated, and the `kd-beads-token` Secret deleted (2026-11-05, gd-gdf3L8kjT5) | `TestRetiredStaticToken_RefusedOnHTTPAndGRPC` | PR #1686 |
| Local execution | An escalation run locally names a runner identity (`target_runner`, its own `gr_` bearer); only that runner's owner approves and only the agent joined on that runner executes | `TestApproveEscalation_NonOwnerRefused`, `TestEmitEscalationApprovedMail_ToJoinedAgent_OrParkedForRunner` | PR #1783, PR #1818, PR #1899, PR #1903 |

**Escalations: designed, exercised, enforcement in progress.** The approve,
reject, and result verbs check who is calling (operator, bridge, the dispatched
agent); the daemon is the only writer of approval and result fields; each
approved escalation has one executor chosen and claimed by the daemon;
four-eyes applies to every verb when policy asks for it; approvals and results
write mutation history. The tests are `TestRejectResult_CallerClasses`,
`TestResult_ExecutorIsServerStamped`, `TestEscalationFields_PatchGuard`,
`TestEscalationFields_CreateRefusesServerOnly`, `TestApprove_ClaimsOneDispatch`,
`TestFourEyes_RejectAndResult`, and `TestApprove_RecordsMutationHistory`
(PR #1662, PR #1715, PR #1831, PR #1794, PR #1826, PR #1828). What is not yet
in place is listed below.

## What stays open

- **Slack approval from a linked account** (`SLACK_LINK_ENABLED`) is off by
  default. The escalation changes above that first shipped in release tag
  2026.306.1 (PR #1826, PR #1899, PR #1903) are live in the reference
  deployment, which runs 2026.309.14; a deployment enforces what it runs, not
  what is on `main`.
- **Chart residue for the retired static token**: the github-bridge and
  gitlab-bridge deployments still fall back to
  `.Values.beads.tokenSecret | default "kd-beads-token"`, and
  `externalSecrets.beadsToken` can still render a `kd-beads-token`
  ExternalSecret (`templates/external-secrets.yaml`). The daemon refuses that
  token whatever the chart says (PR #1686), so this is dead config, not an
  open credential.
- **The shared Slack agent token**: every agent pod holds one bridge token that
  admits every `/api/slack/*` route, so any agent can post, upload, and delete
  bot messages and create, archive, and invite to channels. Only the
  conversation and permission-callback routes check the agent's own token
  (`gardenio/docs/slack-bridge-auth.md`).
- **Per-agent tokens are project-scoped**; an agent that must read another
  project needs a different credential (not built).
- **The node's cloud role** holds DNS writes on one zone, read, write, and
  delete on the backup bucket, and scale and terminate on two autoscaling
  groups. IMDS denial is a chart option (`imdsDeny`, off by default) that the
  reference deployment turns on for the namespaces it lists. `kube-system`
  (left open for the cluster autoscaler), unlisted namespaces, pods labelled
  `gardenio.io/imds: allow` (never agent pods), and host-network pods can
  still reach the role.
- **Field-edit endpoints** (`/v1/beads/{id}/fields/...` and
  `/v1/beads/{id}/increment`) apply the escalation and user-identity guards but
  not the scrub-request guard.
