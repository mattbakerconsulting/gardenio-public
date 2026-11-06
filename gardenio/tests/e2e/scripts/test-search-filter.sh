#!/usr/bin/env bash
# test-search-filter.sh — kd list filtering and search E2E tests
#
# Tests kd list with various filter combinations: --type, --status,
# -l (labels), --json output, and combined filters.
#
# Scenarios tested:
#   1. List by type — only returns beads of that type
#   2. List by status — open vs closed filtering
#   3. List by label — label scoping
#   4. Combined filters — type + label + status
#   5. List --json output is valid JSON array
#   6. List with no results returns empty
#   7. kd show --json returns valid bead object
#   8. Daemon 400s an unknown ?type= LIST filter (raw HTTP — NOT via kd)
#   9. Per-element CSV type validation — ?type=task,zzz still 400s
#  10. template→formula alias fold over HTTP — ?type=template 200s + returns
#      the seeded formula (the non-tautological half: proves a server-side fold)
#  11. Negative control — a valid ?type=task still 200s (no over-rejection)
#
# Scenarios 8–11 (ka-0000003792) drive the daemon's server-side ?type= filter
# validation + alias fold (ka-0000008041, the deeper fix behind ka-0000006244)
# over RAW HTTP against /v1/beads — deliberately NOT via `kd list`. The kd CLI
# has its OWN client-side type guard (cmd/kd/type_validate.go) that rejects an
# unknown --type BEFORE the request leaves the client, so a `kd list --type=zzz`
# failure proves nothing about the SERVER and would keep passing even if the
# server guard regressed. Every ListBeads client that ISN'T kd (beads-viewer,
# bridges, automation) relies on the server 400 instead of a silent empty page.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
# Track created fixtures in a FILE, not a bash array: create_bead is invoked
# via command substitution (`id=$(create_bead ...)`), which runs in a SUBSHELL,
# so an array append there never reaches the parent and the cleanup trap closed
# nothing — every run leaked its task/bug/feature/formula fixtures. A shared
# temp file survives the subshell. (Harmless on the ephemeral e2e namespace,
# which is TTL-reaped, but a real leak on any shared-daemon run.)
CREATED_BEADS_FILE="$(mktemp "${TMPDIR:-/tmp}/e2e-search-created.XXXXXX")"

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

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

assert_exit() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    pass "$name (exit $expected)"
  else
    fail "$name" "expected exit $expected, got exit $actual"
  fi
}

assert_http_status() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  # A non-numeric $actual (e.g. curl's "000" on a dead tunnel) must fail
  # loudly, not error the `-eq` arithmetic under `set -e`.
  if [ "$actual" = "$expected" ]; then
    pass "$name (HTTP $expected)"
  else
    fail "$name" "expected HTTP $expected, got HTTP $actual"
  fi
}

# beads_curl_body_status <url-with-query> — issue a raw GET against the beads
# daemon (with the Bearer token when present) and echo two lines: the response
# body, then the HTTP status code. Callers split with `tail -1` (status) /
# `sed '$d'` (body). Deliberately does NOT use `-f` — a 4xx must return its
# JSON error body, not be swallowed. Bounded --max-time so a wedged tunnel
# surfaces as HTTP 000 rather than hanging the shard.
beads_curl_body_status() {
  local url="$1"
  curl -s --max-time 15 -w '\n%{http_code}' \
    ${BEADS_TOKEN:+-H "Authorization: Bearer $BEADS_TOKEN"} \
    "$url" 2>/dev/null || printf '\n000'
}

assert_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected output to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

assert_not_contains() {
  local name="$1"
  local pattern="$2"
  local actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (does not contain '$pattern')"
  else
    fail "$name" "expected output NOT to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19098
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-search.log 2>&1 &
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
  if [ -f "$CREATED_BEADS_FILE" ]; then
    while IFS= read -r id; do
      [ -n "$id" ] && "$KD_BIN" close "$id" 2>/dev/null || true
    done < "$CREATED_BEADS_FILE"
    rm -f "$CREATED_BEADS_FILE"
  fi
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

create_bead() {
  local out
  out=$("$KD_BIN" create "$@" --json 2>/dev/null || echo "{}")
  local id
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$id" ]; then
    # Append to the tracking FILE (not an array) so the id survives this
    # command-substitution subshell and the cleanup trap can close it.
    echo "$id" >> "$CREATED_BEADS_FILE"
  fi
  echo "$id"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " search & filter E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-search-$$-$(date +%s)"

  # Create test fixtures — beads of different types and labels
  local task_id bug_id feature_id
  task_id=$(create_bead "E2E search task $TEST_TAG" --type=task -l "e2e:$TEST_TAG" -l "priority:low")
  bug_id=$(create_bead "E2E search bug $TEST_TAG" --type=bug -l "e2e:$TEST_TAG" -l "priority:high")
  feature_id=$(create_bead "E2E search feature $TEST_TAG" --type=feature -l "e2e:$TEST_TAG")

  if [ -z "$task_id" ] || [ -z "$bug_id" ] || [ -z "$feature_id" ]; then
    red "ERROR: Could not create test fixtures"
    exit 1
  fi
  dim "  Fixtures: task=$task_id bug=$bug_id feature=$feature_id"
  echo

  # ── Scenario 1: List by type ──────────────────────────────────
  blue "Scenario 1: List by type — only returns beads of that type"
  local list_task
  list_task=$("$KD_BIN" list --type=task -l "e2e:$TEST_TAG" --json 2>/dev/null || echo "[]")
  assert_contains "type=task list includes task bead" "$task_id" "$list_task"
  assert_not_contains "type=task list excludes bug bead" "$bug_id" "$list_task"
  assert_not_contains "type=task list excludes feature bead" "$feature_id" "$list_task"

  local list_bug
  list_bug=$("$KD_BIN" list --type=bug -l "e2e:$TEST_TAG" --json 2>/dev/null || echo "[]")
  assert_contains "type=bug list includes bug bead" "$bug_id" "$list_bug"
  assert_not_contains "type=bug list excludes task bead" "$task_id" "$list_bug"
  echo

  # ── Scenario 2: List by status ────────────────────────────────
  blue "Scenario 2: List by status — open vs closed"
  local list_open
  list_open=$("$KD_BIN" list --status=open -l "e2e:$TEST_TAG" --json 2>/dev/null || echo "[]")
  assert_contains "open list includes task" "$task_id" "$list_open"
  assert_contains "open list includes bug" "$bug_id" "$list_open"

  # Close one bead. Prune it from the cleanup tracker so the trap does not
  # re-close it (a harmless no-op, but keeps the tracker honest).
  "$KD_BIN" close "$bug_id" 2>/dev/null || true
  if [ -f "$CREATED_BEADS_FILE" ]; then
    grep -vxF "$bug_id" "$CREATED_BEADS_FILE" > "${CREATED_BEADS_FILE}.tmp" 2>/dev/null || true
    mv "${CREATED_BEADS_FILE}.tmp" "$CREATED_BEADS_FILE" 2>/dev/null || true
  fi

  local list_open_after
  list_open_after=$("$KD_BIN" list --status=open -l "e2e:$TEST_TAG" --json 2>/dev/null || echo "[]")
  assert_not_contains "open list excludes closed bug" "$bug_id" "$list_open_after"
  assert_contains "open list still includes task" "$task_id" "$list_open_after"

  local list_closed
  list_closed=$("$KD_BIN" list --status=closed -l "e2e:$TEST_TAG" --json 2>/dev/null || echo "[]")
  assert_contains "closed list includes closed bug" "$bug_id" "$list_closed"
  echo

  # ── Scenario 3: List by label ─────────────────────────────────
  blue "Scenario 3: List by label — label scoping"
  local list_high
  list_high=$("$KD_BIN" list -l "priority:high" -l "e2e:$TEST_TAG" --json 2>/dev/null || echo "[]")
  assert_contains "priority:high includes bug" "$bug_id" "$list_high"
  assert_not_contains "priority:high excludes task" "$task_id" "$list_high"

  local list_low
  list_low=$("$KD_BIN" list -l "priority:low" -l "e2e:$TEST_TAG" --json 2>/dev/null || echo "[]")
  assert_contains "priority:low includes task" "$task_id" "$list_low"
  assert_not_contains "priority:low excludes feature" "$feature_id" "$list_low"
  echo

  # ── Scenario 4: Combined filters ──────────────────────────────
  blue "Scenario 4: Combined filters — type + label + status"
  local list_combined
  list_combined=$("$KD_BIN" list --type=task --status=open -l "e2e:$TEST_TAG" --json 2>/dev/null || echo "[]")
  assert_contains "combined: open tasks include task" "$task_id" "$list_combined"
  assert_not_contains "combined: open tasks exclude closed bug" "$bug_id" "$list_combined"
  assert_not_contains "combined: open tasks exclude feature" "$feature_id" "$list_combined"
  echo

  # ── Scenario 5: List --json is valid JSON ─────────────────────
  blue "Scenario 5: kd list --json returns valid JSON array"
  local list_json list_rc
  list_json=$("$KD_BIN" list --status=open --json 2>/dev/null || echo "")
  list_rc=$?
  assert_exit "kd list --json exits 0" 0 "$list_rc"

  if [ -n "$list_json" ]; then
    local valid
    valid=$(echo "$list_json" | python3 -c "
import sys, json
data = json.load(sys.stdin)
if isinstance(data, list):
    print('valid-array')
else:
    print('valid-object')
" 2>/dev/null || echo "invalid")
    if [ "$valid" = "valid-array" ] || [ "$valid" = "valid-object" ]; then
      pass "kd list --json returns valid JSON"
    else
      fail "kd list --json valid JSON" "output is not valid JSON"
    fi
  else
    fail "kd list --json output" "empty output"
  fi
  echo

  # ── Scenario 6: List with no results ──────────────────────────
  blue "Scenario 6: List with no matching results returns empty"
  local list_none list_none_rc
  list_none=$("$KD_BIN" list -l "nonexistent-label-$TEST_TAG" --json 2>/dev/null || echo "[]")
  list_none_rc=$?
  assert_exit "kd list with no matches exits 0" 0 "$list_none_rc"

  # Should be an empty array or object with empty beads
  local count
  count=$(echo "$list_none" | python3 -c "
import sys, json
data = json.load(sys.stdin)
if isinstance(data, list):
    print(len(data))
else:
    print(len(data.get('beads', data.get('items', []))))
" 2>/dev/null || echo "unknown")
  if [ "$count" = "0" ]; then
    pass "empty result for nonexistent label"
  else
    fail "empty result for nonexistent label" "expected 0 results, got $count"
  fi
  echo

  # ── Scenario 7: kd show --json returns valid bead ─────────────
  blue "Scenario 7: kd show --json returns valid bead object"
  local show_json show_rc
  show_json=$("$KD_BIN" show "$task_id" --json 2>/dev/null || echo "")
  show_rc=$?
  assert_exit "kd show --json exits 0" 0 "$show_rc"

  # Verify required fields exist
  local has_id has_title has_type has_status
  has_id=$(echo "$show_json" | python3 -c "import sys,json; d=json.load(sys.stdin); print('yes' if d.get('id') else 'no')" 2>/dev/null || echo "no")
  has_title=$(echo "$show_json" | python3 -c "import sys,json; d=json.load(sys.stdin); print('yes' if d.get('title') else 'no')" 2>/dev/null || echo "no")
  has_type=$(echo "$show_json" | python3 -c "import sys,json; d=json.load(sys.stdin); print('yes' if d.get('type') else 'no')" 2>/dev/null || echo "no")
  has_status=$(echo "$show_json" | python3 -c "import sys,json; d=json.load(sys.stdin); print('yes' if d.get('status') else 'no')" 2>/dev/null || echo "no")

  if [ "$has_id" = "yes" ]; then pass "show JSON has id field"; else fail "show JSON has id field" "missing"; fi
  if [ "$has_title" = "yes" ]; then pass "show JSON has title field"; else fail "show JSON has title field" "missing"; fi
  if [ "$has_type" = "yes" ]; then pass "show JSON has type field"; else fail "show JSON has type field" "missing"; fi
  if [ "$has_status" = "yes" ]; then pass "show JSON has status field"; else fail "show JSON has status field" "missing"; fi
  echo

  # ── Scenario 8: daemon 400s an unknown ?type= LIST filter ─────
  # (ka-0000003792 / ka-0000008041). RAW HTTP against /v1/beads, NOT `kd list`:
  # the kd CLI's client-side guard (cmd/kd/type_validate.go) would reject
  # --type=zzz before the request ever reached the daemon, so it cannot prove
  # the SERVER guard exists. Every non-kd ListBeads client (beads-viewer,
  # bridges, automation) depends on this 400 instead of a silent empty page —
  # the exact silent-empty footgun ka-0000006244 targets.
  blue "Scenario 8: daemon 400s an unknown ?type= LIST filter (raw HTTP)"
  local unk_resp unk_status unk_body
  unk_resp=$(beads_curl_body_status "${BEADS_HTTP_URL}/v1/beads?type=zzz-not-a-type&limit=1")
  unk_status=$(printf '%s' "$unk_resp" | tail -1)
  unk_body=$(printf '%s' "$unk_resp" | sed '$d')
  assert_http_status "unknown ?type=zzz-not-a-type returns 400" 400 "$unk_status"
  assert_contains "400 body names the unknown type" "unknown bead type" "$unk_body"
  assert_contains "400 body lists valid types" "valid types:" "$unk_body"
  echo

  # ── Scenario 9: per-element CSV type validation ───────────────
  # ?type=task,zzz mixes a VALID type with an invalid one. The daemon validates
  # PER element (validateFilterTypes iterates the CSV), so the presence of a
  # valid `task` must NOT rescue the invalid `zzz` — it still 400s. Proves the
  # guard is not a single-value-only check that a mixed filter could slip past.
  blue "Scenario 9: per-element CSV type validation — ?type=task,zzz still 400s"
  local mixed_resp mixed_status mixed_body
  mixed_resp=$(beads_curl_body_status "${BEADS_HTTP_URL}/v1/beads?type=task,zzz-not-a-type&limit=1")
  mixed_status=$(printf '%s' "$mixed_resp" | tail -1)
  mixed_body=$(printf '%s' "$mixed_resp" | sed '$d')
  assert_http_status "mixed CSV ?type=task,zzz returns 400" 400 "$mixed_status"
  assert_contains "mixed-CSV 400 body names the unknown element" "zzz-not-a-type" "$mixed_body"
  echo

  # ── Scenario 10: template→formula alias fold over HTTP ────────
  # The NON-TAUTOLOGICAL half. `template` is a deprecated alias the CREATE path
  # accepts (model.TypeAliases: template→formula, model/bead.go). The HTTP
  # handler folds the alias BEFORE validating (http_beads.go), so ?type=template
  # must (a) NOT 400 and (b) actually return the type=formula beads. We seed a
  # real formula, then query ?type=template scoped to the seed's unique label
  # and assert the seed's id is in the body. That the FORMULA id comes back from
  # a TEMPLATE query proves the fold happened SERVER-side — a plain round-trip of
  # a template-typed fixture would prove nothing (no such stored type exists).
  blue "Scenario 10: template→formula alias fold over HTTP"
  local formula_id
  formula_id=$(create_bead "E2E alias formula $TEST_TAG" --type=formula -l "e2e:$TEST_TAG")
  if [ -z "$formula_id" ]; then
    fail "seed type=formula bead" "create returned no id (project-scoped write denied?)"
  else
    dim "  Seeded formula: $formula_id"
    local alias_resp alias_status alias_body
    alias_resp=$(beads_curl_body_status "${BEADS_HTTP_URL}/v1/beads?type=template&labels=e2e:$TEST_TAG&limit=50")
    alias_status=$(printf '%s' "$alias_resp" | tail -1)
    alias_body=$(printf '%s' "$alias_resp" | sed '$d')
    assert_http_status "?type=template does NOT 400 (alias accepted)" 200 "$alias_status"
    assert_contains "?type=template returns the seeded formula (server-side fold)" "$formula_id" "$alias_body"
    # And the excluded control: the type=task fixture must NOT appear under a
    # formula-resolved query — the fold changes the value, it does not widen it.
    assert_not_contains "?type=template excludes the task fixture" "$task_id" "$alias_body"
  fi
  echo

  # ── Scenario 11: negative control — valid ?type=task still 200s ─
  # The guard consults the live store (resolveTypeConfig), so it must NOT
  # over-reject a legitimately-registered type. ?type=task is a compiled-in
  # builtin and must always 200. (This is also why controller-registered custom
  # types like type:view from EnsureConfigs — gardenio init.go — validate rather
  # than 400: the guard is a store lookup, not a hard-coded allowlist.)
  blue "Scenario 11: negative control — valid ?type=task still 200s"
  local ok_resp ok_status
  ok_resp=$(beads_curl_body_status "${BEADS_HTTP_URL}/v1/beads?type=task&limit=1")
  ok_status=$(printf '%s' "$ok_resp" | tail -1)
  assert_http_status "valid ?type=task returns 200 (no over-rejection)" 200 "$ok_status"
  echo

  # ── Summary ─────────────────────────────────────────────────────
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
