#!/usr/bin/env bash
# test-role-skill-toggle.sh — Capability B (role-bead skill toggle) E2E
#
# Epic ka-0000004989 / case E2E-2. Proves the kd/gb role-bead round-trip
# (enabled_skills / disabled_skills persisted comma-joined and re-read) drives
# `gb setup claude` to build a {workspace}/.claude/skills tree that reflects the
# toggle, while agents/ and commands/ are untouched.
#
# What it asserts (a role with an ALLOW-LIST + a DENY entry):
#   role: --enabled-skills deploy,onboarding  --disabled-skills video
#   scaffold: primary skills {deploy,onboarding,video}, sibling {sib-deploy,sib-extra}
#             primary agents {deploy(file)}, primary commands {run(file)}
#   expected linked skills == {deploy, onboarding}
#     - video: dropped (not in allow-list AND explicitly denied)
#     - sib-deploy: dropped (namespaced name "sib-deploy" NOT in allow-list)
#     - sib-extra:  dropped (namespaced name "sib-extra"  NOT in allow-list)
#     NOTE: a non-empty allow-list links ONLY the named skills (matched on the
#     LINKED/namespaced name), so the sibling skills are excluded here. This is
#     the documented B4/INT-3 semantics; the sibling would link only if the
#     allow-list named "sib-deploy"/"sib-extra" explicitly.
#   agents == {deploy}, commands == {run}  (skill filter never touches these)
#   log (2>&1): STDOUT "skipping skills video (role skill toggle)"
#               STDERR "[setup] role skill toggle: N enabled-allow, M disabled"
#
# Harness notes: identical to test-multirepo-skill-discovery.sh (ADDENDUM #1/#2/#3).
# The bot root is the hardcoded /home/agent/bot/{BOAT_PROJECT}; a throwaway
# BOAT_PROJECT keeps it isolated; XFAIL cleanly if the source mkdir is denied.

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

SUFFIX="$$-${RANDOM}"
PROJ="e2e-skilltog-${SUFFIX}"
ROLE="e2e-skilltog-role-${SUFFIX}"
BOT_ROOT="/home/agent/bot/${PROJ}"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  red "  FAIL: $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}
xfail() { blue "  XFAIL: $1"; dim "    $2"; }

# assert linked-name set equals exactly the expected set
assert_set_eq() {
  # assert_set_eq <name> <listfile> <space-separated-expected>
  local name="$1" f="$2"; shift 2
  local want; want=$(printf '%s\n' "$@" | sort | tr '\n' ' ')
  local got; got=$(sort "$f" | tr '\n' ' ')
  if [ "$got" = "$want" ]; then
    pass "$name (== [$want])"
  else
    fail "$name" "got [$got], want [$want]"
  fi
}

assert_log() {
  # assert_log <name> <needle> <logfile>
  if grep -qF -- "$2" "$3"; then
    pass "$1 (log has '$2')"
  else
    fail "$1" "log missing '$2'; tail: $(tail -3 "$3" | tr '\n' ' ')"
  fi
}

setup_daemon() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"; return 0; fi
  if "$KD_BIN" list --limit 1 --json >/dev/null 2>&1; then dim "  daemon reachable (no port-forward)"; return 0; fi
  command -v kubectl >/dev/null 2>&1 || return 1
  blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC}..."
  local port=19132
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" >/tmp/kd-pf-skilltog.log 2>&1 &
  PF_PID=$!; sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  "$KD_BIN" list --limit 1 --json >/dev/null 2>&1
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then export BEADS_TOKEN="$secret"; fi
}

cleanup() {
  for id in "${CREATED_BEADS[@]:-}"; do
    [ -n "$id" ] && "$KD_BIN" close "$id" 2>/dev/null || true
  done
  [ -d "${DEST_DIR:-}" ] && rm -rf "$DEST_DIR"
  [ -d "${BOT_ROOT:-/nonexistent}" ] && rm -rf "$BOT_ROOT" 2>/dev/null || true
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

create_config() {
  local out id
  out=$("$KD_BIN" create "$1" --type=config -l "$2" -d "$3" --json 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  echo "$id"
}

mk_skill() { mkdir -p "$1/.claude/$2/$3"; echo x > "$1/.claude/$2/$3/SKILL.md"; }
mk_file()  { mkdir -p "$1/.claude/$2"; echo x > "$1/.claude/$2/$3"; }

main() {
  blue "═══════════════════════════════════════════════════"
  blue " E2E-2: role-bead skill toggle (gb role scaffold + setup claude)"
  blue "═══════════════════════════════════════════════════"

  if ! setup_daemon; then
    xfail "daemon unreachable" "no BEADS_HTTP_URL, in-pod URL down, kubectl port-forward unavailable"
    exit 0
  fi
  setup_token
  green "  daemon reachable"

  if ! mkdir -p "$BOT_ROOT/work" "$BOT_ROOT/repos/sib" 2>/dev/null; then
    xfail "cannot scaffold $BOT_ROOT" "/home/agent/bot not writable in this environment"
    exit 0
  fi

  # primary skills + agents + commands; sibling "sib" skills
  mk_skill "$BOT_ROOT/work" skills deploy
  mk_skill "$BOT_ROOT/work" skills onboarding
  mk_skill "$BOT_ROOT/work" skills video
  mk_file  "$BOT_ROOT/work" agents deploy
  mk_file  "$BOT_ROOT/work" commands run
  mk_skill "$BOT_ROOT/repos/sib" skills deploy   # -> sib-deploy
  mk_skill "$BOT_ROOT/repos/sib" skills extra     # -> sib-extra

  # ── Scaffold a role bead WITH the toggle fields ──
  blue "Scaffolding role $ROLE (--enabled-skills deploy,onboarding --disabled-skills video)"
  local scaffold_out
  scaffold_out=$(BOAT_PROJECT="$PROJ" "$GB_BIN" role scaffold "$ROLE" --no-instructions \
    --enabled-skills deploy,onboarding --disabled-skills video --json 2>&1 || true)
  dim "  $(echo "$scaffold_out" | tail -2)"

  # Locate + track the created role bead, and verify the round-trip fields.
  local role_bead
  role_bead=$("$KD_BIN" list --type=role --status=open --all-projects --json 2>/dev/null \
    | python3 -c "
import sys,json
d=json.load(sys.stdin); bs=d if isinstance(d,list) else d.get('beads',d.get('issues',[]))
print(next((b['id'] for b in bs if b.get('title')=='${ROLE}'),''))
" 2>/dev/null || echo "")
  if [ -z "$role_bead" ]; then
    fail "role bead created" "no type:role bead titled $ROLE found"
  else
    CREATED_BEADS+=("$role_bead")
    pass "role bead created ($role_bead)"
    # Round-trip: fields persisted comma-joined and re-readable.
    local fields
    fields=$("$KD_BIN" show "$role_bead" --json 2>/dev/null | python3 -c "
import sys,json
d=json.load(sys.stdin)
f=d.get('fields',{}) or {}
print('enabled='+str(f.get('enabled_skills','')))
print('disabled='+str(f.get('disabled_skills','')))
" 2>/dev/null || echo "")
    dim "  $(echo "$fields" | tr '\n' ' ')"
    if grep -q "enabled=deploy,onboarding" <<<"$fields"; then
      pass "round-trip enabled_skills == 'deploy,onboarding'"
    else
      fail "round-trip enabled_skills" "got: $(echo "$fields" | grep enabled=)"
    fi
    if grep -q "disabled=video" <<<"$fields"; then
      pass "round-trip disabled_skills == 'video'"
    else
      fail "round-trip disabled_skills" "got: $(echo "$fields" | grep disabled=)"
    fi
  fi

  # ── Seed a role-scoped hooks bead so setup reaches the symlink step ──
  local h_id
  h_id=$(create_config "claude-hooks" "role:${ROLE}" \
    '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"gb hook prime 2>/dev/null || true"}]}]}}')
  [ -n "$h_id" ] && dim "  claude-hooks bead: $h_id"

  # ── Run gb setup claude, capturing STDOUT+STDERR (ADDENDUM #3) ──
  DEST_DIR=$(mktemp -d /tmp/e2e-skilltog-dst-XXXXXX)
  local iso_home; iso_home=$(mktemp -d /tmp/e2e-skilltog-home-XXXXXX); mkdir -p "$iso_home/.claude"
  local logf; logf=$(mktemp /tmp/e2e-skilltog-log-XXXXXX)

  blue "Running: gb setup claude --role $ROLE --workspace <dst> (BOAT_PROJECT=$PROJ)"
  local rc=0
  HOME="$iso_home" BOAT_PROJECT="$PROJ" "$GB_BIN" setup claude \
    --role "$ROLE" --workspace "$DEST_DIR" > "$logf" 2>&1 || rc=$?
  rm -rf "$iso_home"
  if [ "$rc" -eq 0 ]; then pass "gb setup claude exit 0"; else
    fail "gb setup claude exit 0" "exit $rc; tail: $(tail -5 "$logf" | tr '\n' ' ')"
  fi

  # ── Assert the toggle log lines (STDOUT skip + STDERR summary, merged) ──
  assert_log "skip line for denied 'video'" "skipping skills video (role skill toggle)" "$logf"
  assert_log "toggle summary emitted"       "[setup] role skill toggle:" "$logf"

  # ── Walk the tree ──
  local cd_="$DEST_DIR/.claude"
  local sk ag cm
  sk=$(mktemp); ag=$(mktemp); cm=$(mktemp)
  ls "$cd_/skills"   2>/dev/null | sort > "$sk" || true
  ls "$cd_/agents"   2>/dev/null | sort > "$ag" || true
  ls "$cd_/commands" 2>/dev/null | sort > "$cm" || true
  dim "  skills:   $(tr '\n' ' ' < "$sk")"
  dim "  agents:   $(tr '\n' ' ' < "$ag")"
  dim "  commands: $(tr '\n' ' ' < "$cm")"

  # allow-list links ONLY named skills, on the LINKED name (B4/INT-3):
  #   deploy, onboarding kept; video denied; sib-deploy/sib-extra not in allow-list.
  assert_set_eq "skills == {deploy,onboarding}" "$sk" deploy onboarding
  # agents/commands untouched by the skill filter
  assert_set_eq "agents == {deploy}"  "$ag" deploy
  assert_set_eq "commands == {run}"   "$cm" run

  rm -f "$sk" "$ag" "$cm" "$logf"
  echo

  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " All $total assertions passed"
  else
    red " $FAIL/$total assertions failed"
    for err in "${ERRORS[@]}"; do red "   - $err"; done
  fi
  blue "═══════════════════════════════════════════════════"
  [ "$FAIL" -eq 0 ]
}

main "$@"
