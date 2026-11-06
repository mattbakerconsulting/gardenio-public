#!/usr/bin/env bash
# test-comments-events.sh — comments and event log E2E tests
#
# Tests kd comment add/list and /v1/beads/{id}/events API against a live
# kbeads server.
#
# Usage:
#   ./tests/e2e/scripts/test-comments-events.sh
#
# Prerequisites:
#   - kd binary in PATH (or set KD_BIN)
#   - curl, jq, and python3 installed
#   - BEADS_HTTP_URL set (or kubectl context for port-forward)
#
# Scenarios tested:
#   1. Create a bead, add a comment via kd comment add
#   2. List comments and verify the comment appears
#   3. Verify the bead's event log records create, update, comment events
#   4. Add multiple comments and verify ordering
#   5. Verify comment author (actor) is correctly recorded

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CREATED_BEADS=()

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
    fail "$name" "expected output to NOT contain '$pattern'"
  fi
}

assert_eq() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$expected" = "$actual" ]; then
    pass "$name (== '$expected')"
  else
    fail "$name" "expected '$expected', got '$actual'"
  fi
}

# ── Setup: port-forward kd server ────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi

  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19098
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-comments.log 2>&1 &
  PF_PID=$!
  sleep 3

  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

# ── Setup: get token ─────────────────────────────────────────────
setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then
    return
  fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  else
    dim "  WARNING: No token found; kd server may reject requests"
  fi
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Helpers: HTTP API calls ──────────────────────────────────────
# Build the standard curl header arg list. The kd CLI auto-injects
# Authorization, X-Actor, and X-Operated-By; raw curl callers must do the same
# so writes pass the R2 fail-closed operator resolver (ka-0000005772). Without
# X-Operated-By, mutation handlers reject with HTTP 400
# "operator could not be resolved" and `curl -sf` silently swallows the
# response, masking the failure as missing data downstream.
_api_headers() {
  local -a headers=(-H "Content-Type: application/json")
  if [ -n "${BEADS_TOKEN:-}" ]; then
    headers+=(-H "Authorization: Bearer ${BEADS_TOKEN}")
  fi
  if [ -n "${BEADS_OPERATED_BY:-}" ]; then
    headers+=(-H "X-Operated-By: ${BEADS_OPERATED_BY}")
  fi
  if [ -n "${KD_ACTOR:-}" ]; then
    headers+=(-H "X-Actor: ${KD_ACTOR}")
  fi
  printf '%s\n' "${headers[@]}"
}

api_get() {
  local path="$1"
  local -a headers=()
  mapfile -t headers < <(_api_headers)
  curl -sf "${headers[@]}" \
    "${BEADS_HTTP_URL}${path}" 2>/dev/null || echo "{}"
}

api_post() {
  local path="$1"
  local body="$2"
  local -a headers=()
  mapfile -t headers < <(_api_headers)
  curl -sf -X POST "${headers[@]}" \
    -d "$body" \
    "${BEADS_HTTP_URL}${path}" 2>/dev/null || echo "{}"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " comments-events E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  # Verify kd server is reachable
  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-comments-$$-$(date +%s)"

  # ── Scenario 1: Create a bead and add a comment via kd comment add ──
  blue "Scenario 1: Create a bead and add a comment"
  local create_out create_rc bead_id
  create_out=$("$KD_BIN" create "E2E comments test $TEST_TAG" \
    --type=task -l "e2e:$TEST_TAG" \
    -d "Automated E2E comments test bead — safe to delete" \
    --actor "$KD_ACTOR" --json 2>/dev/null) && create_rc=0 || create_rc=$?

  assert_exit "kd create exits 0" 0 "$create_rc"

  bead_id=$(echo "$create_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -z "$bead_id" ]; then
    fail "kd create returns ID" "no id in output: $create_out"
    red " Cannot continue without a bead ID"
    return 1
  fi
  pass "kd create returns ID ($bead_id)"
  CREATED_BEADS+=("$bead_id")

  # Add a comment via kd CLI
  local comment_out comment_rc
  comment_out=$("$KD_BIN" comment add "$bead_id" "First comment from E2E test" \
    --actor "$KD_ACTOR" --json 2>/dev/null) && comment_rc=0 || comment_rc=$?
  assert_exit "kd comment add exits 0" 0 "$comment_rc"
  assert_contains "comment output contains text" "First comment from E2E test" "$comment_out"
  # Server derives author from the authenticated identity subject, not the client-supplied
  # --actor flag (kbeads AUDIT §C3: client-supplied author is ignored to prevent audit-log
  # spoofing). Assert the author field is present and non-empty rather than matching a
  # specific client value.
  local comment_author
  comment_author=$(echo "$comment_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('author',''))" 2>/dev/null || echo "")
  if [ -n "$comment_author" ]; then
    pass "comment output has non-empty author ($comment_author)"
  else
    fail "comment output has non-empty author" "author missing/empty: $comment_out"
  fi
  echo

  # ── Scenario 2: List comments and verify the comment appears ────────
  blue "Scenario 2: List comments via kd comment list"
  local list_out list_rc
  list_out=$("$KD_BIN" comment list "$bead_id" --json 2>/dev/null) && list_rc=0 || list_rc=$?
  assert_exit "kd comment list exits 0" 0 "$list_rc"
  assert_contains "comment list contains text" "First comment from E2E test" "$list_out"
  # Same AUDIT §C3 rationale: author is server-derived. Verify the listed comment carries
  # the same server-derived author seen on the add response.
  local list_author
  list_author=$(echo "$list_out" | python3 -c "
import sys, json
comments = json.load(sys.stdin)
# kd comment list --json returns a top-level list of comments.
if isinstance(comments, dict):
    comments = comments.get('comments', [])
for c in comments:
    if 'First comment from E2E test' in c.get('text',''):
        print(c.get('author',''))
        break
" 2>/dev/null || echo "")
  assert_eq "comment list author matches add response" "$comment_author" "$list_author"

  # Also verify via API directly
  local api_comments
  api_comments=$(api_get "/v1/beads/${bead_id}/comments")
  assert_contains "API GET comments contains text" "First comment from E2E test" "$api_comments"

  # Verify comment count is 1
  local comment_count
  comment_count=$(echo "$api_comments" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('comments',[])))" 2>/dev/null || echo "0")
  assert_eq "comment count is 1" "1" "$comment_count"
  echo

  # ── Scenario 3: Verify event log records create, update, comment events ──
  blue "Scenario 3: Verify event log"

  # First, update the bead to generate an update event
  "$KD_BIN" update "$bead_id" -d "Updated description for event log test" \
    --actor "$KD_ACTOR" 2>/dev/null

  local events_out
  events_out=$(api_get "/v1/beads/${bead_id}/events")

  # Check for create event
  assert_contains "events contain bead.created" "beads.bead.created" "$events_out"

  # Check for update event
  assert_contains "events contain bead.updated" "beads.bead.updated" "$events_out"

  # Check for comment event
  assert_contains "events contain comment.added" "beads.comment.added" "$events_out"

  # Verify event count is at least 3 (create + comment + update)
  local event_count
  event_count=$(echo "$events_out" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('events',[])))" 2>/dev/null || echo "0")
  if [ "$event_count" -ge 3 ]; then
    pass "event count >= 3 (got $event_count)"
  else
    fail "event count >= 3" "got $event_count"
  fi
  echo

  # ── Scenario 4: Add multiple comments and verify ordering ──────────
  blue "Scenario 4: Add multiple comments, verify ordering"

  "$KD_BIN" comment add "$bead_id" "Second comment" --actor "$KD_ACTOR" --json 2>/dev/null
  "$KD_BIN" comment add "$bead_id" "Third comment" --actor "$KD_ACTOR" --json 2>/dev/null

  local multi_comments
  multi_comments=$(api_get "/v1/beads/${bead_id}/comments")

  # Verify count is now 3
  local multi_count
  multi_count=$(echo "$multi_comments" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('comments',[])))" 2>/dev/null || echo "0")
  assert_eq "comment count after adding more is 3" "3" "$multi_count"

  # Verify ordering: first comment should appear before third
  local first_idx third_idx
  first_idx=$(echo "$multi_comments" | python3 -c "
import sys, json
comments = json.load(sys.stdin).get('comments', [])
for i, c in enumerate(comments):
    if 'First comment' in c.get('text', ''):
        print(i)
        break
else:
    print(-1)
" 2>/dev/null || echo "-1")

  third_idx=$(echo "$multi_comments" | python3 -c "
import sys, json
comments = json.load(sys.stdin).get('comments', [])
for i, c in enumerate(comments):
    if 'Third comment' in c.get('text', ''):
        print(i)
        break
else:
    print(-1)
" 2>/dev/null || echo "-1")

  if [ "$first_idx" != "-1" ] && [ "$third_idx" != "-1" ] && [ "$first_idx" -lt "$third_idx" ]; then
    pass "comments are in chronological order (First@$first_idx < Third@$third_idx)"
  else
    fail "comments are in chronological order" "First@$first_idx, Third@$third_idx"
  fi
  echo

  # ── Scenario 5: Verify comment author (actor) is correctly recorded ──
  blue "Scenario 5: Verify comment author"

  # Add a comment via API with a client-supplied author. Per kbeads AUDIT §C3 the
  # server MUST ignore the client-supplied "author" and derive it from the
  # authenticated identity. This is a security contract — asserting it here
  # locks the behaviour in against regressions.
  local api_comment_out api_comment_author
  api_comment_out=$(api_post "/v1/beads/${bead_id}/comments" \
    "{\"author\":\"api-test-user\",\"text\":\"Comment via API\"}")
  api_comment_author=$(echo "$api_comment_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('author',''))" 2>/dev/null || echo "")
  assert_not_contains "API ignores client-supplied author (AUDIT §C3)" "api-test-user" "$api_comment_out"
  if [ -n "$api_comment_author" ]; then
    pass "API comment response has non-empty server-derived author ($api_comment_author)"
  else
    fail "API comment response has non-empty server-derived author" "author missing/empty: $api_comment_out"
  fi

  # Verify the server-derived author persists when fetched
  local final_comments
  final_comments=$(api_get "/v1/beads/${bead_id}/comments")
  assert_not_contains "stored comment does not leak client-supplied author" "api-test-user" "$final_comments"
  assert_contains "comments contain API comment text" "Comment via API" "$final_comments"

  # Verify final count is 4 (3 from CLI + 1 from API)
  local final_count
  final_count=$(echo "$final_comments" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('comments',[])))" 2>/dev/null || echo "0")
  assert_eq "final comment count is 4" "4" "$final_count"

  # Verify the event log now has comment.added events for all 4 comments
  local final_events
  final_events=$(api_get "/v1/beads/${bead_id}/events")
  local comment_event_count
  comment_event_count=$(echo "$final_events" | python3 -c "
import sys, json
events = json.load(sys.stdin).get('events', [])
print(sum(1 for e in events if e.get('topic') == 'beads.comment.added'))
" 2>/dev/null || echo "0")
  assert_eq "comment.added event count is 4" "4" "$comment_event_count"

  # Verify events have actor field populated
  local events_with_actor
  events_with_actor=$(echo "$final_events" | python3 -c "
import sys, json
events = json.load(sys.stdin).get('events', [])
comment_events = [e for e in events if e.get('topic') == 'beads.comment.added']
with_actor = [e for e in comment_events if e.get('actor', '')]
print(len(with_actor))
" 2>/dev/null || echo "0")
  if [ "$events_with_actor" -ge 1 ]; then
    pass "comment events have actor field ($events_with_actor/$comment_event_count)"
  else
    fail "comment events have actor field" "none found"
  fi
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
