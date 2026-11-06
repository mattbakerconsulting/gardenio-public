#!/usr/bin/env bash
# test-slack-ui.sh — E2E runner for the real-user Playwright Slack surface.
#
# Drives the gardenio Slack surface the way a human does: Playwright
# authenticates as a dedicated TEST Slack human user (via a pre-captured
# browser storageState, NOT a scripted login) against a dedicated test
# workspace/app, types/clicks in app.slack.com, and asserts the
# controller/bridge react end-to-end over Socket Mode. This complements the
# outbound-only slackmock + bash Slack scripts (test-slack-{threads,cards,
# dashboard}.sh), which post as the bot and read back but never render the
# real client UI or flow a human-authored event inbound.
#
# Plan:  gardenio/plans/slack-e2e-playwright.md
# Epic:  ka-0000006991 (Real-user Playwright Slack E2E testing)
# Task:  ka-0000006398 (this wiring) · ka-0000004549 (the Playwright scaffold)
# Runbook (provisioning + session refresh): gardenio/docs/slack-e2e-provisioning.md
#
# ── SKIP-WHEN-UNSET CONTRACT ─────────────────────────────────────────
# This module is inert unless the whole real-user rig is present, mirroring
# the SLACK_BOT_TOKEN guard on the sibling bash Slack scripts. It SKIP-passes
# (exit 0) — never fails — when any of the following is absent, so local dev,
# fork CI, and every ephemeral namespace that has not opted in stay green:
#
#   1. The Playwright scaffold gardenio/tests/slack-e2e/ (task ka-0000004549).
#   2. The captured test-user browser session — K8s Secret `slack-e2e-session`
#      (key `storage-state.json`), materialized by the ESO ExternalSecret only
#      when the ephemeral overlay sets externalSecrets.slackE2ESession.remoteRef
#      (default empty). See gardenio/docs/slack-e2e-provisioning.md Part A7.
#   3. A live slack-bridge in the namespace (BRIDGE_HTTP_URL) — the Socket Mode
#      attach point for the test Slack app. Ephemeral overlays disable the
#      bridge by default (slackBridge.enabled=false).
#   4. node / npx on PATH (the Playwright toolchain).
#
# FAIL-LOUD OVERRIDE (mirrors REQUIRE_OUTBOX_RIG / require-outbox-rig): when
# SLACK_UI_REQUIRE=true, an absent rig is a HARD FAIL instead of a skip. Set it
# only on a SCHEDULED readiness dispatch AFTER the test-Slack secrets are
# provisioned (escalation ka-0000001279), so a silent skip can never masquerade
# as a green run once the harness is meant to be live. Dormant (unset) by
# default — behaviour is unchanged until an operator arms it.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-slack-ui.sh
#   ONLY=slack-ui ./gardenio/tests/e2e/scripts/run-shard.sh   # pinned to shard 2

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Repo root = .../gardenio/tests/e2e/scripts → up 4.
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../../.." && pwd)"
SCAFFOLD_DIR="${REPO_ROOT}/gardenio/tests/slack-e2e"

NAMESPACE="${NAMESPACE:-gardenio-e2e}"
SLACK_UI_REQUIRE="${SLACK_UI_REQUIRE:-}"
# K8s Secret carrying the captured storageState (chart default
# externalSecrets.slackE2ESession.secretName). Overridable for a bespoke deploy.
SLACK_E2E_SESSION_SECRET="${SLACK_E2E_SESSION_SECRET:-slack-e2e-session}"
SLACK_E2E_SESSION_KEY="${SLACK_E2E_SESSION_KEY:-storage-state.json}"

# skip_or_fail <reason> — SKIP-pass by default, HARD-fail when armed.
skip_or_fail() {
  local reason="$1"
  if [ "$SLACK_UI_REQUIRE" = "true" ]; then
    echo "FAIL: SLACK_UI_REQUIRE=true but $reason" >&2
    echo "  The real-user Slack rig is meant to be live on this dispatch." >&2
    echo "  Provision per gardenio/docs/slack-e2e-provisioning.md (escalation ka-0000001279)," >&2
    echo "  or drop require-slack-ui/SLACK_UI_REQUIRE to let it skip cleanly." >&2
    exit 1
  fi
  echo "SKIP: $reason"
  echo "  slack-ui is inert until the real-user test rig is provisioned —"
  echo "  see gardenio/docs/slack-e2e-provisioning.md. This is expected on local,"
  echo "  fork CI, and any ephemeral namespace that has not opted in."
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

# ── Gate 1: Playwright scaffold present (task ka-0000004549) ─────────
if [ ! -f "${SCAFFOLD_DIR}/package.json" ]; then
  skip_or_fail "Playwright scaffold ${SCAFFOLD_DIR#"${REPO_ROOT}"/} not present (task ka-0000004549)"
fi

# ── Gate 2: node / npx toolchain present ────────────────────────────
if ! command -v node >/dev/null 2>&1 || ! command -v npx >/dev/null 2>&1; then
  skip_or_fail "node/npx not on PATH — Playwright toolchain unavailable"
fi

# ── Gate 3: live slack-bridge (Socket Mode attach point) ────────────
# run-shard.sh exports BRIDGE_HTTP_URL (empty when the bridge Service is absent
# from the overlay). A real-user event has nowhere to flow without it.
if [ -z "${BRIDGE_HTTP_URL:-}" ]; then
  skip_or_fail "no slack-bridge in namespace '${NAMESPACE}' (BRIDGE_HTTP_URL empty) — test Slack app has no Socket Mode attach point"
fi

# ── Gate 4: captured test-user session Secret → storageState file ────
# Mirrors the SLACK_BOT_TOKEN guard: the storageState is THE credential. Pull
# it out of the ESO-materialized K8s Secret into a private temp file that
# Playwright loads as its browser storageState.
if ! kubectl -n "$NAMESPACE" get "secret/${SLACK_E2E_SESSION_SECRET}" >/dev/null 2>&1; then
  skip_or_fail "Secret '${SLACK_E2E_SESSION_SECRET}' absent in namespace '${NAMESPACE}' — externalSecrets.slackE2ESession.remoteRef not set (Part A7)"
fi

STORAGE_STATE_DIR="$(mktemp -d)"
STORAGE_STATE_FILE="${STORAGE_STATE_DIR}/storage-state.json"
# Best-effort scrub of the extracted session on exit — it carries the live
# `d`/`d-s` Slack session cookies.
cleanup() {
  rm -f "$STORAGE_STATE_FILE" 2>/dev/null || true
  rmdir "$STORAGE_STATE_DIR" 2>/dev/null || true
}
trap cleanup EXIT

if ! kubectl -n "$NAMESPACE" get "secret/${SLACK_E2E_SESSION_SECRET}" \
      -o "jsonpath={.data.${SLACK_E2E_SESSION_KEY//./\\.}}" 2>/dev/null \
      | base64 -d > "$STORAGE_STATE_FILE" 2>/dev/null || [ ! -s "$STORAGE_STATE_FILE" ]; then
  skip_or_fail "Secret '${SLACK_E2E_SESSION_SECRET}' has no non-empty '${SLACK_E2E_SESSION_KEY}' key — storageState not materialized"
fi

# Sanity: the storageState must carry the Slack session cookies (`d`, `d-s`).
# A stale/blank capture that lacks them means auth will land on the login
# screen — surface that as an EXPIRY pointer, not an opaque selector flake
# (gardenio/docs/slack-e2e-provisioning.md Part B).
if command -v jq >/dev/null 2>&1; then
  if ! jq -e '(.cookies // []) | map(.name) | (index("d") and index("d-s"))' \
        "$STORAGE_STATE_FILE" >/dev/null 2>&1; then
    skip_or_fail "storageState is missing the 'd'/'d-s' Slack session cookies — SESSION EXPIRED or malformed capture; refresh per slack-e2e-provisioning.md Part B"
  fi
fi

echo "════════════════════════════════════════════════════"
echo " slack-ui: real-user Playwright Slack E2E"
echo "   namespace:     ${NAMESPACE}"
echo "   scaffold:      ${SCAFFOLD_DIR#"${REPO_ROOT}"/}"
echo "   bridge:        ${BRIDGE_HTTP_URL}"
echo "   storageState:  ${SLACK_E2E_SESSION_SECRET}[${SLACK_E2E_SESSION_KEY}] (extracted)"
echo "   channel:       ${SLACK_E2E_CHANNEL:-<unset — scaffold resolves from bridge>}"
echo "════════════════════════════════════════════════════"

# Contract handed to the Playwright scaffold (ka-0000004549). The scaffold's
# playwright.config.js / globalSetup read these; kept env-based so this runner
# never has to know the scaffold's internal layout.
export SLACK_E2E_STORAGE_STATE="$STORAGE_STATE_FILE"
export SLACK_E2E_BRIDGE_URL="$BRIDGE_HTTP_URL"
export BEADS_HTTP_URL="${BEADS_HTTP_URL:-http://localhost:19090}"
# SLACK_E2E_CHANNEL / BEADS_AUTH_TOKEN pass through from the environment if set.

cd "$SCAFFOLD_DIR"

# Install deps + browser once per run. Prefer `npm ci` (lockfile-exact) and fall
# back to `npm install` if no lockfile is present in the scaffold yet.
if [ -f package-lock.json ]; then
  npm ci
else
  npm install
fi
npx playwright install --with-deps chromium

# Hand off to the scaffold's own test script. `npm test` maps to
# `playwright test` (mirrors gardenio/tests/beads-viewer-e2e/package.json).
npm test
