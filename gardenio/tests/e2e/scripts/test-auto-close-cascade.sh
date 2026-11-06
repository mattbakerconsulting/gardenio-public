#!/usr/bin/env bash
# test-auto-close-cascade.sh — kbeads daemon auto-close cascade E2E (ka-0000003089)
#
# The daemon-side cascade at the single bead-close choke point
# (kbeads/internal/server/auto_close.go afterBeadClosed) composes five
# transitions that, before this module, had NO end-to-end coverage:
#
#   1. last-child close   -> parent epic drains and auto-closes  (ka-0000001587)
#   2. MR-bead close      -> producing LEAF issue bead closes     (ka-0000005910)
#   3. decision close     -> `blocks`-gated beads park to blocked (ka-0000005620)
#   4. live gardener bound -> epic drain DEFERS (does not close)   (ka-0000001525)
#   5. any parent close   -> still-open orphaned children park    (ka-0000000504)
#
# Why this module exists. auto_close_test.go is a 44-test unit suite, but it
# runs against an in-memory mockStore: the SQL reverse-dep / ParentSubtreeID
# enumeration the cascade is built on is stubbed out, and the
# bridge-writes-merge-state -> daemon-reacts seam is never crossed. The
# pre-existing e2e scripts do not close the gap either — test-epics-children.sh
# scenario 5 closes the epic MANUALLY (so last-child auto-close never fires; the
# assertion passes identically with the cascade ripped out), and
# test-mr-merge-flow.sh only exercises the jira-bridge Review transition. This
# module drives the REAL daemon over HTTP and asserts the transitions the daemon
# performed on its own.
#
# The regressions it catches are silent by construction — the cascade is
# best-effort and never fails the originating close, so a broken cascade returns
# HTTP 200 and simply stops transitioning work. Concretely: leaf close-on-merge
# silently stops closing merged work (beads churn in `gb ready` forever), or the
# live-gardener guard regresses and an epic is hard-closed out from under a
# running gardener, making the lane invisible to /plans.
#
# EVERY positive assertion is paired with the daemon's own attribution
# (closed_by=system:auto-close) rather than mere terminal status, so a test bead
# closed by the cleanup trap, by the harness, or by our own explicit `kd close`
# can never masquerade as a cascade. Each scenario also carries a NEGATIVE
# CONTROL (a gate that must NOT fire) so the module fails if the cascade
# over-fires, not just if it under-fires.
#
# Scenarios tested:
#   1. Last-child epic drain — epic stays OPEN at 1-of-2 children closed, then
#      auto-closes (closed_by=system:auto-close) when the LAST child closes.
#   2. Merged-MR leaf close — closing a merged type=merge-request bead closes the
#      leaf that `produces` it; an UNMERGED MR bead must NOT close its leaf.
#   3. Composed cascade — one MR-bead close closes the leaf AND drains the leaf's
#      parent epic in a single propagation (the seam no unit test crosses).
#   4. Decision-close park — a `blocks`-gated bead parks to `blocked` (NOT
#      closed) when its gating decision closes; a `parent-child`-linked bead that
#      keeps a second live parent is left alone (only `blocks` edges gate the
#      decision-close park, and orphan-park exempts a child with a live parent).
#   5. Live-gardener defer — a drained epic with an OPEN, non-disabled
#      trigger/schedule descendant must NOT be closed; flipping that gardener to
#      enabled=false releases the drain on the next child close.
#   6. Orphan-park — closing a parent that still has OPEN parent-child children
#      parks each to `blocked` (NOT closed); a child that keeps another live
#      parent is left alone (the hasOtherLiveParent exemption).
#
# Prereqs:
#   - kd CLI on PATH (KD_BIN to override)
#   - reachable beads daemon: BEADS_HTTP_URL set, or kubectl port-forward access
#     to svc/${NAMESPACE}-beads
#   - python3 (JSON extraction, per the house convention in these scripts)
#
# Quick run:
#   BEADS_HTTP_URL=http://localhost:8080 ./gardenio/tests/e2e/scripts/test-auto-close-cascade.sh

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""

# Fixture registry for the cleanup trap.
#
# This is a FILE, not an array, and that is load-bearing. Every fixture is
# created as `ID=$(mk_bead ...)`, and command substitution runs the function in a
# SUBSHELL — so a `CREATED_BEADS+=("$id")` inside mk_bead mutates a copy that
# dies with the subshell, leaving the parent's array empty and the trap a silent
# no-op that deletes nothing. (Verified against a live daemon: the array-based
# form leaked every fixture of every run.) Appending to a file crosses the
# subshell boundary, so teardown sees the ids that were actually created.
BEAD_REGISTRY="$(mktemp -t autoclose-beads.XXXXXX)"

# Load shared retry helpers so a transient port-forward drop doesn't fail a
# whole scenario (kd_retry, retry_on_empty).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-retry.sh"

# Per-run unique actor: this module may run against a SHARED daemon alongside
# other shards, and every assertion here keys on the daemon's own
# `system:auto-close` / `system:decision-gate` attribution. A unique actor keeps
# our explicit closes distinguishable from another run's (mirrors
# test-decisions-yield.sh's KD_ACTOR discipline).
export KD_ACTOR="${KD_ACTOR:-e2e-test}-autoclose-$$"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() {
  local name="$1"
  green "  ✓ $name"
  PASS=$((PASS + 1))
}

fail() {
  local name="$1"
  local reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

assert_equals() {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$expected" = "$actual" ]; then
    pass "$name (= '$expected')"
  else
    fail "$name" "expected '$expected', got '$actual'"
  fi
}

# json_field — project one dotted path out of a kd --json object. Returns "" for
# a missing path so callers can assert on a plain string compare.
json_field() {
  local json="$1"
  local path="$2"
  echo "$json" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print('')
    sys.exit(0)
for part in '$path'.split('.'):
    if isinstance(d, dict):
        d = d.get(part)
    else:
        d = None
        break
print('' if d is None else d)
" 2>/dev/null || echo ""
}

# bead_status / bead_closed_by — the two projections every assertion needs.
bead_status()    { json_field "$(retry_on_empty "$KD_BIN" show "$1" --json 2>/dev/null)" "status"; }
bead_closed_by() { json_field "$(retry_on_empty "$KD_BIN" show "$1" --json 2>/dev/null)" "closed_by"; }

# await_status — poll until a bead reaches want, up to CASCADE_TIMEOUT seconds.
#
# The cascade runs SYNCHRONOUSLY inside the close request (auto_close.go is
# invoked before handleCloseBead writes its response), so the transition is
# already durable when `kd close` returns. We poll anyway rather than assert
# once: this module runs against a shared daemon over a port-forward, and a
# fixed `sleep` would either flake under load or waste wall-clock on every
# assertion. Polling returns the instant the state is observed and only spends
# the full timeout on a genuine failure.
#
# Returns the final observed status on stdout either way, so the caller reports
# what it actually saw instead of a bare timeout.
CASCADE_TIMEOUT="${CASCADE_TIMEOUT:-30}"
await_status() {
  local id="$1"
  local want="$2"
  local deadline=$((SECONDS + CASCADE_TIMEOUT))
  local got=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    got=$(bead_status "$id")
    [ "$got" = "$want" ] && { echo "$got"; return 0; }
    sleep 1
  done
  echo "$got"
  return 1
}

# assert_stays — assert a bead does NOT reach `unwanted` within a settle window.
#
# This is the negative-control primitive, and it is the reason this module can
# tell "the cascade correctly declined" apart from "the cascade hadn't run yet".
# A negative assertion made immediately after a close would pass even with a
# badly-broken (merely slow) cascade, so we hold for SETTLE_SECONDS and fail on
# the FIRST sighting of the unwanted state rather than only sampling at the end.
SETTLE_SECONDS="${SETTLE_SECONDS:-5}"
assert_stays() {
  local name="$1"
  local id="$2"
  local unwanted="$3"
  local deadline=$((SECONDS + SETTLE_SECONDS))
  local got=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    got=$(bead_status "$id")
    if [ "$got" = "$unwanted" ]; then
      fail "$name" "bead $id reached '$unwanted' but must not have"
      return 1
    fi
    sleep 1
  done
  pass "$name (stayed '$got', never '$unwanted')"
  return 0
}

# mk_bead — create a test bead and register it for cleanup.
#
# --no-parent is REQUIRED here, not cosmetic. The daemon auto-parents a new bead
# to the creating agent's task bead (ka-0000007224); inside this module that
# would splice every fixture into a live epic's subtree, so a fixture close could
# drain a REAL epic and our own epic-drain assertions would race an unrelated
# subtree. --no-parent gives each scenario a hermetic graph whose only edges are
# the ones it writes itself.
#
# EVERY fixture carries `fixture:e2e:auto-close-cascade` (ka-0000000809), not
# just scenario 5's trigger. That label is the marker isE2EFixture /
# isE2EDecisionRespawn key on to refuse spawning a real agent pod for a test
# fixture on the shared prod cluster. Scenario 4's decision fixture originally
# lacked it, so closing it drove the un-guarded decision-resume respawn path
# (nudgeAgent → respawnAgentForDecision) into minting a live CrashLooping crew
# pod. Labelling here closes the hole for every current AND future fixture by
# default, independent of the bridge-side project=e2e-test belt-and-suspenders.
mk_bead() {
  local title="$1"
  local type="$2"
  shift 2
  local out id
  out=$(kd_retry "$KD_BIN" create "$title" --type="$type" --no-parent \
    -l "e2e:autoclose-$$" -l "project:e2e-test" \
    -l "fixture:e2e:auto-close-cascade" "$@" --json 2>/dev/null)
  id=$(json_field "$out" "id")
  if [ -n "$id" ]; then
    # See BEAD_REGISTRY: this runs in a subshell, so the id must go to a file.
    echo "$id" >> "$BEAD_REGISTRY"
  fi
  echo "$id"
}

# link — write one dependency edge (child/blocked side first, per kd dep add).
link() {
  kd_do "dep add $1 -> $2 ($3)" "$KD_BIN" dep add "$1" "$2" --type "$3"
}

# kd_do — run a fixture-mutating kd command, reporting a failure as a test
# failure instead of letting `set -e` kill the run.
#
# Every scenario here is a SETUP-then-observe shape, so a silent `set -e` death
# mid-setup would abort the script before the summary block and print no reason
# at all — the failure mode that made the boolean-typed `enabled` field (which
# rejects the quoted-string form the other fields use) cost a debugging cycle.
# Reporting and continuing means one broken step names itself and the remaining
# scenarios still run.
kd_do() {
  local what="$1"; shift
  if ! kd_retry "$@" >/dev/null 2>&1; then
    fail "setup: $what" "command failed: $*"
    return 1
  fi
  return 0
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19143
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-autoclose.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  else
    dim "  WARNING: No token found; kd server may reject requests"
  fi
}

# cleanup — DELETE every fixture rather than close it.
#
# Closing them would fire the very cascade under test against whatever graph
# survives the run, so the teardown of a failed run could mutate beads a later
# scenario (or a concurrent shard) is asserting on. Delete is inert and also
# leaves no TEST-DELETE-ME residue in the shared daemon. Deletion order is
# reversed so an MR/decision bead outlives the edges pointing at it.
cleanup() {
  if [ -s "$BEAD_REGISTRY" ]; then
    # Reverse order so an MR / decision bead outlives the edges pointing at it.
    local id
    while IFS= read -r id; do
      [ -n "$id" ] && "$KD_BIN" delete "$id" >/dev/null 2>&1 || true
    done < <(tac "$BEAD_REGISTRY" 2>/dev/null || tail -r "$BEAD_REGISTRY" 2>/dev/null)
  fi
  rm -f "$BEAD_REGISTRY"
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " kbeads auto-close cascade E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! kd_retry "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL after $RETRY_ATTEMPTS attempts"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TAG
  TAG="e2e-autoclose-$$-$(date +%s)"

  # ── Scenario 1: last-child close drains the parent epic ─────────
  # The gap test-epics-children.sh leaves open: it closes the epic by hand, so
  # its "epic is closed" assertion holds even with the cascade removed. Here the
  # epic is NEVER closed by us — if it ends up closed, the daemon did it.
  blue "Scenario 1: last-child close drains the parent epic"
  local EPIC C1 C2
  EPIC=$(mk_bead "TEST-DELETE-ME autoclose epic $TAG" "epic")
  C1=$(mk_bead "TEST-DELETE-ME autoclose child-1 $TAG" "task")
  C2=$(mk_bead "TEST-DELETE-ME autoclose child-2 $TAG" "task")
  if [ -z "$EPIC" ] || [ -z "$C1" ] || [ -z "$C2" ]; then
    fail "scenario-1-setup" "could not create epic + 2 children"
  else
    dim "  Epic: $EPIC  children: $C1, $C2"
    link "$C1" "$EPIC" "parent-child"
    link "$C2" "$EPIC" "parent-child"

    # NEGATIVE CONTROL: one of two children closed — the epic must stay open.
    # Without this, an epic that auto-closed on the FIRST child (or one closed
    # by something else entirely) would still satisfy the positive assertion.
    kd_do "close child-1" "$KD_BIN" close "$C1"
    assert_stays "epic stays open while a sibling is still open" "$EPIC" "closed"

    # Now the LAST open child closes -> the daemon should drain the epic.
    kd_do "close LAST child" "$KD_BIN" close "$C2"
    local epic_status
    epic_status=$(await_status "$EPIC" "closed" || true)
    assert_equals "epic auto-closed after LAST child closed" "closed" "$epic_status"
    # Attribution is what proves the DAEMON closed it, not us or the harness.
    assert_equals "epic close attributed to the daemon cascade" \
      "system:auto-close" "$(bead_closed_by "$EPIC")"
  fi
  echo

  # ── Scenario 2: merged-MR bead closes its producing leaf ────────
  # Crosses the bridge->daemon seam: the bridge's observable act is writing
  # merge state onto the canonical type=merge-request bead and closing it. We
  # reproduce exactly that and assert the daemon closes the producing leaf.
  blue "Scenario 2: merged MR bead closes its producing leaf"
  local LEAF MR_BEAD
  LEAF=$(mk_bead "TEST-DELETE-ME autoclose leaf $TAG" "task")
  MR_BEAD=$(mk_bead "TEST-DELETE-ME autoclose mr $TAG" "merge-request")
  if [ -z "$LEAF" ] || [ -z "$MR_BEAD" ]; then
    fail "scenario-2-setup" "could not create leaf + merge-request beads"
  else
    dim "  Leaf: $LEAF  MR bead: $MR_BEAD"
    # issue --produces--> merge-request is the edge the gitlab-bridge writes and
    # the edge linkedMRBeads walks; the cascade gates on the FORWARD edge, so a
    # reverse-only link would (correctly) not close the leaf.
    link "$LEAF" "$MR_BEAD" "produces"
    kd_do "mark MR bead merged" "$KD_BIN" update "$MR_BEAD" -f 'mr_merged="true"' -f 'mr_state="merged"'
    kd_do "close merged MR bead" "$KD_BIN" close "$MR_BEAD"

    local leaf_status
    leaf_status=$(await_status "$LEAF" "closed" || true)
    assert_equals "leaf auto-closed when its merged MR bead closed" "closed" "$leaf_status"
    assert_equals "leaf close attributed to the daemon cascade" \
      "system:auto-close" "$(bead_closed_by "$LEAF")"
  fi

  # NEGATIVE CONTROL: an UNMERGED MR bead must not close its leaf. This is the
  # assertion that would catch a regression collapsing isMRMerged into "the MR
  # bead closed at all" — the difference between closing merged work and closing
  # work whose MR was abandoned.
  local LEAF2 MR_BEAD2
  LEAF2=$(mk_bead "TEST-DELETE-ME autoclose leaf-unmerged $TAG" "task")
  MR_BEAD2=$(mk_bead "TEST-DELETE-ME autoclose mr-unmerged $TAG" "merge-request")
  if [ -n "$LEAF2" ] && [ -n "$MR_BEAD2" ]; then
    link "$LEAF2" "$MR_BEAD2" "produces"
    kd_do "mark MR bead closed-not-merged" "$KD_BIN" update "$MR_BEAD2" -f 'mr_state="closed"'
    kd_do "close unmerged MR bead" "$KD_BIN" close "$MR_BEAD2"
    assert_stays "leaf stays open when its MR closed WITHOUT merging" "$LEAF2" "closed"
  fi
  echo

  # ── Scenario 3: composed cascade (MR -> leaf -> parent epic) ────
  # The propagation no unit test crosses end to end: autoCloseMergedLeaf
  # re-enters afterBeadClosed, so ONE close should transition two other beads.
  blue "Scenario 3: composed cascade — MR close drains leaf AND its parent epic"
  local EPIC3 LEAF3 MR3
  EPIC3=$(mk_bead "TEST-DELETE-ME autoclose composed-epic $TAG" "epic")
  LEAF3=$(mk_bead "TEST-DELETE-ME autoclose composed-leaf $TAG" "task")
  MR3=$(mk_bead "TEST-DELETE-ME autoclose composed-mr $TAG" "merge-request")
  if [ -z "$EPIC3" ] || [ -z "$LEAF3" ] || [ -z "$MR3" ]; then
    fail "scenario-3-setup" "could not create composed-cascade fixtures"
  else
    dim "  Epic: $EPIC3  leaf: $LEAF3  MR: $MR3"
    link "$LEAF3" "$EPIC3" "parent-child"
    link "$LEAF3" "$MR3" "produces"
    kd_do "mark composed MR merged" "$KD_BIN" update "$MR3" -f 'mr_merged="true"' -f 'mr_state="merged"'

    # A single close of the MR bead. We touch neither the leaf nor the epic.
    kd_do "close composed MR bead" "$KD_BIN" close "$MR3"

    local leaf3_status epic3_status
    leaf3_status=$(await_status "$LEAF3" "closed" || true)
    assert_equals "composed: leaf closed from the MR merge" "closed" "$leaf3_status"
    epic3_status=$(await_status "$EPIC3" "closed" || true)
    assert_equals "composed: parent epic drained by the leaf's auto-close" "closed" "$epic3_status"
    assert_equals "composed: epic close attributed to the daemon cascade" \
      "system:auto-close" "$(bead_closed_by "$EPIC3")"
  fi
  echo

  # ── Scenario 4: decision close parks its gated beads ────────────
  # ka-0000005620: a CLOSED decision's blocks-edge stops suppressing its bead in
  # FilterNoOpenDeps, so gated work fell back into the ready lane the instant the
  # operator answered. Parking to `blocked` is the durable gate.
  blue "Scenario 4: decision close parks its blocks-gated beads"
  local GATED SIBLING SIBLING_EPIC DECISION
  GATED=$(mk_bead "TEST-DELETE-ME autoclose gated $TAG" "task")
  SIBLING=$(mk_bead "TEST-DELETE-ME autoclose decision-child $TAG" "task")
  # SIBLING's other, still-open parent. The negative control below asserts that
  # closing the DECISION does not park SIBLING; but a decision can itself be a
  # parent-child parent, and closing ANY parent now parks its still-open orphaned
  # children (maybeParkOrphanedChildren, ka-0000000504 — landed after this module
  # was first written). That orphan-park is CORRECT and has its own coverage
  # (scenario 6); it is a different cascade from the decision-gate under test
  # here. To keep this control aimed only at the decision-gate path — parking the
  # WRONG edge type (parent-child instead of blocks) — give SIBLING a second live
  # parent so orphan-park's documented `hasOtherLiveParent` exemption applies: a
  # child with another live parent is not orphaned and is left alone. SIBLING then
  # reaches `blocked` ONLY if the decision-gate path wrongly parks a parent-child
  # neighbour, which is exactly the regression this control exists to catch.
  SIBLING_EPIC=$(mk_bead "TEST-DELETE-ME autoclose decision-child-live-parent $TAG" "epic")
  # The daemon rejects an option-less decision (decision_validation.go), so the
  # fixture carries a real two-option payload like any agent-filed decision.
  DECISION=$(mk_bead "TEST-DELETE-ME autoclose decision $TAG" "decision" \
    -f 'prompt="e2e auto-close cascade fixture — not a real decision"' \
    -f 'options=[{"id":"a","label":"Alpha"},{"id":"b","label":"Beta"}]')
  if [ -z "$GATED" ] || [ -z "$DECISION" ]; then
    fail "scenario-4-setup" "could not create gated task + decision"
  else
    dim "  Gated: $GATED  decision: $DECISION  sibling: $SIBLING (live parent $SIBLING_EPIC)"
    link "$GATED" "$DECISION" "blocks"
    # NEGATIVE CONTROL: only `blocks` edges gate the decision-close park. A
    # parent-child neighbour of the same decision must be left alone by the
    # decision-gate path, so a regression that parks every reverse-dep (not just
    # blocks) is caught. SIBLING keeps a second live parent (SIBLING_EPIC, never
    # closed) so the unrelated orphan-park cascade exempts it and cannot muddy
    # this signal.
    if [ -n "$SIBLING" ]; then
      link "$SIBLING" "$DECISION" "parent-child"
      [ -n "$SIBLING_EPIC" ] && link "$SIBLING" "$SIBLING_EPIC" "parent-child"
    fi

    kd_do "close gating decision" "$KD_BIN" close "$DECISION"

    local gated_status
    gated_status=$(await_status "$GATED" "blocked" || true)
    assert_equals "blocks-gated bead parked to blocked on decision close" "blocked" "$gated_status"
    # A park is NOT a close — parkDecisionGatedBead updates status and
    # deliberately does not re-enter afterBeadClosed. Asserting the bead is not
    # closed keeps "park" and "close" from silently converging.
    assert_equals "parked bead was NOT closed (park != close)" "" "$(bead_closed_by "$GATED")"

    if [ -n "$SIBLING" ]; then
      assert_stays "parent-child neighbour of the decision is NOT parked" "$SIBLING" "blocked"
    fi
  fi
  echo

  # ── Scenario 5: live gardener defers the epic drain ──────────────
  # Bug ka-0000001525: a trigger/schedule is data-kind, so Gate A's issue-kind
  # allowlist cannot see it, and a mid-loop gardener's epic was hard-closed the
  # instant its last audit step drained.
  #
  # The fixture trigger is INERT BY FOUR INDEPENDENT MECHANISMS, because it must
  # be impossible for a test fixture to spawn a real agent on a shared daemon:
  #   1. No `trigger:*` source label. EVERY TriggerSource.LoadTriggers filters on
  #      one (trigger:bead / :schedule / :multi / :jira / :gitlab / :webhook /
  #      :slack-reaction), so no source can even enumerate this bead.
  #   2. `fixture:e2e:` label — isE2EFixture (triggers.go) makes the bead source
  #      and the gardener sweeps skip it explicitly (ka-0000005435). Applied to
  #      EVERY fixture by mk_bead now (ka-0000000809), not just this trigger.
  #   3. action=assert — `action` is the type's one REQUIRED field, and assert is
  #      the only value that spawns no agent and bypasses the SpawnGuard by
  #      construction (ka-0000008157). It also clears both MUTATING gardener
  #      sweeps at their first gate: dark-lane skips any action that is not
  #      spawn_agent/apply_formula (trigger_dark_lane.go), and pouring-stall
  #      requires apply_formula (isPourStallCandidate). Do NOT "simplify" this to
  #      spawn_agent or apply_formula — either drops the fixture into a sweep's
  #      mutating population where only the fixture label saves it.
  #   4. No `gardener` field — isGardenerTrigger is false, so the liveness and
  #      closed-epic-reaper sweeps (the ones that comment/close) skip at gate 1.
  # `enabled` is deliberately never set to "true" either: that would place the
  # bead in closed_enabled_drain's {status=closed, enabled=true} roster once this
  # scenario closes it.
  # It exists only so firstLiveGardenerChild sees an open, non-disabled gardener
  # descendant; `enabled` is deliberately left UNSET because the engine reads
  # absent-as-live, which is exactly the state the guard must honor.
  blue "Scenario 5: a live gardener child defers the epic drain"
  local EPIC5 C5 GARDENER
  EPIC5=$(mk_bead "TEST-DELETE-ME autoclose gardener-epic $TAG" "epic")
  C5=$(mk_bead "TEST-DELETE-ME autoclose gardener-epic-child $TAG" "task")
  # The fixture:e2e:auto-close-cascade label (mechanism #2) is now applied by
  # mk_bead to every fixture, so it is not repeated here — only the trigger's
  # own action=assert (mechanism #3) needs to be set explicitly.
  GARDENER=$(mk_bead "TEST-DELETE-ME autoclose inert-gardener $TAG" "trigger" \
    -f 'action="assert"')
  if [ -z "$EPIC5" ] || [ -z "$C5" ] || [ -z "$GARDENER" ]; then
    fail "scenario-5-setup" "could not create epic + child + gardener fixtures"
  else
    dim "  Epic: $EPIC5  child: $C5  gardener: $GARDENER"
    link "$C5" "$EPIC5" "parent-child"
    link "$GARDENER" "$EPIC5" "parent-child"

    # Drain the epic's only issue-kind child. Gate A is now satisfied, so the
    # ONLY thing holding the epic open is the live gardener.
    kd_do "close gardener-epic child" "$KD_BIN" close "$C5"
    assert_stays "epic drain DEFERS while a live gardener child remains" "$EPIC5" "closed"

    # Flip the gardener to explicitly disabled — the exact liveness read the
    # trigger-engine uses (enabled=="false" is inert; absent/unset is LIVE) — and
    # close it. That close re-enters the cascade with no live gardener left, so
    # the epic must now drain. This proves scenario 5's negative was the
    # gardener guard holding, not the epic being undrainable for some other
    # reason (which is what a plain "it stayed open" assertion would allow).
    kd_do "disable gardener fixture" "$KD_BIN" update "$GARDENER" -f 'enabled=false'
    kd_do "close gardener fixture" "$KD_BIN" close "$GARDENER"
    local epic5_status
    epic5_status=$(await_status "$EPIC5" "closed" || true)
    assert_equals "epic drains once the gardener child is disabled and closed" \
      "closed" "$epic5_status"
    assert_equals "gardener-gated epic close attributed to the daemon cascade" \
      "system:auto-close" "$(bead_closed_by "$EPIC5")"
  fi
  echo

  # ── Scenario 6: closing a parent parks its still-open orphaned children ──
  # Bug ka-0000000504: closing a bead that still has OPEN parent-child children
  # left them status=open under a dead parent, claimable in `gb ready` forever —
  # the auto-close cascade only DRAINS an already-empty parent and never inspects
  # the children of the bead being closed. maybeParkOrphanedChildren now parks
  # each still-open child to `blocked` on ANY parent close, of ANY parent type.
  # This landed after the module was first written, so it had no e2e coverage;
  # scenario 4's negative control relies on its `hasOtherLiveParent` exemption,
  # so the exemption is exercised here as a first-class assertion, not a side
  # effect.
  blue "Scenario 6: closing a parent parks its still-open orphaned children"
  local PARENT6 ORPHAN LIVE_PARENT KEPT
  PARENT6=$(mk_bead "TEST-DELETE-ME autoclose orphan-parent $TAG" "task")
  ORPHAN=$(mk_bead "TEST-DELETE-ME autoclose orphan-child $TAG" "task")
  # KEPT has a SECOND live parent, so closing PARENT6 must leave it alone.
  LIVE_PARENT=$(mk_bead "TEST-DELETE-ME autoclose orphan-live-parent $TAG" "epic")
  KEPT=$(mk_bead "TEST-DELETE-ME autoclose orphan-kept $TAG" "task")
  if [ -z "$PARENT6" ] || [ -z "$ORPHAN" ] || [ -z "$LIVE_PARENT" ] || [ -z "$KEPT" ]; then
    fail "scenario-6-setup" "could not create parent + orphan + live-parent + kept"
  else
    dim "  Parent: $PARENT6  orphan: $ORPHAN  kept: $KEPT (live parent $LIVE_PARENT)"
    link "$ORPHAN" "$PARENT6" "parent-child"
    link "$KEPT" "$PARENT6" "parent-child"
    link "$KEPT" "$LIVE_PARENT" "parent-child"

    # Closing PARENT6 orphans both children; only ORPHAN is truly orphaned.
    kd_do "close orphan parent" "$KD_BIN" close "$PARENT6"

    local orphan_status
    orphan_status=$(await_status "$ORPHAN" "blocked" || true)
    assert_equals "orphaned child parked to blocked when its only parent closed" \
      "blocked" "$orphan_status"
    # A park is NOT a close — the child stays intact and reopenable.
    assert_equals "orphan-parked child was NOT closed (park != close)" \
      "" "$(bead_closed_by "$ORPHAN")"
    # NEGATIVE CONTROL: a child with another still-live parent is not orphaned,
    # so it must stay put — the hasOtherLiveParent exemption scenario 4 leans on.
    assert_stays "child with another live parent is NOT parked" "$KEPT" "blocked"
  fi
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
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
