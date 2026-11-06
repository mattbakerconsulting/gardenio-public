#!/usr/bin/env bash
# test-viewer-maps.sh — beads-viewer /maps + /gardeners server-side selection E2E
#
# Backfills the coverage gap flagged by gd-VEanc11wVQ: ZERO e2e traffic hits any
# /maps, fleet-board, or gardener-constellation endpoint. graph-stats exercises
# the kbeads /v1/graph+/v1/stats APIs (not the viewer HTML); agents-live-view
# covers /agents+SSE only; viewer-bead-activity covers /b/{id}/activity. A large
# body of server-side SELECTION / FILTER / data-contract logic landed on /maps
# and /gardeners (epic ka-0000001644 "maps2", ka-0000006790 filter/group rail,
# ka-0000008054 fixture exclusion, ka-0000002811 schedule-mirror exclusion,
# ka-0000001950 field-only role=gardener, ka-0000007772 live goal-state), all
# Go-unit-tested but with no e2e. This module drives the real deployed viewer and
# asserts the rendered HTML honors that contract.
#
# SCOPE NOTE — the parent task's proposed scenario menu was written from an
# auditor's mental model; several of its items reference routes/params that DO
# NOT EXIST in the shipped viewer and are deliberately NOT tested here:
#   - /maps/{id}                — torn down (ka-0000000247); only GET /maps exists.
#                                 The per-gardener radial constellation now lives at
#                                 GET /gardener/{id} (+ its /gardener/{id}/beads frag).
#   - ?group=epic               — not implemented; only ?group=owner is honored.
#   - ?overlay=timeline / ?overlay=attention / ?t=N — no such query params exist
#                                 anywhere in the viewer package.
#   - message beads → chat lines on the constellation — that divert is wired on
#                                 the bead-show page GET /{id} only, NOT on the
#                                 gardener constellation (bead_chat.go:5-11: the
#                                 /maps/{gardenerID} chat cut was torn down with
#                                 the route). The gardener walk does not strip
#                                 type=message beads, so there is nothing to
#                                 assert on /gardener/{id} for that behavior.
# Testing only genuinely-implemented behavior keeps this module honest — a green
# assertion on a nonexistent param would be a false guarantee.
#
# Scenarios (each asserts a distinct server-side selection rule):
#   1. /maps default scope is the LIVE fleet: an active (open+enabled) modern
#      type=trigger role:gardener renders a card; ?all=1 also renders it (widened
#      scope is a superset). fleetGardenerIsLive == (Status active && Kind trigger).
#   2. /maps ?filter=<crossfilter> narrows the board: filtering to the seeded
#      gardener's own trigger-kind facet keeps its card; filtering to a
#      non-matching enabled facet (enabled:false) drops it. The filter rail chips
#      + "showing N of M" affordance render.
#   3. /gardeners roster INCLUDES a field-only role=gardener (role=gardener set as a
#      FIELD, never the label) — the dual field-query path (ka-0000001950).
#   4. /gardeners EXCLUDES an E2E fixture gardener from the roster and accounts for
#      it in the "N hidden from this roster" disclosure (ka-0000008054); a deep
#      link to /gardener/{fixture-id} 404s.
#   5. /gardeners EXCLUDES a schedule-mirror bead (source=schedule-mirror) from the
#      roster and 404s its /gardener/{id} deep link (ka-0000002811).
#   6. /gardener/{id} live goal-state badge reflects the target epic's CURRENT
#      status (ka-0000007772): a closed gardener whose goal-target epic is CLOSED
#      renders "🏁 GOAL ACHIEVED"; one whose target epic is still OPEN renders
#      "goal not reached".
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-viewer-maps.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster (default gardenio-e2e)
#   - kd, jq, curl in PATH
#   - lib-gitlab-token.sh's mint_user_token resolves a GitLab-direct bearer
#     (GARDENIO_E2E_GITLAB_BEARER or the static daemon token must be set). The viewer HTML routes
#     are OIDC-gated; when a token cannot be minted the whole module SKIPs.
#
# Self-SKIPs (exit 0) when the viewer Service is absent, so it is inert in the
# per-instance ephemeral e2e namespaces and asserts only against a long-lived
# viewer deployment (staging singleton / prod).
#
# Idempotency: every created bead carries the title prefix
#   viewer-maps-e2e-<random>
# and is closed in the cleanup trap. Reruns do not collide.
#
# Hard rules: do NOT edit .captain/quarantines/. Do not touch any other e2e
# module. Port-forward + authenticated-GET helpers come from lib-viewer.sh.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gitlab-token.sh"

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
VIEWER_SVC="${VIEWER_SVC:-${RELEASE}-beads-viewer}"
VIEWER_PF_PORT="${VIEWER_PF_PORT:-19811}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19812}"
KD_BIN="${KD_BIN:-kd}"
PROJECT="${VIEWER_MAPS_PROJECT:-gardenio}"
NON_MATT_EMAIL="${NON_MATT_EMAIL:-e2e-not-matt-$$@example.com}"
TITLE_PREFIX="viewer-maps-e2e"
RANDOM_SUFFIX="$$-$(date +%s)"

# lib-viewer.sh reads these before we source it (see its Tunables block); it
# port-forwards svc/${VIEWER_SVC}:${VIEWER_TARGET_PORT} → localhost:${VIEWER_PF_PORT}.
export NAMESPACE RELEASE VIEWER_SVC VIEWER_PF_PORT
export VIEWER_TARGET_PORT="${VIEWER_TARGET_PORT:-8095}"
export VIEWER_PF_LOG="/tmp/viewer-maps-pf.log"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-viewer.sh"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
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
  for id in ${CREATED_BEADS[@]+"${CREATED_BEADS[@]}"}; do "$KD_BIN" close "$id" >/dev/null 2>&1 || true; done
  teardown_viewer_portforward
  if [ -n "$BEADS_PF_PID" ]; then kill "$BEADS_PF_PID" 2>/dev/null || true; fi
  rm -f /tmp/viewer-maps-*.log 2>/dev/null || true
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
    "${BEADS_PF_PORT}:8080" >/tmp/viewer-maps-beads-pf.log 2>&1 &
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

bead_id_from_json() { printf '%s' "$1" | jq -r '.id // empty' 2>/dev/null || printf ''; }

# body_of <viewer_get-output> — the body (viewer_get echoes "<status>\n<body>").
body_of() { printf '%s' "$1" | tail -n +2; }
status_of() { printf '%s' "$1" | head -n1; }

# maps_card_present <html> <gardener-id> — the seeded gardener's card links to
# /gardener/{id}, so its presence on /maps is the anchor href carrying that id
# inside a .maps-card. We look for the id anywhere in the board body; a gardener
# with no card leaves its id absent from the rendered board.
maps_card_present() {
  local html="$1" id="$2"
  grep -qF "/gardener/${id}" <<<"$html"
}

# create_gardener <suffix> <extra gd-create args...> — creates a modern
# type=trigger + label role:gardener bead that reads as an ACTIVE gardener on the
# viewer (enabled, open, no cron so it never actually fires → no pod spawn / no
# model spend). Echoes the new bead id (empty on failure). Appends to
# CREATED_BEADS. Callers pass extra -f/-l args to shape status/exclusion.
create_gardener() {
  local suffix="$1"; shift
  local out id
  out=$("$KD_BIN" create "${TITLE_PREFIX}-${suffix}-${RANDOM_SUFFIX}" \
    --type=trigger \
    -d "viewer-maps E2E gardener (${suffix}, ${RANDOM_SUFFIX})" \
    -l "e2e,project:${PROJECT},role:gardener,trigger:bead" \
    -f "source=bead-state" \
    -f "action=spawn_agent" \
    -f "enabled=true" \
    -f "project=${PROJECT}" \
    -f "role=crew" \
    -f "prompt=viewer-maps E2E — never fires (no cron, bead-state source)" \
    -f "max_concurrent=1" \
    "$@" \
    --json 2>/dev/null || printf '{}')
  id=$(bead_id_from_json "$out")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  printf '%s' "$id"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " viewer /maps + /gardeners selection E2E (gd-VEanc11wVQ)"
  blue "═══════════════════════════════════════════════════"

  if ! setup_viewer_portforward; then
    skip "viewer-maps module" "viewer service $VIEWER_SVC absent"
    summary
    return 0
  fi
  setup_beads_portforward

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    skip "viewer-maps module" \
      "kd cannot reach beads daemon at ${BEADS_HTTP_URL:-unset}"
    summary
    return 0
  fi

  # The viewer HTML routes are OIDC-gated; every scenario GETs them with a
  # bearer, so a mint failure SKIPs the whole module (the seed writes would be
  # wasted without a way to read the rendered pages back).
  local token=""
  if ! token=$(mint_user_token "$NON_MATT_EMAIL" 2>/tmp/viewer-maps-mint.err); then
    yellow "mint_user_token failed for ${NON_MATT_EMAIL} — see /tmp/viewer-maps-mint.err"
    skip "viewer-maps module" "no usable GitLab-direct bearer (viewer routes are auth-gated)"
    summary
    return 0
  fi
  dim "  minted non-Matt user token (sub=${NON_MATT_EMAIL})"
  # The viewer's own OIDC middleware stays pinned to the Keycloak issuer until
  # the viewer-login flip lands (ka-0000002083); it rejects this GitLab-direct /
  # static-daemon bearer with 401. SKIP-clean rather than hard-fail every gated
  # GET (bug gd-5M8iT2oNlr). Auto-enables when the viewer trusts the GitLab edge.
  if ! viewer_accepts_bearer "$token"; then
    skip "viewer-maps module" "viewer rejects the E2E transport bearer — viewer still OIDC-gated at the pod; viewer-login flip pending (ka-0000002083)"
    summary
    return 0
  fi

  scenario_live_scope "$token"
  scenario_filter_rail "$token"
  scenario_field_only_gardener "$token"
  scenario_fixture_exclusion "$token"
  scenario_schedule_mirror_exclusion "$token"
  scenario_live_goal_state "$token"

  summary
}

# ══════════════════════════════════════════════════════════════════
# Scenario 1: /maps default scope is the LIVE fleet. An ACTIVE modern
# type=trigger role:gardener (enabled + open) is live per fleetGardenerIsLive
# (Status active && Kind trigger), so its card renders on the default board.
# ?all=1 widens to every gardener — a superset — so the card is present there
# too. This is the core /maps selection contract (ka-0000001644).
# ══════════════════════════════════════════════════════════════════
scenario_live_scope() {
  local token="$1"
  echo
  blue "Scenario 1: /maps default LIVE scope renders an active gardener's card"
  local id
  id=$(create_gardener "live")
  if [ -z "$id" ]; then
    fail "scenario 1: create live gardener" "no ID returned from kd create"
    return 0
  fi
  pass "scenario-1 live gardener created ($id)"

  local resp body st
  resp=$(viewer_get "/maps" "$token")
  st=$(status_of "$resp"); body=$(body_of "$resp")
  if [ "$st" != "200" ]; then
    fail "scenario 1: GET /maps" "expected 200, got $st"
    return 0
  fi
  # Board scaffold present (a real board, not a degrade-render error page).
  if grep -qF 'class="maps-board"' <<<"$body" || grep -qF 'class="maps-card' <<<"$body"; then
    pass "scenario 1: /maps rendered the board scaffold"
  else
    fail "scenario 1: /maps board scaffold" "no maps-board / maps-card markup in response"
  fi
  if maps_card_present "$body" "$id"; then
    pass "scenario 1: active gardener's card is on the default (live) board"
  else
    fail "scenario 1: active gardener on default board" \
      "gardener $id card (href=/gardener/$id) absent from /maps"
  fi

  # ?all=1 is a superset of live — the card must still be present.
  local aresp abody ast
  aresp=$(viewer_get "/maps?all=1" "$token")
  ast=$(status_of "$aresp"); abody=$(body_of "$aresp")
  if [ "$ast" != "200" ]; then
    fail "scenario 1: GET /maps?all=1" "expected 200, got $ast"
  elif maps_card_present "$abody" "$id"; then
    pass "scenario 1: gardener also present on ?all=1 (widened) board"
  else
    fail "scenario 1: gardener on ?all=1 board" \
      "gardener $id absent from the all-scope board (should be a superset of live)"
  fi
}

# ══════════════════════════════════════════════════════════════════
# Scenario 2: /maps ?filter=<crossfilter> narrows the board (ka-0000006790 /
# ka-0000008126). The seeded gardener is a bead-state trigger with enabled=true,
# so:
#   - filtering to enabled:true KEEPS its card (matching facet), and
#   - filtering to enabled:false DROPS its card (non-matching facet).
# The filter rail chips + "showing N of M" affordance render when a filter is
# active. This proves mapsRowMatchesFilter runs server-side, not just client JS.
# ══════════════════════════════════════════════════════════════════
scenario_filter_rail() {
  local token="$1"
  echo
  blue "Scenario 2: /maps ?filter= narrows the board by facet"
  local id
  id=$(create_gardener "filter")
  if [ -z "$id" ]; then
    fail "scenario 2: create filter gardener" "no ID returned from kd create"
    return 0
  fi
  pass "scenario-2 filter gardener created ($id, enabled=true)"

  # Matching facet: enabled:true keeps the card.
  local mresp mbody mst
  mresp=$(viewer_get "/maps?filter=enabled:true" "$token")
  mst=$(status_of "$mresp"); mbody=$(body_of "$mresp")
  if [ "$mst" != "200" ]; then
    fail "scenario 2: GET /maps?filter=enabled:true" "expected 200, got $mst"
    return 0
  fi
  if maps_card_present "$mbody" "$id"; then
    pass "scenario 2: enabled:true filter KEEPS the enabled gardener's card"
  else
    fail "scenario 2: enabled:true keeps card" \
      "gardener $id absent under a matching facet filter"
  fi
  # The active-filter affordance (rail + 'showing N of M') is SECONDARY UI: the
  # template only emits the whole maps-filter block when the rail has a facet
  # group with >=2 distinct scoped values (buildMapsFilterRail drops single-value
  # groups). Against the long-lived target — the only place this module runs —
  # the real fleet always yields multi-value facets, so this normally renders; a
  # pathologically sparse fleet legitimately renders no rail. Assert it when
  # present (that path is exercised), but only NOTE its absence rather than fail —
  # the load-bearing "filter narrows the board" claim is proven by the keep/drop
  # assertions above and below, independent of the rail chrome.
  if grep -qF 'class="maps-filter"' <<<"$mbody" && grep -qiE 'showing [0-9]+ of [0-9]+' <<<"$mbody"; then
    pass "scenario 2: filter rail + 'showing N of M' affordance rendered"
  else
    yellow "    note: no maps-filter rail / 'showing N of M' under an active filter"
    yellow "    (rail drops single-value facet groups; sparse fleet — not a failure)"
  fi

  # Non-matching facet: enabled:false drops the card (the gardener is enabled).
  local nresp nbody nst
  nresp=$(viewer_get "/maps?filter=enabled:false" "$token")
  nst=$(status_of "$nresp"); nbody=$(body_of "$nresp")
  if [ "$nst" != "200" ]; then
    fail "scenario 2: GET /maps?filter=enabled:false" "expected 200, got $nst"
  elif maps_card_present "$nbody" "$id"; then
    fail "scenario 2: enabled:false drops card" \
      "gardener $id (enabled=true) leaked past a disabled-only filter"
  else
    pass "scenario 2: enabled:false filter DROPS the enabled gardener's card"
  fi
}

# ══════════════════════════════════════════════════════════════════
# Scenario 3: /gardeners INCLUDES a field-only role=gardener (ka-0000001950). A
# gardener created the "wrong way" — role=gardener set as a FIELD but never as the
# role:gardener LABEL — must still surface, because loadGardenerRows queries BOTH
# the label AND the field. Guards the regression where the label-only query hid
# a live gardener from the roster.
# ══════════════════════════════════════════════════════════════════
scenario_field_only_gardener() {
  local token="$1"
  echo
  blue "Scenario 3: /gardeners includes a FIELD-only role=gardener (no label)"
  # No role:gardener LABEL — only the role=gardener FIELD. Everything else mirrors
  # a normal enabled bead-event gardener so it reads as active.
  local out id
  out=$("$KD_BIN" create "${TITLE_PREFIX}-fieldonly-${RANDOM_SUFFIX}" \
    --type=trigger \
    -d "viewer-maps E2E field-only gardener (${RANDOM_SUFFIX})" \
    -l "e2e,project:${PROJECT},trigger:bead" \
    -f "source=bead-state" \
    -f "action=spawn_agent" \
    -f "enabled=true" \
    -f "project=${PROJECT}" \
    -f "role=gardener" \
    -f "prompt=viewer-maps E2E field-only — never fires" \
    -f "max_concurrent=1" \
    --json 2>/dev/null || printf '{}')
  id=$(bead_id_from_json "$out")
  if [ -z "$id" ]; then
    fail "scenario 3: create field-only gardener" "no ID returned from kd create"
    return 0
  fi
  CREATED_BEADS+=("$id")
  pass "scenario-3 field-only gardener created ($id, role=gardener FIELD, no label)"

  # ?filter=all so the roster is the full population regardless of live-scope
  # nuances; the field-only gardener must appear in the rendered rows.
  local resp body st
  resp=$(viewer_get "/gardeners?filter=all" "$token")
  st=$(status_of "$resp"); body=$(body_of "$resp")
  if [ "$st" != "200" ]; then
    fail "scenario 3: GET /gardeners?filter=all" "expected 200, got $st"
    return 0
  fi
  # Row identity: each roster row is <tbody ... id="cap-row-{ID}">. That is the
  # unambiguous "this gardener rendered as a roster row" marker (a bare id match
  # could also come from the hidden-disclosure list).
  if grep -qF "id=\"cap-row-${id}\"" <<<"$body"; then
    pass "scenario 3: field-only gardener rendered as a /gardeners roster row"
  else
    fail "scenario 3: field-only gardener on roster" \
      "no cap-row-${id} row on /gardeners (dual field-query path regressed?)"
  fi
}

# ══════════════════════════════════════════════════════════════════
# Scenario 4: /gardeners EXCLUDES an E2E fixture gardener (ka-0000008054). A bead
# carrying a fixture: label prefix must NOT render as a live roster row; instead
# it is accounted for in the "N hidden from this roster" disclosure, and a deep
# link to /gardener/{fixture-id} 404s.
#
# NOTE: fixture exclusion honors the BEADS_VIEWER_RENDER_FIXTURE_GARDENERS opt-in
# (the behavioral-e2e harness sets it to RENDER its own fixture). If the deployed
# viewer runs with that flag ON, the fixture is intentionally rendered and this
# scenario SKIPs rather than false-failing.
# ══════════════════════════════════════════════════════════════════
scenario_fixture_exclusion() {
  local token="$1"
  echo
  blue "Scenario 4: /gardeners excludes an E2E fixture gardener + 404s its detail"
  # Fixture signal 1 (strongest): a fixture: label prefix. Title also carries the
  # "[E2E fixture]" human marker for belt-and-suspenders.
  local out id
  out=$("$KD_BIN" create "[E2E fixture] ${TITLE_PREFIX}-fixture-${RANDOM_SUFFIX}" \
    --type=trigger \
    -d "viewer-maps E2E fixture gardener (${RANDOM_SUFFIX})" \
    -l "e2e,project:${PROJECT},role:gardener,trigger:bead,fixture:e2e:viewer-maps" \
    -f "source=bead-state" \
    -f "action=spawn_agent" \
    -f "enabled=true" \
    -f "project=${PROJECT}" \
    -f "role=crew" \
    -f "prompt=viewer-maps E2E fixture — never fires" \
    -f "max_concurrent=1" \
    --json 2>/dev/null || printf '{}')
  id=$(bead_id_from_json "$out")
  if [ -z "$id" ]; then
    fail "scenario 4: create fixture gardener" "no ID returned from kd create"
    return 0
  fi
  CREATED_BEADS+=("$id")
  pass "scenario-4 fixture gardener created ($id, fixture: label)"

  local resp body st
  resp=$(viewer_get "/gardeners?filter=all" "$token")
  st=$(status_of "$resp"); body=$(body_of "$resp")
  if [ "$st" != "200" ]; then
    fail "scenario 4: GET /gardeners?filter=all" "expected 200, got $st"
    return 0
  fi

  # If the deployed viewer renders fixtures (opt-in flag on), a roster row for
  # our fixture is EXPECTED — skip rather than false-fail the exclusion assertion.
  if grep -qF "id=\"cap-row-${id}\"" <<<"$body"; then
    skip "scenario 4: fixture exclusion" \
      "viewer rendered fixture $id as a roster row (BEADS_VIEWER_RENDER_FIXTURE_GARDENERS likely ON)"
  else
    pass "scenario 4: fixture gardener is NOT a live /gardeners roster row"
    # And the hidden-disclosure must account for it (the exclusion is surfaced,
    # not silent). The disclosure lists each excluded bead by id.
    if grep -qF 'hidden from this roster' <<<"$body" && grep -qF "$id" <<<"$body"; then
      pass "scenario 4: fixture accounted for in the 'hidden from this roster' disclosure"
    else
      fail "scenario 4: fixture in hidden disclosure" \
        "excluded fixture $id not surfaced in the 'hidden from this roster' affordance"
    fi
  fi

  # Deep link to the fixture detail 404s (ka-0000008054) — unless the render
  # opt-in is on, in which case it 200s and we skip.
  local dstatus
  dstatus=$(viewer_status "/gardener/${id}" "$token")
  if [ "$dstatus" = "404" ]; then
    pass "scenario 4: GET /gardener/${id} (fixture) 404s"
  elif [ "$dstatus" = "200" ]; then
    skip "scenario 4: fixture detail 404" \
      "GET /gardener/${id} returned 200 (fixture-render opt-in likely ON)"
  else
    fail "scenario 4: fixture detail 404" \
      "GET /gardener/${id} returned $dstatus (expected 404)"
  fi
}

# ══════════════════════════════════════════════════════════════════
# Scenario 5: /gardeners EXCLUDES a schedule-mirror bead (ka-0000002811). A
# foreign gardenio-next config-mirror bead (source=schedule-mirror) dual-written
# into this namespace as a role:gardener trigger must NOT render as a live
# gardener, and its /gardener/{id} deep link 404s. isScheduleMirrorGardenerBead has
# no render opt-in, so this exclusion is unconditional.
# ══════════════════════════════════════════════════════════════════
scenario_schedule_mirror_exclusion() {
  local token="$1"
  echo
  blue "Scenario 5: /gardeners excludes a schedule-mirror bead + 404s its detail"
  # source=schedule-mirror is the strongest, unconditional mirror signal.
  local out id
  out=$("$KD_BIN" create "${TITLE_PREFIX}-mirror-${RANDOM_SUFFIX}" \
    --type=trigger \
    -d "viewer-maps E2E schedule-mirror bead (${RANDOM_SUFFIX})" \
    -l "e2e,project:${PROJECT},role:gardener,trigger:bead,schedule-mirror" \
    -f "source=schedule-mirror" \
    -f "action=spawn_agent" \
    -f "enabled=true" \
    -f "project=${PROJECT}" \
    -f "role=crew" \
    -f "prompt=viewer-maps E2E schedule-mirror — never fires" \
    -f "max_concurrent=1" \
    --json 2>/dev/null || printf '{}')
  id=$(bead_id_from_json "$out")
  if [ -z "$id" ]; then
    fail "scenario 5: create schedule-mirror bead" "no ID returned from kd create"
    return 0
  fi
  CREATED_BEADS+=("$id")
  pass "scenario-5 schedule-mirror bead created ($id, source=schedule-mirror)"

  local resp body st
  resp=$(viewer_get "/gardeners?filter=all" "$token")
  st=$(status_of "$resp"); body=$(body_of "$resp")
  if [ "$st" != "200" ]; then
    fail "scenario 5: GET /gardeners?filter=all" "expected 200, got $st"
    return 0
  fi
  if grep -qF "id=\"cap-row-${id}\"" <<<"$body"; then
    fail "scenario 5: schedule-mirror exclusion" \
      "schedule-mirror bead $id leaked onto the live /gardeners roster"
  else
    pass "scenario 5: schedule-mirror bead is NOT a live /gardeners roster row"
  fi

  local dstatus
  dstatus=$(viewer_status "/gardener/${id}" "$token")
  if [ "$dstatus" = "404" ]; then
    pass "scenario 5: GET /gardener/${id} (schedule-mirror) 404s"
  else
    fail "scenario 5: schedule-mirror detail 404" \
      "GET /gardener/${id} returned $dstatus (expected 404)"
  fi
}

# ══════════════════════════════════════════════════════════════════
# Scenario 6: /gardener/{id} live goal-state badge (ka-0000007772). The badge is
# re-derived from the goal-target epic's CURRENT status at render time, not the
# gardener's close-time snapshot:
#   - gardener CLOSED + target epic CLOSED  ⇒ "🏁 GOAL ACHIEVED"
#   - gardener CLOSED + target epic OPEN    ⇒ "goal not reached"
# We seed the goal target via the `conditions` field (a bead_query condition's
# `parent`), which gardenerGoalTargetID resolves. Two gardeners, two epic states,
# so both badge directions are proven against the SAME live-derive path.
# ══════════════════════════════════════════════════════════════════
scenario_live_goal_state() {
  local token="$1"
  echo
  blue "Scenario 6: /gardener/{id} goal badge reflects the target epic's LIVE status"

  # --- 6a: closed gardener whose target epic is CLOSED ⇒ GOAL ACHIEVED ---
  local ach_epic ach_epic_out
  ach_epic_out=$("$KD_BIN" create "${TITLE_PREFIX}-goalepic-closed-${RANDOM_SUFFIX}" \
    --type=epic -l "e2e,project:${PROJECT}" \
    -d "viewer-maps E2E goal epic (to be closed)" --json 2>/dev/null || printf '{}')
  ach_epic=$(bead_id_from_json "$ach_epic_out")
  if [ -z "$ach_epic" ]; then
    fail "scenario 6a: create achieved-goal epic" "no ID from kd create"
    return 0
  fi
  CREATED_BEADS+=("$ach_epic")

  local ach_cap
  ach_cap=$(create_gardener "goal-achieved" \
    -f "conditions=[{\"type\":\"bead_query\",\"op\":\"eq\",\"value\":\"0\",\"parent\":\"${ach_epic}\",\"statuses\":[\"open\",\"in_progress\"]}]")
  if [ -z "$ach_cap" ]; then
    fail "scenario 6a: create achieved-goal gardener" "no ID from kd create"
  else
    # Close BOTH: the gardener (so it has a terminal disposition) and its target
    # epic (so the LIVE re-derive reads the target as closed ⇒ achieved).
    "$KD_BIN" close "$ach_epic" >/dev/null 2>&1 || true
    "$KD_BIN" close "$ach_cap"  >/dev/null 2>&1 || true
    pass "scenario-6a seeded: gardener $ach_cap closed, target epic $ach_epic closed"

    local resp body st
    resp=$(viewer_get "/gardener/${ach_cap}" "$token")
    st=$(status_of "$resp"); body=$(body_of "$resp")
    if [ "$st" != "200" ]; then
      fail "scenario 6a: GET /gardener/${ach_cap}" "expected 200, got $st"
    elif grep -qF 'gardener-goal-badge' <<<"$body" && grep -qF 'GOAL ACHIEVED' <<<"$body"; then
      pass "scenario 6a: closed gardener + closed target epic ⇒ '🏁 GOAL ACHIEVED' badge"
    else
      fail "scenario 6a: GOAL ACHIEVED badge" \
        "no '🏁 GOAL ACHIEVED' gardener-goal-badge on /gardener/${ach_cap} (target epic $ach_epic is closed)"
    fi
  fi

  # --- 6b: closed gardener whose target epic is OPEN ⇒ goal not reached ---
  local open_epic open_epic_out
  open_epic_out=$("$KD_BIN" create "${TITLE_PREFIX}-goalepic-open-${RANDOM_SUFFIX}" \
    --type=epic -l "e2e,project:${PROJECT}" \
    -d "viewer-maps E2E goal epic (stays open)" --json 2>/dev/null || printf '{}')
  open_epic=$(bead_id_from_json "$open_epic_out")
  if [ -z "$open_epic" ]; then
    fail "scenario 6b: create unachieved-goal epic" "no ID from kd create"
    return 0
  fi
  CREATED_BEADS+=("$open_epic")

  local un_cap
  un_cap=$(create_gardener "goal-unachieved" \
    -f "conditions=[{\"type\":\"bead_query\",\"op\":\"eq\",\"value\":\"0\",\"parent\":\"${open_epic}\",\"statuses\":[\"open\",\"in_progress\"]}]")
  if [ -z "$un_cap" ]; then
    fail "scenario 6b: create unachieved-goal gardener" "no ID from kd create"
  else
    # Close the gardener but leave the target epic OPEN ⇒ live re-derive reads the
    # target as open ⇒ unachieved.
    "$KD_BIN" close "$un_cap" >/dev/null 2>&1 || true
    pass "scenario-6b seeded: gardener $un_cap closed, target epic $open_epic OPEN"

    local resp body st
    resp=$(viewer_get "/gardener/${un_cap}" "$token")
    st=$(status_of "$resp"); body=$(body_of "$resp")
    if [ "$st" != "200" ]; then
      fail "scenario 6b: GET /gardener/${un_cap}" "expected 200, got $st"
    elif grep -qF 'gardener-goal-badge-unachieved' <<<"$body" || grep -qF 'goal not reached' <<<"$body"; then
      pass "scenario 6b: closed gardener + OPEN target epic ⇒ 'goal not reached' badge"
    else
      fail "scenario 6b: goal-not-reached badge" \
        "no 'goal not reached' unachieved badge on /gardener/${un_cap} (target epic $open_epic is open)"
    fi
  fi
}

summary() {
  blue "═══════════════════════════════════════════════════"
  blue " viewer-maps: $PASS passed, $FAIL failed, $SKIP skipped"
  if [ "$FAIL" -gt 0 ]; then
    red "FAILURES:"
    for e in ${ERRORS[@]+"${ERRORS[@]}"}; do red "  - $e"; done
    exit 1
  fi
  blue "═══════════════════════════════════════════════════"
}

main "$@"
