#!/usr/bin/env bash
# test-multirepo-skill-discovery.sh — Capability A (multi-repo skill discovery) E2E
#
# Epic ka-0000004989 / case E2E-1. Proves `gb setup claude` builds the full merged
# {workspace}/.claude/{agents,skills,commands} tree from a scaffolded multi-repo bot
# root, exercising the HARDCODED /home/agent/bot/{BOAT_PROJECT}/{work,repos} source
# path (which the internal unit helpers cannot reach — they take explicit dirs).
#
# What it asserts (allow-all / no-toggle role, isolating Capability A):
#   - primary repo entries link under BARE names
#   - sibling repo entries link under NAMESPACED "{repoBase}-{entry}" names
#   - primary wins on a bare-name collision (readlink resolves under work/)
#   - a sibling entry resolves under repos/{repo}/
#   - a subdir that NO repo ships is NOT created (no empty dir left behind)
#   - all three subdirs (agents, skills, commands) are each merged
#
# Harness notes (see plan ADDENDUM):
#   #1 sources live under the hardcoded /home/agent/bot/{PROJ} tree; --workspace
#      only sets the DESTINATION. /home/agent/bot is world-writable in the agent
#      env, so we mkdir the source tree ourselves. If that mkdir is denied we
#      XFAIL/skip cleanly (exit 0) rather than hard-fail.
#   #2 `gb setup claude` (no --defaults) runs the full pipeline: it needs the
#      daemon reachable AND a claude-hooks config bead, else it errors before the
#      symlink step. We seed a ROLE-SCOPED claude-hooks bead for a unique
#      throwaway role so the run is isolated and self-cleaning.
#
# A throwaway BOAT_PROJECT keeps the scaffold away from any real workspace.

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

# Unique, throwaway identifiers so this test is side-effect isolated on any
# daemon (including a shared one) and self-cleaning.
SUFFIX="$$-${RANDOM}"
PROJ="e2e-skilldisc-${SUFFIX}"
ROLE="e2e-skilldisc-role-${SUFFIX}"
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

# XFAIL: record an expected/environmental skip and exit 0 (does not fail the run).
xfail() {
  blue "  XFAIL: $1"
  dim "    $2"
}

assert_has() {
  # assert_has <name> <needle> <listfile>
  if grep -qxF -- "$2" "$3"; then
    pass "$1 (has '$2')"
  else
    fail "$1" "expected entry '$2' in: $(tr '\n' ' ' < "$3")"
  fi
}

assert_absent() {
  if ! grep -qxF -- "$2" "$3"; then
    pass "$1 (no '$2')"
  else
    fail "$1" "entry '$2' unexpectedly present"
  fi
}

# ── Daemon reachability (in-pod OR port-forward) ──────────────────
setup_daemon() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"; return 0
  fi
  # Already reachable via the in-pod default URL? No port-forward needed.
  if "$KD_BIN" list --limit 1 --json >/dev/null 2>&1; then
    dim "  daemon already reachable (no port-forward)"; return 0
  fi
  if ! command -v kubectl >/dev/null 2>&1; then
    return 1
  fi
  blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC}..."
  local port=19131
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-skilldisc.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  "$KD_BIN" list --limit 1 --json >/dev/null 2>&1
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
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
  # create_config <title> <label> <json-value> -> prints bead id
  local out id
  out=$("$KD_BIN" create "$1" --type=config -l "$2" -d "$3" --json 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  [ -n "$id" ] && CREATED_BEADS+=("$id")
  echo "$id"
}

# mkdir a skill (directory-shaped extension) or a file (agents/commands).
mk_skill() { mkdir -p "$1/.claude/$2/$3"; echo x > "$1/.claude/$2/$3/SKILL.md"; }
mk_file()  { mkdir -p "$1/.claude/$2"; echo x > "$1/.claude/$2/$3"; }

main() {
  blue "═══════════════════════════════════════════════════"
  blue " E2E-1: multi-repo skill discovery (gb setup claude)"
  blue "═══════════════════════════════════════════════════"

  if ! setup_daemon; then
    xfail "daemon unreachable" "no BEADS_HTTP_URL, in-pod URL down, and kubectl port-forward unavailable"
    exit 0
  fi
  setup_token
  green "  daemon reachable"

  # ── Scaffold the hardcoded bot root (ADDENDUM #1: gate on mkdir) ──
  if ! mkdir -p "$BOT_ROOT/work" "$BOT_ROOT/repos" 2>/dev/null; then
    xfail "cannot scaffold $BOT_ROOT" "/home/agent/bot not writable in this environment"
    exit 0
  fi

  # primary (work): skills {onboarding,video}, agents reviewer.md — NO commands.
  mk_skill "$BOT_ROOT/work" skills onboarding
  mk_skill "$BOT_ROOT/work" skills video
  mk_file  "$BOT_ROOT/work" agents reviewer.md
  # sibling helm: skills {deploy,onboarding(collides)}, agents reviewer.md(collides)
  mk_skill "$BOT_ROOT/repos/helm" skills deploy
  mk_skill "$BOT_ROOT/repos/helm" skills onboarding
  mk_file  "$BOT_ROOT/repos/helm" agents reviewer.md
  # sibling coop: skills credmgr
  mk_skill "$BOT_ROOT/repos/coop" skills credmgr
  # NOTE: no repo ships a "commands" subdir -> dst/.claude/commands must be absent.

  # ── Seed a role-scoped claude-hooks bead + a plain (no-toggle) role ──
  # gb setup claude errors with "no claude-hooks config found" if nothing
  # resolves; a role-scoped bead keeps the run isolated to our throwaway role.
  local h_id
  h_id=$(create_config "claude-hooks" "role:${ROLE}" \
    '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"gb hook prime 2>/dev/null || true"}]}]}}')
  [ -n "$h_id" ] && dim "  claude-hooks bead: $h_id"

  # Plain role: no --enabled-skills/--disabled-skills => allow-all, no toggle.
  # IMPORTANT: only ever track for cleanup the role bead whose title EXACTLY
  # matches our unique throwaway $ROLE — never scrape arbitrary kd- IDs out of
  # command output, which would close unrelated production beads on cleanup.
  BOAT_PROJECT="$PROJ" "$GB_BIN" role scaffold "$ROLE" --no-instructions --json >/dev/null 2>&1 || true
  local role_bead
  role_bead=$("$KD_BIN" list --type=role --status=open --all-projects --json 2>/dev/null \
    | python3 -c "
import sys,json
d=json.load(sys.stdin); bs=d if isinstance(d,list) else d.get('beads',d.get('issues',[]))
print(next((b['id'] for b in bs if b.get('title')=='${ROLE}'),''))
" 2>/dev/null || echo "")
  if [ -n "$role_bead" ]; then CREATED_BEADS+=("$role_bead"); dim "  role bead: $role_bead"; fi

  # ── Run gb setup claude into an isolated destination workspace ──
  DEST_DIR=$(mktemp -d /tmp/e2e-skilldisc-dst-XXXXXX)
  local iso_home
  iso_home=$(mktemp -d /tmp/e2e-skilldisc-home-XXXXXX); mkdir -p "$iso_home/.claude"

  blue "Running: gb setup claude --role $ROLE --workspace <dst>  (BOAT_PROJECT=$PROJ)"
  local out rc=0
  out=$(HOME="$iso_home" BOAT_PROJECT="$PROJ" "$GB_BIN" setup claude \
    --role "$ROLE" --workspace "$DEST_DIR" 2>&1) || rc=$?
  rm -rf "$iso_home"
  if [ "$rc" -ne 0 ]; then
    fail "gb setup claude exit 0" "exit $rc; output: $(echo "$out" | tail -5)"
    # continue to summary
  else
    pass "gb setup claude exit 0"
  fi
  if grep -q "role skill toggle" <<<"$out"; then
    fail "no-toggle role emits no toggle summary" "unexpected toggle summary for a plain role"
  else
    pass "no toggle summary for plain role"
  fi

  # ── Walk the merged tree ──
  local cd_="$DEST_DIR/.claude"
  local sk ag
  sk=$(mktemp); ag=$(mktemp)
  ls "$cd_/skills" 2>/dev/null | sort > "$sk" || true
  ls "$cd_/agents" 2>/dev/null | sort > "$ag" || true
  dim "  skills: $(tr '\n' ' ' < "$sk")"
  dim "  agents: $(tr '\n' ' ' < "$ag")"

  # skills: primary bare + sibling namespaced
  assert_has    "skills primary bare (onboarding)"  "onboarding"     "$sk"
  assert_has    "skills primary bare (video)"       "video"          "$sk"
  assert_has    "skills sibling namespaced (helm-deploy)" "helm-deploy" "$sk"
  assert_has    "skills sibling collision namespaced (helm-onboarding)" "helm-onboarding" "$sk"
  assert_has    "skills sibling namespaced (coop-credmgr)" "coop-credmgr" "$sk"

  # PRIMARY WINS: bare colliding name resolves under work/
  local tgt
  tgt=$(readlink -f "$cd_/skills/onboarding" 2>/dev/null || echo "")
  if [[ "$tgt" == "$BOT_ROOT/work/"* ]]; then
    pass "primary wins: bare 'onboarding' resolves under work/"
  else
    fail "primary wins" "onboarding -> '$tgt', want under $BOT_ROOT/work/"
  fi
  # sibling entry resolves under repos/helm/
  tgt=$(readlink -f "$cd_/skills/helm-deploy" 2>/dev/null || echo "")
  if [[ "$tgt" == "$BOT_ROOT/repos/helm/"* ]]; then
    pass "sibling 'helm-deploy' resolves under repos/helm/"
  else
    fail "sibling resolve" "helm-deploy -> '$tgt', want under $BOT_ROOT/repos/helm/"
  fi

  # agents: primary bare + sibling namespaced (all three subdirs merged)
  assert_has    "agents primary bare (reviewer.md)"    "reviewer.md"      "$ag"
  assert_has    "agents sibling namespaced (helm-reviewer.md)" "helm-reviewer.md" "$ag"

  # no repo ships commands -> the subdir must NOT be created
  if [ ! -e "$cd_/commands" ]; then
    pass "no empty 'commands' subdir (no repo ships one)"
  else
    fail "empty subdir guard" "commands/ was created despite no source shipping it"
  fi

  rm -f "$sk" "$ag"
  echo

  # ── Summary ─────────────────────────────────────────────────────
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
