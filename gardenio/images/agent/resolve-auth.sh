#!/bin/bash
# resolve-auth.sh — Resolve account bead credentials for agent auth.
#
# Sourceable script providing _resolve_account_auth().  Called from
# entrypoint.sh after daemon connection is established and before the
# existing credential provisioning block.
#
# When an account bead is resolved, this script:
#   - Sets/unsets ANTHROPIC_BASE_URL based on the account's base_url field
#   - Sets CLAUDE_MODEL if the account defines a model
#   - Fetches credentials via coopmux, K8s secret mount, or env var
#   - Writes .credentials.json to $CREDS_PVC (same path as entrypoint.sh)
#   - Sets ACCOUNT_CREDS_SET=1 so entrypoint.sh skips its default provisioning
#
# Required globals (set by entrypoint.sh before sourcing):
#   BOAT_PROJECT   — current project name
#   CREDS_PVC      — path to write .credentials.json
#   MOCK_MODE      — "1" to skip entirely
#
# Dependencies: kd, jq, curl (all present in agent image).

# Exported result flag — entrypoint.sh checks this.
ACCOUNT_CREDS_SET=0

_resolve_account_auth() {
    # Skip in mock mode or if no project is set.
    if [ "${MOCK_MODE:-0}" = "1" ]; then
        return 0
    fi
    if [ -z "${BOAT_PROJECT:-}" ]; then
        echo "[resolve-auth] No BOAT_PROJECT set, skipping account resolution"
        return 0
    fi
    if ! command -v kd &>/dev/null; then
        echo "[resolve-auth] kd not available, skipping account resolution"
        return 0
    fi
    if ! command -v jq &>/dev/null; then
        echo "[resolve-auth] jq not available, skipping account resolution"
        return 0
    fi

    # ── Step 1: Find config:auth bead for this project ─────────────────
    #
    # Config beads have type=config. The auth config bead has title="auth"
    # and a label "project:<name>".  Its description is a JSON object
    # containing {"account": "<account-bead-title>"}.

    local auth_beads
    auth_beads=$(kd list --type=config -s open -l "project:${BOAT_PROJECT}" --all-projects --json --limit 50 2>/dev/null) || true

    if [ -z "${auth_beads}" ] || [ "${auth_beads}" = "[]" ]; then
        echo "[resolve-auth] No config beads for project:${BOAT_PROJECT}, using default auth"
        return 0
    fi

    local account_name
    account_name=$(echo "${auth_beads}" | jq -r '
        [.[] | select(.title == "auth")] | first |
        if . == null then "" else .description end
    ' 2>/dev/null | jq -r '.account // empty' 2>/dev/null) || true

    if [ -z "${account_name}" ]; then
        echo "[resolve-auth] No config:auth bead for project:${BOAT_PROJECT}, using default auth"
        return 0
    fi

    echo "[resolve-auth] Found auth config for project:${BOAT_PROJECT} → account: ${account_name}"

    # Export BOAT_ACCOUNT so coop and coopmux registration scope API
    # calls to this project's account instead of round-robining all.
    export BOAT_ACCOUNT="${account_name}"
    echo "[resolve-auth] Set BOAT_ACCOUNT=${account_name}"

    # ── Step 2: Resolve the account bead by title ──────────────────────

    local account_beads account_json
    account_beads=$(kd list --type=account -s open --all-projects --json --limit 50 2>/dev/null) || true

    if [ -z "${account_beads}" ] || [ "${account_beads}" = "[]" ]; then
        echo "[resolve-auth] WARNING: No account beads found"
        return 0
    fi

    account_json=$(echo "${account_beads}" | jq -r --arg name "${account_name}" '
        [.[] | select(.title == $name)] | first // empty
    ' 2>/dev/null) || true

    if [ -z "${account_json}" ] || [ "${account_json}" = "null" ]; then
        echo "[resolve-auth] WARNING: Account bead '${account_name}' not found"
        return 0
    fi

    # ── Step 3: Extract fields ─────────────────────────────────────────

    local provider base_url credential model max_budget
    provider=$(echo "${account_json}" | jq -r '.fields.provider // empty')
    base_url=$(echo "${account_json}" | jq -r '.fields.base_url // empty')
    credential=$(echo "${account_json}" | jq -r '.fields.credential // empty')
    model=$(echo "${account_json}" | jq -r '.fields.model // empty')
    max_budget=$(echo "${account_json}" | jq -r '.fields.max_budget // empty')

    echo "[resolve-auth] Account '${account_name}': provider=${provider}, credential=${credential%%:*}:***"

    # ── Step 4: Apply base_url ─────────────────────────────────────────

    if [ -n "${base_url}" ]; then
        export ANTHROPIC_BASE_URL="${base_url}"
        echo "[resolve-auth] Set ANTHROPIC_BASE_URL=${base_url}"
    elif [ -n "${ANTHROPIC_BASE_URL:-}" ]; then
        # Keep the controller's ANTHROPIC_BASE_URL (e.g. LiteLLM fallback URL).
        # Only unset if the account bead explicitly needs direct-to-provider routing,
        # which would be indicated by a non-empty base_url field, not by its absence.
        echo "[resolve-auth] Kept existing ANTHROPIC_BASE_URL=${ANTHROPIC_BASE_URL}"
    else
        echo "[resolve-auth] No ANTHROPIC_BASE_URL set (direct to provider)"
    fi

    # ── Step 5: Apply model override ───────────────────────────────────

    if [ -n "${model}" ]; then
        export CLAUDE_MODEL="${model}"
        echo "[resolve-auth] Set CLAUDE_MODEL=${model}"
    fi

    # ── Step 6: Apply max_budget ───────────────────────────────────────

    if [ -n "${max_budget}" ]; then
        export CLAUDE_MAX_BUDGET="${max_budget}"
        echo "[resolve-auth] Set CLAUDE_MAX_BUDGET=${max_budget}"
    fi

    # ── Step 7: Resolve credential ─────────────────────────────────────

    # When ANTHROPIC_API_KEY is already set (e.g. LiteLLM virtual key from
    # controller), skip OAuth credential provisioning. OAuth sends
    # Authorization: Bearer which LiteLLM forwards to the upstream provider,
    # causing 403 errors (Anthropic/Bedrock expects x-api-key, not Authorization).
    if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
        echo "[resolve-auth] ANTHROPIC_API_KEY set (LiteLLM mode) — skipping credential resolution"
        ACCOUNT_CREDS_SET=1
        return 0
    fi

    if [ -z "${credential}" ]; then
        echo "[resolve-auth] WARNING: No credential on account '${account_name}'"
        return 0
    fi

    local cred_type cred_value
    cred_type="${credential%%:*}"
    cred_value="${credential#*:}"

    case "${cred_type}" in
        coopmux)
            _resolve_cred_coopmux "${cred_value}"
            ;;
        secret)
            _resolve_cred_secret "${cred_value}"
            ;;
        env)
            _resolve_cred_env "${cred_value}"
            ;;
        *)
            echo "[resolve-auth] WARNING: Unknown credential type '${cred_type}'"
            ;;
    esac
}

# Fetch OAuth token from coopmux distribute endpoint.
_resolve_cred_coopmux() {
    local account="$1"
    if [ -z "${COOP_MUX_URL:-}" ]; then
        echo "[resolve-auth] WARNING: COOP_MUX_URL not set, cannot fetch coopmux credentials"
        return 0
    fi

    local mux_auth="${COOP_MUX_AUTH_TOKEN:-${COOP_BROKER_TOKEN:-}}"
    local mux_creds
    mux_creds=$(curl -sf "${COOP_MUX_URL}/api/v1/credentials/distribute" \
        ${mux_auth:+-H "Authorization: Bearer ${mux_auth}"} \
        -H 'Content-Type: application/json' \
        -d "{\"session_id\":\"${HOSTNAME:-$(hostname)}\",\"account\":\"${account}\"}" 2>/dev/null) || true

    if [ -n "${mux_creds}" ] && echo "${mux_creds}" | jq -e '.claudeAiOauth.accessToken' >/dev/null 2>&1; then
        echo "${mux_creds}" > "${CREDS_PVC}"
        echo "[resolve-auth] Seeded credentials from coopmux (account: ${account})"
        # Unset API key when OAuth is present to avoid Claude's confirmation prompt.
        if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
            unset ANTHROPIC_API_KEY
            echo "[resolve-auth] Unset ANTHROPIC_API_KEY (OAuth takes precedence)"
        fi
        ACCOUNT_CREDS_SET=1
    else
        echo "[resolve-auth] WARNING: coopmux distribute failed for account '${account}'"
    fi
}

# Read API key from mounted K8s secret.
_resolve_cred_secret() {
    local secret_name="$1"
    # Convention: secrets are mounted at /var/run/secrets/<name>/token
    # by the pod manager when the account bead's credential references a secret.
    local secret_path="/var/run/secrets/${secret_name}/token"

    if [ -f "${secret_path}" ]; then
        export ANTHROPIC_API_KEY
        ANTHROPIC_API_KEY=$(cat "${secret_path}")
        echo "[resolve-auth] Loaded API key from K8s secret: ${secret_name}"
        ACCOUNT_CREDS_SET=1
    else
        echo "[resolve-auth] WARNING: Secret not mounted at ${secret_path}"
    fi
}

# Read API key from an existing environment variable.
_resolve_cred_env() {
    local var_name="$1"
    local env_val="${!var_name:-}"

    if [ -n "${env_val}" ]; then
        export ANTHROPIC_API_KEY="${env_val}"
        echo "[resolve-auth] Loaded API key from env var: ${var_name}"
        ACCOUNT_CREDS_SET=1
    else
        echo "[resolve-auth] WARNING: Env var '${var_name}' is empty"
    fi
}
