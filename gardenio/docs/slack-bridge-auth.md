# Slack bridge: who can call what

| Surface | Who | How it authenticates |
|---|---|---|
| Slack events, interactions, slash commands | Slack | Socket Mode connection with the bot and app tokens |
| `/api/slack/*` | agents | shared bearer `SLACK_BRIDGE_AGENT_TOKEN` (every agent pod holds it as `SLACK_BRIDGE_TOKEN`), or an OIDC admin |
| `POST /api/conversation/say`, `GET /api/conversation/thread`, legacy `GET /api/slack/threads`, `POST /api/permission/callback` | agents | the agent's own per-agent daemon token in `X-Gardenio-Agent-Token`, verified against the daemon's `GET /v1/me`: the agent and land are the token's, and a `?agent=` or body agent naming another agent is refused (gd-P5wjHDweih). Else the shared bearer, whose self-named agent reaches operator lands only (an agent or a target in a tenant land is refused); `SLACK_BRIDGE_SHARED_BEARER_AGENTS=0` refuses the bearer on these routes altogether (Lands 10.3). Else an OIDC admin. |
| `/api/escalations`, `/api/decisions`, `/api/unreleased` | humans | OIDC bearer verified by the bridge (`OIDC_ISSUER_URL`). **Without OIDC these routes return 403.** `SLACK_BRIDGE_INSECURE_OPEN_API=1` opens them for local development only. |
| `/api/escalations/{id}/approve`, `/reject` | nobody | removed (410). Approve in the Slack card or the beads viewer. |
| Permission prompt Allow / Deny (Slack button) | humans | the clicker's Slack user id, mapped to a person (a linked account, else the one user bead with that `slack_id`), who must be an operator or a member of the request's land (`conversation.ActionPermissionAnswer`): `gardenio-admin` anywhere; a `gardenio-project-member` by the land's `/project-<land>` group; any other `gardenio-operator` anywhere. Roles and groups come from Keycloak; with no Keycloak client a mapped person answers in operator lands only. The answer records `answered_by` (`oidc:<email>`) and `answered_by_slack_id`, never the Slack username (gd-eemeNhUt5I). |
| Bridge to daemon | the bridge | fleet token `slack-bridge`; `X-Operated-By` relays the Slack user for attribution and, because `slack-bridge` is a bridge subject, for team and decision attribution |

Known residual: any agent can use `/api/slack/*` to post, upload, and delete bot
messages, because the agent token is shared. See SECURITY.md.
