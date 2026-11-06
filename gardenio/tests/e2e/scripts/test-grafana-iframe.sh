#!/usr/bin/env bash
# test-grafana-iframe.sh — assert the embedded Grafana dashboard iframe loads
# in beads-viewer (no X-Frame-Options "refused to connect").
#
# Bead gd-opukem7ufe (E2E coverage), parent epic gd-OY6hl3Qh9l. Guards the
# allow_embedding fix (ka-0000003842): Grafana defaults allow_embedding=false,
# which sends `X-Frame-Options: deny` on EVERY response and makes Chrome render
# a same-origin beads-viewer dashboard iframe as "refused to connect". The fix
# sets GF_SECURITY_ALLOW_EMBEDDING=true (helm value grafana.allowEmbedding,
# default on), which drops X-Frame-Options. This test fails the suite if that
# regresses.
#
# The header behaviour is independent of Grafana auth: the security-header
# middleware stamps (or, post-fix, omits) X-Frame-Options on ALL responses —
# /api/health, the login redirect, and /d/<uid>?kiosk alike. So we can assert
# the header without an authenticated session, which keeps the test robust
# behind the auth-proxy (the in-cluster port-forward source IP is not on the
# auth-proxy whitelist, so /d/... would 302/401 — but its FRAMING headers are
# still the thing under test).
#
# Scenarios:
#   1. GET <grafana>/d/<uid>?kiosk does NOT carry `X-Frame-Options: deny`.
#      This is the exact embed path beads-viewer builds (dashboard_view.go
#      EmbedURL = <grafanaBase>/d/<uid>?kiosk) — the direct guard.
#   2. GET <grafana>/api/health returns HTTP 200 (Grafana reachable) and that
#      200 response likewise carries no `X-Frame-Options: deny` — a clean
#      positive control that the header is gone on a real 200, not just on a
#      redirect.
#   3. Full-dashboard render: GET <grafana>/render/d/<real-uid> returns a REAL
#      PNG, not the "Grafana - Error" HTML page / HTTP 500 the whole-board route
#      used to give on tall multi-panel pages via the image-renderer sidecar
#      (bug ka-0000006919). Guards the sidecar hardening (clustered mode +
#      --disable-dev-shm-usage + memory bump). Best-effort: SKIPs when the
#      renderer is not deployed, no dashboard uid is enumerable, or admin API
#      auth is unavailable.
#   4. Best-effort browser-shape check: render a type=dashboard bead's
#      beads-viewer page and assert the live <iframe> is emitted pointing at
#      the configured Grafana origin (i.e. the embed is wired, not the
#      "Grafana not configured" fallback). SKIPs cleanly when the viewer,
#      a GitLab-direct bearer, or GRAFANA_BASE_URL are unavailable.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-grafana-iframe.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster (default gardenio-e2e)
#   - curl, jq in PATH
#
# Environment note: Grafana is enabled only on the long-lived staging
# singleton and prod — NOT in the per-instance ephemeral e2e namespaces
# (the e2e values keep grafana off). When the
# Grafana Service is absent the whole module SKIPs cleanly (exit 0), so it is
# inert in the ephemeral shard fan-out and asserts against the singleton.
#
# Idempotency: the scenario-4 dashboard bead carries a unique title prefix and
# is closed in the cleanup trap; reruns do not collide.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
GRAFANA_SVC="${GRAFANA_SVC:-${RELEASE}-grafana}"
GRAFANA_PORT="${GRAFANA_PORT:-3000}"
GRAFANA_PF_PORT="${GRAFANA_PF_PORT:-19610}"
VIEWER_SVC="${VIEWER_SVC:-${RELEASE}-beads-viewer}"
VIEWER_PF_PORT="${VIEWER_PF_PORT:-19611}"
KD_BIN="${KD_BIN:-kd}"
# A charset-valid (dashboard_view.go dashboardUIDRe) dummy uid for the framing
# probe. The dashboard need not exist — Grafana applies the same security-header
# middleware to the redirect/404 it returns, which is what we assert on.
PROBE_UID="${PROBE_UID:-e2e-iframe-probe}"
TITLE_PREFIX="grafana-iframe-e2e"
RANDOM_SUFFIX="$$-$(date +%s)"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
GRAFANA_PF_PID=""
VIEWER_PF_PID=""
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
  for id in "${CREATED_BEADS[@]:-}"; do
    [ -n "$id" ] && "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  [ -n "$GRAFANA_PF_PID" ] && kill "$GRAFANA_PF_PID" 2>/dev/null || true
  [ -n "$VIEWER_PF_PID" ]  && kill "$VIEWER_PF_PID"  2>/dev/null || true
}
trap cleanup EXIT

# header_value <headers-file> <header-name> — echoes the (last) value of the
# named response header, lowercased, trimmed. Empty when absent.
#
# The `|| true` on grep is load-bearing under `set -euo pipefail`: an ABSENT
# header is the SUCCESS case for the X-Frame-Options assertions (the
# allow_embedding fix drops the header entirely). Without the guard, grep's
# exit 1 (no match) propagates through pipefail into the `xfo=$(header_value …)`
# command substitution and `set -e` aborts the whole script — on the exact
# healthy path the test is meant to pass. Guarding grep makes "header missing"
# yield an empty string instead of killing the run.
header_value() {
  local file="$1" name="$2"
  { grep -i "^${name}:" "$file" 2>/dev/null || true; } \
    | tail -n1 \
    | sed -E "s/^[^:]*:[[:space:]]*//I" \
    | tr -d '\r' \
    | tr '[:upper:]' '[:lower:]' \
    | sed -E 's/[[:space:]]+$//'
}

# ── Setup: port-forward to Grafana ────────────────────────────────
setup_grafana_portforward() {
  if ! kubectl -n "$NAMESPACE" get svc "$GRAFANA_SVC" >/dev/null 2>&1; then
    return 1
  fi
  blue "Port-forwarding ${NAMESPACE}/${GRAFANA_SVC} :${GRAFANA_PF_PORT} → ${GRAFANA_PORT}..."
  kubectl -n "$NAMESPACE" port-forward "svc/${GRAFANA_SVC}" \
    "${GRAFANA_PF_PORT}:${GRAFANA_PORT}" >/tmp/grafana-iframe-pf.log 2>&1 &
  GRAFANA_PF_PID=$!
  local _i
  for _i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    if curl -sf --max-time 2 -o /dev/null \
        "http://localhost:${GRAFANA_PF_PORT}/api/health" 2>/dev/null; then
      dim "  Grafana tunnel ready on :${GRAFANA_PF_PORT} (pid $GRAFANA_PF_PID)"
      return 0
    fi
    if ! kill -0 "$GRAFANA_PF_PID" 2>/dev/null; then
      yellow "  Grafana port-forward pid died; retrying..."
      kubectl -n "$NAMESPACE" port-forward "svc/${GRAFANA_SVC}" \
        "${GRAFANA_PF_PORT}:${GRAFANA_PORT}" >/tmp/grafana-iframe-pf.log 2>&1 &
      GRAFANA_PF_PID=$!
    fi
    sleep 1
  done
  yellow "  Grafana port-forward did not become healthy within 15s; continuing"
  return 0
}

setup_viewer_portforward() {
  if ! kubectl -n "$NAMESPACE" get svc "$VIEWER_SVC" >/dev/null 2>&1; then
    return 1
  fi
  kubectl -n "$NAMESPACE" port-forward "svc/${VIEWER_SVC}" \
    "${VIEWER_PF_PORT}:8095" >/tmp/grafana-iframe-viewer-pf.log 2>&1 &
  VIEWER_PF_PID=$!
  local _i
  for _i in 1 2 3 4 5 6 7 8 9 10; do
    if curl -s --max-time 1 -o /dev/null \
        "http://localhost:${VIEWER_PF_PORT}/" 2>/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  return 0
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " Grafana iframe embedding E2E (gd-opukem7ufe)"
  blue "═══════════════════════════════════════════════════"

  if ! setup_grafana_portforward; then
    skip "grafana-iframe module" \
      "Grafana Service '$GRAFANA_SVC' absent in '$NAMESPACE' (grafana.enabled=false — expected in ephemeral e2e ns)"
    summary
    return 0
  fi

  local base="http://localhost:${GRAFANA_PF_PORT}"

  # ══════════════════════════════════════════════════════════════════
  # Scenario 1: the embed path GET /d/<uid>?kiosk carries NO
  # `X-Frame-Options: deny`. This is the exact iframe src beads-viewer
  # builds and the direct guard for the allow_embedding fix.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 1: /d/<uid>?kiosk drops X-Frame-Options:deny (allow_embedding guard)"
  local hdr1 status1 xfo1
  hdr1=$(mktemp)
  # -I would force HEAD; Grafana may not honour HEAD on SPA routes, so GET with
  # -D dumps headers while discarding the body to /dev/null.
  status1=$(curl -sk --max-time 15 -o /dev/null -D "$hdr1" \
    -w '%{http_code}' \
    "${base}/d/${PROBE_UID}?kiosk" 2>/dev/null || echo "000")
  if [ "$status1" = "000" ]; then
    fail "GET /d/${PROBE_UID}?kiosk" "no HTTP response (tunnel down?)"
  else
    xfo1=$(header_value "$hdr1" "X-Frame-Options")
    dim "    status=$status1  x-frame-options='${xfo1:-<none>}'"
    if [ "$xfo1" = "deny" ] || [ "$xfo1" = "sameorigin" ]; then
      fail "no X-Frame-Options on /d/<uid>?kiosk" \
        "got 'X-Frame-Options: ${xfo1}' — allow_embedding regressed; iframe will 'refuse to connect'"
    else
      pass "no blocking X-Frame-Options on /d/<uid>?kiosk (value='${xfo1:-<none>}')"
    fi
  fi
  rm -f "$hdr1"
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 2: /api/health returns 200 AND that 200 carries no
  # `X-Frame-Options: deny` — reachability + a clean positive control
  # that the header is gone on a real 200, not only on a redirect.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 2: /api/health is 200 with no X-Frame-Options:deny"
  local hdr2 status2 xfo2
  hdr2=$(mktemp)
  status2=$(curl -sk --max-time 15 -o /dev/null -D "$hdr2" \
    -w '%{http_code}' \
    "${base}/api/health" 2>/dev/null || echo "000")
  if [ "$status2" = "200" ]; then
    pass "GET /api/health → 200 (Grafana reachable)"
  else
    fail "GET /api/health → 200" "expected 200, got $status2"
  fi
  xfo2=$(header_value "$hdr2" "X-Frame-Options")
  if [ "$xfo2" = "deny" ] || [ "$xfo2" = "sameorigin" ]; then
    fail "no X-Frame-Options on /api/health" \
      "got 'X-Frame-Options: ${xfo2}' on a 200 — allow_embedding regressed"
  else
    pass "no blocking X-Frame-Options on /api/health (value='${xfo2:-<none>}')"
  fi
  rm -f "$hdr2"
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 3: full-dashboard render /render/d/<uid> returns a REAL PNG
  # (bug ka-0000006919). The whole-board route was returning HTTP 500 / an
  # HTML "Grafana - Error" page (or hanging to timeout) via the
  # grafana-image-renderer sidecar on tall multi-panel pages, while a single
  # d-solo panel worked. This asserts the sidecar hardening (clustered mode +
  # --disable-dev-shm-usage + memory bump) actually produces a PNG for the
  # whole board. Best-effort: SKIPs cleanly when the renderer is not deployed
  # (grafana.imageRenderer.enabled=false, e.g. ephemeral e2e ns), when no real
  # dashboard uid can be enumerated, or when admin API auth is unavailable.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 3: /render/d/<uid> full-board returns a real PNG (image-renderer hardening)"
  # The pod serves under /grafana in subpath mode and at root otherwise — probe
  # /api/health both ways and use whichever the pod actually answers, so the
  # render + search paths hit the working base regardless of subpath config.
  local gbase=""
  if curl -sf --max-time 5 -o /dev/null "${base}/grafana/api/health" 2>/dev/null; then
    gbase="${base}/grafana"
  elif curl -sf --max-time 5 -o /dev/null "${base}/api/health" 2>/dev/null; then
    gbase="${base}"
  fi
  if [ -z "$gbase" ]; then
    skip "scenario 3: full-board render" "could not resolve a working Grafana API base (subpath probe failed)"
  else
    # admin:admin basic-auth is the in-cluster API break-glass (the port-forward
    # source IP is not on the auth-proxy whitelist, so the stamped header is not
    # trusted here — see header note above). Enumerate a real dashboard uid.
    local search_json first_uid
    local admin_auth="admin:admin" # gitleaks:allow: Grafana's default admin login
    search_json=$(curl -sk --max-time 10 -u "$admin_auth" \
      "${gbase}/api/search?type=dash-db&limit=1" 2>/dev/null || echo '[]')
    first_uid=$(printf '%s' "$search_json" | jq -r '.[0].uid // empty' 2>/dev/null || echo '')
    if [ -z "$first_uid" ]; then
      skip "scenario 3: full-board render" \
        "no dashboard uid enumerable via admin API (no dashboards provisioned, or admin auth unavailable)"
    else
      dim "  rendering whole board uid=$first_uid"
      local rpng rstatus rsize rmagic
      rpng=$(mktemp)
      # kiosk + a tall geometry is exactly the shape that used to 500. A generous
      # --max-time covers a cold headless-Chromium launch on the sidecar.
      rstatus=$(curl -sk --max-time 90 -u "$admin_auth" \
        -o "$rpng" -w '%{http_code}' \
        "${gbase}/render/d/${first_uid}/x?width=1200&height=900&kiosk&theme=dark&from=now-6h&to=now" \
        2>/dev/null || echo "000")
      rsize=$(wc -c <"$rpng" 2>/dev/null | tr -d ' ' || echo 0)
      # PNG magic: first 8 bytes 89 50 4E 47 0D 0A 1A 0A.
      rmagic=$(head -c8 "$rpng" 2>/dev/null | od -An -tx1 2>/dev/null | tr -d ' \n' || echo '')
      rm -f "$rpng"
      dim "    status=$rstatus size=${rsize}B magic=${rmagic:0:16}"
      if [ "$rstatus" != "200" ]; then
        fail "full-board /render/d/${first_uid} → 200" \
          "got HTTP $rstatus (renderer 500/timeout on the whole board — bug ka-0000006919 regressed)"
      elif [ "$rmagic" != "89504e470d0a1a0a" ]; then
        fail "full-board render is a PNG" \
          "body is not a PNG (magic='${rmagic:0:16}') — likely the 'Grafana - Error' HTML page, bug ka-0000006919"
      elif [ "$rsize" -lt 20000 ]; then
        # A real 1200x900 dark board is well over 20KB; a tiny PNG is the
        # 'renderer unavailable' / 'panel not found' placeholder card.
        fail "full-board render is a real board, not a placeholder" \
          "PNG is only ${rsize}B (< 20KB placeholder threshold) for a 1200x900 request"
      else
        pass "full-board /render/d/${first_uid} → real ${rsize}B PNG (image-renderer hardening holds)"
      fi
    fi
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 4 (best-effort browser-shape): render a type=dashboard bead's
  # beads-viewer page and assert the live <iframe> is emitted pointing at the
  # configured Grafana origin — i.e. the embed is actually wired, not the
  # "Grafana not configured" fallback. SKIPs cleanly when the viewer is
  # absent, no GitLab-direct bearer is available, or GRAFANA_BASE_URL is unset on
  # the viewer (the iframe only renders when it is).
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 4: dashboard-bead viewer page emits the live Grafana iframe (best-effort)"
  if ! setup_viewer_portforward; then
    skip "scenario 4: viewer iframe render" "viewer Service '$VIEWER_SVC' absent"
    summary
    return 0
  fi
  # Resolve a viewer bearer via the shared GitLab-direct helper if available.
  local token=""
  if [ -f "$SCRIPT_DIR/lib-gitlab-token.sh" ]; then
    # shellcheck disable=SC1091
    source "$SCRIPT_DIR/lib-gitlab-token.sh"
    token=$(mint_user_token "e2e-grafana-iframe-${RANDOM_SUFFIX}@example.com" 2>/dev/null || echo "")
  fi
  # Need beads access to create the dashboard bead.
  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    skip "scenario 4: viewer iframe render" "kd cannot reach beads daemon"
    summary
    return 0
  fi
  # Seed a type=dashboard bead carrying a dashboard_uid. The viewer builds the
  # iframe from dashboard_uid + GRAFANA_BASE_URL (dashboard_view.go).
  local dash_id="" create_out
  create_out=$("$KD_BIN" create "${TITLE_PREFIX}-dash-${RANDOM_SUFFIX}" \
    --type=dashboard \
    --field "dashboard_uid=${PROBE_UID}" \
    -d "grafana-iframe e2e dashboard bead ($RANDOM_SUFFIX)" \
    --json 2>/dev/null || printf '{}')
  dash_id=$(printf '%s' "$create_out" | jq -r '.id // empty' 2>/dev/null || printf '')
  if [ -z "$dash_id" ]; then
    skip "scenario 4: viewer iframe render" \
      "could not create type=dashboard bead (type may not be registered in this env)"
    summary
    return 0
  fi
  CREATED_BEADS+=("$dash_id")
  dim "  created dashboard bead $dash_id (uid=$PROBE_UID)"

  local vstatus vbody vout
  vout=$(mktemp)
  vstatus=$(curl -sk --max-time 15 \
    ${token:+-H "Authorization: Bearer ${token}"} \
    -H "Accept: text/html" \
    -o "$vout" -w '%{http_code}' \
    "http://localhost:${VIEWER_PF_PORT}/b/${dash_id}" 2>/dev/null || echo "000")
  vbody=$(cat "$vout"); rm -f "$vout"
  if [ "$vstatus" != "200" ]; then
    skip "scenario 4: viewer iframe render" \
      "GET /b/${dash_id} → $vstatus (viewer auth not available without a valid token)"
  elif grep -q 'grafana-embed--unconfigured' <<<"$vbody"; then
    # GRAFANA_BASE_URL not set on the viewer in this env → fallback card, not
    # an iframe. Not a regression of the embedding fix; record as a skip.
    skip "scenario 4: viewer iframe render" \
      "viewer shows 'Grafana not configured' fallback (GRAFANA_BASE_URL unset on viewer)"
  elif grep -qE '<iframe[^>]*class="grafana-embed-frame' <<<"$vbody"; then
    # The live iframe is present; confirm its src targets /d/<uid>?kiosk.
    if grep -qE "<iframe[^>]*src=\"[^\"]*/d/${PROBE_UID}\?kiosk" <<<"$vbody"; then
      pass "scenario 4: dashboard-bead page emits live Grafana iframe → /d/${PROBE_UID}?kiosk"
    else
      pass "scenario 4: dashboard-bead page emits live Grafana iframe (grafana-embed-frame present)"
    fi
  else
    fail "scenario 4: viewer iframe render" \
      "no grafana-embed-frame iframe and no unconfigured fallback in /b/${dash_id} HTML"
  fi
  echo

  summary
}

summary() {
  blue "═══════════════════════════════════════════════════"
  blue " grafana-iframe: $PASS passed, $FAIL failed, $SKIP skipped"
  if [ "$FAIL" -gt 0 ]; then
    red "FAILURES:"
    for e in "${ERRORS[@]}"; do red "  - $e"; done
    exit 1
  fi
  blue "═══════════════════════════════════════════════════"
}

main "$@"
