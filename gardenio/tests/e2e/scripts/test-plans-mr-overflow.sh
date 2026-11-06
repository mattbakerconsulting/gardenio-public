#!/usr/bin/env bash
# test-plans-mr-overflow.sh — /plans MR overflow pill FailedHiddenCount E2E
# (ka-0000000730, related gd-VHwFGOoKDV / ka-0000000387, epic ka-0000000958)
#
# THE GAP. The /plans index card surfaces the open MRs a plan's steps produced
# as click-through pills (plans_mrs.go planOpenMRsBulk → plan_card.html). Past a
# render cap (planMRMaxRender=3) the extra MRs collapse behind a "+N MR" overflow
# pill. iteration-20 (ka-0000000387) added the load-bearing attention signal this
# module guards: when a HIDDEN (elided) MR has a FAILED head pipeline, the
# overflow pill tints loud-red and folds a "· ✕ K" facet onto it — because
# without it a plan whose hidden MRs are all failing "reads identically to one
# whose hidden MRs are all green" (the code comment), a silently-lost attention
# signal. iteration-21 (gd-VHwFGOoKDV) then PROMOTES failed MRs ahead of green
# into the rendered (clickable) slots, so FailedHiddenCount counts only failures
# that STILL overflow after promotion — never a failure the operator can click.
#
# plans_mrs_test.go unit-covers FailedHiddenCount()/HasFailedHidden() and the
# promotion ordering against a fake daemon; the render-level regressions
# (TestHandlePlans_IndexOverflowPill*) run the http.Handler against a seeded
# in-memory fake. This module is the missing REAL e2e: it seeds actual
# step-->produces-->merge-request bead graphs in a LIVE kbeads daemon and renders
# them through the REAL beads-viewer binary end to end, asserting the exact
# operator-visible pixels of the "+N MR · ✕ K" overflow — the thing an operator
# sees fail when a plan's hidden red MRs stop screaming for attention.
#
# WHY the real viewer binary + a live daemon (not a stub, not the deployed
# viewer):
#   - The overflow signal is DERIVED, not stored: planOpenMRsBulk walks a
#     batched forward `produces`-edge fan-out over every plan's rendered step
#     ids, hydrates the produced MR beads, dedups by bead-id then by mr_url,
#     sorts newest-first, and finally PARTITIONS failed-CI ahead of the rest —
#     then Rendered()/elidedMRs()/FailedHiddenCount() project off that. Faking
#     that multi-hop graph faithfully in a stub is far more brittle than seeding
#     the real beads it reads. So we seed a live daemon and let the real enrich
#     pass do the derivation.
#   - The DEPLOYED staging viewer sits behind Keycloak OIDC, and a token can
#     only be minted where the realm admin / a direct-grant test client is
#     reachable (the sibling viewer modules SKIP without one). So instead of
#     depending on that, we boot our OWN throwaway beads-viewer with the
#     documented local-dev OIDC bypass (BEADS_VIEWER_DEV_AUTH_BYPASS, exactly the
#     grafana-directive module's technique) pointed at the SAME daemon we seed.
#     Identical render code, real data, no token dependency.
#
# Scenarios (each seeds ONE isolated plan under its own project label, so a
# scoped `/plans?project=<uniqueproj>` GET returns exactly that one card and the
# assertions never race other plans on the board):
#   1. UNDER the cap — a plan with planMRMaxRender or fewer open MRs renders its
#      inline pills and NO overflow pill at all.
#   2. OVER the cap, all non-failed — the "+N MR" overflow appears with the
#      correct MoreCount, and stays CALM (no failed tint / no ✕ facet).
#   3. THE POINT — a FAILED MR remains hidden after promotion (seed MORE failing
#      MRs than inline slots): the overflow pill gains the loud-red failed tint
#      and a "· ✕ K" facet carrying the exact FailedHiddenCount.
#   4. NEGATIVE — an over-cap overflow whose hidden MRs are all green/running
#      renders the count only, never the ✕ facet (adds no false attention ink).
#   5. INLINE-failed does NOT inflate — a single failed MR is PROMOTED into a
#      clickable inline slot (iter-21), so it renders as a real href AND the
#      overflow (its green tail) stays calm: a failure the operator can already
#      click never counts as hidden.
#
# Assertion hygiene (the plans_mrs_test.go footgun, kept here): the CSS class
# names .plan-card-mr-more-failed / .plan-card-mr-more-ci ALSO live in the
# always-served page <style> block, so keying on a class name false-matches on
# EVERY render. We key strictly on RENDERED-ONLY strings that appear only on an
# actually-emitted overflow pill: the "+N MR" text, the "N more open merge
# request(s)" title, the "N with FAILED CI" tooltip, and the
# `aria-label="K hidden MR(s) with failed CI"` facet label.
#
# Self-SKIPs (exit 0) when it cannot reach a beads daemon or lacks a go
# toolchain / prebuilt viewer to boot — inert where it cannot run, never a false
# red (mirrors test-grafana-directive.sh + the sibling viewer modules).
#
# Idempotency: every seeded bead carries a per-run fixture label
#   fixture:mr-overflow-e2e-<run>
# and a per-scenario project label; the cleanup trap closes them all with
# --allow-open-mr (an unmerged MR bead refuses a plain close). Reruns never
# collide.
#
# Quick run (against the live singleton daemon via port-forward):
#   NAMESPACE=gardenio-e2e ./gardenio/tests/e2e/scripts/test-plans-mr-overflow.sh
# or point straight at a daemon:
#   BEADS_HTTP_URL=http://localhost:8080 BEADS_TOKEN=... \
#     ./gardenio/tests/e2e/scripts/test-plans-mr-overflow.sh
#
# Hard rules: do NOT edit .captain/quarantines/. Do not touch any other e2e
# module.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19913}"
KD_BIN="${KD_BIN:-kd}"
CONTROLLER_DIR="${CONTROLLER_DIR:-$REPO_ROOT/gardenio/controller}"
RUN_ID="mr-overflow-$$-$(date +%s 2>/dev/null || echo t)"
FIXTURE_LABEL="fixture:mr-overflow-e2e-$$"
# planMRMaxRender in plans_mrs.go — the inline render cap. Kept as a named knob
# so a future cap change is a one-line edit here (the scenarios derive their
# seed counts from it).
PLAN_MR_MAX_RENDER="${PLAN_MR_MAX_RENDER:-3}"

export KD_ACTOR="${KD_ACTOR:-e2e-test}-mroverflow-$$"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
BEADS_PF_PID=""
VIEWER_PID=""
VIEWER_TEMP=""
# Fixture ids go to a FILE, not an array: every seed runs in a `$(...)` subshell
# (command substitution), so an array append inside a helper dies with the
# subshell and the trap deletes nothing (the exact bug test-auto-close-cascade.sh
# documents). A file crosses the subshell boundary.
BEAD_REGISTRY="$(mktemp -t mr-overflow-beads.XXXXXX)"

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
  [ -n "$VIEWER_PID" ] && kill "$VIEWER_PID" 2>/dev/null || true
  # Close every seeded fixture. --allow-open-mr because an unmerged
  # type=merge-request bead refuses a plain close (the daemon guard: "a closed
  # bead would claim a fix shipped while its code is not on main"). It is a
  # harmless no-op on the non-MR fixtures (epics/steps), so one flag closes all.
  if [ -f "$BEAD_REGISTRY" ]; then
    local id
    while IFS= read -r id; do
      [ -n "$id" ] || continue
      "$KD_BIN" close "$id" --allow-open-mr >/dev/null 2>&1 || true
    done < "$BEAD_REGISTRY"
    rm -f "$BEAD_REGISTRY"
  fi
  [ -n "$BEADS_PF_PID" ] && kill "$BEADS_PF_PID" 2>/dev/null || true
  [ -n "$VIEWER_TEMP" ] && rm -f "$VIEWER_TEMP" 2>/dev/null || true
}
trap cleanup EXIT

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

wait_http() {
  local url="$1" i
  for i in $(seq 1 40); do
    if curl -sf --max-time 1 -o /dev/null "$url" 2>/dev/null; then
      return 0
    fi
    sleep 0.25
  done
  return 1
}

bead_id_from_json() {
  printf '%s' "$1" | python3 -c 'import sys,json
try: print(json.load(sys.stdin).get("id",""))
except Exception: print("")' 2>/dev/null || printf ''
}

register_bead() {
  [ -n "$1" ] && printf '%s\n' "$1" >> "$BEAD_REGISTRY"
}

# ── beads daemon connectivity ─────────────────────────────────────
# Prefer a caller-provided BEADS_HTTP_URL; else port-forward the daemon Service.
setup_beads() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  using BEADS_HTTP_URL=$BEADS_HTTP_URL"
  else
    command -v kubectl >/dev/null 2>&1 || return 1
    kubectl -n "$NAMESPACE" get svc "$BEADS_SVC" >/dev/null 2>&1 || return 1
    blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
    kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
      "${BEADS_PF_PORT}:8080" >/tmp/mr-overflow-beads-pf.log 2>&1 &
    BEADS_PF_PID=$!
    export BEADS_HTTP_URL="http://localhost:${BEADS_PF_PORT}"
    if [ -z "${BEADS_TOKEN:-}" ]; then
      local secret
      secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
        -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
      [ -n "$secret" ] && export BEADS_TOKEN="$secret"
    fi
    wait_http "${BEADS_HTTP_URL}/v1/health" || true
  fi
  # Confirm kd can actually reach the daemon before we commit to seeding.
  "$KD_BIN" list --status=open --limit 1 --json >/dev/null 2>&1
}

# ── viewer binary ─────────────────────────────────────────────────
# Resolve a beads-viewer binary: prefer a prebuilt one (CI installs
# /usr/local/bin/beads-viewer via the build-beads-viewer task), else build from
# source in workspace mode. Sets VIEWER_BIN. Returns non-zero (caller SKIPs)
# when neither is possible. Mirrors test-grafana-directive.sh §1.
resolve_viewer_bin() {
  if [ -n "${BEADS_VIEWER_BIN:-}" ] && [ -x "${BEADS_VIEWER_BIN:-}" ]; then
    VIEWER_BIN="$BEADS_VIEWER_BIN"
    dim "  using prebuilt beads-viewer (\$BEADS_VIEWER_BIN=$VIEWER_BIN)"
    return 0
  fi
  if command -v beads-viewer >/dev/null 2>&1; then
    VIEWER_BIN="$(command -v beads-viewer)"
    dim "  using prebuilt beads-viewer ($VIEWER_BIN)"
    return 0
  fi
  command -v go >/dev/null 2>&1 || return 1
  [ -d "$CONTROLLER_DIR/cmd/beads-viewer" ] || return 1
  [ -f "$REPO_ROOT/go.work" ] || return 1
  blue "[build] cmd/beads-viewer (from source, workspace mode)"
  VIEWER_TEMP="$(mktemp "/tmp/beads-viewer.${RUN_ID}.XXXXXX")"
  VIEWER_BIN="$VIEWER_TEMP"
  local build_log="/tmp/beads-viewer-build-${RUN_ID}.log"
  if ( cd "$REPO_ROOT" && GOWORK="$REPO_ROOT/go.work" CGO_ENABLED=0 \
        go build -o "$VIEWER_BIN" ./gardenio/controller/cmd/beads-viewer/ ) \
        >"$build_log" 2>&1; then
    dim "  built beads-viewer from source"
    return 0
  fi
  red "  build failed: $(tail -6 "$build_log" 2>/dev/null)"
  return 2
}

# start_viewer — boot the throwaway beads-viewer against the LIVE daemon with the
# OIDC bypass. Sets VIEWER_PORT + VIEWER_PID. Returns non-zero if it never became
# healthy.
start_viewer() {
  VIEWER_PORT="$(free_port)"
  local log="/tmp/mr-overflow-viewer-${VIEWER_PORT}.log"
  # BEADS_VIEWER_DEV_AUTH_BYPASS + the agent-pod ACK: the shard runs inside a pod
  # with a mounted SA token, which the plain bypass refuses (prod-like-env
  # guard). The ACK opt-in allows it and forces LISTEN_ADDR to loopback — exactly
  # right for a throwaway local viewer. Identity comes from the edge-stamped
  # X-Auth-Request-Email header on each GET. Same technique as
  # test-grafana-directive.sh's render_bead_show.
  BEADS_VIEWER_DEV_AUTH_BYPASS=1 \
  BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD=1 \
  BEADS_HTTP_ADDR="$BEADS_HTTP_URL" \
  BEADS_DAEMON_TOKEN="${BEADS_TOKEN:-}" \
  LISTEN_ADDR="127.0.0.1:${VIEWER_PORT}" \
  LOG_LEVEL=error \
  "$VIEWER_BIN" >"$log" 2>&1 &
  VIEWER_PID=$!
  if ! wait_http "http://127.0.0.1:${VIEWER_PORT}/healthz"; then
    red "  viewer never became healthy — log tail:"
    tail -6 "$log" 2>/dev/null | sed 's/^/    /'
    return 1
  fi
  return 0
}

# fetch_plan_card <project> <out-html> — GET the /plans index scoped to one
# project (so the response holds exactly the single seeded card) with the OIDC
# bypass identity header. Echoes the HTTP status; body lands in <out-html>.
fetch_plan_card() {
  local project="$1" out="$2"
  curl -s --max-time 15 -o "$out" -w '%{http_code}' \
    -H 'X-Auth-Request-Email: mr-overflow-e2e@acme.example' \
    "http://127.0.0.1:${VIEWER_PORT}/plans?operated_by=&status=all&project=${project}" \
    2>/dev/null || echo "000"
}

# ── seed helpers ──────────────────────────────────────────────────
# seed_plan_project — mint a plan epic + one step under a UNIQUE project label,
# so a scoped /plans?project= GET returns exactly this card. Echoes
# "<project> <epic-id> <step-id>". Empty epic/step on failure.
seed_plan_project() {
  local tag="$1"
  local project="mr-overflow-${tag}-$$"
  local epic step
  epic=$(bead_id_from_json "$("$KD_BIN" create "mr-overflow-${tag} ${RUN_ID}" \
    --type=epic --no-parent -l "plan" -l "project:${project}" -l "$FIXTURE_LABEL" \
    -d "mr-overflow e2e plan (${tag})" --json 2>/dev/null || printf '{}')")
  register_bead "$epic"
  step=$(bead_id_from_json "$("$KD_BIN" create "mr-overflow-${tag}-step ${RUN_ID}" \
    --type=task --no-parent -l "project:${project}" -l "$FIXTURE_LABEL" \
    -d "mr-overflow e2e step (${tag})" --json 2>/dev/null || printf '{}')")
  register_bead "$step"
  if [ -n "$epic" ] && [ -n "$step" ]; then
    "$KD_BIN" dep add "$step" "$epic" --type parent-child >/dev/null 2>&1 || true
  fi
  # Trailing newline is load-bearing: the caller does `read -r project epic step
  # < <(seed_plan_project ...)`, and `read` returns exit 1 on input with no final
  # newline — which under `set -e` would abort the whole scenario.
  printf '%s %s %s\n' "$project" "$epic" "$step"
}

# seed_mr <project> <step-id> <iid> <pipeline-status>
# Create a non-closed type=merge-request bead carrying the fields the renderer
# reads (mr_url, mr_iid, mr_state, mr_pipeline_status) and link it as
# step --produces--> mr (the FORWARD edge planOpenMRsBulk walks). An empty
# pipeline-status seeds an MR with no CI (no badge). Echoes the MR id.
seed_mr() {
  local project="$1" step="$2" iid="$3" status="$4"
  # A no-CI MR gets NO mr_pipeline_status field at all (empty $extra); an intended
  # word-split passes "-f mr_pipeline_status=<status>" as two argv when set, or
  # nothing when unset — so the split is deliberate, not an accident.
  local extra=""
  [ -n "$status" ] && extra="-f mr_pipeline_status=${status}"
  local mr
  # shellcheck disable=SC2086 # $extra is an intentional two-token flag or empty
  mr=$(bead_id_from_json "$("$KD_BIN" create "mr-overflow-mr-${iid} ${RUN_ID}" \
    --type=merge-request --no-parent -l "project:${project}" -l "$FIXTURE_LABEL" \
    -f "mr_url=https://gitlab.example/mr-overflow/${RUN_ID}/${iid}" \
    -f "mr_iid=\"${iid}\"" \
    -f "mr_state=opened" \
    $extra \
    -d "mr-overflow e2e MR ${iid} (${status:-no-ci})" --json 2>/dev/null || printf '{}')")
  register_bead "$mr"
  if [ -n "$mr" ]; then
    "$KD_BIN" dep add "$step" "$mr" --type produces >/dev/null 2>&1 || true
  fi
  printf '%s' "$mr"
}

# ── rendered-only marker helpers (never key on a class name) ───────
# has_overflow <html> — the overflow pill is rendered ⇔ the "N more open merge
# request(s)" title text is present (that title lives ONLY on an emitted pill).
has_overflow() { grep -qF 'more open merge request' "$1"; }
# more_count <html> — the visible "+N MR" count off the emitted pill (empty when
# no overflow). The leading '>' is the close of the <a ...> tag, so it can only
# match rendered pill text, never a CSS rule.
more_count() { grep -oE '>\+[0-9]+ MR' "$1" | head -1 | grep -oE '[0-9]+' || true; }
# has_failed_tooltip <html> — the "N with FAILED CI" tooltip fragment, rendered
# ONLY when HasFailedHidden (uppercase; distinct from any CSS/comment text).
has_failed_tooltip() { grep -qF 'with FAILED CI' "$1"; }
# hidden_failed_count <html> — the exact K from the facet's a11y label
# `aria-label="K hidden MR(s) with failed CI"` (rendered only on the ✕ facet).
hidden_failed_count() {
  grep -oE 'aria-label="[0-9]+ hidden MR[^"]*with failed CI"' "$1" \
    | head -1 | grep -oE '[0-9]+' | head -1 || true
}
# mr_href_present <html> <iid> — is the given MR rendered as a clickable inline
# pill href (proves it landed in a rendered, not elided, slot)?
mr_href_present() {
  grep -qF "href=\"https://gitlab.example/mr-overflow/${RUN_ID}/${2}\"" "$1"
}

# ══════════════════════════════════════════════════════════════════
# Scenarios
# ══════════════════════════════════════════════════════════════════

# scenario_under_cap — planMRMaxRender or fewer MRs → inline pills, NO overflow.
scenario_under_cap() {
  blue "Scenario 1: under the render cap → inline pills, no overflow pill"
  read -r project epic step < <(seed_plan_project "undercap")
  if [ -z "$epic" ] || [ -z "$step" ]; then
    fail "scenario 1: seed plan" "no epic/step id from kd create"
    return 0
  fi
  # Two MRs (< cap of 3): one green, one with no CI.
  local m1 m2
  m1=$(seed_mr "$project" "$step" 1001 success)
  m2=$(seed_mr "$project" "$step" 1002 "")
  if [ -z "$m1" ] || [ -z "$m2" ]; then
    fail "scenario 1: seed MRs" "kd create merge-request failed"
    return 0
  fi
  sleep 1
  local out; out="/tmp/mr-overflow-s1-${VIEWER_PORT}.html"
  local code; code=$(fetch_plan_card "$project" "$out")
  if [ "$code" != "200" ]; then
    fail "scenario 1: GET /plans" "status $code (want 200)"
    return 0
  fi
  if ! grep -qF "plan-card-${epic}" "$out"; then
    fail "scenario 1: seeded card present" "no plan-card-${epic} in the scoped index"
    return 0
  fi
  pass "scenario 1: scoped index renders exactly the seeded card ($epic)"
  # Both inline MR pills render as click-through hrefs.
  if mr_href_present "$out" 1001 && mr_href_present "$out" 1002; then
    pass "scenario 1: both under-cap MRs render as inline click-through pills"
  else
    fail "scenario 1: inline MR pills render" "one of the 2 MR hrefs is missing"
  fi
  # And there is NO overflow pill at all.
  if has_overflow "$out"; then
    fail "scenario 1: no overflow pill under the cap" \
      "overflow pill rendered for a $((PLAN_MR_MAX_RENDER))-cap plan with 2 MRs (+$(more_count "$out") MR)"
  else
    pass "scenario 1: no overflow pill for a plan under the render cap"
  fi
  echo
}

# scenario_over_cap_calm — over cap, all green/running → overflow with the right
# MoreCount, and CALM (no failed tint, no ✕ facet).
scenario_over_cap_calm() {
  blue "Scenario 2/4: over the cap, all non-failed → overflow count only (calm)"
  read -r project epic step < <(seed_plan_project "calm")
  if [ -z "$epic" ] || [ -z "$step" ]; then
    fail "scenario 2: seed plan" "no epic/step id"
    return 0
  fi
  # cap + 3 MRs, all green except one running — none failed. MoreCount = 3.
  local ok=1 i
  for i in 2001 2002 2003 2004 2005; do
    [ -n "$(seed_mr "$project" "$step" "$i" success)" ] || ok=0
  done
  [ -n "$(seed_mr "$project" "$step" 2006 running)" ] || ok=0
  if [ "$ok" != 1 ]; then
    fail "scenario 2: seed MRs" "a kd create merge-request failed"
    return 0
  fi
  sleep 1
  local out; out="/tmp/mr-overflow-s2-${VIEWER_PORT}.html"
  local code; code=$(fetch_plan_card "$project" "$out")
  if [ "$code" != "200" ]; then
    fail "scenario 2: GET /plans" "status $code (want 200)"
    return 0
  fi
  if has_overflow "$out"; then
    pass "scenario 2: overflow pill rendered for an over-cap plan"
  else
    fail "scenario 2: overflow pill present" "no overflow pill for 6 MRs over a cap of $PLAN_MR_MAX_RENDER"
    return 0
  fi
  local mc; mc=$(more_count "$out")
  local want_more=$((6 - PLAN_MR_MAX_RENDER))
  if [ "$mc" = "$want_more" ]; then
    pass "scenario 2: overflow MoreCount = $want_more (6 MRs − cap $PLAN_MR_MAX_RENDER)"
  else
    fail "scenario 2: overflow MoreCount" "rendered +${mc:-<none>} MR, want +$want_more"
  fi
  # NEGATIVE (scenario 4): all-green overflow must stay calm.
  if has_failed_tooltip "$out"; then
    fail "scenario 4: all-green overflow stays calm" \
      "a non-failing overflow rendered the 'with FAILED CI' tooltip"
  else
    pass "scenario 4: all-green overflow renders no FAILED-CI tooltip (calm)"
  fi
  if [ -n "$(hidden_failed_count "$out")" ]; then
    fail "scenario 4: no ✕ facet on a calm overflow" \
      "a non-failing overflow rendered a hidden-failed aria facet (count=$(hidden_failed_count "$out"))"
  else
    pass "scenario 4: all-green overflow renders no ✕ hidden-failed facet"
  fi
  echo
}

# scenario_hidden_failed — THE POINT. Seed MORE failing MRs than inline slots so
# a failure STILL overflows after iter-21 promotion: the overflow pill tints
# failed + carries a "· ✕ K" facet with the exact FailedHiddenCount.
scenario_hidden_failed() {
  blue "Scenario 3: a FAILED hidden MR tints the overflow + folds a ✕ K facet (THE POINT)"
  read -r project epic step < <(seed_plan_project "hiddenfail")
  if [ -z "$epic" ] || [ -z "$step" ]; then
    fail "scenario 3: seed plan" "no epic/step id"
    return 0
  fi
  # cap+1 FAILED MRs + 2 green. After attention-first promotion the failures
  # fill all $PLAN_MR_MAX_RENDER rendered slots and exactly ONE failure still
  # overflows → FailedHiddenCount = 1. Total = cap+3 MRs → MoreCount = cap.
  # (MoreCount = $PLAN_MR_MAX_RENDER ≠ FailedHiddenCount = 1, so the two are
  # asserted as distinct numbers — a bug conflating them cannot hide.)
  local ok=1 i
  for i in 3001 3002 3003 3004; do
    [ -n "$(seed_mr "$project" "$step" "$i" failed)" ] || ok=0
  done
  for i in 3005 3006; do
    [ -n "$(seed_mr "$project" "$step" "$i" success)" ] || ok=0
  done
  if [ "$ok" != 1 ]; then
    fail "scenario 3: seed MRs" "a kd create merge-request failed"
    return 0
  fi
  sleep 1
  local out; out="/tmp/mr-overflow-s3-${VIEWER_PORT}.html"
  local code; code=$(fetch_plan_card "$project" "$out")
  if [ "$code" != "200" ]; then
    fail "scenario 3: GET /plans" "status $code (want 200)"
    return 0
  fi
  if ! has_overflow "$out"; then
    fail "scenario 3: overflow pill present" "no overflow pill for 6 MRs over a cap of $PLAN_MR_MAX_RENDER"
    return 0
  fi
  local mc; mc=$(more_count "$out")
  if [ "$mc" = "$PLAN_MR_MAX_RENDER" ]; then
    pass "scenario 3: overflow MoreCount = $PLAN_MR_MAX_RENDER (6 MRs − cap)"
  else
    fail "scenario 3: overflow MoreCount" "rendered +${mc:-<none>} MR, want +$PLAN_MR_MAX_RENDER"
  fi
  # THE attention signal: the loud-red FAILED-CI tooltip on the overflow pill.
  if has_failed_tooltip "$out"; then
    pass "scenario 3: overflow pill carries the 'FAILED CI' attention tooltip"
  else
    fail "scenario 3: overflow tints failed" \
      "a hidden-failing overflow did NOT render the 'with FAILED CI' tooltip — the lost-signal regression"
  fi
  # The ✕ facet's exact count. cap+1 failed, cap rendered → exactly 1 still hidden.
  local hfc; hfc=$(hidden_failed_count "$out")
  if [ "$hfc" = "1" ]; then
    pass "scenario 3: ✕ facet reports FailedHiddenCount = 1 (the residual failure past promotion)"
  else
    fail "scenario 3: ✕ facet count" \
      "aria hidden-failed count = ${hfc:-<none>}, want 1"
  fi
  # And the ✕ facet count is DISTINCT from MoreCount, so a bug collapsing the two
  # (rendering MoreCount where FailedHiddenCount belongs) is caught.
  if [ -n "$hfc" ] && [ -n "$mc" ] && [ "$hfc" != "$mc" ]; then
    pass "scenario 3: FailedHiddenCount ($hfc) ≠ MoreCount ($mc) — the two facets are independent"
  else
    fail "scenario 3: facet counts independent" \
      "FailedHiddenCount=${hfc:-<none>} vs MoreCount=${mc:-<none>} — cannot distinguish a conflation bug"
  fi
  echo
}

# scenario_inline_failed_no_inflate — a single failed MR is PROMOTED to a
# clickable inline pill (iter-21), so it renders as an href AND does NOT inflate
# the hidden-failed count (its green tail overflows calm).
scenario_inline_failed_no_inflate() {
  blue "Scenario 5: a single failed MR promotes to a clickable inline pill and does NOT inflate FailedHiddenCount"
  read -r project epic step < <(seed_plan_project "inlinefail")
  if [ -z "$epic" ] || [ -z "$step" ]; then
    fail "scenario 5: seed plan" "no epic/step id"
    return 0
  fi
  # ONE failed MR + cap+2 green. Promotion pulls the lone failure into a rendered
  # slot (clickable), so the elided tail is all-green → FailedHiddenCount = 0.
  local failed_mr; failed_mr=$(seed_mr "$project" "$step" 5501 failed)
  local ok=1 i
  for i in 5502 5503 5504 5505 5506; do
    [ -n "$(seed_mr "$project" "$step" "$i" success)" ] || ok=0
  done
  if [ -z "$failed_mr" ] || [ "$ok" != 1 ]; then
    fail "scenario 5: seed MRs" "a kd create merge-request failed"
    return 0
  fi
  sleep 1
  local out; out="/tmp/mr-overflow-s5-${VIEWER_PORT}.html"
  local code; code=$(fetch_plan_card "$project" "$out")
  if [ "$code" != "200" ]; then
    fail "scenario 5: GET /plans" "status $code (want 200)"
    return 0
  fi
  if ! has_overflow "$out"; then
    fail "scenario 5: overflow pill present" "no overflow for 6 MRs over a cap of $PLAN_MR_MAX_RENDER"
    return 0
  fi
  # The failed MR is a clickable inline pill (promoted out of the overflow).
  if mr_href_present "$out" 5501; then
    pass "scenario 5: the failed MR renders as a clickable inline pill (promoted, iter-21)"
  else
    fail "scenario 5: failed MR promoted to inline" \
      "the failed MR's href is absent — it should be a rendered click-through pill, not buried in +N"
  fi
  # Its promotion means nothing failing overflows → NO failed tooltip, NO ✕ facet.
  if has_failed_tooltip "$out"; then
    fail "scenario 5: inline failure does not inflate hidden count" \
      "a promoted (inline) failure rendered a 'FAILED CI' overflow tooltip — it must not count as hidden"
  else
    pass "scenario 5: promoted inline failure adds no 'FAILED CI' overflow tooltip"
  fi
  if [ -n "$(hidden_failed_count "$out")" ]; then
    fail "scenario 5: no ✕ facet when the only failure is inline" \
      "rendered a hidden-failed facet (count=$(hidden_failed_count "$out")) though the failure is clickable inline"
  else
    pass "scenario 5: no ✕ hidden-failed facet — an already-clickable failure is never counted as hidden"
  fi
  echo
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " /plans MR overflow pill — FailedHiddenCount E2E (ka-0000000730)"
  blue "═══════════════════════════════════════════════════"

  if ! command -v python3 >/dev/null 2>&1; then
    skip "plans-mr-overflow module" "python3 absent (needed for JSON extraction + free_port)"
    summary
    return 0
  fi

  if ! setup_beads; then
    skip "plans-mr-overflow module" \
      "no reachable beads daemon (set BEADS_HTTP_URL, or need kubectl access to svc/${BEADS_SVC})"
    summary
    return 0
  fi

  local rc=0
  resolve_viewer_bin || rc=$?
  if [ "${rc:-0}" != 0 ]; then
    if [ "${rc}" = 2 ]; then
      fail "resolve beads-viewer binary" "go build failed (see /tmp/beads-viewer-build-${RUN_ID}.log)"
    else
      skip "plans-mr-overflow module" \
        "no beads-viewer binary and no go toolchain / go.work to build one"
    fi
    summary
    return 0
  fi

  if ! start_viewer; then
    fail "start throwaway beads-viewer" "viewer never became healthy against $BEADS_HTTP_URL"
    summary
    return 0
  fi
  dim "  throwaway beads-viewer up on :${VIEWER_PORT} (dev-auth bypass, daemon=$BEADS_HTTP_URL)"
  echo

  scenario_under_cap
  scenario_over_cap_calm
  scenario_hidden_failed
  scenario_inline_failed_no_inflate

  summary
}

summary() {
  blue "═══════════════════════════════════════════════════"
  blue " plans-mr-overflow: $PASS passed, $FAIL failed, $SKIP skipped"
  if [ "$FAIL" -gt 0 ]; then
    red "FAILURES:"
    for e in "${ERRORS[@]}"; do red "  - $e"; done
    exit 1
  fi
  blue "═══════════════════════════════════════════════════"
}

main "$@"
