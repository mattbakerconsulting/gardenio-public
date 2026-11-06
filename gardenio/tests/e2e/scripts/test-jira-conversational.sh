#!/usr/bin/env bash
# test-jira-conversational.sh — jira-bridge CONVERSATIONAL round-trip E2E
#
# Closes the gap the outbox suite leaves open (ka-0000003999, epic ka-0000007401):
# the E2E suite exercises the jira-OUTBOX security matrix (test-jira-outbox-*.sh)
# but NOTHING walks the conversational loop the F1–F5 features built —
# mention -> spawn -> reply -> squawk -> follow-up. F1–F5 have solid UNIT
# coverage (jira_reply_test.go, jira_squawk_forward_wired_test.go,
# jira_fsm_guardrails_spine_test.go); this is their END-TO-END gate.
#
# ── Why a built-from-source probe, not a live-bridge port-forward ─────────────
#
# Two hard constraints shape this module:
#
#   1. The jira-bridge is OFF in the ephemeral e2e namespace (see
#      deploy-values), so there is no live /webhook to POST to — the same reason
#      test-jira-bridge.sh Scenario 6 skips its mention-attribution leg.
#   2. The mention/follow-up paths call daemon.SpawnAgent, which CREATES a
#      type=agent bead — and the deployed controller/reconciler mints a REAL pod
#      for ANY active agent bead (the image is controller-wide, not project-
#      gated). Driving the real spawn path against the SHARED daemon would spawn
#      costly pods in the shared namespace — the exact class of accident
#      a shared-namespace guardrail would exist to prevent. `kd serve` needs Postgres, so an
#      isolated daemon is not cheap to stand up in a shard either.
#
# So this module follows the EXACT precedent of the trigger-source-* claudeless
# scenarios' cmd/trigger-probe (the blessed "real bridge + stub Jira, no product
# image" pattern) and the gvisor build-in-shell probes: it builds and runs
# cmd/jira-conv-probe, a TEST-ONLY binary that wires the REAL bridge components
# (JiraWebhookHandler, JiraMentionHandler, JiraReplyAPI, JiraSquawkForwarder,
# JiraAgentResolver, WritePolicyStore) through the REAL /webhook HTTP handler and
# the REAL SSE-driven squawk handler, against:
#   - a STUB Jira (httptest server) that RECORDS every outbound comment write and
#     serves comment history — the same "stub Jira records the write" shape the
#     outbox tests use to prove/deny an outbound write; and
#   - an in-process mock daemon that RECORDS CreateBead/SpawnAgent/CloseBead — so
#     the spawn is observed as a recorded call and NO real bead (hence NO pod) is
#     ever created.
#
# The probe drives the mention path with jiraWebhookDevMode (recorded-webhook
# injection through /webhook, HMAC skipped) exactly as the bead description asks.
# It is hermetic: ZERO pods, ZERO shared-daemon writes, ZERO sandbox rig — so it
# is green today and is a HARD gate (unlike the outbox live matrix, which
# skip-PASSES until the operator provisions the sandbox creds).
#
# ── Legs (each a hard sub-gate; any FAIL reddens the shard) ───────────────────
#
#   1  mention        comment_created @gardenio mention -> agent spawn with
#                     jira_key + jira_comment_id stamped on the task AND agent bead.
#   2  routing        multi-project mention routing (MR !5674, G3): with a
#                     projectMap wired, a SECOND JIRA prefix routes its mention to
#                     a DIFFERENT boat project's role/credentials, and a BLOCKED
#                     prefix (FSM) is denied — no spawn, no task — even when it
#                     carries a map entry (the deny gate keys on the prefix, not
#                     on map membership). Backfills ka-0000002247.
#   3  reply          gb jira reply -> AddCommentWithVisibility(internal) recorded
#                     by the stub Jira on the agent's OWN bound ticket; a reply
#                     naming a different ticket is refused (bound-ticket scoping).
#   3b reply-reclassify  the reply handler's post-error classification
#                     (classifyReplyPostError) is only reachable when the stub Jira
#                     can FAIL a comment POST — the base stub only 201s, so the 422
#                     branch was dead code from the probe's view (the exact
#                     structural blindness ka-0000005455 names). A Jira 400 (content
#                     rejection) must surface as 422 "retry as plain text"; a Jira
#                     500 AND a 429 must stay 502 (transient outage, NOT
#                     reclassified). The 500/429 cases are the MANDATORY anti-vacuity
#                     controls — without them a blanket-422 regression passes.
#   3c reply-codemark ADF code-mark exclusivity (appendCodeMark): a bold code span
#                     **`kubectl`** sent through the REAL markdown-comment client
#                     must record an ADF text node whose marks are EXACTLY [code]
#                     (no `strong`) — a node with both is what Jira Cloud 400s. The
#                     leg also asserts a plain bold node + a link mark survive
#                     (anti-vacuity: a fix that stripped ALL marks would red) and
#                     that a linked code span keeps code+link (the one coexistence
#                     the schema allows). No bold-code fixture existed in the corpus
#                     before, so appendCodeMark never ran under any harness.
#   4  squawk         a closed squawk bead from a Jira-bound agent -> internal
#                     note via the SSE squawk forwarder.
#   5  followup       a follow-up comment webhook -> a LIVE agent is nudged; a
#                     STOPPED (terminal agent_state) agent is RE-SPAWNED under the
#                     same name (F3 session-resume).
#   6  fsm-negative   an FSM-key webhook (mention + reply + squawk) yields ZERO
#                     outbound Jira writes AND ZERO spawn (F5 spine in E2E).
#   7  spawn-ack      a @gardenio mention with the JiraSpawnAcknowledger wired onto
#                     OnSpawn (as cmd/jira-bridge wires it) posts ONE immediate
#                     internal-note "👋 On it" comment to the ticket — the Jira
#                     analog of the Slack agent card (ka-0000003551; feature
#                     ka-0000001164). The leg carries its OWN anti-vacuity control:
#                     the SAME mention with OnSpawn nil records ZERO Jira writes,
#                     so a green assertion proves the ack path (not a latent
#                     mention-path write) produced the note. Legs 1–6 pass
#                     identically with the ack deleted, so without this leg the ack
#                     is structurally unreachable from the e2e.
#   8  crosslink-nowake  GitLab's Jira integration posts an automated "<user>
#                     mentioned this issue in a merge request/commit of <project>"
#                     crosslink whenever ANY branch cites the ticket. The bridge
#                     must BRIDGE that comment to the bead but wake NOTHING — no
#                     nudge, no F3 respawn — or a finished agent resurrects itself
#                     off its own MR in a self-sustaining loop (observed on
#                     DEVOPS-738: same agent respawned 3x in 64 min; ka-0000005445,
#                     jira_webhook.go:447). Drives the crosslink against a CLOSED
#                     task whose agent is TERMINAL (the exact F3 respawn shape) and
#                     asserts 0 spawns + 0 nudges YET comment-count == 1 (wake-only
#                     suppression, not a drop), across the merge-request and commit
#                     markers. Carries the MANDATORY anti-vacuity control: the SAME
#                     fixture with a HUMAN body that has the verb but no marker
#                     object ("I mentioned this issue in standup") respawns exactly
#                     once, so a globally-dead respawn path cannot pass vacuously.
#   9  bridge-nowake   The bridge posts its OWN Jira comments (claim / "Automated
#                     MR created" / "MR merged" / bead-closure). It must BRIDGE
#                     them to the bead but NOT wake the assignee — else the agent
#                     wakes on its own notice and loops (observed on DEVOPS-897;
#                     ka-0000004998, jira_webhook.go:461). Unlike a hand-authored
#                     fixture, this leg stands up the REAL jira-SYNC producer
#                     (bridge.NewJiraSync) behind the REAL SSE stream and CAPTURES
#                     the exact bytes it posts, so a producer-side wording drift OR
#                     a matcher-table narrowing reds it (the trap ka-0000000099 was
#                     filed for; advice ka-0000001521). Replays each of the FOUR
#                     notices in BOTH Jira Server (plain string) AND Jira Cloud (raw
#                     ADF) shapes (ka-0000006177), each asserting 0 spawns + 0
#                     nudges YET comment-count == 1. The bead-closure notice
#                     (ka-0000004442) is now in bridgeGeneratedMarkers
#                     (jira_webhook.go:740), so it is suppressed like its siblings.
#                     MANDATORY anti-vacuity: a human quoting a generated sentence,
#                     a human opening "Gardenio agent please take another look"
#                     (claim prefix, no suffix), a plain follow-up, and a human
#                     quoting the CLOSURE sentence back (closure marker has an EMPTY
#                     suffix, so the prefix-vs-contains boundary is load-bearing)
#                     all still wake exactly once.
#
# ── Skip-clean contract ──────────────────────────────────────────────────────
#
# The Go toolchain + the cmd/jira-conv-probe source are present in every e2e
# shard container (the shard `use:` list includes the build-jira-conv-probe task,
# and go is installed). If either is somehow absent the module SKIPS CLEANLY
# (exit 0) with a LOUD banner — an absent toolchain must never red the suite —
# mirroring the gvisor probes' skip_all("loud") contract. When present, every leg
# is a HARD assertion.

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
RUN_ID="${RUN_ID:-$$}"
PROBE_BIN=""

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

# skip_all exits 0 (a missing toolchain must NOT red the suite) with a LOUD
# banner so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP jira-conversational (LOUD): $why"
  red " This run did NOT assert the mention->spawn->reply->squawk->follow-up"
  red " conversational round-trip. A green suite here proves NOTHING about the"
  red " F1–F5 conversational loop. The e2e shard is built with a Go toolchain +"
  red " the cmd/jira-conv-probe source (build-jira-conv-probe task) — if this"
  red " fires in CI, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

PATH_INSTALLED=0
cleanup() {
  # Only remove a temp binary we built; never a PATH-installed one.
  [ "$PATH_INSTALLED" != "1" ] && [ -n "$PROBE_BIN" ] && rm -f "$PROBE_BIN" 2>/dev/null || true
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " jira-bridge conversational round-trip E2E (ka-0000003999)"
echo " mention -> spawn -> reply -> squawk -> follow-up (+ FSM negative)"
echo "════════════════════════════════════════════════════"

# ── 1. Resolve the probe binary ─────────────────────────────────────
# Prefer a binary pre-built + smoke-checked
# onto PATH (the trigger-probe precedent, with per-source caching). Fall back to
# an in-shell build from source (the gvisor-probe precedent) for local dev and
# for robustness if the pre-built binary is absent. Only when BOTH the pre-built
# binary is absent AND no Go toolchain/source is available do we skip clean.
if command -v jira-conv-probe >/dev/null 2>&1; then
  PROBE_BIN="$(command -v jira-conv-probe)"
  # PROBE_BIN is on PATH, not a temp file — do not let cleanup() rm it.
  PATH_INSTALLED=1
  pass "using pre-built jira-conv-probe ($PROBE_BIN)"
else
  PATH_INSTALLED=0
  command -v go >/dev/null 2>&1 \
    || skip_all "no pre-built jira-conv-probe on PATH and no go toolchain to build it"
  [ -d "$CONTROLLER_DIR/cmd/jira-conv-probe" ] \
    || skip_all "no pre-built jira-conv-probe on PATH and cmd/jira-conv-probe missing under $CONTROLLER_DIR"
  blue "[build] cmd/jira-conv-probe (from source)"
  PROBE_BIN="$(mktemp "/tmp/jira-conv-probe.${RUN_ID}.XXXXXX")"
  BUILD_LOG="/tmp/jira-conv-probe-build-${RUN_ID}.log"
  if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 go build -o "$PROBE_BIN" ./cmd/jira-conv-probe/ ) >"$BUILD_LOG" 2>&1; then
    pass "cmd/jira-conv-probe built from source"
  else
    fail "build jira-conv-probe" "go build failed: $(tail -8 "$BUILD_LOG")"
    # A build failure IS a hard failure — the probe is the whole module.
    blue "════════════════════════════════════════════════════"
    red " ✗ 1/1 assertions failed (probe build)"
    for err in "${ERRORS[@]}"; do red "   - $err"; done
    blue "════════════════════════════════════════════════════"
    exit 1
  fi
fi
echo

# ── 2. Run the probe; capture per-leg lines ─────────────────────────
# The probe prints one "LEG <name>: PASS|FAIL <detail>" line per leg and exits
# non-zero if any leg failed. We tee its output, map each leg line to a pass/fail
# assertion, and require ALL of the EXPECTED_LEGS below to be present (a missing
# leg is a failure, so a probe that silently drops a leg cannot pass).
blue "[run] driving mention -> spawn -> reply -> squawk -> follow-up (+ FSM negative)"
PROBE_OUT="/tmp/jira-conv-probe-${RUN_ID}.out"
probe_rc=0
"$PROBE_BIN" >"$PROBE_OUT" 2>/tmp/jira-conv-probe-${RUN_ID}.err || probe_rc=$?

EXPECTED_LEGS="mention routing reply reply-reclassify reply-codemark squawk followup fsm-negative spawn-ack crosslink-nowake bridge-nowake"
for leg in $EXPECTED_LEGS; do
  line=$(grep -E "^LEG ${leg}: " "$PROBE_OUT" 2>/dev/null | head -1 || true)
  if [ -z "$line" ]; then
    fail "leg $leg" "no result line emitted by the probe (leg dropped)"
    continue
  fi
  detail="${line#LEG ${leg}: }"
  case "$detail" in
    PASS*) pass "leg $leg — ${detail#PASS }" ;;
    FAIL*) fail "leg $leg" "${detail#FAIL }" ;;
    *)     fail "leg $leg" "unrecognized result: $detail" ;;
  esac
done

# Belt-and-suspenders: the probe's own exit code must agree with our parse. If
# the probe exited non-zero but every leg line parsed PASS (should be
# impossible), treat it as a failure so a probe-level error is never masked.
if [ "$probe_rc" -ne 0 ] && [ "$FAIL" -eq 0 ]; then
  fail "probe exit code" "probe exited $probe_rc but all parsed legs passed — probe-level error (stderr: $(tail -3 /tmp/jira-conv-probe-${RUN_ID}.err 2>/dev/null))"
fi
echo

# ── Summary ─────────────────────────────────────────────────────────
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total assertions passed"
else
  red " ✗ $FAIL/$total assertions failed"
  echo
  for err in "${ERRORS[@]}"; do red "   - $err"; done
  echo
  dim "  Probe output:"
  sed 's/^/    /' "$PROBE_OUT" 2>/dev/null || true
fi
blue "════════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
