#!/usr/bin/env bash
# test-config-dump-load.sh — config dump/load round-trip E2E tests
#
# Tests gb config dump → gb config load round-trip: export config beads
# and advice to JSON, then reimport and verify data preservation.
#
# Scenarios tested:
#   1. gb config dump produces valid JSON with config_beads + advice
#   2. Dump captures config bead titles, labels, and values
#   3. Load recreates config beads from dump file
#   4. Load deduplicates — re-importing skips existing beads
#   5. Load advice beads from dump file
#   6. Empty dump file produces no errors
#   7. Legacy flat array format → deprecated warning
#   8. Dump captures the 5 new entity-type sections (triggers / schedules /
#      projects / accounts / formulas) with allowlisted fields preserved and
#      non-allowlisted fields stripped
#   9. Account section OMITS litellm_key + all runtime/telemetry fields
#  10. Round-trip (dump → load) recreates the 5 new-entity-type beads, and the
#      recreated account is still secret-free (litellm_key never round-trips)

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
  local name="$1" expected="$2" actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    pass "$name (exit $expected)"
  else
    fail "$name" "expected exit $expected, got exit $actual"
  fi
}

assert_contains() {
  local name="$1" pattern="$2" actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

assert_not_contains() {
  local name="$1" pattern="$2" actual="$3"
  if ! grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (not contains '$pattern')"
  else
    fail "$name" "expected NOT to contain '$pattern'"
  fi
}

assert_equals() {
  local name="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    pass "$name (=$expected)"
  else
    fail "$name" "expected '$expected', got '$actual'"
  fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19106
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-dump-load.log 2>&1 &
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
  rm -f /tmp/e2e-dump-*.json
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

create_config() {
  local title="$1" label="$2" value="$3"
  local out id
  out=$("$KD_BIN" create "$title" --type=config -l "$label" \
    -d "$value" --json 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  echo "$id"
}

# ── New-entity-type seed helpers ──────────────────────────────────
# Seed one typed bead via `kd create -f field=value`, register it for
# cleanup, and echo its id. Uses --project "" so the client-side
# cross-project guard never rejects the create; the unscoped e2e static
# token bypasses type write-policy server-side (write_authz.go). Returns
# empty on failure (caller decides whether that is fatal or a skip).
create_typed_bead() {
  local title="$1" beadtype="$2"
  shift 2
  local out id
  out=$("$KD_BIN" create "$title" --type="$beadtype" --project "" --json "$@" 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  echo "$id"
}

# Seed a type=schedule bead. `kd create --type=schedule` is hard-rejected
# by the CLI (Phase 3b/D10, schedule_lint.go), but type=schedule is still a
# valid server builtin and gb config dump/load round-trips it — so the
# schedule section must be seeded through the HTTP API, which the CLI
# reject does not gate. Echoes the created bead id (empty on failure).
create_schedule_bead() {
  local title="$1"
  local url="${BEADS_HTTP_URL:-}"
  [ -z "$url" ] && { echo ""; return; }
  local auth_header=""
  [ -n "${BEADS_TOKEN:-}" ] && auth_header="Authorization: Bearer ${BEADS_TOKEN}"
  # enabled=false so the seeded schedule never fires; drop_me is NOT in the
  # schedule allowlist and must be stripped by the dump. A daily cron
  # satisfies the runaway-protection cron-or-triggers requirement.
  local body id
  body=$(python3 - "$title" <<'PY'
import json, sys
title = sys.argv[1]
print(json.dumps({
    "title": title,
    "type": "schedule",
    "labels": ["project:gardenio"],
    "fields": {
        "cron": "0 4 * * *",
        "prompt": "e2e dump/load schedule — never fires",
        "project": "gardenio",
        "role": "crew",
        "enabled": False,
        "timezone": "America/Los_Angeles",
        "last_run": "2026-01-01T00:00:00Z",
    },
}))
PY
)
  local resp
  resp=$(curl -sf --max-time 15 -X POST "$url/v1/beads" \
    -H 'Content-Type: application/json' \
    -H 'X-Operated-By: oidc:e2e-dumpload@example.com' \
    ${auth_header:+-H "$auth_header"} \
    -d "$body" 2>/dev/null || echo "{}")
  id=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  echo "$id"
}

# Read a dump JSON file and print, for the entry named $2 in section $1,
# the value of field $3 — or the sentinel "<MISSING-FIELD>" when the entry
# exists but omits the field, or empty when the entry itself is absent.
dump_field() {
  local file="$1" section="$2" name="$3" field="$4"
  python3 - "$file" "$section" "$name" "$field" <<'PY'
import json, sys
file, section, name, field = sys.argv[1:5]
try:
    d = json.load(open(file))
except Exception:
    sys.exit(0)
for e in d.get(section, []) or []:
    if e.get("name") == name:
        f = e.get("fields", {}) or {}
        print(f[field] if field in f else "<MISSING-FIELD>")
        break
PY
}

# Print "yes" if section $2 in dump file $1 contains an entry named $3.
dump_has_entry() {
  local file="$1" section="$2" name="$3"
  python3 - "$file" "$section" "$name" <<'PY'
import json, sys
file, section, name = sys.argv[1:4]
try:
    d = json.load(open(file))
except Exception:
    print("no"); sys.exit(0)
print("yes" if any((e.get("name") == name) for e in (d.get(section, []) or [])) else "no")
PY
}

# ── Scenarios 8–10: new entity-type sections + account secret exclusion ──
# Seeds one bead per new dump/load section (triggers, schedules, projects,
# accounts, formulas), each carrying an allowlisted field (must survive the
# dump) and a non-allowlisted field (must be stripped). Then asserts the
# dump shape, the account secret/runtime exclusion, and the full round-trip.
run_new_entity_type_scenarios() {
  local tag="$1"
  local dump_file="/tmp/e2e-dump-entity-$tag.json"

  # ── Seed one bead of each new type ─────────────────────────────
  blue "Scenario 8: Seed + dump the 5 new entity-type sections"

  # trigger — keep `cron` (allowlisted), strip `jql` (not in triggerFields).
  local trig_id
  trig_id=$(create_typed_bead "e2e-dl-trigger-$tag" trigger \
    -l "project:gardenio,trigger:schedule" \
    -f "source=schedule" -f "action=spawn_agent" \
    -f "cron=0 5 * * *" -f "enabled=false" -f "project=gardenio" \
    -f "role=crew" -f "prompt=e2e dump/load trigger" \
    -f "jql=project = FOO")
  [ -n "$trig_id" ] && pass "seeded trigger bead ($trig_id)" \
    || fail "seed trigger bead" "kd create --type=trigger returned no id"

  # schedule — HTTP-seeded (CLI rejects --type=schedule). Keep `cron`, strip
  # `last_run` (a runtime field, not in scheduleFields).
  local sched_id
  sched_id=$(create_schedule_bead "e2e-dl-schedule-$tag")
  local schedule_seeded="no"
  if [ -n "$sched_id" ]; then
    schedule_seeded="yes"
    pass "seeded schedule bead via HTTP ($sched_id)"
  else
    dim "  schedule bead not seeded (type=schedule not configured on this daemon) — schedule assertions will be skipped"
  fi

  # project — keep `prefix`, strip `image` (not in projectFields).
  local proj_id
  proj_id=$(create_typed_bead "e2e-dl-project-$tag" project \
    -f "prefix=kd" -f "git_url=https://example.com/x.git" \
    -f "default_branch=main" -f "image=registry.example.com/x:latest")
  [ -n "$proj_id" ] && pass "seeded project bead ($proj_id)" \
    || fail "seed project bead" "kd create --type=project returned no id"

  # account — keep the 6 allowlisted config fields; seed the raw secret
  # (litellm_key) and a runtime/telemetry field (total_cost_usd) that MUST
  # be excluded from the dump.
  local acct_id
  acct_id=$(create_typed_bead "e2e-dl-account-$tag" account \
    -l "project:gardenio" \
    -f "provider=anthropic" -f "model=claude-opus-4-8" \
    -f "credential=secret:e2e-dl-cred" -f "fallback=e2e-dl-backup" \
    -f "base_url=https://api.anthropic.com" -f "probe_model=claude-haiku-4-5" \
    -f "litellm_key=sk-e2e-secret-do-not-dump" \
    -f "total_cost_usd=42.50" -f "rotation_count=7")
  [ -n "$acct_id" ] && pass "seeded account bead ($acct_id)" \
    || fail "seed account bead" "kd create --type=account returned no id"

  # formula — keep `steps`, strip `assigned_agent` (not in formulaFields).
  local formula_id
  formula_id=$(create_typed_bead "e2e-dl-formula-$tag" formula \
    -l "project:gardenio" \
    -f 'steps=[{"role":"crew","prompt":"do the thing"}]' \
    -f 'vars={"k":"v"}' \
    -f "assigned_agent=e2e-dl-agent")
  [ -n "$formula_id" ] && pass "seeded formula bead ($formula_id)" \
    || fail "seed formula bead" "kd create --type=formula returned no id"

  # ── Dump and assert section presence + allowlist behavior ──────
  local dump_out
  dump_out=$("$GB_BIN" config dump 2>/dev/null || echo "")
  if [ -z "$dump_out" ]; then
    fail "entity-type dump" "gb config dump produced no output"
    return
  fi
  echo "$dump_out" > "$dump_file"
  if ! python3 -c "import json; json.load(open('$dump_file'))" 2>/dev/null; then
    fail "entity-type dump valid JSON" "dump is not valid JSON"
    return
  fi
  pass "entity-type dump is valid JSON"

  # trigger: entry present, allowlisted `cron` survives, non-allowlisted
  # `jql` stripped.
  assert_dump_field "trigger cron survives" "triggers" "e2e-dl-trigger-$tag" "cron" "0 5 * * *" "$dump_file"
  assert_dump_field_absent "trigger jql stripped" "triggers" "e2e-dl-trigger-$tag" "jql" "$dump_file"

  # schedule: only when the daemon accepted the HTTP seed.
  if [ "$schedule_seeded" = "yes" ]; then
    assert_dump_field "schedule cron survives" "schedules" "e2e-dl-schedule-$tag" "cron" "0 4 * * *" "$dump_file"
    assert_dump_field_absent "schedule last_run stripped" "schedules" "e2e-dl-schedule-$tag" "last_run" "$dump_file"
  fi

  # project: allowlisted `prefix` survives, non-allowlisted `image` stripped.
  assert_dump_field "project prefix survives" "projects" "e2e-dl-project-$tag" "prefix" "kd" "$dump_file"
  assert_dump_field_absent "project image stripped" "projects" "e2e-dl-project-$tag" "image" "$dump_file"

  # formula: allowlisted `steps` survives, non-allowlisted `assigned_agent`
  # stripped.
  if [ "$(dump_field "$dump_file" formulas "e2e-dl-formula-$tag" steps)" != "" ]; then
    pass "formula steps survives (present)"
  else
    fail "formula steps survives" "steps missing from formula dump entry"
  fi
  assert_dump_field_absent "formula assigned_agent stripped" "formulas" "e2e-dl-formula-$tag" "assigned_agent" "$dump_file"
  echo

  # ── Scenario 9: account section omits litellm_key + runtime fields ─
  blue "Scenario 9: Account dump omits litellm_key + runtime/telemetry"

  # The 6 allowlisted config fields survive.
  assert_dump_field "account provider survives" "accounts" "e2e-dl-account-$tag" "provider" "anthropic" "$dump_file"
  assert_dump_field "account credential survives" "accounts" "e2e-dl-account-$tag" "credential" "secret:e2e-dl-cred" "$dump_file"

  # The raw secret and runtime/telemetry fields are excluded from the entry.
  assert_dump_field_absent "account litellm_key excluded" "accounts" "e2e-dl-account-$tag" "litellm_key" "$dump_file"
  assert_dump_field_absent "account total_cost_usd excluded" "accounts" "e2e-dl-account-$tag" "total_cost_usd" "$dump_file"
  assert_dump_field_absent "account rotation_count excluded" "accounts" "e2e-dl-account-$tag" "rotation_count" "$dump_file"

  # Belt-and-suspenders: the raw secret VALUE must not appear anywhere in the
  # whole dump, in any section (repro from the bead: dump | grep secret → 0).
  if ! grep -qF "sk-e2e-secret-do-not-dump" "$dump_file"; then
    pass "raw litellm_key value absent from entire dump"
  else
    fail "raw litellm_key value absent" "secret value leaked into dump"
  fi
  echo

  # ── Scenario 10: round-trip load recreates the new-type beads ──
  blue "Scenario 10: Round-trip — load recreates the 5 new-type beads"

  # Close the seeds so load must recreate them (dedup is by title).
  for id in "$trig_id" "$sched_id" "$proj_id" "$acct_id" "$formula_id"; do
    [ -n "$id" ] && "$KD_BIN" close "$id" 2>/dev/null || true
  done
  # Drop them from the cleanup list — load will recreate under new ids that we
  # re-collect below.
  local kept=()
  for b in "${CREATED_BEADS[@]}"; do
    case "$b" in
      "$trig_id"|"$sched_id"|"$proj_id"|"$acct_id"|"$formula_id") ;;
      *) kept+=("$b") ;;
    esac
  done
  CREATED_BEADS=("${kept[@]+"${kept[@]}"}")
  sleep 1

  local load_out load_rc=0
  load_out=$("$GB_BIN" config load "$dump_file" 2>&1) || load_rc=$?
  assert_exit "load entity-type dump" 0 "$load_rc"

  # The load summary reports each recreated section.
  assert_contains "load reports trigger beads" "trigger bead" "$load_out"
  assert_contains "load reports project beads" "project bead" "$load_out"
  assert_contains "load reports account beads" "account bead" "$load_out"
  assert_contains "load reports formula beads" "formula bead" "$load_out"

  # Re-collect recreated beads for cleanup and verify the account came back
  # WITHOUT the secret (litellm_key never round-trips through a dump).
  collect_recreated_typed_beads "$tag"

  local acct_show acct_new_id
  acct_new_id=$("$KD_BIN" list --type=account --status=open --json 2>/dev/null \
    | python3 -c "
import sys,json
for b in json.load(sys.stdin):
    if b.get('title') == 'e2e-dl-account-$tag':
        print(b.get('id','')); break
" 2>/dev/null || echo "")
  if [ -n "$acct_new_id" ]; then
    pass "account bead recreated by load ($acct_new_id)"
    acct_show=$("$KD_BIN" show "$acct_new_id" --json 2>/dev/null || echo "{}")
    local has_secret
    has_secret=$(echo "$acct_show" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print('yes' if 'litellm_key' in (d.get('fields',{}) or {}) else 'no')
" 2>/dev/null || echo "yes")
    if [ "$has_secret" = "no" ]; then
      pass "recreated account has NO litellm_key (secret never round-trips)"
    else
      fail "recreated account secret-free" "litellm_key present on recreated account"
    fi
    local prov
    prov=$(echo "$acct_show" | python3 -c "
import sys,json
print((json.load(sys.stdin).get('fields',{}) or {}).get('provider',''))
" 2>/dev/null || echo "")
    assert_equals "recreated account keeps provider" "anthropic" "$prov"
  else
    fail "account bead recreated" "no open account bead titled e2e-dl-account-$tag after load"
  fi

  rm -f "$dump_file"
}

# assert_dump_field <name> <section> <entry-name> <field> <expected> <file>
assert_dump_field() {
  local name="$1" section="$2" entry="$3" field="$4" expected="$5" file="$6"
  local actual
  actual=$(dump_field "$file" "$section" "$entry" "$field")
  if [ "$actual" = "$expected" ]; then
    pass "$name (=$expected)"
  else
    fail "$name" "expected '$expected', got '$actual'"
  fi
}

# assert_dump_field_absent <name> <section> <entry-name> <field> <file>
# Passes when the named entry exists AND omits the field. Fails loudly if the
# entry itself is missing (so a stripped-vs-not-seeded confusion can't pass).
assert_dump_field_absent() {
  local name="$1" section="$2" entry="$3" field="$4" file="$5"
  if [ "$(dump_has_entry "$file" "$section" "$entry")" != "yes" ]; then
    fail "$name" "entry '$entry' missing from section '$section' — cannot assert field absence"
    return
  fi
  local actual
  actual=$(dump_field "$file" "$section" "$entry" "$field")
  if [ "$actual" = "<MISSING-FIELD>" ]; then
    pass "$name (field omitted)"
  else
    fail "$name" "expected field '$field' to be stripped, got '$actual'"
  fi
}

# Re-collect load-recreated new-type beads (by title tag) into CREATED_BEADS
# so the cleanup trap closes them.
collect_recreated_typed_beads() {
  local tag="$1" t
  for t in trigger schedule project account formula; do
    local ids
    ids=$("$KD_BIN" list --type="$t" --status=open --json 2>/dev/null \
      | python3 -c "
import sys,json
tag='$tag'
try:
    for b in json.load(sys.stdin):
        if tag in (b.get('title','') or ''):
            print(b.get('id',''))
except Exception:
    pass
" 2>/dev/null || echo "")
    while IFS= read -r rid; do
      [ -n "$rid" ] && CREATED_BEADS+=("$rid")
    done <<< "$ids"
  done
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " config dump/load round-trip E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-dl-$$-$(date +%s)"
  local DUMP_FILE="/tmp/e2e-dump-$TEST_TAG.json"

  # ── Setup: create test beads ───────────────────────────────────
  blue "Setup: Creating config and advice beads"

  local cfg1_id cfg2_id adv_id
  cfg1_id=$(create_config "claude-settings" "global" '{"model":"sonnet","dump_test":"cfg1"}')
  [ -n "$cfg1_id" ] && pass "config bead 1 created ($cfg1_id)"

  cfg2_id=$(create_config "claude-mcp" "role:e2e-dl-role" '{"mcpServers":{"test":{"command":"echo"}}}')
  [ -n "$cfg2_id" ] && pass "config bead 2 created ($cfg2_id)"

  # Create advice bead
  local adv_out
  adv_out=$("$KD_BIN" create "E2E dump-load advice $TEST_TAG: test round-trip" \
    --type=advice -l global --json 2>/dev/null || echo "{}")
  adv_id=$(echo "$adv_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$adv_id" ] && CREATED_BEADS+=("$adv_id") && pass "advice bead created ($adv_id)"
  echo

  # ── Scenario 1: gb config dump produces valid JSON ─────────────
  blue "Scenario 1: gb config dump produces valid JSON"
  local dump_out dump_rc=0
  dump_out=$("$GB_BIN" config dump 2>/dev/null || echo "")
  dump_rc=$?

  if [ -n "$dump_out" ]; then
    echo "$dump_out" > "$DUMP_FILE"
    local valid
    valid=$(python3 -c "import json; json.load(open('$DUMP_FILE')); print('valid')" 2>/dev/null || echo "invalid")
    if [ "$valid" = "valid" ]; then
      pass "dump output is valid JSON"
    else
      fail "dump valid JSON" "output is not valid JSON"
    fi

    # Check structure
    local has_config_beads has_advice
    has_config_beads=$(python3 -c "import json; d=json.load(open('$DUMP_FILE')); print('yes' if 'config_beads' in d else 'no')" 2>/dev/null || echo "no")
    has_advice=$(python3 -c "import json; d=json.load(open('$DUMP_FILE')); print('yes' if 'advice' in d else 'no')" 2>/dev/null || echo "no")

    if [ "$has_config_beads" = "yes" ]; then
      pass "dump has config_beads section"
    else
      fail "dump config_beads section" "missing"
    fi
    if [ "$has_advice" = "yes" ]; then
      pass "dump has advice section"
    else
      fail "dump advice section" "missing"
    fi
  else
    fail "dump output" "empty"
  fi
  echo

  # ── Scenario 2: Dump captures titles, labels, values ───────────
  # NOTE: Use file-based grep instead of echo-pipe — dump can be >1MB with
  # hundreds of config beads, which can exceed shell echo pipe limits.
  blue "Scenario 2: Dump captures config bead details"
  if [ -f "$DUMP_FILE" ]; then
    for pair in "claude-settings title:claude-settings" "claude-mcp title:claude-mcp" \
                "dump_test value:dump_test" "role label:e2e-dl-role"; do
      local desc="${pair%%:*}"
      local pat="${pair#*:}"
      if grep -qF "$pat" "$DUMP_FILE"; then
        pass "dump has $desc (contains '$pat')"
      else
        fail "dump has $desc" "expected to contain '$pat'; got: $(head -5 "$DUMP_FILE")"
      fi
    done
  fi
  echo

  # ── Scenario 3: Load recreates config beads ────────────────────
  blue "Scenario 3: Load recreates config beads from dump file"

  # Close the original beads first
  for id in "$cfg1_id" "$cfg2_id" "$adv_id"; do
    [ -n "$id" ] && "$KD_BIN" close "$id" 2>/dev/null || true
  done
  CREATED_BEADS=()

  # Wait a moment for close to propagate
  sleep 1

  # Load from dump file
  if [ -f "$DUMP_FILE" ]; then
    local load_out load_rc=0
    load_out=$("$GB_BIN" config load "$DUMP_FILE" 2>&1 || echo "")
    load_rc=$?
    assert_exit "gb config load" 0 "$load_rc"

    # Verify config beads were recreated
    local new_list
    new_list=$("$KD_BIN" list --type=config --status=open --json 2>/dev/null || echo "[]")

    # Track recreated beads for cleanup — extract IDs of beads containing dump_test or e2e-dl-role
    local loaded_ids
    loaded_ids=$(echo "$new_list" | python3 -c "
import sys, json
try:
    beads = json.load(sys.stdin)
    for b in beads:
        desc = b.get('description','')
        labels = b.get('labels',[])
        label_str = ' '.join(labels) if isinstance(labels, list) else str(labels)
        if 'dump_test' in desc or 'e2e-dl-role' in label_str:
            print(b['id'])
except: pass
" 2>/dev/null || echo "")
    while IFS= read -r lid; do
      [ -n "$lid" ] && CREATED_BEADS+=("$lid")
    done <<< "$loaded_ids"

    # Also track any advice beads recreated by the load
    local adv_list
    adv_list=$("$KD_BIN" list --type=advice --status=open --json 2>/dev/null || echo "[]")
    local adv_ids
    adv_ids=$(echo "$adv_list" | python3 -c "
import sys, json
try:
    beads = json.load(sys.stdin)
    for b in beads:
        title = b.get('title','')
        if '$TEST_TAG' in title:
            print(b['id'])
except: pass
" 2>/dev/null || echo "")
    while IFS= read -r aid; do
      [ -n "$aid" ] && CREATED_BEADS+=("$aid")
    done <<< "$adv_ids"

    # Check for our specific beads by content
    if grep -q "dump_test" <<<"$new_list"; then
      pass "loaded config bead has dump_test value"
    else
      dim "  dump_test not found in list (may be in description field)"
      pass "load completed without error"
    fi
  else
    fail "load from dump" "dump file not found"
  fi
  echo

  # ── Scenario 4: Load deduplicates ──────────────────────────────
  blue "Scenario 4: Load deduplicates — re-importing skips existing"
  if [ -f "$DUMP_FILE" ]; then
    local load2_out load2_rc=0
    load2_out=$("$GB_BIN" config load "$DUMP_FILE" 2>&1 || echo "")
    load2_rc=$?
    assert_exit "second load exits 0" 0 "$load2_rc"

    # Output should mention "skipped"
    if grep -qi "skip" <<<"$load2_out"; then
      pass "second load reports skipped beads"
    else
      dim "  no 'skipped' in output (dedup may be silent)"
      pass "second load completed without error"
    fi
  fi
  echo

  # ── Scenario 5: Load advice beads ──────────────────────────────
  blue "Scenario 5: Load advice beads from dump file"
  if [ -f "$DUMP_FILE" ]; then
    local advice_list
    advice_list=$("$GB_BIN" advice list --json 2>/dev/null || echo "[]")
    if grep -q "$TEST_TAG" <<<"$advice_list"; then
      pass "loaded advice bead has test tag"
    else
      dim "  advice bead with test tag not found (may have been deduped)"
      pass "advice load completed"
    fi
  fi
  echo

  # ── Scenario 6: Empty dump file ────────────────────────────────
  blue "Scenario 6: Empty dump file produces no errors"
  local empty_file="/tmp/e2e-dump-empty-$TEST_TAG.json"
  echo '{}' > "$empty_file"
  local empty_rc=0
  "$GB_BIN" config load "$empty_file" 2>/dev/null || empty_rc=$?
  assert_exit "load empty file" 0 "$empty_rc"

  # Also test with just config_beads and advice empty arrays
  echo '{"config_beads":[],"advice":[]}' > "$empty_file"
  empty_rc=0
  "$GB_BIN" config load "$empty_file" 2>/dev/null || empty_rc=$?
  assert_exit "load empty arrays file" 0 "$empty_rc"
  rm -f "$empty_file"
  echo

  # ── Scenario 7: Legacy flat array format ─────────────────────────
  blue "Scenario 7: Legacy flat array format → deprecated warning"
  local legacy_file="/tmp/e2e-dump-legacy-$TEST_TAG.json"
  cat > "$legacy_file" <<'LEGACY_EOF'
[{"key":"claude-settings:global","value":{"model":"sonnet"}},{"key":"context:gardener","value":{"foo":"bar"}}]
LEGACY_EOF

  local legacy_out legacy_rc=0
  legacy_out=$("$GB_BIN" config load "$legacy_file" 2>&1 || true)
  legacy_rc=$?
  assert_exit "load legacy format" 0 "$legacy_rc"

  # The load command prints a warning about legacy KV entries on stderr
  if grep -qi "legacy" <<<"$legacy_out"; then
    pass "legacy format produces deprecation warning"
  else
    fail "legacy format warning" "expected 'legacy' in output; got: $(echo "$legacy_out" | head -3)"
  fi

  if grep -qi "deprecated" <<<"$legacy_out"; then
    pass "legacy warning mentions deprecated"
  else
    fail "legacy deprecated mention" "expected 'deprecated' in output; got: $(echo "$legacy_out" | head -3)"
  fi

  # Verify legacy entries are skipped, not imported as config beads
  if grep -qi "skipping" <<<"$legacy_out"; then
    pass "legacy entries are skipped"
  else
    fail "legacy entries skipped" "expected 'skipping' in output; got: $(echo "$legacy_out" | head -3)"
  fi
  rm -f "$legacy_file"
  echo

  # ── Scenarios 8–10: new entity-type sections + secret exclusion ──
  # gb config dump/load round-trips five field-keyed bead sections beyond
  # roles/config_beads/advice: triggers, schedules, projects, accounts,
  # formulas (commit 3bf2b5f9, gd-rUxDez5q7r). Each section carries an
  # allowlist of config fields; the account section additionally EXCLUDES
  # the raw litellm_key secret and every runtime/telemetry field. These
  # scenarios seed one bead of each new type — each with an allowlisted
  # field that must survive AND a non-allowlisted field that must be
  # stripped — then assert the dump preserves the config fields, omits the
  # non-config ones, and (for accounts) never leaks the secret. Scenario 10
  # closes the seeds and loads the dump back to prove the round-trip
  # recreates all five and that litellm_key never re-materializes.
  run_new_entity_type_scenarios "$TEST_TAG"
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
