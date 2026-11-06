#!/usr/bin/env bash
# test-claudemore-agentchoice.sh — claudemore agent-choice e2e (REPLAY tier)
#
# Bead ka-0000005944 (Phase 2 §T2/T3/T4, epic ka-0000001159). Sibling of the
# hooks e2e (test-claudemore-hooks.sh, ka-0000004452): both drive the REAL
# `claude` binary through claudemore so the suite can assert REAL-REASONING
# behaviours the claudeless (faked-binary) tier structurally cannot. This module
# owns the three *agent-choice* failure classes from the R1 report
# (docs/research/claudemore-r1-why-claudemore.md §3):
#
#   T2  right-bead selection      (R1 class 5) — given a `gb ready` listing with
#                                 decoys, the real model reasons and runs a REAL
#                                 `kd claim` on the CORRECT bead. claudeless uses
#                                 `echo 'Simulated claim…'`; the parity doc §3.1
#                                 explicitly DROPs "picks the right bead".
#   T3  decision divergence       (R1 class 1) — the real model files a REAL
#                                 `gb decision` ONLY when options genuinely
#                                 diverge; a non-divergent (status-update) prompt
#                                 produces NO decision. The "options must
#                                 diverge / no daily check-in" rule is prose to
#                                 the model, enforced nowhere in code (the server
#                                 guard only rejects zero-option/single-ack).
#   T4  wrap-up blockers judgment (R1 class 3) — on a clean run the real model
#                                 OMITS the `blockers` field, so the derived card
#                                 outcome is `done`, not ⛔ `blocked`. claudeless's
#                                 unit test bakes in `blockers:"none"` as valid.
#
# ── WHY THIS IS SEPARATE FROM THE HOOKS E2E ───────────────────────────────────
#
# The hooks e2e records the model against a DETERMINISTIC STUB upstream: its legs
# assert claude's real *hook plumbing* (context injection, the Stop-gate loop),
# so a scripted model is fine and the whole thing is $0 even at record time.
#
# The agent-choice legs assert the model's *semantic choice* — which bead, whether
# to fork, whether to omit blockers. A scripted upstream would make those
# assertions circular (they would only prove the stub emitted what we scripted).
# So this module RECORDS against the REAL model (real litellm) to exercise real
# reasoning, then REPLAYS the recorded cassettes in the same run to PROVE the $0
# / byte-identical determinism property (T1) the CI tier rests on. Because a real
# record costs real tokens, this module is OPT-IN (CLAUDEMORE_AGENTCHOICE_E2E=1)
# and SKIPS CLEANLY otherwise — matching the synthesis Target-rung placement:
# "scheduled-tier, non-blocking; never a per-PR/main-push hard gate" (§T7,
# R4 §3). The scheduled tier sets the opt-in; the drift canary (T7) is what keeps
# the recorded behaviour honest over time.
#
# Assertions target OUTCOMES/side-effects — the bead-state mutation the real
# `kd`/`gb` binaries wrote (captured by a stub beads daemon), the decision bead's
# options, the wrap-up's persisted fields — NEVER model text (R4-DET-1). The
# model's prose varies run to run; the bead writes are what we pin.
#
# ── EPHEMERAL CASSETTES (no daily staleness) ──────────────────────────────────
#
# The opus system prompt embeds "Today's date is <date>", which is inside
# claudemore's hashed request body — a checked-in cassette would MISS the next
# day (fail_on_miss → daily-red CI; verified on this bead). So each scenario
# RECORDS then REPLAYS within the SAME run (same clock → identical hashes).
# Cassettes are ephemeral temp files; nothing is committed and nothing drifts.
# The filesystem cassette store (CLAUDEMORE_CASSETTE_DIR, ka-0000004452) is what
# makes this hermetic — CI has no S3/MinIO.
#
# ── DETERMINISM CHECKLIST (adding a scenario) ─────────────────────────────────
#
# Replay serves cassettes keyed by the request hash, so the record and replay
# phases MUST send byte-identical request bodies. claude threads a lot of
# environment into those bodies (tool outputs, cwd, session id), so any volatile
# token a tool prints and the model then re-reads breaks the hash. The isolations
# that keep this deterministic (all handled by the harness — see drive_claude and
# record_and_replay_agentchoice): a clean cwd (no repo hooks), a fixed
# --session-id + shared HOME path (stable gb-stop fsync line), a project-labelled
# agent seed (no timestamped advice warn), DISABLE_NONESSENTIAL_TRAFFIC (no haiku
# race), and REUSED daemon+claudemore ports across phases (gb --help prints the
# --http-url default WITH its port). If you add a scenario whose prompt leads the
# model to run a command that emits a WALL-CLOCK time, a RANDOM id, a PORT, or a
# filesystem mtime/size (`ls -l`, `date`, `ps`, …), expect a replay MISS — prefer
# prompts that drive the specific kd/gb action under test and assert its bead
# side-effect, not free-form exploration.
#
# ── STUB BEADS DAEMON (assert real bead writes without Postgres) ──────────────
#
# The real beads daemon is Postgres-backed; a hermetic shard cannot stand one up.
# fixtures/claudemore-agentchoice/stub_beads_daemon.py serves the endpoints
# kd/gb touch, records every write to a JSONL log, and emulates the decision
# divergence guard so T3's non-divergent leg fails through the REAL binary
# exactly as prod would. kd/gb are pointed at it via BEADS_HTTP_URL; the agent's
# identity/coop endpoints are pointed at synthetic ids so `gb stop` never touches
# a real agent bead or requests a real coop shutdown.
#
# ── SKIP-CLEAN CONTRACT ───────────────────────────────────────────────────────
#
# Requires the opt-in AND: the `claude` binary, a Go toolchain (to build
# claudemore + gb + kd from source), python3, curl, and a reachable model
# upstream (ANTHROPIC_BASE_URL — real litellm). If the opt-in is unset or ANY
# prerequisite is missing the module SKIPS CLEANLY (exit 0) with a LOUD banner —
# an absent toolchain or a locked-down offline shard must never red the suite
# (mirrors test-claudemore-hooks / test-gvisor-freeze-thaw). When everything is
# present, every leg is a HARD assertion.
#
# Standalone runnable:
#   CLAUDEMORE_AGENTCHOICE_E2E=1 ./gardenio/tests/e2e/scripts/test-claudemore-agentchoice.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# scripts/ -> e2e/ -> tests/ -> gardenio/ -> controller
CONTROLLER_DIR="$(cd "$SCRIPT_DIR/../../../controller" 2>/dev/null && pwd)" || CONTROLLER_DIR=""
# repo root -> kbeads (kd lives there, a sibling Go module)
KBEADS_DIR="$(cd "$SCRIPT_DIR/../../../../kbeads" 2>/dev/null && pwd)" || KBEADS_DIR=""
FIXTURE_DIR="$SCRIPT_DIR/../fixtures/claudemore-agentchoice"
STUB_DAEMON="$FIXTURE_DIR/stub_beads_daemon.py"

CLAUDE_BIN="${CLAUDE_BIN:-claude}"
GO_BIN="${GO_BIN:-go}"
MODEL="${CLAUDEMORE_E2E_MODEL:-claude-opus-4-8-oauth}"
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/claudemore-agentchoice.XXXXXX")"
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
  blue "SKIP claudemore-agentchoice: $1"
  blue "  The claudemore agent-choice e2e drives the REAL claude binary against"
  blue "  claudemore, recording REAL reasoning (real tokens) then replaying at \$0."
  blue "  Skipping cleanly (exit 0) — the opt-in/toolchain/upstream gates must"
  blue "  never red the suite (this is a scheduled-tier, non-gating module — §T7)."
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

cleanup() {
  for pidf in "$WORKDIR"/*.pid; do
    [ -f "$pidf" ] || continue
    local p; p="$(cat "$pidf" 2>/dev/null)"
    [ -n "$p" ] && kill "$p" >/dev/null 2>&1 || true
  done
  rm -rf "$WORKDIR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

wait_http() {
  local url="$1" tries="${2:-40}"
  for _ in $(seq 1 "$tries"); do
    if curl -s -o /dev/null -m 1 "$url" 2>/dev/null; then return 0; fi
    sleep 0.25
  done
  return 1
}

# ── Preflight ───────────────────────────────────────────────────────
blue "╔══════════════════════════════════════════════════════════╗"
blue "║  claudemore agent-choice e2e (ka-0000005944) — REPLAY tier ║"
blue "╚══════════════════════════════════════════════════════════╝"
echo ""

# Opt-in gate FIRST: this module records real reasoning (real tokens), so it is
# never part of the default per-PR fan-out. The scheduled tier sets the opt-in.
if [ "${CLAUDEMORE_AGENTCHOICE_E2E:-}" != "1" ]; then
  skip_clean "opt-in unset (set CLAUDEMORE_AGENTCHOICE_E2E=1). Records real reasoning; scheduled-tier only."
fi

command -v "$CLAUDE_BIN" >/dev/null 2>&1 || skip_clean "'$CLAUDE_BIN' binary not on PATH"
command -v "$GO_BIN"     >/dev/null 2>&1 || skip_clean "'$GO_BIN' toolchain not on PATH"
command -v python3       >/dev/null 2>&1 || skip_clean "python3 not on PATH"
command -v curl          >/dev/null 2>&1 || skip_clean "curl not on PATH"
[ -n "$CONTROLLER_DIR" ] && [ -f "$CONTROLLER_DIR/go.mod" ] || skip_clean "controller module not found (looked at '${CONTROLLER_DIR:-<unresolved>}')"
[ -n "$KBEADS_DIR" ] && [ -f "$KBEADS_DIR/go.mod" ] || skip_clean "kbeads module not found (looked at '${KBEADS_DIR:-<unresolved>}')"
[ -f "$STUB_DAEMON" ] || skip_clean "stub beads daemon fixture not found at $STUB_DAEMON"
UPSTREAM="${CLAUDEMORE_UPSTREAM_URL:-${ANTHROPIC_BASE_URL:-}}"
[ -n "$UPSTREAM" ] || skip_clean "no model upstream (set ANTHROPIC_BASE_URL / CLAUDEMORE_UPSTREAM_URL) — record needs a reachable litellm"

dim "  claude:     $(command -v "$CLAUDE_BIN")  ($("$CLAUDE_BIN" --version 2>/dev/null | head -1))"
dim "  go:         $(command -v "$GO_BIN")  ($("$GO_BIN" version 2>/dev/null))"
dim "  model:      $MODEL"
dim "  upstream:   $UPSTREAM"
dim "  workdir:    $WORKDIR"
echo ""

# ── Build claudemore + gb + kd from source ──────────────────────────
blue "Building claudemore + gb + kd from source..."
CM_BIN="$WORKDIR/claudemore"
GB_BIN="$WORKDIR/gb"
KD_BIN="$WORKDIR/kd"
BUILD_LOG="$WORKDIR/build.log"
if ! ( cd "$CONTROLLER_DIR" \
       && CGO_ENABLED=0 "$GO_BIN" build -o "$CM_BIN" ./cmd/claudemore/ \
       && CGO_ENABLED=0 "$GO_BIN" build -o "$GB_BIN" ./cmd/gb/ ) >"$BUILD_LOG" 2>&1 \
   || ! ( cd "$KBEADS_DIR" \
       && CGO_ENABLED=0 "$GO_BIN" build -o "$KD_BIN" ./cmd/kd/ ) >>"$BUILD_LOG" 2>&1; then
  if grep -qiE "cannot find module|dial tcp|no such host|timeout|connection refused|proxy" "$BUILD_LOG"; then
    skip_clean "go build could not fetch deps (offline shard): $(tail -3 "$BUILD_LOG" | tr '\n' ' ')"
  fi
  red "  ✗ build failed:"
  dim "$(tail -15 "$BUILD_LOG")"
  exit 1
fi
green "  ✓ built claudemore + gb + kd"
echo ""

# Minimal claude settings: NO hooks. This isolates the driven `claude` from the
# host pod's real SessionStart/Stop-gate/relay hooks — otherwise the real
# stop-gate would block the model's Stop and loop it (learned building this
# bead). The agent-choice legs exercise the model's CHOICE, not hook plumbing
# (that is the hooks e2e's job), so an empty hook set is correct here.
EMPTY_SETTINGS="$WORKDIR/settings.json"
echo '{}' > "$EMPTY_SETTINGS"

# The driven claude/gb run under a FRESH isolated HOME per invocation (see
# make_iso_home + drive_claude's isolation #2). REAL_CLAUDE_DIR is the account's
# ~/.claude — the auth source we symlink into each fresh HOME.
REAL_CLAUDE_DIR="${HOME:-/root}/.claude"
REAL_CLAUDE_JSON="${HOME:-/root}/.claude.json"
if [ ! -d "$REAL_CLAUDE_DIR" ]; then
  dim "  note: real ~/.claude not found — auth isolation degraded (T4 replay hash-stability may vary)"
fi

# make_iso_home <dir> — build a fresh isolated HOME at <dir>. Auth artifacts
# (~/.claude/* incl. hidden .credentials.json, and ~/.claude.json login state)
# are symlinked so the driven claude authenticates against the SAME account; but
# ~/.claude/projects is a FRESH EMPTY dir and history.jsonl is a private COPY, so
# NEITHER the record nor the replay phase can see the other's transcripts. This
# is load-bearing for hash-stability: claude threads prior-session transcript
# context into the request body, and `gb stop`'s syncAgentSessions prints one
# "fsynced <path>" line per transcript file (paths carry volatile session UUIDs)
# into stdout the model then re-reads — so a SHARED projects dir makes the two
# phases diverge and MISS on replay (the T4 miss chased while building this bead).
make_iso_home() {
  local dir="$1"
  rm -rf "$dir"; mkdir -p "$dir/.claude/projects"
  if [ -d "$REAL_CLAUDE_DIR" ]; then
    for e in "$REAL_CLAUDE_DIR"/* "$REAL_CLAUDE_DIR"/.[!.]*; do
      [ -e "$e" ] || continue
      local b; b="$(basename "$e")"
      case "$b" in
        projects|history.jsonl) continue ;;  # per-run, not shared
      esac
      [ -e "$dir/.claude/$b" ] || ln -s "$e" "$dir/.claude/$b"
    done
    # history.jsonl: a private empty copy (claude appends to it — must not be shared/symlinked)
    : > "$dir/.claude/history.jsonl"
    [ -e "$REAL_CLAUDE_JSON" ] && ln -s "$REAL_CLAUDE_JSON" "$dir/.claude.json"
  fi
}

# ── record_and_replay_agentchoice — the core primitive ──────────────
# Runs the REAL claude binary through claudemore twice against a scenario, with
# kd/gb pointed at a per-scenario stub beads daemon that captures every write:
#   1. RECORD: claudemore LIVE+record, upstream = REAL litellm, FS cassette store.
#      The model reasons for real; kd/gb writes land in the stub daemon's log.
#   2. REPLAY: claudemore REPLAY over the SAME cassette dir, upstream pointed at a
#      closed port to PROVE no LIVE fall-back. Same prompt + settings + a FRESH
#      stub daemon (fresh write log), so replay is a true $0 re-run.
#
# Args:
#   $1 scenario dir
#   $2 prompt
#   $3 seed-beads JSON (STUB_SEED_BEADS for the stub daemon)
#   $4 agent bead id (KD_AGENT_ID — synthetic, so gb stop never hits a real one)
#   $5 label
# Side effects: writes $sdir/record-writes.jsonl and $sdir/replay-writes.jsonl.
# Echoes: "<record_rc> <replay_rc> <replay_miss_count>"
record_and_replay_agentchoice() {
  local sdir="$1" prompt="$2" seed="$3" agent_id="$4" label="$5"
  local cass="$sdir/cass"; mkdir -p "$cass"
  # Stable clean cwd for BOTH record and replay: claude embeds the cwd in the
  # request body, so a per-phase cwd would change the hash and miss on replay.
  local cleancwd="$sdir/agent-cwd"; mkdir -p "$cleancwd"
  # A FIXED session id AND a SHARED HOME PATH, both stable across record+replay
  # (drive_claude isolation #3) so gb stop's fsync line — which embeds the
  # transcript name (session id) and the HOME dir — is byte-identical across
  # phases. Derived deterministically from the workdir + label so unique per
  # scenario but stable within one.
  local session_id iso_home
  session_id="$(printf '%s-%s' "$(basename "$WORKDIR")" "$label" | md5sum | cut -c1-32 \
    | sed -E 's/(.{8})(.{4})(.{4})(.{4})(.{12})/\1-\2-\3-\4-\5/')"
  iso_home="$sdir/agent-home"

  # Allocate the daemon + claudemore ports ONCE and reuse them for BOTH phases.
  # This is load-bearing for determinism: `gb`'s --help output embeds the
  # --http-url default (= the stub daemon URL, BEADS_HTTP_URL) WITH its port. If
  # the model runs any `gb … --help` mid-run, that port lands in a tool_result
  # the next turn hashes — so a per-phase random port makes record and replay
  # diverge and MISS (an intermittent flake chased on this bead). Record's
  # servers are torn down before replay binds, so reusing the ports is safe.
  local dport cmport
  dport="$(free_port)"; cmport="$(free_port)"

  # ── phase 1: RECORD against real litellm ──
  STUB_SEED_BEADS="$seed" python3 "$STUB_DAEMON" "$dport" "$sdir/record-writes.jsonl" \
    >"$sdir/daemon-record.log" 2>&1 &
  echo $! > "$WORKDIR/daemon.pid"

  CLAUDEMORE_MODE=live \
  CLAUDEMORE_LISTEN="127.0.0.1:$cmport" \
  CLAUDEMORE_UPSTREAM_URL="$UPSTREAM" \
  CLAUDEMORE_CASSETTE_DIR="$cass" \
  LOG_LEVEL=error \
    "$CM_BIN" >"$sdir/cm-record.log" 2>&1 &
  echo $! > "$WORKDIR/cmrec.pid"
  wait_http "http://127.0.0.1:$cmport/healthz" || { red "  claudemore(record) did not come up"; return 1; }
  wait_http "http://127.0.0.1:$dport/v1/beads/$agent_id" || { red "  stub daemon(record) did not come up"; return 1; }

  drive_claude "$cmport" "$dport" "$agent_id" "$prompt" "$sdir/$label-record" "$cleancwd" "$session_id" "$iso_home"
  local rec_rc=$?
  sleep 1  # let the async recorder flush its cassette Put
  kill "$(cat "$WORKDIR/daemon.pid")" "$(cat "$WORKDIR/cmrec.pid")" >/dev/null 2>&1 || true
  sleep 0.5  # ensure the ports are released before the replay phase rebinds them

  # ── phase 2: REPLAY (no upstream) — SAME ports as record (see above) ──
  local rport rdport
  rport="$cmport"; rdport="$dport"
  STUB_SEED_BEADS="$seed" python3 "$STUB_DAEMON" "$rdport" "$sdir/replay-writes.jsonl" \
    >"$sdir/daemon-replay.log" 2>&1 &
  echo $! > "$WORKDIR/daemonr.pid"

  CLAUDEMORE_MODE=replay \
  CLAUDEMORE_LISTEN="127.0.0.1:$rport" \
  CLAUDEMORE_UPSTREAM_URL="http://127.0.0.1:1" \
  CLAUDEMORE_CASSETTE_DIR="$cass" \
  LOG_LEVEL=info \
    "$CM_BIN" >"$sdir/cm-replay.log" 2>&1 &
  echo $! > "$WORKDIR/cmrep.pid"
  wait_http "http://127.0.0.1:$rport/healthz" || { red "  claudemore(replay) did not come up"; return 1; }
  wait_http "http://127.0.0.1:$rdport/v1/beads/$agent_id" || { red "  stub daemon(replay) did not come up"; return 1; }

  drive_claude "$rport" "$rdport" "$agent_id" "$prompt" "$sdir/$label-replay" "$cleancwd" "$session_id" "$iso_home"
  local rep_rc=$?
  kill "$(cat "$WORKDIR/cmrep.pid")" "$(cat "$WORKDIR/daemonr.pid")" >/dev/null 2>&1 || true
  sleep 0.3

  local miss; miss="$(grep -c "cassette MISS" "$sdir/cm-replay.log" 2>/dev/null || echo 0)"
  echo "$rec_rc $rep_rc $miss"
}

# drive_claude — run the real binary through claudemore($1) with kd/gb pointed at
# the stub daemon($2), as synthetic agent id($3), on prompt($4), logging to $5.*
#
# Two isolations make the record→replay hash-stable and prod-safe:
#
#  1. CLEAN cwd (a fresh temp dir with no .claude/). claude loads PROJECT
#     settings (.claude/settings.json) from the current directory and MERGES them
#     with --settings, so running from the repo root would inherit the repo's
#     real Stop-gate/SessionStart/relay hooks — the real `gb hook stop-gate`
#     would then block the model's Stop until a decision exists and loop it
#     forever (the T4 hang seen while building this bead). A clean cwd + empty
#     --settings gives the driven claude NO hooks, which is what these
#     agent-choice legs want (they exercise the model's CHOICE, not hook
#     plumbing — that is the hooks e2e's job).
#
#  2. FRESH ISOLATED HOME per invocation (make_iso_home) whose ~/.claude is
#     symlinked for AUTH but whose projects/ is EMPTY and history.jsonl private —
#     so the record and replay phases never see each other's transcripts.
#
#  3. FIXED --session-id ($7) and a SHARED HOME PATH ($8), the SAME for a
#     scenario's record and replay. `gb stop` runs syncAgentSessions(), which
#     fsyncs each transcript under $HOME/.claude/projects and prints "fsynced
#     <path>" — and the model's post-stop summary turn receives gb stop's stdout
#     as a tool_result. That path contains BOTH the session-id (transcript name)
#     AND the HOME dir, so a per-phase random session-id OR a per-phase HOME dir
#     makes the two phases' tool_result differ → the summary turn hashes
#     differently and MISSES on replay (chased at length while building this
#     bead). Pinning BOTH — and wiping projects/ before each phase so exactly one
#     transcript of that fixed name exists — makes gb stop's fsync line
#     byte-identical across phases. The other volatile token in gb stop's output
#     (a timestamped advice slog.Warn) is silenced by seeding the agent bead with
#     a real project: label — see the *_SEED values.
#
#  4. CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 suppresses claude's haiku
#     title/quota side-call, which fires CONCURRENTLY with the main opus request.
#     That concurrency is a race: record and replay can interleave the two
#     differently, so the model occasionally takes a different-length trajectory
#     and a turn hashes to something never recorded (an intermittent replay MISS
#     — the flake behind T4's non-determinism). Disabling it also keeps the tier
#     to opus-only traffic, which is all these legs assert.
drive_claude() {
  local cmport="$1" dport="$2" agent_id="$3" prompt="$4" outbase="$5" cleancwd="$6" session_id="$7" iso_home="$8"
  mkdir -p "$cleancwd"
  # Rebuild the shared HOME fresh for THIS phase (empty projects/, private
  # history) — same PATH both phases, but no transcript carries over.
  make_iso_home "$iso_home"
  ( cd "$cleancwd" && \
    HOME="$iso_home" \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    BEADS_HTTP_URL="http://127.0.0.1:$dport" \
    BEADS_HTTP_ADDR="http://127.0.0.1:$dport" \
    COOP_URL="http://127.0.0.1:$dport" \
    BEADS_AUTH_TOKEN="stub-token" \
    BEADS_TOKEN="stub-token" \
    BEADS_ACTOR="claudemore-agentchoice-e2e" \
    BEADS_OPERATED_BY="oidc:agentchoice-e2e@example.com" \
    KD_PROJECT="gardenio" \
    BOAT_PROJECT="gardenio" \
    KD_AGENT_ID="$agent_id" \
    ANTHROPIC_BASE_URL="http://127.0.0.1:$cmport" \
    PATH="$WORKDIR:$PATH" \
      timeout 150 "$CLAUDE_BIN" --print --model "$MODEL" --session-id "$session_id" \
        --dangerously-skip-permissions --settings "$EMPTY_SETTINGS" "$prompt" \
        >"$outbase.out" 2>"$outbase.err" )
}

# jqf — run a python filter over a writes JSONL. $1=file, remaining=python expr
# that receives `recs` (list of dicts) and prints the answer.
py_over_writes() {
  local file="$1"; shift
  python3 - "$file" "$@" <<'PY'
import json, sys
recs = []
try:
    for line in open(sys.argv[1]):
        line = line.strip()
        if line:
            recs.append(json.loads(line))
except FileNotFoundError:
    pass
exec(sys.argv[2])
PY
}

# assert_replay_deterministic — shared T1 property for a scenario: replay exited 0
# with zero cassette misses (served entirely from cassettes, litellm bypassed).
assert_replay_deterministic() {
  local label="$1" rep_rc="$2" miss="$3" logf="$4"
  if [ "${rep_rc:-1}" = "0" ] && [ "${miss:-1}" = "0" ]; then
    pass "$label REPLAYed at \$0 with zero cassette misses (deterministic — T1)"
  else
    fail "$label replay zero-miss" "replay_rc=${rep_rc:-?} miss_count=${miss:-?} (see $logf)"
  fi
}

# ══════════════════════════════════════════════════════════════════
#  T2 — right-bead selection (R1 class 5)
# ══════════════════════════════════════════════════════════════════
blue "T2. right-bead selection (real model claims the CORRECT bead)..."
T2="$WORKDIR/t2"; mkdir -p "$T2"
# Correct: gardenio bug, open, unassigned. Decoys: wrong project; already claimed.
T2_CORRECT="gd-RIGHT001"
T2_SEED='[
 {"id":"gd-RIGHT001","title":"Fix nil deref in bridge notify","type":"bug","status":"open","assignee":"","labels":["project:gardenio"],"fields":{"project":"gardenio"},"priority":1,"kind":"issue"},
 {"id":"gd-WRONG002","title":"Refactor coop mux","type":"task","status":"open","assignee":"","labels":["project:coop"],"fields":{"project":"coop"},"priority":1,"kind":"issue"},
 {"id":"gd-WRONG003","title":"Already being worked","type":"task","status":"in_progress","assignee":"someone-else","labels":["project:gardenio"],"fields":{"project":"gardenio"},"priority":1,"kind":"issue"}
]'
T2_PROMPT='You are a gardenio crew agent. Your project is gardenio. Here is the output of `gb ready` (issues ready to work):

  gd-RIGHT001  bug   P1  [project:gardenio]  Fix nil deref in bridge notify
  gd-WRONG002  task  P1  [project:coop]     Refactor coop mux
  gd-WRONG003  task  P1  [project:gardenio]  Already being worked (in_progress, assigned to someone-else)

Claim exactly ONE bead that is appropriate for you to start: it must be in your project (gardenio), open, and unassigned. Use the real command `kd claim <id>`. Claim only the correct one, then stop.'

# Record exit code is not asserted (the model may exit non-zero for benign
# reasons; side-effects are the oracle) — capture it for symmetry only.
# shellcheck disable=SC2034
read -r T2_REC T2_REP T2_MISS <<<"$(record_and_replay_agentchoice "$T2" "$T2_PROMPT" "$T2_SEED" "gd-STUBAGENT-T2" "t2")"

# Headline: the RECORD run issued a real claim PATCH for the CORRECT bead only.
T2_CLAIMS="$(py_over_writes "$T2/record-writes.jsonl" '
claims=[r["id"] for r in recs if r.get("op")=="patch" and isinstance(r.get("body"),dict)
        and r["body"].get("status")=="in_progress" and "assignee" in r["body"]]
print(",".join(claims))')"
if [ "$T2_CLAIMS" = "$T2_CORRECT" ]; then
  pass "real model claimed exactly the CORRECT bead ($T2_CORRECT) via real kd claim — asserted by bead-state PATCH"
elif grep -q ",$T2_CORRECT," <<<",$T2_CLAIMS,"; then
  fail "T2 claim precision" "claimed the correct bead but ALSO others: [$T2_CLAIMS]"
else
  fail "T2 right-bead selection" "expected a claim PATCH for $T2_CORRECT only, got: [${T2_CLAIMS:-<none>}] (see $T2/t2-record.out)"
fi
# No decoy was claimed.
if grep -qE ",gd-WRONG00[23]," <<<",$T2_CLAIMS,"; then
  fail "T2 decoy avoidance" "a decoy bead was claimed: [$T2_CLAIMS]"
else
  pass "no decoy bead (wrong-project / already-claimed) was claimed"
fi
assert_replay_deterministic "T2" "$T2_REP" "$T2_MISS" "$T2/cm-replay.log"
echo ""

# ══════════════════════════════════════════════════════════════════
#  T3 — decision divergence (R1 class 1)
# ══════════════════════════════════════════════════════════════════
blue "T3. decision divergence (decision ONLY when options genuinely diverge)..."
# The agent bead `gb decision create` stamps requesting_agent_bead_id onto — a
# synthetic id so nothing touches a real agent bead.
# The project:gardenio label is load-bearing: gb stop enriches advice subs from
# the agent bead's labels, and without a real project: label it prints a
# TIMESTAMPED slog.Warn into its stdout — a volatile token that would break the
# post-stop turn's replay hash (see drive_claude isolation #3).
T3_AGENT_SEED='[{"id":"gd-STUBAGENT-T3","title":"stub agent","type":"agent","kind":"config","status":"open","assignee":"claudemore-agentchoice-e2e","labels":["project:gardenio","role:crew"],"fields":{"project":"gardenio","role":"crew"}}]'

# ── T3a: a GENUINE fork → the model SHOULD file a decision ──
blue "  T3a. divergent fork → files a decision..."
T3A="$WORKDIR/t3a"; mkdir -p "$T3A"
T3A_PROMPT='You are a gardenio crew agent finishing a task. You have hit a genuine fork you cannot resolve yourself: the schema migration is done, but the data backfill can either (A) run online NOW (fast, but briefly doubles DB load during peak) or (B) defer to tonight'"'"'s maintenance window (safe, but delays the feature ~8h). These lead to materially different next actions and only the operator can choose. Get a human to pick: if you need a human to choose between divergent options, run `gb decision create --no-wait --prompt "<one-line question>" --option "a:<label>:report" --option "b:<label>:report"`. Then stop.'
# shellcheck disable=SC2034
read -r T3A_REC T3A_REP T3A_MISS <<<"$(record_and_replay_agentchoice "$T3A" "$T3A_PROMPT" "$T3_AGENT_SEED" "gd-STUBAGENT-T3" "t3a")"
# Assert on the OUTCOME: at least one decision-create was ACCEPTED (reject=None)
# with ≥2 options. The model may fumble the CLI --option syntax on a first try
# (a malformed single-option attempt the guard rejects) before correcting, so we
# look for ANY accepted divergent decision, not strictly the first create.
T3A_DEC="$(py_over_writes "$T3A/record-writes.jsonl" '
d=[r for r in recs if r.get("op")=="create" and r.get("type")=="decision"]
accepted=[r for r in d if r.get("reject") in (None,"None") and len(r.get("options",[]))>=2]
best=max((len(r.get("options",[])) for r in accepted), default=0)
print(len(d), "|", len(accepted), "|", best)')"
T3A_N="$(echo "$T3A_DEC" | awk -F' \\| ' '{print $1}')"
T3A_ACCEPTED="$(echo "$T3A_DEC" | awk -F' \\| ' '{print $2}')"
T3A_NOPTS="$(echo "$T3A_DEC" | awk -F' \\| ' '{print $3}')"
if [ "${T3A_ACCEPTED:-0}" -ge 1 ]; then
  pass "divergent fork: real model filed a REAL decision with $T3A_NOPTS diverging options (guard passed)"
else
  fail "T3a divergent → decision" "expected ≥1 ACCEPTED decision with ≥2 options; got creates=$T3A_N accepted=$T3A_ACCEPTED best_opts=$T3A_NOPTS (see $T3A/t3a-record.out)"
fi
assert_replay_deterministic "T3a" "$T3A_REP" "$T3A_MISS" "$T3A/cm-replay.log"
echo ""

# ── T3b: a routine STATUS update, NO fork → the model must NOT file a decision ──
blue "  T3b. non-divergent status → NO decision (squawk instead)..."
T3B="$WORKDIR/t3b"; mkdir -p "$T3B"
T3B_PROMPT='You are a gardenio crew agent. You finished your task cleanly: the fix is merged and CI is green. There is nothing for a human to choose between — it is a routine status update. Report progress appropriately with `gb squawk "<msg>"`. Do NOT manufacture a `gb decision` when there is no genuine fork between divergent next actions. Then stop.'
# shellcheck disable=SC2034
read -r T3B_REC T3B_REP T3B_MISS <<<"$(record_and_replay_agentchoice "$T3B" "$T3B_PROMPT" "$T3_AGENT_SEED" "gd-STUBAGENT-T3" "t3b")"
# Two facts: zero decisions created, AND the model actually acted (squawked) —
# so the "no decision" is a real choice, not a vacuous no-op. `gb squawk` creates
# a type=message bead; count decisions and squawk-messages separately.
T3B_COUNTS="$(py_over_writes "$T3B/record-writes.jsonl" '
dec=len([r for r in recs if r.get("op")=="create" and r.get("type")=="decision"])
sq=len([r for r in recs if r.get("op")=="create" and r.get("type")=="message"])
print(dec, "|", sq)')"
T3B_DEC_N="$(echo "$T3B_COUNTS" | awk -F' \\| ' '{print $1}')"
T3B_SQ_N="$(echo "$T3B_COUNTS" | awk -F' \\| ' '{print $2}')"
if [ "${T3B_DEC_N:-1}" = "0" ]; then
  pass "non-divergent status: real model filed NO decision (the prose-only divergence rule claudeless cannot test)"
else
  fail "T3b non-divergent → no decision" "model filed $T3B_DEC_N decision(s) for a no-fork status update (see $T3B/t3b-record.out)"
fi
# Positive control: the model DID act (squawked), so the null decision is a real
# choice and not the model simply doing nothing.
if [ "${T3B_SQ_N:-0}" -ge 1 ]; then
  pass "non-divergent status: model instead squawked (positive control — the null decision is a real choice)"
else
  dim "    note: model filed no decision but also did not squawk — the negative still holds, but weaker (see $T3B/t3b-record.out)"
fi
assert_replay_deterministic "T3b" "$T3B_REP" "$T3B_MISS" "$T3B/cm-replay.log"
echo ""

# ══════════════════════════════════════════════════════════════════
#  T4 — wrap-up blockers judgment (R1 class 3)
# ══════════════════════════════════════════════════════════════════
blue "T4. wrap-up blockers judgment (clean run OMITS blockers → card not ⛔)..."
T4="$WORKDIR/t4"; mkdir -p "$T4"
T4_AGENT="gd-STUBAGENT-T4"
T4_SEED="[{\"id\":\"$T4_AGENT\",\"title\":\"stub agent\",\"type\":\"agent\",\"kind\":\"config\",\"status\":\"open\",\"assignee\":\"claudemore-agentchoice-e2e\",\"labels\":[\"project:gardenio\",\"role:crew\"],\"fields\":{\"project\":\"gardenio\",\"role\":\"crew\"}}]"
T4_PROMPT='You are a gardenio crew agent. You finished all your work successfully and NOTHING is blocking further progress. Wrap up and stop by running via Bash: gb stop --wrapup '"'"'{"tldr":"...","accomplishments":"..."}'"'"' — include ONLY the wrap-up fields that truly apply to a CLEAN run. Do not stamp your card blocked if you are not blocked.'
# shellcheck disable=SC2034
read -r T4_REC T4_REP T4_MISS <<<"$(record_and_replay_agentchoice "$T4" "$T4_PROMPT" "$T4_SEED" "$T4_AGENT" "t4")"

# Headline: the persisted wrap-up has NO non-empty blockers field. We read the
# wrapup PATCH the real `gb stop` wrote and apply the SAME emptiness rule the
# bridge uses (wrapUpFieldEmpty: blank / none / n/a / … → empty) to derive the
# card outcome. A clean run must land on `done`, never ⛔ `blocked`.
T4_VERDICT="$(py_over_writes "$T4/record-writes.jsonl" '
import re
NEG={"none","n/a","na","nil","null","-","—","no","nope","nothing"}
def empty(s):
    if not isinstance(s,str): return True
    t=s.strip().lower().rstrip(".").strip()
    return t=="" or t in NEG
wrapups=[]
for r in recs:
    if r.get("op")=="patch" and isinstance(r.get("body"),dict):
        f=r["body"].get("fields") or {}
        if "wrapup" in f:
            try: wrapups.append(json.loads(f["wrapup"]))
            except Exception: pass
if not wrapups:
    print("NOWRAP|")
else:
    w=wrapups[-1]
    blk=w.get("blockers","")
    # card outcome per bridge precedence (simplified to the clean/blocked axis)
    outcome = "blocked" if not empty(blk) else "done"
    present = "blockers" in w
    print(outcome+"|"+("present" if present else "omitted")+"|"+str(blk)[:40])')"
T4_OUTCOME="$(echo "$T4_VERDICT" | awk -F'|' '{print $1}')"
T4_PRESENCE="$(echo "$T4_VERDICT" | awk -F'|' '{print $2}')"
if [ "$T4_OUTCOME" = "NOWRAP" ]; then
  fail "T4 wrap-up persisted" "no wrap-up PATCH found — gb stop --wrapup did not persist a wrapup (see $T4/t4-record.out)"
elif [ "$T4_OUTCOME" = "done" ]; then
  pass "clean run: wrap-up yields card outcome 'done', not ⛔ blocked (blockers $T4_PRESENCE)"
  if [ "$T4_PRESENCE" = "omitted" ]; then
    pass "real model OMITTED the blockers field entirely on a clean run (did not stamp blockers:\"none\")"
  else
    dim "    note: blockers field present but empty/sentinel — folds to 'done' via the bridge rule; ideal is omission"
  fi
else
  fail "T4 blockers judgment" "clean run yielded card outcome '$T4_OUTCOME' (blockers $T4_PRESENCE) — a clean run must not stamp blocked (see $T4/t4-record.out)"
fi
assert_replay_deterministic "T4" "$T4_REP" "$T4_MISS" "$T4/cm-replay.log"
echo ""

# ── Summary ──────────────────────────────────────────────────────
blue "══════════════════════════════════════════════"
if [ "$FAIL" -eq 0 ]; then
  green "  All $PASS claudemore agent-choice e2e assertions passed"
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
