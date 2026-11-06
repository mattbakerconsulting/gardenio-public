#!/usr/bin/env bash
# test-hook-prime-assignment.sh — `gb hook prime` ## Assignment field-elision E2E
# (task ka-0000007410, coverage-backfill epic ka-0000000958, runtime ka-0000006516).
#
# Closes a ZERO-e2e gap on a fleet-wide hot path. `gb hook prime` runs on EVERY
# agent SessionStart. Its `## Assignment` block is `kd show <agent bead>` with any
# oversized FIELD value elided (gardenio/controller/cmd/gb/hook_assignment.go):
# a value over assignmentFieldMaxBytes (512) collapses to a
# assignmentPreviewRunes (120)-rune head preview + a `kd show <id>` pointer. The
# whole point is the agent config bead's `prompt:` field IS the entire spawn
# prompt already delivered once — re-printing it verbatim was ~17.7KB of
# duplicated input tokens per gardener spawn, on gardeners firing every 20-30m
# (ka-0000006516 killed exactly that).
#
# ── Why this is un-covered before this module ─────────────────────────────────
#
# Every prior grep hit for `hook prime|## Assignment|assignmentFieldMaxBytes|elid`
# across BOTH e2e suites is a settings.json WIRING assertion — six modules assert
# the STRING `"gb hook prime"` appears in a rendered hooks block
# (test-config-setup-claude.sh:210 et al) and one comment
# (test-claudemore-hooks.sh:290) explains why `--setting-sources ""` turns the
# SessionStart hook OFF. NONE executes the command or looks at its OUTPUT.
#
# And the 30 unit tests (hook_assignment_test.go) STUB `runKD` — the var exists
# explicitly "so tests can stub the two `kd show` round-trips without a live
# daemon", so they NEVER see real `kd show` TEXT. The renderer deliberately parses
# the `kd show` TEXT rendering (not `--json`) because the text carries
# Depends On / Children / Mentions that the JSON omits — its own doc comment says
# so. A drift in kd's text renderer (a field-line layout change, a section-header
# rename) silently breaks that parse and all 30 unit tests stay green. THAT seam —
# real `gb` binary → real `kd show` text → the elision — is what this module, and
# only this module, exercises.
#
# ── The three legs (each a hard sub-gate; any FAIL reddens the shard) ──────────
#
#   over-elided   the OVER-ceiling field (`prompt`, >512B) is elided: its head
#                 preview is present, the `full text: kd show <id>` pointer is
#                 present, AND a deep-body sentinel from the prompt is ABSENT from
#                 the whole block. Reds if the elision stops firing.
#   under-intact  ANTI-VACUITY CONTROL. The UNDER-ceiling field
#                 (`schedule_terminal_goal`, ~145B here) survives INTACT — its
#                 full tail sentinel is present and it carries NO elision marker.
#                 Without this leg the module would pass against a renderer that
#                 elides EVERYTHING (or emits nothing at all). This is the leg the
#                 tiny-ceiling mutant below reddens.
#   structure     the TEXT-only sections the parse depends on still render:
#                 `Depends On:` (with the seeded blocker id), `Children:`, and
#                 `Mentions:`. This is the leg that catches a kd text-format drift
#                 the unit tests are blind to — if `kd show`'s section headers
#                 moved, the field-line anchor would miss and the block would fall
#                 through to raw (or drop the sections).
#
# ── ANTI-VACUITY MANDATE (task ka-0000007410) ─────────────────────────────────
#
# Two overlay-built mutant `gb` binaries machine-check that the legs are
# load-bearing, not decorative:
#   • huge ceiling (assignmentFieldMaxBytes → 100000000): elision NEVER fires, so
#     the prompt renders verbatim → leg `over-elided` FAILS (its deep-body
#     sentinel would then be present). This is the exact silent-cost regression
#     ka-0000006516 guards against — nothing else goes red, the bill just goes up.
#   • tiny ceiling (assignmentFieldMaxBytes → 1): EVERYTHING is elided, so the
#     under-ceiling `schedule_terminal_goal` is now collapsed too → leg
#     `under-intact` FAILS. Proves the happy verdict is a REAL preserved field,
#     not a renderer that never elides.
# Each mutant is driven against the SAME seeded fixture; the module asserts the
# mutant's behavior FLIPS the leg. Skips CLEANLY (loud) only if no Go toolchain is
# present to build a mutant — the real-binary legs above remain the gate.
#
# ── Hermeticity / spawn-safety ────────────────────────────────────────────────
#
# Drives the REAL `gb` binary against a REAL `kd show` over the shard's live
# daemon (BEADS_HTTP_URL) — at least one leg MUST go through real `kd show` text,
# else we'd have re-created the unit tests' stub-blind spot. The fixture is a
# seeded agent bead (carrying an oversized `prompt` and an under-ceiling
# `schedule_terminal_goal`) + a blocker task + a child task, wired with edges,
# then CLOSED before any assertion. The reconciler NEVER spawns a pod for a closed
# agent bead (same zero-churn stance as test-agent-lifecycle.sh Scenario 7), and
# the fixture lives under an ephemeral `e2e-hookprime-*` project with no account
# pool, so it is doubly spawn-safe. BOAT_AGENT_BEAD_ID points `gb hook prime` at
# the fixture. All fixtures are deleted on EXIT.
#
# ── Skip contract ─────────────────────────────────────────────────────────────
#
# The `gb` binary + `kd` + python3 are present in every e2e shard (build-gb rides
# every shard's use: list; kd is the daemon CLI; python3 ships in
# e2e-system-deps.lock). If the binary/daemon is somehow absent the module SKIPS
# CLEANLY (exit 0) with a LOUD banner + E2E_SKIP_SENTINEL (ka-0000000989) — an
# absent tool must never red the suite — mirroring the verify-prod-images /
# release-status skip contract. When present, every real-binary leg is a HARD
# assertion.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-hook-prime-assignment.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
REPO_ROOT="${REPO_ROOT:-$(cd "$(dirname "$0")/../../../.." && pwd)}"
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
KD_BIN="${KD_BIN:-kd}"
RUN_ID="${RUN_ID:-$$}"
WORKDIR="$(mktemp -d "/tmp/hook-prime-assignment.${RUN_ID}.XXXXXX")"
PROJECT="e2e-hookprime-${RUN_ID}"
GB_BIN=""
GB_TEMP=""            # non-empty only when we built a throwaway gb to clean up
# Fixture bead ids are recorded in a FILE, not an array: create_bead is invoked
# via $(...) command substitution, whose subshell cannot mutate a parent array,
# so an in-memory registry would silently lose every id and leak the fixtures
# (bash subshell semantics). The file survives the subshell and the EXIT trap
# reads it back.
BEAD_REGISTRY="$WORKDIR/created-beads.txt"
: > "$BEAD_REGISTRY"

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
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}

# skip_all exits 0 (a missing tool must NOT red the suite) with a LOUD banner so a
# skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP hook-prime-assignment (LOUD): $why"
  red " This run did NOT assert the 'gb hook prime' ## Assignment field-elision"
  red " (the fleet-wide SessionStart hot path that collapses an agent bead's"
  red " oversized prompt to a preview + pointer). A green suite here proves"
  red " NOTHING about that elision. The e2e shard ships gb (build-gb), kd, and"
  red " python3 — if this fires in CI, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$why" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

cleanup() {
  # Delete every fixture bead we created (agent bead + blocker + child), read
  # back from the registry file. Prefer delete so the ephemeral project leaves no
  # trace; fall back to close.
  if [ -f "$BEAD_REGISTRY" ]; then
    while IFS= read -r id; do
      [ -n "$id" ] || continue
      "$KD_BIN" delete "$id" >/dev/null 2>&1 || "$KD_BIN" close "$id" >/dev/null 2>&1 || true
    done < "$BEAD_REGISTRY"
  fi
  # Only remove a gb we built ourselves; never a PATH-installed one.
  [ -n "$GB_TEMP" ] && rm -f "$GB_TEMP" 2>/dev/null || true
  rm -rf "$WORKDIR" 2>/dev/null || true
}
trap cleanup EXIT

# create_bead <gd-args...> — run `kd create ... --json`, echo the new id, and
# record it in the registry FILE for teardown. Echoes "" on any failure. The file
# (not an array) is used deliberately: create_bead runs inside a $(...) subshell
# whose array writes would not survive to the parent.
create_bead() {
  local out id
  out="$("$KD_BIN" create "$@" --json 2>/dev/null)" || return 0
  id="$(printf '%s' "$out" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("id",""))
except Exception: print("")' 2>/dev/null)"
  if [ -n "$id" ]; then printf '%s\n' "$id" >> "$BEAD_REGISTRY"; fi
  printf '%s' "$id"
}

# prime_block <gb-binary> <bead-id> — drive `gb hook prime` with BOAT_AGENT_BEAD_ID
# pinned to the fixture and print ONLY the `## Assignment` … `</system-reminder>`
# block. All other prime output (advice, roster) is irrelevant here.
prime_block() {
  local bin="$1" bead="$2"
  BOAT_AGENT_BEAD_ID="$bead" "$bin" hook prime 2>/dev/null \
    | sed -n '/## Assignment/,/\/system-reminder/p'
}

echo "════════════════════════════════════════════════════"
echo " gb hook prime ## Assignment field-elision E2E (ka-0000007410)"
echo " over-elided · under-intact · structure · mutation-proof"
echo "════════════════════════════════════════════════════"

cd "$REPO_ROOT" || skip_all "cannot cd to repo root $REPO_ROOT"

# ── 1. Resolve the gb binary + prerequisites ────────────────────────
# Prefer a binary pre-built + installed onto PATH. Fall
# back to an in-shell workspace build for local dev. Only when BOTH the pre-built
# binary is absent AND no Go toolchain/source is available do we skip.
if command -v gb >/dev/null 2>&1; then
  GB_BIN="$(command -v gb)"
  pass "using pre-built gb ($GB_BIN)"
else
  command -v go >/dev/null 2>&1 \
    || skip_all "no gb on PATH and no go toolchain to build it"
  [ -d "$CONTROLLER_DIR/cmd/gb" ] \
    || skip_all "no gb on PATH and cmd/gb missing under $CONTROLLER_DIR"
  blue "[build] cmd/gb (from source, workspace mode)"
  GB_TEMP="$(mktemp "/tmp/gb.${RUN_ID}.XXXXXX")"
  GB_BIN="$GB_TEMP"
  BUILD_LOG="/tmp/gb-build-hookprime-${RUN_ID}.log"
  # cmd/gb imports the local clicreds module via go.work (replace => ../clicreds),
  # so it MUST build in workspace mode. Pin GOWORK to the repo-root go.work
  # explicitly (a GOWORK=off build FATALs on the missing replacement).
  if ( GOWORK="${REPO_ROOT}/go.work" CGO_ENABLED=0 go build -o "$GB_BIN" ./gardenio/controller/cmd/gb/ ) >"$BUILD_LOG" 2>&1; then
    pass "cmd/gb built from source"
  else
    fail "build gb" "go build failed: $(tail -8 "$BUILD_LOG")"
    blue "════════════════════════════════════════════════════"
    red " ✗ 1/1 assertions failed (gb build)"
    for err in "${ERRORS[@]}"; do red "   - $err"; done
    blue "════════════════════════════════════════════════════"
    exit 1
  fi
fi

command -v "$KD_BIN" >/dev/null 2>&1 \
  || skip_all "kd absent — cannot render the real 'kd show' text the elision parses"
command -v python3 >/dev/null 2>&1 \
  || skip_all "python3 absent — cannot parse the fixture create response"

# The daemon must answer, else the fixture create + the real `kd show` inside
# `gb hook prime` cannot run. A dead daemon is environmental, not a coverage
# failure, so skip loud.
if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
  skip_all "kd daemon unreachable (BEADS_HTTP_URL=${BEADS_HTTP_URL:-<unset>}) — cannot seed the fixture bead"
fi
pass "kd daemon reachable"
echo

# ── 2. Seed the fixture bead graph ──────────────────────────────────
# The fixture agent bead carries:
#   • prompt (OVER ceiling): a >512B value with a distinctive head + a deep-body
#     sentinel that must vanish once elided.
#   • schedule_terminal_goal (UNDER ceiling): ~145B (matching the live corpus
#     median of 148B) with a full-value tail sentinel that must SURVIVE.
# plus a blocker task (Depends On edge) and a child task (Children section), so
# all three structural sections render. The agent bead is CLOSED before any
# assertion — a closed agent bead is never spawned by the reconciler.
PROMPT_FILE="$WORKDIR/prompt.txt"
GOAL_FILE="$WORKDIR/goal.txt"

# Deep-body sentinel is UNIQUE so a grep for it is unambiguous across the block.
{
  printf 'HOOKPRIME-PROMPT-HEAD-%s this is the first line of the fixture prompt\n' "$RUN_ID"
  for i in $(seq 1 40); do
    printf 'step %d: fixture prompt body line, padding the value past the 512B ceiling\n' "$i"
  done
  printf 'HOOKPRIME-DEEP-BODY-SENTINEL-%s must be ABSENT once the prompt is elided\n' "$RUN_ID"
} > "$PROMPT_FILE"

# Under-ceiling goal: a full-value tail sentinel that must survive intact. Kept
# comfortably under 512B (≈150B) so it is preserved by the real binary and only
# elided by the tiny-ceiling mutant.
printf 'HOOKPRIME-GOAL-HEAD-%s STANDING gardener — drive every open child to closure; HOOKPRIME-GOAL-TAIL-SENTINEL-%s' \
  "$RUN_ID" "$RUN_ID" > "$GOAL_FILE"

prompt_bytes="$(wc -c < "$PROMPT_FILE" | tr -d ' ')"
goal_bytes="$(wc -c < "$GOAL_FILE" | tr -d ' ')"
blue "[seed] project=$PROJECT  prompt=${prompt_bytes}B (over 512)  schedule_terminal_goal=${goal_bytes}B (under 512)"
if [ "$prompt_bytes" -le 512 ] || [ "$goal_bytes" -gt 512 ]; then
  fail "seed sizing" "prompt must be >512B and goal <=512B (got prompt=${prompt_bytes} goal=${goal_bytes})"
  blue "════════════════════════════════════════════════════"
  red " ✗ fixture sizing invariant broken"
  exit 1
fi

BLOCKER_ID="$(create_bead "hookprime-blocker-${RUN_ID}" --type=task -l "project:${PROJECT}")"
CHILD_ID="$(create_bead "hookprime-child-${RUN_ID}" --type=task -l "project:${PROJECT}")"
# The description references the blocker id so `kd show` emits a `Mentions:`
# section (bead-id references in the description populate Mentions). That gives
# the structure leg a third text-only section to gate on — the one most likely to
# move if kd's text renderer drifts.
AGENT_ID="$(create_bead "hookprime-agent-${RUN_ID}" --type=agent --role crew \
  -d "fixture agent bead; mentions ${BLOCKER_ID} to populate the Mentions section" \
  -f "agent=hookprime-agent-${RUN_ID}" \
  -f "project=${PROJECT}" \
  -f "prompt=@${PROMPT_FILE}" \
  -f "schedule_terminal_goal=@${GOAL_FILE}")"

if [ -z "$BLOCKER_ID" ] || [ -z "$CHILD_ID" ] || [ -z "$AGENT_ID" ]; then
  fail "seed" "could not create the fixture beads (blocker=$BLOCKER_ID child=$CHILD_ID agent=$AGENT_ID)"
  blue "════════════════════════════════════════════════════"
  red " ✗ fixture seed failed"
  for err in "${ERRORS[@]}"; do red "   - $err"; done
  blue "════════════════════════════════════════════════════"
  exit 1
fi
dim "  agent=$AGENT_ID  blocker=$BLOCKER_ID  child=$CHILD_ID"

# Edges: the agent bead is BLOCKED BY the blocker (Depends On) and PARENTS the
# child (Children). Both render only in the `kd show` TEXT output.
"$KD_BIN" dep add "$AGENT_ID" "$BLOCKER_ID" --type blocks >/dev/null 2>&1 \
  || fail "seed edge (blocks)" "could not add blocks edge $AGENT_ID -> $BLOCKER_ID"
"$KD_BIN" dep add "$CHILD_ID" "$AGENT_ID" --type parent-child >/dev/null 2>&1 \
  || fail "seed edge (parent-child)" "could not add parent-child edge $CHILD_ID -> $AGENT_ID"

# Close the agent bead: a closed agent bead is never spawned by the reconciler
# (test-agent-lifecycle.sh Scenario 7), so the fixture is zero-churn. `kd show`
# and thus `gb hook prime` still render a closed bead in full.
"$KD_BIN" close "$AGENT_ID" >/dev/null 2>&1 \
  || fail "seed close" "could not close fixture agent bead $AGENT_ID (spawn-safety)"
echo

# ── 3. Capture the real gb hook prime block ─────────────────────────
BLOCK="$WORKDIR/block.real.txt"
prime_block "$GB_BIN" "$AGENT_ID" > "$BLOCK"
if [ ! -s "$BLOCK" ]; then
  fail "prime block" "gb hook prime emitted no ## Assignment block for $AGENT_ID"
  blue "════════════════════════════════════════════════════"
  red " ✗ no assignment block to assert on"
  for err in "${ERRORS[@]}"; do red "   - $err"; done
  blue "════════════════════════════════════════════════════"
  exit 1
fi

DEEP_SENTINEL="HOOKPRIME-DEEP-BODY-SENTINEL-${RUN_ID}"
GOAL_TAIL="HOOKPRIME-GOAL-TAIL-SENTINEL-${RUN_ID}"
PROMPT_HEAD="HOOKPRIME-PROMPT-HEAD-${RUN_ID}"

# ── Leg over-elided (the OVER-ceiling prompt is elided) ──────────────
blue "[leg over-elided] the >512B prompt renders as a preview + pointer, deep body gone"
prompt_line="$(grep -E '^  prompt:' "$BLOCK" | head -1)"
if [ -z "$prompt_line" ]; then
  fail "leg over-elided prompt line" "no 'prompt:' field line in the block:\n$(cat "$BLOCK")"
elif ! grep -qF "$PROMPT_HEAD" <<< "$prompt_line"; then
  fail "leg over-elided preview" "prompt line lost its head preview ($PROMPT_HEAD): $prompt_line"
elif ! grep -qE "full text: kd show ${AGENT_ID}\]" <<< "$prompt_line"; then
  fail "leg over-elided pointer" "prompt line missing the 'full text: kd show $AGENT_ID' pointer: $prompt_line"
elif ! grep -qE "elided" <<< "$prompt_line"; then
  fail "leg over-elided marker" "prompt line missing the 'elided' marker: $prompt_line"
elif grep -qF "$DEEP_SENTINEL" "$BLOCK"; then
  # THE anti-vacuity assertion: if the elision stopped firing (huge ceiling), the
  # deep body sentinel would survive into the block.
  fail "leg over-elided body-gone" "deep-body sentinel '$DEEP_SENTINEL' survived into the block — the prompt was NOT elided (elision regressed / ceiling raised?)"
else
  pass "leg over-elided — prompt shows head preview + 'kd show $AGENT_ID' pointer; deep body absent"
fi
echo

# ── Leg under-intact (ANTI-VACUITY: the UNDER-ceiling goal survives) ─
blue "[leg under-intact] the <512B schedule_terminal_goal survives INTACT (anti-vacuity)"
goal_line="$(grep -E '^  schedule_terminal_goal:' "$BLOCK" | head -1)"
if [ -z "$goal_line" ]; then
  fail "leg under-intact goal line" "no 'schedule_terminal_goal:' field line in the block:\n$(cat "$BLOCK")"
elif ! grep -qF "$GOAL_TAIL" "$BLOCK"; then
  # If the goal were elided (everything-elided renderer), its full tail sentinel
  # would be gone. Its presence proves a REAL preserved field.
  fail "leg under-intact tail" "full-value tail sentinel '$GOAL_TAIL' missing — the under-ceiling goal was elided (renderer elides everything?)"
elif grep -qE "elided — full text" <<< "$goal_line"; then
  fail "leg under-intact no-marker" "schedule_terminal_goal carries an elision marker but must survive intact: $goal_line"
else
  pass "leg under-intact — schedule_terminal_goal preserved verbatim with its full tail sentinel, no elision marker"
fi
echo

# ── Leg structure (TEXT-only sections still render) ─────────────────
blue "[leg structure] Depends On / Children / Mentions render (the kd text-drift leg)"
struct_ok=1
if ! grep -qE '^Depends On:' "$BLOCK"; then
  fail "leg structure Depends On" "no 'Depends On:' section — kd text format drifted / anchor missed"; struct_ok=0
elif ! grep -qF "$BLOCKER_ID" "$BLOCK"; then
  fail "leg structure blocker id" "'Depends On:' present but the seeded blocker $BLOCKER_ID is absent from it:\n$(cat "$BLOCK")"; struct_ok=0
fi
if ! grep -qE '^Children:' "$BLOCK"; then
  fail "leg structure Children" "no 'Children:' section — kd text format drifted"; struct_ok=0
fi
if ! grep -qE '^Mentions:' "$BLOCK"; then
  fail "leg structure Mentions" "no 'Mentions:' section — kd text format drifted"; struct_ok=0
fi
if [ "$struct_ok" = 1 ]; then
  pass "leg structure — Depends On (with blocker $BLOCKER_ID), Children, and Mentions all render"
fi
echo

# ── Leg mutation-proof (machine-checked negative controls) ──────────
# Two overlay-built mutant gb binaries prove the legs above are load-bearing:
#   • huge ceiling  → elision never fires  → leg over-elided FAILS (deep body present)
#   • tiny ceiling  → everything elided    → leg under-intact FAILS (goal elided)
# Skips CLEANLY (loud) if no Go toolchain/source is present; the real-binary legs
# remain the gate in that case.
blue "[leg mutation-proof] huge-ceiling mutant stops eliding; tiny-ceiling mutant elides the goal"
HOOK_SRC="${CONTROLLER_DIR}/cmd/gb/hook_assignment.go"
ORIG_ABS="${REPO_ROOT}/${HOOK_SRC}"
if ! command -v go >/dev/null 2>&1; then
  red "  ⚠ SKIP leg mutation-proof (LOUD): no go toolchain to build the mutants."
  dim "    The real-binary legs above remain the gate; this leg only MACHINE-CHECKS"
  dim "    that they redden on a ceiling regression. Its absence does not weaken them."
  [ -n "${E2E_SKIP_SENTINEL:-}" ] && printf 'hook-prime mutation-proof: no go toolchain\n' >"$E2E_SKIP_SENTINEL" 2>/dev/null || true
elif [ ! -f "$ORIG_ABS" ]; then
  red "  ⚠ SKIP leg mutation-proof (LOUD): $HOOK_SRC missing (pre-built gb, no source)."
  dim "    The real-binary legs above remain the gate."
  [ -n "${E2E_SKIP_SENTINEL:-}" ] && printf 'hook-prime mutation-proof: hook_assignment.go missing\n' >"$E2E_SKIP_SENTINEL" 2>/dev/null || true
else
  MUT_BUILD_TIMEOUT="${MUT_BUILD_TIMEOUT:-240}"

  # build_ceiling_mutant <new-ceiling-int> <out-var-name> — overlay-build a gb
  # whose assignmentFieldMaxBytes is replaced by <new-ceiling-int>. Sets the named
  # out-var to the binary path on success, or "" on a prep/build problem (the
  # caller decides skip-vs-fail). Emits its own diagnostics.
  MUT_NEEDLE="const assignmentFieldMaxBytes = 512"
  build_ceiling_mutant() {
    local ceiling="$1" __outvar="$2"
    local mut_src="$WORKDIR/hook_mut_${ceiling}.go"
    local prep_rc
    python3 - "$ORIG_ABS" "$mut_src" "$MUT_NEEDLE" "const assignmentFieldMaxBytes = ${ceiling}" <<'PY'
import sys
orig, dst, needle, repl = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
src = open(orig).read()
if needle not in src:
    sys.stderr.write("MUTATION-TARGET-MISSING")  # constant moved/renamed
    sys.exit(3)
open(dst, "w").write(src.replace(needle, repl, 1))
PY
    prep_rc=$?
    if [ "$prep_rc" -eq 3 ]; then
      # The constant this module keys on is gone — HARD failure: either
      # hook_assignment.go was refactored (update the needle) or the elision was
      # reverted flat. Either way the coverage premise changed; re-derive it.
      fail "leg mutation-proof prep" "'$MUT_NEEDLE' not found in $HOOK_SRC — the elision ceiling moved/reverted; re-derive the gap"
      printf -v "$__outvar" '%s' ""
      return 1
    fi
    local overlay="$WORKDIR/overlay_${ceiling}.json"
    python3 - "$ORIG_ABS" "$mut_src" "$overlay" <<'PY'
import json, sys
orig, mut, dst = sys.argv[1], sys.argv[2], sys.argv[3]
json.dump({"Replace": {orig: mut}}, open(dst, "w"))
PY
    local bin="$WORKDIR/gb-mutant-${ceiling}"
    local log="$WORKDIR/gb-mutant-${ceiling}.log"
    if timeout "$MUT_BUILD_TIMEOUT" env GOWORK="${REPO_ROOT}/go.work" CGO_ENABLED=0 \
         go build -overlay="$overlay" -o "$bin" ./gardenio/controller/cmd/gb/ >"$log" 2>&1; then
      printf -v "$__outvar" '%s' "$bin"
      return 0
    fi
    dim "    mutant (ceiling=$ceiling) build log: $(tail -4 "$log" 2>/dev/null)"
    printf -v "$__outvar" '%s' ""
    return 2
  }

  # Mutant A: huge ceiling → elision never fires → the deep body sentinel that
  # leg over-elided asserts is ABSENT must now be PRESENT.
  HUGE_BIN=""
  if build_ceiling_mutant 100000000 HUGE_BIN && [ -n "$HUGE_BIN" ]; then
    HB="$WORKDIR/block.huge.txt"
    prime_block "$HUGE_BIN" "$AGENT_ID" > "$HB"
    if [ ! -s "$HB" ]; then
      fail "leg mutation-proof huge" "huge-ceiling mutant emitted no block — build produced a no-op binary?"
    elif grep -qF "$DEEP_SENTINEL" "$HB"; then
      pass "leg mutation-proof huge — ceiling→∞ stops eliding: deep body '$DEEP_SENTINEL' now present (leg over-elided reddens on this regression)"
    else
      fail "leg mutation-proof huge" "huge-ceiling mutant STILL elided the prompt (deep body absent) — leg over-elided is NOT sensitive to the ceiling"
    fi
  elif [ -z "$HUGE_BIN" ] && [ "${#ERRORS[@]}" -gt 0 ] && grep -q "leg mutation-proof prep" <<<"${ERRORS[@]}"; then
    : # prep already hard-failed above; do not double-count
  else
    red "  ⚠ SKIP leg mutation-proof huge (LOUD): mutant overlay build failed (environmental)."
    dim "    Leg over-elided remains the gate."
    [ -n "${E2E_SKIP_SENTINEL:-}" ] && printf 'hook-prime mutation-proof huge: overlay build failed\n' >"$E2E_SKIP_SENTINEL" 2>/dev/null || true
  fi

  # Mutant B: tiny ceiling → everything elided → the under-ceiling goal's full
  # tail sentinel that leg under-intact asserts is PRESENT must now be ABSENT.
  TINY_BIN=""
  if build_ceiling_mutant 1 TINY_BIN && [ -n "$TINY_BIN" ]; then
    TB="$WORKDIR/block.tiny.txt"
    prime_block "$TINY_BIN" "$AGENT_ID" > "$TB"
    if [ ! -s "$TB" ]; then
      fail "leg mutation-proof tiny" "tiny-ceiling mutant emitted no block — build produced a no-op binary?"
    elif grep -qF "$GOAL_TAIL" "$TB"; then
      fail "leg mutation-proof tiny" "tiny-ceiling mutant did NOT elide the under-ceiling goal (tail '$GOAL_TAIL' still present) — leg under-intact is NOT sensitive; it would pass against an elide-everything renderer"
    else
      pass "leg mutation-proof tiny — ceiling→1 elides the goal too: tail '$GOAL_TAIL' now absent (leg under-intact reddens on this regression)"
    fi
  elif [ -z "$TINY_BIN" ] && [ "${#ERRORS[@]}" -gt 0 ] && grep -q "leg mutation-proof prep" <<<"${ERRORS[@]}"; then
    : # prep already hard-failed above
  else
    red "  ⚠ SKIP leg mutation-proof tiny (LOUD): mutant overlay build failed (environmental)."
    dim "    Leg under-intact remains the gate."
    [ -n "${E2E_SKIP_SENTINEL:-}" ] && printf 'hook-prime mutation-proof tiny: overlay build failed\n' >"$E2E_SKIP_SENTINEL" 2>/dev/null || true
  fi
fi
echo

# ── Summary ─────────────────────────────────────────────────────────
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total assertions passed — gb hook prime ## Assignment elision verified"
else
  red " ✗ $FAIL/$total assertions failed"
  echo
  for err in "${ERRORS[@]}"; do red "   - $err"; done
fi
blue "════════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
