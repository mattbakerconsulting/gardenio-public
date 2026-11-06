#!/usr/bin/env bash
# test-pool-enable-disable-rmw.sh — LIVE beadsapi contract test for the
# read-modify-write (RMW) semantics of `gb pool disable` / `gb pool enable`
# (coverage-backfill ka-0000005192, epic ka-0000000958 — "E2E coverage gap
# sprint"). Anchors on the RMW-preserve logic at cmd/gb/pool.go:115-188.
#
# The property under test:
#
#   `gb pool enable` / `gb pool disable` do a READ-MODIFY-WRITE on the project
#   bead's `prewarmed_pool` JSON field: they flip ONLY `enabled` while
#   PRESERVING the existing role/min_size/max_size/mode, and — on a brand-new
#   (never-configured) pool — `enable` FILLS the historical K8s defaults
#   (role=thread, min_size=2, max_size=5, mode=crew). This is the exact
#   behavior that separates the CLI from a raw `kd update -f prewarmed_pool={…}`
#   WHOLE-OBJECT write, which clobbers every unspecified field.
#
# Why the gap was real (gardener ka-0000007838, HEAD-verified at main 52f3ba34):
# every existing pool e2e (pool-manager, pool-recycle, pool-assign-attribution)
# sets `prewarmed_pool` via a raw `kd update -f prewarmed_pool={…}` whole-object
# PATCH — precisely the clobbering write the RMW path exists to AVOID. GB_BIN is
# declared but never invoked in those files; the three "pool enable/disable"
# grep hits there are prose or the wrong mechanism. So the RMW-preserve path was
# uncovered end-to-end. This module drives the ACTUAL `gb pool enable/disable`
# binary against a real daemon and asserts the OTHER fields survive.
#
# Scenarios:
#   1. Disable preserves config — seed a fully-populated pool (enabled=true,
#      role/min/max/mode all set) via a raw whole-object write, run
#      `gb pool disable`, and assert ONLY `enabled` flipped to false while
#      role/min_size/max_size/mode survived byte-for-byte.
#   2. Enable preserves config — from the disabled state above, run
#      `gb pool enable` and assert `enabled` flipped back to true while every
#      other field survived unchanged (the round-trip is lossless).
#   3. Brand-new pool defaults — `gb pool enable` on a project with NO
#      prewarmed_pool field fills the historical K8s defaults
#      (role=thread, min_size=2, max_size=5, mode=crew, enabled=true).
#   4. Partial config: defaults fill ONLY unset fields — seed a partial config
#      (role + max_size set; min_size/mode unset), run `gb pool enable`, and
#      assert the SET fields (role, max_size) survived while ONLY the unset ones
#      (min_size, mode) took defaults — the RMW never clobbers an explicit value.
#   5. Disable on a never-configured pool — `gb pool disable` on a project with
#      no prewarmed_pool writes enabled=false without inventing a config
#      (role/mode stay empty, sizes stay 0) — disable is a pure flag flip and
#      does not fabricate defaults the way enable does.
#
# Self-SKIPs (exit 0) when the daemon is unreachable / no token, so it is inert
# where it cannot run and never reds the suite (ka-0000000847) — a skip is never
# mistaken for a pass.
#
# Idempotency: every seeded project carries the title prefix pool-rmw-e2e-<rand>
# and is closed in the cleanup trap. Reruns do not collide.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-pool-enable-disable-rmw.sh

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19841}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
TITLE_PREFIX="pool-rmw-e2e"
RANDOM_SUFFIX="$$-$(date +%s)"
OPERATOR="${OPERATOR:-oidc:e2e-pool-rmw@example.com}"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
BEADS_PF_PID=""

export KD_ACTOR="${KD_ACTOR:-e2e-test}"
# kd/gb mutations need a non-empty operated_by (ka-0000005772).
export BEADS_OPERATED_BY="${BEADS_OPERATED_BY:-$OPERATOR}"

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

# All project beads we create, closed in the trap.
CREATED_BEADS=()

cleanup() {
  for id in "${CREATED_BEADS[@]:-}"; do
    [ -n "$id" ] || continue
    "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  [ -n "$BEADS_PF_PID" ] && kill "$BEADS_PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

# ── Setup: reach the daemon + load the token ──────────────────────
# If BEADS_HTTP_URL/BEADS_AUTH_TOKEN are already exported (a direct daemon run),
# use them as-is. Otherwise port-forward the in-cluster daemon and read the
# kd-beads-token secret — the same token an agent pod carries as BEADS_AUTH_TOKEN.
setup_daemon() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  using BEADS_HTTP_URL=$BEADS_HTTP_URL"
  else
    blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
    kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
      "${BEADS_PF_PORT}:8080" >/tmp/pool-rmw-beads-pf.log 2>&1 &
    BEADS_PF_PID=$!
    sleep 3
    export BEADS_HTTP_URL="http://localhost:${BEADS_PF_PORT}"
  fi
  if [ -z "${BEADS_AUTH_TOKEN:-}" ]; then
    # Accept BEADS_TOKEN (other modules' name) as a fallback source.
    if [ -n "${BEADS_TOKEN:-}" ]; then
      export BEADS_AUTH_TOKEN="$BEADS_TOKEN"
    else
      local secret
      secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
        -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
      [ -n "$secret" ] && export BEADS_AUTH_TOKEN="$secret"
    fi
  fi
  # kd reads BEADS_HTTP_URL/BEADS_TOKEN; align both names so seeding works.
  export BEADS_TOKEN="${BEADS_TOKEN:-${BEADS_AUTH_TOKEN:-}}"
}

# daemon_reachable — a token-authenticated read must succeed, else SKIP.
daemon_reachable() {
  [ -n "${BEADS_AUTH_TOKEN:-}" ] || return 1
  curl -sf -o /dev/null --max-time 10 \
    -H "Authorization: Bearer ${BEADS_AUTH_TOKEN}" \
    "${BEADS_HTTP_URL}/v1/health" 2>/dev/null
}

# gb_pool — run the REAL gb binary against the daemon (token-only). An agent pod
# resolves gb straight from BEADS_HTTP_URL/BEADS_AUTH_TOKEN; no viewer env needed.
gb_pool() {
  BEADS_HTTP_URL="$BEADS_HTTP_URL" \
  BEADS_AUTH_TOKEN="$BEADS_AUTH_TOKEN" \
  BEADS_OPERATED_BY="$OPERATOR" \
    "$GB_BIN" pool "$@"
}

# create_project <name> — create an ephemeral project bead and echo its id.
# Its title is the bare name (gb pool matches the project bead by title,
# stripping an optional "Project: " prefix).
#
# NOTE: the caller MUST register the returned id for cleanup via `track <id>`.
# We deliberately do NOT append to CREATED_BEADS in here: create_project is
# always invoked in `$(...)` command substitution, which runs in a SUBSHELL, so
# any array append here is discarded before the parent's cleanup trap runs. The
# id must be tracked in the parent shell (see `track`).
create_project() {
  local name="$1"
  # --no-parent: never graft an ephemeral test project onto whatever bead the
  # caller's session happens to hold (a claimed task auto-parents children).
  "$KD_BIN" create "$name" --type=project --no-parent \
    -d "Ephemeral project for gb pool RMW E2E (${RANDOM_SUFFIX})" \
    -l "e2e,${TITLE_PREFIX}-${RANDOM_SUFFIX}" --json 2>/dev/null \
    | jq -r '.id // empty'
}

# track <id> — register a created bead for the cleanup trap (parent shell).
# Always returns 0 so an empty id (create failure) does not abort under `set -e`
# before the caller's own graceful `fail` handling runs.
track() { [ -n "$1" ] && CREATED_BEADS+=("$1"); return 0; }

# pool_field <project-id> <key> — read one key out of the stored prewarmed_pool
# config on the project bead. Echoes empty when the field is unset.
#
# Shape note: `kd show --json` renders prewarmed_pool DIFFERENTLY depending on
# which writer last touched it — a raw `kd update -f prewarmed_pool={…}` write
# surfaces as a nested JSON OBJECT, while a `gb pool` write (UpdateBeadFields,
# which marshals to a string) surfaces as a JSON STRING. This helper normalizes
# both shapes before indexing, so a baseline read (post kd -f) and a post-`gb`
# read agree.
#
# The final branch guards on key PRESENCE (has($k)) rather than `$obj[$k] // ""`:
# `enabled` is a boolean and jq treats `false` as empty for the `//` operator
# (advice ka-0000006874), so a clean enabled=false would collapse to "". A
# present key echoes its value (false / 0 included); a missing key echoes "".
pool_field() {
  local id="$1" key="$2"
  "$KD_BIN" show "$id" --json 2>/dev/null \
    | jq -r --arg k "$key" '
        .fields.prewarmed_pool as $v
        | ( if $v == null then null
            elif ($v | type) == "string" then (if $v == "" then null else ($v | fromjson) end)
            else $v end ) as $obj
        | if $obj == null then "" elif ($obj | has($k)) then $obj[$k] else "" end
      ' 2>/dev/null
}

# assert_eq <name> <want> <got>
assert_eq() {
  local name="$1" want="$2" got="$3"
  if [ "$got" = "$want" ]; then
    pass "$name (= '$want')"
  else
    fail "$name" "want '$want', got '$got'"
  fi
}

# ══════════════════════════════════════════════════════════════════
# Scenario 1: disable preserves the existing role/min/max/mode
# ══════════════════════════════════════════════════════════════════
# Seed a fully-populated pool via a raw whole-object write (the pattern every
# existing pool e2e uses), then run `gb pool disable`. Only `enabled` may flip;
# role/min_size/max_size/mode must survive byte-for-byte — the RMW contract.
DISABLE_PROJECT="" DISABLE_ID=""
scenario_disable_preserves() {
  blue "Scenario 1: gb pool disable flips only enabled, preserves role/min/max/mode"

  DISABLE_PROJECT="${TITLE_PREFIX}-disable-${RANDOM_SUFFIX}"
  DISABLE_ID=$(create_project "$DISABLE_PROJECT")
  track "$DISABLE_ID"
  if [ -z "$DISABLE_ID" ]; then
    fail "create disable-scenario project" "kd create returned no id"
    return
  fi

  # Fully-populated pool, enabled=true, non-default sizes so a clobber is visible.
  "$KD_BIN" update "$DISABLE_ID" \
    -f 'prewarmed_pool={"enabled":true,"min_size":3,"max_size":7,"role":"crew-e2e","mode":"crew"}' \
    >/dev/null 2>&1 || { fail "seed disable pool config" "kd update -f failed"; return; }

  # Baseline: confirm the seed took, so the post-disable read is a genuine delta.
  assert_eq "baseline enabled=true"   "true"    "$(pool_field "$DISABLE_ID" enabled)"
  assert_eq "baseline role=crew-e2e"  "crew-e2e" "$(pool_field "$DISABLE_ID" role)"

  local out rc=0
  out=$(gb_pool disable --project "$DISABLE_PROJECT" 2>&1) || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "gb pool disable" "exited non-zero: $(head -3 <<<"$out")"
    return
  fi
  pass "gb pool disable succeeded"

  # ONLY enabled flipped; every other field survived.
  assert_eq "disable → enabled=false"   "false"    "$(pool_field "$DISABLE_ID" enabled)"
  assert_eq "disable preserves min_size" "3"       "$(pool_field "$DISABLE_ID" min_size)"
  assert_eq "disable preserves max_size" "7"       "$(pool_field "$DISABLE_ID" max_size)"
  assert_eq "disable preserves role"     "crew-e2e" "$(pool_field "$DISABLE_ID" role)"
  assert_eq "disable preserves mode"     "crew"    "$(pool_field "$DISABLE_ID" mode)"
}

# ══════════════════════════════════════════════════════════════════
# Scenario 2: enable preserves config (lossless round-trip)
# ══════════════════════════════════════════════════════════════════
# Continue from scenario 1's disabled-but-configured pool: `gb pool enable` must
# flip enabled back to true while every other field survives — re-enabling
# restores the SAME config, which is the whole point of preserving it on disable.
scenario_enable_preserves() {
  blue "Scenario 2: gb pool enable flips enabled back, config round-trips losslessly"

  if [ -z "$DISABLE_ID" ]; then
    skip "enable round-trip" "scenario 1 did not seed a project"
    return
  fi
  # Guard: we must be entering from the disabled state scenario 1 left behind.
  if [ "$(pool_field "$DISABLE_ID" enabled)" != "false" ]; then
    skip "enable round-trip" "pool not in disabled state at entry"
    return
  fi

  local out rc=0
  out=$(gb_pool enable --project "$DISABLE_PROJECT" 2>&1) || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "gb pool enable" "exited non-zero: $(head -3 <<<"$out")"
    return
  fi
  pass "gb pool enable succeeded"

  assert_eq "enable → enabled=true"      "true"    "$(pool_field "$DISABLE_ID" enabled)"
  assert_eq "enable preserves min_size"  "3"       "$(pool_field "$DISABLE_ID" min_size)"
  assert_eq "enable preserves max_size"  "7"       "$(pool_field "$DISABLE_ID" max_size)"
  assert_eq "enable preserves role"      "crew-e2e" "$(pool_field "$DISABLE_ID" role)"
  assert_eq "enable preserves mode"      "crew"    "$(pool_field "$DISABLE_ID" mode)"
}

# ══════════════════════════════════════════════════════════════════
# Scenario 3: brand-new pool → enable fills the K8s defaults
# ══════════════════════════════════════════════════════════════════
# A project with NO prewarmed_pool field: `gb pool enable` must fill the
# historical K8s defaults role=thread / min_size=2 / max_size=5 / mode=crew and
# set enabled=true (cmd/gb/pool.go:148-188). This is the "fill defaults on a
# brand-new pool" half of the task.
scenario_new_pool_defaults() {
  blue "Scenario 3: gb pool enable on a brand-new pool fills defaults (thread/2/5/crew)"

  local project id
  project="${TITLE_PREFIX}-new-${RANDOM_SUFFIX}"
  id=$(create_project "$project")
  track "$id"
  if [ -z "$id" ]; then
    fail "create new-pool project" "kd create returned no id"
    return
  fi

  # Precondition: the field must be unset before we enable.
  local before
  before=$("$KD_BIN" show "$id" --json 2>/dev/null | jq -r '.fields.prewarmed_pool // ""')
  if [ -n "$before" ]; then
    fail "new pool precondition" "prewarmed_pool unexpectedly present: $before"
    return
  fi
  pass "brand-new project has no prewarmed_pool field (baseline)"

  local out rc=0
  out=$(gb_pool enable --project "$project" 2>&1) || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "gb pool enable (new pool)" "exited non-zero: $(head -3 <<<"$out")"
    return
  fi
  pass "gb pool enable (new pool) succeeded"

  assert_eq "new pool enabled=true"    "true"   "$(pool_field "$id" enabled)"
  assert_eq "new pool default role"    "thread" "$(pool_field "$id" role)"
  assert_eq "new pool default min_size" "2"     "$(pool_field "$id" min_size)"
  assert_eq "new pool default max_size" "5"     "$(pool_field "$id" max_size)"
  assert_eq "new pool default mode"    "crew"   "$(pool_field "$id" mode)"
}

# ══════════════════════════════════════════════════════════════════
# Scenario 4: partial config → defaults fill ONLY the unset fields
# ══════════════════════════════════════════════════════════════════
# The sharpest RMW assertion: seed a PARTIAL config (role + max_size set;
# min_size + mode unset), run enable, and assert the SET fields survive while
# ONLY the unset ones take defaults. A whole-object write would have wiped role
# and max_size back to defaults — this proves the CLI does not.
scenario_partial_config() {
  blue "Scenario 4: enable fills only unset fields, never clobbers an explicit value"

  local project id
  project="${TITLE_PREFIX}-partial-${RANDOM_SUFFIX}"
  id=$(create_project "$project")
  track "$id"
  if [ -z "$id" ]; then
    fail "create partial-config project" "kd create returned no id"
    return
  fi

  # role + max_size explicitly set (non-default); min_size + mode omitted.
  "$KD_BIN" update "$id" \
    -f 'prewarmed_pool={"enabled":false,"role":"crew-e2e","max_size":9}' \
    >/dev/null 2>&1 || { fail "seed partial pool config" "kd update -f failed"; return; }

  local out rc=0
  out=$(gb_pool enable --project "$project" 2>&1) || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "gb pool enable (partial)" "exited non-zero: $(head -3 <<<"$out")"
    return
  fi
  pass "gb pool enable (partial config) succeeded"

  # Explicit values survive.
  assert_eq "partial preserves role"     "crew-e2e" "$(pool_field "$id" role)"
  assert_eq "partial preserves max_size" "9"        "$(pool_field "$id" max_size)"
  # Unset values take the K8s defaults.
  assert_eq "partial fills min_size=2"   "2"        "$(pool_field "$id" min_size)"
  assert_eq "partial fills mode=crew"    "crew"     "$(pool_field "$id" mode)"
  assert_eq "partial enabled=true"       "true"     "$(pool_field "$id" enabled)"
}

# ══════════════════════════════════════════════════════════════════
# Scenario 5: disable on a never-configured pool is a pure flag flip
# ══════════════════════════════════════════════════════════════════
# Unlike enable, disable does NOT fabricate defaults: on a project with no
# prewarmed_pool it writes enabled=false and leaves role/mode empty, sizes 0.
# This asymmetry is deliberate — disabling a pool should never invent a config.
scenario_disable_new_pool() {
  blue "Scenario 5: gb pool disable on a never-configured pool invents no defaults"

  local project id
  project="${TITLE_PREFIX}-disnew-${RANDOM_SUFFIX}"
  id=$(create_project "$project")
  track "$id"
  if [ -z "$id" ]; then
    fail "create disable-new project" "kd create returned no id"
    return
  fi

  local out rc=0
  out=$(gb_pool disable --project "$project" 2>&1) || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "gb pool disable (new pool)" "exited non-zero: $(head -3 <<<"$out")"
    return
  fi
  pass "gb pool disable (new pool) succeeded"

  assert_eq "disable-new enabled=false" "false" "$(pool_field "$id" enabled)"
  assert_eq "disable-new role empty"    ""      "$(pool_field "$id" role)"
  assert_eq "disable-new mode empty"    ""      "$(pool_field "$id" mode)"
  assert_eq "disable-new min_size=0"    "0"     "$(pool_field "$id" min_size)"
  assert_eq "disable-new max_size=0"    "0"     "$(pool_field "$id" max_size)"
}

# ── Main ──────────────────────────────────────────────────────────
main() {
  blue "╔══════════════════════════════════════════════════════╗"
  blue "║  gb pool enable/disable RMW e2e (ka-0000005192)       ║"
  blue "╚══════════════════════════════════════════════════════╝"

  setup_daemon
  if ! daemon_reachable; then
    skip "pool-enable-disable-rmw" "daemon unreachable or no BEADS_AUTH_TOKEN at ${BEADS_HTTP_URL:-unset}"
    echo ""; blue "Passed: $PASS  Skipped: $SKIP"
    return 0
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  scenario_disable_preserves
  scenario_enable_preserves
  scenario_new_pool_defaults
  scenario_partial_config
  scenario_disable_new_pool

  echo ""
  blue "────────────────────────────────────────────────────"
  green "Passed: $PASS"
  [ "$SKIP" -gt 0 ] && yellow "Skipped: $SKIP"
  if [ "$FAIL" -gt 0 ]; then
    red "Failed: $FAIL"
    for e in "${ERRORS[@]}"; do red "  ✗ $e"; done
  fi
  blue "────────────────────────────────────────────────────"
  [ "$FAIL" -eq 0 ]
}

main
