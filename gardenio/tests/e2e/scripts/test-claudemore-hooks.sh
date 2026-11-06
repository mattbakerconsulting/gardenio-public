#!/usr/bin/env bash
# test-claudemore-hooks.sh — claudemore hooks e2e (REPLAY tier, the Target rung)
#
# Bead ka-0000004452 (Phase 2 §T5/T6, epic ka-0000001159). This is the tier where
# the REAL `claude` binary runs in the loop against claudemore-REPLAY at $0, so
# the suite can assert real-reasoning hook behaviours that the claudeless
# (faked-binary) tier structurally cannot. Assertions target OUTCOMES/side-effects
# (evidence files, cassette hits, gate transitions, NATS events) — NEVER model
# text (R4-DET-1) — so the replayed run is deterministic.
#
# ── WHY REPLAY, HERMETIC, $0 ──────────────────────────────────────────────────
#
# claudemore fakes only the LLM wire; the agent (`claude`) stays REAL. In REPLAY
# mode claudemore serves recorded cassettes byte-for-byte with litellm bypassed
# entirely and fail_on_miss (a miss is a loud 424, never a LIVE fall-back). That
# lets a CI e2e drive `claude` through a full hook loop with zero model spend.
#
# The cassettes are recorded IN THIS RUN, not committed. The opus system prompt
# embeds the current date, which is inside claudemore's hashed request body — a
# checked-in cassette would MISS the next day (fail_on_miss → daily-red CI). So
# each scenario RECORDS against a deterministic stub upstream (LIVE+record, a
# filesystem cassette store) and immediately REPLAYS from that same store (same
# clock → identical hashes). Cassettes are ephemeral temp files; nothing drifts.
#
# The filesystem cassette store (CLAUDEMORE_CASSETTE_DIR) is what makes this
# hermetic — CI has no S3/MinIO. It mirrors the S3 store's key layout exactly.
#
# ── LEGS (each a hard sub-gate; any FAIL reds the shard) ──────────────────────
#
#   T5 prime-consumption   The SessionStart hook injects context into the model.
#                          A record run wires a SessionStart hook that emits a
#                          unique marker; the REAL claude then sends that marker
#                          back to claudemore inside its request body (proving the
#                          model CONSUMED the injected context, not merely that the
#                          hook fired). Asserted structurally: the marker appears
#                          in a recorded cassette request.
#
#   T6 stop-gate-loop      The Stop-hook block→continue→allow cycle. A stop-gate
#                          hook exits 2 (block) with a reason on the first Stop,
#                          exits 0 (allow) once a decision-file exists. The REAL
#                          claude, driven by a stub that reacts to the injected
#                          block reason, ACTS on the block (creates the decision)
#                          then stops cleanly. Asserted by the gate log ordering
#                          BLOCK→ALLOW and the decision side-effect.
#
#   replay-zero-miss       The recorded cassettes REPLAY to the SAME claude run
#                          with ZERO cassette misses and no upstream — the
#                          determinism property the whole tier rests on.
#
#   nats-relay             PreToolUse + PostToolUse relay to NATS end-to-end via
#                          the real `gb hook relay` (embedded-NATS probe).
#
# ── SKIP-CLEAN CONTRACT ───────────────────────────────────────────────────────
#
# Requires: the `claude` binary, a Go toolchain (to build claudemore + gb +
# the NATS probe from source), and python3. If ANY is absent the module SKIPS
# CLEANLY (exit 0) with a LOUD banner — an absent toolchain must never red the
# suite (mirrors test-gvisor-freeze-thaw / test-airgap-image-distribution). When
# all are present, every leg is a HARD assertion.
#
# Standalone runnable:
#   ./gardenio/tests/e2e/scripts/test-claudemore-hooks.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# scripts/ -> e2e/ -> tests/ -> gardenio/ -> controller
CONTROLLER_DIR="$(cd "$SCRIPT_DIR/../../../controller" 2>/dev/null && pwd)" || CONTROLLER_DIR=""
FIXTURE_DIR="$SCRIPT_DIR/../fixtures/claudemore-hooks"
STUB="$FIXTURE_DIR/stub_upstream.py"

CLAUDE_BIN="${CLAUDE_BIN:-claude}"
GO_BIN="${GO_BIN:-go}"
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/claudemore-hooks.XXXXXX")"
PASS=0
FAIL=0
ERRORS=()

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  local name="$1" reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

skip_clean() {
  blue "SKIP claudemore-hooks: $1"
  blue "  The claudemore hooks e2e drives the REAL claude binary against"
  blue "  claudemore-REPLAY; without the toolchain there is nothing to drive."
  blue "  Skipping cleanly (exit 0) — an absent toolchain must never red the"
  blue "  suite (see test-gvisor-freeze-thaw / test-airgap-image-distribution)."
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

cleanup() {
  # Kill any stragglers we launched, then remove the workdir.
  for pidf in "$WORKDIR"/*.pid; do
    [ -f "$pidf" ] || continue
    local p; p="$(cat "$pidf" 2>/dev/null)"
    [ -n "$p" ] && kill "$p" >/dev/null 2>&1 || true
  done
  rm -rf "$WORKDIR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# free_port — echo an ephemeral TCP port that is currently free.
free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

# wait_http — poll a URL until it answers or a timeout elapses.
wait_http() {
  local url="$1" tries="${2:-40}"
  for _ in $(seq 1 "$tries"); do
    if curl -s -o /dev/null -m 1 "$url" 2>/dev/null; then return 0; fi
    sleep 0.25
  done
  return 1
}

# wait_pid_exit — poll until a PID is gone (up to `tries` × 0.25s), then SIGKILL
# it as a backstop and return. Used to WAIT for claudemore's graceful SIGTERM
# drain to finish before REPLAY reads the cassette store (gd-1B3EYCqIXu) rather
# than racing it with a fixed sleep.
wait_pid_exit() {
  local pid="$1" tries="${2:-40}"
  [ -n "$pid" ] || return 0
  for _ in $(seq 1 "$tries"); do
    kill -0 "$pid" 2>/dev/null || return 0   # gone → drain complete
    sleep 0.25
  done
  kill -KILL "$pid" >/dev/null 2>&1 || true   # backstop: force-stop a wedged drain
  return 0
}

# ── Preflight: skip cleanly if any prerequisite is missing ──────────
blue "╔══════════════════════════════════════════════════════════╗"
blue "║  claudemore hooks e2e (ka-0000004452) — REPLAY tier       ║"
blue "╚══════════════════════════════════════════════════════════╝"
echo ""

command -v "$CLAUDE_BIN" >/dev/null 2>&1 || skip_clean "'$CLAUDE_BIN' binary not on PATH"
command -v "$GO_BIN"     >/dev/null 2>&1 || skip_clean "'$GO_BIN' toolchain not on PATH"
command -v python3       >/dev/null 2>&1 || skip_clean "python3 not on PATH"
command -v curl          >/dev/null 2>&1 || skip_clean "curl not on PATH"
[ -n "$CONTROLLER_DIR" ] && [ -f "$CONTROLLER_DIR/go.mod" ] || skip_clean "controller module not found (looked at '${CONTROLLER_DIR:-<unresolved>}')"
[ -f "$STUB" ] || skip_clean "stub upstream fixture not found at $STUB"

dim "  claude:     $(command -v "$CLAUDE_BIN")  ($("$CLAUDE_BIN" --version 2>/dev/null | head -1))"
dim "  go:         $(command -v "$GO_BIN")  ($("$GO_BIN" version 2>/dev/null))"
dim "  controller: $CONTROLLER_DIR"
dim "  workdir:    $WORKDIR"
echo ""

# ── Build claudemore + gb from source ───────────────────────────────
# The PreToolUse/PostToolUse → NATS assertion is an `//go:build e2e`-tagged Go
# test (internal/claudemore/hooks_natsrelay_e2e_test.go) run via `go test -tags
# e2e` below — it stays off the default build/lint surface so its heavy embedded
# NATS server never inflates the lint task's typecheck time.
blue "Building claudemore + gb from source..."
CM_BIN="$WORKDIR/claudemore"
GB_BIN="$WORKDIR/gb"
BUILD_LOG="$WORKDIR/build.log"
if ! ( cd "$CONTROLLER_DIR" \
       && CGO_ENABLED=0 "$GO_BIN" build -o "$CM_BIN" ./cmd/claudemore/ \
       && CGO_ENABLED=0 "$GO_BIN" build -o "$GB_BIN" ./cmd/gb/ ) >"$BUILD_LOG" 2>&1; then
  # A build failure of OUR code is a real failure, not a skip — but a missing
  # network / module-download failure in a locked-down shard is an environment
  # gap that must not red the suite. Distinguish by grepping the log.
  if grep -qiE "cannot find module|dial tcp|no such host|timeout|connection refused|proxy" "$BUILD_LOG"; then
    skip_clean "go build could not fetch deps (offline shard): $(tail -3 "$BUILD_LOG" | tr '\n' ' ')"
  fi
  red "  ✗ build failed:"
  dim "$(tail -15 "$BUILD_LOG")"
  exit 1
fi
green "  ✓ built claudemore + gb"
echo ""

# ── Seed a hermetic, logged-in claude HOME ──────────────────────────
# claude `--print` REFUSES to run without local login state: with no
# ~/.claude/.credentials.json (+ ~/.claude.json onboarding marker) it prints
# "Not logged in · Please run /login" and exits 1 BEFORE any model turn — so the
# SessionStart hook fires but claude never makes the tool call, record_rc=1, and
# the gate.log/marker/PreToolUse assertions all fail. This is EXACTLY what red
# the release gate on 2026.218.8/.9 (ka-0000000461 / gd-1B3EYCqIXu): the shard's
# install-claude task (ka-0000007022) `sudo install`s a FRESH claude ELF with no
# ~/.claude, while an agent pod / dev box happens to carry login state — a
# works-on-my-machine that passed locally (12/12) but red deterministically on
# staging (0/12, retry-proof).
#
# Fix: drive claude under a DEDICATED, seeded HOME so the module is hermetic and
# no longer depends on whatever ambient ~/.claude the host provides. The token is
# a $0 OFFLINE stub — every request is redirected to the local claudemore/stub
# via ANTHROPIC_BASE_URL, so it is never presented to a real API.
#
# CRUCIAL — RECORD and REPLAY are two SEPARATE claude launches that MUST emit
# byte-identical request bodies, or the replay MISSes the cassette. Two coupled
# properties are required (both learned the hard way — ka-0000000461 / gd-1B3EYCqIXu):
#
#   (a) SAME home PATH for both launches. claude embeds HOME-derived, per-machine
#       content into the system prompt — notably the `# Memory` section's path
#       `$HOME/.claude/projects/<cwd-slug>/memory/`. Two DIFFERENT home dirs (e.g.
#       two `mktemp -d` paths) put two different paths in the prompt → different
#       request hash → MISS on EVERY turn. This is the field that survived the
#       metadata/cache_control/cc_version hash-normalization (ka-0000000029) and
#       kept the module red on staging even after those landed. So we use ONE
#       fixed home path for both launches.
#   (b) FRESH state for each launch. claude also MUTATES ~/.claude.json per launch
#       (session id, project cache, numStartups). If the record launch's mutations
#       are still on disk when replay launches, replay emits a slightly different
#       body. So we RE-SEED (wipe + repopulate) that one fixed home before EACH
#       launch — same path, fresh contents.

# seed_claude_home <dir> — (re)populate <dir> as a hermetic, logged-in claude HOME.
# Wipes any prior contents first so a re-seed between launches leaves NO mutated
# state (property (b) above); the PATH stays constant across calls (property (a)).
seed_claude_home() {
  local home="$1"
  rm -rf "$home"
  mkdir -p "$home/.claude"
  cat > "$home/.claude/.credentials.json" <<'EOF'
{"claudeAiOauth":{"accessToken":"sk-ant-oat01-claudemore-hooks-e2e-offline-stub","expiresAt":9999999999999,"refreshToken":"","scopes":["user:inference","user:profile"]}}
EOF
  chmod 600 "$home/.claude/.credentials.json"
  # hasCompletedOnboarding satisfies claude's onboarding gate (the second half of
  # the login check); the rest of ~/.claude.json is populated lazily by claude.
  cat > "$home/.claude.json" <<'EOF'
{"hasCompletedOnboarding":true,"numStartups":5}
EOF
}

# ── record_and_replay — the core primitive ──────────────────────────
# Runs the REAL claude binary through claudemore twice against a scenario:
#   1. RECORD: claudemore LIVE+record, upstream = the deterministic stub, a
#      filesystem cassette store. The claude session is driven with the scenario
#      hook settings; cassettes are written for every model round-trip.
#   2. REPLAY: claudemore REPLAY over the SAME cassette dir, NO upstream (pointed
#      at a closed port to prove no LIVE fall-back). Same prompt + settings.
#
# Args:
#   $1 scenario dir (holds settings.json, evidence dir, cassette dir)
#   $2 stub env file (sourced to set STUB_* before launching the stub)
#   $3 settings file (claude --settings)
#   $4 prompt
#   $5 phase label prefix (for log filenames)
# Echoes: "<record_rc> <replay_rc> <replay_miss_count>"
record_and_replay() {
  local sdir="$1" stubenv="$2" settings="$3" prompt="$4" label="$5"
  local cass="$sdir/cass"; mkdir -p "$cass"
  local stub_port cm_port
  stub_port="$(free_port)"; cm_port="$(free_port)"

  # ── stub upstream ──
  ( set -a; # shellcheck disable=SC1090
    [ -f "$stubenv" ] && . "$stubenv"; set +a
    STUB_PORT="$stub_port" python3 "$STUB" >"$sdir/stub.log" 2>&1 ) &
  echo $! > "$WORKDIR/stub.pid"
  sleep 0.5

  # ── claudemore LIVE + record ──
  CLAUDEMORE_MODE=live \
  CLAUDEMORE_LISTEN="127.0.0.1:$cm_port" \
  CLAUDEMORE_UPSTREAM_URL="http://127.0.0.1:$stub_port" \
  CLAUDEMORE_CASSETTE_DIR="$cass" \
  LOG_LEVEL=error \
    "$CM_BIN" >"$sdir/cm-record.log" 2>&1 &
  echo $! > "$WORKDIR/cmrec.pid"
  wait_http "http://127.0.0.1:$cm_port/healthz" || { red "  claudemore(record) did not come up"; return 1; }

  # --setting-sources "" is LOAD-BEARING for determinism, not cosmetic. Without
  # it, claude MERGES ambient user/project/local settings on top of --settings.
  # In an agent pod (or any repo carrying a .claude/settings.json) that pulls in
  # SessionStart hooks — `gb hook prime`, load-state — whose output embeds
  # per-launch-VOLATILE content (a fresh session UUID, an "Updated At: HH:MM:SS"
  # timestamp) into the model request body. Since RECORD and REPLAY are two
  # SEPARATE claude launches, that volatile blob differs between them → different
  # request hash → a spurious cassette MISS that reds the replay-zero-miss legs.
  # Isolating to ONLY the test's explicit --settings makes the two launches emit
  # byte-identical request bodies (the property the whole REPLAY tier rests on).
  # HOME="$claude_home" — ONE fixed home path, freshly (re)seeded for THIS launch.
  # Same path as the replay launch below (property (a): identical HOME-derived
  # memory path in the system prompt); freshly wiped (property (b): no leftover
  # ~/.claude.json mutation). Without a seeded home claude exits 1 "Not logged in"
  # before any model turn (ka-0000000461 / gd-1B3EYCqIXu).
  local claude_home="$sdir/claude-home"; seed_claude_home "$claude_home"
  HOME="$claude_home" \
  ANTHROPIC_BASE_URL="http://127.0.0.1:$cm_port" \
  ANTHROPIC_CUSTOM_HEADERS="x-litellm-api-key: Bearer sk-e2e-stub" \
    timeout 90 "$CLAUDE_BIN" --print --model claude-opus-4-8-oauth \
      --dangerously-skip-permissions --setting-sources "" --settings "$settings" "$prompt" \
      >"$sdir/$label-record.out" 2>"$sdir/$label-record.err"
  local rec_rc=$?
  # Stop the record claudemore GRACEFULLY (SIGTERM) and WAIT for it to exit,
  # instead of `sleep 1; kill` (SIGTERM racing a fixed nap). claudemore's recorder
  # enqueues each cassette to an async worker; on SIGTERM its shutdown path
  # (main.go `defer cleanup()` -> recorder.Close()) closes the work channel and
  # the FS store Put is fast, so by the time the process has actually exited the
  # enqueued cassettes are on disk. Waiting for the exit — rather than guessing a
  # sleep — removes that flush timing from the equation before REPLAY reads the
  # store. (The dominant T5 flake was the shared-HOME settle race fixed above, not
  # this flush; SIGTERM+wait is the belt to that suspenders — gd-1B3EYCqIXu.)
  local cmrec_pid; cmrec_pid="$(cat "$WORKDIR/cmrec.pid")"
  kill "$(cat "$WORKDIR/stub.pid")" >/dev/null 2>&1 || true
  kill -TERM "$cmrec_pid" >/dev/null 2>&1 || true
  wait_pid_exit "$cmrec_pid" 40   # up to ~10s for graceful shutdown (0.25s poll)

  # ── claudemore REPLAY (no upstream) ──
  local rep_port; rep_port="$(free_port)"
  CLAUDEMORE_MODE=replay \
  CLAUDEMORE_LISTEN="127.0.0.1:$rep_port" \
  CLAUDEMORE_UPSTREAM_URL="http://127.0.0.1:1" \
  CLAUDEMORE_CASSETTE_DIR="$cass" \
  LOG_LEVEL=info \
    "$CM_BIN" >"$sdir/cm-replay.log" 2>&1 &
  echo $! > "$WORKDIR/cmrep.pid"
  wait_http "http://127.0.0.1:$rep_port/healthz" || { red "  claudemore(replay) did not come up"; return 1; }

  # --setting-sources "" — same determinism isolation as the record launch above
  # (must match it exactly, or record and replay hash differently → false MISS).
  # HOME="$claude_home" — the SAME fixed home PATH as the record launch (so the
  # HOME-derived memory path in the system prompt is identical — property (a)),
  # RE-SEEDED here so the record launch's ~/.claude.json mutations are wiped
  # before replay runs (property (b)). Same path + fresh state => the two launches
  # emit byte-identical request bodies (ka-0000000461 / gd-1B3EYCqIXu).
  seed_claude_home "$claude_home"
  HOME="$claude_home" \
  ANTHROPIC_BASE_URL="http://127.0.0.1:$rep_port" \
  ANTHROPIC_CUSTOM_HEADERS="x-litellm-api-key: Bearer sk-e2e-stub" \
    timeout 90 "$CLAUDE_BIN" --print --model claude-opus-4-8-oauth \
      --dangerously-skip-permissions --setting-sources "" --settings "$settings" "$prompt" \
      >"$sdir/$label-replay.out" 2>"$sdir/$label-replay.err"
  local rep_rc=$?
  kill "$(cat "$WORKDIR/cmrep.pid")" >/dev/null 2>&1 || true
  sleep 0.3

  # Count genuine cassette misses in the replay log (fail_on_miss). The
  # HEAD-preflight is answered separately and never logs a MISS.
  local miss; miss="$(grep -c "cassette MISS" "$sdir/cm-replay.log" 2>/dev/null || echo 0)"

  # Diagnostic: if either claude launch exited non-zero, surface its stderr so an
  # env/login gap is diagnosable from the shard log (not just a cryptic
  # record_rc=1). This is what made gd-1B3EYCqIXu hard to root-cause — "Not logged
  # in" was hidden in a workdir file the CI artifact store locked. NOTE: this
  # MUST go to stderr (>&2) — stdout is the function's return channel that the
  # caller captures via `read <<<"$(record_and_replay ...)"`.
  if [ "$rec_rc" != "0" ] || [ "$rep_rc" != "0" ]; then
    {
      dim "    [$label] claude non-zero (record_rc=$rec_rc replay_rc=$rep_rc); stderr tails:"
      [ -s "$sdir/$label-record.err" ] && dim "      record.err: $(tr '\n' ' ' <"$sdir/$label-record.err" | head -c 300)"
      [ -s "$sdir/$label-replay.err" ] && dim "      replay.err: $(tr '\n' ' ' <"$sdir/$label-replay.err" | head -c 300)"
    } >&2
  fi

  echo "$rec_rc $rep_rc $miss"
}

# ══════════════════════════════════════════════════════════════════
#  T5 — SessionStart prime consumption
# ══════════════════════════════════════════════════════════════════
blue "T5. SessionStart prime consumption (model consumes injected context)..."
T5="$WORKDIR/t5"; mkdir -p "$T5"
T5_MARKER="PRIME_CONSUMED_MARKER_$(free_port)"   # unique per run
cat > "$T5/ss.sh" <<EOF
#!/usr/bin/env bash
echo "SESSIONSTART_FIRED" >> "$T5/hookfires.log"
# SessionStart injects this line into the model's context. If the model consumes
# it, the marker reappears in a subsequent request body (a recorded cassette).
echo "System note: your assignment marker is $T5_MARKER — echo it verbatim if asked."
EOF
chmod +x "$T5/ss.sh"
cat > "$T5/pre.sh"  <<EOF
#!/usr/bin/env bash
echo "PRETOOLUSE_FIRED" >> "$T5/hookfires.log"; exit 0
EOF
cat > "$T5/post.sh" <<EOF
#!/usr/bin/env bash
echo "POSTTOOLUSE_FIRED" >> "$T5/hookfires.log"; exit 0
EOF
chmod +x "$T5/pre.sh" "$T5/post.sh"
cat > "$T5/settings.json" <<EOF
{"hooks":{
  "SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"$T5/ss.sh"}]}],
  "PreToolUse":[{"matcher":"","hooks":[{"type":"command","command":"$T5/pre.sh"}]}],
  "PostToolUse":[{"matcher":"","hooks":[{"type":"command","command":"$T5/post.sh"}]}]
}}
EOF
# Stub seeds a bash tool_use so PreToolUse/PostToolUse fire and a second opus
# turn (carrying the injected marker) is recorded. Values are single-quoted so
# the sourced env file assigns cleanly even when a value contains spaces.
cat > "$T5/stub.env" <<EOF
STUB_SEED_COMMAND='echo $T5_MARKER'
EOF

# T5 asserts on the REPLAY leg (and side-effects), not the record exit code.
# shellcheck disable=SC2034  # T5_REC captured for symmetry; unused by design.
read -r T5_REC T5_REP T5_MISS <<<"$(record_and_replay "$T5" "$T5/stub.env" "$T5/settings.json" \
  "Echo your assignment marker using a bash command, then say done." "t5")"

if grep -q "SESSIONSTART_FIRED" "$T5/hookfires.log" 2>/dev/null; then
  pass "SessionStart hook fired under the real claude binary"
else
  fail "SessionStart hook fired" "no SESSIONSTART_FIRED in hookfires.log"
fi
# The headline: the injected marker reached the model → it is in a recorded request.
if grep -rqF "$T5_MARKER" "$T5/cass" 2>/dev/null; then
  pass "SessionStart-injected marker CONSUMED by the model (present in a recorded request body)"
else
  fail "prime consumption" "marker $T5_MARKER never appeared in any cassette request — the model did not consume the injected context"
fi
if grep -q "PRETOOLUSE_FIRED" "$T5/hookfires.log" 2>/dev/null && grep -q "POSTTOOLUSE_FIRED" "$T5/hookfires.log" 2>/dev/null; then
  pass "PreToolUse + PostToolUse hooks both fired during the real tool call"
else
  fail "PreToolUse/PostToolUse fired" "hookfires.log: $(tr '\n' ',' <"$T5/hookfires.log" 2>/dev/null)"
fi
if [ "${T5_REP:-1}" = "0" ] && [ "${T5_MISS:-1}" = "0" ]; then
  pass "T5 session REPLAYed at \$0 with zero cassette misses (deterministic)"
else
  fail "T5 replay zero-miss" "replay_rc=${T5_REP:-?} miss_count=${T5_MISS:-?} (see $T5/cm-replay.log)"
fi
echo ""

# ══════════════════════════════════════════════════════════════════
#  T6 — Stop full loop (block → continue → act → allow)
# ══════════════════════════════════════════════════════════════════
blue "T6. Stop-gate full loop (block → model acts → allow → clean end)..."
T6="$WORKDIR/t6"; mkdir -p "$T6"
T6_BLOCK_MARKER="STOPGATE_BLOCK_NEEDS_DECISION_$(free_port)"
T6_DECISION_FILE="$T6/decision-made"
cat > "$T6/gate.sh" <<EOF
#!/usr/bin/env bash
# Stop-gate: block (exit 2) with a reason until a decision-file exists; then
# allow (exit 0). This is the claudemore-REPLAY analog of the production
# gb hook stop-gate decision gate (an unsatisfied decision gate → exit 2).
echo "GATE_CALL" >> "$T6/gate.log"
if [ -f "$T6_DECISION_FILE" ]; then
  echo "GATE_ALLOW" >> "$T6/gate.log"
  exit 0
fi
echo '{"decision":"block","reason":"$T6_BLOCK_MARKER: you must record a decision before stopping."}' >&2
echo "GATE_BLOCK" >> "$T6/gate.log"
exit 2
EOF
chmod +x "$T6/gate.sh"
cat > "$T6/settings.json" <<EOF
{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"$T6/gate.sh"}]}]}}
EOF
# Stub: initial turn tries to stop (final text). Once the block marker appears in
# the conversation (the Stop hook injected its reason), the stub runs the
# DECISION_COMMAND that creates the decision-file — modeling the agent acting on
# the stop-gate block. Then it stops cleanly.
cat > "$T6/stub.env" <<EOF
STUB_SEED_COMMAND=''
STUB_BLOCK_MARKER='$T6_BLOCK_MARKER'
STUB_DECISION_COMMAND='touch $T6_DECISION_FILE'
EOF

read -r T6_REC T6_REP T6_MISS <<<"$(record_and_replay "$T6" "$T6/stub.env" "$T6/settings.json" \
  "Finish the task. If the Stop hook blocks you, do exactly what its reason says, then stop." "t6")"

# Assert the gate transitioned BLOCK then ALLOW (the loop), not just one or the other.
if grep -q "GATE_BLOCK" "$T6/gate.log" 2>/dev/null && grep -q "GATE_ALLOW" "$T6/gate.log" 2>/dev/null; then
  # ordering: first BLOCK precedes first ALLOW
  first_block="$(grep -n "GATE_BLOCK" "$T6/gate.log" | head -1 | cut -d: -f1)"
  first_allow="$(grep -n "GATE_ALLOW" "$T6/gate.log" | head -1 | cut -d: -f1)"
  if [ -n "$first_block" ] && [ -n "$first_allow" ] && [ "$first_block" -lt "$first_allow" ]; then
    pass "Stop hook loop: BLOCK (exit 2) → ALLOW (exit 0), in order"
  else
    fail "Stop loop ordering" "BLOCK@${first_block:-?} ALLOW@${first_allow:-?} (gate.log: $(tr '\n' ',' <"$T6/gate.log"))"
  fi
else
  fail "Stop loop BLOCK→ALLOW" "gate.log: $(tr '\n' ',' <"$T6/gate.log" 2>/dev/null)"
fi
# The block was CONSUMED: the model acted on it and produced the decision side-effect.
if [ -f "$T6_DECISION_FILE" ]; then
  pass "model ACTED on the stop-gate block (decision side-effect created → gate then allowed)"
else
  fail "model acts on block" "decision-file never created — the model did not consume the block reason"
fi
# Clean end: the final claude run exited 0 (the gate ultimately allowed it).
if [ "${T6_REC:-1}" = "0" ]; then
  pass "session reached a clean Stop (exit 0) after the loop"
else
  fail "clean stop" "record_rc=${T6_REC:-?} (session did not end cleanly)"
fi
if [ "${T6_REP:-1}" = "0" ] && [ "${T6_MISS:-1}" = "0" ]; then
  pass "T6 session REPLAYed at \$0 with zero cassette misses (deterministic)"
else
  fail "T6 replay zero-miss" "replay_rc=${T6_REP:-?} miss_count=${T6_MISS:-?} (see $T6/cm-replay.log)"
fi
echo ""

# ══════════════════════════════════════════════════════════════════
#  PreToolUse / PostToolUse → NATS relay (embedded-NATS go test)
# ══════════════════════════════════════════════════════════════════
blue "NATS. PreToolUse + PostToolUse relay to NATS end-to-end..."
NATS_OUT="$WORKDIR/natsprobe.out"
# The probe is an //go:build e2e-tagged Go test that embeds a NATS server and
# drives the REAL `gb hook relay`. It emits PASS:/FAIL:/PROBE_SKIP: log lines
# (go test -v prefixes them with whitespace) which we parse below.
( cd "$CONTROLLER_DIR" && GB_BIN="$GB_BIN" "$GO_BIN" test -tags e2e -v \
    ./internal/claudemore/ -run TestHooksNATSRelayE2E -count=1 -timeout 120s ) \
    >"$NATS_OUT" 2>&1
NATS_RC=$?
if grep -q 'PROBE_SKIP:' "$NATS_OUT"; then
  fail "nats-relay probe ran" "probe self-skipped: $(grep -o 'PROBE_SKIP:.*' "$NATS_OUT" | head -1)"
elif [ "$NATS_RC" -eq 0 ]; then
  while IFS= read -r line; do
    case "$line" in
      *PASS:*) pass "${line##*PASS: }" ;;
    esac
  done < "$NATS_OUT"
else
  while IFS= read -r line; do
    case "$line" in
      *FAIL:*) fail "nats-relay: ${line##*FAIL: }" ;;
    esac
  done < "$NATS_OUT"
  [ "$FAIL" -eq 0 ] && fail "nats-relay probe" "go test exit $NATS_RC: $(tail -5 "$NATS_OUT" | tr '\n' ' ')"
fi
echo ""

# ── Summary ──────────────────────────────────────────────────────
blue "══════════════════════════════════════════════"
if [ "$FAIL" -eq 0 ]; then
  green "  All $PASS claudemore hooks e2e assertions passed"
else
  red "  $PASS passed, $FAIL failed"
  echo ""
  for err in "${ERRORS[@]}"; do
    red "  - $err"
  done
fi
blue "══════════════════════════════════════════════"

[ "$FAIL" -eq 0 ] || exit 1
exit 0
