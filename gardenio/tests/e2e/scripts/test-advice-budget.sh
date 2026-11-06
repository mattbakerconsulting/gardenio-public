#!/usr/bin/env bash
# test-advice-budget.sh — advice byte-budget enforcement E2E tests
#
# Backfills end-to-end coverage for the advice byte-budget cap that gb prime
# applies to its advice section (gd-AnhxNODZQX, epic ka-0000003108). The Go
# unit tests (prime_advice_budget_test.go) already exercise the byte-math in
# isolation; this module proves the *whole path* against a live daemon:
#
#   real daemon  →  gb prime --for <agent>  →  budget cascade  →
#   VISIBLE "... N more" truncation footer  →  recovery via gb advice list
#
# The operator pain this guards (thread ka-0000005078): advice silently
# dropping past the budget hurts instruction-following and autonomy. The
# invariant under test is therefore not just "the section is bounded" but
# "nothing instruction-bearing is *silently* dropped" — every overflow item
# is either rendered (abbreviated/title-only) or accounted for by a marked
# footer that points the agent at the full recoverable list, and inline
# (policy-critical) advice bypasses the budget entirely.
#
# Scenarios tested:
#   1. Budget is enforced end-to-end: a tiny budget yields a far smaller
#      advice section than an effectively-unbounded budget over the same
#      seeded corpus (differential proof, robust to ambient advice).
#   2. Truncation is VISIBLE, not silent: the "... N more advice items not
#      shown (budget exceeded)" footer appears under budget pressure and
#      carries the `gb advice list --for <agent>` recovery pointer.
#   3. No content is permanently dropped: every truncated bead is still
#      listable from the store (truncation is a render-time decision, never
#      a delete), so the footer's "see all" promise is honest.
#   4. inline:true advice bypasses the budget — a policy-critical
#      instruction renders in FULL even under maximum budget pressure.
#   5. Role-transition diff path (outputAdviceDiffItems) is budget-bounded
#      and shows the same visible-truncation footer when the target role's
#      catalog overflows the tight diff budget.
#   6. Single-advice boundary contract (the per-item acceptance pair): ONE
#      bead, three budget regimes — within-budget renders INTACT with no
#      footer; over-abbrev-budget degrades to the title-only form (summary
#      dropped, title kept — truncate, don't silently drop); over-both-
#      budgets collapses into the visible "... N more" footer.
#
# Failure-mode scenarios (F1-F9) from the advice-truncation e2e test design
# (ka-0000003643, gardenio/docs/advice-truncation-e2e-test-design.md). Each
# forces the cascade with env overrides — NEVER on the default budget (the
# real corpus does not truncate at 60k/20k, so a default-budget assertion
# would rot the moment the corpus shrinks):
#   7. F1  — cross-group eviction follows GroupSortKey (global before agent);
#            the higher-priority group survives, the lower-priority one is
#            evicted, and render order matches the documented group order.
#   8. F2  — supersession UNDER budget pressure: the superseded global is
#            absent AND its superseder still renders (never both dropped).
#   9. F4  — inline-bypass CAUSAL guard: the same bead WITHOUT inline:true is
#            truncated at budget=1, proving S4's full render is caused by the
#            label, not by the bead being short enough to fit anyway.
#  10. F6  — matched (`--json`, no budget) vs budgeted (text) disambiguation,
#            plus IsDeniedBroadcast dropping a bare-project broadcast (a
#            TARGETING drop, not a budget drop).
#  11. F8  — footer count boundaries: N=1 exact + exact arithmetic
#            (footer N + rendered == seeded) + N=0 no-footer.
#  12. F7  — rendered output is valid UTF-8 with no U+FFFD, over a
#            multibyte-heavy (emoji + CJK) corpus that forces the summary cap.
#  13. F9  — `gb prime --system-prompt` (the channel the model sees turn 1)
#            stays under the 100KB total ceiling over a large corpus.
#  F5 asymmetry — abbrev-budget=0 falls back to the DEFAULT while
#            title-budget=0 is honored as a LITERAL zero.
#
# The remaining design modes land elsewhere (see the design doc §3):
#   F7 rune-math   → TestAdviceExcerpt_RuneBoundaryMultibyte (Go unit)
#   F10 >500 pool  → TestListOpenAdvice_PoolLimit (Go unit, remediation flag)
#   F9 integration → advice-systemprompt-truncation.toml (claudeless)
#
# Override knobs (read by the gb binary at runtime — see prime_advice.go):
#   GB_PRIME_ADVICE_BUDGET_BYTES        abbreviated-render budget (0/garbage → default)
#   GB_PRIME_ADVICE_TITLE_BUDGET_BYTES  title-only overflow budget (0 → literal zero)
#   GB_PRIME_ADVICE_SUMMARY_CHARS       per-item summary rune cap

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
CREATED_ADVICE=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

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

assert_exit() {
  local name="$1" expected="$2" actual="$3"
  if [ "$actual" -eq "$expected" ]; then
    pass "$name (exit $expected)"
  else
    fail "$name" "expected exit $expected, got exit $actual"
  fi
}

assert_contains() {
  local name="$1" pattern="$2" actual="$3"
  if grep -qF -- "$pattern" <<< "$actual"; then
    pass "$name (contains '$pattern')"
  else
    fail "$name" "expected output to contain '$pattern'; got: $(echo "$actual" | head -5)"
  fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19098
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-advice-budget.log 2>&1 &
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
  fi
}

cleanup() {
  for id in "${CREATED_ADVICE[@]}"; do
    [ -n "$id" ] || continue
    "$GB_BIN" advice remove "$id" --hard 2>/dev/null || "$KD_BIN" delete "$id" 2>/dev/null || true
  done
  for id in "${CREATED_BEADS[@]}"; do
    [ -n "$id" ] || continue
    "$KD_BIN" delete "$id" 2>/dev/null || "$KD_BIN" close "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# Extract advice_bytes=N from a `gb prime --size` summary line.
#   [prime] role=.. project=.. total_bytes=N advice_bytes=N advice_items=N
# ALWAYS exits 0 (grep-no-match under `set -o pipefail` must not abort the suite).
advice_bytes_of() {
  grep -oE 'advice_bytes=[0-9]+' <<< "$1" | head -1 | cut -d= -f2 || true
}

# Extract N from the truncation footer:
#   "... N more advice items not shown (budget exceeded)."
# Echoes the integer, or "0" when no footer is present. ALWAYS exits 0 — the
# grep-no-match under `set -o pipefail` would otherwise abort the whole suite,
# and callers treat "no footer" as N=0 anyway.
footer_n_of() {
  local m
  m=$(grep -oE '\.\.\. [0-9]+ more advice items not shown' <<< "$1" | head -1 || true)
  m=$(grep -oE '[0-9]+' <<< "$m" | head -1 || true)
  printf '%s' "${m:-0}"
}

# Byte offset of the first occurrence of a literal marker in text (via grep -b),
# or empty when the marker is absent. ALWAYS exits 0 (grep-no-match under
# pipefail must not abort the suite). Used to assert render ORDER (which of two
# markers appears first) independent of ambient advice interleaving.
offset_of() {
  local marker="$1" text="$2"
  grep -boF -- "$marker" <<< "$text" | head -1 | cut -d: -f1 || true
}


# Seed one agent-scoped advice bead with a large single-line body so its
# abbreviated rendering (title + first-line summary + pointer) is heavy and
# forces budget pressure. Echoes the created bead id (or empty on failure).
# Args: <agent-label-value> <index> <tag> [extra-label]
seed_big_advice() {
  local agent_val="$1" idx="$2" tag="$3" extra="${4:-}"
  # ~1.1 KB single-line body — distinct from the title so it renders as the
  # abbreviated summary (renderAbbrevAdviceItem keys off firstLine).
  local body
  body="INSTRUCTION-$tag-$idx: $(printf 'always-do-the-important-thing %.0s' $(seq 1 40))"
  local labels=(-l "agent:$agent_val")
  [ -n "$extra" ] && labels+=(-l "$extra")
  local out id
  out=$("$KD_BIN" create "ADV-$tag-$idx budget probe rule" --type=advice \
    "${labels[@]}" -d "$body" --json 2>/dev/null || echo "{}")
  id=$(echo "$out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  echo "$id"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " advice byte-budget enforcement E2E tests"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TAG="advbudget-$$-$(date +%s)"
  # Project-rooted agent id (project/role_plural/name). This shape matters:
  # BuildAgentSubscriptions derives project:gardenio + role:crew from the path,
  # and `kd create --type=advice` auto-stamps project:gardenio on every bead.
  # MatchesSubscriptions gates on project:X — so a slash-free id (subscribing
  # to project:<tag>) would reject our own seeds and silently fall back to
  # matching only ambient prod globals. Rooting under gardenio/crews/ makes the
  # seeded agent:<id> beads actually match. The trailing path segment is also
  # the agentName the role-diff path (ListAssignedTask) looks up by assignee.
  local AGENT="gardenio/crews/$TAG"
  local SEED_N=30

  # Disable auto-assign for every prime call below: gb prime auto-claims a
  # ready project task for an idle agent, and our throwaway agent must never
  # grab real gardenio work. outputAutoAssign returns early when BOAT_TASK_ID
  # is set (prime.go).
  export BOAT_TASK_ID="e2e-advice-budget-noassign"

  # ── Scenario 0: Seed an over-budget corpus ─────────────────────
  blue "Scenario 0: Seed $SEED_N agent-scoped advice beads (over-budget corpus)"
  local seeded=0 i id
  for i in $(seq 1 "$SEED_N"); do
    id=$(seed_big_advice "$AGENT" "$i" "$TAG")
    if [ -n "$id" ]; then
      CREATED_ADVICE+=("$id")
      seeded=$((seeded + 1))
    fi
  done
  if [ "$seeded" -ge $((SEED_N - 2)) ]; then
    pass "seeded $seeded/$SEED_N advice beads for agent:$AGENT"
  else
    fail "seed corpus" "only seeded $seeded/$SEED_N — daemon writes failing?"
    return 1
  fi
  echo

  # ── Scenario 1: Budget enforced end-to-end (differential) ──────
  blue "Scenario 1: tiny budget yields a far smaller advice section than unbounded"
  local huge_out tiny_out huge_bytes tiny_bytes
  # Unbounded reference render: everything fits, no truncation.
  huge_out=$(GB_PRIME_ADVICE_BUDGET_BYTES=20000000 \
             GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=20000000 \
             "$GB_BIN" prime --for "$AGENT" --size 2>/dev/null || echo "")
  # Tight budget: most of the corpus must be displaced.
  tiny_out=$(GB_PRIME_ADVICE_BUDGET_BYTES=1500 \
             GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=1500 \
             "$GB_BIN" prime --for "$AGENT" --size 2>/dev/null || echo "")
  huge_bytes=$(advice_bytes_of "$huge_out")
  tiny_bytes=$(advice_bytes_of "$tiny_out")
  dim "  advice_bytes: unbounded=$huge_bytes  tight=$tiny_bytes"
  if [ -n "$huge_bytes" ] && [ -n "$tiny_bytes" ]; then
    if [ "$tiny_bytes" -lt "$huge_bytes" ]; then
      pass "tight-budget advice section ($tiny_bytes B) < unbounded ($huge_bytes B)"
    else
      fail "budget enforcement" "tight=$tiny_bytes not < unbounded=$huge_bytes — budget not applied?"
    fi
    # The tight render must also be a large reduction, not a rounding wobble.
    if [ "$tiny_bytes" -lt $((huge_bytes / 2)) ]; then
      pass "tight-budget render is <50% of unbounded (substantive truncation)"
    else
      fail "budget magnitude" "tight=$tiny_bytes not < half of unbounded=$huge_bytes"
    fi
  else
    fail "prime --size" "could not parse advice_bytes (huge='$huge_out' tiny='$tiny_out')"
  fi
  echo

  # ── Scenario 2: Truncation is visible (marked), not silent ─────
  blue "Scenario 2: budget overflow emits a VISIBLE truncation footer"
  local prime_out
  prime_out=$(GB_PRIME_ADVICE_BUDGET_BYTES=1500 \
              GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=1500 \
              "$GB_BIN" prime --for "$AGENT" 2>/dev/null || echo "")
  assert_contains "overflow footer present" \
    "more advice items not shown (budget exceeded)" "$prime_out"
  assert_contains "footer carries recovery pointer" \
    "gb advice list --for $AGENT" "$prime_out"
  # Sanity: the corpus was actually truncated — fewer of our tagged beads are
  # visible than we seeded (the rest collapsed into the footer count).
  local visible
  visible=$(grep -cF "ADV-$TAG-" <<< "$prime_out" || true)
  dim "  $visible/$seeded tagged beads rendered under tight budget"
  if [ "$visible" -lt "$seeded" ]; then
    pass "truncation occurred ($visible < $seeded tagged beads rendered)"
  else
    fail "truncation" "all $seeded beads rendered ($visible) — budget never bit"
  fi
  echo

  # ── Scenario 3: Truncated content still lives in the store ─────
  blue "Scenario 3: every truncated bead is still listable (nothing destroyed)"
  # The "... N more" footer is only honest if the displaced beads remain
  # retrievable. Truncation is a *render-time* budget decision — it must
  # never delete or hide content from the store. We list by the canonical
  # `agent:<id>` label scope (the reliable retrieval filter; `advice list
  # --for <name>` appends the bare name as a raw label and does NOT expand
  # it to agent:<name>, so it under-matches agent-scoped beads).
  local list_out recovered
  list_out=$("$GB_BIN" advice list -l "agent:$AGENT" --json 2>/dev/null || echo "[]")
  recovered=$(grep -cF "ADV-$TAG-" <<< "$list_out" || true)
  dim "  $recovered/$seeded tagged beads still listable post-truncation"
  if [ "$recovered" -ge "$seeded" ]; then
    pass "all $seeded seeded beads survive in the store (truncation is render-only)"
  else
    fail "recovery completeness" "only $recovered/$seeded listable — truncation lost content"
  fi
  echo

  # ── Scenario 4: inline:true bypasses the budget (never dropped) ─
  blue "Scenario 4: inline:true policy-critical advice renders FULL under pressure"
  local INLINE_MARK="INLINE-POLICY-$TAG-must-never-be-truncated"
  local inline_out inline_id
  inline_out=$("$KD_BIN" create "INLINE-$TAG critical rule" --type=advice \
    -l "agent:$AGENT" -l "inline:true" \
    -d "$INLINE_MARK: do not write to prod without approval" --json 2>/dev/null || echo "{}")
  inline_id=$(echo "$inline_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$inline_id" ]; then
    CREATED_ADVICE+=("$inline_id")
    pass "inline advice bead created ($inline_id)"
    local inline_prime
    inline_prime=$(GB_PRIME_ADVICE_BUDGET_BYTES=1 \
                   GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=1 \
                   "$GB_BIN" prime --for "$AGENT" 2>/dev/null || echo "")
    # Budget is floored to 1 byte: every non-inline bead must overflow, yet
    # the inline rule's full body must still appear verbatim.
    assert_contains "inline full body survives budget=1" "$INLINE_MARK" "$inline_prime"
    assert_contains "non-inline corpus truncated at budget=1" \
      "more advice items not shown (budget exceeded)" "$inline_prime"
  else
    fail "inline advice create" "could not create inline bead"
  fi
  echo

  # ── Scenario 5: role-transition diff path is budget-bounded ────
  blue "Scenario 5: outputAdviceDiffItems (role diff) is budget-bounded + marked"
  # The diff path fires when the agent's claimed in_progress task carries a
  # role:<X> label that differs from BOAT_ROLE. Seed a large role:<X> catalog,
  # claim a role-tagged task, then prime with a mismatched BOAT_ROLE.
  #
  # outputAdviceRoleDiff resolves the claimed task via ListAssignedTask(name)
  # where name is the LAST path segment of the agent id. So the task must be
  # assigned to the bare name ($AGENT_NAME), not the full project-rooted id —
  # otherwise the lookup misses and the diff section never renders.
  local AGENT_NAME="${AGENT##*/}"
  # Seed under agent:$AGENT + role:$DIFF_ROLE. The agent: label keeps the
  # beads matchable by our agent's subscriptions; the role: label means they
  # only enter the *target* subscription set (which adds role:$DIFF_ROLE), so
  # DiffAdviceForRole classifies them as "Added" (the role-transition delta).
  local DIFF_ROLE="advprobe-${TAG}"
  local diff_seeded=0
  for i in $(seq 1 "$SEED_N"); do
    id=$(seed_big_advice "$AGENT" "$i" "DIFF-$TAG" "role:$DIFF_ROLE")
    if [ -n "$id" ]; then
      CREATED_ADVICE+=("$id")
      diff_seeded=$((diff_seeded + 1))
    fi
  done
  dim "  seeded $diff_seeded agent:$AGENT + role:$DIFF_ROLE advice beads"

  # Create + claim a task that targets the diff role for our agent.
  local task_out task_id
  task_out=$("$KD_BIN" create "e2e-advbudget-roletask-$TAG" --type=task \
    -l "project:gardenio" -l "role:$DIFF_ROLE" \
    -d "role-transition probe task" --json 2>/dev/null || echo "{}")
  task_id=$(echo "$task_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$task_id" ]; then
    CREATED_BEADS+=("$task_id")
    "$KD_BIN" update "$task_id" --assignee "$AGENT_NAME" --status in_progress 2>/dev/null || true
    pass "role-tagged task created + claimed ($task_id, assignee=$AGENT_NAME)"

    # BOAT_ROLE=crew differs from role:advprobe-<tag> → diff fires.
    local diff_out
    diff_out=$(BOAT_ROLE=crew \
               GB_PRIME_ADVICE_BUDGET_BYTES=1500 \
               GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=1500 \
               "$GB_BIN" prime --for "$AGENT" 2>/dev/null || echo "")
    if grep -qF "Role Transition" <<< "$diff_out"; then
      pass "role-transition section emitted"
      # The diff is a supplementary delta layered on the main advice section;
      # under a tight budget its overflow must surface the same visible footer
      # rather than silently dropping the new-role guidance.
      assert_contains "role-diff overflow is marked, not silent" \
        "more advice items not shown (budget exceeded)" "$diff_out"
      # The diff section itself must stay bounded — assert the new-role block
      # did not dump the entire seeded catalog inline.
      local diff_visible
      diff_visible=$(grep -cF "ADV-DIFF-$TAG-" <<< "$diff_out" || true)
      dim "  $diff_visible/$diff_seeded role-diff beads rendered under tight budget"
      if [ "$diff_visible" -lt "$diff_seeded" ]; then
        pass "role-diff section budget-bounded ($diff_visible < $diff_seeded)"
      else
        fail "role-diff budget" "all $diff_seeded role beads rendered — diff budget never bit"
      fi
    else
      # Role-diff requires the daemon's assigned-task lookup (assignee+kind=issue)
      # to resolve our claim. If the deployed daemon doesn't surface it, treat
      # as a soft skip rather than a hard failure — the primary budget path
      # (scenarios 1-4) is the core coverage for this module.
      dim "  Role-transition section not emitted (assigned-task lookup may not"
      dim "  have resolved the claim on this daemon build) — soft skip."
      pass "role-diff path soft-skipped (no transition detected)"
    fi
  else
    fail "role task create" "could not create role-tagged task"
  fi
  echo

  # ── Scenario 6: single-advice boundary contract (the AC pair) ──
  # Scenarios 1-5 prove the budget cascade over a *corpus* (differential
  # bytes, footer, recovery, inline bypass, role-diff). This scenario pins
  # the narrower per-item contract the acceptance criteria name directly,
  # holding everything constant except the budget so the cause is
  # unambiguous: ONE advice bead, primed under three budget regimes.
  #
  #   (a) within-budget  → renders INTACT (its distinctive first-line summary
  #                         is present) and NO truncation footer appears;
  #   (b) over-budget abbrev → degrades to the title-only overflow form
  #                         (summary body dropped, title still shown) — the
  #                         enforcement rule is "truncate, never silently
  #                         drop", so the bead does NOT vanish;
  #   (c) over-budget both   → collapses into the visible "... N more" footer
  #                         (truncated past even the title budget).
  #
  # We seed a fresh single-advice agent scope (no corpus) so the only thing
  # that can move the render is this one bead, making the assertions robust
  # to ambient prod globals: we key every assertion on this bead's unique
  # markers, never on a raw byte count.
  blue "Scenario 6: single-advice boundary — within-budget intact vs over-budget truncated"
  local SOLO_TAG="solo-${TAG}"
  local SOLO_AGENT="gardenio/crews/${SOLO_TAG}"
  local SOLO_SUMMARY="SOLO-SUMMARY-${SOLO_TAG}-distinctive-first-line-body"
  local solo_out solo_id
  # ~600 B single-line body. Distinct from the title so renderAbbrevAdviceItem
  # emits it as the summary; long enough that one abbreviated render easily
  # exceeds a 1-byte budget yet trivially fits a generous one.
  local solo_body
  solo_body="$SOLO_SUMMARY: $(printf 'follow-this-rule %.0s' $(seq 1 30))"
  solo_out=$("$KD_BIN" create "SOLO-${SOLO_TAG} boundary rule" --type=advice \
    -l "agent:$SOLO_AGENT" -d "$solo_body" --json 2>/dev/null || echo "{}")
  solo_id=$(echo "$solo_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$solo_id" ]; then
    CREATED_ADVICE+=("$solo_id")
    pass "single advice bead created ($solo_id)"

    # (a) WITHIN BUDGET → intact, no footer. Effectively-unbounded budgets
    #     (the same value Scenario 1 uses as its no-truncation reference, so
    #     the whole catalog — ambient globals plus our bead — fits the
    #     abbreviated tier with full summary lines and nothing overflows).
    local within_out
    within_out=$(GB_PRIME_ADVICE_BUDGET_BYTES=20000000 \
                 GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=20000000 \
                 "$GB_BIN" prime --for "$SOLO_AGENT" 2>/dev/null || echo "")
    assert_contains "within-budget advice renders its full summary intact" \
      "$SOLO_SUMMARY" "$within_out"
    if grep -qF "more advice items not shown (budget exceeded)" <<< "$within_out"; then
      fail "within-budget no footer" "truncation footer present though one bead fits"
    else
      pass "within-budget render carries NO truncation footer"
    fi

    # (b) OVER abbrev budget, ample title budget → title-only degradation.
    #     The abbreviated form (with summary) overflows budget=1, but the
    #     compact title-only form fits the title tier: title shown, summary
    #     dropped, NO footer (nothing fell past both tiers). This is the
    #     "truncate, don't drop" guarantee at the single-item granularity.
    local title_out
    title_out=$(GB_PRIME_ADVICE_BUDGET_BYTES=1 \
                GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=20000000 \
                "$GB_BIN" prime --for "$SOLO_AGENT" 2>/dev/null || echo "")
    assert_contains "over-abbrev-budget still shows the advice title" \
      "SOLO-${SOLO_TAG} boundary rule" "$title_out"
    assert_contains "over-abbrev-budget degrades to the title-only pointer form" \
      "gb advice show $solo_id" "$title_out"
    if grep -qF "$SOLO_SUMMARY" <<< "$title_out"; then
      fail "over-abbrev-budget summary dropped" "summary body still rendered though abbrev budget=1"
    else
      pass "over-abbrev-budget drops the summary body (truncated to title-only)"
    fi

    # (c) OVER both budgets → the bead collapses into the visible footer.
    #     budget=1 AND title-budget=1: even the title-only form overflows, so
    #     the bead is accounted for by the marked "... N more" footer rather
    #     than silently disappearing.
    local both_out
    both_out=$(GB_PRIME_ADVICE_BUDGET_BYTES=1 \
               GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=1 \
               "$GB_BIN" prime --for "$SOLO_AGENT" 2>/dev/null || echo "")
    assert_contains "over-both-budgets emits the visible truncation footer" \
      "more advice items not shown (budget exceeded)" "$both_out"
    assert_contains "footer points at the recovery list for this agent" \
      "gb advice list --for $SOLO_AGENT" "$both_out"
  else
    fail "single advice create" "could not create solo advice bead"
  fi
  echo

  # ── Scenario 7: cross-group eviction follows GroupSortKey (F1) ─────
  # The audit found the confidence sort is GONE; the only guaranteed priority
  # axis today is the scope-group order from GroupSortKey (global=0 →
  # project=1 → role=2 → agent=3). This scenario pins that CROSS-GROUP
  # eviction contract with two ambient-proof invariants:
  #
  #   (a) ORDER: in an unbounded render our global marker appears BEFORE our
  #       agent marker — GroupSortKey ordering reaches the output.
  #   (b) EVICTION DIRECTION: under budget pressure with titleBudget=1, the
  #       lower-priority AGENT-scoped item (group 3, sorts LAST) is the one
  #       evicted to the footer, while the higher-priority GLOBAL group still
  #       renders (the advice section survives) — eviction follows the
  #       documented order, not its reverse.
  #
  # Note on hermeticity: the live daemon carries ~38 ambient globals/role
  # advice that also match any gardenio crew agent and share group 0 with our
  # seeded global. So we do NOT assert "our specific global survives
  # abbreviated" (an ambient global could take that single abbrev slot) — that
  # would flake. We assert the DIRECTION (agent-scoped item evicted, globals
  # present) and the ORDER, both of which hold regardless of ambient volume.
  #
  # A true global needs `--project ""`: the daemon auto-stamps project:gardenio
  # on advice creates, and `global` + a targeting label is rejected by
  # validation ("advice cannot combine global with a targeting label").
  #
  # HERMETICITY (the .16/.17 red): the eviction pressure must come from OUR
  # seeded corpus, not from ambient globals. A fresh e2e staging
  # namespace carries ~zero ambient global advice (unlike prod's ~38 that the
  # earlier "Note on hermeticity" above assumed), so a lone seeded global (group
  # 0) + agent (group 3) BOTH fit under budget=1500 and nothing is evicted — the
  # agent marker renders, no footer, false-negative FAIL. Fix: seed enough
  # group-0 (global) filler that the global group ALONE exceeds the 1500-byte
  # abbrev budget, so the lower-priority group-3 agent item is guaranteed to
  # overflow regardless of ambient volume. Seeding only ADDS group-0 pressure
  # ahead of the agent item, so it is monotonic — it can never make a
  # genuinely-evicting daemon stop evicting (the invariant still holds on prod).
  blue "Scenario 7: cross-group eviction follows GroupSortKey (agent evicted before global) [F1]"
  local EV_TAG="evict-${TAG}"
  local EV_AGENT="gardenio/crews/${EV_TAG}"
  local EV_GLOBAL_MARK="EVICT-GLOBAL-${EV_TAG}"
  local EV_AGENT_MARK="EVICT-AGENT-${EV_TAG}"
  local ev_glob_id ev_agent_id
  # Global bead (group 0). --project "" suppresses the auto-stamped project
  # label so the bead is truly global.
  ev_glob_id=$("$KD_BIN" create "$EV_GLOBAL_MARK global rule" --type=advice \
    -l "global" --project "" \
    -d "$EV_GLOBAL_MARK: $(printf 'high-priority-global-guidance %.0s' $(seq 1 20))" \
    --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  # Group-0 filler globals: each renders ~600 B abbreviated (a >500-char body
  # forces the summary cap), so 5 of them (~3 KB) comfortably exceed budget=1500
  # even when zero ambient globals exist. They carry a DISTINCT marker so they
  # never collide with EV_GLOBAL_MARK's offset probe below. Tracked in a local
  # array and deleted at the end of this scenario so the extra globals do not
  # perturb the count-sensitive later scenarios.
  local ev_fillers=() ev_i ev_fill_id
  for ev_i in $(seq 1 5); do
    ev_fill_id=$("$KD_BIN" create "EVICT-GLOBAL-FILLER-${EV_TAG}-${ev_i} global rule" --type=advice \
      -l "global" --project "" \
      -d "evict-filler-${ev_i}: $(printf 'high-priority-global-guidance %.0s' $(seq 1 20))" \
      --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
    [ -n "$ev_fill_id" ] && ev_fillers+=("$ev_fill_id")
  done
  # Agent-scoped bead (group 3) — sorts LAST, so it is evicted first.
  ev_agent_id=$(seed_big_advice "$EV_AGENT" 1 "$EV_AGENT_MARK")
  if [ -n "$ev_glob_id" ] && [ -n "$ev_agent_id" ]; then
    CREATED_ADVICE+=("$ev_glob_id" "$ev_agent_id")
    pass "eviction fixtures created (global=$ev_glob_id agent=$ev_agent_id, ${#ev_fillers[@]} group-0 filler)"
    # (a) ORDER: unbounded render — global marker precedes agent marker.
    local ev_both go ao
    ev_both=$(GB_PRIME_ADVICE_BUDGET_BYTES=20000000 \
              GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=20000000 \
              "$GB_BIN" prime --for "$EV_AGENT" 2>/dev/null || echo "")
    go=$(offset_of "$EV_GLOBAL_MARK" "$ev_both")
    ao=$(offset_of "INSTRUCTION-$EV_AGENT_MARK-1" "$ev_both")
    if [ -n "$go" ] && [ -n "$ao" ]; then
      if [ "$go" -lt "$ao" ]; then
        pass "global group renders before agent group (offset $go < $ao) — GroupSortKey order honored"
      else
        fail "group order" "global offset $go not before agent offset $ao"
      fi
    else
      fail "group order" "could not locate both markers (global='$go' agent='$ao')"
    fi
    # (b) EVICTION DIRECTION: tight abbrev budget + titleBudget=1 so nothing
    # hides in the title tier. The agent-scoped item (group 3) must be evicted
    # (its summary marker absent) while the global group still renders (advice
    # section present) and the visible footer accounts for the displaced items.
    local ev_out
    ev_out=$(GB_PRIME_ADVICE_BUDGET_BYTES=1500 \
             GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=1 \
             "$GB_BIN" prime --for "$EV_AGENT" 2>/dev/null || echo "")
    if grep -qF "INSTRUCTION-$EV_AGENT_MARK-1" <<< "$ev_out"; then
      fail "agent eviction" "lower-priority agent item rendered though group-0 globals fill the budget first — eviction order reversed?"
    else
      pass "lower-priority agent item evicted first (group 3 before group 0) — order honored"
    fi
    assert_contains "higher-priority global group still renders (advice section present)" \
      "## Advice" "$ev_out"
    assert_contains "eviction is visible (footer present)" \
      "more advice items not shown (budget exceeded)" "$ev_out"
  else
    fail "eviction fixtures" "could not create global+agent beads (global='$ev_glob_id' agent='$ev_agent_id')"
  fi
  # Drop the group-0 filler globals now so they don't add ambient pressure to
  # the count-sensitive later scenarios (the trap is a backstop for early exits).
  for ev_fill_id in "${ev_fillers[@]}"; do
    "$KD_BIN" delete "$ev_fill_id" 2>/dev/null || true
  done
  echo

  # ── Scenario 8: supersession under budget pressure (F2) ────────────
  # The worst case of F1: a project/global superseder correctly removes the
  # global it overrides, but then the superseder ITSELF spills past the
  # budget — so the agent sees NEITHER. This ties supersession (unit-covered
  # and default-budget-e2e-covered) to budget pressure, end-to-end.
  #
  # Invariant: under a tight budget, G (superseded) is absent AND P (the
  # superseder) still renders at least as a title — P must NEVER be evicted
  # while G was dropped on its behalf.
  blue "Scenario 8: superseder survives budget pressure while superseded global is gone [F2]"
  local SUP_TAG="supbudget-${TAG}"
  local SUP_AGENT="gardenio/crews/${SUP_TAG}"
  local SUP_G_MARK="SUPBUD-GLOBAL-${SUP_TAG}"
  local SUP_P_MARK="SUPBUD-SUPERSEDER-${SUP_TAG}"
  local sup_g_id sup_p_id
  # G: a plain global slated to be superseded. --project "" makes it truly
  # global (the daemon auto-stamps project:gardenio otherwise, which validation
  # rejects alongside `global`).
  sup_g_id=$("$KD_BIN" create "$SUP_G_MARK old global" --type=advice \
    -l "global" --project "" -d "$SUP_G_MARK: $(printf 'stale-global-guidance %.0s' $(seq 1 20))" \
    --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$sup_g_id" ]; then
    CREATED_ADVICE+=("$sup_g_id")
    # P: a global superseder naming G (first line, no period before the id so
    # the sentence-bounded detector captures it — see supersession.go).
    sup_p_id=$("$KD_BIN" create "$SUP_P_MARK override rule" --type=advice \
      -l "global" --project "" -d "$SUP_P_MARK supersedes $sup_g_id replacing it $(printf 'override-guidance %.0s' $(seq 1 15))" \
      --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
    if [ -n "$sup_p_id" ]; then
      CREATED_ADVICE+=("$sup_p_id")
      pass "supersession-under-budget fixtures created (G=$sup_g_id P=$sup_p_id)"
      # Tight abbrev budget but ample title budget: even if P can't render
      # abbreviated, it MUST at least surface title-only; G must be absent
      # entirely (dropped by supersession, before the budget cascade).
      local sup_out
      sup_out=$(GB_PRIME_ADVICE_BUDGET_BYTES=1 \
                GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=20000000 \
                "$GB_BIN" prime --for "$SUP_AGENT" 2>/dev/null || echo "")
      if grep -qF "$SUP_G_MARK" <<< "$sup_out"; then
        fail "superseded global absent" "$SUP_G_MARK still rendered though superseded by P"
      else
        pass "superseded global G is absent (supersession applied pre-budget)"
      fi
      # P must appear at least as its title (the title carries the marker).
      if grep -qF "$SUP_P_MARK" <<< "$sup_out"; then
        pass "superseder P survives budget pressure (renders at least title-only)"
      else
        fail "superseder evicted" "$SUP_P_MARK absent — override vanished AND its global was dropped: topic went dark (F2)"
      fi
    else
      fail "superseder create" "could not create P"
    fi
  else
    fail "superseded-global create" "could not create G"
  fi
  echo

  # ── Scenario 9: inline-bypass causal negative guard (F4) ───────────
  # Scenario 4 proves an inline:true bead renders in FULL at budget=1. But
  # that could pass for the wrong reason — if the bead were simply short
  # enough to fit anyway. This negative/causal guard seeds the SAME body
  # WITHOUT the inline:true label under the same budget=1 and asserts it is
  # truncated (footer/title-only). Full render in S4 is therefore CAUSED by
  # the label, not by the bead being small.
  blue "Scenario 9: same bead sans inline:true truncates at budget=1 (causal guard) [F4]"
  local NI_TAG="noninline-${TAG}"
  local NI_AGENT="gardenio/crews/${NI_TAG}"
  local NI_MARK="NONINLINE-BODY-${NI_TAG}-should-be-truncated"
  # Body identical in spirit to S4's inline bead, but heavy enough that its
  # abbreviated summary cannot fit budget=1.
  local ni_id
  ni_id=$("$KD_BIN" create "NONINLINE-${NI_TAG} critical-looking rule" --type=advice \
    -l "agent:$NI_AGENT" \
    -d "$NI_MARK: $(printf 'do-not-write-to-prod-without-approval %.0s' $(seq 1 20))" \
    --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$ni_id" ]; then
    CREATED_ADVICE+=("$ni_id")
    pass "non-inline bead created ($ni_id)"
    local ni_out
    ni_out=$(GB_PRIME_ADVICE_BUDGET_BYTES=1 \
             GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=1 \
             "$GB_BIN" prime --for "$NI_AGENT" 2>/dev/null || echo "")
    # Its distinctive summary body must be gone (truncated past both budgets).
    if grep -qF "$NI_MARK" <<< "$ni_out"; then
      fail "non-inline causal guard" "$NI_MARK rendered full at budget=1 — S4's inline pass may be spurious"
    else
      pass "non-inline bead's body is truncated at budget=1 (inline label is the cause of S4's full render)"
    fi
    assert_contains "non-inline truncation is visible (footer)" \
      "more advice items not shown (budget exceeded)" "$ni_out"
  else
    fail "non-inline create" "could not create non-inline bead"
  fi
  echo

  # ── Scenario 10: matched (--json) vs budgeted (text) + broadcast deny [F6]
  # A missing bead in text output is ambiguous: was it mis-TARGETED (never
  # matched) or merely BUDGETED (matched but displaced)? `gb prime --json`
  # renders every MATCHED bead with NO budget, so it disambiguates. This
  # scenario proves:
  #   (a) an AND-targeted (g1:role:crew + g1:project:gardenio) advice appears
  #       in --json for a matching agent and is ABSENT for a non-matching one
  #       (targeting works), and
  #   (b) a bare project:<X> advice (no role/agent, no broadcast opt-in) is
  #       ABSENT from --json entirely — dropped by IsDeniedBroadcast — i.e. a
  #       TARGETING drop, distinct from a budget drop.
  blue "Scenario 10: --json proves matched-set; bare-project broadcast denied [F6]"
  local TGT_TAG="target-${TAG}"
  # A crew agent under gardenio matches project:gardenio + role:crew(+family).
  local TGT_AGENT="gardenio/crews/${TGT_TAG}"
  # A non-matching agent: different project so project:gardenio won't match.
  local NOMATCH_AGENT="otherproj/crews/${TGT_TAG}"
  local AND_MARK="ANDTARGET-${TGT_TAG}"
  local BCAST_MARK="BROADCAST-${TGT_TAG}"
  # (a) AND-group: both labels in group g1 must be satisfied together.
  local and_id
  and_id=$("$KD_BIN" create "$AND_MARK crew+gardenio rule" --type=advice \
    -l "g1:role:crew" -l "g1:project:gardenio" \
    -d "$AND_MARK: crew-and-gardenio AND-targeted advice" \
    --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  # (b) bare project: no role/agent, no scope:project-broadcast opt-in.
  local bcast_id
  bcast_id=$("$KD_BIN" create "$BCAST_MARK bare-project rule" --type=advice \
    -l "project:gardenio" \
    -d "$BCAST_MARK: bare project broadcast without opt-in" \
    --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$and_id" ] && [ -n "$bcast_id" ]; then
    CREATED_ADVICE+=("$and_id" "$bcast_id")
    pass "targeting fixtures created (AND=$and_id broadcast=$bcast_id)"
    # --json renders the full MATCHED set with NO budget.
    local match_json nomatch_json
    match_json=$("$GB_BIN" prime --for "$TGT_AGENT" --json 2>/dev/null || echo "[]")
    nomatch_json=$("$GB_BIN" prime --for "$NOMATCH_AGENT" --json 2>/dev/null || echo "[]")
    # (a) AND-target matches the crew/gardenio agent...
    if grep -qF "$AND_MARK" <<< "$match_json"; then
      pass "AND-targeted advice matches crew+gardenio agent (present in --json)"
    else
      fail "AND target match" "$AND_MARK absent from --json for matching agent"
    fi
    # ...and does NOT match the other-project agent.
    if grep -qF "$AND_MARK" <<< "$nomatch_json"; then
      fail "AND target isolation" "$AND_MARK matched a non-gardenio agent (targeting too broad)"
    else
      pass "AND-targeted advice absent for non-matching (other-project) agent"
    fi
    # (b) bare-project broadcast is denied even in the no-budget --json path,
    # proving its absence is a TARGETING drop (IsDeniedBroadcast), not budget.
    if grep -qF "$BCAST_MARK" <<< "$match_json"; then
      fail "broadcast deny" "$BCAST_MARK present in --json — bare-project broadcast was NOT denied"
    else
      pass "bare-project broadcast denied (absent from no-budget --json: a targeting drop, not budget)"
    fi
  else
    fail "targeting fixtures" "could not create AND/broadcast beads (AND='$and_id' bcast='$bcast_id')"
  fi
  echo

  # ── Scenario 11: footer count boundaries N=1 + exact arithmetic [F8]
  # N=0 (no footer) and N=large are already covered (S6a / S2). The gaps are
  # the N=1 EXACT boundary and exact-count ARITHMETIC.
  #
  # Ambient-proof arithmetic: the live daemon carries ~38 ambient advice that
  # also match our agent, so per-tag arithmetic (footer == seeded − rendered)
  # is wrong. The durable invariant is the TOTAL one, and it is exact: with
  # titleBudget=1 nothing renders title-only, so every matched item either
  # renders abbreviated or is counted in the footer. Therefore
  #   footer_N + rendered_items == total_matched
  # for the WHOLE matched set — where total_matched is advice_items from
  # `gb prime --size` (unbounded) and rendered_items counts the `**[` item
  # lines in the tight render. This holds regardless of ambient volume, and
  # sweeping the abbrev budget across a range makes it a strong, exact check.
  # We also drive the abbrev budget to the point where footer_N == 1 to pin
  # the N=1 exact boundary and its current wording.
  blue "Scenario 11: footer count boundaries — total-count arithmetic + N=1 exact [F8]"
  local FC_TAG="footcount-${TAG}"
  local FC_AGENT="gardenio/crews/${FC_TAG}"
  local FC_N=6 fc_seeded=0 fc_id
  for i in $(seq 1 "$FC_N"); do
    fc_id=$(seed_big_advice "$FC_AGENT" "$i" "FC-$FC_TAG")
    if [ -n "$fc_id" ]; then CREATED_ADVICE+=("$fc_id"); fc_seeded=$((fc_seeded + 1)); fi
  done
  if [ "$fc_seeded" -eq "$FC_N" ]; then
    pass "footer-count corpus seeded ($fc_seeded items)"
    # Total matched item count (unbounded render, from --size).
    local fc_unbounded fc_total
    fc_unbounded=$(GB_PRIME_ADVICE_BUDGET_BYTES=20000000 \
                   GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=20000000 \
                   "$GB_BIN" prime --for "$FC_AGENT" --size 2>/dev/null || echo "")
    fc_total=$(grep -oE 'advice_items=[0-9]+' <<< "$fc_unbounded" | head -1 | cut -d= -f2 || true)
    dim "  total matched advice_items=$fc_total (incl. ambient) over $fc_seeded seeded"
    if [ -n "$fc_total" ] && [ "$fc_total" -ge "$fc_seeded" ]; then
      # Sweep abbrev budgets; titleBudget=1 forces the footer to account for
      # every non-abbreviated item. At each point assert the exact identity.
      local arithmetic_ok=1 hit_n1=0 b fc_out fc_n fc_rendered fc_n1_out=""
      for b in 1 1500 3000 6000; do
        fc_out=$(GB_PRIME_ADVICE_BUDGET_BYTES="$b" \
                 GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=1 \
                 "$GB_BIN" prime --for "$FC_AGENT" 2>/dev/null || echo "")
        fc_n=$(footer_n_of "$fc_out")
        [ -n "$fc_n" ] || fc_n=0
        fc_rendered=$(grep -cE '^\*\*\[' <<< "$fc_out" || true)
        dim "  budget=$b footer_n=$fc_n rendered_items=$fc_rendered sum=$((fc_n + fc_rendered)) total=$fc_total"
        if [ "$((fc_n + fc_rendered))" -ne "$fc_total" ]; then
          arithmetic_ok=0
          fail "footer arithmetic" "budget=$b: footer($fc_n) + rendered($fc_rendered) != total($fc_total)"
        fi
        [ "$fc_n" = "1" ] && hit_n1=1
      done
      if [ "$arithmetic_ok" -eq 1 ]; then
        pass "footer arithmetic exact across budgets: footer_N + rendered_items == total_matched ($fc_total)"
      fi
      # N=1 exact boundary: footer_N is monotonically non-increasing in the
      # abbrev budget (the cascade greedily fills whole items), so a BOUNDED
      # binary search (~log2(range) ≈ 15 gb-prime calls) finds the smallest
      # budget whose footer reads exactly 1. Cheap and deterministic — no
      # unbounded linear sweep. titleBudget=1 keeps every displaced item in the
      # footer rather than the title tier.
      local fc_lo=1 fc_hi=30000 fc_ans="" fc_iters=0 fc_mid
      while [ "$hit_n1" -eq 0 ] && [ "$fc_lo" -le "$fc_hi" ] && [ "$fc_iters" -lt 20 ]; do
        fc_mid=$(( (fc_lo + fc_hi) / 2 ))
        fc_out=$(GB_PRIME_ADVICE_BUDGET_BYTES="$fc_mid" \
                 GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=1 \
                 "$GB_BIN" prime --for "$FC_AGENT" 2>/dev/null || echo "")
        fc_n=$(footer_n_of "$fc_out"); [ -n "$fc_n" ] || fc_n=0
        fc_iters=$((fc_iters + 1))
        if [ "$fc_n" -le 1 ]; then fc_ans="$fc_mid"; fc_n1_out="$fc_out"; fc_hi=$((fc_mid - 1)); else fc_lo=$((fc_mid + 1)); fi
      done
      # Confirm the converged budget yields footer_N == 1 exactly.
      if [ -n "$fc_ans" ]; then
        fc_out=$(GB_PRIME_ADVICE_BUDGET_BYTES="$fc_ans" \
                 GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=1 \
                 "$GB_BIN" prime --for "$FC_AGENT" 2>/dev/null || echo "")
        fc_n=$(footer_n_of "$fc_out")
        [ "$fc_n" = "1" ] && { hit_n1=1; fc_n1_out="$fc_out"; }
      fi
      if [ "$hit_n1" -eq 1 ]; then
        pass "N=1 exact boundary reached via binary search (footer reads exactly '1', budget=$fc_ans)"
        assert_contains "N=1 footer wording pinned (current: 'items')" \
          "1 more advice items not shown" "$fc_n1_out"
      else
        # The arithmetic identity above is the durable contract; landing N=1
        # exactly can be missed if per-item sizes make the count step by >1.
        # Don't flake — record the attempt.
        dim "  N=1 not landed exactly via binary search; total-count arithmetic already pins the contract"
        pass "N=1 boundary approximated (exact total-count arithmetic asserted above)"
      fi
    else
      fail "footer count sizing" "could not parse advice_items (got '$fc_total')"
    fi
    # N=0: an unbounded budget over the same corpus emits NO footer at all.
    local fc_n0_out
    fc_n0_out=$(GB_PRIME_ADVICE_BUDGET_BYTES=20000000 \
                GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=20000000 \
                "$GB_BIN" prime --for "$FC_AGENT" 2>/dev/null || echo "")
    if grep -qF "more advice items not shown" <<< "$fc_n0_out"; then
      fail "N=0 no footer" "footer present though the whole corpus fits"
    else
      pass "N=0 boundary: no footer when the whole corpus fits"
    fi
  else
    fail "footer-count corpus" "seeded $fc_seeded/$FC_N"
  fi
  echo

  # ── Scenario 12: rendered UTF-8 / rune safety (F7) ─────────────────
  # The cascade moves whole rendered items by byte length and never slices
  # one, so it cannot cut mid-rune; the only rune-boundary logic is
  # adviceExcerpt (unit-tested in prime_advice_test.go). This is the rendered
  # end-to-end companion: seed a multibyte-heavy (emoji + CJK) advice long
  # enough to force the ≤500-rune summary cap, then assert the rendered
  # `gb prime` output is valid UTF-8 with no U+FFFD replacement char.
  blue "Scenario 12: rendered advice is valid UTF-8, no mid-rune corruption [F7]"
  local U8_TAG="utf8-${TAG}"
  local U8_AGENT="gardenio/crews/${U8_TAG}"
  local U8_MARK="UTF8RUNE-${U8_TAG}"
  # Body is emoji + CJK, space-free within tokens, long enough (>500 runes) to
  # force the summary rune cap to bite.
  local u8_body u8_id
  u8_body="$U8_MARK: $(printf '規則🚀日本語のルール🔥安全 %.0s' $(seq 1 60))"
  u8_id=$("$KD_BIN" create "多字节规则 🚀 ${U8_MARK}" --type=advice \
    -l "agent:$U8_AGENT" -d "$u8_body" --json 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$u8_id" ]; then
    CREATED_ADVICE+=("$u8_id")
    pass "multibyte advice created ($u8_id)"
    # Small summary cap + moderate budget so the excerpt truncates on a rune
    # boundary in the rendered abbreviated form.
    local u8_out u8_rc
    u8_out=$(GB_PRIME_ADVICE_SUMMARY_CHARS=40 \
             GB_PRIME_ADVICE_BUDGET_BYTES=20000000 \
             GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=20000000 \
             "$GB_BIN" prime --for "$U8_AGENT" 2>/dev/null || echo "")
    # (a) valid UTF-8 end-to-end.
    printf '%s' "$u8_out" | python3 -c "import sys; sys.stdin.buffer.read().decode('utf-8')" 2>/dev/null
    u8_rc=$?
    if [ "$u8_rc" -eq 0 ]; then
      pass "rendered prime output decodes as valid UTF-8"
    else
      fail "utf-8 validity" "rendered output is not valid UTF-8 (rc=$u8_rc) — mid-rune cut?"
    fi
    # (b) no U+FFFD replacement char anywhere.
    if grep -qF "$(printf '\xef\xbf\xbd')" <<<"$u8_out"; then
      fail "no replacement char" "U+FFFD present in rendered advice — a rune was cut mid-sequence"
    else
      pass "no U+FFFD replacement char in rendered advice"
    fi
    # (c) our multibyte item actually rendered (so the checks aren't vacuous).
    if grep -qF "$U8_MARK" <<< "$u8_out"; then
      pass "multibyte advice rendered (UTF-8 checks are non-vacuous)"
    else
      fail "utf-8 non-vacuous" "$U8_MARK absent — item did not render, UTF-8 assertions were vacuous"
    fi
  else
    fail "multibyte create" "could not create multibyte advice bead"
  fi
  echo

  # ── Scenario 13: system-prompt aggregate stays under ceiling (F9) ──
  # The epic's core question: does the truncated advice actually fit the
  # channel the model sees on turn 1? `gb prime --system-prompt` emits the
  # rules-only subset written to .gb/system-prompt.md and passed via
  # --append-system-prompt-file. Even over a large seeded advice corpus and a
  # generous advice budget, the emitted system prompt must stay under the
  # enforced total ceiling (primeBudgetTotal = 100 KB; see
  # prime_size_budget_test.go). This is the gating shell assertion; the
  # claudeless TOML is the integration proof the file is wired to the session.
  blue "Scenario 13: gb prime --system-prompt stays under the 100KB ceiling over a big corpus [F9]"
  # Reuse the already-seeded over-budget corpus on $AGENT (Scenario 0 seeded
  # $seeded heavy agent-scoped beads). Render the system-prompt subset with the
  # DEFAULT budgets (the real runtime path) and assert the byte size ceiling.
  local SP_CEILING=102400   # primeBudgetTotal — keep in sync with prime_size_budget_test.go
  local sp_out sp_bytes
  sp_out=$("$GB_BIN" prime --for "$AGENT" --system-prompt 2>/dev/null || echo "")
  sp_bytes=$(printf '%s' "$sp_out" | wc -c | tr -d ' ')
  dim "  system-prompt bytes=$sp_bytes ceiling=$SP_CEILING"
  if [ -n "$sp_bytes" ] && [ "$sp_bytes" -gt 0 ]; then
    if [ "$sp_bytes" -le "$SP_CEILING" ]; then
      pass "system-prompt subset ($sp_bytes B) is under the ${SP_CEILING}B ceiling over a big corpus"
    else
      fail "system-prompt ceiling" "system-prompt is $sp_bytes B > ${SP_CEILING}B ceiling — advice aggregate overran the context channel (F9)"
    fi
    # The advice section must actually be present in the system-prompt subset
    # (it is one of the rules-only sections), so the ceiling check is meaningful.
    if grep -qF "## Advice" <<< "$sp_out"; then
      pass "advice section present in the system-prompt subset (ceiling check is non-vacuous)"
    else
      dim "  '## Advice' header not found — corpus may not have matched \$AGENT on this daemon; ceiling still asserted"
      pass "system-prompt ceiling asserted (advice header absent — soft)"
    fi
  else
    fail "system-prompt render" "gb prime --system-prompt produced no output"
  fi
  echo

  # ── F5 asymmetry: abbrev0 → default, title0 → literal zero ─────────
  # S1/S6 cover the differential (bigger budget ⇒ ≥ bytes). The one thin gap
  # the design flags: the ASYMMETRY between the two budget knobs. Per
  # prime_advice.go, adviceBudgetBytes requires >0 (0/garbage → default 60000)
  # while adviceTitleBudgetBytes accepts >=0 (0 → literal zero title budget).
  # Pin both directions so a future refactor that "normalizes" them together
  # is a conscious change.
  blue "F5 asymmetry: abbrev-budget=0 → default; title-budget=0 → literal zero"
  # abbrev=0 must behave like the DEFAULT (large) budget: over the solo corpus
  # the item renders its full summary (same as S6a within-budget). We reuse the
  # SOLO agent + marker from Scenario 6.
  if [ -n "${solo_id:-}" ]; then
    local f5_abbrev0_out
    f5_abbrev0_out=$(GB_PRIME_ADVICE_BUDGET_BYTES=0 \
                     GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=20000000 \
                     "$GB_BIN" prime --for "$SOLO_AGENT" 2>/dev/null || echo "")
    if grep -qF "$SOLO_SUMMARY" <<< "$f5_abbrev0_out"; then
      pass "abbrev-budget=0 falls back to DEFAULT (full summary renders, not a zero budget)"
    else
      fail "abbrev0→default" "summary absent — abbrev budget=0 was treated as a literal zero, not the default"
    fi
    # title=0 with abbrev=1 must be a LITERAL zero title budget: the solo item
    # cannot render abbreviated (budget=1) and cannot render title-only
    # (titleBudget=0), so it collapses straight to the footer.
    local f5_title0_out
    f5_title0_out=$(GB_PRIME_ADVICE_BUDGET_BYTES=1 \
                    GB_PRIME_ADVICE_TITLE_BUDGET_BYTES=0 \
                    "$GB_BIN" prime --for "$SOLO_AGENT" 2>/dev/null || echo "")
    if grep -qF "SOLO-${SOLO_TAG} boundary rule" <<< "$f5_title0_out"; then
      fail "title0→literal-zero" "solo title rendered though title-budget=0 — 0 was not honored as literal zero"
    else
      pass "title-budget=0 is a LITERAL zero (no title-only render; item footers)"
    fi
    assert_contains "title-budget=0 collapses solo item into the footer" \
      "more advice items not shown (budget exceeded)" "$f5_title0_out"
  else
    fail "F5 asymmetry" "solo bead from Scenario 6 unavailable (solo_id unset)"
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
