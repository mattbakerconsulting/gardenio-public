#!/usr/bin/env bash
# test-agent-state-enum.sh — E2E gate for the type:agent `agent_state` enum,
# specifically the three controller-written values merge 568969352 added but
# that NOTHING wrote to a live daemon: `queued`, `model_rate_limited`,
# `stop_requested` (bead ka-0000000270, parent ka-0000000958).
#
# ── THE GAP THIS CLOSES ────────────────────────────────────────────
# `queued` (reconciler markQueued + spawn-declined/pool-exhausted),
# `model_rate_limited` (in-pod coop ErrorCategory=model_rate_limited,
# init_agent.go) and `stop_requested` as an agent_state VALUE (RequestAgentStop,
# client.go) all landed in the builtin type:agent enum
# (kbeads/internal/server/config.go). All three call sites only LOG a PATCH
# error, so a rejected enum value means the state SILENTLY never lands — a
# pool-exhausted agent reads `spawning` forever instead of `queued`, and
# `gb crew` / the gardeners page / the stale-reaper queued-count all show a wrong
# lifecycle state with zero error surfaced anywhere. This is the third
# recurrence of the identical silent-PATCH-drop class
# (ka-0000000585 → ka-0000006367 → this).
#
# The pre-existing net provably could NOT catch a fourth: the only apparent
# coverage — test-microvm-execution-target.sh state_accepted() — is a pure shell
# loop over a HARDCODED literal `BUILTIN_AGENT_STATES`; it asserts a shell string
# against itself, never contacts the daemon, and passes with kbeads uninstalled
# entirely. Every other grep hit is a READ of whatever the controller wrote, not
# a WRITE that would red on a rejected enum value.
#
# ── WHAT THIS MODULE ASSERTS (against the LIVE daemon) ─────────────
#   A. SCHEMA: the daemon's RESOLVED type:agent config lists each of the three
#      values in the agent_state enum (`kd fields agent --json`).
#   B. WRITE+READBACK: create an OPEN, project=e2e-test type=agent bead at
#      agent_state=spawning, then `kd update -f agent_state=<v>` for each value
#      and read it BACK via `kd show --json | .fields.agent_state`. The read-back
#      is load-bearing — several controller PATCH paths log-and-swallow a
#      rejection yet still exit 0, so exit 0 alone is NOT proof the value landed.
#      Because the fixture bead IS briefly cold-started into a pod (see "WHY A
#      POD IS COLD-STARTED"), the write+readback goes through write_confirm,
#      which retries past the reporter race while still failing LOUD on a genuine
#      daemon rejection (a rejected value never reads back on ANY attempt).
#   C. NEGATIVE CONTROL: `agent_state=totally_not_a_state` must be REJECTED
#      (nonzero exit AND the stored value unchanged). Without this the leg could
#      silently degrade into "the enum stopped validating and now accepts
#      anything" — the exact vacuity mode that produced the shell-literal
#      assertion this module supersedes.
#
# ── WHY OPEN, NOT "closed immediately" ────────────────────────────
# `agent_state` is in the daemon's transientClosedBeadFieldKeys
# (kbeads/internal/server/beads.go): a CLOSED bead silently DROPS writes to it
# (dropTransientClosedFields), returning exit 0 with the value unchanged — so a
# "close immediately, then update" recipe would make BOTH the write leg AND the
# negative control vacuous (every write, valid or bogus, no-ops identically).
# The bead is therefore kept OPEN across the writes and closed in the cleanup
# trap, mirroring test-agent-lifecycle.sh / test-agents-live-view.sh.
#
# ── WHY A POD IS COLD-STARTED — AND HOW THE WRITE LEG SURVIVES IT ──
# An OPEN type=agent bead is a DESIRED pod to the controller. Its `created` SSE
# frame fires the event-door AgentSpawn UNCONDITIONALLY (subscriber/sse.go), and
# `project=e2e-test` does NOT fence that spawn: evaluateSpawnCredentials
# (controller spawn_credential_guard.go) refuses only (a) an empty project or
# (b) a referenced-but-missing OAuth secret. project=e2e-test has NO OAuth
# SecretEnv at all, so the guard treats it as a direct-provider-auth project and
# ALLOWS the spawn. (An earlier version of this header claimed the credential
# guard was the "PRIMARY fence" — that was factually wrong; verified against the
# 2026.219.3 release-blocker failure, bug gd-lJhluh3CLA.)
#
# So the controller DOES cold-start a short-lived pod for the fixture, and its
# status reporter maps the pod phase (PhaseToAgentState: Pending->"spawning",
# Failed->"failed", Succeeded->"done") and PATCHes agent_state back onto the
# bead — racing this module's own write+readback. That race is what red 219.3's
# deploy stage: `agent_state=queued` was written, the reporter stamped
# "spawning" over it before the readback, and the module reported a false
# silent-drop. It is a TIMING race, not a daemon or code regression (219.2
# passed this module with a byte-identical daemon).
#
# The write leg absorbs this via write_confirm (retry + best-effort pod
# force-delete of the racing reporter's pod), which still fails LOUD on a genuine
# daemon rejection. Belt-and-suspenders, the cleanup trap force-deletes any pod
# and closes the bead (closed => excluded from ListAgentBeads, so the
# desired-vs-actual sweep stops considering it the instant the module ends).
#
# ── WHY THE ENUM MUST BE AUTHORITATIVE (self-SKIP guard) ──────────
# The invariant is only OBSERVABLE on a daemon whose RESOLVED type:agent config
# keeps agent_state as an `enum`. A store-level type:agent override that declares
# agent_state as a plain `string` MASKS the builtin enum (resolveTypeConfig only
# unions enum values when BOTH store and builtin declare the field as an enum),
# so such a daemon accepts ANY string — including the negative control — and the
# whole invariant is untestable. That is exactly the prod-daemon shape today.
# The e2e namespace is the RIGHT place precisely because seed-types-and-users.sh
# does NOT seed type:agent (`grep -c type:agent` → 0), so the e2e daemon uses the
# BUILTIN enum with no masking override. If a run ever finds agent_state resolved
# as a string, the module SKIPs LOUD (not a false green, not a false red) —
# there is no enum contract to assert on that daemon.
#
# ── Usage ─────────────────────────────────────────────────────────
#   ./gardenio/tests/e2e/scripts/test-agent-state-enum.sh
#   BEADS_HTTP_URL=http://localhost:8080 BEADS_TOKEN=… \
#     ./gardenio/tests/e2e/scripts/test-agent-state-enum.sh
#
# Self-SKIPs (exit 0) when no daemon is reachable or when agent_state is not a
# resolved enum. Daemon-only; no kubectl required to pass (kubectl is used only
# for an opportunistic port-forward and a best-effort pod sweep).

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19151}"
KD_BIN="${KD_BIN:-kd}"
PREFIX="agent-state-enum-e2e"
SUFFIX="$$-$(date +%s)"
# project=e2e-test is UNSEEDED for type:agent, so the builtin agent_state enum is
# unmasked (the invariant this module asserts). It is NOT a spawn fence — the
# controller still cold-starts a short-lived pod for the fixture; the write leg
# tolerates the resulting reporter race via write_confirm (see header).
FIXTURE_PROJECT="e2e-test"
# A human operator so the daemon's fail-closed audit resolver always attributes
# the create/update — independent of whatever BEADS_OPERATED_BY the harness
# exports. Matches the assignment's operated_by.
OPERATED_BY="${AGENT_STATE_ENUM_OPERATED_BY:-slack:U0OPERATOR1}"

# The three values under test — the ones merge 568969352 added and nothing wrote
# to a live daemon. Keep in lockstep with the builtin enum (config.go).
NEW_STATES="queued model_rate_limited stop_requested"
# The negative control: a value that is NOT in the enum and MUST be rejected.
BOGUS_STATE="totally_not_a_state"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
BEADS_PF_PID=""
# Registries are FILES (not arrays): create runs in a $(...) subshell, so an
# array append inside it dies with the subshell. A file crosses the boundary so
# the cleanup trap sees every id/agent actually created.
BEAD_REGISTRY="$(mktemp -t agent-state-enum-beads.XXXXXX)"
AGENT_REGISTRY="$(mktemp -t agent-state-enum-agents.XXXXXX)"

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

have() { command -v "$1" >/dev/null 2>&1; }
id_from_json() { printf '%s' "$1" | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))" 2>/dev/null || printf ''; }

# agent_state_field_type — the RESOLVED type of the agent_state field on the
# live daemon's type:agent config ("enum" on an unmasked daemon, "string" when a
# store override masks the builtin). Empty if it cannot be read.
agent_state_field_type() {
  "$KD_BIN" fields agent --json 2>/dev/null | python3 -c "
import sys,json
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
for f in d.get('fields',[]):
  if f.get('name')=='agent_state':
    print(f.get('type','')); break
" 2>/dev/null || printf ''
}

# agent_state_enum_values — space-joined resolved enum values for agent_state.
agent_state_enum_values() {
  "$KD_BIN" fields agent --json 2>/dev/null | python3 -c "
import sys,json
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
for f in d.get('fields',[]):
  if f.get('name')=='agent_state':
    print(' '.join(f.get('values') or [])); break
" 2>/dev/null || printf ''
}

# readback_state <bead-id> — the stored agent_state field value, or empty.
readback_state() {
  "$KD_BIN" show "$1" --json 2>/dev/null | python3 -c "
import sys,json
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
print((d.get('fields') or {}).get('agent_state',''))
" 2>/dev/null || printf ''
}

# write_confirm <bead-id> <agent-name> <state> — write agent_state=<state> via
# `kd update` and CONFIRM it reads back, tolerating the controller status
# reporter that briefly races this OPEN fixture bead (see "WHY A POD IS
# COLD-STARTED" in the header). `kd update` faithfully surfaces a daemon enum
# rejection as a NONZERO exit (the negative control proves this), so:
#   • nonzero kd-update exit  -> HARD rejection (a value the daemon's own config
#     lists was refused — the silent-PATCH-drop class this module guards).
#     Returned immediately as rc so the caller reds LOUD.
#   • exit 0, readback==state -> the daemon accepted AND persisted the value.
#   • exit 0, readback!=state -> a concurrent writer overwrote our value AFTER
#     the daemon stored it. That writer is the cold-started fixture pod's status
#     reporter mapping its pod phase -> "spawning"/"failed"/"done" (bug
#     gd-lJhluh3CLA: the 2026.219.3 release-blocker). Force-delete that pod to
#     stop the reporter at the source, re-write, and retry. A GENUINE
#     silent-200-drop never reads back as <state> on ANY attempt, so the retry
#     preserves the invariant's teeth — it only absorbs the reporter race.
# Returns 0 once readback==state; the kd-update rc on a hard rejection; 3 if the
# value never settled within the window (caller treats as "did not land").
write_confirm() {
  local id="$1" agent="$2" st="$3" attempts=16 rc back i
  for ((i = 1; i <= attempts; i++)); do
    rc=0
    "$KD_BIN" update "$id" -f "agent_state=${st}" \
      --operated-by "$OPERATED_BY" >/dev/null 2>&1 || rc=$?
    if [ "$rc" -ne 0 ]; then
      return "$rc" # hard daemon rejection — surface it unretried
    fi
    back=$(readback_state "$id")
    if [ "$back" = "$st" ]; then
      return 0
    fi
    # readback was clobbered by the cold-started pod's reporter. Stop it at the
    # source (best-effort; the module still passes without kubectl — the pod
    # dies on its own within ~1s since project=e2e-test has no credential), then
    # retry the write.
    if have kubectl; then
      kubectl -n "$NAMESPACE" delete pod -l "gardenio.io/agent=${agent}" \
        --force --grace-period=0 --wait=false >/dev/null 2>&1 || true
    fi
    sleep 0.4
  done
  return 3
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return 0
  fi
  if ! have kubectl; then
    yellow "no BEADS_HTTP_URL and kubectl not installed — skipping module"
    return 1
  fi
  if ! kubectl -n "$NAMESPACE" get svc "$BEADS_SVC" >/dev/null 2>&1; then
    yellow "beads service $BEADS_SVC not found in $NAMESPACE — skipping module"
    return 1
  fi
  blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
    "${BEADS_PF_PORT}:8080" >"/tmp/${PREFIX}-pf.log" 2>&1 &
  BEADS_PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${BEADS_PF_PORT}"
  if [ -z "${BEADS_TOKEN:-}" ]; then
    local secret
    secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
    [ -n "$secret" ] && export BEADS_TOKEN="$secret"
  fi
  return 0
}

cleanup() {
  # Close every registered bead (closed ⇒ excluded from ListAgentBeads, so the
  # reconciler stops treating it as a desired pod). Read from the FILE registry
  # so ids created in $(...) subshells are actually seen.
  if [ -s "$BEAD_REGISTRY" ]; then
    local id
    while IFS= read -r id; do
      [ -n "$id" ] || continue
      "$KD_BIN" close "$id" >/dev/null 2>&1 || true
    done < "$BEAD_REGISTRY"
  fi
  # Best-effort: force-delete any pod the reconciler/event-door cold-started
  # from a seeded agent bead before its fence took effect.
  if [ -s "$AGENT_REGISTRY" ] && have kubectl; then
    local agent
    while IFS= read -r agent; do
      [ -n "$agent" ] || continue
      kubectl -n "$NAMESPACE" delete pod \
        -l "gardenio.io/agent=${agent}" \
        --force --grace-period=0 --wait=false >/dev/null 2>&1 || true
    done < "$AGENT_REGISTRY"
  fi
  rm -f "$BEAD_REGISTRY" "$AGENT_REGISTRY" 2>/dev/null || true
  if [ -n "$BEADS_PF_PID" ]; then kill "$BEADS_PF_PID" 2>/dev/null || true; fi
  rm -f "/tmp/${PREFIX}-"*.log 2>/dev/null || true
}
trap cleanup EXIT

# ── Test suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " type:agent agent_state enum E2E (ka-0000000270)"
  blue "═══════════════════════════════════════════════════"

  if ! setup_portforward; then
    skip "agent-state-enum" "beads daemon not reachable (no port-forward)"
    echo; green " ⊘ SKIPPED (no daemon)"; return 0
  fi
  # Reachability probe — SKIP (not FAIL) if the tunnel is up but the daemon
  # returns nothing, matching the inert-when-unreachable sibling contract.
  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    skip "agent-state-enum" "kd cannot reach daemon at ${BEADS_HTTP_URL:-?}"
    echo; green " ⊘ SKIPPED (no daemon)"; return 0
  fi
  green "  Daemon reachable: ${BEADS_HTTP_URL:-(env)}"
  echo

  # ── Enum-authoritative guard ────────────────────────────────────
  # The invariant is only observable when the RESOLVED agent_state field is an
  # enum. A string (store override masking the builtin) accepts anything — the
  # negative control could not fail there, so there is nothing to assert.
  local ftype
  ftype=$(agent_state_field_type)
  if [ "$ftype" != "enum" ]; then
    skip "agent-state-enum" \
      "resolved agent_state field type is '${ftype:-∅}', not 'enum' — a store type:agent override masks the builtin enum on this daemon, so the enum contract is unobservable (expected on prod; the fresh e2e daemon does not seed type:agent)"
    echo; green " ⊘ SKIPPED (agent_state not an authoritative enum here)"; return 0
  fi
  pass "resolved type:agent config keeps agent_state as an authoritative enum"

  # ── A. SCHEMA: the three values are in the resolved enum ─────────
  local enum_values st
  enum_values=" $(agent_state_enum_values) "
  for st in $NEW_STATES; do
    if grep -q " $st " <<<"$enum_values"; then
      pass "resolved enum lists agent_state=$st"
    else
      fail "resolved enum MISSING agent_state=$st" \
        "merge 568969352 added it to the builtin enum, but the daemon's resolved type:agent config does not list it (values:${enum_values})"
    fi
  done
  # Guard the schema check itself: the bogus value must NOT already be in the
  # enum, else assertion C below would be meaningless.
  if grep -q " $BOGUS_STATE " <<<"$enum_values"; then
    fail "negative-control value is in the enum" \
      "the enum unexpectedly lists '$BOGUS_STATE' — pick a different control"
  fi

  # ── Fixture: an OPEN, project=e2e-test type=agent bead at agent_state=spawning ──
  local agent_name="${PREFIX}-${SUFFIX}" ag_out AG_ID
  agent_name="${agent_name//_/-}"
  ag_out=$("$KD_BIN" create "$agent_name" --type=agent \
    --project "$FIXTURE_PROJECT" --force \
    --operated-by "$OPERATED_BY" --no-parent \
    -f "agent=${agent_name}" \
    -f "role=crew-e2e" \
    -f "project=${FIXTURE_PROJECT}" \
    -f "agent_state=spawning" \
    -f "slack_user_id=U0OPERATOR1" \
    --json 2>/dev/null || printf '{}')
  AG_ID=$(id_from_json "$ag_out")
  if [ -z "$AG_ID" ]; then
    fail "fixture-setup" "could not create project=e2e-test type=agent bead (out: $(printf '%s' "$ag_out" | head -c 160))"
    echo; summarize; return
  fi
  echo "$AG_ID" >> "$BEAD_REGISTRY"
  echo "$agent_name" >> "$AGENT_REGISTRY"
  green "  seeded OPEN agent bead $AG_ID (project=$FIXTURE_PROJECT, agent_state=spawning)"
  echo

  # ── B. WRITE+READBACK each new value against the LIVE daemon ─────
  # write_confirm distinguishes a HARD daemon rejection (nonzero kd-update exit —
  # the silent-PATCH-drop class this module guards) from the controller reporter
  # race that clobbers our readback (retried, not a false red). See its comment
  # and the header's "WHY A POD IS COLD-STARTED" block.
  blue "[write] each new agent_state value must WRITE and READ BACK (exit 0 alone is not proof)"
  for st in $NEW_STATES; do
    local rc=0 back
    write_confirm "$AG_ID" "$agent_name" "$st" || rc=$?
    if [ "$rc" -eq 0 ]; then
      pass "agent_state=$st accepted AND read back as '$st'"
    elif [ "$rc" -eq 3 ]; then
      back=$(readback_state "$AG_ID")
      fail "agent_state=$st did not land" "kd update exited 0 but the value never read back within the retry window (last read '${back:-∅}') — a silent-200-drop the daemon should not do, OR a persistent concurrent overwrite"
    else
      fail "agent_state=$st write rejected" "kd update exited $rc — the daemon enum does not accept a value its own config lists (silent-PATCH-drop class)"
    fi
  done
  echo

  # ── C. NEGATIVE CONTROL: a bogus value MUST be rejected ─────────
  # This leg is naturally immune to the reporter race: the reporter only ever
  # writes real pod-phase states ("spawning"/"working"/"done"/"failed"), never
  # the bogus value, so `after != BOGUS_STATE` holds unless the daemon genuinely
  # accepted the bogus write. Park at a known-good value (retried via
  # write_confirm) so the "value unchanged" evidence is unambiguous.
  blue "[control] a value NOT in the enum must be REJECTED (nonzero exit AND value unchanged)"
  write_confirm "$AG_ID" "$agent_name" "queued" >/dev/null 2>&1 || true
  local before after rc=0
  before=$(readback_state "$AG_ID")
  "$KD_BIN" update "$AG_ID" -f "agent_state=${BOGUS_STATE}" \
    --operated-by "$OPERATED_BY" >/dev/null 2>&1 || rc=$?
  after=$(readback_state "$AG_ID")
  if [ "$rc" -ne 0 ] && [ "$after" != "$BOGUS_STATE" ]; then
    pass "bogus agent_state='$BOGUS_STATE' REJECTED (exit $rc) and value unchanged ('${after}')"
  elif [ "$rc" -eq 0 ] && [ "$after" = "$BOGUS_STATE" ]; then
    fail "enum stopped validating" \
      "bogus '$BOGUS_STATE' was ACCEPTED (exit 0, read back '$after') — agent_state is no longer an authoritative enum; the write leg above is vacuous"
  elif [ "$rc" -eq 0 ]; then
    fail "bogus write exited 0" "value read back '${after}' (was '${before}') — expected a nonzero exit rejecting '$BOGUS_STATE'"
  else
    fail "bogus write rejected but value changed" \
      "exit $rc yet value moved to '${after}' (was '${before}') — unexpected"
  fi

  echo
  summarize
}

summarize() {
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed${SKIP:+ (${SKIP} skipped)}"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do red "   - $err"; done
  fi
  blue "═══════════════════════════════════════════════════"
  [ "$FAIL" -eq 0 ]
}

main "$@"
