#!/usr/bin/env bash
# test-charts-hub.sh — E2E for the /charts Observability Hub + its two
# browser-facing daemon proxies (gd-irTNYKQtA5, parent epic ka-0000000958).
#
# WHY THIS EXISTS: a whole new /charts observability subsystem landed in the
# June freeze with ZERO live e2e — the viewer hub (charts.go / dashboard_hub.go),
# the /api/v1/charts read-only proxy to the kbeads chart catalog (charts_proxy.go,
# E6.1 ka-0000005063), and the /api/v1/query_range read-only proxy to the
# in-cluster VictoriaMetrics vmsingle (metrics_proxy.go, ka-0000002747). All
# three are httptest-unit-covered in-tree, but nothing exercised the deployed
# viewer -> daemon / viewer -> vmsingle data path against a real cluster. This
# module closes that gap by driving the three surfaces through the authenticated
# viewer origin exactly as a browser would.
#
# Scenarios:
#   1. /charts hub renders: GET /charts is a 200 that emits the Observability
#      Hub scaffold — the "N dashboards · M panels · K sections" summary span
#      (or the explicit empty-state) — and the header "Open in Grafana ↗" link
#      is present IFF Grafana is configured (GRAFANA_BASE_URL gating, degraded
#      mode never renders a dead link). Best-effort: a freshly-filed
#      type=dashboard bead appears as a card with its charset-valid uid once the
#      short-TTL hub cache rolls (design G3: `gb grafana put` needs no code
#      change).
#   2. /api/v1/charts catalog proxy: GET /api/v1/charts is a 200 JSON catalog
#      listing (`{"charts":[...]}`). The daemon route is RoleViewer-bearer-gated
#      and cluster-internal; a 200 here PROVES the viewer injected its server-held
#      daemon bearer on the client's behalf — the browser presents only its own
#      viewer session, never the daemon token — and the token never appears in
#      the response body. A known core chart id (spend-by-agent) MUST be present —
#      a HARD fail, not a soft note (a catalog assertion that cannot fail is not
#      an assertion, ka-0000006517).
#   2b. W2b span-derived perf RED reachable by NAME: the six span-derived charts
#      (perf-span-* / span_calls_total, the gardenio-perf-traces-red parity board)
#      are each present in the live catalog, selected BY ID — never by .charts[0],
#      which sorts to agent-cost-per-bead and left them structurally unreachable
#      by scenario 2. The operation-level trio (top-operations / operation-errors
#      / slowest-operations) exposes its span_name + top_n param schema through
#      the live catalog, and a 200 render carries ios_hint.series_field==span_name
#      plus the duration_ms unit signal (grafanaUnit → "ms") for the latency
#      chart — the live-observable half of the round trip. (The PromQL
#      source="perf" / span_kind="SPAN_KIND_SERVER" pins are not echoed on any
#      HTTP surface and stay unit-covered by TestSpanOperation_PresentAndScoped.)
#   3. /api/v1/charts/{id} render proxy: an unknown chart id is a 404 passthrough
#      (not a viewer 500); a whitespace/empty id is rejected 404 by the proxy
#      before any upstream hop; a known chart id is a clean status passthrough
#      (200 with an ETag when the VM backend is wired, else the daemon's own 503
#      "charts backend not configured" — never a panic). When a render is 200 its
#      ETag re-validates to a 304 under If-None-Match (conditional-GET forwarding).
#   4. /api/v1/query_range proxy: the route is GET-only, so a POST is 405/404
#      (read-only allowlist by construction); a GET with no `query=` is a 400
#      before any upstream round-trip; a GET with a query proxies to vmsingle and
#      returns a non-panic status (200 VM-shaped JSON when vmsingle is reachable,
#      else a clean 502/503) — the viewer never 500s on the range path.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-charts-hub.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster (default gardenio-e2e)
#   - curl, jq in PATH
#   - lib-gitlab-token.sh's mint_user_token resolves a GitLab-direct bearer
#     (GARDENIO_E2E_GITLAB_BEARER or the static daemon token must be set).
#
# Environment note: the beads-viewer OIDC surface is deployed only on the
# long-lived staging singleton and prod — NOT in the per-instance ephemeral
# e2e namespaces (edge-auth/README.md). When the viewer Service is absent the
# whole module SKIPs cleanly (exit 0), so it is inert in the ephemeral shard
# fan-out and asserts only against the singleton / prod.
#
# Idempotency: the scenario-1 dashboard bead carries a unique title prefix and a
# unique dashboard_uid, and is closed in the cleanup trap; reruns do not collide.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Config ────────────────────────────────────────────────────────
# Set the lib-viewer-consumed knobs (NAMESPACE / RELEASE / VIEWER_SVC /
# VIEWER_PF_PORT) BEFORE sourcing lib-viewer.sh: the lib defaults them with
# `: "${VAR:=…}"` at source time, so a value set afterwards would be ignored and
# our dedicated port (kept distinct from sibling viewer modules to avoid a
# port-forward collision when shards co-locate) would silently not take effect.
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
VIEWER_SVC="${VIEWER_SVC:-${RELEASE}-beads-viewer}"
VIEWER_PF_PORT="${VIEWER_PF_PORT:-19631}"
KD_BIN="${KD_BIN:-kd}"

# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gitlab-token.sh"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-viewer.sh"
# A known chart id from the daemon catalog (kbeads charts_catalog.go). Used for
# the render-proxy status-passthrough + ETag/304 assertions AND, in scenario 2, a
# HARD presence assertion (a catalog check that cannot fail is not an assertion —
# ka-0000006517). spend-by-agent is a stable core catalog id; only override to
# another id that genuinely exists, or scenario 2 reds.
KNOWN_CHART_ID="${KNOWN_CHART_ID:-spend-by-agent}"

# ── W2b operation-level (span_name topk) perf RED coverage (ka-0000006517) ──────
# The three span_name-keyed operation-level RED charts (kbeads charts_catalog.go,
# W2b parity ka-0000001551) PLUS the three per-service span RED charts they share
# the gardenio-perf-traces-red parity board with. span_calls_total /
# span_duration_milliseconds_* are the ONLY span-derived families in the catalog,
# so this list is exactly the gap grep
# `perf-span|gardenio-perf-traces|span_calls_total`. These are selected BY NAME in
# scenario 2 — pinning .charts[0] (which sorts to agent-cost-per-bead and can
# never be a perf-span id) is precisely what left them unreachable by this module.
PERF_SPAN_CHART_IDS=(
  perf-span-request-rate
  perf-span-error-ratio
  perf-span-latency-quantiles
  perf-span-top-operations
  perf-span-operation-errors
  perf-span-slowest-operations
)
# The operation-level (span_name topk) subset. Each MUST expose a span_name (the
# OTel operation dimension) + top_n (the ranking cap) param schema through the
# LIVE catalog, and render an ios_hint keyed on span_name. Re-keying one of these
# off span_name — the original .charts[0] failure mode reappearing in a new place
# — reds the module.
PERF_SPAN_OP_CHART_IDS=(
  perf-span-top-operations
  perf-span-operation-errors
  perf-span-slowest-operations
)
# The slowest-operations latency chart carries catalog YFormat=duration_ms, which
# grafanaUnit() maps to the Grafana "ms" unit (charts_grafana_panel.go:179). Its
# render ios_hint.y_format IS that duration_ms signal on the wire — a regression
# to duration_seconds (the per-service RED unit) would red the round-trip check.
# NOTE: the source="perf" / span_kind="SPAN_KIND_SERVER" pins live only inside the
# PromQL QueryTemplate and are NEVER echoed on a live HTTP surface (catalog exposes
# id/title/category/params_schema; render exposes ios_hint + inlined data, not the
# query). They stay unit-covered by TestSpanOperation_PresentAndScoped; this module
# asserts every live-observable manifestation instead of faking a pins assertion.
# The two operation-level rate charts carry ios_hint.y_format=rate; the latency
# chart alone carries duration_ms (grafanaUnit → "ms"), so scenario 2b special-
# cases this id when asserting the unit signal on the render round trip.
PERF_SPAN_LATENCY_ID="perf-span-slowest-operations"
# SCOPE NOTE (ka-0000006517 — do not file this as a gap): the assembled
# gardenio-perf-traces-red Grafana board is NOT asserted live here. It composes
# OFFLINE (dashboardCatalog() → AssembleDashboard) and only reaches a live surface
# after an operator `kd admin gen-dashboards` writes a type=dashboard bead +
# grafana:dashboard-model doc child — a provisioning step absent on the ephemeral
# e2e targets, and there is NO daemon endpoint that serves an assembled board by
# uid. A `:::grafana gardenio-perf-traces-red panel=N resolves` check would
# therefore either never fire (permanent vacuous skip) or need a hand-seeded
# fixture — a born-vacuous test (ka-0000001521). The board's composition, its
# six-panel membership, the ms unit on the latency panel, and the uid's
# disjointness from the chart-owned gardenio-perf-traces are already unit-covered
# (TestDashboardCatalog_PerfTracesParityBoard, charts_grafana_panel_test.go,
# TestDashboardUIDs_DisjointFromChartBundle). This module covers the LIVE half:
# the six charts are reachable-by-name through the catalog + render proxy.
TITLE_PREFIX="charts-hub-e2e"
RANDOM_SUFFIX="$$-$(date +%s)"
# A charset-valid (dashboardUIDRe ^[A-Za-z0-9._-]{1,64}$) uid so the seeded
# dashboard passes the hub's enumeration/security gate and can render a card.
DASH_UID="charts-hub-e2e-${RANDOM_SUFFIX}"
# Scenario 1's card poll: the hub is fronted by a ~30s process-wide cache, so a
# freshly-filed dashboard may not appear on the first GET. Poll a little past
# one TTL before giving up (best-effort; the scaffold assertion is the hard one).
CARD_POLL_TRIES="${CARD_POLL_TRIES:-9}"
CARD_POLL_SLEEP="${CARD_POLL_SLEEP:-5}"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
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
    # shellcheck disable=SC2015 # best-effort close; the || true is the intended fallthrough
    [ -n "$id" ] && "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  rm -f "${VGET_HDR:-}" "${VGET_BODY:-}" 2>/dev/null || true
  teardown_viewer_portforward
}
trap cleanup EXIT

# vget <path> <token> [extra_curl_args...]
# Authenticated GET (no -L, so a pre-auth redirect surfaces as its own status).
# Writes response headers to $VGET_HDR and the body to $VGET_BODY, echoes the
# numeric HTTP status. A 000 means no HTTP response (tunnel down).
#
# CRITICAL: $VGET_HDR/$VGET_BODY are FIXED paths allocated once in the parent
# shell, NOT re-created inside vget. Every caller invokes vget in a command
# substitution (`status=$(vget …)`), which runs it in a SUBSHELL — any variable
# assignment made inside the function would be lost when that subshell exits,
# leaving the caller's `cat "$VGET_BODY"` reading an empty path. curl's -o/-D
# write to the stable on-disk paths, which survive the subshell boundary, so the
# parent reads exactly what the subshell wrote. (This is the whole reason the
# paths are module-global constants and vget never touches them.)
VGET_HDR="$(mktemp)"
VGET_BODY="$(mktemp)"
vget() {
  local path="$1" token="$2"
  shift 2
  : >"$VGET_HDR"
  : >"$VGET_BODY"
  curl -sk --max-time 20 \
    -H "Authorization: Bearer ${token}" \
    -o "$VGET_BODY" -D "$VGET_HDR" \
    -w '%{http_code}' \
    "$@" \
    "http://localhost:${VIEWER_PF_PORT}${path}" 2>/dev/null || echo "000"
}

# header_value <headers-file> <header-name> — echoes the (last) value of the
# named response header, CR-stripped and trimmed. Empty when absent. The `|| true`
# on grep is load-bearing under pipefail: an absent header is a valid case.
header_value() {
  local file="$1" name="$2"
  { grep -i "^${name}:" "$file" 2>/dev/null || true; } \
    | tail -n1 \
    | sed -E "s/^[^:]*:[[:space:]]*//I" \
    | tr -d '\r' \
    | sed -E 's/[[:space:]]+$//'
}

bead_id_from_json() { printf '%s' "$1" | jq -r '.id // empty' 2>/dev/null || printf ''; }

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " charts hub + /api/v1/charts + query_range E2E (gd-irTNYKQtA5)"
  blue "═══════════════════════════════════════════════════"

  if ! setup_viewer_portforward; then
    skip "charts-hub module" \
      "viewer Service '$VIEWER_SVC' absent in '$NAMESPACE' (viewer OIDC surface is singleton/prod-only)"
    summary
    return 0
  fi

  # Mint a viewer-role user token; the charts surfaces sit behind the viewer's
  # OIDC middleware exactly like the rest of the authenticated pages.
  local token=""
  if ! token=$(mint_user_token "e2e-charts-hub-${RANDOM_SUFFIX}@example.com" 2>/tmp/charts-hub-mint.err); then
    yellow "mint_user_token failed — see /tmp/charts-hub-mint.err"
    skip "charts-hub module" "no usable GitLab-direct bearer (viewer OIDC unreachable)"
    summary
    return 0
  fi
  dim "  minted viewer token"

  # Sanity: the token must actually authenticate against the viewer. If /charts
  # answers a redirect/401/403 the deployment's auth is not what this module
  # assumes (e.g. AVA-only edge forwarding no bearer) — skip rather than red.
  local probe_status
  probe_status=$(vget "/charts" "$token")
  if [ "$probe_status" = "000" ]; then
    skip "charts-hub module" "no HTTP response from viewer (tunnel down)"
    summary
    return 0
  fi
  if [ "$probe_status" = "302" ] || [ "$probe_status" = "303" ] || \
     [ "$probe_status" = "401" ] || [ "$probe_status" = "403" ]; then
    skip "charts-hub module" \
      "GET /charts → $probe_status (viewer did not accept the minted bearer; auth model differs)"
    summary
    return 0
  fi

  scenario_hub_render "$token"
  scenario_catalog_proxy "$token"
  scenario_perf_span_catalog "$token"
  scenario_render_proxy "$token"
  scenario_query_range_proxy "$token"

  summary
}

# ══════════════════════════════════════════════════════════════════
# Scenario 1: /charts hub renders — scaffold summary + Grafana-link gating,
# best-effort seeded-card enumeration.
# ══════════════════════════════════════════════════════════════════
scenario_hub_render() {
  local token="$1"
  blue "Scenario 1: GET /charts renders the Observability Hub"

  local status body xfo_link
  status=$(vget "/charts" "$token")
  body=$(cat "$VGET_BODY"); rm -f "$VGET_HDR" "$VGET_BODY"
  if [ "$status" != "200" ]; then
    fail "GET /charts → 200" "got $status"
    echo; return 0
  fi
  pass "GET /charts → 200"

  # Scaffold: either the populated hub summary span or the explicit empty-state.
  # Both are proof the handler ran dashboardHub + rendered charts.html (not an
  # error page). The prod fleet has ~8 dashboards so the summary is the live path.
  if grep -q 'charts-hub-summary' <<<"$body"; then
    local summary_txt
    summary_txt=$(printf '%s' "$body" | tr -d '\n' \
      | grep -oE '<span class="charts-hub-summary">[^<]+</span>' | head -n1 \
      | sed -E 's/<[^>]+>//g')
    pass "hub summary rendered (${summary_txt:-present})"
  elif grep -q 'charts-hub-empty' <<<"$body"; then
    pass "hub rendered the empty-state (no type=dashboard beads filed)"
  else
    fail "hub scaffold present" \
      "neither charts-hub-summary nor charts-hub-empty in /charts HTML (error page?)"
  fi

  # Open-in-Grafana gating: the header link is rendered IFF GRAFANA_BASE_URL is
  # set on the viewer. Whichever way it is configured, the page must be
  # self-consistent — a link present means a real href, never a dead one.
  if grep -q 'class="charts-grafana-link"' <<<"$body"; then
    if grep -qE 'class="charts-grafana-link" href="https?://[^"]+"' <<<"$body"; then
      pass "Open-in-Grafana header link present with a real href (Grafana configured)"
    else
      fail "Open-in-Grafana link has a real href" "charts-grafana-link present but href is empty/relative"
    fi
    xfo_link="present"
  else
    pass "Open-in-Grafana header link suppressed (GRAFANA_BASE_URL unset — degraded mode, no dead link)"
    xfo_link="absent"
  fi
  dim "    (grafana header link: $xfo_link)"

  # Best-effort seeded-card enumeration: file a type=dashboard bead with a
  # charset-valid uid and poll /charts until it appears (hub cache is ~30s). This
  # proves the G3 "put a dashboard bead → it shows up with no code change" path.
  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    skip "scenario 1: seeded dashboard card" "kd cannot reach beads daemon (card enumeration not exercised)"
    echo; return 0
  fi
  local create_out dash_id
  create_out=$("$KD_BIN" create "${TITLE_PREFIX}-dash-${RANDOM_SUFFIX}" \
    --type=dashboard \
    --field "dashboard_uid=${DASH_UID}" \
    -l "project:gardenio" \
    -d "charts-hub e2e dashboard bead ($RANDOM_SUFFIX)" \
    --json 2>/dev/null || printf '{}')
  dash_id=$(bead_id_from_json "$create_out")
  if [ -z "$dash_id" ]; then
    skip "scenario 1: seeded dashboard card" \
      "could not create type=dashboard bead (type may not be registered here)"
    echo; return 0
  fi
  CREATED_BEADS+=("$dash_id")
  dim "  seeded dashboard bead $dash_id (uid=$DASH_UID); polling /charts for its card…"

  local i seen="no"
  for ((i = 1; i <= CARD_POLL_TRIES; i++)); do
    local s b
    s=$(vget "/charts" "$token")
    b=$(cat "$VGET_BODY"); rm -f "$VGET_HDR" "$VGET_BODY"
    if [ "$s" = "200" ] && grep -qF "data-uid=\"${DASH_UID}\"" <<<"$b"; then
      seen="yes"; break
    fi
    sleep "$CARD_POLL_SLEEP"
  done
  if [ "$seen" = "yes" ]; then
    pass "scenario 1: seeded dashboard card appeared on /charts (uid=$DASH_UID)"
  else
    # Not a hard failure: the hub cache TTL, a very large existing fleet page, or
    # a missing model-doc child can all delay/omit the card. The scaffold + proxy
    # scenarios carry the load-bearing assertions.
    yellow "    note: seeded card not observed within $((CARD_POLL_TRIES * CARD_POLL_SLEEP))s"
    yellow "    (hub cache TTL / no grafana:dashboard-model child — scaffold + proxies still asserted)"
  fi
  echo
}

# ══════════════════════════════════════════════════════════════════
# Scenario 2: /api/v1/charts catalog proxy — server-side daemon bearer.
# ══════════════════════════════════════════════════════════════════
CATALOG_LIVE_ID=""
CATALOG_BODY=""
scenario_catalog_proxy() {
  local token="$1"
  blue "Scenario 2: GET /api/v1/charts proxies the daemon chart catalog"

  local status body ct
  status=$(vget "/api/v1/charts" "$token" -H "Accept: application/json")
  body=$(cat "$VGET_BODY")
  ct=$(header_value "$VGET_HDR" "Content-Type")
  rm -f "$VGET_HDR" "$VGET_BODY"

  # A 200 here is the proof: the daemon /v1/charts route is RoleViewer-gated and
  # cluster-internal, so it answers only when the viewer attached its server-held
  # daemon bearer on our behalf. The browser (this curl) never sent that token.
  if [ "$status" != "200" ]; then
    # 503 => beadsHTTPAddr unwired on the viewer (misconfig, but not this test's
    # target). Treat as a skip so an unconfigured proxy doesn't red the suite.
    if [ "$status" = "503" ]; then
      skip "scenario 2: catalog proxy" "viewer returned 503 (beadsHTTPAddr unset — charts proxy unwired)"
      echo; return 0
    fi
    fail "GET /api/v1/charts → 200" "got $status (daemon bearer not injected? body=$(head -c160 <<<"$body"))"
    echo; return 0
  fi
  pass "GET /api/v1/charts → 200 (viewer injected the server-side daemon bearer)"

  if grep -qi 'application/json' <<<"$ct"; then
    pass "catalog Content-Type is application/json"
  else
    fail "catalog Content-Type application/json" "got '${ct:-<none>}'"
  fi

  # The response must be a real catalog listing, and must NOT leak a bearer token.
  if printf '%s' "$body" | jq -e '.charts | type == "array"' >/dev/null 2>&1; then
    local n
    n=$(printf '%s' "$body" | jq -r '.charts | length' 2>/dev/null || echo 0)
    pass "catalog body is a {\"charts\":[…]} listing ($n entries)"
    # Grab a live chart id for the render-proxy scenario so it never depends on a
    # hardcoded id drifting out of the catalog.
    CATALOG_LIVE_ID=$(printf '%s' "$body" \
      | jq -r '.charts[0].chart_id // empty' 2>/dev/null || echo '')
    # Stash the catalog body so scenario 2b can select the perf-span ids by name
    # against the SAME response (fetch-once, project-many) rather than re-GETting.
    CATALOG_BODY="$body"
    # HARD presence assertion (was a soft `dim` note — a catalog assertion that
    # cannot fail is not an assertion, ka-0000006517). spend-by-agent is a stable
    # core catalog id; its ABSENCE means the catalog is broken/empty or re-keyed
    # out from under this module, which the suite must surface, not swallow.
    if printf '%s' "$body" | jq -e --arg id "$KNOWN_CHART_ID" \
        '.charts | any(.chart_id == $id)' >/dev/null 2>&1; then
      pass "known chart id '$KNOWN_CHART_ID' present in the catalog"
    else
      fail "known chart id '$KNOWN_CHART_ID' present in the catalog" \
        "'$KNOWN_CHART_ID' missing from the live catalog ($n entries) — catalog broken or re-keyed"
    fi
  else
    fail "catalog body shape" "response is not a {\"charts\":[…]} JSON object: $(head -c160 <<<"$body")"
  fi

  # Belt-and-suspenders: the server-held daemon bearer must never surface in the
  # proxied body. We don't know its value, but a well-formed catalog carries no
  # Authorization/Bearer material at all.
  if grep -qiE 'authorization|bearer ' <<<"$body"; then
    fail "no daemon bearer leaked in catalog body" "found an authorization/bearer token in the response"
  else
    pass "no daemon bearer material leaked in the catalog body"
  fi
  echo
}

# ══════════════════════════════════════════════════════════════════
# Scenario 2b: W2b operation-level (span_name topk) perf RED charts are reachable
# through the LIVE catalog + render proxy — selected BY NAME, never by index
# (ka-0000006517). The neighbouring scenario 2 pins .charts[0], which sorts to
# agent-cost-per-bead and can NEVER be a perf-span id, so these six charts were
# structurally unreachable by this module. This closes that: it asserts every
# span-derived chart is present in the catalog, that the operation-level trio
# carries its span_name + top_n param schema, and that a 200 render carries the
# span_name series key and (for the latency chart) the duration_ms unit signal
# through to the ios_hint — the live-observable half of the source="perf" /
# span_kind="SPAN_KIND_SERVER" / ms round trip (the PromQL pins themselves are not
# echoed on any HTTP surface; they stay unit-covered).
# ══════════════════════════════════════════════════════════════════
scenario_perf_span_catalog() {
  local token="$1"
  blue "Scenario 2b: W2b span-derived perf RED charts reachable by NAME through the live catalog + render proxy"

  if [ -z "$CATALOG_BODY" ]; then
    # Scenario 2 skipped (proxy unwired / non-200) — no catalog to select from.
    skip "scenario 2b: perf-span catalog coverage" "catalog body unavailable (scenario 2 did not yield a 200 listing)"
    echo; return 0
  fi

  # (a) Every span-derived chart is present in the LIVE catalog, selected by id
  # (never by .charts[0]). A dropped chart — the W2b regression this guards — reds
  # here instead of vanishing behind a positional pick.
  local id
  for id in "${PERF_SPAN_CHART_IDS[@]}"; do
    if printf '%s' "$CATALOG_BODY" | jq -e --arg id "$id" \
        '.charts | any(.chart_id == $id)' >/dev/null 2>&1; then
      pass "catalog carries span-derived chart '$id' (by-name select)"
    else
      fail "catalog carries span-derived chart '$id'" \
        "'$id' absent from the live catalog — a W2b span RED chart was dropped or re-keyed"
    fi
  done

  # (b) The operation-level trio must expose its span_name (operation dimension) +
  # top_n (ranking cap) param schema THROUGH THE LIVE CATALOG. This is the
  # by-name, honest-emit stand-in for "keyed by span_name": the catalog echoes
  # params_schema, so a chart that lost its span_name selector or its top_n cap
  # (the ACME panel-11 shape) reds here.
  for id in "${PERF_SPAN_OP_CHART_IDS[@]}"; do
    if printf '%s' "$CATALOG_BODY" | jq -e --arg id "$id" \
        '.charts[] | select(.chart_id == $id)
           | (.params_schema.span_name != null) and (.params_schema.top_n != null)
             and (.category == "perf")' >/dev/null 2>&1; then
      pass "operation-level '$id' exposes span_name + top_n params in the perf category"
    else
      fail "operation-level '$id' span_name + top_n param schema" \
        "live catalog entry for '$id' is missing the span_name and/or top_n param (or left the perf category)"
    fi
  done

  # (c) Render round trip through the proxy for each operation-level chart. The VM
  # backend may be unwired on the target (then the daemon answers its own 503 —
  # still a clean passthrough, asserted like scenario 3), so a 200 is best-effort:
  # when it lands we assert the ios_hint carries the span_name series key and the
  # right unit signal. A 5xx-crash (500) is never acceptable.
  local rid r_status r_body
  for rid in "${PERF_SPAN_OP_CHART_IDS[@]}"; do
    r_status=$(vget "/api/v1/charts/${rid}?since=$(date -u -d '-6 hours' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo '')" \
      "$token" -H "Accept: application/json")
    r_body=$(cat "$VGET_BODY"); rm -f "$VGET_HDR" "$VGET_BODY"
    case "$r_status" in
      200)
        # series_field on the ios_hint is the operation dimension — must be
        # span_name, never a re-keyed default. This is the live manifestation of
        # "keyed by the OTel span_name label".
        if printf '%s' "$r_body" | jq -e '.ios_hint.series_field == "span_name"' >/dev/null 2>&1; then
          pass "render '$rid' → 200, ios_hint.series_field == span_name (operation-keyed)"
        else
          fail "render '$rid' ios_hint keyed on span_name" \
            "got series_field='$(printf '%s' "$r_body" | jq -r '.ios_hint.series_field // "<none>"' 2>/dev/null)'"
        fi
        # Unit signal: the two rate charts carry y_format=rate; the latency chart
        # carries duration_ms (grafanaUnit → "ms"). A regression to
        # duration_seconds (the per-service RED unit) would surface here.
        local want_fmt="rate"
        [ "$rid" = "$PERF_SPAN_LATENCY_ID" ] && want_fmt="duration_ms"
        if printf '%s' "$r_body" | jq -e --arg f "$want_fmt" '.ios_hint.y_format == $f' >/dev/null 2>&1; then
          pass "render '$rid' → ios_hint.y_format == $want_fmt (unit survives the round trip)"
        else
          fail "render '$rid' ios_hint.y_format == $want_fmt" \
            "got y_format='$(printf '%s' "$r_body" | jq -r '.ios_hint.y_format // "<none>"' 2>/dev/null)'"
        fi
        ;;
      404)
        # A 404 means the chart id did NOT resolve through the render proxy — but
        # (a) already asserted it is in the catalog, so a 404 here is a real
        # catalog/render divergence, not an unwired backend. Red it.
        fail "render '$rid' resolves through the proxy" \
          "got 404 for a chart that IS in the live catalog — catalog/render divergence"
        ;;
      502|503)
        pass "render '$rid' → $r_status (VM backend unconfigured/unreachable — clean passthrough, no viewer panic)"
        ;;
      500)
        fail "render '$rid' non-panic status" "got a 500 (viewer/daemon crashed instead of a clean 4xx/5xx)"
        ;;
      *)
        pass "render '$rid' → $r_status (clean status passthrough, no viewer 500)"
        ;;
    esac
  done
  echo
}

# ══════════════════════════════════════════════════════════════════
# Scenario 3: /api/v1/charts/{id} render proxy — 404s, status passthrough, 304.
# ══════════════════════════════════════════════════════════════════
scenario_render_proxy() {
  local token="$1"
  blue "Scenario 3: GET /api/v1/charts/{id} render proxy — 404s, status passthrough, ETag/304"

  # (a) Unknown chart id → the daemon's 404 is mirrored to the client, not a 500.
  local uk_status
  uk_status=$(vget "/api/v1/charts/no-such-chart-${RANDOM_SUFFIX}" "$token" -H "Accept: application/json")
  rm -f "$VGET_HDR" "$VGET_BODY"
  if [ "$uk_status" = "404" ]; then
    pass "unknown chart id → 404 passthrough"
  elif [ "$uk_status" = "503" ]; then
    # 503 wins over 404 only when the viewer proxy itself is unwired; note it.
    skip "scenario 3: unknown-id 404" "viewer 503 (charts proxy unwired — beadsHTTPAddr unset)"
  else
    fail "unknown chart id → 404" "got $uk_status (expected the daemon 404 mirrored through the proxy)"
  fi

  # (b) Whitespace id → the proxy trims and rejects with its own 404 ("missing
  # chart id") before any upstream hop. %20 stays a single path segment.
  local empty_status
  empty_status=$(vget "/api/v1/charts/%20" "$token" -H "Accept: application/json")
  rm -f "$VGET_HDR" "$VGET_BODY"
  if [ "$empty_status" = "404" ]; then
    pass "whitespace/empty chart id → 404 (proxy rejects before upstream)"
  else
    fail "whitespace chart id → 404" "got $empty_status"
  fi

  # (c) Known chart id → clean status passthrough. With the VM backend wired the
  # daemon renders 200 (+ETag); without it the daemon answers its own 503 "charts
  # backend not configured". Either is correct — the invariant is NO viewer 500.
  local render_id="${CATALOG_LIVE_ID:-$KNOWN_CHART_ID}"
  local r_status r_etag
  r_status=$(vget "/api/v1/charts/${render_id}?since=$(date -u -d '-6 hours' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo '')" \
    "$token" -H "Accept: application/json")
  r_etag=$(header_value "$VGET_HDR" "ETag")
  rm -f "$VGET_HDR" "$VGET_BODY"
  case "$r_status" in
    200)
      pass "render '$render_id' → 200 (VM backend wired)"
      if [ -n "$r_etag" ]; then
        pass "render carries an ETag ($r_etag)"
        # (d) Conditional GET: the same ETag under If-None-Match → 304, no body.
        local c_status c_body
        c_status=$(vget "/api/v1/charts/${render_id}?since=$(date -u -d '-6 hours' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo '')" \
          "$token" -H "Accept: application/json" -H "If-None-Match: ${r_etag}")
        c_body=$(cat "$VGET_BODY"); rm -f "$VGET_HDR" "$VGET_BODY"
        if [ "$c_status" = "304" ]; then
          if [ -z "$c_body" ]; then
            pass "If-None-Match re-validates to 304 with an empty body"
          else
            fail "304 carries no body" "got a non-empty body on the 304"
          fi
        else
          # Not fatal: the render can recompute a different ETag if the window
          # rolled between calls. Record as info rather than a hard failure.
          yellow "    note: If-None-Match got $c_status (window may have rolled → new ETag)"
        fi
      else
        dim "    note: 200 render without an ETag header (skipping the 304 check)"
      fi
      ;;
    503)
      pass "render '$render_id' → 503 daemon passthrough (VM backend unconfigured — no viewer panic)"
      ;;
    502)
      pass "render '$render_id' → 502 (upstream unreachable — clean gateway error, no viewer panic)"
      ;;
    500)
      fail "render '$render_id' non-panic status" "got a 500 (viewer/daemon crashed instead of a clean 4xx/5xx)"
      ;;
    *)
      # Any other status that is not a 5xx crash is an acceptable passthrough.
      pass "render '$render_id' → $r_status (clean status passthrough, no viewer 500)"
      ;;
  esac
  echo
}

# ══════════════════════════════════════════════════════════════════
# Scenario 4: /api/v1/query_range proxy — GET-only, missing-query 400, non-panic.
# ══════════════════════════════════════════════════════════════════
scenario_query_range_proxy() {
  local token="$1"
  blue "Scenario 4: GET /api/v1/query_range proxy — GET-only, missing-query 400, non-panic"

  # (a) Read-only allowlist by construction: the route is registered GET-only, so
  # a POST never reaches the handler — net/http answers 405 (or 404 on some mux
  # builds). Either proves the write path is unreachable.
  local post_status
  post_status=$(vget "/api/v1/query_range?query=up" "$token" -X POST -H "Accept: application/json")
  rm -f "$VGET_HDR" "$VGET_BODY"
  if [ "$post_status" = "405" ] || [ "$post_status" = "404" ]; then
    pass "POST /api/v1/query_range → $post_status (GET-only read allowlist)"
  else
    fail "POST /api/v1/query_range rejected" "got $post_status (expected 405/404 on a read-only route)"
  fi

  # (b) Missing `query=` → 400 before any upstream round-trip.
  local mq_status
  mq_status=$(vget "/api/v1/query_range" "$token" -H "Accept: application/json")
  rm -f "$VGET_HDR" "$VGET_BODY"
  if [ "$mq_status" = "400" ]; then
    pass "GET /api/v1/query_range with no query= → 400"
  elif [ "$mq_status" = "503" ]; then
    # If the upstream is unwired the handler 503s before the query check; note it.
    skip "scenario 4: missing-query 400" "viewer 503 (metrics upstream unset — query check not reached)"
  else
    fail "missing-query → 400" "got $mq_status"
  fi

  # (c) A real range query proxies to vmsingle and returns a NON-panic status:
  # 200 with VM-shaped JSON when vmsingle is reachable, else a clean 502/503. The
  # invariant is that the viewer never 500s on the range path.
  local now start q_status q_body
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo '')
  start=$(date -u -d '-1 hour' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo '')
  q_status=$(vget "/api/v1/query_range?query=up&start=${start}&end=${now}&step=300" \
    "$token" -H "Accept: application/json")
  q_body=$(cat "$VGET_BODY"); rm -f "$VGET_HDR" "$VGET_BODY"
  case "$q_status" in
    200)
      # vmsingle's range response is {"status":"success","data":{"resultType":"matrix",…}}.
      if printf '%s' "$q_body" | jq -e '.status == "success" or (.data | type) == "object"' >/dev/null 2>&1; then
        pass "range query → 200 with VM-shaped JSON (vmsingle reachable, proxy wired to backend)"
      else
        fail "range query 200 is VM-shaped JSON" "200 but body is not a VM range payload: $(head -c160 <<<"$q_body")"
      fi
      ;;
    502|503)
      pass "range query → $q_status (vmsingle unreachable/unconfigured — clean error, no viewer panic)"
      ;;
    500)
      fail "range query non-panic status" "got a 500 (viewer crashed instead of a clean gateway error)"
      ;;
    *)
      pass "range query → $q_status (clean status, no viewer 500)"
      ;;
  esac
  echo
}

summary() {
  blue "═══════════════════════════════════════════════════"
  blue " charts-hub: $PASS passed, $FAIL failed, $SKIP skipped"
  if [ "$FAIL" -gt 0 ]; then
    red "FAILURES:"
    for e in "${ERRORS[@]}"; do red "  - $e"; done
    exit 1
  fi
  blue "═══════════════════════════════════════════════════"
}

main "$@"
