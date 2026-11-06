#!/usr/bin/env bash
# test-multi-user-e2e.sh — C6 multi-user E2E smoke (gd-jaM1ZC9p0E).
#
# Exercises the invariant that two distinct human operators (matt + dana)
# remain isolated end-to-end across the kbeads → controller → viewer stack
# even when their writes flow through different surface paths (Slack-shaped
# agent stamps vs. OIDC-shaped direct stamps). The four assertions mirror
# the C6 acceptance:
#
#   A1. Server stamps the human id (slack:U…) on the bead's operated_by
#       column when an agent-shaped write arrives with X-Operated-By, not
#       the calling agent's identity. Guards the C2 resolver + sanitizer
#       contract (ka-0000002601).
#
#   A2. /v1/beads?operated_by_alias_in=oidc:<user> returns ONLY that user's
#       beads — confirms multi-user isolation through the operator_aliases
#       lookup path (the same path the viewer uses).
#
#   A3. beads-viewer GET /active?operated_by=<oidc:user> (with a GitLab-direct
#       bearer in the Authorization header) returns each user's bead
#       in the rendered HTML. Pins the viewer's daemon-query wiring.
#
#   A4. Slack ↔ OIDC alias collapse: matt's slack-stamped bead AND matt's
#       OIDC-stamped bead share a single operator id after the admin
#       link-aliases call. Both surface in matt's /active filter even
#       though they were stamped with different (kind,subject) aliases.
#       Guards the C3 alias-collapse path (ka-0000000483).
#
# Bead chain: gd-jaM1ZC9p0E (this impl) → ka-0000006809 (epic: operated_by
# must be human-with-email) → ka-0000002601 (C2 sanitizer) →
# ka-0000000483 (C3 Slack↔OIDC resync).
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-multi-user-e2e.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster (default gardenio-e2e)
#   - kd, jq, curl in PATH
#   - lib-gitlab-token.sh's mint_user_token resolves a GitLab-direct bearer
#     (GARDENIO_E2E_GITLAB_BEARER or the static daemon token must be set). The viewer-side
#     assertions degrade to skip when no bearer is available so the
#     CI matrix only fails on real regressions, not infra outages.
#
# Hard rules: do NOT touch other e2e modules. All created beads carry the
# title prefix `multi-user-e2e-<random>` and are closed in the cleanup
# trap, so reruns do not collide.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gitlab-token.sh"

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
VIEWER_SVC="${VIEWER_SVC:-${RELEASE}-beads-viewer}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19610}"
VIEWER_PF_PORT="${VIEWER_PF_PORT:-19611}"
KD_BIN="${KD_BIN:-kd}"

RANDOM_SUFFIX="$$-$(date +%s)-${RANDOM}"
TITLE_PREFIX="multi-user-e2e"

# Matt + Dana are synthetic operator ids. BOTH have their slack id AND
# email scoped to this run via RANDOM_SUFFIX so the test is self-isolating
# — it can run in parallel with itself and, crucially, against the
# persistent staging singleton namespace whose operator_aliases table
# accumulates across runs.
#
# Why matt's slack id must NOT be the fixed real id (gd-nDzilk3Wc5):
# multi-user-e2e runs against a long-lived namespace. link_aliases pins a
# single operator's canonical (kind,subject) to the surviving oidc alias.
# If matt always reused the SAME slack id (U0OPERATOR1) while his email
# rotated per run, every run linked the shared slack id to a NEW oidc
# email — and the operator row's canonical oidc subject was whatever a
# PRIOR run last linked. Scenario 1 then read back operated_by as a stale
# `oidc:matt+c6-<old-run>@…`, failing the "this run's alias" assertion
# (cross-run state contamination, root-caused in ka-0000006320). Making
# the slack id per-run-unique gives every run its own fresh operator row,
# so no prior run's alias can ever be the survivor.
#
# Slack user ids are uppercase alnum without dashes; sanitize the suffix
# and pin a length-bounded fallback so a degenerate suffix can't yield an
# empty id.
MATT_SLACK_ID="${MATT_SLACK_ID:-U07MATT${RANDOM_SUFFIX}}"
MATT_SLACK_ID=$(printf '%s' "$MATT_SLACK_ID" | tr -cd 'A-Z0-9' | head -c 11)
[ -n "$MATT_SLACK_ID" ] || MATT_SLACK_ID="U07MATTTEST"
MATT_EMAIL="${MATT_EMAIL:-matt+c6-${RANDOM_SUFFIX}@e2e.example.com}"
DANA_SLACK_ID="${DANA_SLACK_ID:-U07DANA${RANDOM_SUFFIX}}"
DANA_SLACK_ID=$(printf '%s' "$DANA_SLACK_ID" | tr -cd 'A-Z0-9' | head -c 11)
[ -n "$DANA_SLACK_ID" ] || DANA_SLACK_ID="U07DANATEST"
DANA_EMAIL="${DANA_EMAIL:-dana+c6-${RANDOM_SUFFIX}@e2e.example.com}"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
BEADS_PF_PID=""
VIEWER_PF_PID=""
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-multi-user}"

# ── Helpers ───────────────────────────────────────────────────────
red()    { printf '\033[0;31m%s\033[0m\n' "$*"; }
green()  { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()   { printf '\033[0;34m%s\033[0m\n' "$*"; }
yellow() { printf '\033[0;33m%s\033[0m\n' "$*"; }
dim()    { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}
skip() {
  local name="$1" reason="${2:-}"
  yellow "  ⊘ SKIP: $name${reason:+ — $reason}"
  SKIP=$((SKIP + 1))
}

cleanup() {
  for id in ${CREATED_BEADS[@]+"${CREATED_BEADS[@]}"}; do
    "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  [ -n "$BEADS_PF_PID"  ] && kill "$BEADS_PF_PID"  2>/dev/null || true
  [ -n "$VIEWER_PF_PID" ] && kill "$VIEWER_PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

# ── Setup ─────────────────────────────────────────────────────────
setup_beads_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return 0
  fi
  blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
    "${BEADS_PF_PORT}:8080" >/tmp/multi-user-beads-pf.log 2>&1 &
  BEADS_PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${BEADS_PF_PORT}"
  if [ -z "${BEADS_TOKEN:-}" ]; then
    local secret
    secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
    [ -n "$secret" ] && export BEADS_TOKEN="$secret"
  fi
}

setup_viewer_portforward() {
  if ! kubectl -n "$NAMESPACE" get svc "$VIEWER_SVC" >/dev/null 2>&1; then
    yellow "viewer service $VIEWER_SVC not found in $NAMESPACE — viewer assertions will skip"
    return 1
  fi
  blue "Port-forwarding ${NAMESPACE}/${VIEWER_SVC} :${VIEWER_PF_PORT} → 8095..."
  kubectl -n "$NAMESPACE" port-forward "svc/${VIEWER_SVC}" \
    "${VIEWER_PF_PORT}:8095" >/tmp/multi-user-viewer-pf.log 2>&1 &
  VIEWER_PF_PID=$!
  local i
  for i in 1 2 3 4 5 6 7 8 9 10; do
    if curl -sf --max-time 1 -o /dev/null \
        "http://localhost:${VIEWER_PF_PORT}/healthz" 2>/dev/null \
        || curl -s --max-time 1 -o /dev/null \
            "http://localhost:${VIEWER_PF_PORT}/" 2>/dev/null; then
      dim "  Viewer tunnel ready on :${VIEWER_PF_PORT} (pid $VIEWER_PF_PID)"
      return 0
    fi
    sleep 0.5
  done
  yellow "  Viewer port-forward did not become healthy within 5s; continuing"
  return 0
}

# beads_api <METHOD> <PATH> [extra curl args...] — wrapper that always
# sends the admin token + a fixed operator header so write paths satisfy
# the server's non-empty operated_by guard (ka-0000005772).
beads_api() {
  local method="$1" path="$2"
  shift 2
  curl -sS -X "$method" \
    --max-time 15 \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    ${BEADS_TOKEN:+-H "Authorization: Bearer ${BEADS_TOKEN}"} \
    -H "X-Operated-By: ${BEADS_OPERATED_BY:-oidc:e2e-multi-user@example.com}" \
    "$@" \
    "${BEADS_HTTP_URL}${path}"
}

# create_bead_for_user <slack-id> <email> <title-tag> — POSTs a fresh
# in_progress bead under X-Operated-By: slack:<id>. Echoes the bead id on
# stdout, registers it for cleanup, fails the test on error.
create_bead_for_user() {
  local slack_id="$1" email="$2" tag="$3"
  local op="slack:${slack_id}"
  local title="${TITLE_PREFIX}-${tag}-${RANDOM_SUFFIX}"
  local body
  body=$(jq -nc \
    --arg title "$title" \
    --arg desc "C6 multi-user E2E ($tag, op=$op)" \
    '{title:$title,type:"task",status:"in_progress",description:$desc,labels:["e2e:multi-user"]}')
  local resp
  resp=$(curl -sS -X POST \
    --max-time 15 \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    -H "Authorization: Bearer ${BEADS_TOKEN}" \
    -H "X-Operated-By: ${op}" \
    -H "X-Auth-Request-Email: ${email}" \
    --data "$body" \
    "${BEADS_HTTP_URL}/v1/beads" 2>&1 || true)
  local bid
  bid=$(printf '%s' "$resp" | jq -r '.id // empty' 2>/dev/null || true)
  if [ -z "$bid" ]; then
    fail "create $tag bead via $op" "no id in response: $resp"
    return 1
  fi
  CREATED_BEADS+=("$bid")
  printf '%s' "$bid"
}

# create_bead_oidc <email> <title-tag> — POST a bead with X-Operated-By:
# oidc:<email>. Used for the alias-collapse assertion.
create_bead_oidc() {
  local email="$1" tag="$2"
  local op="oidc:${email}"
  local title="${TITLE_PREFIX}-${tag}-${RANDOM_SUFFIX}"
  local body
  body=$(jq -nc \
    --arg title "$title" \
    --arg desc "C6 multi-user E2E ($tag, op=$op)" \
    '{title:$title,type:"task",status:"in_progress",description:$desc,labels:["e2e:multi-user"]}')
  local resp
  resp=$(curl -sS -X POST \
    --max-time 15 \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    -H "Authorization: Bearer ${BEADS_TOKEN}" \
    -H "X-Operated-By: ${op}" \
    -H "X-Auth-Request-Email: ${email}" \
    --data "$body" \
    "${BEADS_HTTP_URL}/v1/beads" 2>&1 || true)
  local bid
  bid=$(printf '%s' "$resp" | jq -r '.id // empty' 2>/dev/null || true)
  if [ -z "$bid" ]; then
    fail "create $tag bead via $op" "no id in response: $resp"
    return 1
  fi
  CREATED_BEADS+=("$bid")
  printf '%s' "$bid"
}

# link_aliases <slack-id> <email> — POST /v1/admin/operators/link-aliases
# to collapse (slack:<id>) + (oidc:<email>) into a single operator row.
# Mirrors the bridge's slack_oidc_operator_alias_resync.go shape.
link_aliases() {
  local slack_id="$1" email="$2"
  local body
  body=$(jq -nc \
    --arg sid "$slack_id" \
    --arg eml "$email" \
    '{aliases:[{kind:"slack",subject:$sid},{kind:"oidc",subject:$eml}],source:"e2e-multi-user"}')
  local resp http
  resp=$(curl -sS -o /tmp/multi-user-link.json \
    -w '%{http_code}' \
    --max-time 15 \
    -X POST \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${BEADS_TOKEN}" \
    -H "X-Operated-By: ${BEADS_OPERATED_BY:-oidc:e2e-multi-user@example.com}" \
    --data "$body" \
    "${BEADS_HTTP_URL}/v1/admin/operators/link-aliases" 2>&1 || true)
  http="$resp"
  local body_out
  body_out=$(cat /tmp/multi-user-link.json 2>/dev/null || printf '{}')
  if [ "$http" != "200" ]; then
    printf 'http=%s body=%s\n' "$http" "$body_out" >&2
    return 1
  fi
  printf '%s' "$body_out"
}

# bead_operated_by <bead-id> — echoes operated_by string for the bead via
# the HTTP API, empty if unset.
bead_operated_by() {
  local bid="$1"
  beads_api GET "/v1/beads/${bid}" 2>/dev/null \
    | jq -r '.operated_by // .bead.operated_by // empty' 2>/dev/null || true
}

# list_operated_by_alias <alias-string> — returns the JSON body of GET
# /v1/beads?operated_by_alias_in=<alias>. Uses status filter so the result
# set is bounded to the in_progress beads this test created.
#
# URL-encodes the alias: OIDC alias subjects are emails which can contain
# `+` (used as our randomized run-suffix delimiter). Without encoding the
# `+` is decoded to a space server-side and the lookup misses.
list_operated_by_alias() {
  local alias="$1"
  local encoded
  encoded=$(printf '%s' "$alias" | jq -sRr @uri)
  curl -sS --max-time 15 \
    -H "Authorization: Bearer ${BEADS_TOKEN}" \
    "${BEADS_HTTP_URL}/v1/beads?status=in_progress&operated_by_alias_in=${encoded}" \
    2>/dev/null || printf '{"beads":[]}'
}

# viewer_get_active <token> <query-string> — GETs /active?<query> with the
# GitLab-direct bearer + ForwardAuth email header. Echoes "<status>\n<body>".
viewer_get_active() {
  local token="$1" qs="$2" email="$3"
  local out status
  out=$(mktemp)
  status=$(curl -sk --max-time 15 \
    -H "Authorization: Bearer ${token}" \
    -H "X-Auth-Request-Email: ${email}" \
    -H "Accept: text/html" \
    -o "$out" \
    -w '%{http_code}' \
    "http://localhost:${VIEWER_PF_PORT}/active?${qs}" 2>/dev/null || echo "000")
  printf '%s\n' "$status"
  cat "$out"
  rm -f "$out"
}

# ── Main ─────────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " C6 multi-user E2E (gd-jaM1ZC9p0E)"
  blue "  matt slack=$MATT_SLACK_ID email=$MATT_EMAIL"
  blue "  dana slack=$DANA_SLACK_ID email=$DANA_EMAIL"
  blue "═══════════════════════════════════════════════════"

  setup_beads_portforward
  local viewer_ok=1
  setup_viewer_portforward || viewer_ok=0

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    fail "kd connectivity" "kd cannot reach beads daemon at ${BEADS_HTTP_URL:-unset}"
    summary
    return 1
  fi

  # ── Pre-link aliases for both users. Doing this BEFORE the writes is
  # the path the production bridge takes (ka-0000000483 resync runs
  # every 6h; in steady state a write lands on an already-linked
  # operator). The collapse-after-write path is covered separately
  # below in Scenario 4.
  blue "Pre-linking (slack,oidc) aliases for matt + dana"
  if ! link_aliases "$MATT_SLACK_ID" "$MATT_EMAIL" >/tmp/multi-user-link-matt.json; then
    fail "link aliases for matt" "$(cat /tmp/multi-user-link.json 2>/dev/null || true)"
    summary
    return 1
  fi
  pass "linked aliases for matt (slack:$MATT_SLACK_ID + oidc:$MATT_EMAIL)"

  if ! link_aliases "$DANA_SLACK_ID" "$DANA_EMAIL" >/tmp/multi-user-link-dana.json; then
    fail "link aliases for dana" "$(cat /tmp/multi-user-link.json 2>/dev/null || true)"
    summary
    return 1
  fi
  pass "linked aliases for dana (slack:$DANA_SLACK_ID + oidc:$DANA_EMAIL)"

  # ══════════════════════════════════════════════════════════════════
  # Scenario 1 (A1): operated_by stamps the human slack id, not the
  # calling agent. Two distinct users each spawn an "agent-shaped"
  # write — server records the human operator on the bead row.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 1: bead.operated_by carries human slack id (not agent id)"
  local matt_slack_bead dana_slack_bead
  matt_slack_bead=$(create_bead_for_user "$MATT_SLACK_ID" "$MATT_EMAIL" "matt-slack")
  dana_slack_bead=$(create_bead_for_user "$DANA_SLACK_ID" "$DANA_EMAIL" "dana-slack")
  if [ -z "$matt_slack_bead" ] || [ -z "$dana_slack_bead" ]; then
    fail "scenario 1: bead creation" "matt=$matt_slack_bead dana=$dana_slack_bead"
    summary
    return 1
  fi
  pass "matt slack-shaped bead created ($matt_slack_bead)"
  pass "dana slack-shaped bead created ($dana_slack_bead)"

  local matt_op dana_op
  matt_op=$(bead_operated_by "$matt_slack_bead")
  dana_op=$(bead_operated_by "$dana_slack_bead")
  # Accept either linked alias as the canonical operated_by. The v2 read
  # path resolves `op.kind || ':' || op.subject` from the operators row
  # the bead points at via operated_by_id (migration 024). When slack +
  # oidc aliases have been pre-linked (as we do above), link-aliases
  # picks oidc as the survivor and pins the operator's canonical
  # (kind,subject) to it (http_admin_operator_aliases_link.go). The
  # operator identity is preserved either way — the slack and oidc
  # aliases point at the same operator id — which is the invariant
  # this scenario is guarding.
  if [ "$matt_op" = "slack:${MATT_SLACK_ID}" ] || [ "$matt_op" = "oidc:${MATT_EMAIL}" ]; then
    pass "matt bead operated_by is a matt-linked alias ($matt_op)"
  else
    fail "matt bead operated_by mismatch" \
      "got '$matt_op', want 'slack:${MATT_SLACK_ID}' or 'oidc:${MATT_EMAIL}'"
  fi
  if [ "$dana_op" = "slack:${DANA_SLACK_ID}" ] || [ "$dana_op" = "oidc:${DANA_EMAIL}" ]; then
    pass "dana bead operated_by is a dana-linked alias ($dana_op)"
  else
    fail "dana bead operated_by mismatch" \
      "got '$dana_op', want 'slack:${DANA_SLACK_ID}' or 'oidc:${DANA_EMAIL}'"
  fi
  # Guard against the regression where the server stamps the agent's
  # identity instead of the human header. KD_ACTOR is the agent name; if
  # it leaks into operated_by, both above would read "e2e-multi-user".
  if [ "$matt_op" = "$KD_ACTOR" ] || [ "$dana_op" = "$KD_ACTOR" ]; then
    fail "operated_by leaked agent identity" \
      "expected human ids, found agent KD_ACTOR=$KD_ACTOR"
  else
    pass "operated_by did not leak agent identity ($KD_ACTOR)"
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 2 (A2): API-level alias filter isolates each user.
  # The daemon's operated_by_alias_in path joins operator_aliases →
  # operator_id → beads. After link-aliases, querying for matt's oidc
  # alias must surface matt's slack-stamped bead AND only matt's.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 2: alias-aware filter isolates each user"
  local matt_alias_resp dana_alias_resp
  matt_alias_resp=$(list_operated_by_alias "oidc:${MATT_EMAIL}")
  dana_alias_resp=$(list_operated_by_alias "oidc:${DANA_EMAIL}")

  if printf '%s' "$matt_alias_resp" | jq -e --arg id "$matt_slack_bead" \
      '.beads | map(.id) | index($id) != null' >/dev/null 2>&1; then
    pass "matt's slack-stamped bead surfaces under oidc:${MATT_EMAIL}"
  else
    fail "matt's slack-stamped bead missing from oidc:${MATT_EMAIL} alias query" \
      "response: $(printf '%s' "$matt_alias_resp" | jq -c '.beads | map(.id)' 2>/dev/null || true)"
  fi
  if printf '%s' "$matt_alias_resp" | jq -e --arg id "$dana_slack_bead" \
      '.beads | map(.id) | index($id) == null' >/dev/null 2>&1; then
    pass "dana's bead does NOT leak into matt's alias query"
  else
    fail "dana bead leaked into matt's alias query" \
      "response: $(printf '%s' "$matt_alias_resp" | jq -c '.beads | map(.id)' 2>/dev/null || true)"
  fi
  if printf '%s' "$dana_alias_resp" | jq -e --arg id "$dana_slack_bead" \
      '.beads | map(.id) | index($id) != null' >/dev/null 2>&1; then
    pass "dana's slack-stamped bead surfaces under oidc:${DANA_EMAIL}"
  else
    fail "dana's slack-stamped bead missing from oidc:${DANA_EMAIL} alias query" \
      "response: $(printf '%s' "$dana_alias_resp" | jq -c '.beads | map(.id)' 2>/dev/null || true)"
  fi
  if printf '%s' "$dana_alias_resp" | jq -e --arg id "$matt_slack_bead" \
      '.beads | map(.id) | index($id) == null' >/dev/null 2>&1; then
    pass "matt's bead does NOT leak into dana's alias query"
  else
    fail "matt bead leaked into dana's alias query" \
      "response: $(printf '%s' "$dana_alias_resp" | jq -c '.beads | map(.id)' 2>/dev/null || true)"
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 3 (A3): beads-viewer /active filtered per OIDC viewer.
  # resolve a GitLab-direct bearer for each operator, GET /active with the
  # ForwardAuth email header, and assert each user sees their own bead.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 3: /active?operated_by=oidc:<user> per-viewer isolation"
  if [ "$viewer_ok" = 0 ]; then
    skip "scenario 3: viewer per-user /active" "viewer service absent"
  else
    local matt_token="" dana_token=""
    if ! matt_token=$(mint_user_token "$MATT_EMAIL" 2>/tmp/multi-user-mint-matt.err); then
      yellow "mint_user_token failed for matt — see /tmp/multi-user-mint-matt.err"
      skip "scenario 3: viewer /active for matt" "no GitLab-direct bearer available"
    elif ! dana_token=$(mint_user_token "$DANA_EMAIL" 2>/tmp/multi-user-mint-dana.err); then
      yellow "mint_user_token failed for dana — see /tmp/multi-user-mint-dana.err"
      skip "scenario 3: viewer /active for dana" "no GitLab-direct bearer available"
    elif ! viewer_accepts_bearer "$matt_token" "/active"; then
      # The viewer's own OIDC middleware stays pinned to the Keycloak issuer
      # until the viewer-login flip lands (ka-0000002083); it rejects this
      # GitLab-direct / static-daemon bearer with 401. SKIP-clean rather than
      # hard-fail the per-viewer /active asserts (bug gd-5M8iT2oNlr). Scenarios
      # 1, 2, 4 in this module are daemon-only and unaffected.
      skip "scenario 3: viewer per-user /active" "viewer rejects the E2E transport bearer — viewer still OIDC-gated at the pod; viewer-login flip pending (ka-0000002083)"
    else
      pass "resolved GitLab-direct bearers for matt + dana"
      local matt_qs="operated_by=$(printf '%s' "oidc:${MATT_EMAIL}" | jq -sRr @uri)"
      local dana_qs="operated_by=$(printf '%s' "oidc:${DANA_EMAIL}" | jq -sRr @uri)"

      local resp_m status_m body_m
      resp_m=$(viewer_get_active "$matt_token" "$matt_qs" "$MATT_EMAIL")
      status_m=$(printf '%s' "$resp_m" | head -n1)
      body_m=$(printf '%s' "$resp_m" | tail -n +2)
      if [ "$status_m" != "200" ]; then
        fail "GET /active as matt" "expected 200, got $status_m"
      else
        if grep -qF -- "$matt_slack_bead" <<<"$body_m"; then
          pass "matt's bead present on matt's /active"
        else
          fail "matt's bead missing from matt's /active" \
            "bead id $matt_slack_bead not in HTML body"
        fi
        if grep -qF -- "$dana_slack_bead" <<<"$body_m"; then
          fail "dana's bead leaked onto matt's /active" \
            "bead id $dana_slack_bead present in HTML body"
        else
          pass "dana's bead NOT visible on matt's /active"
        fi
      fi

      local resp_d status_d body_d
      resp_d=$(viewer_get_active "$dana_token" "$dana_qs" "$DANA_EMAIL")
      status_d=$(printf '%s' "$resp_d" | head -n1)
      body_d=$(printf '%s' "$resp_d" | tail -n +2)
      if [ "$status_d" != "200" ]; then
        fail "GET /active as dana" "expected 200, got $status_d"
      else
        if grep -qF -- "$dana_slack_bead" <<<"$body_d"; then
          pass "dana's bead present on dana's /active"
        else
          fail "dana's bead missing from dana's /active" \
            "bead id $dana_slack_bead not in HTML body"
        fi
        if grep -qF -- "$matt_slack_bead" <<<"$body_d"; then
          fail "matt's bead leaked onto dana's /active" \
            "bead id $matt_slack_bead present in HTML body"
        else
          pass "matt's bead NOT visible on dana's /active"
        fi
      fi
    fi
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 4 (A4): Slack ↔ OIDC alias collapse.
  # matt writes a *second* bead under oidc:<email> directly (the
  # OIDC-driven write path). Because link-aliases collapsed his slack
  # and oidc aliases into one operator id, BOTH beads must surface
  # under either alias.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 4: Slack+OIDC alias collapse — single operator id"
  local matt_oidc_bead
  matt_oidc_bead=$(create_bead_oidc "$MATT_EMAIL" "matt-oidc")
  if [ -z "$matt_oidc_bead" ]; then
    fail "scenario 4: matt OIDC-stamped bead creation" "no id returned"
  else
    pass "matt OIDC-stamped bead created ($matt_oidc_bead)"

    local matt_via_slack matt_via_oidc
    matt_via_slack=$(list_operated_by_alias "slack:${MATT_SLACK_ID}")
    matt_via_oidc=$(list_operated_by_alias "oidc:${MATT_EMAIL}")

    # Both beads under matt's slack alias
    if printf '%s' "$matt_via_slack" | jq -e \
        --arg a "$matt_slack_bead" --arg b "$matt_oidc_bead" \
        '(.beads | map(.id) | index($a) != null) and (.beads | map(.id) | index($b) != null)' \
        >/dev/null 2>&1; then
      pass "both matt beads surface under slack:${MATT_SLACK_ID}"
    else
      fail "alias collapse via slack alias" \
        "slack:${MATT_SLACK_ID} returned $(printf '%s' "$matt_via_slack" | jq -c '.beads | map(.id)' 2>/dev/null || true), wanted both $matt_slack_bead and $matt_oidc_bead"
    fi

    # Both beads under matt's oidc alias
    if printf '%s' "$matt_via_oidc" | jq -e \
        --arg a "$matt_slack_bead" --arg b "$matt_oidc_bead" \
        '(.beads | map(.id) | index($a) != null) and (.beads | map(.id) | index($b) != null)' \
        >/dev/null 2>&1; then
      pass "both matt beads surface under oidc:${MATT_EMAIL} (collapse holds)"
    else
      fail "alias collapse via oidc alias" \
        "oidc:${MATT_EMAIL} returned $(printf '%s' "$matt_via_oidc" | jq -c '.beads | map(.id)' 2>/dev/null || true), wanted both $matt_slack_bead and $matt_oidc_bead"
    fi

    # Dana's alias query must still NOT include either of matt's beads
    # — collapse never merges across distinct humans.
    local dana_recheck
    dana_recheck=$(list_operated_by_alias "oidc:${DANA_EMAIL}")
    if printf '%s' "$dana_recheck" | jq -e \
        --arg a "$matt_slack_bead" --arg b "$matt_oidc_bead" \
        '(.beads | map(.id) | index($a) == null) and (.beads | map(.id) | index($b) == null)' \
        >/dev/null 2>&1; then
      pass "matt's beads still isolated from dana after collapse"
    else
      fail "collapse leaked matt's beads across humans" \
        "dana alias returned $(printf '%s' "$dana_recheck" | jq -c '.beads | map(.id)' 2>/dev/null || true)"
    fi
  fi
  echo

  summary
}

summary() {
  blue "═══════════════════════════════════════════════════"
  blue " multi-user-e2e: $PASS passed, $FAIL failed, $SKIP skipped"
  if [ "$FAIL" -gt 0 ]; then
    red "FAILURES:"
    for e in ${ERRORS[@]+"${ERRORS[@]}"}; do red "  - $e"; done
    exit 1
  fi
  blue "═══════════════════════════════════════════════════"
}

main "$@"
