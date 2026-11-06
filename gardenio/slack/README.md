# Slack App Manifest

`manifest.json` is the single source of truth for the gardenio Slack app
(scopes, slash commands, event subscriptions, Socket Mode). Apply it to a
workspace via **api.slack.com → Your Apps → (app) → App Manifest**, or with the
[App Manifest API](https://api.slack.com/reference/manifests).

## Scopes

| Block | Token | Used by |
|---|---|---|
| `oauth_config.scopes.bot` | `xoxb-…` | The slack-bridge — posting, mentions, reactions, files, DMs. This is the token the bridge runs on. |
| `oauth_config.scopes.user` | `xoxp-…` | Acting **as the installing user** — broad workspace access (search, channel admin, profile/DND/reminders/usergroups, etc.). |

### Why a `user` block?

Bots cannot hold certain scopes at all (e.g. `search:read`), and acting *as a
user* is sometimes needed for richer workspace automation. Declaring the scopes
here means a workspace owner gets the full consent screen on install/reinstall —
no manual scope-adding in the app config UI.

### What is NOT here (and why)

`admin.*` scopes (org/workspace administration via the Admin API) are
**deliberately omitted**: they require a **Slack Enterprise Grid** org and an
Org-Admin/Owner installer. On a standard (non-Grid) workspace they cannot be
granted, and including them makes the manifest fail to apply. Everything in the
`user` block above is grantable on a standard paid workspace.

## Applying changes

Editing scopes (bot **or** user) requires a **reinstall** of the app to the
workspace before the new permissions take effect — Slack will not silently
upgrade a token's scopes. After reinstall, the **user token changes**; if you
use it anywhere, refresh it. The **bot token is unaffected by user-scope
changes** but a reinstall is still required to grant them.
