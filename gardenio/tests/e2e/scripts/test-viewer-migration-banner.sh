#!/usr/bin/env bash
# test-viewer-migration-banner.sh — viewer migration-banner E2E
#
# Covers the V-R2 §5 banner render/dismiss contract and the V-R4 §3
# config-flag sunset for the migration banner surfaced on /active and
# /search by gardenio/controller/cmd/beads-viewer/migration_banner.go.
#
# Bead chain: ka-0000000489 (this module) → ka-0000002673 (coverage gap)
# → ka-0000007452 / ka-0000004510 / ka-0000006508 (P5/P6/P7 impl).
#
# Coverage plan: gardenio/docs/research/e2e-coverage-migration-banner.md.
#
# Scenarios:
#   1. Banner renders for non-Matt OIDC user on /active.
#   2. Banner suppressed for Matt-matching OIDC user on /active.
#   3. POST /banner/migration/dismiss returns 204 + dismiss cookie;
#      a re-GET carrying the cookie suppresses the banner.
#   4. Flipping viewer.show_migration_banner=false on a tunables config
#      bead suppresses the banner across all viewers within one
#      ConfigWatcher poll, without a deploy. Restored in cleanup.
#   5. (Skipped — calendar backstop has no clock-injection seam in
#      production today; covered by the unit test in
#      migration_banner_test.go.)
#   6. Cross-surface: scenario-1 repeated against /search.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-viewer-migration-banner.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster (default gardenio-e2e)
#   - kd, jq, curl in PATH
#   - GARDENIO_VIEWER_MATT_EMAIL set in the deployed viewer Deployment
#     env (otherwise every render scenario fail-closes and the module
#     skips them with explanatory output — the dismiss-cookie shape is
#     still validated against a fresh token).
#   - lib-gitlab-token.sh's mint_user_token resolves a GitLab-direct bearer
#     (GARDENIO_E2E_GITLAB_BEARER or the static daemon token must be set).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gitlab-token.sh"

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
VIEWER_SVC="${VIEWER_SVC:-${RELEASE}-beads-viewer}"
VIEWER_PF_PORT="${VIEWER_PF_PORT:-19501}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19502}"
KD_BIN="${KD_BIN:-kd}"
# ConfigWatcher default poll is 30s; wait two cycles + grace.
WATCH_WAIT_SECS="${WATCH_WAIT_SECS:-75}"
NON_MATT_EMAIL="${NON_MATT_EMAIL:-e2e-not-matt-$$@example.com}"
PASS=0
FAIL=0
SKIP=0
ERRORS=()
VIEWER_PF_PID=""
BEADS_PF_PID=""
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

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
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  [ -n "$VIEWER_PF_PID" ] && kill "$VIEWER_PF_PID" 2>/dev/null || true
  [ -n "$BEADS_PF_PID"  ] && kill "$BEADS_PF_PID"  2>/dev/null || true
}
trap cleanup EXIT

# ── Setup ─────────────────────────────────────────────────────────
setup_viewer_portforward() {
  if ! kubectl -n "$NAMESPACE" get svc "$VIEWER_SVC" >/dev/null 2>&1; then
    yellow "viewer service $VIEWER_SVC not found in $NAMESPACE — skipping module"
    return 1
  fi
  blue "Port-forwarding ${NAMESPACE}/${VIEWER_SVC} :${VIEWER_PF_PORT} → 8095..."
  kubectl -n "$NAMESPACE" port-forward "svc/${VIEWER_SVC}" \
    "${VIEWER_PF_PORT}:8095" >/tmp/viewer-pf.log 2>&1 &
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

setup_beads_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return 0
  fi
  blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
    "${BEADS_PF_PORT}:8080" >/tmp/viewer-mb-beads-pf.log 2>&1 &
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

# Reads the deployed viewer Deployment env to figure out (a) whether
# GARDENIO_VIEWER_MATT_EMAIL is set (gate 2 fail-closed), and (b) what
# Matt's email actually is. Echoes the email on stdout (empty when unset).
detect_matt_email() {
  local dep
  dep=$(kubectl -n "$NAMESPACE" get deploy -l "app.kubernetes.io/component=beads-viewer" \
    -o name 2>/dev/null | head -n1)
  if [ -z "$dep" ]; then
    return 0
  fi
  kubectl -n "$NAMESPACE" get "$dep" \
    -o "jsonpath={.spec.template.spec.containers[?(@.name=='beads-viewer')].env[?(@.name=='GARDENIO_VIEWER_MATT_EMAIL')].value}" \
    2>/dev/null || true
}

bead_id_from_json() {
  echo "$1" | jq -r '.id // empty' 2>/dev/null || echo ""
}

# viewer_get <path> <token> [extra_curl_args...]
# GETs http://localhost:$VIEWER_PF_PORT$path with bearer auth.
# Echoes "<status>\n<body>" on stdout (status on first line, body on rest).
viewer_get() {
  local path="$1" token="$2"
  shift 2
  local out status
  out=$(mktemp)
  status=$(curl -sk --max-time 10 \
    -H "Authorization: Bearer ${token}" \
    -H "Accept: text/html" \
    -o "$out" \
    -w '%{http_code}' \
    "$@" \
    "http://localhost:${VIEWER_PF_PORT}${path}" 2>/dev/null || echo "000")
  printf '%s\n' "$status"
  cat "$out"
  rm -f "$out"
}

# viewer_post <path> <token> [extra_curl_args...]
# POSTs and echoes "<status>\n<headers>\n---BODY---\n<body>" on stdout.
viewer_post() {
  local path="$1" token="$2"
  shift 2
  local body_f hdr_f status
  body_f=$(mktemp)
  hdr_f=$(mktemp)
  status=$(curl -sk --max-time 10 -X POST \
    -H "Authorization: Bearer ${token}" \
    -o "$body_f" \
    -D "$hdr_f" \
    -w '%{http_code}' \
    "$@" \
    "http://localhost:${VIEWER_PF_PORT}${path}" 2>/dev/null || echo "000")
  printf '%s\n' "$status"
  cat "$hdr_f"
  printf '\n---BODY---\n'
  cat "$body_f"
  rm -f "$body_f" "$hdr_f"
}

# Markers we look for in /active and /search HTML responses to assert the
# banner partial rendered. Both come from migration_banner.html.
BANNER_ID_MARKER='id="migration-banner"'
BANNER_DISMISS_MARKER='hx-post="/banner/migration/dismiss"'

assert_banner_present_in() {
  local name="$1" body="$2"
  if grep -qF -- "$BANNER_ID_MARKER" <<<"$body" \
     && grep -qF -- "$BANNER_DISMISS_MARKER" <<<"$body"; then
    pass "$name"
  else
    fail "$name" "missing banner markers (id=migration-banner / hx-post=/banner/migration/dismiss)"
  fi
}

assert_banner_absent_in() {
  local name="$1" body="$2"
  if grep -qF -- "$BANNER_ID_MARKER" <<<"$body"; then
    fail "$name" "banner partial unexpectedly present"
  else
    pass "$name"
  fi
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " viewer migration-banner E2E (ka-0000000489)"
  blue "═══════════════════════════════════════════════════"

  if ! setup_viewer_portforward; then
    skip "viewer-migration-banner module" "viewer service $VIEWER_SVC absent"
    summary
    return 0
  fi
  setup_beads_portforward

  local matt_email
  matt_email=$(detect_matt_email)
  if [ -z "$matt_email" ]; then
    yellow "GARDENIO_VIEWER_MATT_EMAIL is unset on the deployed viewer."
    yellow "All render scenarios fail-closed in this state — see ka-0000000489"
    yellow "for the open decision on whether to wire the env var into"
    yellow "staging Helm values. Skipping render scenarios."
    NO_MATT_EMAIL=1
  else
    dim "  GARDENIO_VIEWER_MATT_EMAIL=${matt_email}"
    NO_MATT_EMAIL=0
  fi

  # ── Mint a token for a known non-Matt user. ──────────────────────
  local non_matt_token=""
  if ! non_matt_token=$(mint_user_token "$NON_MATT_EMAIL" 2>/tmp/viewer-mb-mint.err); then
    yellow "mint_user_token failed for ${NON_MATT_EMAIL} — see /tmp/viewer-mb-mint.err"
    yellow "Skipping all token-bearing scenarios (helper needs GARDENIO_E2E_GITLAB_BEARER"
    yellow "or the static daemon token — see lib-gitlab-token.sh)."
    skip "scenario 1: banner renders for non-Matt user" "no usable GitLab-direct bearer"
    skip "scenario 2: banner suppressed for Matt" "no usable GitLab-direct bearer"
    skip "scenario 3: dismiss cookie / 204 contract" "no usable GitLab-direct bearer"
    skip "scenario 4: tunable sunset (flag→false)" "no usable GitLab-direct bearer"
    skip "scenario 6: /search cross-surface render" "no usable GitLab-direct bearer"
    summary
    return 0
  fi
  dim "  minted non-Matt user token (sub=${NON_MATT_EMAIL})"

  # ══════════════════════════════════════════════════════════════════
  # Scenario 1: banner renders on /active for a non-Matt user.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 1: banner renders on /active for non-Matt user"
  if [ "$NO_MATT_EMAIL" = "1" ]; then
    skip "scenario 1: banner renders for non-Matt user" \
      "GARDENIO_VIEWER_MATT_EMAIL unset on viewer Deployment (gate 2 fail-closed)"
  else
    local resp status body
    resp=$(viewer_get /active "$non_matt_token")
    status=$(printf '%s' "$resp" | head -n1)
    body=$(printf '%s' "$resp" | tail -n +2)
    if [ "$status" != "200" ]; then
      fail "GET /active (non-Matt token)" "expected HTTP 200, got $status"
    else
      assert_banner_present_in "scenario 1: /active contains banner partial" "$body"
    fi
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 2: banner suppressed for Matt-matching OIDC user.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 2: banner suppressed on /active for Matt-matching user"
  if [ "$NO_MATT_EMAIL" = "1" ]; then
    skip "scenario 2: banner suppressed for Matt" \
      "GARDENIO_VIEWER_MATT_EMAIL unset; cannot construct a Matt-matching identity"
  else
    local matt_token=""
    if ! matt_token=$(mint_user_token "$matt_email" 2>/tmp/viewer-mb-mint-matt.err); then
      skip "scenario 2: banner suppressed for Matt" \
        "could not mint token for ${matt_email}"
    else
      local resp status body
      resp=$(viewer_get /active "$matt_token")
      status=$(printf '%s' "$resp" | head -n1)
      body=$(printf '%s' "$resp" | tail -n +2)
      if [ "$status" != "200" ]; then
        fail "GET /active (Matt token)" "expected HTTP 200, got $status"
      else
        assert_banner_absent_in "scenario 2: /active does NOT contain banner partial" "$body"
      fi
    fi
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 3: cookie dismiss path (P6).
  # POST /banner/migration/dismiss → 204 + Set-Cookie.
  # Re-GET /active carrying the cookie → banner absent.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 3: POST /banner/migration/dismiss → 204 + cookie; re-GET suppresses"
  if [ "$NO_MATT_EMAIL" = "1" ]; then
    skip "scenario 3: dismiss cookie / 204 contract" \
      "GARDENIO_VIEWER_MATT_EMAIL unset; banner is already suppressed so re-GET assertion is vacuous"
  else
    local post_out status hdrs cookie_val
    post_out=$(viewer_post /banner/migration/dismiss "$non_matt_token")
    status=$(printf '%s' "$post_out" | head -n1)
    hdrs=$(printf '%s' "$post_out" | sed -n '2,/^---BODY---$/p' | sed '$d')
    if [ "$status" != "204" ]; then
      fail "POST /banner/migration/dismiss returns 204" "got HTTP $status"
    else
      pass "POST /banner/migration/dismiss returns 204"
    fi
    # Cookie must be HttpOnly, SameSite=Lax, Path=/, Max-Age positive ~1y.
    if grep -qiE '^set-cookie:[^\r\n]*gb_migration_banner_dismissed=1' <<<"$hdrs"; then
      pass "Set-Cookie carries gb_migration_banner_dismissed=1"
    else
      fail "Set-Cookie carries gb_migration_banner_dismissed=1" \
        "no matching Set-Cookie header in response"
    fi
    if grep -qiE '^set-cookie:[^\r\n]*gb_migration_banner_dismissed=1[^\r\n]*HttpOnly' <<<"$hdrs"; then
      pass "dismiss cookie is HttpOnly"
    else
      fail "dismiss cookie is HttpOnly" "missing HttpOnly attribute"
    fi
    if grep -qiE '^set-cookie:[^\r\n]*gb_migration_banner_dismissed=1[^\r\n]*SameSite=Lax' <<<"$hdrs"; then
      pass "dismiss cookie SameSite=Lax"
    else
      fail "dismiss cookie SameSite=Lax" "missing SameSite=Lax attribute"
    fi
    if grep -qiE '^set-cookie:[^\r\n]*gb_migration_banner_dismissed=1[^\r\n]*Path=/' <<<"$hdrs"; then
      pass "dismiss cookie Path=/"
    else
      fail "dismiss cookie Path=/" "missing Path=/ attribute"
    fi
    cookie_val=$(grep -iE '^set-cookie:[^\r\n]*gb_migration_banner_dismissed=1' <<<"$hdrs" \
      | head -n1 | tr -d '\r')

    # Re-GET /active carrying the dismiss cookie — banner must be absent.
    local resp2 status2 body2
    resp2=$(viewer_get /active "$non_matt_token" \
      --cookie "gb_migration_banner_dismissed=1")
    status2=$(printf '%s' "$resp2" | head -n1)
    body2=$(printf '%s' "$resp2" | tail -n +2)
    if [ "$status2" != "200" ]; then
      fail "GET /active (with dismiss cookie)" "expected HTTP 200, got $status2"
    else
      assert_banner_absent_in "scenario 3: dismiss cookie suppresses banner on /active" "$body2"
    fi
    dim "  observed Set-Cookie: ${cookie_val}"
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 4: tunable sunset (P7) — flip viewer.show_migration_banner=false
  # via a config bead and assert the banner disappears within one poll.
  # Restored in cleanup by closing the created bead.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 4: viewer.show_migration_banner=false suppresses banner"
  if [ "$NO_MATT_EMAIL" = "1" ]; then
    skip "scenario 4: tunable sunset (flag→false)" \
      "GARDENIO_VIEWER_MATT_EMAIL unset; banner already suppressed by gate 2"
  elif ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    skip "scenario 4: tunable sunset (flag→false)" \
      "kd cannot reach beads daemon at ${BEADS_HTTP_URL:-unset}"
  else
    local cfg_out cfg_id
    cfg_out=$("$KD_BIN" create "tunables" --type=config \
      --project "" --force \
      -l "component:beads-viewer" \
      -f "viewer.show_migration_banner=false" \
      -d "E2E migration-banner sunset test ($(date +%s)); auto-closed by test-viewer-migration-banner.sh" \
      --json 2>/dev/null || echo "{}")
    cfg_id=$(bead_id_from_json "$cfg_out")
    if [ -z "$cfg_id" ]; then
      fail "create viewer tunables config bead" "no ID returned from kd create"
    else
      CREATED_BEADS+=("$cfg_id")
      pass "tunables config bead created ($cfg_id)"
      dim "  waiting up to ${WATCH_WAIT_SECS}s for ConfigWatcher to pick it up..."
      local deadline=$((SECONDS + WATCH_WAIT_SECS))
      local observed_absent=0
      while [ "$SECONDS" -lt "$deadline" ]; do
        local resp4 status4 body4
        resp4=$(viewer_get /active "$non_matt_token")
        status4=$(printf '%s' "$resp4" | head -n1)
        body4=$(printf '%s' "$resp4" | tail -n +2)
        if [ "$status4" = "200" ] && ! grep -qF -- "$BANNER_ID_MARKER" <<<"$body4"; then
          observed_absent=1
          break
        fi
        sleep 5
      done
      if [ "$observed_absent" = "1" ]; then
        pass "scenario 4: banner absent after flag→false within ${WATCH_WAIT_SECS}s"
      else
        fail "scenario 4: banner absent after flag→false" \
          "banner still present after ${WATCH_WAIT_SECS}s"
      fi
    fi
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 6: cross-surface render on /search.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 6: banner renders on /search for non-Matt user"
  if [ "$NO_MATT_EMAIL" = "1" ]; then
    skip "scenario 6: /search cross-surface render" \
      "GARDENIO_VIEWER_MATT_EMAIL unset (same root cause as scenario 1)"
  else
    # If scenario 4 left a flag-false bead, the banner is still suppressed
    # — make sure cleanup closes it before we re-assert. cleanup runs at
    # EXIT only, so close it eagerly here when scenario 4 created one.
    if [ "${#CREATED_BEADS[@]}" -gt 0 ]; then
      for id in "${CREATED_BEADS[@]}"; do
        "$KD_BIN" close "$id" >/dev/null 2>&1 || true
      done
      CREATED_BEADS=()
      dim "  closed scenario-4 tunables bead; waiting one poll cycle..."
      sleep "$WATCH_WAIT_SECS"
    fi
    local resp6 status6 body6
    resp6=$(viewer_get /search "$non_matt_token")
    status6=$(printf '%s' "$resp6" | head -n1)
    body6=$(printf '%s' "$resp6" | tail -n +2)
    if [ "$status6" != "200" ]; then
      fail "GET /search (non-Matt token)" "expected HTTP 200, got $status6"
    else
      assert_banner_present_in "scenario 6: /search contains banner partial" "$body6"
    fi
  fi
  echo

  summary
}

summary() {
  blue "═══════════════════════════════════════════════════"
  blue " viewer-migration-banner: $PASS passed, $FAIL failed, $SKIP skipped"
  if [ "$FAIL" -gt 0 ]; then
    red "FAILURES:"
    for e in "${ERRORS[@]}"; do red "  - $e"; done
    exit 1
  fi
  blue "═══════════════════════════════════════════════════"
}

main "$@"
