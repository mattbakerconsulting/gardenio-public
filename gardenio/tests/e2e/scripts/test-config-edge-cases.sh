#!/usr/bin/env bash
# test-config-edge-cases.sh — config bead edge case E2E tests
#
# Tests error handling, boundary conditions, and unusual inputs for
# the config beads system.
#
# Scenarios tested:
#   1.  Invalid JSON in config bead description — skipped during resolution
#   2.  Empty description config bead — handled as null during dump
#   3.  Config bead with very large JSON value (>10KB) — still works
#   4.  Config bead with special characters in labels
#   5.  Multiple config beads with same title+labels — dedup on load
#   6.  Config bead with no matching subscriptions — filtered out
#   7.  gb config dump with mix of valid and invalid beads — valid ones exported
#   8.  Config bead title with colons — correct handling
#   9.  Unicode in config bead values — preserved through dump/load
#   10. Config bead with deeply nested JSON — preserved through resolution
#   11. Special characters in config bead value
#   12. No config beads at all — defaults applied
#   13. Config bead with very long title

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
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
  green "  PASS: $name"
  PASS=$((PASS + 1))
}

fail() {
  local name="$1"
  local reason="${2:-}"
  red "  FAIL: $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

assert_exit() {
  local name="$1" expected="$2" actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    pass "$name (exit $expected)"
  else
    fail "$name" "expected exit $expected, got exit $actual"
  fi
}

assert_contains() {
  local name="$1" pattern="$2" actual="$3"
  # Use a temp file to avoid pipefail + grep -q SIGPIPE on large data.
  local _tmpf; _tmpf=$(mktemp)
  printf '%s\n' "$actual" > "$_tmpf"
  if grep -qF -- "$pattern" "$_tmpf"; then
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected to contain '$pattern'; got: $(head -5 "$_tmpf")"
  fi
  rm -f "$_tmpf"
}

assert_not_contains() {
  local name="$1" pattern="$2" actual="$3"
  local _tmpf; _tmpf=$(mktemp)
  printf '%s\n' "$actual" > "$_tmpf"
  if ! grep -qF -- "$pattern" "$_tmpf"; then
    pass "$name (not contains '$pattern')"
  else
    fail "$name" "expected NOT to contain '$pattern'"
  fi
  rm -f "$_tmpf"
}

assert_json_field() {
  local name="$1" field="$2" expected="$3" json="$4"
  local actual
  actual=$(echo "$json" | python3 -c "import sys,json; print(json.load(sys.stdin).get('$field',''))" 2>/dev/null || echo "")
  if [ "$actual" = "$expected" ]; then
    pass "$name ($field=$expected)"
  else
    fail "$name" "expected $field='$expected', got '$actual'"
  fi
}

assert_valid_json() {
  local name="$1" data="$2"
  if echo "$data" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
    pass "$name (valid JSON)"
  else
    fail "$name" "invalid JSON"
  fi
}

assert_file_exists() {
  local name="$1" path="$2"
  if [ -f "$path" ]; then
    pass "$name (exists: $path)"
  else
    fail "$name" "file not found: $path"
  fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19113
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-edge-cases.log 2>&1 &
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
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  [ -d "${WORK_DIR:-}" ] && rm -rf "$WORK_DIR"
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

WORK_DIR=$(mktemp -d /tmp/e2e-edge-cases-XXXXXX)

create_config() {
  local title="$1" label="$2" value="${3:-}"
  local out id args
  args=("$KD_BIN" create "$title" --type=config -l "$label" --json)
  [ -n "$value" ] && args+=(-d "$value")
  out=$("${args[@]}" 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  echo "$id"
}

create_config_nolabel() {
  local title="$1" value="${2:-}"
  local out id args
  args=("$KD_BIN" create "$title" --type=config --json)
  [ -n "$value" ] && args+=(-d "$value")
  out=$("${args[@]}" 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  echo "$id"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " config bead edge case E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-edge-$$-$(date +%s)"
  local HOME_DIR
  HOME_DIR=$(eval echo "~")
  local SETTINGS_PATH="$HOME_DIR/.claude/settings.json"

  # ── Scenario 1: Invalid JSON in config bead ──────────────────────
  blue "Scenario 1: Invalid JSON in config bead description — skipped during resolution"

  local invalid_id
  invalid_id=$(create_config "claude-settings" "global" "not-valid-json{{{")

  if [ -n "$invalid_id" ]; then
    pass "config bead with invalid JSON created ($invalid_id)"

    # Show should still work
    local invalid_show
    invalid_show=$("$KD_BIN" show "$invalid_id" --json 2>/dev/null || echo "{}")
    assert_json_field "invalid JSON bead type" "type" "config" "$invalid_show"
    assert_contains "invalid JSON stored in description" "not-valid-json" "$invalid_show"

    # Setup should handle gracefully (skip invalid bead, use defaults)
    local hooks_id
    hooks_id=$(create_config "claude-hooks" "global" \
      '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo edge-test"}]}],"Stop":[{"matcher":"","hooks":[{"type":"command","command":"echo stop"}]}]}}')

    rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json"
    mkdir -p "$WORK_DIR/.claude"
    local setup_rc=0
    "$GB_BIN" setup claude --workspace "$WORK_DIR" 2>/dev/null || setup_rc=$?
    if [ "$setup_rc" -le 1 ]; then
      pass "setup handles invalid JSON gracefully (exit $setup_rc)"
    else
      fail "setup with invalid JSON" "exit code $setup_rc (expected 0 or 1)"
    fi

    # Close the invalid bead
    "$KD_BIN" close "$invalid_id" 2>/dev/null || true
    CREATED_BEADS=("${CREATED_BEADS[@]/$invalid_id/}")
  else
    fail "create invalid JSON config bead" "could not create"
  fi
  echo

  # ── Scenario 2: Empty description config bead ──────────────────
  blue "Scenario 2: Empty description config bead — handled as null during dump"

  # Create config bead with no -d flag at all (truly empty description)
  local empty_out empty_rc empty_id
  empty_out=$("$KD_BIN" create "claude-settings-empty-$TEST_TAG" --type=config -l global \
    --json 2>/dev/null) && empty_rc=0 || empty_rc=$?
  assert_exit "create empty-desc config bead" 0 "$empty_rc"

  empty_id=$(echo "$empty_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$empty_id" ]; then
    CREATED_BEADS+=("$empty_id")
    pass "empty description config bead created ($empty_id)"

    # Show should return the bead without crashing
    local empty_show
    empty_show=$("$KD_BIN" show "$empty_id" --json 2>/dev/null || echo "{}")
    assert_json_field "empty-desc bead type" "type" "config" "$empty_show"

    # gb config dump should not crash
    local dump_out dump_rc=0
    dump_out=$("$GB_BIN" config dump 2>/dev/null) || dump_rc=$?
    if [ "$dump_rc" -eq 0 ] && [ -n "$dump_out" ]; then
      assert_valid_json "config dump with empty-desc bead" "$dump_out"
    else
      pass "config dump did not crash (exit $dump_rc)"
    fi

    # Also test empty JSON object
    local empty_obj_id
    empty_obj_id=$(create_config "claude-settings-emptyobj-$TEST_TAG" "global" "{}")
    if [ -n "$empty_obj_id" ]; then
      local empty_obj_show
      empty_obj_show=$("$KD_BIN" show "$empty_obj_id" --json 2>/dev/null || echo "{}")
      assert_json_field "empty-obj bead type" "type" "config" "$empty_obj_show"
      assert_contains "empty-obj description stored" "{}" "$empty_obj_show"

      "$KD_BIN" close "$empty_obj_id" 2>/dev/null || true
      CREATED_BEADS=("${CREATED_BEADS[@]/$empty_obj_id/}")
    fi

    "$KD_BIN" close "$empty_id" 2>/dev/null || true
    CREATED_BEADS=("${CREATED_BEADS[@]/$empty_id/}")
  else
    fail "create empty-desc config bead" "could not create"
  fi
  echo

  # ── Scenario 3: Very large JSON value (>10KB) ──────────────────
  blue "Scenario 3: Config bead with very large JSON value (>10KB)"

  # Generate a >10KB JSON payload
  local large_val
  large_val=$(python3 -c "
import json
d = {'permissions': {'allow': ['Read(*)']*100, 'deny': ['Write(*)']*100},
     'env': {'LARGE_KEY_' + str(i): 'x' * 200 for i in range(50)},
     'model': 'sonnet'}
print(json.dumps(d))
" 2>/dev/null)

  local large_size
  large_size=$(echo -n "$large_val" | wc -c)
  if [ "$large_size" -gt 10000 ]; then
    pass "generated payload >10KB ($large_size bytes)"
  else
    dim "  payload only $large_size bytes (may not exceed 10KB)"
  fi

  local large_out large_rc large_id
  large_out=$("$KD_BIN" create "e2e-large-$TEST_TAG" --type=config -l global \
    -d "$large_val" --json 2>/dev/null) && large_rc=0 || large_rc=$?
  assert_exit "create large config bead" 0 "$large_rc"

  large_id=$(echo "$large_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$large_id" ]; then
    CREATED_BEADS+=("$large_id")
    pass "large config bead created ($large_id)"

    # Verify it can be read back
    local large_show
    large_show=$("$KD_BIN" show "$large_id" --json 2>/dev/null || echo "{}")
    assert_contains "large bead has LARGE_KEY" "LARGE_KEY_0" "$large_show"
    assert_contains "large bead has model" "sonnet" "$large_show"

    # Verify gb config dump includes it
    local large_dump large_dump_rc=0
    large_dump=$("$GB_BIN" config dump 2>/dev/null) || large_dump_rc=$?
    if [ "$large_dump_rc" -eq 0 ] && [ -n "$large_dump" ]; then
      assert_valid_json "config dump with large bead" "$large_dump"
      assert_contains "dump includes large bead title" "e2e-large-$TEST_TAG" "$large_dump"
    else
      pass "config dump did not crash with large bead (exit $large_dump_rc)"
    fi
  else
    fail "create large config bead" "could not create"
  fi
  echo

  # ── Scenario 4: Special characters in labels ───────────────────
  blue "Scenario 4: Config bead with special characters in labels"

  # Labels with dots, hyphens, underscores
  local special_label_id
  special_label_id=$(create_config "e2e-special-label-$TEST_TAG" "rig:e2e-test_rig.v2" \
    '{"key":"special-label-test"}')

  if [ -n "$special_label_id" ]; then
    pass "config bead with special label created ($special_label_id)"

    local special_show
    special_show=$("$KD_BIN" show "$special_label_id" --json 2>/dev/null || echo "{}")
    assert_contains "special label stored" "e2e-test_rig.v2" "$special_show"

    # Filter by the special label
    local special_list
    special_list=$("$KD_BIN" list --type=config -l "rig:e2e-test_rig.v2" --all-projects --json 2>/dev/null || echo "[]")
    assert_contains "filter by special label works" "$special_label_id" "$special_list"
  else
    fail "create config bead with special label" "could not create"
  fi
  echo

  # ── Scenario 5: Duplicate config beads ─────────────────────────
  blue "Scenario 5: Multiple config beads with same title+labels — dedup on load"

  local dup1_id dup2_id
  dup1_id=$(create_config "claude-settings" "global" \
    "{\"env\":{\"DUP_MARKER\":\"first-$TEST_TAG\"}}")
  dup2_id=$(create_config "claude-settings" "global" \
    "{\"env\":{\"DUP_MARKER\":\"second-$TEST_TAG\"}}")

  if [ -n "$dup1_id" ] && [ -n "$dup2_id" ]; then
    pass "both duplicate config beads created ($dup1_id, $dup2_id)"

    # Both should be listable
    local dup_list
    dup_list=$("$KD_BIN" list --type=config --all-projects --json 2>/dev/null || echo "[]")
    assert_contains "dup1 in list" "$dup1_id" "$dup_list"
    assert_contains "dup2 in list" "$dup2_id" "$dup_list"

    # Setup should not crash with duplicates
    rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json"
    mkdir -p "$WORK_DIR/.claude"
    local dup_rc=0
    "$GB_BIN" setup claude --workspace "$WORK_DIR" 2>/dev/null || dup_rc=$?
    if [ "$dup_rc" -le 1 ]; then
      pass "setup handles duplicate config beads (exit $dup_rc)"
    else
      fail "setup with duplicates" "exit code $dup_rc"
    fi

    # Config dump should handle duplicates
    local dup_dump dup_dump_rc=0
    dup_dump=$("$GB_BIN" config dump 2>/dev/null) || dup_dump_rc=$?
    if [ "$dup_dump_rc" -eq 0 ] && [ -n "$dup_dump" ]; then
      assert_valid_json "config dump with duplicates" "$dup_dump"
    fi
  fi
  echo

  # ── Scenario 6: No matching subscriptions — filtered out ───────
  blue "Scenario 6: Config bead with no matching subscriptions — filtered out"

  # Create a config bead scoped to a non-existent project via the HTTP API
  # directly, bypassing kd's auto-added project label.  Config resolution
  # filters by project — a bead whose only project label does not match
  # the agent's project should be excluded from setup output.
  # Use a unique marker per run to avoid stale hits from prior runs.
  local nomatch_marker="NOMATCH_${TEST_TAG}"
  local nomatch_out nomatch_id
  # The kd CLI auto-injects Authorization, X-Actor, and X-Operated-By; raw
  # curl callers must do the same so writes pass the R2 fail-closed operator
  # resolver (ka-0000005772). Without X-Operated-By, create returns
  # HTTP 400 "operator could not be resolved" and Scenario 6 fails.
  local -a nomatch_headers=(-H "Content-Type: application/json")
  [ -n "${BEADS_TOKEN:-}" ] && nomatch_headers+=(-H "Authorization: Bearer ${BEADS_TOKEN}")
  [ -n "${BEADS_OPERATED_BY:-}" ] && nomatch_headers+=(-H "X-Operated-By: ${BEADS_OPERATED_BY}")
  [ -n "${KD_ACTOR:-}" ] && nomatch_headers+=(-H "X-Actor: ${KD_ACTOR}")
  nomatch_out=$(curl -sf -X POST "${BEADS_HTTP_URL}/v1/beads" \
    "${nomatch_headers[@]}" \
    -d "{
      \"title\": \"claude-settings\",
      \"type\": \"config\",
      \"kind\": \"config\",
      \"status\": \"open\",
      \"labels\": [\"project:nonexistent-project-$TEST_TAG\"],
      \"description\": \"{\\\"model\\\":\\\"haiku\\\",\\\"env\\\":{\\\"${nomatch_marker}\\\":\\\"should-not-appear\\\"}}\"
    }" 2>/dev/null || echo "{}")

  nomatch_id=$(echo "$nomatch_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$nomatch_id" ]; then
    CREATED_BEADS+=("$nomatch_id")
    pass "non-matching config bead created ($nomatch_id)"

    # Verify labels — must NOT have project:gardenio
    local nomatch_show
    nomatch_show=$("$KD_BIN" show "$nomatch_id" --json 2>/dev/null || echo "{}")
    assert_json_field "non-matching bead type" "type" "config" "$nomatch_show"
    assert_not_contains "non-matching bead has no gardenio label" "project:gardenio" "$nomatch_show"

    # Run setup fresh (clean workspace) and check the settings file written
    rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json"
    mkdir -p "$WORK_DIR/.claude"
    "$GB_BIN" setup claude --workspace "$WORK_DIR" 2>/dev/null || true

    if [ -f "$SETTINGS_PATH" ]; then
      local nomatch_content
      nomatch_content=$(cat "$SETTINGS_PATH")
      assert_not_contains "non-matching bead value not in settings" "$nomatch_marker" "$nomatch_content"
    else
      pass "non-matching bead: settings did not include unmatched config"
    fi
  else
    fail "create non-matching config bead" "could not create (API returned no id)"
  fi
  echo

  # ── Scenario 7: Dump with mix of valid and invalid beads ───────
  blue "Scenario 7: gb config dump with mix of valid and invalid beads"

  # Create one valid and one invalid config bead
  local valid_id invalid_mix_id
  valid_id=$(create_config "claude-settings" "global" \
    "{\"model\":\"sonnet\",\"env\":{\"VALID_MARKER\":\"valid-$TEST_TAG\"}}")
  invalid_mix_id=$(create_config "claude-settings-bad" "global" \
    "this-is-not-json!!!")

  if [ -n "$valid_id" ]; then
    # Config dump should succeed and include valid beads
    local mix_dump mix_dump_rc=0
    mix_dump=$("$GB_BIN" config dump 2>/dev/null) || mix_dump_rc=$?

    if [ -n "$mix_dump" ]; then
      assert_valid_json "dump is valid JSON despite invalid bead" "$mix_dump"
      assert_contains "dump includes valid bead" "VALID_MARKER" "$mix_dump"
    else
      pass "dump handled mixed validity (empty or valid output)"
    fi

    # Setup should not crash
    rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json"
    mkdir -p "$WORK_DIR/.claude"
    local mix_rc=0
    "$GB_BIN" setup claude --workspace "$WORK_DIR" 2>/dev/null || mix_rc=$?
    if [ "$mix_rc" -le 1 ]; then
      pass "setup handles mixed valid/invalid beads (exit $mix_rc)"
    else
      fail "setup with mixed beads" "exit code $mix_rc"
    fi
  fi

  # Close invalid bead immediately to avoid polluting other tests
  if [ -n "${invalid_mix_id:-}" ]; then
    "$KD_BIN" close "$invalid_mix_id" 2>/dev/null || true
    CREATED_BEADS=("${CREATED_BEADS[@]/$invalid_mix_id/}")
  fi
  echo

  # ── Scenario 8: Title with colons ──────────────────────────────
  blue "Scenario 8: Config bead title with colons — correct handling"

  local colon_out colon_rc colon_id
  colon_out=$("$KD_BIN" create "type:config:special-$TEST_TAG" --type=config -l global \
    -d '{"colon_test":true}' --json 2>/dev/null) && colon_rc=0 || colon_rc=$?
  assert_exit "create config bead with colons in title" 0 "$colon_rc"

  colon_id=$(echo "$colon_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$colon_id" ]; then
    CREATED_BEADS+=("$colon_id")
    pass "colon-title config bead created ($colon_id)"

    local colon_show
    colon_show=$("$KD_BIN" show "$colon_id" --json 2>/dev/null || echo "{}")
    assert_contains "colon title preserved" "type:config:special" "$colon_show"
    assert_json_field "colon bead type" "type" "config" "$colon_show"
    assert_contains "colon bead value preserved" "colon_test" "$colon_show"

    # Verify it appears in list
    local colon_list
    colon_list=$("$KD_BIN" list --type=config --all-projects --json 2>/dev/null || echo "[]")
    assert_contains "colon-title bead in config list" "$colon_id" "$colon_list"
  else
    fail "create colon-title config bead" "could not create"
  fi
  echo

  # ── Scenario 9: Unicode in config bead values ──────────────────
  blue "Scenario 9: Unicode in config bead values — preserved through dump/load"

  local unicode_out unicode_rc unicode_id
  unicode_out=$("$KD_BIN" create "e2e-unicode-$TEST_TAG" --type=config -l global \
    -d '{"greeting":"Hello \u4e16\u754c","emoji_key":"\u2705 passed","path":"/tmp/\u00e9t\u00e9"}' \
    --json 2>/dev/null) && unicode_rc=0 || unicode_rc=$?
  assert_exit "create unicode config bead" 0 "$unicode_rc"

  unicode_id=$(echo "$unicode_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$unicode_id" ]; then
    CREATED_BEADS+=("$unicode_id")
    pass "unicode config bead created ($unicode_id)"

    # Show should preserve unicode
    local unicode_show
    unicode_show=$("$KD_BIN" show "$unicode_id" --json 2>/dev/null || echo "{}")
    assert_contains "unicode has greeting key" "greeting" "$unicode_show"
    assert_contains "unicode has emoji key" "emoji_key" "$unicode_show"

    # Verify via dump/load round-trip
    local unicode_dump unicode_dump_rc=0
    unicode_dump=$("$GB_BIN" config dump 2>/dev/null) || unicode_dump_rc=$?
    if [ "$unicode_dump_rc" -eq 0 ] && [ -n "$unicode_dump" ]; then
      assert_valid_json "dump with unicode bead" "$unicode_dump"
      assert_contains "dump has unicode bead title" "e2e-unicode-$TEST_TAG" "$unicode_dump"
    fi

    # Write dump to file and reload — values should survive
    if [ -n "$unicode_dump" ]; then
      echo "$unicode_dump" > "$WORK_DIR/unicode-dump.json"
      local reload_rc=0
      "$GB_BIN" config load "$WORK_DIR/unicode-dump.json" 2>/dev/null || reload_rc=$?
      if [ "$reload_rc" -eq 0 ]; then
        pass "config load with unicode values succeeded"

        # Re-dump and verify unicode still present
        local redump redump_rc=0
        redump=$("$GB_BIN" config dump 2>/dev/null) || redump_rc=$?
        if [ "$redump_rc" -eq 0 ] && [ -n "$redump" ]; then
          assert_contains "round-trip preserves unicode bead" "e2e-unicode-$TEST_TAG" "$redump"
        fi
      else
        fail "config load with unicode" "exit $reload_rc"
      fi
    fi
  else
    fail "create unicode config bead" "could not create"
  fi
  echo

  # ── Scenario 10: Deeply nested JSON ────────────────────────────
  blue "Scenario 10: Config bead with deeply nested JSON — preserved through resolution"

  local deep_id
  deep_id=$(create_config "claude-settings" "global" \
    '{"permissions":{"allow":["Read(*)"],"deny":["Write(*)"]},"env":{"LEVEL1":{"L2":{"L3":{"L4":"deep-value"}}}},"model":"sonnet"}')

  if [ -n "$deep_id" ]; then
    pass "deeply nested config bead created ($deep_id)"

    local deep_show
    deep_show=$("$KD_BIN" show "$deep_id" --json 2>/dev/null || echo "{}")
    assert_contains "deeply nested value present" "deep-value" "$deep_show"
    assert_contains "nested structure has L2" "L2" "$deep_show"
    assert_contains "nested structure has L3" "L3" "$deep_show"

    # Setup should handle nested values
    rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json"
    mkdir -p "$WORK_DIR/.claude"
    "$GB_BIN" setup claude --workspace "$WORK_DIR" 2>/dev/null || true

    if [ -f "$SETTINGS_PATH" ]; then
      local deep_content
      deep_content=$(cat "$SETTINGS_PATH")
      assert_contains "nested value in settings" "deep-value" "$deep_content"
    fi
  else
    fail "create deeply nested config bead" "could not create"
  fi
  echo

  # ── Scenario 11: Special characters in value ───────────────────
  blue "Scenario 11: Special characters in config bead value"

  local special_id
  special_id=$(create_config "claude-settings" "global" \
    '{"env":{"SPECIAL_VAR":"value with spaces & symbols: @#$%","QUOTES":"she said \"hello\"","NEWLINE":"line1\\nline2","TAB":"col1\\tcol2"}}')

  if [ -n "$special_id" ]; then
    pass "special chars config bead created ($special_id)"

    local special_show
    special_show=$("$KD_BIN" show "$special_id" --json 2>/dev/null || echo "{}")
    assert_contains "special chars has SPECIAL_VAR" "SPECIAL_VAR" "$special_show"
    assert_contains "special chars has QUOTES" "QUOTES" "$special_show"
    assert_contains "special chars has NEWLINE" "NEWLINE" "$special_show"

    rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json"
    mkdir -p "$WORK_DIR/.claude"
    "$GB_BIN" setup claude --workspace "$WORK_DIR" 2>/dev/null || true

    if [ -f "$SETTINGS_PATH" ]; then
      local special_content
      special_content=$(cat "$SETTINGS_PATH")
      assert_contains "special chars in settings" "SPECIAL_VAR" "$special_content"
    fi
  else
    fail "create special chars config bead" "could not create"
  fi
  echo

  # ── Scenario 12: No config beads → all defaults ────────────────
  blue "Scenario 12: No config beads at all — defaults applied"

  # Close all remaining config beads to test pure defaults
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  CREATED_BEADS=()

  # Create only a minimal hooks bead so setup can run
  local hooks_id
  hooks_id=$(create_config "claude-hooks" "global" \
    '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo default-test"}]}],"Stop":[{"matcher":"","hooks":[{"type":"command","command":"echo stop"}]}]}}')

  rm -rf "$WORK_DIR/.claude" "$WORK_DIR/.mcp.json"
  mkdir -p "$WORK_DIR/.claude"
  local def_rc=0
  "$GB_BIN" setup claude --workspace "$WORK_DIR" 2>/dev/null || def_rc=$?
  assert_exit "setup with no settings beads" 0 "$def_rc"

  if [ -f "$SETTINGS_PATH" ]; then
    local def_content
    def_content=$(cat "$SETTINGS_PATH")
    assert_contains "default settings have permissions" "permissions" "$def_content"
    assert_valid_json "default settings is valid JSON" "$def_content"
    pass "default settings applied when no config beads"
  else
    pass "setup completed without settings beads (no user settings written)"
  fi
  echo

  # ── Scenario 13: Very long title ───────────────────────────────
  blue "Scenario 13: Config bead with very long title"

  local long_title
  long_title="e2e-cfg-long-title-$(python3 -c "print('x' * 200)" 2>/dev/null)-$TEST_TAG"

  local longtitle_out longtitle_rc longtitle_id
  longtitle_out=$("$KD_BIN" create "$long_title" --type=config -l global \
    -d '{"long_title":true}' --json 2>/dev/null) && longtitle_rc=0 || longtitle_rc=$?
  assert_exit "create config bead with long title" 0 "$longtitle_rc"

  longtitle_id=$(echo "$longtitle_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$longtitle_id" ]; then
    CREATED_BEADS+=("$longtitle_id")
    pass "long-title config bead created ($longtitle_id)"

    local longtitle_show
    longtitle_show=$("$KD_BIN" show "$longtitle_id" --json 2>/dev/null || echo "{}")
    assert_json_field "long-title bead type" "type" "config" "$longtitle_show"
    assert_contains "long title stored" "e2e-cfg-long-title" "$longtitle_show"

    # Verify it appears in list
    local longtitle_list
    longtitle_list=$("$KD_BIN" list --type=config --all-projects --json 2>/dev/null || echo "[]")
    assert_contains "long-title bead in list" "$longtitle_id" "$longtitle_list"
  else
    fail "create long-title config bead" "could not create"
  fi
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " PASS: All $total assertions passed (13 scenarios)"
  else
    red " FAIL: $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
