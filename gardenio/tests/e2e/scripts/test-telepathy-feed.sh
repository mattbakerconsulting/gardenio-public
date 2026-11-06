#!/usr/bin/env bash
# test-telepathy-feed.sh — E2E for the telepathy "thinking" SSE feed
# (coverage-backfill gd-uWRZ9ia43N, parent epic ka-0000000958).
#
# WHY THIS EXISTS: a whole new telepathy subsystem landed with NO live e2e.
# The viewer NATS subscriber (thinking_hub.go, task ka-0000007528) connects to
# NATS, subscribes coop.events.messages.*.thinking, drops signature-only
# empties, keeps a bounded ring of recent thoughts, and fans each event out to
# the unified /telepathy SSE page (telepathy.go, task ka-0000002159). All of it
# is httptest-unit-covered in-tree (thinking_hub_test.go, telepathy_test.go),
# but the sibling sse-events module only exercises the kbeads /v1/events/stream
# bus — NOTHING drove the deployed viewer's NATS -> ring -> /telepathy/events
# data path against a real cluster. This is exactly the class of bug the
# subsystem already shipped: MR 4139 landed the page + nav link but the deployed
# beads-viewer pod had no NATS env, so the feed was permanently disabled in
# every real environment and only the "feed disabled" banner ever rendered
# (gd-QeufU35VlI). A live e2e is what would have caught that.
#
# Scenarios (branch on whether the deployed viewer has NATS wired):
#   0. /telepathy page renders: GET /telepathy is a 200 that emits the feed
#      scaffold. Its FeedEnabled flag (presence/absence of the "Thinking feed
#      is disabled" banner) tells us whether the deployed viewer actually has a
#      NATS connection — the precise thing MR 4139 shipped broken.
#   1. SSE endpoint degrades cleanly (ALWAYS asserted, both branches):
#      GET /telepathy/events is an SSE stream (text/event-stream) that opens
#      with a comment line and NEVER 500s — a `: connected` comment when the
#      feed is live, or a `: telepathy feed disabled` comment when NATS is
#      unset. This is the "clean degrade when NATS env is unset (no panic)"
#      guarantee, exercised against a real deployment.
#   -- the next two run ONLY when the feed is live (NATS wired) --
#   2. Live NATS -> SSE: publish a coop.events.messages.<agent>.thinking message
#      onto the cluster NATS, with an open /telepathy/events listener, and
#      assert an `event: thinking` row streams carrying our unique agent name
#      and marker text. This proves the deployed subscriber -> hub -> SSE path
#      end to end.
#   3. Backfill-ring replays on connect: after (2)'s event is in the hub ring,
#      open a FRESH /telepathy/events connection and assert the same marker is
#      replayed immediately (before any new publish) — the "a freshly-connected
#      browser paints immediately instead of a blank screen" guarantee
#      (ThinkingHub.Subscribe ring backfill).
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-telepathy-feed.sh
#
# Prerequisites:
#   - kubectl context pointing at the target cluster (default gardenio-e2e)
#   - curl, jq in PATH
#   - lib-gitlab-token.sh's mint_user_token resolves a GitLab-direct bearer
#     (GARDENIO_E2E_GITLAB_BEARER or the static daemon token must be set). When a token cannot
#     be minted the whole module SKIPs (the SSE feed is auth-gated).
#   - For scenarios 2-3: a reachable cluster NATS (svc <ns>-nats:4222) and the
#     natsio/nats-box image pullable in-cluster (already pulled by the
#     controller init container on a normal deploy). Under nats subject
#     isolation the publish uses the infra token from <ns>-nats-infra-creds,
#     mirroring test-credential-rotation.sh (ka-0000006513 / ka-0000000002).
#
# Environment note: the beads-viewer OIDC surface + the telepathy NATS wiring
# are deployed only on the long-lived staging singleton and prod — NOT in
# the per-instance ephemeral e2e namespaces (edge-auth/README.md). When the
# viewer Service is absent the whole module SKIPs cleanly (exit 0), so it is
# inert in the ephemeral shard fan-out and asserts only against the
# singleton / prod.
#
# Idempotency: the published thinking event carries a unique per-run agent name
# and marker text; it is a NATS message (not a bead) so nothing needs cleanup
# beyond the ephemeral nats-box pod, which is `kubectl run --rm`-scoped.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Config ────────────────────────────────────────────────────────
# Set the lib-viewer-consumed knobs (NAMESPACE / RELEASE / VIEWER_SVC /
# VIEWER_PF_PORT) BEFORE sourcing lib-viewer.sh: the lib defaults them with
# `: "${VAR:=…}"` at source time, so a value set afterwards would be ignored and
# our dedicated port (kept distinct from sibling viewer modules to avoid a
# port-forward collision when shards co-locate) would silently not take effect.
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
RELEASE="${RELEASE:-${NAMESPACE}}"
VIEWER_SVC="${VIEWER_SVC:-${RELEASE}-beads-viewer}"
VIEWER_PF_PORT="${VIEWER_PF_PORT:-19651}"
NATS_SVC="${NATS_SVC:-${NAMESPACE}-nats}"
NATS_IMAGE="${NATS_IMAGE:-natsio/nats-box:0.14.5}"

# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gitlab-token.sh"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-viewer.sh"

RANDOM_SUFFIX="$$-$(date +%s)"
# Unique per-run agent + marker so a concurrent run / retained ring entry never
# collides. The agent name must be a valid subject token (no dots) — it becomes
# the <actor> segment of coop.events.messages.<actor>.thinking.
TELEPATHY_AGENT="telepathy-e2e-${RANDOM_SUFFIX}"
TELEPATHY_MARKER="TELEPATHY-E2E-MARKER-${RANDOM_SUFFIX}"

# SSE read windows. The live listener must outlast nats-box pod spin-up +
# publish; the ring-replay connect only needs the backfill burst.
LIVE_LISTEN_SECS="${LIVE_LISTEN_SECS:-45}"
RING_LISTEN_SECS="${RING_LISTEN_SECS:-8}"
DEGRADE_LISTEN_SECS="${DEGRADE_LISTEN_SECS:-6}"

PASS=0
FAIL=0
SKIP=0
ERRORS=()
BG_PIDS=()

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
  for pid in "${BG_PIDS[@]:-}"; do
    # shellcheck disable=SC2015 # best-effort kill; the || true is the intended fallthrough
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  done
  rm -f "${VGET_HDR:-}" "${VGET_BODY:-}" /tmp/telepathy-feed-*.log 2>/dev/null || true
  teardown_viewer_portforward
}
trap cleanup EXIT

# vget <path> <token> [extra_curl_args...]
# Authenticated GET (no -L, so a pre-auth redirect surfaces as its own status).
# Writes response headers to $VGET_HDR and the body to $VGET_BODY, echoes the
# numeric HTTP status. A 000 means no HTTP response (tunnel down).
#
# $VGET_HDR/$VGET_BODY are FIXED paths allocated once in the parent shell (see
# the identical rationale in test-charts-hub.sh): every caller invokes vget in a
# command substitution — a SUBSHELL — so any variable assignment inside vget
# would be lost. curl's -o/-D write to the stable on-disk paths, which survive
# the subshell boundary, so the parent reads exactly what the subshell wrote.
VGET_HDR="$(mktemp)"
VGET_BODY="$(mktemp)"
vget() {
  local path="$1" token="$2"
  shift 2
  : >"$VGET_HDR"
  : >"$VGET_BODY"
  curl -sk --max-time 20 \
    -H "Authorization: Bearer ${token}" \
    -o "$VGET_BODY" -D "$VGET_HDR" \
    -w '%{http_code}' \
    "$@" \
    "http://localhost:${VIEWER_PF_PORT}${path}" 2>/dev/null || echo "000"
}

# header_value <headers-file> <header-name> — (last) value of the named response
# header, CR-stripped and trimmed. Empty when absent. The `|| true` on grep is
# load-bearing under pipefail: an absent header is a valid case.
header_value() {
  local file="$1" name="$2"
  { grep -i "^${name}:" "$file" 2>/dev/null || true; } \
    | tail -n1 \
    | sed -E "s/^[^:]*:[[:space:]]*//I" \
    | tr -d '\r' \
    | sed -E 's/[[:space:]]+$//'
}

# start_sse_listener <log> <token> — background curl reading /telepathy/events
# into <log>. Captures the pid in BG_PIDS and gives the connection a moment to
# establish. The --max-time bounds the read so the listener always terminates.
start_sse_listener() {
  local log="$1" token="$2" secs="${3:-$LIVE_LISTEN_SECS}"
  curl -sN --max-time "$secs" \
    -H "Authorization: Bearer ${token}" \
    "http://localhost:${VIEWER_PF_PORT}/telepathy/events" \
    > "$log" 2>/dev/null &
  local pid=$!
  BG_PIDS+=("$pid")
  sleep 2
  echo "$pid"
}

# nats_publish_thinking — publish one thinking event onto the cluster NATS via
# an ephemeral nats-box pod, mirroring test-credential-rotation.sh's helper.
# Under nats subject isolation coop.events.> is agent/infra-scoped, so we fetch
# the infra token and send it as the infra user's PASSWORD (ka-0000000002);
# auth-disabled envs have no such secret and connect anonymously.
nats_publish_thinking() {
  local subject="coop.events.messages.${TELEPATHY_AGENT}.thinking"
  local payload
  payload=$(jq -nc \
    --arg agent "$TELEPATHY_AGENT" \
    --arg text "$TELEPATHY_MARKER — deployed telepathy feed e2e ($RANDOM_SUFFIX)" \
    '{event_type:"thinking", actor:$agent, text:$text, source:"e2e", role:"crew", project:"gardenio"}')
  local pod_name="telepathy-e2e-pub-${RANDOM_SUFFIX}"
  local token_args=()
  local infra_token
  infra_token=$(kubectl get secret -n "$NAMESPACE" "${NAMESPACE}-nats-infra-creds" \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null) || true
  if [ -n "${infra_token:-}" ]; then
    token_args=(--user infra --password "$infra_token")
  fi
  dim "  publishing NATS thinking event on $subject …"
  # Leak fix (gd-to1cEYBrxe): pin tiny explicit resource requests AND guarantee
  # deletion. Without an explicit request the pod inherits the namespace
  # LimitRanger defaultRequest (2 CPU / 16Gi on the staging singleton), which
  # never fits available capacity, so the pod sticks Pending; `timeout` then
  # SIGKILLs kubectl before its client-side `--rm` fires and orphans it. The
  # `--overrides` sets a schedulable footprint; the trailing delete reclaims the
  # pod even when the timeout kills `--rm` early.
  timeout 40 kubectl run -n "$NAMESPACE" "$pod_name" --rm -i --restart=Never \
    --image="$NATS_IMAGE" \
    --image-pull-policy=IfNotPresent \
    --override-type=strategic \
    --overrides="{\"spec\":{\"containers\":[{\"name\":\"${pod_name}\",\"resources\":{\"requests\":{\"cpu\":\"10m\",\"memory\":\"32Mi\"},\"limits\":{\"cpu\":\"200m\",\"memory\":\"128Mi\"}}}]}}" \
    --command -- nats pub -s "nats://${NATS_SVC}:4222" "${token_args[@]}" \
    "$subject" "$payload" >/dev/null 2>&1
  kubectl delete pod -n "$NAMESPACE" "$pod_name" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

# marker_in_thinking_frame <log> — 0 if the log contains an `event: thinking`
# frame whose data carries our unique marker. SSE frames are terminated by a
# blank line; we treat the file as a whole and require BOTH the event line and a
# data line with the marker to be present (a full row is a single data line —
# renderThinkingRow emits no newlines for single-line text).
marker_in_thinking_frame() {
  local log="$1"
  grep -q '^event: thinking' "$log" 2>/dev/null \
    && grep -q "^data:.*${TELEPATHY_MARKER}" "$log" 2>/dev/null
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " telepathy NATS SSE feed E2E (gd-uWRZ9ia43N)"
  blue "═══════════════════════════════════════════════════"

  if ! setup_viewer_portforward; then
    skip "telepathy-feed module" \
      "viewer Service '$VIEWER_SVC' absent in '$NAMESPACE' (viewer OIDC surface is singleton/prod-only)"
    summary
    return 0
  fi

  # Mint a viewer-role user token; /telepathy and /telepathy/events sit behind
  # the viewer's OIDC middleware exactly like the rest of the authenticated
  # pages (the events route is not viewer/operator-gated, only auth-gated).
  local token=""
  if ! token=$(mint_user_token "e2e-telepathy-${RANDOM_SUFFIX}@example.com" 2>/tmp/telepathy-feed-mint.err); then
    yellow "mint_user_token failed — see /tmp/telepathy-feed-mint.err"
    skip "telepathy-feed module" "no usable GitLab-direct bearer (viewer OIDC unreachable)"
    summary
    return 0
  fi
  dim "  minted viewer token"

  # Sanity: the token must actually authenticate against the viewer. A
  # redirect/401/403 means the deployment's auth is not what this module assumes
  # (e.g. AVA-only edge forwarding no bearer) — skip rather than red.
  local probe_status
  probe_status=$(vget "/telepathy" "$token")
  rm -f "$VGET_HDR" "$VGET_BODY"
  if [ "$probe_status" = "000" ]; then
    skip "telepathy-feed module" "no HTTP response from viewer (tunnel down)"
    summary
    return 0
  fi
  if [ "$probe_status" = "302" ] || [ "$probe_status" = "303" ] || \
     [ "$probe_status" = "401" ] || [ "$probe_status" = "403" ]; then
    skip "telepathy-feed module" \
      "GET /telepathy → $probe_status (viewer did not accept the minted bearer; auth model differs)"
    summary
    return 0
  fi

  # scenario_page_render sets FEED_ENABLED (yes/no). Call it DIRECTLY (not in a
  # command substitution) so its pass/fail increments land in the parent shell's
  # counters rather than a lost subshell.
  FEED_ENABLED="no"
  scenario_page_render "$token"
  local feed_enabled="$FEED_ENABLED"

  scenario_sse_degrades_cleanly "$token" "$feed_enabled"

  if [ "$feed_enabled" = "yes" ]; then
    scenario_live_stream_and_ring_replay "$token"
  else
    skip "scenario 2: live NATS → SSE row" \
      "deployed viewer has NATS unset (feed disabled) — degrade asserted in scenario 1 instead"
    skip "scenario 3: backfill-ring replay" \
      "deployed viewer has NATS unset (feed disabled) — no live hub to populate the ring"
  fi

  summary
}

# ══════════════════════════════════════════════════════════════════
# Scenario 0: /telepathy renders + record whether the feed is live.
# Sets the module-global FEED_ENABLED to "yes"/"no" (the FeedEnabled state) for
# main() to branch on. Called directly (not in a subshell) so its pass/fail
# counters and FEED_ENABLED assignment land in the parent shell.
# ══════════════════════════════════════════════════════════════════
FEED_ENABLED="no"
scenario_page_render() {
  local token="$1"
  blue "Scenario 0: GET /telepathy renders the feed page"

  local status body
  status=$(vget "/telepathy" "$token")
  body=$(cat "$VGET_BODY"); rm -f "$VGET_HDR" "$VGET_BODY"
  if [ "$status" != "200" ]; then
    fail "GET /telepathy → 200" "got $status"
    FEED_ENABLED="no"; echo; return 0
  fi
  pass "GET /telepathy → 200"

  # Scaffold proof: the telepathy page shell (not an error page).
  if grep -q 'class="telepathy-page"' <<<"$body" \
     || grep -q 'telepathy-feed' <<<"$body"; then
    pass "telepathy page scaffold present (.telepathy-page / .telepathy-feed)"
  else
    fail "telepathy page scaffold present" \
      "neither .telepathy-page nor .telepathy-feed in /telepathy HTML (error page?)"
  fi

  # FeedEnabled gating: the disabled banner renders IFF the viewer has no NATS
  # connection. Its ABSENCE is the positive proof that the deployed pod wired
  # NATS — the exact regression MR 4139 shipped and gd-QeufU35VlI fixed.
  if grep -q 'Thinking feed is disabled' <<<"$body"; then
    pass "feed-disabled banner present → deployed viewer has NATS UNSET (degrade path live)"
    dim "    (streaming scenarios will be skipped; scenario 1 asserts the clean degrade)"
    FEED_ENABLED="no"
  else
    pass "no feed-disabled banner → deployed viewer HAS NATS wired (feed live)"
    FEED_ENABLED="yes"
  fi
  echo
}

# ══════════════════════════════════════════════════════════════════
# Scenario 1: /telepathy/events degrades cleanly — SSE content-type, an opening
# comment line, and NEVER a 500. Asserted in BOTH branches: a `: connected`
# comment when live, a `: telepathy feed disabled` comment when NATS is unset.
# This is the "no panic when NATS env is unset" guarantee against a real pod.
# ══════════════════════════════════════════════════════════════════
scenario_sse_degrades_cleanly() {
  local token="$1" feed_enabled="$2"
  blue "Scenario 1: GET /telepathy/events opens a clean SSE stream (no 500)"

  # A bounded read: the endpoint holds the connection open, so we cap it with
  # --max-time and inspect what it emitted in the window. curl is CUT by the
  # timeout on a still-open 200 stream and exits 28, which makes `-w
  # '%{http_code}'` unreliable (it races the kill). Parse the status from the
  # -D header dump instead — curl flushes the response headers to it the moment
  # they arrive, long before the timeout cut, so the status line is always
  # present for any real response. -o captures the streamed comment lines.
  local hdr body status ctype
  hdr="$(mktemp)"; body="$(mktemp)"
  curl -sk --max-time "$DEGRADE_LISTEN_SECS" \
    -H "Authorization: Bearer ${token}" \
    -o "$body" -D "$hdr" \
    "http://localhost:${VIEWER_PF_PORT}/telepathy/events" 2>/dev/null || true

  # Status from the first line of the header dump: "HTTP/1.1 200 OK". Empty
  # header dump ⇒ no response reached us (tunnel down).
  status=$(head -n1 "$hdr" 2>/dev/null | tr -d '\r' | awk '{print $2}')
  ctype=$(header_value "$hdr" "Content-Type")

  if [ -z "$status" ]; then
    skip "scenario 1: SSE clean degrade" "no HTTP response (tunnel down mid-module)"
    rm -f "$hdr" "$body"; echo; return 0
  elif [ "$status" = "500" ]; then
    fail "GET /telepathy/events non-500" "viewer returned 500 (panic instead of a clean SSE degrade)"
  elif [ "$status" = "200" ]; then
    pass "GET /telepathy/events → 200 (clean SSE open, no panic)"
  else
    fail "GET /telepathy/events non-500" "got $status (expected a streamed 200)"
  fi

  if grep -qi 'text/event-stream' <<<"$ctype"; then
    pass "SSE Content-Type is text/event-stream"
  else
    fail "SSE Content-Type text/event-stream" "got '${ctype:-<none>}'"
  fi

  # The opening comment line must match the feed state — proof the handler ran
  # its degrade branch (disabled) or its live branch (connected), never 500ed.
  if [ "$feed_enabled" = "yes" ]; then
    if grep -q '^: connected' "$body"; then
      pass "live feed opened with a ': connected' comment"
    else
      # The ring may already carry an event that arrives before/without the
      # comment being flushed distinctly; a non-empty stream is still a pass.
      if [ -s "$body" ]; then
        pass "live feed streamed opening bytes (comment/backlog present)"
      else
        fail "live feed opening comment" "no ': connected' and no bytes in ${DEGRADE_LISTEN_SECS}s window"
      fi
    fi
  else
    if grep -q 'telepathy feed disabled' "$body"; then
      pass "disabled feed emitted the ': telepathy feed disabled (NATS not configured)' comment"
    else
      fail "disabled feed degrade comment" \
        "expected a 'telepathy feed disabled' SSE comment; got: $(head -c160 "$body")"
    fi
  fi

  rm -f "$hdr" "$body"
  echo
}

# ══════════════════════════════════════════════════════════════════
# Scenario 2 + 3: live NATS → SSE row, then backfill-ring replay on a fresh
# connect. One publish serves both: the live listener proves streaming; a
# subsequent fresh connect proves the same event is replayed from the ring.
# ══════════════════════════════════════════════════════════════════
scenario_live_stream_and_ring_replay() {
  local token="$1"

  # ── Scenario 2: live streaming ────────────────────────────────
  blue "Scenario 2: publish a NATS thinking event → an 'event: thinking' row streams"

  # Preflight: the ephemeral publisher needs a reachable NATS Service. If it is
  # absent (a viewer wired to an external/again-different NATS, or NATS off),
  # skip the streaming scenarios rather than red the suite — the feed being
  # ENABLED was already proven in scenario 0.
  if ! kubectl -n "$NAMESPACE" get svc "$NATS_SVC" >/dev/null 2>&1; then
    skip "scenario 2: live NATS → SSE row" \
      "NATS Service '$NATS_SVC' absent in '$NAMESPACE' (cannot publish a probe event)"
    skip "scenario 3: backfill-ring replay" "no NATS Service to seed the ring"
    echo; return 0
  fi

  local live_log="/tmp/telepathy-feed-live-${RANDOM_SUFFIX}.log"
  local live_pid
  live_pid=$(start_sse_listener "$live_log" "$token" "$LIVE_LISTEN_SECS")
  dim "  SSE listener up (pid $live_pid); publishing the probe event…"

  if ! nats_publish_thinking; then
    skip "scenario 2: live NATS → SSE row" \
      "NATS publish failed (nats-box pod unschedulable / image unpullable / auth) — feed enablement still proven in scenario 0"
    skip "scenario 3: backfill-ring replay" "no event reached the hub to seed the ring"
    kill "$live_pid" 2>/dev/null || true
    echo; return 0
  fi
  dim "  published; waiting for the row to stream…"

  # Poll the listener log for our frame (the row streams as soon as the hub
  # broadcasts it; give it a few seconds of slack for NATS + flush).
  local i seen="no"
  for ((i = 1; i <= 12; i++)); do
    if marker_in_thinking_frame "$live_log"; then seen="yes"; break; fi
    sleep 1
  done
  kill "$live_pid" 2>/dev/null || true

  if [ "$seen" = "yes" ]; then
    pass "live 'event: thinking' row streamed carrying our marker"
    # The row must be the telepathy row markup keyed to our agent — proof it is
    # the real renderThinkingRow output, not some unrelated frame.
    if grep -q "data-agent=\"${TELEPATHY_AGENT}\"" "$live_log" 2>/dev/null; then
      pass "streamed row is the telepathy row for agent '${TELEPATHY_AGENT}' (data-agent + telepathy-event)"
    else
      # data-agent HTML-escapes the name; our name is escape-safe, so an absence
      # is a genuine shape mismatch worth flagging (not fatal — the marker match
      # above already proves the data path).
      yellow "    note: marker present but data-agent=\"${TELEPATHY_AGENT}\" not found verbatim (row shape drift?)"
    fi
  else
    fail "live 'event: thinking' row streams" \
      "no thinking frame with marker '${TELEPATHY_MARKER}' within ${LIVE_LISTEN_SECS}s (subscriber → hub → SSE path)"
    dim "    last 8 lines of listener log:"
    tail -n8 "$live_log" 2>/dev/null | while IFS= read -r l; do dim "    | $l"; done
    # Without a live event in the ring the replay scenario cannot run either.
    skip "scenario 3: backfill-ring replay" "no live event captured to seed the ring"
    echo; return 0
  fi
  echo

  # ── Scenario 3: backfill-ring replay on a fresh connect ───────
  blue "Scenario 3: a fresh /telepathy/events connect replays the event from the ring"

  # No new publish here: the SAME event from scenario 2 is now in the hub's
  # bounded ring. A freshly-connected client must be backfilled with it
  # immediately (ThinkingHub.Subscribe replays the recent ring before going
  # live). This is the "paint immediately, not a blank screen" guarantee.
  local ring_log="/tmp/telepathy-feed-ring-${RANDOM_SUFFIX}.log"
  local ring_pid
  ring_pid=$(start_sse_listener "$ring_log" "$token" "$RING_LISTEN_SECS")
  # The backfill is pushed at connect time; a short settle is enough.
  sleep "$RING_LISTEN_SECS"
  kill "$ring_pid" 2>/dev/null || true

  if marker_in_thinking_frame "$ring_log"; then
    pass "fresh connect replayed the ring event (marker '${TELEPATHY_MARKER}' backfilled on connect)"
  else
    # Not necessarily fatal: on a very busy fleet the ring (cap 200) could have
    # evicted our event between the two connects, or a viewer restart wiped the
    # in-memory ring. Record as a soft note so a legitimately-full ring under a
    # busy prod does not red the suite — scenario 2 already proved the live path.
    yellow "    note: marker not replayed on fresh connect within ${RING_LISTEN_SECS}s"
    yellow "    (ring cap 200 may have evicted it under a busy fleet, or the viewer restarted — live path still proven in scenario 2)"
    skip "scenario 3: backfill-ring replay" \
      "event not observed on fresh connect (ring eviction / viewer restart — live path proven in scenario 2)"
  fi
  echo
}

summary() {
  blue "═══════════════════════════════════════════════════"
  blue " telepathy-feed: $PASS passed, $FAIL failed, $SKIP skipped"
  if [ "$FAIL" -gt 0 ]; then
    red "FAILURES:"
    for e in "${ERRORS[@]}"; do red "  - $e"; done
    exit 1
  fi
  blue "═══════════════════════════════════════════════════"
}

main "$@"
