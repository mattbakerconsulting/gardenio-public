#!/usr/bin/env bash
# test-admin-strip-primary.sh — E2E for `gb admin strip-primary`
#
# `gb admin strip-primary` is Phase 6 (the LAST, explicitly IRREVERSIBLE phase)
# of the abolish-primary epic ka-0000008240. It has TWO wire semantics that a
# unit test cannot prove, because the unit test (admin_strip_primary_test.go)
# drives an in-memory fakeStripClient whose "unset" is a plain delete(map,key)
# and whose description write is a plain map assignment — both sides of the wire
# behave identically for "", "null", and absent. The load-bearing behavior lives
# on the FAR side of the daemon wire:
#
#   1. Account marker removal is an RFC 7386 top-level-null DELETE
#      (UpdateBeadFieldsTyped(id, {"primary": nil}) -> kbeads beads.go:1057),
#      so the `primary` field becomes ABSENT — distinct from empty-string or a
#      stored literal "null". planStripPrimary keys on `v, ok := Fields["primary"];
#      if !ok || v == ""`, so a degraded delete that wrote "null" would re-plan
#      forever (non-idempotent) and one that wrote "" would read as already-clean.
#      The `has("primary") == false` distinction is THE assertion this module
#      exists to defend; the fake passes every degraded variant.
#   2. Config:auth description rewrite drops ONLY `primary_account`, preserving
#      the `account` field and any unknown/future observability keys byte-for-byte
#      (stripPrimaryAccountFromDesc rewrites a generic map, not a typed struct).
#
# It also guards the operator-safety surface of an irreversible migration:
#   - `--dry-run` NAMES every planned mutation and leaves all beads UNCHANGED.
#   - `--snapshot-out <file>` writes a parseable pre-mutation snapshot whose
#     prev_primary / prev_description are the hand-restore payload — a silent
#     snapshot no-op means an operator runs the irreversible step with no
#     restore data and finds out only after the data is gone.
#
# ── Why we do NOT run `gb admin strip-primary --apply` here ──────────────────
# The runtime command is FLEET-WIDE and has NO id/project scoping: one apply
# strips `primary` / `primary_account` from EVERY account and config:auth bead on
# the daemon. The sharded e2e suite shares ONE daemon per dispatch, and sibling
# modules (test-credential-rotation.sh:455, test-session-resume.sh:704,
# test-cred-rotation-resume.sh) seed config:auth beads carrying `primary_account`
# on that same daemon. A real --apply from here would clobber a concurrent
# sibling's state mid-assertion (role advice gd-TXvglBUtiA). So we exercise the
# REAL command only on its READ-ONLY surface (--dry-run / --snapshot-out, zero
# blast radius) and prove the APPLY semantic (null-delete -> ABSENT, unknown-key
# preservation, idempotence) via the EXACT scoped wire calls the migration makes
# — `kd update --unset-field primary` (same {"primary": null} patch as the
# runtime's UnsetField) and `kd update -d <stripped-desc>` — against ONLY our own
# throwaway beads. The plan/snapshot that the fleet-wide dry-run produces for
# THOSE beads is what wires the two halves together: we assert the real command's
# derived new_description is exactly what a correct apply would write, then apply
# it by the scoped path.
#
# Usage:
#   ./tests/e2e/scripts/test-admin-strip-primary.sh [--namespace <ns>]
#
# Prerequisites:
#   - kubectl context at the target cluster (or BEADS_HTTP_URL preset by run-shard)
#   - kd + gb binaries in PATH (or KD_BIN / GB_BIN)
#   - jq installed

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
CLEANUP_BEADS=()
TMPDIR_STRIP=""

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }
have()  { command -v "$1" >/dev/null 2>&1; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}

assert_equals() {
  local name="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    pass "$name (== '$expected')"
  else
    fail "$name" "expected '$expected', got '$actual'"
  fi
}

assert_contains() {
  local name="$1" pattern="$2" actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected output to contain '$pattern'; got: $actual"
  fi
}

# SKIP loud — a missing prerequisite (no gb/kd/jq, no reachable daemon) must
# never be mistaken for a pass, but must not red the suite (this module is wired
# into a shard). Use ONLY for a genuine environment gap, NEVER after a fail().
skip_all() {
  blue "SKIP admin-strip-primary: $1"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

# ── Setup: port-forward kd server (skipped when BEADS_HTTP_URL preset) ────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19124
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-strip-primary.log 2>&1 &
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
  else
    dim "  WARNING: No token found; kd server may reject requests"
  fi
}

# gb reads BEADS_AUTH_TOKEN (main.go authToken precedence); kd reads BEADS_TOKEN.
# Mirror whichever we have onto both so both binaries authenticate identically to
# what run-shard already exports in CI.
mirror_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then
    export BEADS_AUTH_TOKEN="${BEADS_AUTH_TOKEN:-$BEADS_TOKEN}"
  elif [ -n "${BEADS_AUTH_TOKEN:-}" ]; then
    export BEADS_TOKEN="$BEADS_AUTH_TOKEN"
  fi
}

# ── Cleanup ───────────────────────────────────────────────────────
# shellcheck disable=SC2317  # invoked indirectly via `trap cleanup EXIT`
cleanup() {
  for id in "${CLEANUP_BEADS[@]:-}"; do
    [ -n "$id" ] || continue
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  [ -n "$TMPDIR_STRIP" ] && [ -d "$TMPDIR_STRIP" ] && rm -rf "$TMPDIR_STRIP"
  if [ -n "$PF_PID" ]; then kill "$PF_PID" 2>/dev/null || true; fi
  return 0
}
trap cleanup EXIT

# Create an account bead (primary field carried as a STRING — the account schema
# types `primary` as string, and `kd -f primary=true` would auto-type a JSON
# boolean the schema rejects, so we pass the JSON-quoted string "true").
create_account() {
  local title="$1"; shift
  local out id
  out=$("$KD_BIN" create "$title" --type=account --json "$@" 2>/dev/null)
  id=$(echo "$out" | jq -r '.id // empty' 2>/dev/null || echo "")
  [ -n "$id" ] && CLEANUP_BEADS+=("$id")
  echo "$id"
}

create_config() {
  local title="$1"; shift
  local out id
  out=$("$KD_BIN" create "$title" --type=config --json "$@" 2>/dev/null)
  id=$(echo "$out" | jq -r '.id // empty' 2>/dev/null || echo "")
  [ -n "$id" ] && CLEANUP_BEADS+=("$id")
  echo "$id"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " gb admin strip-primary E2E (Phase 6, epic ka-0000008240)"
  blue "═══════════════════════════════════════════════════"

  have jq        || skip_all "jq not available"
  have "$KD_BIN" || skip_all "kd binary not available (set KD_BIN)"
  have "$GB_BIN" || skip_all "gb binary not available (set GB_BIN)"

  setup_portforward
  setup_token
  mirror_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    skip_all "kd server unreachable at ${BEADS_HTTP_URL:-<unset>}"
  fi
  green "  Server reachable: ${BEADS_HTTP_URL:-<unset>}"
  echo

  TMPDIR_STRIP="$(mktemp -d -t strip-primary-e2e-XXXXXX)"
  local suffix
  suffix="$$-$(date +%s)"
  local TEST_PROJECT="e2e-strip-${suffix}"
  local acct_title="e2e-strip-acct-${suffix}"
  # A distinctive unknown key + value so "preserved byte-for-byte" is unambiguous.
  local unknown_key="zz_unknown_key"
  local unknown_val="KEEPME-${suffix}"

  # ── Seed: one account with primary="true", one config:auth carrying
  #         primary_account + an unknown observability key. ─────────────────
  blue "Seed: account (primary=\"true\") + config:auth (primary_account + unknown key)"

  local acct_id
  acct_id=$(create_account "$acct_title" \
    -l "project:$TEST_PROJECT" \
    -f "provider=anthropic" \
    -f "credential=secret:e2e-strip" \
    -f 'primary="true"')
  if [ -z "$acct_id" ]; then
    fail "seed account bead" "kd create returned no id"
    print_summary_and_exit
  fi
  CLEANUP_BEADS+=("$acct_id")
  pass "account bead seeded ($acct_id)"

  # Confirm the marker really landed as a non-empty string before we test removal.
  local seeded_primary
  seeded_primary=$("$KD_BIN" show "$acct_id" --json 2>/dev/null | jq -r '.fields.primary // empty')
  assert_equals "seeded account carries primary=true" "true" "$seeded_primary"

  local cfg_desc
  cfg_desc="{\"account\":\"${acct_title}\",\"primary_account\":\"${acct_title}\",\"${unknown_key}\":\"${unknown_val}\"}"
  local cfg_id
  cfg_id=$(create_config "auth" -l "project:$TEST_PROJECT" -d "$cfg_desc")
  if [ -z "$cfg_id" ]; then
    fail "seed config:auth bead" "kd create returned no id"
    print_summary_and_exit
  fi
  pass "config:auth bead seeded ($cfg_id)"
  echo

  # ── Scenario 1: --dry-run NAMES both planned mutations, mutates NOTHING ──
  blue "Scenario 1: --dry-run plans both beads and leaves them UNCHANGED"

  local dry_out
  dry_out=$("$GB_BIN" admin strip-primary --dry-run 2>&1 || true)
  assert_contains "dry-run announces DRY RUN" "DRY RUN" "$dry_out"
  assert_contains "dry-run names the account marker removal" \
    "[plan] account $acct_id" "$dry_out"
  assert_contains "dry-run says it will remove primary=true" \
    "remove primary=true" "$dry_out"
  assert_contains "dry-run names the config primary_account drop" \
    "[plan] config $cfg_id" "$dry_out"

  # The load-bearing safety property: dry-run mutated nothing.
  local after_primary after_cfg_pa
  after_primary=$("$KD_BIN" show "$acct_id" --json 2>/dev/null | jq -r '.fields.primary // empty')
  assert_equals "dry-run left account primary UNCHANGED" "true" "$after_primary"
  after_cfg_pa=$("$KD_BIN" show "$cfg_id" --json 2>/dev/null \
    | jq -r '.description' | jq -r 'has("primary_account")')
  assert_equals "dry-run left config primary_account UNCHANGED" "true" "$after_cfg_pa"
  echo

  # ── Scenario 2: --snapshot-out writes a parseable pre-mutation snapshot ──
  blue "Scenario 2: --snapshot-out writes a parseable snapshot with restore payload"

  local snap="${TMPDIR_STRIP}/phase6-snapshot.json"
  local snap_out
  snap_out=$("$GB_BIN" admin strip-primary --dry-run --snapshot-out "$snap" 2>&1 || true)
  if [ -f "$snap" ]; then
    pass "snapshot file written to disk"
    # The snapshot file is JSON body prefixed with `// ` comment header lines;
    # the restore payload must parse after stripping them.
    local snap_body
    snap_body=$(grep -v '^//' "$snap" || true)
    if echo "$snap_body" | jq -e '.' >/dev/null 2>&1; then
      pass "snapshot JSON body parses"

      # prev_primary for OUR account is the exact hand-restore value.
      local snap_prev_primary
      snap_prev_primary=$(echo "$snap_body" | jq -r \
        '.accounts[]? | select(.bead_id=="'"$acct_id"'") | .prev_primary')
      assert_equals "snapshot records prev_primary=true for our account" \
        "true" "$snap_prev_primary"

      # prev_description round-trips to JSON and still carries primary_account +
      # the unknown key — i.e. the restore payload is the FULL original desc.
      local snap_prev_has_pa snap_prev_unknown
      snap_prev_has_pa=$(echo "$snap_body" | jq -r \
        '.configs[]? | select(.bead_id=="'"$cfg_id"'") | .prev_description' \
        | jq -r 'has("primary_account")')
      assert_equals "snapshot prev_description still has primary_account (restore payload)" \
        "true" "$snap_prev_has_pa"
      snap_prev_unknown=$(echo "$snap_body" | jq -r \
        '.configs[]? | select(.bead_id=="'"$cfg_id"'") | .prev_description' \
        | jq -r ".${unknown_key} // empty")
      assert_equals "snapshot prev_description preserves unknown key" \
        "$unknown_val" "$snap_prev_unknown"
    else
      fail "snapshot JSON body parses" "body: $(echo "$snap_body" | head -c 200)"
    fi
  else
    fail "snapshot file written to disk" "no file at $snap; out: $snap_out"
  fi
  # --snapshot-out is still a dry-run: nothing mutated.
  after_primary=$("$KD_BIN" show "$acct_id" --json 2>/dev/null | jq -r '.fields.primary // empty')
  assert_equals "snapshot-out dry-run left account UNCHANGED" "true" "$after_primary"
  echo

  # ── Scenario 3: apply semantic — null-delete makes primary ABSENT ───────
  # We apply via the EXACT wire call the migration makes (kd --unset-field sends
  # the same {"primary": null} patch as daemonAdapter.UnsetField), scoped to OUR
  # account only. THE load-bearing assertion: has("primary") == false — ABSENT,
  # not "" and not the literal "null".
  blue "Scenario 3: apply removes the marker to ABSENT (RFC 7386 null-delete)"

  "$KD_BIN" update "$acct_id" --unset-field primary >/dev/null 2>&1 || true
  local apply_show apply_has apply_raw
  apply_show=$("$KD_BIN" show "$acct_id" --json 2>/dev/null || echo '{}')
  apply_has=$(echo "$apply_show" | jq -r '.fields | has("primary")')
  assert_equals "primary field is ABSENT after null-delete" "false" "$apply_has"
  # Belt-and-suspenders: the field must not have degraded to "" or "null".
  apply_raw=$(echo "$apply_show" | jq -r '.fields.primary // "<absent>"')
  assert_equals "primary is not degraded to empty/null string" "<absent>" "$apply_raw"
  echo

  # ── Scenario 4: idempotence against the REAL daemon ─────────────────────
  # planStripPrimary would re-plan forever if the delete had degraded to "null"
  # (it keys on `!ok || v == ""`). A clean account must now be OMITTED from the
  # plan, and a second unset must be a harmless no-op.
  blue "Scenario 4: idempotence — cleaned account is no longer planned"

  local dry2
  dry2=$("$GB_BIN" admin strip-primary --dry-run 2>&1 || true)
  if grep -qF "[plan] account $acct_id" <<< "$dry2"; then
    fail "cleaned account excluded from re-plan" \
      "account $acct_id still appears in dry-run plan (non-idempotent — degraded delete?)"
  else
    pass "cleaned account excluded from re-plan"
  fi
  local reunset_rc=0
  "$KD_BIN" update "$acct_id" --unset-field primary >/dev/null 2>&1 || reunset_rc=$?
  assert_equals "re-unset of an absent field is a no-op (exit 0)" "0" "$reunset_rc"
  local still_absent
  still_absent=$("$KD_BIN" show "$acct_id" --json 2>/dev/null | jq -r '.fields | has("primary")')
  assert_equals "primary stays ABSENT after re-unset" "false" "$still_absent"
  echo

  # ── Scenario 5: config strip drops ONLY primary_account, preserves rest ─
  # Apply the migration's derived new_description (taken from the REAL command's
  # snapshot) via the scoped `kd update -d` path, then assert primary_account is
  # gone while `account` and the unknown key survive byte-for-byte.
  blue "Scenario 5: config strip drops primary_account, preserves account + unknown key"

  local snap_new_desc
  snap_new_desc=$(grep -v '^//' "$snap" 2>/dev/null \
    | jq -r '.configs[]? | select(.bead_id=="'"$cfg_id"'") | .new_description' 2>/dev/null || echo "")
  if [ -z "$snap_new_desc" ] || [ "$snap_new_desc" = "null" ]; then
    fail "derived new_description available from snapshot" \
      "no new_description for $cfg_id in snapshot"
  else
    pass "derived new_description available from snapshot"
    # Sanity: the command's derived new_description already has primary_account
    # gone and the unknown key kept — proves the pure rewrite, before we apply.
    local derived_has_pa derived_unknown
    derived_has_pa=$(echo "$snap_new_desc" | jq -r 'has("primary_account")')
    assert_equals "derived new_description has primary_account removed" "false" "$derived_has_pa"
    derived_unknown=$(echo "$snap_new_desc" | jq -r ".${unknown_key} // empty")
    assert_equals "derived new_description keeps unknown key" "$unknown_val" "$derived_unknown"

    # Apply it (scoped to our bead) and verify the persisted result.
    "$KD_BIN" update "$cfg_id" -d "$snap_new_desc" >/dev/null 2>&1 || true
    local cfg_after cfg_has_pa cfg_unknown cfg_account
    cfg_after=$("$KD_BIN" show "$cfg_id" --json 2>/dev/null | jq -r '.description' || echo '{}')
    cfg_has_pa=$(echo "$cfg_after" | jq -r 'has("primary_account")')
    assert_equals "applied config has primary_account removed" "false" "$cfg_has_pa"
    cfg_unknown=$(echo "$cfg_after" | jq -r ".${unknown_key} // empty")
    assert_equals "applied config preserves unknown key byte-for-byte" "$unknown_val" "$cfg_unknown"
    cfg_account=$(echo "$cfg_after" | jq -r '.account // empty')
    assert_equals "applied config preserves account field" "$acct_title" "$cfg_account"
  fi
  echo

  print_summary_and_exit
}

print_summary_and_exit() {
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
  else
    red " ✗ $FAIL/$total assertions failed"
    for err in "${ERRORS[@]}"; do red "   - $err"; done
  fi
  blue "═══════════════════════════════════════════════════"
  [ "$FAIL" -eq 0 ]
  exit $?
}

main "$@"
