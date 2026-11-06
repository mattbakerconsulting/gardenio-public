#!/usr/bin/env bash
# test-auto-parent-suppression.sh — bookkeeping-bead auto-parent SUPPRESSION E2E
# (backfill ka-0000005752 / ka-0000004462, fix MR !6699 / ka-0000002199).
#
# THE INVARIANT THIS MODULE PROTECTS (advice ka-0000002123). A non-work
# bookkeeping bead a gardener mints while running is NEVER a parent-child child of
# its goal epic. The module asserts the INVARIANT, not the enumeration of
# currently-known offending types — a new offending type is a test-extension, not
# a surprise. This is deliberate: an enumeration-scoped test (agent/session only)
# read GREEN for a full sprint while type=message leaked 41 children and type=mail
# 6 into this very epic (bug ka-0000004462), because the test mirrored the fix's
# skip list instead of the invariant it exists to hold.
#
# WHAT THE DAEMON DOES. On the static-actor auto-parent step (kbeads
# internal/server/auto_parent.go), a bead created by an agent whose task_id is a
# goal-bearing epic is auto-wired with a parent-child edge INTO that epic — so
# the agent's derivative work rolls up under its task. Bug ka-0000002199 /
# ka-0000004462: FOUR machine-bookkeeping bead types — type=agent, type=session,
# type=message (the shape `gb squawk` produces) and type=mail — were being
# auto-parented too. A gardener whose own task_id IS a goal-bearing epic mints
# these while it runs (spawns a crew ⇒ agent/session; squawks ⇒ message; mails a
# peer ⇒ mail), and because none of them close on the WORK lifecycle, an epic
# whose gardener's terminal_goal is "all children closed" becomes UNREACHABLE by
# construction — every spawn/squawk manufactures a fresh non-issue child that
# never drains, and inflates the closed-child count gardeners quote as progress.
# The fix (auto_parent.go, enforced in maybeAddAutoParent) skips these TYPES:
#
#     var autoParentSkipTypes = map[model.BeadType]bool{
#         model.BeadType("agent"): true, model.BeadType("session"): true,
#         model.BeadType("message"): true, model.BeadType("mail"): true,
#     }
#     ...
#     if autoParentSkipTypes[bead.Type] { return }
#
# WHY THIS NEEDS AN E2E. The unit suite (auto_parent_test.go) drives an
# in-memory mockStore: the static-actor inference path (FindAgentByName → read
# fields.task_id) and the real dep INSERT are stubbed out. The offending write
# is made by the DAEMON in response to a real create carrying a static token
# with X-Actor set — precisely the seam the unit test cannot cross. Before this
# module NO e2e spawned an agent against a goal-bearing epic and asserted the
# bookkeeping bead has NO parent-child edge (grep ka-0000002199 over
# gardenio/tests/ was 0). The three prior e2e references to auto-parent are prose
# comments that AVOID auto-parenting (--no-parent on --type=task fixtures), never
# assertions that the suppression fired.
#
# WHAT THIS MODULE ASSERTS. Against the deployed e2e-namespace daemon, acting AS
# a synthetic gardener agent whose fields.task_id IS a goal-bearing epic (the
# X-Actor static path the daemon infers lineage from):
#   1. A type=agent bead created by the gardener has NO parent-child edge to the
#      epic (the fix's primary guarantee).
#   2. A type=session bead created by the gardener has NO parent-child edge to the
#      epic (the sibling guarantee).
#   3. A type=message bead created by the gardener (the `gb squawk` shape) has NO
#      parent-child edge to the epic (the comms-path guarantee, bug ka-0000004462
#      — the leak an enumeration-scoped test missed).
#   4. POSITIVE CONTROL — a type=task bead created by the SAME gardener in the
#      SAME scope DOES get the parent-child edge to the epic. This proves the
#      skip is TYPE-scoped, not a globally-broken/disabled auto-parent: without
#      it, a daemon with auto-parent entirely dead would pass 1-3 vacuously.
#   5. NEGATIVE CONTROL on the epic's child set — the epic gains exactly ONE
#      parent-child child (the task), never the agent, session, or message, so an
#      over-firing regression that parents everything is caught from the
#      parent side too.
#
# WHY type=message AND NOT type=mail HERE. type=mail traverses the IDENTICAL
# server-side guard (autoParentSkipTypes[bead.Type], keyed on the bead TYPE
# alone), so it is proven by the same code path. But a `beads.bead.created` frame
# for a type=mail bead trips the controller's mail watcher (bridge/mail.go:47),
# which nudges the mail's assignee — a live side-effect against the shared,
# long-lived e2e-namespace daemon this module is otherwise careful to avoid (see
# POD-SPAWN HAZARD below). type=message fires no such door, so it is the safe
# representative of the comms class here; mail's skip is pinned by the unit suite
# (auto_parent_test.go: TestCreateBead_AutoParent_SkipsMailBead) and the backfill
# SQL test instead. If you extend this e2e to mail, fence the nudge (assignee
# empty AND project=e2e-test) the way the agent fixtures fence pod spawn.
#
# TEETH / NEGATIVE-CONTROL PROPERTY. On a PRE-fix daemon the gardener reproduced
# the leak deterministically — every spawn/squawk against a goal-bearing epic
# manufactured the parent-child edge (agent/session: ka-0000005752, 2 spawns 2
# leaks 0 misses; message: ka-0000004462, 41 leaked children on this epic). The
# agent/session skip shipped in 2026.217.14; the message/mail skip is newer
# (ka-0000004462). Pointing this module at a daemon predating the RELEVANT fix
# makes the matching suppression assertion FAIL, and a fully-fixed daemon passes
# all — a suppression test that never observed the unsuppressed behavior would be
# indistinguishable from one that asserts nothing. The e2e suite runs against the
# freshly-built release image, so it is green in CI; a red here on an old image is
# the fix not being deployed, not a broken test. NOTE: assertion 3 (message)
# reds on any daemon built before the message/mail skip landed — that is the
# ordering dependency called out in ka-0000004462, not a test defect.
#
# POD-SPAWN HAZARD (mirrors test-news-alive-freshness.sh / test-agents-live-view.sh).
# An active type=agent bead is a DESIRED pod to the controller reconciler
# (ListAgentBeads → create-missing) AND fires an event-door AgentSpawn on the
# `created` SSE frame. This module fences EVERY agent fixture three ways so it is
# a good citizen against the long-lived / shared daemon:
#   • project=e2e-test — evaluateSpawnCredentials (controller spawn_credential_guard.go)
#     REFUSES to launch a pod for a project with no Claude-OAuth secret, and no
#     e2e-test account is provisioned, so the event door fails closed.
#   • agent_state=failed — the reconciler create-missing loop skips a bead in a
#     terminal agent_state (reconciler.go), so the desired-vs-actual sweep never
#     cold-starts it. (agent_state is irrelevant to the daemon auto-parent logic
#     under test, so this changes nothing about assertions 1-5.)
#   • inline close + cleanup pod force-delete — every agent bead is closed the
#     instant its assertions complete (closed ⇒ excluded from ListAgentBeads),
#     and the trap force-deletes any pod that slipped through.
# The gardener agent bead itself is fenced the same way. Its status/agent_state do
# NOT affect the daemon's actor→task_id resolution (FindAgentByName matches
# regardless of status), so fencing it is free.
#
# Self-SKIPs (exit 0) when the beads Service is absent, so it is inert where no
# daemon is reachable. Idempotency: every bead carries the title/agent prefix
#   auto-parent-suppression-e2e-<pid>-<epoch>
# and is closed/deleted in the cleanup trap. Reruns do not collide.
#
# Quick run:
#   BEADS_HTTP_URL=http://localhost:8080 BEADS_TOKEN=… \
#     ./gardenio/tests/e2e/scripts/test-auto-parent-suppression.sh

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19148}"
KD_BIN="${KD_BIN:-kd}"
PREFIX="auto-parent-suppression-e2e"
SUFFIX="$$-$(date +%s)"
# project=e2e-test is the spawn fence (no OAuth account ⇒ event door refuses).
FIXTURE_PROJECT="e2e-test"
# A human operator so the daemon's fail-closed audit resolver
# (resolveAuditOperatorRequired) always attributes the create — independent of
# whatever BEADS_OPERATED_BY the harness exports. Matches the assignment's
# operated_by (slack:U0OPERATOR1).
OPERATED_BY="${AUTO_PARENT_OPERATED_BY:-slack:U0OPERATOR1}"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
BEADS_PF_PID=""

# Fixture registries are FILES, not arrays — and that is load-bearing (same
# reason as test-auto-close-cascade.sh's BEAD_REGISTRY). Every fixture is created
# as `ID=$(create_as_gardener …)`, and command substitution runs the function in
# a SUBSHELL, so an `arr+=("$id")` inside it mutates a copy that dies with the
# subshell, leaving the parent's array empty and the cleanup trap a silent no-op.
# (This module hit exactly that: the positive-control task leaked open because it
# was never registered.) Appending to a file crosses the subshell boundary, so
# teardown sees every id/agent actually created.
BEAD_REGISTRY="$(mktemp -t aps-beads.XXXXXX)"     # every bead id → close sweep
AGENT_REGISTRY="$(mktemp -t aps-agents.XXXXXX)"   # agent NAMES → pod force-delete

# The synthetic gardener whose task_id is the goal-bearing epic. Its NAME is the
# X-Actor value (= KD_ACTOR) the daemon resolves the acting agent by
# (FindAgentByName on fields.agent). Distinct from any real agent name.
GARDENER_NAME="${PREFIX}-gardener-${SUFFIX}"

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

id_from_json() { printf '%s' "$1" | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))" 2>/dev/null || printf ''; }

# is_json_array <string> — 0 (true) when the string's first non-space byte is
# `[`, else 1. `kd dep list --json` always emits a JSON array, but `kd dep
# children --json` prints "No dependents found." (NOT JSON) when the child set is
# empty, so callers must guard before piping into jq.
is_json_array() {
  case "$(printf '%s' "$1" | tr -d '[:space:]' | head -c1)" in
    '[') return 0 ;;
    *)   return 1 ;;
  esac
}

# has_parent_child_to <child-id> <parent-id> — "yes"/"no" whether `kd dep list
# <child-id>` (OUTGOING deps, child→parent orientation) carries a parent-child
# edge whose depends_on_id is <parent-id>. `kd dep list --json` emits a
# []*Dependency array where `.type` IS the dependency edge type (NOT the bead
# type) and `.depends_on_id` is the parent — the exact projection assertions 1-3
# need. A depless bead serializes as `[]`/`null`; `.[]?` tolerates both.
has_parent_child_to() {
  local child="$1" parent="$2" out
  out=$("$KD_BIN" dep list "$child" --json 2>/dev/null || printf '[]')
  is_json_array "$out" || { printf 'no'; return 0; }
  if printf '%s' "$out" | jq -e --arg p "$parent" \
      'any(.[]?; .type == "parent-child" and .depends_on_id == $p)' >/dev/null 2>&1; then
    printf 'yes'
  else
    printf 'no'
  fi
}

# epic_parent_child_children <epic-id> — space-joined list of bead ids that are
# parent-child CHILDREN of the epic (`kd dep children <epic-id> --json`, i.e. the
# INCOMING parent-child edges). NOTE the aligned row schema (kd children --json):
# `.type` is the BEAD type (task/agent/session) and `.dep_type` is the EDGE type,
# so the parent-child filter keys on `.dep_type` (falling back to `.type` only
# for a pre-alignment daemon that still carried the edge type in `.type`). The
# child bead id is `.bead_id` (== `.id`). Prints "" when the epic has no children
# (the non-JSON "No dependents found." case).
epic_parent_child_children() {
  local epic="$1" out
  out=$("$KD_BIN" dep children "$epic" --json 2>/dev/null || printf '')
  is_json_array "$out" || { printf ''; return 0; }
  printf '%s' "$out" | jq -r \
    '[.[]? | select((.dep_type // .type) == "parent-child") | (.bead_id // .id)] | join(" ")' \
    2>/dev/null || printf ''
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return 0
  fi
  if ! kubectl -n "$NAMESPACE" get svc "$BEADS_SVC" >/dev/null 2>&1; then
    yellow "beads service $BEADS_SVC not found in $NAMESPACE — skipping module"
    return 1
  fi
  blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
    "${BEADS_PF_PORT}:8080" >/tmp/${PREFIX}-pf.log 2>&1 &
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
  # Close every registered bead. Read from the FILE registry so ids created in
  # command-substitution subshells are actually seen (see BEAD_REGISTRY).
  if [ -s "$BEAD_REGISTRY" ]; then
    local id
    while IFS= read -r id; do
      [ -n "$id" ] || continue
      "$KD_BIN" close "$id" >/dev/null 2>&1 || true
    done < "$BEAD_REGISTRY"
  fi
  # Force-delete any pod the reconciler/event-door cold-started from a seeded
  # agent bead before its fence took effect. Best-effort; --wait=false for speed.
  if [ -s "$AGENT_REGISTRY" ]; then
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
  rm -f /tmp/${PREFIX}-*.log 2>/dev/null || true
}
trap cleanup EXIT

# create_as_gardener <title> <type> <extra -f field...> — create a bead ACTING AS
# the synthetic gardener (X-Actor = GARDENER_NAME via KD_ACTOR), which is what
# drives the daemon's static-actor auto-parent inference. Echoes the bead id
# (empty on failure). Registered for cleanup.
#
# --parent-task "" is LOAD-BEARING, not cosmetic. `kd create --parent-task`
# defaults to $BOAT_TASK_ID; inside a live agent pod that env var is set to the
# agent's OWN claimed task. A non-empty --parent-task makes the CLI (a) wire an
# explicit client-side parent dep and (b) set X-No-Auto-Parent — which SUPPRESSES
# the very server-side auto-parent step this module tests, silently making every
# assertion vacuous (and 500ing when $BOAT_TASK_ID names a bead absent from the
# ephemeral daemon). Forcing it empty leaves the server-side auto-parent step
# enabled (create.go: empty parent-task + no --no-parent ⇒ auto-parent fires),
# which is exactly the path under test. The positive control (assertion 3) is
# what caught the missing flag.
create_as_gardener() {
  local title="$1" type="$2"; shift 2
  local out id
  out=$(KD_ACTOR="$GARDENER_NAME" "$KD_BIN" create "$title" \
    --type="$type" \
    --project "$FIXTURE_PROJECT" --force \
    --operated-by "$OPERATED_BY" \
    --parent-task "" \
    "$@" --json 2>/dev/null || printf '{}')
  id=$(id_from_json "$out")
  # Register via the FILE registry: this function is called in a $(...) subshell,
  # so an array append here would not survive back to the parent.
  [ -n "$id" ] && echo "$id" >> "$BEAD_REGISTRY"
  printf '%s' "$id"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " agent/session auto-parent SUPPRESSION E2E (ka-0000002199)"
  blue "═══════════════════════════════════════════════════"

  if ! setup_portforward; then
    skip "auto-parent-suppression" "beads daemon not reachable"
    echo; blue "═══════════════════════════════════════════════════"
    green " ⊘ SKIPPED (no daemon)"
    blue "═══════════════════════════════════════════════════"
    return 0
  fi

  # Reachability probe — a bare kd list against the daemon. SKIP (not FAIL) if
  # the tunnel is up but the daemon rejects/returns nothing, matching the
  # inert-when-unreachable contract of the sibling daemon-only modules.
  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    skip "auto-parent-suppression" "kd cannot reach daemon at ${BEADS_HTTP_URL:-?}"
    return 0
  fi
  green "  Daemon reachable: ${BEADS_HTTP_URL:-(env)}"
  echo

  # ── Fixtures ──────────────────────────────────────────────────
  # 1. The goal-bearing epic — the gardener's task_id, and the thing nothing but
  #    a real work bead may join. Created with a NEUTRAL actor + --no-parent so
  #    it is hermetic (no stray lineage of its own).
  local epic_out EPIC
  epic_out=$(KD_ACTOR="${PREFIX}-harness-${SUFFIX}" "$KD_BIN" create \
    "${PREFIX}-goal-epic-${SUFFIX}" --type=epic \
    --project "$FIXTURE_PROJECT" --force \
    --operated-by "$OPERATED_BY" --no-parent --json 2>/dev/null || printf '{}')
  EPIC=$(id_from_json "$epic_out")
  if [ -n "$EPIC" ]; then echo "$EPIC" >> "$BEAD_REGISTRY"; fi

  # 2. The synthetic gardener agent bead. fields.task_id = the epic ⇒ the daemon
  #    infers "this actor is working the epic" and would auto-parent its
  #    derivative beads under it. type=agent + agent_state=failed + project=e2e-test
  #    fence it from ever spawning a pod. Created with a neutral actor so its own
  #    create is not self-referential; type=agent is itself skip-typed so it
  #    would not auto-parent regardless. --no-parent keeps it hermetic.
  local cap_out GARDENER_BEAD
  cap_out=$(KD_ACTOR="${PREFIX}-harness-${SUFFIX}" "$KD_BIN" create \
    "${PREFIX}-gardener-bead-${SUFFIX}" --type=agent \
    --project "$FIXTURE_PROJECT" --force \
    --operated-by "$OPERATED_BY" --no-parent \
    -f "agent=${GARDENER_NAME}" \
    -f "role=crew-e2e" \
    -f "project=${FIXTURE_PROJECT}" \
    -f "agent_state=failed" \
    -f "task_id=${EPIC}" \
    -f "slack_user_id=U0OPERATOR1" \
    --json 2>/dev/null || printf '{}')
  GARDENER_BEAD=$(id_from_json "$cap_out")
  if [ -n "$GARDENER_BEAD" ]; then
    echo "$GARDENER_BEAD" >> "$BEAD_REGISTRY"
    echo "$GARDENER_NAME" >> "$AGENT_REGISTRY"
  fi

  if [ -z "$EPIC" ] || [ -z "$GARDENER_BEAD" ]; then
    fail "fixture-setup" "could not create epic (${EPIC:-∅}) + gardener agent bead (${GARDENER_BEAD:-∅})"
    summarize; return
  fi
  dim "  Epic (goal-bearing): $EPIC"
  dim "  Gardener agent: $GARDENER_BEAD (name=$GARDENER_NAME, task_id=$EPIC)"
  echo

  # The crew AGENT-under-test name, needed for the pod force-delete fence.
  local CREW_NAME="${PREFIX}-crew-${SUFFIX}"
  echo "$CREW_NAME" >> "$AGENT_REGISTRY"

  # ── Create the bookkeeping beads + control AS the gardener ─────
  # These carry NO explicit parent — the ONLY thing that could wire a
  # parent-child edge is the daemon's static-actor auto-parent step, which is
  # exactly what is under test. (Passing --no-parent would suppress that step and
  # make the test vacuous, so we deliberately do NOT.)
  local AGENT_UT SESSION_UT MESSAGE_UT TASK_CTRL

  # type=agent under test — must NOT auto-parent. Fenced (failed + e2e-test).
  AGENT_UT=$(create_as_gardener "${PREFIX}-crew-agent-${SUFFIX}" agent \
    -f "agent=${CREW_NAME}" \
    -f "role=crew-e2e" \
    -f "project=${FIXTURE_PROJECT}" \
    -f "agent_state=failed" \
    -f "task_id=${EPIC}")

  # type=session under test — must NOT auto-parent. Data-kind, never a pod.
  SESSION_UT=$(create_as_gardener "${PREFIX}-crew-session-${SUFFIX}" session \
    -f "agent=${CREW_NAME}" \
    -f "project=${FIXTURE_PROJECT}")

  # type=message under test (bug ka-0000004462) — the shape `gb squawk` produces.
  # Must NOT auto-parent. This is the leak an enumeration-scoped test (agent +
  # session only) missed for a full sprint. No extra -f fields: the daemon's
  # type-config for `message` rejects agent/project as unknown fields, and the
  # skip keys on bead.Type alone, so the bare type is all this fixture needs.
  MESSAGE_UT=$(create_as_gardener "${PREFIX}-crew-squawk-${SUFFIX}" message)

  # POSITIVE CONTROL: type=task by the SAME gardener — MUST auto-parent to the
  # epic. Proves the skip is type-scoped, not a dead/disabled auto-parent.
  TASK_CTRL=$(create_as_gardener "${PREFIX}-work-task-${SUFFIX}" task)

  # Close the agent fixture's spawn exposure ASAP: the agent bead is the pod
  # hazard, so close it now that it exists (assertions read deps, which survive
  # close). The session/message (data-kind) and task are harmless open.
  # NOTE: we assert BEFORE closing the agent so a close-side cascade can't be
  # blamed for a missing edge; closing happens in cleanup.

  if [ -z "$AGENT_UT" ] || [ -z "$SESSION_UT" ] || [ -z "$MESSAGE_UT" ] || [ -z "$TASK_CTRL" ]; then
    fail "create-as-gardener" "agent=${AGENT_UT:-∅} session=${SESSION_UT:-∅} message=${MESSAGE_UT:-∅} task=${TASK_CTRL:-∅}"
    summarize; return
  fi
  dim "  Agent-under-test:   $AGENT_UT (name=$CREW_NAME)"
  dim "  Session-under-test: $SESSION_UT"
  dim "  Message-under-test: $MESSAGE_UT"
  dim "  Task (pos control): $TASK_CTRL"
  echo

  # ── Assertions ────────────────────────────────────────────────
  blue "Suppression: agent/session/message beads must NOT auto-parent into the goal epic"

  # 1. agent bead has NO parent-child edge to the epic.
  local a_edge; a_edge=$(has_parent_child_to "$AGENT_UT" "$EPIC")
  if [ "$a_edge" = "no" ]; then
    pass "type=agent bead has NO parent-child edge to the goal epic"
  else
    fail "type=agent bead auto-parented into the goal epic" \
      "kd dep list $AGENT_UT shows a parent-child edge to $EPIC (leak ka-0000002199 — pre-2026.217.14 daemon?)"
  fi

  # 2. session bead has NO parent-child edge to the epic.
  local s_edge; s_edge=$(has_parent_child_to "$SESSION_UT" "$EPIC")
  if [ "$s_edge" = "no" ]; then
    pass "type=session bead has NO parent-child edge to the goal epic"
  else
    fail "type=session bead auto-parented into the goal epic" \
      "kd dep list $SESSION_UT shows a parent-child edge to $EPIC (leak ka-0000002199)"
  fi

  # 3. message bead has NO parent-child edge to the epic (bug ka-0000004462 —
  #    the `gb squawk` shape). This is the assertion the module was blind to when
  #    it mirrored the enumeration {agent,session} instead of the invariant.
  local m_edge; m_edge=$(has_parent_child_to "$MESSAGE_UT" "$EPIC")
  if [ "$m_edge" = "no" ]; then
    pass "type=message bead has NO parent-child edge to the goal epic"
  else
    fail "type=message bead auto-parented into the goal epic" \
      "kd dep list $MESSAGE_UT shows a parent-child edge to $EPIC (leak ka-0000004462 — daemon predates the message/mail skip). Every squawk from a goal-epic gardener pollutes its own terminal goal."
  fi

  echo
  blue "Positive control: a real work bead by the SAME gardener DOES auto-parent"

  # 4. POSITIVE CONTROL — the task DID auto-parent. Anti-vacuity: if this fails,
  #    auto-parent is globally broken/disabled and assertions 1-3 proved nothing.
  local t_edge; t_edge=$(has_parent_child_to "$TASK_CTRL" "$EPIC")
  if [ "$t_edge" = "yes" ]; then
    pass "type=task control bead auto-parented into the goal epic (skip is type-scoped)"
  else
    fail "positive control FAILED: type=task bead did NOT auto-parent" \
      "kd dep list $TASK_CTRL has no parent-child edge to $EPIC — auto-parent appears globally broken/disabled, so the suppression assertions above are VACUOUS. Is the daemon >= 2026.217.14 and the static-actor path reachable?"
  fi

  echo
  blue "Parent-side negative control: the epic's child set is exactly {task}"

  # 5. From the EPIC side: exactly one parent-child child (the task), never the
  #    agent, session, or message. Catches an over-firing regression that parents
  #    everything (which the child-side checks 1/2/3 would also catch, but the
  #    parent-side view guards against a dep-orientation quirk masking it).
  local children; children=$(epic_parent_child_children "$EPIC")
  dim "  epic parent-child children: [${children}]"
  if printf ' %s ' "$children" | grep -q " ${TASK_CTRL} "; then
    pass "epic child set CONTAINS the task control bead"
  else
    fail "epic child set is missing the task control bead" \
      "expected $TASK_CTRL among the epic's parent-child children; got [${children}]"
  fi
  if printf ' %s ' "$children" | grep -q " ${AGENT_UT} "; then
    fail "epic child set CONTAINS the agent bead (leak)" \
      "$AGENT_UT must not be a parent-child child of $EPIC"
  else
    pass "epic child set does NOT contain the agent bead"
  fi
  if printf ' %s ' "$children" | grep -q " ${SESSION_UT} "; then
    fail "epic child set CONTAINS the session bead (leak)" \
      "$SESSION_UT must not be a parent-child child of $EPIC"
  else
    pass "epic child set does NOT contain the session bead"
  fi
  if printf ' %s ' "$children" | grep -q " ${MESSAGE_UT} "; then
    fail "epic child set CONTAINS the message bead (leak)" \
      "$MESSAGE_UT must not be a parent-child child of $EPIC (leak ka-0000004462)"
  else
    pass "epic child set does NOT contain the message bead"
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
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"
  [ "$FAIL" -eq 0 ]
}

main "$@"
