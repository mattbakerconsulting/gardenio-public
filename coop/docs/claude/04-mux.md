# Mux Architecture

Coopmux is a multiplexing proxy that aggregates multiple coop sessions behind
a single API. It provides real-time session monitoring, a web dashboard, and
optional credential brokering for OAuth token refresh and distribution.


## 1. Session Registration

### Protocol

Sessions register via `POST /api/v1/sessions`:

```json
{
  "url": "http://localhost:8080",
  "auth_token": "bearer-token-for-this-coop",
  "id": "optional-session-id",
  "metadata": {"label": "worker-1"}
}
```

The mux validates upstream reachability (health check) before accepting.

### Self-Registration

Coop sessions auto-register when `COOP_MUX_URL` is set:

| Variable | Required | Purpose |
|----------|----------|---------|
| `COOP_MUX_URL` | Yes | Mux server base URL |
| `COOP_MUX_TOKEN` | No | Auth token for mux API |
| `COOP_URL` | No | This coop's callback URL (default: `http://127.0.0.1:{port}`) |
| `COOP_MUX_PROFILES` | No | Comma-separated credential account names |

The client retries registration up to 5 times with exponential backoff,
re-registers every 60s as a heartbeat, and deregisters on shutdown.

### Health Monitoring

Mux polls each registered session's health endpoint at
`COOP_MUX_HEALTH_CHECK_MS` (default 10s). After `COOP_MUX_MAX_HEALTH_FAILURES`
(default 3) consecutive failures, the session is evicted.


## 2. Event Feed

### Three-Tier Monitoring

| Tier | When | Resources | Data |
|------|------|-----------|------|
| **Registered** | Always | Health poll only | Alive/dead |
| **Dashboard-visible** | `/ws/mux` client subscribes | Event feed + screen poller | State transitions + screen |
| **Focused** | `/ws/{id}` client connects | WS bridge | Real-time PTY bytes |

Event feeds and screen pollers are **lazy** — started when a `/ws/mux` client
subscribes to a session, stopped when the last subscriber disconnects.

### MuxEvent Types

| Type | Fields | Source |
|------|--------|--------|
| `state` | `session`, `prev`, `next`, `seq` | Upstream WS state subscription |
| `session_online` | `session`, `url` | Registration |
| `session_offline` | `session` | Deregistration or health eviction |

Events are broadcast via a `tokio::sync::broadcast` channel (capacity 256).


## 3. Dashboard

`GET /mux` serves an embedded HTML dashboard with:

- Grid of terminal tiles (one per session)
- Color-coded state badges
- Click-to-focus for full-screen view
- Auto-reconnecting WebSocket to `/ws/mux`

### Multiplexed WebSocket (`/ws/mux`)

Client messages:

| Type | Payload | Effect |
|------|---------|--------|
| `subscribe` | `{"sessions": ["id1", "id2"]}` | Start watching sessions |
| `unsubscribe` | `{"sessions": ["id1"]}` | Stop watching sessions |

Server messages:

| Type | Payload |
|------|---------|
| `sessions` | Full session list (sent on connect) |
| `event` | `MuxEvent` (state transitions, online/offline) |
| `screen_batch` | `[{session, screen}]` (periodic, 1-2 Hz) |
| `error` | Error description |


## 4. Credential Brokering

Optional feature activated by `--credential-config <path>` (env:
`COOP_MUX_CREDENTIAL_CONFIG`). Manages OAuth token refresh and distributes
fresh credentials to registered sessions as profiles.

### Configuration

```json
{
  "accounts": [
    {
      "name": "claude-primary",
      "provider": "claude",
      "env_key": "ANTHROPIC_API_KEY",
      "token_url": "https://claude.ai/oauth/token",
      "client_id": "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
      "auth_url": "https://claude.ai/oauth/authorize",
      "device_auth_url": null
    }
  ]
}
```

| Field | Default | Purpose |
|-------|---------|---------|
| `env_key` | Provider default | Env var name for the credential |
| `auth_url` | Provider default | OAuth authorization endpoint |
| `token_url` | Provider default | OAuth token endpoint |
| `client_id` | Provider default | OAuth client ID |
| `device_auth_url` | None | OAuth device authorization endpoint (RFC 8628) |

For Claude accounts, `auth_url`, `token_url`, and `client_id` default
to the known Claude OAuth endpoints and can be omitted.

When `device_auth_url` is set, the broker uses device code flow instead
of authorization code + PKCE. The user is shown a short code to enter at
the verification URL. A background poll task auto-seeds credentials on
completion.

Provider defaults: `claude` → `ANTHROPIC_API_KEY`, `openai` → `OPENAI_API_KEY`,
`gemini` → `GOOGLE_API_KEY`.

Credentials are persisted to `$COOP_MUX_STATE_DIR/credentials.json` (falling
back to `$XDG_STATE_HOME/coop/mux/` then `$HOME/.local/state/coop/mux/`).

### Refresh Lifecycle

1. **Seed**: initial tokens injected via `POST /api/v1/credentials/seed` or
   loaded from the state directory on startup
2. **Refresh loop**: per-account background task refreshes tokens before expiry
   (exponential backoff on failure: 1s → 60s, 5 retries)
3. **Distribution**: on refresh, credentials are pushed to all registered sessions
4. **Persistence**: after each refresh, tokens are atomically saved to the
   state directory
5. **Re-auth**: if refresh permanently fails, triggers reauth via
   `POST /api/v1/credentials/reauth`. Uses device code flow (RFC 8628) when
   `device_auth_url` is configured, otherwise authorization code + PKCE.
   On `invalid_grant` errors, accounts with `device_auth_url` auto-initiate
   device code reauth.

### Event Channels

Credential events (`CredentialEvent`) flow through the broker's own
`broadcast::Sender<CredentialEvent>` channel — they do **not** flow through
`MuxEvent` (the session feed channel). This is because credential events
originate in the mux broker, not in upstream coop sessions.

The distributor subscribes to `CredentialEvent` and pushes fresh tokens to
matching sessions. Dashboard clients receive credential status updates by
polling `GET /api/v1/credentials/status`.

### Account Status

| Status | Meaning |
|--------|---------|
| `healthy` | Token is valid |
| `refreshing` | Refresh in progress |
| `expired` | Token expired, refresh pending |

### HTTP Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/api/v1/credentials/status` | List accounts with status |
| `POST` | `/api/v1/credentials/seed` | Inject initial tokens |
| `POST` | `/api/v1/credentials/reauth` | Initiate OAuth reauth flow |

All return 400 when credential broker is not configured.


## 5. CLI: `coop cred`

Thin HTTP client for managing mux credentials. Requires `COOP_MUX_URL`.

```sh
coop cred list                           # List accounts and status
coop cred seed <account> --token <tok>   # Seed initial token
coop cred reauth [account]               # Trigger OAuth reauth flow
```


## 6. Environment Variables

### Mux Server

| Variable | Default | Purpose |
|----------|---------|---------|
| `COOP_MUX_HOST` | `127.0.0.1` | Bind host |
| `COOP_MUX_PORT` | `9800` | Bind port |
| `COOP_MUX_AUTH_TOKEN` | None | Bearer token for API auth |
| `COOP_MUX_SCREEN_POLL_MS` | `500` | Screen poller interval |
| `COOP_MUX_STATUS_POLL_MS` | `2000` | Status poller interval |
| `COOP_MUX_HEALTH_CHECK_MS` | `10000` | Health check interval |
| `COOP_MUX_MAX_HEALTH_FAILURES` | `3` | Eviction threshold |
| `COOP_MUX_CREDENTIAL_CONFIG` | None | Path to credential config JSON |
| `COOP_MUX_STATE_DIR` | XDG state dir | State directory for persisted credentials |
| `COOP_MUX_REFRESH_MARGIN_SECS` | `900` | Refresh this many seconds before token expiry |

### Coop Client

| Variable | Purpose |
|----------|---------|
| `COOP_MUX_URL` | Enable mux self-registration |
| `COOP_MUX_TOKEN` | Auth token for mux API |


## 7. Relationship to Profiles

Credentials (mux concern) feed profiles (coop CLI concern):

1. Mux manages token freshness — refresh, persist, distribute
2. Distribution pushes fresh tokens to coop sessions as profiles
   via `POST /api/v1/session/profiles`
3. Coop's profile rotation handles when to switch — on rate-limit errors
   (auto mode) or on explicit API call (manual mode)
4. The `switch` endpoint handles waiting for idle before restarting the agent
