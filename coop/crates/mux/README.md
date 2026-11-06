# Coopmux

PTY multiplexing proxy for coop instances. Coopmux acts as a central hub that:
- Manages multiple coop session instances
- Provides a unified web dashboard for monitoring sessions
- Distributes credentials to sessions via a credential broker
- Supports launching new sessions on-demand (local or K8s)

## Launch API

### `POST /api/v1/sessions/launch`

Spawns a new session via the configured launch command.

#### Request Body (optional)

```json
{
  "env": {
    "GIT_REPO": "https://github.com/user/repo",
    "GIT_BRANCH": "main",
    "WORKING_DIR": "/workspace/repo"
  }
}
```

The `env` field is optional. If omitted or empty, the session launches with default environment variables.

#### Supported Environment Variables

The following environment variables are commonly used:

- **`GIT_REPO`**: Git repository URL (used by k8s-launch.sh to clone into session pod)
- **`GIT_BRANCH`**: Git branch to checkout (default: `main`)
- **`WORKING_DIR`**: Working directory for the session
  - Local: inherited from coopmux process (can be set via shell: `cd /path && coopmux ...`)
  - Kubernetes: default `/workspace`, customizable via this variable

Additional custom variables can be passed and will be available to the launch command script.

#### Reserved Variables

The following environment variables are **reserved by the system** and cannot be overridden by user-supplied env vars:

**System variables:**
- `COOP_MUX_URL` — mux server URL
- `COOP_MUX_TOKEN` — authentication token for mux
- `COOP_URL` — individual session URL
- `COOP_SESSION_ID` — session identifier

**Kubernetes variables:**
- `POD_NAME` — K8s pod name (downward API)
- `POD_NAMESPACE` — K8s namespace (downward API)
- `POD_IP` — K8s pod IP address (downward API)
- `POD_UID` — K8s pod UID (downward API)

**Credential variables (managed by credential broker):**
- `ANTHROPIC_API_KEY` — Anthropic API key
- `CLAUDE_CODE_OAUTH_TOKEN` — Claude OAuth token
- `OPENAI_API_KEY` — OpenAI API key
- `GEMINI_API_KEY` — Google Gemini API key

Credential variables are always injected with **highest priority** and cannot be overridden.

#### Environment Variable Precedence

When launching a session, environment variables are set in this order:

1. **User-supplied vars** (from request body) — filtered to remove reserved keys
2. **System vars** (mux URL, token) — can override user vars
3. **Credentials** (from broker) — highest priority, cannot be overridden

#### Response

```json
{
  "launched": true
}
```

Returns HTTP 200 on success, 400 if launch command is not configured, or 500 if spawn fails.

### `GET /api/v1/config/launch`

Check if launch functionality is available.

#### Response

```json
{
  "available": true
}
```

## Configuration

Launch command is configured via CLI flag or environment variable:

```bash
coopmux --launch "coop --port 0 --log-format text -- claude"
# or
COOP_MUX_LAUNCH="coop --port 0 -- claude" coopmux
```

The launch command:
- Executes via `sh -c` (full shell scripting supported)
- Receives environment variables: `COOP_MUX_URL`, `COOP_MUX_TOKEN`, credentials, and user-supplied vars
- Should spawn a coop session that auto-registers with the mux

## Examples

### Local Launch with Working Directory

```bash
# Start coopmux
coopmux --launch 'cd "${WORKING_DIR:-.}" && coop --port 0 -- claude'

# Launch via API
curl -X POST http://localhost:9800/api/v1/sessions/launch \
  -H "Content-Type: application/json" \
  -d '{"env": {"WORKING_DIR": "/Users/me/projects/myapp"}}'
```

### Kubernetes Launch with Git Clone

See `deploy/README.md` for Kubernetes-specific configuration and the `k8s-launch.sh` script.

## See Also

- [Deploy README](../../deploy/README.md) — Kubernetes deployment and k8s-launch.sh documentation
