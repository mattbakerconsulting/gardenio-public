#!/usr/bin/env bash
# lib-local-runner.sh — shared preamble for the local runner e2e scripts
# (local-agents spec §11 scenarios a, b, c, e, f; plan gd-fwFefkDP4H Task 18,
# gd-jW7KJ7ZdNy):
#
#   test-local-runner-mail.sh        (a)      pod agent: nudge, decision, kill as mail
#   test-local-runner-lifecycle.sh   (b + e)  register, join, claim, squawk; away/resume
#   test-local-runner-escalation.sh  (c + d)  cloud-to-local escalation; d is M2, skipped
#   test-local-runner-listener.sh    (f)      public runner listener allowlist
#
# The skeleton is test-local-mode.sh's: BEADS_OPERATED_BY default, pf-supervisor
# port-forward, kd-beads-token, pass/fail/skip counters, CREATED_BEADS cleanup,
# LOG_ARCHIVE_DIR. On top of it: a runner registered from a real invite into a
# private GARDENIO_HOME, `gb runner join` with a claudeless --command, and
# field waits.
#
# Usage (from a test script, after `set -euo pipefail`):
#   # shellcheck source=SCRIPTDIR/lib-local-runner.sh
#   source "$(dirname "${BASH_SOURCE[0]}")/lib-local-runner.sh"
#   lr_init "<suite title>"            # binaries, daemon, token; skips without them
#   ...
#   lr_summary                         # prints PASS/FAIL/SKIP, exits non-zero on a fail

# shellcheck disable=SC2317
if [ -n "${__GARDENIO_LIB_LOCAL_RUNNER_LOADED:-}" ]; then
  return 0 2>/dev/null || true
fi
__GARDENIO_LIB_LOCAL_RUNNER_LOADED=1

# Since ka-0000002601 the daemon wants a human operator shape; CI's run-shard.sh
# exports a per-shard oidc fixture, this is the laptop fallback.
export BEADS_OPERATED_BY="${BEADS_OPERATED_BY:-oidc:e2e-local-runner@example.com}"

LR_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib-kd-create.sh
source "$LR_LIB_DIR/lib-kd-create.sh"

SCRIPT_DIR="$LR_LIB_DIR"
# Used by the scripts that source this file.
# shellcheck disable=SC2034
SCENARIO_DIR="$SCRIPT_DIR/../claudeless"
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
E2E_PROJECT="${E2E_PROJECT:-gardenio}"
# The runner's owner is the invite's invitee. Escalation approval (c) must come
# from this identity, so set it to the person whose approver token you pass.
RUNNER_OWNER="${E2E_RUNNER_OWNER:-$BEADS_OPERATED_BY}"
TEST_TAG="lr-$$-$(date +%s)"
CREATED_BEADS=()
PASS=0
FAIL=0
SKIP=0
ERRORS=()
PF_PORT=""
PF_SENTINEL=""
# Set by lr_register_runner / lr_join.
RUNNER_HOME=""
RUNNER_ID=""
INVITE_CODE=""
JOIN_PID=""
JOIN_LOG=""
AGENT_NAME=""
AGENT_BEAD=""

LOG_ARCHIVE_DIR="${LOG_ARCHIVE_DIR:-}"
if [ -n "$LOG_ARCHIVE_DIR" ]; then
  mkdir -p "$LOG_ARCHIVE_DIR" || LOG_ARCHIVE_DIR=""
fi

# ── Output and counters ───────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  PASS $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  FAIL $name"
  [ -n "$reason" ] && dim "    $(printf '%s' "$reason" | head -c 1500)"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name")
}
skip() {
  local name="$1" reason="${2:-}"
  dim "  SKIP $name${reason:+ ($reason)}"
  SKIP=$((SKIP + 1))
}

have() { command -v "$1" >/dev/null 2>&1; }

# file_mode <path> — octal permission bits (GNU stat, then BSD stat).
file_mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1" 2>/dev/null || echo missing; }

# utc_in_one_hour — RFC 3339 an hour from now (GNU date, then BSD date).
utc_in_one_hour() { date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v+1H +%Y-%m-%dT%H:%M:%SZ; }

# skip_all <reason> — the whole script cannot run here. Writes the run-shard
# skip sentinel first so a self-skip is not counted as a pass (ka-0000000989).
skip_all() {
  dim "SKIP: $*"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

# lr_summary — print the PASS/FAIL/SKIP summary and exit non-zero on any fail.
lr_summary() {
  echo
  blue "═══════════════════════════════════════════════════"
  blue "  Results: $PASS passed, $FAIL failed, $SKIP skipped"
  if [ "$FAIL" -gt 0 ]; then
    local e
    for e in "${ERRORS[@]}"; do red "   - $e"; done
  fi
  blue "═══════════════════════════════════════════════════"
  [ "$FAIL" -eq 0 ] || exit 1
  exit 0
}

# ── Bead reads ────────────────────────────────────────────────────
bead_json() { "$KD_BIN" show "$1" --json 2>/dev/null || true; }

# bead_field <id> <field> — a custom field ("" when unset or unreadable).
bead_field() {
  bead_json "$1" | jq -r --arg k "$2" '.fields[$k] // empty | if type == "string" then . else tojson end' 2>/dev/null || true
}

# bead_attr <id> <jq-path> — a top-level attribute, e.g. .status or .assignee.
bead_attr() {
  bead_json "$1" | jq -r "$2 // empty" 2>/dev/null || true
}

# wait_for_jq <id> <jq-predicate> <secs> — poll kd show until the predicate
# holds. Returns 0 on match, 1 on timeout.
wait_for_jq() {
  local id="$1" pred="$2" timeout="${3:-60}" start=$SECONDS
  while [ $((SECONDS - start)) -lt "$timeout" ]; do
    if bead_json "$id" | jq -e "$pred" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
}

wait_for_status() { wait_for_jq "$1" ".status == \"$2\"" "${3:-60}"; }

# wait_for_field <id> <field> <value> <secs>
wait_for_field() { wait_for_jq "$1" ".fields[\"$2\"] == \"$3\"" "${4:-60}"; }

# wait_for_log <log> <fixed-string> <secs> [pid] — stop early when pid exits.
wait_for_log() {
  local log="$1" needle="$2" timeout="${3:-60}" pid="${4:-}" start=$SECONDS
  while [ $((SECONDS - start)) -lt "$timeout" ]; do
    if grep -qF -- "$needle" "$log" 2>/dev/null; then
      return 0
    fi
    if [ -n "$pid" ] && ! kill -0 "$pid" 2>/dev/null; then
      grep -qF -- "$needle" "$log" 2>/dev/null
      return $?
    fi
    sleep 1
  done
  return 1
}

# dump_log <name> <log> — tail the log inline and archive the whole file.
dump_log() {
  local name="$1" log="$2"
  [ -s "$log" ] || { dim "    (log $log empty or missing)"; return 0; }
  dim "  --- $log (last 80 lines) ---"
  tail -n 80 "$log" | sed 's/^/      /'
  if [ -n "$LOG_ARCHIVE_DIR" ] && [ -d "$LOG_ARCHIVE_DIR" ]; then
    cp "$log" "$LOG_ARCHIVE_DIR/${name}.log" 2>/dev/null || true
  fi
}

# kd_new <kd create args...> — create a bead, record it for cleanup, print its id.
kd_new() {
  local id
  id="$(kd_create_id "$KD_BIN" create "$@" --no-parent)"
  if [ -n "$id" ]; then
    CREATED_BEADS+=("$id")
  fi
  printf '%s' "$id"
}

# ── Daemon reachability (test-local-mode.sh's pattern) ────────────
port_forward_alive() {
  [ -n "$PF_PORT" ] || return 1
  ( exec 3<>/dev/tcp/127.0.0.1/"$PF_PORT" ) >/dev/null 2>&1
}

setup_portforward() {
  if have kubectl && kubectl -n "$NAMESPACE" get "svc/${BEADS_SVC}" >/dev/null 2>&1; then
    PF_PORT="${LR_PF_PORT:-19098}"
    PF_SENTINEL="/tmp/kd-pf-local-runner-${TEST_TAG}.sentinel"
    if have setsid; then
      setsid -f bash "$SCRIPT_DIR/pf-supervisor.sh" "$NAMESPACE" "$BEADS_SVC" "$PF_PORT" \
        "/tmp/kd-pf-local-runner-${TEST_TAG}.log" "$PF_SENTINEL"
    else
      bash "$SCRIPT_DIR/pf-supervisor.sh" "$NAMESPACE" "$BEADS_SVC" "$PF_PORT" \
        "/tmp/kd-pf-local-runner-${TEST_TAG}.log" "$PF_SENTINEL" &
      disown 2>/dev/null || true
    fi
    local start=$SECONDS
    while [ $((SECONDS - start)) -lt 15 ]; do
      if port_forward_alive; then
        export BEADS_HTTP_URL="http://localhost:${PF_PORT}"
        dim "  Daemon at $BEADS_HTTP_URL (port-forward)"
        return 0
      fi
      sleep 1
    done
    red "  kubectl port-forward did not come up on :${PF_PORT}"
    PF_PORT=""
  fi
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return 0
  fi
  skip_all "BEADS_HTTP_URL not set and the ${NAMESPACE}/${BEADS_SVC} service is not reachable"
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ] || ! have kubectl; then return 0; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  fi
}

# lr_init <title> [extra required binaries...]
lr_init() {
  local title="$1"; shift
  blue "═══════════════════════════════════════════════════"
  blue "  $title"
  blue "═══════════════════════════════════════════════════"
  local missing=() bin
  for bin in kd gb jq "$@"; do
    have "$bin" || missing+=("$bin")
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    skip_all "missing required binaries: ${missing[*]}"
  fi
  setup_portforward
  setup_token
  if ! "$KD_BIN" list --status=open --limit 1 --json >/dev/null 2>&1; then
    red "ERROR: cannot reach the beads daemon at ${BEADS_HTTP_URL:-?}"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo
}

# ── Runner registration and join ──────────────────────────────────

# lr_register_runner [daemon-url] — mint a single-use invite for RUNNER_OWNER
# (type=runner_invite is admin-gated: without an admin token the create is
# refused and this returns 1, so the caller skips), then redeem it with
# `gb runner register` into a fresh GARDENIO_HOME. Sets RUNNER_HOME and
# RUNNER_ID, and INVITE_CODE for a replay check. The code is never printed.
lr_register_runner() {
  local daemon_url="${1:-$BEADS_HTTP_URL}" hash invite out
  INVITE_CODE="GRI-E2E-$(printf '%s' "$TEST_TAG" | tr '[:lower:]' '[:upper:]' | tr -c 'A-Z0-9\n' '-')"
  hash="$(printf '%s' "$INVITE_CODE" | sha256sum 2>/dev/null || printf '%s' "$INVITE_CODE" | shasum -a 256)"
  hash="${hash%% *}"
  invite="$(kd_new "e2e runner invite $TEST_TAG" --type=runner_invite \
    --short-title "e2e runner invite" \
    -l "project:$E2E_PROJECT" \
    -f "code_hash=$hash" \
    -f "invitee=$RUNNER_OWNER" \
    -f "projects=[\"$E2E_PROJECT\"]" \
    -f "expires_at=$(utc_in_one_hour)" 2>/dev/null)"
  if [ -z "$invite" ]; then
    return 1
  fi
  RUNNER_HOME="$(mktemp -d -t gardenio-home-XXXXXX)"
  chmod 700 "$RUNNER_HOME"
  if ! out="$(GARDENIO_HOME="$RUNNER_HOME" "$GB_BIN" runner register \
      --invite "$INVITE_CODE" --daemon "$daemon_url" --tags e2e 2>&1)"; then
    fail "gb runner register redeems the invite" "$out"
    return 2
  fi
  RUNNER_ID="$(jq -r '.runner_id // empty' "$RUNNER_HOME/runner.json" 2>/dev/null || true)"
  if [ -z "$RUNNER_ID" ]; then
    fail "gb runner register writes runner.json" "$out"
    return 2
  fi
  CREATED_BEADS+=("$RUNNER_ID")
  pass "gb runner register redeemed the invite: runner $RUNNER_ID"
  return 0
}

# lr_join <agent-name> <scenario-toml> — `gb runner join` in its own process
# group (so lr_pause can stop the whole tree, as a closed lid does) with
# claudeless as the command. Sets JOIN_PID, JOIN_LOG, AGENT_NAME and, once the
# banner shows, AGENT_BEAD. Returns 1 when the agent did not come up.
lr_join() {
  AGENT_NAME="$1"
  local toml="$2"
  JOIN_LOG="/tmp/test-local-runner-${TEST_TAG}-${AGENT_NAME}.log"
  # The agent's --dir gets a .state dir; keep it out of the checkout.
  mkdir -p "$RUNNER_HOME/work"
  # setsid without job control execs in place, so $! is the group leader.
  # Without setsid (macOS) lr_pause stops only gb runner join.
  local launcher=()
  if have setsid; then launcher=(setsid); fi
  GARDENIO_HOME="$RUNNER_HOME" "${launcher[@]}" "$GB_BIN" runner join \
    --project "$E2E_PROJECT" \
    --dir "$RUNNER_HOME/work" \
    --name "$AGENT_NAME" \
    --command "claudeless --scenario $toml --dangerously-skip-permissions" \
    </dev/null >"$JOIN_LOG" 2>&1 &
  JOIN_PID=$!
  if ! wait_for_log "$JOIN_LOG" "Agent bead:" 90 "$JOIN_PID"; then
    fail "gb runner join registers the agent" "no 'Agent bead:' banner"
    dump_log "join-$AGENT_NAME" "$JOIN_LOG"
    return 1
  fi
  AGENT_BEAD="$(grep -oE 'Agent bead:[[:space:]]+(gd|ka)-[A-Za-z0-9]+' "$JOIN_LOG" | head -n1 | awk '{print $NF}')"
  CREATED_BEADS+=("$AGENT_BEAD")
  if ! wait_for_log "$JOIN_LOG" "Mail subscriber started" 60 "$JOIN_PID"; then
    fail "the joined agent subscribes to its mail" "no 'Mail subscriber started' banner"
    dump_log "join-$AGENT_NAME" "$JOIN_LOG"
    return 1
  fi
  pass "gb runner join started $AGENT_NAME ($AGENT_BEAD) and subscribed to its mail"
  return 0
}

# lr_pause / lr_resume — SIGSTOP/SIGCONT the whole join process group: gb's
# runner heartbeat, the agent, coop and claudeless stop together, as on a
# closed lid.
lr_pause()  { kill -STOP -- "-$JOIN_PID" 2>/dev/null || kill -STOP "$JOIN_PID"; }
lr_resume() { kill -CONT -- "-$JOIN_PID" 2>/dev/null || kill -CONT "$JOIN_PID"; }

# lr_leave — `gb runner leave`, then wait for join to exit (TERM, then KILL).
lr_leave() {
  [ -n "$RUNNER_HOME" ] || return 0
  GARDENIO_HOME="$RUNNER_HOME" timeout 60 "$GB_BIN" runner leave >/dev/null 2>&1 || true
  if [ -n "$JOIN_PID" ]; then
    local start=$SECONDS
    while kill -0 "$JOIN_PID" 2>/dev/null; do
      if [ $((SECONDS - start)) -ge 45 ]; then
        kill -TERM -- "-$JOIN_PID" 2>/dev/null || true
        sleep 5
        kill -KILL -- "-$JOIN_PID" 2>/dev/null || true
        break
      fi
      sleep 1
    done
    wait "$JOIN_PID" 2>/dev/null || true
  fi
}

# mail_send <recipient> <subject> <body> [gb mail send flags...] — print the id
# on stdout, and nothing else: callers capture it with $(...), so gb's error
# goes to stderr and a refused send prints "" (gd-yTZ6lbS6uf).
mail_send() {
  local to="$1" subject="$2" body="$3" out id
  shift 3
  out="$("$GB_BIN" mail send "$to" -s "$subject" -b "$body" "$@" --json 2>&1 || true)"
  id="$(printf '%s' "$out" | jq -r '.id // empty' 2>/dev/null || true)"
  if [ -n "$id" ]; then
    CREATED_BEADS+=("$id")
  else
    dim "    gb mail send: $(printf '%s' "$out" | head -c 300)" >&2
  fi
  printf '%s' "$id"
}

lr_cleanup() {
  set +e
  if [ -n "$JOIN_PID" ] && kill -0 "$JOIN_PID" 2>/dev/null; then
    kill -CONT -- "-$JOIN_PID" 2>/dev/null
    lr_leave
  fi
  if [ -n "$JOIN_LOG" ] && [ -n "$LOG_ARCHIVE_DIR" ] && [ -d "$LOG_ARCHIVE_DIR" ]; then
    cp "$JOIN_LOG" "$LOG_ARCHIVE_DIR/" 2>/dev/null
  fi
  local id
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" >/dev/null 2>&1
  done
  [ -n "$RUNNER_HOME" ] && rm -rf "$RUNNER_HOME"
  if [ -n "$PF_SENTINEL" ] && [ -f "$PF_SENTINEL" ]; then
    rm -f "$PF_SENTINEL"
    sleep 2
  fi
}
