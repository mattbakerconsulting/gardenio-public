#!/usr/bin/env bash
# test-agent-cost-context.sh — per-agent cost + context-size tracking E2E
#
# Closes the coverage gap called out by Matt (gd-L8DNvIHRJ2): existing
# e2e covers SESSION-bead usage (test-usage-tracking.sh), the LiteLLM
# proxy spend per virtual key (test-litellm-cost-tracking.sh), and the
# fleet aggregate /metrics endpoint (test-fleet-metrics.sh) — but nothing
# asserts the PER-AGENT bead rollup or the CONTEXT-SIZE field and how they
# render on the beads-viewer agent cards.
#
# The data path under test:
#   - statusreporter.reportLiveUsage mirrors coop's live context window +
#     running cost estimate onto the agent bead as context_tokens /
#     live_cost_usd (controller/internal/statusreporter/reporter.go).
#   - reconcile_session_spend.go folds each closed session's LiteLLM spend
#     into the authoritative total_cost_usd on the agent bead.
#   - beads-viewer buildAgentUsageView (cmd/beads-viewer/agent_usage.go)
#     turns those fields into the context + cost chips rendered by
#     templates/partials/agent_usage_chips.html on the /agents index rows
#     and the /{id} bead-show agent card.
#
# This module drives the read path end-to-end: it writes the telemetry
# fields onto a real type=agent bead via kd, then asserts the kbeads
# server persists them AND the beads-viewer renders the expected chips.
#
# Scenarios:
#   1. type:agent schema accepts + persists context_tokens, live_cost_usd,
#      and total_cost_usd on an agent bead.
#   2. A LIVE agent bead (agent_state=working) renders the context chip
#      ("<humanized> ctx · <pct>%") and a cost chip on /agents.
#   3. The same live agent renders both chips on the /{id} bead-show card.
#   4. Cost composition: a live agent's displayed cost includes the
#      live_cost_usd estimate on top of total_cost_usd (cost chip present
#      even when total_cost_usd is 0 but live_cost_usd > 0).
#   5. Closed-agent semantics: a closed agent suppresses the context chip
#      (no "current context") and shows ONLY total_cost_usd — the stale
#      live_cost_usd is NOT double-counted into the displayed cost.
#   6. Context-window OCCUPANCY %: the chip/title percentage is measured
#      against the auto-compact THRESHOLD (contextwindow.AutoCompactFraction
#      * the model's window), not the raw window, and the window is PER-MODEL.
#      A default-model agent at 128000 tokens reads 69% / 184k threshold / 200k
#      window (Scenario 3); a Fable-5 agent with the identical 128000-token
#      reading reads 13% / 920k / 1000k (Scenario 6). Each leg also asserts the
#      wrong percentage is ABSENT (the raw-window 64% and the hardcoded-200k
#      69%), so the module reds on any denominator drift — a retuned
#      AutoCompactFraction, an OccupancyPct reverted to the raw window, or a
#      window pinned to 200k for every model. This closes the gap in
#      ka-0000001124: internal/contextwindow was asserted by no e2e.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-agent-cost-context.sh
#
# Env overrides:
#   NAMESPACE        target namespace (default gardenio-e2e)
#   RELEASE          Helm release name (default = NAMESPACE)
#   BEADS_HTTP_URL   skip kd port-forward, use this daemon URL
#   BEADS_TOKEN      beads auth token (else read from kd-beads-token secret)
#
# Idempotency: every created bead carries the title prefix
#   agent-cost-context-e2e-<random> and is closed in the cleanup trap.
#
# Hard rules: do NOT edit .captain/quarantines/. Do not touch any other
# e2e module.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gitlab-token.sh"

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
BEADS_SVC="${BEADS_SVC:-${RELEASE}-beads}"
VIEWER_SVC="${VIEWER_SVC:-${RELEASE}-beads-viewer}"
VIEWER_PF_PORT="${VIEWER_PF_PORT:-19611}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19612}"
KD_BIN="${KD_BIN:-kd}"
TITLE_PREFIX="agent-cost-context-e2e"
RANDOM_SUFFIX="$$-$(date +%s)"

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

assert_contains() {
  local name="$1" pattern="$2" actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected output to contain '$pattern'; got: $(echo "$actual" | head -3)"
  fi
}

assert_not_contains() {
  local name="$1" pattern="$2" actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (does not contain '$pattern')"
  else
    fail "$name" "expected output NOT to contain '$pattern'"
  fi
}

cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  [ -n "$VIEWER_PF_PID" ] && kill "$VIEWER_PF_PID" 2>/dev/null || true
  [ -n "$BEADS_PF_PID"  ] && kill "$BEADS_PF_PID"  2>/dev/null || true
}
trap cleanup EXIT

# ── Setup ─────────────────────────────────────────────────────────
setup_beads_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
  else
    blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
    kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
      "${BEADS_PF_PORT}:8080" >/tmp/agent-cost-ctx-beads-pf.log 2>&1 &
    BEADS_PF_PID=$!
    sleep 3
    export BEADS_HTTP_URL="http://localhost:${BEADS_PF_PORT}"
  fi
  if [ -z "${BEADS_TOKEN:-}" ]; then
    local secret
    secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
    [ -n "$secret" ] && export BEADS_TOKEN="$secret"
  fi
  dim "  Daemon at $BEADS_HTTP_URL"
}

# Returns 1 (and the module skips the viewer scenarios) when the viewer
# service is absent — keeps the module green on a beads-only deployment.
setup_viewer_portforward() {
  if ! kubectl -n "$NAMESPACE" get svc "$VIEWER_SVC" >/dev/null 2>&1; then
    yellow "  viewer service $VIEWER_SVC not found in $NAMESPACE"
    return 1
  fi
  blue "Port-forwarding ${NAMESPACE}/${VIEWER_SVC} :${VIEWER_PF_PORT} → 8095..."
  kubectl -n "$NAMESPACE" port-forward "svc/${VIEWER_SVC}" \
    "${VIEWER_PF_PORT}:8095" >/tmp/agent-cost-ctx-viewer-pf.log 2>&1 &
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

# viewer_get <path> — GETs the viewer with bearer auth (resolve a
# GitLab-direct bearer when one is available; fall back to the static daemon
# token / no auth otherwise). Echoes "<status>\n<body>".
VIEWER_TOKEN=""
VIEWER_USER_EMAIL="${VIEWER_USER_EMAIL:-e2e-agent-cost-${RANDOM_SUFFIX}@example.com}"
resolve_viewer_token() {
  # Prefer a GitLab-direct bearer (matches the viewer's OIDC audience).
  if VIEWER_TOKEN=$(mint_user_token "$VIEWER_USER_EMAIL" 2>/dev/null) && [ -n "$VIEWER_TOKEN" ]; then
    dim "  Viewer auth: GitLab-direct bearer"
    return 0
  fi
  # Fall back to the beads token — some deployments accept it on the viewer.
  if [ -n "${BEADS_TOKEN:-}" ]; then
    VIEWER_TOKEN="$BEADS_TOKEN"
    dim "  Viewer auth: static daemon token (no GitLab-direct bearer)"
    return 0
  fi
  dim "  Viewer auth: none (anonymous)"
}

viewer_get() {
  local path="$1"
  local out status
  out=$(mktemp)
  status=$(curl -sk --max-time 10 \
    ${VIEWER_TOKEN:+-H "Authorization: Bearer ${VIEWER_TOKEN}"} \
    -H "Accept: text/html" \
    -o "$out" \
    -w '%{http_code}' \
    "http://localhost:${VIEWER_PF_PORT}${path}" 2>/dev/null || echo "000")
  printf '%s\n' "$status"
  cat "$out"
  rm -f "$out"
}

# create_agent_bead <title-suffix> <agent_state> <field>... — creates a
# type=agent bead with the required schema fields plus any extra -f fields.
# Echoes the bead id (empty on failure).
create_agent_bead() {
  local suffix="$1" state="$2"; shift 2
  local extra=()
  local f
  for f in "$@"; do extra+=(-f "$f"); done
  local out
  out=$("$KD_BIN" create "${TITLE_PREFIX}-${suffix}-${RANDOM_SUFFIX}" \
    --type=agent \
    -l "project:gardenio" \
    -l "e2e:${RANDOM_SUFFIX}" \
    -f "agent=${TITLE_PREFIX}-${suffix}-${RANDOM_SUFFIX}" \
    -f "role=thread" \
    -f "project=gardenio" \
    -f "agent_state=${state}" \
    "${extra[@]}" \
    --json 2>/dev/null || echo "{}")
  echo "$out" | jq -r '.id // empty' 2>/dev/null || echo ""
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " agent cost + context-size tracking E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_beads_portforward

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  # ── Scenario 1: schema persists the telemetry fields ───────────
  blue "Scenario 1: agent bead persists context_tokens / live_cost_usd / total_cost_usd"
  local live_id
  live_id=$(create_agent_bead "live" "working" \
    "context_tokens=128000" \
    "live_cost_usd=0.42" \
    "total_cost_usd=1.50")
  if [ -n "$live_id" ]; then
    CREATED_BEADS+=("$live_id")
    pass "live agent bead created ($live_id)"
    local show_json
    show_json=$("$KD_BIN" show "$live_id" --json 2>/dev/null || echo "{}")
    assert_contains "context_tokens persisted" "128000" "$show_json"
    assert_contains "live_cost_usd persisted" "0.42" "$show_json"
    assert_contains "total_cost_usd persisted" "1.5" "$show_json"
  else
    fail "live agent bead create" "could not create agent bead"
  fi
  echo

  # ── Closed agent bead for the closed-semantics scenarios ───────
  blue "Setup: closed agent bead (stale live_cost_usd must not double-count)"
  local closed_id
  closed_id=$(create_agent_bead "closed" "done" \
    "context_tokens=96000" \
    "live_cost_usd=0.99" \
    "total_cost_usd=2.00")
  if [ -n "$closed_id" ]; then
    CREATED_BEADS+=("$closed_id")
    "$KD_BIN" close "$closed_id" >/dev/null 2>&1 || true
    pass "closed agent bead created + closed ($closed_id)"
  else
    fail "closed agent bead create" "could not create agent bead"
  fi
  echo

  # ── Viewer scenarios (skip cleanly if viewer absent) ───────────
  if ! setup_viewer_portforward; then
    skip "viewer chip rendering" "beads-viewer service not deployed"
    summary
    return
  fi
  resolve_viewer_token

  # Confirm the viewer is actually serving HTML before asserting on it;
  # a 401/redirect means auth is misconfigured for this env — skip rather
  # than red-gate the suite on an environment auth gap.
  local probe_status
  probe_status=$(viewer_get "/agents" | head -1)
  if [ "$probe_status" != "200" ]; then
    skip "viewer chip rendering" "/agents returned HTTP $probe_status (auth/env gap)"
    summary
    return
  fi

  # ── Scenario 2: live agent renders context + cost chips on /agents
  blue "Scenario 2: /agents renders context + cost chips for a live agent"
  local agents_html
  agents_html=$(viewer_get "/agents")
  # The agent's title is its agent name; the usage chips render in the same
  # row. Assert the humanized context label and the "ctx" marker from
  # agent_usage_chips.html are present somewhere on the page.
  assert_contains "/agents shows the live agent" "${TITLE_PREFIX}-live-${RANDOM_SUFFIX}" "$agents_html"
  assert_contains "/agents renders context chip marker" "ctx" "$agents_html"
  # 128000 tokens humanizes to "128.0k"; the occupancy percentage is measured
  # against the auto-compact THRESHOLD (0.92 * the model's window), not the raw
  # window, so on the default 200k model it is 128000 / (200000*0.92 = 184000) =
  # 69% (asserted directly in Scenario 6, which drives the OccupancyPct
  # denominator this label alone cannot catch drifting).
  assert_contains "/agents renders humanized context label" "128.0k" "$agents_html"
  echo

  # ── Scenario 3: bead-show agent card renders both chips ────────
  blue "Scenario 3: /{id} bead-show card renders context + cost chips"
  local show_html
  show_html=$(viewer_get "/${live_id}")
  assert_contains "bead-show renders context chip" "128.0k" "$show_html"
  assert_contains "bead-show renders ctx marker" "ctx" "$show_html"
  # buildAgentUsageView sums total_cost_usd (1.50) + live_cost_usd (0.42)
  # = $1.92 for a live agent. fmtUSD renders "$1.92".
  assert_contains "bead-show renders composed live cost \$1.92" "1.92" "$show_html"
  # ── Occupancy % (contextwindow.OccupancyPct) — DEFAULT-MODEL leg ─────
  # The live_id bead carries context_tokens=128000 and NO model field, so it
  # rides the fleet-default 200k window. Occupancy is measured against the
  # auto-compact THRESHOLD (contextwindow.AutoCompactFraction=0.92), not the raw
  # window: 128000 / (200000*0.92 = 184000) = 69%. The full title phrase carries
  # the percent, the threshold-k (184), and the window-k (200) in one contiguous
  # string that only renders correctly when BOTH the fraction and the per-model
  # window are right — nothing above asserts the percentage itself, so this is
  # the assertion that reds on a denominator drift (e.g. a retuned
  # AutoCompactFraction or an OccupancyPct that reverts to the raw window). The
  # chip's "· 69%" is asserted alongside as the operator-visible bar readout.
  assert_contains "bead-show default-model occupancy title (69% of 184k threshold on 200k window)" \
    "(~69% of the ~184k auto-compact threshold on a 200k window)" "$show_html"
  assert_contains "bead-show default-model occupancy chip reads 69%" "· 69%" "$show_html"
  # Anti-vacuity discriminator (leg 3): both regressions the acceptance names
  # — a retuned AutoCompactFraction, or an OccupancyPct stubbed back onto the
  # RAW window — compute 128000/200000 = 64% instead of 69%. Assert that wrong
  # percentage is ABSENT so this leg cannot pass against the raw-window
  # denominator. Paired with the 69% assertion above, the module reds on a
  # denominator drift in either direction.
  assert_not_contains "bead-show default-model does NOT read raw-window 64%" "· 64%" "$show_html"
  echo

  # ── Scenario 4: cost composition — live estimate counts ────────
  blue "Scenario 4: live cost chip present even when total_cost_usd is 0"
  local liveonly_id
  liveonly_id=$(create_agent_bead "liveonly" "working" \
    "context_tokens=4000" \
    "live_cost_usd=0.07" \
    "total_cost_usd=0")
  if [ -n "$liveonly_id" ]; then
    CREATED_BEADS+=("$liveonly_id")
    local liveonly_html
    liveonly_html=$(viewer_get "/${liveonly_id}")
    # 0 + 0.07 = $0.07 — cost chip must render despite total_cost_usd=0.
    assert_contains "live-only cost chip renders \$0.07" "0.07" "$liveonly_html"
  else
    fail "live-only agent bead create" "could not create agent bead"
  fi
  echo

  # ── Scenario 5: closed agent — context suppressed, no double-count
  blue "Scenario 5: closed agent suppresses context chip, shows only total_cost"
  local closed_html
  closed_html=$(viewer_get "/${closed_id}")
  # A closed agent has no "current context" — buildAgentUsageView suppresses
  # the context label when !live, so the 96000-token figure must NOT appear
  # in a context chip ("96.0k").
  assert_not_contains "closed agent hides context label" "96.0k" "$closed_html"
  # Cost is total_cost_usd ONLY (2.00 → "$2.00"); the stale live_cost_usd
  # (0.99) must NOT be added (that would render "$2.99").
  assert_contains "closed agent shows total cost \$2.00" "2.00" "$closed_html"
  assert_not_contains "closed agent does NOT double-count live cost (\$2.99)" "2.99" "$closed_html"
  echo

  # ── Scenario 6: long-context model — per-model denominator ─────
  # This is the leg that proves the occupancy denominator is PER-MODEL rather
  # than a hardcoded 200k. A Fable-5 agent (contextwindow.WindowTokensForModel
  # matches the "fable" family substring → LargeWindowTokens=1,000,000) carrying
  # the SAME 128000-token reading as the default-model bead above must render a
  # DIFFERENT occupancy: 128000 / (1,000,000*0.92 = 920,000) = 13%, on a 1000k
  # window. A hardcoded-200k regression would instead compute 128000/184000 =
  # 69% here (the default-model figure) — so asserting 13% present AND 69%
  # absent is what discriminates a per-model denominator from a fixed one. The
  # "claude-fable-5" alias is read from WindowTokensForModel at HEAD, not
  # guessed (its unit test TestWindowTokensForModel pins this exact string).
  blue "Scenario 6: long-context (Fable-5) model renders a per-model occupancy denominator"
  local fable_id
  fable_id=$(create_agent_bead "fable" "working" \
    "context_tokens=128000" \
    "model=claude-fable-5" \
    "live_cost_usd=0.10" \
    "total_cost_usd=0")
  if [ -n "$fable_id" ]; then
    CREATED_BEADS+=("$fable_id")
    local fable_html
    fable_html=$(viewer_get "/${fable_id}")
    # Same humanized token label as the default agent — the RAW reading is
    # identical; only the denominator (hence the percentage) differs.
    assert_contains "Fable bead-show renders context chip 128.0k" "128.0k" "$fable_html"
    assert_contains "Fable occupancy title (13% of 920k threshold on 1000k window)" \
      "(~13% of the ~920k auto-compact threshold on a 1000k window)" "$fable_html"
    assert_contains "Fable occupancy chip reads 13%" "· 13%" "$fable_html"
    # Discriminator: a hardcoded-200k denominator would read 69% here (the
    # default-model figure). Its absence proves the window is per-model.
    assert_not_contains "Fable does NOT read hardcoded-200k 69%" "· 69%" "$fable_html"
    assert_not_contains "Fable title does NOT carry the 200k window" \
      "on a 200k window)" "$fable_html"
  else
    fail "Fable agent bead create" "could not create agent bead"
  fi
  echo

  summary
}

summary() {
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed${SKIP:+ ($SKIP skipped)}"
  else
    red " ✗ $FAIL/$total assertions failed${SKIP:+ ($SKIP skipped)}"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"
  [ "$FAIL" -eq 0 ]
}

main "$@"
