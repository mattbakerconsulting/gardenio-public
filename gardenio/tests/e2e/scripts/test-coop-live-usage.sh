#!/usr/bin/env bash
# test-coop-live-usage.sh — E2E for coop's live-usage seam: the dedicated
# Prometheus metrics port (:9093 in gardenio pods) and the /api/v1/agent
# live-usage fields the controller's statusreporter mirrors onto agent beads
# (ka-0000005996, parent epic ka-0000000958; commits f8d40ae, 598cba8;
# design beads ka-0000005703, gd-e7FiGP33Q2).
#
# WHY THIS EXISTS: NOTHING in the e2e suite scraped coop's metrics port. The
# freeze/metrics modules scrape the CONTROLLER's :9092 (test-controller-metrics)
# and kbeads' :9091 (test-fleet-metrics); `grep -rln "tool_call_latency\|
# llm_request_latency" gardenio/tests/e2e/` returned nothing at all. The two
# apparent ":9093" hits elsewhere in the suite (test-account-auth.sh,
# test-beads-crud.sh) are a RED HERRING — both are `local port=19093`, an
# unrelated local forward port, not coop live-usage.
#
# It also closes the OTHER half of the hole. test-agent-cost-context.sh SEEDS
# `context_tokens=128000 live_cost_usd=0.42` by hand via kd and asserts the
# type=agent schema persists + renders them — a real assertion about the READ
# path, but it never proves anything COMPUTES those values. This module drives
# the producer end: a live coop session whose own usage accounting populates
# /api/v1/agent, which is exactly the field set statusreporter's reportLiveUsage
# reads (controller/internal/statusreporter/reporter.go — coopAgentResponse's
# context_tokens / cost_usd / request_count) before mirroring it onto the bead.
# Together the two modules span the seam end to end.
#
# Scenarios:
#   1. The metrics port is a SEPARATE, UNAUTHENTICATED, /metrics-only router.
#      With --auth-token set: GET /metrics on the metrics port is 200 with no
#      bearer, while GET /api/v1/agent on the main port is 401 without one and
#      200 with it, and /api/v1/agent on the METRICS port is 404. That is the
#      whole point of the dedicated port — Prometheus scrapes it without
#      holding the API token (config.rs: "an unauthenticated `/metrics`-only
#      router … without the bearer token required on the main API port").
#   2. Both live-usage histograms are REGISTERED and well-formed before any
#      work: correct HELP/TYPE lines, the documented 12 bucket bounds plus the
#      mandatory +Inf, and _sum/_count present. A histogram that vanishes from
#      the exposition is a silent dashboard outage.
#   3. BOTH latency histograms are OBSERVED, with EXACT counts. Driving 3 tool
#      calls then 1 more yields tool-call _count 3 then 4 — not merely "> 0" —
#      the assertion that would have caught ka-0000004093, where a restricted
#      hook matcher left the histogram registered-but-dead fleet-wide. The
#      llm_request histogram, now that ka-0000005556 is fixed, observes EXACTLY
#      session_request_count samples (one per deduped LLM request; every request
#      with a predecessor to gap against is observed, including the near-zero-gap
#      startup ack). For both, the +Inf bucket must equal _count and _sum must
#      be > 0 — the non-ack requests carry real multi-millisecond gaps (turn
#      boundaries are seconds apart, within-turn fanout ~10ms), so the summed
#      latency is comfortably positive even though the ack sample itself is ~0.
#   4. Session token/request counters go non-zero off the same session, and
#      carry the documented agent_name/project/account label set.
#   5. GET /api/v1/agent exposes the live-usage triple statusreporter consumes
#      (context_tokens / cost_usd / request_count) with the right JSON types,
#      and context_tokens + request_count are non-zero after real turns.
#
# NOW-DRIVEN (was a fixture limit; kept as a note so the history is legible):
#   * gardenio_agent_llm_request_latency_seconds IS driven by value as of
#     ka-0000005556 / MR !6631. coop derives per-response LLM latency from
#     session-log entry timestamp gaps (usage.rs LlmLatencyTracker, the
#     ka-0000004093 fix, because interactive sessions carry no durationMs).
#     Its parser `parse_rfc3339_ms` USED to open with `strip_suffix('Z')?`,
#     REQUIRING a trailing Z; claudeless writes "…+00:00", so every entry
#     parsed to None and the histogram observed nothing (measured while first
#     writing this module: tool_call _count=4 but llm_request _count=0). The
#     fix splits the zone designator and normalizes any numeric ±HH:MM offset
#     to UTC, so offset-form transcripts now drive the histogram. Scenario 3
#     asserts an EXACT count of session_request_count (see ka-0000004208: the
#     count was == session_request_count-1 until coop stopped silently dropping
#     the requests whose ms-truncated gap floored to zero — that -1 was a flaky
#     fixture artifact, not a real invariant).
#
# KNOWN FIXTURE LIMITS — asserted as shape, never as value, and WHY:
#   * /api/v1/agent cost_usd is always 0.0 here. extract_usage_delta reads the
#     per-turn cost from a top-level `response_cost` / `_litellm_response_cost`
#     key that only a real LiteLLM-proxied request stamps; claudeless emits
#     neither. Asserted as "present and numeric", not "> 0".
#   * Token magnitudes are not controllable — claudeless ignores the scenario's
#     `usage` numbers in interactive PTY mode. Asserted "> 0".
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-coop-live-usage.sh
#
# Skipped (cleanly, exit 0) when coop, claudeless, python3, curl or jq are not
# on PATH — same convention as test-coop-local-mode.sh / test-claudeless.sh, so
# this file is safe to wire into CI before every shard ships those binaries.
#
# No daemon or cluster connection is required: coop is launched against
# claudeless and the test only talks to 127.0.0.1.

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCENARIO_DIR="$SCRIPT_DIR/../claudeless"
SCENARIO="$SCENARIO_DIR/coop-live-usage.toml"
COOP_BIN="${COOP_BIN:-coop}"
CLAUDELESS_BIN="${CLAUDELESS_BIN:-claudeless}"

# The bearer we hand coop's main API port, to prove /metrics is exempt from it.
COOP_TEST_TOKEN="coop-live-usage-e2e-token"

# Label values we export so the exposition's label set is assertable. coop reads
# these from BOAT_AGENT / BOAT_PROJECT at scrape time (transport/http/metrics.rs).
EXPECT_AGENT="coop-live-usage-e2e"
EXPECT_PROJECT="gardenio"

# Tool calls the scenario issues, per turn. Keep in lockstep with
# claudeless/coop-live-usage.toml: block 2 = 3 calls, block 3 = 1 call.
TOOLS_TURN1=3
TOOLS_TURN2=1

# The documented bucket bounds (usage.rs LATENCY_BUCKETS_SECONDS). Rendered via
# Rust f64 Display, so whole numbers lose their ".0" — le="1" not le="1.0".
EXPECT_BUCKETS=(0.01 0.05 0.1 0.25 0.5 1 2.5 5 10 30 60 120)

PASS=0
FAIL=0
SKIP=0
ERRORS=()

COOP_PID=""
COOP_LOG=""
COOP_PORT=""
METRICS_PORT=""
CFG_DIR=""
METRICS_FILE=""

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
skip_all() { yellow "SKIP: $1"; if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$1" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi; exit 0; }  # skip visibility (ka-0000000989)

have() { command -v "$1" >/dev/null 2>&1; }

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

# scrape [outfile] — GET the metrics port with NO auth header (the contract).
scrape() {
  local out="${1:-$METRICS_FILE}"
  curl -sS --max-time 8 -o "$out" "http://127.0.0.1:${METRICS_PORT}/metrics" 2>/dev/null
}

# metric_value <file> <metric_name_with_suffix> — echo the value of the first
# matching sample line, or "" when the series is absent.
#
# Grep a FILE, never `printf '%s' "$body" | grep …`: under `set -o pipefail` a
# short-circuiting `grep -q` SIGPIPEs the writer and the pipeline reports 141,
# turning a genuine match into a false miss.
metric_value() {
  local file="$1" name="$2"
  awk -v n="$name" '
    index($0, n) == 1 && substr($0, length(n)+1, 1) == "{" { print $NF; exit }
  ' "$file"
}

# http_code <url> [curl args...]
http_code() {
  local url="$1"; shift
  curl -sS --max-time 8 -o /dev/null -w '%{http_code}' "$@" "$url" 2>/dev/null || echo "000"
}

agent_json() {
  curl -sS --max-time 8 -H "Authorization: Bearer ${COOP_TEST_TOKEN}" \
    "http://127.0.0.1:${COOP_PORT}/api/v1/agent" 2>/dev/null
}

# nudge <message> — deliver a turn over the authenticated main API port.
nudge() {
  curl -sS --max-time 10 -X POST \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer ${COOP_TEST_TOKEN}" \
    -d "{\"message\": $(python3 -c 'import json,sys;print(json.dumps(sys.argv[1]))' "$1")}" \
    "http://127.0.0.1:${COOP_PORT}/api/v1/agent/nudge" 2>/dev/null
}

# wait_for_tool_count <want> <timeout_s> — poll the histogram until _count
# reaches want. Hook events arrive over a FIFO and are folded in asynchronously,
# so a fixed sleep would be racy; poll and echo the last value seen.
wait_for_tool_count() {
  local want="$1" timeout="${2:-25}" start=$SECONDS got=""
  while [ $((SECONDS - start)) -lt "$timeout" ]; do
    scrape
    got="$(metric_value "$METRICS_FILE" 'gardenio_agent_tool_call_latency_seconds_count')"
    if [ -n "$got" ] && [ "${got%.*}" -ge "$want" ] 2>/dev/null; then
      echo "$got"; return 0
    fi
    sleep 0.5
  done
  echo "${got:-}"
  return 1
}

# wait_for_llm_count <timeout_s> — poll until the llm_request histogram count
# settles at session_request_count and echo "<llm_count> <request_count>".
# Both counters are derived by the async session-log watcher, so a fixed sleep
# would be racy: poll until session_request_count is STABLE across two scrapes
# AND llm_count == request_count (the settled relationship — one latency
# observation per deduped LLM request; see the EXACT-count rationale at the
# assertion below for why it is == and no longer == req-1). Reading both from
# the SAME scrape file keeps them consistent for the caller's follow-up
# _sum / +Inf reads.
wait_for_llm_count() {
  local timeout="${1:-30}" start=$SECONDS llm="" req="" prev_req="-1"
  while [ $((SECONDS - start)) -lt "$timeout" ]; do
    scrape
    req="$(metric_value "$METRICS_FILE" 'gardenio_session_request_count_total')"; req="${req%.*}"
    llm="$(metric_value "$METRICS_FILE" 'gardenio_agent_llm_request_latency_seconds_count')"; llm="${llm%.*}"
    if [ -n "$req" ] && [ -n "$llm" ] && [ "$req" -ge 2 ] 2>/dev/null \
       && [ "$req" = "$prev_req" ] && [ "$llm" = "$req" ]; then
      echo "$llm $req"; return 0
    fi
    prev_req="$req"
    sleep 0.5
  done
  echo "${llm:-} ${req:-}"
  return 1
}

start_coop() {
  COOP_PORT="$(free_port)"
  METRICS_PORT="$(free_port)"
  COOP_LOG="$(mktemp -t coop-live-usage-XXXXXX.log)"
  METRICS_FILE="$(mktemp -t coop-metrics-XXXXXX.prom)"

  # CLAUDE_CONFIG_DIR is LOAD-BEARING, not hygiene. coop's Tier-2 log detector
  # discovers the transcript under the Claude config dir; without this set to a
  # writable, stable location claudeless scatters its log somewhere discovery
  # never finds, and every gardenio_session_* counter silently stays 0 while the
  # tool-call histogram (Tier-1 FIFO, log-independent) still works — a
  # confusing half-dead state. Verified while writing this module.
  CFG_DIR="$(mktemp -d -t coop-live-usage-cfg-XXXXXX)"

  blue "Starting coop (api :$COOP_PORT, metrics :$METRICS_PORT)"
  dim "  log: $COOP_LOG"
  dim "  CLAUDE_CONFIG_DIR: $CFG_DIR"

  # Run with a SCRUBBED environment. In a real agent pod the inherited COOP_*
  # vars (COOP_S3_*, COOP_MUX_*, COOP_HOOK_PIPE, COOP_URL…) point at THIS pod's
  # own live session: coop would attempt an S3 resume, restore a foreign
  # transcript and exit immediately. `env -u` each one so the test session is
  # self-contained wherever it runs.
  local -a unset_args=()
  local v
  for v in $(env | grep -o '^COOP_[A-Z0-9_]*' | sort -u); do
    unset_args+=("-u" "$v")
  done
  # CLAUDECODE / CLAUDE_CODE_* likewise leak the harness's own session identity.
  for v in CLAUDECODE COOP CLAUDE_CODE_SESSION_ID CLAUDE_CODE_ENTRYPOINT; do
    unset_args+=("-u" "$v")
  done

  env "${unset_args[@]}" \
    CLAUDE_CONFIG_DIR="$CFG_DIR" \
    BOAT_AGENT="$EXPECT_AGENT" \
    BOAT_PROJECT="$EXPECT_PROJECT" \
    "$COOP_BIN" \
      --host 127.0.0.1 \
      --port "$COOP_PORT" \
      --port-metrics "$METRICS_PORT" \
      --auth-token "$COOP_TEST_TOKEN" \
      --cols 100 \
      --rows 30 \
      -- \
      "$CLAUDELESS_BIN" --scenario "$SCENARIO" "coop live-usage e2e" \
      >"$COOP_LOG" 2>&1 &
  COOP_PID=$!

  local start=$SECONDS
  while [ $((SECONDS - start)) -lt 20 ]; do
    if curl -sf --max-time 1 "http://127.0.0.1:${COOP_PORT}/api/v1/livez" >/dev/null 2>&1; then
      dim "  coop ready (pid $COOP_PID)"
      return 0
    fi
    if ! kill -0 "$COOP_PID" 2>/dev/null; then
      red "coop exited prematurely. Last 30 lines of log:"
      tail -n 30 "$COOP_LOG" | sed 's/^/    /'
      return 1
    fi
    sleep 0.2
  done
  red "Timed out waiting for coop /api/v1/livez. Last 30 lines of log:"
  tail -n 30 "$COOP_LOG" | sed 's/^/    /'
  return 1
}

cleanup() {
  if [ -n "${COOP_PID:-}" ] && kill -0 "$COOP_PID" 2>/dev/null; then
    kill -TERM "$COOP_PID" 2>/dev/null || true
    for _ in 1 2 3 4 5; do
      kill -0 "$COOP_PID" 2>/dev/null || break
      sleep 0.4
    done
    kill -KILL "$COOP_PID" 2>/dev/null || true
    wait "$COOP_PID" 2>/dev/null || true
  fi
  [ -n "${CFG_DIR:-}" ] && rm -rf "$CFG_DIR" 2>/dev/null || true
  rm -f "${COOP_LOG:-}" "${METRICS_FILE:-}" 2>/dev/null || true
}
trap cleanup EXIT

# ── Pre-flight ────────────────────────────────────────────────────
blue "═══════════════════════════════════════════════════"
blue "  coop live-usage / :9093 metrics seam E2E (ka-0000005996)"
blue "═══════════════════════════════════════════════════"
echo

missing=()
for bin in "$COOP_BIN" "$CLAUDELESS_BIN" python3 curl jq; do
  have "$bin" || missing+=("$bin")
done
if [ "${#missing[@]}" -gt 0 ]; then
  skip_all "missing required binaries: ${missing[*]}"
fi

if [ ! -f "$SCENARIO" ]; then
  fail "scenario fixture present" "$SCENARIO not found"
  exit 1
fi
pass "scenario fixture present"

if ! start_coop; then
  red "coop did not start; cannot run live-usage tests"
  exit 1
fi
echo

# ── Scenario 1: the metrics port's auth contract ───────────────────
blue "Scenario 1: dedicated metrics port is unauthenticated and /metrics-only"
code="$(http_code "http://127.0.0.1:${METRICS_PORT}/metrics")"
if [ "$code" = "200" ]; then
  pass "GET :metrics/metrics with NO bearer → 200 (Prometheus needs no token)"
else
  fail "GET :metrics/metrics with NO bearer → 200" "got $code"
fi

code="$(http_code "http://127.0.0.1:${COOP_PORT}/api/v1/agent")"
if [ "$code" = "401" ]; then
  pass "GET :api/api/v1/agent with NO bearer → 401 (main port IS gated)"
else
  fail "GET :api/api/v1/agent with NO bearer → 401" "got $code"
fi

code="$(http_code "http://127.0.0.1:${COOP_PORT}/api/v1/agent" \
  -H "Authorization: Bearer ${COOP_TEST_TOKEN}")"
if [ "$code" = "200" ]; then
  pass "GET :api/api/v1/agent WITH bearer → 200"
else
  fail "GET :api/api/v1/agent WITH bearer → 200" "got $code"
fi

# The metrics router exposes ONLY /metrics — the API surface must not be
# reachable on an unauthenticated port.
code="$(http_code "http://127.0.0.1:${METRICS_PORT}/api/v1/agent")"
if [ "$code" = "404" ]; then
  pass "GET :metrics/api/v1/agent → 404 (API not exposed on the open port)"
else
  fail "GET :metrics/api/v1/agent → 404" "got $code — API leaking on the unauthenticated port"
fi
echo

# ── Scenario 2: both histograms registered + well-formed ───────────
blue "Scenario 2: both live-usage histograms are registered and well-formed"
scrape
dim "  scraped $(wc -l < "$METRICS_FILE") exposition lines"
for m in gardenio_agent_tool_call_latency_seconds gardenio_agent_llm_request_latency_seconds; do
  if grep -qF "# TYPE ${m} histogram" "$METRICS_FILE"; then
    pass "TYPE line present: $m"
  else
    fail "TYPE line present: $m" "missing '# TYPE ${m} histogram'"
  fi
  if grep -qF "# HELP ${m} " "$METRICS_FILE"; then
    pass "HELP line present: $m"
  else
    fail "HELP line present: $m" "missing HELP line"
  fi
  # Every documented bound, plus the mandatory +Inf.
  missing_b=()
  for b in "${EXPECT_BUCKETS[@]}" '+Inf'; do
    grep -qF "${m}_bucket{" "$METRICS_FILE" \
      && grep -F "${m}_bucket{" "$METRICS_FILE" | grep -qF "le=\"${b}\"" \
      || missing_b+=("$b")
  done
  if [ "${#missing_b[@]}" -eq 0 ]; then
    pass "all ${#EXPECT_BUCKETS[@]} documented buckets + Inf present: $m"
  else
    fail "all documented buckets + Inf present: $m" "missing le=: ${missing_b[*]}"
  fi
  for suffix in _sum _count; do
    if grep -qF "${m}${suffix}{" "$METRICS_FILE"; then
      pass "${suffix#_} series present: $m"
    else
      fail "${suffix#_} series present: $m" "missing ${m}${suffix}"
    fi
  done
done
echo

# ── Scenario 3: tool-call latency observed, EXACT count ────────────
blue "Scenario 3: tool-call latency histogram observes an EXACT count"
before="$(metric_value "$METRICS_FILE" 'gardenio_agent_tool_call_latency_seconds_count')"
dim "  count before any nudge: ${before:-<absent>}"

resp="$(nudge 'run three tools')"
if [ "$(printf '%s' "$resp" | jq -r '.delivered // empty' 2>/dev/null)" = "true" ]; then
  pass "turn 1 nudge delivered (${TOOLS_TURN1} tool calls)"
else
  fail "turn 1 nudge delivered" "response: $resp"
fi

want=$((before + TOOLS_TURN1))
got="$(wait_for_tool_count "$want" 30 || true)"
if [ "${got%.*}" = "$want" ] 2>/dev/null; then
  pass "tool-call count is EXACTLY $want after $TOOLS_TURN1 calls (not merely > 0)"
else
  fail "tool-call count is EXACTLY $want after $TOOLS_TURN1 calls" \
    "got '${got:-<absent>}' — the hook ToolBefore→ToolAfter pairing under-/over-counted"
fi

# _sum must reflect real elapsed wall-clock, and +Inf must equal _count.
sum="$(metric_value "$METRICS_FILE" 'gardenio_agent_tool_call_latency_seconds_sum')"
if [ -n "$sum" ] && python3 -c "import sys; sys.exit(0 if float(sys.argv[1]) > 0 else 1)" "$sum" 2>/dev/null; then
  pass "tool-call _sum is a positive elapsed time ($sum s)"
else
  fail "tool-call _sum is a positive elapsed time" "got '${sum:-<absent>}'"
fi
inf="$(grep -F 'gardenio_agent_tool_call_latency_seconds_bucket{' "$METRICS_FILE" \
        | grep -F 'le="+Inf"' | awk '{print $NF}' | head -1)"
cnt="$(metric_value "$METRICS_FILE" 'gardenio_agent_tool_call_latency_seconds_count')"
if [ -n "$inf" ] && [ "$inf" = "$cnt" ]; then
  pass "+Inf bucket equals _count ($inf) — histogram is internally consistent"
else
  fail "+Inf bucket equals _count" "+Inf='${inf:-?}' vs _count='${cnt:-?}'"
fi

# Second turn: the count must advance by exactly the new call count. This is
# what proves observations accumulate per call rather than per turn.
resp="$(nudge 'run one more tool')"
if [ "$(printf '%s' "$resp" | jq -r '.delivered // empty' 2>/dev/null)" = "true" ]; then
  pass "turn 2 nudge delivered (${TOOLS_TURN2} tool call)"
else
  fail "turn 2 nudge delivered" "response: $resp"
fi
want2=$((want + TOOLS_TURN2))
got2="$(wait_for_tool_count "$want2" 30 || true)"
if [ "${got2%.*}" = "$want2" ] 2>/dev/null; then
  pass "tool-call count advanced to EXACTLY $want2 (accumulates per call)"
else
  fail "tool-call count advanced to EXACTLY $want2" "got '${got2:-<absent>}'"
fi

# The LLM-latency histogram is driven BY VALUE now that ka-0000005556 is fixed
# (MR !6631: parse_rfc3339_ms normalizes claudeless's +00:00 offset to UTC, so
# every log-timestamp gap parses and LlmLatencyTracker::update observes). coop
# derives one latency sample per DEDUPED assistant message.id from the gap to
# the preceding entry, so the count is EXACTLY session_request_count: both
# counters walk the same deduped-message stream, and EVERY request that has a
# predecessor to gap against — including the startup idle-ack, whose gap is a
# genuine (near-)zero — is observed exactly once.
#
# WHY == AND NOT == req-1 (ka-0000004208): the old premise expected exactly ONE
# unobserved request, the startup ack, on the theory that its zero gap was
# dropped. But the gap is truncated to MILLISECONDS, and a zero-ms gap is not
# unique to the ack: on fast hardware, same-turn tool-fanout assistant messages
# land in the SAME millisecond as their predecessor and ALSO floored to 0. The
# old `duration_api_ms > 0` observation gate then dropped a NONDETERMINISTIC
# number of those (1 on a slow host, 3 on the release CI), so llm_count trailed
# request_count by a flaky amount — the "6 vs 9 / 8 vs 9" flake that re-filed
# this bug. The fix (usage.rs: duration_api_ms is an Option; a measured Some(0)
# is observed, only an unmeasurable None is skipped) makes the histogram count
# the number of MEASURED requests deterministically. In this fixture every
# request has a predecessor, so llm_count == request_count exactly. An EXACT
# count — not "> 0" — is still what catches a PARTIALLY-dead observer, the
# ka-0000004093 shape a bare positivity check passes forever. wait_for_llm_count
# polls until the async watcher settles both counters.
read -r llm_cnt req_cnt < <(wait_for_llm_count 30 || true)
if [ -z "$llm_cnt" ]; then
  fail "llm_request histogram still exported" "series disappeared from the exposition"
elif [ -n "$req_cnt" ] && [ "$llm_cnt" = "$req_cnt" ] 2>/dev/null; then
  pass "llm_request count is EXACTLY session_request_count ($llm_cnt of $req_cnt — one sample per measured LLM request, incl. the near-zero-gap startup ack)"
else
  fail "llm_request count is EXACTLY session_request_count" \
    "llm_count='${llm_cnt:-<absent>}' vs request_count='${req_cnt:-<absent>}' — the LlmLatencyTracker gap pairing under-/over-counted (a partially-dead observer)"
fi

# _sum must reflect real elapsed latency (positive seconds), and +Inf must
# equal _count — the same internal-consistency check the tool-call histogram
# gets. Both read from the settled scrape wait_for_llm_count left in place.
llm_sum="$(metric_value "$METRICS_FILE" 'gardenio_agent_llm_request_latency_seconds_sum')"
if [ -n "$llm_sum" ] && python3 -c "import sys; sys.exit(0 if float(sys.argv[1]) > 0 else 1)" "$llm_sum" 2>/dev/null; then
  pass "llm_request _sum is a positive elapsed latency ($llm_sum s)"
else
  fail "llm_request _sum is a positive elapsed latency" "got '${llm_sum:-<absent>}'"
fi
llm_inf="$(grep -F 'gardenio_agent_llm_request_latency_seconds_bucket{' "$METRICS_FILE" \
            | grep -F 'le="+Inf"' | awk '{print $NF}' | head -1)"
if [ -n "$llm_inf" ] && [ "$llm_inf" = "$llm_cnt" ]; then
  pass "llm_request +Inf bucket equals _count ($llm_inf) — histogram is internally consistent"
else
  fail "llm_request +Inf bucket equals _count" "+Inf='${llm_inf:-?}' vs _count='${llm_cnt:-?}'"
fi
echo

# ── Scenario 4: session counters + label set ───────────────────────
blue "Scenario 4: session usage counters populate with the documented labels"
for m in gardenio_session_input_tokens_total gardenio_session_output_tokens_total \
         gardenio_session_request_count_total; do
  v="$(metric_value "$METRICS_FILE" "$m")"
  if [ -n "$v" ] && [ "${v%.*}" -gt 0 ] 2>/dev/null; then
    pass "$m is non-zero ($v)"
  else
    # A zero here almost always means the transcript was not discoverable, i.e.
    # CLAUDE_CONFIG_DIR did not take effect — call that out explicitly.
    fail "$m is non-zero" \
      "got '${v:-<absent>}' — transcript likely undiscoverable (CLAUDE_CONFIG_DIR)"
  fi
done
# The label set feeds every downstream dashboard/alert grouping.
label_line="$(grep -F 'gardenio_agent_tool_call_latency_seconds_count{' "$METRICS_FILE" | head -1)"
for lbl in "agent_name=\"${EXPECT_AGENT}\"" "project=\"${EXPECT_PROJECT}\"" 'account='; do
  if grep -qF -- "$lbl" <<<"$label_line"; then
    pass "exposition carries label: $lbl"
  else
    fail "exposition carries label: $lbl" "line: $label_line"
  fi
done
echo

# ── Scenario 5: /api/v1/agent live-usage fields ────────────────────
blue "Scenario 5: /api/v1/agent exposes the statusreporter live-usage triple"
aj="$(agent_json)"
if [ -z "$aj" ]; then
  fail "GET /api/v1/agent returns a body" "empty response"
else
  pass "GET /api/v1/agent returns a body"
  dim "  $(printf '%s' "$aj" | jq -c '{state,context_tokens,cost_usd,request_count}' 2>/dev/null)"

  # Presence + JSON type. statusreporter's coopAgentResponse decodes
  # context_tokens as uint64 and cost_usd as float64, so a type change here
  # silently breaks the bead mirror.
  types="$(printf '%s' "$aj" | jq -r '
    [ (.context_tokens | type), (.cost_usd | type), (.request_count | type) ] | join(",")
  ' 2>/dev/null)"
  if [ "$types" = "number,number,number" ]; then
    pass "context_tokens / cost_usd / request_count are all JSON numbers"
  else
    fail "context_tokens / cost_usd / request_count are all JSON numbers" "got: $types"
  fi

  for f in context_tokens request_count; do
    v="$(printf '%s' "$aj" | jq -r ".${f} // empty" 2>/dev/null)"
    if [ -n "$v" ] && python3 -c "import sys; sys.exit(0 if float(sys.argv[1]) > 0 else 1)" "$v" 2>/dev/null; then
      pass "$f is non-zero after live turns ($v)"
    else
      fail "$f is non-zero after live turns" "got '${v:-<absent>}'"
    fi
  done

  # cost_usd needs a LiteLLM response_cost key claudeless never emits — assert
  # it is present and numeric (done above), and report the value honestly.
  cost="$(printf '%s' "$aj" | jq -r '.cost_usd' 2>/dev/null)"
  if python3 -c "import sys; sys.exit(0 if float(sys.argv[1]) > 0 else 1)" "$cost" 2>/dev/null; then
    pass "cost_usd is non-zero ($cost) — a LiteLLM cost key reached the session"
  else
    skip "cost_usd non-zero" \
      "claudeless emits no response_cost/_litellm_response_cost key; field presence+type asserted above"
  fi

  # The metrics port and the JSON API must agree — two views of one counter.
  # Both derive from UsageState, so a divergence means one read path is stale.
  m_req="$(metric_value "$METRICS_FILE" 'gardenio_session_request_count_total')"
  j_req="$(printf '%s' "$aj" | jq -r '.request_count' 2>/dev/null)"
  if [ -n "$m_req" ] && [ "${m_req%.*}" = "${j_req%.*}" ]; then
    pass "request_count agrees across /metrics and /api/v1/agent (both $j_req)"
  else
    # A scrape taken microseconds earlier can legitimately trail by a turn, so
    # re-scrape once before calling it a real divergence.
    scrape
    m_req="$(metric_value "$METRICS_FILE" 'gardenio_session_request_count_total')"
    if [ "${m_req%.*}" = "${j_req%.*}" ]; then
      pass "request_count agrees across /metrics and /api/v1/agent (both $j_req, after re-scrape)"
    else
      fail "request_count agrees across /metrics and /api/v1/agent" \
        "/metrics=$m_req vs /api/v1/agent=$j_req"
    fi
  fi
fi
echo

# ── Summary ───────────────────────────────────────────────────────
blue "═══════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green "  ✓ All $total assertions passed ($SKIP skipped)"
else
  red "  ✗ $FAIL/$total assertions failed ($SKIP skipped)"
  echo
  for err in "${ERRORS[@]}"; do
    red "   - $err"
  done
fi
blue "═══════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
