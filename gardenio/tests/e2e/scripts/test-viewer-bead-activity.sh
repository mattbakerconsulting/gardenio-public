#!/usr/bin/env bash
# test-viewer-bead-activity.sh — viewer activity-feed operated_by E2E
#
# Covers the read-path integration shape unblocked by !2040
# (b71ae811): bridge / kd CLI stamps operated_by → kbeads persists →
# viewer's /b/{id}/activity renders the operator in the activity-actor
# cell. Source coverage doc:
# gardenio/docs/research/e2e-coverage-operated-by-activity-feed.md.
#
# Bead chain: ka-0000005811 (coverage gap) → ka-0000007045 (this impl).
# R5 (gd-xmfGWkm0H1): the probe_operated_by_supported / skip escape hatch
# is REMOVED. A deployed server that fails to stamp operated_by now makes
# this module fail RED — green CI means the column actually populated,
# not that the assertion was skipped (epic ka-0000006809).
#
# Scenarios:
#   1. Comment carrying X-Operated-By persists operated_by on the comment
#      (asserted via the HTTP comments API) AND renders the comment.added
#      activity row on /b/{id}/activity. The persisted-column check is the
#      falsifiable "did the stamp land?" assertion — a missing stamp fails.
#   2. State-transition event (close) carrying X-Operated-By renders
#      that operator id in the activity-actor cell, NOT the calling agent
#      name. Guards the operated_by → actor precedence end-to-end (and,
#      by construction, that the column populated).
#   3. Negative — event written without X-Operated-By renders a
#      non-empty actor (falls through to actor / payload), never the
#      empty string. Guards a regression where !2040's scan defaults a
#      missing operator to "" instead of falling through.
#   4. CREATE-time operated_by emits a mutation_history row with
#      field='operated_by', mutation_type='create' (gd-uJkKs75lH1),
#      asserted via the HTTP mutations API.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-viewer-bead-activity.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster (default gardenio-e2e)
#   - kd, jq, curl in PATH
#   - lib-gitlab-token.sh's mint_user_token resolves a GitLab-direct bearer
#     (GARDENIO_E2E_GITLAB_BEARER or the static daemon token must be set).
#   - The deployed kbeads server MUST stamp operated_by. R5 removed the
#     BEADS_OPERATED_BY_ENABLED escape hatch — if the server fails to
#     stamp the column, the operated_by scenarios FAIL red. They are
#     never skipped for a missing stamp.
#
# Idempotency: every created bead carries the title prefix
#   viewer-bead-activity-e2e-<random>
# and is closed in the cleanup trap. Reruns do not collide.
#
# Hard rules: do NOT edit .captain/quarantines/. Do not touch any other
# e2e module. Port-forward + viewer_get helpers are inlined here per the
# coverage doc — extract into lib-viewer.sh on the third caller.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gitlab-token.sh"

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
VIEWER_SVC="${VIEWER_SVC:-${RELEASE}-beads-viewer}"
VIEWER_PF_PORT="${VIEWER_PF_PORT:-19601}"
BEADS_PF_PORT="${BEADS_PF_PORT:-19602}"
KD_BIN="${KD_BIN:-kd}"
NON_MATT_EMAIL="${NON_MATT_EMAIL:-e2e-not-matt-$$@example.com}"
TITLE_PREFIX="viewer-bead-activity-e2e"
RANDOM_SUFFIX="$$-$(date +%s)"
COMMENT_OPERATOR="oidc:e2e-commenter-${RANDOM_SUFFIX}@example.com"
EVENT_OPERATOR="oidc:e2e-closer-${RANDOM_SUFFIX}@example.com"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
VIEWER_PF_PID=""
BEADS_PF_PID=""
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

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

cleanup() {
  for id in ${CREATED_BEADS[@]+"${CREATED_BEADS[@]}"}; do
    "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  [ -n "$VIEWER_PF_PID" ] && kill "$VIEWER_PF_PID" 2>/dev/null || true
  [ -n "$BEADS_PF_PID"  ] && kill "$BEADS_PF_PID"  2>/dev/null || true
}
trap cleanup EXIT

setup_viewer_portforward() {
  if ! kubectl -n "$NAMESPACE" get svc "$VIEWER_SVC" >/dev/null 2>&1; then
    yellow "viewer service $VIEWER_SVC not found in $NAMESPACE — skipping module"
    return 1
  fi
  blue "Port-forwarding ${NAMESPACE}/${VIEWER_SVC} :${VIEWER_PF_PORT} → 8095..."
  kubectl -n "$NAMESPACE" port-forward "svc/${VIEWER_SVC}" \
    "${VIEWER_PF_PORT}:8095" >/tmp/viewer-ba-pf.log 2>&1 &
  VIEWER_PF_PID=$!
  local i
  for i in 1 2 3 4 5 6 7 8 9 10; do
    if curl -sf --max-time 1 -o /dev/null \
        "http://localhost:${VIEWER_PF_PORT}/healthz" 2>/dev/null \
        || curl -s --max-time 1 -o /dev/null \
            "http://localhost:${VIEWER_PF_PORT}/" 2>/dev/null; then
      dim "  Viewer tunnel ready on :${VIEWER_PF_PORT} (pid $VIEWER_PF_PID)"
      return 0
    fi
    sleep 0.5
  done
  yellow "  Viewer port-forward did not become healthy within 5s; continuing"
  return 0
}

setup_beads_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    dim "  using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return 0
  fi
  blue "Port-forwarding ${NAMESPACE}/${BEADS_SVC} :${BEADS_PF_PORT} → 8080..."
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" \
    "${BEADS_PF_PORT}:8080" >/tmp/viewer-ba-beads-pf.log 2>&1 &
  BEADS_PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${BEADS_PF_PORT}"
  if [ -z "${BEADS_TOKEN:-}" ]; then
    local secret
    secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
    [ -n "$secret" ] && export BEADS_TOKEN="$secret"
  fi
}

# viewer_get <path> <token> [extra_curl_args...]
# GETs http://localhost:$VIEWER_PF_PORT$path with bearer auth.
# Echoes "<status>\n<body>" on stdout (status on first line, body on rest).
viewer_get() {
  local path="$1" token="$2"
  shift 2
  local out status
  out=$(mktemp)
  status=$(curl -sk --max-time 10 \
    -H "Authorization: Bearer ${token}" \
    -H "Accept: text/html" \
    -o "$out" \
    -w '%{http_code}' \
    "$@" \
    "http://localhost:${VIEWER_PF_PORT}${path}" 2>/dev/null || echo "000")
  printf '%s\n' "$status"
  cat "$out"
  rm -f "$out"
}

bead_id_from_json() {
  printf '%s' "$1" | jq -r '.id // empty' 2>/dev/null || printf ''
}

# comment_operated_by_persisted <bead_id> <operator>
# Returns 0 iff the bead has at least one comment whose persisted
# operated_by column equals <operator>, read back via the HTTP comments
# API. This is the load-bearing "did the server actually stamp the
# column?" assertion. R5 (gd-xmfGWkm0H1) removed the probe/skip escape
# hatch that this logic used to gate: a missing stamp is now a hard test
# failure at the call site, never a silent skip.
comment_operated_by_persisted() {
  local bead_id="$1" operator="$2" api
  api=$(curl -sk --max-time 10 \
    ${BEADS_TOKEN:+-H "Authorization: Bearer ${BEADS_TOKEN}"} \
    "${BEADS_HTTP_URL}/v1/beads/${bead_id}/comments" 2>/dev/null || printf '{}')
  printf '%s' "$api" | jq -e --arg op "$operator" \
    '.comments | map(select(.operated_by==$op)) | length > 0' \
    >/dev/null 2>&1
}

# activity_actor_for_topic <html> <topic>
# Extracts the activity-actor strong-text for the first row whose
# data-topic attribute matches <topic>. Empty string when no row matches.
#
# grep is captured in its own command substitution (|| true) rather than
# piped straight into head/sed. Two reasons, both load-bearing for R5's
# "green CI is trustworthy evidence" goal (epic ka-0000006809), since the
# operated_by scenarios now run this helper unconditionally:
#   - No `| head -n1`: under `set -o pipefail`, head closing the pipe
#     after one line SIGPIPEs grep (exit 141) and aborts a genuine PASS.
#   - `|| true`: grep exits 1 on no-match; a bare pipeline would trip
#     `set -e` instead of yielding the empty string callers handle.
# sed reads the whole captured string, so nothing closes a pipe early.
activity_actor_for_topic() {
  local html="$1" topic="$2" matches
  matches=$(printf '%s' "$html" \
    | tr -d '\n' \
    | grep -oE "<li class=\"activity-row activity-${topic}\"[^<]*<span class=\"activity-glyph\"[^<]*</span>[[:space:]]*<span class=\"activity-actor\"><strong>[^<]+</strong>") \
    || true
  printf '%s' "$matches" \
    | sed -nE '1 s|.*<strong>([^<]+)</strong>.*|\1|p'
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " viewer bead-activity operated_by E2E (ka-0000007045)"
  blue "═══════════════════════════════════════════════════"

  if ! setup_viewer_portforward; then
    skip "viewer-bead-activity module" "viewer service $VIEWER_SVC absent"
    summary
    return 0
  fi
  setup_beads_portforward

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    skip "viewer-bead-activity module" \
      "kd cannot reach beads daemon at ${BEADS_HTTP_URL:-unset}"
    summary
    return 0
  fi

  # ── Mint a non-Matt token for viewer GETs. ───────────────────────
  local non_matt_token=""
  if ! non_matt_token=$(mint_user_token "$NON_MATT_EMAIL" 2>/tmp/viewer-ba-mint.err); then
    yellow "mint_user_token failed for ${NON_MATT_EMAIL} — see /tmp/viewer-ba-mint.err"
    skip "scenario 1: comment operated_by chip" "no usable GitLab-direct bearer"
    skip "scenario 2: event operated_by chip"  "no usable GitLab-direct bearer"
    skip "scenario 3: missing operated_by fallback" "no usable GitLab-direct bearer"
    summary
    return 0
  fi
  dim "  minted non-Matt user token (sub=${NON_MATT_EMAIL})"
  # The viewer's own OIDC middleware stays pinned to the Keycloak issuer until
  # the viewer-login flip lands (ka-0000002083); it rejects this GitLab-direct /
  # static-daemon bearer with 401. SKIP-clean rather than hard-fail every gated
  # GET (bug gd-5M8iT2oNlr). Auto-enables when the viewer trusts the GitLab edge.
  if ! viewer_accepts_bearer "$non_matt_token"; then
    skip "viewer-bead-activity module" "viewer rejects the E2E transport bearer — viewer still OIDC-gated at the pod; viewer-login flip pending (ka-0000002083)"
    summary
    return 0
  fi

  # R5: no operated_by probe / skip. Each scenario asserts the column
  # populated at its own call site; a server that fails to stamp fails
  # the test RED (epic ka-0000006809).

  # ══════════════════════════════════════════════════════════════════
  # Scenario 1: Comment carrying operated_by persists the column and
  # renders the comment.added activity row on /b/{id}/activity.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 1: comment with operated_by persists column + renders row"
  local s1_id=""
  local s1_create_out
  s1_create_out=$("$KD_BIN" create "${TITLE_PREFIX}-comment-${RANDOM_SUFFIX}" \
    --type=task -d "scenario 1 ($RANDOM_SUFFIX)" --json 2>/dev/null || printf '{}')
  s1_id=$(bead_id_from_json "$s1_create_out")
  if [ -z "$s1_id" ]; then
    fail "create scenario-1 bead" "no ID returned from kd create"
  else
    CREATED_BEADS+=("$s1_id")
    pass "scenario-1 bead created ($s1_id)"

    BEADS_OPERATED_BY="$COMMENT_OPERATOR" "$KD_BIN" comment add "$s1_id" \
      "scenario 1 comment carrying operated_by ($RANDOM_SUFFIX)" \
      --json >/dev/null 2>&1 || true

    # Load-bearing assertion: the server MUST have persisted operated_by
    # on the comment. Previously gated behind probe_operated_by_supported
    # (skipped when the stamp was absent) — R5 makes a missing stamp a
    # hard failure so green CI is falsifiable evidence.
    if comment_operated_by_persisted "$s1_id" "$COMMENT_OPERATOR"; then
      pass "scenario 1: comment operated_by column persisted ($COMMENT_OPERATOR)"
    else
      fail "scenario 1: comment operated_by column persisted" \
        "server did not stamp operated_by=$COMMENT_OPERATOR (GET /v1/beads/${s1_id}/comments)"
    fi

    local resp1 status1 body1
    resp1=$(viewer_get "/b/${s1_id}/activity" "$non_matt_token")
    # Split "<status>\n<body>" with pure-bash expansion, not
    # `printf | head`/`tail`: under `set -o pipefail`, head closing the
    # pipe after one line can SIGPIPE printf (exit 141) and abort an
    # otherwise-green run (epic ka-0000006809 — green must be trustworthy).
    status1="${resp1%%$'\n'*}"
    body1="${resp1#*$'\n'}"
    if [ "$status1" != "200" ]; then
      fail "GET /b/${s1_id}/activity" "expected 200, got $status1"
    else
      # The comment payload sets payload.comment.author, which
      # activityActor uses for beads.comment.added events. The kd CLI's
      # comment add path resolves the author from the authenticated
      # identity (server-side), not from operated_by. The persisted
      # operated_by column is asserted directly above; here we confirm
      # the comment row renders.
      if grep -qE 'class="activity-row activity-beads.comment.added"' <<<"$body1"; then
        pass "scenario 1: comment.added activity row rendered"
      else
        fail "scenario 1: comment.added activity row rendered" \
          "no activity row with class beads.comment.added in response"
      fi
      # Direct chip presence: the operator string may appear in the
      # activity HTML, but comment.added rows render payload.comment.author
      # (server-derived), which can mask operated_by on that row. The
      # persisted-column assertion above is authoritative; presence here
      # is informational, not a gate.
      if grep -qF -- "$COMMENT_OPERATOR" <<<"$body1"; then
        pass "scenario 1: operated_by visible in activity HTML"
      else
        yellow "    note: operated_by string not present in activity HTML"
        yellow "    (comment.added uses payload.comment.author which can mask"
        yellow "    operated_by; the persisted-column assertion above covers it)."
      fi
    fi
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 2: State-transition event (close) carrying operated_by
  # renders that operator in the activity-actor cell, NOT the calling
  # agent. activityActor: operated_by → actor → payload.bead.assignee
  # → payload.bead.created_by. Asserting the actor == operator proves
  # the column populated end-to-end.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 2: close event with operated_by renders operator"
  local s2_id=""
  local s2_create_out
  s2_create_out=$("$KD_BIN" create "${TITLE_PREFIX}-event-${RANDOM_SUFFIX}" \
    --type=task -d "scenario 2 ($RANDOM_SUFFIX)" --json 2>/dev/null || printf '{}')
  s2_id=$(bead_id_from_json "$s2_create_out")
  if [ -z "$s2_id" ]; then
    fail "create scenario-2 bead" "no ID returned from kd create"
  else
    CREATED_BEADS+=("$s2_id")
    pass "scenario-2 bead created ($s2_id)"

    BEADS_OPERATED_BY="$EVENT_OPERATOR" "$KD_BIN" close "$s2_id" \
      >/dev/null 2>&1 || true

    local resp2 status2 body2
    resp2=$(viewer_get "/b/${s2_id}/activity" "$non_matt_token")
    status2="${resp2%%$'\n'*}"
    body2="${resp2#*$'\n'}"
    if [ "$status2" != "200" ]; then
      fail "GET /b/${s2_id}/activity" "expected 200, got $status2"
    else
      local closed_actor
      closed_actor=$(activity_actor_for_topic "$body2" "beads.bead.closed")
      if [ -z "$closed_actor" ]; then
        fail "scenario 2: bead.closed row present" \
          "no activity row with topic beads.bead.closed"
      elif [ "$closed_actor" = "$EVENT_OPERATOR" ]; then
        pass "scenario 2: bead.closed activity-actor == $EVENT_OPERATOR"
      else
        fail "scenario 2: bead.closed activity-actor matches operated_by" \
          "got '$closed_actor', expected '$EVENT_OPERATOR'"
      fi
    fi
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 3: Negative — event without operated_by renders a non-empty
  # actor (falls through to actor / payload), not the empty string.
  # Guards against a regression where the read scan defaults a missing
  # operator to "" instead of falling through.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 3: event without operated_by — non-empty actor fallback"
  local s3_id=""
  local s3_create_out
  s3_create_out=$("$KD_BIN" create "${TITLE_PREFIX}-noop-${RANDOM_SUFFIX}" \
    --type=task -d "scenario 3 ($RANDOM_SUFFIX)" --json 2>/dev/null || printf '{}')
  s3_id=$(bead_id_from_json "$s3_create_out")
  if [ -z "$s3_id" ]; then
    fail "create scenario-3 bead" "no ID returned from kd create"
  else
    CREATED_BEADS+=("$s3_id")
    pass "scenario-3 bead created ($s3_id)"

    # Explicitly clear BEADS_OPERATED_BY to simulate an autonomous /
    # scheduler-style write that does not stamp the column.
    env -u BEADS_OPERATED_BY "$KD_BIN" close "$s3_id" \
      >/dev/null 2>&1 || true

    local resp3 status3 body3
    resp3=$(viewer_get "/b/${s3_id}/activity" "$non_matt_token")
    status3="${resp3%%$'\n'*}"
    body3="${resp3#*$'\n'}"
    if [ "$status3" != "200" ]; then
      fail "GET /b/${s3_id}/activity" "expected 200, got $status3"
    else
      local closed_actor3
      closed_actor3=$(activity_actor_for_topic "$body3" "beads.bead.closed")
      if [ -z "$closed_actor3" ]; then
        fail "scenario 3: bead.closed row present" \
          "no activity row with topic beads.bead.closed"
      elif [ "$closed_actor3" = "unknown" ]; then
        fail "scenario 3: actor falls through, never 'unknown'" \
          "activity-actor rendered the literal 'unknown' (activityActor terminal fallback)"
      else
        pass "scenario 3: bead.closed activity-actor non-empty fallback ($closed_actor3)"
      fi
    fi
  fi
  echo

  # ══════════════════════════════════════════════════════════════════
  # Scenario 4: CREATE-time operated_by emits a mutation_history row
  # with field='operated_by', mutation_type='create' (gd-uJkKs75lH1).
  # Without this row, a freshly-created bead loses human attribution
  # in the audit/undo log even though the column is stamped — every
  # downstream consumer that scans mutations (undo, viewer activity,
  # operator-history API) would mis-attribute the create event.
  # ══════════════════════════════════════════════════════════════════
  blue "Scenario 4: kd create with operated_by emits mutation_history row"
  local s4_op="oidc:e2e-creator-${RANDOM_SUFFIX}@example.com"
  local s4_id="" s4_create_out
  s4_create_out=$(BEADS_OPERATED_BY="$s4_op" "$KD_BIN" create \
    "${TITLE_PREFIX}-create-${RANDOM_SUFFIX}" \
    --type=task -d "scenario 4 ($RANDOM_SUFFIX)" --json 2>/dev/null || printf '{}')
  s4_id=$(bead_id_from_json "$s4_create_out")
  if [ -z "$s4_id" ]; then
    fail "create scenario-4 bead with operated_by" "no ID returned from kd create"
  else
    CREATED_BEADS+=("$s4_id")
    pass "scenario-4 bead created ($s4_id) with operated_by=$s4_op"

    # Query mutation_history via HTTP API. The gd-uJkKs75lH1 fix
    # requires a paired field='operated_by' row alongside the
    # field='*' create marker.
    local muts
    muts=$(curl -sk --max-time 10 \
      ${BEADS_TOKEN:+-H "Authorization: Bearer ${BEADS_TOKEN}"} \
      "${BEADS_HTTP_URL}/v1/beads/${s4_id}/mutations" 2>/dev/null || printf '{}')
    if printf '%s' "$muts" | jq -e --arg op "$s4_op" '
          .mutations
          | map(select(.mutation_type=="create" and .field=="operated_by" and .new_value==$op))
          | length > 0
        ' >/dev/null 2>&1; then
      pass "scenario 4: mutation_history has create row for field='operated_by' new_value=$s4_op"
    else
      fail "scenario 4: create-time mutation_history row missing" \
        "expected mutation_type=create field=operated_by new_value=$s4_op in /v1/beads/${s4_id}/mutations"
    fi
  fi
  echo

  summary
}

summary() {
  blue "═══════════════════════════════════════════════════"
  blue " viewer-bead-activity: $PASS passed, $FAIL failed, $SKIP skipped"
  if [ "$FAIL" -gt 0 ]; then
    red "FAILURES:"
    for e in ${ERRORS[@]+"${ERRORS[@]}"}; do red "  - $e"; done
    exit 1
  fi
  blue "═══════════════════════════════════════════════════"
}

main "$@"
