#!/usr/bin/env bash
# test-gb-spawn-success-token.sh — E2E gate for the `gb spawn` SPAWNED success
# token: the machine-greppable, first-line contract a gardener loop scripts
# against to confirm a dispatch instead of parsing human prose.
#
# Bead ka-0000004253 (parent ka-0000000958). Guards the feature merged by
# `feat(gardenio/gb): give gb spawn a machine-greppable success signal`
# (SHA 283942476, ka-0000000982): 67 net-new lines in spawn.go + a 94-line
# SpawnResult/SpawnOutcome contract in beadsapi/client.go that shipped with
# UNIT tests only (spawn_test.go, 238 lines) — no e2e leg exercised the REAL
# compiled `gb spawn` binary's stdout.
#
# WHY THIS MATTERS MORE THAN A NORMAL FIELD-ADD: this is a contract for MACHINE
# consumers. The commit message records the exact failure it exists to stop —
# the prior first line was the present participle "Spawning agent <name>", so a
# gardener grepping '^Spawned' read a SUCCESSFUL spawn as failure and
# re-dispatched THREE times (bug ka-0000001243). The token exists specifically
# to end duplicate-spawn storms fleet-wide, so a silent regression (reverting to
# prose, or the token drifting off line 1) re-opens that failure mode — and a
# unit test on the formatter cannot catch a change in what the shipped CLI
# actually PRINTS end-to-end. Only driving the real binary does.
#
# WHAT THIS PROVES — the REAL `gb` binary, not a reimplementation. `gb spawn`
# talks to the kbeads daemon over HTTP; we stand up a tiny, deterministic,
# STATEFUL mock daemon (python3 http.server) on localhost and point the binary
# at it via BEADS_HTTP_URL. The mock lets us drive the three spawn outcomes
# hermetically — and, decisively, a genuine FAILING spawn for the negative
# control, which no live daemon would produce on demand. The mock never
# hardcodes the token: it serves bead JSON, and the token/status is whatever the
# real spawn.go prints from it.
#
# Legs (each maps to a Definition-of-Done item on the bead):
#   1. HEADLINE (DoD 1): a successful `gb spawn --task` prints a line matching
#      `^SPAWNED ` as the FIRST line of STDOUT and exits 0.
#   2. FIELDS PARSE (DoD 2): agent=, bead=, status= are all present and
#      non-empty on that line; task= is present because --task was passed.
#   3. ENUM MEMBERSHIP (DoD 3): status is a MEMBER of {created, existing} —
#      asserted as membership, never a fixed value (a fresh spawn happens to be
#      `created`, but the leg would still pass if the enum were extended and
#      this call resolved to the other member).
#   4. RE-SPAWN DEDUP (DoD 4): re-spawning the SAME task is the single-claim
#      guard's no-op → status=existing, reason=single_claim. This is the one
#      place a specific value IS deterministic. It is NOT hardcoded by the mock:
#      spawn #1 CREATES an agent bead the mock records as the live holder of the
#      task; spawn #2's real findLiveAgentForTask guard finds THAT record and
#      returns it — the bead= it reports is the exact id spawn #1 created.
#   5. NEGATIVE CONTROL (DoD 5): a FAILING spawn (mock 500 on bead create)
#      prints NO `^SPAWNED` line anywhere in stdout and exits NON-ZERO — proving
#      the grep discriminates rather than always-passing.
#   6. --jq CONTRACT: `gb spawn --jq '.status'` yields a bare enum member, and
#      `--jq 'has("status")'` is true — the JSON half of the scripting contract,
#      asserted with jq has() (NOT the `//` default, which treats false/null as
#      empty and manufactures phantoms — advice ka-0000006874).
#
# NEGATIVE-CONTROL DISCRIMINATION (DoD, mandatory): the plausible bad
# regressions are (a) reverting to prose ("Spawning agent foo"), and (b) moving
# SPAWNED off the first line into trailing noise. Leg 1 anchors the token at
# `head -1` of stdout — a whole-stream `grep -q SPAWNED` would pass even if the
# token drifted, which is exactly the scripting contract the feature guarantees.
# Leg 5 proves the anchor is falsifiable: on a real failure the line is absent
# AND the exit is non-zero. Every positive leg first asserts the captured stdout
# is NON-EMPTY before matching, so an empty capture can never pass vacuously.
#
# STDOUT vs STDERR: the token is printed to STDOUT; spawn.go's slog lines
# ("SpawnAgent: returning existing…") and the soft role warning go to STDERR.
# Every leg captures the two streams to SEPARATE files and asserts the token
# against STDOUT ONLY, so stderr noise can neither satisfy nor mask the line-1
# anchor.
#
# Prerequisites: a `gb` binary (GB_BIN, default `gb`) + python3. NO kubectl
# context, NO namespace, NO real beads daemon, NO network egress — the daemon is
# a localhost mock. Fully HERMETIC, so it runs unconditionally in every shard
# and self-SKIPs LOUD if a prerequisite is missing (never reds by skip).
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-gb-spawn-success-token.sh
#   GB_BIN=/path/to/gb ./gardenio/tests/e2e/scripts/test-gb-spawn-success-token.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
GB_BIN="${GB_BIN:-gb}"
RUN_ID="${RUN_ID:-$$}"
# A high, PID-derived port so parallel shards on the same host don't collide.
PORT="${GB_SPAWN_MOCK_PORT:-$(( 20000 + (RUN_ID % 20000) ))}"
TASK_ID="gd-SPAWNTOK-${RUN_ID}"

PASS=0
FAIL=0
ERRORS=()
TMPROOT=""
MOCK_PID=""

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}
have() { command -v "$1" >/dev/null 2>&1; }

# SKIP loud — a missing prerequisite must never be mistaken for a pass, but must
# also never red the suite (this module is wired into every shard).
skip_all() {
  blue "SKIP gb-spawn-success-token: $1"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

# shellcheck disable=SC2317  # invoked indirectly via `trap cleanup EXIT`
cleanup() {
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  [ -n "$TMPROOT" ] && [ -d "$TMPROOT" ] && rm -rf "$TMPROOT"
  return 0
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " gb spawn SPAWNED success-token E2E (ka-0000004253)"
echo " the anti-duplicate-dispatch scripting contract"
echo "════════════════════════════════════════════════════"

have python3 || skip_all "python3 not available"
GB_ABS="$(command -v "$GB_BIN" 2>/dev/null)"
[ -n "$GB_ABS" ] || skip_all "gb binary not available (set GB_BIN)"
TMPROOT="$(mktemp -d -t gb-spawn-token-e2e-XXXXXX)"

# The token is a 2026.218+ feature (SHA 283942476). On an older gb the first
# stdout line is the legacy "Spawning agent <name>" prose; SKIP loud rather than
# red so this module is safe to wire in ahead of / across the rollout. The probe
# is `--help` text, which requires no daemon. Capture to a FILE first, then grep
# — NOT `gb ... | grep -q` — because under `set -o pipefail` grep -q exits at the
# first match and closes the pipe, SIGPIPE-ing a still-writing gb (rc 141) and
# turning a token-bearing binary into a FALSE skip under concurrent shard load.
"$GB_ABS" spawn --help >"${TMPROOT}/help.txt" 2>&1 || true
if ! grep -q 'SPAWNED agent=' "${TMPROOT}/help.txt"; then
  skip_all "this gb binary has no SPAWNED token in 'gb spawn --help' (pre-2026.218)"
fi

# ── The stateful mock kbeads daemon ───────────────────────────────
# Serves exactly the endpoints `gb spawn --task` touches (traced against the
# real binary):
#   GET  /v1/beads/<task>                → the pre-assigned task bead (project label)
#   GET  /v1/agents-by-name/<name>       → name-dedup guard (404 = no incumbent)
#   GET  /v1/beads?...field=task_id=<t>  → single-claim guard (empty = free)
#   POST /v1/beads                       → create the agent bead (or 500 in fail mode)
#   POST /v1/beads/<id>/labels           → best-effort project/role labels (204)
#   POST /v1/beads/<id>/dependencies     → best-effort task link (204)
#   PATCH /v1/beads/<task>               → synchronous single-claim pre-stamp (204)
#
# STATEFUL: a successful POST records the new agent bead as the LIVE holder of
# its task_id AND under its name, so a SECOND spawn's real single-claim /
# name-dedup guard finds it and resolves to status=existing — the dedup is
# produced by spawn.go's own guard, not faked by the mock. Set MOCK_FAIL=1 in
# the environment to make POST /v1/beads return 500 (the negative control).
cat > "${TMPROOT}/mock_kbeads.py" <<'PYEOF'
import json, os, sys, urllib.parse
from http.server import BaseHTTPRequestHandler
from socketserver import ThreadingMixIn, TCPServer

PORT = int(sys.argv[1])
TASK = sys.argv[2]
FAIL = os.environ.get("MOCK_FAIL", "0") == "1"

# task_id -> agent bead JSON (records the live agent holding a task);
# agent name -> agent bead JSON (records the live agent under its name).
LIVE_BY_TASK = {}
LIVE_BY_NAME = {}
_seq = [0]


def agent_bead(bid, name, task, role):
    # agent_state left non-terminal ("" would also be treated as live) so the
    # single-claim guard's terminalAgentStates check does NOT skip it.
    return {
        "id": bid, "title": name, "type": "agent", "status": "in_progress",
        "labels": ["project:gardenio"],
        "fields": {"agent": name, "task_id": task, "role": role,
                   "agent_state": "working"},
    }


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):  # keep the mock quiet on stderr
        pass

    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _read(self):
        n = int(self.headers.get("Content-Length", "0") or 0)
        return self.rfile.read(n) if n else b""

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        query = self.path.split("?", 1)[1] if "?" in self.path else ""
        if path == "/v1/beads/" + TASK:
            return self._send(200, {
                "id": TASK, "title": "Backfill spawn token e2e", "type": "task",
                "status": "open", "labels": ["project:gardenio"], "fields": {},
            })
        if path.startswith("/v1/agents-by-name/"):
            name = urllib.parse.unquote(path[len("/v1/agents-by-name/"):])
            if name in LIVE_BY_NAME:
                return self._send(200, LIVE_BY_NAME[name])
            return self._send(404, {"error": "agent bead not found"})
        if path == "/v1/beads":
            params = urllib.parse.parse_qs(query)
            task = None
            for f in params.get("field", []):
                if f.startswith("task_id="):
                    task = f.split("=", 1)[1]
            if task and task in LIVE_BY_TASK:
                return self._send(200, {"beads": [LIVE_BY_TASK[task]], "total": 1})
            return self._send(200, {"beads": [], "total": 0})
        return self._send(404, {"error": "unexpected GET " + path})

    def do_POST(self):
        raw = self._read()
        path = self.path.split("?", 1)[0]
        if path == "/v1/beads":
            if FAIL:
                return self._send(500, {"error": "synthetic daemon failure (negative control)"})
            body = json.loads(raw or b"{}")
            fields = body.get("fields", {})
            if isinstance(fields, str):
                fields = json.loads(fields)
            _seq[0] += 1
            bid = "gd-MOCKNEW%03d" % _seq[0]
            name = fields.get("agent", "")
            task = fields.get("task_id", "")
            role = fields.get("role", "")
            bead = agent_bead(bid, name, task, role)
            if task:
                LIVE_BY_TASK[task] = bead
            if name:
                LIVE_BY_NAME[name] = bead
            return self._send(200, {
                "id": bid, "title": name, "type": "agent", "status": "open",
                "labels": ["project:gardenio"], "fields": {},
            })
        # labels / dependencies — best-effort, 204.
        return self._send(204, None) if False else self._send(200, {})

    def do_PATCH(self):
        self._read()
        return self._send(200, {})


class Server(ThreadingMixIn, TCPServer):
    allow_reuse_address = True
    daemon_threads = True


with Server(("127.0.0.1", PORT), H) as httpd:
    # Announce readiness on stdout so the launcher can wait for a real bind
    # instead of racing a fixed sleep.
    print("READY %d" % PORT, flush=True)
    httpd.serve_forever()
PYEOF

# ── Launch helpers ────────────────────────────────────────────────
# Start the mock and block until it prints READY (a real successful bind), so
# no leg races a not-yet-listening socket. $1=fail-mode (0/1).
start_mock() {
  local failmode="$1"
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  MOCK_PID=""
  local readyf="${TMPROOT}/mock-ready.$$"
  : > "$readyf"
  MOCK_FAIL="$failmode" python3 "${TMPROOT}/mock_kbeads.py" "$PORT" "$TASK_ID" \
    > "$readyf" 2>"${TMPROOT}/mock.err" &
  MOCK_PID=$!
  local waited=0
  while [ "$waited" -lt 100 ]; do
    if grep -q '^READY ' "$readyf" 2>/dev/null; then return 0; fi
    if ! kill -0 "$MOCK_PID" 2>/dev/null; then
      dim "  mock exited early: $(head -c 200 "${TMPROOT}/mock.err" 2>/dev/null)"
      return 1
    fi
    python3 -c 'import time;time.sleep(0.05)'
    waited=$((waited + 1))
  done
  return 1
}

stop_mock() {
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  # Reap so a new bind on the same port isn't refused by a lingering socket.
  [ -n "$MOCK_PID" ] && wait "$MOCK_PID" 2>/dev/null || true
  MOCK_PID=""
}

# Run the real `gb spawn`, capturing STDOUT and STDERR to SEPARATE files so the
# line-1 token anchor is asserted against stdout ONLY. Runs with KD_AGENT_ID /
# BOAT_AGENT_BEAD_ID UNSET so spawnerSlackChannel makes no extra GetBead call,
# and with a static BEADS_TOKEN so no credstore/login path is taken.
# $1=stdout file  $2=stderr file  rest=gb args
run_spawn() {
  local of="$1" ef="$2"; shift 2
  env -u KD_AGENT_ID -u BOAT_AGENT_BEAD_ID \
    BEADS_HTTP_URL="http://127.0.0.1:${PORT}" \
    BEADS_TOKEN="e2e-mock-token" \
    "$GB_ABS" --actor "e2e-spawn-token" --operated-by "system:e2e" \
    spawn "$@" >"$of" 2>"$ef"
  return $?
}

# ── Leg 1–3, 6: a successful spawn (created) ──────────────────────
blue "Scenario A: successful spawn → SPAWNED token on stdout line 1 (created)"
if ! start_mock 0; then
  skip_all "could not bind mock daemon on 127.0.0.1:${PORT}"
fi

OUT_A="${TMPROOT}/a.out"; ERR_A="${TMPROOT}/a.err"
run_spawn "$OUT_A" "$ERR_A" --task "$TASK_ID" --role crew
RC_A=$?

# Anti-vacuity: the capture must be non-empty before any grep can "pass".
if [ -s "$OUT_A" ]; then
  pass "spawn produced non-empty stdout"
else
  fail "spawn produced non-empty stdout" "stdout empty; stderr: $(head -c 200 "$ERR_A")"
fi

LINE1_A="$(head -1 "$OUT_A")"

# Leg 1: token anchored at stdout LINE 1 (negative control: not merely present).
# Matched with a `case` glob rather than a `… | grep` pipe: under `set -o
# pipefail` an early-exiting grep can SIGPIPE its producer, and a pipe adds a
# race for no benefit when the subject is already an in-shell string.
case "$LINE1_A" in
  "SPAWNED "*) pass "stdout line 1 matches '^SPAWNED ' (DoD 1)" ;;
  *) fail "stdout line 1 matches '^SPAWNED '" "line 1 was: '$LINE1_A'" ;;
esac

# Leg 1: exit 0 on success.
if [ "$RC_A" -eq 0 ]; then
  pass "successful spawn exits 0 (DoD 1)"
else
  fail "successful spawn exits 0" "got exit $RC_A; stderr: $(head -c 200 "$ERR_A")"
fi

# Belt-and-braces for the line-1 contract: the token must NOT sit only in
# trailing noise. A whole-stream grep passing while head -1 fails is exactly the
# drift the anchor guards against. (grep -c reads a FILE, not a pipe from gb, so
# no SIGPIPE race; the line-1 check is the pipe-free `case` above.)
line1_is_token=0
case "$LINE1_A" in "SPAWNED "*) line1_is_token=1 ;; esac
if [ "$(grep -c '^SPAWNED ' "$OUT_A")" -ge 1 ] && [ "$line1_is_token" -eq 0 ]; then
  fail "token drifted off line 1" "appears in stdout but not on line 1 — scripting contract broken"
fi

# Leg 2: fields present and non-empty; task= present because --task was passed.
field_val() { printf '%s\n' "$LINE1_A" | grep -oE "(^| )$1=[^ ]+" | head -1 | sed -E "s/^ ?$1=//"; }
for key in agent bead status; do
  v="$(field_val "$key")"
  if [ -n "$v" ]; then
    pass "field ${key}= present and non-empty (DoD 2): ${key}=${v}"
  else
    fail "field ${key}= present and non-empty" "line 1: '$LINE1_A'"
  fi
done
TASK_FIELD="$(field_val task)"
if [ "$TASK_FIELD" = "$TASK_ID" ]; then
  pass "field task= present and equals the --task bead (DoD 2): task=${TASK_FIELD}"
else
  fail "field task= present and equals the --task bead" "got '$TASK_FIELD', want '$TASK_ID'"
fi

# Leg 3: status is a MEMBER of {created, existing} — membership, not a fixed value.
STATUS_A="$(field_val status)"
case "$STATUS_A" in
  created|existing) pass "status is a member of {created, existing} (DoD 3): status=${STATUS_A}" ;;
  *) fail "status is a member of {created, existing}" "got status='$STATUS_A'" ;;
esac
# A fresh spawn against a free task IS deterministically 'created' — assert it
# WITHOUT collapsing leg 3 into a fixed-value check.
if [ "$STATUS_A" = "created" ]; then
  pass "fresh spawn against a free task reports status=created"
else
  fail "fresh spawn against a free task reports status=created" "got '$STATUS_A'"
fi

# Leg 6: the --jq JSON contract. Re-run against a FRESH mock so this spawn is
# also a fresh 'created' (the mock from scenario A now holds the task as live).
stop_mock
start_mock 0 || skip_all "could not restart mock daemon for --jq leg"
JQ_OUT="${TMPROOT}/jq.out"; JQ_ERR="${TMPROOT}/jq.err"
run_spawn "$JQ_OUT" "$JQ_ERR" --task "$TASK_ID" --role crew --jq '.status'
RC_JQ=$?
JQ_STATUS="$(tr -d '[:space:]' < "$JQ_OUT")"
if [ "$RC_JQ" -eq 0 ] && { [ "$JQ_STATUS" = "created" ] || [ "$JQ_STATUS" = "existing" ]; }; then
  pass "gb spawn --jq '.status' yields an enum member (DoD 6): $JQ_STATUS"
else
  fail "gb spawn --jq '.status' yields an enum member" "rc=$RC_JQ out='$JQ_STATUS' stderr: $(head -c 160 "$JQ_ERR")"
fi
# has() — key presence via jq, NEVER `//` (advice ka-0000006874).
stop_mock
start_mock 0 || skip_all "could not restart mock daemon for --jq has() leg"
HAS_OUT="${TMPROOT}/has.out"
run_spawn "$HAS_OUT" "${TMPROOT}/has.err" --task "$TASK_ID" --role crew --jq 'has("status")'
if [ "$(tr -d '[:space:]' < "$HAS_OUT")" = "true" ]; then
  pass "gb spawn --jq 'has(\"status\")' is true — status key is always present (DoD 6)"
else
  fail "gb spawn --jq 'has(\"status\")' is true" "got '$(tr -d '[:space:]' < "$HAS_OUT")'"
fi
echo

# ── Leg 4: re-spawn the SAME task → status=existing (single_claim) ─
blue "Scenario B: re-spawn same task → guard-deduped no-op (existing)"
# One fresh mock; spawn #1 CREATES and the mock records it live, spawn #2's real
# single-claim guard finds THAT record → existing. The dedup is spawn.go's, not
# the mock's — proven by bead= on #2 equalling the id #1 created.
stop_mock
start_mock 0 || skip_all "could not restart mock daemon for re-spawn leg"

OUT_B1="${TMPROOT}/b1.out"; run_spawn "$OUT_B1" "${TMPROOT}/b1.err" --task "$TASK_ID" --role crew
RC_B1=$?
LINE_B1="$(head -1 "$OUT_B1")"
BEAD_B1="$(printf '%s\n' "$LINE_B1" | grep -oE '(^| )bead=[^ ]+' | head -1 | sed -E 's/^ ?bead=//')"
STATUS_B1="$(printf '%s\n' "$LINE_B1" | grep -oE '(^| )status=[^ ]+' | head -1 | sed -E 's/^ ?status=//')"
if [ "$RC_B1" -eq 0 ] && [ "$STATUS_B1" = "created" ] && [ -n "$BEAD_B1" ]; then
  pass "re-spawn setup: first spawn created bead $BEAD_B1"
else
  fail "re-spawn setup: first spawn created a fresh agent" "rc=$RC_B1 status='$STATUS_B1' bead='$BEAD_B1'"
fi

OUT_B2="${TMPROOT}/b2.out"; ERR_B2="${TMPROOT}/b2.err"
run_spawn "$OUT_B2" "$ERR_B2" --task "$TASK_ID" --role crew
RC_B2=$?
LINE_B2="$(head -1 "$OUT_B2")"
STATUS_B2="$(printf '%s\n' "$LINE_B2" | grep -oE '(^| )status=[^ ]+' | head -1 | sed -E 's/^ ?status=//')"
REASON_B2="$(printf '%s\n' "$LINE_B2" | grep -oE '(^| )reason=[^ ]+' | head -1 | sed -E 's/^ ?reason=//')"
BEAD_B2="$(printf '%s\n' "$LINE_B2" | grep -oE '(^| )bead=[^ ]+' | head -1 | sed -E 's/^ ?bead=//')"

if [ "$RC_B2" -eq 0 ]; then
  pass "re-spawn exits 0 (a deduped no-op is success, not failure)"
else
  fail "re-spawn exits 0" "got exit $RC_B2; stderr: $(head -c 200 "$ERR_B2")"
fi
case "$LINE_B2" in
  "SPAWNED "*) pass "re-spawn still prints a SPAWNED line on stdout line 1" ;;
  *) fail "re-spawn still prints a SPAWNED line on stdout line 1" "line 1: '$LINE_B2'" ;;
esac
if [ "$STATUS_B2" = "existing" ]; then
  pass "re-spawn of the same task reports status=existing (DoD 4)"
else
  fail "re-spawn of the same task reports status=existing" "got status='$STATUS_B2'; line: '$LINE_B2'"
fi
if [ "$REASON_B2" = "single_claim" ]; then
  pass "re-spawn carries reason=single_claim (the guard that fired)"
else
  fail "re-spawn carries reason=single_claim" "got reason='$REASON_B2'"
fi
# The strongest anti-vacuity check: the incumbent bead #2 returned is the exact
# bead #1 created — so the 'existing' verdict came from the REAL guard finding a
# real record, not from a hardcoded mock response.
if [ -n "$BEAD_B2" ] && [ "$BEAD_B2" = "$BEAD_B1" ]; then
  pass "re-spawn returns the incumbent bead #1 created ($BEAD_B2) — dedup is the real guard, not the mock"
else
  fail "re-spawn returns the incumbent bead #1 created" "b1=$BEAD_B1 b2=$BEAD_B2"
fi
echo

# ── Leg 5: NEGATIVE CONTROL — a failing spawn prints no token, exits ≠0 ──
blue "Scenario C: failing spawn → NO SPAWNED line, non-zero exit (negative control)"
stop_mock
start_mock 1 || skip_all "could not start mock daemon in fail mode"

OUT_C="${TMPROOT}/c.out"; ERR_C="${TMPROOT}/c.err"
run_spawn "$OUT_C" "$ERR_C" --task "$TASK_ID" --role crew
RC_C=$?

if [ "$RC_C" -ne 0 ]; then
  pass "failing spawn exits non-zero (DoD 5): exit $RC_C"
else
  fail "failing spawn exits non-zero" "got exit 0; stdout: $(head -c 200 "$OUT_C")"
fi
# The discriminating assertion: NO ^SPAWNED anywhere in stdout on failure. If
# this ever passes, the leg-1 grep is meaningful; if a regression printed the
# token unconditionally, THIS leg reds.
if grep -q '^SPAWNED' "$OUT_C"; then
  fail "failing spawn prints NO SPAWNED line" "found: $(grep '^SPAWNED' "$OUT_C" | head -1)"
else
  pass "failing spawn prints NO '^SPAWNED' line anywhere in stdout (DoD 5)"
fi
stop_mock
echo

# ── Summary ───────────────────────────────────────────────────────
echo "════════════════════════════════════════════════════"
echo " gb-spawn-success-token Summary: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All assertions passed — the SPAWNED scripting contract holds end-to-end."
exit 0
