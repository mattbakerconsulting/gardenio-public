# Claude Code Credential Shape

How Claude Code stores authentication state across its config directory
(`~/.claude/` or `$CLAUDE_CONFIG_DIR`).

## Auth Flows

Claude Code supports two OAuth-based authentication flows. Both use OAuth to
authenticate the user, but differ in how the resulting credential is stored.

### Flow A: OAuth Token (`.credentials.json`)

Tokens are stored in a separate `.credentials.json` file.
Claude Code uses the token directly for API calls.

### Flow B: Auto-Created API Key (`primaryApiKey`)

Claude auto-creates an API key via the `org:create_api_key` scope and stores it as `primaryApiKey` in `.claude.json`.
No `.credentials.json` is written. The screen shows "Creating API key for Claude Code…" during this step.

## Onboarding Stages

Both flows share the same `.claude.json` progression:

| Stage | Key fields added |
|-------|-----------------|
| 1. Pre-auth | `cachedGrowthBookFeatures`, `firstStartTime`, `userID`, migration flags |
| 2. Post-auth | `oauthAccount` + credential (see below) |
| 3. Onboarded | `hasCompletedOnboarding`, `lastOnboardingVersion` |
| 4. Trusted | `projects.{workspace}` (trust + tools + MCP), caches |

The credential written at stage 2 depends on the flow:
- **Flow A**: `.credentials.json` with `claudeAiOauth` object
- **Flow B**: `primaryApiKey` + `customApiKeyResponses` in `.claude.json`

## Files

### `.claude.json` -- account metadata and state

Primary config file. The `oauthAccount` field is the identity payload.
Shape varies by billing type:

**Flow A** (stripe subscription, Claude Max):
```json
{
  "accountUuid": "...",
  "emailAddress": "...",
  "organizationUuid": "...",
  "billingType": "stripe_subscription",
  "displayName": "...",
  "organizationRole": "admin",
  "organizationName": "..."
}
```

**Flow B** (prepaid / API billing):
```json
{
  "accountUuid": "...",
  "emailAddress": "...",
  "organizationUuid": "...",
  "hasExtraUsageEnabled": false,
  "billingType": "prepaid",
  "subscriptionCreatedAt": "2025-07-15T20:56:15.592526Z",
  "displayName": "...",
  "organizationRole": "admin",
  "workspaceRole": "workspace_developer",
  "organizationName": "..."
}
```

**Flow B only** — API key fields in `.claude.json`:
```json
{
  "primaryApiKey": "sk-ant-api03-...",
  "customApiKeyResponses": {
    "approved": ["<key-suffix>"],
    "rejected": []
  }
}
```

### `.credentials.json` -- OAuth tokens (Flow A only)

Written after successful OAuth flow. Single top-level key `claudeAiOauth`:

```json
{
  "claudeAiOauth": {
    "accessToken": "sk-ant-oat01-...",
    "refreshToken": "sk-ant-ort01-...",
    "expiresAt": 1770705499628,
    "scopes": ["user:inference", "user:mcp_servers", "user:profile", "user:sessions:claude_code"],
    "subscriptionType": "max",
    "rateLimitTier": "default_claude_max_20x"
  }
}
```

Not created in Flow B.

### `settings.json` -- global settings

Created during onboarding. Defaults to `{}`.

## Environment Variables

| Variable | Purpose |
|----------|---------|
| `CLAUDE_CODE_OAUTH_TOKEN` | OAuth access token (alternative to `.credentials.json`, Flow A) |
| `ANTHROPIC_API_KEY` | API key auth (alternative to `primaryApiKey`, Flow B) |
| `CLAUDE_CONFIG_DIR` | Override config directory location |

## Credential Switch (Issue #36)

The minimum credential switch for a session depends on the flow:

**Flow A (OAuth token):**
1. Replace `.credentials.json` (or set `CLAUDE_CODE_OAUTH_TOKEN` env var)
2. Update `oauthAccount` in `.claude.json` to match the new identity

**Flow B (API key):**
1. Replace `primaryApiKey` in `.claude.json` (or set `ANTHROPIC_API_KEY` env var)
2. Update `oauthAccount` in `.claude.json` to match the new identity

The `hasCompletedOnboarding`, `lastOnboardingVersion`, and `projects.*` fields
can remain from the previous session -- they are per-install, not per-identity.


## Coop Integration

Coop does **not** read `.credentials.json` or `.claude.json` directly. Instead,
credentials are passed to the Claude child process via environment variables
(`ANTHROPIC_API_KEY` for Flow B, `CLAUDE_CODE_OAUTH_TOKEN` for Flow A). Claude
Code's own logic reads from these variables or its config files.

**Session switch** (`POST /api/v1/session/switch`): the orchestrator sends a
`credentials` map (key-value pairs that become env vars). Coop merges these
into the child process environment and respawns with `--resume`, preserving the
conversation.

**Named profiles** (`POST /api/v1/session/profiles`): the orchestrator
registers multiple credential sets. Coop rotates between profiles automatically
on rate-limit errors, parking the session when all profiles are exhausted.
