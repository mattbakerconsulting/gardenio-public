#!/usr/bin/env bash
# test-list-with-blockers.sh — `kd list --with-blockers` open_blockers[] projection E2E
#
# Backfills the ZERO e2e coverage on da5fc505f (feat(kbeads): kd list
# --with-blockers — per-dependency blocker projection, ka-0000000945), verified
# HEAD-clean by gardener ka-0000000738: all three e2e suites grepped, no hit on
# with-blockers / open_blockers / BLOCKED BY. Task ka-0000002610.
#
# Why an e2e and not the existing unit tests: the store-layer tests
# (TestQueryGetOpenBlockersForBeads / TestQueryListBeads_WithOpenBlockers in
# kbeads/internal/store/postgres/postgres_test.go) are go-sqlmock tests — they
# assert the SQL *string* and the row-scan but NEVER execute the deps⋈beads join
# against real Postgres. sqlmock cannot catch a malformed join, a wrong column
# order, or a status/edge-type predicate that leaks a closed blocker. The
# failure mode is SILENT WRONG DATA — an agent using --with-blockers to triage
# would see an empty/wrong open_blockers[] and treat a GATED bead as READY — so
# the un-executed join is the untested surface this module drives, live.
#
# Scenarios (scope of ka-0000002610):
#   1. JSON projection: a bead gated by TWO OPEN blockers (different statuses AND
#      edge types) plus one CLOSED blocker → open_blockers[] contains EXACTLY the
#      two OPEN ones with correct {id,status,type,title,edge_type}; the closed
#      blocker is EXCLUDED (the load-bearing assertion — a query returning all
#      blockers regardless of status would pass a weaker test).
#   2. Table path: the BLOCKED BY cell renders the blocker ids, capped at 3 with
#      a "+N more" elision.
#   3. Old-daemon refusal: the hard error at cmd/kd/list.go:92 that stops a
#      falsely-unblocked-looking list from printing. Driven against a MOCK old
#      daemon (rows present, open_blockers absent) — deterministic and
#      version-independent, since the live daemon always answers.
#   4. ANTI-VACUITY control: a bead with NO blockers renders an EMPTY
#      open_blockers[] / a "-" BLOCKED BY cell. Without this leg a module
#      asserting only "blockers appear" would pass against an implementation that
#      returned every bead's blockers for every row.
#
# "What would have to BREAK for this to go red?" (advice gd-4E9o92p0DY):
#   - the deps⋈beads join returns the wrong set               → leg 1 reds
#   - the closed-blocker status predicate is dropped          → leg 1 (excl) reds
#   - edge_type/status denormalization is wrong               → leg 1 fields red
#   - the table BLOCKED BY column or 3-id cap regresses       → leg 2 reds
#   - the version-skew guard stops refusing an all-absent page→ leg 3 reds
#   - the enrichment returns blockers for an unblocked row    → leg 4 reds

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
MOCK_PID=""
MOCK_PORT=18771
CREATED_BEADS=()
PROJECT="e2e-wb-$$"

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# Load shared retry helpers so transient port-forward drops don't fail a whole
# scenario (kd_retry, retry_on_empty).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-retry.sh"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() {
  local name="$1"
  green "  ✓ $name"
  PASS=$((PASS + 1))
}

fail() {
  local name="$1"
  local reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

assert_eq() {
  local name="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    pass "$name (= '$expected')"
  else
    fail "$name" "expected '$expected', got '$actual'"
  fi
}

assert_contains() {
  local name="$1" pattern="$2" actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected output to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

assert_not_contains() {
  local name="$1" pattern="$2" actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (does not contain '$pattern')"
  else
    fail "$name" "expected output to NOT contain '$pattern'"
  fi
}

# jq_get PATH < json — thin python3 wrapper (matches test-dependencies.sh's
# python3 usage; python3 is a documented CI prereq). Prints the value or "".
py() { python3 -c "$1" 2>/dev/null || true; }

create_task() {
  local title="$1"
  local out id
  out=$(kd_retry "$KD_BIN" create "$title" --type=task -l "project:${PROJECT}" --json 2>/dev/null)
  id=$(echo "$out" | py "import sys,json; print(json.load(sys.stdin).get('id',''))")
  if [ -n "$id" ]; then
    CREATED_BEADS+=("$id")
  fi
  echo "$id"
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19095
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-with-blockers.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  fi
}

cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    if [ -n "$id" ]; then
      kd_retry "$KD_BIN" close "$id" 2>/dev/null || true
    fi
  done
  if [ -n "$MOCK_PID" ]; then
    kill "$MOCK_PID" 2>/dev/null || true
  fi
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " list --with-blockers E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! kd_retry "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at ${BEADS_HTTP_URL:-<unset>}"
    exit 1
  fi
  green "  Server reachable: ${BEADS_HTTP_URL:-<in-cluster>}"
  echo

  # ── Seed: one gated bead + two OPEN blockers (distinct status+edge type)
  #    + one CLOSED blocker + one unblocked control bead ─────────────
  blue "Seeding blocker graph (project ${PROJECT})"
  local B_OPEN B_INPROG B_CLOSED GATED CONTROL
  B_OPEN=$(create_task "wb-blocker-open-$$")
  B_INPROG=$(create_task "wb-blocker-inprog-$$")
  B_CLOSED=$(create_task "wb-blocker-closed-$$")
  GATED=$(create_task "wb-gated-$$")
  CONTROL=$(create_task "wb-control-$$")
  if [ -z "$B_OPEN" ] || [ -z "$B_INPROG" ] || [ -z "$B_CLOSED" ] || [ -z "$GATED" ] || [ -z "$CONTROL" ]; then
    fail "seed" "could not create the five fixture beads"
    finish; return
  fi
  dim "  gated=$GATED  blockers: open=$B_OPEN inprog=$B_INPROG closed=$B_CLOSED  control=$CONTROL"

  # B_INPROG → in_progress (a DIFFERENT open-status than B_OPEN, so leg 1 also
  # proves the status set is {open, in_progress, ...} not just 'open').
  kd_retry "$KD_BIN" update "$B_INPROG" --status=in_progress --assignee wb-inprog 2>/dev/null || true
  # B_CLOSED → closed: this is the blocker that MUST be excluded from the projection.
  kd_retry "$KD_BIN" close "$B_CLOSED" 2>/dev/null || true
  # Drop the closed blocker from the cleanup list (already closed).
  CREATED_BEADS=("${CREATED_BEADS[@]/$B_CLOSED/}")

  # Edges: GATED is gated by all three, via TWO different blocking edge types
  # (blocks + depends-on) so leg 1 proves edge_type is denormalized per-edge.
  kd_retry "$KD_BIN" dep add "$GATED" "$B_OPEN"   --type blocks     2>/dev/null || true
  kd_retry "$KD_BIN" dep add "$GATED" "$B_INPROG" --type depends-on 2>/dev/null || true
  kd_retry "$KD_BIN" dep add "$GATED" "$B_CLOSED" --type blocks     2>/dev/null || true
  echo

  # ── Scenario 1: JSON open_blockers[] — exact OPEN set, closed excluded ──
  blue "Scenario 1: --with-blockers --json projects exactly the OPEN blockers"
  local json
  json=$(kd_retry "$KD_BIN" list --project "$PROJECT" --with-blockers --json 2>/dev/null || echo "[]")

  # Pull GATED's open_blockers as a sorted, normalized digest so the assertions
  # are order-independent (the server orders by bead_id,blocker_id, but we do
  # not want the test to depend on id lexical order).
  local gated_ids gated_count closed_present
  gated_count=$(echo "$json" | py "
import sys,json
d=json.load(sys.stdin)
for b in d:
    if b.get('id')=='$GATED':
        ob=b.get('open_blockers')
        print(len(ob) if ob is not None else 'NIL')
        break
else:
    print('NOROW')
")
  assert_eq "GATED open_blockers count = 2 (closed one excluded)" "2" "$gated_count"

  gated_ids=$(echo "$json" | py "
import sys,json
d=json.load(sys.stdin)
for b in d:
    if b.get('id')=='$GATED':
        print(','.join(sorted(x['id'] for x in (b.get('open_blockers') or []))))
        break
")
  local want_ids
  want_ids=$(printf '%s\n%s\n' "$B_OPEN" "$B_INPROG" | sort | paste -sd, -)
  assert_eq "GATED open_blockers ids = {open, inprog}" "$want_ids" "$gated_ids"

  closed_present=$(echo "$json" | py "
import sys,json
d=json.load(sys.stdin)
for b in d:
    if b.get('id')=='$GATED':
        print('yes' if any(x['id']=='$B_CLOSED' for x in (b.get('open_blockers') or [])) else 'no')
        break
")
  assert_eq "CLOSED blocker EXCLUDED from projection" "no" "$closed_present"

  # Per-blocker field fidelity: status, type, title AND edge_type for each edge.
  local open_fields inprog_fields
  open_fields=$(echo "$json" | py "
import sys,json
d=json.load(sys.stdin)
for b in d:
    if b.get('id')=='$GATED':
        for x in (b.get('open_blockers') or []):
            if x['id']=='$B_OPEN':
                print('%s|%s|%s|%s' % (x.get('status'),x.get('type'),x.get('edge_type'),x.get('title')))
")
  assert_eq "open blocker fields {status|type|edge_type|title}" \
    "open|task|blocks|wb-blocker-open-$$" "$open_fields"

  inprog_fields=$(echo "$json" | py "
import sys,json
d=json.load(sys.stdin)
for b in d:
    if b.get('id')=='$GATED':
        for x in (b.get('open_blockers') or []):
            if x['id']=='$B_INPROG':
                print('%s|%s|%s|%s' % (x.get('status'),x.get('type'),x.get('edge_type'),x.get('title')))
")
  assert_eq "in_progress blocker fields {status|type|edge_type|title}" \
    "in_progress|task|depends-on|wb-blocker-inprog-$$" "$inprog_fields"
  echo

  # ── Scenario 4 (checked here, same JSON): control bead has EMPTY set ──
  blue "Scenario 4: anti-vacuity — unblocked control bead has open_blockers == []"
  local control_state
  control_state=$(echo "$json" | py "
import sys,json
d=json.load(sys.stdin)
for b in d:
    if b.get('id')=='$CONTROL':
        ob=b.get('open_blockers')
        print('MISSING' if ob is None else ('EMPTY' if len(ob)==0 else 'NONEMPTY:%d'%len(ob)))
        break
else:
    print('NOROW')
")
  # A present-but-empty array (&[] on the wire) is the load-bearing shape: it is
  # what distinguishes "checked, nothing blocks" from "not checked" (nil). An
  # implementation that copied every bead's blockers onto every row would make
  # this NONEMPTY; a nil here would mean the enrichment silently no-op'd.
  assert_eq "CONTROL open_blockers is present-and-empty" "EMPTY" "$control_state"
  echo

  # ── Scenario 2: table path renders BLOCKED BY with the 3-id cap ──────
  blue "Scenario 2: table path renders the BLOCKED BY column (+ 3-id cap)"
  local table
  table=$(kd_retry "$KD_BIN" list --project "$PROJECT" --with-blockers 2>/dev/null || echo "")
  assert_contains "table has BLOCKED BY header" "BLOCKED BY" "$table"
  # The GATED row names its two open blockers with their status; the control row
  # shows "-". Grep the GATED row specifically.
  local gated_row control_row
  gated_row=$(echo "$table" | grep -F "$GATED" || true)
  control_row=$(echo "$table" | grep -F "$CONTROL" || true)
  assert_contains "GATED row cites the open blocker"   "$B_OPEN"   "$gated_row"
  assert_contains "GATED row cites the in_progress blocker" "$B_INPROG" "$gated_row"
  assert_not_contains "GATED row does NOT cite the closed blocker" "$B_CLOSED" "$gated_row"
  # The BLOCKED BY cell is the LAST column, so the control row's final
  # whitespace-delimited field is that cell. Assert it is exactly "-" rather
  # than grepping for a bare "-" — every row contains "kd-" in its id, so a
  # substring match would pass vacuously. This checks the cell, not the id.
  local control_cell
  control_cell=$(echo "$control_row" | awk '{print $NF}')
  assert_eq "unblocked control BLOCKED BY cell is '-'" "-" "$control_cell"

  # 3-id cap: add two MORE open blockers to GATED (total 4 open) → the table cell
  # names 3 and elides the rest with "+N more" (openBlockerSummaryMax=3).
  local B_X B_Y
  B_X=$(create_task "wb-cap-x-$$")
  B_Y=$(create_task "wb-cap-y-$$")
  if [ -n "$B_X" ] && [ -n "$B_Y" ]; then
    kd_retry "$KD_BIN" dep add "$GATED" "$B_X" --type blocks 2>/dev/null || true
    kd_retry "$KD_BIN" dep add "$GATED" "$B_Y" --type blocks 2>/dev/null || true
    local capped_row
    capped_row=$(kd_retry "$KD_BIN" list --project "$PROJECT" --with-blockers 2>/dev/null \
      | grep -F "$GATED" || true)
    assert_contains "table cell elides past 3 blockers with '+N more'" "more" "$capped_row"
    # JSON is UNcapped — all four open blockers are present there (cap is a
    # table-cell affordance only, not a data truncation).
    local json_capped
    json_capped=$(kd_retry "$KD_BIN" list --project "$PROJECT" --with-blockers --json 2>/dev/null \
      | py "
import sys,json
for b in json.load(sys.stdin):
    if b.get('id')=='$GATED':
        print(len(b.get('open_blockers') or []))
        break
")
    assert_eq "JSON projection is uncapped (4 open blockers)" "4" "$json_capped"
  else
    fail "cap-setup" "could not create the two extra cap blockers"
  fi
  echo

  # ── Scenario 3: old-daemon refusal (the guard that makes the failure loud) ──
  blue "Scenario 3: --with-blockers REFUSES an old daemon's all-absent page"
  # The live daemon always answers, so the version-skew guard at
  # cmd/kd/list.go:92 can never fire against it. Drive it deterministically
  # against a mock "old daemon" that returns rows but NEVER an open_blockers
  # key — byte-identical to a daemon that predates the param.
  python3 - "$MOCK_PORT" <<'PY' &
import http.server, json, sys
port = int(sys.argv[1])
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        # Old daemon: rows present, open_blockers key ABSENT even when asked.
        body = json.dumps({"beads": [
            {"id": "gd-old1", "status": "open", "type": "task", "title": "row-a", "priority": 2},
            {"id": "gd-old2", "status": "open", "type": "task", "title": "row-b", "priority": 2},
        ], "total": 2}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
PY
  MOCK_PID=$!
  sleep 1
  local mock_url="http://127.0.0.1:${MOCK_PORT}"

  # Baseline: WITHOUT the flag the same old-daemon page prints fine (proves the
  # refusal is specific to --with-blockers, not a broken mock).
  local base_rc
  BEADS_HTTP_URL="$mock_url" BEADS_TOKEN="mock" BEADS_AUTH_TOKEN="mock" \
    "$KD_BIN" list --http-url "$mock_url" --json >/dev/null 2>&1 && base_rc=0 || base_rc=$?
  assert_eq "old-daemon page prints WITHOUT --with-blockers (exit 0)" "0" "$base_rc"

  # WITH the flag: hard refusal, non-zero exit, specific error text.
  local refuse_out refuse_rc
  refuse_out=$(BEADS_HTTP_URL="$mock_url" BEADS_TOKEN="mock" BEADS_AUTH_TOKEN="mock" \
    "$KD_BIN" list --http-url "$mock_url" --with-blockers --json 2>&1) && refuse_rc=0 || refuse_rc=$?
  if [ "$refuse_rc" -ne 0 ]; then
    pass "old-daemon --with-blockers refuses (exit $refuse_rc)"
  else
    fail "old-daemon --with-blockers refuses" "expected non-zero exit, got 0"
  fi
  assert_contains "refusal names the version-skew cause" \
    "too old to support this flag" "$refuse_out"
  assert_contains "refusal states it is refusing to print" \
    "Refusing to print an unblocked-looking list" "$refuse_out"

  kill "$MOCK_PID" 2>/dev/null || true
  MOCK_PID=""
  echo

  finish
}

finish() {
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
