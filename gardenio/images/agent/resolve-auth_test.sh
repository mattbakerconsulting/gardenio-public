#!/bin/bash
# resolve-auth_test.sh — Integration tests for resolve-auth.sh
#
# Mocks kd and curl to test _resolve_account_auth without a real daemon.
# Run: bash resolve-auth_test.sh
#
# Exit code 0 = all tests passed, non-zero = failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PASS=0
FAIL=0
TESTS_RUN=0

# ── Test helpers ─────────────────────────────────────────────────────────

_setup() {
    # Reset all state between tests.
    unset ANTHROPIC_BASE_URL CLAUDE_MODEL CLAUDE_MAX_BUDGET ANTHROPIC_API_KEY 2>/dev/null || true
    ACCOUNT_CREDS_SET=0
    MOCK_MODE=0
    BOAT_PROJECT=""
    CREDS_PVC=$(mktemp)
    rm -f "${CREDS_PVC}"

    # Source the script under test.
    source "${SCRIPT_DIR}/resolve-auth.sh"
}

_teardown() {
    rm -f "${CREDS_PVC}" 2>/dev/null || true
    # Remove mock bins from PATH.
    unset -f kd curl 2>/dev/null || true
}

_assert_eq() {
    local label="$1" expected="$2" actual="$3"
    if [ "${expected}" = "${actual}" ]; then
        return 0
    fi
    echo "  FAIL: ${label}: expected '${expected}', got '${actual}'"
    return 1
}

_assert_file_exists() {
    local label="$1" path="$2"
    if [ -f "${path}" ]; then
        return 0
    fi
    echo "  FAIL: ${label}: file '${path}' does not exist"
    return 1
}

_assert_file_missing() {
    local label="$1" path="$2"
    if [ ! -f "${path}" ]; then
        return 0
    fi
    echo "  FAIL: ${label}: file '${path}' should not exist"
    return 1
}

_run_test() {
    local name="$1"
    TESTS_RUN=$((TESTS_RUN + 1))
    _setup
    if "$name" 2>/dev/null; then
        PASS=$((PASS + 1))
        echo "  ok  ${name}"
    else
        FAIL=$((FAIL + 1))
        echo "  FAIL ${name}"
    fi
    _teardown
}

# ── Mock kd responses ────────────────────────────────────────────────────

# Mock kd that returns auth config + account beads.
_mock_kd_with_account() {
    kd() {
        case "$*" in
            *"--type=config"*)
                cat <<'JSON'
[{"id":"gd-auth1","kind":"config","type":"config","title":"auth","description":"{\"account\":\"acme-team\"}","status":"open","labels":["project:testproj"]}]
JSON
                ;;
            *"--type=account"*)
                cat <<'JSON'
[{"id":"gd-acct1","kind":"config","type":"account","title":"acme-team","status":"open","fields":{"provider":"anthropic","base_url":"","credential":"env:MY_API_KEY","model":"claude-opus-4-6","max_budget":"10.00","fallback":"backup-acct"}}]
JSON
                ;;
            *)
                echo "[]"
                ;;
        esac
    }
}

# Mock kd that returns auth config pointing to coopmux account.
_mock_kd_coopmux_account() {
    kd() {
        case "$*" in
            *"--type=config"*)
                cat <<'JSON'
[{"id":"gd-auth1","kind":"config","type":"config","title":"auth","description":"{\"account\":\"acme-team\"}","status":"open","labels":["project:testproj"]}]
JSON
                ;;
            *"--type=account"*)
                cat <<'JSON'
[{"id":"gd-acct1","kind":"config","type":"account","title":"acme-team","status":"open","fields":{"provider":"anthropic","base_url":"","credential":"coopmux:acme-team","model":"claude-opus-4-6"}}]
JSON
                ;;
            *)
                echo "[]"
                ;;
        esac
    }
}

# Mock kd that returns auth config pointing to account with base_url.
_mock_kd_litellm_account() {
    kd() {
        case "$*" in
            *"--type=config"*)
                cat <<'JSON'
[{"id":"gd-auth1","kind":"config","type":"config","title":"auth","description":"{\"account\":\"backup-acct\"}","status":"open","labels":["project:testproj"]}]
JSON
                ;;
            *"--type=account"*)
                cat <<'JSON'
[{"id":"gd-acct2","kind":"config","type":"account","title":"backup-acct","status":"open","fields":{"provider":"anthropic","base_url":"https://litellm.example.com","credential":"secret:litellm-key","model":"claude-sonnet-4-6","fallback":"acme-team"}}]
JSON
                ;;
            *)
                echo "[]"
                ;;
        esac
    }
}

# Mock kd that returns no config beads.
_mock_kd_no_auth() {
    kd() {
        echo "[]"
    }
}

# Mock kd that returns config bead but no matching account.
_mock_kd_missing_account() {
    kd() {
        case "$*" in
            *"--type=config"*)
                cat <<'JSON'
[{"id":"gd-auth1","kind":"config","type":"config","title":"auth","description":"{\"account\":\"nonexistent\"}","status":"open","labels":["project:testproj"]}]
JSON
                ;;
            *"--type=account"*)
                echo "[]"
                ;;
            *)
                echo "[]"
                ;;
        esac
    }
}

# Mock curl that returns a valid coopmux response.
_mock_curl_coopmux_success() {
    curl() {
        echo '{"claudeAiOauth":{"accessToken":"test-token-123"}}'
    }
}

# ── Tests ────────────────────────────────────────────────────────────────

test_no_project_skips() {
    BOAT_PROJECT=""
    _mock_kd_with_account
    _resolve_account_auth
    _assert_eq "ACCOUNT_CREDS_SET" "0" "${ACCOUNT_CREDS_SET}"
}

test_mock_mode_skips() {
    MOCK_MODE=1
    BOAT_PROJECT="testproj"
    _mock_kd_with_account
    _resolve_account_auth
    _assert_eq "ACCOUNT_CREDS_SET" "0" "${ACCOUNT_CREDS_SET}"
}

test_no_auth_bead_falls_through() {
    BOAT_PROJECT="testproj"
    _mock_kd_no_auth
    _resolve_account_auth
    _assert_eq "ACCOUNT_CREDS_SET" "0" "${ACCOUNT_CREDS_SET}"
}

test_missing_account_bead_warns() {
    BOAT_PROJECT="testproj"
    _mock_kd_missing_account
    _resolve_account_auth
    _assert_eq "ACCOUNT_CREDS_SET" "0" "${ACCOUNT_CREDS_SET}"
}

test_env_credential_sets_api_key() {
    BOAT_PROJECT="testproj"
    export MY_API_KEY="sk-test-key-12345"
    _mock_kd_with_account
    _resolve_account_auth
    _assert_eq "ACCOUNT_CREDS_SET" "1" "${ACCOUNT_CREDS_SET}" &&
    _assert_eq "ANTHROPIC_API_KEY" "sk-test-key-12345" "${ANTHROPIC_API_KEY}" &&
    _assert_eq "CLAUDE_MODEL" "claude-opus-4-6" "${CLAUDE_MODEL}" &&
    _assert_eq "CLAUDE_MAX_BUDGET" "10.00" "${CLAUDE_MAX_BUDGET}"
    local rc=$?
    unset MY_API_KEY
    return ${rc}
}

test_env_credential_empty_base_url_unsets() {
    BOAT_PROJECT="testproj"
    export MY_API_KEY="sk-test-key-12345"
    export ANTHROPIC_BASE_URL="https://old.example.com"
    _mock_kd_with_account
    _resolve_account_auth
    # base_url is empty in the account bead → should be unset.
    _assert_eq "ANTHROPIC_BASE_URL unset" "" "${ANTHROPIC_BASE_URL:-}"
    local rc=$?
    unset MY_API_KEY
    return ${rc}
}

test_litellm_account_sets_base_url() {
    BOAT_PROJECT="testproj"
    _mock_kd_litellm_account
    # Secret mount won't exist in test, so creds won't be set,
    # but base_url and model should be applied.
    _resolve_account_auth
    _assert_eq "ANTHROPIC_BASE_URL" "https://litellm.example.com" "${ANTHROPIC_BASE_URL:-}" &&
    _assert_eq "CLAUDE_MODEL" "claude-sonnet-4-6" "${CLAUDE_MODEL:-}"
}

test_coopmux_credential_writes_creds_file() {
    BOAT_PROJECT="testproj"
    export COOP_MUX_URL="http://localhost:9999"
    _mock_kd_coopmux_account
    _mock_curl_coopmux_success
    _resolve_account_auth
    _assert_eq "ACCOUNT_CREDS_SET" "1" "${ACCOUNT_CREDS_SET}" &&
    _assert_file_exists "credentials file" "${CREDS_PVC}"
    local rc=$?
    unset COOP_MUX_URL
    return ${rc}
}

test_coopmux_no_mux_url_warns() {
    BOAT_PROJECT="testproj"
    unset COOP_MUX_URL 2>/dev/null || true
    _mock_kd_coopmux_account
    _resolve_account_auth
    _assert_eq "ACCOUNT_CREDS_SET" "0" "${ACCOUNT_CREDS_SET}"
}

# ── Run all tests ────────────────────────────────────────────────────────

echo "=== resolve-auth.sh tests ==="

_run_test test_no_project_skips
_run_test test_mock_mode_skips
_run_test test_no_auth_bead_falls_through
_run_test test_missing_account_bead_warns
_run_test test_env_credential_sets_api_key
_run_test test_env_credential_empty_base_url_unsets
_run_test test_litellm_account_sets_base_url
_run_test test_coopmux_credential_writes_creds_file
_run_test test_coopmux_no_mux_url_warns

echo ""
echo "${TESTS_RUN} tests: ${PASS} passed, ${FAIL} failed"

if [ "${FAIL}" -gt 0 ]; then
    exit 1
fi
exit 0
